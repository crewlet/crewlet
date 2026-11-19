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
// An operation id names WHAT a call asks for, not WHEN: `in_progress`, `done`,
// `in_progress` derived one id for the first and third, so the third was
// answered as the first's retry. An id therefore carries [CallLog.Ordinal], how
// many earlier calls to the same tool asked for something ELSE; a repeat keeps
// its count. KEYED ON THE TOOL, NOT THE OBJECT (a model spells one object as a
// key, then a uuid): coarser only raises counts, and over-counting is the safe
// direction throughout. In memory; a resumed run seeds a fresh log ([Seed]), a
// re-run starts empty. Every method is safe on a nil log (zeros).
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

// Seed records calls the run made before this log existed, each by its identity
// where its record kept one ([SeededCall]).
//
// BY IDENTITY, NEVER BY WHAT A RECORD KEPT: records are bounded in bytes and
// count, so arguments may be partial and a long run's middle a tally. A kept
// call carries the digest its run admitted it under, a dropped one is a count
// of it; one whose identity is unkeepable or withheld (sealed approval) matches
// no other ([SeededCall.Unknown], [CallLog.SeedUnknown]). Seeding can only
// over-count. The bridge admits a call ([CallLog.Admit]) only once the record
// holds its entry (internal/api/mcpbridge, "A call is on its run's record
// before it runs"), so no counted call is missing from the seed: listed,
// in-flight, tallied, [Called] own work ([CallLog.Own]), and closed rounds.
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
// write one state in opposite directions (join and leave a room): a per-tool
// count cannot see the other tool's call, so a join after a leave counted 0 like
// the first and was answered as its retry. Every earlier call to tool or any
// family tool that is not this same call counts. The domain owning the tools
// declares the family; too wide only raises counts, too narrow is that hole.
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
// far, plus whatever the worker calls on it. Workers write under the parent's
// unit of work, so each sees what the run did before it; a wave's workers run
// concurrently, so each counts its OWN calls, or a count would depend on which
// sibling finished first.
func (l *CallLog) Fork() *CallLog {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return &CallLog{entries: append([]callEntry(nil), l.entries...), unknown: l.unknown,
		base: len(l.entries)}
}

// Admit records one call when ADMITTED and returns the log it counts from: the
// run's calls before it, as a [CallLog.Fork].
//
// FOR CALLS THAT RUN SIDE BY SIDE. A harness makes a response's calls at once
// and they return in whatever order their tools finish, so counts read against
// returns recorded by [CallLog.Record] would depend on which sibling finished
// first and a re-run would not reproduce them. Recorded at admission, in
// arrival order, every count is a function of that order alone.
//
// ONE LOCK FOR BOTH HALVES, so two admissions cannot both fork before either
// records and count as the run's first of their kind. The fork's own records
// return with [CallLog.Absorb]. A nil log admits nothing and returns nil.
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
// ONE RULE for the log and every id derived beside it. CANONICAL, so a re-run
// reproduces it: encoding/json writes a map's keys sorted at every depth, so
// two decodes of one call encode alike whatever order the model emitted.
//
// A FRESH VALUE WHERE THE ARGUMENTS CANNOT BE ENCODED (a decoded JSON object
// never is): a fresh digest makes a retry write twice, visible and bounded,
// where a constant one would merge two different calls and drop the second
// with a success.
func ArgsDigest(args map[string]any) string {
	raw, err := json.Marshal(args)
	if err != nil {
		return uuid.NewString()
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
