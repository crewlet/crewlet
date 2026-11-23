package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// A Muse Code run takes its notes through HOOKS of a plugin of its own
// (ADR-0177's hook channel, measured on 1.4.25): three PostToolUse hooks
// between its steps, each answering at most one item, and a Stop hook that
// blocks the finish with them; each runs the note helper once per firing
// (internal/notehelper, -adapter muse-hook), which takes over the bridge's
// steer_take and writes the answer on the hook's stdout and the take's token
// on its stderr. The receipts are read off the session's record
// (musethread.go).

// MuseSteerInstruction is the standing instruction about notes a Muse Code run
// is given, saying where its harness puts one (measured on 1.4.25).
const MuseSteerInstruction = steerInstructionHead +
	"Muse Code adds it to the conversation as context of its own: a developer " +
	"message after the results of a step's tool calls, and, as you are about to " +
	"finish, a developer instruction. " + steerInstructionTail

// NoteInstruction is a Muse Code run's instruction about notes ([noteReader]).
func (Muse) NoteInstruction() string { return MuseSteerInstruction }

// HandsSkills reports that a Muse Code run takes skills of its own
// ([skillHolder]): each written as `~/.agents/skills/crewlet:<name>/SKILL.md`,
// which Muse catalogues under that name and loads with its read_skill tool.
func (Muse) HandsSkills() bool { return true }

// museNotesPlugin is the plugin's id, and museNoteHooks its note hooks' ids:
// three PostToolUse hooks and the finish's. The receipts are taken from these
// hooks' records alone, whose key is `plugin:<id>:<hook id>`; a repository's
// hook is recorded under its own file's path.
const museNotesPlugin = "crewlet-notes"

var museNoteHooks = []museHook{
	{id: "posttooluse-1", event: "PostToolUse", slot: 1},
	{id: "posttooluse-2", event: "PostToolUse", slot: 2},
	{id: "posttooluse-3", event: "PostToolUse", slot: 3},
	{id: "stop", event: "Stop", slot: 4},
}

// museHook is one hook of the plugin.
type museHook struct {
	id, event string
	slot      int
}

// museHookKey is how the session's record names hook h's run.
func museHookKey(h museHook) string { return "plugin:" + museNotesPlugin + ":" + h.id }

// museHookTimeout is how long Muse Code waits on one of the run's hooks
// before killing it and going on with nothing from it: Claude Code's ten
// seconds ([ClaudeHookTimeout]) for its reason, the bridge answering a take
// within half of it.
const museHookTimeout = 10 * time.Second

// museHookAnswerBytes is Muse Code's ceiling on one hook's stdout: past 16,384
// bytes it kills the hook and parses nothing (measured on 1.4.25). The answer
// is raw UTF-8 (escaped, a character costs six bytes and more).
const museHookAnswerBytes = 16384

// museHookFraming is the widest answer's bytes around its message: the
// PostToolUse answer's, beside which the Stop block's is shorter.
var museHookFraming = len(`{"hookSpecificOutput":{"additionalContext":"","hookEventName":"PostToolUse"}}`)

// museItemUnits is the most UTF-16 units one Muse Code hook answer carries:
// the ceiling less the framing, at three bytes a unit, the most any unit of
// an item takes: the engine draws every control but tab and newline as
// U+FFFD as it renders an item (internal/agent/prompts), so JSON escapes no
// unit past two bytes. It holds the engine's widest item (4,505 units), ONE
// ITEM AN ANSWER: a second message goes to the next hook's answer, three a
// step (ADR-0177's bounds stay as they are).
var museItemUnits = (museHookAnswerBytes - museHookFraming) / 3

var _ hookDelivery = Muse{}

// notesThatFit is how many of messages one Muse Code hook answer carries: one.
func (Muse) notesThatFit(messages []string) int { return min(1, len(messages)) }

// roomBeside is the room a second message has beside first: an answer of its
// own, the next hook's ([museItemUnits]).
func (Muse) roomBeside(string) int { return museItemUnits }

// museTakeField is the field of a hook's answer carrying its take's token,
// which the note helper removes and writes on the hook's stderr (its
// takeField): the record keeps a hook's stderr, and the model never sees it.
const museTakeField = "crewlet_take"

