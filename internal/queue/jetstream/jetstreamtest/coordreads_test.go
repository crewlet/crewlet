package jetstreamtest

import (
	"context"
	"encoding/json"
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
// THE MEMBER IS ASKED IN PROCESS ([js.Server.LeadsStream]), never over the
// network. A member that does not lead a stream answers a request about it
// with silence, so over the network "no" could only be read off a timeout —
// and a leader slow to answer on a loaded machine then read as a member that
// did not lead, and failed the case over a read it was entitled to answer.
// In process the answer is the predicate the server itself asks before it
// answers a leader-only read, so a "no" is a no.
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
	// leaders among three members. Leadership may still move before the cut
	// lands — [refused] waits out a member that believes it leads.
	cut := slices.IndexFunc(c.Servers, func(s *js.Server) bool {
		return !slices.ContainsFunc(streams, s.LeadsStream)
	})
	if cut < 0 {
		t.Fatalf("every member leads one of %v, which two streams cannot do", streams)
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
	// AND THE CLIENT'S OWN HANDLES ON BOTH BUCKETS, bound through the same
	// member while it can still reach a leader: binding reads the stream's
	// configuration, and the handle keeps the allow_direct it read — which
	// is what makes its Get the direct get the control below asks.
	budgets, runs := clientBucket(ctx, t, conns[cut], "behind_budgets"),
		clientBucket(ctx, t, conns[cut], "behind_sandbox_runs")

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

	// THE CONTROL: the client's own read through the cut member answers, and
	// answers from before the majority's writes. Without it this case
	// would pass for a reason that has nothing to do with whose copy a read
	// asks — a follower joins the direct-get group only on a two-second
	// check of how far it has caught up, so a member cut early enough
	// refuses EVERY read with "no responders", the client's own included,
	// and a store reading through the client would pass here too.
	var counter struct {
		Used int `json:"used"`
	}
	if v := staleThrough(t, budgets, coord.DocumentKey(scope)); json.Unmarshal(v, &counter) != nil ||
		counter.Used != 1 {
		t.Fatalf("the client's own read of the counter through the cut member answered %s, "+
			"want the count of 1 it held before the majority's charge", v)
	}
	// The run is gone on the majority, so ANY answer is from before that.
	staleThrough(t, runs, coord.DocumentKey(turn))

	member := c.Servers[cut]
	name := c.Configs[cut].ServerName
	refused(t, member, name, streams[0], "Used", func(ctx context.Context) (any, error) {
		return far.Used(ctx, scope)
	})
	refused(t, member, name, streams[1], "SandboxRun", func(ctx context.Context) (any, error) {
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
func refused(t *testing.T, srv *js.Server, member, stream, what string,
	read func(context.Context) (any, error)) {

	t.Helper()
	// Past the server's lost-quorum interval twice over: it is checked every
	// ten seconds and trips ten seconds after the last word from a quorum.
	deadline := time.Now().Add(45 * time.Second)
	for {
		// ASKED FIRST: a member cut off can only stop leading, never
		// start, so "does not lead" here holds for the read after it.
		leads := srv.LeadsStream(stream)
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

// clientBucket binds the client's own handle on a bucket through nc.
func clientBucket(ctx context.Context, t *testing.T, nc *nats.Conn, bucket string) jetstream.KeyValue {
	t.Helper()
	jsc, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	kv, err := jsc.KeyValue(ctx, bucket)
	if err != nil {
		t.Fatalf("bind %s: %v", bucket, err)
	}
	return kv
}

// staleThrough waits until the client's own read of key through kv — a direct
// get, which on a member cut off from its cluster only that member can serve —
// is answered, and returns what it said.
//
// THE WAIT IS THE SERVER'S. A follower joins the direct-get group once a
// two-second check finds it within 90% of the leader's commit
// (server/jetstream_cluster.go, monitorStream), and a member cut off has
// nothing left to apply, so it joins on its next check and is never dropped.
// Thirty seconds is fifteen of those checks.
func staleThrough(t *testing.T, kv jetstream.KeyValue, key string) []byte {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		e, err := kv.Get(ctx, key)
		cancel()
		switch {
		case err == nil:
			return e.Value()
		case time.Now().After(deadline):
			t.Fatalf("the client's own read of %s in %s through the cut member never "+
				"answered (%v), so this case cannot tell a read the leader answers from "+
				"one nobody does", key, kv.Bucket(), err)
		}
		time.Sleep(250 * time.Millisecond)
	}
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
