package contracts

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// HtmxExtractor detects htmx request attributes (hx-get, hx-post, hx-put,
// hx-patch, hx-delete) in HTML template files and emits the HTTP consumer
// side of each request: the template instructs the browser to call that
// route, so a route consumed only from a template is not an orphan
// provider. Canonical IDs collide with provider route contracts through
// normalizeHtmxPath, which maps whole-segment template expressions
// ({{.P.ID}}) onto the same {p1} placeholder space as a provider's
// declared {id} params before delegating to NormalizeHTTPPathWithParams.
//
// Coverage gaps: .gohtml carries Go-template htmx pages but has no
// registered language in the parser (nothing maps the extension), and
// .tpl is claimed by the Helm extractor ahead of the forest gotmpl
// grammar — so htmx attributes in those files are not scanned.
type HtmxExtractor struct{}

// htmxAttrRe matches the five request-issuing htmx attributes with either
// quote style. The attribute name must be preceded by a whitespace or
// quote character, and admits exactly hx-* and the official data-hx-*
// form — nothing hyphen-prefixed beyond that (track-hx-get and friends
// never match). Group 1: full attribute name; group 2: verb; group 3:
// double-quoted value; group 4: single-quoted value. Deliberately an
// attribute-level scan, not an HTML parse — Go templates are routinely
// not well-formed HTML until rendered ({{if}} blocks split tags
// mid-element). Known limitation: an hx-* written inside ANOTHER
// attribute's value (data-doc="hx-get='/y'") is quote-preceded and still
// matches; full tag-context scanning is an upstream follow-up.
var htmxAttrRe = regexp.MustCompile(`(?i)[\s"']((?:data-)?hx-(get|post|put|patch|delete))\s*=\s*(?:"([^"]*)"|'([^']*)')`)

// htmxCommentRe matches HTML comments and Go template comments (the
// {{- /* ... */ -}} form, trim markers optional). (?s) lets HTML
// comments span lines. Matches
// are blanked with an equal-length run of spaces before scanning, so a
// commented-out hx-* attribute emits no contract while every byte offset
// — and therefore every Line number, computed from the ORIGINAL source —
// stays true to the file.
var htmxCommentRe = regexp.MustCompile(`(?s)<!--.*?-->|\{\{-?\s*/\*.*?\*/\s*-?\}\}`)

// htmxTemplateSegment matches a path segment that is entirely a Go template
// value expression — {{…}} output forms only. Control actions ({{if…}},
// {{range…}}, {{end}}, …) render nothing and must not become params; other
// template families ({%…%}, <%…%>) are statement syntax and are not scanned
// (only Go-template languages are in SupportedLanguages).
var htmxTemplateSegment = regexp.MustCompile(`^\{\{[^{}]*\}\}$`)

// htmxControlAction reports whether a {{…}} segment is a Go template control
// action rather than a value interpolation.
var htmxControlAction = regexp.MustCompile(`^\{\{-?\s*(if|else|end|range|with|define|template|block|break|continue|nil)\b`)

// htmxExternalSchemeRe matches an absolute URL whose scheme sits at the
// START of the value — the only position where "://" marks a knowably
// external origin. A scheme deeper in the value is inside a query
// parameter (/login?next=https://app.example/) and the route is local.
var htmxExternalSchemeRe = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*://`)

// normalizeHtmxPath applies template-aware segment collapsing BEFORE the
// shared normalizer, so template semantics stay local to this extractor
// and provider-side (and every other consumer's) identity is untouched.
// Returns ok=false for values that cannot produce a trustworthy route ID:
// still containing template syntax after normalization (control flow
// like /api/{{if}}/v2{{else}}/v1{{end}}/items), or normalizing to root
// from an empty-ish value.
//
// Note: a LEADING whole-segment expression ({{.Base}}/v1/users) is treated
// as a path parameter here, not a base-URL slot — so it pairs only with a
// provider declaring a first param (/{base}/v1/users), never with /v1/users.
func normalizeHtmxPath(raw string) (path string, ok bool) {
	segs := strings.Split(raw, "/")
	changed := false
	for i, seg := range segs {
		// Control-action segments ({{if…}}, {{end}}, …) are left LITERAL —
		// skipped from collapsing, not from the loop — so the residue check
		// below still sees the {{ and rejects the whole value.
		if seg == "" || !htmxTemplateSegment.MatchString(seg) || htmxControlAction.MatchString(seg) {
			continue
		}
		// Declaration actions ({{$id := .ID}}, {{x = y}}) assign rather than interpolate — left literal so the residue check rejects the value.
		if strings.Contains(seg, ":=") || strings.Contains(seg, " = ") {
			continue
		}
		segs[i] = "{tplparam}"
		changed = true
	}
	if changed {
		raw = strings.Join(segs, "/")
	}
	norm, _ := NormalizeHTTPPathWithParams(raw)
	if strings.Contains(norm, "{{") || strings.Contains(norm, "{%") || strings.Contains(norm, "<%") {
		return "", false
	}
	return norm, true
}

// SupportedLanguages covers the registered template languages that carry
// htmx attributes in standard quoted form: html (.html/.htm), gotmpl
// (.gotmpl/.tmpl), and templ (.templ).
//
// htmldjango (.djhtml) is deliberately dropped, not overlooked:
// plain-Django providers mint method-less http::ANY::<path> IDs, which
// never collide with this extractor's verb-specific consumers, and
// Django's idiomatic {% url 'name' %} indirection is a skip-shape anyway
// — so scanning .djhtml adds unpairable noise. Revisit when upstream
// bridges ANY providers into verb-specific identity.
func (e *HtmxExtractor) SupportedLanguages() []string {
	return []string{"html", "gotmpl", "templ"}
}

