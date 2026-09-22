package tracker_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/tracker"
)

// EVERY HISTORY ROW SAYS WHAT MOVED — the round-trip half.
//
// The comparisons themselves are pure and are exercised as values in
// `deltas_test.go`. What these cases hold is the wiring nothing else can see:
// that the APPLIER runs them, on the record's own subject, against the
// document this node had stored — and that it does so on the commits that wake
// nobody, which is every project reconcile, every view save, every catalogue
// edit and most person writes.
//
// Before this, `fields_json` on all five document kinds was the literal `{}`,
// and a dependency's row was `{}` too: the activity feed of a company on its
// first day was six rows reading "project created" with a dash where the
// change belongs.

// fieldsFor reads the newest history row's deltas for one kind.
func (r *roundTrip) fieldsFor(kind tracker.ChangeKind) string {
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

// A PROJECT'S OWN ROW SAYS WHAT THE CHART CHANGED.
//
// A chart reconcile carries no notification at all — it is the engine keeping
// a project in step with the config, and nobody is woken for it — so its
// delta could only ever come from the applier. Until it did, a founder editing
// a unit's purpose produced a `project_updated` row that named the project and
// nothing about the change.
func TestAProjectReconcileRecordsWhatTheChartMoved(t *testing.T) {
	t.Parallel()
	r := newRoundTripWithoutProject(t)

	if _, err := r.writer.ApplyChart(t.Context(), 100, []tracker.ChartProject{
		{Key: "ENG", Name: "Engineering", Purpose: "builds it", Unit: "Engineering"},
	}); err != nil {
		t.Fatalf("the first chart apply: %v", err)
	}
	r.drain()
	// A CREATE LISTS WHAT IT SET, because every field moves from empty.
	if got := r.fieldsFor(tracker.ChangeProjectCreated); got != `{`+
		`"chart_epoch":{"from":"0","to":"100"},`+
		`"name":{"from":"","to":"Engineering"},`+
		`"purpose":{"from":"","to":"builds it"},`+
		`"unit":{"from":"","to":"Engineering"}}` {

		t.Errorf("a project create recorded %s", got)
	}

	// AND AN EDIT NAMES ONLY WHAT IT MOVED. The epoch travels with it
	// because a re-declaration at a new revision is a real fact about the
	// row — and because without it the reconcile that changes nothing else
	// would be the empty row again.
	if _, err := r.writer.ApplyChart(t.Context(), 101, []tracker.ChartProject{
		{Key: "ENG", Name: "Engineering", Purpose: "ships it", Unit: "Engineering"},
	}); err != nil {
		t.Fatalf("the second chart apply: %v", err)
	}
	r.drain()
	if got := r.fieldsFor(tracker.ChangeProjectUpdated); got != `{`+
		`"chart_epoch":{"from":"100","to":"101"},`+
		`"purpose":{"from":"builds it","to":"ships it"}}` {

		t.Errorf("a project purpose edit recorded %s — the row is supposed to "+
			"carry the purpose before and after, and nothing that did not "+
			"move", got)
	}
}

// A SAVED VIEW'S ROW NAMES THE PARAMETERS THAT MOVED, and only those.
//
// A saved query is bounded at [tracker.MaxViewParamsBytes], so carrying it
// whole on both sides would put tens of kilobytes of unchanged predicate into
// a log line to show that one filter was narrowed. The keys alone would be
// worse: re-pointing `assignee` changes no key at all, so the commonest edit a
// view gets would record nothing.
func TestASavedViewRecordsWhichParametersMoved(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	if _, err := r.writer.WriteView(t.Context(), "op-view", aView("v-1", nil)); err != nil {
		t.Fatalf("save the view: %v", err)
	}
	r.drain()

	if _, err := r.writer.WriteView(t.Context(), "op-view-2", aView("v-1", func(v *tracker.View) {
		v.Params = map[string]string{"assignee": "bo", "status": "todo"}
	})); err != nil {
		t.Fatalf("re-save the view: %v", err)
	}
	r.drain()

	// `sort` LEFT, `status` ARRIVED, `assignee` MOVED — three different
	// pictures, which is exactly what the two sides carrying only the
	// changed keys buys.
	if got := r.fieldsFor(tracker.ChangeViewSaved); got != `{"params":{`+
		`"from":"assignee=ana, sort=-updated",`+
		`"to":"assignee=bo, status=todo"}}` {

		t.Errorf("a view re-save recorded %s", got)
	}
}

// A PRIORITY LIST'S ROW CARRIES THE ORDER, BEFORE AND AFTER.
//
// The order IS the instruction, so a re-ordering with the same members is
// precisely the change somebody made — which is why this one collection is
// recorded as it is stored rather than sorted, unlike every set beside it.
func TestAPriorityListRecordsTheOrderItMoved(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "t-1")
	filedTask(t, r, "t-2")

	lead := r.writer.As("bob", tracker.AuthorHuman, tracker.Provenance{})
	if _, err := lead.WritePriorities(t.Context(), "op-prio", "ana",
		[]string{"t-1", "t-2"}, tracker.PersonAuthority{Lead: true}); err != nil {
		t.Fatalf("WritePriorities: %v", err)
	}
	r.drain()
	if _, err := lead.WritePriorities(t.Context(), "op-prio-2", "ana",
		[]string{"t-2", "t-1"}, tracker.PersonAuthority{Lead: true}); err != nil {
		t.Fatalf("WritePriorities again: %v", err)
	}
	r.drain()

	// THE EXCERPT ALREADY SAID "put X at position 1"; the delta is what
	// makes the whole move readable, and it is the only thing that
	// distinguishes a re-order from a list somebody replaced.
	if got := r.fieldsFor(tracker.ChangePrioritised); got != `{"priorities":{`+
		`"from":"t-1, t-2","to":"t-2, t-1"}}` {

		t.Errorf("a re-ordered priority list recorded %s", got)
	}
}

