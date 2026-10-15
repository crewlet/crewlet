package engine

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/jetstream/externaltest"
)

// A BOOT THAT FAILS ON A BROKER CONNECTION NATS CLOSED FOR GOOD SAYS WHY.
//
// The connection is closed under a boot the way an operator closes it under a
// running node — the server restarted with its credentials rotated, refusing
// the node's on the reconnect and again on the attempt after it — and the boot
// then reaches its first step that needs the broker. That step fails in its own
// words, which say the connection is closed and nothing an operator can act
// on; the sentence that names the cause and the setting to change was recorded
// on the queue, and a failed boot returned without it. It must carry both: the
// classified sentence, because it is what to fix, and the step's own error,
// because it is where the boot stopped.
//
// Borrowed backends, so the boot is the engine's own and nothing else in it
// differs from `crewlet run`'s: the engine opened them in every other respect
// exactly as OpenBackends does.
//
// Mutation: drop lostDuring from New's failure path and the classified half
// goes red.
func TestABootThatLosesItsBrokerConnectionSaysWhy(t *testing.T) {
	t.Parallel()
	const token = "the-one-this-node-holds"
	srv := externaltest.Start(t, queue.MaxPayloadBytes, func(o *server.Options) {
		o.Authorization = token
	})
	b := testBootstrap(t)
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	b.Stream.Type = config.StreamNATS
	b.Stream.URL = srv.URL()
	b.Stream.Token = token
	b.Coordination.Type = config.CoordinationEmbeddedKV
	back, err := OpenBackends(t.Context(), &b, nil)
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { back.Close(context.WithoutCancel(t.Context())) })
	lost, ok := back.Queue.(brokerLoss)
	if !ok {
		t.Fatalf("the stream is %T, which has no connection to lose", back.Queue)
	}

	// THE OPERATOR'S GESTURE, and a wait for the loss it causes: the case is
	// about a boot that meets a recorded loss, so it starts once there is one.
	srv.RestartAs(func(o *server.Options) { o.Authorization = "rotated" })
	select {
	case <-lost.Lost():
	case <-time.After(30 * time.Second):
		t.Fatal("the server refused the node's credentials twice and the " +
			"queue recorded no loss: the case staged nothing")
	}

	_, err = New(t.Context(), Options{Bootstrap: &b, Backends: back})
	if err == nil {
		t.Fatal("a boot over a broker connection closed for good succeeded")
	}
	for _, want := range []string{"closed this node's connection for good",
		"Authorization Violation", "stream.token", srv.HostPort()} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the boot failed with %q, which does not say %q: the step's "+
				"own error reached the operator and the cause did not", err, want)
		}
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("the boot error %q carries the token the node signs in with", err)
	}
	// AND THE STEP'S OWN ERROR beside it, still in the chain for a caller
	// that asks: the cause says what to change, the step where the boot was.
	if !errors.Is(err, nats.ErrConnectionClosed) {
		t.Errorf("the boot error %q no longer carries the failing step's own "+
			"error (nats: connection closed)", err)
	}
}
