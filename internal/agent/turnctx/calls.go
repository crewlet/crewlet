package turnctx

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"sync"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/tools/toolargs"
)

// CallLog is what one RUN of a turn has called so far, kept to answer one
// question: is this call the same write as an earlier one, or a new one?
//
// # Why a run needs it
//
// A write a turn makes derives its operation id from the unit of work, the
// tool, the verb, the object and a digest of the call's own arguments, so a
// re-run — which makes the same call again — collapses onto the first copy
// instead of writing twice. That identity names WHAT a call asks for and not
// WHEN, so a run that moved an item to `in_progress`, then to `done`, then
// back to `in_progress` derived one id for the first and the third: the third
// was answered as the first's retry, `applied` at the first's position, and
// the item stayed `done` while the tool reported otherwise.
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
// # Unless two tools write one state
//
// A count kept per tool cannot see a call to ANOTHER tool, and that is a hole
// wherever two tools write one piece of state in opposite directions —
// joining a room and leaving it are two tools and one membership. A run that
// joined, left and joined again counted the second join 0, exactly as the
// first: the leave between them was another tool's call. So the second join
// derived the first one's id and was answered as its retry — `applied`, at the
// first join's position, with nothing published — and the seat stayed outside
// the room while the tool reported it inside. The `in_progress` → `done` →
// `in_progress` hazard above, back again because one object has two tools.
//
// Such tools are a FAMILY and are counted together ([CallLog.OrdinalAmong]):
// every earlier call in the family that is not this same call raises the
// count. The family is declared by the domain that owns the tools, because
// only it knows which of them write the same thing; a family drawn too wide
// can only raise counts, which is the safe direction for the reason above,
// and one drawn too narrow is this hole.
//
// # What it holds, and for how long
//
// A run's calls and nothing else, in memory, for the run's life. A run that is
// SUSPENDED and resumed — possibly in another process — is the same run, so
// its resume seeds a fresh log with what the run already called ([Seed]); a
// RE-RUN after a crash is a new run and starts empty, which is correct: it
// makes its calls again, and each count comes out as it did the first time.
//
// A SEED IS BY IDENTITY, NEVER BY WHAT A RECORD KEPT. The record a resume
// seeds from — a bridged run's tool log, a parked turn's closed rounds — is
// bounded in bytes, so it may keep a call's arguments in part, and in count, so
// it may keep a long run's middle only as a tally of identities. Each call it
// keeps carries the digest its run admitted it under ([SeededCall.Digest]), and
// each it dropped is a count of that digest, so neither moves a count; one
// whose identity the record could not keep at all, or keeps withheld — a call
// held for a person's approval, whose arguments ride sealed — is seeded as a
// call matching no other ([SeededCall.Unknown], [CallLog.SeedUnknown]). Seeding
// can therefore only over-count (a call that failed before deriving an id is
// recorded anyway, and so is a call whose identity is unknown), and
// over-counting is safe for the reason above.
//
// WHAT A RESUME SEEDS, then, is everything the run's log held that its record
// keeps an identity of: the calls a bridged run's tool log lists, the ones it
// holds in flight — begun and not finished, whose entry the bridge wrote
// before it ran them — the ones it counts without listing (a long run's
// dropped middle), the calls the log absorbed from a bridged call's own work
// (a delegated worker's, [Called], [CallLog.Own]), and the same for every
// closed round. An under-count — a call the run's log counted that the seed
// leaves out — is the hazard this log exists to close, and the bridge keeps
// the two in step: it admits a call to the run's log ([CallLog.Admit]) only once
// the run's record has taken the call's entry, so a call whose entry the record
// did not take is neither run nor counted, and a call the log admitted that
// then could not run is recorded as one that did not, by the identity the log
// counted (internal/api/mcpbridge, "A call is on its run's record before it
// runs"). Every call the run's log counts is on its record by its identity, a
// call whose outcome the record does not hold yet among them.
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
// id as if its call were the run's first of its kind. The engine builds every
// real turn with one.
type CallLog struct {
	mu      sync.Mutex
	entries []callEntry

	// unknown is how many calls the run made whose tool and arguments its
	// record could not keep ([CallLog.SeedUnknown]): each counts against
	// every later call, of every tool, as a call that asked for something
	// else.
	unknown int

	// base is how many entries this log inherited when it was forked, so
	// [Absorb] hands back only the calls made on the fork itself.
	base int
}

