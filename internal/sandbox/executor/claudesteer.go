package executor

import (
	"bytes"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/words"
)

// claudeHookContext follows the event's name in the heading Claude Code puts
// before a hook's additional context ("PostToolBatch hook additional context:").
const claudeHookContext = " hook additional context:"

// ClaudeCodeSteerInstruction is the standing instruction about notes a Claude
// Code run is given.
//
// WHERE THE NOTE LANDS DEPENDS ON THE MODEL (measured on 2.1.310): after a
// batch, a system message on claude-opus-5-5 and claude-sonnet-5 but a system
// reminder appended to the last tool result on claude-sonnet-4-5; at the
// finish, a system reminder in a user message. Each opens with the event's name
// and [claudeHookContext], so the instruction names that heading as the carrier.
const ClaudeCodeSteerInstruction = steerInstructionHead +
	"Claude Code adds it to the conversation as context of its own, between your " +
	"steps: once the tool calls of a step have returned, headed \"" +
	claudeEventToolBatch + claudeHookContext + "\" — a system message, or on some " +
	"models a system reminder at the end of the last tool result — and as you are " +
	"about to finish, headed \"" + claudeEventFinish + claudeHookContext + "\". " +
	steerInstructionTail

// ClaudeHookTimeout is how long Claude Code waits on one of the run's hooks
// before going on without it.
//
// TEN SECONDS, not the vendor's 600: the harness holds its next model request
// on the hook at every batch, so a stuck bridge would cost ten minutes a step.
// The bridge answers within half of this (internal/api/mcpbridge, its take
// budget), so a note it served arrives in time; one whose answer comes too
// late is never read, its job's session showing no pair. The field is in
// seconds, and past it the harness discards a late answer (measured on 2.1.310).
const ClaudeHookTimeout = 10 * time.Second

// The Claude Code hook events a run's notes are taken at: the only two where
// every call of a step has its result, so a note never splits a call from it.
const (
	claudeEventToolBatch = "PostToolBatch"
	claudeEventFinish    = "Stop"
)

// claudeNotesSettings is the `--settings` file of a Claude Code run wired to
// its turn's notes ([Paths.ClaudeNotesSettings], [claudeNotesGate]): the
// every run's settings ([claudeSettings]) and the hooks, nothing else.
//
// NO FIELD NEWER THAN THE mcp_tool HOOK (2.1.118): in print mode the harness
// SILENTLY ignores a settings file that fails validation, so one unknown field
// costs every hook and the retention beside them. A Stop hook's context reaches
// the model only from 2.1.163, so an older release gets no file
// ([claudeNotesFloor]).
type claudeNotesSettings struct {
	claudeSettings

	// Hooks maps an event to its matchers. A map, so the file is written in
	// one order: encoding/json sorts its keys.
	Hooks map[string][]claudeHookMatcher `json:"hooks"`
}

// claudeHookMatcher is one entry of an event's hooks. One that names no
// matcher applies at every firing of the event, as the notes' hooks do; the
// skill-load hook names the tool it fires after ([writeClaudeSkills]).
type claudeHookMatcher struct {
	Matcher string       `json:"matcher,omitempty"`
	Hooks   []claudeHook `json:"hooks"`
}

// claudeHook is one hook: a call of a tool on one of the run's MCP servers.
//
// AN mcp_tool HOOK, not http or command: it rides the run's bridge connection
// and token (no new credential, URL or program in the box), its input is two
// strings rather than every batch's calls and results, and every failure is
// non-blocking, where a command hook exiting 2 (a shell syntax error) ends the
// loop. Input is a [claudeHookInput] or, for the skill-load hook, a
// [claudeSkillLoad].
type claudeHook struct {
	Type    string `json:"type"`
	Server  string `json:"server"`
	Tool    string `json:"tool"`
	Input   any    `json:"input"`
	Timeout int    `json:"timeout"`
}

// claudeHookInput is what the hook hands the tool, filled in by the harness.
// Agent is empty on the main agent's boundaries, the only ones a note is taken at.
type claudeHookInput struct {
	Event string `json:"event"`
	Agent string `json:"agent"`
}

