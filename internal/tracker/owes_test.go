package tracker_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/tracker"
)

// AN INFORMED ANSWER OWES ITS ASKER'S CHAT SURFACE, AND NOBODY ELSE'S TURN.
//
// The asker said it would report the outcome in a channel, and the person who
// answered is told it will: the asker's `answered` wake names that surface in
// [notify.Inbound.Owes], which is what the turn engine holds the turn open on.
// A watcher on the same item receives the same record under its own reason and
// owes nothing — the promise was the asker's — and an answer to a decision
// that asked for no inform owes nothing at all.
func TestAnInformedAnswerOwesItsAskerAndNobodyElse(t *testing.T) {
	t.Parallel()
	answers := "c-ask"
	decision := decisionFixture()
	decision.Inform = &tracker.Inform{Surface: tracker.InformSlack, Channel: "releases"}
	answer := tracker.Comment{
		ID: "c-ans", Task: "t-1", Author: "ana", AuthorKind: tracker.AuthorHuman,
		Body: "the audit first", Answers: &answers, Choice: "hold",
	}
	wake := tracker.Wake{
		Kind: tracker.ChangeComment,
		After: tracker.Task{Key: "ENG-1", Project: "ENG", Title: "release",
			Watchers: []string{"bo"}},
		Comment: &answer, AnswersDecision: decision,
		Thread: tracker.ThreadParties{AnsweredAuthor: "cy"},
	}
	byHandle := routedByHandle(t, askRecord(t, tracker.TaskPatch{Comment: &answer}, wake))
	asker, watcher := byHandle["cy"], byHandle["bo"]
	if asker.Metadata[tracker.MetaVia] != string(tracker.ReasonAnswered) {
		t.Fatalf("the asker was routed under %q, so the case asserts nothing",
			asker.Metadata[tracker.MetaVia])
	}
	if asker.Owes != string(tracker.InformSlack) {
		t.Errorf("the asker's wake owes %q, want %q — the answer would close on "+
			"a tracker comment and the promised post would never be made",
			asker.Owes, tracker.InformSlack)
	}
	if watcher.Metadata[tracker.MetaVia] == "" {
		t.Fatal("the watcher was not routed, so the case asserts nothing")
	}
	if watcher.Owes != "" {
		t.Errorf("a watcher's copy owes %q — it promised nobody anything", watcher.Owes)
	}

	uninformed := wake
	uninformed.AnswersDecision = decisionFixture()
	if got := routedByHandle(t, askRecord(t, tracker.TaskPatch{Comment: &answer},
		uninformed))["cy"]; got.Owes != "" {
		t.Errorf("an answer to a decision with no inform owes %q", got.Owes)
	}
}

// A WAKE THAT OWES A SURFACE IS ITS OWN PARTITION OF THE CONVERSATION.
//
// A turn owes one surface, and a coalesced partition is one turn — so two
// answers on one item promising two different surfaces, merged, would lose one
// of the promises. The partition key cuts the conversation by the surface owed
// while the conversation identity stays the task key, so the ledger still files
// every wake about ENG-1 in one thread.
func TestAnOwedWakeIsItsOwnPartitionOfTheTask(t *testing.T) {
	t.Parallel()
	meta := func(via tracker.Reason, inform string) map[string]string {
		m := map[string]string{tracker.MetaTaskKey: "ENG-1", tracker.MetaVia: string(via)}
		if inform != "" {
			m[tracker.MetaInform] = inform
		}
		return m
	}
	slack := meta(tracker.ReasonAnswered, `{"surface":"slack","channel":"releases"}`)
	mattermost := meta(tracker.ReasonAnswered, `{"surface":"mattermost","channel":"eng"}`)
	plain := meta(tracker.ReasonAnswered, "")
	watcher := meta(tracker.ReasonWatcher, `{"surface":"slack","channel":"releases"}`)

	keys := map[string]string{}
	for name, m := range map[string]map[string]string{
		"slack": slack, "mattermost": mattermost, "plain": plain,
	} {
		key := (tracker.Prompt{}).PartitionKey(m, "")
		if other, clash := keys[key]; clash {
			t.Errorf("%s and %s share the partition %q, so a merge would owe one "+
				"of them", name, other, key)
		}
		keys[key] = name
		if got := (tracker.Prompt{}).ConversationIdentity(m, ""); got != "ENG-1" {
			t.Errorf("%s: the conversation identity is %q, want the task key", name, got)
		}
		if !strings.HasPrefix(key, "ENG-1") {
			t.Errorf("%s: the partition %q is not a cut of the conversation", name, key)
		}
	}
	if got, want := (tracker.Prompt{}).PartitionKey(watcher, ""),
		(tracker.Prompt{}).PartitionKey(plain, ""); got != want {
		t.Errorf("a watcher's copy partitions as %q, apart from the task's own %q",
			got, want)
	}
	if got := tracker.Owes(meta(tracker.ReasonAnswered, `{"surface":"teams","channel":"x"}`)); got != "" {
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
	informed.Inform = &tracker.Inform{Surface: tracker.InformSlack, Channel: "releases"}
	for _, kind := range []tracker.AuthorKind{tracker.AuthorOperator, tracker.AuthorHuman} {
		_, err := r.writer.UpdateTask(t.Context(), "op-"+string(kind), "decide", "ENG",
			tracker.NoIfMatch, tracker.TaskPatch{Comment: &tracker.Comment{
				ID: "c-" + string(kind), Task: "decide", Author: "someone",
				AuthorKind: kind, Body: "Friday?", Ask: "ana", Decision: informed,
				CreatedAt: wednesday,
			}}, tracker.ChangeComment, nil)
		if !errors.Is(err, tracker.ErrInvalid) || !strings.Contains(err.Error(), "only an agent seat") {
			t.Errorf("a %s asker's inform: %v — want invalid", kind, err)
		}
	}
	askOn(t, r, "op-agent", "decide", tracker.Comment{
		ID: "c-agent", Task: "decide", Author: "dev", AuthorKind: tracker.AuthorAgent,
		Body: "Friday?", Ask: "ana", Decision: informed, CreatedAt: wednesday,
	})
}

// routedByHandle parses a record and indexes every recipient's inbound.
func routedByHandle(t *testing.T, record tracker.MutationRecord) map[string]notify.Inbound {
	t.Helper()
	routed, err := tracker.NewParser(tracker.ParserOptions{}).Parse(
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
