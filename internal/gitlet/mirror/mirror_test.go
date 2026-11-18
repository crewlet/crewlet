package mirror_test

import (
	"context"
	"database/sql"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitletfault"
	"github.com/crewlet/crewlet/internal/gitlet/gitlettest"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/gitlet/mirror"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// source is a repository at another host: a bare repository served over smart
// HTTP (git's own http-backend, as a forge serves one) behind TLS, and a work
// tree its history is made in. The process's SSL_CERT_FILE names the server's
// authority, as a Gitlet node's git trusts a source (gitproc.ResolveRemote), so
// no case using one runs in parallel.
type source struct {
	t    *testing.T
	c    *gittest.Client
	wt   string
	bare string
	url  string
	// reads counts advertisement requests (`GET
	// info/refs?service=git-upload-pack`): exactly one per fetch or ls-remote.
	reads *atomic.Int64
	// served counts the bytes upload-pack answered past its advertisement:
	// under protocol v2, each ls-refs listing and each fetch.
	served *atomic.Int64
}

// counted is a response every byte of which is added to n.
type counted struct {
	http.ResponseWriter
	n *atomic.Int64
}

func (c counted) Write(p []byte) (int, error) {
	c.n.Add(int64(len(p)))
	return c.ResponseWriter.Write(p)
}

func newSource(t *testing.T) *source {
	t.Helper()
	c := gittest.NewClient(t)
	root := t.TempDir()
	bare := filepath.Join(root, "src.git")
	c.Run(t, "", "init", "-q", "--bare", bare)
	reads, served := &atomic.Int64{}, &atomic.Int64{}
	backend := c.HTTPBackend(root)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/info/refs") &&
			r.URL.Query().Get("service") == "git-upload-pack" {
			reads.Add(1)
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git-upload-pack") {
			w = counted{ResponseWriter: w, n: served}
		}
		backend.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	block := &pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}
	if err := os.WriteFile(ca, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSL_CERT_FILE", ca)
	return &source{t: t, c: c, wt: c.Init(t), bare: bare, url: srv.URL + "/src.git", reads: reads,
		served: served}
}

// commit commits files in the source's work tree.
func (s *source) commit(files map[string]string, message string) string {
	s.t.Helper()
	return s.c.Commit(s.t, s.wt, files, message)
}

// push pushes the work tree to the served repository.
func (s *source) push(args ...string) {
	s.t.Helper()
	s.c.Run(s.t, s.wt, append([]string{"push", "-q", s.bare}, args...)...)
}

// recording is the code log as an attempt reaches it, every proposal kept.
type recording struct {
	mirror.Estate
	mu      sync.Mutex
	imports []gitlet.ImportProposal
	syncs   []gitlet.MirrorSyncProposal
}

func (r *recording) Import(ctx context.Context, p gitlet.ImportProposal) (gitlet.SyncResult, error) {
	r.mu.Lock()
	r.imports = append(r.imports, p)
	r.mu.Unlock()
	return r.Estate.Import(ctx, p)
}

func (r *recording) MirrorSync(ctx context.Context, p gitlet.MirrorSyncProposal) (gitlet.SyncResult, error) {
	r.mu.Lock()
	r.syncs = append(r.syncs, p)
	r.mu.Unlock()
	return r.Estate.MirrorSync(ctx, p)
}

// mirrorSyncs is how many syncs were proposed so far.
func (r *recording) mirrorSyncs() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.syncs)
}

// chunks and dones are the import's proposals so far, each kind apart.
func (r *recording) chunks() (chunks []gitlet.ImportChunk, dones []gitlet.ImportDone) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.imports {
		if p.Chunk != nil {
			chunks = append(chunks, *p.Chunk)
		}
		if p.Done != nil {
			dones = append(dones, *p.Done)
		}
	}
	return chunks, dones
}

// announced is a node's event stream, keeping every push's and import's
// account; an event published on a subject other than its type's fails the
// case.
type announced struct {
	t        *testing.T
	mu       sync.Mutex
	pushed   []types.CodeMirrorPushed
	imported []types.CodeImported
}

func (a *announced) Publish(_ context.Context, subject string, ev *events.Event) error {
	if want := topics.Event(ev.Type); subject != want {
		a.t.Errorf("%s was published on %q, want %q", ev.Type, subject, want)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	switch p := ev.Data.(type) {
	case *types.CodeMirrorPushed:
		a.pushed = append(a.pushed, *p)
	case *types.CodeImported:
		a.imported = append(a.imported, *p)
	}
	return nil
}

// imports is every import's account so far.
func (a *announced) imports() []types.CodeImported {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.imported)
}

// all is every push's account so far.
func (a *announced) all() []types.CodeMirrorPushed {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.pushed)
}

// harness is one data node and its Gitlet node, a source at another host, and
// the clock its workers read.
type harness struct {
	t      *testing.T
	f      *gitlettest.SoloFleet
	src    *source
	est    *recording
	faults *gitletfault.Script
	limits gitlet.Limits
	events announced
	// dial is the workers' dialer: nil dials the source as Tier A's
	// gitlet.import_allow_private lets a node dial loopback.
	dial mirror.Dial
	// git is the git pool the workers run in: nil for the Gitlet node's own.
	git mirror.Pool
	// people is the identity directory a push mirror's seat is looked up in:
	// nil for gitlettest.FixturePeople.
	people gitlet.People
	// ended is the workers' mirror.Options.Ended: nil for none.
	ended func(repoID string, out bool, err error)

	mu  sync.Mutex
	now time.Time
}

func newHarness(t *testing.T, limits gitlet.Limits) *harness {
	t.Helper()
	return newHarnessWith(t, limits, nil)
}

// newHarnessWith is newHarness with edit applied to the fleet's options, handed
// the harness (its clock included) before the fleet is built.
func newHarnessWith(t *testing.T, limits gitlet.Limits, edit func(*harness, *gitlettest.SoloOptions)) *harness {
	t.Helper()
	h := &harness{t: t, src: newSource(t), faults: &gitletfault.Script{}, events: announced{t: t}, now: time.Now()}
	opts := gitlettest.SoloOptions{Shape: gitlettest.Shape{Limits: limits}, Faults: h.faults}
	if edit != nil {
		edit(h, &opts)
	}
	h.f = gitlettest.SoloNode(t, opts)
	h.est = &recording{Estate: h.f.Code(t)}
	h.limits = gitlet.DefaultLimits()
	if limits.MaxBlobBytes != 0 {
		h.limits.MaxBlobBytes = limits.MaxBlobBytes
	}
	return h
}

// stampedApplier stamps every record a data node applies with the case's clock
// in place of the broker's instant: the instant its rows keep and its gates
// count a rejection at.
type stampedApplier struct {
	statelog.Applier
	clock func() time.Time
}

func (a stampedApplier) Gated(ctx context.Context, tx *sql.Tx, rec statelog.Record) (statelog.Reason, bool,
	error) {
	rec.StoredAt = a.clock()
	return a.Applier.Gated(ctx, tx, rec)
}

func (a stampedApplier) Apply(ctx context.Context, tx *sql.Tx, rec statelog.Record,
	opts statelog.ApplyOptions) (int, error) {
	at := a.clock()
	rec.StoredAt, opts.StoredAt = at, at
	return a.Applier.Apply(ctx, tx, rec, opts)
}

// clock is the workers' clock; advance moves it.
func (h *harness) clock() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.now
}

func (h *harness) advance(d time.Duration) {
	h.mu.Lock()
	h.now = h.now.Add(d)
	h.mu.Unlock()
}

// mirrors is the Gitlet node's worker as the engine builds it, over the node's
// cache and pool, the fleet's object store, the fixture chart and the harness's
// stream, dialing loopback as Tier A's gitlet.import_allow_private allows
// unless the case's dial replaces it. Each call is a fresh worker, knowing only
// the rows.
func (h *harness) mirrors() *mirror.Mirrors {
	h.t.Helper()
	n := h.f.Nodes()[0]
	objects, err := objstore.NewStore(h.f.Objects())
	if err != nil {
		h.t.Fatal(err)
	}
	dial := h.dial
	if dial == nil {
		dial = func(ctx context.Context, url, _ string) (gitproc.Remote, error) {
			return gitproc.ResolveRemote(ctx, url, true)
		}
	}
	var git mirror.Pool = n.Pool()
	if h.git != nil {
		git = h.git
	}
	var people gitlet.People = gitlettest.FixturePeople{}
	if h.people != nil {
		people = h.people
	}
	m, err := mirror.New(mirror.Options{Estate: h.est, Cache: n.Cache(), Git: git, Objects: objects,
		Dial: dial, NodeID: n.ID(), Limits: func() gitlet.Limits { return h.limits },
		Roster: gitlettest.FixtureRoster, People: people, Events: &h.events, Faults: h.faults, Now: h.clock,
		Ended: h.ended})
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { m.Stop(context.Background()) })
	return m
}

