package store_sqlite

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// countFTSRowidRows returns the symbol_fts_rowid rows mapped to nodeID.
func countFTSRowidRows(t *testing.T, path, nodeID string) int {
	t.Helper()
	var n int
	withRawDB(t, path, func(db *sql.DB) {
		require.NoError(t, db.QueryRow(
			`SELECT COUNT(*) FROM symbol_fts_rowid WHERE node_id = ?`, nodeID).Scan(&n))
	})
	return n
}

// TestOpenPurgesLegacyCoverageSpellings is the upgrade proof for the
// coverage-domain path-spelling purge. Stores written on Windows before
// the builders preserved the extractor's path spelling hold todo/fixture
// nodes and licensed_as / owns / generated_by / depends_on_module /
// annotated edges keyed by the forward-slash twin of the native
// backslash spelling. Nothing evicts those rows (eviction is
// spelling-exact), so a versioned migration removes them: per-file
// artifact nodes and coverage edges selectively by kind + FilePath
// spelling, shared targets only once nothing references them. Every
// native-spelled row must survive untouched.
func TestOpenPurgesLegacyCoverageSpellings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")

	const (
		nativeA = `r/src\a.go`
		nativeB = `r/src\b.go`
		legacyA = `r/src/a.go`

		nativeTodo  = nativeA + `::todo:5`
		legacyTodo  = legacyA + `::todo:3`
		legacyFix   = `r/testdata/x.json`
		licMIT      = `r/license::MIT`
		licGPL      = `r/license::GPL-3.0`
		teamCore    = `r/team::core`
		moduleX     = `r/module::go::example.com/x@v1`
		nativeMod   = `r/sub\go.mod`
		legacyMod   = `r/sub/go.mod`
		genExternal = `r/external::protoc`
		teamSolo    = `r/team::solo`
		generator   = `r/generator::protoc`
		symbolA     = nativeA + `::Alpha`
	)

	s, err := Open(path)
	require.NoError(t, err)
	s.AddBatch([]*graph.Node{
		// Native file nodes: prove the store keys paths with backslashes.
		{ID: nativeA, Kind: graph.KindFile, Name: "a.go", FilePath: nativeA, RepoPrefix: "r"},
		{ID: nativeB, Kind: graph.KindFile, Name: "b.go", FilePath: nativeB, RepoPrefix: "r"},
		// Native todo: must survive.
		{ID: nativeTodo, Kind: graph.KindTodo, Name: "todo:5", FilePath: nativeA, RepoPrefix: "r"},
		// Legacy per-file artifacts: must be purged.
		{ID: legacyTodo, Kind: graph.KindTodo, Name: "todo:3", FilePath: legacyA, RepoPrefix: "r"},
		{ID: legacyFix, Kind: graph.KindFixture, Name: "x.json", FilePath: legacyFix, RepoPrefix: "r"},
		// Shared target anchored to the LEGACY spelling but still
		// referenced natively by b: node must survive, anchor and all.
		{ID: licMIT, Kind: graph.KindLicense, Name: "MIT", FilePath: legacyA, RepoPrefix: "r"},
		// Shared target whose only reference is legacy: orphaned after
		// the edge purge, so the node goes too.
		{ID: licGPL, Kind: graph.KindLicense, Name: "GPL-3.0", FilePath: legacyA, RepoPrefix: "r"},
		// Team referenced by a non-coverage authored edge: the legacy
		// owns edge is purged but the node must survive.
		{ID: teamCore, Kind: graph.KindTeam, Name: "core", FilePath: legacyA, RepoPrefix: "r"},
		// Shared target anchored NATIVELY with one legacy + one native
		// edge: node and native edge survive.
		{ID: moduleX, Kind: graph.KindModule, Name: "x", FilePath: nativeMod, RepoPrefix: "r"},
		// A team whose ONLY reference is a legacy owns edge: orphaned by
		// the purge, so it must go. Without it the owns arm of the
		// shared-target snapshot is unobservable.
		{ID: teamSolo, Kind: graph.KindTeam, Name: "solo", FilePath: legacyA, RepoPrefix: "r"},
		// A materialized generator stub referenced only by a legacy
		// generated_by edge: likewise orphaned. (Generator targets are
		// synthetic `external::` ids; the exporter materializes them as
		// artifact-shaped stubs.)
		{ID: generator, Kind: graph.KindArtifact, Name: "protoc", FilePath: legacyA, RepoPrefix: "r"},
		// A symbol under the NATIVE file, used below to prove a
		// non-coverage edge is never selected by kind.
		{ID: symbolA, Kind: graph.KindFunction, Name: "Alpha", FilePath: nativeA, RepoPrefix: "r"},
	}, []*graph.Edge{
		// Native rows: every one must survive.
		{From: nativeA, To: nativeTodo, Kind: graph.EdgeAnnotated, FilePath: nativeA, Line: 5},
		{From: nativeB, To: licMIT, Kind: graph.EdgeLicensedAs, FilePath: nativeB},
		{From: teamCore, To: nativeB, Kind: graph.EdgeAuthored, FilePath: nativeB},
		{From: nativeMod, To: moduleX, Kind: graph.EdgeDependsOnModule, FilePath: nativeMod, Line: 2},
		// Legacy rows: every one must be purged.
		{From: legacyA, To: legacyTodo, Kind: graph.EdgeAnnotated, FilePath: legacyA, Line: 3},
		{From: legacyA, To: licMIT, Kind: graph.EdgeLicensedAs, FilePath: legacyA},
		{From: legacyA, To: licGPL, Kind: graph.EdgeLicensedAs, FilePath: legacyA},
		{From: teamCore, To: legacyA, Kind: graph.EdgeOwns, FilePath: legacyA},
		{From: legacyA, To: genExternal, Kind: graph.EdgeGeneratedBy, FilePath: legacyA},
		{From: legacyMod, To: moduleX, Kind: graph.EdgeDependsOnModule, FilePath: legacyMod, Line: 2},
		{From: teamSolo, To: legacyA, Kind: graph.EdgeOwns, FilePath: legacyA},
		{From: legacyA, To: generator, Kind: graph.EdgeGeneratedBy, FilePath: legacyA},
		// A NON-coverage edge carrying the legacy spelling. Selection is
		// by kind, so this must survive untouched: without it, widening
		// coverageEdgeKinds to a structural kind would go unnoticed.
		{From: symbolA, To: moduleX, Kind: graph.EdgeReferences, FilePath: legacyA, Line: 9},
	})
	// Legacy artifact nodes were FTS-indexed by the old binary; the purge
	// must clear their search rows so no ghost hits outlive the nodes.
	require.NoError(t, s.UpsertSymbolFTS(legacyTodo, "stale marker"))
	require.NoError(t, s.UpsertSymbolFTS(nativeTodo, "live marker"))
	require.NoError(t, s.Close())

	// Simulate a store written before the purge shipped.
	withRawDB(t, path, func(db *sql.DB) {
		_, err := db.Exec(`PRAGMA user_version = 12`)
		require.NoError(t, err, "reset to the pre-purge version")
	})

	s2, err := Open(path)
	require.NoError(t, err)

	// Purged rows.
	require.Nil(t, s2.GetNode(legacyTodo), "legacy todo node must be purged")
	require.Nil(t, s2.GetNode(legacyFix), "legacy fixture node must be purged")
	require.Nil(t, s2.GetNode(licGPL), "orphaned shared license must be removed")
	require.Nil(t, s2.GetNode(teamSolo), "a team left with no references at all must be removed")
	require.Nil(t, s2.GetNode(generator), "a generator left with no references at all must be removed")
	// Survivors.
	require.NotNil(t, s2.GetNode(nativeTodo), "native todo node must survive")
	require.NotNil(t, s2.GetNode(licMIT), "shared license anchored to a legacy path stays while referenced")
	require.NotNil(t, s2.GetNode(teamCore), "team referenced by an authored edge stays")
	require.NotNil(t, s2.GetNode(moduleX), "natively referenced module stays")

	inMIT := s2.GetInEdges(licMIT)
	require.Len(t, inMIT, 1, "exactly b's native licensed_as edge remains on MIT")
	require.Equal(t, nativeB, inMIT[0].From)

	outTeam := s2.GetOutEdges(teamCore)
	require.Len(t, outTeam, 1, "only the authored edge remains on the team")
	require.Equal(t, graph.EdgeAuthored, outTeam[0].Kind)

	var modFroms []string
	for _, e := range s2.GetInEdges(moduleX) {
		if e != nil && e.Kind == graph.EdgeDependsOnModule {
			modFroms = append(modFroms, e.From)
		}
	}
	require.Equal(t, []string{nativeMod}, modFroms,
		"exactly the native depends_on_module edge remains on the module")

	outA := s2.GetOutEdges(nativeA)
	require.Len(t, outA, 1, "the native annotated edge survives")
	require.Equal(t, graph.EdgeAnnotated, outA[0].Kind)

	// Selection is by kind: a structural edge is untouched even when it
	// carries the legacy spelling.
	outSym := s2.GetOutEdges(symbolA)
	require.Len(t, outSym, 1, "a non-coverage edge is never selected, whatever its path spelling")
	require.Equal(t, graph.EdgeReferences, outSym[0].Kind)

	var legacyCoverage []string
	for _, e := range s2.GetOutEdges(legacyA) {
		if e != nil {
			legacyCoverage = append(legacyCoverage, string(e.Kind))
		}
	}
	require.Empty(t, legacyCoverage, "no legacy-spelled coverage edge may survive")

	require.NoError(t, s2.Close())
	require.Zero(t, countFTSRowidRows(t, path, legacyTodo), "purged node's FTS rows must go with it")
	require.NotZero(t, countFTSRowidRows(t, path, nativeTodo), "surviving node keeps its FTS rows")
}

