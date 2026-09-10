package indexer

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"go.uber.org/zap"
)

// Serialize the full record, including metadata and ownership, and count
// multiplicity. An ID-keyed map would hide the exact regression under test.
func bridgeRoundtripRecordMultiset(t *testing.T, records []contracts.Contract) map[string]int {
	t.Helper()
	set := make(map[string]int, len(records))
	for _, c := range records {
		encoded, err := json.Marshal(c)
		require.NoError(t, err)
		set[string(encoded)]++
	}
	return set
}

func bridgeRoundtripMatches(t *testing.T, reg *contracts.Registry) map[string]int {
	t.Helper()
	set := make(map[string]int)
	result := contracts.Match(reg)
	for _, match := range result.Matched {
		// Keep both entire endpoint records: a shared ID does not identify
		// either role, source file, symbol, or metadata payload by itself.
		encoded, err := json.Marshal(struct {
			Provider  contracts.Contract
			Consumer  contracts.Contract
			CrossRepo bool
		}{match.Provider, match.Consumer, match.CrossRepo})
		require.NoError(t, err)
		set[string(encoded)]++
	}
	return set
}

func TestBridgeRegistryRoundtripPreservesSharedIDRecords(t *testing.T) {
	factories := map[string]func(*testing.T) graph.Store{
		"memory": func(t *testing.T) graph.Store { return graph.New() },
		"sqlite": func(t *testing.T) graph.Store {
			store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			return store
		},
	}
	for backend, factory := range factories {
		for _, kind := range []string{"env", "graphql"} {
			for _, shared := range []bool{false, true} {
				name := "unique_ids"
				if shared {
					name = "shared_id"
				}
				t.Run(backend+"/"+kind+"/"+name, func(t *testing.T) {
					store := factory(t)
					idx := New(store, newTestRegistry(), config.IndexConfig{}, zap.NewNop())
					idx.SetRepoPrefix("roundtrip")
					idx.SetWorkspaceID("workspace")
					idx.SetProjectID("project")
					idx.storeRootPath(t.TempDir())
					t.Cleanup(func() { idx.Close() })
					providerID, consumerID := "env::ROUNDTRIP_PROVIDER", "env::ROUNDTRIP_CONSUMER"
					providerMeta := map[string]any{"var": "ROUNDTRIP_PROVIDER"}
					consumerMeta := map[string]any{"var": "ROUNDTRIP_CONSUMER"}
					if kind == "graphql" {
						providerID, consumerID = "graphql::Query::Provider", "graphql::Query::Consumer"
						providerMeta = map[string]any{"operation": "Query", "field": "Provider"}
						consumerMeta = map[string]any{"operation": "Query", "field": "Consumer"}
					}
					if shared {
						consumerID, consumerMeta = providerID, providerMeta
					}
					provider := contracts.Contract{
						ID: providerID, Type: contracts.ContractType(kind), Role: contracts.Role("provider"),
						RepoPrefix: "roundtrip", WorkspaceID: "workspace", ProjectID: "project",
						FilePath: "roundtrip/provider.go", SymbolID: "roundtrip/provider.go::Provide", Line: 11,
						Confidence: 1, Meta: providerMeta,
					}
					consumer := contracts.Contract{
						ID: consumerID, Type: contracts.ContractType(kind), Role: contracts.Role("consumer"),
						RepoPrefix: "roundtrip", WorkspaceID: "workspace", ProjectID: "project",
						FilePath: "roundtrip/consumer.go", SymbolID: "roundtrip/consumer.go::Consume", Line: 22,
						Confidence: 1, Meta: consumerMeta,
					}
					original := contracts.NewRegistry()
					original.Add(provider)
					original.Add(consumer)
					require.Len(t, original.All(), 2, "the in-memory registry preserves both distinct roles")
					beforeRecords := bridgeRoundtripRecordMultiset(t, original.All())
					beforeMatches := bridgeRoundtripMatches(t, original)
					if shared {
						require.Len(t, beforeMatches, 1, "shared-ID fixture must establish a real initial match")
					} else {
						require.Empty(t, beforeMatches, "unique-ID control intentionally has no matching pair")
					}

					store.AddBatch([]*graph.Node{
						{ID: provider.SymbolID, Kind: graph.KindFunction, RepoPrefix: "roundtrip", FilePath: provider.FilePath, WorkspaceID: "workspace", ProjectID: "project"},
						{ID: consumer.SymbolID, Kind: graph.KindFunction, RepoPrefix: "roundtrip", FilePath: consumer.FilePath, WorkspaceID: "workspace", ProjectID: "project"},
					}, nil)
					idx.commitContracts(original)
					require.Equal(t, beforeRecords, bridgeRoundtripRecordMultiset(t, original.All()), "cold in-memory records remain intact")
					var persistedRoles []any
					for _, n := range store.GetRepoNodes("roundtrip") {
						if n.Kind == graph.KindContract {
							persistedRoles = append(persistedRoles, n.Meta["role"])
						}
					}
					loaded := contracts.LoadRegistryFromGraph(store, "roundtrip")
					require.NotNil(t, loaded)
					ownerIndexer := New(store, newTestRegistry(), config.IndexConfig{}, zap.NewNop())
					ownerIndexer.SetRepoPrefix("roundtrip")
					ownerIndexer.SetWorkspaceID("workspace")
					ownerIndexer.SetProjectID("project")
					t.Cleanup(func() { ownerIndexer.Close() })
					ownerRecovered := ownerIndexer.ensureIncrementalContractRegistry()
					require.Equal(t, beforeRecords, bridgeRoundtripRecordMultiset(t, ownerRecovered.All()), "existing owner edges recover both symbol-backed records")
					require.Equal(t, beforeMatches, bridgeRoundtripMatches(t, ownerRecovered), "existing owner-edge recovery preserves matches for this fixture")
					t.Logf("cold_records=%d stored_contract_nodes=%d stored_roles=%v warm_records=%d cold_matches=%d warm_matches=%d owner_records=%d owner_matches=%d",
						len(original.All()), len(persistedRoles), persistedRoles, len(loaded.All()), len(beforeMatches), len(bridgeRoundtripMatches(t, loaded)), len(ownerRecovered.All()), len(bridgeRoundtripMatches(t, ownerRecovered)))
					assert.Equal(t, beforeRecords, bridgeRoundtripRecordMultiset(t, loaded.All()), "all full records and their multiplicities must survive persistence/hydration")
					assert.Equal(t, beforeMatches, bridgeRoundtripMatches(t, loaded), "provider-consumer matches must survive a warm registry load")
				})
			}
		}
	}
}

