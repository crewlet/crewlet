package sandboxtest

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A reply too large to be its run's answer, recorded as its question's refusal
// (ADR-0114): the cases [Run] drives for [sandbox.PendingStore.RecordRefusal].

// refusalCases are the cases that read a refusal through the store.
var refusalCases = []struct {
	name string
	fn   func(*testing.T, sandbox.PendingStore)
}{
	{"ARefusalIsRecordedOnAnAnswersTermsWithNoneOfTheReply", testARefusalIsRecordedOnAnAnswersTermsWithNoneOfTheReply},
	{"ARefusalLetGoOfLeavesNoRefusalBehind", testARefusalLetGoOfLeavesNoRefusalBehind},
	{"ARefusalsAddressIsHeldToItsBound", testARefusalsAddressIsHeldToItsBound},
	{"ARefusalTheRoundIsCarriedOnFromIsKeptForItsReview", testARefusalTheRoundIsCarriedOnFromIsKeptForItsReview},
}

// refusalOf is what a run's row records of reply, refused as too large: the
// answer naming its deliveries and carrying a seal of no words, and the
// refusal naming the same deliveries with the figures the store refused it on.
func refusalOf(t *testing.T, s sandbox.PendingStore, turnID, launch string,
	reply sandbox.RecordedAnswer) (sandbox.RecordedAnswer, sandbox.AnswerRefusal) {

	t.Helper()
	_, _, err := s.RecordAnswer(t.Context(), turnID, launch, reply, sandbox.Fence{})
	tooLarge, ok := errors.AsType[*sandbox.AnswerTooLargeError](err)
	if !ok {
		t.Fatalf("the premise: the reply = %v, want it refused as too large", err)
	}
	answer := sandbox.RecordedAnswer{Sealed: sealOf(""), Via: types.AnswerViaChat,
		EventIDs: reply.EventIDs, PostedAt: base, RecordedAt: base}
	return answer, sandbox.AnswerRefusal{EventIDs: reply.EventIDs, Bytes: tooLarge.Weighs, MaxBytes: tooLarge.May}
}

// A REFUSAL IS RECORDED ON AN ANSWER'S TERMS, WITH NONE OF THE REPLY. A reply
// past what the row carries as an answer cannot be recorded, and what the row
// records as its question's outcome instead is its refusal: the run answered,
// holding an answer that names the reply's deliveries and carries none of
// them, and the refusal beside it naming the same deliveries and both figures
// — one write, by the same compare-and-set an answer is recorded by, so a
// second refusal or a short reply after it finds the question answered and is
// false with no error. A refusal whose answer carries any of the reply's
// deliveries, naming other deliveries than its answer, or stating figures
// within the share is an error and writes nothing: the first would put on the
// row the very bytes it could not carry, and the others are no refusal of the
// reply the answer names.
//
// Mutation: record a refusal whose answer carries a delivery of the reply, and
// the row holds the bytes the refusal says it could not; leave the refusal off
// the write, and the run holds an answer with no words, which it would be
// resumed with.
func testARefusalIsRecordedOnAnAnswersTermsWithNoneOfTheReply(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	r := run("t-refusal")
	launch := mustLaunched(t, s, r)
	park(t, s, r.TurnID)
	reply := replyBatch(20, 32<<10)
	answer, refusal := refusalOf(t, s, r.TurnID, launch, reply)

	carrying := answer
	carrying.Events = reply.Events[:1]
	other := refusal
	other.EventIDs = []string{"another-delivery"}
	within := refusal
	within.Bytes = within.MaxBytes
	for name, write := range map[string]struct {
		answer  sandbox.RecordedAnswer
		refusal sandbox.AnswerRefusal
	}{
		"carrying a delivery of the reply": {carrying, refusal},
		"naming other deliveries":          {answer, other},
		"within the share":                 {answer, within},
	} {
		if _, ok, err := s.RecordRefusal(ctx, r.TurnID, launch, write.answer, write.refusal,
			sandbox.Fence{}); err == nil || ok {
			t.Errorf("a refusal %s = %v, %v; want an error and nothing written", name, ok, err)
		}
	}
	if got := mustGet(t, s, r.TurnID); got.Status != sandbox.StatusAwaiting || got.Answer != nil ||
		got.AnswerRefusal != nil {
		t.Fatalf("a refused write left the run %s holding %+v and %+v; want it waiting", got.Status,
			got.Answer, got.AnswerRefusal)
	}

	written, ok, err := s.RecordRefusal(ctx, r.TurnID, launch, answer, refusal, sandbox.Fence{})
	if err != nil || !ok {
		t.Fatalf("RecordRefusal = %v, %v; want the refusal recorded", ok, err)
	}
	for name, got := range map[string]sandbox.PendingRun{"as written": written, "as read": mustGet(t, s, r.TurnID)} {
		held, refused := got.Refused()
		switch {
		case got.Status != sandbox.StatusAnswered || got.Answer == nil:
			t.Errorf("%s: the run is %s holding %+v; want it answered", name, got.Status, got.Answer)
		case len(got.Answer.Events) != 0:
			t.Errorf("%s: the answer carries %d deliveries; want none of the reply", name, len(got.Answer.Events))
		case !refused || held.Messages() != 20 || held.Bytes != refusal.Bytes || held.MaxBytes != refusal.MaxBytes:
			t.Errorf("%s: the refusal reads %+v, %v; want twenty messages at %d past %d", name, held, refused,
				refusal.Bytes, refusal.MaxBytes)
		}
	}
	if _, ok, err := s.RecordRefusal(ctx, r.TurnID, launch, answer, refusal, sandbox.Fence{}); err != nil || ok {
		t.Errorf("the same refusal again = %v, %v; want false with no error", ok, err)
	}
	if _, ok, err := s.RecordAnswer(ctx, r.TurnID, launch, replyBatch(1, 64), sandbox.Fence{}); err != nil || ok {
		t.Errorf("a short reply after the refusal = %v, %v; want false with no error: the question no longer "+
			"waits", ok, err)
	}
}

