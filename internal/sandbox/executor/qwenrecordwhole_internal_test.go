package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// wholeQwen reads a Qwen Code archive with the whole-line oracle
// (chatwhole_internal_test.go).
type wholeQwen struct{ QwenCode }

func (wholeQwen) transcript(ctx context.Context, b *transcriptBuilder, archive io.Reader) error {
	return b.chatArchive(ctx, archive, qwenChatMember, b.qwenSessionWhole)
}

// piecesQwen reads a Qwen Code archive on the walk, its inflated bytes handed
// n a read.
type piecesQwen struct {
	QwenCode
	n int
}

func (p piecesQwen) transcript(ctx context.Context, b *transcriptBuilder, archive io.Reader) error {
	return QwenCode{}.transcript(ctx, b, &exportPieces{r: archive, n: p.n})
}

// qwenRecordWire is the part of one chat-file record the whole-line read
// reads.
type qwenRecordWire struct {
	Type          json.RawMessage `json:"type"`
	Subtype       json.RawMessage `json:"subtype"`
	Timestamp     json.RawMessage `json:"timestamp"`
	Message       json.RawMessage `json:"message"`
	SystemPayload json.RawMessage `json:"systemPayload"`
}

// qwenSessionWhole reads one Qwen Code session from r into b, a line held and
// decoded whole at a time.
func (b *transcriptBuilder) qwenSessionWhole(ctx context.Context, r io.Reader) error {
	read := 0
	return readLines(bufio.NewReaderSize(r, lineChunkBytes), maxLineBytes, func(line []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if read++; read > b.bounds.ReadLines {
			return b.linesPast("record", "lines")
		}
		if len(strings.TrimSpace(string(line))) == 0 {
			return nil
		}
		var w qwenRecordWire
		if object := chatObjectWhole(line); object == nil || !decodes(wireAlias, object, &w) {
			return b.notARecord(len(line))
		}
		if looseString(w.Type) == "system" && looseString(w.Subtype) == "chat_compression" {
			return b.qwenCompressedWhole(carriedInstant(claudeStamp(w.Timestamp)), w.SystemPayload)
		}
		var message struct {
			Parts json.RawMessage `json:"parts"`
		}
		if !wireObject(w.Message) || !decodes(wireAlias, w.Message, &message) {
			return nil
		}
		at := carriedInstant(claudeStamp(w.Timestamp))
		switch looseString(w.Type) {
		case "assistant":
			return b.qwenRespondedWhole(at, message.Parts)
		case "user", "tool_result":
			return b.userPartsWhole(at, message.Parts, "")
		}
		return nil
	}, b.chatPastWhole)
}

