package indexer

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/text/unicode/norm"
)

func TestCanonicalRepositoryMutationPathsDeduplicatesAliases(t *testing.T) {
	root := t.TempDir()
	asciiPath := filepath.Join(root, "a.go")
	unicodeName := "caf\u00e9.go"
	unicodePath := filepath.Join(root, unicodeName)
	require.NoError(t, os.WriteFile(asciiPath, []byte("package p\n"), 0o644))
	require.NoError(t, os.WriteFile(unicodePath, []byte("package p\n"), 0o644))

	paths, err := canonicalRepositoryMutationPaths(root, []string{
		"a.go",
		filepath.Join(".", "a.go"),
		asciiPath,
		unicodeName,
		norm.NFD.String(unicodeName),
		unicodePath,
	})
	require.NoError(t, err)
	require.Equal(t, []string{"a.go", unicodeName}, paths)
}

func TestCanonicalRepositoryMutationPathsValidation(t *testing.T) {
	root := t.TempDir()

	t.Run("missing in root is a deletion scope", func(t *testing.T) {
		paths, err := canonicalRepositoryMutationPaths(root, []string{"deleted.go"})
		require.NoError(t, err)
		require.Equal(t, []string{"deleted.go"}, paths)
	})

	t.Run("explicit root is full", func(t *testing.T) {
		paths, err := canonicalRepositoryMutationPaths(root, []string{"."})
		require.NoError(t, err)
		require.Nil(t, paths)
	})

	t.Run("empty explicit path is rejected", func(t *testing.T) {
		_, err := canonicalRepositoryMutationPaths(root, []string{""})
		require.ErrorIs(t, err, errEmptyRepositoryMutationPath)
	})

	t.Run("outside root is rejected", func(t *testing.T) {
		_, err := canonicalRepositoryMutationPaths(root, []string{filepath.Join(root, "..", "outside.go")})
		require.ErrorContains(t, err, "outside repository root")
	})

	t.Run("invalid stat is rejected", func(t *testing.T) {
		_, err := canonicalRepositoryMutationPaths(root, []string{"bad\x00.go"})
		// A NUL byte is rejected at a different stage per platform, and both
		// stages are the caller-local refusal this pins. POSIX absolutises the
		// path by string manipulation alone and only the stat syscall refuses
		// it; Windows resolves through syscall.FullPath inside filepath.Abs,
		// which rejects the NUL one step earlier.
		want := "repository mutation stat"
		if runtime.GOOS == "windows" {
			want = "repository mutation path"
		}
		require.ErrorContains(t, err, want)
	})
}

func TestIncrementalReindexPathsValidatesBeforeCoordinatorAdmission(t *testing.T) {
	root := t.TempDir()
	filePath := filepath.Join(root, "a.go")
	require.NoError(t, os.WriteFile(filePath, []byte("package p\n"), 0o644))

	executed := make(chan []string, 1)
	coordinator := newRepositoryMutationCoordinator(func(paths []string) (*IndexResult, error) {
		executed <- append([]string(nil), paths...)
		return &IndexResult{}, nil
	})
	idx := &Indexer{rootPath: root}
	idx.attachRepositoryMutationCoordinator(coordinator)

	_, err := idx.IncrementalReindexPaths(root, []string{filepath.Join(root, "..", "outside.go")})
	require.Error(t, err)
	stats := coordinator.stats()
	require.Zero(t, stats.RequestedGeneration)
	require.Zero(t, stats.PendingPaths)

	_, err = idx.IncrementalReindexPaths(root, []string{"a.go", filePath})
	require.NoError(t, err)
	require.Equal(t, []string{"a.go"}, <-executed)
	require.Equal(t, uint64(1), coordinator.stats().RequestedGeneration)

	_, err = coordinator.reconcile(context.Background(), []string{""})
	require.ErrorIs(t, err, errEmptyRepositoryMutationPath)
	require.Equal(t, uint64(1), coordinator.stats().RequestedGeneration)
}
