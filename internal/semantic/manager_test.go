package semantic

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

// mockProvider is a test provider that records calls.
type mockProvider struct {
	name       string
	languages  []string
	available  bool
	enrichFunc func(g graph.Store, root string) (*EnrichResult, error)
	closed     bool
}

type contextBatchProvider struct {
	*mockProvider
	hadDeadline bool
	deadline    time.Time
	files       []string
}

func (p *contextBatchProvider) EnrichFilesContext(ctx context.Context, _ graph.Store, _, _ string, files []string) (*EnrichResult, error) {
	p.deadline, p.hadDeadline = ctx.Deadline()
	p.files = append([]string(nil), files...)
	return &EnrichResult{Provider: p.name, Language: "go"}, nil
}

func (m *mockProvider) Name() string        { return m.name }
func (m *mockProvider) Languages() []string { return m.languages }
func (m *mockProvider) Available() bool     { return m.available }
func (m *mockProvider) Close() error        { m.closed = true; return nil }

func (m *mockProvider) Enrich(g graph.Store, repoRoot string) (*EnrichResult, error) {
	if m.enrichFunc != nil {
		return m.enrichFunc(g, repoRoot)
	}
	return &EnrichResult{
		Provider:        m.name,
		Language:        m.languages[0],
		EdgesConfirmed:  5,
		EdgesAdded:      2,
		CoveragePercent: 95.0,
	}, nil
}

func (m *mockProvider) EnrichFile(g graph.Store, repoRoot, filePath string) (*EnrichResult, error) {
	return nil, nil
}

func TestManager_EnrichAll(t *testing.T) {
	logger := zap.NewNop()
	cfg := Config{
		Enabled:  true,
		EagerLSP: true, // router-backed LSP dispatch is opt-in
		Providers: []ProviderConfig{
			{Name: "test-go", Languages: []string{"go"}, Priority: 1, Enabled: true},
		},
	}

	mgr := NewManager(cfg, logger)
	mgr.RegisterProvider(&mockProvider{
		name:      "test-go",
		languages: []string{"go"},
		available: true,
	})

	g := graph.New()
	g.AddNode(&graph.Node{ID: "main.go::main", Kind: graph.KindFunction, Name: "main", FilePath: "main.go", Language: "go"})

	roots := map[string]string{"default": "/tmp/test"}
	results, _, err := mgr.EnrichAll(g, roots, EnrichOptions{})
	require.NoError(t, err)
	assert.Len(t, results, 1)
	assert.Equal(t, "test-go", results[0].Provider)
	assert.Equal(t, 5, results[0].EdgesConfirmed)
}

func TestManagerEnrichFilesPropagatesConfiguredDeadlineToContextBatch(t *testing.T) {
	provider := &contextBatchProvider{mockProvider: &mockProvider{
		name: "go-context", languages: []string{"go"}, available: true,
	}}
	manager := NewManager(Config{
		Enabled:        true,
		TimeoutSeconds: 5,
		Providers: []ProviderConfig{{
			Name: "go-context", Languages: []string{"go"}, Priority: 1, Enabled: true,
		}},
	}, zap.NewNop())
	manager.RegisterProvider(provider)

	_, err := manager.EnrichFiles(graph.New(), "repo", t.TempDir(), "go", []string{"repo/b.go", "repo/a.go", "repo/a.go"})
	require.NoError(t, err)
	require.True(t, provider.hadDeadline, "context-aware batch must inherit the manager deadline")
	remaining := time.Until(provider.deadline)
	require.Positive(t, remaining)
	require.LessOrEqual(t, remaining, 5*time.Second)
	assert.Equal(t, []string{"repo/a.go", "repo/b.go"}, provider.files)
}

