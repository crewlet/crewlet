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

// AN UPLOAD-PACK REQUEST, READ FOR TWO FACTS: which level the answer needs
// (an `ls-refs` or a `want-ref` names refs, so it is linearizable; a fetch by
// object id is not), and which objects it wants (so a node that lacks one
// syncs once before it answers). Never a refusal: a malformed request is
// passed to upload-pack, which refuses it in its own words. Read as a STREAM
// from wherever the request is held — memory and a spool — so a partial
// clone's request of a million wants is never held whole.

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
	// wantRefs is how many `want-ref` lines it carries.
	wantRefs int
	// wants is whether it names any object by id.
	wants bool
}

// command is what the request is recorded as.
func (n negotiation) recorded() string {
	if n.v2 && n.command == commandLsRefs {
		return commandLsRefs
	}
	return commandFetch
}

// linearizable reports whether the answer names a ref, and so must be read
// through the code log's barrier: an `ls-refs`, or a fetch by `want-ref`.
func (n negotiation) linearizable() bool {
	return (n.v2 && n.command == commandLsRefs) || n.wantRefs > 0
}

// parseNegotiation reads a request to its end, or to the first packet it
// cannot read — what it learnt by then is the answer, since upload-pack, not
// this parse, is what refuses a malformed request. Only the source's own read
// failing is an error.
//
// v2: `command=<c>` (the first pkt-line), capability lines to the delimiter,
// then arguments to the flush — `want <oid>` and `want-ref <ref>`. v0: `want
// <oid>[ <caps>]` lines to the first flush.
func parseNegotiation(src io.Reader, size int64, v2 bool) (negotiation, error) {
	n := negotiation{v2: v2, command: commandFetch}
	r := pktReader(src, size)
	args := !v2
	for first := true; ; first = false {
		p, err := readPkt(r)
		if err != nil {
			return n, readFailure(err)
		}
		switch {
		case p.flush, p.end:
			return n, nil
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
		if !args {
			continue
		}
		switch {
		case strings.HasPrefix(line, "want-ref "):
			n.wantRefs++
		case wantOID(line) != "":
			n.wants = true
		}
	}
}

// wantOID is the object a `want <oid>[ <caps>]` line names, or "".
func wantOID(line string) string {
	rest, ok := strings.CutPrefix(line, "want ")
	if !ok {
		return ""
	}
	oid, _, _ := strings.Cut(rest, " ")
	if !gitref.ValidOID(oid) || oid == gitref.ZeroOID {
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
