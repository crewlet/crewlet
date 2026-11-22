package sandbox

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/redact/redacttest"
	"github.com/crewlet/crewlet/internal/secrets"
)

// parksOnAHeldCall records a call to a tool at Needs approval as turnID's
// job's question, its arguments sealed, and parks the run on it, answering the
// held call's id.
func parksOnAHeldCall(t *testing.T, rig *coordRig, turnID string) string {
	t.Helper()
	cipher, err := secrets.NewCipher(secrets.Keyring{ActiveID: "k1", Keys: map[string][]byte{"k1": make([]byte, 32)}})
	if err != nil {
		t.Fatal(err)
	}
	run := rig.get(turnID)
	held := HeldCall{ID: "0190f3a8-0000-7000-8000-000000000001", LaunchID: run.LaunchID, Server: "tracker",
		Tool: "tr_close_issue", Raw: "close_issue", Digest: "sha256:d1", HeldAt: time.Now().UTC()}
	if held.Arguments, err = NewCallSeal(cipher).SealArguments(turnID, held, map[string]any{"id": "7"}); err != nil {
		t.Fatal(err)
	}
	const question = "May swe call tr_close_issue on the tracker MCP server?"
	rig.ask(turnID, Ask{Question: question, Decision: HeldDecision(question, nil), To: AskToManager, Held: &held})
	rig.runner.Finish(Result{})
	payload, ev := rig.completion(turnID)
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	if got := rig.get(turnID); got.Status != StatusAwaiting || got.HeldCall == nil {
		t.Fatalf("the run is %q holding %+v, want it parked on its held call", got.Status, got.HeldCall)
	}
	return held.ID
}

// A CHAT REPLY NEVER ANSWERS A HELD CALL, AND AN ANSWER BY TURN ONLY BY ITS ID
// (ADR-0077). Speaking on the run's conversation is no grant to approve a tool
// call, so a reply there is an ordinary message and the run waits on. An answer
// by turn not naming the held call is spent unrecorded, as is one naming a held
// call for a question that is not one; the approval naming it is recorded.
//
// Mutation: drop the held-call check from ConversationRef.Answers, and the
// chat reply approves the call.
func TestAChatReplyNeverAnswersAHeldCall(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.coordinator.countRun("swe", StatusRunning)
	id := parksOnAHeldCall(t, rig, "t1")

	d, err := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe", chatReply(answerOnTheDM, "once", nil))
	if err != nil || d != AnswerNotMine {
		t.Fatalf("a chat reply to a held call = %q, %v, want not mine", d, err)
	}
	if got := rig.get("t1"); got.Answer != nil || got.Status != StatusAwaiting {
		t.Fatalf("a chat reply was recorded as the held call's answer: %+v", got.Answer)
	}

	given, _ := givenAgainst(t, rig, "t1")
	given.Choice = HeldOnce
	if d, err := rig.coordinator.AnswerByTurn(t.Context(), given, events.New(given, events.TraceContext{})); err != nil ||
		d != AnswerNotMine {
		t.Fatalf("an answer by turn not naming the held call = %q, %v, want not mine", d, err)
	}
	if got := rig.get("t1"); got.Answer != nil {
		t.Fatal("an answer by turn not naming the held call was recorded")
	}
	records := rig.answeredRecords()
	if len(records) != 1 || records[0].Outcome != types.AnswerUnoffered {
		t.Fatalf("recorded %+v, want one unoffered", records)
	}

	given.Approval = id
	if d, err := rig.coordinator.AnswerByTurn(t.Context(), given, events.New(given, events.TraceContext{})); err != nil ||
		d != AnswerConsumed {
		t.Fatalf("the approval naming the held call = %q, %v, want consumed", d, err)
	}
}

