package artifactdata

import (
	"encoding/json"
	"time"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/costlimit"
)

// THE COST-WEIGHTED WRITE LIMITS (D28): per node, per principal on a store and
// per store, token buckets weighed by what a write costs the applier rather
// than by how many records it is. A record's cost is the rows its apply
// writes, and every data node applies every record through one writer, so a
// write is weighed by those rows.
//
// THE ROWS ARE THE DOCUMENT'S, NOT THE OP'S. An apply rewrites a changed
// document's whole field index (deleted and inserted in one statement each,
// writeDoc), so a merge naming one cell of a twelve-column row writes the
// document row and every one of its index entries twice; spike DS-1 measured
// 21 rows for such a cell edit and 2,804 for a pasted block of 100 rows. The
// writer therefore reads, from this node's copy, how many index entries each
// document the batch names holds now ([Writer.cost]), and [Cost] weighs each
// op by them. A count read from a copy a little behind the log is still the
// right order of magnitude, which is all a guard needs.
//
// PER NODE, AND SAID SO: a bucket (internal/costlimit's, which the dashboard's
// live source reads share) protects this node's applier queue and the fleet's
// one log from one client. It is not a fleet quota (that is the stamped
// footprint), it is not shared across nodes, and it never locks anybody out: a
// refusal names when the bucket holds the cost again.

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
