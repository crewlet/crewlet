package ledger

import (
	"slices"
	"strings"
)

// Call is one tool invocation, as the ledger needs it.
//
// A ledger-owned type rather than the tool loop's Execution because the two
// answer different questions: the loop's record is live state a phase is still
// accumulating, this is a closed fact a later round reads. Keeping them apart
// is also what keeps the tool loop out of this package's imports — everything
// that HOLDS a ledger (the turn context, the prompt builder, the API layer)
// would otherwise drag the provider stack in behind it.
type Call struct {
	Name string

	// Args is the tool's arguments as the model supplied them. A map, not
	// the JSON string the wire carries, because elision works per VALUE and
	// re-parsing a string at render time would make every reader of a
	// persisted ledger repeat the parse — and disagree about malformed input.
	Args map[string]any

	// Result is the tool's output, or its error text when Failed.
	Result string

	// Failed marks a tool that reported failure. Recorded rather than
	// inferred from Result: a tool whose successful output happens to begin
	// "error:" is not a failure, and a phase that reads it as one loops
	// trying to fix something that worked.
	Failed bool

	// Digest is the call's identity as its run's call log admitted it — the
	// digest of its whole canonical arguments (internal/agent/turnctx's
	// ArgsDigest) — and empty where none was recorded: a call nothing
	// admitted, or one a build before it recorded.
	//
	// CARRIED BESIDE ARGS BECAUSE ARGS MAY NOT BE WHOLE. An executor run's
	// calls are recorded in a log bounded in bytes, beside its row, which keeps
	// a value past its share as a marker (internal/kept), and a parked turn's
	// closed rounds are condensed the same way to fit theirs; a later segment
	// that seeds its call log from the arguments it holds would take the call
	// for another, and derive another operation's id. Seeded by this, it
	// cannot.
	Digest string `json:"Digest,omitempty"`

	// Withheld is a call whose arguments, and with them their digest, every
	// record withholds: a call held for a person's approval and the one they
	// approved, whose arguments ride the run's row sealed (ADR-0077). Args
	// and Digest are both empty on it, and a later segment's call log counts
	// it as a call of its tool whose arguments are not known — never as the
	// tool called with none, which it was not.
	Withheld bool `json:"Withheld,omitempty"`

	// Unfinished is a call whose OUTCOME IS NOT KNOWN: an executor run's log
	// held it in flight when its round's calls were read — begun, its identity
	// on the record, and not finished as its collection stopped waiting for it:
	// still running past its bound or its turn's cap, its process gone, or its
	// update refused by the store (internal/sandbox's BridgeLog.InFlight). It
	// is Failed, so no reader takes it for a delivery or a submission, and its
	// Result says its effects may stand and are to be checked before it is
	// made again, never that it failed; Args are empty, an entry keeping none,
	// and its line says no record keeps them ([FormatCalls]).
	// A later segment's call log counts it by Digest, or, where it has none —
	// a tool that may ask a person first — as a call of its tool whose
	// arguments are not known, never as the tool called with none.
	Unfinished bool `json:"Unfinished,omitempty"`

	// ArgsUnread is a call whose arguments text could not be read: the record
	// holds text that does not read back (internal/tools' ReadArgs answers
	// nil), or the bridge could not parse what the model wrote and answered
	// the call unrun (internal/sandbox's BridgeCall.ArgsUnparsed). Args are
	// nil, as on a call made with none, so this says which it is. Every
	// reader that compares or counts calls by arguments reads it as a call of
	// its tool with unknown arguments, never as the tool called with none
	// ([FormatCalls] says so in its line).
	//
	// ADDITIVE ON THE WIRE: an older build drops the key and counts the call
	// as one with no arguments.
	ArgsUnread bool `json:"ArgsUnread,omitempty"`

	// Late is a call of an earlier run of the work: an executor run's log took
	// it after the job that made it was replaced (internal/sandbox's
	// BridgeCall.Late). The round's record carries it, so the round's reviewer
	// reads the call's line ending in words that say an earlier run made it
	// (internal/agent/prompts' ReviewLateCall, a [FormatOptions.Notes] note; no
	// line this package draws marks it), while the submission's citations and
	// every delivery check count it as the turn's, as they count a closed
	// round's: what one of them refuses another must not accept
	// (internal/agent/turn's Record). ADDITIVE ON THE WIRE, as ArgsUnread is:
	// a build before it drops the key and reads the call unmarked.
	Late bool `json:"Late,omitempty"`
}

