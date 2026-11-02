package executor

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
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
//     tokens, and the last one that measured a prompt — no error, a prompt
//     of more than nothing — is the input OpenCode's own compaction reads the
//     session's context by (core/src/session/compaction.ts, hasMeasuredPrompt,
//     at 2.0.32): its input, cache reads and cache writes.
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
			n, msgErr := exportMessageInput(dec)
			if msgErr != nil {
				return 0, msgErr
			}
			if n > 0 {
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
// request measured where it is an assistant message that measured one without
// an error, zero otherwise. Every field but its type, its tokens and its error
// is passed over a token at a time, so a message's parts are never held whole.
func exportMessageInput(dec *json.Decoder) (int64, error) {
	if err := expectDelim(dec, '{'); err != nil {
		return 0, err
	}
	var (
		kind   string
		failed bool
		tokens *struct {
			Input int64 `json:"input"`
			Cache struct {
				Read  int64 `json:"read"`
				Write int64 `json:"write"`
			} `json:"cache"`
		}
	)
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return 0, err
		}
		switch key {
		case "type":
			err = dec.Decode(&kind)
		case "tokens":
			err = dec.Decode(&tokens)
		case "error":
			var raw json.RawMessage
			err = dec.Decode(&raw)
			failed = len(raw) > 0 && string(raw) != "null"
		default:
			err = skipValue(dec)
		}
		if err != nil {
			return 0, err
		}
	}
	if err := expectDelim(dec, '}'); err != nil {
		return 0, err
	}
	if kind != "assistant" || failed || tokens == nil {
		return 0, nil
	}
	return max(tokens.Input, 0) + max(tokens.Cache.Read, 0) + max(tokens.Cache.Write, 0), nil
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
