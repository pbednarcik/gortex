package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	_ "modernc.org/sqlite"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestIssue767IdleIOIntegration launches only explicitly supplied binaries.
// Every process owns private XDG directories, SQLite storage and a Git fixture.
// It never addresses the default daemon.
//
// GORTEX_ISSUE767_TEST_BINARY is required; BASELINE_BINARY optionally runs first.
// GORTEX_ISSUE767_ARTIFACT_DIR preserves logs/reports when supplied.
// GORTEX_ISSUE767_IDLE_DURATION defaults to 45s, bounded 15s–1h; cold idle caps 60s.
// GORTEX_ISSUE767_WRITE_BUDGET_BYTES is optional; absent/zero means no assertion.
// The 5s janitor interval accelerates this regression scenario; measurements are
// not default-configuration idle performance or physical NAND writes.
func TestIssue767IdleIOIntegration(t *testing.T) {
	fixed := os.Getenv("GORTEX_ISSUE767_TEST_BINARY")
	if fixed == "" {
		t.Skip("set GORTEX_ISSUE767_TEST_BINARY to opt into isolated daemon validation")
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("process I/O sampler supports Darwin and Linux")
	}
	idle := 45 * time.Second
	if value := os.Getenv("GORTEX_ISSUE767_IDLE_DURATION"); value != "" {
		var err error
		idle, err = time.ParseDuration(value)
		if err != nil || idle < 15*time.Second || idle > time.Hour {
			t.Fatal("GORTEX_ISSUE767_IDLE_DURATION must be between 15s and 1h")
		}
	}
	var budget uint64
	if value := os.Getenv("GORTEX_ISSUE767_WRITE_BUDGET_BYTES"); value != "" {
		var err error
		budget, err = strconv.ParseUint(value, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
	}
	required := issue767RequiredTimeout(idle, os.Getenv("GORTEX_ISSUE767_BASELINE_BINARY") != "")
	if deadline, ok := t.Deadline(); ok && time.Until(deadline) < required {
		t.Fatalf("isolated scenario needs at least %s remaining; increase go test -timeout before launching daemon children", required)
	}
	for _, variant := range []struct{ name, binary string }{{"baseline", os.Getenv("GORTEX_ISSUE767_BASELINE_BINARY")}, {"fixed", fixed}} {
		if variant.binary == "" {
			continue
		}
		t.Run(variant.name, func(t *testing.T) {
			binary, err := filepath.Abs(variant.binary)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(binary); err != nil {
				t.Fatal(err)
			}
			f := newIssue767Fixture(t, binary)
			coldStart := time.Now()
			f.start()
			f.command(10*time.Second, f.primary, "daemon", "status", "--no-progress")
			f.awaitSymbol(f.primary, "Issue767PrimaryMarker")
			t.Logf("configured cold startup to selected primary query: %s", time.Since(coldStart))
			f.write(filepath.Join(f.primary, "marker.go"), issue767MarkerSource("Issue767PrimaryMarker", "Issue767DirtyMarker"))
			f.awaitSymbol(f.primary, "Issue767DirtyMarker")
			f.git(f.primary, "worktree", "add", "-b", "issue767-linked", f.linked)
			f.write(filepath.Join(f.linked, "marker.go"), issue767MarkerSource("Issue767PrimaryMarker", "Issue767LinkedMarker"))
			// The linked checkout is intentionally never explicitly tracked.
			f.awaitSymbol(f.linked, "Issue767LinkedMarker")
			leaked, err := f.trySearchSymbol(f.primary, "Issue767LinkedMarker")
			if err != nil {
				t.Fatalf("primary isolation query failed: %v", err)
			}
			if leaked {
				t.Fatal("linked-checkout symbol leaked into primary view")
			}
			f.git(f.primary, "worktree", "remove", "--force", f.linked)
			f.awaitRemoved()
			f.awaitSymbol(f.primary, "Issue767DirtyMarker")
			f.settle()
			cold := f.measureIdle("cold_idle", min(idle, time.Minute))
			f.stop()
			warmStart := time.Now()
			f.start()
			f.awaitSymbol(f.primary, "Issue767DirtyMarker")
			t.Logf("warm startup to selected primary query: %s", time.Since(warmStart))
			f.awaitRemoved()
			f.settle()
			warm := f.measureIdle("warm_idle", idle)
			if warm.Before.Sequence != cold.After.Sequence {
				t.Errorf("unchanged warm restart allocated new generations: cold=%d warm=%d", cold.After.Sequence, warm.Before.Sequence)
			}
			if variant.name == "fixed" && budget > 0 {
				for _, report := range []issue767IdleReport{cold, warm} {
					if report.ProcessBytesWritten > budget {
						t.Errorf("%s wrote %d process-accounted bytes, budget %d", report.Phase, report.ProcessBytesWritten, budget)
					}
				}
			}
		})
	}
}

func issue767RequiredTimeout(idle time.Duration, baseline bool) time.Duration {
	variants := 1
	if baseline {
		variants++
	}
	return time.Duration(variants) * (min(idle, time.Minute) + idle + 3*time.Minute)
}

type issue767Fixture struct {
	t                                    *testing.T
	binary, root, primary, linked, store string
	env                                  []string
	cmd                                  *exec.Cmd
	cancel                               context.CancelFunc
	done                                 chan error
	log                                  *os.File
	run                                  int
	lastOutput                           string
}

func newIssue767Fixture(t *testing.T, binary string) *issue767Fixture {
	t.Helper()
	parent := os.Getenv("GORTEX_ISSUE767_ARTIFACT_DIR")
	preserve := parent != ""
	if !preserve {
		parent = "/tmp"
	}
	var err error
	parent, err = filepath.Abs(parent)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(parent, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(parent, "gx767-")
	if err != nil {
		t.Fatal(err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if !preserve {
		t.Cleanup(func() { _ = os.RemoveAll(root) })
	}
	f := &issue767Fixture{t: t, binary: binary, root: root, primary: filepath.Join(root, "repo"), linked: filepath.Join(root, "linked"), store: filepath.Join(root, "store.sqlite")}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "GORTEX_") || strings.HasPrefix(key, "XDG_") || strings.HasPrefix(key, "GIT_") {
			continue
		}
		f.env = append(f.env, entry)
	}
	f.env = append(f.env,
		"XDG_CONFIG_HOME="+filepath.Join(root, "config"), "XDG_DATA_HOME="+filepath.Join(root, "data"), "XDG_CACHE_HOME="+filepath.Join(root, "cache"),
		"GORTEX_DAEMON_PPROF_ADDR=127.0.0.1:0", "GORTEX_RECONCILE_INTERVAL=5s",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+filepath.Join(root, "gitconfig"), "GIT_TERMINAL_PROMPT=0", "GOWORK=off", "NO_COLOR=1", "CI=1")
	f.write(filepath.Join(root, "gitconfig"), "[user]\n\tname = Issue767 Test\n\temail = issue767@example.invalid\n[commit]\n\tgpgsign = false\n")
	f.write(filepath.Join(f.primary, "go.mod"), "module example.invalid/issue767\n\ngo 1.24\n")
	f.write(filepath.Join(f.primary, "marker.go"), issue767MarkerSource("Issue767PrimaryMarker"))
	for i := 0; i < 32; i++ {
		f.write(filepath.Join(f.primary, fmt.Sprintf("file%02d.go", i)), fmt.Sprintf("package fixture\nfunc Issue767Target%02d() int { return %d }\nfunc Issue767Caller%02d() int { return Issue767Target00() }\n", i, i, i))
	}
	f.git(f.primary, "init", "-b", "main")
	f.git(f.primary, "add", ".")
	f.git(f.primary, "commit", "-m", "isolated fixture")
	// Match configured cold startup; runtime track is a separate scenario.
	f.write(filepath.Join(root, "config", "gortex", "config.yaml"), "repos:\n  - path: "+strconv.Quote(f.primary)+"\n    name: issue767\n")
	t.Cleanup(f.stop)
	t.Logf("isolated fixture artifacts: %s", root)
	return f
}

func issue767MarkerSource(names ...string) string {
	source := "package fixture\n"
	for _, name := range names {
		source += "func " + name + "() int { return Issue767Target00() }\n"
	}
	return source
}

func (f *issue767Fixture) write(path, content string) {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *issue767Fixture) git(dir string, args ...string) {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(f.t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir, cmd.Env = dir, f.env
	if output, err := cmd.CombinedOutput(); err != nil {
		f.t.Fatalf("fixture git %v: %v\n%s", args, err, output)
	}
}

func (f *issue767Fixture) command(timeout time.Duration, dir string, args ...string) []byte {
	f.t.Helper()
	output, err := f.tryCommand(timeout, dir, args...)
	if err != nil {
		f.t.Fatalf("isolated CLI %v: %v\n%s", args, err, output)
	}
	return output
}

func (f *issue767Fixture) tryCommand(timeout time.Duration, dir string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(f.t.Context(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, f.binary, args...)
	cmd.Dir, cmd.Env = dir, f.env
	output, err := cmd.CombinedOutput()
	f.lastOutput = string(output)
	if len(f.lastOutput) > 4096 {
		f.lastOutput = f.lastOutput[len(f.lastOutput)-4096:]
	}
	return output, err
}

func (f *issue767Fixture) start() {
	f.t.Helper()
	f.run++
	var err error
	f.log, err = os.Create(filepath.Join(f.root, fmt.Sprintf("daemon-%d.log", f.run)))
	if err != nil {
		f.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(f.t.Context())
	if deadline, ok := f.t.Deadline(); ok {
		cancel()
		ctx, cancel = context.WithDeadline(f.t.Context(), deadline.Add(-time.Minute))
	}
	f.cancel = cancel
	cmd := exec.CommandContext(ctx, f.binary, "daemon", "start", "--embeddings=false", "--backend", "sqlite", "--backend-path", f.store, "--no-progress")
	// Global testing timeout bypasses Cleanup. This captured child receives
	// SIGINT and a bounded forced exit before the parent testing deadline.
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 10 * time.Second
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = f.primary, f.env, f.log, f.log
	if err := cmd.Start(); err != nil {
		cancel()
		_ = f.log.Close()
		f.t.Fatal(err)
	}
	f.cmd, f.done = cmd, make(chan error, 1)
	done := f.done
	go func() { done <- cmd.Wait() }()
	f.await("isolated daemon socket", time.Minute, func() bool {
		select {
		case err := <-f.done:
			f.cmd = nil
			_ = f.log.Close()
			f.t.Fatalf("isolated daemon exited during start: %v; log %s", err, f.log.Name())
		default:
		}
		_, err := f.tryCommand(5*time.Second, f.primary, "daemon", "status", "--no-progress")
		return err == nil
	})
}

func (f *issue767Fixture) stop() {
	if cancel := f.cancel; cancel != nil {
		f.cancel = nil
		defer cancel()
	}
	if f.cmd == nil {
		return
	}
	cmd, done := f.cmd, f.done
	f.cmd = nil
	_ = cmd.Process.Signal(os.Interrupt)
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			f.t.Errorf("isolated child %d did not exit after kill", cmd.Process.Pid)
		}
		f.t.Errorf("isolated child %d required force kill", cmd.Process.Pid)
	}
	_ = f.log.Close()
}

func (f *issue767Fixture) await(label string, timeout time.Duration, ready func() bool) {
	f.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		select {
		case <-f.t.Context().Done():
			f.t.Fatal(f.t.Context().Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
	f.t.Fatalf("timed out waiting for %s; artifacts %s; last CLI response: %s", label, f.root, f.lastOutput)
}

func (f *issue767Fixture) searchHasSymbol(root, name string) bool {
	found, err := f.trySearchSymbol(root, name)
	return err == nil && found
}

func (f *issue767Fixture) trySearchSymbol(root, name string) (bool, error) {
	request := map[string]any{"operation": "symbols", "query": name, "options": map[string]any{"limit": 10, "query_class": "symbol", "expand": "off"}}
	if root != f.primary {
		request["view"] = map[string]any{"kind": "worktree", "path": root}
	}
	payload, err := json.Marshal(request)
	if err != nil {
		f.t.Fatal(err)
	}
	output, err := f.tryCommand(10*time.Second, root, "call", "search", "--index", root, "--json", string(payload), "--format", "json")
	if err != nil {
		return false, fmt.Errorf("search command: %w: %s", err, output)
	}
	var value any
	if err := json.Unmarshal(output, &value); err != nil {
		return false, fmt.Errorf("search response: %w: %s", err, output)
	}
	found, fallback := issue767JSONEvidence(value, name)
	if fallback {
		return false, errors.New("search returned fallback or tool error")
	}
	if root != f.primary && !issue767JSONExact(value) {
		return false, errors.New("automatic checkout search did not prove exact freshness")
	}
	if found && !issue767JSONSource(value, name, root) {
		return false, errors.New("symbol did not belong to selected source root and repository")
	}
	return found, nil
}

func issue767JSONEvidence(value any, name string) (found, fallback bool) {
	merge := func(child any) {
		childFound, childFallback := issue767JSONEvidence(child, name)
		found, fallback = found || childFound, fallback || childFallback
	}
	switch value := value.(type) {
	case map[string]any:
		exact, hasExact := value["exact"].(bool)
		fallback = hasExact && !exact
		if code, ok := value["error_code"].(string); ok && code != "" {
			fallback = true
		}
		if isError, ok := value["isError"].(bool); ok && isError {
			fallback = true
		}
		found = value["name"] == name
		for _, child := range value {
			merge(child)
		}
	case []any:
		for _, child := range value {
			merge(child)
		}
	case string:
		var child any
		if strings.HasPrefix(value, "{") || strings.HasPrefix(value, "[") {
			if json.Unmarshal([]byte(value), &child) == nil {
				merge(child)
			}
		}
	}
	return found, fallback
}

func issue767JSONExact(value any) bool {
	switch value := value.(type) {
	case map[string]any:
		if exact, ok := value["exact"].(bool); ok && exact {
			return true
		}
		for _, child := range value {
			if issue767JSONExact(child) {
				return true
			}
		}
	case []any:
		for _, child := range value {
			if issue767JSONExact(child) {
				return true
			}
		}
	case string:
		var child any
		if (strings.HasPrefix(value, "{") || strings.HasPrefix(value, "[")) && json.Unmarshal([]byte(value), &child) == nil {
			return issue767JSONExact(child)
		}
	}
	return false
}

func issue767JSONSource(value any, name, root string) bool {
	switch value := value.(type) {
	case map[string]any:
		if value["name"] == name && value["repo_prefix"] == "issue767" {
			path, ok := value["absolute_file_path"].(string)
			if ok && filepath.Clean(path) == filepath.Join(root, "marker.go") {
				return true
			}
		}
		for _, child := range value {
			if issue767JSONSource(child, name, root) {
				return true
			}
		}
	case []any:
		for _, child := range value {
			if issue767JSONSource(child, name, root) {
				return true
			}
		}
	case string:
		var child any
		if (strings.HasPrefix(value, "{") || strings.HasPrefix(value, "[")) && json.Unmarshal([]byte(value), &child) == nil {
			return issue767JSONSource(child, name, root)
		}
	}
	return false
}

func (f *issue767Fixture) awaitSymbol(root, name string) {
	f.t.Helper()
	f.await("selected symbol "+name, 2*time.Minute, func() bool { return f.searchHasSymbol(root, name) })
}

func (f *issue767Fixture) openReadOnly() *sql.DB {
	f.t.Helper()
	u := &url.URL{Scheme: "file", Path: filepath.ToSlash(f.store), RawQuery: "mode=ro"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		f.t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	f.t.Cleanup(func() { _ = db.Close() })
	return db
}

func (f *issue767Fixture) awaitRemoved() {
	db := f.openReadOnly()
	defer db.Close()
	f.await("removed checkout cleanup", 2*time.Minute, func() bool {
		ctx, cancel := context.WithTimeout(f.t.Context(), time.Second)
		defer cancel()
		var count int
		return db.QueryRowContext(ctx, "SELECT COUNT(*) FROM checkouts WHERE root_path=?", f.linked).Scan(&count) == nil && count == 0
	})
}

type issue767GenerationSnapshot struct {
	Count, Max, Sequence                    int64
	Nodes, Edges, RefFacts, PrimaryRefFacts int64
}

func issue767ReadGenerations(ctx context.Context, db *sql.DB) (issue767GenerationSnapshot, error) {
	result := issue767GenerationSnapshot{Sequence: -1}
	err := db.QueryRowContext(ctx, "SELECT COUNT(*), COALESCE(MAX(generation_id),0) FROM view_generations").Scan(&result.Count, &result.Max)
	if err != nil {
		return result, err
	}
	err = db.QueryRowContext(ctx, "SELECT (SELECT COUNT(*) FROM nodes), (SELECT COUNT(*) FROM edges), (SELECT COUNT(*) FROM ref_facts), (SELECT COUNT(*) FROM ref_facts WHERE view_gen=0 AND repo_prefix='issue767')").Scan(&result.Nodes, &result.Edges, &result.RefFacts, &result.PrimaryRefFacts)
	if err != nil {
		return result, err
	}
	err = db.QueryRowContext(ctx, "SELECT seq FROM sqlite_sequence WHERE name='view_generations'").Scan(&result.Sequence)
	if err != nil {
		return result, err
	}
	return result, nil
}

func (f *issue767Fixture) settle() {
	db := f.openReadOnly()
	defer db.Close()
	var previous issue767GenerationSnapshot
	stable := 0
	f.await("three stable generation samples", 2*time.Minute, func() bool {
		ctx, cancel := context.WithTimeout(f.t.Context(), 3*time.Second)
		defer cancel()
		current, err := issue767ReadGenerations(ctx, db)
		if err != nil {
			return false
		}
		if current == previous {
			stable++
		} else {
			stable = 0
		}
		previous = current
		if stable < 3 {
			time.Sleep(5 * time.Second)
		}
		return stable >= 3
	})
}

type issue767ProcessIO struct {
	BytesWritten        uint64
	LogicalBytesWritten *uint64
	StartTicks          uint64
}

const issue767DarwinIOScript = "import ctypes,json,sys\nfields='user_time system_time pkg_idle_wkups interrupt_wkups pageins wired_size resident_size phys_footprint proc_start_abstime proc_exit_abstime child_user_time child_system_time child_pkg_idle_wkups child_interrupt_wkups child_pageins child_elapsed_abstime diskio_bytesread diskio_byteswritten cpu_time_qos_default cpu_time_qos_maintenance cpu_time_qos_background cpu_time_qos_utility cpu_time_qos_legacy cpu_time_qos_user_initiated cpu_time_qos_user_interactive billed_system_time serviced_system_time logical_writes lifetime_max_phys_footprint instructions cycles billed_energy serviced_energy interval_max_phys_footprint runnable_time'.split()\nclass R(ctypes.Structure):\n _fields_=[('uuid',ctypes.c_uint8*16)]+[(n,ctypes.c_uint64) for n in fields]\nr=R();lib=ctypes.CDLL('/usr/lib/libproc.dylib',use_errno=True);fn=lib.proc_pid_rusage;fn.argtypes=[ctypes.c_int,ctypes.c_int,ctypes.c_void_p];fn.restype=ctypes.c_int\nif fn(int(sys.argv[1]),4,ctypes.byref(r))!=0: raise OSError(ctypes.get_errno(),'proc_pid_rusage')\nprint(json.dumps({'BytesWritten':r.diskio_byteswritten,'LogicalBytesWritten':r.logical_writes,'StartTicks':r.proc_start_abstime}))\n"

func issue767ReadProcessIO(ctx context.Context, pid int) (issue767ProcessIO, error) {
	var result issue767ProcessIO
	if runtime.GOOS == "darwin" {
		output, err := exec.CommandContext(ctx, "python3", "-c", issue767DarwinIOScript, strconv.Itoa(pid)).Output()
		if err != nil {
			return result, err
		}
		err = json.Unmarshal(output, &result)
		return result, err
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return result, err
	}
	end := strings.LastIndexByte(string(stat), ')')
	if end < 0 {
		return result, errors.New("invalid process stat")
	}
	fields := strings.Fields(string(stat[end+1:]))
	if len(fields) < 20 {
		return result, errors.New("process starttime missing")
	}
	result.StartTicks, err = strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return result, err
	}
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/io", pid))
	if err != nil {
		return result, err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if value, found := strings.CutPrefix(line, "write_bytes:"); found {
			result.BytesWritten, err = strconv.ParseUint(strings.TrimSpace(value), 10, 64)
			return result, err
		}
	}
	return result, errors.New("process write_bytes counter not found")
}

type issue767IdleReport struct {
	Phase                         string
	ElapsedSeconds                float64
	ProcessBytesWritten           uint64
	ProcessLogicalBytesWritten    *uint64 `json:",omitempty"`
	WALBeforeBytes, WALAfterBytes int64
	Before, After                 issue767GenerationSnapshot
}

func (f *issue767Fixture) measureIdle(phase string, duration time.Duration) issue767IdleReport {
	f.t.Helper()
	db := f.openReadOnly()
	defer db.Close()
	ctx, cancel := context.WithTimeout(f.t.Context(), duration+30*time.Second)
	defer cancel()
	before, err := issue767ReadGenerations(ctx, db)
	if err != nil {
		f.t.Fatal(err)
	}
	first, err := issue767ReadProcessIO(ctx, f.cmd.Process.Pid)
	if err != nil {
		f.t.Fatal(err)
	}
	if before.PrimaryRefFacts == 0 {
		f.t.Fatal("selected primary did not persist reference facts; idle I/O would not exercise the repaired projection")
	}
	report := issue767IdleReport{Phase: phase, Before: before, WALBeforeBytes: issue767FileSize(f.store + "-wal")}
	start := time.Now()
	for time.Since(start) < duration {
		if !f.searchHasSymbol(f.primary, "Issue767DirtyMarker") {
			f.t.Fatal("read-only query lost the selected ready symbol")
		}
		select {
		case <-ctx.Done():
			f.t.Fatal(ctx.Err())
		case <-time.After(5 * time.Second):
		}
	}
	last, err := issue767ReadProcessIO(ctx, f.cmd.Process.Pid)
	if err != nil {
		f.t.Fatal(err)
	}
	if first.StartTicks != last.StartTicks || last.BytesWritten < first.BytesWritten {
		f.t.Fatal("process identity/counter changed during sample")
	}
	report.ProcessBytesWritten = last.BytesWritten - first.BytesWritten
	if first.LogicalBytesWritten != nil && last.LogicalBytesWritten != nil {
		if *last.LogicalBytesWritten < *first.LogicalBytesWritten {
			f.t.Fatal("logical write counter regressed during sample")
		}
		delta := *last.LogicalBytesWritten - *first.LogicalBytesWritten
		report.ProcessLogicalBytesWritten = &delta
	}
	report.ElapsedSeconds = time.Since(start).Seconds()
	report.WALAfterBytes = issue767FileSize(f.store + "-wal")
	report.After, err = issue767ReadGenerations(ctx, db)
	if err != nil {
		f.t.Fatal(err)
	}
	if report.After.PrimaryRefFacts == 0 {
		f.t.Error("selected primary lost its reference facts during the idle phase")
	}
	if report.Before.Sequence != report.After.Sequence {
		f.t.Errorf("settled unchanged fixture allocated new generations: before=%+v after=%+v", report.Before, report.After)
	}
	// Retired payload may legitimately be collected during a long sample.
	// Counts are evidence, not proof that no existing rows were rewritten.
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		f.t.Fatal(err)
	}
	f.write(filepath.Join(f.root, phase+".json"), string(data)+"\n")
	f.t.Logf("%s: %s", f.root, data)
	return report
}

func issue767FileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

func TestIssue767JSONEvidence(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		value                  any
		wantFound, wantRefusal bool
	}{
		{"exact", map[string]any{"results": []any{map[string]any{"name": "marker"}}, "freshness": map[string]any{"exact": true}}, true, false},
		{"sibling_fallback", map[string]any{"results": []any{map[string]any{"name": "marker"}}, "freshness": map[string]any{"exact": false}}, true, true},
		{"wrapped", map[string]any{"text": "{\"name\":\"marker\",\"freshness\":{\"exact\":false}}"}, true, true},
		{"tool_error", map[string]any{"error_code": "view_building"}, false, true},
		{"missing", map[string]any{"results": []any{}}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			found, refusal := issue767JSONEvidence(tc.value, "marker")
			if found != tc.wantFound || refusal != tc.wantRefusal {
				t.Fatalf("got found=%v refusal=%v, want found=%v refusal=%v", found, refusal, tc.wantFound, tc.wantRefusal)
			}
		})
	}
}

func TestIssue767RequiredTimeout(t *testing.T) {
	for _, tc := range []struct {
		idle     time.Duration
		baseline bool
		want     time.Duration
	}{
		{45 * time.Second, false, 270 * time.Second},
		{time.Minute, true, 10 * time.Minute},
		{30 * time.Minute, false, 34 * time.Minute},
		{time.Hour, true, 128 * time.Minute},
	} {
		if got := issue767RequiredTimeout(tc.idle, tc.baseline); got != tc.want {
			t.Errorf("idle=%s baseline=%v: got %s, want %s", tc.idle, tc.baseline, got, tc.want)
		}
	}
}
