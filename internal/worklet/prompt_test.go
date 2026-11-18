package worklet_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/decision"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/worklet"
)

// EVERY ROUTING REASON PRODUCES A PROMPT, AND EVERY PRIMARY ONE ASKS.
//
// A walk over the enum, not a list of cases: eighteen reasons share three
// frames, so a reason added later would silently take the weakest frame and
// tell an assignee it is merely watching. Here a new reason must be classified
// or the test names it.
func TestEveryReasonIsFramedAndOnlyThePrimaryOnesAsk(t *testing.T) {
	t.Parallel()
	for _, reason := range worklet.Reasons {
		t.Run(string(reason), func(t *testing.T) {
			t.Parallel()
			n := promptNotification(reason, worklet.ChangeStatus)
			body := worklet.Prompt{}.Build(n, nil)
			if strings.TrimSpace(body) == "" {
				t.Fatal("this reason renders an empty prompt, so the seat is " +
					"woken with nothing to act on")
			}
			if !strings.Contains(body, "**Task:**") {
				t.Error("the prompt names no task, and every rule in it rests " +
					"on the seat knowing which one")
			}
			if got := (worklet.Prompt{}).Addressed(n); got != reason.Addressed() {
				t.Errorf("Addressed = %v and this reason is primary=%v — an "+
					"addressed turn may not end in silence, and marking a "+
					"watcher addressed makes a seat answer every field change "+
					"in its unit's projects", got, reason.Addressed())
			}
		})
	}
}

// A FALLBACK EXPLAINS ITSELF AND ASKS FOR A DECISION: it says only that nobody
// else was available, and unexplained a lead absorbs every unowned task in
// their project.
func TestAFallbackRoutingExplainsItself(t *testing.T) {
	t.Parallel()
	body := worklet.Prompt{}.Build(
		promptNotification(worklet.ReasonLeadFallback, worklet.ChangeCreated), nil)
	for _, want := range []string{
		"Why you received this", "FALLBACK", "Delegate", "Escalate",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the fallback prompt does not say %q:\n%s", want, body)
		}
	}
	// And not two sets of steps: a second list makes a lead follow neither.
	if strings.Contains(body, "How to handle this") {
		t.Error("a fallback gets both the decision block and the general " +
			"handling list, which is two sets of steps for one wake")
	}
}

// THE HANDLING BLOCK NEVER CONTRADICTS THE DELIVERY CHECK, AND NEVER HANDS A
// COLLEAGUE'S TASK AWAY. Both walked over every reason:
//
//   - Staying out is for an unaddressed wake only: an addressed one may not end
//     in silence, so telling it to stay out is sent back for a second round.
//   - Reassigning is for the task's holder only (the assignee, and the
//     blocker's assignee): everyone else is asked about a task not theirs to
//     move, and a watcher is not asked at all.
//
// The set is written out rather than read off the package, so a reason that
// starts or stops offering the gesture is decided in this list.
func TestTheHandlingBlockFitsTheReason(t *testing.T) {
	t.Parallel()
	const (
		stayOut = "stay silent"
		decline = "reassign it to the right teammate"
	)
	holds := map[worklet.Reason]bool{
		worklet.ReasonAssignee: true,
		worklet.ReasonBlocking: true,
	}
	for _, reason := range worklet.Reasons {
		if reason == worklet.ReasonLeadFallback {
			// Its delegate / take it / escalate block is the whole
			// instruction, and is pinned on its own above.
			continue
		}
		body := worklet.Prompt{}.Build(promptNotification(reason, worklet.ChangeComment), nil)
		if !strings.Contains(body, "## How to handle this") {
			t.Fatalf("%s renders no handling block, so the case asserts nothing:\n%s",
				reason, body)
		}
		if got := strings.Contains(body, stayOut); got == reason.Addressed() {
			t.Errorf("%s: told to stay silent = %v, and the reason is addressed = %v "+
				"— an addressed wake may not end in silence:\n%s",
				reason, got, reason.Addressed(), body)
		}
		if got := strings.Contains(body, decline); got != holds[reason] {
			t.Errorf("%s: told to reassign the task = %v, and the seat holds it = %v:\n%s",
				reason, got, holds[reason], body)
		}
		if holds[reason] && !reason.Addressed() {
			t.Errorf("%s offers a decline, and nobody is waiting on it to decline", reason)
		}
		if strings.Contains(body, "go quiet") || strings.Contains(body, "Going silent") {
			t.Errorf("%s restates the engine's own delivery rule:\n%s", reason, body)
		}
	}
}

