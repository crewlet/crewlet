package executor

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
)

// wholeCopilot reads a Copilot archive with the whole-line oracle
// (linewhole_internal_test.go).
type wholeCopilot struct{ Copilot }

func (wholeCopilot) transcript(ctx context.Context, b *transcriptBuilder, archive io.Reader) error {
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
		if err := b.copilotRecordWhole(ctx, bufio.NewReaderSize(&b.handed, lineChunkBytes)); err != nil {
			return err
		}
	}
}

// piecesCopilot reads a Copilot archive on the walk, its inflated bytes handed
// n a read.
type piecesCopilot struct {
	Copilot
	n int
}

func (p piecesCopilot) transcript(ctx context.Context, b *transcriptBuilder, archive io.Reader) error {
	return Copilot{}.transcript(ctx, b, &exportPieces{r: archive, n: p.n})
}

// decodeCopilotNoteLine reads line, one line of an events.jsonl, as
// [copilotNotesHeld] reads each.
func decodeCopilotNoteLine(line []byte) (copilotNoteLine, bool) {
	var w lineWalk
	w.setupNotes(bufio.NewReader(bytes.NewReader(line)))
	var ev copilotNoteLine
	read, err := w.noteLine(context.Background(), func() error { return ev.read(&w) })
	if err != nil {
		return copilotNoteLine{}, false
	}
	return ev, ev.decoded(read)
}

// The whole-line read of an events.jsonl, a person's and a collection's.

func (b *transcriptBuilder) copilotRecordWhole(ctx context.Context, lines *bufio.Reader) error {
	read := 0
	err := readLines(lines, maxLineBytes, func(line []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if read++; read > b.bounds.ReadLines {
			return b.linesPast("record", "lines")
		}
		return b.copilotLineWhole(line)
	}, func(n int64) error {
		return b.unread(fmt.Sprintf("a line of %s of the harness's record is past the %s one read reads, "+
			"and was not read", humanSize(n), humanSize(maxLineBytes)))
	})
	return err
}

func (b *transcriptBuilder) copilotLineWhole(line []byte) error {
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
		return b.copilotWordsWhole(data.Content, at)
	case "assistant.message":
		if strings.TrimSpace(data.Content) != "" {
			if err := b.add(TranscriptEntry{Kind: EntryModel, At: at, Thread: b.thread}, data.Content); err != nil {
				return err
			}
		}
		return b.copilotCallsWhole(data.ToolRequests, at)
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

func (b *transcriptBuilder) copilotWordsWhole(text string, at time.Time) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	if len(text) > b.valueMax || !opensWithHeading(text) {
		return b.add(TranscriptEntry{Kind: EntryBrief, At: at, Thread: b.thread}, text)
	}
	for kind, words := range headedIn(text) {
		if err := b.add(TranscriptEntry{Kind: kind, At: at, Thread: b.thread}, words); err != nil {
			return err
		}
	}
	return nil
}

func (b *transcriptBuilder) copilotCallsWhole(raw json.RawMessage, at time.Time) error {
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
		if b.refusedName(r.Name) {
			if err := b.add(TranscriptEntry{Kind: EntryUnread, Thread: b.thread}, unredactedText); err != nil {
				return err
			}
			continue
		}
		b.called(r.ID, r.Name)
		if err := b.add(TranscriptEntry{Kind: EntryToolCall, At: at, Thread: b.thread, Tool: r.Name},
			compactJSON(r.Arguments)); err != nil {
			return err
		}
	}
	return nil
}

