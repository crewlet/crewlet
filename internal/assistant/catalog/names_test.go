package catalog_test

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/assistant/assistanttest"
	"github.com/crewlet/crewlet/internal/assistant/cards"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/provenance"
)

// namedWorld is [assistanttest.World] with the directories a name resolves
// against: two spaces (one named Company whose key is HOME), two projects and
// two seats sharing a name.
func namedWorld() map[string]any {
	world := assistanttest.World()
	world["/containers"] = map[string]any{"containers": []any{
		map[string]any{"key": "HOME", "name": "Company"},
		map[string]any{"key": "FAIR", "name": "FairSplit"}}}
	world["/worklet/projects"] = map[string]any{"projects": []any{
		map[string]any{"key": "ENG", "name": "Engineering"},
		map[string]any{"key": "MKT", "name": "Marketing"}}}
	// World already holds the seat qa, "QA Engineer"; qa-web shares its name.
	org := world["/org"].(map[string]any)
	org["roles"] = append(org["roles"].([]any),
		map[string]any{"handle": "qa-web", "name": "QA engineer", "kind": "agent"})
	return world
}

// A NAME RESOLVES TO ITS KEY BEFORE ANYTHING IS BUILT, read through the
// person's own grant: "the Company space" is HOME, a project by its name is its
// key, a key in any case is the key, and a seat's name is its handle. A name
// two seats share is refused naming both, and a write naming nothing is
// refused listing the choices, so the model picks rather than guesses
// ("COMPANY", the defect this answers). A read keeps a key-shaped value it
// cannot place (a seat no longer on the chart still has turns), and refuses
// only what can only be a name. Mutation: match names with case, drop the
// trailing-noun rule, or let a write through unresolved, and this fails.
func TestANameResolvesToItsKeyOrIsRefusedWithTheChoices(t *testing.T) {
	t.Parallel()
	page, _ := catalog.Lookup("create_page")
	tasks, _ := catalog.Lookup("list_tasks")
	turns, _ := catalog.Lookup("list_turns")
	for name, tc := range map[string]struct {
		tool       catalog.Tool
		args       map[string]any
		arg, want  string
		refusedFor string
	}{
		"a space by its name":            {page, map[string]any{"container": "the Company space"}, "container", "HOME", ""},
		"a space by its key in any case": {page, map[string]any{"container": "fair"}, "container", "FAIR", ""},
		"a project by its name":          {tasks, map[string]any{"project": "marketing"}, "project", "MKT", ""},
		"a seat by its name":             {tasks, map[string]any{"assignee": "Product Manager"}, "assignee", "pm", ""},
		"a seat by its handle":           {tasks, map[string]any{"assignee": "pm"}, "assignee", "pm", ""},
		"a seat's name read as a removed seat's handle": {turns,
			map[string]any{"handle": "former-qa"}, "handle", "former-qa", ""},
		"a key guessed from a space's name": {page, map[string]any{"container": "COMPANY"}, "container", "HOME", ""},
		"a write to a space nobody declares": {page, map[string]any{"container": "WIKI"}, "", "",
			"The spaces are: Company (HOME), FairSplit (FAIR)."},
		"a read naming a seat that is not there": {tasks, map[string]any{"assignee": "Head of Fun"}, "",
			"", `"Head of Fun" is not one of the seats you can see.`},
		"a name two seats share": {tasks, map[string]any{"assignee": "qa engineer"}, "", "",
			"names 2 seats; name one by its key: QA Engineer (qa), QA engineer (qa-web)"},
	} {
		r := catalog.NewResolver(reader(namedWorld()))
		got, err := r.Resolve(t.Context(), tc.tool, tc.args)
		if tc.refusedFor != "" {
			if !errors.Is(err, catalog.ErrInvalidArguments) || !strings.Contains(err.Error(), tc.refusedFor) {
				t.Errorf("%s: resolved %v (%v), want refused saying %q", name, got, err, tc.refusedFor)
			}
			continue
		}
		if err != nil || got[tc.arg] != tc.want {
			t.Errorf("%s: %s = %v (%v), want %q", name, tc.arg, got[tc.arg], err, tc.want)
		}
	}
}

