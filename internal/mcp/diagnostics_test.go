package mcp

import (
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/semantic/lsp"
)

// wantFileURI is the file:// URI the broadcasters must publish for the
// POSIX-shaped fixture path p.
//
// On POSIX p is already absolute and the URI is the literal "file://" + p.
// On Windows the very same string is drive-RELATIVE — filepath.IsAbs
// ("/work/main.go") is false there — so lspuri.PathToURI resolves it
// against the current drive before building the URI and the result carries
// that drive: "file:///D:/work/main.go". Deriving only the Windows
// expectation keeps the POSIX assertion an exact literal.
func wantFileURI(t *testing.T, p string) string {
	t.Helper()
	if runtime.GOOS != "windows" {
		return "file://" + p
	}
	abs, err := filepath.Abs(p)
	require.NoError(t, err)
	return "file:///" + filepath.ToSlash(abs)
}

// fakeSpecificSender records every SendNotificationToSpecificClient
// call so tests can assert delivery target + payload.
type fakeSpecificSender struct {
	mu    sync.Mutex
	calls []fakeSpecificCall
}

type fakeSpecificCall struct {
	sessionID string
	method    string
	params    map[string]any
}

func (f *fakeSpecificSender) SendNotificationToSpecificClient(sessionID, method string, params map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeSpecificCall{sessionID: sessionID, method: method, params: params})
	return nil
}

func (f *fakeSpecificSender) snapshot() []fakeSpecificCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakeSpecificCall, len(f.calls))
	copy(out, f.calls)
	return out
}

func (f *fakeSpecificSender) sessionsTargeted() map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]int)
	for _, c := range f.calls {
		out[c.sessionID]++
	}
	return out
}

// noSnapshot is a snapFn used by tests that don't exercise the
// initial-state replay path.
func noSnapshot() []lsp.DiagnosticsEntry { return nil }

// TestDiagnosticsBroadcaster_NoSubscribers — no subscribers means no
// SendNotification calls. The hash IS still recorded so a late
// subscriber doesn't get a stale replay through the publish path.
func TestDiagnosticsBroadcaster_NoSubscribers(t *testing.T) {
	fake := &fakeSpecificSender{}
	b := newDiagnosticsBroadcaster(fake, noSnapshot, zap.NewNop())

	diags := []lsp.Diagnostic{{Message: "missing semicolon"}}
	b.publish("gopls", "/abs/path/main.go", diags)

	assert.Empty(t, fake.snapshot(), "no subscribers — no broadcast")

	b.subscribe("session-A", subscribeOptions{})
	b.publish("gopls", "/abs/path/main.go", diags)
	assert.Empty(t, fake.snapshot(), "duplicate payload still suppressed by delta filter")
}

// TestDiagnosticsBroadcaster_PerSessionDelivery — only subscribed
// sessions receive notifications, and they receive exactly one each.
// A non-subscribed session must NEVER appear in the call log.
func TestDiagnosticsBroadcaster_PerSessionDelivery(t *testing.T) {
	fake := &fakeSpecificSender{}
	b := newDiagnosticsBroadcaster(fake, noSnapshot, zap.NewNop())

	b.subscribe("session-A", subscribeOptions{})
	b.subscribe("session-B", subscribeOptions{})
	// session-C never subscribes.

	b.publish("gopls", "/work/main.go", []lsp.Diagnostic{{Message: "x"}})

	targets := fake.sessionsTargeted()
	assert.Equal(t, 1, targets["session-A"])
	assert.Equal(t, 1, targets["session-B"])
	assert.Equal(t, 0, targets["session-C"])
}

// TestDiagnosticsBroadcaster_DeltaFilter — identical re-publishes are
// suppressed; payload changes produce a new delivery.
func TestDiagnosticsBroadcaster_DeltaFilter(t *testing.T) {
	fake := &fakeSpecificSender{}
	b := newDiagnosticsBroadcaster(fake, noSnapshot, zap.NewNop())
	b.subscribe("session-A", subscribeOptions{})

	first := []lsp.Diagnostic{{Message: "one"}}
	second := []lsp.Diagnostic{{Message: "two"}}

	b.publish("gopls", "/work/main.go", first)
	b.publish("gopls", "/work/main.go", first)
	b.publish("gopls", "/work/main.go", second)
	b.publish("gopls", "/work/main.go", second)

	require.Len(t, fake.snapshot(), 2, "exactly two deliveries after delta filter")
}

