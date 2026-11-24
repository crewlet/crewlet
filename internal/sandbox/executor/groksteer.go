package executor

import (
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// A Grok Build run takes its notes through HOOKS in its settings (ADR-0177's
// hook channel, measured on 1.0.92): PostToolUse and PostToolUseFailure after
// each call of a step, every one of them added after all of the step's
// results, and Stop, whose block makes the run go on. Each runs the note helper
// once per firing (internal/notehelper, -adapter grok-hook), which takes over
// the bridge's steer_take and writes the answer on the hook's stdout. Grok
// shows the model a hook's words as they were written, its record keeps them
// whole, and the take's token is read off that record (grokthread.go).

// GrokSteerInstruction is the standing instruction about notes a Grok Build run
// is given, saying where its harness puts one (measured on 1.0.92).
const GrokSteerInstruction = steerInstructionHead +
	"Grok Build adds it to the conversation as a system reminder of its own: after the results of a " +
	"step's tool calls, under \"Context from PostToolUse hook\", and, as you are about to finish, " +
	"under \"Stop hook feedback\". It ends with a token of Crewlet's own in square brackets, which is " +
	"no part of the note. " + steerInstructionTail

// NoteInstruction is a Grok Build run's instruction about notes ([noteReader]).
func (Grok) NoteInstruction() string { return GrokSteerInstruction }

// HandsSkills reports that a Grok Build run takes skills of its own
// ([skillHolder]): each written as `~/.grok/skills/crewlet:<name>/SKILL.md`,
// which Grok lists by its absolute path and its model reads with read_file
// (measured on 1.0.92); the read is reported as the skill's load.
func (Grok) HandsSkills() bool { return true }

// grokHookTimeout is how long Grok Build waits on one of the run's hooks before
// going on with nothing from it, in seconds as its settings take it: Claude
// Code's ten ([ClaudeHookTimeout]) for its reason. A hook past it is killed and
// its answer never handed over (measured on 1.0.92).
const grokHookTimeout = 10

// grokContextChars is the most characters of a hook's answer Grok Build hands
// its model: past 10,000 it hands the first 10,000 and "… [+N chars]"
// (measured on 1.0.92, answers of 16,000 to 1,100,000), cutting off the take's
// token at the end.
const grokContextChars = 10000

// grokContextBudget is the most UTF-16 units of notes one answer carries: the
// ceiling less the token and the separator before it. A character is at least
// one unit, so a budget in units never passes the ceiling.
var grokContextBudget = grokContextChars - len(claudeNoteSeparator) - claudeTakeTokenLen

var _ hookDelivery = Grok{}

// notesThatFit is how many of messages, from the first, one Grok Build hook
// answer carries within [grokContextBudget]: at least one.
func (Grok) notesThatFit(messages []string) int {
	total := 0
	for i, msg := range messages {
		size := UTF16Len(msg)
		if i > 0 {
			size += UTF16Len(claudeNoteSeparator)
		}
		if i > 0 && total+size > grokContextBudget {
			return i
		}
		total += size
	}
	return len(messages)
}

// roomBeside is how many UTF-16 units one Grok Build hook answer has left for a
// second message once it carries first.
func (Grok) roomBeside(first string) int {
	return grokContextBudget - UTF16Len(first) - UTF16Len(claudeNoteSeparator)
}

// hookAnswer is the answer to a Grok Build hook at event, the bridge's boundary
// (mcpbridge's SteerEventToolBatch or SteerEventFinish), handing notes under
// take: their messages, then take's token, as a PostToolUse's additional
// context between steps, or as a Stop block's reason at the finish, which Grok
// hands the model as feedback and goes on; and the token in [museTakeField]
// too, which the note helper writes on the hook's stderr. Empty for none. The
// note helper names the event a PostToolUseFailure fired for.
func (Grok) hookAnswer(event, take string, notes []HookNote) string {
	if len(notes) == 0 {
		return ""
	}
	messages := make([]string, len(notes))
	for i, n := range notes {
		messages[i] = n.Message
	}
	token := claudeTakeToken(take)
	text := museJSONString(strings.Join(messages, claudeNoteSeparator) + claudeNoteSeparator + token)
	field := `{"` + museTakeField + `":` + museJSONString(token)
	switch event {
	case claudeEventToolBatch:
		return field + `,"hookSpecificOutput":{"hookEventName":"PostToolUse","additionalContext":` + text + `}}`
	case claudeEventFinish:
		return field + `,"decision":"block","reason":` + text + `}`
	}
	return ""
}

// grokHookEvents are the hooks a run wired to its notes registers, each with
// whether it takes notes: the two after a call, and the finish.
var grokHookEvents = []string{"PostToolUse", "PostToolUseFailure", "Stop"}

// grokHooked is the events of [grokHookEvents] a run registers a hook at: each
// where it takes notes, PostToolUse alone where it reports loads only.
func grokHooked(notes, loads bool) []string {
	var events []string
	for _, event := range grokHookEvents {
		if notes || loads && event == "PostToolUse" {
			events = append(events, event)
		}
	}
	return events
}

// grokHooks is the run's hooks in its settings: one per [grokHookEvents] where
// notes says so, each the note helper at its path once per firing, held to the
// run's own session ([Paths.GrokThread]), the first also reporting a load of
// one of skills, read whole as written (its -skill-digest), where loads says
// so, and PostToolUse alone, taking nothing, where the run reports loads and
// takes no notes.
func grokHooks(paths Paths, notes, loads bool, skills []sandbox.Skill) string {
	var b strings.Builder
	for _, event := range grokHooked(notes, loads) {
		takes := notes
		reports := loads && event == "PostToolUse"
		command := []string{paths.GrokNoteHelper(), "-adapter", "grok-hook", "-config", paths.GrokTakeConfig(),
			"-thread-file", paths.GrokThread()}
		if reports {
			command = append(command, "-skill-loads")
			for _, digest := range skillDigests(skills) {
				command = append(command, "-skill-digest", digest)
			}
		}
		if !takes {
			command = append(command, "-loads-only")
		}
		quoted := make([]string, len(command))
		for i, c := range command {
			quoted[i] = shellword.Quote(c)
		}
		fmt.Fprintf(&b, "[[hooks.%s]]\nhooks = [{ type = \"command\", command = %s, timeout = %d }]\n\n", event,
			grokTOMLString(strings.Join(quoted, " ")), grokHookTimeout)
	}
	return b.String()
}

// notesOffWhy is what switches a Grok Build run's reading of notes off once
// its guard has passed ([grokGuard]): a note helper that could not reach the
// run's notes route, which its hooks' errors name in the session.
func (Grok) notesOffWhy() string {
	return "a note helper that could not reach the run's notes route, which the session's hook errors name"
}
