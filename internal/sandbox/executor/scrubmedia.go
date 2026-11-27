package executor

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"io"
	"strings"

	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A KEPT SESSION'S LARGE MEDIA IS CUT OUT AT THE GZIP LEVEL, never inside the
// JSON: the first reading classes each string by its text as it came, before
// any redaction ([mediaRun]), measures a payload's bytes as kept
// ([payloadSpan]), and the second writes each as a gzip member of its own,
// which the take's body frames apart, so every reader sees the bytes the take
// digested (the package doc's "A kept session's media is kept apart";
// docs/concepts/code-sandbox.md's "A session's large payloads, kept apart").

// mediaMinBytes is the shortest payload kept apart: ONE MEBIBYTE of kept text.
// The store paces an upload per mebibyte begun (objstore.PaceFor), so a smaller
// object costs a whole pace unit, a record entry and a collector object for at
// most 0.75 MiB of archive a copy; a whole-PDF read (26.7 MiB a copy) and an
// image at OpenCode's size are far past it.
const mediaMinBytes = 1 << 20

// A UNIT KEPT AS TEXT HOLDS NO PAYLOAD THE PLAN LISTS: one whose JSON fails is
// read again from its mark, which drops what its JSON listed
// ([tarPlan.memberAt]), unless its text way still holds it whole, at most
// [wayRoomBytes], under the shortest payload. A negative constant does not
// compile.
const _ = uint(mediaMinBytes - wayRoomBytes)

// mediaHeadPartMax is the most bytes a data URI's media type, or its subtype,
// may hold where a payload follows: RFC 6838's restricted-name characters,
// fewer than the shortest match of any shape rule they can spell (redact's
// TestTheRulesBase64CanSpellAreKnown holds that), so shaping a payload's head
// never changes it and a box counting payloads reads the same heads.
const mediaHeadPartMax = 19

// base64Marker ends a data URI's head, where its payload begins: no
// parameter is read before it (`;charset=`, `;name=`), so such a string is
// no payload.
const base64Marker = ";base64,"

// mediaClass is what each byte is to a payload's run ([mediaRun.run]): no
// byte of one, a letter or digit, standard base64's own (`+`, `/`), base64url's
// own (`-`, `_`), or the pad. None needs a JSON escape, so a run's kept text
// is its kept bytes.
var mediaClass = func() (t [256]byte) {
	for _, c := range "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789" {
		t[c] = classAlnum
	}
	t['+'], t['/'], t['-'], t['_'], t['='] = classStandard, classStandard, classURL, classURL, classPad
	return t
}()

const (
	classNone byte = iota
	classAlnum
	classStandard
	classURL
	classPad
)

// restrictedName reports whether c may stand in an RFC 6838 restricted name,
// first its first byte.
func restrictedName(c byte, first bool) bool {
	if mediaClass[c] == classAlnum {
		return true
	}
	return !first && strings.IndexByte("!#$&-^_.+", c) >= 0
}

var (
	// errMediaDiverged is a payload whose member at a repeat holds other bytes
	// than at its first occurrence: the take's one compressor broke its own
	// invariant, so it is refused rather than kept splicing the wrong bytes.
	errMediaDiverged = errors.New("executor: a payload kept apart compressed otherwise at a repeat")
	// errMediaCut is a payload's member that ended before its last byte.
	errMediaCut = errors.New("executor: the archive ended inside a payload kept apart")
)

// mediaState is where a [mediaRun] stands in a string's text as it came.
type mediaState uint8

const (
	// mediaHead reads the text's first bytes, to tell a data URI's head.
	mediaHead mediaState = iota
	// mediaType and mediaSubtype read a data URI's media type, and
	// mediaMarker its [base64Marker].
	mediaType
	mediaSubtype
	mediaMarker
	// mediaRunning reads the payload, and mediaNone has let the text go.
	mediaRunning
	mediaNone
)

// mediaRun classes one JSON string value by its text AS IT CAME, before any
// redaction ([payloadScrub]): whether it is a payload, a data URI's base64 or
// base64 from its first byte to its last, and its head. ONLY WELL-FORMED
// BASE64: one alphabet a run (standard's `+/` or base64url's `-_`, never
// both), `=` only as one or two pad bytes at its end, the padded run a
// multiple of four, so a run spells no keyed rule's `KEY=` (redact's
// TestTheRulesBase64CanSpellAreKnown reads runs so). CLASSED BEFORE THE
// REDACTION, so no marker written into a payload turns it into text
// (ADR-0073's amendment "a payload is classed as it came").
type mediaRun struct {
	state         mediaState
	head          [len("data:") + 2*mediaHeadPartMax + len("/") + len(base64Marker)]byte
	nhead, part   int
	match         int
	n             int64
	pad           int
	standard, url bool
}

