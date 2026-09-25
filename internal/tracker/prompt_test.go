package tracker_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/tracker"
)

// EVERY ROUTING REASON PRODUCES A PROMPT, AND EVERY PRIMARY ONE ASKS.
//
// # Why this is a walk over the enum rather than a list of cases
//
// The router distinguishes eighteen reasons and the prompt frames three, so
// most of them share a frame by default. That is deliberate — what a RECIPIENT
// needs to be told collapses to "somebody is asking you", "this is your work"
// and "this is activity you follow" — but it means a reason added later
// silently takes the weakest frame, and the seat is told it is merely watching
// something it was in fact assigned. Walking the enum is what makes that
// visible: a new reason has to be classified here or the test names it.
func TestEveryReasonIsFramedAndOnlyThePrimaryOnesAsk(t *testing.T) {
	t.Parallel()
	for _, reason := range tracker.Reasons {
		t.Run(string(reason), func(t *testing.T) {
			t.Parallel()
			n := promptNotification(reason, tracker.ChangeStatus)
			body := tracker.Prompt{}.Build(n, nil)
			if strings.TrimSpace(body) == "" {
				t.Fatal("this reason renders an empty prompt, so the seat is " +
					"woken with nothing to act on")
			}
			if !strings.Contains(body, "**Task:**") {
				t.Error("the prompt names no task, and every rule in it rests " +
					"on the seat knowing which one")
			}
			if got := (tracker.Prompt{}).Addressed(n); got != reason.Addressed() {
				t.Errorf("Addressed = %v and this reason is primary=%v — an "+
					"addressed turn may not end in silence, and marking a "+
					"watcher addressed makes a seat answer every field change "+
					"in its unit's projects", got, reason.Addressed())
			}
		})
	}
}

// A FALLBACK EXPLAINS ITSELF AND ASKS FOR A DECISION.
//
// A directed routing carries its own signal — being assigned or named says
// what is wanted — while a fallback says only that nobody else here was
// available, which is a fact about the org chart rather than about the work.
// Left unexplained, a lead reads it as "this is mine" and quietly absorbs
// every unowned task in their project.
func TestAFallbackRoutingExplainsItself(t *testing.T) {
	t.Parallel()
	body := tracker.Prompt{}.Build(
		promptNotification(tracker.ReasonLeadFallback, tracker.ChangeCreated), nil)
	for _, want := range []string{
		"Why you received this", "FALLBACK", "Delegate", "Escalate",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the fallback prompt does not say %q:\n%s", want, body)
		}
	}
	// AND NOT TWO SETS OF STEPS. The delegate / take it / escalate block
	// IS this seat's instruction, and a second list under a second
	// heading makes a lead read both and follow neither.
	if strings.Contains(body, "How to handle this") {
		t.Error("a fallback gets both the decision block and the general " +
			"handling list, which is two sets of steps for one wake")
	}
}

// A REMOVED TASK SENDS NOBODY TO READ IT.
//
// Fetching a task that no longer exists costs the seat a round and a failed
// tool call, and the answer it gets back says nothing about what to do.
func TestARemovedTaskIsNotSentForRecon(t *testing.T) {
	t.Parallel()
	n := promptNotification(tracker.ReasonAssignee, tracker.ChangeRemoved)
	body := tracker.Prompt{}.Build(n, nil)
	if strings.Contains(body, "Get full context") {
		t.Errorf("a removed task sends the seat to read it:\n%s", body)
	}
	if !strings.Contains(body, "no longer exists") {
		t.Errorf("the prompt does not say the task is gone:\n%s", body)
	}
}

// A REPAIR SAYS IT IS ONE.
//
// A late notice reaches somebody hours after the change, and a reader who
// cannot tell that from a fresh event re-reads a thread looking for what just
// moved.
func TestALateNoticeSaysWhyItIsLate(t *testing.T) {
	t.Parallel()
	n := promptNotification(tracker.ReasonUnblocked, tracker.ChangeRelations)
	n.Metadata[tracker.MetaLate] = "true"
	body := tracker.Prompt{}.Build(n, nil)
	if !strings.Contains(body, "repair") {
		t.Errorf("a late notice reads as a fresh change:\n%s", body)
	}
}

// THE ONE REASON THAT WAKES ITS OWN ACTOR IS THE ONE THAT IS A CONSEQUENCE.
//
// A person who closed a blocker learns from this that the task it was blocking
// is now workable — which they could not have derived from their own write.
// Every other change here is something the actor already knows, and waking
// them about it is a seat reading its own comment back to itself.
func TestOnlyAnUnblockedNoticeWakesItsActor(t *testing.T) {
	t.Parallel()
	for _, reason := range tracker.Reasons {
		want := reason == tracker.ReasonUnblocked
		if got := (tracker.Prompt{}).WakesActor(string(reason)); got != want {
			t.Errorf("WakesActor(%s) = %v, want %v", reason, got, want)
		}
	}
}

