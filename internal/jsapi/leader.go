package jsapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
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
type Leader struct {
	conn    *nats.Conn
	subject string
	stream  string

	// timeout bounds a read whose context has no deadline — the client's
	// OWN default, taken from the client, because this read goes past the
	// client and would otherwise be the one request that waited for ever
	// on a leader that never answers.
	timeout time.Duration
}

// NewLeader addresses the leader reads of stream in the API js speaks.
func NewLeader(js jetstream.JetStream, stream string) (*Leader, error) {
	api, err := Of(js)
	if err != nil {
		return nil, err
	}
	subject, err := api.Subject(fmt.Sprintf(server.JSApiMsgGetT, stream))
	if err != nil {
		return nil, err
	}
	return &Leader{conn: js.Conn(), subject: subject, stream: stream,
		timeout: js.Options().DefaultTimeout}, nil
}

// Last answers the newest message on subject, or [ErrNoMessage].
func (l *Leader) Last(ctx context.Context, subject string) (Message, error) {
	msg, err := l.get(ctx, server.JSApiMsgGetRequest{LastFor: subject})
	if err != nil {
		return Message{}, err
	}
	if msg.Subject != subject {
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

func (l *Leader) get(ctx context.Context, ask server.JSApiMsgGetRequest) (Message, error) {
	if _, bounded := ctx.Deadline(); !bounded {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, l.timeout)
		defer cancel()
	}
	req, err := json.Marshal(ask)
	if err != nil {
		return Message{}, fmt.Errorf("jsapi: encode the read: %w", err)
	}
	reply, err := l.conn.RequestWithContext(ctx, l.subject, req)
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
