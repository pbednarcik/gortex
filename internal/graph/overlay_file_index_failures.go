package graph

// FileIndexFailureLayerReader identifies a layer whose failure snapshot is
// authoritative for a repository, including when the snapshot is empty.
// Filesystem access belongs to a checkout, not to its shared symbol identities.
type FileIndexFailureLayerReader interface {
	FileIndexFailureReader
	OwnsFileIndexFailures(repoPrefix string) bool
}

func (v *OverlaidView) FileIndexFailuresForRepo(repoPrefix string) ([]FileIndexFailure, error) {
	if layer, ok := v.layer.(FileIndexFailureLayerReader); ok && layer.OwnsFileIndexFailures(repoPrefix) {
		return layer.FileIndexFailuresForRepo(repoPrefix)
	}
	reader, ok := v.base.(FileIndexFailureReader)
	if !ok {
		return nil, nil
	}
	failures, err := reader.FileIndexFailuresForRepo(repoPrefix)
	if err != nil || v.layer == nil {
		return failures, err
	}
	// An editor buffer or tombstone supersedes the base file's content.
	// Do not qualify that replacement with the hidden base file's failure.
	visible := make([]FileIndexFailure, 0, len(failures))
	for _, failure := range failures {
		if !v.layer.HasFile(failure.Path) {
			visible = append(visible, failure)
		}
	}
	return visible, nil
}

var _ FileIndexFailureReader = (*OverlaidView)(nil)
