package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func boolp(b bool) *bool { return &b }

// recordingRemote returns an httptest server plus a flag that flips when
// it is hit, so a test can assert a gate fired BEFORE any outbound call.
func recordingRemote(t *testing.T) (*httptest.Server, *bool) {
	t.Helper()
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		_, _ = w.Write([]byte(`{"from":"remote"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &hit
}

// TestRouter_DisabledRemoteRefused asserts an explicit route to a
// disabled remote returns the structured remote_disabled refusal and
// never sends a request to the remote.
func TestRouter_DisabledRemoteRefused(t *testing.T) {
	srv, hit := recordingRemote(t)
	cfg := &ServersConfig{Server: []ServerEntry{{Slug: "r2", URL: srv.URL, Default: true, Enabled: boolp(false)}}}
	router := NewRouter(RouterConfig{
		Servers:     cfg,
		Rosters:     NewWorkspaceRosterCache(time.Minute),
		CwdResolver: func(string) (string, bool) { return "", false },
		LocalSlug:   LocalServerSentinel,
		LocalExecute: func(context.Context, string, []byte) ([]byte, int, error) {
			t.Error("local executor must not run for an explicit disabled-remote route")
			return nil, 0, nil
		},
	})
	out, status, err := router.RouteToolCall(context.Background(), "find_usages", []byte(`{}`), RouteContext{})
	if err != nil {
		t.Fatalf("RouteToolCall: %v", err)
	}
	if status != http.StatusForbidden {
		t.Fatalf("disabled remote should refuse with 403, got %d", status)
	}
	var env map[string]any
	_ = json.Unmarshal(out, &env)
	if env["error_code"] != "remote_disabled" {
		t.Fatalf("expected remote_disabled, got %v", env["error_code"])
	}
	if *hit {
		t.Fatal("the disabled remote must never be contacted")
	}
}

// TestRouter_WriteGateRefusesRemote asserts every effectful tool routed to an
// enabled remote is refused before any outbound HTTP. Session-only effects are
// intentionally not IsMutating (so planning-mode recovery remains possible),
// but they must still stay machine-local.
func TestRouter_WriteGateRefusesRemote(t *testing.T) {
	srv, hit := recordingRemote(t)
	cfg := &ServersConfig{Server: []ServerEntry{{Slug: "r2", URL: srv.URL, Default: true}}}
	router := NewRouter(RouterConfig{
		Servers:     cfg,
		Rosters:     NewWorkspaceRosterCache(time.Minute),
		CwdResolver: func(string) (string, bool) { return "", false },
		LocalSlug:   LocalServerSentinel,
		LocalExecute: func(context.Context, string, []byte) ([]byte, int, error) {
			return []byte(`{}`), 200, nil
		},
	})
	tools := []string{
		// Durable legacy writers.
		"edit_file", "batch_edit", "rename_symbol", "track_repository",
		// Facade-v1 session writers.
		"session", "overlay",
		// Legacy session writer.
		"proxy_enable",
	}
	for _, tool := range tools {
		if !IsEffectful(tool) {
			t.Fatalf("test fixture %q must be classified as effectful", tool)
		}
		out, status, err := router.RouteToolCall(context.Background(), tool, []byte(`{}`), RouteContext{})
		if err != nil {
			t.Fatalf("%s: %v", tool, err)
		}
		if status != http.StatusForbidden {
			t.Fatalf("%s to a remote should refuse with 403, got %d", tool, status)
		}
		var env map[string]any
		_ = json.Unmarshal(out, &env)
		if env["error_code"] != "remote_read_only" {
			t.Fatalf("%s: expected remote_read_only, got %v", tool, env["error_code"])
		}
	}
	if *hit {
		t.Fatal("a write tool must never reach a remote")
	}
}

// TestRouter_LocalWriteAllowed asserts the write-gate is remote-only: a
// mutating tool that resolves locally runs normally.
func TestRouter_LocalWriteAllowed(t *testing.T) {
	ran := false
	// No roster => everything resolves local.
	router := NewRouter(RouterConfig{
		Servers: &ServersConfig{},
		LocalExecute: func(context.Context, string, []byte) ([]byte, int, error) {
			ran = true
			return []byte(`{"ok":true}`), 200, nil
		},
	})
	if _, _, err := router.RouteToolCall(context.Background(), "edit_file", []byte(`{}`), RouteContext{}); err != nil {
		t.Fatalf("local write: %v", err)
	}
	if !ran {
		t.Fatal("a local write must run (the gate is remote-only)")
	}
}

// TestRouter_EffectiveEnabledRemotes covers the precedence rules: global
// Enabled, session override (both directions), fail-closed for absent,
// and ignore a session override for a slug not in the roster.
func TestRouter_EffectiveEnabledRemotes(t *testing.T) {
	cfg := &ServersConfig{Server: []ServerEntry{
		{Slug: "r2", URL: "https://r2:4747"},                        // global on (nil)
		{Slug: "r3", URL: "https://r3:4747", Enabled: boolp(false)}, // global off
	}}
	router := NewRouter(RouterConfig{Servers: cfg, LocalSlug: LocalServerSentinel})

	slugs := func(es []ServerEntry) map[string]bool {
		m := map[string]bool{}
		for _, e := range es {
			m[e.Slug] = true
		}
		return m
	}

	// No session: r2 on, r3 off.
	g := slugs(router.EffectiveEnabledRemotes(nil))
	if !g["r2"] || g["r3"] {
		t.Fatalf("global: want {r2}, got %v", g)
	}

	// Session disables r2 (beats global on) and enables r3 (beats global off).
	sess := &Session{ID: "a"}
	sess.SetRemoteOverride("r2", false)
	sess.SetRemoteOverride("r3", true)
	sess.SetRemoteOverride("ghost", true) // not in roster — must be ignored
	s := slugs(router.EffectiveEnabledRemotes(sess))
	if s["r2"] {
		t.Error("session off must beat global on")
	}
	if !s["r3"] {
		t.Error("session on must beat global off")
	}
	if s["ghost"] {
		t.Error("a session override for a slug not in the roster must be ignored")
	}
}

// TestRouter_ReloadConfig_ConcurrentNoRace hammers ReloadConfig against
// concurrent routing + reads; the race detector must stay clean and no
// call may panic on a torn router.
//
// The reloaded roster points at a local httptest server, and the cwd resolver
// is stubbed, because every routing iteration here reaches the remote hop for
// real: with a bogus host each of the ~530 proxied calls paid a full DNS
// resolution, which is cheap only where a single-label name fails fast. On
// Windows, where the resolver also walks the DNS suffix list and falls back to
// LLMNR/NetBIOS, that turned a 0.2s test into a 183s one.
func TestRouter_ReloadConfig_ConcurrentNoRace(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"from":"remote"}`))
	}))
	t.Cleanup(remote.Close)
	router := NewRouter(RouterConfig{
		Servers:     &ServersConfig{},
		Rosters:     NewWorkspaceRosterCache(time.Minute),
		CwdResolver: func(string) (string, bool) { return "", false },
		LocalExecute: func(context.Context, string, []byte) ([]byte, int, error) {
			return []byte(`{}`), 200, nil
		},
	})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				switch (i + j) % 3 {
				case 0:
					cfg := &ServersConfig{Server: []ServerEntry{{Slug: "r2", URL: remote.URL}}}
					router.ReloadConfig(cfg, NewWorkspaceRosterCache(time.Minute))
				case 1:
					_, _, _ = router.RouteToolCall(context.Background(), "find_usages", []byte(`{}`), RouteContext{})
				default:
					_ = router.CurrentConfig()
					_ = router.EffectiveEnabledRemotes(nil)
				}
			}
		}(i)
	}
	wg.Wait()
}

// TestRouter_ReloadConfig_ReflectsNewRoster asserts a swap is observed by
// the next call.
func TestRouter_ReloadConfig_ReflectsNewRoster(t *testing.T) {
	router := NewRouter(RouterConfig{Servers: &ServersConfig{}, LocalSlug: LocalServerSentinel})
	if got := len(router.CurrentConfig().Server); got != 0 {
		t.Fatalf("initial roster should be empty, got %d", got)
	}
	router.ReloadConfig(&ServersConfig{Server: []ServerEntry{{Slug: "r2", URL: "https://r2:4747"}}}, NewWorkspaceRosterCache(time.Minute))
	if got := len(router.CurrentConfig().Server); got != 1 {
		t.Fatalf("after reload roster should hold 1, got %d", got)
	}
	if e := router.EffectiveEnabledRemotes(nil); len(e) != 1 || e[0].Slug != "r2" {
		t.Fatalf("enabled set should reflect the reloaded roster, got %v", e)
	}
}
