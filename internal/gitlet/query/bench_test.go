package query_test

import (
	"bufio"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitlettest"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/gitlet/query"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/memobj"
	"github.com/crewlet/crewlet/internal/objstore/s3obj"
	"github.com/crewlet/crewlet/internal/objstore/s3obj/s3fake"
	"github.com/crewlet/crewlet/internal/statelog"
)

// BenchmarkCodeRead times a seat's read by object id whole through a Gitlet
// node's query server (its sync, check, share unit and git) over a repository
// pushed as a host takes one: a gittest.History a session at a time, then a
// 3 MB file, a WIDE TREE under src/ and an edit across it (readTops). Its arms
// are history= (readSizes), objects= (readStores), kind= (readRig.kinds),
// cache= (warm, miss, memo, cold) and fresh= (readFloors); the rig and its
// figures are in (package doc, "Measured"). The figures decide
// gitlet.GitShareWait, max(2 s, the slowest warm kind's p95 rounded up to a
// second) under gitproc.ReadDeadline/4; confirm a warm read is well inside
// gitproc.ReadDeadline and a cold one inside statelog.ReadBudget from every
// store; and hold the memo's premise: a hit at least ten times faster than
// the read it saves, or the memo costs a sixteenth of the budget for nothing.
func BenchmarkCodeRead(b *testing.B) {
	gitlettest.Quiet(b)
	for _, size := range readSizes {
		b.Run("history="+size.name, func(b *testing.B) {
			if size.large {
				gittest.Large(b, "a 1 GB history under a 100 000-file tree, fast-imported and pushed a session at a time")
			}
			src := newReadSource(b, size)
			for _, store := range readStores {
				b.Run("objects="+store.name, func(b *testing.B) {
					r := newReadRig(b, src, store.open(b))
					if store.warm {
						for _, k := range r.kinds(b) {
							b.Run("kind="+k.name+"/cache=warm", func(b *testing.B) { r.warm(b, k) })
							b.Run("kind="+k.name+"/cache=miss", func(b *testing.B) { r.miss(b, k) })
							b.Run("kind="+k.name+"/cache=memo", func(b *testing.B) { r.memo(b, k) })
						}
						for _, f := range readFloors {
							b.Run("kind=tree-root/cache=memo/fresh="+f.name, func(b *testing.B) { r.floored(b, f) })
						}
					}
					b.Run("kind=tree-root/cache=cold", func(b *testing.B) { r.cold(b) })
				})
			}
		})
	}
}

// readStore is one objects= arm: the store a rig's fleet keeps the
// repository's packs in.
type readStore struct {
	name string
	// open is the arm's store, or a skip of the arm.
	open func(b *testing.B) objstore.Backend
	// warm is whether the arm times the warm kinds as well as the cold one:
	// mem's alone, since a warm read reads no object and another store would
	// time the same files again.
	warm bool
}

// readStores are the objects= arms: the in-memory twin; the S3 fake at both
// ends of S3's documented 100–200 ms to a request's first byte; and the
// bucket CREWLET_BENCH_S3_* names (real), skipped when none is.
var readStores = []readStore{
	{name: "mem", open: func(*testing.B) objstore.Backend { return memobj.New() }, warm: true},
	{name: "s3fake/firstbyte=100ms", open: func(b *testing.B) objstore.Backend {
		return readS3Fake(b, 100*time.Millisecond)
	}},
	{name: "s3fake/firstbyte=200ms", open: func(b *testing.B) objstore.Backend {
		return readS3Fake(b, 200*time.Millisecond)
	}},
	{name: "real", open: s3fake.RealBackend},
}

// readConnBytes is the most a second the S3 fake sends one connection: 80
// MB/s, the low end of what one S3 connection carries, as
// internal/gitlet/host's benchmarks take it.
const readConnBytes = 80_000_000

// readS3Fake is the S3 fake behind the production backend, answering each
// request firstByte after it arrives (on loopback it answers in microseconds,
// which would price the SDK, not the bucket) and sending each connection
// readConnBytes a second.
func readS3Fake(b *testing.B, firstByte time.Duration) objstore.Backend {
	b.Helper()
	srv := s3fake.Start(b, "files")
	srv.Latency, srv.ConnBytesPerSecond = firstByte, readConnBytes
	backend, err := s3obj.Open(b.Context(), s3obj.Config{
		Endpoint: srv.URL, Region: "us-east-1", Bucket: "files", Prefix: "acme/",
		PathStyle: true, AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: "example-secret",
	})
	if err != nil {
		b.Fatalf("opening the S3 fake: %v", err)
	}
	return backend
}

