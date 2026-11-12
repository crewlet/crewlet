package compact_test

import (
	"bufio"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/compact"
	"github.com/crewlet/crewlet/internal/gitlet/gitlettest"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/statelog"
)

// BenchmarkExpunge prices ONE EXPUNGE ON THE MAINTAINER'S OWN SCHEDULE against
// what it frees, the figures gitlet.ExpungeMinBytes is decided by: a repository
// of about repo bytes, share of them unreachable — a branch's random,
// incompressible files pushed and deleted a grace ago — and the rest
// gittest.History's composition, reached by main. Each loop is a repository
// of its own, built and pushed before the clock runs; the clock runs over
// one weekly cycle (compact.Compactor.Expunge): the scan's walk of what the
// tips reach, the builds, the uploads and both records.
//
//   - cycle-ms: the cycle's wall clock, on the home's maintenance lane.
//   - freed-MiB: the live pack bytes the cycle took out — what the
//     repository's quota (code.limits.repo_max_bytes) gets back, and what
//     the store deletes after gitlet.RetireGrace.
//   - redownload-MiB: the live pack bytes the cycle ADDED — its kept packs
//     and its rescue — which every other Gitlet node holding the repository
//     downloads once; on a fleet of eight Gitlet nodes, seven times this.
//   - fleet/freed: seven times redownload over freed, the bytes the fleet
//     moves for each byte freed.
//
// THE RULE: gitlet.ExpungeMinBytes is the unreachable bytes below which a
// cycle's wall clock buys too little — the share alone bounds fleet/freed
// (under three at a quarter, per node) — so it is set where cycle-ms at
// share=0.25 stops being dominated by the walk of the reachable history,
// which a repository pays whatever little it frees. NOT YET DECIDED BY IT
// (below): gitlet.ExpungeMinBytes stays PROVISIONAL.
//
// repo={100MB,1GB} × share={0.1,0.25,0.5}; share=0.1 is NOT DUE by the
// share and is priced as the scan alone, the weekly cost of a clean
// repository.
//
// # Measured
//
// Once each (-benchtime 1x -count 1), on two cores of the crun runner on a
// laptop other sessions loaded (load 4.5 to 9), git 2.56.0, 2026-11-02:
//
//	arm                    cycle-ms  freed-MiB  redownload-MiB  fleet/freed
//	repo=100MB/share=0.1      1 419          0               0            -
//	repo=100MB/share=0.25     2 131      24.01               0            0
//	repo=100MB/share=0.5      4 242      48.02               0            0
//	repo=1000MB/share=0.1    66 078          0               0            -
//	repo=1000MB/share=0.25   24 467      239.1               0            0
//	repo=1000MB/share=0.5    not run
//
// At 100 MB the walk (the scan alone, 1.4 s) is two thirds of the cycle that
// frees 24 MiB and a third of the one that frees 48 MiB, so by THE RULE the
// least lies between the two. At 1 GB the scan alone took longer than the
// whole cycle that freed 239 MiB, which only the load on the host explains,
// so that pair says nothing of where the walk stops dominating; and the
// re-download is zero in every arm, since this composition leaks into a pack
// of its own, which a cycle rewrites whole as dropping. Deciding the least
// needs the 1 GB arms on an unloaded host, several counts each, and a leak
// mixed into packs that also hold reachable objects.
func BenchmarkExpunge(b *testing.B) {
	quiet(b)
	for _, repo := range []int64{100_000_000, 1_000_000_000} {
		for _, share := range []float64{0.1, 0.25, 0.5} {
			b.Run(fmt.Sprintf("repo=%dMB/share=%g", repo/1_000_000, share), func(b *testing.B) {
				benchExpunge(b, repo, share)
			})
		}
	}
}

// benchExpunge is one arm of BenchmarkExpunge: a repository of about repo
// bytes, share of them unreachable, expunged on the schedule a loop.
func benchExpunge(b *testing.B, repo int64, share float64) {
	gittest.RequireNode(b)
	client := gittest.NewClient(b)
	src, commits := gittest.History(b, client, int64(float64(repo)*(1-share)))
	tip := commits[len(commits)-1]
	reached := gittest.Empty(b)
	main := pushed{bare: reached, checksum: gittest.Pack(b, client, reached, src, tip), old: gitlet.ZeroOID, tip: tip}
	leakTip := leakCommit(b, client, src, int64(float64(repo)*share))
	leaked := gittest.Empty(b)
	leak := pushed{bare: leaked, checksum: gittest.Pack(b, client, leaked, src, leakTip, "^"+tip),
		old: gitlet.ZeroOID, tip: leakTip}

	f := newBenchFleet(b, 1)
	c, _, _ := f.compactor(b, gitlet.CompactMinMerge, &traffic{})
	compact.ExpungeFrom(c, 1)
	compact.ClockAt(c, func() time.Time { return time.Now().Add(gitlet.PruneGrace + time.Hour) })
	var cycle time.Duration
	var freed, added int64
	loops, expunged := 0, 0
	for b.Loop() {
		loops++
		b.StopTimer()
		name, id := f.declare(b)
		f.propose(b, id, main)
		f.proposeOn(b, id, leak, "refs/heads/leak")
		at := f.proposeOn(b, id, pushed{old: leakTip, tip: gitlet.ZeroOID}, "refs/heads/leak")
		f.sync(b, f.home(), id, at)
		before := f.livePacks(b, name)
		cand := f.weekly(b, name)
		b.StartTimer()
		began := time.Now()
		out, err := expungeWhileBusy(b, c, cand)
		cycle += time.Since(began) //fixedwait:measure a benchmark reports this; no gate reads it
		b.StopTimer()
		// THE SHARE IS JUDGED ON DISK, never on the arm's nominal share: the
		// history packs smaller than its raw bytes, so share=0.25 may read
		// just past a quarter. `expunged` says how many loops were due.
		switch out {
		case compact.OutcomeApplied:
			expunged++
		case compact.OutcomeNotDue:
		default:
			b.Fatalf("an expunge of %s, %g unreachable, answered %s, %v", name, share, out, err)
		}
		after := f.livePacks(b, name)
		for k, n := range before {
			if _, ok := after[k]; !ok {
				freed += n
			}
		}
		for k, n := range after {
			if _, ok := before[k]; !ok {
				added += n
			}
		}
		b.StartTimer()
	}
	n := float64(loops)
	b.ReportMetric(float64(expunged)/n, "expunged")
	b.ReportMetric(float64(cycle.Milliseconds())/n, "cycle-ms")
	b.ReportMetric(float64(freed)/n/(1<<20), "freed-MiB")
	b.ReportMetric(float64(added)/n/(1<<20), "redownload-MiB")
	if freed > 0 {
		b.ReportMetric(float64(added*(eightNodes-1))/float64(freed), "fleet/freed")
	}
}

