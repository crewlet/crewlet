package kv

import (
	"context"
	"errors"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/jsapi"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/jetstreamtest"
)

// A CERTIFYING READ IS NEVER ANSWERED BY A REPLICA THAT IS BEHIND.
//
// The certification exists for one conclusion: a key the leader's index named
// and the pass never delivered is either read back or the listing fails. Read
// back through the bucket's own Get, it was read by a DIRECT get, which every
// replica answers from its own copy — so a replica that had not applied the
// key answered "not found", the listing read that as "deleted since the
// index", and a key live throughout the listing was absent from it.
//
// A member cut off from its peers is the deterministic form of "behind": the
// majority commits the key, and the cut member never receives it. The case
// first proves THAT PREMISE — the cut member's own direct get answers "not
// found" for a key the quorum holds — because without it the assertions below
// would pass against the very read they exist to rule out. Then it holds both
// halves of the certification to the leader:
//
//   - the certifying read, sent through the cut member, FAILS (never "not
//     found"), both while the member still believes in the old leader and
//     after it knows it has none;
//   - the key index, which a member answers from its own copy once its group
//     has no leader, is refused rather than taken as the leader's.
//
// And before the cut, the same read through the same member IS answered —
// by the leader, over the member's routes — so a read that simply never
// worked cannot pass either.
func TestACertifyingReadIsNeverAnsweredByAReplicaThatIsBehind(t *testing.T) {
	t.Parallel()
	c := jetstreamtest.StartPartitionableCluster(t, 3, js.Config{})
	ctx := t.Context()

	client := func(i int) jetstream.JetStream {
		t.Helper()
		nc, err := c.Servers[i].Conn()
		if err != nil {
			t.Fatalf("member %d: connect: %v", i, err)
		}
		t.Cleanup(nc.Close)
		client, err := jsapi.Embedded().Client(nc)
		if err != nil {
			t.Fatalf("member %d: client: %v", i, err)
		}
		return client
	}
	const bucket = "walk_leader_reads"
	if _, err := openBucket(ctx, client(0), true, jetstream.KeyValueConfig{
		Bucket: bucket, Replicas: 3,
	}); err != nil {
		t.Fatalf("open a three-replica bucket: %v", err)
	}

	// Which member leads the bucket's stream, and one that does not.
	leader, behind := -1, -1
	info, err := func() (*jetstream.StreamInfo, error) {
		stream, err := client(0).Stream(ctx, "KV_"+bucket)
		if err != nil {
			return nil, err
		}
		return stream.Info(ctx)
	}()
	if err != nil || info.Cluster == nil {
		t.Fatalf("read the bucket's stream info: (%v, %v)", info, err)
	}
	for i, cfg := range c.Configs {
		switch {
		case cfg.ServerName == info.Cluster.Leader:
			leader = i
		case behind < 0:
			behind = i
		}
	}
	if leader < 0 || behind < 0 {
		t.Fatalf("the stream's leader is %q, which is none of the members", info.Cluster.Leader)
	}

	onLeader, err := client(leader).KeyValue(ctx, bucket)
	if err != nil {
		t.Fatalf("the bucket on the leader: %v", err)
	}
	cutClient := client(behind)
	onCut, err := cutClient.KeyValue(ctx, bucket)
	if err != nil {
		t.Fatalf("the bucket on the member about to be cut: %v", err)
	}
	read, err := newLeaderReader(cutClient, onCut)
	if err != nil {
		t.Fatalf("reader: %v", err)
	}
	certifyOne := func(key string, within time.Duration) (map[string]jetstream.KeyValueEntry, error) {
		ctx, cancel := context.WithTimeout(ctx, within)
		defer cancel()
		latest := map[string]jetstream.KeyValueEntry{}
		return latest, certify(ctx, read, "the bucket", latest, []string{key})
	}

	// Connected: a read sent through a member that is not the leader is
	// answered, by the leader, with the key.
	if _, err := onLeader.Put(ctx, "before", []byte("1")); err != nil {
		t.Fatalf("put before the cut: %v", err)
	}
	latest, err := certifyOne("before", 10*time.Second)
	if err != nil {
		t.Fatalf("a certifying read through a connected member: %v", err)
	}
	if kve := latest["before"]; kve == nil || string(kve.Value()) != "1" {
		t.Fatalf("a certifying read through a connected member recorded %v, want the key", latest)
	}

	c.Partition(t, behind)
	if _, err := onLeader.Put(ctx, "committed", []byte("2")); err != nil {
		t.Fatalf("the majority's put after the cut: %v", err)
	}

	// THE PREMISE: the cut member's direct get answers from a copy without
	// the key. A member joins the direct-get group only on a timer after it
	// first follows a leader (nats-server jetstream_cluster.go, the direct
	// access monitor), so until it has, the read finds no responder at all
	// on its side of the cut — which is no answer rather than a wrong one,
	// and is waited out.
	premise := time.Now().Add(30 * time.Second)
	for {
		directCtx, cancelDirect := context.WithTimeout(ctx, 3*time.Second)
		_, err := onCut.Get(directCtx, "committed")
		cancelDirect()
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			break
		}
		if time.Now().After(premise) {
			t.Fatalf("the cut member's direct get of a key the quorum committed "+
				"answered %v, want ErrKeyNotFound — without a replica that answers "+
				"from a copy that is behind, this case exercises nothing", err)
		}
		time.Sleep(250 * time.Millisecond)
	}

	assertRefused := func(when string) {
		t.Helper()
		latest, err := certifyOne("committed", 3*time.Second)
		if err == nil {
			t.Fatalf("%s: a certifying read of a key the quorum committed, sent "+
				"through a member that never received it, came back as %v with no "+
				"error — the listing would answer without a key live throughout it",
				when, latest)
		}
		if !errors.Is(err, coord.ErrUnavailable) {
			t.Errorf("%s: err = %v, want it to carry coord.ErrUnavailable", when, err)
		}
	}
	assertRefused("while the cut member's own direct get answers \"not found\"")

	// The key index, through the cut member, until that member stops staying
	// silent and answers from its own copy — which it does once it has
	// decided its group has no leader, NATS's own timing rather than this
	// engine's. Every answer before that is a timeout or a refusal.
	deadline := time.Now().Add(90 * time.Second)
	var last error
	for {
		ictx, cancel := context.WithTimeout(ctx, 3*time.Second)
		names, err := keysUnder(ictx, cutClient, onCut, jetstream.AllKeys)
		cancel()
		if errors.Is(err, errIndexLeaderless) {
			break
		}
		if err == nil {
			slices.Sort(names)
			t.Fatalf("the cut member's own index was taken as the leader's: it "+
				"named %v, and the quorum holds [before committed]", names)
		}
		last = err
		if time.Now().After(deadline) {
			t.Fatalf("the cut member never answered its index from its own "+
				"copy within the window (last: %v), so the refusal this case "+
				"is about was never exercised", last)
		}
		time.Sleep(250 * time.Millisecond)
	}
	assertRefused("once the cut member knows its group has no leader")
}

