package execstate_test

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/ledger"
)

// budget is a parked state's share of its run's row, as the engine passes it.
const budget = 2 << 20

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
// calls its record dropped, and the turn its own task — while a successful
// call's output is gone, and an argument body is left out with the marker that
// says so beside the identifiers that stay.
//
// Mutation: condense a call's identity or its round's tally away, and a later
// segment's call log under-counts; skip the arguments' tier, and the state is
// refused.
func TestAStatePastItsShareGivesUpItsClosedRoundsPayloads(t *testing.T) {
	t.Parallel()
	state := parked()
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
	state := parked()
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
// resume can do without and no row can carry.
//
// Mutation: cut the task to fit, and a resumed turn re-enters with an ask
// nobody made.
func TestAStateWhoseOwnContextIsPastItsShareIsRefused(t *testing.T) {
	t.Parallel()
	state := parked()
	state.Task = strings.Repeat("a", 3<<20)
	if _, _, err := execstate.EncodeWithin(state, budget); !errors.Is(err, execstate.ErrTooLarge) {
		t.Errorf("EncodeWithin = %v, want ErrTooLarge", err)
	}
}
