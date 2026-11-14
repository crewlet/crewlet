package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
)

// heldParked is run turnID on store, parked on a call to `vendor_post` held
// for approval, its arguments sealed under seal, with a person's answer
// allowing it once recorded; and the job that held it.
func heldParked(t *testing.T, store *sandbox.CoordStore, seal *sandbox.CallSeal, turnID string) string {
	t.Helper()
	ctx := t.Context()
	opened, err := store.BeginLaunch(ctx, sandbox.PendingRun{TurnID: turnID, AgentHandle: "swe",
		AgentID: "a-1", Role: "SWE", Harness: "claude-code"}, sandbox.Fence{})
	if err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	launch := opened.LaunchID
	held := sandbox.HeldCall{ID: "0190f3a8-0000-7000-8000-000000000002", LaunchID: launch,
		Server: "vendor", Tool: "vendor_post", Raw: "post", Digest: "sha256:d1", HeldAt: time.Now().UTC()}
	if held.Arguments, err = seal.SealArguments(turnID, held, map[string]any{"body": "done"}); err != nil {
		t.Fatal(err)
	}
	const question = "May swe call vendor_post on the vendor MCP server?"
	if got, err := store.RecordAsk(ctx, turnID, launch, sandbox.Ask{Question: question,
		Decision: sandbox.HeldDecision(question, nil), To: sandbox.AskToManager, Held: &held}); err != nil ||
		got != sandbox.AskRecorded {
		t.Fatalf("RecordAsk = %v, %v", got, err)
	}
	if err := store.MarkAwaiting(ctx, turnID, sandbox.Clarification{AskedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("MarkAwaiting: %v", err)
	}
	if _, ok, err := store.RecordAnswer(ctx, turnID, launch, sandbox.RecordedAnswer{
		Sealed: []byte("sealed: once"), Via: types.AnswerViaOperator, EventIDs: []string{"r1"}},
		sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("RecordAnswer = %v, %v", ok, err)
	}
	return launch
}

// heldAttemptOn is one attempt at run turnID's answer, as the coordinator hands
// it to the runner: claimed, the held call as the claim read the row, and its
// mark and result written under the claim.
func heldAttemptOn(t *testing.T, store *sandbox.CoordStore, seal *sandbox.CallSeal, turnID,
	launch string) *runner.HeldCall {

	t.Helper()
	claimed, ok, err := store.ClaimForResume(t.Context(), turnID, sandbox.RecordedAnswerTail(launch),
		sandbox.Fence{})
	if err != nil || !ok || claimed.HeldCall == nil {
		t.Fatalf("ClaimForResume = %+v, %v, %v, want the answer claimed with its held call",
			claimed.HeldCall, ok, err)
	}
	asked := runnerAsked(&sandbox.AskedRun{Held: &sandbox.HeldResume{
		TurnID: turnID, Call: *claimed.HeldCall, Choice: sandbox.HeldOnce,
		Mark: func(ctx context.Context) (sandbox.HeldCall, bool, error) {
			return store.MarkHeldCall(ctx, turnID, launch, sandbox.Fence{})
		},
		Record: func(ctx context.Context, made sandbox.HeldMade) error {
			recorded, err := store.RecordHeldResult(ctx, turnID, launch, made, sandbox.Fence{})
			if err == nil && !recorded {
				err = errors.New("the result was not recorded")
			}
			return err
		},
	}}, seal)
	return asked.Held
}

// A LATER ATTEMPT ANSWERS A HELD CALL FROM THE MARK ITS CLAIM READ. The first
// attempt at a person's answer marks the call and makes it, and its relaunch
// can be refused after — by the floor of the session it continues, say — so
// its claim is given back and the answer attempted again. The claim that takes
// it again reads the row as the first attempt left it, and the commit the
// runner is handed answers from that read: the result recorded, or, where the
// first attempt stopped before it recorded one, unknown — and, where the node
// carrying the run on cannot open the result recorded, made all the same, with
// whether the tool reported a failure off the call's entry recorded with it,
// since the round's log lists the call made and the lead is its only telling;
// and the call's arguments stay sealed, since nothing makes it again. The runner asks this
// before it holds the call to the tools and skills it carries the run on with
// ([runner.HeldCommit.Marked]), so a guard or a tool that changed in between
// never tells the run a call it made was not made. The mark's own answer
// ([runner.HeldCommit.Begin]) says the same.
//
// AND IT IS HANDED THAT RESULT AS THE ROW SEALED IT, which the answer's lead
// keeps for a later brief to tell again, whether or not it opens here.
//
// AND THE ATTEMPT THAT MAKES THE CALL IS ANSWERED THAT RESULT AS ITS RECORD
// SEALED IT ([runner.HeldCommit.Record]), the usual case, which its lead keeps
// too.
//
// Mutation: have Marked answer false for a claim that read the call marked, or
// drop its result, and the later attempt is not answered from the mark; hand it
// no kept result, or have Record answer none, and a lead telling the call again
// cannot say what it returned; open the arguments of a marked call, and they
// are opened for a call that never runs again; answer a result this node
// cannot open as no result, or without whether the tool failed, and the run is
// told the call may never have been made.
func TestALaterAttemptAnswersAHeldCallFromTheMarkItsClaimRead(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		records, failed bool
		// elsewhere is a later attempt on a node holding no key the first's
		// result was sealed under.
		elsewhere bool
		want      runner.HeldPrior
	}{
		"the first attempt recorded what the call returned": {records: true,
			want: runner.HeldPrior{Made: true, Result: sandbox.HeldResult{Output: "posted"}}},
		"the first attempt stopped before it recorded anything": {},
		"the first attempt recorded a failure this node cannot open": {records: true, failed: true,
			elsewhere: true, want: runner.HeldPrior{Made: true, Unread: true,
				Result: sandbox.HeldResult{Failed: true}}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			seal := sandbox.NewCallSeal(valueRing(t, "k1"))
			store := sandbox.NewCoordStore(coordmemory.NewFleet())
			launch := heldParked(t, store, seal, "run-1")

			// THE FIRST ATTEMPT finds the call unmarked, its arguments
			// opened for the call it makes, and marks it.
			first := heldAttemptOn(t, store, seal, "run-1", launch)
			if prior, marked := first.Commit.Marked(ctx); marked {
				t.Fatalf("a call no attempt marked reads as marked: %+v", prior)
			}
			if first.Args["body"] != "done" || first.Unopened != "" {
				t.Fatalf("the first attempt's arguments = %v (%q), want them opened", first.Args, first.Unopened)
			}
			if prior, err := first.Commit.Begin(ctx); err != nil || !prior.First {
				t.Fatalf("Begin = %+v, %v, want this mark the first", prior, err)
			}
			if tc.records {
				returned := sandbox.HeldResult{Output: "posted", Failed: tc.failed}
				sealed, err := first.Commit.Record(ctx, returned, toolloop.ToolResult{Output: "posted",
					Failed: tc.failed, Origin: tools.Origin("vendor"), Server: "vendor", Source: tools.Origin("vendor")})
				if err != nil {
					t.Fatalf("Record: %v", err)
				}
				// AND THE ATTEMPT THAT MADE THE CALL IS ANSWERED WHAT IT SEALED,
				// which the answer's lead keeps: it opens to what the call
				// returned.
				if result, err := seal.OpenKept("run-1", sealed); err != nil || result != returned {
					t.Fatalf("Record answers %+v, which opens as %+v, %v; want what the call returned, %+v",
						sealed, result, err, returned)
				}
			}
			// ITS RELAUNCH REFUSED, and its claim given back.
			if ok, err := store.ReleaseClaim(ctx, "run-1", sandbox.Release{Launch: launch,
				To: sandbox.StatusAnswered}); err != nil || !ok {
				t.Fatalf("ReleaseClaim = %v, %v", ok, err)
			}

			opens := seal
			if tc.elsewhere {
				opens = sandbox.NewCallSeal(valueRing(t, "k2"))
			}
			again := heldAttemptOn(t, store, opens, "run-1", launch)
			// AND THE RESULT AS THE ROW SEALED IT, wherever the first attempt
			// recorded one, which the answer's lead keeps whether or not it
			// opens here: it opens under the first attempt's key.
			kept := func(prior runner.HeldPrior) runner.HeldPrior {
				t.Helper()
				if (prior.Kept != nil) != tc.records {
					t.Fatalf("the later attempt is handed the result as the row sealed it %+v, want it %v",
						prior.Kept, tc.records)
				}
				if prior.Kept != nil {
					if result, err := seal.OpenKept("run-1", *prior.Kept); err != nil || result.Output != "posted" {
						t.Fatalf("the result kept for the lead opens as %+v, %v; want what the call returned",
							result, err)
					}
				}
				prior.Kept = nil
				return prior
			}
			if prior, marked := again.Commit.Marked(ctx); !marked || kept(prior) != tc.want {
				t.Fatalf("the later attempt's claim reads %+v (marked %v), want %+v off the mark",
					prior, marked, tc.want)
			}
			if again.Args != nil || again.Unopened != "" {
				t.Fatalf("the later attempt opened the arguments of a call it never makes: %v (%q)",
					again.Args, again.Unopened)
			}
			if prior, err := again.Commit.Begin(ctx); err != nil || kept(prior) != tc.want {
				t.Fatalf("the mark answers the later attempt %+v, %v, want %+v", prior, err, tc.want)
			}
		})
	}
}
