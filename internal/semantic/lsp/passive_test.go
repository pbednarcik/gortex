package lsp

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/spf13/viper"

	"github.com/zzet/gortex/internal/config"
)

// fakeLSPSocketServer accepts a single client connection on a TCP or unix
// socket and replies to LSP requests via a caller-supplied handler.
// It records every received method name so the test can assert what
// the Provider sent (or, critically for shutdown semantics, what it
// did NOT send).
type fakeLSPSocketServer struct {
	t        *testing.T
	listener net.Listener
	mu       sync.Mutex
	conn     net.Conn
	methods  []string
	// pushed is fired once the server has accepted its first conn.
	// Tests can wait on it before tearing the server down.
	pushed chan struct{}
	// handler answers a request method. Return (result, ok) — ok=false
	// makes the server send a method-not-found error.
	handler func(method string, params json.RawMessage) (any, bool)
	// stopped guards the accept loop's lifecycle.
	stopped chan struct{}
}

// newFakeLSPSocketServer starts a server on the given network. For "tcp" the
// address is "127.0.0.1:0" (an OS-assigned port); for "unix" the
// address is a fresh socket path under t.TempDir().
//
// The returned addr is what the test passes to DialTransport.Address.
// Cleanup is registered via t.Cleanup.
func newFakeLSPSocketServer(t *testing.T, network string) (server *fakeLSPSocketServer, addr string) {
	t.Helper()
	var (
		ln  net.Listener
		err error
	)
	switch network {
	case "tcp":
		ln, err = net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		addr = ln.Addr().String()
	case "unix":
		// Keep the path short — macOS sun_path is capped at 104B
		// (Linux: 108B). t.TempDir() under /var/folders/... can
		// exceed that, so we use os.MkdirTemp under /tmp with a
		// short suffix instead.
		dir, err := os.MkdirTemp("", "lsp")
		require.NoError(t, err)
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		addr = filepath.Join(dir, "s.sock")
		ln, err = net.Listen("unix", addr)
		require.NoError(t, err)
	default:
		t.Fatalf("unknown fake-LSP network %q", network)
	}
	server = &fakeLSPSocketServer{
		t:        t,
		listener: ln,
		pushed:   make(chan struct{}, 1),
		stopped:  make(chan struct{}),
	}
	server.handler = server.defaultHandler
	t.Cleanup(func() { _ = server.Close() })
	go server.acceptLoop()
	return server, addr
}

// Close shuts the listener (and any active conn) down. Safe to call
// twice — the cleanup hook and an explicit test teardown can race.
func (s *fakeLSPSocketServer) Close() error {
	s.mu.Lock()
	// Closing the listener races with acceptLoop's blocking Accept;
	// the loop's select on stopped unblocks the conn-handler exit.
	select {
	case <-s.stopped:
		s.mu.Unlock()
		return nil
	default:
		close(s.stopped)
	}
	conn := s.conn
	s.conn = nil
	s.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
	return s.listener.Close()
}

// Methods returns a copy of the received method names — convenient for
// assertions like "did the server receive shutdown/exit?".
func (s *fakeLSPSocketServer) Methods() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make([]string, len(s.methods))
	copy(cp, s.methods)
	return cp
}

// CloseConn drops the active client connection without stopping the
// listener — simulates "the IDE quit but the listening socket is back".
// Tests use this to exercise reconnect.
func (s *fakeLSPSocketServer) CloseConn() {
	s.mu.Lock()
	conn := s.conn
	s.conn = nil
	s.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

// PushRequest sends a server-initiated request to the connected client.
// Used by the capability-reset test to drive a registerCapability over
// the dialed connection.
func (s *fakeLSPSocketServer) PushRequest(id any, method string, params any) error {
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	if conn == nil {
		return errors.New("fake server: no connected client")
	}
	body, err := json.Marshal(struct {
		JSONRPC string `json:"jsonrpc"`
		ID      any    `json:"id"`
		Method  string `json:"method"`
		Params  any    `json:"params"`
	}{"2.0", id, method, params})
	if err != nil {
		return err
	}
	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(body))
	if _, err := conn.Write([]byte(header)); err != nil {
		return err
	}
	_, err = conn.Write(body)
	return err
}

func (s *fakeLSPSocketServer) defaultHandler(method string, _ json.RawMessage) (any, bool) {
	switch method {
	case "initialize":
		return InitializeResult{Capabilities: ServerCapabilities{}}, true
	case "shutdown":
		return nil, true
	default:
		return struct{}{}, true
	}
}

