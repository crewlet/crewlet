package gitref

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// TagCaps bounds what a TagStream holds of each field git leaves unbounded:
// past its cap a field is counted, not held.
type TagCaps struct{ Person, Subject int }

// Held is a field's first bytes, at most its cap, and its whole length.
type Held struct {
	Bytes []byte
	Len   int
}

// Tag is one annotated tag object as a TagStream reads it.
type Tag struct {
	OID          string // the tag object's own id, whole
	Object, Type string // what it tags, from its header
	TaggerName   Held   // the bytes before the tagger's first '<', trailing whitespace dropped
	TaggerEmail  Held   // between that '<' and the next '>'
	// TaggedAt is the tagger's date in UTC; zero where the tagger line names
	// no date git reads, or the tag names no tagger.
	TaggedAt time.Time
	Subject  Held // the message's first paragraph as git's %s reads a commit's
}

// ErrNotATag is cat-file output a TagStream does not read: a record that is
// no tag object, one missing, or a stream ending inside an object.
var ErrNotATag = errors.New("gitref: cat-file --batch answered what is no tag object")

// Bounds of what a TagStream holds whole: a record's header line, a tag's
// object and type values, a header's key (a longer one is skipped) and a
// number's digits (a size, a tagger's seconds), an int64's at the most.
const (
	maxTagRecordHeader = 256
	maxTagHeaderValue  = 64
	maxTagHeaderKey    = 8
	maxInt64Digits     = 19
)

// signatureMarkers are the lines git begins a signature with (gpg-interface.c),
// any of which ends a tag's subject.
var signatureMarkers = [][]byte{
	[]byte("-----BEGIN PGP SIGNATURE-----"),
	[]byte("-----BEGIN PGP MESSAGE-----"),
	[]byte("-----BEGIN SSH SIGNATURE-----"),
	[]byte("-----BEGIN SIGNED MESSAGE-----"),
}

// The parts of a record a TagStream is in.
const (
	inRecordHeader = iota
	inObject
	inTrailer
)

// The parts of a tag object a TagStream is in.
const (
	inHeaderLine = iota // at the start of a header line, or in its key
	inHeaderValue
	inMessage
	afterSubject
)

// The parts of a tagger's ident a TagStream is in.
const (
	identLead = iota
	identName
	identEmail
	identAfterEmail
	identDate
	identAfterDate
	identZone
	identBad
)

// TagStream is a Write sink over `git cat-file --batch` of annotated tags that
// hands each to its callback, holding no field past its cap; the callback's
// error ends the run as gitproc.Lines' does.
type TagStream struct {
	caps TagCaps
	each func(Tag) error
	err  error

	part int
	head []byte
	left int64
	tag  Tag

	// Within an object: which part, the header line's key, and a held value.
	obj      int
	key      []byte
	keyLong  bool
	header   string // the header line's key once its space is read
	value    []byte
	tagger   bool // a tagger line was read: later ones are skipped
	ident    int
	name     trimmedText
	email    Held
	digits   []byte
	subject  trimmedText
	started  bool // the subject holds a line already
	content  bool // the message line in hand holds a byte that is not whitespace
	marker   []byte
	matching bool // the message line's first bytes may still be a signature marker
}

// NewTagStream is a TagStream holding each field to caps, handing each tag
// object read to each.
func NewTagStream(caps TagCaps, each func(Tag) error) *TagStream {
	return &TagStream{caps: caps, each: each}
}

// Write reads p as the stream's next bytes.
func (s *TagStream) Write(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	n := len(p)
	for len(p) > 0 && s.err == nil {
		switch s.part {
		case inRecordHeader:
			i := bytes.IndexByte(p, '\n')
			if i < 0 {
				s.head = append(s.head, p...)
				p = nil
				if len(s.head) > maxTagRecordHeader {
					s.err = fmt.Errorf("%w: a header line of %d bytes or more", ErrNotATag, len(s.head))
				}
				continue
			}
			s.head, p = append(s.head, p[:i]...), p[i+1:]
			s.err = s.begin()
		case inObject:
			take := p[:min(int64(len(p)), s.left)]
			for _, c := range take {
				if s.obj == afterSubject {
					break // the rest of the message is nothing the stream holds
				}
				s.objectByte(c)
				if s.err != nil {
					break
				}
			}
			p, s.left = p[len(take):], s.left-int64(len(take))
			if s.left == 0 && s.err == nil {
				s.endObject()
			}
		case inTrailer:
			if p[0] != '\n' {
				s.err = fmt.Errorf("%w: %q follows tag %s where its newline goes", ErrNotATag, p[0], s.tag.OID)
				break
			}
			p, s.part = p[1:], inRecordHeader
			s.err = s.each(s.tag)
		}
	}
	if s.err != nil {
		return 0, s.err
	}
	return n, nil
}

