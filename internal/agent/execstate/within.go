package execstate

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/kept"
	"github.com/crewlet/crewlet/internal/plainjson"
)

// A parked state is held to a share of its run's row (internal/sandbox's
// ExecuteStateBytes); past it the closed rounds give way, the only part that
// grows without bound (`max_iterations` has no ceiling). The rest is the
// turn's fixed context ([Frozen.Endings] among it).
//
// Order, by what no later segment needs whole: call arguments and failed
// errors, produced text, intents, tallies folded by tool, calls folded into
// tallies, review prose, middle rounds into a span, then every round into one.
// Every call keeps its tool and outcome, and a kept round the opening of its
// intent and review, which is all a resumed segment derives an operation id
// or judges a delivery from. Successful calls' outputs are never on a round:
// nothing reads them again.

// ErrTooLarge is a parked state past its budget even with its closed rounds
// condensed ([EncodeWithin]): the turn's own context (its ask, trigger words,
// turn-start blocks and endings) plus one count per tool. No resume can do
// without it and no condensing touches it; the store would refuse every write
// of such a row, which closed rounds alone never cause.
var ErrTooLarge = errors.New("execstate: the parked state is past its share of the run's row")

// TooLargeError is [ErrTooLarge] for one state: what it weighs with every closed
// round folded into one span, the least a park keeps of them; the budget it was
// held to; and what the turn's own context weighs — the state with no closed
// round at all — which is what is past the share.
type TooLargeError struct {
	Bytes, Budget, Context int
}

func (e *TooLargeError) Error() string {
	return fmt.Sprintf("%s: %d bytes with every closed round folded into one count of calls per tool, and its "+
		"share is %d; the turn's own context weighs %d of them, which no condensing touches", ErrTooLarge, e.Bytes,
		e.Budget, e.Context)
}

// Unwrap is [ErrTooLarge], which every caller tests for.
func (e *TooLargeError) Unwrap() error { return ErrTooLarge }

// Condensed is what [EncodeWithin] gave up to fit a state into its budget.
type Condensed struct {
	// LeftOut is how many calls' arguments, or failed calls' errors, were
	// kept in part.
	LeftOut int
	// Texts is how many rounds' produced texts were cut to their opening.
	Texts int
	// Intents is how many rounds' intents were cut to their opening.
	Intents int
	// Identities is how many closed rounds' tallies were folded by tool.
	Identities int
	// Calls is how many closed rounds' calls were folded into their tallies.
	Calls int
	// Reviews is how many rounds' review prose was cut to its opening.
	Reviews int
	// Rounds is how many closed rounds the state kept whole were folded into
	// a span ([ledger.Iteration.Through]); a span an earlier park made, folded
	// into a new one, is not counted again.
	Rounds int
}

// Any reports whether anything was given up.
func (c Condensed) Any() bool {
	return c.LeftOut+c.Texts+c.Intents+c.Identities+c.Calls+c.Reviews+c.Rounds > 0
}

// turnRecord is the record a condensed payload says it is not kept on.
const turnRecord kept.Record = "the turn's record"

// callTiers are the bytes a closed round's call keeps of its arguments, and a
// failed one of its error, tier by tier: two KiB keeps a short body beside its
// identifiers, half a KiB the identifiers of an ordinary call, and an eighth of
// one a call's name for what it was about. A tier is tried only where the one
// before it left the state past its budget.
var callTiers = []int{2 << 10, 512, 128}

// textTiers are the bytes a round's produced text keeps of its opening, tier by
// tier: sixteen KiB is several times what the prior-work ledger fits a round's
// text to, and four is about that.
var textTiers = []int{16 << 10, 4 << 10}

// intentTiers are the bytes a closed round's intent keeps of its opening, tier
// by tier: what the round set out to do in its executor's own words, its
// submission's summary — which the live tool holds to tens of KiB
// (internal/sandbox's MaxSubmissionBytes), and a turn's rounds have no ceiling.
// FOUR KIB is a summary of ordinary length whole, and ONE KIB its first
// paragraph, which still says what the round was about. The tiers of
// [reviewTiers], for prose a reader takes the opening of in the same way.
var intentTiers = []int{4 << 10, 1 << 10}