func (s *fakeLSPSocketServer) acceptLoop() {
	conn, err := s.listener.Accept()
	if err != nil {
		// Listener closed before any client connected. Normal at
		// teardown; nothing to do.
		return
	}
	s.mu.Lock()
	s.conn = conn
	s.mu.Unlock()
	select {
	case s.pushed <- struct{}{}:
	default:
	}
	s.serve(conn)
}

func (s *fakeLSPSocketServer) serve(conn net.Conn) {
	rd := bufio.NewReader(conn)
	for {
		contentLength := -1
		for {
			line, err := rd.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimSpace(line)
			if line == "" {
				break
			}
			if strings.HasPrefix(line, "Content-Length:") {
				val := strings.TrimSpace(strings.TrimPrefix(line, "Content-Length:"))
				contentLength, _ = strconv.Atoi(val)
			}
		}
		if contentLength < 0 {
			continue
		}
		body := make([]byte, contentLength)
		if _, err := io.ReadFull(rd, body); err != nil {
			return
		}
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(body, &msg); err != nil {
			continue
		}
		if msg.Method != "" {
			s.mu.Lock()
			s.methods = append(s.methods, msg.Method)
			h := s.handler
			s.mu.Unlock()
			// Notifications carry no id; nothing to reply with.
			if len(msg.ID) == 0 {
				continue
			}
			if h == nil {
				h = s.defaultHandler
			}
			result, ok := h(msg.Method, msg.Params)
			if !ok {
				resp, _ := json.Marshal(struct {
					JSONRPC string          `json:"jsonrpc"`
					ID      json.RawMessage `json:"id"`
					Error   any             `json:"error"`
				}{"2.0", msg.ID, map[string]any{
					"code":    -32601,
					"message": "method not found: " + msg.Method,
				}})
				header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(resp))
				_, _ = conn.Write([]byte(header))
				_, _ = conn.Write(resp)
				continue
			}
			resp, _ := json.Marshal(struct {
				JSONRPC string          `json:"jsonrpc"`
				ID      json.RawMessage `json:"id"`
				Result  any             `json:"result"`
			}{"2.0", msg.ID, result})
			header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(resp))
			_, _ = conn.Write([]byte(header))
			_, _ = conn.Write(resp)
			continue
		}
		// Otherwise it's a response to a server-initiated request
		// (the client acking a reverse RPC). We don't track them
		// in defaultHandler; tests that care can install a custom
		// handler.
	}
}

// passiveProvider builds a Provider that dials the given network/addr.
// FallbackSpawn is opt-in; callers that test the no-fallback path pass
// fallback=false; callers that test the spawn-fallback path pass true
// plus a spec-style command.
func passiveProvider(t *testing.T, network, addr string, fallback bool) *Provider {
	t.Helper()
	spec := &ServerSpec{
		Name:      "fake-passive",
		Command:   "/bin/echo", // present on every POSIX runner; never invoked unless fallback triggers.
		Languages: []string{"go"},
		Connect: &ConnectSpec{
			Network:       network,
			Address:       addr,
			FallbackSpawn: fallback,
		},
	}
	return NewProviderFromSpec(spec, zap.NewNop())
}

// callMethod issues a synthetic JSON-RPC request on the provider's
// active client and returns true on a successful round trip.
func callMethod(t *testing.T, p *Provider, method string) error {
	t.Helper()
	require.NotNil(t, p.client, "provider has no client")
	return p.client.Call(method, struct{}{}, nil)
}

// TestPassiveProvider_TCPDialHappyPath establishes a fake LSP server
// on 127.0.0.1:0 and verifies that EnsureClient completes the
// initialize handshake against the dialed connection. A follow-up
// synthetic request confirms a round-trip works end-to-end.
func TestPassiveProvider_TCPDialHappyPath(t *testing.T) {
	server, addr := newFakeLSPSocketServer(t, "tcp")
	defer server.Close()

	p := passiveProvider(t, "tcp", addr, false)
	defer p.Close()

	require.NoError(t, p.EnsureClient(t.TempDir()))

	// Wait briefly for the fake server's accept loop to record
	// the initialize call — Accept happens on a separate goroutine.
	require.Eventually(t, func() bool {
		for _, m := range server.Methods() {
			if m == "initialize" {
				return true
			}
		}
		return false
	}, time.Second, 5*time.Millisecond, "fake server did not receive initialize")

	require.NoError(t, callMethod(t, p, "textDocument/hover"))

	gotHover := false
	for _, m := range server.Methods() {
		if m == "textDocument/hover" {
			gotHover = true
		}
	}
	require.True(t, gotHover, "fake server did not receive textDocument/hover: got %v", server.Methods())
}

