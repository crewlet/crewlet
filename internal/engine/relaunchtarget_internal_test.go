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
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/queue"
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

func (onlyClaude) Restores(archive sandbox.SessionArchive) sandbox.SessionContinuity {
	if archive.Harness != "claude-code" {
		return sandbox.SessionContinuity{Loss: executorkeep.LossHarnessChanged,
			Why: "the session was written by " + archive.Harness + ", and this runner drives claude-code"}
	}
	return sandbox.SessionContinuity{Continues: true}
}

// THE NODE HOLDING THE SEAT IS ASKED WHAT A RELAUNCH OF ITS TURNS IS LAUNCHED
// ON, its bench and each turn's revision being its own: a reader elsewhere asks
// the incarnation the lease names, whatever its own clock says of the lease,
// once for every session, and gets that node's pick per turn (harness, cell, entry,
// node, kept sessions, why) and its runner's answer; a holder answers itself.
// What cannot be said is undecided, never a no: an unreadable lease, an unheld
// seat, a silent holder, a holder's own undecided pick with its reason.
//
// Mutation: answer with the reader's own pick or one pick for every turn, drop
// a reply field (undecided or its reason included), reorder the sessions, judge
// the lease on the reader's clock, read an unreadable lease as unheld, or
// answer a question addressed to another incarnation: red.
func TestTheNodeHoldingTheSeatIsAskedWhatARelaunchIsLaunchedOn(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	q := memory.New()
	if err := q.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(ctx)) })
	// THE HOLDER'S PICKS, by turn: a member that restores what Claude Code
	// wrote, in a company that keeps none; every member sitting out; and no
	// member at all, saying why.
	picks := map[string]sandbox.RelaunchTarget{
		"t-held": {Harness: "claude-code", Placement: sandbox.Container, Node: "node-a",
			Entry: "entry-held", LaunchRouted: true, Restorer: onlyClaude{}, NoKept: sandbox.NoKeptSession},
		"t-sitting-out": {Node: "node-a", Undecided: true,
			Why: "every member of the seat's executor chain is sitting out its plan"},
		"t-no-member": {Node: "node-a", Why: "the company the node holding the seat runs holds no seat swe"},
	}
	own := sandbox.RelaunchTarget{Harness: "opencode", LaunchRouted: true, Placement: sandbox.Direct, Node: "node-b",
		Why: "the reader's own pick"}
	holder := relaunchTargets{owner: "node-a:1",
		local: func(_, turn string) sandbox.RelaunchTarget { return picks[turn] }}
	stop, err := q.Serve(ctx, topics.HeldRelaunch, queue.ConcurrentAnswers, func(_ context.Context, raw []byte) ([]byte, error) {
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
			local: func(string, string) sandbox.RelaunchTarget { return own }, budget: 2 * time.Second}
	}
	claude := sandbox.SessionArchive{Harness: "claude-code", SessionID: "s-1", Format: 1, Version: "2.1.310"}
	other := sandbox.SessionArchive{Harness: "opencode", SessionID: "ses_1", Format: 1, Version: "2.3.2"}
	seat := uuid.New()

	got := reader(live("node-a:1")).targets(ctx, seat, "swe", []sandbox.RelaunchAsk{
		{Turn: "t-held", Archive: claude}, {Turn: "t-held", Archive: other},
		{Turn: "t-sitting-out", Archive: claude}, {Turn: "t-no-member", Archive: claude},
	})
	if len(got) != 4 {
		t.Fatalf("four sessions asked about were answered %d targets, want one each", len(got))
	}
	if held := got[0]; held.Undecided || held.Harness != "claude-code" || held.Placement != sandbox.Container ||
		held.Node != "node-a" || held.NoKept != sandbox.NoKeptSession || held.Why != "" ||
		held.Entry != "entry-held" || !held.LaunchRouted {
		t.Fatalf("a reader on another node was answered %+v, want the holder's pick", held)
	}
	if r := got[0].Restores(claude); !r.Continues || r.Why != "" {
		t.Errorf("the holder's runner restores the Claude Code session %v (%q), want it restored", r.Continues, r.Why)
	}
	if r := got[1].Restores(other); r.Continues || !strings.Contains(r.Why, "drives claude-code") {
		t.Errorf("the holder's runner restores the OpenCode session %v (%q), want its refusal", r.Continues, r.Why)
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
	if r := got[3].Restores(claude); r.Continues || r.Why != picks["t-no-member"].Why {
		t.Errorf("a holder with no member restores the session %v (%q), want its reason", r.Continues, r.Why)
	}
	row, run := parkedSession()
	if decided := sandbox.ReadContinuityOf(row.Kept(), "", &run, got[2], sandbox.ContinuedByNextTurn,
		time.Now()); decided.Decided ||
		!strings.Contains(decided.Why, "sitting out") {
		t.Errorf("a row decided on the holder's undecided pick reads %+v, want undecided", decided)
	}

	for name, tc := range map[string]struct {
		leases    leaseTable
		budget    time.Duration
		undecided bool
		why       string
		// node is whose pick the target is: the holder's, the reader's own,
		// or nobody's where it cannot be said.
		node string
	}{
		"the reader holds the seat": {leases: live("node-b:1"), why: "the reader's own pick", node: "node-b"},
		"a lease table that cannot be read": {leases: leaseTable{err: errors.New("the store is down")},
			undecided: true, why: "could not be read"},
		"a seat no node holds": {leases: leaseTable{}, undecided: true, why: "no node holds the seat"},
		// THE STORE'S WORD: it handed the lease out live, and its deadline
		// is past only on the reader's clock, which runs ahead of the store's.
		"a seat held, read on a clock running ahead": {leases: leaseTable{lease: &coord.Lease{
			Owner: "node-a:1", ExpiresAt: time.Now().Add(-time.Minute)}}, node: "node-a"},
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
			if target.Undecided != tc.undecided || !strings.Contains(target.Why, tc.why) || target.Node != tc.node {
				t.Errorf("%s: the reader was answered %+v, want undecided %v saying %q, %q's pick", name, target,
					tc.undecided, tc.why, tc.node)
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
		Object: key.String(), At: at, Through: new(0)}
	row := executordomain.Session{Handle: "swe", Turn: "t-sitting-out", Harness: "claude-code",
		SessionID: "s-1", ArchiveFormat: sandbox.FakeArchiveFormat,
		Object: objstore.Object{Key: key, Size: 1}, LaunchSeq: 2, CaptureSeq: 1}
	return row, sandbox.PendingRun{TurnID: "t-sitting-out", AgentHandle: "swe", Status: sandbox.StatusAwaiting,
		SandboxID: "box-1", Placement: string(sandbox.Direct), LaunchNode: "node-a", Harness: "claude-code",
		SessionID: "s-1", LaunchSeq: 2, Capture: &kept, Kept: &kept}
}
