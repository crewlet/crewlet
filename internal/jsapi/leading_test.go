package jsapi_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/crewlet/crewlet/internal/jsapi"
)

// A STREAM'S LEADER IS NAMED BY ITS LEADER, AND NEVER BY A MEMBER'S OWN COPY.
//
// A reader that cannot ask the leader for a message reads it through a
// consumer, and keeps only one the broker placed on the leader's server
// (internal/queue/jetstream's copy reads): the leader's name is what tells it
// which copy is the leader's. A stream's info is the leader's except while its
// group has none, when a member answers from its own copy and names no leader
// — refused, as is an answer naming no cluster at all. And the read names no
// subjects, so its answer carries none of the stream's subjects: it is the
// size of a stream's info, whatever the stream holds.
//
// Mutation: take the answer of a group with no leader, and the case reads a
// leader named by nobody.
func TestAStreamsLeaderIsNamedByItsLeaderAndNeverByAMembersCopy(t *testing.T) {
	t.Parallel()
	subject := fmt.Sprintf(server.JSApiStreamInfoT, "OBJ")
	led := func(cluster *server.ClusterInfo) func(int, []byte) any {
		return func(int, []byte) any {
			return server.JSApiStreamInfoResponse{
				ApiResponse: server.ApiResponse{Type: server.JSApiStreamInfoResponseType},
				StreamInfo: &server.StreamInfo{
					Config:  server.StreamConfig{Name: "OBJ", Storage: server.FileStorage, Replicas: 3},
					Cluster: cluster,
				},
			}
		}
	}

	f := fakeBroker(t, subject, led(&server.ClusterInfo{Leader: "member-b"}))
	name, replicas, err := mustLeader(t, f.js).Leading(t.Context())
	if err != nil || name != "member-b" || replicas != 3 {
		t.Fatalf("the stream's leader = (%q, %d, %v), want member-b keeping 3 replicas", name, replicas, err)
	}
	f.mu.Lock()
	asked := f.asked
	f.mu.Unlock()
	for i, raw := range asked {
		var req server.JSApiStreamInfoRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			t.Fatalf("decode request %d: %v", i, err)
		}
		if req.SubjectsFilter != "" || req.DeletedDetails {
			t.Errorf("the read of the stream's leader asked for subjects %q and deleted details %t: "+
				"its answer would carry the stream's subjects or its removals", req.SubjectsFilter,
				req.DeletedDetails)
		}
	}

	for name, cluster := range map[string]*server.ClusterInfo{
		"a group with no leader": {Leader: ""},
		"no cluster at all":      nil,
	} {
		leaderless := fakeBroker(t, subject, led(cluster))
		if _, _, err := mustLeader(t, leaderless.js).Leading(t.Context()); !errors.Is(err, jsapi.ErrLeaderless) {
			t.Errorf("a stream's info naming %s = %v, want jsapi.ErrLeaderless", name, err)
		}
	}

	refused := fakeBroker(t, subject, func(int, []byte) any {
		return server.JSApiStreamInfoResponse{ApiResponse: server.ApiResponse{
			Type: server.JSApiStreamInfoResponseType, Error: server.NewJSStreamNotFoundError()}}
	})
	if _, _, err := mustLeader(t, refused.js).Leading(t.Context()); err == nil {
		t.Error("a stream's info the leader refused named a leader")
	}
}

// A STREAM ON A SERVER THAT IS NOT CLUSTERED IS LED BY THAT SERVER, keeping one
// replica — read against the real broker, whose stream info names the server
// that answered: a reader placing a consumer there finds it on the leader at
// once.
//
// Mutation: refuse every answer as leaderless, or take the replicas from
// anything but the stream's configuration, and the solo server's stream is
// led by nobody or keeps another count; and the broker's own — a server that
// named no leader for a stream it holds alone would fail it here, before a
// copy read placed every consumer off a leader nobody named.
func TestAStreamOnAServerNotClusteredIsLedByThatServer(t *testing.T) {
	t.Parallel()
	srv, js := leaderServerFor(t)
	leader, err := jsapi.NewLeader(js, "LOG")
	if err != nil {
		t.Fatal(err)
	}
	name, replicas, err := leader.Leading(t.Context())
	if err != nil || name != srv.Name() || replicas != 1 {
		t.Fatalf("a solo server's stream is led by (%q, %d, %v), want %q keeping one replica",
			name, replicas, err, srv.Name())
	}
}

