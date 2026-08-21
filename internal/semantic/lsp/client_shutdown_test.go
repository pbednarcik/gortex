package lsp

import (
	"bytes"
	"io"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// captureTransport is an in-memory Transport that records every frame the
// client writes and serves an idle read side. SendsShutdown() is true, so
// Shutdown must deliver the LSP shutdown/exit handshake through it.
type captureTransport struct {
	mu     sync.Mutex
	frames bytes.Buffer
	rd     *io.PipeReader
	wr     *io.PipeWriter
}

func (t *captureTransport) Start() (io.WriteCloser, io.Reader, error) {
	t.rd, t.wr = io.Pipe()
	return t, t.rd, nil
}

func (t *captureTransport) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.frames.Write(p)
}

func (t *captureTransport) Close() error { return nil }

func (t *captureTransport) Stop() error {
	if t.wr != nil {
		_ = t.wr.Close() // EOF the read side so readResponses exits
	}
	return nil
}

func (t *captureTransport) SendsShutdown() bool { return true }
func (t *captureTransport) Description() string { return "capture" }

func (t *captureTransport) written() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.frames.String()
}

// Shutdown must deliver the LSP shutdown request and exit notification
// BEFORE the client refuses sends. Setting c.closed first made send()
// reject both frames, so no spawned server ever received a clean exit —
// teardown silently relied on stdin EOF alone, and a server that ignores
// EOF wedged Close forever.
func TestClientShutdown_SendsHandshakeBeforeClosing(t *testing.T) {
	tr := &captureTransport{}
	c, err := NewClientWithTransport(tr, zap.NewNop())
	require.NoError(t, err)

	require.NoError(t, c.Shutdown())
	out := tr.written()
	assert.Contains(t, out, `"method":"shutdown"`, "the shutdown request must reach the wire")
	assert.Contains(t, out, `"method":"exit"`, "the exit notification must reach the wire")
	assert.Less(t,
		strings.Index(out, `"method":"shutdown"`), strings.Index(out, `"method":"exit"`),
		"LSP orders shutdown before exit")

	// Idempotent: a second Shutdown neither blocks nor re-sends.
	require.NoError(t, c.Shutdown())
	assert.Equal(t, out, tr.written())
}

// A spawned server that ignores stdin EOF must not wedge teardown: Stop
// waits spawnStopGrace for a voluntary exit, then kills and reaps. Close
// must ALWAYS return — a drain's cancelRepo waiter, and therefore a
// repository mutation, sits behind it.
func TestSpawnTransport_StopKillsAStubbornServer(t *testing.T) {
	tr := &SpawnTransport{Command: "sleep", Args: []string{"300"}}
	if runtime.GOOS == "windows" {
		// ping -t runs until killed and ignores stdin — the stubborn shape.
		tr = &SpawnTransport{Command: "ping", Args: []string{"-t", "127.0.0.1"}}
	}
	stdin, _, err := tr.Start()
	require.NoError(t, err)

	prev := spawnStopGrace
	spawnStopGrace = 200 * time.Millisecond
	defer func() { spawnStopGrace = prev }()

	_ = stdin.Close()
	done := make(chan error, 1)
	go func() { done <- tr.Stop() }()
	select {
	case <-done:
		// A kill-reaped process surfaces an exit error; the value is
		// irrelevant — returning at all is the contract.
	case <-time.After(5 * time.Second):
		t.Fatal("Stop wedged on a server that ignores stdin EOF")
	}

	// Idempotent: a second Stop returns the memoized result immediately.
	require.NotPanics(t, func() { _ = tr.Stop() })
}