// pass is one backstop turn over m: every page of its listing, each repository
// worked, every attempt started waited for. It answers the repositories listed.
func (h *harness) pass(m *mirror.Mirrors) []string {
	h.t.Helper()
	listed := h.listAndWork(m)
	m.Wait()
	return listed
}

// listAndWork is a pass's listing and its turns, each repository the listing
// names worked, without waiting for the attempts they started.
func (h *harness) listAndWork(m *mirror.Mirrors) []string {
	h.t.Helper()
	ctx := h.t.Context()
	var listed []string
	for cursor := ""; ; {
		repos, next, err := m.Due(ctx, statelog.Position{}, cursor)
		if err != nil {
			h.t.Fatalf("the listing at %q: %v", cursor, err)
		}
		for _, repo := range repos {
			if err := m.Work(ctx, repo, statelog.Position{}); err != nil {
				h.t.Fatalf("work %s: %v", repo, err)
			}
		}
		listed = append(listed, repos...)
		if next == "" {
			break
		}
		cursor = next
	}
	return listed
}

// rows is the repository named name as the log's head holds it.
func (h *harness) rows(name string) gitlettest.Rows {
	h.t.Helper()
	rows, err := h.f.Rows(h.t.Context(), name, gitlettest.Linearizable)
	if err != nil {
		h.t.Fatalf("the rows of %s: %v", name, err)
	}
	return rows
}

// AN IMPORT RESUMES FROM ITS ROWS: an import of more refs than one chunk lands
// chunk 1 and dies; a fresh worker, knowing only the rows, lands chunk 2 at the
// next Seq and ends the import, never landing chunk 1 again. Its fetch asks
// only for what the imported refs do not reach: the source's new commit edits
// one line, so its blob comes as a delta in a THIN pack the fetch completes
// with the old blob from the copy, and the cut stores nothing chunk 1's pack
// holds, that appended base included.
//
// Mutation: start every attempt at Seq 1, and chunk 2 is refused stale; drop
// the cut's exclusion of the copy's live packs, and chunk 2 stores the appended
// base again.
func TestAnImportResumesFromItsRows(t *testing.T) {
	h := newHarness(t, gitlet.Limits{})
	var file strings.Builder
	for i := range 200 {
		fmt.Fprintf(&file, "line %03d of a file long enough that an edit of one line is sent as a delta\n", i)
	}
	first := h.src.commit(map[string]string{"a": file.String()}, "first")
	h.src.push("HEAD:refs/heads/main")
	// One chunk's refs and one more, so the import takes two chunks.
	var tags strings.Builder
	for i := range gitlet.MaxImportRefs + 1 {
		fmt.Fprintf(&tags, "create refs/tags/v%04d %s\n", i, first)
	}
	if _, stderr, err := h.src.c.Try(t, "", strings.NewReader(tags.String()), "--git-dir="+h.src.bare,
		"update-ref", "--stdin"); err != nil {
		t.Fatalf("tagging the source: %v\n%s", err, stderr)
	}
	h.f.Declare(t, gitlet.Declared{Name: "imported", Settings: gitlettest.Settings(),
		Import: &gitlet.ImportSource{URL: h.src.url}})

	h.faults.Arm(gitletfault.ImportAfterPropose, gitletfault.Action{Once: true, Err: errors.New("the home died")})
	h.pass(h.mirrors())
	rows := h.rows("imported")
	if rows.Repo.Status != gitlet.RepoStatusImporting || len(rows.Refs) != gitlet.MaxImportRefs {
		t.Fatalf("after the first attempt the repository is %s with %d refs; want it importing with its first "+
			"chunk's %d", rows.Repo.Status, len(rows.Refs), gitlet.MaxImportRefs)
	}

	second := h.src.commit(map[string]string{"a": strings.Replace(file.String(), "line 100 ", "line one hundred ",
		1)}, "second")
	h.src.push("HEAD:refs/heads/main")
	h.pass(h.mirrors())

	rows = h.rows("imported")
	if rows.Repo.Status != gitlet.RepoStatusActive {
		t.Fatalf("after the second attempt the repository is %s, want active", rows.Repo.Status)
	}
	if got := rows.Refs["refs/heads/main"]; got != second {
		t.Errorf("main is %s, want the source's %s", got, second)
	}
	if len(rows.Refs) != gitlet.MaxImportRefs+2 {
		t.Errorf("the repository holds %d refs, want the source's %d", len(rows.Refs), gitlet.MaxImportRefs+2)
	}
	chunks, dones := h.est.chunks()
	if len(chunks) != 2 || chunks[0].Seq != 1 || chunks[1].Seq != 2 {
		t.Fatalf("the import proposed %d chunks %v; want chunk 1, then chunk 2 from the rows", len(chunks),
			seqs(chunks))
	}
	if len(dones) != 1 || dones[0].Seq != 2 || dones[0].Error != "" {
		t.Fatalf("the import ended %+v; want one end after chunk 2, with no error", dones)
	}
	landed := map[string]bool{}
	for _, c := range chunks[0].Commands {
		landed[c.Ref] = true
	}
	for _, c := range chunks[1].Commands {
		if landed[c.Ref] && c.Ref != "refs/heads/main" {
			t.Errorf("the second chunk sets %s again, which the first landed", c.Ref)
		}
	}
	var objects int64
	for _, p := range chunks[1].Packs {
		objects += p.Objects
	}
	if objects != 3 {
		t.Errorf("the second chunk stores %d objects; want the second commit's three (commit, tree, blob)", objects)
	}
}

// seqs is the chunks' Seqs.
func seqs(chunks []gitlet.ImportChunk) []int64 {
	out := make([]int64, len(chunks))
	for i, c := range chunks {
		out[i] = c.Seq
	}
	return out
}

// A PULL MIRROR FOLLOWS A FORCE-PUSH: an imported pull mirror is not synced
// again within gitlet.MirrorInterval; after it, its sync moves the mirror onto
// its source as it is (main onto a rewritten history, a deleted branch
// deleted), the move off the old tip marked forced so the committer records the
// commit it left.
//
// Mutation: never mark a command forced, and main's move is not forced; skip
// deleting refs the source dropped, and feature stays; list a mirror whatever
// its last attempt, and it is synced within its interval.
func TestAPullMirrorFollowsAForcePush(t *testing.T) {
	h := newHarness(t, gitlet.Limits{})
	a := h.src.commit(map[string]string{"a": "a\n"}, "first")
	b := h.src.commit(map[string]string{"b": "b\n"}, "second")
	h.src.push("HEAD:refs/heads/main", a+":refs/heads/feature")
	s := gitlettest.Settings()
	s.Protections = nil
	s.MirrorFrom = &gitlet.MirrorSource{URL: h.src.url}
	h.f.Declare(t, gitlet.Declared{Name: "mirrored", Settings: s})

	m := h.mirrors()
	h.pass(m)
	rows := h.rows("mirrored")
	if rows.Repo.Status != gitlet.RepoStatusActive || !rows.Repo.Mirrored || rows.Refs["refs/heads/main"] != b ||
		rows.Refs["refs/heads/feature"] != a {
		t.Fatalf("after its import the mirror is %s (mirrored %t) with refs %v; want it active, main at %s and "+
			"feature at %s", rows.Repo.Status, rows.Repo.Mirrored, rows.Refs, b, a)
	}
	if listed := h.pass(m); len(listed) != 0 {
		t.Fatalf("the mirror was listed %v within its interval of its import", listed)
	}

	h.src.c.Run(t, h.src.wt, "reset", "-q", "--hard", a)
	c := h.src.commit(map[string]string{"c": "c\n"}, "rewritten")
	h.src.push("--force", "HEAD:refs/heads/main", ":refs/heads/feature")
	h.advance(gitlet.MirrorInterval + time.Minute)
	if listed := h.pass(m); !slices.Contains(listed, rows.Repo.ID) {
		t.Fatalf("the mirror was not listed %v once its interval had passed", listed)
	}

	rows = h.rows("mirrored")
	if got := rows.Refs["refs/heads/main"]; got != c {
		t.Errorf("main is %s, want the source's rewritten %s", got, c)
	}
	if got, ok := rows.Refs["refs/heads/feature"]; ok {
		t.Errorf("feature is still at %s, which the source deleted", got)
	}
	h.est.mu.Lock()
	syncs := slices.Clone(h.est.syncs)
	h.est.mu.Unlock()
	if len(syncs) != 1 {
		t.Fatalf("the mirror proposed %d syncs, want one", len(syncs))
	}
	moves := map[string]gitlet.Command{}
	for _, cmd := range syncs[0].Sync.Commands {
		moves[cmd.Ref] = cmd
	}
	if got := moves["refs/heads/main"]; got.Old != b || got.New != c || !got.Forced {
		t.Errorf("the sync moved main %+v; want %s to %s, forced", got, b, c)
	}
	if got := moves["refs/heads/feature"]; got.Old != a || got.New != gitlet.ZeroOID {
		t.Errorf("the sync moved feature %+v; want it deleted from %s", got, a)
	}
}

