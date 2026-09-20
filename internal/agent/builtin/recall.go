package builtin

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/tools"
)

// RecallIterationTool is the wire name.
const RecallIterationTool = "recall_iteration"

// recallIteration is the surface the iteration ledger's budgets assume exists.
//
// # Why it exists
//
// [internal/agent/ledger] bounds what a closed round puts in front of the next
// one — argument values past [ledger.ValueLimit], whole arguments past
// [ledger.BlobLimit], read calls past [ledger.MaxReadCalls], produced text
// past [ledger.RenderedArtifactLimit] — because that block is re-sent on every
// round of both phases, so its cost is the value times the round cap rather
// than the value. Those bounds are defensible only if what they cut is
// reachable another way. It was not, and budgets.go said so itself, in the
// sentence naming the consequence: "the ledger is not re-readable from
// anywhere: unlike a chat message or an issue comment, there is no surface to
// go back to." So a round-five executor could read that it had called
// post_message and have no way to learn what it had actually sent — which is
// the exact state a turn is in when it posts the same thing twice.
//
// This is that surface, and it is a READ rather than a bigger budget because
// the record already held everything: [ledger.Call.Args] is the model's own
// map, kept whole, and every cut in that package happens at RENDER time. What
// was missing was a verb. With one, the block stays skimmable AND the whole
// value is one call away, which no single budget can be.
//
// # What it deliberately does not return
//
// A SUCCEEDED call's result, which the record also holds. That is the ledger
// package's own rule rather than a second opinion about it — "Tool results are
// deliberately not carried across rounds or turns, so a read the next round
// needs must be re-run" — and the reason survives being offered a pull instead
// of a push: a read's answer may have MOVED since, so replaying a stale copy
// is worse than re-reading it, and a write's result is a receipt for something
// the ledger already records as done.
//
// An ARGUMENT is the opposite kind of fact and therefore on this side of the
// line: it is fixed, it is spent, and no re-run recovers it. A FAILED call's
// error text is too, for the same reason read one way round — running the call
// again is not a way to learn what it said last time, because it may succeed,
// or fail differently.
//
// # No cap on the answer, and it needs none
//
// A round can be large — [ledger.Iteration.Text] is every assistant message of
// that phase's tool loop concatenated, thinking included — so the question a
// reader asks here is why this does not bound what it returns. Two reasons,
// and the first is the general one: no tool in this engine cuts its result.
// Evidence a turn reasons over is passed whole — internal/textcut's package
// doc states the rule and what it cost when it was not — so a bound HERE would
// be unique to the one tool whose entire purpose is to be the place a cut is
// undone.
//
// The second is specific and stronger: everything this returns ALREADY FIT in
// one conversation. The round's text is what that phase's own model wrote
// across its tool rounds and the arguments are what it sent, so the phase held
// all of it at once and the provider accepted it. A later phase re-reading one
// round is bounded by that, and `tool` narrows further for a caller that wants
// one call out of a busy round.
//
// # This turn's rounds, and no others
//
// The rounds arrive on [turnctx.Turn], derived per phase by the runner from
// the history the turn loop already hands every phase. There is no store read
// here and no way to name another turn: a verb that could read another
// delivery's arguments would be a new read surface over the event store, which
// is a different decision from this one.
//
// # The executor, and not the reviewer
//
// The reviewer reads the same elided block, and its surface deliberately
// carries its submission and nothing else — which is what makes forcing a tool
// call there the same instruction as "submit the review", and the reviewer's
// phase in internal/agent/runner says so where it sets ToolChoiceRequired.
// Widening it would trade that invariant for an advisory signal, because the
// duplicate-delivery question the reviewer reads the block for is decided by
// the engine's own record of what ran rather than by the reviewer's reading
// of it.
type recallIteration struct{}

var _ tools.SeatCallable = (*recallIteration)(nil)

func (t *recallIteration) Name() string { return RecallIterationTool }

// THE BLOCK IS NAMED, AND ITS HEADING IS THE NAME. A description saying "your
// prompt summarises this somewhere" leaves the model looking; the model is
// reading one document and the heading is how it finds the part being talked
// about. Written out rather than composed from [prompts.PriorWorkHeader],
// because that constant is a whole paragraph of instruction and this package
// would import the prompt builder to use six words of it — so
// recall_guard_test.go holds the two against each other instead, which is the
// same arrangement use_skill needed and did not have.
func (t *recallIteration) Description() string {
	return "Re-read one of your EARLIER rounds this turn, in full. The " +
		"`Already done earlier in this turn` block summarises each round and " +
		"shortens long arguments, long output and long lists of reads; this " +
		"returns one round whole — every tool call with the exact arguments " +
		"you sent, and everything you produced. Reach for it before repeating " +
		"a delivery that block says you already made, when you need to know " +
		"what you actually said. Tool results are not kept between rounds: a " +
		"call marked (read) is meant to be run again."
}