// begin readies m for a string's text.
func (m *mediaRun) begin() {
	*m = mediaRun{head: m.head}
}

// feed reads s, the next bytes of the text as it came.
func (m *mediaRun) feed(s []byte) {
	for len(s) > 0 {
		if m.state == mediaNone {
			return
		}
		if m.state == mediaRunning {
			m.run(s)
			return
		}
		c := s[0]
		s = s[1:]
		m.head[m.nhead] = c
		m.nhead++
		switch m.state {
		case mediaHead:
			if m.nhead < len("data:") {
				continue
			}
			if string(m.head[:m.nhead]) == "data:" {
				m.state = mediaType
				continue
			}
			var first [len("data:")]byte
			copy(first[:], m.head[:])
			m.state, m.nhead = mediaRunning, 0
			m.run(first[:])
		case mediaType, mediaSubtype:
			switch {
			case m.state == mediaType && c == '/' && m.part > 0:
				m.state, m.part = mediaSubtype, 0
			case m.state == mediaSubtype && c == ';' && m.part > 0:
				m.state, m.match = mediaMarker, 1
			case restrictedName(c, m.part == 0) && m.part < mediaHeadPartMax:
				m.part++
			default:
				m.state = mediaNone
			}
		case mediaMarker:
			if c != base64Marker[m.match] {
				m.state = mediaNone
				return
			}
			if m.match++; m.match == len(base64Marker) {
				m.state = mediaRunning
			}
		}
	}
}

// run reads s, bytes of the payload, letting the text go at the first that
// leaves well-formed base64.
func (m *mediaRun) run(s []byte) {
	for _, c := range s {
		switch mediaClass[c] {
		case classNone:
			m.state = mediaNone
		case classPad:
			if m.pad++; m.pad > 2 {
				m.state = mediaNone
			}
		default:
			switch {
			case m.pad > 0:
				m.state = mediaNone
			case mediaClass[c] == classStandard:
				m.standard = true
			case mediaClass[c] == classURL:
				m.url = true
			}
			if m.standard && m.url {
				m.state = mediaNone
			}
		}
		if m.state == mediaNone {
			return
		}
		m.n++
	}
}

// candidate reports whether the text read so far may still be a payload.
func (m *mediaRun) candidate() bool { return m.state != mediaNone }

// reached reports whether the text read so far is a payload's run of
// [mediaMinBytes] or more, still well-formed, and its head: a data URI's, up
// to its base64, or none. Whether it ends as one is [mediaRun.payload]'s.
func (m *mediaRun) reached() (head []byte, ok bool) {
	if m.state != mediaRunning || m.n < mediaMinBytes {
		return nil, false
	}
	if m.nhead > len("data:") {
		return m.head[:m.nhead], true
	}
	return nil, true
}

// payload reports whether the text, ended, is a payload: [mediaRun.reached]
// and a run of whole base64, padded to a multiple of four or, unpadded, of a
// length an encoding writes (never one past a multiple of four).
func (m *mediaRun) payload() bool {
	if _, ok := m.reached(); !ok {
		return false
	}
	if m.pad > 0 {
		return m.n%4 == 0
	}
	return m.n%4 != 1
}

// payloadSpan measures, in what a string's rewrite writes, the payload its
// text as it came was classed ([mediaRun]): where its bytes as kept begin,
// past the head, their length and their SHA-256. Nothing is measured unless
// the string's scrub says it is one ([payloadSpan.measure]), before a byte of
// it is written, and nothing listed once it says it is not
// ([payloadSpan.reject]). AS KEPT, never as it came: what a value or a rule
// wrote into a payload is in its member, so the member's bytes are the text's.
type payloadSpan struct {
	on   bool
	at   int64
	head int64
	skip int64
	n    int64
	sum  hash.Hash
}

// begin readies p for a string whose text begins at, among what its rewrite
// has written, measuring nothing yet.
func (p *payloadSpan) begin(at int64) { p.on, p.at, p.head, p.skip, p.n = false, at, 0, 0, 0 }

