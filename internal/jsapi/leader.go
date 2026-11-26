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

// Leader reads single messages of one stream from the stream's LEADER, the one
// single-message read (`STREAM.MSG.GET`) no replica answers from its own copy.
// The client reads a stream allowing direct gets (every KV bucket and object
// store) ONLY by `DIRECT.GET`, which a replica behind the quorum answers "no
// message" for a key just written. ONE implementation for every caller (adr/0008).
//
// Every read is ASKED AGAIN while nobody answers: a non-leader replies not at
// all, so between a leader's loss and its election reads are dropped, while a
// backlogged leader answers LATE. So reads take [jsprovision.ReadBoundedWith]'s
// hedge: presumed lost after [jsprovision.ReadTerm], re-asked every
// [jsprovision.ReAsk] with earlier asks outstanding, the first ANSWER taken
// ("no responders" is none), none past [jsprovision.LookupBudget].
type Leader struct {
	// conn is what the reads are asked on: the client's connection, the one
	// call they make of it ([requester]), so a case inside a bubble can
	// answer them on the bubble's clock.
	conn   requester
	stream string
	// msgGet and info are the stream's MSG.GET and STREAM.INFO subjects in
	// the API the client speaks.
	msgGet string
	info   string
	// arrival is how long the reply to one of this reader's message reads
	// takes to reach it whole once the leader takes the request, where the
	// stream's messages may be large ([Leader.Carrying], [Arrival]) — or,
	// for a reader a writer waits on, the part of that a link costs
	// ([Leader.Deciding], [Crossing]) — and none for a reader of small ones:
	// what each request is given beyond the read term before it is presumed
	// lost, and the read beyond the lookup ceiling
	// ([jsprovision.Timing.Arriving]).
	arrival time.Duration
	// timeout bounds a read whose context has no deadline — the client's
	// OWN default, taken from the client, the bound the read had while it
	// was one request ([jsprovision.ReadBoundedWith]): a page's and the
	// stream's info's as it is, and a message read's beside the arrival of
	// its reply ([Leader.Carrying]).
	timeout time.Duration
	// paged is how long the last subjects page this reader asked took to
	// be answered — or, raised by a page nobody answered, at least how
	// long that one was asked for ([Leader.outlasted]) — which the next
	// page's term is given beside the read term ([Leader.Subjects]); shared
	// by every copy [Leader.Carrying] makes, since they ask the one leader.
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
		timeout: js.Options().DefaultTimeout, paged: new(atomic.Int64)}, nil
}

// Carrying is l for a stream whose messages may be as large as messageBytes,
// on a connection carrying a mebibyte in as long as pace across a link (none on
// a member's own): each read is given the time a reply that size takes to
// ARRIVE ([Arrival]) beyond the read term, the client's own bound where the
// caller set no deadline, and the lookup ceiling ([jsprovision.Timing.Arriving]).
// For reads of possibly large messages where no writer waits on the answer;
// a reader deciding a write uses [Leader.Deciding]. The client hears nothing
// until the last byte, so a 175 KB piece takes five seconds over a leaf link at
// the floor, and re-asks at two and four seconds put three pieces on the link
// carrying lease renewals. The bound and ceiling grow with the term, or a long
// reply is cut at five or thirty seconds every time (crewlet/crewlet#1149).
// In process a megabyte reply is still built and decoded ([Handling], #1153).
func (l *Leader) Carrying(messageBytes int, pace time.Duration) *Leader {
	c := *l
	c.arrival = Arrival(messageBytes, pace)
	return &c
}

// Deciding is [Leader.Carrying] for a reader whose reads DECIDE A WRITE its
// caller is waiting on (a sandbox run's record, internal/coord/kv's runs): each
// read is given its reply's [Crossing] beyond the read term, the client's own
// bound and the lookup ceiling, and no [Handling]; on a member's own connection
// it is read at the read term. No handling: the writer waits within a budget of
// its own (five to ten seconds), inside which a request dropped across an
// election is re-asked at two. Handling is 3.5 ms a MiB without the race
// detector ([HandlingPace]): a claim of a 6 MiB run took at most 949 ms in
// process at six busy goroutines a core, a fifth of the five-second bound
// (#2175). Its crossing all the same: across a link no byte is heard before the
// last, so less re-asks while the first reply is on the link, and one past the
// lookup ceiling is never taken (crewlet/crewlet#1149).
func (l *Leader) Deciding(messageBytes int, pace time.Duration) *Leader {
	c := *l
	c.arrival = Crossing(messageBytes, pace)
	return &c
}

