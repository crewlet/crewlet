package executor

import (
	"bytes"
	"errors"
	"io"

	"github.com/crewlet/crewlet/internal/jsonscan"
)

// MEDIA A SESSION'S LINE HOLDS INLINE IS PASSED, NEVER HELD OR BOUNDED: a walk
// that passes images ([lineWalk.passImages]) walks every value its reader
// passes over ([lineWalk.pass]), reads each payload a piece at a time and
// counts it out of the line's bound ([lineSource.skipped]), so the bound
// holds what the reader keeps. A payload is media of any type, not only an
// image: no reader keeps base64 as text. A text it is joined into shows its
// place and size ([mediaPlaceholder]), and so does a value shown as written
// ([asWrittenImages]). Codex 0.171.0 inlines images as data URLs, a real one
// past 100 MiB; Hermes 0.23.3 keeps those a compaction kept; Gemini CLI and
// Qwen Code write any file or MCP medium as `inlineData`, its base64 raw.

// imageSlack is what a walk that passes images takes of a line past its bound
// before the source stops handing it on: the most bytes of an image taken
// that its reader has yet to pass, so a line of text within the bound is
// never cut short by an image's bytes in flight. Those are the piece the
// line's source holds ([lineChunkBytes]), its scanner's buffer and an inner
// scanner's, a Hermes content's words ([jsonscan.ReadBufferSize] each), and
// a string's opening read to tell it ([dataURLHeadMax]): 192.5 KiB. The
// verdict is [lineSource.over]'s, exact once the line ends.
const imageSlack = lineChunkBytes + 2*jsonscan.ReadBufferSize + dataURLHeadMax

// dataURLHeadMax is the most of a string read to tell a data URL by its head,
// `data:<type>/<subtype>[;<parameter>]...;base64,`: a type and a subtype run
// to RFC 6838's 127 characters each, which leaves room for parameters.
const dataURLHeadMax = 512

// dataURLOpen opens a data URL.
const dataURLOpen = "data:"

// headVerdict is what a string's opening says of it: not yet told, a data
// URL's of some media, or none.
type headVerdict uint8

const (
	headUndecided headVerdict = iota
	headMedia
	headNot
)

// dataURLHead tells head, the opening of a string's words, as a base64 data
// URL of any media: a media type holding a slash, its parameters and
// `;base64` printable ASCII up to the comma that opens the data.
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
			typ, _, _ := bytes.Cut(head[len(dataURLOpen):i], []byte(";"))
			if bytes.HasSuffix(head[:i], []byte(";base64")) && bytes.IndexByte(typ, '/') > 0 {
				return headMedia
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

// mediaPlaceholder is what the payload of a data URL whose head is head, or of
// an inlineData object (head nil), passed bytes long, reads as in a text: an
// image's as [imagePlaceholder] says; other media by its type as written;
// inline data, whose type its text need not name before it, as data.
func mediaPlaceholder(head []byte, passed int64) string {
	if head == nil {
		return "[inline data of " + humanSize(passed) + "]"
	}
	comma := bytes.IndexByte(head, ',')
	typ, _, _ := bytes.Cut(head[len(dataURLOpen):max(comma, len(dataURLOpen))], []byte(";"))
	if bytes.HasPrefix(bytes.ToLower(typ), []byte("image/")) {
		return imagePlaceholder(passed)
	}
	return "[inline " + string(typ) + " of " + humanSize(passed) + "]"
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

// pass passes over the value k begins, holding none of it, each payload in it
// passed ([lineWalk.passString]): a string that is a data URL, the `data`
// string of an object whose `type` is `image`, written before it as serde
// writes the tag of an MCP image (Codex's mcp_tool_call_end), and an
// `inlineData` object's `data` ([lineWalk.passInline]).
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
			case name == "inlineData" && k == jsonscan.ObjectStart:
				return w.passInline()
			}
			return w.pass(k)
		})
	case jsonscan.ArrayStart:
		return w.elements(func(k jsonscan.Kind) (bool, error) { return true, w.pass(k) })
	}
	return w.scan.Skip()
}

