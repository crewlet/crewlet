package host_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/memobj"
	"github.com/crewlet/crewlet/internal/objstore/s3obj"
	"github.com/crewlet/crewlet/internal/objstore/s3obj/s3fake"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// pendingAt is a proposal's answer that its record is durable at seq and
// applied nowhere yet.
func pendingAt(seq uint64) code.ProposeResult {
	return code.ProposeResult{Outcome: statelog.OutcomePending, OpID: "op-pending",
		Position: statelog.Position{Stream: code.Domain{}.Stream().Name, Seq: seq}}
}

// ADR-0054: git is told ok for a ref ONLY from the push's own applied row — a
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
	// A POST THAT NEVER ASKED FOR THE ADVERTISEMENT: deletions carry no
	// pack, so the command section is the whole body. Every command is
	// refused by the token's grant (ADR-0058) — the domain's not_permitted,
	// with the sentence the advertisement gives.
	body := pkt(head+" "+gitrefZero+" refs/heads/main\x00report-status\n") +
		pkt(head+" "+gitrefZero+" refs/heads/feature\n") + "0000"
	resp, report := post(t, c, 0, "", "git-receive-pack", "", body)
	for _, ref := range []string{"refs/heads/main", "refs/heads/feature"} {
		if resp.StatusCode != http.StatusOK ||
			!strings.Contains(report, "ng "+ref+" not_permitted: this token is not bound to a person") {
			t.Errorf("an unbound token's hand-built push answered %d %q for %s", resp.StatusCode, report, ref)
		}
	}
	if got := c.Rows().Refs["refs/heads/main"]; got != head {
		t.Errorf("main is at %s after refused pushes, want %s", got, head)
	}
	var unbound int
	for _, a := range c.Fleet.Audited() {
		// RECORDED AS THE BARE CREDENTIAL, ci's token acting under its own
		// login with no seat — never as a seat's holder.
		if a.Outcome == types.AuditRefused && a.Refusal == string(host.RefusalUnbound) &&
			a.OperatorID == iam.TokenLogin("ci") && a.ActorName == iam.TokenLogin("ci") &&
			a.ActorKind == string(iam.ActorOperator) && a.Transport == types.TransportGit {
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
// whose token pushed it; a fetch is none. The push's row names the same
// person, and git as what built it.
//
// Mutation: the relay's landing names no way (Via unset in session.go) — the
// proposal is refused naming via, and the push lands no row.
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
	// AND ITS ROW AS THE SAME PERSON'S, received by git: the credential the
	// actor, the seat beside it, and no run — a person's push names none.
	if r := rows[0]; r.Via != code.ViaGit || r.Actor != iam.TokenLogin("ceo") ||
		r.ActorKind != string(code.PrincipalOperator) || r.ActorSeat != "ceo" || !r.Run.IsZero() {
		t.Errorf("the applied push's row names it %q by %q (%s, seat %q, run %+v), want git's, by ceo's "+
			"token and their seat, with no run", r.Via, r.Actor, r.ActorKind, r.ActorSeat, r.Run)
	}
	applied, refused := audited[0], audited[1]
	// AS CEO: the seat as the author, kind human, and ceo's token — its login,
	// iam.ActorFor's operator id — as the credential.
	want := types.OperatorActed{ActorName: "ceo", ActorKind: string(iam.ActorHuman),
		OperatorID: iam.TokenLogin("ceo"), Transport: types.TransportGit,
		Tool: host.AuditTool, Repo: codetest.RepoName, Outcome: types.AuditApplied,
		Position: rows[0].Position.String(), RequestID: applied.RequestID}
	if applied != want {
		t.Errorf("the applied push was recorded as %+v, want %+v", applied, want)
	}
	if id, err := uuid.Parse(applied.RequestID); err != nil || id.Version() != 7 {
		t.Errorf("the record's request id %q is not a UUIDv7 (%v)", applied.RequestID, err)
	}
	if refused.Outcome != types.AuditRefused || refused.Refusal != string(code.RefusalProtectedForce) ||
		refused.ActorName != "ceo" || refused.ActorKind != string(iam.ActorHuman) || !refused.Failed {
		t.Errorf("the refused force-push was recorded as %+v", refused)
	}
}

// EVERY PUSH THE NODE ADMITS IS ONE RECORD, HOWEVER IT ENDS — and one push in
// its repository's heat, counted at that record. A push answered 503 at its
// sync, as a git node in a minority of the code log answers every push, at
// its advertisement or at its POST; one a fault stopped on its way to its
// run; one whose body this node cannot decode: each is recorded failed, as
// the person who pushed it. And one declaring more than a push may send is
// recorded refused, as that person's too: judged after its admission, not
// before it. Each step's failure is confirmed where it was meant to be — the
// code log's line for its request, the fault's point reached, the status the
// client was told — so a step that failed elsewhere proves nothing.
//
// Mutation: answer any one of these exits without its record — the request's
// record ended as none of a push's ends there (pushAudit.none), or the
// declared length judged before admission again — and its step finds no
// record and the repository's heat a push short.
func TestEveryPushTheNodeAdmitsIsRecordedHoweverItEnds(t *testing.T) {
	var failing atomic.Bool
	e := &recording{state: func(ctx context.Context, inner host.Estate, q code.StateQuery) (code.RepoSnapshot, error) {
		if failing.Load() && q.Fresh.Level == statelog.ReadLinearizable {
			return code.RepoSnapshot{}, errNoQuorum
		}
		return inner.RepoState(ctx, q)
	}}
	var l logs
	c := newCase(t, codetest.SoloOptions{Logger: l.logger(), Shape: codetest.Shape{WrapEstate: e.wrap}},
		codetest.Settings())
	node := c.Fleet.Nodes()[0]
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main", "main:refs/heads/topic")
	rows := c.Rows()
	id, topic := rows.Repo.ID, rows.Refs["refs/heads/topic"]
	p.Commit(work, map[string]string{"README.md": "two\n"}, "two")

	// gitPush is the person's own push of main, which fails.
	gitPush := func(t *testing.T) {
		t.Helper()
		if res := p.Push(c.Ctx, work, c.URL(0), "main"); res.Err == nil {
			t.Fatalf("the push went through: %s", res)
		}
	}
	// unavailable is a push whose sync's read at the log's head fails, and
	// the code log's line its request logged, which says it failed there.
	unavailable := func(push func(t *testing.T), path string) func(t *testing.T) {
		return func(t *testing.T) {
			t.Helper()
			lines := l.count("code_git_unavailable", path)
			failing.Store(true)
			push(t)
			failing.Store(false)
			if l.count("code_git_unavailable", path) != lines+1 {
				t.Errorf("the push's request logged no code_git_unavailable for %s: %q", path,
					l.lines("code_git_unavailable"))
			}
		}
	}
	errFault := errors.New("a fault this case injected")
	// fault is the person's push stopped at the point at.
	fault := func(at codefault.Point) func(t *testing.T) {
		return func(t *testing.T) {
			t.Helper()
			reached := make(chan struct{})
			c.Fleet.Faults().Arm(at, codefault.Action{Once: true, Err: errFault, Reached: reached})
			gitPush(t)
			select {
			case <-reached:
			default:
				t.Errorf("the push never reached %s", at)
			}
		}
	}
	// answered is a hand-built POST of the push service, which is answered
	// status, saying says.
	answered := func(status int, says string, req func(t *testing.T) (*http.Response, string)) func(t *testing.T) {
		return func(t *testing.T) {
			t.Helper()
			if resp, body := req(t); resp.StatusCode != status || !strings.Contains(body, says) {
				t.Errorf("the push was answered %d %q, want %d saying %q", resp.StatusCode, body, status, says)
			}
		}
	}
	for _, step := range []struct {
		name    string
		push    func(t *testing.T)
		outcome types.AuditOutcome
		refusal host.AuditRefusal
	}{
		{"its advertisement's sync failing", unavailable(gitPush, "/info/refs"), types.AuditFailed, ""},
		// A DELETION OF TOPIC, sent alone: it carries no pack, so its command
		// section is its whole body, and nothing before the sync refuses it.
		{"its POST's sync failing", unavailable(func(t *testing.T) {
			answered(http.StatusServiceUnavailable, "the code log could not be read here",
				func(t *testing.T) (*http.Response, string) {
					return post(t, c, 0, "ceo", "git-receive-pack", "",
						pkt(topic+" "+gitrefZero+" refs/heads/topic\x00report-status\n")+"0000")
				})(t)
		}, "/git-receive-pack"), types.AuditFailed, ""},
		{"a fault past its commands", fault(codefault.HostAfterCommands), types.AuditFailed, ""},
		{"a fault past its sync", fault(codefault.HostAfterSync), types.AuditFailed, ""},
		{"a fault past its scratch", fault(codefault.HostAfterScratch), types.AuditFailed, ""},
		{"a body it cannot decode", answered(http.StatusUnsupportedMediaType, "content encoding br",
			func(t *testing.T) (*http.Response, string) {
				return postEncoded(t, c, "ceo", "br", "0000")
			}), types.AuditFailed, ""},
		{"a length declared past what one push may send", answered(http.StatusRequestEntityTooLarge,
			"past code.limits.max_push_bytes", func(t *testing.T) (*http.Response, string) {
				return raw(t, c, "ceo", "git-receive-pack", "", 2<<30, "0000")
			}), types.AuditRefused, host.RefusalPushTooLarge},
	} {
		t.Run(step.name, func(t *testing.T) {
			before, pushes := len(c.Fleet.Audited()), node.Host().Status(id).Repo.Day.Pushes
			step.push(t)
			audited := c.Fleet.Audited()[before:]
			// FAILED, stamped for a refused push as for a failed one, so the
			// event store's failure filter finds both.
			want := types.OperatorActed{ActorName: "ceo", ActorKind: string(iam.ActorHuman),
				OperatorID: iam.TokenLogin("ceo"), Transport: types.TransportGit, Tool: host.AuditTool,
				Repo: codetest.RepoName, Outcome: step.outcome, Refusal: string(step.refusal), Failed: true}
			if len(audited) != 1 {
				t.Fatalf("the push was recorded %d times: %+v; want once, as %+v", len(audited), audited, want)
			}
			got := audited[0]
			if got.RequestID == "" {
				t.Errorf("the push's record names no request id: %+v", got)
			}
			want.RequestID = got.RequestID
			if got != want {
				t.Errorf("the push was recorded as %+v, want %+v", got, want)
			}
			if n := node.Host().Status(id).Repo.Day.Pushes; n != pushes+1 {
				t.Errorf("the repository's heat counts %d pushes after the push, want %d", n, pushes+1)
			}
		})
	}
}

// postEncoded is a hand-built POST of the push service to node 0, as who, its
// body sent under the Content-Encoding encoding.
func postEncoded(t *testing.T, c *codetest.Case, who, encoding, body string) (*http.Response, string) {
	t.Helper()
	n := c.Fleet.Nodes()[0]
	req, err := http.NewRequestWithContext(c.Ctx, http.MethodPost,
		n.URL()+"/git/"+codetest.RepoName+"/git-receive-pack", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-git-receive-pack-request")
	req.Header.Set("Content-Encoding", encoding)
	req.SetBasicAuth("crewlet", c.Fleet.Token(t, who))
	resp, err := n.Client().Do(req)
	if err != nil {
		t.Fatalf("POST git-receive-pack: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	answer, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read the answer to POST git-receive-pack: %v", err)
	}
	return resp, string(answer)
}

// ADR-0054: receive-pack never runs in a cache — every push, its
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

// fullBucket is a backend with no room left: a put reads what it is handed and
// is refused as a backend refuses one for room ([objstore.Full]), storing
// nothing, naming a room of the case's own ([caseRemedy]) — so a line that
// names it spliced the store's remedy rather than a spelling of its own.
type fullBucket struct{ objstore.Backend }

// caseRemedy is the room [fullBucket] names.
const caseRemedy = "an operator makes room in this case's own bucket"

func (fullBucket) Put(_ context.Context, name string, r io.Reader, _ objstore.PutMeta) error {
	if _, err := io.Copy(io.Discard, r); err != nil {
		return err
	}
	return &objstore.Full{Remedy: caseRemedy, Err: fmt.Errorf(
		"fullbucket: put %s: OBJ_crewlet_files holds as many bytes as it reserves", name)}
}

// A PUSH THE OBJECT STORE HAS NO ROOM FOR IS REFUSED AS FULL, naming the room
// the store names: never `unavailable`, whose sentence says to run git push
// again — into a store that refuses the same push the same way until an
// operator makes room, so a person, or a CI runner on a loop, re-sent the
// whole pack each time. Nothing was stored, nothing proposed, and the push is
// recorded refused under its own class, so the audit tells a full store from
// a store that failed. Over the fleet's own bucket's twin, and over S3 through
// an in-process bucket whose provider has no room (MinIO's drives at their
// floor), through the production backend.
//
// Mutation: answer a full store as `unavailable` again (land.go's
// objstore.Full case removed) and this fails on the line git prints and on
// the record; splice a room of the landing's own and it fails on the room;
// drop s3obj's classification and the s3fake arm fails.
func TestAPushTheStoreHasNoRoomForIsRefusedAsFull(t *testing.T) {
	for _, arm := range []struct {
		name    string
		objects func(t *testing.T) objstore.Backend
		// remedy is the room the store names, and said its own words.
		remedy, said string
	}{
		{"the nats twin", func(*testing.T) objstore.Backend { return fullBucket{memobj.New()} },
			caseRemedy, "OBJ_crewlet_files holds as many bytes"},
		{"s3fake", func(t *testing.T) objstore.Backend {
			srv := s3fake.Start(t, "files")
			b := s3Over(t, srv)
			srv.Fill(http.StatusInsufficientStorage, "XMinioStorageFull")
			return b
		}, s3obj.FullRemedy, "XMinioStorageFull"},
	} {
		t.Run(arm.name, func(t *testing.T) {
			l, e := &logs{}, &recording{}
			c := newCase(t, codetest.SoloOptions{Objects: arm.objects(t), Logger: l.logger(),
				Shape: codetest.Shape{WrapEstate: e.wrap}}, codetest.Settings())
			p := codetest.NewPerson(t, c.Fleet, "ceo")
			work := p.Init()
			p.Commit(work, map[string]string{"README.md": "one\n"}, "one")

			res := p.Push(c.Ctx, work, c.URL(0), "main")
			reason, rejected := res.Rejected("refs/heads/main")
			switch {
			case !rejected || !strings.Contains(reason, "(store_full: the object store has no room for the "):
				t.Errorf("a push the store has no room for printed %q, want (store_full: …): %s", reason, res)
			case !strings.Contains(reason, arm.remedy):
				t.Errorf("a push the store has no room for printed %q, which does not name the store's room %q",
					reason, arm.remedy)
			case strings.Contains(reason, "push again"):
				t.Errorf("a push the store has no room for printed %q, which sends the person to push again "+
					"into a store that refuses it the same", reason)
			case strings.Contains(reason, arm.said):
				t.Errorf("a push the store has no room for printed %q, the store's own words", reason)
			}
			if names := objects(t, c); len(names) > 0 {
				t.Errorf("a push the store had no room for stored %v", names)
			}
			if n := len(e.seen("propose")); n > 0 {
				t.Errorf("a push the store had no room for was proposed %d times", n)
			}
			if rows := pushRows(t, c); len(rows) > 0 {
				t.Errorf("a push the store had no room for left push rows %v", rows)
			}
			if audited := c.Fleet.Audited(); len(audited) != 1 || audited[0].Outcome != types.AuditRefused ||
				audited[0].Refusal != string(code.RefusalStoreFull) {
				t.Errorf("a push the store had no room for was recorded as %+v, want one refused/store_full",
					audited)
			}
			if n := l.count("code_push_store_full", `"level":"WARN"`, arm.said); n != 1 {
				t.Errorf("code_push_store_full was logged %d times with the store's answer, want once: %q", n,
					l.lines("code_push_store_full"))
			}
		})
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
	reachedFirst(t, reached, stale)
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

// ONLY A LONE FLUSH IS GIT'S PROBE before a push past its post buffer —
// answered 200, empty, running nothing and recording nothing. A section
// holding ANY line is a push, whatever the receiver could make of the line: one
// it cannot parse goes to receive-pack, which refuses it in its own words, and
// the push is recorded. Read as "no command parsed", a section of lines the
// receiver could not parse was answered as the probe — an empty 200, the body
// never read, nothing recorded — and git printed only that the remote end hung
// up.
func TestOnlyALoneFlushIsTheProbe(t *testing.T) {
	path, calls := gittest.Recording(t)
	c := newCase(t, codetest.SoloOptions{GitPath: path}, codetest.Settings())
	resp, body := post(t, c, 0, "ceo", "git-receive-pack", "", "0000")
	if resp.StatusCode != http.StatusOK || body != "" ||
		resp.Header.Get("Content-Type") != "application/x-git-receive-pack-result" {
		t.Errorf("the probe answered %d %q %q", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
	if n := receivePacks(calls); n != 0 {
		t.Errorf("the probe ran receive-pack %d times", n)
	}
	if audited := c.Fleet.Audited(); len(audited) != 0 {
		t.Errorf("the probe was recorded: %+v", audited)
	}

	resp, body = post(t, c, 0, "ceo", "git-receive-pack", "", pkt("not a command\n")+"0000")
	if resp.StatusCode == http.StatusOK && body == "" {
		t.Errorf("a section holding a line no command parses from was answered as the probe")
	}
	if !strings.Contains(body, "protocol error") {
		t.Errorf("a line no command parses from answered %d %q, want receive-pack's own words",
			resp.StatusCode, body)
	}
	if n := receivePacks(calls); n != 1 {
		t.Errorf("a line no command parses from ran receive-pack %d times, want once", n)
	}
	if audited := c.Fleet.Audited(); len(audited) != 1 || audited[0].Transport != types.TransportGit ||
		audited[0].ActorName != "ceo" {
		t.Errorf("a push of a line no command parses from was recorded as %+v, want once, as ceo's", audited)
	}
}

// A REF PAST THE LONGEST A PUSH MAY NAME is a command all the same — git
// accepts one, and a pkt-line carries one of up to 64 KiB — refused bad_ref
// and recorded, alone and beside a ref that lands. Read as "not a command",
// such a ref pushed alone was answered as git's probe: git printed that the
// remote end hung up, and nothing was recorded.
func TestARefPastTheLongestIsRefusedAsNotARef(t *testing.T) {
	c := newCase(t, codetest.SoloOptions{}, codetest.Settings())
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	head := p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	long := "refs/heads/" + strings.Repeat("a", 100) + "/" + strings.Repeat("b", 100) + "/" +
		strings.Repeat("c", 100)
	if len(long) <= code.MaxRefNameBytes {
		t.Fatalf("the ref is %d bytes, not past the longest (%d)", len(long), code.MaxRefNameBytes)
	}
	notARef := "(bad_ref: " + code.RefusalBadRef.Sentence() + ")"

	alone := p.Push(c.Ctx, work, c.URL(0), "HEAD:"+long)
	if reason, rejected := alone.Rejected(long); !rejected || !strings.Contains(reason, "[remote rejected]") ||
		!strings.Contains(reason, notARef) {
		t.Errorf("a ref of %d bytes pushed alone printed %q, want it refused %s: %s", len(long), reason,
			notARef, alone)
	}
	audited := c.Fleet.Audited()
	if len(audited) != 1 || audited[0].Outcome != types.AuditRefused ||
		audited[0].Refusal != string(code.RefusalBadRef) || audited[0].Transport != types.TransportGit {
		t.Errorf("a ref of %d bytes pushed alone was recorded as %+v, want one refused/bad_ref", len(long),
			audited)
	}

	beside := p.Push(c.Ctx, work, c.URL(0), "main", "HEAD:"+long)
	if !beside.OK("refs/heads/main") {
		t.Errorf("main, pushed beside a ref of %d bytes: %s", len(long), beside)
	}
	if reason, rejected := beside.Rejected(long); !rejected || !strings.Contains(reason, notARef) {
		t.Errorf("a ref of %d bytes pushed beside main printed %q, want it refused %s", len(long), reason,
			notARef)
	}
	if rows := c.Rows(); rows.Refs["refs/heads/main"] != head || rows.Refs[long] != "" {
		t.Errorf("main is at %q and the long ref at %q, want %s and nothing", rows.Refs["refs/heads/main"],
			rows.Refs[long], head)
	}
}

// A REF PAST THE LONGEST RIDES ITS RECORD BOUNDED: a ref the engine refuses
// bad_ref may run to a pkt-line's 64 KiB, and JSON spells an `&` in six bytes,
// so four such refs carried whole beside main took the push's entry past its
// record's budget — the committer refused the receiver's own proposal, main
// was refused with them, an ERROR said the receiver had a bug, and the bundle
// was stored for nothing. And a ref that is not UTF-8 reads back from JSON as
// another ref: told to git from the row, two of them were statuses for refs
// git never sent, and git printed that proc-receive failed to report. Carried
// as code.RecordedRef, each costs the record a few hundred bytes: main lands,
// git is told each refused ref by the name it sent, and the push's row holds
// each in its bounded form.
func TestALongRefRidesItsRecordBounded(t *testing.T) {
	l := &logs{}
	c := newCase(t, codetest.SoloOptions{Logger: l.logger()}, codetest.Settings())
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	head := p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	refspecs := []string{"main"}
	var long []string
	for i := range 4 {
		ref := fmt.Sprintf("refs/heads/%d/%s", i, strings.Repeat("&", 60_000))
		long = append(long, ref)
		refspecs = append(refspecs, "HEAD:"+ref)
	}
	for _, ref := range []string{"refs/heads/\xff", "refs/heads/\xfe"} {
		long = append(long, ref)
		refspecs = append(refspecs, "HEAD:"+ref)
	}
	notARef := "(bad_ref: " + code.RefusalBadRef.Sentence() + ")"

	res := p.Push(c.Ctx, work, c.URL(0), refspecs...)
	if !res.OK("refs/heads/main") {
		t.Errorf("main, pushed beside four refs of 60 KiB of '&', was not taken: %s",
			strings.ReplaceAll(res.String(), strings.Repeat("&", 60_000), "<60 000 &>"))
	}
	for _, ref := range long {
		if reason, rejected := res.Rejected(ref); !rejected || !strings.Contains(reason, notARef) {
			t.Errorf("a ref of %d bytes printed %q, want it refused %s", len(ref), reason, notARef)
		}
	}
	if got := c.Rows().Refs["refs/heads/main"]; got != head {
		t.Errorf("main is at %q, want %s", got, head)
	}
	if lines := l.at("ERROR"); len(lines) > 0 {
		t.Errorf("a push of refs the engine refuses logged at ERROR: %q", lines)
	}
	rows := pushRows(t, c)
	if len(rows) != 1 {
		t.Fatalf("%d push rows, want the one push", len(rows))
	}
	recorded := map[string]code.Command{}
	for _, cmd := range rows[0].Commands {
		recorded[cmd.Ref] = cmd
	}
	for _, ref := range long {
		cmd, ok := recorded[code.RecordedRef(ref)]
		if !ok || cmd.Result != code.ResultNG || cmd.Reason != code.RefusalBadRef {
			t.Errorf("the push row holds %+v for a ref of %d bytes, want it as %q, refused bad_ref", cmd,
				len(ref), code.RecordedRef(ref))
		}
		if len(cmd.Ref) > code.MaxRefNameBytes {
			t.Errorf("the push row holds a ref of %d bytes, past the longest (%d)", len(cmd.Ref),
				code.MaxRefNameBytes)
		}
	}
}

// A PUSH TOO WIDE FOR ITS RECORD IS REFUSED BEFORE ITS WORK: seven hundred
// refs of the longest, each spelled six times over in the record's JSON (`&`),
// beside main reach about 1.1 MiB of entry, past the room one push may take.
// Refused by its committer, such a push had already run receive-pack, its
// scans and its bundle's upload and made the estate's round trip, and was
// answered as the engine refusing its own proposal — an ERROR, the push
// recorded failed and its bundle stored for nothing. The receiver measures the
// commands as it reads them, with everything the work could add at its
// largest, and refuses the push in the client's words before receive-pack.
func TestAPushTooWideForItsRecordStoresNothing(t *testing.T) {
	l := &logs{}
	path, calls := gittest.Recording(t)
	c := newCase(t, codetest.SoloOptions{GitPath: path, Logger: l.logger()}, codetest.Settings())
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	head := p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	var create strings.Builder
	var wide []string
	for i := range 700 {
		ref := fmt.Sprintf("refs/heads/%s%04d", strings.Repeat("&", code.MaxRefNameBytes-len("refs/heads/")-4), i)
		wide = append(wide, ref)
		fmt.Fprintf(&create, "create %s %s\n", ref, head)
	}
	if _, stderr, err := p.Git().Try(t, work, strings.NewReader(create.String()), "update-ref", "--stdin"); err != nil {
		t.Fatalf("create the refs: %v: %s", err, stderr)
	}

	res := p.Push(c.Ctx, work, c.URL(0), "refs/heads/*:refs/heads/*")
	for _, ref := range append([]string{"refs/heads/main"}, wide[0], wide[len(wide)-1]) {
		reason, rejected := res.Rejected(ref)
		if !rejected || !strings.Contains(reason, "(this push's ref updates take up to ") ||
			!strings.Contains(reason, "push them in parts") {
			t.Errorf("%.40s… of a push too wide for its record printed %q, want it refused for its width",
				ref, reason)
		}
	}
	if n := pushReceives(calls); n != 0 {
		t.Errorf("a push too wide for its record ran receive-pack %d times", n)
	}
	if names := objects(t, c); len(names) > 0 {
		t.Errorf("a push too wide for its record stored %v", names)
	}
	if lines := l.at("ERROR"); len(lines) > 0 {
		t.Errorf("a push too wide for its record logged at ERROR: %q", lines)
	}
	if audited := c.Fleet.Audited(); len(audited) != 1 || audited[0].Outcome != types.AuditRefused ||
		audited[0].Refusal != string(host.RefusalCommands) {
		t.Errorf("a push too wide for its record was recorded as %+v, want one refused/commands", audited)
	}
	if got := c.Rows().Refs["refs/heads/main"]; got != "" {
		t.Errorf("main is at %s after its push was refused whole", got)
	}
}

// A HAND-BUILT PUSH IS JUDGED AS RECEIVE-PACK READS IT. A command whose ids a
// client wrote in capitals, or whose capabilities ride a later line, is the
// command git acts on, so every check the receiver makes before receive-pack
// holds it — read as no command, a push of hundreds of the widest refs in
// capitals ran receive-pack and was refused by the receiver's own proposal
// check, as its bug, at ERROR; and one ref named in both cases was a ref named
// twice the same way. A push certificate, whose commands receive-pack acts on
// though the engine never advertised one, is refused before anything runs.
// And a command list that cannot be read is recorded like every other push.
func TestAHandBuiltPushIsJudgedAsReceivePackReadsIt(t *testing.T) {
	l := &logs{}
	path, calls := gittest.Recording(t)
	settings := codetest.Settings()
	settings.Protections = append([]code.Protection{{Pattern: "refs/heads/frozen", DirectPush: code.DirectNone}},
		settings.Protections...)
	c := newCase(t, codetest.SoloOptions{GitPath: path, Logger: l.logger()}, settings)
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	head := p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")
	upper := strings.ToUpper(head)
	create := func(id, ref string) string { return gitrefZero + " " + id + " " + ref }
	pack := emptyPack(t, p.Git(), work)
	var wide strings.Builder
	for i := range 700 {
		caps := "\n"
		if i == 0 {
			caps = "\x00report-status\n"
		}
		wide.WriteString(pkt(create(upper, fmt.Sprintf("refs/heads/%s%04d",
			strings.Repeat("&", code.MaxRefNameBytes-len("refs/heads/")-4), i)) + caps))
	}
	certificate := pkt("push-cert\x00report-status\n") + pkt("certificate version 0.1\n") +
		pkt("pusher ceo <ceo@example.com> 0 +0000\n") + pkt("pushee crewlet\n") + pkt("nonce n\n") + pkt("\n") +
		pkt(create(head, "refs/heads/signed")+"\n") + pkt("push-cert-end\n") + "0000"

	for _, tc := range []struct {
		name    string
		body    string
		status  int
		banded  bool   // the answer is side-band framed
		answer  string // in the answer (its band 1, when banded)
		absent  string // never in the answer
		outcome types.AuditOutcome
		refusal host.AuditRefusal
		lands   string // the ref the push lands, after receive-pack has run
	}{
		{name: "ids in capitals, too wide for the record", body: wide.String() + "0000" + pack,
			status: http.StatusOK, answer: "0000 this push's ref updates take up to ", outcome: types.AuditRefused,
			refusal: host.RefusalCommands},
		{name: "one ref named in both cases", body: pkt(create(head, "refs/heads/twice")+"\x00report-status\n") +
			pkt(create(upper, "refs/heads/twice")+"\n") + "0000" + pack, status: http.StatusOK,
			answer: `"refs/heads/twice" is named twice`, outcome: types.AuditRefused, refusal: host.RefusalCommands},
		{name: "ids in capitals, protected", body: pkt(create(upper, "refs/heads/frozen")+"\x00report-status\n") +
			"0000" + pack, status: http.StatusOK, answer: "ng refs/heads/frozen protected_direct",
			outcome: types.AuditRefused, refusal: host.AuditRefusal(code.RefusalProtectedDirect)},
		{name: "the capabilities on a later line", body: pkt(create(head, "refs/notes/a")+"\n") +
			pkt(create(head, "refs/notes/b")+"\x00report-status side-band-64k\n") + "0000" + pack,
			status: http.StatusOK, banded: true, answer: "ng refs/notes/b reserved_ref",
			outcome: types.AuditRefused, refusal: host.AuditRefusal(code.RefusalReservedRef)},
		// A CLIENT THAT ASKS FOR NO REPORT GETS NONE, as from receive-pack.
		{name: "a refusal to a client that asked for no report", body: pkt(create(head, "refs/notes/c")+
			"\x00side-band-64k\n") + "0000" + pack, status: http.StatusOK, banded: true, absent: "unpack",
			outcome: types.AuditRefused, refusal: host.AuditRefusal(code.RefusalReservedRef)},
		{name: "a push certificate", body: certificate + pack, status: http.StatusBadRequest,
			answer: "this push carries a push certificate (a signed push)", outcome: types.AuditRefused,
			refusal: host.RefusalCommands},
		{name: "a command list that is not pkt-lines", body: "zzzz", status: http.StatusBadRequest,
			answer: "the push's command list could not be read", outcome: types.AuditRefused,
			refusal: host.RefusalCommands},
		{name: "a command list cut short", body: pkt(create(head, "refs/heads/cut") + "\x00report-status\n")[:40],
			status: http.StatusBadRequest, answer: "the push's command list could not be read",
			outcome: types.AuditFailed},
		{name: "ids in capitals land", body: pkt(create(upper, "refs/heads/upper")+"\x00report-status\n") + "0000" +
			pack, status: http.StatusOK, answer: "ok refs/heads/upper", outcome: types.AuditApplied,
			lands: "refs/heads/upper"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, ran, stored, logged := len(c.Fleet.Audited()), pushReceives(calls), len(objects(t, c)),
				len(l.at("ERROR"))
			resp, answer := post(t, c, 0, "ceo", "git-receive-pack", "", tc.body)
			if resp.StatusCode != tc.status {
				t.Fatalf("answered %d %q, want %d", resp.StatusCode, answer, tc.status)
			}
			if tc.banded {
				data, _, err := demux(answer)
				if err != nil {
					t.Fatalf("the answer to a push asking for side-band on its second line is not side-band: "+
						"%v: %q", err, answer)
				}
				answer = data
			}
			if !strings.Contains(answer, tc.answer) {
				t.Errorf("answered %.300q, want it to say %q", answer, tc.answer)
			}
			if tc.absent != "" && strings.Contains(answer, tc.absent) {
				t.Errorf("answered %.300q, which says %q", answer, tc.absent)
			}
			if runs := pushReceives(calls) - ran; runs != 0 && tc.lands == "" {
				t.Errorf("ran receive-pack %d times for a push refused before it", runs)
			}
			if got := len(objects(t, c)); got != stored {
				t.Errorf("stored %d objects", got-stored)
			}
			if lines := l.at("ERROR")[logged:]; len(lines) > 0 {
				t.Errorf("logged at ERROR: %q", lines)
			}
			if audited := c.Fleet.Audited()[before:]; len(audited) != 1 || audited[0].Outcome != tc.outcome ||
				audited[0].Refusal != string(tc.refusal) {
				t.Errorf("recorded as %+v, want one %s/%s", audited, tc.outcome, tc.refusal)
			}
			for ref, at := range c.Rows().Refs {
				if ref != "refs/heads/main" && (ref != tc.lands || at != head) {
					t.Errorf("%.40s… is at %s after the push", ref, at)
				}
			}
		})
	}
}

// A PROPOSAL ITS RECEIVER REFUSES IS NEVER SENT, AND ITS UPLOAD IS ENDED: the
// receiver holds its proposal to every rule its committer will once the
// commits it introduces are listed, while its bundle is still uploading — so a
// receiver's bug is answered as the committer would answer it, with nobody
// asked and no bundle stored for a push no row will name. Stood in by the
// check itself, since no push the commands' own bound admits reaches it.
func TestAProposalItsReceiverRefusesIsNeverSent(t *testing.T) {
	l, e := &logs{}, &recording{}
	c := newCase(t, codetest.SoloOptions{Logger: l.logger(), Shape: codetest.Shape{WrapEstate: e.wrap}},
		codetest.Settings())
	held, reached := make(chan struct{}), make(chan struct{})
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(held) }) })
	c.Fleet.Faults().Arm(codefault.HostUpload, codefault.Action{Once: true, Block: held, Reached: reached})
	checked := make(chan struct{})
	host.SetProposalCheck(c.Fleet.Nodes()[0].Host(), func(code.Proposal) error {
		select { // the upload is held when the check refuses
		case <-reached:
		case <-time.After(10 * time.Second):
		}
		close(checked)
		return fmt.Errorf("%w: a receiver's bug", code.ErrInvalidProposal)
	})
	go func() { // a check never asked must not hold the push for ever
		select {
		case <-checked:
		case <-time.After(10 * time.Second):
			release.Do(func() { close(held) })
		}
	}()
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"README.md": "one\n"}, "one")

	res := p.Push(c.Ctx, work, c.URL(0), "main")
	if reason, _ := res.Rejected("refs/heads/main"); !strings.Contains(reason,
		"(unavailable: the engine refused its own proposal: ") || !strings.Contains(reason, "a receiver's bug") {
		t.Errorf("a push whose proposal its receiver refused printed %q: %s", reason, res)
	}
	if n := len(e.seen("propose")); n != 0 {
		t.Errorf("a proposal its receiver refused was sent %d times", n)
	}
	if names := objects(t, c); len(names) > 0 {
		t.Errorf("a push whose proposal its receiver refused stored %v", names)
	}
	if n := l.count("code_push_invalid", `"level":"ERROR"`); n != 1 {
		t.Errorf("code_push_invalid logged %d times at ERROR, want once: it is the receiver's bug", n)
	}
	if audited := c.Fleet.Audited(); len(audited) != 1 || audited[0].Outcome != types.AuditFailed {
		t.Errorf("the push was recorded as %+v, want one failed", audited)
	}
}

