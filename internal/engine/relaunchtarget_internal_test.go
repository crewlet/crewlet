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
// ON. Which member sits out a spent plan is the holder's own bench, so a reader
// on another node asks the incarnation the seat's lease names, and is answered
// with that node's pick — its member's harness, the cell its run lands in, the
// node itself, whether the company keeps sessions — and its runner's answer for
// each session asked about, never the reader's own pick. A reader that holds the
// seat answers itself. And what cannot be said is undecided, never a no: a lease
// table that cannot be read, a seat no node holds, a holder that does not
// answer. A question addressed to another incarnation is not answered.
//
// Mutation: answer from the reader's own pick, drop a field from the reply,
// answer the archives in another order, read an unreadable lease as one held by
// nobody, or answer a question addressed elsewhere, and a case goes red.
func TestTheNodeHoldingTheSeatIsAskedWhatARelaunchIsLaunchedOn(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	q := memory.New()
	if err := q.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(ctx)) })
	held := sandbox.RelaunchTarget{Harness: "claude-code", Placement: sandbox.Container, Node: "node-a",
		Restorer: onlyClaude{}, NoKept: sandbox.NoKeptSession}
	own := sandbox.RelaunchTarget{Harness: "opencode", Placement: sandbox.Direct, Node: "node-b",
		Why: "the reader's own pick"}
	holder := relaunchTargets{owner: "node-a:1", local: func(string) sandbox.RelaunchTarget { return held }}
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
			local: func(string) sandbox.RelaunchTarget { return own }, budget: 2 * time.Second, now: time.Now}
	}
	claude := sandbox.SessionArchive{Harness: "claude-code", SessionID: "s-1", Format: 1, Version: "2.1.310"}
	other := sandbox.SessionArchive{Harness: "opencode", SessionID: "ses_1", Format: 1, Version: "2.2.5"}
	seat := uuid.New()

	got := reader(live("node-a:1")).target(ctx, seat, "swe", []sandbox.SessionArchive{claude, other})
	if got.Undecided || got.Harness != "claude-code" || got.Placement != sandbox.Container ||
		got.Node != "node-a" || got.NoKept != sandbox.NoKeptSession || got.Why != "" {
		t.Fatalf("a reader on another node was answered %+v, want the holder's pick", got)
	}
	if ok, why := got.Restores(claude); !ok || why != "" {
		t.Errorf("the holder's runner restores the Claude Code session %v (%q), want it restored", ok, why)
	}
	if ok, why := got.Restores(other); ok || !strings.Contains(why, "drives claude-code") {
		t.Errorf("the holder's runner restores the OpenCode session %v (%q), want its refusal", ok, why)
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
		got := r.target(ctx, seat, "swe", []sandbox.SessionArchive{claude})
		if got.Undecided != tc.undecided || !strings.Contains(got.Why, tc.why) {
			t.Errorf("%s: the reader was answered %+v, want undecided %v saying %q", name, got, tc.undecided,
				tc.why)
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