// callEntry is one recorded call: which tool, and what it asked for.
type callEntry struct {
	tool   string
	digest string
}

// Called is one call as a run's log counts it: its tool, and the digest of
// what it asked for ([ArgsDigest]). What [CallLog.Own] hands a caller that
// records a call's own work where a later segment's log is seeded from.
type Called struct {
	Tool   string
	Digest string
}

// NewCallLog is an empty log, for a run that has called nothing yet.
func NewCallLog() *CallLog { return &CallLog{} }

// Record notes one call this run made, once it has returned.
//
// AFTER THE CALL, because a count is about EARLIER calls: the call being made
// reads its own count before it is recorded.
//
// THE CANONICAL ARGUMENTS ([toolargs.Args]), the form the tool reads and
// derives its operation id from with [CallLog.Ordinal], so a retry spelt
// "Draft" after "draft" is the same call here as it is to the tool.
func (l *CallLog) Record(tool string, args toolargs.Args) {
	if l == nil {
		return
	}
	l.record(tool, ArgsDigest(args.Map()))
}

// SeededCall is one call a run made before this log held it: a resumed run's
// record of a call made before this segment (the bridge's log of its executor
// run's calls, a closed round).
type SeededCall struct {
	Tool string

	// Digest is the call's identity as the run's own log admitted it
	// ([ArgsDigest] of its canonical arguments, whole), carried on the record
	// beside the call. WHERE SET IT IS WHAT THE CALL IS RECORDED BY, and Args
	// is not read: the record may keep the arguments in part, and a count
	// read against what it kept would take the call for another.
	Digest string

	// Args is the call's canonical arguments ([toolargs.Args]), which a call
	// with no Digest is recorded by: one a build before the digest recorded,
	// whose arguments it kept whole. Canonical rather than as the record
	// holds them, which is the model's spelling, and a count read against the
	// canonical form would take a repeat spelt "Draft" after "draft" for
	// another call.
	Args toolargs.Args

	// Unknown is a call to Tool whose identity the record could not keep —
	// a long run's elided middle past what its tally holds, or a call whose
	// arguments the record withholds — recorded as asking for something no
	// later call asks for: an over-count of Tool, the safe direction
	// ([CallLog]).
	Unknown bool
}

// unkept is the digest a call whose identity is unknown is recorded under: no
// [ArgsDigest] is ever this (a digest is hex, or a fresh uuid where the
// arguments do not encode), so it is a call no later one repeats.
const unkept = "unkept"

// Seed records calls the run made before this log existed — a resumed run's
// calls from before this segment, each by its identity where its record kept
// one ([SeededCall]).
func (l *CallLog) Seed(calls ...SeededCall) {
	if l == nil {
		return
	}
	for _, call := range calls {
		switch {
		case call.Unknown:
			l.record(call.Tool, unkept)
		case call.Digest != "":
			l.record(call.Tool, call.Digest)
		default:
			l.Record(call.Tool, call.Args)
		}
	}
}

// SeedUnknown records n calls the run made whose tool and arguments its record
// could not keep: each counts against every later call, of every tool, as one
// that asked for something else — an over-count of every ordinal, the safe
// direction ([CallLog]), and the one a record that kept neither the name nor
// the identity of what it dropped can still promise.
func (l *CallLog) SeedUnknown(n int) {
	if l == nil || n <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.unknown += n
}

