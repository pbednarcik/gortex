package resolver

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// C# applicability narrowing — the arity half of overload resolution.
//
// Receiver-type evidence separates extensions that extend DIFFERENT
// types. It says nothing about a set that extends the SAME type, which
// is the dominant .NET shape: `AddFoo(this IServiceCollection)` beside
// `AddFoo(this IServiceCollection, Action<Options>)`. Namespace
// visibility cannot break that tie either — sibling overloads almost
// always share a namespace — so the binder's ambiguity refusal used to
// drop every such call, leaving the declaration looking uncalled.
//
// C# resolves those by APPLICABILITY: a candidate is in the running only
// if the call's argument count fits its parameter list and its type-
// parameter count matches any explicitly spelled type arguments. Both
// are exact, spec-grounded rules — not heuristics — so applying them
// before the visibility tie-break both matches §12.8.10.3 (each scope
// considers only APPLICABLE candidates) and turns the common overload
// pair into a single answer.
//
// Every function here narrows only. Missing evidence, an unparseable
// stamp, or a filter that would empty the set all return the input
// untouched: this can turn a refusal into a bind, never a bind into a
// different bind.
//
// The arity window matches ResolveCppOverload's cppArityCompatible — the
// same [required, declared] range widened by a variadic tail. The two
// stay separate because C++ ranks on parameter TYPES as well, and C#
// alone has to discount the receiver's `this` slot.

// csharpNarrowByApplicability filters a same-name C# candidate set to the
// members the call site could actually invoke.
func csharpNarrowByApplicability(e *graph.Edge, cands []*graph.Node) []*graph.Node {
	if len(cands) < 2 || e == nil || e.Meta == nil {
		return cands
	}
	out := csharpKeepIf(cands, func(c *graph.Node) bool {
		return csharpAcceptsTypeArgCount(e, c)
	})
	return csharpKeepIf(out, func(c *graph.Node) bool {
		return csharpExtensionAcceptsArgCount(e, c)
	})
}

// csharpKeepIf applies one narrowing predicate. An empty result is
// discarded — a rule that excludes everything is evidence the rule
// misread the code, not evidence the call is unresolvable.
func csharpKeepIf(cands []*graph.Node, keep func(*graph.Node) bool) []*graph.Node {
	if len(cands) < 2 {
		return cands
	}
	out := make([]*graph.Node, 0, len(cands))
	for _, c := range cands {
		if c != nil && keep(c) {
			out = append(out, c)
		}
	}
	if len(out) == 0 || len(out) == len(cands) {
		return cands
	}
	return out
}

// csharpAcceptsTypeArgCount reports whether a candidate's own generic
// arity matches the type arguments the call spells explicitly. A call
// with no explicit type arguments proves nothing (C# infers them), and a
// candidate with no type-parameter stamp is treated as non-generic.
func csharpAcceptsTypeArgCount(e *graph.Edge, c *graph.Node) bool {
	want, ok := metaIntValue(e.Meta["type_arg_count"])
	if !ok {
		return true
	}
	return csharpMethodTypeParamCount(c) == want
}

// csharpMethodTypeParamCount counts a method's own type parameters from
// the extractor's comma-joined stamp.
func csharpMethodTypeParamCount(c *graph.Node) int {
	if c == nil || c.Meta == nil {
		return 0
	}
	s, _ := c.Meta["method_type_params"].(string)
	if s == "" {
		return 0
	}
	n := 1
	for _, r := range s {
		if r == ',' {
			n++
		}
	}
	return n
}

// csharpNarrowMethodByApplicability filters a same-name ORDINARY C#
// candidate set (instance/static methods, not extensions) to the members
// the call site could invoke. Same shape as csharpNarrowByApplicability,
// with ordinary-arity semantics in the arg-count half — see
// csharpMethodAcceptsArgCount (#559).
func csharpNarrowMethodByApplicability(e *graph.Edge, cands []*graph.Node) []*graph.Node {
	if len(cands) < 2 || e == nil || e.Meta == nil {
		return cands
	}
	out := csharpKeepIf(cands, func(c *graph.Node) bool {
		return csharpAcceptsTypeArgCount(e, c)
	})
	return csharpKeepIf(out, func(c *graph.Node) bool {
		return csharpMethodAcceptsArgCount(e, c)
	})
}

