package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

// Receiver-rebind access-path plan locks.
//
// The two receiver candidate queries pin their indexes with INDEXED BY, but
// INDEXED BY pins the index, never the loop order. When sqlite_stat1 claims
// the partial index nodes_go_receiver_type holds a handful of rows — a
// literal '0 0 0 0 0' row written by ANALYZE while the store held no Go
// type/interface node, or an honest '1 ...' row captured before the types
// landed — the planner drives the join from `c` and re-reads every member_of
// edge once per receiver type. That is O(types x member_of): minutes to hours
// on a production store, and the reason issue #651's rebind pass never
// finished.
//
// These locks therefore run the SAME two production queries under four
// distinct statistics regimes and require the same seek-driven shape from all
// four. The plan must be driven by the edge kind index (global) or the file
// index (scoped); `c` must always be the inner, probed relation.
//
// Test names carry "PlanLock" because the Windows CI leg runs only
// -run 'PlanLock|PlansLocked|PlansNeverScan' in this package.
func TestReceiverRebindPlanLockAcrossStatisticsRegimes(t *testing.T) {
	// One full-corpus store serves four of the five regimes. Building it once
	// is worth real time (the fixture costs ~88s of -race wall clock per
	// build) and costs no coverage, because each regime only rewrites
	// sqlite_stat1 — never the corpus. What it does cost is ORDER, so the
	// subtests below run sequentially and their order is load bearing:
	//
	//	no_stats                 first, and only first: sqlite_stat1 cannot be
	//	                         dropped once ANALYZE has created it.
	//	refreshed                honest statistics, before anything poisons them.
	//	zero_row_from_old_engine poisons the receiver row (and runs the real
	//	                         rebind, which mutates edges).
	//	batch_frontier_zero_row  reuses that poisoned row.
	//
	// tiny_row_natural needs a differently seeded corpus (one type at ANALYZE
	// time), so it keeps its own store.
	s, methodFile, disarmClose := newReceiverPlanLockStore(t, receiverPlanLockAllTypes)

	// Set together with disarmClose when a rebind is abandoned mid-flight.
	// RebindGoMethodReceivers holds s.writeMu for its whole run, so once the
	// budget below expires that gate is held by a goroutine nobody can stop:
	// any later subtest that takes it would block until the package timeout
	// and bury the failure that was already recorded. Later subtests on this
	// shared store skip instead.
	var rebindAbandoned atomic.Bool

	t.Run("no_stats", func(t *testing.T) {
		if statsTableExists(t, s) {
			t.Fatal("fixture created sqlite_stat1 without an explicit refresh")
		}
		withReceiverPlanLockWriter(t, s, func(ctx context.Context, conn *sql.Conn) {
			assertReceiverRebindPlansLocked(t, ctx, conn, methodFile)
		})
	})

	t.Run("refreshed", func(t *testing.T) {
		refreshReceiverPlanLockStats(t, s)
		stat := receiverStatOnReader(t, s)
		if stat == "" {
			t.Fatal("refresh wrote no receiver stat row; the healthy-statistics regime is not under test")
		}
		if got := statRowCount(t, stat); got < receiverPlanLockTypes-50 {
			t.Fatalf("refreshed receiver stat = %q (count %d), want a healthy count >= %d",
				stat, got, receiverPlanLockTypes-50)
		}

		withReceiverPlanLockWriter(t, s, func(ctx context.Context, conn *sql.Conn) {
			assertReceiverRebindPlansLocked(t, ctx, conn, methodFile)
		})
	})

	// A store poisoned before the upgrade: ANALYZE ran while the graph held no
	// Go type at all and wrote the degenerate all-zero row, which then survived
	// every later load because nothing re-analyzed the index.
	t.Run("zero_row_from_old_engine", func(t *testing.T) {
		withReceiverPlanLockWriter(t, s, func(ctx context.Context, conn *sql.Conn) {
			// Hand-edited sqlite_stat1 rows only reach a connection's planner
			// after ANALYZE sqlite_schema on that same connection, so poison
			// and reload on the connection the EXPLAINs (and the real rebind)
			// run on.
			//
			// sqlite_stat1 carries no UNIQUE constraint, so INSERT OR REPLACE
			// would silently append a second row for the same index and leave
			// the honest one in front of it. Delete, then insert.
			if _, err := conn.ExecContext(ctx, `DELETE FROM sqlite_stat1 WHERE idx = 'nodes_go_receiver_type'`); err != nil {
				t.Fatalf("clear receiver stat row: %v", err)
			}
			if _, err := conn.ExecContext(ctx, `INSERT INTO sqlite_stat1(tbl, idx, stat) VALUES ('nodes', 'nodes_go_receiver_type', '0 0 0 0 0')`); err != nil {
				t.Fatalf("poison receiver stat row: %v", err)
			}
			if _, err := conn.ExecContext(ctx, `ANALYZE sqlite_schema`); err != nil {
				t.Fatalf("reload poisoned statistics: %v", err)
			}
			if got := receiverStatOnConn(t, ctx, conn); got != "0 0 0 0 0" {
				t.Fatalf("receiver stat = %q, want the poisoned zero row", got)
			}
			assertReceiverRebindPlansLocked(t, ctx, conn, methodFile)
		})

		// The plan lock is only half the guarantee: the query must still be
		// correct (and terminate) on the poisoned store. Skip it once the plan
		// already failed — the misplan is the diagnosis, and running the pass
		// under it only burns the budget below.
		if t.Failed() {
			return
		}
		type rebindResult struct {
			changed int
			err     error
		}
		done := make(chan rebindResult, 1)
		go func() {
			changed, err := s.RebindGoMethodReceivers("")
			done <- rebindResult{changed: changed, err: err}
		}()
		select {
		case got := <-done:
			if got.err != nil {
				t.Fatalf("rebind on a poisoned-statistics store: %v", got.err)
			}
			if got.changed != receiverPlanLockPhantomEdges {
				t.Fatalf("rebind changed = %d, want %d phantom receiver edges", got.changed, receiverPlanLockPhantomEdges)
			}
		case <-time.After(receiverPlanLockRebindBudget):
			// The pass is still running and still holds the write gate.
			// Closing the store would block on it forever, so abandon the
			// handle and let the failure print. Every later subtest on this
			// store takes the same gate, so mark the store unusable too.
			disarmClose()
			rebindAbandoned.Store(true)
			t.Errorf("receiver rebind did not finish within %s on a poisoned-statistics store", receiverPlanLockRebindBudget)
		}
	})

	// The batched frontier sibling shares the same receiver join and pins its
	// whole f -> m -> e -> c chain with CROSS JOIN, so its loop order is
	// statistics-independent for the same reason the global query's is. This
	// lock is the enforcement: without it the batch query could quietly drift
	// back to the state the global one was in before #651.
	t.Run("batch_frontier_zero_row", func(t *testing.T) {
		if rebindAbandoned.Load() {
			t.Skip("previous subtest abandoned a rebind still holding the write gate; this store is unusable")
		}
		withReceiverPlanLockWriter(t, s, func(ctx context.Context, conn *sql.Conn) {
			if _, err := conn.ExecContext(ctx, `DELETE FROM sqlite_stat1 WHERE idx = 'nodes_go_receiver_type'`); err != nil {
				t.Fatalf("clear receiver stat row: %v", err)
			}
			if _, err := conn.ExecContext(ctx, `INSERT INTO sqlite_stat1(tbl, idx, stat) VALUES ('nodes', 'nodes_go_receiver_type', '0 0 0 0 0')`); err != nil {
				t.Fatalf("poison receiver stat row: %v", err)
			}
			if _, err := conn.ExecContext(ctx, `ANALYZE sqlite_schema`); err != nil {
				t.Fatalf("reload poisoned statistics: %v", err)
			}
			// The frontier table is connection-local, so provision it on the
			// connection the EXPLAIN runs on, and fill it: an empty frontier
			// is not the shape production plans against.
			if _, err := conn.ExecContext(ctx, goMethodReceiverFileTableSQL); err != nil {
				t.Fatalf("create frontier table: %v", err)
			}
			for j := 0; j < receiverPlanLockFrontierFiles; j++ {
				if _, err := conn.ExecContext(ctx, `INSERT OR IGNORE INTO temp.go_receiver_rebind_files(file_path) VALUES (?)`, receiverPlanLockMethodFile(j)); err != nil {
					t.Fatalf("seed frontier table: %v", err)
				}
			}

			plan := explainOnConn(t, ctx, conn, goMethodReceiverCandidatesForFilesSQL,
				baseViewGeneration, baseViewGeneration, baseViewGeneration, baseViewGeneration)
			joined := strings.Join(plan, "\n")
			probed := false
			for _, line := range plan {
				if strings.HasPrefix(trimPlanLine(line), "SCAN c") {
					t.Errorf("batch: receiver type table drives the join (O(types x member_of)):\n%s", joined)
				}
				if strings.Contains(line, "SEARCH c USING INDEX nodes_go_receiver_type") {
					probed = true
				}
			}
			if !probed {
				t.Errorf("batch: receiver type table is not probed through nodes_go_receiver_type:\n%s", joined)
			}
		})
	})

	// The regime the cooperative runtime refresh actually produces: a MIXED
	// one. A pass analyzes its work list one index per gate hold and can defer
	// at any point — a busy writer, a spent budget, a per-index timeout — so a
	// store spends real time with some critical indexes freshly analyzed and
	// others still carrying whatever row they had, including the poisoned zero
	// one. The four regimes above are all uniform, and a plan that is only
	// correct when every row agrees would be a plan this mechanism breaks.
	//
	// So: refresh nodes_by_file alone (exactly what one hold of the
	// cooperative loop does) while the receiver row stays poisoned at zero, and
	// require the same seek-driven shape from both queries.
	t.Run("mixed_partial_refresh", func(t *testing.T) {
		if rebindAbandoned.Load() {
			t.Skip("previous subtest abandoned a rebind still holding the write gate; this store is unusable")
		}
		withReceiverPlanLockWriter(t, s, func(ctx context.Context, conn *sql.Conn) {
			// Re-establish the poisoned row rather than inheriting it: the
			// subtest above may have been skipped, and a regime that is only
			// under test when its predecessor ran is not under test.
			if _, err := conn.ExecContext(ctx, `DELETE FROM sqlite_stat1 WHERE idx = 'nodes_go_receiver_type'`); err != nil {
				t.Fatalf("clear receiver stat row: %v", err)
			}
			if _, err := conn.ExecContext(ctx, `INSERT INTO sqlite_stat1(tbl, idx, stat) VALUES ('nodes', 'nodes_go_receiver_type', '0 0 0 0 0')`); err != nil {
				t.Fatalf("poison receiver stat row: %v", err)
			}
			if _, err := conn.ExecContext(ctx, `ANALYZE sqlite_schema`); err != nil {
				t.Fatalf("reload poisoned statistics: %v", err)
			}

			// One index, on the connection the plans are read from — the unit
			// of work the cooperative refresh holds the write gate for.
			hasStatTable, err := preparePlannerStatsConn(ctx, conn)
			if err != nil {
				t.Fatalf("prepare the statistics connection: %v", err)
			}
			if _, err := analyzePlannerStatsIndexOnConn(ctx, conn, "nodes_by_file", hasStatTable); err != nil {
				t.Fatalf("analyze nodes_by_file: %v", err)
			}

			// The mix is the point: one index re-analyzed over the real corpus,
			// the receiver row still zero. Assert both halves, or a future
			// change that made ANALYZE of one index rewrite its neighbours
			// would leave this running the `refreshed` regime again.
			if got := receiverStatOnConn(t, ctx, conn); got != "0 0 0 0 0" {
				t.Fatalf("receiver stat = %q after analyzing nodes_by_file alone, want the poisoned zero row "+
					"left untouched — the mixed regime is not under test", got)
			}
			var refreshed sql.NullString
			if err := conn.QueryRowContext(ctx, `SELECT stat FROM sqlite_stat1 WHERE idx = 'nodes_by_file'`).Scan(&refreshed); err != nil {
				t.Fatalf("read nodes_by_file stat: %v", err)
			}
			if !refreshed.Valid || statRowCount(t, refreshed.String) < receiverPlanLockMethods {
				t.Fatalf("nodes_by_file stat = %q, want a row describing the whole corpus", refreshed.String)
			}

			assertReceiverRebindPlansLocked(t, ctx, conn, methodFile)
		})
	})

	// An honest but stale row: ANALYZE captured the index while exactly one Go
	// type existed, and the other 299 landed afterwards. Nothing about this
	// store is corrupt — the planner is simply working from an old count.
	//
	// This regime needs its own store: the corpus must be seeded with a single
	// type so ANALYZE writes a genuinely tiny row, which no amount of
	// sqlite_stat1 editing on the shared store would reproduce honestly.
	t.Run("tiny_row_natural", func(t *testing.T) {
		tiny, methodFile, _ := newReceiverPlanLockStore(t, receiverPlanLockOneType)
		refreshReceiverPlanLockStats(t, tiny)
		// Assert the regime rather than reporting it: a fixture that quietly
		// stopped producing a tiny row would leave this subtest running the
		// same healthy-statistics plan as `refreshed` while still passing.
		stat := receiverStatOnReader(t, tiny)
		if stat == "" {
			t.Fatal("single-type fixture wrote no receiver stat row; the tiny-row regime is not under test")
		}
		if got := statRowCount(t, stat); got > 3 {
			t.Fatalf("natural single-type receiver stat = %q (count %d), want <= 3 — the tiny-row regime is not under test",
				stat, got)
		}
		addReceiverPlanLockRemainingTypes(tiny)

		withReceiverPlanLockWriter(t, tiny, func(ctx context.Context, conn *sql.Conn) {
			assertReceiverRebindPlansLocked(t, ctx, conn, methodFile)
		})
	})
}