// A GROWTH ACT PAST ITS LAPSE TAKES DENY ALONE, whichever node's route let an
// allow through: the seat spends it unrecorded, so nothing is made on figures
// a day old, and records a deny.
//
// Mutation: drop the lapse check from AnswerByTurn, and the late allow is
// recorded.
func TestALapsedGrowthActIsNeverAllowed(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.coordinator.countRun("swe", StatusRunning)
	cipher, err := secrets.NewCipher(secrets.Keyring{ActiveID: "k1", Keys: map[string][]byte{"k1": make([]byte, 32)}})
	if err != nil {
		t.Fatal(err)
	}
	run := rig.get("t1")
	heldAt := rig.now.Add(-HeldLapse - time.Minute)
	held := HeldCall{ID: "0190f3a8-0000-7000-8000-0000000000e1", LaunchID: run.LaunchID, Server: "crewlet",
		Tool: "ads_raise_budget", Raw: "ads_raise_budget", Digest: "sha256:ads", HeldAt: heldAt,
		LapsesAt: heldAt.Add(HeldLapse), Fixed: true, Approvers: HeldApproversAdmin, Act: "ad_spend",
		Binding: strings.Repeat("cd", 32)}
	if held.Arguments, err = NewCallSeal(cipher).SealArguments("t1", held, map[string]any{"daily_budget": "60"}); err != nil {
		t.Fatal(err)
	}
	const question = "May swe raise a campaign's daily budget?"
	rig.ask("t1", Ask{Question: question, Decision: HeldFixedDecision(question, nil), To: AskToManager, Held: &held})
	rig.runner.Finish(Result{})
	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	given, _ := givenAgainst(t, rig, "t1")
	given.Choice, given.Approval = HeldOnce, held.ID
	if d, err := rig.coordinator.AnswerByTurn(t.Context(), given, events.New(given, events.TraceContext{})); err != nil ||
		d != AnswerNotMine {
		t.Fatalf("a late allow = %q, %v, want not mine", d, err)
	}
	if got := rig.get("t1"); got.Answer != nil {
		t.Fatalf("a late allow was recorded: %+v", got.Answer)
	}
	given.Choice = HeldDeny
	if d, err := rig.coordinator.AnswerByTurn(t.Context(), given, events.New(given, events.TraceContext{})); err != nil ||
		d != AnswerConsumed {
		t.Fatalf("a late deny = %q, %v, want consumed", d, err)
	}
}

// AN APPROVAL NAMES A HELD CALL, so one naming a held call answers no question
// that is not one: spent unrecorded on a run parked on its own question.
func TestAnApprovalAnswersNoQuestionThatIsNotAHeldCall(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	launchWithNoConversation(t, rig, "t1")
	parksOnAQuestion(t, rig, "t1")
	given, _ := givenAgainst(t, rig, "t1")
	given.Answer, given.Approval = "go", "0190f3a8-0000-7000-8000-000000000001"
	ev := events.New(given, events.TraceContext{})
	if d, err := rig.coordinator.AnswerByTurn(t.Context(), given, ev); err != nil || d != AnswerNotMine {
		t.Fatalf("an approval to a run's own question = %q, %v, want not mine", d, err)
	}
	if got := rig.get("t1"); got.Answer != nil {
		t.Fatal("an approval was recorded as the answer to a question that holds no call")
	}
}