// A DEPENDENCY RECORDS BOTH ENDS, EACH ON ITS OWN SUBJECT.
//
// The two halves are separate commits on separate subjects — the authored
// `waiting_on` on the dependent and the mirrored `Dependents` on the blocker —
// and [tracker.Wake] cannot see either, because a dependency commit's wake is
// built with `Before` and `After` set to the SAME task. So the delta had to be
// the applier's, and without it both rows read as the bare word "relations".
func TestADependencyEdgeRecordsBothEnds(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "dep")
	filedTask(t, r, "blk")

	result, err := r.writer.Depend(t.Context(), "op-depend", tracker.DependencyChange{
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
	if got := r.fieldsForSubject("dep"); got != `{"waiting_on":{"from":"","to":"blk"}}` {
		t.Errorf("the dependent's row recorded %s", got)
	}
	// AND THE MIRROR names who now waits on the blocker, under the word
	// this package already uses for that direction.
	if got := r.fieldsForSubject("blk"); got != `{"blocking":{"from":"","to":"dep"}}` {
		t.Errorf("the blocker's row recorded %s", got)
	}

	// AND A REMOVAL IS THE SAME FACT BACKWARDS. It is the sharpest case
	// for the rule that the delta is the applier's rather than the wake's:
	// "an unblock tells nobody", so this commit carries no notification at
	// all and a delta derived from one would not exist.
	if _, err := r.writer.Depend(t.Context(), "op-undepend", tracker.DependencyChange{
		Task: "dep", Project: "ENG", WaitingOnRemove: []string{"blk"},
	}, fixedLeads{project: "eng-lead"}); err != nil {
		t.Fatalf("Depend (remove): %v", err)
	}
	r.drain()
	if got := r.fieldsForSubject("dep"); got != `{"waiting_on":{"from":"blk","to":""}}` {
		t.Errorf("the removal recorded %s", got)
	}
	if got := r.fieldsForSubject("blk"); got != `{"blocking":{"from":"dep","to":""}}` {
		t.Errorf("the blocker's removal recorded %s", got)
	}
}

// AN EDGE WHOSE MIRROR NEVER LANDED STILL RECORDS THE AUTHORED SIDE.
//
// The authored commit is durable the moment it lands and the mirror is best
// effort — so the residue of a crashed dependency gesture is a one-sided edge,
// and the history has to say that the edge was written. A delta that needed
// both ends would go missing on exactly the commit somebody has to go and
// repair.
func TestAnUnmirroredEdgeStillRecordsTheAuthoredSide(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "dep")
	filedTask(t, r, "blk")

	// THE AUTHORED HALF ALONE, which is exactly the residue a gesture
	// leaves when its mirror step did not run.
	if _, err := r.writer.UpdateTask(t.Context(), "op-half", "dep", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Relate: &tracker.RelationIntent{
			Add: []tracker.Relation{{Kind: tracker.RelationWaitingOn, Other: "blk"}},
		}}, tracker.ChangeRelations, nil); err != nil {
		t.Fatalf("write the authored half: %v", err)
	}
	r.drain()

	if got := r.fieldsForSubject("dep"); got != `{"waiting_on":{"from":"","to":"blk"}}` {
		t.Errorf("a half-written edge recorded %s on the dependent", got)
	}
	// AND THE BLOCKER'S OWN HISTORY IS UNTOUCHED, which is the honest
	// answer: no commit landed on it, so it has no row about this edge.
	if rows := r.strings(`SELECT fields_json FROM tracker_history
		WHERE subject_id = ? AND kind = ?`,
		"blk", string(tracker.ChangeRelations)); len(rows) != 0 {

		t.Errorf("the blocker carries %v for an edge nothing mirrored", rows)
	}
}

