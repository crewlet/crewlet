package codetest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/code/host"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
)

// F2: a push whose record is durable and not yet applied when the grace runs
// out is reported pending — never ok — and a re-run once it applied is up to
// date.
func aPushAppliedLateIsReportedPending(t *testing.T, newFleet NewFleet) {
	c := newCase(t, newFleet, Shape{}, Settings())
	p := NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	res, head, resume, err := pausedPush(c, p, work)
	if err != nil {
		resume()
		t.Fatal(err)
	}
	if err := okFromRows(c, res, head); err != nil {
		t.Error(err)
	}
	reason, rejected := res.Rejected("refs/heads/main")
	if !rejected || !strings.Contains(reason, "[remote rejected]") || !strings.Contains(reason, "(pending: durable at ") {
		t.Errorf("a push applied late printed %q, want [remote rejected] (pending: durable at …): %s", reason, res)
	}
	resume()
	// THE LATE PUSH APPLIED FIRST, as the re-run's claim needs: its
	// advertisement is a linearizable read, which waits for the late push's
	// record — the record before its barrier — and a resumed applier that
	// had not yet committed it when the read's budget ran out answered the
	// re-run 503 `behind`, on a machine slow to restart that applier. So the
	// paused data node's rows are awaited, never raced.
	if err := c.Fleet.Level(c.Ctx, c.Fleet.DataNodes()[0]); err != nil {
		t.Fatalf("the paused data node catching up once resumed: %v", err)
	}
	again := p.Push(c.Ctx, work, c.URL(0), "main")
	if again.Err != nil || !again.UpToDate("refs/heads/main") {
		t.Errorf("the push re-run once it applied: %s, want main up to date", again)
	}
	if got := c.Rows().Refs["refs/heads/main"]; got != head {
		t.Errorf("main is at %s, want the late push's %s", got, head)
	}
	assertConverged(t, c.Fleet)
}

// F4: two pushes of one ref from one base land once: one ok, one stale.
func twoPushesToOneRefLandOnce(t *testing.T, newFleet NewFleet) {
	c := newCase(t, newFleet, Shape{}, Settings())
	a, b := NewPerson(t, c.Fleet, "ceo"), NewPerson(t, c.Fleet, "dev")
	workA := a.Init()
	a.Commit(workA, map[string]string{"README.md": "base\n"}, "base")
	c.MustPush(a, workA, "main")
	workB := b.Clone(c.URL(0))
	a.Commit(workA, map[string]string{"a.txt": "a\n"}, "from a")
	fromB := b.Commit(workB, map[string]string{"b.txt": "b\n"}, "from b")

	held, reached := make(chan struct{}), make(chan struct{})
	c.Fleet.Faults().Arm(codefault.HostAfterSync, codefault.Action{Once: true, Block: held, Reached: reached})
	first := make(chan Pushed, 1)
	go func() { first <- a.Push(c.Ctx, workA, c.URL(0), "main") }()
	<-reached
	second := b.Push(c.Ctx, workB, c.URL(0), "main")
	close(held)
	resA := <-first
	if !second.OK("refs/heads/main") {
		t.Fatalf("the push that passed the held one: %s", second)
	}
	reason, rejected := resA.Rejected("refs/heads/main")
	if !rejected || !strings.Contains(reason, "(stale: refs/heads/main is at ") || !strings.Contains(reason, "fetch first") {
		t.Errorf("the held push printed %q, want (stale: refs/heads/main is at …; fetch first): %s", reason, resA)
	}
	if got := c.Rows().Refs["refs/heads/main"]; got != fromB {
		t.Errorf("main is at %s, want the push that landed's %s", got, fromB)
	}
	assertConverged(t, c.Fleet)
}

// F5: an upload that fails stores nothing and names the object store.
func aFailedUploadStoresNothing(t *testing.T, newFleet NewFleet) {
	c := newCase(t, newFleet, Shape{}, Settings())
	p := NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"big.bin": randomFile(64 << 10)}, "big")
	c.Fleet.Faults().Arm(codefault.HostUpload, codefault.Action{Once: true, At: 4096, Err: errInjected})
	res := p.Push(c.Ctx, work, c.URL(0), "main")
	reason, rejected := res.Rejected("refs/heads/main")
	if !rejected || !strings.Contains(reason, "(unavailable: object store unavailable: ") {
		t.Errorf("a push whose upload failed printed %q, want (unavailable: object store unavailable: …): %s",
			reason, res)
	}
	if names := objectNames(t, c.Fleet.Objects()); len(names) > 0 {
		t.Errorf("a failed upload left %v in the object store", names)
	}
	if pushes := pushRows(t, c); len(pushes) > 0 {
		t.Errorf("a failed upload left push rows %v", pushes)
	}
	assertConverged(t, c.Fleet)
}

