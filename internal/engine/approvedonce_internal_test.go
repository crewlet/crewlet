package engine

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A LATER ATTEMPT AT AN ANSWER TELLS THE CALL IT APPROVED ONCE, end to end:
// through the coordination store's log of the round, the conversion of that
// log and of the held call the claim read into the runner's terms, and a runner
// relaunching the round in the asking job's own box. A person allows a held
// call once; the first attempt at the answer marks the call, makes it and
// records what it returned, and its relaunch is refused, so its claim is given
// back and the answer attempted again. The round's log then holds the call
// under the held call's id, after every call it took, the held call the job
// parked on — its question, which the bridge logs as it holds it — among them.
// The later attempt answers the call from its mark, and its lead says what the
// call returned; the copy in the asking job's box, its harness cut off, is told
// every other call of the round, the held one included, and that one nowhere
// else.
//
// AND WITH A CALL OF THE ROUND STILL IN FLIGHT, its entry written ahead of it
// and its update not landed: a call a person approved never passes the bridge,
// so it writes no entry — its identity is on the row before it is made, and its
// append to the log is keyed by its held call's id — and it is placed after the
// call in flight, never at its place; the copy is told the call in flight once,
// as a call whose outcome is unknown, and the approved call by the lead alone.
//
// Mutation: drop the held call's id from the calls a resume hands the runner,
// or from the held call it is handed, or list the lead's approved call among
// the round's calls, and the call is told twice; place the approved call off
// the listed calls alone, and it takes the place of the call in flight.
func TestALaterAttemptAtAnAnswerTellsTheCallItApprovedOnceOffTheRoundsLog(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	const turnID = "t-approved-once"
	read := newHeldTool("read_page")
	close(read.release)
	asking := launchBridged(t, turnID, read)
	asking.call(t, "read_page")
	launch := asking.row(t, turnID).LaunchID
	// THE HELD CALL ITSELF, in the round's log as the bridge records a call the
	// seat's surface held for a person's approval: its arguments withheld.
	held := sandbox.BridgeCall{Name: "vendor_post", Seq: 2, StartedAt: time.Now().UTC(), Failed: true,
		Held: true, Output: "held for a person's approval"}
	recorded, err := asking.pending.AppendBridgeCall(ctx, turnID, sandbox.BridgeAppend{Launch: launch,
		Round: asking.row(t, turnID).Round, Call: held})
	if err != nil || !recorded {
		t.Fatalf("the held call's append = %v, %v", recorded, err)
	}
	if begun, err := asking.pending.AppendBridgeCall(ctx, turnID, sandbox.BridgeAppend{Step: sandbox.AppendBegin,
		Launch: launch, Round: asking.row(t, turnID).Round,
		Call: sandbox.BridgeCall{Name: "delegate", Seq: 3, StartedAt: time.Now().UTC()}}); err != nil || !begun {
		t.Fatalf("the entry of the call in flight = %v, %v", begun, err)
	}
	parkedOnAHeldCall(t, asking.pending, turnID, launch)
	seal := sandbox.NewCallSeal(valueRing(t, "k1"))

	// THE FIRST ATTEMPT marks the call, makes it and records what it
	// returned; then its relaunch is refused, and its claim given back.
	if _, ok, err := asking.pending.ClaimForResume(ctx, turnID, sandbox.RecordedAnswerTail(launch),
		sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("ClaimForResume = %v, %v", ok, err)
	}
	marked, first, err := asking.pending.MarkHeldCall(ctx, turnID, launch, sandbox.Fence{})
	if err != nil || !first {
		t.Fatalf("MarkHeldCall = %v, %v", first, err)
	}
	result, err := seal.SealResult(turnID, marked, sandbox.HeldResult{Output: "posted to the vendor"})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := asking.pending.RecordHeldResult(ctx, turnID, launch, sandbox.HeldMade{Result: result,
		Call: sandbox.BridgeCall{Name: "vendor_post", Origin: "mcp:vendor", Server: "vendor",
			Source: "mcp:vendor"}}, sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("RecordHeldResult = %v, %v", ok, err)
	}
	if ok, err := asking.pending.ReleaseClaim(ctx, turnID, sandbox.Release{Launch: launch,
		To: sandbox.StatusAnswered}); err != nil || !ok {
		t.Fatalf("ReleaseClaim = %v, %v", ok, err)
	}

	// THE LATER ATTEMPT, its claim reading the call as the first left it, and
	// its read of the round's calls as the coordinator makes it: the job's
	// log, with the call a person approved that only the row may hold yet.
	claimed, ok, err := asking.pending.ClaimForResume(ctx, turnID, sandbox.RecordedAnswerTail(launch),
		sandbox.Fence{})
	if err != nil || !ok || claimed.HeldCall == nil || !claimed.HeldCall.Made() {
		t.Fatalf("ClaimForResume = %+v, %v, %v, want the answer claimed with its call made",
			claimed.HeldCall, ok, err)
	}
	row := asking.row(t, turnID)
	logged, err := asking.pending.TurnLog(ctx, row)
	if err != nil {
		t.Fatalf("TurnLog: %v", err)
	}
	at := slices.IndexFunc(logged.Calls, func(c sandbox.BridgeCall) bool {
		return c.Approved && c.HeldID == claimed.HeldCall.ID
	})
	if at < 0 {
		t.Fatalf("the round's calls read %+v, want the call the first attempt made under its held call's id",
			logged.Calls)
	}
	if len(logged.InFlight) != 1 || logged.Calls[at].Seq <= logged.InFlight[0].Seq {
		t.Errorf("the approved call is at place %d beside %+v in flight, want it placed after the call in flight",
			logged.Calls[at].Seq, logged.InFlight)
	}
	row.Launch.HarnessEnd = sandbox.HarnessEndCutOff
	brief := relaunchedInPlace(t, asking, runnerResume(resumeInput{Run: row, Bridged: logged,
		BridgedRead: logged.Appends(), State: execstate.State{Version: execstate.Version, Round: 1},
		Asked: &sandbox.AskedRun{Question: "May swe call vendor_post on the vendor MCP server?",
			By: "founder", SessionID: "sess-ask", BoxKept: true,
			Held: &sandbox.HeldResume{TurnID: turnID, Call: *claimed.HeldCall, Choice: sandbox.HeldOnce,
				By: "founder"}}}, nil, seal))

	if strings.Count(brief, "The call was made, and returned:") != 1 ||
		!strings.Contains(brief, "posted to the vendor") {
		t.Errorf("the lead does not tell once what the approved call returned:\n%s", brief)
	}
	const sealed = "- vendor_post(arguments sealed for a person's approval, kept on no record) → "
	if strings.Contains(brief, sealed+"success") || strings.Count(brief, sealed+"error:") != 1 ||
		strings.Count(brief, "- read_page(") != 1 {
		t.Errorf("the copy is not told every call of the round but the one its lead tells, once each:\n%s",
			brief)
	}
	if strings.Count(brief, runner.UnfinishedNote) != 1 {
		t.Errorf("the copy is not told the call still in flight once, as one whose outcome is unknown:\n%s",
			brief)
	}
}