// A REPOSITORY MADE A PULL MIRROR LATER IS IMPORTED FIRST (M6-D16): an active
// repository holding a person's own branch, then declared a pull mirror of a
// source with more refs than one chunk, is brought onto the source by an IMPORT
// (chunk 1, chunk 2, its end), never a sync, whose fetch has one push's
// deadline; it ends an active pull mirror of exactly the source's refs.
//
// Mutation: drop the settings apply's start of the import (mirrorImportSQL),
// and its home syncs it, no chunk proposed.
func TestARepositoryMadeAPullMirrorIsImportedFirst(t *testing.T) {
	h := newHarness(t, gitlet.Limits{})
	a := h.src.commit(map[string]string{"a": "a\n"}, "first")
	h.src.push("HEAD:refs/heads/main")
	var tags strings.Builder
	for i := range gitlet.MaxImportRefs {
		fmt.Fprintf(&tags, "create refs/tags/v%04d %s\n", i, a)
	}
	if _, stderr, err := h.src.c.Try(t, "", strings.NewReader(tags.String()), "--git-dir="+h.src.bare,
		"update-ref", "--stdin"); err != nil {
		t.Fatalf("tagging the source: %v\n%s", err, stderr)
	}
	s := gitlettest.Settings()
	s.Protections = nil
	h.f.Declare(t, gitlet.Declared{Name: "later", Settings: s})
	p := gitlettest.NewPerson(t, h.f, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"local": "local\n"}, "local")
	if res := p.Push(t.Context(), work, h.f.Nodes()[0].URL()+"/git/later", "HEAD:refs/heads/local"); res.Err != nil {
		t.Fatalf("push local: %s", res)
	}

	s.MirrorFrom = &gitlet.MirrorSource{URL: h.src.url}
	h.f.Declare(t, gitlet.Declared{Name: "later", Settings: s})
	if rows := h.rows("later"); rows.Repo.Status != gitlet.RepoStatusImporting || !rows.Repo.Mirrored {
		t.Fatalf("later made a pull mirror is %s (mirrored %t), want importing from its source",
			rows.Repo.Status, rows.Repo.Mirrored)
	}
	h.pass(h.mirrors())

	rows := h.rows("later")
	if rows.Repo.Status != gitlet.RepoStatusActive || !rows.Repo.Mirrored {
		t.Fatalf("after its import later is %s (mirrored %t), want an active pull mirror", rows.Repo.Status,
			rows.Repo.Mirrored)
	}
	if got, ok := rows.Refs["refs/heads/local"]; ok {
		t.Errorf("local is still at %s, which the source never had", got)
	}
	if got := rows.Refs["refs/heads/main"]; got != a || len(rows.Refs) != gitlet.MaxImportRefs+1 {
		t.Errorf("later holds %d refs, main at %s; want the source's %d, main at %s", len(rows.Refs), got,
			gitlet.MaxImportRefs+1, a)
	}
	chunks, dones := h.est.chunks()
	h.est.mu.Lock()
	syncs := len(h.est.syncs)
	h.est.mu.Unlock()
	if len(chunks) != 2 || chunks[0].Seq != 1 || chunks[1].Seq != 2 || len(dones) != 1 || dones[0].Seq != 2 ||
		syncs != 0 {
		t.Fatalf("later was brought onto its source by chunks %v, %d ends and %d syncs; want an import of "+
			"chunks 1 and 2 and its end, and no sync", seqs(chunks), len(dones), syncs)
	}
}

// A QUIET PULL MIRROR IS NOT FETCHED AGAIN BY A FRESH HOME: the rows, never a
// worker's memory, hold a quiet mirror back an interval. An import that landed
// whole is its last sync, so a worker knowing nothing of it (a restart, a new
// home) does not list it within the interval; after it, a sync finding the
// source moved nothing lands a sync of nothing, moving the last sync to that
// record's instant. stampedApplier stamps both records at the workers' clock,
// so they are compared with no wall clock between them.
//
// Mutation: leave an import's end off its last sync, and the fresh worker lists
// it at once; land nothing where the source moved nothing, and its last sync
// stays the import's.
func TestAQuietPullMirrorIsNotFetchedAgainByAFreshHome(t *testing.T) {
	h := newHarnessWith(t, gitlet.Limits{}, func(h *harness, o *gitlettest.SoloOptions) {
		// The rows keep the microsecond, so an instant they keep equals the
		// clock's.
		h.now = h.now.Truncate(time.Microsecond)
		o.WrapApplier = func(a statelog.Applier) statelog.Applier { return stampedApplier{Applier: a, clock: h.clock} }
	})
	h.src.commit(map[string]string{"a": "a\n"}, "first")
	h.src.push("HEAD:refs/heads/main")
	s := gitlettest.Settings()
	s.Protections = nil
	s.MirrorFrom = &gitlet.MirrorSource{URL: h.src.url}
	h.f.Declare(t, gitlet.Declared{Name: "quiet", Settings: s})
	h.pass(h.mirrors())
	synced := func() gitlet.MirrorCandidate {
		t.Helper()
		a, err := h.est.Read(t.Context(), gitlet.ReadQuery{Kind: gitlet.ReadMirrorsDue, Fresh: gitlettest.Linearizable})
		if err != nil || len(a.Mirrors) != 1 {
			t.Fatalf("mirrors_due answered %+v, %v; want the one pull mirror", a.Mirrors, err)
		}
		return a.Mirrors[0]
	}
	imported := synced()
	if rows := h.rows("quiet"); rows.Repo.Status != gitlet.RepoStatusActive || !imported.SyncedAt.Equal(h.clock()) {
		t.Fatalf("after its import the mirror is %s, last synced %v; want it active, synced at its import's end, "+
			"%v", rows.Repo.Status, imported.SyncedAt, h.clock())
	}
	if listed := h.pass(h.mirrors()); len(listed) != 0 {
		t.Fatalf("a fresh worker listed %v within the interval of the mirror's import", listed)
	}

	h.advance(gitlet.MirrorInterval + time.Minute)
	if listed := h.pass(h.mirrors()); !slices.Contains(listed, imported.ID) {
		t.Fatalf("the mirror was not listed %v once its interval had passed", listed)
	}
	h.est.mu.Lock()
	syncs := slices.Clone(h.est.syncs)
	h.est.mu.Unlock()
	if len(syncs) != 1 || !syncs[0].Sync.Current() {
		t.Fatalf("the quiet mirror proposed %+v; want one sync of nothing", syncs)
	}
	if got := synced(); !got.SyncedAt.Equal(h.clock()) {
		t.Fatalf("after a sync of nothing the mirror's last sync is %v, want its record's instant %v, past its "+
			"import's %v", got.SyncedAt, h.clock(), imported.SyncedAt)
	}
}

