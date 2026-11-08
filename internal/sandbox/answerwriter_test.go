package sandbox

import (
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
)

// ONLY WHO A QUESTION WAS PUT TO ANSWERS IT IN THE CONVERSATION (ADR-0077).
// One turn works for one writer (ADR-0109), and the answer is what the run
// carries on with. So a reply in the run's thread answers its question only
// where its writer is a seat of the question's audience (resolved once at the
// park) or the run's requester, who owns an executor's question on the
// dashboard's route too. A stranger, a credential and a seat outside the
// audience are not the answer, and a row naming nobody admits nobody: never
// anybody.
//
// Mutation: drop putTo from Reply.Best, and Sam's reply in Ada's thread
// answers Ada's question.
func TestOnlyWhoAQuestionWasPutToAnswersItInTheConversation(t *testing.T) {
	t.Parallel()
	asked := time.Unix(1_700_000_000, 0).UTC()
	parked := func(requester string, audience ...string) PendingRun {
		return PendingRun{TurnID: "t1", AgentHandle: "swe", Status: StatusAwaiting,
			ConversationKey: answerOnTheDM.Identity, PartitionKey: answerOnTheDM.Partition,
			AskedAt: asked, Requester: requester, AudienceHandles: audience}
	}
	reply := func(writer string) Reply {
		return Reply{Conv: answerOnTheDM, Events: []*events.Event{replyAt("use main", asked.Add(time.Second))},
			Writer: writer}
	}
	for name, tc := range map[string]struct {
		run     PendingRun
		writer  string
		answers bool
	}{
		"the requester":                      {parked("ada", "cto"), "ada", true},
		"a seat of the audience":             {parked("ada", "cto", "qa"), "qa", true},
		"the lead a fallback question names": {parked("", "cto"), "cto", true},
		"a seat outside the audience":        {parked("ada", "cto"), "sam", false},
		"a writer who holds no seat":         {parked("ada", "cto"), "", false},
		"a row naming nobody":                {parked(""), "sam", false},
	} {
		if _, got := reply(tc.writer).Best([]PendingRun{tc.run}); got != tc.answers {
			t.Errorf("%s: a reply by %q answers = %v, want %v", name, tc.writer, got, tc.answers)
		}
	}
}

// parkedOnAQuestionPutTo is a coordinator rig whose run t1 is parked on a
// question its park put to audience.
func parkedOnAQuestionPutTo(t *testing.T, audience ...string) *coordRig {
	t.Helper()
	rig := newCoordRig(t)
	rig.audience.resolves(Audience{Handles: audience})
	rig.launch("t1")
	rig.ask("t1", Ask{Question: "which branch?", To: AskToManager, Decision: choosing("which branch?")})
	rig.runner.Finish(Result{})
	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	return rig
}

// A REPLY BY SOMEBODY THE QUESTION WAS NOT PUT TO RUNS AS ITS OWN TURN, and the
// run stays parked on its question: the reply is the ordinary message it is,
// worked for its own writer.
func TestAReplyBySomebodyTheQuestionWasNotPutToLeavesTheRunParked(t *testing.T) {
	t.Parallel()
	rig := parkedOnAQuestionPutTo(t, "cto")
	reply := chatReply(answerOnTheDM, "use main", nil)
	reply.Writer = "sam"
	disposition, err := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe", reply)
	if err != nil || disposition != AnswerNotMine {
		t.Fatalf("disposition = %q, %v, want %q: Sam was never asked", disposition, err, AnswerNotMine)
	}
	if got := rig.get("t1"); got.Status != StatusAwaiting || got.Answer != nil {
		t.Fatalf("run = %q with answer %+v, want it still waiting on its question", got.Status, got.Answer)
	}
	reply.Writer = "cto"
	if disposition, err := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe", reply); err != nil ||
		disposition != AnswerConsumed {
		t.Fatalf("the cto's reply = %q, %v, want %q: the question was put to them", disposition, err,
			AnswerConsumed)
	}
}

// THE HOLDER IS READ WHEN THE ANSWER ARRIVES, never at the park: a question put
// to a seat whose person was suspended since (a vendor still delivers their
// chat), or which nobody holds, is answered by nothing they say, and their
// reply runs as its own turn. A directory that cannot say keeps the reply's
// place: never spent on the run, and never on an ordinary turn the run was
// owed.
//
// Mutation: drop the MayAnswer check from the coordinator, and the suspended
// lead's reply answers the question.
func TestAQuestionsSeatAnswersOnlyWhileItsHolderMayAct(t *testing.T) {
	t.Parallel()
	t.Run("a holder who may not act now", func(t *testing.T) {
		t.Parallel()
		rig := parkedOnAQuestionPutTo(t, "cto")
		rig.audience.mayNotAct("cto")
		reply := chatReply(answerOnTheDM, "use main", nil)
		reply.Writer = "cto"
		disposition, err := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe", reply)
		if err != nil || disposition != AnswerNotMine {
			t.Fatalf("disposition = %q, %v, want %q", disposition, err, AnswerNotMine)
		}
		if got := rig.get("t1"); got.Status != StatusAwaiting || got.Answer != nil {
			t.Fatalf("run = %q with answer %+v, want it still waiting on its question", got.Status, got.Answer)
		}
	})
	t.Run("a directory that cannot say", func(t *testing.T) {
		t.Parallel()
		rig := parkedOnAQuestionPutTo(t, "cto")
		rig.audience.unknown(errors.New("the identity directory is behind"))
		reply := chatReply(answerOnTheDM, "use main", nil)
		reply.Writer = "cto"
		disposition, err := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe", reply)
		if disposition != AnswerDeferred || err == nil {
			t.Fatalf("disposition = %q, %v, want %q with the cause", disposition, err, AnswerDeferred)
		}
		if got := rig.get("t1"); got.Status != StatusAwaiting || got.Answer != nil {
			t.Fatalf("run = %q with answer %+v, want it still waiting on its question", got.Status, got.Answer)
		}
	})
}
