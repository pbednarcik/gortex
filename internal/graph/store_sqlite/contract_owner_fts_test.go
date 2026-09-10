package store_sqlite_test

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

const (
	backendFTSCanonical = "env::BACKEND_FTS_SHARED"
	backendFTSFileA     = "repo-a/provider.go"
	backendFTSFileB     = "repo-b/consumer.go"
	backendFTSSourceA   = backendFTSFileA + "::Provide"
	backendFTSSourceB   = backendFTSFileB + "::Consume"
	backendFTSUnrelated = "repo-c/other.go::Unrelated"
)

// Compare complete stored rows, including encoded metadata and FTS rowids.
func backendFTSRows(t *testing.T, db *sql.DB, query string, args ...any) map[string]int {
	t.Helper()
	rows, err := db.Query(query, args...)
	require.NoError(t, err)
	defer rows.Close()
	columns, err := rows.Columns()
	require.NoError(t, err)
	result := make(map[string]int)
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		require.NoError(t, rows.Scan(pointers...))
		for i, value := range values {
			if blob, ok := value.([]byte); ok {
				// Preserve the SQL value type and distinguish an empty BLOB
				// from NULL or a text cell with the same base64 representation.
				values[i] = struct{ Blob []byte }{Blob: append([]byte{}, blob...)}
			}
		}
		encoded, err := json.Marshal(values)
		require.NoError(t, err)
		result[string(encoded)]++
	}
	require.NoError(t, rows.Err())
	return result
}

type backendFTSState struct {
	mapping map[string]int
	virtual map[string]int
	rowIDs  []int64
}

func backendFTSForNode(t *testing.T, db *sql.DB, generation int64, id string) backendFTSState {
	t.Helper()
	state := backendFTSState{
		mapping: backendFTSRows(t, db, "SELECT * FROM symbol_fts_rowid WHERE view_gen = ? AND node_id = ?", generation, id),
		virtual: backendFTSRows(t, db, "SELECT f.rowid, f.* FROM symbol_fts f JOIN symbol_fts_rowid m ON f.rowid = m.fts_rowid WHERE m.view_gen = ? AND m.node_id = ?", generation, id),
	}
	rows, err := db.Query("SELECT fts_rowid FROM symbol_fts_rowid WHERE view_gen = ? AND node_id = ?", generation, id)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var rowID int64
		require.NoError(t, rows.Scan(&rowID))
		state.rowIDs = append(state.rowIDs, rowID)
	}
	require.NoError(t, rows.Err())
	return state
}

func backendFTSAssertGone(t *testing.T, db *sql.DB, generation int64, id string, before backendFTSState) {
	t.Helper()
	assert.Empty(t, backendFTSForNode(t, db, generation, id).mapping, "deleted canonical must have no rowid mapping")
	// Do not check only the join: deleting a mapping while leaking its virtual
	// row would make a joined absence assertion falsely pass.
	for _, rowID := range before.rowIDs {
		assert.Empty(t, backendFTSRows(t, db, "SELECT rowid, * FROM symbol_fts WHERE rowid = ?", rowID), "deleted canonical must have no orphaned virtual FTS row")
	}
}

type backendFTSFixture struct {
	path  string
	store *store_sqlite.Store
	db    *sql.DB
}

func newBackendFTSFixture(t *testing.T) *backendFTSFixture {
	t.Helper()
	f := &backendFTSFixture{path: filepath.Join(t.TempDir(), "graph.sqlite")}
	f.open(t)
	t.Cleanup(func() { f.close(t) })
	return f
}

func (f *backendFTSFixture) open(t *testing.T) {
	t.Helper()
	var err error
	f.store, err = store_sqlite.Open(f.path)
	require.NoError(t, err)
	f.db, err = sql.Open("sqlite", f.path)
	require.NoError(t, err)
}

func (f *backendFTSFixture) close(t *testing.T) {
	t.Helper()
	if f.db != nil {
		require.NoError(t, f.db.Close())
		f.db = nil
	}
	if f.store != nil {
		require.NoError(t, f.store.Close())
		f.store = nil
	}
}

func (f *backendFTSFixture) reopen(t *testing.T) {
	t.Helper()
	f.close(t)
	f.open(t)
}

