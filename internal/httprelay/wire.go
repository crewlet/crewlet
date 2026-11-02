package httprelay

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/queue"
)

// MaxBody is the most request body, and the most response body one frame,
// a relay carries.
//
// HALF THE BROKER'S CEILING ([queue.MaxPayloadBytes]), because a message
// carries the bytes base64-encoded inside JSON — four thirds of them — with
// the request's header and the signed frame around it, and half leaves that
// room to spare. A request over it is refused before anything is sent ([Read]),
// and a response over it is simply more frames.
const MaxBody = queue.MaxPayloadBytes / 2

// The relay's clocks. Each is a property of the conversation rather than of a
// deployment, so none is configuration.
const (
	// Heartbeat is how long a holder's response goes without a frame:
	// an idle event stream is pushed an empty one this often, so its
	// entry knows the holder is still there, and the entry's
	// acknowledgement tells the holder the same of the entry.
	//
	// TEN SECONDS: each beat is one broker round trip per open stream,
	// and the beat only has to be short against [Idle].
	Heartbeat = 10 * time.Second

	// Idle is how long either side goes without hearing from the other
	// before it gives the exchange up: three heartbeats, so a lost frame
	// and a lost acknowledgement in a row are survived with a beat to
	// spare.
	Idle = 3 * Heartbeat

	// openWait bounds one OPEN's round trip. The holder answers an open at
	// once — it starts the exchange and replies, and the response follows
	// as frames — so two seconds is a broker round trip with a wide margin.
	openWait = 2 * time.Second

	// openTries is how many times one [Relay.Forward] sends its OPEN, under
	// one exchange id, before it reports the holder unanswered. Repeated
	// under ONE id, because a lost reply leaves the asker unable to tell
	// an exchange that started from one that did not, and the holder
	// starts an exchange once per id however often it is opened.
	openTries = 3

	// pushWait bounds one frame's round trip, which ends only once the
	// entry has written the frame to its client — a few MiB to a box on a
	// slow link. A frame unacknowledged in time is pushed again, which the
	// entry acknowledges without writing it twice.
	pushWait = 5 * time.Second
)

// clocks is the relay's clocks as one relay runs them: the constants above,
// which a case in this package shortens to see a heartbeat or a silence
// without waiting one out.
type clocks struct {
	heartbeat, idle, openWait, pushWait time.Duration
}

var defaultClocks = clocks{heartbeat: Heartbeat, idle: Idle, openWait: openWait, pushWait: pushWait}

// Verdict is what a process can do for a request addressed to it.
type Verdict string

const (
	// Serve means this process holds what the request needs, and answers it.
	Serve Verdict = "serve"
	// Elsewhere means this process does not hold it — not yet, or no longer —
	// and the asker should find who does again.
	Elsewhere Verdict = "elsewhere"
	// Refuse means nobody holds it, and nobody will.
	Refuse Verdict = "refuse"
)

// Valid reports whether v is one of the three verdicts.
func (v Verdict) Valid() bool { return v == Serve || v == Elsewhere || v == Refuse }

// Request is one HTTP request as a relay carries it: read whole on the entry,
// and rebuilt on the holder for its own handler.
type Request struct {
	Method string      `json:"method"`
	URI    string      `json:"uri"`
	Host   string      `json:"host,omitempty"`
	Header http.Header `json:"header,omitempty"`
	Body   []byte      `json:"body,omitempty"`

	// RemoteAddr is the client's address as the entry saw it, which a
	// handler reads off a request it was handed directly, so the holder's
	// handler is handed the same. The ENTRY'S own address is not carried:
	// no holder's handler judges a call by the interface it arrived on,
	// and the one call that would be judged by where it came from — one
	// to a bridge session admitted from its holder's loopback alone — is
	// never relayed (internal/api/mcpbridge).
	RemoteAddr string `json:"remote_addr,omitempty"`
}

// ErrTooLarge is a request body over [MaxBody]. The caller answers it 413.
var ErrTooLarge = errors.New("httprelay: the request body is larger than a relay carries")

