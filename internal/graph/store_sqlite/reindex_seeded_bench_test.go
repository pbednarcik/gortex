package store_sqlite

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

// Seeded variant of BenchmarkReindexEdgesResolvedConversions50K: the plain
// bench reindexes into a fresh 50k-row store where every B-tree page stays in
// the connection page cache and the sequential target ids insert nearly
// sorted, so it overstates production throughput ~7x (70k vs ~10k rows/s on a
// 2.2M-edge store). This one fattens the table with a background corpus of
// production-length ids first and binds the conversions to targets scattered
// across that keyspace, then prices the FULL cost of a WAL auto-checkpoint
// spacing choice across the three phases production pays: the write burst
// itself, the read-back of freshly written pages through the read pool (the
// guard/enrichment/dispatch pattern — pages still resident in a large WAL
// are served by wal-index probe + pread instead of the main file's mmap),
// and the end-of-batch TRUNCATE drain (where a deferred checkpoint bill
// lands). Spacing is set per run via PRAGMA on the writer connection, so the
// DSN default under test does not bias the sweep.

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

// readBackConversions probes every converted row through the read pool the
// way the guard and enrichment passes do: one index-only probe of the fresh
// edges_by_to leaf and one main-table row fetch. While those pages sit in a
// large WAL they cost a wal-index lookup + pread each instead of an mmap
// access — the read-side tax of wide checkpoint spacing.
func readBackConversions(b *testing.B, store *Store, batch []graph.EdgeReindex) {
	b.Helper()
	for _, r := range batch {
		var n int
		if err := store.db.QueryRow(
			`SELECT COUNT(*) FROM edges WHERE to_id = ? AND kind = ?`,
			r.Edge.To, string(r.Edge.Kind)).Scan(&n); err != nil {
			b.Fatal(err)
		}
		if n == 0 {
			b.Fatalf("converted target %q not found", r.Edge.To)
		}
		var conf float64
		if err := store.db.QueryRow(
			`SELECT confidence FROM edges WHERE file_path = ? AND line = ?`,
			r.Edge.FilePath, r.Edge.Line).Scan(&conf); err != nil {
			b.Fatal(err)
		}
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

	for _, spacing := range []int{1000, 8000, 100000} {
		b.Run(fmt.Sprintf("ckpt%d", spacing), func(b *testing.B) {
			var writeTotal, readTotal, drainTotal time.Duration
			b.ReportAllocs()
			b.ReportMetric(seededConversionCount, "edges/op")
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				dbPath := filepath.Join(dir, fmt.Sprintf("run-%d-%d.sqlite", spacing, i))
				copySeededTemplate(b, template, dbPath)
				store, err := Open(dbPath)
				if err != nil {
					b.Fatal(err)
				}
				if _, err := store.writerDB.Exec(
					fmt.Sprintf("PRAGMA wal_autocheckpoint = %d", spacing)); err != nil {
					_ = store.Close()
					b.Fatal(err)
				}
				b.StartTimer()

				phase := time.Now()
				stats, err := store.reindexEdgesSetOriented(batch)
				writeTotal += time.Since(phase)
				if err != nil {
					b.StopTimer()
					_ = store.Close()
					b.Fatal(err)
				}
				phase = time.Now()
				readBackConversions(b, store, batch)
				readTotal += time.Since(phase)
				phase = time.Now()
				if err := store.CheckpointWAL(); err != nil {
					b.StopTimer()
					_ = store.Close()
					b.Fatal(err)
				}
				drainTotal += time.Since(phase)

				b.StopTimer()
				if stats.updatedRows != seededConversionCount || stats.deletedRows != 0 {
					_ = store.Close()
					b.Fatalf("unexpected fast-path stats: %+v", stats)
				}
				if err := store.Close(); err != nil {
					b.Fatal(err)
				}
				_ = os.Remove(dbPath)
			}
			n := float64(b.N)
			b.ReportMetric(float64(writeTotal.Milliseconds())/n, "write-ms/op")
			b.ReportMetric(float64(readTotal.Milliseconds())/n, "read-ms/op")
			b.ReportMetric(float64(drainTotal.Milliseconds())/n, "drain-ms/op")
		})
	}
}
