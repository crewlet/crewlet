package executor

import (
	"archive/tar"
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"time"
)

// A HERMES SESSION'S RECORD, as the carry script dumps it ([hermesCarryScript]):
// JSON lines, the store's schema first, then rows, each `{"t": <table>, "r":
// <row>}`. The readers here take the `messages` rows alone, in the order the
// store wrote them, the session's compaction lineage after it.

// hermesRowMaxBytes bounds one row of a Hermes session's record. Hermes cuts a
// tool's output it keeps to a size far below this; a longer row is not a shape
// this build read, so it is said and passed over.
const hermesRowMaxBytes = 8 << 20

// hermesRow is one row of a dumped record, the parts the readers take.
type hermesRow struct {
	Table   string
	Message hermesMessage
}

// hermesMessage is a `messages` row: its id, whether it is in the conversation
// ([hermesMessage.current]), how it ended and is shown, its role and words, the
// calls an assistant message made, and the call a tool message answers.
type hermesMessage struct {
	ID                int64           `json:"id"`
	Active            *int            `json:"active"`
	Compacted         *int            `json:"compacted"`
	CompressedSummary *int            `json:"_compressed_summary"`
	FinishReason      *string         `json:"finish_reason"`
	DisplayKind       *string         `json:"display_kind"`
	UID               string          `json:"message_uid"`
	Role              string          `json:"role"`
	Content           json.RawMessage `json:"content"`
	ToolCalls         json.RawMessage `json:"tool_calls"`
	ToolCallID        string          `json:"tool_call_id"`
	ToolName          string          `json:"tool_name"`
	Timestamp         json.RawMessage `json:"timestamp"`
}

// text is the message's words: its content as stored, a string, or the text
// parts of a list of parts.
func (m hermesMessage) text() string {
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(m.Content, &parts) != nil {
		return ""
	}
	var out string
	for _, p := range parts {
		if p.Type == "text" {
			out += p.Text
		}
	}
	return out
}

// current is whether the message is one of the conversation Hermes goes on
// with, as it was said: not a row it set inactive or compacted (an in-place
// compaction deactivates the rows it folds and writes their kept tail again
// under new ids, measured on 0.23.3, testdata session-compacted.jsonl), nor its
// summary, a user row of its own. A row with no flags is.
func (m hermesMessage) current() bool {
	return (m.Active == nil || *m.Active != 0) && (m.Compacted == nil || *m.Compacted == 0) && !m.summary()
}

// summary is whether the message is a compaction's summary, Hermes's own
// account of the rows it folded (measured on 0.23.3).
func (m hermesMessage) summary() bool { return m.CompressedSummary != nil && *m.CompressedSummary != 0 }

// replied is whether the message is a reply the model finished: an assistant
// row with a finish reason, never a summary or one cut off.
func (m hermesMessage) replied() bool {
	return m.Role == "assistant" && m.FinishReason != nil && *m.FinishReason != ""
}

// at is when Hermes recorded the message: seconds since the epoch, a number or
// a numeral; zero where it holds none.
func (m hermesMessage) at() time.Time {
	var f float64
	if json.Unmarshal(m.Timestamp, &f) != nil {
		var s string
		if json.Unmarshal(m.Timestamp, &s) != nil {
			return time.Time{}
		}
		parsed, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return time.Time{}
		}
		f = parsed
	}
	if f <= 0 || math.IsInf(f, 0) || math.IsNaN(f) {
		return time.Time{}
	}
	sec, frac := math.Modf(f)
	return time.Unix(int64(sec), int64(frac*1e9)).UTC()
}

// hermesCall is one call an assistant message made, in either shape Hermes
// stores: OpenAI's `function` object, or its own flat one.
type hermesCall struct {
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Args     json.RawMessage `json:"arguments"`
	Function *struct {
		Name string          `json:"name"`
		Args json.RawMessage `json:"arguments"`
	} `json:"function"`
}

// calls is the calls an assistant message made: its tool_calls, stored as a
// JSON array or as a string holding one.
func (m hermesMessage) calls() []hermesCall {
	raw := []byte(m.ToolCalls)
	var s string
	if json.Unmarshal(raw, &s) == nil {
		raw = []byte(s)
	}
	var out []hermesCall
	if json.Unmarshal(raw, &out) != nil {
		return nil
	}
	for i, c := range out {
		if c.Function != nil {
			out[i].Name, out[i].Args = firstNonBlank(c.Name, c.Function.Name), c.Function.Args
			if len(c.Args) > 0 {
				out[i].Args = c.Args
			}
		}
	}
	return out
}

