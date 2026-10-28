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
	"github.com/crewlet/crewlet/internal/objstore"
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
//   - mix=packs and mix=compaction are a batch of 64 whose pushes each carry
//     a pack — named only: the decide reads no object — and mix=compaction
//     proposes, beside every batch after compactEvery more have landed, ONE
//     compaction of every pack landed since the last to the same committer,
//     which queues it among the pushes and decides it alone (internal/code's
//     "A compaction merges packs, decided alone"). The two differ by the
//     compactions alone: what a busy repository's compactions cost its
//     pushes — a record's slot each, its decide reading its inputs' rows,
//     beside a push batch's.
//
// Each arm reports pushes and records a second, pushes a record, and the p50
// and p99 of one push's latency, from its proposal to its answer; a
// mix=compaction arm the compactions it applied too. An arm stops itself
// before its repository's refs would pass code.MaxRefsPerRepo, which would
// start measuring refusals. internal/code's doc records the figures.
func BenchmarkCommitChain(b *testing.B) {
	codetest.Quiet(b)
	b.Run("members=1", func(b *testing.B) {
		d, declare := soloChain(b)
		for _, batch := range []int{1, 8, code.MaxPushesPerRecord} {
			b.Run(fmt.Sprintf("batch=%d", batch), func(b *testing.B) {
				repos := declare(b, 1)
				chain(b, batch, repos, d.Served().Propose, func() int { return d.Appends() }, nil)
			})
		}
		for _, compacting := range []bool{false, true} {
			b.Run(fmt.Sprintf("batch=%d/%s", code.MaxPushesPerRecord, mixName(compacting)), func(b *testing.B) {
				repos := declare(b, 1)
				chain(b, code.MaxPushesPerRecord, repos, d.Served().Propose, func() int { return d.Appends() },
					newMix(repos[0], compacting, d.Served().ProposeCompaction))
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
					chain(b, batch, repos, c.proposer(b, via, repos[0]), appends, nil)
				})
			}
		}
		for _, compacting := range []bool{false, true} {
			b.Run(fmt.Sprintf("via=committer/batch=%d/%s", code.MaxPushesPerRecord, mixName(compacting)),
				func(b *testing.B) {
					repos := clusterRepos(b, c, 1)
					committer := c.member(b, c.Committer(repos[0])).data.Served()
					chain(b, code.MaxPushesPerRecord, repos, committer.Propose, appends,
						newMix(repos[0], compacting, committer.ProposeCompaction))
				})
		}
		// A FULL INBOX: more than one record holds, so records fill to
		// MaxPushesPerRecord, and at CommitterInbox the whole queue a
		// committer holds — an iteration's ns/op is its drain.
		for _, batch := range []int{4 * code.MaxPushesPerRecord, code.CommitterInbox} {
			b.Run(fmt.Sprintf("via=committer/batch=%d", batch), func(b *testing.B) {
				repos := clusterRepos(b, c, 1)
				chain(b, batch, repos, c.proposer(b, "committer", repos[0]), appends, nil)
			})
		}
		for _, arm := range []struct{ batch, repos int }{{8, 1}, {8, 16}, {64, 16}, {64, 64}} {
			b.Run(fmt.Sprintf("via=router/batch=%d/repos=%d", arm.batch, arm.repos), func(b *testing.B) {
				repos := clusterRepos(b, c, arm.repos)
				// THROUGH A ROUTER EVERY REPOSITORY'S COMMITTER IS ASKED
				// FROM: one member proposes every push, so the pushes of a
				// repository it commits for itself are answered in process
				// and the rest cross the wire, as a receiving node's are.
				chain(b, arm.batch, repos, c.members[0].router.Load().Code().Propose, appends, nil)
			})
		}
	})
}

// compactEvery is a mix=compaction arm's cadence: one compaction for every 300
// pushes landed — internal/code/compact's steady arm's rate, a pass of the
// home's compactor a minute at 300 pushes a minute — merging every pack those
// pushes landed, as a pass whose roll is due merges what the minute added.
const compactEvery = 300

// compactionMix is a mix= arm's pushes and compactions on repo: every push
// carries a pack, and where compact is set — mix=compaction — the packs its
// pushes landed since the last compaction, and the newest position one landed
// at, are proposed through it once compactEvery of them have.
type compactionMix struct {
	repo    string
	compact func(context.Context, code.CompactionProposal) (code.CompactResult, error)

	mu     sync.Mutex
	landed []string
	seen   statelog.Position
	done   int
}

