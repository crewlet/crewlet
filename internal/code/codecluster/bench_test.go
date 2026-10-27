package codecluster_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codetest"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/statelog"
)

// BenchmarkCommitChain is ONE REPOSITORY'S COMMIT CHAIN, end to end: a
// proposal to the repository's committer, the batch it forms, the publisher's
// snapshot, decide and append, the committer node's applier, WaitApplied, and
// the answer read from the push's row — the per-repository ceiling every push
// meets after its receiving node has stored its bundle, and the term group
// commit amortises.
//
// Every push of an iteration creates a distinct ref with NO PACK: the decide
// checks refs, protections and limits and never reads an object, so the chain
// is measured without the object store, whose cost a push pays on the
// receiving node before it proposes (BenchmarkPushSmall). The brokers fsync
// every write, production's default stream.sync.
//
//   - members=1 is one data node on one embedded broker, proposed to in
//     process.
//   - members=3 is the codecluster topology, three data nodes of one broker
//     cluster sharing one machine. via=committer hands each push to the
//     repository's committer member in process; via=router proposes it
//     through a member that is not the committer, through its production
//     estate.Router — the keyed hop every push received elsewhere pays.
//   - batch is how many pushes are proposed at once and waited for together:
//     1 is a lone push, 8 a burst, 64 the most one record carries
//     (code.MaxPushesPerRecord), 256 four records' worth, and 1 024 a full
//     inbox (code.CommitterInbox) — ns/op of that arm is how long the queue
//     a committer holds takes to drain.
//   - repos=16|64 spreads an iteration's pushes over that many repositories
//     declared up front, so their committers spread over the three members
//     by rendezvous and every member's publisher and write lane carry several
//     repositories' records at once — the fleet's figure, against repos=1 at
//     the same pushes. Spread over repositories every push is a record of its
//     own (a record carries one repository's pushes), so batch=64 over 64
//     repositories is sixty-four single-push records at once: what ONE code
//     log appends a second, the company's ceiling for pushes that do not
//     share a repository.
//
// Each arm reports pushes and records a second, pushes a record, and the p50
// and p99 of one push's latency, from its proposal to its answer. An arm stops
// itself before its repository's refs would pass code.MaxRefsPerRepo, which
// would start measuring refusals. internal/code's doc records the figures.
func BenchmarkCommitChain(b *testing.B) {
	codetest.Quiet(b)
	b.Run("members=1", func(b *testing.B) {
		d, declare := soloChain(b)
		for _, batch := range []int{1, 8, code.MaxPushesPerRecord} {
			b.Run(fmt.Sprintf("batch=%d", batch), func(b *testing.B) {
				repos := declare(b, 1)
				chain(b, batch, repos, d.Served().Propose, func() int { return d.Appends() })
			})
		}
	})
	b.Run("members=3", func(b *testing.B) {
		c := startClusterOf(b, "nats", js.Config{SyncAlways: true})
		appends := func() int {
			n := 0
			for _, m := range c.members {
				n += m.data.Appends()
			}
			return n
		}
		for _, via := range []string{"committer", "router"} {
			for _, batch := range []int{1, 8, code.MaxPushesPerRecord} {
				b.Run(fmt.Sprintf("via=%s/batch=%d", via, batch), func(b *testing.B) {
					repos := clusterRepos(b, c, 1)
					chain(b, batch, repos, c.proposer(b, via, repos[0]), appends)
				})
			}
		}
		// A FULL INBOX: more than one record holds, so records fill to
		// MaxPushesPerRecord, and at CommitterInbox the whole queue a
		// committer holds — an iteration's ns/op is its drain.
		for _, batch := range []int{4 * code.MaxPushesPerRecord, code.CommitterInbox} {
			b.Run(fmt.Sprintf("via=committer/batch=%d", batch), func(b *testing.B) {
				repos := clusterRepos(b, c, 1)
				chain(b, batch, repos, c.proposer(b, "committer", repos[0]), appends)
			})
		}
		for _, arm := range []struct{ batch, repos int }{{8, 1}, {8, 16}, {64, 16}, {64, 64}} {
			b.Run(fmt.Sprintf("via=router/batch=%d/repos=%d", arm.batch, arm.repos), func(b *testing.B) {
				repos := clusterRepos(b, c, arm.repos)
				// THROUGH A ROUTER EVERY REPOSITORY'S COMMITTER IS ASKED
				// FROM: one member proposes every push, so the pushes of a
				// repository it commits for itself are answered in process
				// and the rest cross the wire, as a receiving node's are.
				chain(b, arm.batch, repos, c.members[0].router.Load().Code().Propose, appends)
			})
		}
	})
}

