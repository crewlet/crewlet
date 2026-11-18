package gitlet_test

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/statelog"
)

// The two executors the cases push for: each a launch's id, as the engine
// mints one.
const (
	runA = "3f1c2b4a-5d6e-4f70-8a9b-0c1d2e3f4a5b"
	runB = "7d2e1f0a-9b8c-4d7e-a6f5-e4d3c2b1a090"
)

// boxPush is one push of cmds by seat handle's coding box for run: the run
// behind it — the turn that launched it, its work item, its model, the run's
// launch and its harness — received by git, counted on the harness's day.
func boxPush(id, handle, run string, cmds ...gitlet.Command) gitlet.PushEntry {
	e := entry(id, cmds...)
	e.Principal = gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: handle, Run: gitlet.RunProvenance{
		TurnID: "0192f00d-0000-7000-8000-000000000001", WorkBackend: "native", WorkKey: "ENG-42",
		Model: "claude-sonnet-4-5", RunID: run, Harness: "claude-code"}}
	e.Day = harnessAt.Format(time.DateOnly)
	return e
}

// A PUSH ROW NAMES THE RUN THAT MADE IT: a coding box's push writes its
// launch's id and harness beside the turn, work item and model, as its entry
// carries them; a person's push writes neither; the history reads both back.
//
// Mutation: drop the run_id write from the push apply, red.
func TestAPushRowNamesTheRunThatMadeIt(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	id := seededRepo(t, n, "app")
	person := entry("p-person", set("refs/heads/main", gitlet.ZeroOID, oidOf("person")))
	person.Principal = gitlet.Principal{Kind: gitlet.PrincipalOperator, OperatorID: "pat:019a2f4e", Seat: "founder"}
	box := boxPush("p-box", "swe", runA, set("refs/heads/seats/swe/eng-42", gitlet.ZeroOID, oidOf("box")))
	n.push(id, person, box)

	for push, want := range map[string][2]string{"p-person": {"", ""}, "p-box": {runA, "claude-code"}} {
		var got [2]string
		if err := n.db.Reader().Read(t.Context(), func(tx *sql.Tx) error {
			return tx.QueryRowContext(t.Context(), `SELECT run_id, harness FROM code_pushes WHERE push_id = ?`,
				push).Scan(&got[0], &got[1])
		}); err != nil {
			t.Fatalf("read push %s's row: %v", push, err)
		}
		if got != want {
			t.Errorf("push %s's row names run %q by %q, want %q by %q", push, got[0], got[1], want[0], want[1])
		}
	}
	history, err := n.reader.Read(t.Context(), gitlet.ReadQuery{Asker: asker, Kind: gitlet.ReadPushes, Repo: "app",
		Fresh: stale})
	if err != nil {
		t.Fatalf("Read(pushes): %v", err)
	}
	runs := map[string]gitlet.RunProvenance{}
	for _, r := range history.Pushes {
		runs[r.PushID] = r.Run
	}
	if runs["p-box"] != box.Principal.Run || !runs["p-person"].IsZero() || len(runs) != 2 {
		t.Fatalf("the history reads the runs %+v, want the box's whole run %+v and none for the person",
			runs, box.Principal.Run)
	}
}

