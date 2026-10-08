package extension

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerfit"
	"github.com/crewlet/crewlet/internal/compact"
	"github.com/crewlet/crewlet/internal/logging"
	llm "github.com/crewlet/crewlet/internal/providers/llm"
)

var log = logging.Get("agent.extension")

// The model-facing half, and the one this package was named for.
//
// [Judge] had no implementation anywhere in the tree, so [Consider] rescued
// with "no_judge" on every exhaustion and the whole extension mechanism —
// the ceilings, the step size, the enable switch, the llm_judge model role —
// was inert. Everything below it was written and tested; nothing above it
// existed.

const (
	// JudgeTimeout bounds the one call.
	//
	// Short on purpose. This runs on a phase that has ALREADY run out of
	// rounds, with a person or a webhook waiting on the turn behind it, and
	// its entire job is to decide whether to be generous. A judge that
	// takes longer than a round would have taken has cost more than it can
	// save, and the rescue path it falls back to is a real outcome rather
	// than a failure. Sized against the auxiliary passes, which ask a
	// cheap model a similarly small question.
	JudgeTimeout = 30 * time.Second

	// JudgeMaxTokens caps the answer.
	//
	// The answer is a verdict word, a small integer and one sentence, and
	// this is several times that. It applies only to a call that does not
	// think: a thinking model spends its thinking from the same cap, so a
	// tight one returns nothing visible — a judge that reads as refusing
	// rather than as cut off — and a backend sends the model's own ceiling
	// there instead (see llm.Request.MaxTokens). What keeps a thinking
	// judge short is judgeEffort.
	JudgeMaxTokens = 400

	// judgeTemperature is zero because this is a classifier. The same
	// evidence must produce the same verdict — an extension that depended
	// on sampling would make a turn's cost non-reproducible, and a judge
	// that says extend on one run and rescue on the next teaches nobody
	// anything. Honoured where the model takes a sampling parameter and the
	// call is not thinking; the current Claude generation takes none, and
	// there the two-word verdict at judgeEffort is what keeps it stable.
	judgeTemperature = 0.0

	// judgeEffort is the most thinking a verdict is worth. The judge
	// classifies a round log into one of two words, and a thinking model
	// at its entry's level spends JudgeMaxTokens reasoning and returns no
	// verdict at all — which the policy reads as a refusal.
	judgeEffort = llm.EffortLow

	// judgeCallsShown bounds the tool log in the prompt, and it is a
	// window, never a cut: the line says how many calls it shows of how
	// many.
	//
	// The judge's question is "is this phase repeating itself?", which is
	// answered by the RECENT calls: a phase thrashing does it in its last
	// handful of rounds, and the early ones are the part that worked. The
	// whole log of a 20-round Execute would also be the largest thing in a
	// prompt whose point is being cheap.
	judgeCallsShown = 12

	// JudgeTaskBudget is the size the task is REWRITTEN to past it — never
	// cut. Eight kibibytes: the judge is a cheap model asked a small
	// question, and the task is context for the log, which is the
	// evidence. It used to be cut at 1500 bytes from the HEAD, so a task
	// whose ask came after its quoted thread was judged without its ask.
	JudgeTaskBudget = 8 << 10

	// JudgeTextBudget is the size the phase's own last words are rewritten
	// to past it. Four kibibytes, for the task's reason. They used to be cut
	// at 800 bytes from the HEAD — so "what it last said" was what the
	// phase said FIRST.
	JudgeTextBudget = 4 << 10
)

// ErrNoVerdict reports an answer the judge could not read as a decision.
//
// An error rather than a rescue built here, because [Consider] already turns
// an error into a rescue and carries the reason — and because the two are
// genuinely different facts. "The model said stop" and "the model said
// something I could not parse" must not look the same in a log line when
// somebody is working out why a phase never gets extended.
var ErrNoVerdict = errors.New("extension: the judge gave no verdict")

// Completer is the one thing an [LLMJudge] needs, declared here rather than
// taken as an llm.Provider so this package depends on the single method it
// calls. A chain, a single provider and a fake all satisfy it.
type Completer interface {
	Complete(ctx context.Context, req llm.Request) (*llm.Completion, error)
}