// A QUESTION ABOUT A HELD CALL IS ANNOUNCED AS ONE, with the job it pins and
// the call it holds, never the arguments: a live surface offers Allow and Deny
// from the announcement, where the run's own answer would be refused. A
// question about no call announces none.
//
// Mutation: leave the call or the launch off the announcement, or put the
// sealed arguments on it, and this fails.
func TestAHeldCallIsAnnouncedWithItsQuestion(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.coordinator.countRun("swe", StatusRunning)
	id := parksOnAHeldCall(t, rig, "t1")
	asked := rig.questions()
	if len(asked) != 1 {
		t.Fatalf("announced %d questions, want 1", len(asked))
	}
	got := asked[0]
	if got.HeldCall == nil || got.HeldCall.ID != id || got.HeldCall.Server != "tracker" ||
		got.HeldCall.Tool != "tr_close_issue" || got.HeldCall.HeldAt.IsZero() {
		t.Fatalf("the announcement holds %+v, want the held call", got.HeldCall)
	}
	if run := rig.get("t1"); got.LaunchID == "" || got.LaunchID != run.LaunchID {
		t.Errorf("the announcement names job %q, want the run's %q", got.LaunchID, run.LaunchID)
	}

	plain := newCoordRig(t)
	launchWithNoConversation(t, plain, "t2")
	parksOnAQuestion(t, plain, "t2")
	if asked := plain.questions(); len(asked) != 1 || asked[0].HeldCall != nil || asked[0].LaunchID == "" {
		t.Errorf("a question about no call announced %+v, want its job and no held call", asked)
	}
}

// A HELD CALL IS OPEN FROM ITS RECORDING TO ITS ANSWER, which is what a seat's
// repeat of the same request joins (ADR-0125): open before the job has parked
// and while the run waits, and no longer once an answer is recorded. A run
// parked on its own question holds no call to join.
//
// Mutation: count only a parked run, and a second turn asking in the seconds
// before the first one parks makes a second request; count an answered one,
// and an ask after a denial waits on an answer already given.
func TestAHeldCallIsOpenFromItsRecordingToItsAnswer(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.coordinator.countRun("swe", StatusRunning)
	if rig.get("t1").HeldOpen() {
		t.Fatal("a run that held no call reads as holding an open one")
	}
	cipher, err := secrets.NewCipher(secrets.Keyring{ActiveID: "k1", Keys: map[string][]byte{"k1": make([]byte, 32)}})
	if err != nil {
		t.Fatal(err)
	}
	held := HeldCall{ID: "0190f3a8-0000-7000-8000-000000000002", LaunchID: rig.get("t1").LaunchID,
		Server: "crewlet", Tool: "ask_to_merge", Raw: "ask_to_merge", Digest: "sha256:d1", Fixed: true,
		Subject: strings.Repeat("ab", 32), HeldAt: time.Now().UTC()}
	if held.Arguments, err = NewCallSeal(cipher).SealArguments("t1", held, map[string]any{"change": "web!42"}); err != nil {
		t.Fatal(err)
	}
	const question = "May swe merge web!42 at a1a1a1a1a1a1?"
	rig.ask("t1", Ask{Question: question, Decision: HeldFixedDecision(question, nil), To: AskToManager, Held: &held})
	if got := rig.get("t1"); !got.HeldOpen() || got.HeldCall.Subject != held.Subject {
		t.Fatalf("a call just recorded, its job not yet parked, is %q holding %+v, want it open with its subject",
			got.Status, got.HeldCall)
	}
	rig.runner.Finish(Result{})
	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	if got := rig.get("t1"); got.Status != StatusAwaiting || !got.HeldOpen() {
		t.Fatalf("the parked run is %q, open %v, want it waiting and open", got.Status, got.HeldOpen())
	}
	given, _ := givenAgainst(t, rig, "t1")
	given.Choice, given.Approval = HeldDeny, held.ID
	if d, err := rig.coordinator.AnswerByTurn(t.Context(), given, events.New(given, events.TraceContext{})); err != nil ||
		d != AnswerConsumed {
		t.Fatalf("the denial = %q, %v, want consumed", d, err)
	}
	if got := rig.get("t1"); got.HeldOpen() {
		t.Fatalf("an answered call still reads as open: the run is %q", got.Status)
	}

	launchWithNoConversation(t, rig, "t2")
	parksOnAQuestion(t, rig, "t2")
	if rig.get("t2").HeldOpen() {
		t.Fatal("a run parked on its own question reads as holding an open call")
	}
}

