package store_sqlite

import (
	"fmt"
	"strings"
	"testing"
)

// Receiver-rebind join-order lock, read off the SQL text.
//
// This sits next to the EXPLAIN locks in
// method_receiver_rebind_plan_lock_test.go and guards a hole they cannot
// close. Those locks run the real queries under five statistics regimes and
// assert the resulting plan shape, but they bind the CROSS JOIN keywords only
// CONJUNCTIVELY: mutation testing showed that reverting only the receiver-side
// `CROSS JOIN nodes AS c` keywords, or only the method/edge-side ones, leaves
// every plan lock green in every regime. SQLite simply never happened to
// choose the O(types x member_of) order while the remaining keywords still
// pinned part of the loop nesting. Only reverting all of them together flips
// a plan. An accidental edit that dropped ONE keyword would therefore ship.
//
// No statistics regime we can construct discriminates the individual
// keywords, so the honest guard is a static assertion on the join order the
// SQL text declares. That is legitimate rather than a tautology because
// CROSS JOIN is not a semantic choice here: it returns exactly the same rows
// as JOIN and only constrains the planner's loop order (SQLite's documented
// "the right relation is always the inner loop"). The keyword carries no
// meaning a reviewer could read off the result set — it exists purely to pin
// the order — so pinning the keyword itself is pinning the whole of its
// contribution.
//
// The one join that must NOT be CROSS is `t`. Its ON clause carries the
// phantom-target probe (`t.id = e.to_id AND t.view_gen = ?` with the
// `t.id IS NULL` test in the WHERE), so turning it inner would change the
// result set, not just the loop order.
//
// The test name carries "PlanLock" because the Windows CI leg runs only
// -run 'PlanLock|PlansLocked|PlansNeverScan' in this package.
func TestReceiverRebindPlanLockJoinOrderKeywords(t *testing.T) {
	cases := []struct {
		// constant is the Go identifier of the SQL constant, so a failure
		// names the thing to open rather than a nickname.
		constant string
		sql      string
		want     []sqlJoinRelation
	}{
		{
			constant: "goMethodReceiverCandidatesGlobalSQL",
			sql:      goMethodReceiverCandidatesGlobalSQL,
			want: []sqlJoinRelation{
				{Keyword: "FROM", Alias: "e"},
				{Keyword: "CROSS JOIN", Alias: "m"},
				{Keyword: "LEFT JOIN", Alias: "t"},
				{Keyword: "CROSS JOIN", Alias: "c"},
			},
		},
		{
			constant: "goMethodReceiverCandidatesForFileSQL",
			sql:      goMethodReceiverCandidatesForFileSQL,
			want: []sqlJoinRelation{
				{Keyword: "FROM", Alias: "m"},
				{Keyword: "CROSS JOIN", Alias: "e"},
				{Keyword: "LEFT JOIN", Alias: "t"},
				{Keyword: "CROSS JOIN", Alias: "c"},
			},
		},
		{
			constant: "goMethodReceiverCandidatesForFilesSQL",
			sql:      goMethodReceiverCandidatesForFilesSQL,
			want: []sqlJoinRelation{
				{Keyword: "FROM", Alias: "f"},
				{Keyword: "CROSS JOIN", Alias: "m"},
				{Keyword: "CROSS JOIN", Alias: "e"},
				{Keyword: "LEFT JOIN", Alias: "t"},
				{Keyword: "CROSS JOIN", Alias: "c"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.constant, func(t *testing.T) {
			got, err := parseSQLFromClauseJoins(tc.sql)
			if err != nil {
				t.Fatalf("%s: parse FROM clause: %v", tc.constant, err)
			}

			// Report every positional mismatch rather than the first: a
			// reordering shifts several positions at once and seeing only
			// the leading one hides what actually moved.
			for i := 0; i < len(got) || i < len(tc.want); i++ {
				switch {
				case i >= len(got):
					t.Errorf("%s: relation %d missing: want %s %s, got nothing (FROM clause declares %d relations, want %d)",
						tc.constant, i, tc.want[i].Keyword, tc.want[i].Alias, len(got), len(tc.want))
				case i >= len(tc.want):
					t.Errorf("%s: relation %d unexpected: got %s %s, want nothing (FROM clause declares %d relations, want %d)",
						tc.constant, i, got[i].Keyword, got[i].Alias, len(got), len(tc.want))
				case got[i] != tc.want[i]:
					t.Errorf("%s: relation %d = %s %s, want %s %s\nfull FROM clause order: %s",
						tc.constant, i, got[i].Keyword, got[i].Alias,
						tc.want[i].Keyword, tc.want[i].Alias, formatSQLJoinRelations(got))
				}
			}

			for i, rel := range got {
				// An unqualified JOIN (or its INNER spelling, or the comma
				// list that means the same thing) is exactly the pre-#651
				// state: same rows, no order constraint, so the planner is
				// free to hoist the relation above its driver again whenever
				// sqlite_stat1 makes that look cheap.
				if rel.Keyword == "JOIN" || rel.Keyword == "INNER JOIN" || rel.Keyword == "," {
					t.Errorf("%s: relation %d (%s) uses %q, which pins no loop order; every inner relation here must be CROSS JOIN (issue #651)",
						tc.constant, i, rel.Alias, rel.Keyword)
				}
				// `t` is the one relation whose join type is semantic: the
				// phantom-target probe lives in its ON clause, so an inner
				// join there drops the rows the rebind pass exists to repair.
				if rel.Alias == "t" && rel.Keyword != "LEFT JOIN" {
					t.Errorf("%s: relation %d (t) uses %q, want %q — the phantom-target probe rides in t's ON clause, so an inner join changes the result set, not just the loop order",
						tc.constant, i, rel.Keyword, "LEFT JOIN")
				}
			}
		})
	}
}

// sqlJoinRelation is one relation of a FROM clause: the keyword that
// introduced it and the alias it was bound to.
type sqlJoinRelation struct {
	Keyword string
	Alias   string
}

func formatSQLJoinRelations(rels []sqlJoinRelation) string {
	parts := make([]string, 0, len(rels))
	for _, rel := range rels {
		parts = append(parts, fmt.Sprintf("{%s %s}", rel.Keyword, rel.Alias))
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// parseSQLFromClauseJoins tokenizes a statement's top-level FROM clause into
// the ordered sequence of (join keyword, alias) pairs it declares.
//
// It is deliberately a small recognizer for the shape these queries are
// written in — `FROM <table> AS <alias>` followed by `<join keyword> <table>
// AS <alias>`, with optional `INDEXED BY <index>` and `ON <predicate>`
// suffixes — rather than a SQL parser. Anything it does not recognize is an
// error, so a rewrite into a shape this lock cannot read fails loudly instead
// of silently asserting nothing.
func parseSQLFromClauseJoins(query string) ([]sqlJoinRelation, error) {
	tokens := tokenizeSQL(query)
	start := -1
	depth := 0
	for i, tok := range tokens {
		switch tok {
		case "(":
			depth++
			continue
		case ")":
			depth--
			continue
		}
		if depth == 0 && strings.EqualFold(tok, "FROM") {
			start = i
			break
		}
	}
	if start < 0 {
		return nil, fmt.Errorf("no top-level FROM clause")
	}

	var rels []sqlJoinRelation
	depth = 0
	for i := start; i < len(tokens); {
		tok := tokens[i]
		switch tok {
		case "(":
			depth++
			i++
			continue
		case ")":
			depth--
			i++
			continue
		}
		if depth > 0 {
			i++
			continue
		}
		upper := strings.ToUpper(tok)
		if i > start && isSQLFromClauseTerminator(upper) {
			break
		}

		keyword, consumed := matchSQLJoinKeyword(tokens[i:], i == start)
		if keyword == "" {
			// Part of an INDEXED BY / ON suffix; those carry no relation.
			i++
			continue
		}
		i += consumed
		alias, used, err := parseSQLRelationAlias(tokens[i:])
		if err != nil {
			return nil, fmt.Errorf("relation %d (after %q): %w", len(rels), keyword, err)
		}
		rels = append(rels, sqlJoinRelation{Keyword: keyword, Alias: alias})
		i += used
	}
	if len(rels) == 0 {
		return nil, fmt.Errorf("FROM clause declares no relations")
	}
	return rels, nil
}

// matchSQLJoinKeyword recognizes the join keyword starting at tokens[0] and
// reports how many tokens it spans. A bare "," is reported as such: the
// comma-separated FROM list is an implicit inner join that pins nothing, and
// naming it beats silently skipping it.
func matchSQLJoinKeyword(tokens []string, atStart bool) (string, int) {
	if len(tokens) == 0 {
		return "", 0
	}
	upper := strings.ToUpper(tokens[0])
	next := ""
	if len(tokens) > 1 {
		next = strings.ToUpper(tokens[1])
	}
	after := ""
	if len(tokens) > 2 {
		after = strings.ToUpper(tokens[2])
	}
	switch {
	case atStart && upper == "FROM":
		return "FROM", 1
	case tokens[0] == ",":
		return ",", 1
	case upper == "CROSS" && next == "JOIN":
		return "CROSS JOIN", 2
	case upper == "INNER" && next == "JOIN":
		return "INNER JOIN", 2
	case (upper == "LEFT" || upper == "RIGHT" || upper == "FULL") && next == "OUTER" && after == "JOIN":
		return upper + " OUTER JOIN", 3
	case (upper == "LEFT" || upper == "RIGHT" || upper == "FULL") && next == "JOIN":
		return upper + " JOIN", 2
	case upper == "JOIN":
		return "JOIN", 1
	}
	return "", 0
}

// parseSQLRelationAlias reads `<table> [AS] <alias>` and reports how many
// tokens it consumed. A subquery or table-valued function in relation
// position is rejected rather than guessed at.
func parseSQLRelationAlias(tokens []string) (string, int, error) {
	if len(tokens) == 0 {
		return "", 0, fmt.Errorf("no relation follows the join keyword")
	}
	table := tokens[0]
	if table == "(" {
		return "", 0, fmt.Errorf("subquery in relation position is not supported by this lock")
	}
	if len(tokens) > 1 && strings.EqualFold(tokens[1], "AS") {
		if len(tokens) < 3 {
			return "", 0, fmt.Errorf("relation %q ends after AS with no alias", table)
		}
		return tokens[2], 3, nil
	}
	if len(tokens) > 1 && !isSQLRelationSuffix(strings.ToUpper(tokens[1])) {
		return tokens[1], 2, nil
	}
	// No alias: the relation is referred to by its own name.
	return table, 1, nil
}

// isSQLRelationSuffix reports whether the token can only follow a relation
// rather than be its alias.
func isSQLRelationSuffix(upper string) bool {
	switch upper {
	case "ON", "USING", "INDEXED", "NOT", ",", ")",
		"CROSS", "INNER", "LEFT", "RIGHT", "FULL", "NATURAL", "JOIN":
		return true
	}
	return isSQLFromClauseTerminator(upper)
}

func isSQLFromClauseTerminator(upper string) bool {
	switch upper {
	case "WHERE", "GROUP", "HAVING", "WINDOW", "ORDER", "LIMIT",
		"UNION", "INTERSECT", "EXCEPT", "RETURNING":
		return true
	}
	return false
}

// tokenizeSQL splits on whitespace and isolates the punctuation the FROM
// clause recognizer has to see: parentheses (nesting) and commas (implicit
// joins).
func tokenizeSQL(query string) []string {
	var tokens []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			tokens = append(tokens, cur.String())
			cur.Reset()
		}
	}
	for _, r := range query {
		switch r {
		case ' ', '\t', '\n', '\r':
			flush()
		case '(', ')', ',':
			flush()
			tokens = append(tokens, string(r))
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return tokens
}