// A TOMBSTONE A REPLICA SERVED IS READ FROM THE LEADER, ON A CLUSTER.
//
// The failure lives only here: the pass is an R1 consumer the broker places
// on a random member of a replicated bucket's group, so a key deleted and
// created again before the listing began comes back as a MARKER from a member
// that applied the delete and not yet the re-creation — and a listing that
// took the marker as the key's answer dropped a key live throughout it.
//
// The broker offers no way to put a pass on a chosen member, so the member's
// answer is REPLAYED at the pass boundary: the pass the bucket gave before
// the re-creation, recorded through a follower, is what that member delivers.
// Everything after the boundary is the cluster's own — the index answered by
// the stream leader, and the certifying read sent through the follower and
// answered by the leader over the member's routes.
func TestATombstoneAReplicaServedIsReadFromTheLeaderOnACluster(t *testing.T) {
	t.Parallel()
	c := jetstreamtest.StartCluster(t, 3, js.Config{})
	ctx := t.Context()

	client := func(i int) jetstream.JetStream {
		t.Helper()
		nc, err := c.Servers[i].Conn()
		if err != nil {
			t.Fatalf("member %d: connect: %v", i, err)
		}
		t.Cleanup(nc.Close)
		client, err := jsapi.Embedded().Client(nc)
		if err != nil {
			t.Fatalf("member %d: client: %v", i, err)
		}
		return client
	}
	const bucket = "walk_replayed_tombstone"
	if _, err := openBucket(ctx, client(0), true, jetstream.KeyValueConfig{
		Bucket: bucket, Replicas: 3,
	}); err != nil {
		t.Fatalf("open a three-replica bucket: %v", err)
	}
	stream, err := client(0).Stream(ctx, "KV_"+bucket)
	if err != nil {
		t.Fatalf("the bucket's stream: %v", err)
	}
	info, err := stream.Info(ctx)
	if err != nil || info.Cluster == nil {
		t.Fatalf("read the bucket's stream info: (%v, %v)", info, err)
	}
	follower := -1
	for i, cfg := range c.Configs {
		if cfg.ServerName != info.Cluster.Leader {
			follower = i
			break
		}
	}
	if follower < 0 {
		t.Fatalf("every member is the leader %q", info.Cluster.Leader)
	}
	viaFollower := client(follower)
	kv, err := viaFollower.KeyValue(ctx, bucket)
	if err != nil {
		t.Fatalf("the bucket through a follower: %v", err)
	}

	for _, key := range []string{"kept", "recreated"} {
		if _, err := kv.Put(ctx, key, []byte("1")); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}
	if err := kv.Delete(ctx, "recreated"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	behind := recordPass(ctx, t, kv)
	if !markerIn(behind, "recreated") {
		t.Fatal("the recorded pass holds no marker for the re-created key, so " +
			"this case replays nothing a replica that is behind would deliver")
	}
	if _, err := kv.Put(ctx, "recreated", []byte("2")); err != nil {
		t.Fatalf("create again: %v", err)
	}

	read, err := newLeaderReader(viaFollower, kv)
	if err != nil {
		t.Fatalf("reader: %v", err)
	}
	listed := map[string]string{}
	err = listUnder(ctx, viaFollower, read, replayKV{KeyValue: kv, pass: behind},
		jetstream.AllKeys, "the bucket", func(kve jetstream.KeyValueEntry) error {
			listed[kve.Key()] = string(kve.Value())
			return nil
		})
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if want := map[string]string{"kept": "1", "recreated": "2"}; !maps.Equal(listed, want) {
		t.Fatalf("listed %v, want %v: the re-created key is live throughout "+
			"the listing, and a replica's tombstone for it is not its answer", listed, want)
	}
}
