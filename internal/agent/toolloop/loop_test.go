package toolloop_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/llmtest"
)

// The loop's shape, what ends it, and the correctives it sends when a run ends
// without what the phase needed — each driven through the real session by the
// scripted CLI, so every response is a model request a CLI made and every call
// crossed the socket.

// --- script helpers ---------------------------------------------------------

// says is a response of prose that calls nothing.
func says(text string) llmtest.Response { return llmtest.Response{Text: text} }

// calling is a response that says text and calls the named tools, one after
// another.
func calling(text string, names ...string) llmtest.Response {
	r := llmtest.Response{Text: text}
	for _, n := range names {
		r.Calls = append(r.Calls, llmtest.Call{Name: n})
	}
	return r
}

// runOf is one CLI run playing the responses.
func runOf(rs ...llmtest.Response) llmtest.Run { return llmtest.Run{Responses: rs} }

// script plays one run per argument.
func script(runs ...llmtest.Run) *llmtest.Script { return &llmtest.Script{Runs: runs} }

// writtenSubmission is the measured failure: a model answered its last request
// with one fenced block holding submit_work's ARGUMENTS — the report, typed
// out — rather than the call.
const writtenSubmission = "```json\n" +
	`{"summary":"Commented on the work item","outcome":"delivered",` +
	`"deliveries":["comment_on_work_item"]}` + "\n```"

// finishingCorrectives counts the finishing correctives a conversation holds.
func finishingCorrectives(msgs []llm.Message) int {
	n := 0
	for _, m := range msgs {
		if m.Role == llm.RoleUser && strings.Contains(m.Content, "so this phase has not finished") {
			n++
		}
	}
	return n
}

// emptyCorrectives counts the empty-answer correctives a conversation holds.
func emptyCorrectives(msgs []llm.Message) int {
	n := 0
	for _, m := range msgs {
		if m.Role == llm.RoleUser && strings.Contains(m.Content, "Your last reply was empty") {
			n++
		}
	}
	return n
}

// --- the loop's shape -------------------------------------------------------

func TestALoopEndsWhenTheModelStopsAskingForTools(t *testing.T) {
	t.Parallel()
	prov := script(runOf(calling("thinking", "read"), says("finished")))
	res, err := run(t, toolloop.Config{Provider: prov, Surface: newSurface("read"), MaxRounds: 5})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.RoundsUsed != 2 {
		t.Errorf("rounds = %d, want 2", res.RoundsUsed)
	}
	if res.ExhaustedRounds {
		t.Error("a loop the model ended reports exhausted rounds")
	}
	if len(res.Executions) != 1 || res.Executions[0].Name != "read" {
		t.Errorf("executions = %+v, want one read", res.Executions)
	}
	if n := len(prov.Seen()); n != 1 {
		t.Errorf("%d CLI runs, want the one that answered", n)
	}
}

// EVERY RUN IS A SEAT AT WORK, and says so: a backend that runs a vendor's
// harness keeps the harness's own system prompt for exactly these calls
// (llm.Request.Agent), and a run sent unmarked would lose it on the very calls
// it exists for. And every run is handed the host: a run without it is a text
// function with no tools at all.
func TestEveryRunIsSentAsASeatAtWorkWithTheHost(t *testing.T) {
	t.Parallel()
	prov := script(runOf(says(writtenSubmission)), runOf(calling("", "submit_work")))
	if _, err := run(t, toolloop.Config{Provider: prov, Surface: newSurface("submit_work"),
		TerminateAfter: []string{"submit_work"}}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	seen := prov.Seen()
	if len(seen) != 2 {
		t.Fatalf("%d CLI runs, want the declined one and the corrected one", len(seen))
	}
	for i, req := range seen {
		if !req.Agent {
			t.Errorf("run %d was sent as a text function (Agent false)", i+1)
		}
		if req.Tools == nil {
			t.Errorf("run %d was sent no tool host", i+1)
		}
	}
}

// configured is a provider whose configured identity is name, and whose answer
// names the model that served only when served is set — as a bare backend
// that fills nothing in does not.
type configured struct {
	*llmtest.Script
	name   string
	served string
	key    string
}

func (c *configured) Model() string { return c.name }

func (c *configured) Complete(ctx context.Context, req llm.Request) (*llm.Completion, error) {
	comp, err := c.Script.Complete(ctx, req)
	if comp != nil {
		comp.Model, comp.ProviderKey = c.served, c.key
	}
	return comp, err
}

// The loop bills against the model the ANSWER named, and falls back to the
// provider's configured identity only for a backend that named nothing. Both
// directions matter: a chain that fell through mid-phase reports the member
// that served, and a bare backend still reports something billable.
func TestTheResultBillsAgainstTheModelTheCompletionNamed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, served, want string
	}{
		{"the completion names one", "served-model", "served-model"},
		{"the completion names none", "", "configured-model"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			prov := &configured{Script: script(runOf(says("done"))), name: "configured-model", served: tc.served}
			res, err := run(t, toolloop.Config{Provider: prov, Surface: newSurface(), MaxRounds: 3})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if res.Model != tc.want {
				t.Fatalf("Model = %q, want %q", res.Model, tc.want)
			}
		})
	}
}

