package sandbox

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/recordseal"
)

// A reply too large to be its run's answer, recorded as its question's refusal
// and resumed like an answer (ADR-0114).

// refusingRig is a rig whose coordinator reads back the deliveries it recorded
// as worked, as a node's reads its completion ledger
// ([CoordinatorOptions.WasSpent]): what decides whether a refused reply's
// delivery is kept or spent.
func refusingRig(t *testing.T) *coordRig {
	t.Helper()
	var rig *coordRig
	rig = newCoordRigWith(t, func(o *CoordinatorOptions) {
		o.WasSpent = func(_ context.Context, _ string, ev *events.Event) bool {
			return rig.spentOf(ev.ID.String())
		}
	})
	return rig
}

// spentOf reports whether the coordinator recorded delivery id as worked.
func (r *coordRig) spentOf(id string) bool {
	return slices.ContainsFunc(r.spentDeliveries(), func(s spentAt) bool { return s.id == id })
}

// tooLargeReply is one chat message past what a run's row carries as an answer
// (a page of markup, each `<`, `>` and `&` six bytes as the row holds it),
// posted a minute after the question rig's run parked on.
func tooLargeReply(rig *coordRig) Reply {
	text := strings.Repeat("<li>a & b</li>", MaxAnswerBytes/14)
	return chatReply(answerOnTheDM, text, replyAt(text, rig.get("t1").AskedAt.Add(time.Minute)))
}

// tooLargeRecords is every executor_answered too_large the coordinator
// published, as the events they were published as.
func (r *coordRig) tooLargeRecords() []*events.Event {
	r.queue.mu.Lock()
	defer r.queue.mu.Unlock()
	var out []*events.Event
	for _, p := range r.queue.published {
		if payload, ok := p.event.Data.(*types.ExecutorAnswered); ok && payload.Outcome == types.AnswerTooLarge {
			out = append(out, p.event)
		}
	}
	return out
}

// A REPLY PAST WHAT THE ROW CARRIES IS RECORDED AS ITS QUESTION'S REFUSAL, AND
// THE RUN IS RESUMED TOLD SO. A reply is recorded on the run's row as its
// answer, and one past its share (MaxAnswerBytes) cannot be: its refusal is
// recorded in the answer's place instead — the reply's delivery, its figures,
// none of its words — and resumed as an answer is, the run handed the refusal
// where an answer would be ([AskedRun.Refused]) and never an answer with no
// words. The refusal is announced once, `executor_answered` with the outcome
// too_large, and never as `resumed`, which a person's answered decisions read
// as an answer. The turn that took it kept it, so the delivery the seat's
// inbox held for it is spent, and the delivery is acknowledged.
//
// Mutation: hand the reply on as the ordinary message it looks like, as the
// coordinator did, and no run is resumed; resume with the refusal's words as an
// answer, and the run is handed an empty answer and no refusal; announce the
// resume, and a `resumed` record reads it as an answered decision.
func TestAReplyPastWhatTheRowCarriesIsRecordedAsItsQuestionsRefusal(t *testing.T) {
	t.Parallel()
	rig := refusingRig(t)
	parkOnAQuestion(t, rig)
	reply := tooLargeReply(rig)
	disposition, err := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe", reply)
	if err != nil || disposition != AnswerConsumed {
		t.Fatalf("a reply past a recorded answer's share = %q, %v; want %q: its refusal taken and kept "+
			"by the resumed turn", disposition, err, AnswerConsumed)
	}
	calls := rig.resumer.calls()
	if len(calls) != 1 {
		t.Fatalf("%d resumes ran, want the run resumed once with the refusal", len(calls))
	}
	asked := askedOf(calls[0])
	switch {
	case asked.Refused == nil:
		t.Fatalf("the run was resumed with %+v, want the refusal where its answer would be", asked)
	case asked.Answer != "" || asked.By != "" || asked.Label != nil:
		t.Errorf("the run was handed an answer beside its refusal: %q by %q", asked.Answer, asked.By)
	case asked.Refused.Messages() != 1 || asked.Refused.Bytes <= MaxAnswerBytes ||
		asked.Refused.MaxBytes != MaxAnswerBytes:
		t.Errorf("the run is told %+v, want one message past the %d an answer may", *asked.Refused,
			MaxAnswerBytes)
	case asked.Question != "which branch?":
		t.Errorf("the run is told it asked %q", asked.Question)
	}
	resumed := calls[0].Run
	if refusal, refused := resumed.Refused(); !refused || !slices.Equal(refusal.EventIDs, reply.ids()) ||
		resumed.Answer == nil || len(resumed.Answer.Events) != 0 || !resumed.Answer.PostedAt.Equal(reply.Posted()) {
		t.Errorf("the row the run was resumed from holds %+v beside %+v; want the refusal of the reply's "+
			"delivery, posted when it was, with none of its bytes", resumed.AnswerRefusal, resumed.Answer)
	}
	if !rig.spentOf(reply.Events[0].ID.String()) {
		t.Error("the delivery the refusal was recorded from is not spent once a turn kept the refusal")
	}
	told := rig.answeredRecords()
	if len(told) != 1 {
		t.Fatalf("announced %+v, want the refusal recorded once and nothing else", told)
	}
	if got := told[0]; got.Outcome != types.AnswerTooLarge || got.Via != types.AnswerViaChat ||
		got.TurnID != "t1" || got.AnsweredBy == "" || got.AnswerMaxBytes != MaxAnswerBytes ||
		got.AnswerBytes != asked.Refused.Bytes || got.Answer != "" {
		t.Errorf("the refusal was recorded as %+v, want %q on the chat route, by its sender, at the "+
			"figures the run is told, with none of its words", got, types.AnswerTooLarge)
	}
}

