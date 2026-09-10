package pathkey

import (
	"os"
	"path/filepath"
	"testing"
)

// withCaseInsensitive flips the process-global CaseInsensitivePaths for
// the duration of a test and restores it on cleanup. Tests that call it
// must not run in parallel.
func withCaseInsensitive(t *testing.T, v bool) {
	t.Helper()
	prev := CaseInsensitivePaths
	CaseInsensitivePaths = v
	t.Cleanup(func() { CaseInsensitivePaths = prev })
}

func TestFoldPath_CaseInsensitiveLowersRemainder(t *testing.T) {
	// With ci=true, two casings of the same path fold to one key.
	a := foldPath("/Users/me/Documents/Project", true)
	b := foldPath("/Users/me/documents/project", true)
	if a != b {
		t.Fatalf("foldPath ci=true did not converge: %q vs %q", a, b)
	}
}

func TestFoldPath_CaseSensitiveKeepsRemainder(t *testing.T) {
	// With ci=false, differing casings stay distinct.
	a := foldPath("/Users/me/Documents", false)
	b := foldPath("/Users/me/documents", false)
	if a == b {
		t.Fatalf("foldPath ci=false collapsed distinct casings: both %q", a)
	}
}

func TestFoldPath_UppercasesDriveLetter(t *testing.T) {
	// Windows drive letters fold identically regardless of case, even
	// when the rest of the path is compared case-sensitively.
	for _, ci := range []bool{false, true} {
		a := foldPath(`c:\work\git\myrepo`, ci)
		b := foldPath(`C:\work\git\myrepo`, ci)
		if a != b {
			t.Fatalf("foldPath ci=%v did not fold drive-letter case: %q vs %q", ci, a, b)
		}
	}
}

func TestFoldPath_CleansRedundantSegments(t *testing.T) {
	// foldPath canonicalizes with filepath.Clean, which also rewrites
	// separators to the host's native ones — so both the input and the
	// expectation are spelled natively (on Windows "/Users/me/project"
	// folds to `\Users\me\project`).
	got := foldPath(filepath.FromSlash("/Users/me/./project/../project"), false)
	want := filepath.FromSlash("/Users/me/project")
	if got != want {
		t.Fatalf("foldPath did not clean: got %q want %q", got, want)
	}
}

func TestFoldPath_FoldsNFDBeforeCasing(t *testing.T) {
	// The NFC fold runs before case folding, so an NFD macOS spelling
	// and an NFC git spelling of the same accented path converge.
	a := foldPath("/x/"+cafeNFD, true)
	b := foldPath("/x/"+cafeNFC, true)
	if a != b {
		t.Fatalf("foldPath did not reconcile NFD vs NFC: %q vs %q", a, b)
	}
}

func TestEqualPaths_CaseVariants(t *testing.T) {
	withCaseInsensitive(t, true)
	if !EqualPaths("/Users/me/Documents/Project", "/Users/me/documents/project") {
		t.Fatal("EqualPaths should treat case variants as equal when case-insensitive")
	}
}

func TestEqualPaths_CaseSensitiveRejects(t *testing.T) {
	withCaseInsensitive(t, false)
	if EqualPaths("/Users/me/Documents", "/Users/me/documents") {
		t.Fatal("EqualPaths must keep case variants distinct when case-sensitive")
	}
}

func TestEqualPaths_ByteEqualFastPath(t *testing.T) {
	withCaseInsensitive(t, false)
	if !EqualPaths("/a/b/c", "/a/b/c") {
		t.Fatal("EqualPaths byte-equal fast path failed")
	}
}

func TestEqualPaths_DistinctPaths(t *testing.T) {
	withCaseInsensitive(t, true)
	if EqualPaths("/Users/me/alpha", "/Users/me/beta") {
		t.Fatal("EqualPaths reported genuinely distinct paths as equal")
	}
}

func TestHasPathPrefix_RootItself(t *testing.T) {
	withCaseInsensitive(t, true)
	if !HasPathPrefix("/Users/me/Repo", "/Users/me/repo") {
		t.Fatal("a path is a prefix of itself (case-insensitive)")
	}
}

func TestHasPathPrefix_ChildUnderRoot(t *testing.T) {
	withCaseInsensitive(t, true)
	if !HasPathPrefix("/Users/me/Repo/pkg/file.go", "/Users/me/repo") {
		t.Fatal("child path should be under root regardless of case")
	}
}

func TestHasPathPrefix_ComponentBoundary(t *testing.T) {
	withCaseInsensitive(t, false)
	// A shared textual prefix that is not a path-component prefix must
	// not match: "/Users/me/Doc" is NOT under "/Users/me/Documents".
	if HasPathPrefix("/Users/me/Documents", "/Users/me/Doc") {
		t.Fatal("prefix-but-not-component must not match")
	}
	if HasPathPrefix("/Users/me/Doc", "/Users/me/Documents") {
		t.Fatal("shorter sibling must not be under longer sibling")
	}
}

func TestHasPathPrefix_PosixRoot(t *testing.T) {
	withCaseInsensitive(t, false)
	if !HasPathPrefix("/anything/here", "/") {
		t.Fatal("everything is under the POSIX root")
	}
	if !HasPathPrefix("/", "/") {
		t.Fatal("root is a prefix of itself")
	}
}

