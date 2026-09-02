package turn_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/turn"
)

// fake is a scripted Phases. Each phase reads the round-th entry of its
// script, or the last one when the script runs short, so a test that only
// cares about round 1 does not have to spell out five identical rounds.
type fake struct {
	works    []turn.Work
	surfaces []turn.Surface
	reviews  []turn.Review

	workErr, revErr error

	workRounds, revRounds int
	resumeRounds          int
	resumeErr             error
	notesSeen             []string
	historySeen           [][]ledger.Iteration
	reviewedWork          []turn.Work
}

func at[T any](s []T, round int) T {
	var zero T
	if len(s) == 0 {
		return zero
	}
	if round-1 < len(s) {
		return s[round-1]
	}
	return s[len(s)-1]
}

func (f *fake) Execute(_ context.Context, round int, notes string, h []ledger.Iteration) (turn.Work, turn.Surface, error) {
	f.workRounds++
	f.notesSeen = append(f.notesSeen, notes)
	f.historySeen = append(f.historySeen, h)
	if f.workErr != nil {
		return turn.Work{}, turn.Surface{}, f.workErr
	}
	return at(f.works, round), at(f.surfaces, round), nil
}

func (f *fake) Resume(_ context.Context, h []ledger.Iteration) (turn.Work, turn.Surface, error) {
	f.resumeRounds++
	f.historySeen = append(f.historySeen, h)
	if f.resumeErr != nil {
		return turn.Work{}, turn.Surface{}, f.resumeErr
	}
	// A resumed phase re-enters the FIRST round, so it reads the same slot
	// an ordinary executor pass would have.
	return at(f.works, 1), at(f.surfaces, 1), nil
}

func (f *fake) Review(_ context.Context, round int, w turn.Work, _ []ledger.Iteration) (turn.Review, error) {
	f.revRounds++
	f.reviewedWork = append(f.reviewedWork, w)
	if f.revErr != nil {
		return turn.Review{}, f.revErr
	}
	return at(f.reviews, round), nil
}

func settings() turn.Settings { return turn.Settings{MaxIterations: 5} }

// slackSurface is a plain one-write-tool surface, which most tests here only
// need as a backdrop for the delivery check.
func slackSurface() turn.Surface {
	return turn.Surface{
		Catalogue:  []string{"slack_post", "slack_history", "lookup_colleague"},
		MCPTools:   []string{"slack_post", "slack_history"},
		KnownReads: []string{"slack_history"},
	}
}

// delivered is a work submission that says it delivered and has the call to
// back it up — the ordinary shape, and the backdrop for most tests here.
func delivered(text string) turn.Work {
	return turn.Work{
		Outcome: turn.OutcomeDelivered, Summary: "posted it", Text: text,
		Deliveries: []string{"slack_post"},
		Calls:      []ledger.Call{{Name: "slack_post"}},
	}
}

func TestADeliveredRoundReviewedDoneEndsTheTurn(t *testing.T) {
	t.Parallel()
	f := &fake{
		works:    []turn.Work{delivered("posted")},
		surfaces: []turn.Surface{slackSurface()},
		reviews:  []turn.Review{{Decision: phase.Done}},
	}
	res, err := turn.Run(context.Background(), f, settings(),
		turn.Input{TurnID: "t1", Reply: turn.ReplyTool})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Decision != phase.Done {
		t.Errorf("decision = %s, want done", res.Decision)
	}
	if res.Artifact != "posted" {
		t.Errorf("artifact = %q", res.Artifact)
	}
	if res.Rounds != 1 || f.workRounds != 1 || f.revRounds != 1 {
		t.Errorf("rounds = %d (executor %d, review %d), want one of each",
			res.Rounds, f.workRounds, f.revRounds)
	}
	// A done round ends the turn instead of appending, so the ledger stays
	// empty — and the last round reaches the caller through LastWork.
	if len(res.Iterations) != 0 {
		t.Errorf("a done round appended %d ledger entries", len(res.Iterations))
	}
	if res.LastWork == nil || res.LastWork.Summary != "posted it" {
		t.Errorf("LastWork = %+v, want the round's own submission", res.LastWork)
	}
	if res.LastReview == nil || res.LastReview.Decision != phase.Done {
		t.Errorf("LastReview = %+v", res.LastReview)
	}
}

