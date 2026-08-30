package lsp

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLSP_Client_CallTimesOut verifies that a bounded callTimeout unblocks a
// Call against a server that received the request but never replies — the
// csharp-ls-stuck-in-MSBuild failure mode. Without the bound this Call would
// block forever and stall the enrichment WaitGroup.
func TestLSP_Client_CallTimesOut(t *testing.T) {
	c, serverIn, _, cleanup := newPipedClient(t)
	defer cleanup()

	c.SetCallTimeout(50 * time.Millisecond)

	// Fake server: consume the request so the client's framed send
	// completes, then go silent — never write a response.
	go func() {
		_, _ = readFramed(serverIn)
	}()

	done := make(chan error, 1)
	go func() { done <- c.Call("test/never", nil, nil) }()

	select {
	case err := <-done:
		require.Error(t, err)
		assert.Contains(t, err.Error(), "timeout")
		assert.Contains(t, err.Error(), "test/never")
	case <-time.After(2 * time.Second):
		t.Fatal("Call did not return after its call timeout — the timeout case never fired")
	}
}

// TestLSP_Client_CallTimeoutSendsCancel verifies the timeout arm tells the
// server to stop: when a Call burns its whole budget, the client sends a
// $/cancelRequest notification carrying the abandoned request's id. Without
// it the server keeps computing an answer nobody will read — timed-out
// references/callHierarchy calls saturate server slots for minutes.
func TestLSP_Client_CallTimeoutSendsCancel(t *testing.T) {
	c, serverIn, _, cleanup := newPipedClient(t)
	defer cleanup()

	c.SetCallTimeout(50 * time.Millisecond)

	// Fake server: consume the request without ever answering it, then
	// capture the next client→server message — expected to be the cancel.
	type cancelFrame struct {
		Method string `json:"method"`
		Params struct {
			ID int64 `json:"id"`
		} `json:"params"`
	}
	requestID := make(chan int64, 1)
	next := make(chan cancelFrame, 1)
	go func() {
		body, ok := readFramed(serverIn)
		if !ok {
			return
		}
		var req jsonRPCRequest
		_ = json.Unmarshal(body, &req)
		requestID <- req.ID
		body, ok = readFramed(serverIn)
		if !ok {
			return
		}
		var f cancelFrame
		_ = json.Unmarshal(body, &f)
		next <- f
	}()

	err := c.Call("test/never", nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "timeout")

	id := <-requestID
	select {
	case f := <-next:
		assert.Equal(t, "$/cancelRequest", f.Method)
		assert.Equal(t, id, f.Params.ID)
	case <-time.After(2 * time.Second):
		t.Fatal("no $/cancelRequest reached the server after the call timed out")
	}
}

// TestLSP_Client_CallHonorsTimeoutHappyPath verifies the timer case does not
// disturb the normal round-trip: a server that replies well within the bound
// still resolves successfully.
func TestLSP_Client_CallHonorsTimeoutHappyPath(t *testing.T) {
	c, serverIn, serverOut, cleanup := newPipedClient(t)
	defer cleanup()

	c.SetCallTimeout(5 * time.Second)

	go func() {
		body, ok := readFramed(serverIn)
		if !ok {
			return
		}
		var req jsonRPCRequest
		_ = json.Unmarshal(body, &req)
		writeFramed(t, serverOut, jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  json.RawMessage(`"ok"`),
		})
	}()

	var result string
	require.NoError(t, c.Call("test/echo", nil, &result))
	assert.Equal(t, "ok", result)
}

// TestLSP_Client_ReadLoopDropsOnMalformedFrames verifies the read loop does
// not spin forever on a server that emits a run of Content-Length-less header
// blocks: past the bounded tolerance it drops the connection (closing done so
// pending Call()s unblock) instead of burning a core on `continue`.
func TestLSP_Client_ReadLoopDropsOnMalformedFrames(t *testing.T) {
	c, _, serverOut, cleanup := newPipedClient(t)
	defer cleanup()

	// Each blank line is one header block that terminates immediately with
	// no Content-Length — a malformed frame. Emit comfortably more than the
	// drop threshold in one write; bufio buffers them, so readResponses
	// processes them without the pipe write blocking past the drop point.
	go func() {
		_, _ = serverOut.Write([]byte(strings.Repeat("\r\n", 128)))
	}()

	select {
	case <-c.Done():
		// readResponses returned and closed done — the spin guard fired.
	case <-time.After(2 * time.Second):
		t.Fatal("read loop did not drop the connection on a flood of malformed frames")
	}
}
