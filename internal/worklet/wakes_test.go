package worklet_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/worklet"
)

// EVERY CHANGE KIND IS CLASSIFIED as a task commit or not. Nothing links
// taskCommit's allowlist to ChangeKinds at compile time, and a kind missing
// from the allowlist routes to nobody, silently; this table is that link, so a
// new kind fails here until somebody decides which half it is in.
func TestEveryChangeKindIsClassifiedAsTaskOrNot(t *testing.T) {
	t.Parallel()
	// The kinds that are NOT about a task's own routing. Everything else
	// must be a task commit.
	notTasks := map[worklet.ChangeKind]bool{
		// The object wake, routed off its own snapshot field rather
		// than off a task's assignee or watchers.
		worklet.ChangePrioritised: true,

		// Bookkeeping and surfaces of their own: a project's settings,
		// a saved view and the catalogue are read where they live.
		worklet.ChangeProjectCreated: true,
		worklet.ChangeProjectUpdated: true,
		worklet.ChangePolicyChanged:  true,
		worklet.ChangeViewSaved:      true,
		worklet.ChangeViewDeleted:    true,
		worklet.ChangeCatalogue:      true,

		// A person's own bookkeeping (a pin, an inbox mark, a snooze):
		// a kind for its history row, and it wakes nobody. The person
		// write that does is `prioritised`, above.
		worklet.ChangePersonUpdated: true,

		// A project's files, read from the project rather than woken
		// into anybody's inbox.
		worklet.ChangeFileWritten: true,
		worklet.ChangeFileRemoved: true,

		// A flow notice tells the one person its duty chose
		// ([worklet.Snapshot.Flow]), never the item's assignee, collaborators
		// or watchers (ADR-0162).
		worklet.ChangeFlow: true,
	}
	for _, kind := range worklet.ChangeKinds {
		if got := kind.TaskCommit(); got == notTasks[kind] {
			t.Errorf("%q: TaskCommit() = %v and this table says the opposite "+
				"— a kind in neither half routes to nobody, which is exactly "+
				"how a task's priority change came to wake no one", kind, got)
		}
	}
}

// AND EVERY ROUTABLE SUBJECT HAS A PROMPT FRAME: nothing links
// [worklet.ObjectKind.Routable] to the prompt's switch, and a kind missing
// from the switch reaches a seat as an empty turn.
func TestEveryRoutableObjectRendersAPrompt(t *testing.T) {
	t.Parallel()
	for _, kind := range worklet.ObjectKinds {
		if !kind.Routable() {
			continue
		}
		body := worklet.Prompt{}.Build(objectWake(kind, ""), nil)
		if strings.TrimSpace(body) == "" {
			t.Errorf("%q is routable and renders an empty prompt — a seat "+
				"woken by one gets a turn with nothing in it", kind)
		}
	}
}

// A NON-TASK WAKE NEVER CALLS ITSELF A TASK: rendered through the task frame
// it would be a non-empty, wrong prompt ("**Task:** a task: prioritised")
// with no error anywhere on the path.
func TestANonTaskWakeIsNotRenderedAsATask(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		object worklet.ObjectKind
		kind   worklet.ChangeKind
		want   string
	}{
		"a priority list": {worklet.KindPerson, worklet.ChangePrioritised, "**Your priorities:**"},
	} {
		t.Run(name, func(t *testing.T) {
			body := worklet.Prompt{}.Build(objectWake(tc.object, tc.kind), nil)
			if !strings.Contains(body, tc.want) {
				t.Errorf("the body does not carry %q:\n%s", tc.want, body)
			}
			if strings.Contains(body, "**Task:**") {
				t.Errorf("%s rendered as a TASK:\n%s", name, body)
			}
		})
	}
}

// THE PRIORITISED WAKE NAMES THE TASK AND ASKS FOR AN ANSWER. It is the one
// Addressed non-task wake, and Addressed is enforced (the turn is sent back
// until a tool call delivers), so a seat must be told which task.
func TestThePrioritisedWakeIsActionable(t *testing.T) {
	t.Parallel()
	wake := objectWake(worklet.KindPerson, worklet.ChangePrioritised)
	body := worklet.Prompt{}.Build(wake, nil)

	if !strings.Contains(body, "ENG-1") {
		t.Errorf("the wake does not name the task it is about:\n%s", body)
	}
	if !strings.Contains(body, worklet.GetWorkItemTool) {
		t.Errorf("the wake does not say how to read that task:\n%s", body)
	}
	if !(worklet.Prompt{}).Addressed(wake) {
		t.Error("a priorities write is not Addressed, so a seat may absorb it " +
			"silently — and the person who wrote the list cannot tell whether " +
			"it was seen")
	}
	// AND IT IS NOT A POINTER: it carries the task, who put it there and
	// the position, so the recon would only re-fetch what it was told.
	if (worklet.Prompt{}).RequiresRecon(wake) {
		t.Error("a prioritised wake asks for reconnaissance it does not need")
	}
}