// FormatOptions controls how a run of calls renders.
//
// The zero value is the VERBATIM contract Review's single-iteration evidence
// log depends on: nothing fitted, no read cap, nothing dropped. The
// cross-round ledger opts into the budgets explicitly.
type FormatOptions struct {
	// Skip names never render. Meta-tools (activate_tool, list_..._tools)
	// are never a delivery, so in a record whose only job is "what already
	// happened that matters" they are pure noise.
	Skip []string

	// Reads are the tools POSITIVELY annotated read-only, as the delivery
	// gate resolved them. Positively is the operative word — see phase.Delivery.
	Reads []string

	// ValueLimit is the budget past which an argument value or a failed
	// call's error is a [Piece] the caller fits; 0 renders every one whole.
	ValueLimit int

	// MaxReadCalls caps the READ lines one render shows: [FormatCalls]'s
	// list, or every group of [FormatCallGroups] together; 0 renders every
	// one.
	MaxReadCalls int

	// Fitted is the caller's rewrite of each piece [CallPieces] named.
	// A piece missing from it renders whole.
	Fitted Fitted

	// Answers ends a successful write's line with what it returned, fitted
	// past ValueLimit as an error is ([PieceAnswer]): the lists a run that
	// starts a session of its own is told its round made, which it remembers
	// none of. A read's answer is never shown, since a read can be run again.
	Answers bool

	// OneLine draws every line break in a tool's name or its error as a
	// space ([CallLine]): a brief's lists and an item's call, where a line a
	// tool's text starts could read as an entry of its own. Off, they are
	// drawn as the tool handed them, as Review's evidence log reads them.
	OneLine bool

	// Notes is words a call's line ends with, per group of a
	// [FormatCallGroups] render and per call BY ITS PLACE IN THE GROUP: ""
	// or a place past the end for none, so nil renders every line as it was.
	// Rendered as given: never a piece, fitted or counted against a budget.
	// A call the render leaves out prints no note.
	//
	// BY PLACE, NEVER BY SHOWN POSITION: skipped names and reads past the cap
	// are left out from anywhere in a group, so the k-th line printed is not
	// the group's k-th call. [ShownIndices] is the walk by place.
	Notes [][]string
}

// Format is the budgeted form the cross-round ledger uses, and the lists of
// the calls a run that continues a session, kept or in its job's own box, or
// starts one of its own carrying a round on, is told its round made
// (internal/agent/runner's calledAfter): those after the session's save, or
// every call of the round for a run of its own, which [FormatElided] follows
// there as it does a round here; the late ones (those that finished only after
// their run stopped waiting for them, those nobody waited for, and those still
// in flight); the reads an earlier brief to the session counted without
// listing them; and the calls the session holds that a told call could be
// taken for, listed again, rendered together under one read cap
// ([FormatCallGroups]).
// The caller fits the pieces it names and sets [FormatOptions.Fitted].
func Format(skip, reads []string) FormatOptions {
	return FormatOptions{
		Skip: skip, Reads: reads,
		ValueLimit: ValueLimit, MaxReadCalls: MaxReadCalls, OneLine: true,
	}
}

// visible is the calls a render of one list shows: everything but the skipped
// names, and the reads past MaxReadCalls.
func visible(calls []Call, opts FormatOptions) []Call {
	return ShownCallGroups([][]Call{calls}, opts)[0]
}

// ShownIndices is the places in each group of the calls a [FormatCallGroups]
// render of groups under opts shows, ascending: the walk itself, which
// [ShownCallGroups] and the render are both built on, so a place is here
// exactly where its call's line is printed. Every write's place is here, never
// a skipped name's, and the reads past the one read cap, COUNTED ACROSS THE
// GROUPS in the order given, are each group's last reads.
//
// A caller that must know which calls of a group its render shows, to end a
// line with words of its own ([FormatOptions.Notes]), reads the places here
// rather than matching values against [ShownCallGroups]: two equal calls of
// one group, one shown and one left out past the cap, are told apart by place
// and by nothing else.
func ShownIndices(groups [][]Call, opts FormatOptions) [][]int {
	out := make([][]int, len(groups))
	reads := 0
	for i, calls := range groups {
		at := make([]int, 0, len(calls))
		for j, call := range calls {
			if slices.Contains(opts.Skip, call.Name) {
				continue
			}
			if slices.Contains(opts.Reads, call.Name) {
				if opts.MaxReadCalls > 0 && reads >= opts.MaxReadCalls {
					continue
				}
				reads++
			}
			at = append(at, j)
		}
		out[i] = at
	}
	return out
}

