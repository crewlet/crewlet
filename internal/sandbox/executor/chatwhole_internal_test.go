package executor

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode"
)

// THE WHOLE-LINE READS OF A CHAT FILE (Qwen Code's and Gemini CLI's) are the
// oracles a person's read and a collection's read on the walk are held to,
// each line held and decoded whole, with what the walk decides applied where
// it decides it: a line is taken as every bounded walk takes one
// (linewhole_internal_test.go); past the value bound, R1 and R6 hold; a run of
// white space longer than the bound is kept, even where it ends its text
// ([textTrim]); a compaction's summary past the bound is said in its place
// ([qwenCompaction]); and a failure's error of another kind than a string,
// written past the bound, is said in its place ([chatReading.answer]).

// geminiPart is one part of a Gemini API content, each field raw where a
// release may retype it and read loosely ([geminiParts]).
type geminiPart struct {
	// Text is the part's text, nil for a part with none.
	Text *string
	// Thought marks a text part that is the model's reasoning, never its
	// words.
	Thought bool
	// Call is the part's function call, Response a function's response.
	Call     *geminiFunction
	Response *geminiFunction
}

// geminiFunction is a function call or a function's response: the id the
// harness gave the call, the function's name, and its arguments or its
// response as JSON.
type geminiFunction struct {
	ID, Name string
	Body     json.RawMessage
}

// geminiPartWire is one part as the API spells it, each value raw.
type geminiPartWire struct {
	Text             json.RawMessage `json:"text"`
	Thought          json.RawMessage `json:"thought"`
	FunctionCall     json.RawMessage `json:"functionCall"`
	FunctionResponse json.RawMessage `json:"functionResponse"`
}

// geminiFunctionWire is a call or a response as the API spells it.
type geminiFunctionWire struct {
	ID       json.RawMessage `json:"id"`
	Name     json.RawMessage `json:"name"`
	Args     json.RawMessage `json:"args"`
	Response json.RawMessage `json:"response"`
}

// geminiParts is content's parts: a JSON array of parts, or a string, which
// is one text part (Gemini CLI writes a model turn's words so). A part that
// does not decode is passed over, as are content of any other kind and a part
// that is no object.
func geminiParts(content json.RawMessage) []geminiPart {
	content = bytes.TrimSpace(content)
	if s := looseString(content); s != "" {
		return []geminiPart{{Text: &s}}
	}
	if len(content) == 0 || content[0] != '[' {
		return nil
	}
	var raws []json.RawMessage
	if wireDecode(content, &raws) != nil {
		return nil
	}
	parts := make([]geminiPart, 0, len(raws))
	for _, raw := range raws {
		var w geminiPartWire
		if !wireObject(raw) || wireAlias(raw, &w) != nil {
			continue
		}
		var part geminiPart
		if len(w.Text) > 0 && w.Text[0] == '"' {
			text := looseString(w.Text)
			part.Text = &text
		}
		part.Thought = looseTrue(w.Thought)
		part.Call = geminiFunctionOf(w.FunctionCall, false)
		part.Response = geminiFunctionOf(w.FunctionResponse, true)
		parts = append(parts, part)
	}
	return parts
}

// geminiFunctionOf is raw as a call (its args) or, where response is set, a
// function's response (its response); nil where raw is no object.
func geminiFunctionOf(raw json.RawMessage, response bool) *geminiFunction {
	var w geminiFunctionWire
	if !wireObject(raw) || wireAlias(raw, &w) != nil {
		return nil
	}
	f := &geminiFunction{ID: looseString(w.ID), Name: looseString(w.Name), Body: bytes.Clone(w.Args)}
	if response {
		f.Body = bytes.Clone(w.Response)
	}
	return f
}

// decodes reports whether decode reads raw into v. The Gemini-API readers pass
// over a line or a field that does not decode, as Claude Code's reader does,
// and answer nothing for it rather than an error.
func decodes(decode func([]byte, any) error, raw []byte, v any) bool { return decode(raw, v) == nil }

// chatTrim is text as the walk trims it ([textTrim]): strings.TrimSpace, but
// for a run of white space ending it longer than valueMax, which is kept.
func chatTrim(text string, valueMax int) string {
	text = strings.TrimLeftFunc(text, unicode.IsSpace)
	if trimmed := strings.TrimRightFunc(text, unicode.IsSpace); len(text)-len(trimmed) <= valueMax {
		return trimmed
	}
	return text
}

