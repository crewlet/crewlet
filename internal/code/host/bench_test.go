package host_test

import (
	"bufio"
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	crand "crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/compact"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/code/host"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/memobj"
	"github.com/crewlet/crewlet/internal/objstore/s3obj/s3fake"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE CODE HOST'S REQUEST PATHS, MEASURED END TO END: a person's git against
// the real handlers, the real cache and a real code log on one data node
// (codetest.SoloNode), with the timer around what the person waits for and
// every fixture built with it stopped. The pools and the client run the git
// on PATH, which `make bench` makes the node pin — the git the engine image
// builds (scripts/git-pin.env). Every arm logs the pool's git version, and
// doc.go's measured section names it beside each figure.

// benchFirstByte is how long the S3 fake waits before it answers each request
// of an `s3fake` arm: 100 ms, the lower end of the 100–200 ms AWS documents
// for S3's first byte. On loopback the fake answers in microseconds, and a
// figure taken there would price the SDK rather than the bucket.
const benchFirstByte = 100 * time.Millisecond

// benchConnBytes is the most a second the S3 fake sends one connection: 80
// MB/s, the low end of what one S3 connection carries.
const benchConnBytes = 80_000_000

// benchObjects is the object store an objects= arm names: the in-memory twin
// (mem); the S3 fake behind the production backend, answering benchFirstByte
// after each request arrives at benchConnBytes a connection (s3fake); or the
// bucket CREWLET_BENCH_S3_* names (real, s3fake.RealBackend), which skips the
// arm when none is named.
func benchObjects(b *testing.B, objects string) objstore.Backend {
	b.Helper()
	switch objects {
	case "mem":
		return memobj.New()
	case "s3fake":
		srv := s3fake.Start(b, "files")
		srv.Latency, srv.ConnBytesPerSecond = benchFirstByte, benchConnBytes
		return s3Over(b, srv)
	case "real":
		return s3fake.RealBackend(b)
	}
	b.Fatalf("no object store called %q", objects)
	return nil
}

// benchCacheBytes is every benchmark git node's cache budget: room for the
// largest repository an arm builds, the gigabyte history twice over, so no
// figure is of a repository the budget evicted between iterations.
const benchCacheBytes = 4 << 30

// benchLogBytes is every benchmark fleet's code log ceiling: 128 MiB admits
// about 74 MB of records beside its gate reserve (statelog.Reservation's
// fleet term, some 57 MiB), where a solo node's own 64 MiB admits about
// 7 MB — which the gigabyte arm of BenchmarkPushSmall filled at 31 000 of
// the hundred thousand refs its refs arms build (7.26 MB with its history's
// pushes, on the node pin), refused log_full. Its hundred thousand refs and
// everything before them take some 25 MB at that rate, a third of the room.
const benchLogBytes = 128 << 20

// coldCloners is how many people clone a cold node at once in
// BenchmarkCloneCold's widest arm, each with a token of their own.
const coldCloners = 8

// cloner is the i-th of the people a cold node's clones come from.
func cloner(i int) string { return fmt.Sprintf("cloner%d", i) }

// benchFleet is a solo fleet of gitNodes over objects, its pools running the
// git on PATH and its broker fsyncing every write as a deployment's does
// (stream.sync's default), its code log benchLogBytes, with RepoName
// declared, and the person who pushes to it; the coldCloners people beside
// them have a token each.
func benchFleet(b *testing.B, objects objstore.Backend, gitNodes int) (*codetest.SoloFleet, *codetest.Person) {
	b.Helper()
	people := []string{"ceo"}
	for i := range coldCloners {
		people = append(people, cloner(i))
	}
	f := codetest.SoloNode(b, codetest.SoloOptions{Shape: codetest.Shape{GitNodes: gitNodes}, Objects: objects,
		CacheMaxBytes: benchCacheBytes, LogBytes: benchLogBytes, People: people, SyncAlways: true})
	f.Declare(b, code.Declared{Name: codetest.RepoName, Settings: codetest.Settings()})
	b.Logf("the pools run git %s", f.Nodes()[0].Pool().Version())
	return f, codetest.NewPerson(b, f, "ceo")
}

// repoURL is RepoName's URL through node.
func repoURL(n codetest.Node) string { return n.URL() + "/git/" + codetest.RepoName }

// mustPush is p's push of refspecs from dir to url, failing b on any refusal.
func mustPush(b *testing.B, p *codetest.Person, dir, url string, refspecs ...string) {
	b.Helper()
	if res := p.Push(b.Context(), dir, url, refspecs...); res.Err != nil {
		b.Fatalf("git push %d refspecs: %s", len(refspecs), res)
	}
}

// pushSessions pushes the history in src to url as gittest.HistorySessions
// pushes — main moved to the end of each session in turn — which is how a
// repository of that history came to be on a host.
func pushSessions(b *testing.B, p *codetest.Person, src, url string, commits []string) {
	b.Helper()
	for s := 1; s <= gittest.HistorySessions; s++ {
		mustPush(b, p, src, url, commits[s*len(commits)/gittest.HistorySessions-1]+":refs/heads/main")
	}
}

// addRefsBusyTries is how many times addRefs proposes one push answered busy:
// a committer busy that often with nothing else queued is a fixture that
// cannot be built, which fails the arm with the committer's reason rather
// than waiting on it for ever.
const addRefsBusyTries = 5

// addRefs creates the live refs numbered from to to (excluded) beside main in
// repo, all at tip, in pushes of code.MaxCommandsPerPush proposed to the data
// node's committer — records like any push's, with no pack, since every ref
// names a commit the repository holds. Not through a person's git: its push
// matches each of a thousand refspecs against every ref the remote
// advertised, so a push of a thousand against 46 000 refs spent twenty
// seconds in the client alone, and the hundred-thousand-ref fixture an hour
// of every count.
//
// A proposal answered busy is proposed again, at most addRefsBusyTries times
// in all, each try logged with the committer's reason.
func addRefs(b *testing.B, f *codetest.SoloFleet, repo, tip string, from, to int) {
	b.Helper()
	for lo := from; lo < to; lo += code.MaxCommandsPerPush {
		var cmds []code.Command
		for r := lo; r < min(lo+code.MaxCommandsPerPush, to); r++ {
			cmds = append(cmds, code.Command{Ref: fmt.Sprintf("refs/heads/r%06d", r), Old: code.ZeroOID, New: tip})
		}
		p := code.Proposal{
			PushID: uuid.Must(uuid.NewV7()).String(), RepoID: repo,
			Principal: code.Principal{Kind: code.PrincipalOperator, OperatorID: "bench", Seat: "ceo"},
			Via:       code.ViaGit, Node: "bench", ReceivedAt: time.Now().UTC(), Commands: cmds,
		}
		res, err := f.Data().Served().Propose(b.Context(), p)
		for try := 1; err == nil && res.Answer.Refused == code.RefusalBusy && try < addRefsBusyTries; try++ {
			// BUSY IS THE COMMITTER'S "AGAIN", answered with nothing decided,
			// which a person's git meets as a push to run again: the fixture
			// does what that person does, and says so, since a committer
			// busy with nothing else queued is a figure in itself.
			b.Logf("a push of %d refs at %d live refs answered busy (%s); proposing it again", len(cmds), lo,
				res.Answer.Detail)
			res, err = f.Data().Served().Propose(b.Context(), p)
		}
		if err != nil {
			b.Fatalf("a push of %d refs: %v", len(cmds), err)
		}
		if res.Outcome != statelog.OutcomeApplied || len(res.Answer.Commands) != len(cmds) {
			b.Fatalf("a push of %d refs at %d live refs was answered %q with %d commands, refused %s: %s", len(cmds),
				lo, res.Outcome, len(res.Answer.Commands), res.Answer.Refused, res.Answer.Detail)
		}
		for _, c := range res.Answer.Commands {
			if c.Result != code.ResultOK {
				b.Fatalf("a push of %d refs refused %s: %s", len(cmds), c.Ref, c.Result)
			}
		}
	}
}

// serverCPU is what a node's git processes have spent serving requests: the
// transfer lane's upload-packs and receive-packs, and the read lane's scans,
// peels and round indexes — never the maintainer's, which runs beside the
// requests on a schedule of its own.
func serverCPU(rec *metrics.Recorder) float64 {
	return cpuMillis(rec, gitproc.ClassTransfer) + cpuMillis(rec, gitproc.ClassRead)
}

// randomBytes is n incompressible bytes.
func randomBytes(n int) string {
	raw := make([]byte, n)
	_, _ = crand.Read(raw)
	return string(raw)
}

// quantile is the nearest-rank q-quantile of sorted, in milliseconds.
func quantile(sorted []time.Duration, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := max(int(math.Ceil(q*float64(len(sorted)))), 1)
	return float64(sorted[rank-1]) / float64(time.Millisecond)
}

// advertise is one push's advertisement on n — `GET info/refs?service=
// git-receive-pack` as a person's git sends it — timed alone: the
// linearizable sync before it, and receive-pack listing the refs.
func advertise(b *testing.B, n codetest.Node, token string) time.Duration {
	b.Helper()
	req, err := http.NewRequestWithContext(b.Context(), http.MethodGet,
		repoURL(n)+"/info/refs?service=git-receive-pack", nil)
	if err != nil {
		b.Fatal(err)
	}
	req.SetBasicAuth("crewlet", token)
	at := time.Now()
	resp, err := n.Client().Do(req)
	if err != nil {
		b.Fatalf("a push's advertisement: %v", err)
	}
	_, err = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	took := time.Since(at)
	if err != nil || resp.StatusCode != http.StatusOK {
		b.Fatalf("a push's advertisement answered %d (%v)", resp.StatusCode, err)
	}
	return took
}

// firstByteMillis is the median of 64 one-byte page reads of the largest
// object rows names, through the object store as the cache reads one — the
// round trip a GET of that store costs before any byte of a bundle moves,
// which the real arms report beside their figures.
func firstByteMillis(b *testing.B, backend objstore.Backend, rows codetest.Rows) float64 {
	b.Helper()
	store, err := objstore.NewStore(backend)
	if err != nil {
		b.Fatal(err)
	}
	if len(rows.Packs) == 0 {
		b.Fatal("no stored bundle to read a page of")
	}
	pack := slices.MaxFunc(rows.Packs, func(x, y code.PackRow) int { return int(x.Size - y.Size) })
	obj := objstore.Object{Key: pack.Object, Hash: pack.Hash, Size: pack.Size}
	took := make([]time.Duration, 64)
	for i := range took {
		at := time.Now()
		if _, err := store.ReadAt(b.Context(), obj, int64(i)%obj.Size, 1); err != nil {
			b.Fatalf("a one-byte read of %s: %v", obj.Key, err)
		}
		took[i] = time.Since(at)
	}
	slices.Sort(took)
	return quantile(took, 0.5)
}

// BenchmarkPushSmall is ONE PERSON'S PUSH OF ONE NEW COMMIT — one 4 KiB file
// — to main, end to end through the HTTP handler, as the person waits for it:
// the advertisement, receive-pack, the bundle's put, the proposal, the commit
// chain and the wait for the push's row, with git's client process included.
//
//   - objects=mem|s3fake|real/history=small/refs=1 is the object store the
//     bundle is put to, on a small repository: the push's one request that
//     waits on S3 is its put.
//   - objects=mem/history=small|1GB is a repository of that history —
//     gittest.History's composition pushed as gittest.HistorySessions pushes,
//     a megabyte or a gigabyte — built ONCE for every arm under it (1GB
//     only under CREWLET_BENCH_LARGE), each arm taking it as the one before
//     left it:
//   - graph=no|yes/clock=normal|skewed: pushes to the fleet's SECOND git
//     node, emptied and synced whole first: graph=no with every slot of its
//     maintenance lane held, so it has no commit-graph and no index — a copy
//     a node just read whole, before its maintainer reached it — and
//     graph=yes once its maintainer has indexed every pack it holds and
//     written both (warmIndex). clock=normal dates
//     each commit after its parent (gittest.After), clock=skewed before the
//     whole history (gittest.Skewed): a person's clock that is behind, which
//     git's walks by date read as a reason to walk every commit.
//   - step=force|introduced|path|connectivity/graph=no|yes/tip=normal|
//     skewed: each walk a push makes over the history, alone, on the same
//     copy — the force check's merge-base, the listing of what a push
//     introduces, the search for a blob's path, and receive-pack's own
//     connectivity check as git spells it (connected.c) — from a tip dated
//     after its parent or before the history, reporting ns/commit and
//     ns/object of the history (gitproc.WalkPerObject's figures); and
//     step=graph-write, a commit-graph written over the history from main's
//     tip, ns/commit — the write the maintainer's graph-first run makes,
//     which is how long the window in which every walk goes by date lasts.
//   - refs=1000|10000|100000: pushes with that many live refs, main among
//     them, created beforehand by pushes of code.MaxCommandsPerPush proposed
//     to the committer (addRefs) — the refs only grow, so each arm adds what
//     the one before left short; 100000 only under CREWLET_BENCH_LARGE. Each
//     pushes to the second git node EMPTIED, synced whole and indexed whole
//     again first (coldNode, warmIndex), so it measures the refs and never
//     the packs the arms before it left outside an index: a repository the
//     maintainer cannot index again for MidxMinInterval gathers every push
//     of a busy ten minutes outside its index, as the arms before would.
//
// Every arm but graph=no pushes onto a copy its receiving node has indexed
// whole before it (warmIndex), the arm's own pushes gathering outside the
// index as a repository's do between its maintainer's runs.
//
// Each push arm reports, a push: server-ms, the receiving node's own
// crewlet.code.push.duration for the push applied; adv-ms, the push's
// advertisement (`info/refs?service=git-receive-pack`) timed alone after it;
// sync-ms, the OTHER git node's delta sync of the push
// (crewlet.code.cache.sync.duration), which is the packed-refs merge every
// node pays; and transfer-cpu-ms and read-cpu-ms, the CPU of the receiving
// node's git processes by lane (crewlet.code.git.cpu) — receive-pack and its
// index-pack, and the push's scans; and packs-outside, the mean count of the
// receiving copy's packs outside its index, as each push landed. The real arm
// also reports first-byte-ms (firstByteMillis).
func BenchmarkPushSmall(b *testing.B) {
	codetest.Quiet(b)
	for _, objects := range []string{"mem", "s3fake", "real"} {
		b.Run(fmt.Sprintf("objects=%s/history=small/refs=1", objects), func(b *testing.B) {
			backend := benchObjects(b, objects)
			r := newPushRig(b, backend, 1<<20)
			warmIndex(b, r.f.Nodes()[0], r.repo, 0)
			firstByte := -1.0
			if objects == "real" {
				firstByte = firstByteMillis(b, backend, r.rows(b))
			}
			r.pushes(b, r.f.Nodes()[0], r.f.Nodes()[1], r.after)
			if firstByte >= 0 {
				b.ReportMetric(firstByte, "first-byte-ms")
			}
		})
	}
	for _, history := range []struct {
		name  string
		bytes int64
	}{{"small", 1 << 20}, {"1GB", 1 << 30}} {
		b.Run("objects=mem/history="+history.name, func(b *testing.B) {
			if history.name == "1GB" {
				gittest.Large(b, "a gigabyte's history pushed through the host")
			}
			r := newPushRig(b, memobj.New(), history.bytes)
			first := r.f.Nodes()[0]

			// GRAPH=NO: the second node emptied, every slot of its
			// maintenance lane held, and synced whole.
			fresh, release := coldHeld(b, r.f)
			r.syncWhole(b, fresh)
			r.walks(b, fresh, "no")
			for _, clock := range []string{"normal", "skewed"} {
				b.Run("graph=no/clock="+clock, func(b *testing.B) { r.pushes(b, fresh, first, r.dated(clock)) })
			}
			// GRAPH=YES: its maintainer let go, the commit-graph its first
			// run writes, and the index and bitmap after it.
			release()
			warmIndex(b, fresh, r.repo, 0)
			r.walks(b, fresh, "yes")
			for _, clock := range []string{"normal", "skewed"} {
				b.Run("graph=yes/clock="+clock, func(b *testing.B) { r.pushes(b, fresh, first, r.dated(clock)) })
			}

			// THE REFS, each arm adding what the one before left short, to
			// the second node emptied and indexed whole again.
			have := 1
			for _, refs := range []int{1_000, 10_000, code.MaxRefsPerRepo} {
				b.Run(fmt.Sprintf("refs=%d", refs), func(b *testing.B) {
					if refs >= code.MaxRefsPerRepo {
						gittest.Large(b, "a hundred thousand refs proposed to the committer")
					}
					addRefs(b, r.f, r.repo, r.tip(b), have-1, refs-1)
					have = refs
					cold := coldNode(b, r.f)
					r.syncWhole(b, cold)
					warmIndex(b, cold, r.repo, 0)
					r.pushes(b, cold, first, r.after)
				})
			}
		})
	}
}

// pushRig is BenchmarkPushSmall's fixture: a fleet of two git nodes holding
// a repository of one history, and the person whose clone of it every timed
// push is made from.
type pushRig struct {
	f       *codetest.SoloFleet
	p       *codetest.Person
	repo    string
	work    string
	commits []string
	// n is how many commits the rig has made on top of the history.
	n int
}

// newPushRig builds a fleet over backend holding a history of about size
// bytes, pushed through the first node a session at a time, and a clone of it
// to push from; the second node is synced whole too.
func newPushRig(b *testing.B, backend objstore.Backend, size int64) *pushRig {
	b.Helper()
	f, p := benchFleet(b, backend, 2)
	r := &pushRig{f: f, p: p}
	src, commits := gittest.History(b, p.Git(), size)
	pushSessions(b, p, src, repoURL(f.Nodes()[0]), commits)
	r.commits, r.repo = commits, r.rows(b).Repo.ID
	r.work = filepath.Join(b.TempDir(), "work")
	p.Git().Run(b, "", "clone", "--quiet", src, r.work)
	if err := os.RemoveAll(src); err != nil {
		b.Fatal(err)
	}
	r.syncWhole(b, f.Nodes()[1])
	return r
}

// rows is the repository's rows, read at the log's head.
func (r *pushRig) rows(b *testing.B) codetest.Rows {
	b.Helper()
	rows, err := r.f.Rows(b.Context(), codetest.RepoName, codetest.Linearizable)
	if err != nil {
		b.Fatal(err)
	}
	return rows
}

// tip is main's commit as the rows hold it.
func (r *pushRig) tip(b *testing.B) string {
	b.Helper()
	if oid, ok := r.rows(b).Refs["refs/heads/main"]; ok {
		return oid
	}
	b.Fatal("the repository has no main")
	return ""
}

// syncWhole is node's sync of the repository at the log's head: the whole of
// it, on a node that holds none of it.
func (r *pushRig) syncWhole(b *testing.B, node codetest.Node) {
	b.Helper()
	s, err := node.Cache().Sync(b.Context(), r.repo, codetest.Linearizable)
	if err != nil {
		b.Fatalf("node %s's sync: %v", node.ID(), err)
	}
	s.Release()
}

// after is the date of the rig's n-th commit dated after its parent.
func (r *pushRig) after(n int) string { return gittest.After(r.commits, n) }

// dated is the date a clock= arm's n-th commit is made at.
func (r *pushRig) dated(clock string) func(int) string {
	if clock == "skewed" {
		return func(int) string { return gittest.Skewed() }
	}
	return r.after
}

// pushes is one push arm: per iteration, one commit of one 4 KiB file made
// with the timer stopped, dated date(n), then its push of main to receiver
// timed; with the timer stopped again, the push's advertisement alone and
// other's delta sync of it.
func (r *pushRig) pushes(b *testing.B, receiver, other codetest.Node, date func(n int) string) {
	b.Helper()
	url := repoURL(receiver)
	var server, adv, transfer, read, synced float64
	outside := 0
	delta := func() float64 {
		_, sum := observed(other.Metrics(), metrics.CodeCacheSyncDuration, map[string]string{"kind": "delta"})
		return sum
	}
	sync := func() {
		s, err := other.Cache().Sync(b.Context(), r.repo, codetest.Linearizable)
		if err != nil {
			b.Fatalf("the other node's sync: %v", err)
		}
		s.Release()
	}
	sync()
	pushed := 0
	for b.Loop() {
		b.StopTimer()
		at := date(r.n)
		r.p.Git().Env("GIT_AUTHOR_DATE="+at, "GIT_COMMITTER_DATE="+at).Commit(b, r.work,
			map[string]string{fmt.Sprintf("bench/f%06d.bin", r.n): randomBytes(4 << 10)}, fmt.Sprintf("push %d", r.n))
		r.n++
		_, serverBefore := observed(receiver.Metrics(), metrics.CodePushDuration,
			map[string]string{"outcome": "applied"})
		transferBefore := cpuMillis(receiver.Metrics(), gitproc.ClassTransfer)
		readBefore := cpuMillis(receiver.Metrics(), gitproc.ClassRead)
		b.StartTimer()
		mustPush(b, r.p, r.work, url, "main")
		b.StopTimer()
		_, serverAfter := observed(receiver.Metrics(), metrics.CodePushDuration,
			map[string]string{"outcome": "applied"})
		server += serverAfter - serverBefore
		transfer += cpuMillis(receiver.Metrics(), gitproc.ClassTransfer) - transferBefore
		read += cpuMillis(receiver.Metrics(), gitproc.ClassRead) - readBefore
		indexed, held := indexedPacks(b, receiver, r.repo)
		outside += held - indexed
		adv += float64(advertise(b, receiver, r.p.Token())) / float64(time.Millisecond)
		before := delta()
		sync()
		synced += delta() - before
		pushed++
		b.StartTimer()
	}
	b.ReportMetric(server/float64(pushed), "server-ms")
	b.ReportMetric(adv/float64(pushed), "adv-ms")
	b.ReportMetric(synced/float64(pushed), "sync-ms")
	b.ReportMetric(transfer/float64(pushed), "transfer-cpu-ms")
	b.ReportMetric(read/float64(pushed), "read-cpu-ms")
	b.ReportMetric(float64(outside)/float64(pushed), "packs-outside")
}

// walks is the step= arms on node, its copy's commit-graph graph: a scratch
// over the copy, as a push of one commit on top of main is received in, with
// two commits on main's tip written into it — one dated after its parent,
// one before the whole history — and each walk a push makes, timed alone
// from each.
func (r *pushRig) walks(b *testing.B, node codetest.Node, graph string) {
	b.Helper()
	s, err := node.Cache().Sync(b.Context(), r.repo, codetest.Linearizable)
	if err != nil {
		b.Fatal(err)
	}
	defer s.Release()
	sc, err := node.Cache().NewScratch(b.Context(), s, uuid.Must(uuid.NewV7()).String())
	if err != nil {
		b.Fatal(err)
	}
	defer sc.Remove()
	tip := r.tip(b)
	tree := strings.TrimSpace(r.p.Git().Run(b, sc.Dir, "rev-parse", tip+"^{tree}"))
	commit := func(date, msg string) string {
		return strings.TrimSpace(r.p.Git().Env("GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date).Run(b, sc.Dir,
			"commit-tree", tree, "-p", tip, "-m", msg))
	}
	tips := map[string]string{
		"normal": commit(r.after(r.n), "dated after its parent"),
		"skewed": commit(gittest.Skewed(), "dated before the history"),
	}
	commits, objects := len(r.commits)+r.n, s.Repo.LiveObjects
	srv, ctx := node.Host(), b.Context()
	steps := []struct {
		name string
		walk func(tip string) error
	}{
		{"force", func(new string) error {
			_, _, err := host.IsForced(ctx, srv, sc, objects, tip, new)
			return err
		}},
		{"introduced", func(new string) error {
			_, _, err := host.Introduced(ctx, srv, sc, objects, []string{new}, 0)
			return err
		}},
		{"path", func(new string) error {
			host.PathOf(ctx, srv, sc, objects, []string{new}, strings.Repeat("e", 40), 0)
			return nil
		}},
		{"connectivity", func(new string) error {
			// RECEIVE-PACK'S OWN CHECK, as git spells it (connected.c):
			// its arguments are git's, not the engine's.
			_, err := node.Pool().Run(ctx, gitproc.Cmd{
				Dir: sc.Dir, Alternate: sc.Alternate, Class: gitproc.ClassRead,
				Deadline: gitproc.ReadDeadline + gitproc.HistoryWalk(objects),
				Args: []string{"rev-list", "--objects", "--stdin", "--not", "--exclude-hidden=receive", "--all",
					"--quiet", "--alternate-refs"},
				Stdin: strings.NewReader(new + "\n"),
			})
			return err
		}},
	}
	for _, step := range steps {
		for _, at := range []string{"normal", "skewed"} {
			b.Run(fmt.Sprintf("step=%s/graph=%s/tip=%s", step.name, graph, at), func(b *testing.B) {
				walked := 0
				for b.Loop() {
					if err := step.walk(tips[at]); err != nil {
						b.Fatalf("%s from a tip %s: %v", step.name, at, err)
					}
					walked++
				}
				per := float64(b.Elapsed().Nanoseconds()) / float64(walked)
				b.ReportMetric(per/float64(commits), "ns/commit")
				b.ReportMetric(per/float64(objects), "ns/object")
			})
		}
	}
	if graph != "no" {
		return
	}
	// THE GRAPH'S OWN WRITE over the history, as the maintainer's
	// graph-first run writes it: what the window in which every walk above
	// goes by date lasts once a slot is free. Into the scratch, from main's
	// tip, so the copy the arms after it read is left as it was.
	b.Run("step=graph-write", func(b *testing.B) {
		written := filepath.Join(sc.Dir, "objects", "info", "commit-graph")
		for b.Loop() {
			b.StopTimer()
			if err := os.Remove(written); err != nil && !errors.Is(err, fs.ErrNotExist) {
				b.Fatal(err)
			}
			b.StartTimer()
			if _, err := node.Pool().Run(ctx, gitproc.Cmd{
				Dir: sc.Dir, Alternate: sc.Alternate, Class: gitproc.ClassRead, Deadline: time.Hour,
				Args:  []string{"commit-graph", "write", "--stdin-commits", "--no-progress"},
				Stdin: strings.NewReader(tip + "\n"),
			}); err != nil {
				b.Fatalf("write a commit-graph over the history: %v", err)
			}
		}
		b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(commits), "ns/commit")
	})
}

