package tracker_test

import (
	"slices"
	"strings"
	"testing"

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
		// The three object wakes, each routed off its own snapshot
		// field rather than off a task's assignee or watchers.
		tracker.ChangeGoalUpdated:   true,
		tracker.ChangeSprintStarted: true,
		tracker.ChangeSprintClosed:  true,
		tracker.ChangePrioritised:   true,

		// Bookkeeping and surfaces of their own. A project's settings,
		// a saved view and the catalogue are read where they live; a
		// minted sprint is a policy tick eight weeks out.
		tracker.ChangeProjectCreated: true,
		tracker.ChangeProjectUpdated: true,
		tracker.ChangePolicyChanged:  true,
		tracker.ChangeSprintMinted:   true,
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
// actively wrong prompt — "**Task:** a task: goal_updated" with four steps
// about moving the task to an active status — delivered to a real seat with no
// error anywhere on the path.
func TestANonTaskWakeIsNotRenderedAsATask(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		object tracker.ObjectKind
		kind   tracker.ChangeKind
		want   string
	}{
		"a goal":            {tracker.KindGoal, tracker.ChangeGoalUpdated, "**Goal:**"},
		"a sprint starting": {tracker.KindSprint, tracker.ChangeSprintStarted, "**Sprint:**"},
		"a sprint closing":  {tracker.KindSprint, tracker.ChangeSprintClosed, "**Sprint:**"},
		"a priority list":   {tracker.KindPerson, tracker.ChangePrioritised, "**Your priorities:**"},
	} {
		t.Run(name, func(t *testing.T) {
			body := tracker.Prompt{}.Build(objectWake(tc.object, tc.kind), nil)
			if !strings.Contains(body, tc.want) {
				t.Errorf("the body does not carry %q:\n%s", tc.want, body)
			}
			if strings.Contains(body, "**Task:**") {
				t.Errorf("%s rendered as a TASK:\n%s", name, body)
			}
			// AND IT NEVER SENDS THE SEAT TO get_work_item FOR ITSELF.
			// A goal's uuid and a sprint's `KEY.n` are not task keys,
			// and a pointer at one costs the seat a round and a failed
			// tool call to discover.
			if tc.object != tracker.KindPerson &&
				strings.Contains(body, tracker.GetWorkItemTool) {

				t.Errorf("%s points at %s, which cannot resolve it:\n%s",
					name, tracker.GetWorkItemTool, body)
			}
		})
	}
}

// THE PRIORITISED WAKE NAMES THE TASK AND ASKS FOR AN ANSWER.
//
// It is the one Addressed wake of the four — the spec's "a seat starts it, or
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
// Falling through to an empty key would put every goal update in the company
// into ONE ledger with every sprint close and every priorities write — a
// conversation key is what separates threads, and a shared empty one merges
// them all.
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
		key := tracker.Prompt{}.ConversationKey(wake.Metadata, "")
		if key == "" {
			t.Errorf("%q has no conversation key", kind)
			continue
		}
		if other, clash := seen[key]; clash {
			t.Errorf("%q and %q share the conversation key %q", kind, other, key)
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
		switch object {
		case tracker.KindGoal:
			kind = tracker.ChangeGoalUpdated
		case tracker.KindSprint:
			kind = tracker.ChangeSprintClosed
		case tracker.KindPerson:
			kind = tracker.ChangePrioritised
		default:
			kind = tracker.ChangeStatus
		}
	}
	meta := map[string]string{
		tracker.MetaObject:      string(object),
		tracker.MetaObjectID:    "obj-1",
		tracker.MetaChangeKind:  string(kind),
		tracker.MetaProject:     "ENG",
		tracker.MetaVia:         string(reasonFor(kind)),
		tracker.MetaProjectLead: "lead",
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
	switch kind {
	case tracker.ChangeGoalUpdated:
		return tracker.ReasonGoalOwner
	case tracker.ChangeSprintStarted, tracker.ChangeSprintClosed:
		return tracker.ReasonSprint
	case tracker.ChangePrioritised:
		return tracker.ReasonPrioritised
	}
	return tracker.ReasonWatcher
}