// measure has p measure the string's text, as kept, past its head of head
// bytes: called before the text's first byte is written.
func (p *payloadSpan) measure(head int64) {
	if p.sum == nil {
		p.sum = sha256.New()
	}
	p.sum.Reset()
	p.on, p.head, p.skip, p.n = true, head, head, 0
}

// reject has p list nothing of the string.
func (p *payloadSpan) reject() { p.on = false }

// feed reads s, the next kept bytes of the text, as written.
func (p *payloadSpan) feed(s []byte) {
	if !p.on {
		return
	}
	k := min(int64(len(s)), p.skip)
	p.skip, s = p.skip-k, s[k:]
	_, _ = p.sum.Write(s)
	p.n += int64(len(s))
}

// payload is the string, ended: where its payload begins among what its
// rewrite wrote, its length and its SHA-256, ok where it was measured.
func (p *payloadSpan) payload() (at, n int64, sum [sha256.Size]byte, ok bool) {
	if !p.on || p.n == 0 {
		return 0, 0, sum, false
	}
	p.sum.Sum(sum[:0])
	return p.at + p.head, p.n, sum, true
}

// mediaPlan is what the first reading decided of the payloads it found, in
// the order the spool holds them: each one kept apart, the first of its text
// or a repeat of it; one past the take's count or allowance, or in a unit not
// kept as written, is not listed and stays inline. A few dozen bytes a
// payload of a mebibyte or more, far inside the plan's sixteenth.
type mediaPlan struct {
	// on says the take keeps media apart ([scrubBounds.splitting]),
	// allowance is the payload text its first occurrences may hold in all,
	// pace the store's pace units its keep uploads its objects in, and
	// textMax the most text its seal keeps inline ([mediaPlan.inlinePast]).
	on                       bool
	allowance, pace, textMax int64
	// unitAt is where in the spool the unit being read begins, which a
	// payload's place in what its rewrite wrote counts from.
	unitAt int64
	found  []mediaAt
	texts  []mediaText
	taken  int64
}

// mediaAt is one payload kept apart: where the spool holds it, its length,
// and which of the plan's texts it is.
type mediaAt struct {
	at, n int64
	text  int
}

// mediaText is one distinct payload: its text's SHA-256 and length, the
// payload of the plan's that first holds it, and the SHA-256 of its member
// once the second reading has written it.
type mediaText struct {
	sum    [sha256.Size]byte
	n      int64
	first  int
	member [sha256.Size]byte
}

// add lists the payload of n bytes, sum its SHA-256, that the unit being
// read holds rel bytes in: a repeat of a text listed, else a first where the
// count and the allowance hold it.
func (p *mediaPlan) add(rel, n int64, sum [sha256.Size]byte) {
	at := p.unitAt + rel
	for i, t := range p.texts {
		if t.sum == sum && t.n == n {
			p.found = append(p.found, mediaAt{at: at, n: n, text: i})
			return
		}
	}
	if len(p.texts) == executorkeep.MediaMaxCount || p.taken+n > p.allowance {
		return
	}
	p.texts = append(p.texts, mediaText{sum: sum, n: n, first: len(p.found)})
	p.taken += n
	p.found = append(p.found, mediaAt{at: at, n: n, text: len(p.texts) - 1})
}

// dropTo lets go of every payload past the first mark, and every text one of
// them first held: a unit cut back out of the spool, read again or kept
// otherwise than as written.
func (p *mediaPlan) dropTo(mark int) {
	p.found = p.found[:mark]
	for len(p.texts) > 0 && p.texts[len(p.texts)-1].first >= mark {
		p.taken -= p.texts[len(p.texts)-1].n
		p.texts = p.texts[:len(p.texts)-1]
	}
}

// restarted is p for a first reading begun again from its start.
func (p mediaPlan) restarted() mediaPlan {
	return mediaPlan{on: p.on, allowance: p.allowance, pace: p.pace, textMax: p.textMax}
}

