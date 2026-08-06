// gortex-hook is a proof-of-concept THIN agent-hook client.
//
// The production `gortex hook` command links the entire engine — the
// parser (31 tree-sitter grammars via CGO), the MCP surface, the LLM
// providers — into a ~380 MB binary that costs ~280 ms of process
// startup per invocation. Agent harnesses fire PreToolUse hooks on
// EVERY tool call, so a working session pays that cost hundreds of
// times, serialized (~74 s measured over one real session).
//
// This PoC demonstrates the proposed shape:
//
//  1. tracked-roots fast path — read the local config, and when the
//     hook event's cwd is not under any tracked root, exit silently
//     without dialing anything. An untracked session costs one file
//     read.
//  2. slim daemon transport — the daemon socket protocol (handshake +
//     newline-delimited JSON control frames) needs nothing beyond the
//     stdlib; the ~100 lines below are a hand-inlined copy of
//     internal/daemon's client.go/proto.go essentials, which already
//     import stdlib only. Phase 1 of the real change extracts them
//     into a leaf package instead of copying.
//  3. daemon-side dispatch (NOT in this PoC) — a `hook_dispatch`
//     control RPC that takes the raw hook event JSON and returns the
//     response JSON, moving internal/hooks' decision logic behind the
//     daemon boundary where the graph already lives.
//
// Measured on the machine that motivated this (Windows 11, warm
// daemon; ~31 ms of shell-spawn overhead included in every number):
// this binary builds at ~4 MB; the untracked fast path completes in
// ~47 ms wall (~16 ms of process time); dial + handshake + `status`
// lands ~125 ms wall. Two honest caveats on the tracked number: the
// handshake currently performs full per-session bookkeeping, and
// `status` is a heavyweight stand-in — a purpose-built hook_dispatch
// with a lightweight handshake mode should land well under 50 ms.
// The production hook binary measures ~280 ms before ANY work happens.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type hookEvent struct {
	HookEventName string `json:"hook_event_name"`
	CWD           string `json:"cwd"`
}

type handshake struct {
	Version    int    `json:"version"`
	Mode       string `json:"mode"`
	CWD        string `json:"cwd,omitempty"`
	ClientName string `json:"client,omitempty"`
	PID        int    `json:"pid,omitempty"`
}

type handshakeAck struct {
	OK        bool   `json:"ok"`
	ErrorCode string `json:"error_code,omitempty"`
	ErrorMsg  string `json:"error_msg,omitempty"`
	SessionID string `json:"session_id,omitempty"`
}

// trackedRoots reads the `repos:` list from ~/.gortex/config.yaml with a
// deliberately naive line scan — the PoC avoids a YAML dependency; the
// real change would reuse the slim config reader.
func trackedRoots() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(home, ".gortex", "config.yaml"))
	if err != nil {
		return nil
	}
	var roots []string
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(trimmed, "- path:"); ok {
			if p := strings.TrimSpace(rest); p != "" {
				roots = append(roots, filepath.Clean(p))
			}
		}
	}
	return roots
}

func cwdIsTracked(cwd string, roots []string) bool {
	cwd = filepath.Clean(cwd)
	for _, root := range roots {
		rel, err := filepath.Rel(root, cwd)
		if err != nil {
			continue
		}
		if rel == "." || (!strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel)) {
			return true
		}
	}
	return false
}

func socketPath() string {
	if override := os.Getenv("GORTEX_DAEMON_SOCKET"); override != "" {
		return override
	}
	if rt := os.Getenv("XDG_RUNTIME_DIR"); rt != "" && runtime.GOOS == "linux" {
		return filepath.Join(rt, "gortex.sock")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".gortex", "cache", "daemon.sock")
}

func main() {
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return
	}
	var ev hookEvent
	if json.Unmarshal(data, &ev) != nil {
		return
	}

	// Fast path: an untracked cwd gets no enrichment and no daemon
	// dial — the session it belongs to cannot use graph tools anyway.
	roots := trackedRoots()
	if os.Getenv("GORTEX_HOOK_POC_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "poc: cwd=%q roots=%q\n", ev.CWD, roots)
	}
	if ev.CWD == "" || !cwdIsTracked(ev.CWD, roots) {
		return
	}

	// Slim transport: dial + handshake + one control round trip. The
	// PoC calls `status` as a stand-in; the real change adds a
	// `hook_dispatch` control kind that takes the raw event JSON and
	// returns the hook response for this agent protocol.
	d := net.Dialer{Timeout: 500 * time.Millisecond}
	conn, err := d.Dial("unix", socketPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "poc: dial fail %v\n", err)
		return // daemon down: hooks are advisory, degrade silently
	}
	defer conn.Close()

	enc := json.NewEncoder(conn)
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	if enc.Encode(handshake{Version: 1, Mode: "control", CWD: ev.CWD, ClientName: "gortex-hook-poc", PID: os.Getpid()}) != nil {
		return
	}
	reader := bufio.NewReader(conn)
	ackLine, err := reader.ReadBytes('\n')
	if err != nil {
		return
	}
	var ack handshakeAck
	if err := json.Unmarshal(ackLine, &ack); err != nil || !ack.OK {
		fmt.Fprintf(os.Stderr, "poc: ack fail err=%v ack=%+v\n", err, ack)
		return
	}

	req := map[string]any{"kind": "status", "params": map[string]any{}}
	if enc.Encode(req) != nil {
		return
	}
	if _, err := reader.ReadBytes('\n'); err != nil {
		return
	}
	fmt.Fprintln(os.Stderr, "poc: full round trip complete")
}
