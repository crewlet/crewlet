package executor

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/jsonscan"
	"github.com/crewlet/crewlet/internal/redact"
)

// THE SCRUB'S REWRITE OF JSON, a token at a time on internal/jsonscan, holding
// no name, string or number whole (see the file comment of scrub.go).
//
// Every name and string is read from the scanner as the text it decodes to, a
// piece at a time, scrubbed by value and by shape as it streams
// ([scrubber.text]) and written again as a JSON string ([jsonStringWriter]); a
// number is written as its literal reads; and a string or a number under a
// credential's key ([keyReading]) is written as [credentialMarker]. A text
// short enough to be read whole in one piece is scrubbed whole
// ([textWholeBytes]), which is what the streams themselves do with a text that
// short: what each writes, put together, is exactly the whole-text pass of
// everything written to it.

// textWholeBytes is the longest text of a name or a string the rewrite reads
// whole and scrubs in one call, rather than through the streams: the scanner's
// own read buffer, so a text that fits is one read of it.
//
// AN ECONOMY, NEVER A RULE: the streams write exactly what the whole-text
// passes write for any text ([valueStream], redact.Stream), so where a text is
// cut between the two changes no byte of the kept session. A transcript holds
// millions of short names and strings, and each through the streams would
// cost their allocations for nothing.
const textWholeBytes = 64 << 10

// errRedaction is a text the scrub could not redact, which refuses the
// session: what the redaction answered is read off the error it wraps.
var errRedaction = errors.New("executor: the session's text could not be redacted")

// textScrub is the scrub of one text that arrives in pieces: by value
// ([valueStream]), then by shape (redact.Stream, or a reading's stage before
// it, [pieceSink]), onto a writer. What it writes, put together, is
// redact.Secrets of [scrubber.apply] of the whole text.
type textScrub struct {
	values *valueStream
	piece  pieceSink
	out    *textOut
	// in is the charge of what was written to it and taken what its scrub
	// took out, once it is closed, where its scrubber tallies ([escCharge]).
	in, taken int64
}

// textOut is where a [textScrub] writes, keeping the first error its writer
// gave.
//
// SO A REFUSAL IS TOLD FROM A FAILED WRITE: redact.Stream's Close answers its
// writer's first error as it answers a refusal of its own, and a writer's
// error (the take's pipe its reader closed, a tar writer that failed) is no
// text the redaction could not redact. The one it gave is handed on as it
// came, and only the stream's own answer is [errRedaction].
type textOut struct {
	w   io.Writer
	err error
	// own reads what is written for its own words, where the scrub tallies.
	own *ownCharge
}

func (o *textOut) Write(p []byte) (int, error) {
	n, err := o.w.Write(p)
	if o.own != nil && n > 0 {
		o.own.write(p[:n])
	}
	if err != nil && o.err == nil {
		o.err = err
	}
	return n, err
}

// text is s's scrub of a text a piece at a time, writing onto w, staged where
// s's reading stages it.
func (s scrubber) text(w io.Writer) *textScrub {
	out := &textOut{w: w}
	if s.tally {
		out.own = newOwnCharge()
	}
	var piece pieceSink
	if s.first != nil {
		piece = s.first.open(out)
	} else {
		piece = redact.NewStream(out)
	}
	return &textScrub{values: s.stream(piece), piece: piece, out: out}
}

// Write scrubs the next piece of the text. Its errors are Close's: the
// redaction's stream refuses at the write where it finds a window it decided
// too early, while more of the text is still to come, as readily as at its
// close, and that refusal is [errRedaction] wherever it is found.
func (t *textScrub) Write(p []byte) (int, error) {
	n, err := t.values.Write(p)
	if t.out.own != nil {
		t.in += escCharge(p[:n])
	}
	return n, redactionRefusal(err, t.out.err)
}

// Close writes the rest of the text. An error of the redaction's own is
// [errRedaction], wrapping what it answered; one its writer gave is that
// error, as it came ([textOut]).
func (t *textScrub) Close() error {
	if err := t.values.Close(); err != nil {
		return redactionRefusal(err, t.out.err)
	}
	if err := redactionRefusal(t.piece.Close(), t.out.err); err != nil {
		return err
	}
	t.taken = t.out.own.taken(t.in)
	return nil
}

// took is what the text's scrub took out, once it is closed, where its
// scrubber tallies.
func (t *textScrub) took() int64 { return t.taken }

// redactionRefusal is what a text's scrub answers for err, what its redaction's
// stream answered at its close, wrote the first error its writer gave:
// nothing for nothing, the writer's own error as it came, a stage's own
// ([errMemoDiverged]) as it came, and anything else the redaction's own
// refusal, [errRedaction] wrapping it.
func redactionRefusal(err, wrote error) error {
	switch {
	case err == nil:
		return nil
	case wrote != nil && errors.Is(err, wrote),
		errors.Is(err, errMemoDiverged):
		return err
	}
	return fmt.Errorf("%w: %w", errRedaction, err)
}

