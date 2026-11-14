package executor

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
)

// WHAT A SESSION'S LAST REQUEST CARRIED, read off its archive as a take streams
// it out of its box ([sandbox.TakenSession.Context]).
//
// A launch that continues a session sends the session's whole context again
// before it reads a word of its brief, so its budget floor is that context and
// the brief, never the static floor alone (internal/engine's launch floor). The
// harness's own record of its last request is that context, cache included:
//
//   - Claude Code's transcript keeps every assistant message with the usage
//     of the request that produced it, so the last one on the main thread (a
//     subagent's messages are sidechains, or files of their own) is the
//     input the session's last request carried: its uncached input, its cache
//     reads and its cache writes;
//   - OpenCode's export keeps every settled assistant message with its
//     tokens, and the newest that measured a prompt is the input the
//     session's last request carried, its input, cache reads and cache
//     writes, by ONE RULE for every reader of an OpenCode session's measure
//     ([measuredPrompt]): this one, and the read that measures the copy a
//     job's box holds as the job ends, off its session's own message list
//     ([openCodeContext]).
//
// READ OFF THE ARCHIVE AS IT IS KEPT, the scrub's output, so it is the session a
// later launch continues and not the box's copy, and in a goroutine of its own
// beside the take, which every byte reaches and which never holds the take up:
// what it cannot read it says nothing about (zero), and it drains the rest.

// contextMeter reads an archive written to it and keeps the whole input of the
// last request its transcript records.
type contextMeter struct {
	pw     *io.PipeWriter
	done   chan struct{}
	tokens int64
}

// newContextMeter starts a meter for an archive of layout holding session.
func newContextMeter(layout archiveLayout, session string) *contextMeter {
	pr, pw := io.Pipe()
	m := &contextMeter{pw: pw, done: make(chan struct{})}
	go func() {
		defer close(m.done)
		var (
			n   int64
			err error
		)
		switch layout {
		case archiveTar:
			n, err = lastTranscriptInput(pr, session)
		case archiveDocument:
			n, err = lastExportInput(pr)
		}
		if err == nil {
			m.tokens = n
		}
		// DRAINED WHATEVER IT FOUND, so the take that feeds it never waits on
		// a meter that stopped reading; the pipe ends when the take finishes.
		_, _ = io.Copy(io.Discard, pr)
	}()
	return m
}

// Write hands the meter what the take wrote. It never fails the take: a meter
// that cannot read the archive measures nothing.
func (m *contextMeter) Write(p []byte) (int, error) {
	_, _ = m.pw.Write(p)
	return len(p), nil
}

// finish ends the archive, err where the take that fed it failed.
func (m *contextMeter) finish(err error) { _ = m.pw.CloseWithError(err) }

// context is what the meter read, once the archive has ended.
func (m *contextMeter) context() int64 {
	<-m.done
	return m.tokens
}

// lastTranscriptInput is the input of the last request a Claude Code archive's
// transcript of session records, zero where it records none.
func lastTranscriptInput(r io.Reader, session string) (int64, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return 0, err
	}
	transcript := "projects/" + claudeProjectDir + "/" + session + ".jsonl"
	tr := tar.NewReader(zr)
	var last int64
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return last, nil
		}
		if err != nil {
			return 0, err
		}
		if h.Typeflag != tar.TypeReg || path.Clean(h.Name) != transcript {
			continue
		}
		if last, err = lastUsage(tr); err != nil {
			return 0, err
		}
	}
}

// lastUsage is the input of the last main-thread assistant message a
// transcript's lines record.
func lastUsage(r io.Reader) (int64, error) {
	lines := bufio.NewReaderSize(r, 64<<10)
	var last int64
	for {
		line, err := readLine(lines, scrubPieceMaxBytes)
		if n := assistantInput(line); n > 0 {
			last = n
		}
		if errors.Is(err, io.EOF) {
			return last, nil
		}
		if err != nil {
			return 0, err
		}
	}
}

