package jsapi

import (
	"context"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// answering is a client that answers the requests put on it itself, as a
// test's fake does ([Meta]'s seam). It answers none here: the case reads only
// where a listing would put them.
type answering struct {
	jetstream.JetStream
}

func (*answering) RequestWithContext(context.Context, string, []byte) (*nats.Msg, error) {
	return nil, nats.ErrNoResponders
}

// THE ENGINE'S CLIENTS ARE LISTED THROUGH THEIR CONNECTIONS, and only a
// client that answers requests itself is listed through the client.
//
// A Meta puts its pages on the client it is built over where the client
// answers requests itself (a test's fake, in a synctest bubble), else on the
// client's connection. Every client [API.Client] builds is the library's and
// answers none: a release giving it a RequestWithContext would turn every
// listing onto that method unannounced, and this case shows it.
//
// Mutation: put the pages on the connection whatever the client, and a fake's
// never reach the fake; anywhere but the connection, and the engine's never
// reach the broker.
func TestTheEnginesClientsAreListedThroughTheirConnections(t *testing.T) {
	t.Parallel()
	nc := new(nats.Conn)
	for _, api := range []API{Embedded(), Account()} {
		js, err := api.Client(nc)
		if err != nil {
			t.Fatalf("the %v client: %v", api, err)
		}
		m, err := NewMeta(js)
		if err != nil {
			t.Fatalf("a listing over the %v client: %v", api, err)
		}
		if m.conn != requester(nc) {
			t.Errorf("a listing over the %v client puts its pages on %T %p, want the client's "+
				"own connection, %p", api, m.conn, m.conn, nc)
		}

		fake := &answering{JetStream: js}
		if m, err = NewMeta(fake); err != nil {
			t.Fatalf("a listing over a fake of the %v client: %v", api, err)
		}
		if m.conn != requester(fake) {
			t.Errorf("a listing over a fake of the %v client that answers requests itself puts "+
				"its pages on %T, want the fake", api, m.conn)
		}
	}
}