// Extract emits one consumer contract per htmx attribute occurrence,
// deduplicated per (verb, normalized path, line). HTML and Go template
// comments are blanked from the scanned copy first — commented-out
// attributes are dead markup, not consumers.
func (e *HtmxExtractor) Extract(filePath string, src []byte, fileNodes []*graph.Node, _ []*graph.Edge) []Contract {
	var out []Contract
	// Line numbers come from the ORIGINAL source; only the scanned copy
	// is comment-stripped, each comment replaced by an equal-length run
	// of spaces so byte offsets in the stripped copy map 1:1 onto src.
	// Newlines inside a multi-line comment become spaces in the scanned
	// copy — which is exactly why `lines` must be split from src, not
	// from the stripped text, for Line numbers to stay true to the file.
	lines := strings.Split(string(src), "\n")
	scan := string(htmxCommentRe.ReplaceAllFunc(src, func(m []byte) []byte {
		return bytes.Repeat([]byte{' '}, len(m))
	}))
	seen := make(map[string]struct{})
	for _, m := range htmxAttrRe.FindAllStringSubmatchIndex(scan, -1) {
		// Group 1 is the full attribute name ((?:data-)?hx-verb); the
		// verb itself moved to group 2 and the value groups to 3/4 when
		// the leading \b was replaced by the [\s"'] delimiter group.
		verb := strings.ToUpper(scan[m[4]:m[5]])
		raw := ""
		if m[6] != -1 {
			// Trim immediately: a leading-space value (" ?sort=x")
			// otherwise survives the query strip as " " and the
			// normalizer widens it to the root path — a junk contract.
			raw = strings.TrimSpace(scan[m[6]:m[7]])
		}
		if m[8] != -1 {
			raw = strings.TrimSpace(scan[m[8]:m[9]])
		}
		// Query strings and fragments never appear in route registrations.
		// Strip them BEFORE the external-URL check: skipHtmxValue rejects
		// scheme-bearing values, and a legitimate local route with a URL
		// in its query (/login?next=https://app.example/) must survive it.
		if i := strings.IndexAny(raw, "?#"); i >= 0 {
			raw = raw[:i]
		}
		if skipHtmxValue(raw) {
			continue
		}
		// A query-only value ("?sort=mpn") strips to the empty string,
		// which NormalizeHTTPPathWithParams would widen to "/" — a junk
		// root-path consumer that can falsely pair with a real homepage
		// provider. Skip it instead.
		if raw == "" {
			continue
		}
		norm, ok := normalizeHtmxPath(raw)
		if !ok {
			continue
		}
		// Anchor the line number on the attribute-name group (m[2]), not
		// the whole match (m[0]): the leading [\s"'] delimiter is one
		// byte back, which lands on the PREVIOUS line when an attribute
		// directly follows a newline.
		ln := lineNumber(lines, m[2])
		key := fmt.Sprintf("%s::%s::%d", verb, norm, ln)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, Contract{
			ID:         fmt.Sprintf("http::%s::%s", verb, norm),
			Type:       ContractHTTP,
			Role:       RoleConsumer,
			SymbolID:   htmxAnchorSymbol(fileNodes, ln),
			FilePath:   filePath,
			Line:       ln,
			Meta:       map[string]any{"framework": "htmx", "method": verb, "raw_path": raw},
			Confidence: 0.9,
		})
	}
	return out
}

// skipHtmxValue filters attribute values that are not route references:
// knowably-external URLs, empty values, same-page anchors, javascript:
// URIs (case-insensitive — "JavaScript:void(0)" is the same no-op), and
// values that are (or start with) an unrendered control/interpolation
// expression — a dynamically assembled URL has no path we can match.
func skipHtmxValue(v string) bool {
	v = strings.TrimSpace(v)
	// A LEADING scheme or protocol-relative origin is knowably external —
	// pairing it with a local provider after host-stripping would be a false
	// match. Only a scheme at the start counts (htmxExternalSchemeRe): a
	// "://" deeper in the value sits inside a query string
	// (/login?next=https://app.example/), whose route is still local.
	// (Variable bases like {{.Base}}/x are handled by the template rules.)
	if htmxExternalSchemeRe.MatchString(v) || strings.HasPrefix(v, "//") {
		return true
	}
	if v == "" || strings.HasPrefix(v, "#") || strings.HasPrefix(strings.ToLower(v), "javascript:") {
		return true
	}
	for _, open := range []string{"{{", "{%", "<%"} {
		if strings.HasPrefix(v, open) {
			return true
		}
	}
	return false
}

// htmxAnchorSymbol picks the graph anchor for a consumer contract: the
// enclosing template element when one encloses the attribute's line,
// otherwise the file node, otherwise "" (contract still enters the
// registry and gets a KindContract node; only the EdgeConsumes edge is
// skipped by the indexer when SymbolID is empty).
func htmxAnchorSymbol(fileNodes []*graph.Node, ln int) string {
	if sid := findEnclosingSymbol(fileNodes, ln); sid != "" {
		return sid
	}
	for _, n := range fileNodes {
		if n.Kind == graph.KindFile {
			return n.ID
		}
	}
	return ""
}