// A WAKE ABOUT A NON-TASK OBJECT KEYS ITS CONVERSATION ON THAT OBJECT: an
// empty key would merge every priorities write in the company into one thread.
func TestNonTaskWakesDoNotShareOneConversation(t *testing.T) {
	t.Parallel()
	seen := map[string]string{}
	for _, kind := range worklet.ObjectKinds {
		if !kind.Routable() || kind == worklet.KindTask {
			continue
		}
		wake := objectWake(kind, "")
		// The person wake keys on its task where it carries one, and
		// otherwise on the person's own list.
		delete(wake.Metadata, worklet.MetaTaskKey)
		delete(wake.Metadata, worklet.MetaTaskID)
		key := worklet.Prompt{}.PartitionKey(wake.Metadata, "")
		if key == "" {
			t.Errorf("%q has no partition key", kind)
			continue
		}
		// THE OBJECT IS BOTH KEYS, merge unit and durable thread: a
		// divergence would let a coalesced trigger's conversation depend
		// on which constituent sorted first.
		if got := (worklet.Prompt{}).ConversationIdentity(wake.Metadata, ""); got != key {
			t.Errorf("%q: identity %q diverged from the partition key %q", kind, got, key)
		}
		if other, clash := seen[key]; clash {
			t.Errorf("%q and %q share the partition key %q", kind, other, key)
		}
		seen[key] = string(kind)
	}
}

// objectWake is one non-task notification as the parser stamps it, built
// from the parser's own constants: a fixture with a hard-coded `item_key`
// cannot represent a non-task wake rendered as a task, so cannot catch it.
func objectWake(object worklet.ObjectKind, kind worklet.ChangeKind) notify.Inbound {
	if kind == "" {
		if object == worklet.KindPerson {
			kind = worklet.ChangePrioritised
		} else {
			kind = worklet.ChangeStatus
		}
	}
	meta := map[string]string{
		worklet.MetaObject:     string(object),
		worklet.MetaObjectID:   "obj-1",
		worklet.MetaChangeKind: string(kind),
		worklet.MetaProject:    "ENG",
		worklet.MetaVia:        string(reasonFor(kind)),
	}
	if object == worklet.KindPerson {
		meta[worklet.MetaTaskKey] = "ENG-1"
		meta[worklet.MetaTaskID] = "task-1"
		meta[worklet.MetaTitle] = "the work"
	}
	return notify.Inbound{
		Source: worklet.Source, EventType: string(kind), Sender: "alice",
		Subject: "something: " + string(kind), Metadata: meta,
	}
}

func reasonFor(kind worklet.ChangeKind) worklet.Reason {
	if kind == worklet.ChangePrioritised {
		return worklet.ReasonPrioritised
	}
	return worklet.ReasonWatcher
}

// A ROUTABLE KIND AND A PROMPT FRAME ARE THE SAME SET, from the other side: a
// kind the prompt renders and the parser drops is a wake nobody receives. A
// flow notice is about its item and renders the task's frame.
func TestTheRoutableSetIsExactlyTheThreeKinds(t *testing.T) {
	t.Parallel()
	var routable []worklet.ObjectKind
	for _, kind := range worklet.ObjectKinds {
		if kind.Routable() {
			routable = append(routable, kind)
		}
	}
	want := []worklet.ObjectKind{
		worklet.KindTask, worklet.KindProject, worklet.KindPerson, worklet.KindFlow,
	}
	want = slices.DeleteFunc(want, func(k worklet.ObjectKind) bool {
		// A project's settings are read from describe_project rather
		// than woken — stated here so the exclusion is deliberate.
		return k == worklet.KindProject
	})
	slices.Sort(routable)
	slices.Sort(want)
	if !slices.Equal(routable, want) {
		t.Fatalf("routable = %v, want %v — the parser gates on this set and "+
			"the prompt switches on it, so a kind in one and not the other is "+
			"either a dropped wake or an empty one", routable, want)
	}
}