// A RUN'S PUSHES ARE READ IN LOG ORDER: a run_pushes read answers exactly the
// run's pushes in the named repositories, each repository's by position then
// push id, the repositories in the query's order; never another run's, a
// person's, an unnamed repository's, or one at or below the run's launch.
// Across pages of three pushes READ, none repeats or is skipped: pages span
// two repositories, one ends on another's push, one splits a record of two of
// the run's pushes; later pages at session at the first's position. The
// broker's instant runs backwards here (as a stream leader's change can stamp
// it), so ordering by it reverses each repository.
//
// Mutations: order the window statements by `at, push_id`, red; key a later
// page by `position > ?` alone, the split record's second push is skipped.
func TestARunsPushesAreReadInLogOrder(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	web, api, docs, other := seededRepo(t, n, "web"), seededRepo(t, n, "api"), seededRepo(t, n, "docs"),
		seededRepo(t, n, "other")
	// THE BROKER'S INSTANT STEPS BACK a second every record from here on.
	from := n.consumed
	n.storedAt = func(seq uint64, broker time.Time) time.Time {
		if seq <= from {
			return broker
		}
		return harnessAt.Add(-time.Duration(seq-from) * time.Second)
	}
	ref := func(name string) gitlet.Command {
		return set("refs/heads/seats/swe/"+name, gitlet.ZeroOID, oidOf(name))
	}
	// A PUSH OF THE RUN BELOW ITS LAUNCH, which no read of the run answers.
	n.push(web, boxPush("a-before", "swe", runA, ref("before")))
	launch := n.waiter.Committed()

	n.push(web, boxPush("a-web-1", "swe", runA, ref("web-1")))
	n.push(api, boxPush("b-api-1", "swe", runB, ref("b-api-1")), boxPush("a-api-1", "swe", runA, ref("api-1")))
	n.push(web, entry("person-web", set("refs/heads/main", gitlet.ZeroOID, oidOf("person-web"))))
	// TWO OF THE RUN'S PUSHES IN ONE RECORD, at one position.
	n.push(docs, boxPush("a-docs-1", "swe", runA, ref("docs-1")), boxPush("a-docs-2", "swe", runA, ref("docs-2")))
	n.push(other, boxPush("a-other-1", "swe", runA, ref("other-1")))
	n.push(web, boxPush("a-web-2", "swe", runA, ref("web-2")))
	n.push(api, boxPush("b-api-2", "swe", runB, ref("b-api-2")))
	n.push(web, boxPush("a-web-3", "swe", runA, ref("web-3")))
	last := n.push(api, boxPush("a-api-2", "swe", runA, ref("api-2"))).Position

	q := gitlet.ReadQuery{Kind: gitlet.ReadRunPushes, Limit: 3,
		RunPushes: &gitlet.RunPushesQuery{Run: runA, Repos: []string{web, api, docs}, After: launch},
		Fresh:     statelog.FloorAt(statelog.ReadSession, last)}
	var (
		got   []string
		pages int
	)
	for ; pages < 20; pages++ {
		answer, err := n.reader.Read(t.Context(), q)
		if err != nil {
			t.Fatalf("Read(run_pushes) page %d: %v", pages+1, err)
		}
		if len(answer.Pushes) > 3 {
			t.Fatalf("page %d holds %d pushes, past its limit of 3 read", pages+1, len(answer.Pushes))
		}
		for _, p := range answer.Pushes {
			if p.Run.RunID != runA || p.Run.Harness != "claude-code" {
				t.Errorf("push %s names the run %q by %q, want %s's", p.PushID, p.Run.RunID, p.Run.Harness, runA)
			}
			got = append(got, p.PushID)
		}
		if answer.Next == "" {
			break
		}
		q.Cursor = answer.Next
		q.Fresh = statelog.FloorAt(statelog.ReadSession, answer.Position)
	}
	// Within one record, push id order. Pages of three read, the run's kept:
	// [web-1 person-web web-2] [web-3 | api-1 b-api-1] [b-api-2 api-2 |
	// docs-1] [docs-2].
	want := []string{"a-web-1", "a-web-2", "a-web-3", "a-api-1", "a-api-2", "a-docs-1", "a-docs-2"}
	if !slices.Equal(got, want) {
		t.Fatalf("the run's pushes read %v, want %v: its repositories in the query's order, each one's "+
			"pushes in log order, none of another run's, a person's, another repository's or one below its "+
			"launch", got, want)
	}
	if pages+1 != 4 {
		t.Fatalf("the read took %d pages of three, want 4: the case needs its boundaries where it says",
			pages+1)
	}
}

