package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"time"
)

// wholeGemini reads a Gemini CLI archive with the whole-line oracle
// (chatwhole_internal_test.go).
type wholeGemini struct{ GeminiCLI }

func (wholeGemini) transcript(ctx context.Context, b *transcriptBuilder, archive io.Reader) error {
	return b.chatArchive(ctx, archive, geminiChatMember, b.geminiSessionWhole)
}

// piecesGemini reads a Gemini CLI archive on the walk, its inflated bytes
// handed n a read.
type piecesGemini struct {
	GeminiCLI
	n int
}

func (p piecesGemini) transcript(ctx context.Context, b *transcriptBuilder, archive io.Reader) error {
	return GeminiCLI{}.transcript(ctx, b, &exportPieces{r: archive, n: p.n})
}

// geminiEntryWire is the part of one session line the whole-line read reads.
type geminiEntryWire struct {
	ID        json.RawMessage `json:"id"`
	Type      json.RawMessage `json:"type"`
	Timestamp json.RawMessage `json:"timestamp"`
	Content   json.RawMessage `json:"content"`
	ToolCalls json.RawMessage `json:"toolCalls"`
	Patch     json.RawMessage `json:"$patch"`
	Rewind    json.RawMessage `json:"$rewindTo"`
}

// geminiSyncEnds is whether a line of patch and rewind closes a sync of the
// history.
func geminiSyncEnds(patch, rewind json.RawMessage) bool { return len(patch) > 0 || len(rewind) > 0 }

// geminiSummary is the summary a compression's user entry content holds,
// trimmed as the walk trims it: its first text part a thought is not.
func geminiSummary(content json.RawMessage, valueMax int) string {
	for _, part := range geminiParts(content) {
		if part.Text != nil && !part.Thought {
			return chatTrim(*part.Text, valueMax)
		}
	}
	return ""
}

// geminiRecordWhole is what of r the whole-line oracles read, held whole:
// up to the last line break before r failed, and r's error; and which of
// its lines, by number from 0, are a compression's summary, each found as
// the three lines it is ([geminiCompressionSet]) on the record split whole.
func geminiRecordWhole(r io.Reader) ([]byte, map[int]bool, error) {
	record, err := io.ReadAll(r)
	if err != nil {
		record = record[:bytes.LastIndexByte(record, '\n')+1]
	}
	lines := bytes.Split(record, []byte("\n"))
	summaries := map[int]bool{}
	for i := 1; i+1 < len(lines); i++ {
		if geminiCompressionSet.holds(lines[i-1]) && geminiCompressionUser.holds(lines[i]) &&
			geminiCompressionAck.holds(lines[i+1]) {
			summaries[i] = true
		}
	}
	return record, summaries, err
}

// geminiResponseWhole is one `gemini` entry held until the next entry begins.
type geminiResponseWhole struct {
	id    string
	at    time.Time
	words string
	calls json.RawMessage
}

// geminiSessionWhole reads one Gemini CLI main session from r into b, a line
// held and decoded whole at a time. A line said in its place, past the line
// bound or no record, or past the lines bound, ends the response held, as the
// next entry does.
func (b *transcriptBuilder) geminiSessionWhole(ctx context.Context, r io.Reader) error {
	var held *geminiResponseWhole
	restating := false
	flush := func() error {
		if held == nil {
			return nil
		}
		response := *held
		held = nil
		return b.geminiRespondedWhole(response)
	}
	record, summaries, readErr := geminiRecordWhole(r)
	read, physical := 0, -1
	err := readLines(bufio.NewReaderSize(bytes.NewReader(record), lineChunkBytes), maxLineBytes, func(line []byte) error {
		physical++
		if err := ctx.Err(); err != nil {
			return err
		}
		if read++; read > b.bounds.ReadLines {
			if err := flush(); err != nil {
				return err
			}
			return b.linesPast("record", "lines")
		}
		if len(strings.TrimSpace(string(line))) == 0 {
			return nil
		}
		var w geminiEntryWire
		if object := chatObjectWhole(line); object == nil || !decodes(wireAlias, object, &w) {
			if err := flush(); err != nil {
				return err
			}
			return b.notARecord(len(line))
		}
		if geminiSyncEnds(w.Patch, w.Rewind) {
			restating = false
			return nil
		}
		if restating {
			return nil
		}
		at := carriedInstant(claudeStamp(w.Timestamp))
		switch looseString(w.Type) {
		case "gemini":
			id := looseString(w.ID)
			if held != nil && held.id != id {
				if err := flush(); err != nil {
					return err
				}
			}
			response := geminiResponseWhole{id: id, at: at, words: looseString(w.Content),
				calls: append(json.RawMessage(nil), w.ToolCalls...)}
			held = &response
		case "user":
			if err := flush(); err != nil {
				return err
			}
			if summaries[physical] {
				restating = true
				if summary := geminiSummary(w.Content, b.valueMax); summary != "" {
					return b.add(TranscriptEntry{Kind: EntrySummary, At: at, Thread: b.thread}, summary)
				}
				return nil
			}
			return b.userPartsWhole(at, w.Content, geminiSessionContext)
		}
		return nil
	}, func(n int64) error {
		physical++
		if err := flush(); err != nil {
			return err
		}
		return b.chatPastWhole(n)
	})
	if err == nil {
		err = readErr
	}
	if err != nil {
		return err
	}
	return flush()
}