// keptText is a whole text as the scrub keeps it: by value, then by shape.
func keptText(s scrubber, text string) string {
	return redact.Secrets(s.apply(text))
}

// textSink is what a rewrite writes onto: a buffered writer, or a unit's way
// ([candidate]).
type textSink interface {
	io.Writer
	io.StringWriter
	io.ByteWriter
}

// rewriteOut is where a rewrite writes: it counts every byte, hands them to w
// unless the rewrite only counts (w nil), and, where watch is set, says whether
// [redact.Marker] is among them, which a rewritten transcript line's cut reads
// ([definitionsReading]).
type rewriteOut struct {
	w textSink
	n int64
	// watch says the marker is looked for; tail is the last bytes written,
	// one fewer than the marker, and marked says the marker was met.
	watch  bool
	tail   []byte
	marked bool
	// taken is what the rewrite's scrub took out of what it wrote
	// ([tallier]), which its caller hands on where the bytes go.
	taken int64
}

// reset readies o for another text, onto w.
func (o *rewriteOut) reset(w textSink, watch bool) {
	o.w, o.n, o.watch, o.tail, o.marked, o.taken = w, 0, watch, o.tail[:0], false, 0
}

func (o *rewriteOut) tally(taken int64) { o.taken += taken }

// write writes p.
func (o *rewriteOut) write(p []byte) error {
	if len(p) == 0 {
		return nil
	}
	o.n += int64(len(p))
	if o.watch && !o.marked {
		o.look(p)
	}
	if o.w == nil {
		return nil
	}
	_, err := o.w.Write(p)
	return err
}

// writeString writes s.
func (o *rewriteOut) writeString(s string) error {
	if len(s) == 0 {
		return nil
	}
	o.n += int64(len(s))
	if o.watch && !o.marked {
		o.look([]byte(s))
	}
	if o.w == nil {
		return nil
	}
	_, err := o.w.WriteString(s)
	return err
}

// writeByte writes c.
func (o *rewriteOut) writeByte(c byte) error {
	o.n++
	if o.watch && !o.marked {
		o.look([]byte{c})
	}
	if o.w == nil {
		return nil
	}
	return o.w.WriteByte(c)
}

// look reads p, the next bytes written, for the marker: within p, or begun in
// what was written before it.
func (o *rewriteOut) look(p []byte) {
	const keep = len(redact.Marker) - 1
	var room [2 * keep]byte
	joined := append(append(room[:0], o.tail...), p[:min(len(p), keep)]...)
	if bytes.Contains(joined, []byte(redact.Marker)) || bytes.Contains(p, []byte(redact.Marker)) {
		o.marked = true
		return
	}
	if len(p) >= keep {
		o.tail = append(o.tail[:0], p[len(p)-keep:]...)
		return
	}
	o.tail = append(o.tail[:0], joined[max(0, len(joined)-keep):]...)
}

// jsonStringWriter writes a text that arrives in pieces as one JSON string,
// escaped as plainjson escapes: what it writes for a text cut anywhere is
// plainjson.Marshal of the whole text, which a test holds it to.
//
// A PIECE MAY END INSIDE A RUNE (redact.Stream cuts its writes anywhere), so a
// rune's cut bytes wait for the next piece rather than each being written as
// U+FFFD; at the text's end ([jsonStringWriter.end]) they are read as the
// whole text's encoding reads them. It keeps the text's first capMax bytes as
// written, for a reader that judges a short text by what it says
// ([definitionsReading]); over says the text ran past them.
type jsonStringWriter struct {
	out   *rewriteOut
	held  [utf8.UTFMax - 1]byte
	nheld int
	// capture is the text's first bytes, at most capMax; over says it ran
	// past them.
	capture []byte
	capMax  int
	over    bool
	err     error
	// media measures the text as kept for a payload, nil where nothing does
	// ([jsonStringWriter.finding]).
	media *payloadSpan
}

// begin opens a string onto out, keeping its first capMax bytes.
func (e *jsonStringWriter) begin(out *rewriteOut, capMax int) error {
	e.out, e.nheld, e.capture, e.capMax, e.over, e.err, e.media = out, 0, e.capture[:0], capMax, false, nil, nil
	return out.writeByte('"')
}

// finding has span measure the string just begun, where its scrub classes it
// a payload ([payloadScrub]).
func (e *jsonStringWriter) finding(span *payloadSpan) {
	span.begin(e.out.n)
	e.media = span
}

