package worklet_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// EVERY HISTORY ROW SAYS WHAT MOVED — the round-trip half. The comparisons are
// pure and tested as values in `deltas_test.go`; these cases hold the wiring
// nothing else sees: the APPLIER runs them, on the record's own subject,
// against the document this node had stored, including on the commits that
// wake nobody (every project reconcile, view save and catalogue edit, and most
// person writes).

// fieldsFor reads the newest history row's deltas for one kind.
func (r *roundTrip) fieldsFor(kind worklet.ChangeKind) string {
	r.t.Helper()
	got := r.strings(`SELECT fields_json FROM tracker_history
		WHERE kind = ? ORDER BY log_seq DESC LIMIT 1`, string(kind))
	if len(got) == 0 {
		r.t.Fatalf("no history row of kind %q was written, so there is "+
			"nothing for this case to assert about", kind)
	}
	return got[0]
}

// fieldsForSubject reads the newest history row's deltas for one subject.
func (r *roundTrip) fieldsForSubject(id string) string {
	r.t.Helper()
	got := r.strings(`SELECT fields_json FROM tracker_history
		WHERE subject_id = ? ORDER BY log_seq DESC LIMIT 1`, id)
	if len(got) == 0 {
		r.t.Fatalf("no history row was written for subject %q", id)
	}
	return got[0]
}

// A PROJECT'S OWN ROW SAYS WHAT THE CHART CHANGED. A chart reconcile carries
// no notification (nobody is woken for the engine keeping a project in step
// with the config), so its delta can only come from the applier.
func TestAProjectReconcileRecordsWhatTheChartMoved(t *testing.T) {
	t.Parallel()
	r := newRoundTripWithoutProject(t)

	if _, err := r.writer.ApplyChart(t.Context(), activatedAt(100), []worklet.ChartProject{
		{Key: "ENG", Name: "Engineering", Purpose: "builds it", Unit: "Engineering"},
	}); err != nil {
		t.Fatalf("the first chart apply: %v", err)
	}
	r.drain()
	// A CREATE LISTS WHAT IT SET, because every field moves from empty.
	if got := r.fieldsFor(worklet.ChangeProjectCreated); got != `{`+
		`"chart_epoch":{"from":"0","to":"100"},`+
		`"name":{"from":"","to":"Engineering"},`+
		`"purpose":{"from":"","to":"builds it"},`+
		`"unit":{"from":"","to":"Engineering"}}` {

		t.Errorf("a project create recorded %s", got)
	}

	// AND AN EDIT NAMES ONLY WHAT IT MOVED. The epoch travels with it because a
	// re-declaration at a new activation is a real fact about the row, and a
	// reconcile that changes nothing else would otherwise record an empty row.
	if _, err := r.writer.ApplyChart(t.Context(), activatedAt(200), []worklet.ChartProject{
		{Key: "ENG", Name: "Engineering", Purpose: "ships it", Unit: "Engineering"},
	}); err != nil {
		t.Fatalf("the second chart apply: %v", err)
	}
	r.drain()
	if got := r.fieldsFor(worklet.ChangeProjectUpdated); got != `{`+
		`"chart_epoch":{"from":"100","to":"200"},`+
		`"purpose":{"from":"builds it","to":"ships it"}}` {

		t.Errorf("a project purpose edit recorded %s — the row is supposed to "+
			"carry the purpose before and after, and nothing that did not "+
			"move", got)
	}
}