// A DIRECTORY THE PERSON MAY NOT READ RESOLVES NOTHING, and is asked once: the
// value is sent as written and the route decides it in its own words, so a
// name never reaches past the person's own reads and resolution is never a
// second authority. Each directory is read once per resolver, however many
// calls name it. Mutation: refuse when the directory is unreadable, or read
// it per call, and this fails.
func TestAnUnreadableDirectoryResolvesNothingAndIsAskedOnce(t *testing.T) {
	t.Parallel()
	var reads atomic.Int32
	refused := func(_ context.Context, r exec.Request) (exec.Response, error) {
		reads.Add(1)
		return exec.Response{Status: http.StatusForbidden,
			Body: []byte(`{"error":"forbidden","message":"You do not use Doclet."}`)}, nil
	}
	page, _ := catalog.Lookup("create_page")
	r := catalog.NewResolver(refused)
	for range 3 {
		got, err := r.Resolve(t.Context(), page, map[string]any{"container": "Company"})
		if err != nil || got["container"] != "Company" {
			t.Fatalf("an unreadable directory resolved %v (%v), want the value as written", got, err)
		}
	}
	if n := reads.Load(); n != 1 {
		t.Errorf("the directory was read %d times, want once", n)
	}

	counted := func(_ context.Context, r exec.Request) (exec.Response, error) {
		reads.Add(1)
		return assistanttest.Answer(namedWorld(), r), nil
	}
	reads.Store(0)
	r = catalog.NewResolver(counted)
	for _, v := range []string{"Company", "FairSplit", "HOME"} {
		if _, err := r.Resolve(t.Context(), page, map[string]any{"container": v}); err != nil {
			t.Fatal(err)
		}
	}
	if n := reads.Load(); n != 1 {
		t.Errorf("three resolutions read the directory %d times, want once", n)
	}
}

// EVERY ARGUMENT THAT NAMES A SEAT, A PROJECT OR A SPACE IS DECLARED, so the
// turn resolves it: a tool added with a "handle" or a "project" argument and
// no Names entry would take only the key again, and a person's "the QA
// Engineer" would be refused or, worse, guessed. Read off each argument's own
// description, so the rule is the catalogue's words and not a second list.
// Mutation: drop list_tasks' Names, or add a tool whose argument says it takes
// a handle and declare nothing, and this fails.
func TestEveryArgumentThatNamesARecordIsResolved(t *testing.T) {
	t.Parallel()
	for _, tool := range catalog.Tools() {
		props, _ := tool.Parameters["properties"].(map[string]any)
		for arg, raw := range props {
			described, _ := raw.(map[string]any)
			text, _ := described["description"].(string)
			lower := strings.ToLower(text)
			names := strings.Contains(lower, "handle") || strings.Contains(lower, "project's key") ||
				strings.Contains(lower, "space's key")
			// AN AUDIENCE IS ITS OWN GRAMMAR (`unit:<key>`, `company`), which
			// its route reads; a share names no seat by itself.
			if !names || strings.Contains(lower, "unit:<key>") {
				continue
			}
			if _, declared := tool.Names[arg]; !declared {
				t.Errorf("%s.%s takes %q and is not resolved: declare it in Names", tool.Name,
					arg, text)
			}
			if !strings.Contains(lower, "name") {
				t.Errorf("%s.%s is resolved and its description does not say it takes a name",
					tool.Name, arg)
			}
		}
	}
}

