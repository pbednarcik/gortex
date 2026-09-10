package store_sqlite_test

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func sqliteContractFileStore(t *testing.T) *store_sqlite.Store {
	t.Helper()
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	return store
}

func sqliteContractFileNode(id, path, repo string) *graph.Node {
	return &graph.Node{ID: id, Name: id, Kind: graph.KindFunction, FilePath: path, RepoPrefix: repo}
}

func sqliteContractFileCanonical(id, path, repo string, backed, removed bool) *graph.Node {
	return &graph.Node{ID: id, Name: id, Kind: graph.KindContract, FilePath: path, RepoPrefix: repo, Meta: map[string]any{
		"type": "env", "role": "provider", "contract_owner_record": backed, "contract_owner_removed": removed,
		"contract_meta": map[string]any{"nested": map[string]any{"values": []any{"preserve", "payload"}}},
	}}
}

func sqliteContractFileEdge(source *graph.Node, id, path string) *graph.Edge {
	return &graph.Edge{From: source.ID, To: id, Kind: graph.EdgeProvides, FilePath: path, Line: 7, Meta: map[string]any{
		"contract_owner_repo_prefix": source.RepoPrefix, "contract_owner_type": "env",
		"contract_owner_meta": map[string]any{"source": source.ID, "nested": []any{"exact", "payload"}},
	}}
}

func sqliteContractFileEdges(t *testing.T, store graph.Store, source string) string {
	t.Helper()
	var rows []struct {
		Edge *graph.Edge
		Meta map[string]any
	}
	for _, edge := range store.GetOutEdges(source) {
		rows = append(rows, struct {
			Edge *graph.Edge
			Meta map[string]any
		}{edge, edge.Meta})
	}
	encoded, err := json.Marshal(rows)
	require.NoError(t, err)
	return string(encoded)
}

func TestSQLiteFileEvictionRetainsSharedContractAndPrunesLastOwner(t *testing.T) {
	for _, batch := range []bool{false, true} {
		for _, legacy := range []bool{false, true} {
			t.Run(fmt.Sprintf("batch=%t/legacy=%t", batch, legacy), func(t *testing.T) {
				store := sqliteContractFileStore(t)
				const id, fileA, fileB = "env::FILE_RETAIN", "a/owner.go", "b/owner.go"
				a, b := sqliteContractFileNode("a::Owner", fileA, "a"), sqliteContractFileNode("b::Owner", fileB, "b")
				canonical := sqliteContractFileCanonical(id, fileA, "a", !legacy, false)
				store.AddBatch([]*graph.Node{a, b, canonical}, []*graph.Edge{sqliteContractFileEdge(a, id, fileA), sqliteContractFileEdge(b, id, fileB)})
				before := sqliteContractFileEdges(t, store, b.ID)
				evict := func(path string) (int, int) {
					if batch {
						return store.EvictFiles([]string{path, path})
					}
					return store.EvictFile(path)
				}
				token := store.BeginMutationReceipt()
				nodes, edges := evict(fileA)
				receipt := store.EndMutationReceipt(token)
				assert.Equal(t, 1, nodes)
				assert.Equal(t, 1, edges)
				assert.Nil(t, store.GetNode(a.ID))
				assert.NotNil(t, store.GetNode(b.ID))
				current := store.GetNode(id)
				if assert.NotNil(t, current) {
					assert.Equal(t, canonical.Meta["contract_meta"], current.Meta["contract_meta"])
					if legacy {
						assert.Equal(t, true, current.Meta["contract_owner_removed"])
						assert.False(t, receipt.Complete)
					}
				}
				assert.Equal(t, before, sqliteContractFileEdges(t, store, b.ID))
				nodes, edges = evict(fileB)
				assert.Equal(t, 2, nodes, "last owner pruning follows canonical ID outside B's file bucket")
				assert.Equal(t, 1, edges)
				assert.Nil(t, store.GetNode(id))
				assert.Empty(t, store.GetInEdges(id))
				assert.Empty(t, store.GetOutEdges(id))
			})
		}
	}
}

