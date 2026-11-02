package mirror_test

import (
	"context"
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

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/code/mirror"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
)

// source is a repository at another host: a bare repository served over
// smart HTTP — git's own http-backend, as a forge serves one — behind TLS,
// and a work tree its history is made in. The process's SSL_CERT_FILE names
// the authority that signed the server's certificate, which is how a git
// node's git trusts a source (gitproc.ResolveRemote), so no case using one
// runs in parallel.
type source struct {
	t    *testing.T
	c    *gittest.Client
	wt   string
	bare string
	url  string
	// reads counts the git processes that read the served repository: each
	// fetch or ls-remote opens with exactly one advertisement request,
	// `GET info/refs?service=git-upload-pack`, whatever the protocol does
	// after it.
	reads *atomic.Int64
}

func newSource(t *testing.T) *source {
	t.Helper()
	c := gittest.NewClient(t)
	root := t.TempDir()
	bare := filepath.Join(root, "src.git")
	c.Run(t, "", "init", "-q", "--bare", bare)
	reads := &atomic.Int64{}
	backend := c.HTTPBackend(root)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/info/refs") &&
			r.URL.Query().Get("service") == "git-upload-pack" {
			reads.Add(1)
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
	return &source{t: t, c: c, wt: c.Init(t), bare: bare, url: srv.URL + "/src.git", reads: reads}
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
	imports []code.ImportProposal
	syncs   []code.MirrorSyncProposal
}

func (r *recording) Import(ctx context.Context, p code.ImportProposal) (code.SyncResult, error) {
	r.mu.Lock()
	r.imports = append(r.imports, p)
	r.mu.Unlock()
	return r.Estate.Import(ctx, p)
}

func (r *recording) MirrorSync(ctx context.Context, p code.MirrorSyncProposal) (code.SyncResult, error) {
	r.mu.Lock()
	r.syncs = append(r.syncs, p)
	r.mu.Unlock()
	return r.Estate.MirrorSync(ctx, p)
}

// chunks and dones are the import's proposals so far, each kind apart.
func (r *recording) chunks() (chunks []code.ImportChunk, dones []code.ImportDone) {
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

// announced is a node's event stream as a push announces itself on it, every
// push's account kept.
type announced struct {
	mu     sync.Mutex
	pushed []types.CodeMirrorPushed
}

func (a *announced) Publish(_ context.Context, _ string, ev *events.Event) error {
	if p, ok := ev.Data.(*types.CodeMirrorPushed); ok {
		a.mu.Lock()
		a.pushed = append(a.pushed, *p)
		a.mu.Unlock()
	}
	return nil
}

// all is every push's account so far.
func (a *announced) all() []types.CodeMirrorPushed {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.pushed)
}

// harness is one data node and its git node, a source at another host, and
// the clock its workers read.
type harness struct {
	t      *testing.T
	f      *codetest.SoloFleet
	src    *source
	est    *recording
	faults *codefault.Script
	limits code.Limits
	events announced
	// dial is what the workers dial a URL with: nil for the source's own
	// dial, as Tier A's git.import_allow_private lets a node dial loopback.
	dial mirror.Dial
	// git is the git pool the workers run in: nil for the git node's own.
	git mirror.Pool

	mu  sync.Mutex
	now time.Time
}

func newHarness(t *testing.T, limits code.Limits) *harness {
	t.Helper()
	src := newSource(t)
	faults := &codefault.Script{}
	f := codetest.SoloNode(t, codetest.SoloOptions{Shape: codetest.Shape{Limits: limits}, Faults: faults})
	full := code.DefaultLimits()
	if limits.MaxBlobBytes != 0 {
		full.MaxBlobBytes = limits.MaxBlobBytes
	}
	return &harness{t: t, f: f, src: src, est: &recording{Estate: f.Code(t)}, faults: faults, limits: full,
		now: time.Now()}
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

// mirrors is the git node's worker as the engine builds it — the fleet's
// one git node's cache and pool, the fleet's object store, the fixture chart
// a push mirror's seat is judged against and the stream its pushes are
// announced on — dialing the source as Tier A's git.import_allow_private lets
// a node dial loopback, or as the case's dial does. Each call is a fresh
// worker: what it knows of a repository is the rows'.
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
	m, err := mirror.New(mirror.Options{Estate: h.est, Cache: n.Cache(), Git: git, Objects: objects,
		Dial: dial, NodeID: n.ID(), Limits: func() code.Limits { return h.limits },
		Roster: codetest.FixtureRoster, Events: &h.events, Faults: h.faults, Now: h.clock})
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { m.Stop(context.Background()) })
	return m
}