// claudeSteerSettings is the settings of a run whose notes are reached through
// w: one hook at each event calling w's tool, with env as the `env` block
// ([newClaudeSettings]).
func claudeSteerSettings(w sandbox.SteerWiring, env map[string]string) claudeNotesSettings {
	hook := []claudeHookMatcher{{Hooks: []claudeHook{{
		Type: "mcp_tool", Server: w.Server, Tool: w.Tool,
		Input:   claudeHookInput{Event: "${hook_event_name}", Agent: "${agent_id}"},
		Timeout: int(ClaudeHookTimeout / time.Second),
	}}}}
	return claudeNotesSettings{
		claudeSettings: newClaudeSettings(env),
		Hooks:          map[string][]claudeHookMatcher{claudeEventToolBatch: hook, claudeEventFinish: hook},
	}
}

// ClaudeContextBudget is how much one Claude Code hook answer carries, in
// UTF-16 code units.
//
// 9,000: Claude Code moves additionalContext past 10,000 UTF-16 units into a
// file the model only sees a 2 KB preview of; 1,000 is margin for separators
// and counting differences. One rendered note is at most about 4,600 units
// (2,000 characters of up to two units each, and its heading), so every
// answer carries at least one.
const ClaudeContextBudget = 9000

// claudeNoteSeparator joins the notes of one answer.
const claudeNoteSeparator = "\n\n"

// notesThatFit is how many of messages, from the first, one Claude Code hook
// answer carries within [ClaudeContextBudget] — at least one, or an oversized note
// would wait for ever.
func (ClaudeCode) notesThatFit(messages []string) int {
	total := 0
	for i, msg := range messages {
		size := UTF16Len(msg)
		if i > 0 {
			size += UTF16Len(claudeNoteSeparator)
		}
		if i > 0 && total+size > ClaudeContextBudget {
			return i
		}
		total += size
	}
	return len(messages)
}

// roomBeside is how many UTF-16 units one Claude Code hook answer has left for a
// second message once it carries first: what the engine holds its account of a
// late call to, beside a person's widest note, so the two always share one
// answer ([ClaudeCode.notesThatFit]).
func (ClaudeCode) roomBeside(first string) int {
	return ClaudeContextBudget - UTF16Len(first) - UTF16Len(claudeNoteSeparator)
}

// claudeHookAnswer is a Claude Code hook's answer: context for the model, and
// a line for the run's transcript.
type claudeHookAnswer struct {
	HookSpecificOutput claudeHookOutput `json:"hookSpecificOutput"`
	SystemMessage      string           `json:"systemMessage"`
}

type claudeHookOutput struct {
	HookEventName     string `json:"hookEventName"`
	AdditionalContext string `json:"additionalContext"`
}