// TestOpenPurgesLegacyCoverageSpellingsSingleRepo covers the unprefixed
// store shape: no repo prefix means the whole FilePath is the path
// portion, so any forward slash marks the legacy twin on a
// backslash-keyed store.
func TestOpenPurgesLegacyCoverageSpellingsSingleRepo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")

	const (
		nativeA    = `src\a.go`
		legacyA    = `src/a.go`
		nativeTodo = nativeA + `::todo:5`
		legacyTodo = legacyA + `::todo:3`
	)

	s, err := Open(path)
	require.NoError(t, err)
	s.AddBatch([]*graph.Node{
		{ID: nativeA, Kind: graph.KindFile, Name: "a.go", FilePath: nativeA},
		{ID: nativeTodo, Kind: graph.KindTodo, Name: "todo:5", FilePath: nativeA},
		{ID: legacyTodo, Kind: graph.KindTodo, Name: "todo:3", FilePath: legacyA},
	}, []*graph.Edge{
		{From: nativeA, To: nativeTodo, Kind: graph.EdgeAnnotated, FilePath: nativeA, Line: 5},
		{From: legacyA, To: legacyTodo, Kind: graph.EdgeAnnotated, FilePath: legacyA, Line: 3},
	})
	require.NoError(t, s.Close())

	withRawDB(t, path, func(db *sql.DB) {
		_, err := db.Exec(`PRAGMA user_version = 12`)
		require.NoError(t, err, "reset to the pre-purge version")
	})

	s2, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s2.Close() })

	require.Nil(t, s2.GetNode(legacyTodo), "legacy todo node must be purged")
	require.Empty(t, s2.GetOutEdges(legacyA), "legacy annotated edge must be purged")
	require.NotNil(t, s2.GetNode(nativeTodo), "native todo node must survive")
	require.Len(t, s2.GetOutEdges(nativeA), 1, "native annotated edge must survive")
}