// chatObjectWhole is line, JSON's blanks around it cut, where it is one JSON
// object as a bounded walk takes it past [readLines], else nil: a line that is
// no record.
func chatObjectWhole(line []byte) []byte {
	object := bytes.Trim(line, " \t\r\n")
	if !json.Valid(line) || !wireObject(object) {
		return nil
	}
	return object
}

// chatPastWhole says a line past the line bound, as a bounded walk does.
func (b *transcriptBuilder) chatPastWhole(n int64) error {
	return b.unread(fmt.Sprintf("a line of %s of the harness's record is past the %s one read reads, "+
		"and was not read", humanSize(n), humanSize(maxLineBytes)))
}

// userPartsWhole adds one user entry's parts, as Gemini CLI and Qwen Code both
// record them: the brief, or a note, a late call or a wake by its heading, and
// each call's response, as the tool that answered it. A text opening with
// ownContext is the harness's own words and never read; "" reads every text.
func (b *transcriptBuilder) userPartsWhole(at time.Time, content json.RawMessage, ownContext string) error {
	for _, part := range geminiParts(content) {
		switch {
		case part.Response != nil:
			tool := b.answered(part.Response.ID)
			if tool == "" && !b.refusedName(part.Response.Name) {
				tool = part.Response.Name
			}
			text, failed, past := geminiResponseTextWhole(part.Response.Body, b.valueMax)
			e := TranscriptEntry{Kind: EntryToolResult, At: at, Thread: b.thread, Tool: tool, Failed: failed}
			if past {
				e, text = TranscriptEntry{Kind: EntryUnread, Thread: b.thread}, fmt.Sprintf("a tool's failure of "+
					"the harness's record runs past %s, and was not read", humanSize(int64(b.valueMax)))
			}
			if err := b.add(e, text); err != nil {
				return err
			}
		case part.Text != nil && !part.Thought:
			text := chatTrim(*part.Text, b.valueMax)
			if text == "" || (ownContext != "" && strings.HasPrefix(text, ownContext)) {
				continue
			}
			if len(text) > b.valueMax || !opensWithHeading(text) {
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

// geminiResponseTextWhole is a function response's text: its `output` where
// that is a string, its `error` where it failed, else the response as JSON;
// and whether it is an error of another kind than a string written past
// valueMax, which is said in its place.
func geminiResponseTextWhole(body json.RawMessage, valueMax int) (string, bool, bool) {
	var w struct {
		Output json.RawMessage `json:"output"`
		Error  json.RawMessage `json:"error"`
	}
	if wireObject(body) && wireAlias(body, &w) == nil {
		if e := looseString(w.Error); e != "" {
			return e, true, false
		}
		if len(w.Error) > 0 && string(w.Error) != "null" {
			return compactJSON(w.Error), true, w.Error[0] != '"' && len(w.Error) > valueMax
		}
		if out := looseString(w.Output); out != "" {
			return out, false, false
		}
	}
	return compactJSON(body), false, false
}

// chatFixture is one recorded chat file, by its lines.
type chatFixture struct {
	name  string
	lines []string
}

// chatFixtures is every recorded chat file of harness's release: its own
// captures by glob, and the counts' and holdings' gzipped ones.
func chatFixtures(t *testing.T, dir, glob string) []chatFixture {
	t.Helper()
	var paths []string
	for _, g := range []string{glob, "count/*.chat.jsonl.gz", "holdings/*.chat.jsonl.gz"} {
		found, err := filepath.Glob(filepath.Join("testdata", dir, g))
		if err != nil {
			t.Fatal(err)
		}
		paths = append(paths, found...)
	}
	if len(paths) == 0 {
		t.Fatalf("no chat files under testdata/%s", dir)
	}
	var out []chatFixture
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		var r io.Reader = f
		if strings.HasSuffix(path, ".gz") {
			if r, err = gzip.NewReader(f); err != nil {
				t.Fatal(err)
			}
		}
		raw, err := io.ReadAll(r)
		_ = f.Close()
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, chatFixture{name: path, lines: strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")})
	}
	return out
}
