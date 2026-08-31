package extension_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/extension"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	llm "github.com/crewlet/crewlet/internal/providers/llm"
)

// answering is a model that returns one canned answer and records what it was
// asked.
type answering struct {
	answer string
	err    error
	nilOut bool
	seen   llm.Request
	calls  int
}

func (a *answering) Complete(_ context.Context, req llm.Request) (*llm.Completion, error) {
	a.calls++
	a.seen = req
	if a.err != nil {
		return nil, a.err
	}
	if a.nilOut {
		return nil, nil
	}
	return &llm.Completion{Content: a.answer}, nil
}

func judgeReq() extension.Request {
	return extension.Request{
		Phase:                 phase.Execute,
		Task:                  "reply to the review comment",
		PlanSummary:           "read the thread, then post",
		RoundsUsed:            20,
		MaxStep:               8,
		RemainingUnderCeiling: 20,
		Calls: []ledger.Call{
			{Name: "github_get_pull_request", Args: map[string]any{"number": 7}},
			{Name: "github_get_pull_request", Args: map[string]any{"number": 7}},
		},
	}
}

// --- the verdict format is the contract between the prompt and the parser ---

func TestTheVerdictFormatIsRead(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		answer string
		extend bool
		rounds int
		reason string
	}{
		{"the documented extend shape", "EXTEND 4\nstill fetching new issues", true, 4, "still fetching new issues"},
		{"the documented rescue shape", "RESCUE\nsame call twice with identical args", false, 0, "same call twice with identical args"},
		{"lower case", "extend 2\nprogressing", true, 2, "progressing"},
		{"reason on the verdict line", "EXTEND 3 nearly done", true, 3, "nearly done"},
		{"a code fence", "```\nRESCUE\nlooping\n```", false, 0, "looping"},
		{"a bullet", "- EXTEND 5\n- converging", true, 5, "converging"},
		{"a Verdict: prefix", "Verdict: RESCUE\nno progress", false, 0, "no progress"},
		{"leading blank lines", "\n\nEXTEND 1\nnearly", true, 1, "nearly"},
		{"trailing punctuation on the count", "EXTEND 6.\nfine", true, 6, "fine"},
		// A judge that chose extend without a usable number is not a
		// refusal: Policy.Grant gives it the step, which is the right
		// answer for a model that wrote "a few more".
		{"extend with no count", "EXTEND\nmaking progress", true, 0, "making progress"},
		// The verdict line's remainder is the reason when there is one,
		// so "a few" wins over the line below it. The count is what was
		// unreadable, not the reason, and Policy.Grant gives an extend
		// with no usable count the step.
		{"extend with a word count", "EXTEND a few\nmaking progress", true, 0, "a few"},
		{"no reason at all", "RESCUE", false, 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := extension.ParseVerdict(tc.answer)
			if err != nil {
				t.Fatalf("ParseVerdict(%q) = %v", tc.answer, err)
			}
			if got.Extend != tc.extend {
				t.Errorf("Extend = %v, want %v", got.Extend, tc.extend)
			}
			if got.AdditionalRounds != tc.rounds {
				t.Errorf("AdditionalRounds = %d, want %d", got.AdditionalRounds, tc.rounds)
			}
			if got.Reason != tc.reason {
				t.Errorf("Reason = %q, want %q", got.Reason, tc.reason)
			}
		})
	}
}

// AN ANSWER THAT IS NOT A VERDICT IS NOT GUESSED AT.
//
// The safe direction: Consider turns an error into a rescue, and a rescue is
// the outcome the phase was already heading for. Reading "I think it should
// probably continue" as EXTEND grants rounds nobody decided to give.
func TestAnAnswerThatIsNotAVerdictIsRefused(t *testing.T) {
	t.Parallel()
	for _, answer := range []string{
		"",
		"   \n\n  ",
		"I think the phase is making good progress and should extend.",
		"Sure! Here's my assessment:",
		"MAYBE 3\nnot sure",
		// The word appears, but in prose that concludes the opposite —
		// which is exactly why only the FIRST non-empty line is read.
		"The agent asked me to EXTEND but it is clearly looping.\nRESCUE",
	} {
		if _, err := extension.ParseVerdict(answer); !errors.Is(err, extension.ErrNoVerdict) {
			t.Errorf("ParseVerdict(%q) err = %v, want ErrNoVerdict", answer, err)
		}
	}
}

// --- the call itself --------------------------------------------------------

func TestTheJudgeAsksTheModelAndReturnsItsVerdict(t *testing.T) {
	t.Parallel()
	model := &answering{answer: "EXTEND 3\nfetching distinct pages"}
	j := extension.NewLLMJudge(model, "cheap")

	got, err := j.Decide(t.Context(), judgeReq())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if !got.Extend || got.AdditionalRounds != 3 {
		t.Fatalf("decision = %+v", got)
	}
	if model.calls != 1 {
		t.Errorf("the judge made %d model calls, want one", model.calls)
	}
}

