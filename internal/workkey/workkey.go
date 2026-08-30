// Package workkey is THE work-key grammar: what identifies the unit of work
// a turn did.
//
// A turn is NOT identified by its turn id. Two nodes that both run the same
// trigger mint two turn ids, so anything keyed on one records the duplicate
// rather than collapsing it. What IS stable across a re-run is the set of
// trigger events the turn was dispatched for — the same identity the
// completion ledger keys on, derived here so the two cannot disagree.
//
// Two writes carry it: the episode row (exactly one per turn, so a unique
// index makes a second writer's insert a no-op) and the counterparty
// profiler's interaction count (an unconditional increment a duplicate turn
// would double).
//
// # Why a key rather than an epoch fence
//
// The instinct is to guard these writes the way a pending sandbox run is
// guarded, with a fencing predicate. That works for mutating an existing row
// and fails for an insert, which has no row to attach the condition to — and
// even done atomically it loses data in the case where nothing went wrong. A
// node that completes a turn, acks the delivery and THEN lapses would have
// its episode fenced out: the turn happened, the memory of it is gone.
// Keying on the work keeps that row and still collapses the duplicate:
//
//	scenario                                     fence      work key
//	zombie and owner both complete               one row    one row
//	owner completes, acks, then lapses           ROW LOST   one row
//	ledger fails open, turn legitimately re-runs two rows    one row
//
// # The empty key is the honest default
//
// A turn with no ledgerable trigger — a scheduled fire, a sub-agent, a
// sandbox resume running in a later task than the dispatch that bound the
// key — has no cross-node duplicate to collapse. Those rows are
// unconstrained by design: the store maps an empty key to NULL, and SQL
// treats NULLs as distinct, so only the case the key can actually speak for
// is constrained by it.
//
// This package imports nothing else from the engine, for the same reason the
// topic grammar does not: a producer and a consumer that disagree about an
// identity never raise, they just quietly record two of everything.
package workkey

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
)

// keyChars is how much of the digest is kept. 32 hex chars is 128 bits of a
// SHA-256 — far past any collision concern for the number of turns a company
// will ever run, and short enough to read in a log line.
const keyChars = 32

// Derive returns one stable key for the set of triggers a turn was
// dispatched for.
//
// Sorted and deduplicated before hashing, so the key depends on WHICH events
// the turn covered and not on the order a broker happened to deliver them —
// two nodes handed the same batch in different orders must derive the same
// key. Empty ids are dropped; an empty input yields an empty key, which is
// the honest "nothing to collapse" answer rather than a hash of nothing.
func Derive(eventIDs []string) string {
	cleaned := make([]string, 0, len(eventIDs))
	for _, id := range eventIDs {
		if id = strings.TrimSpace(id); id != "" {
			cleaned = append(cleaned, id)
		}
	}
	if len(cleaned) == 0 {
		return ""
	}
	slices.Sort(cleaned)
	cleaned = slices.Compact(cleaned)

	sum := sha256.Sum256([]byte(strings.Join(cleaned, "\n")))
	return hex.EncodeToString(sum[:])[:keyChars]
}

// ctxKey is the private context key type; unexported so nothing outside this
// package can write the value by another route.
type ctxKey struct{}

// With returns a context carrying the work key.
//
// In Python this was a contextvar, set once around an inbox dispatch and
// read several frames below by writers that have no other reason to know
// about it. Go threads it through context instead — the same ambient reach,
// but visible in every signature that carries it, which is the point of
// adr-401's move away from implicit context.
func With(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, ctxKey{}, key)
}

// From returns the work key bound to this context, or "" when none is —
// which callers must treat as "legitimately unconstrained", not an error.
func From(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	key, _ := ctx.Value(ctxKey{}).(string)
	return key
}