// paced lets go of every first occurrence past what the keep's upload pace
// holds, and its repeats, kept being the archive's kept size: the store paces
// each object by the mebibyte begun (objstore.PaceFor), so k members cost up
// to k+1 paced mebibytes more than the archive kept whole, and the keep's
// budget is the take's pace (sandbox.SessionTake.KeepPaceMiB). kept is
// counted with the margin a save's charge has (an eighth or 64 KiB), and one
// mebibyte more covers the seals' tags. The rest stays inline.
func (p *mediaPlan) paced(kept int64) {
	firsts := max(p.pace-2-paceUnits(kept+max(kept/8, 64<<10)), 0)
	if int64(len(p.texts)) <= firsts {
		return
	}
	found := p.found[:0]
	seen := make([]bool, firsts)
	for _, f := range p.found {
		if int64(f.text) >= firsts {
			continue
		}
		if !seen[f.text] {
			seen[f.text], p.texts[f.text].first = true, len(found)
		}
		found = append(found, f)
	}
	p.found, p.texts = found, p.texts[:firsts]
}

// inlinePast refuses, once the first reading is done and the media paced, a
// take whose text kept inline is past textMax, which its seal would refuse
// only after the second reading: at least the spool less every payload kept
// apart, at its text's length (its gzip's no larger) and less the spool's own
// members. A session of text alone between the two bounds is refused here, its
// scrub's second half unspent; nil where it may fit, the seal deciding.
func (p *mediaPlan) inlinePast(spooled int64) error {
	if !p.on {
		return nil
	}
	inline := spooled - executorkeep.SpoolMembersMostBytes
	for _, f := range p.found {
		inline -= f.n
	}
	if inline > p.textMax {
		return fmt.Errorf("executor: the session keeps at least %d bytes of text inline, past the %d its "+
			"text is kept at: %w", inline, p.textMax, executorkeep.ErrSessionTooLarge)
	}
	return nil
}

// paceUnits is how many mebibytes begun n bytes are, the store's pace unit.
func paceUnits(n int64) int64 { return (n + objstore.MiB - 1) / objstore.MiB }

// finding is p where the take keeps media apart, nil where it does not.
func (p *mediaPlan) finding() *mediaPlan {
	if !p.on {
		return nil
	}
	return p
}

// mediaSplit is where a take that keeps its media apart writes its body, the
// payload text it may keep apart in all, and the pace units its keep uploads
// in ([scrubBounds.splitting]).
type mediaSplit struct {
	body                     io.Writer
	allowance, pace, textMax int64
}

// splitting is b keeping each large payload apart in body, its first
// occurrences at most allowance bytes of text in all and paced to pace units,
// beside the archive whole its scrub writes.
func (b scrubBounds) splitting(body io.Writer, allowance, pace int64) scrubBounds {
	b.split = mediaSplit{body: body, allowance: allowance, pace: pace, textMax: executorkeep.SessionMaxBytes}
	return b
}

// memberKind is what a member's bytes go to the body as.
type memberKind uint8

const (
	// memberWhole is the archive's own bytes, the body the archive whole.
	memberWhole memberKind = iota
	// memberText is the text of a spliced body, and memberFirst a payload's
	// first member: each write a frame; memberRepeat goes to no body.
	memberText
	memberFirst
	memberRepeat
)

// memberOut is where the kept archive's gzip writes: every member to full,
// the archive the take keeps, and to body as its kind frames it, a frame a
// write.
type memberOut struct {
	full, body io.Writer
	kind       memberKind
	// sum is the SHA-256 of the media member open.
	sum hash.Hash
	// frame holds a short write with its head, so a frame is one write.
	frame [4 << 10]byte
}

func (o *memberOut) Write(p []byte) (int, error) {
	if _, err := o.full.Write(p); err != nil {
		return 0, err
	}
	if o.kind == memberFirst || o.kind == memberRepeat {
		_, _ = o.sum.Write(p)
	}
	switch {
	case o.body == nil || o.kind == memberRepeat:
		return len(p), nil
	case o.kind == memberWhole:
		return o.body.Write(p)
	}
	kind := executorkeep.FrameText
	if o.kind == memberFirst {
		kind = sandbox.FrameMember
	}
	o.frame[0] = kind
	head := 1 + binary.PutUvarint(o.frame[1:], uint64(len(p)))
	if head+len(p) <= len(o.frame) {
		n := copy(o.frame[head:], p)
		if _, err := o.body.Write(o.frame[:head+n]); err != nil {
			return 0, err
		}
		return len(p), nil
	}
	if _, err := o.body.Write(o.frame[:head]); err != nil {
		return 0, err
	}
	return o.body.Write(p)
}