// A REFUSAL LET GO OF LEAVES NO REFUSAL BEHIND. Letting go of the question's
// outcome lets go of the refusal in the same write: the run waits on its
// question again with the reply's deliveries declined, and the next reply's
// answer is an answer — never read as a refusal through a key left over from
// the one before.
//
// Mutation: clear the answer on a decline and not the refusal beside it, and
// the let-go leaves the run waiting on its question with the refusal still on
// its row.
func testARefusalLetGoOfLeavesNoRefusalBehind(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	r := run("t-refusal-let-go")
	launch := mustLaunched(t, s, r)
	park(t, s, r.TurnID)
	answer, refusal := refusalOf(t, s, r.TurnID, launch, replyBatch(20, 32<<10))
	if _, ok, err := s.RecordRefusal(ctx, r.TurnID, launch, answer, refusal, sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("the premise: RecordRefusal = %v, %v", ok, err)
	}
	declined, ok, err := s.DeclineAnswer(ctx, r.TurnID, launch, answer.EventIDs, nil, sandbox.Fence{})
	if err != nil || !ok {
		t.Fatalf("DeclineAnswer = %v, %v; want the refusal let go of", ok, err)
	}
	if declined.Status != sandbox.StatusAwaiting || declined.Answer != nil || declined.AnswerRefusal != nil {
		t.Fatalf("the let-go left the run %s holding %+v and %+v; want it waiting with neither",
			declined.Status, declined.Answer, declined.AnswerRefusal)
	}
	next := answerOf("r-next", "use main")
	recorded, ok, err := s.RecordAnswer(ctx, r.TurnID, launch, next, sandbox.Fence{})
	if err != nil || !ok {
		t.Fatalf("the next reply = %v, %v; want it recorded", ok, err)
	}
	if _, refused := recorded.Refused(); refused || recorded.AnswerRefusal != nil {
		t.Errorf("the next answer reads as the refusal %+v", recorded.AnswerRefusal)
	}
}