// csharpMethodAcceptsArgCount reports whether an ordinary C# method can
// accept the call's argument count — the [required, declared] window,
// widened by a trailing `params` array.
//
// Two deliberate differences from csharpExtensionAcceptsArgCount: there
// is no `this` slot to discount, and param_count == 0 is a REAL arity —
// an extension method always declares its `this` parameter, so only
// there does zero mean "no evidence".
func csharpMethodAcceptsArgCount(e *graph.Edge, c *graph.Node) bool {
	argc, ok := metaIntValue(e.Meta["arg_count"])
	if !ok || c == nil || c.Meta == nil {
		return true
	}
	if isCSharpExtension(c) {
		// Extensions are adjudicated by their own binder with the
		// `this`-slot discount; the ordinary window would misread them.
		return true
	}
	count, ok := metaIntValue(c.Meta["param_count"])
	if !ok {
		return true // no stamp — an older graph or an unexposed parameter list
	}
	required := count
	if r, rok := metaIntValue(c.Meta["param_required"]); rok {
		required = r
	}
	if variadic, _ := c.Meta["param_variadic"].(bool); variadic {
		return argc >= required
	}
	return argc >= required && argc <= count
}

// csharpExtensionAcceptsArgCount reports whether a candidate extension
// method can accept the call's argument count.
//
// The window is [required, count], widened to [required, +inf) by a
// trailing `params` array. In EXTENSION form (`x.Foo(a)`) the receiver
// fills the `this` slot, so both ends drop by one; in STATIC form
// (`Ext.Foo(x, a)`) the argument list covers every parameter and the
// window is used as declared. Reading the wrong form would exclude an
// overload for the very count it accepts, and — worse — could leave a
// sibling as the sole survivor.
//
// A candidate with no parameter stamp — an older graph, or a declaration
// whose parameter list the grammar did not expose — accepts anything.
func csharpExtensionAcceptsArgCount(e *graph.Edge, c *graph.Node) bool {
	argc, ok := metaIntValue(e.Meta["arg_count"])
	if !ok || c == nil || c.Meta == nil {
		return true
	}
	count, ok := metaIntValue(c.Meta["param_count"])
	if !ok || count == 0 {
		// An extension method always declares at least the `this`
		// parameter, so a zero here means "no evidence", not "takes
		// nothing".
		return true
	}
	required := count
	if r, rok := metaIntValue(c.Meta["param_required"]); rok {
		required = r
	}
	if !csharpCallIsStaticForm(e, c) {
		count--
		required--
		if required < 0 {
			required = 0
		}
	}
	if variadic, _ := c.Meta["param_variadic"].(bool); variadic {
		return argc >= required
	}
	return argc >= required && argc <= count
}

// csharpCallIsStaticForm reports whether the call invokes the extension
// through its declaring class (`BagExt.Add(bag)`) rather than on a
// receiver value (`bag.Add()`).
//
// The extractor stamps receiver_name only for a receiver that no local,
// parameter, builtin or chain walk explains — so a receiver_name whose
// trailing segment is the candidate's own declaring class is a type
// reference, not a shadowing variable. Any receiver the extractor DID
// type is a value by construction, and carries no receiver_name at all.
//
// The trailing segment is what matters because the class may be written
// namespace-qualified: `Lib.BagExt.Add(bag)` and `BagExt.Add(bag)` are
// the same call and must reach the same overload.
func csharpCallIsStaticForm(e *graph.Edge, c *graph.Node) bool {
	recv, _ := e.Meta["receiver_name"].(string)
	if recv == "" {
		return false
	}
	if i := strings.LastIndex(recv, "."); i >= 0 {
		recv = recv[i+1:]
	}
	owner, _ := c.Meta["receiver"].(string)
	return owner != "" && recv == owner
}
