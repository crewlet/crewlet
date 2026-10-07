package extension_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/extension"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/compact"
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
		`"number":7`,              // and its arguments
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

// cachedModel answers with a verdict — or with prose — and a prompt-cache share.
type cachedModel struct{ answer string }

func (c cachedModel) Complete(context.Context, llm.Request) (*llm.Completion, error) {
	return &llm.Completion{
		Content: c.answer, Model: "cheap-model",
		InputTokens: 900, OutputTokens: 12, CacheRead: 700, CacheWrite: 150,
	}, nil
}

// entryModel is a judge model behind a chain, which names the entry that
// answered on the completion.
type entryModel struct{}

func (entryModel) Complete(context.Context, llm.Request) (*llm.Completion, error) {
	return &llm.Completion{
		Content: "EXTEND 3\nprogressing", Model: "cheap-model", ProviderKey: "judge-backup",
		InputTokens: 10, OutputTokens: 2,
	}, nil
}

// THE JUDGE'S RECORD NAMES THE ENTRY THAT ANSWERED, as every phase record
// does: the completion's own when a chain reported one, and the key the judge
// was built over when a single backend could not — it never knows the key it
// was configured under. Without it the judge's spend is filed under no entry.
func TestTheJudgeNamesTheEntryThatAnswered(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		model extension.Completer
		want  string
	}{
		{"a bare backend", cachedModel{answer: "EXTEND 3\nprogressing"}, "cheap"},
		{"a chain", entryModel{}, "judge-backup"},
	} {
		j := extension.NewLLMJudge(tc.model, "cheap")
		_, d := extension.Consider(t.Context(), j, extension.Policy{
			Enabled: true, RoundStep: 4, Ceiling: 40,
		}, judgeReq())
		if d.ProviderKey != tc.want {
			t.Errorf("%s: ProviderKey = %q, want %q", tc.name, d.ProviderKey, tc.want)
		}
	}
}

// THE JUDGE'S CACHE SHARE TRAVELS WITH ITS SPEND, on every path — including
// the one where its answer was not a verdict and the policy rescues. Its record
// is a phase record like any other, and a figure the completion reported and
// nothing kept is one no reader can see.
func TestTheJudgeReportsItsCacheShare(t *testing.T) {
	t.Parallel()
	for _, answer := range []string{"EXTEND 3\nfetching distinct pages", "I think it is doing fine"} {
		j := extension.NewLLMJudge(cachedModel{answer: answer}, "cheap")
		_, d := extension.Consider(t.Context(), j, extension.Policy{
			Enabled: true, RoundStep: 4, Ceiling: 40,
		}, judgeReq())
		if d.CacheRead != 700 || d.CacheWrite != 150 || d.InputTokens != 900 || d.Model != "cheap-model" {
			t.Errorf("answer %q: the decision carries %+v, want the call's spend with its cache share", answer, d)
		}
	}
}

type judgeFitter struct{ asked []compact.Kind }

func (f *judgeFitter) Fit(_ context.Context, kind compact.Kind, text string, _ int) (compact.Result, error) {
	f.asked = append(f.asked, kind)
	return compact.Result{Text: "rewrite of " + string(kind), Compacted: true, From: len(text)}, nil
}