// LLMJudge decides round-cap extensions by asking a cheap model.
//
// It answers ONE question — is this phase progressing or repeating itself —
// and deliberately nothing else. How many rounds that answer is worth is
// [Policy]'s arithmetic, which is why AdditionalRounds is advisory and why
// this type does no clamping: the judge asking for a hundred rounds and the
// judge asking for none are the same input to [Policy.Grant].
type LLMJudge struct {
	model Completer

	// key names the model entry, for the log line. A judge that rescued
	// every turn because one provider key was misconfigured is otherwise
	// indistinguishable from a phase that genuinely deserved no extension.
	key string

	// fit rewrites evidence past its budget — see [LLMJudge.WithCompactor].
	fit Fitter

	// hold is the turn's budget, asked once the evidence is rendered — see
	// [LLMJudge.WithHold].
	hold Hold
}

// Fitter rewrites a text that will not fit its budget: the seat's compactor.
type Fitter interface {
	Fit(ctx context.Context, kind compact.Kind, text string, budget int) (compact.Result, error)
}

// Hold is the turn's budget as the judge asks it: the refusal its call is
// certain to meet, or nil. Asked immediately before the judge's call, on its
// context, because an error is the gate turning that call away and the turn's
// meter records it as it records a refused round.
type Hold interface {
	Held(ctx context.Context) error
}

// ErrHeld is a judge that did not call its model because the turn's budget
// already refuses the call: it would be billed, and its charge refused. Not a
// failed judge — [Consider] rescues it as the budget it is.
var ErrHeld = errors.New("extension: the turn's budget refuses the judge's call")

// NewLLMJudge builds a judge over one model. A nil model yields a nil judge,
// which [Consider] already handles as "no judge" — the alternative, a judge
// that errors on every call, would report a failure on a company that simply
// has no model to ask.
func NewLLMJudge(model Completer, key string) *LLMJudge {
	if model == nil {
		return nil
	}
	return &LLMJudge{model: model, key: key}
}

// WithCompactor makes the judge rewrite evidence past its budget — a pasted
// document in a call's arguments, a whole error page, a long task — with the
// seat's auxiliary model rather than show it whole. Without one, the task and
// the phase's words are shown whole and an over-budget argument by its size
// and digest, which still tells two identical calls from two different ones.
// Nil-safe, so the engine can chain it on a judge that does not exist.
func (j *LLMJudge) WithCompactor(fit Fitter) *LLMJudge {
	if j != nil {
		j.fit = fit
	}
	return j
}

// WithHold makes the judge ask the turn's budget before it calls its model,
// and call nothing on a refusal ([ErrHeld]). Nil-safe, like WithCompactor.
//
// ASKED AFTER THE EVIDENCE IS RENDERED, never only before the judge is
// consulted: rendering can rewrite the task, the phase's last words or a
// call's arguments with the seat's auxiliary model, and each rewrite is
// charged to the same counters the judge's call is. A rewrite that fills a
// window leaves the judge's call certain to be refused, and asked only
// beforehand, that call was made, billed, and then refused.
func (j *LLMJudge) WithHold(hold Hold) *LLMJudge {
	if j != nil {
		j.hold = hold
	}
	return j
}

// Decide asks the model and reads its verdict.
func (j *LLMJudge) Decide(ctx context.Context, req Request) (Decision, error) {
	if j == nil || j.model == nil {
		return Decision{}, ErrNoVerdict
	}
	evidence := j.render(ctx, req)
	if j.hold != nil {
		if held := j.hold.Held(ctx); held != nil {
			// NOT ASKED: no call was made, so there is nothing to report
			// or to charge — see [WithHold].
			return Decision{}, fmt.Errorf("%w: %w", ErrHeld, held)
		}
	}
	// THE CALL'S OWN CLOCK, started once the evidence is in hand: rendering
	// can wait on rewrites, each under its own deadline, and a timer started
	// before it handed the one call [JudgeTimeout] bounds whatever the
	// rewrites left of it — none at all after a slow one.
	call, cancel := context.WithTimeout(ctx, JudgeTimeout)
	defer cancel()

	completion, err := j.model.Complete(call, llm.Request{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: judgeSystemPrompt},
			{Role: llm.RoleUser, Content: evidence},
		},
		// NO TOOLS: the answer is two lines of text, and a tool on the
		// surface invites a model to call it and answer nothing.
		Temperature: llm.Temp(judgeTemperature),
		Effort:      judgeEffort,
		MaxTokens:   JudgeMaxTokens,
	})
	// The spend, on every path out of here: the call happened whatever the
	// answer was, and the caller is the only frame that can charge it. A
	// REFUSED judgement included — it arrives as an error with no
	// completion, and its prompt was billed.
	billed := llm.Billed(completion, err)
	spent := Decision{Asked: true}
	if billed != nil {
		spent = Decision{
			Asked: true,
			Model: billed.Model,
			// The completion's own entry when a chain named one, and the
			// key this judge was built over otherwise — a single backend
			// never knows the key it was configured under.
			ProviderKey:  cmp.Or(billed.ProviderKey, j.key),
			InputTokens:  billed.InputTokens,
			OutputTokens: billed.OutputTokens,
			CacheRead:    billed.CacheRead,
			CacheWrite:   billed.CacheWrite,
		}
	}
	if err != nil {
		// Asked, and the provider never answered — which is a different
		// fact from the policy declining to ask at all. What it billed, if
		// anything, travels with the failure.
		return spent, fmt.Errorf("extension: judge %s: %w", j.key, err)
	}
	if completion == nil {
		// A provider answering (nil, nil) is a contract violation, and
		// checked rather than dereferenced: the panic would surface as a
		// failed turn on the phase this was trying to be generous to.
		return spent, fmt.Errorf("extension: judge %s returned nothing: %w",
			j.key, ErrNoVerdict)
	}
	decision, err := ParseVerdict(completion.Content)
	if err != nil {
		// WHOLE: the answer is bounded by [JudgeMaxTokens], and the part
		// of an unparseable answer worth reading is rarely its opening.
		log.DebugContext(ctx, "extension_judge_unparsed", "model", j.key,
			"answer", completion.Content, "output_tokens", completion.OutputTokens)
		return spent, err
	}
	return decision.withSpend(spent), nil
}