func (f *backendFTSFixture) seed(t *testing.T, tag string, canonicalFTS bool) {
	t.Helper()
	f.store.AddBatch([]*graph.Node{
		{ID: backendFTSSourceA, Name: "Provide", Kind: graph.KindFunction, Language: "go", RepoPrefix: "repo-a", FilePath: backendFTSFileA, WorkspaceID: tag + "-wa", ProjectID: tag + "-pa", Meta: map[string]any{"fixture": tag, "nested": map[string]any{"role": "provider"}}},
		{ID: backendFTSSourceB, Name: "Consume", Kind: graph.KindFunction, Language: "go", RepoPrefix: "repo-b", FilePath: backendFTSFileB, WorkspaceID: tag + "-wb", ProjectID: tag + "-pb", Meta: map[string]any{"fixture": tag, "nested": map[string]any{"role": "consumer"}}},
		{ID: backendFTSUnrelated, Name: "Unrelated", Kind: graph.KindFunction, Language: "go", RepoPrefix: "repo-c", FilePath: "repo-c/other.go", Meta: map[string]any{"fixture": tag, "unrelated": true}},
		{ID: backendFTSCanonical, Name: backendFTSCanonical, Kind: graph.KindContract, Language: "contract", RepoPrefix: "repo-a", FilePath: backendFTSFileA, WorkspaceID: tag + "-wa", ProjectID: tag + "-pa", Meta: map[string]any{"type": "env", "role": "provider", "contract_owner_record": true, "fixture": tag, "contract_meta": map[string]any{"var": "BACKEND_FTS_SHARED", "tag": tag}}},
	}, []*graph.Edge{
		{From: backendFTSSourceA, To: backendFTSCanonical, Kind: graph.EdgeProvides, FilePath: backendFTSFileA, Line: 11, Meta: map[string]any{"contract_owner_repo_prefix": "repo-a", "contract_owner_workspace": tag + "-wa", "contract_owner_project": tag + "-pa", "contract_owner_type": "env", "contract_owner_meta": map[string]any{"tag": tag, "role": "provider"}}},
		{From: backendFTSSourceB, To: backendFTSCanonical, Kind: graph.EdgeConsumes, FilePath: backendFTSFileB, Line: 23, Meta: map[string]any{"contract_owner_repo_prefix": "repo-b", "contract_owner_workspace": tag + "-wb", "contract_owner_project": tag + "-pb", "contract_owner_type": "env", "contract_owner_meta": map[string]any{"tag": tag, "role": "consumer"}}},
	})
	items := []graph.SymbolFTSItem{
		{NodeID: backendFTSSourceA, Tokens: tag + " ordinary provider"},
		{NodeID: backendFTSSourceB, Tokens: tag + " ordinary consumer"},
		{NodeID: backendFTSUnrelated, Tokens: tag + " ordinary unrelated"},
	}
	if canonicalFTS {
		items = append(items, graph.SymbolFTSItem{NodeID: backendFTSCanonical, Tokens: tag + " canonical shared env"})
	}
	// A separate actual indexer.populateSymbolFTS regression proves contract
	// admission. Here the public writer sets up positive backend rows directly.
	batcher, ok := any(f.store).(graph.SymbolFTSBatchUpserter)
	require.True(t, ok)
	require.NoError(t, batcher.BatchUpsertSymbolFTS(items))
	require.Len(t, backendFTSForNode(t, f.db, 0, backendFTSSourceA).virtual, 1)
	require.Len(t, backendFTSForNode(t, f.db, 0, backendFTSSourceB).virtual, 1)
	canonical := backendFTSForNode(t, f.db, 0, backendFTSCanonical)
	if canonicalFTS {
		require.Len(t, canonical.virtual, 1)
		require.Len(t, canonical.mapping, 1)
	} else {
		require.Empty(t, canonical.virtual)
		require.Empty(t, canonical.mapping)
	}
	require.Len(t, f.store.GetInEdges(backendFTSCanonical), 2)
}

func (f *backendFTSFixture) remove(t *testing.T, mode, repo, file string) {
	t.Helper()
	switch mode {
	case "raw_file_eviction":
		f.store.EvictFiles([]string{file})
	case "owner_replacement":
		_, err := graph.ReplaceContractOwners(f.store, graph.ContractOwnerReplacement{RepoPrefix: repo, FilePaths: []string{file}, TouchedNodeIDs: []string{backendFTSCanonical}})
		require.NoError(t, err)
	default:
		t.Fatalf("unknown fixture operation %q", mode)
	}
}