// record appends one call by its tool and digest.
func (l *CallLog) record(tool, digest string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, callEntry{tool: tool, digest: digest})
}

// Ordinal is how many calls to tool earlier in this run asked for something
// other than args — the count a derived operation id carries. See [CallLog].
func (l *CallLog) Ordinal(tool string, args map[string]any) int {
	return l.OrdinalAmong(nil, tool, args)
}

// OrdinalAmong is [CallLog.Ordinal] counted across a FAMILY of tools that
// write one state: how many calls earlier in this run, to tool or to any tool
// in family, were not this same call — another tool of the family, or tool
// with other arguments. See [CallLog] for why a family exists.
//
// tool is counted whether or not family lists it, so a nil family is
// [CallLog.Ordinal]. A call repeated with nothing different in the family
// between keeps its count, exactly as there.
func (l *CallLog) OrdinalAmong(family []string, tool string, args map[string]any) int {
	if l == nil {
		return 0
	}
	digest := ArgsDigest(args)
	l.mu.Lock()
	defer l.mu.Unlock()
	n := l.unknown
	for _, entry := range l.entries {
		if entry.tool == tool && entry.digest == digest {
			// THIS SAME CALL, made before: a repeat, which keeps
			// the count rather than raising it.
			continue
		}
		if entry.tool == tool || slices.Contains(family, entry.tool) {
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
	return &CallLog{entries: append([]callEntry(nil), l.entries...), unknown: l.unknown,
		base: len(l.entries)}
}

// Admit records one call the moment it is ADMITTED and returns the log that
// call counts from: this run's calls before it, as a fork ([CallLog.Fork]).
//
// FOR CALLS THAT RUN SIDE BY SIDE. [CallLog.Record] after each call returns is
// the rule for calls made one after another, where the order they return in is
// the order they were made in. A harness makes a response's calls at once
// and they return in whatever order their tools finish, so a count read
// against a log their returns are recorded into would depend on which sibling
// finished first — and a re-run, making the same calls, would not reproduce
// it. Recorded at admission instead, in the order the calls arrived, every
// count is a function of that order alone.
//
// ONE LOCK FOR BOTH HALVES, so two admissions cannot each take a fork before
// the other's record and both count as the run's first of their kind.
//
// The fork is the call's own: whatever the call records on it is handed back
// with [CallLog.Absorb] once the call has returned. A nil log admits nothing
// and returns nil, which counts every call as the first of its kind.
func (l *CallLog) Admit(tool string, args toolargs.Args) *CallLog {
	if l == nil {
		return nil
	}
	digest := ArgsDigest(args.Map())
	l.mu.Lock()
	defer l.mu.Unlock()
	fork := &CallLog{entries: append([]callEntry(nil), l.entries...), unknown: l.unknown,
		base: len(l.entries)}
	l.entries = append(l.entries, callEntry{tool: tool, digest: digest})
	return fork
}

// Own is the calls made on this log itself, past the ones it was forked with,
// in the order they were recorded — on a fork, what [CallLog.Absorb] hands back
// to the log it was forked from — and nil where there are none, or on a nil
// log.
//
// FOR A CALLER THAT RECORDS A CALL WHERE A LATER SEGMENT IS SEEDED FROM. A call
// admitted on a fork ([CallLog.Admit]) may do work of its own that the run's
// log takes in once it returns — a `delegate`, whose workers each count their
// calls on a fork of its fork, absorbed back wave by wave — and a record that
// keeps the call and not that work leaves a later segment's log short of
// calls the run's own process counted: a join after a worker's leave, taken
// for the first join's retry.
func (l *CallLog) Own() []Called {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.entries) <= l.base {
		return nil
	}
	own := make([]Called, 0, len(l.entries)-l.base)
	for _, entry := range l.entries[l.base:] {
		own = append(own, Called{Tool: entry.tool, Digest: entry.digest})
	}
	return own
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