// coldHeld empties the fleet's second git node (coldNode) and holds every
// slot of its maintenance lane, so its maintainer writes nothing — no
// commit-graph, no index — until the release it answers.
func coldHeld(b *testing.B, f *codetest.SoloFleet) (codetest.Node, func()) {
	b.Helper()
	node := coldNode(b, f)
	var held []*gitproc.Reserved
	for range node.Pool().Slots(gitproc.ClassMaintenance) {
		r, err := node.Pool().Reserve()
		if err != nil {
			b.Fatalf("hold node %s's maintenance lane: %v", node.ID(), err)
		}
		held = append(held, r)
	}
	release := sync.OnceFunc(func() {
		for _, r := range held {
			r.Release()
		}
	})
	b.Cleanup(release)
	return node, release
}

// BenchmarkFetchWarm is what a CI fleet polling a repository costs a git
// node: every one of clients clients fetches main into a clone of its own, at
// once, under protocol v2 — ls-refs (a linearizable advertisement, a barrier
// on the code log) and a fetch of the one commit a writer pushed since — from
// the node the writer pushes to, whose cache is warm and whose maintainer runs
// as in production. packs is how many pushes built the repository, ALL OF
// THEM inside the maintainer's index with its bitmap before the first fetch
// (warmIndex); each iteration's push then adds a pack outside it, as a
// repository's pushes do between the maintainer's runs — which at one push an
// iteration never stand still for MidxDebounce, so the index is not written
// again inside an arm. What a pack count costs a LOOKUP, with and without an
// index, is BenchmarkPackLookup's (internal/code/cache); this is the request
// path a client hits beside the maintainer.
//
// mode is which index the node serves the fetches from (modes): own, the
// maintainer's own whole index with its bitmap; chain, the repository's
// published layers, installed after one publication of every pack it holds
// (publishChain) — so each iteration's push lands outside the chain, in the
// tail a chained repository serves between its publications.
//
// Each reports fetches a second, the p50 and p99 of one fetch, cpu-ms/fetch,
// the serving node's git CPU (serverCPU) a fetch, and packs-outside, the mean
// count of packs outside the index (or the chain) while the arm's fetches
// were served.
func BenchmarkFetchWarm(b *testing.B) {
	codetest.Quiet(b)
	for _, mode := range modes {
		for _, packs := range []int{1, 32} {
			for _, clients := range []int{1, 8, 64} {
				b.Run(fmt.Sprintf("mode=%s/clients=%d/packs=%d", mode, clients, packs), func(b *testing.B) {
					f, p := benchFleet(b, memobj.New(), 1)
					node := f.Nodes()[0]
					url := repoURL(node)
					work := p.Init()
					for i := range packs {
						p.Commit(work, map[string]string{"README.md": fmt.Sprintf("push %d\n", i)}, fmt.Sprintf("push %d", i))
						mustPush(b, p, work, url, "main")
					}
					rows, err := f.Rows(b.Context(), codetest.RepoName, codetest.Linearizable)
					if err != nil {
						b.Fatal(err)
					}
					if mode == modeChain {
						publishChain(b, f, node)
						warmChain(b, node, rows.Repo.ID)
					} else {
						warmIndex(b, node, rows.Repo.ID, packs)
					}
					dirs := make([]string, clients)
					for c := range dirs {
						dirs[c] = p.Clone(url)
					}
					var (
						mu      sync.Mutex
						took    []time.Duration
						cpu     float64
						outside int
					)
					fetches, rounds := 0, 0
					for b.Loop() {
						b.StopTimer()
						p.Commit(work, map[string]string{"README.md": fmt.Sprintf("fetch %d\n", fetches)}, "next")
						mustPush(b, p, work, url, "main")
						indexed, held := indexedPacks(b, node, rows.Repo.ID)
						outside += held - indexed
						rounds++
						before := serverCPU(node.Metrics())
						b.StartTimer()
						var wg sync.WaitGroup
						errs := make([]error, clients)
						for c, dir := range dirs {
							wg.Go(func() {
								at := time.Now()
								_, stderr, err := p.Git().TryContext(b.Context(), dir, nil,
									"-c", "protocol.version=2", "fetch", "--quiet", "origin", "main")
								elapsed := time.Since(at)
								if err != nil {
									errs[c] = fmt.Errorf("git fetch: %w\n%s", err, stderr)
								}
								mu.Lock()
								took = append(took, elapsed)
								mu.Unlock()
							})
						}
						wg.Wait()
						b.StopTimer()
						for _, err := range errs {
							if err != nil {
								b.Fatal(err)
							}
						}
						cpu += serverCPU(node.Metrics()) - before
						fetches += clients
						b.StartTimer()
					}
					slices.Sort(took)
					b.ReportMetric(float64(fetches)/b.Elapsed().Seconds(), "fetches/s")
					b.ReportMetric(quantile(took, 0.50), "p50-ms")
					b.ReportMetric(quantile(took, 0.99), "p99-ms")
					b.ReportMetric(cpu/float64(fetches), "cpu-ms/fetch")
					b.ReportMetric(float64(outside)/float64(rounds), "packs-outside")
				})
			}
		}
	}
}

