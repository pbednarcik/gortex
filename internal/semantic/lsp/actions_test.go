package lsp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestApplyEditsToContent_SingleEdit verifies replacing a substring
// inside a single-line file.
func TestApplyEditsToContent_SingleEdit(t *testing.T) {
	src := []byte("hello world\n")
	edits := []TextEdit{{
		Range:   Range{Start: Position{Line: 0, Character: 6}, End: Position{Line: 0, Character: 11}},
		NewText: "Earth",
	}}
	got, err := applyEditsToContent(src, edits)
	require.NoError(t, err)
	require.Equal(t, "hello Earth\n", string(got))
}

// TestApplyEditsToContent_MultipleEditsReverseOrder verifies that
// independent edits are applied in reverse position order so earlier
// offsets aren't invalidated.
func TestApplyEditsToContent_MultipleEditsReverseOrder(t *testing.T) {
	src := []byte("aaa\nbbb\nccc\n")
	edits := []TextEdit{
		{Range: Range{Start: Position{Line: 0, Character: 0}, End: Position{Line: 0, Character: 3}}, NewText: "AAA"},
		{Range: Range{Start: Position{Line: 2, Character: 0}, End: Position{Line: 2, Character: 3}}, NewText: "CCC"},
	}
	got, err := applyEditsToContent(src, edits)
	require.NoError(t, err)
	require.Equal(t, "AAA\nbbb\nCCC\n", string(got))
}

// TestApplyEditsToContent_Insertion (start==end).
func TestApplyEditsToContent_Insertion(t *testing.T) {
	src := []byte("foo\nbaz\n")
	edits := []TextEdit{{
		Range:   Range{Start: Position{Line: 1, Character: 0}, End: Position{Line: 1, Character: 0}},
		NewText: "bar\n",
	}}
	got, err := applyEditsToContent(src, edits)
	require.NoError(t, err)
	require.Equal(t, "foo\nbar\nbaz\n", string(got))
}

// TestApplyEditsToContent_UnicodeOffsets verifies UTF-16 unit math —
// `é` is one code unit in UTF-16 but two bytes in UTF-8.
func TestApplyEditsToContent_UnicodeOffsets(t *testing.T) {
	src := []byte("café\n") // 5 bytes, 4 UTF-16 code units in 'café'
	edits := []TextEdit{{
		Range:   Range{Start: Position{Line: 0, Character: 4}, End: Position{Line: 0, Character: 4}},
		NewText: "!",
	}}
	got, err := applyEditsToContent(src, edits)
	require.NoError(t, err)
	require.Equal(t, "café!\n", string(got))
}

// TestApplyEditsToContent_PastEOFClamps reproduces a bug pattern
// servers occasionally emit (range past EOF for a "newline at end"
// fix). Should clamp gracefully.
func TestApplyEditsToContent_PastEOFClamps(t *testing.T) {
	src := []byte("only\n")
	edits := []TextEdit{{
		Range:   Range{Start: Position{Line: 99, Character: 0}, End: Position{Line: 99, Character: 0}},
		NewText: "tail",
	}}
	got, err := applyEditsToContent(src, edits)
	require.NoError(t, err)
	require.Equal(t, "only\ntail", string(got))
}

// TestWriteWorkspaceEdit_DocumentChanges writes via the modern
// documentChanges form.
func TestWriteWorkspaceEdit_DocumentChanges(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.txt")
	require.NoError(t, os.WriteFile(p, []byte("old\n"), 0o644))

	// pathToURI, not `"file://" + p`: the naive concatenation puts a Windows
	// drive letter in the URI authority ("file://C:\dir\x.txt"), which
	// uriToAbsPath rejects — WriteWorkspaceEdit would then write nothing and
	// report no files.
	edit := WorkspaceEdit{
		DocumentChanges: []TextDocumentEdit{{
			TextDocument: VersionedTextDocumentIdentifier{URI: pathToURI(p), Version: 1},
			Edits: []TextEdit{{
				Range:   Range{Start: Position{Line: 0, Character: 0}, End: Position{Line: 0, Character: 3}},
				NewText: "new",
			}},
		}},
	}
	files, err := WriteWorkspaceEdit(edit)
	require.NoError(t, err)
	require.Equal(t, []string{p}, files)
	got, err := os.ReadFile(p)
	require.NoError(t, err)
	require.Equal(t, "new\n", string(got))
}

// TestWriteWorkspaceEdit_LegacyChanges writes via the legacy
// `changes` map form.
func TestWriteWorkspaceEdit_LegacyChanges(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "y.txt")
	require.NoError(t, os.WriteFile(p, []byte("a b c\n"), 0o644))

	// Same reason as the documentChanges case: the URI has to be built by
	// pathToURI so a Windows drive letter lands inside the path.
	edit := WorkspaceEdit{
		Changes: map[string][]TextEdit{
			pathToURI(p): {{
				Range:   Range{Start: Position{Line: 0, Character: 2}, End: Position{Line: 0, Character: 3}},
				NewText: "X",
			}},
		},
	}
	files, err := WriteWorkspaceEdit(edit)
	require.NoError(t, err)
	require.Equal(t, []string{p}, files)
	got, err := os.ReadFile(p)
	require.NoError(t, err)
	require.Equal(t, "a X c\n", string(got))
}