func TestTheReviewersArtifactWinsOverTheExecutorsText(t *testing.T) {
	t.Parallel()
	f := &fake{
		works:    []turn.Work{delivered("draft")},
		surfaces: []turn.Surface{slackSurface()},
		reviews:  []turn.Review{{Decision: phase.Done, FinalArtifact: "polished"}},
	}
	res, _ := turn.Run(context.Background(), f, settings(),
		turn.Input{TurnID: "t1", Reply: turn.ReplyTool})
	if res.Artifact != "polished" {
		t.Errorf("artifact = %q, want the reviewer's", res.Artifact)
	}
}

// NOBODY ASKED, AND NOTHING HAPPENED. This is what makes triage cheap: a
// broadcast a seat merely observed ends without spending a review call.
func TestNoActionOnAnUnaddressedTurnSkipsWithoutAReview(t *testing.T) {
	t.Parallel()
	f := &fake{
		works: []turn.Work{{
			Outcome: turn.OutcomeNoAction, Summary: "this was addressed to someone else",
		}},
		surfaces: []turn.Surface{slackSurface()},
	}
	res, err := turn.Run(context.Background(), f, settings(),
		turn.Input{TurnID: "t1", Reply: turn.ReplyNone})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Decision != phase.Skipped {
		t.Errorf("decision = %s, want skipped", res.Decision)
	}
	if res.Artifact != "this was addressed to someone else" {
		t.Errorf("artifact = %q, want the executor's own reason", res.Artifact)
	}
	if f.revRounds != 0 {
		t.Errorf("the reviewer ran %d times on a skip", f.revRounds)
	}
}

// SILENCE IS NOT A DECLINE. The requester cannot tell it from a message that
// was lost, and the engine knows one arrived — so the round is sent back
// without spending a review call on it.
func TestNoActionOnAnAwaitedTurnIsCorrectedWithoutAReview(t *testing.T) {
	t.Parallel()
	for _, reply := range []turn.Reply{turn.ReplyTool, turn.ReplyEngine} {
		f := &fake{
			works:    []turn.Work{{Outcome: turn.OutcomeNoAction, Summary: "not for me"}},
			surfaces: []turn.Surface{slackSurface()},
			reviews:  []turn.Review{{Decision: phase.Done}},
		}
		res, err := turn.Run(context.Background(), f,
			turn.Settings{MaxIterations: 2}, turn.Input{TurnID: "t1", Reply: reply})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if f.revRounds != 0 {
			t.Errorf("%s: the reviewer ran on a claim the engine could refuse itself", reply)
		}
		if res.Decision == phase.Skipped {
			t.Errorf("%s: a turn somebody asked for ended as skipped", reply)
		}
		if len(f.notesSeen) < 2 || !strings.Contains(f.notesSeen[1], "somebody is waiting") {
			t.Errorf("%s: the next round was not told why: %q", reply, f.notesSeen)
		}
	}
}

// Something already reached the outside world. Ending the turn as "nobody was
// asking" would file a side effect nobody reviewed and leave the next turn on
// this thread with no record that it happened.
func TestNoActionAfterActingIsCorrected(t *testing.T) {
	t.Parallel()
	f := &fake{
		works: []turn.Work{{
			Outcome: turn.OutcomeNoAction, Summary: "nothing to do",
			Calls: []ledger.Call{{Name: "slack_post"}},
		}},
		surfaces: []turn.Surface{slackSurface()},
	}
	res, _ := turn.Run(context.Background(), f, turn.Settings{MaxIterations: 1},
		turn.Input{TurnID: "t1", Reply: turn.ReplyNone})
	if res.Decision == phase.Skipped {
		t.Error("a turn that had already called an outward tool ended as skipped")
	}
	if len(f.notesSeen) != 1 || f.revRounds != 0 {
		t.Errorf("notes %q / %d reviews", f.notesSeen, f.revRounds)
	}
}

// WRITING ABOUT AN ACTION DOES NOT PERFORM IT. The claim is checked against
// the engine's own record before any model judges it.
func TestADeliveryClaimWithNoCallIsCorrectedWithoutAReview(t *testing.T) {
	t.Parallel()
	f := &fake{
		works: []turn.Work{{
			Outcome: turn.OutcomeDelivered, Summary: "replied",
			Text: "Here is the answer.", Calls: []ledger.Call{{Name: "slack_history"}},
		}},
		surfaces: []turn.Surface{slackSurface()},
	}
	turn.Run(context.Background(), f, turn.Settings{MaxIterations: 2},
		turn.Input{TurnID: "t1", Reply: turn.ReplyTool})
	if f.revRounds != 0 {
		t.Errorf("the reviewer ran %d times on a claim the record refutes", f.revRounds)
	}
	if len(f.notesSeen) < 2 || !strings.Contains(f.notesSeen[1], "no tool") {
		t.Errorf("the next round was not told what was missing: %q", f.notesSeen)
	}
}