// Read reads r whole, for [Relay.Forward] — once, however many holders it is
// then offered to. A body over [MaxBody] is [ErrTooLarge].
func Read(r *http.Request) (*Request, error) {
	var body []byte
	if r.Body != nil {
		var err error
		body, err = io.ReadAll(io.LimitReader(r.Body, MaxBody+1))
		if err != nil {
			return nil, fmt.Errorf("httprelay: read the request body: %w", err)
		}
		if len(body) > MaxBody {
			return nil, ErrTooLarge
		}
	}
	return &Request{
		Method: r.Method, URI: r.URL.RequestURI(), Host: r.Host,
		Header: withoutHopByHop(r.Header), Body: body, RemoteAddr: r.RemoteAddr,
	}, nil
}

// hopByHop is what a relay never carries: the headers that describe one
// connection rather than the request on it (RFC 9110 §7.6.1). The holder's
// handler is handed a request on no connection, and the entry's client is
// answered on its own.
var hopByHop = []string{
	"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Proxy-Connection", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// withoutHopByHop clones h without the hop-by-hop headers, and without any
// header the Connection header names as one.
func withoutHopByHop(h http.Header) http.Header {
	out := h.Clone()
	for _, listed := range h.Values("Connection") {
		for name := range strings.SplitSeq(listed, ",") {
			out.Del(strings.TrimSpace(name))
		}
	}
	for _, name := range hopByHop {
		out.Del(name)
	}
	return out
}

// --- the two signed channels ----------------------------------------------

// openKind is what an OPEN-channel request asks.
type openKind string

const (
	kindOpen  openKind = "open"
	kindClose openKind = "close"
)

// openRequest is what an entry sends a holder's node subject.
type openRequest struct {
	ID       string    `json:"id"`
	Deadline time.Time `json:"deadline"`
	Kind     openKind  `json:"kind"`

	// Owner is the incarnation the entry's routing named: a node restarted
	// since is a different process, and answers [Elsewhere] rather than
	// for its predecessor.
	Owner string `json:"owner"`

	// Exchange names the exchange: the subject its frames go to, and what
	// an OPEN repeated after a lost reply is recognised by.
	Exchange string `json:"exchange"`

	// Request is the request, on an OPEN.
	Request *Request `json:"request,omitempty"`
}

func (q openRequest) Binding() (string, time.Time) { return q.ID, q.Deadline }

// openReply is a holder's answer to an OPEN or a CLOSE.
type openReply struct {
	Request string  `json:"request"`
	Verdict Verdict `json:"verdict"`
	// Why is what the holder says of a verdict other than [Serve], for the
	// entry's log.
	Why string `json:"why,omitempty"`
}

func (r openReply) Answers() string { return r.Request }

// frameRequest is one frame of a response, pushed by the holder to the
// exchange's subject.
type frameRequest struct {
	ID       string    `json:"id"`
	Deadline time.Time `json:"deadline"`
	Exchange string    `json:"exchange"`

	// Seq is the frame's place in the response, from 1. A frame is pushed
	// again under the same Seq until it is acknowledged.
	Seq int `json:"seq"`

	// Status and Header are the response's, on the first frame after the
	// holder's handler wrote them, and absent before and after.
	Status int         `json:"status,omitempty"`
	Header http.Header `json:"header,omitempty"`

	// Body is the next stretch of the response's body.
	Body []byte `json:"body,omitempty"`

	// End says the response is complete with this frame.
	End bool `json:"end,omitempty"`
}

func (q frameRequest) Binding() (string, time.Time) { return q.ID, q.Deadline }

// frameReply acknowledges a frame.
type frameReply struct {
	Request string `json:"request"`
	Seq     int    `json:"seq"`
	// Closed says the entry has stopped — its client is gone — and wants
	// nothing more of the exchange.
	Closed bool `json:"closed,omitempty"`
}

func (r frameReply) Answers() string { return r.Request }

// nodeSubject is the one subject a relay's node answers OPEN and CLOSE on,
// with the node id escaped as one subject token — an id may carry a dot,
// which would split it into two.
func nodeSubject(name, node string) string {
	return "crewlet." + name + ".node." + coord.DocumentKey(node)
}

// exchangeSubject is the subject one exchange's frames are pushed to. The id
// is [signedask.NewID]'s, which is a subject token as it is.
func exchangeSubject(name, id string) string {
	return "crewlet." + name + ".exchange." + id
}