func TestContractFTSBackendSharedOwnerLifecycle(t *testing.T) {
	for _, mode := range []string{"raw_file_eviction", "owner_replacement"} {
		for _, withFTS := range []bool{true, false} {
			name := mode + "/positive_fts"
			if !withFTS {
				name = mode + "/missing_fts"
			}
			t.Run(name, func(t *testing.T) {
				f := newBackendFTSFixture(t)
				f.seed(t, "selected", withFTS)
				canonical := backendFTSForNode(t, f.db, 0, backendFTSCanonical)
				ordinary := map[string]backendFTSState{}
				for _, id := range []string{backendFTSSourceA, backendFTSSourceB, backendFTSUnrelated} {
					ordinary[id] = backendFTSForNode(t, f.db, 0, id)
				}
				bNode := backendFTSRows(t, f.db, "SELECT * FROM nodes WHERE view_gen = 0 AND id = ?", backendFTSSourceB)
				bEdges := backendFTSRows(t, f.db, "SELECT * FROM edges WHERE view_gen = 0 AND from_id = ?", backendFTSSourceB)
				require.Len(t, bNode, 1)
				require.Len(t, bEdges, 1)
				f.remove(t, mode, "repo-a", backendFTSFileA)
				if mode == "raw_file_eviction" {
					assert.Nil(t, f.store.GetNode(backendFTSSourceA), "the raw source-file mutation actually ran")
				} else {
					assert.NotNil(t, f.store.GetNode(backendFTSSourceA), "owner replacement does not evict ordinary source nodes")
				}
				assert.NotNil(t, f.store.GetNode(backendFTSCanonical), "B keeps the shared canonical alive")
				assert.Equal(t, canonical, backendFTSForNode(t, f.db, 0, backendFTSCanonical), "retained canonical FTS payload and rowid must be untouched")
				assert.Equal(t, bNode, backendFTSRows(t, f.db, "SELECT * FROM nodes WHERE view_gen = 0 AND id = ?", backendFTSSourceB))
				assert.Equal(t, bEdges, backendFTSRows(t, f.db, "SELECT * FROM edges WHERE view_gen = 0 AND from_id = ?", backendFTSSourceB), "B ownership metadata must not be rewritten")
				assert.Empty(t, backendFTSRows(t, f.db, "SELECT * FROM edges WHERE view_gen = 0 AND from_id = ? AND to_id = ?", backendFTSSourceA, backendFTSCanonical))
				f.reopen(t)
				assert.NotNil(t, f.store.GetNode(backendFTSCanonical), "retention must be durable")
				assert.Equal(t, canonical, backendFTSForNode(t, f.db, 0, backendFTSCanonical))
				f.remove(t, mode, "repo-b", backendFTSFileB)
				if mode == "raw_file_eviction" {
					assert.Nil(t, f.store.GetNode(backendFTSSourceB))
				} else {
					assert.NotNil(t, f.store.GetNode(backendFTSSourceB))
				}
				assert.Nil(t, f.store.GetNode(backendFTSCanonical), "final owner removal prunes the off-file canonical")
				assert.Empty(t, backendFTSRows(t, f.db, "SELECT * FROM edges WHERE view_gen = 0 AND (from_id = ? OR to_id = ?)", backendFTSCanonical, backendFTSCanonical))
				backendFTSAssertGone(t, f.db, 0, backendFTSCanonical, canonical)
				for id, before := range ordinary {
					assert.Equal(t, before, backendFTSForNode(t, f.db, 0, id), "raw backend operations must preserve preexisting ordinary FTS behavior for %s", id)
				}
				assert.NotNil(t, f.store.GetNode(backendFTSUnrelated))
				f.reopen(t)
				assert.Nil(t, f.store.GetNode(backendFTSCanonical))
				backendFTSAssertGone(t, f.db, 0, backendFTSCanonical, canonical)
			})
		}
	}
}

