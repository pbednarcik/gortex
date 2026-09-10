package graphview

import "github.com/zzet/gortex/internal/graph"

func (l *GenerationLayer) OwnsFileIndexFailures(repoPrefix string) bool {
	return l.failureRepoScoped && l.failureRepoPrefix == repoPrefix
}

func (l *GenerationLayer) FileIndexFailuresForRepo(repoPrefix string) ([]graph.FileIndexFailure, error) {
	return l.handle.FileIndexFailuresForRepo(repoPrefix)
}

var _ graph.FileIndexFailureLayerReader = (*GenerationLayer)(nil)
