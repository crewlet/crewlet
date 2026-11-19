package artifactdata

import (
	"encoding/json"
	"time"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/costlimit"
)

// THE COST-WEIGHTED WRITE LIMITS (D28): per node, per principal on a store and
// per store, token buckets weighed by the rows a write's apply writes.
//
// THE ROWS ARE THE DOCUMENT'S, NOT THE OP'S. An apply rewrites a changed
// document's whole field index (writeDoc), so one cell of a twelve-column row
// writes the document row and every index entry twice (spike DS-1: 21 rows for
// a cell edit, 2,804 for a pasted block of 100). The writer reads how many
// index entries each named document holds ([Writer.cost]) and [Cost] weighs
// each op by them; a count slightly behind the log is good enough for a guard.
//
// PER NODE: it protects this node's applier queue and the one log from one
// client, not a fleet quota (the stamped footprint is).

// rowsPerUnit is how many rows one unit of cost covers: a cell edit on a
// twelve-column row (about thirty rows: the document, its index deleted and
// written again, a touch and the history row) is two units, and a pasted
// block of a hundred such rows is about two hundred.
const rowsPerUnit = 16

// Cost is what a batch weighs, given held, how many field-index entries a
// named document holds now (zero for one that does not exist): the rows its
// apply writes, one unit per [rowsPerUnit] of them, at least one.
//
// Per op: the document row, its index entries deleted, the entries it holds
// after the op (an upper bound: what it held plus what the op writes, each
// array element an entry) and its touch row; once per batch, the history row.
func Cost(ops []artifacts.DataOp, held func(collection, doc string) int) int {
	rows := 1
	for _, op := range ops {
		before := held(op.Collection, op.Doc)
		rows += 2 // the document row and its touch
		switch op.Op {
		case artifacts.DataDelete:
			rows += before
		case artifacts.DataSet:
			rows += before + fieldEntries(op.Fields)
		case artifacts.DataMerge:
			rows += 2*before + fieldEntries(op.Fields)
		default:
			// increment, append, add, remove: one field changes, and the
			// whole index is written again.
			rows += 2*before + 1
		}
	}
	return (rows + rowsPerUnit - 1) / rowsPerUnit
}

// fieldEntries is how many index entries the fields an op writes take at most:
// one per value, and one per element of a list.
func fieldEntries(fields map[string]json.RawMessage) int {
	n := 0
	for _, raw := range fields {
		var list []json.RawMessage
		if json.Unmarshal(raw, &list) == nil && len(list) > 0 {
			n += len(list)
			continue
		}
		n++
	}
	return n
}

// newLimiter builds the node's write buckets on a clock: a principal on a
// store and the store itself, at the rates the limits table names.
func newLimiter(now func() time.Time) *costlimit.Limiter {
	return costlimit.New(costlimit.Rates{
		PrincipalRate: artifacts.DataWritesPerPrincipalPerSecond, PrincipalBurst: artifacts.DataWritesBurst,
		ScopeRate: artifacts.DataWritesPerStorePerSecond, ScopeBurst: artifacts.DataWritesStoreBurst,
	}, now)
}

// takeWrite charges a write's cost to the caller's bucket on the store and to
// the store's own, and answers the refusal naming which could not pay.
func takeWrite(l *costlimit.Limiter, store, principal string, cost int) *RateLimited {
	r := l.Take(store, principal, cost)
	if r == nil {
		return nil
	}
	scope := "principal"
	if r.Scope == costlimit.ScopeShared {
		scope = "store"
	}
	return &RateLimited{Scope: scope, Cost: cost, RetryAfter: r.RetryAfter}
}
