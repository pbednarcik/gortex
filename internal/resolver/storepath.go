package resolver

import (
	"path"
	"path/filepath"
)

// filePathDir returns the parent directory of a graph file path, always
// '/'-joined.
//
// PURPOSE: every directory key this package builds or consults is
// slash-separated — dirIndex / lastDirIndex, the import closure in
// buildImportClosureFiltered, dirMatchesImport's `strings.HasSuffix(
// importPath, "/"+dir)` test, the byDir name index. filepath.Dir cannot
// derive such a key: on Windows it re-Cleans its result with the native
// separator, so "packages/logger/index.ts" yields "packages\logger", a
// spelling no slash-keyed index and no import-path suffix test can match.
// The whole cascade then silently misses and imports fall through to
// `external::`/`stdlib::` stubs.
//
// RATIONALE: path.Dir over the slash-folded path is byte-identical to
// filepath.Dir on POSIX — including the "." it returns for a bare
// filename and for the empty string — so this is a no-op there, and it
// produces the same slash key on Windows. ToSlash first because an
// indexed path is repoPrefix + '/' + the indexing machine's native
// relative path (see internal/graphpath), i.e. mixed `repo/dir\file` on
// Windows.
//
// KEYWORDS: windows, separator, dir-key, slash-contract, import-closure
func filePathDir(p string) string {
	return path.Dir(filepath.ToSlash(p))
}
