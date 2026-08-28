package languages

import (
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// csharpCollectExtraBindingScopes adds the binding forms the lvar
// capture cannot see to the local-scope index: foreach variables,
// declaration patterns (`o is T x`, `o is var x`), out-var declaration
// expressions, lambda parameters, and the parenthesized
// `using (var x = ...)` resource. (`using var x = ...;` IS a
// local_declaration_statement and needs nothing here.)
//
// The index answers "does a local bind this name at this site" for the
// receiver_name shadow refusal, and function-wide for the
// field-identifier emitter. A name bound by any of these forms shadows a
// same-named field exactly like a declared local does, and these forms
// are idiomatic modern C# - the miss fires precisely when such a name
// coincides with an injected-repository-style field (`repo`, `store`,
// `handler`), which is the shape the dispatch gate exists for.
//
// Extents: a declaration pattern and an out-var escape to the enclosing
// block (C# definite-assignment scoping) - the same extent an ordinary
// local gets. A foreach variable, a lambda parameter, and a using
// resource bind only over their own statement or lambda, so they carry
// that node's span rather than the enclosing block's; a call after the
// statement is back on the field and keeps its evidence.
func csharpCollectExtraBindingScopes(root *sitter.Node, src []byte, funcRanges *csharpFuncLookup, scopes csharpLocalScopes) {
	add := func(nameNode *sitter.Node, sc csharpLocalScope) {
		if nameNode == nil {
			return
		}
		name := nameNode.Content(src)
		if name == "" {
			return
		}
		owner := funcRanges.enclosing(int(nameNode.StartPoint().Row) + 1)
		if owner == "" {
			return
		}
		m := scopes[owner]
		if m == nil {
			m = map[string][]csharpLocalScope{}
			scopes[owner] = m
		}
		m[name] = append(m[name], sc)
	}
	spanOf := func(n *sitter.Node) csharpLocalScope {
		return csharpLocalScope{start: int(n.StartByte()), end: int(n.EndByte())}
	}
	walkNodes(root, func(n *sitter.Node) {
		switch n.Type() {
		case "foreach_statement":
			// left is the loop variable - an identifier, or a tuple
			// pattern whose every identifier binds. All of them scope
			// over the statement.
			if left := n.ChildByFieldName("left"); left != nil {
				if left.Type() == "identifier" {
					add(left, spanOf(n))
				} else {
					walkNodes(left, func(c *sitter.Node) {
						if c.Type() == "identifier" {
							add(c, spanOf(n))
						}
					})
				}
			}
		case "declaration_pattern", "var_pattern", "declaration_expression":
			add(n.ChildByFieldName("name"), csharpLocalScopeOf(n))
		case "lambda_expression":
			sc := spanOf(n)
			params := n.ChildByFieldName("parameters")
			if params == nil {
				return
			}
			switch params.Type() {
			case "implicit_parameter", "identifier":
				// `x => ...` - the parameters node IS the name.
				add(params, sc)
			case "parameter_list":
				for i, _nc := 0, int(params.NamedChildCount()); i < _nc; i++ {
					if p := params.NamedChild(i); p != nil && p.Type() == "parameter" {
						add(p.ChildByFieldName("name"), sc)
					}
				}
			}
		case "using_statement":
			// Only the parenthesized resource form carries a
			// variable_declaration child here.
			for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
				c := n.NamedChild(i)
				if c == nil || c.Type() != "variable_declaration" {
					continue
				}
				walkNodes(c, func(d *sitter.Node) {
					if d.Type() == "variable_declarator" {
						if name := d.ChildByFieldName("name"); name != nil && name.Type() == "identifier" {
							add(name, spanOf(n))
						}
					}
				})
			}
		}
	})
}
