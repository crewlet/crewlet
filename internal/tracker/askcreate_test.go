package tracker_test

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A TASK FILED AS A QUESTION IS ONE RECORD.
//
// "Ask" and "Message" on a person's screen file an item whose whole point is
// the question on it. Written as a create and then a comment, a crash between
// the two leaves an item nobody is asked on: it wakes its assignee as work,
// `asked_of_me` never lists it, and the answer has no ask to close. So the ask
// rides the create — the log grows by the create's own two appends (the key
// counter and the task) and not by a third — and every node writes the comment
// row, the decision on it and the asked person's watch from that one record.
func TestACreateCanCarryItsAskInOneRecord(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	before, err := r.log.End(t.Context())
	if err != nil {
		t.Fatalf("read the log's end: %v", err)
	}

	task := newTask("q-1")
	task.Title = "Ship Friday or hold for the audit?"
	task.Reporter = "dev"
	task.Watchers = []string{"dev"}
	ask := tracker.Comment{
		ID: "c-q", Author: "dev", AuthorKind: tracker.AuthorAgent,
		Body: task.Title, Ask: "ana", Decision: decisionFixture(),
		CreatedAt: wednesday,
	}
	notify := tracker.Wake{
		Kind: tracker.ChangeCreated, After: task, Comment: &ask,
	}.Notify(nil)
	if _, err := r.writer.CreateTaskAsking(t.Context(), "op-q", task, ask, notify); err != nil {
		t.Fatalf("CreateTaskAsking: %v", err)
	}
	after, err := r.log.End(t.Context())
	if err != nil {
		t.Fatalf("read the log's end: %v", err)
	}
	if after-before != 2 {
		t.Fatalf("filing a question appended %d records, want the create's own "+
			"two (the key and the task) — the ask rides the task's record",
			after-before)
	}
	r.drain()

	got := r.myWork("ana")
	if len(got.AskedOfMe) != 1 || got.AskedOfMe[0].Comment != "c-q" {
		t.Fatalf("asked_of_me is %+v, want the question the item was filed as",
			got.AskedOfMe)
	}
	if row := got.AskedOfMe[0]; row.Decision == nil || !row.Open {
		t.Errorf("the ask row is %+v, want it open and carrying its decision", row)
	}
	detail, err := r.reader.Task(t.Context(), "q-1",
		tracker.DetailWants{Comments: true}, statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		t.Fatalf("read the item: %v", err)
	}
	if !slices.Contains(detail.Task.Watchers, "ana") {
		t.Errorf("the person asked is not following the item: watchers %v",
			detail.Task.Watchers)
	}
	if len(detail.Comments) != 1 || detail.Comments[0].Ask != "ana" ||
		detail.Comments[0].Decision == nil {
		t.Errorf("the thread is %+v, want the one ask with its decision",
			detail.Comments)
	}

	// AND THE PERSON ASKED IS WOKEN UNDER `asked`, which outranks being the
	// assignee — so asking the assignee reads as a question, not new work.
	task.Assignee = "ana"
	woken := tracker.Candidates(tracker.Wake{
		Kind: tracker.ChangeCreated, After: task, Comment: &ask,
	}.Notify(nil), false)
	if len(woken) == 0 || woken[0].Handle != "ana" || woken[0].Reason != tracker.ReasonAsked {
		t.Errorf("the create woke %+v, want ana first, asked", woken)
	}
}

// THE ASK A CREATE CARRIES IS A QUESTION ON THIS TASK AND NOTHING ELSE: it
// names a comment id (the row's key on every node), asks somebody, answers and
// replies to nothing — a task that does not exist yet holds no comment to
// answer — and its decision is well formed. Each is refused before the key is
// minted, so a refusal costs no numbering gap.
func TestACreatesAskIsAQuestionAndNothingElse(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	answers := "c-other"
	broken := *decisionFixture()
	broken.Options = broken.Options[:1]
	for name, ask := range map[string]tracker.Comment{
		"no id":        {Ask: "ana", Body: "?"},
		"asks nobody":  {ID: "c-1", Body: "?"},
		"answers":      {ID: "c-1", Ask: "ana", Body: "?", Answers: &answers},
		"chooses":      {ID: "c-1", Ask: "ana", Body: "?", Choice: "ship"},
		"replies":      {ID: "c-1", Ask: "ana", Body: "?", ReplyTo: &answers},
		"another task": {ID: "c-1", Ask: "ana", Body: "?", Task: "elsewhere"},
		"bad decision": {ID: "c-1", Ask: "ana", Body: "?", Decision: &broken},
	} {
		_, err := r.writer.CreateTaskAsking(t.Context(), "op-"+name,
			newTask("bad-"+strings.ReplaceAll(name, " ", "-")), ask, nil)
		if !errors.Is(err, tracker.ErrInvalid) {
			t.Errorf("%s: %v — want invalid", name, err)
		}
	}
	end, err := r.log.End(t.Context())
	if err != nil {
		t.Fatalf("read the log's end: %v", err)
	}
	// ONE RECORD — the project the harness seeded — and nothing a refused
	// create published.
	if end != 1 {
		t.Errorf("the log holds %d records after only refused creates, want "+
			"the seeded project alone", end)
	}
}