// memberSplit is the kept archive's one gzip writer, ending its member at each
// payload the plan keeps apart ([spoolCopy]) and writing that payload as a
// member of its own, then opening the next text member: through Reset, so the
// take holds no second compressor. A member is a pure function of its bytes
// within one build (Go's compressor holds no time, randomness or map order,
// and its output does not hang on how its input was cut), so a repeat's
// member is held to its first's digest.
type memberSplit struct {
	zw   *gzip.Writer
	out  *memberOut
	plan *mediaPlan
	// left is what the media member open still takes of its payload, 0 in a
	// text member; at is which of the plan's payloads it is.
	left int64
	at   int
}

// newMemberSplit is a split writing the archive to full, and to body, where
// the take keeps its media apart, framed: led by sandbox.BodySpliced where
// plan keeps a payload apart, and by sandbox.BodyWhole and the archive
// whole where it keeps none.
func newMemberSplit(full, body io.Writer, plan *mediaPlan) (*memberSplit, error) {
	out := &memberOut{full: full, body: body, sum: sha256.New()}
	if body != nil {
		tag := sandbox.BodyWhole
		if len(plan.found) > 0 {
			tag, out.kind = sandbox.BodySpliced, memberText
		}
		if _, err := body.Write([]byte{tag}); err != nil {
			return nil, err
		}
	}
	zw, err := newKeptGzip(out)
	if err != nil {
		return nil, err
	}
	return &memberSplit{zw: zw, out: out, plan: plan}, nil
}

func (m *memberSplit) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		if m.left == 0 {
			if _, err := m.zw.Write(p); err != nil {
				return 0, err
			}
			return n, nil
		}
		k := min(int64(len(p)), m.left)
		if _, err := m.zw.Write(p[:k]); err != nil {
			return 0, err
		}
		p, m.left = p[k:], m.left-k
		if m.left == 0 {
			if err := m.endMedia(); err != nil {
				return 0, err
			}
		}
	}
	return n, nil
}

// media ends the text member open and opens the member of the plan's payload
// at, which the next of its length bytes written go to.
func (m *memberSplit) media(at int) error {
	if m.left > 0 {
		return errSpoolRead
	}
	if err := m.zw.Close(); err != nil {
		return err
	}
	f := m.plan.found[at]
	m.out.kind = memberRepeat
	if m.plan.texts[f.text].first == at {
		m.out.kind = memberFirst
	}
	m.out.sum.Reset()
	m.zw.Reset(m.out)
	m.left, m.at = f.n, at
	return nil
}

// endMedia ends the media member open, frames its end in the body, a first's
// digest or a repeat's frame naming it, and opens the next text member.
func (m *memberSplit) endMedia() error {
	if err := m.zw.Close(); err != nil {
		return err
	}
	text := &m.plan.texts[m.plan.found[m.at].text]
	var sum [sha256.Size]byte
	m.out.sum.Sum(sum[:0])
	frame := sandbox.FrameMemberEnd
	switch {
	case m.out.kind == memberFirst:
		text.member = sum
	case sum != text.member:
		return errMediaDiverged
	default:
		frame = executorkeep.FrameMedia
	}
	if m.out.body != nil {
		if _, err := m.out.body.Write(append([]byte{frame}, sum[:]...)); err != nil {
			return err
		}
	}
	m.out.kind = memberText
	m.zw.Reset(m.out)
	return nil
}

// Close ends the last member: refused where a payload's member is still open.
func (m *memberSplit) Close() error {
	if m.left > 0 {
		return errMediaCut
	}
	return m.zw.Close()
}

// stringScrub is the scrub of one string value's text, a piece at a time
// ([textScrub], [payloadScrub]).
type stringScrub interface {
	io.Writer
	Close() error
	// took is what the scrub took out, once it is closed, where its scrubber
	// tallies.
	took() int64
}

// errPayloadDiverged is a string kept from the shape rules while it read as a
// payload, then found no payload, whose text the shape rules would have
// changed in what was already written: the first reading is read again with
// every payload's text shaped ([scrubber.shapingPayloads]). A
// [errMemoDiverged], so every reading that hands a diverged text back to be
// read again hands this one back too.
var errPayloadDiverged = fmt.Errorf("%w: a text read as a payload was none, and the shape rules changed it",
	errMemoDiverged)