// hookAnswer is the answer to a Claude Code hook at event handing its
// model notes, wakes and late calls: their messages joined as additional
// context, and a transcript line naming the notes' senders, the wakes' writers
// and how many late calls, in that order, ending in take's token
// ([claudeTakeToken]), which the live line leaves out. Empty for none.
//
// NEVER A DECISION: no `decision`, `continue` or `reason`, so it can never stop
// the loop or block a step; at the finish the model continues its turn, whose
// answer is the run's (measured on 2.1.263 and 2.1.310). The transcript line
// reaches only the run's stream, never a request.
func (ClaudeCode) hookAnswer(event, take string, notes []HookNote) string {
	if len(notes) == 0 {
		return ""
	}
	messages := make([]string, 0, len(notes))
	var senders, writers []string
	persons, wakes, calls := 0, 0, 0
	for _, n := range notes {
		messages = append(messages, n.Message)
		switch {
		case n.Call:
			calls++
			continue
		case len(n.Writers) > 0:
			wakes += len(n.Writers)
			for _, w := range n.Writers {
				if w = strings.TrimSpace(w); w == "" {
					w = "somebody"
				}
				if !slices.Contains(writers, w) {
					writers = append(writers, w)
				}
			}
			continue
		}
		persons++
		by := strings.TrimSpace(n.By)
		if by == "" {
			by = "a person"
		}
		if !slices.Contains(senders, by) {
			senders = append(senders, by)
		}
	}
	// THE NOTES FIRST, then the wakes, so a line naming wakes or calls alone
	// opens with them, which is how the session reader tells its kind
	// ([hookLineKind]).
	var named []words.Said
	switch {
	case persons == 1:
		named = append(named, words.Said("a note from "+senders[0]))
	case persons > 1:
		named = append(named, words.Said(strconv.Itoa(persons)+" notes from "+strings.Join(senders, ", ")))
	}
	switch {
	case wakes == 1:
		named = append(named, words.Said(hookWakeOne+writers[0]+hookWakeAbout))
	case wakes > 1:
		named = append(named, words.Said(strconv.Itoa(wakes)+" "+hookWakesMany+strings.Join(writers, ", ")))
	}
	switch {
	case calls == 1:
		named = append(named, hookCallOne)
	case calls > 1:
		named = append(named, words.Said(strconv.Itoa(calls)+" "+hookCallsMany))
	}
	line := "crewlet: " + words.And(named) + " reached the run"
	if event == claudeEventFinish {
		line += " as it was finishing"
	}
	line += " " + claudeTakeToken(take)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	// A person's "<" or "&" reaches the model as itself, not escaped.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(claudeHookAnswer{
		HookSpecificOutput: claudeHookOutput{HookEventName: event,
			AdditionalContext: strings.Join(messages, claudeNoteSeparator)},
		SystemMessage: line,
	}); err != nil {
		// Unreachable: every field is a string.
		return ""
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

// The hook's line names the late calls it hands over in these words: one, or
// a count and the plural ([ClaudeCode.hookAnswer]). A line naming notes names them
// first, so one that opens with either names calls alone ([hookLineKind]).
const (
	hookCallOne   = "a call that finished after its run stopped waiting for it"
	hookCallsMany = "calls that finished after their run stopped waiting for them"
)

// The hook's line names the wakes it hands over in these words: one, its
// writer between the first two, or a count, the plural and the writers
// ([ClaudeCode.hookAnswer]). Named after the notes and before the calls, so a line
// opening with either names no note ([hookLineKind]).
const (
	hookWakeOne   = "a message from "
	hookWakeAbout = " about this work"
	hookWakesMany = "messages about this work from "
)

// claudeTakePrefix opens the token that names a take in a hook's words
// ([claudeTakeToken]).
const claudeTakePrefix = "[crewlet take "

// claudeTakeTokenLen is every take token's length: a take is named by a uuid's
// 36 characters (internal/engine, takeName), so the reader of a hook's words
// holds one token's length of them and no more ([textTakes]).
const claudeTakeTokenLen = len(claudeTakePrefix) + 36 + len("]")

// claudeTakeToken is the token naming take that a hook's answer carries and
// its job's session then shows ([threadLine.HookTakes]): ONE definition, which
// the answer, the cut and the ask all read.
func claudeTakeToken(take string) string { return claudeTakePrefix + take + "]" }

// withoutTakeToken is said without a trailing take token of exactly the shape
// [claudeTakeToken] writes (a uuid's 36 characters of hex and dashes), and the
// space before it: what the hook said, as a person reads it. Anything else is
// left as it is.
func withoutTakeToken(said string) string {
	trimmed := strings.TrimRightFunc(said, unicode.IsSpace)
	at := len(trimmed) - claudeTakeTokenLen
	if at < 0 || !strings.HasPrefix(trimmed[at:], claudeTakePrefix) || !strings.HasSuffix(trimmed, "]") {
		return said
	}
	for _, r := range trimmed[at+len(claudeTakePrefix) : len(trimmed)-1] {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && r != '-' {
			return said
		}
	}
	return strings.TrimRightFunc(trimmed[:at], unicode.IsSpace)
}
