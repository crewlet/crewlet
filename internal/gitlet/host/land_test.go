package host_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitletfault"
	"github.com/crewlet/crewlet/internal/gitlet/gitlettest"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/gitlet/host"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/memobj"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE ONE LANDING, ASKED BY A BUILDER THAT IS NOT THE RELAY: each case packs
// its own commits into a scratch with the person's git, no receive-pack or
// hook, as an engine write does, and the landing must take it exactly as it
// takes a push git received.

// scratchFor forms a landing of RepoName on the case's first node as a builder
// other than the relay does: the cache synced, a scratch over it under a fresh
// push id and, given revs, ONE pack of them written by the person's git with
// the .idx (version 2) and .rev a bundle carries; checksum is the pack's. It
// names who pushes, when, and gitlet.ViaEngine, and nothing it judged.
func scratchFor(t *testing.T, c *gitlettest.Case, p *gitlettest.Person, person, work string,
	revs ...string) (l host.TestLanding, checksum string) {

	t.Helper()
	node := c.Fleet.Nodes()[0]
	synced, err := node.Cache().Sync(c.Ctx, c.Rows().Repo.ID, gitlettest.Linearizable)
	if err != nil {
		t.Fatalf("sync %s on %s: %v", gitlettest.RepoName, node.ID(), err)
	}
	t.Cleanup(synced.Release)
	pushID := uuid.Must(uuid.NewV7()).String()
	scratch, err := node.Cache().NewScratch(c.Ctx, synced, pushID)
	if err != nil {
		t.Fatalf("a scratch for push %s: %v", pushID, err)
	}
	t.Cleanup(scratch.Remove)
	if len(revs) > 0 {
		checksum = gittest.Pack(t, p.Git(), scratch.Dir, work, revs...)
	}
	return host.TestLanding{Synced: synced, Scratch: scratch, PushID: pushID, ReceivedAt: time.Now(),
		Principal: gitlet.Principal{Kind: gitlet.PrincipalOperator, OperatorID: person, Seat: person},
		Via:       gitlet.ViaEngine}, checksum
}

// gets is a backend that counts every read of each name.
type gets struct {
	objstore.Backend
	mu sync.Mutex
	of map[string]int
}

func (g *gets) Get(ctx context.Context, name string, off, n int64) (io.ReadCloser, error) {
	g.mu.Lock()
	g.of[name]++
	g.mu.Unlock()
	return g.Backend.Get(ctx, name, off, n)
}

func (g *gets) count(name string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.of[name]
}

// adoptions is the node's crewlet.code.cache.adoptions under outcome.
func adoptions(node gitlettest.Node, outcome string) uint64 {
	var n uint64
	for _, s := range node.Metrics().Read() {
		if s.Name == metrics.CodeCacheAdoptions && s.Attrs["outcome"] == outcome {
			n += s.Total
		}
	}
	return n
}