// A HELD CALL IS IN FLIGHT UNTIL ITS ANSWER IS CARRIED OUT, read off the row
// the store keeps at each step: answered and not yet taken, claimed for its
// answer, and allowed and being made, a repeat of the same request joins it;
// once made, or once a denial was taken by the turn that asked, it is over
// (ADR-0125). A second request asked while the first was being carried out
// reached its delegate after the first was made.
//
// Mutation: end the flight at the answer (HeldOpen's reading), and the
// answered, claimed and calling steps read as over.
func TestAHeldCallIsInFlightUntilItsAnswerIsCarriedOut(t *testing.T) {
	t.Parallel()
	for _, choice := range []string{HeldOnce, HeldDeny} {
		rig := newCoordRig(t)
		rig.launch("t1")
		rig.coordinator.countRun("swe", StatusRunning)
		id := parksOnAHeldCall(t, rig, "t1")
		step := func(name string, want bool) {
			t.Helper()
			if got := rig.get("t1"); got.HeldInFlight() != want {
				t.Fatalf("%s, %s: the run is %q, in flight %v, want %v", choice, name, got.Status,
					got.HeldInFlight(), want)
			}
		}
		step("parked", true)
		given, ev := givenAgainst(t, rig, "t1")
		given.Choice, given.Approval = choice, id
		if d, err := rig.coordinator.AnswerByTurn(t.Context(), given, ev); err != nil || d != AnswerConsumed {
			t.Fatalf("%s: the answer = %q, %v, want consumed", choice, d, err)
		}
		if rig.get("t1").HeldOpen() {
			t.Fatalf("%s: an answered call reads as open", choice)
		}
		step("answered", true)
		launch := rig.get("t1").LaunchID
		if _, won, err := rig.pending.ClaimForResume(t.Context(), "t1", RecordedAnswerTail(launch), rigLease); err != nil || !won {
			t.Fatalf("%s: the claim = %v, %v", choice, won, err)
		}
		step("claimed for its answer", true)
		if took, err := rig.pending.TakeAnswer(t.Context(), "t1", launch, rigLease); err != nil || !took {
			t.Fatalf("%s: taking the answer = %v, %v", choice, took, err)
		}
		if choice == HeldDeny {
			step("its denial taken", false)
			continue
		}
		if _, first, err := rig.pending.MarkHeldCall(t.Context(), "t1", launch, rigLease); err != nil || !first {
			t.Fatalf("marking the call = %v, %v", first, err)
		}
		step("being made", true)
		if _, err := rig.pending.RecordHeldResult(t.Context(), "t1", launch, HeldMade{Result: "sealed",
			Call: BridgeCall{Name: "tr_close_issue"}}, rigLease); err != nil {
			t.Fatalf("recording the result: %v", err)
		}
		step("made", false)
	}
}

