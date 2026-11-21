package kv

import (
	"context"
	"net"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

// countedServer hands nats.go in-process connections to ns and counts the
// bytes each one writes and reads: protocol lines, headers and payloads, what
// a link carries of them before any compression.
type countedServer struct {
	ns       *server.Server
	up, down atomic.Int64
}

func (c *countedServer) InProcessConn() (net.Conn, error) {
	conn, err := c.ns.InProcessConn()
	if err != nil {
		return nil, err
	}
	return countedConn{Conn: conn, of: c}, nil
}

// countedConn counts a write as it is made, before the server can answer it,
// and a read before the client parses it, so both are counted by the time the
// call that caused them returns.
type countedConn struct {
	net.Conn
	of *countedServer
}

func (c countedConn) Write(p []byte) (int, error) {
	c.of.up.Add(int64(len(p)))
	return c.Conn.Write(p)
}

func (c countedConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.of.down.Add(int64(n))
	return n, err
}

// Renewal's bytes on the wire, at most (design §2.3, which estimates 573 B up
// and 110 B down): the lease link's budget, seats renewed within a beat at the
// floor, is these divided into the beat's bytes.
const (
	renewalBytesUp   = 700
	renewalBytesDown = 200
)

// A SEAT RENEWAL IS AT MOST 700 B UP AND 200 B DOWN: one request on the
// record's known revision, its acknowledgement the only reply, so the lease
// link's arithmetic of seats per beat holds (ADR-0174).
//
// Mutant: the renewal reads the record before writing it — the leader read's
// reply, the record base64-encoded, is past the 200 B down.
func TestASeatRenewalIsSevenHundredBytesUpAndTwoHundredDown(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ns, read := embeddedServer(t)
	counted := &countedServer{ns: ns}
	hold, err := nats.Connect("", nats.InProcessServer(counted))
	if err != nil {
		t.Fatalf("connect the hold client: %v", err)
	}
	t.Cleanup(hold.Close)
	s, _ := twoClients(t, hold, func() (*nats.Conn, error) { return read, nil }, Config{TTL: knownTTL})
	// An owner of a production node's length: a node id and an instance.
	owner := "node-" + uuid.NewString() + ":1"
	lease := claimUngated(ctx, t, s, knownSeat(0), owner, knownTTL)
	// A first renewal, so whatever a connection sends once is behind it.
	if ok, err := s.Renew(ctx, knownSeat(0), owner, lease.Epoch, knownTTL); err != nil || !ok {
		t.Fatalf("renew = (%v, %v)", ok, err)
	}
	up, down := counted.up.Load(), counted.down.Load()
	if ok, err := s.Renew(ctx, knownSeat(0), owner, lease.Epoch, knownTTL); err != nil || !ok {
		t.Fatalf("renew = (%v, %v)", ok, err)
	}
	up, down = counted.up.Load()-up, counted.down.Load()-down
	t.Logf("a seat renewal: %d B up, %d B down", up, down)
	if up == 0 || up > renewalBytesUp {
		t.Errorf("a seat renewal sent %d B, want at most %d", up, renewalBytesUp)
	}
	if down == 0 || down > renewalBytesDown {
		t.Errorf("a seat renewal read %d B, want at most %d", down, renewalBytesDown)
	}
}