// AN IMPORT PAST THE BLOB LIMIT ENDS WITH ITS ERROR: a blob past
// code.limits.max_blob_bytes is not landed in part and retried for ever: the
// attempt that finds it ends the import, the repository active with nothing
// landed and its import_error naming the blob and the limit.
//
// Mutation: compare the blob's size with >= the limit times two, and the chunk
// is proposed; drop the end's Error, and the import ends clean.
func TestAnImportPastTheBlobLimitEndsWithItsError(t *testing.T) {
	h := newHarness(t, gitlet.Limits{MaxBlobBytes: 1 << 10})
	h.src.commit(map[string]string{"small": "s\n", "big": strings.Repeat("x", 4<<10)}, "too big")
	h.src.push("HEAD:refs/heads/main")
	big := strings.TrimSpace(h.src.c.Run(t, h.src.wt, "rev-parse", "HEAD:big"))
	h.f.Declare(t, gitlet.Declared{Name: "imported", Settings: gitlettest.Settings(),
		Import: &gitlet.ImportSource{URL: h.src.url}})

	h.pass(h.mirrors())

	rows := h.rows("imported")
	if rows.Repo.Status != gitlet.RepoStatusActive || len(rows.Refs) != 0 || len(rows.Packs) != 0 {
		t.Fatalf("the repository is %s with %d refs and %d packs; want it active with nothing landed",
			rows.Repo.Status, len(rows.Refs), len(rows.Packs))
	}
	chunks, dones := h.est.chunks()
	if len(chunks) != 0 {
		t.Errorf("the import proposed %d chunks past its blob limit", len(chunks))
	}
	if len(dones) != 1 || dones[0].Seq != 0 || !strings.Contains(dones[0].Error, big) ||
		!strings.Contains(dones[0].Error, "max_blob_bytes") {
		t.Fatalf("the import ended %+v; want one end at chunk 0 naming blob %s and max_blob_bytes", dones, big)
	}
}

// AN ATTEMPT'S END IS TOLD ONCE THE WORKER HAS RECORDED IT (Options.Ended):
// over, one failure in a row, stamped at the worker's clock.
// Mutation: tell Ended before the record, and the attempt still runs.
func TestAnAttemptsEndIsToldOnceItIsRecorded(t *testing.T) {
	h := newHarness(t, gitlet.Limits{})
	refused := errors.New("the source refuses every connection")
	h.dial = func(context.Context, string, string) (gitproc.Remote, error) { return gitproc.Remote{}, refused }
	type told struct {
		out, running bool
		err          error
		failures     int
		ended        time.Time
	}
	var m *mirror.Mirrors
	ends := make(chan told, 4)
	h.ended = func(repoID string, out bool, err error) {
		running, failures, ended := m.Attempted(repoID, out)
		ends <- told{out: out, running: running, err: err, failures: failures, ended: ended}
	}
	h.f.Declare(t, gitlet.Declared{Name: "imported", Settings: gitlettest.Settings(),
		Import: &gitlet.ImportSource{URL: h.src.url}})
	m = h.mirrors()

	h.pass(m)

	if len(ends) != 1 {
		t.Fatalf("%d attempts were told ended, want the import's one", len(ends))
	}
	got := <-ends
	if got.out || !errors.Is(got.err, refused) {
		t.Errorf("the import's end was told as out %v with %v, want in, with the dial's refusal", got.out, got.err)
	}
	if got.running || got.failures != 1 || !got.ended.Equal(h.clock()) {
		t.Errorf("as its end was told the attempt was running %v, %d failure(s) in a row, ended at %v; "+
			"want it over, one failure, ended at the worker's clock %v", got.running, got.failures, got.ended,
			h.clock())
	}
}

// AN IMPORT IS ANNOUNCED ONCE IT ENDS (M6 §2.11): an import whose end applied
// publishes exactly one code_imported on its type's subject, naming its
// source, its chunk count, the end's position and what the repository then
// holds (refs, live bytes, default branch, from its rows). A pull mirror's first fill is
// such an import, marked mirrored, and its later syncs announce nothing; an
// import its blob limit ended short names its error, having landed nothing.
//
// Mutation: drop announceImported's call from the import's end, and nothing is
// announced; publish the end with no error, and the short import names none;
// announce every applied record of a pull mirror, and its sync is announced
// too.
func TestAnImportIsAnnouncedOnceItEnds(t *testing.T) {
	t.Run("a pull mirror's first fill, and its sync after", func(t *testing.T) {
		h := newHarness(t, gitlet.Limits{})
		a := h.src.commit(map[string]string{"a": "a\n"}, "first")
		h.src.push("HEAD:refs/heads/main", a+":refs/tags/v1")
		s := gitlettest.Settings()
		s.Protections = nil
		s.MirrorFrom = &gitlet.MirrorSource{URL: h.src.url}
		h.f.Declare(t, gitlet.Declared{Name: "mirrored", Settings: s})

		m := h.mirrors()
		h.pass(m)
		rows := h.rows("mirrored")
		if rows.Repo.Status != gitlet.RepoStatusActive || len(rows.Refs) != 2 || rows.Repo.LiveBytes <= 0 {
			t.Fatalf("after its import the mirror is %s with %d refs and %d live bytes; want it active with the "+
				"source's two refs and their bytes", rows.Repo.Status, len(rows.Refs), rows.Repo.LiveBytes)
		}
		got := h.events.imports()
		if len(got) != 1 {
			t.Fatalf("the import announced %d ends %+v; want one", len(got), got)
		}
		at, err := statelog.ParsePosition(got[0].Position)
		if err != nil || at.IsZero() {
			t.Fatalf("the import's end names position %q (%v); want the end's record", got[0].Position, err)
		}
		if after, err := rows.Position.Before(at); err != nil || after {
			t.Errorf("the import's end names %s, past the rows read after it at %s (%v)", at, rows.Position, err)
		}
		got[0].Position = ""
		want := types.CodeImported{RepoID: rows.Repo.ID, RepoName: "mirrored", URL: h.src.url, Mirrored: true,
			Chunks: 1, Refs: 2, Bytes: rows.Repo.LiveBytes, DefaultBranch: "main"}
		if got[0] != want {
			t.Errorf("the import announced\n%+v\nwant\n%+v", got[0], want)
		}

		b := h.src.commit(map[string]string{"b": "b\n"}, "second")
		h.src.push("HEAD:refs/heads/main")
		h.advance(gitlet.MirrorInterval + time.Minute)
		h.pass(m)
		if got := h.est.mirrorSyncs(); got != 1 {
			t.Fatalf("the mirror proposed %d syncs once its interval had passed, want one", got)
		}
		if main := h.rows("mirrored").Refs["refs/heads/main"]; main != b {
			t.Fatalf("the sync left main at %s, want the source's %s", main, b)
		}
		if got := h.events.imports(); len(got) != 1 {
			t.Errorf("the import and the sync after it announced %d imports %+v; want the import's one", len(got),
				got)
		}
	})

	t.Run("an import ended short by its blob limit", func(t *testing.T) {
		h := newHarness(t, gitlet.Limits{MaxBlobBytes: 1 << 10})
		h.src.commit(map[string]string{"small": "s\n", "big": strings.Repeat("x", 4<<10)}, "too big")
		h.src.push("HEAD:refs/heads/main")
		big := strings.TrimSpace(h.src.c.Run(t, h.src.wt, "rev-parse", "HEAD:big"))
		h.f.Declare(t, gitlet.Declared{Name: "imported", Settings: gitlettest.Settings(),
			Import: &gitlet.ImportSource{URL: h.src.url}})

		h.pass(h.mirrors())
		rows := h.rows("imported")
		got := h.events.imports()
		if len(got) != 1 {
			t.Fatalf("the import announced %d ends %+v; want one", len(got), got)
		}
		if !strings.Contains(got[0].Error, big) || !strings.Contains(got[0].Error, "max_blob_bytes") {
			t.Errorf("the import's end names error %q; want blob %s and max_blob_bytes", got[0].Error, big)
		}
		got[0].Error, got[0].Position = "", ""
		want := types.CodeImported{RepoID: rows.Repo.ID, RepoName: "imported", URL: h.src.url,
			DefaultBranch: rows.Repo.DefaultBranch}
		if got[0] != want {
			t.Errorf("the import announced\n%+v\nwant\n%+v, nothing landed", got[0], want)
		}
	})
}