// A PUSH THE RELAY NEVER SAW LANDS as a push git received does: applied from
// its own row listing the commits it introduces, its bundle stored whole and
// sealed, and its pack ADOPTED by the node that built it, so that node's next
// sync does not download it back (cache.Cache.Adopt; nothing else syncs here
// and the disk takes links). The adoption is the landing's, not the relay's,
// or an engine-built push would cost its node a download. Its row names the
// engine as builder and the person's credential and seat as actor, no run.
//
// Mutation: propose the relay's way whatever the landing names (Via: ViaGit in
// land's proposal) — the row reads git.
func TestAPushTheRelayNeverSawLands(t *testing.T) {
	backend := &gets{Backend: memobj.New(), of: map[string]int{}}
	c := newCase(t, gitlettest.SoloOptions{Objects: backend}, gitlettest.Settings())
	node := c.Fleet.Nodes()[0]
	p := gitlettest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	head := p.Commit(work, map[string]string{"README.md": "built by the engine\n"}, "landed")
	l, _ := scratchFor(t, c, p, "ceo", work, head)
	l.Commands = []gitlet.Command{{Ref: "refs/heads/landed", Old: gitlet.ZeroOID, New: head}}
	before := adoptions(node, "adopted")

	got, err := host.Land(c.Ctx, node.Host(), l)
	if err != nil {
		t.Fatalf("land a push the relay never saw: %v", err)
	}
	if got.Outcome != types.AuditApplied || len(got.Commands) != 1 || got.Commands[0].Result != gitlet.ResultOK {
		t.Fatalf("the landing came to %s with %+v, want applied and ok", got.Outcome, got.Commands)
	}
	var row *gitlet.PushRow
	for _, r := range pushRows(t, c) {
		if r.PushID == l.PushID {
			row = &r
		}
	}
	if row == nil || !slices.Contains(row.Introduced, head) {
		t.Fatalf("push %s's row is %+v, want one listing %s among the commits it introduced", l.PushID, row, head)
	}
	if row.Via != gitlet.ViaEngine || row.Actor != "ceo" || row.ActorKind != string(gitlet.PrincipalOperator) ||
		row.ActorSeat != "ceo" || !row.Run.IsZero() {
		t.Errorf("push %s's row names it built %q by %q (%s, seat %q, run %+v), want the engine's, by the "+
			"person's credential and their seat, with no run", l.PushID, row.Via, row.Actor, row.ActorKind,
			row.ActorSeat, row.Run)
	}
	var pack *gitlet.PackRef
	for _, r := range c.Rows().Packs {
		if r.PackID == l.PushID {
			ref := r.Ref()
			pack = &ref
		}
	}
	if pack == nil {
		t.Fatalf("push %s landed and no pack row names its bundle", l.PushID)
	}
	if n := adoptions(node, "adopted") - before; n != 1 || !got.Adopted {
		t.Errorf("the landing moved %s{outcome=adopted} by %d (adopted %v), want its one pack", metrics.CodeCacheAdoptions,
			n, got.Adopted)
	}
	// A round installs the pack row's pack with a GET unless the adoption put
	// it there, so the count below means something only after one has run.
	synced, err := node.Cache().Sync(c.Ctx, c.Rows().Repo.ID, gitlettest.Linearizable)
	if err != nil {
		t.Fatalf("sync %s on %s after the landing: %v", gitlettest.RepoName, node.ID(), err)
	}
	synced.Release()
	if n := backend.count(pack.Object.Name()); n != 0 {
		t.Errorf("the node that built push %s read its bundle back %d times at its next sync: an adopted "+
			"pack is not downloaded", l.PushID, n)
	}

	store, err := objstore.NewStore(backend)
	if err != nil {
		t.Fatal(err)
	}
	body, err := store.Open(c.Ctx, pack.Content())
	if err != nil {
		t.Fatalf("open the stored bundle: %v", err)
	}
	sw := gitlet.NewSealWriter(*pack)
	if _, err := io.Copy(sw, body); err != nil {
		t.Fatalf("read the stored bundle: %v", err)
	}
	_ = body.Close()
	seal, err := sw.Seal()
	if err == nil {
		err = gitlet.CheckBundle(*pack, seal)
	}
	if err != nil {
		t.Errorf("the stored bundle is not the one its row names: %v", err)
	}
}