// A MESSAGE AFTER A REFUSED REPLY IS NOT THE RUN'S ANSWER. From the refusal's
// record on, while the refusal is the run's, the question no longer waits: the
// person's next short message ("any luck?") is the ordinary message it is, and
// never the run's answer — which it was, while a refused reply left the
// question waiting with its old anchor. The refusal stays the question's
// outcome, owed its resume, and the refused reply's own delivery is kept, not
// spent, while no turn has kept it. (A refusal LET GO OF puts the question
// back to waiting, as an answer let go of does:
// TestARefusalLetGoOfLeavesItsQuestionToTheNextReply.)
//
// Mutation: leave the question waiting on a refusal, as the store did, and
// "any luck?" is recorded as the run's answer.
func TestAMessageAfterARefusedReplyIsNotTheRunsAnswer(t *testing.T) {
	t.Parallel()
	rig := refusingRig(t)
	parkOnAQuestion(t, rig)
	rig.resumer.failWith(errors.New("the node is draining"))
	reply := tooLargeReply(rig)
	if d, err := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe", reply); err != nil || d != AnswerRefused {
		t.Fatalf("the refused reply = %q, %v; want %q, kept while its refusal is owed", d, err, AnswerRefused)
	}
	later := replyAt("any luck?", reply.Posted().Add(time.Minute))
	d, err := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe", chatReply(answerOnTheDM, "any luck?", later))
	if err != nil || d != AnswerNotMine {
		t.Fatalf("a message after the refused reply = %q, %v; want %q, the ordinary message it is", d, err,
			AnswerNotMine)
	}
	got := rig.get("t1")
	if refusal, refused := got.Refused(); !refused || !slices.Equal(refusal.EventIDs, reply.ids()) ||
		got.Status != StatusAnswered {
		t.Fatalf("the run is %s holding %+v; want the refusal still its question's outcome", got.Status,
			got.Answer)
	}
	if rig.spentOf(reply.Events[0].ID.String()) {
		t.Error("the refused reply's delivery is spent though no turn took its refusal")
	}
}

// A REFUSAL LET GO OF SENDS THE REPLY BACK AS IT CAME. A refusal no attempt can
// resume is let go of after its attempts, as an answer is: the run waits on its
// question again with the reply declined for it. The row carried none of the
// reply, so nothing is handed back from it; the reply's delivery, kept on the
// seat's inbox and never spent, comes round as the ordinary message it is. And
// the let-go is announced, once, after the refusal's own record — `declined`
// on the chat route with the refusal's figures, by whom the seal names — since
// that record said the run would be told, and it never was.
//
// Mutation: spend the refused reply's delivery when its refusal is let go of,
// as an answer's is before its copies are handed back, and the reply is
// dropped as worked with nothing to hand back; announce nothing of the let-go,
// as a chat answer let go of announces nothing, and the feed keeps "is resumed
// told" for a run waiting on its question again.
func TestARefusalLetGoOfSendsTheReplyBackAsItCame(t *testing.T) {
	t.Parallel()
	rig := refusingRig(t)
	holds := rig.withHold()
	parkOnAQuestion(t, rig)
	rig.resumer.failWith(errors.New("this build cannot read the suspended conversation"))
	reply := tooLargeReply(rig)
	if d, err := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe", reply); err != nil || d != AnswerRefused {
		t.Fatalf("the refused reply = %q, %v; want %q", d, err, AnswerRefused)
	}
	if !holds.holding("swe") {
		t.Fatal("the seat's inbox is not held behind the refusal its run is owed")
	}
	for range MaxAnswerAttempts - 1 {
		if rig.fireRetries() != 1 {
			t.Fatal("the refusal's resume was not retried on its own schedule")
		}
	}
	got := rig.get("t1")
	if got.Status != StatusAwaiting || got.Answer != nil || got.AnswerRefusal != nil ||
		!slices.Contains(got.DeclinedAnswers, reply.ids()[0]) {
		t.Fatalf("the run is %s holding %+v and %+v, declined %v; want it waiting with the reply declined",
			got.Status, got.Answer, got.AnswerRefusal, got.DeclinedAnswers)
	}
	if rig.spentOf(reply.ids()[0]) {
		t.Fatal("the refused reply's delivery was spent, so the reply is dropped as worked")
	}
	if handed := rig.handedBack(); len(handed) != 0 {
		t.Errorf("handed back %v from a row that carried none of the reply", handed)
	}
	if holds.holding("swe") {
		t.Error("the seat's inbox is still held after the refusal was let go of")
	}
	if d, err := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe", reply); err != nil || d != AnswerNotMine {
		t.Fatalf("the kept delivery coming round = %q, %v; want %q, the ordinary message it is", d, err,
			AnswerNotMine)
	}
	told := rig.answeredRecords()
	if len(told) != 2 || told[0].Outcome != types.AnswerTooLarge {
		t.Fatalf("announced %+v, want the refusal's record and then its let-go", told)
	}
	if let := told[1]; let.Outcome != types.AnswerDeclined || let.Via != types.AnswerViaChat ||
		let.AnsweredBy != told[0].AnsweredBy || let.AnswerBytes != told[0].AnswerBytes ||
		let.AnswerMaxBytes != MaxAnswerBytes || let.Answer != "" {
		t.Errorf("the let-go was announced as %+v, want %q on the chat route, by the reply's sender, "+
			"at the refusal's figures and with none of its words", let, types.AnswerDeclined)
	}
}

