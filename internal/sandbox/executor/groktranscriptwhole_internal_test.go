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
	"path"
	"regexp"
	"strings"
	"time"
)

// wholeGrok reads a Grok Build archive with the whole-line oracle
// (linewhole_internal_test.go).
type wholeGrok struct{ Grok }

func (wholeGrok) transcript(ctx context.Context, b *transcriptBuilder, archive io.Reader) error {
	tr := tar.NewReader(archive)
	b.handed.r = tr
	main, read := "", 0
	for {
		h, err := tr.Next()
		switch {
		case errors.Is(err, io.EOF):
			return nil
		case err != nil:
			return fmt.Errorf("%w: %w", errNotTar, err)
		}
		parts := strings.Split(path.Clean(h.Name), "/")
		if len(parts) != 3 || parts[0] == ".." || parts[1] == ".." || parts[2] != "chat_history.jsonl" {
			continue
		}
		if main == "" {
			main = parts[1]
		}
		b.thread = ""
		if parts[1] != main {
			b.thread = parts[1]
		}
		if h.Typeflag != tar.TypeReg {
			if err := b.unread("a transcript of the harness's record is not a plain file in the archive, " +
				"and was not read"); err != nil {
				return err
			}
			continue
		}
		if err := b.grokRecordWhole(ctx, bufio.NewReaderSize(&b.handed, lineChunkBytes), &read); err != nil {
			return err
		}
	}
}

// piecesGrok reads a Grok Build archive on the walk, its inflated bytes handed
// n a read.
type piecesGrok struct {
	Grok
	n int
}

func (p piecesGrok) transcript(ctx context.Context, b *transcriptBuilder, archive io.Reader) error {
	return Grok{}.transcript(ctx, b, &exportPieces{r: archive, n: p.n})
}

// The whole-line read of a chat_history.jsonl, a person's and a collection's.

func (b *transcriptBuilder) grokRecordWhole(ctx context.Context, lines *bufio.Reader, read *int) error {
	return readLines(lines, maxLineBytes, func(line []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if *read++; *read > b.bounds.ReadLines {
			return b.linesPast("record", "lines")
		}
		return b.grokLineWhole(line)
	}, func(n int64, _, _ []byte) error {
		return b.unread(fmt.Sprintf("a line of %s of the harness's record is past the %s one read reads, "+
			"and was not read", humanSize(n), humanSize(maxLineBytes)))
	})
}

func (b *transcriptBuilder) grokLineWhole(line []byte) error {
	if len(strings.TrimSpace(string(line))) == 0 {
		return nil
	}
	var l grokLine
	if json.Unmarshal(line, &l) != nil {
		return b.notARecord(len(line))
	}
	text, lost := grokTextWhole(l.Content, b.valueMax)
	past := lost || len(text) > b.valueMax
	if words, ok := grokHookWords(l); ok {
		if past {
			return nil
		}
		if token := grokTokenOf(words); token != "" {
			words = strings.TrimSuffix(strings.TrimRight(words, " \n"), token)
		}
		return b.museWordsWhole(strings.TrimSpace(words), time.Time{}, "")
	}
	switch l.Type {
	case "user":
		prompt := l.Reason == "" && (l.Prompt != nil || !lost && strings.HasPrefix(text, grokQueryOpen))
		summary := l.Reason == grokCompactionReason && (lost || !strings.HasPrefix(text, grokUserInfoOpen))
		kind := EntryBrief
		if summary {
			kind = EntrySummary
		}
		switch {
		case !prompt && !summary:
		case lost:
			return b.lostJoin()
		case past:
			if grokTypedFirst(line) {
				text = strings.TrimSuffix(strings.TrimPrefix(text, grokQueryOpen), grokQueryClose)
			}
			if strings.TrimSpace(text) != "" {
				return b.add(TranscriptEntry{Kind: kind, Thread: b.thread}, text)
			}
		case summary:
			text = strings.TrimSuffix(strings.TrimPrefix(text, grokQueryOpen), grokQueryClose)
			if strings.TrimSpace(text) != "" {
				return b.add(TranscriptEntry{Kind: EntrySummary, Thread: b.thread}, text)
			}
		default:
			text = strings.TrimSuffix(strings.TrimPrefix(text, grokQueryOpen), grokQueryClose)
			return b.museWordsWhole(text, time.Time{}, EntryBrief)
		}
	case "assistant":
		switch {
		case lost:
			if err := b.lostJoin(); err != nil {
				return err
			}
		case strings.TrimSpace(text) != "":
			if err := b.add(TranscriptEntry{Kind: EntryModel, Thread: b.thread}, text); err != nil {
				return err
			}
		}
		for _, c := range l.Calls {
			if b.refusedName(c.Name) {
				if err := b.add(TranscriptEntry{Kind: EntryUnread, Thread: b.thread}, unredactedText); err != nil {
					return err
				}
				continue
			}
			tool, args := c.Name, c.Arguments
			if len(args) <= b.valueMax {
				tool, args = grokCallOf(c.Name, c.Arguments)
			}
			b.called(c.ID, tool)
			if err := b.add(TranscriptEntry{Kind: EntryToolCall, Thread: b.thread, Tool: tool}, args); err != nil {
				return err
			}
		}
	case "tool_result":
		tool := b.answered(l.CallID)
		if lost {
			return b.lostJoin()
		}
		return b.add(TranscriptEntry{Kind: EntryToolResult, Thread: b.thread, Tool: tool}, text)
	}
	return nil
}

