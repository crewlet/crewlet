package host_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/code/host"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// pendingAt is a proposal's answer that its record is durable at seq and
// applied nowhere yet.
func pendingAt(seq uint64) code.ProposeResult {
	return code.ProposeResult{Outcome: statelog.OutcomePending, OpID: "op-pending",
		Position: statelog.Position{Stream: code.Domain{}.Stream().Name, Seq: seq}}
}

// ADR-0036: git is told ok for a ref ONLY from the push's own applied row — a
// pending answer is waited on and its row read, and never reported as ok.
func TestAnOkIsAnsweredOnlyFromAnAppliedRow(t *testing.T) {
	t.Run("the row is found", func(t *testing.T) {
		e := &recording{propose: func(ctx context.Context, inner host.Estate, p code.Proposal) (code.ProposeResult, error) {
			res, err := inner.Propose(ctx, p)
			if err != nil || res.Outcome != statelog.OutcomeApplied {
				return res, err
			}
			return code.ProposeResult{Outcome: statelog.OutcomePending, Position: res.Position, OpID: res.OpID}, nil
		}}
		c := newCase(t, codetest.SoloOptions{Shape: codetest.Shape{Timing: fast, WrapEstate: e.wrap}},
			codetest.Settings())
		p := codetest.NewPerson(t, c.Fleet, "ceo")
		work := p.Init()
		head := p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
		if res := p.Push(c.Ctx, work, c.URL(0), "main"); !res.OK("refs/heads/main") {
			t.Fatalf("a pending push whose row is then found: %s", res)
		}
		if len(e.seen("outcome")) == 0 {
			t.Error("the push was answered ok without its row being read")
		}
		if got := p.LsRemote(c.URL(0), 0)["refs/heads/main"]; got != head {
			t.Errorf("ls-remote shows main at %q, want %s", got, head)
		}
	})
	for _, tc := range []struct {
		name    string
		seen    uint64 // the position the outcome read answers at
		reason  string
		against uint64 // the record's position
	}{
		{name: "no row until the grace", seen: 5, against: 1 << 20, reason: "(pending: durable at "},
		{name: "no row past its record", seen: 1<<20 + 1, against: 1 << 20, reason: "(not_applied: "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &recording{
				propose: func(context.Context, host.Estate, code.Proposal) (code.ProposeResult, error) {
					return pendingAt(tc.against), nil
				},
				outcome: func(context.Context, host.Estate, code.OutcomeQuery) (code.PushOutcome, error) {
					return code.PushOutcome{Found: false,
						Position: statelog.Position{Stream: code.Domain{}.Stream().Name, Seq: tc.seen}}, nil
				},
			}
			c := newCase(t, codetest.SoloOptions{Shape: codetest.Shape{Timing: fast, WrapEstate: e.wrap}},
				codetest.Settings())
			p := codetest.NewPerson(t, c.Fleet, "ceo")
			work := p.Init()
			p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
			res := p.Push(c.Ctx, work, c.URL(0), "main")
			reason, rejected := res.Rejected("refs/heads/main")
			if !rejected || !strings.Contains(reason, tc.reason) {
				t.Errorf("printed %q, want %s…: %s", reason, tc.reason, res)
			}
			if got := p.LsRemote(c.URL(0), 0)["refs/heads/main"]; got != "" {
				t.Errorf("main moved to %s, and no row was ever applied", got)
			}
		})
	}
}

// A pending push is waited on for at most the grace — read again at a spacing
// that doubles to its ceiling — and then said to be pending.
func TestAPendingPushWaitsAtMostTheStallGrace(t *testing.T) {
	timing := fast
	timing.PendingGrace = 300 * time.Millisecond
	var proposed time.Time
	e := &recording{
		propose: func(context.Context, host.Estate, code.Proposal) (code.ProposeResult, error) {
			proposed = time.Now()
			return pendingAt(1 << 20), nil
		},
		outcome: func(context.Context, host.Estate, code.OutcomeQuery) (code.PushOutcome, error) {
			return code.PushOutcome{}, fmt.Errorf("refused: no data node answered")
		},
	}
	c := newCase(t, codetest.SoloOptions{Shape: codetest.Shape{Timing: timing, WrapEstate: e.wrap}},
		codetest.Settings())
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	res := p.Push(c.Ctx, work, c.URL(0), "main")
	if reason, _ := res.Rejected("refs/heads/main"); !strings.Contains(reason, "(pending: durable at ") {
		t.Errorf("a push whose outcome never resolved printed %q: %s", reason, res)
	}
	reads := e.seen("outcome")
	if len(reads) < 2 {
		t.Fatalf("the outcome was read %d times in a grace of %v", len(reads), timing.PendingGrace)
	}
	const margin = 300 * time.Millisecond
	if last := reads[len(reads)-1].at.Sub(proposed); last > timing.PendingGrace+timing.PendingMax+margin {
		t.Errorf("the last outcome read came %v after the proposal, past the grace %v, a step %v and %v",
			last, timing.PendingGrace, timing.PendingMax, margin)
	}
	// THE LAST STEP IS CUT TO THE GRACE: a read is never put off past it, so
	// only the steps before it are held to the doubling.
	wait := timing.PendingFirst
	for i := 1; i < len(reads)-1; i++ {
		if gap := reads[i].at.Sub(reads[i-1].at); gap < wait-time.Millisecond {
			t.Errorf("outcome read %d came %v after the one before, under the step %v", i, gap, wait)
		}
		wait = min(2*wait, timing.PendingMax)
	}
	if last := reads[len(reads)-1].at.Sub(proposed); last < timing.PendingGrace-10*time.Millisecond {
		t.Errorf("the last outcome read came %v after the proposal, before the grace %v ran out", last,
			timing.PendingGrace)
	}
}

