package sandbox

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/statelog"
)

// rigAnswerKey is the key id every rig's coordinator seals answers under, and
// rigAnswerRing the keyring it is on: ONE RING FOR EVERY RIG, as a fleet shares
// one, so a successor rig opens what its predecessor sealed.
const rigAnswerKey = "answers-k1"

var rigAnswerRing = statelog.OneKey(rigAnswerKey, "the sandbox rigs' fleet key material")

// rigAnswers is the seal every rig's coordinator is built with.
var rigAnswers = sync.OnceValue(func() *AnswerSeal {
	seal, err := NewAnswerSeal(rigAnswerRing)
	if err != nil {
		panic(err)
	}
	return seal
})

// sealedOnto is answer made of words, sealed onto run's question as the
// coordinator that records it seals it ([Coordinator.record]).
func sealedOnto(t *testing.T, seal *AnswerSeal, run PendingRun, words AnswerWords, answer RecordedAnswer,
) RecordedAnswer {
	t.Helper()
	sealed, err := seal.Seal(run, words, answer)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	return sealed
}

// openedOn is the answer recorded on run, opened with the rigs' seal.
func openedOn(t *testing.T, run PendingRun) AnswerWords {
	t.Helper()
	words, err := rigAnswers().Open(run)
	if err != nil {
		t.Fatalf("the answer on run %s does not open: %v", run.TurnID, err)
	}
	return words
}