func TestHasPathPrefix_TrailingSeparatorRoot(t *testing.T) {
	withCaseInsensitive(t, true)
	// A root passed with a trailing separator is cleaned; the child
	// still matches.
	if !HasPathPrefix("/Users/me/Repo/x", "/Users/me/repo/") {
		t.Fatal("trailing-separator root should still match its children")
	}
}

func TestHasPathPrefix_WindowsDriveRoot(t *testing.T) {
	withCaseInsensitive(t, true)
	// The drive-letter fold — the #277 fix — is exercised by the
	// root-equals-cwd path (VS Code passes "c:\..." for a config-stored
	// "C:\..."). This holds with either separator on any host OS.
	if !HasPathPrefix(`C:\work\git\myrepo`, `c:\work\git\myrepo`) {
		t.Fatal("Windows drive-letter case must not defeat the coverage check")
	}
	// The child-under-root branch relies on filepath.Separator, so use a
	// forward-slash Windows path (also accepted by Windows) so the
	// component boundary is verifiable on a POSIX CI runner too.
	if !HasPathPrefix(`c:/work/git/myrepo/pkg`, `C:/work/git/myrepo`) {
		t.Fatal("child under a drive-letter root must match despite case")
	}
}

func TestNormalizeVolume(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`c:\x`, `C:\x`},
		{`C:\x`, `C:\x`},
		{`\\srv\share`, `\\SRV\SHARE`},
		{`\\Srv\Share\dir`, `\\SRV\SHARE\dir`},
		{"/Users/me/project", "/Users/me/project"}, // no volume: no-op
		{"relative/path", "relative/path"},         // no volume: no-op
		{"", ""},
	}
	for _, c := range cases {
		if got := NormalizeVolume(c.in); got != c.want {
			t.Errorf("NormalizeVolume(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestNormalizeVolume_Idempotent proves store-time volume normalization is
// stable: applying it in the CLI (runTrack / install) and again in the
// daemon's TrackRepoCtx / ReconcileRepoCtx never drifts the stored path.
func TestNormalizeVolume_Idempotent(t *testing.T) {
	for _, in := range []string{`c:\x`, `C:\x`, `\\srv\share\dir`, "/Users/me/p", "relative/p", ""} {
		once := NormalizeVolume(in)
		if twice := NormalizeVolume(once); twice != once {
			t.Errorf("NormalizeVolume not idempotent for %q: once=%q twice=%q", in, once, twice)
		}
	}
}

func TestVolumeNameLen(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"/Users/me", 0},
		{"relative", 0},
		{`c:\x`, 2},
		{`C:`, 2},
		{`\\srv\share`, len(`\\srv\share`)},
		{`\\srv\share\dir`, len(`\\srv\share`)},
		{`\\`, 0},
		{"", 0},
		{"a", 0},
	}
	for _, c := range cases {
		if got := volumeNameLen(c.in); got != c.want {
			t.Errorf("volumeNameLen(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestSamePathIdentity_DistinctDirsNotMerged(t *testing.T) {
	// Two genuinely distinct directories must never be merged, even
	// when the fold is forced case-insensitive.
	withCaseInsensitive(t, true)
	a := t.TempDir()
	b := t.TempDir()
	if SamePathIdentity(a, b) {
		t.Fatalf("distinct dirs %q and %q must not be identified", a, b)
	}
}

func TestSamePathIdentity_CaseVariantOfSameDir(t *testing.T) {
	// On this host's default FS, a case variant of a real directory
	// resolves to the same inode. Only assert the merge when the host
	// filesystem is actually case-insensitive (so the variant exists).
	withCaseInsensitive(t, true)
	dir := t.TempDir()
	base := filepath.Base(dir)
	variant := filepath.Join(filepath.Dir(dir), swapCase(base))
	if _, err := os.Stat(variant); err != nil {
		t.Skipf("host filesystem is case-sensitive; %q does not resolve", variant)
	}
	if !SamePathIdentity(dir, variant) {
		t.Fatalf("case variant %q of %q should identify as the same dir", variant, dir)
	}
}

func TestSamePathIdentity_MissingPathTrustsFold(t *testing.T) {
	// When a path cannot be stat'd, the fold is trusted rather than
	// treating the pair as distinct.
	withCaseInsensitive(t, true)
	a := "/nonexistent/gortex/Repo"
	b := "/nonexistent/gortex/repo"
	if !SamePathIdentity(a, b) {
		t.Fatal("fold-equal missing paths should be identified")
	}
}

func TestSamePathIdentity_ByteEqualFastPath(t *testing.T) {
	withCaseInsensitive(t, false)
	if !SamePathIdentity("/x/y", "/x/y") {
		t.Fatal("byte-equal fast path failed")
	}
}

// swapCase toggles the case of ASCII letters so a case-variant directory
// name can be constructed deterministically.
func swapCase(s string) string {
	b := []byte(s)
	for i := range b {
		switch {
		case b[i] >= 'a' && b[i] <= 'z':
			b[i] -= 32
		case b[i] >= 'A' && b[i] <= 'Z':
			b[i] += 32
		}
	}
	return string(b)
}
