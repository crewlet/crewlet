package executor

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// A PERSON'S TRANSCRIPT OF A CODEX SESSION is read off its archive's one
// member ([codexArchiveFormat]) a line at a time ([codexLines]), each line
// decoded on its own: a line is at most [codexRolloutLineMaxBytes], so one
// held whole is a few MiB, inside what a read holds ([TranscriptReadHeldBytes]),
// and its texts past [TranscriptValueMaxBytes] are streamed into their entries
// ([transcriptBuilder.add]). Only the response items are read, never the
// event_msg lines that repeat them, so no entry is read twice.

// transcript reads a Codex archive ([codexArchiveFormat]).
func (Codex) transcript(ctx context.Context, b *transcriptBuilder, archive io.Reader) error {
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
		if h.Typeflag != tar.TypeReg || !codexIsMember(h.Name) {
			continue
		}
		if err := b.codexRollout(ctx, &b.handed); err != nil {
			return err
		}
	}
}

// codexIsMember reports whether name is a Codex archive's member: a rollout
// under the name it is restored by ([codexCarriedName]), whatever thread it
// names.
func codexIsMember(name string) bool {
	rest, ok := strings.CutPrefix(name, "rollout-"+codexCarriedStamp+"-")
	return ok && strings.HasSuffix(rest, ".jsonl") && !strings.Contains(rest, "/")
}

// codexRollout reads one rollout into b, a line at a time until ctx is done,
// ending where the lines pass the read's ReadLines, said.
func (b *transcriptBuilder) codexRollout(ctx context.Context, r io.Reader) error {
	framed := 0
	frame := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if framed++; framed > b.bounds.ReadLines {
			return errReadLines
		}
		return nil
	}
	_, err := codexLines(r, func(_ int, body []byte) error {
		if err := frame(); err != nil {
			return err
		}
		return b.codexLine(body)
	}, func(int) error {
		if err := frame(); err != nil {
			return err
		}
		return b.unread(fmt.Sprintf("a line of the Codex rollout past the %d bytes one line is read to",
			codexRolloutLineMaxBytes))
	})
	// A TORN LAST LINE is passed over, as Codex itself passes one over.
	if errors.Is(err, errReadLines) {
		return b.linesPast("Codex rollout", "lines")
	}
	return err
}

// codexTranscriptLine is a rollout line as the transcript reads it.
type codexTranscriptLine struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
	Metadata  struct {
		CompactionOutput bool `json:"compaction_output"`
	} `json:"metadata"`
}

// codexPart is one part of a message's content or of a call's output.
type codexPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// codexResponseItem is a response_item's payload, each kind's fields beside
// the others: Codex's ResponseItem, tagged by `type`.
type codexResponseItem struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Content   []codexPart     `json:"content"`
	Name      string          `json:"name"`
	Namespace string          `json:"namespace"`
	Arguments string          `json:"arguments"`
	Input     string          `json:"input"`
	CallID    string          `json:"call_id"`
	Output    json.RawMessage `json:"output"`
	Action    json.RawMessage `json:"action"`
	Meta      codexItemMeta   `json:"internal_chat_message_metadata_passthrough"`
}

// codexRecordOf is body as a rollout record, false for a FRAGMENT: a line
// that does not decode is one Codex left where it was cut off, which it skips,
// writing the next record on a line of its own (measured on 0.171.0), so the
// fragment was never part of the session and makes no entry, never the read's
// failure.
func codexRecordOf(body []byte) (codexTranscriptLine, bool) {
	var line codexTranscriptLine
	return line, wireDecode(body, &line) == nil
}

// codexCompaction is the summary a `compacted` line's payload holds, its
// `message`: what the transcript and a box read's summary both read
// ([summaryRead.codexLine]). False for a payload of no shape this read knows.
func codexCompaction(payload json.RawMessage) (string, bool) {
	var c struct {
		Message string `json:"message"`
	}
	return c.Message, wireDecode(payload, &c) == nil
}

// codexCompactionUnshaped says a compaction of no shape this read knows.
const codexCompactionUnshaped = "a Codex compaction of a shape this read does not know"

// codexLine reads one rollout line for its compaction's summary, the newest
// one read last, by the transcript's reading of it ([codexCompaction]).
func (s *summaryRead) codexLine(body []byte) {
	if line, record := codexRecordOf(body); record && line.Type == "compacted" {
		s.codexCompacted(line.Payload)
	}
}

// codexCompacted reads a `compacted` line's payload as the newest summary.
func (s *summaryRead) codexCompacted(payload json.RawMessage) {
	message, ok := codexCompaction(payload)
	if !ok {
		s.unshaped(codexCompactionUnshaped)
		return
	}
	s.take(message)
}

