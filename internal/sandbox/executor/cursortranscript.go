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
)

// transcript reads a Cursor archive ([cursorArchiveFormat]): its conversation,
// a message a line, as the session's entries. Any other member is passed
// unread.
func (CursorAgent) transcript(ctx context.Context, b *transcriptBuilder, archive io.Reader) error {
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
		if !cursorHistoryMember(h.Name) {
			continue
		}
		if h.Typeflag != tar.TypeReg {
			if err := b.unread("a transcript of the harness's record is not a plain file in the archive, " +
				"and was not read"); err != nil {
				return err
			}
			continue
		}
		if err := b.cursorRecord(ctx, bufio.NewReaderSize(&b.handed, lineChunkBytes)); err != nil {
			return err
		}
	}
}

// cursorHistoryMember reports whether an archive's member named name is a
// session's conversation: `cursor-history/<session>.jsonl`.
func cursorHistoryMember(name string) bool {
	parts := strings.Split(path.Clean(name), "/")
	return len(parts) == 2 && parts[0] == cursorArchiveDir && strings.HasSuffix(parts[1], ".jsonl") &&
		parts[1] != ".jsonl"
}

// cursorHistoryMessage is one message of Cursor's ConversationHistory, as the
// export writes it a line: a user's, an assistant's or a tool's result.
type cursorHistoryMessage struct {
	User *struct {
		Content []cursorHistoryContent `json:"content"`
	} `json:"user"`
	Assistant *struct {
		Content []cursorHistoryContent `json:"content"`
	} `json:"assistant"`
	Tool *struct {
		ToolCallID string                 `json:"toolCallId"`
		ToolName   string                 `json:"toolName"`
		Content    []cursorHistoryContent `json:"content"`
		IsError    bool                   `json:"isError"`
	} `json:"tool"`
}

// cursorHistoryContent is one part of a message: text, reasoning, or a call.
type cursorHistoryContent struct {
	Text *struct {
		Text string `json:"text"`
	} `json:"text"`
	ToolCall *struct {
		ToolCallID string `json:"toolCallId"`
		ToolName   string `json:"toolName"`
		ArgsJSON   string `json:"argsJson"`
	} `json:"toolCall"`
}

// cursorHistoryText is the text parts of content, joined.
func cursorHistoryText(content []cursorHistoryContent) string {
	var parts []string
	for _, c := range content {
		if c.Text != nil && strings.TrimSpace(c.Text.Text) != "" {
			parts = append(parts, c.Text.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// cursorRecord reads one conversation as entries, in recorded order: a user
// message is a brief, an assistant's its words and a call per tool call, a
// tool's its result. A line that is no message, or one past the line bound,
// is said where it was.
func (b *transcriptBuilder) cursorRecord(ctx context.Context, lines *bufio.Reader) error {
	read := 0
	return readLines(lines, maxLineBytes, func(line []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if read++; read > b.bounds.ReadLines {
			return b.linesPast("record", "lines")
		}
		return b.cursorLine(line)
	}, func(n int64) error {
		return b.unread(fmt.Sprintf("a line of %s of the harness's record is past the %s one read reads, "+
			"and was not read", humanSize(n), humanSize(maxLineBytes)))
	})
}

// cursorLine adds the entries one message makes.
func (b *transcriptBuilder) cursorLine(line []byte) error {
	if strings.TrimSpace(string(line)) == "" {
		return nil
	}
	var m cursorHistoryMessage
	if err := json.Unmarshal(line, &m); err != nil || (m.User == nil && m.Assistant == nil && m.Tool == nil) {
		return b.notARecord(len(line))
	}
	switch {
	case m.User != nil:
		if text := cursorHistoryText(m.User.Content); strings.TrimSpace(text) != "" {
			return b.add(TranscriptEntry{Kind: EntryBrief}, text)
		}
	case m.Assistant != nil:
		if text := cursorHistoryText(m.Assistant.Content); strings.TrimSpace(text) != "" {
			if err := b.add(TranscriptEntry{Kind: EntryModel}, text); err != nil {
				return err
			}
		}
		for _, c := range m.Assistant.Content {
			if c.ToolCall == nil {
				continue
			}
			b.called(c.ToolCall.ToolCallID, c.ToolCall.ToolName)
			if err := b.add(TranscriptEntry{Kind: EntryToolCall, Tool: c.ToolCall.ToolName},
				compactJSON(json.RawMessage(c.ToolCall.ArgsJSON))); err != nil {
				return err
			}
		}
	case m.Tool != nil:
		return b.add(TranscriptEntry{Kind: EntryToolResult, Tool: b.answered(m.Tool.ToolCallID),
			Failed: m.Tool.IsError}, cursorHistoryText(m.Tool.Content))
	}
	return nil
}