// WHAT THE SEAL BINDS: an answer opens only on the run, the question and the
// deliveries it was sealed for, by the route it was sealed as, under the key
// material it was sealed with — and what it opens to is what was sealed, never
// what the row says beside it.
func TestAnAnswerOpensOnlyAsTheAnswerToTheQuestionItWasSealedFor(t *testing.T) {
	t.Parallel()
	seal := rigAnswers()
	run := PendingRun{TurnID: "t1", LaunchID: "launch-1"}
	words := AnswerWords{Text: "use main", Via: types.AnswerViaOperator, By: "founder",
		ByKind: "human", OperatorID: "pat:founder-token"}
	genuine := sealedOnto(t, seal, run, words, RecordedAnswer{EventIDs: []string{"r1"}})
	run.Answer = &genuine
	got, err := seal.Open(run)
	if err != nil || !reflect.DeepEqual(got, words) {
		t.Fatalf("open = %+v, %v; want the words sealed, by who gave them", got, err)
	}

	elsewhere, err := NewAnswerSeal(statelog.OneKey(rigAnswerKey, "another fleet's key material"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		row  func() PendingRun
	}{
		{"another run's row", func() PendingRun {
			r := run
			r.TurnID = "t2"
			return r
		}},
		{"the same run's next question", func() PendingRun {
			r := run
			r.LaunchID = "launch-2"
			return r
		}},
		{"other deliveries", func() PendingRun {
			r, a := run, genuine
			a.EventIDs = []string{"r2"}
			r.Answer = &a
			return r
		}},
		{"another route", func() PendingRun {
			r, a := run, genuine
			a.Via = types.AnswerViaChat
			r.Answer = &a
			return r
		}},
		{"other key material under the same key id", func() PendingRun {
			r := run
			a := sealedOnto(t, elsewhere, run, words, RecordedAnswer{EventIDs: []string{"r1"}})
			r.Answer = &a
			return r
		}},
		{"a body rewritten after the seal", func() PendingRun {
			r, a := run, genuine
			a.Sealed = []byte(strings.Replace(string(genuine.Sealed), "use main", "use prod", 1))
			r.Answer = &a
			return r
		}},
		{"bytes that are no frame", func() PendingRun {
			r, a := run, genuine
			a.Sealed = []byte(`{"text":"use main"}`)
			r.Answer = &a
			return r
		}},
		{"no seal", func() PendingRun {
			r, a := run, genuine
			a.Sealed = nil
			r.Answer = &a
			return r
		}},
	} {
		got, err := seal.Open(c.row())
		if !errors.Is(err, ErrAnswerUnopened) || !reflect.DeepEqual(got, AnswerWords{}) {
			t.Errorf("%s: open = %+v, %v; want it refused as no answer to this question", c.name, got, err)
		}
	}
}

// A KEY THIS NODE DOES NOT HOLD is its own answer, not a refusal of the
// answer: the seal names a key, this node's ring has no such id, and the line
// it is logged with names the keys the node does hold.
func TestAnAnswerUnderAKeyThisNodeDoesNotHoldIsToldApart(t *testing.T) {
	t.Parallel()
	run := PendingRun{TurnID: "t1", LaunchID: "launch-1"}
	other, err := NewAnswerSeal(statelog.OneKey("answers-k2", "the key a newer node seals under"))
	if err != nil {
		t.Fatal(err)
	}
	answer := sealedOnto(t, other, run, AnswerWords{Text: "use main", Via: types.AnswerViaChat},
		RecordedAnswer{EventIDs: []string{"r1"}})
	run.Answer = &answer
	got, err := rigAnswers().Open(run)
	if !errors.Is(err, ErrAnswerKeyUnknown) || !reflect.DeepEqual(got, AnswerWords{}) {
		t.Fatalf("open = %+v, %v; want it told apart as a key this node does not hold", got, err)
	}
	if !strings.Contains(err.Error(), rigAnswerKey) || !strings.Contains(err.Error(), "secrets.keys") {
		t.Errorf("the refusal %q does not name the keys this node holds and where to add one", err)
	}
}

// THE ANSWER'S WORDS ARE ONLY INSIDE ITS SEAL: a recorded answer, encoded as the
// store keeps it, carries neither the text nor who gave it in a field of its
// own — the deliveries it was made of ride beside it as the messages they are —
// and the turn the answer resumes is handed both, out of the seal.
func TestARecordedAnswerCarriesItsWordsOnlyInsideItsSeal(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	launchScheduled(t, rig, "t1")
	parksOnAQuestion(t, rig, "t1")
	rig.resumer.failWith(errors.New("the node lost the seat"))
	given, ev := givenAgainst(t, rig, "t1")
	given.Answer = "deploy from the release-candidate branch"
	if d, err := rig.coordinator.AnswerByTurn(t.Context(), given, ev); d != AnswerConsumed {
		t.Fatalf("AnswerByTurn = %q, %v, want the answer recorded", d, err)
	}
	got := rig.get("t1")
	bare := *got.Answer
	bare.Events = nil
	raw, err := json.Marshal(bare)
	if err != nil {
		t.Fatal(err)
	}
	for _, clear := range []string{given.Answer, given.AnsweredBy, given.OperatorID} {
		if strings.Contains(string(raw), clear) {
			t.Errorf("the recorded answer carries %q in the clear: %s", clear, raw)
		}
	}
	if words := openedOn(t, got); words.Text != given.Answer || words.By != given.AnsweredBy ||
		words.ByKind != given.AnsweredByKind || words.OperatorID != given.OperatorID {
		t.Errorf("the seal opened to %+v, want the answer and who gave it", words)
	}

	rig.resumer.failWith(nil)
	rig.fireRetries()
	calls := rig.resumer.calls()
	if len(calls) == 0 {
		t.Fatal("the owed answer was never resumed")
	}
	if asked := askedOf(calls[len(calls)-1]); asked.Answer != given.Answer || asked.By != given.AnsweredBy {
		t.Errorf("the resumed turn was handed %+v, want the sealed answer and its author", asked)
	}
}

// AN ANSWER WHOSE SEAL DOES NOT OPEN IS HANDED TO NO TURN AND NAMES NOBODY,
// however it fails to open: no resume, no announcement of who answered,
// nothing recorded as worked or handed back to the seat — and the run waits on
// its question again, logged at error, for the next reply to answer it.
//
// Not parallel: it captures the process-wide logger ([captureLogs]).
func TestAnAnswerWhoseSealDoesNotOpenIsHandedToNoTurn(t *testing.T) {
	for _, c := range []struct {
		name   string
		sealed func(t *testing.T, run PendingRun, words AnswerWords, answer RecordedAnswer) RecordedAnswer
	}{
		{"sealed for another run", func(t *testing.T, run PendingRun, words AnswerWords,
			answer RecordedAnswer) RecordedAnswer {
			other := run
			other.TurnID = "t2"
			return sealedOnto(t, rigAnswers(), other, words, answer)
		}},
		{"sealed for an earlier question", func(t *testing.T, run PendingRun, words AnswerWords,
			answer RecordedAnswer) RecordedAnswer {
			earlier := run
			earlier.LaunchID = "an-earlier-launch"
			return sealedOnto(t, rigAnswers(), earlier, words, answer)
		}},
		{"rewritten after it was sealed", func(t *testing.T, run PendingRun, words AnswerWords,
			answer RecordedAnswer) RecordedAnswer {
			answer = sealedOnto(t, rigAnswers(), run, words, answer)
			answer.Sealed = []byte(strings.Replace(string(answer.Sealed), "use main", "use prod", 1))
			return answer
		}},
		{"sealed with other key material", func(t *testing.T, run PendingRun, words AnswerWords,
			answer RecordedAnswer) RecordedAnswer {
			elsewhere, err := NewAnswerSeal(statelog.OneKey(rigAnswerKey, "another fleet's key material"))
			if err != nil {
				t.Fatal(err)
			}
			return sealedOnto(t, elsewhere, run, words, answer)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			logs := captureLogs(t)
			rig := newCoordRig(t)
			parkOnAQuestion(t, rig)
			run := rig.get("t1")
			// A REAL DELIVERY BEHIND IT, so that a let-go which handed it
			// back or recorded it as worked would have something to show.
			words, answer := chatReply(answerOnTheDM, "use main",
				replyAt("use main", run.AskedAt.Add(time.Minute))).answerOf(rig.now)
			if len(answer.Events) == 0 {
				t.Fatal("the reply carries no delivery")
			}
			if _, won, err := rig.pending.RecordAnswer(t.Context(), "t1", run.LaunchID,
				c.sealed(t, run, words, answer), rigLease); err != nil || !won {
				t.Fatalf("RecordAnswer = %v, %v", won, err)
			}
			holds := rig.withHold()
			if err := rig.recoverSeat(t.Context(), rigLease.Owner, rigLease.Epoch); err != nil {
				t.Fatalf("RecoverSeat: %v", err)
			}
			rig.fireRetries()

			if calls := rig.resumer.calls(); len(calls) != 0 {
				t.Fatalf("the run was resumed with an answer whose seal does not open: %+v", calls)
			}
			got := rig.get("t1")
			if got.Status != StatusAwaiting || got.Answer != nil {
				t.Errorf("run = %q with answer %+v, want it waiting on its question again with none",
					got.Status, got.Answer)
			}
			if records := rig.answeredRecords(); len(records) != 0 {
				t.Errorf("announced %+v for an answer whose seal does not open", records)
			}
			if spent := rig.spentDeliveries(); len(spent) != 0 {
				t.Errorf("recorded %+v as worked for an answer whose seal does not open", spent)
			}
			if handed := rig.handedBack(); len(handed) != 0 || len(got.HandBack) != 0 {
				t.Errorf("handed %v back (owing %v) for an answer whose seal does not open",
					handed, got.HandBack)
			}
			if holds.holding("swe") {
				t.Error("the seat's inbox is still held behind an answer that was let go of")
			}
			if line := logs.String(); !strings.Contains(line, "sandbox_answer_unverified") ||
				!strings.Contains(line, "level=ERROR") {
				t.Errorf("the answer was let go of without an error line: %q", line)
			}

			// THE QUESTION IS OPEN AGAIN: the next reply answers it.
			rig.resumer.failWith(nil)
			if d, err := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe",
				chatReply(answerOnTheDM, "use the release branch", replyAt("use the release branch",
					got.AskedAt.Add(time.Minute)))); err != nil || d != AnswerConsumed {
				t.Fatalf("the next reply = %q, %v, want it to answer the question", d, err)
			}
			if calls := rig.resumer.calls(); len(calls) != 1 ||
				askedOf(calls[0]).Answer != "use the release branch" {
				t.Fatalf("resumes = %+v, want one, with the next reply", calls)
			}
		})
	}
}