// ShownCallGroups is the calls each group of a [FormatCallGroups] render of
// groups under opts shows: the group's calls, in its order, minus the skipped
// names and the reads past the one read cap, COUNTED ACROSS THE GROUPS in the
// order given, so a group shows only the reads the earlier groups left of the
// cap. It is the walk the render draws its lines from: the calls at the
// places [ShownIndices] walks, which a caller needing a place reads instead.
//
// A CALLER MAY RELY ON: each group's shown calls are a subsequence of the
// group, each the very value given; a write is never left out; the reads left
// out are the group's LAST reads. So a caller naming the calls a render left
// out (internal/agent/runner's leftOut) walks each group beside its shown
// calls, never a second rule, and names exactly the ones the render counted.
func ShownCallGroups(groups [][]Call, opts FormatOptions) [][]Call {
	at := ShownIndices(groups, opts)
	out := make([][]Call, len(groups))
	for i, calls := range groups {
		shown := make([]Call, 0, len(at[i]))
		for _, j := range at[i] {
			shown = append(shown, calls[j])
		}
		out[i] = shown
	}
	return out
}

// FormatCalls renders an evidence-only summary of tool calls.
//
// Each line is `- name(args) → success | error: text`, with a trailing
// "(read)" when the tool is annotated read-only, and last the call's
// [FormatOptions.Notes]. Unknown arguments say why in their place, never
// `name()`, which reads as none: [Call.Withheld], [Call.ArgsUnread] or
// [Call.Unfinished]. No surviving calls render "(none)", not an empty string,
// which reads as a section the engine forgot to fill in. It is
// [FormatCallGroups]' render of one group, so the two never part.
func FormatCalls(calls []Call, opts FormatOptions) string {
	return FormatCallGroups([][]Call{calls}, opts)[0]
}

// FormatCallGroups renders the lists of calls one brief tells under headings
// of its own (internal/agent/runner's calledAfter), one string per group in
// the order given, each as [FormatCalls] renders a list, "(none)" and its
// omitted line included, UNDER ONE READ CAP: [FormatOptions.MaxReadCalls]
// counts the read lines of every group together, spent in the order given,
// and each group's omitted line counts its reads the cap left out. A shown
// call's line ends with the [FormatOptions.Notes] for its place in its group.
//
// ONE CAP, NEVER ONE PER GROUP: N lists capped apart would tell N times the
// reads the cap allows. Fit them with [CallPieces] over the groups'
// concatenation, which names exactly what this shows.
func FormatCallGroups(groups [][]Call, opts FormatOptions) []string {
	at := ShownIndices(groups, opts)
	out := make([]string, len(groups))
	for i, calls := range groups {
		var notes []string
		if i < len(opts.Notes) {
			notes = opts.Notes[i]
		}
		out[i] = renderCalls(calls, at[i], notes, opts)
	}
	return out
}

// renderCalls is one group's lines: those of the calls at the places at that
// the render shows ([ShownIndices]), each ending with the note notes gives its
// place where it gives one, and the line counting the reads of calls it leaves
// out, which carries no note.
func renderCalls(calls []Call, at []int, notes []string, opts FormatOptions) string {
	var lines []string
	readsShown := 0
	for _, j := range at {
		call := calls[j]
		marker := ""
		if slices.Contains(opts.Reads, call.Name) {
			marker = " (read)"
			readsShown++
		}
		note := ""
		if j < len(notes) && notes[j] != "" {
			note = " " + notes[j]
		}
		called, outcome := CallLine(call, opts)
		lines = append(lines, "- "+called+" → "+outcome+marker+note)
	}
	if dropped := readsIn(calls, opts) - readsShown; dropped > 0 {
		// Only READS are ever omitted, and they are re-runnable by
		// construction — the prompt permits re-running exactly those. A
		// write is the whole reason the ledger exists and always renders.
		lines = append(lines, "- (+"+itoa(dropped)+" further read call(s) omitted)")
	}
	if len(lines) == 0 {
		return noCalls
	}
	return strings.Join(lines, "\n")
}