// A SAVED VIEW'S ROW NAMES THE PARAMETERS THAT MOVED, and only those: a saved
// query is bounded at [worklet.MaxViewParamsBytes], so carrying it whole would
// log tens of kilobytes to show one filter narrowed, and the keys alone would
// miss a re-pointed `assignee`, a view's commonest edit, which moves no key.
func TestASavedViewRecordsWhichParametersMoved(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	if _, err := save(t, r.writer, "op-view", aView("v-1", nil)); err != nil {
		t.Fatalf("save the view: %v", err)
	}
	r.drain()

	if _, err := save(t, r.writer, "op-view-2", aView("v-1", func(v *worklet.View) {
		v.Params = map[string]string{"assignee": "bo", "status": "todo"}
	})); err != nil {
		t.Fatalf("re-save the view: %v", err)
	}
	r.drain()

	// `sort` LEFT, `status` ARRIVED, `assignee` MOVED: three different moves,
	// which both sides carrying only the changed keys tell apart.
	if got := r.fieldsFor(worklet.ChangeViewSaved); got != `{"params":{`+
		`"from":"assignee=ana, sort=-updated",`+
		`"to":"assignee=bo, status=todo"}}` {

		t.Errorf("a view re-save recorded %s", got)
	}
}

// A PRIORITY LIST'S ROW CARRIES THE ORDER, BEFORE AND AFTER: the order IS the
// instruction, so this one collection is recorded as stored rather than
// sorted, unlike every set beside it.
func TestAPriorityListRecordsTheOrderItMoved(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "t-1")
	filedTask(t, r, "t-2")

	lead := r.writer.As("bob", worklet.AuthorHuman, worklet.Provenance{})
	if _, err := lead.WritePriorities(t.Context(), "op-prio", "ana",
		[]string{"t-1", "t-2"}, nil, worklet.PersonAuthority{Authorized: true}); err != nil {
		t.Fatalf("WritePriorities: %v", err)
	}
	r.drain()
	if _, err := lead.WritePriorities(t.Context(), "op-prio-2", "ana",
		[]string{"t-2", "t-1"}, nil, worklet.PersonAuthority{Authorized: true}); err != nil {
		t.Fatalf("WritePriorities again: %v", err)
	}
	r.drain()

	// THE EXCERPT ALREADY SAID "put X at position 1"; only the delta tells a
	// re-order from a list somebody replaced.
	if got := r.fieldsFor(worklet.ChangePrioritised); got != `{"priorities":{`+
		`"from":"t-1, t-2","to":"t-2, t-1"}}` {

		t.Errorf("a re-ordered priority list recorded %s", got)
	}

	// AND THE FIRST WRITE, onto a person with no list, has a delta too: the
	// applier compares the stored document with the record's, so the list
	// arriving is a move rather than the bare word "prioritised".
	first := r.strings(`SELECT fields_json FROM tracker_history
		WHERE kind = ? ORDER BY log_seq ASC LIMIT 1`, string(worklet.ChangePrioritised))
	if len(first) == 0 {
		t.Fatal("the first priorities write left no history row")
	}
	var moved map[string]worklet.Delta
	if err := json.Unmarshal([]byte(first[0]), &moved); err != nil {
		t.Fatalf("the first write's deltas %s: %v", first[0], err)
	}
	if got := moved["priorities"]; got.From != "" || got.To != "t-1, t-2" {
		t.Errorf("the first priorities write recorded %s, want the list arriving "+
			"from nothing", first[0])
	}
}