// readSize is one history= arm: a gittest.History of about history bytes
// under a wide tree of files files.
type readSize struct {
	name    string
	history int64
	files   int
	// large arms run only under gittest.LargeEnv.
	large bool
}

// readSizes are the history= arms, their trees as wide as
// internal/gitlet/host's tree fixture's: its 10 000 files, and the 100 000 of
// its large arm.
var readSizes = []readSize{
	{name: "32MB", history: 32 << 20, files: 10_000},
	{name: "1GB", history: 1 << 30, files: 100_000, large: true},
}

const (
	// bigLines is the lines of the file a page-50 read pages through: about
	// 3 MB, so its fiftieth 56 KiB page is near its end.
	bigLines = 60_000
	// wideDirFiles is how many files a directory of the wide tree holds: a
	// hundred, as internal/gitlet/host's tree fixture's directories do.
	wideDirFiles = 100
	// wideLines is how many lines a file of the wide tree holds: forty, about
	// two kilobytes, a source file's size.
	wideLines = 40
	// wideEdits is how many files the wide edit changes, spread over every
	// directory: a formatter run over the whole 10 000-file tree, and the same
	// count in the 100 000-file one, so both arms' whole stats price one diff
	// of that width.
	wideEdits = 10_000
	// readIdent is every commit the rig makes on top of its history: the
	// committer gittest.History's own commits name.
	readIdent = "Crewlet Bench <bench@example.com>"
)

// widePath is the path of the wide tree's f-th file.
func widePath(f int) string { return fmt.Sprintf("%s/f%06d.txt", wideDir(f/wideDirFiles), f) }

// wideDir is the path of the wide tree's d-th directory.
func wideDir(d int) string { return fmt.Sprintf("src/w%04d", d) }

// wideFile is the wide tree's f-th file, before the edit or after it: the
// edit rewrites its middle line.
func wideFile(f int, edited bool) string {
	var b strings.Builder
	for i := range wideLines {
		if edited && i == wideLines/2 {
			fmt.Fprintf(&b, "%06d %02d the quick brown fox was edited\n", f, i)
			continue
		}
		fmt.Fprintf(&b, "%06d %02d the quick brown fox jumps over the lazy dog\n", f, i)
	}
	return b.String()
}

// readSource is the repository every rig of one history= arm pushes: a
// gittest.History, and the three commits on top of it.
type readSource struct {
	dir string
	// commits is the history's, oldest first.
	commits []string
	// big, wide and edit are the commits on top of it: big.txt, the wide
	// tree, and the edit across it — edit the repository's tip.
	big, wide, edit string
	files           int
}

// newReadSource fast-imports size's history and the three commits on top of
// it into a repository the engine's way.
func newReadSource(b *testing.B, size readSize) *readSource {
	b.Helper()
	c := gittest.NewClient(b)
	src, commits := gittest.History(b, c, size.history)
	gittest.FastImport(b, c, src, readTops(commits, size.files))
	tops := strings.Fields(c.Run(b, src, "rev-list", "--reverse", commits[len(commits)-1]+"..main"))
	if len(tops) != 3 {
		b.Fatalf("the history took %d commits on top, want big.txt, the wide tree and its edit", len(tops))
	}
	return &readSource{dir: src, commits: commits, big: tops[0], wide: tops[1], edit: tops[2], files: size.files}
}

// readTops is the fast-import stream of the three commits on top of a
// History of commits: big.txt, the wide tree of files files, and the edit of
// wideEdits of them, each dated a second after the one before it, so no walk
// by date reads the history as skewed (gittest.After).
func readTops(commits []string, files int) func(*bufio.Writer) error {
	return func(w *bufio.Writer) error {
		commit := func(n int, msg string) {
			fmt.Fprintf(w, "commit refs/heads/main\ncommitter %s %s\ndata %d\n%s", readIdent,
				strings.TrimPrefix(gittest.After(commits, n), "@"), len(msg), msg)
			if n == 0 {
				// A session's first commit names where main is: fast-import
				// starts every session with no branch of its own.
				_, _ = w.WriteString("from refs/heads/main^0\n")
			}
		}
		file := func(path, content string) {
			fmt.Fprintf(w, "M 100644 inline %s\ndata %d\n%s\n", path, len(content), content)
		}
		commit(0, "big\n")
		var big strings.Builder
		for i := range bigLines {
			fmt.Fprintf(&big, "%06d the quick brown fox jumps over the lazy dog\n", i)
		}
		file("big.txt", big.String())
		_ = w.WriteByte('\n')
		commit(1, "a wide tree\n")
		for f := range files {
			file(widePath(f), wideFile(f, false))
		}
		_ = w.WriteByte('\n')
		commit(2, "an edit across the wide tree\n")
		for f := 0; f < files; f += max(1, files/wideEdits) {
			file(widePath(f), wideFile(f, true))
		}
		// bufio's error is sticky: the last write reports any before it.
		return w.WriteByte('\n')
	}
}