func TestManager_PrioritySelection(t *testing.T) {
	logger := zap.NewNop()
	cfg := Config{
		Enabled:  true,
		EagerLSP: true, // router-backed LSP dispatch is opt-in
		Providers: []ProviderConfig{
			{Name: "high-priority", Languages: []string{"go"}, Priority: 1, Enabled: true},
			{Name: "low-priority", Languages: []string{"go"}, Priority: 2, Enabled: true},
		},
	}

	mgr := NewManager(cfg, logger)

	highCalled := false
	lowCalled := false

	mgr.RegisterProvider(&mockProvider{
		name:      "high-priority",
		languages: []string{"go"},
		available: true,
		enrichFunc: func(g graph.Store, root string) (*EnrichResult, error) {
			highCalled = true
			return &EnrichResult{Provider: "high-priority", Language: "go"}, nil
		},
	})
	mgr.RegisterProvider(&mockProvider{
		name:      "low-priority",
		languages: []string{"go"},
		available: true,
		enrichFunc: func(g graph.Store, root string) (*EnrichResult, error) {
			lowCalled = true
			return &EnrichResult{Provider: "low-priority", Language: "go"}, nil
		},
	})

	g := graph.New()
	roots := map[string]string{"default": "/tmp/test"}
	_, _, err := mgr.EnrichAll(g, roots, EnrichOptions{})
	require.NoError(t, err)

	assert.True(t, highCalled, "high-priority provider should run")
	assert.False(t, lowCalled, "low-priority provider should not run")
}

func TestManager_UnavailableProvider(t *testing.T) {
	logger := zap.NewNop()
	cfg := Config{
		Enabled:  true,
		EagerLSP: true, // router-backed LSP dispatch is opt-in
		Providers: []ProviderConfig{
			{Name: "unavailable", Languages: []string{"go"}, Priority: 1, Enabled: true},
		},
	}

	mgr := NewManager(cfg, logger)
	mgr.RegisterProvider(&mockProvider{
		name:      "unavailable",
		languages: []string{"go"},
		available: false,
	})

	g := graph.New()
	roots := map[string]string{"default": "/tmp/test"}
	results, _, err := mgr.EnrichAll(g, roots, EnrichOptions{})
	require.NoError(t, err)
	assert.Len(t, results, 0)
}

func TestManager_Disabled(t *testing.T) {
	logger := zap.NewNop()
	cfg := Config{Enabled: false}

	mgr := NewManager(cfg, logger)
	mgr.RegisterProvider(&mockProvider{
		name:      "test",
		languages: []string{"go"},
		available: true,
	})

	g := graph.New()
	roots := map[string]string{"default": "/tmp/test"}
	results, _, err := mgr.EnrichAll(g, roots, EnrichOptions{})
	require.NoError(t, err)
	assert.Nil(t, results)
}

func TestManager_Close(t *testing.T) {
	logger := zap.NewNop()
	cfg := Config{Enabled: true}

	mgr := NewManager(cfg, logger)
	p := &mockProvider{name: "test", languages: []string{"go"}, available: true}
	mgr.RegisterProvider(p)

	err := mgr.Close()
	require.NoError(t, err)
	assert.True(t, p.closed)
}

// fakeRouter implements LSPRouter for tests so we can validate the
// Manager↔Router contract without spawning real LSP subprocesses.
type fakeRouter struct {
	specs        []string
	available    map[string]bool
	languages    map[string][]string // spec name → languages
	priorities   map[string]int      // spec name → priority
	providers    map[string]Provider
	closeCalls   int
	providerErrs map[string]error
	maxAlive     int
	evictions    uint64
	calls        []string // method-name trace for ordering assertions
}

func (f *fakeRouter) EnabledSpecNames() []string {
	f.calls = append(f.calls, "EnabledSpecNames")
	out := make([]string, len(f.specs))
	copy(out, f.specs)
	return out
}

func (f *fakeRouter) SpecAvailable(name string) bool {
	f.calls = append(f.calls, "SpecAvailable:"+name)
	if f.available == nil {
		// Default-true so tests that don't set the map still get
		// the spec exercised by the router code path.
		return true
	}
	return f.available[name]
}

func (f *fakeRouter) SpecLanguages(name string) []string {
	f.calls = append(f.calls, "SpecLanguages:"+name)
	if langs, ok := f.languages[name]; ok {
		out := make([]string, len(langs))
		copy(out, langs)
		return out
	}
	// Sensible fallback so tests that don't bother setting languages
	// still get a non-empty list (matching mockProvider's "go" default).
	return []string{"go"}
}

func (f *fakeRouter) SpecPriority(name string) int {
	f.calls = append(f.calls, "SpecPriority:"+name)
	if p, ok := f.priorities[name]; ok {
		return p
	}
	return 99
}

