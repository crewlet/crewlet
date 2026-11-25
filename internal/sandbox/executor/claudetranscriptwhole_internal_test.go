package executor

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// THE WHOLE-LINE READ OF A CLAUDE CODE TRANSCRIPT, the oracle a person's read on
// the scanner ([transcriptBuilder.claudeArchive]) is held to: each line held
// and decoded whole, as the read was before it moved onto the scanner, with
// what the scanner decides applied where it decides it. A line is one JSON text
// with JSON's blanks around it (D2); a heading written with escapes is read
// (D3); a line that is no record is sized as written (D4); and past the value
// bound R1, R2, R4 and R6 (opencodeperson.go names each), and the lines bound,
// counted as the scanner frames them.

// wholeClaudeCode reads a Claude Code archive with the whole-line oracle.
type wholeClaudeCode struct{ ClaudeCode }

func (wholeClaudeCode) transcript(ctx context.Context, b *transcriptBuilder, archive io.Reader) error {
	return b.claudeArchiveWhole(ctx, archive)
}

// piecesClaudeCode reads a Claude Code archive on the scanner, its inflated
// bytes handed n a read.
type piecesClaudeCode struct {
	ClaudeCode
	n int
}

func (p piecesClaudeCode) transcript(ctx context.Context, b *transcriptBuilder, archive io.Reader) error {
	return b.claudeArchive(ctx, &exportPieces{r: archive, n: p.n})
}

// claudeLine reads line, a member of one line, on the scanner.
func (b *transcriptBuilder) claudeLine(ctx context.Context, line []byte) error {
	return b.claudeMember(ctx, bytes.NewReader(line))
}

// claudeRecord is the part of one line of Claude Code's transcript a read
// takes, as its one decode leaves it ([wireDecode]), each key by its exact
// name and raw, and read loosely by type.
type claudeRecord struct {
	Type             json.RawMessage `json:"type"`
	Timestamp        json.RawMessage `json:"timestamp"`
	IsMeta           json.RawMessage `json:"isMeta"`
	IsCompactSummary json.RawMessage `json:"isCompactSummary"`
	Message          json.RawMessage `json:"message"`
	Attachment       json.RawMessage `json:"attachment"`
}

// claudeArchiveWhole reads the session's transcript and each subagent's, a
// member at a time, a line held whole at a time.
func (b *transcriptBuilder) claudeArchiveWhole(ctx context.Context, archive io.Reader) error {
	tr := tar.NewReader(archive)
	b.handed.r = tr
	lines := bufio.NewReaderSize(&b.handed, lineChunkBytes)
	framed := 0
	for {
		h, err := tr.Next()
		switch {
		case errors.Is(err, io.EOF):
			return nil
		case err != nil:
			return fmt.Errorf("%w: %w", errNotTar, err)
		}
		thread, ok := claudeThread(h.Name)
		if !ok {
			continue
		}
		b.thread = thread
		clear(b.calls)
		if h.Typeflag != tar.TypeReg {
			if unreadErr := b.unread("a transcript of the harness's record is not a plain " +
				"file in the archive, and was not read"); unreadErr != nil {
				return unreadErr
			}
			continue
		}
		lines.Reset(&b.handed)
		for {
			raw, readErr := lines.ReadBytes('\n')
			switch {
			case readErr != nil && !errors.Is(readErr, io.EOF):
				return readErr
			case len(raw) == 0:
			default:
				if lineErr := b.claudeLineWhole(ctx, bytes.TrimSuffix(raw, []byte{'\n'}), &framed); lineErr != nil {
					return lineErr
				}
			}
			if readErr != nil {
				break
			}
		}
	}
}

// claudeLineWhole reads one record held whole, after counting what the scanner
// frames of it ([claudeFramed]).
func (b *transcriptBuilder) claudeLineWhole(ctx context.Context, raw []byte, framed *int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	line := bytes.Trim(raw, " \t\r\n")
	if *framed += claudeFramed(line); *framed > b.bounds.ReadLines {
		return b.linesPast("harness's record", "lines, blocks and parts")
	}
	if len(line) == 0 {
		return nil
	}
	if line[0] != '{' {
		return b.notARecord(len(raw))
	}
	var rec claudeRecord
	if wireDecode(line, &rec) != nil {
		return b.notARecord(len(raw))
	}
	at := claudeTime(b.wordOf(rec.Timestamp))
	switch record := b.wordOf(rec.Type); {
	case record == "assistant":
		return b.claudeMessageWhole(ctx, rec.Message, EntryModel, at)
	case record == "user" && !looseTrue(rec.IsMeta):
		kind := EntryBrief
		if looseTrue(rec.IsCompactSummary) {
			kind = EntrySummary
		}
		return b.claudeMessageWhole(ctx, rec.Message, kind, at)
	case record == "attachment":
		if line, ok := b.hookLineWhole(rec.Attachment); ok {
			return b.add(TranscriptEntry{Kind: hookLineKind(line), At: at, Thread: b.thread}, line)
		}
	}
	return b.claudeNotesWhole(ctx, rec.Attachment, at)
}

