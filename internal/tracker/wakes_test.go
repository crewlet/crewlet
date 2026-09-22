package tracker_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/tracker"
)

// EVERY CHANGE KIND IS CLASSIFIED, and this is the guard the tree did not have.
//
// `prioritised` was in the enum, was Valid(), passed Validate(), rode a real
// record — and was absent from taskCommit(), so a task's priority change woke
// no assignee, no collaborator and no watcher. Nothing in the build caught it,
// because the allowlist inside taskCommit has no compiler link to ChangeKinds:
// a kind added to one and forgotten in the other is a kind that routes to
// nobody, silently, for ever.
//
// So the table below is that link, written out. A new kind fails this test
// until somebody DECIDES which half it is in — which is the whole point: the
// decision is cheap and the omission is invisible.
func TestEveryChangeKindIsClassifiedAsTaskOrNot(t *testing.T) {
	t.Parallel()
	// The kinds that are NOT about a task's own routing. Everything else
	// must be a task commit.
	notTasks := map[tracker.ChangeKind]bool{
		// The object wake, routed off its own snapshot field rather
		// than off a task's assignee or watchers.
		tracker.ChangePrioritised: true,

		// Bookkeeping and surfaces of their own: a project's settings,
		// a saved view and the catalogue are read where they live.
		tracker.ChangeProjectCreated: true,
		tracker.ChangeProjectUpdated: true,
		tracker.ChangePolicyChanged:  true,
		tracker.ChangeViewSaved:      true,
		tracker.ChangeCatalogue:      true,

		// A person's own bookkeeping — a pin, an inbox mark, a snooze.
		// It writes a history row like every other document's, which is
		// why it needs a kind at all, and it wakes nobody: the one
		// person write that announces itself is `prioritised`, above,
		// because somebody else reordered your day.
		tracker.ChangePersonUpdated: true,
	}
	for _, kind := range tracker.ChangeKinds {
		if got := kind.TaskCommit(); got == notTasks[kind] {
			t.Errorf("%q: TaskCommit() = %v and this table says the opposite "+
				"— a kind in neither half routes to nobody, which is exactly "+
				"how a task's priority change came to wake no one", kind, got)
		}
	}
}

// AND EVERY ROUTABLE SUBJECT HAS A PROMPT FRAME.
//
// The parser's gate and the prompt's dispatch are two halves of one set, with
// nothing linking them: a kind admitted by [tracker.ObjectKind.Routable] and
// missing from the prompt's switch renders an EMPTY body, which reaches a seat
// as a turn with nothing in it.
func TestEveryRoutableObjectRendersAPrompt(t *testing.T) {
	t.Parallel()
	for _, kind := range tracker.ObjectKinds {
		if !kind.Routable() {
			continue
		}
		body := tracker.Prompt{}.Build(objectWake(kind, ""), nil)
		if strings.TrimSpace(body) == "" {
			t.Errorf("%q is routable and renders an empty prompt — a seat "+
				"woken by one gets a turn with nothing in it", kind)
		}
	}
}

// A NON-TASK WAKE NEVER CALLS ITSELF A TASK, which is the failure lifting the
// parser's gate alone would have produced: a fully rendered, non-empty,
// actively wrong prompt — "**Task:** a task: prioritised" with four steps
// about moving the task to an active status — delivered to a real seat with no
// error anywhere on the path.
func TestANonTaskWakeIsNotRenderedAsATask(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		object tracker.ObjectKind
		kind   tracker.ChangeKind
		want   string
	}{
		"a priority list": {tracker.KindPerson, tracker.ChangePrioritised, "**Your priorities:**"},
	} {
		t.Run(name, func(t *testing.T) {
			body := tracker.Prompt{}.Build(objectWake(tc.object, tc.kind), nil)
			if !strings.Contains(body, tc.want) {
				t.Errorf("the body does not carry %q:\n%s", tc.want, body)
			}
			if strings.Contains(body, "**Task:**") {
				t.Errorf("%s rendered as a TASK:\n%s", name, body)
			}
		})
	}
}

