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

// transcript reads a Muse Code archive ([museArchiveFormat]): the main
// thread's session.jsonl and each subagent's, a line at a time, as the
// session's entries, a subagent's named by its thread. Any other member is
// passed unread.
func (Muse) transcript(ctx context.Context, b *transcriptBuilder, archive io.Reader) error {
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
		thread, ok := museRecordMember(h.Name)
		if !ok {
			continue
		}
		if h.Typeflag != tar.TypeReg {
			if err := b.unread("a transcript of the harness's record is not a plain file in the archive, " +
				"and was not read"); err != nil {
				return err
			}
			continue
		}
		b.thread = thread
		if err := b.museRecord(ctx, bufio.NewReaderSize(&b.handed, lineChunkBytes)); err != nil {
			return err
		}
	}
}

// museRecordMember reports whether an archive's member named name is a
// session's record, `<YYYY>/<MM>/<DD>/<session>/session.jsonl`, or a
// subagent's, `.../<session>/subagent/<thread>/session.jsonl`, with the
// subagent's thread ("" for the main thread's).
func museRecordMember(name string) (thread string, ok bool) {
	parts := strings.Split(path.Clean(name), "/")
	for _, p := range parts {
		if p == "" || p == ".." {
			return "", false
		}
	}
	switch {
	case len(parts) == 5 && parts[4] == "session.jsonl":
		return "", true
	case len(parts) == 7 && parts[4] == "subagent" && parts[6] == "session.jsonl":
		return parts[5], true
	}
	return "", false
}

// museRecord reads one session.jsonl as entries, in recorded order:
//   - a run's start is its prompt: the brief, or, where it opens with an
//     engine heading, each it carries ([headedIn]);
//   - an assistant message is the model's words, a committed call batch a
//     call each, a committed result batch a result each;
//   - a hook's context the engine wrote ([museHookBlock]) each note, late
//     call and wake it carries;
//   - an installed compaction its summary.
//
// A line that is no JSON is said where it was; a record of no payload type
// (the store's own frames) is passed over.
func (b *transcriptBuilder) museRecord(ctx context.Context, lines *bufio.Reader) error {
	read := 0
	return readLines(lines, maxLineBytes, func(line []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if read++; read > b.bounds.ReadLines {
			return b.linesPast("record", "lines")
		}
		return b.museLine(line)
	}, func(n int64) error {
		return b.unread(fmt.Sprintf("a line of %s of the harness's record is past the %s one read reads, "+
			"and was not read", humanSize(n), humanSize(maxLineBytes)))
	})
}

// museSessionEvent is a session record's `runtime.session` event, the fields
// the transcript reads.
type museSessionEvent struct {
	Kind      string `json:"kind"`
	Prompt    string `json:"prompt"`
	Text      string `json:"text"`
	Source    string `json:"source"`
	ToolCalls []struct {
		CallID string          `json:"call_id"`
		Name   string          `json:"name"`
		Args   json.RawMessage `json:"args"`
	} `json:"tool_calls"`
	Results []struct {
		ToolCallID string `json:"tool_call_id"`
		Text       string `json:"text"`
	} `json:"results"`
	Replacement struct {
		Text string `json:"text"`
	} `json:"replacement"`
	Entries []struct {
		Update museHookBlock `json:"update"`
	} `json:"entries"`
}

// museHookBlock is a context block a hook's answer added: its source,
// `runtime_hook`, and the whole text the model was handed.
type museHookBlock struct {
	Source string `json:"source"`
	Text   string `json:"text"`
}

// museHookSource is the source of a context block a hook's answer added
// (measured on 1.4.25).
const museHookSource = "runtime_hook"

// museLine adds the entries one line of a session.jsonl makes.
func (b *transcriptBuilder) museLine(line []byte) error {
	if len(strings.TrimSpace(string(line))) == 0 {
		return nil
	}
	if !json.Valid(line) {
		return b.notARecord(len(line))
	}
	env, ok := decodeMuseEnvelope(line)
	if !ok || env.PayloadType != "runtime.session" {
		return nil
	}
	var p struct {
		Event museSessionEvent `json:"event"`
	}
	if json.Unmarshal(env.Payload, &p) != nil {
		return b.unread("a record of the harness's session has no shape this read knows, and was not read")
	}
	at := museTime(env.RecordedAt)
	ev := p.Event
	switch ev.Kind {
	case "started":
		return b.museWords(ev.Prompt, at, EntryBrief)
	case "assistant_message_committed":
		if strings.TrimSpace(ev.Text) != "" {
			return b.add(TranscriptEntry{Kind: EntryModel, At: at, Thread: b.thread}, ev.Text)
		}
	case "assistant_tool_calls_committed":
		for _, c := range ev.ToolCalls {
			b.called(c.CallID, c.Name)
			if err := b.add(TranscriptEntry{Kind: EntryToolCall, At: at, Thread: b.thread, Tool: c.Name},
				museArgs(c.Args)); err != nil {
				return err
			}
		}
	case "tool_result_batch_committed":
		for _, r := range ev.Results {
			if err := b.add(TranscriptEntry{Kind: EntryToolResult, At: at, Thread: b.thread,
				Tool: b.answered(r.ToolCallID)}, r.Text); err != nil {
				return err
			}
		}
	case "context_block_updated":
		if ev.Source == museHookSource {
			return b.museWords(ev.Text, at, "")
		}
	case "context_block_update_batch_committed":
		for _, e := range ev.Entries {
			if e.Update.Source != museHookSource {
				continue
			}
			if err := b.museWords(e.Update.Text, at, ""); err != nil {
				return err
			}
		}
	case "context_compaction_installed":
		if strings.TrimSpace(ev.Replacement.Text) != "" {
			return b.add(TranscriptEntry{Kind: EntrySummary, At: at, Thread: b.thread}, ev.Replacement.Text)
		}
	}
	return nil
}

// museWords adds text: each note, late call and wake it carries where it
// opens with an engine heading, else an entry of kind plain, or nothing where
// plain is none (a hook's context the engine did not write).
func (b *transcriptBuilder) museWords(text string, at time.Time, plain EntryKind) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	if !opensWithHeading(text) {
		if plain == "" {
			return nil
		}
		return b.add(TranscriptEntry{Kind: plain, At: at, Thread: b.thread}, text)
	}
	for kind, words := range headedIn(text) {
		if err := b.add(TranscriptEntry{Kind: kind, At: at, Thread: b.thread}, words); err != nil {
			return err
		}
	}
	return nil
}

// museArgs is a call's arguments as a transcript shows them: Muse records them
// as a JSON string of the object the model wrote.
func museArgs(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return compactJSON(json.RawMessage(text))
	}
	return compactJSON(raw)
}

// museTime is a record's `recorded_at`, microseconds since the epoch, in UTC;
// zero where it carries none, or none an answer can carry ([carriedInstant]).
func museTime(micros int64) time.Time {
	if micros <= 0 {
		return time.Time{}
	}
	return carriedInstant(time.UnixMicro(micros).UTC())
}
