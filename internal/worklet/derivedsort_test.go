package worklet_test

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// applyNames is a chart apply of the seats' names, handle to name, at the n-th
// activation, applied before it returns ([worklet.Writer.ApplyRoster]).
func applyNames(t *testing.T, r *roundTrip, n int, names map[string]string) {
	t.Helper()
	if _, err := r.writer.ApplyRoster(t.Context(),
		wednesday.Add(time.Duration(n)*time.Minute), names); err != nil {
		t.Fatalf("ApplyRoster: %v", err)
	}
	r.drain()
}

// filed creates one task with the edits a case makes to it, and applies it.
func filed(t *testing.T, r *roundTrip, id string, edit func(*worklet.Task)) {
	t.Helper()
	task := newTask(id)
	task.Key = "ENG-" + id
	edit(&task)
	if _, err := r.writer.CreateTask(t.Context(), "op-"+id, task, nil); err != nil {
		t.Fatalf("CreateTask %s: %v", id, err)
	}
	r.drain()
}

// inStatus files a task in a status, under the group that status belongs to.
func inStatus(status worklet.Status) func(*worklet.Task) {
	return func(task *worklet.Task) {
		task.Status, task.StatusGroup = status, status.Group()
	}
}

// askAs is [roundTrip.ask], its error returned rather than failed on.
func askAs(t *testing.T, r *roundTrip, kv map[string]any) (worklet.Answer, error) {
	t.Helper()
	q, err := worklet.ParseQuery(worklet.MapParams(kv), wednesday, berlin)
	if err != nil {
		t.Fatalf("ParseQuery: %v", err)
	}
	q.Level = statelog.ReadStale
	return r.reader.Tasks(t.Context(), q, wednesday)
}

// orderOf is every id an order answers, first page to last, read limit rows
// at a time through the cursor each page mints.
func orderOf(t *testing.T, r *roundTrip, kv map[string]any, limit int) []string {
	t.Helper()
	params := map[string]any{"limit": fmt.Sprint(limit)}
	for key, value := range kv {
		params[key] = value
	}
	var out []string
	for range 50 {
		answer, err := askAs(t, r, params)
		if err != nil {
			t.Fatalf("Tasks %v: %v", params, err)
		}
		out = append(out, ids(answer)...)
		if answer.NextCursor == "" {
			return out
		}
		params["cursor"] = answer.NextCursor
	}
	t.Fatalf("%v never reached its last page", kv)
	return nil
}

// holdsOrder checks an order both whole and paged across every boundary it has:
// a page of one row puts a boundary between every pair of neighbours, ties
// included, and a page of two puts them between every other pair.
func holdsOrder(t *testing.T, r *roundTrip, kv map[string]any, want []string) {
	t.Helper()
	for _, limit := range []int{50, 2, 1} {
		if got := orderOf(t, r, kv, limit); !slices.Equal(got, want) {
			t.Errorf("%v in pages of %d answers %v, want %v", kv, limit, got, want)
		}
	}
}