// withReceiverPlanLockWriter runs fn against the store's writer connection —
// the one RebindGoMethodReceivers itself uses, and the only one that sees a
// writer-side ANALYZE.
//
// The gate and the connection are released with defer, and that is load
// bearing: t.Fatalf unwinds through runtime.Goexit, which runs this frame's
// defers but would otherwise leave writeMu held. Store.Close takes the same
// gate, so a plan assertion that failed while holding it would deadlock the
// t.Cleanup that closes the store and turn a one-line failure into a package
// timeout.
func withReceiverPlanLockWriter(t *testing.T, s *Store, fn func(ctx context.Context, conn *sql.Conn)) {
	t.Helper()
	ctx := context.Background()
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	conn, release, err := s.activeWriteConnLocked(ctx)
	if err != nil {
		t.Fatalf("acquire writer connection: %v", err)
	}
	defer release()
	fn(ctx, conn)
}

// assertReceiverRebindPlansLocked runs both production candidate queries on the
// writer connection — the connection RebindGoMethodReceivers itself uses, and
// the only one that sees writer-side ANALYZE — and locks the join order.
func assertReceiverRebindPlansLocked(t *testing.T, ctx context.Context, conn *sql.Conn, methodFile string) {
	t.Helper()

	globalPlan := explainOnConn(t, ctx, conn, goMethodReceiverCandidatesGlobalSQL,
		baseViewGeneration, baseViewGeneration, baseViewGeneration, baseViewGeneration)
	assertPlanShape(t, "global", globalPlan, "e USING INDEX edges_by_kind",
		// `c` must be reached as a probe through its partial index, not
		// merely "not scanned": an autoindex or a covering-key probe would
		// pass the negative assertion while abandoning the vetted path.
		[]string{"SEARCH c USING INDEX nodes_go_receiver_type"},
		[]string{"USE TEMP B-TREE"})

	// The scoped sibling binds (view_gen, view_gen, view_gen, file_path, view_gen).
	filePlan := explainOnConn(t, ctx, conn, goMethodReceiverCandidatesForFileSQL,
		baseViewGeneration, baseViewGeneration, baseViewGeneration, methodFile, baseViewGeneration)
	// A scoped GROUP BY over one file's member edges may legitimately
	// materialise a temp B-tree: it is bounded by that single file.
	assertPlanShape(t, "scoped", filePlan, "m USING INDEX nodes_by_file",
		// The file-scoped method set reaches member_of through edges_by_from;
		// losing that seek is what would turn the streaming tail back into
		// O(files * all_methods).
		[]string{
			"SEARCH e USING INDEX edges_by_from",
			"SEARCH c USING INDEX nodes_go_receiver_type",
		},
		nil)
}

