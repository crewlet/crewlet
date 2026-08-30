package engine_test

import (
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
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
// started under, for the config shape a container orchestrator actually uses.
//
// A node's name has three sources in precedence order — `node.id` in the file,
// then CREWLET_NODE_ID, then the default — and only the first is a struct
// field. The fleet guide tells an operator to inject the variable and leave
// the key out, which is exactly the shape that leaves that field EMPTY.
//
// That mattered because JetStream places replicas BY SERVER NAME, so this
// value is the member's identity in the cluster. Passing the raw field meant a
// clustered member was started with no name and refused outright at boot —
// the documented fleet shape failing to start — while a solo member silently
// fell back to the broker's own literal default, detaching its identity from
// the node's for as long as nobody looked.
//
// Asserted through the SOLO path because it is the one that failed QUIETLY:
// the clustered path errors, and an error is a symptom somebody chases.
func TestTheBrokerTakesTheRESOLVEDNodeID(t *testing.T) {
	// Not parallel: it sets the environment variable whose precedence is
	// the subject.
	t.Setenv(config.NodeIDEnvVar, "node-from-the-orchestrator")

	b := config.DefaultBootstrap()
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
	// Deliberately unset, which is the whole point: the id lives only in
	// the environment.
	b.Node.ID = ""

	back, err := engine.OpenBackends(t.Context(), &b, parsedCompany(t, companyDoc))
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

// TestAClusteredMemberStartsOnAnEnvironmentSuppliedID is the failure the fix
// was actually for: with no name, a clustered embedded member is refused at
// boot rather than starting anonymously.
func TestAClusteredMemberStartsOnAnEnvironmentSuppliedID(t *testing.T) {
	t.Setenv(config.NodeIDEnvVar, "node-a")

	b := config.DefaultBootstrap()
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
	b.Node.ID = ""
	b.Coordination.Type = config.CoordinationEmbeddedKV
	// A cluster port with no peers: enough to make this a clustered member
	// — which is what requires a server name — without needing peers to
	// answer for a quorum this test never asks for.
	b.Stream.Cluster.Name = "acme"
	b.Stream.Cluster.Port = 0

	back, err := engine.OpenBackends(t.Context(), &b, parsedCompany(t, companyDoc))
	if err != nil {
		t.Fatalf("a clustered member refused to start on an environment-supplied "+
			"node id: %v", err)
	}
	t.Cleanup(func() { back.Close(t.Context()) })

	if got := brokerServerName(t, back); got != "node-a" {
		t.Errorf("the clustered member calls itself %q, want %q", got, "node-a")
	}
}