// judgeSystemPrompt is the whole of the judge's instructions.
//
// It states the answer format first and the question second, because the
// format is the part a smaller model drops. The examples are two lines rather
// than prose for the same reason.
const judgeSystemPrompt = `You judge whether an AI agent's tool-calling phase should be given more rounds.

You are shown the task, the plan, and the phase's tool-call log. Decide ONE thing:
is the phase making PROGRESS, or is it REPEATING itself?

Progress looks like: each call advances on the last, arguments change meaningfully,
the log is converging on the task. Repetition looks like: the same tool called with
the same or near-identical arguments, alternating between two calls, or retrying
something that has already failed the same way more than twice.

Answer in exactly this shape and nothing else:

  EXTEND <rounds>
  <one short sentence of evidence>

or

  RESCUE
  <one short sentence of evidence>

<rounds> is how many additional tool-call rounds you believe finishes the work.
Never exceed the maximum you are told. When in doubt, answer RESCUE: more rounds
cost real money and a phase that is looping will loop in them too.`

// render turns the evidence into the user message.
//
// NOTHING IN IT IS CUT. The plan and the counters are small by construction;
// the task, the phase's last words, a call's argument values and a failed
// call's error are rewritten past their budgets by the seat's auxiliary model
// (or, with none, shown whole — and an argument by its size and digest). The
// calls render exactly as the prior-work ledger renders them, so a payload
// the ledger has already condensed this turn is answered from the cache.
func (j *LLMJudge) render(ctx context.Context, req Request) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Phase: %s\n", req.Phase)
	fmt.Fprintf(&b, "Rounds already used: %d\n", req.RoundsUsed)
	fmt.Fprintf(&b, "Most you may grant now: %d\n", req.MaxStep)
	fmt.Fprintf(&b, "Rounds left under the phase ceiling: %d\n", req.RemainingUnderCeiling)

	if task := strings.TrimSpace(req.Task); task != "" {
		b.WriteString("\n## Task\n")
		b.WriteString(j.fitted(ctx, compact.KindTask, task, JudgeTaskBudget))
		b.WriteString("\n")
	}
	if plan := strings.TrimSpace(req.PlanSummary); plan != "" {
		b.WriteString("\n## Plan\n")
		b.WriteString(plan)
		b.WriteString("\n")
	}

	b.WriteString("\n## Tool calls so far")
	if len(req.Calls) == 0 {
		// A phase that exhausted its rounds without calling anything is
		// the clearest rescue there is, and saying so beats an empty
		// heading the model has to interpret.
		b.WriteString("\n(none — the phase used its rounds without calling a tool)\n")
	} else {
		shown := req.Calls
		if len(shown) > judgeCallsShown {
			fmt.Fprintf(&b, " (last %d of %d)", judgeCallsShown, len(shown))
			shown = shown[len(shown)-judgeCallsShown:]
		}
		// THE LEDGER'S OWN RENDERING, with its argument budget: one line
		// per call, its arguments as sorted JSON — so the same call always
		// renders the same way and a loop cannot read as progress — and
		// every payload past the budget fitted rather than cut.
		opts := ledger.FormatOptions{ValueLimit: ledger.ValueLimit}
		opts.Fitted = ledgerfit.Fit(ctx, j.fitter(), ledger.CallPieces(shown, opts))
		b.WriteString("\n")
		b.WriteString(ledger.FormatCalls(shown, opts))
		b.WriteString("\n")
	}

	if last := strings.TrimSpace(req.LastText); last != "" {
		b.WriteString("\n## What it last said\n")
		b.WriteString(j.fitted(ctx, compact.KindProduced, last, JudgeTextBudget))
		b.WriteString("\n")
	}
	b.WriteString("\nVerdict:")
	return b.String()
}

