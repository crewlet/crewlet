package executor

import (
	"bytes"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"slices"
	"unicode/utf8"
)

// WHAT CLAUDE CODE ESTIMATES A LINE'S CONTENT AT.
//
// Before a request, Claude Code decides whether to compact by its own count of
// the conversation: the last reply whose usage counts, and an ESTIMATE of every
// record after it (2.1.322's Xh and k7o @294311543, 2.1.310's Zg and KFo
// @247646809). These are the facts a reader of that count reads of a line
// ([threadLine.Estimate], [threadLine.AttachmentEstimate]), filled once, as the
// line is read ([factsOf]), by the harness's own rules:
//
//   - A USER OR ASSISTANT RECORD is its message's content (EX, 2.1.322
//     @292455010; zY in 2.1.310): a string at its length, a list element by
//     element (n7e @292455428; GJn): a string or a `text` block at its text's
//     length; `thinking` at its text, `redacted_thinking` at its data (the
//     signature is not charged); a `tool_use` at its name's length plus its
//     input's JSON, which is the input as the line writes it, since the
//     harness writes a line with JSON.stringify; a `tool_result` at its
//     content, by these rules again; a `document` whose source is text at its
//     data, at TWO characters a token where its title's last dotted part is
//     `json`, `jsonl` or `jsonc` in any case, and whose source is content at
//     that content (a string, or its list's objects alone); an `image`, or a
//     `document` of any other source, 2,000 flat (`m=2000`); and any other
//     element at its JSON as written. On 2.1.322 the record also charges
//     2,000 for each image its MCP task delivery carries (Ebs), which 2.1.310
//     does not, so a figure read on that release holds 2,000 an image more
//     than its own count did: the floor's safe side.
//   - AN ATTACHMENT RECORD is what its record stores as its rendering, the
//     record's own `rendered` (rTe and qLe @298643213, its shape held by
//     Cje's schema @298645370): every rendered message's content, a non-empty
//     string or a non-empty list of `text` blocks of non-empty text; plus, on
//     a queued command, 2,000 for each image its task delivery carries. A PDF
//     read is 3,000 a page whatever it stores (vbs, `kbs=3000` @292454941).
//     Where no rendering is stored, or the one stored does not hold to that
//     shape, the harness renders the attachment again by its type (VCt
//     @298645939), a template this layer does not hold, so the estimate does
//     not read ([threadLine.AttachmentEstimate]'s false): so too always for
//     a type the harness renders again whatever it stores (`poll_events`,
//     `project_doc_reads`; L_r @298641216). Only an attachment of no type,
//     and one of the five types the harness renders as no message (`Nh`
//     @283080639), reads as nothing where it stores no rendering, as none of
//     the five does.
//
// LENGTH IS JAVASCRIPT'S: a string's length in UTF-16 code units, two for a
// character past the basic plane, never capped ([quotedUnits]; the skill
// listing's [textUnits] stops past a name's bound). And EACH ELEMENT IS
// ROUNDED AS THE HARNESS ROUNDS IT, Math.round(length / n) (y @292454158),
// half up, before the elements are summed, so a list of short texts charges
// less than their summed length would ([estimateUnits]).
//
// AT BOTH RATES ([threadEstimate]): the harness reads four characters a token
// for exactly fourteen older models and three for every other (2.1.322's F_
// @286842850, 2.1.310's ry @240809635), and which model counts is the reader's
// to say, so the facts hold the two sums.
//
// ONE PASS OVER A VALUE, HOLDING NO TEXT: a content value is walked once, as
// its decoder reads it ([estimateWalk]), each element's charge read as it
// passes and the length of what an element writes read off the value where it
// lies, so a value nested a thousand levels deep costs its bytes once, never
// once a level, and nothing of a text is kept but its length.

// threadEstimate is Claude Code's estimate of a value's tokens at each of the
// two rates it counts text at: At3 at three characters a token, the rate of
// every current model, and At4 at four, the rate of the fourteen older models
// it names. Each is a sum of per-element charges each rounded as the harness
// rounds it, so the two are two sums, never one scaled to the other.
type threadEstimate struct{ At3, At4 int64 }