// THE CAPABILITIES ARE THE FIRST COMMAND LINE'S, WHATEVER ITS REF: a push
// whose first command names a ref past the longest still asked for side-band
// and --atomic, so its refusal is framed in the band it asked for and refused
// whole before receive-pack runs — and a refusal line naming the longest ref a
// pkt-line holds still fits one, the ref whole and the sentence cut to the
// room left.
func TestTheFirstCommandsCapabilitiesAreReadWhateverItsRef(t *testing.T) {
	path, calls := gittest.Recording(t)
	c := newCase(t, codetest.SoloOptions{GitPath: path}, codetest.Settings())
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	head := p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")

	// The longest ref the first command line's pkt-line holds beside both
	// ids and these capabilities.
	const caps = "\x00report-status side-band-64k atomic\n"
	ids := gitrefZero + " " + head + " "
	long := "refs/heads/" + strings.Repeat("l", largePacketDataMax-len(ids)-len(caps)-len("refs/heads/"))
	body := pkt(ids+long+caps) + pkt(gitrefZero+" "+head+" refs/heads/other\n") + "0000"
	before, ran := len(c.Fleet.Audited()), receivePacks(calls)
	resp, answer := post(t, c, 0, "ceo", "git-receive-pack", "", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a push whose first ref is %d bytes answered %d %q", len(long), resp.StatusCode, answer)
	}
	report, progress, err := demux(answer)
	if err != nil {
		t.Fatalf("the report of a push that asked for side-band is not side-band: %v", err)
	}
	if !strings.Contains(progress, "crewlet: refused before anything was stored") {
		t.Errorf("band 2 carried %q", progress)
	}
	lines, err := pktLines(report)
	if err != nil {
		t.Fatalf("the report is not pkt-lines git reads: %v", err)
	}
	if len(lines) != 3 || lines[0] != "unpack ok\n" ||
		!strings.HasPrefix(lines[1], "ng "+long+" bad_ref: ") ||
		!strings.HasPrefix(lines[2], "ng refs/heads/other atomic: ") {
		short := make([]string, len(lines))
		for i, l := range lines {
			short[i] = strings.Replace(l, long, "<the long ref>", 1)
		}
		t.Errorf("the report read %q, want unpack ok, the long ref refused bad_ref and other refused atomic",
			short)
	}
	if n := receivePacks(calls) - ran; n != 0 {
		t.Errorf("an --atomic push one ref of which the receiver refused ran receive-pack %d times", n)
	}
	if audited := c.Fleet.Audited()[before:]; len(audited) != 1 || audited[0].Outcome != types.AuditRefused ||
		audited[0].Refusal != string(code.RefusalBadRef) {
		t.Errorf("the push was recorded as %+v, want one refused/bad_ref", audited)
	}
	if got := c.Rows().Refs["refs/heads/other"]; got != "" {
		t.Errorf("other is at %s after an --atomic push refused whole", got)
	}
}