// passInline passes over an `inlineData` object, its opening brace current:
// its `data`, base64 of any media type (Gemini CLI 0.70.0 writes it before or
// after `mimeType`, by the tool), passed whole, as is any payload in the rest.
func (w *lineWalk) passInline() error {
	return w.members(func(name string, k jsonscan.Kind) error {
		if name == "data" && k == jsonscan.String {
			return w.passString(true)
		}
		return w.pass(k)
	})
}

// skipInline passes over the value k begins, an `inlineData` member's: as
// [lineWalk.passInline] passes an object on a walk that passes images, else
// as [lineWalk.skip] does.
func (w *lineWalk) skipInline(k jsonscan.Kind) error {
	if w.images && k == jsonscan.ObjectStart {
		return w.passInline()
	}
	return w.skip(k)
}

// skipMember passes over the value k of the member name: an `inlineData`
// member's as [lineWalk.skipInline] does, any other's as [lineWalk.skip].
func (w *lineWalk) skipMember(name string, k jsonscan.Kind) error {
	if name == "inlineData" {
		return w.skipInline(k)
	}
	return w.skip(k)
}

// passString passes over the current string, its words read a piece at a
// time: a payload's, where image says it is one or its head tells a data URL
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
// until it is told a data URL or not ([dataURLHead]), and is what it read,
// whether it is one, and whether the string ended within it.
func (w *lineWalk) told(head []byte) ([]byte, bool, bool, error) {
	for {
		switch dataURLHead(head) {
		case headMedia:
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
			return head, dataURLHead(head) == headMedia, true, nil
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
// where the walk passes images and it is a data URL: its place written
// ([mediaPlaceholder]), its bytes passed; it reports whether it was one, and
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
	_, _ = io.WriteString(to, mediaPlaceholder(head, passed))
	return true, nil, true, err
}

// asWritten is t, where a value is written as written, through the walk's
// [asWrittenImages] on a walk that passes images.
func (w *lineWalk) asWritten(t io.Writer) io.Writer {
	if !w.images {
		return t
	}
	a := &w.written
	*a = asWrittenImages{w: t, head: a.head[:0], key: a.key[:0], open: a.open[:0], out: a.out[:0]}
	return a
}

// asWrittenImages writes a JSON value as written on to w, but each payload in
// it as its place ([mediaPlaceholder]), sized by the bytes written between
// its quotes: a string that is a data URL ([dataURLHead]), and an
// `inlineData` object's `data` string. Its writes never fail.
type asWrittenImages struct {
	w io.Writer
	// at is where in the value the next byte falls; escaped says it follows
	// a backslash in a string, head is a string's opening while it is told,
	// and passed is the bytes of the payload being passed.
	at      writtenAt
	escaped bool
	head    []byte
	passed  int64
	// open is the containers open, innermost last: an array, an object, or
	// an `inlineData` member's object; key is the last member name read and
	// valued that a colon followed it.
	open   []writtenOpen
	key    []byte
	valued bool
	out    []byte
}

// writtenAt is where a value written falls: outside every string, in a
// string's opening being told, in a data URL's payload, in an `inlineData`
// object's `data` or in another string.
type writtenAt uint8

const (
	writtenOutside writtenAt = iota
	writtenHead
	writtenImage
	writtenInline
	writtenString
)

// writtenOpen is a container a value written holds open.
type writtenOpen uint8

const (
	writtenArray writtenOpen = iota
	writtenObject
	writtenInlineObject
)

func (a *asWrittenImages) Write(p []byte) (int, error) {
	a.out = a.out[:0]
	for _, c := range p {
		switch a.at {
		case writtenOutside:
			a.outside(c)
		case writtenHead:
			a.headByte(c)
		case writtenImage, writtenInline:
			switch {
			case a.escaped:
				a.escaped = false
			case c == '\\':
				a.escaped = true
			case c == '"':
				head := a.head
				if a.at == writtenInline {
					head = nil
				}
				a.out = append(a.out, mediaPlaceholder(head, a.passed)...)
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
				a.keep(c)
			case c == '\\':
				a.escaped = true
				a.keep(c)
			case c == '"':
				a.ended()
			default:
				a.keep(c)
			}
		}
	}
	_, _ = a.w.Write(a.out)
	return len(p), nil
}

// outside takes c, a byte outside every string: a string's opening quote, an
// `inlineData` object's `data` passed from it, and the containers and names
// that tell one.
func (a *asWrittenImages) outside(c byte) {
	a.out = append(a.out, c)
	top := len(a.open) - 1
	switch c {
	case '"':
		a.at, a.head = writtenHead, a.head[:0]
		if top >= 0 && a.open[top] == writtenInlineObject && a.valued && string(a.key) == "data" {
			a.at, a.passed = writtenInline, 0
		}
	case ':':
		a.valued = true
	case ',':
		a.valued = false
	case '{':
		inline := top >= 0 && a.open[top] != writtenArray && a.valued && string(a.key) == "inlineData"
		a.open = append(a.open, writtenObject)
		if inline {
			a.open[top+1] = writtenInlineObject
		}
		a.valued = false
	case '[':
		a.open, a.valued = append(a.open, writtenArray), false
	case '}', ']':
		if top >= 0 {
			a.open = a.open[:top]
		}
	}
}

// writtenKeyMax is the most of a string kept to tell it as a member's name:
// past it, it names no member a payload is told by.
const writtenKeyMax = 16

// keep keeps c, the next byte of a string written as it is, to tell it as a
// member's name.
func (a *asWrittenImages) keep(c byte) {
	if len(a.head) <= writtenKeyMax {
		a.head = append(a.head, c)
	}
}

// ended ends a string written as it is: a member's name, where it stands for
// one, is kept as the key.
func (a *asWrittenImages) ended() {
	a.at = writtenOutside
	if top := len(a.open) - 1; top >= 0 && a.open[top] != writtenArray && !a.valued {
		a.key = a.key[:0]
		if len(a.head) <= writtenKeyMax {
			a.key = append(a.key, a.head...)
		}
	}
}

// headByte takes c, the next byte of a string's opening: a string that ends
// or escapes a byte before it is told a data URL's is written as it is.
func (a *asWrittenImages) headByte(c byte) {
	if c == '"' {
		if dataURLHead(a.head) == headMedia {
			a.out = append(a.out, mediaPlaceholder(a.head, int64(len(a.head)))...)
		} else {
			a.out = append(a.out, a.head...)
		}
		a.out = append(a.out, c)
		a.ended()
		return
	}
	a.head = append(a.head, c)
	switch told := dataURLHead(a.head); {
	case told == headMedia:
		a.at, a.passed = writtenImage, int64(len(a.head))
	case told == headNot, c == '\\':
		a.out = append(a.out, a.head...)
		a.at, a.escaped = writtenString, c == '\\'
	}
}

// passing counts a string read as text out of the line's bound where it is a
// payload: told by its head ([dataURLHead]) on a walk that passes images, or
// from its first byte where the walk reads an inline one ([lineWalk.inline]).
// Its head is the walk's ([lineWalk.head]); taken is what it read before.
type passing struct {
	telling, on bool
	taken       int64
}

// start starts a string's text on w.
func (p *passing) start(w *lineWalk) {
	*p = passing{telling: w.images && !w.inline, on: w.images && w.inline}
	w.head = w.head[:0]
}

// read takes b, the string's next bytes read.
func (p *passing) read(w *lineWalk, b []byte) {
	switch {
	case p.on:
		w.src.skipped += int64(len(b))
		return
	case !p.telling:
		return
	}
	p.taken += int64(len(b))
	w.head = append(w.head, b[:min(len(b), cap(w.head)-len(w.head))]...)
	switch dataURLHead(w.head) {
	case headMedia:
		p.on, p.telling = true, false
		w.src.skipped += p.taken
	case headNot:
		p.telling = false
	default:
		p.telling = len(w.head) < cap(w.head)
	}
}