// reviewTiers are the bytes each of a round's two pieces of review prose keeps
// of its opening, tier by tier: four KiB is a reviewer's whole correction of
// ordinary length, and one KiB its first paragraph — what tells the next round
// what it was sent back for. The last step before a refusal, because the last
// round's correction is what the round the turn goes on to is there to make.
var reviewTiers = []int{4 << 10, 1 << 10}

// EncodeWithin is [Encode], held to budget bytes: a state within it is
// encoded as it is, and one past it is condensed — see the file comment for
// what, and in what order — and encoded within it, or refused with
// [ErrTooLarge]. s itself is never changed.
//
// EVERY TIER CUTS A PAYLOAD AS ITS ROUND WAS PARKED, never what the tier before
// it kept: a cut's marker says how much of how much is not kept, and a cut of a
// cut would state the first cut's size as the whole — an intent of 29 KiB said
// to weigh 3, of which 3 are left out, though 1 is kept. And what it gave up
// counts each round, and each call, once: a tier cuts every payload the tier
// before it cut, and more, so each count is assigned a tier at a time rather
// than added to.
func EncodeWithin(s State, budget int) (json.RawMessage, Condensed, error) {
	blob, err := Encode(s)
	if err != nil || len(blob) <= budget {
		return blob, Condensed{}, err
	}
	var c Condensed
	parked := s.Iterations
	s.Iterations = cloneRounds(parked)
	for _, tier := range callTiers {
		c.LeftOut = condenseCalls(s.Iterations, parked, tier)
		if blob, err = Encode(s); err != nil || len(blob) <= budget {
			return blob, c, err
		}
	}
	for _, tier := range textTiers {
		c.Texts = cutProse(s.Iterations, parked, tier, roundText)
		if blob, err = Encode(s); err != nil || len(blob) <= budget {
			return blob, c, err
		}
	}
	for _, tier := range intentTiers {
		c.Intents = cutProse(s.Iterations, parked, tier, roundIntent)
		if blob, err = Encode(s); err != nil || len(blob) <= budget {
			return blob, c, err
		}
	}
	// THE ROUNDS WHOSE TALLIES WERE FOLDED BY TOOL, which the second fold —
	// once the rounds' calls are folded into their tallies — may fold again:
	// one round folded twice is one round whose tallies were folded.
	folded := make([]bool, len(s.Iterations))
	c.Identities = foldTallies(s.Iterations, folded)
	if blob, err = Encode(s); err != nil || len(blob) <= budget {
		return blob, c, err
	}
	for i := range s.Iterations {
		round := &s.Iterations[i]
		if len(round.Calls) == 0 {
			continue
		}
		for _, call := range round.Calls {
			round.Elided = tallied(round.Elided, call)
		}
		// AND WHICH OF ITS TOOLS ARE READS, which only a call the round
		// lists is drawn with ([ledger.Iteration.Reads]): a tally is drawn
		// by tool without them, and a round that called many tools would
		// otherwise keep every one of their names for nothing.
		round.Calls, round.Reads = nil, nil
		c.Calls++
	}
	if blob, err = Encode(s); err != nil || len(blob) <= budget {
		return blob, c, err
	}
	c.Identities = foldTallies(s.Iterations, folded)
	if blob, err = Encode(s); err != nil || len(blob) <= budget {
		return blob, c, err
	}
	for _, tier := range reviewTiers {
		c.Reviews = cutProse(s.Iterations, parked, tier, roundCompletedWork, roundReviewNotes)
		if blob, err = Encode(s); err != nil || len(blob) <= budget {
			return blob, c, err
		}
	}
	// WHOLE ROUNDS FROM THE MIDDLE, as few as fit: the first round and the
	// newest one are always kept, and the fold that fits is found by halving
	// the count of middle rounds folded, rather than by encoding the state once
	// per round of a turn that may have run hundreds. Folding one more round
	// takes its entry out and adds no more to the span than that entry's own
	// tallies held — the digits of a count aside — so the halving lands on the
	// least fold that fits, or within a round of it.
	condensed := s.Iterations
	if n := len(condensed); n >= 3 {
		var fit json.RawMessage
		folded := 0
		for lo, hi := 1, n-2; lo <= hi; {
			k := (lo + hi) / 2
			s.Iterations = spanned(condensed, 1, 1+k)
			if blob, err = Encode(s); err != nil {
				return nil, c, err
			}
			if len(blob) <= budget {
				fit, folded, hi = blob, k, k-1
			} else {
				lo = k + 1
			}
		}
		if fit != nil {
			c.Rounds = keptWhole(condensed[1 : 1+folded])
			return fit, c, nil
		}
	}
	// AND PAST THAT EVERY ROUND, into one span: the census of the tools the
	// turn called, which is the least a park keeps of its closed rounds.
	if len(condensed) > 0 {
		s.Iterations = spanned(condensed, 0, len(condensed))
		if blob, err = Encode(s); err != nil || len(blob) <= budget {
			c.Rounds = keptWhole(condensed)
			return blob, c, err
		}
	}
	own := s
	own.Iterations = nil
	context, err := Encode(own)
	if err != nil {
		return nil, c, err
	}
	return nil, c, &TooLargeError{Bytes: len(blob), Budget: budget, Context: len(context)}
}

