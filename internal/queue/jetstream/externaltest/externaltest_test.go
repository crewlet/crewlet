package externaltest_test

import (
	"errors"
	"testing"
	"time"

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

// A RELOAD CHANGES THE RUNNING SERVER AND LEAVES ITS CLIENTS WHERE THEY ARE.
//
// What the suites that use it stage is an operator's LIVE reload, which is a
// different gesture from a restart in exactly the way that matters to them: a
// connected client is not reconnected, so it learns nothing the server does not
// send it — nats-server sends no INFO for a reloaded max_payload — while a new
// connection reads the reloaded figure. A Reload that restarted would hand a
// case a client that had reconnected and read the new limit, and the close for
// good the case exists to stage would never happen.
//
// Mutation: implement Reload as a stop and a start on the same port and the
// reconnect row goes red; build the reloaded set from scratch rather than from
// the running one and the reload is refused for changing what cannot be
// reloaded.
func TestAReloadChangesTheServerAndNotItsClients(t *testing.T) {
	t.Parallel()
	const before, after = 8 << 20, 2 << 20
	srv := externaltest.Start(t, before)
	nc, err := nats.Connect(srv.URL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)

	srv.Reload(after)

	if err := nc.FlushTimeout(5 * time.Second); err != nil {
		t.Fatalf("a round trip after the reload: %v", err)
	}
	if nc.Reconnects != 0 {
		t.Errorf("the client reconnected %d time(s) across a reload: it was a "+
			"restart, and the client has read the new limit", nc.Reconnects)
	}
	if got := nc.MaxPayload(); got != before {
		t.Errorf("the connected client reads %d after a reload to %d, want the "+
			"%d it was told when it connected", got, after, before)
	}
	fresh, err := nats.Connect(srv.URL())
	if err != nil {
		t.Fatalf("connect after the reload: %v", err)
	}
	t.Cleanup(fresh.Close)
	if got := fresh.MaxPayload(); got != after {
		t.Errorf("a connection opened after the reload reads %d, want the "+
			"reloaded %d", got, after)
	}
}

// A RESTART AS ANOTHER CONFIGURATION IS THAT CONFIGURATION, ON THE SAME ADDRESS.
//
// What the suites that use it stage is a server that stopped admitting a
// running client — its credentials rotated — so the restarted server has to be
// at the address the client is reconnecting to, and has to be the new
// configuration rather than the one Start was given, or the client is simply
// admitted again and the case stages nothing.
//
// Mutation: restart from Start's configuration and the old credential is
// admitted; restart on a fresh port and the new one finds nobody.
func TestARestartAsAnotherConfigurationIsThatConfiguration(t *testing.T) {
	t.Parallel()
	srv := externaltest.Start(t, 8<<20, func(o *server.Options) { o.Authorization = "before" })
	url := srv.URL()

	srv.RestartAs(func(o *server.Options) { o.Authorization = "after" })

	if srv.URL() != url {
		t.Fatalf("the restarted server is at %s, not %s where its clients reconnect",
			srv.URL(), url)
	}
	if nc, err := nats.Connect(url, nats.Token("before")); err == nil {
		nc.Close()
		t.Error("the restarted server admits the credential it was rotated away from")
	}
	nc, err := nats.Connect(url, nats.Token("after"))
	if err != nil {
		t.Fatalf("the restarted server refuses the credential it was configured with: %v", err)
	}
	nc.Close()
}
