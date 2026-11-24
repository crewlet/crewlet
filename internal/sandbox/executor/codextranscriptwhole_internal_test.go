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
	"strings"
	"time"
)

// THE WHOLE-LINE READS OF A CODEX ROLLOUT, a person's, a collection's and the
// count's, as they were before they moved onto the line walk: the oracles the
// walk is held to, with what the walk decides applied where it decides it
// (codextranscript.go): every line within the bound counts toward the lines
// bound, and one past it is said in the walk's words and counts toward none; a
// word past [transcriptNameMaxBytes] reads as none; past the value bound R1,
// R2, R3 and R6 (linewhole_internal_test.go), a tool whose name or namespace
// runs past the word bound shows the two openings joined, and an output list
// of no shape is said.

// wholeCodex reads a Codex archive with the whole-line oracle.
type wholeCodex struct{ Codex }

func (wholeCodex) transcript(ctx context.Context, b *transcriptBuilder, archive io.Reader) error {
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
		if err := b.codexRolloutWhole(ctx, &b.handed); err != nil {
			return err
		}
	}
}

// piecesCodex reads a Codex archive on the walk, its inflated bytes handed n a
// read.
type piecesCodex struct {
	Codex
	n int
}

func (p piecesCodex) transcript(ctx context.Context, b *transcriptBuilder, archive io.Reader) error {
	return Codex{}.transcript(ctx, b, &exportPieces{r: archive, n: p.n})
}

// codexWholeLines hands each line of r, its break among it, to each, and
// passes a last line with no break over.
func codexWholeLines(r io.Reader, each func(line []byte) error) error {
	br := bufio.NewReaderSize(r, lineChunkBytes)
	for {
		line, err := br.ReadBytes('\n')
		switch {
		case errors.Is(err, io.EOF):
			return nil
		case err != nil:
			return err
		}
		if err := each(line); err != nil {
			return err
		}
	}
}

// wordWhole is s as the walk reads a word: none past [transcriptNameMaxBytes].
func wordWhole(s string) string {
	if len(s) > transcriptNameMaxBytes {
		return ""
	}
	return s
}

// pastWhole says a line past the bound the walk reads, sized with its break.
func (b *transcriptBuilder) pastWhole(n, bound int) error {
	return b.unread(fmt.Sprintf("a line of %s of the harness's record is past the %s one read reads, "+
		"and was not read", humanSize(int64(n)), humanSize(int64(bound))))
}

func (b *transcriptBuilder) codexRolloutWhole(ctx context.Context, r io.Reader) error {
	framed := 0
	return codexWholeLines(r, func(line []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(line) > codexWalkBound {
			return b.pastWhole(len(line), codexWalkBound)
		}
		if framed++; framed > b.bounds.ReadLines {
			return b.linesPast("Codex rollout", "lines")
		}
		if body := bytes.TrimSpace(line); len(body) > 0 {
			return b.codexLineWhole(body)
		}
		return nil
	})
}

// The rollout's wire as the whole-line reads decoded it.
type (
	codexTranscriptLine struct {
		Timestamp string          `json:"timestamp"`
		Type      string          `json:"type"`
		Payload   json.RawMessage `json:"payload"`
		Metadata  struct {
			CompactionOutput bool `json:"compaction_output"`
		} `json:"metadata"`
	}
	codexPart struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	codexResponseItem struct {
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
	codexItemMeta struct {
		Kinds []string `json:"content_item_kinds"`
		Parts []struct {
			Provenance struct {
				Type string `json:"type"`
			} `json:"provenance"`
		} `json:"content_item_metadata"`
	}
	codexItemWire struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Meta codexItemMeta `json:"internal_chat_message_metadata_passthrough"`
	}
)

// codexPersonsMessageWhole is the whole-line reads' rule for a person's
// message ([codexMeta.persons]).
func codexPersonsMessageWhole(role string, meta codexItemMeta) bool {
	if role != "user" {
		return false
	}
	if len(meta.Kinds) > 0 {
		for _, kind := range meta.Kinds {
			if kind != "user.text" {
				return false
			}
		}
		return true
	}
	for _, part := range meta.Parts {
		if part.Provenance.Type == "harness" {
			return false
		}
	}
	return true
}

