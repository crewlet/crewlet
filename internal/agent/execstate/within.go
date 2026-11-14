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

// A parked state is held to a share of its run's row, and what gives way past
// it is the closed rounds' payloads.
//
// # Why a share, and why these give way
//
// The state rides the run's row in the coordination store, held to what a key
// holds and to a byte budget every field of the row has a share of
// (internal/sandbox's ExecuteStateBytes, rowbytes.go). What grows in a state is
// its CLOSED ROUNDS: each round's intent, text and review, and every call its
// executor's run made — up to two hundred a round, and a turn's rounds are
// bounded by its company's `max_iterations`, which has no ceiling. Everything
// else in it is the turn's own context, fixed when the turn began — how its
// conversation's earlier turns ended among it ([Frozen.Endings]), which every
// brief of the turn tells and its renderer holds to 24,000 bytes as this state
// encodes them, escapes included (internal/agent/prompts' EndingsMaxBytes).
//
// So past its share a state gives up, in this order, what no later segment of
// the turn needs whole, and never what it does:
//
//  1. A closed round's SUCCESSFUL calls' outputs. Nothing reads them again: the
//     prior-work ledger shows a failed call's error alone, a delivery check
//     reads names and outcomes, and a seed reads identities.
//  2. The closed rounds' ARGUMENTS, held to a tier of bytes per call — values
//     left out largest first, the leaves before their containers
//     (internal/kept's Args), so a call keeps the identifiers that say what it
//     was about and its digest says what it asked for whole — and their
//     FAILED calls' errors held to the same tier, as their opening.
//  3. The rounds' PRODUCED TEXT, held to its opening: the prior-work ledger
//     fits a round's text to a few thousand characters anyway
//     (ledger.RenderedArtifactLimit), and the opening it is fitted from is
//     what is kept.
//  4. The rounds' INTENTS — what each set out to do, its submission's summary,
//     which the prior-work ledger prints whole as its "Set out to:" — held to
//     their opening: prose, which gives way before anything a resumed call
//     log counts by, and after the text it summarises.
//  5. The DIGESTS in a closed round's tallies — the calls its run's record
//     dropped ([ledger.Iteration.Elided]) and the ones its calls' own work made
//     ([ledger.Iteration.Absorbed]) — each tool's calls folded into one count of
//     calls to it whose arguments are not known: an over-count of that tool for
//     a resumed segment's call log, the safe direction, and nothing a delivery
//     check reads, which is names and outcomes. A round's tallies are bounded on
//     its run's row, but a turn's rounds are not, so many long ones can carry
//     more of them than the state has room for.
//  6. The closed rounds' CALLS THEMSELVES, each folded into its round's tally of
//     the calls its record does not list ([ledger.Iteration.Elided]) — by
//     identity first, its tool, its digest and its outcome, so a resumed call
//     log still seeds it exactly, and past that by tool, as the tallies were —
//     and with them the names of the round's tools that are reads
//     ([ledger.Iteration.Reads]), which only a listed call is drawn with.
//     What a call keeps however far it is condensed — a name, an identity and
//     an outcome — is all a tally holds of it too, and a delivery check reads
//     a tally's calls as the calls they were ([ledger.Called]); what is lost is
//     the prior-work ledger's line for each, which says how many calls of each
//     tool the round made instead ([ledger.FormatElided]).
//  7. The rounds' REVIEW PROSE — what their reviewer said landed and what it
//     sent back to correct — held to its opening, as the produced text is.
//
// Every call keeps its tool, its identity — or, past the last fold, the count
// of calls to its tool it is one of — and its outcome, and every round the
// opening of its intent and of its review and the tools and outcomes of the
// calls its record no longer lists — which is everything a resumed segment
// derives an operation id or judges a delivery from.
//
// PAST THAT THE STATE IS REFUSED ([ErrTooLarge], as a [*TooLargeError] saying
// which of two things is past the share). The turn's own context — what it was
// asked, its trigger's words, its turn-start blocks and its conversation's
// endings — which no resume can do without and no condensing touches; or its
// closed rounds, condensed as far as they go, of which a turn whose
// `max_iterations` has no ceiling can carry more than any share holds. Either
// way a park that big would be refused by the store on its write and every
// write of the row after it.

// ErrTooLarge is a parked state past its budget even with its closed rounds
// condensed ([EncodeWithin]).
var ErrTooLarge = errors.New("execstate: the parked state is past its share of the run's row")

// TooLargeError is [ErrTooLarge] for one state: what it weighs condensed as far
// as it goes, the budget it was held to, and what the turn's own context
// weighs — the state with no closed round at all — which says what is past the
// share.
type TooLargeError struct {
	Bytes, Budget, Context int
}

func (e *TooLargeError) Error() string {
	what := "its closed rounds, condensed as far as they go, take it past its share"
	if e.OwnContext() {
		what = "the turn's own context is past its share by itself"
	}
	return fmt.Sprintf("%s: %d bytes with every closed round condensed, and its share is %d; the turn's own "+
		"context weighs %d — %s", ErrTooLarge, e.Bytes, e.Budget, e.Context, what)
}

// Unwrap is [ErrTooLarge], which every caller tests for.
func (e *TooLargeError) Unwrap() error { return ErrTooLarge }

// OwnContext reports whether the turn's own context alone is past the share —
// what it was asked, its trigger's words, its turn-start blocks — which no
// condensing of its closed rounds could ever make fit.
func (e *TooLargeError) OwnContext() bool { return e.Context > e.Budget }

// Condensed is what [EncodeWithin] gave up to fit a state into its budget.
type Condensed struct {
	// Results is how many successful calls' outputs were dropped.
	Results int
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
}

// Any reports whether anything was given up.
func (c Condensed) Any() bool {
	return c.Results+c.LeftOut+c.Texts+c.Intents+c.Identities+c.Calls+c.Reviews > 0
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
	for i := range s.Iterations {
		for j := range s.Iterations[i].Calls {
			call := &s.Iterations[i].Calls[j]
			if !call.Failed && call.Result != "" {
				call.Result = ""
				c.Results++
			}
		}
	}
	if blob, err = Encode(s); err != nil || len(blob) <= budget {
		return blob, c, err
	}
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
// digest was recorded, and whether it failed — as the record of the calls a
// round does not list keeps one ([ledger.Elided]).
func tallied(tally []ledger.Elided, call ledger.Call) []ledger.Elided {
	digest := call.Digest
	if call.Withheld {
		digest = ""
	}
	failed := 0
	if call.Failed {
		failed = 1
	}
	if i := slices.IndexFunc(tally, func(e ledger.Elided) bool {
		return e.Name == call.Name && e.Digest == digest
	}); i >= 0 {
		tally[i].Calls++
		tally[i].Failed += failed
		return tally
	}
	return append(tally, ledger.Elided{Name: call.Name, Digest: digest, Calls: 1, Failed: failed})
}

// byTool is a tally with each tool's calls folded into one count of calls to it
// whose arguments are not known — the calls and the failures kept — and whether
// that changed it. The entries keep the order of each tool's first.
func byTool(tally []ledger.Elided) ([]ledger.Elided, bool) {
	var out []ledger.Elided
	changed := false
	for _, e := range tally {
		changed = changed || e.Digest != ""
		if i := slices.IndexFunc(out, func(o ledger.Elided) bool { return o.Name == e.Name }); i >= 0 {
			out[i].Calls += e.Calls
			out[i].Failed += e.Failed
			changed = true
			continue
		}
		out = append(out, ledger.Elided{Name: e.Name, Calls: e.Calls, Failed: e.Failed})
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
