package indexer

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer/source"
)

func TestParseErrorsClearRecoveredOrDeletedFullIndexFailures(t *testing.T) {
	for _, mode := range []string{"incremental", "direct", "deleted"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			deniedPath := filepath.Join(root, "denied.go")
			for _, name := range []string{"denied.go", "blocked.go"} {
				require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte("package p\nfunc Keep() {}\n"), 0o600))
			}
			fsSource, err := source.NewFilesystemSource(root)
			require.NoError(t, err)
			t.Cleanup(func() { _ = fsSource.Close() })
			idx := newContentSourceIndexer(t, graph.New())
			idx.SetContentSource(&failingIndexSource{
				ContentSource: fsSource, opens: map[string]int{},
				errs: map[string][]error{"denied.go": {syscall.EPERM}, "blocked.go": {syscall.EACCES}},
			})
			_, err = idx.IndexCtx(context.Background(), root)
			require.NoError(t, err)
			before := idx.ParseErrors()
			require.Len(t, before, 2)
			var blocked IndexError
			for _, entry := range before {
				if filepath.Base(entry.FilePath) == "blocked.go" {
					blocked = entry
				}
			}
			require.NotEmpty(t, blocked.FilePath)
			idx.SetContentSource(&failingIndexSource{
				ContentSource: fsSource, opens: map[string]int{},
				errs: map[string][]error{"blocked.go": {syscall.EACCES}},
			})

			switch mode {
			case "direct":
				require.NoError(t, idx.IndexFile(deniedPath))
			case "deleted":
				require.NoError(t, os.Remove(deniedPath))
				result, err := idx.IncrementalReindexPaths(root, nil)
				require.NoError(t, err)
				require.Equal(t, 1, result.DeletedFileCount)
			default:
				_, err := idx.IncrementalReindexPaths(root, nil)
				require.NoError(t, err)
			}
			require.Equal(t, []IndexError{blocked}, idx.ParseErrors(), "recovery must clear only the resolved error")
		})
	}
}

func TestParseErrorsCleanupNormalizesDirectoryAndFailedPaths(t *testing.T) {
	root := t.TempDir()
	idx := newContentSourceIndexer(t, graph.New())
	idx.SetRootPath(root)
	stillFailed := IndexError{FilePath: "blocked/still.go", Error: "permission denied"}
	unrelated := IndexError{FilePath: "unrelated.go", Error: "parse failed"}
	idx.parseErrors = []IndexError{
		{FilePath: "blocked", Error: "readdir: permission denied"},
		stillFailed,
		{FilePath: "deleted.go", Error: "open: permission denied"},
		unrelated,
	}
	idx.clearRecoveredParseErrors(
		[]string{filepath.Join(root, "blocked"), filepath.Join(root, "blocked", "still.go")},
		[]string{filepath.Join(root, "blocked", "still.go")},
		[]string{filepath.Join(root, "deleted.go")},
	)
	require.Equal(t, []IndexError{stillFailed, unrelated}, idx.ParseErrors())
}

func TestParseErrorsReturnsIndependentSnapshot(t *testing.T) {
	idx := newContentSourceIndexer(t, graph.New())
	idx.parseErrors = []IndexError{{FilePath: "blocked.go", Error: "permission denied"}}
	snapshot := idx.ParseErrors()
	require.Len(t, snapshot, 1)
	snapshot[0].Error = "changed by caller"
	require.Equal(t, "permission denied", idx.ParseErrors()[0].Error)
}