// A REFUSAL LET GO OF LEAVES ITS QUESTION TO THE NEXT REPLY. The guarantee that
// nothing written after a refusal is the run's answer holds while the refusal
// is the run's. Let go of after its attempts — no resume could tell the run —
// the question waits again under the anchor it was asked at, as one whose
// answer was let go of does, and the next reply that qualifies answers it: the
// person's "any luck?" written since included, which then carries the run on
// as its answer. The ADR and the docs say so beside the guarantee.
//
// THE CLOCK MOVES ON after the question is asked, so a let-go that anchored
// it again would anchor it at another instant, where the rig's clock left
// standing read the two equal.
//
// Mutation: close the question for good once it has let a reply go (WaitsOn
// false while DeclinedAnswers holds any), and the person's next reply is an
// ordinary message, the run waiting on a question nothing can answer; anchor
// the question again as the refusal is let go of, and it was not asked then.
func TestARefusalLetGoOfLeavesItsQuestionToTheNextReply(t *testing.T) {
	t.Parallel()
	rig := refusingRig(t)
	parkOnAQuestion(t, rig)
	asked := rig.get("t1").AskedAt
	rig.now = rig.now.Add(time.Second)
	rig.resumer.failWith(errors.New("this build cannot read the suspended conversation"))
	reply := tooLargeReply(rig)
	if d, err := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe", reply); err != nil || d != AnswerRefused {
		t.Fatalf("the refused reply = %q, %v; want %q", d, err, AnswerRefused)
	}
	for range MaxAnswerAttempts - 1 {
		rig.fireRetries()
	}
	if got := rig.get("t1"); got.Status != StatusAwaiting || !got.AskedAt.Equal(asked) {
		t.Fatalf("the let-go left the run %s asked at %s; want it waiting on the question asked at %s",
			got.Status, got.AskedAt, asked)
	}
	rig.resumer.failWith(nil)
	later := replyAt("any luck?", reply.Posted().Add(time.Minute))
	d, err := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe", chatReply(answerOnTheDM, "any luck?", later))
	if err != nil || d != AnswerConsumed {
		t.Fatalf("a message after the let-go = %q, %v; want %q, the question's answer", d, err, AnswerConsumed)
	}
	calls := rig.resumer.calls()
	if len(calls) != 1 || askedOf(calls[0]).Answer != "any luck?" || askedOf(calls[0]).Refused != nil {
		t.Errorf("the run was resumed %d times, with %+v; want once, with the next reply as its answer",
			len(calls), calls)
	}
}

// A REFUSAL THE INLINE ATTEMPT LET GO OF IS THE ORDINARY MESSAGE AT ONCE. The
// inline attempt at a refusal's resume can let the refusal go before any turn
// takes it — here a resume that broke before its turn, which ends the run — and
// what becomes of the reply's delivery is decided from the row as the attempt
// left it: the refusal has left the run unspent, so the delivery is the
// ordinary message it is, handed on now rather than deferred as kept for a
// refusal no run holds (which stopped the seat's inbox until its next renew and
// spent one of the delivery's deliveries for nothing). And the run's ending
// says that what goes back to the seat is a reply too large for it, which it
// was never told of — not "the answer a person gave it".
//
// Mutation: decide off the snapshot the reply was matched against, as
// refusedReply did, and the delivery comes back AnswerRefused; announce the
// ending as one that hands back an answer, and it names an answer the run never
// had.
func TestARefusalTheInlineAttemptLetGoOfIsTheOrdinaryMessageAtOnce(t *testing.T) {
	t.Parallel()
	rig := refusingRig(t)
	parkOnAQuestion(t, rig)
	rig.resumer.failWith(fmt.Errorf("%w: a panic re-entering the conversation", ErrResumeAbandoned))
	rig.resumer.beforeTurn = true
	reply := tooLargeReply(rig)
	d, err := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe", reply)
	if err != nil || d != AnswerNotMine {
		t.Fatalf("a refused reply whose refusal the inline attempt let go of = %q, %v; want %q, the "+
			"ordinary message it is", d, err, AnswerNotMine)
	}
	if run, found, err := rig.pending.Get(t.Context(), "t1"); err != nil || (found && run.AnswerRefusal != nil) {
		t.Fatalf("the run still holds %+v (found %v, %v); want the refusal gone from it", run.AnswerRefusal,
			found, err)
	}
	if rig.spentOf(reply.ids()[0]) {
		t.Error("the reply's delivery was spent, so the ordinary message it is would be dropped as worked")
	}
	rig.finished("t1")
	got := announcedOnce(t, rig, types.ExecutorFailureResumeBroken)
	if got[0].failed.Detail != resumeBrokenDetail+refusedReturnedDetail {
		t.Errorf("the ending says %q, want it saying the refused reply goes back to the seat",
			got[0].failed.Detail)
	}
}

