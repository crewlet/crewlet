package host

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"iter"
	"strings"

	"github.com/crewlet/crewlet/internal/gitref"
)

// AN UPLOAD-PACK REQUEST, READ FOR THREE FACTS: the level its answer needs (an
// `ls-refs` names refs, so is read at the request's refs level; a fetch by
// object id names none), the objects it wants (a node lacking one syncs once
// before it answers), and whether it offers any `have` (one offering none is
// sent each want's whole history, a clone to the heat: cache.Use.Clones). A
// fetch never names a ref: the host offers no ref-in-want (cache.ServeConfig),
// and upload-pack refuses a `want-ref` it did not offer. Never a refusal: upload-pack refuses a malformed request in its own words.
// Read as a STREAM from memory or a spool, so a partial clone's million wants
// are never held whole.

// The fetch commands a request is recorded as (crewlet.code.fetch.duration's
// `command`), a closed set: a v2 command this build does not name records as a
// fetch, as upload-pack treats it.
const (
	commandAdvertise = "advertise"
	commandLsRefs    = "ls-refs"
	commandFetch     = "fetch"
)

// negotiation is what an upload-pack request asks.
type negotiation struct {
	// v2 is a protocol v2 request: a command, its capabilities, its arguments.
	v2 bool
	// command is the v2 command, or commandFetch for v0.
	command string
	// wants is whether it names any object by id.
	wants bool
	// haves is whether it offers any object the client has (`have <oid>`),
	// which cuts the answer.
	haves bool
}

// recorded is the command the request is recorded as.
func (n negotiation) recorded() string {
	if n.v2 && n.command == commandLsRefs {
		return commandLsRefs
	}
	return commandFetch
}

// clone reports a fetch that wants objects and offers none, so is sent each
// want's whole history. Git negotiates nothing for any fresh clone (full,
// shallow, partial) nor a partial clone's fetch of what it lacks, so both
// count; a client holding the repository offers its tips.
func (n negotiation) clone() bool {
	return n.recorded() == commandFetch && n.wants && !n.haves
}

// namesRefs reports an `ls-refs`, whose answer names refs and so is read at the
// request's refs level (admission.refs), not from whatever the cache holds.
func (n negotiation) namesRefs() bool {
	return n.v2 && n.command == commandLsRefs
}

// parseNegotiation reads a request until it has learnt what it reads for, or
// to the first packet it cannot read: what it learnt by then is the answer,
// since upload-pack refuses a malformed request (readFailure).
//
// v2: `command=<c>`, capability lines to the delimiter, then arguments to the
// flush. v0: `want` lines (and any shallow and deepen lines) to the first
// flush, then only to the round's first `have`: all clone asks, and a clone's
// next line is its `done`.
func parseNegotiation(src io.Reader, size int64, v2 bool) (negotiation, error) {
	n := negotiation{v2: v2, command: commandFetch}
	r := pktReader(src, size)
	args := !v2
	// rounds: a v0 request past its first flush, where its haves are.
	rounds := false
	for first := true; ; first = false {
		p, err := readPkt(r)
		if err != nil {
			return n, readFailure(err)
		}
		switch {
		case p.end:
			return n, nil
		case p.flush:
			if v2 || rounds {
				return n, nil
			}
			rounds = true
			continue
		case p.delim:
			args = true
			continue
		}
		line := string(bytes.TrimSuffix(p.payload, []byte{'\n'}))
		if v2 && first {
			if c, ok := strings.CutPrefix(line, "command="); ok {
				n.command = c
			}
			continue
		}
		switch {
		case !args:
		case haveOID(line) != "":
			n.haves = true
			if rounds {
				return n, nil
			}
		case !rounds && wantOID(line) != "":
			n.wants = true
		}
	}
}

// wantOID is the object a `want <oid>[ <caps>]` line names, or "", read as
// upload-pack reads it (receive_needs for v0, parse_want for v2): the
// gitref.OIDHexLen bytes after `want `, hex in either case (wireOID), whatever
// follows. A want read more narrowly than git reads it is never checked as
// held, and upload-pack answers it `not our ref` on a node one sync from
// serving it.
func wantOID(line string) string { return oidAfter(line, "want ") }

// haveOID is the object a `have <oid>` line offers, or "": the id read as a
// want's is (oidAfter).
func haveOID(line string) string { return oidAfter(line, "have ") }

// oidAfter is the object id the gitref.OIDHexLen bytes after prefix name, as
// upload-pack reads one, or "" for none or the zero id.
func oidAfter(line, prefix string) string {
	rest, ok := strings.CutPrefix(line, prefix)
	if !ok || len(rest) < gitref.OIDHexLen {
		return ""
	}
	oid, ok := wireOID(rest[:gitref.OIDHexLen])
	if !ok || oid == gitref.ZeroOID {
		return ""
	}
	return oid
}

// wantsOf is the object ids a request wants, read from src as a stream (a v2
// fetch's arguments, or v0's want lines), skipping a want naming no object id,
// which upload-pack refuses itself. A failed read yields its error and ends.
func wantsOf(src func() io.Reader, size int64, v2 bool) iter.Seq2[string, error] {
	return func(yield func(string, error) bool) {
		r := pktReader(src(), size)
		args := !v2
		for first := true; ; first = false {
			p, err := readPkt(r)
			if err != nil {
				if err = readFailure(err); err != nil {
					yield("", err)
				}
				return
			}
			switch {
			case p.flush, p.end:
				return
			case p.delim:
				args = true
				continue
			}
			if !args || (v2 && first) {
				continue
			}
			if oid := wantOID(string(bytes.TrimSuffix(p.payload, []byte{'\n'}))); oid != "" {
				if !yield(oid, nil) {
					return
				}
			}
		}
	}
}

// pktReader buffers a request of size bytes: as large as the request, up to
// 64 KiB, so a clone's negotiation of a few hundred bytes never costs 64 KiB.
func pktReader(src io.Reader, size int64) *bufio.Reader {
	return bufio.NewReaderSize(src, int(min(max(size, 16), 64<<10)))
}

// readFailure is a parse's ending as its caller sees it: the end of the bytes,
// or a packet that is not one, is no failure (what was read is the answer;
// upload-pack refuses a malformed request); the source's own read failing is.
func readFailure(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, errMalformed) {
		return nil
	}
	return err
}
