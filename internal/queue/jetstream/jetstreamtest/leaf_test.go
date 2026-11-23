package jetstreamtest

import (
	"context"
	"testing"
	"time"

	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A CLUSTER STARTED WITH LEAF LISTENERS IS ONE A LEAF CAN USE: every member
// listens, and a leaf joined to them provisions, appends and reads across its
// link. This is the topology the partition benchmark measures a node without
// the `data` role on, so a listener that bound but served nothing would turn
// every leaf number there into a number about a timeout.
func TestALeafJoinsAClusterStartedWithLeafListeners(t *testing.T) {
	t.Parallel()
	c := startCluster(t, 3, js.Config{}, true)
	urls := c.LeafURLs()
	if len(urls) != 3 {
		t.Fatalf("a three-member cluster with leaf listeners offers %d leaf URLs %v, "+
			"want one per member", len(urls), urls)
	}

	leaf, err := js.StartServer(t.Context(), js.Config{ServerName: "leaf", LeafURLs: urls,
		Replicas: 3, FleetAuth: fleetAuth()})
	if err != nil {
		t.Fatalf("start a leaf of the cluster: %v", err)
	}
	t.Cleanup(leaf.Shutdown)
	q, err := leaf.Client(t.Context())
	if err != nil {
		t.Fatalf("a client of the leaf: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(t.Context()) })

	const stream = "CREWLET_LEAF_PROBE"
	if err := q.EnsureDomainStream(t.Context(), js.DomainStream{Name: stream,
		Subjects: []string{"crewlet.leafprobe.>"}, MaxBytes: 1 << 20,
		Duplicates: time.Minute}); err != nil {
		t.Fatalf("provision a replicated stream across the leaf link: %v", err)
	}
	log, err := q.DomainLog(t.Context(), stream)
	if err != nil {
		t.Fatalf("open the stream's log: %v", err)
	}
	var none uint64
	seq, _, err := log.Append(t.Context(), "crewlet.leafprobe.a", "", &none, []byte("x"))
	if err != nil {
		t.Fatalf("a conditional append across the leaf link: %v", err)
	}
	reader, err := q.DomainConsumer(t.Context(), stream, "leaf", 0)
	if err != nil {
		t.Fatalf("a durable reader across the leaf link: %v", err)
	}
	// Read as the applier reads, each pull an idle wait (statelog.FetchWait)
	// until the record comes: how soon it crosses a loaded runner's link is
	// the partition benchmark's number, not this claim (#1922). The bound is
	// liveness, never the claim: a link that serves nothing never hands it
	// over, and a data node whose position stands still for
	// statelog.StallGrace already has its copy taken out of service.
	ctx, cancel := context.WithTimeout(t.Context(), statelog.StallGrace)
	defer cancel()
	var msgs []statelog.Message
	for len(msgs) == 0 {
		msgs, err = reader.Fetch(ctx, 1, 0, statelog.FetchWait)
		switch {
		case ctx.Err() != nil:
			t.Fatalf("the record appended at %d through the leaf never came back across its link "+
				"in %s, the stall grace a data node's copy is taken out of service at", seq,
				statelog.StallGrace)
		case err != nil:
			t.Fatalf("a read across the leaf link: %v", err)
		}
	}
	if len(msgs) != 1 || msgs[0].Seq != seq {
		t.Fatalf("read back %d records, the first at %d, want the one appended at %d",
			len(msgs), msgs[0].Seq, seq)
	}

	// THE CONTROL: members without listeners are named by none, or the
	// count above passes on a harness that reports every member.
	plain := &Cluster{Configs: []js.Config{{ServerName: "a"}, {ServerName: "b"}}}
	if got := plain.LeafURLs(); len(got) != 0 {
		t.Fatalf("members without leaf listeners are offered as %v", got)
	}
}
