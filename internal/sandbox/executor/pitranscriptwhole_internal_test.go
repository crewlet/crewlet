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

// wholePi reads a Pi archive with the whole-line oracle
// (linewhole_internal_test.go).
type wholePi struct{ Pi }

func (wholePi) transcript(ctx context.Context, b *transcriptBuilder, archive io.Reader) error {
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
		if !piSessionMember(h.Name) {
			continue
		}
		if h.Typeflag != tar.TypeReg {
			if err := b.unread("a transcript of the harness's record is not a plain file in the archive, " +
				"and was not read"); err != nil {
				return err
			}
			continue
		}
		if err := b.piRecordWhole(ctx, bufio.NewReaderSize(&b.handed, lineChunkBytes)); err != nil {
			return err
		}
	}
}

// piecesPi reads a Pi archive on the walk, its inflated bytes handed n a read.
type piecesPi struct {
	Pi
	n int
}

func (p piecesPi) transcript(ctx context.Context, b *transcriptBuilder, archive io.Reader) error {
	return Pi{}.transcript(ctx, b, &exportPieces{r: archive, n: p.n})
}

// piTextWhole is raw's text blocks joined as [piText] joins them, past the
// value bound as the walk joins them (R2), and whether that join is lost; a
// join of blank words alone is none.
func piTextWhole(raw json.RawMessage, valueMax int) (string, bool) {
	text, lost := piText(raw), false
	if trimmed := bytes.TrimSpace(raw); len(trimmed) > 0 && trimmed[0] == '[' && piBlocks(raw) != nil {
		text, lost = joinedWhole(raw, piTextBlocks, valueMax)
	}
	if strings.TrimSpace(text) == "" {
		text = ""
	}
	return text, lost
}

// The whole-line read of a session file, a person's and a collection's.

func (b *transcriptBuilder) piRecordWhole(ctx context.Context, lines *bufio.Reader) error {
	read := 0
	return readLines(lines, maxLineBytes, func(line []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if read++; read > b.bounds.ReadLines {
			return b.linesPast("record", "lines")
		}
		return b.piLineWhole(line)
	}, func(n int64, _, _ []byte) error {
		return b.unread(fmt.Sprintf("a line of %s of the harness's record is past the %s one read reads, "+
			"and was not read", humanSize(n), humanSize(maxLineBytes)))
	})
}

func (b *transcriptBuilder) piLineWhole(line []byte) error {
	if len(strings.TrimSpace(string(line))) == 0 {
		return nil
	}
	var e piEntry
	if err := json.Unmarshal(line, &e); err != nil || e.Type == "" {
		return b.notARecord(len(line))
	}
	at := claudeTime(e.Timestamp)
	switch e.Type {
	case "message":
		if e.Message == nil {
			return nil
		}
		m := *e.Message
		switch m.Role {
		case "user":
			text, lost := piTextWhole(m.Content, b.valueMax)
			if lost {
				return b.lostJoin()
			}
			if strings.TrimSpace(text) != "" {
				return b.add(TranscriptEntry{Kind: EntryBrief, At: at}, text)
			}
		case "assistant":
			return b.piAssistantWhole(m, at)
		case "toolResult":
			tool := b.answered(m.ToolCallID)
			text, lost := piTextWhole(m.Content, b.valueMax)
			if lost {
				return b.lostJoin()
			}
			return b.add(TranscriptEntry{Kind: EntryToolResult, At: at, Tool: tool, Failed: m.IsError}, text)
		}
	case "custom_message":
		if e.CustomType != piNoteType {
			return nil
		}
		var text string
		if json.Unmarshal(e.Content, &text) != nil {
			var lost bool
			if text, lost = piTextWhole(e.Content, b.valueMax); lost {
				return b.lostJoin()
			}
		}
		if strings.TrimSpace(text) == "" {
			return nil
		}
		if len(text) > b.valueMax || !opensWithHeading(text) {
			return b.add(TranscriptEntry{Kind: EntryNote, At: at}, text)
		}
		for kind, words := range headedIn(text) {
			if err := b.add(TranscriptEntry{Kind: kind, At: at}, words); err != nil {
				return err
			}
		}
	case "compaction":
		if strings.TrimSpace(e.Summary) != "" {
			return b.add(TranscriptEntry{Kind: EntrySummary, At: at}, e.Summary)
		}
	}
	return nil
}

