package storetest

import "strings"

// Decorrelated reports the first line of a plan at which this engine's planner
// has UNNESTED a correlated scalar subquery — turned a per-row aggregate into
// one grouped read of the whole child table, joined back to the outer rows —
// and whether there is one.
//
// # The rule, stated once
//
// A correlated scalar subquery reads, for each outer row, the child rows its
// equality names: `(SELECT COUNT(*) FROM child c WHERE c.parent = p.id)` is a
// seek of an index on `parent` per row of `p`, and its cost is the rows the
// statement answers. The pinned driver (tursogo v0.8.1, whose rule is
// core/translate/optimizer/unnest.rs in turso's source) may rewrite one such
// subquery as a LEFT JOIN against the child table GROUPED BY its correlation
// columns, materialised and searched through an ephemeral index — which reads
// EVERY row of the child table, whatever the outer rows are. It is eligible
// when the subquery selects ONE aggregate (avg, count, max, min, total), has
// no GROUP BY, ORDER BY, OFFSET or DISTINCT, takes no LIMIT but the literal 1,
// and is correlated only by equalities on ONE outer table; and the planner
// plans both forms and keeps the rewrite whenever its cost is no higher. That
// cost is reckoned over the fallback row estimates of a database nobody has
// ANALYZEd, which is every database this engine opens — so whether a
// statement reads one child row per answer or the whole child table is a
// property of no line of its code, and an added index or a driver bump can
// flip it. Measured: the unfiltered `MIN` below is rewritten, the same `MIN`
// over a key-bound outer is not, and `ORDER BY … LIMIT 1` never is.
//
// So a gate does not ask whether a statement LOOKS correlated; it asks the
// plan, through this one predicate, and a site it fires on is rewritten in a
// form the rule cannot reach: a minimum or maximum as `ORDER BY … LIMIT 1`, a
// count or a sum read for the batch of rows already in hand, in one
// `… WHERE parent IN (…) GROUP BY parent` over their keys.
//
// # What it matches
//
// The rewrite's own operand: the subquery becomes a table the driver names
// `scalar_subquery_<n>`, read (as measured, `SEARCH scalar_subquery_t2 USING
// INDEX ephemeral_subquery_t2 (correlation_key_0=?) LEFT-JOIN`) beside the
// outer table. NOT the ephemeral index alone: a subquery written in a FROM
// clause and joined is materialised and searched through an
// `ephemeral_subquery_<n>` index too, and that is a statement saying what it
// reads, not a planner deciding it. [TestTheDecorrelationCheckSeesTheDriverUnnest]
// holds the predicate to the driver in both directions, so a bump that renames
// the rewrite or stops making it fails there rather than leaving every gate
// that asks this blind.
func Decorrelated(plan []string) (string, bool) {
	for _, line := range plan {
		if strings.Contains(line, decorrelatedOperand) {
			return line, true
		}
	}
	return "", false
}

// decorrelatedOperand is the name the driver gives a scalar subquery it has
// rewritten into a join operand.
const decorrelatedOperand = "scalar_subquery_"