// TWO READS OF ONE TOOL ARE TOLD APART on the step and the card: every
// argument that narrows a read's rows is said in its scope, so "Spend by
// agent" over seven days and over fourteen are two titles, not one card drawn
// twice (the C1 report). Every read with a narrowing argument declares a
// scope; an argument that only picks the card's shape (a view, a section), a
// record's identity or a page size is exempt. Mutation: drop get_spend's
// Scope, or the title's use of it, and this fails.
func TestTwoReadsOfOneToolAreToldApart(t *testing.T) {
	t.Parallel()
	shapeOnly := map[string]bool{"limit": true, "view": true, "by": true, "scope": true,
		"section": true, "item": true, "page": true, "artifact": true, "key": true}
	for _, tool := range catalog.Tools() {
		if tool.Kind != catalog.KindRead {
			continue
		}
		props, _ := tool.Parameters["properties"].(map[string]any)
		for arg := range props {
			if !shapeOnly[arg] && tool.Scope == nil {
				t.Errorf("%s narrows its read by %q and states no Scope", tool.Name, arg)
			}
		}
	}

	spend, _ := catalog.Lookup("get_spend")
	week := map[string]any{"days": float64(7)}
	fortnight := map[string]any{"days": float64(14)}
	if a, b := spend.StepLabel(week), spend.StepLabel(fortnight); a == b ||
		a != "Reading token spend, last 7 days" {
		t.Errorf("the two reads are labelled %q and %q", a, b)
	}
	answer := []byte(`{"by_agent":[{"role":"QA Engineer","handle":"qa","total_tokens":10}]}`)
	var titles []string
	for _, args := range []map[string]any{week, fortnight} {
		res, err := catalog.Shape(spend, args, exec.Response{Status: http.StatusOK, Body: answer},
			provenance.NewBoundary(), wednesday, cards.Options{})
		if err != nil || len(res.Cards) == 0 {
			t.Fatalf("shape: %v", err)
		}
		titles = append(titles, res.Cards[0].Table.Title)
		if !strings.Contains(res.Model, res.Cards[0].Table.Title) {
			t.Errorf("the model reads %s without the card's title", res.Model)
		}
	}
	if titles[0] != "Spend by agent, last 7 days" || titles[1] != "Spend by agent, last 14 days" {
		t.Errorf("the cards are titled %q", titles)
	}
}

// A SEAT COLUMN IS DRAWN BY THE SEAT'S NAME, for the person and the model
// alike, and the name with its handle where two seats share it; a handle the
// chart does not hold is drawn as it is. A failed turn's row carries why it
// failed, which is what the model is asked. Mutation: draw the handle, or
// drop the turns card's reason column, and this fails.
func TestASeatIsShownByItsNameAndAFailedTurnByItsReason(t *testing.T) {
	t.Parallel()
	r := catalog.NewResolver(reader(namedWorld()))
	opts := cards.Options{SeatName: func(h string) (string, bool) { return r.SeatName(t.Context(), h) }}
	tasks, _ := catalog.Lookup("list_tasks")
	answer := []byte(`{"items":[{"key":"ENG-1","title":"a","assignee":"pm"},` +
		`{"key":"ENG-2","title":"b","assignee":"qa"},{"key":"ENG-3","title":"c","assignee":"gone"}]}`)
	res, err := catalog.Shape(tasks, nil, exec.Response{Status: http.StatusOK, Body: answer},
		provenance.NewBoundary(), wednesday, opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Product manager", "QA Engineer (qa)", "gone"} {
		if !strings.Contains(res.Model, want) {
			t.Errorf("the model reads %s, without %q", res.Model, want)
		}
	}
	if strings.Contains(res.Model, `"pm"`) {
		t.Errorf("the model reads the handle pm: %s", res.Model)
	}

	turns, _ := catalog.Lookup("list_turns")
	answer = []byte(`{"turns":[{"turn_id":"t1","role":"QA Engineer","failed":true,` +
		`"error":"the CLI never reached the seat's tools","error_kind":"server"}]}`)
	res, err = catalog.Shape(turns, map[string]any{"handle": "qa"}, exec.Response{Status: http.StatusOK,
		Body: answer}, provenance.NewBoundary(), wednesday, cards.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Model, "Why it failed") || !strings.Contains(res.Model, "never reached the seat") {
		t.Errorf("the model reads %s, without why the turn failed", res.Model)
	}
}