// TestDiagnosticsBroadcaster_ClearPrunesHash — a file clearing its
// diagnostics broadcasts the clear exactly once (only when it was
// previously non-empty) and drops its lastHash key, so the map doesn't
// retain an entry per file the server ever touched.
func TestDiagnosticsBroadcaster_ClearPrunesHash(t *testing.T) {
	fake := &fakeSpecificSender{}
	b := newDiagnosticsBroadcaster(fake, noSnapshot, zap.NewNop())
	b.subscribe("session-A", subscribeOptions{})

	const path = "/work/main.go"
	b.publish("gopls", path, []lsp.Diagnostic{{Message: "boom", Severity: 1}}) // non-empty
	b.publish("gopls", path, nil)                                              // cleared
	b.publish("gopls", path, nil)                                              // still empty — suppressed

	calls := fake.snapshot()
	require.Len(t, calls, 2, "one non-empty publish + one clear; the second clear is suppressed")

	// The clear delivery carries an empty diagnostics list.
	clear, _ := calls[1].params["diagnostics"].([]map[string]any)
	assert.Empty(t, clear, "clear broadcast carries no diagnostics")

	// The key is pruned, so lastHash doesn't grow one entry per touched file.
	b.mu.RLock()
	_, present := b.lastHash[path]
	b.mu.RUnlock()
	assert.False(t, present, "lastHash key removed after clear")
}

// TestDiagnosticsBroadcaster_ClearWithoutPriorSuppressed — an empty
// publish for a file that never had diagnostics is not broadcast (no
// spurious clear) and leaves no lastHash entry.
func TestDiagnosticsBroadcaster_ClearWithoutPriorSuppressed(t *testing.T) {
	fake := &fakeSpecificSender{}
	b := newDiagnosticsBroadcaster(fake, noSnapshot, zap.NewNop())
	b.subscribe("session-A", subscribeOptions{})

	b.publish("gopls", "/work/clean.go", nil)

	assert.Empty(t, fake.snapshot(), "no prior diagnostics — clear suppressed")
	b.mu.RLock()
	_, present := b.lastHash["/work/clean.go"]
	b.mu.RUnlock()
	assert.False(t, present)
}

// TestDiagnosticsBroadcaster_PayloadShape — payload carries the
// expected fields and the wire-form diagnostics list.
func TestDiagnosticsBroadcaster_PayloadShape(t *testing.T) {
	fake := &fakeSpecificSender{}
	b := newDiagnosticsBroadcaster(fake, noSnapshot, zap.NewNop())
	b.subscribe("session-A", subscribeOptions{})

	diags := []lsp.Diagnostic{{Message: "boom", Severity: 1}}
	b.publish("gopls", "/work/main.go", diags)

	calls := fake.snapshot()
	require.Len(t, calls, 1)
	assert.Equal(t, "session-A", calls[0].sessionID)
	assert.Equal(t, "notifications/diagnostics", calls[0].method)
	assert.Equal(t, wantFileURI(t, "/work/main.go"), calls[0].params["uri"])
	assert.Equal(t, "/work/main.go", calls[0].params["path"])
	assert.Equal(t, "gopls", calls[0].params["server"])
	wire, ok := calls[0].params["diagnostics"].([]map[string]any)
	require.True(t, ok, "diagnostics should be wire-form slice")
	assert.Len(t, wire, 1)
}

// TestDiagnosticsBroadcaster_SeverityFilter — a min_severity=2
// subscriber receives errors and warnings but not info / hint.
// Severity-0 (unset) entries always pass through.
func TestDiagnosticsBroadcaster_SeverityFilter(t *testing.T) {
	fake := &fakeSpecificSender{}
	b := newDiagnosticsBroadcaster(fake, noSnapshot, zap.NewNop())
	b.subscribe("session-A", subscribeOptions{MinSeverity: 2})

	diags := []lsp.Diagnostic{
		{Severity: 1, Message: "err"},
		{Severity: 2, Message: "warn"},
		{Severity: 3, Message: "info"},
		{Severity: 4, Message: "hint"},
		{Severity: 0, Message: "no-severity"},
	}
	b.publish("gopls", "/work/main.go", diags)

	calls := fake.snapshot()
	require.Len(t, calls, 1)
	wire := calls[0].params["diagnostics"].([]map[string]any)
	require.Len(t, wire, 3, "expected err + warn + no-severity through the filter")
}

