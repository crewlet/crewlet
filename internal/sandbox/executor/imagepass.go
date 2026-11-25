package executor

import (
	"bytes"
	"errors"
	"io"

	"github.com/crewlet/crewlet/internal/jsonscan"
)

// AN IMAGE A SESSION'S LINE HOLDS INLINE IS PASSED, NEVER HELD OR BOUNDED: a
// walk that passes images ([lineWalk.passImages]) walks every value its
// reader passes over ([lineWalk.pass]), reads each image's bytes a piece at a
// time and counts them out of the line's bound ([lineSource.skipped]), so the
// bound holds what the reader keeps. A text an image is joined into shows its
// place and size ([imagePlaceholder]), and a value shown as written shows it
// so too ([asWrittenImages]). Codex inlines every image it reads or is handed
// (view_image, a person's, an MCP tool's) as a data URL, a real one past
// 100 MiB (0.171.0), and Hermes 0.23.3 the newest a compaction kept.

// imageSlack is what a walk that passes images takes of a line past its bound
// before the source stops handing it on: the most bytes of an image taken
// that its reader has yet to pass, so a line of text within the bound is
// never cut short by an image's bytes in flight. Those are the piece the
// line's source holds ([lineChunkBytes]), its scanner's buffer and an inner
// scanner's, a Hermes content's words ([jsonscan.ReadBufferSize] each), and
// a string's opening read to tell it ([dataURLHeadMax]): 192.5 KiB. The
// verdict is [lineSource.over]'s, exact once the line ends.
const imageSlack = lineChunkBytes + 2*jsonscan.ReadBufferSize + dataURLHeadMax

// dataURLHeadMax is the most of a string read to tell an image's data URL by
// its head, `data:image/<subtype>[;<parameter>]...;base64,`: a subtype runs to
// RFC 6838's 127 characters, which leaves room for parameters.
const dataURLHeadMax = 512

// dataURLOpen opens an image's data URL.
const dataURLOpen = "data:image/"

// headVerdict is what a string's opening says of it: not yet told, an image's
// data URL, or none.
type headVerdict uint8

const (
	headUndecided headVerdict = iota
	headImage
	headNot
)

// dataURLHead tells head, the opening of a string's words, as an image's
// base64 data URL: its media type an image's, its parameters and `;base64`
// printable ASCII up to the comma that opens the data.
func dataURLHead(head []byte) headVerdict {
	if len(head) < len(dataURLOpen) {
		if bytes.HasPrefix([]byte(dataURLOpen), head) {
			return headUndecided
		}
		return headNot
	}
	if !bytes.HasPrefix(head, []byte(dataURLOpen)) {
		return headNot
	}
	for i := len(dataURLOpen); i < len(head); i++ {
		switch c := head[i]; {
		case c == ',':
			if bytes.HasSuffix(head[:i], []byte(";base64")) {
				return headImage
			}
			return headNot
		case c <= ' ' || c >= 0x7f || c == '"' || c == '\\':
			return headNot
		}
	}
	if len(head) >= dataURLHeadMax {
		return headNot
	}
	return headUndecided
}

// imagePlaceholder is what an image reads as in a text: [claudeImagePlaceholder],
// and where the walk passed its bytes inline, their size.
func imagePlaceholder(passed int64) string {
	if passed == 0 {
		return claudeImagePlaceholder
	}
	return "[an image of " + humanSize(passed) + "]"
}

// passImages makes w a walk that passes images.
func (w *lineWalk) passImages() {
	w.images, w.src.slack = true, imageSlack
	w.head = make([]byte, 0, dataURLHeadMax)
}

// skip passes over the value k, the current token, begins: as the scanner
// skips it, or on a walk that passes images, walked for them ([lineWalk.pass]).
func (w *lineWalk) skip(k jsonscan.Kind) error {
	if !w.images {
		return w.scan.Skip()
	}
	return w.pass(k)
}

// pass passes over the value k begins, holding none of it, each image in it
// passed ([lineWalk.passString]): a string that is an image's data URL, or
// the `data` string of an object whose `type` is `image`, written before it
// as serde writes the tag of an MCP image (Codex's mcp_tool_call_end).
func (w *lineWalk) pass(k jsonscan.Kind) error {
	switch k {
	case jsonscan.String:
		return w.passString(false)
	case jsonscan.ObjectStart:
		image := false
		return w.members(func(name string, k jsonscan.Kind) error {
			switch {
			case name == "type" && k == jsonscan.String:
				kind, err := w.plainWord()
				image = kind == "image"
				return err
			case name == "data" && image && k == jsonscan.String:
				return w.passString(true)
			}
			return w.pass(k)
		})
	case jsonscan.ArrayStart:
		return w.elements(func(k jsonscan.Kind) (bool, error) { return true, w.pass(k) })
	}
	return w.scan.Skip()
}

// passString passes over the current string, its words read a piece at a
// time: an image's, where image says it is one or its head tells a data URL
// ([dataURLHead]), each piece counted out of the line's bound as it is read;
// else the rest skipped.
func (w *lineWalk) passString(image bool) error {
	head := w.head[:0]
	if !image {
		var ended bool
		var err error
		switch head, image, ended, err = w.told(head); {
		case err != nil:
			return err
		case !image && ended:
			return nil
		case !image:
			return w.scan.Skip()
		case ended:
			w.src.skipped += int64(len(head))
			return nil
		}
	}
	_, err := w.passRest(int64(len(head)))
	return err
}

