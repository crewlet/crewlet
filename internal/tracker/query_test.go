package tracker_test

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

func parse(t *testing.T, kv map[string]any) (tracker.Query, error) {
	t.Helper()
	return tracker.ParseQuery(tracker.MapParams(kv), wednesday, berlin)
}

func mustParse(t *testing.T, kv map[string]any) tracker.Query {
	t.Helper()
	q, err := parse(t, kv)
	if err != nil {
		t.Fatalf("ParseQuery(%v): %v", kv, err)
	}
	return q
}

// AN ABSENT CONTAINER IS NEITHER THE WORKSPACE NOR A PROJECT.
//
// Defaulting to the workspace would make an omitted key the most expensive
// query in the system; defaulting to a project would answer about one the
// caller never named. The surface resolves it, and it can only do that if the
// parser reports honestly that nothing was said.
func TestAnAbsentContainerIsNotTheWorkspace(t *testing.T) {
	t.Parallel()
	q := mustParse(t, map[string]any{})
	if q.Scope.Workspace || q.Scope.Project != "" {
		t.Fatalf("an absent container parsed as %+v", q.Scope)
	}
	if got := mustParse(t, map[string]any{"container": "workspace"}); !got.Scope.Workspace {
		t.Error("an explicit workspace did not parse as one")
	}
	// AND THE KEY IS UPPER-CASED, both forms. The scope is an EXACT
	// compare against `project_key`, which is stored upper — so a board
	// asked for the same project in the case somebody typed would answer
	// an empty list rather than a refusal, and an empty board is a thing
	// a person acts on.
	for _, form := range []string{"project:ENG", "ENG", "project:eng", "eng"} {
		if got := mustParse(t, map[string]any{"container": form}); got.Scope.Project != "ENG" {
			t.Errorf("%q parsed as project %q", form, got.Scope.Project)
		}
	}
}

// A STATUS OUTSIDE THE SIX IS REFUSED, AND NEGATION IS ITS OWN LIST.
//
// The set is fixed, so a slug nobody declared can only be a typo — and a
// filter that silently dropped it would answer over every status, which reads
// as a filter that found more than it should have.
func TestAStatusOutsideTheSixIsRefused(t *testing.T) {
	t.Parallel()
	q := mustParse(t, map[string]any{"status": "todo,!closed,in_review"})
	if len(q.Status) != 2 || q.Status[0] != tracker.StatusTodo {
		t.Fatalf("the positive statuses parsed as %v", q.Status)
	}
	if len(q.StatusNot) != 1 || q.StatusNot[0] != tracker.StatusClosed {
		t.Fatalf("the negated statuses parsed as %v", q.StatusNot)
	}
	if _, err := parse(t, map[string]any{"status": "blocked"}); err == nil {
		t.Error("a seventh status was accepted")
	}
	// And there is no `cancelled=` key, because cancelled IS a status.
	if _, err := parse(t, map[string]any{"status": "cancelled"}); err != nil {
		t.Errorf("cancelled was refused as a status: %v", err)
	}
}

// A TAG MODE IS ONE OF THREE, AND THE DEFAULT IS `any`.
func TestTheTagModesAreThreeAndDefaultToAny(t *testing.T) {
	t.Parallel()
	q := mustParse(t, map[string]any{"tag": "api,v2"})
	if q.Tags.Mode != tracker.TagAny || len(q.Tags.Tags) != 2 {
		t.Fatalf("a bare tag list parsed as %+v", q.Tags)
	}
	if got := mustParse(t, map[string]any{"tag": "all:api,v2"}); got.Tags.Mode != tracker.TagAll {
		t.Errorf("all: parsed as %q", got.Tags.Mode)
	}
	if _, err := parse(t, map[string]any{"tag": "most:api"}); err == nil {
		t.Error("a fourth tag mode was accepted")
	}
}

// A DATE FILTER RESOLVES AT PARSE TIME, AGAINST THE COMPANY'S CLOCK.
//
// What reaches the store is two numbers. A query that carried the token would
// resolve it per row, and two nodes in two datacentres would resolve it
// differently.
func TestADateFilterIsResolvedRatherThanCarried(t *testing.T) {
	t.Parallel()
	q := mustParse(t, map[string]any{"due": "thisweek", "created": "lt:today"})
	due, held := q.Dates["due"]
	if !held {
		t.Fatal("the due filter did not parse")
	}
	if due.Op != tracker.DateRange || due.From.At.IsZero() || due.To.At.IsZero() {
		t.Fatalf("thisweek parsed as %+v", due)
	}
	if got := q.Dates["created"]; got.Op != tracker.DateLT {
		t.Fatalf("lt:today parsed as %+v", got)
	}
	if _, err := parse(t, map[string]any{"due": "soon"}); err == nil {
		t.Error("an unresolvable date was accepted")
	}
}