// assertPlanShape requires the outermost loop (EXPLAIN QUERY PLAN emits rows in
// loop-nesting order, so row 0 is the outer driver) to be the vetted seek,
// requires every `want` substring somewhere in the joined plan (the
// plan_lock_test.go convention), and forbids the receiver type table from ever
// becoming a driving scan.
//
// It reports with Errorf rather than Fatalf so a regression prints BOTH the
// global and the scoped plan in one run: the two queries fail together and
// seeing only the first would hide half the diagnosis.
func assertPlanShape(t *testing.T, label string, plan []string, wantOuter string, want []string, forbid []string) {
	t.Helper()
	joined := strings.Join(plan, "\n")
	if len(plan) == 0 {
		t.Errorf("%s: empty query plan", label)
		return
	}
	if !strings.Contains(plan[0], wantOuter) {
		t.Errorf("%s: outermost loop = %q, want it to contain %q\nfull plan:\n%s", label, plan[0], wantOuter, joined)
	}
	for _, wanted := range want {
		if !strings.Contains(joined, wanted) {
			t.Errorf("%s: plan missing %q:\n%s", label, wanted, joined)
		}
	}
	for _, line := range plan {
		if strings.HasPrefix(trimPlanLine(line), "SCAN c") {
			t.Errorf("%s: receiver type table drives the join (O(types x member_of)):\n%s", label, joined)
			break
		}
	}
	for _, forbidden := range forbid {
		for _, line := range plan {
			if strings.Contains(line, forbidden) {
				t.Errorf("%s: plan contains forbidden %q:\n%s", label, forbidden, joined)
				break
			}
		}
	}
}