// The index a warm arm's node serves from (BenchmarkFetchWarm's and
// BenchmarkCloneWarm's mode): modeOwn, the node's own whole multi-pack-index
// and bitmap, as a node serves a repository nobody has published; modeChain,
// the repository's published chain (internal/code/cache's layers.go), as
// every git node serves one whose maintainer publishes it.
const (
	modeOwn   = "own"
	modeChain = "chain"
)

// modes is every mode a warm arm runs, own first: BenchmarkCloneWarm's
// chain arms run on the fixture its own arms served, published between them.
var modes = []string{modeOwn, modeChain}

// publishChain is one publication of the repository by node, its
// maintainer as the only git node of f — the unit of the maintainer's pass,
// driven here because a fleet's compactors are never started (Node.Compactor)
// — asked again while it answers busy or waiting, for two minutes at the
// most. The candidate is the one the publication due listing names, or the
// rows' own where it names none yet (its debounce not run out), which is the
// shape a repair publication is planned from.
func publishChain(b *testing.B, f *codetest.SoloFleet, node codetest.Node) {
	b.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		rows, err := f.Rows(b.Context(), codetest.RepoName, codetest.Linearizable)
		if err != nil {
			b.Fatal(err)
		}
		cand := code.PublicationCandidate{ID: rows.Repo.ID, Name: codetest.RepoName,
			LivePacks: int64(len(rows.Packs)), Version: rows.Position}
		for _, p := range rows.Packs {
			cand.LiveObjects += p.Objects
		}
		due, err := node.Read(b.Context(), code.ReadQuery{Kind: code.ReadPublicationDue, Fresh: codetest.Linearizable})
		if err != nil {
			b.Fatal(err)
		}
		for _, c := range due.Publish {
			if c.ID == rows.Repo.ID {
				cand = c
			}
		}
		out, err := node.Compactor().Publish(b.Context(), cand)
		if out == compact.OutcomeApplied {
			return
		}
		if (out != compact.OutcomeBusy && out != compact.OutcomeWaiting) || time.Now().After(deadline) {
			b.Fatalf("node %s's publication of the repository: %s, %v", node.ID(), out, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// warmChain waits until node's copy of the repository has installed every
// layer of its published chain — what a chained copy serves from once the
// installer has placed the publication's layers — and logs how many of the
// packs it holds the chain covers: a publication leaves outside its chain
// whatever it did not take in (its tail), which a chained repository serves
// beside the chain until the next one.
func warmChain(b *testing.B, node codetest.Node, repoID string) {
	b.Helper()
	deadline := time.Now().Add(5 * time.Minute)
	for {
		s, err := node.Cache().Sync(b.Context(), repoID, codetest.Linearizable)
		if err != nil {
			b.Fatal(err)
		}
		ch := node.Cache().Chain(s)
		s.Release()
		if len(ch.Rows) > 0 && ch.Installed == len(ch.Rows) {
			indexed, held := indexedPacks(b, node, repoID)
			b.Logf("node %s serves a chain of %d layers over %d of its %d packs", node.ID(), len(ch.Rows),
				indexed, held)
			return
		}
		if time.Now().After(deadline) {
			b.Fatalf("node %s installed %d of the %d published layers in five minutes (refused: %v)", node.ID(),
				ch.Installed, len(ch.Rows), ch.Refused)
		}
		time.Sleep(time.Second)
	}
}

// warmIndex waits until node's copy of the repository is maintained over
// packs packs — every pack it holds, for packs 0: the maintainer's
// multi-pack-index covering every one, with its bitmap — what a cache that
// has stood still for MidxDebounce serves from.
func warmIndex(b *testing.B, node codetest.Node, repoID string, packs int) {
	b.Helper()
	deadline := time.Now().Add(5 * time.Minute)
	for {
		indexed, held := indexedPacks(b, node, repoID)
		dir := incarnation(b, node, repoID)
		bitmaps, _ := filepath.Glob(filepath.Join(dir, "objects", "pack", "multi-pack-index-*.bitmap"))
		want := packs
		if want <= 0 {
			want = held
		}
		if held > 0 && held == want && indexed == want && len(bitmaps) > 0 {
			return
		}
		if time.Now().After(deadline) {
			b.Fatalf("node %s indexed %d of %d packs (%d bitmaps) in five minutes, want all %d", node.ID(),
				indexed, held, len(bitmaps), packs)
		}
		time.Sleep(time.Second)
	}
}

// incarnation is the directory of node's newest copy of the repository —
// read from the cache's layout (repos/<id>.<n>.git) without a sync, so
// looking does not move what a timed request finds.
func incarnation(b *testing.B, node codetest.Node, repoID string) string {
	b.Helper()
	found, err := filepath.Glob(filepath.Join(node.CacheDir(), "repos", repoID+".*.git"))
	if err != nil || len(found) == 0 {
		b.Fatalf("node %s holds no copy of %s (%v)", node.ID(), repoID, err)
	}
	slices.SortFunc(found, func(x, y string) int {
		return cmp.Compare(incarnationNumber(x), incarnationNumber(y))
	})
	return found[len(found)-1]
}

// incarnationNumber is the n of a copy's directory name, <id>.<n>.git.
func incarnationNumber(dir string) int {
	name := strings.TrimSuffix(filepath.Base(dir), ".git")
	n, _ := strconv.Atoi(name[strings.LastIndexByte(name, '.')+1:])
	return n
}

// indexedPacks is how many packs node's copy of the repository holds and how
// many of them its multi-pack-index covers: the count its header states
// (git's format — "MIDX", version, hash version, chunk count, base count, then
// the packs as a four-byte count) — summed over every layer its chain file
// names where the copy holds an installed chain and no whole index, since
// each layer's header counts only the packs it adds — and none without either.
func indexedPacks(b *testing.B, node codetest.Node, repoID string) (indexed, packs int) {
	b.Helper()
	dir := filepath.Join(incarnation(b, node, repoID), "objects", "pack")
	held, err := filepath.Glob(filepath.Join(dir, "pack-*.pack"))
	if err != nil {
		b.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "multi-pack-index")); err == nil {
		return midxPacks(b, node, filepath.Join(dir, "multi-pack-index")), len(held)
	}
	layers := filepath.Join(dir, "multi-pack-index.d")
	chain, err := os.ReadFile(filepath.Join(layers, "multi-pack-index-chain"))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, len(held)
	}
	if err != nil {
		b.Fatal(err)
	}
	for _, sum := range strings.Fields(string(chain)) {
		indexed += midxPacks(b, node, filepath.Join(layers, "multi-pack-index-"+sum+".midx"))
	}
	return indexed, len(held)
}

// midxPacks is the pack count a multi-pack-index file's header states — a
// whole index's or one layer's. A file gone between the listing and the read
// (the installer's swap took the incarnation's layer) counts none, so the
// caller's poll looks again.
func midxPacks(b *testing.B, node codetest.Node, path string) int {
	b.Helper()
	head := make([]byte, 12)
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0
	}
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := io.ReadFull(f, head); err != nil || string(head[:4]) != "MIDX" {
		b.Fatalf("node %s's %s has no header (%v)", node.ID(), filepath.Base(path), err)
	}
	return int(binary.BigEndian.Uint32(head[8:12]))
}