func TestContractFTSBackendOutsideEndpointRecordFileScope(t *testing.T) {
	f := newBackendFTSFixture(t)
	const id = "env::BACKEND_FTS_RECORD_ONLY"
	const source = "repo-b/live.go::Owner"
	const recordFile = "repo-b/removed-record.env"
	f.store.AddBatch([]*graph.Node{
		{ID: source, Name: "Owner", Kind: graph.KindFunction, RepoPrefix: "repo-b", FilePath: "repo-b/live.go"},
		{ID: id, Name: id, Kind: graph.KindContract, RepoPrefix: "repo-c", FilePath: "repo-c/canonical.go", Meta: map[string]any{"contract_owner_record": true, "type": "env", "role": "provider"}},
	}, []*graph.Edge{{From: source, To: id, Kind: graph.EdgeProvides, FilePath: recordFile, Line: 31, Meta: map[string]any{"contract_owner_repo_prefix": "repo-b", "contract_owner_type": "env", "contract_owner_meta": map[string]any{"fixture": "outside-endpoints"}}}})
	batcher, ok := any(f.store).(graph.SymbolFTSBatchUpserter)
	require.True(t, ok)
	require.NoError(t, batcher.BatchUpsertSymbolFTS([]graph.SymbolFTSItem{{NodeID: id, Tokens: "canonical record only"}, {NodeID: source, Tokens: "ordinary outside source"}}))
	canonical := backendFTSForNode(t, f.db, 0, id)
	ordinary := backendFTSForNode(t, f.db, 0, source)
	require.Len(t, canonical.virtual, 1)
	require.Len(t, ordinary.virtual, 1)
	nodes := backendFTSRows(t, f.db, "SELECT * FROM nodes WHERE view_gen = 0")
	edges := backendFTSRows(t, f.db, "SELECT * FROM edges WHERE view_gen = 0")
	require.Len(t, nodes, 2)
	require.Len(t, edges, 1)
	removedNodes, removedEdges := f.store.EvictFiles([]string{recordFile})
	assert.Zero(t, removedNodes)
	assert.Zero(t, removedEdges)
	assert.Equal(t, nodes, backendFTSRows(t, f.db, "SELECT * FROM nodes WHERE view_gen = 0"), "raw node-file frontier must not expand to record-file-only endpoints")
	assert.Equal(t, edges, backendFTSRows(t, f.db, "SELECT * FROM edges WHERE view_gen = 0"))
	assert.Equal(t, canonical, backendFTSForNode(t, f.db, 0, id))
	_, err := graph.ReplaceContractOwners(f.store, graph.ContractOwnerReplacement{RepoPrefix: "repo-b", FilePaths: []string{recordFile}, TouchedNodeIDs: []string{id}})
	require.NoError(t, err)
	assert.Nil(t, f.store.GetNode(id), "record lifecycle can prune the final owner-backed scalar even when both endpoints are off-file")
	backendFTSAssertGone(t, f.db, 0, id, canonical)
	assert.NotNil(t, f.store.GetNode(source))
	assert.Equal(t, ordinary, backendFTSForNode(t, f.db, 0, source))
	f.reopen(t)
	assert.Nil(t, f.store.GetNode(id))
	backendFTSAssertGone(t, f.db, 0, id, canonical)
}

type backendFTSGenerationState struct {
	nodes, edges, mapping, virtual map[string]int
}

func backendFTSGeneration(t *testing.T, db *sql.DB, generation int64) backendFTSGenerationState {
	t.Helper()
	return backendFTSGenerationState{
		nodes:   backendFTSRows(t, db, "SELECT * FROM nodes WHERE view_gen = ?", generation),
		edges:   backendFTSRows(t, db, "SELECT * FROM edges WHERE view_gen = ?", generation),
		mapping: backendFTSRows(t, db, "SELECT * FROM symbol_fts_rowid WHERE view_gen = ?", generation),
		virtual: backendFTSRows(t, db, "SELECT f.rowid, f.* FROM symbol_fts f JOIN symbol_fts_rowid m ON f.rowid = m.fts_rowid WHERE m.view_gen = ?", generation),
	}
}