// threadEstimateMax is the most either rate of an estimate holds: 2^53 - 1
// ([wireCountMax]), past every model's window by nine orders of magnitude, and
// where a sum of estimates stops ([threadEstimate.plus]), so a PDF read whose
// stated pages charge past an int64 reads as at least every window rather than
// as a wrapped, negative, figure.
const threadEstimateMax = wireCountMax

// plus is e and o summed, each rate at most [threadEstimateMax].
func (e threadEstimate) plus(o threadEstimate) threadEstimate {
	return threadEstimate{At3: min(e.At3+o.At3, threadEstimateMax), At4: min(e.At4+o.At4, threadEstimateMax)}
}

// estimateMediaTokens is what the harness charges an image or a document it
// does not read as text, flat (`m=2000`, 2.1.322 @292454717), and an image an
// MCP task delivers; estimatePDFPageTokens what it charges a page of a PDF
// read (`kbs=3000` @292454941).
const (
	estimateMediaTokens   = 2_000
	estimatePDFPageTokens = 3_000
)

// estimateFlat is a charge of tokens whatever the rate.
func estimateFlat(tokens int64) threadEstimate {
	tokens = min(tokens, threadEstimateMax)
	return threadEstimate{At3: tokens, At4: tokens}
}

// estimateUnits is a text of units UTF-16 code units at both rates.
func estimateUnits(units int64) threadEstimate {
	return threadEstimate{At3: roundedPer(units, 3), At4: roundedPer(units, 4)}
}

// estimateHalf is a text of units UTF-16 code units at two characters a token,
// a JSON-titled text document's rate at either model's (Math.min(n, 2)).
func estimateHalf(units int64) threadEstimate {
	return estimateFlat(roundedPer(units, 2))
}

// roundedPer is Math.round(units / n), which rounds half up, in integers: a
// length is never negative, and no length of a line comes near overflowing
// 2*units.
func roundedPer(units, n int64) int64 { return (2*units + n) / (2 * n) }

// quotedUnits is how many UTF-16 code units JavaScript counts raw, one JSON
// string as written with its quotes, as long once it is decoded: an escape one
// unit (a character past the basic plane is written as two `\u` escapes, its
// two units), a character past the basic plane written as itself two, every
// other character one, and each byte that is no UTF-8 one, as the line's
// decode reads each as U+FFFD; JavaScript's decoder reads a run of such bytes
// as fewer characters, so the count is never under the harness's. It is
// [textUnits] over the decoded text with no cap, read where the string lies,
// so a text of any length costs no copy.
func quotedUnits(raw []byte) int64 {
	if len(raw) < 2 {
		return 0
	}
	var n int64
	for p := raw[1 : len(raw)-1]; len(p) > 0; {
		switch {
		case p[0] == '\\':
			// The line decoded whole, so every escape is whole: `\uXXXX`
			// six bytes, every other two.
			if len(p) > 1 && p[1] == 'u' {
				p = p[min(6, len(p)):]
			} else {
				p = p[min(2, len(p)):]
			}
			n++
		case p[0] < utf8.RuneSelf:
			p = p[1:]
			n++
		default:
			r, size := utf8.DecodeRune(p)
			p = p[size:]
			n++
			if r > 0xffff {
				n++
			}
		}
	}
	return n
}

// writtenUnits is how many UTF-16 code units the text p, JSON as written, is
// long: what the harness counts a value at where it charges the value's
// JSON.stringify, which writes the value as the line holds it.
func writtenUnits(p []byte) int64 {
	var n int64
	for len(p) > 0 {
		if p[0] < utf8.RuneSelf {
			p = p[1:]
			n++
			continue
		}
		r, size := utf8.DecodeRune(p)
		p = p[size:]
		n++
		if r > 0xffff {
			n++
		}
	}
	return n
}

