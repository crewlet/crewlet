package code_test

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/statelog"
)

// The two coding runs the cases push for: each a launch's id, as the engine
// mints one.
const (
	runA = "3f1c2b4a-5d6e-4f70-8a9b-0c1d2e3f4a5b"
	runB = "7d2e1f0a-9b8c-4d7e-a6f5-e4d3c2b1a090"
)

// boxPush is one push of cmds by seat handle's coding box for run: the run
// behind it — the turn that launched it, its work item, its model, the run's
// launch and its coding agent — received by git, counted on the harness's day.
func boxPush(id, handle, run string, cmds ...code.Command) code.PushEntry {
	e := entry(id, cmds...)
	e.Principal = code.Principal{Kind: code.PrincipalAgent, Seat: handle, Run: code.RunProvenance{
		TurnID: "0192f00d-0000-7000-8000-000000000001", WorkBackend: "native", WorkKey: "ENG-42",
		Model: "claude-sonnet-4-5", RunID: run, CodingAgent: "claude-code"}}
	e.Day = harnessAt.Format(time.DateOnly)
	return e
}

// A PUSH ROW NAMES THE RUN THAT MADE IT: a coding box's push writes the run
// that pushed — its launch's id — and its coding agent into its row, beside the
// turn, the work item and the model, as its record's entry carries them; a
// person's push writes neither; and the history reads both back as the entry
// said them.
//
// Mutation: drop the run_id write from the push apply — the box's row names no
// run, red.
func TestAPushRowNamesTheRunThatMadeIt(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	id := seededRepo(t, n, "app")
	person := entry("p-person", set("refs/heads/main", code.ZeroOID, oidOf("person")))
	person.Principal = code.Principal{Kind: code.PrincipalOperator, OperatorID: "pat:019a2f4e", Seat: "founder"}
	box := boxPush("p-box", "swe", runA, set("refs/heads/seats/swe/eng-42", code.ZeroOID, oidOf("box")))
	n.push(id, person, box)

	for push, want := range map[string][2]string{"p-person": {"", ""}, "p-box": {runA, "claude-code"}} {
		var got [2]string
		if err := n.db.Reader().Read(t.Context(), func(tx *sql.Tx) error {
			return tx.QueryRowContext(t.Context(), `SELECT run_id, coding_agent FROM code_pushes WHERE push_id = ?`,
				push).Scan(&got[0], &got[1])
		}); err != nil {
			t.Fatalf("read push %s's row: %v", push, err)
		}
		if got != want {
			t.Errorf("push %s's row names run %q by %q, want %q by %q", push, got[0], got[1], want[0], want[1])
		}
	}
	history, err := n.reader.Read(t.Context(), code.ReadQuery{Asker: asker, Kind: code.ReadPushes, Repo: "app",
		Fresh: stale})
	if err != nil {
		t.Fatalf("Read(pushes): %v", err)
	}
	runs := map[string]code.RunProvenance{}
	for _, r := range history.Pushes {
		runs[r.PushID] = r.Run
	}
	if runs["p-box"] != box.Principal.Run || !runs["p-person"].IsZero() || len(runs) != 2 {
		t.Fatalf("the history reads the runs %+v, want the box's whole run %+v and none for the person",
			runs, box.Principal.Run)
	}
}