// A REFUSAL'S RESUME IS RAISED WHERE THE REPLY WAS POSTED, AND CARRIES NONE OF
// ITS WORDS. A resumed turn raises its working indicator off its trigger's
// transport metadata, for the person who replied; an answer's resume reads it
// off the deliveries the answer carries, and a refusal carries none of them. So
// the refusal keeps the reply's address (AnswerRefusal.Address) and its resume
// is triggered by a notification carrying that address and no body, subject or
// salient text: on every attempt, the inline one and a retry alike.
//
// Mutation: raise the refusal's resume off answer.trigger(), as an answer's,
// and it carries no trigger, so no indicator is raised; keep no address on the
// refusal (addressOf answering nil), and the same.
func TestARefusalsResumeIsRaisedWhereTheReplyWasPosted(t *testing.T) {
	t.Parallel()
	address := map[string]string{"transport": "slack", "channel": "D0ANA", "thread_ts": "1712345678.000100"}
	for name, inline := range map[string]bool{"the inline attempt": true, "a retry": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig := refusingRig(t)
			parkOnAQuestion(t, rig)
			if !inline {
				rig.resumer.failWith(errors.New("the node is draining"))
			}
			reply := tooLargeReply(rig)
			n, _ := events.DataAs[*types.ExternalNotification](reply.Events[0])
			n.Metadata = address
			if _, err := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe", reply); err != nil {
				t.Fatalf("the refused reply: %v", err)
			}
			if !inline {
				rig.resumer.failWith(nil)
				if rig.fireRetries() != 1 {
					t.Fatal("the refusal's resume was not retried")
				}
			}
			calls := rig.resumer.calls()
			if len(calls) != 1 {
				t.Fatalf("%d resumes reached a turn, want the refusal's one", len(calls))
			}
			trigger := calls[0].Trigger
			got, ok := events.DataAs[*types.ExternalNotification](trigger)
			switch {
			case !ok || got == nil:
				t.Fatalf("the resume carried trigger %+v, want a notification raised where the reply "+
					"was posted", trigger)
			case !maps.Equal(got.Metadata, address):
				t.Errorf("the resume is raised at %v, want the reply's address %v", got.Metadata, address)
			case got.Body != "" || got.SalientBody != nil || got.Subject != "" || got.Sender != "":
				t.Errorf("the resume's trigger carries the reply's words: %+v", got)
			case trigger.ID != reply.Events[0].ID || !trigger.Timestamp.Equal(reply.Posted()):
				t.Errorf("the resume's trigger is %s at %s, want the reply's own delivery %s at %s",
					trigger.ID, trigger.Timestamp, reply.Events[0].ID, reply.Posted())
			}
		})
	}
}

// A REFUSAL A TURN GAVE BACK KEEPS ITS REPLY. A turn takes the refusal and then
// gives its claim back as a retry, which says nothing it did reached anybody:
// the refusal is owed again and may yet be let go of, so its delivery — the
// only copy of the reply — is not spent at the take, as an answer's is (its
// row carries copies), but only once a turn that took it keeps it.
//
// Mutation: spend the refusal's delivery at the take, and it is recorded as
// worked by a turn that gave it back.
func TestARefusalATurnGaveBackKeepsItsReply(t *testing.T) {
	t.Parallel()
	rig := refusingRig(t)
	parkOnAQuestion(t, rig)
	rig.resumer.failWith(errors.New("the relaunch was refused before it reached anybody"))
	rig.resumer.failsInTurn = true
	reply := tooLargeReply(rig)
	if d, err := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe", reply); err != nil || d != AnswerRefused {
		t.Fatalf("the refused reply = %q, %v; want %q", d, err, AnswerRefused)
	}
	if got := rig.get("t1"); got.Status != StatusAnswered || got.Answer == nil || got.Answer.Taken() {
		t.Fatalf("the run is %s holding %+v; want the refusal owed again, untaken", got.Status, got.Answer)
	}
	if rig.spentOf(reply.ids()[0]) {
		t.Fatal("the refused reply's delivery was spent by a turn that gave its take back")
	}
	rig.resumer.failWith(nil)
	if rig.fireRetries() != 1 {
		t.Fatal("the refusal's resume was not retried")
	}
	if !rig.spentOf(reply.ids()[0]) {
		t.Fatal("the refused reply's delivery is not spent once a turn kept the refusal: the seat's " +
			"completion ledger would run it as a turn when it comes round")
	}
}