// The types of a content element the harness's estimate reads by name, beside
// the layer's own blocks ([blockText], [blockThinking]...).
const (
	estimateImage    = "image"
	estimateDocument = "document"
	// A document's source types.
	estimateSourceText    = "text"
	estimateSourceContent = "content"
)

// estimateElementTypes is every type of a content element the estimate charges
// by a rule of its own; any other is charged at its JSON ([estimateObject]).
var estimateElementTypes = [...]string{blockText, estimateDocument, estimateImage, blockToolResult, blockToolUse,
	blockThinking, blockRedactedThinking}

// quotedIs reports whether raw, a JSON value as written, is the string want,
// where want is ASCII: compared as written where it holds no escape, else
// decoded, where it is short enough to be want at all (six bytes a character
// written as an escape such as `\u0074`, and the quotes), so a long string
// costs no copy.
func quotedIs(raw []byte, want string) bool {
	if len(raw) < 2 || raw[0] != '"' {
		return false
	}
	inner := raw[1 : len(raw)-1]
	if bytes.IndexByte(inner, '\\') < 0 {
		return string(inner) == want
	}
	if len(raw) > 2+6*len(want) {
		return false
	}
	var buf [2 + 6*len(blockRedactedThinking)]byte
	got, err := jsontext.AppendUnquote(buf[:0], raw)
	return err == nil && string(got) == want
}

// estimateTypeOf is raw, a type as written, as the one of types it is, "" where
// it is none of them, a string or not.
func estimateTypeOf(raw []byte, types []string) string {
	for _, typ := range types {
		if quotedIs(raw, typ) {
			return typ
		}
	}
	return ""
}

// estimateCharge is a value's estimate, and fault where the harness's own
// estimate of it throws rather than answering: no figure, which a reader
// takes as the estimate not reading. Held beside the estimate rather than
// returned at once, since a value whose fault is in an element its parent's
// rule does not read (a malformed call inside an unknown block, charged at its
// JSON) is charged with no fault.
type estimateCharge struct {
	est   threadEstimate
	fault error
}

// plus is c and o summed, the first fault kept.
func (c estimateCharge) plus(o estimateCharge) estimateCharge {
	if c.fault == nil {
		c.fault = o.fault
	}
	c.est = c.est.plus(o.est)
	return c
}

// estimateWalk walks one JSON value once, as its decoder reads it, charging
// each element as it passes ([estimateWalk.element]). The length of what an
// element writes is read off raw, the value walked, from where the last
// length read ended (at) to where the element ends, so every byte of the value
// is counted once however deeply its elements nest: units is the length of
// raw[:at].
type estimateWalk struct {
	dec   *jsontext.Decoder
	raw   []byte
	at    int
	units int64
}

// estimateContent is a message's content, raw, as the harness estimates it
// (EX), in one walk: a string at its length, a list element by element,
// anything else nothing, and nothing where raw is absent, the harness's
// `!e`.
func estimateContent(raw json.RawMessage) (threadEstimate, error) {
	if len(raw) == 0 {
		return threadEstimate{}, nil
	}
	w := &estimateWalk{dec: jsontext.NewDecoder(bytes.NewBuffer(raw), threadDecoding), raw: raw}
	l, err := w.list()
	if err != nil {
		return threadEstimate{}, err
	}
	return l.all.est, l.all.fault
}

// unitsTo is the length of raw[:off], read on from where the last read ended:
// off is never before it, as the decoder reads the value in order.
func (w *estimateWalk) unitsTo(off int) int64 {
	w.units += writtenUnits(w.raw[w.at:off])
	w.at = off
	return w.units
}

// offset is where the decoder has read to.
func (w *estimateWalk) offset() int { return int(w.dec.InputOffset()) }

// estimateList is a value read as a list of elements: its kind, every
// element's charge summed (all), and the charges of its elements that are
// objects alone (objects), the two ways the harness reads a list. A string
// is one element, all its own; any other value charges nothing.
type estimateList struct {
	kind         valueKind
	all, objects estimateCharge
}

