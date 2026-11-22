package assistant_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/assistant/assistanttest"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/evals"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// The atlas the questions below are asked over: a Datadog account that
// monitors, a repository, a database and the service that depends on it.
const (
	atlasDatadogID = "4f0a2b96-7081-4293-8e3f-4a5b6c7d8e9f"
	atlasRepoID    = "5a1b3ca7-8192-4304-9f40-5b6c7d8e9fa0"
	atlasDBID      = "6b2c4db8-92a3-4415-8051-6c7d8e9fa0b1"
	atlasAPIID     = "7c3d5ec9-a3b4-4526-9162-7d8e9fa0b1c2"
)

func atlasSummary(id, kind, lane, key, name string) map[string]any {
	return map[string]any{"id": id, "kind": kind, "lane": lane, "name": name, "key": key,
		"state": "confirmed", "word": "confirmed", "health": "quiet", "signals": 0, "review": false,
		"not_used_recently": false, "last_seen": "2026-11-11T09:00:00Z"}
}

func atlasList(rows ...map[string]any) exec.Response {
	list := make([]any, 0, len(rows))
	for _, r := range rows {
		list = append(list, r)
	}
	return evals.JSON(map[string]any{"revision": "r1", "coverage": []any{}, "withheld": 0,
		"rows": list, "total": len(list)})
}

// atlasRoutes answer the three questions' reads as the atlas's routes do: a
// search of accounts, of repositories and of databases, the Datadog account
// with its capabilities, and what depends on the database.
func atlasRoutes() func(exec.Request) exec.Response {
	datadog := atlasSummary(atlasDatadogID, "vendor_account", "platforms", "datadog/acme", "Datadog")
	repo := atlasSummary(atlasRepoID, "repository", "code", "github/acme/api", "acme/api")
	db := atlasSummary(atlasDBID, "datastore", "data", "orders-db", "orders-db")
	api := atlasSummary(atlasAPIID, "service", "services", "checkout-api", "checkout-api")
	byKind := func(r exec.Request) exec.Response {
		switch r.Query.Get("kind") {
		case "vendor_account":
			return atlasList(datadog)
		case "repository":
			return atlasList(repo)
		case "datastore":
			return atlasList(db)
		}
		return atlasList(datadog, repo, db, api)
	}
	entry := evals.JSON(map[string]any{"revision": "r1", "coverage": []any{}, "withheld": 0,
		"health": "quiet", "vendor": "datadog", "used_by": []any{},
		"entry": map[string]any{"id": atlasDatadogID, "ref": map[string]any{"kind": "vendor_account",
			"key": "datadog/acme"}, "lane": "platforms", "name": "Datadog", "state": "confirmed",
			"word": "confirmed", "last_seen": "2026-11-11T09:00:00Z",
			"members": []any{map[string]any{"kind": "vendor_account", "key": "datadog/acme"}},
			"facts": []any{
				map[string]any{"fact": "capabilities", "value": map[string]any{"type": "word", "text": "monitoring"},
					"source": map[string]any{"kind": "settings", "revision": "r1"}, "word": "confirmed",
					"standing": "settled"},
				map[string]any{"fact": "capabilities", "value": map[string]any{"type": "word", "text": "alerting"},
					"source": map[string]any{"kind": "settings", "revision": "r1"}, "word": "confirmed",
					"standing": "settled"}},
			"relations": []any{}, "sources": []any{map[string]any{"kind": "settings", "revision": "r1"}}}})
	trace := evals.JSON(map[string]any{"revision": "r1", "coverage": []any{}, "withheld": 0,
		"from": db, "direction": "up",
		"hops":      []any{map[string]any{"distance": 1, "entries": []any{api}}},
		"edges":     []any{map[string]any{"from": atlasAPIID, "to": atlasDBID, "relation": "depends_on"}},
		"truncated": false})
	routes := assistanttest.Routes(map[string]exec.Response{
		"GET /atlas/" + atlasDatadogID:       entry,
		"GET /atlas/" + atlasDBID + "/trace": trace,
	})
	return func(r exec.Request) exec.Response {
		if r.Method == "GET" && r.Path == "/atlas" {
			return byKind(r)
		}
		return routes(r)
	}
}