// unknownEstate answers every proposal's outcome unknown after the committer
// ran it: the answer that was lost.
type unknownEstate struct{ host.Estate }

func (e unknownEstate) Propose(ctx context.Context, p code.Proposal) (code.ProposeResult, error) {
	res, err := e.Estate.Propose(ctx, p)
	if err != nil {
		return res, err
	}
	return code.ProposeResult{Outcome: statelog.OutcomeUnknown, OpID: res.OpID}, nil
}

// F6: a push whose outcome is unknown says so, and a fetch then shows what
// the rows did.
func anUnknownOutcomeIsSaidAndConverges(t *testing.T, newFleet NewFleet) {
	c := newCase(t, newFleet, Shape{WrapEstate: func(e host.Estate) host.Estate { return unknownEstate{e} }},
		Settings())
	p := NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	head := p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	res := p.Push(c.Ctx, work, c.URL(0), "main")
	reason, rejected := res.Rejected("refs/heads/main")
	if !rejected || !strings.Contains(reason, "(unknown: outcome unknown (op ") ||
		!strings.Contains(reason, "fetch, then re-run git push") {
		t.Errorf("an unknown outcome printed %q, want (unknown: outcome unknown (op …); fetch, then re-run git "+
			"push): %s", reason, res)
	}
	if got := p.LsRemote(c.URL(0), 2)["refs/heads/main"]; got != head {
		t.Errorf("after an unknown outcome a fetch shows main at %q, want the push's %s (it landed)", got, head)
	}
	assertConverged(t, c.Fleet)
}

// F7: a client that hangs up while its push is held leaves no scratch,
// nothing uploaded and no row.
func aClientThatHangsUpLeavesNoScratch(t *testing.T, newFleet NewFleet) {
	c := newCase(t, newFleet, Shape{}, Settings())
	p := NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"big.bin": randomFile(256 << 10)}, "big")
	held, reached := make(chan struct{}), make(chan struct{})
	c.Fleet.Faults().Arm(codefault.HostAfterScratch, codefault.Action{Once: true, Block: held, Reached: reached})
	ctx, hangUp := context.WithCancel(c.Ctx)
	done := make(chan Pushed, 1)
	go func() { done <- p.Push(ctx, work, c.URL(0), "main") }()
	<-reached
	hangUp()
	<-done
	close(held)
	for _, n := range c.Fleet.Nodes() {
		scratch := filepath.Join(n.CacheDir(), "scratch")
		if !within(10*time.Second, func() bool { return emptyDir(scratch) }) {
			t.Errorf("%s's scratch/ still holds %v after the client hung up", n.ID(), entries(scratch))
		}
	}
	if names := objectNames(t, c.Fleet.Objects()); len(names) > 0 {
		t.Errorf("a push whose client hung up stored %v", names)
	}
	if pushes := pushRows(t, c); len(pushes) > 0 {
		t.Errorf("a push whose client hung up left push rows %v", pushes)
	}
	assertConverged(t, c.Fleet)
}

// F8: an engine that drops the hook before its report fails the push — and
// what the rows say is what a person then sees.
func anEngineThatDiesInTheHookFailsThePush(t *testing.T, newFleet NewFleet) {
	c := newCase(t, newFleet, Shape{}, Settings())
	p := NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	head := p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.Fleet.Faults().Arm(codefault.HostHookClose, codefault.Action{Once: true, Err: errInjected})
	res := p.Push(c.Ctx, work, c.URL(0), "main")
	if res.Err == nil || res.OK("refs/heads/main") {
		t.Errorf("a push whose hook was dropped succeeded: %s", res)
	}
	if !strings.Contains(res.Stderr, "the engine went away during this push") {
		t.Errorf("a push whose hook was dropped printed %s, want the engine went away during this push", res)
	}
	if got := p.LsRemote(c.URL(0), 0)["refs/heads/main"]; got != head {
		t.Errorf("after the dropped hook main is at %q; the row applied the push at %s", got, head)
	}
	assertConverged(t, c.Fleet)
}