// EXHAUSTED MEANS THE MODEL WAS STILL ASKING. The caller may EXTEND the cap: a
// loop that stopped because the model stopped asking used its last round
// legitimately and must not be extended; one still asking was truncated. The
// counterfactual keeps the first half honest against a loop that always
// reports exhausted.
func TestExhaustedRoundsMeansTheModelWasStillAsking(t *testing.T) {
	t.Parallel()
	asking := script(runOf(calling("", "read"), calling("", "read"), calling("", "read")))
	res, err := run(t, toolloop.Config{Provider: asking, Surface: newSurface("read"), MaxRounds: 2})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.ExhaustedRounds {
		t.Error("a loop cut off mid-conversation did not report exhausted rounds")
	}

	stopping := script(runOf(calling("", "read"), says("done")))
	res, err = run(t, toolloop.Config{Provider: stopping, Surface: newSurface("read"), MaxRounds: 2})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExhaustedRounds {
		t.Error("a loop that used its last round legitimately reports exhausted")
	}
}

// --- the loop that does not finish by a call --------------------------------

// PROSE WITHOUT A TERMINATOR IS A CLEAN FINISH, and not an empty answer: the
// counterfactual for every finishing and empty-answer case below. A loop that
// names no call that finishes it takes prose as the answer, re-prompts nothing,
// marks nothing declined and counts nothing empty.
func TestProseWithoutATerminatorIsACleanFinish(t *testing.T) {
	t.Parallel()
	prov := script(runOf(says("the answer")))
	res, err := run(t, toolloop.Config{Provider: prov, Surface: newSurface("read"), MaxRounds: 5})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.RoundsUsed != 1 || len(prov.Seen()) != 1 {
		t.Errorf("rounds = %d over %d runs, want 1 — a prose answer was re-prompted",
			res.RoundsUsed, len(prov.Seen()))
	}
	if len(res.Narration) != 1 || res.Narration[0].Declined {
		t.Errorf("narration = %+v, want one round that is NOT declined", res.Narration)
	}
	if res.EmptyAnswers != 0 {
		t.Errorf("EmptyAnswers = %d, want 0 — prose reached somebody", res.EmptyAnswers)
	}
	if res.Text != "the answer" {
		t.Errorf("text = %q, want the answer", res.Text)
	}
}

// --- the run that reached nobody ---------------------------------------------

// A RUN THAT ENDED WITH NO CALL AND NO PROSE IS NOT A FINISH: a model that
// spent its output on hidden reasoning and stopped. Keyed on CONTENT, not on
// the output billed — the 627 tokens went somewhere nobody reads.
func TestARoundThatSaidNothingAtAllIsCorrectedRatherThanAccepted(t *testing.T) {
	t.Parallel()
	prov := script(
		runOf(llmtest.Response{Usage: llm.Usage{Input: 100, Output: 627}}),
		runOf(says("the answer")),
	)
	res, err := run(t, toolloop.Config{Provider: prov, Surface: newSurface("read"), MaxRounds: 5})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.RoundsUsed != 2 {
		t.Fatalf("rounds = %d, want 2 — the empty round was accepted as a finish", res.RoundsUsed)
	}
	if !strings.Contains(res.Text, "the answer") {
		t.Errorf("text = %q, want the answer the corrective produced", res.Text)
	}
	if emptyCorrectives(res.Messages) != 1 {
		t.Error("no corrective re-prompt was issued for the empty round")
	}
	if res.EmptyAnswers != 1 {
		t.Errorf("EmptyAnswers = %d, want 1 — the count is what survives into the phase record",
			res.EmptyAnswers)
	}
}

