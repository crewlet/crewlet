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

// AN UPLOAD-PACK REQUEST, READ FOR THREE FACTS: which level the answer needs
// (an `ls-refs` names refs, so it is read at the request's refs level —
// linearizable for a person, a box's run's session; a fetch by object id
// names none), which objects it wants (so a node that lacks one syncs once before
// it answers), and whether it offers any object the client already has — a
// fetch that offers none is sent each want's whole history, which the
// repository's heat counts as a clone (cache.Use.Clones). A fetch never names
// a ref: the host offers no ref-in-want (cache.ServeConfig says why), and
// upload-pack refuses a `want-ref` it did not offer. Never a refusal: a
// malformed request is passed to upload-pack, which refuses it in its own
// words. Read as a STREAM from wherever the request is held — memory and a
// spool — so a partial clone's request of a million wants is never held
// whole.

// The fetch commands a request is recorded as (crewlet.code.fetch.duration's
// `command`). A closed set: a v2 command this build does not name is recorded
// as a fetch, which is what upload-pack does with it if it does anything.
const (
	commandAdvertise = "advertise"
	commandLsRefs    = "ls-refs"
	commandFetch     = "fetch"
)

// negotiation is what an upload-pack request asks.
type negotiation struct {
	// v2 is a protocol v2 request: a command, its capabilities, its
	// arguments.
	v2 bool
	// command is the v2 command, or commandFetch for v0.
	command string
	// wants is whether it names any object by id.
	wants bool
	// haves is whether it offers any object the client already has (`have
	// <oid>`), which the answer is cut by.
	haves bool
}

// command is what the request is recorded as.
func (n negotiation) recorded() string {
	if n.v2 && n.command == commandLsRefs {
		return commandLsRefs
	}
	return commandFetch
}

// clone reports a fetch that wants objects and offers none the client has:
// the client is sent each want's whole history. Git negotiates nothing for a
// fresh clone of any shape — full, shallow, partial — and nothing for a
// partial clone's fetch of what it lacks, so both read as this; a fetch from
// a repository the client holds offers its tips, and one whose client is up to
// date sends no fetch at all.
func (n negotiation) clone() bool {
	return n.recorded() == commandFetch && n.wants && !n.haves
}

// namesRefs reports whether the answer names a ref, and so is read at the
// request's refs level (admission.refs) — through the code log's barrier for
// a person, at a box's run's session — rather than from whatever the cache
// holds: an `ls-refs`.
func (n negotiation) namesRefs() bool {
	return n.v2 && n.command == commandLsRefs
}

// parseNegotiation reads a request until it has learnt what it reads for, or
// to the first packet it cannot read — what it learnt by then is the answer,
// since upload-pack, not this parse, is what refuses a malformed request. Only
// the source's own read failing is an error.
//
// v2: `command=<c>` (the first pkt-line), capability lines to the delimiter,
// then arguments to the flush — `want <oid>` and `have <oid>` among them. v0:
// `want <oid>[ <caps>]` lines (and any shallow and deepen lines) to the first
// flush, then the round's `have <oid>` lines and its `done` or flush — read
// past the first flush only to its first have, which is all the clone test
// asks, and a clone's request holds none: its next line is its `done`.
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

// wantOID is the object a `want <oid>[ <caps>]` line names, or "": the id as
// upload-pack reads it (receive_needs for v0, parse_want for v2) — the
// gitref.OIDHexLen bytes after `want `, hex in either case (wireOID), whatever
// follows them, since git reads the id and nothing past it. A want read more
// narrowly than git reads it is one this node never checks it holds, and
// upload-pack answers it `not our ref` on a node one sync from serving it.
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

// wantsOf is the object ids a request wants, read from src as a stream — the
// argument lines of a v2 fetch, or v0's want lines — skipping any want that
// names no object id, which upload-pack refuses in its own words. A source
// that fails to read yields its error and ends.
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

// pktReader buffers a request of size bytes for reading its packets: as
// large as the request, up to 64 KiB — a few hundred bytes for a clone's
// negotiation, never a fixed 64 KiB a parse.
func pktReader(src io.Reader, size int64) *bufio.Reader {
	return bufio.NewReaderSize(src, int(min(max(size, 16), 64<<10)))
}

// readFailure is a request parse's ending as its caller sees it: the end of
// the bytes, or a packet that is not one, is no failure — what was read so far
// is the answer, and upload-pack is what refuses a malformed request — while
// the source's own read failing is.
func readFailure(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, errMalformed) {
		return nil
	}
	return err
}