// F14: a push carrying a blob over the limit is refused whole, every ref, and
// stores nothing.
func aPushOverTheBlobLimitIsRefusedWhole(t *testing.T, newFleet NewFleet) {
	limits := code.Limits{MaxBlobBytes: 1 << 10}
	c := newCase(t, newFleet, Shape{Limits: limits}, Settings())
	p := NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"README.md": "small\n"}, "small")
	p.Git().Run(t, work, "branch", "big")
	p.Git().Run(t, work, "checkout", "--quiet", "big")
	p.Commit(work, map[string]string{"big.bin": randomFile(2 << 10)}, "big")
	res := p.Push(c.Ctx, work, c.URL(0), "main", "big")
	for _, ref := range []string{"refs/heads/main", "refs/heads/big"} {
		reason, rejected := res.Rejected(ref)
		if !rejected || !strings.Contains(reason, "(blob_too_large: blob ") || !strings.Contains(reason, "(big.bin)") {
			t.Errorf("%s printed %q, want (blob_too_large: blob … (big.bin) …): %s", ref, reason, res)
		}
	}
	if names := objectNames(t, c.Fleet.Objects()); len(names) > 0 {
		t.Errorf("a push over the blob limit stored %v", names)
	}
	assertConverged(t, c.Fleet)
}

// F15: every protection refuses by the name of the rule it applied.
func protectionRulesRefuseByName(t *testing.T, newFleet NewFleet) {
	settings := Settings()
	settings.Protections = []code.Protection{
		{Pattern: "refs/heads/main", DirectPush: code.DirectPersons},
		{Pattern: "refs/heads/release/*", DirectPush: code.DirectNone},
	}
	c := newCase(t, newFleet, Shape{}, settings)
	p := NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	p.Git().Run(t, work, "tag", "v1")
	c.MustPush(p, work, "main", "v1")

	p.Git().Run(t, work, "commit", "--quiet", "--amend", "-m", "one, rewritten")
	forced := p.PushArgs(c.Ctx, work, []string{"--force"}, c.URL(0), "main")
	wantRefused(t, forced, "refs/heads/main", "protected_force")

	p.Commit(work, map[string]string{"b.txt": "b\n"}, "two")
	p.Git().Run(t, work, "tag", "--force", "v1")
	res := p.PushArgs(c.Ctx, work, []string{"--force"}, c.URL(0),
		":refs/heads/main", "HEAD:refs/heads/release/1", "v1", "HEAD:refs/crewlet/x")
	wantRefused(t, res, "refs/heads/main", "protected_delete")
	wantRefused(t, res, "refs/heads/release/1", "protected_direct")
	wantRefused(t, res, "refs/tags/v1", "tag_moved")
	wantRefused(t, res, "refs/crewlet/x", "reserved_ref")
	assertConverged(t, c.Fleet)
}

// F25: a ref beside a directory of its name is refused, and every clone keeps
// fetching. Each client keeps a ref as a file, so with refs/heads/feature and
// refs/heads/feature/x both live, a clone that fetched one fails every later
// fetch ("cannot lock ref") — and nothing but the committer can see the two
// side by side, since the receive-pack that would refuse the pair runs no ref
// transaction here. A rename in ONE push — feature deleted, feature/v2 created
// — lands, as git's own transaction takes it, and a clone that prunes follows
// it.
func aRefBesideADirectoryOfItsNameIsRefused(t *testing.T, newFleet NewFleet) {
	c := newCase(t, newFleet, Shape{}, Settings())
	p := NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	head := p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main", "HEAD:refs/heads/feature", "HEAD:refs/heads/topic/a")
	clone := p.Clone(c.URL(0))

	res := p.Push(c.Ctx, work, c.URL(0), "HEAD:refs/heads/feature/x", "HEAD:refs/heads/topic")
	for ref, inTheWay := range map[string]string{
		"refs/heads/feature/x": "refs/heads/feature", "refs/heads/topic": "refs/heads/topic/a",
	} {
		wantRefused(t, res, ref, "ref_conflict")
		if reason, _ := res.Rejected(ref); !strings.Contains(reason, inTheWay+" is in the way") {
			t.Errorf("%s printed %q, want it to name %s in its way", ref, reason, inTheWay)
		}
	}
	rows := c.Rows()
	for _, ref := range []string{"refs/heads/feature/x", "refs/heads/topic"} {
		if got, ok := rows.Refs[ref]; ok {
			t.Errorf("%s is at %s after its push was refused", ref, got)
		}
	}
	if _, stderr, err := p.Git().Try(t, clone, nil, "fetch", "--quiet", "origin"); err != nil {
		t.Errorf("a clone's fetch after the refused pushes: %v\n%s", err, stderr)
	}

	renamed := p.Push(c.Ctx, work, c.URL(0), ":refs/heads/feature", "HEAD:refs/heads/feature/v2")
	if !renamed.OK("refs/heads/feature") || !renamed.OK("refs/heads/feature/v2") {
		t.Errorf("a rename of feature to feature/v2 in one push: %s", renamed)
	}
	if _, stderr, err := p.Git().Try(t, clone, nil, "fetch", "--quiet", "--prune", "origin"); err != nil {
		t.Errorf("a clone's fetch after the rename: %v\n%s", err, stderr)
	}
	got := strings.TrimSpace(p.Git().Run(t, clone, "rev-parse", "refs/remotes/origin/feature/v2"))
	if got != head {
		t.Errorf("the clone's origin/feature/v2 is %s, want %s", got, head)
	}
	assertConverged(t, c.Fleet)
}