// A CREATE CARRYING ITS ASK IS RETAINED BY A BUILD THAT CANNOT READ IT.
//
// A build reading 5 decodes a create as a bare task and writes no comment row
// from it, so its copy of the item would have no question on it for good. The
// create carrying an ask is stamped at 6; a create carrying none is still 1,
// so an older node holds back only the records it would apply lossily.
func TestACreateCarryingItsAskIsRetainedByABuildThatCannotReadIt(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		ask  *tracker.Comment
		want int
	}{
		"filed as a question": {&tracker.Comment{ID: "c-q", Ask: "ana", Body: "?"}, 6},
		"a plain create":      {nil, 1},
	} {
		mutation, err := json.Marshal(tracker.TaskCreate{Task: newTask("t-1"), Comment: c.ask})
		if err != nil {
			t.Fatalf("%s: marshal: %v", name, err)
		}
		body, err := tracker.MutationRecord{
			RecordEnvelope: tracker.RecordEnvelope{
				OpID: "op-" + name, Subject: tracker.TaskSubject("t-1"),
				Op: tracker.OpCreate, CreatedAt: wednesday,
				Scope: tracker.ScopeSet{Subject: true, Container: "ENG"},
			},
			Kind: tracker.ChangeCreated, Mutation: mutation,
			Actor: "dev", ActorKind: tracker.AuthorAgent,
		}.Encode()
		if err != nil {
			t.Fatalf("%s: encode: %v", name, err)
		}
		env, err := tracker.Domain{}.Envelope(body)
		if err != nil {
			t.Fatalf("%s: envelope: %v", name, err)
		}
		if env.V != c.want {
			t.Errorf("%s: stamped %d, want %d", name, env.V, c.want)
		}
		if c.want > 5 && env.ReadableBy(5) {
			t.Errorf("%s: a build reading version 5 would apply it and drop "+
				"the question", name)
		}
	}
}

// THE PERSON A QUESTION ASKS STARTS FOLLOWING THE ITEM — the promise the tool
// and the guide both made and nothing kept — unless they MUTED it, which is
// the one gesture that says they chose not to follow it; the question still
// reaches them under `asked`. And only when the question is written: an edit
// re-sends `ask` with the rest of the comment, and must not re-watch somebody
// who has left since.
func TestAnAskMakesTheAskedPersonFollowUnlessTheyMutedIt(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	assign(t, r, "follow", "dev")
	watchers := func() []string {
		t.Helper()
		detail, err := r.reader.Task(t.Context(), "follow", tracker.DetailWants{},
			statelog.Freshness{Level: statelog.ReadStale})
		if err != nil {
			t.Fatalf("read the item: %v", err)
		}
		return detail.Task.Watchers
	}
	question := tracker.Comment{
		ID: "c-1", Task: "follow", Author: "dev", AuthorKind: tracker.AuthorAgent,
		Body: "which region?", Ask: "ana", CreatedAt: wednesday,
	}
	askOn(t, r, "op-ask", "follow", question)
	if !slices.Contains(watchers(), "ana") {
		t.Fatalf("the person asked is not following: %v", watchers())
	}

	// SHE LEAVES, and an edit of that question does not bring her back.
	if _, err := r.writer.UpdateTask(t.Context(), "op-leave", "follow", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Watch: &tracker.WatchIntent{
			Handle: "ana", Watch: false,
		}}, tracker.ChangeWatchers, nil); err != nil {
		t.Fatalf("unwatch: %v", err)
	}
	r.drain()
	edited := question
	edited.Body = "which region, eu or us?"
	askOn(t, r, "op-edit", "follow", edited)
	if slices.Contains(watchers(), "ana") {
		t.Errorf("an edit of the question re-watched somebody who left: %v", watchers())
	}
	// NOR DOES A NEW QUESTION: she muted the item.
	again := question
	again.ID, again.Body = "c-2", "and the instance size?"
	askOn(t, r, "op-again", "follow", again)
	if slices.Contains(watchers(), "ana") {
		t.Errorf("a new question overrode a mute: %v", watchers())
	}
}