// A CUSTOM-FIELD PREDICATE IS COLLECTED, NOT RESOLVED.
//
// Which ops a field admits depends on its declared type, and the declaration
// lives in a catalogue this parser does not read. A parser that read one could
// fail on a store — and a query that cannot be parsed without I/O cannot be
// parsed inside the transaction that answers it.
func TestCustomFieldsAreCollectedInAStableOrder(t *testing.T) {
	t.Parallel()
	q := mustParse(t, map[string]any{
		"f.severity": "any:s1,s2",
		"f.area":     "platform",
		"status":     "todo",
	})
	if len(q.Fields) != 2 {
		t.Fatalf("the field filters parsed as %+v", q.Fields)
	}
	if q.Fields[0].Ref != "area" || q.Fields[1].Ref != "severity" {
		t.Fatalf("the field filters are not in a stable order: %+v", q.Fields)
	}
	if q.Fields[0].Op != "eq" || q.Fields[0].Value != "platform" {
		t.Errorf("a bare value did not default to eq: %+v", q.Fields[0])
	}
	if q.Fields[1].Op != "any" || q.Fields[1].Value != "s1,s2" {
		t.Errorf("an explicit op did not survive: %+v", q.Fields[1])
	}
}

// A MODE WITHOUT TEXT IS REFUSED NAMING THE MISSING KEY.
//
// Ignoring it answers an unfiltered list, which reads as a search that matched
// everything — the one wrong answer a caller cannot tell from a right one.
// THERE IS NO SEARCH MODE ON THIS GRAMMAR, and `mode` is refused as the
// unknown key it now is.
//
// `q` here is a substring of a key or a title — the item somebody half
// remembers. Ranked search over the company's prose is `search_knowledge`'s,
// behind the knowledge seam, which is where the analyzer, the inverted list
// and the vectors are; `kb_docs` and `kb_postings` index PAGES and nothing has
// ever put a task in them. Three modes over one behaviour is a knob whose
// values cannot differ, and a caller that asked for `semantic` and got a
// substring match was answered by a name rather than by a search.
func TestThereIsNoSearchModeOnTheTaskGrammar(t *testing.T) {
	t.Parallel()
	_, err := parse(t, map[string]any{"q": "auth", "mode": "semantic"})
	if err == nil {
		t.Fatal("a search mode was accepted on a grammar with no ranker")
	}
	if !strings.Contains(err.Error(), "not a query parameter") {
		t.Fatalf("the refusal is %q and does not say the key is unknown", err)
	}
	if got := mustParse(t, map[string]any{"q": "auth"}); got.Text != "auth" {
		t.Errorf("the find text parsed as %q", got.Text)
	}
}

// THE DISJUNCTION IS ONE LEVEL, CAPPED, AND CARRIES NO ANSWER KEYS.
//
// A branch with its own limit, cursor, sort or grouping is a second query
// pretending to be a predicate — and an N-way OR across indexes is the one
// shape the planner handles worst, which is what the cap is for.
func TestTheDisjunctionIsOneLevelAndCapped(t *testing.T) {
	t.Parallel()
	q := mustParse(t, map[string]any{
		"any": `[{"assignee":"me"},{"assignee":"none","container":"project:ENG"}]`,
	})
	if len(q.Any) != 2 {
		t.Fatalf("the disjunction parsed as %d branch(es)", len(q.Any))
	}
	if q.Any[1].Scope.Project != "ENG" {
		t.Errorf("a branch's own container did not parse: %+v", q.Any[1].Scope)
	}

	nested := `[{"any":"[]"}]`
	if _, err := parse(t, map[string]any{"any": nested}); err == nil {
		t.Error("a nested disjunction was accepted")
	}
	for _, key := range []string{"limit", "cursor", "sort", "group_by", "totals"} {
		branch := `[{"` + key + `":"1"}]`
		if _, err := parse(t, map[string]any{"any": branch}); err == nil {
			t.Errorf("a branch carrying %q was accepted; it is about the answer "+
				"rather than about the rows", key)
		}
	}

	var wide strings.Builder
	wide.WriteString("[")
	for i := range tracker.MaxAnyBranches + 1 {
		if i > 0 {
			wide.WriteString(",")
		}
		wide.WriteString(`{"assignee":"me"}`)
	}
	wide.WriteString("]")
	if _, err := parse(t, map[string]any{"any": wide.String()}); err == nil {
		t.Errorf("a disjunction of %d branches was accepted against a cap of %d",
			tracker.MaxAnyBranches+1, tracker.MaxAnyBranches)
	}
}

