package prompts

import (
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/toolname"
)

// PriorWorkHeader introduces the prior-work ledger: what an EARLIER ROUND of
// this turn already did, so the model does not redo a delivery that fired.
// A `(read)`'s results are gone and must be re-fetched; anything else changed
// the world, and a second copy is a duplicate. The markers are the ledger's
// own (ledger.FormatCalls). An earlier delivery is citable (the engine reads
// the whole turn.Record), or a model barred from repeating it submits uncited
// or delivers elsewhere. An `→ error` whose outcome is unknown
// (internal/agent/runner's UnfinishedNote) may have taken effect, so it is
// excepted, as in [ContinuedRoundRules], [ReviewHeader] and [lateCallsHeader];
// a line counting calls by tool (ledger.FormatElided) is read by the same
// rules, or a counted call is made again.
const PriorWorkHeader = "## Already done earlier in this turn" +
	"\nAn earlier round of this turn was sent back. Every call below marked" +
	" `→ success` ALREADY RAN:" +
	"\n- **`(read)`** fetched data only, and its results are not carried" +
	" over: re-run it if you need the data, never invent it." +
	"\n- **Everything else** changed something outside the engine: do NOT" +
	" issue it again — a person sees the second one as a duplicate. If it" +
	" was the delivery, it still counts: name it in `" + toolname.SubmitWork + "`'s" +
	" `deliveries`." +
	"\n- **`→ error`** did not take effect, unless it says its outcome is unknown: that one may have, so" +
	" check whether it did before making it again. Retry any other, fixed." +
	"\n- **A line counting calls by tool** (`some_tool ×3 (1 failed)`)" +
	" stands for calls the record no longer lists one by one, and they ran" +
	" all the same: those it does not count as failed ALREADY RAN, read by" +
	" the rules above, and one it says has an unknown outcome may have taken" +
	" effect: check whether it did before making it again." +
	"\nDeliver only what is still missing. A follow-up to an earlier" +
	" delivery ADDS to it — reply in its thread, edit the item — rather than" +
	" re-sending it, and is not a duplicate."

// UserMessage is the per-round content of an executor turn: the half of the
// prompt ALLOWED to move (package doc, "Byte-stability across rounds"). The
// reviewer does not use it: its evidence is fixed per round, a system-prompt
// fact.
type UserMessage struct {
	// TaskDescription is the ask; empty renders "(no description)" rather
	// than a blank.
	TaskDescription string

	// PriorWork is the rendered iteration ledger, empty on a turn's first
	// iteration.
	PriorWork string

	// Endings is how this conversation's earlier turns ended, rendered whole
	// with its own heading by [BuildConversationEndings]; empty on a
	// conversation's first turn, a turn serving none, and a company keeping no
	// session. Rendered at the turn's dispatch and frozen on its park, so every
	// brief carries the same bytes.
	Endings string

	// CapabilityChanges is what changed in the seat's access since it was
	// last told, with its own heading (internal/capability's ChangesBlock),
	// frozen like Endings; empty when nothing changed and on the seat's first
	// told turn.
	CapabilityChanges string

	// Steered is every note a person sent this turn that the brief carries,
	// each rendered by [SteerMessage]: those already read, in order, then
	// those this brief reads first. In the brief because the run it opens is
	// a fresh session with no engine conversation to carry them, so a note
	// the reviewer read would otherwise miss the phase that redoes the work.
	Steered []string
}

// KeyCapabilityChanges is the outline key of the access changes the turn is
// told.
const KeyCapabilityChanges = "capability_changes"

// taskHeader introduces the ask as a HEADING, like the blocks around it: a
// bare label files the ask under whatever heading precedes it (the earlier
// turns' endings), for the model and the dashboard's outline alike.
const taskHeader = "## Task"

// BuildPhaseUserMessage renders the executor's user message.
//
// The parts run oldest to newest (earlier turns' endings, the ask, earlier
// rounds of THIS turn), so the newest context sits nearest the answer; all
// are top-level peer headings, since nesting them misreads the ask.
//
// The task is ONE outline section whatever headings it carries (a trigger's
// whole body: a notification's "## Triage", a pull request's "# Title"); so
// are the endings and the ledger, whose quoted entries can hold anything.
func BuildPhaseUserMessage(m UserMessage) Prompt {
	task := m.TaskDescription
	if task == "" {
		task = "(no description)"
	}
	b := NewBuilder("\n\n")
	if m.Endings != "" {
		b.Heading("conversation_endings", m.Endings)
	}
	// Before the ask: the seat reads an access change before deciding how
	// to do it.
	if m.CapabilityChanges != "" {
		b.Heading(KeyCapabilityChanges, m.CapabilityChanges)
	}
	b.Heading("task", taskHeader+"\n"+task)
	if m.PriorWork != "" {
		b.Heading("prior_work", PriorWorkHeader+"\n"+m.PriorWork)
	}
	for i, note := range m.Steered {
		// Section keys are unique within a prompt.
		b.Heading(fmt.Sprintf("steered_%d", i+1), note)
	}
	return b.Build()
}

// SteerMessage is a person's note to a running turn, as the model reads it.
//
// Runtime- and phase-neutral: the same text lands in the engine's loop, an
// executor's brief and, via a hook, inside its run (internal/agent/steer), so
// it says nothing of where it arrived. It says WHO sent it (not weighed like
// tool output), that the work was UNDER WAY (no restart, no reply of its own),
// and that it STANDS for the turn as the later word over the task. Plain
// statements, not commands: command-like hook text can trip a model's
// injection guard. The executor reads an instruction, the reviewer what the
// work is judged against. The note is last and verbatim.
func SteerMessage(sender, note string) string {
	who, lead := steerSender(sender)
	return "## A note from " + who + ", sent while this turn was running" +
		"\n" + lead + " sent this through Crewlet while the work was under way." +
		" It is not a new request and not a repeat of the task: it corrects or" +
		" adds to the work in hand and stands for the rest of this turn; where" +
		" it and the task disagree, the note is the later word. What has" +
		" already been done stays unless the note says otherwise." +
		"\n\n" + strings.TrimSpace(note)
}

// SteerMessageSentBefore is a person's note handed to a reader after a note
// filed later was already handed to it, as the model reads it: the heading
// [SteerMessage] opens with, which a captured session's reader finds a note by
// (internal/sandbox/executor, transcript.go), then one line saying the note
// was sent before one already handed over, which stands where the two
// disagree, then the note. Without it the reader would take the older note as
// the later word, since it arrived last.
func SteerMessageSentBefore(sender, note string) string {
	who, lead := steerSender(sender)
	return "## A note from " + who + ", sent while this turn was running" +
		"\n" + lead + " sent this through Crewlet while the work was under way, before a later note" +
		" already handed to this turn: where the two disagree, the later note stands; otherwise this" +
		" note corrects or adds to the work in hand and stands for the rest of this turn." +
		"\n\n" + strings.TrimSpace(note)
}

// steerSender is who a note names its sender as, in its heading and leading
// its first line: the person, or a person who runs the company where the note
// names nobody.
func steerSender(sender string) (who, lead string) {
	if strings.TrimSpace(sender) == "" {
		return "a person who runs this company", "A person who runs this company"
	}
	return sender, sender
}
