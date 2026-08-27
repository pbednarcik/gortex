package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// Generic type arguments gate the interface-dispatch fan-out: a receiver
// declared IBoxStore<Crate> can never dispatch into the class implementing
// IBoxStore<Widget>, so the fan-out must not fabricate that usage. The
// filter is evidence-gated on BOTH sides — the receiver's declared field
// type and the implementor's stamped base-list arguments — and absence of
// either keeps today's full fan-out (precision only ever improves).
func TestResolveCSharpInterfaceDispatch_GenericTypeArgsGateFanout(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Stores.cs": `namespace App {
    public class Widget { }
    public class Crate { }
    public interface IBoxStore<T> {
        T Fetch(int id);
    }
    public class WidgetBoxStore : IBoxStore<Widget> {
        public Widget Fetch(int id) { return new Widget(); }
    }
    public class CrateBoxStore : IBoxStore<Crate> {
        public Crate Fetch(int id) { return new Crate(); }
    }
}`,
		"CrateFlow.cs": `namespace App {
    public class CrateFlow {
        private readonly IBoxStore<Crate> _store;
        public CrateFlow(IBoxStore<Crate> store) { _store = store; }
        public Crate Pull(int id) {
            return _store.Fetch(id);
        }
    }
}`,
	})
	New(g).ResolveAll()

	callerID := "CrateFlow.cs::CrateFlow.Pull"
	bindFieldReceiverCall(t, g, callerID, "_store", "Stores.cs::IBoxStore.Fetch")

	ResolveCSharpInterfaceDispatch(g)

	var targets []string
	for _, e := range g.GetOutEdges(callerID) {
		if isIfaceDispatchEdge(e) {
			targets = append(targets, e.To)
		}
	}
	assert.Contains(t, targets, "Stores.cs::CrateBoxStore.Fetch",
		"the type-compatible implementation still receives the fan-out")
	assert.NotContains(t, targets, "Stores.cs::WidgetBoxStore.Fetch",
		"an IBoxStore<Crate> receiver can never dispatch to the IBoxStore<Widget> impl")
}

// bindFieldReceiverCall mirrors what the enrichment/LSP tiers do on a live
// store for a FIELD-receiver member call the core resolver leaves
// unresolved: a resolved call edge lands at the same site, while the
// extraction's own unresolved companion edge (carrying receiver_name)
// stays alongside it. The dispatch pass reads the receiver evidence from
// that companion - exactly the join a production store requires.
func bindFieldReceiverCall(t *testing.T, g graph.Store, callerID, receiver, target string) {
	t.Helper()
	var companion *graph.Edge
	for _, e := range g.GetOutEdges(callerID) {
		if e != nil && e.Kind == graph.EdgeCalls && graph.IsUnresolvedTarget(e.To) &&
			e.Meta != nil && e.Meta["receiver_name"] == receiver {
			companion = e
			break
		}
	}
	require.NotNil(t, companion, "fixture: the extraction must leave a receiver_name companion edge")
	g.AddEdge(&graph.Edge{
		From: callerID, To: target, Kind: graph.EdgeCalls,
		FilePath: companion.FilePath, Line: companion.Line,
		Origin: graph.OriginASTResolved, Confidence: 0.95,
	})
}

// The sibling mechanism is gated the same way: a call bound directly to the
// Widget implementation's own method must not fan into the Crate
// implementation — they implement different constructed interfaces — while
// the erased interface member itself (argument-less evidence) still
// receives the site.
func TestResolveCSharpInterfaceDispatch_SiblingFanoutRespectsTypeArgs(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Stores.cs": `namespace App {
    public class Widget { }
    public class Crate { }
    public interface IBoxStore<T> {
        T Fetch(int id);
    }
    public class WidgetBoxStore : IBoxStore<Widget> {
        public Widget Fetch(int id) { return new Widget(); }
    }
    public class CrateBoxStore : IBoxStore<Crate> {
        public Crate Fetch(int id) { return new Crate(); }
    }
    public class WidgetUser {
        public Widget Load(int id) {
            WidgetBoxStore store = new WidgetBoxStore();
            return store.Fetch(id);
        }
    }
}`,
	})
	New(g).ResolveAll()

	callerID := "Stores.cs::WidgetUser.Load"
	require.Contains(t, callTargetsFrom(g, callerID), "Stores.cs::WidgetBoxStore.Fetch",
		"fixture: the typed-local call must bind to the Widget implementation")

	ResolveCSharpInterfaceDispatch(g)

	var targets []string
	for _, e := range g.GetOutEdges(callerID) {
		if isIfaceDispatchEdge(e) {
			targets = append(targets, e.To)
		}
	}
	assert.Contains(t, targets, "Stores.cs::IBoxStore.Fetch",
		"the erased interface member still receives the sibling site")
	assert.NotContains(t, targets, "Stores.cs::CrateBoxStore.Fetch",
		"a Widget-impl site never fans into the Crate impl - different constructed interfaces")
}

// An open-generic implementor (Relay<T> : IBoxStore<T>) carries no stamp and
// can bind ANY argument - it must stay in every fan-out.
func TestResolveCSharpInterfaceDispatch_OpenGenericImplStaysInFanout(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Stores.cs": `namespace App {
    public class Widget { }
    public class Crate { }
    public interface IBoxStore<T> {
        T Fetch(int id);
    }
    public class Relay<T> : IBoxStore<T> {
        public T Fetch(int id) { return default(T); }
    }
    public class CrateBoxStore : IBoxStore<Crate> {
        public Crate Fetch(int id) { return new Crate(); }
    }
}`,
		"CrateFlow.cs": `namespace App {
    public class CrateFlow {
        private readonly IBoxStore<Crate> _store;
        public CrateFlow(IBoxStore<Crate> store) { _store = store; }
        public Crate Pull(int id) {
            return _store.Fetch(id);
        }
    }
}`,
	})
	New(g).ResolveAll()

	callerID := "CrateFlow.cs::CrateFlow.Pull"
	bindFieldReceiverCall(t, g, callerID, "_store", "Stores.cs::IBoxStore.Fetch")

	ResolveCSharpInterfaceDispatch(g)

	var targets []string
	for _, e := range g.GetOutEdges(callerID) {
		if isIfaceDispatchEdge(e) {
			targets = append(targets, e.To)
		}
	}
	assert.Contains(t, targets, "Stores.cs::Relay.Fetch",
		"an open-generic implementor can bind any argument and must stay in the fan-out")
	assert.Contains(t, targets, "Stores.cs::CrateBoxStore.Fetch")
}

// outEdgeCountingStore counts GetOutEdges reads per node ID. Plain
// interface embedding deliberately hides the optional projection
// capabilities the same way csharpProjectionlessStore does.
type outEdgeCountingStore struct {
	graph.Store
	outEdgeReads map[string]int
}

func (s *outEdgeCountingStore) GetOutEdges(id string) []*graph.Edge {
	s.outEdgeReads[id]++
	return s.Store.GetOutEdges(id)
}

// Review RED (revision P2): every receiver lookup re-read the caller's full
// out-edge adjacency - one scan per call site per evidence pass, so a method
// with N through-interface sites paid ~2N GetOutEdges reads. The lookup must
// read a caller's adjacency once per pass and serve every site from it.
func TestResolveCSharpInterfaceDispatch_ReceiverLookupReadsCallerAdjacencyOnce(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Multi.cs": `namespace App {
    public class Crate { }
    public class Widget { }
    public interface IBox<T> {
        int Get(int id);
    }
    public class CrateBox : IBox<Crate> {
        public int Get(int id) { return 1; }
    }
    public class WidgetBox : IBox<Widget> {
        public int Get(int id) { return 2; }
    }
    public class Flow {
        private readonly IBox<Crate> _a;
        private readonly IBox<Crate> _b;
        private readonly IBox<Crate> _c;
        public Flow(IBox<Crate> a, IBox<Crate> b, IBox<Crate> c) { _a = a; _b = b; _c = c; }
        public int Pull() {
            int x = _a.Get(1);
            int y = _b.Get(2);
            int z = _c.Get(3);
            return x + y + z;
        }
    }
}`,
	})
	New(g).ResolveAll()

	callerID := "Multi.cs::Flow.Pull"
	bindFieldReceiverCall(t, g, callerID, "_a", "Multi.cs::IBox.Get")
	bindFieldReceiverCall(t, g, callerID, "_b", "Multi.cs::IBox.Get")
	bindFieldReceiverCall(t, g, callerID, "_c", "Multi.cs::IBox.Get")

	counting := &outEdgeCountingStore{Store: g, outEdgeReads: map[string]int{}}
	ResolveCSharpInterfaceDispatch(counting)

	if n := counting.outEdgeReads[callerID]; n > 1 {
		t.Fatalf("receiver lookups read the caller's out edges %d times, want at most 1", n)
	}
	targets := dispatchTargets(g, callerID)
	assert.Contains(t, targets, "Multi.cs::CrateBox.Get",
		"the cached adjacency still yields the receiver evidence")
	assert.NotContains(t, targets, "Multi.cs::WidgetBox.Get",
		"the gate still filters on the cached evidence")
}

// Sweep pin: a PROPERTY receiver rides the same evidence path as a field -
// properties mint KindField nodes, the field-identifier emitter covers
// their bare-identifier reads, and the declared-type stamp carries the
// closed arguments - so the gate must filter for property receivers
// exactly as it does for fields.
func TestResolveCSharpInterfaceDispatch_PropertyReceiverGatesFanout(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Props.cs": `namespace App {
    public class Crate { }
    public class Widget { }
    public interface IBox<T> {
        int Get(int id);
    }
    public class CrateBox : IBox<Crate> {
        public int Get(int id) { return 1; }
    }
    public class WidgetBox : IBox<Widget> {
        public int Get(int id) { return 2; }
    }
    public class Flow {
        private IBox<Crate> Store { get; set; }
        public int Pull() { return Store.Get(1); }
    }
}`,
	})
	New(g).ResolveAll()

	callerID := "Props.cs::Flow.Pull"
	bindFieldReceiverCall(t, g, callerID, "Store", "Props.cs::IBox.Get")

	ResolveCSharpInterfaceDispatch(g)

	targets := dispatchTargets(g, callerID)
	assert.Contains(t, targets, "Props.cs::CrateBox.Get",
		"a property receiver's declared arguments gate exactly like a field's")
	assert.NotContains(t, targets, "Props.cs::WidgetBox.Get",
		"an IBox<Crate> property receiver never dispatches to the Widget impl")
}

// Sweep pin: an implementor reached TRANSITIVELY (class D : IDerived where
// IDerived : IBox<Crate>) carries no stamp against the root interface - its
// base-list evidence names IDerived, not IBox - so it must stay in every
// fan-out, even one whose receiver closes over different arguments. The
// documented conservative rule, pinned so stamp inheritance can never
// silently flip it into filtering.
func TestResolveCSharpInterfaceDispatch_TransitiveImplementorStaysInFanout(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Derived.cs": `namespace App {
    public class Crate { }
    public class Widget { }
    public interface IBox<T> {
        int Get(int id);
    }
    public interface IDerived : IBox<Crate> {
    }
    public class DerivedBox : IDerived {
        public int Get(int id) { return 1; }
    }
    public class WidgetBox : IBox<Widget> {
        public int Get(int id) { return 2; }
    }
    public class Flow {
        private readonly IBox<Widget> _widgets;
        public Flow(IBox<Widget> w) { _widgets = w; }
        public int Pull() { return _widgets.Get(1); }
    }
}`,
	})
	New(g).ResolveAll()

	callerID := "Derived.cs::Flow.Pull"
	bindFieldReceiverCall(t, g, callerID, "_widgets", "Derived.cs::IBox.Get")

	ResolveCSharpInterfaceDispatch(g)

	targets := dispatchTargets(g, callerID)
	assert.Contains(t, targets, "Derived.cs::WidgetBox.Get",
		"the receiver's own closure keeps its matching impl")
	assert.Contains(t, targets, "Derived.cs::DerivedBox.Get",
		"a transitive implementor is unstamped and never filtered")
}

// Sweep pin: a nullable-annotated receiver (`IBox<Crate?>`) is a non-simple
// spelling and stamps nothing - the site keeps the full fan-out. Pinned
// against a future fold treating Crate? as Crate: right for reference
// types, wrong for value types (int? is Nullable<int>), so the refusal is
// the correct conservative rule.
func TestResolveCSharpInterfaceDispatch_NullableReceiverSpellingNeverFilters(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Nullable.cs": `namespace App {
    public class Crate { }
    public class Widget { }
    public interface IBox<T> {
        int Get(int id);
    }
    public class CrateBox : IBox<Crate> {
        public int Get(int id) { return 1; }
    }
    public class WidgetBox : IBox<Widget> {
        public int Get(int id) { return 2; }
    }
    public class Flow {
        private readonly IBox<Crate?> _maybe;
        public Flow(IBox<Crate?> m) { _maybe = m; }
        public int Pull() { return _maybe.Get(1); }
    }
}`,
	})
	New(g).ResolveAll()

	callerID := "Nullable.cs::Flow.Pull"
	bindFieldReceiverCall(t, g, callerID, "_maybe", "Nullable.cs::IBox.Get")

	ResolveCSharpInterfaceDispatch(g)

	targets := dispatchTargets(g, callerID)
	assert.Contains(t, targets, "Nullable.cs::CrateBox.Get",
		"a non-simple spelling stamps nothing and filters nothing")
	assert.Contains(t, targets, "Nullable.cs::WidgetBox.Get",
		"a non-simple spelling stamps nothing and filters nothing")
}

// dispatchTargets returns the fan-out targets minted from callerID.
func dispatchTargets(g graph.Store, callerID string) []string {
	var targets []string
	for _, e := range g.GetOutEdges(callerID) {
		if isIfaceDispatchEdge(e) {
			targets = append(targets, e.To)
		}
	}
	return targets
}

// Review RED 1: a receiver field typed with an ENCLOSING generic type's
// parameter (Outer<T> { class Flow { IBoxStore<T> _store; } }) closes
// nothing - the site must keep the FULL fan-out, and the nested
// implementor's own bogus stamp must not survive either.
func TestResolveCSharpInterfaceDispatch_EnclosingTypeParamNeverFilters(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Stores.cs": `namespace App {
    public class Widget { }
    public class Crate { }
    public interface IBoxStore<T> {
        T Fetch(int id);
    }
    public class WidgetBoxStore : IBoxStore<Widget> {
        public Widget Fetch(int id) { return new Widget(); }
    }
    public class CrateBoxStore : IBoxStore<Crate> {
        public Crate Fetch(int id) { return new Crate(); }
    }
}`,
		"Nested.cs": `namespace App {
    public class Outer<T> {
        public class Flow {
            private readonly IBoxStore<T> _store;
            public Flow(IBoxStore<T> store) { _store = store; }
            public T Pull(int id) {
                return _store.Fetch(id);
            }
        }
    }
}`,
	})
	New(g).ResolveAll()

	callerID := "Nested.cs::Flow.Pull"
	bindFieldReceiverCall(t, g, callerID, "_store", "Stores.cs::IBoxStore.Fetch")

	ResolveCSharpInterfaceDispatch(g)

	targets := dispatchTargets(g, callerID)
	assert.Contains(t, targets, "Stores.cs::CrateBoxStore.Fetch",
		"an open receiver argument filters nothing")
	assert.Contains(t, targets, "Stores.cs::WidgetBoxStore.Fetch",
		"an open receiver argument filters nothing")
}

// Review RED 3: two same-named member calls on one line share a single
// unresolved companion edge - the receiver evidence is ambiguous, so the
// site must keep the FULL fan-out rather than applying one receiver's
// arguments to both calls.
func TestResolveCSharpInterfaceDispatch_AmbiguousSameLineReceiverNeverFilters(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Stores.cs": `namespace App {
    public class Widget { }
    public class Crate { }
    public interface IBoxStore<T> {
        int Fetch(int id);
    }
    public class WidgetBoxStore : IBoxStore<Widget> {
        public int Fetch(int id) { return 1; }
    }
    public class CrateBoxStore : IBoxStore<Crate> {
        public int Fetch(int id) { return 2; }
    }
    public class Flow {
        private readonly IBoxStore<Crate> _crates;
        private readonly IBoxStore<Widget> _widgets;
        public Flow(IBoxStore<Crate> c, IBoxStore<Widget> w) { _crates = c; _widgets = w; }
        public int Pull() { return _crates.Fetch(_widgets.Fetch(1)); }
    }
}`,
	})
	New(g).ResolveAll()

	callerID := "Stores.cs::Flow.Pull"
	// One bound edge stands in for both same-line calls (same from/to/line).
	var companion *graph.Edge
	for _, e := range g.GetOutEdges(callerID) {
		if e != nil && e.Kind == graph.EdgeCalls && graph.IsUnresolvedTarget(e.To) {
			companion = e
			break
		}
	}
	require.NotNil(t, companion)
	g.AddEdge(&graph.Edge{
		From: callerID, To: "Stores.cs::IBoxStore.Fetch", Kind: graph.EdgeCalls,
		FilePath: companion.FilePath, Line: companion.Line,
		Origin: graph.OriginASTResolved, Confidence: 0.95,
	})

	ResolveCSharpInterfaceDispatch(g)

	targets := dispatchTargets(g, callerID)
	assert.Contains(t, targets, "Stores.cs::CrateBoxStore.Fetch",
		"ambiguous receiver evidence filters nothing")
	assert.Contains(t, targets, "Stores.cs::WidgetBoxStore.Fetch",
		"ambiguous receiver evidence filters nothing")
}

// Multi-argument closures compare as one normalized list end-to-end - the
// string-equality contract between extractor stamp and receiver stamp is
// exactly what would regress silently without a pin.
func TestResolveCSharpInterfaceDispatch_MultiArgClosureGatesFanout(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Pairs.cs": `namespace App {
    public class Widget { }
    public class Crate { }
    public interface IPair<K, V> {
        int Fetch(int id);
    }
    public class WidgetPair : IPair<int, Widget> {
        public int Fetch(int id) { return 1; }
    }
    public class CratePair : IPair<int, Crate> {
        public int Fetch(int id) { return 2; }
    }
    public class Flow {
        private readonly IPair<int, Crate> _pairs;
        public Flow(IPair<int, Crate> p) { _pairs = p; }
        public int Pull() { return _pairs.Fetch(1); }
    }
}`,
	})
	New(g).ResolveAll()

	callerID := "Pairs.cs::Flow.Pull"
	bindFieldReceiverCall(t, g, callerID, "_pairs", "Pairs.cs::IPair.Fetch")

	ResolveCSharpInterfaceDispatch(g)

	targets := dispatchTargets(g, callerID)
	assert.Contains(t, targets, "Pairs.cs::CratePair.Fetch")
	assert.NotContains(t, targets, "Pairs.cs::WidgetPair.Fetch",
		"an IPair<int,Crate> receiver never dispatches to the IPair<int,Widget> impl")
}

// Review RED (revision 1): two DIFFERENT member calls on one line, each on
// its own field of the same generic interface closed with different
// arguments — `_crates.Fetch(1) + _widgets.Save(2)`. The receiver lookup is
// cached per call site, and a cache key without the member identity lets the
// first receiver's arguments poison the second call's gate: WidgetBoxStore.Save
// silently loses its usage. Each member call must gate on ITS OWN receiver.
func TestResolveCSharpInterfaceDispatch_SameLineDistinctMembersKeepOwnReceivers(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Stores.cs": `namespace App {
    public class Widget { }
    public class Crate { }
    public interface IBoxStore<T> {
        int Fetch(int id);
        int Save(int id);
    }
    public class WidgetBoxStore : IBoxStore<Widget> {
        public int Fetch(int id) { return 1; }
        public int Save(int id) { return 1; }
    }
    public class CrateBoxStore : IBoxStore<Crate> {
        public int Fetch(int id) { return 2; }
        public int Save(int id) { return 2; }
    }
    public class Flow {
        private readonly IBoxStore<Crate> _crates;
        private readonly IBoxStore<Widget> _widgets;
        public Flow(IBoxStore<Crate> c, IBoxStore<Widget> w) { _crates = c; _widgets = w; }
        public int Pull() { return _crates.Fetch(1) + _widgets.Save(2); }
    }
}`,
	})
	New(g).ResolveAll()

	callerID := "Stores.cs::Flow.Pull"
	bindFieldReceiverCall(t, g, callerID, "_crates", "Stores.cs::IBoxStore.Fetch")
	bindFieldReceiverCall(t, g, callerID, "_widgets", "Stores.cs::IBoxStore.Save")

	ResolveCSharpInterfaceDispatch(g)

	targets := dispatchTargets(g, callerID)
	assert.Contains(t, targets, "Stores.cs::CrateBoxStore.Fetch",
		"the Fetch call gates on _crates and keeps the Crate impl")
	assert.NotContains(t, targets, "Stores.cs::WidgetBoxStore.Fetch",
		"the Fetch call's receiver is IBoxStore<Crate>")
	assert.Contains(t, targets, "Stores.cs::WidgetBoxStore.Save",
		"the Save call gates on _widgets - the Fetch lookup must not poison it")
	assert.NotContains(t, targets, "Stores.cs::CrateBoxStore.Save",
		"the Save call's receiver is IBoxStore<Widget>")
}

// Review RED (revision 2a): a method parameter shadows the receiver field —
// `IBox<Crate> _box` field, `IBox<Widget> _box` parameter. The receiver
// identifier binds to the PARAMETER, but a bare text lookup finds the field
// and its Crate arguments suppress WidgetBox.Get. Without binding evidence
// the receiver is unknown and the site must keep the full fan-out.
func TestResolveCSharpInterfaceDispatch_ParameterShadowedReceiverNeverFilters(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Boxes.cs": `namespace App {
    public class Widget { }
    public class Crate { }
    public interface IBox<T> {
        int Get(int id);
    }
    public class WidgetBox : IBox<Widget> {
        public int Get(int id) { return 1; }
    }
    public class CrateBox : IBox<Crate> {
        public int Get(int id) { return 2; }
    }
    public class Flow {
        private readonly IBox<Crate> _box;
        public Flow(IBox<Crate> b) { _box = b; }
        public int Pull(IBox<Widget> _box) {
            return _box.Get(7);
        }
    }
}`,
	})
	New(g).ResolveAll()

	callerID := "Boxes.cs::Flow.Pull"
	// The extractor now refuses receiver_name for a param-shadowed bare
	// receiver, so the bind keys on the member companion alone.
	bindMemberCall(t, g, callerID, "Get", "Boxes.cs::IBox.Get")

	ResolveCSharpInterfaceDispatch(g)

	targets := dispatchTargets(g, callerID)
	assert.Contains(t, targets, "Boxes.cs::WidgetBox.Get",
		"the receiver is the shadowing IBox<Widget> parameter - its impl must stay in")
	assert.Contains(t, targets, "Boxes.cs::CrateBox.Get",
		"an unknown receiver filters nothing")
}

// Review RED (revision 2b): a `var` local shadows the receiver field the
// same way - the identifier binds to the local, not the field the text
// lookup finds. (An interface-TYPED local never reaches the receiver
// lookup: the tenv binds its call directly and leaves no companion.)
func TestResolveCSharpInterfaceDispatch_LocalShadowedReceiverNeverFilters(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Boxes.cs": `namespace App {
    public class Widget { }
    public class Crate { }
    public interface IBox<T> {
        int Get(int id);
    }
    public class WidgetBox : IBox<Widget> {
        public int Get(int id) { return 1; }
    }
    public class CrateBox : IBox<Crate> {
        public int Get(int id) { return 2; }
    }
    public class Flow {
        private readonly IBox<Crate> _box;
        public Flow(IBox<Crate> b) { _box = b; }
        public int Pull(IBox<Widget> source) {
            var _box = source;
            return _box.Get(7);
        }
    }
}`,
	})
	New(g).ResolveAll()

	callerID := "Boxes.cs::Flow.Pull"
	// The extractor now refuses receiver_name for a local-shadowed bare
	// receiver, so the bind keys on the member companion alone.
	bindMemberCall(t, g, callerID, "Get", "Boxes.cs::IBox.Get")

	ResolveCSharpInterfaceDispatch(g)

	targets := dispatchTargets(g, callerID)
	assert.Contains(t, targets, "Boxes.cs::WidgetBox.Get",
		"the receiver is the shadowing IBox<Widget> local - its impl must stay in")
	assert.Contains(t, targets, "Boxes.cs::CrateBox.Get",
		"an unknown receiver filters nothing")
}

// Review RED (revision 3a): a covariant interface (`ISource<out T>`) makes
// ISource<Dog> assignable to an ISource<Animal> receiver, so DogSource.Get
// is a real dispatch target at the site - the closed-and-unequal equality
// gate only models INVARIANT parameters and must stand down entirely when
// the interface declares any variance.
func TestResolveCSharpInterfaceDispatch_CovariantInterfaceNeverFilters(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Sources.cs": `namespace App {
    public class Animal { }
    public class Dog : Animal { }
    public class Cat : Animal { }
    public interface ISource<out T> {
        T Get();
    }
    public class DogSource : ISource<Dog> {
        public Dog Get() { return new Dog(); }
    }
    public class CatSource : ISource<Cat> {
        public Cat Get() { return new Cat(); }
    }
    public class Flow {
        private readonly ISource<Animal> _source;
        public Flow(ISource<Animal> s) { _source = s; }
        public Animal Pull() {
            return _source.Get();
        }
    }
}`,
	})
	New(g).ResolveAll()

	callerID := "Sources.cs::Flow.Pull"
	bindFieldReceiverCall(t, g, callerID, "_source", "Sources.cs::ISource.Get")

	ResolveCSharpInterfaceDispatch(g)

	targets := dispatchTargets(g, callerID)
	assert.Contains(t, targets, "Sources.cs::DogSource.Get",
		"out T: ISource<Dog> satisfies an ISource<Animal> receiver - the impl must stay in")
	assert.Contains(t, targets, "Sources.cs::CatSource.Get",
		"out T: ISource<Cat> satisfies an ISource<Animal> receiver - the impl must stay in")
}

// Review RED (revision 3b): the contravariant twin (`ISink<in T>`) -
// ISink<Animal> is assignable to an ISink<Dog> receiver.
func TestResolveCSharpInterfaceDispatch_ContravariantInterfaceNeverFilters(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Sinks.cs": `namespace App {
    public class Animal { }
    public class Dog : Animal { }
    public interface ISink<in T> {
        int Put(int item);
    }
    public class AnimalSink : ISink<Animal> {
        public int Put(int item) { return 1; }
    }
    public class DogSink : ISink<Dog> {
        public int Put(int item) { return 2; }
    }
    public class Flow {
        private readonly ISink<Dog> _sink;
        public Flow(ISink<Dog> s) { _sink = s; }
        public int Push() {
            return _sink.Put(3);
        }
    }
}`,
	})
	New(g).ResolveAll()

	callerID := "Sinks.cs::Flow.Push"
	bindFieldReceiverCall(t, g, callerID, "_sink", "Sinks.cs::ISink.Put")

	ResolveCSharpInterfaceDispatch(g)

	targets := dispatchTargets(g, callerID)
	assert.Contains(t, targets, "Sinks.cs::AnimalSink.Put",
		"in T: ISink<Animal> satisfies an ISink<Dog> receiver - the impl must stay in")
	assert.Contains(t, targets, "Sinks.cs::DogSink.Put")
}

// Review RED (revision 4a): `IBox<dynamic>` and `IBox<object>` construct
// over the same underlying type (dynamic erases to object) - the gate must
// fold them together and retain the edge. Re-review fix: the original
// fixture implemented IBox<dynamic> directly, which the compiler refuses
// (CS1966 - a class cannot implement a dynamic interface); the legal
// orientation exercises the same fold from the receiver side.
func TestResolveCSharpInterfaceDispatch_DynamicObjectSpellingsRetainTheEdge(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Dyn.cs": `namespace App {
    public class Crate { }
    public interface IBox<T> {
        int Get(int id);
    }
    public class ObjectBox : IBox<object> {
        public int Get(int id) { return 1; }
    }
    public class CrateBox : IBox<Crate> {
        public int Get(int id) { return 2; }
    }
    public class Flow {
        private readonly IBox<dynamic> _objects;
        public Flow(IBox<dynamic> o) { _objects = o; }
        public int Pull() { return _objects.Get(1); }
    }
}`,
	})
	New(g).ResolveAll()

	callerID := "Dyn.cs::Flow.Pull"
	bindFieldReceiverCall(t, g, callerID, "_objects", "Dyn.cs::IBox.Get")

	ResolveCSharpInterfaceDispatch(g)

	targets := dispatchTargets(g, callerID)
	assert.Contains(t, targets, "Dyn.cs::ObjectBox.Get",
		"dynamic and object spell the same constructed interface - the edge stays")
	assert.NotContains(t, targets, "Dyn.cs::CrateBox.Get",
		"the genuinely different closure still filters")
}

// Review RED (revision 4b): `nint` IS System.IntPtr (and nuint UIntPtr) -
// the native-int keyword and the struct name spell one type.
func TestResolveCSharpInterfaceDispatch_NativeIntSpellingsRetainTheEdge(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Native.cs": `namespace App {
    public class Crate { }
    public interface IBox<T> {
        int Get(int id);
    }
    public class PtrBox : IBox<System.IntPtr> {
        public int Get(int id) { return 1; }
    }
    public class CrateBox : IBox<Crate> {
        public int Get(int id) { return 2; }
    }
    public class Flow {
        private readonly IBox<nint> _ptrs;
        public Flow(IBox<nint> p) { _ptrs = p; }
        public int Pull() { return _ptrs.Get(1); }
    }
}`,
	})
	New(g).ResolveAll()

	callerID := "Native.cs::Flow.Pull"
	bindFieldReceiverCall(t, g, callerID, "_ptrs", "Native.cs::IBox.Get")

	ResolveCSharpInterfaceDispatch(g)

	targets := dispatchTargets(g, callerID)
	assert.Contains(t, targets, "Native.cs::PtrBox.Get",
		"nint and System.IntPtr spell the same constructed interface - the edge stays")
	assert.NotContains(t, targets, "Native.cs::CrateBox.Get")
}

// Review RED (revision 4c): a verbatim identifier (`@Crate`) and a
// global-qualified spelling (`global::App.Crate`) both name plain Crate.
func TestResolveCSharpInterfaceDispatch_VerbatimAndGlobalSpellingsRetainTheEdge(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Spell.cs": `namespace App {
    public class Crate { }
    public class Widget { }
    public interface IBox<T> {
        int Get(int id);
    }
    public class VerbatimBox : IBox<@Crate> {
        public int Get(int id) { return 1; }
    }
    public class WidgetBox : IBox<Widget> {
        public int Get(int id) { return 2; }
    }
    public class Flow {
        private readonly IBox<global::App.Crate> _crates;
        public Flow(IBox<global::App.Crate> c) { _crates = c; }
        public int Pull() { return _crates.Get(1); }
    }
}`,
	})
	New(g).ResolveAll()

	callerID := "Spell.cs::Flow.Pull"
	bindFieldReceiverCall(t, g, callerID, "_crates", "Spell.cs::IBox.Get")

	ResolveCSharpInterfaceDispatch(g)

	targets := dispatchTargets(g, callerID)
	assert.Contains(t, targets, "Spell.cs::VerbatimBox.Get",
		"@Crate and global::App.Crate both name Crate - the edge stays")
	assert.NotContains(t, targets, "Spell.cs::WidgetBox.Get",
		"the genuinely different closure still filters")
}

// Review RED (revision 4d): `class Relay<T> : IBox<@T>` closes NOTHING -
// the escaped spelling still names the open parameter T, and reading it as
// a closed type called "@T" would filter the open implementor out of every
// differently-closed receiver's fan-out.
func TestResolveCSharpInterfaceDispatch_EscapedOpenParamStaysInFanout(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Relay.cs": `namespace App {
    public class Crate { }
    public interface IBox<T> {
        int Get(int id);
    }
    public class Relay<T> : IBox<@T> {
        public int Get(int id) { return 1; }
    }
    public class CrateBox : IBox<Crate> {
        public int Get(int id) { return 2; }
    }
    public class Flow {
        private readonly IBox<Crate> _crates;
        public Flow(IBox<Crate> c) { _crates = c; }
        public int Pull() { return _crates.Get(1); }
    }
}`,
	})
	New(g).ResolveAll()

	callerID := "Relay.cs::Flow.Pull"
	bindFieldReceiverCall(t, g, callerID, "_crates", "Relay.cs::IBox.Get")

	ResolveCSharpInterfaceDispatch(g)

	targets := dispatchTargets(g, callerID)
	assert.Contains(t, targets, "Relay.cs::Relay.Get",
		"an open-generic implementor spelled with a verbatim parameter stays in the fan-out")
	assert.Contains(t, targets, "Relay.cs::CrateBox.Get")
}

// Review RED (revision 5a): a PROJECT-WIDE alias (`global using Entity =
// App.Crate;` in another file) makes the spelling "Entity" opaque in every
// file - the receiver's IBox<Entity> and the implementor's IBox<Crate> are
// the same constructed interface, but the ancestor-only alias scan cannot
// see the cross-file directive and the stamps compare unequal. Any stamp
// naming a project-global alias must be refused (never filter).
func TestResolveCSharpInterfaceDispatch_GlobalUsingAliasReceiverNeverFilters(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Global.cs": `global using Entity = App.Crate;
`,
		"Stores.cs": `namespace App {
    public class Crate { }
    public class Widget { }
    public interface IBox<T> {
        int Get(int id);
    }
    public class CrateBox : IBox<Crate> {
        public int Get(int id) { return 1; }
    }
    public class WidgetBox : IBox<Widget> {
        public int Get(int id) { return 2; }
    }
}`,
		"Flow.cs": `namespace App {
    public class Flow {
        private readonly IBox<Entity> _crates;
        public Flow(IBox<Entity> c) { _crates = c; }
        public int Pull() { return _crates.Get(1); }
    }
}`,
	})
	New(g).ResolveAll()

	callerID := "Flow.cs::Flow.Pull"
	bindFieldReceiverCall(t, g, callerID, "_crates", "Stores.cs::IBox.Get")

	ResolveCSharpInterfaceDispatch(g)

	targets := dispatchTargets(g, callerID)
	assert.Contains(t, targets, "Stores.cs::CrateBox.Get",
		"Entity IS Crate through the project-global alias - the impl must stay in")
	assert.Contains(t, targets, "Stores.cs::WidgetBox.Get",
		"an alias-named receiver stamp is opaque and filters nothing")
}

// Review RED (revision 5b): the implementor-side twin - a base list spelled
// through the project-global alias (`CrateBox : IBox<Entity>`) must not
// stamp a closed argument the gate would compare against literal spellings.
func TestResolveCSharpInterfaceDispatch_GlobalUsingAliasImplementorStaysInFanout(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Global.cs": `global using Entity = App.Crate;
`,
		"Stores.cs": `namespace App {
    public class Crate { }
    public interface IBox<T> {
        int Get(int id);
    }
    public class CrateBox : IBox<Entity> {
        public int Get(int id) { return 1; }
    }
}`,
		"Flow.cs": `namespace App {
    public class Flow {
        private readonly IBox<Crate> _crates;
        public Flow(IBox<Crate> c) { _crates = c; }
        public int Pull() { return _crates.Get(1); }
    }
}`,
	})
	New(g).ResolveAll()

	callerID := "Flow.cs::Flow.Pull"
	bindFieldReceiverCall(t, g, callerID, "_crates", "Stores.cs::IBox.Get")

	ResolveCSharpInterfaceDispatch(g)

	targets := dispatchTargets(g, callerID)
	assert.Contains(t, targets, "Stores.cs::CrateBox.Get",
		"the alias-spelled base list stamps nothing - the impl stays in the fan-out")
}

// Codex review RED: `IBoxStore<System.Int32>` and `IBoxStore<int>` are the
// SAME constructed interface in different spellings — the gate must fold
// both to one canonical form and RETAIN the edge, never suppress it on a
// spelling mismatch.
func TestResolveCSharpInterfaceDispatch_BCLAliasSpellingsRetainTheEdge(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Ints.cs": `namespace App {
    public class Crate { }
    public interface IBoxStore<T> {
        int Fetch(int id);
    }
    public class IntBox : IBoxStore<System.Int32> {
        public int Fetch(int id) { return 1; }
    }
    public class CrateBox : IBoxStore<Crate> {
        public int Fetch(int id) { return 2; }
    }
    public class Flow {
        private readonly IBoxStore<int> _ints;
        public Flow(IBoxStore<int> i) { _ints = i; }
        public int Pull() { return _ints.Fetch(1); }
    }
}`,
	})
	New(g).ResolveAll()

	callerID := "Ints.cs::Flow.Pull"
	bindFieldReceiverCall(t, g, callerID, "_ints", "Ints.cs::IBoxStore.Fetch")

	ResolveCSharpInterfaceDispatch(g)

	targets := dispatchTargets(g, callerID)
	assert.Contains(t, targets, "Ints.cs::IntBox.Fetch",
		"System.Int32 and int spell the same constructed interface - the edge stays")
	assert.NotContains(t, targets, "Ints.cs::CrateBox.Fetch",
		"the genuinely different closure still filters")
}

// bindMemberCall binds the unresolved companion for the named member to the
// interface member regardless of receiver evidence - the enrichment/LSP
// tiers key on the site, not on extraction's receiver stamps, so a call
// whose receiver extraction could not certify still arrives bound.
func bindMemberCall(t *testing.T, g graph.Store, callerID, member, target string) {
	t.Helper()
	var companion *graph.Edge
	for _, e := range g.GetOutEdges(callerID) {
		if e != nil && e.Kind == graph.EdgeCalls && e.To == "unresolved::*."+member {
			companion = e
			break
		}
	}
	require.NotNil(t, companion, "fixture: extraction must leave the member companion edge")
	g.AddEdge(&graph.Edge{
		From: callerID, To: target, Kind: graph.EdgeCalls,
		FilePath: companion.FilePath, Line: companion.Line,
		Origin: graph.OriginASTResolved, Confidence: 0.95,
	})
}

// Re-review RED: comment trivia in the receiver's declared type is legal
// C# and no part of type identity - `IBox</**/Crate>` must dispatch
// exactly like `IBox<Crate>`, not carry "/**/Crate" into the compare and
// lose the valid implementor.
func TestResolveCSharpInterfaceDispatch_CommentTriviaInReceiverTypeRetainsTheEdge(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Trivia.cs": `namespace App {
    public class Crate { }
    public class Widget { }
    public interface IBox<T> {
        int Get(int id);
    }
    public class CrateBox : IBox<Crate> {
        public int Get(int id) { return 1; }
    }
    public class WidgetBox : IBox<Widget> {
        public int Get(int id) { return 2; }
    }
    public class Flow {
        private readonly IBox</**/Crate> _box;
        public Flow(IBox<Crate> b) { _box = b; }
        public int Pull() { return _box.Get(1); }
    }
}`,
	})
	New(g).ResolveAll()

	callerID := "Trivia.cs::Flow.Pull"
	bindFieldReceiverCall(t, g, callerID, "_box", "Trivia.cs::IBox.Get")

	ResolveCSharpInterfaceDispatch(g)

	targets := dispatchTargets(g, callerID)
	assert.Contains(t, targets, "Trivia.cs::CrateBox.Get",
		"trivia-bearing and plain spellings name one constructed interface - the edge stays")
	assert.NotContains(t, targets, "Trivia.cs::WidgetBox.Get",
		"the argument still reads as Crate - the different closure still filters")
}

// Re-review RED: a DECLARATION-side escaped type parameter (`class
// Outer<@T>`) is the same open parameter T every use-side spelling names.
// Storing raw "@T" in the open-parameter set while the use side
// normalizes to "T" stamps the open receiver as CLOSED over a type
// called "T" - and exact-head dispatch then returns no targets at all.
func TestResolveCSharpInterfaceDispatch_EscapedDeclarationParamKeepsFanout(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Outer.cs": `namespace App {
    public class Crate { }
    public class Widget { }
    public interface IBox<T> {
        int Get(int id);
    }
    public class CrateBox : IBox<Crate> {
        public int Get(int id) { return 1; }
    }
    public class WidgetBox : IBox<Widget> {
        public int Get(int id) { return 2; }
    }
    public class Outer<@T> {
        private readonly IBox<@T> _box;
        public Outer(IBox<@T> b) { _box = b; }
        public int Pull() { return _box.Get(1); }
    }
}`,
	})
	New(g).ResolveAll()

	callerID := "Outer.cs::Outer.Pull"
	bindFieldReceiverCall(t, g, callerID, "_box", "Outer.cs::IBox.Get")

	ResolveCSharpInterfaceDispatch(g)

	targets := dispatchTargets(g, callerID)
	assert.Contains(t, targets, "Outer.cs::CrateBox.Get",
		"an open receiver keeps the conservative full fan-out")
	assert.Contains(t, targets, "Outer.cs::WidgetBox.Get",
		"an open receiver keeps the conservative full fan-out")
}

// Re-review RED (unicode twin): `class Outer<\u0054>` declares the same
// parameter T - the escape is a legal identifier spelling the decoder
// must fold before the open-parameter set is consulted.
func TestResolveCSharpInterfaceDispatch_UnicodeEscapedDeclarationParamKeepsFanout(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Uni.cs": `namespace App {
    public class Crate { }
    public interface IBox<T> {
        int Get(int id);
    }
    public class CrateBox : IBox<Crate> {
        public int Get(int id) { return 1; }
    }
    public class Outer<\u0054> {
        private readonly IBox<T> _box;
        public Outer(IBox<T> b) { _box = b; }
        public int Pull() { return _box.Get(1); }
    }
}`,
	})
	New(g).ResolveAll()

	callerID := "Uni.cs::Outer.Pull"
	bindFieldReceiverCall(t, g, callerID, "_box", "Uni.cs::IBox.Get")

	ResolveCSharpInterfaceDispatch(g)

	targets := dispatchTargets(g, callerID)
	assert.Contains(t, targets, "Uni.cs::CrateBox.Get",
		"the escaped declaration parameter is open - the fan-out survives")
}

// Re-review RED: alias keys and arguments must live in ONE normalization
// domain. `using @Entity = App.Crate;` stores "@Entity" in the alias set
// while the argument normalizer strips the verbatim prefix to "Entity" -
// the alias guard misses and the valid implementor is filtered.
func TestResolveCSharpInterfaceDispatch_VerbatimAliasReceiverNeverFilters(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Alias.cs": `using @Entity = App.Crate;

namespace App {
    public class Crate { }
    public class Widget { }
    public interface IBox<T> {
        int Get(int id);
    }
    public class CrateBox : IBox<Crate> {
        public int Get(int id) { return 1; }
    }
    public class WidgetBox : IBox<Widget> {
        public int Get(int id) { return 2; }
    }
    public class Flow {
        private readonly IBox<@Entity> _box;
        public Flow(IBox<@Entity> b) { _box = b; }
        public int Pull() { return _box.Get(1); }
    }
}`,
	})
	New(g).ResolveAll()

	callerID := "Alias.cs::Flow.Pull"
	bindFieldReceiverCall(t, g, callerID, "_box", "Alias.cs::IBox.Get")

	ResolveCSharpInterfaceDispatch(g)

	targets := dispatchTargets(g, callerID)
	assert.Contains(t, targets, "Alias.cs::CrateBox.Get",
		"@Entity denotes the alias - the stamp must refuse and the fan-out survive")
	assert.Contains(t, targets, "Alias.cs::WidgetBox.Get",
		"an alias-opaque receiver keeps the conservative full fan-out")
}

// Re-review RED: a project-global alias whose identifier the argument
// normalizer FOLDS (`global using Int32 = App.Crate;`) escapes the alias
// guard - the receiver argument is canonicalized to "int" while the
// global-alias metadata says "Int32", and the guard compares the two in
// different domains. The alias-comparable forms must meet the stamp in
// the stamp's own domain.
func TestResolveCSharpInterfaceDispatch_FoldableGlobalAliasNeverFilters(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Global.cs": `global using Int32 = App.Crate;
`,
		"Stores.cs": `namespace App {
    public class Crate { }
    public class Widget { }
    public interface IBox<T> {
        int Get(int id);
    }
    public class CrateBox : IBox<Crate> {
        public int Get(int id) { return 1; }
    }
    public class WidgetBox : IBox<Widget> {
        public int Get(int id) { return 2; }
    }
}`,
		"Flow.cs": `namespace App {
    public class Flow {
        private readonly IBox<Int32> _box;
        public Flow(IBox<Int32> b) { _box = b; }
        public int Pull() { return _box.Get(1); }
    }
}`,
	})
	New(g).ResolveAll()

	callerID := "Flow.cs::Flow.Pull"
	bindFieldReceiverCall(t, g, callerID, "_box", "Stores.cs::IBox.Get")

	ResolveCSharpInterfaceDispatch(g)

	targets := dispatchTargets(g, callerID)
	assert.Contains(t, targets, "Stores.cs::CrateBox.Get",
		"Int32 legally denotes the project-global alias App.Crate - the stamp must refuse")
}

// Re-review RED: same-line field-read evidence must not certify a
// DIFFERENT expression's receiver. `this._box.Save()` emits the field
// read for _box; the shadowing parameter's `_box.Get()` shares the
// physical line, and binding the parameter's call through Flow._box
// filtered the valid WidgetBox.Get out of the store.
func TestResolveCSharpInterfaceDispatch_SameLineFieldReadDoesNotBindShadowingParam(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Boxes.cs": `namespace App {
    public sealed class Crate { }
    public sealed class Widget { }

    public interface IBox<T> {
        int Get();
        int Save();
    }

    public sealed class CrateBox : IBox<Crate> {
        public int Get() { return 1; }
        public int Save() { return 1; }
    }

    public sealed class WidgetBox : IBox<Widget> {
        public int Get() { return 1; }
        public int Save() { return 1; }
    }

    public class Flow {
        private readonly IBox<Crate> _box;
        public Flow(IBox<Crate> b) { _box = b; }
        public int Pull(IBox<Widget> _box) { return this._box.Save() + _box.Get(); }
    }
}`,
	})
	New(g).ResolveAll()

	callerID := "Boxes.cs::Flow.Pull"
	bindMemberCall(t, g, callerID, "Get", "Boxes.cs::IBox.Get")

	ResolveCSharpInterfaceDispatch(g)

	targets := dispatchTargets(g, callerID)
	assert.Contains(t, targets, "Boxes.cs::WidgetBox.Get",
		"the parameter shadows the field - its receiver is IBox<Widget> and the Widget impl must stay in")
}

// Re-review RED (P2): a positional record property is a first-class
// receiver - `record Flow(IBox<Crate> Store)` must narrow Store.Get()
// exactly like an ordinary property of the same declared type.
func TestResolveCSharpInterfaceDispatch_PositionalRecordPropertyNarrows(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Rec.cs": `namespace App {
    public class Crate { }
    public class Widget { }
    public interface IBox<T> {
        int Get(int id);
    }
    public class CrateBox : IBox<Crate> {
        public int Get(int id) { return 1; }
    }
    public class WidgetBox : IBox<Widget> {
        public int Get(int id) { return 2; }
    }
    public record Flow(IBox<Crate> Store) {
        public int Pull() { return Store.Get(1); }
    }
}`,
	})
	New(g).ResolveAll()

	callerID := "Rec.cs::Flow.Pull"
	bindFieldReceiverCall(t, g, callerID, "Store", "Rec.cs::IBox.Get")

	ResolveCSharpInterfaceDispatch(g)

	targets := dispatchTargets(g, callerID)
	assert.Contains(t, targets, "Rec.cs::CrateBox.Get")
	assert.NotContains(t, targets, "Rec.cs::WidgetBox.Get",
		"the positional property's declared closure is IBox<Crate> - the Widget impl filters")
}