// AN A2A TURN'S DELIVERY IS ITS ARTIFACT. The engine answers the asker on the
// channel the ask opened, so demanding a tool call would loop every colleague
// exchange to exhaustion.
func TestAnEngineDeliveredTurnNeedsNoToolCall(t *testing.T) {
	t.Parallel()
	f := &fake{
		works: []turn.Work{{
			Outcome: turn.OutcomeDelivered, Summary: "answered",
			Text: "Yes, ship it.",
		}},
		surfaces: []turn.Surface{slackSurface()},
		reviews:  []turn.Review{{Decision: phase.Done}},
	}
	res, err := turn.Run(context.Background(), f, settings(),
		turn.Input{TurnID: "t1", Reply: turn.ReplyEngine})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Decision != phase.Done {
		t.Errorf("decision = %s, want done", res.Decision)
	}
	if f.revRounds != 1 {
		t.Errorf("the reviewer ran %d times, want 1", f.revRounds)
	}
}

// A RESCUE TAKES NO FAST PATH. The engine wrote the outcome, so there is
// nothing anybody committed to — and every check above turns on a claim.
func TestARescuedRoundIsAlwaysReviewed(t *testing.T) {
	t.Parallel()
	f := &fake{
		works: []turn.Work{{
			Outcome: turn.OutcomeIncomplete, Summary: "ran out of rounds",
			Rescued: true, Text: "half an answer",
		}},
		surfaces: []turn.Surface{slackSurface()},
		reviews:  []turn.Review{{Decision: phase.Failed, Notes: "nothing usable"}},
	}
	res, err := turn.Run(context.Background(), f, settings(),
		turn.Input{TurnID: "t1", Reply: turn.ReplyNone})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if f.revRounds != 1 {
		t.Errorf("a rescued round was judged by the engine instead of the reviewer")
	}
	if res.Decision != phase.Failed {
		t.Errorf("decision = %s, want the reviewer's failed", res.Decision)
	}
	// And the reviewer is told it was rescued, or it grades a commitment
	// nobody made.
	if len(f.reviewedWork) != 1 || !f.reviewedWork[0].Rescued {
		t.Error("the rescue flag did not reach the reviewer")
	}
}

// THE LAST LAYER. The reviewer's model judges the produced TEXT, finds a good
// answer in it, and says done even though nothing put that answer anywhere a
// person can see.
func TestDoneIsOverturnedWhenNothingDelivered(t *testing.T) {
	t.Parallel()
	f := &fake{
		works: []turn.Work{{
			// `blocked` is what gets past the pre-review check with no
			// delivery: a delivery claim would be refused earlier.
			Outcome: turn.OutcomeBlocked, Summary: "could not post",
			Evidence: "the channel 404s", Text: "Here is what I would have said.",
			Calls: []ledger.Call{{Name: "slack_history"}},
		}},
		surfaces: []turn.Surface{slackSurface()},
		reviews:  []turn.Review{{Decision: phase.Done, Notes: "reads fine"}},
	}
	res, _ := turn.Run(context.Background(), f, turn.Settings{MaxIterations: 2},
		turn.Input{TurnID: "t1", Reply: turn.ReplyTool})
	if res.Decision == phase.Done {
		t.Error("a turn that delivered nothing was allowed to finish")
	}
	if len(f.notesSeen) < 2 {
		t.Fatalf("the turn did not loop back: %q", f.notesSeen)
	}
	// The reviewer's own notes survive, and the engine's correction goes
	// LAST because it is the one the next round must act on.
	if !strings.Contains(f.notesSeen[1], "reads fine") ||
		!strings.Contains(f.notesSeen[1], "requester will never see it") {
		t.Errorf("correction = %q", f.notesSeen[1])
	}
	if i := strings.Index(f.notesSeen[1], "reads fine"); i > strings.Index(f.notesSeen[1], "never see it") {
		t.Errorf("the engine's correction did not go last: %q", f.notesSeen[1])
	}
}

