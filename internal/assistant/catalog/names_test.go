package catalog_test

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/assistant/assistanttest"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/exec"
)

// namedWorld is [assistanttest.World] with the directories a name resolves
// against: two spaces (one named Company whose key is HOME), two projects and
// two seats sharing a name.
func namedWorld() map[string]any {
	world := assistanttest.World()
	world["/containers"] = map[string]any{"containers": []any{
		map[string]any{"key": "HOME", "name": "Company"},
		map[string]any{"key": "FAIR", "name": "Acme"}}}
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
// ("COMPANY", the defect this answers). Mutation: match names with case, drop
// the trailing-noun rule, or let a write through unresolved, and this fails.
func TestANameResolvesToItsKeyOrIsRefusedWithTheChoices(t *testing.T) {
	t.Parallel()
	page, _ := catalog.Lookup("create_page")
	tasks, _ := catalog.Lookup("create_task")
	for name, tc := range map[string]struct {
		tool       catalog.Tool
		args       map[string]any
		arg, want  string
		refusedFor string
	}{
		"a space by its name":               {page, map[string]any{"container": "the Company space"}, "container", "HOME", ""},
		"a space by its key in any case":    {page, map[string]any{"container": "fair"}, "container", "FAIR", ""},
		"a project by its name":             {tasks, map[string]any{"project": "marketing"}, "project", "MKT", ""},
		"a seat by its name":                {tasks, map[string]any{"assignee": "Product Manager"}, "assignee", "pm", ""},
		"a seat by its handle":              {tasks, map[string]any{"assignee": "pm"}, "assignee", "pm", ""},
		"a key guessed from a space's name": {page, map[string]any{"container": "COMPANY"}, "container", "HOME", ""},
		"a write to a space nobody declares": {page, map[string]any{"container": "WIKI"}, "", "",
			"The spaces are: Company (HOME), Acme (FAIR)."},
		"a write naming a seat that is not there": {tasks, map[string]any{"assignee": "Head of Fun"}, "",
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
	for _, v := range []string{"Company", "Acme", "HOME"} {
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
// Mutation: drop create_task's Names or send_direct_message's, or add a tool
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
// same way, and a task filed in a project and for an assignee neither of which
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
		"a project and an assignee": {"create_task", map[string]any{"title": "Plan the launch",
			"project": "Head office", "assignee": "Head of Fun"},
			[]string{`create_task.project: "Head office" is not one of the projects you can see`,
				`create_task.assignee: "Head of Fun" is not one of the seats you can see`},
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
