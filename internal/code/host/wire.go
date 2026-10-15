package host

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/textcut"
)

// GIT'S WIRE: pkt-lines, side-band framing, and the two forms a refusal takes
// before a git process has run.

const (
	// maxPktPayload is the most a pkt-line carries: git's
	// LARGE_PACKET_DATA_MAX, 65520 bytes on the wire less the four of its
	// length.
	maxPktPayload = 65516

	// flushPkt ends a section; delimPkt separates a v2 request's capabilities
	// from its arguments; responseEndPkt ends a v2 stateless response.
	flushPkt = "0000"
	delimPkt = "0001"
)

// errMalformed is bytes that are not a pkt-line: a length that is not four hex
// digits, one under four that is no special packet, or one past git's largest.
var errMalformed = errors.New("host: not a pkt-line")

// wireOID is an object id AS GIT READS ONE FROM A CLIENT — parse_oid_hex:
// gitref.OIDHexLen hex digits of EITHER case — in the one spelling the engine
// holds an id in, lowercase (gitref.ValidOID); ok is false for anything else.
// Every reader of what a client sends holds an id to this, never to ValidOID
// alone, because git takes an id written in capitals: held to ValidOID, a
// push's command in capitals was a line the receiver skipped and receive-pack
// acted on — one step 2 never measured.
func wireOID[S ~string | ~[]byte](id S) (string, bool) {
	if len(id) != gitref.OIDHexLen {
		return "", false
	}
	out := make([]byte, len(id))
	for i := range len(id) {
		switch c := id[i]; {
		case '0' <= c && c <= '9', 'a' <= c && c <= 'f':
			out[i] = c
		case 'A' <= c && c <= 'F':
			out[i] = c + ('a' - 'A')
		default:
			return "", false
		}
	}
	return string(out), true
}

// pktLine is payload as one pkt-line. payload is at most maxPktPayload bytes;
// every caller writes what it built itself.
func pktLine(payload []byte) []byte {
	out := make([]byte, 4, 4+len(payload))
	putLength(out, len(payload)+4)
	return append(out, payload...)
}

// pktString is pktLine of a string.
func pktString(payload string) []byte { return pktLine([]byte(payload)) }

func putLength(dst []byte, n int) {
	const hexDigits = "0123456789abcdef"
	for i := 3; i >= 0; i-- {
		dst[i] = hexDigits[n&0xf]
		n >>= 4
	}
}

// pkt is one packet read: its payload, or which special packet it was.
type pkt struct {
	payload []byte
	flush   bool // 0000
	delim   bool // 0001, protocol v2's section delimiter
	end     bool // 0002, protocol v2's response end
}

// readPkt reads one packet from r. The payload is a fresh slice the caller
// keeps. io.EOF is returned only before the first byte of a packet; a packet
// cut short is io.ErrUnexpectedEOF.
func readPkt(r *bufio.Reader) (pkt, error) {
	var head [4]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return pkt{}, err
	}
	n, ok := pktLength(head)
	if !ok {
		return pkt{}, fmt.Errorf("%w: length %q", errMalformed, head[:])
	}
	switch {
	case n == 0:
		return pkt{flush: true}, nil
	case n == 1:
		return pkt{delim: true}, nil
	case n == 2:
		return pkt{end: true}, nil
	case n < 4 || n > maxPktPayload+4:
		return pkt{}, fmt.Errorf("%w: length %d", errMalformed, n)
	}
	payload := make([]byte, n-4)
	if _, err := io.ReadFull(r, payload); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return pkt{}, err
	}
	return pkt{payload: payload}, nil
}

// pktLength reads a pkt-line's length: four LOWERCASE hex digits, the only
// spelling git writes (`%04x`). An upper-case digit is refused rather than read,
// so that every packet this package accepts re-encodes to the bytes it was read
// from — which is what lets the receive path hand git exactly what it was sent.
func pktLength(head [4]byte) (int, bool) {
	n := 0
	for _, c := range head {
		switch {
		case c >= '0' && c <= '9':
			n = n<<4 | int(c-'0')
		case c >= 'a' && c <= 'f':
			n = n<<4 | int(c-'a'+10)
		default:
			return 0, false
		}
	}
	return n, true
}

