package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// Overload evidence: the counts a call site spells and the arity a
// declaration accepts. Together they are what lets the resolver pick one
// member out of a same-name set instead of refusing the whole set.

// The counts that let the resolver split an overload set ride the call
// edge: how many arguments it passes and how many type arguments it
// spells. A call with no explicit type arguments must stamp none — C#
// infers them there, so a count would be an invention.
func TestCSharpExtractor_CallStampsApplicabilityCounts(t *testing.T) {
	src := []byte(`namespace App {
    public class Registry {
        public T Resolve<T>() { return default(T); }
    }
    public class Runner {
        public void Explicit(Registry r) { r.Resolve<string>(); }
        public void TwoArgs(Registry r) { r.Save("a", 1); }
        public void Inferred(Registry r) { r.Save(1); }
    }
}
`)
	e := NewCSharpExtractor()
	result, err := e.Extract("App.cs", src)
	require.NoError(t, err)

	explicit := callEdgesFrom(result.Edges, "App.cs::Runner.Explicit", "Resolve")
	require.Len(t, explicit, 1)
	assert.Equal(t, 0, explicit[0].Meta["arg_count"])
	assert.Equal(t, 1, explicit[0].Meta["type_arg_count"])

	two := callEdgesFrom(result.Edges, "App.cs::Runner.TwoArgs", "Save")
	require.Len(t, two, 1)
	assert.Equal(t, 2, two[0].Meta["arg_count"])
	assert.NotContains(t, two[0].Meta, "type_arg_count",
		"no explicit type arguments means no type-arity evidence")

	inferred := callEdgesFrom(result.Edges, "App.cs::Runner.Inferred", "Save")
	require.Len(t, inferred, 1)
	assert.Equal(t, 1, inferred[0].Meta["arg_count"])
}

// The type-argument count is recorded for every MEMBER call shape, not
// just the simplest one — receiver-qualified, null-conditional, this-
// and base-qualified generic calls are all equally narrowable.
//
// A receiverless call is deliberately excluded: no consumer resolves one
// through the extension binder, and the scope rules that do bind it never
// consult arity, so stamping it would be dead weight on every C# call
// edge in the graph.
func TestCSharpExtractor_TypeArgCountAcrossCallShapes(t *testing.T) {
	src := []byte(`namespace App {
    public interface IBox { }
    public class Registry { public T Resolve<T>() { return default(T); } }
    public class Base { public virtual T Make<T>() { return default(T); } }

    public class Runner : Base {
        public void ViaLocal() { Registry r = new Registry(); r.Resolve<IBox>(); }
        public void ViaConditional(Registry r) { r?.Resolve<IBox>(); }
        public void ViaThis() { this.Helper<IBox>(); }
        public override T Make<T>() { return base.Make<T>(); }
        public void Bare() { Helper<IBox>(); }
        public T Helper<T>() { return default(T); }
    }
}
`)
	e := NewCSharpExtractor()
	result, err := e.Extract("App.cs", src)
	require.NoError(t, err)

	for _, row := range []struct{ caller, method string }{
		{"ViaLocal", "Resolve"},
		{"ViaConditional", "Resolve"},
		{"ViaThis", "Helper"},
		{"Make", "Make"},
	} {
		edges := callEdgesFrom(result.Edges, "App.cs::Runner."+row.caller, row.method)
		require.Len(t, edges, 1, "%s emits one %s call", row.caller, row.method)
		assert.Equal(t, 1, edges[0].Meta["type_arg_count"],
			"%s spells one type argument", row.caller)
	}

	bare := callEdgesFrom(result.Edges, "App.cs::Runner.Bare", "Helper")
	require.Len(t, bare, 1, "the receiverless call still emits its edge")
	// #559: receiverless calls carry the applicability stamps too — the
	// resolver's same-file and locality tiers pick among ordinary
	// overload sets, and without arg_count that pick is declaration
	// order.
	assert.Equal(t, 1, bare[0].Meta["type_arg_count"],
		"a receiverless generic call spells its type argument")
	assert.Equal(t, 0, bare[0].Meta["arg_count"],
		"zero arguments is a measured count, not missing evidence")
}

// The declaration side of the same evidence. `param_required` is stamped
// only when it differs from the total, and `param_variadic` only when a
// trailing `params` array lifts the upper bound.
func TestCSharpExtractor_MethodStampsParamArity(t *testing.T) {
	src := []byte(`namespace App {
    public class Opts { }
    public static class Ext {
        public static void Fixed(this string s, int a, int b) { }
        public static void Optional(this string s, int a, int b = 2) { }
        public static void Variadic(this string s, params object[] rest) { }
        public static void Nullary() { }
    }
}
`)
	e := NewCSharpExtractor()
	result, err := e.Extract("Ext.cs", src)
	require.NoError(t, err)

	byID := map[string]*graph.Node{}
	for _, n := range result.Nodes {
		byID[n.ID] = n
	}

	fixed := byID["Ext.cs::Ext.Fixed"]
	require.NotNil(t, fixed)
	assert.Equal(t, 3, fixed.Meta["param_count"])
	assert.NotContains(t, fixed.Meta, "param_required",
		"every parameter is required — the count already says so")

	opt := byID["Ext.cs::Ext.Optional"]
	require.NotNil(t, opt)
	assert.Equal(t, 3, opt.Meta["param_count"])
	assert.Equal(t, 2, opt.Meta["param_required"], "a defaulted parameter is omissible")

	// tree-sitter-c-sharp flattens a `params` entry into loose type/name
	// siblings. The extractor reconstructs that grammar shape so the
	// resolver can use the variadic applicability window.
	vari := byID["Ext.cs::Ext.Variadic"]
	require.NotNil(t, vari)
	assert.Equal(t, 2, vari.Meta["param_count"])
	assert.Equal(t, 1, vari.Meta["param_required"])
	assert.Equal(t, true, vari.Meta["param_variadic"])

	nullary := byID["Ext.cs::Ext.Nullary"]
	require.NotNil(t, nullary)
	assert.Equal(t, 0, nullary.Meta["param_count"])
}

// An underscore in a method declaration is an ordinary named parameter and a
// real positional slot. Emitting it under its own name keeps the node ID, Name,
// named-argument spelling, and later parameter positions consistent.
func TestCSharpExtractor_DiscardParamKeepsPositions(t *testing.T) {
	src := []byte(`namespace App {
    public class Handler {
        public void Handle(int _, string name, int count) { }
    }
}
`)
	e := NewCSharpExtractor()
	result, err := e.Extract("Handler.cs", src)
	require.NoError(t, err)

	pos := map[string]any{}
	for _, n := range result.Nodes {
		if n.Kind == graph.KindParam {
			pos[n.Name] = n.Meta["position"]
		}
	}
	assert.Equal(t, 0, pos["_"], "the declared underscore parameter keeps its own name and slot")
	assert.Equal(t, 1, pos["name"])
	assert.Equal(t, 2, pos["count"])
}