func copilotNotesHeldWhole(ctx context.Context, events io.Reader, takes []ServedTake) ([]string, error) {
	asked := make(map[string]string, len(takes))
	for _, take := range takes {
		asked[CopilotClientTurnID(take.InboxID)] = take.InboxID
	}
	// pending is each note sent and not yet held, by its line; window the
	// lines that may be messages the main agent's model reads, and retained
	// the notes the compaction started last carries over whole.
	pending := map[string]copilotPending{}
	var window copilotWindowWhole
	var retained map[string]bool
	held := map[string]bool{}
	n := 0
	// moved is what the line read last moved to held, taken back where that
	// line turns out to be the file's unended last.
	var moved []string
	src := &lastByte{r: events}
	err := readLines(bufio.NewReaderSize(src, lineChunkBytes), maxLineBytes, func(line []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		n++
		moved = moved[:0]
		ev, ok := decodeCopilotNoteLineWhole(line)
		window.read(n, ev, ok)
		if !ok {
			return nil
		}
		// A note and a request are the main agent's only where the line names
		// no agent at all: the window counts more lines than this holds.
		switch ev.Type {
		case "user.message":
			if ev.AgentID != "" {
				return nil
			}
			inbox, ours := asked[ev.Data.ClientTurnID]
			at, timed := copilotTime(ev.Timestamp)
			// A line whose time cannot be read is one no request can be
			// shown to have begun after.
			if ours && timed && (ev.Data.Delivery == "steering" || ev.Data.Delivery == "queued") {
				pending[inbox] = copilotPending{line: n, at: at}
			}
		case "session.usage_record":
			// A FAILED MAIN REQUEST WRITES NONE (measured on 1.0.114: a
			// provider answering 500 past its five retries left a
			// session.error and no usage record), so it settles nothing.
			if ev.AgentID != "" || ev.Data.Usage.InteractionType != copilotMainRequest {
				return nil
			}
			began, ok := copilotBeganWhole(ev)
			if !ok {
				return nil
			}
			for inbox, note := range pending {
				if note.at.After(began) { //fixedwait:stated the note's record stamp against its request's record stamp
					continue
				}
				delete(pending, inbox)
				if !held[inbox] {
					held[inbox] = true
					moved = append(moved, inbox)
				}
			}
		case "session.compaction_start":
			retained = copilotRetained(pending, window.messages, looseInt(ev.Data.RetainedInputMessages))
		case "session.compaction_complete":
			// Past a compaction, a request may carry the note only inside
			// its summary: only one that says it failed keeps every note, and
			// one that applied those it carried over whole.
			if string(ev.Data.Success) != "false" {
				for inbox := range pending {
					if !retained[inbox] {
						delete(pending, inbox)
					}
				}
			}
			retained = nil
		}
		return nil
	}, func(int64) error {
		n++
		moved = moved[:0]
		window.read(n, copilotNoteLineWhole{}, false)
		return nil
	})
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

func (w *copilotWindowWhole) read(n int, ev copilotNoteLineWhole, ok bool) {
	if ok && ev.Type == "subagent.started" && ev.AgentID != "" {
		if w.announced == nil {
			w.announced = map[string]bool{}
		}
		w.announced[ev.AgentID] = true
	}
	if ok && (w.announced[ev.AgentID] || ev.Type == "session.compaction_start" || !copilotMessageWhole(ev)) {
		return
	}
	w.messages = append(w.messages, n)
}

func copilotBeganWhole(ev copilotNoteLineWhole) (time.Time, bool) {
	at, ok := copilotTime(ev.Timestamp)
	raw := ev.Data.Usage.Duration
	if !ok || len(raw) == 0 || raw[0] < '0' || raw[0] > '9' {
		return time.Time{}, false
	}
	var ms float64
	if json.Unmarshal(raw, &ms) != nil {
		return time.Time{}, false
	}
	return at.Add(-time.Duration(ms * float64(time.Millisecond))), true
}

type copilotNoteLineWhole struct {
	Type      string `json:"type"`
	AgentID   string `json:"agentId"`
	Timestamp string `json:"timestamp"`
	Data      struct {
		ClientTurnID string          `json:"clientTurnId"`
		Delivery     string          `json:"delivery"`
		Success      json.RawMessage `json:"success"`
		// RetainedInputMessages, on a compaction's start: "Number of
		// trailing conversation messages (the turn's not-yet-answered input)
		// kept verbatim after the summary instead of being compacted.
		// Omitted when every message at compaction start is summarized."
		// (schemas/session-events.schema.json, 1.0.114's bundle.)
		RetainedInputMessages json.RawMessage `json:"retainedInputMessages"`
		Usage                 struct {
			InteractionType string          `json:"interactionType"`
			Duration        json.RawMessage `json:"duration"`
		} `json:"usage"`
	} `json:"data"`
	// bareHook is a `hook.end` whose data holds only its id, its kind and its
	// success: a hook that handed the model nothing.
	bareHook bool
}

func decodeCopilotNoteLineWhole(line []byte) (copilotNoteLineWhole, bool) {
	var ev copilotNoteLineWhole
	if err := json.Unmarshal(line, &ev); err != nil {
		var typed struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(line, &typed) != nil || typed.Type == "" {
			return copilotNoteLineWhole{}, false
		}
		return copilotNoteLineWhole{Type: typed.Type}, true
	}
	if ev.Type == "hook.end" {
		var hook struct {
			Data map[string]json.RawMessage `json:"data"`
		}
		ev.bareHook = json.Unmarshal(line, &hook) == nil
		for field := range hook.Data {
			if field != "hookInvocationId" && field != "hookType" && field != "success" {
				ev.bareHook = false
			}
		}
	}
	return ev, ev.Type != ""
}

type copilotWindowWhole struct {
	messages  []int
	announced map[string]bool
}

func copilotMessageWhole(ev copilotNoteLineWhole) bool {
	switch ev.Type {
	case "hook.start", "assistant.turn_start", "assistant.turn_end", "tool.execution_start",
		"session.start", "session.permissions_changed", "session.usage_record", "session.info":
		return false
	case "hook.end":
		return !ev.bareHook
	}
	return true
}