// treeFilesPerPush is how many files each push of a tree fixture adds, and
// treeFileBytes each one's size: fifty files of 3 000 seeded random bytes,
// 150 KB — the median push ADR-0054 sizes the host for — spread over fifty
// directories.
const (
	treeFilesPerPush = 50
	treeFileBytes    = 3_000
	// treeDirFiles is how many files a directory of the tree ends up with.
	treeDirFiles = 100
)

// treeFixture is a repository built by packs pushes to url, each one commit
// adding treeFilesPerPush files to a REAL TREE — packs × treeFilesPerPush
// files in directories of treeDirFiles, file f in directory f mod D, so every
// push touches fifty directories as a commit to a real repository does — and
// answers its commits and the CONE a sparse checkout takes: the first tenth
// of the directories.
func treeFixture(b *testing.B, p *codetest.Person, url string, packs int) (commits, cone []string) {
	b.Helper()
	dirs := packs * treeFilesPerPush / treeDirFiles
	src := gittest.Empty(b)
	gittest.FastImport(b, p.Git(), src, func(w *bufio.Writer) error {
		data := rand.NewChaCha8([32]byte{'t', 'r', 'e', 'e'})
		for i := range packs {
			msg := fmt.Sprintf("push %d\n", i)
			fmt.Fprintf(w, "commit refs/heads/main\ncommitter Crewlet Bench <bench@example.com> %d +0000\ndata %d\n%s",
				1_767_225_600+i, len(msg), msg)
			for j := range treeFilesPerPush {
				file := i*treeFilesPerPush + j
				fmt.Fprintf(w, "M 100644 inline d%04d/f%06d.bin\ndata %d\n", file%dirs, file, treeFileBytes)
				if _, err := io.CopyN(w, data, treeFileBytes); err != nil {
					return err
				}
				if err := w.WriteByte('\n'); err != nil {
					return err
				}
			}
		}
		return nil
	})
	commits = strings.Fields(p.Git().Run(b, src, "rev-list", "--reverse", "main"))
	for _, c := range commits {
		mustPush(b, p, src, url, c+":refs/heads/main")
	}
	if err := os.RemoveAll(src); err != nil {
		b.Fatal(err)
	}
	for d := range max(1, dirs/10) {
		cone = append(cone, fmt.Sprintf("d%04d", d))
	}
	return commits, cone
}