// CallLine is the two halves of call's line as [FormatCalls] draws it under
// opts: what was called, `name(args)`, and its outcome, `success` or `error: `
// and the error, fitted as the render fits it. A line joins them with ` → `,
// and a render adds its read marker and note after.
//
// ONE LINE EACH under [FormatOptions.OneLine]: every line break in the tool's
// name or its error (\r, \n, U+2028, U+2029) is drawn as a space, so a tool's
// own text can never start a line of a brief or an item, where a heading at a
// line's start is an entry of its own (internal/sandbox/executor,
// transcript.go). The arguments are JSON, which escapes its line breaks.
func CallLine(call Call, opts FormatOptions) (called, outcome string) {
	outcome = "success"
	if answers(call, opts) {
		// FITTED as an error is: the first ValueLimit characters, a rewrite
		// past them, so the identifier a write returned (a created item's
		// key, a message's id) reaches a run that remembers none of it.
		text := call.Result
		if over(text, opts.ValueLimit) {
			text = opts.Fitted.text(Piece{Kind: PieceAnswer, Text: text, Limit: opts.ValueLimit})
		}
		outcome = "success: " + oneLine(text, opts)
	}
	if call.Failed {
		// FITTED, like the arguments beside it. Call.Result is the
		// tool's own output, which is a PAYLOAD by this package's
		// definition — authored outside the engine and unbounded, so a
		// failed HTTP call can put a whole error document on one
		// ledger line, re-sent on every round of every later phase.
		// The full text is on the phase event this line summarises.
		text := call.Result
		if over(text, opts.ValueLimit) {
			text = opts.Fitted.text(Piece{Kind: PieceError, Text: text, Limit: opts.ValueLimit})
		}
		outcome = "error: " + oneLine(text, opts)
	}
	args := renderArgs(call.Args, opts)
	switch {
	case call.Withheld:
		// SAID, never drawn as `()`: a call whose arguments every record
		// withholds made a call with arguments, and `tool()` reads as
		// the tool called with none — a different call, and a repeat a
		// reader takes for a retry of nothing.
		args = withheldArgs
	case call.ArgsUnread:
		// SAID WHATEVER ARGS HOLDS, as a withheld call's are: its record
		// says only that it was made with some.
		args = unreadArgs
	case call.Unfinished && len(call.Args) == 0:
		// SAID, for the same reason: its entry kept its identity alone,
		// so no record says what it was called with. One that carries
		// arguments is drawn with them.
		args = unkeptArgs
	}
	return oneLine(call.Name, opts) + "(" + args + ")", outcome
}

// answers reports whether call's line under opts ends with what it returned
// ([FormatOptions.Answers]): a write that succeeded and returned something.
func answers(call Call, opts FormatOptions) bool {
	return opts.Answers && !call.Failed && strings.TrimSpace(call.Result) != "" &&
		!slices.Contains(opts.Reads, call.Name)
}

// oneLine is text with every line break drawn as a space where opts draws a
// call on one line ([FormatOptions.OneLine]), and text as it is otherwise.
func oneLine(text string, opts FormatOptions) string {
	if !opts.OneLine {
		return text
	}
	return strings.Map(func(r rune) rune {
		switch r {
		case '\r', '\n', '\u2028', '\u2029':
			return ' '
		}
		return r
	}, text)
}

// withheldArgs is what [FormatCalls] renders in place of the arguments of a
// call whose arguments every record withholds ([Call.Withheld]). ONE WORDING
// FOR BOTH such calls, which a [Call] does not tell apart: the one the run
// held, whose arguments wait sealed for a person's approval, and the one the
// person approved, made off those sealed arguments.
const withheldArgs = "arguments sealed for a person's approval, kept on no record"

// unreadArgs is what [FormatCalls] renders in place of the arguments of a call
// made with arguments in text that could not be read ([Call.ArgsUnread]): its
// record holds text that does not read back, or the bridge could not parse
// what the model wrote. TRUE OF BOTH, so it never says the text was recorded,
// which the second is not: it was made with arguments, and no record says
// which.
const unreadArgs = "arguments in text that could not be read"