// A PAGE OF A RUN'S PUSHES IS HELD TO WHAT IT READS, NOT TO WHAT IT ANSWERS:
// six other pushes sit between the run's launch and its one push, and a page
// reads no more than its row limit or its bytes (each push weighed by its
// row), answering a cursor after the last push it read, until the run's push
// is answered once. A page held only to what it kept would read every other
// push to find the run's.
//
// Mutations, each red (the first page ends with no cursor): runPushesOf not
// counting a span's pushes and weight; a full page's cursor set only where it
// kept a push. The span's own bound is TestARunsWindowReachesNoRowPastItsPagesBytes.
func TestAPageOfARunsPushesIsHeldToWhatItReads(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	id := seededRepo(t, n, "app")
	launch := n.waiter.Committed()
	var others []string
	for k := range 6 {
		push := fmt.Sprintf("p-other-%d", k)
		person := entry(push, set(fmt.Sprintf("refs/heads/topic-%d", k), gitlet.ZeroOID, oidOf(push)))
		n.push(id, person)
		others = append(others, push)
	}
	n.push(id, boxPush("a-run", "swe", runA, set("refs/heads/seats/swe/eng-42", gitlet.ZeroOID, oidOf("run"))))
	last := n.push(id, entry("p-after", set("refs/heads/later", gitlet.ZeroOID, oidOf("after")))).Position
	// THE WEIGHT a page counts a push by: its row_bytes, the same for every
	// other push here.
	var weight int
	if err := n.db.Reader().Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), `SELECT row_bytes FROM code_pushes WHERE push_id = ?`,
			others[0]).Scan(&weight)
	}); err != nil {
		t.Fatalf("read a push's weight: %v", err)
	}

	read := func(t *testing.T, limit int) (pages [][]string, cursors []string) {
		t.Helper()
		q := gitlet.ReadQuery{Kind: gitlet.ReadRunPushes, Limit: limit,
			RunPushes: &gitlet.RunPushesQuery{Run: runA, Repos: []string{id}, After: launch},
			Fresh:     statelog.FloorAt(statelog.ReadSession, last)}
		for len(pages) < 20 {
			answer, err := n.reader.Read(t.Context(), q)
			if err != nil {
				t.Fatalf("Read(run_pushes) page %d: %v", len(pages)+1, err)
			}
			var page []string
			for _, p := range answer.Pushes {
				page = append(page, p.PushID)
			}
			pages = append(pages, page)
			if answer.Next == "" {
				return pages, cursors
			}
			// The cursor names the last push the page read, by its id.
			cursors = append(cursors, answer.Next[strings.LastIndex(answer.Next, "/")+1:])
			q.Cursor = answer.Next
			q.Fresh = statelog.FloorAt(statelog.ReadSession, answer.Position)
		}
		t.Fatalf("the read took more than 20 pages: %v", pages)
		return nil, nil
	}
	for name, tc := range map[string]struct {
		limit, bytes int
		// first is the last push the first page read, which its cursor names.
		first string
	}{
		// TWO PUSHES A PAGE by its row limit, its bytes far off.
		"the rows a page reads": {limit: 2, bytes: gitlet.MaxPageBytes, first: others[1]},
		// THREE PUSHES' WEIGHT A PAGE, its row limit far off: the third
		// reaches its bytes, and no fourth is read.
		"what the rows weigh": {bytes: 3 * weight, first: others[2]},
	} {
		t.Run(name, func(t *testing.T) {
			gitlet.SetPageLimits(n.reader, gitlet.MaxSnapshotItems, tc.bytes)
			pages, cursors := read(t, tc.limit)
			if len(pages[0]) != 0 || len(cursors) == 0 || cursors[0] != tc.first {
				t.Fatalf("the first page answered %v and read through %v, want none of the run's and a cursor "+
					"after %s: a page reads no more than its bound of the pushes before the run's", pages[0],
					cursors, tc.first)
			}
			var answered []string
			for _, page := range pages {
				answered = append(answered, page...)
			}
			if !slices.Equal(answered, []string{"a-run"}) {
				t.Fatalf("the pages answered %v, want the run's one push once: %v", answered, pages)
			}
		})
	}
}

