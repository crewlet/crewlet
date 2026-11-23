package executor

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// WHAT A GEMINI CLI OR QWEN CODE CONTINUATION'S FIRST REQUEST CARRIES, read off
// the one chat file its archive holds, a line at a time (geminicount.go,
// qwencount.go): an upper bound of the figure each harness compares with its
// point before the brief, judged in the box the launch gets (chatpoint.go).

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
		if _, err := codexLines(tr, func(_ int, body []byte) error { return line(body) },
			func(n int) error { return fmt.Errorf("%w: line %d", errChatLine, n) }); err != nil {
			return err
		}
	}
}

// lineSeparators reports whether raw, a chat file line, holds U+2028 or
// U+2029, raw or escaped: a JavaScript reader may split the line there, so
// the history it restores is not the one the line reads as.
func lineSeparators(raw []byte) bool {
	if bytes.Contains(raw, []byte(" ")) || bytes.Contains(raw, []byte(" ")) {
		return true
	}
	lower := bytes.ToLower(raw)
	return bytes.Contains(lower, []byte(` `)) || bytes.Contains(lower, []byte(` `))
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