// A DEPENDENCY RECORDS BOTH ENDS, EACH ON ITS OWN SUBJECT: the authored
// `waiting_on` on the dependent and the mirrored `Dependents` on the blocker
// are separate commits, and [worklet.Wake] sees neither, because a dependency
// commit's wake has `Before` and `After` set to the SAME task. So the delta is
// the applier's.
func TestADependencyEdgeRecordsBothEnds(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "dep")
	filedTask(t, r, "blk")

	result, err := r.writer.Depend(t.Context(), "op-depend", worklet.DependencyChange{
		Task: "dep", Project: "ENG", WaitingOnAdd: []string{"blk"},
	}, fixedLeads{project: "eng-lead"})
	if err != nil {
		t.Fatalf("Depend: %v", err)
	}
	r.drain()
	if len(result.OneSided) != 0 {
		t.Fatalf("the mirror did not land: %v", result.OneSided)
	}

	// THE AUTHORED END names what this task now waits on.
	if got := r.fieldsForSubject("dep"); got != `{"waiting_on":{"from":"","to":"","added":["blk"]}}` {
		t.Errorf("the dependent's row recorded %s", got)
	}
	// AND THE MIRROR names who now waits on the blocker, under this package's
	// word for that direction.
	if got := r.fieldsForSubject("blk"); got != `{"blocking":{"from":"","to":"","added":["dep"]}}` {
		t.Errorf("the blocker's row recorded %s", got)
	}

	// AND A REMOVAL IS THE SAME FACT BACKWARDS, the sharpest case for the delta
	// being the applier's: an unblock tells nobody, so this commit carries no
	// notification to derive one from.
	if _, err := r.writer.Depend(t.Context(), "op-undepend", worklet.DependencyChange{
		Task: "dep", Project: "ENG", WaitingOnRemove: []string{"blk"},
	}, fixedLeads{project: "eng-lead"}); err != nil {
		t.Fatalf("Depend (remove): %v", err)
	}
	r.drain()
	if got := r.fieldsForSubject("dep"); got != `{"waiting_on":{"from":"","to":"","removed":["blk"]}}` {
		t.Errorf("the removal recorded %s", got)
	}
	if got := r.fieldsForSubject("blk"); got != `{"blocking":{"from":"","to":"","removed":["dep"]}}` {
		t.Errorf("the blocker's removal recorded %s", got)
	}
}

// AN EDGE WHOSE MIRROR NEVER LANDED STILL RECORDS THE AUTHORED SIDE: the
// authored commit is durable and the mirror best effort, so a crashed
// dependency gesture leaves a one-sided edge, and a delta that needed both
// ends would go missing on exactly the commit somebody has to repair.
func TestAnUnmirroredEdgeStillRecordsTheAuthoredSide(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "dep")
	filedTask(t, r, "blk")

	// THE AUTHORED HALF ALONE: the residue of a gesture whose mirror step did
	// not run.
	if _, err := r.writer.UpdateTask(t.Context(), "op-half", "dep", "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{Relate: &worklet.RelationIntent{
			Add: []worklet.Relation{{Kind: worklet.RelationWaitingOn, Other: "blk"}},
		}}, worklet.ChangeRelations, nil); err != nil {
		t.Fatalf("write the authored half: %v", err)
	}
	r.drain()

	if got := r.fieldsForSubject("dep"); got != `{"waiting_on":{"from":"","to":"","added":["blk"]}}` {
		t.Errorf("a half-written edge recorded %s on the dependent", got)
	}
	// AND THE BLOCKER'S OWN HISTORY IS UNTOUCHED: no commit landed on it.
	if rows := r.strings(`SELECT fields_json FROM tracker_history
		WHERE subject_id = ? AND kind = ?`,
		"blk", string(worklet.ChangeRelations)); len(rows) != 0 {

		t.Errorf("the blocker carries %v for an edge nothing mirrored", rows)
	}
}