// list reads the next value as a list of elements ([estimateList]).
func (w *estimateWalk) list() (estimateList, error) {
	l := estimateList{kind: valueKind(w.dec.PeekKind())}
	switch l.kind {
	case '"':
		v, err := w.dec.ReadValue()
		l.all.est = estimateUnits(quotedUnits(v))
		return l, err
	case '[':
	default:
		return l, w.dec.SkipValue()
	}
	if _, err := w.dec.ReadToken(); err != nil {
		return l, err
	}
	for w.dec.PeekKind() != ']' {
		object := w.dec.PeekKind() == '{'
		c, err := w.element()
		if err != nil {
			return l, err
		}
		l.all = l.all.plus(c)
		if object {
			l.objects = l.objects.plus(c)
		}
	}
	_, err := w.dec.ReadToken()
	return l, err
}

// element reads the next value as one element of a content list (n7e): a
// string at its length, an object by its type ([estimateObject]), anything
// else nothing.
func (w *estimateWalk) element() (estimateCharge, error) {
	switch w.dec.PeekKind() {
	case '"':
		v, err := w.dec.ReadValue()
		return estimateCharge{est: estimateUnits(quotedUnits(v))}, err
	case '{':
		return w.object()
	}
	return estimateCharge{}, w.dec.SkipValue()
}

// estimateText is a key's value where the rule reads it as a text: its kind,
// and its length where it is a string.
type estimateText struct {
	kind  valueKind
	units int64
}

// text reads the next value as a text.
func (w *estimateWalk) text() (estimateText, error) {
	v, err := w.dec.ReadValue()
	if err != nil || kindOfValue(v) != '"' {
		return estimateText{kind: kindOfValue(v)}, err
	}
	return estimateText{kind: '"', units: quotedUnits(v)}, nil
}

// estimateObject is what an element's rules read of an object, each key by
// its exact name and a key written twice by its last value, as JSON.parse
// reads it: its type, where it is one a rule names; each text a rule reads;
// its input's length as written; its source, title and content.
type estimateObject struct {
	typ                        string
	text, thinking, data, name estimateText
	// input is the length of `input` as written; inputSet whether it is
	// there and not null, the harness's `e.input ?? {}`.
	input    int64
	inputSet bool
	// sourceKind is `source`'s kind, and source what it holds where that is
	// an object.
	sourceKind valueKind
	source     estimateSource
	// titleJSON is whether `title` names a JSON file, and titleKind its kind.
	titleJSON bool
	titleKind valueKind
	content   estimateList
}

// estimateSource is what a document's source holds that its rule reads.
type estimateSource struct {
	typ     string
	data    estimateText
	content estimateList
}

// object reads the next value, an object, as one element, and charges it by
// its type ([estimateObject.charge]), handed the length it writes.
func (w *estimateWalk) object() (estimateCharge, error) {
	if _, err := w.dec.ReadToken(); err != nil {
		return estimateCharge{}, err
	}
	start := w.unitsTo(w.offset() - 1)
	var o estimateObject
	for w.dec.PeekKind() != '}' {
		name, err := w.dec.ReadValue()
		if err != nil {
			return estimateCharge{}, err
		}
		if err = w.key(&o, name); err != nil {
			return estimateCharge{}, err
		}
	}
	if _, err := w.dec.ReadToken(); err != nil {
		return estimateCharge{}, err
	}
	return o.charge(w.unitsTo(w.offset()) - start), nil
}

// key reads the value of the key name, as written, into o, where a rule reads
// it; every other key's value is passed over. name is compared before the
// value is read, since the decoder hands it out only until its next read.
func (w *estimateWalk) key(o *estimateObject, name []byte) error {
	var (
		v   jsontext.Value
		err error
	)
	switch {
	case quotedIs(name, "type"):
		if v, err = w.dec.ReadValue(); err == nil {
			o.typ = estimateTypeOf(v, estimateElementTypes[:])
		}
	case quotedIs(name, "text"):
		o.text, err = w.text()
	case quotedIs(name, "thinking"):
		o.thinking, err = w.text()
	case quotedIs(name, "data"):
		o.data, err = w.text()
	case quotedIs(name, "name"):
		o.name, err = w.text()
	case quotedIs(name, "title"):
		if v, err = w.dec.ReadValue(); err == nil {
			o.titleKind, o.titleJSON = kindOfValue(v), jsonTitled(v)
		}
	case quotedIs(name, "input"):
		if v, err = w.dec.ReadValue(); err == nil {
			end := w.offset()
			from := w.unitsTo(end - len(v))
			o.input, o.inputSet = w.unitsTo(end)-from, kindOfValue(v) != 'n'
		}
	case quotedIs(name, "source"):
		o.sourceKind, o.source, err = w.source()
	case quotedIs(name, "content"):
		o.content, err = w.list()
	default:
		err = w.dec.SkipValue()
	}
	return err
}