// A CHANGE WAKE TELLS THE SEAT WHAT MOVED, NOT ONLY THAT SOMETHING DID.
//
// [tracker.Notify] has carried the deltas since the first wake this engine
// wrote, under a comment calling them "the deltas a card renders" — and no
// card rendered them. The prompt said "The status changed by ana." and
// stopped, so the seat's only route to the value was a `get_work_item` round,
// which cannot recover the side the field moved FROM: that side is nowhere on
// the task.
//
// THROUGH THE PARSER rather than a hand-built [notify.Inbound], because the
// gap was exactly the seam between the two — the record holds typed deltas,
// the prompt is handed a string map, and each half read as complete on its
// own.
func TestAChangeWakeNamesWhatMoved(t *testing.T) {
	t.Parallel()
	record := parseRecord(&tracker.Notify{
		Kind: tracker.ChangeStatus,
		Fields: map[string]tracker.Delta{
			"status":   {From: "todo", To: "in_progress"},
			"assignee": {From: "", To: "cy"},
		},
		Snapshot: tracker.Snapshot{
			Key: "ENG-1", Project: "ENG", Title: "wire it",
			Status: tracker.StatusInProgress, Assignee: "cy",
		},
	})
	body := promptFor(t, record)
	for _, want := range []string{
		"## What changed",
		"- status: todo → in_progress",
		// AN EMPTY SIDE IS AN EM DASH, which is what every other surface
		// draws for one: a blank reads as a rendering fault where the
		// dash reads as an assignment.
		"- assignee: — → cy",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the prompt does not carry %q:\n%s", want, body)
		}
	}
	// IN FIELD ORDER, so one record renders one prompt wherever it is
	// delivered and however often it is redelivered — a map's range order
	// would make the bytes the event store keeps differ per delivery.
	if strings.Index(body, "- assignee:") > strings.Index(body, "- status:") {
		t.Errorf("the deltas are not sorted by field name:\n%s", body)
	}
}

// AND THE EXCERPT IS STILL THERE, under the same heading.
//
// The two answer different questions — the deltas are what moved and the
// excerpt is what was said about it (a new task's description, a purge's
// reason) — so the block carries both rather than one displacing the other.
func TestAChangeWakeKeepsTheExcerptBesideTheDeltas(t *testing.T) {
	t.Parallel()
	record := parseRecord(&tracker.Notify{
		Kind:    tracker.ChangeCreated,
		Excerpt: "the payment webhook drops retries",
		Fields: map[string]tracker.Delta{
			"status": {From: "", To: "todo"},
		},
		Snapshot: tracker.Snapshot{
			Key: "ENG-1", Project: "ENG", Title: "wire it",
			Status: tracker.StatusTodo, Assignee: "cy",
		},
	})
	body := promptFor(t, record)
	if !strings.Contains(body, "- status: — → todo") {
		t.Errorf("the create does not name what it set:\n%s", body)
	}
	if !strings.Contains(body, "the payment webhook drops retries") {
		t.Errorf("the deltas displaced the excerpt:\n%s", body)
	}
}

// AN ASKED WAKE RENDERS THE OPTIONS AND THE ANSWERING CALL, WHOLE.
//
// Every part of the call is something the seat would otherwise go and find:
// the ask's comment id is in a thread it would read for no other reason, and
// an option's id is typed back exactly or refused. So the options are listed
// by id, the recommendation is marked, and the call is written out with the
// item, the ask's id and a choice — the seat edits one value and sends it.
func TestAnAskedWakeRendersTheOptionsAndTheAnsweringCall(t *testing.T) {
	t.Parallel()
	ask := tracker.Comment{
		ID: "c-ask", Task: "t-1", Author: "ana", AuthorKind: tracker.AuthorAgent,
		Body: "The audit starts Monday.", Ask: "cy", Decision: decisionFixture(),
	}
	record := askRecord(t, tracker.TaskPatch{Comment: &ask}, tracker.Wake{
		Kind:    tracker.ChangeComment,
		After:   tracker.Task{Key: "ENG-1", Project: "ENG", Title: "release"},
		Comment: &ask, Thread: tracker.ThreadParties{Asked: "cy"},
	})
	prompt := promptFor(t, record)
	for _, want := range []string{
		"You were asked a question",
		"## The decision you are asked for",
		"**Question:** Ship on Friday or hold for the audit?",
		"approver — your answer IS the decision",
		"- `ship` — Ship Friday *(recommended)*",
		"- `hold` — Hold for the audit: about a week",
		"The audit reviews last quarter's code.",
		`{"item":"ENG-1","answers":"c-ask","choice":"ship","body":`,
		"one of: ship, hold",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the asked wake does not say %q:\n%s", want, prompt)
		}
	}

	// A TASK FILED AS THE QUESTION renders the same block: the ask rides
	// the create's own payload.
	filed := tracker.Task{ID: "t-1", Key: "ENG-1", Project: "ENG",
		Title: "Ship on Friday?", Assignee: "cy"}
	created, err := json.Marshal(tracker.TaskCreate{Task: filed, Comment: &ask})
	if err != nil {
		t.Fatalf("marshal the create: %v", err)
	}
	record = parseRecord(tracker.Wake{
		Kind: tracker.ChangeCreated, After: filed, Comment: &ask,
	}.Notify(nil))
	record.Op, record.Mutation = tracker.OpCreate, created
	if got := promptFor(t, record); !strings.Contains(got, "You were asked a question") ||
		!strings.Contains(got, `"answers":"c-ask","choice":"ship"`) {
		t.Errorf("a task filed as a question does not ask it:\n%s", got)
	}

	// A WATCHER SEES THE REMARK, NOT THE BALLOT: the options are for the
	// person who owes the answer.
	watcher := promptNotification(tracker.ReasonWatcher, tracker.ChangeComment)
	watcher.Metadata[tracker.MetaDecision] = `{"question":"q","options":[{"id":"a","label":"A"},{"id":"b","label":"B"}],"role":"approver"}`
	if got := (tracker.Prompt{}).Build(watcher, nil); strings.Contains(got, "The decision you are asked for") {
		t.Errorf("a watcher was handed the answering call:\n%s", got)
	}
}

