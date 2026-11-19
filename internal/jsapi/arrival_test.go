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
// No link is crossed, but an eleven megabyte reply is still built by the leader
// and decoded in this process, seconds under load. At the bare read term the
// read was re-asked at two and four seconds, each another whole reply, none
// answered (crewlet/crewlet#1153). Carrying the ceiling, a leader answering
// after a term, a re-ask and half a term more is asked once; the control twice.
// EXACT, IN A BUBBLE: the connection answers on the bubble's clock through the
// reader's own path ([Leader.At], its get and hedge), never a bet on a runner.
// Mutation: give a carrying read no handling, ask without its arrival, or run
// the ask at the bare read term, and the carrying read is asked twice.
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