// source reads the next value as a document's source: its kind, and where it
// is an object its type, data and content.
func (w *estimateWalk) source() (valueKind, estimateSource, error) {
	kind := valueKind(w.dec.PeekKind())
	if kind != '{' {
		return kind, estimateSource{}, w.dec.SkipValue()
	}
	var s estimateSource
	if _, err := w.dec.ReadToken(); err != nil {
		return kind, s, err
	}
	for w.dec.PeekKind() != '}' {
		name, err := w.dec.ReadValue()
		if err != nil {
			return kind, s, err
		}
		switch {
		case quotedIs(name, "type"):
			var v jsontext.Value
			if v, err = w.dec.ReadValue(); err == nil {
				s.typ = estimateTypeOf(v, []string{estimateSourceText, estimateSourceContent})
			}
		case quotedIs(name, "data"):
			s.data, err = w.text()
		case quotedIs(name, "content"):
			s.content, err = w.list()
		default:
			err = w.dec.SkipValue()
		}
		if err != nil {
			return kind, s, err
		}
	}
	_, err := w.dec.ReadToken()
	return kind, s, err
}

// charge is the object's charge by its type, written the length of its JSON
// as written (n7e, in its order).
func (o *estimateObject) charge(written int64) estimateCharge {
	switch o.typ {
	case blockText:
		return estimateCharge{est: estimateUnits(o.text.units)}
	case estimateDocument:
		if c, ok := o.sourceCharge(); ok {
			return c
		}
		return estimateCharge{est: estimateFlat(estimateMediaTokens)}
	case estimateImage:
		return estimateCharge{est: estimateFlat(estimateMediaTokens)}
	case blockToolResult:
		// Its content by the rules again (EX): a list, a string, or nothing.
		return o.content.all
	case blockToolUse:
		// `e.name + JSON.stringify(e.input ?? {})`: a name that is no string
		// the harness's own concatenation reads as JavaScript prints that
		// value, which no rule here does.
		if o.name.kind != '"' {
			return estimateCharge{fault: fmt.Errorf("a tool_use's name is %s, not a string", o.name.kind)}
		}
		input := o.input
		if !o.inputSet {
			input = int64(len(`{}`))
		}
		return estimateCharge{est: estimateUnits(o.name.units + input)}
	case blockThinking:
		return estimateCharge{est: estimateUnits(o.thinking.units)}
	case blockRedactedThinking:
		return estimateCharge{est: estimateUnits(o.data.units)}
	}
	return estimateCharge{est: estimateUnits(written)}
}

// sourceCharge is a document's charge by its source, false where the source
// is none the rule reads as text, which charges the document flat: an object
// whose type is text (its data, at two characters a token where its title
// names a JSON file), or content (a string, or the objects of a list).
func (o *estimateObject) sourceCharge() (estimateCharge, bool) {
	if o.sourceKind != '{' {
		return estimateCharge{}, false
	}
	switch o.source.typ {
	case estimateSourceText:
		// `e.title?.split(".")`: a title that is there, not null and no
		// string has no split, and the harness's estimate throws.
		if o.titleKind != 0 && o.titleKind != 'n' && o.titleKind != '"' {
			return estimateCharge{fault: fmt.Errorf("a document's title is %s, not a string", o.titleKind)}, true
		}
		if o.titleJSON {
			return estimateCharge{est: estimateHalf(o.source.data.units)}, true
		}
		return estimateCharge{est: estimateUnits(o.source.data.units)}, true
	case estimateSourceContent:
		switch o.source.content.kind {
		case '"':
			return o.source.content.all, true
		case '[':
			return o.source.content.objects, true
		}
	}
	return estimateCharge{}, false
}