// TestPassiveProvider_UnixDialHappyPath mirrors the TCP test using a
// Unix-domain socket. Skipped on Windows where AF_UNIX support is
// limited and varies by runner image.
func TestPassiveProvider_UnixDialHappyPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix-domain sockets are not exercised in the Windows CI image")
	}
	server, addr := newFakeLSPSocketServer(t, "unix")
	defer server.Close()

	p := passiveProvider(t, "unix", addr, false)
	defer p.Close()

	require.NoError(t, p.EnsureClient(t.TempDir()))

	require.Eventually(t, func() bool {
		for _, m := range server.Methods() {
			if m == "initialize" {
				return true
			}
		}
		return false
	}, time.Second, 5*time.Millisecond, "fake server did not receive initialize over unix socket")
}

// TestPassiveProvider_DialFailsNoFallback verifies that a closed
// endpoint with fallback_spawn=false produces a clear dial error and
// does NOT spawn a subprocess. We assert the spawn path was avoided by
// checking that the error message describes the dial failure (the
// subprocess path would surface a different "start <cmd>" error).
func TestPassiveProvider_DialFailsNoFallback(t *testing.T) {
	// Allocate a TCP listener just to grab a free port, then close
	// it so the dial fails with "connection refused".
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	deadAddr := ln.Addr().String()
	require.NoError(t, ln.Close())

	p := passiveProvider(t, "tcp", deadAddr, false)
	defer p.Close()

	err = p.EnsureClient(t.TempDir())
	require.Error(t, err, "EnsureClient should fail when dial fails with no fallback")
	require.Contains(t, err.Error(), "passive attach",
		"error should identify passive-attach failure mode, not a spawn-path message: %v", err)
	require.Nil(t, p.client, "no client should be cached on dial-only failure")
}

// TestPassiveProvider_DialFailsSpawnFallback verifies that a closed
// endpoint with fallback_spawn=true falls back to spawning the spec's
// command. The fixture's "command" is set to a no-op binary that
// blocks on stdin, so the spawn succeeds and the initialize handshake
// then fails (because /bin/cat is not a real LSP server). We just need
// to confirm the spawn was attempted, which we detect via the spec's
// SpawnTransport — the Provider's client.transport is reachable
// internally because the test is in the same package.
func TestPassiveProvider_DialFailsSpawnFallback(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	deadAddr := ln.Addr().String()
	require.NoError(t, ln.Close())

	// The helper's cat mode blocks on stdin without writing anything, so
	// the spawn succeeds but the (immediate) initialize Call we don't make
	// would have blocked indefinitely. We don't invoke initialize here —
	// dialOrSpawn just constructs the Client. It exits cleanly when we
	// close stdin during Shutdown, so the deferred Close is fast. This is
	// the test binary rather than /bin/cat because Windows has neither
	// /bin/cat nor a shell at a fixed path.
	useHelper(t, helperCat, 0)
	spec := &ServerSpec{
		Name:      "fake-passive-spawn-fallback",
		Command:   helperCommand(t),
		Args:      helperArgs,
		Languages: []string{"go"},
		Connect: &ConnectSpec{
			Network:       "tcp",
			Address:       deadAddr,
			FallbackSpawn: true,
		},
	}
	p := NewProviderFromSpec(spec, zap.NewNop())
	defer p.Close()

	// We don't await initialize success — the helper is not a real
	// LSP server, so the handshake will eventually time out. The
	// dialOrSpawn helper is what we're verifying: it must succeed
	// on the spawn fallback. Run it directly to keep the test fast.
	client, err := p.dialOrSpawn(t.TempDir())
	require.NoError(t, err, "dialOrSpawn should fall back to spawn")
	defer func() { _ = client.Shutdown() }()

	// The transport on the freshly built client must be a spawn
	// transport — that's the proof the dial was abandoned and the
	// fallback engaged.
	_, isSpawn := client.transport.(*SpawnTransport)
	require.True(t, isSpawn, "client transport should be a SpawnTransport after fallback, got %T", client.transport)
}

