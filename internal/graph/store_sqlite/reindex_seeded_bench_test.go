package store_sqlite

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// Seeded variant of BenchmarkReindexEdgesResolvedConversions50K: the plain
// bench reindexes into a fresh 50k-row store where every B-tree page stays in
// the connection page cache and the sequential target ids insert nearly
// sorted, so it overstates production throughput ~7x (70k vs ~10k rows/s on a
// 2.2M-edge store). This one fattens the table with a background corpus of
// production-length ids first, and binds the conversions to targets scattered
// across that keyspace, so index maintenance pays the page-cache and
// tree-depth costs the resolve pass actually sees. The sorted sub-benchmark
// applies the batch in new-target order to price B-tree locality separately.

const (
	seededBackgroundEdges = 1_000_000
	seededConversionCount = 50_000
	seededAddChunk        = 50_000
)

func seededBackgroundID(i int) string {
	return fmt.Sprintf(`repo/area%02d/pkg%03d\file%03d.cs::Space%02d.Type%03d.Member%03d`,
		i%97, i%911, i%499, i%89, i%997, i%701)
}

// buildSeededTemplate populates a template store once; iterations copy the
// closed file instead of re-adding a million edges.
func buildSeededTemplate(b *testing.B, path string) {
	b.Helper()
	store, err := Open(path)
	if err != nil {
		b.Fatal(err)
	}
	background := make([]*graph.Edge, 0, seededAddChunk)
	for i := 0; i < seededBackgroundEdges; i++ {
		background = append(background, &graph.Edge{
			From: seededBackgroundID(i) + ".Caller", To: seededBackgroundID(i + 1),
			Kind:     graph.EdgeCalls,
			FilePath: fmt.Sprintf(`repo/area%02d/pkg%03d\file%03d.cs`, i%97, i%911, i%499),
			Line:     i%3000 + 1,
		})
		if len(background) == seededAddChunk {
			store.AddBatch(nil, background)
			background = background[:0]
		}
	}
	if len(background) > 0 {
		store.AddBatch(nil, background)
	}
	unresolved := make([]*graph.Edge, 0, seededConversionCount)
	for i := 0; i < seededConversionCount; i++ {
		unresolved = append(unresolved, &graph.Edge{
			From:     fmt.Sprintf(`repo/callers/pkg%03d\callers%05d.cs::Caller%05d.Invoke`, i%211, i, i),
			To:       fmt.Sprintf("unresolved::Symbol%05d", i),
			Kind:     graph.EdgeCalls,
			FilePath: fmt.Sprintf(`repo/callers/pkg%03d\callers%05d.cs`, i%211, i),
			Line:     i%400 + 1,
		})
	}
	store.AddBatch(nil, unresolved)
	if err := store.Close(); err != nil {
		b.Fatal(err)
	}
}

func copySeededTemplate(b *testing.B, src, dst string) {
	b.Helper()
	in, err := os.Open(src)
	if err != nil {
		b.Fatal(err)
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		b.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		b.Fatal(err)
	}
	if err := out.Close(); err != nil {
		b.Fatal(err)
	}
}

func BenchmarkReindexEdgesResolvedConversionsSeeded1M(b *testing.B) {
	dir := b.TempDir()
	template := filepath.Join(dir, "template.sqlite")
	buildSeededTemplate(b, template)

	// Conversions bind to existing background symbols, scattered across the
	// keyspace by a multiplicative hash — the shape a resolve pass produces.
	batch := make([]graph.EdgeReindex, 0, seededConversionCount)
	for i := 0; i < seededConversionCount; i++ {
		target := seededBackgroundID(int((uint32(i) * 2654435761) % seededBackgroundEdges))
		batch = append(batch, graph.EdgeReindex{
			OldTo: fmt.Sprintf("unresolved::Symbol%05d", i),
			Edge: &graph.Edge{
				From: fmt.Sprintf(`repo/callers/pkg%03d\callers%05d.cs::Caller%05d.Invoke`, i%211, i, i),
				To:   target, Kind: graph.EdgeCalls,
				FilePath:   fmt.Sprintf(`repo/callers/pkg%03d\callers%05d.cs`, i%211, i),
				Line:       i%400 + 1,
				Confidence: 0.95, Origin: "resolved",
			},
		})
	}

	for _, sorted := range []bool{false, true} {
		name := "unsorted"
		if sorted {
			name = "sortedByNewTo"
		}
		b.Run(name, func(b *testing.B) {
			run := append([]graph.EdgeReindex(nil), batch...)
			if sorted {
				sort.Slice(run, func(x, y int) bool { return run[x].Edge.To < run[y].Edge.To })
			}
			b.ReportAllocs()
			b.ReportMetric(seededConversionCount, "edges/op")
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				dbPath := filepath.Join(dir, fmt.Sprintf("run-%s-%d.sqlite", name, i))
				copySeededTemplate(b, template, dbPath)
				store, err := Open(dbPath)
				if err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
				stats, err := store.reindexEdgesSetOriented(run)
				b.StopTimer()
				if err != nil {
					_ = store.Close()
					b.Fatal(err)
				}
				if stats.updatedRows != seededConversionCount || stats.deletedRows != 0 {
					_ = store.Close()
					b.Fatalf("unexpected fast-path stats: %+v", stats)
				}
				if err := store.Close(); err != nil {
					b.Fatal(err)
				}
				_ = os.Remove(dbPath)
			}
		})
	}
}