// qwenRespondedWhole adds one response's words and calls; a call whose tool's
// redaction is refused is said in its place (R6).
func (b *transcriptBuilder) qwenRespondedWhole(at time.Time, parts json.RawMessage) error {
	for _, part := range geminiParts(parts) {
		switch {
		case part.Call != nil && b.refusedName(part.Call.Name):
			if err := b.add(TranscriptEntry{Kind: EntryUnread, Thread: b.thread}, unredactedText); err != nil {
				return err
			}
		case part.Call != nil:
			b.called(part.Call.ID, part.Call.Name)
			if err := b.add(TranscriptEntry{Kind: EntryToolCall, At: at, Thread: b.thread, Tool: part.Call.Name},
				compactJSON(part.Call.Body)); err != nil {
				return err
			}
		case part.Text != nil && !part.Thought:
			if words := chatTrim(*part.Text, b.valueMax); words != "" {
				if err := b.add(TranscriptEntry{Kind: EntryModel, At: at, Thread: b.thread}, words); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// qwenCompressedWhole reads a chat_compression record as the summary Qwen
// Code goes on from; one past the value bound is said in its place.
func (b *transcriptBuilder) qwenCompressedWhole(at time.Time, payload json.RawMessage) error {
	var p struct {
		SummaryPrefixLength *int `json:"summaryPrefixLength"`
		CompressedHistory   []struct {
			Role  json.RawMessage `json:"role"`
			Parts json.RawMessage `json:"parts"`
		} `json:"compressedHistory"`
	}
	if !wireObject(payload) || wireDecode(payload, &p) != nil {
		return b.unread(qwenCompactionUnread)
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
				texts = append(texts, chatTrim(*part.Text, b.valueMax))
			}
		}
	}
	summary := strings.Join(texts, "\n\n")
	switch {
	case len(texts) == 0:
		return nil
	case len(summary) > b.valueMax:
		return b.unread(fmt.Sprintf("a compaction record's summary runs past %s, and was not read",
			humanSize(int64(b.valueMax))))
	}
	return b.add(TranscriptEntry{Kind: EntrySummary, At: at, Thread: b.thread}, summary)
}

// qwenNotesHeldWhole is the inbox ids of takes, in serve order, whose items the
// session read from r holds as a current instruction: each record, at or past
// the line from where the takes' run began, matches the first unmatched take
// after the last one matched with its handed text's digest, so two takes of
// identical text need two records, and a take the helper never handed leaves
// the later ones to be matched, unheld itself. A match is settled only by the
// next assistant record, the reply to a request that carried the note plain,
// a turn cancelled mid-reply's streamed part included; a compaction before it
// keeps the match only where the note survives plain in the history Qwen Code
// goes on with, past the summary (ADR-0177). A match no reply follows is not
// held, so the note is served again. A line past bound or one that does not decode drops every
// pending match: it may have been a compaction that summarized them.
func qwenNotesHeldWhole(_ context.Context, r io.Reader, takes []ServedTake, from, bound int) ([]string, error) {
	type match struct{ id, digest string }
	var settled []string
	var pending []match
	next, at := 0, -1
	framing := "\n" + qwenNoteFraming
	err := readLines(bufio.NewReaderSize(r, lineChunkBytes), bound, func(line []byte) error {
		if at++; at < from {
			return nil
		}
		var record struct {
			Type          string          `json:"type"`
			Subtype       string          `json:"subtype"`
			SystemPayload json.RawMessage `json:"systemPayload"`
			Message       struct {
				Parts []struct {
					Text *string `json:"text"`
				} `json:"parts"`
			} `json:"message"`
		}
		if len(bytes.TrimSpace(line)) == 0 {
			return nil
		}
		if !decodes(json.Unmarshal, line, &record) {
			pending = pending[:0]
			return nil
		}
		switch {
		case record.Type == "assistant":
			// A TURN CANCELLED MID-REPLY records the reply's streamed part as
			// an assistant record, and session/load hands the note back plain
			// before it (measured on 0.28.4: session/cancel after a first
			// chunk, then a resumed request): it settles as any reply does.
			for _, m := range pending {
				settled = append(settled, m.id)
			}
			pending = pending[:0]
		case record.Type == "system" && record.Subtype == "chat_compression":
			tail := qwenPreservedWhole(record.SystemPayload)
			kept := pending[:0]
			for _, m := range pending {
				if tail[m.digest] > 0 {
					tail[m.digest]--
					kept = append(kept, m)
				}
			}
			pending = kept
		case record.Type == "user" && record.Subtype == "mid_turn_user_message" &&
			len(record.Message.Parts) > 0 && record.Message.Parts[0].Text != nil:
			handed, framed := strings.CutPrefix(*record.Message.Parts[0].Text, framing)
			digest := ItemDigest(handed)
			for i := next; framed && i < len(takes); i++ {
				if takes[i].Digest == digest {
					pending, next = append(pending, match{takes[i].InboxID, digest}), i+1
					break
				}
			}
		}
		return nil
	}, func(int64) error {
		at++
		pending = pending[:0]
		return nil
	})
	return settled, err
}

// qwenPreservedWhole counts, by digest, the handed texts a chat_compression record
// keeps plain: each text part of a user entry of the history Qwen Code goes on
// with, past its prefix, its framing cut (a model entry's words are never a
// note). The prefix is the larger of summaryPrefixLength (the summary and the
// model's acknowledgement, measured on 0.28.4) and contextPrefixLength, which
// no capture tells apart from it: the larger keeps less, so a note the
// compaction summarized is never held. A record whose summary prefix does not
// read keeps nothing.
func qwenPreservedWhole(payload json.RawMessage) map[string]int {
	var p struct {
		SummaryPrefixLength *int `json:"summaryPrefixLength"`
		ContextPrefixLength *int `json:"contextPrefixLength"`
		CompressedHistory   []struct {
			Role  string `json:"role"`
			Parts []struct {
				Text *string `json:"text"`
			} `json:"parts"`
		} `json:"compressedHistory"`
	}
	kept := map[string]int{}
	if json.Unmarshal(payload, &p) != nil || p.SummaryPrefixLength == nil || *p.SummaryPrefixLength < 0 {
		return kept
	}
	prefix := *p.SummaryPrefixLength
	if p.ContextPrefixLength != nil {
		prefix = max(prefix, *p.ContextPrefixLength)
	}
	for i := prefix; i < len(p.CompressedHistory); i++ {
		if p.CompressedHistory[i].Role != "user" {
			continue
		}
		for _, part := range p.CompressedHistory[i].Parts {
			if part.Text != nil {
				text, _ := strings.CutPrefix(*part.Text, "\n"+qwenNoteFraming)
				kept[ItemDigest(text)]++
			}
		}
	}
	return kept
}