func TestBridgeRegistryOwnerRecoveryPreservesSymbolLessProvider(t *testing.T) {
	for _, providerLast := range []bool{false, true} {
		name := "provider_first"
		if providerLast {
			name = "provider_last"
		}
		t.Run(name, func(t *testing.T) {
			store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			idx := New(store, newTestRegistry(), config.IndexConfig{}, zap.NewNop())
			idx.SetRepoPrefix("roundtrip")
			idx.SetWorkspaceID("workspace")
			idx.SetProjectID("project")
			idx.storeRootPath(t.TempDir())
			t.Cleanup(func() { idx.Close() })
			provider := contracts.Contract{
				ID: "env::SYMBOL_LESS", Type: contracts.ContractType("env"), Role: contracts.Role("provider"),
				RepoPrefix: "roundtrip", WorkspaceID: "workspace", ProjectID: "project",
				FilePath: "roundtrip/.env", Line: 1, Confidence: 1,
				Meta: map[string]any{"var": "SYMBOL_LESS"},
			}
			consumer := provider
			consumer.Role = contracts.Role("consumer")
			consumer.FilePath, consumer.SymbolID, consumer.Line = "roundtrip/consumer.go", "roundtrip/consumer.go::Consume", 22
			store.AddBatch([]*graph.Node{
				{ID: "roundtrip/.env", Kind: graph.KindFile, RepoPrefix: "roundtrip", FilePath: "roundtrip/.env"},
				{ID: consumer.SymbolID, Kind: graph.KindFunction, RepoPrefix: "roundtrip", FilePath: consumer.FilePath},
			}, nil)
			original := contracts.NewRegistry()
			if providerLast {
				original.Add(consumer)
				original.Add(provider)
			} else {
				original.Add(provider)
				original.Add(consumer)
			}
			require.Len(t, bridgeRoundtripMatches(t, original), 1)
			idx.commitContracts(original)
			warm := New(store, newTestRegistry(), config.IndexConfig{}, zap.NewNop())
			warm.SetRepoPrefix("roundtrip")
			warm.SetWorkspaceID("workspace")
			warm.SetProjectID("project")
			t.Cleanup(func() { warm.Close() })
			ownerRecovered := warm.ensureIncrementalContractRegistry()
			node := store.GetNodesByIDs([]string{provider.ID})[provider.ID]
			require.NotNil(t, node)
			t.Logf("node_role=%v node_symbol=%v cold_records=%d owner_records=%d cold_matches=%d owner_matches=%d",
				node.Meta["role"], node.Meta["symbol_id"], len(original.All()), len(ownerRecovered.All()), len(bridgeRoundtripMatches(t, original)), len(bridgeRoundtripMatches(t, ownerRecovered)))
			assert.Equal(t, bridgeRoundtripRecordMultiset(t, original.All()), bridgeRoundtripRecordMultiset(t, ownerRecovered.All()), "an existing file node does not make the writer emit a symbol-less ownership record")
			assert.Equal(t, bridgeRoundtripMatches(t, original), bridgeRoundtripMatches(t, ownerRecovered))
		})
	}
}