// Close reports a stream that ended inside a record, or the error that ended it.
func (s *TagStream) Close() error {
	if s.err != nil {
		return s.err
	}
	if s.part != inRecordHeader || len(s.head) > 0 {
		return fmt.Errorf("%w: the stream ended inside a record", ErrNotATag)
	}
	return nil
}

// begin reads the record header in hand: a tag object's, whose bytes follow.
func (s *TagStream) begin() error {
	line := string(s.head)
	s.head = s.head[:0]
	oid, rest, _ := bytes.Cut([]byte(line), []byte(" "))
	kind, size, ok := bytes.Cut(rest, []byte(" "))
	switch {
	case !ValidOID(oid):
		return fmt.Errorf("%w: %q names no object id", ErrNotATag, line)
	case string(rest) == "missing":
		return fmt.Errorf("%w: %s is missing", ErrNotATag, oid)
	case !ok || string(kind) != "tag":
		return fmt.Errorf("%w: %q is no tag object's header", ErrNotATag, line)
	}
	n, err := strconv.ParseInt(string(size), 10, 64)
	if err != nil || len(size) > maxInt64Digits || len(bytes.Trim(size, "0123456789")) != 0 {
		return fmt.Errorf("%w: %q names no size", ErrNotATag, line)
	}
	s.tag = Tag{OID: string(oid)}
	s.obj, s.key, s.keyLong, s.header, s.value = inHeaderLine, s.key[:0], false, "", s.value[:0]
	s.tagger, s.started, s.content, s.matching = false, false, false, true
	s.subject = trimmedText{cap: s.caps.Subject}
	s.marker = s.marker[:0]
	s.left, s.part = n, inObject
	if n == 0 {
		s.endObject()
	}
	return nil
}

// endObject ends the object at its last byte: the line in hand ended there.
func (s *TagStream) endObject() {
	if s.obj == inHeaderValue || len(s.key) > 0 || s.keyLong {
		s.headerLineEnd()
	} else if s.obj == inMessage {
		s.messageLineEnd()
	}
	s.tag.Subject = s.subject.held()
	s.part = inTrailer
}

// objectByte reads one byte of a tag object.
func (s *TagStream) objectByte(c byte) {
	switch s.obj {
	case inHeaderLine:
		switch {
		case c == '\n' && len(s.key) == 0 && !s.keyLong:
			s.obj = inMessage // the empty line that ends the headers
		case c == '\n':
			s.headerLineEnd()
		case c == ' ' && len(s.key) == 0 && !s.keyLong:
			s.header, s.obj = "", inHeaderValue // a continuation, skipped
		case c == ' ':
			s.header, s.obj = string(s.key), inHeaderValue
			if s.keyLong {
				s.header = ""
			}
			if s.header == "tagger" && !s.tagger {
				s.ident, s.name, s.email, s.digits = identLead, trimmedText{cap: s.caps.Person}, Held{}, s.digits[:0]
			}
		case len(s.key) < maxTagHeaderKey:
			s.key = append(s.key, c)
		default:
			s.keyLong = true
		}
	case inHeaderValue:
		if c == '\n' {
			s.headerLineEnd()
			return
		}
		switch s.header {
		case "object", "type":
			if len(s.value) == maxTagHeaderValue {
				s.err = fmt.Errorf("%w: tag %s's %s header runs past %d bytes", ErrNotATag, s.tag.OID, s.header,
					maxTagHeaderValue)
				return
			}
			s.value = append(s.value, c)
		case "tagger":
			if !s.tagger {
				s.identByte(c)
			}
		}
	case inMessage:
		s.messageByte(c)
	}
}

// headerLineEnd ends a header line: its value kept where it is one the stream
// holds, and the first tagger's ident read.
func (s *TagStream) headerLineEnd() {
	switch s.header {
	case "object":
		if s.tag.Object == "" {
			s.tag.Object = string(s.value)
		}
	case "type":
		if s.tag.Type == "" {
			s.tag.Type = string(s.value)
		}
	case "tagger":
		if !s.tagger {
			s.tagger = true
			s.identEnd()
		}
	}
	s.obj, s.key, s.keyLong, s.header, s.value = inHeaderLine, s.key[:0], false, "", s.value[:0]
}