// A HELD CALL'S ARGUMENTS OPEN REDACTED UNLESS ASKED FOR RAW. Every reader but
// who consents to or makes the pending call reads OpenArguments, so a key in a
// string, a password under its key and a provider token come back as markers
// there, the rest as it was; OpenRawArguments gives back the bytes the call
// would send, which the approval's digest is of.
//
// Mutation: return the opened arguments from OpenArguments without redact.Args,
// and the secrets come back from it.
func TestAHeldCallsArgumentsOpenRedactedUnlessAskedRaw(t *testing.T) {
	t.Parallel()
	sample := map[string]string{}
	for _, s := range redacttest.Shapes() {
		sample[s.Name] = s.Sample
	}
	key, token := sample["anthropic-key"], sample["github-token"]
	password := strings.TrimPrefix(sample["password"], "password: ")
	cipher, err := secrets.NewCipher(secrets.Keyring{ActiveID: "k1", Keys: map[string][]byte{"k1": make([]byte, 32)}})
	if err != nil {
		t.Fatal(err)
	}
	seal := NewCallSeal(cipher)
	held := HeldCall{ID: "0190f3a8-0000-7000-8000-000000000009", LaunchID: "l1", Server: "ops", Raw: "run",
		Digest: "sha256:d1"}
	sent := map[string]any{"command": "curl -H 'x-api-key: " + key + "' https://example.com",
		"password": password, "token": token, "path": "internal/a.go"}
	if held.Arguments, err = seal.SealArguments("t1", held, sent); err != nil {
		t.Fatal(err)
	}
	raw, err := seal.OpenRawArguments("t1", held)
	if err != nil {
		t.Fatal(err)
	}
	wantDigest, _ := HeldArgumentsDigest(sent)
	if got, _ := HeldArgumentsDigest(raw); got != wantDigest || raw["password"] != password {
		t.Errorf("the raw arguments are %v, want the ones the call would send", raw)
	}
	shown, err := seal.OpenArguments("t1", held)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{key, password, token} {
		for name, v := range shown {
			if s, _ := v.(string); strings.Contains(s, secret) {
				t.Errorf("the arguments shown hold a secret under %q: %v", name, shown)
			}
		}
	}
	if shown["path"] != "internal/a.go" || !strings.Contains(shown["command"].(string), redact.Marker) {
		t.Errorf("the arguments shown are %v, want the secrets as markers and the rest as it was", shown)
	}
}

// A HELD CALL'S ARGUMENTS OPEN AS SENT ONLY TO WHO MAY ANSWER IT WHILE IT
// WAITS, with the digest Allow once names; a reader who may not, and every
// reader once the call is answered, get them redacted and no digest.
//
// Mutation: drop either arm of OpenArgumentsFor's rule, and a case reads the
// token.
func TestAHeldCallsArgumentsOpenAsSentOnlyToWhoMayAnswerItWhileItWaits(t *testing.T) {
	t.Parallel()
	var token string
	for _, s := range redacttest.Shapes() {
		if s.Name == "github-token" {
			token = s.Sample
		}
	}
	cipher, err := secrets.NewCipher(secrets.Keyring{ActiveID: "k1", Keys: map[string][]byte{"k1": make([]byte, 32)}})
	if err != nil {
		t.Fatal(err)
	}
	seal := NewCallSeal(cipher)
	held := HeldCall{ID: "0190f3a8-0000-7000-8000-00000000000a", LaunchID: "l1", Server: "ops", Raw: "run",
		Digest: "sha256:d1"}
	sent := map[string]any{"command": "deploy --token " + token}
	if held.Arguments, err = seal.SealArguments("t1", held, sent); err != nil {
		t.Fatal(err)
	}
	waiting := PendingRun{TurnID: "t1", LaunchID: "l1", Status: StatusAwaiting, HeldCall: &held,
		Question: "May swe run it?"}
	answered := waiting
	answered.Status, answered.Answer = StatusResumed, &RecordedAnswer{RecordedAt: time.Now().UTC()}
	wantDigest, _ := HeldArgumentsDigest(sent)
	for _, c := range []struct {
		name      string
		run       PendingRun
		mayAnswer bool
		raw       bool
	}{
		{"who may answer it, while it waits", waiting, true, true},
		{"who may not answer it, while it waits", waiting, false, false},
		{"who may answer it, once answered", answered, true, false},
		{"who may not answer it, once answered", answered, false, false},
	} {
		args, digest, err := seal.OpenArgumentsFor(c.run, c.mayAnswer)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		command, _ := args["command"].(string)
		switch {
		case c.raw && (command != sent["command"] || digest != wantDigest):
			t.Errorf("%s reads %q with digest %q, want it as sent with its digest", c.name, command, digest)
		case !c.raw && (strings.Contains(command, token) || !strings.Contains(command, redact.Marker) || digest != ""):
			t.Errorf("%s reads %q with digest %q, want the token as a marker and no digest", c.name, command, digest)
		}
	}
}
