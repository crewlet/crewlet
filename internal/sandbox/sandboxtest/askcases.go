package sandboxtest

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/decision"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A run's question on its record: the cases [Run] drives for it.
//
// EVERY ONE IS A PROPERTY OF THE WRITE THAT RECORDS IT
// ([sandbox.PendingStore.RecordAsk]): pinned to the job that asked and to its
// running, one per job, redacted where it is written, and gone when the next
// job opens. The tool that takes the ask and the park that reads it are the
// engine's and the coordinator's; what is certified here is that the row holds
// exactly the question a collection may park on, and nothing else.

// askCases are the ask cases, in the shape [Run] drives.
var askCases = []struct {
	name string
	fn   func(*testing.T, sandbox.PendingStore)
}{
	{"AnAskIsRecordedOnTheJobsRow", testAnAskIsRecordedOnTheJobsRow},
	{"AnAskIsRecordedRedacted", testAnAskIsRecordedRedacted},
	{"AnAskIsRecordedOnlyForTheJobThatIsRunning", testAnAskIsRecordedOnlyForTheJobThatIsRunning},
	{"AJobAsksOneQuestion", testAJobAsksOneQuestion},
	{"TheNextJobOpensWithNoQuestion", testTheNextJobOpensWithNoQuestion},
	{"AnInvalidAskIsAnError", testAnInvalidAskIsAnError},
	{"AnAnswerCutsTheRoundWhereTheAskingJobStopped", testAnAnswerCutsTheRoundWhereTheAskingJobStopped},
}

// branchDecision is a decision a run asks with: two options, its
// recommendation and why, and what it looked at.
func branchDecision(question string) *decision.Decision {
	return &decision.Decision{
		Question: question,
		Options: []decision.Option{
			{ID: "main", Label: "Target main", Detail: "ships with the next release"},
			{ID: "release", Label: "Target release/2", Detail: "a patch release"},
		},
		Recommended: "main",
		Rationale:   "the flake reproduces on main only",
		Evidence: []decision.Evidence{
			{Kind: decision.EvidenceURL, Ref: "https://example.com/ci/42", Label: "the failing job"},
		},
		Role: decision.RoleApprover,
	}
}

// branchAsk is a structured ask with every field set.
func branchAsk() sandbox.Ask {
	const question = "Which branch should the fix target?"
	return sandbox.Ask{
		Question: question, Context: "The flake is in `ci/test.sh`; both branches carry it.",
		Decision: branchDecision(question), To: sandbox.AskToManager, Branch: "wip/swe/t1",
	}
}

// questionOf is the part of a row an ask writes, for comparing two rows on it.
func questionOf(r sandbox.PendingRun) sandbox.Ask {
	return sandbox.Ask{Question: r.Question, Context: r.QuestionContext, Decision: r.Decision,
		To: r.Audience, Branch: r.Branch}
}

func testAnAskIsRecordedOnTheJobsRow(t *testing.T, s sandbox.PendingStore) {
	// RECORDED ON THE FIELDS A PARKED QUESTION IS READ FROM, whole, and on
	// a job still launching as well as on one running: a harness can ask before
	// the turn that started it has suspended.
	for name, launch := range map[string]func(*testing.T, sandbox.PendingStore, sandbox.PendingRun) string{
		"launching": mustBeginLaunch,
		"running":   mustLaunched,
	} {
		t.Run(name, func(t *testing.T) {
			r := run("t-ask-" + name)
			job := launch(t, s, r)
			ask := branchAsk()
			got, err := s.RecordAsk(t.Context(), r.TurnID, job, ask)
			if err != nil || got != sandbox.AskRecorded {
				t.Fatalf("RecordAsk = %q, %v; want it recorded", got, err)
			}
			row := mustGet(t, s, r.TurnID)
			if !reflect.DeepEqual(questionOf(row), ask) {
				t.Fatalf("the row holds %+v (decision %+v), want the ask as it was asked, %+v",
					questionOf(row), row.Decision, ask)
			}
			// AND NOTHING ELSE MOVES: the job is still the one running,
			// and nothing is parked until its collection parks it.
			if row.LaunchID != job || row.Status == sandbox.StatusAwaiting || !row.AskedAt.IsZero() {
				t.Errorf("recording the ask moved the row to launch %q, status %q, asked at %v",
					row.LaunchID, row.Status, row.AskedAt)
			}
		})
	}
}

