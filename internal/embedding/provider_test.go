package embedding

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/knights-analytics/hugot"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStaticProvider_Embed(t *testing.T) {
	p, err := NewStaticProvider()
	require.NoError(t, err)

	// Inject test vectors.
	p.SetVectors(map[string][]float32{
		"validate": {1, 0, 0},
		"check":    {0.9, 0.1, 0},
		"token":    {0, 1, 0},
		"auth":     {0, 0.9, 0.1},
	})

	ctx := context.Background()

	// "validate token" should produce a non-zero vector.
	vec, err := p.Embed(ctx, "validateToken")
	require.NoError(t, err)
	assert.Len(t, vec, 3)
	assert.NotEqual(t, float32(0), vec[0], "should have non-zero components")

	// "check auth" should be similar to "validate token".
	vec2, err := p.Embed(ctx, "checkAuth")
	require.NoError(t, err)
	assert.Len(t, vec2, 3)

	// Both should be non-zero (found words in vocabulary).
	hasNonZero := false
	for _, v := range vec2 {
		if v != 0 {
			hasNonZero = true
			break
		}
	}
	assert.True(t, hasNonZero, "should find words in vocabulary")
}

func TestStaticProvider_EmbedBatch(t *testing.T) {
	p, err := NewStaticProvider()
	require.NoError(t, err)
	p.SetVectors(map[string][]float32{
		"hello": {1, 0},
		"world": {0, 1},
	})

	ctx := context.Background()
	vecs, err := p.EmbedBatch(ctx, []string{"hello", "world", "unknown"})
	require.NoError(t, err)
	assert.Len(t, vecs, 3)
}

func TestStaticProvider_UnknownWords(t *testing.T) {
	p, err := NewStaticProvider()
	require.NoError(t, err)
	p.SetVectors(map[string][]float32{
		"known": {1, 0},
	})

	ctx := context.Background()
	vec, err := p.Embed(ctx, "completelyunknownword")
	require.NoError(t, err)
	// Should return zero vector for unknown words.
	for _, v := range vec {
		assert.Equal(t, float32(0), v)
	}
}

func TestNopProvider(t *testing.T) {
	var p NopProvider
	ctx := context.Background()

	_, err := p.Embed(ctx, "test")
	assert.ErrorIs(t, err, ErrDisabled)

	_, err = p.EmbedBatch(ctx, []string{"test"})
	assert.ErrorIs(t, err, ErrDisabled)

	assert.Equal(t, 0, p.Dimensions())
	assert.NoError(t, p.Close())
}

func TestStaticProvider_SemanticSimilarity(t *testing.T) {
	p, err := NewStaticProvider()
	require.NoError(t, err)
	require.Greater(t, len(p.vectors), 1000, "should have loaded GloVe vectors")

	ctx := context.Background()

	// "validate token" and "check authentication" should produce non-zero, similar vectors.
	vec1, err := p.Embed(ctx, "validate token")
	require.NoError(t, err)

	vec2, err := p.Embed(ctx, "check authentication")
	require.NoError(t, err)

	// Both should be non-zero.
	nonZero1 := false
	for _, v := range vec1 {
		if v != 0 {
			nonZero1 = true
			break
		}
	}
	assert.True(t, nonZero1, "validate token should produce non-zero vector")

	// Compute cosine similarity.
	var dot float64
	for i := range vec1 {
		dot += float64(vec1[i]) * float64(vec2[i])
	}
	// Both vectors are normalized, so dot product = cosine similarity.
	assert.Greater(t, dot, 0.3, "semantically similar queries should have cosine > 0.3")
}

// testDownloadBudget bounds the cold model download this test triggers.
// The fetch takes ~26s on a healthy runner, so three minutes is many
// times the honest cost while still failing fast: without a bound, a
// Hugging Face transfer that stalls parks the whole package until the
// go test timeout kills it (a 45-minute CI kill, observed on Windows).
const testDownloadBudget = 3 * time.Minute

