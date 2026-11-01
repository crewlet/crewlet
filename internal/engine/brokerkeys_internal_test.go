package engine

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// twoKeyBootstrap is testBootstrap holding two keys, the second active: what a
// node looks like halfway through a keyring rotation.
func twoKeyBootstrap(t *testing.T) config.Bootstrap {
	t.Helper()
	b := testBootstrap(t)
	second, err := secrets.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	b.Secrets.Keys = append(b.Secrets.Keys, config.SecretKey{ID: "test-2",
		Material: secrets.EncodeKey(second)})
	b.Secrets.ActiveKeyID = "test-2"
	return b
}

// THE BROKER IS KEYED FROM TIER A, on every topology.
//
// What a node's embedded broker presents and accepts on its fleet listeners is
// derived from its own secrets.keys by the keyring's rule (ADR-0087) — every
// key accepted, the active one presented — so the broker moves with the
// keyring's rotation like everything else signed under it. A member, a leaf
// and a solo member alike: which listeners a broker opens is the jetstream
// package's to decide from the rest of the config, and a solo member's
// credentials simply go unpresented.
//
// Mutation: leave FleetAuth zero → every row, red.
func TestTheBrokerIsKeyedFromTierA(t *testing.T) {
	t.Parallel()
	for name, shape := range map[string]func(*config.Bootstrap){
		"a clustered member": func(b *config.Bootstrap) {
			b.Stream.Cluster = config.StreamCluster{Name: "acme", Port: 6222,
				Peers: []string{"nats://node-b.internal:6222", "nats://node-c.internal:6222"}}
		},
		"a leaf": func(b *config.Bootstrap) {
			b.Node.Roles = []string{"seats"}
			b.Stream.Leaf.URLs = []string{"nats-leaf://member-a.internal:7422"}
		},
		"a solo member": func(*config.Bootstrap) {},
	} {
		b := twoKeyBootstrap(t)
		shape(&b)
		cfg, err := streamConfig(&b)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		want, err := jetstream.FleetAuthFrom(b.Secrets.TokenMaterial())
		if err != nil {
			t.Fatal(err)
		}
		if cfg.FleetAuth.IsZero() || cfg.FleetAuth.String() != want.String() {
			t.Errorf("%s's broker presents and accepts %q, want %q", name,
				cfg.FleetAuth, want)
		}
		if got := cfg.FleetAuth.String(); got != "presents test-2, accepts test,test-2" {
			t.Errorf("%s's broker: %s — it must accept both keys and present the active one",
				name, got)
		}
	}
}

// A NODE'S BROKER COUNTS WHAT IT REFUSES, from the moment it starts.
//
// openStream installs the counter on the broker it has just started, over the
// node's recorder: a leaf on another keyring dialling this member's leaf
// listener is refused, and crewlet.broker.peer.refusals rises under
// {leaf, unknown}. The sink counts both attributes, and a node with no
// recorder counts nothing and does not fail.
//
// Mutations: openStream installs nothing → the live row never rises, red; drop
// an attribute → the sink row, red.
func TestOpenStreamCountsItsBrokersRefusals(t *testing.T) {
	t.Parallel()
	rec, err := metrics.New()
	if err != nil {
		t.Fatal(err)
	}
	b := testBootstrap(t)
	b.Node.ID = "refusing-member"
	b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
	b.Stream.Leaf.Host = "127.0.0.1"
	var out *Backends
	for attempt := 1; ; attempt++ {
		// A PORT LOST BETWEEN THE PROBE AND THE BIND says nothing about the
		// case; anything else is the answer.
		b.Stream.Leaf.Port = freeLoopbackPort(t)
		cfg, err := streamConfig(&b)
		if err != nil {
			t.Fatal(err)
		}
		if out, _, err = openStream(t.Context(), &b, cfg, rec); err == nil {
			break
		}
		if !errors.Is(err, jetstream.ErrLeafPortTaken) || attempt == 4 {
			t.Fatalf("open the member's stream: %v", err)
		}
	}
	t.Cleanup(func() { out.Close(context.WithoutCancel(t.Context())) })
	port := b.Stream.Leaf.Port

	stranger, err := jetstream.FleetAuthFrom(runtoken.OneKey("k9",
		"another fleet's placeholder material"))
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := jetstream.StartServer(t.Context(), jetstream.Config{ServerName: "stranger",
		LeafURLs:  []string{fmt.Sprintf("nats-leaf://127.0.0.1:%d", port)},
		FleetAuth: stranger})
	if err != nil {
		t.Fatalf("start the stranger leaf: %v", err)
	}
	t.Cleanup(leaf.Shutdown)

	deadline := time.Now().Add(30 * time.Second)
	for brokerRefusals(rec, "leaf", "unknown") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the member's broker refused the stranger's link and counted nothing")
		}
		time.Sleep(20 * time.Millisecond)
	}

	sink, err := metrics.New()
	if err != nil {
		t.Fatal(err)
	}
	countRefusals(sink)(jetstream.PeerRefusal{Listener: jetstream.ListenerRoute,
		Presented: jetstream.PresentedInvalid}, 3)
	if got := brokerRefusals(sink, "route", "invalid"); got != 3 {
		t.Errorf("the sink counted %d route/invalid refusals, want 3", got)
	}
	countRefusals(nil)(jetstream.PeerRefusal{Listener: jetstream.ListenerRoute,
		Presented: jetstream.PresentedNone}, 1)
}

// brokerRefusals is the refusal counter's total for one class.
func brokerRefusals(rec *metrics.Recorder, listener, presented string) uint64 {
	for _, s := range rec.Read() {
		if s.Name == metrics.BrokerPeerRefusals && s.Attrs["listener"] == listener &&
			s.Attrs["presented"] == presented {
			return s.Total
		}
	}
	return 0
}

// freeLoopbackPort is a loopback port nothing held a moment ago.
func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	var lc net.ListenConfig
	l, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find a free port: %v", err)
	}
	defer l.Close()
	//nolint:errcheck // Listen on a TCP address always yields *TCPAddr.
	return l.Addr().(*net.TCPAddr).Port
}
