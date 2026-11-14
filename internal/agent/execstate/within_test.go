package execstate_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/kept"
	"github.com/crewlet/crewlet/internal/plainjson"
)

// budget is a parked state's share of its run's row, as the engine passes it.
const budget = 2 << 20

// worst is a parked state whose own context carries what no park condenses at
// its worst: how its conversation's earlier turns ended (Frozen.Endings) at the
// bound its renderer holds the block to as the state encodes it
// (prompts.EndingsMaxBytes), in the byte that escapes the most — six bytes a
// `<` — which every park of a conversation's turn carries whole.
func worst() execstate.State {
	state := parked()
	room := prompts.EndingsMaxBytes - len(`""`)
	state.Context.Endings = strings.Repeat("<", room/6) + strings.Repeat("a", room%6)
	return state
}

// endingsKept reports whether got carries want's endings byte for byte, and
// that they weigh what their bound allows: a park never condenses the block a
// resumed turn's every brief tells, however far it condenses the rounds.
func endingsKept(t *testing.T, got, want execstate.State) {
	t.Helper()
	encoded, err := json.Marshal(got.Context.Endings)
	if err != nil {
		t.Fatal(err)
	}
	if got.Context.Endings != want.Context.Endings || len(encoded) != prompts.EndingsMaxBytes {
		t.Errorf("the state came back with %d bytes of endings as it encodes them, want the %d it parked with, "+
			"whole", len(encoded), prompts.EndingsMaxBytes)
	}
}

// rounds is a turn's closed rounds at their heaviest a row keeps them: n
// rounds of two hundred calls, each call's arguments a body the row kept
// whole beside the identifiers that say where it went, every other call a
// failure with its error, every call with an output and its identity.
func rounds(n int) []ledger.Iteration {
	out := make([]ledger.Iteration, n)
	for i := range out {
		calls := make([]ledger.Call, 200)
		for j := range calls {
			calls[j] = ledger.Call{Name: "post_message",
				Args:   map[string]any{"channel": fmt.Sprintf("C%03d", j), "text": strings.Repeat("<p>", 2600)},
				Result: strings.Repeat("ok ", 1300), Failed: j%2 == 1, Digest: fmt.Sprintf("%064x", i*1000+j)}
		}
		out[i] = ledger.Iteration{Iteration: i + 1, Intent: fmt.Sprintf("round %d", i+1), Calls: calls,
			Text:   strings.Repeat("the draft. ", 3000),
			Elided: []ledger.Elided{{Name: "read_file", Digest: "ab", Calls: 40}}}
	}
	return out
}

// A STATE PAST ITS SHARE GIVES UP ITS CLOSED ROUNDS' PAYLOADS, AND NOTHING ELSE.
// Three closed rounds of two hundred heavy calls weigh several times a parked
// state's share of the row; condensed, the state fits, and every call keeps
// its name, its identity and its outcome, every round its intent and the
// calls its record dropped, and the turn its own context — its task, and how
// its conversation's earlier turns ended, at their bound — while a successful
// call's output is gone, and an argument body is left out with the marker that
// says so beside the identifiers that stay.
//
// Mutation: condense a call's identity or its round's tally away, and a later
// segment's call log under-counts; skip the arguments' tier, and the state is
// refused; cut the turn's endings to make room, and a resumed turn is told its
// conversation's earlier turns short.
func TestAStatePastItsShareGivesUpItsClosedRoundsPayloads(t *testing.T) {
	t.Parallel()
	state := worst()
	state.Iterations = rounds(3)
	raw, _ := execstate.Encode(state)
	if len(raw) <= budget {
		t.Fatalf("the case's state weighs %d, within its share: it needs one past it", len(raw))
	}
	blob, condensed, err := execstate.EncodeWithin(state, budget)
	if err != nil {
		t.Fatalf("EncodeWithin: %v", err)
	}
	if len(blob) > budget || !condensed.Any() {
		t.Fatalf("condensed to %d bytes (%+v), want within %d", len(blob), condensed, budget)
	}
	got, ok, err := execstate.Decode(blob)
	if err != nil || !ok {
		t.Fatalf("the condensed state does not read back: %v", err)
	}
	if got.Task != state.Task || len(got.Iterations) != 3 {
		t.Fatalf("the condensed state lost the turn's own context: task %q, %d rounds", got.Task, len(got.Iterations))
	}
	endingsKept(t, got, state)
	for i, round := range got.Iterations {
		want := state.Iterations[i]
		if round.Intent != want.Intent || len(round.Calls) != len(want.Calls) || len(round.Elided) != 1 ||
			round.Elided[0] != want.Elided[0] {
			t.Fatalf("round %d lost its account: %+v", i+1, round.Elided)
		}
		for j, call := range round.Calls {
			w := want.Calls[j]
			if call.Name != w.Name || call.Digest != w.Digest || call.Failed != w.Failed {
				t.Fatalf("round %d call %d lost what it was: %+v", i+1, j, call)
			}
			if !call.Failed && call.Result != "" {
				t.Errorf("round %d call %d kept its successful output", i+1, j)
			}
			text, _ := call.Args["text"].(string)
			if call.Args["channel"] != w.Args["channel"] || !strings.Contains(text, "not kept on the turn's record") {
				t.Fatalf("round %d call %d keeps arguments %v, want its channel beside a left-out body",
					i+1, j, call.Args)
			}
		}
	}
	// THE CALLER'S STATE IS UNTOUCHED: the park condenses a copy.
	if state.Iterations[0].Calls[0].Result == "" || state.Iterations[0].Calls[0].Args["text"] == nil {
		t.Error("condensing changed the state it was handed")
	}
}

