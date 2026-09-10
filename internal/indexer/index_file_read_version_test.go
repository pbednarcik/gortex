package indexer

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

type blockingExtractor struct {
	parser.Extractor
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (e *blockingExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
	e.once.Do(func() { close(e.entered) })
	<-e.release
	return e.Extractor.Extract(filePath, src)
}

func TestIndexFileDoesNotStampWriteThatLandsDuringParse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	writeTestFile(t, path, "package main\n\nfunc Initial() {}\n")

	g := graph.New()
	registry := parser.NewRegistry()
	goExtractor := languages.NewGoExtractor()
	registry.Register(goExtractor)
	cfg := config.Default()
	cfg.Index.Workers = 1
	idx := New(g, registry, cfg.Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)

	initial := indexedFileMtime(t, idx, path)
	firstMtime := initial.Add(time.Second)
	secondMtime := initial.Add(2 * time.Second)
	writeTestFile(t, path, "package main\n\nfunc First() {}\n")
	require.NoError(t, os.Chtimes(path, firstMtime, firstMtime))

	blocked := &blockingExtractor{
		Extractor: goExtractor,
		entered:   make(chan struct{}),
		release:   make(chan struct{}),
	}
	registry.Register(blocked)
	done := make(chan error, 1)
	go func() { done <- idx.IndexFile(path) }()
	<-blocked.entered

	// The first parse owns First's bytes. Land a second write while extraction
	// is blocked, then release it. The graph may commit First, but its receipt
	// must not claim Second's newer disk version.
	writeTestFile(t, path, "package main\n\nfunc Second() {}\n")
	require.NoError(t, os.Chtimes(path, secondMtime, secondMtime))
	close(blocked.release)
	require.ErrorIs(t, <-done, errFileVersionChanged)

	key := idx.relKey(path)
	require.NotEqual(t, secondMtime.UnixNano(), idx.FileMtimes()[key])
	require.Equal(t, []string{"First"}, watcherSymbolNames(g.GetFileNodes(idx.relKey(path))))

	registry.Register(goExtractor)
	require.NoError(t, idx.IndexFile(path))
	require.Equal(t, secondMtime.UnixNano(), idx.FileMtimes()[key])
	require.Equal(t, []string{"Second"}, watcherSymbolNames(g.GetFileNodes(idx.relKey(path))))
}

func TestIndexFileSyntheticSkipsStampAcceptedReadVersion(t *testing.T) {
	tests := []struct {
		name      string
		fileName  string
		content   string
		configure func(*parser.Registry, *config.Config)
	}{
		{
			name: "max size", fileName: "main.go", content: "package main\n\nfunc TooLarge() {}\n",
			configure: func(registry *parser.Registry, cfg *config.Config) {
				registry.Register(languages.NewGoExtractor())
				cfg.Index.MaxFileSize = 1
			},
		},
		{
			name: "content admission", fileName: "data.parquet", content: "synthetic parquet bytes\n",
			configure: func(registry *parser.Registry, _ *config.Config) {
				registry.Register(languages.NewDataAssetExtractor())
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, tt.fileName)
			writeTestFile(t, path, tt.content)
			mtime := time.Unix(1_700_000_000, 123_000_000)
			require.NoError(t, os.Chtimes(path, mtime, mtime))
			info, err := os.Stat(path)
			require.NoError(t, err)

			registry := parser.NewRegistry()
			cfg := config.Default()
			tt.configure(registry, cfg)
			idx := New(graph.New(), registry, cfg.Index, zap.NewNop())
			idx.SetRootPath(dir)
			require.NoError(t, idx.IndexFile(path))
			require.Equal(t, info.ModTime().UnixNano(), idx.FileMtimes()[idx.relKey(path)])
		})
	}
}