func testAnAskIsRecordedRedacted(t *testing.T, s sandbox.PendingStore) {
	// REDACTED WHERE IT IS WRITTEN, because the row, the park's
	// announcement and the continuing run's prompt keep it, and a box holds
	// the seat's credentials. A branch the redaction would change is no
	// branch at all, so it is not recorded.
	secret := "ghp_" + strings.Repeat("d", 36)
	ask := branchAsk()
	ask.Question = "Is " + secret + " the token to push with?"
	ask.Context = "It was in the environment: " + secret
	ask.Decision = branchDecision(ask.Question)
	ask.Decision.Options[0].Label = "Use " + secret
	ask.Decision.Options[1].Detail = "not " + secret
	ask.Decision.Rationale = "found " + secret
	ask.Decision.Evidence[0].Label = "the " + secret + " log"
	ask.Branch = "wip/" + secret
	if err := ask.Validate(); err != nil {
		t.Fatalf("the premise: the ask is valid before it is redacted: %v", err)
	}
	job := mustLaunched(t, s, run("t1"))
	if got, err := s.RecordAsk(t.Context(), "t1", job, ask); err != nil || got != sandbox.AskRecorded {
		t.Fatalf("RecordAsk = %q, %v; want it recorded", got, err)
	}
	row := mustGet(t, s, "t1")
	if row.Decision == nil || len(row.Decision.Options) != 2 || len(row.Decision.Evidence) != 1 {
		t.Fatalf("the row holds decision %+v, want the ask's", row.Decision)
	}
	for where, text := range map[string]string{
		"question": row.Question, "context": row.QuestionContext,
		"decision.question":    row.Decision.Question,
		"decision.options[0]":  row.Decision.Options[0].Label,
		"decision.options[1]":  row.Decision.Options[1].Detail,
		"decision.rationale":   row.Decision.Rationale,
		"decision.evidence[0]": row.Decision.Evidence[0].Label,
	} {
		if strings.Contains(text, secret) {
			t.Errorf("a credential was recorded in the %s: %q", where, text)
		}
	}
	if row.Decision.Question != row.Question {
		t.Errorf("the decision asks %q and the row %q; want one question", row.Decision.Question, row.Question)
	}
	if row.Branch != "" {
		t.Errorf("the row names branch %q, which a redaction changed; want none", row.Branch)
	}
	// And the ask the caller holds is its own: the store redacted a copy.
	if !strings.Contains(ask.Decision.Options[0].Label, secret) {
		t.Error("recording the ask rewrote the caller's decision in place")
	}
}

