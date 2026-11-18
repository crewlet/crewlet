package gitletcluster_test

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

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitlettest"
	"github.com/crewlet/crewlet/internal/objstore"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/statelog"
)

// BenchmarkCommitChain is one repository's commit chain end to end, from the
// proposal to its committer to the answer read from the push's row (package
// doc, "The commit chain, measured"). Every push creates a ref with no stored
// pack, since the decide reads no object, so the object store is not timed;
// brokers fsync every write. members=3 proposes via=committer in process or
// via=router through another member's estate.Router; batch runs from a lone
// push to gitlet.CommitterInbox, a committer's whole queue; repos=N spreads a
// batch over N repositories, a record each; mix=compaction adds to mix=packs'
// pack per push one compaction per compactEvery pushes, decided alone
// (internal/gitlet's "A compaction merges packs, decided alone"), and nothing
// else. Before a batch would pass a bound (room) an arm moves to fresh
// repositories, untimed. It reports pushes/s, records/s, pushes/record, p50/p99.
func BenchmarkCommitChain(b *testing.B) {
	gitlettest.Quiet(b)
	b.Run("members=1", func(b *testing.B) {
		d, declare := soloChain(b)
		// fresh is one repository declared afresh, its committer the one node.
		fresh := func(b *testing.B) target {
			return target{repos: declare(b, 1), propose: d.Served().Propose,
				compact: d.Served().ProposeCompaction}
		}
		appends := func() int { return d.Appends() }
		for _, batch := range []int{1, 8, gitlet.MaxPushesPerRecord} {
			b.Run(fmt.Sprintf("batch=%d", batch), func(b *testing.B) {
				chain(b, batch, fresh, appends, nil)
			})
		}
		for _, compacting := range []bool{false, true} {
			b.Run(fmt.Sprintf("batch=%d/%s", gitlet.MaxPushesPerRecord, mixName(compacting)), func(b *testing.B) {
				chain(b, gitlet.MaxPushesPerRecord, fresh, appends, newMix(compacting))
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
		// fresh asks each fresh repository's committer anew: rendezvous may
		// place it on another member.
		fresh := func(via string) func(*testing.B) target {
			return func(b *testing.B) target {
				repos := clusterRepos(b, c, 1)
				return target{repos: repos, propose: c.proposer(b, via, repos[0]),
					compact: c.member(b, c.Committer(repos[0])).data.Served().ProposeCompaction}
			}
		}
		for _, via := range []string{"committer", "router"} {
			for _, batch := range []int{1, 8, gitlet.MaxPushesPerRecord} {
				b.Run(fmt.Sprintf("via=%s/batch=%d", via, batch), func(b *testing.B) {
					chain(b, batch, fresh(via), appends, nil)
				})
			}
		}
		for _, compacting := range []bool{false, true} {
			b.Run(fmt.Sprintf("via=committer/batch=%d/%s", gitlet.MaxPushesPerRecord, mixName(compacting)),
				func(b *testing.B) {
					chain(b, gitlet.MaxPushesPerRecord, fresh("committer"), appends, newMix(compacting))
				})
		}
		// Past one record's worth, up to a full inbox: at CommitterInbox an
		// iteration's ns/op is a whole committer queue's drain.
		for _, batch := range []int{4 * gitlet.MaxPushesPerRecord, gitlet.CommitterInbox} {
			b.Run(fmt.Sprintf("via=committer/batch=%d", batch), func(b *testing.B) {
				chain(b, batch, fresh("committer"), appends, nil)
			})
		}
		for _, arm := range []struct{ batch, repos int }{{8, 1}, {8, 16}, {64, 16}, {64, 64}} {
			b.Run(fmt.Sprintf("via=router/batch=%d/repos=%d", arm.batch, arm.repos), func(b *testing.B) {
				// One member proposes every push: those it commits for
				// itself answer in process, the rest cross the wire.
				chain(b, arm.batch, func(b *testing.B) target {
					return target{repos: clusterRepos(b, c, arm.repos),
						propose: c.members[0].router.Load().Code().Propose}
				}, appends, nil)
			})
		}
	})
}

// target is the repositories an arm pushes to, its pushes going through
// propose and a mix=compaction arm's compactions of repos[0] through compact.
type target struct {
	repos   []string
	propose func(context.Context, gitlet.Proposal) (gitlet.ProposeResult, error)
	compact func(context.Context, gitlet.CompactionProposal) (gitlet.CompactResult, error)
}

// compactEvery is a mix=compaction arm's cadence, one compaction of every pack
// landed per 300 pushes: internal/gitlet/compact's steady arm, a compactor
// pass a minute at 300 pushes a minute.
const compactEvery = 300

// compactionMix is a mix= arm's state on its one repository: every push
// carries a pack and, when compacting, the packs landed since the last
// compaction are proposed through compact once compactEvery have landed.
type compactionMix struct {
	compacting bool

	mu      sync.Mutex
	repo    string
	compact func(context.Context, gitlet.CompactionProposal) (gitlet.CompactResult, error)
	landed  []string
	seen    statelog.Position
	// live is repo's live packs as the decide counts them for the pack bound.
	live int
	done int
}

// newMix is a mix= arm's: mix=compaction where compacting, mix=packs — the
// same pushes, no compaction — where not. It pushes nowhere until moveTo.
func newMix(compacting bool) *compactionMix {
	return &compactionMix{compacting: compacting}
}

// moveTo points the mix at t's one fresh repository, dropping what the last
// one landed.
func (m *compactionMix) moveTo(t target) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.repo, m.landed, m.seen, m.live = t.repos[0], m.landed[:0], statelog.Position{}, 0
	if m.compacting {
		m.compact = t.compact
	}
}

// livePacks is the packs live in the mix's repository.
func (m *compactionMix) livePacks() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.live
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
	m.live++
	// Every push lands on the code log, so the two compare.
	if later, err := m.seen.Before(at); m.seen.IsZero() || (err == nil && later) {
		m.seen = at
	}
}

// due is the compaction of every pack landed since the last, planned at the
// newest position one landed at, once compactEvery have; nil before then or
// in a mix that does not compact.
func (m *compactionMix) due() func(context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.compact == nil || len(m.landed) < compactEvery {
		return nil
	}
	retire, seen, repo, compact := slices.Clone(m.landed), m.seen, m.repo, m.compact
	m.landed = m.landed[:0]
	slices.Sort(retire)
	return func(ctx context.Context) error {
		cid := statelog.NewOpID(time.Now(), "")
		out := benchPack(gitlet.DeriveOutputPackID(cid, 0), int64(len(retire)))
		res, err := compact(ctx, gitlet.CompactionProposal{Compaction: gitlet.Compaction{RepoID: repo,
			CompactionID: cid, Node: "bench", Seen: seen, Retire: retire, Add: []gitlet.PackRef{*out}}})
		switch {
		case err != nil:
			return fmt.Errorf("the compaction of %d packs: %w", len(retire), err)
		case res.Outcome != statelog.OutcomeApplied:
			return fmt.Errorf("the compaction of %d packs was answered %+v, want applied", len(retire), res)
		}
		m.mu.Lock()
		m.done++
		m.live += 1 - len(retire)
		m.mu.Unlock()
		return nil
	}
}

// benchPack is pack id's row holding objects objects, with the sizes
// gitlet.PackRef.Check demands of that many; named, never stored, since a
// decide reads no object.
func benchPack(id string, objects int64) *gitlet.PackRef {
	return &gitlet.PackRef{PackID: id, Object: objstore.KeyAt(time.Now()), Hash: objstore.HashOf([]byte(id)),
		Size:       32*objects + gitlet.IndexBytesFor(objects) + gitlet.ReverseBytesFor(objects),
		IndexBytes: gitlet.IndexBytesFor(objects), ReverseBytes: gitlet.ReverseBytesFor(objects),
		GitChecksum: randomOID(), Objects: objects}
}

// chain is one arm: per iteration, batch pushes, each creating its own ref,
// proposed at once round robin over the target's repositories and waited for
// until all apply ok, with a mix's due compaction beside them. A batch the
// target has no room for goes to a fresh target, declared with the timer
// stopped.
func chain(b *testing.B, batch int, fresh func(*testing.B) target, appends func() int, mix *compactionMix) {
	b.Helper()
	var (
		mu   sync.Mutex
		took []time.Duration
	)
	t := renew(b, fresh, batch, mix)
	// pushes counts the arm's, naming its refs; made counts t's, filling it.
	pushes, made := 0, 0
	before := appends()
	for b.Loop() {
		if !room(t, made, batch, mix) {
			b.StopTimer()
			t, made = renew(b, fresh, batch, mix), 0
			b.StartTimer()
		}
		var wg sync.WaitGroup
		// One slot per push, plus the mix's compaction's.
		errs := make([]error, batch+1)
		propose := t.propose
		for k := range batch {
			p := gitlet.Proposal{
				PushID: uuid.Must(uuid.NewV7()).String(), RepoID: t.repos[(made+k)%len(t.repos)],
				Principal: gitlet.Principal{Kind: gitlet.PrincipalOperator, OperatorID: "bench", Seat: "ceo"},
				Via:       gitlet.ViaGit, Node: "bench", ReceivedAt: time.Now().UTC(),
				Commands: []gitlet.Command{{Ref: fmt.Sprintf("refs/heads/b%d-%d", pushes, k), Old: gitlet.ZeroOID,
					New: randomOID()}},
			}
			if mix != nil {
				p.Pack = benchPack(p.PushID, 1)
			}
			wg.Go(func() {
				at := time.Now()
				res, err := propose(b.Context(), p)
				elapsed := time.Since(at) //fixedwait:measure a benchmark reports this; no gate reads it
				switch {
				case err != nil:
					errs[k] = err
				case res.Outcome != statelog.OutcomeApplied || len(res.Answer.Commands) != 1 ||
					res.Answer.Commands[0].Result != gitlet.ResultOK:
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
		made += batch
	}
	records := appends() - before
	elapsed := b.Elapsed().Seconds()
	slices.Sort(took)
	b.ReportMetric(float64(pushes)/elapsed, "pushes/s")
	b.ReportMetric(float64(records)/elapsed, "records/s")
	b.ReportMetric(float64(pushes)/float64(max(records, 1)), "pushes/record")
	b.ReportMetric(quantile(took, 0.50), "p50-ms")
	b.ReportMetric(quantile(took, 0.99), "p99-ms")
	if mix != nil && mix.compacting {
		b.ReportMetric(float64(mix.done), "compactions")
	}
}

// renew is a target fresh declares, the mix moved onto it; the arm fails if
// even an empty target has no room for a batch, which no renewal would cure.
func renew(b *testing.B, fresh func(*testing.B) target, batch int, mix *compactionMix) target {
	b.Helper()
	t := fresh(b)
	if mix != nil {
		// One repository: a committer answers a compaction naming another
		// repository's packs stale.
		if len(t.repos) != 1 {
			b.Fatalf("a mix= arm pushes to one repository, and this one declares %d", len(t.repos))
		}
		mix.moveTo(t)
	}
	if !room(t, 0, batch, mix) {
		b.Fatalf("a batch of %d pushes passes a bound on %d repositories holding nothing: "+
			"gitlet.MaxRefsPerRepo (%d), or with packs gitlet.MaxPacksPerRepo (%d)",
			batch, len(t.repos), gitlet.MaxRefsPerRepo, gitlet.MaxPacksPerRepo)
	}
	return t
}

// room reports whether t's repositories, holding made pushes, take a batch
// more within gitlet.MaxRefsPerRepo refs each (one a push, round robin) and,
// with a mix, gitlet.MaxPacksPerRepo live packs, the batch's compaction
// counted as landing last. Past either a push is refused (repo_refs,
// compaction_behind) and the arm would measure refusals.
func room(t target, made, batch int, mix *compactionMix) bool {
	n := len(t.repos)
	if (made+batch+n-1)/n > gitlet.MaxRefsPerRepo {
		return false
	}
	return mix == nil || mix.livePacks()+batch <= gitlet.MaxPacksPerRepo
}

// soloChain is one data node on an embedded broker of its own that fsyncs
// every write, and a declare of n fresh repositories on it.
func soloChain(b *testing.B) (*gitlettest.DataNode, func(b *testing.B, n int) []string) {
	b.Helper()
	dir := brokertest.TempDir(b)
	q, err := js.Open(b.Context(), js.Config{StoreDir: filepath.Join(dir, "stream"),
		StoreMaxBytes: brokertest.StoreMaxBytes, SyncAlways: true})
	if err != nil {
		b.Fatalf("open the broker: %v", err)
	}
	// Registered first so it runs last, after the data node's teardown.
	b.Cleanup(func() { _ = q.Stop(context.Background()) })
	d := gitlettest.StartDataNode(b, gitlettest.DataOptions{ID: "solo", Queue: q, Dir: dir, LogBytes: codeLogBytes})
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	activations := 0
	return d, func(b *testing.B, n int) []string {
		b.Helper()
		declared := make([]gitlet.Declared, n)
		for i := range declared {
			declared[i] = gitlet.Declared{Name: "chain-" + uuid.NewString()[:8], Settings: gitlettest.Settings()}
		}
		activations++
		d.Declare(b, base.Add(time.Duration(activations)*time.Second), declared...)
		ids := make([]string, n)
		for i, r := range declared {
			ref, err := d.Served().Resolve(b.Context(), gitlet.ResolveQuery{Name: r.Name, Fresh: gitlettest.Linearizable})
			if err != nil {
				b.Fatalf("resolve %s: %v", r.Name, err)
			}
			ids[i] = ref.ID
		}
		return ids
	}
}

// clusterRepos declares n fresh repositories on c and answers their ids once
// every member has drained the declarations, so each committer decides its
// first push from a copy that answers.
func clusterRepos(b *testing.B, c *cluster, n int) []string {
	b.Helper()
	declared := make([]gitlet.Declared, n)
	for i := range declared {
		declared[i] = gitlet.Declared{Name: "chain-" + uuid.NewString()[:8], Settings: gitlettest.Settings()}
	}
	c.Declare(b, declared...)
	if err := c.drained(b.Context()); err != nil {
		b.Fatal(err)
	}
	ids := make([]string, n)
	for i, r := range declared {
		rows, err := c.Rows(b.Context(), r.Name, gitlettest.Linearizable)
		if err != nil {
			b.Fatalf("the rows of %s: %v", r.Name, err)
		}
		ids[i] = rows.Repo.ID
	}
	return ids
}

// proposer is how a via= arm proposes a push to repo: to its committer member
// in process, or through the router of a member that is not its committer.
func (c *cluster) proposer(b *testing.B, via, repo string) func(context.Context, gitlet.Proposal) (gitlet.ProposeResult, error) {
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
	b.Fatal("gitletcluster: no member but the committer")
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

// BenchmarkEngineWrite is internal/gitlet/host's BenchmarkEngineWrite over the
// production route between members (package doc, "An engine write's route"):
// one seat's commit of one file at a time onto its own branch, asked in
// process at the home (route=home) or through another member's router, which asks the home
// over the broker (route=router). Each is timed from estate.CodeSeam.GitWrite
// to the push applied and its pack adopted, not from the tool, whose part is
// the same on either route. The home is the committer too (gitlettest's F40);
// the object store is the broker's bucket, every write fsynced. A solo
// benchmark (internal/solo): the host's arms, not these, decide an engine
// write's budgets (estate.GitWriteAttempt).
func BenchmarkEngineWrite(b *testing.B) {
	gitlettest.Quiet(b)
	c := startClusterOf(b, "nats", js.Config{SyncAlways: true})
	repo := clusterRepos(b, c, 1)[0]
	home := c.Committer(repo)
	routes := []struct{ name, via string }{{"home", home}}
	for _, m := range c.Members() {
		if m != home {
			routes = append(routes, struct{ name, via string }{"router", m})
			break
		}
	}
	for _, route := range routes {
		b.Run("route="+route.name, func(b *testing.B) {
			w := &routedWrites{c: c, repo: repo, via: route.via,
				ref: "refs/heads/seats/" + gitlettest.FixtureAlice + "/route-" + route.name,
				who: gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: gitlettest.FixtureAlice, Run: gitlet.RunProvenance{
					TurnID: statelog.NewOpID(time.Now(), ""), WorkBackend: "native", WorkKey: "ENG-1234",
					Model: "fixture-model-2031"}}}
			// The branch's root, untimed: only moves of the branch are timed.
			w.land(b)
			var took []time.Duration
			for b.Loop() {
				start := time.Now()
				w.land(b)
				took = append(took, time.Since(start)) //fixedwait:measure a benchmark reports this; no gate reads it
			}
			slices.Sort(took)
			b.ReportMetric(quantile(took, 0.50), "p50-ms")
			b.ReportMetric(quantile(took, 0.99), "p99-ms")
		})
	}
}

// routedWrites is a route= arm's writes: one seat's commits of one file, one
// at a time, onto its branch of repo, each asked through member via's router.
type routedWrites struct {
	c        *cluster
	repo     string
	via, ref string
	who      gitlet.Principal
	head     string
	at       statelog.Position
	n        int
}

// land is the arm's next write, failing b unless it applied.
func (r *routedWrites) land(b *testing.B) {
	b.Helper()
	r.n++
	content := fmt.Sprintf("write %d\n", r.n)
	w := gitlet.EngineWrite{Kind: gitlet.WriteCommitFiles, RepoID: r.repo, Principal: r.who,
		WrittenFor: types.ForNobody(), OpID: statelog.NewOpID(time.Now(), ""),
		Fresh: statelog.FloorAt(statelog.ReadSession, r.at), Ref: r.ref,
		ExpectedHead: r.head, Message: fmt.Sprintf("write %d", r.n),
		Files: []gitlet.FileOp{{Op: gitlet.FileOpPut, Path: "route/f.txt", Content: &content}}}
	res, err := r.c.GitWriteThrough(b.Context(), r.via, w)
	if err != nil || res.Outcome != statelog.OutcomeApplied || res.Refusal != "" || res.Commit == "" {
		b.Fatalf("write %d through %s: %+v %v", r.n, r.via, res, err)
	}
	r.head, r.at = res.Commit, res.Position
}