// AN ATTEMPT LANDS ITS PACKS BEFORE ITS REFS: no record carries a ref before
// every pack the attempt cut is in it or an earlier one, none carries more than
// gitlet.MaxImportPacks packs or more refs than its record takes (here
// gitlet.MaxImportRefs), and every ref lands once, in order.
//
// Mutation: put the commands in the first record, and a ref rides ahead of its
// packs; drop the cap on a record's packs, and one record carries them all;
// move past every ref offered rather than those taken, and refs are lost.
func TestAnAttemptLandsItsPacksBeforeItsRefs(t *testing.T) {
	t.Parallel()
	outs := make([]string, 2*gitlet.MaxImportPacks+1)
	for i := range outs {
		outs[i] = fmt.Sprintf("%040x", i)
	}
	cmds := make([]gitlet.Command, gitlet.MaxImportRefs+1)
	for i := range cmds {
		cmds[i] = gitlet.Command{Ref: fmt.Sprintf("refs/tags/v%04d", i), Old: gitlet.ZeroOID, New: outs[0]}
	}
	packs, refs := mirror.Steps(outs, cmds, func(_ []string, offered []gitlet.Command) int {
		return min(len(offered), gitlet.MaxImportRefs)
	})
	var gotOuts []string
	var gotCmds []gitlet.Command
	for i := range packs {
		if len(packs[i]) > gitlet.MaxImportPacks || len(refs[i]) > gitlet.MaxImportRefs {
			t.Errorf("record %d carries %d packs and %d refs, past a record's %d and %d", i, len(packs[i]),
				len(refs[i]), gitlet.MaxImportPacks, gitlet.MaxImportRefs)
		}
		if len(refs[i]) > 0 && len(gotOuts)+len(packs[i]) != len(outs) {
			t.Errorf("record %d carries refs with %d of the %d packs landed", i, len(gotOuts)+len(packs[i]),
				len(outs))
		}
		gotOuts, gotCmds = append(gotOuts, packs[i]...), append(gotCmds, refs[i]...)
	}
	if !slices.Equal(gotOuts, outs) || !slices.Equal(gotCmds, cmds) {
		t.Fatalf("the records carry %d packs and %d refs, want every one of the %d and %d once, in order",
			len(gotOuts), len(gotCmds), len(outs), len(cmds))
	}
	if len(packs) != 4 {
		t.Errorf("the attempt lands %d records, want 4: two of packs alone, the last packs with the first "+
			"refs, and the rest of the refs", len(packs))
	}
}

// AN IMPORT OF LONG REFS LANDS IN THE CHUNKS ITS RECORDS HOLD:
// gitlet.MaxImportRefs tags whose names JSON spells long (`&`, six bytes each,
// at the longest ref name) pass one record's bytes long before its count, so
// each chunk is cut to what its record holds: more chunks than the count alone
// cuts, each applied, ending active with every ref.
//
// Mutation: cut chunks by gitlet.MaxImportRefs alone, and chunk 1 is refused
// invalid at every attempt, the repository importing for ever.
func TestAnImportOfLongRefsLandsInTheChunksItsRecordsHold(t *testing.T) {
	h := newHarness(t, gitlet.Limits{})
	first := h.src.commit(map[string]string{"a": "a\n"}, "first")
	h.src.push("HEAD:refs/heads/main")
	var tags strings.Builder
	for i := range gitlet.MaxImportRefs {
		fmt.Fprintf(&tags, "create refs/tags/%s%05d %s\n", strings.Repeat("&", gitlet.MaxRefNameBytes-15), i, first)
	}
	if _, stderr, err := h.src.c.Try(t, "", strings.NewReader(tags.String()), "--git-dir="+h.src.bare,
		"update-ref", "--stdin"); err != nil {
		t.Fatalf("tagging the source: %v\n%s", err, stderr)
	}
	h.f.Declare(t, gitlet.Declared{Name: "imported", Settings: gitlettest.Settings(),
		Import: &gitlet.ImportSource{URL: h.src.url}})

	h.pass(h.mirrors())
	rows := h.rows("imported")
	if rows.Repo.Status != gitlet.RepoStatusActive || len(rows.Refs) != gitlet.MaxImportRefs+1 {
		t.Fatalf("after its import the repository is %s with %d refs; want it active with the source's %d",
			rows.Repo.Status, len(rows.Refs), gitlet.MaxImportRefs+1)
	}
	chunks, dones := h.est.chunks()
	if len(chunks) < 2 || len(dones) != 1 || dones[0].Error != "" || dones[0].Seq != int64(len(chunks)) {
		t.Fatalf("the import proposed %d chunks and ended %+v; want more than the one its count alone cuts, "+
			"then one end after the last, with no error", len(chunks), dones)
	}
}

// A CUT OF WHAT THE COPY HOLDS STORES NOTHING: a fetch bringing only objects
// the copy holds (the restart after a chunk of packs alone applied, which no
// ref of the copy reaches) is cut into no output, never an empty pack, which no
// record may carry, so a resumed import would fail on it at every attempt.
//
// Mutation: drop the cut's --non-empty, and it names the empty pack git writes.
func TestACutOfWhatTheCopyHoldsStoresNothing(t *testing.T) {
	h := newHarness(t, gitlet.Limits{})
	h.src.commit(map[string]string{"a": "a\n"}, "first")
	h.src.push("HEAD:refs/heads/main")
	h.f.Declare(t, gitlet.Declared{Name: "imported", Settings: gitlettest.Settings(),
		Import: &gitlet.ImportSource{URL: h.src.url}})
	h.pass(h.mirrors())
	rows := h.rows("imported")
	if rows.Repo.Status != gitlet.RepoStatusActive || len(rows.Packs) == 0 {
		t.Fatalf("after its import the repository is %s with %d packs; want it active with what it imported",
			rows.Repo.Status, len(rows.Packs))
	}

	outs, err := h.mirrors().CutOfWhatTheCopyHolds(t.Context(), rows.Repo.ID, gitlettest.Linearizable)
	if err != nil {
		t.Fatalf("the cut of what the copy holds: %v", err)
	}
	if len(outs) != 0 {
		t.Fatalf("the cut of what the copy holds wrote %v; want no output", outs)
	}
}

// held is every ref the source's served repository holds, and the object
// each names: what the other host holds, for a push mirror's case.
func (s *source) held() map[string]string {
	s.t.Helper()
	out := map[string]string{}
	for line := range strings.Lines(s.c.Run(s.t, "", "--git-dir="+s.bare, "for-each-ref",
		"--format=%(refname) %(objectname)")) {
		if ref, oid, ok := strings.Cut(strings.TrimSpace(line), " "); ok {
			out[ref] = oid
		}
	}
	return out
}

// A PUSH MIRROR WAITS FOR GREEN (M6-D16): a protected main is pushed at its
// GREEN row's commit and every tag at its tip (the source stands for a forge
// taking pushes). While main's gate requires a check no commit passed, main has
// no green: the tag goes, main held back as waiting. A push under a gate
// requiring no check is main's green, and the next attempt pushes it; a later
// push above it, under the check again, leaves the other host's main at the
// green while the new tag goes. Each attempt that moved something announces
// code_mirror_pushed, naming what it held back.
//
// Mutation: push a protected branch at its tip, and the other host's main is a
// commit no check passed; leave waiting branches out of the account, and the
// first announcement names none.
func TestAPushMirrorWaitsForGreen(t *testing.T) {
	h := newHarness(t, gitlet.Limits{})
	h.src.c.Run(t, "", "--git-dir="+h.src.bare, "config", "http.receivepack", "true")
	gated := gitlettest.Settings()
	gated.Protections[0].Gate = &gitlet.MergeGate{MergeBy: gitlet.MergeByWriters, Approvals: gitlet.ApprovalsNone,
		Checks: []string{"unit"}, Landing: gitlet.Landing{Depth: 4, Batch: 4}, DeleteSource: gitlet.DeleteNone}
	gated.MirrorTo = &gitlet.MirrorTarget{URL: h.src.url, Credential: "${MIRROR_TOKEN}", As: gitlettest.FixturePat}
	open := gated
	open.Protections = []gitlet.Protection{{PushRule: gated.Protections[0].PushRule}}
	h.f.Declare(t, gitlet.Declared{Name: "pushed", Settings: gated})
	p := gitlettest.NewPerson(t, h.f, "ceo")
	url := h.f.Nodes()[0].URL() + "/git/pushed"
	wt := p.Init()
	push := func(refspecs ...string) {
		t.Helper()
		if got := p.Push(t.Context(), wt, url, refspecs...); got.Err != nil {
			t.Fatalf("push %v: %s", refspecs, got)
		}
	}
	m := h.mirrors()

	// NO GREEN YET: main waits, its tag goes.
	a := p.Commit(wt, map[string]string{"a": "a\n"}, "first")
	push("HEAD:refs/heads/main", "HEAD:refs/tags/v1")
	h.pass(m)
	held := h.src.held()
	if got, ok := held["refs/heads/main"]; ok {
		t.Errorf("the other host's main is %s while main has no green commit; want it held back", got)
	}
	if got := held["refs/tags/v1"]; got != a {
		t.Errorf("the other host's v1 is %q, want the tag's %s", got, a)
	}
	if pushed := h.events.all(); len(pushed) != 1 ||
		!slices.Equal(pushed[0].Waiting, []string{"refs/heads/main"}) ||
		!slices.Equal(pushed[0].Updated, []string{"refs/tags/v1 " + a}) || pushed[0].As != gitlettest.FixturePat {
		t.Fatalf("the pushes announced %+v; want one, setting v1 at %s and holding main back, as %s", pushed, a,
			gitlettest.FixturePat)
	}

	// A GREEN COMMIT: a push under a gate that requires no check is main's
	// green as it lands, and the next attempt pushes it.
	h.f.Declare(t, gitlet.Declared{Name: "pushed", Settings: open})
	b := p.Commit(wt, map[string]string{"b": "b\n"}, "second")
	push("HEAD:refs/heads/main")
	h.advance(gitlet.MirrorInterval + time.Minute)
	h.pass(m)
	if got := h.src.held()["refs/heads/main"]; got != b {
		t.Fatalf("the other host's main is %q once %s is green, want it", got, b)
	}

	// ABOVE THE GREEN: a push under the check again lands above main's
	// green, which the other host keeps; the new tag goes at its tip.
	h.f.Declare(t, gitlet.Declared{Name: "pushed", Settings: gated})
	c := p.Commit(wt, map[string]string{"c": "c\n"}, "third")
	push("HEAD:refs/heads/main", "HEAD:refs/tags/v2")
	h.advance(gitlet.MirrorInterval + time.Minute)
	h.pass(m)
	held = h.src.held()
	if got := held["refs/heads/main"]; got != b {
		t.Errorf("the other host's main is %s with %s landed above its green %s; want the green", got, c, b)
	}
	if got := held["refs/tags/v2"]; got != c {
		t.Errorf("the other host's v2 is %q, want the tag's %s", got, c)
	}
}