// THE TWO QUIETEST DOCUMENTS RECORD WHAT THEY MOVED TOO.
//
// A tag set and a workspace catalogue are edited with NO notification at all,
// deliberately — "a wake per catalogue edit would page the whole company for a
// renamed dropdown" — while the same comment promises that "the feed still has
// to be able to say a dropdown was renamed". It could not: both rows stored
// `{}`. This case is also what holds the applier's per-kind wiring, which the
// pure comparisons cannot see: an arm that decoded the wrong document would
// compare two zero values and quietly record nothing.
func TestTheQuietDocumentsRecordWhatMoved(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	if _, err := r.writer.WriteTags(t.Context(), "op-tags", "ENG",
		tracker.TagEdit{Add: []tracker.Tag{{Slug: "regression"}}},
		tracker.TagAuthority{}); err != nil {
		t.Fatalf("declare a tag: %v", err)
	}
	r.drain()
	if got := r.fieldsFor(tracker.ChangeTags); got != `{`+
		`"tags":{"from":"","to":"regression"},`+
		`"tags_version":{"from":"0","to":"1"}}` {

		t.Errorf("a tag declaration recorded %s", got)
	}

	// AND A RENAME, which moves no slug at all — the counter beside them
	// is the only witness that anything happened, which is why it is
	// recorded rather than left as bookkeeping.
	if _, err := r.writer.WriteTags(t.Context(), "op-rename", "ENG",
		tracker.TagEdit{Rename: map[string]string{"regression": "Regression"}},
		tracker.TagAuthority{Lead: true}); err != nil {
		t.Fatalf("rename a tag: %v", err)
	}
	r.drain()
	if got := r.fieldsFor(tracker.ChangeTags); got != `{"tags_version":{`+
		`"from":"1","to":"2"}}` {

		t.Errorf("a tag rename recorded %s", got)
	}

	if _, err := r.writer.WriteTypes(t.Context(), "op-types", []tracker.TaskType{
		{Slug: "incident", Name: "Incident"},
	}); err != nil {
		t.Fatalf("declare a type: %v", err)
	}
	r.drain()
	if got := r.fieldsFor(tracker.ChangeCatalogue); got != `{"types":{`+
		`"from":"","to":"incident"}}` {

		t.Errorf("a type declaration recorded %s", got)
	}

	if _, err := r.writer.WriteFields(t.Context(), "op-fields", []tracker.FieldDef{
		{ID: "f-sev", Slug: "severity", Name: "Severity", Type: tracker.FieldText},
	}); err != nil {
		t.Fatalf("declare a field: %v", err)
	}
	r.drain()
	if got := r.fieldsFor(tracker.ChangeCatalogue); got != `{`+
		`"fields":{"from":"","to":"severity"},`+
		`"policy_version":{"from":"0","to":"1"}}` {

		t.Errorf("a field declaration recorded %s", got)
	}
}

// A TASK'S OWN DELTA ROW IS WHAT IT ALWAYS WAS.
//
// The edges were added to [tracker.TaskDeltas] rather than filed under a
// change kind of their own, which puts a field move and an edge move in ONE
// row — and makes this the case that has to hold: a commit that touches no
// edge must write exactly the bytes it wrote before, because `fields_json` is
// in the state log's identity claim and every row already in a company's
// history was written by the old comparison.
func TestATaskDeltaRowIsUnchangedByTheEdgeFields(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "t-1")

	status := tracker.StatusInProgress
	if _, err := r.writer.UpdateTask(t.Context(), "op-status", "t-1", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Status: &status},
		tracker.ChangeStatus, nil); err != nil {
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
