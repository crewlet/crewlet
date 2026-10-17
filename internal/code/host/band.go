package host

import (
	"bytes"
	"errors"

	"github.com/crewlet/crewlet/internal/code/gitproc"
)

// AN UPLOAD-PACK ANSWER, READ AS GIT WRITES IT, for the two facts the host
// needs before it lets a fetch's answer start: whether its PACK has begun, and
// what git said in its side band.
//
// A fetch's pack-objects runs AFTER upload-pack has written its answer's first
// line — protocol v2's `packfile`, v0's NAK or ACK — and the words it dies
// with reach the client on side band 2 and 3 of that answer, never on
// upload-pack's stderr, which says only that pack-objects died ("aborting due
// to possible repository corruption on the remote side", whatever killed it).
// A damaged cache is named there and nowhere else: over a multi-pack-index
// whose reverse index is damaged, the bitmap a clone reads makes pack-objects
// die "invalid MIDX object position, MIDX is likely corrupt". Answered at its
// first byte, such a fetch had already begun its response when git said so —
// past the point a repair can run it again (repair.go) — and git's own
// failure was its answer, on every fetch the copy served, until an operator's
// repair. So a fetch's sink HOLDS the answer until the pack begins (sink.go),
// reading the side band as it arrives; a process that fails holding only its
// preamble has shown the client nothing, and damage its words name is
// repaired, and the request run again, like any failure before the first byte.
//
// THE PACK HAS BEGUN at the first of: a band-1 packet — pack data, or the
// empty keepalive upload-pack sends after uploadpack.keepAlive seconds of
// silence from pack-objects — or a band-2 line that names no damage, which
// is pack-objects' progress (and arrives each second while it counts). So a
// hold lasts until pack-objects' first word, at most a keepalive for a client
// that asked for no progress; the damage line is pack-objects' first word,
// since it dies loading the bitmap, before it writes any progress. Band 3 is a
// process ending, and never begins anything.
//
// THE BAND IS THE PACKET'S FIRST BYTE, under either protocol: every line of
// the negotiation upload-pack writes before its pack — ACK, NAK, ready,
// shallow, a v2 section header, ERR — is text, and the host advertises no
// sideband-all, the one capability that would put those lines in band 1 too.
// A packet that is not one (a v0 pack sent with no side band begins with its
// raw "PACK") begins the pack, and nothing past it is read.

// answerScan reads an upload-pack answer's packets as they are written —
// headers whole, payloads in whatever pieces they arrive — keeping no band-1
// payload and no more of a side band's line than gitproc.MaxLine.
type answerScan struct {
	head  [4]byte
	nhead int  // bytes of the current header read
	left  int  // payload bytes of the current packet still to read
	first bool // the next payload byte is the current packet's first
	band  byte // the current packet's band; 0 a line of the negotiation
	// text is band 2's and band 3's line in progress: a line upload-pack
	// relays is cut wherever pack-objects' write was, so it may continue in
	// the next packet of its band.
	text [2][]byte
	raw  bool // the answer is no longer pkt-lines: nothing past it is read
	// begun is the pack having begun (above).
	begun bool
	words gitproc.Words
}

// scan reads p, the next bytes of the answer.
func (s *answerScan) scan(p []byte) {
	for len(p) > 0 && !s.raw {
		if s.left == 0 {
			n := copy(s.head[s.nhead:], p)
			s.nhead += n
			p = p[n:]
			if s.nhead < len(s.head) {
				return
			}
			s.nhead = 0
			length, ok := pktLength(s.head)
			switch {
			case !ok, length == 3, length > maxPktPayload+4:
				s.raw, s.begun = true, true
				return
			case length < 4:
				continue // a flush, a delimiter or a response end: no payload
			}
			s.left, s.first, s.band = length-4, true, 0
			continue
		}
		if s.first {
			s.first = false
			if b := p[0]; b == bandData || b == bandProgress || b == bandError {
				s.band = b
				s.begun = s.begun || b == bandData
				p = p[1:]
				s.left--
				continue
			}
		}
		n := min(len(p), s.left)
		if s.band == bandProgress || s.band == bandError {
			s.say(s.band, p[:n])
		}
		p = p[n:]
		s.left -= n
	}
}