// THE TWO QUIETEST DOCUMENTS RECORD WHAT THEY MOVED TOO: a tag set and a
// workspace catalogue are edited with NO notification, deliberately, yet the
// feed must still say a dropdown was renamed. The case also holds the
// applier's per-kind wiring the pure comparisons cannot see: an arm that
// decoded the wrong document would compare two zero values and record nothing.
func TestTheQuietDocumentsRecordWhatMoved(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	if _, err := r.writer.WriteTags(t.Context(), "op-tags", "ENG",
		worklet.TagEdit{Add: []worklet.Tag{{Slug: "regression"}}},
		worklet.TagAuthority{}); err != nil {
		t.Fatalf("declare a tag: %v", err)
	}
	r.drain()
	if got := r.fieldsFor(worklet.ChangeTags); got != `{`+
		`"tags":{"from":"","to":"","added":["regression"]},`+
		`"tags_version":{"from":"0","to":"1"}}` {

		t.Errorf("a tag declaration recorded %s", got)
	}

	// AND A RENAME, which moves no slug: the counter beside them is the only
	// witness, which is why it is recorded rather than left as bookkeeping.
	if _, err := r.writer.WriteTags(t.Context(), "op-rename", "ENG",
		worklet.TagEdit{Rename: map[string]string{"regression": "Regression"}},
		worklet.TagAuthority{Policy: true}); err != nil {
		t.Fatalf("rename a tag: %v", err)
	}
	r.drain()
	if got := r.fieldsFor(worklet.ChangeTags); got != `{"tags_version":{`+
		`"from":"1","to":"2"}}` {

		t.Errorf("a tag rename recorded %s", got)
	}

	if _, err := r.writer.WriteTypes(t.Context(), "op-types", []worklet.TaskType{
		{Slug: "incident", Name: "Incident"},
	}); err != nil {
		t.Fatalf("declare a type: %v", err)
	}
	r.drain()
	if got := r.fieldsFor(worklet.ChangeCatalogue); got != `{"types":{`+
		`"from":"","to":"","added":["incident"]}}` {

		t.Errorf("a type declaration recorded %s", got)
	}

	if _, err := r.writer.WriteFields(t.Context(), "op-fields", []worklet.FieldDef{
		{ID: "f-sev", Slug: "severity", Name: "Severity", Type: worklet.FieldText},
	}); err != nil {
		t.Fatalf("declare a field: %v", err)
	}
	r.drain()
	if got := r.fieldsFor(worklet.ChangeCatalogue); got != `{`+
		`"fields":{"from":"","to":"","added":["severity"]},`+
		`"policy_version":{"from":"0","to":"1"}}` {

		t.Errorf("a field declaration recorded %s", got)
	}
}

// THE ANSWER NAMES THE TASKS ITS DELTAS POINT AT, so a page of edges is
// readable. A delta names the other end by ID, because a key is a fact about
// another task's row and a history row is inside this domain's identity claim,
// repaired by nothing; a surface holds no map to resolve one. So the READ
// resolves it, on the answer, where it binds nothing and two nodes may
// legitimately differ.
func TestAnActivityAnswerNamesTheTasksItsDeltasPointAt(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "dep")
	filedTask(t, r, "blk")

	if _, err := r.writer.Depend(t.Context(), "op-depend", worklet.DependencyChange{
		Task: "dep", Project: "ENG", WaitingOnAdd: []string{"blk"},
	}, fixedLeads{project: "eng-lead"}); err != nil {
		t.Fatalf("Depend: %v", err)
	}
	r.drain()

	answer, err := r.reader.Activity(t.Context(), worklet.ActivityQuery{
		Project: "ENG", Level: statelog.ReadStale,
	}, wednesday)
	if err != nil {
		t.Fatalf("Activity: %v", err)
	}
	// BOTH ENDS, because each row names the other by id. THE KEY IS READ OFF
	// THE ROW rather than written here: the create MINTS it from the project's
	// own counter, so a fixture's name for a task is not its key, which is why
	// a writer cannot state one and the read has to resolve it.
	for _, id := range []string{"dep", "blk"} {
		want := r.task(t, id).Task.Key
		if want == "" {
			t.Fatalf("task %s has no key, so this case asserts nothing", id)
		}
		if got := answer.Keys[id]; got != want {
			t.Errorf("the answer resolves %s to %q, want %q — the page shows "+
				"a uuid without it", id, got, want)
		}
	}

	// AND AN ID THIS NODE HOLDS NO ROW FOR IS ABSENT, never an empty string: a
	// renderer falls back to the id, as it does past the cap. Which delta
	// fields are asked about is asserted on the declaration
	// (TestOnlyTheDeltaFieldsThatNameTasksAreResolved): a `page` edge's id
	// resolves to nothing here either way, so it could not test the exclusion.
	if _, err := r.writer.UpdateTask(t.Context(), "op-ghost", "dep", "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{Relate: &worklet.RelationIntent{
			Add: []worklet.Relation{
				{Kind: worklet.RelationLinked, Other: "no-such-task"},
			},
		}}, worklet.ChangeRelations, nil); err != nil {
		t.Fatalf("write the edge: %v", err)
	}
	r.drain()

	answer, err = r.reader.Activity(t.Context(), worklet.ActivityQuery{
		Project: "ENG", Level: statelog.ReadStale,
	}, wednesday)
	if err != nil {
		t.Fatalf("Activity after the edge: %v", err)
	}
	if got, named := answer.Keys["no-such-task"]; named {
		t.Errorf("the answer resolves no-such-task to %q, and this node holds "+
			"no row for it", got)
	}
	// THE EDGE STILL REACHED THE ROW, or the assertion above passes vacuously.
	if got := r.fieldsForSubject("dep"); !strings.Contains(got, "no-such-task") {
		t.Fatalf("the edge recorded %s, so the assertion above asserts "+
			"nothing", got)
	}

	// AND A PAGE THAT POINTS AT NOTHING CARRIES NO MAP, so the field is
	// omitted rather than sent as an empty object.
	plain, err := r.reader.Activity(t.Context(), worklet.ActivityQuery{
		Project: "ENG", Kinds: []worklet.ChangeKind{worklet.ChangeCreated},
		Level: statelog.ReadStale,
	}, wednesday)
	if err != nil {
		t.Fatalf("Activity over the creates: %v", err)
	}
	if len(plain.Records) == 0 {
		t.Fatal("no create rows came back, so this half asserts nothing")
	}
	if plain.Keys != nil {
		t.Errorf("a page whose deltas name no task carries %v", plain.Keys)
	}
}

