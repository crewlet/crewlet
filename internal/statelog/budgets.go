package statelog

import "time"

// The apply loop's bounds. Each is the framework's, not a domain's: what it
// protects is shared, one store, one write lane and the callers waiting on it.
const (
	// ApplyTxRowBudget is how many rows one transaction may write before it
	// ends at the next record boundary.
	//
	// The store has one writer, and learning, the knowledge base and config
	// revisions compete for it, each with a bounded retry budget. One
	// transaction over a whole backlog (a restart's catch-up, a cross-project
	// move re-keying up to a thousand tasks) would hold the writer for all of
	// it and starve them; 4,000 rows is about two seconds at the measured drain
	// floor of 2,000 rows a second. The split is only ever at a record
	// boundary, so a record's rows, operation id and checkpoint still commit
	// together (package doc, "The checkpoint commits with the rows").
	ApplyTxRowBudget = 4_000

	// ApplyTxTimeBudget bounds a transaction by duration, because a row count
	// is only a proxy for one: a record whose rows are unusually expensive
	// overruns the time the row budget implies while staying inside its count,
	// and duration is what a waiter pays and what a conflict window is. 250 ms,
	// as the linger: the longest a caller should wait on a transaction it did
	// not start. It is checked at record boundaries, so an oversized record is
	// still one transaction: the budget ends a batch early, never splits one.
	ApplyTxTimeBudget = 250 * time.Millisecond

	// ApplyLinger is how long a partially filled batch waits for more records
	// before committing; it is what turns single records into batches.
	//
	// It is given up the moment somebody waits for a row the batch already
	// holds (a write for its own record, a linearizable read for the record
	// before its barrier): otherwise the caller pays the whole linger, a
	// hundred times the append, and on an idle company every record is a
	// partial batch. A caller waiting on a barrier with no row left before it
	// pays none: the batch releases it once admitted ([Runner.Reached]).
	ApplyLinger = 250 * time.Millisecond

	// FetchMessages bounds one pull's records and FetchBytes its bytes, sent as
	// one request (`batch`, `max_bytes`) the jetstream Fetcher builds, since the
	// client library cannot. Bytes are the real bound: records run from a
	// barrier's hundred bytes to half a MiB. The count is never applied to what
	// came back: a delivered record holds an ack-pending slot until acked or its
	// 30 s ack window ends, so dropping any wedges the applier, and a [Fetcher]
	// returns all a pull delivered. It is [ApplyTxRowBudget], so a pull commits
	// whole, and MaxAckPending: barriers are reached when decoded but acked at
	// commit, so it caps how far [Runner.Reached] outruns a stalled commit, 53 s
	// of one node's barriers (~75/s) or 18 s of three's against fsync stalls of
	// 2.1–9.3 s; larger would cover only unmeasured stalls. Past it reads wait,
	// refused `behind` at [ReadBudget], never served stale rows.
	FetchMessages = ApplyTxRowBudget
	// FetchBytes is 28 MiB, above a floor: the broker refuses, every time, a
	// pull bounded below the next record, stopping the applier there for ever.
	// The largest record is [queue.MaxPayloadBytes] (8 MiB) plus the subjects
	// and headers the broker counts; 28 MiB holds three and a half, and is at
	// most 28 MiB in memory per domain (56 while two requests deliver at once,
	// the Fetcher's standing pull keeping both).
	// TestAPullCanAlwaysHoldTheLargestRecord holds the floor.
	FetchBytes = 29_360_128

	// ApplyRetryBeat and ApplyRetryCeiling pace the retry of a failure that is
	// not a stop: the first retry waits the beat, each later one twice the
	// last, up to the ceiling. The beat is the linger, so a retry inside it
	// looks like an ordinary partial batch closing. The ceiling is five
	// seconds, a broker election's scale and the write path's resolve budget
	// ([DefaultResolveBudget]): a broker silent that long cannot answer, and
	// asking more often only loads it. Against [ApplyRetryBudget] it leaves a
	// fault at least six attempts before it is reported, so one failed call
	// never takes a copy out of service.
	ApplyRetryBeat    = ApplyLinger
	ApplyRetryCeiling = 5 * time.Second

	// FetchWait is how long a pull waits when the stream is idle. It does not
	// bound read latency: the standing pull (internal/queue/jetstream) hands a
	// fetch its records as soon as their burst is complete. It bounds a
	// request lost without an answer: the client gives up at this wait plus the
	// queue's one-second grace and asks again, so a record appended meanwhile
	// waits 1.5 s at most, inside [ReadBudget]'s two; hence a quarter of it.
	// The cost is the idle rate, two requests a second per domain. A delivery
	// the client stopped waiting for is not lost: a consumer's requests share
	// one standing inbox, so it lands in the handle's buffer instead of waiting
	// out the ack window.
	FetchWait = 500 * time.Millisecond
)