// pushMirrored is a harness whose source takes pushes (a push mirror's other
// host), "pushed" declared with settings(url), a person's work tree, and a push
// of refspecs from it to "pushed" that fails the case unless every ref went
// through.
func pushMirrored(t *testing.T, settings func(url string) gitlet.RepoSettings) (*harness, *gitlettest.Person, string,
	func(refspecs ...string)) {

	t.Helper()
	h := newHarness(t, gitlet.Limits{})
	h.src.c.Run(t, "", "--git-dir="+h.src.bare, "config", "http.receivepack", "true")
	h.f.Declare(t, gitlet.Declared{Name: "pushed", Settings: settings(h.src.url)})
	p := gitlettest.NewPerson(t, h.f, "ceo")
	url := h.f.Nodes()[0].URL() + "/git/pushed"
	wt := p.Init()
	push := func(refspecs ...string) {
		t.Helper()
		if got := p.Push(t.Context(), wt, url, refspecs...); got.Err != nil {
			t.Fatalf("push %v: %s", refspecs, got)
		}
	}
	return h, p, wt, push
}

// mirroredTo is the fixture settings — main protected by a rule with no
// gate, so every push to it lands its green — with a push mirror to url
// covering refs (the defaults, where it names none).
func mirroredTo(url string, refs ...string) gitlet.RepoSettings {
	s := gitlettest.Settings()
	s.MirrorTo = &gitlet.MirrorTarget{URL: url, Credential: "${MIRROR_TOKEN}", As: gitlettest.FixturePat, Refs: refs}
	return s
}

// A MIRROR LISTS A COPY PAST WHAT A CAPTURE HOLDS, LEAVING THE SEATS OUT: a
// copy may hold more refs than gitproc.CaptureMax holds of a listing (here over
// a megabyte of `<id> <ref>` lines) beside branches in the seats' namespace;
// the listing answers every tag and main, and none of the seats' branches,
// which for-each-ref excludes itself.
//
// Mutation: capture the listing (Cmd.Stdout nil), and git's output outgrows it;
// drop for-each-ref's --exclude, and the seats' branches are listed.
func TestAMirrorListsACopyPastWhatACaptureHoldsLeavingTheSeatsOut(t *testing.T) {
	h := newHarness(t, gitlet.Limits{})
	f := gittest.NewFixture(t)
	s := gittest.Bare(t, f)
	tags := gitproc.CaptureMax / 40 // each line at least an id, a space and a ref: past 40 bytes
	refs := map[string]string{"refs/heads/main": f.Tip()}
	for i := range tags {
		refs[fmt.Sprintf("refs/tags/t-%06d", i)] = f.Tip()
	}
	for i := range 3 {
		refs[fmt.Sprintf("%sana/w-%d", gitref.SeatsRoot, i)] = f.Tip()
	}
	gittest.SetRefs(t, s.Dir, refs)
	got, err := h.mirrors().RefsOf(t.Context(), s.Dir)
	if err != nil {
		t.Fatalf("listing a copy of %d tags: %v", tags, err)
	}
	if len(got) != tags+1 || got["refs/heads/main"] != f.Tip() {
		t.Errorf("the listing named %d refs (main at %q); want main at %s and %d tags", len(got),
			got["refs/heads/main"], f.Tip(), tags)
	}
	for ref := range got {
		if strings.HasPrefix(ref, gitref.SeatsRoot) {
			t.Errorf("the listing named %s, in the seats' namespace", ref)
		}
	}
}

// A PUSH MIRROR LANDS BESIDE A FORGE'S OTHER REFS: the other host holds more
// refs than a capture holds (refs/pull/<n>/head, which no push mirror covers),
// and main still lands, the listing answered in a few bytes: ls-remote asks
// only for the arms its patterns reach (branches, tags), sent as protocol v2
// ref prefixes. With as many branches beside main, which the arms reach and the
// listing streams, the next main still lands.
//
// Mutation: list with `ls-remote --refs` alone and capture it, and the listing
// outgrows the capture; drop the arms alone, and the other host answers with
// every pull-request ref; capture the arms' listing, and the branches outgrow
// it.
func TestAPushMirrorLandsBesideAForgesOtherRefs(t *testing.T) {
	h, p, wt, push := pushMirrored(t, func(url string) gitlet.RepoSettings { return mirroredTo(url) })
	theirs := h.src.commit(map[string]string{"x": "x\n"}, "the forge's own")
	h.src.push("HEAD:refs/pull/0/head")
	var create strings.Builder
	pulls := gitproc.CaptureMax / 40 // each line an id, a tab and refs/pull/<n>/head: past 40 bytes
	for n := 1; n < pulls; n++ {
		fmt.Fprintf(&create, "create refs/pull/%d/head %s\n", n, theirs)
	}
	if _, stderr, err := h.src.c.Try(t, "", strings.NewReader(create.String()), "--git-dir="+h.src.bare,
		"update-ref", "--stdin"); err != nil {
		t.Fatalf("creating %d pull-request refs at the other host: %v\n%s", pulls, err, stderr)
	}

	a := p.Commit(wt, map[string]string{"a": "a\n"}, "first")
	push("HEAD:refs/heads/main")
	h.src.served.Store(0)
	h.pass(h.mirrors())
	if got := h.src.held()["refs/heads/main"]; got != a {
		t.Fatalf("the other host's main is %q beside its %d pull-request refs; want %s", got, pulls, a)
	}
	if got, whole := h.src.served.Load(), int64(pulls)*40; got >= whole {
		t.Errorf("the other host answered the mirror's listing with %d bytes; want far under the %d its "+
			"pull-request refs alone take, which the mirror never asks for", got, whole)
	}

	// AS MANY BRANCHES BESIDE MAIN: an arm the mirror asks for, past what a
	// capture holds, which the listing reads as it streams.
	var branches strings.Builder
	for n := range pulls {
		fmt.Fprintf(&branches, "create refs/heads/b-%06d %s\n", n, theirs)
	}
	if _, stderr, err := h.src.c.Try(t, "", strings.NewReader(branches.String()), "--git-dir="+h.src.bare,
		"update-ref", "--stdin"); err != nil {
		t.Fatalf("creating %d branches at the other host: %v\n%s", pulls, err, stderr)
	}
	b := p.Commit(wt, map[string]string{"b": "b\n"}, "second")
	push("HEAD:refs/heads/main")
	h.advance(gitlet.MirrorInterval + time.Minute)
	h.pass(h.mirrors())
	if got := h.src.held()["refs/heads/main"]; got != b {
		t.Fatalf("the other host's main is %q beside its %d other branches; want %s", got, pulls, b)
	}
}

