package extension

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/logging"
	llm "github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/textcut"
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

	// judgeTaskShown bounds the turn's ask in the prompt.
	//
	// THE ONE VALUE HERE THAT NOTHING ELSE BOUNDS. A task is whatever woke
	// the seat — a chat message, or a webhook body somebody pasted an
	// incident report into — so it is the only block that can arrive
	// arbitrarily large, and the judge runs on a cheap model whose context
	// is the smallest in the deployment. The HEAD is kept because an ask
	// leads with what is being asked.
	//
	// Four thousand bytes rather than the 1500 this was: 1500 cut an
	// ordinary issue body mid-sentence, and a judge ruling on half an ask
	// reads the calls it cannot match to anything as drift — which biases
	// it toward rescue on long tasks, precisely the turns that legitimately
	// need more rounds. 4000 is [ledger.RenderedArtifactLimit]'s order,
	// carries any real trigger whole, and is still a fraction of the
	// smallest context the shipped models offer.
	judgeTaskShown = 4000

	// judgeLastTextShown bounds the phase's own last words.
	//
	// A BOUND IS EARNED HERE because [toolloop.Result.Text] is the AGGREGATE
	// — every assistant message of the loop concatenated, thinking included
	// — so its size is the round cap times the phase's max_tokens rather
	// than one message.
	//
	// THE TAIL, through [ledger.ElideTail], which is the whole point of the
	// block: it is headed "What it last said" and a head cut rendered what
	// the phase said FIRST, so the sentence that most distinguishes a phase
	// about to finish from one thrashing was the one systematically
	// dropped. 2000 runes holds the closing exchange.
	judgeLastTextShown = 2000
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
		// Asked, with no model and no tokens: the call was made and the
		// provider never answered, which is a different fact from the
		// policy declining to ask at all.
		return Decision{Asked: true}, fmt.Errorf("extension: judge %s: %w", j.key, err)
	}
	if completion == nil {
		// A provider answering (nil, nil) is a contract violation, and
		// checked rather than dereferenced: the panic would surface as a
		// failed turn on the phase this was trying to be generous to.
		return Decision{Asked: true}, fmt.Errorf("extension: judge %s returned nothing: %w",
			j.key, ErrNoVerdict)
	}
	// The spend, on every path out of here: the call happened whatever the
	// answer was, and the caller is the only frame that can charge it.
	spent := Decision{
		Asked:        true,
		Model:        completion.Model,
		InputTokens:  completion.InputTokens,
		OutputTokens: completion.OutputTokens,
	}
	decision, err := ParseVerdict(completion.Content)
	if err != nil {
		// THE WHOLE ANSWER, because the question this line exists to
		// answer is "did the verdict appear LATER, after prose?" —
		// [ParseVerdict] refuses to search past the first non-empty line,
		// and [JudgeMaxTokens]' own note says a thinking model spends its
		// cap reasoning. A 200-byte head cut can never show that, so the
		// bound removed the root cause it was written to record. It is not
		// an unbounded log line either way: the provider was capped at
		// JudgeMaxTokens, so this is bounded by construction.
		log.DebugContext(ctx, "extension_judge_unparsed", "model", j.key,
			"answer", completion.Content,
			"output_tokens", completion.OutputTokens)
		return spent, err
	}
	decision.Asked, decision.Model = spent.Asked, spent.Model
	decision.InputTokens, decision.OutputTokens = spent.InputTokens, spent.OutputTokens
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
		b.WriteString(textcut.Ellipsis(task, judgeTaskShown))
		b.WriteString("\n")
	}
	if plan := strings.TrimSpace(req.PlanSummary); plan != "" {
		// WHOLE. This is the executor's own intent line, which every other
		// reader in the engine renders whole — the later rounds, the
		// reviewer, the conversation ledger — and it is the STANDARD the
		// judge measures the tool log against. Cutting it hid the back half
		// of a multi-step plan, which is the half a phase that has run out
		// of rounds is still working on, so the judge saw calls it could
		// not match to anything and read them as drift.
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
		b.WriteString("\n")
		for i, c := range shown {
			b.WriteString(renderJudgeCall(i+1, c))
		}
	}

	if last := strings.TrimSpace(req.LastText); last != "" {
		b.WriteString("\n## What it last said\n")
		b.WriteString(ledger.ElideTail(last, judgeLastTextShown))
		b.WriteString("\n")
	}
	b.WriteString("\nVerdict:")
	return b.String()
}

