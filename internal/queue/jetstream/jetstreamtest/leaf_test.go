package jetstreamtest

import (
	"testing"
	"time"

	js "github.com/crewlet/crewlet/internal/queue/jetstream"
)

// A CLUSTER STARTED WITH LEAF LISTENERS IS ONE A LEAF CAN USE: every member
// listens, and a leaf joined to them provisions, appends and reads across its
// link. This is the topology the partition benchmark measures a node without
// the `data` role on, so a listener that bound but served nothing would turn
// every leaf number there into a number about a timeout.
func TestALeafJoinsAClusterStartedWithLeafListeners(t *testing.T) {
	c := startCluster(t, 3, js.Config{}, true)
	urls := c.LeafURLs()
	if len(urls) != 3 {
		t.Fatalf("a three-member cluster with leaf listeners offers %d leaf URLs %v, "+
			"want one per member", len(urls), urls)
	}

	leaf, err := js.StartServer(t.Context(), js.Config{ServerName: "leaf", LeafURLs: urls,
		Replicas: 3})
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
	msgs, err := reader.Fetch(t.Context(), 1, 0, 5*time.Second)
	if err != nil || len(msgs) != 1 || msgs[0].Seq != seq {
		t.Fatalf("read back %d records (err %v), want the one appended at %d",
			len(msgs), err, seq)
	}

	// THE CONTROL: members without listeners are named by none, or the
	// count above passes on a harness that reports every member.
	plain := &Cluster{Configs: []js.Config{{ServerName: "a"}, {ServerName: "b"}}}
	if got := plain.LeafURLs(); len(got) != 0 {
		t.Fatalf("members without leaf listeners are offered as %v", got)
	}
}