// leakCommit writes a root commit on refs/heads/leak in src whose files are
// size bytes of seeded random — incompressible, as a leaked artifact or a
// vendored binary is — in files of a mebibyte, and answers it.
func leakCommit(b *testing.B, client *gittest.Client, src string, size int64) string {
	b.Helper()
	rng := rand.New(rand.NewChaCha8([32]byte{7}))
	gittest.FastImport(b, client, src, func(w *bufio.Writer) error {
		const msg = "leak\n"
		if _, err := fmt.Fprintf(w, "commit refs/heads/leak\ncommitter Bench <bench@example.com> 1767225600 "+
			"+0000\ndata %d\n%s", len(msg), msg); err != nil {
			return err
		}
		chunk := make([]byte, 1<<20)
		for i := 0; int64(i)*int64(len(chunk)) < size; i++ {
			for j := range chunk {
				chunk[j] = byte(rng.Uint32())
			}
			if _, err := fmt.Fprintf(w, "M 100644 inline leak/%05d.bin\ndata %d\n", i, len(chunk)); err != nil {
				return err
			}
			if _, err := w.Write(chunk); err != nil {
				return err
			}
			if err := w.WriteByte('\n'); err != nil {
				return err
			}
		}
		return nil
	})
	return strings.TrimSpace(client.Run(b, src, "rev-parse", "refs/heads/leak"))
}

// proposeOn lands p's command on ref of the repository id — with p's pack
// when it names one, none otherwise — through the data node's committer, and
// answers the position it landed at.
func (f *benchFleet) proposeOn(b *testing.B, id string, p pushed, ref string) statelog.Position {
	b.Helper()
	pushID := uuid.Must(uuid.NewV7()).String()
	prop := gitlet.Proposal{PushID: pushID, RepoID: id, Principal: pushRef, Via: gitlet.ViaGit, Node: f.home().ID(),
		ReceivedAt: time.Now().UTC(), Commands: []gitlet.Command{{Ref: ref, Old: p.old, New: p.tip}}}
	if p.checksum != "" {
		path := filepath.Join(p.bare, "objects", "pack", "pack-"+p.checksum+".pack")
		pack, err := gitlet.LayBundle(pushID, path, p.checksum)
		if err != nil {
			b.Fatal(err)
		}
		if err := gitlet.StoreBundle(b.Context(), f.store, path, pack, nil); err != nil {
			b.Fatalf("store pack-%s: %v", p.checksum, err)
		}
		prop.Pack = pack
	}
	res, err := f.fleet.Data().Served().Propose(b.Context(), prop)
	if err != nil || res.Outcome != statelog.OutcomeApplied || len(res.Answer.Commands) != 1 ||
		res.Answer.Commands[0].Result != gitlet.ResultOK {
		b.Fatalf("the push to %s: %+v, %v", ref, res, err)
	}
	return res.Position
}

// livePacks is the repository called name's live pack rows' pack bytes, by
// their bundle's object key, read linearizable.
func (f *benchFleet) livePacks(b *testing.B, name string) map[string]int64 {
	b.Helper()
	rows, err := f.fleet.Rows(b.Context(), name, gitlettest.Linearizable)
	if err != nil {
		b.Fatalf("the rows of %s: %v", name, err)
	}
	out := map[string]int64{}
	for _, r := range rows.Packs {
		if r.State == gitlet.PackLive {
			out[r.Object.String()] = r.Ref().PackBytes()
		}
	}
	return out
}

// weekly is the repository called name as the expunge listing's weekly arm
// names it, asked a week before the compactor's clock, read linearizable.
func (f *benchFleet) weekly(b *testing.B, name string) gitlet.ExpungeCandidate {
	b.Helper()
	q := gitlet.ReadQuery{Kind: gitlet.ReadExpungeDue,
		ScannedBefore: time.Now().Add(gitlet.PruneGrace + time.Hour - gitlet.ExpungeInterval),
		Fresh:         gitlettest.Linearizable}
	for {
		a, err := f.fleet.Data().Served().Read(b.Context(), q)
		if err != nil {
			b.Fatalf("the expunge listing: %v", err)
		}
		for _, cand := range a.Expunge {
			if cand.Name == name {
				return cand
			}
		}
		if a.Next == "" {
			b.Fatalf("the weekly arm names no %s", name)
		}
		q.Cursor = a.Next
	}
}