// cloneShapes are the clones a person or a box makes, by what each fetches:
//
//   - full: every object (no checkout, so the figure is the transfer's);
//   - blobless+checkout: every commit and tree, then a checkout whose ONE
//     lazy fetch asks for every blob of the tree;
//   - depth1+blobless+cone: the tip's commit and trees, then a sparse
//     checkout of the cone, whose lazy fetch asks for the cone's blobs alone
//     — how a box clones for one sha;
//   - treeless+cone: every commit and no tree, then the cone — trees and
//     blobs fetched lazily, level by level.
var cloneShapes = []struct {
	name  string
	clone []string
	// after runs in the clone once it is made, nil for none; cone is
	// appended to it.
	after []string
}{
	{"full", []string{"--no-checkout"}, nil},
	{"blobless+checkout", []string{"--filter=blob:none", "--no-checkout"}, []string{"checkout", "--quiet", "main"}},
	{"depth1+blobless+cone", []string{"--depth=1", "--filter=blob:none", "--sparse"},
		[]string{"sparse-checkout", "set"}},
	{"treeless+cone", []string{"--filter=tree:0", "--sparse"}, []string{"sparse-checkout", "set"}},
}

// cloneInto is one clone of shape from url into a fresh directory under root,
// its lazy fetches going to url too, and the directory.
func cloneInto(ctx context.Context, p *codetest.Person, root, url string, shape int, cone []string) (string, error) {
	s := cloneShapes[shape]
	dir, err := os.MkdirTemp(root, "clone")
	if err != nil {
		return "", err
	}
	args := slices.Concat([]string{"-c", "protocol.version=2", "clone", "--quiet"}, s.clone, []string{url, dir})
	if _, stderr, err := p.Git().TryContext(ctx, "", nil, args...); err != nil {
		return dir, fmt.Errorf("git clone %s: %w\n%s", s.name, err, stderr)
	}
	if s.after == nil {
		return dir, nil
	}
	after := slices.Concat([]string{"-c", "protocol.version=2"}, s.after)
	if after[len(after)-1] == "set" {
		after = append(after, cone...)
	}
	if _, stderr, err := p.Git().TryContext(ctx, dir, nil, after...); err != nil {
		return dir, fmt.Errorf("git %s after a %s clone: %w\n%s", s.after[0], s.name, err, stderr)
	}
	return dir, nil
}

