package tracker_test

import (
	"strings"
	"testing"

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
		"", wednesday, berlin)
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
	}, "", wednesday, berlin)
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

	// MY QUEUE NEEDS A VIEWER, and the surface supplies it.
	q, err := r.reader.ExpandedQuery(t.Context(),
		map[string]any{"container": "project:ENG", "preset": "my_queue"},
		"ana", wednesday, berlin)
	if err != nil {
		t.Fatalf("ExpandedQuery: %v", err)
	}
	if len(q.Assignee) != 1 || q.Assignee[0] != "ana" {
		t.Fatalf("my_queue resolved to %v, want the viewer's own name",
			q.Assignee)
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
		map[string]any{"preset": "my_queue"}, "", wednesday, berlin); err == nil {
		t.Fatal("my_queue was answered for nobody")
	}

	blocked, err := r.reader.ExpandedQuery(t.Context(),
		map[string]any{"preset": "blocked"}, "ana", wednesday, berlin)
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
		map[string]any{"preset": "overdue"}, "ana", wednesday, berlin)
	if err != nil {
		t.Fatalf("ExpandedQuery: %v", err)
	}
	if filter, held := overdue.Dates["due"]; !held || !filter.Overdue {
		t.Fatalf("overdue resolved to %v, want the due alias", overdue.Dates)
	}

	if _, err := r.reader.ExpandedQuery(t.Context(),
		map[string]any{"preset": "whatever"}, "ana", wednesday, berlin); err == nil {
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
	}, "ana", wednesday, berlin)
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
	}, "ana", wednesday, berlin)
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
		"", wednesday, berlin)
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
	}, "ana", wednesday, berlin)
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
