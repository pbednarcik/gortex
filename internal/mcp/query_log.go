package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/platform"
)

// Retrieval query logging — an append-only JSONL record of every
// retrieval-shaped tool call (question, corpus, result size, latency,
// zero-result signal). This is the daemon-side data substrate that
// offline recall tuning and the retrieval-precision eval harness read
// from; without it there is no record of what was asked or how big the
// answer was. Fail-silent by design: a logging error never disturbs a
// tool call.
//
// Gated by environment, mirroring the established telemetry knobs:
//   - GORTEX_QUERY_LOG_DISABLE=1   turn logging off entirely
//   - GORTEX_QUERY_LOG=<path>      override the log file location
//   - GORTEX_QUERY_LOG_RESPONSES=1 also persist the full response body
//   - GORTEX_QUERY_LOG_MAX_MB=<n>  rotation threshold (default 64 MiB)
//
// Default location: <CacheDir>/query-log.jsonl (disposable telemetry).

// queryLogRecord is one JSONL line. Field order/names are stable wire —
// downstream tooling (analyze kind:"retrieval_log", offline scripts)
// parses these keys.
type queryLogRecord struct {
	TS            string  `json:"ts"`
	Tool          string  `json:"tool"`
	Surface       string  `json:"surface,omitempty"`
	Facade        string  `json:"facade,omitempty"`
	Operation     string  `json:"operation,omitempty"`
	CanonicalTool string  `json:"canonical_tool,omitempty"`
	Repo          string  `json:"repo,omitempty"`
	Project       string  `json:"project,omitempty"`
	Question      string  `json:"question"`
	Corpus        string  `json:"corpus,omitempty"`
	NodesReturned int     `json:"nodes_returned"`
	DurationMS    float64 `json:"duration_ms"`
	ResultBytes   int     `json:"result_bytes"`
	ZeroResult    bool    `json:"zero_result"`
	OK            bool    `json:"ok"`
	Error         string  `json:"error,omitempty"`
	Session       string  `json:"session,omitempty"`
	Response      string  `json:"response,omitempty"`
}

// queryToolSpec describes how to extract a logged tool's "question"
// and a default corpus label.
type queryToolSpec struct {
	questionKeys []string // arg keys tried in order for the question text
	corpus       string   // default corpus label
	corpusKey    string   // arg key whose value overrides corpus (optional)
}

// retrievalToolSpecs enumerates the tools whose calls are recorded. A
// tool absent from this map is never logged — the substrate is about
// retrieval recall, not the full tool surface (tool_profile already
// covers registry introspection).
var retrievalToolSpecs = map[string]queryToolSpec{
	"search_symbols":          {questionKeys: []string{"query"}, corpus: "code", corpusKey: "corpus"},
	"smart_context":           {questionKeys: []string{"task"}, corpus: "context"},
	"find_usages":             {questionKeys: []string{"symbol", "id", "name"}, corpus: "usages"},
	"get_callers":             {questionKeys: []string{"symbol", "id", "name"}, corpus: "callers"},
	"get_call_chain":          {questionKeys: []string{"from", "to", "symbol"}, corpus: "call_chain"},
	"trace_path":              {questionKeys: []string{"source_id", "sink_id"}, corpus: "call_path"},
	"search_text":             {questionKeys: []string{"query", "pattern"}, corpus: "text"},
	"search_ast":              {questionKeys: []string{"query", "pattern"}, corpus: "ast"},
	"winnow_symbols":          {questionKeys: []string{"text_match", "query"}, corpus: "code"},
	"context_closure":         {questionKeys: []string{"seeds", "symbol", "id"}, corpus: "closure"},
	"ask":                     {questionKeys: []string{"question"}, corpus: "ask"},
	"find_implementations":    {questionKeys: []string{"symbol", "interface", "id"}, corpus: "implementations"},
	"nav":                     {questionKeys: []string{"query", "from", "symbol"}, corpus: "nav"},
	"graph_query":             {questionKeys: []string{"query"}, corpus: "graph_query"},
	"suggest_queries":         {questionKeys: []string{"query", "task"}, corpus: "suggest"},
	"search_artifacts":        {questionKeys: []string{"query"}, corpus: "artifacts"},
	"graph_completion_search": {questionKeys: []string{"query", "prefix"}, corpus: "completion"},
}

var facadeRetrievalTools = map[string]bool{
	"explore": true, "search": true, "read": true, "relations": true,
	"trace": true, "analyze": true, "ask": true, "change": true,
	"review": true, "pr": true, "recall": true, "workspace": true,
	"response": true,
}

// countResultKeys are the JSON array keys a retrieval response is most
// likely to carry its result list under, tried before a generic scan.
var countResultKeys = []string{
	"results", "symbols", "usages", "callers", "callees", "relevant_symbols",
	"rows", "matches", "candidates", "implementations", "nodes", "chain",
	"items", "hits", "members", "edges",
}

// queryLogger appends retrieval records to a JSONL file. Safe for
// concurrent use across sessions. A disabled logger is a cheap no-op.
type queryLogger struct {
	mu           sync.Mutex
	enabled      bool
	logResponses bool
	path         string
	maxBytes     int64
	file         *os.File
	written      int64
}

