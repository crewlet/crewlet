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
)

// transcript reads a Copilot archive ([copilotArchiveFormat]): its
// events.jsonl, a line at a time, as the session's entries. Any other member
// is passed unread.
func (Copilot) transcript(ctx context.Context, b *transcriptBuilder, archive io.Reader) error {
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
		if !copilotEventsMember(h.Name) {
			continue
		}
		if h.Typeflag != tar.TypeReg {
			if err := b.unread("a transcript of the harness's record is not a plain file in the archive, " +
				"and was not read"); err != nil {
				return err
			}
			continue
		}
		if err := b.copilotRecord(ctx, bufio.NewReaderSize(&b.handed, lineChunkBytes)); err != nil {
			return err
		}
	}
}

// copilotEventsMember reports whether an archive's member named name is a
// session's events.jsonl: `session-state/<session>/events.jsonl`.
func copilotEventsMember(name string) bool {
	parts := strings.Split(path.Clean(name), "/")
	return len(parts) == 3 && parts[0] == copilotSessionState && parts[1] != "" && parts[2] == "events.jsonl"
}

// copilotRecord reads one events.jsonl as entries, in recorded order:
//   - a user message is the brief, or, where it opens with an engine heading
//     (a note, a late call, a wake the extension sent), each it carries
//     ([headedIn]);
//   - an assistant message is the model's words and a call per tool request;
//   - a call's completion its result, failed where Copilot recorded it so;
//   - a completed compaction its summary.
//
// A subagent's lines are its thread, named by its agent. A line that is no
// record, or one past the line bound, is said where it was.
func (b *transcriptBuilder) copilotRecord(ctx context.Context, lines *bufio.Reader) error {
	read := 0
	err := readLines(lines, maxLineBytes, func(line []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if read++; read > b.bounds.ReadLines {
			return b.linesPast("record", "lines")
		}
		return b.copilotLine(line)
	}, func(n int64) error {
		return b.unread(fmt.Sprintf("a line of %s of the harness's record is past the %s one read reads, "+
			"and was not read", humanSize(n), humanSize(maxLineBytes)))
	})
	return err
}

// copilotLine adds the entries one line of an events.jsonl makes.
func (b *transcriptBuilder) copilotLine(line []byte) error {
	if len(strings.TrimSpace(string(line))) == 0 {
		return nil
	}
	ev, ok := decodeCopilotEvent(line)
	if !ok {
		return b.notARecord(len(line))
	}
	data := ev.Data
	b.thread = ev.AgentID
	at := claudeTime(ev.Timestamp)
	switch ev.Type {
	case "user.message":
		return b.copilotWords(data.Content, at)
	case "assistant.message":
		if strings.TrimSpace(data.Content) != "" {
			if err := b.add(TranscriptEntry{Kind: EntryModel, At: at, Thread: b.thread}, data.Content); err != nil {
				return err
			}
		}
		return b.copilotCalls(data.ToolRequests, at)
	case "tool.execution_complete":
		failed := !looseTrue(data.Success)
		text := data.Result.Content
		if failed {
			var why struct {
				Message string `json:"message"`
			}
			_ = json.Unmarshal(data.Error, &why)
			text = firstNonBlank(why.Message, errorText(data.Error))
		}
		return b.add(TranscriptEntry{Kind: EntryToolResult, At: at, Thread: b.thread,
			Tool: b.answered(data.ToolCallID), Failed: failed}, text)
	case "session.compaction_complete":
		var summary struct {
			Data struct {
				SummaryContent string `json:"summaryContent"`
			} `json:"data"`
		}
		if json.Unmarshal(line, &summary) == nil && strings.TrimSpace(summary.Data.SummaryContent) != "" {
			return b.add(TranscriptEntry{Kind: EntrySummary, At: at, Thread: b.thread}, summary.Data.SummaryContent)
		}
	}
	return nil
}