// A token bound to nobody fetches, and every push it makes is
// refused — at the advertisement, and at a POST that skipped it — and
// recorded.
func TestAnUnboundTokenCannotPush(t *testing.T) {
	c := newCase(t, codetest.SoloOptions{}, codetest.Settings())
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	head := p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")

	ci := codetest.NewPerson(t, c.Fleet, "")
	clone := ci.Clone(c.URL(0))
	ci.Commit(clone, map[string]string{"ci.txt": "ci\n"}, "from ci")
	res := ci.Push(c.Ctx, clone, c.URL(0), "main")
	if res.Err == nil || !strings.Contains(res.Stderr, "fatal: remote error: this token is not bound to a person: ") {
		t.Errorf("an unbound token's push: %s", res)
	}
	// A POST THAT NEVER ASKED FOR THE ADVERTISEMENT: a deletion carries no
	// pack, so the command section is the whole body.
	body := pkt(head+" "+gitrefZero+" refs/heads/main\x00report-status\n") + "0000"
	resp, report := post(t, c, 0, "", "git-receive-pack", "", body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(report, "ng refs/heads/main this token is not bound to a person") {
		t.Errorf("an unbound token's hand-built push answered %d %q", resp.StatusCode, report)
	}
	if got := c.Rows().Refs["refs/heads/main"]; got != head {
		t.Errorf("main is at %s after refused pushes, want %s", got, head)
	}
	var unbound int
	for _, a := range c.Fleet.Audited() {
		if a.Outcome == types.AuditRefused && a.Refusal == string(host.RefusalUnbound) && a.OperatorID == "ci" &&
			a.ActorSeat == "" && a.Transport == types.TransportGit {
			unbound++
		}
	}
	if unbound != 2 {
		t.Errorf("%d unbound refusals were recorded, want 2: %+v", unbound, c.Fleet.Audited())
	}
}

// gitrefZero is the id git sends for a ref that does not exist.
const gitrefZero = "0000000000000000000000000000000000000000"

// Every push is one record on the receiving node, as the person
// whose token pushed it; a fetch is none.
func TestEveryPushIsAuditedAsItsPerson(t *testing.T) {
	c := newCase(t, codetest.SoloOptions{}, codetest.Settings())
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")
	p.Git().Run(t, work, "commit", "--quiet", "--amend", "-m", "rewritten")
	forced := p.PushArgs(c.Ctx, work, []string{"--force"}, c.URL(0), "main")
	if _, rejected := forced.Rejected("refs/heads/main"); !rejected {
		t.Fatalf("a force-push to protected main: %s", forced)
	}
	before := len(c.Fleet.Audited())
	p.Clone(c.URL(0))
	audited := c.Fleet.Audited()
	if len(audited) != before {
		t.Errorf("a clone was recorded: %+v", audited[before:])
	}
	if len(audited) != 2 {
		t.Fatalf("two pushes were recorded as %d: %+v", len(audited), audited)
	}
	rows := pushRows(t, c)
	if len(rows) != 1 {
		t.Fatalf("one applied push left %d rows", len(rows))
	}
	applied, refused := audited[0], audited[1]
	want := types.OperatorActed{OperatorID: "ceo", ActorSeat: "ceo", Transport: types.TransportGit,
		Tool: host.AuditTool, Repo: codetest.RepoName, Outcome: types.AuditApplied,
		Position: rows[0].Position.String(), RequestID: applied.RequestID}
	if applied != want {
		t.Errorf("the applied push was recorded as %+v, want %+v", applied, want)
	}
	if id, err := uuid.Parse(applied.RequestID); err != nil || id.Version() != 7 {
		t.Errorf("the record's request id %q is not a UUIDv7 (%v)", applied.RequestID, err)
	}
	if refused.Outcome != types.AuditRefused || refused.Refusal != string(code.RefusalProtectedForce) ||
		refused.ActorSeat != "ceo" || !refused.Failed {
		t.Errorf("the refused force-push was recorded as %+v", refused)
	}
}

// ADR-0036: receive-pack never runs in a cache — every push, its
// advertisement included, is received in a scratch repository.
func TestReceivePackNeverRunsInACache(t *testing.T) {
	path, calls := gittest.Recording(t)
	c := newCase(t, codetest.SoloOptions{GitPath: path}, codetest.Settings())
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main", "main:refs/heads/topic")
	p.Commit(work, map[string]string{"README.md": "two\n"}, "two")
	c.MustPush(p, work, "main")
	p.Git().Run(t, work, "commit", "--quiet", "--amend", "-m", "rewritten")
	if res := p.PushArgs(c.Ctx, work, []string{"--force"}, c.URL(0), "main"); res.OK("refs/heads/main") {
		t.Fatalf("a force-push to protected main: %s", res)
	}
	clone := p.Clone(c.URL(0))
	p.Git().Run(t, clone, "fetch", "--quiet", c.URL(0))

	dir := c.Fleet.Nodes()[0].CacheDir()
	scratch, repos := filepath.Join(dir, "scratch")+"/", filepath.Join(dir, "repos")+"/"
	received := 0
	for _, call := range calls() {
		if len(call.Args) == 0 || call.Args[0] != "receive-pack" {
			continue
		}
		received++
		where := call.GitDir
		if where == "" {
			where = call.Args[len(call.Args)-1]
		}
		if strings.HasPrefix(where, repos) || !strings.HasPrefix(where, scratch) {
			t.Errorf("receive-pack ran in %s %v, outside scratch/", where, call.Args)
		}
	}
	if received < 4 {
		t.Errorf("only %d receive-pack processes ran for three pushes and their advertisements", received)
	}
}

// A push carrying a blob past code.limits.max_blob_bytes stores nothing and
// proposes nothing.
func TestAPushOverTheBlobLimitStoresNothing(t *testing.T) {
	e := &recording{}
	c := newCase(t, codetest.SoloOptions{Shape: codetest.Shape{Limits: code.Limits{MaxBlobBytes: 1 << 10},
		WrapEstate: e.wrap}}, codetest.Settings())
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"big.bin": strings.Repeat("x", 2<<10)}, "big")
	res := p.Push(c.Ctx, work, c.URL(0), "main")
	reason, _ := res.Rejected("refs/heads/main")
	if !strings.Contains(reason, "[remote rejected]") || !strings.Contains(reason, "(blob_too_large: blob ") ||
		!strings.Contains(reason, "(big.bin) is 2.0 KiB, past code.limits.max_blob_bytes") {
		t.Errorf("a push over the blob limit printed %q: %s", reason, res)
	}
	if names := objects(t, c); len(names) > 0 {
		t.Errorf("a push over the blob limit stored %v", names)
	}
	if n := len(e.seen("propose")); n > 0 {
		t.Errorf("a push over the blob limit was proposed %d times", n)
	}
}