func (t *recallIteration) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"iteration": map[string]any{
				"type": "integer",
				"description": "Which round, as numbered by the `Iteration N` " +
					"headings in your prompt. Defaults to the most recent " +
					"round that has closed",
			},
			"tool": map[string]any{
				"type": "string",
				"description": "Optional: return only that round's calls to " +
					"this tool, and nothing else about the round. Use it when " +
					"a round did a lot and you want one call out of it",
			},
		},
	}
}

func (t *recallIteration) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *recallIteration) CallForTurn(_ context.Context, turn *turnctx.Turn,
	args map[string]any,
) (tools.Result, error) {
	// REFUSED rather than answered "nothing yet". A nil turn means the
	// surface was never bound to one, which is a wiring fault, and the two
	// honest answers a reader needs to tell apart are "this turn has closed
	// no rounds" and "this tool was never told which turn it is in".
	if turn == nil {
		return failed(RecallIterationTool + " can only be called during a turn."), nil
	}
	rounds := turn.Rounds
	if len(rounds) == 0 {
		// NOT a failure: asking on round one is reasonable, and the model
		// cannot fix it by calling differently. What it can do is stop
		// looking for a record that does not exist yet.
		return tools.Result{Output: "No earlier round to re-read — this is the " +
			"first round of the turn, so everything that has happened is " +
			"already in front of you."}, nil
	}

	// Defaulted to the LAST record's own number, not to len(rounds): a
	// resumed turn carries the rounds it closed before the suspend, so the
	// two agree only for a turn that never parked — and the model reads
	// these numbers off the `Iteration N` headings either way.
	want := argInt(args, "iteration", rounds[len(rounds)-1].Iteration)
	rec, ok := roundNumbered(rounds, want)
	if !ok {
		return failed(fmt.Sprintf("There is no round %d in this turn. Closed so far: %s.",
			want, roundNumbers(rounds))), nil
	}

	if name := strings.TrimSpace(argString(args, "tool")); name != "" {
		return t.oneTool(rec, name), nil
	}
	return tools.Result{Output: renderRound(rec)}, nil
}

// oneTool answers a narrowed request: that round's calls to one tool.
//
// ONLY THE CALLS, because narrowing is the whole point of naming a tool. A
// round's produced text is every assistant message of that phase's tool loop
// concatenated, thinking included — megabytes on a long Execute (see
// [ledger.RenderedArtifactLimit], which exists to bound the RENDER of exactly
// that) — so folding it into a narrowed answer would hand back the wall the
// model was trying to avoid.
func (t *recallIteration) oneTool(rec ledger.Iteration, name string) tools.Result {
	kept := make([]ledger.Call, 0, len(rec.Calls))
	for _, c := range rec.Calls {
		if c.Name == name {
			kept = append(kept, c)
		}
	}
	if len(kept) == 0 {
		if len(rec.Calls) == 0 {
			return failed(fmt.Sprintf("Round %d made no tool calls at all.", rec.Iteration))
		}
		return failed(fmt.Sprintf("Round %d made no call to %s. It called: %s.",
			rec.Iteration, clip(name), strings.Join(calledNames(rec.Calls), ", ")))
	}
	var b strings.Builder
	// The MATCHED call's own name rather than the argument, which is a
	// model-supplied string: it matched exactly, so echoing the record's
	// copy is the one spelling that cannot carry a smuggled newline into a
	// line-structured answer.
	fmt.Fprintf(&b, "Round %d — its %d call(s) to %s, arguments exactly as you sent them:\n\n",
		rec.Iteration, len(kept), kept[0].Name)
	b.WriteString(renderWholeCalls(kept, rec.Reads))
	if hasRead(kept, rec.Reads) {
		// SAID HERE TOO, and this is the likelier of the two places to
		// need it: narrowing to one tool is what a model does when it
		// is looking for what that call came back with.
		b.WriteString("\n\nResults are not kept between rounds. A call marked " +
			"(read) is safe to run again; that is how you get its answer back.")
	}
	return tools.Result{Output: strings.TrimRight(b.String(), "\n")}
}

