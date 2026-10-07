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
// would double). Both run in the REFLECTION pass, which is a queue consumer
// on whichever node wins the delivery — routinely not the one that took the
// turn — so each reads the key off the completed-turn event's own payload
// (see learning.Turn.WorkKey). This package derives it; it does not carry it.
//
// It was carried, on the turn's context, by a With/From pair that nothing in
// production ever read: the two writers above are on the far side of a broker
// from the frame that bound it, so the channel could not reach them and two
// package docs asserted it did. A third carrier of an identity that can
// disagree with the two real ones is worse than none, so there is no ambient
// work key.
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
// # It is not the turn id, and the difference is load-bearing
//
// A turn id names ONE RUN. A trigger whose turn fails without acting is naked
// and redelivered, so one work key legitimately produces several runs — which
// is exactly why a write that must happen once per unit of work keys on this
// and not on the run. The two were one value once; ADR-0017 records what that
// cost.
//
// This package imports nothing else from the engine, for the same reason the
// topic grammar does not: a producer and a consumer that disagree about an
// identity never raise, they just quietly record two of everything.
package workkey

import (
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