// objects is every name RepoName's fleet's object store lists.
func objects(t *testing.T, c *codetest.Case) []string {
	t.Helper()
	var names []string
	if err := c.Fleet.Objects().List(c.Ctx, func(i objstore.Info) error {
		names = append(names, i.Name)
		return nil
	}); err != nil {
		t.Fatalf("list the object store: %v", err)
	}
	return names
}

// ADR-0027: a push the committer refuses has uploaded its pack, which no row
// names, and the collector — the one deleter — removes it; a push refused at
// the receiver uploaded nothing.
func TestARefusedPushLeavesNoObjectARowNames(t *testing.T) {
	c := newCase(t, codetest.SoloOptions{}, codetest.Settings())
	a, b := codetest.NewPerson(t, c.Fleet, "ceo"), codetest.NewPerson(t, c.Fleet, "dev")
	workA := a.Init()
	a.Commit(workA, map[string]string{"README.md": "base\n"}, "base")
	c.MustPush(a, workA, "main")
	workB := b.Clone(c.URL(0))
	a.Commit(workA, map[string]string{"a.txt": "a\n"}, "from a")
	b.Commit(workB, map[string]string{"b.txt": "b\n"}, "from b")
	held, reached := make(chan struct{}), make(chan struct{})
	c.Fleet.Faults().Arm(codefault.HostAfterSync, codefault.Action{Once: true, Block: held, Reached: reached})
	stale := make(chan codetest.Pushed, 1)
	go func() { stale <- a.Push(c.Ctx, workA, c.URL(0), "main") }()
	<-reached
	c.MustPush(b, workB, "main")
	close(held)
	if reason, _ := (<-stale).Rejected("refs/heads/main"); !strings.Contains(reason, "(stale: ") {
		t.Fatalf("the held push printed %q, want stale", reason)
	}
	named := map[string]bool{}
	for _, pack := range c.Rows().Packs {
		named[pack.Object.Name()] = true
	}
	var orphans []string
	for _, name := range objects(t, c) {
		if !named[name] {
			orphans = append(orphans, name)
		}
	}
	if len(orphans) != 1 {
		t.Fatalf("the stale push left %d objects no row names (%v), want its one upload", len(orphans), orphans)
	}
	report := c.Fleet.Collect(t)
	for _, name := range objects(t, c) {
		if !named[name] {
			t.Errorf("the collector left %s, which no row names (%+v)", name, report)
		}
	}

	stored := len(objects(t, c))
	if res := b.Push(c.Ctx, workB, c.URL(0), ":refs/heads/main"); res.OK("refs/heads/main") {
		t.Fatalf("a deletion of protected main: %s", res)
	}
	if got := len(objects(t, c)); got != stored {
		t.Errorf("a push refused at the receiver stored %d objects", got-stored)
	}
}

