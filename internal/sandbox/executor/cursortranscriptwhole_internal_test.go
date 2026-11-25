package executor

import (
	"archive/tar"
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// wholeCursor reads a Cursor archive with the whole-line oracle
// (linewhole_internal_test.go): each line held and decoded whole, as the read
// was before it moved onto the walk, with R2, R3 and R6 applied past the value
// bound. It reads no key written twice or in another case, which the walk
// reads by its exact name and its last value
// ([TestACursorKeyWrittenTwiceIsReadByItsLast]).
type wholeCursor struct{ CursorAgent }

func (wholeCursor) transcript(ctx context.Context, b *transcriptBuilder, archive io.Reader) error {
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
		if err := b.cursorRecordWhole(ctx, bufio.NewReaderSize(&b.handed, lineChunkBytes)); err != nil {
			return err
		}
	}
}

// piecesCursor reads a Cursor archive on the walk, its inflated bytes handed
// n a read.
type piecesCursor struct {
	CursorAgent
	n int
}

func (p piecesCursor) transcript(ctx context.Context, b *transcriptBuilder, archive io.Reader) error {
	return CursorAgent{}.transcript(ctx, b, &exportPieces{r: archive, n: p.n})
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

// cursorHistoryText is the text parts of content, joined: a part past
// valueMax kept whatever it holds (R2), and a join of no words empty.
func cursorHistoryText(content []cursorHistoryContent, valueMax int) string {
	var parts []string
	for _, c := range content {
		if c.Text != nil && (len(c.Text.Text) > valueMax || strings.TrimSpace(c.Text.Text) != "") {
			parts = append(parts, c.Text.Text)
		}
	}
	text := strings.Join(parts, "\n\n")
	if strings.TrimSpace(text) == "" {
		return ""
	}
	return text
}

func (b *transcriptBuilder) cursorRecordWhole(ctx context.Context, lines *bufio.Reader) error {
	read := 0
	return readLines(lines, maxLineBytes, func(line []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if read++; read > b.bounds.ReadLines {
			return b.linesPast("record", "lines")
		}
		return b.cursorLineWhole(line)
	}, func(n int64) error {
		return b.unread(fmt.Sprintf("a line of %s of the harness's record is past the %s one read reads, "+
			"and was not read", humanSize(n), humanSize(maxLineBytes)))
	})
}

func (b *transcriptBuilder) cursorLineWhole(line []byte) error {
	if strings.TrimSpace(string(line)) == "" {
		return nil
	}
	var m cursorHistoryMessage
	if err := json.Unmarshal(line, &m); err != nil || (m.User == nil && m.Assistant == nil && m.Tool == nil) {
		return b.notARecord(len(line))
	}
	switch {
	case m.User != nil:
		if text := cursorHistoryText(m.User.Content, b.valueMax); text != "" {
			return b.add(TranscriptEntry{Kind: EntryBrief}, text)
		}
	case m.Assistant != nil:
		if text := cursorHistoryText(m.Assistant.Content, b.valueMax); text != "" {
			if err := b.add(TranscriptEntry{Kind: EntryModel}, text); err != nil {
				return err
			}
		}
		for _, c := range m.Assistant.Content {
			if c.ToolCall == nil {
				continue
			}
			if b.refusedName(c.ToolCall.ToolName) {
				if err := b.add(TranscriptEntry{Kind: EntryUnread}, unredactedText); err != nil {
					return err
				}
				continue
			}
			b.called(c.ToolCall.ToolCallID, c.ToolCall.ToolName)
			args := c.ToolCall.ArgsJSON
			if len(args) <= b.valueMax {
				args = compactJSON(json.RawMessage(args))
			}
			if err := b.add(TranscriptEntry{Kind: EntryToolCall, Tool: c.ToolCall.ToolName}, args); err != nil {
				return err
			}
		}
	case m.Tool != nil:
		return b.add(TranscriptEntry{Kind: EntryToolResult, Tool: b.answered(m.Tool.ToolCallID),
			Failed: m.Tool.IsError}, cursorHistoryText(m.Tool.Content, b.valueMax))
	}
	return nil
}
