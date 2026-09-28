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

	// FetchMessages and FetchBytes bound one pull from the broker, and one
	// pull carries BOTH: the count is the request's `batch` and the bytes
	// its `max_bytes`, and the broker enforces each.
	//
	// BYTES ARE THE REAL BOUND and the message count is the secondary one:
	// a log whose records vary from a barrier's hundred-odd bytes to a
	// bulk write's megabytes cannot be sized by count, and a count-only
	// bound on a catch-up replay fetches whatever the largest records
	// happen to be. The client library cannot send the two together — its
	// byte-bounded fetch fixes the count at a million, and allocated 32 MiB
	// on every idle pull doing it — so the jetstream Fetcher makes the
	// broker's own request itself.
	//
	// # The count is never the caller's to apply afterwards
	//
	// It used to be applied by the loop to what came back: take the first
	// FetchMessages of a byte-bounded batch and drop the rest. The rest had
	// already been DELIVERED — the broker counts every one of them against
	// the consumer's ack-pending cap, which defaults to a thousand, and
	// redelivers them only after the thirty-second ack window. Measured on
	// the embedded broker: the first pull handed over records 1 to 256,
	// every later pull handed over nothing, and at thirty seconds the same
	// 1 to 256 came back. With the loop refusing to commit while records
	// were pending, a backlog of 257 records wedged the applier for the
	// life of the process.
	//
	// So the count is the pull's own, the broker-side consumer's
	// MaxAckPending holds the same number IN FLIGHT across pulls, and a
	// [Fetcher] returns EVERYTHING a pull delivered — to the fetch that
	// asked, or to the next one. A record the broker handed over and the
	// loop never took is a hole for an ack window, on every pull.
	//
	// FOUR THOUSAND, which is [ApplyTxRowBudget]: the loop estimates a row
	// per record when it decides whether another pull could fit, so a
	// consumer that can hand over one transaction's worth of records in
	// flight is one whose every pull the loop can commit whole. Larger
	// buys nothing, because the run closes at the budget; smaller commits
	// more often than the budget asks.
	//
	// 28 MiB OF BYTES, and the floor under it is not a preference: a pull
	// whose byte bound is smaller than the next record is REFUSED by the
	// broker with nothing delivered, every time, so the applier would stop
	// at that record for ever. The largest record a log can hold is
	// [queue.MaxPayloadBytes] (8 MiB) plus the subject, the acknowledgement
	// subject and the headers the broker counts against the bound, and 28
	// MiB is three and a half of them — a pull that can always make
	// progress, and at most 28 MiB in memory per domain between a pull and
	// its commit: 56 in the one case two requests deliver at once, a server
	// still serving one the client had stopped counting (the jetstream
	// Fetcher's standing pull holds what both delivered rather than losing
	// either). TestAPullCanAlwaysHoldTheLargestRecord holds the floor.
	FetchMessages = ApplyTxRowBudget
	FetchBytes    = 29_360_128

	// ApplyRetryBeat and ApplyRetryCeiling pace the retry of a failure
	// that is not a stop: the first retry waits the beat, and each one
	// after that waits twice the last, up to the ceiling.
	//
	// THE BEAT IS THE LINGER, because a retry inside it is
	// indistinguishable from an ordinary partial batch closing. THE
	// CEILING IS FIVE SECONDS, which is a broker election's own scale
	// and the write path's resolve budget: a broker that has not
	// answered in five seconds is one without a quorum rather than a
	// slow one, and asking it more often than that adds load to the
	// thing that is failing. Against [ApplyRetryBudget] the ceiling
	// leaves a fault at least six attempts before it is reported, so a
	// single failed call never sheds a seat.
	ApplyRetryBeat    = ApplyLinger
	ApplyRetryCeiling = 5 * time.Second

	// FetchWait is how long a pull waits when the stream is idle.
	//
	// # It WAS a ceiling on read latency, and is not any more
	//
	// A pull request does not end when one record reaches it, and a fetch
	// that read its request until it ended held a record appended into it
	// for the rest of this wait — so this was the dominant term in how long
	// a reader waited for a record it was waiting on, and at five seconds,
	// longer than [ReadBudget], every `linearizable` read on an idle company
	// refused `behind`. The broker's consumer now hands a fetch its records
	// as soon as their burst is complete (internal/queue/jetstream's
	// standing pull), so an idle fetch returns at the first record however
	// long it was prepared to wait.
	//
	// What the value still bounds is a request LOST WITHOUT AN ANSWER — a
	// connection that dropped it, a server that never ends it. The client
	// stops counting a request at this wait plus the queue's grace (one
	// second) and sends another, so a record appended meanwhile waits that
	// long at most: 1.5 s here, inside ReadBudget's two, which is why the
	// wait stays at a quarter of it rather than growing now that it no
	// longer costs latency. The other side of the trade is the idle rate:
	// two requests a second per domain, a subject publish to a broker in
	// this same process on the default topology.
	//
	// A delivery the client has stopped waiting for is not lost either way:
	// every request of a consumer is answered on one standing inbox, so it
	// lands in the handle's buffer rather than waiting out the thirty-second
	// ack window, which is what cancelling a parked fetch used to cost.
	FetchWait = 500 * time.Millisecond
)