// TestSortActions confirms preferred actions float to the top.
func TestSortActions(t *testing.T) {
	actions := []CodeActionOrCommand{
		{Title: "Refactor X", Kind: CodeActionKindRefactorExtract},
		{Title: "Fix Y", Kind: CodeActionKindQuickFix},
		{Title: "Preferred", Kind: CodeActionKindRefactor, IsPreferred: true},
		{Title: "Organize", Kind: CodeActionKindSourceOrganizeImports},
	}
	sortActions(actions)
	if actions[0].Title != "Preferred" {
		t.Fatalf("preferred should sort first; got %v", actions[0].Title)
	}
	// Quickfix should beat OrganizeImports which should beat
	// refactor.extract.
	titles := make([]string, 0, len(actions))
	for _, a := range actions {
		titles = append(titles, a.Title)
	}
	want := []string{"Preferred", "Fix Y", "Organize", "Refactor X"}
	assert.Equal(t, want, titles)
}

// TestCodeActionOrCommand_AsCodeAction projects the union into the
// canonical CodeAction form for both legacy and literal shapes.
func TestCodeActionOrCommand_AsCodeAction(t *testing.T) {
	legacyJSON := `{"title":"L","command":"editor.action","arguments":[1,2]}`
	literalJSON := `{"title":"R","kind":"refactor","edit":{"changes":{"file:///tmp/a":[]}}}`
	var legacy, literal CodeActionOrCommand
	require.NoError(t, json.Unmarshal([]byte(legacyJSON), &legacy))
	require.NoError(t, json.Unmarshal([]byte(literalJSON), &literal))
	require.True(t, legacy.IsCommand())
	require.False(t, literal.IsCommand())
	la := legacy.AsCodeAction()
	require.NotNil(t, la.Command)
	require.Equal(t, "editor.action", la.Command.Command)
	ra := literal.AsCodeAction()
	require.NotNil(t, ra.Edit)
}

// TestUriToAbsPath rejects non-file URIs and parses well-formed ones.
// uriToAbsPath returns an OS-NATIVE path, so the expectation is spelled
// natively too (on Windows "file:///abs/path.go" is `\abs\path.go`). The
// per-platform URI literals — including the drive-letter shapes — are
// table-tested in internal/lspuri; here the wrapper's delegation plus a
// lossless round trip through a real native absolute path is what matters.
func TestUriToAbsPath(t *testing.T) {
	cases := []struct {
		uri  string
		want string
	}{
		{"file:///abs/path.go", filepath.FromSlash("/abs/path.go")},
		{"http://example.com/x", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := uriToAbsPath(c.uri); got != c.want {
			t.Errorf("uriToAbsPath(%q) = %q, want %q", c.uri, got, c.want)
		}
	}

	native := filepath.Join(t.TempDir(), "round trip.go")
	if got := uriToAbsPath(pathToURI(native)); got != native {
		t.Errorf("round trip of %q = %q", native, got)
	}
}

// TestApplyEditsToContent_Stable ensures applyEditsToContent doesn't
// mutate the input slice.
func TestApplyEditsToContent_Stable(t *testing.T) {
	src := []byte("aaa\n")
	srcCopy := make([]byte, len(src))
	copy(srcCopy, src)
	edits := []TextEdit{{
		Range:   Range{Start: Position{Line: 0, Character: 0}, End: Position{Line: 0, Character: 3}},
		NewText: "BBB",
	}}
	if _, err := applyEditsToContent(src, edits); err != nil {
		t.Fatal(err)
	}
	if string(src) != string(srcCopy) {
		t.Fatalf("input mutated: %q vs %q", src, srcCopy)
	}
}

// TestSortActionsIsStable verifies that ties (same kind, same
// preference) preserve original order.
func TestSortActionsIsStable(t *testing.T) {
	actions := []CodeActionOrCommand{
		{Title: "First", Kind: CodeActionKindQuickFix},
		{Title: "Second", Kind: CodeActionKindQuickFix},
		{Title: "Third", Kind: CodeActionKindQuickFix},
	}
	sortActions(actions)
	titles := make([]string, 0, 3)
	for _, a := range actions {
		titles = append(titles, a.Title)
	}
	sort.Strings(titles)
	require.Equal(t, []string{"First", "Second", "Third"}, titles)
}

// TestWholeFileEnd regresses the fix-all "line number 1073741824 out
// of range" panic — gopls validates End.Line against actual file
// length, so wholeFileEnd must return real positions, not a 1<<30
// sentinel.
func TestWholeFileEnd(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		wantLine int
		wantChar int
	}{
		{name: "empty file", content: "", wantLine: 0, wantChar: 0},
		{name: "single line no newline", content: "package main", wantLine: 0, wantChar: 12},
		{name: "single line with trailing newline", content: "package main\n", wantLine: 1, wantChar: 0},
		{name: "two lines no trailing newline", content: "a\nbb", wantLine: 1, wantChar: 2},
		{name: "two lines trailing newline", content: "a\nbb\n", wantLine: 2, wantChar: 0},
		{name: "many lines", content: "line1\nline2\nline3\nline4\n", wantLine: 4, wantChar: 0},
		{name: "blank lines", content: "\n\n\n", wantLine: 3, wantChar: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := filepath.Join(t.TempDir(), "x.go")
			require.NoError(t, os.WriteFile(f, []byte(tt.content), 0o644))
			line, char := wholeFileEnd(f)
			require.Equal(t, tt.wantLine, line, "line for %q", tt.content)
			require.Equal(t, tt.wantChar, char, "char for %q", tt.content)
		})
	}
}

// TestWholeFileEnd_NonexistentFallsBackBounded — read errors must
// return a bounded fallback rather than the old 1<<30 sentinel.
func TestWholeFileEnd_NonexistentFallsBackBounded(t *testing.T) {
	line, char := wholeFileEnd(filepath.Join(t.TempDir(), "does-not-exist.go"))
	require.Equal(t, 1_000_000, line, "should fall back to a million-line bound, not 1<<30")
	require.Equal(t, 0, char)
}