// ONE EMPTY-ANSWER CORRECTIVE, not two: an empty answer re-prompted twice is
// the same prompt against the same model.
func TestTheEmptyAnswerCorrectiveIsBoundedToOne(t *testing.T) {
	t.Parallel()
	empty := runOf(says(""))
	prov := script(empty, empty, empty, empty, empty)
	res, err := run(t, toolloop.Config{Provider: prov, Surface: newSurface("read"), MaxRounds: 10})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.RoundsUsed != 2 {
		t.Errorf("rounds = %d, want 2 — one attempt plus one corrective", res.RoundsUsed)
	}
	if res.EmptyAnswers != 2 {
		t.Errorf("EmptyAnswers = %d, want 2 — both rounds reached nobody", res.EmptyAnswers)
	}
}

// The allowance bounds a RUN of empty rounds, not the phase's lifetime: a
// stall, real work, and a second stall gets a second nudge.
func TestAnEmptyRoundAfterRealWorkGetsItsOwnCorrective(t *testing.T) {
	t.Parallel()
	prov := script(
		runOf(calling("filing it", "write"), says("")),
		runOf(calling("trying again", "write"), says("")),
		runOf(says("posted")),
	)
	res, err := run(t, toolloop.Config{Provider: prov, Surface: newSurface("write"), MaxRounds: 10})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.RoundsUsed != 5 {
		t.Fatalf("rounds = %d, want 5 — the second stall broke the loop instead of being corrected",
			res.RoundsUsed)
	}
	if !strings.Contains(res.Text, "posted") {
		t.Errorf("text = %q, want the answer the second corrective produced", res.Text)
	}
	if got := emptyCorrectives(res.Messages); got != 2 {
		t.Errorf("correctives = %d, want 2 — one per run of empty rounds", got)
	}
	if res.EmptyAnswers != 2 {
		t.Errorf("EmptyAnswers = %d, want 2 — the metric stays a phase TOTAL", res.EmptyAnswers)
	}
}

// --- the phase that finishes by a call ---------------------------------------