// packBytes is every byte a clone stored under its objects/pack — the packs
// its clone and its lazy fetches brought.
func packBytes(b *testing.B, dir string) int64 {
	b.Helper()
	var total int64
	err := filepath.WalkDir(filepath.Join(dir, ".git", "objects", "pack"), func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err == nil {
			total += info.Size()
		}
		return err
	})
	if err != nil {
		b.Fatal(err)
	}
	return total
}

// requestCounter is a reverse proxy in front of a git node that counts what
// every upload-pack request it forwards carried, decoded — the bytes a
// request is held to on the node (the spool's threshold and the repository's
// bound are on decoded bytes), whatever compression git sent them under —
// and the requests the node SERVED: each upload-pack POST, and each fetch's
// advertisement (GET info/refs?service=git-upload-pack), each of them an
// upload-pack the node ran in its transfer lane.
type requestCounter struct {
	srv     *httptest.Server
	decoded atomic.Int64
	posts   atomic.Int64
	adverts atomic.Int64
}

// countRequests starts a counter in front of node, until b ends.
func countRequests(b *testing.B, node codetest.Node) *requestCounter {
	b.Helper()
	target, err := url.Parse(node.URL())
	if err != nil {
		b.Fatal(err)
	}
	c := &requestCounter{}
	proxy := &httputil.ReverseProxy{
		Rewrite:   func(r *httputil.ProxyRequest) { r.SetURL(target) },
		Transport: node.Client().Transport,
		// COUNTED AS THE NODE ANSWERS THEM, never as they arrive: git
		// asks every new connection's first request without credentials
		// and again with them once it is challenged, and the guard's 401
		// runs no git.
		ModifyResponse: func(resp *http.Response) error {
			if resp.StatusCode != http.StatusOK {
				return nil
			}
			switch r := resp.Request; {
			case strings.HasSuffix(r.URL.Path, "/info/refs") && r.URL.Query().Get("service") == "git-upload-pack":
				c.adverts.Add(1)
			case strings.HasSuffix(r.URL.Path, "/git-upload-pack"):
				c.posts.Add(1)
			}
			return nil
		},
	}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/git-upload-pack") {
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			body := io.Reader(bytes.NewReader(raw))
			if r.Header.Get("Content-Encoding") == "gzip" {
				if body, err = gzip.NewReader(body); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
			}
			n, _ := io.Copy(io.Discard, body)
			c.decoded.Add(n)
			r.Body = io.NopCloser(bytes.NewReader(raw))
		}
		proxy.ServeHTTP(w, r)
	}))
	b.Cleanup(c.srv.Close)
	return c
}

// coldNode empties the fleet's second git node — stopped, its directory
// removed, started again — so its cache has never served the repository, and
// answers it.
func coldNode(b *testing.B, f *codetest.SoloFleet) codetest.Node {
	b.Helper()
	g := f.GitNode(1)
	dir := g.Node().CacheDir()
	g.Stop(b.Context())
	if err := os.RemoveAll(dir); err != nil {
		b.Fatal(err)
	}
	g.Restart(b)
	return g.Node()
}

// BenchmarkCloneCold is the FIRST CLONE ON A GIT NODE THAT HAS NEVER SERVED
// THE REPOSITORY — one that just joined, or whose budget evicted it: the cold
// sync (a GET a push, FetchConcurrency at once), the round's index past
// MidxRoundPacks packs and the first upload-pack, in series before the
// clone's first byte, and then the transfer. The repository is a tree
// fixture (treeFixture) of packs pushes through the fleet's first git node;
// every iteration clones from its second, emptied first (coldNode).
//
//   - packs=200|2000 is 10 000 or 100 000 files; 2000 runs only under
//     CREWLET_BENCH_LARGE.
//   - objects=s3fake|real is the store the cold sync reads (benchObjects).
//   - shape is the clone (cloneShapes); clients is how many identical clones
//     start at once on the cold node, sharing its one sync.
//
// Each reports, a clone: the p50 and p99 of one clone; server-cpu-ms, the
// cold node's git CPU (serverCPU) — its upload-packs, and the sync's own peel
// and round index; sync-ms, the cold sync's duration; bytes, every pack byte
// the clone stored; and for blobless+checkout, lazy-request-bytes, what the
// checkout's lazy fetch asked for, decoded (requestCounter) — past 1 MiB a
// request is spooled. Every arm of a real bucket also reports first-byte-ms,
// measured once for the bucket.
//
// op=blob is one lazy fetch of one blob into a blobless clone, from the first
// node — warm, its maintainer's bitmap written — as every file a blobless
// checkout reads for the first time costs, timed with its server-cpu-ms.
//
// op=round-index, on a repository of cache.MidxRoundPacks packs or more, is
// the index the cold sync's round writes over them before it answers, timed
// alone (roundIndex) — so an arm's sync-ms splits into the GETs and that
// index, which is what the round's share of a cold request is.
func BenchmarkCloneCold(b *testing.B) {
	codetest.Quiet(b)
	for _, packs := range []int{200, 2000} {
		for _, objects := range []string{"s3fake", "real"} {
			b.Run(fmt.Sprintf("packs=%d/objects=%s", packs, objects), func(b *testing.B) {
				if packs > 200 {
					gittest.Large(b, "2 000 pushes of a 100 000-file tree through the host")
				}
				backend := benchObjects(b, objects)
				f, p := benchFleet(b, backend, 2)
				commits, cone := treeFixture(b, p, repoURL(f.Nodes()[0]), packs)
				// MEASURED HERE AND REPORTED BY EVERY LEAF: a benchmark with
				// sub-benchmarks is never measured itself, so a metric it
				// reports is printed nowhere, and b.Loop's first call clears
				// any reported before it.
				firstByte := -1.0
				if objects == "real" {
					rows, err := f.Rows(b.Context(), codetest.RepoName, codetest.Linearizable)
					if err != nil {
						b.Fatal(err)
					}
					firstByte = firstByteMillis(b, backend, rows)
				}
				cloners := make([]*codetest.Person, coldCloners)
				for i := range cloners {
					cloners[i] = codetest.NewPerson(b, f, cloner(i))
				}
				for shape := range cloneShapes {
					for _, clients := range []int{1, coldCloners} {
						b.Run(fmt.Sprintf("shape=%s/clients=%d", cloneShapes[shape].name, clients), func(b *testing.B) {
							coldClones(b, f, cloners[:clients], shape, cone, firstByte)
						})
					}
				}
				b.Run("op=blob", func(b *testing.B) { lazyBlobs(b, f, p, commits[len(commits)-1], firstByte) })
				if packs >= cache.MidxRoundPacks {
					b.Run("op=round-index", func(b *testing.B) { roundIndex(b, f) })
				}
			})
		}
	}
}