// AN ANSWERED WAKE RENDERS THE CHOICE BY ITS LABEL, and the channel the asker
// promised to report it in — the asker is the one that stopped on this branch,
// and this wake is what it was waiting for.
func TestAnAnsweredWakeRendersTheChoice(t *testing.T) {
	t.Parallel()
	answers := "c-ask"
	decision := decisionFixture()
	decision.Inform = &tracker.Inform{Surface: tracker.InformSlack, Channel: "releases"}
	answer := tracker.Comment{
		ID: "c-ans", Task: "t-1", Author: "ana", AuthorKind: tracker.AuthorHuman,
		Body: "the audit first", Answers: &answers, Choice: "hold",
	}
	record := askRecord(t, tracker.TaskPatch{Comment: &answer}, tracker.Wake{
		Kind:    tracker.ChangeComment,
		After:   tracker.Task{Key: "ENG-1", Project: "ENG", Title: "release"},
		Comment: &answer, AnswersDecision: decision,
		Thread: tracker.ThreadParties{AnsweredAuthor: "cy"},
	})
	prompt := promptFor(t, record)
	for _, want := range []string{
		"A question you asked on a task was answered.",
		"Chose “Hold for the audit”: the audit first",
		"**You asked:** Ship on Friday or hold for the audit?",
		"**They chose:** Hold for the audit (`hold`)",
		"releases on slack",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the answered wake does not say %q:\n%s", want, prompt)
		}
	}
	// AN ANSWER IN PROSE is still an answer, and the wake says it is one
	// rather than rendering an empty choice.
	prose := answer
	prose.Choice, prose.Body = "", "neither — split the release"
	record = askRecord(t, tracker.TaskPatch{Comment: &prose}, tracker.Wake{
		Kind:    tracker.ChangeComment,
		After:   tracker.Task{Key: "ENG-1", Project: "ENG", Title: "release"},
		Comment: &prose, AnswersDecision: decisionFixture(),
		Thread: tracker.ThreadParties{AnsweredAuthor: "cy"},
	})
	if got := promptFor(t, record); !strings.Contains(got, "answered in prose") ||
		strings.Contains(got, "They chose") {
		t.Errorf("an answer in prose renders as:\n%s", got)
	}
}

// askRecord is a comment record as the writer publishes it: the patch as its
// payload, and the notification its wake builds.
func askRecord(t *testing.T, patch tracker.TaskPatch, wake tracker.Wake) tracker.MutationRecord {
	t.Helper()
	mutation, err := json.Marshal(patch)
	if err != nil {
		t.Fatalf("marshal the patch: %v", err)
	}
	record := parseRecord(wake.Notify(nil))
	record.Mutation = mutation
	return record
}

// promptFor routes a record and builds the first recipient's prompt.
func promptFor(t *testing.T, record tracker.MutationRecord) string {
	t.Helper()
	routed, err := tracker.NewParser(tracker.ParserOptions{}).Parse(
		t.Context(), delivery(t, record), registry(t, "ana", "cy"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(routed) == 0 {
		t.Fatal("the change woke nobody, so the case asserts nothing")
	}
	return tracker.Prompt{}.Build(routed[0].Inbound, nil)
}

func promptNotification(reason tracker.Reason, kind tracker.ChangeKind) notify.Inbound {
	return notify.Inbound{
		Source:    tracker.Source,
		EventType: string(kind),
		Sender:    "ana",
		Subject:   "ENG-1 " + string(kind) + ": wire it",
		Metadata: map[string]string{
			tracker.MetaVia:        string(reason),
			tracker.MetaTaskKey:    "ENG-1",
			tracker.MetaTaskID:     "t-1",
			tracker.MetaProject:    "ENG",
			tracker.MetaChangeKind: string(kind),
			tracker.MetaStatus:     "in_progress",
			tracker.MetaAssignee:   "cy",
		},
	}
}