func (f *fakeRouter) ProviderForSpec(name string) (Provider, error) {
	f.calls = append(f.calls, "ProviderForSpec:"+name)
	if err, ok := f.providerErrs[name]; ok {
		return nil, err
	}
	p, ok := f.providers[name]
	if !ok {
		return nil, assertionError("no provider for spec " + name)
	}
	return p, nil
}

func (f *fakeRouter) ProviderForSpecWorkspace(name, workspace string) (Provider, error) {
	f.calls = append(f.calls, "ProviderForSpecWorkspace:"+name)
	if err, ok := f.providerErrs[name]; ok {
		return nil, err
	}
	p, ok := f.providers[name]
	if !ok {
		return nil, assertionError("no provider for spec " + name)
	}
	return p, nil
}

func (f *fakeRouter) ReleaseSpecWorkspace(name, workspace string) {
	f.calls = append(f.calls, "ReleaseSpecWorkspace:"+name)
}

// PeekProviderForSpec is the spawn-free census path (backgroundPeekRouter).
func (f *fakeRouter) PeekProviderForSpec(name string) (Provider, error) {
	f.calls = append(f.calls, "PeekProviderForSpec:"+name)
	if err, ok := f.providerErrs[name]; ok {
		return nil, err
	}
	p, ok := f.providers[name]
	if !ok {
		return nil, assertionError("no provider for spec " + name)
	}
	return p, nil
}

func (f *fakeRouter) MaxAlive() int {
	f.calls = append(f.calls, "MaxAlive")
	return f.maxAlive
}

func (f *fakeRouter) SetMaxAlive(n int) {
	f.calls = append(f.calls, "SetMaxAlive")
	f.maxAlive = n
}

func (f *fakeRouter) EvictionCount() uint64 {
	f.calls = append(f.calls, "EvictionCount")
	return f.evictions
}

func (f *fakeRouter) Close() error {
	f.closeCalls++
	return nil
}

type assertionError string

func (e assertionError) Error() string { return string(e) }

// TestManager_LSPRouter_RoundTrip — SetLSPRouter / LSPRouter accessor
// pair returns the same instance.
func TestManager_LSPRouter_RoundTrip(t *testing.T) {
	mgr := NewManager(Config{Enabled: true, EagerLSP: true}, zap.NewNop())
	r := &fakeRouter{}
	mgr.SetLSPRouter(r)
	assert.Same(t, r, mgr.LSPRouter())
}

// TestManager_EnrichAll_RoutesThroughLSPRouter — when a router is
// installed, EnrichAll asks it for each enabled spec and runs Enrich
// against the returned provider exactly once per repo root.
func TestManager_EnrichAll_RoutesThroughLSPRouter(t *testing.T) {
	logger := zap.NewNop()
	// EagerLSP: this test exercises the synchronous router-backed LSP dispatch,
	// which is opt-in (LSP is lazy by default).
	cfg := Config{Enabled: true, EagerLSP: true}

	mgr := NewManager(cfg, logger)

	rsProvider := &mockProvider{
		name:      "lsp-rust-analyzer",
		languages: []string{"rust"},
		available: true,
	}
	r := &fakeRouter{
		specs:     []string{"rust-analyzer"},
		providers: map[string]Provider{"rust-analyzer": rsProvider},
	}
	mgr.SetLSPRouter(r)

	g := graph.New()
	roots := map[string]string{"repo-a": "/tmp/a", "repo-b": "/tmp/b"}
	results, _, err := mgr.EnrichAll(g, roots, EnrichOptions{})
	require.NoError(t, err)
	// Two repos × one router-backed spec = two enrichment results.
	assert.Len(t, results, 2)
	assert.Equal(t, "lsp-rust-analyzer", results[0].Provider)
}