// A PAGE IS NEVER READ AS THE WHOLE: an answer whose cursor says more rows lie
// past it tells the model its count is a floor ("at least 5"), and the person
// that there are more; one with no cursor is the whole. "Five turns in a row"
// was once said of a seat that had failed twenty-five. Mutation: drop the
// turns card's More path, or the summary's use of it, and this fails.
func TestAPageWithMoreIsNeverTheWhole(t *testing.T) {
	t.Parallel()
	turns, _ := catalog.Lookup("list_turns")
	for _, tc := range []struct {
		next string
		more bool
	}{{`"bmV4dA"`, true}, {`null`, false}} {
		answer := []byte(`{"turns":[{"turn_id":"t1","failed":true},{"turn_id":"t2","failed":true}],` +
			`"next":` + tc.next + `}`)
		res, err := catalog.Shape(turns, map[string]any{"handle": "qa"}, exec.Response{
			Status: http.StatusOK, Body: answer}, provenance.NewBoundary(), wednesday, cards.Options{})
		if err != nil {
			t.Fatal(err)
		}
		saysFloor := strings.Contains(res.Model, "at least 2")
		hasNotice := len(res.Cards) == 2 && strings.Contains(res.Cards[1].Notice.Text, "there are more")
		if saysFloor != tc.more || hasNotice != tc.more {
			t.Errorf("next %s: the model reads %s and the cards are %+v, want more=%v", tc.next,
				res.Model, res.Cards, tc.more)
		}
	}
}

// THE MODEL READS EVERY INSTANT ON THE COMPANY'S CLOCK, and a midnight there as
// the date: handed UTC with a Z it told people times eight hours off their own
// screen, and called a task due all day (stored as the company's midnight) due
// the day before. The person's card keeps the instant, which the dashboard
// draws in the viewer's zone. Mutation: hand the model the card's cells as
// they are and this fails.
func TestTheModelReadsTimesOnTheCompanysClock(t *testing.T) {
	t.Parallel()
	zone, err := time.LoadLocation("America/Vancouver")
	if err != nil {
		t.Fatalf("the zone database: %v", err)
	}
	tasks, _ := catalog.Lookup("list_tasks")
	answer := []byte(`{"items":[{"key":"FAIR-2","title":"a","due":"2026-10-26T07:00:00Z"},` +
		`{"key":"FAIR-3","title":"b","due":"2026-10-26T08:45:00Z"}]}`)
	res, err := catalog.Shape(tasks, nil, exec.Response{Status: http.StatusOK, Body: answer},
		provenance.NewBoundary(), wednesday, cards.Options{Zone: zone})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`Mon 26 Oct 2026\"`, `Mon 26 Oct 2026 01:45 PDT`} {
		if !strings.Contains(res.Model, want) {
			t.Errorf("the model reads %s, without %s", res.Model, want)
		}
	}
	if strings.Contains(res.Model, "07:00:00Z") || strings.Contains(res.Model, "08:45:00Z") {
		t.Errorf("the model reads a UTC instant: %s", res.Model)
	}
	if got := res.Cards[0].Table.Rows[0]; !slices.Contains(got, any("2026-10-26T07:00:00Z")) {
		t.Errorf("the card's row is %v, want the instant kept for the dashboard", got)
	}
}

// EVERY TASK AND PROJECT CARD NAMES ITS KEY, and no seat card names one: the
// drawer makes the answer's first mention of a record its chip, the model
// writes a task or a project by its key and a seat by its name (the prompt's
// rule), so a seat keyed by its handle would never be found. Mutation: drop
// Key from a task spec, or key the seat detail by its handle.
func TestARecordIsKeyedTheWayTheAnswerNamesIt(t *testing.T) {
	t.Parallel()
	keyed := 0
	for _, tool := range catalog.Tools() {
		for variant, spec := range tool.Cards {
			e := spec.Entity
			if e == nil {
				continue
			}
			switch e.Kind {
			case wire.ResourceTask, wire.ResourceProject:
				if e.Key == "" {
					t.Errorf("%s (%q) cards a %s with no key", tool.Name, variant, e.Kind)
				}
				keyed++
			case wire.ResourceSeat:
				if e.Key != "" {
					t.Errorf("%s (%q) keys a seat by %q; the answer names a seat by its name",
						tool.Name, variant, e.Key)
				}
			}
		}
	}
	if keyed == 0 {
		t.Error("no read cards a task or a project")
	}
}
