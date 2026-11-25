package executor

import (
	"archive/tar"
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
)

// wholeKimi reads a Kimi archive with the whole-line oracle
// (linewhole_internal_test.go).
type wholeKimi struct{ Kimi }

func (wholeKimi) transcript(ctx context.Context, b *transcriptBuilder, archive io.Reader) error {
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
		if err := b.kimiRecordWhole(ctx, thread, bufio.NewReaderSize(&b.handed, lineChunkBytes)); err != nil {
			return err
		}
	}
}

// piecesKimi reads a Kimi archive on the walk, its inflated bytes handed n a
// read.
type piecesKimi struct {
	Kimi
	n int
}

func (p piecesKimi) transcript(ctx context.Context, b *transcriptBuilder, archive io.Reader) error {
	return Kimi{}.transcript(ctx, b, &exportPieces{r: archive, n: p.n})
}

// kimiLineWhole is the part of one wire.jsonl line the whole-line read reads.
type kimiLineWhole struct {
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

// kimiRecordWhole reads one agent's wire.jsonl as entries of thread, a line
// held and decoded whole at a time.
func (b *transcriptBuilder) kimiRecordWhole(ctx context.Context, thread string, lines *bufio.Reader) error {
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
		var l kimiLineWhole
		if json.Unmarshal(line, &l) != nil || l.Type == "" {
			return b.notARecord(len(line))
		}
		b.thread = thread
		return b.kimiEntryWhole(l, line)
	}, func(n int64) error {
		return b.unread(fmt.Sprintf("a line of %s of the harness's record is past the %s one read reads, "+
			"and was not read", humanSize(n), humanSize(maxLineBytes)))
	})
}

// kimiEntryWhole adds the entries one wire.jsonl line makes.
func (b *transcriptBuilder) kimiEntryWhole(l kimiLineWhole, line []byte) error {
	at := time.UnixMilli(l.Time).UTC()
	switch l.Type {
	case "context.append_message":
		if l.Message.Role != "user" || l.Message.Origin.Kind != "user" {
			return nil
		}
		var raw struct {
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		_ = json.Unmarshal(line, &raw)
		text, lost := joinedWhole(raw.Message.Content, kimiParts, b.valueMax)
		if lost {
			return b.lostJoin()
		}
		return b.kimiWordsWhole(text, at)
	case "context.append_loop_event":
		ev := l.Event
		switch ev.Type {
		case "content.part":
			if ev.Part.Type == "text" && strings.TrimSpace(ev.Part.Text) != "" {
				return b.add(TranscriptEntry{Kind: EntryModel, At: at, Thread: b.thread}, ev.Part.Text)
			}
		case "tool.call":
			if b.refusedName(ev.Name) {
				return b.add(TranscriptEntry{Kind: EntryUnread, Thread: b.thread}, unredactedText)
			}
			b.called(ev.ToolCallID, ev.Name)
			return b.add(TranscriptEntry{Kind: EntryToolCall, At: at, Thread: b.thread, Tool: ev.Name},
				compactJSON(ev.Args))
		case "tool.result":
			tool := b.answered(ev.ToolCallID)
			text, lost := kimiOutputWhole(ev.Result.Output, b.valueMax)
			if lost {
				return b.lostJoin()
			}
			return b.add(TranscriptEntry{Kind: EntryToolResult, At: at, Thread: b.thread,
				Tool: tool, Failed: ev.Result.IsError}, text)
		}
	case "context.apply_compaction":
		if strings.TrimSpace(l.Summary) != "" {
			return b.add(TranscriptEntry{Kind: EntrySummary, At: at, Thread: b.thread}, l.Summary)
		}
	}
	return nil
}

// kimiWordsWhole adds a person's message: each note, late call and wake it
// carries where it opens with an engine heading, else the brief; past the
// value bound the brief (R1).
func (b *transcriptBuilder) kimiWordsWhole(text string, at time.Time) error {
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

// kimiOutputWhole is a tool result's output as text: a string as it is, or the
// text parts of a list (an MCP tool's), else the value as compact JSON; and
// whether the parts' join is lost (R2).
func kimiOutputWhole(raw json.RawMessage, valueMax int) (string, bool) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, false
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		return joinedWhole(raw, kimiParts, valueMax)
	}
	return compactJSON(raw), false
}

// kimiNotesHeldWhole is the whole-line read of the notes a wire.jsonl holds.
func kimiNotesHeldWhole(ctx context.Context, wire io.Reader, takes []ServedTake) ([]string, error) {
	asked := make(map[string]string, len(takes))
	for _, take := range takes {
		asked[KimiNoteID(take.InboxID)] = take.InboxID
	}
	pending := map[string]bool{}
	held := map[string]bool{}
	var moved []string
	src := &lastByte{r: wire}
	err := readLines(bufio.NewReaderSize(src, lineChunkBytes), maxLineBytes, func(line []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		moved = moved[:0]
		var l struct {
			Type    string `json:"type"`
			Message struct {
				ID   string `json:"id"`
				Role string `json:"role"`
			} `json:"message"`
			UsageScope string `json:"usageScope"`
		}
		if err := json.Unmarshal(line, &l); err != nil {
			var typed struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(line, &typed) != nil {
				typed.Type = ""
			}
			l.Type, l.Message.ID, l.Message.Role, l.UsageScope = typed.Type, "", "", ""
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
		return nil, err
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