func TestSQLiteFileEvictionPreservesOnlyLiveOffFrontierLegacyScalar(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		backed, removed, keep bool
	}{
		{name: "legacy", keep: true},
		{name: "owner_backed", backed: true},
		{name: "already_removed", removed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := sqliteContractFileStore(t)
			const id, fileA, fileB = "env::OFF_FILE", "a/owner.go", "b/scalar.env"
			a := sqliteContractFileNode("a::Owner", fileA, "a")
			canonical := sqliteContractFileCanonical(id, fileB, "b", tc.backed, tc.removed)
			store.AddBatch([]*graph.Node{a, canonical}, []*graph.Edge{sqliteContractFileEdge(a, id, fileA)})
			store.EvictFiles([]string{fileA})
			if tc.keep {
				assert.NotNil(t, store.GetNode(id))
			} else {
				assert.Nil(t, store.GetNode(id), "a removed or owner-backed scalar cannot keep an orphan alive")
			}
			store.EvictFiles([]string{fileB})
			assert.Nil(t, store.GetNode(id))
			assert.Empty(t, store.GetInEdges(id))
		})
	}
}

func TestSQLiteFileEvictionKeepsFileOnlyOwnershipOutsideNodeFrontier(t *testing.T) {
	for _, touched := range []bool{false, true} {
		t.Run(fmt.Sprintf("touched=%t", touched), func(t *testing.T) {
			store := sqliteContractFileStore(t)
			const id, fileA, fileC = "env::BOUNDED_FRONTIER", "a/routes.go", "c/owner.go"
			a := sqliteContractFileNode("a::Source", fileA, "a")
			b := sqliteContractFileNode("a::Handler", "a/handler.go", "a")
			c := sqliteContractFileNode("c::Owner", fileC, "c")
			path := fileC
			if touched {
				path = fileA
			}
			store.AddBatch([]*graph.Node{a, b, c, sqliteContractFileCanonical(id, path, "a", true, false)}, []*graph.Edge{sqliteContractFileEdge(b, id, fileA), sqliteContractFileEdge(c, id, fileC)})
			beforeB, beforeC := sqliteContractFileEdges(t, store, b.ID), sqliteContractFileEdges(t, store, c.ID)
			store.EvictFiles([]string{fileA})
			assert.NotNil(t, store.GetNode(id))
			assert.Equal(t, beforeC, sqliteContractFileEdges(t, store, c.ID))
			if touched {
				assert.Empty(t, store.GetOutEdges(b.ID), "removed file's incident owner must not survive a retained canonical")
			} else {
				assert.Equal(t, beforeB, sqliteContractFileEdges(t, store, b.ID), "both endpoints were outside old raw eviction; replacement owns later cleanup")
			}
		})
	}
}

func TestSQLiteFileEvictionLargeContractFrontierUsesBoundedBindings(t *testing.T) {
	store := sqliteContractFileStore(t)
	const count, fileA, fileB = 2049, "a/many.go", "b/many.go"
	a, b := sqliteContractFileNode("a::Many", fileA, "a"), sqliteContractFileNode("b::Many", fileB, "b")
	nodes := []*graph.Node{a, b}
	edges := make([]*graph.Edge, 0, count*2)
	ids := make([]string, 0, count)
	for i := range count {
		id := fmt.Sprintf("env::MANY_%04d", i)
		ids = append(ids, id)
		nodes = append(nodes, sqliteContractFileCanonical(id, fileA, "a", true, false))
		edges = append(edges, sqliteContractFileEdge(a, id, fileA), sqliteContractFileEdge(b, id, fileB))
	}
	store.AddBatch(nodes, edges)
	require.Len(t, store.GetOutEdges(a.ID), count)
	require.Len(t, store.GetOutEdges(b.ID), count)
	removedNodes, removedEdges := store.EvictFiles([]string{fileA})
	assert.Equal(t, 1, removedNodes)
	assert.Equal(t, count, removedEdges)
	assert.Len(t, store.GetNodesByIDs(ids), count)
	assert.Len(t, store.GetOutEdges(b.ID), count)
	removedNodes, removedEdges = store.EvictFiles([]string{fileB})
	assert.Equal(t, count+1, removedNodes)
	assert.Equal(t, count, removedEdges)
	assert.Empty(t, store.GetNodesByIDs(ids))
}

