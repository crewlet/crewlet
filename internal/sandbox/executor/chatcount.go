package executor

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// WHAT A GEMINI CLI OR QWEN CODE CONTINUATION'S FIRST REQUEST CARRIES, read off
// the one chat file a capture's archive holds, or a job's box at its
// collection, a line at a time (geminicount.go, qwencount.go): an upper bound
// of the figure each harness compares with its point before the brief, judged
// in the box the launch gets (chatpoint.go).

// errChatArchive is an archive that holds no one chat file the meter reads.
var errChatArchive = errors.New("executor: the archive holds no one chat file")

// errChatLine is a chat file line past [codexRolloutLineMaxBytes], which no
// count reads past.
var errChatLine = errors.New("executor: a chat file line is past the bound a count reads")

// chatMemberLines hands line each whole line of the one regular member of the
// gzipped tar r that member names, a torn last line passed over, as the gate
// cuts it before a resume ([chatSessionGate]); errChatArchive where the
// archive holds none of it or more than one.
func chatMemberLines(r io.Reader, member func(string) bool, line func([]byte) error) error {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("%w: %w", errNotGzip, err)
	}
	tr := tar.NewReader(zr)
	read := false
	for {
		h, err := tr.Next()
		switch {
		case errors.Is(err, io.EOF) && read:
			return nil
		case errors.Is(err, io.EOF):
			return errChatArchive
		case err != nil:
			return fmt.Errorf("%w: %w", errNotTar, err)
		}
		if !member(h.Name) {
			continue
		}
		if read || h.Typeflag != tar.TypeReg {
			return errChatArchive
		}
		read = true
		if err := chatFileLines(tr, line); err != nil {
			return err
		}
	}
}

// chatFileLines hands line each whole line of r, one chat file, a torn last
// line passed over, as the gate cuts it before a resume ([chatSessionGate]).
func chatFileLines(r io.Reader, line func([]byte) error) error {
	_, err := codexLines(r, func(_ int, body []byte) error { return line(body) },
		func(n int) error { return fmt.Errorf("%w: line %d", errChatLine, n) })
	return err
}

// chatCounter is a harness's count of its chat file, a line at a time
// ([geminiCounting], [qwenCounting]): under marks, with taken bytes a take's
// scrub took out of the file put back where its rules read a size
// ([meterReading.unscrubbed]), 0 for a file as its box holds it.
type chatCounter interface {
	line(raw []byte)
	count(marks countMarks, taken int64) *sandbox.ResumedCount
}

// chatMeter is the take's meter of a chat archive r: c's count of the one
// chat file member names, by the rules of measured alone ([countChat]), and
// its count again with what the take's scrub took out ([meterReading.recount]).
func chatMeter(ctx context.Context, c chatCounter, r io.Reader, member func(string) bool,
	release, measured string) (meterReading, error) {

	marks, err := countChat(ctx, c.line, release, measured, func(line func([]byte) error) error {
		return chatMemberLines(r, member, line)
	})
	return meterReading{count: c.count(marks, 0), recount: &chatRecount{c: c, marks: marks}}, err
}

// chatRecount is a chat file's count, to be taken again with what a take's
// scrub took out ([meterReading.unscrubbed]): its counter, and its marks.
type chatRecount struct {
	c     chatCounter
	marks countMarks
}

// countChat hands line the lines each hands it and is the marks of its count,
// by the rules of measured, the one release they were read in, alone:
// [sandbox.CountRelease] for a chat file release wrote, [sandbox.CountUnread]
// with each's error where it did not hand every line (ctx's end among them).
func countChat(ctx context.Context, line func([]byte), release, measured string,
	each func(line func([]byte) error) error) (countMarks, error) {

	var marks countMarks
	if release != measured {
		marks = marks.with(sandbox.CountRelease)
	}
	err := each(func(raw []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		line(raw)
		return nil
	})
	if err != nil {
		return marks.with(sandbox.CountUnread), err
	}
	return marks, nil
}