// ---- what the writers actually publish ---------------------------------- //

// WRITING SOMEBODY ELSE'S PRIORITIES WAKES THEM, and it is ADDRESSED: being
// told what to do next by somebody above you is an instruction.
func TestWritingSomebodyElsesPrioritiesWakesThem(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	task := r.createTask("The urgent thing")

	lead := r.writer.As("lead", worklet.AuthorHuman, worklet.Provenance{})
	if _, err := lead.WritePriorities(t.Context(), "op-prio", "alice",
		[]string{task.ID}, nil, worklet.PersonAuthority{Authorized: true}); err != nil {

		t.Fatalf("write alice's priorities: %v", err)
	}
	wake := r.lastWake()
	if wake == nil {
		t.Fatal("a lead wrote somebody's queue and nobody was told — the " +
			"stamp is seen by whoever opens a screen, and a seat has none")
	}
	if wake.Kind != worklet.ChangePrioritised {
		t.Fatalf("kind = %q, want %q", wake.Kind, worklet.ChangePrioritised)
	}
	if wake.Snapshot.Person != "alice" {
		t.Errorf("the wake is routed to %q, want alice", wake.Snapshot.Person)
	}
	// IT NAMES THE TASK: the list does not record what was new, so "your
	// list changed" is not actionable.
	if wake.Snapshot.Key != task.Key {
		t.Errorf("the wake names %q, want the task at the top (%q)",
			wake.Snapshot.Key, task.Key)
	}
	candidates := worklet.Candidates(wake)
	if len(candidates) != 1 || candidates[0].Handle != "alice" {
		t.Fatalf("candidates = %+v, want alice alone", candidates)
	}
	if !candidates[0].Addressed {
		t.Error("a priorities write is not Addressed, so a seat may absorb it " +
			"silently and the lead cannot tell whether it was seen")
	}
}

// A PERSON REORDERING A REPORT'S QUEUE IS NAMED AS THEIR SEAT — on the stamp
// the report reads, in the wake the report's seat is given and on the history
// row — and the credential they held rides beside it. A person acts AS their
// seat ([iam.ActorFor]), so the one name every screen resolves is who decided,
// while the audit trail keeps the credential; the credential as actor would
// put a token's label on the stamp.
func TestAReorderByAPersonNamesTheirSeatBesideTheirCredential(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	task := r.createTask("The thing to do first")

	person := r.writer.As("jane-founder", worklet.AuthorHuman, worklet.Provenance{
		OperatorID: "pat:cred-1",
	})
	if _, err := person.WritePriorities(t.Context(), "op-person", "alice",
		[]string{task.ID}, nil, worklet.PersonAuthority{Authorized: true}); err != nil {
		t.Fatalf("a person reordering alice's queue: %v", err)
	}
	wake := r.lastWake()
	if wake == nil {
		t.Fatal("somebody else set alice's queue and she was not told")
	}
	if wake.Snapshot.PrioritisedBy != "jane-founder" {
		t.Errorf("the wake says the order came from %q, want the seat "+
			"jane-founder", wake.Snapshot.PrioritisedBy)
	}
	if !strings.HasPrefix(wake.Excerpt, "jane-founder put ") {
		t.Errorf("the excerpt alice's seat reads is %q, want it to name "+
			"jane-founder", wake.Excerpt)
	}
	r.drain()
	if got := r.person("alice").PrioritiesSetBy; got != "jane-founder" {
		t.Errorf("the stamp on alice's queue says %q set it, want jane-founder", got)
	}
	// AND THE AUDIT TRAIL NAMES THE CREDENTIAL beside the seat.
	if got := r.strings(
		`SELECT actor || '/' || actor_kind || '/' || operator_id
		 FROM tracker_history ORDER BY rowid DESC LIMIT 1`,
	); len(got) != 1 || got[0] != "jane-founder/human/pat:cred-1" {
		t.Fatalf("the history row is authored %v, want the seat, the kind "+
			"human and the credential", got)
	}
}

