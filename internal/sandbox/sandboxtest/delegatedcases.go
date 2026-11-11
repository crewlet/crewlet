package sandboxtest

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/delegated"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A held call put to delegates (ADR-0125): the cases [Run] drives for it. Each
// is a property of a write of the row: a delegate's answer recorded once per
// delegate and only while the call waits on its delegates, and an escalation
// that ends that wait once.

// DelegatedParked launches turnID, holds an ssh_operate call put to delegates
// (rule 1) for it and parks it on the question, answering the held call as
// recorded. The engine's sweep cases seed their rows with it.
func DelegatedParked(t *testing.T, s sandbox.PendingStore, turnID string, delegates []string,
	heldAt time.Time) sandbox.HeldCall {
	t.Helper()
	ctx := t.Context()
	launch := mustLaunched(t, s, run(turnID))
	held := sandbox.HeldCall{ID: "0190f3a8-0000-7000-8000-0000000000d1", LaunchID: launch,
		Server: "crewlet", Tool: "ssh_operate", Raw: "ssh_operate", Digest: "sha256:op", HeldAt: heldAt,
		Fixed: true, Approvers: sandbox.HeldApproversSSH, Act: string(delegated.KindSSHOperate),
		Delegation: &sandbox.HeldDelegation{Rule: 1, Delegates: delegates, Digest: strings.Repeat("ab", 32)}}
	sealed, err := heldSeal(t).SealArguments(turnID, held, map[string]any{"host": "api-1",
		"command": "sudo systemctl restart api", "reason": "the release needs it"})
	if err != nil {
		t.Fatal(err)
	}
	held.Arguments = sealed
	const question = "May sre run a command on api-1 as crewlet-ops?"
	ask := sandbox.Ask{Question: question, Decision: sandbox.HeldFixedDecision(question, nil),
		To: sandbox.AskToManager, Held: &held}
	if got, err := s.RecordAsk(ctx, turnID, launch, ask); err != nil || got != sandbox.AskRecorded {
		t.Fatalf("RecordAsk of a delegated held call = %v, %v", got, err)
	}
	if err := s.MarkAwaiting(ctx, turnID, sandbox.Clarification{AskedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("MarkAwaiting: %v", err)
	}
	return held
}

var delegatedCases = []struct {
	name string
	fn   func(*testing.T, sandbox.PendingStore)
}{
	{"APanelVerdictIsRecordedOncePerDelegate", testAPanelVerdictIsRecordedOncePerDelegate},
	{"AnEscalationEndsTheWaitOnTheDelegatesOnce", testAnEscalationEndsTheWaitOnTheDelegatesOnce},
}

// verdictBy is a high-confidence answer by.
func verdictBy(by, verdict string) sandbox.HeldVerdict {
	return sandbox.HeldVerdict{By: by, Verdict: verdict, Confidence: string(delegated.High),
		Reason: "routine restart", At: base}
}

// A PANEL'S ANSWERS ARE RECORDED ONCE PER DELEGATE, and only a delegate's: the
// second answer by the same seat, and one by a seat the call was not put to,
// write nothing, and every write answers the call as written.
func testAPanelVerdictIsRecordedOncePerDelegate(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	held := DelegatedParked(t, s, "t-panel", []string{"security-a", "security-b"}, base)
	got, ok, err := s.GiveDelegateVerdict(ctx, "t-panel", held.LaunchID, held.ID, verdictBy("security-a", "allow"))
	if err != nil || !ok || len(got.Delegation.Given) != 1 || got.Delegation.Given[0].By != "security-a" {
		t.Fatalf("the first delegate's answer = %+v, %v, %v; want it recorded", got.Delegation, ok, err)
	}
	_, again, againErr := s.GiveDelegateVerdict(ctx, "t-panel", held.LaunchID, held.ID,
		verdictBy("security-a", "deny"))
	if againErr != nil || again {
		t.Fatalf("a delegate's second answer = %v, %v; want nothing written", again, againErr)
	}
	_, stranger, strangerErr := s.GiveDelegateVerdict(ctx, "t-panel", held.LaunchID, held.ID,
		verdictBy("intruder", "allow"))
	if strangerErr != nil || stranger {
		t.Fatalf("an answer by a seat the call was not put to = %v, %v; want nothing written", stranger,
			strangerErr)
	}
	got, ok, err = s.GiveDelegateVerdict(ctx, "t-panel", held.LaunchID, held.ID, verdictBy("security-b", "allow"))
	if err != nil || !ok || len(got.Delegation.Given) != 2 {
		t.Fatalf("the second delegate's answer = %+v, %v, %v; want both on the call", got.Delegation, ok, err)
	}
	if _, _, badErr := s.GiveDelegateVerdict(ctx, "t-panel", held.LaunchID, held.ID,
		sandbox.HeldVerdict{By: "security-b", Verdict: "maybe", Confidence: "high", At: base}); badErr == nil {
		t.Fatalf("an answer naming no verdict was taken")
	}
}

// AN ESCALATION ENDS THE WAIT ON THE DELEGATES ONCE: it is written while the
// call waits on them, the call is no longer delegated after it, and neither a
// second escalation nor a late answer writes anything.
func testAnEscalationEndsTheWaitOnTheDelegatesOnce(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	held := DelegatedParked(t, s, "t-esc", []string{"security-a"}, base)
	e := sandbox.HeldEscalation{Overruled: []string{delegated.OverruledDeadline},
		Engine: []string{"No delegate answered in time."}, At: base}
	if ok, err := s.EscalateHeld(ctx, "t-esc", held.LaunchID, held.ID, e); err != nil || !ok {
		t.Fatalf("the escalation = %v, %v; want it written", ok, err)
	}
	r, found, err := s.Get(ctx, "t-esc")
	if err != nil || !found || r.HeldCall.Delegated() || r.HeldCall.Delegation.Escalated == nil ||
		r.HeldCall.Delegation.Escalated.Overruled[0] != delegated.OverruledDeadline {
		t.Fatalf("the escalated call reads %+v, %v, %v", r.HeldCall, found, err)
	}
	if ok, err := s.EscalateHeld(ctx, "t-esc", held.LaunchID, held.ID, e); err != nil || ok {
		t.Fatalf("a second escalation = %v, %v; want nothing written", ok, err)
	}
	if _, ok, err := s.GiveDelegateVerdict(ctx, "t-esc", held.LaunchID, held.ID,
		verdictBy("security-a", "allow")); err != nil || ok {
		t.Fatalf("a late answer = %v, %v; want nothing written", ok, err)
	}
	if ok, err := s.EscalateHeld(ctx, "t-esc", "another-launch", held.ID, e); err != nil || ok {
		t.Fatalf("an escalation naming another question = %v, %v", ok, err)
	}
}