// A ROUTABLE KIND AND A PROMPT FRAME ARE THE SAME SET, stated from the other
// side: a kind the prompt can render and the parser drops is a wake nobody
// receives, which is the state all four of these were in.
func TestTheRoutableSetIsExactlyTheFourKinds(t *testing.T) {
	t.Parallel()
	var routable []tracker.ObjectKind
	for _, kind := range tracker.ObjectKinds {
		if kind.Routable() {
			routable = append(routable, kind)
		}
	}
	want := []tracker.ObjectKind{
		tracker.KindTask, tracker.KindProject, tracker.KindSprint,
		tracker.KindGoal, tracker.KindPerson,
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

// A GOAL SAVE WAKES ITS OWNERS AND ITS MEMBERS — D11's rule, and the wake the
// writer published as a literal nil for as long as the routing arm existed.
func TestAGoalSaveWakesItsOwnersAndMembers(t *testing.T) {
	r := newRoundTrip(t)
	goal := tracker.Goal{
		ID: "g-1", Name: "Ship the thing", Health: "on_track",
		Owners: []string{"alice"}, Members: []string{"bob", "carol"},
	}
	if _, err := r.writer.WriteGoal(t.Context(), "op-goal", goal); err != nil {
		t.Fatalf("write the goal: %v", err)
	}
	wake := r.lastWake()
	if wake == nil {
		t.Fatal("a goal save published no notification, so nobody who owns " +
			"the outcome is ever told it moved")
	}
	if wake.Kind != tracker.ChangeGoalUpdated {
		t.Fatalf("kind = %q, want %q", wake.Kind, tracker.ChangeGoalUpdated)
	}
	handles := candidateHandles(wake)
	for _, who := range []string{"alice", "bob", "carol"} {
		if !slices.Contains(handles, who) {
			t.Errorf("%s is named on the goal and hears nothing: %v", who, handles)
		}
	}
	if wake.Snapshot.GoalName != goal.Name {
		t.Errorf("the wake does not carry the goal's name: %+v", wake.Snapshot)
	}
}

// AND A SAVE THAT CHANGES NOTHING WAKES NOBODY. This verb is a whole
// post-state replace, so a form that submits every control saves on every
// submit — and eight owners paged for a no-op is how a company learns to
// ignore the one that mattered.
func TestARepeatedGoalSaveWakesNobody(t *testing.T) {
	r := newRoundTrip(t)
	goal := tracker.Goal{
		ID: "g-1", Name: "Ship the thing", Health: "on_track",
		Owners: []string{"alice"},
	}
	if _, err := r.writer.WriteGoal(t.Context(), "op-first", goal); err != nil {
		t.Fatalf("first save: %v", err)
	}
	r.drain()
	if _, err := r.writer.WriteGoal(t.Context(), "op-second", goal); err != nil {
		t.Fatalf("second save: %v", err)
	}
	if wake := r.lastWake(); wake != nil {
		t.Fatalf("a save that changed nothing woke %v", candidateHandles(wake))
	}
}

// A HEALTH UPDATE IS THE ONE A GOAL'S OWNERS ACTUALLY NEED, and it can arrive
// with no other field moving at all — so it has to count as a change on its
// own, and its prose is what the card carries.
func TestAGoalHealthUpdateCarriesItsProse(t *testing.T) {
	r := newRoundTrip(t)
	goal := tracker.Goal{
		ID: "g-1", Name: "Ship the thing", Health: "on_track",
		Owners: []string{"alice"},
	}
	if _, err := r.writer.WriteGoal(t.Context(), "op-first", goal); err != nil {
		t.Fatalf("first save: %v", err)
	}
	r.drain()

	goal.Updates = []tracker.GoalUpdate{{
		Health: "at_risk", Text: "the vendor slipped a fortnight",
	}}
	if _, err := r.writer.WriteGoal(t.Context(), "op-update", goal); err != nil {
		t.Fatalf("post an update: %v", err)
	}
	wake := r.lastWake()
	if wake == nil {
		t.Fatal("a health update woke nobody")
	}
	if !strings.Contains(wake.Excerpt, "vendor slipped") {
		t.Errorf("the card does not carry what somebody wrote: %q", wake.Excerpt)
	}
}

// A SPRINT CLOSE WAKES THE SEATS WITH WORK IN IT, and the project's lead.
func TestASprintCloseWakesItsAssigneesAndLead(t *testing.T) {
	r := newRoundTrip(t)
	r.writer.Leads = fixedLeads{project: "lead"}
	number := r.startedSprintWithWork("alice")
	r.drainForClose()

	if _, err := r.writer.CloseSprint(t.Context(), "op-close", "ENG", number); err != nil {
		t.Fatalf("close the sprint: %v", err)
	}
	wake := r.lastWake()
	if wake == nil {
		t.Fatal("a sprint close published no notification — the one moment a " +
			"sprint has something to report, and nobody is told")
	}
	if wake.Kind != tracker.ChangeSprintClosed {
		t.Fatalf("kind = %q, want %q", wake.Kind, tracker.ChangeSprintClosed)
	}
	handles := candidateHandles(wake)
	for _, who := range []string{"alice", "lead"} {
		if !slices.Contains(handles, who) {
			t.Errorf("%s is not woken by the close: %v", who, handles)
		}
	}
	if !strings.Contains(wake.Excerpt, "closed") {
		t.Errorf("the card does not say what happened: %q", wake.Excerpt)
	}
	// AND IT COUNTS WHAT IT CLOSED WITH. `open_at_close` has been a column
	// on the row and a field in the applier's INSERT since the tracker
	// landed, and no writer ever set it — so every sprint this engine has
	// closed reported 0, `rollover_pending` could never be true, and the
	// card read "closed with 0 open task(s)" over a fortnight's unfinished
	// work.
	if !strings.Contains(wake.Excerpt, "1 open task") {
		t.Errorf("the sprint held one unfinished task and its card says %q",
			wake.Excerpt)
	}
	// THE SPILLOVER IS PENDING AT THE CLOSE, which is not a bug: the
	// rollover runs after, and a close that claimed to know where the work
	// went would be reporting a decision nobody had made.
	if !strings.Contains(wake.Excerpt, "rollover pending") {
		t.Errorf("the card does not say the spillover is unsettled: %q",
			wake.Excerpt)
	}
}

// AND A START WAKES THEM TOO, under the same reason.
func TestASprintStartWakesItsAssignees(t *testing.T) {
	r := newRoundTrip(t)
	r.writer.Leads = fixedLeads{project: "lead"}
	r.startedSprintWithWork("alice")

	wake := r.lastWake()
	if wake == nil || wake.Kind != tracker.ChangeSprintStarted {
		t.Fatalf("a sprint start published %+v, want a %q wake",
			wake, tracker.ChangeSprintStarted)
	}
	if handles := candidateHandles(wake); !slices.Contains(handles, "alice") {
		t.Errorf("the seat with work in the sprint is not woken: %v", handles)
	}
}

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

// startedSprintWithWork mints a sprint, puts one assigned task in it and
// starts it, returning the sprint's number.
func (r *roundTrip) startedSprintWithWork(assignee string) int {
	r.t.Helper()
	const number = 1
	seedNamedSprint(r.t, r, number, "Kickoff", tracker.SprintFuture)

	task := newTask("t-sprint-work")
	task.Key = ""
	task.Assignee = assignee
	sprint := number
	task.Sprint = &sprint
	if _, err := r.writer.CreateTask(r.t.Context(), "op-sprint-task", task, nil); err != nil {
		r.t.Fatalf("file work into the sprint: %v", err)
	}
	r.drain()

	if _, err := r.writer.StartSprint(r.t.Context(), "op-start", "ENG", number); err != nil {
		r.t.Fatalf("start the sprint: %v", err)
	}
	// NOT DRAINED. The caller that wants the START's own wake reads it off
	// the log before anything else lands on it, and the caller that closes
	// drains first — see [roundTrip.drainForClose].
	return number
}

// drainForClose applies the start so the sprint is `active` in this node's own
// rows, which is what the close's state machine reads.
func (r *roundTrip) drainForClose() { r.drain() }

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

// A GOAL SAVE DOES NOT DESTROY ITS HEALTH HISTORY.
//
// A save is a whole post-state replace and `write_work_goal` builds its Goal
// from the tool's own arguments, which carry no updates — so every save
// through the only shipped surface wiped the entire history, silently, and
// reported `outcome: applied`. The updates are the one part of a goal somebody
// wrote in their own words, and the part the wake is about.
func TestAGoalSaveKeepsItsUpdateHistory(t *testing.T) {
	r := newRoundTrip(t)
	goal := tracker.Goal{
		ID: "g-1", Name: "Ship it", Health: "on_track", Owners: []string{"alice"},
		Updates: []tracker.GoalUpdate{{Health: "on_track", Text: "week one fine"}},
	}
	if _, err := r.writer.WriteGoal(t.Context(), "op-1", goal); err != nil {
		t.Fatalf("first save: %v", err)
	}
	r.drain()

	// A LATER SAVE CARRYING NO UPDATES — which is every save the tool
	// makes — must leave the history alone.
	goal.Updates = nil
	goal.Health = "at_risk"
	if _, err := r.writer.WriteGoal(t.Context(), "op-2", goal); err != nil {
		t.Fatalf("second save: %v", err)
	}
	r.drain()

	got := r.goals(tracker.GoalQuery{ID: "g-1"})
	if len(got.Goals) != 1 {
		t.Fatalf("read back %d goals", len(got.Goals))
	}
	if len(got.Goals[0].Updates) != 1 {
		t.Fatalf("the history is %v — a save with no updates destroyed what "+
			"somebody wrote", got.Goals[0].Updates)
	}
	// AND THE READER SURFACES IT, which is what the wake tells a seat to
	// go and read.
	if got.Goals[0].Updates[0].Text != "week one fine" {
		t.Errorf("the update's prose is %q", got.Goals[0].Updates[0].Text)
	}
	// AND THE AUTHOR AND INSTANT ARE THE WRITER'S, never the caller's.
	if got.Goals[0].Updates[0].Author == "" {
		t.Error("the update names no author, so a health assessment cannot " +
			"be attributed to anybody")
	}
}

// AND A NEW UPDATE APPENDS RATHER THAN REPLACING.
func TestAGoalUpdateAppends(t *testing.T) {
	r := newRoundTrip(t)
	goal := tracker.Goal{
		ID: "g-1", Name: "Ship it", Health: "on_track", Owners: []string{"alice"},
		Updates: []tracker.GoalUpdate{{Health: "on_track", Text: "week one"}},
	}
	if _, err := r.writer.WriteGoal(t.Context(), "op-1", goal); err != nil {
		t.Fatalf("first save: %v", err)
	}
	r.drain()
	goal.Updates = []tracker.GoalUpdate{{Health: "at_risk", Text: "week two"}}
	if _, err := r.writer.WriteGoal(t.Context(), "op-2", goal); err != nil {
		t.Fatalf("second save: %v", err)
	}
	r.drain()

	got := r.goals(tracker.GoalQuery{ID: "g-1"})
	if len(got.Goals[0].Updates) != 2 {
		t.Fatalf("the history is %v, want both updates", got.Goals[0].Updates)
	}
	if got.Goals[0].Updates[1].Text != "week two" {
		t.Errorf("the newest update is %q", got.Goals[0].Updates[1].Text)
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

// A GOAL UPDATE PAST ITS CAP IS REFUSED RATHER THAN CUT.
//
// The updates are the STORED value rather than a preview of one — there is
// nowhere to go and read the rest — so cutting would silently discard the end
// of somebody's assessment and leave them believing they had filed it.
func TestAnOversizedGoalUpdateIsRefused(t *testing.T) {
	r := newRoundTrip(t)
	_, err := r.writer.WriteGoal(t.Context(), "op-goal", tracker.Goal{
		ID: "g-1", Name: "Ship it", Owners: []string{"alice"},
		Updates: []tracker.GoalUpdate{{
			Health: "at_risk",
			Text:   strings.Repeat("y", tracker.MaxGoalUpdateText+1),
		}},
	})
	if err == nil {
		t.Fatal("an oversized goal update was silently cut and stored")
	}
	if !strings.Contains(err.Error(), "at most") {
		t.Errorf("the refusal does not name the cap: %v", err)
	}
}

func clipForTest(s string) string {
	if len(s) <= 80 {
		return s
	}
	return s[:80] + "..."
}