// pass is one turn of a home's backstop over m: every page of its listing,
// each repository it names worked, and every attempt that started waited
// for. It answers the repositories listed.
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
func (h *harness) rows(name string) codetest.Rows {
	h.t.Helper()
	rows, err := h.f.Rows(h.t.Context(), name, codetest.Linearizable)
	if err != nil {
		h.t.Fatalf("the rows of %s: %v", name, err)
	}
	return rows
}

// AN IMPORT RESUMES FROM ITS ROWS: an import of more refs than one chunk
// carries lands its first chunk and dies before the next; a fresh worker —
// knowing nothing but the rows — lands the next chunk at the next Seq and
// ends the import, never landing the first chunk again, and its fetch asks
// only for what the imported refs do not reach: the commit the source made
// meanwhile — an edit of one line of a file the first chunk holds, so the
// source sends that file's new blob as a delta against the old one, a THIN
// pack the fetch completes by appending the old blob from the copy — and its
// cut stores nothing the first chunk's pack holds, that appended base
// included.
//
// Mutation: start every attempt at Seq 1 rather than the listing's, and the
// second attempt's chunk is refused stale; drop the cut's exclusion of the
// copy's live packs, and the second chunk stores the appended base again.
func TestAnImportResumesFromItsRows(t *testing.T) {
	h := newHarness(t, code.Limits{})
	var file strings.Builder
	for i := range 200 {
		fmt.Fprintf(&file, "line %03d of a file long enough that an edit of one line is sent as a delta\n", i)
	}
	first := h.src.commit(map[string]string{"a": file.String()}, "first")
	h.src.push("HEAD:refs/heads/main")
	// One chunk's refs and one more, so the import takes two chunks.
	var tags strings.Builder
	for i := range code.MaxImportRefs + 1 {
		fmt.Fprintf(&tags, "create refs/tags/v%04d %s\n", i, first)
	}
	if _, stderr, err := h.src.c.Try(t, "", strings.NewReader(tags.String()), "--git-dir="+h.src.bare,
		"update-ref", "--stdin"); err != nil {
		t.Fatalf("tagging the source: %v\n%s", err, stderr)
	}
	h.f.Declare(t, code.Declared{Name: "imported", Settings: codetest.Settings(),
		Import: &code.ImportSource{URL: h.src.url}})

	h.faults.Arm(codefault.ImportAfterPropose, codefault.Action{Once: true, Err: errors.New("the home died")})
	h.pass(h.mirrors())
	rows := h.rows("imported")
	if rows.Repo.Status != code.RepoStatusImporting || len(rows.Refs) != code.MaxImportRefs {
		t.Fatalf("after the first attempt the repository is %s with %d refs; want it importing with its first "+
			"chunk's %d", rows.Repo.Status, len(rows.Refs), code.MaxImportRefs)
	}

	second := h.src.commit(map[string]string{"a": strings.Replace(file.String(), "line 100 ", "line one hundred ",
		1)}, "second")
	h.src.push("HEAD:refs/heads/main")
	h.pass(h.mirrors())

	rows = h.rows("imported")
	if rows.Repo.Status != code.RepoStatusActive {
		t.Fatalf("after the second attempt the repository is %s, want active", rows.Repo.Status)
	}
	if got := rows.Refs["refs/heads/main"]; got != second {
		t.Errorf("main is %s, want the source's %s", got, second)
	}
	if len(rows.Refs) != code.MaxImportRefs+2 {
		t.Errorf("the repository holds %d refs, want the source's %d", len(rows.Refs), code.MaxImportRefs+2)
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
func seqs(chunks []code.ImportChunk) []int64 {
	out := make([]int64, len(chunks))
	for i, c := range chunks {
		out[i] = c.Seq
	}
	return out
}

// A PULL MIRROR FOLLOWS A FORCE-PUSH: a pull mirror is imported from its
// source first, is not synced again within code.MirrorInterval, and once the
// interval has passed its sync moves it onto its source as it is — main onto
// a rewritten history, a branch the source deleted deleted — the move off the
// rewritten tip marked forced, so the committer records the commit it left.
//
// Mutation: never mark a command forced, and the sync's main is not forced;
// leave out the deletions of refs the source dropped, and feature stays; list
// a mirror whatever its last attempt, and it is synced within its interval.
func TestAPullMirrorFollowsAForcePush(t *testing.T) {
	h := newHarness(t, code.Limits{})
	a := h.src.commit(map[string]string{"a": "a\n"}, "first")
	b := h.src.commit(map[string]string{"b": "b\n"}, "second")
	h.src.push("HEAD:refs/heads/main", a+":refs/heads/feature")
	s := codetest.Settings()
	s.Protections = nil
	s.MirrorFrom = &code.MirrorSource{URL: h.src.url}
	h.f.Declare(t, code.Declared{Name: "mirrored", Settings: s})

	m := h.mirrors()
	h.pass(m)
	rows := h.rows("mirrored")
	if rows.Repo.Status != code.RepoStatusActive || !rows.Repo.Mirrored || rows.Refs["refs/heads/main"] != b ||
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
	h.advance(code.MirrorInterval + time.Minute)
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
	moves := map[string]code.Command{}
	for _, cmd := range syncs[0].Sync.Commands {
		moves[cmd.Ref] = cmd
	}
	if got := moves["refs/heads/main"]; got.Old != b || got.New != c || !got.Forced {
		t.Errorf("the sync moved main %+v; want %s to %s, forced", got, b, c)
	}
	if got := moves["refs/heads/feature"]; got.Old != a || got.New != code.ZeroOID {
		t.Errorf("the sync moved feature %+v; want it deleted from %s", got, a)
	}
}

// A REPOSITORY MADE A PULL MIRROR LATER IS IMPORTED FIRST (M6-D16): an active
// repository a person pushed a branch of their own to, then declared a pull
// mirror of a source with more refs than one chunk carries, is brought onto
// its source by an IMPORT — chunk 1, then chunk 2 from the rows, then its end
// — never by a sync, whose fetch is given one push's deadline; and once it is
// done it is the active pull mirror of exactly the source's refs, the branch
// the source never had deleted.
//
// Mutation: drop the settings apply's start of the import (mirrorImportSQL) —
// the repository stays active and its home syncs it, no chunk proposed, red.
func TestARepositoryMadeAPullMirrorIsImportedFirst(t *testing.T) {
	h := newHarness(t, code.Limits{})
	a := h.src.commit(map[string]string{"a": "a\n"}, "first")
	h.src.push("HEAD:refs/heads/main")
	var tags strings.Builder
	for i := range code.MaxImportRefs {
		fmt.Fprintf(&tags, "create refs/tags/v%04d %s\n", i, a)
	}
	if _, stderr, err := h.src.c.Try(t, "", strings.NewReader(tags.String()), "--git-dir="+h.src.bare,
		"update-ref", "--stdin"); err != nil {
		t.Fatalf("tagging the source: %v\n%s", err, stderr)
	}
	s := codetest.Settings()
	s.Protections = nil
	h.f.Declare(t, code.Declared{Name: "later", Settings: s})
	p := codetest.NewPerson(t, h.f, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"local": "local\n"}, "local")
	if res := p.Push(t.Context(), work, h.f.Nodes()[0].URL()+"/git/later", "HEAD:refs/heads/local"); res.Err != nil {
		t.Fatalf("push local: %s", res)
	}

	s.MirrorFrom = &code.MirrorSource{URL: h.src.url}
	h.f.Declare(t, code.Declared{Name: "later", Settings: s})
	if rows := h.rows("later"); rows.Repo.Status != code.RepoStatusImporting || !rows.Repo.Mirrored {
		t.Fatalf("later made a pull mirror is %s (mirrored %t), want importing from its source",
			rows.Repo.Status, rows.Repo.Mirrored)
	}
	h.pass(h.mirrors())

	rows := h.rows("later")
	if rows.Repo.Status != code.RepoStatusActive || !rows.Repo.Mirrored {
		t.Fatalf("after its import later is %s (mirrored %t), want an active pull mirror", rows.Repo.Status,
			rows.Repo.Mirrored)
	}
	if got, ok := rows.Refs["refs/heads/local"]; ok {
		t.Errorf("local is still at %s, which the source never had", got)
	}
	if got := rows.Refs["refs/heads/main"]; got != a || len(rows.Refs) != code.MaxImportRefs+1 {
		t.Errorf("later holds %d refs, main at %s; want the source's %d, main at %s", len(rows.Refs), got,
			code.MaxImportRefs+1, a)
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
// worker's memory, hold a quiet source's mirror back an interval — its import
// that landed whole is its last sync, so a worker that knows nothing of it (a
// restart, or a node newly home to it) does not list it within the interval;
// and once the interval has passed, a sync that finds the source moved nothing
// lands a sync of nothing, which moves the mirror's last sync on.
//
// Mutation: leave a mirror's import end off its last sync, and the fresh
// worker lists it at once; land nothing where the source moved nothing, and
// its last sync stays the import's.
func TestAQuietPullMirrorIsNotFetchedAgainByAFreshHome(t *testing.T) {
	h := newHarness(t, code.Limits{})
	h.src.commit(map[string]string{"a": "a\n"}, "first")
	h.src.push("HEAD:refs/heads/main")
	s := codetest.Settings()
	s.Protections = nil
	s.MirrorFrom = &code.MirrorSource{URL: h.src.url}
	h.f.Declare(t, code.Declared{Name: "quiet", Settings: s})
	h.pass(h.mirrors())
	synced := func() code.MirrorCandidate {
		t.Helper()
		a, err := h.est.Read(t.Context(), code.ReadQuery{Kind: code.ReadMirrorsDue, Fresh: codetest.Linearizable})
		if err != nil || len(a.Mirrors) != 1 {
			t.Fatalf("mirrors_due answered %+v, %v; want the one pull mirror", a.Mirrors, err)
		}
		return a.Mirrors[0]
	}
	imported := synced()
	if rows := h.rows("quiet"); rows.Repo.Status != code.RepoStatusActive || imported.SyncedAt.IsZero() {
		t.Fatalf("after its import the mirror is %s, last synced %v; want it active, synced at its import's end",
			rows.Repo.Status, imported.SyncedAt)
	}
	if listed := h.pass(h.mirrors()); len(listed) != 0 {
		t.Fatalf("a fresh worker listed %v within the interval of the mirror's import", listed)
	}

	h.advance(code.MirrorInterval + time.Minute)
	if listed := h.pass(h.mirrors()); !slices.Contains(listed, imported.ID) {
		t.Fatalf("the mirror was not listed %v once its interval had passed", listed)
	}
	h.est.mu.Lock()
	syncs := slices.Clone(h.est.syncs)
	h.est.mu.Unlock()
	if len(syncs) != 1 || !syncs[0].Sync.Current() {
		t.Fatalf("the quiet mirror proposed %+v; want one sync of nothing", syncs)
	}
	if got := synced(); !got.SyncedAt.After(imported.SyncedAt) {
		t.Fatalf("after a sync of nothing the mirror's last sync is %v, want it past its import's %v",
			got.SyncedAt, imported.SyncedAt)
	}
}

// AN IMPORT PAST THE BLOB LIMIT ENDS WITH ITS ERROR: a source holding a blob
// past code.limits.max_blob_bytes is not landed in part and retried for ever
// — the attempt that finds the blob ends the import, the repository active
// with nothing landed and its import_error naming the blob and the limit.
//
// Mutation: compare the blob's size with >= the limit times two, and the
// chunk is proposed; drop the end's Error, and the import ends clean.
func TestAnImportPastTheBlobLimitEndsWithItsError(t *testing.T) {
	h := newHarness(t, code.Limits{MaxBlobBytes: 1 << 10})
	h.src.commit(map[string]string{"small": "s\n", "big": strings.Repeat("x", 4<<10)}, "too big")
	h.src.push("HEAD:refs/heads/main")
	big := strings.TrimSpace(h.src.c.Run(t, h.src.wt, "rev-parse", "HEAD:big"))
	h.f.Declare(t, code.Declared{Name: "imported", Settings: codetest.Settings(),
		Import: &code.ImportSource{URL: h.src.url}})

	h.pass(h.mirrors())

	rows := h.rows("imported")
	if rows.Repo.Status != code.RepoStatusActive || len(rows.Refs) != 0 || len(rows.Packs) != 0 {
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

// AN ATTEMPT LANDS ITS PACKS BEFORE ITS REFS: no record carries a ref before
// every pack an attempt cut is in a record before it or beside it, and no
// record carries more than code.MaxImportPacks packs or the refs its record
// takes of those offered to it — here code.MaxImportRefs, the records a chunk
// or a sync is held to — and every ref lands once, in order.
//
// Mutation: put the commands in the first record, and a ref rides ahead of
// the packs it needs; drop the cap on a record's packs, and one record
// carries them all; move past every ref offered rather than those taken, and
// refs are lost.
func TestAnAttemptLandsItsPacksBeforeItsRefs(t *testing.T) {
	t.Parallel()
	outs := make([]string, 2*code.MaxImportPacks+1)
	for i := range outs {
		outs[i] = fmt.Sprintf("%040x", i)
	}
	cmds := make([]code.Command, code.MaxImportRefs+1)
	for i := range cmds {
		cmds[i] = code.Command{Ref: fmt.Sprintf("refs/tags/v%04d", i), Old: code.ZeroOID, New: outs[0]}
	}
	packs, refs := mirror.Steps(outs, cmds, func(_ []string, offered []code.Command) int {
		return min(len(offered), code.MaxImportRefs)
	})
	var gotOuts []string
	var gotCmds []code.Command
	for i := range packs {
		if len(packs[i]) > code.MaxImportPacks || len(refs[i]) > code.MaxImportRefs {
			t.Errorf("record %d carries %d packs and %d refs, past a record's %d and %d", i, len(packs[i]),
				len(refs[i]), code.MaxImportPacks, code.MaxImportRefs)
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

// AN IMPORT OF LONG REFS LANDS IN THE CHUNKS ITS RECORDS HOLD: a source of
// code.MaxImportRefs tags whose names JSON spells long — `&`, six bytes each,
// at the longest name a ref may have — and its main branch is past what one
// chunk's record carries by its bytes long before its count, so the attempt
// cuts each chunk to what its record holds: the import lands in more chunks
// than the count alone would cut, each one applied, and ends active with
// every ref of the source.
//
// Mutation: cut each chunk by code.MaxImportRefs alone, and its first chunk is
// refused invalid at every attempt, the repository importing for ever with
// nothing landed.
func TestAnImportOfLongRefsLandsInTheChunksItsRecordsHold(t *testing.T) {
	h := newHarness(t, code.Limits{})
	first := h.src.commit(map[string]string{"a": "a\n"}, "first")
	h.src.push("HEAD:refs/heads/main")
	var tags strings.Builder
	for i := range code.MaxImportRefs {
		fmt.Fprintf(&tags, "create refs/tags/%s%05d %s\n", strings.Repeat("&", code.MaxRefNameBytes-15), i, first)
	}
	if _, stderr, err := h.src.c.Try(t, "", strings.NewReader(tags.String()), "--git-dir="+h.src.bare,
		"update-ref", "--stdin"); err != nil {
		t.Fatalf("tagging the source: %v\n%s", err, stderr)
	}
	h.f.Declare(t, code.Declared{Name: "imported", Settings: codetest.Settings(),
		Import: &code.ImportSource{URL: h.src.url}})

	h.pass(h.mirrors())
	rows := h.rows("imported")
	if rows.Repo.Status != code.RepoStatusActive || len(rows.Refs) != code.MaxImportRefs+1 {
		t.Fatalf("after its import the repository is %s with %d refs; want it active with the source's %d",
			rows.Repo.Status, len(rows.Refs), code.MaxImportRefs+1)
	}
	chunks, dones := h.est.chunks()
	if len(chunks) < 2 || len(dones) != 1 || dones[0].Error != "" || dones[0].Seq != int64(len(chunks)) {
		t.Fatalf("the import proposed %d chunks and ended %+v; want more than the one its count alone cuts, "+
			"then one end after the last, with no error", len(chunks), dones)
	}
}

// A CUT OF WHAT THE COPY HOLDS STORES NOTHING: a fetch that brings only
// objects the copy holds already — the restart after a chunk of packs alone
// applied, whose fetch asks again for all they hold, since no ref of the copy
// reaches them — is cut into no output at all, never into a pack of no
// objects, which no record may carry: an import resumed there would fail on it
// at every attempt and never land its refs.
//
// Mutation: drop the cut's --non-empty, and it names the empty pack git
// writes.
func TestACutOfWhatTheCopyHoldsStoresNothing(t *testing.T) {
	h := newHarness(t, code.Limits{})
	h.src.commit(map[string]string{"a": "a\n"}, "first")
	h.src.push("HEAD:refs/heads/main")
	h.f.Declare(t, code.Declared{Name: "imported", Settings: codetest.Settings(),
		Import: &code.ImportSource{URL: h.src.url}})
	h.pass(h.mirrors())
	rows := h.rows("imported")
	if rows.Repo.Status != code.RepoStatusActive || len(rows.Packs) == 0 {
		t.Fatalf("after its import the repository is %s with %d packs; want it active with what it imported",
			rows.Repo.Status, len(rows.Packs))
	}

	outs, err := h.mirrors().CutOfWhatTheCopyHolds(t.Context(), rows.Repo.ID, codetest.Linearizable)
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

// A PUSH MIRROR WAITS FOR GREEN (M6-D16): a repository whose main a rule
// protects is pushed to its push mirror — the source here, standing for
// another forge that takes pushes — with main at its GREEN row's commit and
// every tag at its tip: while main's gate requires a check none of its
// commits has passed, main has no green row and the other host is given its
// tag alone, main held back as waiting; once a push lands under a gate that
// requires no check, that commit is main's green and the next attempt pushes
// it; and a later push above the green, under the check again, leaves the
// other host's main at the green while its new tag goes. Each attempt that
// moved something is announced (code_mirror_pushed), naming what it held
// back.
//
// Mutation: push a protected branch at its tip rather than its green, and
// the other host's main is a commit no check passed; leave the waiting
// branches out of the account, and the first announcement names none.
func TestAPushMirrorWaitsForGreen(t *testing.T) {
	h := newHarness(t, code.Limits{})
	h.src.c.Run(t, "", "--git-dir="+h.src.bare, "config", "http.receivepack", "true")
	gated := codetest.Settings()
	gated.Protections[0].Gate = &code.MergeGate{MergeBy: code.MergeByWriters, Approvals: code.ApprovalsNone,
		Checks: []string{"unit"}, Landing: code.Landing{Depth: 4, Batch: 4}, DeleteSource: code.DeleteNone}
	gated.MirrorTo = &code.MirrorTarget{URL: h.src.url, Credential: "${MIRROR_TOKEN}", As: codetest.FixturePat}
	open := gated
	open.Protections = []code.Protection{{PushRule: gated.Protections[0].PushRule}}
	h.f.Declare(t, code.Declared{Name: "pushed", Settings: gated})
	p := codetest.NewPerson(t, h.f, "ceo")
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
		!slices.Equal(pushed[0].Updated, []string{"refs/tags/v1 " + a}) || pushed[0].As != codetest.FixturePat {
		t.Fatalf("the pushes announced %+v; want one, setting v1 at %s and holding main back, as %s", pushed, a,
			codetest.FixturePat)
	}

	// A GREEN COMMIT: a push under a gate that requires no check is main's
	// green as it lands, and the next attempt pushes it.
	h.f.Declare(t, code.Declared{Name: "pushed", Settings: open})
	b := p.Commit(wt, map[string]string{"b": "b\n"}, "second")
	push("HEAD:refs/heads/main")
	h.advance(code.MirrorInterval + time.Minute)
	h.pass(m)
	if got := h.src.held()["refs/heads/main"]; got != b {
		t.Fatalf("the other host's main is %q once %s is green, want it", got, b)
	}

	// ABOVE THE GREEN: a push under the check again lands above main's
	// green, which the other host keeps; the new tag goes at its tip.
	h.f.Declare(t, code.Declared{Name: "pushed", Settings: gated})
	c := p.Commit(wt, map[string]string{"c": "c\n"}, "third")
	push("HEAD:refs/heads/main", "HEAD:refs/tags/v2")
	h.advance(code.MirrorInterval + time.Minute)
	h.pass(m)
	held = h.src.held()
	if got := held["refs/heads/main"]; got != b {
		t.Errorf("the other host's main is %s with %s landed above its green %s; want the green", got, c, b)
	}
	if got := held["refs/tags/v2"]; got != c {
		t.Errorf("the other host's v2 is %q, want the tag's %s", got, c)
	}
}

// busyLane is the git node's pool with every refusal of a maintenance slot
// told on refused: the moment an attempt's cut finds the lane full.
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

// A CUT WAITS FOR A MAINTENANCE SLOT AND KEEPS ITS FETCH: an import whose
// fetch has landed in its scratch while every slot of the node's maintenance
// lane is held — a compaction's build, a merge — waits for one, and once a
// slot frees cuts and lands what it fetched, the source read ONCE: never an
// attempt ended on the lane's refusal, its scratch removed with everything it
// downloaded, and the next attempt downloading it all again.
//
// Mutation: ask the lane once and end the attempt on its refusal, as a
// maintenance Run does, and the repository is still importing once the slots
// are free.
func TestACutWaitsForAMaintenanceSlotAndKeepsItsFetch(t *testing.T) {
	h := newHarness(t, code.Limits{})
	head := h.src.commit(map[string]string{"a": "a\n"}, "first")
	h.src.push("HEAD:refs/heads/main")
	h.f.Declare(t, code.Declared{Name: "imported", Settings: codetest.Settings(),
		Import: &code.ImportSource{URL: h.src.url}})

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
	case <-time.After(time.Minute):
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
	if rows.Repo.Status != code.RepoStatusActive || rows.Refs["refs/heads/main"] != head {
		t.Fatalf("once a slot freed the repository is %s with main at %q; want it active at the source's %s",
			rows.Repo.Status, rows.Refs["refs/heads/main"], head)
	}
	if got := h.src.reads.Load(); got != 1 {
		t.Errorf("the source was read %d times; want once, the fetch the cut waited with", got)
	}
}

// EVERY DUE MIRROR IS ATTEMPTED WITHIN ITS INTERVAL: a node home to more due
// repositories than MaxAttempts at once, a pass a minute, could start in a
// code.MirrorInterval — here imports from a source that answers at once,
// refusing — attempts every one of them within that interval: a repository
// listed while MaxAttempts run waits on the worker and starts as one ends,
// rather than waiting for a pass that hands every slot to the ones listed
// first again.
//
// Mutation: drop a repository listed while MaxAttempts run, as a full worker
// did, and the passes of a whole interval attempt MaxAttempts of them.
func TestEveryDueMirrorIsAttemptedWithinItsInterval(t *testing.T) {
	const backstop = time.Minute // internal/code/home's Backstop: a pass a minute
	passes := int(code.MirrorInterval / backstop)
	n := mirror.MaxAttempts*passes + 1
	h := newHarness(t, code.Limits{})
	var (
		mu     sync.Mutex
		gate   chan struct{}
		dialed = map[string]bool{}
	)
	// THE GATE holds every attempt that dials while a pass's turns run, so
	// each turn after the first MaxAttempts finds them running however fast
	// the source answers; once the turns are done, the source refuses at
	// once.
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
	repos := make([]code.Declared, n)
	for i := range repos {
		repos[i] = code.Declared{Name: fmt.Sprintf("imported-%02d", i), Settings: codetest.Settings(),
			Import: &code.ImportSource{URL: fmt.Sprintf("https://source.example.com/r%02d.git", i)}}
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
			passes, code.MirrorInterval, got, n)
	}
}

// A QUEUED ATTEMPT STARTS LONGEST UNATTEMPTED FIRST: of the attempts waiting
// for a slot, the one this node never attempted starts first, then the one
// it attempted longest ago, and of two that ended at one instant the one
// queued first — never the order the listing named them in, which keeps an
// idle pull mirror, whose row no sync moves, at its front.
//
// Mutation: start them in the order they were queued, and the repository
// attempted last starts first; break a tie by the later arrival, and d
// starts ahead of c.
func TestAQueuedAttemptStartsLongestUnattemptedFirst(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	got := mirror.StartOrder([]string{"a", "b", "c", "d"}, map[string]time.Time{
		"a": at.Add(2 * time.Minute), "c": at.Add(time.Minute), "d": at.Add(time.Minute)})
	if want := []string{"b", "c", "d", "a"}; !slices.Equal(got, want) {
		t.Fatalf("the queued attempts start %v; want %v", got, want)
	}
}

// A MIRROR THE LISTING NO LONGER NAMES IS FORGOTTEN: once an import has
// ended, the next whole listing — which no longer names it — leaves the
// worker remembering nothing of it, so a node holds the imports and mirrors
// its last listing named rather than every repository any listing ever
// named.
//
// Mutation: keep what a listing does not name, and the ended import's memory
// stays.
func TestAMirrorTheListingNoLongerNamesIsForgotten(t *testing.T) {
	h := newHarness(t, code.Limits{})
	h.src.commit(map[string]string{"a": "a\n"}, "first")
	h.src.push("HEAD:refs/heads/main")
	h.f.Declare(t, code.Declared{Name: "imported", Settings: codetest.Settings(),
		Import: &code.ImportSource{URL: h.src.url}})
	m := h.mirrors()
	h.pass(m)
	if rows := h.rows("imported"); rows.Repo.Status != code.RepoStatusActive {
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

// A MIRROR QUEUED PAST ITS PROMISE IS FAILING: a pull mirror that every pass
// lists and queues behind attempts holding every slot, and that so is never
// brought level, raises code_mirror_failing once its promise has passed —
// queued is asked for, and a node home to more mirrors than its attempts
// bring level is told so rather than taking the mirror for no longer its own.
//
// Mutation: record nothing of a mirror a turn queues, and the alarm counts
// none.
func TestAMirrorQueuedPastItsPromiseIsFailing(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	var turns []time.Time
	for d := time.Duration(0); d <= code.MirrorInterval+mirror.AttemptDeadline; d += time.Minute {
		turns = append(turns, at.Add(d))
	}
	if got := mirror.FailingWhileQueued(turns); got != 1 {
		t.Fatalf("a mirror queued at every pass for %v counts %d failing; want 1", turns[len(turns)-1].Sub(at),
			got)
	}
}