// prose is a piece of a closed round's prose that a park cuts to its opening.
type prose func(*ledger.Iteration) *string

func roundText(r *ledger.Iteration) *string          { return &r.Text }
func roundIntent(r *ledger.Iteration) *string        { return &r.Intent }
func roundCompletedWork(r *ledger.Iteration) *string { return &r.CompletedWork }
func roundReviewNotes(r *ledger.Iteration) *string   { return &r.ReviewNotes }

// cutProse holds the named pieces of every closed round's prose to tier bytes
// of their opening, each cut from the round as it was parked — a piece that
// fits the tier keeps what it holds, whole or cut by a tier before — and
// answers how many rounds it cut a piece of, each round once.
func cutProse(rounds, parked []ledger.Iteration, tier int, pieces ...prose) int {
	n := 0
	for i := range rounds {
		cut := false
		for _, piece := range pieces {
			if text, whole := turnRecord.Text(*piece(&parked[i]), tier); whole > 0 {
				*piece(&rounds[i]) = text
				cut = true
			}
		}
		if cut {
			n++
		}
	}
	return n
}

// foldTallies folds every closed round's two tallies by tool ([byTool]),
// marking each round it changed in folded, and answers how many rounds folded
// marks: a round's tallies folded once and then again, once its calls were
// folded into them, are one round's.
func foldTallies(rounds []ledger.Iteration, folded []bool) int {
	n := 0
	for i := range rounds {
		elided, changed := byTool(rounds[i].Elided)
		absorbed, changedToo := byTool(rounds[i].Absorbed)
		if changed || changedToo {
			rounds[i].Elided, rounds[i].Absorbed = elided, absorbed
			folded[i] = true
		}
		if folded[i] {
			n++
		}
	}
	return n
}

// tallied is tally with call counted in it by its identity — its tool, the
// digest it was admitted under, none where its arguments are withheld or no
// digest was recorded, whether it failed and whether its outcome is unknown —
// as the record of the calls a round does not list keeps one ([ledger.Elided]).
//
// AN UNKNOWN OUTCOME IS COUNTED APART ([ledger.Elided.Unfinished]), never as
// the failure the call is also marked: the next round reads the tally's line,
// which says such a call may have taken effect, and the turn's end reads it as
// a reply that may have gone (internal/engine's unfinishedIn).
func tallied(tally []ledger.Elided, call ledger.Call) []ledger.Elided {
	digest := call.Digest
	if call.Withheld {
		digest = ""
	}
	failed, unfinished := 0, 0
	if call.Failed {
		failed = 1
	}
	if call.Unfinished {
		unfinished = 1
	}
	if i := slices.IndexFunc(tally, func(e ledger.Elided) bool {
		return e.Name == call.Name && e.Digest == digest
	}); i >= 0 {
		tally[i].Calls++
		tally[i].Failed += failed
		tally[i].Unfinished += unfinished
		return tally
	}
	return append(tally, ledger.Elided{Name: call.Name, Digest: digest, Calls: 1, Failed: failed,
		Unfinished: unfinished})
}

// byTool is a tally with each tool's calls folded into one count of calls to it
// whose arguments are not known — the calls, the failures and the unknown
// outcomes kept — and whether that changed it. The entries keep the order of
// each tool's first.
func byTool(tally []ledger.Elided) ([]ledger.Elided, bool) {
	var out []ledger.Elided
	changed := false
	for _, e := range tally {
		changed = changed || e.Digest != ""
		if i := slices.IndexFunc(out, func(o ledger.Elided) bool { return o.Name == e.Name }); i >= 0 {
			out[i].Calls += e.Calls
			out[i].Failed += e.Failed
			out[i].Unfinished += e.Unfinished
			changed = true
			continue
		}
		out = append(out, ledger.Elided{Name: e.Name, Calls: e.Calls, Failed: e.Failed, Unfinished: e.Unfinished})
	}
	return out, changed
}