// A KEY IS SORTED AS IT READS, its number as a number: ENG-2 comes before
// ENG-10, which a sort over the text puts the other way round, and every key
// of one project comes before the next project's.
//
// THE KEYS ARE MINTED, in the order the tasks are filed: each create takes the
// next number of its project's counter, whatever key the task arrived with.
func TestSortingByKeyReadsItsNumberAsANumber(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	if _, err := r.writer.WriteDocument(t.Context(), "op-ops", worklet.ProjectSubject("OPS"),
		"", worklet.Project{V: 1, Key: "OPS", Name: "Operations",
			CreatedAt: wednesday, UpdatedAt: wednesday},
		worklet.ChangeProjectCreated, nil); err != nil {
		t.Fatalf("seed a second project: %v", err)
	}
	r.drain()
	// The ids sort as text, so neither they nor the keys' text are the order.
	var ascending, eng []string
	for n := 1; n <= 12; n++ {
		id := fmt.Sprintf("k-%d", n)
		filed(t, r, id, func(*worklet.Task) {})
		eng = append(eng, id)
		if n == 5 || n == 11 {
			ops := fmt.Sprintf("o-%d", n)
			filed(t, r, ops, func(task *worklet.Task) { task.Project = "OPS" })
		}
	}
	ascending = append(slices.Clone(eng), "o-5", "o-11")

	// THE PREMISE: the keys are ENG-1 to ENG-12 and OPS-1 and OPS-2, in that
	// order, so a sort over the text would put ENG-10 before ENG-2.
	answer := r.ask(map[string]any{"container": "workspace", "sort": "key"})
	var keys []string
	for _, row := range answer.Rows {
		keys = append(keys, row.Key)
	}
	want := []string{"ENG-1", "ENG-2", "ENG-3", "ENG-4", "ENG-5", "ENG-6", "ENG-7",
		"ENG-8", "ENG-9", "ENG-10", "ENG-11", "ENG-12", "OPS-1", "OPS-2"}
	if !slices.Equal(keys, want) {
		t.Fatalf("sort=key answers %v, want %v", keys, want)
	}

	holdsOrder(t, r, map[string]any{"container": "workspace", "sort": "key"}, ascending)
	descending := slices.Clone(ascending)
	slices.Reverse(descending)
	holdsOrder(t, r, map[string]any{"container": "workspace", "sort": "-key"}, descending)
	holdsOrder(t, r, map[string]any{"container": "project:ENG", "sort": "key"}, eng)
}

// A STATUS IS SORTED IN THE ORDER A BOARD DRAWS THE SIX, never by its name:
// alphabetically, Cancelled would lead and To do would follow Done. Tasks in
// one status keep one order between them in both directions, so a page
// boundary between two of them drops neither.
func TestSortingByStatusFollowsTheBoard(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filed(t, r, "d", inStatus(worklet.StatusDone))
	filed(t, r, "a", inStatus(worklet.StatusTodo))
	filed(t, r, "x", inStatus(worklet.StatusClosed))
	filed(t, r, "r", inStatus(worklet.StatusInReview))
	filed(t, r, "c", inStatus(worklet.StatusCancelled))
	filed(t, r, "p", inStatus(worklet.StatusInProgress))
	filed(t, r, "b", inStatus(worklet.StatusTodo))
	all := map[string]any{"container": "project:ENG", "show_closed": "true"}
	with := func(sort string) map[string]any {
		out := map[string]any{"sort": sort}
		for k, v := range all {
			out[k] = v
		}
		return out
	}
	holdsOrder(t, r, with("status"), []string{"a", "b", "p", "r", "d", "c", "x"})
	holdsOrder(t, r, with("-status"), []string{"x", "c", "d", "r", "p", "a", "b"})
}

// A TYPE IS SORTED IN THE COMPANY'S OWN CATALOGUE ORDER (the builtins, then
// what the company declared, in the order it declared them), and a type the
// catalogue no longer declares sorts LAST in both directions, as an absent due
// date does: it has no place in the order to be first or last in.
func TestSortingByTypeFollowsTheCatalogue(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	declare := func(op string, slugs ...string) {
		t.Helper()
		var types []worklet.TaskType
		for _, slug := range slugs {
			types = append(types, worklet.TaskType{Slug: slug, Name: strings.ToUpper(slug)})
		}
		if _, err := r.writer.WriteTypes(t.Context(), op, types); err != nil {
			t.Fatalf("WriteTypes: %v", err)
		}
		r.drain()
	}
	declare("op-types", "research", "incident", "legacy")
	typed := func(kind string) func(*worklet.Task) {
		return func(task *worklet.Task) { task.Type = kind }
	}
	filed(t, r, "inc", typed("incident"))
	filed(t, r, "old", typed("legacy"))
	filed(t, r, "bug", typed("bug"))
	filed(t, r, "res", typed("research"))
	filed(t, r, "t1", typed("task"))
	filed(t, r, "t2", typed("task"))
	// AND THE COMPANY DROPS ONE, which leaves the work filed under it.
	declare("op-retire", "research", "incident")

	holdsOrder(t, r, map[string]any{"container": "project:ENG", "sort": "type"},
		[]string{"t1", "t2", "bug", "res", "inc", "old"})
	holdsOrder(t, r, map[string]any{"container": "project:ENG", "sort": "-type"},
		[]string{"inc", "res", "bug", "t1", "t2", "old"})
}