// told reads the current string's opening on from head, which is w.head's,
// until it is told an image's data URL or not ([dataURLHead]), and is what it
// read, whether it is an image's, and whether the string ended within it.
func (w *lineWalk) told(head []byte) ([]byte, bool, bool, error) {
	for {
		switch dataURLHead(head) {
		case headImage:
			return head, true, false, nil
		case headNot:
			return head, false, false, nil
		}
		if len(head) == cap(head) {
			return head, false, false, nil
		}
		n, err := w.scan.Read(head[len(head):cap(head)])
		head = head[:len(head)+n]
		switch {
		case errors.Is(err, io.EOF):
			return head, dataURLHead(head) == headImage, true, nil
		case err != nil:
			return head, false, false, err
		case n == 0:
			// The next character is longer than the room left: no data URL's
			// ASCII head.
			return head, false, false, nil
		}
	}
}

// passRest passes over the rest of the current string, an image whose first
// passed bytes are read, each piece counted out of the line's bound as it is
// read, and is the image's bytes.
func (w *lineWalk) passRest(passed int64) (int64, error) {
	w.src.skipped += passed
	for {
		n, err := w.scan.Read(w.piece)
		passed += int64(n)
		w.src.skipped += int64(n)
		switch {
		case errors.Is(err, io.EOF):
			return passed, nil
		case err != nil:
			return passed, err
		}
	}
}

// readImage reads the current string on from its opening, head, into to,
// where the walk passes images and it is one: its place written
// ([imagePlaceholder]), its bytes passed; it reports whether it was one, and
// else is what it read of the opening and whether the string ended there.
func (w *lineWalk) readImage(to io.Writer, head []byte, ended bool) (bool, []byte, bool, error) {
	if !w.images || ended {
		return false, head, ended, nil
	}
	head, image, ended, err := w.told(head)
	if err != nil || !image {
		return false, head, ended, err
	}
	passed := int64(len(head))
	if ended {
		w.src.skipped += passed
	} else {
		passed, err = w.passRest(passed)
	}
	_, _ = io.WriteString(to, imagePlaceholder(passed))
	return true, nil, true, err
}

// asWritten is t, where a value is written as written, through the walk's
// [asWrittenImages] on a walk that passes images.
func (w *lineWalk) asWritten(t io.Writer) io.Writer {
	if !w.images {
		return t
	}
	w.written = asWrittenImages{w: t, head: w.written.head[:0], out: w.written.out[:0]}
	return &w.written
}

// asWrittenImages writes a JSON value as written on to w, but each string that
// is an image's data URL ([dataURLHead]) as its place ([imagePlaceholder]),
// sized by the bytes written between its quotes. Its writes never fail.
type asWrittenImages struct {
	w io.Writer
	// at is where in the value the next byte falls; escaped says it follows
	// a backslash in a string, head is a string's opening while it is told,
	// and passed is the bytes of the image being passed.
	at      writtenAt
	escaped bool
	head    []byte
	passed  int64
	out     []byte
}

// writtenAt is where a value written falls: outside every string, in a
// string's opening being told, in an image's data URL or in another string.
type writtenAt uint8

const (
	writtenOutside writtenAt = iota
	writtenHead
	writtenImage
	writtenString
)

func (a *asWrittenImages) Write(p []byte) (int, error) {
	a.out = a.out[:0]
	for _, c := range p {
		switch a.at {
		case writtenOutside:
			a.out = append(a.out, c)
			if c == '"' {
				a.at, a.head = writtenHead, a.head[:0]
			}
		case writtenHead:
			a.headByte(c)
		case writtenImage:
			switch {
			case a.escaped:
				a.escaped = false
			case c == '\\':
				a.escaped = true
			case c == '"':
				a.out = append(a.out, imagePlaceholder(a.passed)...)
				a.out = append(a.out, c)
				a.at = writtenOutside
				continue
			}
			a.passed++
		case writtenString:
			a.out = append(a.out, c)
			switch {
			case a.escaped:
				a.escaped = false
			case c == '\\':
				a.escaped = true
			case c == '"':
				a.at = writtenOutside
			}
		}
	}
	_, _ = a.w.Write(a.out)
	return len(p), nil
}

// headByte takes c, the next byte of a string's opening: a string that ends
// or escapes a byte before it is told an image's is written as it is.
func (a *asWrittenImages) headByte(c byte) {
	if c == '"' {
		if dataURLHead(a.head) == headImage {
			a.out = append(a.out, imagePlaceholder(int64(len(a.head)))...)
		} else {
			a.out = append(a.out, a.head...)
		}
		a.out = append(a.out, c)
		a.at = writtenOutside
		return
	}
	a.head = append(a.head, c)
	switch told := dataURLHead(a.head); {
	case told == headImage:
		a.at, a.passed = writtenImage, int64(len(a.head))
	case told == headNot, c == '\\':
		a.out = append(a.out, a.head...)
		a.at, a.escaped = writtenString, c == '\\'
	}
}