// A REMOVED TASK SENDS NOBODY TO READ IT: the fetch would cost a round and a
// failed tool call.
func TestARemovedTaskIsNotSentForRecon(t *testing.T) {
	t.Parallel()
	n := promptNotification(worklet.ReasonAssignee, worklet.ChangeRemoved)
	body := worklet.Prompt{}.Build(n, nil)
	if strings.Contains(body, "Get full context") {
		t.Errorf("a removed task sends the seat to read it:\n%s", body)
	}
	if !strings.Contains(body, "no longer exists") {
		t.Errorf("the prompt does not say the task is gone:\n%s", body)
	}
}

// A REPAIR SAYS IT IS ONE: arriving hours late, an unmarked one sends the
// reader through the thread for what just moved.
func TestALateNoticeSaysWhyItIsLate(t *testing.T) {
	t.Parallel()
	n := promptNotification(worklet.ReasonUnblocked, worklet.ChangeRelations)
	n.Metadata[worklet.MetaLate] = "true"
	body := worklet.Prompt{}.Build(n, nil)
	if !strings.Contains(body, "repair") {
		t.Errorf("a late notice reads as a fresh change:\n%s", body)
	}
}

// EVERY COMMENT WAKE CARRIES THE COMMENT, whatever reason routed it: to the
// assignee, reporter and watchers the change is the remark too, and the wake
// reports itself as carrying its text, so quoting it only for the four
// "somebody said something to you" reasons left the rest with nothing.
func TestEveryCommentWakeCarriesTheComment(t *testing.T) {
	t.Parallel()
	const said = "please also cover the rollback path"
	for _, kind := range []worklet.ChangeKind{worklet.ChangeComment, worklet.ChangeCommentEdited} {
		for _, reason := range worklet.Reasons {
			n := promptNotification(reason, kind)
			n.Body = said
			if (worklet.Prompt{}).RequiresRecon(n) {
				t.Errorf("a %s wake routed as %s asks for recon although it carries its text",
					kind, reason)
			}
			if got := (worklet.Prompt{}).Build(n, nil); !strings.Contains(got, said) {
				t.Errorf("a %s wake routed as %s does not say what was said:\n%s",
					kind, reason, got)
			}
		}
	}
}

// THE ONE REASON THAT WAKES ITS OWN ACTOR IS THE ONE THAT IS A CONSEQUENCE:
// whoever closed a blocker learns its task is now workable; every other change
// the actor already knows.
//
// Every copy carries the event type `status`, and only the routing reason on
// each copy tells them apart, so a reading of the event type answers no for
// the one copy that has to reach the actor.
func TestOnlyAnUnblockedNoticeWakesItsActor(t *testing.T) {
	t.Parallel()
	for _, reason := range worklet.Reasons {
		want := reason == worklet.ReasonUnblocked
		n := promptNotification(reason, worklet.ChangeStatus)
		if got := (worklet.Prompt{}).WakesActor(n); got != want {
			t.Errorf("WakesActor(a status change's copy routed as %s) = %v, want %v", reason, got, want)
		}
	}
}

// AND THE SPINE LETS THAT COPY THROUGH TO THE ACTOR'S OWN SEAT: the
// self-action guard drops an actor's own notification unless the prompt says
// the copy reaches its actor, so a spine reading the event type instead would
// drop the one notice the router kept.
func TestTheSpineDeliversTheUnblockedCopyToItsActor(t *testing.T) {
	t.Parallel()
	reg := firstPartyRegistry(t, "eng")
	seat, ok := reg.ByHandle("eng")
	if !ok {
		t.Fatal("the registry does not hold the seat")
	}
	prompts := notify.NewPrompts(worklet.Prompt{})
	for reason, want := range map[worklet.Reason]bool{
		worklet.ReasonUnblocked: true,
		worklet.ReasonAssignee:  false,
		worklet.ReasonWatcher:   false,
	} {
		n := promptNotification(reason, worklet.ChangeStatus)
		n.Metadata[notify.ActorField] = "eng"
		got, why := notify.Deliverable(prompts, reg, n, seat)
		if got != want {
			t.Errorf("a copy routed as %s to its own actor: deliverable = %v (%s), want %v",
				reason, got, why, want)
		}
	}
}