// TestDiagnosticsBroadcaster_PathPrefixFilter — a subscriber scoped
// to /work/api/ does not receive diagnostics for /work/web/.
func TestDiagnosticsBroadcaster_PathPrefixFilter(t *testing.T) {
	fake := &fakeSpecificSender{}
	b := newDiagnosticsBroadcaster(fake, noSnapshot, zap.NewNop())
	b.subscribe("session-A", subscribeOptions{PathPrefix: "/work/api/"})

	b.publish("gopls", "/work/api/users.go", []lsp.Diagnostic{{Message: "in scope"}})
	b.publish("gopls", "/work/web/page.go", []lsp.Diagnostic{{Message: "out of scope"}})

	calls := fake.snapshot()
	require.Len(t, calls, 1)
	assert.Equal(t, "/work/api/users.go", calls[0].params["path"])
}

// TestDiagnosticsBroadcaster_PerSubscriberFilters — two subscribers
// with different filters receive different slices of the same publish.
func TestDiagnosticsBroadcaster_PerSubscriberFilters(t *testing.T) {
	fake := &fakeSpecificSender{}
	b := newDiagnosticsBroadcaster(fake, noSnapshot, zap.NewNop())
	b.subscribe("errors-only", subscribeOptions{MinSeverity: 1})
	b.subscribe("everything", subscribeOptions{})

	diags := []lsp.Diagnostic{
		{Severity: 1, Message: "err"},
		{Severity: 4, Message: "hint"},
	}
	b.publish("gopls", "/work/main.go", diags)

	bySession := make(map[string][]map[string]any)
	for _, c := range fake.snapshot() {
		bySession[c.sessionID] = c.params["diagnostics"].([]map[string]any)
	}
	require.Len(t, bySession["errors-only"], 1)
	require.Len(t, bySession["everything"], 2)
}

// TestDiagnosticsBroadcaster_InitialReplay — subscribing replays the
// current snapshot for files matching the filter, tagged
// initial_replay=true.
func TestDiagnosticsBroadcaster_InitialReplay(t *testing.T) {
	fake := &fakeSpecificSender{}
	snap := func() []lsp.DiagnosticsEntry {
		return []lsp.DiagnosticsEntry{
			{SpecName: "gopls", AbsPath: "/work/api/main.go", Diagnostics: []lsp.Diagnostic{{Severity: 1, Message: "old"}}},
			{SpecName: "gopls", AbsPath: "/work/web/x.go", Diagnostics: []lsp.Diagnostic{{Severity: 1, Message: "elsewhere"}}},
		}
	}
	b := newDiagnosticsBroadcaster(fake, snap, zap.NewNop())
	replayed := b.subscribe("session-A", subscribeOptions{PathPrefix: "/work/api/"})
	assert.Equal(t, 1, replayed, "only /work/api/* should replay")

	calls := fake.snapshot()
	require.Len(t, calls, 1)
	assert.Equal(t, true, calls[0].params["initial_replay"])
	assert.Equal(t, "/work/api/main.go", calls[0].params["path"])
}

// TestDiagnosticsBroadcaster_Resubscribe — resubscribing with new
// options overwrites the previous filter and re-replays.
func TestDiagnosticsBroadcaster_Resubscribe(t *testing.T) {
	fake := &fakeSpecificSender{}
	snap := func() []lsp.DiagnosticsEntry {
		return []lsp.DiagnosticsEntry{
			{SpecName: "gopls", AbsPath: "/work/a.go", Diagnostics: []lsp.Diagnostic{{Severity: 1}}},
		}
	}
	b := newDiagnosticsBroadcaster(fake, snap, zap.NewNop())
	b.subscribe("session-A", subscribeOptions{PathPrefix: "/elsewhere/"})
	b.subscribe("session-A", subscribeOptions{PathPrefix: "/work/"})

	// Two replay attempts — first found nothing in scope, second
	// found one match.
	calls := fake.snapshot()
	require.Len(t, calls, 1)
	assert.Equal(t, "/work/a.go", calls[0].params["path"])
}