// A REFUSAL'S ADDRESS IS HELD TO ITS BOUND. Where the reply was posted rides the
// refusal — in the clear and again in the answer's seal — so the resume it
// drives raises its working indicator there ([sandbox.AnswerRefusal.Address]):
// ids, a few hundred bytes on every surface the engine reads. One past
// [sandbox.MaxRefusalAddressBytes] as the row holds it is an error and writes
// nothing, and one within it is kept whole.
//
// Mutation: drop the address's bound from the refusal's check, and a refusal
// carrying a page of metadata is written to the row.
func testARefusalsAddressIsHeldToItsBound(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	r := run("t-refusal-address")
	launch := mustLaunched(t, s, r)
	park(t, s, r.TurnID)
	answer, refusal := refusalOf(t, s, r.TurnID, launch, replyBatch(20, 32<<10))

	wide := refusal
	wide.Address = map[string]string{"transport": "slack",
		"channel": strings.Repeat("<", sandbox.MaxRefusalAddressBytes/6)}
	if _, ok, err := s.RecordRefusal(ctx, r.TurnID, launch, answer, wide, sandbox.Fence{}); err == nil || ok {
		t.Fatalf("a refusal whose address is past its bound = %v, %v; want an error and nothing written", ok, err)
	}
	if got := mustGet(t, s, r.TurnID); got.Answer != nil || got.AnswerRefusal != nil {
		t.Fatalf("a refused write left %+v and %+v on the run", got.Answer, got.AnswerRefusal)
	}
	kept := refusal
	kept.Address = map[string]string{"transport": "slack", "channel": "D0ANA", "thread_ts": "1712345678.000100"}
	if _, ok, err := s.RecordRefusal(ctx, r.TurnID, launch, answer, kept, sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("a refusal within its bound = %v, %v; want it recorded", ok, err)
	}
	if held, refused := mustGet(t, s, r.TurnID).Refused(); !refused || !maps.Equal(held.Address, kept.Address) {
		t.Errorf("the refusal reads %+v, %v; want its address kept whole", held, refused)
	}
}

// A REFUSAL THE ROUND IS CARRIED ON FROM IS KEPT FOR ITS REVIEW. The relaunch
// that carries a round on from a question whose reply was refused clears the
// refusal with the question, and keeps what the round's reviewer needs of it in
// the same write ([sandbox.PendingRun.RoundRefusals]): the job it answered, how
// many messages it was and its figures — never its words or its deliveries.
// Kept once however often the round is carried on after it, and cleared by a
// new round, whose review judges other calls.
//
// Mutation: clear the refusal at the relaunch and keep nothing of it, and the
// round's reviewer reads the run's question asked again as one asked twice;
// keep it across a new round, and that round's reviewer is told of a reply to
// a question it never asked.
func testARefusalTheRoundIsCarriedOnFromIsKeptForItsReview(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	const turn = "t-refusal-round"
	asking := mustLaunched(t, s, run(turn))
	park(t, s, turn)
	answer, refusal := refusalOf(t, s, turn, asking, replyBatch(20, 32<<10))
	if _, ok, err := s.RecordRefusal(ctx, turn, asking, answer, refusal, sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("the premise: RecordRefusal = %v, %v", ok, err)
	}
	carryOn := func() {
		t.Helper()
		next := run(turn)
		next.Continues = true
		mustLaunched(t, s, next)
	}
	want := []sandbox.RoundRefusal{{Job: asking, Messages: 20, Bytes: refusal.Bytes, MaxBytes: refusal.MaxBytes}}
	carryOn()
	if got := mustGet(t, s, turn); !slices.Equal(got.RoundRefusals, want) || got.AnswerRefusal != nil {
		t.Fatalf("the round carried on holds %+v beside the refusal %+v; want %+v kept and the refusal "+
			"cleared", got.RoundRefusals, got.AnswerRefusal, want)
	}
	carryOn()
	if got := mustGet(t, s, turn); !slices.Equal(got.RoundRefusals, want) {
		t.Errorf("the round carried on again holds %+v, want %+v, once", got.RoundRefusals, want)
	}
	mustLaunched(t, s, run(turn))
	if got := mustGet(t, s, turn); got.RoundRefusals != nil {
		t.Errorf("a new round holds %+v of the round before", got.RoundRefusals)
	}
}