// A CHANGE WAKE TELLS THE SEAT WHAT MOVED, NOT ONLY THAT SOMETHING DID: a
// `get_work_item` round cannot recover the side a field moved FROM.
//
// Through the parser rather than a hand-built [notify.Inbound]: the seam
// between typed deltas and the prompt's string map is where they go missing.
func TestAChangeWakeNamesWhatMoved(t *testing.T) {
	t.Parallel()
	record := parseRecord(&worklet.Notify{
		Kind: worklet.ChangeAssignee,
		Fields: map[string]worklet.Delta{
			"status":   {From: "todo", To: "in_progress"},
			"assignee": {From: "", To: "cy"},
		},
		Snapshot: worklet.Snapshot{
			Key: "ENG-1", Project: "ENG", Title: "wire it",
			Status: worklet.StatusInProgress, Assignee: "cy",
		},
	})
	body := promptFor(t, record)
	for _, want := range []string{
		"## What changed",
		"- status: todo → in_progress",
		// An empty side is an em dash, as on every other surface: a blank reads
		// as a rendering fault.
		"- assignee: — → cy",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the prompt does not carry %q:\n%s", want, body)
		}
	}
	// In field order, so every delivery of one record renders the same bytes
	// the event store keeps.
	if strings.Index(body, "- assignee:") > strings.Index(body, "- status:") {
		t.Errorf("the deltas are not sorted by field name:\n%s", body)
	}
}