// A STALENESS BOUND AT A LEVEL THAT IS NOT STALE IS REFUSED.
//
// It is not a level of its own and it does not narrow anything at the other
// three: passing it there is a caller who believes they asked for something
// they did not.
func TestAStalenessBoundBelongsToTheStaleLevelAlone(t *testing.T) {
	t.Parallel()
	q := mustParse(t, map[string]any{"read_level": "stale", "max_lag_seconds": "30"})
	if q.Level != statelog.ReadStale {
		t.Fatalf("read_level parsed as %q", q.Level)
	}
	for _, level := range []string{"linearizable", "session", "consistent_prefix"} {
		_, err := parse(t, map[string]any{"read_level": level, "max_lag_seq": "10"})
		if err == nil {
			t.Errorf("a staleness bound was accepted at read_level=%s", level)
		}
	}
	if _, err := parse(t, map[string]any{"read_level": "eventually"}); err == nil {
		t.Error("a fifth read level was accepted")
	}
}

// ABSENT IS NOT A FOURTH READ LEVEL.
//
// It resolves to the SURFACE's own default — a seat tool linearizable, a
// dashboard poll stale — which is what makes the default a property of the
// surface rather than of the model that happened to omit the key.
func TestAnAbsentReadLevelIsTheSurfacesToResolve(t *testing.T) {
	t.Parallel()
	if got := mustParse(t, map[string]any{}).Level; got != "" {
		t.Fatalf("an absent read level parsed as %q rather than leaving the "+
			"choice to the surface", got)
	}
}

// A SECONDARY GROUPING IS A SPLIT OF THE FIRST, NOT A SECOND QUESTION.
func TestTheSecondaryGroupingRefusesItsThreeMistakes(t *testing.T) {
	t.Parallel()
	if _, err := parse(t, map[string]any{"group_by2": "assignee"}); err == nil {
		t.Error("a secondary grouping with no primary was accepted")
	}
	if _, err := parse(t, map[string]any{"group_by": "tag", "group_by2": "tag"}); err == nil {
		t.Error("a secondary grouping equal to the first was accepted; every " +
			"row would be alone in its own subgroup")
	}
	if _, err := parse(t, map[string]any{
		"container": "project:ENG", "group_by": "project",
	}); err == nil {
		t.Error("group_by=project inside one project was accepted")
	}
	if got := mustParse(t, map[string]any{
		"container": "workspace", "group_by": "project",
	}); got.GroupBy != "project" {
		t.Errorf("group_by=project at the workspace level parsed as %q", got.GroupBy)
	}
}

// A SORT KEY OUTSIDE THE GRAMMAR IS REFUSED, AND `-` IS DESCENDING.
func TestSortParsesItsDirectionAndRefusesTheRest(t *testing.T) {
	t.Parallel()
	q := mustParse(t, map[string]any{"sort": "-updated,due,f.severity"})
	if len(q.Sort) != 3 {
		t.Fatalf("the sort parsed as %+v", q.Sort)
	}
	if !q.Sort[0].Descending || q.Sort[0].Key != "updated" {
		t.Errorf("-updated parsed as %+v", q.Sort[0])
	}
	if q.Sort[1].Descending {
		t.Errorf("due parsed as descending")
	}
	if _, err := parse(t, map[string]any{"sort": "randomly"}); err == nil {
		t.Error("a sort key outside the grammar was accepted")
	}
}

// SHOW_CLOSED TAKES THREE FORMS AND `recent:` APPLIES TO THE WHOLE FINISHED
// SET.
//
// Because the finish stamp is by GROUP, a task cancelled inside the window is
// in the answer exactly as one done inside it is — which is what makes a
// board's Cancelled column non-empty without a second field.
func TestShowClosedTakesItsThreeForms(t *testing.T) {
	t.Parallel()
	if got := mustParse(t, map[string]any{}).ShowClosed; got.All || got.Recent != 0 {
		t.Fatalf("the default show_closed is %+v", got)
	}
	if got := mustParse(t, map[string]any{"show_closed": "true"}).ShowClosed; !got.All {
		t.Error("show_closed=true did not parse")
	}
	got := mustParse(t, map[string]any{"show_closed": "recent:336h"}).ShowClosed
	if got.Recent != 336*time.Hour {
		t.Errorf("recent: parsed as %v", got.Recent)
	}
	for _, bad := range []string{"recent:", "recent:-1h", "maybe"} {
		if _, err := parse(t, map[string]any{"show_closed": bad}); err == nil {
			t.Errorf("show_closed=%q was accepted", bad)
		}
	}
}

// A BOOLEAN FILTER TELLS ABSENT FROM FALSE.
//
// `blocked=false` asks for tasks with nothing blocking them; an absent key
// asks for all of them. A plain bool cannot say which, which is why every one
// of these is a pointer.
func TestABooleanFilterTellsAbsentFromFalse(t *testing.T) {
	t.Parallel()
	if got := mustParse(t, map[string]any{}).Blocked; got != nil {
		t.Fatalf("an absent blocked filter parsed as %v", *got)
	}
	got := mustParse(t, map[string]any{"blocked": "false"}).Blocked
	if got == nil {
		t.Fatal("blocked=false parsed as absent, so it would not filter at all")
	}
	if *got {
		t.Error("blocked=false parsed as true")
	}
}