// newMix is a mix= arm's on repo: mix=compaction proposing through compact
// where compacting, mix=packs — the same pushes, no compaction — where not.
func newMix(repo string, compacting bool,
	compact func(context.Context, code.CompactionProposal) (code.CompactResult, error)) *compactionMix {

	m := &compactionMix{repo: repo}
	if compacting {
		m.compact = compact
	}
	return m
}

// mixName is the arm's name for a mix that compacts or does not.
func mixName(compacting bool) string {
	if compacting {
		return "mix=compaction"
	}
	return "mix=packs"
}

// land counts a push's pack as landed at at.
func (m *compactionMix) land(packID string, at statelog.Position) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.landed = append(m.landed, packID)
	// Every push lands on the code log, so the two compare.
	if later, err := m.seen.Before(at); m.seen.IsZero() || (err == nil && later) {
		m.seen = at
	}
}

// due is the compaction of every pack landed since the last, once
// compactEvery have — planned at the newest position one landed at, its one
// output holding every object of its inputs — and nil before, or in a mix
// that does not compact.
func (m *compactionMix) due() func(context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.compact == nil || len(m.landed) < compactEvery {
		return nil
	}
	retire, seen := slices.Clone(m.landed), m.seen
	m.landed = m.landed[:0]
	slices.Sort(retire)
	return func(ctx context.Context) error {
		cid := statelog.NewOpID(time.Now(), "")
		out := benchPack(code.DeriveOutputPackID(cid, 0), int64(len(retire)))
		res, err := m.compact(ctx, code.CompactionProposal{Compaction: code.Compaction{RepoID: m.repo,
			CompactionID: cid, Node: "bench", Seen: seen, Retire: retire, Add: []code.PackRef{*out}}})
		switch {
		case err != nil:
			return fmt.Errorf("the compaction of %d packs: %w", len(retire), err)
		case res.Outcome != statelog.OutcomeApplied:
			return fmt.Errorf("the compaction of %d packs was answered %+v, want applied", len(retire), res)
		}
		m.mu.Lock()
		m.done++
		m.mu.Unlock()
		return nil
	}
}

// benchPack is pack id's row as a mix= arm names it: objects
// objects, its bundle's sizes the exact ones a bundle of that many has
// (code.PackRef.Check), under a key minted now — named and never stored,
// since a decide reads no object.
func benchPack(id string, objects int64) *code.PackRef {
	return &code.PackRef{PackID: id, Object: objstore.KeyAt(time.Now()), Hash: objstore.HashOf([]byte(id)),
		Size:       32*objects + code.IndexBytesFor(objects) + code.ReverseBytesFor(objects),
		IndexBytes: code.IndexBytesFor(objects), ReverseBytes: code.ReverseBytesFor(objects),
		GitChecksum: randomOID(), Objects: objects}
}

// chain is one arm: per iteration, batch pushes proposed at once through
// propose — the k-th to repos[k mod len(repos)] — each creating a ref of its
// own, and waited for until every one is applied with its command ok. With a
// mix, every push carries a pack and the mix's due compaction is proposed
// beside the batch, waited for with it.
func chain(b *testing.B, batch int, repos []string, propose func(context.Context, code.Proposal) (code.ProposeResult, error),
	appends func() int, mix *compactionMix) {

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
		// The batch's pushes, and the slot past them the mix's compaction's.
		errs := make([]error, batch+1)
		for k := range batch {
			p := code.Proposal{
				PushID: uuid.Must(uuid.NewV7()).String(), RepoID: repos[(pushes+k)%len(repos)],
				Principal: code.Principal{Kind: code.PrincipalOperator, OperatorID: "bench", Seat: "ceo"},
				Via:       code.ViaGit, Node: "bench", ReceivedAt: time.Now().UTC(),
				Commands: []code.Command{{Ref: fmt.Sprintf("refs/heads/b%d-%d", pushes, k), Old: code.ZeroOID,
					New: randomOID()}},
			}
			if mix != nil {
				p.Pack = benchPack(p.PushID, 1)
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
				case mix != nil:
					mix.land(p.PushID, res.Position)
				}
				mu.Lock()
				took = append(took, elapsed)
				mu.Unlock()
			})
		}
		if mix != nil {
			if compaction := mix.due(); compaction != nil {
				wg.Go(func() { errs[batch] = compaction(b.Context()) })
			}
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
	if mix != nil && mix.compact != nil {
		b.ReportMetric(float64(mix.done), "compactions")
	}
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
