package store_sqlite

import (
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestFileIndexFailuresSurviveFileEviction(t *testing.T) {
	s, err := Open(t.TempDir() + "/graph.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	const repo = "example/repo"
	failures := []graph.FileIndexFailure{
		{Path: "unreadable.go", Error: "permission denied", PermissionDenied: true},
		{Path: "other.go", Error: "read failed"},
	}
	if err := s.ReplaceFileIndexFailures(repo, failures); err != nil {
		t.Fatal(err)
	}

	for _, paths := range [][]string{{"unreadable.go"}, {"unreadable.go", "other.go"}} {
		s.EvictFiles(paths)
		got, err := s.FileIndexFailuresForRepo(repo)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(failures) {
			t.Fatalf("EvictFiles(%v) cleared unresolved failures: got %+v", paths, got)
		}
	}

	s.EvictRepo(repo)
	got, err := s.FileIndexFailuresForRepo(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("repository eviction retained failures: %+v", got)
	}
}

func TestFileIndexFailuresPersistAndScope(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []graph.FileIndexFailure{{Path: "repo/unreadable.go", Error: "open: permission denied", PermissionDenied: true, RepoPrefix: "repo", WorkspaceID: "workspace", ProjectID: "project"}}
	if err := s.ReplaceFileIndexFailures("repo", want); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceFileIndexFailures("other", []graph.FileIndexFailure{{Path: "other/a.go", Error: "other failure"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	got, err := s.FileIndexFailuresForRepo("repo")
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("reopened failures = %#v, %v; want %#v", got, err, want)
	}
	// A different generation must treat its empty ledger as authoritative.
	otherView := *s
	otherView.viewGen = 17
	otherView.ownsCore = false
	got, err = otherView.FileIndexFailuresForRepo("repo")
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("healthy generation inherited failures: %#v, %v", got, err)
	}
	if err := otherView.ReplaceFileIndexFailures("repo", []graph.FileIndexFailure{{Path: "repo/different.go", Error: "generation failure"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceFileIndexFailures("repo", nil); err != nil {
		t.Fatal(err)
	}
	got, _ = otherView.FileIndexFailuresForRepo("repo")
	if len(got) != 1 || got[0].Path != "repo/different.go" {
		t.Fatalf("base recovery changed another generation: %#v", got)
	}
	if err := s.PurgeRepo("repo"); err != nil {
		t.Fatal(err)
	}
	got, _ = otherView.FileIndexFailuresForRepo("repo")
	if len(got) != 0 {
		t.Fatalf("purge left generation failures: %#v", got)
	}
	got, _ = s.FileIndexFailuresForRepo("other")
	if len(got) != 1 {
		t.Fatalf("purge changed another repository: %#v", got)
	}
}

func TestFileIndexFailuresReplacementIsAtomic(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	want := []graph.FileIndexFailure{{Path: "repo/original.go", Error: "original", RepoPrefix: "repo"}}
	if err := s.ReplaceFileIndexFailures("repo", want); err != nil {
		t.Fatal(err)
	}
	if _, err := s.writerDB.Exec(`CREATE TRIGGER reject_failure BEFORE INSERT ON file_index_failures
WHEN NEW.error = 'reject' BEGIN SELECT RAISE(ABORT, 'test failure'); END`); err != nil {
		t.Fatal(err)
	}
	batch := make([]graph.FileIndexFailure, fileMetaChunk+1)
	for i := range batch {
		batch[i] = graph.FileIndexFailure{Path: fmt.Sprintf("repo/%03d.go", i), Error: "failure"}
	}
	batch[len(batch)-1].Error = "reject"
	if err := s.ReplaceFileIndexFailures("repo", batch); err == nil {
		t.Fatal("expected replacement to fail")
	}
	got, err := s.FileIndexFailuresForRepo("repo")
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("failed replacement changed snapshot: %#v, %v", got, err)
	}
}

func TestFileIndexFailuresEvictWithoutNodes(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.ReplaceFileIndexFailures("repo", []graph.FileIndexFailure{{Path: "repo/unreadable.go", Error: "permission denied"}}); err != nil {
		t.Fatal(err)
	}
	if got := s.OrphanRepoPrefixes(nil); !reflect.DeepEqual(got, []string{"repo"}) {
		t.Fatalf("failure-only orphan was not found: %v", got)
	}
	s.EvictRepo("repo")
	got, err := s.FileIndexFailuresForRepo("repo")
	if err != nil || len(got) != 0 {
		t.Fatalf("eviction left failures: %#v, %v", got, err)
	}
}
