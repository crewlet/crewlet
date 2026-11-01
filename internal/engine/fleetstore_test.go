package engine_test

import (
	"errors"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/fleetsecrets"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/queue/jetstream/externaltest"
	"github.com/crewlet/crewlet/internal/secrets"
)

// putSecret writes one value onto the fleet's store the way every writer of it
// does, through fleetsecrets under the node's own keyring.
func putSecret(t *testing.T, b *config.Bootstrap, fs *engine.FleetStore, name, value string) {
	t.Helper()
	cipher, err := b.Secrets.Cipher()
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	if err := fleetsecrets.New(fs.Fleet, cipher).Set(t.Context(), name, value,
		secrets.Author{Name: "the test", Kind: "operator"}, secrets.SourceCLI,
		time.Now()); err != nil {
		t.Fatalf("set %s: %v", name, err)
	}
}

// readSecret reads one value back off whichever store holds it.
func readSecret(t *testing.T, b *config.Bootstrap, fleet coord.Secrets, name string) string {
	t.Helper()
	cipher, err := b.Secrets.Cipher()
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	got, err := fleetsecrets.New(fleet, cipher).Get(t.Context(), name)
	if err != nil {
		t.Fatalf("get %s: %v", name, err)
	}
	return got
}

// A STOPPED SOLO NODE'S FLEET STORE IS REACHED FROM OUTSIDE THE ENGINE, and
// what is written there is what the engine reads when it boots.
//
// The whole point of the opener: with the engine stopped, a `crewlet` command
// reads and writes the company's values where the engine keeps them, rather
// than in a table of its own that the fleet never sees. Booting the engine on
// the same store afterwards is what proves the buckets this created — on a
// store no engine had ever booted — are the ones the engine adopts.
//
// Mutation: open a bucket of the fleet store with settings of its own (a
// second constructor) and the engine's boot sees a different store, or
// refuses the bucket; drop the store directory from the broker this starts and
// the value is gone at the next open.
func TestAStoppedNodesFleetStoreIsReachedAndTheEngineReadsIt(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "stream")
	b := bootstrap(t, func(b *config.Bootstrap) { brokertest.StoreIn(&b.Stream, dir) })

	fs, err := engine.OpenFleetStore(t.Context(), b)
	if err != nil {
		t.Fatalf("open the fleet's store on a node that has never booted: %v", err)
	}
	if !strings.Contains(fs.Where, dir) {
		t.Errorf("Where = %q, want it to name the store directory %s", fs.Where, dir)
	}
	putSecret(t, b, fs, "GITLAB_TOKEN", "glpat-offline")
	fs.Close()

	again, err := engine.OpenFleetStore(t.Context(), b)
	if err != nil {
		t.Fatalf("open the fleet's store a second time: %v", err)
	}
	if got := readSecret(t, b, again.Fleet, "GITLAB_TOKEN"); got != "glpat-offline" {
		t.Errorf("a second open read %q, want the value the first wrote", got)
	}
	again.Close()

	back, err := openBackends(t, b)
	if err != nil {
		t.Fatalf("the engine did not boot on the store the opener created: %v", err)
	}
	t.Cleanup(func() { back.Close(t.Context()) })
	if got := readSecret(t, b, back.Fleet, "GITLAB_TOKEN"); got != "glpat-offline" {
		t.Errorf("the engine read %q, want the value written while it was stopped", got)
	}
}

// WHILE THE ENGINE RUNS, ITS STORE DIRECTORY IS ITS OWN: the opener is refused
// rather than starting a second broker on the engine's files — which
// nats-server itself would accept, and which would corrupt them.
//
// Mutation: drop the store directory's lock and the open succeeds beside the
// running engine.
func TestTheFleetStoreIsRefusedWhileTheEngineHoldsItsBroker(t *testing.T) {
	t.Parallel()
	b := bootstrap(t, func(b *config.Bootstrap) {
		brokertest.StoreIn(&b.Stream, filepath.Join(t.TempDir(), "stream"))
	})
	back, err := openBackends(t, b)
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { back.Close(t.Context()) })

	fs, err := engine.OpenFleetStore(t.Context(), b)
	if err == nil {
		fs.Close()
		t.Fatal("a second broker was started on the running engine's store directory")
	}
	if !errors.Is(err, jetstream.ErrStoreDirHeld) {
		t.Errorf("the refusal is not ErrStoreDirHeld: %v", err)
	}
}