// The override fires only where a TOOL was the way to deliver. Nobody waiting
// means a research turn that legitimately ends in prose; waiting on the engine
// means the artifact reaches them either way.
func TestDoneIsNotOverturnedWhenNoToolWasOwed(t *testing.T) {
	t.Parallel()
	for _, reply := range []turn.Reply{turn.ReplyNone, turn.ReplyEngine} {
		f := &fake{
			works: []turn.Work{{
				Outcome: turn.OutcomeDelivered, Summary: "answered in prose",
				Text: "The answer.",
			}},
			surfaces: []turn.Surface{slackSurface()},
			reviews:  []turn.Review{{Decision: phase.Done}},
		}
		res, _ := turn.Run(context.Background(), f, settings(),
			turn.Input{TurnID: "t1", Reply: reply})
		if res.Decision != phase.Done {
			t.Errorf("%s: decision = %s, want done", reply, res.Decision)
		}
	}
}

// A READ IS NOT A DELIVERY, and the annotation is what says so. Counting one
// would let a turn that only looked things up report itself delivered.
func TestAKnownReadIsNotADelivery(t *testing.T) {
	t.Parallel()
	f := &fake{
		works: []turn.Work{{
			Outcome: turn.OutcomeBlocked, Summary: "read only", Evidence: "no write tool",
			Calls: []ledger.Call{{Name: "slack_history"}},
		}},
		surfaces: []turn.Surface{slackSurface()},
		reviews:  []turn.Review{{Decision: phase.Done}},
	}
	res, _ := turn.Run(context.Background(), f, turn.Settings{MaxIterations: 1},
		turn.Input{TurnID: "t1", Reply: turn.ReplyTool})
	if res.Decision == phase.Done {
		t.Error("a turn whose only call was a known read finished as delivered")
	}
}

// FAIL-CLOSED ON ANNOTATIONS. A tool an MCP server never annotated counts as
// a possible delivery; the alternative exempts every tool a server forgot to
// classify.
func TestAnUnannotatedMCPToolCountsAsADelivery(t *testing.T) {
	t.Parallel()
	f := &fake{
		works: []turn.Work{{
			Outcome: turn.OutcomeDelivered, Summary: "filed it",
			Deliveries: []string{"tracker_do_thing"},
			Calls:      []ledger.Call{{Name: "tracker_do_thing"}},
		}},
		surfaces: []turn.Surface{{
			Catalogue: []string{"tracker_do_thing"},
			MCPTools:  []string{"tracker_do_thing"},
		}},
		reviews: []turn.Review{{Decision: phase.Done}},
	}
	res, _ := turn.Run(context.Background(), f, settings(),
		turn.Input{TurnID: "t1", Reply: turn.ReplyTool})
	if res.Decision != phase.Done {
		t.Errorf("decision = %s: an unannotated MCP tool did not count as a delivery", res.Decision)
	}
}

// A FIRST-PARTY BUILTIN NEVER DELIVERS, however much it writes: an agent's own
// diary is not an answer anybody is waiting for.
func TestABuiltinIsNeverADelivery(t *testing.T) {
	t.Parallel()
	f := &fake{
		works: []turn.Work{{
			Outcome: turn.OutcomeBlocked, Summary: "wrote a note", Evidence: "no channel tool",
			Calls: []ledger.Call{{Name: "reflect_and_persist"}},
		}},
		surfaces: []turn.Surface{{
			Catalogue: []string{"reflect_and_persist", "slack_post"},
			MCPTools:  []string{"slack_post"},
		}},
		reviews: []turn.Review{{Decision: phase.Done}},
	}
	res, _ := turn.Run(context.Background(), f, turn.Settings{MaxIterations: 1},
		turn.Input{TurnID: "t1", Reply: turn.ReplyTool})
	if res.Decision == phase.Done {
		t.Error("a builtin call was read as a delivery")
	}
}

// A FAILED CALL DID NOT DELIVER, and counting it would close the check on
// exactly the turn that needs to iterate.
func TestAFailedCallIsNotADelivery(t *testing.T) {
	t.Parallel()
	f := &fake{
		works: []turn.Work{{
			Outcome: turn.OutcomeBlocked, Summary: "post failed", Evidence: "429",
			Calls: []ledger.Call{{Name: "slack_post", Failed: true}},
		}},
		surfaces: []turn.Surface{slackSurface()},
		reviews:  []turn.Review{{Decision: phase.Done}},
	}
	res, _ := turn.Run(context.Background(), f, turn.Settings{MaxIterations: 1},
		turn.Input{TurnID: "t1", Reply: turn.ReplyTool})
	if res.Decision == phase.Done {
		t.Error("a failed post was read as a delivery")
	}
}