func TestBridgeRegistryLegacyRemovedScalarDoesNotResurrect(t *testing.T) {
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	provider := contracts.Contract{
		ID: "env::LEGACY_REMOVAL", Type: contracts.ContractType("env"), Role: contracts.Role("provider"),
		RepoPrefix: "repo-a", WorkspaceID: "workspace-a", ProjectID: "project-a",
		FilePath: "repo-a/.env", Line: 1, Confidence: 1, Meta: map[string]any{"var": "LEGACY_REMOVAL"},
	}
	consumer := provider
	consumer.Role = contracts.Role("consumer")
	consumer.RepoPrefix, consumer.WorkspaceID, consumer.ProjectID = "repo-b", "workspace-b", "project-b"
	consumer.FilePath, consumer.SymbolID, consumer.Line = "repo-b/consumer.go", "repo-b/consumer.go::Consume", 22
	// Explicit legacy scalar fixture, deliberately without the future per-record
	// owner marker. A had no SymbolID/owner edge; B has a surviving real owner.
	store.AddBatch([]*graph.Node{
		{ID: provider.FilePath, Kind: graph.KindFile, RepoPrefix: provider.RepoPrefix, FilePath: provider.FilePath},
		{ID: consumer.SymbolID, Kind: graph.KindFunction, RepoPrefix: consumer.RepoPrefix, FilePath: consumer.FilePath},
		{
			ID: provider.ID, Kind: graph.KindContract, RepoPrefix: provider.RepoPrefix,
			FilePath: provider.FilePath, WorkspaceID: provider.WorkspaceID, ProjectID: provider.ProjectID,
			Meta: map[string]any{"type": "env", "role": "provider", "symbol_id": "", "line": 1, "confidence": float64(1), "contract_meta": provider.Meta},
		},
	}, []*graph.Edge{{From: consumer.SymbolID, To: provider.ID, Kind: graph.EdgeConsumes, FilePath: consumer.FilePath, Line: consumer.Line, Meta: contractOwnerEdgeMeta(consumer)}})
	before := contracts.LoadRegistryFromGraph(store, provider.RepoPrefix)
	require.NotNil(t, before)
	require.Equal(t, bridgeRoundtripRecordMultiset(t, []contracts.Contract{provider}), bridgeRoundtripRecordMultiset(t, before.All()), "a recoverable unremoved legacy scalar remains compatible")
	fixtureReceipt := store.BeginMutationReceipt()
	result, err := graph.ReplaceContractOwners(store, graph.ContractOwnerReplacement{
		RepoPrefix: provider.RepoPrefix, FilePaths: []string{provider.FilePath}, TouchedNodeIDs: []string{provider.ID},
	})
	require.NoError(t, err)
	receipt := store.EndMutationReceipt(fixtureReceipt)
	require.NotNil(t, store.GetNodesByIDs([]string{provider.ID})[provider.ID], "B still owns the canonical contract node")
	var consumerOwners int
	for _, edge := range store.GetOutEdgesByNodeIDs([]string{consumer.SymbolID})[consumer.SymbolID] {
		if edge.To == provider.ID && edge.Kind == graph.EdgeConsumes {
			consumerOwners++
		}
	}
	require.Equal(t, 1, consumerOwners, "A's removal must retain B's owner")
	after := contracts.LoadRegistryFromGraph(store, provider.RepoPrefix)
	afterCount := 0
	if after != nil {
		afterCount = len(after.All())
	}
	t.Logf("legacy_before=1 after_explicit_removal=%d sibling_owners=%d nodes_changed=%d nodes_removed=%d receipt_complete=%t",
		afterCount, consumerOwners, result.NodesChanged, result.NodesRemoved, receipt.Complete)
	assert.Zero(t, afterCount, "explicit removal is evidence against scalar fallback; do not resurrect removed A")
	assert.Positive(t, result.NodesChanged+result.NodesRemoved, "invalidating a removed scalar must participate in mutation bookkeeping")
	assert.False(t, receipt.Complete, "this replacement's semantic mutation must invalidate its receipt as other owner replacements do")

	_, err = graph.ReplaceContractOwners(store, graph.ContractOwnerReplacement{
		RepoPrefix: consumer.RepoPrefix, FilePaths: []string{consumer.FilePath}, TouchedNodeIDs: []string{consumer.ID},
	})
	require.NoError(t, err)
	require.Nil(t, store.GetNodesByIDs([]string{provider.ID})[provider.ID], "the canonical node may be pruned once the final real owner is removed")
}

