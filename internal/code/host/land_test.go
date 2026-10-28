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

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codefault"
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
// pack's. The landing names who pushes, when, and nothing it judged — and that
// the engine built it (code.ViaEngine), as the builder it stands for will.
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
		Principal: code.Principal{Kind: code.PrincipalOperator, OperatorID: person, Seat: person},
		Via:       code.ViaEngine}, checksum
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
// node a download of what it holds. And its row says how it was built — as
// its builder named it, the engine — and who pushed: the person's credential
// as the actor beside the seat they hold, with no run.
//
// Mutation: propose the relay's way whatever the landing names (Via: ViaGit in
// land's proposal) — the row reads git.
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
	if row.Via != code.ViaEngine || row.Actor != "ceo" || row.ActorKind != string(code.PrincipalOperator) ||
		row.ActorSeat != "ceo" || !row.Run.IsZero() {
		t.Errorf("push %s's row names it built %q by %q (%s, seat %q, run %+v), want the engine's, by the "+
			"person's credential and their seat, with no run", l.PushID, row.Via, row.Actor, row.ActorKind,
			row.ActorSeat, row.Run)
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
	want := code.ArchivedDetail(codetest.RepoName, code.ManagedConfig)
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

// A LANDING THAT CANNOT FINISH IS THE PUSH'S, NEVER THE HOOK'S: a step of the
// landing that fails — here the one after the bundle's upload, so the bundle IS
// stored — fails the push in the engine's words and is logged
// code_push_unchecked, naming the repository, the push and why. Logged as
// code_hook_protocol, which says the hook spoke out of turn and nothing was
// stored, it sent an operator to the hook for a git deadline on this node and
// told them a stored bundle did not exist.
func TestALandingThatCannotFinishIsLoggedAsTheLandings(t *testing.T) {
	var lines logs
	c := newCase(t, codetest.SoloOptions{Logger: lines.logger()}, codetest.Settings())
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"README.md": "unchecked\n"}, "unchecked")
	const why = "the step after the upload failed"
	c.Fleet.Faults().Arm(codefault.HostAfterUpload, codefault.Action{Once: true, Err: errors.New(why)})
	stored := len(objects(t, c))

	res := p.Push(c.Ctx, work, c.URL(0), "main")
	if res.OK("refs/heads/main") || !strings.Contains(res.Stderr, "the engine could not check this push: "+why) {
		t.Fatalf("a push whose landing failed after its upload: %s", res)
	}
	if n := len(objects(t, c)) - stored; n != 1 {
		t.Fatalf("the push stored %d objects, want its one bundle before the step that failed", n)
	}
	if n := lines.count("code_push_unchecked", `"level":"WARN"`, `"repo":"`+codetest.RepoName+`"`,
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

// A LANDING ITS CLIENT ABANDONED IS NO WARNING: the push's request ending under
// it — the client hanging up mid-check — ends the landing too, and that is
// logged at DEBUG, since the handler records the request's own end for what it
// was and a WARN for every person who presses Ctrl-C hides the one that
// matters.
func TestALandingItsClientAbandonedIsNoWarning(t *testing.T) {
	var lines logs
	timing := fast
	timing.Keepalive = 30 * time.Second // no line on receive-pack's stderr: nothing starts the response
	c := newCase(t, codetest.SoloOptions{Shape: codetest.Shape{Timing: timing}, Logger: lines.logger()},
		codetest.Settings())
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"README.md": "abandoned\n"}, "abandoned")
	reached := make(chan struct{})
	c.Fleet.Faults().Arm(codefault.HostAfterUpload, codefault.Action{Once: true, Block: make(chan struct{}),
		Reached: reached})
	ctx, cancel := context.WithCancel(c.Ctx)
	pushed := make(chan codetest.Pushed, 1)
	go func() { pushed <- p.Push(ctx, work, c.URL(0), "main") }()
	reachedFirst(t, reached, pushed)
	cancel()
	<-pushed
	eventually(t, "the abandoned landing's line", func() bool {
		return lines.count("code_push_unchecked", `"level":"DEBUG"`, `"repo":"`+codetest.RepoName+`"`) == 1
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
func rewriteWithABigBlob(t *testing.T, p *codetest.Person, work string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(work, "big.bin"), []byte(strings.Repeat("x", 2<<10)), 0o644); err != nil {
		t.Fatal(err)
	}
	p.Git().Run(t, work, "add", "big.bin")
	p.Git().Run(t, work, "commit", "--quiet", "--amend", "-m", "rewritten with a big blob")
}

// A PUSH THE LANDING HAS ALREADY REFUSED WHOLE IS NEVER SCANNED FOR BLOBS, and
// a blob past the limit never overwrites a refusal that came first: a
// `git push --force` of a rewritten, protected main carrying an oversized
// blob is refused for its force — what the person has to change — never as
// blob_too_large at a path "(unreachable in this push)", which is what a scan
// of a push with nothing left to decide walked to, through a read slot and
// every object of its pack, for an answer that overwrote the right one.
func TestARefusedPushIsNeverScannedForBlobs(t *testing.T) {
	path, calls := gittest.Recording(t)
	c := newCase(t, codetest.SoloOptions{GitPath: path,
		Shape: codetest.Shape{Limits: code.Limits{MaxBlobBytes: 1 << 10}}}, codetest.Settings())
	p := codetest.NewPerson(t, c.Fleet, "ceo")
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
	c := newCase(t, codetest.SoloOptions{Shape: codetest.Shape{Limits: code.Limits{MaxBlobBytes: 1 << 10}}},
		codetest.Settings())
	p := codetest.NewPerson(t, c.Fleet, "ceo")
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