// unkeptArgs is what [FormatCalls] renders in place of the arguments of a call
// whose outcome is unknown ([Call.Unfinished]) and which carries none: its log
// entry kept its identity alone, so no record holds what it was called with,
// and `tool()` would read as the tool called with none, a different call.
const unkeptArgs = "arguments kept on no record"

// noCalls is what [FormatCalls] renders for a run of calls that shows none.
const noCalls = "(none)"

// readsIn counts the read calls a render would show without a cap.
func readsIn(calls []Call, opts FormatOptions) int {
	n := 0
	for _, call := range calls {
		if !slices.Contains(opts.Skip, call.Name) && slices.Contains(opts.Reads, call.Name) {
			n++
		}
	}
	return n
}

// Iteration is one completed executor → reviewer round of a single turn.
//
// Appended just before a self_iterate loops back, so it is a CLOSED snapshot.
// Every record is a self_iterate round (done and failed end the turn), so the
// review decision is implied and not stored. ONE CALL LIST: one phase makes
// the calls.
//
// JSON tags are load-bearing: a detached executor ends the turn's segment and
// its completion starts a NEW one, so without round-tripping the ledger
// through the pending run's state the resumed turn would forget every earlier
// round and could re-fire its deliveries.
type Iteration struct {
	Iteration int `json:"iteration"`

	// Through is the last round this entry stands for as a SPAN of rounds
	// (Iteration the first), and zero for a round as the loop closed it.
	//
	// A SPAN IS ROUNDS A PARK DROPPED: a parked turn whose closed rounds
	// outgrow their share of the run's row folds whole rounds into one entry
	// (internal/agent/execstate). It keeps their calls in two tallies by tool,
	// never a name folded away: a resumed call log seeds them and a delivery
	// check reads them by name and outcome ([Called]). It keeps the
	// [Iteration.ArtifactHash] of its LAST round, so the stall guard still
	// compares each round with the one before. Intent, output and briefs are
	// not kept ([RenderIterations]). An older build reads a span as its first
	// round holding every call, the safe direction.
	Through int `json:"through,omitempty"`

	// Intent is what the round set out to do, in the executor's own words.
	Intent string `json:"intent,omitempty"`

	// Calls is everything the round invoked, engine-recorded — every call
	// its run's record kept.
	Calls []Call `json:"tool_calls,omitempty"`

	// Elided is the round's calls its run's record did not keep, by identity
	// ([Elided]): carried with the round so the turn's later segments still
	// count them — a resumed call log seeds them, and a delivery check reads
	// them as calls of the turn ([Called]) — where the calls themselves are
	// gone.
	Elided []Elided `json:"elided_calls,omitempty"`

	// Absorbed is the calls the run's call log took in from the round's own
	// calls' work — a delegated worker's under a `delegate` — by identity
	// ([Elided], Failed unset): carried with the round so a later segment's
	// call log seeds them, and read by nothing else, since the round did not
	// make them — no delivery check counts a worker's call as the turn's.
	Absorbed []Elided `json:"absorbed_calls,omitempty"`

	// Reads are the names among the calls above that MCP annotations
	// positively mark read-only. Carried per-record rather than looked up
	// at render time because the surface can change between rounds, and a
	// read re-classified as a write would retroactively rewrite what the
	// ledger says happened.
	Reads []string `json:"read_only_names,omitempty"`

	Text        string `json:"text,omitempty"`
	ReviewNotes string `json:"review_notes,omitempty"`

	// CompletedWork is reviewer-authored prose naming what already landed.
	// Empty whenever Review itself never chose self_iterate — above all on
	// the engine's post-Review done→self_iterate override, which is
	// precisely why the tool-call lists above carry the guarantee and this
	// field only enriches it.
	CompletedWork string `json:"completed_work,omitempty"`

	// ArtifactHash is the hash of what the round produced, as the turn's
	// stall guard observed it when the round was sent back
	// (internal/agent/turn's ArtifactHash).
	//
	// ON THE RECORD because a turn's rounds run in different segments: each
	// executor launch parks the turn, and the round after it is reviewed
	// in a segment that never saw the rounds before. The guard that ends a
	// turn repeating itself has to compare across segments, so the closed
	// rounds carry what it compares. A hash rather than the text: the record
	// rides every park, and the text is already on it under Text where it
	// is the round's own prose rather than the reviewer's chosen artifact.
	ArtifactHash string `json:"artifact_hash,omitempty"`
}