// A NODE THAT CANNOT REACH THE STORE ALONE IS SENT TO ITS PEERS, before
// anything is started: a member of a cluster, whose store needs a quorum it
// cannot reach by itself, and a node that holds no data, whose store is the
// members'.
//
// Mutation: drop either refusal and that node starts a broker of its own — a
// clustered member waits out its whole readiness budget and fails naming
// nothing useful, and a leaf with no store directory opens nothing at all.
func TestANodeThatCannotReachTheStoreAloneIsSentToItsPeers(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]func(*config.Bootstrap){
		"a member of a cluster": func(b *config.Bootstrap) {
			brokertest.StoreIn(&b.Stream, filepath.Join(t.TempDir(), "stream"))
			b.Stream.Cluster = config.StreamCluster{Name: "fleet",
				Peers: []string{"nats://127.0.0.1:1"}}
			b.Stream.Replicas = 3
		},
		"a node that holds no data": func(b *config.Bootstrap) {
			b.Stream.Leaf.URLs = []string{"nats-leaf://127.0.0.1:1"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b, _ := unseededBootstrap(t, mutate)
			fs, err := engine.OpenFleetStore(t.Context(), b)
			if err == nil {
				fs.Close()
				t.Fatal("the node reached the fleet's store on its own")
			}
			if !errors.Is(err, engine.ErrNoFleetStoreHere) {
				t.Errorf("the refusal is not ErrNoFleetStoreHere: %v", err)
			}
		})
	}
}

// THE BROKER IT STARTS LISTENS FOR NOBODY, even on a member that has a leaf
// listener: the fleet's leaves would rejoin a broker that is about to go away.
//
// The node's leaf port is held by the test for the whole case, so a broker that
// tried to open it would be refused — a member probes its leaf port before it
// starts ([jetstream.ErrLeafPortTaken]) — and nothing else on the machine can
// take the port in between, as it could a port merely found free.
//
// Mutation: pass the node's leaf port through to the broker and the open is
// refused with the port taken.
func TestTheFleetStoreOpensNoLeafListener(t *testing.T) {
	t.Parallel()
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("hold a port: %v", err)
	}
	defer held.Close()
	_, port, err := net.SplitHostPort(held.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	leafPort, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	b := bootstrap(t, func(b *config.Bootstrap) {
		brokertest.StoreIn(&b.Stream, filepath.Join(t.TempDir(), "stream"))
		b.Stream.Leaf.Port = leafPort
		b.Stream.Leaf.Host = "127.0.0.1"
	})
	fs, err := engine.OpenFleetStore(t.Context(), b)
	if err != nil {
		t.Fatalf("a member whose leaf port is taken could not open its fleet store, "+
			"so the broker opened for it tried to listen there: %v", err)
	}
	fs.Close()
}

// A NODE ON AN EXTERNAL BROKER DIALS IT, and the store there is the one every
// node of the fleet shares.
//
// Mutation: start an embedded broker for a node with stream.url and the value
// lands in a private store no peer reads.
func TestAnExternalBrokersFleetStoreIsDialled(t *testing.T) {
	t.Parallel()
	url := externaltest.Start(t, queue.MaxPayloadBytes).URL()
	b := bootstrap(t, func(b *config.Bootstrap) {
		b.Stream.Type = config.StreamNATS
		b.Stream.URL = url
		b.Stream.Token = externaltest.Token
	})
	fs, err := engine.OpenFleetStore(t.Context(), b)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if !strings.Contains(fs.Where, "external broker") {
		t.Errorf("Where = %q, want it to say it reached the external broker", fs.Where)
	}
	putSecret(t, b, fs, "JIRA_TOKEN", "on-the-cluster")
	fs.Close()

	back, err := openBackends(t, b)
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { back.Close(t.Context()) })
	if got := readSecret(t, b, back.Fleet, "JIRA_TOKEN"); got != "on-the-cluster" {
		t.Errorf("a node on the same broker read %q, want the value the opener wrote", got)
	}
}