// jsonTitled reports whether raw, a document's title as written, is a string
// whose last part after a dot, lower-cased, is `json`, `jsonl` or `jsonc`
// (`e.title?.split(".").pop()?.toLowerCase()`): the harness counts such a
// document's text at two characters a token. A title of no dot is its own
// last part. Read where the title lies, character by character, holding at
// most the five of its last part a match needs: no character past ASCII
// lower-cases to one of those letters.
func jsonTitled(raw []byte) bool {
	if len(raw) < 2 || raw[0] != '"' {
		return false
	}
	var (
		part [len("jsonl")]byte
		n    int
		long bool
	)
	for p := raw[1 : len(raw)-1]; len(p) > 0; {
		r, size := rune(p[0]), 1
		switch {
		case p[0] == '\\':
			r, size = quotedEscape(p)
		case p[0] >= utf8.RuneSelf:
			r, size = utf8.DecodeRune(p)
		}
		p = p[size:]
		switch {
		case r == '.':
			n, long = 0, false
		case long:
		case n == len(part) || r >= utf8.RuneSelf:
			long = true
		default:
			if r >= 'A' && r <= 'Z' {
				r += 'a' - 'A'
			}
			part[n] = byte(r)
			n++
		}
	}
	if long {
		return false
	}
	switch string(part[:n]) {
	case "json", "jsonl", "jsonc":
		return true
	}
	return false
}

// quotedEscape is the character the escape p opens stands for, and how many
// bytes it takes: a `\u` escape its code unit (a surrogate standing alone,
// which is no ASCII character either way), every other its character.
func quotedEscape(p []byte) (rune, int) {
	if len(p) < 2 {
		return utf8.RuneError, len(p)
	}
	if p[1] != 'u' || len(p) < 6 {
		switch p[1] {
		case 'b':
			return '\b', 2
		case 'f':
			return '\f', 2
		case 'n':
			return '\n', 2
		case 'r':
			return '\r', 2
		case 't':
			return '\t', 2
		}
		return rune(p[1]), 2
	}
	var r rune
	for _, h := range p[2:6] {
		switch {
		case h >= '0' && h <= '9':
			r = r<<4 | rune(h-'0')
		case h >= 'a' && h <= 'f':
			r = r<<4 | rune(h-'a'+10)
		case h >= 'A' && h <= 'F':
			r = r<<4 | rune(h-'A'+10)
		}
	}
	return r, 6
}

// messageEstimate is a user or assistant record's charge: its message's
// content, raw, and, where that is no empty string (the harness's
// `e.message?.content` charges a record only where it is truthy), the images
// its task delivery, raw, carries.
func messageEstimate(content, delivery json.RawMessage) (threadEstimate, error) {
	e, err := estimateContent(content)
	if err != nil || string(content) == `""` {
		return e, err
	}
	return e.plus(estimateFlat(estimateMediaTokens * taskImages(delivery))), nil
}

// taskImages is how many images a task delivery, raw, carries (S
// @292454728): the elements of its MCP task snapshot's result's content,
// where it carries a snapshot, else of its own result's, that are objects of
// type image; none where any of those is no object, or the content no list.
func taskImages(delivery json.RawMessage) int64 {
	var d struct {
		Snapshot json.RawMessage `json:"mcpTaskSnapshot"`
		Result   json.RawMessage `json:"result"`
	}
	if !wireObject(delivery) || wireDecode(delivery, &d) != nil {
		return 0
	}
	result := d.Result
	if wireObject(d.Snapshot) {
		var snapshot struct {
			Result json.RawMessage `json:"result"`
		}
		if wireDecode(d.Snapshot, &snapshot) != nil {
			return 0
		}
		result = snapshot.Result
	}
	var r struct {
		Content json.RawMessage `json:"content"`
	}
	if !wireObject(result) || wireDecode(result, &r) != nil || kindOfValue(r.Content) != '[' {
		return 0
	}
	var images int64
	_ = eachElement(r.Content, func(element json.RawMessage, _ int) error {
		var e struct {
			Type json.RawMessage `json:"type"`
		}
		if wireObject(element) && wireDecode(element, &e) == nil && quotedIs(e.Type, estimateImage) {
			images++
		}
		return nil
	})
	return images
}

