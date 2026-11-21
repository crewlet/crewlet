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
	b.thread = data.AgentID
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

// copilotNotesHeld is, of the inbox ids asked, those whose note Copilot's own
// record shows reached the model, read off a session's events.jsonl: the
// note's `user.message` under its clientTurnId ([CopilotClientTurnID]),
// delivered `steering` or `queued`, AND a `session.usage_record` after it — a
// model request made once it was in the conversation.
//
// THE LINE ALONE IS NO PROOF (ruling 1 of #1818's design, measured on 1.0.114):
// Copilot writes the line before the request it rides, flushing about every
// 100 ms, so a process killed in that gap leaves the line and no request; and a
// request refused before it was sent (past the model's prompt budget) leaves
// the line and a `session.error`, no record. Either way the note is not held,
// and is served again. A line past the line bound is read as no line.
func copilotNotesHeld(ctx context.Context, events io.Reader, inboxes []string) (map[string]bool, error) {
	asked := make(map[string]string, len(inboxes))
	for _, inbox := range inboxes {
		asked[CopilotClientTurnID(inbox)] = inbox
	}
	pending := map[string]bool{}
	held := map[string]bool{}
	err := readLines(bufio.NewReaderSize(events, lineChunkBytes), maxLineBytes, func(line []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		ev, ok := decodeCopilotEvent(line)
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
				held[inbox] = true
			}
			clear(pending)
		}
		return nil
	}, func(int64) error { return nil })
	if err != nil {
		return nil, fmt.Errorf("executor: reading which notes the Copilot session holds: %w", err)
	}
	return held, nil
}

// notesHeld is, of the inbox ids asked ([Copilot.inboxID]), those whose note
// the box's copy of session shows reached the model ([copilotNotesHeld]): the
// confirm path of a runner whose extension never confirms (ruling 1 of #1818's
// design), read off the record by the runner, as a hook runner's receipts are
// read off its main thread. An error where the record could not be read, which
// is neither held nor not.
func (Copilot) notesHeld(ctx context.Context, box sandbox.Sandbox, session string,
	inboxes []string) (map[string]bool, error) {
	events, err := box.OpenFile(ctx, PathsFor(box).CopilotSessionDir(session)+"/events.jsonl")
	if err != nil {
		return nil, fmt.Errorf("executor: opening the Copilot session's record: %w", err)
	}
	defer func() { _ = events.Close() }()
	return copilotNotesHeld(ctx, events, inboxes)
}