// Bounded, on the finishing allowance: a model that will not submit is asked
// twice, then the phase ends without its submission — for the caller's rescue
// — rather than burning the round budget. Every round is marked declined, the
// last included: that is what tells a reader the phase ended without it.
func TestTheFinishingCorrectiveIsBounded(t *testing.T) {
	t.Parallel()
	ws := runOf(says(writtenSubmission))
	prov := script(ws, ws, ws, ws)
	res, err := run(t, toolloop.Config{Provider: prov, Surface: newSurface("submit_work"),
		MaxRounds: 10, TerminateAfter: []string{"submit_work"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.RoundsUsed != 3 {
		t.Errorf("rounds = %d, want 3 — one attempt plus two correctives", res.RoundsUsed)
	}
	if got := finishingCorrectives(res.Messages); got != 2 {
		t.Errorf("finishing correctives = %d, want 2", got)
	}
	for _, n := range res.Narration {
		if !n.Declined {
			t.Errorf("round %d not marked declined", n.Round)
		}
	}
	if res.ExhaustedRounds {
		t.Error("a phase that stopped asking for tools reported itself exhausted")
	}
}

// Per RUN of declined rounds: a run that called a tool in between is a model
// that can, so its next run of prose earns its own two correctives.
func TestTheFinishingAllowanceResetsAfterACall(t *testing.T) {
	t.Parallel()
	ws := says(writtenSubmission)
	prov := script(
		runOf(ws), runOf(ws), // the run of declines' whole allowance
		runOf(calling("", "read"), ws),
		runOf(ws), // a new run of declines
		runOf(calling("", "submit_work")),
	)
	surf := newSurface("read", "submit_work")
	res, err := run(t, toolloop.Config{Provider: prov, Surface: surf, MaxRounds: 10,
		TerminateAfter: []string{"submit_work"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := surf.calls(); res.RoundsUsed != 6 || !slices.Equal(got, []string{"read", "submit_work"}) {
		t.Errorf("rounds = %d ran = %v, want the submission reached at round 6", res.RoundsUsed, got)
	}
	if got := finishingCorrectives(res.Messages); got != 4 {
		t.Errorf("finishing correctives = %d, want 4 — two per run of declines", got)
	}
}

// AN EMPTY ROUND IN A SUBMISSION PHASE gets the finishing corrective, not the
// empty-answer one — whose "write it in the response itself" steers a phase
// that finishes by a call straight into its rescue — and is still counted.
func TestAnEmptyRoundInASubmissionPhaseGetsTheFinishingCorrective(t *testing.T) {
	t.Parallel()
	prov := script(
		runOf(llmtest.Response{Usage: llm.Usage{Input: 100, Output: 627}}),
		runOf(calling("", "submit_work")),
	)
	surf := newSurface("submit_work")
	res, err := run(t, toolloop.Config{Provider: prov, Surface: surf, MaxRounds: 5,
		TerminateAfter: []string{"submit_work"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.RoundsUsed != 2 || len(surf.calls()) != 1 {
		t.Fatalf("rounds = %d ran = %v, want the submission at round 2", res.RoundsUsed, surf.calls())
	}
	if res.EmptyAnswers != 1 {
		t.Errorf("EmptyAnswers = %d, want 1 — the round still reached nobody", res.EmptyAnswers)
	}
	if finishingCorrectives(res.Messages) != 1 {
		t.Error("the empty round did not get the finishing corrective")
	}
	if emptyCorrectives(res.Messages) != 0 {
		t.Error("the empty-answer corrective fired in a phase that finishes by a call")
	}
	// Declined is for a round that SAID something; this one said nothing,
	// so it narrates nothing, and the count above is its record.
	if len(res.Narration) != 0 {
		t.Errorf("narration = %+v, want none: the empty round said nothing to decline with", res.Narration)
	}
}

// EMPTY ROUNDS IN A SUBMISSION PHASE SHARE THE FINISHING ALLOWANCE, and the
// empty-answer corrective never tops it up once it is spent.
func TestEmptyRoundsInASubmissionPhaseShareTheFinishingAllowance(t *testing.T) {
	t.Parallel()
	empty := runOf(says(""))
	prov := script(empty, empty, empty, empty, empty)
	res, err := run(t, toolloop.Config{Provider: prov, Surface: newSurface("submit_work"),
		MaxRounds: 10, TerminateAfter: []string{"submit_work"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.RoundsUsed != 3 {
		t.Errorf("rounds = %d, want 3 — one attempt plus two finishing correctives", res.RoundsUsed)
	}
	if got := finishingCorrectives(res.Messages); got != 2 {
		t.Errorf("finishing correctives = %d, want 2", got)
	}
	if emptyCorrectives(res.Messages) != 0 {
		t.Error("the empty-answer corrective fired in a phase that finishes by a call")
	}
	if res.EmptyAnswers != 3 {
		t.Errorf("EmptyAnswers = %d, want 3", res.EmptyAnswers)
	}
}

// A FAILED SUBMISSION IS NOT A FINISH, and neither is the prose a model writes
// after it: a model that read the refusal and gave up in words is asked again,
// and the corrected submission ends the phase.
func TestProseAfterAFailedSubmissionIsCorrected(t *testing.T) {
	t.Parallel()
	surf := newSurface("submit_work")
	surf.failOnce = map[string]bool{"submit_work": true}
	prov := script(
		runOf(calling("", "submit_work"), says("I could not submit, so here is my report: all done.")),
		runOf(calling("", "submit_work")),
	)
	res, err := run(t, toolloop.Config{Provider: prov, Surface: surf, MaxRounds: 5,
		TerminateAfter: []string{"submit_work"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.RoundsUsed != 3 || len(surf.calls()) != 2 {
		t.Fatalf("rounds = %d ran = %v, want the second submission at round 3", res.RoundsUsed, surf.calls())
	}
	if finishingCorrectives(res.Messages) != 1 {
		t.Error("the prose after the refused submission was not corrected")
	}
}

// Several terminators are named as a choice — and only the ones the surface
// offers: one it does not carry cannot be called.
func TestTheFinishingCorrectiveNamesEveryOfferedTerminator(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		offered []string
		want    string
		notWant string
	}{
		{"both offered", []string{"approve", "reject"}, "one of `approve` or `reject`", ""},
		{"one offered", []string{"approve"}, "`approve`", "reject"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			prov := script(runOf(says("done")), runOf(calling("", "approve")))
			if _, err := run(t, toolloop.Config{Provider: prov, Surface: newSurface(tc.offered...),
				MaxRounds: 5, TerminateAfter: []string{"approve", "reject"}}); err != nil {
				t.Fatalf("Run: %v", err)
			}
			seen := prov.Seen()
			if len(seen) != 2 {
				t.Fatalf("%d runs, want the declined one and the corrected one", len(seen))
			}
			corrective := seen[1].Messages[len(seen[1].Messages)-1].Content
			if !strings.Contains(corrective, tc.want) {
				t.Errorf("corrective = %q, want %s named", corrective, tc.want)
			}
			if tc.notWant != "" && strings.Contains(corrective, tc.notWant) {
				t.Errorf("corrective = %q names %s, which the surface does not offer", corrective, tc.notWant)
			}
		})
	}
}

// A CORRECTIVE NOTHING WILL READ IS NOT SENT. On the last round of the budget
// no round follows, so the loop ends there; a FINISHING corrective is handed
// back as Withheld, for a caller that may run the phase past this budget, and
// an empty-answer one is not, since that loop's prose is a finish.
func TestNoCorrectiveIsAppendedOnTheLastRound(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		answer      string
		terminators []string
		declined    bool
		withheld    bool
	}{
		{"finishing", writtenSubmission, []string{"submit_work"}, true, true},
		{"finishing, empty", "", []string{"submit_work"}, false, true},
		{"empty", "", nil, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			prov := script(runOf(says(tc.answer)))
			res, err := run(t, toolloop.Config{Provider: prov, Surface: newSurface("submit_work"),
				MaxRounds: 1, TerminateAfter: tc.terminators})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if got := res.Messages[len(res.Messages)-1]; got.Role == llm.RoleUser {
				t.Errorf("the conversation ends on a user message %q nothing will answer", got.Content)
			}
			if res.RoundsUsed != 1 || len(prov.Seen()) != 1 {
				t.Errorf("rounds = %d over %d runs, want 1", res.RoundsUsed, len(prov.Seen()))
			}
			if tc.declined && (len(res.Narration) != 1 || !res.Narration[0].Declined) {
				t.Errorf("narration = %+v, want the last round marked declined", res.Narration)
			}
			if got := res.Withheld != ""; got != tc.withheld {
				t.Errorf("withheld = %q, want one: %v", res.Withheld, tc.withheld)
			}
			if tc.withheld && !strings.Contains(res.Withheld, "`submit_work`") {
				t.Errorf("withheld = %q, want the finishing corrective", res.Withheld)
			}
			if res.ExhaustedRounds {
				t.Error("a loop that stopped asking for tools reported itself exhausted")
			}
		})
	}
}

// NOTHING IS WITHHELD ONCE THE ALLOWANCE IS SPENT: a run of declines that had
// both its correctives inside the budget has none left to hand back.
func TestNothingIsWithheldOnceTheAllowanceIsSpent(t *testing.T) {
	t.Parallel()
	ws := runOf(says(writtenSubmission))
	res, err := run(t, toolloop.Config{Provider: script(ws, ws, ws), Surface: newSurface("submit_work"),
		MaxRounds: 3, TerminateAfter: []string{"submit_work"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Withheld != "" || res.CorrectivesSpent != 2 {
		t.Errorf("withheld = %q spent = %d, want nothing withheld after both correctives",
			res.Withheld, res.CorrectivesSpent)
	}
}

// A CONTINUATION SHARES THE ALLOWANCE it was seeded with: one that picks up a
// run of declines that already had both its correctives has none to send.
func TestASeededAllowanceIsSharedNotReset(t *testing.T) {
	t.Parallel()
	ws := runOf(says(writtenSubmission))
	res, err := run(t, toolloop.Config{Provider: script(ws, ws), Surface: newSurface("submit_work"),
		MaxRounds: 5, TerminateAfter: []string{"submit_work"}, CorrectivesSpent: 2})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.RoundsUsed != 1 || finishingCorrectives(res.Messages) != 0 {
		t.Errorf("rounds = %d correctives = %d, want the run to end on its first decline",
			res.RoundsUsed, finishingCorrectives(res.Messages))
	}
}

// A TERMINATOR ON THE BUDGET'S LAST ROUND IS A FINISH, not exhaustion: the
// round's last message is a call, which is what an exhausted loop's is.
func TestATerminatorOnTheLastRoundIsNotExhaustion(t *testing.T) {
	t.Parallel()
	prov := script(runOf(calling("", "read"), calling("", "submit_work")))
	res, err := run(t, toolloop.Config{Provider: prov, Surface: newSurface("read", "submit_work"),
		MaxRounds: 2, TerminateAfter: []string{"submit_work"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExhaustedRounds {
		t.Error("a loop its terminator ended reported itself exhausted")
	}
	if res.RoundsUsed != 2 {
		t.Errorf("rounds = %d, want 2", res.RoundsUsed)
	}
}

// THE TEXT IS THE MODEL'S ANSWER TO ITS TASK, not to the correctives. A run of
// rounds that called nothing keeps its first answer and drops the rest; a round
// that called a tool starts a new answer; a round that said nothing does not
// stand in for one.
func TestTheTextKeepsTheAnswerNotItsRepeats(t *testing.T) {
	t.Parallel()
	prov := script(
		runOf(says("first report")),
		runOf(says("first report, again")),
		runOf(calling("working", "read"), llmtest.Response{Usage: llm.Usage{Input: 10, Output: 40}}),
		runOf(says("second report")),
		runOf(says("second report, again")),
	)
	res, err := run(t, toolloop.Config{Provider: prov, Surface: newSurface("read", "submit_work"),
		MaxRounds: 6, TerminateAfter: []string{"submit_work"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if want := "first report\n\nworking\n\nsecond report"; res.Text != want {
		t.Errorf("text = %q, want %q", res.Text, want)
	}
	if res.RoundsUsed != 6 || len(res.Narration) != 5 {
		t.Errorf("rounds = %d narrated = %d, want 6 rounds and the 5 that spoke on the record",
			res.RoundsUsed, len(res.Narration))
	}
}

// ONLY A TERMINATOR THE SURFACE OFFERS IS OWED: prose is the answer of a loop
// whose terminator cannot be called, and nothing is marked or withheld.
func TestATerminatorTheSurfaceDoesNotOfferIsNotOwed(t *testing.T) {
	t.Parallel()
	prov := script(runOf(says("the answer")), runOf(says("the answer")))
	res, err := run(t, toolloop.Config{Provider: prov, Surface: newSurface("read"),
		MaxRounds: 5, TerminateAfter: []string{"submit_work"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.RoundsUsed != 1 || finishingCorrectives(res.Messages) != 0 {
		t.Errorf("rounds = %d, correctives = %d, want 1 and 0 — the submission was not on the surface",
			res.RoundsUsed, finishingCorrectives(res.Messages))
	}
	if len(res.Narration) != 1 || res.Narration[0].Declined {
		t.Errorf("narration = %+v, want the round not marked declined", res.Narration)
	}
	if res.Withheld != "" {
		t.Errorf("withheld = %q, want nothing owed to a phase that could not call it", res.Withheld)
	}
}

// A FAILED TERMINATOR DOES NOT END THE PHASE: its failure went back to the
// model, which is expected to fix it. The counterfactual — a successful one
// ends it at once — keeps the first half honest against a terminator that never
// terminates.
func TestAFailedTerminatorDoesNotEndThePhase(t *testing.T) {
	t.Parallel()
	surf := newSurface("submit")
	surf.results["submit"] = toolloop.ToolResult{Output: "Invalid submission: decision must be one of…", Failed: true}
	prov := script(runOf(calling("", "submit"), calling("", "submit"), says("gave up")))
	res, err := run(t, toolloop.Config{Provider: prov, Surface: surf, MaxRounds: 5,
		TerminateAfter: []string{"submit"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.RoundsUsed < 3 || len(surf.calls()) != 2 {
		t.Errorf("rounds = %d ran %v — a failed terminator ended the phase", res.RoundsUsed, surf.calls())
	}

	ok := newSurface("submit")
	fresh := script(runOf(calling("", "submit"), says("should never be reached")))
	res, err = run(t, toolloop.Config{Provider: fresh, Surface: ok, MaxRounds: 5,
		TerminateAfter: []string{"submit"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.RoundsUsed != 1 {
		t.Errorf("rounds = %d, want the successful terminator to end it at 1", res.RoundsUsed)
	}
}

// --- validation and the surface ----------------------------------------------

func TestRunRefusesAConfigThatCannotWork(t *testing.T) {
	t.Parallel()
	h := host(t)
	base := toolloop.Config{Provider: script(), Surface: newSurface(), Host: h, MaxRounds: 1}
	for name, mangle := range map[string]func(*toolloop.Config){
		"no provider":                func(c *toolloop.Config) { c.Provider = nil },
		"no surface":                 func(c *toolloop.Config) { c.Surface = nil },
		"no session host":            func(c *toolloop.Config) { c.Host = nil },
		"no rounds":                  func(c *toolloop.Config) { c.MaxRounds = 0 },
		"negative cap":               func(c *toolloop.Config) { c.MaxRounds = -1 },
		"negative correctives spent": func(c *toolloop.Config) { c.CorrectivesSpent = -1 },
		"more correctives spent than the bound": func(c *toolloop.Config) {
			c.CorrectivesSpent = toolloop.MaxFinishingCorrectives + 1
		},
	} {
		cfg := base
		mangle(&cfg)
		if _, err := toolloop.Run(t.Context(), cfg); err == nil {
			t.Errorf("%s: Run accepted it", name)
		}
	}
}

// A FAILING SURFACE IS AN ENGINE FAILURE, not a tool's: the loop cannot go on.
// The counterfactual is the next case.
func TestASurfaceFailureIsAnEngineFailure(t *testing.T) {
	t.Parallel()
	surf := newSurface("read")
	surf.broken = errors.New("registry gone")
	prov := script(runOf(calling("", "read"), says("never")))
	if _, err := run(t, toolloop.Config{Provider: prov, Surface: surf, MaxRounds: 2}); err == nil {
		t.Fatal("a broken surface was treated as a tool failure")
	}
}

// A FAILING TOOL GOES BACK TO THE MODEL, flagged: its message is what the model
// reacts to, and the flag is what a backend renders the failure from — on the
// answer the CLI read and on the conversation the next run reads alike, and
// only on the call that failed.
func TestAFailingToolGoesBackToTheModelFlagged(t *testing.T) {
	t.Parallel()
	surf := newSurface("bad", "good")
	surf.results["bad"] = toolloop.ToolResult{Output: "permission denied", Failed: true}
	prov := script(runOf(calling("", "bad", "good"), says("I see it failed")))
	res, err := run(t, toolloop.Config{Provider: prov, Surface: surf, MaxRounds: 5})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Executions) != 2 || !res.Executions[0].Failed || res.Executions[1].Failed {
		t.Fatalf("executions = %+v, want only bad marked failed", res.Executions)
	}
	answered := prov.Results()
	if len(answered) != 2 || !answered[0].Failed || answered[0].Output != "permission denied" ||
		answered[1].Failed {
		t.Errorf("the CLI was answered %+v, want only bad's failure flagged", answered)
	}
	var flags []bool
	for _, m := range res.Messages {
		if m.Role == llm.RoleTool {
			flags = append(flags, m.Failed)
			if m.Name == "bad" && m.Content != "permission denied" {
				t.Errorf("the failing tool's output never reached the record: %+v", m)
			}
		}
	}
	if !slices.Equal(flags, []bool{true, false}) {
		t.Errorf("tool messages flagged %v, want only the failed one", flags)
	}
}

// AN ACTIVATION IS OFFERED TO THE RUN'S NEXT REQUEST on a CLI that re-lists: the
// session's list moves with the surface, so the tool is callable at once and the
// run is not ended for it.
func TestAnActivatedToolIsCallableOnTheNextRequest(t *testing.T) {
	t.Parallel()
	surf := newSurface("activate_tool")
	surf.activates = "activate_tool"
	surf.adds = llm.ToolDef{Name: "newly_activated", Parameters: map[string]any{"type": "object"}}
	prov := script(runOf(calling("", "activate_tool"), calling("", "newly_activated"), says("done")))
	if _, err := run(t, toolloop.Config{Provider: prov, Surface: surf}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := surf.calls(); !slices.Equal(got, []string{"activate_tool", "newly_activated"}) {
		t.Errorf("the surface ran %v, want the activated tool called on the next request", got)
	}
	for _, r := range prov.Results() {
		if r.Err != nil || r.Failed {
			t.Errorf("the CLI was answered %+v", r)
		}
	}
	if n := len(prov.Seen()); n != 1 {
		t.Errorf("%d runs, want one — a CLI that re-lists needs no new run", n)
	}
}

// --- narration ---------------------------------------------------------------

// NARRATION STAYS ATTACHED TO ITS OWN ROUND, one-based and on the scale
// Execution.Round is, because that is the only thing letting a reader
// interleave the two lists — and a round that only called tools narrates
// nothing, since an empty entry renders as a blank paragraph above its tools.
func TestNarrationStaysAttachedToItsOwnRound(t *testing.T) {
	t.Parallel()
	prov := script(runOf(calling("looking it up", "search"), calling("", "search"), says("done")))
	res, err := run(t, toolloop.Config{Provider: prov, Surface: newSurface("search"), MaxRounds: 4})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := []toolloop.Narration{{Round: 1, Content: "looking it up"}, {Round: 3, Content: "done"}}
	if !slices.Equal(res.Narration, want) {
		t.Fatalf("narration = %+v, want %+v", res.Narration, want)
	}
	if len(res.Executions) != 2 || res.Executions[0].Round != 1 || res.Executions[1].Round != 2 {
		t.Errorf("executions %+v, want one on each of rounds 1 and 2", res.Executions)
	}
}

// THE FAILURE VIEW CARRIES WHAT THE PHASE MANAGED: a phase that died used to
// leave only its "started" event. Its calls, its tokens, its prose — and its
// last words, which are what a frozen row needs most — are on the snapshot the
// caller publishes with the error, and it does not read as exhausted: it did
// not run out, it failed.
//
// The round a CLI run died on is the last one its record showed: what the CLI
// asked after it, if anything, never came back.
func TestTheFailureViewCarriesWhatThePhaseManaged(t *testing.T) {
	t.Parallel()
	prog := &toolloop.Progress{}
	died := errors.New("the CLI fell over")
	prov := script(llmtest.Run{
		Responses: []llmtest.Response{{Text: "first", Calls: []llmtest.Call{{Name: "read"}},
			Usage: llm.Usage{Input: 7}}},
		Err: died,
	})
	_, err := run(t, toolloop.Config{Provider: prov, Surface: newSurface("read"), MaxRounds: 5,
		Progress: prog})
	if !errors.Is(err, died) {
		t.Fatalf("Run returned %v, want the run's failure", err)
	}
	snap := prog.Snapshot()
	if len(snap.Executions) != 1 {
		t.Errorf("executions = %+v, want the call that ran before the failure", snap.Executions)
	}
	if snap.InputTokens != 7 {
		t.Errorf("input tokens = %d, want the 7 already billed", snap.InputTokens)
	}
	if !strings.Contains(snap.Text, "first") {
		t.Errorf("text = %q, want the conversation so far", snap.Text)
	}
	if len(snap.Narration) != 1 || snap.Narration[0].Content != "first" {
		t.Errorf("narration = %+v, want the round that ran", snap.Narration)
	}
	if snap.RoundsUsed != 1 || len(snap.Rounds) != 1 {
		t.Errorf("rounds = %d (%d recorded), want the one the record showed", snap.RoundsUsed, len(snap.Rounds))
	}
	if inv := snap.Invocations; len(inv) != 1 || inv[0].Outcome != toolloop.OutcomeFailed {
		t.Errorf("invocations = %+v, want the one run, failed", inv)
	}
	if snap.ExhaustedRounds {
		t.Error("a phase that DIED reports exhausted rounds")
	}
}