// identByte reads one byte of the tagger's ident: name <email> seconds zone.
func (s *TagStream) identByte(c byte) {
	switch s.ident {
	case identLead:
		if space(c) {
			return
		}
		s.ident = identName
		s.identByte(c)
	case identName:
		if c == '<' {
			s.tag.TaggerName = s.name.held()
			s.ident = identEmail
			return
		}
		s.name.add(c)
	case identEmail:
		if c == '>' {
			s.tag.TaggerEmail = s.email
			s.ident = identAfterEmail
			return
		}
		s.email.add(c, s.caps.Person)
	case identAfterEmail, identDate:
		switch {
		case '0' <= c && c <= '9' && len(s.digits) < maxInt64Digits:
			s.digits, s.ident = append(s.digits, c), identDate
		case space(c) && s.ident == identAfterEmail:
		case space(c):
			s.ident = identAfterDate
		default:
			s.ident = identBad
		}
	case identAfterDate:
		if !space(c) {
			s.ident = identZone
		}
	}
}

// identEnd ends the tagger's line: a date stands where seconds and a zone
// after them were read.
func (s *TagStream) identEnd() {
	if s.ident != identZone {
		return
	}
	if n, err := strconv.ParseInt(string(s.digits), 10, 64); err == nil {
		s.tag.TaggedAt = time.Unix(n, 0).UTC()
	}
}

// messageByte reads one byte of the message, to the end of its subject.
func (s *TagStream) messageByte(c byte) {
	if c == '\n' {
		s.messageLineEnd()
		return
	}
	if !s.matching {
		s.subjectByte(c)
		return
	}
	s.marker = append(s.marker, c)
	prefix := false
	for _, m := range signatureMarkers {
		if bytes.Equal(m, s.marker) {
			s.obj = afterSubject
			return
		}
		prefix = prefix || bytes.HasPrefix(m, s.marker)
	}
	if !prefix {
		s.replayMarker()
	}
}

// replayMarker reads a line's first bytes, held while they might begin a
// signature marker, as the line's own.
func (s *TagStream) replayMarker() {
	s.matching = false
	for _, b := range s.marker {
		s.subjectByte(b)
	}
	s.marker = s.marker[:0]
}

// subjectByte reads one byte of a message line that is no marker.
func (s *TagStream) subjectByte(c byte) {
	if !space(c) && !s.content {
		s.content = true
		if s.started {
			s.subject.sep()
		}
	}
	s.subject.add(c)
}

// messageLineEnd ends a message line: a blank one before the subject is
// skipped, and one after it ends it.
func (s *TagStream) messageLineEnd() {
	if s.obj != inMessage {
		return
	}
	if s.matching {
		s.replayMarker()
	}
	s.subject.endLine()
	switch {
	case s.content:
		s.started = true
	case s.started:
		s.obj = afterSubject
	}
	s.content, s.matching = false, true
}

// add counts b into h, holding it where h is short of capacity.
func (h *Held) add(b byte, capacity int) {
	h.Len++
	if len(h.Bytes) < capacity {
		h.Bytes = append(h.Bytes, b)
	}
}

// trimmedText is a field read a byte at a time whose lines lose their trailing
// whitespace: whitespace is held back until a byte that is not whitespace
// follows it on its line.
type trimmedText struct {
	h   Held
	cap int
	// ws is the whitespace held back, at most cap bytes of it, and wsLen how
	// much there was; space that a line is joined with goes before it.
	ws    []byte
	wsLen int
	join  bool
}

func (t *trimmedText) add(c byte) {
	if space(c) {
		t.wsLen++
		if len(t.ws) < t.cap {
			t.ws = append(t.ws, c)
		}
		return
	}
	if t.join {
		t.h.add(' ', t.cap)
		t.join = false
	}
	for _, b := range t.ws {
		t.h.add(b, t.cap)
	}
	t.h.Len += t.wsLen - len(t.ws)
	t.ws, t.wsLen = t.ws[:0], 0
	t.h.add(c, t.cap)
}

// sep joins the next line to the text with one space.
func (t *trimmedText) sep() { t.join = true }

// endLine drops the whitespace a line ended with.
func (t *trimmedText) endLine() { t.ws, t.wsLen = t.ws[:0], 0 }

// held is the text as read, its trailing whitespace dropped.
func (t *trimmedText) held() Held { return t.h }

// space is git's isspace: the whitespace a line's end loses.
func space(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\v' || c == '\f' || c == '\r'
}