// roundIndex is the op=round-index arm: `multi-pack-index write
// --stdin-packs` over every pack of the repository, in the read lane with the
// serve configuration — the index a cold sync's round writes past
// cache.MidxRoundPacks packs, as the round runs it — timed alone, the index
// removed between iterations with the timer stopped. In a scratch over the
// first node's copy with every pack hard-linked into it, so no node's cache
// is written, once that node's maintainer has written its bitmap (warmBitmap),
// so no run of its own goes on beside it. Reports packs, the count indexed.
func roundIndex(b *testing.B, f *codetest.SoloFleet) {
	b.Helper()
	node := f.Nodes()[0]
	rows, err := f.Rows(b.Context(), codetest.RepoName, codetest.Linearizable)
	if err != nil {
		b.Fatal(err)
	}
	warmBitmap(b, node, rows.Repo.ID)
	s, err := node.Cache().Sync(b.Context(), rows.Repo.ID, codetest.Linearizable)
	if err != nil {
		b.Fatal(err)
	}
	defer s.Release()
	sc, err := node.Cache().NewScratch(b.Context(), s, uuid.Must(uuid.NewV7()).String())
	if err != nil {
		b.Fatal(err)
	}
	defer sc.Remove()
	from, into := filepath.Join(s.Dir, "objects", "pack"), filepath.Join(sc.Dir, "objects", "pack")
	if err := os.MkdirAll(into, 0o755); err != nil {
		b.Fatal(err)
	}
	indexes, err := filepath.Glob(filepath.Join(from, "pack-*.idx"))
	if err != nil || len(indexes) < cache.MidxRoundPacks {
		b.Fatalf("the copy holds %d packs (%v), under the %d a round indexes", len(indexes), err,
			cache.MidxRoundPacks)
	}
	var names strings.Builder
	for _, idx := range indexes {
		base := strings.TrimSuffix(filepath.Base(idx), ".idx")
		for _, ext := range []string{".pack", ".idx", ".rev"} {
			if err := os.Link(filepath.Join(from, base+ext), filepath.Join(into, base+ext)); err != nil {
				b.Fatalf("link %s%s into the scratch: %v", base, ext, err)
			}
		}
		names.WriteString(base + ".idx\n")
	}
	written := filepath.Join(into, "multi-pack-index")
	for b.Loop() {
		b.StopTimer()
		if err := os.Remove(written); err != nil && !errors.Is(err, fs.ErrNotExist) {
			b.Fatal(err)
		}
		b.StartTimer()
		if _, err := node.Pool().Run(b.Context(), gitproc.Cmd{
			Dir: sc.Dir, Alternate: sc.Alternate, Class: gitproc.ClassRead, Deadline: time.Hour,
			Args:   []string{"multi-pack-index", "write", "--stdin-packs", "--no-progress"},
			Config: node.Cache().ServeConfig(), Stdin: strings.NewReader(names.String()),
		}); err != nil {
			b.Fatalf("write the round's index over %d packs: %v", len(indexes), err)
		}
	}
	b.ReportMetric(float64(len(indexes)), "packs")
}

// coldClones is one BenchmarkCloneCold arm: per iteration, the second node
// emptied with the timer stopped, then one clone of shape from it by each of
// people at once. People, not one token, because the arm is of eight clones
// by eight people: one token's quarter of the spool (256 MiB on a lane of
// sixteen) holds all eight lazy fetches of the 100 000-file tree, 40 MB, so
// under one token they would not queue either, and a token's wider bursts are
// TestACredentialsEndedSpoolsOutlastingTheWait's (doc.go says what the
// quarter costs).
func coldClones(b *testing.B, f *codetest.SoloFleet, people []*codetest.Person, shape int, cone []string,
	firstByte float64) {

	b.Helper()
	clients := len(people)
	var (
		took                []time.Duration
		cpu, synced         float64
		stored, lazyRequest int64
	)
	root := b.TempDir()
	lazy := cloneShapes[shape].name == "blobless+checkout"
	clones := 0
	for b.Loop() {
		b.StopTimer()
		node := coldNode(b, f)
		target := repoURL(node)
		var counter *requestCounter
		if lazy {
			counter = countRequests(b, node)
			target = counter.srv.URL + "/git/" + codetest.RepoName
		}
		dirs := make([]string, clients)
		errs := make([]error, clients)
		times := make([]time.Duration, clients)
		b.StartTimer()
		var wg sync.WaitGroup
		for c := range clients {
			wg.Go(func() {
				at := time.Now()
				dirs[c], errs[c] = cloneInto(b.Context(), people[c], root, target, shape, cone)
				times[c] = time.Since(at)
			})
		}
		wg.Wait()
		b.StopTimer()
		for _, err := range errs {
			if err != nil {
				b.Fatal(err)
			}
		}
		took = append(took, times...)
		cpu += serverCPU(node.Metrics())
		_, sum := observed(node.Metrics(), metrics.CodeCacheSyncDuration, map[string]string{"kind": "full"})
		synced += sum
		for _, dir := range dirs {
			stored += packBytes(b, dir)
			if err := os.RemoveAll(dir); err != nil {
				b.Fatal(err)
			}
		}
		if lazy {
			lazyRequest += counter.decoded.Load()
		}
		clones += clients
		b.StartTimer()
	}
	slices.Sort(took)
	b.ReportMetric(quantile(took, 0.50), "p50-ms")
	b.ReportMetric(quantile(took, 0.99), "p99-ms")
	b.ReportMetric(cpu/float64(clones), "server-cpu-ms")
	b.ReportMetric(synced/float64(b.N), "sync-ms")
	b.ReportMetric(float64(stored)/float64(clones), "bytes")
	if lazy {
		b.ReportMetric(float64(lazyRequest)/float64(clones), "lazy-request-bytes")
	}
	if firstByte >= 0 {
		b.ReportMetric(firstByte, "first-byte-ms")
	}
}

// warmBitmap waits until node's copy of the repository carries its
// maintainer's bitmap — a warm cache in production has written it within
// cache.MidxDebounce of the last push — so a warm figure is the one a person
// meets, not the minute after a push.
func warmBitmap(b *testing.B, node codetest.Node, repoID string) {
	b.Helper()
	deadline := time.Now().Add(5 * time.Minute)
	for {
		s, err := node.Cache().Sync(b.Context(), repoID, codetest.Linearizable)
		if err != nil {
			b.Fatal(err)
		}
		found, _ := filepath.Glob(filepath.Join(s.Dir, "objects", "pack", "multi-pack-index-*.bitmap"))
		s.Release()
		if len(found) > 0 {
			return
		}
		if time.Now().After(deadline) {
			b.Fatalf("node %s wrote no bitmap for the repository in five minutes", node.ID())
		}
		time.Sleep(time.Second)
	}
}

// lazyBlobs is the op=blob arm: a blobless clone of the tip made with the
// timer stopped, then per iteration one `git cat-file -p` of a blob it lacks
// — one lazy fetch of one blob — from the first node, warm.
func lazyBlobs(b *testing.B, f *codetest.SoloFleet, p *codetest.Person, tip string, firstByte float64) {
	b.Helper()
	node := f.Nodes()[0]
	rows, err := f.Rows(b.Context(), codetest.RepoName, codetest.Linearizable)
	if err != nil {
		b.Fatal(err)
	}
	warmBitmap(b, node, rows.Repo.ID)
	dir := filepath.Join(b.TempDir(), "blobless")
	p.Git().Run(b, "", "-c", "protocol.version=2", "clone", "--quiet", "--filter=blob:none", "--no-checkout",
		repoURL(node), dir)
	var blobs []string
	for line := range strings.Lines(p.Git().Run(b, dir, "ls-tree", "-r", "--format=%(objectname)", tip)) {
		blobs = append(blobs, strings.TrimSpace(line))
	}
	var cpu float64
	n := 0
	for b.Loop() {
		if n == len(blobs) {
			b.Fatalf("the arm read every one of the tip's %d blobs: run it for less time", len(blobs))
		}
		b.StopTimer()
		before := serverCPU(node.Metrics())
		b.StartTimer()
		if _, stderr, err := p.Git().TryContext(b.Context(), dir, nil, "-c", "protocol.version=2", "cat-file", "-p",
			blobs[n]); err != nil {
			b.Fatalf("git cat-file -p of a blob a blobless clone lacks: %v\n%s", err, stderr)
		}
		b.StopTimer()
		cpu += serverCPU(node.Metrics()) - before
		n++
		b.StartTimer()
	}
	b.ReportMetric(cpu/float64(n), "server-cpu-ms")
	if firstByte >= 0 {
		b.ReportMetric(firstByte, "first-byte-ms")
	}
}

// blameFile is the file BenchmarkLazyBlame's history rewrites, blameLines its
// length in lines and blameCommits the commits that rewrite it.
const (
	blameFile    = "blame.txt"
	blameLines   = 200
	blameCommits = 1000
)

// blameHistory is a bare repository whose main holds blameCommits commits of
// blameFile, each rewriting one of its blameLines lines in turn, built by one
// fast-import through p's git.
func blameHistory(b *testing.B, p *codetest.Person) string {
	b.Helper()
	src := gittest.Empty(b)
	lines := make([]string, blameLines)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %03d as first written", i)
	}
	gittest.FastImport(b, p.Git(), src, func(w *bufio.Writer) error {
		for c := range blameCommits {
			lines[c%blameLines] = fmt.Sprintf("line %03d as commit %04d left it", c%blameLines, c)
			body := strings.Join(lines, "\n") + "\n"
			msg := fmt.Sprintf("commit %04d\n", c)
			fmt.Fprintf(w, "commit refs/heads/main\ncommitter Crewlet Bench <bench@example.com> %d +0000\ndata %d\n%s",
				1_767_225_600+c, len(msg), msg)
			fmt.Fprintf(w, "M 100644 inline %s\ndata %d\n%s\n", blameFile, len(body), body)
		}
		return nil
	})
	return src
}

