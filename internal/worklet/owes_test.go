package worklet_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/decision"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/worklet"
)

// AN INFORMED ANSWER OWES ITS ASKER'S CHAT SURFACE, AND NOBODY ELSE'S TURN.
//
// The asker said it would report the outcome in a channel, and the person who
// answered is told it will: the asker's `answered` wake names that surface in
// [notify.Inbound.Owes], which is what the turn engine holds the turn open on.
// A thread participant on the same item receives the same record under its own
// reason and owes nothing (the promise was the asker's), and an answer to a decision
// that asked for no inform owes nothing at all.
func TestAnInformedAnswerOwesItsAskerAndNobodyElse(t *testing.T) {
	t.Parallel()
	answers := "c-ask"
	asked := decisionFixture()
	asked.Inform = &decision.Inform{Surface: decision.InformSlack, Channel: "releases"}
	answer := worklet.Comment{
		ID: "c-ans", Task: "t-1", Author: "ana", AuthorKind: worklet.AuthorHuman,
		Body: "the audit first", Answers: &answers, Choice: "hold",
	}
	wake := worklet.Wake{
		Kind:    worklet.ChangeComment,
		After:   worklet.Task{Key: "ENG-1", Project: "ENG", Title: "release"},
		Comment: &answer, AnswersDecision: asked,
		Thread: worklet.ThreadParties{AnsweredAuthor: "cy", Participants: []string{"bo"}},
	}
	byHandle := routedByHandle(t, askRecord(t, worklet.TaskPatch{Comment: &answer}, wake))
	asker, participant := byHandle["cy"], byHandle["bo"]
	if asker.Metadata[worklet.MetaVia] != string(worklet.ReasonAnswered) {
		t.Fatalf("the asker was routed under %q, so the case asserts nothing",
			asker.Metadata[worklet.MetaVia])
	}
	if asker.Owes != string(decision.InformSlack) {
		t.Errorf("the asker's wake owes %q, want %q — the answer would close on "+
			"a tracker comment and the promised post would never be made",
			asker.Owes, decision.InformSlack)
	}
	if participant.Metadata[worklet.MetaVia] == "" {
		t.Fatal("the thread participant was not routed, so the case asserts nothing")
	}
	if participant.Owes != "" {
		t.Errorf("a thread participant's copy owes %q, and it promised nobody anything",
			participant.Owes)
	}

	uninformed := wake
	uninformed.AnswersDecision = decisionFixture()
	if got := routedByHandle(t, askRecord(t, worklet.TaskPatch{Comment: &answer},
		uninformed))["cy"]; got.Owes != "" {
		t.Errorf("an answer to a decision with no inform owes %q", got.Owes)
	}
}

// A WAKE THAT OWES A SURFACE IS ITS OWN PARTITION OF THE CONVERSATION.
//
// A turn owes one surface, and a coalesced partition is one turn — so two
// answers on one item promising two different surfaces, merged, would lose one
// of the promises. The partition key cuts the conversation by the surface owed
// while the conversation identity stays the task's (`task:<id>`, never its
// key: see [worklet.Prompt.ConversationIdentity]), so the ledger still files
// every wake about the task in one thread.
func TestAnOwedWakeIsItsOwnPartitionOfTheTask(t *testing.T) {
	t.Parallel()
	meta := func(via worklet.Reason, inform string) map[string]string {
		m := map[string]string{worklet.MetaTaskID: "t-1", worklet.MetaTaskKey: "ENG-1",
			worklet.MetaVia: string(via)}
		if inform != "" {
			m[worklet.MetaInform] = inform
		}
		return m
	}
	slack := meta(worklet.ReasonAnswered, `{"surface":"slack","channel":"releases"}`)
	mattermost := meta(worklet.ReasonAnswered, `{"surface":"mattermost","channel":"eng"}`)
	plain := meta(worklet.ReasonAnswered, "")
	watcher := meta(worklet.ReasonWatcher, `{"surface":"slack","channel":"releases"}`)

	keys := map[string]string{}
	for name, m := range map[string]map[string]string{
		"slack": slack, "mattermost": mattermost, "plain": plain,
	} {
		key := (worklet.Prompt{}).PartitionKey(m, "")
		if other, clash := keys[key]; clash {
			t.Errorf("%s and %s share the partition %q, so a merge would owe one "+
				"of them", name, other, key)
		}
		keys[key] = name
		if got := (worklet.Prompt{}).ConversationIdentity(m, ""); got != "task:t-1" {
			t.Errorf("%s: the conversation identity is %q, want the task's", name, got)
		}
		if !strings.HasPrefix(key, "task:t-1") {
			t.Errorf("%s: the partition %q is not a cut of the conversation", name, key)
		}
	}
	if got, want := (worklet.Prompt{}).PartitionKey(watcher, ""),
		(worklet.Prompt{}).PartitionKey(plain, ""); got != want {
		t.Errorf("a watcher's copy partitions as %q, apart from the task's own %q",
			got, want)
	}
	if got := worklet.Owes(meta(worklet.ReasonAnswered, `{"surface":"teams","channel":"x"}`)); got != "" {
		t.Errorf("an inform naming a surface this build does not know owes %q", got)
	}
}

// ONLY AN AGENT SEAT MAY ASK THE ENGINE TO INFORM.
//
// The engine keeps the promise by holding the asker's answered turn open, and
// a person has no turn: an inform on their ask would put "posts it to
// #releases" on the answering person's card as a promise nothing keeps. The
// writer refuses it in its own decide, whichever surface wrote it.
func TestOnlyAnAgentAskerMayPromiseToInform(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	assign(t, r, "decide", "ana")
	informed := decisionFixture()
	informed.Inform = &decision.Inform{Surface: decision.InformSlack, Channel: "releases"}
	for _, kind := range []worklet.AuthorKind{worklet.AuthorOperator, worklet.AuthorHuman} {
		_, err := r.writer.UpdateTask(t.Context(), "op-"+string(kind), "decide", "ENG",
			worklet.NoIfMatch, worklet.TaskPatch{Comment: &worklet.Comment{
				ID: "c-" + string(kind), Task: "decide", Author: "someone",
				AuthorKind: kind, Body: "Friday?", Ask: "ana", Decision: informed,
				CreatedAt: wednesday,
			}}, worklet.ChangeComment, nil)
		if !errors.Is(err, worklet.ErrInvalid) || !strings.Contains(err.Error(), "only an agent seat") {
			t.Errorf("a %s asker's inform: %v — want invalid", kind, err)
		}
	}
	askOn(t, r, "op-agent", "decide", worklet.Comment{
		ID: "c-agent", Task: "decide", Author: "dev", AuthorKind: worklet.AuthorAgent,
		Body: "Friday?", Ask: "ana", Decision: informed, CreatedAt: wednesday,
	})
}

// routedByHandle parses a record and indexes every recipient's inbound.
func routedByHandle(t *testing.T, record worklet.MutationRecord) map[string]notify.Inbound {
	t.Helper()
	routed, err := worklet.NewParser(worklet.ParserOptions{}).Parse(
		t.Context(), delivery(t, record), registry(t, "ana", "bo", "cy"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	out := make(map[string]notify.Inbound, len(routed))
	for _, r := range routed {
		out[r.To.Handle] = r.Inbound
	}
	return out
}
