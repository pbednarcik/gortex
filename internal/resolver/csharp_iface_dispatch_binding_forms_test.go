package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// bindMemberCallAtLine mirrors the enrichment/LSP binding for a member
// call whose extraction companion carries NO receiver evidence - the
// exact state a shadow-refused site is in. bindFieldReceiverCall finds
// the companion by its receiver_name stamp, which such a site does not
// have; here the companion is found by member name alone.
func bindMemberCallAtLine(t *testing.T, g graph.Store, callerID, memberName, target string) {
	t.Helper()
	var companion *graph.Edge
	for _, e := range g.GetOutEdges(callerID) {
		if e != nil && e.Kind == graph.EdgeCalls && e.To == "unresolved::*."+memberName {
			companion = e
			break
		}
	}
	require.NotNil(t, companion, "fixture: the extraction must leave an unresolved companion for the member call")
	g.AddEdge(&graph.Edge{
		From: callerID, To: target, Kind: graph.EdgeCalls,
		FilePath: companion.FilePath, Line: companion.Line,
		Origin: graph.OriginASTResolved, Confidence: 0.95,
	})
}

// The shadow indexes were fed by exactly two binding forms - the
// local_declaration_statement capture and emitted KindParam nodes. C#
// has more: foreach variables, declaration patterns (`o is T x`),
// out-var declaration expressions, lambda parameters, and the
// parenthesized `using (var x = ...)` resource. A name bound by any of
// them was invisible to the refusal, so when it coincided with a field
// name the site stamped receiver_name, the field-identifier emitter
// minted the read-edge evidence off the SAME index, and the gate
// filtered on the field's closure - a receiver the call site does not
// have. Both layers of the two-layer guard failed together, because a
// two-layer guard whose layers share one index is one layer.
//
// Every fixture binds `_box` (an IBox<Widget>) over a call site while
// the enclosing type declares `IBox<Crate> _box`. The receiver is the
// bound name, not the field, so no closure is provable and the site
// must keep the full fan-out.
func TestResolveCSharpInterfaceDispatch_BindingFormsShadowFieldReceivers(t *testing.T) {
	for _, tc := range []struct {
		name   string
		caller string
		body   string
	}{
		{"foreach variable", "Flow.Sum", `
        public int Sum(IBox<Widget>[] all) {
            int t = 0;
            foreach (var _box in all) { t += _box.Get(7); }
            return t;
        }`},
		{"declaration pattern", "Flow.Check", `
        public int Check(object o) {
            if (o is IBox<Widget> _box) { return _box.Get(7); }
            return 0;
        }`},
		{"out var", "Flow.Pull", `
        private bool TryMake(out IBox<Widget> made) { made = null; return false; }
        public int Pull() {
            if (TryMake(out var _box)) { return _box.Get(7); }
            return 0;
        }`},
		{"lambda parameter", "Flow.Total", `
        public int Total(IBox<Widget>[] all) {
            return all.Sum(_box => _box.Get(7));
        }`},
		{"parenthesized using", "Flow.Use", `
        private IBox<Widget> Make(IBox<Widget> s) { return s; }
        public int Use(IBox<Widget> src) {
            using (var _box = Make(src)) { return _box.Get(7); }
        }`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := buildCSharpResolverGraph(t, map[string]string{
				"F.cs": `namespace App {
    public class Crate { }
    public class Widget { }
    public interface IBox<T> { int Get(int id); }
    public class CrateBox : IBox<Crate> { public int Get(int id) { return 1; } }
    public class WidgetBox : IBox<Widget> { public int Get(int id) { return 2; } }
    public class Flow {
        private readonly IBox<Crate> _box;
        public Flow(IBox<Crate> b) { _box = b; }
` + tc.body + `
    }
}`,
			})
			New(g).ResolveAll()

			callerID := "F.cs::" + tc.caller
			bindMemberCallAtLine(t, g, callerID, "Get", "F.cs::IBox.Get")
			ResolveCSharpInterfaceDispatch(g)

			assert.ElementsMatch(t, []string{
				"F.cs::CrateBox.Get",
				"F.cs::WidgetBox.Get",
			}, dispatchTargets(g, callerID),
				"the receiver is the bound name, not the field - no closure is provable, so the full fan-out stays")
		})
	}
}