func TestSQLiteFileEvictionContractOwnerClosure(t *testing.T) {
	for _, initial := range []bool{false, true} {
		for _, legacyC := range []bool{false, true} {
			t.Run(fmt.Sprintf("initial_target=%t/legacy_c=%t", initial, legacyC), func(t *testing.T) {
				store := sqliteContractFileStore(t)
				a := sqliteContractFileNode("a::Owner", "a/owner.go", "a")
				b := sqliteContractFileCanonical("env::CHAIN_B", "b/record.env", "b", true, false)
				c := sqliteContractFileCanonical("env::CHAIN_C", "c/record.env", "c", !legacyC, false)
				edges := []*graph.Edge{sqliteContractFileEdge(a, b.ID, a.FilePath), sqliteContractFileEdge(b, c.ID, b.FilePath)}
				if initial {
					edges = append(edges, sqliteContractFileEdge(a, c.ID, a.FilePath))
				}
				store.AddBatch([]*graph.Node{a, b, c}, edges)
				require.Len(t, store.GetOutEdges(b.ID), 1, "legacy canonical-source ownership is durably admitted")
				store.EvictFiles([]string{a.FilePath})
				assert.Nil(t, store.GetNode(b.ID))
				if legacyC {
					assert.NotNil(t, store.GetNode(c.ID), "independent recoverable scalar C survives")
				} else {
					assert.Nil(t, store.GetNode(c.ID), "doomed B cannot count as a live source owner for C")
				}
				assert.Empty(t, store.GetInEdges(c.ID))
				assert.Empty(t, store.GetOutEdges(b.ID))
			})
		}
	}
}

func TestSQLiteFileEvictionOwnerRepoMetadataMatchesDecoder(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
		keep  bool
	}{
		{name: "non_string_falls_back", value: 42, keep: true},
		{name: "explicit_empty_is_authoritative", value: "", keep: false},
		{name: "wrong_repo_is_authoritative", value: "wrong", keep: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := sqliteContractFileStore(t)
			const id = "env::REPO_METADATA"
			a := sqliteContractFileNode("a::Owner", "a/owner.go", "a")
			b := sqliteContractFileNode("b::Owner", "b/owner.go", "b")
			edgeB := sqliteContractFileEdge(b, id, b.FilePath)
			edgeB.Meta["contract_owner_repo_prefix"] = tc.value
			store.AddBatch([]*graph.Node{a, b, sqliteContractFileCanonical(id, a.FilePath, "a", true, false)}, []*graph.Edge{sqliteContractFileEdge(a, id, a.FilePath), edgeB})
			require.Len(t, store.GetOutEdges(b.ID), 1)
			store.EvictFiles([]string{a.FilePath})
			if tc.keep {
				assert.NotNil(t, store.GetNode(id))
			} else {
				assert.Nil(t, store.GetNode(id))
			}
		})
	}
}

func sqliteContractFileHistoricalRows(t *testing.T, db *sql.DB) map[string][]string {
	t.Helper()
	result := make(map[string][]string)
	for _, query := range []struct{ table, order string }{
		{"nodes", "id"},
		{"edges", "from_id, to_id, kind, file_path, line"},
	} {
		rows, err := db.Query("SELECT * FROM " + query.table + " WHERE view_gen = 7 ORDER BY " + query.order)
		require.NoError(t, err)
		columns, err := rows.Columns()
		require.NoError(t, err)
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			require.NoError(t, rows.Scan(pointers...))
			encoded, err := json.Marshal(values)
			require.NoError(t, err)
			result[query.table] = append(result[query.table], string(encoded))
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
	}
	return result
}

