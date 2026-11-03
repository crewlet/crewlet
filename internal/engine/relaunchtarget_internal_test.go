package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// leaseTable is a seat's lease as the coordination store answers it: one lease,
// or a failure to read it.
type leaseTable struct {
	lease *coord.Lease
	err   error
}

func (l leaseTable) Get(context.Context, string) (*coord.Lease, error) { return l.lease, l.err }

// onlyClaude restores every session Claude Code wrote, and none another wrote.
type onlyClaude struct{}

func (onlyClaude) Restores(archive sandbox.SessionArchive) (bool, string) {
	if archive.Harness != "claude-code" {
		return false, "the session was written by " + archive.Harness + ", and this runner drives claude-code"
	}
	return true, ""
}

// THE NODE HOLDING THE SEAT IS ASKED WHAT A RELAUNCH OF ITS TURNS IS LAUNCHED
// ON. Which member sits out a spent plan is the holder's own bench, and which
// chain a turn's relaunch picks from is the revision that turn's relaunch runs
// on there, so a reader on another node asks the incarnation the seat's lease
// names, once for every session it reads, each named with its turn, and is
// answered for each with that node's pick for the turn — its member's harness,
// the cell its run lands in, the node itself, whether the company keeps
// sessions, why there is no member or why none can be said now — and its
// runner's answer for the session, never the reader's own pick. A reader that
// holds the seat answers itself. And what cannot be said is undecided, never a
// no: a lease table that cannot be read, a seat no node holds, a holder that
// does not answer — and a holder's own undecided pick, which crosses as one,
// with its reason. A question addressed to another incarnation is not answered.
//
// Mutation: answer from the reader's own pick, answer every turn with one pick,
// drop a field from the reply — its undecided word or its reason included —
// answer the sessions in another order, read an unreadable lease as one held by
// nobody, or answer a question addressed elsewhere, and a case goes red.
func TestTheNodeHoldingTheSeatIsAskedWhatARelaunchIsLaunchedOn(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	q := memory.New()
	if err := q.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(ctx)) })
	// THE HOLDER'S PICKS, by turn: a member that restores what Claude Code
	// wrote in a company that keeps none; every member sitting out; and no
	// member at all, which says why.
	picks := map[string]sandbox.RelaunchTarget{
		"t-held": {Harness: "claude-code", Placement: sandbox.Container, Node: "node-a",
			Restorer: onlyClaude{}, NoKept: sandbox.NoKeptSession},
		"t-sitting-out": {Node: "node-a", Undecided: true,
			Why: "every member of the seat's executor chain is sitting out its plan"},
		"t-no-member": {Node: "node-a", Why: "the company the node holding the seat runs holds no seat swe"},
	}
	own := sandbox.RelaunchTarget{Harness: "opencode", Placement: sandbox.Direct, Node: "node-b",
		Why: "the reader's own pick"}
	holder := relaunchTargets{owner: "node-a:1",
		local: func(_, turn string) sandbox.RelaunchTarget { return picks[turn] }}
	stop, err := q.Serve(ctx, topics.HeldRelaunch, func(_ context.Context, raw []byte) ([]byte, error) {
		return holder.answer(raw)
	})
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	t.Cleanup(func() { _ = stop(context.WithoutCancel(ctx)) })
	live := func(owner string) leaseTable {
		return leaseTable{lease: &coord.Lease{Owner: owner, ExpiresAt: time.Now().Add(time.Minute)}}
	}
	reader := func(leases leaseTable) relaunchTargets {
		return relaunchTargets{owner: "node-b:1", queue: q, leases: leases,
			local: func(string, string) sandbox.RelaunchTarget { return own }, budget: 2 * time.Second,
			now: time.Now}
	}
	claude := sandbox.SessionArchive{Harness: "claude-code", SessionID: "s-1", Format: 1, Version: "2.1.310"}
	other := sandbox.SessionArchive{Harness: "opencode", SessionID: "ses_1", Format: 1, Version: "2.2.5"}
	seat := uuid.New()

	got := reader(live("node-a:1")).targets(ctx, seat, "swe", []sandbox.RelaunchAsk{
		{Turn: "t-held", Archive: claude}, {Turn: "t-held", Archive: other},
		{Turn: "t-sitting-out", Archive: claude}, {Turn: "t-no-member", Archive: claude},
	})
	if len(got) != 4 {
		t.Fatalf("four sessions asked about were answered %d targets, want one each", len(got))
	}
	if held := got[0]; held.Undecided || held.Harness != "claude-code" || held.Placement != sandbox.Container ||
		held.Node != "node-a" || held.NoKept != sandbox.NoKeptSession || held.Why != "" {
		t.Fatalf("a reader on another node was answered %+v, want the holder's pick", held)
	}
	if ok, why := got[0].Restores(claude); !ok || why != "" {
		t.Errorf("the holder's runner restores the Claude Code session %v (%q), want it restored", ok, why)
	}
	if ok, why := got[1].Restores(other); ok || !strings.Contains(why, "drives claude-code") {
		t.Errorf("the holder's runner restores the OpenCode session %v (%q), want its refusal", ok, why)
	}
	// THE HOLDER'S OWN UNKNOWN CROSSES AS ONE, with its reason, and a row
	// decided on it is undecided: read as decided, every member sitting out
	// would show as a session no relaunch continues.
	if sat := got[2]; !sat.Undecided || sat.Why != picks["t-sitting-out"].Why || sat.Harness != "" {
		t.Errorf("a holder whose every member sits out was read as %+v, want undecided, saying why", sat)
	}
	if none := got[3]; none.Undecided || none.Why != picks["t-no-member"].Why {
		t.Errorf("a holder with no member was read as %+v, want decided, saying why", none)
	}
	if ok, why := got[3].Restores(claude); ok || why != picks["t-no-member"].Why {
		t.Errorf("a holder with no member restores the session %v (%q), want its reason", ok, why)
	}
	row, run := parkedSession()
	if decided := sandbox.ReadContinuityOf(row, "", &run, got[2], time.Now()); decided.Decided ||
		!strings.Contains(decided.Why, "sitting out") {
		t.Errorf("a row decided on the holder's undecided pick reads %+v, want undecided", decided)
	}

	for name, tc := range map[string]struct {
		leases    leaseTable
		budget    time.Duration
		undecided bool
		why       string
	}{
		"the reader holds the seat": {leases: live("node-b:1"), why: "the reader's own pick"},
		"a lease table that cannot be read": {leases: leaseTable{err: errors.New("the store is down")},
			undecided: true, why: "could not be read"},
		"a seat no node holds": {leases: leaseTable{lease: &coord.Lease{Owner: "node-a:1",
			ExpiresAt: time.Now().Add(-time.Minute)}}, undecided: true, why: "no node holds the seat"},
		"a holder that does not answer": {leases: live("node-c:1"), budget: 200 * time.Millisecond,
			undecided: true, why: "did not say"},
	} {
		r := reader(tc.leases)
		if tc.budget > 0 {
			r.budget = tc.budget
		}
		got := r.targets(ctx, seat, "swe", []sandbox.RelaunchAsk{{Turn: "t-held", Archive: claude},
			{Turn: "t-held", Archive: other}})
		if len(got) != 2 {
			t.Errorf("%s: two sessions asked about were answered %d targets, want one each", name, len(got))
			continue
		}
		for _, target := range got {
			if target.Undecided != tc.undecided || !strings.Contains(target.Why, tc.why) {
				t.Errorf("%s: the reader was answered %+v, want undecided %v saying %q", name, target,
					tc.undecided, tc.why)
			}
		}
	}

	elsewhere, err := json.Marshal(relaunchAsk{Version: relaunchWireVersion, Owner: "node-c:1", Handle: "swe"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := holder.answer(elsewhere); !errors.Is(err, errRelaunchNotAddressed) {
		t.Errorf("a question addressed to another incarnation was answered (%v)", err)
	}
}

// parkedSession is a live turn's session whose job parked on a question in a
// box held on node-a's host, its capture kept and named by its row: decided on
// a target alone where the target cannot be said.
func parkedSession() (executordomain.Session, sandbox.PendingRun) {
	at := time.Now().UTC()
	key := objstore.KeyAt(at)
	kept := sandbox.SessionCapture{LaunchSeq: 2, CaptureSeq: 1, State: sandbox.CaptureLanded,
		Object: key.String(), At: at}
	row := executordomain.Session{Handle: "swe", Turn: "t-sitting-out", Harness: "claude-code",
		SessionID: "s-1", ArchiveFormat: sandbox.FakeArchiveFormat,
		Object: objstore.Object{Key: key, Size: 1}, LaunchSeq: 2, CaptureSeq: 1}
	return row, sandbox.PendingRun{TurnID: "t-sitting-out", AgentHandle: "swe", Status: sandbox.StatusAwaiting,
		SandboxID: "box-1", Placement: string(sandbox.Direct), LaunchNode: "node-a", Harness: "claude-code",
		SessionID: "s-1", LaunchSeq: 2, Capture: &kept, Kept: &kept}
}
