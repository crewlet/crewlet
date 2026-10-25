package jsapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsprovision"
)

// Of names which of the engine's two APIs js was built on.
//
// A caller that reaches past a client it was HANDED — rather than one it
// built — has to address its raw request where the client's own requests go,
// or nothing answers it: a leaf's broker serves JetStream only under the
// embedded fleet's domain. [API.Subject] is the one rule for that address, so
// this recovers the API from the client instead of a caller spelling the
// address a second time. Every client the engine builds comes from
// [API.Client], so any other shape is a wiring mistake and is named.
func Of(js jetstream.JetStream) (API, error) {
	switch o := js.Options(); {
	case o.APIPrefix != "":
		return API{}, fmt.Errorf("jsapi: the JetStream client addresses the "+
			"custom API prefix %q, which is neither API this engine speaks — "+
			"build it with internal/jsapi", o.APIPrefix)
	case o.Domain == "":
		return Account(), nil
	case o.Domain == Domain:
		return Embedded(), nil
	default:
		return API{}, fmt.Errorf("jsapi: the JetStream client addresses the "+
			"domain %q, which is neither API this engine speaks — build it "+
			"with internal/jsapi", o.Domain)
	}
}

// ErrNoMessage is a stream leader answering that it holds no message where one
// was asked for: none on the subject, or none at the sequence. An ANSWER, not
// a failure — and only the leader's: see [Leader].
var ErrNoMessage = errors.New("jsapi: the stream leader holds no such message")

// Message is one stored message as a stream's leader answered it.
type Message struct {
	Subject  string
	Sequence uint64
	// Header is the message's headers in their wire form (`NATS/1.0` and
	// the lines after it), nil for a message with none —
	// nats.DecodeHeadersMsg reads it.
	Header []byte
	Data   []byte
	Time   time.Time
}

// Leader reads single messages of one stream from the stream's LEADER: a
// `$JS.API.STREAM.MSG.GET`, the one read of a single message that no replica
// answers from its own copy.
//
// # Why not the client's own reads
//
// A stream that allows direct gets — every KV bucket and every object store
// does — is read by the client ONLY through `DIRECT.GET`, which any replica
// answers, one that is behind the quorum included: the client picks the API
// from the stream's allow_direct flag alone and offers no way to ask for the
// other. A behind replica answers "no message" for a key written a moment
// ago, which is the answer a caller deciding something from absence can
// least afford — coordination's listings certify themselves with this read,
// and the object store reads an object's metadata with it, where a "not
// found" would answer a just-uploaded file as unreadable and a deletion as
// already done.
//
// ONE implementation for every caller, because the request, its address
// across a leaf link and the reading of its answer are a rule that would
// otherwise be written once per caller (adr/0008).
//
// # And every read ASKED AGAIN while nobody answers it
//
// Only the leader answers, so a read is at the mercy of the moment it is
// sent. The broker decides it when a worker takes it from the API queue every
// routed request waits in, and a member that is not the stream's leader —
// while it still believes in one — returns without replying
// (server/jetstream_api.go, jsMsgGetRequest: "only the stream leader should
// answer"). So for the seconds between a leader's loss and its group's
// election every read is DROPPED, and a request held until its caller's
// deadline is a request nobody will ever answer; while a leader behind a
// backlog answers LATE, so a request thrown away at its term would throw away
// the only answer. Every read is therefore asked as a metadata read is
// ([jsprovision.ReadBoundedWith]) — the hedge the domain logs' leader reads
// already take, never a second idea of when a request is lost: each request
// presumed lost after [jsprovision.ReadTerm], the next sent
// [jsprovision.ReAsk] later with every earlier one still outstanding, the
// first answer from any of them taken, none outliving the call, and none
// asked past the longest silence the server keeps
// ([jsprovision.LookupBudget]). An ANSWER ends it whatever it says — a
// message, no message, a group with no leader ([server.JSClusterNotAvailErr]).
// "No responders" is no answer: a node whose link to the broker is down hears
// it at once, and its read is asked again every [jsprovision.ReAsk], where it
// failed at once while it was one request — a link that comes back inside the
// read's bound is answered, and one that does not costs the bound.
//
// A read whose caller set no deadline KEEPS THE BOUND IT HAD as one request,
// the client's own DefaultTimeout, and is asked again inside it: at none, two
// and four seconds of five, so a request a member dropped is asked again where
// the new leader answers it, while every caller sized against the five seconds
// — a boot's steps, a duty's tick — waits no longer for a broker that never
// answers than it did.
//
// It was one request, held for its caller's deadline or, for a caller with
// none, for the client's own five-second default — which is how the object
// collector's boot pass lost its read of the files bucket on a loaded fleet
// of three: `context deadline exceeded`, every node's first collection
// failed, and the next ten minutes away. An election measured why: with the
// leader stepped down or killed, the first read on every other member was
// never answered at all, and the one asked again two seconds later was
// answered by the new leader.
type Leader struct {
	conn   *nats.Conn
	stream string
	// msgGet and info are the stream's MSG.GET and STREAM.INFO subjects in
	// the API the client speaks.
	msgGet string
	info   string
	// term is how long one request for a message is given before it is
	// presumed lost: [jsprovision.ReadTerm], and the time a whole message
	// takes to cross a link where the stream's messages are large
	// ([Leader.Carrying]).
	term time.Duration
	// timeout bounds a read whose context has no deadline — the client's
	// OWN default, taken from the client, the bound the read had while it
	// was one request ([jsprovision.ReadBoundedWith]).
	timeout time.Duration
	// paged is how long the last subjects page this reader asked took to
	// be answered, which the next page's term is given beside the read
	// term ([Leader.Subjects]) — shared by every copy [Leader.Carrying]
	// makes, since they ask the one leader.
	paged *atomic.Int64
}