// A TASK'S OWN DELTA ROW IS WHAT IT ALWAYS WAS: [worklet.TaskDeltas] puts a
// field move and an edge move in ONE row, so a commit that touches no edge
// must write exactly the bytes it wrote before, because `fields_json` is in
// the state log's identity claim and every existing row in a company's
// history was written by the old comparison.
func TestATaskDeltaRowIsUnchangedByTheEdgeFields(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "t-1")

	status := worklet.StatusInProgress
	if _, err := r.writer.UpdateTask(t.Context(), "op-status", "t-1", "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{Status: &status},
		worklet.ChangeStatus, nil); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	r.drain()

	if got := r.fieldsForSubject("t-1"); got != `{"status":{`+
		`"from":"todo","to":"in_progress"}}` {

		t.Errorf("a status change recorded %s — a commit that moved no edge "+
			"must write the same bytes it wrote before the edge fields "+
			"existed", got)
	}
}

// AND IT IS STILL WHAT IT ALWAYS WAS AFTER THE TEN FIELDS BELOW ARRIVED:
// [TestATaskDeltaRowIsUnchangedByTheEdgeFields]'s assertion against a task
// that CARRIES a body, a reporter, a watcher, a checklist and a custom value.
// A comparison that recorded a field's CURRENT value rather than its move
// would put all five on every status change in the company for ever.
func TestATaskDeltaRowIsUnchangedByTheNewFields(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	if _, err := r.writer.WriteFields(t.Context(), "op-fields", []worklet.FieldDef{
		{ID: "f-sev", Slug: "severity", Name: "Severity", Type: worklet.FieldText},
	}); err != nil {
		t.Fatalf("declare a field: %v", err)
	}
	r.drain()

	task := newTask("t-1")
	task.Key = "ENG-t-1"
	task.Body = "what this is about"
	task.Reporter = "cy"
	task.RoutingUnit = "Engineering"
	task.Watchers = []string{"ana"}
	task.Collaborators = []string{"bo"}
	task.Checklists = []worklet.Checklist{{
		ID: "c-1", Name: "Setup", Items: []worklet.ChecklistItem{{ID: "i-1"}},
	}}
	task.Fields = map[string]json.RawMessage{"severity": json.RawMessage(`"low"`)}
	if _, err := r.writer.CreateTask(t.Context(), "op-create", task, nil); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	r.drain()

	status := worklet.StatusInProgress
	if _, err := r.writer.UpdateTask(t.Context(), "op-status", "t-1", "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{Status: &status},
		worklet.ChangeStatus, nil); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	r.drain()

	if got := r.fieldsForSubject("t-1"); got != `{"status":{`+
		`"from":"todo","to":"in_progress"}}` {

		t.Errorf("a status change on a task carrying a body, a reporter, a "+
			"watcher, a checklist and a custom value recorded %s — a commit "+
			"that moved none of them must write the bytes it always wrote", got)
	}
}