// A REFUSED REPLY OFFERED AGAIN IS ONE RECORD. The refusal's record is published
// as it is recorded and again by every copy of the kept delivery that finds it
// on the run — a node that recorded it may have stopped before it published —
// and every publish carries the one id and the one instant the run, its
// question and the delivery derive, which the event store keeps as one row.
//
// Mutation: publish the record under a fresh identity, as every other
// announcement is, and the second offer is a second row.
func TestARefusedReplyOfferedAgainIsOneRecord(t *testing.T) {
	t.Parallel()
	rig := refusingRig(t)
	parkOnAQuestion(t, rig)
	rig.resumer.failWith(errors.New("the node is draining"))
	reply := tooLargeReply(rig)
	for offer := range 2 {
		if d, err := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe", reply); err != nil ||
			d != AnswerRefused {
			t.Fatalf("offer %d = %q, %v; want %q", offer, d, err, AnswerRefused)
		}
	}
	records := rig.tooLargeRecords()
	if len(records) != 2 {
		t.Fatalf("published %d records, want one per offer", len(records))
	}
	for _, ev := range records[1:] {
		if ev.ID != records[0].ID || !ev.Timestamp.Equal(records[0].Timestamp) {
			t.Errorf("the refusal was published as %s at %s and %s at %s; want one id and one instant",
				records[0].ID, records[0].Timestamp, ev.ID, ev.Timestamp)
		}
	}
	if !records[0].Timestamp.Equal(reply.Posted()) {
		t.Errorf("the record is stamped %s, want the reply's own instant %s", records[0].Timestamp, reply.Posted())
	}
}

// A REFUSAL OPENS ONLY AS THE REFUSAL ITS ROW RECORDS. What a resumed run is
// handed comes out of the answer's seal, and the seal binds whether it is an
// answer or a refusal to what the row records beside it: a row recording a
// refusal opens only to a seal of that very refusal, which hands the run the
// refusal and no words, and a row recording an answer never opens to a
// refusal's seal — so no refusal is ever handed over as an answer with no
// words, nor an answer as a refusal. A refusal key beside an answer naming
// other deliveries (one a build that did not know the key carried while it let
// the refusal go and recorded another) is no refusal at all.
//
// Mutation: open the seal without comparing its refusal with the row's, and a
// refusal's seal opens on a row recording an answer as an answer with no words.
func TestARefusalOpensOnlyAsTheRefusalItsRowRecords(t *testing.T) {
	t.Parallel()
	run := PendingRun{TurnID: "t1", LaunchID: "launch-1", Status: StatusAnswered}
	refusal := AnswerRefusal{EventIDs: []string{"r1", "r2"}, Bytes: MaxAnswerBytes + 1, MaxBytes: MaxAnswerBytes}
	answer := RecordedAnswer{Via: types.AnswerViaChat, EventIDs: refusal.EventIDs, RecordedAt: time.Now().UTC()}
	refused := run
	sealed := sealedOnto(t, rigAnswers(), run, AnswerWords{Via: types.AnswerViaChat, By: "ana",
		ByKind: "human", Refusal: &refusal}, answer)
	refused.Answer, refused.AnswerRefusal = &sealed, &refusal

	words := openedOn(t, refused)
	if words.Refusal == nil || words.Text != "" || words.Choice != "" ||
		words.Refusal.Bytes != refusal.Bytes || words.Refusal.Messages() != 2 {
		t.Fatalf("the refusal opens to %+v, want the refusal it was sealed as and no words", words)
	}
	if asked := askedRun(refused, words); asked.Refused == nil || asked.Answer != "" {
		t.Errorf("the run is resumed with %+v, want the refusal where its answer would be", asked)
	}

	asAnswer := refused
	asAnswer.AnswerRefusal = nil
	if _, err := rigAnswers().Open(asAnswer); !errors.Is(err, ErrAnswerUnopened) {
		t.Errorf("a refusal's seal on a row recording an answer opens (%v), want ErrAnswerUnopened", err)
	}
	other := refusal
	other.Bytes++
	asOther := refused
	asOther.AnswerRefusal = &other
	if _, err := rigAnswers().Open(asOther); !errors.Is(err, ErrAnswerUnopened) {
		t.Errorf("a refusal's seal on a row recording another refusal opens (%v), want ErrAnswerUnopened", err)
	}
	stale := refused
	stale.AnswerRefusal = &AnswerRefusal{EventIDs: []string{"r0"}, Bytes: MaxAnswerBytes + 1,
		MaxBytes: MaxAnswerBytes}
	if _, isRefusal := stale.Refused(); isRefusal {
		t.Error("a refusal key naming other deliveries than the answer reads as a refusal")
	}
	answered := run
	plain := sealedOnto(t, rigAnswers(), run, AnswerWords{Text: "use main", Via: types.AnswerViaChat},
		RecordedAnswer{Via: types.AnswerViaChat, EventIDs: []string{"r3"}, RecordedAt: time.Now().UTC()})
	answered.Answer, answered.AnswerRefusal = &plain, &AnswerRefusal{EventIDs: []string{"r3"},
		Bytes: MaxAnswerBytes + 1, MaxBytes: MaxAnswerBytes}
	if _, err := rigAnswers().Open(answered); !errors.Is(err, ErrAnswerUnopened) {
		t.Errorf("an answer's seal on a row recording a refusal opens (%v), want ErrAnswerUnopened", err)
	}
}