// archivedAfterTheAdvertisement answers every resolve after the first as
// archived: a repository archived between a push's advertisement and its
// POST, which the receiver refuses before receive-pack runs.
func archivedAfterTheAdvertisement(e *recording) {
	e.resolve = func(ctx context.Context, inner host.Estate, q code.ResolveQuery) (code.RepoRef, error) {
		ref, err := inner.Resolve(ctx, q)
		if err == nil && len(e.seen("resolve")) > 1 {
			ref.Status = code.RepoStatusArchived
		}
		return ref, err
	}
}

// D3: a push refused before receive-pack runs is answered only once its body
// has been read to its end — a report written while git is still uploading is
// lost, and git prints `RPC failed` instead of the reason.
func TestAnEarlyRefusalReadsTheWholeBody(t *testing.T) {
	e := &recording{}
	archivedAfterTheAdvertisement(e)
	c := newCase(t, codetest.SoloOptions{Shape: codetest.Shape{WrapEstate: e.wrap}}, codetest.Settings())
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"big.bin": randomText(5 << 20)}, "big")
	res := p.Push(c.Ctx, work, c.URL(0), "main")
	reason, _ := res.Rejected("refs/heads/main")
	if !strings.Contains(reason, "[remote rejected]") || !strings.Contains(reason, "repo_archived") ||
		!strings.Contains(reason, "archived") || strings.Contains(res.Stderr, "RPC failed") {
		t.Errorf("a 5 MiB push into an archived repository printed %q: %s", reason, res)
	}
}

// randomText is n incompressible bytes.
func randomText(n int) string {
	raw := make([]byte, n)
	_, _ = rand.Read(raw)
	return string(raw)
}