// TestPassiveProvider_ShutdownDoesNotSendExit confirms that closing a
// dialed Provider does NOT send `shutdown`/`exit` to the IDE-managed
// LSP server. Only the socket is closed; the IDE keeps its server up.
func TestPassiveProvider_ShutdownDoesNotSendExit(t *testing.T) {
	server, addr := newFakeLSPSocketServer(t, "tcp")
	defer server.Close()

	p := passiveProvider(t, "tcp", addr, false)
	require.NoError(t, p.EnsureClient(t.TempDir()))

	// Wait for initialize so we know the connection is live.
	require.Eventually(t, func() bool {
		for _, m := range server.Methods() {
			if m == "initialize" {
				return true
			}
		}
		return false
	}, time.Second, 5*time.Millisecond)

	require.NoError(t, p.Close(), "Provider.Close")

	// Give the server's read loop a moment to drain anything in
	// flight — shutdown/exit would arrive shortly after Close().
	time.Sleep(50 * time.Millisecond)

	for _, m := range server.Methods() {
		require.NotEqual(t, "shutdown", m, "passive attach must not send shutdown: full transcript %v", server.Methods())
		require.NotEqual(t, "exit", m, "passive attach must not send exit: full transcript %v", server.Methods())
	}
}

// TestPassiveProvider_ServerEOFReconnect drops the connection mid-
// session and verifies the next EnsureClient re-dials and re-completes
// initialize. The Provider's reconnect path resets per-connection
// state (open docs, dynamic caps); the server records two
// initialize calls — one before, one after the reconnect.
func TestPassiveProvider_ServerEOFReconnect(t *testing.T) {
	server, addr := newFakeLSPSocketServer(t, "tcp")
	defer server.Close()

	p := passiveProvider(t, "tcp", addr, false)
	defer p.Close()

	require.NoError(t, p.EnsureClient(t.TempDir()))

	require.Eventually(t, func() bool {
		for _, m := range server.Methods() {
			if m == "initialize" {
				return true
			}
		}
		return false
	}, time.Second, 5*time.Millisecond, "first initialize")

	// Drop the connection on the server side; relaunch acceptLoop
	// so the next dial gets a fresh socket.
	server.CloseConn()
	// Start a new accept loop on the existing listener so the
	// re-dial finds a partner.
	go server.acceptLoop()

	// The Provider's read loop notices EOF and closes Done(). Wait
	// for that so the next EnsureClient sees the dead-client path.
	require.Eventually(t, func() bool {
		select {
		case <-p.client.Done():
			return true
		default:
			return false
		}
	}, time.Second, 5*time.Millisecond, "client.Done did not fire after server EOF")

	require.NoError(t, p.EnsureClient(t.TempDir()), "reconnect should succeed")

	// Two initialize calls in the transcript prove the reconnect
	// performed a fresh handshake.
	require.Eventually(t, func() bool {
		count := 0
		for _, m := range server.Methods() {
			if m == "initialize" {
				count++
			}
		}
		return count >= 2
	}, time.Second, 5*time.Millisecond, "second initialize after reconnect: transcript %v", server.Methods())
}

// TestPassiveProvider_CapabilityResetOnReconnect drives a dynamic
// capability registration on the first connection, then forces a
// reconnect. After reconnect, the dynamicCaps map must be empty until
// the new connection's registerCapability replays — the reset is
// load-bearing because the server's dynamic table starts empty on
// every fresh session.
func TestPassiveProvider_CapabilityResetOnReconnect(t *testing.T) {
	server, addr := newFakeLSPSocketServer(t, "tcp")
	defer server.Close()

	p := passiveProvider(t, "tcp", addr, false)
	defer p.Close()

	// First connection. Wire the handlers ensureClient would install,
	// then complete the handshake.
	require.NoError(t, p.EnsureClient(t.TempDir()))
	require.Eventually(t, func() bool {
		for _, m := range server.Methods() {
			if m == "initialize" {
				return true
			}
		}
		return false
	}, time.Second, 5*time.Millisecond)

	// Push a dynamic registration from the server. The Provider's
	// reverse-RPC handler will absorb it into dynamicCaps.
	reg := RegistrationParams{
		Registrations: []Registration{
			{ID: "reg-1", Method: "textDocument/hover"},
		},
	}
	require.NoError(t, server.PushRequest(1, "client/registerCapability", reg))

	require.Eventually(t, func() bool {
		p.capsMu.RLock()
		_, ok := p.dynamicCaps["reg-1"]
		p.capsMu.RUnlock()
		return ok
	}, time.Second, 5*time.Millisecond, "dynamic registration should have landed in dynamicCaps")

	// Force a reconnect: drop the server-side connection, restart
	// accept, then ensureClient again.
	server.CloseConn()
	go server.acceptLoop()
	require.Eventually(t, func() bool {
		select {
		case <-p.client.Done():
			return true
		default:
			return false
		}
	}, time.Second, 5*time.Millisecond)

	require.NoError(t, p.EnsureClient(t.TempDir()))

	// After reconnect, the dynamic table must be empty — the new
	// connection has had no chance to re-register yet.
	p.capsMu.RLock()
	regs := len(p.dynamicCaps)
	p.capsMu.RUnlock()
	require.Equal(t, 0, regs, "dynamicCaps must be reset on reconnect, found %d entries", regs)
}