// A STATE WITHIN ITS SHARE IS ENCODED AS IT IS, byte for byte what Encode
// writes, and says it gave up nothing.
//
// Mutation: drop successful outputs whatever the size, and a state that fits
// loses them.
func TestAStateWithinItsShareIsEncodedAsItIs(t *testing.T) {
	t.Parallel()
	state := worst()
	want, err := execstate.Encode(state)
	if err != nil {
		t.Fatal(err)
	}
	got, condensed, err := execstate.EncodeWithin(state, budget)
	if err != nil || condensed.Any() || !bytes.Equal(got, want) {
		t.Errorf("EncodeWithin = %d bytes (%+v, %v), want Encode's %d", len(got), condensed, err, len(want))
	}
}

// A STATE WHOSE OWN CONTEXT IS PAST ITS SHARE IS REFUSED, never condensed into a
// different turn: a task of three MiB is what the turn was asked, which no
// resume can do without and no row can carry — beside endings at their bound,
// which no park condenses either.
//
// Mutation: cut the task to fit, and a resumed turn re-enters with an ask
// nobody made.
func TestAStateWhoseOwnContextIsPastItsShareIsRefused(t *testing.T) {
	t.Parallel()
	state := worst()
	state.Task = strings.Repeat("a", 3<<20)
	_, _, err := execstate.EncodeWithin(state, budget)
	tooLarge, ok := errors.AsType[*execstate.TooLargeError](err)
	if !errors.Is(err, execstate.ErrTooLarge) || !ok || !tooLarge.OwnContext() {
		t.Errorf("EncodeWithin = %v, want ErrTooLarge naming the turn's own context", err)
	}
}

// postRounds is n closed rounds of two hundred calls each already as small as
// a call gets — a channel, an identity, a failure's one-word error — so nothing
// but folding the calls themselves makes the rounds any smaller.
func postRounds(n int) []ledger.Iteration {
	out := make([]ledger.Iteration, n)
	for i := range out {
		calls := make([]ledger.Call, 200)
		for j := range calls {
			calls[j] = ledger.Call{Name: "post_message", Args: map[string]any{"channel": fmt.Sprintf("C%03d", j)},
				Digest: fmt.Sprintf("%064x", i*1000+j)}
			if j%2 == 1 {
				calls[j].Failed, calls[j].Result = true, "no"
			}
		}
		out[i] = ledger.Iteration{Iteration: i + 1, Intent: fmt.Sprintf("round %d", i+1), Calls: calls}
	}
	return out
}

