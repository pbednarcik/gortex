package daemon

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/testenv"
)

func TestSessionRegistryRebindsLogicalSession(t *testing.T) {
	registry := NewSessionRegistry()
	h := Handshake{
		Mode:             ModeMCP,
		PID:              os.Getpid(),
		LogicalSessionID: "logical-session-rebind",
		CWD:              "/workspace",
		Tools:            "core",
	}
	proxy1, peer1 := net.Pipe()
	defer peer1.Close()
	first := registry.Register(proxy1, h)
	state := &struct{ value string }{value: "preserved"}
	first.SessionState = state

	if removed := registry.Remove(proxy1); removed != nil {
		t.Fatalf("logical disconnect removed session %q", removed.ID)
	}
	if first.Conn != nil {
		t.Fatal("detached logical session retained stale connection")
	}
	if got := registry.GetByID(h.LogicalSessionID); got != first {
		t.Fatalf("detached session lookup = %p, want %p", got, first)
	}

	proxy2, peer2 := net.Pipe()
	defer peer2.Close()
	second := registry.Register(proxy2, h)
	if second != first {
		t.Fatalf("rebind created a new Session: got %p, want %p", second, first)
	}
	if second.SessionState != state {
		t.Fatal("rebind lost daemon-owned session state")
	}
	if second.Conn != proxy2 || registry.Get(proxy2) != first {
		t.Fatal("new connection was not bound to the logical session")
	}
}

func TestSessionRegistryRejectsLogicalRebindMetadataDrift(t *testing.T) {
	registry := NewSessionRegistry()
	h := Handshake{
		Mode:             ModeMCP,
		PID:              os.Getpid(),
		LogicalSessionID: "logical-session-metadata",
		CWD:              "/workspace-a",
		Tools:            "core",
		ToolsMode:        "hide",
	}
	proxy1, peer1 := net.Pipe()
	defer peer1.Close()
	first := registry.Register(proxy1, h)
	if removed := registry.Remove(proxy1); removed != nil {
		t.Fatal("logical session was removed")
	}

	drifted := h
	drifted.CWD = "/workspace-b"
	proxy2, peer2 := net.Pipe()
	defer proxy2.Close()
	defer peer2.Close()
	second := registry.Register(proxy2, drifted)
	if second == first || second.ID == h.LogicalSessionID {
		t.Fatal("metadata drift rebound the existing logical session")
	}
	if first.CWD != h.CWD || first.ClientPID != h.PID || first.ToolSpec != h.Tools || first.ToolMode != h.ToolsMode {
		t.Fatalf("original session metadata mutated: %+v", first)
	}
	if registry.GetByID(h.LogicalSessionID) != first {
		t.Fatal("original logical session was displaced")
	}
}

type reconnectDispatcher struct {
	mu       sync.Mutex
	sessions []*Session
	ended    int
}

func (d *reconnectDispatcher) Dispatch(_ context.Context, sess *Session, frame []byte) ([]byte, error) {
	d.mu.Lock()
	if sess.SessionState == nil {
		sess.SessionState = &struct{ generation int }{generation: 1}
	}
	d.sessions = append(d.sessions, sess)
	d.mu.Unlock()

	var request struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(frame, &request); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      request.ID,
		"result":  map[string]any{"ok": true},
	})
}

func (d *reconnectDispatcher) SessionEnded(*Session) {
	d.mu.Lock()
	d.ended++
	d.mu.Unlock()
}

func (d *reconnectDispatcher) snapshot() ([]*Session, int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]*Session(nil), d.sessions...), d.ended
}