// wantRefused fails t unless git printed ref rejected by the remote under
// code.
func wantRefused(t *testing.T, res Pushed, ref, code string) {
	t.Helper()
	reason, rejected := res.Rejected(ref)
	if !rejected || !strings.Contains(reason, "[remote rejected]") || !strings.Contains(reason, "("+code+": ") {
		t.Errorf("%s printed %q, want [remote rejected] (%s: …): %s", ref, reason, code, res)
	}
}

// F16: an atomic push lands whole or not at all.
func anAtomicPushLandsWholeOrNotAtAll(t *testing.T, newFleet NewFleet) {
	c := newCase(t, newFleet, Shape{}, Settings())
	x, y := NewPerson(t, c.Fleet, "ceo"), NewPerson(t, c.Fleet, "dev")
	workX := x.Init()
	base := x.Commit(workX, map[string]string{"README.md": "base\n"}, "base")
	c.MustPush(x, workX, "main", "main:refs/heads/topic")
	workY := y.Clone(c.URL(0))
	x.Git().Run(t, workX, "branch", "topic")
	x.Commit(workX, map[string]string{"x.txt": "x\n"}, "x on main")
	x.Git().Run(t, workX, "checkout", "--quiet", "topic")
	x.Commit(workX, map[string]string{"t.txt": "x\n"}, "x on topic")
	x.Git().Run(t, workX, "checkout", "--quiet", "main")

	// Y moves topic while X's push is held past its sync, so the committer
	// — not git — finds topic stale.
	moveTopic := func(n int) {
		y.Git().Run(t, workY, "checkout", "--quiet", "-B", "topic", "origin/topic")
		y.Commit(workY, map[string]string{"y.txt": fmt.Sprintf("y%d\n", n)}, "y on topic")
		if res := y.Push(c.Ctx, workY, c.URL(0), "topic"); !res.OK("refs/heads/topic") {
			t.Fatalf("y's push of topic: %s", res)
		}
		y.Git().Run(t, workY, "fetch", "--quiet", "origin")
	}
	push := func(opts []string, n int, refspecs ...string) Pushed {
		held, reached := make(chan struct{}), make(chan struct{})
		c.Fleet.Faults().Arm(codefault.HostAfterSync, codefault.Action{Once: true, Block: held, Reached: reached})
		done := make(chan Pushed, 1)
		go func() { done <- x.PushArgs(c.Ctx, workX, opts, c.URL(0), refspecs...) }()
		<-reached
		moveTopic(n)
		close(held)
		return <-done
	}

	atomic := push([]string{"--atomic"}, 1, "main", "topic")
	wantRefused(t, atomic, "refs/heads/topic", "stale")
	wantRefused(t, atomic, "refs/heads/main", "atomic")
	if got := c.Rows().Refs["refs/heads/main"]; got != base {
		t.Errorf("an atomic push refused in part moved main to %s", got)
	}

	// FORCED ON THE CLIENT, so git sends topic though the remote's is no
	// ancestor of it: what refuses it is the committer finding it moved
	// again, never git's own fast-forward check.
	plain := push(nil, 2, "main", "+topic")
	wantRefused(t, plain, "refs/heads/topic", "stale")
	if !plain.OK("refs/heads/main") {
		t.Errorf("without --atomic main did not land: %s", plain)
	}
	assertConverged(t, c.Fleet)
}