// TestManager_EnrichAll_SkipsCoveredLanguages — when an eager
// provider already covers the same language a router-backed spec
// serves, the router-backed enrichment is skipped (priority semantics
// preserved).
func TestManager_EnrichAll_SkipsCoveredLanguages(t *testing.T) {
	logger := zap.NewNop()
	cfg := Config{
		Enabled:  true,
		EagerLSP: true, // router-backed LSP dispatch is opt-in
		Providers: []ProviderConfig{
			{Name: "eager-go", Languages: []string{"go"}, Priority: 1, Enabled: true},
		},
	}

	mgr := NewManager(cfg, logger)
	mgr.RegisterProvider(&mockProvider{name: "eager-go", languages: []string{"go"}, available: true})

	routerProvider := &mockProvider{name: "lsp-gopls", languages: []string{"go"}, available: true}
	r := &fakeRouter{
		specs:     []string{"gopls"},
		providers: map[string]Provider{"gopls": routerProvider},
	}
	mgr.SetLSPRouter(r)

	g := graph.New()
	roots := map[string]string{"default": "/tmp/test"}
	results, _, err := mgr.EnrichAll(g, roots, EnrichOptions{})
	require.NoError(t, err)
	// Eager provider runs (1 repo), router-backed gopls is skipped
	// because go is already covered.
	assert.Len(t, results, 1)
	assert.Equal(t, "eager-go", results[0].Provider)
}

// TestManager_EnrichAll_GatesAbsentLanguages — on a populated repo (positive
// presence evidence), a router-backed spec whose language the repo does NOT
// contain is never spawned (ProviderForSpec is not called for it), while a
// provider whose language IS present still runs.
func TestManager_EnrichAll_GatesAbsentLanguages(t *testing.T) {
	logger := zap.NewNop()
	cfg := Config{
		Enabled:  true,
		EagerLSP: true, // router-backed LSP dispatch is opt-in
		Providers: []ProviderConfig{
			{Name: "eager-go", Languages: []string{"go"}, Priority: 1, Enabled: true},
		},
	}
	mgr := NewManager(cfg, logger)
	mgr.RegisterProvider(&mockProvider{name: "eager-go", languages: []string{"go"}, available: true})

	// A router spec serving rust — a language this repo does not contain.
	rustProvider := &mockProvider{name: "lsp-rust-analyzer", languages: []string{"rust"}, available: true}
	r := &fakeRouter{
		specs:     []string{"rust-analyzer"},
		available: map[string]bool{"rust-analyzer": true},
		languages: map[string][]string{"rust-analyzer": {"rust"}},
		providers: map[string]Provider{"rust-analyzer": rustProvider},
	}
	mgr.SetLSPRouter(r)

	// Populated repo: one Go node tagged with the roots-key prefix, no Rust.
	g := graph.New()
	g.AddNode(&graph.Node{ID: "myrepo/main.go::main", Kind: graph.KindFunction, Name: "main", FilePath: "myrepo/main.go", Language: "go", RepoPrefix: "myrepo"})

	roots := map[string]string{"myrepo": "/tmp/r"}
	results, _, err := mgr.EnrichAll(g, roots, EnrichOptions{})
	require.NoError(t, err)

	// The eager Go provider runs; the Rust spec is gated out before any spawn.
	require.Len(t, results, 1)
	assert.Equal(t, "eager-go", results[0].Provider)
	for _, c := range r.calls {
		if c == "ProviderForSpec:rust-analyzer" {
			t.Fatalf("rust spec should be gated out (repo has no rust nodes), but ProviderForSpec was called — calls: %v", r.calls)
		}
	}
}

// TestManager_HasProviders_RouterOnly — a router-only setup with at
// least one available spec returns true, and the check does NOT
// trigger ProviderForSpec (which would lazy-spawn a real LSP).
func TestManager_HasProviders_RouterOnly(t *testing.T) {
	mgr := NewManager(Config{Enabled: true, EagerLSP: true}, zap.NewNop())
	r := &fakeRouter{
		specs:     []string{"rust-analyzer"},
		available: map[string]bool{"rust-analyzer": true},
	}
	mgr.SetLSPRouter(r)

	assert.True(t, mgr.HasProviders())
	for _, c := range r.calls {
		if c == "ProviderForSpec:rust-analyzer" {
			t.Fatalf("HasProviders should not lazy-spawn — calls: %v", r.calls)
		}
	}
}

// TestManager_HasProviders_NoneAvailable — router enabled but no spec
// available returns false (and again does not spawn).
func TestManager_HasProviders_NoneAvailable(t *testing.T) {
	mgr := NewManager(Config{Enabled: true, EagerLSP: true}, zap.NewNop())
	r := &fakeRouter{
		specs:     []string{"rust-analyzer"},
		available: map[string]bool{"rust-analyzer": false},
	}
	mgr.SetLSPRouter(r)
	assert.False(t, mgr.HasProviders())
}

