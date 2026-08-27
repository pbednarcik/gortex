package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Class-virtual dispatch: the sibling of the interface implements-family
// cascade. A virtual/abstract member on a base CLASS and its override
// closure form one Roslyn family - a call bound to any member surfaces on
// every member. Anchors require the extractor's dispatch_modifier stamp
// (virtual/abstract root, override members), so name-matched hiding and
// statically-bound members never join, and a store extracted before the
// stamp existed synthesizes nothing until it re-extracts.

// Through-base calls fan to every override; a plain (non-virtual) member
// of the same base never seeds a family.
func TestResolveCSharpVirtualDispatch_ThroughBaseFansToOverrides(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Panels.cs": `namespace App {
    public abstract class PanelBase {
        public abstract int Render(int id);
        public int Plain(int id) { return 0; }
    }
    public class TilePanel : PanelBase {
        public override int Render(int id) { return 1; }
    }
    public class GridPanel : PanelBase {
        public override int Render(int id) { return 2; }
    }
    public class Decoy {
        public int Plain(int id) { return 9; }
    }
    public class Wall {
        private readonly PanelBase _panel;
        public Wall(PanelBase p) { _panel = p; }
        public int Draw(int id) {
            return _panel.Render(id) + _panel.Plain(id);
        }
    }
}`,
	})
	New(g).ResolveAll()

	callerID := "Panels.cs::Wall.Draw"
	bindFieldReceiverCall(t, g, callerID, "_panel", "Panels.cs::PanelBase.Render")

	ResolveCSharpInterfaceDispatch(g)

	targets := dispatchTargets(g, callerID)
	assert.Contains(t, targets, "Panels.cs::TilePanel.Render",
		"a base-typed receiver dispatches into every override")
	assert.Contains(t, targets, "Panels.cs::GridPanel.Render",
		"a base-typed receiver dispatches into every override")
	assert.NotContains(t, targets, "Panels.cs::Decoy.Plain",
		"a plain member never seeds a family - no fan-out to unrelated same-names")
	for _, to := range targets {
		assert.NotEqual(t, "Panels.cs::PanelBase.Plain", to,
			"a non-virtual member never seeds a family")
	}
}

// A `new`-hiding member matches by name but is statically bound - it must
// stay out of the family its name collides with.
func TestResolveCSharpVirtualDispatch_NewHidingMemberStaysOut(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Hiding.cs": `namespace App {
    public class PanelBase {
        public virtual int Render(int id) { return 0; }
    }
    public class TilePanel : PanelBase {
        public override int Render(int id) { return 1; }
    }
    public class DecoyPanel : PanelBase {
        public new int Render(int id) { return 9; }
    }
    public class Wall {
        private readonly PanelBase _panel;
        public Wall(PanelBase p) { _panel = p; }
        public int Draw(int id) {
            return _panel.Render(id);
        }
    }
}`,
	})
	New(g).ResolveAll()

	callerID := "Hiding.cs::Wall.Draw"
	bindFieldReceiverCall(t, g, callerID, "_panel", "Hiding.cs::PanelBase.Render")

	ResolveCSharpInterfaceDispatch(g)

	targets := dispatchTargets(g, callerID)
	assert.Contains(t, targets, "Hiding.cs::TilePanel.Render",
		"the override joins the family")
	assert.NotContains(t, targets, "Hiding.cs::DecoyPanel.Render",
		"a new-hiding member is statically bound - never in the family")
}

// A base-qualified call is static dispatch: `base.Flush()` executes the
// base body, never an override. The site must not fan.
func TestResolveCSharpVirtualDispatch_BaseQualifiedCallNeverFans(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"BaseCalls.cs": `namespace App {
    public class PanelBase {
        public virtual int Flush(int id) { return 0; }
    }
    public class TilePanel : PanelBase {
        public override int Flush(int id) { return base.Flush(id) + 1; }
    }
    public class OtherPanel : PanelBase {
        public override int Flush(int id) { return 2; }
    }
}`,
	})
	New(g).ResolveAll()

	callerID := "BaseCalls.cs::TilePanel.Flush"
	require.Contains(t, callTargetsFrom(g, callerID), "BaseCalls.cs::PanelBase.Flush",
		"fixture: base.Flush binds the base member")

	ResolveCSharpInterfaceDispatch(g)

	targets := dispatchTargets(g, callerID)
	assert.NotContains(t, targets, "BaseCalls.cs::OtherPanel.Flush",
		"base-qualified dispatch is static - the site never fans to sibling overrides")
}

// The sibling mechanism: a call bound directly to one override still
// surfaces on the base member and the sibling overrides - Roslyn reports
// the union for the whole family.
func TestResolveCSharpVirtualDispatch_SiblingOverrideCallFansToFamily(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Sibs.cs": `namespace App {
    public class PanelBase {
        public virtual int Render(int id) { return 0; }
    }
    public class TilePanel : PanelBase {
        public override int Render(int id) { return 1; }
    }
    public class GridPanel : PanelBase {
        public override int Render(int id) { return 2; }
    }
    public class Studio {
        public int Snap(int id) {
            TilePanel p = new TilePanel();
            return p.Render(id);
        }
    }
}`,
	})
	New(g).ResolveAll()

	callerID := "Sibs.cs::Studio.Snap"
	require.Contains(t, callTargetsFrom(g, callerID), "Sibs.cs::TilePanel.Render",
		"fixture: the typed-local call binds the override directly")

	ResolveCSharpInterfaceDispatch(g)

	targets := dispatchTargets(g, callerID)
	assert.Contains(t, targets, "Sibs.cs::PanelBase.Render",
		"the family root receives the sibling site")
	assert.Contains(t, targets, "Sibs.cs::GridPanel.Render",
		"sibling overrides receive the site")
}

// Generic base classes gate on their closed type arguments exactly like
// constructed interfaces: a StoreBase<Crate> receiver never dispatches
// into the StoreBase<Widget> subclass.
func TestResolveCSharpVirtualDispatch_GenericBaseArgsGateFanout(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Stores.cs": `namespace App {
    public class Widget { }
    public class Crate { }
    public abstract class StoreBase<T> {
        public abstract int Keep(int id);
    }
    public class CrateStore : StoreBase<Crate> {
        public override int Keep(int id) { return 1; }
    }
    public class WidgetStore : StoreBase<Widget> {
        public override int Keep(int id) { return 2; }
    }
    public class Flow {
        private readonly StoreBase<Crate> _crates;
        public Flow(StoreBase<Crate> c) { _crates = c; }
        public int Pull(int id) {
            return _crates.Keep(id);
        }
    }
}`,
	})
	New(g).ResolveAll()

	callerID := "Stores.cs::Flow.Pull"
	bindFieldReceiverCall(t, g, callerID, "_crates", "Stores.cs::StoreBase.Keep")

	ResolveCSharpInterfaceDispatch(g)

	targets := dispatchTargets(g, callerID)
	assert.Contains(t, targets, "Stores.cs::CrateStore.Keep",
		"the type-compatible subclass receives the fan-out")
	assert.NotContains(t, targets, "Stores.cs::WidgetStore.Keep",
		"a StoreBase<Crate> receiver never dispatches into the StoreBase<Widget> subclass")
}
