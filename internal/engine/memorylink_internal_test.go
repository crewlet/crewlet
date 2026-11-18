package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/learning/memsync"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/queue/jetstream/externaltest"
	"github.com/crewlet/crewlet/internal/seat/placement"
)

// A NODE'S HYDRATION READS ITS LINK'S METER: hydration across a link tells a
// replay held behind the link's other traffic from a stalled copy by the meter
// the queue makes on the connection it dials (a leaf's data link, any external
// cluster's). So the backends carry that meter into the memory sync: a member
// hands it no link, a leaf and an external node their own queue's meter.
//
// Mutants: the memory sync's configuration leaves the link out, or the
// backends (the external branch's included) carry no meter.
func TestANodesHydrationReadsItsLinksMeter(t *testing.T) {
	t.Parallel()
	open := func(b *bootstrapOf) *Backends {
		t.Helper()
		cfg, err := streamConfig(b.b)
		if err != nil {
			t.Fatal(err)
		}
		out, _, err := openStream(t.Context(), b.b, cfg, nil)
		if err != nil {
			b.err = err
			return nil
		}
		t.Cleanup(func() { out.Close(context.WithoutCancel(t.Context())) })
		return out
	}

	member := testBootstrap(t)
	member.Node.ID = "metered-member"
	brokertest.StoreIn(&member.Stream, filepath.Join(brokertest.TempDir(t), "stream"))
	member.Stream.Leaf.Host = "127.0.0.1"
	var onMember *Backends
	for attempt := 1; onMember == nil; attempt++ {
		// A PORT LOST BETWEEN THE PROBE AND THE BIND says nothing about the
		// case; anything else is the answer.
		member.Stream.Leaf.Port = freeLoopbackPort(t)
		of := &bootstrapOf{b: &member}
		if onMember = open(of); onMember == nil &&
			(!errors.Is(of.err, jetstream.ErrLeafPortTaken) || attempt == 4) {
			t.Fatalf("open the member's stream: %v", of.err)
		}
	}

	leaf := testBootstrap(t)
	leaf.Node.ID = "metered-leaf"
	// THE FLEET'S KEYRING, which every node holds: each test bootstrap mints
	// a key of its own.
	leaf.Secrets = member.Secrets
	leaf.Stream.Leaf.URLs = []string{fmt.Sprintf("nats-leaf://127.0.0.1:%d", member.Stream.Leaf.Port)}
	if kind := leaf.BrokerKind(); kind != placement.BrokerLeaf {
		t.Fatalf("the premise: the leaf's bootstrap names a broker of kind %q, want a leaf", kind)
	}
	of := &bootstrapOf{b: &leaf}
	onLeaf := open(of)
	if onLeaf == nil {
		t.Fatalf("open the leaf's stream: %v", of.err)
	}
	q, ok := onLeaf.Queue.(*jetstream.Queue)
	if !ok || q.LinkMeter() == nil {
		t.Fatalf("the premise: the leaf's queue is %T with no meter of its link", onLeaf.Queue)
	}

	srv := externaltest.Start(t, queue.MaxPayloadBytes)
	external := testBootstrap(t)
	external.Node.ID = "metered-external"
	external.Stream.Type = config.StreamNATS
	external.Stream.URL = srv.URL()
	external.Stream.Token = externaltest.Token
	external.Coordination.Type = config.CoordinationEmbeddedKV
	of = &bootstrapOf{b: &external}
	onExternal := open(of)
	if onExternal == nil {
		t.Fatalf("open the external node's stream: %v", of.err)
	}
	eq, ok := onExternal.Queue.(*jetstream.Queue)
	if !ok || eq.LinkMeter() == nil {
		t.Fatalf("the premise: the external node's queue is %T with no meter of its link", onExternal.Queue)
	}

	for _, c := range []struct {
		name     string
		cfg      memsync.Config
		want     memsync.Link
		acrossIt bool
	}{
		{"the member", memoryConfig(&member, onMember), nil, false},
		{"the leaf", memoryConfig(&leaf, onLeaf), q.LinkMeter(), true},
		{"the external node", memoryConfig(&external, onExternal), eq.LinkMeter(), true},
	} {
		if c.cfg.Link != c.want || c.cfg.AcrossLink != c.acrossIt {
			t.Errorf("%s's memory sync reads a link %t (its queue's meter: %t), across a link %t; "+
				"want a link %t, the queue's meter, across a link %t", c.name, c.cfg.Link != nil,
				c.want != nil && c.cfg.Link == c.want, c.cfg.AcrossLink, c.want != nil, c.acrossIt)
		}
	}
}

// bootstrapOf is a bootstrap a case opens a stream for, and what refused it.
type bootstrapOf struct {
	b   *config.Bootstrap
	err error
}