// trimPlanLine strips the tree-drawing prefix some SQLite builds prepend to
// EXPLAIN QUERY PLAN details so a prefix match sees the opcode itself.
func trimPlanLine(line string) string {
	return strings.TrimLeft(line, " \t|-`+")
}

// explainOnConn runs EXPLAIN QUERY PLAN on an already-held writer connection.
// Never issue s.writerDB.Exec / QueryRow while holding one: the writer pool is
// limited to a single connection and would deadlock.
func explainOnConn(t *testing.T, ctx context.Context, conn *sql.Conn, query string, args ...any) []string {
	t.Helper()
	rows, err := conn.QueryContext(ctx, "EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatalf("explain query plan: %v", err)
	}
	var plan []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			_ = rows.Close()
			t.Fatalf("scan plan row: %v", err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		t.Fatalf("iterate plan rows: %v", err)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close plan rows: %v", err)
	}
	return plan
}

const (
	receiverPlanLockRepo    = "repo"
	receiverPlanLockDirs    = 100
	receiverPlanLockTypes   = 300
	receiverPlanLockMethods = 3000
	receiverPlanLockFanout  = 7
	// Every hundredth member_of edge points at a type node that does not
	// exist; those are exactly the rows the rebind pass must repair.
	receiverPlanLockPhantomEdges = receiverPlanLockMethods / 100
	// Budget for the functional rebind on the poisoned store. The misplan it
	// guards against is O(types x member_of) — minutes on this fixture, hours
	// on a real store — so the bound only has to be far below that while
	// staying well clear of a loaded or -race CI box. Two minutes still
	// discriminates by more than an order of magnitude against a misplan the
	// healthy plan finishes in well under a second, and the ubuntu CI legs are
	// slow enough that a tighter bound buys nothing but flakes. Raise this
	// constant if it ever flakes; do NOT shrink the fixture, a smaller one
	// stops reproducing the misplan at all.
	receiverPlanLockRebindBudget = 2 * time.Minute
	// Frontier files handed to the batched sibling's TEMP table.
	receiverPlanLockFrontierFiles = 200
)