func (b *transcriptBuilder) codexLineWhole(body []byte) error {
	var line codexTranscriptLine
	if wireDecode(body, &line) != nil {
		return nil
	}
	at := codexInstant(wordWhole(line.Timestamp))
	switch wordWhole(line.Type) {
	case "compacted":
		var c struct {
			Message string `json:"message"`
		}
		if wireDecode(line.Payload, &c) != nil {
			return b.unread("a Codex compaction of a shape this read does not know")
		}
		return b.codexProseWhole(EntrySummary, c.Message, at)
	case "response_item":
	default:
		return nil
	}
	var item codexResponseItem
	var raw struct {
		Content json.RawMessage `json:"content"`
	}
	if wireDecode(line.Payload, &item) != nil || wireDecode(line.Payload, &raw) != nil {
		return b.unread("a Codex record of a shape this read does not know")
	}
	item.Type, item.Role, item.CallID = wordWhole(item.Type), wordWhole(item.Role), wordWhole(item.CallID)
	switch item.Type {
	case "message":
		var kind EntryKind
		switch {
		case codexPersonsMessageWhole(item.Role, item.Meta):
			kind = EntryBrief
		case item.Role == "assistant" && !line.Metadata.CompactionOutput:
			kind = EntryModel
		default:
			return nil
		}
		text, lost := joinedWhole(raw.Content, codexContentParts, b.valueMax)
		if lost {
			return b.lostJoin()
		}
		return b.codexProseWhole(kind, text, at)
	case "reasoning", "ghost_snapshot":
		return nil
	case "function_call":
		input := item.Arguments
		if len(input) <= b.valueMax {
			input = compactJSON([]byte(input))
		}
		return b.codexCallWhole(item, input, at)
	case "custom_tool_call":
		return b.codexCallWhole(item, item.Input, at)
	case "web_search_call", "local_shell_call":
		tool := strings.TrimSuffix(item.Type, "_call")
		return b.add(TranscriptEntry{Kind: EntryToolCall, Tool: tool, At: at}, compactJSON(item.Action))
	case "function_call_output", "custom_tool_call_output":
		tool := b.answered(item.CallID)
		text, said := codexOutputTextWhole(item.Output, b.valueMax)
		switch said {
		case codexOutputLost:
			return b.lostJoin()
		case codexOutputUnshaped:
			return b.unread(fmt.Sprintf("a tool's result of the harness's record of no shape this read knows "+
				"runs past %s, and was not read", humanSize(int64(b.valueMax))))
		}
		return b.add(TranscriptEntry{Kind: EntryToolResult, Tool: tool, At: at}, text)
	}
	return b.unread(fmt.Sprintf("a Codex record of a kind this read does not know (%.64q)", item.Type))
}

// codexProseWhole adds the entries a text of kind makes: none for a blank
// one; within the value bound, a brief that opens with a heading each note,
// late call and wake it carries ([headedIn]); else one entry (R1).
func (b *transcriptBuilder) codexProseWhole(kind EntryKind, text string, at time.Time) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	if kind == EntryBrief && len(text) <= b.valueMax && opensWithHeading(text) {
		for heading, words := range headedIn(text) {
			if err := b.add(TranscriptEntry{Kind: heading, At: at}, words); err != nil {
				return err
			}
		}
		return nil
	}
	return b.add(TranscriptEntry{Kind: kind, At: at}, text)
}

// codexCallWhole adds a call's entry, remembered by its id for its answer;
// one whose name's redaction the stream refuses is said (R6).
func (b *transcriptBuilder) codexCallWhole(item codexResponseItem, input string, at time.Time) error {
	if b.refusedName(item.Name) || b.refusedName(item.Namespace) {
		return b.add(TranscriptEntry{Kind: EntryUnread}, unredactedText)
	}
	tool := item.Name
	switch {
	case item.Namespace == "":
	case len(item.Name) <= transcriptNameMaxBytes && len(item.Namespace) <= transcriptNameMaxBytes:
		tool = item.Namespace + "__" + item.Name
	default:
		part := func(s string) string {
			if len(s) <= transcriptNameMaxBytes {
				return s
			}
			shown, _, _ := entryOpening(s, transcriptNameMaxBytes)
			return shown
		}
		tool = part(item.Namespace) + "__" + part(item.Name)
		return b.add(TranscriptEntry{Kind: EntryToolCall, Tool: tool, At: at}, input)
	}
	b.called(item.CallID, tool)
	return b.add(TranscriptEntry{Kind: EntryToolCall, Tool: tool, At: at}, input)
}