// A RUN'S PUSHES ARE THE ENGINE'S OWN READ: a run_pushes read names no asker
// or seat (nothing would judge one), and names its run by launch UUID, one to
// MaxRunRepos distinct repositories by id, and the launch position on the code
// log; nothing else a listing names, a page limit of at most MaxSnapshotItems,
// and a level of its own. A read naming a run is refused on every other kind,
// as is a cursor it never issued; a purged repository answers no pushes.
//
// Mutation: validateRunPushes dropping its first case, a person's read is
// served, red.
func TestOnlyTheEngineReadsARunsPushes(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	id := seededRepo(t, n, "app")
	n.push(id, boxPush("a-1", "swe", runA, set("refs/heads/seats/swe/x", gitlet.ZeroOID, oidOf("x"))))
	at := n.waiter.Committed()
	launch := statelog.Position{Stream: at.Stream, Generation: at.Generation, Seq: 1}
	run := func(change func(*gitlet.RunPushesQuery)) *gitlet.RunPushesQuery {
		in := &gitlet.RunPushesQuery{Run: runA, Repos: []string{id}, After: launch}
		change(in)
		return in
	}
	nine := make([]string, gitlet.MaxRunRepos+1)
	for i := range nine {
		nine[i] = fmt.Sprintf("0190f3c4-0000-7000-8000-%012d", i)
	}
	for name, tc := range map[string]struct {
		q     gitlet.ReadQuery
		field string
	}{
		"a person asking": {gitlet.ReadQuery{Asker: asker,
			RunPushes: run(func(*gitlet.RunPushesQuery) {})}, "asker"},
		"a seat asking": {gitlet.ReadQuery{Asker: agent("swe"),
			RunPushes: run(func(*gitlet.RunPushesQuery) {})}, "asker"},
		"a read for a seat": {gitlet.ReadQuery{CeilingSeat: "swe",
			RunPushes: run(func(*gitlet.RunPushesQuery) {})}, "ceiling_seat"},
		"no run": {gitlet.ReadQuery{}, "run_pushes"},
		"a run that is no UUID": {gitlet.ReadQuery{RunPushes: run(func(r *gitlet.RunPushesQuery) {
			r.Run = "launch-1"
		})}, "run_pushes.run"},
		"a run spelled in capitals": {gitlet.ReadQuery{RunPushes: run(func(r *gitlet.RunPushesQuery) {
			r.Run = strings.ToUpper(runA)
		})}, "run_pushes.run"},
		"no repository": {gitlet.ReadQuery{RunPushes: run(func(r *gitlet.RunPushesQuery) { r.Repos = nil })},
			"run_pushes.repos"},
		"more repositories than a run is given": {gitlet.ReadQuery{RunPushes: run(func(r *gitlet.RunPushesQuery) {
			r.Repos = nine
		})}, "run_pushes.repos"},
		"a repository by name": {gitlet.ReadQuery{RunPushes: run(func(r *gitlet.RunPushesQuery) {
			r.Repos = []string{"app"}
		})}, "run_pushes.repos[0]"},
		"a repository twice": {gitlet.ReadQuery{RunPushes: run(func(r *gitlet.RunPushesQuery) {
			r.Repos = []string{id, id}
		})}, "run_pushes.repos[1]"},
		"no launch": {gitlet.ReadQuery{RunPushes: run(func(r *gitlet.RunPushesQuery) {
			r.After = statelog.Position{}
		})}, "run_pushes.after"},
		"a launch on another log": {gitlet.ReadQuery{RunPushes: run(func(r *gitlet.RunPushesQuery) {
			r.After = statelog.Position{Stream: "CREWLET_TRACKER_LOG", Generation: 1, Seq: 1}
		})}, "run_pushes.after"},
		"a repository's name beside it": {gitlet.ReadQuery{Repo: "app",
			RunPushes: run(func(*gitlet.RunPushesQuery) {})}, "repo"},
		"a prefix beside it": {gitlet.ReadQuery{Prefix: "refs/heads/",
			RunPushes: run(func(*gitlet.RunPushesQuery) {})}, "prefix"},
		"a range beside it": {gitlet.ReadQuery{Introduced: &gitlet.IntroducedQuery{RepoID: id},
			RunPushes: run(func(*gitlet.RunPushesQuery) {})}, "introduced"},
		"a limit past a page": {gitlet.ReadQuery{Limit: gitlet.MaxSnapshotItems + 1,
			RunPushes: run(func(*gitlet.RunPushesQuery) {})}, "limit"},
		"a read in brief": {gitlet.ReadQuery{Brief: true,
			RunPushes: run(func(*gitlet.RunPushesQuery) {})}, "brief"},
		"a read narrowed to one push": {gitlet.ReadQuery{Push: "0190f3c4-0000-7000-8000-000000000001",
			RunPushes: run(func(*gitlet.RunPushesQuery) {})}, "push"},
		"a change's number beside it": {gitlet.ReadQuery{Change: 1,
			RunPushes: run(func(*gitlet.RunPushesQuery) {})}, "change"},
		"a change's state beside it": {gitlet.ReadQuery{State: "open",
			RunPushes: run(func(*gitlet.RunPushesQuery) {})}, "state"},
		"a change's owner beside it": {gitlet.ReadQuery{Owner: "swe",
			RunPushes: run(func(*gitlet.RunPushesQuery) {})}, "owner"},
		"a work item beside it": {gitlet.ReadQuery{Work: "native:task-1",
			RunPushes: run(func(*gitlet.RunPushesQuery) {})}, "work"},
	} {
		tc.q.Kind, tc.q.Fresh = gitlet.ReadRunPushes, stale
		if _, err := n.reader.Read(t.Context(), tc.q); !errors.Is(err, gitlet.ErrInvalid) ||
			!strings.Contains(err.Error(), tc.field+":") {
			t.Errorf("%s: Read(run_pushes) = %v, want ErrInvalid naming %s", name, err, tc.field)
		}
	}
	// A RUN NAMED ON ANOTHER KIND is refused, whoever reads it.
	for name, q := range map[string]gitlet.ReadQuery{
		"a history":            {Asker: asker, Kind: gitlet.ReadPushes, Repo: "app"},
		"a listing of changes": {Asker: asker, Kind: gitlet.ReadChanges, Repo: "app"},
		"a change":             {Asker: asker, Kind: gitlet.ReadChange, Repo: "app", Change: 1},
		"an introduced read":   {Kind: gitlet.ReadIntroduced, Introduced: &gitlet.IntroducedQuery{RepoID: id}},
		"a duty listing":       {Kind: gitlet.ReadCompactionDue, MinPacks: 1},
	} {
		q.RunPushes, q.Fresh = run(func(*gitlet.RunPushesQuery) {}), stale
		if _, err := n.reader.Read(t.Context(), q); !errors.Is(err, gitlet.ErrInvalid) ||
			!strings.Contains(err.Error(), "run_pushes:") {
			t.Errorf("%s naming a run: Read = %v, want ErrInvalid naming run_pushes", name, err)
		}
	}
	for name, fresh := range map[string]statelog.Freshness{
		"no level":                {},
		"a session with no floor": {Level: statelog.ReadSession},
	} {
		_, err := n.reader.Read(t.Context(), gitlet.ReadQuery{Kind: gitlet.ReadRunPushes,
			RunPushes: run(func(*gitlet.RunPushesQuery) {}), Fresh: fresh})
		if !errors.Is(err, gitlet.ErrBadLevel) {
			t.Errorf("%s: Read(run_pushes) = %v, want ErrBadLevel", name, err)
		}
	}
	for _, cursor := range []string{"not a cursor", "1/" + fmt.Sprint(at.Packed()) + "/a-1",
		"0/" + fmt.Sprint(launch.Packed()) + "/a-1", "00/" + fmt.Sprint(at.Packed()) + "/a-1"} {
		_, err := n.reader.Read(t.Context(), gitlet.ReadQuery{Kind: gitlet.ReadRunPushes,
			RunPushes: run(func(*gitlet.RunPushesQuery) {}), Cursor: cursor, Fresh: stale})
		if !errors.Is(err, gitlet.ErrBadCursor) {
			t.Errorf("a run_pushes read after the cursor %q it never issued = %v, want ErrBadCursor", cursor, err)
		}
	}
	// THE ENGINE'S OWN READ IS SERVED: the run's push, and none of a
	// repository holding no rows.
	answer, err := n.reader.Read(t.Context(), gitlet.ReadQuery{Kind: gitlet.ReadRunPushes,
		RunPushes: run(func(r *gitlet.RunPushesQuery) {
			r.Repos = []string{"0190f3c4-0000-7000-8000-00000000dead", id}
		}), Fresh: stale})
	if err != nil || len(answer.Pushes) != 1 || answer.Pushes[0].PushID != "a-1" || answer.Next != "" {
		t.Fatalf("the engine's read of the run's pushes = %+v, %v; want its one push and no cursor", answer, err)
	}
}