// git's probe before a push past its post buffer is answered 200, empty, and
// runs nothing.
func TestTheProbeIsAnsweredEmpty(t *testing.T) {
	path, calls := gittest.Recording(t)
	c := newCase(t, codetest.SoloOptions{GitPath: path}, codetest.Settings())
	resp, body := post(t, c, 0, "ceo", "git-receive-pack", "", "0000")
	if resp.StatusCode != http.StatusOK || body != "" ||
		resp.Header.Get("Content-Type") != "application/x-git-receive-pack-result" {
		t.Errorf("the probe answered %d %q %q", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
	for _, call := range calls() {
		if len(call.Args) > 0 && call.Args[0] == "receive-pack" {
			t.Errorf("the probe ran receive-pack %v", call.Args)
		}
	}
}

// A refusal the receiver synthesises is what git prints, per ref; and what
// git refuses itself — push options, never advertised — git refuses.
func TestASynthesizedReportIsWhatGitPrints(t *testing.T) {
	c := newCase(t, codetest.SoloOptions{}, codetest.Settings())
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	head := p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	var refs strings.Builder
	for i := range code.MaxCommandsPerPush + 1 {
		fmt.Fprintf(&refs, "create refs/heads/b%04d %s\n", i, head)
	}
	if _, stderr, err := p.Git().Try(t, work, strings.NewReader(refs.String()), "update-ref", "--stdin"); err != nil {
		t.Fatalf("make the branches: %v\n%s", err, stderr)
	}
	res := p.Push(c.Ctx, work, c.URL(0), "refs/heads/b*:refs/heads/b*")
	if reason, _ := res.Rejected("refs/heads/b0000"); !strings.Contains(reason, "[remote rejected]") ||
		!strings.Contains(reason, fmt.Sprintf("this push names more than %d refs", code.MaxCommandsPerPush)) {
		t.Errorf("a push of %d refs printed %q for the first", code.MaxCommandsPerPush+1, reason)
	}
	for ref := range res.Refs {
		if res.OK(ref) {
			t.Errorf("a push past the command limit printed %s ok", ref)
		}
	}
	options := p.PushArgs(c.Ctx, work, []string{"-o", "x"}, c.URL(0), "main")
	if options.Err == nil || !strings.Contains(options.Stderr, "does not support push options") {
		t.Errorf("a push with an option: %s", options)
	}
}

// A push waiting on its record keeps the connection alive with a line on the
// client's stderr naming the step it waits on.
func TestAKeepaliveFlowsWhileAPushWaits(t *testing.T) {
	timing := fast
	timing.Keepalive = 50 * time.Millisecond
	e := &recording{propose: func(ctx context.Context, inner host.Estate, p code.Proposal) (code.ProposeResult, error) {
		time.Sleep(400 * time.Millisecond)
		return inner.Propose(ctx, p)
	}}
	c := newCase(t, codetest.SoloOptions{Shape: codetest.Shape{Timing: timing, WrapEstate: e.wrap}},
		codetest.Settings())
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	res := p.Push(c.Ctx, work, c.URL(0), "main")
	if !res.OK("refs/heads/main") {
		t.Fatalf("a push held 400 ms at its proposal: %s", res)
	}
	if n := strings.Count(res.Stderr, "remote: crewlet: committing the push ("); n < 2 {
		t.Errorf("a push held 400 ms at a keepalive of 50 ms printed %d keepalives: %s", n, res)
	}
}

// An atomic push is never answered in part: a ref the receiver refuses
// refuses every other, nothing is uploaded and nothing proposed.
func TestAnAtomicPushIsNeverAnsweredMixed(t *testing.T) {
	settings := codetest.Settings()
	settings.Protections = append(settings.Protections,
		code.Protection{Pattern: "refs/heads/release/*", DirectPush: code.DirectNone})
	e := &recording{}
	c := newCase(t, codetest.SoloOptions{Shape: codetest.Shape{WrapEstate: e.wrap}}, settings)
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")
	stored, proposed := len(objects(t, c)), len(e.seen("propose"))
	p.Commit(work, map[string]string{"README.md": "two\n"}, "two")
	res := p.PushArgs(c.Ctx, work, []string{"--atomic"}, c.URL(0), "main", "HEAD:refs/heads/release/1")
	for ref, code := range map[string]string{"refs/heads/release/1": "protected_direct", "refs/heads/main": "atomic"} {
		if reason, _ := res.Rejected(ref); !strings.Contains(reason, "("+code+": ") {
			t.Errorf("%s printed %q, want (%s: …): %s", ref, reason, code, res)
		}
	}
	if got := len(objects(t, c)); got != stored {
		t.Errorf("a refused atomic push stored %d objects", got-stored)
	}
	if got := len(e.seen("propose")); got != proposed {
		t.Errorf("a refused atomic push was proposed")
	}

	// AND ONE THE SESSION REFUSES IN PART: a forced update of protected main
	// is known only once its objects are — in the hook — and the new branch
	// beside it goes with it.
	p.Git().Run(t, work, "reset", "--quiet", "--hard", "HEAD~1")
	p.Git().Run(t, work, "commit", "--quiet", "--amend", "-m", "one, rewritten")
	res = p.PushArgs(c.Ctx, work, []string{"--atomic", "--force"}, c.URL(0), "main", "HEAD:refs/heads/topic")
	for ref, code := range map[string]string{"refs/heads/main": "protected_force", "refs/heads/topic": "atomic"} {
		if reason, _ := res.Rejected(ref); !strings.Contains(reason, "("+code+": ") {
			t.Errorf("%s printed %q, want (%s: …): %s", ref, reason, code, res)
		}
	}
	if got := len(objects(t, c)); got != stored {
		t.Errorf("an atomic push the session refused in part stored %d objects", got-stored)
	}
	if got := len(e.seen("propose")); got != proposed {
		t.Errorf("an atomic push the session refused in part was proposed")
	}
}

// O5: the force check runs over a push's branch updates in parallel, as wide as
// the read lane's free slots, and still names exactly the updates that are
// forced — every other update of the push lands.
func TestEveryForcedUpdateOfAWidePushIsFound(t *testing.T) {
	settings := codetest.Settings()
	settings.Protections = []code.Protection{{Pattern: "refs/heads/**", DirectPush: code.DirectPersons}}
	c := newCase(t, codetest.SoloOptions{MaxProcesses: 4}, settings)
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	base := p.Commit(work, map[string]string{"README.md": "base\n"}, "base")
	const branches = 24
	var specs []string
	for i := range branches {
		name := fmt.Sprintf("b%02d", i)
		p.Git().Run(t, work, "checkout", "--quiet", "-B", name, base)
		p.Commit(work, map[string]string{"f.txt": name + "\n"}, name)
		specs = append(specs, name)
	}
	c.MustPush(p, work, specs...)
	forced := map[string]bool{}
	for i, name := range specs {
		p.Git().Run(t, work, "checkout", "--quiet", name)
		if i%3 == 0 {
			// REWRITTEN: no longer a descendant of what the remote holds.
			p.Git().Run(t, work, "commit", "--quiet", "--amend", "-m", name+", rewritten")
			forced["refs/heads/"+name] = true
		} else {
			p.Commit(work, map[string]string{"g.txt": name + "\n"}, name+" again")
		}
	}
	res := p.PushArgs(c.Ctx, work, []string{"--force"}, c.URL(0), specs...)
	for _, name := range specs {
		ref := "refs/heads/" + name
		reason, rejected := res.Rejected(ref)
		switch {
		case forced[ref] && (!rejected || !strings.Contains(reason, "(protected_force: ")):
			t.Errorf("forced %s printed %q, want (protected_force: …)", ref, reason)
		case !forced[ref] && !res.OK(ref):
			t.Errorf("fast-forward %s printed %q, want it through", ref, reason)
		}
	}
}

// O11: a receive-pack's loan of read cores is repaid once its index-pack is
// done — when its hook reaches the relay — never held through the push's
// upload: with the push held inside its object-store put, the read lane lends
// nothing and the gauge says so.
//
// The lane is read until it is whole rather than once, because the push lists
// the commits it introduces BESIDE its upload (introduceAndStore), and that
// listing holds a read slot of its own — a run, never a loan — until it ends by
// itself, which on a loaded machine is after the upload reaches its put. A
// loan that outlived its index-pack would last as long as the receive-pack,
// which is held in its hook for as long as the upload is, so the lane never
// comes whole and the case fails at the deadline.
func TestALoanIsRepaidBeforeThePushsUpload(t *testing.T) {
	c := newCase(t, codetest.SoloOptions{MaxProcesses: 4}, codetest.Settings())
	node := c.Fleet.Nodes()[0]
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"big.bin": randomText(256 << 10)}, "big")
	held, reached := make(chan struct{}), make(chan struct{})
	c.Fleet.Faults().Arm(codefault.HostUpload, codefault.Action{Once: true, Block: held, Reached: reached})
	done := make(chan codetest.Pushed, 1)
	go func() { done <- p.Push(c.Ctx, work, c.URL(0), "main") }()
	<-reached
	slots := node.Pool().Slots(gitproc.ClassRead)
	free := node.Pool().Free(gitproc.ClassRead)
	for deadline := time.Now().Add(5 * time.Second); free != slots && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
		free = node.Pool().Free(gitproc.ClassRead)
	}
	lent := -1.0
	for _, s := range node.Metrics().Read() {
		if s.Name == metrics.CodeGitLentCores {
			lent = s.Value
		}
	}
	close(held)
	if res := <-done; !res.OK("refs/heads/main") {
		t.Fatalf("the push held at its upload: %s", res)
	}
	if free != slots {
		t.Errorf("with a push held in its upload the read lane had %d of %d slots free after five seconds: "+
			"a loan outlived its index-pack", free, slots)
	}
	if lent != 0 {
		t.Errorf("with a push held in its upload %s reads %v, want 0", metrics.CodeGitLentCores, lent)
	}
}