// Write writes the next piece of the text, escaped.
func (e *jsonStringWriter) Write(p []byte) (int, error) {
	if e.err != nil {
		return 0, e.err
	}
	n := len(p)
	if e.capMax > 0 && !e.over {
		room := e.capMax - len(e.capture)
		e.capture = append(e.capture, p[:min(len(p), room)]...)
		e.over = len(p) > room
	}
	if e.nheld > 0 {
		// The rune a piece ended inside, finished from this one: at most
		// three of its bytes are needed, and a run that is no rune is
		// escaped a byte at a time, as the whole text's encoding reads it.
		var joined [2*utf8.UTFMax - 2]byte
		s := append(joined[:0], e.held[:e.nheld]...)
		s = append(s, p[:min(len(p), utf8.UTFMax-1)]...)
		i := 0
		for i < e.nheld {
			if !utf8.FullRune(s[i:]) {
				// The whole of p is in s, and still no rune.
				e.nheld = copy(e.held[:], s[i:])
				return n, nil
			}
			_, w := utf8.DecodeRune(s[i:])
			if e.err = e.escape(s[i : i+w]); e.err != nil {
				return 0, e.err
			}
			i += w
		}
		p = p[i-e.nheld:]
		e.nheld = 0
	}
	whole := len(p)
	for at := max(0, len(p)-(utf8.UTFMax-1)); at < len(p); at++ {
		if utf8.RuneStart(p[at]) && !utf8.FullRune(p[at:]) {
			whole = at
			break
		}
	}
	if e.err = e.escape(p[:whole]); e.err != nil {
		return 0, e.err
	}
	e.nheld = copy(e.held[:], p[whole:])
	return n, nil
}

// end writes what waits of the text, as its last bytes, and closes the string.
func (e *jsonStringWriter) end() error {
	if e.err != nil {
		return e.err
	}
	if err := e.escape(e.held[:e.nheld]); err != nil {
		return err
	}
	e.nheld = 0
	return e.out.writeByte('"')
}

// escape writes s, whole runes or the text's last bytes, escaped as
// plainjson escapes a string: `"` and `\` and every byte below a space escaped
// (`\b`, `\f`, `\n`, `\r` and `\t` by name, the rest as `\u00XX`), U+2028 and
// U+2029 as their escapes, each byte that begins no rune as U+FFFD, and every
// other byte as it is, `<`, `>` and `&` among them.
func (e *jsonStringWriter) escape(s []byte) error {
	if e.media != nil {
		e.media.feed(s)
	}
	start := 0
	flush := func(i int) error {
		err := e.out.write(s[start:i])
		return err
	}
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			if c >= ' ' && c != '"' && c != '\\' {
				i++
				continue
			}
			if err := flush(i); err != nil {
				return err
			}
			if err := e.out.writeString(escapedASCII(c)); err != nil {
				return err
			}
			i++
			start = i
			continue
		}
		r, w := utf8.DecodeRune(s[i:])
		var with string
		switch {
		case r == utf8.RuneError && w == 1:
			with = "\uFFFD"
		case r == '\u2028':
			with = `\u2028`
		case r == '\u2029':
			with = `\u2029`
		default:
			i += w
			continue
		}
		if err := flush(i); err != nil {
			return err
		}
		if err := e.out.writeString(with); err != nil {
			return err
		}
		i += w
		start = i
	}
	return flush(len(s))
}

// escapedASCII is how plainjson escapes an ASCII byte that needs it.
func escapedASCII(c byte) string {
	switch c {
	case '"':
		return `\"`
	case '\\':
		return `\\`
	case '\b':
		return `\b`
	case '\f':
		return `\f`
	case '\n':
		return `\n`
	case '\r':
		return `\r`
	case '\t':
		return `\t`
	}
	const hex = "0123456789abcdef"
	return string([]byte{'\\', 'u', '0', '0', hex[c>>4], hex[c&0xf]})
}

// jsonRewrite rewrites one JSON text read from a scanner, token by token, onto
// a [rewriteOut]: every name and string scrubbed and written again as a JSON
// string, every number, true, false and null as written, a string or a number
// under a credential's key as [credentialMarker], and no blank between tokens.
// A text the scanner refuses ends the rewrite on its *jsonscan.Error, what the
// rewrite wrote of it before counted and not taken back.
//
// For a transcript's line it also reads the cut of a loaded tool definition
// the scrub renamed ([definitionsReading]), which a tar's second reading
// applies as it copies the line ([cutFilter]).
type jsonRewrite struct {
	sc    *jsonscan.Scanner
	out   *rewriteOut
	scrub scrubber
	// frames are the containers open, the outermost first.
	frames []rewriteFrame
	// buf is a piece of a token's text read from the scanner; str writes a
	// string; key reads a name for a credential's.
	buf []byte
	str jsonStringWriter
	key keyReading
	// payload measures a string value its scrub classed a payload, where
	// the scrub finds media ([scrubber.media]).
	payload payloadSpan
	// held is the room a string value is held in while it is read for a
	// payload ([payloadHeldBytes]).
	held []byte
	// containers is how many containers the text has opened so far, which
	// names the list a cut applies to.
	containers int
	// defs reads the cut of a transcript line's loaded tool definitions, nil
	// for a document.
	defs *definitionsReading
	// gate is held while a name or a string past [scrubPieceBaseBytes] is
	// scrubbed: an export's, whose text is never one piece of its own.
	gate pieceGate
}