// AND WRITING YOUR OWN WAKES NOBODY: the one tracker write that vetoes its
// own delivery per call.
func TestWritingYourOwnPrioritiesWakesNobody(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	task := r.createTask("My own thing")

	mine := r.writer.As("alice", worklet.AuthorAgent, worklet.Provenance{WrittenFor: types.ForTurn(nil)})
	if _, err := mine.WritePriorities(t.Context(), "op-own", "alice",
		[]string{task.ID}, nil, worklet.PersonAuthority{}); err != nil {

		t.Fatalf("write my own priorities: %v", err)
	}
	if wake := r.lastWake(); wake != nil {
		t.Fatalf("re-ordering my own queue woke %v", candidateHandles(wake))
	}
}

// ---- helpers ------------------------------------------------------------ //

// A CHECKLIST WAKE IS ABOUT THE ITEM, NOT WHERE IT SITS OR HOW IT WAS READ:
// an item's nesting is a pointer (two readings, two addresses) and its
// position moves when a neighbour is dragged, so comparing whole items wakes
// people for nothing. Only the text, the done flag or the holder is news.
func TestAChecklistWakeIsAboutTheItemNotItsLayout(t *testing.T) {
	t.Parallel()
	items := func(orderA, orderB int, doneC bool) []worklet.ChecklistItem {
		// A FRESH POINTER PER READING, as two decodes of one document
		// give: equal values at different addresses.
		under := "i0"
		return []worklet.ChecklistItem{
			{ID: "ia", Name: "draft", Assignee: "di", Parent: &under, Order: orderA},
			{ID: "ib", Name: "review", Assignee: "ed", Order: orderB},
			{ID: "ic", Name: "ship", Assignee: "fa", Done: doneC},
		}
	}
	task := worklet.Task{ID: "t", Key: "ENG-1", Project: "ENG",
		Status: worklet.StatusTodo, StatusGroup: worklet.GroupNotStarted}
	before, after := task, task
	before.Checklists = []worklet.Checklist{{ID: "l", Items: items(1, 2, false)}}
	// ia and ib swap places, and only ic is actually done.
	after.Checklists = []worklet.Checklist{{ID: "l", Items: items(2, 1, true)}}

	notify := worklet.Wake{Kind: worklet.ChangeChecklist, Before: before,
		After: after}.Notify(fixedLeads{})
	if notify == nil {
		t.Fatal("the wake is quiet, so this case tests nothing")
	}
	if got := notify.Snapshot.ChecklistAssignees; !slices.Equal(got, []string{"fa"}) {
		t.Errorf("the checklist wake reached %v, want only fa, whose item was "+
			"done — a nested item re-read and two lines swapping places are "+
			"news to nobody", got)
	}
}

type fixedLeads struct{ project, unit string }

func (f fixedLeads) ProjectLead(string) string { return f.project }
func (f fixedLeads) UnitLead(string) string    { return f.unit }

// lastWake reads the notification off the newest record on the log, not a
// return value: the wake is what travels to the routing node, and an
// in-memory struct would pass for one that never reached the wire.
func (r *roundTrip) lastWake() *worklet.Notify {
	r.t.Helper()
	last, err := r.log.End(r.t.Context())
	if err != nil {
		r.t.Fatalf("read the log's end: %v", err)
	}
	_, payload, _, ok, err := r.log.At(r.t.Context(), last)
	if err != nil || !ok {
		r.t.Fatalf("read record %d: %v", last, err)
	}
	record, err := worklet.Decode(r.open(last, payload))
	if err != nil {
		r.t.Fatalf("decode record %d: %v", last, err)
	}
	return record.Notify
}

func candidateHandles(wake *worklet.Notify) []string {
	var out []string
	for _, c := range worklet.Candidates(wake) {
		out = append(out, c.Handle)
	}
	return out
}