// failingOpenLedgerRig is a rig whose completion ledger cannot be read, as a
// coordination store that times out, or a drain that cancels the read, leaves
// it: every delivery reads as unspent, which is what every ledger read answers
// when it fails ([CoordinatorOptions.WasSpent]).
func failingOpenLedgerRig(t *testing.T) *coordRig {
	t.Helper()
	return newCoordRigWith(t, func(o *CoordinatorOptions) {
		o.WasSpent = func(context.Context, string, *events.Event) bool { return false }
	})
}

// A REFUSAL THE INLINE ATTEMPT'S TURN KEPT IS SPENT, WHATEVER THE LEDGER SAYS.
// The reply's delivery is in hand while the inline attempt resumes the run with
// its refusal, and once a turn of that attempt has told the run the reply never
// reached it, the delivery is spent — acknowledged, and run as no turn — on the
// attempt's own account, as an answer's is at its record. Nothing else the node
// reads then says so for certain: the relaunch that told the run cleared the
// refusal from the row, or the run is over and its row gone, and the completion
// ledger the turn recorded the delivery as worked in fails open, an unreadable
// read answering "unspent". Decided off those, the seat was handed, as a new
// message, the very reply its run had just been told never reached it.
//
// Mutation: decide the delivery off the ledger and the row alone, reading the
// attempt's account as nothing kept (refusedReply's kept false), and both
// cases answer AnswerNotMine: the dispatcher runs the reply as an ordinary turn
// of the seat.
func TestARefusalTheInlineAttemptsTurnKeptIsSpentWhateverTheLedgerSays(t *testing.T) {
	t.Parallel()
	for name, turn := range map[string]func(*testing.T, *coordRig) func(context.Context, PendingRun){
		"the run, told, asks again": asksAgain,
		"the run, told, is over":    func(*testing.T, *coordRig) func(context.Context, PendingRun) { return nil },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig := failingOpenLedgerRig(t)
			parkOnAQuestion(t, rig)
			rig.resumer.during = turn(t, rig)
			reply := tooLargeReply(rig)
			d, err := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe", reply)
			calls := rig.resumer.calls()
			if len(calls) != 1 || askedOf(calls[0]).Refused == nil {
				t.Fatalf("premise: the run was resumed %d times, with %+v; want it told of the refusal once",
					len(calls), calls)
			}
			if err != nil || d != AnswerConsumed {
				t.Fatalf("the refused reply whose refusal the inline attempt's turn kept = %q, %v; want %q, "+
					"spent on the attempt's account, the ledger unreadable", d, err, AnswerConsumed)
			}
			if !rig.spentOf(reply.ids()[0]) {
				t.Error("the delivery was not recorded as worked by the turn that kept its refusal")
			}
		})
	}
}

// claimTakenFirst is a store on which another attempt claims an owed refusal's
// run the instant before this node's does — the seat's next holder, across a
// handoff — and that attempt's turn takes the refusal and tells the run of it,
// relaunching the run to carry its round on from the refused question as such a
// turn does (sandbox.LaunchRequest.Continue): this node's claim then finds the
// run moved on, and no turn of its own keeps anything.
type claimTakenFirst struct {
	PendingStore
	t    *testing.T
	once sync.Once
}

func (s *claimTakenFirst) ClaimForResume(ctx context.Context, turnID string, tail Tail, fence Fence,
) (PendingRun, bool, error) {
	s.once.Do(func() {
		claimed, won, err := s.PendingStore.ClaimForResume(ctx, turnID, tail, fence)
		if err != nil || !won {
			s.t.Errorf("the other attempt's claim = %v, %v", won, err)
			return
		}
		if took, err := s.PendingStore.TakeAnswer(ctx, turnID, claimed.LaunchID, fence); err != nil || !took {
			s.t.Errorf("the other attempt's turn taking the refusal = %v, %v", took, err)
			return
		}
		claimed.Continues = true
		if _, err := s.PendingStore.BeginLaunch(ctx, claimed, fence); err != nil {
			s.t.Errorf("the other attempt's turn telling the run: %v", err)
		}
	})
	return s.PendingStore.ClaimForResume(ctx, turnID, tail, fence)
}