// A refusal the receiver synthesises is what git prints, per ref; and a push
// option of a namespace the code host does not read — a forge's — is read by
// nobody: the push lands, told nothing of a change it never asked for.
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
	options := p.PushArgs(c.Ctx, work, []string{"-o", "ci.skip"}, c.URL(0), "main")
	if options.Err != nil || !options.OK("refs/heads/main") || strings.Contains(options.Stderr, "crewlet: change") {
		t.Errorf("a push with a forge's option: %s", options)
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
	reachedFirst(t, reached, done)
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
	reachedFirst(t, reached, pushed)
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
	asked, err := host.ProtectAfterForce(cmds, rules, code.PrincipalOperator, judged)
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

// A STALE UPDATE IS STALE BESIDE DAMAGE GIT READS AROUND. The force check
// asks `merge-base --is-ancestor <old> <new>` in the push's scratch, whose
// alternate is the cache; an old this company holds no object for makes git
// say "Not a valid commit name" (gitproc.ErrMissingObject), which is the
// update's refusal as stale. A multi-pack-index whose header names another
// hash version is a file git reads around — it says so ("multi-pack-index hash
// version … does not match", gitproc.ErrCorrupt) and answers from the packs —
// and git looks for an object it does not hold through every index of the
// scratch's alternate, so the run names both. The run's kind is the HIGHEST it
// named, the index's, and a check that asked only for the kind answered the
// push as the engine's own failure: every kind the run named is reachable
// through errors.Is, so the stale update is refused as stale, as on a sound
// copy. (A commit-graph's damaged header is no such case: merge-base never
// opens the graph for an id no pack holds — measured on git 2.43 and 2.47.)
//
// Mutation: make gitproc.ExitError match its Kind alone, and the push is
// answered as a failure.
func TestAStaleForcedPushBesideADamagedIndexIsStale(t *testing.T) {
	var l logs
	c := newCase(t, codetest.SoloOptions{Logger: l.logger()}, codetest.Settings())
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	head := p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")
	// The header's sixth byte is the hash version the index's ids are in.
	midx := filepath.Join(maintainersBitmap(t, c, &l), "objects", "pack", "multi-pack-index")
	overwriteFile(t, midx, 5, []byte{7})

	unheld := randomOID()
	body := pkt(unheld+" "+head+" refs/heads/main\x00report-status\n") + "0000" + emptyPack(t, p.Git(), work)
	resp, answer := post(t, c, 0, "ceo", "git-receive-pack", "", body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(answer, "ng refs/heads/main stale") {
		t.Fatalf("a push of main from %s, which the repository never held, beside a damaged multi-pack-index: "+
			"answered %d %.300q, want it refused stale", unheld, resp.StatusCode, answer)
	}
	if got := c.Rows().Refs["refs/heads/main"]; got != head {
		t.Errorf("main is at %s after the stale push, want %s", got, head)
	}
}

// AN OLD COMMIT GIT CANNOT READ IS NO STALE UPDATE. The force check's
// merge-base over an old commit whose entry in the copy's pack is damaged
// says what it met (a pack report, gitproc.ErrCorrupt) and then that the old
// is "Not a valid commit name" (gitproc.ErrMissingObject) — the second only
// BECAUSE of the first. Stale says the company never held the old, which is
// false here, and it would hide the damage from the operator; the update is
// answered as a check the engine could not make, logged code_push_unchecked
// naming the damage, and the branch does not move. Only damage git reads
// AROUND — a derived file, beside the missing object
// (TestAStaleForcedPushBesideADamagedIndexIsStale) — leaves an update stale.
//
// A SERVED copy, because that is where a force check runs over damage: the
// maintainer's commit-graph names the old commit, so receive-pack's own
// connectivity check takes it from the graph (without one, receive-pack
// refuses the update itself, "missing necessary objects"). Which damage
// reaches the check is git's to decide, so there are two arms. A zlib stream
// damaged in the old's data: 2.43 and 2.47 parse the old for merge-base from
// its object and meet it, while 2.56 takes it from the graph and needs only
// the entry's header — the update is then answered from the graph, and
// lands. An entry's TYPE damaged: every git reads the type and meets it
// (measured on 2.43, 2.47 and 2.56). No arm may answer stale, and on every
// git at least one meets the damage in the force check.
//
// Mutation: answer stale whenever the run named a missing object, whatever
// else it named — the arm that meets the damage answers stale.
func TestAnOldCommitInADamagedPackIsNoStaleUpdate(t *testing.T) {
	var unchecked atomic.Int32
	t.Run("arms", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			damage func(t *testing.T, pack string, start, data int64)
		}{
			{"its zlib stream", func(t *testing.T, pack string, _, data int64) {
				overwriteFile(t, pack, data+4, mangled(readAt(t, pack, data+4, 6)))
			}},
			{"its type", func(t *testing.T, pack string, start, _ int64) {
				b := readAt(t, pack, start, 1)[0]
				overwriteFile(t, pack, start, []byte{b&0x8f | 5<<4}) // type 5 is reserved
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				var l logs
				c := newCase(t, codetest.SoloOptions{Logger: l.logger()}, codetest.Settings())
				p := codetest.NewPerson(t, c.Fleet, "ceo")
				work := p.Init()
				old := p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
				c.MustPush(p, work, "main")
				next := p.Commit(work, map[string]string{"README.md": "two\n"}, "two")
				c.MustPush(p, work, "HEAD:refs/heads/side")
				dir := maintainersBitmap(t, c, &l)
				eventually(t, "the maintainer's commit-graph", func() bool {
					_, err := os.Stat(filepath.Join(dir, "objects", "info", "commit-graph"))
					return err == nil
				})
				pack, start, data := entryOf(t, dir, old)
				tc.damage(t, pack, start, data)

				body := pkt(old+" "+next+" refs/heads/main\x00report-status\n") + "0000" +
					emptyPack(t, p.Git(), work)
				resp, answer := post(t, c, 0, "ceo", "git-receive-pack", "", body)
				if resp.StatusCode != http.StatusOK || strings.Contains(answer, "stale") ||
					!strings.Contains(answer, "ok refs/heads/main") && !strings.Contains(answer, "ng refs/heads/main") {
					t.Fatalf("a fast-forward of main from %s, whose pack entry is damaged: answered %d %.300q, "+
						"want it landed from the graph or refused, never stale", old, resp.StatusCode, answer)
				}
				met := l.count("code_push_unchecked", `"level":"WARN"`, "git read a pack")
				if met > 0 {
					unchecked.Add(1)
					if got := c.Rows().Refs["refs/heads/main"]; got != old {
						t.Errorf("main is at %s after the unchecked push, want %s", got, old)
					}
				}
				t.Logf("answered %q; the force check met the damage: %v", answer, met > 0)
			})
		}
	})
	if unchecked.Load() == 0 {
		t.Errorf("no arm's force check met the damage on this git: the case asserts nothing about it")
	}
}