// TestOpenLeavesPosixRepoUntouchedInMixedStore is the guard against the
// worst failure this migration could have: judging one repository by
// another's separator.
//
// A `fixture` node reuses the file node's ID by design (internal/fixtures:
// "the fixture is the file"; ReclassifyFileToFixture upgrades a file node
// in place). So if a POSIX-indexed repository's paths were judged by the
// Windows rule, the purge would delete a LIVE file node and orphan every
// symbol it defines. The scope is therefore per-repository: a store holding
// a Windows-indexed repo beside a POSIX-indexed one heals the first and
// must not touch a single row of the second.
func TestOpenLeavesPosixRepoUntouchedInMixedStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")

	const (
		winFile    = `win/src\a.go`
		winLegacy  = `win/src/a.go`
		winTodo    = winLegacy + `::todo:3`
		nixFixture = `nix/testdata/golden.json` // the fixture IS the file node
		nixSymbol  = `nix/testdata/golden.json::Root`
		nixFile    = `nix/src/b.go`
		nixTodo    = nixFile + `::todo:7`
		nixLicense = `nix/license::MIT`
	)

	s, err := Open(path)
	require.NoError(t, err)
	s.AddBatch([]*graph.Node{
		{ID: winFile, Kind: graph.KindFile, Name: "a.go", FilePath: winFile, RepoPrefix: "win"},
		{ID: winTodo, Kind: graph.KindTodo, Name: "todo:3", FilePath: winLegacy, RepoPrefix: "win"},
		{ID: nixFixture, Kind: graph.KindFixture, Name: "golden.json", FilePath: nixFixture, RepoPrefix: "nix"},
		{ID: nixSymbol, Kind: graph.KindVariable, Name: "Root", FilePath: nixFixture, RepoPrefix: "nix"},
		{ID: nixFile, Kind: graph.KindFile, Name: "b.go", FilePath: nixFile, RepoPrefix: "nix"},
		{ID: nixTodo, Kind: graph.KindTodo, Name: "todo:7", FilePath: nixFile, RepoPrefix: "nix"},
		{ID: nixLicense, Kind: graph.KindLicense, Name: "MIT", FilePath: nixFile, RepoPrefix: "nix"},
	}, []*graph.Edge{
		{From: winLegacy, To: winTodo, Kind: graph.EdgeAnnotated, FilePath: winLegacy, Line: 3},
		{From: nixFixture, To: nixSymbol, Kind: graph.EdgeDefines, FilePath: nixFixture, Line: 1},
		{From: nixFile, To: nixTodo, Kind: graph.EdgeAnnotated, FilePath: nixFile, Line: 7},
		{From: nixFile, To: nixLicense, Kind: graph.EdgeLicensedAs, FilePath: nixFile},
	})
	require.NoError(t, s.Close())

	withRawDB(t, path, func(db *sql.DB) {
		_, err := db.Exec(`PRAGMA user_version = 12`)
		require.NoError(t, err, "reset to the pre-purge version")
	})

	s2, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s2.Close() })

	require.Nil(t, s2.GetNode(winTodo), "the Windows repo's legacy row still heals")

	require.NotNil(t, s2.GetNode(nixFixture),
		"a POSIX repo's fixture node IS its file node and must survive")
	require.Len(t, s2.GetOutEdges(nixFixture), 1,
		"the POSIX file's defines edge must survive, or its symbols are orphaned")
	require.NotNil(t, s2.GetNode(nixSymbol), "the defined symbol must keep its parent")
	require.NotNil(t, s2.GetNode(nixTodo), "a POSIX repo's todo is correctly spelled, not legacy")
	require.NotNil(t, s2.GetNode(nixLicense), "a POSIX repo's license must survive")
	require.Len(t, s2.GetOutEdges(nixFile), 2, "both POSIX coverage edges must survive")
}