// newQueryLogger constructs the logger from the environment. It never
// fails: an unresolvable path or a disable flag yields a no-op logger.
func newQueryLogger() *queryLogger {
	ql := &queryLogger{maxBytes: 64 << 20}
	if isTruthyEnv(os.Getenv("GORTEX_QUERY_LOG_DISABLE")) {
		return ql // disabled
	}
	path := strings.TrimSpace(os.Getenv("GORTEX_QUERY_LOG"))
	if path == "" {
		path = filepath.Join(platform.CacheDir(), "query-log.jsonl")
	} else if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, path[2:])
		}
	}
	ql.path = path
	ql.enabled = true
	ql.logResponses = isTruthyEnv(os.Getenv("GORTEX_QUERY_LOG_RESPONSES"))
	if v := strings.TrimSpace(os.Getenv("GORTEX_QUERY_LOG_MAX_MB")); v != "" {
		if mb, err := strconv.ParseInt(v, 10, 64); err == nil && mb > 0 {
			ql.maxBytes = mb << 20
		}
	}
	return ql
}

// isTruthyEnv reports whether an env value means "on".
func isTruthyEnv(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// shouldLog reports whether a tool's calls are recorded.
func (q *queryLogger) shouldLog(tool string) bool {
	if q == nil || !q.enabled {
		return false
	}
	_, ok := retrievalToolSpecs[tool]
	return ok || facadeRetrievalTools[tool]
}

// Path returns the resolved log file path ("" when disabled).
func (q *queryLogger) Path() string {
	if q == nil {
		return ""
	}
	return q.path
}

// record builds and appends one record for a completed tool call.
// override (>=0) is an exact result count reported by the handler via
// the request context; when <0 the count is parsed from the response.
func (q *queryLogger) record(s *Server, ctx context.Context, req mcp.CallToolRequest, res *mcp.CallToolResult, hErr error, start time.Time) {
	if q == nil || !q.enabled {
		return
	}
	tool := req.Params.Name
	spec, ok := retrievalToolSpecs[tool]
	if !ok && !facadeRetrievalTools[tool] {
		return
	}
	if !ok {
		spec = queryToolSpec{questionKeys: []string{"query", "task", "question", "symbol", "id", "name"}, corpus: "facade"}
	}
	args := req.GetArguments()
	question := firstStringArg(args, spec.questionKeys)
	if question == "" {
		question = nestedFacadeQuestion(args)
	}
	corpus := spec.corpus
	if spec.corpusKey != "" {
		if c := stringArg(args, spec.corpusKey); c != "" {
			corpus = c
		}
	}

	text := toolResultText(res)
	okCall := hErr == nil && (res == nil || !res.IsError)

	var count int
	if override := resultCountFromContext(ctx); override >= 0 {
		count = override
	} else if okCall {
		count = countFromResultText(text)
	}

	rec := queryLogRecord{
		TS:            time.Now().UTC().Format(time.RFC3339Nano),
		Tool:          tool,
		Question:      truncateRunes(question, 1024),
		Corpus:        corpus,
		NodesReturned: count,
		DurationMS:    float64(time.Since(start).Microseconds()) / 1000.0,
		ResultBytes:   len(text),
		ZeroResult:    okCall && count == 0,
		OK:            okCall,
	}
	if s != nil {
		rec.Repo, rec.Project = s.sessionLocality(ctx)
		rec.Session = SessionIDFromContext(ctx)
		if p := s.effectiveSessionPolicy(ctx); p != nil && isFacadePreset(p.preset) && isFacadeToolName(tool) {
			rec.Surface = FacadeSurfaceVersion
			rec.Facade = tool
			rec.Operation = normalizeFacadeOperation(stringArg(args, "operation"))
			if tool == "analyze" {
				rec.Operation = normalizeFacadeOperation(stringArg(args, "kind"))
			}
			if rec.Operation == "" {
				rec.Operation = defaultFacadeOperation(tool)
			}
			if op, found := s.facades.operation(tool, rec.Operation); found {
				rec.CanonicalTool = op.Legacy
			}
		}
	}
	if !okCall {
		if hErr != nil {
			rec.Error = truncateRunes(hErr.Error(), 512)
		} else {
			rec.Error = truncateRunes(strings.TrimSpace(text), 512)
		}
	}
	if q.logResponses {
		rec.Response = text
	}

	line, err := json.Marshal(&rec)
	if err != nil {
		return
	}
	q.append(line)
}

func nestedFacadeQuestion(args map[string]any) string {
	for _, field := range []string{"target", "source", "arguments", "options"} {
		obj, ok := args[field].(map[string]any)
		if !ok {
			continue
		}
		if value := firstStringArg(obj, []string{"query", "task", "question", "symbol", "file", "id", "name"}); value != "" {
			return value
		}
	}
	return ""
}

// append writes one JSONL line, opening (and rotating) the file lazily.
// All errors are swallowed — logging must never break a tool call.
func (q *queryLogger) append(line []byte) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.file == nil {
		if err := os.MkdirAll(filepath.Dir(q.path), 0o755); err != nil {
			q.enabled = false
			return
		}
		f, err := os.OpenFile(q.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			q.enabled = false
			return
		}
		q.file = f
		if fi, err := f.Stat(); err == nil {
			q.written = fi.Size()
		}
	}
	if q.maxBytes > 0 && q.written+int64(len(line))+1 > q.maxBytes {
		q.rotateLocked()
	}
	n, err := q.file.Write(append(line, '\n'))
	if err != nil {
		// A write failure (full disk, revoked handle) disables logging
		// rather than spamming errors on every subsequent call.
		_ = q.file.Close()
		q.file = nil
		q.enabled = false
		return
	}
	q.written += int64(n)
}