// condenseCalls holds every closed round's calls to tier bytes of arguments,
// and a failed one's error to tier bytes of its opening — each cut from the
// call as its round was parked — answering how many calls it cut, each once.
func condenseCalls(rounds, parked []ledger.Iteration, tier int) int {
	n := 0
	for i := range rounds {
		for j := range rounds[i].Calls {
			call, was := &rounds[i].Calls[j], parked[i].Calls[j]
			changed := false
			if args, ok := keptArgs(was.Args, tier); ok {
				call.Args, changed = args, true
			}
			if was.Failed {
				if text, whole := turnRecord.Text(was.Result, tier); whole > 0 {
					call.Result, changed = text, true
				}
			}
			if changed {
				n++
			}
		}
	}
	return n
}

// keptArgs is args held to tier bytes as internal/kept keeps a call's
// arguments, and false where they fit already. Through the arguments' JSON
// text and back, every number kept as the digits it was written in, which is
// how a closed round's call holds them.
func keptArgs(args map[string]any, tier int) (map[string]any, bool) {
	if len(args) == 0 {
		return nil, false
	}
	text, err := plainjson.Marshal(args, "")
	if err != nil {
		return nil, false
	}
	shorter, whole := turnRecord.Args(string(text), tier)
	if whole == 0 {
		return nil, false
	}
	dec := json.NewDecoder(strings.NewReader(shorter))
	dec.UseNumber()
	var out map[string]any
	if dec.Decode(&out) != nil {
		return nil, false
	}
	return out, true
}

// cloneRounds copies rounds deeply enough that condensing the copy leaves the
// caller's untouched: each round, its calls and its two tallies, which a call
// folded into one counts in place ([tallied]). A call's arguments are replaced
// whole when they are condensed ([keptArgs]), never changed in place.
func cloneRounds(rounds []ledger.Iteration) []ledger.Iteration {
	out := slices.Clone(rounds)
	for i := range out {
		out[i].Calls = slices.Clone(out[i].Calls)
		out[i].Elided = slices.Clone(out[i].Elided)
		out[i].Absorbed = slices.Clone(out[i].Absorbed)
	}
	return out
}

// spanned is rounds with rounds[from:to] folded into one span standing for them
// ([span]), the rounds on either side as they are.
func spanned(rounds []ledger.Iteration, from, to int) []ledger.Iteration {
	out := make([]ledger.Iteration, 0, len(rounds)-(to-from)+1)
	out = append(out, rounds[:from]...)
	out = append(out, span(rounds[from:to]))
	return append(out, rounds[to:]...)
}

// span is rounds folded into one entry standing for them
// ([ledger.Iteration.Through]): their two tallies merged by tool — every call,
// failure and unknown outcome counted under its tool's name, never a name
// folded away — and the artifact hash of the last of them, the round before
// whatever follows the span. Nothing of their prose.
//
// THE TALLIES ARE EVERY CALL THE ROUNDS MADE: a round reaches a span with its
// calls already in its tally, which the fifth tier folds every round's into
// (see the file comment), and a span an earlier park made lists none.
//
// A span an earlier park made folds like any round: the new span starts at its
// first round and runs through the last round the last entry stands for.
func span(rounds []ledger.Iteration) ledger.Iteration {
	last := rounds[len(rounds)-1]
	out := ledger.Iteration{Iteration: rounds[0].Iteration, Through: max(last.Through, last.Iteration),
		ArtifactHash: last.ArtifactHash}
	for _, round := range rounds {
		out.Elided = append(out.Elided, round.Elided...)
		out.Absorbed = append(out.Absorbed, round.Absorbed...)
	}
	out.Elided, _ = byTool(out.Elided)
	out.Absorbed, _ = byTool(out.Absorbed)
	return out
}

// keptWhole is how many of rounds are rounds rather than spans an earlier park
// made: what [Condensed.Rounds] counts of a fold.
func keptWhole(rounds []ledger.Iteration) int {
	n := 0
	for _, round := range rounds {
		if round.Through == 0 {
			n++
		}
	}
	return n
}
