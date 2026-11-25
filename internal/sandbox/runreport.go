package sandbox

import (
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/redact"
)

func fenceOf(run PendingRun) Fence {
	return Fence{Owner: run.Owner, Epoch: run.OwnerEpoch}
}

// runReport is a collected run's own account of itself, filed by the resumed turn
// as its executor's report: the reviewer judges the run on it beside its bridge
// calls, and the turn's record carries it.
//
// THE RUN'S WORDS AND WHY IT ENDED, nothing addressed to anybody: the run IS the
// executor (it worked, told whoever waited through its tools, ended), so no later
// executor is there to instruct, and an instruction here would be read by the
// reviewer as the executor's and filed as what the turn did. Secret-redacted, as it
// is published on the phase record. It carries the findings (reported text,
// delivered refs, the push record's pushes to Gitlet, or the error) and TAKES THE
// RUN, since whether it says "nothing" was pushed turns on what the run was given
// ([pushedText]): its repositories are the row's, never the result's.
func runReport(run PendingRun, result Result) string {
	status := "did NOT fully succeed"
	if result.Success {
		status = "succeeded"
	}
	lines := []string{"The executor's run " + status + "."}
	switch {
	case len(result.DeliveredRefs) > 0:
		delivered := "Delivered: " + strings.Join(result.DeliveredRefs, ", ")
		if result.DeliveredRefsElided > 0 {
			// SAID, so a short list is not read as all of it.
			delivered += fmt.Sprintf(" (and %d more its report names, not listed here)",
				result.DeliveredRefsElided)
		}
		lines = append(lines, delivered)
	case result.DeliveredRefsElided > 0:
		// SAID WHERE NONE IS LISTED TOO: a ref past [MaxDeliveredRefBytes], or not
		// UTF-8, is never listed ([boundRefs]) yet the record and dashboard count it;
		// dropped unsaid, the report looked like a run that delivered nothing.
		lines = append(lines, fmt.Sprintf("Delivered: %d ref(s) its report names, not listed here",
			result.DeliveredRefsElided))
	}
	if pushed := pushedText(run, result); pushed != "" {
		lines = append(lines, pushed)
	}
	if opened := openedText(result); opened != "" {
		lines = append(lines, opened)
	}
	switch {
	case result.Text != "":
		lines = append(lines, "\n"+result.Text)
		// WHY, BESIDE WHAT IT SAID: a run that wrote a report and still failed
		// (crashed after, unreadable result, harness error after an answer) would
		// otherwise be filed "did NOT fully succeed" with no reason.
		if !result.Success && result.Error != "" && result.Error != result.Text {
			lines = append(lines, "\nWhy it did not succeed: "+result.Error)
		}
	case result.Error != "":
		lines = append(lines, "\nError: "+result.Error)
	}
	return redact.Secrets(strings.Join(lines, "\n"))
}

// AskedRun is what a person's answer resumes a parked run with
// ([ResumeRequest.Asked]). Redacted, like everything a resume hands the turn,
// which hands it as is to the run it carries on, whose box and session keep it.
type AskedRun struct {
	// Question is what the run asked and Answer the reply; By is who gave it
	// where the route knows (an answer by turn names the person; a chat reply's
	// sender is already in the conversation), else "".
	Question, Answer, By string

	// Branch is the work-in-progress branch the run pushed before asking, "" if its
	// ask named none: the durable half of the work, which a run on a fresh machine
	// starts from.
	Branch string

	// SessionID is the asking job's harness session, continued by the relaunch where
	// it runs in the asking job's box ([Continuation]): the one the box ran
	// ([PendingRun.SessionRan]), "" where the row cannot say, continuing none.
	SessionID string

	// BoxKept is whether that box is still there. False once reclaimed while the
	// question waited (reaped past the pause TTL, or torn down at the block under a
	// zero TTL): the work carries on on a fresh machine from the branch, and the
	// run must be told or it looks for a working tree that is gone.
	BoxKept bool

	// Label is who wrote Answer as the fence around the reply's message labelled it
	// ([AnswerWords.Label]), for a run reading a chat reply inside a fence of its
	// turn's token; nil for an answer by turn, the person's own instruction, read
	// defused.
	Label *provenance.Label

	// Held is the call to a tool at Needs approval the question was about, with the
	// answer's choice and the writes making it at most once; nil otherwise
	// (ADR-0077).
	Held *HeldResume

	// Withdrawn is whether the run's own seat gave the answer: it withdrew the
	// question, or the held call (as Deny), Answer its reason (ADR-0111). Derived
	// from who answered, never set beside Refused: a refused reply is no answer.
	Withdrawn bool

	// Waited is how the run a wait was for left its seat's slots, where the
	// engine carried a run parked by `wait_for_run` on (ADR-0176, waitover.go);
	// nil otherwise.
	Waited *types.RunWaited

	// Called is every bridged call the job parked on ([PendingRun.ParkedCalls]),
	// whatever answered its ask: each told with what it returned. CallsReturned
	// is whether the engine carried the job on because they returned, the job
	// having asked nothing else (waitover.go), never a person's answer.
	Called        []types.CallReturned
	CallsReturned bool

	// Refused is set where the question's outcome is no answer but the REFUSAL of a
	// reply too large to be one ([PendingRun.AnswerRefusal]): what the run is told
	// (how many messages, the figures) in place of an answer, Answer and By empty.
	// NEVER AN EMPTY ANSWER: every reader handing the run its answer tells it the
	// refusal instead where this is set (ADR-0114).
	Refused *AnswerRefusal
}

// WithdrawnBy reports an answer given by the run's own seat: an agent seat
// answers no question but its own run's, and only to withdraw it.
func WithdrawnBy(run PendingRun, by, byKind string) bool {
	return by != "" && by == run.AgentHandle && byKind == string(iam.ActorAgent)
}

// askedRun is what run's recorded answer resumes it with: the words its seal
// opened to ([AnswerSeal.Open]), nothing else on the row about them; for a
// refusal, the refusal they carry in place of an answer.
func askedRun(run PendingRun, answer AnswerWords) *AskedRun {
	if answer.Refusal != nil {
		refusal := *answer.Refusal
		return &AskedRun{
			Question: redact.Secrets(run.Question), Branch: run.Branch, SessionID: run.SessionRan(),
			BoxKept: run.SandboxID != "", Refused: &refusal, Called: run.CallsReturned(),
		}
	}
	text, err := AnswerText(run.Decision, answer.Choice, answer.Text)
	if err != nil {
		// UNREACHABLE: a choice is checked against its question before recording,
		// and a new question comes only under a new job. Said, and the run is handed
		// the words given, not a choice nobody offered.
		log.Error("sandbox_answer_choice_unread", "turn_id", run.TurnID,
			"launch_id", run.LaunchID, "choice", answer.Choice, "error", err.Error())
		text = answer.Text
	}
	return &AskedRun{
		Question: redact.Secrets(run.Question), Answer: redact.Secrets(text),
		By: answer.attribution(), Branch: run.Branch, SessionID: run.SessionRan(),
		BoxKept:   run.SandboxID != "",
		Label:     answer.Label,
		Withdrawn: WithdrawnBy(run, answer.By, answer.ByKind),
		Called:    run.CallsReturned(),
	}
}