func TestContractFTSBackendLifecyclePreservesSiblingGeneration(t *testing.T) {
	for _, mode := range []string{"raw_file_eviction", "owner_replacement"} {
		t.Run(mode, func(t *testing.T) {
			f := newBackendFTSFixture(t)
			f.seed(t, "historical-seven", true)
			f.close(t)
			// Only this closed disposable fixture is modified. Keep virtual rowids
			// fixed; ownership lives in symbol_fts_rowid, not a virtual-table gen column.
			db, err := sql.Open("sqlite", f.path)
			require.NoError(t, err)
			tx, err := db.Begin()
			require.NoError(t, err)
			for _, query := range []string{"UPDATE nodes SET view_gen = 7", "UPDATE edges SET view_gen = 7", "UPDATE symbol_fts_rowid SET view_gen = 7"} {
				_, err = tx.Exec(query)
				require.NoError(t, err)
			}
			require.NoError(t, tx.Commit())
			historical := backendFTSGeneration(t, db, 7)
			require.Len(t, historical.nodes, 4)
			require.Len(t, historical.edges, 2)
			require.Len(t, historical.mapping, 4)
			require.Len(t, historical.virtual, 4)
			require.NoError(t, db.Close())
			f.open(t)
			f.seed(t, "selected-zero", true)
			assert.Equal(t, historical, backendFTSGeneration(t, f.db, 7), "seeding identical selected IDs must preserve distinct historical payloads")
			selected := backendFTSForNode(t, f.db, 0, backendFTSCanonical)
			historicalCanonical := backendFTSForNode(t, f.db, 7, backendFTSCanonical)
			require.NotEqual(t, selected.virtual, historicalCanonical.virtual, "generation fixture must distinguish exact FTS payloads and rowids")
			f.remove(t, mode, "repo-a", backendFTSFileA)
			assert.NotNil(t, f.store.GetNode(backendFTSCanonical))
			assert.Equal(t, selected, backendFTSForNode(t, f.db, 0, backendFTSCanonical))
			assert.Equal(t, historical, backendFTSGeneration(t, f.db, 7), "retained phase must not mutate any sibling node/edge/FTS/map column")
			f.reopen(t)
			f.remove(t, mode, "repo-b", backendFTSFileB)
			assert.Nil(t, f.store.GetNode(backendFTSCanonical))
			backendFTSAssertGone(t, f.db, 0, backendFTSCanonical, selected)
			assert.Equal(t, historical, backendFTSGeneration(t, f.db, 7), "final selected prune must not delete same-ID sibling FTS or graph rows")
			f.reopen(t)
			backendFTSAssertGone(t, f.db, 0, backendFTSCanonical, selected)
			assert.Equal(t, historical, backendFTSGeneration(t, f.db, 7), "generation isolation must survive reopen")
		})
	}
}