// chatCopyCount is what a job's harness counts the copy of its session its box
// holds at, read at the job's collection ([sandbox.MainThreadAsk.Count]) by
// the rules the take's meter counts a capture by: the chat file file names
// ("" for none, [sandbox.CountUnread]) read whole within the read's wait. A box
// whose job continues it in place resumes that file, unscrubbed, so a relaunch
// continuing the copy, or a capture of it taken whole, is judged by this count
// ([sandbox.LaunchRecord.ResumedCount]); a release but measured counts nothing.
// The same walk hands each line to also where it is not nil, and err is why it
// did not read the file to its end; c nil counts nothing.
func chatCopyCount(ctx context.Context, box sandbox.Sandbox, file string, c chatCounter,
	release, measured string, also func([]byte)) (count *sandbox.ResumedCount, err error) {

	marked := func(mark sandbox.CountMark) *sandbox.ResumedCount {
		if c == nil {
			return nil
		}
		return &sandbox.ResumedCount{Mark: mark}
	}
	if release != measured {
		return marked(sandbox.CountRelease), fmt.Errorf("the job ran release %q, and this build reads %s alone",
			release, measured)
	}
	if file == "" {
		return marked(sandbox.CountUnread), errChatFile
	}
	r, err := box.OpenFile(ctx, file)
	if err != nil {
		return marked(sandbox.CountUnread), err
	}
	defer func() { _ = r.Close() }()
	marks, err := countChat(ctx, func(raw []byte) {
		if c != nil {
			c.line(raw)
		}
		if also != nil {
			also(raw)
		}
	}, release, measured, func(line func([]byte) error) error {
		return chatFileLines(r, line)
	})
	if c == nil {
		return nil, err
	}
	return c.count(marks, 0), err
}

// errChatFile is a session no one chat file in its box names.
var errChatFile = errors.New("no one chat file in the box names the session")

// lineSeparators reports whether raw, a chat file line, holds U+2028 or
// U+2029, raw or escaped: a JavaScript reader may split the line there, so
// the history it restores is not the one the line reads as. The take's meter
// reads the scrub's output, which writes both escaped (scrubjson.go), so the
// escaped form is the one a take meets; a text holding the six characters
// themselves reads as one too, which only tells more.
func lineSeparators(raw []byte) bool {
	if bytes.Contains(raw, []byte(" ")) || bytes.Contains(raw, []byte(" ")) {
		return true
	}
	lower := bytes.ToLower(raw)
	return bytes.Contains(lower, []byte(`\u2028`)) || bytes.Contains(lower, []byte(`\u2029`))
}

// jsLength is the length of JSON.stringify of the JSON value raw, as UTF-8
// bytes, AT LEAST what JavaScript writes: a string's characters escaped as it
// escapes them (a quote, a backslash or one of five controls in two, another
// control in six, and an unreadable byte as a lone surrogate's six), a number
// at its own length where it has no exponent and at 25 where it has one (no
// double's shortest form is longer), and a name given twice counted twice,
// though JavaScript keeps one.
func jsLength(raw []byte) (int64, error) {
	dec := jsontext.NewDecoder(bytes.NewReader(raw), threadDecoding)
	type open struct {
		object bool
		n      int
	}
	var (
		stack []open
		n     int64
	)
	for {
		tok, err := dec.ReadToken()
		if errors.Is(err, io.EOF) {
			return n, nil
		}
		if err != nil {
			return 0, err
		}
		kind := tok.Kind()
		if kind == '}' || kind == ']' {
			stack = stack[:len(stack)-1]
			n++
			continue
		}
		if len(stack) > 0 {
			top := &stack[len(stack)-1]
			switch {
			case top.object && top.n%2 == 1:
				n++ // the name's colon
			case top.n > 0:
				n++ // the comma before
			}
			top.n++
		}
		switch kind {
		case '{', '[':
			stack = append(stack, open{object: kind == '{'})
			n++
		case 'n', 't':
			n += 4
		case 'f':
			n += 5
		case '0':
			if s := tok.String(); strings.ContainsAny(s, "eE") {
				n += 25
			} else {
				n += int64(len(s))
			}
		case '"':
			n += jsStringLength(tok.String())
		}
	}
}

// jsStringLength is the UTF-8 length of s as JSON.stringify writes it,
// quotes included ([jsLength]).
func jsStringLength(s string) int64 {
	n := int64(2)
	for len(s) > 0 {
		r, size := utf8.DecodeRuneInString(s)
		s = s[size:]
		switch {
		case r == '"' || r == '\\' || r == '\b' || r == '\f' || r == '\n' || r == '\r' || r == '\t':
			n += 2
		case r < 0x20 || r == utf8.RuneError:
			n += 6
		default:
			n += int64(utf8.RuneLen(r))
		}
	}
	return n
}