// TestOpenLeavesSyntheticPathsUntouched pins the synthetic-namespace
// exclusion. A stub node's FilePath is not a file: `external::go:x/y` and
// `module::go:x/y@v1` carry forward slashes that are part of an import
// path, not separators, and a depends_on_module edge can be minted against
// them (see modules.LinkImports). Judging those by the separator rule would
// delete live third-party attribution.
func TestOpenLeavesSyntheticPathsUntouched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")

	const (
		nativeA = `r/src\a.go`
		extPath = `r/external::go:github.com/pkg/errors`
		extSym  = `r/external::go:github.com/pkg/errors::New`
		modNode = `r/module::go:github.com/pkg/errors@v0.9.1`
	)

	s, err := Open(path)
	require.NoError(t, err)
	s.AddBatch([]*graph.Node{
		{ID: nativeA, Kind: graph.KindFile, Name: "a.go", FilePath: nativeA, RepoPrefix: "r"},
		{ID: extSym, Kind: graph.KindFunction, Name: "New", FilePath: extPath, RepoPrefix: "r"},
		{ID: modNode, Kind: graph.KindModule, Name: "errors", FilePath: extPath, RepoPrefix: "r"},
	}, []*graph.Edge{
		{From: extSym, To: modNode, Kind: graph.EdgeDependsOnModule, FilePath: extPath},
		{From: nativeA, To: extSym, Kind: graph.EdgeCalls, FilePath: nativeA, Line: 3},
	})
	require.NoError(t, s.Close())

	withRawDB(t, path, func(db *sql.DB) {
		_, err := db.Exec(`PRAGMA user_version = 12`)
		require.NoError(t, err, "reset to the pre-purge version")
	})

	s2, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s2.Close() })

	require.NotNil(t, s2.GetNode(modNode), "a module stub is not a file and must survive")
	require.Len(t, s2.GetOutEdges(extSym), 1,
		"the module attribution edge must survive: its FilePath is a stub namespace, not a path")
}