// The attachments the harness's estimate reads by type, not by what they
// store: a PDF read ([pdfReadsEstimate]), and the two it renders again
// whatever they store (L_r @298641216); and a queued command, which also
// charges the images its task delivery carries.
const (
	attachmentPDFReads      = "message_pdf_reads"
	attachmentPollEvents    = "poll_events"
	attachmentProjectDocs   = "project_doc_reads"
	attachmentQueuedCommand = "queued_command"
)

// attachmentsOfNoMessage is the five attachment types the harness renders as
// no message (Nh @283080639), none of which stores a rendering: each reads as
// nothing where it stores none.
var attachmentsOfNoMessage = [...]string{"prompt_snapshot", "prompt_render_point", "deferred_tools_record",
	"thinking_drop", "credential_org"}

// attachmentEstimate is an attachment's charge, its keys raw: its `type`, a
// PDF read's `reads`, a queued command's `taskDelivery`, and the record's own
// `rendered`; false where the estimate does not read ([lineFacts]'s rules,
// claudelineestimate.go). A type that is no string reads as not reading: its
// accessor answers the type's error first.
func attachmentEstimate(typ, reads, delivery, rendered json.RawMessage) (threadEstimate, bool) {
	kind, err := wireString("attachment.type", typ)
	if err != nil {
		return threadEstimate{}, false
	}
	switch kind {
	case attachmentPDFReads:
		return pdfReadsEstimate(reads)
	case attachmentPollEvents, attachmentProjectDocs:
		return threadEstimate{}, false
	}
	if e, ok := renderedEstimate(rendered); ok {
		if kind == attachmentQueuedCommand {
			e = e.plus(estimateFlat(estimateMediaTokens * taskImages(delivery)))
		}
		return e, true
	}
	if typ == nil || slices.Contains(attachmentsOfNoMessage[:], kind) {
		return threadEstimate{}, true
	}
	return threadEstimate{}, false
}

// errNotRendered is a stored rendering that does not hold to the shape the
// harness reads one by ([renderedEstimate]).
var errNotRendered = errors.New("no rendering the harness reads")

// renderedEstimate is an attachment record's stored rendering, raw, charged as
// the harness charges it: a list of at least one message, each an object
// whose `content` is a non-empty string or a non-empty list of objects of
// type `text` whose text is a non-empty string, every content at its length,
// each text block on its own; false for any other value, absent among them
// (Cje's schema @298645370, whose objects keep any other key).
func renderedEstimate(rendered json.RawMessage) (threadEstimate, bool) {
	if kindOfValue(rendered) != '[' {
		return threadEstimate{}, false
	}
	var (
		sum      threadEstimate
		messages int
	)
	err := eachElement(rendered, func(element json.RawMessage, _ int) error {
		messages++
		var m struct {
			Content json.RawMessage `json:"content"`
		}
		if !wireObject(element) || wireDecode(element, &m) != nil {
			return errNotRendered
		}
		e, ok := renderedContent(m.Content)
		if !ok {
			return errNotRendered
		}
		sum = sum.plus(e)
		return nil
	})
	return sum, err == nil && messages > 0
}