// AN ASSIGNEE IS SORTED BY THE NAME A PERSON READS, which the chart holds and
// the handle does not: `zed` named Ada Lovelace comes first. A handle the
// chart no longer holds sorts by itself after every named seat, and the
// unassigned come last, in both directions.
func TestSortingByAssigneeReadsTheChartsNames(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	held := func(handle string) func(*worklet.Task) {
		return func(task *worklet.Task) { task.Assignee = handle }
	}
	filed(t, r, "n1", held(""))
	filed(t, r, "amy", held("amy"))
	filed(t, r, "gone", held("gone"))
	filed(t, r, "zed1", held("zed"))
	filed(t, r, "bob", held("bob"))
	filed(t, r, "zed2", held("zed"))
	filed(t, r, "n2", held(""))
	applyNames(t, r, 1, map[string]string{"zed": "Ada Lovelace", "bob": "bea Smith",
		"amy": "Zoe Quinn"})

	holdsOrder(t, r, map[string]any{"container": "project:ENG", "sort": "assignee"},
		[]string{"zed1", "zed2", "bob", "amy", "gone", "n1", "n2"})
	holdsOrder(t, r, map[string]any{"container": "project:ENG", "sort": "-assignee"},
		[]string{"amy", "bob", "zed1", "zed2", "gone", "n1", "n2"})

	// A CHART NAMING NONE OF THEM ORDERS BY THE HANDLE, which is the name
	// such a surface draws, and the unassigned are still last.
	applyNames(t, r, 2, map[string]string{"someone": "Someone Else"})
	holdsOrder(t, r, map[string]any{"container": "project:ENG", "sort": "assignee"},
		[]string{"amy", "bob", "gone", "zed1", "zed2", "n1", "n2"})
	holdsOrder(t, r, map[string]any{"container": "project:ENG", "sort": "-assignee"},
		[]string{"zed1", "zed2", "gone", "bob", "amy", "n1", "n2"})
}

// AN ORDER BUILT FROM THE CHART IS ONE ORDER PER EDIT OF IT. A seat renamed
// or added between two pages moves the places after it, so the second page
// cannot be read after the first one's last row: the cursor is refused as an
// [worklet.ErrBadCursor], which every surface answers as a request to start
// again, rather than resumed in the middle of an order it was not minted in.
func TestAnEditedChartStartsTheListingAgain(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for _, handle := range []string{"amy", "bob"} {
		filed(t, r, handle, func(task *worklet.Task) { task.Assignee = handle })
	}
	applyNames(t, r, 1, map[string]string{"amy": "Amy", "bob": "Bob"})
	first, err := askAs(t, r, map[string]any{
		"container": "project:ENG", "sort": "assignee", "limit": "1"})
	if err != nil || first.NextCursor == "" {
		t.Fatalf("the first page: %v, cursor %q", err, first.NextCursor)
	}
	next := map[string]any{"container": "project:ENG", "sort": "assignee",
		"limit": "1", "cursor": first.NextCursor}
	if _, err := askAs(t, r, next); err != nil {
		t.Fatalf("the same chart refused its own cursor: %v", err)
	}
	// AND THE SAME CHART APPLIED AGAIN, by another node or at a boot, is
	// still the same order: it writes nothing, so the cursor still resumes.
	applyNames(t, r, 1, map[string]string{"amy": "Amy", "bob": "Bob"})
	if _, err := askAs(t, r, next); err != nil {
		t.Fatalf("a chart applied again refused a cursor of its own order: %v", err)
	}
	for i, after := range []map[string]string{
		{"amy": "Zoe", "bob": "Bob"},
		{"amy": "Amy", "bob": "Bob", "abe": "Abe"},
	} {
		applyNames(t, r, 2+i, after)
		_, err = askAs(t, r, next)
		if !errors.Is(err, worklet.ErrBadCursor) {
			t.Errorf("after %v: a cursor minted before the edit was answered %v, "+
				"want an ErrBadCursor", after, err)
		}
	}
}

