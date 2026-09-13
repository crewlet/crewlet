package statelog

import "time"

// The apply loop's bounds. Every one is the FRAMEWORK's rather than a
// domain's, because what they protect is shared: one store, one write lane,
// and the callers waiting on it.
const (
	// ApplyTxRowBudget is how many rows one transaction may write before
	// it ends at the next record boundary.
	//
	// FOUR THOUSAND. The store has one writer and three other subsystems
	// competing for it — learning, the knowledge base, config revisions —
	// each with a bounded retry budget to burn while it waits. A maximal
	// bulk write is about 31 800 rows, roughly sixteen seconds at the
	// measured drain rate, and one transaction that long starves every one
	// of them.
	//
	// AT A RECORD BOUNDARY, never inside one: the rows, the operation id
	// and the checkpoint of any single record still land together, which
	// is the whole commit contract. What the split gives up is only batch
	// size.
	ApplyTxRowBudget = 4_000

	// ApplyTxTimeBudget is the other bound, and it exists because a row
	// count is a PROXY for a duration rather than a duration.
	//
	// Two hundred and fifty milliseconds, the same value the batch linger
	// takes, and for a related reason: it is the longest a caller should
	// wait on a transaction it did not start. The row budget was chosen
	// from a steady-state drain rate; a single record whose rows are
	// unusually expensive blows through the time that rate implies while
	// staying well inside the count. Duration is what a waiter actually
	// pays and what a conflict window actually is.
	//
	// A single oversized record is still ONE transaction: the budget is
	// checked at record boundaries, so it can end a batch early and can
	// never split a commit.
	ApplyTxTimeBudget = 250 * time.Millisecond

	// ApplyLinger is how long a partially filled batch waits for more
	// records before committing.
	//
	// It is what turns a stream of single records into batches, and it is
	// given up the instant somebody is waiting: a linearizable read
	// appends a barrier and then waits for it, so lingering on a batch
	// that already contains that barrier makes the read pay the whole
	// linger — a hundred times the append it is waiting on, and on an idle
	// company EVERY barrier is a partial batch.
	ApplyLinger = 250 * time.Millisecond

	// FetchMessages and FetchBytes bound one pull from the broker.
	//
	// BYTES ARE THE REAL BOUND and the message count is the secondary one:
	// a log whose records vary from a barrier's hundred-odd bytes to a
	// bulk write's megabytes cannot be sized by count, and a count-only
	// bound on a catch-up replay fetches whatever the largest records
	// happen to be. The vendored client fixes a byte-bounded fetch's
	// message count at a million, so the two cannot both be asked of one
	// call — the count is applied by the framework to what comes back.
	//
	// 29.3 MiB is a fetch that fits comfortably inside the apply loop's
	// own working set at the largest record this design admits; 256 is the
	// count at which the per-message overhead stops mattering.
	FetchMessages = 256
	FetchBytes    = 29_360_128

	// FetchWait is how long a pull waits when the stream is idle.
	//
	// # It is a CEILING ON READ LATENCY, not just on polling
	//
	// The vendored client's batch closes when it is FULL or when this
	// expires — one record arriving does not end it — so a fetch of
	// [FetchMessages] on a quiet log costs the whole wait however fast the
	// record got there. Measured on a three-member cluster: the append
	// acknowledges in about 500 microseconds and the fetch that collects
	// it still takes the full wait, every time.
	//
	// That makes this the dominant term in how long a reader waits for a
	// record it is waiting on, and at five seconds it was longer than
	// [ReadBudget] — so every `linearizable` read on an idle company
	// refused `behind`, having appended a barrier the applier would not
	// collect for another three seconds.
	//
	// 500ms is chosen against ReadBudget rather than against polling cost:
	// a reader must be able to wait out one full fetch and still be served
	// inside its budget, which puts the ceiling at a quarter of it. An
	// idle applier now issues two pull requests a second per domain — to a
	// broker in this same process on the default topology, where a pull
	// request is a subject publish and not a network round trip at all.
	//
	// NOT SOLVED BY CANCELLING A PARKED FETCH, which was tried: messages
	// the server has already dispatched toward a pull request it never
	// hears back about are pending-ack for `domainConsumerAckWait`, which
	// is thirty seconds — six times the delay being removed.
	FetchWait = 500 * time.Millisecond
)