type receiverPlanLockTypeSeed int

const (
	receiverPlanLockAllTypes receiverPlanLockTypeSeed = iota
	receiverPlanLockOneType
)

func receiverPlanLockDir(typeIndex int) string {
	return fmt.Sprintf("%s/pkg/d%02d", receiverPlanLockRepo, typeIndex%receiverPlanLockDirs)
}

func receiverPlanLockTypeName(typeIndex int) string {
	return fmt.Sprintf("T%03d", typeIndex)
}

func receiverPlanLockTypeNode(typeIndex int) *graph.Node {
	dir := receiverPlanLockDir(typeIndex)
	name := receiverPlanLockTypeName(typeIndex)
	file := dir + "/types.go"
	return &graph.Node{
		ID:         file + "::" + name,
		Name:       name,
		Kind:       graph.KindType,
		FilePath:   file,
		Language:   "go",
		RepoPrefix: receiverPlanLockRepo,
		StartLine:  typeIndex + 1,
		EndLine:    typeIndex + 4,
	}
}

func receiverPlanLockMethodFile(methodIndex int) string {
	return fmt.Sprintf("%s/m%04d.go", receiverPlanLockDir(methodIndex%receiverPlanLockTypes), methodIndex)
}

// newReceiverPlanLockStore builds a production-shaped Go workspace: many
// packages, receiver types split across their package's types.go, methods in
// their own files, a sprinkling of phantom receiver targets, and a dense call
// graph so edges_by_kind is genuinely selective for 'member_of'.
//
// It returns the store, one method file path for the scoped query's bind, and
// a disarm func for the cleanup close. Disarming is for one situation only: a
// query left running on the write gate. Store.Close takes that gate, so the
// cleanup would block until the package timeout instead of letting the already
// recorded failure print. Leaking the handle costs a Windows TempDir removal
// error on a run that has already failed; a deadlock costs the whole report.
func newReceiverPlanLockStore(t *testing.T, seed receiverPlanLockTypeSeed) (*Store, string, func()) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "receiver_plan_lock.sqlite"))
	if err != nil {
		t.Fatalf("open receiver plan-lock store: %v", err)
	}
	var abandoned atomic.Bool
	// Registered after t.TempDir() so LIFO cleanup closes the database before
	// the directory is removed — Windows cannot unlink an open file.
	t.Cleanup(func() {
		if abandoned.Load() {
			return
		}
		_ = s.Close()
	})
	disarm := func() { abandoned.Store(true) }

	var nodes []*graph.Node
	if seed == receiverPlanLockAllTypes {
		for i := 0; i < receiverPlanLockTypes; i++ {
			nodes = append(nodes, receiverPlanLockTypeNode(i))
		}
	} else {
		nodes = append(nodes, receiverPlanLockTypeNode(0))
	}

	var edges []*graph.Edge
	for j := 0; j < receiverPlanLockMethods; j++ {
		typeIndex := j % receiverPlanLockTypes
		dir := receiverPlanLockDir(typeIndex)
		typeName := receiverPlanLockTypeName(typeIndex)
		file := receiverPlanLockMethodFile(j)
		methodName := fmt.Sprintf("M%04d", j)
		methodID := file + "::" + typeName + "." + methodName
		nodes = append(nodes, &graph.Node{
			ID:         methodID,
			Name:       methodName,
			Kind:       graph.KindMethod,
			FilePath:   file,
			Language:   "go",
			RepoPrefix: receiverPlanLockRepo,
			StartLine:  1,
			EndLine:    9,
		})
		target := dir + "/types.go::" + typeName
		if j%100 == 0 {
			target = dir + "/phantom.go::" + typeName
		}
		edges = append(edges, &graph.Edge{
			From:     methodID,
			To:       target,
			Kind:     graph.EdgeMemberOf,
			FilePath: file,
			Line:     1,
		})
	}
	// Dense call edges so 'member_of' is a small slice of edges_by_kind: a
	// fixture where every edge is a member edge would make the wrong plan look
	// cheap for the wrong reason.
	for j := 0; j < receiverPlanLockMethods; j++ {
		file := receiverPlanLockMethodFile(j)
		typeName := receiverPlanLockTypeName(j % receiverPlanLockTypes)
		from := file + "::" + typeName + "." + fmt.Sprintf("M%04d", j)
		for n := 1; n <= receiverPlanLockFanout; n++ {
			callee := (j + n*37) % receiverPlanLockMethods
			calleeType := receiverPlanLockTypeName(callee % receiverPlanLockTypes)
			edges = append(edges, &graph.Edge{
				From:     from,
				To:       receiverPlanLockMethodFile(callee) + "::" + calleeType + "." + fmt.Sprintf("M%04d", callee),
				Kind:     graph.EdgeCalls,
				FilePath: file,
				Line:     10 + n,
			})
		}
	}
	s.AddBatch(nodes, edges)

	wantTypes := receiverPlanLockTypes
	if seed != receiverPlanLockAllTypes {
		wantTypes = 1
	}
	assertReceiverPlanLockFixtureLanded(t, s, wantTypes)
	return s, receiverPlanLockMethodFile(1), disarm
}