// fitted is text within budget: whole if it already fits, the seat's
// rewrite, announced as one, if it does not — and whole again if no rewrite
// can be had, because the judge cannot weigh a phase against a task it was
// shown none of, and the rescue its failure falls back to is safe.
func (j *LLMJudge) fitted(ctx context.Context, kind compact.Kind, text string, budget int) string {
	if len(text) <= budget || j.fit == nil {
		return text
	}
	res, err := j.fit.Fit(ctx, kind, text, budget)
	if err != nil {
		log.DebugContext(ctx, "extension_judge_evidence_whole", "model", j.key,
			"kind", string(kind), "bytes", len(text), "error", err.Error())
		return text
	}
	return res.Note() + "\n" + res.Text
}

// fitter is the judge's compactor as ledgerfit takes one — a nil interface,
// not a typed nil, when the judge has none.
func (j *LLMJudge) fitter() ledgerfit.Fitter {
	if j.fit == nil {
		return nil
	}
	return j.fit
}

// ParseVerdict reads a judge's answer.
//
// Exported because it is the contract between the prompt above and every
// model that has to satisfy it: the cases it accepts are the cases the prompt
// may be reworded within, and a change to one without the other is how a
// judge starts silently rescuing everything.
//
// Lenient about SHAPE and strict about MEANING. A model that adds a code
// fence, a bullet or a "Verdict:" prefix still meant its verdict; a model
// that answered something else entirely did not, and guessing on its behalf
// is how a phase gets granted rounds nobody decided to give it.
func ParseVerdict(answer string) (Decision, error) {
	lines := strings.Split(strings.TrimSpace(answer), "\n")
	for i, raw := range lines {
		line := strings.TrimSpace(strings.Trim(strings.TrimSpace(raw), "`*#>-"))
		line = strings.TrimSpace(strings.TrimPrefix(line, "Verdict:"))
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		verdict := strings.ToUpper(strings.Trim(fields[0], ":.,"))
		switch verdict {
		case "EXTEND":
			rounds, counted := judgeRounds(fields[1:])
			return Decision{
				Extend:           true,
				Reason:           judgeReason(fields[1:], counted, lines[i+1:]),
				AdditionalRounds: rounds,
			}, nil
		case "RESCUE":
			// No count to consume: a rescue grants nothing, so every
			// word after the verdict is the evidence.
			return Decision{Reason: judgeReason(fields[1:], false, lines[i+1:])}, nil
		}
		// The first non-empty line was neither verdict. Later lines are
		// not searched: a model that explained itself first and decided
		// afterwards has not followed the format, and the word EXTEND
		// appears in prose that concludes the opposite.
		break
	}
	return Decision{}, ErrNoVerdict
}

// judgeRounds reads the count off the verdict line, reporting whether the
// first word was one.
//
// The second return is what keeps [judgeReason] honest: a model that wrote
// "EXTEND a few more" put its reason where the count goes, and skipping that
// word unconditionally would report the reason as "few more".
func judgeRounds(rest []string) (n int, counted bool) {
	if len(rest) == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(strings.Trim(rest[0], ":.,"))
	if err != nil || n < 0 {
		// Advisory anyway: [Policy.Grant] gives a judge that chose
		// extend without a usable number the step, which is the right
		// answer for a model that wrote "EXTEND a few more".
		return 0, false
	}
	return n, true
}

// judgeReason is the evidence, from whatever the model put after the verdict:
// the rest of the verdict line if there is any, and otherwise the next
// non-empty line.
func judgeReason(rest []string, countConsumed bool, following []string) string {
	if countConsumed && len(rest) > 0 {
		rest = rest[1:]
	}
	// WHOLE: the answer is bounded by [JudgeMaxTokens], and the reason is
	// what the extended phase is handed as its nudge — a reason cut
	// mid-sentence is an instruction cut mid-sentence.
	if tail := strings.TrimSpace(strings.Join(rest, " ")); tail != "" {
		return tail
	}
	for _, raw := range following {
		if line := strings.TrimSpace(strings.Trim(strings.TrimSpace(raw), "`*#>-")); line != "" {
			return line
		}
	}
	return ""
}