func TestContractFTSBackendMutationRollsBackOnFixtureAbort(t *testing.T) {
	for _, mode := range []string{"raw_file_eviction", "owner_replacement"} {
		for _, boundary := range []string{"canonical_node", "fts_mapping"} {
			t.Run(mode+"/"+boundary, func(t *testing.T) {
				f := newBackendFTSFixture(t)
				f.seed(t, "rollback", true)
				// Use the public owner API to leave exactly A as the final owner.
				// B's ordinary source/FTS remain positive unaffected controls.
				_, err := graph.ReplaceContractOwners(f.store, graph.ContractOwnerReplacement{
					RepoPrefix: "repo-b", FilePaths: []string{backendFTSFileB}, TouchedNodeIDs: []string{backendFTSCanonical},
				})
				require.NoError(t, err)
				require.NotNil(t, f.store.GetNode(backendFTSCanonical))
				require.Len(t, f.store.GetInEdges(backendFTSCanonical), 1)
				f.close(t)
				db, err := sql.Open("sqlite", f.path)
				require.NoError(t, err)
				// Fault injection is a fixture-only SQLite trigger, not a new
				// production hook/private API. FTS virtual tables cannot have
				// triggers; the ordinary rowid map is a supported later boundary
				// after virtual-row deletion in the shared cleanup helper.
				trigger := `CREATE TRIGGER contract_fts_fixture_abort BEFORE DELETE ON nodes
WHEN OLD.id = 'env::BACKEND_FTS_SHARED' AND OLD.view_gen = 0
BEGIN SELECT RAISE(ABORT, 'contract_fts_fixture_abort'); END`
				if boundary == "fts_mapping" {
					trigger = `CREATE TRIGGER contract_fts_fixture_abort BEFORE DELETE ON symbol_fts_rowid
WHEN OLD.node_id = 'env::BACKEND_FTS_SHARED' AND OLD.view_gen = 0
BEGIN SELECT RAISE(ABORT, 'contract_fts_fixture_abort'); END`
				}
				_, err = db.Exec(trigger)
				require.NoError(t, err)
				require.NoError(t, db.Close())
				f.open(t)
				before := backendFTSGeneration(t, f.db, 0)
				virtualBefore := backendFTSRows(t, f.db, "SELECT rowid, * FROM symbol_fts")
				canonicalBefore := backendFTSForNode(t, f.db, 0, backendFTSCanonical)
				require.Len(t, before.nodes, 4)
				require.Len(t, before.edges, 1)
				require.Len(t, canonicalBefore.mapping, 1)
				require.Len(t, canonicalBefore.virtual, 1)
				if mode == "raw_file_eviction" {
					// Actual public EvictFiles routes this SQLite failure through
					// panicOnFatal. Preserve that existing error surface; recover
					// only to inspect durable rollback, not to mask another panic.
					var recovered any
					func() {
						defer func() { recovered = recover() }()
						f.store.EvictFiles([]string{backendFTSFileA})
					}()
					require.NotNil(t, recovered, "the installed failure boundary must actually be reached")
					require.Contains(t, fmt.Sprint(recovered), "contract_fts_fixture_abort")
				} else {
					_, err = graph.ReplaceContractOwners(f.store, graph.ContractOwnerReplacement{
						RepoPrefix: "repo-a", FilePaths: []string{backendFTSFileA}, TouchedNodeIDs: []string{backendFTSCanonical},
					})
					require.ErrorContains(t, err, "contract_fts_fixture_abort", "the installed failure boundary must actually be reached")
				}
				require.Equal(t, before, backendFTSGeneration(t, f.db, 0), "failed mutation must roll back every selected node/edge/FTS/map column")
				require.Equal(t, virtualBefore, backendFTSRows(t, f.db, "SELECT rowid, * FROM symbol_fts"), "rollback must restore deleted virtual rows, not merely their mappings")
				require.NotNil(t, f.store.GetNode(backendFTSCanonical))
				f.reopen(t)
				require.Equal(t, before, backendFTSGeneration(t, f.db, 0), "rollback must remain intact after reopen")
				require.Equal(t, virtualBefore, backendFTSRows(t, f.db, "SELECT rowid, * FROM symbol_fts"))
				_, err = f.db.Exec("DROP TRIGGER contract_fts_fixture_abort")
				require.NoError(t, err)
				// Positive retry rules out an empty frontier/no-op masquerading
				// as rollback after the expected public error/panic surface.
				f.remove(t, mode, "repo-a", backendFTSFileA)
				assert.Nil(t, f.store.GetNode(backendFTSCanonical))
				backendFTSAssertGone(t, f.db, 0, backendFTSCanonical, canonicalBefore)
				assert.NotNil(t, f.store.GetNode(backendFTSSourceB))
				assert.NotNil(t, f.store.GetNode(backendFTSUnrelated))
			})
		}
	}
}

const (
	ownerFTSOrderF     = "repo/f.go::FTSOrderF"
	ownerFTSOrderA     = "env::FTS_OWNER_PRUNE_A"
	ownerFTSOrderB     = "env::FTS_OWNER_PRUNE_B"
	ownerFTSOrderFileF = "repo/f.go"
)