// hookLineWhole is the line the run's own hook said that a main thread's
// `hook_system_message` attachment holds, read whole: its content as the hook
// said it, its first line, within the value bound.
func (b *transcriptBuilder) hookLineWhole(attachment json.RawMessage) (string, bool) {
	var said struct {
		Type    json.RawMessage `json:"type"`
		Content json.RawMessage `json:"content"`
	}
	var content string
	if b.thread != "" || wireDecode(attachment, &said) != nil || b.wordOf(said.Type) != hookSystemMessage ||
		wireDecode(said.Content, &content) != nil || len(content) > b.valueMax {
		return "", false
	}
	hooked, ok := claudeHookSaid(content)
	line, _, _ := strings.Cut(strings.TrimSpace(hooked), "\n")
	line = strings.TrimSpace(line)
	return line, ok && line != ""
}

// wordOf is raw's string where it is one within [transcriptNameMaxBytes], the
// longest word the reader holds, else "".
func (b *transcriptBuilder) wordOf(raw json.RawMessage) string {
	if word := looseString(raw); len(word) <= transcriptNameMaxBytes {
		return word
	}
	return ""
}

// claudeFramed is how many lines and elements the scanner frames for line:
// the line, every element of each `content` list of each `message` object of
// the record, and every element of each `content` list of each of those that
// is an object, every key written twice among them, up to where the line
// stops being one JSON value.
func claudeFramed(line []byte) int {
	n := 1
	dec := jsontext.NewDecoder(bytes.NewReader(line), threadDecoding)
	framed := map[string]bool{"/message/content": true, "/message/content/*/content": true}
	var walk func(path string, counted bool) error
	walk = func(path string, counted bool) error {
		tok, err := dec.ReadToken()
		if err != nil {
			return err
		}
		if counted {
			n++
		}
		switch tok.Kind() {
		case '{':
			for dec.PeekKind() != '}' {
				name, nameErr := dec.ReadToken()
				if nameErr != nil {
					return nameErr
				}
				if err = walk(path+"/"+name.String(), false); err != nil {
					return err
				}
			}
		case '[':
			for dec.PeekKind() != ']' {
				if err = walk(path+"/*", framed[path]); err != nil {
					return err
				}
			}
		default:
			return nil
		}
		_, err = dec.ReadToken()
		return err
	}
	_ = walk("", false)
	return n
}

// claudeNotesWhole adds an entry for every note and late call a record's
// attachment hands its model, where it is a hook_additional_context: every
// string and name of its content, in order; one past the value bound names
// none (R1).
func (b *transcriptBuilder) claudeNotesWhole(ctx context.Context, attachment json.RawMessage, at time.Time) error {
	var handed struct {
		Type    json.RawMessage `json:"type"`
		Content json.RawMessage `json:"content"`
	}
	// The line decoded whole, so only an attachment that is no object fails
	// here, and it names no type: it hands nothing, as the reader's does.
	_ = wireDecode(attachment, &handed)
	if b.wordOf(handed.Type) != hookAdditionalContext {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(handed.Content))
	for tok, err := dec.Token(); err == nil; tok, err = dec.Token() {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		text, ok := tok.(string)
		if !ok || len(text) > b.valueMax {
			continue
		}
		if notesErr := b.headed(ctx, at, text); notesErr != nil {
			return notesErr
		}
	}
	return nil
}

// headed adds an entry for every note and late call text carries ([headedIn]).
func (b *transcriptBuilder) headed(ctx context.Context, at time.Time, text string) error {
	for kind, words := range headedIn(text) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := b.add(TranscriptEntry{Kind: kind, At: at, Thread: b.thread}, words); err != nil {
			return err
		}
	}
	return nil
}