// TestOpenLeavesUnprefixedSyntheticPathsUntouched is the single-repo form
// of the synthetic-path exclusion, and the one that matters in production:
// the Go externals lane mints live depends_on_module edges whose FilePath
// is `external::go:<importPath>` (internal/semantic/goanalysis:
// externalFilePath). In a single-repo store the unprefixed arm of the
// predicate is active, so without the `::` exclusion that import path's own
// forward slashes would read as separators and the purge would delete live
// third-party attribution.
func TestOpenLeavesUnprefixedSyntheticPathsUntouched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")

	const (
		nativeA  = `src\a.go`
		extPath  = `external::go:github.com/pkg/errors`
		bareExt  = `external::go`
		extSym   = `external::go:github.com/pkg/errors::New`
		modErr   = `module::go:github.com/pkg/errors@v0.9.1`
		modStd   = `module::go:std`
		stdSym   = `external::go::Println`
		legacyTd = `src/a.go::todo:3`
	)

	s, err := Open(path)
	require.NoError(t, err)
	s.AddBatch([]*graph.Node{
		{ID: nativeA, Kind: graph.KindFile, Name: "a.go", FilePath: nativeA},
		{ID: legacyTd, Kind: graph.KindTodo, Name: "todo:3", FilePath: `src/a.go`},
		{ID: extSym, Kind: graph.KindFunction, Name: "New", FilePath: extPath},
		{ID: modErr, Kind: graph.KindModule, Name: "errors", FilePath: extPath},
		{ID: stdSym, Kind: graph.KindFunction, Name: "Println", FilePath: bareExt},
		{ID: modStd, Kind: graph.KindModule, Name: "stdlib", FilePath: bareExt},
	}, []*graph.Edge{
		{From: `src/a.go`, To: legacyTd, Kind: graph.EdgeAnnotated, FilePath: `src/a.go`, Line: 3},
		{From: extSym, To: modErr, Kind: graph.EdgeDependsOnModule, FilePath: extPath},
		{From: stdSym, To: modStd, Kind: graph.EdgeDependsOnModule, FilePath: bareExt},
	})
	require.NoError(t, s.Close())

	withRawDB(t, path, func(db *sql.DB) {
		_, err := db.Exec(`PRAGMA user_version = 12`)
		require.NoError(t, err, "reset to the pre-purge version")
	})

	s2, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s2.Close() })

	require.Nil(t, s2.GetNode(legacyTd), "the store's real legacy row still heals")

	require.NotNil(t, s2.GetNode(modErr), "a module stub is not a file and must survive")
	require.NotNil(t, s2.GetNode(modStd), "the stdlib module stub must survive")
	require.Len(t, s2.GetOutEdges(extSym), 1,
		"module attribution survives: the import path's slashes are not separators")
	require.Len(t, s2.GetOutEdges(stdSym), 1, "the bare external namespace survives too")
}