// F17: a token bound to nobody fetches, and never pushes.
func anUnboundTokenFetchesAndNeverPushes(t *testing.T, newFleet NewFleet) {
	c := newCase(t, newFleet, Shape{}, Settings())
	p := NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	head := p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")
	ci := NewPerson(t, c.Fleet, "")
	clone := ci.Clone(c.URL(0))
	if got := ci.Head(clone); got != head {
		t.Errorf("an unbound token's clone is at %s, want %s", got, head)
	}
	ci.Commit(clone, map[string]string{"ci.txt": "ci\n"}, "from ci")
	res := ci.Push(c.Ctx, clone, c.URL(0), "main")
	if res.Err == nil || !strings.Contains(res.Stderr, "fatal: remote error: this token is not bound to a person: ") {
		t.Errorf("an unbound token's push printed %s, want fatal: remote error: this token is not bound to a "+
			"person: …", res)
	}
	assertConverged(t, c.Fleet)
}

// F19: concurrent pushes share records — a committer holding one record takes
// everything queued behind it into the next.
func concurrentPushesShareRecords(t *testing.T, newFleet NewFleet) {
	const pushers = 50
	// A TRANSFER LANE FOR EVERY PUSH: a receive-pack holds its slot until
	// its record is answered, so a lane of fewer slots than pushes would
	// leave the rest waiting for a slot rather than queued at the
	// committer.
	c := newCase(t, newFleet, Shape{MaxTransfers: pushers + 8}, Settings())
	p := NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"README.md": "base\n"}, "base")
	c.MustPush(p, work, "main")
	for i := range pushers {
		p.Git().Run(t, work, "checkout", "--quiet", "-B", fmt.Sprintf("b%02d", i), "main")
		p.Commit(work, map[string]string{fmt.Sprintf("f%02d.txt", i): fmt.Sprintf("%d\n", i)}, fmt.Sprintf("b%02d", i))
	}
	uploads := c.Fleet.Faults().Hits(codefault.HostAfterUpload)
	held, reached := make(chan struct{}), make(chan struct{})
	c.Fleet.Faults().Arm(codefault.CommitterAfterAppend, codefault.Action{Once: true, Block: held, Reached: reached})
	results := make([]Pushed, pushers)
	var wg sync.WaitGroup
	for i := range pushers {
		wg.Go(func() { results[i] = p.Push(c.Ctx, work, c.URL(0), fmt.Sprintf("b%02d", i)) })
	}
	<-reached
	// EVERY OTHER PUSH IS PROPOSING, queued behind the held record, before
	// the record is let go: deterministic, where a race against the
	// committer would commit singletons on a fast machine and be right.
	if !within(time.Minute, func() bool { return c.Fleet.Faults().Hits(codefault.HostAfterUpload)-uploads == pushers }) {
		t.Errorf("only %d of %d pushes uploaded", c.Fleet.Faults().Hits(codefault.HostAfterUpload)-uploads, pushers)
	}
	close(held)
	wg.Wait()
	for i, res := range results {
		if ref := fmt.Sprintf("refs/heads/b%02d", i); !res.OK(ref) {
			t.Errorf("push %d: %s", i, res)
		}
	}
	positions := map[statelog.Position]bool{}
	for _, row := range pushRows(t, c) {
		positions[row.Position] = true
	}
	if len(positions) >= pushers+1 {
		t.Errorf("%d pushes landed in %d records: none shared one", pushers, len(positions)-1)
	}
	assertConverged(t, c.Fleet)
}

// objectNames is every name the backend lists.
func objectNames(t *testing.T, b objstore.Backend) []string {
	t.Helper()
	var names []string
	if err := b.List(t.Context(), func(i objstore.Info) error {
		names = append(names, i.Name)
		return nil
	}); err != nil {
		t.Fatalf("list the object store: %v", err)
	}
	return names
}

// pushRows is every push row of RepoName, read linearizable.
func pushRows(t *testing.T, c *Case) []code.PushRow {
	t.Helper()
	var rows []code.PushRow
	q := code.ReadQuery{Asker: Reader, Kind: code.ReadPushes, Repo: RepoName, Limit: 200, Fresh: Linearizable}
	for {
		a, err := c.Fleet.Nodes()[0].Read(c.Ctx, q)
		if err != nil {
			t.Fatalf("read the pushes: %v", err)
		}
		rows = append(rows, a.Pushes...)
		if a.Next == "" {
			return rows
		}
		q.Cursor = a.Next
	}
}

// emptyDir reports whether dir holds nothing (or does not exist).
func emptyDir(dir string) bool { return len(entries(dir)) == 0 }

// entries is dir's entries' names.
func entries(dir string) []string {
	list, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	names := make([]string, len(list))
	for i, e := range list {
		names[i] = e.Name()
	}
	return names
}
