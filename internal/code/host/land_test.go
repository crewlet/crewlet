package host_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/code/host"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/memobj"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE ONE LANDING, ASKED BY A BUILDER THAT IS NOT THE RELAY: each case forms a
// push the way an engine-built write will — its own commits, packed into a
// scratch by the person's git, with no receive-pack and no hook — and hands it
// to the landing, which must take it exactly as it takes a push git received.

// scratchFor is a landing of RepoName on the case's first node as a builder
// other than the relay forms one: the node's cache synced through the
// barrier, a scratch made over it under a fresh push id, and — revs given —
// ONE pack of revs written into it by the person's own git, with the .idx
// (version 2) and the .rev a push's bundle carries; its checksum is the
// pack's. The landing names who pushes, when, and nothing it judged.
func scratchFor(t *testing.T, c *codetest.Case, p *codetest.Person, person, work string,
	revs ...string) (l host.TestLanding, checksum string) {

	t.Helper()
	node := c.Fleet.Nodes()[0]
	synced, err := node.Cache().Sync(c.Ctx, c.Rows().Repo.ID, codetest.Linearizable)
	if err != nil {
		t.Fatalf("sync %s on %s: %v", codetest.RepoName, node.ID(), err)
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
		Principal: code.Principal{Kind: code.PrincipalOperator, OperatorID: person, Seat: person}}, checksum
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
func adoptions(node codetest.Node, outcome string) uint64 {
	var n uint64
	for _, s := range node.Metrics().Read() {
		if s.Name == metrics.CodeCacheAdoptions && s.Attrs["outcome"] == outcome {
			n += s.Total
		}
	}
	return n
}

// A PUSH THE RELAY NEVER SAW LANDS as a push git received does: applied from
// its own row, the commits it introduces on that row, its bundle stored whole
// and sealed as every installing cache holds it — and its pack ADOPTED by the
// node that built it, so that node's next sync does not download it back. (It
// would where a round that read the push's row had already begun the pack's GET
// before the adoption, or where a link fails — cache.Cache.Adopt — and neither
// happens here: nothing else syncs the repository and the disk takes links.)
// The adoption is the landing's, not the relay's: kept where only the hook's
// session reached it, a push built any other way would land and cost its own
// node a download of what it holds.
func TestAPushTheRelayNeverSawLands(t *testing.T) {
	backend := &gets{Backend: memobj.New(), of: map[string]int{}}
	c := newCase(t, codetest.SoloOptions{Objects: backend}, codetest.Settings())
	node := c.Fleet.Nodes()[0]
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	head := p.Commit(work, map[string]string{"README.md": "built by the engine\n"}, "landed")
	l, _ := scratchFor(t, c, p, "ceo", work, head)
	l.Commands = []code.Command{{Ref: "refs/heads/landed", Old: code.ZeroOID, New: head}}
	before := adoptions(node, "adopted")

	got, err := host.Land(c.Ctx, node.Host(), l)
	if err != nil {
		t.Fatalf("land a push the relay never saw: %v", err)
	}
	if got.Outcome != types.AuditApplied || len(got.Commands) != 1 || got.Commands[0].Result != code.ResultOK {
		t.Fatalf("the landing came to %s with %+v, want applied and ok", got.Outcome, got.Commands)
	}
	var row *code.PushRow
	for _, r := range pushRows(t, c) {
		if r.PushID == l.PushID {
			row = &r
		}
	}
	if row == nil || !slices.Contains(row.Introduced, head) {
		t.Fatalf("push %s's row is %+v, want one listing %s among the commits it introduced", l.PushID, row, head)
	}
	var pack *code.PackRef
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
	// The node's next round reads the push's pack row — and installs the
	// pack it names with a GET unless the adoption already put it there, so
	// the count below says something only once a round has run.
	synced, err := node.Cache().Sync(c.Ctx, c.Rows().Repo.ID, codetest.Linearizable)
	if err != nil {
		t.Fatalf("sync %s on %s after the landing: %v", codetest.RepoName, node.ID(), err)
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
	sw := code.NewSealWriter(*pack)
	if _, err := io.Copy(sw, body); err != nil {
		t.Fatalf("read the stored bundle: %v", err)
	}
	_ = body.Close()
	seal, err := sw.Seal()
	if err == nil {
		err = code.CheckBundle(*pack, seal)
	}
	if err != nil {
		t.Errorf("the stored bundle is not the one its row names: %v", err)
	}
}

// A BUNDLE ITS OWN SEAL REFUSES is never named by a row: the landing seals the
// bytes on their way to the store and holds the seal to the pack it laid out,
// the one rule every installing cache holds a bundle to. The reverse index's
// magic is the case because only the seal reads it — every size still matches
// what the object count says (code.PackRef.Check), so the proposal's own check
// passes it, and a shorter .rev would be refused there instead.
func TestALandingRefusesABundleItsSealRefuses(t *testing.T) {
	var lines logs
	c := newCase(t, codetest.SoloOptions{Logger: lines.logger()}, codetest.Settings())
	node := c.Fleet.Nodes()[0]
	p := codetest.NewPerson(t, c.Fleet, "ceo")
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
	l.Commands = []code.Command{{Ref: "refs/heads/landed", Old: code.ZeroOID, New: head}}

	got, err := host.Land(c.Ctx, node.Host(), l)
	if err != nil {
		t.Fatalf("land a push whose reverse index is damaged: %v", err)
	}
	for _, cmd := range got.Commands {
		if cmd.Result != code.ResultNG || cmd.Reason != code.RefusalUnavailable ||
			!strings.Contains(cmd.Detail, "this node stored a pack the code host cannot serve: ") ||
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
	c := newCase(t, codetest.SoloOptions{}, codetest.Settings())
	node := c.Fleet.Nodes()[0]
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	first := p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")
	p.Git().Run(t, work, "commit", "-q", "--amend", "-m", "rewritten")
	rewritten := p.Head(work)

	l, _ := scratchFor(t, c, p, "ceo", work, rewritten)
	l.Commands = []code.Command{{Ref: "refs/heads/main", Old: first, New: rewritten}}
	got, err := host.Land(c.Ctx, node.Host(), l)
	if err != nil {
		t.Fatalf("land a rewrite of main: %v", err)
	}
	const detail = "refuses a force-push (protection refs/heads/main: allow_force)"
	if cmd := got.Commands[0]; cmd.Result != code.ResultNG || cmd.Reason != code.RefusalProtectedForce ||
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
	settings := codetest.Settings()
	settings.Archived = true
	c := newCase(t, codetest.SoloOptions{}, settings)
	node := c.Fleet.Nodes()[0]
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	head := p.Commit(work, map[string]string{"README.md": "too late\n"}, "archived")
	l, _ := scratchFor(t, c, p, "ceo", work, head)
	if l.Synced.Repo.Status != code.RepoStatusArchived {
		t.Fatalf("the synced row is %s, want archived", l.Synced.Repo.Status)
	}
	stored := len(objects(t, c))
	l.Commands = []code.Command{{Ref: "refs/heads/landed", Old: code.ZeroOID, New: head}}

	got, err := host.Land(c.Ctx, node.Host(), l)
	if err != nil {
		t.Fatalf("land a push to an archived repository: %v", err)
	}
	want := code.ArchivedDetail(codetest.RepoName)
	for _, cmd := range got.Commands {
		if cmd.Result != code.ResultNG || cmd.Reason != code.RefusalRepoArchived || cmd.Detail != want {
			t.Errorf("%s came to %s %s %q, want repo_archived %q", cmd.Ref, cmd.Result, cmd.Reason, cmd.Detail, want)
		}
	}
	if got.Outcome != types.AuditRefused || got.Refusal != host.AuditRefusal(code.RefusalRepoArchived) {
		t.Errorf("the landing came to %s (%s), want refused repo_archived", got.Outcome, got.Refusal)
	}
	if n := len(objects(t, c)) - stored; n != 0 {
		t.Errorf("a push to an archived repository stored %d objects", n)
	}
}
