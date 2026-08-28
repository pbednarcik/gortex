package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// A stamp is recorded per (type -> erased interface), but a type can
// legally implement several CONSTRUCTIONS of one generic interface.
// `class C : IEnumerable<int>, IEnumerable<string>` is the canonical
// form: CS0695 only fires when the type arguments contain type
// parameters that could unify, so distinct concrete types are legal.
//
// Only the type's own DIRECT base-list edge is recorded, and that single
// closure is then painted onto every same-named member of the type. A
// second construction reaching the type through an inherited interface
// or a base class is invisible, so the members implementing it are
// filtered against a closure they do not have.
//
// The rule these tests pin: a stamp is usable only when the implementor
// reaches the interface by exactly ONE closure. Two distinct closures,
// or any path that carries no closure at all, must disarm the filter for
// that implementor - while leaving it armed for everyone else.

// 2a: the second closure arrives through an inherited interface.
func TestResolveCSharpInterfaceDispatch_SecondClosureViaInheritedInterface(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Dual.cs": `namespace App {
    public class Crate { }
    public class Widget { }
    public interface IBox<T> { void Put(T item); }
    public interface ICrateBox : IBox<Crate> { }
    public class CrateBox : IBox<Crate> { public void Put(Crate c) { } }
    public class WidgetBox : IBox<Widget> { public void Put(Widget w) { } }
    public class Store : ICrateBox, IBox<Widget> {
        public void Put(Crate c) { }
        public void Put(Widget w) { }
    }
    public class Flow {
        private readonly IBox<Crate> _box;
        public Flow(IBox<Crate> b) { _box = b; }
        public void Pull(Crate c) { _box.Put(c); }
    }
}`,
	})
	New(g).ResolveAll()

	const callerID = "Dual.cs::Flow.Pull"
	bindFieldReceiverCall(t, g, callerID, "_box", "Dual.cs::IBox.Put")
	ResolveCSharpInterfaceDispatch(g)

	// WidgetBox reaches IBox by exactly one closure and that closure is
	// type-impossible for an IBox<Crate> receiver, so the gate keeps
	// doing its job there. Store reaches IBox by two - IBox<Crate> via
	// ICrateBox and IBox<Widget> directly - so no single closure
	// describes it and both overloads stay.
	assert.ElementsMatch(t, []string{
		"Dual.cs::CrateBox.Put",
		"Dual.cs::Store.Put",
		"Dual.cs::Store.Put_L10",
	}, dispatchTargets(g, callerID),
		"an ambiguous implementor keeps its fan-out; an unambiguous type-impossible one is still filtered")
}

// 2b: the second closure arrives through a base class, and the member
// dropped is the `override` that actually executes at runtime.
func TestResolveCSharpInterfaceDispatch_SecondClosureViaBaseClass(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Derived.cs": `namespace App {
    public class Crate { }
    public class Widget { }
    public interface IBox<T> { void Put(T item); }
    public class CrateBox : IBox<Crate> { public virtual void Put(Crate c) { } }
    public class WidgetBox : IBox<Widget> { public void Put(Widget w) { } }
    public class Store : CrateBox, IBox<Widget> {
        public override void Put(Crate c) { }
        public void Put(Widget w) { }
    }
    public class Flow {
        private readonly IBox<Crate> _box;
        public Flow(IBox<Crate> b) { _box = b; }
        public void Pull(Crate c) { _box.Put(c); }
    }
}`,
	})
	New(g).ResolveAll()

	const callerID = "Derived.cs::Flow.Pull"
	bindFieldReceiverCall(t, g, callerID, "_box", "Derived.cs::IBox.Put")
	ResolveCSharpInterfaceDispatch(g)

	// Keeping only the base method while dropping the override is the
	// worst possible orientation for a virtual-dispatch answer: the
	// surviving target is the one that does NOT run.
	assert.ElementsMatch(t, []string{
		"Derived.cs::CrateBox.Put",
		"Derived.cs::Store.Put",
		"Derived.cs::Store.Put_L9",
	}, dispatchTargets(g, callerID),
		"a closure inherited through a base class is still a second closure")
}