// assertReceiverPlanLockFixtureLanded pins the corpus these plan locks are
// measured against. Everything below depends on the two cardinalities the
// misplan is a product of, and both are silent when wrong: a corpus that
// landed twice (or half) still EXPLAINs, just against a shape nobody chose.
// The receiver count is read THROUGH the partial index with the index's own
// predicate, so it also proves the index the plans probe is populated.
func assertReceiverPlanLockFixtureLanded(t *testing.T, s *Store, wantTypes int) {
	t.Helper()
	var types int
	if err := s.db.QueryRow(`SELECT count(*) FROM nodes INDEXED BY nodes_go_receiver_type WHERE ` +
		nodesGoReceiverTypePredicate).Scan(&types); err != nil {
		t.Fatalf("count receiver index entries: %v", err)
	}
	if types != wantTypes {
		t.Fatalf("nodes_go_receiver_type holds %d entries, want %d receiver types", types, wantTypes)
	}
	var memberEdges int
	if err := s.db.QueryRow(`SELECT count(*) FROM edges WHERE kind = 'member_of'`).Scan(&memberEdges); err != nil {
		t.Fatalf("count member_of edges: %v", err)
	}
	if memberEdges != receiverPlanLockMethods {
		t.Fatalf("fixture holds %d member_of edges, want %d (one per method)", memberEdges, receiverPlanLockMethods)
	}
}

