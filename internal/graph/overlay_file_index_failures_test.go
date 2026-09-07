package graph

import (
	"errors"
	"testing"
)

type failureSnapshotLayer struct {
	OverlayLayerReader
	repo          string
	authoritative bool
	failures      []FileIndexFailure
	covered       map[string]bool
	err           error
}

func (l *failureSnapshotLayer) OwnsFileIndexFailures(repo string) bool {
	return l.authoritative && l.repo == repo
}
func (l *failureSnapshotLayer) FileIndexFailuresForRepo(string) ([]FileIndexFailure, error) {
	return l.failures, l.err
}
func (l *failureSnapshotLayer) HasFile(path string) bool { return l.covered[path] }

func TestOverlaidFileIndexFailuresRespectCheckoutAndTombstones(t *testing.T) {
	base := New()
	for _, repo := range []string{"repo", "other"} {
		if err := base.ReplaceFileIndexFailures(repo, []FileIndexFailure{{Path: repo + "/denied.go", RepoPrefix: repo, Error: "permission denied", PermissionDenied: true}}); err != nil {
			t.Fatal(err)
		}
	}
	layer := &failureSnapshotLayer{repo: "repo", authoritative: true}
	view := NewOverlaidViewWithLayer(base, layer)
	rows, err := view.FileIndexFailuresForRepo("repo")
	if err != nil || len(rows) != 0 {
		t.Fatalf("healthy checkout inherited primary failures: %+v, %v", rows, err)
	}
	rows, err = view.FileIndexFailuresForRepo("other")
	if err != nil || len(rows) != 1 || rows[0].Path != "other/denied.go" {
		t.Fatalf("unrelated repo lost base failures: %+v, %v", rows, err)
	}
	layer.failures = []FileIndexFailure{{Path: "repo/checkout.go", RepoPrefix: "repo", Error: "I/O error"}}
	rows, err = view.FileIndexFailuresForRepo("repo")
	if err != nil || len(rows) != 1 || rows[0].Path != "repo/checkout.go" {
		t.Fatalf("checkout did not supply its own failures: %+v, %v", rows, err)
	}
	// Unsaved buffers and deletions mask only covered files; they are not
	// authoritative for the rest of the repository's disk state.
	buffer := &failureSnapshotLayer{covered: map[string]bool{"repo/checkout.go": true}}
	top := NewOverlaidViewWithLayer(view, buffer)
	rows, err = top.FileIndexFailuresForRepo("repo")
	if err != nil || len(rows) != 0 {
		t.Fatalf("covered base failure remained visible: %+v, %v", rows, err)
	}
	wantErr := errors.New("failure ledger unavailable")
	layer.err = wantErr
	if _, err := view.FileIndexFailuresForRepo("repo"); !errors.Is(err, wantErr) {
		t.Fatalf("lost ledger read error: %v", err)
	}
}