// rewriteFrame is one open object or array.
type rewriteFrame struct {
	object bool
	// n is how many members (an object's pairs, an array's values) are
	// written.
	n int
	// credential says the member being read is under a credential's key.
	credential bool
	// role is what the frame is to a transcript line's cut, and member which
	// of the names the role reads the member being read is under.
	role   frameRole
	member readName
}

// frameRole is what a container is to the cut of a transcript line's loaded
// tool definitions: the line's own object, its loaded-tools attachment, that
// attachment's list of definitions, or one definition; none for every other.
type frameRole uint8

const (
	roleNone frameRole = iota
	roleLine
	roleAttachment
	roleEntries
	roleDefinition
)

// readName is which name a member of a container with a role is under, of
// those the cut reads: the line's `attachment`, the attachment's `type` and
// `entries`, a definition's `name`; other for every other.
type readName uint8

const (
	readOther readName = iota
	readAttachment
	readType
	readEntries
	readToolName
)

// namesRead is the names each role reads, as the rewrite writes them.
var namesRead = map[frameRole]map[string]readName{
	roleLine:       {"attachment": readAttachment},
	roleAttachment: {"type": readType, "entries": readEntries},
	roleDefinition: {"name": readToolName},
}

// nameCaptureBytes is the most of a name the cut reads: one byte past the
// longest of [namesRead], so a longer name reads as none of them.
const nameCaptureBytes = len("attachment") + 1

// newJSONRewrite is a rewrite with s's values, its buffers allocated once for
// every text it rewrites.
func newJSONRewrite(s scrubber) *jsonRewrite {
	return &jsonRewrite{scrub: s, buf: make([]byte, textWholeBytes)}
}

// reset readies j to rewrite the text sc reads onto out, reading a cut where
// defs is set.
func (j *jsonRewrite) reset(sc *jsonscan.Scanner, out *rewriteOut, defs *definitionsReading) {
	j.sc, j.out, j.frames, j.containers, j.defs = sc, out, j.frames[:0], 0, defs
	if defs != nil {
		defs.reset()
	}
}

// run rewrites the text to its end: nil once the scanner has read one whole
// JSON value, and else what ended it.
func (j *jsonRewrite) run() error {
	for {
		k, err := j.sc.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err = j.token(k); err != nil {
			return err
		}
	}
}

// frame is the innermost container open, nil outside every one.
func (j *jsonRewrite) frame() *rewriteFrame {
	if len(j.frames) == 0 {
		return nil
	}
	return &j.frames[len(j.frames)-1]
}

func (j *jsonRewrite) token(k jsonscan.Kind) error {
	switch k {
	case jsonscan.ObjectEnd, jsonscan.ArrayEnd:
		return j.close(k)
	case jsonscan.Name:
		return j.name()
	}
	if err := j.beginValue(); err != nil {
		return err
	}
	switch k {
	case jsonscan.ObjectStart, jsonscan.ArrayStart:
		return j.open(k)
	case jsonscan.String:
		if err := j.stringValue(); err != nil {
			return err
		}
	case jsonscan.Number:
		if err := j.number(); err != nil {
			return err
		}
	case jsonscan.True:
		if err := j.out.writeString("true"); err != nil {
			return err
		}
	case jsonscan.False:
		if err := j.out.writeString("false"); err != nil {
			return err
		}
	case jsonscan.Null:
		if err := j.out.writeString("null"); err != nil {
			return err
		}
	default:
		return fmt.Errorf("executor: a JSON token of kind %q", k)
	}
	j.endValue(k)
	return nil
}

// beginValue writes what comes before a value: a comma between an array's
// values (an object's value follows its name's colon). The value's first byte
// is the next written, which the cut's reading marks for an element of the
// list it reads.
func (j *jsonRewrite) beginValue() error {
	f := j.frame()
	if f != nil && !f.object && f.n > 0 {
		if err := j.out.writeByte(','); err != nil {
			return err
		}
	}
	if j.defs != nil {
		j.defs.valueBegins(j, f)
	}
	return nil
}

// endValue counts a value written, of kind k: its last byte is the last
// written.
func (j *jsonRewrite) endValue(k jsonscan.Kind) {
	f := j.frame()
	if j.defs != nil {
		j.defs.valueEnds(j, f, k)
	}
	if f != nil {
		f.n++
	}
}

