package contracts

import "testing"

// TestNormalizeHTTPPathWithParams_TemplateSegments pins the shared
// normalizer's pre-htmx-branch behavior, restored by scoping template
// handling to the htmx extractor: template expressions (Go {{...}},
// Jinja {%...%}, ERB <%...%>) are NOT path parameters and NOT base-URL
// slots at this layer — they stay literal. Only the htmx extractor's
// normalizeHtmxPath collapses them, so every other caller (route_ast_go,
// http_filebased, TS fetch sites, ...) sees template syntax as opaque
// text exactly as it did before the htmx branch existed.
func TestNormalizeHTTPPathWithParams_TemplateSegments(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"go template param stays literal", "/ui/parts/{{.P.ID}}/exp", "/ui/parts/{{.P.ID}}/exp"},
		// Pre-branch quirk, verified against b55b9a0d: a BARE identifier
		// between double braces ({{ID}}) contains an inner {ID} brace
		// param, so the positional renamer rewrites it to {{p1}} — the
		// outer braces survive. Dotted forms ({{.P.ID}}) stay fully
		// literal because "." cannot start the \w+ param name.
		{"bare double-brace id keeps inner brace param", "/u/{{ID}}/x", "/u/{{p1}}/x"},
		{"two template params stay literal", "/a/{{.A}}/b/{{.B}}", "/a/{{.A}}/b/{{.B}}"},
		{"jinja expression segment stays literal", "/shop/{% sku %}/edit", "/shop/{% sku %}/edit"},
		{"erb segment stays literal", "/shop/<%= sku %>/edit", "/shop/<%= sku %>/edit"},
		{"leading base slot no longer stripped", "{{.Base}}/v1/users", "/{{.Base}}/v1/users"},
		{"leading base slot with slash no longer stripped", "/{{.Base}}/v1/users", "/{{.Base}}/v1/users"},
		{"partial-segment stays literal", "/items-{{.ID}}", "/items-{{.ID}}"},
		{"query untouched by normalizer", "/s?q={{.Q}}", "/s?q={{.Q}}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := NormalizeHTTPPathWithParams(tc.in)
			if got != tc.want {
				t.Fatalf("NormalizeHTTPPathWithParams(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