// geminiRespondedWhole adds one response's words and calls; a call whose
// tool's redaction is refused is said in its place (R6).
func (b *transcriptBuilder) geminiRespondedWhole(r geminiResponseWhole) error {
	if words := chatTrim(r.words, b.valueMax); words != "" {
		if err := b.add(TranscriptEntry{Kind: EntryModel, At: r.at, Thread: b.thread}, words); err != nil {
			return err
		}
	}
	var calls []json.RawMessage
	if len(r.calls) == 0 || r.calls[0] != '[' || !decodes(wireDecode, r.calls, &calls) {
		return nil
	}
	for _, raw := range calls {
		call := geminiFunctionOf(raw, false)
		switch {
		case call == nil:
			continue
		case b.refusedName(call.Name):
			if err := b.add(TranscriptEntry{Kind: EntryUnread, Thread: b.thread}, unredactedText); err != nil {
				return err
			}
			continue
		}
		b.called(call.ID, call.Name)
		if err := b.add(TranscriptEntry{Kind: EntryToolCall, At: r.at, Thread: b.thread, Tool: call.Name},
			compactJSON(call.Body)); err != nil {
			return err
		}
	}
	return nil
}

// geminiConfirmedWhole is which of takes, in the order they were made, the main
// session read from r holds: each user text part confirms the first
// unconfirmed take after the last one confirmed whose digest its text has and
// whose run had started by its line, so two takes of identical text need two
// deliveries, and a take never delivered (its hook killed after it answered)
// leaves the later ones to be confirmed. A line that does not decode is passed
// over, as a torn last line is, and counted.
func geminiConfirmedWhole(_ context.Context, r io.Reader, takes []geminiAskedTake, bound int) ([]bool, error) {
	confirmed := make([]bool, len(takes))
	record, summaries, readErr := geminiRecordWhole(r)
	next, restating, at := 0, false, -1
	err := readLines(bufio.NewReaderSize(bytes.NewReader(record), lineChunkBytes), bound, func(line []byte) error {
		at++
		var entry struct {
			Type    string          `json:"type"`
			Content json.RawMessage `json:"content"`
			Patch   json.RawMessage `json:"$patch"`
			Rewind  json.RawMessage `json:"$rewindTo"`
		}
		if next == len(takes) || !decodes(wireDecode, line, &entry) {
			return nil
		}
		if geminiSyncEnds(entry.Patch, entry.Rewind) {
			restating = false
		}
		if restating || entry.Type != "user" {
			return nil
		}
		if summaries[at] {
			restating = true
			return nil
		}
		for _, part := range geminiParts(entry.Content) {
			if part.Text == nil {
				continue
			}
			digest := ItemDigest(*part.Text)
			for i := next; i < len(takes); i++ {
				if takes[i].digest == digest && takes[i].from <= at && (takes[i].until < 0 || at < takes[i].until) {
					confirmed[i], next = true, i+1
					break
				}
			}
		}
		return nil
	}, func(int64) error { at++; return nil })
	if err == nil {
		err = readErr
	}
	return confirmed, err
}