func TestBridgeRegistryBothWritersRetainFileOwnersAndRemoveOne(t *testing.T) {
	for _, incremental := range []bool{false, true} {
		name := "full"
		if incremental {
			name = "incremental"
		}
		t.Run(name, func(t *testing.T) {
			store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			idx := New(store, newTestRegistry(), config.IndexConfig{}, zap.NewNop())
			idx.SetRepoPrefix("repo")
			idx.SetWorkspaceID("workspace")
			idx.SetProjectID("project")
			idx.storeRootPath(t.TempDir())
			t.Cleanup(func() { idx.Close() })
			provider := contracts.Contract{ID: "env::FILE_OWNED", Type: contracts.ContractType("env"), Role: contracts.RoleProvider,
				RepoPrefix: "repo", WorkspaceID: "workspace", ProjectID: "project", FilePath: "repo/provider.env", Line: 1,
				Confidence: 1, Meta: map[string]any{"var": "FILE_OWNED"}}
			consumer := provider
			consumer.Role, consumer.FilePath, consumer.Line = contracts.RoleConsumer, "repo/consumer.env", 2
			store.AddBatch([]*graph.Node{
				{ID: provider.FilePath, Kind: graph.KindFile, RepoPrefix: "repo", FilePath: provider.FilePath},
				{ID: consumer.FilePath, Kind: graph.KindFile, RepoPrefix: "repo", FilePath: consumer.FilePath},
			}, nil)
			reg := contracts.NewRegistry()
			reg.Add(provider)
			reg.Add(consumer)
			for range 2 {
				if incremental {
					idx.commitIncrementalContractFiles(reg, []string{provider.FilePath, consumer.FilePath}, nil)
				} else {
					idx.commitContracts(reg)
				}
			}
			loaded := contracts.LoadRegistryFromGraph(store, "repo")
			require.NotNil(t, loaded)
			assert.Equal(t, bridgeRoundtripRecordMultiset(t, reg.All()), bridgeRoundtripRecordMultiset(t, loaded.All()))
			assert.Len(t, bridgeRoundtripMatches(t, loaded), 1)
			var owners int
			for _, edge := range store.GetInEdges(provider.ID) {
				if edge.Kind != graph.EdgeProvides && edge.Kind != graph.EdgeConsumes {
					continue
				}
				owners++
				symbol, present := edge.Meta["contract_owner_symbol_id"]
				assert.True(t, present)
				assert.Equal(t, "", symbol, "file endpoint must not become the reconstructed symbol")
			}
			assert.Equal(t, 2, owners, "repeated commits keep one row per source/role")
			remaining := contracts.NewRegistry()
			remaining.Add(consumer)
			idx.commitIncrementalContractFiles(remaining, []string{provider.FilePath}, map[string]struct{}{provider.ID: {}})
			loaded = contracts.LoadRegistryFromGraph(store, "repo")
			require.NotNil(t, loaded)
			assert.Equal(t, bridgeRoundtripRecordMultiset(t, []contracts.Contract{consumer}), bridgeRoundtripRecordMultiset(t, loaded.All()))
			assert.Empty(t, bridgeRoundtripMatches(t, loaded))
		})
	}
}

func TestBridgeRegistryMissingFileOwnerDoesNotFabricateSource(t *testing.T) {
	for _, incremental := range []bool{false, true} {
		name := "full"
		if incremental {
			name = "incremental"
		}
		t.Run(name, func(t *testing.T) {
			store := graph.New()
			idx := New(store, newTestRegistry(), config.IndexConfig{}, zap.NewNop())
			idx.SetRepoPrefix("repo")
			idx.SetWorkspaceID("workspace")
			idx.SetProjectID("project")
			idx.storeRootPath(t.TempDir())
			t.Cleanup(func() { idx.Close() })
			c := contracts.Contract{ID: "env::NO_ADMITTED_FILE", Type: contracts.ContractType("env"), Role: contracts.RoleProvider,
				RepoPrefix: "repo", WorkspaceID: "workspace", ProjectID: "project", FilePath: "repo/missing.env", Line: 1,
				Confidence: 1, Meta: map[string]any{"var": "NO_ADMITTED_FILE"}}
			// A conflicting file-path row owned by another repo must not be
			// accepted as a genuine source owner for this contract.
			store.AddNode(&graph.Node{ID: "other/missing.env", Kind: graph.KindFile, FilePath: c.FilePath, RepoPrefix: "other"})
			reg := contracts.NewRegistry()
			reg.Add(c)
			if incremental {
				idx.commitIncrementalContractFiles(reg, []string{c.FilePath}, nil)
			} else {
				idx.commitContracts(reg)
			}
			for _, node := range store.GetFileNodes(c.FilePath) {
				assert.False(t, node.Kind == graph.KindFile && node.RepoPrefix == "repo", "writer may not fabricate an admitted source")
			}
			assert.Empty(t, store.GetInEdges(c.ID))
			canonical := store.GetNode(c.ID)
			require.NotNil(t, canonical)
			ownerBacked, _ := canonical.Meta["contract_owner_record"].(bool)
			assert.False(t, ownerBacked, "do not claim the scalar has a persisted ownership row")
			loaded := contracts.LoadRegistryFromGraph(store, "repo")
			require.NotNil(t, loaded)
			assert.Equal(t, bridgeRoundtripRecordMultiset(t, []contracts.Contract{c}), bridgeRoundtripRecordMultiset(t, loaded.All()), "retain the one known scalar record without inventing missing multiplicity")
		})
	}
}

