package mirror_test

import (
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/code/mirror"
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
}

func newSource(t *testing.T) *source {
	t.Helper()
	c := gittest.NewClient(t)
	root := t.TempDir()
	bare := filepath.Join(root, "src.git")
	c.Run(t, "", "init", "-q", "--bare", bare)
	srv := httptest.NewTLSServer(c.HTTPBackend(root))
	t.Cleanup(srv.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	block := &pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}
	if err := os.WriteFile(ca, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSL_CERT_FILE", ca)
	return &source{t: t, c: c, wt: c.Init(t), bare: bare, url: srv.URL + "/src.git"}
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

// harness is one data node and its git node, a source at another host, and
// the clock its workers read.
type harness struct {
	t      *testing.T
	f      *codetest.SoloFleet
	src    *source
	est    *recording
	faults *codefault.Script
	limits code.Limits

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
// one git node's cache and pool, the fleet's object store — dialing the
// source as Tier A's git.import_allow_private lets a node dial loopback.
// Each call is a fresh worker: what it knows of a repository is the rows'.
func (h *harness) mirrors() *mirror.Mirrors {
	h.t.Helper()
	n := h.f.Nodes()[0]
	objects, err := objstore.NewStore(h.f.Objects())
	if err != nil {
		h.t.Fatal(err)
	}
	m, err := mirror.New(mirror.Options{Estate: h.est, Cache: n.Cache(), Git: n.Pool(), Objects: objects,
		Dial: func(ctx context.Context, url, _ string) (gitproc.Remote, error) {
			return gitproc.ResolveRemote(ctx, url, true)
		},
		NodeID: n.ID(), Limits: func() code.Limits { return h.limits }, Faults: h.faults, Now: h.clock})
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
	m.Wait()
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
// record carries more than code.MaxImportPacks packs or code.MaxImportRefs
// refs — the records a chunk or a sync is held to.
//
// Mutation: put the commands in the first record, and a ref rides ahead of
// the packs it needs; drop the cap on a record's packs, and one record
// carries them all.
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
	packs, refs := mirror.Steps(outs, cmds)
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
	if !slices.Equal(gotOuts, outs) || len(gotCmds) != len(cmds) {
		t.Fatalf("the records carry %d packs and %d refs, want every one of the %d and %d once, in order",
			len(gotOuts), len(gotCmds), len(outs), len(cmds))
	}
	if len(packs) != 4 {
		t.Errorf("the attempt lands %d records, want 4: two of packs alone, the last packs with the first "+
			"refs, and the rest of the refs", len(packs))
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