// A BUNDLE ITS OWN SEAL REFUSES is never named by a row: the landing seals the
// bytes on their way to the store and holds the seal to the pack it laid out.
// The .rev's magic is damaged because only the seal reads it: every size still
// matches the object count (gitlet.PackRef.Check), and a shorter .rev would be
// refused by the proposal's check instead.
func TestALandingRefusesABundleItsSealRefuses(t *testing.T) {
	var lines logs
	c := newCase(t, gitlettest.SoloOptions{Logger: lines.logger()}, gitlettest.Settings())
	node := c.Fleet.Nodes()[0]
	p := gitlettest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	head := p.Commit(work, map[string]string{"README.md": "sealed\n"}, "sealed")
	l, checksum := scratchFor(t, c, p, "ceo", work, head)
	rev := filepath.Join(l.Scratch.Dir, "objects", "pack", "pack-"+checksum+".rev")
	if err := os.Chmod(rev, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(rev, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte{'X'}, 0); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	l.Commands = []gitlet.Command{{Ref: "refs/heads/landed", Old: gitlet.ZeroOID, New: head}}

	got, err := host.Land(c.Ctx, node.Host(), l)
	if err != nil {
		t.Fatalf("land a push whose reverse index is damaged: %v", err)
	}
	for _, cmd := range got.Commands {
		if cmd.Result != gitlet.ResultNG || cmd.Reason != gitlet.RefusalUnavailable ||
			!strings.Contains(cmd.Detail, "this node stored a pack Gitlet cannot serve: ") ||
			!strings.Contains(cmd.Detail, "the reverse index section begins") {
			t.Errorf("%s came to %s %s %q, want unavailable naming the reverse index the seal refused",
				cmd.Ref, cmd.Result, cmd.Reason, cmd.Detail)
		}
	}
	if got.Outcome != types.AuditFailed {
		t.Errorf("the landing came to %s, want failed", got.Outcome)
	}
	for _, r := range pushRows(t, c) {
		if r.PushID == l.PushID {
			t.Errorf("push %s, whose bundle its seal refused, has a row: %+v", l.PushID, r)
		}
	}
	if _, moved := c.Rows().Refs["refs/heads/landed"]; moved {
		t.Error("refs/heads/landed exists, and the only push that made it stored a bundle its seal refused")
	}
	if n := lines.count("code_pack_rejected", `"push_id":"`+l.PushID+`"`); n != 1 {
		t.Errorf("code_pack_rejected was logged %d times for push %s, want once", n, l.PushID)
	}
}

// THE LANDING JUDGES FORCE, as it does for a push git received: Forced is
// formed only by its own check of the pushed objects, and protected_force turns
// on it — so a non-ancestor update handed over with nothing judged is refused
// in the very sentence `git push --force` prints.
func TestALandingJudgesForceAsAPushDoes(t *testing.T) {
	c := newCase(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	node := c.Fleet.Nodes()[0]
	p := gitlettest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	first := p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")
	p.Git().Run(t, work, "commit", "-q", "--amend", "-m", "rewritten")
	rewritten := p.Head(work)

	l, _ := scratchFor(t, c, p, "ceo", work, rewritten)
	l.Commands = []gitlet.Command{{Ref: "refs/heads/main", Old: first, New: rewritten}}
	got, err := host.Land(c.Ctx, node.Host(), l)
	if err != nil {
		t.Fatalf("land a rewrite of main: %v", err)
	}
	const detail = "refuses a force-push (protection refs/heads/main: allow_force)"
	if cmd := got.Commands[0]; cmd.Result != gitlet.ResultNG || cmd.Reason != gitlet.RefusalProtectedForce ||
		cmd.Detail != detail {
		t.Errorf("a rewrite of main through the landing came to %s %s %q, want protected_force %q",
			cmd.Result, cmd.Reason, cmd.Detail, detail)
	}

	res := p.PushArgs(c.Ctx, work, []string{"--force"}, c.URL(0), "main")
	if reason, _ := res.Rejected("refs/heads/main"); !strings.Contains(reason, "(protected_force: "+detail+")") {
		t.Errorf("git push --force of the same rewrite printed %q, want protected_force: %s: %s", reason, detail, res)
	}
	if got := c.Rows().Refs["refs/heads/main"]; got != first {
		t.Errorf("main is at %s, want %s: neither rewrite may land", got, first)
	}
}

// A REPOSITORY THAT TAKES NO PUSH TAKES NONE FROM ANY BUILDER: the landing
// reads the status on its own synced row FIRST, so a bundle is never stored
// for an archived repository — the committer would refuse its proposal, and
// the object would be one no row names until the collector found it.
func TestALandingRefusesARepositoryThatTakesNoPush(t *testing.T) {
	settings := gitlettest.Settings()
	settings.Archived = true
	c := newCase(t, gitlettest.SoloOptions{}, settings)
	node := c.Fleet.Nodes()[0]
	p := gitlettest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	head := p.Commit(work, map[string]string{"README.md": "too late\n"}, "archived")
	l, _ := scratchFor(t, c, p, "ceo", work, head)
	if l.Synced.Repo.Status != gitlet.RepoStatusArchived {
		t.Fatalf("the synced row is %s, want archived", l.Synced.Repo.Status)
	}
	stored := len(objects(t, c))
	l.Commands = []gitlet.Command{{Ref: "refs/heads/landed", Old: gitlet.ZeroOID, New: head}}

	got, err := host.Land(c.Ctx, node.Host(), l)
	if err != nil {
		t.Fatalf("land a push to an archived repository: %v", err)
	}
	want := gitlet.ArchivedDetail(gitlettest.RepoName, gitlet.ManagedConfig)
	for _, cmd := range got.Commands {
		if cmd.Result != gitlet.ResultNG || cmd.Reason != gitlet.RefusalRepoArchived || cmd.Detail != want {
			t.Errorf("%s came to %s %s %q, want repo_archived %q", cmd.Ref, cmd.Result, cmd.Reason, cmd.Detail, want)
		}
	}
	if got.Outcome != types.AuditRefused || got.Refusal != host.AuditRefusal(gitlet.RefusalRepoArchived) {
		t.Errorf("the landing came to %s (%s), want refused repo_archived", got.Outcome, got.Refusal)
	}
	if n := len(objects(t, c)) - stored; n != 0 {
		t.Errorf("a push to an archived repository stored %d objects", n)
	}
}

// A PULL MIRROR REFUSES EVERY PUSH, as does a repository being imported: each
// moves only by what Gitlet fetches, so a person's push is refused at its
// advertisement and a builder's landing before its bundle is stored, in
// gitlet.MirroredDetail or gitlet.ImportingDetail. A fetch is served throughout.
//
// Mutation: drop the mirror or the importing arm from statusRefusal — the
// landing stores a bundle and the advertisement admits the push, red.
func TestAMirroredRepositoryRefusesEveryPush(t *testing.T) {
	c := newCase(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	node := c.Fleet.Nodes()[0]
	p := gitlettest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	first := p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")

	mirrored := gitlettest.Settings()
	mirrored.MirrorFrom = &gitlet.MirrorSource{URL: "https://forge.example.com/acme/web.git",
		Credential: "${FORGE_TOKEN}"}
	c.Fleet.Declare(t, gitlet.Declared{Name: gitlettest.RepoName, Settings: mirrored})
	// A new pull mirror imports first; its home ends the import once the
	// source's refs are in, as the proposal below does.
	if row := c.Rows().Repo; !row.Mirrored || row.Status != gitlet.RepoStatusImporting {
		t.Fatalf("the repository after its mirror_from is %s mirrored=%v, want a pull mirror importing from "+
			"its source", row.Status, row.Mirrored)
	}
	done := gitlet.ImportDone{RepoID: c.Rows().Repo.ID, Node: node.ID()}
	if res, err := c.Fleet.(*gitlettest.SoloFleet).Code(t).Import(c.Ctx, gitlet.ImportProposal{Done: &done}); err != nil ||
		res.Outcome != statelog.OutcomeApplied {
		t.Fatalf("end the pull mirror's import: %+v, %v", res, err)
	}
	if row := c.Rows().Repo; !row.Mirrored || row.Status != gitlet.RepoStatusActive {
		t.Fatalf("the repository after its mirror_from is %s mirrored=%v, want an active pull mirror",
			row.Status, row.Mirrored)
	}
	head := p.Commit(work, map[string]string{"README.md": "two\n"}, "two")
	want := gitlet.MirroredDetail(gitlettest.RepoName, gitlet.ManagedConfig)
	res := p.Push(c.Ctx, work, c.URL(0), "main")
	if res.Err == nil || !strings.Contains(res.Stderr+res.Stdout, want) {
		t.Errorf("a person's push to a pull mirror printed %s, want it refused saying %q", res, want)
	}

	stored := len(objects(t, c))
	l, _ := scratchFor(t, c, p, "ceo", work, head)
	if !l.Synced.Repo.Mirrored {
		t.Fatalf("the synced row says no pull mirror")
	}
	l.Commands = []gitlet.Command{{Ref: "refs/heads/landed", Old: gitlet.ZeroOID, New: head}}
	got, err := host.Land(c.Ctx, node.Host(), l)
	if err != nil {
		t.Fatalf("land a push to a pull mirror: %v", err)
	}
	for _, cmd := range got.Commands {
		if cmd.Result != gitlet.ResultNG || cmd.Reason != gitlet.RefusalRepoMirrored || cmd.Detail != want {
			t.Errorf("%s came to %s %s %q, want repo_mirrored %q", cmd.Ref, cmd.Result, cmd.Reason, cmd.Detail, want)
		}
	}
	if got.Outcome != types.AuditRefused || got.Refusal != host.AuditRefusal(gitlet.RefusalRepoMirrored) {
		t.Errorf("the landing came to %s (%s), want refused repo_mirrored", got.Outcome, got.Refusal)
	}
	if n := len(objects(t, c)) - stored; n != 0 {
		t.Errorf("a push to a pull mirror stored %d objects", n)
	}
	if got := p.LsRemote(c.URL(0), 2)["refs/heads/main"]; got != first {
		t.Errorf("a fetch of the pull mirror reads main at %q, want %s: a mirror is read as any repository", got,
			first)
	}

	// AN IMPORT: a repository code.repos declares filled from another host
	// is importing from its create, and refuses a push as a mirror does.
	imported := gitlettest.Settings()
	c.Fleet.Declare(t, gitlet.Declared{Name: "imported", Settings: imported,
		Import: &gitlet.ImportSource{URL: "https://forge.example.com/acme/api.git"}})
	stored = len(objects(t, c))
	res = p.Push(c.Ctx, work, node.URL()+"/git/imported", "main")
	if want := gitlet.ImportingDetail("imported"); res.Err == nil || !strings.Contains(res.Stderr+res.Stdout, want) {
		t.Errorf("a person's push to a repository being imported printed %s, want it refused saying %q", res,
			want)
	}
	// Refused before its bundle is stored: the committer would refuse it
	// too, but only after an unneeded bundle is in the object store.
	if n := len(objects(t, c)) - stored; n != 0 {
		t.Errorf("a push to a repository being imported stored %d objects", n)
	}
	if rows, err := c.Fleet.Rows(c.Ctx, "imported", gitlettest.Linearizable); err != nil {
		t.Fatalf("the rows of imported: %v", err)
	} else if rows.Repo.Status != gitlet.RepoStatusImporting || len(rows.Refs) != 0 {
		t.Errorf("imported is %s holding %d refs, want importing and empty", rows.Repo.Status, len(rows.Refs))
	}
}

// A LANDING THAT CANNOT FINISH IS THE PUSH'S, NEVER THE HOOK'S: a landing step
// failing after the bundle's upload fails the push in the engine's words and
// logs code_push_unchecked with the repository, the push and why — never
// code_hook_protocol, which says the hook spoke out of turn and nothing was
// stored.
func TestALandingThatCannotFinishIsLoggedAsTheLandings(t *testing.T) {
	var lines logs
	c := newCase(t, gitlettest.SoloOptions{Logger: lines.logger()}, gitlettest.Settings())
	p := gitlettest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"README.md": "unchecked\n"}, "unchecked")
	const why = "the step after the upload failed"
	c.Fleet.Faults().Arm(gitletfault.HostAfterUpload, gitletfault.Action{Once: true, Err: errors.New(why)})
	stored := len(objects(t, c))

	res := p.Push(c.Ctx, work, c.URL(0), "main")
	if res.OK("refs/heads/main") || !strings.Contains(res.Stderr, "the engine could not check this push: "+why) {
		t.Fatalf("a push whose landing failed after its upload: %s", res)
	}
	if n := len(objects(t, c)) - stored; n != 1 {
		t.Fatalf("the push stored %d objects, want its one bundle before the step that failed", n)
	}
	if n := lines.count("code_push_unchecked", `"level":"WARN"`, `"repo":"`+gitlettest.RepoName+`"`,
		`"push_id":"`, why); n != 1 {
		t.Errorf("code_push_unchecked was logged %d times naming the repository, the push and why, want once:\n%s",
			n, strings.Join(lines.at("WARN"), ""))
	}
	if n := lines.count("code_hook_protocol"); n != 0 {
		t.Errorf("a landing that failed after its upload was logged code_hook_protocol %d times: the hook "+
			"spoke in turn", n)
	}
	if _, moved := c.Rows().Refs["refs/heads/main"]; moved {
		t.Error("refs/heads/main exists, and the only push to it failed before its proposal")
	}
}

// A LANDING ITS CLIENT ABANDONED IS NO WARNING: a client hanging up mid-check
// ends the landing, logged at DEBUG; the handler records the request's end,
// and a WARN for every Ctrl-C would hide the one that matters.
func TestALandingItsClientAbandonedIsNoWarning(t *testing.T) {
	var lines logs
	timing := fast
	timing.Keepalive = 30 * time.Second // no line on receive-pack's stderr: nothing starts the response
	c := newCase(t, gitlettest.SoloOptions{Shape: gitlettest.Shape{Timing: timing}, Logger: lines.logger()},
		gitlettest.Settings())
	p := gitlettest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"README.md": "abandoned\n"}, "abandoned")
	reached := make(chan struct{})
	c.Fleet.Faults().Arm(gitletfault.HostAfterUpload, gitletfault.Action{Once: true, Block: make(chan struct{}),
		Reached: reached})
	ctx, cancel := context.WithCancel(c.Ctx)
	pushed := make(chan gitlettest.Pushed, 1)
	go func() { pushed <- p.Push(ctx, work, c.URL(0), "main") }()
	reachedFirst(t, reached, pushed)
	cancel()
	<-pushed
	eventually(t, "the abandoned landing's line", func() bool {
		return lines.count("code_push_unchecked", `"level":"DEBUG"`, `"repo":"`+gitlettest.RepoName+`"`) == 1
	})
	if n := lines.count("code_push_unchecked", `"level":"WARN"`); n != 0 {
		t.Errorf("a landing its client abandoned was logged code_push_unchecked at WARN %d times", n)
	}
}