// A PUSH MIRROR DELETES WHAT THE REPOSITORY DROPPED, AND NEVER A BRANCH IT
// HOLDS BACK: after a first push of main, release and v1, the repository
// deletes v1 and a rule protects release with a check no commit passed, so
// release has no green; the next attempt deletes v1 at the other host and
// leaves release there, waiting rather than deleted as a ref the push does not
// set. The announcement names both.
//
// Mutation: emit no deletion, and the other host keeps v1; delete a waiting
// branch like any ref the push does not set, and the other host loses release.
func TestAPushMirrorDeletesWhatTheRepositoryDropped(t *testing.T) {
	covers := []string{"refs/heads/main", "refs/heads/release", "refs/tags/*"}
	var settings gitlet.RepoSettings
	h, p, wt, push := pushMirrored(t, func(url string) gitlet.RepoSettings {
		settings = mirroredTo(url, covers...)
		return settings
	})
	m := h.mirrors()
	a := p.Commit(wt, map[string]string{"a": "a\n"}, "first")
	push("HEAD:refs/heads/main", "HEAD:refs/heads/release", "HEAD:refs/tags/v1")
	h.pass(m)
	if held := h.src.held(); held["refs/heads/release"] != a || held["refs/tags/v1"] != a {
		t.Fatalf("the first push left the other host holding %v; want release and v1 at %s", held, a)
	}

	gated := settings
	gated.Protections = append(slices.Clone(settings.Protections), gitlet.Protection{
		PushRule: gitlet.PushRule{Pattern: "refs/heads/release", DirectPush: gitlet.DirectPersons},
		Gate: &gitlet.MergeGate{MergeBy: gitlet.MergeByWriters, Approvals: gitlet.ApprovalsNone,
			Checks: []string{"unit"}, Landing: gitlet.Landing{Depth: 4, Batch: 4}, DeleteSource: gitlet.DeleteNone}})
	h.f.Declare(t, gitlet.Declared{Name: "pushed", Settings: gated})
	push(":refs/tags/v1")
	h.advance(gitlet.MirrorInterval + time.Minute)
	h.pass(m)
	held := h.src.held()
	if got, ok := held["refs/tags/v1"]; ok {
		t.Errorf("the other host still holds v1 at %s once the repository deleted it; want it deleted", got)
	}
	if got := held["refs/heads/release"]; got != a {
		t.Errorf("the other host's release is %q while release waits for green; want it left at %s", got, a)
	}
	pushed := h.events.all()
	if last := pushed[len(pushed)-1]; !slices.Equal(last.Deleted, []string{"refs/tags/v1"}) ||
		!slices.Equal(last.Waiting, []string{"refs/heads/release"}) {
		t.Errorf("the last push announced %+v; want v1 deleted and release waiting", last)
	}
}

// A PUSH MIRROR DIALS ONLY WHEN WHAT IT COVERS MOVED: once its first push left
// the other host level, a nudge after a push beside main dials nobody; an
// interval on, a nudge reads the other host whatever moved and puts back what
// another writer moved there; a nudge after main moved dials and pushes it.
//
// Mutation: dial on every nudge (drop the comparison with what the last attempt
// left level), and the push beside main dials; never read the other host again
// (drop the interval), and the other writer's main stays.
func TestAPushMirrorDialsOnlyWhenWhatItCoversMoved(t *testing.T) {
	h, p, wt, push := pushMirrored(t, func(url string) gitlet.RepoSettings { return mirroredTo(url) })
	var dials atomic.Int64
	h.dial = func(ctx context.Context, url, _ string) (gitproc.Remote, error) {
		dials.Add(1)
		return gitproc.ResolveRemote(ctx, url, true)
	}
	m := h.mirrors()
	a := p.Commit(wt, map[string]string{"a": "a\n"}, "first")
	push("HEAD:refs/heads/main")
	h.pass(m)
	if got := h.src.held()["refs/heads/main"]; got != a || dials.Load() != 1 {
		t.Fatalf("the first push left the other host's main at %q after %d dials; want %s after one", got,
			dials.Load(), a)
	}
	id := h.rows("pushed").Repo.ID
	nudge := func(after time.Duration) {
		t.Helper()
		h.advance(after)
		if err := m.Work(t.Context(), id, statelog.Position{}); err != nil {
			t.Fatalf("nudge %s: %v", id, err)
		}
		m.Wait()
	}

	// NOTHING IT COVERS MOVED: a branch beside main.
	b := p.Commit(wt, map[string]string{"b": "b\n"}, "second")
	push("HEAD:refs/heads/feature")
	nudge(mirror.PushPace + time.Second)
	if got := dials.Load(); got != 1 {
		t.Errorf("a nudge after a push of nothing the mirror covers dialed the other host (%d dials); want "+
			"none", got-1)
	}

	// AN INTERVAL ON: another writer moved main at the other host, and the
	// next nudge reads it and puts main back.
	h.src.commit(map[string]string{"x": "x\n"}, "somebody else's")
	h.src.push("+HEAD:refs/heads/main")
	nudge(gitlet.MirrorInterval)
	if got := h.src.held()["refs/heads/main"]; got != a || dials.Load() != 2 {
		t.Errorf("an interval on, the other host's main is %q after %d dials; want %s put back after two",
			got, dials.Load(), a)
	}

	// MAIN MOVED: dialed, and pushed.
	push("HEAD:refs/heads/main")
	nudge(mirror.PushPace + time.Second)
	if got := h.src.held()["refs/heads/main"]; got != b || dials.Load() != 3 {
		t.Errorf("after main moved the other host's main is %q after %d dials; want %s after three", got,
			dials.Load(), b)
	}
}

// busyLane is the node's pool, signalling refused each time it refuses a
// maintenance slot: the moment an attempt's cut finds the lane full.
type busyLane struct {
	*gitproc.Pool
	refused chan struct{}
}

func (b busyLane) Reserve() (*gitproc.Reserved, error) {
	slot, err := b.Pool.Reserve()
	if errors.Is(err, gitproc.ErrBusy) {
		select {
		case b.refused <- struct{}{}:
		default:
		}
	}
	return slot, err
}

// A CUT WAITS FOR A MAINTENANCE SLOT AND KEEPS ITS FETCH: an import whose fetch
// landed in its scratch while every maintenance slot is held waits for one,
// then cuts and lands what it fetched, the source read ONCE, never ending on
// the lane's refusal and downloading it all again.
//
// Mutation: ask the lane once and end the attempt on its refusal, as a
// maintenance Run does, and the repository is still importing once the slots
// are free.
func TestACutWaitsForAMaintenanceSlotAndKeepsItsFetch(t *testing.T) {
	h := newHarness(t, gitlet.Limits{})
	head := h.src.commit(map[string]string{"a": "a\n"}, "first")
	h.src.push("HEAD:refs/heads/main")
	h.f.Declare(t, gitlet.Declared{Name: "imported", Settings: gitlettest.Settings(),
		Import: &gitlet.ImportSource{URL: h.src.url}})

	pool := h.f.Nodes()[0].Pool()
	var held []*gitproc.Reserved
	for {
		slot, err := pool.Reserve()
		if errors.Is(err, gitproc.ErrBusy) {
			break
		}
		if err != nil {
			t.Fatalf("holding the maintenance lane: %v", err)
		}
		held = append(held, slot)
	}
	refused := make(chan struct{}, 1)
	h.git = busyLane{Pool: pool, refused: refused}
	m := h.mirrors()
	h.listAndWork(m)
	select {
	case <-refused:
	case <-time.After(time.Minute): //fixedwait:liveness
		t.Fatal("the import never asked the full maintenance lane for its cut's slot")
	}
	if got := h.src.reads.Load(); got != 1 {
		t.Fatalf("the source was read %d times before the cut; want the fetch's once", got)
	}
	for _, slot := range held {
		slot.Release()
	}
	m.Wait()

	rows := h.rows("imported")
	if rows.Repo.Status != gitlet.RepoStatusActive || rows.Refs["refs/heads/main"] != head {
		t.Fatalf("once a slot freed the repository is %s with main at %q; want it active at the source's %s",
			rows.Repo.Status, rows.Refs["refs/heads/main"], head)
	}
	if got := h.src.reads.Load(); got != 1 {
		t.Errorf("the source was read %d times; want once, the fetch the cut waited with", got)
	}
}

