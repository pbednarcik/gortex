package resolver

import (
	"fmt"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// One caller with many through-interface sites. Each bound site consults
// the caller's out-edge evidence twice - the companion join and the
// field-read proof - and a per-site rescan of the caller's adjacency
// makes the dispatch pass quadratic in the caller's site count. The
// shape is unrealistic in the high hundreds, but the growth curve is
// the regression this benchmark pins.
func BenchmarkCSharpIfaceDispatchManySitesOneCaller(b *testing.B) {
	for _, sites := range []int{200, 800, 3200} {
		b.Run(fmt.Sprintf("sites=%d", sites), func(b *testing.B) {
			var body strings.Builder
			for i := 0; i < sites; i++ {
				fmt.Fprintf(&body, "            t += _box.Get(%d);\n", i)
			}
			files := map[string]string{
				"Many.cs": `namespace App {
    public class Crate { }
    public class Widget { }
    public interface IBox<T> { int Get(int id); }
    public class CrateBox : IBox<Crate> { public int Get(int id) { return 1; } }
    public class WidgetBox : IBox<Widget> { public int Get(int id) { return 2; } }
    public class Flow {
        private readonly IBox<Crate> _box;
        public Flow(IBox<Crate> b) { _box = b; }
        public int Pull() {
            int t = 0;
` + body.String() + `            return t;
        }
    }
}`,
			}

			const callerID = "Many.cs::Flow.Pull"
			bindEverySite := func(g graph.Store) {
				var companions []*graph.Edge
				for _, e := range g.GetOutEdges(callerID) {
					if e != nil && e.Kind == graph.EdgeCalls && e.To == "unresolved::*.Get" {
						companions = append(companions, e)
					}
				}
				if len(companions) != sites {
					b.Fatalf("fixture: %d companions, want %d", len(companions), sites)
				}
				for _, c := range companions {
					g.AddEdge(&graph.Edge{
						From: callerID, To: "Many.cs::IBox.Get", Kind: graph.EdgeCalls,
						FilePath: c.FilePath, Line: c.Line,
						Origin: graph.OriginASTResolved, Confidence: 0.95,
					})
				}
			}

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				g := buildCSharpResolverGraph(b, files)
				New(g).ResolveAll()
				bindEverySite(g)
				b.StartTimer()
				ResolveCSharpInterfaceDispatch(g)
			}
		})
	}
}