// blobScans is how many of the landing's blob scans — the pack's cat-file and
// a found blob's path walk — the recorded git ran.
func blobScans(calls []gittest.Call) (scans, paths int) {
	for _, call := range calls {
		switch {
		case len(call.Args) > 1 && call.Args[0] == "cat-file" &&
			call.Args[1] == "--batch-check=%(objectname) %(objecttype) %(objectsize)":
			scans++
		case len(call.Args) > 1 && call.Args[0] == "rev-list" && call.Args[1] == "--objects":
			paths++
		}
	}
	return scans, paths
}

// rewriteWithABigBlob amends work's one commit to carry big.bin, 2 KiB: a
// rewrite of main, past a 1 KiB code.limits.max_blob_bytes.
func rewriteWithABigBlob(t *testing.T, p *gitlettest.Person, work string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(work, "big.bin"), []byte(strings.Repeat("x", 2<<10)), 0o644); err != nil {
		t.Fatal(err)
	}
	p.Git().Run(t, work, "add", "big.bin")
	p.Git().Run(t, work, "commit", "--quiet", "--amend", "-m", "rewritten with a big blob")
}

// A PUSH THE LANDING HAS ALREADY REFUSED WHOLE IS NEVER SCANNED FOR BLOBS: a
// force-push of a rewritten, protected main carrying an oversized blob is
// refused for its force, what the person must change, never as blob_too_large
// "(unreachable in this push)" from a scan with nothing left to decide.
func TestARefusedPushIsNeverScannedForBlobs(t *testing.T) {
	path, calls := gittest.Recording(t)
	c := newCase(t, gitlettest.SoloOptions{GitPath: path,
		Shape: gitlettest.Shape{Limits: gitlet.Limits{MaxBlobBytes: 1 << 10}}}, gitlettest.Settings())
	p := gitlettest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	first := p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")
	rewriteWithABigBlob(t, p, work)
	scans, paths := blobScans(calls())

	res := p.PushArgs(c.Ctx, work, []string{"--force"}, c.URL(0), "main")
	const detail = "refuses a force-push (protection refs/heads/main: allow_force)"
	if reason, _ := res.Rejected("refs/heads/main"); !strings.Contains(reason, "(protected_force: "+detail+")") {
		t.Errorf("a force-push of protected main carrying a blob past the limit printed %q, want protected_force: "+
			"%s", reason, res)
	}
	if s, w := blobScans(calls()); s != scans || w != paths {
		t.Errorf("a push refused whole before its blobs ran %d blob scans and %d path walks, want none",
			s-scans, w-paths)
	}
	if got := c.Rows().Refs["refs/heads/main"]; got != first {
		t.Errorf("main is at %s, want %s", got, first)
	}
}