// payloadScrub is the scrub of a string value the first reading finds media
// in ([scrubber.media]). Its text is classed as it came ([mediaRun]): held, at
// most [mediaMinBytes] and a piece, until it is no payload, scrubbed as every
// text is ([textScrub]), or one, measured as kept ([payloadSpan]) and KEPT FROM
// THE SHAPE RULES, never from the values: each byte is written as the by-value
// pass decides it and handed to the shape pass beside it, whose output is only
// compared ([payloadTee]). A text that turns out no payload goes on through
// that shape pass from where its written bytes end, where the two agree, and
// is read again where they do not ([errPayloadDiverged]). ADR-0073's amendment
// "a payload is classed as it came, and kept from the shape rules" has why.
type payloadScrub struct {
	s     scrubber
	str   *jsonStringWriter
	span  *payloadSpan
	class mediaRun
	held  []byte
	// classing says the text may still turn out no payload.
	classing bool
	// text is the ordinary scrub, once the text is no payload, or every
	// payload's shaped ([scrubber.shapingPayloads]).
	text *textScrub
	// tee, values and out are a payload's own scrub; in is what it was
	// handed, as charged.
	tee    *payloadTee
	values *valueStream
	out    *textOut
	in     int64
}

// payloadHeldBytes is the most a [payloadScrub] holds of a text while it is
// read for a payload: under [mediaMinBytes] of its run, a data URI's head, and
// the piece that decides it, at most [textWholeBytes].
const payloadHeldBytes = mediaMinBytes + len(mediaRun{}.head) + textWholeBytes

// payloadText is s's scrub of a string value str writes, span measuring it
// where its text is a payload, held reusing *held's room ([payloadHeldBytes]).
func (s scrubber) payloadText(str *jsonStringWriter, span *payloadSpan, held *[]byte) *payloadScrub {
	if cap(*held) < payloadHeldBytes {
		*held = make([]byte, 0, payloadHeldBytes)
	}
	p := &payloadScrub{s: s, str: str, span: span, classing: true, held: (*held)[:0]}
	p.class.begin()
	return p
}

func (p *payloadScrub) Write(b []byte) (int, error) {
	n := len(b)
	if p.classing {
		p.class.feed(b)
	}
	switch {
	case p.tee != nil:
		if p.classing && !p.class.candidate() {
			p.classing = false
			p.span.reject()
			p.tee.becomeText()
		}
		return n, p.payloadWrite(b)
	case p.text != nil:
		if p.classing && !p.class.candidate() {
			p.classing = false
			p.span.reject()
		}
		return p.text.Write(b)
	}
	p.held = append(p.held, b...)
	if !p.class.candidate() {
		p.classing = false
		return n, p.toText()
	}
	if head, ok := p.class.reached(); ok {
		// A HEAD THE SHAPE RULES WOULD CHANGE leaves no payload: written
		// unshaped, it would stand in the text unread by them.
		if len(head) > 0 && redact.Secrets(string(head)) != string(head) {
			p.classing = false
			return n, p.toText()
		}
		p.span.measure(int64(len(head)))
		if p.s.shapePayloads {
			return n, p.toText()
		}
		return n, p.toPayload()
	}
	return n, nil
}

// toText hands the text held so far, and every piece after it, to the
// ordinary scrub.
func (p *payloadScrub) toText() error {
	p.text = p.s.text(p.str)
	held := p.held
	p.held = nil
	_, err := p.text.Write(held)
	return err
}

// toPayload begins the payload's own scrub with the text held so far.
func (p *payloadScrub) toPayload() error {
	p.out = &textOut{w: p.str}
	if p.s.tally {
		p.out.own = newOwnCharge()
	}
	p.tee = newPayloadTee(p.out)
	p.values = p.s.stream(p.tee)
	held := p.held
	p.held = nil
	return p.payloadWrite(held)
}

// payloadWrite hands b to the payload's by-value pass.
func (p *payloadScrub) payloadWrite(b []byte) error {
	if p.s.tally {
		p.in += escCharge(b)
	}
	_, err := p.values.Write(b)
	return redactionRefusal(err, p.out.err)
}

// Close writes the rest of the text: a payload kept from the shape rules, or
// a text no payload, shaped, read again where it diverged. A run that ends
// short of whole base64 ([mediaRun.payload]) is no payload.
func (p *payloadScrub) Close() error {
	ended := p.classing && !p.class.payload()
	switch {
	case p.tee != nil:
		if ended {
			p.span.reject()
			p.tee.becomeText()
		}
	case p.text != nil:
		if ended {
			p.span.reject()
		}
		return p.text.Close()
	default:
		if err := p.toText(); err != nil {
			return err
		}
		return p.text.Close()
	}
	if err := redactionRefusal(p.values.Close(), p.out.err); err != nil {
		return err
	}
	return p.tee.close()
}