// TestManager_Close_ShutsDownRouter — Manager.Close cascades into
// LSPRouter.Close exactly once.
func TestManager_Close_ShutsDownRouter(t *testing.T) {
	mgr := NewManager(Config{Enabled: true, EagerLSP: true}, zap.NewNop())
	r := &fakeRouter{}
	mgr.SetLSPRouter(r)
	require.NoError(t, mgr.Close())
	assert.Equal(t, 1, r.closeCalls)
}

// TestManager_EnrichAll_ArbitratesRouterDupes — two router-backed
// specs serving the same language: only the lower-priority spec runs.
// The losing spec is never spawned (ProviderForSpec is not called for
// it).
func TestManager_EnrichAll_ArbitratesRouterDupes(t *testing.T) {
	logger := zap.NewNop()
	mgr := NewManager(Config{Enabled: true, EagerLSP: true}, logger)

	pyrightProvider := &mockProvider{name: "lsp-pyright", languages: []string{"python"}, available: true}
	jediProvider := &mockProvider{name: "lsp-jedi", languages: []string{"python"}, available: true}

	r := &fakeRouter{
		specs:      []string{"pyright", "jedi-language-server"},
		available:  map[string]bool{"pyright": true, "jedi-language-server": true},
		languages:  map[string][]string{"pyright": {"python"}, "jedi-language-server": {"python"}},
		priorities: map[string]int{"pyright": 5, "jedi-language-server": 6},
		providers:  map[string]Provider{"pyright": pyrightProvider, "jedi-language-server": jediProvider},
	}
	mgr.SetLSPRouter(r)

	g := graph.New()
	roots := map[string]string{"default": "/tmp/test"}
	results, _, err := mgr.EnrichAll(g, roots, EnrichOptions{})
	require.NoError(t, err)
	require.Len(t, results, 1, "exactly one provider should run for python")
	assert.Equal(t, "lsp-pyright", results[0].Provider, "pyright wins the lower-priority arbitration")

	// Spawn must NOT have happened for the loser.
	for _, c := range r.calls {
		if c == "ProviderForSpec:jedi-language-server" {
			t.Fatalf("loser spec was spawned — calls: %v", r.calls)
		}
	}
}

// TestManager_EnrichAll_RouterTieBreakerByName — equal priorities tie-
// break alphabetically by spec name (deterministic).
func TestManager_EnrichAll_RouterTieBreakerByName(t *testing.T) {
	mgr := NewManager(Config{Enabled: true, EagerLSP: true}, zap.NewNop())
	pa := &mockProvider{name: "lsp-aaa", languages: []string{"go"}, available: true}
	pb := &mockProvider{name: "lsp-bbb", languages: []string{"go"}, available: true}
	r := &fakeRouter{
		specs:      []string{"bbb", "aaa"},
		available:  map[string]bool{"aaa": true, "bbb": true},
		languages:  map[string][]string{"aaa": {"go"}, "bbb": {"go"}},
		priorities: map[string]int{"aaa": 5, "bbb": 5},
		providers:  map[string]Provider{"aaa": pa, "bbb": pb},
	}
	mgr.SetLSPRouter(r)

	results, _, err := mgr.EnrichAll(graph.New(), map[string]string{"default": "/tmp/x"}, EnrichOptions{})
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "lsp-aaa", results[0].Provider)
}

// TestManager_EnrichAll_RouterDedupAcrossLanguages — one spec serving
// two languages runs Enrich exactly once, not once per language.
func TestManager_EnrichAll_RouterDedupAcrossLanguages(t *testing.T) {
	mgr := NewManager(Config{Enabled: true, EagerLSP: true}, zap.NewNop())
	tsProvider := &mockProvider{
		name:      "lsp-typescript-language-server",
		languages: []string{"typescript", "javascript"},
		available: true,
	}
	r := &fakeRouter{
		specs:      []string{"typescript-language-server"},
		available:  map[string]bool{"typescript-language-server": true},
		languages:  map[string][]string{"typescript-language-server": {"typescript", "javascript"}},
		priorities: map[string]int{"typescript-language-server": 5},
		providers:  map[string]Provider{"typescript-language-server": tsProvider},
	}
	mgr.SetLSPRouter(r)

	results, _, err := mgr.EnrichAll(graph.New(), map[string]string{"default": "/tmp/x"}, EnrichOptions{})
	require.NoError(t, err)
	assert.Len(t, results, 1, "spec serving 2 languages should still run Enrich once")
}