// TestLegacyPathPredicateAcrossRepoScopes exercises the predicate directly,
// with explicit spellings and no store. This is the platform-parametric
// helper the review asked for: it takes both spellings as literals, so it
// exercises the Windows split identically on every runner, and it is the
// only place the multi-arm shape (several Windows repos, a POSIX repo
// beside them, a prefix that is a prefix of another) is covered.
func TestLegacyPathPredicateAcrossRepoScopes(t *testing.T) {
	scope := coverageSpellingScope{
		windowsPrefixes: []string{"win", "gortex"},
		knownPrefixes:   []string{"win", "gortex", "gortexish", "nix"},
	}

	cases := []struct {
		path string
		want bool
		why  string
	}{
		{`win/src/a.go`, true, "legacy spelling inside a Windows repo"},
		{`win/src\a.go`, false, "native spelling inside a Windows repo"},
		{`win/main.go`, false, "top-level file: no separator below the prefix"},
		{`gortex/internal/foo.go`, true, "second Windows repo is judged too"},
		{`gortexish/internal/foo.go`, false, "a repo whose prefix merely shares a leading string is not"},
		{`nix/src/b.go`, false, "a POSIX repo is never judged"},
		{`nix/testdata/golden.json`, false, "including its fixture, which IS its file node"},
		{`win/external::go:github.com/pkg/errors`, false, "synthetic namespace, not a path"},
		{`unknown/src/c.go`, false, "no matching Windows prefix and the unprefixed arm is off"},
	}

	path := filepath.Join(t.TempDir(), "store.sqlite")
	s, err := Open(path)
	require.NoError(t, err)
	require.NoError(t, s.Close())
	withRawDB(t, path, func(db *sql.DB) {
		pred := scope.legacyPathPredicate("file_path")
		for _, tc := range cases {
			var got bool
			require.NoError(t,
				db.QueryRow(`WITH p(file_path) AS (VALUES (?)) SELECT (`+pred+`) FROM p`, tc.path).Scan(&got))
			require.Equal(t, tc.want, got, "%s: %s", tc.path, tc.why)
		}
	})

	t.Run("single repo store judges the whole path", func(t *testing.T) {
		solo := coverageSpellingScope{unprefixedIsWindows: true}
		soloPath := filepath.Join(t.TempDir(), "store.sqlite")
		s, err := Open(soloPath)
		require.NoError(t, err)
		require.NoError(t, s.Close())
		withRawDB(t, soloPath, func(db *sql.DB) {
			pred := solo.legacyPathPredicate("file_path")
			for _, tc := range []struct {
				path string
				want bool
			}{
				{`src/a.go`, true},
				{`src\a.go`, false},
				{`main.go`, false},
				{`external::go:github.com/pkg/errors`, false},
			} {
				var got bool
				require.NoError(t,
					db.QueryRow(`WITH p(file_path) AS (VALUES (?)) SELECT (`+pred+`) FROM p`, tc.path).Scan(&got))
				require.Equal(t, tc.want, got, tc.path)
			}
		})
	})

	t.Run("no windows repo disables the purge entirely", func(t *testing.T) {
		none := coverageSpellingScope{knownPrefixes: []string{"nix"}}
		require.True(t, none.empty())
		require.Equal(t, "0", none.legacyPathPredicate("file_path"))
	})
}