func TestSQLiteFileEvictionPreservesExactSiblingGenerationRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.sqlite")
	store, err := store_sqlite.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() {
		if store != nil {
			require.NoError(t, store.Close())
		}
	})
	const id, fileA, fileB = "env::FILE_GENERATION", "a/owner.go", "b/owner.go"
	seed := func(version string) {
		a, b := sqliteContractFileNode("a::Owner", fileA, "a"), sqliteContractFileNode("b::Owner", fileB, "b")
		canonical := sqliteContractFileCanonical(id, fileA, "a", false, false)
		canonical.Meta["fixture_version"] = version
		aEdge, bEdge := sqliteContractFileEdge(a, id, fileA), sqliteContractFileEdge(b, id, fileB)
		aEdge.Meta["fixture_version"], bEdge.Meta["fixture_version"] = version, version
		store.AddBatch([]*graph.Node{a, b, canonical}, []*graph.Edge{aEdge, bEdge})
	}
	seed("historical-seven")
	require.NoError(t, store.Close())
	store = nil
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	_, err = db.Exec("UPDATE nodes SET view_gen = 7")
	require.NoError(t, err)
	_, err = db.Exec("UPDATE edges SET view_gen = 7")
	require.NoError(t, err)
	before := sqliteContractFileHistoricalRows(t, db)
	require.Len(t, before["nodes"], 3)
	require.Len(t, before["edges"], 2)
	require.NoError(t, db.Close())
	store, err = store_sqlite.Open(path)
	require.NoError(t, err)
	seed("current-zero")
	store.EvictFiles([]string{fileA})
	if current := store.GetNode(id); assert.NotNil(t, current) {
		assert.Equal(t, "current-zero", current.Meta["fixture_version"])
		assert.Equal(t, true, current.Meta["contract_owner_removed"])
	}
	assert.Len(t, store.GetOutEdges("b::Owner"), 1)
	require.NoError(t, store.Close())
	store = nil
	db, err = sql.Open("sqlite", path)
	require.NoError(t, err)
	assert.Equal(t, before, sqliteContractFileHistoricalRows(t, db), "all historical columns and full binary node/edge payloads remain unchanged")
	require.NoError(t, db.Close())
	store, err = store_sqlite.Open(path)
	require.NoError(t, err)
	store.EvictFiles([]string{fileB})
	assert.Nil(t, store.GetNode(id))
	assert.Empty(t, store.GetInEdges(id))
}