func seedOwnerFTSPruneOrder(t *testing.T, f *backendFTSFixture, tag string) {
	t.Helper()
	f.store.AddBatch([]*graph.Node{
		{ID: ownerFTSOrderF, Name: "F", Kind: graph.KindFunction, RepoPrefix: "repo", FilePath: ownerFTSOrderFileF, Meta: map[string]any{"fixture": tag}},
		{ID: ownerFTSOrderA, Name: "A", Kind: graph.KindContract, RepoPrefix: "repo", FilePath: "repo/a.go", WorkspaceID: tag + "-wa", ProjectID: tag + "-pa", Meta: map[string]any{"contract_owner_record": true, "type": "env", "role": "provider", "contract_meta": map[string]any{"var": "FTS_OWNER_PRUNE_A", "fixture": tag}}},
		{ID: ownerFTSOrderB, Name: "B", Kind: graph.KindContract, RepoPrefix: "repo", FilePath: "repo/b.go", WorkspaceID: tag + "-wb", ProjectID: tag + "-pb", Meta: map[string]any{"contract_owner_record": true, "type": "env", "role": "consumer", "contract_meta": map[string]any{"var": "FTS_OWNER_PRUNE_B", "fixture": tag}}},
	}, []*graph.Edge{
		{From: ownerFTSOrderF, To: ownerFTSOrderA, Kind: graph.EdgeProvides, FilePath: ownerFTSOrderFileF, Line: 11, Meta: map[string]any{"contract_owner_repo_prefix": "repo", "contract_owner_type": "env", "contract_owner_meta": map[string]any{"record": "F owns A", "fixture": tag}}},
		{From: ownerFTSOrderA, To: ownerFTSOrderB, Kind: graph.EdgeConsumes, FilePath: "repo/b.go", Line: 23, Meta: map[string]any{"contract_owner_repo_prefix": "repo", "contract_owner_type": "env", "contract_owner_meta": map[string]any{"record": "A owns B", "fixture": tag}}},
	})
	batcher, ok := any(f.store).(graph.SymbolFTSBatchUpserter)
	require.True(t, ok)
	require.NoError(t, batcher.BatchUpsertSymbolFTS([]graph.SymbolFTSItem{
		{NodeID: ownerFTSOrderF, Tokens: tag + " ordinary F"},
		{NodeID: ownerFTSOrderA, Tokens: tag + " canonical A"},
		{NodeID: ownerFTSOrderB, Tokens: tag + " canonical B"},
	}))
	require.Equal(t, 3, f.store.NodeCount())
	require.Equal(t, 2, f.store.EdgeCount())
	require.Len(t, f.store.GetInEdges(ownerFTSOrderA), 1)
	require.Len(t, f.store.GetInEdges(ownerFTSOrderB), 1)
	for _, id := range []string{ownerFTSOrderF, ownerFTSOrderA, ownerFTSOrderB} {
		state := backendFTSForNode(t, f.db, 0, id)
		require.Len(t, state.mapping, 1)
		require.Len(t, state.virtual, 1)
	}
}

