package contracts

import (
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

const htmxFixture = `<!doctype html>
<html><body>
<tr><button hx-get="/ui/parts/{{.P.ID}}/exp">Export</button></tr>
<tr><button hx-post="/ui/parts/{{.P.ID}}/reset?t=1">Reset</button></tr>
<tr><button hx-delete='/ui/parts/{{.P.ID}}' hx-get="/health">Check</button></tr>
<a hx-get="#section">jump</a>
<a hx-post="javascript:void(0)">noop</a>
<a hx-put="{{ if .Edit }}/edit{{ else }}/new{{ end }}">dyn</a>
<a hx-get="">empty</a>
<div hx-get="/ui/parts/{{.P.ID}}/exp">dup same path other element</div>
<a hx-get="?sort=mpn&dir=asc">sort</a>
<!-- <button hx-get="/commented/route"> -->
<!--
<button hx-get="/also/commented">inside multi-line comment</button>
-->
<a hx-get="/after-comment-block">after multi-line comment</a>
<a hx-get="/api/{{if .V2}}/v2{{else}}/v1{{end}}/items">cond</a>
<a hx-get=" ?sort=mpn&dir=asc">leading-space sort</a>
<a hx-get="JavaScript:void(0)">JS uri</a>
<button data-hx-get="/health2">data- prefixed</button>
<button hx-put="/ui/parts/{{.P.ID}}/archive">archive</button>
<button hx-patch="/ui/parts/{{.P.ID}}">patch</button>
<a hx-get="https://api.vendor.com/v1/charge">literal scheme — knowably external</a>
<a hx-get="//cdn.example.com/x">protocol-relative — knowably external</a>
<my-widget track-hx-get="/x">hyphen-prefixed lookalike</my-widget>
<div data-doc="hx-get='/y'">hx inside another attribute's value</div>
<a hx-get="/login?next=https://app.example/">local route with URL in query</a>
<a hx-get="/orders/{{$id := .ID}}/items">declaration action</a>
</body></html>
`

func TestHtmxExtractor_SupportedLanguages(t *testing.T) {
	got := (&HtmxExtractor{}).SupportedLanguages()
	want := []string{"html", "gotmpl", "templ"}
	if len(got) != len(want) {
		t.Fatalf("SupportedLanguages() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("SupportedLanguages()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestHtmxExtractor_Extract(t *testing.T) {
	fileNodes := []*graph.Node{{ID: "ui/internal/templates/parts.html", Kind: graph.KindFile}}
	out := (&HtmxExtractor{}).Extract("ui/internal/templates/parts.html", []byte(htmxFixture), fileNodes, nil)
	if len(out) != 11 {
		t.Fatalf("got %d contracts, want 11 (10 distinct ids + the line-3/line-10 re-occurrence pair; none from skip, external, lookalike, commented, query-URL, or declaration shapes): %+v", len(out), out)
	}

	byID := make(map[string]Contract)
	for _, c := range out {
		byID[c.ID] = c
	}

	// The full expected ID set. Export + the duplicate div share one
	// contract ID (dup is a second occurrence of the same route; both
	// recorded, keyed by ID here). /after-comment-block proves the
	// multi-line comment strip (line 16, true to the file), /health2
	// pins the data-hx-* prefix form, the PUT/PATCH pair pins the two
	// verbs the fixture previously never exercised positively.
	//
	// http::GET::/y is the KNOWN LIMITATION pinned deliberately: an
	// hx-* attribute written inside ANOTHER attribute's value
	// (data-doc="hx-get='/y'") is quote-preceded and still matches.
	// The attribute boundary cannot see tag context without a real
	// HTML parse; full tag-context scanning is an upstream follow-up.
	wantIDs := map[string]bool{
		"http::GET::/ui/parts/{p1}/exp":     false,
		"http::POST::/ui/parts/{p1}/reset":  false,
		"http::DELETE::/ui/parts/{p1}":      false,
		"http::GET::/health":                false,
		"http::GET::/after-comment-block":   false,
		"http::GET::/health2":               false,
		"http::PUT::/ui/parts/{p1}/archive": false,
		"http::PATCH::/ui/parts/{p1}":       false,
		"http::GET::/y":                     false,
		// N1 pin: a local route with a URL embedded in its query string
		// must survive the external-URL check (query stripped first).
		"http::GET::/login": false,
	}
	for id := range wantIDs {
		c, ok := byID[id]
		if !ok {
			t.Fatalf("missing contract %q; got IDs %v", id, keysOf(byID))
		}
		if c.Role != RoleConsumer || c.Type != ContractHTTP {
			t.Fatalf("%q: role=%v type=%v, want consumer/http", id, c.Role, c.Type)
		}
		if c.FilePath != "ui/internal/templates/parts.html" {
			t.Fatalf("%q: FilePath=%q", id, c.FilePath)
		}
		if c.Meta["framework"] != "htmx" || c.Meta["raw_path"] == nil {
			t.Fatalf("%q: Meta=%v", id, c.Meta)
		}
		if c.Meta["method"] == nil || c.Meta["method"] == "" {
			t.Fatalf("%q: Meta[method] empty", id)
		}
		if c.Confidence != 0.9 {
			t.Fatalf("%q: Confidence=%v, want 0.9", id, c.Confidence)
		}
	}

	// Query string stripped before normalization.
	if c := byID["http::POST::/ui/parts/{p1}/reset"]; c.Meta["raw_path"] != "/ui/parts/{{.P.ID}}/reset" {
		t.Fatalf("query not stripped: raw_path=%v", c.Meta["raw_path"])
	}

	// Skipped shapes produce nothing — including a query-only value, whose
	// stripped raw_path ("") must never reach a contract, the leading-space
	// variant whose untrimmed form used to bypass the guard, the two
	// knowably-external URLs (literal scheme + protocol-relative — pairing
	// either with a local provider after host-stripping would be a false
	// match), the hyphen-prefixed attribute lookalike track-hx-get,
	// which the quote/whitespace attribute boundary must not admit, and
	// the declaration action ({{$id := .ID}}) — it assigns rather than
	// interpolates, so the whole value must be rejected, not collapsed
	// to a param.
	for _, id := range byID {
		if id.Meta["raw_path"] == "#section" || id.Meta["raw_path"] == "" ||
			id.Meta["raw_path"] == "?sort=mpn&dir=asc" ||
			id.Meta["raw_path"] == "javascript:void(0)" ||
			id.Meta["raw_path"] == "JavaScript:void(0)" ||
			id.Meta["raw_path"] == "/commented/route" ||
			id.Meta["raw_path"] == "/also/commented" ||
			id.Meta["raw_path"] == "/api/{{if .V2}}/v2{{else}}/v1{{end}}/items" ||
			id.Meta["raw_path"] == "https://api.vendor.com/v1/charge" ||
			id.Meta["raw_path"] == "//cdn.example.com/x" ||
			id.Meta["raw_path"] == "/x" ||
			id.Meta["raw_path"] == "/v1/charge" ||
			id.Meta["raw_path"] == "/orders/{{$id := .ID}}/items" ||
			id.Meta["raw_path"] == "{{ if .Edit }}/edit{{ else }}/new{{ end }}" {
			t.Fatalf("skip-shape extracted: %+v", id)
		}
	}
	// No contract for this fixture may normalize to the root path: a
	// query-only hx value must not widen to a junk http::<VERB>::/
	// consumer that could falsely pair with a real homepage provider.
	for id := range byID {
		if strings.HasSuffix(id, "::/") {
			t.Fatalf("root-path contract %q extracted from fixture", id)
		}
	}

	// Every contract anchors on the file node (no enclosing symbol in HTML).
	for id, c := range byID {
		if c.SymbolID != "ui/internal/templates/parts.html" {
			t.Fatalf("%q: SymbolID=%q, want file-node fallback", id, c.SymbolID)
		}
	}

	// Line numbers point at the attributes and are computed from the
	// ORIGINAL source (lineNumber is 1-based), even though scanning runs
	// on the comment-stripped copy: /health sits on line 5, the
	// after-comment-block attribute on line 16 — AFTER a comment
	// spanning lines 13-15, whose equal-length space replacement keeps
	// every byte offset true to the file — and the data-hx pin on line 20.
	// The PUT pin (line 21) and the known-limitation /y pin (line 26,
	// inside another attribute's value) anchor on the attribute-name
	// group, not the whole match, whose leading delimiter character
	// could sit on the previous line.
	if c := byID["http::GET::/health"]; c.Line != 5 {
		t.Fatalf("GET /health Line=%d, want 5", c.Line)
	}
	if c := byID["http::GET::/after-comment-block"]; c.Line != 16 {
		t.Fatalf("GET /after-comment-block Line=%d, want 16 (after multi-line comment)", c.Line)
	}
	if c := byID["http::GET::/health2"]; c.Line != 20 {
		t.Fatalf("GET /health2 Line=%d, want 20 (data-hx-get pin)", c.Line)
	}
	if c := byID["http::PUT::/ui/parts/{p1}/archive"]; c.Line != 21 {
		t.Fatalf("PUT archive Line=%d, want 21", c.Line)
	}
	if c := byID["http::GET::/y"]; c.Line != 26 {
		t.Fatalf("GET /y Line=%d, want 26 (known limitation)", c.Line)
	}
	// N1 pin: the query-URL value on line 27 must yield the local route
	// path, not be discarded as knowably external.
	if c := byID["http::GET::/login"]; c.Line != 27 {
		t.Fatalf("GET /login Line=%d, want 27 (query-URL pin)", c.Line)
	}
}

// TestNormalizeHtmxPath pins the extractor-local template pre-pass:
// whole-segment expressions collapse to positional params so consumer
// IDs collide with provider route IDs, while control-flow templates
// that survive normalization are rejected (ok=false) rather than
// minting a junk contract.
func TestNormalizeHtmxPath(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		want   string
		wantOK bool
	}{
		{"go template param", "/ui/parts/{{.P.ID}}/exp", "/ui/parts/{p1}/exp", true},
		{"go template param with spaces", "/orders/{{ order.ID }}", "/orders/{p1}", true},
		{"bare go template param", "/items/{{.ID}}", "/items/{p1}", true},
		// {%…%} and <%…%> are statement syntax of template families this
		// extractor does not scan (SupportedLanguages covers Go-template
		// languages only) — the segments stay literal and the post-
		// normalization residue check rejects the value.
		{"jinja statement segment not scanned", "/shop/{% sku %}/edit", "", false},
		{"erb statement segment not scanned", "/shop/<%= sku %>/edit", "", false},
		// A control action renders nothing and must not become a param:
		// left literal, the residue check rejects the whole value.
		{"control action segment rejected", "/api/{{if .V2}}/items", "", false},
		// A declaration action ({{$id := .ID}}) assigns rather than
		// interpolates — left literal so the residue check rejects the
		// value instead of minting a bogus param slot.
		{"declaration action segment rejected", "/orders/{{$id := .ID}}/items", "", false},
		{"two template params", "/a/{{.A}}/b/{{.B}}", "/a/{p1}/b/{p2}", true},
		{"declared then template param", "/w/{wid}/t/{{.TID}}", "/w/{p1}/t/{p2}", true},
		// A partial-segment expression leaves {{...}} in the normalized
		// path, so normalizeHtmxPath rejects it (no trustworthy route ID)
		// rather than minting a contract with template syntax in its ID.
		// The same residue rejection covers two value expressions
		// concatenated inside ONE segment: [^{}]* refuses the nested
		// braces, the segment stays literal, the value is rejected.
		{"partial-segment template rejected", "/items-{{.ID}}", "", false},
		{"concatenated expressions in one segment rejected", "/x/{{.A}}{{.B}}", "", false},
		{"control flow survives normalization", "/api/{{if .V2}}/v2{{else}}/v1{{end}}/items", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := normalizeHtmxPath(tc.in)
			if ok != tc.wantOK {
				t.Fatalf("normalizeHtmxPath(%q) ok = %v, want %v (path %q)", tc.in, ok, tc.wantOK, got)
			}
			if got != tc.want {
				t.Fatalf("normalizeHtmxPath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}

	// Position parity: the collapsed consumer path must equal what the
	// provider side normalizes for a declared {id} param — same {p1}
	// slot, so the matcher pairs them.
	provider, _ := NormalizeHTTPPathWithParams("/ui/parts/{id}/exp")
	consumer, ok := normalizeHtmxPath("/ui/parts/{{.P.ID}}/exp")
	if !ok || consumer != provider {
		t.Fatalf("consumer %q (ok=%v) != provider %q — matcher will orphan the route", consumer, ok, provider)
	}

	// Whole-value expressions are owned by the skip layer, not this
	// function: skipHtmxValue drops them before normalizeHtmxPath runs.
	if !skipHtmxValue("{{ if .Edit }}/edit{{ else }}/new{{ end }}") {
		t.Fatalf("whole-value expression should be skipped before normalizeHtmxPath")
	}
}

func TestHtmxConsumerIDCollidesWithProvider(t *testing.T) {
	tmpl := `<button hx-get="/ui/parts/{{.P.ID}}/exp">Export</button>`
	consumers := (&HtmxExtractor{}).Extract("ui/internal/templates/parts.html", []byte(tmpl), nil, nil)
	if len(consumers) != 1 {
		t.Fatalf("got %d contracts, want 1: %+v", len(consumers), consumers)
	}

	// Provider side: what route_ast_go / http_filebased build for a
	// declared route — same normalizer, same ID format.
	norm, _ := NormalizeHTTPPathWithParams("/ui/parts/{id}/exp")
	providerID := "http::GET::" + norm
	if consumers[0].ID != providerID {
		t.Fatalf("consumer ID %q != provider ID %q — matcher will orphan the route",
			consumers[0].ID, providerID)
	}

	// Registry round-trip: both sides land in the same workspace bucket.
	reg := NewRegistry()
	reg.AddAllScoped([]Contract{{
		ID: providerID, Type: ContractHTTP, Role: RoleProvider,
		FilePath: "ui/internal/router.go", Line: 42,
	}}, "go-parts", "", "")
	reg.AddAllScoped(consumers, "go-parts", "", "")
	bucket := reg.ByWorkspace("go-parts")
	if len(bucket) != 2 {
		t.Fatalf("workspace bucket has %d contracts, want 2", len(bucket))
	}

	// Real matcher pass: the htmx consumer must rescue the provider —
	// the pair appears in Matched and the provider is NOT an orphan.
	res := Match(reg)
	matched := false
	for _, link := range res.Matched {
		if link.ContractID == providerID && link.Consumer.ID == consumers[0].ID {
			matched = true
		}
	}
	if !matched {
		t.Fatalf("Match() did not pair provider %q with the htmx consumer; matched=%d orphans(prov=%d cons=%d)",
			providerID, len(res.Matched), len(res.OrphanProviders), len(res.OrphanConsumers))
	}
	for _, p := range res.OrphanProviders {
		if p.ID == providerID {
			t.Fatalf("provider %q left in OrphanProviders despite the htmx consumer", providerID)
		}
	}
}

// keysOf is already declared in http_test.go (same signature, same body);
// reuse it here rather than redeclaring it in this package.