// hookAnswer is the answer to a Muse Code hook at event, the bridge's boundary
// (mcpbridge's SteerEventToolBatch or SteerEventFinish), handing notes under
// take: their messages as a PostToolUse's additional context between steps,
// or as a Stop block's reason at the finish, which Muse Code hands the model
// and continues the turn; and take's token in [museTakeField]. Empty for none.
func (Muse) hookAnswer(event, take string, notes []HookNote) string {
	if len(notes) == 0 {
		return ""
	}
	messages := make([]string, len(notes))
	for i, n := range notes {
		messages[i] = n.Message
	}
	text := museJSONString(strings.Join(messages, claudeNoteSeparator))
	token := museJSONString(claudeTakeToken(take))
	switch event {
	case claudeEventToolBatch:
		return `{"` + museTakeField + `":` + token +
			`,"hookSpecificOutput":{"hookEventName":"PostToolUse","additionalContext":` + text + `}}`
	case claudeEventFinish:
		return `{"` + museTakeField + `":` + token + `,"decision":"block","reason":` + text + `}`
	}
	return ""
}

// museJSONString is s as a JSON string escaping only what JSON requires (the
// quote, the backslash and U+0000-U+001F), every other character raw UTF-8,
// which [museItemUnits] counts at three bytes a unit at most: encoding/json
// escapes `<`, `>` and `&` unless told not to, and U+2028 and U+2029 always,
// six bytes each. A byte that is not UTF-8 is written as U+FFFD, as
// encoding/json writes it.
func museJSONString(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// musePluginManifest is the notes plugin's manifest for a run whose take
// config is config: a hook per [museNoteHooks], each the note helper at
// helper once per firing; the first PostToolUse hook also reports skill loads
// where skills says so. Its approval is the definition's hash, which the run's
// script asks for after installing it.
func musePluginManifest(helper, config string, notes, skills bool) ([]byte, error) {
	var hooks []map[string]any
	for _, h := range museNoteHooks {
		takes := notes
		loads := skills && h.slot == 1
		if !takes && !loads {
			continue
		}
		command := []string{helper, "-adapter", "muse-hook", "-config", config, "-slot", strconv.Itoa(h.slot)}
		if loads {
			command = append(command, "-skill-loads")
		}
		if !takes {
			command = append(command, "-loads-only")
		}
		hooks = append(hooks, map[string]any{"id": h.id, "event": h.event, "command": command,
			"timeoutMs": museHookTimeout.Milliseconds(), "async": false})
	}
	return json.MarshalIndent(map[string]any{
		"schemaVersion": 1, "name": museNotesPlugin, "displayName": "Crewlet notes", "version": "1.0.0",
		"description": "A person's notes to this run, between its steps.",
		"compat":      map[string]string{"source": "native", "manifestDir": ".muse-plugin"},
		"capabilities": map[string]any{"skills": []any{}, "developerPrompts": []any{}, "mcpServers": []any{},
			"commands": []any{}, "reminders": []any{}, "hooks": hooks},
	}, "", "  ")
}

// writeMuseSkills writes each of a run's skills as `<skills>/<listed
// name>/SKILL.md` ([Paths.MuseSkills]), after removing every skill an earlier
// run wrote there (`crewlet:*`), so a reused box lists only this run's beside
// the home's own.
func writeMuseSkills(ctx context.Context, box sandbox.Sandbox, skills []sandbox.Skill, paths Paths) error {
	if _, err := box.Exec(ctx, "rm -rf "+shellword.Quote(paths.MuseSkills())+"/"+
		shellword.Quote(sandbox.SkillNamespace+":")+"*", sandbox.ExecOptions{}); err != nil {
		return fmt.Errorf("executor: removing an earlier run's Muse Code skills: %w", err)
	}
	for _, s := range skills {
		file, err := skillFile(s.Listed(), s.Description, s.Body)
		if err != nil {
			return err
		}
		if err = box.WriteFile(ctx, paths.MuseSkills()+"/"+s.Listed()+"/SKILL.md", file); err != nil {
			return fmt.Errorf("executor: writing the run's skill %q: %w", s.Listed(), err)
		}
	}
	return nil
}

// notesOffWhy is what switches a Muse Code run's reading of notes off: a
// managed policy that allows only managed hooks, or the run's plugin that
// Muse Code would not install or approve, which the run's error stream names.
func (Muse) notesOffWhy() string {
	return "a managed policy that allows only managed hooks, or the run's notes plugin that Muse Code would " +
		"not install or approve, which the run's error stream names"
}
