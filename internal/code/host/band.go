package host

import (
	"bytes"
)

// AN UPLOAD-PACK ANSWER, READ AS GIT WRITES IT, for the one fact the host
// needs before it lets a fetch's answer start: whether its PACK has begun.
//
// A fetch's pack-objects runs AFTER upload-pack has written its answer's first
// line — protocol v2's `packfile`, v0's NAK or ACK — and when it meets a
// damaged cache it dies there, with upload-pack's answer already begun: over a
// multi-pack-index whose reverse index is damaged, the bitmap a clone reads
// makes pack-objects die "invalid MIDX object position, MIDX is likely
// corrupt". What it met is classified from its trace2 events, which reach the
// pool on the stream every process of the tree writes to (gitproc), never from
// the words it sends the client. Answered at its first byte, such a fetch had
// already begun its response when git died — past the point a repair can run
// it again (repair.go) — and git's own failure was its answer, on every fetch
// the copy served, until an operator's repair. So a fetch's sink HOLDS the
// answer until the pack begins (sink.go); a process that fails holding only
// its preamble has shown the client nothing, and the damage its trace names is
// repaired, and the request run again, like any failure before the first byte.
//
// THE PACK HAS BEGUN at the first of: a band-1 packet — pack data, or the
// empty keepalive upload-pack sends after uploadpack.keepAlive seconds of
// silence from pack-objects — or a band-2 line that is not one of git's
// REPORTS, which is pack-objects' progress (and arrives each second while it
// counts). A report is a line beginning with one of usage.c's prefixes —
// "fatal: ", "error: ", "warning: ", "BUG: ", English under the pool's
// LC_ALL=C — so the line pack-objects dies with keeps the hold, whatever it
// says: the release rule judges only whether a line is a report, and nothing
// about what it names, which the run's trace decides once it has ended. So a
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

// reportPrefixes are the prefixes git's reports begin with (usage.c's die,
// error, warning and BUG routines), as a side band carries them.
var reportPrefixes = [][]byte{[]byte("fatal: "), []byte("error: "), []byte("warning: "), []byte("BUG: ")}

// reportLead is the longest report prefix: all of a band-2 line the release
// rule reads.
const reportLead = len("warning: ")

// answerScan reads an upload-pack answer's packets as they are written —
// headers whole, payloads in whatever pieces they arrive — keeping no band-1
// payload and no more of a band-2 line than reportLead bytes.
type answerScan struct {
	head  [4]byte
	nhead int  // bytes of the current header read
	left  int  // payload bytes of the current packet still to read
	first bool // the next payload byte is the current packet's first
	band  byte // the current packet's band; 0 a line of the negotiation
	// lead is the start of band 2's line in progress, up to reportLead: a
	// line upload-pack relays is cut wherever pack-objects' write was, so
	// its prefix may continue in the next packet of its band. judged is set
	// once the line in progress has been judged.
	lead   []byte
	judged bool
	raw    bool // the answer is no longer pkt-lines: nothing past it is read
	// begun is the pack having begun (above).
	begun bool
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
		if s.band == bandProgress {
			s.progress(p[:n])
		}
		p = p[n:]
		s.left -= n
	}
}

// progress reads b, more of band 2's text, line by line: a line ends at a
// newline or at a carriage return (git's progress rewrites its line in place
// with one). Each line is judged once, as soon as reportLead of it — or all
// of it, if shorter — has arrived.
func (s *answerScan) progress(b []byte) {
	for len(b) > 0 {
		end := bytes.IndexAny(b, "\r\n")
		text := b
		if end >= 0 {
			text = b[:end]
		}
		if !s.judged {
			take := min(len(text), reportLead-len(s.lead))
			s.lead = append(s.lead, text[:take]...)
			if len(s.lead) == reportLead {
				s.judge()
			}
		}
		if end < 0 {
			return
		}
		if !s.judged && len(s.lead) > 0 {
			s.judge()
		}
		s.lead, s.judged = s.lead[:0], false
		b = b[end+1:]
	}
}

// judge judges band 2's line in progress by its lead: a line that is not
// one of git's reports begins the pack.
func (s *answerScan) judge() {
	s.judged = true
	for _, prefix := range reportPrefixes {
		if bytes.HasPrefix(s.lead, prefix) {
			return
		}
	}
	s.begun = true
}