// grokTextWhole is a line's words ([grokLine.text]), past the value bound
// joined as the walk joins them (R2), and whether that join is lost.
func grokTextWhole(raw json.RawMessage, valueMax int) (string, bool) {
	var parts []struct {
		Text string `json:"text"`
	}
	if t := bytes.TrimSpace(raw); len(t) > 0 && t[0] == '[' && json.Unmarshal(raw, &parts) == nil {
		return joinedWhole(raw, grokParts, valueMax)
	}
	return grokLine{Content: raw}.text(), false
}

// grokTypedFirst reports whether a line names its type before its content,
// which the walk then frames as a prompt (R1).
func grokTypedFirst(line []byte) bool {
	typed, first := false, false
	_ = eachKey(line, func(name []byte, _ json.RawMessage, _ int) error {
		switch string(name) {
		case "type":
			typed = true
		case "content":
			first = typed
			return io.EOF
		}
		return nil
	})
	return first
}

// grokHookWords is what a hook of the run's settings handed the model on line,
// without its framing, and true; false where line is no such hook's.
func grokHookWords(l grokLine) (string, bool) {
	if l.Type != "user" {
		return "", false
	}
	text := l.text()
	switch l.Reason {
	case "system_reminder":
		if m := grokPlacedHead.FindStringIndex(text); m != nil {
			return strings.TrimSuffix(text[m[1]:], grokReminderTail), true
		}
	case "stop_hook_feedback":
		if inner, ok := strings.CutPrefix(text, grokStopHead); ok {
			return strings.TrimSuffix(inner, "\n"), true
		}
	}
	return "", false
}

// grokPlacedHead is the head of a reminder a hook of the home's settings
// answered with, after a call or a failed one, at any place of up to four
// digits: written apart from [grokHookAnswers], as a regular expression.
var grokPlacedHead = regexp.MustCompile(`^<system-reminder>\nContext from (PostToolUse hook 'user:post_tool_use|` +
	`PostToolUseFailure hook 'user:post_tool_use_failure)\[[0-9]{1,4}\]\.hooks\[0\]':\n`)

// grokCallOf is a call as a transcript shows it: use_tool's by the tool the
// model named and that tool's input, any other by its name and its arguments.
func grokCallOf(name, arguments string) (tool, args string) {
	var use struct {
		ToolName  string          `json:"tool_name"`
		ToolInput json.RawMessage `json:"tool_input"`
	}
	if name == "use_tool" && json.Unmarshal([]byte(arguments), &use) == nil && use.ToolName != "" {
		return use.ToolName, compactJSON(use.ToolInput)
	}
	return name, compactJSON(json.RawMessage(arguments))
}

// museWordsWhole adds text: each note, late call and wake it carries where it
// opens with an engine heading, else an entry of kind plain, or nothing where
// plain is none (a hook's context the engine did not write).
func (b *transcriptBuilder) museWordsWhole(text string, at time.Time, plain EntryKind) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	if !opensWithHeading(text) {
		if plain == "" {
			return nil
		}
		return b.add(TranscriptEntry{Kind: plain, At: at, Thread: b.thread}, text)
	}
	for kind, words := range headedIn(text) {
		if err := b.add(TranscriptEntry{Kind: kind, At: at, Thread: b.thread}, words); err != nil {
			return err
		}
	}
	return nil
}

func grokHeldWhole(r io.Reader) (map[string]bool, error) {
	held := map[string]bool{}
	pending := map[string]bool{}
	var unread *museUnreadLine
	n := 0
	next := func() error {
		n++
		if unread != nil {
			return unread
		}
		return nil
	}
	err := readLines(bufio.NewReaderSize(r, lineChunkBytes), maxLineBytes, func(line []byte) error {
		if err := next(); err != nil {
			return err
		}
		l, ok := decodeGrokLine(line)
		if !ok {
			unread = &museUnreadLine{at: n, why: "is not a record Grok Build writes"}
			return nil
		}
		if words, ok := grokHookWords(l); ok {
			if token := grokTokenOf(words); token != "" {
				pending[token] = true
			}
			return nil
		}
		if l.Type == "assistant" {
			for token := range pending {
				held[token] = true
			}
			clear(pending)
		}
		return nil
	}, func(size int64, _, _ []byte) error {
		if err := next(); err != nil {
			return err
		}
		unread = &museUnreadLine{at: n, why: fmt.Sprintf("is %d bytes, past the %d read of a line", size, maxLineBytes)}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return held, nil
}
