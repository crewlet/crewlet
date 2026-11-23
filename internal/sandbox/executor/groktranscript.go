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
)

// transcript reads a Grok Build archive ([grokArchiveFormat]): each session's
// record, `<key>/<session>/chat_history.jsonl`, a line at a time, as the
// session's entries, the first its main thread and each subagent's, after it,
// a thread named by its session. Any other member is passed unread.
func (Grok) transcript(ctx context.Context, b *transcriptBuilder, archive io.Reader) error {
	tr := tar.NewReader(archive)
	b.handed.r = tr
	main := ""
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
		if err := b.grokRecord(ctx, bufio.NewReaderSize(&b.handed, lineChunkBytes)); err != nil {
			return err
		}
	}
}

// grokRecord reads one chat_history.jsonl as entries, in recorded order:
//   - a person's prompt (a user line with its prompt_index and no
//     synthetic_reason) is the brief, or, where it opens with an engine
//     heading, each it carries ([headedIn]); one with a reason is Grok's own
//     (`subagent_completed`: a background subagent ended, measured on 1.0.92);
//   - a hook's words the run's settings answered with ([grokHookWords]) each
//     note, late call and wake it carries, without the take's token;
//   - an assistant line is the model's words and a call each, and a tool
//     result each call's result.
//
// The system message, Grok's own context and its reminders are passed over;
// the record carries no time. A line that is no JSON is said where it was.
func (b *transcriptBuilder) grokRecord(ctx context.Context, lines *bufio.Reader) error {
	read := 0
	return readLines(lines, maxLineBytes, func(line []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if read++; read > b.bounds.ReadLines {
			return b.linesPast("record", "lines")
		}
		return b.grokLine(line)
	}, func(n int64) error {
		return b.unread(fmt.Sprintf("a line of %s of the harness's record is past the %s one read reads, "+
			"and was not read", humanSize(n), humanSize(maxLineBytes)))
	})
}

// grokLine adds the entries one line of a chat_history.jsonl makes.
func (b *transcriptBuilder) grokLine(line []byte) error {
	if len(strings.TrimSpace(string(line))) == 0 {
		return nil
	}
	var l grokLine
	if json.Unmarshal(line, &l) != nil {
		return b.notARecord(len(line))
	}
	if words, ok := grokHookWords(l); ok {
		if token := grokTokenOf(words); token != "" {
			words = strings.TrimSuffix(strings.TrimRight(words, " \n"), token)
		}
		return b.museWords(strings.TrimSpace(words), time.Time{}, "")
	}
	switch l.Type {
	case "user":
		if l.Prompt == nil || l.Reason != "" {
			return nil
		}
		text := strings.TrimSuffix(strings.TrimPrefix(l.text(), "<user_query>\n"), "\n</user_query>")
		return b.museWords(text, time.Time{}, EntryBrief)
	case "assistant":
		if text := l.text(); strings.TrimSpace(text) != "" {
			if err := b.add(TranscriptEntry{Kind: EntryModel, Thread: b.thread}, text); err != nil {
				return err
			}
		}
		for _, c := range l.Calls {
			tool, args := grokCallOf(c.Name, c.Arguments)
			b.called(c.ID, tool)
			if err := b.add(TranscriptEntry{Kind: EntryToolCall, Thread: b.thread, Tool: tool}, args); err != nil {
				return err
			}
		}
	case "tool_result":
		return b.add(TranscriptEntry{Kind: EntryToolResult, Thread: b.thread, Tool: b.answered(l.CallID)}, l.text())
	}
	return nil
}

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
