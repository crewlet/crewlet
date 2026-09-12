package tracker_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"

	"github.com/crewlet/crewlet/internal/tracker"
)

// A VIEW IS A SET OF DEFAULTS, never a lock.
//
// The grammar's own doc has always said so — "loaded FIRST and explicit keys
// override them" — and nothing loaded one: `view=` was parsed and never read,
// so opening a saved board answered the UNFILTERED list.
func TestASavedViewIsDefaultsTheCallerCanOverride(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	if _, err := r.writer.WriteView(t.Context(), "op-view", tracker.View{
		ID:        "v-mine",
		Name:      "Ana's todo",
		Type:      tracker.ViewList,
		Container: tracker.Container{Kind: tracker.ContainerProject, ID: "ENG"},
		Params: map[string]string{
			"assignee": "ana", "status": "todo", "sort": "-updated",
		},
	}); err != nil {
		t.Fatalf("WriteView: %v", err)
	}
	r.drain()

	q, err := r.reader.ExpandedQuery(t.Context(),
		map[string]any{"container": "project:ENG", "view": "v-mine"},
		tracker.Viewer{}, wednesday, berlin)
	if err != nil {
		t.Fatalf("ExpandedQuery: %v", err)
	}
	switch {
	case len(q.Assignee) != 1 || q.Assignee[0] != "ana":
		t.Fatalf("the view's assignee reached the query as %v", q.Assignee)
	case len(q.Status) != 1 || q.Status[0] != tracker.StatusTodo:
		t.Fatalf("the view's status reached the query as %v", q.Status)
	case len(q.Sort) != 1 || q.Sort[0].Key != "updated" || !q.Sort[0].Descending:
		t.Fatalf("the view's sort reached the query as %v", q.Sort)
	}

	// AND AN EXPLICIT KEY WINS, which is the whole of "defaults rather
	// than a lock": somebody who opens a saved board and picks another
	// assignee gets the view with that one key changed.
	q, err = r.reader.ExpandedQuery(t.Context(), map[string]any{
		"container": "project:ENG", "view": "v-mine", "assignee": "bob",
	}, tracker.Viewer{}, wednesday, berlin)
	if err != nil {
		t.Fatalf("ExpandedQuery: %v", err)
	}
	if len(q.Assignee) != 1 || q.Assignee[0] != "bob" {
		t.Fatalf("the explicit assignee is %v, want bob — a view that could "+
			"not be overridden is a lock", q.Assignee)
	}
	if len(q.Status) != 1 || q.Status[0] != tracker.StatusTodo {
		t.Fatalf("overriding one key dropped the view's others: %v", q.Status)
	}
}