// assistantInput is the whole input one transcript line's assistant message
// records, zero for any other line — a subagent's included.
func assistantInput(line []byte) int64 {
	// MOST LINES ARE NOT ONE: a tool's result, a person's message, a summary.
	if !bytes.Contains(line, []byte(`"assistant"`)) || !bytes.Contains(line, []byte(`"usage"`)) {
		return 0
	}
	var rec struct {
		Type        string `json:"type"`
		IsSidechain bool   `json:"isSidechain"`
		Message     struct {
			Usage *struct {
				Input      int64 `json:"input_tokens"`
				CacheWrite int64 `json:"cache_creation_input_tokens"`
				CacheRead  int64 `json:"cache_read_input_tokens"`
			} `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &rec) != nil || rec.Type != "assistant" || rec.IsSidechain ||
		rec.Message.Usage == nil {
		return 0
	}
	u := rec.Message.Usage
	return max(u.Input, 0) + max(u.CacheWrite, 0) + max(u.CacheRead, 0)
}

// lastExportInput is the input of the last request an OpenCode export records:
// its last assistant message that measured a prompt.
func lastExportInput(r io.Reader) (int64, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return 0, err
	}
	dec := json.NewDecoder(zr)
	if err = expectDelim(dec, '{'); err != nil {
		return 0, err
	}
	var last int64
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return 0, err
		}
		if key != "messages" {
			if err = skipValue(dec); err != nil {
				return 0, err
			}
			continue
		}
		if err = expectDelim(dec, '['); err != nil {
			return 0, err
		}
		for dec.More() {
			n, measured, msgErr := exportMessageInput(dec)
			if msgErr != nil {
				return 0, msgErr
			}
			if measured {
				last = n
			}
		}
		if err = expectDelim(dec, ']'); err != nil {
			return 0, err
		}
	}
	return last, nil
}

// exportMessageInput reads one message of an export and answers the input its
// request measured, and whether it measured one by [measuredPrompt]'s rule.
//
// THE MESSAGE IS THE ONE EVERY READER OF AN EXPORT TAKES ([openCodeMessage]),
// so this and a session's transcript never disagree about what a message is,
// its kind or its failure. A KEY AT A TIME, the export streamed: each of the
// small values the measure reads, its type, its time, its tokens and its error,
// is taken raw by its exact name and decoded on its own, by its keys exactly as
// written ([openCodeMeasuredWire]), and every other field is passed over a
// token at a time, so a message's content is never held whole. Decoded by
// encoding/json into the message's fields, the tokens were read with a key that
// differs from one they declare only in case taken for that one, the last of
// the two written winning.
func exportMessageInput(dec *json.Decoder) (int64, bool, error) {
	if err := expectDelim(dec, '{'); err != nil {
		return 0, false, err
	}
	var w openCodeMeasuredWire
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return 0, false, err
		}
		switch key {
		case "type":
			err = dec.Decode(&w.Type)
		case "time":
			err = dec.Decode(&w.Time)
		case "tokens":
			err = dec.Decode(&w.Tokens)
		case "error":
			err = dec.Decode(&w.Error)
		default:
			err = skipValue(dec)
		}
		if err != nil {
			return 0, false, err
		}
	}
	if err := expectDelim(dec, '}'); err != nil {
		return 0, false, err
	}
	m, _, err := w.message()
	if err != nil {
		return 0, false, err
	}
	n, measured := measuredPrompt(m)
	return n, measured, nil
}

// openCodeMeasuredWire is the part of one message of an OpenCode session the
// measure of its request reads, each key by its exact name and raw: its id, its
// kind, its time, its tokens and its error, as an export's message and a page of
// the session's message list each carry them. Every other key, its content
// above all, is passed over unread.
type openCodeMeasuredWire struct {
	ID     json.RawMessage `json:"id"`
	Type   json.RawMessage `json:"type"`
	Time   json.RawMessage `json:"time"`
	Tokens json.RawMessage `json:"tokens"`
	Error  json.RawMessage `json:"error"`
}

// message is the message w holds, as far as [measuredPrompt] reads one, each
// value decoded on its own BY ITS KEYS EXACTLY AS WRITTEN ([wireDecode]):
// its kind a string, its time's `completed` a whole number, its tokens'
// counts whole numbers, null as absent; and its id. AN ERROR, never a message
// read as one that measured nothing, where any of them is of another type: a
// reader that passed over a message it could not read would take an earlier
// message's figure for a request that may have carried more.
//
// ONLY WHAT THE MEASURE READS IS HELD TO ITS TYPE: of the time, its
// `completed` alone ([openCodeSettled]), and of the tokens, the prompt's
// counts alone ([openCodeTokens]). A value the measure never reads, a step's
// `created` or its reply's count, is no reason to lose a figure its counts
// and its completion state.
func (w openCodeMeasuredWire) message() (openCodeMessage, string, error) {
	var (
		m       openCodeMessage
		id      string
		settled openCodeSettled
	)
	for _, v := range [...]struct {
		key string
		raw json.RawMessage
		to  any
	}{
		{"id", w.ID, &id},
		{"type", w.Type, &m.Type},
		{"time", w.Time, &settled},
		{"tokens", w.Tokens, &m.Tokens},
	} {
		if v.raw == nil {
			continue
		}
		if err := wireDecode(v.raw, v.to); err != nil {
			return openCodeMessage{}, "", fmt.Errorf("a message's %s: %w", v.key, err)
		}
	}
	m.Time.Completed = settled.Completed
	m.Error = w.Error
	return m, id, nil
}

// openCodeSettled is the part of a message's time the measure reads: when its
// step settled, zero while it is in flight ([measuredPrompt]).
type openCodeSettled struct {
	Completed int64 `json:"completed"`
}

// measuredPrompt is the input one message of an OpenCode session says its
// step's request carried, and whether it measured one: THE ONE RULE for every
// reader of an OpenCode session's measure, an export's ([lastExportInput]) and
// the session's own message list's as a job ends ([openCodeContext]). A message
// measured a prompt where it is an assistant's step, settled (its time's
// `completed` set: an export holds settled messages alone, and the list hands
// the step in flight too), ended on no error (its `error` absent or null), and
// carries tokens whose prompt, its input, cache reads and cache writes, is
// above zero. A count below zero is read as none.
func measuredPrompt(m openCodeMessage) (int64, bool) {
	if m.Type != openCodeAssistant || m.Time.Completed <= 0 || !absent(m.Error) || m.Tokens == nil {
		return 0, false
	}
	n := max(m.Tokens.Input, 0) + max(m.Tokens.Cache.Read, 0) + max(m.Tokens.Cache.Write, 0)
	return n, n > 0
}

// skipValue passes over the next value whole, a token at a time.
func skipValue(dec *json.Decoder) error {
	depth := 0
	for {
		t, err := dec.Token()
		if err != nil {
			return err
		}
		if d, ok := t.(json.Delim); ok {
			switch d {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
		if depth == 0 {
			return nil
		}
	}
}