// A REFUSED REPLY WHOSE ROUND WAS CARRIED ON FROM ITS REFUSAL IS SPENT. Another
// attempt can win the claim of an owed refusal's run — the seat's next holder,
// across a handoff — and its turn tells the run of the refusal by relaunching it
// to carry the round on from the refused question. This node's own attempt then
// finds the run moved on, no turn of its own kept anything, and the refusal is
// gone from the row; the completion ledger, which that turn writes once it
// returns, may not say so yet, and fails open besides. What says so is the
// row: the relaunch that told the run keeps the refused question for the
// round's review (PendingRun.RoundRefusals), and only a turn that took the
// refusal writes it. So the delivery in hand is spent, never run as a new
// message to a seat whose run was just told the reply never reached it.
//
// Mutation: read nothing of the round's record (carriedOnFromRefusal false),
// and the delivery answers AnswerNotMine: the dispatcher runs the reply as an
// ordinary turn of the seat.
func TestARefusedReplyWhoseRoundWasCarriedOnFromItsRefusalIsSpent(t *testing.T) {
	t.Parallel()
	rig := failingOpenLedgerRig(t)
	parkOnAQuestion(t, rig)
	asked := rig.get("t1").LaunchID
	rig.coordinator.pending = &claimTakenFirst{PendingStore: rig.pending, t: t}
	reply := tooLargeReply(rig)
	d, err := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe", reply)
	if calls := rig.resumer.calls(); len(calls) != 0 {
		t.Fatalf("premise: this node resumed the run %d times; want its claim lost to the other attempt",
			len(calls))
	}
	if got := rig.get("t1"); got.AnswerRefusal != nil || got.LaunchID == asked ||
		!slices.ContainsFunc(got.RoundRefusals, func(r RoundRefusal) bool { return r.Job == asked }) {
		t.Fatalf("premise: the run is on job %s holding %+v, its round's refusals %+v; want it relaunched "+
			"from the refused question of %s, the refusal kept for the round", got.LaunchID,
			got.AnswerRefusal, got.RoundRefusals, asked)
	}
	if err != nil || d != AnswerConsumed {
		t.Fatalf("the refused reply whose round another attempt's turn carried on = %q, %v; want %q, "+
			"spent, the ledger unreadable", d, err, AnswerConsumed)
	}
}

// refusedAndClaimed records a reply too large for t1 as its question's refusal
// — the inline attempt failing, so the refusal is owed and its delivery kept —
// and then claims the run for the refusal's resume and has its turn take the
// refusal, as the node holding the seat does the instant before it stops.
func refusedAndClaimed(t *testing.T, rig *coordRig) Reply {
	t.Helper()
	parkOnAQuestion(t, rig)
	rig.resumer.failWith(errors.New("transient"))
	reply := tooLargeReply(rig)
	if d, err := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe", reply); err != nil || d != AnswerRefused {
		t.Fatalf("the refused reply = %q, %v; want %q, its refusal owed", d, err, AnswerRefused)
	}
	launch := rig.get("t1").LaunchID
	if _, won, err := rig.pending.ClaimForResume(t.Context(), "t1", RecordedAnswerTail(launch), rigLease); err != nil || !won {
		t.Fatalf("the dying node's claim = %v, %v", won, err)
	}
	if ok, err := rig.pending.TakeAnswer(t.Context(), "t1", launch, rigLease); err != nil || !ok {
		t.Fatalf("the dying node's turn taking the refusal = %v, %v", ok, err)
	}
	// THE NODE STOPS: none of its retries run again, and nothing it held in
	// memory survives.
	rig.coordinator.Stop()
	return reply
}

// A REFUSAL A TURN TOOK AND DIED HOLDING IS SPENT BY THE REAP. A turn that took
// a refusal may have told the run of it before its node stopped, so the seat's
// next holder reaps the claim as spent, as it reaps an answer a turn took:
// nothing is handed back — the row carries none of the reply, and the turn may
// have reached the person — and the refusal's delivery, which the seat's inbox
// kept and which comes round to this holder unacknowledged, is recorded as
// worked in the completion ledger, which drops it when it does. The take
// itself spends nothing (TestARefusalATurnGaveBackKeepsItsReply), so for a
// turn that died holding the refusal the reap is the one place its delivery is
// spent.
//
// Mutation: spend nothing of a refusal at the reap (spendTaken without
// spendRefused), and its delivery comes round to the next holder unspent,
// matched to no run, and runs as an ordinary turn of the seat.
func TestARefusalATurnTookAndDiedHoldingIsSpentByTheReap(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	reply := refusedAndClaimed(t, rig)
	if got := rig.get("t1"); got.Status != StatusResumed || got.AnswerRefusal == nil || !got.Answer.Taken() {
		t.Fatalf("premise: the run is %s holding %+v and %+v; want the refusal taken by the claim's turn",
			got.Status, got.AnswerRefusal, got.Answer)
	}
	if spent := rig.spentDeliveries(); len(spent) != 0 {
		t.Fatalf("premise: the take spent %+v; a refusal's delivery is spent only once a turn keeps it", spent)
	}
	next := reaper(t.Context(), t, rig, rig.pending, 2, nil)

	if spent := next.spentDeliveries(); len(spent) != 1 || spent[0].id != reply.ids()[0] {
		t.Fatalf("the reap spent %+v, want the refused reply's delivery recorded as worked once", spent)
	}
	if got := rig.handedBack(); len(got) != 0 {
		t.Fatalf("handed back %v for a refusal a turn had already taken", got)
	}
	rig.finished("t1")
	abandonedOnce(t, rig, false)
}

