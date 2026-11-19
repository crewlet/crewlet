package engine

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// failureWords is n bytes of an error's prose, then leakedToken, then a long
// tail: with n near executorkeep.FailureMaxBytes the token straddles the cut.
func failureWords(n int) string {
	return strings.Repeat("the box said no ", n/16+1)[:n] + leakedToken + " and then " +
		strings.Repeat("more output ", 4<<10)
}

// A TURN'S FAILURE IS FIT WHERE IT IS COMPOSED: every builder of a failed
// ending answers the excerpt the log keeps of the sentence it composes
// ([executorkeep.Excerpt]), within FailureMaxBytes, a credential straddling
// the cut redacted; and an ending owed with a failure nobody fit is recorded
// fitted, as the log keeps it.
//
// Mutation: redact a builder's failure whole instead of fitting it, and the
// failure is carried past its bound.
func TestATurnsFailureIsFitWhereItIsComposed(t *testing.T) {
	t.Parallel()
	reply := turn.Reply{Kind: turn.ReplyTool, Surface: "chatlet"}
	fit := func(s string) string { return executorkeep.Excerpt(s, executorkeep.FailureMaxBytes) }
	for n := executorkeep.FailureMaxBytes - 120; n <= executorkeep.FailureMaxBytes; n += 7 {
		words := failureWords(n)
		builders := map[string]struct{ got, composed string }{
			"abandoned": {abandonedEnding(turn.Result{}, reply, colleagueAnswer{}, "it broke",
				errors.New(words)).Failure,
				fmt.Sprintf("the resumed turn broke and was abandoned (%s): %v", "it broke", words)},
			"launch abandoned": {launchAbandonedEnding(turn.Result{}, reply, colleagueAnswer{},
				errors.New(words)).Failure,
				"the turn's executor launch was abandoned after its run was recorded: " + words},
			"panicked": {panickedEnding(&turn.PanicError{Value: words}).Failure,
				"the resumed turn panicked and was abandoned: " + words},
			"breach": {endingOf(turn.Result{Breach: &turn.Breach{Kind: types.GuardDepthCap, Detail: words}},
				reply, colleagueAnswer{}).Failure,
				"the turn stopped on the engine's " + string(types.GuardDepthCap) + " guard: " + words},
		}
		for name, b := range builders {
			if want := fit(b.composed); b.got != want || len(b.got) > executorkeep.FailureMaxBytes ||
				strings.Contains(b.got, "ghp_") {
				t.Errorf("%s, a token at byte %d: the failure is %d bytes ending %q, want %q", name, n,
					len(b.got), b.got[max(0, len(b.got)-60):], want[max(0, len(want)-60):])
			}
		}
	}
	e, spy := loggingEngine(t)
	rigRuns(t, e, halfwayPlay)
	unfit := failureWords(executorkeep.FailureMaxBytes - 10)
	e.oweExecutorEnd(t.Context(), sandbox.PendingRun{TurnID: "run-9", AgentHandle: "swe", WorkKey: "wk-9",
		WorkSince: time.Now().UTC()}, executorkeep.Ending{Outcome: executorkeep.OutcomeFailed, Failure: unfit})
	if ends := spy.endsOf("run-9"); len(ends) != 1 || ends[0].Ending.Failure != fit(unfit) {
		t.Fatalf("recorded %+v, want the turn's one end with its failure fit", ends)
	}
}

// A REFUSED REQUEST'S MESSAGE IS REDACTED WHOLE, THEN CLIPPED: what ends the
// turn is the diagnostic clip of the whole message's redaction, a credential
// straddling the clip redacted.
//
// Mutation: clip the message before redacting it, and the credential's head
// is carried.
func TestARefusedRequestsMessageIsRedactedWholeThenClipped(t *testing.T) {
	t.Parallel()
	for _, at := range []int{events.MaxDiagnosticBytes - 20, events.MaxDiagnosticBytes - 1} {
		said := failureWords(at)
		got := refusedMessage(sandbox.RunEnding{Error: said, Refused: true})
		if want := events.ClipDiagnostic(redact.Secrets(strings.TrimSpace(said))); got != want {
			t.Errorf("a token at byte %d: the message ends %q, want %q", at, got[max(0, len(got)-120):],
				want[max(0, len(want)-120):])
		}
	}
}