// open writes a container's opening and opens its frame.
func (j *jsonRewrite) open(k jsonscan.Kind) error {
	object := k == jsonscan.ObjectStart
	c := byte('[')
	if object {
		c = '{'
	}
	if err := j.out.writeByte(c); err != nil {
		return err
	}
	j.containers++
	f := rewriteFrame{object: object}
	if j.defs != nil {
		f.role = roleFor(j.frame(), object)
	}
	j.frames = append(j.frames, f)
	if j.defs != nil {
		j.defs.opened(j, &j.frames[len(j.frames)-1])
	}
	return nil
}

// roleFor is the role of a container opened as the value of parent's member
// being read (or as the line's own value, parent nil): the line's object; the
// line's `attachment` where it is an object; its `entries` where it is a list;
// and each object in that list a definition.
func roleFor(parent *rewriteFrame, object bool) frameRole {
	switch {
	case parent == nil:
		if object {
			return roleLine
		}
	case parent.role == roleLine && parent.member == readAttachment && object:
		return roleAttachment
	case parent.role == roleAttachment && parent.member == readEntries && !object:
		return roleEntries
	case parent.role == roleEntries && object:
		return roleDefinition
	}
	return roleNone
}

// close writes a container's closing and closes its frame.
func (j *jsonRewrite) close(k jsonscan.Kind) error {
	f := j.frames[len(j.frames)-1]
	j.frames = j.frames[:len(j.frames)-1]
	c := byte(']')
	if k == jsonscan.ObjectEnd {
		c = '}'
	}
	if err := j.out.writeByte(c); err != nil {
		return err
	}
	if j.defs != nil {
		j.defs.closed(j, &f)
	}
	j.endValue(k)
	return nil
}

// name writes an object's member name, as it is kept, and reads it for a
// credential's.
//
// THE NAME AS IT IS KEPT: a name is a string a tool's structured result can
// name anything by, a secret included. Judged as a credential's by what it
// said ([keyReading]), and written as what is kept.
func (j *jsonRewrite) name() error {
	f := j.frame()
	if f.n > 0 {
		if err := j.out.writeByte(','); err != nil {
			return err
		}
	}
	reads := namesRead[f.role]
	capMax := 0
	if reads != nil {
		capMax = nameCaptureBytes
	}
	j.key.reset()
	if err := j.text(&j.key, capMax); err != nil {
		return err
	}
	if err := j.out.writeByte(':'); err != nil {
		return err
	}
	f.credential = j.key.Credential()
	f.member = readOther
	if reads != nil && !j.str.over {
		f.member = reads[string(j.str.capture)]
	}
	return nil
}

// stringValue writes a string value as it is kept: under a credential's key,
// [credentialMarker] where it is not empty, and else scrubbed.
func (j *jsonRewrite) stringValue() error {
	if f := j.frame(); f != nil && f.credential {
		return j.credentialValue()
	}
	return j.text(nil, j.defs.valueCapture(j.frame()))
}

// credentialValue writes a string under a credential's key: the marker once
// its first piece shows it is not empty, the rest passed over unread, and an
// empty one as the empty string it is.
func (j *jsonRewrite) credentialValue() error {
	for {
		n, err := j.sc.Read(j.buf)
		switch {
		case n > 0:
			if terr := j.takenWhole(escCharge(j.buf[:n]), err); terr != nil {
				return terr
			}
			return j.marker()
		case errors.Is(err, io.EOF):
			return j.out.writeString(`""`)
		case err != nil:
			return err
		}
	}
}

// marker writes [credentialMarker] as a JSON string, where the value under a
// credential's key stood.
func (j *jsonRewrite) marker() error {
	if err := j.str.begin(j.out, 0); err != nil {
		return err
	}
	if _, err := j.str.Write([]byte(credentialMarker)); err != nil {
		return err
	}
	return j.str.end()
}