// A PUSH WHOSE CLIENT HANGS UP AFTER ITS PROPOSAL is recorded as its session
// concluded it — the committer runs a batch under its own lifetime, so the
// push may have landed — never as failed; and a client that hung up is no code
// log failing, so nothing says it is.
func TestAPushAbandonedAfterItsProposalIsRecordedAsItsSession(t *testing.T) {
	var (
		reached = make(chan struct{})
		once    sync.Once
	)
	e := &recording{propose: func(ctx context.Context, inner host.Estate, p code.Proposal) (code.ProposeResult, error) {
		res, err := inner.Propose(ctx, p)
		once.Do(func() { close(reached) })
		<-ctx.Done()
		if err == nil {
			err = context.Cause(ctx)
		}
		return res, err
	}}
	timing := fast
	timing.Keepalive = 30 * time.Second // no line on receive-pack's stderr: nothing starts the response
	var l logs
	c := newCase(t, codetest.SoloOptions{Shape: codetest.Shape{Timing: timing, WrapEstate: e.wrap},
		Logger: l.logger()}, codetest.Settings())
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	head := p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	ctx, cancel := context.WithCancel(c.Ctx)
	pushed := make(chan codetest.Pushed, 1)
	go func() { pushed <- p.Push(ctx, work, c.URL(0), "main") }()
	<-reached
	cancel()
	<-pushed
	eventually(t, "the push's record", func() bool { return len(c.Fleet.Audited()) == 1 })
	if got := c.Fleet.Audited()[0].Outcome; got != types.AuditUnknown && got != types.AuditApplied {
		t.Errorf("a push whose client hung up after its proposal landed was recorded %q, want unknown or "+
			"applied", got)
	}
	if got := p.LsRemote(c.URL(0), 0)["refs/heads/main"]; got != head {
		t.Errorf("main is at %q after the abandoned push's batch, want %s", got, head)
	}
	if n := l.count("code_git_unavailable"); n != 0 {
		t.Errorf("a client that hung up was logged %d times as the code log failing", n)
	}
}