func testAnAskIsRecordedOnlyForTheJobThatIsRunning(t *testing.T, s sandbox.PendingStore) {
	// PINNED TO THE JOB AND TO ITS RUNNING, like a bridged call: a call that
	// outlived its job never parks the next one, a job its collection has
	// claimed is not changed under it, and a question on a row already
	// parked would be a second question one answer cannot resume.
	ctx := t.Context()
	for name, arrange := range map[string]func(t *testing.T, turnID string) string{
		"another job's launch": func(t *testing.T, turnID string) string {
			first := mustLaunched(t, s, run(turnID))
			mustBeginLaunch(t, s, run(turnID))
			return first
		},
		"no launch at all": func(t *testing.T, turnID string) string {
			mustLaunched(t, s, run(turnID))
			return ""
		},
		"a row its collection claimed": func(t *testing.T, turnID string) string {
			launch := mustLaunched(t, s, run(turnID))
			mustClaim(t, s, turnID)
			return launch
		},
		"a row parked on a question": func(t *testing.T, turnID string) string {
			launch := mustLaunched(t, s, run(turnID))
			park(t, s, turnID)
			return launch
		},
		"a row whose ending is decided": func(t *testing.T, turnID string) string {
			launch := mustLaunched(t, s, run(turnID))
			decide(t, s, turnID, everyJob(sandbox.Fence{}, sandbox.Active))
			return launch
		},
		"no row": func(*testing.T, string) string { return "launch-of-nothing" },
	} {
		t.Run(name, func(t *testing.T) {
			turnID := "t-pinned-" + strings.ReplaceAll(name, " ", "-")
			launch := arrange(t, turnID)
			before, found, err := s.Get(ctx, turnID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			got, err := s.RecordAsk(ctx, turnID, launch, sandbox.Ask{
				Question: "a question nobody should be asked?", To: sandbox.AskToRequester,
			})
			if err != nil || got != sandbox.AskNotRunning {
				t.Fatalf("RecordAsk = %q, %v; want %q", got, err, sandbox.AskNotRunning)
			}
			after, stillFound, err := s.Get(ctx, turnID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if stillFound != found || !reflect.DeepEqual(questionOf(after), questionOf(before)) ||
				after.Status != before.Status {
				t.Errorf("a refused ask wrote the row: %q %+v, was %q %+v",
					after.Status, questionOf(after), before.Status, questionOf(before))
			}
		})
	}
}

func testAJobAsksOneQuestion(t *testing.T, s sandbox.PendingStore) {
	// ONE QUESTION PER JOB, the first standing, because one answer resumes
	// the run: a second would be asked of nobody, or replace the one a
	// person is already reading.
	ctx := t.Context()
	job := mustLaunched(t, s, run("t1"))
	first := branchAsk()
	if got, err := s.RecordAsk(ctx, "t1", job, first); err != nil || got != sandbox.AskRecorded {
		t.Fatalf("the first ask = %q, %v; want it recorded", got, err)
	}
	got, err := s.RecordAsk(ctx, "t1", job, sandbox.Ask{Question: "Actually, which test suite?",
		To: sandbox.AskToTeam})
	if err != nil || got != sandbox.AskAlreadyAsked {
		t.Fatalf("the second ask = %q, %v; want %q", got, err, sandbox.AskAlreadyAsked)
	}
	if row := mustGet(t, s, "t1"); !reflect.DeepEqual(questionOf(row), first) {
		t.Errorf("the row holds %+v, want the first question standing", questionOf(row))
	}
}

func testTheNextJobOpensWithNoQuestion(t *testing.T, s sandbox.PendingStore) {
	// A JOB'S QUESTION IS ITS OWN: answered, or never parked on, it is gone
	// when the next job opens, and the next job may ask one of its own.
	ctx := t.Context()
	first := mustLaunched(t, s, run("t1"))
	if got, err := s.RecordAsk(ctx, "t1", first, branchAsk()); err != nil || got != sandbox.AskRecorded {
		t.Fatalf("RecordAsk = %q, %v; want it recorded", got, err)
	}
	second := mustBeginLaunch(t, s, run("t1"))
	row := mustGet(t, s, "t1")
	if row.Question != "" || row.QuestionContext != "" || row.Decision != nil || row.Audience != "" {
		t.Fatalf("the next job opened asking %q (context %q, decision %+v, to %q); want nothing",
			row.Question, row.QuestionContext, row.Decision, row.Audience)
	}
	next := sandbox.Ask{Question: "Which test suite?", To: sandbox.AskToTeam}
	if got, err := s.RecordAsk(ctx, "t1", second, next); err != nil || got != sandbox.AskRecorded {
		t.Fatalf("the next job's ask = %q, %v; want it recorded", got, err)
	}
	if row := mustGet(t, s, "t1"); row.Question != next.Question || row.Audience != next.To {
		t.Errorf("the row holds %q to %q, want the next job's own question", row.Question, row.Audience)
	}
}

func testAnInvalidAskIsAnError(t *testing.T, s sandbox.PendingStore) {
	// THE TOOL THAT TAKES AN ASK HAS ALREADY REFUSED AN INVALID ONE, so one
	// reaching the store is a caller's defect: an error, marked as a
	// refusal, with nothing written.
	job := mustLaunched(t, s, run("t1"))
	got, err := s.RecordAsk(t.Context(), "t1", job, sandbox.Ask{To: sandbox.AskToRequester})
	if !errors.Is(err, decision.ErrInvalid) {
		t.Fatalf("RecordAsk of an ask with no question = %q, %v; want a refusal", got, err)
	}
	if row := mustGet(t, s, "t1"); row.Question != "" || row.Audience != "" {
		t.Errorf("a refused ask wrote %q to %q", row.Question, row.Audience)
	}
}

// AN ANSWER CUTS THE ROUND WHERE THE ASKING JOB STOPPED. The job that
// continues one which asked is opened after every call the row holds then, and
// the row records that place ([sandbox.PendingRun.AnsweredThrough]): the
// collection reports no submission from before it, made without the answer. A
// job carried on after a loss keeps the cut, which the answer made and the loss
// did not move; a new round has none.
//
// Mutation: leave the place unwritten, or write it on every continuation, and
// the round reports a submission made before the answer, or loses one made
// after it.
func testAnAnswerCutsTheRoundWhereTheAskingJobStopped(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	const turn = "t-answer-cut"
	call := func(launch string, seq int, name string) {
		t.Helper()
		if _, err := s.AppendBridgeCall(ctx, turn, sandbox.BridgeAppend{Launch: launch,
			Call: sandbox.BridgeCall{Name: name, Seq: seq, At: base}}); err != nil {
			t.Fatalf("AppendBridgeCall: %v", err)
		}
	}
	carryOn := func() string {
		t.Helper()
		next := run(turn)
		next.Continues = true
		return mustLaunched(t, s, next)
	}

	asking := mustLaunched(t, s, run(turn))
	call(asking, 1, "submit_work")
	call(asking, 2, "ask_and_wait")
	if got, err := s.RecordAsk(ctx, turn, asking, branchAsk()); err != nil || got != sandbox.AskRecorded {
		t.Fatalf("RecordAsk = %q, %v; want it recorded", got, err)
	}
	if got := mustGet(t, s, turn); got.AnsweredThrough != 0 {
		t.Fatalf("a round no answer carried on is cut at %d, want 0", got.AnsweredThrough)
	}
	answered := carryOn()
	if got := mustGet(t, s, turn); got.AnsweredThrough != 2 || len(got.BridgeCalls) != 2 {
		t.Fatalf("the answer's job opened with the cut at %d over %d calls, want 2 over the "+
			"asking job's 2", got.AnsweredThrough, len(got.BridgeCalls))
	}
	call(answered, 3, "read_file")
	carryOn()
	if got := mustGet(t, s, turn); got.AnsweredThrough != 2 {
		t.Errorf("a job carried on without an answer moved the cut to %d, want 2", got.AnsweredThrough)
	}
	mustLaunched(t, s, run(turn))
	if got := mustGet(t, s, turn); got.AnsweredThrough != 0 {
		t.Errorf("a new round starts cut at %d, want 0", got.AnsweredThrough)
	}
}
