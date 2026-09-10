package crashpool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/zzet/gortex/internal/platform"
)

// QuarantineEntry records one file that crashed, hung, or panicked the
// parser.
type QuarantineEntry struct {
	RelPath string `json:"rel_path"`
	Reason  string `json:"reason"`
	// MtimeNano pins the file revision that failed. When the file's
	// current mtime differs the file is treated as changed and gets
	// one retry instead of being skipped.
	MtimeNano int64  `json:"mtime_nano"`
	FirstSeen string `json:"first_seen"`
	LastSeen  string `json:"last_seen"`
	Attempts  int    `json:"attempts"`
}

// Quarantine is a persistent set of files that have crashed parsing. It
// is JSON-backed so a file that SIGSEGVs the parser survives daemon
// restarts and is skipped until its content changes.
//
// The zero value is not usable; construct with LoadQuarantine. A nil
// *Quarantine is safe for every method (no-op / not-quarantined), so
// callers can keep crash isolation optional without nil checks.
type Quarantine struct {
	mu sync.Mutex
	// saveMu serialises the temp-file-plus-replace commit. It is separate
	// from mu — Save snapshots the entries under mu and must not hold it
	// across the write — and it is what keeps two Saves from replacing the
	// backing file at the same instant, which Windows refuses.
	saveMu  sync.Mutex
	path    string
	entries map[string]*QuarantineEntry
}

// LoadQuarantine reads the quarantine file at path, tolerating a
// missing or corrupt file (treated as empty). path is where Save will
// persist; an empty path makes Save a no-op (in-memory only).
func LoadQuarantine(path string) *Quarantine {
	q := &Quarantine{path: path, entries: map[string]*QuarantineEntry{}}
	if path == "" {
		return q
	}
	data, err := os.ReadFile(path) //nolint:gosec // path is daemon-internal
	if err != nil {
		return q
	}
	var list []*QuarantineEntry
	if json.Unmarshal(data, &list) != nil {
		return q
	}
	for _, e := range list {
		if e != nil && e.RelPath != "" {
			q.entries[e.RelPath] = e
		}
	}
	return q
}

// IsQuarantined reports whether relPath is a known-bad file at the
// given mtime. It returns true only when the file is unchanged since
// it failed — a changed file (different mtime) is not quarantined so
// it gets retried.
func (q *Quarantine) IsQuarantined(relPath string, mtimeNano int64) bool {
	if q == nil {
		return false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	e, ok := q.entries[relPath]
	return ok && e.MtimeNano == mtimeNano
}

// Add records relPath as quarantined. Re-adding bumps the attempt
// count and refreshes the mtime + reason.
func (q *Quarantine) Add(relPath, reason string, mtimeNano int64) {
	if q == nil || relPath == "" {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	q.mu.Lock()
	defer q.mu.Unlock()
	if e, ok := q.entries[relPath]; ok {
		e.Reason = reason
		e.MtimeNano = mtimeNano
		e.LastSeen = now
		e.Attempts++
		return
	}
	q.entries[relPath] = &QuarantineEntry{
		RelPath:   relPath,
		Reason:    reason,
		MtimeNano: mtimeNano,
		FirstSeen: now,
		LastSeen:  now,
		Attempts:  1,
	}
}

// Forget drops relPath from the quarantine — used when a previously
// bad file parses cleanly on retry.
func (q *Quarantine) Forget(relPath string) {
	if q == nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.entries, relPath)
}

// Entries returns a stable, sorted snapshot of the quarantine.
func (q *Quarantine) Entries() []QuarantineEntry {
	if q == nil {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]QuarantineEntry, 0, len(q.entries))
	for _, e := range q.entries {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RelPath < out[j].RelPath })
	return out
}

// Len returns the number of quarantined files.
func (q *Quarantine) Len() int {
	if q == nil {
		return 0
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.entries)
}

// Save persists the quarantine to its backing file. A no-op when the
// quarantine was loaded with an empty path.
func (q *Quarantine) Save() error {
	if q == nil || q.path == "" {
		return nil
	}
	entries := q.Entries()
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(q.path), 0o755); err != nil {
		return err
	}
	// The indexer re-indexes files in parallel through one shared
	// quarantine, so Saves overlap. Two of them commit here: the
	// unique temp name keeps their in-flight writes apart, saveMu
	// keeps their commits apart (Windows refuses to replace a file a
	// second replace is already moving onto), and platform.ReplaceFile
	// absorbs the same refusal from another process.
	q.saveMu.Lock()
	defer q.saveMu.Unlock()
	f, err := os.CreateTemp(filepath.Dir(q.path), ".quarantine-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return platform.ReplaceFile(tmp, q.path)
}