// BenchmarkLazyBlame is ONE `git blame` IN A BLOBLESS CLONE of a file
// blameCommits commits rewrote, by a token bound to nobody — a CI job's, or a
// box's answering a history question with its own git — and what it costs
// the git node it lazily fetches from: each fetch is an advertisement and an
// upload-pack, both runs of the transfer lane charged to the token's share
// (gitproc's fair share), and the figures are what the share bounds in place
// of a lazy-fetch limiter of the host's own (code-host.md, "Fair share"):
// fetches and advertisements a blame, the bytes they bring, and the server
// CPU they spend. Each operation is a fresh clone made with the timer
// stopped, from a warm node whose maintainer has written the repository's
// bitmap (warmBitmap), through a proxy that counts the requests
// (requestCounter).
func BenchmarkLazyBlame(b *testing.B) {
	f, p := benchFleet(b, memobj.New(), 1)
	node := f.Nodes()[0]
	mustPush(b, p, blameHistory(b, p), repoURL(node), "main")
	rows, err := f.Rows(b.Context(), codetest.RepoName, codetest.Linearizable)
	if err != nil {
		b.Fatal(err)
	}
	warmBitmap(b, node, rows.Repo.ID)
	ci := codetest.NewPerson(b, f, "")
	counter := countRequests(b, node)
	url := counter.srv.URL + "/git/" + codetest.RepoName
	var (
		fetches, adverts, brought int64
		cpu                       float64
	)
	for b.Loop() {
		b.StopTimer()
		dir := filepath.Join(b.TempDir(), "blobless")
		ci.Git().Run(b, "", "-c", "protocol.version=2", "clone", "--quiet", "--filter=blob:none", "--no-checkout",
			url, dir)
		posts0, adverts0, bytes0, cpu0 := counter.posts.Load(), counter.adverts.Load(), packBytes(b, dir),
			serverCPU(node.Metrics())
		b.StartTimer()
		out := ci.Git().Run(b, dir, "-c", "protocol.version=2", "blame", "--porcelain", "main", "--", blameFile)
		b.StopTimer()
		if got := strings.Count(out, "\n\t"); got != blameLines {
			b.Fatalf("git blame named %d lines of a %d-line file", got, blameLines)
		}
		fetches += counter.posts.Load() - posts0
		adverts += counter.adverts.Load() - adverts0
		brought += packBytes(b, dir) - bytes0
		cpu += serverCPU(node.Metrics()) - cpu0
		if err := os.RemoveAll(dir); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
	}
	n := float64(b.N)
	b.ReportMetric(float64(fetches)/n, "fetches/blame")
	b.ReportMetric(float64(adverts)/n, "advertisements/blame")
	b.ReportMetric(float64(brought)/n/1024, "KiB/blame")
	b.ReportMetric(cpu/n, "server-cpu-ms/blame")
}

// BenchmarkCloneWarm is IDENTICAL CLONES OF ONE SHA AT ONCE FROM A WARM
// CACHE — a CI fleet, or a run of boxes for one change — each its own
// upload-pack counting and packing the same objects: what a response cache
// for identical requests would save is this figure's to decide. The node's
// maintainer has written the repository's bitmap (warmBitmap), and every
// push was received as a person's git pushes — thin, completed by
// receive-pack with the whole bases it borrowed — so what a clone is sent
// follows cache.ServeConfig's pack.allowPackReuse (cache's
// BenchmarkPackReuse holds both values side by side over the same layout).
//
//   - history=tree is the 200-push tree fixture (treeFixture): every push
//     ADDS fifty files.
//   - history=edits is gittest.History's seeded history of edits — every
//     commit four files of one directory, each version a window of its file
//     rewritten — pushed as 200 pushes (pushEdits), so every push edits
//     files an earlier one wrote, as a team's pushes do.
//   - mode=own serves from the node's own whole index and bitmap; mode=chain
//     from the repository's published chain, installed after ONE publication
//     of the same fixture once the own arms have run (publishChain), so the
//     two modes clone one history and the chain arms differ by the index:
//     the chain, and whatever the publication left outside it, which
//     warmChain logs.
//
// Each reports the p50 and p99 of one clone, the serving node's git CPU
// (serverCPU) a round of clients clones and a clone, and bytes, every pack
// byte a clone stored.
func BenchmarkCloneWarm(b *testing.B) {
	codetest.Quiet(b)
	for _, history := range []string{"tree", "edits"} {
		b.Run("history="+history, func(b *testing.B) {
			f, p := benchFleet(b, memobj.New(), 1)
			node := f.Nodes()[0]
			var cone []string
			switch history {
			case "tree":
				_, cone = treeFixture(b, p, repoURL(node), 200)
			case "edits":
				cone = pushEdits(b, p, repoURL(node), editsPushes)
			}
			rows, err := f.Rows(b.Context(), codetest.RepoName, codetest.Linearizable)
			if err != nil {
				b.Fatal(err)
			}
			warmBitmap(b, node, rows.Repo.ID)
			for _, mode := range modes {
				if mode == modeChain {
					publishChain(b, f, node)
					warmChain(b, node, rows.Repo.ID)
				}
				for _, shape := range []int{0, 2} {
					for _, clients := range []int{1, 8, 32} {
						b.Run(fmt.Sprintf("mode=%s/shape=%s/clients=%d", mode, cloneShapes[shape].name, clients),
							func(b *testing.B) { warmClones(b, node, p, shape, clients, cone) })
					}
				}
			}
		})
	}
}

// editsBytes and editsPushes are BenchmarkCloneWarm's history=edits: a
// gittest.History of 20 MB — 26 files of 4 KiB, 8 960 commits — in 200
// pushes, as many as the tree fixture's.
const (
	editsBytes  = 20_000_000
	editsPushes = 200
)

// pushEdits pushes gittest.History's history of editsBytes to url as pushes
// pushes of consecutive commits, main moved to each one's last in turn, and
// answers the cone a sparse checkout takes: the history's one directory.
func pushEdits(b *testing.B, p *codetest.Person, url string, pushes int) []string {
	b.Helper()
	src, commits := gittest.History(b, p.Git(), editsBytes)
	for k := 1; k <= pushes; k++ {
		mustPush(b, p, src, url, commits[k*len(commits)/pushes-1]+":refs/heads/main")
	}
	if err := os.RemoveAll(src); err != nil {
		b.Fatal(err)
	}
	return []string{"d000"}
}

// warmClones is one BenchmarkCloneWarm arm: rounds of clients identical
// clones of shape from node at once.
func warmClones(b *testing.B, node codetest.Node, p *codetest.Person, shape, clients int, cone []string) {
	b.Helper()
	root := b.TempDir()
	var (
		took   []time.Duration
		cpu    float64
		stored int64
	)
	rounds := 0
	for b.Loop() {
		b.StopTimer()
		before := serverCPU(node.Metrics())
		dirs := make([]string, clients)
		errs := make([]error, clients)
		times := make([]time.Duration, clients)
		b.StartTimer()
		var wg sync.WaitGroup
		for c := range clients {
			wg.Go(func() {
				at := time.Now()
				dirs[c], errs[c] = cloneInto(b.Context(), p, root, repoURL(node), shape, cone)
				times[c] = time.Since(at)
			})
		}
		wg.Wait()
		b.StopTimer()
		for _, err := range errs {
			if err != nil {
				b.Fatal(err)
			}
		}
		cpu += serverCPU(node.Metrics()) - before
		took = append(took, times...)
		for _, dir := range dirs {
			stored += packBytes(b, dir)
			if err := os.RemoveAll(dir); err != nil {
				b.Fatal(err)
			}
		}
		rounds++
		b.StartTimer()
	}
	slices.Sort(took)
	b.ReportMetric(quantile(took, 0.50), "p50-ms")
	b.ReportMetric(quantile(took, 0.99), "p99-ms")
	b.ReportMetric(cpu/float64(rounds), "server-cpu-ms/round")
	b.ReportMetric(cpu/float64(rounds*clients), "server-cpu-ms/clone")
	b.ReportMetric(float64(stored)/float64(rounds*clients), "bytes")
}