func TestContractFTSOwnerPruningPreservesFinalNodeSet(t *testing.T) {
	for _, boundary := range []string{"success", "final_B_node_abort", "final_B_fts_mapping_abort"} {
		t.Run(boundary, func(t *testing.T) {
			f := newBackendFTSFixture(t)
			seedOwnerFTSPruneOrder(t, f, "sibling-seven")
			f.close(t)
			// Fixture-only generation setup on a closed disposable Store, using
			// the already-established exact-column generation move. FTS virtual
			// rowids stay fixed; the generation belongs to their rowid mapping.
			db, err := sql.Open("sqlite", f.path)
			require.NoError(t, err)
			tx, err := db.Begin()
			require.NoError(t, err)
			for _, query := range []string{"UPDATE nodes SET view_gen = 7", "UPDATE edges SET view_gen = 7", "UPDATE symbol_fts_rowid SET view_gen = 7"} {
				_, err = tx.Exec(query)
				require.NoError(t, err)
			}
			require.NoError(t, tx.Commit())
			historical := backendFTSGeneration(t, db, 7)
			require.Len(t, historical.nodes, 3)
			require.Len(t, historical.edges, 2)
			require.Len(t, historical.mapping, 3)
			require.Len(t, historical.virtual, 3)
			require.NoError(t, db.Close())
			f.open(t)
			seedOwnerFTSPruneOrder(t, f, "selected-zero")
			require.Equal(t, historical, backendFTSGeneration(t, f.db, 7))
			before := backendFTSGeneration(t, f.db, 0)
			virtualBefore := backendFTSRows(t, f.db, "SELECT rowid, * FROM symbol_fts")
			ordinaryNode := backendFTSRows(t, f.db, "SELECT * FROM nodes WHERE id = ? AND view_gen = 0", ownerFTSOrderF)
			ordinaryFTS := backendFTSForNode(t, f.db, 0, ownerFTSOrderF)
			canonicalFTS := make(map[string]backendFTSState)
			for _, id := range []string{ownerFTSOrderA, ownerFTSOrderB} {
				canonicalFTS[id] = backendFTSForNode(t, f.db, 0, id)
				require.NotEqual(t, canonicalFTS[id].virtual, backendFTSForNode(t, f.db, 7, id).virtual, "same-ID generations must have distinct positive FTS payloads and rowids")
			}
			replace := func() (graph.ContractOwnerReplaceResult, error) {
				return graph.ReplaceContractOwners(f.store, graph.ContractOwnerReplacement{
					RepoPrefix: "repo", FilePaths: []string{ownerFTSOrderFileF}, TouchedNodeIDs: []string{ownerFTSOrderA, ownerFTSOrderB},
				})
			}
			if boundary != "success" {
				f.close(t)
				db, err = sql.Open("sqlite", f.path)
				require.NoError(t, err)
				trigger := `CREATE TRIGGER owner_fts_prune_order_abort BEFORE DELETE ON nodes
WHEN OLD.id = 'env::FTS_OWNER_PRUNE_B' AND OLD.view_gen = 0
BEGIN SELECT RAISE(ABORT, 'owner_fts_prune_order_abort'); END`
				if boundary == "final_B_fts_mapping_abort" {
					trigger = `CREATE TRIGGER owner_fts_prune_order_abort BEFORE DELETE ON symbol_fts_rowid
WHEN OLD.node_id = 'env::FTS_OWNER_PRUNE_B' AND OLD.view_gen = 0
BEGIN SELECT RAISE(ABORT, 'owner_fts_prune_order_abort'); END`
				}
				_, err = db.Exec(trigger)
				require.NoError(t, err)
				require.NoError(t, db.Close())
				f.open(t)
				_, err = replace()
				require.ErrorContains(t, err, "owner_fts_prune_order_abort", "the final B boundary must actually be reached; initial-A-only pruning is not equivalent")
				require.Equal(t, before, backendFTSGeneration(t, f.db, 0), "both contract/FTS rows and earlier owner-edge deletion must roll back")
				require.Equal(t, virtualBefore, backendFTSRows(t, f.db, "SELECT rowid, * FROM symbol_fts"), "rollback restores exact virtual rows, not only their maps")
				require.Equal(t, historical, backendFTSGeneration(t, f.db, 7))
				f.reopen(t)
				require.Equal(t, before, backendFTSGeneration(t, f.db, 0))
				require.Equal(t, virtualBefore, backendFTSRows(t, f.db, "SELECT rowid, * FROM symbol_fts"))
				require.Equal(t, historical, backendFTSGeneration(t, f.db, 7))
				_, err = f.db.Exec("DROP TRIGGER owner_fts_prune_order_abort")
				require.NoError(t, err)
			}
			result, err := replace()
			require.NoError(t, err)
			require.Equal(t, 2, result.NodesRemoved, "preserve SQLite's existing final orphan re-evaluation: remove A AND B")
			require.Equal(t, 2, result.EdgesRemoved)
			require.Equal(t, 1, f.store.NodeCount())
			require.Zero(t, f.store.EdgeCount())
			for _, id := range []string{ownerFTSOrderA, ownerFTSOrderB} {
				require.Nil(t, f.store.GetNode(id))
				backendFTSAssertGone(t, f.db, 0, id, canonicalFTS[id])
			}
			assert.Equal(t, ordinaryNode, backendFTSRows(t, f.db, "SELECT * FROM nodes WHERE id = ? AND view_gen = 0", ownerFTSOrderF))
			assert.Equal(t, ordinaryFTS, backendFTSForNode(t, f.db, 0, ownerFTSOrderF))
			assert.Equal(t, historical, backendFTSGeneration(t, f.db, 7), "every sibling node/edge/map/virtual column and rowid remains unchanged")
			f.reopen(t)
			for _, id := range []string{ownerFTSOrderA, ownerFTSOrderB} {
				require.Nil(t, f.store.GetNode(id))
				backendFTSAssertGone(t, f.db, 0, id, canonicalFTS[id])
			}
			assert.Equal(t, ordinaryFTS, backendFTSForNode(t, f.db, 0, ownerFTSOrderF))
			assert.Equal(t, historical, backendFTSGeneration(t, f.db, 7))
		})
	}
}