// AND THE EXCERPT IS STILL THERE, under the same heading: the deltas are what
// moved and the excerpt what was said about it, so neither displaces the other.
func TestAChangeWakeKeepsTheExcerptBesideTheDeltas(t *testing.T) {
	t.Parallel()
	record := parseRecord(&worklet.Notify{
		Kind:    worklet.ChangeCreated,
		Excerpt: "the payment webhook drops retries",
		Fields: map[string]worklet.Delta{
			"status": {From: "", To: "todo"},
		},
		Snapshot: worklet.Snapshot{
			Key: "ENG-1", Project: "ENG", Title: "wire it",
			Status: worklet.StatusTodo, Assignee: "cy",
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

// AN ASKED WAKE RENDERS THE OPTIONS AND THE ANSWERING CALL, WHOLE: options by
// id with the recommendation marked, and the call with the item, the ask's id
// and a choice, so the seat edits one value and sends it rather than hunting
// for ids.
func TestAnAskedWakeRendersTheOptionsAndTheAnsweringCall(t *testing.T) {
	t.Parallel()
	ask := worklet.Comment{
		ID: "c-ask", Task: "t-1", Author: "ana", AuthorKind: worklet.AuthorAgent,
		Body: "The audit starts Monday.", Ask: "cy", Decision: decisionFixture(),
	}
	record := askRecord(t, worklet.TaskPatch{Comment: &ask}, worklet.Wake{
		Kind:    worklet.ChangeComment,
		After:   worklet.Task{Key: "ENG-1", Project: "ENG", Title: "release"},
		Comment: &ask, Thread: worklet.ThreadParties{Asked: "cy"},
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
		`one of "ship" or "hold"`,
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the asked wake does not say %q:\n%s", want, prompt)
		}
	}

	// A TASK FILED AS THE QUESTION renders the same block: the ask rides
	// the create's own payload.
	filed := worklet.Task{ID: "t-1", Key: "ENG-1", Project: "ENG",
		Title: "Ship on Friday?", Assignee: "cy"}
	created, err := json.Marshal(worklet.TaskCreate{Task: filed, Comment: &ask})
	if err != nil {
		t.Fatalf("marshal the create: %v", err)
	}
	record = parseRecord(worklet.Wake{
		Kind: worklet.ChangeCreated, After: filed, Comment: &ask,
	}.Notify(nil))
	record.Op, record.Mutation = worklet.OpCreate, created
	if got := promptFor(t, record); !strings.Contains(got, "You were asked a question") ||
		!strings.Contains(got, `"answers":"c-ask","choice":"ship"`) {
		t.Errorf("a task filed as a question does not ask it:\n%s", got)
	}

	// A WATCHER SEES THE REMARK, NOT THE BALLOT: the options are for the
	// person who owes the answer.
	watcher := promptNotification(worklet.ReasonWatcher, worklet.ChangeComment)
	watcher.Metadata[worklet.MetaDecision] = `{"question":"q","options":[{"id":"a","label":"A"},{"id":"b","label":"B"}],"role":"approver"}`
	if got := (worklet.Prompt{}).Build(watcher, nil); strings.Contains(got, "The decision you are asked for") {
		t.Errorf("a watcher was handed the answering call:\n%s", got)
	}
}

// AN ANSWERED WAKE RENDERS THE CHOICE BY ITS LABEL, and the channel the asker
// promised to report it in — the asker is the one that stopped on this branch,
// and this wake is what it was waiting for.
func TestAnAnsweredWakeRendersTheChoice(t *testing.T) {
	t.Parallel()
	answers := "c-ask"
	asked := decisionFixture()
	asked.Inform = &decision.Inform{Surface: decision.InformSlack, Channel: "releases"}
	answer := worklet.Comment{
		ID: "c-ans", Task: "t-1", Author: "ana", AuthorKind: worklet.AuthorHuman,
		Body: "the audit first", Answers: &answers, Choice: "hold",
	}
	record := askRecord(t, worklet.TaskPatch{Comment: &answer}, worklet.Wake{
		Kind:    worklet.ChangeComment,
		After:   worklet.Task{Key: "ENG-1", Project: "ENG", Title: "release"},
		Comment: &answer, AnswersDecision: asked,
		Thread: worklet.ThreadParties{AnsweredAuthor: "cy"},
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
	record = askRecord(t, worklet.TaskPatch{Comment: &prose}, worklet.Wake{
		Kind:    worklet.ChangeComment,
		After:   worklet.Task{Key: "ENG-1", Project: "ENG", Title: "release"},
		Comment: &prose, AnswersDecision: decisionFixture(),
		Thread: worklet.ThreadParties{AnsweredAuthor: "cy"},
	})
	if got := promptFor(t, record); !strings.Contains(got, "answered in prose") ||
		strings.Contains(got, "They chose") {
		t.Errorf("an answer in prose renders as:\n%s", got)
	}
}

// askRecord is a comment record as the writer publishes it: the patch as its
// payload, and the notification its wake builds.
func askRecord(t *testing.T, patch worklet.TaskPatch, wake worklet.Wake) worklet.MutationRecord {
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
func promptFor(t *testing.T, record worklet.MutationRecord) string {
	t.Helper()
	routed, err := worklet.NewParser(worklet.ParserOptions{}).Parse(
		t.Context(), delivery(t, record), registry(t, "ana", "cy"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(routed) == 0 {
		t.Fatal("the change woke nobody, so the case asserts nothing")
	}
	return worklet.Prompt{}.Build(routed[0].Inbound, nil)
}

func promptNotification(reason worklet.Reason, kind worklet.ChangeKind) notify.Inbound {
	return notify.Inbound{
		Source:    worklet.Source,
		EventType: string(kind),
		Sender:    "ana",
		Subject:   "ENG-1 " + string(kind) + ": wire it",
		Metadata: map[string]string{
			worklet.MetaVia:        string(reason),
			worklet.MetaTaskKey:    "ENG-1",
			worklet.MetaTaskID:     "t-1",
			worklet.MetaProject:    "ENG",
			worklet.MetaChangeKind: string(kind),
			worklet.MetaStatus:     "in_progress",
			worklet.MetaAssignee:   "cy",
		},
	}
}