// copilotWords adds a user message: each note, late call and wake it carries
// where it opens with an engine heading, else the brief.
func (b *transcriptBuilder) copilotWords(text string, at time.Time) error {
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

// copilotCalls adds a call per tool request of an assistant message
// (`toolRequests[{toolCallId, name, arguments}]`), each remembered for its
// result.
func (b *transcriptBuilder) copilotCalls(raw json.RawMessage, at time.Time) error {
	if len(raw) == 0 {
		return nil
	}
	var requests []struct {
		ID        string          `json:"toolCallId"`
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(raw, &requests); err != nil {
		return b.unread("a message's tool requests have no shape this read knows, and were not read")
	}
	for _, r := range requests {
		b.called(r.ID, r.Name)
		if err := b.add(TranscriptEntry{Kind: EntryToolCall, At: at, Thread: b.thread, Tool: r.Name},
			compactJSON(r.Arguments)); err != nil {
			return err
		}
	}
	return nil
}

// copilotNotesHeld is the inbox ids of takes, in serve order and once each,
// whose note a session's events.jsonl shows reached the model as a current
// message: its `user.message` under its clientTurnId ([CopilotClientTurnID]),
// `steering` or `queued`, then a `session.usage_record` with no compaction
// between, since a queued note's line precedes its turn's compaction, which
// can leave it only in the summary (ADR-0177; measured on 1.0.114,
// compacted-queued). The line alone is no proof (ruling 1 of #1818's design):
// a kill or a refusal can follow it before any request. A last line with no
// newline is a write cut off; a line past the bound is no line.
func copilotNotesHeld(ctx context.Context, events io.Reader, takes []ServedTake) ([]string, error) {
	asked := make(map[string]string, len(takes))
	for _, take := range takes {
		asked[CopilotClientTurnID(take.InboxID)] = take.InboxID
	}
	pending := map[string]bool{}
	held := map[string]bool{}
	// moved is what the line read last moved to held, taken back where that
	// line turns out to be the file's unended last.
	var moved []string
	src := &lastByte{r: events}
	err := readLines(bufio.NewReaderSize(src, lineChunkBytes), maxLineBytes, func(line []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		moved = moved[:0]
		ev, ok := decodeCopilotNoteLine(line)
		if !ok {
			return nil
		}
		switch ev.Type {
		case "user.message":
			inbox, ours := asked[ev.Data.ClientTurnID]
			if ours && (ev.Data.Delivery == "steering" || ev.Data.Delivery == "queued") {
				pending[inbox] = true
			}
		case "session.usage_record":
			for inbox := range pending {
				if !held[inbox] {
					held[inbox] = true
					moved = append(moved, inbox)
				}
			}
			clear(pending)
		case "session.compaction_complete":
			// Past a compaction, a request may carry the note only inside
			// its summary: only one that says it failed keeps the note.
			if string(ev.Data.Success) != "false" {
				clear(pending)
			}
		}
		return nil
	}, func(int64) error { moved = moved[:0]; return nil })
	if err != nil {
		return nil, fmt.Errorf("executor: reading which notes the Copilot session holds: %w", err)
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

// copilotNoteLine is what [copilotNotesHeld] reads of one events.jsonl line:
// its own few fields, raw where Copilot's kind could vary, so that no other
// field of the line can lose them, as a field of another type loses the whole
// of [decodeCopilotEvent]'s.
type copilotNoteLine struct {
	Type string `json:"type"`
	Data struct {
		ClientTurnID string          `json:"clientTurnId"`
		Delivery     string          `json:"delivery"`
		Success      json.RawMessage `json:"success"`
	} `json:"data"`
}

// decodeCopilotNoteLine decodes one line for [copilotNotesHeld], false where
// it is no JSON object of a type. A line whose own fields are of another type
// is read for its type alone: a note line is then no note's, and a compaction
// one that applied.
func decodeCopilotNoteLine(line []byte) (copilotNoteLine, bool) {
	var ev copilotNoteLine
	if err := json.Unmarshal(line, &ev); err != nil {
		var typed struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(line, &typed) != nil || typed.Type == "" {
			return copilotNoteLine{}, false
		}
		return copilotNoteLine{Type: typed.Type}, true
	}
	return ev, ev.Type != ""
}

// lastByte is r, remembering the last byte it handed out.
type lastByte struct {
	r    io.Reader
	last byte
	read bool
}

func (l *lastByte) Read(p []byte) (int, error) {
	n, err := l.r.Read(p)
	if n > 0 {
		l.last, l.read = p[n-1], true
	}
	return n, err
}

// notesHeld is the inbox ids of takes ([Copilot.inboxID]) whose note
// the box's copy of session shows reached the model ([copilotNotesHeld]): its
// [recordConfirmer], as its extension confirms nothing (ruling 1 of #1818's
// design), which the Runner's main-thread read asks for the takes it is asked.
// An error where the record could not be read, which is neither held nor not.
func (Copilot) notesHeld(ctx context.Context, box sandbox.Sandbox, session Session,
	takes []ServedTake) ([]string, error) {
	events, err := box.OpenFile(ctx, PathsFor(box).CopilotSessionDir(session.ID)+"/events.jsonl")
	if err != nil {
		return nil, fmt.Errorf("executor: opening the Copilot session's record: %w", err)
	}
	defer func() { _ = events.Close() }()
	return copilotNotesHeld(ctx, events, takes)
}