// codexLine adds the entries one rollout line makes: a user message's brief
// (or the notes it carries), the model's words, a call and its answer, a
// compaction's summary. Bookkeeping lines (session_meta, turn_context, the
// event_msg lines that repeat each item, token counts) and reasoning make
// none, as do kinds a later release adds, and a line that is no record; an
// item of a kind this read does not know is said.
func (b *transcriptBuilder) codexLine(body []byte) error {
	line, record := codexRecordOf(body)
	if !record {
		return nil
	}
	at := codexInstant(line.Timestamp)
	switch line.Type {
	case "compacted":
		message, ok := codexCompaction(line.Payload)
		if !ok {
			return b.unread(codexCompactionUnshaped)
		}
		return b.codexProse(EntrySummary, message, at)
	case "response_item":
	default:
		return nil
	}
	var item codexResponseItem
	if wireDecode(line.Payload, &item) != nil {
		return b.unread("a Codex record of a shape this read does not know")
	}
	switch item.Type {
	case "message":
		return b.codexMessage(item, line.Metadata.CompactionOutput, at)
	case "reasoning", "ghost_snapshot":
		return nil
	case "function_call":
		return b.codexCall(item, item.Arguments, true, at)
	case "custom_tool_call":
		return b.codexCall(item, item.Input, false, at)
	case "web_search_call", "local_shell_call":
		tool := strings.TrimSuffix(item.Type, "_call")
		return b.add(TranscriptEntry{Kind: EntryToolCall, Tool: tool, At: at}, compactJSON(item.Action))
	case "function_call_output", "custom_tool_call_output":
		return b.add(TranscriptEntry{Kind: EntryToolResult, Tool: b.answered(item.CallID), At: at},
			codexOutputText(item.Output))
	}
	return b.unread(fmt.Sprintf("a Codex record of a kind this read does not know (%.64q)", item.Type))
}

// codexMessage adds a message's entries: a person's ([codexPersonsMessage])
// as a brief, split into the notes, late calls and wakes it carries where it
// opens with one's heading; the model's as its words. Every other message is
// Codex's bookkeeping and makes none: a user message of its own (the
// environment context, a hook's prompt), a developer or system message, and
// the model's answer to a compaction (compactionOutput), whose summary the
// `compacted` line holds.
func (b *transcriptBuilder) codexMessage(item codexResponseItem, compactionOutput bool, at time.Time) error {
	var kind EntryKind
	switch {
	case codexPersonsMessage(item.Role, item.Meta):
		kind = EntryBrief
	case item.Role == "assistant" && !compactionOutput:
		kind = EntryModel
	default:
		return nil
	}
	var text strings.Builder
	for _, part := range item.Content {
		switch part.Type {
		case "input_text", "output_text":
			text.WriteString(part.Text)
		case "input_image":
			text.WriteString(claudeImagePlaceholder)
		}
	}
	return b.codexProse(kind, text.String(), at)
}

// codexProse adds the entries a text of kind makes: none for a blank one; a
// brief that opens with a heading, each note, late call and wake it carries
// ([headedIn]); else one entry.
func (b *transcriptBuilder) codexProse(kind EntryKind, text string, at time.Time) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	if kind == EntryBrief && opensWithHeading(text) {
		for heading, words := range headedIn(text) {
			if err := b.add(TranscriptEntry{Kind: heading, At: at}, words); err != nil {
				return err
			}
		}
		return nil
	}
	return b.add(TranscriptEntry{Kind: kind, At: at}, text)
}

// codexCall adds a call's entry, remembered by its id for its answer: a
// function's arguments as compacted JSON (isJSON says input is JSON text), a
// custom tool's input as written. An MCP tool is named as Codex offers it
// to the model, its namespace before its name.
func (b *transcriptBuilder) codexCall(item codexResponseItem, input string, isJSON bool, at time.Time) error {
	tool := item.Name
	if item.Namespace != "" {
		tool = item.Namespace + "__" + item.Name
	}
	b.called(item.CallID, tool)
	if isJSON {
		input = compactJSON([]byte(input))
	}
	return b.add(TranscriptEntry{Kind: EntryToolCall, Tool: tool, At: at}, input)
}

// codexOutputText is a call's output as text: a string as written, a list of
// parts their texts joined (an image as one reads in a transcript), anything
// else as its JSON.
func codexOutputText(raw json.RawMessage) string {
	var s string
	if wireDecode(raw, &s) == nil {
		return s
	}
	var parts []codexPart
	if wireDecode(raw, &parts) != nil {
		return compactJSON(raw)
	}
	var text strings.Builder
	for _, part := range parts {
		switch part.Type {
		case "input_image":
			text.WriteString(claudeImagePlaceholder)
		default:
			text.WriteString(part.Text)
		}
	}
	return text.String()
}

// codexInstant is a rollout line's timestamp, zero where it carries none an
// answer can carry ([carriedInstant]).
func codexInstant(stamp string) time.Time {
	at, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		return time.Time{}
	}
	return carriedInstant(at.UTC())
}