// TestManager_Stats_IncludesRouterSpecs — Stats() reports
// router-enabled LSP specs alongside eagerly-registered providers.
// Router specs show up as "lsp-<spec>" with status driven by
// SpecAvailable, no spawn triggered.
func TestManager_Stats_IncludesRouterSpecs(t *testing.T) {
	mgr := NewManager(Config{Enabled: true, EagerLSP: true}, zap.NewNop())
	mgr.RegisterProvider(&mockProvider{
		name:      "scip-go",
		languages: []string{"go"},
		available: true,
	})
	r := &fakeRouter{
		specs:     []string{"rust-analyzer", "pyright"},
		available: map[string]bool{"rust-analyzer": true, "pyright": false},
		languages: map[string][]string{
			"rust-analyzer": {"rust"},
			"pyright":       {"python"},
		},
	}
	mgr.SetLSPRouter(r)

	stats := mgr.Stats()
	byName := make(map[string]string)
	for _, s := range stats {
		byName[s.Name+"/"+s.Language] = s.Status
	}
	assert.Equal(t, "ready", byName["scip-go/go"])
	assert.Equal(t, "ready", byName["lsp-rust-analyzer/rust"])
	assert.Equal(t, "unavailable", byName["lsp-pyright/python"])

	// Router specs should not have triggered a spawn.
	for _, c := range r.calls {
		if c == "ProviderForSpec:rust-analyzer" || c == "ProviderForSpec:pyright" {
			t.Fatalf("Stats spawned an LSP provider — calls: %v", r.calls)
		}
	}
}

// TestManager_ConfigPriority_Override — config Priority overrides the
// spec's built-in default in arbitration. User boosts jedi to win
// over pyright.
func TestManager_ConfigPriority_Override(t *testing.T) {
	cfg := Config{
		Enabled:  true,
		EagerLSP: true, // router-backed LSP dispatch is opt-in
		Providers: []ProviderConfig{
			{Name: "jedi-language-server", Priority: 1, Enabled: true},
			{Name: "pyright", Priority: 5, Enabled: true},
		},
	}
	mgr := NewManager(cfg, zap.NewNop())
	pyrightProvider := &mockProvider{name: "lsp-pyright", languages: []string{"python"}, available: true}
	jediProvider := &mockProvider{name: "lsp-jedi", languages: []string{"python"}, available: true}
	r := &fakeRouter{
		specs:      []string{"pyright", "jedi-language-server"},
		available:  map[string]bool{"pyright": true, "jedi-language-server": true},
		languages:  map[string][]string{"pyright": {"python"}, "jedi-language-server": {"python"}},
		priorities: map[string]int{"pyright": 5, "jedi-language-server": 6},
		providers:  map[string]Provider{"pyright": pyrightProvider, "jedi-language-server": jediProvider},
	}
	mgr.SetLSPRouter(r)

	results, _, err := mgr.EnrichAll(graph.New(), map[string]string{"default": "/tmp/x"}, EnrichOptions{})
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "lsp-jedi", results[0].Provider, "config priority should beat spec default")
}

func TestManager_Stats(t *testing.T) {
	logger := zap.NewNop()
	cfg := Config{
		Enabled:  true,
		EagerLSP: true, // router-backed LSP dispatch is opt-in
		Providers: []ProviderConfig{
			{Name: "test-go", Languages: []string{"go"}, Priority: 1, Enabled: true},
		},
	}

	mgr := NewManager(cfg, logger)
	mgr.RegisterProvider(&mockProvider{
		name:      "test-go",
		languages: []string{"go"},
		available: true,
	})

	stats := mgr.Stats()
	require.Len(t, stats, 1)
	assert.Equal(t, "test-go", stats[0].Name)
	assert.Equal(t, "go", stats[0].Language)
	assert.Equal(t, "ready", stats[0].Status)
}