// A BLOB PAST THE LIMIT REFUSES WHAT IS STILL UNDECIDED, BY ITS PATH: the
// pack carries every pushed ref's objects, so a branch the push creates is
// refused for a blob only a refused neighbour reaches — and the refusal names
// the path that neighbour reaches it by, never "(unreachable in this push)" —
// while the neighbour keeps the refusal that came first.
func TestABlobPastTheLimitKeepsAnEarlierRefusal(t *testing.T) {
	c := newCase(t, gitlettest.SoloOptions{Shape: gitlettest.Shape{Limits: gitlet.Limits{MaxBlobBytes: 1 << 10}}},
		gitlettest.Settings())
	p := gitlettest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	first := p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")
	rewriteWithABigBlob(t, p, work)

	res := p.PushArgs(c.Ctx, work, []string{"--force"}, c.URL(0), "main", first+":refs/heads/topic")
	const detail = "refuses a force-push (protection refs/heads/main: allow_force)"
	if reason, _ := res.Rejected("refs/heads/main"); !strings.Contains(reason, "(protected_force: "+detail+")") {
		t.Errorf("the force-push of protected main printed %q, want protected_force: %s", reason, res)
	}
	if reason, _ := res.Rejected("refs/heads/topic"); !strings.Contains(reason, "(blob_too_large: blob ") ||
		!strings.Contains(reason, "(big.bin) is 2.0 KiB, past code.limits.max_blob_bytes") {
		t.Errorf("topic, created beside a pack carrying a blob past the limit, printed %q, want blob_too_large "+
			"naming big.bin: %s", reason, res)
	}
	if _, moved := c.Rows().Refs["refs/heads/topic"]; moved {
		t.Error("refs/heads/topic exists, and its push's pack carried a blob past the limit")
	}
}