func TestBridgeRegistryInvalidSymbolOwnerDoesNotHideScalar(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, incremental := range []bool{false, true} {
			for _, wrongRepo := range []bool{false, true} {
				writer, source := "full", "missing"
				if incremental {
					writer = "incremental"
				}
				if wrongRepo {
					source = "wrong_repo"
				}
				t.Run(backend+"/"+writer+"/"+source, func(t *testing.T) {
					var store graph.Store = graph.New()
					if backend == "sqlite" {
						sqliteStore, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
						require.NoError(t, err)
						t.Cleanup(func() { require.NoError(t, sqliteStore.Close()) })
						store = sqliteStore
					}
					idx := New(store, newTestRegistry(), config.IndexConfig{}, zap.NewNop())
					idx.SetRepoPrefix("a")
					idx.SetWorkspaceID("workspace")
					idx.SetProjectID("project")
					idx.storeRootPath(t.TempDir())
					t.Cleanup(func() { idx.Close() })
					c := contracts.Contract{ID: "env::INVALID_SYMBOL_OWNER", Type: contracts.ContractType("env"), Role: contracts.RoleProvider,
						RepoPrefix: "a", WorkspaceID: "workspace", ProjectID: "project", FilePath: "a/owner.go", SymbolID: "a/owner.go::Provide", Line: 1,
						Confidence: 1, Meta: map[string]any{"var": "INVALID_SYMBOL_OWNER"}}
					if wrongRepo {
						store.AddNode(&graph.Node{ID: c.SymbolID, Kind: graph.KindFunction, RepoPrefix: "b", FilePath: c.FilePath})
					}
					reg := contracts.NewRegistry()
					reg.Add(c)
					if incremental {
						idx.commitIncrementalContractFiles(reg, []string{c.FilePath}, nil)
					} else {
						idx.commitContracts(reg)
					}
					canonical := store.GetNode(c.ID)
					require.NotNil(t, canonical)
					ownerBacked, _ := canonical.Meta["contract_owner_record"].(bool)
					assert.False(t, ownerBacked, "a symbol ID alone is not proof of a persistable correctly scoped owner row")
					rows := graph.ReadRepoEdgesByKinds(store, []string{"a"}, []graph.EdgeKind{graph.EdgeProvides, graph.EdgeConsumes})
					assert.Empty(t, rows)
					loaded := contracts.LoadRegistryFromGraph(store, "a")
					require.NotNil(t, loaded, "invalid ownership must not hide the known scalar record")
					assert.Equal(t, bridgeRoundtripRecordMultiset(t, []contracts.Contract{c}), bridgeRoundtripRecordMultiset(t, loaded.All()))
					t.Logf("raw_incoming_edges=%d scoped_owner_rows=%d owner_backed=%t", len(store.GetInEdges(c.ID)), len(rows), ownerBacked)
				})
			}
		}
	}
}

func producerCompatStore(t *testing.T, backend string) graph.Store {
	t.Helper()
	if backend == "memory" {
		return graph.New()
	}
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "producer.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	return store
}

func producerCompatIndexer(t *testing.T, store graph.Store) *Indexer {
	t.Helper()
	idx := New(store, newTestRegistry(), config.IndexConfig{}, zap.NewNop())
	idx.SetRepoPrefix("producer")
	idx.SetWorkspaceID("indexer-workspace")
	idx.SetProjectID("indexer-project")
	idx.storeRootPath(t.TempDir())
	t.Cleanup(func() { idx.Close() })
	return idx
}