// number writes a number: as its literal reads, a piece at a time; under a
// credential's key, as [credentialMarker].
//
// A NUMBER UNDER A CREDENTIAL'S KEY goes as a string under one does (a PIN, a
// numeric password, an account's key), written as the marker, a string where
// the number stood: no shape places a bare number, and kept as it came it
// survived every pass.
func (j *jsonRewrite) number() error {
	if f := j.frame(); f != nil && f.credential {
		if err := j.takenWhole(0, nil); err != nil {
			return err
		}
		return j.marker()
	}
	for {
		n, err := j.sc.Read(j.buf)
		if n > 0 {
			if werr := j.out.write(j.buf[:n]); werr != nil {
				return werr
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// takenWhole tallies the value the rewrite takes out whole, where its scrub
// tallies: charged so far before err, and the rest the scanner still holds,
// read on. Nothing is read where it does not tally: the rest is passed over.
func (j *jsonRewrite) takenWhole(charged int64, err error) error {
	if !j.scrub.tally {
		return nil
	}
	for err == nil {
		var n int
		n, err = j.sc.Read(j.buf)
		charged += escCharge(j.buf[:n])
	}
	if !errors.Is(err, io.EOF) {
		return err
	}
	j.out.tally(charged)
	return nil
}

// text writes the current name's or string's text, scrubbed, as a JSON string,
// handing each piece of what it said to key where key is set, and keeping the
// first capMax bytes of what is kept ([jsonStringWriter]).
//
// WHOLE WHERE IT IS SHORT: a text that ends within [textWholeBytes] is scrubbed
// in one call, and a longer one through the streams, its first piece and every
// one after it.
func (j *jsonRewrite) text(key *keyReading, capMax int) error {
	if err := j.str.begin(j.out, capMax); err != nil {
		return err
	}
	if key == nil && j.scrub.media != nil {
		j.str.finding(&j.payload)
	}
	read := 0
	for {
		n, err := j.sc.Read(j.buf[read:])
		read += n
		if errors.Is(err, io.EOF) {
			if key != nil {
				_, _ = key.Write(j.buf[:read])
			}
			text := string(j.buf[:read])
			kept := keptText(j.scrub, text)
			if j.scrub.tally {
				j.out.tally(takenFrom(text, kept))
			}
			if _, werr := j.str.Write([]byte(kept)); werr != nil {
				return werr
			}
			return j.ended()
		}
		if err != nil {
			return err
		}
		if len(j.buf)-read < utf8.UTFMax {
			break
		}
	}
	var t stringScrub
	if key == nil && j.scrub.media != nil {
		t = j.scrub.payloadText(&j.str, &j.payload, &j.held)
	} else {
		t = j.scrub.text(&j.str)
	}
	total, held := 0, false
	defer func() {
		if held {
			j.gate.drop()
		}
	}()
	for {
		if key != nil {
			_, _ = key.Write(j.buf[:read])
		}
		// A TEXT PAST THE BASE holds the slot before its redaction holds more.
		if total += read; !held && j.gate.hold != nil && total > scrubPieceBaseBytes {
			if err := j.gate.hold(); err != nil {
				return err
			}
			held = true
		}
		if _, err := t.Write(j.buf[:read]); err != nil {
			return err
		}
		n, err := j.sc.Read(j.buf)
		read = n
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
	}
	if err := t.Close(); err != nil {
		return err
	}
	j.out.tally(t.took())
	return j.ended()
}

// ended closes the string, telling the scrub's plan of the payload it is
// ([payloadSpan]).
func (j *jsonRewrite) ended() error {
	if err := j.str.end(); err != nil {
		return err
	}
	if j.str.media != nil {
		if at, n, sum, ok := j.payload.payload(); ok {
			j.scrub.media.add(at, n, sum)
		}
	}
	return nil
}

// definitionsReading is the first reading's half of the cut of a transcript
// line's loaded tool definitions ([loadedToolsRecord] says which and why): it
// reads, as the line is rewritten, whether the rewritten line holds
// [redact.Marker], which loaded-tools attachment JSON.parse reads (the last
// `attachment`, and of it the last `type` and `entries`), and of each list
// which definitions are broken and how many bytes the rewrite wrote of them.
// Once the line is read whole, [definitionsReading.decide] says whether it
// loses definitions and which.
//
// ITS STATE IS A BIT FOR EACH ELEMENT of the list being read and of the last
// one read before it, so at most a bit for each two bytes of the line.
type definitionsReading struct {
	// chosen is what the line's last attachment read holds, the one
	// JSON.parse reads; none where the last was no object.
	chosen attachmentRead
	// attachment is the attachment being read, and list the list being
	// read in it.
	attachment attachmentRead
	list       listRead
	// named says the definition being read is named by what the provider
	// takes, by its last `name` read so far; elementAt is where the element
	// being read began in what the rewrite wrote.
	named     bool
	elementAt int64
}

// attachmentRead is what one loaded-tools attachment holds, as the cut reads
// it: whether its last `type` is the record's ([loadedToolsRecord]), and its
// last `entries` where that is a list.
type attachmentRead struct {
	loaded bool
	list   listRead
}

// listRead is one list of definitions as the cut reads it: whether it was read
// whole, which container of the line it is, a bit for each element (set for
// one broken), how many there are and are broken, and how many bytes the
// rewrite wrote of the broken ones.
type listRead struct {
	whole       bool
	container   int
	bits        bitList
	n, broken   int
	brokenBytes int64
}

// reset readies d for another line.
func (d *definitionsReading) reset() {
	d.chosen.loaded, d.attachment.loaded = false, false
	d.chosen.list.clear()
	d.attachment.list.clear()
	d.list.clear()
}

// clear empties l, keeping its bits' room.
func (l *listRead) clear() {
	l.whole, l.container, l.n, l.broken, l.brokenBytes = false, 0, 0, 0, 0
	l.bits.clear()
}

// take makes l what from read, from left holding l's bits until it is
// cleared: swapped, never copied, so a list's bits are never held twice.
func (l *listRead) take(from *listRead) {
	l.whole, l.container, l.n, l.broken, l.brokenBytes = from.whole, from.container, from.n, from.broken,
		from.brokenBytes
	l.bits, from.bits = from.bits, l.bits
	from.clear()
}

// valueCapture is how much of a string value under parent's member being read
// the cut reads: the attachment's `type`, one byte past the record's name; a
// definition's `name`, one byte past the provider's longest; none of every
// other, and none where d is nil.
func (d *definitionsReading) valueCapture(parent *rewriteFrame) int {
	if d == nil || parent == nil {
		return 0
	}
	switch {
	case parent.role == roleAttachment && parent.member == readType:
		return len(loadedToolsRecord) + 1
	case parent.role == roleDefinition && parent.member == readToolName:
		return providerToolNameMax + 1
	}
	return 0
}

// valueBegins reads a value of parent's beginning: an element of the list
// being read begins at the next byte written, and a line's `attachment` that
// is no object is an attachment that holds no list.
func (d *definitionsReading) valueBegins(j *jsonRewrite, parent *rewriteFrame) {
	if parent == nil {
		return
	}
	if parent.role == roleEntries {
		d.elementAt = j.out.n
		d.named = false
	}
}

// opened reads a container opened, f its frame: an attachment, a list or a
// definition begun.
func (d *definitionsReading) opened(j *jsonRewrite, f *rewriteFrame) {
	switch f.role {
	case roleAttachment:
		d.attachment.loaded = false
		d.attachment.list.clear()
	case roleEntries:
		d.list.clear()
		d.list.container = j.containers
	case roleDefinition:
		d.named = false
	}
}

// closed reads a container closed, f its frame: a list read whole is its
// attachment's last `entries`, and an attachment read whole the line's last.
func (d *definitionsReading) closed(_ *jsonRewrite, f *rewriteFrame) {
	switch f.role {
	case roleEntries:
		d.list.whole = true
		d.attachment.list.take(&d.list)
	case roleAttachment:
		d.chosen.loaded = d.attachment.loaded
		d.chosen.list.take(&d.attachment.list)
	}
}

// valueEnds reads a value of kind k ended, parent the container it is in: the
// member of a role's container it is the value of, or an element of the list
// being read.
func (d *definitionsReading) valueEnds(j *jsonRewrite, parent *rewriteFrame, k jsonscan.Kind) {
	if parent == nil {
		return
	}
	switch parent.role {
	case roleLine:
		if parent.member == readAttachment && k != jsonscan.ObjectEnd {
			// AN ATTACHMENT THAT IS NO OBJECT is the one JSON.parse reads
			// all the same, and holds no list.
			d.chosen.loaded = false
			d.chosen.list.clear()
		}
	case roleAttachment:
		switch parent.member {
		case readType:
			d.attachment.loaded = k == jsonscan.String && !j.str.over &&
				string(j.str.capture) == loadedToolsRecord
		case readEntries:
			if k != jsonscan.ArrayEnd {
				// AN `entries` THAT IS NO LIST is the last one all the
				// same, and none JSON.parse reads definitions from.
				d.attachment.list.clear()
			}
		}
	case roleDefinition:
		if parent.member == readToolName {
			d.named = k == jsonscan.String && !j.str.over && providerToolName(string(j.str.capture))
		}
	case roleEntries:
		broken := k != jsonscan.ObjectEnd || !d.named
		d.list.bits.add(broken)
		d.list.n++
		if broken {
			d.list.broken++
			d.list.brokenBytes += j.out.n - d.elementAt
		}
	}
}

// decide is the line's cut, once the line is read whole as one JSON value: the
// list JSON.parse reads, its bits among it, which the plan takes
// ([tarPlan.addCut]), and how many bytes the cut takes from the line; none
// where the line holds no marker (the rewrite renamed nothing in it), its
// attachment is no loaded-tools record or holds no list, or no definition in
// it is broken.
func (d *definitionsReading) decide(marked bool) (list *listRead, taken int64, ok bool) {
	l := &d.chosen.list
	if !marked || !d.chosen.loaded || !l.whole || l.broken == 0 {
		return nil, 0, false
	}
	// THE COMMAS GO WITH THEM: n elements are joined by n-1, and the k kept
	// by k-1, or none.
	kept := l.n - l.broken
	return l, l.brokenBytes + int64(l.n-1-max(kept-1, 0)), true
}

// definitionCut is the second reading's half of the cut of one line: the list
// it applies to, as the line's containers count it, how many elements the
// first reading read in it, and the plan's bits for them, one an element, set
// for one cut, read an element at a time as the copy reads the list
// ([cutFilter]).
type definitionCut struct {
	list    int
	n, read int
	bits    *planBits
}

// next says whether the list's next element is cut. A list of more elements
// than the first reading read is a spool read otherwise than it was written.
func (c *definitionCut) next() (bool, error) {
	if c.read == c.n {
		return false, errSpoolRead
	}
	cut, ok := c.bits.take()
	if !ok {
		return false, errSpoolRead
	}
	c.read++
	return cut, nil
}

// bitList is a list of bits that grows a block at a time: a plan's, or a
// list's of definitions, which can hold a bit for every few bytes of a
// reading's plaintext, so it never holds twice its bits while it grows, as a
// slice grown by append does.
type bitList struct {
	blocks [][]uint64
	n      int
}

// bitBlockWords is a [bitList]'s block: 4 KiB.
const bitBlockWords = 512

// add appends v.
func (b *bitList) add(v bool) {
	word := b.n / 64
	block, at := word/bitBlockWords, word%bitBlockWords
	if block == len(b.blocks) {
		b.blocks = append(b.blocks, make([]uint64, bitBlockWords))
	}
	if v {
		b.blocks[block][at] |= 1 << (b.n % 64)
	} else {
		b.blocks[block][at] &^= 1 << (b.n % 64)
	}
	b.n++
}

// get is the i-th bit, false past the last.
func (b *bitList) get(i int) bool {
	if i < 0 || i >= b.n {
		return false
	}
	word := i / 64
	return b.blocks[word/bitBlockWords][word%bitBlockWords]&(1<<(i%64)) != 0
}

// count is how many bits b holds.
func (b *bitList) count() int { return b.n }

// truncate keeps b's first n bits, letting every block past them go.
func (b *bitList) truncate(n int) {
	const per = bitBlockWords * 64
	keep := (n + per - 1) / per
	clear(b.blocks[keep:])
	b.blocks, b.n = b.blocks[:keep], n
}

// clear empties b, keeping its first block and letting every other go.
func (b *bitList) clear() {
	b.n = 0
	if len(b.blocks) > 1 {
		clear(b.blocks[1:])
		b.blocks = b.blocks[:1]
	}
}

// moveTo appends b's bits to dst and empties b, letting each of b's blocks go
// once its bits are moved: the two never hold a list's bits twice over, but
// for the block each is at.
func (b *bitList) moveTo(dst *planBits) {
	const per = bitBlockWords * 64
	for i := range b.n {
		dst.put(b.get(i))
		if (i+1)%per == 0 {
			b.blocks[i/per] = nil
		}
	}
	clear(b.blocks)
	b.blocks, b.n = b.blocks[:0], 0
}

// planBits is a stream of a plan's bits: the tar's first reading writes them
// in the order it decides them, and its second reads them in that same order,
// each once ([tarPlan]). A number is written in Elias's gamma code, so a small
// one, which nearly every one is, takes a few bits.
type planBits struct {
	list bitList
	// read is how many the second reading has taken.
	read int
}

// put writes v.
func (p *planBits) put(v bool) { p.list.add(v) }

// putByte writes c's eight bits.
func (p *planBits) putByte(c byte) {
	for i := 7; i >= 0; i-- {
		p.put(c>>i&1 == 1)
	}
}

// putNumber writes n in the gamma code of n+1: as many 0 bits as n+1 has
// binary digits past its first, then those digits, the first a 1. So n takes
// twice its binary digits and one more, and a reader knows where it ends.
func (p *planBits) putNumber(n uint64) {
	x := n + 1
	digits := bits.Len64(x)
	for range digits - 1 {
		p.put(false)
	}
	for i := digits - 1; i >= 0; i-- {
		p.put(x>>i&1 == 1)
	}
}

// count is how many bits are written.
func (p *planBits) count() int { return p.list.count() }

// take reads the next bit; ok is false past the last.
func (p *planBits) take() (v, ok bool) {
	if p.read >= p.list.count() {
		return false, false
	}
	v = p.list.get(p.read)
	p.read++
	return v, true
}

// takeByte reads eight bits as [planBits.putByte] wrote them.
func (p *planBits) takeByte() (byte, bool) {
	var c byte
	for range 8 {
		v, ok := p.take()
		if !ok {
			return 0, false
		}
		c <<= 1
		if v {
			c |= 1
		}
	}
	return c, true
}

// takeNumber reads a number as [planBits.putNumber] wrote it.
func (p *planBits) takeNumber() (uint64, bool) {
	zeros := 0
	for {
		v, ok := p.take()
		if !ok || zeros == 64 {
			return 0, false
		}
		if v {
			break
		}
		zeros++
	}
	x := uint64(1)
	for range zeros {
		v, ok := p.take()
		if !ok {
			return 0, false
		}
		x <<= 1
		if v {
			x |= 1
		}
	}
	return x - 1, true
}

// done says every bit written is read.
func (p *planBits) done() bool { return p.read == p.list.count() }
