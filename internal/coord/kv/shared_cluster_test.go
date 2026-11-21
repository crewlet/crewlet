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
// no copy of the bucket at all. A direct get is answered by a replica, which
// acknowledges nothing and can be a write behind the quorum, so a claim read
// back through one reads as never made; a leaf node without `data` has no
// copy, so every read it makes goes to a picked replica. FIVE MEMBERS AND
// THREE-REPLICA BUCKETS put two members, with the handles on them, on no copy
// of the lease bucket, so no read is local and the case asks which copy
// answers: every read a caller acts on must be the stream LEADER's.
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
			Clustered: true, Replicas: 3, Reach: InProcess,
		}
		first, err := Open(t.Context(), client(t, 0), cfg)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		info := streamOf(t.Context(), t, first.js, first.leases.stream)
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

// THE BROWSER CONTROL SUITE WITH ITS HANDLES ON NO COPY OF THE DUTY BUCKET,
// where the class lives, for TestSharedContractOnACluster's reason: every
// answer a take, a renewal, an end or a read gives must be the leader's. The
// members are chosen by the duty bucket's holders, which say nothing of where
// the lease bucket's are.
func TestBrowserControlContractOnACluster(t *testing.T) {
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
	coordtest.RunBrowserControls(t, func(t *testing.T) []coordtest.BrowserControlStore {
		cfg := Config{
			TTL: coordtest.LongTTL, BucketPrefix: fmt.Sprintf("s%d", bucketSeq.Add(1)),
			Clustered: true, Replicas: 3, Reach: InProcess,
		}
		first, err := Open(t.Context(), client(t, 0), cfg)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		info := streamOf(t.Context(), t, first.js, first.duties.stream)
		holders := []string{info.Cluster.Leader}
		for _, peer := range info.Cluster.Replicas {
			holders = append(holders, peer.Name)
		}
		var out []coordtest.BrowserControlStore
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
		// THE PREMISE, as the shared case's: a handle on a member holding a
		// copy reads locally, and this certifies the easy position.
		if len(out) != len(c.Configs)-cfg.Replicas {
			t.Fatalf("the duty bucket is held by %v, leaving %d member(s) with no copy, want %d",
				holders, len(out), len(c.Configs)-cfg.Replicas)
		}
		return out
	})
}

// A READ A CALLER ACTS ON IS NEVER ANSWERED BY A COPY THAT IS BEHIND — a lease
// or a fleet record the quorum holds is unknown through a member cut off from
// it, never absent. Its own direct get answers "not found" for the majority's
// write (proved first, or the case passes against the read it rules out),
// which a lease read would pass on as "definitively not held" and a seat host
// as "shed this seat now"; through the leader the same reads FAIL, and the
// node keeps what it holds and waits.
//
// THE CUT MEMBER LEADS NEITHER STREAM THE CASE READS: a cut leader answers
// leader reads from its own copy until it notices (package doc, "Every
// single-key read is the leader's", about ten seconds), a window the broker
// gives no guarantee inside.
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
	cfg := Config{TTL: coordtest.LongTTL, BucketPrefix: "cut", Clustered: true, Replicas: 3, Reach: InProcess}
	fleetCfg := FleetConfig{
		BucketPrefix: "cutfleet", Clustered: true, Replicas: 3, Reach: InProcess,
		MaxRateWindow: time.Minute, MaxClaimTTL: 10 * time.Minute,
		LedgerRetention: 10 * time.Minute, FireRetention: 10 * time.Minute,
		FollowRetention: 10 * time.Minute, BudgetRetention: time.Minute, RebaseRetention: 10 * time.Minute,
		StatusFreshness:  10 * time.Minute,
		CustodyRetention: 10 * time.Minute,
	}

	// The lease bucket's leader, and a member that leads neither the lease
	// bucket nor the mailbox bucket — the one about to be cut, with a handle
	// on it. Three members and two streams always leave one.
	first, err := Open(ctx, client(0), cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	fleetFirst, err := OpenFleet(ctx, client(0), fleetCfg)
	if err != nil {
		t.Fatalf("OpenFleet: %v", err)
	}
	leaderOf := func(stream string) string {
		t.Helper()
		info := streamOf(ctx, t, first.js, stream)
		if info.Cluster.Leader == "" {
			t.Fatalf("stream %s's placement names no leader: %+v", stream, info.Cluster)
		}
		return info.Cluster.Leader
	}
	leaseLeader := leaderOf(first.leases.stream)
	mailboxLeader := leaderOf(bucketStream(fleetFirst.mailboxes))
	leader, cut := -1, -1
	for i, member := range c.Configs {
		switch {
		case member.ServerName == leaseLeader:
			leader = i
		case member.ServerName != mailboxLeader && cut < 0:
			cut = i
		}
	}
	if leader < 0 || cut < 0 {
		t.Fatalf("the lease bucket is led by %q and the mailbox bucket by %q, which leaves "+
			"no member leading neither", leaseLeader, mailboxLeader)
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
	// The majority's writes, straight after the cut, retried through the
	// election a group the cut member led needs, each on a FRESH resource: a
	// claim that failed part way while the lease bucket had no leader cannot
	// give its claiming record back, which holds the resource until its TTL.
	var (
		lease    *coord.Lease
		resource string
	)
	deadline := time.Now().Add(60 * time.Second)
	for attempt := 0; ; attempt++ {
		resource = coord.ClassSeat.Resource(fmt.Sprintf("claimed-after-the-cut-%d", attempt))
		lease, _, err = onLeader.TryAcquire(ctx, resource, coord.AcquireOptions{
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
		_, created, err := fleetOnLeader.CreateMailbox(ctx, coord.MailboxRecord{
			Seat: mailboxSeat("ana"), Handle: "ana",
		})
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
	_, found, err := fleetOnCut.Mailbox(readCtx, mailboxSeat("ana"))
	cancel()
	if err == nil {
		t.Errorf("a fleet record read through the cut member answered found=%v "+
			"with no error, for a record the quorum holds", found)
	}
	if err != nil && !errors.Is(err, coord.ErrUnavailable) {
		t.Errorf("the refusal is %v, want it to carry coord.ErrUnavailable", err)
	}
}