// A WATCHER ROW NAMES WHO, AND A MUTE SAYS WHO OPTED OUT. The two fields stay
// separate because [settleWatch] writes both (an unwatch drops a handle from
// the set AND mutes it), and folding them would make "was never watching" and
// "chose to stop" the same row.
func TestAWatchCommitRecordsTheHandlesThatMoved(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "t-1")

	if _, err := r.writer.UpdateTask(t.Context(), "op-watch", "t-1", "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{
			Watch: &worklet.WatchIntent{Handle: "bo", Watch: true},
		}, worklet.ChangeWatchers, nil); err != nil {
		t.Fatalf("watch: %v", err)
	}
	r.drain()
	if got := r.fieldsForSubject("t-1"); got != `{"watchers":{"from":"","to":"","added":["bo"]}}` {
		t.Errorf("starting to watch recorded %s", got)
	}

	if _, err := r.writer.UpdateTask(t.Context(), "op-unwatch", "t-1", "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{
			Watch: &worklet.WatchIntent{Handle: "bo", Watch: false},
		}, worklet.ChangeWatchers, nil); err != nil {
		t.Fatalf("unwatch: %v", err)
	}
	r.drain()
	// BOTH HALVES OF THE ONE GESTURE, which is what an unwatch is.
	if got := r.fieldsForSubject("t-1"); got != `{`+
		`"muted":{"from":"","to":"","added":["bo"]},`+
		`"watchers":{"from":"","to":"","removed":["bo"]}}` {

		t.Errorf("an unwatch recorded %s", got)
	}
}

// A CUSTOM FIELD IS RESOLVED TO ITS SLUG INSIDE THE APPLY. A caller's wake
// holds no catalogue to compute this delta (an update's decide restates its
// card with one, [worklet.Notify]'s restatement). Values are keyed by field
// ID, and the declarations the applier reads to write the value rows turn the
// key back into the slug, not `f-sev=low → f-sev=high` or a minted uuid.
func TestACustomFieldRowNamesTheFieldBySlug(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	if _, err := r.writer.WriteFields(t.Context(), "op-fields", []worklet.FieldDef{
		{ID: "f-sev", Slug: "severity", Name: "Severity", Type: worklet.FieldDropdown,
			Config: worklet.FieldConfig{Options: []worklet.Option{
				{ID: "o-low", Slug: "low", Name: "Low"},
				{ID: "o-high", Slug: "high", Name: "High"},
			}}},
	}); err != nil {
		t.Fatalf("declare a field: %v", err)
	}
	r.drain()
	filedTask(t, r, "t-1")

	set := map[string]json.RawMessage{"severity": json.RawMessage(`"low"`)}
	if _, err := r.writer.UpdateTask(t.Context(), "op-set", "t-1", "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{Fields: &set},
		worklet.ChangeFields, nil); err != nil {
		t.Fatalf("set the field: %v", err)
	}
	r.drain()
	// THE OPTION'S SLUG, NOT ITS ID: [coerceOption] stores the option id so a
	// rename never re-points a stored value, and the same catalogue reads it.
	if got := r.fieldsForSubject("t-1"); got != `{"fields":{`+
		`"from":"","to":"severity=low"}}` {

		t.Errorf("setting a custom field recorded %s", got)
	}

	raise := map[string]json.RawMessage{"severity": json.RawMessage(`"high"`)}
	if _, err := r.writer.UpdateTask(t.Context(), "op-raise", "t-1", "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{Fields: &raise},
		worklet.ChangeFields, nil); err != nil {
		t.Fatalf("raise the field: %v", err)
	}
	r.drain()
	if got := r.fieldsForSubject("t-1"); got != `{"fields":{`+
		`"from":"severity=low","to":"severity=high"}}` {

		t.Errorf("re-pointing a custom field recorded %s", got)
	}

	// AND CLEARING IT IS THE SAME FACT BACKWARDS, on a commit whose only
	// content is the clearing, so its row would otherwise be empty.
	empty := map[string]json.RawMessage{}
	if _, err := r.writer.UpdateTask(t.Context(), "op-clear", "t-1", "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{Fields: &empty},
		worklet.ChangeFields, nil); err != nil {
		t.Fatalf("clear the field: %v", err)
	}
	r.drain()
	if got := r.fieldsForSubject("t-1"); got != `{"fields":{`+
		`"from":"severity=high","to":""}}` {

		t.Errorf("clearing a custom field recorded %s", got)
	}
}