func TestDaemonRebindsLogicalMCPStateAcrossRealSocketReconnect(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket reconnect test")
	}
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	socketDir := testenv.ShortTempDir(t)
	socket := filepath.Join(socketDir, "s")
	dispatcher := &reconnectDispatcher{}
	server := New(socket, "test", zap.NewNop())
	server.MCPDispatcher = dispatcher
	if err := server.Listen(); err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve() }()
	t.Cleanup(func() {
		_ = server.Shutdown()
		select {
		case <-serveDone:
		case <-time.After(2 * time.Second):
			t.Error("daemon did not stop")
		}
	})

	h := Handshake{
		Version:          ProtocolVersion,
		Mode:             ModeMCP,
		PID:              os.Getpid(),
		LogicalSessionID: "real-socket-logical-session",
		CWD:              t.TempDir(),
	}
	first, err := DialTo(socket, h)
	if err != nil {
		t.Fatal(err)
	}
	firstInstance := first.Ack.DaemonInstance
	if first.Ack.SessionID != h.LogicalSessionID || firstInstance == "" {
		t.Fatalf("first ack = %+v", first.Ack)
	}
	if err := first.WriteMCPFrame([]byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := first.ReadMCPFrame(); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for server.Sessions().IsAttached(h.LogicalSessionID) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if server.Sessions().IsAttached(h.LogicalSessionID) {
		t.Fatal("logical session did not detach from the first socket")
	}

	second, err := DialTo(socket, h)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if second.Ack.SessionID != h.LogicalSessionID || second.Ack.DaemonInstance != firstInstance {
		t.Fatalf("reconnect ack = %+v; first instance %q", second.Ack, firstInstance)
	}
	if err := second.WriteMCPFrame([]byte(`{"jsonrpc":"2.0","id":2,"method":"ping"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := second.ReadMCPFrame(); err != nil {
		t.Fatal(err)
	}

	sessions, ended := dispatcher.snapshot()
	if len(sessions) != 2 {
		t.Fatalf("dispatch count = %d, want 2", len(sessions))
	}
	if sessions[0] != sessions[1] || sessions[0].SessionState != sessions[1].SessionState {
		t.Fatal("real reconnect did not preserve the daemon Session and its state")
	}
	if ended != 0 {
		t.Fatalf("SessionEnded called %d times during reconnect", ended)
	}
}

func TestDaemonReplaysCanonicalIDAfterRealSocketLoss(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket reconnect test")
	}
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	socketDir := testenv.ShortTempDir(t)

	dispatcher := &reconnectDispatcher{}
	server := New(filepath.Join(socketDir, "s"), "test", zap.NewNop())
	server.MCPDispatcher = dispatcher
	if err := server.Listen(); err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve() }()
	t.Cleanup(func() {
		_ = server.Shutdown()
		select {
		case <-serveDone:
		case <-time.After(2 * time.Second):
			t.Error("daemon did not stop")
		}
	})

	h := Handshake{
		Version:          ProtocolVersion,
		Mode:             ModeMCP,
		PID:              os.Getpid(),
		LogicalSessionID: "response-loss-logical-session",
		CWD:              t.TempDir(),
	}
	first, err := DialTo(server.SocketPath, h)
	if err != nil {
		t.Fatal(err)
	}
	firstRequest := []byte(`{"jsonrpc":"2.0","id":"\u003c","method":"tools/call","params":{"name":"read","arguments":{"operation":"source"}}}`)
	if err := first.WriteMCPFrame(firstRequest); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		sessions, _ := dispatcher.snapshot()
		if len(sessions) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first request was not dispatched")
		}
		time.Sleep(time.Millisecond)
	}
	// Dispatch returns before serveMCP publishes the cache; allow that short
	// critical section to complete, then drop the unread response.
	time.Sleep(10 * time.Millisecond)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	for server.Sessions().IsAttached(h.LogicalSessionID) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	second, err := DialTo(server.SocketPath, h)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	secondRequest := []byte(`{"jsonrpc":"2.0","id":"<","method":"tools/call","params":{"name":"read","arguments":{"operation":"source"}}}`)
	if err := second.WriteMCPFrame(secondRequest); err != nil {
		t.Fatal(err)
	}
	response, err := second.ReadMCPFrame()
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(response, &envelope) != nil || envelope.ID != "<" {
		t.Fatalf("cached response changed reconnect request ID value: %s", response)
	}
	sessions, ended := dispatcher.snapshot()
	if len(sessions) != 1 || ended != 0 {
		t.Fatalf("identical replay redispatched: calls=%d ended=%d", len(sessions), ended)
	}

	different := []byte(`{"jsonrpc":"2.0","id":"<","method":"tools/call","params":{"name":"read","arguments":{"operation":"source","symbol":"other"}}}`)
	if err := second.WriteMCPFrame(different); err != nil {
		t.Fatal(err)
	}
	if _, err := second.ReadMCPFrame(); err != nil {
		t.Fatal(err)
	}
	sessions, _ = dispatcher.snapshot()
	if len(sessions) != 2 {
		t.Fatalf("same ID with different payload was deduplicated: calls=%d", len(sessions))
	}
}

func TestDaemonInstanceChangesAcrossServerProcesses(t *testing.T) {
	first := New("first.sock", "test", zap.NewNop())
	second := New("second.sock", "test", zap.NewNop())
	if first.instanceID == "" || second.instanceID == "" || first.instanceID == second.instanceID {
		t.Fatalf("daemon instance IDs are not unique: %q %q", first.instanceID, second.instanceID)
	}
}
