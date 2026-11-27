package sandboxtest

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/delegated"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/redact/redacttest"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A held call put to delegates (ADR-0125): the cases [Run] drives for it. Each
// is a property of a write of the row: a delegate's answer recorded once per
// delegate and only while the call waits on its delegates, and an escalation
// that ends that wait once.

// HoldingClock is the incarnation whose clock [DelegatedParked] holds its call
// on ([sandbox.HeldCall.HeldClock]): a sweep under it judges the call as its
// own.
const HoldingClock = "node-a:holder"

// DelegatedParked launches turnID, holds an ssh_operate call put to delegates
// (rule 1) for it at heldAt on [HoldingClock] and parks it on the question,
// answering the held call as recorded. The engine's sweep cases seed their
// rows with it.
func DelegatedParked(t *testing.T, s sandbox.PendingStore, turnID string, delegates []string,
	heldAt time.Time) sandbox.HeldCall {
	t.Helper()
	ctx := t.Context()
	launch := mustLaunched(t, s, run(turnID))
	held := sandbox.HeldCall{ID: "0190f3a8-0000-7000-8000-0000000000d1", LaunchID: launch,
		Server: "crewlet", Tool: "ssh_operate", Raw: "ssh_operate", Digest: "sha256:op", HeldAt: heldAt,
		HeldClock: HoldingClock, Fixed: true, Approvers: sandbox.HeldApproversSSH, Act: string(delegated.KindSSHOperate),
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
	{"ADelegatesWordsReachTheRowWithNoSecretTheyCarried", testADelegatesWordsReachTheRowWithNoSecretTheyCarried},
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

// GrowthParked records turnID's run as parked on a held growth spend act,
// held at heldAt, which lapses a day after (sandbox.HeldApproversAdmin).
func GrowthParked(t *testing.T, s sandbox.PendingStore, turnID string, heldAt time.Time) sandbox.HeldCall {
	t.Helper()
	ctx := t.Context()
	launch := mustLaunched(t, s, run(turnID))
	held := sandbox.HeldCall{ID: "0190f3a8-0000-7000-8000-0000000000e1", LaunchID: launch,
		Server: "crewlet", Tool: "ads_raise_budget", Raw: "ads_raise_budget", Digest: "sha256:ads", HeldAt: heldAt,
		LapsesAt: heldAt.Add(sandbox.HeldLapse), Fixed: true, Approvers: sandbox.HeldApproversAdmin,
		Act: string(delegated.KindAdSpend), Binding: strings.Repeat("cd", 32)}
	sealed, err := heldSeal(t).SealArguments(turnID, held, map[string]any{"campaign": "c1", "daily_budget": "60.00"})
	if err != nil {
		t.Fatal(err)
	}
	held.Arguments = sealed
	const question = "May sre raise a campaign's daily budget?"
	ask := sandbox.Ask{Question: question, Decision: sandbox.HeldFixedDecision(question, nil),
		To: sandbox.AskToManager, Held: &held}
	if got, err := s.RecordAsk(ctx, turnID, launch, ask); err != nil || got != sandbox.AskRecorded {
		t.Fatalf("RecordAsk of a held growth act = %v, %v", got, err)
	}
	if err := s.MarkAwaiting(ctx, turnID, sandbox.Clarification{AskedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("MarkAwaiting: %v", err)
	}
	return held
}

// A DELEGATE'S WORDS REACH THE ROW WITH NO SECRET THEY CARRIED: its reason
// and findings, which people read before the call itself, hold a provider's
// key, a password's value and a bearer token, and the answer the write gives
// back, the row and the list of active runs all hold markers in their place,
// the words around them as written.
func testADelegatesWordsReachTheRowWithNoSecretTheyCarried(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	sample := map[string]string{}
	for _, shape := range redacttest.Shapes() {
		sample[shape.Name] = shape.Sample
	}
	planted := []struct{ text, value string }{
		{sample["anthropic-key"], sample["anthropic-key"]},
		{sample["password"], strings.TrimPrefix(sample["password"], "password: ")},
		{"Authorization: Bearer " + sample["jwt"], sample["jwt"]},
	}
	held := DelegatedParked(t, s, "t-words", []string{"security-a"}, base)
	v := verdictBy("security-a", "deny")
	v.Reason = "it echoes " + planted[0].text + " to the log"
	v.Findings = []string{"the script sets " + planted[1].text, "it sends " + planted[2].text, "nothing else"}
	written, ok, err := s.GiveDelegateVerdict(ctx, "t-words", held.LaunchID, held.ID, v)
	if err != nil || !ok {
		t.Fatalf("the delegate's answer = %v, %v; want it recorded", ok, err)
	}
	r, found, err := s.Get(ctx, "t-words")
	if err != nil || !found || r.HeldCall == nil || r.HeldCall.Delegation == nil {
		t.Fatalf("the row reads %+v, %v, %v", r.HeldCall, found, err)
	}
	active, err := s.ListActive(ctx)
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	reads := map[string]*sandbox.HeldDelegation{"the answer written": written.Delegation,
		"the row": r.HeldCall.Delegation}
	for _, a := range active {
		if a.TurnID == "t-words" && a.HeldCall != nil {
			reads["the active list"] = a.HeldCall.Delegation
		}
	}
	if len(reads) != 3 {
		t.Fatalf("the active list holds no held call of t-words")
	}
	for where, d := range reads {
		if d == nil || len(d.Given) != 1 || len(d.Given[0].Findings) != 3 {
			t.Fatalf("%s holds %+v; want the one answer with its three findings", where, d)
		}
		g := d.Given[0]
		words := append([]string{g.Reason}, g.Findings...)
		for i, p := range planted {
			text := words[i]
			if strings.Contains(text, p.value) || !strings.Contains(text, redact.Marker) {
				t.Errorf("%s holds %q; want a marker in place of the secret", where, text)
			}
		}
		if !strings.HasPrefix(g.Reason, "it echoes ") || !strings.HasSuffix(g.Reason, " to the log") ||
			g.Findings[2] != "nothing else" {
			t.Errorf("%s changed the delegate's other words: %q, %q", where, g.Reason, g.Findings)
		}
		if g.By != "security-a" || g.Verdict != "deny" || g.Confidence != string(delegated.High) {
			t.Errorf("%s changed who answered or what: %+v", where, g)
		}
	}
	if !strings.Contains(v.Reason, planted[0].value) || !strings.Contains(v.Findings[1], planted[2].value) {
		t.Errorf("recording the answer changed the caller's copy: %+v", v)
	}
}
