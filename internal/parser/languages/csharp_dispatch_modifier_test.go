package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// Class-virtual dispatch evidence: the fan-out pass needs to know which
// class members participate in runtime dispatch. `virtual`/`abstract`
// members are family roots, `override` members are family members, and a
// plain or `new`-hiding member is statically bound and must stay OUT -
// name matching alone cannot tell these apart.
func TestCSharpExtractor_DispatchModifierStamps(t *testing.T) {
	src := []byte(`namespace App {
    public abstract class PanelBase {
        public abstract int Render(int id);
        public virtual int Flush(int id) { return 0; }
        public int Plain(int id) { return 0; }
    }
    public class TilePanel : PanelBase {
        public override int Render(int id) { return 1; }
        public sealed override int Flush(int id) { return 2; }
        public new int Plain(int id) { return 3; }
    }
}`)
	e := NewCSharpExtractor()
	result, err := e.Extract("Panels.cs", src)
	require.NoError(t, err)

	byID := map[string]*graph.Node{}
	for _, n := range result.Nodes {
		if n != nil {
			byID[n.ID] = n
		}
	}
	want := map[string]string{
		"Panels.cs::PanelBase.Render": "abstract",
		"Panels.cs::PanelBase.Flush":  "virtual",
		"Panels.cs::TilePanel.Render": "override",
		"Panels.cs::TilePanel.Flush":  "override",
	}
	for id, mod := range want {
		n := byID[id]
		require.NotNil(t, n, id)
		got, _ := n.Meta["dispatch_modifier"].(string)
		assert.Equal(t, mod, got, id)
	}
	for _, id := range []string{"Panels.cs::PanelBase.Plain", "Panels.cs::TilePanel.Plain"} {
		n := byID[id]
		require.NotNil(t, n, id)
		_, stamped := n.Meta["dispatch_modifier"]
		assert.False(t, stamped, "%s is statically bound - no dispatch stamp", id)
	}
}

// A `base.`-qualified call is STATIC dispatch - it can never reach an
// override, so the fan-out pass must be able to recognise and skip the
// site. The extractor marks it; an unqualified or `this.`-qualified call
// stays unmarked (those still dispatch at runtime).
func TestCSharpExtractor_BaseQualifiedCallCarriesQualifier(t *testing.T) {
	src := []byte(`namespace App {
    public class PanelBase {
        public virtual int Flush(int id) { return 0; }
        public virtual int Render(int id) { return 0; }
    }
    public class TilePanel : PanelBase {
        public override int Flush(int id) { return base.Flush(id); }
        public override int Render(int id) { return this.Render(id); }
    }
}`)
	e := NewCSharpExtractor()
	result, err := e.Extract("Panels.cs", src)
	require.NoError(t, err)

	baseMarked, thisMarked := false, false
	for _, edge := range result.Edges {
		if edge == nil || edge.Kind != graph.EdgeCalls || edge.Meta == nil {
			continue
		}
		q, _ := edge.Meta["receiver_qualifier"].(string)
		switch edge.From {
		case "Panels.cs::TilePanel.Flush":
			if q == "base" {
				baseMarked = true
			}
		case "Panels.cs::TilePanel.Render":
			if q != "" {
				thisMarked = true
			}
		}
	}
	assert.True(t, baseMarked, "the base-qualified call site must carry receiver_qualifier=base")
	assert.False(t, thisMarked, "a this-qualified call still dispatches at runtime - no qualifier mark")
}