// A MESSAGE READ'S REPLY IS NEVER LARGER THAN REPLYBYTES SAYS.
//
// Whether a leader's read of a message can cross a leaf's link at all is
// decided by its reply's size, which the server sends as JSON with the
// message's header block and payload base64-encoded inside it beside an
// envelope: a reply larger than [jsapi.ReplyBytes] promised would close the
// link of a reader that took it to fit. Measured against the real broker, for
// messages with and without headers, the largest the case's broker carries
// among them.
//
// Mutation: count the payload alone, or leave the envelope out, and a reply
// is larger than the count.
func TestAMessageReadsReplyIsNeverLargerThanReplyBytes(t *testing.T) {
	t.Parallel()
	srv, js := leaderServerFor(t)
	nc, err := nats.Connect("", nats.InProcessServer(srv))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)
	for _, c := range []struct {
		name   string
		header nats.Header
		size   int
	}{
		{"an empty message", nil, 0},
		{"a small message", nil, 17},
		{"a message with headers", nats.Header{"Nats-Msg-Id": {"0123456789abcdef"},
			"Crewlet-Dead-Letter": {`{"v":1,"stream":"CREWLET_LOG","seq":42}`}}, 4 << 10},
		{"a message near the broker's limit", nats.Header{"Nats-Msg-Id": {"x"}}, 700 << 10},
	} {
		msg := &nats.Msg{Subject: "log.reply", Header: c.header, Data: make([]byte, c.size)}
		ack, err := js.PublishMsg(t.Context(), msg)
		if err != nil {
			t.Fatalf("%s: publish: %v", c.name, err)
		}
		reply, err := nc.RequestWithContext(t.Context(), fmt.Sprintf(server.JSApiMsgGetT, "LOG"),
			fmt.Appendf(nil, `{"seq":%d}`, ack.Sequence))
		if err != nil {
			t.Fatalf("%s: read it from the leader: %v", c.name, err)
		}
		carried := msg.Size() - len(msg.Subject) - len(msg.Reply)
		if limit := jsapi.ReplyBytes(carried); len(reply.Data) > limit {
			t.Errorf("%s of %d bytes came back in a reply of %d bytes, past the %d ReplyBytes "+
				"promised", c.name, carried, len(reply.Data), limit)
		}
		// AND IT IS THE BASE64 OF THE MESSAGE AT LEAST, so the rule is not
		// one any reply passes.
		if floor := base64.StdEncoding.EncodedLen(c.size); len(reply.Data) < floor {
			t.Errorf("%s came back in %d bytes, under its payload's base64 %d: the case read "+
				"something other than the reply", c.name, len(reply.Data), floor)
		}
	}
	// THE CROSSING TIMES THE SAME COUNT, so a read is given its reply's
	// bytes, never its message's: at a nanosecond a byte, a mebibyte's
	// reply crosses in as many nanoseconds as ReplyBytes counts.
	if got, want := jsapi.Crossing(1<<20, 1<<20), time.Duration(jsapi.ReplyBytes(1<<20)); got != want {
		t.Errorf("a mebibyte's reply at a nanosecond a byte crosses in %v, want %v", got, want)
	}
}

// THE LARGEST MESSAGE A LEADER'S READ CARRIES IS THE LARGEST WHOSE REPLY FITS.
//
// A store that holds its values to what its leader's read carries back across
// a link holds them to MaxReadable of the link's max_payload: a byte more and
// the reply closes a leaf's link, a byte less and a value the read could carry
// is refused. At the transport's eight mebibytes that is 6,290,688 bytes.
//
// Mutation: answer a byte more, or a byte less, and a row goes red.
func TestTheLargestMessageALeadersReadCarriesIsTheLargestWhoseReplyFits(t *testing.T) {
	t.Parallel()
	if got := jsapi.MaxReadable(8 << 20); got != 6290688 {
		t.Errorf("the largest message read across a link held to eight mebibytes = %d, want 6290688", got)
	}
	for _, transport := range []int64{8 << 20, 1 << 20, 64 << 10, 4096, 1027, 1024} {
		got := jsapi.MaxReadable(transport)
		if got < 0 || int64(jsapi.ReplyBytes(got)) > transport {
			t.Errorf("MaxReadable(%d) = %d, whose reply of %d bytes does not fit", transport, got,
				jsapi.ReplyBytes(got))
			continue
		}
		if int64(jsapi.ReplyBytes(got+1)) <= transport {
			t.Errorf("MaxReadable(%d) = %d, but a message a byte larger is replied to in %d bytes, "+
				"which fits", transport, got, jsapi.ReplyBytes(got+1))
		}
	}
	if got := jsapi.MaxReadable(1023); got != -1 {
		t.Errorf("MaxReadable(1023) = %d, want -1: not even an empty message's reply fits", got)
	}
}