func TestSQLiteContractFileEvictionQueryPlans(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.sqlite")
	store, err := store_sqlite.Open(path)
	require.NoError(t, err)
	const id, fileA, fileB = "env::QUERY_PLAN", "a/owner.go", "b/owner.go"
	a, b := sqliteContractFileNode("a::Owner", fileA, "a"), sqliteContractFileNode("b::Owner", fileB, "b")
	store.AddBatch([]*graph.Node{a, b, sqliteContractFileCanonical(id, fileA, "a", true, false)}, []*graph.Edge{sqliteContractFileEdge(a, id, fileA), sqliteContractFileEdge(b, id, fileB)})
	// Exercise the installed implementation, not only copied query text. The
	// access shapes below match its guarded, source-reviewed SQL; they are not
	// a claim that EXPLAIN captures statements dynamically from the operation.
	removed, _ := store.EvictFiles([]string{fileA})
	require.Equal(t, 1, removed)
	require.Nil(t, store.GetNode(a.ID))
	require.NotNil(t, store.GetNode(b.ID))
	require.NotNil(t, store.GetNode(id))
	require.Len(t, store.GetInEdges(id), 1)
	require.NoError(t, store.Close())
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	files, err := json.Marshal([]string{fileA})
	require.NoError(t, err)
	ids, err := json.Marshal([]string{id})
	require.NoError(t, err)
	queries := []struct {
		name, sql  string
		args       []any
		mustSearch []string
	}{
		{"affected_contracts", `SELECT * FROM nodes WHERE view_gen = ? AND kind = ? AND id IN (
SELECT id FROM nodes WHERE file_path IN (SELECT CAST(value AS TEXT) FROM json_each(?)) AND view_gen = ?
UNION
SELECT to_id FROM edges WHERE from_id IN (SELECT id FROM nodes WHERE file_path IN (SELECT CAST(value AS TEXT) FROM json_each(?)) AND view_gen = ?)
AND view_gen = ? AND kind IN (?, ?, ?))`,
			[]any{int64(0), string(graph.KindContract), string(files), int64(0), string(files), int64(0), int64(0), string(graph.EdgeProvides), string(graph.EdgeConsumes), string(graph.EdgeHandlesRoute)},
			[]string{"nodes", "edges"}},
		{"surviving_owners", `SELECT owner.to_id, owner.from_id, source.repo_prefix, owner.meta FROM edges AS owner
JOIN nodes AS source ON source.id = owner.from_id AND source.view_gen = ?
WHERE owner.to_id IN (SELECT CAST(value AS TEXT) FROM json_each(?))
AND owner.view_gen = ? AND owner.kind IN (?, ?, ?)
AND source.file_path NOT IN (SELECT CAST(value AS TEXT) FROM json_each(?))
AND owner.file_path NOT IN (SELECT CAST(value AS TEXT) FROM json_each(?))`,
			[]any{int64(0), string(ids), int64(0), string(graph.EdgeProvides), string(graph.EdgeConsumes), string(graph.EdgeHandlesRoute), string(files), string(files)},
			[]string{"owner", "source"}},
		{"incident_closure_ids", `SELECT DISTINCT to_id FROM edges
WHERE from_id IN (SELECT CAST(value AS TEXT) FROM json_each(?))
AND view_gen = ? AND kind IN (?, ?, ?)`,
			[]any{string(ids), int64(0), string(graph.EdgeProvides), string(graph.EdgeConsumes), string(graph.EdgeHandlesRoute)},
			[]string{"edges"}},
		{"unseen_contract_payloads", `SELECT * FROM nodes WHERE view_gen = ? AND kind = ?
AND id IN (SELECT CAST(value AS TEXT) FROM json_each(?))`,
			[]any{int64(0), string(graph.KindContract), string(ids)},
			[]string{"nodes"}},
	}
	nodeScoped := `((file_path IN (SELECT CAST(value AS TEXT) FROM json_each(?)) AND view_gen = ?) AND kind <> ?) OR (id IN (SELECT CAST(value AS TEXT) FROM json_each(?)) AND kind = ? AND view_gen = ?)`
	for _, column := range []string{"from_id", "to_id"} {
		queries = append(queries, struct {
			name, sql  string
			args       []any
			mustSearch []string
		}{
			"delete_" + column,
			`DELETE FROM edges WHERE ` + column + ` IN (SELECT id FROM nodes WHERE ` + nodeScoped + `) AND view_gen = ?`,
			[]any{string(files), int64(0), string(graph.KindContract), string(ids), string(graph.KindContract), int64(0), int64(0)},
			[]string{"edges", "nodes"},
		})
	}
	for _, query := range queries {
		t.Run(query.name, func(t *testing.T) {
			rows, err := db.Query("EXPLAIN QUERY PLAN "+query.sql, query.args...)
			require.NoError(t, err)
			searches := map[string]bool{}
			for rows.Next() {
				var id, parent, unused int
				var detail string
				require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
				t.Log(detail)
				fields := strings.Fields(detail)
				if len(fields) >= 2 {
					if fields[0] == "SEARCH" {
						searches[fields[1]] = true
					}
					if fields[0] == "SCAN" {
						require.NotContains(t, []string{"nodes", "edges", "owner", "source"}, fields[1], "no whole ordinary-node/edge scan")
					}
				}
			}
			require.NoError(t, rows.Err())
			require.NoError(t, rows.Close())
			for _, table := range query.mustSearch {
				require.True(t, searches[table], "expected indexed lookup for "+table)
			}
		})
	}
}