// oidN is a distinct object id for n.
func oidN(n int) string { return fmt.Sprintf("%040x", n+1) }

// shellWord is s as one POSIX shell word.
func shellWord(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// A PROTECTED BRANCH IS NOT FORCED THROUGH A DAMAGED GRAPH. The force check
// asks git whether a push's old commit is an ancestor of its new one, and git
// answers from the commit-graph without checking it: a graph a previous
// process left that names another commit as the tip's first parent makes a
// force look like a fast-forward — 2.55 and 2.56 answer from it, 2.40 to
// 2.48 read the commit itself here and answer right. The first round of the
// process that serves the copy holds the graph to its trailer before it
// answers anything, drops it, and the push is refused as a force, as on a
// sound copy.
//
// Mutation: skip the check (sync.go apply) — on 2.56 the force passes the
// protection and main moves; on every git the graph is left in place.
func TestAProtectedBranchIsNotForcedThroughADamagedGraph(t *testing.T) {
	var l logs
	c := newCase(t, codetest.SoloOptions{Logger: l.logger()}, codetest.Settings())
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	first := p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	side := strings.TrimSpace(p.Git().Run(t, work, "commit-tree", first+"^{tree}", "-p", first, "-m", "side"))
	c.MustPush(p, work, side+":refs/heads/main")
	p.Commit(work, map[string]string{"README.md": "two\n"}, "two")
	tip := p.Commit(work, map[string]string{"b.txt": "three\n"}, "three")
	c.MustPush(p, work, "HEAD:refs/heads/dev")
	dir := maintainersBitmap(t, c, &l)
	graph := filepath.Join(dir, "objects", "info", "commit-graph")
	eventually(t, "the maintainer's commit-graph", func() bool {
		_, err := os.Stat(graph)
		return err == nil
	})

	// A previous process's copy: its graph says the tip's parent is side.
	node := c.Fleet.(*codetest.SoloFleet).GitNode(0)
	node.Stop(t.Context())
	gittest.Damage(t, dir, gittest.GraphParentOther, tip, side)
	node.Restart(t)

	body := pkt(side+" "+tip+" refs/heads/main\x00report-status\n") + "0000" + emptyPack(t, p.Git(), work)
	resp, answer := post(t, c, 0, "ceo", "git-receive-pack", "", body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(answer, "ng refs/heads/main") ||
		!strings.Contains(answer, "force") {
		t.Errorf("a force of protected main from %s to %s over a damaged graph: answered %d %.300q, want it "+
			"refused as a force", side, tip, resp.StatusCode, answer)
	}
	if got := c.Rows().Refs["refs/heads/main"]; got != side {
		t.Errorf("main is at %s after the force, want %s", got, side)
	}
	var damaged uint64
	for _, s := range node.Node().Metrics().Read() {
		if s.Name == metrics.CodeCacheChecks && s.Attrs["file"] == "commit-graph" && s.Attrs["outcome"] == "damaged" {
			damaged += s.Total
		}
	}
	if damaged != 1 {
		t.Errorf("%s{file=commit-graph,outcome=damaged} = %d, want the one graph the first round found",
			metrics.CodeCacheChecks, damaged)
	}
}