// TestPassiveProvider_ConnectSpecValidate covers the explicit
// validation surface — an empty connect block must be rejected with a
// clear error; a well-formed block must be accepted.
func TestPassiveProvider_ConnectSpecValidate(t *testing.T) {
	cases := []struct {
		name string
		c    *ConnectSpec
		want string
	}{
		{
			name: "both empty",
			c:    &ConnectSpec{Network: "", Address: ""},
			want: "network and address are required",
		},
		{
			name: "missing network",
			c:    &ConnectSpec{Network: "", Address: "127.0.0.1:7677"},
			want: "network is required",
		},
		{
			name: "missing address",
			c:    &ConnectSpec{Network: "tcp", Address: ""},
			want: "address is required",
		},
		{
			name: "bad network",
			c:    &ConnectSpec{Network: "wss", Address: "127.0.0.1:1"},
			want: "unsupported network",
		},
		{
			name: "tcp ok",
			c:    &ConnectSpec{Network: "tcp", Address: "127.0.0.1:7677"},
			want: "",
		},
		{
			name: "unix ok",
			c:    &ConnectSpec{Network: "unix", Address: "/tmp/lsp.sock"},
			want: "",
		},
		{
			name: "nil ok",
			c:    nil,
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.c.Validate()
			if tc.want == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

// TestPassiveConfig_LoadAcceptsConnectBlock confirms that the YAML
// loader binds a passive-attach block under
// `semantic.providers[*].connect` into the typed config struct.
func TestPassiveConfig_LoadAcceptsConnectBlock(t *testing.T) {
	const yaml = `
semantic:
  providers:
    - name: gopls
      enabled: true
      languages: [go]
      connect:
        network: tcp
        address: 127.0.0.1:7677
        fallback_spawn: false
`
	cfg := loadInlineConfig(t, yaml)
	require.Len(t, cfg.Semantic.Providers, 1)
	pc := cfg.Semantic.Providers[0]
	require.Equal(t, "gopls", pc.Name)
	require.NotNil(t, pc.Connect)
	require.Equal(t, "tcp", pc.Connect.Network)
	require.Equal(t, "127.0.0.1:7677", pc.Connect.Address)
	require.False(t, pc.Connect.FallbackSpawn)
}

// TestPassiveConfig_LoadRejectsEmptyConnectBlock asserts that a
// `connect: {}` (both fields blank) is refused by the validation pass.
func TestPassiveConfig_LoadRejectsEmptyConnectBlock(t *testing.T) {
	const yaml = `
semantic:
  providers:
    - name: gopls
      enabled: true
      languages: [go]
      connect:
        network: ""
        address: ""
`
	v := viper.New()
	v.SetConfigType("yaml")
	require.NoError(t, v.ReadConfig(strings.NewReader(yaml)))
	cfg := config.Default()
	require.NoError(t, v.Unmarshal(cfg))
	// Trigger validation explicitly by re-using the published Load
	// path — Default + Unmarshal does not validate; the load loop
	// does. We call the test surface directly because Load reads
	// from disk, not a string.
	err := cfg.ValidateSemanticConnectForTest()
	require.Error(t, err)
	require.Contains(t, err.Error(), "network and address are required")
}

// loadInlineConfig parses a YAML string into a Config using the same
// viper machinery `config.Load` uses, then runs the same validation
// pass. Returns the populated Config on success; fails the test on
// either parse or validation error.
func loadInlineConfig(t *testing.T, yamlText string) *config.Config {
	t.Helper()
	v := viper.New()
	v.SetConfigType("yaml")
	require.NoError(t, v.ReadConfig(strings.NewReader(yamlText)))
	cfg := config.Default()
	require.NoError(t, v.Unmarshal(cfg))
	require.NoError(t, cfg.ValidateSemanticConnectForTest())
	return cfg
}