// AN OPERATOR MAY WRITE ANYBODY'S PRIORITIES, and a SEAT may not. This decides
// whether the wake above can fire: `set_priorities` is on the operator MCP
// alone, and an operator's actor is a token's name, never a chart handle, so
// `Lead` is false for every operator and a gate of `own || Lead` would refuse
// every cross-person write.
func TestWhoMayWriteSomebodyElsesPriorities(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	task := r.createTask("The thing")

	for name, tc := range map[string]struct {
		actor     string
		kind      worklet.AuthorKind
		authority worklet.PersonAuthority
		allowed   bool
	}{
		"an operator": {"ops", worklet.AuthorOperator,
			worklet.PersonAuthority{Authorized: true}, true},
		"a human": {"founder", worklet.AuthorHuman,
			worklet.PersonAuthority{Authorized: true}, true},
		"a lead": {"lead", worklet.AuthorAgent,
			worklet.PersonAuthority{Authorized: true}, true},
		// A SEAT MAY NOT: re-ordering a colleague's list is a hand-off
		// bypassing the guarded take and the reassignment budget.
		"a plain seat": {"peer", worklet.AuthorAgent,
			worklet.PersonAuthority{}, false},
	} {
		t.Run(name, func(t *testing.T) {
			// FOR NOBODY ELSE, so a seat is refused by its authority and
			// never because its actor stated nothing.
			writer := r.writer.As(tc.actor, tc.kind, worklet.Provenance{WrittenFor: types.ForTurn(nil)})
			_, err := writer.WritePriorities(t.Context(), "op-"+tc.actor,
				"alice", []string{task.ID}, nil, tc.authority)
			switch {
			case tc.allowed && err != nil:
				t.Fatalf("%s could not write alice's priorities: %v", name, err)
			case !tc.allowed && err == nil:
				t.Fatalf("%s wrote alice's priorities", name)
			}
			if tc.allowed {
				r.drain()
			}
		})
	}
}

// AN EXCERPT SAYS WHEN IT WAS CUT, and the marker fits inside the cap. An
// unmarked cut reads as a comment that ended there; the marker counts against
// MaxExcerpt because [worklet.Notify.Validate] refuses an excerpt above it,
// so an ellipsis outside the budget fails every long comment's write.
func TestALongExcerptIsMarkedAndStillFits(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", worklet.MaxExcerpt*2)
	wake := worklet.Wake{
		Kind:    worklet.ChangeComment,
		Comment: &worklet.Comment{ID: "c-1", Body: long, Author: "alice"},
		After:   worklet.Task{ID: "t-1", Key: "ENG-1", Project: "ENG"},
	}.Notify(nil)
	if wake == nil {
		t.Fatal("a comment produced no notification")
	}
	if len(wake.Excerpt) > worklet.MaxExcerpt {
		t.Fatalf("the excerpt is %d bytes against a %d cap — Validate refuses "+
			"that, so the write FAILS rather than the excerpt being marked",
			len(wake.Excerpt), worklet.MaxExcerpt)
	}
	if err := wake.Validate(); err != nil {
		t.Fatalf("the excerpt does not survive its own validation: %v", err)
	}
	if !strings.HasSuffix(wake.Excerpt, "…") {
		t.Errorf("a cut excerpt does not say it was cut, so it reads as a "+
			"comment that ended there: %q", clipForTest(wake.Excerpt))
	}

	// AND A SHORT ONE IS UNTOUCHED.
	short := "one line"
	got := worklet.Wake{
		Kind:    worklet.ChangeComment,
		Comment: &worklet.Comment{ID: "c-2", Body: short, Author: "alice"},
		After:   worklet.Task{ID: "t-1", Key: "ENG-1", Project: "ENG"},
	}.Notify(nil)
	if got == nil || got.Excerpt != short {
		t.Errorf("a short comment's excerpt is %q, want it verbatim", got.Excerpt)
	}
}

func clipForTest(s string) string {
	if len(s) <= 80 {
		return s
	}
	return s[:80] + "..."
}

// A SCHEDULE CHANGE CARRIES WHAT IT CHANGED in [worklet.TaskDeltas]: due,
// start, estimate and size. Without deltas a card announces a bare kind, and
// the applier, which rebuilds a task's spans from the history row's deltas,
// would hide the commit from every report derived from them.
func TestAScheduleChangeCarriesTheValuesThatMoved(t *testing.T) {
	t.Parallel()
	day := func(iso string) *time.Time {
		at, err := time.Parse(time.RFC3339, iso)
		if err != nil {
			t.Fatalf("parse %s: %v", iso, err)
		}
		return &at
	}
	before := worklet.Task{
		ID: "t-1", Title: "the same title",
		DueAt: day("2031-04-16T00:00:00Z"), EstimateMinutes: 90, Points: 3,
	}
	after := worklet.Task{
		ID: "t-1", Title: "the same title",
		DueAt: day("2031-04-23T00:00:00Z"), EstimateMinutes: 120, Points: 5.5,
		StartAt: day("2031-04-17T00:00:00Z"),
	}

	moved := worklet.TaskDeltas(before, after, nil)
	for field, want := range map[string]worklet.Delta{
		// THE WHOLE INSTANT: a day hides a same-day move and names
		// the wrong day off UTC.
		"due":      {From: "2031-04-16T00:00:00Z", To: "2031-04-23T00:00:00Z"},
		"start":    {From: "", To: "2031-04-17T00:00:00Z"},
		"estimate": {From: "90m", To: "120m"},
		// A HALF POINT STILL PRINTS.
		"points": {From: "3", To: "5.5"},
	} {
		if got := moved[field]; got.From != want.From || got.To != want.To || got.Added != nil || got.Removed != nil {
			t.Errorf("%s = %+v, want %+v", field, got, want)
		}
	}
	if _, held := moved["title"]; held {
		t.Error("the title did not move and is in the deltas")
	}
}