// A STATE WHOSE CLOSED ROUNDS' CALLS OUTGROW ITS SHARE FOLDS THEM INTO THEIR
// TALLIES. A turn's rounds have no ceiling, and many rounds of two hundred calls
// are past a parked state's share once every payload is gone, though the turn's
// own context is small: so each closed round's calls are folded into its tally
// of the calls its record does not list — by identity while that fits, so a
// resumed call log still seeds each exactly, and by tool past that — every
// call and every failure counted, rather than the park refused as a turn whose
// own context is too large, which it is not. AND EVERY OUTCOME NOBODY KNOWS
// counted apart from the failures (ledger.Elided.Unfinished): each round's
// first call, and its last — a retry of an earlier call's very arguments — were
// still in flight as its collection read them, and a tally that counted them as
// failures alone would tell the next round they did nothing.
//
// Mutation: refuse the state once its tallies are folded, and a turn of many
// rounds can no longer park; drop a call rather than tally it, or its failure,
// and a resumed call log or a delivery check counts fewer than the round made;
// fold an unknown outcome as a failure alone, by identity or by tool, and the
// round reads as one whose call did nothing.
func TestAStateWhoseClosedRoundsCallsOutgrowItsShareFoldsThemIntoTheirTallies(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		rounds int
		exact  bool
	}{
		"by identity, where that fits": {rounds: 80, exact: true},
		"by tool, past that":           {rounds: 100},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			state := worst()
			state.Iterations = postRounds(tc.rounds)
			for i := range state.Iterations {
				calls := state.Iterations[i].Calls
				calls[199].Digest = calls[197].Digest
				for _, j := range []int{0, 199} {
					calls[j].Args, calls[j].Result, calls[j].Failed, calls[j].Unfinished = nil,
						"its outcome is unknown", true, true
				}
			}
			if raw, _ := execstate.Encode(state); len(raw) <= budget {
				t.Fatalf("the case's state weighs %d, within its share: it needs one past it", len(raw))
			}
			blob, condensed, err := execstate.EncodeWithin(state, budget)
			if err != nil || len(blob) > budget || condensed.Calls != tc.rounds {
				t.Fatalf("EncodeWithin = %d bytes, %+v, %v; want it within %d, every round's calls folded",
					len(blob), condensed, err, budget)
			}
			got, _, err := execstate.Decode(blob)
			if err != nil {
				t.Fatal(err)
			}
			endingsKept(t, got, state)
			for _, round := range got.Iterations {
				calls, failed, unfinished, entries := 0, 0, 0, 0
				for _, e := range round.Elided {
					if e.Name != "post_message" || (e.Digest != "") != tc.exact {
						t.Fatalf("round %d keeps %+v, want every call under its tool, its digest kept %v",
							round.Iteration, e, tc.exact)
					}
					calls, failed, unfinished = calls+e.Calls, failed+e.Failed, unfinished+e.Unfinished
					entries++
				}
				if len(round.Calls) != 0 || calls != 200 || failed != 101 || unfinished != 2 {
					t.Errorf("round %d lists %d calls and tallies %d (%d failed, %d of unknown outcome), want "+
						"all 200 (101, 2) tallied", round.Iteration, len(round.Calls), calls, failed, unfinished)
				}
				if want := map[bool]int{true: 199, false: 1}[tc.exact]; entries != want {
					t.Errorf("round %d tallies its calls in %d entries, want %d", round.Iteration, entries, want)
				}
			}
			// THE CALLER'S STATE IS UNTOUCHED: the park condenses a copy.
			if len(state.Iterations[0].Calls) != 200 || len(state.Iterations[0].Elided) != 0 {
				t.Error("condensing changed the state it was handed")
			}
		})
	}
}