// EVERY DUE MIRROR IS ATTEMPTED WITHIN ITS INTERVAL: a node home to more due
// repositories than MaxAttempts a pass, a pass a minute, could start in a
// gitlet.MirrorInterval (here imports whose source refuses at once) attempts every one within it: one
// listed while MaxAttempts run waits on the worker and starts as one ends,
// rather than waiting for a pass that hands every slot to the first-listed
// again.
//
// Mutation: drop a repository listed while MaxAttempts run, and a whole
// interval's passes attempt MaxAttempts of them.
func TestEveryDueMirrorIsAttemptedWithinItsInterval(t *testing.T) {
	const backstop = time.Minute // internal/gitlet/home's Backstop: a pass a minute
	passes := int(gitlet.MirrorInterval / backstop)
	n := mirror.MaxAttempts*passes + 1
	h := newHarness(t, gitlet.Limits{})
	var (
		mu     sync.Mutex
		gate   chan struct{}
		dialed = map[string]bool{}
	)
	// THE GATE holds every attempt dialed during a pass's turns, so each turn
	// past the first MaxAttempts finds them running however fast the source
	// answers; after the turns, the source refuses at once.
	h.dial = func(ctx context.Context, url, _ string) (gitproc.Remote, error) {
		mu.Lock()
		dialed[url] = true
		g := gate
		mu.Unlock()
		select {
		case <-g:
		case <-ctx.Done():
		}
		return gitproc.Remote{}, errors.New("the source refuses every fetch")
	}
	repos := make([]gitlet.Declared, n)
	for i := range repos {
		repos[i] = gitlet.Declared{Name: fmt.Sprintf("imported-%02d", i), Settings: gitlettest.Settings(),
			Import: &gitlet.ImportSource{URL: fmt.Sprintf("https://source.example.com/r%02d.git", i)}}
	}
	h.f.Declare(t, repos...)
	attempted := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(dialed)
	}

	m := h.mirrors()
	for pass := 0; pass < passes && attempted() < n; pass++ {
		g := make(chan struct{})
		mu.Lock()
		gate = g
		mu.Unlock()
		h.listAndWork(m)
		close(g)
		m.Wait()
		h.advance(backstop)
	}
	if got := attempted(); got != n {
		t.Fatalf("%d passes, a %v interval's, attempted %d of the %d due repositories; want every one",
			passes, gitlet.MirrorInterval, got, n)
	}
}

// A QUEUED ATTEMPT STARTS LONGEST UNATTEMPTED FIRST: one this node never
// attempted, then the one attempted longest ago, a tie by queue order; never
// listing order, which keeps an idle pull mirror (whose row no sync moves) at
// its front.
//
// Mutation: start them in queue order, and the repository attempted last starts
// first; break a tie by the later arrival, and d starts ahead of c.
func TestAQueuedAttemptStartsLongestUnattemptedFirst(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	got := mirror.StartOrder([]string{"a", "b", "c", "d"}, map[string]time.Time{
		"a": at.Add(2 * time.Minute), "c": at.Add(time.Minute), "d": at.Add(time.Minute)})
	if want := []string{"b", "c", "d", "a"}; !slices.Equal(got, want) {
		t.Fatalf("the queued attempts start %v; want %v", got, want)
	}
}

// A MIRROR THE LISTING NO LONGER NAMES IS FORGOTTEN: once an import has ended,
// the next whole listing, no longer naming it, leaves the worker remembering
// nothing of it, so its memory is what the last listing named, not every
// repository ever listed.
//
// Mutation: keep what a listing does not name, and the ended import's memory
// stays.
func TestAMirrorTheListingNoLongerNamesIsForgotten(t *testing.T) {
	h := newHarness(t, gitlet.Limits{})
	h.src.commit(map[string]string{"a": "a\n"}, "first")
	h.src.push("HEAD:refs/heads/main")
	h.f.Declare(t, gitlet.Declared{Name: "imported", Settings: gitlettest.Settings(),
		Import: &gitlet.ImportSource{URL: h.src.url}})
	m := h.mirrors()
	h.pass(m)
	if rows := h.rows("imported"); rows.Repo.Status != gitlet.RepoStatusActive {
		t.Fatalf("after its import the repository is %s, want active", rows.Repo.Status)
	}
	if got := m.Remembered(); got == 0 {
		t.Fatal("the worker remembers nothing of the import it has just run; the case proves nothing")
	}

	h.pass(m)
	if got := m.Remembered(); got != 0 {
		t.Errorf("after a listing that no longer names the ended import the worker remembers %d entries of "+
			"it; want none", got)
	}
}

// A MIRROR QUEUED PAST ITS PROMISE IS FAILING: a pull mirror every pass lists
// and queues behind attempts holding every slot, so never brought level, raises
// code_mirror_failing once its promise has passed: queued counts as asked for,
// so a node home to more mirrors than its attempts bring level says so rather
// than taking the mirror for no longer its own.
//
// Mutation: record nothing of a mirror a turn queues, and the alarm counts
// none.
func TestAMirrorQueuedPastItsPromiseIsFailing(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	var turns []time.Time
	for d := time.Duration(0); d <= gitlet.MirrorInterval+mirror.AttemptDeadline; d += time.Minute {
		turns = append(turns, at.Add(d))
	}
	if got := mirror.FailingWhileQueued(turns); got != 1 {
		t.Fatalf("a mirror queued at every pass for %v counts %d failing; want 1", turns[len(turns)-1].Sub(at), //fixedwait:stated the case's own turns, a time.Date and whole minutes on
			got)
	}
}

// AN ATTEMPT RUNNING PAST ITS PROMISE IS FAILING: push mirrors whose attempts
// hold every slot past the promise, and a pull mirror queued behind them, all
// count toward code_mirror_failing: the ask at every pass says the node is
// still home, whether it found the attempt running or queued it.
//
// Mutation: record an ask only where it starts or queues an attempt, and the
// running attempts count none.
func TestAnAttemptRunningPastItsPromiseIsFailing(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	var turns []time.Time
	for d := time.Duration(0); d <= gitlet.MirrorInterval+mirror.AttemptDeadline; d += time.Minute {
		turns = append(turns, at.Add(d))
	}
	if got, want := mirror.FailingPastThePromise(turns), mirror.MaxAttempts+1; got != want {
		t.Fatalf("%d attempts running and a mirror queued behind them for %v count %d failing; want %d",
			mirror.MaxAttempts, turns[len(turns)-1].Sub(at), got, want) //fixedwait:stated the case's own turns, a time.Date and whole minutes on
	}
}

// gitletless is an identity directory whose every seat is an active person
// given every app but Gitlet, until given is set.
type gitletless struct{ given atomic.Bool }

func (g *gitletless) Requester(_ context.Context, seat string) (iam.Principal, error) {
	grants := []iam.Grant{iam.GrantWorklet, iam.GrantDoclet, iam.GrantArtifacts, iam.GrantChatlet}
	if g.given.Load() {
		grants = append(grants, iam.GrantGitlet)
	}
	return iam.Principal{Kind: iam.KindPerson, Seat: seat, Stage: iam.StageActive, Grants: grants}, nil
}

// A PUSH MIRROR PUSHES AS A PERSON ONLY WHILE THEY USE GITLET (ADR-0110): a
// push mirror publishes only what its seat reads, and a person reads only
// through Gitlet, so while the directory denies pat (the fixture mirror's seat)
// Gitlet, an attempt publishes nothing; once given, the next publishes main. The directory is read before every push, so taking the
// app away stops the next one.
//
// Mutation: hand gitlet.MirrorSeatRefusal false in the push, and the other host
// gets main while pat holds no Gitlet.
func TestAPushMirrorPushesAsAPersonOnlyWhileTheyUseGitlet(t *testing.T) {
	h, p, wt, push := pushMirrored(t, func(url string) gitlet.RepoSettings { return mirroredTo(url) })
	people := &gitletless{}
	h.people = people
	a := p.Commit(wt, map[string]string{"a": "a\n"}, "first")
	push("HEAD:refs/heads/main")
	h.pass(h.mirrors())
	if got := h.src.held()["refs/heads/main"]; got != "" {
		t.Fatalf("the other host's main is %q while pat holds no Gitlet; want nothing published", got)
	}
	people.given.Store(true)
	h.advance(gitlet.MirrorInterval + time.Minute)
	h.pass(h.mirrors())
	if got := h.src.held()["refs/heads/main"]; got != a {
		t.Fatalf("the other host's main is %q once pat holds Gitlet; want %s", got, a)
	}
}