func TestTwoIdenticalRoundsAbortAsAStall(t *testing.T) {
	t.Parallel()
	f := &fake{
		works:    []turn.Work{delivered("same")},
		surfaces: []turn.Surface{slackSurface()},
		reviews:  []turn.Review{{Decision: phase.SelfIterate, Notes: "again"}},
	}
	res, _ := turn.Run(context.Background(), f, settings(),
		turn.Input{TurnID: "t1", Reply: turn.ReplyTool})
	if res.Decision != phase.Failed {
		t.Errorf("decision = %s, want failed", res.Decision)
	}
	if res.Breach == nil || res.Breach.Kind != turn.BreachStall {
		t.Errorf("breach = %+v, want a stall", res.Breach)
	}
	if res.Rounds != 2 {
		t.Errorf("rounds = %d, want the stall caught on the second", res.Rounds)
	}
}

func TestProgressIsNotAStall(t *testing.T) {
	t.Parallel()
	f := &fake{
		works: []turn.Work{
			delivered("first"), delivered("second"), delivered("third"),
		},
		surfaces: []turn.Surface{slackSurface()},
		reviews: []turn.Review{
			{Decision: phase.SelfIterate, Notes: "more"},
			{Decision: phase.SelfIterate, Notes: "more"},
			{Decision: phase.Done},
		},
	}
	res, _ := turn.Run(context.Background(), f, settings(),
		turn.Input{TurnID: "t1", Reply: turn.ReplyTool})
	if res.Decision != phase.Done {
		t.Errorf("decision = %s, want done — changing artifacts are not a stall", res.Decision)
	}
	if res.Rounds != 3 {
		t.Errorf("rounds = %d, want 3", res.Rounds)
	}
}

func TestRunningOutOfRoundsIsAFailureThatSaysSo(t *testing.T) {
	t.Parallel()
	f := &fake{
		works: []turn.Work{
			delivered("a"), delivered("b"), delivered("c"),
		},
		surfaces: []turn.Surface{slackSurface()},
		reviews:  []turn.Review{{Decision: phase.SelfIterate, Notes: "again"}},
	}
	res, _ := turn.Run(context.Background(), f, turn.Settings{MaxIterations: 3},
		turn.Input{TurnID: "t1", Reply: turn.ReplyTool})
	if res.Decision != phase.Failed {
		t.Errorf("decision = %s, want failed", res.Decision)
	}
	if res.Breach == nil || res.Breach.Kind != turn.BreachMaxIterations {
		t.Fatalf("breach = %+v, want max_iterations", res.Breach)
	}
	if !strings.Contains(res.Breach.Detail, "3 rounds") {
		t.Errorf("detail = %q, want it to name the cap", res.Breach.Detail)
	}
}

func TestZeroIterationsStillRunsOneRound(t *testing.T) {
	t.Parallel()
	// A cap of zero cannot be what anyone configured, and treating it as
	// unbounded would let a misconfiguration spend a company's whole budget
	// on one trigger.
	f := &fake{
		works:    []turn.Work{delivered("x")},
		surfaces: []turn.Surface{slackSurface()},
		reviews:  []turn.Review{{Decision: phase.Done}},
	}
	res, _ := turn.Run(context.Background(), f, turn.Settings{},
		turn.Input{TurnID: "t1", Reply: turn.ReplyTool})
	if res.Rounds != 1 || res.Decision != phase.Done {
		t.Errorf("rounds = %d, decision = %s", res.Rounds, res.Decision)
	}
}

func TestTheLedgerCarriesWhatTheNextRoundNeeds(t *testing.T) {
	t.Parallel()
	f := &fake{
		works: []turn.Work{
			{
				Outcome: turn.OutcomeDelivered, Summary: "post the summary",
				Text: "posted", Deliveries: []string{"slack_post"},
				Calls: []ledger.Call{{Name: "slack_post"}, {Name: "slack_history"}},
			},
			delivered("second"),
		},
		surfaces: []turn.Surface{slackSurface()},
		reviews: []turn.Review{
			{Decision: phase.SelfIterate, Notes: "wrong link", CompletedWork: "the post landed"},
			{Decision: phase.Done},
		},
	}
	res, _ := turn.Run(context.Background(), f, settings(),
		turn.Input{TurnID: "t1", Reply: turn.ReplyTool})
	if len(res.Iterations) != 1 {
		t.Fatalf("ledger = %d entries, want the one closed round", len(res.Iterations))
	}
	rec := res.Iterations[0]
	if rec.Iteration != 1 || rec.Intent != "post the summary" || rec.Text != "posted" {
		t.Errorf("entry = %+v", rec)
	}
	if len(rec.Calls) != 2 {
		t.Errorf("calls = %+v, want both", rec.Calls)
	}
	if rec.ReviewNotes != "wrong link" || rec.CompletedWork != "the post landed" {
		t.Errorf("reviewer's words lost: %+v", rec)
	}
	// And the next round is handed the ledger, or it re-fires what landed.
	if len(f.historySeen) < 2 || len(f.historySeen[1]) != 1 {
		t.Errorf("the next round saw %v", f.historySeen)
	}
}