// say reads b, more of a side band's text, line by line: a line ends at a
// newline or at a carriage return (git's progress rewrites its line in place
// with one), and one longer than gitproc.MaxLine is read as one at that
// length, as a process's stderr is.
func (s *answerScan) say(band byte, b []byte) {
	buf := &s.text[band-bandProgress]
	for len(b) > 0 {
		end := bytes.IndexAny(b, "\r\n")
		text := b
		if end >= 0 {
			text = b[:end]
		}
		for len(text) > 0 {
			take := min(len(text), gitproc.MaxLine-len(*buf))
			*buf = append(*buf, text[:take]...)
			text = text[take:]
			if len(*buf) == gitproc.MaxLine {
				s.line(band, *buf)
				*buf = (*buf)[:0]
			}
		}
		if end < 0 {
			return
		}
		s.line(band, *buf)
		*buf = (*buf)[:0]
		b = b[end+1:]
	}
}

// line reads one complete line of band band: classified with every other
// (gitproc.Words), and — in band 2, naming no damage — the pack's beginning.
func (s *answerScan) line(band byte, l []byte) {
	if len(l) == 0 {
		return
	}
	s.words.Add(l)
	if band == bandProgress && !s.begun {
		var alone gitproc.Words
		alone.Add(l)
		s.begun = alone.Damage() == nil
	}
}

// finish reads what is left of each side band's text as a line: what git
// wrote last, if it ended without a newline. Called once the process has
// exited.
func (s *answerScan) finish() {
	for i := range s.text {
		if len(s.text[i]) > 0 {
			s.line(bandProgress+byte(i), s.text[i])
			s.text[i] = s.text[i][:0]
		}
	}
}

// bandDamage is a git process's failure beside the damage git named in its
// answer's side band rather than on its stderr: upload-pack's stderr says only
// that its pack-objects died, and pack-objects said why in band. It is the
// failure (errors.As reaches the *gitproc.ExitError) and the damage
// (errors.Is reaches gitproc.ErrCorrupt or gitproc.ErrMissingObject), so
// cache.DamageReason reads it as it reads damage on stderr.
type bandDamage struct {
	err   error  // the process's failure: a *gitproc.ExitError
	kind  error  // gitproc.ErrCorrupt or gitproc.ErrMissingObject
	named string // the side band's line that named it
}

func (e *bandDamage) Error() string {
	return e.err.Error() + "; in its answer's side band: " + e.named
}

func (e *bandDamage) Unwrap() []error { return []error{e.kind, e.err} }

// judged is err, the failure of the process whose answer s read, with the
// damage THE ANSWER'S SIDE BAND named folded in (bandDamage) — the SIDE BAND
// AND NOTHING ELSE, because the side band carries pack-objects' own words over
// bounded object ids, which a request cannot inject into, while upload-pack's
// stderr ECHOES the client's request lines ("fatal: unexpected line: '<arg>'",
// a bad filter's quoted type, a want it could not parse): a crafted argument
// quoting " is corrupt" or "MIDX is likely corrupt" classifies on that stderr
// as damage, so a repair keyed on it would let any fetch — a token bound to
// nobody among them — force a cache rebuild. So an upload-pack run's repair
// reads git's verdict from bandDamage alone ([fetchDamage]), never
// [gitproc.ExitError.Kind] from the run's stderr; genuine pack-phase damage is
// the side band's and is kept, and the pool's refusal of the incarnation's
// shape, made before git starts, is no process's words and is read as itself.
// A process the ENGINE ended — past its deadline, stalled, its input or output
// failing — is no verdict about what it read, and is answered as itself.
func (s *answerScan) judged(err error) error {
	s.finish()
	var exit *gitproc.ExitError
	if !errors.As(err, &exit) {
		return err
	}
	if kind := s.words.Damage(); kind != nil {
		return &bandDamage{err: err, kind: kind, named: s.words.Named()}
	}
	return err
}