// readRig is BenchmarkCodeRead's fixture: a fleet of two Gitlet nodes holding a
// source's repository, a query server over the first node, and one over the
// same node with its memo hidden (unkept); p50 is each warm and miss arm's
// p50, by its kind and arm, which the memo arm weighs its own against.
type readRig struct {
	f      *gitlettest.SoloFleet
	src    *readSource
	repo   string
	s      *query.Server
	unkept *query.Server
	p50    map[string]time.Duration
}

// readCacheBytes is every rig's Gitlet node's cache budget, as in
// internal/gitlet/host's benchmarks: the largest arm's repository twice over,
// so no figure is of a repository evicted between iterations.
const readCacheBytes = 4 << 30

// newReadRig is a fleet over objects holding src's repository, pushed as a
// host takes one: the history a session at a time, then each commit on top of
// it.
func newReadRig(b *testing.B, src *readSource, objects objstore.Backend) *readRig {
	b.Helper()
	f := gitlettest.SoloNode(b, gitlettest.SoloOptions{Shape: gitlettest.Shape{GitletNodes: 2}, Objects: objects,
		People: []string{"ceo"}, CacheMaxBytes: readCacheBytes, SyncAlways: true})
	f.Declare(b, gitlet.Declared{Name: gitlettest.RepoName, Settings: gitlettest.Settings()})
	p := gitlettest.NewPerson(b, f, "ceo")
	url := f.Nodes()[0].URL() + "/git/" + gitlettest.RepoName
	pushes := make([]string, 0, gittest.HistorySessions+3)
	for s := 1; s <= gittest.HistorySessions; s++ {
		pushes = append(pushes, src.commits[s*len(src.commits)/gittest.HistorySessions-1])
	}
	for _, commit := range append(pushes, src.big, src.wide, src.edit) {
		if res := p.Push(b.Context(), src.dir, url, commit+":refs/heads/main"); res.Err != nil {
			b.Fatalf("git push: %s", res)
		}
	}
	rows, err := f.Rows(b.Context(), gitlettest.RepoName, gitlettest.Linearizable)
	if err != nil {
		b.Fatal(err)
	}
	n := f.Nodes()[0]
	server := func(c query.Cache) *query.Server {
		s, err := query.New(query.Options{NodeID: n.ID(), Cache: c, Git: n.Pool(),
			Share: gitlet.NewGitShare(n.ID(), n.Pool().Slots(gitproc.ClassRead), nil), Resyncs: gitlet.NewResyncs(nil, nil),
			Roster: func() gitlet.Roster { return nil }, Fetched: noFetched})
		if err != nil {
			b.Fatal(err)
		}
		return s
	}
	b.Logf("the pools run git %s; %d commits in %d packs, a tree of %d files", n.Pool().Version(),
		len(src.commits)+3, len(rows.Packs), src.files)
	return &readRig{f: f, src: src, repo: rows.Repo.ID, s: server(n.Cache()), unkept: server(memoless{n.Cache()}),
		p50: map[string]time.Duration{}}
}

// kind is one warm arm's read.
type kind struct {
	name string
	q    gitlet.GitQuery
}

