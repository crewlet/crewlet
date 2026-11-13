package assistant_test

import (
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

// atlasTools are the three reads, as the catalogue declares them.
func atlasTools(t *testing.T) []catalog.Tool {
	return []catalog.Tool{assistanttest.Tool(t, "search_atlas"), assistanttest.Tool(t, "get_atlas_entry"),
		assistanttest.Tool(t, "trace_atlas_entry")}
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

// "WHAT DO WE USE FOR MONITORING?" IS ANSWERED FROM THE ATLAS: the accounts
// are searched, the one that monitors is read, and the answer names it. The
// account is a chip (an entity card of the frame's atlas object) whose link
// opens the map on it; the model was handed its capabilities. Mutation: card
// the entry under no kind, or read its facts from the wrong key, and this
// fails.
func TestWhatWeUseForMonitoringIsAnsweredFromTheAtlas(t *testing.T) {
	t.Parallel()
	person := evals.Person("grace", "grace", iam.GrantAtlas)
	f := assistanttest.NewRig(t, evals.RigOptions{Tools: atlasTools(t), Handler: atlasRoutes(),
		Person: &person, Rounds: []evals.Round{
			evals.Call("c1", "search_atlas", map[string]any{"kind": "vendor_account"}),
			evals.Call("c2", "get_atlas_entry", map[string]any{"entry": "Datadog"}),
			evals.Say("Datadog: its account carries monitoring and alerting."),
		}})
	accepted, err := f.Ask("monitoring", "What do we use for monitoring?", nil)
	if err != nil {
		t.Fatal(err)
	}
	frames := f.Follow(accepted.TurnID, 0)
	var paths []string
	for _, s := range f.Dispatcher.Sent() {
		paths = append(paths, s.Request.Path+"?"+s.Request.Query.Encode())
	}
	// THE NAME IS RESOLVED THROUGH THE ATLAS'S OWN SEARCH, then the entry
	// read by its id.
	want := "/atlas?kind=vendor_account&limit=50 /atlas?limit=500&q=Datadog /atlas/" + atlasDatadogID + "?"
	if strings.Join(paths, " ") != want {
		t.Errorf("sent %v, want %q", paths, want)
	}
	var entry *wire.EntityCard
	for _, c := range cardsOf(frames) {
		if c.Type == wire.CardEntity {
			entry = c.Entity
		}
	}
	if entry == nil || entry.Kind != wire.ResourceAtlasEntry || entry.ID != atlasDatadogID ||
		entry.Title != "Datadog" {
		t.Fatalf("the account is carded as %+v, want the frame's atlas object", entry)
	}
	if entry.Open == nil || entry.Open.Label != "Show on the map" ||
		entry.Open.Href != "#/company/atlas?peek=atlas%3A"+atlasDatadogID {
		t.Errorf("the account opens %+v, want the map with its peek open", entry.Open)
	}
	results := toolResults(f)
	if len(results) != 2 || !strings.Contains(results[1], "monitoring") || !strings.Contains(results[1], "alerting") {
		t.Errorf("the model was handed %q, want the account's capabilities", results)
	}
}

// "WHERE DOES OUR CODE LIVE?" IS THE REPOSITORIES OF THE ATLAS, as one table
// titled by what narrowed it, with the atlas to open. Mutation: drop the
// scope from the title and this fails.
func TestWhereOurCodeLivesIsAnsweredFromTheAtlas(t *testing.T) {
	t.Parallel()
	person := evals.Person("grace", "grace", iam.GrantAtlas)
	f := assistanttest.NewRig(t, evals.RigOptions{Tools: atlasTools(t), Handler: atlasRoutes(),
		Person: &person, Rounds: []evals.Round{
			evals.Call("c1", "search_atlas", map[string]any{"kind": "repository"}),
			evals.Say("The code lives in acme/api on GitHub."),
		}})
	accepted, err := f.Ask("code", "Where does our code live?", nil)
	if err != nil {
		t.Fatal(err)
	}
	cards := cardsOf(f.Follow(accepted.TurnID, 0))
	if len(cards) != 1 || cards[0].Type != wire.CardTable {
		t.Fatalf("the search drew %+v, want one table", cards)
	}
	table := cards[0].Table
	if table.Title != "Atlas entries, repository" || table.RowCount != 1 ||
		table.Open == nil || table.Open.Href != "#/company/atlas" {
		t.Errorf("the repositories are carded as %+v", table)
	}
	if results := toolResults(f); len(results) != 1 || !strings.Contains(results[0], "acme/api") {
		t.Errorf("the model was handed %q, want the repository", results)
	}
}

// "WHAT DEPENDS ON THE DATABASE?" IS A TRACE UP FROM IT: the databases are
// searched, the one found is traced by its name, and the answer lists what
// relies on it under the database's own chip, with the map to show the same.
// Mutation: link the trace without its direction and this fails.
func TestWhatDependsOnTheDatabaseIsAnsweredFromTheAtlas(t *testing.T) {
	t.Parallel()
	person := evals.Person("grace", "grace", iam.GrantAtlas)
	f := assistanttest.NewRig(t, evals.RigOptions{Tools: atlasTools(t), Handler: atlasRoutes(),
		Person: &person, Rounds: []evals.Round{
			evals.Call("c1", "search_atlas", map[string]any{"kind": "datastore"}),
			evals.Call("c2", "trace_atlas_entry", map[string]any{"entry": "orders-db", "direction": "up"}),
			evals.Say("checkout-api depends on orders-db."),
		}})
	accepted, err := f.Ask("database", "What depends on the database?", nil)
	if err != nil {
		t.Fatal(err)
	}
	frames := f.Follow(accepted.TurnID, 0)
	var trace *wire.EntityCard
	for _, c := range cardsOf(frames) {
		if c.Type == wire.CardEntity {
			trace = c.Entity
		}
	}
	if trace == nil || trace.Kind != wire.ResourceAtlasEntry || trace.ID != atlasDBID ||
		trace.Title != "orders-db" || len(trace.Items) != 1 || trace.Items[0].Label != "checkout-api" ||
		trace.Items[0].Depth != 1 {
		t.Fatalf("the trace is carded as %+v, want the database with its dependent beneath", trace)
	}
	if trace.Open == nil || trace.Open.Label != "Show on the map" ||
		trace.Open.Href != "#/company/atlas?peek=atlas%3A"+atlasDBID+"&trace=up" {
		t.Errorf("the trace opens %+v, want the map lit along it", trace.Open)
	}
	if results := toolResults(f); len(results) != 2 || !strings.Contains(results[1], "checkout-api") {
		t.Errorf("the model was handed %q, want the dependent", results)
	}
}

// A PERSON WITHOUT THE ATLAS GETS THE LOCKED ANSWER, NEVER THE DATA: the
// reads are not offered to them, a model that asks for one anyway is told it
// was not run, no request reaches the atlas and no card is drawn. Mutation:
// offer the reads on no app and the search is sent as the person.
func TestAPersonWithoutTheAtlasIsNeverHandedIt(t *testing.T) {
	t.Parallel()
	person := evals.Person("grace", "grace", iam.GrantWorklet)
	f := assistanttest.NewRig(t, evals.RigOptions{Catalogue: catalog.Curated{}, Handler: atlasRoutes(),
		Person: &person, Rounds: []evals.Round{
			evals.Call("c1", "search_atlas", map[string]any{"kind": "datastore"}),
			evals.Say("I cannot read the atlas for you."),
		}})
	accepted, err := f.Ask("locked", "What depends on the database?", nil)
	if err != nil {
		t.Fatal(err)
	}
	frames := f.Follow(accepted.TurnID, 0)
	for _, s := range f.Dispatcher.Sent() {
		if strings.HasPrefix(s.Request.Path, "/atlas") {
			t.Errorf("the atlas was read as a person without it: %s", s.Request.Path)
		}
	}
	if cards := cardsOf(frames); len(cards) != 0 {
		t.Errorf("a person without the atlas was drawn %+v", cards)
	}
	if results := toolResults(f); len(results) != 1 || !strings.Contains(results[0], "No such tool") {
		t.Errorf("the model was handed %q, want that there is no such tool for them", results)
	}
}
