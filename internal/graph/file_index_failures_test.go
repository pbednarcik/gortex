package graph

import (
	"reflect"
	"testing"
)

func TestFileIndexFailuresIsolationAndRecovery(t *testing.T) {
	g := &Graph{}
	failures := []FileIndexFailure{{Path: "repo/b.go", Error: "permission denied", PermissionDenied: true}, {Path: "repo/a.go", Error: "read failed"}}
	if err := g.ReplaceFileIndexFailures("repo", failures); err != nil {
		t.Fatal(err)
	}
	if err := g.ReplaceFileIndexFailures("other", []FileIndexFailure{{Path: "other/a.go", Error: "other failure"}}); err != nil {
		t.Fatal(err)
	}
	failures[0].Error = "caller changed input"
	got, err := g.FileIndexFailuresForRepo("repo")
	if err != nil || len(got) != 2 || got[0].Path != "repo/a.go" || got[1].Error != "permission denied" || got[1].RepoPrefix != "repo" {
		t.Fatalf("failure snapshot = %#v, %v", got, err)
	}
	got[0].Error = "caller changed output"
	again, _ := g.FileIndexFailuresForRepo("repo")
	if again[0].Error != "read failed" {
		t.Fatal("caller mutation leaked into the stored snapshot")
	}
	if err := g.ReplaceFileIndexFailures("repo", nil); err != nil {
		t.Fatal(err)
	}
	got, _ = g.FileIndexFailuresForRepo("repo")
	if got == nil || len(got) != 0 {
		t.Fatalf("recovered repository failures = %#v", got)
	}
	other, _ := g.FileIndexFailuresForRepo("other")
	if len(other) != 1 {
		t.Fatalf("other repository changed: %#v", other)
	}
}

func TestFileIndexFailuresEvictRepoWithoutNodes(t *testing.T) {
	g := &Graph{}
	_ = g.ReplaceFileIndexFailures("repo", []FileIndexFailure{{Path: "repo/unreadable.go", Error: "permission denied"}})
	g.EvictRepo("repo")
	got, err := g.FileIndexFailuresForRepo("repo")
	if err != nil || !reflect.DeepEqual(got, []FileIndexFailure{}) {
		t.Fatalf("evicted repository failures = %#v, %v", got, err)
	}
}
