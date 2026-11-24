package executor

import (
	"archive/tar"
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// kimiWireLine is the part of one wire.jsonl line the confirm reader reads: its
// type, a message's id and role, and a usage record's scope.
type kimiWireLine struct {
	Type    string `json:"type"`
	Message struct {
		ID   string `json:"id"`
		Role string `json:"role"`
	} `json:"message"`
	UsageScope string `json:"usageScope"`
}

// kimiNotesHeld is the inbox ids of takes, in serve order and once each, whose
// note the main agent's wire.jsonl shows reached the model as a current
// message: its `context.append_message`, a user message under its prompt_id
// ([KimiNoteID]), then a `usage.record` of `usageScope: turn` (a request of the
// agent's loop answered) with no compaction, undo or clear between. Measured on
// 2.7.3: the line is on disk before its request; killed between the
// two, the record holds the line alone; and past a compaction, whose record
// names no message it kept, a request may carry the note only inside its
// summary (ADR-0177), so such a note is told again (code-sandbox.md, "Kimi
// Code"). A last line with no newline is a write cut off; a line past the
// bound is no line.
func kimiNotesHeld(ctx context.Context, wire io.Reader, takes []ServedTake) ([]string, error) {
	asked := make(map[string]string, len(takes))
	for _, take := range takes {
		asked[KimiNoteID(take.InboxID)] = take.InboxID
	}
	pending := map[string]bool{}
	held := map[string]bool{}
	// moved is what the line read last moved to held, taken back where that
	// line turns out to be the file's unended last.
	var moved []string
	src := &lastByte{r: wire}
	err := readLines(bufio.NewReaderSize(src, lineChunkBytes), maxLineBytes, func(line []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		moved = moved[:0]
		var l kimiWireLine
		if err := json.Unmarshal(line, &l); err != nil {
			// A LINE READ ONLY IN PART STILL COUNTS BY ITS TYPE: a compaction
			// whose other fields do not decode still clears, and nothing
			// else is read off it, so no such line confirms a note.
			l = kimiWireLine{Type: kimiLineType(line)}
		}
		switch l.Type {
		case "context.append_message":
			if inbox, ours := asked[l.Message.ID]; ours && l.Message.Role == "user" {
				pending[inbox] = true
			}
		case "usage.record":
			if l.UsageScope != "turn" {
				return nil
			}
			for inbox := range pending {
				if !held[inbox] {
					held[inbox] = true
					moved = append(moved, inbox)
				}
			}
			clear(pending)
		case "context.apply_compaction", "context.undo", "context.clear":
			clear(pending)
		}
		return nil
	}, func(int64) error { moved = moved[:0]; return nil })
	if err != nil {
		return nil, fmt.Errorf("executor: reading which notes the Kimi session holds: %w", err)
	}
	if src.read && src.last != '\n' {
		for _, inbox := range moved {
			delete(held, inbox)
		}
	}
	var out []string
	for _, take := range takes {
		if held[take.InboxID] && !slices.Contains(out, take.InboxID) {
			out = append(out, take.InboxID)
		}
	}
	return out, nil
}

// kimiLineType is the type a wire.jsonl line names, "" for a line that names
// none.
func kimiLineType(line []byte) string {
	var typed struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(line, &typed) != nil {
		return ""
	}
	return typed.Type
}

// notesHeld is the inbox ids of takes ([Kimi.inboxID]) whose note the box's
// copy of session's main agent record shows reached the model
// ([kimiNotesHeld]): its [recordConfirmer], as its driver confirms nothing.
// The record is found under any workspace directory, since Kimi names that
// directory by the run's working directory: an id matching none, or more than
// one, is a record that could not be read, which is neither held nor not.
func (Kimi) notesHeld(ctx context.Context, box sandbox.Sandbox, session Session,
	takes []ServedTake) ([]string, error) {
	if !strings.HasPrefix(session.ID, "session_") || !sandbox.ValidSessionID(session.ID) {
		return nil, fmt.Errorf("executor: %q is no Kimi session id", session.ID)
	}
	paths := PathsFor(box)
	found, err := box.Exec(ctx, `for w in `+shellword.Quote(paths.KimiSessions())+`/*/`+session.ID+
		`/agents/main/wire.jsonl; do [ -f "$w" ] && printf '%s\n' "$w"; done; :`, sandbox.ExecOptions{})
	if err != nil {
		return nil, fmt.Errorf("executor: finding the Kimi session's record: %w", err)
	}
	records := strings.Fields(strings.TrimSpace(found.Stdout))
	if len(records) != 1 {
		return nil, fmt.Errorf("executor: the box holds %d records of the Kimi session %s, not one", len(records),
			session.ID)
	}
	wire, err := box.OpenFile(ctx, records[0])
	if err != nil {
		return nil, fmt.Errorf("executor: opening the Kimi session's record: %w", err)
	}
	defer func() { _ = wire.Close() }()
	return kimiNotesHeld(ctx, wire, takes)
}

// transcript reads a Kimi archive ([kimiArchiveFormat]): each agent's
// wire.jsonl, a line at a time, as the session's entries, the main agent's
// its main thread and a subagent's its own. Any other member is passed unread.
func (Kimi) transcript(ctx context.Context, b *transcriptBuilder, archive io.Reader) error {
	tr := tar.NewReader(archive)
	b.handed.r = tr
	for {
		h, err := tr.Next()
		switch {
		case errors.Is(err, io.EOF):
			return nil
		case err != nil:
			return fmt.Errorf("%w: %w", errNotTar, err)
		}
		agent, ok := kimiWireMember(h.Name)
		if !ok {
			continue
		}
		if h.Typeflag != tar.TypeReg {
			if err := b.unread("a transcript of the harness's record is not a plain file in the archive, " +
				"and was not read"); err != nil {
				return err
			}
			continue
		}
		thread := agent
		if agent == "main" {
			thread = ""
		}
		if err := b.kimiRecord(ctx, thread, bufio.NewReaderSize(&b.handed, lineChunkBytes)); err != nil {
			return err
		}
	}
}

// kimiWireMember is the agent whose wire.jsonl an archive's member named name
// is, `wd_<key>/session_<id>/agents/<agent>/wire.jsonl`, false for any other.
func kimiWireMember(name string) (string, bool) {
	parts := strings.Split(path.Clean(name), "/")
	if len(parts) != 5 || !strings.HasPrefix(parts[1], "session_") || parts[2] != "agents" || parts[3] == "" ||
		parts[4] != "wire.jsonl" {
		return "", false
	}
	return parts[3], true
}

// kimiLine is the part of one wire.jsonl line a transcript reads.
type kimiLine struct {
	Type    string `json:"type"`
	Time    int64  `json:"time"`
	Message struct {
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Origin struct {
			Kind string `json:"kind"`
		} `json:"origin"`
	} `json:"message"`
	Event struct {
		Type       string          `json:"type"`
		ToolCallID string          `json:"toolCallId"`
		Name       string          `json:"name"`
		Args       json.RawMessage `json:"args"`
		Part       struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"part"`
		Result struct {
			Output  json.RawMessage `json:"output"`
			IsError bool            `json:"isError"`
		} `json:"result"`
	} `json:"event"`
	Summary string `json:"summary"`
}

// kimiRecord reads one agent's wire.jsonl as entries of thread, in recorded
// order: a person's message (`origin.kind: user`, never Kimi's own injected
// reminders) is the brief, or each note, late call and wake it carries where it
// opens with an engine heading ([headedIn]); a text part the model's words; a
// tool call and its result; a compaction its summary. A line that is no
// record, or one past the line bound, is said where it was.
func (b *transcriptBuilder) kimiRecord(ctx context.Context, thread string, lines *bufio.Reader) error {
	read := 0
	return readLines(lines, maxLineBytes, func(line []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if read++; read > b.bounds.ReadLines {
			return b.linesPast("record", "lines")
		}
		if len(strings.TrimSpace(string(line))) == 0 {
			return nil
		}
		var l kimiLine
		if json.Unmarshal(line, &l) != nil || l.Type == "" {
			return b.notARecord(len(line))
		}
		b.thread = thread
		return b.kimiEntry(l)
	}, func(n int64) error {
		return b.unread(fmt.Sprintf("a line of %s of the harness's record is past the %s one read reads, "+
			"and was not read", humanSize(n), humanSize(maxLineBytes)))
	})
}

// kimiEntry adds the entries one wire.jsonl line makes.
func (b *transcriptBuilder) kimiEntry(l kimiLine) error {
	at := time.UnixMilli(l.Time).UTC()
	switch l.Type {
	case "context.append_message":
		if l.Message.Role != "user" || l.Message.Origin.Kind != "user" {
			return nil
		}
		var text strings.Builder
		for _, c := range l.Message.Content {
			if c.Type == "text" {
				text.WriteString(c.Text)
			}
		}
		return b.kimiWords(text.String(), at)
	case "context.append_loop_event":
		ev := l.Event
		switch ev.Type {
		case "content.part":
			if ev.Part.Type == "text" && strings.TrimSpace(ev.Part.Text) != "" {
				return b.add(TranscriptEntry{Kind: EntryModel, At: at, Thread: b.thread}, ev.Part.Text)
			}
		case "tool.call":
			b.called(ev.ToolCallID, ev.Name)
			return b.add(TranscriptEntry{Kind: EntryToolCall, At: at, Thread: b.thread, Tool: ev.Name},
				compactJSON(ev.Args))
		case "tool.result":
			return b.add(TranscriptEntry{Kind: EntryToolResult, At: at, Thread: b.thread,
				Tool: b.answered(ev.ToolCallID), Failed: ev.Result.IsError}, kimiOutput(ev.Result.Output))
		}
	case "context.apply_compaction":
		if strings.TrimSpace(l.Summary) != "" {
			return b.add(TranscriptEntry{Kind: EntrySummary, At: at, Thread: b.thread}, l.Summary)
		}
	}
	return nil
}

// kimiWords adds a person's message: each note, late call and wake it carries
// where it opens with an engine heading, else the brief.
func (b *transcriptBuilder) kimiWords(text string, at time.Time) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	if !opensWithHeading(text) {
		return b.add(TranscriptEntry{Kind: EntryBrief, At: at, Thread: b.thread}, text)
	}
	for kind, words := range headedIn(text) {
		if err := b.add(TranscriptEntry{Kind: kind, At: at, Thread: b.thread}, words); err != nil {
			return err
		}
	}
	return nil
}

// kimiOutput is a tool result's output as text: a string as it is, or the text
// parts of a list (an MCP tool's), else the value as compact JSON.
func kimiOutput(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			if p.Type == "text" {
				b.WriteString(p.Text)
			}
		}
		return b.String()
	}
	return compactJSON(raw)
}