// NewLeader addresses the leader reads of stream in the API js speaks.
func NewLeader(js jetstream.JetStream, stream string) (*Leader, error) {
	api, err := Of(js)
	if err != nil {
		return nil, err
	}
	msgGet, err := api.Subject(fmt.Sprintf(server.JSApiMsgGetT, stream))
	if err != nil {
		return nil, err
	}
	info, err := api.Subject(fmt.Sprintf(server.JSApiStreamInfoT, stream))
	if err != nil {
		return nil, err
	}
	return &Leader{conn: js.Conn(), stream: stream, msgGet: msgGet, info: info,
		term: jsprovision.ReadTerm, timeout: js.Options().DefaultTimeout,
		paged: new(atomic.Int64)}, nil
}

// Carrying is l for a stream whose messages may be as large as messageBytes,
// over a link that may carry a mebibyte in as long as pace: every message read
// is presumed lost only once a reply of that size has had time to cross the
// link, beyond the read term.
//
// # Why a large reply gets a longer term
//
// The broker sends a message read's reply whole, and the client hears nothing
// of it until its last byte has arrived — so a reply carrying an object's
// whole piece, a base64-encoded 128 KiB in a reply of about 175 KB, spends
// five seconds crossing a leaf link at the object store's thirty-seconds-a-
// mebibyte floor before the reader sees any of it. Presumed lost at the one-
// second read term, it was asked again at two seconds and at four while the
// first reply was still arriving: the broker answers every request it is
// sent, so each re-ask put another whole piece on the link a node's lease
// renewals cross, where a put keeps its own bytes in flight to half a
// mebibyte ([objstore/natsobj]'s window) — three pieces on the link for one
// read. Given the transfer, a read on a link at the floor asks once; one
// asked again is a read whose reply could not have been on its way.
func (l *Leader) Carrying(messageBytes int, pace time.Duration) *Leader {
	c := *l
	reply := base64.StdEncoding.EncodedLen(messageBytes) + replyOverhead
	c.term = jsprovision.ReadTerm + time.Duration(float64(pace)*float64(reply)/mebibyte)
	return &c
}

// replyOverhead is the most a message read's reply carries beside the
// message's own data: the JSON envelope, the subject, the sequence, the time
// and the headers, base64-encoded — a kibibyte, which every reply the engine
// reads fits in with room to spare (a piece's headers are none; an object's
// metadata, its own data, is a few hundred bytes).
const replyOverhead = 1 << 10

// mebibyte is the unit a pace is stated in.
const mebibyte = 1 << 20

// Term is how long one of l's message reads is given before it is presumed
// lost and asked again ([Leader.Carrying]).
func (l *Leader) Term() time.Duration { return l.term }