// Arrival is how long the reply to a message read that carries a message of
// messageBytes ([ReplyBytes]) takes to reach its reader whole once the leader
// takes the request, heard only once its last byte is in: its [Handling] in
// the engine's own processes, on every connection, and its [Crossing] of a
// link carrying a mebibyte in as long as pace — none for a pace of zero, a
// member's own connection. The one figure such a reply is given beyond the
// read term and the lookup ceiling ([jsprovision.Timing.Arriving]), whoever
// reads ([Leader.Carrying], and a state log's read of a record by its
// position).
func Arrival(messageBytes int, pace time.Duration) time.Duration {
	return Handling(messageBytes) + Crossing(messageBytes, pace)
}

// Delivering is [Arrival]'s RAW twin: how long a consumer's delivery of
// messageBytes, as the broker counts them against a pull (the subject, the
// reply, the header block and the payload), takes to reach its reader whole —
// handled at [HandlingPace] in the engine's own processes and crossing a link
// carrying a mebibyte in as long as pace, none for a pace of zero. A delivery
// carries its messages as they were stored, never base64-encoded inside JSON
// as a leader's reply does ([ReplyBytes]), so Arrival(m, pace) is
// Delivering(ReplyBytes(m), pace) to within a nanosecond (Arrival rounds its
// two terms down apart, this its one). It is what a reader of a consumer's
// pulls gives a request whose answer queues behind a pull of that many bytes
// still on its way ([Pulls]).
func Delivering(messageBytes int, pace time.Duration) time.Duration {
	return time.Duration(float64(HandlingPace+pace) * float64(messageBytes) / mebibyte)
}

// Handling is how long the reply to a message read that carries a message of
// messageBytes ([ReplyBytes]) is given to be built by the stream's leader,
// carried in process and decoded by its reader, at [HandlingPace]: what every
// connection costs such a reply, a member's own included, beside whatever
// link it crosses ([Crossing]).
func Handling(messageBytes int) time.Duration {
	return time.Duration(float64(HandlingPace) * float64(ReplyBytes(messageBytes)) / mebibyte)
}

// HandlingPace is how long a mebibyte of a leader's reply is given to be built,
// carried in process and decoded: a SECOND, so the largest reply (a state log's
// record or a dead letter at the transport's ceiling, 10.7 MiB) gets 10.7 s.
// The reply is CPU, not a link: JSON with base64 inside, parsed whole by each
// server and decoded by the reader. BenchmarkADomainLogReadOfItsLargestRecord
// took 3.5 ms a mebibyte without the race detector and, under it with nine
// runnable threads a core (past the 8.25 a core top measured on this
// repository's runners, internal/config's MinLeaseTTL), half a second. A
// second is twice that. It is a TERM, never a deadline: it only delays the
// re-ask of a request presumed lost, keeping a leader from building one reply
// twice, and only for readers no writer waits on ([Leader.Carrying]); a
// [Leader.Deciding] reader is given none, or every write would pass its reason.
const HandlingPace = time.Second

// Crossing is how long the reply to a message read that carries a message of
// messageBytes ([ReplyBytes]) takes to cross a link carrying a mebibyte in as
// long as pace, heard only once its last byte is in — the link's part of a
// reply's [Arrival], whoever reads ([Leader.Carrying], and a state log's read
// of a record by its position). A write's request crosses the other way, raw
// ([Sending]).
func Crossing(messageBytes int, pace time.Duration) time.Duration {
	return time.Duration(float64(pace) * float64(ReplyBytes(messageBytes)) / mebibyte)
}

// ReplyBytes is the most the reply to a message read (a `STREAM.MSG.GET`)
// carries for a message of messageBytes, header block and payload: JSON with
// the message base64-encoded beside its envelope ([replyOverhead]), a third
// larger than the message.
//
// A reply past the transport cannot cross a leaf's link: the receiving server
// holds every message to its max_payload (eight mebibytes on every embedded
// server) and CLOSES THE WHOLE LINK on a larger one (nats-server v2.15.2,
// processLeafMsgArgs), cutting every lease and mailbox across it. A message
// over about six mebibytes has such a reply, so a reader that may meet one
// across a leaf link reads it raw (internal/queue/jetstream's copy reads). A
// route carries any size, and a client of another's broker reads from the leader.
func ReplyBytes(messageBytes int) int {
	return base64.StdEncoding.EncodedLen(messageBytes) + replyOverhead
}

