package executor

import (
	"errors"
	"io"

	"github.com/crewlet/crewlet/internal/jsonscan"
)

// A HERMES ROW'S CONTENT IS READ AS HERMES READS IT BACK: a string, or where
// it opens with [hermesContentJSON], the JSON Hermes stored a structured
// content as (a list of parts, or a tool's `_multimodal` envelope of them),
// decoded as its _decode_content decodes it (0.23.3,
// persistence/sqlite/conversation/_store/messages.py) and read through the
// walk's inner scanner, holding none of it ([lineWalk.decoded]), an image
// part in its place ([hermesStoredParts]). A content that decodes as no shape
// Hermes reads is said in its place ([hermesStoredOdd]).

// hermesContentJSON opens a content Hermes stored as JSON: its words after it
// are the JSON text.
const hermesContentJSON = "\x00json:"

// hermesStoredParts is how a stored content's parts join, as Hermes's own
// text of one joins them (session_rules/turns.py, _durable_content): each
// `text` part's words a line apart, an image part in its place.
var hermesStoredParts = joinRule{kind: "text", image: "image_url", sep: "\n"}

// hermesStoredOdd is what a stored content of no shape this read knows says in
// its place.
const hermesStoredOdd = "a Hermes message's stored content of no shape this read knows, and was not read"

// opening reads the current string's opening, up to prefix's length, and is
// what it read, valid until the walk's next opening, whether it is prefix, and
// whether the string ended within it.
func (w *lineWalk) opening(prefix string) (head []byte, is, ended bool, err error) {
	if cap(w.head) < len(prefix) {
		w.head = make([]byte, 0, len(prefix))
	}
	head = w.head[:0]
	for len(head) < len(prefix) {
		n, err := w.scan.Read(head[len(head):len(prefix)])
		head = head[:len(head)+n]
		switch {
		case errors.Is(err, io.EOF):
			return head, string(head) == prefix, true, nil
		case err != nil:
			return head, false, false, err
		case n == 0:
			// The next character is longer than the room left: no ASCII
			// prefix's.
			return head, false, false, nil
		}
	}
	return head, string(head) == prefix, false, nil
}

// decoded reads the rest of the current string as one JSON text through the
// walk's inner scanner, read reading its value from its first token (w.scan
// the inner scanner meanwhile), and reports whether read took it and nothing
// but blanks follows it. What of the string a fault leaves is passed over; the
// outer scan's own failure is the error, which no inner fault hides.
func (w *lineWalk) decoded(read func(k jsonscan.Kind) (bool, error)) (bool, error) {
	outer := w.scan
	w.innerSrc = stringWords{scan: outer}
	if w.inner == nil {
		w.inner = jsonscan.New(&w.innerSrc)
	} else {
		w.inner.Reset(&w.innerSrc)
	}
	w.scan = w.inner
	took, err := w.innerValue(read)
	w.scan = outer
	if w.innerSrc.err != nil {
		return false, w.innerSrc.err
	}
	if _, scanned := errors.AsType[*jsonscan.Error](err); err != nil && !scanned && !errors.Is(err, io.EOF) {
		return false, err
	}
	if err != nil {
		took = false
	}
	for {
		_, readErr := outer.Read(w.piece)
		switch {
		case errors.Is(readErr, io.EOF):
			return took, nil
		case readErr != nil:
			return false, readErr
		}
	}
}

// innerValue reads the one value an inner text holds with read, and nothing
// but blanks after it.
func (w *lineWalk) innerValue(read func(k jsonscan.Kind) (bool, error)) (bool, error) {
	k, err := w.scan.Next()
	if err != nil {
		return false, err
	}
	took, err := read(k)
	if err != nil {
		return false, err
	}
	if _, err = w.scan.Next(); !errors.Is(err, io.EOF) {
		return false, err
	}
	return took, nil
}

// stringWords is a string's words as the scanner reading it hands them: what
// a scanner of a JSON text a string holds reads ([lineWalk.decoded]). err
// keeps the outer scan's own failure, which no inner fault hides.
type stringWords struct {
	scan *jsonscan.Scanner
	err  error
}

func (s *stringWords) Read(p []byte) (int, error) {
	n, err := s.scan.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		s.err = err
	}
	return n, err
}

// storedEnvelope reads a tool's stored `_multimodal` envelope, its opening
// brace current, its `content` list read by parts (each `content` written
// starting it again), and reports whether it is one: `_multimodal` true and a
// list of parts its decode takes.
func (w *lineWalk) storedEnvelope(parts func(k jsonscan.Kind) (bool, error)) (bool, error) {
	multimodal, listed, took := false, false, false
	err := w.members(func(name string, k jsonscan.Kind) error {
		switch {
		case name == "_multimodal":
			multimodal = k == jsonscan.True
		case name == "content" && k == jsonscan.ArrayStart:
			var err error
			listed = true
			took, err = parts(k)
			return err
		case name == "content":
			listed = false
		}
		return w.scan.Skip()
	})
	return multimodal && listed && took, err
}