// THE PRIORITISED WAKE NAMES THE TASK AND ASKS FOR AN ANSWER.
//
// It is the one Addressed non-task wake — the spec's "a seat starts it, or
// says why not" — and Addressed is ENFORCED: the turn is sent back for more
// rounds until a tool call delivered. A seat obliged to deliver on a task it
// was never told the identity of is the worst shape this change could take.
func TestThePrioritisedWakeIsActionable(t *testing.T) {
	t.Parallel()
	wake := objectWake(tracker.KindPerson, tracker.ChangePrioritised)
	body := tracker.Prompt{}.Build(wake, nil)

	if !strings.Contains(body, "ENG-1") {
		t.Errorf("the wake does not name the task it is about:\n%s", body)
	}
	if !strings.Contains(body, tracker.GetWorkItemTool) {
		t.Errorf("the wake does not say how to read that task:\n%s", body)
	}
	if !(tracker.Prompt{}).Addressed(wake) {
		t.Error("a priorities write is not Addressed, so a seat may absorb it " +
			"silently — and the person who wrote the list cannot tell whether " +
			"it was seen")
	}
	// AND IT IS NOT A POINTER. It carries the task, the person who put it
	// there and the position, so turning the recon on would cost the seat
	// its own memory and episode recall to fetch what it was already told.
	if (tracker.Prompt{}).RequiresRecon(wake) {
		t.Error("a prioritised wake asks for reconnaissance it does not need")
	}
}

// A WAKE ABOUT A NON-TASK OBJECT KEYS ITS CONVERSATION ON THAT OBJECT.
//
// Falling through to an empty key would put every priorities write in the
// company into ONE ledger — a conversation key is what separates threads, and
// a shared empty one merges them all.
func TestNonTaskWakesDoNotShareOneConversation(t *testing.T) {
	t.Parallel()
	seen := map[string]string{}
	for _, kind := range tracker.ObjectKinds {
		if !kind.Routable() || kind == tracker.KindTask {
			continue
		}
		wake := objectWake(kind, "")
		// The person wake carries a task key and keys on it, which is
		// right: a priorities write IS about that task.
		delete(wake.Metadata, tracker.MetaTaskKey)
		key := tracker.Prompt{}.PartitionKey(wake.Metadata, "")
		if key == "" {
			t.Errorf("%q has no partition key", kind)
			continue
		}
		// THE OBJECT IS BOTH KEYS: a task (or the object a non-task wake
		// names) is one thing that is both the merge unit and the durable
		// thread, so the identity delegates. A divergence would break the
		// alignment the key is chosen for — a chat thread about ENG-42 and
		// the tracker activity on it landing in one ledger.
		if got := (tracker.Prompt{}).ConversationIdentity(wake.Metadata, ""); got != key {
			t.Errorf("%q: identity %q diverged from the partition key %q", kind, got, key)
		}
		if other, clash := seen[key]; clash {
			t.Errorf("%q and %q share the partition key %q", kind, other, key)
		}
		seen[key] = string(kind)
	}
}

// objectWake is one non-task notification as the parser stamps it.
//
// BUILT FROM THE PARSER'S OWN CONSTANTS rather than from literals, because the
// existing prompt suite hard-codes `item_key: ENG-1` into every case it builds
// — which is exactly why it stayed green while a non-task wake rendered as a
// task. A fixture that cannot represent the failure cannot catch it.
func objectWake(object tracker.ObjectKind, kind tracker.ChangeKind) notify.Inbound {
	if kind == "" {
		if object == tracker.KindPerson {
			kind = tracker.ChangePrioritised
		} else {
			kind = tracker.ChangeStatus
		}
	}
	meta := map[string]string{
		tracker.MetaObject:     string(object),
		tracker.MetaObjectID:   "obj-1",
		tracker.MetaChangeKind: string(kind),
		tracker.MetaProject:    "ENG",
		tracker.MetaVia:        string(reasonFor(kind)),
	}
	if object == tracker.KindPerson {
		meta[tracker.MetaTaskKey] = "ENG-1"
		meta[tracker.MetaTaskID] = "task-1"
		meta[tracker.MetaTitle] = "the work"
	}
	return notify.Inbound{
		Source: tracker.Source, EventType: string(kind), Sender: "alice",
		Subject: "something: " + string(kind), Metadata: meta,
	}
}

func reasonFor(kind tracker.ChangeKind) tracker.Reason {
	if kind == tracker.ChangePrioritised {
		return tracker.ReasonPrioritised
	}
	return tracker.ReasonWatcher
}