// A STATE WHOSE CLOSED ROUNDS' REVIEWS OUTGROW ITS SHARE CUTS THEM TO THEIR
// OPENING. A reviewer's prose is a round's last word, and a turn of many rounds
// sent back with long corrections carries more of it than the state has room
// for once every call is tallied: each piece keeps its opening, said to be cut,
// rather than the park being refused.
//
// Mutation: refuse the state once its calls are folded, and a turn sent back
// often with long corrections can no longer park.
func TestAStateWhoseClosedRoundsReviewsOutgrowItsShareCutsThemToTheirOpening(t *testing.T) {
	t.Parallel()
	state := worst()
	state.Iterations = nil
	for i := range 100 {
		state.Iterations = append(state.Iterations, ledger.Iteration{Iteration: i + 1,
			CompletedWork: strings.Repeat("landed. ", 4000), ReviewNotes: strings.Repeat("fix it. ", 4000)})
	}
	blob, condensed, err := execstate.EncodeWithin(state, budget)
	if err != nil || len(blob) > budget || condensed.Reviews != 100 {
		t.Fatalf("EncodeWithin = %d bytes, %+v, %v; want it within %d, every round's review cut",
			len(blob), condensed, err, budget)
	}
	got, _, err := execstate.Decode(blob)
	if err != nil {
		t.Fatal(err)
	}
	endingsKept(t, got, state)
	for _, round := range got.Iterations {
		if !strings.HasPrefix(round.ReviewNotes, "fix it. ") || !strings.Contains(round.ReviewNotes, "not kept") ||
			!strings.HasPrefix(round.CompletedWork, "landed. ") || len(round.ReviewNotes) > 5<<10 {
			t.Fatalf("round %d keeps a review of %d bytes, want its opening with the cut said",
				round.Iteration, len(round.ReviewNotes))
		}
	}
}

// A STATE WHOSE CLOSED ROUNDS' INTENTS OUTGROW ITS SHARE CUTS THEM TO THEIR
// OPENING. A round's intent is its submission's summary, tens of KiB at the
// most, and a turn's rounds have no ceiling: three hundred rounds of long
// summaries are past the share with nothing else in them to give up. Each
// intent keeps its opening, said to be cut — what the round was about, which
// the prior-work ledger prints as its "Set out to:" — rather than the park
// being refused for a turn whose own context fits.
//
// Mutation: refuse the state with its intents whole, and a turn of many rounds
// with long summaries can no longer park; cut the turn's endings instead, and
// a resumed turn is told its conversation's earlier turns short.
func TestAStateWhoseClosedRoundsIntentsOutgrowItsShareCutsThemToTheirOpening(t *testing.T) {
	t.Parallel()
	state := worst()
	state.Iterations = nil
	for i := range 300 {
		state.Iterations = append(state.Iterations, ledger.Iteration{Iteration: i + 1,
			Intent: strings.Repeat("set out to do it all. ", 400)})
	}
	if raw, _ := execstate.Encode(state); len(raw) <= budget {
		t.Fatalf("the case's state weighs %d, within its share: it needs one past it", len(raw))
	}
	blob, condensed, err := execstate.EncodeWithin(state, budget)
	if err != nil || len(blob) > budget || condensed.Intents != 300 {
		t.Fatalf("EncodeWithin = %d bytes, %+v, %v; want it within %d, every round's intent cut",
			len(blob), condensed, err, budget)
	}
	got, _, err := execstate.Decode(blob)
	if err != nil {
		t.Fatal(err)
	}
	endingsKept(t, got, state)
	for _, round := range got.Iterations {
		if !strings.HasPrefix(round.Intent, "set out to do it all. ") ||
			!strings.Contains(round.Intent, "not kept") || len(round.Intent) > 5<<10 {
			t.Fatalf("round %d keeps an intent of %d bytes, want its opening with the cut said",
				round.Iteration, len(round.Intent))
		}
	}
}

// A STATE WHOSE CLOSED ROUNDS ARE PAST ITS SHARE, CONDENSED, SAYS SO. A turn of
// enough rounds is past the share however far its rounds are condensed, though
// its own context is small: every round's intent, text and review cut to the
// last of their openings still weighs some kilobytes, and four hundred of them
// are past it. The refusal names the closed rounds as what does not fit, never
// the turn's own context, which a reader would go looking for in the trigger's
// words.
//
// Mutation: blame the turn's own context whatever is past the share, and a park
// refused for its rounds sends its reader to a trigger that is a line long.
func TestAStateWhoseClosedRoundsArePastItsShareCondensedSaysSo(t *testing.T) {
	t.Parallel()
	state := worst()
	state.Iterations = nil
	for i := range 400 {
		state.Iterations = append(state.Iterations, ledger.Iteration{Iteration: i + 1,
			Intent: strings.Repeat("set out to do it all. ", 400), Text: strings.Repeat("the draft. ", 2000),
			CompletedWork: strings.Repeat("landed. ", 1000), ReviewNotes: strings.Repeat("fix it. ", 1000)})
	}
	_, condensed, err := execstate.EncodeWithin(state, budget)
	if condensed.Intents == 0 || condensed.Texts == 0 || condensed.Reviews == 0 {
		t.Errorf("the refused state was condensed %+v, want its intents, texts and reviews cut first", condensed)
	}
	tooLarge, ok := errors.AsType[*execstate.TooLargeError](err)
	if !ok || tooLarge.OwnContext() || !strings.Contains(err.Error(), "closed rounds") {
		t.Errorf("EncodeWithin = %v, want ErrTooLarge naming the closed rounds", err)
	}
}

