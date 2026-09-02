package tstypes

// Pins for issue #731: the extractor owns a C# property's calls by the
// span it RECORDED (the body-bearing fragment of a partial property, the
// body-bearing arm of a conditional-compilation pair), which can lie
// outside the property NODE's lines (the first fragment's). buildIndex's
// extent guard must test the stub against the recorded ownership span,
// not the declaration span, or adoption never fires for exactly the
// owner kind it exists to serve.

import (
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

// C# 13 partial property, declaring fragment first: the node spans the
// declaring line, the extractor's stub sits in the implementing fragment.
func TestCSharp_PartialPropertyDeclaringFirstResolvesBodyCall(t *testing.T) {
	g, dir := buildFixture(t, map[string]string{
		"A/Svc.cs": csSvcInt,
		"B/App.cs": `namespace B {
    public partial class App {
        private Svc worker;
        public partial int Tick { get; set; }
    }

    public partial class App {
        public partial int Tick {
            get { worker.Run(); return 1; }
        }
    }
}
`,
	})
	p := NewProvider(CSharpSpec(), zap.NewNop())
	res, err := p.Enrich(g, dir)
	if err != nil {
		t.Fatal(err)
	}
	tick := nodeByNameKind(t, g, "Tick", graph.KindField)
	run := nodeByNameKind(t, g, "Run", graph.KindMethod)
	e := callEdgeTo(g, tick.ID, run.ID)
	if e == nil {
		t.Fatalf("partial property's body call not resolved (EdgesConfirmed=%d); edges: %v", res.EdgesConfirmed, g.GetOutEdges(tick.ID))
	}
	assertASTProvenance(t, e, "csharp-types")
	if got := callEdgesNamed(g, tick.ID, "Run"); len(got) != 1 {
		t.Errorf("want exactly one Run edge on the property, got %d: %v", len(got), got)
	}
}

// Conditional compilation: the same property declared in both arms of an
// #if / #else (tree-sitter parses both); the body-bearing arm is second.
func TestCSharp_ConditionalPropertySecondArmResolvesBodyCall(t *testing.T) {
	g, dir := buildFixture(t, map[string]string{
		"A/Svc.cs": csSvcInt,
		"B/App.cs": `namespace B {
    public class App {
        private Svc worker;
#if LEGACY
        public int Tick { get; set; }
#else
        public int Tick { get { worker.Run(); return 1; } }
#endif
    }
}
`,
	})
	p := NewProvider(CSharpSpec(), zap.NewNop())
	res, err := p.Enrich(g, dir)
	if err != nil {
		t.Fatal(err)
	}
	tick := nodeByNameKind(t, g, "Tick", graph.KindField)
	run := nodeByNameKind(t, g, "Run", graph.KindMethod)
	e := callEdgeTo(g, tick.ID, run.ID)
	if e == nil {
		t.Fatalf("conditional property's body call not resolved (EdgesConfirmed=%d); edges: %v", res.EdgesConfirmed, g.GetOutEdges(tick.ID))
	}
	assertASTProvenance(t, e, "csharp-types")
	if got := callEdgesNamed(g, tick.ID, "Run"); len(got) != 1 {
		t.Errorf("want exactly one Run edge on the property, got %d: %v", len(got), got)
	}
}

// Control: implementing fragment FIRST. The node already spans the body,
// no ownership stamp is needed, and the call resolves as before.
func TestCSharp_PartialPropertyImplementingFirstStillResolves(t *testing.T) {
	g, dir := buildFixture(t, map[string]string{
		"A/Svc.cs": csSvcInt,
		"B/App.cs": `namespace B {
    public partial class App {
        private Svc worker;
        public partial int Tick {
            get { worker.Run(); return 1; }
        }
    }

    public partial class App {
        public partial int Tick { get; set; }
    }
}
`,
	})
	p := NewProvider(CSharpSpec(), zap.NewNop())
	if _, err := p.Enrich(g, dir); err != nil {
		t.Fatal(err)
	}
	tick := nodeByNameKind(t, g, "Tick", graph.KindField)
	run := nodeByNameKind(t, g, "Run", graph.KindMethod)
	if e := callEdgeTo(g, tick.ID, run.ID); e == nil {
		t.Fatalf("implementing-first partial property's body call not resolved; edges: %v", g.GetOutEdges(tick.ID))
	} else {
		assertASTProvenance(t, e, "csharp-types")
	}
}