func producerCompatRecords(t *testing.T, records []contracts.Contract) map[string]int {
	t.Helper()
	out := make(map[string]int, len(records))
	for _, record := range records {
		encoded, err := json.Marshal(record)
		require.NoError(t, err)
		out[string(encoded)]++
	}
	return out
}

func producerCompatCanonical(c contracts.Contract) *graph.Node {
	return &graph.Node{
		ID: c.ID, Kind: graph.KindContract, Name: c.ID,
		RepoPrefix: c.RepoPrefix, FilePath: c.FilePath,
		WorkspaceID: c.WorkspaceID, ProjectID: c.ProjectID,
		Meta: map[string]any{
			"contract_owner_record": true,
			"type":                  string(c.Type), "role": string(c.Role),
			"symbol_id": c.SymbolID, "line": c.Line,
			"confidence": c.Confidence, "contract_meta": c.Meta,
		},
	}
}

func TestBridgeProducerRetainsContractKindSymbolOwners(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, incremental := range []bool{false, true} {
			writer := "full"
			if incremental {
				writer = "incremental"
			}
			for _, kind := range []graph.NodeKind{graph.KindContract, graph.KindContractBridge} {
				for _, role := range []contracts.Role{contracts.RoleProvider, contracts.RoleConsumer} {
					t.Run(backend+"/"+writer+"/"+string(kind)+"/"+string(role), func(t *testing.T) {
						store := producerCompatStore(t, backend)
						idx := producerCompatIndexer(t, store)
						c := contracts.Contract{
							ID: "env::COMPATIBLE_SYMBOL_OWNER", Type: contracts.ContractType("env"), Role: role,
							RepoPrefix: "producer", WorkspaceID: "owner-workspace", ProjectID: "owner-project",
							FilePath: "producer/source.go", SymbolID: "producer/source.go::ExistingOwner", Line: 17,
							Confidence: 0.75, Meta: map[string]any{"var": "COMPATIBLE_SYMBOL_OWNER", "nested": map[string]any{"detail": "preserved"}},
						}
						store.AddBatch([]*graph.Node{{
							ID: c.SymbolID, Kind: kind, RepoPrefix: c.RepoPrefix, FilePath: c.FilePath,
							WorkspaceID: c.WorkspaceID, ProjectID: c.ProjectID,
						}}, nil)
						reg := contracts.NewRegistry()
						reg.Add(c)
						if incremental {
							idx.commitIncrementalContractFiles(reg, []string{c.FilePath}, nil)
						} else {
							idx.commitContracts(reg)
						}
						wantKind := graph.EdgeProvides
						if role == contracts.RoleConsumer {
							wantKind = graph.EdgeConsumes
						}
						var owners []*graph.Edge
						for _, edge := range store.GetInEdges(c.ID) {
							if edge.From == c.SymbolID && edge.Kind == wantKind {
								owners = append(owners, edge)
							}
						}
						// These prerequisites must already pass with the old writers:
						// the new admission guard cannot reject these existing kinds.
						require.Len(t, owners, 1, "valid same-repo SymbolID must still emit one owner row")
						require.Equal(t, c.FilePath, owners[0].FilePath)
						require.Equal(t, c.Line, owners[0].Line)
						wantMeta, err := json.Marshal(contractOwnerEdgeMeta(c))
						require.NoError(t, err)
						gotMeta, err := json.Marshal(owners[0].Meta)
						require.NoError(t, err)
						require.JSONEq(t, string(wantMeta), string(gotMeta), "persist every owner field and nested payload")
						source := store.GetNode(c.SymbolID)
						require.NotNil(t, source)
						require.Equal(t, kind, source.Kind)
						require.Len(t, graph.ReadRepoEdgesByKinds(store, []string{c.RepoPrefix}, []graph.EdgeKind{wantKind}), 1, "repository projection must admit the existing source kind")
						t.Log("prerequisite passed: existing unusual-kind source and complete owner edge persisted")
						canonical := store.GetNode(c.ID)
						require.NotNil(t, canonical)
						assert.Equal(t, true, canonical.Meta["contract_owner_record"], "admitted persisted owner must mark the canonical row owner-backed")
						loaded := contracts.LoadRegistryFromGraph(store, c.RepoPrefix)
						require.NotNil(t, loaded)
						assert.Equal(t, producerCompatRecords(t, []contracts.Contract{c}), producerCompatRecords(t, loaded.ByID(c.ID)))
					})
				}
			}
		}
	}
}