// A STATE WHOSE CLOSED ROUNDS' TALLIES OUTGROW ITS SHARE FOLDS THEM BY TOOL. A
// round's tallies — the calls its record dropped and the ones its calls' own
// work made — are bounded on its run's row, a turn's rounds are not, and a turn
// of many long rounds carries more identities than the state has room for, past
// everything else it can give up. Folded by tool, each keeps every call and
// every failure under its tool's name — which a delivery check reads, and a
// resumed call log counts as calls whose arguments are not known, an
// over-count — rather than the park being refused.
//
// Mutation: refuse the state past its payloads, and a turn of many long rounds
// can no longer park at all; fold a tally into one count with no tool, and a
// reply among its calls is no longer a delivery.
func TestAStateWhoseClosedRoundsTalliesOutgrowItsShareFoldsThemByTool(t *testing.T) {
	t.Parallel()
	state := worst()
	tally := func(round int) []ledger.Elided {
		out := make([]ledger.Elided, 0, 500)
		for k := range 500 {
			out = append(out, ledger.Elided{Name: fmt.Sprintf("tool_%d", k%5), Digest: fmt.Sprintf("%064x", round*1000+k),
				Calls: 2, Failed: k % 2})
		}
		return out
	}
	state.Iterations = nil
	for i := range 40 {
		state.Iterations = append(state.Iterations, ledger.Iteration{Iteration: i + 1, Elided: tally(i),
			Absorbed: tally(i + 100)})
	}
	raw, _ := execstate.Encode(state)
	if len(raw) <= budget {
		t.Fatalf("the case's state weighs %d, within its share: it needs one past it", len(raw))
	}
	blob, condensed, err := execstate.EncodeWithin(state, budget)
	if err != nil || len(blob) > budget || condensed.Identities != 40 {
		t.Fatalf("EncodeWithin = %d bytes, %+v, %v; want it within %d, every round's tallies folded",
			len(blob), condensed, err, budget)
	}
	got, _, err := execstate.Decode(blob)
	if err != nil {
		t.Fatal(err)
	}
	endingsKept(t, got, state)
	for _, it := range got.Iterations {
		calls, failed := 0, 0
		for _, e := range it.Elided {
			if e.Name == "" || e.Digest != "" {
				t.Fatalf("round %d keeps %+v, want each tool's calls under its name, no digest", it.Iteration, e)
			}
			calls += e.Calls
			failed += e.Failed
		}
		if calls != 1000 || failed != 250 || len(it.Elided) != 5 {
			t.Errorf("round %d keeps %d calls (%d failed) in %d entries, want 1000 (250) in 5", it.Iteration,
				calls, failed, len(it.Elided))
		}
	}
}

// condensedWithin is state condensed within its share, as a park keeps it, and
// what the park says it gave up.
func condensedWithin(t *testing.T, state execstate.State) (execstate.State, execstate.Condensed) {
	t.Helper()
	if raw, _ := execstate.Encode(state); len(raw) <= budget {
		t.Fatalf("the case's state weighs %d, within its share: it needs one past it", len(raw))
	}
	blob, condensed, err := execstate.EncodeWithin(state, budget)
	if err != nil || len(blob) > budget {
		t.Fatalf("EncodeWithin = %d bytes, %+v, %v; want it within %d", len(blob), condensed, err, budget)
	}
	got, _, err := execstate.Decode(blob)
	if err != nil {
		t.Fatal(err)
	}
	endingsKept(t, got, state)
	return got, condensed
}