// encodePkt is the bytes p was read from — the inverse of readPkt for every
// packet it accepts.
func encodePkt(p pkt) []byte {
	switch {
	case p.flush:
		return []byte(flushPkt)
	case p.delim:
		return []byte(delimPkt)
	case p.end:
		return []byte("0002")
	}
	return pktLine(p.payload)
}

// The side-band channels a refusal report travels in when the client asked
// for side-band-64k: the report itself, and progress (git prints it as
// `remote: …`). The third, a fatal error, is receive-pack's to send; the
// engine's refusals are reports.
const (
	bandData     byte = 1
	bandProgress byte = 2
)

// banded is data in channel ch, as pkt-lines of at most maxPktPayload bytes
// with the channel byte first.
func banded(ch byte, data []byte) []byte {
	var out []byte
	for len(data) > 0 || out == nil {
		n := min(len(data), maxPktPayload-1)
		out = append(out, pktLine(append([]byte{ch}, data[:n]...))...)
		data = data[n:]
		if len(data) == 0 {
			break
		}
	}
	return out
}

// sentence is a refusal's words as git prints them: one line, at most
// maxSentence bytes (textcut's rule for a diagnostic).
func sentence(s string) string {
	s = strings.Join(strings.Fields(strings.ReplaceAll(s, "\n", " ")), " ")
	return textcut.Ellipsis(s, maxSentence)
}

// errAdvert is an advertisement that refuses: the service line, a flush and
// `ERR <sentence>` — what git prints as `fatal: remote error: <sentence>`
// for protocol v0, v2 and a push alike. A 200, because git reads an
// advertisement's body only from one.
func errAdvert(w http.ResponseWriter, svc, words string) {
	h := w.Header()
	h.Set("Content-Type", "application/x-"+svc+"-advertisement")
	h.Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	var body bytes.Buffer
	body.Write(pktString("# service=" + svc + "\n"))
	body.WriteString(flushPkt)
	body.Write(pktString("ERR " + sentence(words) + "\n"))
	_, _ = w.Write(body.Bytes())
}

// writeRefusalReport answers a push the engine refused before receive-pack
// ran, as receive-pack itself would: in band 2 `crewlet: <note>` (omitted
// under quiet and without side-band), then — in band 1 when the client asked
// for side-band-64k, bare otherwise — the REPORT, `unpack ok`, `ng <ref>
// <sentence>` for every command and a flush, written only to a client that
// asked for one (report-status or report-status-v2: receive-pack writes no
// report otherwise, and git's own client always asks); then, under side-band,
// the outer flush. THE CALLER HAS READ THE BODY TO ITS END: a response written
// while git is still uploading is lost to a broken pipe, and git prints `RPC
// failed` instead of the reason (measured).
func writeRefusalReport(w http.ResponseWriter, caps capabilities, refused []answered, note string) {
	h := w.Header()
	h.Set("Content-Type", "application/x-git-receive-pack-result")
	h.Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(refusalReport(caps, refused, note))
}

// refusalReport is writeRefusalReport's body.
func refusalReport(caps capabilities, refused []answered, note string) []byte {
	var report bytes.Buffer
	if caps.reportStatus || caps.reportStatusV2 {
		report.Write(pktString("unpack ok\n"))
		for _, a := range refused {
			report.Write(pktString(a.line() + "\n"))
		}
		report.WriteString(flushPkt)
	}
	if !caps.sideBand64k {
		return report.Bytes()
	}
	var out bytes.Buffer
	if note != "" && !caps.quiet {
		out.Write(banded(bandProgress, []byte("crewlet: "+sentence(note)+"\n")))
	}
	if report.Len() > 0 {
		out.Write(banded(bandData, report.Bytes()))
	}
	out.WriteString(flushPkt)
	return out.Bytes()
}
