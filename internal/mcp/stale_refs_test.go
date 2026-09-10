package mcp

import (
	"context"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

// stateWithViewed builds a sessionState whose working set carries
// the supplied viewed symbols and files. Used to drive the
// intersect logic without standing up the full sessionMap.
func stateWithViewed(symbols, files, modified []string) *sessionState {
	ss := newSessionState()
	for _, s := range symbols {
		ss.recordSymbol(s)
	}
	for _, f := range files {
		ss.recordFile(f)
	}
	for _, m := range modified {
		ss.recordModified(m)
	}
	return ss
}

// TestStaleRefsBroadcaster_NoSubscribers — symbol change with no
// subscribers is a no-op.
func TestStaleRefsBroadcaster_NoSubscribers(t *testing.T) {
	fake := &fakeSpecificSender{}
	b := newStaleRefsBroadcaster(fake, nil, newSessionState(), zap.NewNop())

	b.handleSymbolChange("/x.go",
		[]*graph.Node{{ID: "x.go::Foo", Kind: graph.KindFunction}},
		[]*graph.Node{},
	)
	assert.Empty(t, fake.snapshot())
}

// TestStaleRefsBroadcaster_RemovedSymbol_InWorkingSet — a session
// that viewed Foo sees it in `removed_symbols` when Foo disappears.
func TestStaleRefsBroadcaster_RemovedSymbol_InWorkingSet(t *testing.T) {
	fake := &fakeSpecificSender{}
	defaults := stateWithViewed([]string{"x.go::Foo"}, nil, nil)
	b := newStaleRefsBroadcaster(fake, nil, defaults, zap.NewNop())
	b.subscribe("A")

	b.handleSymbolChange("/x.go",
		[]*graph.Node{{ID: "x.go::Foo", Kind: graph.KindFunction}},
		[]*graph.Node{},
	)
	calls := fake.snapshot()
	require.Len(t, calls, 1)
	assert.Equal(t, "/x.go", calls[0].params["path"])
	assert.Equal(t, wantFileURI(t, "/x.go"), calls[0].params["uri"])
	removed := calls[0].params["removed_symbols"].([]string)
	require.Len(t, removed, 1)
	assert.Equal(t, "x.go::Foo", removed[0])
}

// TestStaleRefsBroadcaster_ChangedSignature_InWorkingSet — a
// session that viewed Foo sees it in `changed_signatures` when the
// signature mutates.
func TestStaleRefsBroadcaster_ChangedSignature_InWorkingSet(t *testing.T) {
	fake := &fakeSpecificSender{}
	defaults := stateWithViewed([]string{"x.go::Foo"}, nil, nil)
	b := newStaleRefsBroadcaster(fake, nil, defaults, zap.NewNop())
	b.subscribe("A")

	oldNode := &graph.Node{ID: "x.go::Foo", Kind: graph.KindFunction, Meta: map[string]any{"signature": "func Foo() int"}}
	newNode := &graph.Node{ID: "x.go::Foo", Kind: graph.KindFunction, Meta: map[string]any{"signature": "func Foo(x string) int"}}

	b.handleSymbolChange("/x.go",
		[]*graph.Node{oldNode},
		[]*graph.Node{newNode},
	)
	calls := fake.snapshot()
	require.Len(t, calls, 1)
	changed := calls[0].params["changed_signatures"].([]string)
	require.Len(t, changed, 1)
	assert.Equal(t, "x.go::Foo", changed[0])
}

// TestStaleRefsBroadcaster_OutOfWorkingSet_Skipped — a session
// that never touched Foo sees no notification when Foo changes.
func TestStaleRefsBroadcaster_OutOfWorkingSet_Skipped(t *testing.T) {
	fake := &fakeSpecificSender{}
	defaults := stateWithViewed([]string{"y.go::Bar"}, nil, nil)
	b := newStaleRefsBroadcaster(fake, nil, defaults, zap.NewNop())
	b.subscribe("A")

	b.handleSymbolChange("/x.go",
		[]*graph.Node{{ID: "x.go::Foo", Kind: graph.KindFunction}},
		[]*graph.Node{},
	)
	assert.Empty(t, fake.snapshot(), "Foo is not in the session's working set")
}

// TestStaleRefsBroadcaster_ViewedFile_FiresWithoutSymbolHit — when
// the session has viewed the file but no specific symbol matches,
// it still gets a notification with viewed_file=true so it knows
// the file it cares about churned.
func TestStaleRefsBroadcaster_ViewedFile_FiresWithoutSymbolHit(t *testing.T) {
	fake := &fakeSpecificSender{}
	defaults := stateWithViewed(nil, []string{"/x.go"}, nil)
	b := newStaleRefsBroadcaster(fake, nil, defaults, zap.NewNop())
	b.subscribe("A")

	b.handleSymbolChange("/x.go",
		[]*graph.Node{{ID: "x.go::Foo", Kind: graph.KindFunction}},
		[]*graph.Node{},
	)
	calls := fake.snapshot()
	require.Len(t, calls, 1)
	assert.Equal(t, true, calls[0].params["viewed_file"])
	assert.Empty(t, calls[0].params["removed_symbols"])
}

// TestStaleRefsBroadcaster_ModifiedFile_TreatedAsViewed — modified
// files behave like viewed files for the staleness check.
func TestStaleRefsBroadcaster_ModifiedFile_TreatedAsViewed(t *testing.T) {
	fake := &fakeSpecificSender{}
	defaults := stateWithViewed(nil, nil, []string{"/x.go"})
	b := newStaleRefsBroadcaster(fake, nil, defaults, zap.NewNop())
	b.subscribe("A")

	b.handleSymbolChange("/x.go",
		[]*graph.Node{{ID: "x.go::Foo", Kind: graph.KindFunction}},
		[]*graph.Node{},
	)
	calls := fake.snapshot()
	require.Len(t, calls, 1)
	assert.Equal(t, true, calls[0].params["viewed_file"])
}

// TestStaleRefsBroadcaster_DeltaFilter — identical
// (removed, changed, viewed_file) tuples are suppressed.
func TestStaleRefsBroadcaster_DeltaFilter(t *testing.T) {
	fake := &fakeSpecificSender{}
	defaults := stateWithViewed([]string{"x.go::Foo"}, nil, nil)
	b := newStaleRefsBroadcaster(fake, nil, defaults, zap.NewNop())
	b.subscribe("A")

	for i := 0; i < 3; i++ {
		b.handleSymbolChange("/x.go",
			[]*graph.Node{{ID: "x.go::Foo", Kind: graph.KindFunction}},
			[]*graph.Node{},
		)
	}
	assert.Len(t, fake.snapshot(), 1, "delta filter must suppress repeated identical change reports")
}

// TestStaleRefsBroadcaster_DeltaFilter_ResetOnResubscribe — when a
// session unsubscribes and resubscribes the per-session delta hash
// is cleared so a fresh identical change re-fires.
func TestStaleRefsBroadcaster_DeltaFilter_ResetOnResubscribe(t *testing.T) {
	fake := &fakeSpecificSender{}
	defaults := stateWithViewed([]string{"x.go::Foo"}, nil, nil)
	b := newStaleRefsBroadcaster(fake, nil, defaults, zap.NewNop())
	b.subscribe("A")

	b.handleSymbolChange("/x.go",
		[]*graph.Node{{ID: "x.go::Foo", Kind: graph.KindFunction}},
		[]*graph.Node{},
	)
	require.Len(t, fake.snapshot(), 1)

	b.unsubscribe("A")
	b.subscribe("A")
	b.handleSymbolChange("/x.go",
		[]*graph.Node{{ID: "x.go::Foo", Kind: graph.KindFunction}},
		[]*graph.Node{},
	)
	assert.Len(t, fake.snapshot(), 2, "resubscribe must clear the per-session delta cache")
}

// TestStaleRefsBroadcaster_PerSessionFilter — two sessions with
// disjoint working sets receive disjoint notifications from the
// same change event.
func TestStaleRefsBroadcaster_PerSessionFilter(t *testing.T) {
	fake := &fakeSpecificSender{}
	sessions := newSessionMap()

	a := sessions.get("A")
	a.session.recordSymbol("x.go::Foo")

	b := sessions.get("B")
	b.session.recordSymbol("x.go::Bar")

	br := newStaleRefsBroadcaster(fake, sessions, newSessionState(), zap.NewNop())
	br.subscribe("A")
	br.subscribe("B")

	br.handleSymbolChange("/x.go",
		[]*graph.Node{
			{ID: "x.go::Foo", Kind: graph.KindFunction},
			{ID: "x.go::Bar", Kind: graph.KindFunction},
		},
		[]*graph.Node{},
	)
	calls := fake.snapshot()
	require.Len(t, calls, 2)
	bySession := make(map[string][]string)
	for _, c := range calls {
		bySession[c.sessionID] = c.params["removed_symbols"].([]string)
	}
	assert.Equal(t, []string{"x.go::Foo"}, bySession["A"])
	assert.Equal(t, []string{"x.go::Bar"}, bySession["B"])
}

// TestDiffSymbolSets_KindFileImportIgnored — file / import nodes
// are not user-authored symbols and must never appear in the diff.
func TestDiffSymbolSets_KindFileImportIgnored(t *testing.T) {
	removed, changed := diffSymbolSets(
		[]*graph.Node{
			{ID: "x.go", Kind: graph.KindFile},
			{ID: "x.go::import:os", Kind: graph.KindImport},
			{ID: "x.go::Foo", Kind: graph.KindFunction},
		},
		[]*graph.Node{},
	)
	assert.Equal(t, []string{"x.go::Foo"}, removed)
	assert.Empty(t, changed)
}

// TestStaleRefsBroadcaster_Unsubscribe — after unsubscribe a
// session stops receiving notifications.
func TestStaleRefsBroadcaster_Unsubscribe(t *testing.T) {
	fake := &fakeSpecificSender{}
	defaults := stateWithViewed([]string{"x.go::Foo"}, nil, nil)
	b := newStaleRefsBroadcaster(fake, nil, defaults, zap.NewNop())
	b.subscribe("A")
	b.unsubscribe("A")

	b.handleSymbolChange("/x.go",
		[]*graph.Node{{ID: "x.go::Foo", Kind: graph.KindFunction}},
		[]*graph.Node{},
	)
	assert.Empty(t, fake.snapshot())
}

// TestServer_ReleaseSession_UnsubscribesStaleRefs — Server cleanup
// drops stale_refs subscribers.
func TestServer_ReleaseSession_UnsubscribesStaleRefs(t *testing.T) {
	fake := &fakeSpecificSender{}
	srv := &Server{staleRefsBroadcaster: newStaleRefsBroadcaster(fake, nil, newSessionState(), zap.NewNop())}
	srv.staleRefsBroadcaster.subscribe("A")
	require.Equal(t, 1, srv.staleRefsBroadcaster.subscriberCount())

	srv.ReleaseSession("A")
	assert.Equal(t, 0, srv.staleRefsBroadcaster.subscriberCount())
}

// TestRegisterStaleRefsTools_Wiring — tool handlers operate on the
// broadcaster.
func TestRegisterStaleRefsTools_Wiring(t *testing.T) {
	fake := &fakeSpecificSender{}
	srv := &Server{staleRefsBroadcaster: newStaleRefsBroadcaster(fake, nil, newSessionState(), zap.NewNop())}

	req := mcp.CallToolRequest{}
	res, err := srv.handleSubscribeStaleRefs(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, 1, srv.staleRefsBroadcaster.subscriberCount())

	res, err = srv.handleUnsubscribeStaleRefs(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, 0, srv.staleRefsBroadcaster.subscriberCount())
}