func (p *payloadScrub) took() int64 {
	if p.text != nil {
		return p.text.took()
	}
	return p.out.own.taken(p.in)
}

// payloadTee is where a payload's by-value pass writes: onto out as decided,
// while it is a payload, and to the shape pass beside it, whose output is
// compared with what was written ([payloadShadow]); once the text is no
// payload, only to the shape pass, whose output past what was written goes
// on to out.
type payloadTee struct {
	out    *textOut
	shape  *redact.Stream
	shadow *payloadShadow
	// text says the text is no payload; written and sum are what went to out
	// while it read as one.
	text    bool
	written int64
	sum     hash.Hash
	// shapeErr is the shape pass's first refusal, which counts only once the
	// text is no payload.
	shapeErr error
}

func newPayloadTee(out *textOut) *payloadTee {
	t := &payloadTee{out: out, sum: sha256.New()}
	t.shadow = &payloadShadow{tee: t, sum: sha256.New()}
	t.shape = redact.NewStream(t.shadow)
	return t
}

func (t *payloadTee) Write(b []byte) (int, error) {
	if !t.text {
		if _, err := t.out.Write(b); err != nil {
			return 0, err
		}
		_, _ = t.sum.Write(b)
		t.written += int64(len(b))
	}
	if t.shapeErr == nil {
		if _, err := t.shape.Write(b); err != nil {
			t.shapeErr = err
		}
	}
	if t.text && t.shapeErr != nil {
		return 0, t.shapeErr
	}
	if err := t.shadow.err; err != nil {
		return 0, err
	}
	return len(b), nil
}

// becomeText has the shape pass write on to out past what was written, which
// it must have written itself.
func (t *payloadTee) becomeText() {
	t.text = true
	t.shadow.settle()
}

// close ends the shape pass: a payload's output is let go, and a text's
// written on, refused where the shape pass refused it or diverged from what
// was written.
func (t *payloadTee) close() error {
	err := t.shape.Close()
	if !t.text {
		return nil
	}
	if t.shapeErr != nil {
		err = t.shapeErr
	}
	if err != nil {
		return redactionRefusal(err, t.out.err)
	}
	if t.shadow.err == nil && t.shadow.n < t.written {
		t.shadow.err = errPayloadDiverged
	}
	return t.shadow.err
}

// payloadShadow is where a payload's shape pass writes: its first bytes, up
// to what was written as the payload, hashed and compared with them; past
// them, once the text is no payload, on to out.
type payloadShadow struct {
	tee *payloadTee
	sum hash.Hash
	n   int64
	// checked says the first bytes were compared; err is a divergence, or
	// out's failure.
	checked bool
	err     error
}

// Write takes every byte, whatever came of the ones before: the shape pass
// reads on to its end, and a divergence or out's failure is the tee's to
// answer ([payloadTee.Write]).
func (s *payloadShadow) Write(b []byte) (int, error) {
	if s.tee.text {
		s.past(b)
		return len(b), nil
	}
	_, _ = s.sum.Write(b)
	s.n += int64(len(b))
	return len(b), nil
}

// past reads b once the text is no payload: its bytes up to what was written
// compared, and the rest written on, until a divergence or out's failure.
func (s *payloadShadow) past(b []byte) {
	if s.err != nil {
		return
	}
	if take := min(int64(len(b)), s.tee.written-s.n); take > 0 {
		_, _ = s.sum.Write(b[:take])
		s.n += take
		b = b[take:]
	}
	s.settle()
	if s.err == nil && s.checked && len(b) > 0 {
		if _, err := s.tee.out.Write(b); err != nil {
			s.err = err
		}
	}
}

// settle compares, once the shape pass has written as much as was written as
// the payload, the two: equal, its output goes on; otherwise, or where it
// wrote more before the text was no payload, the text diverged.
func (s *payloadShadow) settle() {
	if s.checked || s.err != nil || !s.tee.text || s.n < s.tee.written {
		return
	}
	s.checked = true
	if s.n > s.tee.written || !bytes.Equal(s.sum.Sum(nil), s.tee.sum.Sum(nil)) {
		s.err = errPayloadDiverged
	}
}