// A PRESET IS THE QUESTION ITS INDEX IS NAMED FOR.
func TestAPresetExpandsToTheQuestionItNames(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	// MY QUEUE NEEDS A VIEWER, and the surface supplies it — with the
	// viewer's own PROJECT, because the preset asks two things about the
	// reader rather than one.
	q, err := r.reader.ExpandedQuery(t.Context(),
		map[string]any{"container": "project:ENG", "preset": "my_queue"},
		tracker.Viewer{Handle: "ana", Project: "ENG"}, wednesday, berlin)
	if err != nil {
		t.Fatalf("ExpandedQuery: %v", err)
	}
	// "WHAT CAN I PICK UP" IS A DISJUNCTION, and both arms matter: the
	// work this person HOLDS, and the work in their own project that
	// NOBODY holds. Written as `assignee=me` alone it answered only the
	// first, so a seat with an empty queue read the company as having
	// nothing for it while its project's unclaimed backlog sat there.
	if len(q.Any) != 2 {
		t.Fatalf("my_queue expanded to %d branches, want two — mine, and the "+
			"unclaimed work in my own project", len(q.Any))
	}
	mine, unclaimed := q.Any[0], q.Any[1]
	if len(mine.Assignee) != 1 || mine.Assignee[0] != "ana" {
		t.Fatalf("the first branch is %v, want the viewer's own name",
			mine.Assignee)
	}
	if len(unclaimed.Assignee) != 1 || unclaimed.Assignee[0] != "none" ||
		unclaimed.Scope.Project != "ENG" {
		t.Fatalf("the second branch is assignee=%v in %q, want the unassigned "+
			"work in the viewer's OWN project — unscoped it offers every "+
			"unclaimed task in the company",
			unclaimed.Assignee, unclaimed.Scope.Project)
	}
	// AND BLOCKED WORK IS NOT SOMETHING TO PICK UP: it is
	// `preset=blocked`'s answer, and leaving it here would make the two
	// presets return the same rows for the wrong reason.
	if q.Blocked == nil || *q.Blocked {
		t.Fatalf("my_queue's blocked filter is %v, want false", q.Blocked)
	}
	if len(q.StatusGroups) != 2 {
		t.Fatalf("my_queue's groups are %v, want the two open ones",
			q.StatusGroups)
	}
	// PRIORITY THEN DUE, which is the order the index it names is built in.
	if len(q.Sort) != 2 || q.Sort[0].Key != "priority" || !q.Sort[0].Descending ||
		q.Sort[1].Key != "due" {
		t.Fatalf("my_queue's order is %v, want priority then due", q.Sort)
	}

	// A QUEUE WITH NOBODY'S NAME ON IT IS EVERY OPEN TASK, which is the
	// widest possible reading of "mine" — so it is refused.
	if _, err := r.reader.ExpandedQuery(t.Context(),
		map[string]any{"preset": "my_queue"}, tracker.Viewer{}, wednesday, berlin); err == nil {
		t.Fatal("my_queue was answered for nobody")
	}

	blocked, err := r.reader.ExpandedQuery(t.Context(),
		map[string]any{"preset": "blocked"}, tracker.Viewer{Handle: "ana"}, wednesday, berlin)
	if err != nil {
		t.Fatalf("ExpandedQuery: %v", err)
	}
	if blocked.Blocked == nil || !*blocked.Blocked {
		t.Fatalf("blocked resolved to %v", blocked.Blocked)
	}

	// OVERDUE IS THE ALIAS, not a second predicate: `due=overdue` carries
	// its own open condition, and writing it again would be two spellings
	// of one question that drift apart.
	overdue, err := r.reader.ExpandedQuery(t.Context(),
		map[string]any{"preset": "overdue"}, tracker.Viewer{Handle: "ana"}, wednesday, berlin)
	if err != nil {
		t.Fatalf("ExpandedQuery: %v", err)
	}
	if filter, held := overdue.Dates["due"]; !held || !filter.Overdue {
		t.Fatalf("overdue resolved to %v, want the due alias", overdue.Dates)
	}

	if _, err := r.reader.ExpandedQuery(t.Context(),
		map[string]any{"preset": "whatever"}, tracker.Viewer{Handle: "ana"}, wednesday, berlin); err == nil {
		t.Fatal("a preset that is not one was expanded")
	}
}