// A RECEIVE-PACK THAT FAILS BY ITSELF AFTER WRITING ITS REPORT ends the
// response as git ended it: the client reads the report — every ref it
// names — rather than a connection cut short, which git prints as `RPC
// failed`; the failure is logged as the push's.
func TestAReceivePackFailingAfterItsReportEndsTheResponse(t *testing.T) {
	// The stand-in runs the real receive-pack of a push — never its
	// advertisement's — and then fails, having written all it writes.
	fake := gittest.Wrap(t, `case " $* " in
*" --advertise-refs "*) ;;
" receive-pack "*) "$real_git" "$@"; exit 3 ;;
esac`)
	var l logs
	c := newCase(t, codetest.SoloOptions{GitPath: fake, Logger: l.logger()}, codetest.Settings())
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	head := p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	res := p.Push(c.Ctx, work, c.URL(0), "main")
	if !res.OK("refs/heads/main") || strings.Contains(res.Stderr, "RPC failed") {
		t.Errorf("a push whose receive-pack failed after its report: %s", res)
	}
	if got := p.LsRemote(c.URL(0), 0)["refs/heads/main"]; got != head {
		t.Errorf("main is at %q, want %s", got, head)
	}
	if n := l.count("code_push_failed"); n != 1 {
		t.Errorf("a receive-pack that failed after its report was logged code_push_failed %d times, want 1", n)
	}
}