// kinds is every warm arm's read, its cursors read off the repository first.
func (r *readRig) kinds(b *testing.B) []kind {
	b.Helper()
	ctx := b.Context()
	page := func(q gitlet.GitQuery, n int) gitlet.GitQuery {
		for range n - 1 {
			a, err := r.s.Read(ctx, q)
			if err != nil || a.Next == "" {
				b.Fatalf("paging to page %d: %v (next %q)", n, err, a.Next)
			}
			q.Cursor = a.Next
		}
		return q
	}
	tip, commits := r.src.edit, r.src.commits
	whole := treeQ(r.repo, gitlet.TreeQuery{Rev: tip, Recursive: true})
	listing, err := r.s.Read(ctx, whole)
	if err != nil || listing.Next == "" {
		b.Fatalf("the whole tree's first page: %v (next %q), want a listing of pages", err, listing.Next)
	}
	// A CURSOR IS A POSITION IN THE LISTING (e:<entry>), the same on every
	// node: a page nine tenths of the way through it is asked as one.
	late := whole
	late.Cursor = fmt.Sprintf("e:%d", listing.Tree.Total*9/10)
	session := commits[len(commits)-len(commits)/gittest.HistorySessions-1]
	last := commits[len(commits)-1]
	return []kind{
		{"tree-root", treeQ(r.repo, gitlet.TreeQuery{Rev: tip})},
		{"tree-recursive", whole},
		{"tree-recursive-late", late},
		{"blob-first-page", blobQ(r.repo, gitlet.BlobQuery{Rev: tip, Path: "big.txt"})},
		{"blob-page-50", page(blobQ(r.repo, gitlet.BlobQuery{Rev: tip, Path: "big.txt"}), 50)},
		{"diff-stat", diffQ(r.repo, gitlet.DiffQuery{Base: session, Head: last})},
		{"diff-stat-wide", diffQ(r.repo, gitlet.DiffQuery{Base: r.src.wide, Head: tip})},
		{"diff-stat-wide-dir", diffQ(r.repo, gitlet.DiffQuery{Base: r.src.wide, Head: tip,
			Paths: []string{wideDir(0)}})},
		{"diff-one-file", diffQ(r.repo, gitlet.DiffQuery{Base: r.src.wide, Head: tip,
			File: widePath(r.src.files / 2)})},
		{"compare", compareQ(r.repo, commits[len(commits)/2], last)},
	}
}

// warm times k's read over the first node with its memo hidden, the read a
// memo hit is weighed against, reporting its percentiles.
func (r *readRig) warm(b *testing.B, k kind) {
	took := make([]time.Duration, 0, b.N)
	for b.Loop() {
		start := time.Now()
		if _, err := r.unkept.Read(b.Context(), k.q); err != nil {
			b.Fatal(err)
		}
		took = append(took, time.Since(start)) //fixedwait:measure a benchmark reports this; no gate reads it
	}
	r.p50[k.name+"/warm"] = report(b, took)
}

// miss times k's read over the first node with its memo, every read one the
// memo holds no page of: a server whose page bound is a byte narrower than the
// last one's, which keys its pages apart.
func (r *readRig) miss(b *testing.B, k kind) {
	took := make([]time.Duration, 0, b.N)
	bound := gitlet.MaxGitAnswerBytes
	for b.Loop() {
		b.StopTimer()
		bound--
		s := r.server()
		query.SetPageBytes(s, bound)
		b.StartTimer()
		start := time.Now()
		a, err := s.Read(b.Context(), k.q)
		took = append(took, time.Since(start)) //fixedwait:measure a benchmark reports this; no gate reads it
		switch {
		case err != nil:
			b.Fatal(err)
		case a.Memo:
			b.Fatal("a read at a bound no read used before was answered from the memo")
		}
	}
	r.p50[k.name+"/miss"] = report(b, took)
}

// memo times k's read over the first node, its page in the memo: every read
// a hit.
func (r *readRig) memo(b *testing.B, k kind) {
	if _, err := r.s.Read(b.Context(), k.q); err != nil {
		b.Fatal(err)
	}
	took := make([]time.Duration, 0, b.N)
	for b.Loop() {
		start := time.Now()
		a, err := r.s.Read(b.Context(), k.q)
		took = append(took, time.Since(start)) //fixedwait:measure a benchmark reports this; no gate reads it
		switch {
		case err != nil:
			b.Fatal(err)
		case !a.Memo:
			b.Fatal("a read of a page the memo holds was cut again")
		}
	}
	hit := report(b, took)
	for _, arm := range []string{"warm", "miss"} {
		if was := r.p50[k.name+"/"+arm]; was > 0 && hit > 0 {
			b.ReportMetric(float64(was)/float64(hit), "x-"+arm)
		}
	}
}

// readFloor is one fresh= arm: the floor a read is sent with, given the
// answer of the linearizable resolve of main made just before it.
type readFloor struct {
	name string
	of   func(gitlet.RefAnswer) statelog.Freshness
}

// readFloors are the fresh= arms, which decide no constant: no floor; the
// ref's move (gitlet.ResolvedRef.Moved), which must read as none does; and
// where the resolve read the ref, which prices a delta round of the copy on
// every read, at its least: this fleet's data node is one call away.
var readFloors = []readFloor{
	{"none", func(gitlet.RefAnswer) statelog.Freshness { return statelog.Freshness{} }},
	{"moved", func(a gitlet.RefAnswer) statelog.Freshness {
		return statelog.FloorAt(statelog.ReadSession, a.Refs[0].Moved)
	}},
	{"read", func(a gitlet.RefAnswer) statelog.Freshness {
		return statelog.FloorAt(statelog.ReadSession, a.Position)
	}},
}