// Last answers the newest message on subject, or [ErrNoMessage]. Subject may
// be a FILTER — `$O.<bucket>.M.>` — and the answer is then the newest message
// on any subject it matches.
func (l *Leader) Last(ctx context.Context, subject string) (Message, error) {
	msg, err := l.get(ctx, server.JSApiMsgGetRequest{LastFor: subject})
	if err != nil {
		return Message{}, err
	}
	return l.on(msg, subject)
}

// First answers the oldest message on subject, or [ErrNoMessage]; subject may
// be a filter, as [Leader.Last]'s may.
func (l *Leader) First(ctx context.Context, subject string) (Message, error) {
	return l.Next(ctx, subject, 0)
}

// Next answers the oldest message on subject at or after the sequence from,
// or [ErrNoMessage] where the leader holds none there; subject may be a
// filter, as [Leader.Last]'s may.
//
// It is the one question a reader of a REPLICA's copy has to ask before it
// takes the end of that copy for the end of the stream: a consumer is made on
// whichever replica the broker places it, one still catching up included, and
// such a replica says it has nothing more where the leader has.
func (l *Leader) Next(ctx context.Context, subject string, from uint64) (Message, error) {
	msg, err := l.get(ctx, server.JSApiMsgGetRequest{Seq: from, NextFor: subject})
	if err != nil {
		return Message{}, err
	}
	if msg.Sequence < from {
		// A LEADER THAT ANSWERED FROM BEFORE where it was asked to look
		// did not do what it was asked, and a reader taking its message
		// for the next one would take one it already had.
		return Message{}, fmt.Errorf("jsapi: the leader of %s answered with sequence %d for "+
			"the next message on %q from %d", l.stream, msg.Sequence, subject, from)
	}
	return l.on(msg, subject)
}

// on is msg, which the leader answered for subject — refused unless subject
// names or matches the subject msg is on.
func (l *Leader) on(msg Message, subject string) (Message, error) {
	if !server.SubjectMatchesFilter(msg.Subject, subject) {
		// A LEADER THAT ANSWERED ABOUT ANOTHER SUBJECT did not do what it
		// was asked, and a caller taking its message as this subject's
		// would read a value the subject never held.
		return Message{}, fmt.Errorf("jsapi: the leader of %s answered with %q for %q",
			l.stream, msg.Subject, subject)
	}
	return msg, nil
}

// At answers the message at seq, on whatever subject it is, or
// [ErrNoMessage].
func (l *Leader) At(ctx context.Context, seq uint64) (Message, error) {
	return l.get(ctx, server.JSApiMsgGetRequest{Seq: seq})
}

// get asks the leader for one message — asked again while nobody answers, see
// [Leader] — and reads its answer.
//
// THE REPLY IS DECODED ONCE, after the asking: every request carries the same
// question, so whichever was answered first is the answer, and a request the
// hedge cancelled is never read at all.
func (l *Leader) get(ctx context.Context, q server.JSApiMsgGetRequest) (Message, error) {
	req, err := json.Marshal(q)
	if err != nil {
		return Message{}, fmt.Errorf("jsapi: encode the read: %w", err)
	}
	reply, err := ask(ctx, l.conn, l.msgGet, req, l.term, l.timeout)
	if err != nil {
		return Message{}, fmt.Errorf("jsapi: read %s from its leader: %w", l.stream, err)
	}
	var resp server.JSApiMsgGetResponse
	if err := json.Unmarshal(reply.Data, &resp); err != nil {
		return Message{}, fmt.Errorf("jsapi: decode the leader of %s's answer: %w", l.stream, err)
	}
	switch msg := resp.Message; {
	case resp.Error != nil && server.IsNatsErr(resp.Error, server.JSNoMessageFoundErr):
		return Message{}, ErrNoMessage
	case resp.Error != nil:
		return Message{}, resp.Error
	case msg == nil:
		return Message{}, fmt.Errorf("jsapi: the leader of %s answered with neither a "+
			"message nor an error", l.stream)
	default:
		return Message{Subject: msg.Subject, Sequence: msg.Sequence, Header: msg.Header,
			Data: msg.Data, Time: msg.Time}, nil
	}
}
