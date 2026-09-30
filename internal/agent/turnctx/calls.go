package turnctx

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"

	"github.com/google/uuid"
)

// CallLog is what one RUN of a turn has called so far, kept to answer one
// question: is this call the same write as an earlier one, or a new one?
//
// # Why a run needs it
//
// A write a turn makes derives its operation id from the unit of work, the
// verb, the object and a digest of the call's own arguments, so a re-run —
// which makes the same call again — collapses onto the first copy instead of
// writing twice. That identity names WHAT a call asks for and not WHEN, so a
// run that moved an item to `in_progress`, then to `done`, then back to
// `in_progress` derived one id for the first and the third: the third was
// answered as the first's retry, `applied` at the first's position, and the
// item stayed `done` while the tool reported otherwise.
//
// So an id also carries [CallLog.Ordinal]: how many earlier calls to the same
// tool in this run asked for something ELSE. The first and third calls above
// are 0 and 1 and are two operations. A call repeated with nothing different
// in between — an executor that asks twice, a retry after an `unknown` — keeps
// its count and stays one operation, and a re-run that makes the same calls in
// the same order reproduces every count, so it still collapses call for call.
//
// # Keyed on the tool, not the object
//
// Coarser than the operation it disambiguates, deliberately. A tool names its
// object from arguments a model wrote — a key once, a uuid the next time, for
// the same item — so a count per object would miss a differing call made under
// another spelling and hand the repeat its first copy's id. Counting every
// call to the tool can only raise a count, and a raised count is a fresh
// operation: the cost is that an identical repeat separated by a call to the
// same tool about something else writes again, which is what was asked.
//
// # What it holds, and for how long
//
// A run's calls and nothing else, in memory, for the run's life. A run that is
// SUSPENDED and resumed — possibly in another process — is the same run, so
// its resume seeds a fresh log with what the run already called ([Seed]); a
// RE-RUN after a crash is a new run and starts empty, which is correct: it
// makes its calls again, and each count comes out as it did the first time.
// Seeding can only over-count (a call that failed before deriving an id is
// recorded anyway), and over-counting is safe for the reason above.
//
// A DELEGATED WORKER forks the log ([Fork]) and its calls are absorbed back
// ([Absorb]) once its wave is done. Workers write under the parent's unit of
// work, so each has to see what the run did before it; and workers in one wave
// run concurrently, so each counts its OWN calls rather than its siblings' —
// otherwise a count would depend on which sibling finished first, and a re-run
// would not reproduce it.
//
// # Nil is a log that holds nothing
//
// Every method is safe on a nil log and a nil log's every count is zero:
// a turn built without one — a test, a tool exercised directly — derives each
// id as if its call were the run's first of its kind, which is what every id
// was before this existed. The engine builds every real turn with one.
type CallLog struct {
	mu      sync.Mutex
	entries []callEntry

	// base is how many entries this log inherited when it was forked, so
	// [Absorb] hands back only the calls made on the fork itself.
	base int
}

// callEntry is one recorded call: which tool, and what it asked for.
type callEntry struct {
	tool   string
	digest string
}

// NewCallLog is an empty log, for a run that has called nothing yet.
func NewCallLog() *CallLog { return &CallLog{} }

// Record notes one call this run made, once it has returned.
//
// AFTER THE CALL, because a count is about EARLIER calls: the call being made
// reads its own count before it is recorded.
func (l *CallLog) Record(tool string, args map[string]any) {
	if l == nil {
		return
	}
	digest := ArgsDigest(args)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, callEntry{tool: tool, digest: digest})
}

// SeededCall is one call a run made before this log held it.
type SeededCall struct {
	Tool string
	Args map[string]any
}

// Seed records calls the run made before this log existed — a resumed run's
// calls from before its suspension.
func (l *CallLog) Seed(calls ...SeededCall) {
	for _, call := range calls {
		l.Record(call.Tool, call.Args)
	}
}

// Ordinal is how many calls to tool earlier in this run asked for something
// other than args — the count a derived operation id carries. See [CallLog].
func (l *CallLog) Ordinal(tool string, args map[string]any) int {
	if l == nil {
		return 0
	}
	digest := ArgsDigest(args)
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, entry := range l.entries {
		if entry.tool == tool && entry.digest != digest {
			n++
		}
	}
	return n
}

// Fork is a log for a delegated worker: everything this run has called so
// far, plus whatever the worker calls on it. See [CallLog] for why a worker
// counts on its own fork.
func (l *CallLog) Fork() *CallLog {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return &CallLog{entries: append([]callEntry(nil), l.entries...), base: len(l.entries)}
}

// Absorb records the calls made on a fork, once the worker that made them is
// done, so a later count in this run sees them.
//
// THE CALLER ORDERS THE FORKS — a wave's workers in task order — so the log a
// re-run rebuilds is the same log.
func (l *CallLog) Absorb(fork *CallLog) {
	if l == nil || fork == nil {
		return
	}
	fork.mu.Lock()
	own := append([]callEntry(nil), fork.entries[fork.base:]...)
	fork.mu.Unlock()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, own...)
}

// ArgsDigest is what one tool call asks for, as a digest of its arguments.
//
// ONE RULE for the log and for every id derived beside it, because a count
// read under one digest and recorded under another would compare two
// different things.
//
// CANONICAL, because a re-run must reproduce it: the arguments are JSON a
// model wrote and a decoder read, and encoding/json writes a map's keys in
// sorted order at every depth, so two decodes of the same call encode to the
// same bytes whatever order the model emitted them in.
//
// A FRESH VALUE WHERE THE ARGUMENTS CANNOT BE ENCODED, which a decoded JSON
// object never is — and the direction matters: a fresh digest makes a retry
// write twice, which is visible and bounded, where a constant one would make
// two different calls one operation and drop the second with a success.
func ArgsDigest(args map[string]any) string {
	raw, err := json.Marshal(args)
	if err != nil {
		return uuid.NewString()
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