// saysParked fails unless got is parked's opening ended by the marker saying
// how much of how much is not kept on the turn's record — both figures of
// parked, the payload as its round was parked.
func saysParked(t *testing.T, what, got, parked string) {
	t.Helper()
	at := strings.LastIndex(got, "…\n(")
	if at < 0 || !strings.HasPrefix(parked, got[:at]) {
		t.Fatalf("%s keeps %q, want an opening of what its round was parked with, ended by its marker", what, got)
	}
	want := "…\n(" + kept.Size(len(parked)-at) + " of " + kept.Size(len(parked)) + " not kept on the turn's record)"
	if marker := got[at:]; marker != want {
		t.Fatalf("%s ends %q, want %q: the sizes of what its round was parked with", what, marker, want)
	}
}

// EVERY CUT A PARK MAKES SAYS WHAT ITS ROUND WAS PARKED WITH, AND IS COUNTED
// ONCE. A payload past one tier is cut again at the next, and the marker each
// cut ends with says how much of how much is not kept on the turn's record: of
// the payload as its round was parked, never of what the tier before kept of
// it — an intent of 8 KiB is never said to weigh the 4 its first cut kept. A
// call's arguments past every tier are the marker of the arguments the call was
// parked with, their size and their digest. And what the park says it gave up
// counts each round, and each call, once, however many tiers cut it and however
// often its tallies were folded.
//
// Mutation: cut a tier from what the tier before it kept — a round's text, its
// intent, its review, or a failed call's error and arguments — and the marker
// states the first cut's size, or its digest, as the whole; add a tier's count
// to the one before it, or count a round's tallies once for each fold, and the
// park says it gave up more than it did.
func TestEveryCutAParkMakesSaysWhatItsRoundWasParkedWith(t *testing.T) {
	t.Parallel()
	t.Run("a round's produced text, cut twice", func(t *testing.T) {
		t.Parallel()
		state := worst()
		state.Iterations = nil
		for i := range 200 {
			state.Iterations = append(state.Iterations, ledger.Iteration{Iteration: i + 1,
				Text: strings.Repeat("the draft. ", 3000)})
		}
		got, condensed := condensedWithin(t, state)
		if condensed.Texts != 200 {
			t.Errorf("the park says it cut %d rounds' texts, want the 200 it cut, each once", condensed.Texts)
		}
		for i, round := range got.Iterations {
			saysParked(t, fmt.Sprintf("round %d's text", i+1), round.Text, state.Iterations[i].Text)
		}
	})
	t.Run("a round's intent, cut twice", func(t *testing.T) {
		t.Parallel()
		state := worst()
		state.Iterations = nil
		for i := range 600 {
			state.Iterations = append(state.Iterations, ledger.Iteration{Iteration: i + 1,
				Intent: strings.Repeat("set out to do it all. ", 400)})
		}
		got, condensed := condensedWithin(t, state)
		if condensed.Intents != 600 {
			t.Errorf("the park says it cut %d rounds' intents, want the 600 it cut, each once", condensed.Intents)
		}
		for i, round := range got.Iterations {
			saysParked(t, fmt.Sprintf("round %d's intent", i+1), round.Intent, state.Iterations[i].Intent)
		}
	})
	t.Run("a round's review, cut twice", func(t *testing.T) {
		t.Parallel()
		state := worst()
		state.Iterations = nil
		for i := range 300 {
			state.Iterations = append(state.Iterations, ledger.Iteration{Iteration: i + 1,
				CompletedWork: strings.Repeat("landed. ", 4000), ReviewNotes: strings.Repeat("fix it. ", 4000)})
		}
		got, condensed := condensedWithin(t, state)
		if condensed.Reviews != 300 {
			t.Errorf("the park says it cut %d rounds' reviews, want the 300 it cut, each once", condensed.Reviews)
		}
		for i, round := range got.Iterations {
			want := state.Iterations[i]
			saysParked(t, fmt.Sprintf("round %d's landed work", i+1), round.CompletedWork, want.CompletedWork)
			saysParked(t, fmt.Sprintf("round %d's correction", i+1), round.ReviewNotes, want.ReviewNotes)
		}
	})
	t.Run("a failed call's error and its arguments, cut twice", func(t *testing.T) {
		t.Parallel()
		state := worst()
		state.Iterations = nil
		for i := range 5 {
			calls := make([]ledger.Call, 200)
			for j := range calls {
				args := map[string]any{}
				for k := range 10 {
					args[fmt.Sprintf("k%d", k)] = strings.Repeat("x", 300)
				}
				calls[j] = ledger.Call{Name: "post_message", Args: args, Failed: true,
					Result: strings.Repeat("refused: ", 1100), Digest: fmt.Sprintf("%064x", i*1000+j)}
			}
			state.Iterations = append(state.Iterations, ledger.Iteration{Iteration: i + 1, Calls: calls})
		}
		got, condensed := condensedWithin(t, state)
		if condensed.LeftOut != 1000 {
			t.Errorf("the park says it cut %d calls, want the 1000 it cut, each once", condensed.LeftOut)
		}
		for i, round := range got.Iterations {
			for j, call := range round.Calls {
				was := state.Iterations[i].Calls[j]
				saysParked(t, fmt.Sprintf("round %d call %d's error", i+1, j), call.Result, was.Result)
				raw, err := plainjson.Marshal(was.Args, "")
				if err != nil {
					t.Fatal(err)
				}
				want := map[string]any{kept.NotKeptKey: kept.Record("the turn's record").LeftOut(raw)}
				if !reflect.DeepEqual(call.Args, want) {
					t.Fatalf("round %d call %d keeps arguments %v, want %v: the marker of the arguments it was "+
						"parked with", i+1, j, call.Args, want)
				}
			}
		}
	})
	t.Run("a round's tallies, folded twice", func(t *testing.T) {
		t.Parallel()
		state := worst()
		state.Iterations = postRounds(100)
		for i := range state.Iterations {
			for k := range 20 {
				state.Iterations[i].Elided = append(state.Iterations[i].Elided, ledger.Elided{Name: "read_file",
					Digest: fmt.Sprintf("%064x", 1_000_000+i*100+k), Calls: 1})
			}
		}
		_, condensed := condensedWithin(t, state)
		if condensed.Identities != 100 || condensed.Calls != 100 {
			t.Errorf("the park says it folded %d rounds' tallies and %d rounds' calls, want each of the 100 "+
				"rounds once", condensed.Identities, condensed.Calls)
		}
	})
}

