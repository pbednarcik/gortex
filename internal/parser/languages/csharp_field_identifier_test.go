package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// C# fields are read through bare identifiers, not dotted access: an
// injected field is used as a call receiver (`_store.Add(1)`), an access
// receiver (`_store.Count`), or an assignment target (`_store = store`).
// None of those positions previously emitted any edge naming the FIELD
// itself, so find_usages on a field answered empty no matter how often
// the class used it. Only the unidiomatic `this._store` spelling left a
// trace. These tests pin the field-identifier emission: reads/writes of
// the enclosing type's own fields, with declared locals, parameters, and
// builtin-typed locals shadowing the field name.
func TestCSharpExtractor_FieldIdentifierUses(t *testing.T) {
	src := []byte(`namespace App {
    public class Store {
        public void Add(int n) { }
        public int Count { get; set; }
    }
    public class Ledger {
        private readonly Store _store;
        private int _total;
        private int _unused;

        public Ledger(Store store) { _store = store; }

        public void Post() { _store.Add(1); }
        public int Peek() { return _store.Count; }
        public void Tally() { _total = 5; }
        public void Shadowed() { var _store = new Store(); _store.Add(2); }
        public void ShadowParam(Store _store) { _store.Add(3); }
    }
}
`)
	e := NewCSharpExtractor()
	result, err := e.Extract("Ledger.cs", src)
	require.NoError(t, err)

	// Call-receiver read: `_store.Add(1)` reads the field _store.
	post := accessEdges(result.Edges, "Ledger.cs::Ledger.Post", "_store")
	require.Len(t, post, 1, "a field used as a call receiver is one read of the field")
	assert.Equal(t, graph.EdgeReads, post[0].Kind)
	require.NotNil(t, post[0].Meta)
	assert.Equal(t, "Ledger", post[0].Meta["receiver_type"],
		"the field's implicit receiver is the enclosing type")

	// Access-receiver read: `_store.Count` reads the field too (the Count
	// read is the access emitter's existing edge, asserted separately).
	peek := accessEdges(result.Edges, "Ledger.cs::Ledger.Peek", "_store")
	require.Len(t, peek, 1, "a field used as an access receiver is one read of the field")
	assert.Equal(t, graph.EdgeReads, peek[0].Kind)
	count := accessEdges(result.Edges, "Ledger.cs::Ledger.Peek", "Count")
	require.Len(t, count, 1, "the member access itself still emits its own read")

	// Constructor assignment: `_store = store` writes the field.
	ctor := accessEdges(result.Edges, "Ledger.cs::Ledger.<init>", "_store")
	require.Len(t, ctor, 1, "bare assignment lhs is one write of the field")
	assert.Equal(t, graph.EdgeWrites, ctor[0].Kind)
	assert.Equal(t, "Ledger", ctor[0].Meta["receiver_type"])

	// Bare-identifier assignment beyond the ctor: `_total = 5`.
	tally := accessEdges(result.Edges, "Ledger.cs::Ledger.Tally", "_total")
	require.Len(t, tally, 1)
	assert.Equal(t, graph.EdgeWrites, tally[0].Kind)

	// Shadowing: a declared local or parameter with the field's name owns
	// the identifier — no field edge may be minted from those methods.
	assert.Empty(t, accessEdges(result.Edges, "Ledger.cs::Ledger.Shadowed", "_store"),
		"a local named like the field shadows it")
	assert.Empty(t, accessEdges(result.Edges, "Ledger.cs::Ledger.ShadowParam", "_store"),
		"a parameter named like the field shadows it")

	// Control: the untouched field keeps its honest empty.
	for _, ed := range result.Edges {
		if ed.To == "unresolved::*._unused" {
			t.Fatalf("unused field gained an edge from %s", ed.From)
		}
	}
}
