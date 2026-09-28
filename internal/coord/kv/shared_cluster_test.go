package kv

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/coordtest"
	"github.com/crewlet/crewlet/internal/jsapi"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/jetstreamtest"
)

// THE SHARED CASES WITH THEIR HANDLES WHERE MOST OF A FLEET'S NODES ARE: on
// no copy of the bucket at all.
//
// A handle's reads were the bucket handle's own Get, a DIRECT get, which the
// broker answers from a replica — the copy on the handle's own member when
// there is one, and otherwise whichever replica it picks. A replica
// acknowledges nothing, so it can be a write behind the quorum that
// acknowledged the claim; read back through it, the claim's own write read as
// never made, and the claim answered (nil, nil) for a lease it held. A node
// without the `data` role is a leaf with no copy of anything, so every one of
// its reads went to a picked replica — which is where the fleet measured it.
//
// FIVE MEMBERS AND THREE-REPLICA BUCKETS put two members on no copy of the
// lease bucket, which is that position without a leaf: the handles are on
// them, so no read can be answered locally, and the case asks which copy
// answers. Every read a caller acts on is now the stream LEADER's.
func TestSharedContractOnACluster(t *testing.T) {
	t.Parallel()
	c := jetstreamtest.StartCluster(t, 5, js.Config{})
	client := func(t *testing.T, i int) jetstream.JetStream {
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
	coordtest.RunShared(t, func(t *testing.T) []coord.Backend {
		cfg := Config{
			TTL: coordtest.LongTTL, BucketPrefix: fmt.Sprintf("s%d", bucketSeq.Add(1)),
			Clustered: true, Replicas: 3,
		}
		first, err := Open(t.Context(), client(t, 0), cfg)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		stream, err := first.js.Stream(t.Context(), first.leases.stream)
		if err != nil {
			t.Fatalf("the lease bucket's stream: %v", err)
		}
		info, err := stream.Info(t.Context())
		if err != nil || info.Cluster == nil {
			t.Fatalf("the lease bucket's placement: (%v, %v)", info, err)
		}
		holders := []string{info.Cluster.Leader}
		for _, peer := range info.Cluster.Replicas {
			holders = append(holders, peer.Name)
		}
		var out []coord.Backend
		for i, member := range c.Configs {
			if slices.Contains(holders, member.ServerName) {
				continue
			}
			s, err := Open(t.Context(), client(t, i), cfg)
			if err != nil {
				t.Fatalf("member %d: Open: %v", i, err)
			}
			out = append(out, s)
		}
		// THE PREMISE: without a handle on a member holding no copy,
		// every read is answered locally and this case certifies the
		// easy position twice.
		if len(out) != len(c.Configs)-cfg.Replicas {
			t.Fatalf("the lease bucket is held by %v, leaving %d member(s) with "+
				"no copy, want %d", holders, len(out), len(c.Configs)-cfg.Replicas)
		}
		return out
	})
}

// A READ A CALLER ACTS ON IS NEVER ANSWERED BY A COPY THAT IS BEHIND — a lease
// or a fleet record the quorum holds is unknown through a member cut off from
// it, never absent.
//
// The deterministic form of what the shared case above catches as a rate. A
// member cut off from its peers is "behind" for certain: the majority commits
// a write, and the cut member never receives it. Its own direct get then
// answers "not found" for that write — this case proves that premise first,
// or it would pass against the very read it rules out — and a lease read that
// took that answer told its caller "definitively not held" about a lease the
// store held: Get answered nil, and Renew answered false, which a seat host
// reads as "shed this seat now". Through the leader the same reads FAIL, which
// the contract's third answer is for: a node that cannot reach its store
// keeps what it holds and waits.
func TestAReadIsNeverAnsweredByACopyThatIsBehind(t *testing.T) {
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
	cfg := Config{TTL: coordtest.LongTTL, BucketPrefix: "cut", Clustered: true, Replicas: 3}
	fleetCfg := FleetConfig{
		BucketPrefix: "cutfleet", Clustered: true, Replicas: 3,
		RateWindow: time.Minute, ClaimTTL: 10 * time.Minute,
		LedgerRetention: 10 * time.Minute, FireRetention: 10 * time.Minute,
		FollowRetention: 10 * time.Minute, CooldownMax: time.Hour,
		StatusFreshness: 10 * time.Minute,
	}

	// The lease bucket's leader, and a member that is not it — the one about
	// to be cut, with a handle on it.
	first, err := Open(ctx, client(0), cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	stream, err := first.js.Stream(ctx, first.leases.stream)
	if err != nil {
		t.Fatalf("the lease bucket's stream: %v", err)
	}
	info, err := stream.Info(ctx)
	if err != nil || info.Cluster == nil {
		t.Fatalf("the lease bucket's placement: (%v, %v)", info, err)
	}
	leader, cut := -1, -1
	for i, member := range c.Configs {
		switch {
		case member.ServerName == info.Cluster.Leader:
			leader = i
		case cut < 0:
			cut = i
		}
	}
	if leader < 0 || cut < 0 {
		t.Fatalf("the lease bucket is led by %q, which is none of the members", info.Cluster.Leader)
	}
	onLeader, err := Open(ctx, client(leader), cfg)
	if err != nil {
		t.Fatalf("Open on the leader: %v", err)
	}
	onCut, err := Open(ctx, client(cut), cfg)
	if err != nil {
		t.Fatalf("Open on the member about to be cut: %v", err)
	}
	fleetOnLeader, err := OpenFleet(ctx, client(leader), fleetCfg)
	if err != nil {
		t.Fatalf("OpenFleet on the leader: %v", err)
	}
	fleetOnCut, err := OpenFleet(ctx, client(cut), fleetCfg)
	if err != nil {
		t.Fatalf("OpenFleet on the member about to be cut: %v", err)
	}

	c.Partition(t, cut)
	const owner = "node-a/1"
	// The majority's writes, straight after the cut. A group the cut member
	// led has to elect again among the two that remain, so the writes are
	// retried through that — each on a FRESH resource, because a claim that
	// failed part way leaves its record claiming under this owner until its
	// TTL, and the next claim of the same resource waits on it.
	var (
		lease    *coord.Lease
		resource string
	)
	deadline := time.Now().Add(60 * time.Second)
	for attempt := 0; ; attempt++ {
		resource = coord.ClassSeat.Resource(fmt.Sprintf("claimed-after-the-cut-%d", attempt))
		lease, err = onLeader.TryAcquire(ctx, resource, coord.AcquireOptions{
			Owner: owner, TTL: coordtest.LongTTL, Ungated: true,
		})
		if err == nil && lease != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the majority could not claim after the cut: (%v, %v)", lease, err)
		}
		time.Sleep(250 * time.Millisecond)
	}
	for {
		_, created, err := fleetOnLeader.CreateMailbox(ctx, coord.MailboxRecord{Handle: "ana"})
		if err == nil && created {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the majority could not write a fleet record after the cut: (%v, %v)",
				created, err)
		}
		time.Sleep(250 * time.Millisecond)
	}

	// THE PREMISE: the cut member's own direct get answers from a copy
	// without the write. A member joins the direct-get group only on a
	// timer after it first follows a leader, so until it has, the read
	// finds no responder — no answer rather than a wrong one — and that is
	// waited out.
	premise := time.Now().Add(30 * time.Second)
	for {
		directCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		_, err := onCut.leases.kv.Get(directCtx, encodeResource(resource))
		cancel()
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			break
		}
		if time.Now().After(premise) {
			t.Fatalf("the cut member's direct get of a lease the quorum holds "+
				"answered %v, want ErrKeyNotFound — without a copy that answers "+
				"from behind, this case exercises nothing", err)
		}
		time.Sleep(250 * time.Millisecond)
	}

	within := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(ctx, 3*time.Second)
	}
	readCtx, cancel := within()
	got, err := onCut.Get(readCtx, resource)
	cancel()
	if err == nil {
		t.Errorf("Get through the cut member answered %+v with no error, for a "+
			"lease the quorum holds at epoch %d — a copy that is behind answered "+
			"it", got, lease.Epoch)
	}
	readCtx, cancel = within()
	renewed, err := onCut.Renew(readCtx, resource, owner, lease.Epoch, coordtest.LongTTL)
	cancel()
	if err == nil {
		t.Errorf("Renew through the cut member answered %v with no error, for a "+
			"lease the quorum holds — false tells a seat host to shed the seat", renewed)
	}
	readCtx, cancel = within()
	released, err := onCut.Release(readCtx, resource, owner, lease.Epoch)
	cancel()
	if err == nil {
		t.Errorf("Release through the cut member answered %v with no error, for "+
			"a lease the quorum holds", released)
	}
	readCtx, cancel = within()
	_, found, err := fleetOnCut.Mailbox(readCtx, "ana")
	cancel()
	if err == nil {
		t.Errorf("a fleet record read through the cut member answered found=%v "+
			"with no error, for a record the quorum holds", found)
	}
	if err != nil && !errors.Is(err, coord.ErrUnavailable) {
		t.Errorf("the refusal is %v, want it to carry coord.ErrUnavailable", err)
	}
}
