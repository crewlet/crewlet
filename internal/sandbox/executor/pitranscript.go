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

// transcript reads a Pi archive ([piArchiveFormat]): its session file, a line
// at a time, as the session's entries. Any other member is passed unread.
func (Pi) transcript(ctx context.Context, b *transcriptBuilder, archive io.Reader) error {
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
		if err := b.piRecord(ctx, bufio.NewReaderSize(&b.handed, lineChunkBytes)); err != nil {
			return err
		}
	}
}

// piSessionMember reports whether an archive's member named name is a
// session's file: `.pi/sessions/<session>.jsonl`.
func piSessionMember(name string) bool {
	parts := strings.Split(path.Clean(name), "/")
	return len(parts) == 3 && parts[0] == ".pi" && parts[1] == "sessions" &&
		strings.HasSuffix(parts[2], ".jsonl") && parts[2] != ".jsonl"
}

// piRecord reads one session file as entries, in recorded order:
//   - a user message is the brief;
//   - a note the extension handed over (a `crewlet-note` custom message) is
//     each note, late call and wake it carries ([headedIn]);
//   - an assistant message is the model's words and a call per tool call;
//   - a tool result its result, failed where pi recorded it so;
//   - a compaction its summary.
//
// A line that is no record, or one past the line bound, is said where it was.
func (b *transcriptBuilder) piRecord(ctx context.Context, lines *bufio.Reader) error {
	read := 0
	return readLines(lines, maxLineBytes, func(line []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if read++; read > b.bounds.ReadLines {
			return b.linesPast("record", "lines")
		}
		return b.piLine(line)
	}, func(n int64) error {
		return b.unread(fmt.Sprintf("a line of %s of the harness's record is past the %s one read reads, "+
			"and was not read", humanSize(n), humanSize(maxLineBytes)))
	})
}

// piLine adds the entries one line of a session file makes.
func (b *transcriptBuilder) piLine(line []byte) error {
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
			if text := piText(m.Content); strings.TrimSpace(text) != "" {
				return b.add(TranscriptEntry{Kind: EntryBrief, At: at}, text)
			}
		case "assistant":
			return b.piAssistant(m, at)
		case "toolResult":
			return b.add(TranscriptEntry{Kind: EntryToolResult, At: at, Tool: b.answered(m.ToolCallID),
				Failed: m.IsError}, piText(m.Content))
		}
	case "custom_message":
		if e.CustomType != piNoteType {
			return nil
		}
		var text string
		if json.Unmarshal(e.Content, &text) != nil {
			text = piText(e.Content)
		}
		if strings.TrimSpace(text) == "" {
			return nil
		}
		if !opensWithHeading(text) {
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

// piAssistant adds an assistant message: its words, then a call per tool call
// (`toolCall{id, name, arguments}`), each remembered for its result.
func (b *transcriptBuilder) piAssistant(m piMessage, at time.Time) error {
	blocks := piBlocks(m.Content)
	if text := piText(m.Content); strings.TrimSpace(text) != "" {
		if err := b.add(TranscriptEntry{Kind: EntryModel, At: at}, text); err != nil {
			return err
		}
	}
	for _, c := range blocks {
		if c.Type != "toolCall" {
			continue
		}
		b.called(c.ID, c.Name)
		if err := b.add(TranscriptEntry{Kind: EntryToolCall, At: at, Tool: c.Name}, compactJSON(c.Arguments)); err != nil {
			return err
		}
	}
	return nil
}

// piNode is what [piNotesHeld] keeps of one session entry: its parent, and
// what it is to a note's confirmation.
type piNode struct {
	parent string
	kind   piNodeKind
	inbox  string
	target string
}

type piNodeKind uint8

const (
	// piNote is a note the extension handed over, under its inbox id. The
	// zero kind is an entry of no other kind.
	piNote piNodeKind = iota + 1
	// piAnswer is an assistant message that answered (`stop`, `toolUse`,
	// `length`).
	piAnswer
	// piCompaction is a compaction: past it, a request carries what lay
	// before it only inside its summary.
	piCompaction
	// piEdit is a context_edit, which takes its target out of what the model
	// reads, or replaces it.
	piEdit
)

// piNotesHeld is the inbox ids of takes, in serve order and once each, whose
// note a session file shows reached the model as a current message (ADR-0177):
// on the session's ACTIVE BRANCH (its last entry and every parent back to the
// root), a `crewlet-note` custom message under the take's inbox id, then an
// assistant message that answered, with no compaction between them (one would
// have left the note only inside its summary) and no context_edit taking the
// note out first. A note with no answer after it (a run killed or refused
// after the hand-over, an error or an abort) is not held, so it is served
// again. A last line with no newline is a write cut off and decides nothing;
// a line of no entry read is skipped, which can only cut the branch short and
// hold less.
func piNotesHeld(ctx context.Context, r io.Reader, takes []ServedTake) ([]string, error) {
	asked := make(map[string]bool, len(takes))
	for _, take := range takes {
		asked[take.InboxID] = true
	}
	nodes := map[string]piNode{}
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
		e, ok := piEntryOf(line)
		if !ok || e.ID == "" || e.Type == "session" {
			return nil
		}
		n := piNode{parent: e.ParentID}
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
	}, func(int64) error { return nil })
	if err != nil {
		return nil, fmt.Errorf("executor: reading which notes the Pi session holds: %w", err)
	}
	if held != "" && src.last == '\n' {
		leaf = held
	}
	// The branch, root first.
	var branch []piNode
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

// piEntryOf decodes one session line, false for a line that is no entry: one
// the confirm reader skips, which can only cut the branch it walks short, so
// it never holds a note it would not hold without it.
func piEntryOf(line []byte) (piEntry, bool) {
	var e piEntry
	return e, json.Unmarshal(line, &e) == nil
}

// piTail is r, remembering the last byte it handed out.
type piTail struct {
	r    io.Reader
	last byte
}

func (t *piTail) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 {
		t.last = p[n-1]
	}
	return n, err
}

// notesHeld is the inbox ids of takes ([Pi.inboxID]) whose note the box's copy
// of session shows reached the model ([piNotesHeld]): its [recordConfirmer],
// as its extension confirms nothing. An error where the record could not be
// read, which is neither held nor not.
func (Pi) notesHeld(ctx context.Context, box sandbox.Sandbox, session Session, takes []ServedTake) ([]string, error) {
	f, err := box.OpenFile(ctx, PathsFor(box).PiSessionFile(session.ID))
	if err != nil {
		return nil, fmt.Errorf("executor: opening the Pi session's record: %w", err)
	}
	defer func() { _ = f.Close() }()
	return piNotesHeld(ctx, f, takes)
}