// ONLY THE READS ACTUALLY USED. Rendering is a membership test either way, so
// the block reads the same — but the row is persisted across a sandbox
// suspend, and carrying every read-only tool on a large MCP surface makes it
// grow with the catalogue rather than with what the round did.
func TestOnlyTheReadsActuallyUsedAreRecorded(t *testing.T) {
	t.Parallel()
	f := &fake{
		works: []turn.Work{
			{
				Outcome: turn.OutcomeDelivered, Summary: "s", Deliveries: []string{"slack_post"},
				Calls: []ledger.Call{{Name: "slack_post"}, {Name: "slack_history"}},
			},
			delivered("second"),
		},
		surfaces: []turn.Surface{{
			Catalogue:  []string{"slack_post", "slack_history", "jira_get", "gh_get"},
			MCPTools:   []string{"slack_post", "slack_history", "jira_get", "gh_get"},
			KnownReads: []string{"slack_history", "jira_get", "gh_get"},
		}},
		reviews: []turn.Review{{Decision: phase.SelfIterate, Notes: "again"}, {Decision: phase.Done}},
	}
	res, _ := turn.Run(context.Background(), f, settings(),
		turn.Input{TurnID: "t1", Reply: turn.ReplyTool})
	if len(res.Iterations) != 1 {
		t.Fatalf("ledger = %d entries", len(res.Iterations))
	}
	if got := res.Iterations[0].Reads; len(got) != 1 || got[0] != "slack_history" {
		t.Errorf("reads = %v, want only the one that was called", got)
	}
}

// A SUSPEND IS NOT AN ENDING. This round's review has not run, so appending it
// would tell the resumed turn a delivery was judged when nothing judged it.
func TestASuspendHandsTheLedgerOutWithoutClosingTheRound(t *testing.T) {
	t.Parallel()
	prior := []ledger.Iteration{{Iteration: 1, Intent: "earlier"}}
	f := &fake{
		works:    []turn.Work{{Suspended: true, Text: "started a coding run"}},
		surfaces: []turn.Surface{slackSurface()},
	}
	res, err := turn.Run(context.Background(), f, settings(),
		turn.Input{TurnID: "t1", Reply: turn.ReplyTool, History: prior})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Suspended {
		t.Error("the suspend was not reported")
	}
	if res.Decision != phase.SelfIterate {
		t.Errorf("decision = %s, want self_iterate", res.Decision)
	}
	if f.revRounds != 0 {
		t.Error("the reviewer ran on a round that has not finished")
	}
	if len(res.Iterations) != 1 || res.Iterations[0].Intent != "earlier" {
		t.Errorf("iterations = %+v, want only the rounds that actually closed", res.Iterations)
	}
}

