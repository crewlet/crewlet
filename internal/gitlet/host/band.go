package host

import (
	"bytes"
)

// AN UPLOAD-PACK ANSWER, READ AS GIT WRITES IT, for whether its PACK has
// begun: ONE reading for the hold before a fetch's answer starts (sink.go;
// package doc, "A damaged cache is repaired under the request that found it")
// and the fetch audit's "sent the contents" (fetch.go), so both judge alike.
//
// THE PACK HAS BEGUN at the first band-1 packet (data, or the keepalive's empty
// one) or band-2 line that is no REPORT of git's (reportPrefixes, English under
// LC_ALL=C). Only the prefix is judged, so the damage line (before any
// progress: pack-objects dies loading the bitmap) keeps the hold, its trace
// naming it. Band 3 begins nothing. The band is a packet's first byte, since
// the host advertises no sideband-all; a non-packet (v0's raw "PACK") begins it.

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
	// relayed line is cut wherever pack-objects' write was, so its prefix may
	// continue in the next band-2 packet. judged: the line has been judged.
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
// newline or a carriage return (git's progress rewrites its line with one).
// Each line is judged once, when reportLead of it, or all of a shorter one,
// has arrived.
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
