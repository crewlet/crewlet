package executor

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"time"
)

// QWEN CODE'S CHAT FILE, read as a transcript: one record a line, written as
// the run goes (measured on 0.28.3). A `user` record is the brief, or with
// subtype `mid_turn_user_message` a message the helper handed between steps
// behind Qwen Code's framing (a note, a late call or a wake by its heading,
// [headingFraming]); an `assistant` record is one response, its parts the
// API's ([geminiParts]): its reasoning, its words and its calls; a
// `tool_result` record is a call's response, written once; a `system` record
// is Qwen Code's own bookkeeping and never the conversation, but for a
// `chat_compression` record, read as the summary a resume goes on from
// ([transcriptBuilder.qwenCompressed]). Read in file order: a headless turn
// appends one chain, and Qwen Code rewinds a session only at a person's
// command.

// qwenRecordWire is the part of one chat-file record the transcript reads.
type qwenRecordWire struct {
	Type          json.RawMessage `json:"type"`
	Subtype       json.RawMessage `json:"subtype"`
	Timestamp     json.RawMessage `json:"timestamp"`
	Message       json.RawMessage `json:"message"`
	SystemPayload json.RawMessage `json:"systemPayload"`
}

// qwenSession reads one Qwen Code session from r into b, record by record,
// until b's page is whole or r ends; a line that does not decode is passed
// over, as a torn last line is.
func (b *transcriptBuilder) qwenSession(ctx context.Context, r io.Reader) error {
	return readLines(bufio.NewReaderSize(r, lineChunkBytes), maxLineBytes, func(line []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		var w qwenRecordWire
		if !wireObject(line) || wireAlias(line, &w) != nil {
			return nil
		}
		if looseString(w.Type) == "system" && looseString(w.Subtype) == "chat_compression" {
			return b.qwenCompressed(carriedInstant(claudeStamp(w.Timestamp)), w.SystemPayload)
		}
		var message struct {
			Parts json.RawMessage `json:"parts"`
		}
		if !wireObject(w.Message) || wireAlias(w.Message, &message) != nil {
			return nil
		}
		at := carriedInstant(claudeStamp(w.Timestamp))
		switch looseString(w.Type) {
		case "assistant":
			return b.qwenResponded(at, message.Parts)
		case "user", "tool_result":
			// A user record's text is the brief or a handed message; a
			// tool_result record's part is a call's response: Gemini CLI's
			// reading, Qwen Code's framing known to the heading's reader, and
			// no text Qwen Code's own, so none is passed over.
			return b.userParts(at, message.Parts, "")
		}
		return nil
	}, func(int64) error { return nil })
}

// qwenResponded adds one response's words and calls, each call named by its
// id so its response says which tool answered; its reasoning is never read.
func (b *transcriptBuilder) qwenResponded(at time.Time, parts json.RawMessage) error {
	for _, part := range geminiParts(parts) {
		switch {
		case part.Call != nil:
			b.called(part.Call.ID, part.Call.Name)
			if err := b.add(TranscriptEntry{Kind: EntryToolCall, At: at, Thread: b.thread, Tool: part.Call.Name},
				compactJSON(part.Call.Body)); err != nil {
				return err
			}
		case part.Text != nil && !part.Thought:
			if words := strings.TrimSpace(*part.Text); words != "" {
				if err := b.add(TranscriptEntry{Kind: EntryModel, At: at, Thread: b.thread}, words); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// qwenCompressed reads a chat_compression record as the summary Qwen Code
// goes on from (measured on 0.28.3): the user text of compressedHistory's
// first summaryPrefixLength entries, as one [EntrySummary], its headings
// heading nothing. The prefix's model entry is Qwen Code's canned
// acknowledgement and a copy of the tail's opening call, and every entry
// after the prefix copies a record this file already holds, so neither is
// read again: a page's ordinals are file order, and a copy would show a
// call, its result and a note twice. A prefix that does not read makes every
// entry the summary's, so no text in the record can head a note.
func (b *transcriptBuilder) qwenCompressed(at time.Time, payload json.RawMessage) error {
	var p struct {
		SummaryPrefixLength *int `json:"summaryPrefixLength"`
		CompressedHistory   []struct {
			Role  json.RawMessage `json:"role"`
			Parts json.RawMessage `json:"parts"`
		} `json:"compressedHistory"`
	}
	if !wireObject(payload) || wireDecode(payload, &p) != nil {
		return b.unread("a compaction record of no shape this read knows")
	}
	prefix := len(p.CompressedHistory)
	if p.SummaryPrefixLength != nil && *p.SummaryPrefixLength >= 0 {
		prefix = min(*p.SummaryPrefixLength, prefix)
	}
	var texts []string
	for _, entry := range p.CompressedHistory[:prefix] {
		if looseString(entry.Role) != "user" {
			continue
		}
		for _, part := range geminiParts(entry.Parts) {
			if part.Text != nil && !part.Thought && strings.TrimSpace(*part.Text) != "" {
				texts = append(texts, strings.TrimSpace(*part.Text))
			}
		}
	}
	if len(texts) == 0 {
		return nil
	}
	return b.add(TranscriptEntry{Kind: EntrySummary, At: at, Thread: b.thread}, strings.Join(texts, "\n\n"))
}
