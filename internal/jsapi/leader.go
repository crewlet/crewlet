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
// ([jsprovision.LookupBudget]) — each of the three beyond the time a reply
// as large as the stream's messages may be takes to arrive, for a reader of
// large ones ([Leader.Carrying]), or to cross a link, for one a writer waits
// on ([Leader.Deciding]). An ANSWER ends it whatever it says — a
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
// on a connection that may carry a mebibyte in as long as pace across a link —
// none on a member's own connection, which crosses nothing: every message read
// is given the time a reply of that size takes to ARRIVE ([Arrival]) — its
// handling on every connection, and its crossing where there is a link —
// beyond the read term before it is presumed lost, beyond the client's own
// bound where its caller set no deadline, and beyond the lookup ceiling every
// read is held to ([jsprovision.Timing.Arriving]). The ONE rule for a leader
// read of a message that may be large, on every connection, where no writer
// waits on what the read answers: a reader of dead letters
// (internal/queue/jetstream) and of an object's pieces across a link
// (internal/objstore/natsobj) each read through it, sized at the largest
// message the stream may hold, since a reader cannot know how large the
// message it asks for is until its reply is in. A reader whose reads decide a
// write its caller is waiting on is given the reply's crossing alone
// ([Leader.Deciding]).
//
// # Why the bound grows with the term
//
// The bound a deadline-less caller is held to is what its one request was
// given to be answered in ([jsprovision.ReadBoundedWith]): the broker's
// answer, with no arrival in it. Held there, a read whose term said its
// reply took longer than five seconds to cross was cut at five every time,
// whatever its term: a dead letter's frame read across a link by a feed's
// replay, which sets no deadline, failed at every start for an entry whose
// reply took that long — about six megabytes below about 13 Mbit/s — and was
// left until it expired, its wake lost.
//
// # Why the ceiling grows with it too
//
// The ceiling ([jsprovision.LookupBudget]) is the broker's longest silence,
// and a reply on its way is no silence. Held to the ceiling alone, a read
// whose reply took longer than thirty seconds to arrive failed at every ask,
// whatever its term: a value of more than about 768 KiB, read across a link
// at the object store's floor, could not be read at all, and every
// read-decide-write of one — a run's record of six megabytes, rewritten by
// every change of it — failed on its read, whatever its write was given
// (crewlet/crewlet#1149).
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
//
// AND ON A MEMBER'S OWN CONNECTION, which crosses nothing, a reply of
// megabytes is still built by the leader and decoded by its reader, in the
// engine's own process ([Handling]): a dead letter at the transport's
// ceiling, its reply eleven megabytes, read at the bare read term under
// load, was asked again at two seconds and at four while the leader was
// still building the first — every re-ask another whole reply built beside
// it, more of the work that made the first one late — and ran out its bound
// with none of them answered (crewlet/crewlet#1153).
func (l *Leader) Carrying(messageBytes int, pace time.Duration) *Leader {
	c := *l
	c.arrival = Arrival(messageBytes, pace)
	return &c
}

// Deciding is [Leader.Carrying] for a reader whose reads DECIDE A WRITE its
// caller is waiting on — the read-decide-write of a value that may be as large
// as messageBytes, a sandbox run's record (internal/coord/kv's runs) — on a
// connection that may carry a mebibyte in as long as pace across a link: every
// message read is given the time its reply takes to CROSS that link
// ([Crossing]) beyond the read term, beyond the client's own bound where its
// caller set no deadline, and beyond the lookup ceiling, and none of the
// reply's [Handling]. So on a member's own connection, which crosses nothing,
// it is read at the read term, as a reader of small messages is.
//
// # Why not the reply's handling
//
// The writer waits on the read within a budget of its own reason — a resumed
// turn's record of what it carries five seconds, an attempt at settling a
// take ten — and a request a member dropped across an election has to be
// asked again inside that budget, at two seconds, where the new leader answers
// it. Given the handling of a reply at a run's bound — eight seconds — the
// read would ask again only after such a budget had ended, and the change
// would be refused for a request the new leader would have answered in two.
// What the handling buys is a reply built once while the race detector makes
// building it slow on a loaded runner ([HandlingPace]); a node not running
// under it builds and decodes such a reply in tens of milliseconds, which the
// read term covers many times over, so the read term costs a writer's read
// nothing a node pays, and the two seconds it keeps are what the hedge is for.
// A domain log's read of a subject's newest record keeps the read term for the
// same reason (internal/queue/jetstream's DomainLog.LastSeq).
//
// # Why its crossing all the same
//
// Across a link no byte of the reply is heard before its last is in, so a read
// given less than its crossing is asked again while its first reply is still
// on the link, every re-ask another whole reply ahead of the node's lease
// renewals, and one whose crossing outlasts the lookup ceiling is never taken
// at all: a run's record of six megabytes takes four minutes at the slowest
// pace a link is allowed (crewlet/crewlet#1149). A writer that bounds such a
// change counts that crossing in its budget (internal/coord/kv's
// Reach.valueCrossing), so the wait it adds is one the writer already gives.
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