// AN ANSWER SEALED UNDER A KEY THIS NODE DOES NOT HOLD WAITS: never resumed and
// never let go of on this node — recorded and owed, uncounted, the seat's inbox
// held behind it and a warning naming the keys the node holds — and resumed by
// the first holder of the seat that holds the key.
//
// Not parallel: it captures the process-wide logger ([captureLogs]).
func TestAnAnswerUnderAKeyThisNodeDoesNotHoldWaitsForANodeThatHoldsIt(t *testing.T) {
	logs := captureLogs(t)
	rig := newCoordRig(t)
	parkOnAQuestion(t, rig)
	run := rig.get("t1")
	newer := statelog.Keyring{ActiveID: "answers-k2", Keys: []statelog.Key{
		rigAnswerRing.Keys[0], {ID: "answers-k2", Material: "the key a newer node seals under"},
	}}
	newerSeal, err := NewAnswerSeal(newer)
	if err != nil {
		t.Fatal(err)
	}
	if _, won, err := rig.pending.RecordAnswer(t.Context(), "t1", run.LaunchID,
		sealedOnto(t, newerSeal, run, chatWords("use main"), answerMadeOf("r1")), rigLease); err != nil || !won {
		t.Fatalf("RecordAnswer = %v, %v", won, err)
	}
	holds := rig.withHold()
	if err := rig.recoverSeat(t.Context(), rigLease.Owner, rigLease.Epoch); err != nil {
		t.Fatalf("RecoverSeat: %v", err)
	}
	// AS MANY TIMES AS THE BACKSTOP COMES ROUND, past every attempt bound a
	// failed resume has: nothing is counted, so nothing is ever let go of.
	for range MaxAnswerAttempts + 2 {
		rig.fireRetries()
	}
	if calls := rig.resumer.calls(); len(calls) != 0 {
		t.Fatalf("the run was resumed with an answer this node cannot open: %+v", calls)
	}
	got := rig.get("t1")
	if got.Status != StatusAnswered || got.Answer == nil || len(got.DeclinedAnswers) != 0 {
		t.Fatalf("run = %q with answer %+v, declined %v; want the answer still recorded and owed",
			got.Status, got.Answer, got.DeclinedAnswers)
	}
	if spent := rig.spentDeliveries(); len(spent) != 0 {
		t.Errorf("recorded %+v as worked for an answer no turn took", spent)
	}
	if !holds.holding("swe") {
		t.Error("the seat's inbox was opened ahead of the answer it owes")
	}
	if waiting := rig.coordinator.waitingOn("t1"); waiting != ConditionAnswerKey {
		t.Errorf("the owed answer waits on %q, want %q", waiting, ConditionAnswerKey)
	}
	line := logs.String()
	if !strings.Contains(line, "sandbox_answer_key_unknown") || !strings.Contains(line, "level=WARN") ||
		!strings.Contains(line, rigAnswerKey) {
		t.Errorf("the wait was not warned of with the keys this node holds: %q", line)
	}
	if strings.Contains(line, "sandbox_answer_unverified") {
		t.Errorf("an answer under a key this node does not hold was refused as one that does not open: %q", line)
	}

	// THE SEAT'S NEXT HOLDER HOLDS THE KEY, and resumes with the answer.
	successor := newCoordRig(t)
	successor.waiterRig = rig.waiterRig
	successor.coordinator.pending = rig.pending
	successor.coordinator.answers = newerSeal
	rig.coordinator.Stop()
	if err := successor.recoverSeat(t.Context(), "node-b", 2); err != nil {
		t.Fatalf("RecoverSeat: %v", err)
	}
	successor.fireRetries()
	if calls := successor.resumer.calls(); len(calls) != 1 || askedOf(calls[0]).Answer != "use main" {
		t.Fatalf("the holder of the key resumed %+v, want the recorded answer", calls)
	}
}

