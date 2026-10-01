package externaltest_test

import (
	"errors"
	"testing"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/queue/jetstream/externaltest"
)

// A RESTART CHANGES THE LIMIT AND NOTHING ELSE A RUNNING CLIENT DEPENDS ON.
//
// What the suites that use it stage is a server that CHANGED under a running
// node, so the restart has to hand the same client back on the same address
// with the same streams — only the max_payload moved — and return only once
// the client reads the new limit, since that is what the client refuses a
// message against. A restart on a fresh store would drop every stream the
// node provisioned at boot, and one that returned when the server was up
// would hand a case a client still holding the old limit.
//
// Mutation: start the restarted server on a new store and the stream row goes
// red; return from Restart without waiting for the client and the limit row
// does; read an unset limit as zero rather than nats-server's default and the
// last one does.
func TestARestartChangesTheLimitAndKeepsTheStreams(t *testing.T) {
	t.Parallel()
	const before, after = 8 << 20, 2 << 20
	srv := externaltest.Start(t, before)
	nc, err := nats.Connect(srv.URL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)
	if got := nc.MaxPayload(); got != before {
		t.Fatalf("the server announces a max_payload of %d, want %d", got, before)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if _, err := js.CreateStream(t.Context(), jetstream.StreamConfig{
		Name: "KEPT", Subjects: []string{"kept.>"}, Storage: jetstream.FileStorage,
	}); err != nil {
		t.Fatalf("create a stream: %v", err)
	}

	srv.Restart(after, nc)
	if got := nc.MaxPayload(); got != after {
		t.Errorf("after a restart announcing %d the client reads %d", after, got)
	}
	if _, err := js.Stream(t.Context(), "KEPT"); err != nil {
		if errors.Is(err, jetstream.ErrStreamNotFound) {
			t.Errorf("a stream provisioned before the restart is gone after it")
		} else {
			t.Errorf("look the stream up after the restart: %v", err)
		}
	}

	srv.Restart(0, nc)
	if got := nc.MaxPayload(); got != server.MAX_PAYLOAD_SIZE {
		t.Errorf("a restart with no limit of its own reads %d, want nats-server's "+
			"default %d", got, server.MAX_PAYLOAD_SIZE)
	}
}