func TestBridgeIncrementalRegistryRetainsDistinctOwnerRows(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			store := producerCompatStore(t, backend)
			first := contracts.Contract{
				ID: "env::DISTINCT_OWNER_ROWS", Type: contracts.ContractType("env"), Role: contracts.RoleProvider,
				RepoPrefix: "producer", WorkspaceID: "owner-workspace", ProjectID: "owner-project",
				FilePath: "producer/source.go", SymbolID: "producer/source.go::Owner", Line: 11,
				Confidence: 0.5, Meta: map[string]any{"var": "DISTINCT_OWNER_ROWS", "variant": "first"},
			}
			second := first
			second.Line, second.Confidence = 23, 0.9
			second.Meta = map[string]any{"var": "DISTINCT_OWNER_ROWS", "variant": "second", "nested": map[string]any{"detail": "retained"}}
			want := producerCompatRecords(t, []contracts.Contract{first, second})
			original := contracts.NewRegistry()
			original.Add(first)
			original.Add(second)
			require.Equal(t, want, producerCompatRecords(t, original.ByID(first.ID)))
			require.Len(t, original.All(), 1, "fixture must actually exercise All's unchanged coalescing key")
			// Seed representable persisted owner rows directly. The full writer
			// legitimately consumes Registry.All(), whose historical key ignores
			// Line/metadata; asking it to emit both would be a different contract.
			store.AddBatch([]*graph.Node{
				{ID: first.SymbolID, Kind: graph.KindFunction, RepoPrefix: first.RepoPrefix, FilePath: first.FilePath},
				producerCompatCanonical(first),
			}, []*graph.Edge{
				{From: first.SymbolID, To: first.ID, Kind: graph.EdgeProvides, FilePath: first.FilePath, Line: first.Line, Meta: contractOwnerEdgeMeta(first)},
				{From: second.SymbolID, To: second.ID, Kind: graph.EdgeProvides, FilePath: second.FilePath, Line: second.Line, Meta: contractOwnerEdgeMeta(second)},
			})
			rows := graph.ReadRepoEdgesByKinds(store, []string{first.RepoPrefix}, []graph.EdgeKind{graph.EdgeProvides})
			require.Len(t, rows, 2, "repo projection must really admit two distinct persisted line-keyed owner rows")
			persisted := store.GetInEdges(first.ID)
			require.Len(t, persisted, 2)
			byLine := map[int]contracts.Contract{first.Line: first, second.Line: second}
			for _, edge := range persisted {
				wantRecord, ok := byLine[edge.Line]
				require.True(t, ok)
				require.Equal(t, wantRecord.SymbolID, edge.From)
				require.Equal(t, wantRecord.FilePath, edge.FilePath)
				wantMeta, err := json.Marshal(contractOwnerEdgeMeta(wantRecord))
				require.NoError(t, err)
				gotMeta, err := json.Marshal(edge.Meta)
				require.NoError(t, err)
				require.JSONEq(t, string(wantMeta), string(gotMeta))
			}
			t.Log("prerequisite passed: both complete persisted owner rows admitted")
			loaded := contracts.LoadRegistryFromGraph(store, first.RepoPrefix)
			require.NotNil(t, loaded)
			assert.Equal(t, want, producerCompatRecords(t, loaded.ByID(first.ID)), "public loader must retain the complete record multiset")
			assert.Equal(t, want, producerCompatRecords(t, loaded.ByRepo(first.RepoPrefix)))
			require.Len(t, loaded.All(), 1, "Registry.All historical logical-key coalescing is unchanged")
			idx := producerCompatIndexer(t, store)
			cached := idx.ensureIncrementalContractRegistry()
			require.NotNil(t, cached)
			assert.Equal(t, want, producerCompatRecords(t, cached.ByID(first.ID)), "cache restore must use repo records, not coalescing All()")
			assert.Equal(t, want, producerCompatRecords(t, cached.ByFile(first.FilePath)))
			assert.Equal(t, producerCompatRecords(t, loaded.ByID(first.ID)), producerCompatRecords(t, cached.ByID(first.ID)), "public and incremental restoration must agree on full records")
			require.Len(t, cached.All(), 1, "do not change Registry.All semantics to hide restoration loss")
		})
	}
}