// chatWords is a chat reply's words: the text, by a person on the chat
// surface, with no credential.
func chatWords(text string) AnswerWords {
	return AnswerWords{Text: text, Via: types.AnswerViaChat, By: "ana", ByKind: "human"}
}

// answerMadeOf is the bookkeeping of an answer made of the deliveries ids, to be
// sealed ([sealedOnto]).
func answerMadeOf(ids ...string) RecordedAnswer {
	return RecordedAnswer{EventIDs: ids, RecordedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
}

// waitingOn is what the owed answer of turnID waits on, or "" where it waits on
// nothing or is not owed.
func (c *Coordinator) waitingOn(turnID string) Condition {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r := c.retries[turnID]; r != nil {
		return r.waiting
	}
	return ""
}

// WHO WROTE A REPLY RIDES ITS SEAL WITH THE WORDS. The run a chat reply carries
// on reads it fenced under who wrote it, so that label is handed to the run out
// of the seal like the words, never off the row beside it: a row that named a
// stranger's reply as a colleague's would have the run take a stranger's words
// as a colleague's request.
//
// Mutation: leave the label out of the sealed body and the opened answer names
// no writer.
func TestWhoWroteAReplyRidesItsSealWithTheWords(t *testing.T) {
	t.Parallel()
	seal := rigAnswers()
	run := PendingRun{TurnID: "t1", LaunchID: "launch-1"}
	words := AnswerWords{Text: "Use Postgres", Via: types.AnswerViaChat, By: "ana", ByKind: "human",
		Label: &provenance.Label{Source: provenance.SourceChatThread, Trust: provenance.TrustColleague,
			Authors: []provenance.Author{{Kind: iam.ActorHuman, Seat: "ana"}}}}
	sealed := sealedOnto(t, seal, run, words, RecordedAnswer{EventIDs: []string{"r1"}})
	run.Answer = &sealed
	if got := openedOn(t, run); !reflect.DeepEqual(got, words) {
		t.Errorf("the seal opened to %+v, want the reply and who wrote it, as sealed", got)
	}
	if asked := askedRun(run, openedOn(t, run)); !reflect.DeepEqual(asked.Label, words.Label) {
		t.Errorf("the resumed run is handed the reply under %+v; want it as sealed, %+v",
			asked.Label, words.Label)
	}
}