// floored times the root's listing over the first node, its page in the memo,
// each read floored by f off a linearizable resolve of main made untimed just
// before it, as each code tool call resolves its ref anew, so a floor at the
// resolve's checkpoint is past the copy on every read.
func (r *readRig) floored(b *testing.B, f readFloor) {
	q := treeQ(r.repo, gitlet.TreeQuery{Rev: r.src.edit})
	if _, err := r.s.Read(b.Context(), q); err != nil {
		b.Fatal(err)
	}
	took := make([]time.Duration, 0, b.N)
	for b.Loop() {
		b.StopTimer()
		ref, err := r.f.Data().Served().ResolveRef(b.Context(), gitlet.RefQuery{Repo: gitlettest.RepoName,
			Refs: []string{"refs/heads/main"}, Fresh: gitlettest.Linearizable, Asker: person})
		if err == nil {
			err = ref.Err()
		}
		if err != nil {
			b.Fatal(err)
		}
		q.Fresh = f.of(ref)
		b.StartTimer()
		start := time.Now()
		a, err := r.s.Read(b.Context(), q)
		took = append(took, time.Since(start)) //fixedwait:measure a benchmark reports this; no gate reads it
		switch {
		case err != nil:
			b.Fatal(err)
		case !a.Memo:
			b.Fatal("a read of a page the memo holds was cut again")
		}
	}
	report(b, took)
}

// server is a query server over the first node's cache, its memo kept.
func (r *readRig) server() *query.Server {
	n := r.f.Nodes()[0]
	s, err := query.New(query.Options{NodeID: n.ID(), Cache: n.Cache(), Git: n.Pool(),
		Share: gitlet.NewGitShare(n.ID(), n.Pool().Slots(gitproc.ClassRead), nil), Resyncs: gitlet.NewResyncs(nil, nil),
		Roster: func() gitlet.Roster { return nil }, Fetched: noFetched})
	if err != nil {
		panic(err) // the options are the rig's own, valid once
	}
	return s
}

// cold times the root's listing on the second node, emptied before every
// iteration, with the production sync wait.
func (r *readRig) cold(b *testing.B) {
	var took []time.Duration
	unreadies := 0
	g := r.f.GitletNode(1)
	for range b.N {
		b.StopTimer()
		dir := g.Node().CacheDir()
		g.Stop(b.Context())
		if err := os.RemoveAll(dir); err != nil {
			b.Fatal(err)
		}
		g.Restart(b)
		n := g.Node()
		s, err := query.New(query.Options{NodeID: n.ID(), Cache: n.Cache(), Git: n.Pool(),
			Share: gitlet.NewGitShare(n.ID(), n.Pool().Slots(gitproc.ClassRead), nil), Resyncs: gitlet.NewResyncs(nil, nil),
			Roster: func() gitlet.Roster { return nil }, Fetched: noFetched})
		if err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		start := time.Now()
		_, err = s.Read(b.Context(), treeQ(r.repo, gitlet.TreeQuery{Rev: r.src.edit}))
		// REPORTED, never failed on: a cold read answering within
		// statelog.ReadBudget and its margin is
		// TestAColdRepositoryAnswersUnreadyAndKeepsSyncing's claim.
		took = append(took, time.Since(start)) //fixedwait:measure a benchmark reports this; no gate reads it
		switch {
		case unready(err, gitlet.UnreadySyncing):
			unreadies++
		case err != nil:
			b.Fatal(err)
		}
	}
	report(b, took)
	b.ReportMetric(float64(unreadies)/float64(len(took)), "unready/op")
}

// report reports took's p50, p95 and p99 in milliseconds, and answers the
// p50.
func report(b *testing.B, took []time.Duration) time.Duration {
	if len(took) == 0 {
		return 0
	}
	slices.Sort(took)
	at := func(q float64) time.Duration { return took[min(len(took)-1, int(q*float64(len(took))))] }
	ms := func(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
	b.ReportMetric(ms(at(0.50)), "p50-ms")
	b.ReportMetric(ms(at(0.95)), "p95-ms")
	b.ReportMetric(ms(at(0.99)), "p99-ms")
	return at(0.50)
}
