package worklet_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"

	"github.com/crewlet/crewlet/internal/worklet"
)

// A VIEW IS A SET OF DEFAULTS, never a lock: it is loaded first and explicit
// keys override it, and opening a saved board never answers the unfiltered
// list.
func TestASavedViewIsDefaultsTheCallerCanOverride(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	if _, err := save(t, r.writer, "op-view", worklet.View{
		ID:        "v-mine",
		Name:      "Ana's todo",
		Type:      worklet.ViewList,
		Container: worklet.Container{Kind: worklet.ContainerProject, ID: "ENG"},
		Params: map[string]string{
			"assignee": "ana", "status": "todo", "sort": "-updated",
		},
	}); err != nil {
		t.Fatalf("WriteView: %v", err)
	}
	r.drain()

	q, err := r.reader.ExpandedQuery(t.Context(),
		map[string]any{"container": "project:ENG", "view": "v-mine"},
		worklet.Viewer{}, wednesday, berlin)
	if err != nil {
		t.Fatalf("ExpandedQuery: %v", err)
	}
	switch {
	case len(q.Assignee) != 1 || q.Assignee[0] != "ana":
		t.Fatalf("the view's assignee reached the query as %v", q.Assignee)
	case len(q.Status) != 1 || q.Status[0] != worklet.StatusTodo:
		t.Fatalf("the view's status reached the query as %v", q.Status)
	case len(q.Sort) != 1 || q.Sort[0].Key != "updated" || !q.Sort[0].Descending:
		t.Fatalf("the view's sort reached the query as %v", q.Sort)
	}

	// AN EXPLICIT KEY WINS, and only that key changes.
	q, err = r.reader.ExpandedQuery(t.Context(), map[string]any{
		"container": "project:ENG", "view": "v-mine", "assignee": "bob",
	}, worklet.Viewer{}, wednesday, berlin)
	if err != nil {
		t.Fatalf("ExpandedQuery: %v", err)
	}
	if len(q.Assignee) != 1 || q.Assignee[0] != "bob" {
		t.Fatalf("the explicit assignee is %v, want bob — a view that could "+
			"not be overridden is a lock", q.Assignee)
	}
	if len(q.Status) != 1 || q.Status[0] != worklet.StatusTodo {
		t.Fatalf("overriding one key dropped the view's others: %v", q.Status)
	}
}

// A PRESET IS THE QUESTION ITS INDEX IS NAMED FOR.
func TestAPresetExpandsToTheQuestionItNames(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	// MY QUEUE NEEDS A VIEWER and the viewer's own PROJECT: the preset
	// asks two things about the reader.
	q, err := r.reader.ExpandedQuery(t.Context(),
		map[string]any{"container": "project:ENG", "preset": "my_queue"},
		worklet.Viewer{Handle: "ana", Project: "ENG"}, wednesday, berlin)
	if err != nil {
		t.Fatalf("ExpandedQuery: %v", err)
	}
	// "WHAT CAN I PICK UP" IS A DISJUNCTION: the work this person holds,
	// and the work in their own project nobody holds. `assignee=me` alone
	// would show an empty queue over an unclaimed backlog.
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
	// BLOCKED WORK IS NOT SOMETHING TO PICK UP: it is `preset=blocked`'s
	// answer.
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

	// A VIEWER WITH NO PROJECT OF THEIR OWN PICKS UP WHAT THEY HOLD: an
	// unscoped unclaimed arm would offer every unassigned task in the
	// company.
	homeless, err := r.reader.ExpandedQuery(t.Context(),
		map[string]any{"container": "workspace", "preset": "my_queue"},
		worklet.Viewer{Handle: "jane.doe"}, wednesday, berlin)
	if err != nil {
		t.Fatalf("ExpandedQuery with no project: %v", err)
	}
	if len(homeless.Any) != 0 {
		t.Fatalf("my_queue for a viewer with no project has %d branches: %+v "+
			"— an unscoped unclaimed arm is every unassigned task in the company",
			len(homeless.Any), homeless.Any)
	}
	if len(homeless.Assignee) != 1 || homeless.Assignee[0] != "jane.doe" {
		t.Fatalf("my_queue for a viewer with no project asks for %v, want "+
			"what they hold", homeless.Assignee)
	}

	// A QUEUE WITH NOBODY'S NAME ON IT IS EVERY OPEN TASK, which is the
	// widest possible reading of "mine" — so it is refused.
	if _, err := r.reader.ExpandedQuery(t.Context(),
		map[string]any{"preset": "my_queue"}, worklet.Viewer{}, wednesday, berlin); err == nil {
		t.Fatal("my_queue was answered for nobody")
	}

	blocked, err := r.reader.ExpandedQuery(t.Context(),
		map[string]any{"preset": "blocked"}, worklet.Viewer{Handle: "ana"}, wednesday, berlin)
	if err != nil {
		t.Fatalf("ExpandedQuery: %v", err)
	}
	if blocked.Blocked == nil || !*blocked.Blocked {
		t.Fatalf("blocked resolved to %v", blocked.Blocked)
	}

	// OVERDUE IS THE ALIAS `due=overdue`, not a second predicate that
	// could drift from it.
	overdue, err := r.reader.ExpandedQuery(t.Context(),
		map[string]any{"preset": "overdue"}, worklet.Viewer{Handle: "ana"}, wednesday, berlin)
	if err != nil {
		t.Fatalf("ExpandedQuery: %v", err)
	}
	if filter, held := overdue.Dates["due"]; !held || !filter.Overdue {
		t.Fatalf("overdue resolved to %v, want the due alias", overdue.Dates)
	}

	if _, err := r.reader.ExpandedQuery(t.Context(),
		map[string]any{"preset": "whatever"}, worklet.Viewer{Handle: "ana"}, wednesday, berlin); err == nil {
		t.Fatal("a preset that is not one was expanded")
	}
}