// rotateLocked renames the current log to "<path>.1" (keeping one
// backup) and reopens a fresh file. Caller holds q.mu.
func (q *queryLogger) rotateLocked() {
	if q.file != nil {
		_ = q.file.Close()
		q.file = nil
	}
	_ = os.Rename(q.path, q.path+".1")
	f, err := os.OpenFile(q.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		q.enabled = false
		return
	}
	q.file = f
	q.written = 0
}

// Close flushes and closes the underlying file.
func (q *queryLogger) Close() {
	if q == nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.file != nil {
		_ = q.file.Close()
		q.file = nil
	}
}

// --- result-count extraction ------------------------------------------------

// resultCountContextKey carries a handler-reported exact result count
// through the request context. Handlers that know their result size set
// it via recordQueryResultCount; the logger prefers it over parsing.
type resultCountContextKey struct{}

type resultCountHolder struct{ n int }

// withResultCount installs a result-count holder on ctx. Returns the
// new context and the holder. The wrapper calls this for logged tools.
func withResultCount(ctx context.Context) (context.Context, *resultCountHolder) {
	h := &resultCountHolder{n: -1}
	return context.WithValue(ctx, resultCountContextKey{}, h), h
}

// recordQueryResultCount lets a handler report its exact result count
// for the query log. No-op when the context carries no holder (the
// tool isn't logged, or logging is disabled).
func recordQueryResultCount(ctx context.Context, n int) {
	if h, ok := ctx.Value(resultCountContextKey{}).(*resultCountHolder); ok && h != nil {
		h.n = n
	}
}

// resultCountFromContext returns the handler-reported count, or -1.
func resultCountFromContext(ctx context.Context) int {
	if h, ok := ctx.Value(resultCountContextKey{}).(*resultCountHolder); ok && h != nil {
		return h.n
	}
	return -1
}

// countFromResultText best-effort counts the results in a tool response
// across the formats Gortex emits (JSON object/array, GCX1, TOON/text).
// Used only when the handler did not report an exact count.
func countFromResultText(text string) int {
	t := strings.TrimSpace(text)
	if t == "" {
		return 0
	}
	// not_modified / etag short-circuits carry no fresh results.
	if strings.HasPrefix(t, "GCX1") {
		// Header line + one line per row; subtract the header.
		n := strings.Count(t, "\n")
		if n <= 0 {
			return 0
		}
		return n
	}
	switch t[0] {
	case '{':
		var m map[string]json.RawMessage
		if json.Unmarshal([]byte(t), &m) == nil {
			if raw, ok := m["not_modified"]; ok {
				var b bool
				if json.Unmarshal(raw, &b) == nil && b {
					return 0
				}
			}
			if raw, ok := m["total"]; ok {
				var n int
				if json.Unmarshal(raw, &n) == nil {
					return n
				}
			}
			best := -1
			for _, key := range countResultKeys {
				if raw, ok := m[key]; ok {
					if c := jsonArrayLen(raw); c > best {
						best = c
					}
				}
			}
			if best < 0 {
				for _, raw := range m {
					if c := jsonArrayLen(raw); c > best {
						best = c
					}
				}
			}
			if best >= 0 {
				return best
			}
			return 0
		}
	case '[':
		if c := jsonArrayLen(json.RawMessage(t)); c >= 0 {
			return c
		}
	}
	// TOON / plain text: count non-empty lines.
	n := 0
	for _, ln := range strings.Split(t, "\n") {
		if strings.TrimSpace(ln) != "" {
			n++
		}
	}
	return n
}

// jsonArrayLen returns the element count of a JSON array message, or -1
// when it isn't an array.
func jsonArrayLen(raw json.RawMessage) int {
	var arr []json.RawMessage
	if json.Unmarshal(raw, &arr) == nil {
		return len(arr)
	}
	return -1
}

// firstStringArg returns the first present, non-empty arg among keys,
// stringifying scalars and joining string/array values.
func firstStringArg(args map[string]any, keys []string) string {
	for _, k := range keys {
		if v, ok := args[k]; ok {
			if s := anyToQueryString(v); s != "" {
				return s
			}
		}
	}
	return ""
}

// anyToQueryString renders an arg value as a compact question string.
func anyToQueryString(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case []any:
		parts := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, ",")
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	}
	return ""
}

// truncateRunes caps a string at n runes, appending an ellipsis marker.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