// WHAT IT LAST SAID IS WHAT IT SAID LAST. The evidence used to be cut to its
// first eight hundred bytes under a heading promising the last — so a phase
// that ended by announcing it was done was judged on its opening thoughts.
// With no compactor it is shown whole; nothing of it is cut away.
func TestTheJudgeSeesTheEndOfWhatThePhaseSaid(t *testing.T) {
	t.Parallel()
	req := judgeReq()
	req.LastText = strings.Repeat("thinking about the approach. ", 200) + "DONE: posted the reply in the thread."
	model := &answering{answer: "RESCUE\nx"}
	if _, err := extension.NewLLMJudge(model, "k").Decide(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(model.seen.Messages[1].Content, "DONE: posted the reply in the thread.") {
		t.Fatal("the judge was not shown how the phase ended")
	}
}

// TWO CALLS THAT DIFFER PAST TWO HUNDRED BYTES STILL READ AS TWO CALLS. The
// arguments were cut there, so two posts of different bodies sharing an
// opening rendered identically and a phase making progress read as a loop.
func TestTwoCallsThatDifferLateStillRenderDifferently(t *testing.T) {
	t.Parallel()
	opening := strings.Repeat("Hello team, here is the update. ", 10)
	req := judgeReq()
	req.Calls = []ledger.Call{
		{Name: "post", Args: map[string]any{"text": opening + "first version"}},
		{Name: "post", Args: map[string]any{"text": opening + "second version"}},
	}
	model := &answering{answer: "RESCUE\nx"}
	if _, err := extension.NewLLMJudge(model, "k").Decide(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	user := model.seen.Messages[1].Content
	lines := []string{}
	for _, line := range strings.Split(user, "\n") {
		if strings.HasPrefix(line, "- post(") {
			lines = append(lines, line)
		}
	}
	if len(lines) != 2 || lines[0] == lines[1] {
		t.Fatalf("two different calls rendered as one:\n%s", strings.Join(lines, "\n"))
	}
	if strings.Contains(user, "…") {
		t.Error("the judge's evidence was cut")
	}
}

// WITH A COMPACTOR, EVIDENCE PAST ITS BUDGET IS REWRITTEN — the task, the
// last words and an argument — and each rewrite is announced as one.
func TestTheJudgeCondensesEvidencePastItsBudget(t *testing.T) {
	t.Parallel()
	req := judgeReq()
	req.Task = strings.Repeat("the quoted thread goes on. ", 400) + "THE ASK"
	req.LastText = strings.Repeat("x", extension.JudgeTextBudget+1)
	req.Calls = []ledger.Call{{Name: "post", Args: map[string]any{"text": strings.Repeat("body ", 100)}}}
	fit := &judgeFitter{}
	model := &answering{answer: "RESCUE\nx"}
	if _, err := extension.NewLLMJudge(model, "k").WithCompactor(fit).Decide(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	user := model.seen.Messages[1].Content
	for _, want := range []string{"rewrite of task", "rewrite of produced", "(condensed) rewrite of argument",
		"condensed by a model"} {
		if !strings.Contains(user, want) {
			t.Errorf("missing %q:\n%s", want, user)
		}
	}
	if len(fit.asked) != 3 {
		t.Errorf("asked for %d rewrites, want 3", len(fit.asked))
	}
}

// errFull is the refusal a turn's budget holds once a window is full.
var errFull = errors.New("toolloop: token budget exhausted (agent day budget)")

// fillingBudget is a turn's budget a rewrite fills: it refuses nothing until
// the compactor beside it has rewritten something, and everything after.
type fillingBudget struct {
	mu   sync.Mutex
	full bool
}

func (b *fillingBudget) Held(context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.full {
		return errFull
	}
	return nil
}

// fillingFitter is the seat's compactor, each rewrite charged to the budget —
// and taking long enough to be seen on a clock — as a real one's is.
type fillingFitter struct {
	budget *fillingBudget
	took   time.Duration
	// returned is when the last rewrite came back.
	returned time.Time
}

func (f *fillingFitter) Fit(_ context.Context, kind compact.Kind, text string, _ int) (compact.Result, error) {
	time.Sleep(f.took)
	f.budget.mu.Lock()
	f.budget.full = true
	f.budget.mu.Unlock()
	f.returned = time.Now()
	return compact.Result{Text: "rewrite of " + string(kind), Compacted: true, From: len(text)}, nil
}

// A JUDGE WHOSE OWN EVIDENCE FILLS THE TURN'S WINDOW IS NOT CALLED.
//
// Its caller asks the turn's budget before consulting it, and that answer was
// room; rendering the evidence then rewrote a long task with the seat's
// auxiliary model, charged to the same counters, and that rewrite filled the
// window. The judge's call is now certain to be billed and its charge refused,
// so the judge asks again once the evidence is in hand and calls nothing — and
// says it was the budget, not a failed judge, and that nothing was called.
func TestAJudgeWhoseEvidenceFillsTheWindowIsNotCalled(t *testing.T) {
	t.Parallel()
	req := judgeReq()
	req.Task = strings.Repeat("the quoted thread goes on. ", 400) + "THE ASK"
	budget := &fillingBudget{}
	model := &answering{answer: "EXTEND 4\nprogressing"}
	judge := extension.NewLLMJudge(model, "k").
		WithCompactor(&fillingFitter{budget: budget}).WithHold(budget)

	d, err := judge.Decide(t.Context(), req)
	if !errors.Is(err, extension.ErrHeld) || !errors.Is(err, errFull) {
		t.Fatalf("Decide = %v, want ErrHeld carrying the budget's own refusal", err)
	}
	if model.calls != 0 {
		t.Errorf("the judge's model was called %d times on a window its evidence had filled", model.calls)
	}
	if d.Asked || d.Tokens() != 0 {
		t.Errorf("decision = %+v, want nothing asked and nothing to charge", d)
	}

	granted, rescued := extension.Consider(t.Context(), judge,
		extension.Policy{Enabled: true, RoundStep: 5, Ceiling: 40}, req)
	if granted != 0 || rescued.Extend || rescued.Asked ||
		rescued.Reason != extension.ReasonBudgetExhausted {
		t.Errorf("Consider = (%d, %+v), want a budget rescue that claims no call", granted, rescued)
	}
}

// A JUDGE WHOSE BUDGET HAS ROOM IS CALLED, so the case above is about the
// window and not about a hold that refuses everything.
func TestAJudgeWithRoomLeftIsCalled(t *testing.T) {
	t.Parallel()
	model := &answering{answer: "EXTEND 4\nprogressing"}
	judge := extension.NewLLMJudge(model, "k").WithHold(&fillingBudget{})
	if _, err := judge.Decide(t.Context(), judgeReq()); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if model.calls != 1 {
		t.Errorf("the model was called %d times, want once", model.calls)
	}
}

// THE CALL'S DEADLINE STARTS WHEN THE CALL DOES. JudgeTimeout bounds the one
// call; rendering the evidence can wait on rewrites first, each under its own
// deadline, and a timer started before them handed the call whatever they
// left — none at all after a slow rewrite.
func TestTheJudgesTimeoutStartsAfterItsEvidenceIsRendered(t *testing.T) {
	t.Parallel()
	req := judgeReq()
	req.Task = strings.Repeat("the quoted thread goes on. ", 400) + "THE ASK"
	fit := &fillingFitter{budget: &fillingBudget{}, took: 20 * time.Millisecond}
	model := &deadlineModel{}
	if _, err := extension.NewLLMJudge(model, "k").WithCompactor(fit).Decide(t.Context(), req); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if !model.had {
		t.Fatal("the judge's call carried no deadline")
	}
	if earliest := fit.returned.Add(extension.JudgeTimeout); model.deadline.Before(earliest) {
		t.Errorf("the call's deadline is %v before the rewrite returned plus JudgeTimeout: "+
			"the rewrite's time was taken out of the call's", earliest.Sub(model.deadline))
	}
}

// deadlineModel records the deadline its call was made under.
type deadlineModel struct {
	deadline time.Time
	had      bool
}

func (m *deadlineModel) Complete(ctx context.Context, _ llm.Request) (*llm.Completion, error) {
	m.deadline, m.had = ctx.Deadline()
	return &llm.Completion{Content: "RESCUE\nlooping"}, nil
}