func TestBridgeIncrementalRegistryPreservesExplicitEmptyOwnerScope(t *testing.T) {
	cases := []struct {
		name             string
		workspacePresent bool
		workspace        string
		projectPresent   bool
		project          string
	}{
		{name: "absent_uses_indexer_fallback"},
		{name: "explicit_empty_overrides_indexer_fallback", workspacePresent: true, projectPresent: true},
		{name: "workspace_empty_project_absent", workspacePresent: true},
		{name: "workspace_absent_project_empty", projectPresent: true},
		{name: "workspace_nonempty_project_absent", workspacePresent: true, workspace: "payload-workspace"},
		{name: "workspace_absent_project_nonempty", projectPresent: true, project: "payload-project"},
	}
	for _, backend := range []string{"memory", "sqlite"} {
		for _, tc := range cases {
			t.Run(backend+"/"+tc.name, func(t *testing.T) {
				store := producerCompatStore(t, backend)
				c := contracts.Contract{
					ID: "env::OWNER_SCOPE", Type: contracts.ContractType("env"), Role: contracts.RoleProvider,
					RepoPrefix: "producer", FilePath: "producer/scope.go", SymbolID: "producer/scope.go::Owner", Line: 7,
					Confidence: 1, Meta: map[string]any{"var": "OWNER_SCOPE"},
				}
				meta := contractOwnerEdgeMeta(c)
				// A persisted legacy row can distinguish explicit empty from
				// absent fields even if today's writer normalizes empty scope.
				want := c
				want.WorkspaceID, want.ProjectID = "indexer-workspace", "indexer-project"
				if tc.workspacePresent {
					meta["contract_owner_workspace"] = tc.workspace
					want.WorkspaceID = tc.workspace
				} else {
					delete(meta, "contract_owner_workspace")
				}
				if tc.projectPresent {
					meta["contract_owner_project"] = tc.project
					want.ProjectID = tc.project
				} else {
					delete(meta, "contract_owner_project")
				}
				store.AddBatch([]*graph.Node{
					{ID: c.SymbolID, Kind: graph.KindFunction, RepoPrefix: c.RepoPrefix, FilePath: c.FilePath},
					producerCompatCanonical(c),
				}, []*graph.Edge{{From: c.SymbolID, To: c.ID, Kind: graph.EdgeProvides, FilePath: c.FilePath, Line: c.Line, Meta: meta}})
				rows := graph.ReadRepoEdgesByKinds(store, []string{c.RepoPrefix}, []graph.EdgeKind{graph.EdgeProvides})
				require.Len(t, rows, 1)
				persisted := store.GetInEdges(c.ID)
				require.Len(t, persisted, 1)
				if tc.workspacePresent {
					require.Equal(t, tc.workspace, persisted[0].Meta["contract_owner_workspace"])
				} else {
					require.NotContains(t, persisted[0].Meta, "contract_owner_workspace")
				}
				if tc.projectPresent {
					require.Equal(t, tc.project, persisted[0].Meta["contract_owner_project"])
				} else {
					require.NotContains(t, persisted[0].Meta, "contract_owner_project")
				}
				idx := producerCompatIndexer(t, store)
				cached := idx.ensureIncrementalContractRegistry()
				require.NotNil(t, cached)
				assert.Equal(t, producerCompatRecords(t, []contracts.Contract{want}), producerCompatRecords(t, cached.ByID(c.ID)), "explicit empty and absent metadata have different fallback semantics")
				assert.Equal(t, producerCompatRecords(t, []contracts.Contract{want}), producerCompatRecords(t, cached.ByFile(c.FilePath)))
			})
		}
	}
}

func TestBridgeIncrementalRegistryRetainsScalarScopeOverride(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			store := producerCompatStore(t, backend)
			stored := contracts.Contract{
				ID: "env::LEGACY_SCALAR_SCOPE", Type: contracts.ContractType("env"), Role: contracts.RoleProvider,
				RepoPrefix: "producer", FilePath: "producer/legacy.env", Line: 3,
				WorkspaceID: "stored-workspace", ProjectID: "stored-project",
				Confidence: 1, Meta: map[string]any{"var": "LEGACY_SCALAR_SCOPE"},
			}
			node := producerCompatCanonical(stored)
			delete(node.Meta, "contract_owner_record")
			store.AddBatch([]*graph.Node{node}, nil)
			require.Empty(t, store.GetInEdges(stored.ID), "fixture is a legacy scalar, not owner metadata")
			public := contracts.LoadRegistryFromGraph(store, stored.RepoPrefix)
			require.NotNil(t, public)
			assert.Equal(t, producerCompatRecords(t, []contracts.Contract{stored}), producerCompatRecords(t, public.ByID(stored.ID)), "public two-argument loader retains stored scalar scope")
			idx := producerCompatIndexer(t, store)
			cached := idx.ensureIncrementalContractRegistry()
			require.NotNil(t, cached)
			want := stored
			want.WorkspaceID, want.ProjectID = "indexer-workspace", "indexer-project"
			assert.Equal(t, producerCompatRecords(t, []contracts.Contract{want}), producerCompatRecords(t, cached.ByID(stored.ID)), "private incremental scalar restoration retains its historical indexer-scope override")
			assert.Equal(t, producerCompatRecords(t, []contracts.Contract{want}), producerCompatRecords(t, cached.ByFile(stored.FilePath)))
		})
	}
}
