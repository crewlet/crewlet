package jetstreamtest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
	coordkv "github.com/crewlet/crewlet/internal/coord/kv"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
)

// A COORDINATION READ IS NEVER ANSWERED BY A MEMBER THAT IS BEHIND.
//
// Every bucket the fleet shares is replicated, and only the stream leader's
// copy is sure to hold every acknowledged write. The client's own read is a
// DIRECT GET, served by whichever replica the server hands the request to —
// and a member that is behind answers it from its own copy, as definitely as
// a current one would. That is how a counter read straight after a charge
// came back short and a run just written read back as absent on a fleet under
// load, a few times in a hundred and never on one server.
//
// A race reproduces that a few times in a hundred, so this case stages the
// limit of it instead: one member cut off from the other two, which go on
// writing. The cut member is as far behind as a member can be and still
// running, and the buckets' own allow_direct is ON — the configuration every
// bucket an earlier build created still carries. Asked through that member, a
// read may only say it could not tell.
//
// # The one member that may still answer, and for how long
//
// The leader read has a residue the server owns: a LEADER cut off from its
// quorum goes on believing it leads until it notices the quorum is gone —
// nats-server's lost-quorum interval, ten seconds checked every ten
// (server/raft.go) — and answers from its own copy meanwhile, while every
// write through it fails. So the member cut here is one that led neither
// bucket, and each read is preceded by asking that member whether it believes
// it leads: a member that does not can never come to (it has no quorum to win
// an election), so a read it answers after saying so is a non-leader's
// answer, which is the failure. One that still believes it leads is waited
// out, and the case holds it to stopping.
//
// Mutation: answer [coordkv.FleetStore]'s point reads through the client's
// own Get rather than the leader's, and the cut member answers both from what
// it had.
func TestACoordinationReadIsNeverAnsweredByAMemberThatIsBehind(t *testing.T) {
	t.Parallel()
	c := StartPartitionableCluster(t, 3, js.Config{})
	for i := range c.Servers {
		awaitRoutes(t, c, i, 2)
	}
	cfg := coordkv.FleetConfig{
		BucketPrefix: "behind", Replicas: len(c.Servers), Clustered: true,
		RateWindow: time.Minute, ClaimTTL: 10 * time.Minute, SetupOnceRetention: 10 * time.Minute,
		LedgerRetention: 10 * time.Minute, FireRetention: 10 * time.Minute,
		FollowRetention: 10 * time.Minute, CooldownMax: time.Hour,
		StatusFreshness: 10 * time.Minute,
	}
	conns := make([]*nats.Conn, len(c.Servers))
	stores := make([]*coordkv.FleetStore, len(c.Servers))
	for i := range c.Servers {
		conns[i] = c.Client(t, i).Conn()
		store, err := coordkv.OpenFleet(t.Context(), conns[i], cfg)
		if err != nil {
			t.Fatalf("open the fleet store on member %d: %v", i, err)
		}
		stores[i] = store
	}
	// The two buckets read below, by the stream behind each.
	streams := []string{"KV_behind_budgets", "KV_behind_sandbox_runs"}

	ctx := t.Context()
	scope := coord.AgentScope("behind")
	const turn = "turn-behind"
	if _, err := stores[0].Charge(ctx, scope, 1, 0, 0); err != nil {
		t.Fatalf("charge before the cut: %v", err)
	}
	if _, err := stores[0].CreateSandboxRun(ctx, turn, []byte(`{"n":1}`)); err != nil {
		t.Fatalf("create the run before the cut: %v", err)
	}

	// CUT A MEMBER THAT LEADS NEITHER BUCKET: two streams have at most two
	// leaders among three members.
	leaders := make([]string, 0, len(streams))
	for _, stream := range streams {
		leaders = append(leaders, leaderOf(ctx, t, conns[0], stream))
	}
	cut := -1
	for i := range c.Servers {
		if !slices.Contains(leaders, c.Configs[i].ServerName) {
			cut = i
			break
		}
	}
	if cut < 0 {
		t.Fatalf("every member leads one of %v (%v), which two streams cannot do", streams, leaders)
	}
	near, far := stores[(cut+1)%len(stores)], stores[cut]

	// BOTH RECORDS READ THROUGH THE MEMBER ABOUT TO BE CUT, so what is
	// asserted below is about the cut and not about a read that never
	// worked from there.
	if used, err := far.Used(ctx, scope); err != nil || used != 1 {
		t.Fatalf("Used through member %d before the cut = (%d, %v), want 1", cut, used, err)
	}
	if _, found, err := far.SandboxRun(ctx, turn); err != nil || !found {
		t.Fatalf("SandboxRun through member %d before the cut = (found=%t, %v), want it found",
			cut, found, err)
	}

	c.Partition(t, cut)
	awaitRoutes(t, c, cut, 0)

	// WRITES THE CUT MEMBER NEVER HEARS OF, retried: the pair left answers
	// only once each bucket has a leader it can reach — see
	// TestAMajoritySurvivesAPartition.
	retry(t, "charge again on the majority", func() error {
		_, err := near.Charge(ctx, scope, 1, 0, 0)
		return err
	})
	retry(t, "remove the run on the majority", func() error {
		rec, found, err := near.SandboxRun(ctx, turn)
		switch {
		case err != nil:
			return err
		case !found:
			return nil
		}
		removed, err := near.DeleteSandboxRun(ctx, turn, rec.Version)
		if err == nil && !removed {
			err = errors.New("the delete lost its race")
		}
		return err
	})

	name := c.Configs[cut].ServerName
	refused(t, conns[cut], name, streams[0], "Used", func(ctx context.Context) (any, error) {
		return far.Used(ctx, scope)
	})
	refused(t, conns[cut], name, streams[1], "SandboxRun", func(ctx context.Context) (any, error) {
		_, found, err := far.SandboxRun(ctx, turn)
		return found, err
	})
}

