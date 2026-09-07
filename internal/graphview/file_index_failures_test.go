package graphview

import "testing"

func TestGenerationLayerFileFailureOwnership(t *testing.T) {
	layer := &GenerationLayer{}
	if layer.OwnsFileIndexFailures("") || layer.OwnsFileIndexFailures("repo") {
		t.Fatal("unbound generation claimed checkout file health")
	}
	layer.failureRepoPrefix, layer.failureRepoScoped = "repo", true
	if !layer.OwnsFileIndexFailures("repo") || layer.OwnsFileIndexFailures("other") {
		t.Fatal("generation failure scope crossed repository boundaries")
	}
}