func TestAResumedTurnRe_entersRatherThanRestarting(t *testing.T) {
	t.Parallel()
	f := &fake{
		works:    []turn.Work{delivered("finished the run")},
		surfaces: []turn.Surface{slackSurface()},
		reviews:  []turn.Review{{Decision: phase.Done}},
	}
	res, err := turn.Run(context.Background(), f, settings(),
		turn.Input{TurnID: "t1", Reply: turn.ReplyTool, Resume: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if f.resumeRounds != 1 || f.workRounds != 0 {
		t.Errorf("resume %d / execute %d — a resumed turn must not start over",
			f.resumeRounds, f.workRounds)
	}
	if res.Decision != phase.Done {
		t.Errorf("decision = %s", res.Decision)
	}
}

// ONLY THE FIRST ROUND. Everything after it is an ordinary round, or a resumed
// turn that looped would re-enter a conversation it has already finished.
func TestOnlyTheFirstRoundOfAResumedTurnResumes(t *testing.T) {
	t.Parallel()
	f := &fake{
		works:    []turn.Work{delivered("first"), delivered("second")},
		surfaces: []turn.Surface{slackSurface()},
		reviews:  []turn.Review{{Decision: phase.SelfIterate, Notes: "again"}, {Decision: phase.Done}},
	}
	turn.Run(context.Background(), f, settings(),
		turn.Input{TurnID: "t1", Reply: turn.ReplyTool, Resume: true})
	if f.resumeRounds != 1 || f.workRounds != 1 {
		t.Errorf("resume %d / execute %d, want one of each", f.resumeRounds, f.workRounds)
	}
}

func TestAResumedTurnInheritsTheLedgerItLeftBehind(t *testing.T) {
	t.Parallel()
	prior := []ledger.Iteration{{Iteration: 1, Intent: "before the run"}}
	f := &fake{
		works:    []turn.Work{delivered("done now")},
		surfaces: []turn.Surface{slackSurface()},
		reviews:  []turn.Review{{Decision: phase.Done}},
	}
	turn.Run(context.Background(), f, settings(),
		turn.Input{TurnID: "t1", Reply: turn.ReplyTool, Resume: true, History: prior})
	if len(f.historySeen) == 0 || len(f.historySeen[0]) != 1 {
		t.Fatalf("the resumed round saw %v", f.historySeen)
	}
	if f.historySeen[0][0].Intent != "before the run" {
		t.Errorf("the inherited ledger was not the suspended turn's: %+v", f.historySeen[0])
	}
}

func TestAResumedTurnCanSuspendAgain(t *testing.T) {
	t.Parallel()
	f := &fake{
		works:    []turn.Work{{Suspended: true, Text: "second run"}},
		surfaces: []turn.Surface{slackSurface()},
	}
	res, err := turn.Run(context.Background(), f, settings(),
		turn.Input{TurnID: "t1", Reply: turn.ReplyTool, Resume: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Suspended {
		t.Error("a resumed turn that called the sandbox again did not suspend")
	}
}

func TestAFailedResumeIsAnError(t *testing.T) {
	t.Parallel()
	f := &fake{resumeErr: errors.New("no suspended state")}
	if _, err := turn.Run(context.Background(), f, settings(),
		turn.Input{TurnID: "t1", Resume: true}); err == nil {
		t.Error("a broken resume was reported as a turn outcome")
	}
}

func TestTheDelegationCapEndsTheTurnBeforeAnyPhaseRuns(t *testing.T) {
	t.Parallel()
	f := &fake{}
	res, err := turn.Run(context.Background(), f,
		turn.Settings{MaxIterations: 5, DelegationDepthLimit: 2},
		turn.Input{TurnID: "t1", Depth: 2})
	if err != nil {
		t.Fatalf("a breach was reported as an error: %v", err)
	}
	if res.Decision != phase.Failed {
		t.Errorf("decision = %s, want failed", res.Decision)
	}
	if res.Breach == nil || res.Breach.Kind != turn.BreachDepth {
		t.Errorf("breach = %+v, want depth", res.Breach)
	}
	if f.workRounds != 0 {
		t.Error("a phase ran past the depth cap")
	}
}

func TestADepthLimitOfZeroDisablesTheCap(t *testing.T) {
	t.Parallel()
	f := &fake{
		works:    []turn.Work{delivered("x")},
		surfaces: []turn.Surface{slackSurface()},
		reviews:  []turn.Review{{Decision: phase.Done}},
	}
	res, _ := turn.Run(context.Background(), f, settings(),
		turn.Input{TurnID: "t1", Depth: 99, Reply: turn.ReplyTool})
	if res.Decision != phase.Done {
		t.Errorf("decision = %s, want the cap disabled", res.Decision)
	}
}

func TestAReviewerThatGivesUpEndsTheTurnWithoutABreach(t *testing.T) {
	t.Parallel()
	f := &fake{
		works:    []turn.Work{delivered("x")},
		surfaces: []turn.Surface{slackSurface()},
		reviews:  []turn.Review{{Decision: phase.Failed, Notes: "cannot be done"}},
	}
	res, _ := turn.Run(context.Background(), f, settings(),
		turn.Input{TurnID: "t1", Reply: turn.ReplyTool})
	if res.Decision != phase.Failed {
		t.Errorf("decision = %s, want failed", res.Decision)
	}
	if res.Breach != nil {
		t.Errorf("breach = %+v — no guard fired, so none may be reported", res.Breach)
	}
	if res.LastReview == nil || res.LastReview.Notes != "cannot be done" {
		t.Errorf("the reviewer's reason was lost: %+v", res.LastReview)
	}
}

func TestAPhaseThatBrokeIsAnErrorNotAFailedTurn(t *testing.T) {
	t.Parallel()
	// The distinction the loop's doc comment exists to keep: "the model did
	// not finish" and "the process is broken" must not be one condition.
	boom := errors.New("provider unreachable")
	for name, f := range map[string]*fake{
		"executor": {workErr: boom},
		"review": {
			works:    []turn.Work{delivered("x")},
			surfaces: []turn.Surface{slackSurface()},
			revErr:   boom,
		},
	} {
		if _, err := turn.Run(context.Background(), f, settings(),
			turn.Input{TurnID: "t1", Reply: turn.ReplyTool}); !errors.Is(err, boom) {
			t.Errorf("%s: err = %v, want the phase's own", name, err)
		}
	}
}

func TestNoPhasesIsRefused(t *testing.T) {
	t.Parallel()
	if _, err := turn.Run(context.Background(), nil, settings(), turn.Input{}); err == nil {
		t.Error("a nil Phases ran a turn")
	}
}

// THE SURFACE THE PHASE REPORTS is what the check judges, not one assumed in
// advance: activating a tool mid-run changes the catalogue, and judging a real
// delivery against a stale one reads it as no delivery at all.
func TestTheReportedSurfaceIsWhatTheCheckJudges(t *testing.T) {
	t.Parallel()
	f := &fake{
		works: []turn.Work{{
			Outcome: turn.OutcomeDelivered, Summary: "posted",
			Deliveries: []string{"discovered_post"},
			Calls:      []ledger.Call{{Name: "discovered_post"}},
		}},
		// The tool was activated mid-run, so it is on the surface the phase
		// reports and on no list built before it.
		surfaces: []turn.Surface{{
			Catalogue: []string{"discovered_post"},
			MCPTools:  []string{"discovered_post"},
		}},
		reviews: []turn.Review{{Decision: phase.Done}},
	}
	res, _ := turn.Run(context.Background(), f, settings(),
		turn.Input{TurnID: "t1", Reply: turn.ReplyTool})
	if res.Decision != phase.Done {
		t.Errorf("decision = %s: a mid-run activation was not seen", res.Decision)
	}
}

// The scheduled wall-clock cap. It was documented (docs/concepts/scheduling.md),
// had a GuardKind reserved for it, and was enforced NOWHERE — the value rode a
// Payload key nothing read, so a scheduled turn ran until its round cap.
func TestAScheduledTurnStopsAtItsWallClockCap(t *testing.T) {
	t.Parallel()
	now := time.Unix(0, 0)
	f := &fake{
		works:   []turn.Work{{Text: "a"}, {Text: "b"}, {Text: "c"}},
		reviews: []turn.Review{{Decision: phase.SelfIterate}, {Decision: phase.SelfIterate}, {Decision: phase.Done}},
	}
	res, err := turn.Run(context.Background(), f, turn.Settings{
		MaxIterations: 5,
		MaxWallClock:  90 * time.Second,
		// One minute passes per read; Run reads once per round boundary.
		Now: func() time.Time { now = now.Add(time.Minute); return now },
	}, turn.Input{TurnID: "t-1"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Breach == nil || res.Breach.Kind != turn.BreachScheduledTimeout {
		t.Fatalf("breach = %+v, want a scheduled_timeout", res.Breach)
	}
	// ROUND ONE ALWAYS RUNS. A cap that refused before any work started
	// would report a turn as timed out having done nothing.
	if res.Rounds < 1 {
		t.Errorf("rounds = %d, want at least the first", res.Rounds)
	}
	// And it stopped BEFORE the round cap, which is the point.
	if res.Rounds >= 5 {
		t.Errorf("rounds = %d — the cap did not stop the loop", res.Rounds)
	}
	if !strings.Contains(res.Breach.Detail, "cap") {
		t.Errorf("the breach does not explain itself: %q", res.Breach.Detail)
	}
}

// Every other trigger carries no cap, and an unbounded turn must not acquire
// one by accident.
func TestAnUncappedTurnRunsItsRounds(t *testing.T) {
	t.Parallel()
	f := &fake{
		works:   []turn.Work{{Text: "a"}},
		reviews: []turn.Review{{Decision: phase.Done}},
	}
	res, err := turn.Run(context.Background(), f, turn.Settings{
		MaxIterations: 3,
		Now:           func() time.Time { return time.Unix(0, 0).Add(24 * time.Hour) },
	}, turn.Input{TurnID: "t-2"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Breach != nil {
		t.Errorf("an uncapped turn breached: %+v", res.Breach)
	}
}