// A RUN'S PUSHES ARE READ IN LOG ORDER: a run_pushes read answers exactly the
// pushes that name its run, of the repositories its query names — each
// repository's in log order, its position and then its push id, and the
// repositories in the query's order — never another run's, a person's, a
// repository's it was not given, or one from at or below the run's launch;
// across pages of three pushes READ — the run's and everybody else's its
// repositories took — none repeated and none skipped: pages spanning two
// repositories, a page ending on a push that is not the run's, and a page
// boundary inside a record of two of the run's pushes; every later page at
// session at the first's position. Log order is the broker's instant's
// reverse here — each record stamped a second BEFORE the one ahead of it, as
// a stream leader's change can stamp — so an order by the instant answers
// every repository's pushes backwards.
//
// Mutations: order runWindowFirstSQL and runWindowAfterSQL by `at, push_id`
// rather than `position, push_id` — the pushes of each repository come back
// newest first, red; key a later page by its position alone (runWindowAfterSQL
// testing `position > ?`) — the second push of the record the boundary cuts
// is skipped.
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
	ref := func(name string) code.Command {
		return set("refs/heads/seats/swe/"+name, code.ZeroOID, oidOf(name))
	}
	// A PUSH OF THE RUN BELOW ITS LAUNCH, which no read of the run answers.
	n.push(web, boxPush("a-before", "swe", runA, ref("before")))
	launch := n.waiter.Committed()

	n.push(web, boxPush("a-web-1", "swe", runA, ref("web-1")))
	n.push(api, boxPush("b-api-1", "swe", runB, ref("b-api-1")), boxPush("a-api-1", "swe", runA, ref("api-1")))
	n.push(web, entry("person-web", set("refs/heads/main", code.ZeroOID, oidOf("person-web"))))
	// TWO OF THE RUN'S PUSHES IN ONE RECORD, at one position.
	n.push(docs, boxPush("a-docs-1", "swe", runA, ref("docs-1")), boxPush("a-docs-2", "swe", runA, ref("docs-2")))
	n.push(other, boxPush("a-other-1", "swe", runA, ref("other-1")))
	n.push(web, boxPush("a-web-2", "swe", runA, ref("web-2")))
	n.push(api, boxPush("b-api-2", "swe", runB, ref("b-api-2")))
	n.push(web, boxPush("a-web-3", "swe", runA, ref("web-3")))
	last := n.push(api, boxPush("a-api-2", "swe", runA, ref("api-2"))).Position

	q := code.ReadQuery{Kind: code.ReadRunPushes, Limit: 3,
		RunPushes: &code.RunPushesQuery{Run: runA, Repos: []string{web, api, docs}, After: launch},
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
			if p.Run.RunID != runA || p.Run.CodingAgent != "claude-code" {
				t.Errorf("push %s names the run %q by %q, want %s's", p.PushID, p.Run.RunID, p.Run.CodingAgent, runA)
			}
			got = append(got, p.PushID)
		}
		if answer.Next == "" {
			break
		}
		q.Cursor = answer.Next
		q.Fresh = statelog.FloorAt(statelog.ReadSession, answer.Position)
	}
	// Within one record a repository's pushes are in push id order. Pages of
	// three read, the run's kept: [web-1 person-web web-2] [web-3 | api-1
	// b-api-1] [b-api-2 api-2 | docs-1] [docs-2] — the second page ending on
	// the other run's push of the api record, the third page's boundary
	// inside the docs record.
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
// between the run's launch and its one push, its repository took six pushes of
// other people's, and a page reads no more of them than its row limit — and
// no more than its bytes, each push weighed by what its row holds — answering
// none of the run's and a cursor after the last push it read, from which the
// next page reads on, until the run's push is answered once. Read whole, a
// page that held only to what it kept would read every other push to find the
// run's, however many and however wide.
//
// Mutations: leave the pushes a page reads uncounted (runPushesOf adding a
// span's pushes and weight to nothing) — the first page ends with no cursor,
// short of the run's push, red; set the cursor of a page whose reads are full
// only where it kept a push — the same, red. That the span behind a page looks
// up no row past its bytes is held by
// TestARunsWindowReachesNoRowPastItsPagesBytes, which the answer cannot show.
func TestAPageOfARunsPushesIsHeldToWhatItReads(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	id := seededRepo(t, n, "app")
	launch := n.waiter.Committed()
	var others []string
	for k := range 6 {
		push := fmt.Sprintf("p-other-%d", k)
		person := entry(push, set(fmt.Sprintf("refs/heads/topic-%d", k), code.ZeroOID, oidOf(push)))
		n.push(id, person)
		others = append(others, push)
	}
	n.push(id, boxPush("a-run", "swe", runA, set("refs/heads/seats/swe/eng-42", code.ZeroOID, oidOf("run"))))
	last := n.push(id, entry("p-after", set("refs/heads/later", code.ZeroOID, oidOf("after")))).Position
	// THE WEIGHT a page counts a push it reads by: its row's row_bytes, the
	// same for every other push here, each one command on a ref named alike.
	var weight int
	if err := n.db.Reader().Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), `SELECT row_bytes FROM code_pushes WHERE push_id = ?`,
			others[0]).Scan(&weight)
	}); err != nil {
		t.Fatalf("read a push's weight: %v", err)
	}

	read := func(t *testing.T, limit int) (pages [][]string, cursors []string) {
		t.Helper()
		q := code.ReadQuery{Kind: code.ReadRunPushes, Limit: limit,
			RunPushes: &code.RunPushesQuery{Run: runA, Repos: []string{id}, After: launch},
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
		// first is the push the first page's cursor must name: the last
		// it read.
		first string
	}{
		// TWO PUSHES A PAGE by its row limit, its bytes far off.
		"the rows a page reads": {limit: 2, bytes: code.MaxPageBytes, first: others[1]},
		// THREE PUSHES' WEIGHT A PAGE, its row limit far off: the third
		// push takes it to its bytes, and no fourth is read.
		"what the rows weigh": {bytes: 3 * weight, first: others[2]},
	} {
		t.Run(name, func(t *testing.T) {
			code.SetPageLimits(n.reader, code.MaxSnapshotItems, tc.bytes)
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
// and no seat it reads for — one there would be judged by nothing — and names
// its run by its launch's UUID, one to MaxRunRepos of the run's repositories
// by id, each once, and the position the run was launched at on the code log;
// nothing else a listing names, a limit of the pushes a page reads, at most
// MaxSnapshotItems, and a level of its own. A read naming a run is refused on every other kind. A cursor it
// never issued is refused, and a repository holding no rows — purged since —
// answers none of the run's pushes.
//
// Mutation: accept an asker on a run_pushes read (validateRunPushes dropping
// its first case) — a person's read of the run's pushes is served, red.
func TestOnlyTheEngineReadsARunsPushes(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	id := seededRepo(t, n, "app")
	n.push(id, boxPush("a-1", "swe", runA, set("refs/heads/seats/swe/x", code.ZeroOID, oidOf("x"))))
	at := n.waiter.Committed()
	launch := statelog.Position{Stream: at.Stream, Generation: at.Generation, Seq: 1}
	run := func(change func(*code.RunPushesQuery)) *code.RunPushesQuery {
		in := &code.RunPushesQuery{Run: runA, Repos: []string{id}, After: launch}
		change(in)
		return in
	}
	nine := make([]string, code.MaxRunRepos+1)
	for i := range nine {
		nine[i] = fmt.Sprintf("0190f3c4-0000-7000-8000-%012d", i)
	}
	for name, tc := range map[string]struct {
		q     code.ReadQuery
		field string
	}{
		"a person asking": {code.ReadQuery{Asker: asker,
			RunPushes: run(func(*code.RunPushesQuery) {})}, "asker"},
		"a seat asking": {code.ReadQuery{Asker: agent("swe"),
			RunPushes: run(func(*code.RunPushesQuery) {})}, "asker"},
		"a read for a seat": {code.ReadQuery{CeilingSeat: "swe",
			RunPushes: run(func(*code.RunPushesQuery) {})}, "ceiling_seat"},
		"no run": {code.ReadQuery{}, "run_pushes"},
		"a run that is no UUID": {code.ReadQuery{RunPushes: run(func(r *code.RunPushesQuery) {
			r.Run = "launch-1"
		})}, "run_pushes.run"},
		"a run spelled in capitals": {code.ReadQuery{RunPushes: run(func(r *code.RunPushesQuery) {
			r.Run = strings.ToUpper(runA)
		})}, "run_pushes.run"},
		"no repository": {code.ReadQuery{RunPushes: run(func(r *code.RunPushesQuery) { r.Repos = nil })},
			"run_pushes.repos"},
		"more repositories than a run is given": {code.ReadQuery{RunPushes: run(func(r *code.RunPushesQuery) {
			r.Repos = nine
		})}, "run_pushes.repos"},
		"a repository by name": {code.ReadQuery{RunPushes: run(func(r *code.RunPushesQuery) {
			r.Repos = []string{"app"}
		})}, "run_pushes.repos[0]"},
		"a repository twice": {code.ReadQuery{RunPushes: run(func(r *code.RunPushesQuery) {
			r.Repos = []string{id, id}
		})}, "run_pushes.repos[1]"},
		"no launch": {code.ReadQuery{RunPushes: run(func(r *code.RunPushesQuery) {
			r.After = statelog.Position{}
		})}, "run_pushes.after"},
		"a launch on another log": {code.ReadQuery{RunPushes: run(func(r *code.RunPushesQuery) {
			r.After = statelog.Position{Stream: "CREWLET_TRACKER_LOG", Generation: 1, Seq: 1}
		})}, "run_pushes.after"},
		"a repository's name beside it": {code.ReadQuery{Repo: "app",
			RunPushes: run(func(*code.RunPushesQuery) {})}, "repo"},
		"a prefix beside it": {code.ReadQuery{Prefix: "refs/heads/",
			RunPushes: run(func(*code.RunPushesQuery) {})}, "prefix"},
		"a range beside it": {code.ReadQuery{Introduced: &code.IntroducedQuery{RepoID: id},
			RunPushes: run(func(*code.RunPushesQuery) {})}, "introduced"},
		"a limit past a page": {code.ReadQuery{Limit: code.MaxSnapshotItems + 1,
			RunPushes: run(func(*code.RunPushesQuery) {})}, "limit"},
		"a read in brief": {code.ReadQuery{Brief: true,
			RunPushes: run(func(*code.RunPushesQuery) {})}, "brief"},
		"a read narrowed to one push": {code.ReadQuery{Push: "0190f3c4-0000-7000-8000-000000000001",
			RunPushes: run(func(*code.RunPushesQuery) {})}, "push"},
		"a change's number beside it": {code.ReadQuery{Change: 1,
			RunPushes: run(func(*code.RunPushesQuery) {})}, "change"},
		"a change's state beside it": {code.ReadQuery{State: "open",
			RunPushes: run(func(*code.RunPushesQuery) {})}, "state"},
		"a change's owner beside it": {code.ReadQuery{Owner: "swe",
			RunPushes: run(func(*code.RunPushesQuery) {})}, "owner"},
		"a work item beside it": {code.ReadQuery{Work: "native:task-1",
			RunPushes: run(func(*code.RunPushesQuery) {})}, "work"},
	} {
		tc.q.Kind, tc.q.Fresh = code.ReadRunPushes, stale
		if _, err := n.reader.Read(t.Context(), tc.q); !errors.Is(err, code.ErrInvalid) ||
			!strings.Contains(err.Error(), tc.field+":") {
			t.Errorf("%s: Read(run_pushes) = %v, want ErrInvalid naming %s", name, err, tc.field)
		}
	}
	// A RUN NAMED ON ANOTHER KIND is refused, whoever reads it.
	for name, q := range map[string]code.ReadQuery{
		"a history":            {Asker: asker, Kind: code.ReadPushes, Repo: "app"},
		"a listing of changes": {Asker: asker, Kind: code.ReadChanges, Repo: "app"},
		"a change":             {Asker: asker, Kind: code.ReadChange, Repo: "app", Change: 1},
		"an introduced read":   {Kind: code.ReadIntroduced, Introduced: &code.IntroducedQuery{RepoID: id}},
		"a duty listing":       {Kind: code.ReadCompactionDue, MinPacks: 1},
	} {
		q.RunPushes, q.Fresh = run(func(*code.RunPushesQuery) {}), stale
		if _, err := n.reader.Read(t.Context(), q); !errors.Is(err, code.ErrInvalid) ||
			!strings.Contains(err.Error(), "run_pushes:") {
			t.Errorf("%s naming a run: Read = %v, want ErrInvalid naming run_pushes", name, err)
		}
	}
	for name, fresh := range map[string]statelog.Freshness{
		"no level":                {},
		"a session with no floor": {Level: statelog.ReadSession},
	} {
		_, err := n.reader.Read(t.Context(), code.ReadQuery{Kind: code.ReadRunPushes,
			RunPushes: run(func(*code.RunPushesQuery) {}), Fresh: fresh})
		if !errors.Is(err, code.ErrBadLevel) {
			t.Errorf("%s: Read(run_pushes) = %v, want ErrBadLevel", name, err)
		}
	}
	for _, cursor := range []string{"not a cursor", "1/" + fmt.Sprint(at.Packed()) + "/a-1",
		"0/" + fmt.Sprint(launch.Packed()) + "/a-1", "00/" + fmt.Sprint(at.Packed()) + "/a-1"} {
		_, err := n.reader.Read(t.Context(), code.ReadQuery{Kind: code.ReadRunPushes,
			RunPushes: run(func(*code.RunPushesQuery) {}), Cursor: cursor, Fresh: stale})
		if !errors.Is(err, code.ErrBadCursor) {
			t.Errorf("a run_pushes read after the cursor %q it never issued = %v, want ErrBadCursor", cursor, err)
		}
	}
	// THE ENGINE'S OWN READ IS SERVED: the run's push, and none of a
	// repository holding no rows.
	answer, err := n.reader.Read(t.Context(), code.ReadQuery{Kind: code.ReadRunPushes,
		RunPushes: run(func(r *code.RunPushesQuery) {
			r.Repos = []string{"0190f3c4-0000-7000-8000-00000000dead", id}
		}), Fresh: stale})
	if err != nil || len(answer.Pushes) != 1 || answer.Pushes[0].PushID != "a-1" || answer.Next != "" {
		t.Fatalf("the engine's read of the run's pushes = %+v, %v; want its one push and no cursor", answer, err)
	}
}