// RenderIterations renders accumulated rounds as the shared prior-work block.
//
// Returns "" when there is nothing to show — the first round of every turn —
// so callers drop the whole section rather than emit an empty heading.
//
// Section HEADINGS are the caller's: the executor frames this as "already
// done, do not repeat" while the reviewer frames it as duplicate-delivery
// evidence, so engine prose stays in the prompt package and this stays a
// renderer.
//
// fitted is the caller's rewrite of the pieces [IterationPieces] named; nil
// renders every payload whole.
func RenderIterations(records []Iteration, skip []string, fitted Fitted) string {
	if len(records) == 0 {
		return ""
	}
	blocks := make([]string, 0, len(records))
	for _, rec := range records {
		if rec.Through != 0 {
			blocks = append(blocks, renderSpan(rec, skip))
			continue
		}
		lines := []string{"### Iteration " + itoa(rec.Iteration)}
		if rec.Intent != "" {
			lines = append(lines, "Set out to: "+rec.Intent)
		}
		opts := Format(skip, rec.Reads)
		opts.Fitted = fitted
		lines = append(lines, "Called:")
		// "(none)" ONLY FOR A ROUND THAT CALLED NOTHING: one whose record
		// keeps its calls as a tally alone — a parked turn's closed round
		// condensed that far — called what its tally line says.
		if len(rec.Calls) > 0 || Count(rec.Elided) == 0 {
			lines = append(lines, FormatCalls(rec.Calls, opts))
		}
		if elided := FormatElided(rec.Elided, skip); elided != "" {
			lines = append(lines, elided)
		}
		if rec.Text != "" {
			// FITTED at render, whole in the record. See
			// RenderedArtifactLimit: this text is a whole tool loop's
			// assistant output, one per iteration, re-sent on every round
			// of every later phase — so past the limit it is rewritten
			// to fit, deliverable first, rather than cut from either end.
			text := rec.Text
			if over(text, RenderedArtifactLimit) {
				text = fitted.text(Piece{Kind: PieceProduced, Text: text, Limit: RenderedArtifactLimit})
			}
			lines = append(lines, "Produced: "+text)
		}
		if rec.CompletedWork != "" {
			lines = append(lines, "Reviewer, on what already landed: "+rec.CompletedWork)
		}
		if rec.ReviewNotes != "" {
			lines = append(lines, "Reviewer's correction: "+rec.ReviewNotes)
		}
		blocks = append(blocks, strings.Join(lines, "\n"))
	}
	return strings.Join(blocks, "\n\n")
}

// renderSpan is the block of an entry that stands for a span of rounds a park
// dropped ([Iteration.Through]): which rounds, that what they set out to do,
// produced and were told is not kept — said, so no reader takes the rounds for
// ones that said nothing — and what they called, by tool, which is all the
// span keeps of their calls; "(none)" only where they called nothing. A span
// of one round says it of one.
func renderSpan(rec Iteration, skip []string) string {
	heading, notKept, calls := "### Iteration "+itoa(rec.Iteration), roundNotKept, roundCalls
	if rec.Through > rec.Iteration {
		heading, notKept, calls = "### Iterations "+itoa(rec.Iteration)+"–"+itoa(rec.Through), spanNotKept,
			spanCalls
	}
	lines := []string{heading, notKept, "Called:"}
	if called := tallyLine(rec.Elided, skip, calls); called != "" {
		lines = append(lines, called)
	} else {
		lines = append(lines, noCalls)
	}
	return strings.Join(lines, "\n")
}

// spanNotKept and roundNotKept are what a span's block says of its rounds'
// prose, for a span of several rounds and of one.
const (
	spanNotKept = "(These rounds are not kept on the parked turn: what they set out to do, what they " +
		"produced and what their reviewer said are gone; their calls are counted by tool below.)"
	roundNotKept = "(This round is not kept on the parked turn: what it set out to do, what it produced " +
		"and what its reviewer said are gone; its calls are counted by tool below.)"
)

// spanCalls and roundCalls open the line counting a span's calls
// ([tallyLine]), for a span of several rounds and of one.
func spanCalls(total int) string {
	return "- (" + itoa(total) + " call(s) these rounds' runs made, which the turn's record counts by tool alone"
}

func roundCalls(total int) string {
	return "- (" + itoa(total) + " call(s) its run made, which the turn's record counts by tool alone"
}