// addReceiverPlanLockRemainingTypes lands the 299 receiver types that were
// deliberately withheld from the initial batch, leaving the freshly written
// sqlite_stat1 row honestly describing a one-row index.
func addReceiverPlanLockRemainingTypes(s *Store) {
	var nodes []*graph.Node
	for i := 1; i < receiverPlanLockTypes; i++ {
		nodes = append(nodes, receiverPlanLockTypeNode(i))
	}
	s.AddBatch(nodes, nil)
}

func refreshReceiverPlanLockStats(t *testing.T, s *Store) {
	t.Helper()
	s.writeMu.Lock()
	err := s.refreshPlannerStatsLocked(context.Background())
	s.writeMu.Unlock()
	if err != nil {
		t.Fatalf("refresh planner stats: %v", err)
	}
}

func statsTableExists(t *testing.T, s *Store) bool {
	t.Helper()
	var exists bool
	if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'sqlite_stat1')`).Scan(&exists); err != nil {
		t.Fatalf("probe sqlite_stat1: %v", err)
	}
	return exists
}

func receiverStatOnConn(t *testing.T, ctx context.Context, conn *sql.Conn) string {
	t.Helper()
	var stat sql.NullString
	err := conn.QueryRowContext(ctx, `SELECT stat FROM sqlite_stat1 WHERE idx = 'nodes_go_receiver_type'`).Scan(&stat)
	if err == sql.ErrNoRows {
		return ""
	}
	if err != nil {
		t.Fatalf("read receiver stat: %v", err)
	}
	return stat.String
}

func receiverStatOnReader(t *testing.T, s *Store) string {
	t.Helper()
	var stat sql.NullString
	err := s.db.QueryRow(`SELECT stat FROM sqlite_stat1 WHERE idx = 'nodes_go_receiver_type'`).Scan(&stat)
	if err == sql.ErrNoRows {
		return ""
	}
	if err != nil {
		t.Fatalf("read receiver stat: %v", err)
	}
	return stat.String
}