// A PUSH WHOSE BODY FAILED BEFORE GIT WROTE A BYTE IS TOLD SO — 400 naming the
// body, as a fetch's is — never the empty 200 a handler that returns without
// writing answers, which git cannot tell from a push that did nothing. The
// failure is one the CLIENT is still there to read: a gzip stream whose
// checksum is wrong, found only at its end, from a client that asked for no
// report (so receive-pack, its input failed, writes nothing of its own). A
// body cut short is not this case: net/http ends the request's context on any
// failed read of the connection, and a push whose client is gone is answered
// as that. It is recorded failed, and lands nothing.
func TestAPushWhoseBodyFailedIsToldSo(t *testing.T) {
	c := newCase(t, codetest.SoloOptions{}, codetest.Settings())
	var zipped bytes.Buffer
	z := gzip.NewWriter(&zipped)
	if _, err := io.WriteString(z, pkt(gitrefZero+" "+oidN(1)+" refs/heads/main\n")+"0000"+
		"PACK\x00\x00\x00\x02\x00\x00\x00\x01"); err != nil {
		t.Fatal(err)
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	body := zipped.Bytes()
	body[len(body)-8] ^= 0xff // the trailer's CRC-32
	node := c.Fleet.Nodes()[0]
	req, err := http.NewRequestWithContext(c.Ctx, http.MethodPost,
		node.URL()+"/git/"+codetest.RepoName+"/git-receive-pack", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-git-receive-pack-request")
	req.Header.Set("Content-Encoding", "gzip")
	req.SetBasicAuth("crewlet", c.Fleet.Token(t, "ceo"))
	resp, err := node.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	answer, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest ||
		!strings.Contains(string(answer), "the push's body could not be read: ") {
		t.Errorf("a push whose gzip stream failed its checksum answered %d %q", resp.StatusCode, answer)
	}
	eventually(t, "the push's record", func() bool { return len(c.Fleet.Audited()) == 1 })
	if got := c.Fleet.Audited()[0].Outcome; got != types.AuditFailed {
		t.Errorf("a push whose body failed was recorded %q, want failed", got)
	}
	if _, ok := c.Rows().Refs["refs/heads/main"]; ok {
		t.Error("a push whose body failed created main")
	}
}

// A REFUSAL THE RECEIVER WRITES FITS THE PROPOSAL it travels in, whatever
// the ref and the rule: a ref of the longest a push may name, under a literal
// protection of that same ref, is refused protected_direct by itself — and
// the rest of the push lands, where a sentence past the proposal's bound had
// the committer refuse the whole push as the engine's own bug.
func TestALongProtectedRefIsRefusedAlone(t *testing.T) {
	frozen := "refs/heads/frozen/" + strings.Repeat("x", code.MaxRefNameBytes-len("refs/heads/frozen/"))
	settings := codetest.Settings()
	settings.Protections = append([]code.Protection{{Pattern: frozen, DirectPush: code.DirectNone}},
		settings.Protections...)
	var l logs
	c := newCase(t, codetest.SoloOptions{Logger: l.logger()}, settings)
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	head := p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	res := p.Push(c.Ctx, work, c.URL(0), "main", "HEAD:"+frozen)
	if !res.OK("refs/heads/main") {
		t.Errorf("main, pushed beside a protected ref of %d bytes: %s", len(frozen), res)
	}
	if reason, rejected := res.Rejected(frozen); !rejected ||
		!strings.Contains(reason, "(protected_direct: takes no direct push (protection "+frozen+": direct_push none))") {
		t.Errorf("the protected ref printed %q, want its rule named: %s", reason, res)
	}
	if got := p.LsRemote(c.URL(0), 0)["refs/heads/main"]; got != head {
		t.Errorf("main is at %q, want %s", got, head)
	}
	if n := l.count("code_push_invalid"); n != 0 {
		t.Errorf("the push's proposal was refused as invalid %d times", n)
	}
}

// THE HOOK ASKS THE PROTECTIONS AGAIN ONLY WHAT FORCED CAN CHANGE: a forced
// update, and a command the handler did not judge as it stands. Every other
// command's verdict is the handler's, over the same rules and the same ids — a
// maximal push under the most rules a repository may carry asked them all
// twice.
func TestTheHookAsksTheProtectionsOnlyWhatForcedChanges(t *testing.T) {
	var rules []code.Protection
	for i := range code.MaxProtectionsPerRepo - 1 {
		rules = append(rules, code.Protection{Pattern: fmt.Sprintf("refs/heads/r%02d/**", i),
			DirectPush: code.DirectPersons})
	}
	rules = append(rules, code.Protection{Pattern: "refs/heads/z*", DirectPush: code.DirectPersons})
	var cmds, judged []code.Command
	for i := range code.MaxCommandsPerPush {
		c := code.Command{Ref: fmt.Sprintf("refs/heads/z%04d", i), Old: oidN(i), New: oidN(i + code.MaxCommandsPerPush)}
		judged = append(judged, c)
		c.Forced = i%250 == 0
		cmds = append(cmds, c)
	}
	// One the handler never judged: a command git handed the hook as it
	// stands — a protection asked about it all the same.
	cmds[1].New = oidN(3 * code.MaxCommandsPerPush)
	asked, err := host.ProtectAfterForce(cmds, rules, judged)
	if err != nil {
		t.Fatal(err)
	}
	if asked != 5 {
		t.Errorf("the hook asked the protections about %d of %d commands, want the 4 forced and the 1 the "+
			"handler did not judge", asked, len(cmds))
	}
	for i, c := range cmds {
		forced := i%250 == 0
		if refused := c.Result == code.ResultNG; refused != forced ||
			(forced && (c.Reason != code.RefusalProtectedForce ||
				c.Detail != "refuses a force-push (protection refs/heads/z*: allow_force)")) {
			t.Errorf("command %d (forced %v) came to %q %q %q", i, forced, c.Result, c.Reason, c.Detail)
		}
	}
}

// A PUSH'S UPLOAD DOES NOT WAIT FOR THE LISTING of the commits it introduces:
// the listing refuses nothing — it only fills the proposal — so the two
// longest steps of a push's own work overlap rather than add. The stand-in git
// holds the listing until the upload has been seen to start, which a push
// that stored only after listing never reaches.
func TestAPushsUploadOverlapsItsListing(t *testing.T) {
	release := filepath.Join(t.TempDir(), "release")
	fake := gittest.Wrap(t, `case " $* " in
" rev-list "*" --alternate-refs "*) while [ ! -e `+shellWord(release)+` ]; do sleep 0.02; done ;;
esac`)
	c := newCase(t, codetest.SoloOptions{GitPath: fake}, codetest.Settings())
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	reached := make(chan struct{})
	c.Fleet.Faults().Arm(codefault.HostUpload, codefault.Action{Once: true, Reached: reached})
	pushed := make(chan codetest.Pushed, 1)
	go func() { pushed <- p.Push(c.Ctx, work, c.URL(0), "main") }()
	select {
	case <-reached:
	case <-time.After(10 * time.Second):
		t.Error("the push's upload did not start while its listing was held: it waited for the listing")
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if res := <-pushed; !res.OK("refs/heads/main") {
		t.Errorf("the push, its listing released: %s", res)
	}
}

// oidN is a distinct object id for n.
func oidN(n int) string { return fmt.Sprintf("%040x", n+1) }

// shellWord is s as one POSIX shell word.
func shellWord(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