// A CURSOR STAYS SHORT HOWEVER BIG THE CHART IS. The assignee order names every
// seat, and a cursor carrying it would be kilobytes in every address and every
// model's context that pages a listing.
func TestACursorDoesNotCarryTheChart(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filed(t, r, "a", func(task *worklet.Task) { task.Assignee = "seat-1" })
	filed(t, r, "b", func(task *worklet.Task) { task.Assignee = "seat-2" })
	chart := map[string]string{}
	for i := range 500 {
		chart[fmt.Sprintf("seat-%d", i)] = fmt.Sprintf("A seat with a long name %d", i)
	}
	applyNames(t, r, 1, chart)
	answer, err := askAs(t, r, map[string]any{
		"container": "project:ENG", "sort": "assignee", "limit": "1"})
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	if answer.NextCursor == "" || len(answer.NextCursor) > 256 {
		t.Fatalf("the cursor is %d bytes, want a page's place rather than the "+
			"order it is in", len(answer.NextCursor))
	}
}

// AN OLDER CHART DOES NOT WALK THE SEATS' ORDER BACK. Two nodes applying two
// activations is ordinary during a rollout, and the one applying the older
// one second must leave the newer order standing, as a project's chart-owned
// fields are left ([worklet.Writer.ApplyChart]).
func TestAnOlderChartLeavesTheSeatsOrderAlone(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for _, handle := range []string{"amy", "bob"} {
		filed(t, r, handle, func(task *worklet.Task) { task.Assignee = handle })
	}
	applyNames(t, r, 2, map[string]string{"amy": "Zoe", "bob": "Bob"})
	wrote, err := r.writer.ApplyRoster(t.Context(), wednesday.Add(time.Minute),
		map[string]string{"amy": "Amy", "bob": "Bob"})
	if err != nil || wrote {
		t.Fatalf("an older chart's apply wrote %v (err %v), want nothing", wrote, err)
	}
	r.drain()
	holdsOrder(t, r, map[string]any{"container": "project:ENG", "sort": "assignee"},
		[]string{"bob", "amy"})
}

// THE CHART APPLY WRITES EVERY SEAT'S PLACE, in the order of its name, and once:
// the same chart applied again, at the same activation, writes nothing, which
// is what makes a reconcile on every apply and every boot free after the
// first node's.
func TestTheChartApplyPlacesEverySeatOnce(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	names := map[string]string{"zoe": "Zoe Quinn", "emile": "Émile Zola", "olga": "Olga Berg"}
	at := wednesday.Add(time.Minute)
	for i, want := range []bool{true, false} {
		wrote, err := r.writer.ApplyRoster(t.Context(), at, names)
		if err != nil || wrote != want {
			t.Fatalf("apply %d wrote %v (err %v), want %v", i+1, wrote, err, want)
		}
		r.drain()
	}
	var places []string
	if err := r.db.Read(t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(t.Context(),
			`SELECT handle FROM tracker_seat_places ORDER BY place`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var handle string
			if err := rows.Scan(&handle); err != nil {
				return err
			}
			places = append(places, handle)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("read the places: %v", err)
	}
	if want := []string{"emile", "olga", "zoe"}; !slices.Equal(places, want) {
		t.Fatalf("the seats are placed %v, want %v", places, want)
	}
}