func TestNewLocalProvider_ReturnsWorkingProvider(t *testing.T) {
	if raceEnabled {
		// NewLocalProvider falls through to Hugot on a fresh machine,
		// which triggers hugot.DownloadModel → go-huggingface's
		// DownloadFilesCtx. That function has a data race on a shared
		// loop variable in its internal worker goroutines (upstream
		// bug; reproduces with every `hub.Repo.DownloadFiles` call).
		// We don't own that code and the race doesn't affect our own
		// logic, so skip under -race rather than bundle a vendored
		// patch. Non-race builds still exercise the full fallback.
		t.Skip("upstream data race in go-huggingface/hub.DownloadFilesCtx — skipping under -race")
	}
	t.Setenv(embeddingDownloadBudgetEnv, testDownloadBudget.String())

	// The report variant is the same chain NewLocalProvider runs — it
	// just keeps the per-backend errors instead of discarding them, and
	// those errors are the only place a stalled download is visible: a
	// Hugot that gave up on the network falls through to the static
	// provider, so the constructor succeeds either way.
	p, report := NewLocalProviderWithReport()
	for _, attempt := range report.Attempts {
		if errors.Is(attempt.Err, context.DeadlineExceeded) {
			t.Skipf("embedding model %s did not download within %s — network conditions, not a defect (%s backend: %v)",
				miniLMRepo, testDownloadBudget, attempt.Backend, attempt.Err)
		}
	}
	require.NotNil(t, p, "no backend constructed, not even the static fallback: %+v", report.Attempts)
	defer p.Close()

	// Default build walks ONNX → GoMLX → Hugot → Static and returns
	// the first that initialises. With Hugot's onnx file pinned to
	// onnx/model.onnx, Hugot succeeds when the model is cached or
	// the network is reachable. Either is fine — the invariant is
	// "NewLocalProvider returns a working provider."
	assert.Greater(t, p.Dimensions(), 0, "provider must report positive dimensions")
}

// TestDownloadHugotModelAbandonsAStalledFetch pins the watchdog the test
// above leans on. Nothing inside the download honours a deadline —
// hugot.DownloadModel hands its context only to the post-fetch copy, and
// go-huggingface's worker pool waits on a context-blind sync.Cond — so
// the budget has to expire out in our own code. The stub stands in for a
// transfer that never progresses.
func TestDownloadHugotModelAbandonsAStalledFetch(t *testing.T) {
	stalled := make(chan struct{})
	defer close(stalled)
	restore := hugotDownloadModel
	t.Cleanup(func() { hugotDownloadModel = restore })
	hugotDownloadModel = func(context.Context, string, string, hugot.DownloadOptions) (string, error) {
		<-stalled // context-blind, exactly like the upstream semaphore wait
		return "", nil
	}
	t.Setenv(embeddingDownloadBudgetEnv, "50ms")

	spec, ok := LookupHugotVariant(DefaultHugotVariant)
	require.True(t, ok)
	start := time.Now()
	path, err := downloadHugotModel(spec, t.TempDir(), hugot.NewDownloadOptions())

	require.Error(t, err, "a stalled download returned a path (%q) instead of giving up", path)
	require.ErrorIs(t, err, context.DeadlineExceeded, "the error must name the deadline so callers can tell a stall from a broken model")
	assert.Less(t, time.Since(start), 10*time.Second, "the budget did not bind")
}

// TestDownloadBudgetHonoursTheEnvOverride pins both halves of the budget
// resolution: a generous default for a real cold fetch, and an override
// a test or an operator on a slow link can dial in.
func TestDownloadBudgetHonoursTheEnvOverride(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		want  time.Duration
	}{
		{"unset", "", defaultDownloadBudget},
		{"override", "90s", 90 * time.Second},
		{"unparseable", "soon", defaultDownloadBudget},
		{"zero", "0s", defaultDownloadBudget},
		{"negative", "-1m", defaultDownloadBudget},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(embeddingDownloadBudgetEnv, tc.value)
			assert.Equal(t, tc.want, downloadBudget())
		})
	}
}

func TestTokenizeForEmbedding(t *testing.T) {
	tests := []struct {
		input    string
		expected []string
	}{
		{"validateToken", []string{"validate", "token"}},
		{"get_user_by_id", []string{"get", "user", "by", "id"}},
		{"internal/auth/service.go", []string{"internal", "auth", "service", "go"}},
		{"a b", nil}, // single chars dropped
		{"HandleRequest", []string{"handle", "request"}},
	}
	for _, tt := range tests {
		got := tokenizeForEmbedding(tt.input)
		assert.Equal(t, tt.expected, got, "tokenize(%q)", tt.input)
	}
}