// A VIEW OVERRIDES A PRESET, and the caller overrides both.
//
// A view is the more specific thing — somebody saved it — and what was typed
// is more specific still.
func TestAViewBeatsAPresetAndTheCallerBeatsBoth(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	if _, err := r.writer.WriteView(t.Context(), "op-view", tracker.View{
		ID:        "v-1",
		Name:      "Bob's",
		Type:      tracker.ViewList,
		Container: tracker.Container{Kind: tracker.ContainerProject, ID: "ENG"},
		Params:    map[string]string{"assignee": "bob"},
	}); err != nil {
		t.Fatalf("WriteView: %v", err)
	}
	r.drain()

	q, err := r.reader.ExpandedQuery(t.Context(), map[string]any{
		"container": "project:ENG", "preset": "my_queue", "view": "v-1",
	}, tracker.Viewer{Handle: "ana"}, wednesday, berlin)
	if err != nil {
		t.Fatalf("ExpandedQuery: %v", err)
	}
	if len(q.Assignee) != 1 || q.Assignee[0] != "bob" {
		t.Fatalf("the assignee is %v, want the view's over the preset's",
			q.Assignee)
	}
	// AND THE PRESET'S OTHER KEYS SURVIVE, because a view overrides the
	// keys it names and not the ones it does not.
	if len(q.StatusGroups) != 2 {
		t.Fatalf("the preset's groups are %v, want both — a view overriding "+
			"one key must not drop the rest", q.StatusGroups)
	}

	explicit, err := r.reader.ExpandedQuery(t.Context(), map[string]any{
		"container": "project:ENG", "preset": "my_queue", "view": "v-1",
		"assignee": "cleo",
	}, tracker.Viewer{Handle: "ana"}, wednesday, berlin)
	if err != nil {
		t.Fatalf("ExpandedQuery: %v", err)
	}
	if len(explicit.Assignee) != 1 || explicit.Assignee[0] != "cleo" {
		t.Fatalf("the assignee is %v, want what was typed", explicit.Assignee)
	}
}

// A SAVED VIEW MAY NOT CARRY A KEY ABOUT THE CALLER'S OWN READ.
//
// `view` would expand into itself, `cursor` would resume a page nobody asked
// for, and a stored `read_level` would let a board silently downgrade a seat's
// own read.
func TestASavedViewCannotCarryTheCallersOwnKeys(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	for _, key := range []string{"view", "preset", "cursor", "read_level",
		"max_lag_seconds"} {
		_, err := r.writer.WriteView(t.Context(), "op-"+key, tracker.View{
			ID:        "v-" + key,
			Name:      "Bad",
			Type:      tracker.ViewList,
			Container: tracker.Container{Kind: tracker.ContainerProject, ID: "ENG"},
			Params:    map[string]string{key: "x"},
		})
		if err == nil {
			t.Fatalf("a view carrying %q was saved", key)
		}
		if !strings.Contains(err.Error(), "about the CALLER") {
			t.Fatalf("the refusal for %q is %q", key, err)
		}
	}
}

// A VIEW THAT IS NOT ON THIS NODE IS UNAVAILABLE, never an unfiltered list.
func TestAMissingViewIsRefusedRatherThanIgnored(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	_, err := r.reader.ExpandedQuery(t.Context(),
		map[string]any{"container": "project:ENG", "view": "v-gone"},
		tracker.Viewer{}, wednesday, berlin)
	if err == nil {
		t.Fatal("a view nobody saved expanded to nothing and answered the " +
			"whole board")
	}
	if !strings.Contains(err.Error(), "not on this node") {
		t.Fatalf("the refusal %q does not say what happened", err)
	}
}

// AN ANSWER SAYS WHAT IT WAS EXPANDED FROM.
//
// A request knows what it sent; an answer does not otherwise, and these travel
// detached from their requests — a socket frame, a cached payload, a screen
// restored from a URL. Without the echo a board cannot say which saved view it
// is showing, and a caller cannot tell an expansion that resolved from one
// that was quietly dropped.
func TestAnAnswerSaysWhatItWasExpandedFrom(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	if _, err := r.writer.WriteView(t.Context(), "op-view", tracker.View{
		ID:        "v-1",
		Name:      "Open work",
		Type:      tracker.ViewList,
		Container: tracker.Container{Kind: tracker.ContainerProject, ID: "ENG"},
		Params:    map[string]string{"status": "todo"},
	}); err != nil {
		t.Fatalf("WriteView: %v", err)
	}
	r.drain()

	q, err := r.reader.ExpandedQuery(t.Context(), map[string]any{
		"container": "project:ENG", "view": "v-1", "preset": "blocked",
	}, tracker.Viewer{Handle: "ana"}, wednesday, berlin)
	if err != nil {
		t.Fatalf("ExpandedQuery: %v", err)
	}
	q.Level = "stale"
	answer, err := r.reader.Tasks(t.Context(), q, wednesday)
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	if answer.View != "v-1" {
		t.Fatalf("the answer says it came from view %q, want v-1", answer.View)
	}
	if answer.Preset != "blocked" {
		t.Fatalf("the answer says it came from preset %q, want blocked",
			answer.Preset)
	}

	// AND AN ORDINARY QUERY CLAIMS NEITHER, or every board would report a
	// view it was never opened from.
	plain := r.ask(map[string]any{"container": "project:ENG"})
	if plain.View != "" || plain.Preset != "" {
		t.Fatalf("an ordinary answer claims view %q and preset %q",
			plain.View, plain.Preset)
	}
}