// renderedContent is one rendered message's content, raw, charged
// ([renderedEstimate]); false where it does not hold to the shape.
func renderedContent(content json.RawMessage) (threadEstimate, bool) {
	switch kindOfValue(content) {
	case '"':
		return estimateUnits(quotedUnits(content)), string(content) != `""`
	case '[':
	default:
		return threadEstimate{}, false
	}
	var (
		sum    threadEstimate
		blocks int
	)
	err := eachElement(content, func(element json.RawMessage, _ int) error {
		blocks++
		var b struct {
			Type json.RawMessage `json:"type"`
			Text json.RawMessage `json:"text"`
		}
		if !wireObject(element) || wireDecode(element, &b) != nil || !quotedIs(b.Type, blockText) ||
			kindOfValue(b.Text) != '"' || string(b.Text) == `""` {
			return errNotRendered
		}
		sum = sum.plus(estimateUnits(quotedUnits(b.Text)))
		return nil
	})
	return sum, err == nil && blocks > 0
}

// pdfReadsEstimate is a PDF read's charge, its `reads` raw: 3,000 a page, its
// pages the sum of each read's `pages`, a count of at least 1 (vbs
// @292454941); false where `reads` is no list, or a read no object stating
// its pages. The harness charges nothing where a read fails the rest of its
// schema (a path, a result), which this charges by its pages: the floor's safe
// side.
func pdfReadsEstimate(reads json.RawMessage) (threadEstimate, bool) {
	if kindOfValue(reads) != '[' {
		return threadEstimate{}, false
	}
	var pages int64
	err := eachElement(reads, func(element json.RawMessage, _ int) error {
		var r struct {
			Pages json.RawMessage `json:"pages"`
		}
		if !wireObject(element) || wireDecode(element, &r) != nil || r.Pages == nil {
			return errNotRendered
		}
		n, err := wireCount("pages", r.Pages)
		if err != nil || n < 1 {
			return errNotRendered
		}
		pages = min(pages+n, threadEstimateMax)
		return nil
	})
	if err != nil {
		return threadEstimate{}, false
	}
	return estimateFlat(min(pages, threadEstimateMax/estimatePDFPageTokens) * estimatePDFPageTokens), true
}

// Estimate is Claude Code's estimate of a user or assistant record after the
// last reply its count reads ([lineFacts]'s rules, claudelineestimate.go): its
// message's content, and on a record whose content is no empty string the
// images its task delivery carries. Nothing, and no error, on every record of
// another type.
//
// ITS READER is the reading of the size of a session's next request, which
// sums it over every record after that reply, at the rate the model the next
// request goes to is counted at.
//
// ITS ERRORS ARE THE MESSAGE'S ([threadLine.Message]), and where the
// harness's own estimate throws on the content (a document's title that is
// there and no string, a call's name that is no string), that.
//
// Read once, as the line is decoded ([factsOf]): never a second read of the
// line.
func (l threadLine) Estimate() (threadEstimate, error) {
	if _, err := l.Message(); err != nil {
		return threadEstimate{}, err
	}
	m := &l.facts().message
	switch {
	case l.Head.Type != recordUser && l.Head.Type != recordAssistant:
		return threadEstimate{}, nil
	case m.estimateErr != nil:
		return threadEstimate{}, fmt.Errorf("its message's content: %w", m.estimateErr)
	}
	return m.estimate, nil
}

// AttachmentEstimate is Claude Code's estimate of an attachment record after
// the last reply its count reads ([lineFacts]'s rules,
// claudelineestimate.go): what its stored rendering holds, or a PDF read's
// pages; and whether that reads, false where the harness renders the
// attachment again by its type, a template this layer does not hold, so its
// reader bounds it otherwise. Nothing, read, on an attachment record with no
// attachment and on every record of another type, which the harness charges
// nothing as an attachment.
//
// ITS ERRORS ARE THE TYPE'S ([threadLine.AttachmentType]).
//
// Read once, as the line is decoded ([factsOf]): never a second read of the
// line.
func (l threadLine) AttachmentEstimate() (threadEstimate, bool, error) {
	if _, err := l.AttachmentType(); err != nil {
		return threadEstimate{}, false, err
	}
	a := &l.facts().attachment
	if l.Head.Type != recordAttachment || a.kind == 0 {
		return threadEstimate{}, true, nil
	}
	return a.estimate, a.estimated, nil
}