func (b *transcriptBuilder) piAssistantWhole(m piMessage, at time.Time) error {
	blocks := piBlocks(m.Content)
	text, lost := piTextWhole(m.Content, b.valueMax)
	switch {
	case lost:
		if err := b.lostJoin(); err != nil {
			return err
		}
	case strings.TrimSpace(text) != "":
		if err := b.add(TranscriptEntry{Kind: EntryModel, At: at}, text); err != nil {
			return err
		}
	}
	for _, c := range blocks {
		if c.Type != "toolCall" {
			continue
		}
		if b.refusedName(c.Name) {
			if err := b.add(TranscriptEntry{Kind: EntryUnread}, unredactedText); err != nil {
				return err
			}
			continue
		}
		b.called(c.ID, c.Name)
		if err := b.add(TranscriptEntry{Kind: EntryToolCall, At: at, Tool: c.Name}, compactJSON(c.Arguments)); err != nil {
			return err
		}
	}
	return nil
}

type piNodeWhole struct {
	parent string
	kind   piNodeKind
	inbox  string
	target string
}

func piNotesHeldWhole(ctx context.Context, r io.Reader, takes []ServedTake) ([]string, error) {
	asked := make(map[string]bool, len(takes))
	for _, take := range takes {
		asked[take.InboxID] = true
	}
	nodes := map[string]piNodeWhole{}
	leaf, held := "", ""
	src := &piTail{r: r}
	err := readLines(bufio.NewReaderSize(src, lineChunkBytes), maxLineBytes, func(line []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		// The line read last is held back until another follows or the
		// file is known to end in a break.
		if held != "" {
			leaf = held
			held = ""
		}
		e, ok := piEntryOfWhole(line)
		if !ok || e.ID == "" || e.Type == "session" {
			return nil
		}
		n := piNodeWhole{parent: e.ParentID}
		switch {
		case e.Type == "custom_message" && e.CustomType == piNoteType && asked[e.inbox()]:
			n.kind, n.inbox = piNote, e.inbox()
		case e.Type == "message" && e.Message != nil && e.Message.Role == "assistant" &&
			piAnswered(e.Message.StopReason):
			n.kind = piAnswer
		case e.Type == "compaction":
			n.kind = piCompaction
		case e.Type == "context_edit":
			n.kind, n.target = piEdit, e.TargetID
		}
		nodes[e.ID] = n
		held = e.ID
		return nil
	}, func(int64, []byte, []byte) error { return nil })
	if err != nil {
		return nil, fmt.Errorf("executor: reading which notes the Pi session holds: %w", err)
	}
	if held != "" && src.last == '\n' {
		leaf = held
	}
	// The branch, root first.
	var branch []piNodeWhole
	seen := map[string]bool{}
	for id := leaf; id != "" && !seen[id]; {
		n, ok := nodes[id]
		if !ok {
			break
		}
		seen[id] = true
		branch = append(branch, n)
		id = n.parent
	}
	slices.Reverse(branch)
	pending := map[string]bool{}
	confirmed := map[string]bool{}
	for _, n := range branch {
		switch n.kind {
		case piNote:
			pending[n.inbox] = true
		case piAnswer:
			for inbox := range pending {
				confirmed[inbox] = true
			}
			clear(pending)
		case piCompaction:
			clear(pending)
		case piEdit:
			for inbox := range pending {
				if nodes[n.target].inbox == inbox && nodes[n.target].kind == piNote {
					delete(pending, inbox)
				}
			}
		}
	}
	var out []string
	for _, take := range takes {
		if confirmed[take.InboxID] && !slices.Contains(out, take.InboxID) {
			out = append(out, take.InboxID)
		}
	}
	return out, nil
}

func piEntryOfWhole(line []byte) (piEntry, bool) {
	var e piEntry
	return e, json.Unmarshal(line, &e) == nil
}
