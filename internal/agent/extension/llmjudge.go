package extension

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/agent/ledger"
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
	// The answer is a verdict word, a small integer and one sentence. This
	// is deliberately several times that: a thinking model spends its cap
	// reasoning and returns nothing visible if the cap is tight, which
	// reads as a judge that refused rather than one that was cut off.
	JudgeMaxTokens = 400

	// judgeTemperature is zero because this is a classifier. The same
	// evidence must produce the same verdict — an extension that depended
	// on sampling would make a turn's cost non-reproducible, and a judge
	// that says extend on one run and rescue on the next teaches nobody
	// anything.
	judgeTemperature = 0.0

	// judgeCallsShown bounds the tool log in the prompt.
	//
	// The judge's question is "is this phase repeating itself?", which is
	// answered by the RECENT calls: a phase thrashing does it in its last
	// handful of rounds, and the early ones are the part that worked. The
	// whole log of a 20-round Execute would also be the largest thing in a
	// prompt whose point is being cheap.
	judgeCallsShown = 12

	// judgeArgsShown bounds one call's rendered arguments. Enough to tell
	// two calls to the same tool apart, which is the entire discrimination
	// the judge has to make, and not enough for one pasted document to
	// crowd out the rest of the log.
	judgeArgsShown = 200
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
}

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

// Decide asks the model and reads its verdict.
func (j *LLMJudge) Decide(ctx context.Context, req Request) (Decision, error) {
	if j == nil || j.model == nil {
		return Decision{}, ErrNoVerdict
	}
	call, cancel := context.WithTimeout(ctx, JudgeTimeout)
	defer cancel()

	completion, err := j.model.Complete(call, llm.Request{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: judgeSystemPrompt},
			{Role: llm.RoleUser, Content: renderJudgeRequest(req)},
		},
		// NO TOOLS: the answer is two lines of text, and a tool on the
		// surface invites a model to call it and answer nothing.
		Temperature: llm.Temp(judgeTemperature),
		MaxTokens:   JudgeMaxTokens,
	})
	if err != nil {
		return Decision{}, fmt.Errorf("extension: judge %s: %w", j.key, err)
	}
	if completion == nil {
		// A provider answering (nil, nil) is a contract violation, and
		// checked rather than dereferenced: the panic would surface as a
		// failed turn on the phase this was trying to be generous to.
		return Decision{}, fmt.Errorf("extension: judge %s returned nothing: %w",
			j.key, ErrNoVerdict)
	}
	decision, err := ParseVerdict(completion.Content)
	if err != nil {
		log.DebugContext(ctx, "extension_judge_unparsed", "model", j.key,
			"answer", truncate(completion.Content, 200),
			"output_tokens", completion.OutputTokens)
		return Decision{}, err
	}
	return decision, nil
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

// renderJudgeRequest turns the evidence into the user message.
func renderJudgeRequest(req Request) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Phase: %s\n", req.Phase)
	fmt.Fprintf(&b, "Rounds already used: %d\n", req.RoundsUsed)
	fmt.Fprintf(&b, "Most you may grant now: %d\n", req.MaxStep)
	fmt.Fprintf(&b, "Rounds left under the phase ceiling: %d\n", req.RemainingUnderCeiling)

	if task := strings.TrimSpace(req.Task); task != "" {
		b.WriteString("\n## Task\n")
		b.WriteString(truncate(task, 1500))
		b.WriteString("\n")
	}
	if plan := strings.TrimSpace(req.PlanSummary); plan != "" {
		b.WriteString("\n## Plan\n")
		b.WriteString(truncate(plan, 1000))
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
		b.WriteString("\n")
		for i, c := range shown {
			b.WriteString(renderJudgeCall(i+1, c))
		}
	}

	if last := strings.TrimSpace(req.LastText); last != "" {
		b.WriteString("\n## What it last said\n")
		b.WriteString(truncate(last, 800))
		b.WriteString("\n")
	}
	b.WriteString("\nVerdict:")
	return b.String()
}

// renderJudgeCall renders one call as a line the judge can compare against
// its neighbours. The arguments are the discrimination — a log of bare tool
// names cannot tell a loop from a sequence — so they are included and
// bounded rather than dropped.
func renderJudgeCall(n int, c ledger.Call) string {
	line := fmt.Sprintf("%d. %s(%s)", n, c.Name,
		truncate(collapseSpace(renderArgs(c.Args)), judgeArgsShown))
	if c.Failed {
		// The FAILURE is the signal, and the text after it is what tells
		// a second identical failure from a different one — which is the
		// difference between a phase retrying usefully and a phase stuck.
		line += " -> failed: " + truncate(collapseSpace(c.Result), 120)
	}
	return line + "\n"
}

// renderArgs renders a call's arguments in a stable order.
//
// SORTED, because Go map iteration is randomised and this text is the
// judge's only way to tell two calls apart: the same call rendered with its
// keys in a different order reads as a different call, which turns a loop
// into apparent progress at random.
func renderArgs(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, args[k]))
	}
	return strings.Join(parts, ", ")
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
	if tail := strings.TrimSpace(strings.Join(rest, " ")); tail != "" {
		return truncate(tail, 300)
	}
	for _, raw := range following {
		if line := strings.TrimSpace(strings.Trim(strings.TrimSpace(raw), "`*#>-")); line != "" {
			return truncate(line, 300)
		}
	}
	return ""
}

// collapseSpace folds whitespace so one call renders on one line: a pretty
// printed JSON argument would otherwise turn a twelve-line log into a page.
func collapseSpace(s string) string { return strings.Join(strings.Fields(s), " ") }

// truncate bounds a rendered field, marking where it was cut so the judge
// does not read a severed argument as a different one.
//
// NEVER THROUGH A RUNE. A plain s[:max] splits whatever multi-byte character
// straddles the boundary and yields invalid UTF-8, which a JSON encoder
// replaces with U+FFFD — so a task, a plan or a tool argument that is not
// ASCII reaches the judge garbled rather than merely short, and the judge's
// whole job is telling two renderings apart.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max] + "…"
}