// atlasTools are the generic read over the atlas's three routes.
func atlasTools(t *testing.T) []catalog.Tool {
	return []catalog.Tool{assistanttest.ReadAPI(t, "GET /atlas", "GET /atlas/{id}",
		"GET /atlas/{id}/trace")}
}

// atlasRead is a read_api call on one of the atlas's routes.
func atlasRead(id, route string, path, query map[string]any) evals.Round {
	args := map[string]any{"route": route}
	if path != nil {
		args["path"] = path
	}
	if query != nil {
		args["query"] = query
	}
	return evals.Call(id, catalog.ReadAPITool, args)
}

// cardsOf are the turn's cards, in order.
func cardsOf(frames []wire.Frame) []wire.Card {
	var out []wire.Card
	for _, fr := range frames {
		if fr.Kind == wire.EventCard && fr.Card != nil {
			out = append(out, fr.Card.Card)
		}
	}
	return out
}

// toolResults are what the model was handed for each call, in order.
func toolResults(f *assistanttest.Rig) []string {
	var out []string
	for _, m := range f.Model.Seen() {
		if m.Role == llm.RoleTool {
			out = append(out, m.Content)
		}
	}
	return out
}

// "WHAT DO WE USE FOR MONITORING?" IS ANSWERED FROM THE ATLAS: the accounts are
// searched, the one that monitors is read by its id with only the fields asked,
// and the model is handed its capabilities. The search is fenced as outside
// text, since an integration's users name its rows. Mutation: read the atlas
// list at the company's own trust, or ignore select, and this fails.
func TestWhatWeUseForMonitoringIsAnsweredFromTheAtlas(t *testing.T) {
	t.Parallel()
	person := evals.Person("grace", "grace", iam.GrantAtlas)
	f := assistanttest.NewRig(t, evals.RigOptions{Tools: atlasTools(t), Handler: atlasRoutes(),
		Person: &person, Rounds: []evals.Round{
			atlasRead("c1", "GET /atlas", nil, map[string]any{"kind": "vendor_account"}),
			evals.Call("c2", catalog.ReadAPITool, map[string]any{"route": "GET /atlas/{id}",
				"path":   map[string]any{"id": atlasDatadogID},
				"select": []any{"entry.name", "entry.facts.*.value.text"}}),
			evals.Say("Datadog: its account carries monitoring and alerting."),
		}})
	accepted, err := f.Ask("monitoring", "What do we use for monitoring?", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.Follow(accepted.TurnID, 0)
	sent := f.Dispatcher.Sent()
	if len(sent) != 2 || sent[0].Request.Path != "/atlas" ||
		sent[0].Request.Query.Get("kind") != "vendor_account" ||
		sent[1].Request.Path != "/atlas/"+atlasDatadogID {
		t.Fatalf("sent %+v, want the search then the entry by its id", sent)
	}
	results := toolResults(f)
	if len(results) != 2 {
		t.Fatalf("the model was handed %d results, want 2", len(results))
	}
	if !strings.Contains(results[0], "trust=external") {
		t.Errorf("the atlas search reached the model as %q, want it fenced as outside text",
			results[0])
	}
	if !strings.Contains(results[1], "monitoring") || !strings.Contains(results[1], "alerting") ||
		strings.Contains(results[1], "last_seen") {
		t.Errorf("the model was handed %q, want the selected capabilities alone", results[1])
	}
}

// "WHERE DOES OUR CODE LIVE?" IS THE REPOSITORIES OF THE ATLAS, drawn as one
// card of the row the search answered. Mutation: answer the search with no row
// and no card is drawn.
func TestWhereOurCodeLivesIsAnsweredFromTheAtlas(t *testing.T) {
	t.Parallel()
	person := evals.Person("grace", "grace", iam.GrantAtlas)
	f := assistanttest.NewRig(t, evals.RigOptions{Tools: atlasTools(t), Handler: atlasRoutes(),
		Person: &person, Rounds: []evals.Round{
			atlasRead("c1", "GET /atlas", nil, map[string]any{"kind": "repository"}),
			evals.Say("The code lives in acme/api on GitHub."),
		}})
	accepted, err := f.Ask("code", "Where does our code live?", nil)
	if err != nil {
		t.Fatal(err)
	}
	if cards := cardsOf(f.Follow(accepted.TurnID, 0)); len(cards) != 1 {
		t.Fatalf("the search drew %+v, want one card of the repository", cards)
	}
	if results := toolResults(f); len(results) != 1 || !strings.Contains(results[0], "acme/api") {
		t.Errorf("the model was handed %q, want the repository", results)
	}
}

// "WHAT DEPENDS ON THE DATABASE?" IS A TRACE UP FROM IT: the databases are
// searched and the one found is traced up by its id, and the model is handed
// what relies on it, fenced as outside text. Mutation: drop the direction from
// the trace's query and the dependent is never read.
func TestWhatDependsOnTheDatabaseIsAnsweredFromTheAtlas(t *testing.T) {
	t.Parallel()
	person := evals.Person("grace", "grace", iam.GrantAtlas)
	f := assistanttest.NewRig(t, evals.RigOptions{Tools: atlasTools(t), Handler: atlasRoutes(),
		Person: &person, Rounds: []evals.Round{
			atlasRead("c1", "GET /atlas", nil, map[string]any{"kind": "datastore"}),
			atlasRead("c2", "GET /atlas/{id}/trace", map[string]any{"id": atlasDBID},
				map[string]any{"direction": "up"}),
			evals.Say("checkout-api depends on orders-db."),
		}})
	accepted, err := f.Ask("database", "What depends on the database?", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.Follow(accepted.TurnID, 0)
	sent := f.Dispatcher.Sent()
	if len(sent) != 2 || sent[1].Request.Path != "/atlas/"+atlasDBID+"/trace" ||
		sent[1].Request.Query.Get("direction") != "up" {
		t.Fatalf("sent %+v, want the trace up from the database", sent)
	}
	results := toolResults(f)
	if len(results) != 2 || !strings.Contains(results[1], "checkout-api") ||
		!strings.Contains(results[1], "trust=external") {
		t.Errorf("the model was handed %q, want the dependent, fenced as outside text", results)
	}
}

// A PERSON WITHOUT THE ATLAS GETS THE LOCKED ANSWER, NEVER THE DATA: the route
// refuses their read, so the step is refused, no card is drawn and the model
// reads the refusal and no row. Mutation: draw a refused read's body and the
// row reaches the person.
func TestAPersonWithoutTheAtlasIsNeverHandedIt(t *testing.T) {
	t.Parallel()
	person := evals.Person("grace", "grace", iam.GrantWorklet)
	locked := func(exec.Request) exec.Response {
		return exec.Response{Status: http.StatusForbidden, Body: []byte(
			`{"error":"forbidden","message":"Reading the atlas needs the atlas permission.",` +
				`"rows":[{"name":"orders-db"}]}`)}
	}
	f := assistanttest.NewRig(t, evals.RigOptions{Tools: atlasTools(t), Handler: locked,
		Person: &person, Rounds: []evals.Round{
			atlasRead("c1", "GET /atlas", nil, map[string]any{"kind": "datastore"}),
			evals.Say("I cannot read the atlas for you."),
		}})
	accepted, err := f.Ask("locked", "What depends on the database?", nil)
	if err != nil {
		t.Fatal(err)
	}
	frames := f.Follow(accepted.TurnID, 0)
	for _, c := range cardsOf(frames) {
		if c.Type != wire.CardError {
			t.Errorf("a person without the atlas was drawn %+v", c)
		}
	}
	if step := evals.Last(frames, wire.EventStep); step == nil || step.Step.Refusal == nil {
		t.Errorf("step = %+v, want the route's refusal", step)
	}
	if results := toolResults(f); len(results) != 1 || strings.Contains(results[0], "orders-db") {
		t.Errorf("the model was handed %q, want the refusal and no row", results)
	}
}