// A ROUTABLE KIND AND A PROMPT FRAME ARE THE SAME SET, stated from the other
// side: a kind the prompt can render and the parser drops is a wake nobody
// receives, which is the state all of these were in.
func TestTheRoutableSetIsExactlyTheTwoKinds(t *testing.T) {
	t.Parallel()
	var routable []tracker.ObjectKind
	for _, kind := range tracker.ObjectKinds {
		if kind.Routable() {
			routable = append(routable, kind)
		}
	}
	want := []tracker.ObjectKind{
		tracker.KindTask, tracker.KindProject, tracker.KindPerson,
	}
	want = slices.DeleteFunc(want, func(k tracker.ObjectKind) bool {
		// A project's settings are read from describe_project rather
		// than woken — stated here so the exclusion is deliberate.
		return k == tracker.KindProject
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
	r := newRoundTrip(t)
	task := r.createTask("The urgent thing")

	lead := r.writer.As("lead", tracker.AuthorHuman, tracker.Provenance{})
	if _, err := lead.WritePriorities(t.Context(), "op-prio", "alice",
		[]string{task.ID}, tracker.PersonAuthority{Lead: true}); err != nil {

		t.Fatalf("write alice's priorities: %v", err)
	}
	wake := r.lastWake()
	if wake == nil {
		t.Fatal("a lead wrote somebody's queue and nobody was told — the " +
			"stamp is seen by whoever opens a screen, and a seat has none")
	}
	if wake.Kind != tracker.ChangePrioritised {
		t.Fatalf("kind = %q, want %q", wake.Kind, tracker.ChangePrioritised)
	}
	if wake.Snapshot.Person != "alice" {
		t.Errorf("the wake is routed to %q, want alice", wake.Snapshot.Person)
	}
	// IT NAMES THE TASK, which is what makes it actionable: "your list
	// changed" sends a seat to read the whole list and work out what is
	// new, and the list does not record what was new.
	if wake.Snapshot.Key != task.Key {
		t.Errorf("the wake names %q, want the task at the top (%q)",
			wake.Snapshot.Key, task.Key)
	}
	candidates := tracker.Candidates(wake, false)
	if len(candidates) != 1 || candidates[0].Handle != "alice" {
		t.Fatalf("candidates = %+v, want alice alone", candidates)
	}
	if !candidates[0].Addressed {
		t.Error("a priorities write is not Addressed, so a seat may absorb it " +
			"silently and the lead cannot tell whether it was seen")
	}
}

// AND WRITING YOUR OWN WAKES NOBODY. Re-ordering your own queue is not news to
// anybody, least of all to you — the one tracker write that vetoes its own
// delivery per call.
func TestWritingYourOwnPrioritiesWakesNobody(t *testing.T) {
	r := newRoundTrip(t)
	task := r.createTask("My own thing")

	mine := r.writer.As("alice", tracker.AuthorAgent, tracker.Provenance{})
	if _, err := mine.WritePriorities(t.Context(), "op-own", "alice",
		[]string{task.ID}, tracker.PersonAuthority{}); err != nil {

		t.Fatalf("write my own priorities: %v", err)
	}
	if wake := r.lastWake(); wake != nil {
		t.Fatalf("re-ordering my own queue woke %v", candidateHandles(wake))
	}
}

// ---- helpers ------------------------------------------------------------ //

type fixedLeads struct{ project, unit string }

func (f fixedLeads) ProjectLead(string) string { return f.project }
func (f fixedLeads) UnitLead(string) string    { return f.unit }

// lastWake reads the notification off the newest record on the log.
//
// OFF THE LOG rather than out of a return value, because the wake is what
// TRAVELS: the node that routes it is rarely the node that wrote it, and a
// test asserting a struct the writer held in memory would pass for a
// notification that never reached the wire.
func (r *roundTrip) lastWake() *tracker.Notify {
	r.t.Helper()
	last, err := r.log.End(r.t.Context())
	if err != nil {
		r.t.Fatalf("read the log's end: %v", err)
	}
	_, payload, _, ok, err := r.log.At(r.t.Context(), last)
	if err != nil || !ok {
		r.t.Fatalf("read record %d: %v", last, err)
	}
	record, err := tracker.Decode(payload)
	if err != nil {
		r.t.Fatalf("decode record %d: %v", last, err)
	}
	return record.Notify
}

func candidateHandles(wake *tracker.Notify) []string {
	var out []string
	for _, c := range tracker.Candidates(wake, false) {
		out = append(out, c.Handle)
	}
	return out
}

// AN OPERATOR MAY WRITE ANYBODY'S PRIORITIES, and a SEAT may not.
//
// This is the case that decides whether the wake above can fire at all.
// `set_priorities` is registered on the operator MCP alone, and an operator's
// actor is an API TOKEN's name — never a handle in the chart — so no ancestor
// walk can match it and `Lead` is false for every operator by construction.
// With the gate reading `own || Lead`, the only shipped surface for this verb
// refused every cross-person write, and omitting the handle silently wrote a
// person record for the token instead.
func TestWhoMayWriteSomebodyElsesPriorities(t *testing.T) {
	r := newRoundTrip(t)
	task := r.createTask("The thing")

	for name, tc := range map[string]struct {
		actor     string
		kind      tracker.AuthorKind
		authority tracker.PersonAuthority
		allowed   bool
	}{
		"an operator": {"ops", tracker.AuthorOperator,
			tracker.PersonAuthority{Person: true}, true},
		"a human": {"founder", tracker.AuthorHuman,
			tracker.PersonAuthority{Person: true}, true},
		"a lead": {"lead", tracker.AuthorAgent,
			tracker.PersonAuthority{Lead: true}, true},
		// A SEAT IS THE ONE PARTY THAT MAY NOT. An agent re-ordering a
		// colleague's list is a hand-off in disguise: an Addressed wake
		// that bypasses the guarded take and the reassignment budget.
		"a plain seat": {"peer", tracker.AuthorAgent,
			tracker.PersonAuthority{}, false},
	} {
		t.Run(name, func(t *testing.T) {
			writer := r.writer.As(tc.actor, tc.kind, tracker.Provenance{})
			_, err := writer.WritePriorities(t.Context(), "op-"+tc.actor,
				"alice", []string{task.ID}, tc.authority)
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

// AN EXCERPT SAYS WHEN IT WAS CUT, and the marker fits inside the cap.
//
// A card is the whole of what most recipients read, so a comment cut at
// exactly MaxExcerpt and handed over unmarked reads as a comment that ENDED
// there — a different message from the one somebody wrote. The marker has to
// be counted against the budget rather than added outside it, because
// [tracker.Notify.Validate] REFUSES an excerpt above the cap: an ellipsis that
// pushed it three bytes over would turn every long comment into a failed write
// instead of a marked one.
func TestALongExcerptIsMarkedAndStillFits(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", tracker.MaxExcerpt*2)
	wake := tracker.Wake{
		Kind:    tracker.ChangeComment,
		Comment: &tracker.Comment{ID: "c-1", Body: long, Author: "alice"},
		After:   tracker.Task{ID: "t-1", Key: "ENG-1", Project: "ENG"},
	}.Notify(nil)
	if wake == nil {
		t.Fatal("a comment produced no notification")
	}
	if len(wake.Excerpt) > tracker.MaxExcerpt {
		t.Fatalf("the excerpt is %d bytes against a %d cap — Validate refuses "+
			"that, so the write FAILS rather than the excerpt being marked",
			len(wake.Excerpt), tracker.MaxExcerpt)
	}
	if err := wake.Validate(); err != nil {
		t.Fatalf("the excerpt does not survive its own validation: %v", err)
	}
	if !strings.HasSuffix(wake.Excerpt, "…") {
		t.Errorf("a cut excerpt does not say it was cut, so it reads as a "+
			"comment that ended there: %q", clipForTest(wake.Excerpt))
	}

	// AND A SHORT ONE IS UNTOUCHED, or every comment in the company gains
	// an ellipsis it did not earn.
	short := "one line"
	got := tracker.Wake{
		Kind:    tracker.ChangeComment,
		Comment: &tracker.Comment{ID: "c-2", Body: short, Author: "alice"},
		After:   tracker.Task{ID: "t-1", Key: "ENG-1", Project: "ENG"},
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

// A SCHEDULE CHANGE CARRIES WHAT IT CHANGED.
//
// A due date, a start, an estimate and a size had no producer in this tree
// until the write tools gained one, so their absence from
// [tracker.TaskDeltas] was invisible: nothing could move them, so nothing
// reported the move. With a producer, a re-estimate wrote a history row and a
// notification card carrying NO deltas at all — which every renderer falls
// back on the bare KIND for, so a card announced a change with nothing in
// it.
//
// It also matters beyond display: the applier rebuilds a task's spans from the
// history row's deltas, and a row with none is a commit the reports derived
// from those spans cannot see.
func TestAScheduleChangeCarriesTheValuesThatMoved(t *testing.T) {
	t.Parallel()
	day := func(iso string) *time.Time {
		at, err := time.Parse(time.RFC3339, iso)
		if err != nil {
			t.Fatalf("parse %s: %v", iso, err)
		}
		return &at
	}
	before := tracker.Task{
		ID: "t-1", Title: "the same title",
		DueAt: day("2031-04-16T00:00:00Z"), EstimateMinutes: 90, Points: 3,
	}
	after := tracker.Task{
		ID: "t-1", Title: "the same title",
		DueAt: day("2031-04-23T00:00:00Z"), EstimateMinutes: 120, Points: 5.5,
		StartAt: day("2031-04-17T00:00:00Z"),
	}

	moved := tracker.TaskDeltas(before, after)
	for field, want := range map[string]tracker.Delta{
		// THE WHOLE INSTANT: a day compares equal to itself
		// whenever a move stays inside one, and it names the wrong
		// day for any company that is not on UTC.
		"due":      {From: "2031-04-16T00:00:00Z", To: "2031-04-23T00:00:00Z"},
		"start":    {From: "", To: "2031-04-17T00:00:00Z"},
		"estimate": {From: "90m", To: "120m"},
		// A HALF POINT STILL PRINTS: a scale with halves in it is a
		// scale somebody chose.
		"points": {From: "3", To: "5.5"},
	} {
		if got := moved[field]; got != want {
			t.Errorf("%s = %+v, want %+v", field, got, want)
		}
	}
	if _, held := moved["title"]; held {
		t.Error("the title did not move and is in the deltas")
	}
}

// CLEARING A SIZE IS A CHANGE, although a zero renders as the absence it is.
//
// [tracker.Task.Points] and [tracker.Task.EstimateMinutes] are a plain float64
// and a plain int over NOT NULL columns, so nothing anywhere in this package
// can tell "never sized" from "sized at nothing" — a workload sums the zero
// and a total adds it — and a card
// printing "0" would be the one surface claiming a fact the rest of the engine
// does not hold. What makes the clearing visible is not a distinction between
// two zeroes: it is that the FROM side is a number and the TO side is not, so
// "8" → "" differs and is recorded like any other delta. Only 0 → 0 folds away,
// and that is not a change.
func TestClearingASizeIsAChange(t *testing.T) {
	t.Parallel()
	sized := tracker.Task{ID: "t-1", Points: 8, EstimateMinutes: 45}
	cleared := tracker.Task{ID: "t-1"}
	moved := tracker.TaskDeltas(sized, cleared)
	if got := moved["points"]; got != (tracker.Delta{From: "8", To: ""}) {
		t.Errorf("points = %+v, want 8 → nothing", got)
	}
	if got := moved["estimate"]; got != (tracker.Delta{From: "45m", To: ""}) {
		t.Errorf("estimate = %+v, want 45m → nothing", got)
	}
	// And a task that never had either reports no change at all, rather
	// than two deltas from nothing to nothing.
	if moved := tracker.TaskDeltas(cleared, cleared); len(moved) != 0 {
		t.Errorf("an unchanged task moved %+v", moved)
	}
}

// A MOVE INSIDE ONE DAY IS STILL A MOVE.
//
// The delta is text and a field is only recorded when the two sides DIFFER, so
// rendering a schedule instant as a calendar day made every same-day change
// compare equal to itself: pulling a due time from the morning to the end of
// the afternoon produced no `due` entry at all, and the history row and the
// notification card carried the change's kind with nothing it changed. That is
// the same failure the schedule fields were added to these deltas to end, one
// granularity down.
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
	before := tracker.Task{ID: "t-1", DueAt: morning, StartAt: morning}
	after := tracker.Task{ID: "t-1", DueAt: evening, StartAt: evening}

	moved := tracker.TaskDeltas(before, after)
	for _, field := range []string{"due", "start"} {
		got, held := moved[field]
		if !held {
			t.Fatalf("a same-day %s move produced no delta; the change is "+
				"absent from history and from every card", field)
		}
		want := tracker.Delta{
			From: "2031-04-16T09:00:00Z",
			To:   "2031-04-16T17:00:00Z",
		}
		if got != want {
			t.Errorf("%s = %+v, want %+v", field, got, want)
		}
	}
}

// AND AN ALL-DAY DATE KEEPS THE DAY ITS COMPANY MEANT.
//
// An all-day due date is stored as the COMPANY's own midnight, so a company
// east of UTC stores the 16th as the 15th at 22:00Z. Truncated to a UTC
// calendar day that delta read "2031-04-15" — a day nobody chose, for every
// company not on UTC. The instant cannot be wrong about which day it is,
// and the applier that writes this row has no company zone to consult: these
// rows are the state log's N identical copies, so text derived from live
// configuration would differ between two nodes at different epochs.
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
	before := tracker.Task{ID: "t-1"}
	after := tracker.Task{ID: "t-1", DueAt: berlinMidnight, DueAllDay: true}

	got := tracker.TaskDeltas(before, after)["due"]
	if got.To != "2031-04-15T22:00:00Z" {
		t.Errorf("due = %q, want the stored instant: a day rendered here is "+
			"rendered without the zone that decided it", got.To)
	}
}