// A REFUSAL WHOSE SEAL DOES NOT OPEN IS LET GO OF, AND SAID ONCE. What a resumed
// run is handed comes out of the answer's seal, so a refusal whose seal does not
// open under this node's keyring is never resumed: it is let go of as an answer
// whose seal does not open is — the run waits on its question again with the
// reply declined for it, nothing spent and nothing handed back, so the kept
// delivery comes round as the ordinary message it is. Unlike that answer it is
// announced, because the refusal's own `too_large` record said the run would be
// told: its let-go is published once, `declined` on the chat route at the
// refusal's figures, with none of the reply's words and naming nobody, since
// nothing on the row about the reply can be attributed once its seal does not
// open.
//
// THE CLOCK MOVES ON after the question is asked, so a let-go that anchored
// it again would anchor it at another instant, where the rig's clock left
// standing read the two equal.
//
// Mutation: let it go without its announcement, as an answer whose seal does
// not open is (dropUnopened skipping announceRefusalLetGo), and the record
// that the run would be told stands for a run waiting on its question again;
// anchor the question again as the refusal is let go of, and it was not asked
// then.
func TestARefusalWhoseSealDoesNotOpenIsLetGoOfAndSaidOnce(t *testing.T) {
	t.Parallel()
	rig := refusingRig(t)
	parkOnAQuestion(t, rig)
	asked := rig.get("t1").AskedAt
	rig.now = rig.now.Add(time.Second)
	rig.resumer.failWith(errors.New("the node is draining"))
	reply := tooLargeReply(rig)
	if d, err := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe", reply); err != nil || d != AnswerRefused {
		t.Fatalf("the refused reply = %q, %v; want %q", d, err, AnswerRefused)
	}
	// THE NEXT ATTEMPT IS MADE UNDER A KEYRING THAT DOES NOT OPEN IT: the same
	// key id, other material.
	elsewhere, err := NewAnswerSeal(recordseal.OneKey(rigAnswerKey, "another fleet's key material"))
	if err != nil {
		t.Fatal(err)
	}
	rig.coordinator.answers = elsewhere
	rig.resumer.failWith(nil)
	if rig.fireRetries() != 1 {
		t.Fatal("the refusal's resume was not retried")
	}

	if calls := rig.resumer.calls(); len(calls) != 0 {
		t.Fatalf("the run was resumed with a refusal whose seal does not open: %+v", calls)
	}
	got := rig.get("t1")
	if got.Status != StatusAwaiting || got.Answer != nil || got.AnswerRefusal != nil ||
		!got.AskedAt.Equal(asked) || !slices.Contains(got.DeclinedAnswers, reply.ids()[0]) {
		t.Fatalf("the run is %s asked at %s, holding %+v and %+v, declined %v; want it waiting on the "+
			"question asked at %s with the reply declined", got.Status, got.AskedAt, got.Answer,
			got.AnswerRefusal, got.DeclinedAnswers, asked)
	}
	if rig.spentOf(reply.ids()[0]) {
		t.Error("the refused reply's delivery was spent, so the reply is dropped as worked")
	}
	if handed := rig.handedBack(); len(handed) != 0 {
		t.Errorf("handed back %v from a row that carried none of the reply", handed)
	}
	told := rig.answeredRecords()
	if len(told) != 2 || told[0].Outcome != types.AnswerTooLarge {
		t.Fatalf("announced %+v, want the refusal's record and then its let-go, once", told)
	}
	if let := told[1]; let.Outcome != types.AnswerDeclined || let.Via != types.AnswerViaChat ||
		let.AnsweredBy != "" || let.AnswerBytes != told[0].AnswerBytes ||
		let.AnswerMaxBytes != MaxAnswerBytes || let.Answer != "" {
		t.Errorf("the let-go was announced as %+v, want %q on the chat route at the refusal's figures, "+
			"naming nobody and with none of the reply's words", let, types.AnswerDeclined)
	}
	if d, err := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe", reply); err != nil || d != AnswerNotMine {
		t.Fatalf("the kept delivery coming round = %q, %v; want %q, the ordinary message it is", d, err,
			AnswerNotMine)
	}
}
