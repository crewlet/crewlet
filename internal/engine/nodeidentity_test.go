package engine_test

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// brokerServerName asks the broker what it calls itself, over the connection
// the queue is actually using. Read from the server's INFO rather than from
// the config that produced it, so the assertion cannot pass by restating its
// own input.
func brokerServerName(t *testing.T, back *engine.Backends) string {
	t.Helper()
	q, ok := back.Queue.(*jetstream.Queue)
	if !ok {
		t.Fatalf("the stream is %T, not the JetStream backend", back.Queue)
	}
	conn := q.Conn()
	if conn == nil {
		t.Fatal("the queue has no connection to ask")
	}
	return conn.ConnectedServerName()
}

// TestTheBrokerTakesTheRESOLVEDNodeID pins the identity the embedded broker is
// started under, for the config shape a container orchestrator uses.
//
// A node's name comes from `node.id`, then CREWLET_NODE_ID, then the default,
// and only the first is a struct field; the fleet guide injects the variable
// and leaves the key out, so the field is EMPTY. JetStream places replicas BY
// SERVER NAME, so the broker takes the resolved id: the raw field fails a
// clustered member at boot and silently gives a solo member the broker's own
// default. Asserted through the solo path, the one that fails QUIETLY.
func TestTheBrokerTakesTheRESOLVEDNodeID(t *testing.T) {
	// Not parallel: it sets the environment variable whose precedence is
	// the subject.
	t.Setenv(config.NodeIDEnvVar, "node-from-the-orchestrator")

	b := config.DefaultBootstrap()
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	brokertest.StoreIn(&b.Stream, filepath.Join(brokertest.TempDir(t), "stream"))
	// Deliberately unset, which is the whole point: the id lives only in
	// the environment.
	b.Node.ID = ""

	engine.SeedStore(t, &b)
	back, err := engine.OpenBackends(t.Context(), &b, parsedCompany(t, companyDoc), nil)
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { back.Close(t.Context()) })

	// The broker reports the name it was started under. Anything else means
	// the raw field was used and the environment was never consulted.
	name := brokerServerName(t, back)
	if name != "node-from-the-orchestrator" {
		t.Errorf("the broker calls itself %q, want the resolved node id "+
			"%q: the stream was handed the raw node.id field, which is empty "+
			"whenever the id comes from the environment",
			name, "node-from-the-orchestrator")
	}
}

// TestAClusteredMemberStartsOnAnEnvironmentSuppliedID: a clustered embedded
// member with no name is refused at boot, so it must take the resolved id.
func TestAClusteredMemberStartsOnAnEnvironmentSuppliedID(t *testing.T) {
	t.Setenv(config.NodeIDEnvVar, "node-a")

	b := config.DefaultBootstrap()
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	brokertest.StoreIn(&b.Stream, filepath.Join(brokertest.TempDir(t), "stream"))
	b.Node.ID = ""
	b.Coordination.Type = config.CoordinationEmbeddedKV
	// A cluster port with no peers: enough to make this a clustered member
	// — which is what requires a server name — without needing peers to
	// answer for a quorum this test never asks for.
	b.Stream.Cluster.Name = "acme"
	b.Stream.Cluster.Port = 0

	engine.SeedStore(t, &b)
	back, err := engine.OpenBackends(t.Context(), &b, parsedCompany(t, companyDoc), nil)
	if err != nil {
		t.Fatalf("a clustered member refused to start on an environment-supplied "+
			"node id: %v", err)
	}
	t.Cleanup(func() { back.Close(t.Context()) })

	if got := brokerServerName(t, back); got != "node-a" {
		t.Errorf("the clustered member calls itself %q, want %q", got, "node-a")
	}
}

// TestWhatANodePublishesNamesThatNode pins the other half of a node's name:
// every event it publishes carries it as the envelope's `node`, and the event
// store's row carries the same, in the payload and in its `node` column
// (node/0054).
//
// The field is the only thing that says where an event HAPPENED, not the store
// a row is read from: a node without `data` keeps no event log, and a data node
// keeping events in custody holds rows it never published; the turn answer's
// `nodes` and an operator reading a row or frame ask it. Asserted on the
// stored row, what a reader later holds, and through the environment-supplied
// id, the orchestrator's shape, for which the raw config field reads empty.
func TestWhatANodePublishesNamesThatNode(t *testing.T) {
	// Not parallel: it sets the environment variable whose precedence is
	// the subject.
	t.Setenv(config.NodeIDEnvVar, "node-from-the-orchestrator")

	b := config.DefaultBootstrap()
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	brokertest.StoreIn(&b.Stream, filepath.Join(brokertest.TempDir(t), "stream"))
	b.Node.ID = ""

	engine.SeedStore(t, &b)
	back, err := engine.OpenBackends(t.Context(), &b, parsedCompany(t, companyDoc), nil)
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { back.Close(t.Context()) })

	sent := events.New(types.OrgStarted{OrgName: "Nimbus"}, events.TraceContext{})
	if err := back.Queue.Publish(t.Context(), topics.Event(sent.Type), sent); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	row, err := back.Store.Events().ByID(t.Context(), sent.ID.String(), time.Now())
	if err != nil {
		t.Fatalf("the event store holds no row for what this node published: %v", err)
	}
	var stored events.Event
	if err := json.Unmarshal(row.Payload, &stored); err != nil {
		t.Fatalf("decode the stored row: %v", err)
	}
	if stored.Node != "node-from-the-orchestrator" {
		t.Errorf("the row this node wrote names node %q, want the resolved node id %q: "+
			"the queue was built without the node it publishes for",
			stored.Node, "node-from-the-orchestrator")
	}
	if row.Node != "node-from-the-orchestrator" {
		t.Errorf("the row's node column reads %q, want %q: the payload names its "+
			"origin and the column every reader of where a turn ran asks does not",
			row.Node, "node-from-the-orchestrator")
	}
}