// Handling is how long the reply to a message read that carries a message of
// messageBytes ([ReplyBytes]) is given to be built by the stream's leader,
// carried in process and decoded by its reader, at [HandlingPace]: what every
// connection costs such a reply, a member's own included, beside whatever
// link it crosses ([Crossing]).
func Handling(messageBytes int) time.Duration {
	return time.Duration(float64(HandlingPace) * float64(ReplyBytes(messageBytes)) / mebibyte)
}

// HandlingPace is how long a mebibyte of a leader's reply is given to be
// built, carried in process and decoded: a SECOND — so the largest reply a
// leader's read carries, a state log's record or a dead letter at the
// transport's ceiling (eleven megabytes, 10.7 MiB), is given 10.7 seconds.
//
// # Why a second
//
// The reply is CPU, not a link: the leader reads the message and encodes it
// into JSON, base64 inside, each server it passes parses it whole, and the
// reader decodes it. Read back on a member's own connection
// (BenchmarkADomainLogReadOfItsLargestRecord, -benchtime 10x, two E-cores of
// a Core Ultra 7 265U), the largest reply took 36.9 ms without the race
// detector — 3.5 ms a mebibyte — and 0.79 s under it, and with two, four and
// eight threads more kept busy beside it on those two cores 1.26, 2.24 and
// 5.77 seconds: a load of five runnable threads a core under the race
// detector, inside the ten to thirty-three on four cores this repository's
// runners were measured at (internal/config's MinLeaseTTL), is half a second
// a mebibyte. A second a mebibyte is twice that, and nearly three hundred
// times what the reply costs a node that is not running under the race
// detector.
//
// It is a TERM, never a deadline: a reply that is answered in milliseconds
// ends the read as soon as it is in. What the second a mebibyte costs is the
// wait before a request presumed lost is asked again — only for a reader of
// messages that may be that large whose reads no writer waits on
// ([Leader.Carrying]), and a request a member dropped across an election is
// asked again eleven seconds later for a dead letter at the transport's
// ceiling rather than two — which is what keeps a leader from building one
// reply twice while the first is still being built. A reader a writer waits
// on is given none of it ([Leader.Deciding]): the race detector's figure would
// hold every such write past its own reason.
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

// ReplyBytes is the most the reply to a message read — a `STREAM.MSG.GET`,
// this package's or the client's own where a stream allows no direct get —
// carries for a message of messageBytes, its header block and its payload: the
// reply is JSON, the message base64-encoded inside it beside its envelope
// ([replyOverhead]), so it is a third larger than the message.
//
// # A reply past the transport cannot cross a leaf's link
//
// The server on the receiving side of a leaf link holds every message the
// link carries to its own max_payload — the transport's eight mebibytes on
// every embedded server — and CLOSES THE WHOLE LINK on one larger (nats-server
// v2.15.2, leafnode.go, processLeafMsgArgs → maxPayloadViolation), every lease
// and mailbox the node holds across it cut off until it is dialled again. A
// message of more than about six mebibytes, which the transport carries
// whole, has a reply larger than that: a reader that may meet one across a
// leaf's link reads it raw instead (internal/queue/jetstream's copy reads),
// and one whose messages are held below it reads from the leader. A route
// carries any size, and nothing checks what a server sends its own client, so
// a client of a broker somebody else runs reads from the leader whatever the
// message's size.
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

// Sending is how long the REQUEST of an acknowledged write — a publish that
// carries a message of messageBytes, its header block and its payload — takes
// to cross a link carrying a mebibyte in as long as pace: [Crossing]'s twin
// for the other direction, and what a write is given beyond the request term
// for each of its first [WriteSends] sends ([WriteBound], [Budget]), since the
// stream acknowledges a message only once its last byte is in.
//
// RAW, where a read's reply is not: a publish carries its message as it is,
// behind the protocol line that names it ([requestOverhead]) — never
// base64-encoded and never inside JSON — so a write crosses in three quarters
// of the time the reply to a read of the same message does, and a bound
// counted as a read's would hold a write on a dead link a third longer than
// its bytes need.
func Sending(messageBytes int, pace time.Duration) time.Duration {
	request := messageBytes + requestOverhead
	return time.Duration(float64(pace) * float64(request) / mebibyte)
}

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
// replicas the stream keeps, as the stream's LEADER states them: one
// `STREAM.INFO` that names no subjects, so its answer is the stream's state
// and configuration and none of its messages — a few kibibytes, read at the
// read term — asked again while nobody answers it, as a message read is (see
// [Leader]).
//
// A reader that cannot ask the leader for a message — one too large for the
// leader's reply to cross its link (internal/queue/jetstream's copy reads) —
// reads it from a consumer instead, which the broker places on one of the
// stream's replicas at random; only the one on the leader's server reads the
// leader's own copy, and this is how that reader knows which server that is.
//
// AN ANSWER THAT NAMES NO LEADER IS REFUSED ([ErrLeaderless]): a stream's
// info is the leader's except while its group has none, when every member
// answers from its own copy (nats-server v2.15.2, server/jetstream_api.go,
// jsStreamInfoRequest), naming no leader to read from. A stream on a server
// that is not clustered names that server, as a stream of one replica does.
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