// hermesRows hands each row of the dumped record r to each, in order; a row
// past [hermesRowMaxBytes] to long, passed over. An error from either ends the
// read with it.
func hermesRows(r io.Reader, each func(hermesRow) error, long func() error) error {
	br := bufio.NewReaderSize(r, 64<<10)
	return readLines(br, hermesRowMaxBytes, func(line []byte) error {
		// A LINE THAT IS NO ROW (the schema, the launch, one torn) is passed
		// over: the rows are what the readers take.
		if row, ok := hermesRowOf(line); ok {
			return each(row)
		}
		return nil
	}, func(int64) error {
		if long == nil {
			return nil
		}
		return long()
	})
}

// hermesRowOf is line as a row, and false for a line that is none.
func hermesRowOf(line []byte) (hermesRow, bool) {
	var wire struct {
		Table string          `json:"t"`
		Row   json.RawMessage `json:"r"`
	}
	if json.Unmarshal(line, &wire) != nil || wire.Table == "" {
		return hermesRow{}, false
	}
	row := hermesRow{Table: wire.Table}
	if wire.Table == "messages" && json.Unmarshal(wire.Row, &row.Message) != nil {
		return hermesRow{}, false
	}
	return row, true
}

// transcript reads a Hermes archive ([hermesArchiveFormat]): its one member's
// messages, each a person's brief or note, the model's words, a call, a
// result or a compaction's summary.
func (Hermes) transcript(ctx context.Context, b *transcriptBuilder, archive io.Reader) error {
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
		if h.Typeflag != tar.TypeReg || h.Name != hermesArchiveMember {
			continue
		}
		if err := b.hermesRecord(ctx, &b.handed); err != nil {
			return err
		}
	}
}

// hermesRecord reads one dumped record into b, a row at a time until ctx is
// done, ending where the rows pass the read's ReadLines, said. A row whose
// message_uid an earlier row carried is a copy a compaction wrote again (the
// kept tail, in place or in a rotated child, measured on 0.23.3), and is not
// read twice; each uid is kept as a 16-byte digest, so the rows' bound bounds
// what the read holds.
func (b *transcriptBuilder) hermesRecord(ctx context.Context, r io.Reader) error {
	framed := 0
	read := map[[16]byte]struct{}{}
	frame := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if framed++; framed > b.bounds.ReadLines {
			return errReadLines
		}
		return nil
	}
	err := hermesRows(r, func(row hermesRow) error {
		if err := frame(); err != nil {
			return err
		}
		if row.Table != "messages" || hermesCopy(read, row.Message.UID) {
			return nil
		}
		return b.hermesMessage(row.Message)
	}, func() error {
		if err := frame(); err != nil {
			return err
		}
		return b.unread(fmt.Sprintf("a row of the Hermes session past the %d bytes one row is read to",
			hermesRowMaxBytes))
	})
	if errors.Is(err, errReadLines) {
		return b.linesPast("Hermes session", "rows")
	}
	return err
}

// hermesCopy reports whether a row of uid copies one read before it, keeping
// uid where it does not; a row of no uid copies none.
func hermesCopy(read map[[16]byte]struct{}, uid string) bool {
	if uid == "" {
		return false
	}
	sum := sha256.Sum256([]byte(uid))
	key := [16]byte(sum[:16])
	if _, ok := read[key]; ok {
		return true
	}
	read[key] = struct{}{}
	return false
}

// hermesMessage adds a message's entries: a compaction's summary as the
// summary, a person's as a brief, a steered note as the note it frames, the
// model's words and calls, a tool's result.
func (b *transcriptBuilder) hermesMessage(m hermesMessage) error {
	at := m.at()
	if m.summary() {
		return b.codexProse(EntrySummary, m.text(), at)
	}
	switch m.Role {
	case "user":
		text := m.text()
		if note, ok := hermesSteered(text); ok {
			text = note
		}
		return b.codexProse(EntryBrief, text, at)
	case "assistant":
		if err := b.codexProse(EntryModel, m.text(), at); err != nil {
			return err
		}
		for _, c := range m.calls() {
			b.called(c.ID, c.Name)
			if err := b.add(TranscriptEntry{Kind: EntryToolCall, Tool: c.Name, At: at}, hermesArgs(c.Args)); err != nil {
				return err
			}
		}
		return nil
	case "tool":
		return b.add(TranscriptEntry{Kind: EntryToolResult, Tool: firstNonBlank(b.answered(m.ToolCallID), m.ToolName),
			At: at}, m.text())
	}
	return nil
}

// hermesArgs is a call's arguments as text: the string a model wrote, or the
// object compacted.
func hermesArgs(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return compactJSON(raw)
}
