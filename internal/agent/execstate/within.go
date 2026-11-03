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
// The state rides the run's row in the coordination store, which is one broker
// message and held to a byte budget every field of the row has a share of
// (internal/sandbox's ExecuteStateBytes, rowbytes.go). What grows in a state is
// its CLOSED ROUNDS: each round's intent, text and review, and every call its
// executor's run made — up to two hundred a round, and a turn's rounds are
// bounded by its company's `max_iterations`, which has no ceiling. Everything
// else in it is the turn's own context, fixed when the turn began.
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
//
// Every call keeps its name, its identity ([ledger.Call.Digest]) and its
// outcome, and every round its intent, its review and the identities of the
// calls its run's record dropped — which is everything a resumed segment
// derives an operation id or judges a delivery from.
//
// PAST THAT THE STATE IS REFUSED ([ErrTooLarge]). What is left is the turn's own
// context — what it was asked, its trigger's words, its turn-start blocks — which
// no resume can do without and no row can carry: a park that big would be
// refused by the broker on its write and every write of the row after it.

// ErrTooLarge is a parked state past its budget even with its closed rounds
// condensed ([EncodeWithin]).
var ErrTooLarge = errors.New("execstate: the parked state is past its share of the run's row")

// Condensed is what [EncodeWithin] gave up to fit a state into its budget.
type Condensed struct {
	// Results is how many successful calls' outputs were dropped.
	Results int
	// LeftOut is how many calls' arguments, or failed calls' errors, were
	// kept in part.
	LeftOut int
	// Texts is how many rounds' produced texts were cut to their opening.
	Texts int
}

// Any reports whether anything was given up.
func (c Condensed) Any() bool { return c.Results+c.LeftOut+c.Texts > 0 }

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

// EncodeWithin is [Encode], held to budget bytes: a state within it is
// encoded as it is, and one past it is condensed — see the file comment for
// what, and in what order — and encoded within it, or refused with
// [ErrTooLarge]. s itself is never changed.
func EncodeWithin(s State, budget int) (json.RawMessage, Condensed, error) {
	blob, err := Encode(s)
	if err != nil || len(blob) <= budget {
		return blob, Condensed{}, err
	}
	var c Condensed
	s.Iterations = cloneRounds(s.Iterations)
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
		c.LeftOut += condenseCalls(s.Iterations, tier)
		if blob, err = Encode(s); err != nil || len(blob) <= budget {
			return blob, c, err
		}
	}
	for _, tier := range textTiers {
		for i := range s.Iterations {
			if text, whole := turnRecord.Text(s.Iterations[i].Text, tier); whole > 0 {
				s.Iterations[i].Text = text
				c.Texts++
			}
		}
		if blob, err = Encode(s); err != nil || len(blob) <= budget {
			return blob, c, err
		}
	}
	return nil, c, fmt.Errorf("%w: %d bytes with every closed round condensed, and its share is %d",
		ErrTooLarge, len(blob), budget)
}

// condenseCalls holds every closed round's calls to tier bytes of arguments,
// and a failed one's error to tier bytes of its opening, answering how many it
// changed.
func condenseCalls(rounds []ledger.Iteration, tier int) int {
	n := 0
	for i := range rounds {
		for j := range rounds[i].Calls {
			call := &rounds[i].Calls[j]
			changed := false
			if args, ok := keptArgs(call.Args, tier); ok {
				call.Args, changed = args, true
			}
			if call.Failed {
				if text, whole := turnRecord.Text(call.Result, tier); whole > 0 {
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
// caller's untouched: each round and its calls. A call's arguments are
// replaced whole when they are condensed ([keptArgs]), never changed in place.
func cloneRounds(rounds []ledger.Iteration) []ledger.Iteration {
	out := slices.Clone(rounds)
	for i := range out {
		out[i].Calls = slices.Clone(out[i].Calls)
	}
	return out
}