// chain is one arm: per iteration, batch pushes proposed at once through
// propose — the k-th to repos[k mod len(repos)] — each creating a ref of its
// own, and waited for until every one is applied with its command ok.
func chain(b *testing.B, batch int, repos []string, propose func(context.Context, code.Proposal) (code.ProposeResult, error),
	appends func() int) {

	b.Helper()
	var (
		mu   sync.Mutex
		took []time.Duration
	)
	pushes := 0
	before := appends()
	for b.Loop() {
		// A REPOSITORY'S REFS STOP AT code.MaxRefsPerRepo, where a create
		// is refused: an arm past it would be measuring refusals.
		if per := (pushes + batch + len(repos) - 1) / len(repos); per >= code.MaxRefsPerRepo {
			b.Fatalf("this arm would create %d refs in one repository, past code.MaxRefsPerRepo (%d): "+
				"run it for less time", per, code.MaxRefsPerRepo)
		}
		var wg sync.WaitGroup
		errs := make([]error, batch)
		for k := range batch {
			p := code.Proposal{
				PushID: uuid.Must(uuid.NewV7()).String(), RepoID: repos[(pushes+k)%len(repos)],
				Principal: code.Principal{Kind: code.PrincipalOperator, OperatorID: "bench", Seat: "ceo"},
				Via:       code.ViaGit, Node: "bench", ReceivedAt: time.Now().UTC(),
				Commands: []code.Command{{Ref: fmt.Sprintf("refs/heads/b%d-%d", pushes, k), Old: code.ZeroOID,
					New: randomOID()}},
			}
			wg.Go(func() {
				at := time.Now()
				res, err := propose(b.Context(), p)
				elapsed := time.Since(at)
				switch {
				case err != nil:
					errs[k] = err
				case res.Outcome != statelog.OutcomeApplied || len(res.Answer.Commands) != 1 ||
					res.Answer.Commands[0].Result != code.ResultOK:
					errs[k] = fmt.Errorf("push %s was answered %+v, want applied and ok", p.PushID, res)
				}
				mu.Lock()
				took = append(took, elapsed)
				mu.Unlock()
			})
		}
		wg.Wait()
		for _, err := range errs {
			if err != nil {
				b.Fatal(err)
			}
		}
		pushes += batch
	}
	records := appends() - before
	elapsed := b.Elapsed().Seconds()
	slices.Sort(took)
	b.ReportMetric(float64(pushes)/elapsed, "pushes/s")
	b.ReportMetric(float64(records)/elapsed, "records/s")
	b.ReportMetric(float64(pushes)/float64(max(records, 1)), "pushes/record")
	b.ReportMetric(quantile(took, 0.50), "p50-ms")
	b.ReportMetric(quantile(took, 0.99), "p99-ms")
}

// soloChain is one data node on an embedded broker of its own that fsyncs
// every write, and a declare of n fresh repositories on it.
func soloChain(b *testing.B) (*codetest.DataNode, func(b *testing.B, n int) []string) {
	b.Helper()
	dir := b.TempDir()
	q, err := js.Open(b.Context(), js.Config{StoreDir: filepath.Join(dir, "stream"), SyncAlways: true})
	if err != nil {
		b.Fatalf("open the broker: %v", err)
	}
	// REGISTERED FIRST, SO IT RUNS LAST: the data node's teardown stops it
	// before its broker goes.
	b.Cleanup(func() { _ = q.Stop(context.Background()) })
	d := codetest.StartDataNode(b, codetest.DataOptions{ID: "solo", Queue: q, Dir: dir, LogBytes: codeLogBytes})
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	activations := 0
	return d, func(b *testing.B, n int) []string {
		b.Helper()
		declared := make([]code.Declared, n)
		for i := range declared {
			declared[i] = code.Declared{Name: "chain-" + uuid.NewString()[:8], Settings: codetest.Settings()}
		}
		activations++
		d.Declare(b, base.Add(time.Duration(activations)*time.Second), declared...)
		ids := make([]string, n)
		for i, r := range declared {
			ref, err := d.Served().Resolve(b.Context(), code.ResolveQuery{Name: r.Name, Fresh: codetest.Linearizable})
			if err != nil {
				b.Fatalf("resolve %s: %v", r.Name, err)
			}
			ids[i] = ref.ID
		}
		return ids
	}
}

// clusterRepos declares n fresh repositories on c and answers their ids, every
// member level with the declarations and drained, so each committer decides
// its first push from a copy that answers.
func clusterRepos(b *testing.B, c *cluster, n int) []string {
	b.Helper()
	declared := make([]code.Declared, n)
	for i := range declared {
		declared[i] = code.Declared{Name: "chain-" + uuid.NewString()[:8], Settings: codetest.Settings()}
	}
	c.Declare(b, declared...)
	if err := c.drained(b.Context()); err != nil {
		b.Fatal(err)
	}
	ids := make([]string, n)
	for i, r := range declared {
		rows, err := c.Rows(b.Context(), r.Name, codetest.Linearizable)
		if err != nil {
			b.Fatalf("the rows of %s: %v", r.Name, err)
		}
		ids[i] = rows.Repo.ID
	}
	return ids
}

// proposer is how a via= arm proposes a push to repo: to its committer member
// in process, or through the router of a member that is not its committer.
func (c *cluster) proposer(b *testing.B, via, repo string) func(context.Context, code.Proposal) (code.ProposeResult, error) {
	b.Helper()
	committer := c.member(b, c.Committer(repo))
	if via == "committer" {
		return committer.data.Served().Propose
	}
	for _, m := range c.members {
		if m != committer {
			return m.router.Load().Code().Propose
		}
	}
	b.Fatal("codecluster: no member but the committer")
	return nil
}

// randomOID is a random object id: a ref's target the decide never reads.
func randomOID() string {
	var raw [20]byte
	_, _ = rand.Read(raw[:])
	return hex.EncodeToString(raw[:])
}

// quantile is the nearest-rank q-quantile of sorted, in milliseconds.
func quantile(sorted []time.Duration, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := max(int(math.Ceil(q*float64(len(sorted)))), 1)
	return float64(sorted[rank-1]) / float64(time.Millisecond)
}