// TestDiagnosticsBroadcaster_Unsubscribe — after unsubscribe the
// session stops receiving.
func TestDiagnosticsBroadcaster_Unsubscribe(t *testing.T) {
	fake := &fakeSpecificSender{}
	b := newDiagnosticsBroadcaster(fake, noSnapshot, zap.NewNop())

	b.subscribe("session-A", subscribeOptions{})
	b.publish("gopls", "/work/a.go", []lsp.Diagnostic{{Message: "a"}})
	require.Len(t, fake.snapshot(), 1)

	b.unsubscribe("session-A")
	b.publish("gopls", "/work/b.go", []lsp.Diagnostic{{Message: "b"}})
	require.Len(t, fake.snapshot(), 1, "no subscribers — no broadcast")

	assert.Equal(t, 0, b.subscriberCount())
}

// TestDiagnosticsBroadcaster_NilBroadcaster — publish on a broadcaster
// with no underlying server is a safe no-op.
func TestDiagnosticsBroadcaster_NilBroadcaster(t *testing.T) {
	b := newDiagnosticsBroadcaster(nil, noSnapshot, zap.NewNop())
	b.subscribe("session-A", subscribeOptions{})
	// Should not panic.
	b.publish("gopls", "/abs/main.go", []lsp.Diagnostic{{Message: "x"}})
}

// TestServer_ReleaseSession_UnsubscribesDiagnostics — the Server's
// session-lifecycle hook must drop the session out of the diagnostics
// broadcaster so subscriber slots don't leak across the daemon's
// lifetime.
func TestServer_ReleaseSession_UnsubscribesDiagnostics(t *testing.T) {
	fake := &fakeSpecificSender{}
	b := newDiagnosticsBroadcaster(fake, noSnapshot, zap.NewNop())

	srv := &Server{diagBroadcaster: b}
	b.subscribe("session-A", subscribeOptions{})
	require.Equal(t, 1, b.subscriberCount())

	srv.ReleaseSession("session-A")
	assert.Equal(t, 0, b.subscriberCount(), "ReleaseSession should drop the subscriber")
}

// TestPathToFileURI_Absolute — an absolute path round-trips into the
// expected file:// URI.
func TestPathToFileURI_Absolute(t *testing.T) {
	assert.Equal(t, wantFileURI(t, "/work/main.go"), pathToFileURI("/work/main.go"))
	assert.Equal(t, "", pathToFileURI(""))
}

// TestHashDiagnostics_Stable — identical content hashes match;
// different content does not.
func TestHashDiagnostics_Stable(t *testing.T) {
	a := []lsp.Diagnostic{{Message: "x", Severity: 2}}
	b := []lsp.Diagnostic{{Message: "x", Severity: 2}}
	assert.Equal(t, hashDiagnostics(a), hashDiagnostics(b))

	c := []lsp.Diagnostic{{Message: "y", Severity: 2}}
	assert.NotEqual(t, hashDiagnostics(a), hashDiagnostics(c))

	assert.Equal(t, "empty", hashDiagnostics(nil))
}

// TestFilterDiagnosticsBySeverity — boundary checks: severity-0 always
// passes, threshold inclusion is ≤, no-filter copies input.
func TestFilterDiagnosticsBySeverity(t *testing.T) {
	in := []lsp.Diagnostic{
		{Severity: 1, Message: "err"},
		{Severity: 2, Message: "warn"},
		{Severity: 0, Message: "blank"},
	}
	assert.Len(t, filterDiagnosticsBySeverity(in, 0), 3, "no filter copies all")
	assert.Len(t, filterDiagnosticsBySeverity(in, 1), 2, "min 1 keeps err + blank")
	assert.Len(t, filterDiagnosticsBySeverity(in, 2), 3, "min 2 keeps everything")
}