// TestPurgeLegacyCoverageSpellingsIsIdempotent runs the step twice on
// one connection: the second pass must find nothing left to remove and
// must not trip over the temp tables the first pass created.
func TestPurgeLegacyCoverageSpellingsIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")

	const (
		nativeA    = `r/src\a.go`
		legacyA    = `r/src/a.go`
		legacyTodo = legacyA + `::todo:3`
		lic        = `r/license::MIT`
	)

	s, err := Open(path)
	require.NoError(t, err)
	s.AddBatch([]*graph.Node{
		{ID: nativeA, Kind: graph.KindFile, Name: "a.go", FilePath: nativeA, RepoPrefix: "r"},
		{ID: legacyTodo, Kind: graph.KindTodo, Name: "todo:3", FilePath: legacyA, RepoPrefix: "r"},
		{ID: lic, Kind: graph.KindLicense, Name: "MIT", FilePath: legacyA, RepoPrefix: "r"},
	}, []*graph.Edge{
		{From: legacyA, To: legacyTodo, Kind: graph.EdgeAnnotated, FilePath: legacyA, Line: 3},
		{From: legacyA, To: lic, Kind: graph.EdgeLicensedAs, FilePath: legacyA},
	})
	require.NoError(t, s.Close())

	withRawDB(t, path, func(db *sql.DB) {
		nodesAfter := func() int {
			var n int
			require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&n))
			return n
		}
		for pass := 1; pass <= 2; pass++ {
			tx, err := db.Begin()
			require.NoError(t, err)
			require.NoError(t, purgeLegacyCoverageSpellings(tx), "pass %d", pass)
			require.NoError(t, tx.Commit())
		}
		require.Equal(t, 1, nodesAfter(), "only the native file node survives both passes")
	})
}

// TestOpenLeavesPosixCoverageRowsUntouched pins the guard: on a store
// with no backslash-keyed paths every row IS the native spelling, so
// the purge must not run at all. Without the guard the migration would
// eat every coverage artifact on every POSIX store.
func TestOpenLeavesPosixCoverageRowsUntouched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")

	const (
		fileA = `r/src/a.go`
		todoA = `r/src/a.go::todo:3`
		lic   = `r/license::MIT`
	)

	s, err := Open(path)
	require.NoError(t, err)
	s.AddBatch([]*graph.Node{
		{ID: fileA, Kind: graph.KindFile, Name: "a.go", FilePath: fileA, RepoPrefix: "r"},
		{ID: todoA, Kind: graph.KindTodo, Name: "todo:3", FilePath: fileA, RepoPrefix: "r"},
		{ID: lic, Kind: graph.KindLicense, Name: "MIT", FilePath: fileA, RepoPrefix: "r"},
	}, []*graph.Edge{
		{From: fileA, To: todoA, Kind: graph.EdgeAnnotated, FilePath: fileA, Line: 3},
		{From: fileA, To: lic, Kind: graph.EdgeLicensedAs, FilePath: fileA},
	})
	require.NoError(t, s.Close())

	withRawDB(t, path, func(db *sql.DB) {
		_, err := db.Exec(`PRAGMA user_version = 12`)
		require.NoError(t, err, "reset to the pre-purge version")
	})

	s2, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s2.Close() })

	require.NotNil(t, s2.GetNode(todoA), "POSIX todo node must survive")
	require.NotNil(t, s2.GetNode(lic), "POSIX license node must survive")
	require.Len(t, s2.GetOutEdges(fileA), 2, "both POSIX coverage edges must survive")
}