// A ROUND FOLDED INTO ITS TALLY KEEPS NO NAMES ONLY ITS LISTED CALLS ARE DRAWN
// WITH. Which of a round's tools are reads (ledger.Iteration.Reads) is read only
// beside the calls its record lists — to mark a line "(read)" and to cap how
// many reads are shown — so once the round's calls are folded into its tally,
// nothing reads it. Two hundred rounds that each called a hundred read tools
// carry kilobytes of those names a round beside each tool's count in the
// tally, which is past the share; dropped with the calls, the turn parks.
//
// Mutation: keep a folded round's names of its read tools, and the turn is
// refused at its park for names nothing reads.
func TestARoundFoldedIntoItsTallyKeepsNoNamesOnlyItsListedCallsAreDrawnWith(t *testing.T) {
	t.Parallel()
	state := worst()
	state.Iterations = nil
	reads := make([]string, 100)
	for k := range reads {
		reads[k] = fmt.Sprintf("read_%055d", k)
	}
	for i := range 200 {
		calls := make([]ledger.Call, 200)
		for j := range calls {
			calls[j] = ledger.Call{Name: reads[j%len(reads)], Digest: fmt.Sprintf("%064x", i*1000+j)}
		}
		state.Iterations = append(state.Iterations, ledger.Iteration{Iteration: i + 1, Calls: calls, Reads: reads})
	}
	got, condensed := condensedWithin(t, state)
	if condensed.Calls != 200 {
		t.Fatalf("the park folded %d rounds' calls, want all 200", condensed.Calls)
	}
	for _, round := range got.Iterations {
		calls := 0
		for _, e := range round.Elided {
			calls += e.Calls
		}
		if round.Reads != nil || round.Calls != nil || len(round.Elided) != len(reads) || calls != 200 {
			t.Fatalf("round %d keeps %d read names and %d calls listed, and tallies %d calls in %d entries; "+
				"want no names, none listed, and all 200 tallied by tool", round.Iteration, len(round.Reads),
				len(round.Calls), calls, len(round.Elided))
		}
	}
	// THE CALLER'S STATE IS UNTOUCHED: the park condenses a copy.
	if len(state.Iterations[0].Reads) != len(reads) || len(state.Iterations[0].Calls) != 200 {
		t.Error("condensing changed the state it was handed")
	}
}
