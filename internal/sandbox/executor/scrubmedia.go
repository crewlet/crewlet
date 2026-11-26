package executor

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"hash"
	"io"

	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A KEPT SESSION'S LARGE MEDIA IS CUT OUT AT THE GZIP LEVEL, never inside the
// JSON: the first reading finds each large base64 payload as a string's kept
// text, after the scrub, and the second writes each as a gzip member of its
// own, which the take's body frames apart, so every reader sees the bytes the
// take digested (the package doc's "A kept session's media is kept apart";
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

// mediaTypeMax bounds a data URI's media type a payload follows: RFC 6838's
// type and subtype, 127 bytes each, and their slash.
const mediaTypeMax = 255

// base64Marker ends a data URI's head, where its payload begins.
const base64Marker = ";base64,"

// mediaAlphabet is every byte a payload's run holds: base64's and base64url's
// alphabets and the pad. None needs a JSON escape, so a run's kept text is its
// kept bytes.
var mediaAlphabet = func() (t [256]bool) {
	for _, c := range "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/-_=" {
		t[c] = true
	}
	return t
}()

// mediaTypeByte reports whether c may stand in a data URI's media type.
func mediaTypeByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
		c == '.' || c == '+' || c == '/' || c == '-'
}

var (
	// errMediaDiverged is a payload whose member at a repeat holds other bytes
	// than at its first occurrence: the take's one compressor broke its own
	// invariant, so it is refused rather than kept splicing the wrong bytes.
	errMediaDiverged = errors.New("executor: a payload kept apart compressed otherwise at a repeat")
	// errMediaCut is a payload's member that ended before its last byte.
	errMediaCut = errors.New("executor: the archive ended inside a payload kept apart")
)

// mediaState is where a [mediaRun] stands in a string's kept text.
type mediaState uint8

const (
	// mediaHead reads the text's first bytes, to tell a data URI's head.
	mediaHead mediaState = iota
	// mediaType reads a data URI's media type, and mediaMarker its
	// [base64Marker].
	mediaType
	mediaMarker
	// mediaRunning reads the payload, and mediaNone has let the text go.
	mediaRunning
	mediaNone
)

// mediaRun reads one JSON string value's kept text as the rewrite writes it
// ([jsonStringWriter]), holding nothing of it: whether it is a payload (a data
// URI's base64, or base64 from its first byte to its last), where the payload
// begins among what the rewrite wrote, its length and its SHA-256. An ordinary
// string is let go at its first byte outside the alphabet.
type mediaRun struct {
	state  mediaState
	head   [len("data:")]byte
	nhead  int
	at     int64
	prefix int64
	typed  int
	match  int
	n      int64
	sum    hash.Hash
}

// begin readies m for a string whose text begins at, among what its rewrite
// has written.
func (m *mediaRun) begin(at int64) {
	if m.sum == nil {
		m.sum = sha256.New()
	}
	m.sum.Reset()
	m.state, m.nhead, m.at, m.prefix, m.typed, m.match, m.n = mediaHead, 0, at, 0, 0, 0, 0
}

// feed reads s, the next kept bytes of the text, as written.
func (m *mediaRun) feed(s []byte) {
	for len(s) > 0 {
		switch m.state {
		case mediaNone:
			return
		case mediaRunning:
			m.run(s)
			return
		case mediaHead:
			k := copy(m.head[m.nhead:], s)
			m.nhead, s = m.nhead+k, s[k:]
			if m.nhead < len(m.head) {
				return
			}
			if string(m.head[:]) == "data:" {
				m.state, m.prefix = mediaType, int64(len(m.head))
				continue
			}
			m.state = mediaRunning
			m.run(m.head[:])
		case mediaType:
			c := s[0]
			switch {
			case c == ';':
				m.state, m.match = mediaMarker, 1
			case mediaTypeByte(c) && m.typed < mediaTypeMax:
				m.typed++
			default:
				m.state = mediaNone
			}
			m.prefix, s = m.prefix+1, s[1:]
		case mediaMarker:
			if s[0] != base64Marker[m.match] {
				m.state = mediaNone
				return
			}
			m.match, m.prefix, s = m.match+1, m.prefix+1, s[1:]
			if m.match == len(base64Marker) {
				m.state = mediaRunning
			}
		}
	}
}

// run reads s, bytes of the payload, letting the text go at the first outside
// the alphabet.
func (m *mediaRun) run(s []byte) {
	i := 0
	for i < len(s) && mediaAlphabet[s[i]] {
		i++
	}
	_, _ = m.sum.Write(s[:i])
	m.n += int64(i)
	if i < len(s) {
		m.state = mediaNone
	}
}

// payload is the text, ended: where its payload begins among what its rewrite
// wrote, its length and its SHA-256, ok where it is one of [mediaMinBytes]
// or more.
func (m *mediaRun) payload() (at, n int64, sum [sha256.Size]byte, ok bool) {
	if m.state != mediaRunning || m.n < mediaMinBytes {
		return 0, 0, sum, false
	}
	m.sum.Sum(sum[:0])
	return m.at + m.prefix, m.n, sum, true
}

// mediaPlan is what the first reading decided of the payloads it found, in
// the order the spool holds them: each one kept apart, the first of its text
// or a repeat of it; one past the take's count or allowance, or in a unit not
// kept as written, is not listed and stays inline. A few dozen bytes a
// payload of a mebibyte or more, far inside the plan's sixteenth.
type mediaPlan struct {
	// on says the take keeps media apart ([scrubBounds.splitting]), and
	// allowance is the payload text its first occurrences may hold in all.
	on        bool
	allowance int64
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
	return mediaPlan{on: p.on, allowance: p.allowance}
}

// finding is p where the take keeps media apart, nil where it does not.
func (p *mediaPlan) finding() *mediaPlan {
	if !p.on {
		return nil
	}
	return p
}

// mediaSplit is where a take that keeps its media apart writes its body, and
// the payload text it may keep apart in all ([scrubBounds.splitting]).
type mediaSplit struct {
	body      io.Writer
	allowance int64
}

// splitting is b keeping each large payload apart in body, its first
// occurrences at most allowance bytes of text in all, beside the archive whole
// its scrub writes.
func (b scrubBounds) splitting(body io.Writer, allowance int64) scrubBounds {
	b.split = mediaSplit{body: body, allowance: allowance}
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
