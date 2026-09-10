package main

import (
	"fmt"
	"io"
	"runtime"
	"strings"

	"github.com/zzet/gortex/internal/daemon"
)

// applyIndexFailureStatus refreshes file health separately from the cached
// aggregate. Ready describes whether queries can run, not whether every file
// could be indexed; leave it intact so a denied file cannot block startup.
func applyIndexFailureStatus(st *daemon.StatusResponse, counts func(string) (int, int, error)) {
	st.FailedFiles, st.UnreadableFiles = 0, 0
	var healthErrors []string
	for i := range st.TrackedRepos {
		r := &st.TrackedRepos[i]
		var err error
		r.FailedFiles, r.UnreadableFiles, err = counts(r.Prefix)
		r.IndexHealthError = ""
		if err != nil {
			r.IndexHealthError = err.Error()
			healthErrors = append(healthErrors, r.Prefix+": "+err.Error())
		}
		r.IndexDegraded = r.FailedFiles > 0 || err != nil
		st.FailedFiles += r.FailedFiles
		st.UnreadableFiles += r.UnreadableFiles
	}
	st.IndexHealthError = strings.Join(healthErrors, "; ")
	st.IndexDegraded = st.FailedFiles > 0 || st.IndexHealthError != ""
}

func renderIndexFailureWarning(w io.Writer, rows []daemon.TrackedRepoStatus) {
	unreadable := false
	for _, r := range rows {
		if r.IndexHealthError != "" {
			fmt.Fprintf(w, "\n%s: file indexing health unavailable: %s; search results may be incomplete.\n", r.Prefix, r.IndexHealthError)
		}
		if r.FailedFiles == 0 {
			continue
		}
		fmt.Fprintf(w, "\n%s: index degraded — %d failed files (%d unreadable); search results may be incomplete.\n",
			r.Prefix, r.FailedFiles, r.UnreadableFiles)
		unreadable = unreadable || r.UnreadableFiles > 0
	}
	if unreadable && runtime.GOOS == "darwin" {
		fmt.Fprintln(w, "Check the daemon's Documents / Full Disk Access permission in System Settings → Privacy & Security.")
	}
}