// THE TWO LISTS A PERSON HAS that the rows alone cannot express.
//
// `priorities` is the ORDER somebody arranged, which lives on their own
// object and has no column to sort by; `triage` is the work nobody has picked
// up, which with one fixed status set is the only honest definition of
// "needs somebody to decide" — a company cannot declare an intake status.
func TestThePersonalPresets(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for _, id := range []string{"a", "b", "c"} {
		inSprint(t, r, id, nil)
	}
	mine := newTask("mine")
	mine.Assignee = "ana"
	if _, err := r.writer.CreateTask(t.Context(), "op-mine", mine, nil); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	r.drain()
	// DELIBERATELY NOT ALPHABETICAL and not creation order, so an answer
	// sorted by either is visibly wrong.
	if _, err := r.writer.WritePriorities(t.Context(), "op-prio", "ana",
		[]string{"c", "a"}, tracker.PersonAuthority{}); err != nil {
		t.Fatalf("WritePriorities: %v", err)
	}
	r.drain()

	q, err := r.reader.ExpandedQuery(t.Context(),
		map[string]any{"container": "project:ENG", "preset": "priorities"},
		tracker.Viewer{Handle: "ana", Project: "ENG"}, wednesday, berlin)
	if err != nil {
		t.Fatalf("ExpandedQuery: %v", err)
	}
	q.Level = statelog.ReadStale
	answer, err := r.reader.Tasks(t.Context(), q, wednesday)
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	got := ids(answer)
	if len(got) != 2 || got[0] != "c" || got[1] != "a" {
		t.Fatalf("preset=priorities answers %v, want [c a] — the order of the "+
			"list IS the answer, and there is no column to sort by", got)
	}

	// TRIAGE IS THE UNASSIGNED OPEN WORK, so the one task somebody holds
	// is out of it.
	triage, err := r.reader.ExpandedQuery(t.Context(),
		map[string]any{"container": "project:ENG", "preset": "triage"},
		tracker.Viewer{Handle: "ana", Project: "ENG"}, wednesday, berlin)
	if err != nil {
		t.Fatalf("ExpandedQuery: %v", err)
	}
	triage.Level = statelog.ReadStale
	unclaimed, err := r.reader.Tasks(t.Context(), triage, wednesday)
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	for _, row := range unclaimed.Rows {
		if row.Assignee != "" {
			t.Errorf("preset=triage answered %s, which %s holds — triage is "+
				"what nobody has picked up", row.ID, row.Assignee)
		}
	}
	if len(unclaimed.Rows) != 3 {
		t.Fatalf("preset=triage answers %d tasks, want the three nobody holds",
			len(unclaimed.Rows))
	}

	// AND `priorities` NEEDS A VIEWER, exactly as `my_queue` does: a list
	// with nobody's name on it is nobody's list.
	if _, err := r.reader.ExpandedQuery(t.Context(),
		map[string]any{"preset": "priorities"}, tracker.Viewer{},
		wednesday, berlin); err == nil {
		t.Error("preset=priorities was answered for nobody")
	}
}