// renderJudgeCall renders one call as a line the judge can compare against
// its neighbours. The arguments are the discrimination — a log of bare tool
// names cannot tell a loop from a sequence — so they are included and
// bounded rather than dropped.
//
// THE BUDGET IS THE LEDGER'S, not one of this package's own, and that is the
// whole of what changed here. This rendered the sorted keys into one string
// and head-cut the blob at 200 bytes, which is the failure
// [ledger.RenderArgs] exists to prevent and whose doc names it: a head cut on
// a serialised object drops whichever keys SORT LAST, and the discriminating
// argument — channel, key, page_id — is usually the shortest one. A call
// carrying a long `body` and a short `url` therefore rendered with the url
// gone, so two calls to different pages became byte-identical and the judge
// read a phase that was working as one looping. The ledger elides per VALUE
// and drops whole KEYS shortest-first with a "+N more", which keeps the
// identifiers and bounds the line, and it is now the only implementation of
// that rule in the tree.
func renderJudgeCall(n int, c ledger.Call) string {
	line := fmt.Sprintf("%d. %s(%s)", n, c.Name,
		collapseSpace(ledger.RenderArgs(c.Args, judgeArgs)))
	if c.Failed {
		// The FAILURE is the signal, and the text after it is what tells
		// a second identical failure from a different one — which is the
		// difference between a phase retrying usefully and a phase stuck.
		//
		// [ledger.ValueLimit] rather than the 120 this carried, for the
		// same one-implementation reason: the ledger bounds this exact
		// value — a tool's own output on a failure — one package over, and
		// its doc is where the justification lives. 120 cut an ordinary
		// wrapped tool error mid-reason, so "HTTP 422: Validation Failed:
		// body is too long (maximum is 65536)" reached the judge without
		// the part that says what to do, and two different failures
		// rendered the same.
		line += " -> failed: " + ledger.Elide(collapseSpace(c.Result), ledger.ValueLimit)
	}
	return line + "\n"
}

// judgeArgs is the ledger's per-value budget, with its read cap OFF.
//
// Skip and Reads are empty and MaxReadCalls is zero on purpose: those three
// shape what a LEDGER carries — a record of deliveries, where a read is noise
// — and the judge's question is the opposite one. It has to see every call
// the loop made, reads most of all, because a phase re-reading the same page
// twenty times is exactly the thrashing it is asked to detect. Only the two
// size budgets carry over.
var judgeArgs = ledger.FormatOptions{
	ValueLimit: ledger.ValueLimit,
	BlobLimit:  ledger.BlobLimit,
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
//
// WHOLE. It used to be cut at 300 bytes on both paths, which was a second
// bound on a quantity [JudgeMaxTokens] had already capped one function
// earlier — the answer cannot be longer than the completion the provider was
// allowed to write. And it is the one string here that reaches a model, a
// dashboard and a log at once: it rides the nudge back into the phase, it is
// what a screen shows beside "extended by 8 rounds", and it is what an
// operator asking why a phase never gets extended reads. A sentence cut at
// 300 bytes is the worst of those three.
func judgeReason(rest []string, countConsumed bool, following []string) string {
	if countConsumed && len(rest) > 0 {
		rest = rest[1:]
	}
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

// collapseSpace folds whitespace so one call renders on one line: a pretty
// printed JSON argument would otherwise turn a twelve-line log into a page.
func collapseSpace(s string) string { return strings.Join(strings.Fields(s), " ") }