// prose adds one entry of words unless it holds none; a brief that opens with
// a heading is what it carries, each by its kind.
func (b *transcriptBuilder) prose(ctx context.Context, kind EntryKind, at time.Time, text string) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	if kind == EntryBrief && opensWithHeading(text) {
		return b.headed(ctx, at, text)
	}
	return b.add(TranscriptEntry{Kind: kind, At: at, Thread: b.thread}, text)
}

// unshaped says a message of the harness's record of no shape this read knows.
func (b *transcriptBuilder) unshaped() error {
	return b.unread(unshapedText)
}

// claudeMessageWhole reads a record's message as kind, and says one of no
// shape this read knows.
func (b *transcriptBuilder) claudeMessageWhole(ctx context.Context, raw json.RawMessage, kind EntryKind,
	at time.Time) error {

	if absent(raw) {
		return nil
	}
	if trimmed := bytes.TrimSpace(raw); trimmed[0] != '{' {
		return b.unshaped()
	}
	var message struct {
		Content json.RawMessage `json:"content"`
	}
	if err := wireDecode(raw, &message); err != nil {
		return b.unshaped()
	}
	return b.claudeContentWhole(ctx, message.Content, kind, at)
}

// claudeContentWhole reads a message's content: a string is one text of kind,
// and a list of blocks an entry a block; content that is neither is said.
func (b *transcriptBuilder) claudeContentWhole(ctx context.Context, raw json.RawMessage, kind EntryKind,
	at time.Time) error {

	raw = bytes.TrimSpace(raw)
	switch {
	case absent(raw):
		return nil
	case raw[0] == '"':
		var text string
		if err := wireDecode(raw, &text); err != nil {
			return b.unshaped()
		}
		return b.proseWhole(ctx, kind, at, text)
	case raw[0] != '[':
		return b.unshaped()
	}
	return eachClaudeBlock(raw, func(block claudeBlock) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		switch block.Type {
		case "text":
			return b.proseWhole(ctx, kind, at, block.Text)
		case "tool_use":
			if len(block.ID) <= b.valueMax && len(block.Name) <= b.valueMax {
				b.called(block.ID, block.Name)
			}
			if len(block.Name) > b.valueMax {
				s := b.streamedOpening()
				writeRunes(s, block.Name)
				if _, err := s.opening(transcriptNameMaxBytes); err != nil {
					return b.unread(unredactedText)
				}
			}
			return b.add(TranscriptEntry{Kind: EntryToolCall, At: at, Thread: b.thread, Tool: block.Name},
				compactJSON(block.Input))
		case "tool_result":
			tool := b.answered(block.ToolUseID)
			text, lost := claudeResultWhole(block.Content, b.valueMax)
			if lost {
				return b.unread(fmt.Sprintf("a tool's result in the harness's record runs past %s in a "+
					"part written out of order, and was not read", humanSize(int64(b.valueMax))))
			}
			return b.add(TranscriptEntry{Kind: EntryToolResult, At: at, Thread: b.thread, Tool: tool,
				Failed: block.IsError}, text)
		}
		return nil
	})
}

// claudeResultWhole is a tool's result's text as [claudeResultText] reads it,
// a text part past valueMax kept whatever it holds (R2), and whether a part
// whose text runs past valueMax is written out of order: its `type` once and
// then its `text` once.
func claudeResultWhole(raw json.RawMessage, valueMax int) (string, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '[' {
		return looseString(raw), false
	}
	var (
		lines []string
		lost  bool
	)
	_ = eachElement(raw, func(element json.RawMessage, _ int) error {
		if !wireObject(element) {
			return nil
		}
		var kind, text json.RawMessage
		types, texts, past, inOrder := 0, 0, false, true
		_ = eachKeyInPlace(element, func(name []byte, value json.RawMessage) error {
			switch string(name) {
			case "type":
				types++
				kind = value
			case "text":
				texts++
				inOrder = inOrder && types == 1
				past = past || len(looseString(value)) > valueMax
				text = value
			}
			return nil
		})
		if past && (!inOrder || types != 1 || texts != 1) {
			lost = true
			return nil
		}
		words := looseString(text)
		switch looseString(kind) {
		case "text":
			if len(words) > valueMax || strings.TrimSpace(words) != "" {
				lines = append(lines, words)
			}
		case "image":
			lines = append(lines, claudeImagePlaceholder)
		}
		return nil
	})
	return strings.Join(lines, "\n"), lost
}
