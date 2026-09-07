package graph

import "sort"

// FileIndexFailure records a file that could not be indexed, including files
// which have never produced a node or a successful file metadata row.
type FileIndexFailure struct {
	Path             string `json:"path"`
	Error            string `json:"error"`
	PermissionDenied bool   `json:"permission_denied,omitempty"`
	RepoPrefix       string `json:"repo_prefix"`
	WorkspaceID      string `json:"workspace_id,omitempty"`
	ProjectID        string `json:"project_id,omitempty"`
}

// FileIndexFailureReader is an optional, generation-scoped health capability.
type FileIndexFailureReader interface {
	FileIndexFailuresForRepo(repoPrefix string) ([]FileIndexFailure, error)
}

// FileIndexFailureWriter replaces one repository's complete failure snapshot.
// An empty snapshot clears failures after recovery without touching graph data.
type FileIndexFailureWriter interface {
	ReplaceFileIndexFailures(repoPrefix string, failures []FileIndexFailure) error
}

func (g *Graph) FileIndexFailuresForRepo(repoPrefix string) ([]FileIndexFailure, error) {
	g.fileIndexFailuresMu.Lock()
	defer g.fileIndexFailuresMu.Unlock()
	out := make([]FileIndexFailure, 0, len(g.fileIndexFailures[repoPrefix]))
	for _, failure := range g.fileIndexFailures[repoPrefix] {
		out = append(out, failure)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func (g *Graph) ReplaceFileIndexFailures(repoPrefix string, failures []FileIndexFailure) error {
	byPath := make(map[string]FileIndexFailure, len(failures))
	for _, failure := range failures {
		failure.RepoPrefix = repoPrefix
		byPath[failure.Path] = failure
	}
	g.fileIndexFailuresMu.Lock()
	defer g.fileIndexFailuresMu.Unlock()
	if len(byPath) == 0 {
		delete(g.fileIndexFailures, repoPrefix)
		return nil
	}
	if g.fileIndexFailures == nil {
		g.fileIndexFailures = make(map[string]map[string]FileIndexFailure)
	}
	g.fileIndexFailures[repoPrefix] = byPath
	return nil
}

var (
	_ FileIndexFailureReader = (*Graph)(nil)
	_ FileIndexFailureWriter = (*Graph)(nil)
)