// CLEARING A SIZE IS A CHANGE, although a zero renders as the absence it is.
// [worklet.Task.Points] and [worklet.Task.EstimateMinutes] are NOT NULL, so
// nothing tells "never sized" from "sized at nothing", and a card printing "0"
// would claim a fact the engine does not hold. The clearing shows because
// "8" → "" differs; only 0 → 0 folds away.
func TestClearingASizeIsAChange(t *testing.T) {
	t.Parallel()
	sized := worklet.Task{ID: "t-1", Points: 8, EstimateMinutes: 45}
	cleared := worklet.Task{ID: "t-1"}
	moved := worklet.TaskDeltas(sized, cleared, nil)
	if got := moved["points"]; got.From != "8" || got.To != "" {
		t.Errorf("points = %+v, want 8 → nothing", got)
	}
	if got := moved["estimate"]; got.From != "45m" || got.To != "" {
		t.Errorf("estimate = %+v, want 45m → nothing", got)
	}
	// A task that never had either reports no change at all.
	if moved := worklet.TaskDeltas(cleared, cleared, nil); len(moved) != 0 {
		t.Errorf("an unchanged task moved %+v", moved)
	}
}

// A MOVE INSIDE ONE DAY IS STILL A MOVE: a field is recorded only when its
// text sides differ, so a schedule rendered as a calendar day would drop a
// same-day move from the history row and the card.
func TestASameDayScheduleMoveIsRecorded(t *testing.T) {
	t.Parallel()
	at := func(iso string) *time.Time {
		t.Helper()
		parsed, err := time.Parse(time.RFC3339, iso)
		if err != nil {
			t.Fatalf("parse %s: %v", iso, err)
		}
		return &parsed
	}
	morning := at("2031-04-16T09:00:00Z")
	evening := at("2031-04-16T17:00:00Z")
	before := worklet.Task{ID: "t-1", DueAt: morning, StartAt: morning}
	after := worklet.Task{ID: "t-1", DueAt: evening, StartAt: evening}

	moved := worklet.TaskDeltas(before, after, nil)
	for _, field := range []string{"due", "start"} {
		got, held := moved[field]
		if !held {
			t.Fatalf("a same-day %s move produced no delta; the change is "+
				"absent from history and from every card", field)
		}
		want := worklet.Delta{
			From: "2031-04-16T09:00:00Z",
			To:   "2031-04-16T17:00:00Z",
		}
		if got.From != want.From || got.To != want.To {
			t.Errorf("%s = %+v, want %+v", field, got, want)
		}
	}
}

// AND AN ALL-DAY DATE KEEPS THE DAY ITS COMPANY MEANT. It is stored as the
// company's midnight (the 16th in Berlin is the 15th at 22:00Z), so a UTC day
// would name the wrong one. The delta keeps the instant: the applier has no
// company zone, and text derived from live config would differ between nodes
// at different epochs on the log's identical copies.
func TestAnAllDayDueDateIsNotTruncatedToTheWrongDay(t *testing.T) {
	t.Parallel()
	at := func(iso string) *time.Time {
		t.Helper()
		parsed, err := time.Parse(time.RFC3339, iso)
		if err != nil {
			t.Fatalf("parse %s: %v", iso, err)
		}
		return &parsed
	}
	// 2031-04-16 00:00 in Berlin, which is how an all-day date is stored.
	berlinMidnight := at("2031-04-15T22:00:00Z")
	before := worklet.Task{ID: "t-1"}
	after := worklet.Task{ID: "t-1", DueAt: berlinMidnight, DueAllDay: true}

	got := worklet.TaskDeltas(before, after, nil)["due"]
	if got.To != "2031-04-15T22:00:00Z" {
		t.Errorf("due = %q, want the stored instant: a day rendered here is "+
			"rendered without the zone that decided it", got.To)
	}
}