// A VIEW OVERRIDES A PRESET, and the caller overrides both: each is more
// specific than the last.
func TestAViewBeatsAPresetAndTheCallerBeatsBoth(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	if _, err := save(t, r.writer, "op-view", worklet.View{
		ID:        "v-1",
		Name:      "Bob's",
		Type:      worklet.ViewList,
		Container: worklet.Container{Kind: worklet.ContainerProject, ID: "ENG"},
		Params:    map[string]string{"assignee": "bob"},
	}); err != nil {
		t.Fatalf("WriteView: %v", err)
	}
	r.drain()

	q, err := r.reader.ExpandedQuery(t.Context(), map[string]any{
		"container": "project:ENG", "preset": "my_queue", "view": "v-1",
	}, worklet.Viewer{Handle: "ana"}, wednesday, berlin)
	if err != nil {
		t.Fatalf("ExpandedQuery: %v", err)
	}
	if len(q.Assignee) != 1 || q.Assignee[0] != "bob" {
		t.Fatalf("the assignee is %v, want the view's over the preset's",
			q.Assignee)
	}
	// THE PRESET'S OTHER KEYS SURVIVE: a view overrides only the keys it
	// names.
	if len(q.StatusGroups) != 2 {
		t.Fatalf("the preset's groups are %v, want both — a view overriding "+
			"one key must not drop the rest", q.StatusGroups)
	}

	explicit, err := r.reader.ExpandedQuery(t.Context(), map[string]any{
		"container": "project:ENG", "preset": "my_queue", "view": "v-1",
		"assignee": "cleo",
	}, worklet.Viewer{Handle: "ana"}, wednesday, berlin)
	if err != nil {
		t.Fatalf("ExpandedQuery: %v", err)
	}
	if len(explicit.Assignee) != 1 || explicit.Assignee[0] != "cleo" {
		t.Fatalf("the assignee is %v, want what was typed", explicit.Assignee)
	}
}

// A SAVED VIEW MAY NOT CARRY A KEY ABOUT THE CALLER'S OWN READ: `view` would
// expand into itself, `cursor` resume a page nobody asked for, and a stored
// `read_level` silently downgrade a seat's own read.
func TestASavedViewCannotCarryTheCallersOwnKeys(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	for _, key := range []string{"view", "preset", "cursor", "read_level",
		"max_lag_seconds", "max_lag_seq", "min_position"} {
		_, err := save(t, r.writer, "op-"+key, worklet.View{
			ID:        "v-" + key,
			Name:      "Bad",
			Type:      worklet.ViewList,
			Container: worklet.Container{Kind: worklet.ContainerProject, ID: "ENG"},
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
		worklet.Viewer{}, wednesday, berlin)
	if err == nil {
		t.Fatal("a view nobody saved expanded to nothing and answered the " +
			"whole board")
	}
	if !strings.Contains(err.Error(), "not on this node") {
		t.Fatalf("the refusal %q does not say what happened", err)
	}
}

// AN ANSWER SAYS WHAT IT WAS EXPANDED FROM: answers travel detached from
// their requests (a socket frame, a cached payload, a URL), and without the
// echo a board cannot say which view it shows, nor a caller tell a resolved
// expansion from a dropped one.
func TestAnAnswerSaysWhatItWasExpandedFrom(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	if _, err := save(t, r.writer, "op-view", worklet.View{
		ID:        "v-1",
		Name:      "Open work",
		Type:      worklet.ViewList,
		Container: worklet.Container{Kind: worklet.ContainerProject, ID: "ENG"},
		Params:    map[string]string{"status": "todo"},
	}); err != nil {
		t.Fatalf("WriteView: %v", err)
	}
	r.drain()

	q, err := r.reader.ExpandedQuery(t.Context(), map[string]any{
		"container": "project:ENG", "view": "v-1", "preset": "blocked",
	}, worklet.Viewer{Handle: "ana"}, wednesday, berlin)
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

	// AN ORDINARY QUERY CLAIMS NEITHER.
	plain := r.ask(map[string]any{"container": "project:ENG"})
	if plain.View != "" || plain.Preset != "" {
		t.Fatalf("an ordinary answer claims view %q and preset %q",
			plain.View, plain.Preset)
	}
}

// THE TWO LISTS A PERSON HAS that the rows alone cannot express: `priorities`
// is the order somebody arranged, on their own object with no column to sort
// by; `triage` is the work nobody has picked up, the only honest "needs
// somebody to decide" while a company cannot declare an intake status.
func TestThePersonalPresets(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for _, id := range []string{"a", "b", "c"} {
		filedTask(t, r, id)
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
		[]string{"c", "a"}, nil, worklet.PersonAuthority{}); err != nil {
		t.Fatalf("WritePriorities: %v", err)
	}
	r.drain()

	q, err := r.reader.ExpandedQuery(t.Context(),
		map[string]any{"container": "project:ENG", "preset": "priorities"},
		worklet.Viewer{Handle: "ana", Project: "ENG"}, wednesday, berlin)
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

	// TRIAGE IS THE UNASSIGNED OPEN WORK.
	triage, err := r.reader.ExpandedQuery(t.Context(),
		map[string]any{"container": "project:ENG", "preset": "triage"},
		worklet.Viewer{Handle: "ana", Project: "ENG"}, wednesday, berlin)
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

	// `priorities` NEEDS A VIEWER, as `my_queue` does.
	if _, err := r.reader.ExpandedQuery(t.Context(),
		map[string]any{"preset": "priorities"}, worklet.Viewer{},
		wednesday, berlin); err == nil {
		t.Error("preset=priorities was answered for nobody")
	}
}
