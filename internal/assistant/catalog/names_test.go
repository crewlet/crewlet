package catalog_test

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"reflect"
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
// Mutation: drop list_tasks' Names or send_direct_message's, or add a tool
// whose argument says it takes a handle and declare nothing, and this fails.
func TestEveryArgumentThatNamesARecordIsResolved(t *testing.T) {
	t.Parallel()
	for _, tool := range catalog.Tools() {
		props, _ := tool.Parameters["properties"].(map[string]any)
		for arg, raw := range props {
			described, _ := raw.(map[string]any)
			text, _ := described["description"].(string)
			lower := strings.ToLower(text)
			// A MENTION IS A MESSAGE'S OWN GRAMMAR (`@handle` in its words),
			// which the room reads; the words name no seat by themselves.
			names := strings.Contains(strings.ReplaceAll(lower, "@handle", ""), "handle") ||
				strings.Contains(lower, "project's key") || strings.Contains(lower, "space's key")
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

// A LIST RESOLVES NAME BY NAME, by the rule one name does: the people a direct
// message goes to, each a seat by its name or its handle, and a room's members,
// each naming its seat by `handle` beside how they follow the room, which is
// kept. A membership keeps a handle the chart no longer lists, since a seat
// removed from the chart is still in the rooms it was in, and the preview,
// which reads the room, decides it; a direct message refuses one, listing the
// seats. The call's own lists are never changed. Mutation: resolve strings
// alone, drop a member's follow_all, hold a member to the chart, or write into
// the call's own list, and this fails.
func TestEveryNameInAListResolves(t *testing.T) {
	t.Parallel()
	direct, _ := catalog.Lookup("send_direct_message")
	members, _ := catalog.Lookup("set_chat_room_members")
	r := catalog.NewResolver(reader(namedWorld()))

	to := []any{"Product Manager", "ada"}
	got, err := r.Resolve(t.Context(), direct, map[string]any{"to": to, "body": "Hi."})
	if err != nil || !reflect.DeepEqual(got["to"], []any{"pm", "ada"}) {
		t.Errorf("the people a message goes to resolve to %v (%v), want pm and ada", got["to"], err)
	}
	if to[0] != "Product Manager" {
		t.Errorf("the call's own list was changed: %v", to)
	}
	if _, err := r.Resolve(t.Context(), direct, map[string]any{"to": []any{"former-qa"}}); !errors.Is(err,
		catalog.ErrInvalidArguments) || !strings.Contains(err.Error(), "The seats are:") {
		t.Errorf("a message to a seat nobody holds answered %v, want the seats listed", err)
	}

	stated := []any{map[string]any{"handle": "Product Manager", "follow_all": true},
		map[string]any{"handle": "former-qa"}}
	got, err = r.Resolve(t.Context(), members, map[string]any{"channel": assistanttest.RecordID,
		"members": stated, "removes": []any{"Ada Lovelace"}})
	want := []any{map[string]any{"handle": "pm", "follow_all": true},
		map[string]any{"handle": "former-qa"}}
	if err != nil || !reflect.DeepEqual(got["members"], want) ||
		!reflect.DeepEqual(got["removes"], []any{"ada"}) {
		t.Errorf("a membership resolves to %v removing %v (%v), want %v removing ada",
			got["members"], got["removes"], err, want)
	}
	if stated[0].(map[string]any)["handle"] != "Product Manager" {
		t.Errorf("the call's own member was changed: %v", stated[0])
	}
	if _, err := r.Resolve(t.Context(), members, map[string]any{"members": []any{
		map[string]any{"handle": "Head of Fun"}}}); !errors.Is(err, catalog.ErrInvalidArguments) {
		t.Errorf("a member named by a name no seat has answered %v", err)
	}
}

// EVERY FAULT OF A CALL THAT NAMES RECORDS IS ONE REFUSAL (ADR-0059): every
// name the turn cannot place, each by its path, beside every fault the tool's
// schema finds, with each directory's choices listed once after them. A direct
// message to two people no seat is and an argument the tool does not take, a
// membership naming a member by a name nobody has beside a removal named the
// same way, and a list of tasks for a project and an assignee neither of which
// is there each come back in one answer. Resolving stopped at the first name it
// could not place, and ran before the schema's check, so three misnamed
// people, or a misnamed seat and a misspelt argument, cost an attempt each.
// Mutation: return at the first fault of a list or of a call, resolve before
// the schema's check, resolve a value the schema refused, or list a
// directory's choices per fault, and a case here fails.
func TestEveryFaultOfACallNamingRecordsIsOneRefusal(t *testing.T) {
	t.Parallel()
	r := catalog.NewResolver(reader(namedWorld()))
	for name, tc := range map[string]struct {
		tool  string
		args  map[string]any
		want  []string
		lists map[string]int
	}{
		"two people and an argument": {"send_direct_message", map[string]any{
			"to": []any{"Head of Fun", "pm", "Chief of Vibes"}, "body": "Hi.", "cc": "ada"},
			[]string{`send_direct_message.to[0]: "Head of Fun" is not one of the seats you can see`,
				`send_direct_message.to[2]: "Chief of Vibes" is not one of the seats you can see`,
				`send_direct_message takes no "cc"`},
			map[string]int{"The seats are:": 1}},
		"a member and a removal": {"set_chat_room_members", map[string]any{
			"channel": assistanttest.RecordID,
			"members": []any{map[string]any{"handle": "Head of Fun"}, map[string]any{"handle": "pm"}},
			"removes": []any{"Chief of Vibes", "ada"}},
			[]string{`set_chat_room_members.members[0].handle: "Head of Fun" is not one of the seats`,
				`set_chat_room_members.removes[0]: "Chief of Vibes" is not one of the seats`},
			map[string]int{"The seats are:": 1}},
		"a project and an assignee": {"list_tasks", map[string]any{
			"project": "Head office", "assignee": "Head of Fun"},
			[]string{`list_tasks.project: "Head office" is not one of the projects you can see`,
				`list_tasks.assignee: "Head of Fun" is not one of the seats you can see`},
			map[string]int{"The seats are:": 1, "The projects are:": 1}},
	} {
		tool := mustTool(t, tc.tool)
		_, err := r.Prepare(t.Context(), tool, tc.args)
		if !errors.Is(err, catalog.ErrInvalidArguments) {
			t.Errorf("%s: answered %v, want one refusal", name, err)
			continue
		}
		for _, want := range tc.want {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: the refusal does not say %q: %v", name, want, err)
			}
		}
		for list, n := range tc.lists {
			if got := strings.Count(err.Error(), list); got != n {
				t.Errorf("%s: the refusal says %q %d times, want %d: %v", name, list, got, n, err)
			}
		}
	}

	// A VALUE THE SCHEMA REFUSES IS NOT ALSO RESOLVED: a recipient given as a
	// number is the schema's fault alone, and the name beside it is still
	// placed.
	direct := mustTool(t, "send_direct_message")
	_, err := r.Prepare(t.Context(), direct, map[string]any{"to": []any{7.0, "Head of Fun"},
		"body": "Hi."})
	if err == nil || !strings.Contains(err.Error(), "send_direct_message.to[0] is a string") ||
		!strings.Contains(err.Error(), `to[1]: "Head of Fun" is not one of the seats`) ||
		strings.Contains(err.Error(), "to[0]:") {
		t.Errorf("a recipient of the wrong type beside a misnamed one answered %v", err)
	}
	// Nor is a name past the bound every name is held to: it is named once, by
	// its size, rather than echoed back whole as a seat nobody holds.
	_, err = r.Prepare(t.Context(), direct, map[string]any{
		"to": []any{strings.Repeat("Head of Fun ", 40)}, "body": "Hi."})
	if err == nil || !strings.Contains(err.Error(), "send_direct_message.to[0] is 480 bytes") ||
		strings.Contains(err.Error(), "to[0]:") {
		t.Errorf("a recipient past the bound answered %v", err)
	}
	got, err := r.Prepare(t.Context(), direct, map[string]any{"to": []any{"Product Manager"},
		"body": "Hi."})
	if err != nil || !reflect.DeepEqual(got["to"], []any{"pm"}) {
		t.Errorf("a call with nothing at fault prepared %v (%v), want pm", got["to"], err)
	}
}

// TWO READS OF ONE TOOL ARE TOLD APART on the step and the card, PROVED FOR
// EVERY READ rather than declared: the drawer keeps one read per title (a
// table's), or per record (an entity's kind and id), the freshest, so two
// reads it took for one lost the first (the C1 report: "Spend by agent" over
// seven days and over fourteen; two direct conversations both "Messages"; one
// search by two authors). So for every read and every argument that changes
// what it sends, other than a page's size: one that picks the card (a view, a
// section) is two cards, one of a table changes its title's scope (a room read
// by its id said as the turn says the room, a direct conversation by its
// people), and one of a record names the record. Mutation: drop get_spend's
// Scope or the title's use of it, search_chat's author, read_chat_messages'
// room, list_turns' seat or get_task_cost's task, or give a record read a
// paging argument, and this fails.
func TestTwoReadsOfOneToolAreToldApart(t *testing.T) {
	t.Parallel()
	// record are the arguments that name the record an entity read is of,
	// told apart by its own kind and id.
	record := map[string]bool{"item": true, "page": true, "artifact": true, "key": true,
		"handle": true, "channel": true, "message_id": true}
	said := cards.Options{Room: func(ref string) (cards.Room, bool) {
		return cards.Room{With: "the people of " + ref}, true
	}}
	for _, tool := range catalog.Tools() {
		if tool.Kind != catalog.KindRead || tool.Infer {
			continue
		}
		props, _ := tool.Parameters["properties"].(map[string]any)
		for _, v := range slices.Sorted(maps.Keys(tool.Cards)) {
			base := variantArgs(t, tool, v)
			for _, arg := range slices.Sorted(maps.Keys(props)) {
				if arg == "limit" {
					continue
				}
				one, two := twoValues(props[arg].(map[string]any))
				a, b := maps.Clone(base), maps.Clone(base)
				a[arg], b[arg] = one, two
				ra, errA := tool.Request(a)
				rb, errB := tool.Request(b)
				if errA != nil || errB != nil || variantOf(tool, a) != v || variantOf(tool, b) != v ||
					(ra.Path == rb.Path && ra.Query.Encode() == rb.Query.Encode()) {
					continue
				}
				name := tool.Name + "/" + v
				if tool.Cards[v].Entity != nil {
					if !record[arg] {
						t.Errorf("%s reads one record and %s changes what it asks without "+
							"naming another, so two reads of it are one record to the drawer",
							name, arg)
					}
					continue
				}
				if sa, sb := tool.ScopeOf(a, said), tool.ScopeOf(b, said); sa == sb {
					t.Errorf("%s asked with %s=%v and with %v is titled alike (%q), so the "+
						"drawer keeps one of the two", name, arg, one, two, sa)
				}
			}
		}
	}

	spend, _ := catalog.Lookup("get_spend")
	week := map[string]any{"days": float64(7)}
	fortnight := map[string]any{"days": float64(14)}
	if a, b := spend.StepLabel(week, cards.Options{}), spend.StepLabel(fortnight,
		cards.Options{}); a == b || a != "Reading token spend, last 7 days" {
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

// variantOf is the card variant a call of tool is answered with.
func variantOf(tool catalog.Tool, args map[string]any) string {
	if tool.Variant == nil {
		return ""
	}
	return tool.Variant(args)
}

// twoValues are two values an argument's schema takes, which differ: an enum's
// first and last member, the two lowest numbers, both truths, and two words or
// lists of one word.
func twoValues(prop map[string]any) (any, any) {
	switch kind, _ := prop["type"].(string); {
	case prop["enum"] != nil:
		enum := prop["enum"].([]string)
		return enum[0], enum[len(enum)-1]
	case kind == "integer" || kind == "number":
		lo, _ := prop["minimum"].(int)
		return float64(lo), float64(lo + 1)
	case kind == "boolean":
		return false, true
	case kind == "array":
		items, _ := prop["items"].(map[string]any)
		one, two := twoValues(items)
		return []any{one}, []any{two}
	}
	return "alpha", "bravo"
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
		`"error":"the harness never reached the seat's tools","error_kind":"server"}]}`)
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
// past it tells the model its count is a floor ("at least 5"), and the person,
// on the card itself (`more`), that there are more; one with no cursor is the
// whole. "Five turns in a row"
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
		marked := len(res.Cards) == 1 && res.Cards[0].Table.More
		if saysFloor != tc.more || marked != tc.more {
			t.Errorf("next %s: the model reads %s and the cards are %+v, want more=%v", tc.next,
				res.Model, res.Cards, tc.more)
		}
	}
	// A RECORD'S LIST IS A PAGE TOO: a thread whose answer has older
	// comments past it says so on the card and to the model.
	thread, _ := catalog.Lookup("get_task_thread")
	for _, tc := range []struct {
		next string
		more bool
	}{{`"b2xkZXI"`, true}, {`null`, false}} {
		answer := []byte(`{"key":"GP-11","title":"Crash","comments":[{"id":"c1","body":"Seen",` +
			`"author":"qa"},{"id":"c2","body":"Fixed","author":"dev"}],"next_cursor":` + tc.next + `}`)
		res, err := catalog.Shape(thread, map[string]any{"item": "GP-11"}, exec.Response{
			Status: http.StatusOK, Body: answer}, provenance.NewBoundary(), wednesday, cards.Options{})
		if err != nil {
			t.Fatal(err)
		}
		saysFloor := strings.Contains(res.Model, "at least 2")
		marked := len(res.Cards) == 1 && res.Cards[0].Entity.ItemsMore
		if saysFloor != tc.more || marked != tc.more {
			t.Errorf("thread next %s: the model reads %s and the cards are %+v, want more=%v",
				tc.next, res.Model, res.Cards, tc.more)
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

// EVERY TASK, PROJECT AND ROOM CARD NAMES ITS KEY, no seat card names one, and
// EVERY RECORD HEADED BY ITS OWN NAME IS OF A KIND: the drawer makes the
// answer's first mention of a record its chip, the model writes a task, a
// project or a room by its key and a seat by its name (the prompt's rule), so
// a seat keyed by its handle would never be found, and a record of no kind is
// never matched at all, as a chat room was before it had one. A heading of
// somebody's words (a thread by its first message, cut) names no record, and
// is the one heading a record of no kind may have. Mutation: drop Key from a
// task spec, key the seat detail by its handle, or card a room as it was
// carded before (no kind, no key), and this fails.
func TestARecordIsKeyedTheWayTheAnswerNamesIt(t *testing.T) {
	t.Parallel()
	keyed := 0
	for _, tool := range catalog.Tools() {
		for variant, spec := range tool.Cards {
			e := spec.Entity
			if e == nil {
				continue
			}
			if err := spec.Validate(); err != nil {
				t.Errorf("%s (%q): %v", tool.Name, variant, err)
			}
			if e.Title != "" && e.Kind == "" && e.TitleCut == 0 {
				t.Errorf("%s (%q) heads a record by its %s and gives it no kind, so the "+
					"answer's mention of it is never its chip", tool.Name, variant, e.Title)
			}
			switch {
			case e.Kind.Keyed():
				if e.Key == "" {
					t.Errorf("%s (%q) cards a %s with no key", tool.Name, variant, e.Kind)
				}
				keyed++
			case e.Kind == wire.ResourceSeat:
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