// MaxReadable is the largest message, header block and payload, whose reply
// to a message read ([ReplyBytes]) is no larger than transport — what a
// leader's read can carry back across a leaf's link held to that max_payload:
// 6,290,688 bytes at the transport's eight mebibytes, and -1 where not even an
// empty message's reply fits. A store that holds its values to it
// ([ReplyBytes]'s doc) is one every read of which crosses any link.
func MaxReadable(transport int64) int {
	// ReplyBytes only grows with the message, so the largest that fits is
	// found by halving.
	lo, hi := 0, int(max(transport, 0))
	for lo < hi {
		mid := lo + (hi-lo+1)/2
		if int64(ReplyBytes(mid)) <= transport {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	if int64(ReplyBytes(lo)) > transport {
		return -1
	}
	return lo
}

// replyOverhead is the most a message read's reply carries beside the
// message's own data: the JSON envelope, the subject, the sequence, the time
// and the headers, base64-encoded — a kibibyte, which every reply the engine
// reads fits in with room to spare (a piece's headers are none; an object's
// metadata, its own data, is a few hundred bytes; a state log record's are the
// few its append carried).
const replyOverhead = 1 << 10

// Sending is how long the REQUEST of an acknowledged write carrying a message
// of messageBytes (header block and payload) takes to cross a link carrying a
// mebibyte in as long as pace: [Crossing]'s twin for the other direction, given
// beyond the request term for each of a write's first [WriteSends] sends
// ([WriteBound], [Budget]), since the stream acknowledges only once the last
// byte is in. RAW, where a read's reply is not: a publish is neither
// base64-encoded nor inside JSON, so it crosses in three quarters of a read's
// time, and a read's bound would hold a write on a dead link a third longer.
func Sending(messageBytes int, pace time.Duration) time.Duration {
	return time.Duration(float64(pace) * float64(RequestBytes(messageBytes)) / mebibyte)
}

// RequestBytes is what the request of a write carrying a message of
// messageBytes puts on a link: the message and its protocol line
// ([requestOverhead]). It is a put's take of its link's [Window].
func RequestBytes(messageBytes int) int { return messageBytes + requestOverhead }

// requestOverhead is the most a write's request carries beside its message:
// the protocol line ahead of it — the verb, the subject, the reply subject its
// acknowledgement comes back on and the message's two sizes — and the line
// ends. A kibibyte, as a reply's envelope is ([replyOverhead]): the longest
// subject the engine publishes on is a few hundred bytes, and a reply subject
// a few dozen.
const requestOverhead = 1 << 10

// mebibyte is the unit a pace is stated in.
const mebibyte = 1 << 20

// Term is how long one of l's message reads is given before it is presumed
// lost and asked again: the read term, and the arrival of a reply as large as
// l's messages may be ([Leader.Carrying]).
func (l *Leader) Term() time.Duration { return jsprovision.ReadTerm + l.arrival }

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

// Leading is the server that leads the stream, by its name, and how many
// replicas it keeps, as the LEADER states them: one `STREAM.INFO` naming no
// subjects (a few kibibytes, read at the read term), asked again while nobody
// answers as a message read is (see [Leader]). A reader that cannot ask the
// leader for a message (one too large for its reply to cross the link) reads
// from a consumer, placed on a random replica, and learns here which server
// holds the leader's copy.
//
// AN ANSWER THAT NAMES NO LEADER IS REFUSED ([ErrLeaderless]): while a group
// has none every member answers info from its own copy (nats-server v2.15.2,
// jsStreamInfoRequest). An unclustered server names itself.
func (l *Leader) Leading(ctx context.Context) (name string, replicas int, err error) {
	var resp server.JSApiStreamInfoResponse
	if _, _, err = page(ctx, l.conn, l.info, server.JSApiStreamInfoRequest{}, jsprovision.ReadTerm,
		l.timeout, &resp); err != nil {
		return "", 0, fmt.Errorf("jsapi: read who leads %s: %w", l.stream, err)
	}
	switch {
	case resp.Error != nil:
		return "", 0, fmt.Errorf("jsapi: read who leads %s: %w", l.stream, resp.Error)
	case resp.StreamInfo == nil:
		return "", 0, fmt.Errorf("jsapi: the leader of %s answered a read of who leads it with "+
			"neither a stream nor an error", l.stream)
	case resp.Cluster == nil || resp.Cluster.Leader == "":
		return "", 0, fmt.Errorf("jsapi: read who leads %s: %w", l.stream, ErrLeaderless)
	}
	return resp.Cluster.Leader, max(resp.Config.Replicas, 1), nil
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
	reply, err := ask(ctx, l.conn, l.msgGet, req, l.arrival, l.timeout+l.arrival)
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
