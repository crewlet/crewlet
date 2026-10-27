package codingagent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// A person's notes to a run's turn, in the shapes the coding CLIs take them.
//
// The engine's run inbox (internal/engine, runsteer.go) decides which notes a
// run's CLI is handed and records them read; what is here is only how each CLI
// is handed them — the shape of a Claude Code hook's answer and how much of it
// the CLI keeps in front of its model, and the id OpenCode admits a note under —
// because those are facts about the CLIs, measured on them, and this package is
// where every other such fact lives.

// ClaudeContextBudget is how much one Claude Code hook answer carries, in
// UTF-16 code units.
//
// 9,000: Claude Code moves a hook's additionalContext longer than 10,000
// characters — its JavaScript length, which is UTF-16 code units — into a file
// and shows the model a 2 KB preview it is never asked to read, so a note past
// that would reach the model as a pointer to itself. The 1,000 below that are
// margin for the separators and a CLI that counts a little differently. One
// note rendered whole is at most about 4,600 units (2,000 characters of note,
// each at most two units, and its heading), so every answer carries at least
// one, and the rest wait for the CLI's next boundary.
const ClaudeContextBudget = 9000

// UTF16Len is s's length as Claude Code measures it — JavaScript's string
// length, in UTF-16 code units: a character outside the Basic Multilingual
// Plane, an emoji among them, is two. A byte that is not UTF-8 counts as one,
// as the replacement character it decodes to.
func UTF16Len(s string) int {
	n := 0
	for len(s) > 0 {
		r, size := utf8.DecodeRuneInString(s)
		s = s[size:]
		if r >= 0x10000 {
			n += 2
			continue
		}
		n++
	}
	return n
}

// claudeNoteSeparator joins the notes of one answer.
const claudeNoteSeparator = "\n\n"

// ClaudeNotesThatFit is how many of messages, from the first, one hook answer
// carries within [ClaudeContextBudget] once joined — never fewer than one where
// there is any, since a note the budget could never carry would wait for ever.
func ClaudeNotesThatFit(messages []string) int {
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

// HookNote is one note a hook answer hands the CLI: the message the model reads,
// and who sent it, which the CLI's own transcript line names.
type HookNote struct {
	By      string
	Message string
}

// claudeHookAnswer is the shape a Claude Code hook's answer takes: context for
// the model, and a line for the run's transcript.
type claudeHookAnswer struct {
	HookSpecificOutput claudeHookOutput `json:"hookSpecificOutput"`
	SystemMessage      string           `json:"systemMessage"`
}

type claudeHookOutput struct {
	HookEventName     string `json:"hookEventName"`
	AdditionalContext string `json:"additionalContext"`
}

// ClaudeHookAnswer is the answer to a Claude Code hook at event that hands its
// model notes: their messages as the hook's additional context, joined, and a
// line naming who sent them for the run's transcript. Empty for no notes.
//
// NEVER A DECISION: the answer carries no `decision`, `continue` or `reason`,
// so it can never stop the CLI's loop or block a step — what it adds is context
// the model reads (measured on 2.1.263 and 2.1.310: after a batch, beside the
// tool results; at the run's finish, a continuation of its turn whose answer is
// the run's). The transcript line reaches only the run's own stream, never a
// request.
func ClaudeHookAnswer(event string, notes []HookNote) string {
	if len(notes) == 0 {
		return ""
	}
	messages := make([]string, 0, len(notes))
	var senders []string
	for _, n := range notes {
		messages = append(messages, n.Message)
		by := strings.TrimSpace(n.By)
		if by == "" {
			by = "a person"
		}
		if !slices.Contains(senders, by) {
			senders = append(senders, by)
		}
	}
	line := "crewlet: a note from " + senders[0] + " reached the run"
	if len(notes) > 1 {
		line = "crewlet: " + strconv.Itoa(len(notes)) + " notes from " +
			strings.Join(senders, ", ") + " reached the run"
	}
	if event == "Stop" {
		line += " as it was finishing"
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	// THE NOTE AS WRITTEN: a person's "<" or "&" reaches the model as itself,
	// not as an escape the model has to undo.
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

// OpenCodeInboxID is the id OpenCode admits a note under for a job: `msg_` and
// the first 32 hex digits of a hash of the job and the note.
//
// UNIQUE TO THE JOB AND THE NOTE, because OpenCode's message ids are unique
// across the box's whole database, which a relaunch in the same box shares; and
// STABLE, because OpenCode answers a second admission under an id it already
// holds with the first one (measured), so a poller that admits a note again —
// after a reload that polls from the start — admits it once.
func OpenCodeInboxID(launch, noteID string) string {
	sum := sha256.Sum256([]byte("crewlet.steer\x00" + launch + "\x00" + noteID))
	return "msg_" + hex.EncodeToString(sum[:])[:32]
}