// THE EVIDENCE REACHES THE MODEL, arguments included.
//
// A judge shown only tool NAMES cannot tell a loop from a sequence — two
// calls to the same tool are the whole question — so the arguments are the
// discrimination and their absence would make every verdict a guess.
func TestTheJudgeIsShownTheEvidence(t *testing.T) {
	t.Parallel()
	model := &answering{answer: "RESCUE\nlooping"}
	j := extension.NewLLMJudge(model, "cheap")
	if _, err := j.Decide(t.Context(), judgeReq()); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	if len(model.seen.Messages) != 2 {
		t.Fatalf("the judge sent %d messages", len(model.seen.Messages))
	}
	user := model.seen.Messages[1].Content
	for _, want := range []string{
		"github_get_pull_request", // the tool log
		"number=7",                // and its arguments
		"reply to the review comment",
		"read the thread, then post",
		"Most you may grant now: 8",
	} {
		if !strings.Contains(user, want) {
			t.Errorf("the judge was not shown %q:\n%s", want, user)
		}
	}
	// A classifier, so the same evidence must reach the same verdict.
	if model.seen.Temperature == nil || *model.seen.Temperature != 0 {
		t.Errorf("temperature = %v, want a deterministic 0", model.seen.Temperature)
	}
	// No tools: a tool on the surface invites a model to call it and
	// answer nothing, and there is none this pass could use.
	if len(model.seen.Tools) != 0 {
		t.Errorf("the judge was offered %d tools", len(model.seen.Tools))
	}
}

// ARGUMENTS RENDER IN A STABLE ORDER.
//
// Go map iteration is randomised, and this text is the judge's only way to
// tell two calls apart: the same call rendered with its keys in a different
// order reads as a different call, which turns a loop into apparent progress
// at random.
func TestOneCallAlwaysRendersTheSameWay(t *testing.T) {
	t.Parallel()
	req := extension.Request{
		Phase: phase.Execute,
		Calls: []ledger.Call{{Name: "search", Args: map[string]any{
			"zebra": 1, "alpha": 2, "mid": 3, "beta": 4, "yankee": 5,
		}}},
	}
	var first string
	for range 20 {
		model := &answering{answer: "RESCUE\nx"}
		if _, err := extension.NewLLMJudge(model, "k").Decide(t.Context(), req); err != nil {
			t.Fatal(err)
		}
		rendered := model.seen.Messages[1].Content
		if first == "" {
			first = rendered
			continue
		}
		if rendered != first {
			t.Fatal("the same call rendered two different ways, so a loop can read as progress")
		}
	}
}

// A FAILED CALL IS AN ERROR, NOT A SILENT RESCUE.
//
// Consider turns it into a rescue and carries the reason; building the rescue
// here would make "the model said stop" and "the model could not be reached"
// the same line in a log somebody is reading to find out why nothing is ever
// extended.
func TestAFailedModelCallIsAnError(t *testing.T) {
	t.Parallel()
	j := extension.NewLLMJudge(&answering{err: errors.New("429")}, "cheap")
	if _, err := j.Decide(t.Context(), judgeReq()); err == nil {
		t.Fatal("a failed model call was reported as a decision")
	}
}

// A PROVIDER ANSWERING (nil, nil) IS A CONTRACT VIOLATION, and it is checked
// rather than dereferenced: the panic would surface as a failed turn on the
// phase this was trying to be generous to.
func TestAProviderThatReturnsNothingDoesNotPanic(t *testing.T) {
	t.Parallel()
	j := extension.NewLLMJudge(&answering{nilOut: true}, "cheap")
	if _, err := j.Decide(t.Context(), judgeReq()); !errors.Is(err, extension.ErrNoVerdict) {
		t.Errorf("err = %v, want ErrNoVerdict", err)
	}
}

// A NIL MODEL YIELDS A NIL JUDGE, which Consider already reads as "do not
// ask". A judge that errored on every call would report a failure on a
// company that simply has no model to ask.
func TestNoModelMeansNoJudge(t *testing.T) {
	t.Parallel()
	if j := extension.NewLLMJudge(nil, "k"); j != nil {
		t.Fatal("a nil model produced a judge")
	}
}

// AND THE WHOLE PATH: a judge wired into Consider grants rounds the policy
// clamps, rather than the rescue the engine gave for every exhaustion while
// no implementation of this interface existed.
func TestAWiredJudgeGrantsRoundsThroughThePolicy(t *testing.T) {
	t.Parallel()
	p := extension.Policy{Enabled: true, RoundStep: 5, Ceiling: 40}
	j := extension.NewLLMJudge(&answering{answer: "EXTEND 50\nclearly progressing"}, "cheap")

	granted, d := extension.Consider(t.Context(), j, p, judgeReq())
	if !d.Extend {
		t.Fatalf("decision = %+v", d)
	}
	// The judge asked for 50; the policy's step is what it gets. The
	// arithmetic is deliberately not the judge's.
	if granted != 5 {
		t.Errorf("granted = %d, want the policy's step of 5", granted)
	}
}

// EACH PHASE IS TOLD ITS OWN WAY OUT.
//
// A phase told the wrong way to finish spends its granted rounds trying to
// exit through a door it does not have. Onboarding is the case that was
// missing and the one that was silent: told to stop calling tools, an
// extended onboarding pass never reaches mark_onboarded, so the marker is
// never stamped and the pass re-runs on every turn that seat ever takes.
func TestEveryPhaseIsToldHowItActuallyEnds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		ph   phase.Phase
		want string
	}{
		{phase.Plan, "submit_plan"},
		{phase.Onboarding, "mark_onboarded"},
		{phase.Execute, "stop calling tools"},
		{phase.Review, "stop calling tools"},
	} {
		if got := extension.FinishHint(tc.ph); !strings.Contains(got, tc.want) {
			t.Errorf("FinishHint(%s) = %q, want it to name %q", tc.ph, got, tc.want)
		}
	}
}
