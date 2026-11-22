package executor

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"time"
)

// GEMINI CLI'S SESSION FILE, read as a transcript: one JSON object a line, a
// header, `$set` lines, and entries, each `user` or `gemini` (measured on
// 0.70.0 and 0.65.0). A `user` entry's content is the API's parts
// ([geminiParts]): the harness's own session context first, then the brief,
// each call's response, and the note a finish was blocked with. A `gemini`
// entry is one response, its words a string and its calls in `toolCalls`;
// 0.70.0 writes a response up to three times as it goes (its words, its calls
// scheduled, its calls run), each a whole copy under the response's id, so
// the latest copy is the response, read once the next entry begins.

// geminiSnapshot opens the user entry a compression writes its summary in.
// A COMPRESSION READS AS CLAUDE CODE'S COMPACTION DOES ([claudeReading]): every
// entry before it stays where it was read, and its summary is [EntrySummary].
// Gemini CLI 0.70.0 writes one in a single sync of its history
// (updateMessagesFromHistory): the snapshot, its own "Got it" reply, copies of
// the entries it kept under new ids, then one `$patch` (or `$rewindTo`) closing
// the sync and naming the entries a resume drops, which stay on disk. Up to
// that line it restates what was already read, never read again: the copies
// would read the kept entries twice and confirm a note's one delivery as two.
const geminiSnapshot = "<state_snapshot>"

// geminiSessionContext opens the user entry Gemini CLI writes ahead of the
// brief, its working directory and date: the harness's own words, never the
// run's.
const geminiSessionContext = "<session_context>"

// geminiEntryWire is the part of one session line the transcript reads.
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
// history: the line ending a compression's restatement.
func geminiSyncEnds(patch, rewind json.RawMessage) bool { return len(patch) > 0 || len(rewind) > 0 }

// geminiSummary is the summary content holds, and whether it is a
// compression's: its first text part, opening with [geminiSnapshot].
func geminiSummary(content json.RawMessage) (string, bool) {
	for _, part := range geminiParts(content) {
		if part.Text != nil && !part.Thought {
			text := strings.TrimSpace(*part.Text)
			return text, strings.HasPrefix(text, geminiSnapshot)
		}
	}
	return "", false
}

// geminiResponse is one `gemini` entry held until the next entry begins: its
// latest copy.
type geminiResponse struct {
	id    string
	at    time.Time
	words string
	calls json.RawMessage
}

// geminiSession reads one Gemini CLI main session from r into b, entry by
// entry, until b's page is whole or r ends; a line that does not decode is
// passed over, as a torn last line is.
func (b *transcriptBuilder) geminiSession(ctx context.Context, r io.Reader) error {
	var held *geminiResponse
	restating := false
	flush := func() error {
		if held == nil {
			return nil
		}
		response := *held
		held = nil
		return b.geminiResponded(response)
	}
	err := readLines(bufio.NewReaderSize(r, lineChunkBytes), maxLineBytes, func(line []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		var w geminiEntryWire
		if !wireObject(line) || wireAlias(line, &w) != nil {
			return nil
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
			response := geminiResponse{id: id, at: at, words: looseString(w.Content), calls: append(json.RawMessage(nil), w.ToolCalls...)}
			held = &response
		case "user":
			if err := flush(); err != nil {
				return err
			}
			if summary, ok := geminiSummary(w.Content); ok {
				// Whole: a heading inside a summary heads nothing.
				restating = true
				return b.add(TranscriptEntry{Kind: EntrySummary, At: at, Thread: b.thread}, summary)
			}
			return b.userParts(at, w.Content, geminiSessionContext)
		}
		return nil
	}, func(int64) error { return nil })
	if err != nil {
		return err
	}
	return flush()
}

// geminiResponded adds one response's words and calls, each call named by its
// id so its response says which tool answered.
func (b *transcriptBuilder) geminiResponded(r geminiResponse) error {
	if words := strings.TrimSpace(r.words); words != "" {
		if err := b.add(TranscriptEntry{Kind: EntryModel, At: r.at, Thread: b.thread}, words); err != nil {
			return err
		}
	}
	var calls []json.RawMessage
	if len(r.calls) == 0 || r.calls[0] != '[' || wireDecode(r.calls, &calls) != nil {
		return nil
	}
	for _, raw := range calls {
		call := geminiFunctionOf(raw, false)
		if call == nil {
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

// userParts adds one user entry's parts, as Gemini CLI and Qwen Code both
// record them: the brief, or a note, a late call or a wake by its heading
// (Gemini CLI hands a finish's block as the user's words, Qwen Code a
// message between steps behind its framing), and each call's response, as
// the tool that answered it. A text opening with ownContext, where the
// harness names one, is the harness's own words and never read (Gemini CLI's
// [geminiSessionContext]); "" reads every text.
func (b *transcriptBuilder) userParts(at time.Time, content json.RawMessage, ownContext string) error {
	for _, part := range geminiParts(content) {
		switch {
		case part.Response != nil:
			tool := b.answered(part.Response.ID)
			if tool == "" {
				tool = part.Response.Name
			}
			text, failed := geminiResponseText(part.Response.Body)
			if err := b.add(TranscriptEntry{Kind: EntryToolResult, At: at, Thread: b.thread, Tool: tool,
				Failed: failed}, text); err != nil {
				return err
			}
		case part.Text != nil && !part.Thought:
			text := strings.TrimSpace(*part.Text)
			if text == "" || (ownContext != "" && strings.HasPrefix(text, ownContext)) {
				continue
			}
			if !opensWithHeading(text) {
				if err := b.add(TranscriptEntry{Kind: EntryBrief, At: at, Thread: b.thread}, text); err != nil {
					return err
				}
				continue
			}
			for kind, words := range headedIn(text) {
				if err := b.add(TranscriptEntry{Kind: kind, At: at, Thread: b.thread}, words); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// geminiResponseText is a function response's text: its `output` where that
// is a string (Gemini CLI writes a tool's result so), its `error` where it
// failed, else the response as JSON.
func geminiResponseText(body json.RawMessage) (string, bool) {
	var w struct {
		Output json.RawMessage `json:"output"`
		Error  json.RawMessage `json:"error"`
	}
	if wireObject(body) && wireAlias(body, &w) == nil {
		if e := looseString(w.Error); e != "" {
			return e, true
		}
		if len(w.Error) > 0 && string(w.Error) != "null" {
			return compactJSON(w.Error), true
		}
		if out := looseString(w.Output); out != "" {
			return out, false
		}
	}
	return compactJSON(body), false
}