// codexOutputSaid is how a call's output the oracle reads is said in place of
// its text.
type codexOutputSaid uint8

const (
	codexOutputText codexOutputSaid = iota
	codexOutputLost
	codexOutputUnshaped
)

// codexOutputTextWhole is a call's output as text: a string as written, a list
// of parts their texts joined (an image as one reads in a transcript), lost
// where a part past the bound is out of order (R2), else as its JSON, said
// past the bound; anything else as its JSON.
func codexOutputTextWhole(raw json.RawMessage, valueMax int) (string, codexOutputSaid) {
	var s string
	if wireDecode(raw, &s) == nil {
		return s, codexOutputText
	}
	var parts []codexPart
	if wireDecode(raw, &parts) != nil {
		compact := compactJSON(raw)
		if trimmed := bytes.TrimSpace(raw); len(trimmed) > 0 && trimmed[0] == '[' && len(compact) > valueMax {
			return "", codexOutputUnshaped
		}
		return compact, codexOutputText
	}
	text, lost := joinedWhole(raw, codexOutputParts, valueMax)
	if lost {
		return "", codexOutputLost
	}
	return text, codexOutputText
}

// codexNotesHeldWhole is the whole-line read of a collection's receipts.
func codexNotesHeldWhole(r io.Reader, takes []ServedTake, from int) ([]string, error) {
	var settled, pending []string
	next := 0
	_, err := codexLines(r, func(n int, body []byte) error {
		var line codexTranscriptLine
		if n <= from || wireDecode(body, &line) != nil {
			return nil
		}
		if line.Type == "compacted" {
			pending = pending[:0]
			return nil
		}
		var item codexItemWire
		if line.Type != "response_item" || wireDecode(line.Payload, &item) != nil {
			return nil
		}
		switch {
		case item.Type == "message" && codexPersonsMessageWhole(item.Role, item.Meta):
			var text strings.Builder
			for _, part := range item.Content {
				if part.Type == "input_text" {
					text.WriteString(part.Text)
				}
			}
			digest := ItemDigest(text.String())
			for i := next; i < len(takes); i++ {
				if takes[i].Digest == digest {
					pending, next = append(pending, takes[i].InboxID), i+1
					break
				}
			}
		case codexReply(item.Type, item.Role) && !line.Metadata.CompactionOutput:
			settled = append(settled, pending...)
			pending = pending[:0]
		}
		return nil
	}, func(n int) error {
		return fmt.Errorf("%w: line %d is over %d bytes", errCodexRolloutLine, n, codexRolloutLineMaxBytes)
	})
	if err != nil {
		return nil, err
	}
	return settled, nil
}

// codexCountWhole is the whole-line read of the count: each line decoded, a
// window id past the word bound read as none.
func (c *codexCounting) readWhole(ctx context.Context, rollout io.Reader) error {
	_, err := codexLines(rollout, func(_ int, body []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		var env struct {
			Type    string `json:"type"`
			Ordinal int64  `json:"ordinal"`
			Payload struct {
				Type string `json:"type"`
				Info *struct {
					Last *struct {
						Input int64 `json:"input_tokens"`
						Total int64 `json:"total_tokens"`
					} `json:"last_token_usage"`
					Window int64 `json:"model_context_window"`
				} `json:"info"`
				WindowID string `json:"window_id"`
			} `json:"payload"`
		}
		if wireDecode(body, &env) != nil {
			return nil
		}
		l := codexCountLine{typ: env.Type, ordinal: env.Ordinal, took: true, payloadType: env.Payload.Type,
			windowID: wordWhole(env.Payload.WindowID)}
		if info := env.Payload.Info; info != nil {
			l.info, l.window = true, info.Window
			if last := info.Last; last != nil {
				l.last, l.input, l.total = true, last.Input, last.Total
			}
		}
		c.line(&l)
		return nil
	}, func(n int) error {
		return fmt.Errorf("%w: line %d", errCodexRolloutLine, n)
	})
	return err
}