// A BODY EDIT SAYS THAT ONE HAPPENED AND HOW BIG IT IS, AND NEVER THE PROSE:
// [MaxBody] is 32 KiB and this table is never swept, so both sides would cost
// 64 KiB per edit on every node for the company's life. The text is on the
// row's own `document` column.
func TestABodyEditRecordsAMarkerRatherThanTheProse(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "t-1")

	written := "the original description, at some length"
	if _, err := r.writer.UpdateTask(t.Context(), "op-body", "t-1", "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{Body: &written},
		worklet.ChangeFields, nil); err != nil {
		t.Fatalf("write the body: %v", err)
	}
	r.drain()
	if got := r.fieldsForSubject("t-1"); got != `{"body":{"from":"","to":"40 bytes"}}` {
		t.Errorf("writing a body recorded %s", got)
	}

	// A REWRITE OF THE SAME LENGTH IS STILL A CHANGE: the markers read the
	// same, so the key's presence says the field moved; compared on the
	// markers, a typo fix would record nothing.
	fixed := "the original description, at some weight"
	if _, err := r.writer.UpdateTask(t.Context(), "op-typo", "t-1", "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{Body: &fixed},
		worklet.ChangeFields, nil); err != nil {
		t.Fatalf("fix the body: %v", err)
	}
	r.drain()
	if got := r.fieldsForSubject("t-1"); got !=
		`{"body":{"from":"40 bytes","to":"40 bytes"}}` {

		t.Errorf("an edit that kept the length recorded %s", got)
	}
}

// A RE-PARENT NAMES BOTH ENDS, BY ID — AND THE ANSWER NAMES THE KEY. The id is
// `deltas.go`'s rule (a key is a fact about another task's row), and the
// activity read resolves it. Both halves in one case, because either alone
// leaves a uuid on screen. `parent` is a SCALAR delta and the key map was
// built for the edge lists, so nothing but this says the read asks about it
// (TestOnlyTheDeltaFieldsThatNameTasksAreResolved holds the field list).
func TestAReparentRecordsBothParentsByID(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	r.applyWhileWriting()
	filedTask(t, r, "t-parent")
	filedTask(t, r, "t-child")

	parent := "t-parent"
	if _, err := r.writer.UpdateTask(t.Context(), "op-adopt", "t-child", "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{Parent: &parent},
		worklet.ChangeReparented, nil); err != nil {
		t.Fatalf("re-parent: %v", err)
	}
	r.drain()
	if got := r.fieldsForSubject("t-child"); got !=
		`{"parent":{"from":"","to":"t-parent"}}` {

		t.Errorf("a re-parent recorded %s", got)
	}

	// THE KEY IS READ OFF THE ROW: the create mints it, so the fixture's name
	// for the task is not its key.
	want := r.task(t, "t-parent").Task.Key
	if want == "" {
		t.Fatal("the parent has no key, so this half asserts nothing")
	}
	answer := r.activity(worklet.ActivityQuery{Project: "ENG"})
	if got := answer.Keys["t-parent"]; got != want {
		t.Errorf("the answer resolves the new parent to %q, want %q — the "+
			"item's History tab renders the uuid without it", got, want)
	}
}