// renderRound prints one closed round with nothing elided.
//
// THE WHOLE ROUND, including the reviewer's two fields, which the prompt
// already carries verbatim. Not a redundancy worth trimming: the contract is
// "this round as it happened", and one that returned everything EXCEPT the
// parts some other block happens to hold is a contract nobody can state in a
// sentence — while the reviewer's prose is bounded, where the arguments and
// the produced text are exactly what is not.
func renderRound(rec ledger.Iteration) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Round %d of this turn, whole — arguments exactly as you sent them.\n",
		rec.Iteration)
	if rec.Intent != "" {
		fmt.Fprintf(&b, "\nSet out to: %s\n", rec.Intent)
	}
	b.WriteString("\nCalled:\n")
	b.WriteString(renderWholeCalls(rec.Calls, rec.Reads))
	b.WriteString("\n")
	if rec.Text != "" {
		fmt.Fprintf(&b, "\nProduced:\n%s\n", rec.Text)
	}
	if rec.CompletedWork != "" {
		fmt.Fprintf(&b, "\nReviewer, on what already landed: %s\n", rec.CompletedWork)
	}
	if rec.ReviewNotes != "" {
		fmt.Fprintf(&b, "\nReviewer's correction: %s\n", rec.ReviewNotes)
	}
	if hasRead(rec.Calls, rec.Reads) {
		// SAID HERE, because this is where a model is looking at a read it
		// made and wondering what came back. The answer is not in the
		// record by design, and pointing at the re-run is what stops the
		// next move being an invented one.
		b.WriteString("\nResults are not kept between rounds. A call marked " +
			"(read) is safe to run again; that is how you get its answer back.\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderWholeCalls prints calls with NOTHING elided.
//
// [ledger.FormatOptions]'s zero value is that package's own verbatim contract
// — no value limit, no blob limit, no read cap — so [ledger.RenderArgs] hands
// back the arguments exactly as the model sent them, which is the whole reason
// this tool exists. Numbered rather than bulleted, and deliberately: the
// prompt's own block uses bullets, and an answer that looked identical to it
// would read as a repeat of something the model had already been shown.
func renderWholeCalls(calls []ledger.Call, reads []string) string {
	if len(calls) == 0 {
		return "(no tool calls)"
	}
	lines := make([]string, 0, len(calls))
	for i, c := range calls {
		outcome := "success"
		if c.Failed {
			// WHOLE, where the ledger line elides it at ValueLimit: a
			// failed call's error text is the one output no re-run
			// recovers, because the call may succeed next time or fail
			// differently.
			outcome = "error: " + c.Result
		}
		line := fmt.Sprintf("%d. %s(%s) → %s", i+1, c.Name,
			ledger.RenderArgs(c.Args, ledger.FormatOptions{}), outcome)
		if slices.Contains(reads, c.Name) {
			line += " (read)"
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// roundNumbered finds the record a model named by its `Iteration N` heading.
//
// BY THE RECORD'S OWN NUMBER rather than by position: a resumed turn's history
// starts at whatever round it suspended on, so index N and round N are the
// same value only for a turn that never parked.
func roundNumbered(rounds []ledger.Iteration, want int) (ledger.Iteration, bool) {
	for _, rec := range rounds {
		if rec.Iteration == want {
			return rec, true
		}
	}
	return ledger.Iteration{}, false
}

// roundNumbers lists the rounds a refusal can offer instead.
//
// EVERY round, never a sample: this message exists so the model can retry with
// a number that works, and a shortened list answers a different question than
// the one it was asked — the same rule use_skill's suggest() follows.
func roundNumbers(rounds []ledger.Iteration) string {
	out := make([]string, 0, len(rounds))
	for _, rec := range rounds {
		out = append(out, strconv.Itoa(rec.Iteration))
	}
	return strings.Join(out, ", ")
}

// calledNames lists a round's tool names in call order, deduplicated.
func calledNames(calls []ledger.Call) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		if !slices.Contains(out, c.Name) {
			out = append(out, c.Name)
		}
	}
	return out
}

// hasRead reports whether any of these calls was positively annotated
// read-only, as the delivery gate resolved it for that round.
func hasRead(calls []ledger.Call, reads []string) bool {
	return slices.ContainsFunc(calls, func(c ledger.Call) bool {
		return slices.Contains(reads, c.Name)
	})
}