// refused asks read through a member cut off from its cluster until the
// member refuses it, failing on any answer given while the member did not
// believe it led the stream behind the read.
//
// See [TestACoordinationReadIsNeverAnsweredByAMemberThatIsBehind] for why a
// member that believes it leads is waited out rather than failed.
func refused(t *testing.T, nc *nats.Conn, member, stream, what string,
	read func(context.Context) (any, error)) {

	t.Helper()
	// Past the server's lost-quorum interval twice over: it is checked every
	// ten seconds and trips ten seconds after the last word from a quorum.
	deadline := time.Now().Add(45 * time.Second)
	for {
		// ASKED FIRST: a member cut off can only stop leading, never
		// start, so "does not lead" here holds for the read after it.
		leads := believesItLeads(t, nc, member, stream)
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		answer, err := read(ctx)
		cancel()
		switch {
		case err != nil && !errors.Is(err, coord.ErrUnavailable):
			t.Fatalf("%s through the cut member = %v, want it to carry coord.ErrUnavailable",
				what, err)
		case err != nil:
			return
		case !leads:
			t.Fatalf("%s through %s, cut off from its cluster and not leading %s, answered "+
				"%v from its own copy rather than saying it could not tell", what, member,
				stream, answer)
		case time.Now().After(deadline):
			t.Fatalf("%s through %s was still answered (%v) 45s after the cut: a leader "+
				"that has lost its quorum must stop answering", what, member, answer)
		}
		t.Logf("%s through %s answered %v while it still believed it led %s — the "+
			"server's lost-quorum window; asking again", what, member, answer, stream)
		time.Sleep(500 * time.Millisecond)
	}
}

// leaderOf is the member a stream's leader is, as the cluster reports it.
func leaderOf(ctx context.Context, t *testing.T, nc *nats.Conn, stream string) string {
	t.Helper()
	jsc, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	s, err := jsc.Stream(ctx, stream)
	if err != nil {
		t.Fatalf("read %s: %v", stream, err)
	}
	if s.CachedInfo().Cluster == nil || s.CachedInfo().Cluster.Leader == "" {
		t.Fatalf("%s reports no leader", stream)
	}
	return s.CachedInfo().Cluster.Leader
}

// believesItLeads reports whether member answers for stream as its leader.
// Only the member that believes it leads answers a stream's state, so any
// other outcome — a refusal, or nothing before the deadline — is "no".
func believesItLeads(t *testing.T, nc *nats.Conn, member, stream string) bool {
	t.Helper()
	jsc, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	s, err := jsc.Stream(ctx, stream)
	if err != nil {
		return false
	}
	return s.CachedInfo().Cluster != nil && s.CachedInfo().Cluster.Leader == member
}

// retry runs op until it succeeds, for as long as a majority takes to elect.
func retry(t *testing.T, what string, op func() error) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		if last = op(); last == nil {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatal(fmt.Errorf("%s: the majority never answered: %w", what, last))
}
