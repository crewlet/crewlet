package jsapi

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/crewlet/crewlet/internal/jsprovision"
)

// answeredBy is a connection whose every request is answered by answer, a
// case's, counting how many it was asked: the one call a leader's read makes
// of its connection ([requester]), answered inside the case's bubble.
type answeredBy struct {
	asked  atomic.Int32
	answer func(ctx context.Context) (*nats.Msg, error)
}

func (c *answeredBy) RequestWithContext(ctx context.Context, _ string, _ []byte) (*nats.Msg, error) {
	c.asked.Add(1)
	return c.answer(ctx)
}

// A READ OF A MESSAGE AS LARGE AS THE TRANSPORT, ON A MEMBER'S OWN CONNECTION,
// IS ASKED ONCE WHILE ITS REPLY IS STILL BEING BUILT.
//
// A member's own connection crosses no link, but a reply of eleven megabytes —
// a dead letter at the transport's ceiling, base64-encoded in JSON — is still
// built by the stream's leader and decoded by its reader, in the engine's own
// process, which under load takes seconds. Read at the bare read term, such a
// read was asked again at two seconds and at four while the leader was still
// building the first reply, each re-ask another whole reply built beside it,
// and ran out its bound with none of them answered (crewlet/crewlet#1153).
// Carrying the transport's ceiling at no pace, the read is given that reply's
// handling: here a leader that answers after a term, a re-ask and half a term
// more is asked once, and answered; the plain read, the control, is asked
// again at two seconds, exactly twice.
//
// EXACT, IN A BUBBLE: the reads are put on a connection that answers them on
// the bubble's clock, through the reader's own path — [Leader.At], its get and
// the hedge its ask runs — so the instants the term, the re-ask and the reply
// fall at are arithmetic, never a bet on how late a loaded runner wakes.
//
// Mutation: give a carrying read no handling, have the reader ask without its
// arrival, or the ask run at the bare read term, and the carrying read is
// asked twice.
func TestAReadOfAMessageAsLargeAsTheTransportOnAMembersOwnConnectionIsAskedOnce(t *testing.T) {
	t.Parallel()
	const transport = 8 << 20
	slow := jsprovision.ReadTerm + jsprovision.ReAsk + jsprovision.ReadTerm/2
	if handling := Handling(transport); handling <= slow {
		t.Fatalf("a reply of the transport's ceiling is given %v to be handled, which a leader "+
			"answering after %v does not fall inside", handling, slow)
	}
	reply, err := json.Marshal(server.JSApiMsgGetResponse{
		ApiResponse: server.ApiResponse{Type: server.JSApiMsgGetResponseType},
		Message:     &server.StoredMsg{Subject: "obj.c.x", Sequence: 1, Data: []byte("letter")},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		carrying bool
		asked    int32
	}{
		{"carrying the transport's ceiling", true, 1},
		{"control", false, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				conn := &answeredBy{answer: func(ctx context.Context) (*nats.Msg, error) {
					built := time.NewTimer(slow)
					defer built.Stop()
					select {
					case <-ctx.Done():
						return nil, ctx.Err()
					case <-built.C:
						return &nats.Msg{Data: reply}, nil
					}
				}}
				leader := &Leader{conn: conn, stream: "OBJ", msgGet: "$JS.API.STREAM.MSG.GET.OBJ",
					timeout: 5 * time.Second, paged: new(atomic.Int64)}
				if tc.carrying {
					leader = leader.Carrying(transport, 0)
				}
				msg, err := leader.At(t.Context(), 1)
				if err != nil || string(msg.Data) != "letter" {
					t.Fatalf("At = %q, %v, want the letter", msg.Data, err)
				}
				if got := conn.asked.Load(); got != tc.asked {
					t.Errorf("the read was asked %d times, want %d", got, tc.asked)
				}
			})
		})
	}
}
