package executor

import (
	"archive/tar"
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
)

// wholeMuse reads a Muse Code archive with the whole-line oracle
// (linewhole_internal_test.go).
type wholeMuse struct{ Muse }

func (wholeMuse) transcript(ctx context.Context, b *transcriptBuilder, archive io.Reader) error {
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
		if err := b.museRecordWhole(ctx, bufio.NewReaderSize(&b.handed, lineChunkBytes)); err != nil {
			return err
		}
	}
}

// piecesMuse reads a Muse Code archive on the walk, its inflated bytes handed
// n a read.
type piecesMuse struct {
	Muse
	n int
}

func (p piecesMuse) transcript(ctx context.Context, b *transcriptBuilder, archive io.Reader) error {
	return Muse{}.transcript(ctx, b, &exportPieces{r: archive, n: p.n})
}

// The whole-line read of a session.jsonl, a person's and a collection's.

func (b *transcriptBuilder) museRecordWhole(ctx context.Context, lines *bufio.Reader) error {
	read := 0
	return readLines(lines, maxLineBytes, func(line []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if read++; read > b.bounds.ReadLines {
			return b.linesPast("record", "lines")
		}
		return b.museLineWhole(line)
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

func (b *transcriptBuilder) museLineWhole(line []byte) error {
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
		if len(ev.Prompt) > b.valueMax {
			if strings.TrimSpace(ev.Prompt) == "" {
				return nil
			}
			return b.add(TranscriptEntry{Kind: EntryBrief, At: at, Thread: b.thread}, ev.Prompt)
		}
		return b.museWordsWhole(ev.Prompt, at, EntryBrief)
	case "assistant_message_committed":
		if strings.TrimSpace(ev.Text) != "" {
			return b.add(TranscriptEntry{Kind: EntryModel, At: at, Thread: b.thread}, ev.Text)
		}
	case "assistant_tool_calls_committed":
		for _, c := range ev.ToolCalls {
			if b.refusedName(c.Name) {
				if err := b.add(TranscriptEntry{Kind: EntryUnread, Thread: b.thread}, unredactedText); err != nil {
					return err
				}
				continue
			}
			b.called(c.CallID, c.Name)
			if err := b.add(TranscriptEntry{Kind: EntryToolCall, At: at, Thread: b.thread, Tool: c.Name},
				museArgsWhole(c.Args, b.valueMax)); err != nil {
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
		if ev.Source == museHookSource && len(ev.Text) <= b.valueMax {
			return b.museWordsWhole(ev.Text, at, "")
		}
	case "context_block_update_batch_committed":
		for _, e := range ev.Entries {
			if e.Update.Source != museHookSource || len(e.Update.Text) > b.valueMax {
				continue
			}
			if err := b.museWordsWhole(e.Update.Text, at, ""); err != nil {
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

// museArgsWhole is a call's arguments as a transcript shows them: Muse records
// them as a JSON string of the object the model wrote, read as JSON within
// valueMax and as written past it (R3).
func museArgsWhole(raw json.RawMessage, valueMax int) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if len(text) > valueMax {
			return text
		}
		return compactJSON(json.RawMessage(text))
	}
	return compactJSON(raw)
}

// museLineWhole is a line of a session's record as the whole-line museHeld
// reads it: its payload type, its runtime event's kind and the event itself,
// and the call a tool batch's effect names.
type museLineWhole struct {
	payloadType string
	kind        string
	event       json.RawMessage
	call        string
}

func decodeMuseLineWhole(line []byte) (museLineWhole, bool) {
	var env struct {
		PayloadType string `json:"payload_type"`
		Payload     struct {
			Event  json.RawMessage `json:"event"`
			Record struct {
				Kind   string `json:"kind"`
				CallID string `json:"call_id"`
			} `json:"record"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &env) != nil {
		return museLineWhole{}, false
	}
	l := museLineWhole{payloadType: env.PayloadType, event: env.Payload.Event}
	switch {
	case env.PayloadType == "tool_batch.effect.terminal" && env.Payload.Record.Kind == "terminal":
		l.call = env.Payload.Record.CallID
	case env.PayloadType == "runtime.session" && len(env.Payload.Event) > 0:
		var kind struct {
			Kind string `json:"kind"`
		}
		if json.Unmarshal(env.Payload.Event, &kind) != nil {
			return museLineWhole{}, false
		}
		l.kind = kind.Kind
	}
	return l, true
}

// museHookRun is a hook run's end as the record keeps it.
type museHookRun struct {
	Event   string   `json:"event"`
	HookKey string   `json:"hook_key"`
	Status  string   `json:"status"`
	Effects []string `json:"effects"`
	Stderr  string   `json:"stderr"`
	Origin  struct {
		SourceDigest string `json:"source_digest"`
	} `json:"origin"`
}

// museBlock is a context block as the record keeps it.
type museBlock struct {
	ID     string `json:"id"`
	Source string `json:"source"`
}

func decodeMuseEventWhole[T any](event json.RawMessage) (T, bool) {
	var v T
	return v, json.Unmarshal(event, &v) == nil
}

// museHeldWhole is the whole-line museHeld, the tokens of asked it holds; a
// call id past [transcriptNameMaxBytes] names no block a run awaits, as on the
// walk.
func museHeldWhole(r io.Reader, digest string, asked map[string]bool) (map[string]bool, error) {
	held := map[string]bool{}
	awaiting := map[string]museAnswered{}
	pending := map[string]bool{}
	answers := map[string]int{}
	call, stops := "", 0
	var unread *museUnreadLine
	n := 0
	next := func() error {
		n++
		if unread != nil {
			return unread
		}
		return nil
	}
	restart := func() {
		for token, finish := range pending {
			if finish {
				delete(pending, token)
			}
		}
		clear(awaiting)
		clear(answers)
		call = ""
	}
	err := readLines(bufio.NewReaderSize(r, lineChunkBytes), maxLineBytes, func(line []byte) error {
		if err := next(); err != nil {
			return err
		}
		l, ok := decodeMuseLineWhole(line)
		if !ok {
			unread = &museUnreadLine{at: n, why: "is not a record Muse Code writes"}
			return nil
		}
		switch {
		case l.payloadType == "session.started":
			restart()
			stops = 0
		case l.call != "":
			call = l.call
		case l.kind == "hook_run_terminal":
			h, read := decodeMuseEventWhole[museHookRun](l.event)
			if !read {
				unread = &museUnreadLine{at: n, why: "is a hook run's end Muse Code does not write"}
				return nil
			}
			var id string
			finish, ours := museNoteHookOf(h.HookKey)
			switch {
			case h.Event == "PostToolUse" && h.Status == "completed" && slices.Contains(h.Effects, "context") &&
				len(call) <= transcriptNameMaxBytes:
				id = fmt.Sprintf("hook:post_tool_use:%s:%d", call, answers[call])
				answers[call]++
			case h.Event == "Stop" && h.Status == "blocked" && slices.Contains(h.Effects, "blocked"):
				id = fmt.Sprintf("hook:stop:stop:%d", stops)
				stops++
			}
			if id == "" || !ours || h.Origin.SourceDigest != digest {
				return nil
			}
			var tokens []string
			for _, token := range strings.Split(h.Stderr, "\n") {
				if museIsToken(token) {
					tokens = append(tokens, token)
				}
			}
			awaiting[id] = museAnswered{tokens: tokens, finish: finish}
		case l.kind == "context_block_updated":
			b, read := decodeMuseEventWhole[museBlock](l.event)
			if !read {
				unread = &museUnreadLine{at: n, why: "is a context block Muse Code does not write"}
				return nil
			}
			if a, ok := awaiting[b.ID]; ok && b.Source == "runtime_hook" {
				delete(awaiting, b.ID)
				for _, token := range a.tokens {
					pending[token] = a.finish
				}
			}
		case l.kind == "context_compaction_installed":
			clear(pending)
			clear(awaiting)
		case l.kind == "model_completed":
			for token := range pending {
				held[token] = true
			}
			clear(pending)
			stops = 0
		}
		return nil
	}, func(size int64) error {
		if err := next(); err != nil {
			return err
		}
		unread = &museUnreadLine{at: n, why: fmt.Sprintf("is %d bytes, past the %d read of a line", size, maxLineBytes)}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for token := range held {
		if !asked[token] {
			delete(held, token)
		}
	}
	return held, nil
}
