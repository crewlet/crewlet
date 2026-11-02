package catalog_test

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/assistant/cards"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/provenance"
)

// readRoutes are the generic read routes the answers below come from.
var readRoutes = []catalog.Route{
	{Pattern: "GET /agents"},
	{Pattern: "GET /agents/{id}"},
	{Pattern: "GET /agents/{id}/memory"},
	{Pattern: "GET /config"},
	{Pattern: "GET /config/revisions"},
	{Pattern: "GET /config/roles/{id}"},
	{Pattern: "GET /events"},
	{Pattern: "GET /mcp-servers"},
	{Pattern: "GET /tokens/breakdown"},
	{Pattern: "GET /worklet/files/{project}/{path...}"},
	{Pattern: "GET /worklet/projects/{key}"},
	// A change, which the generic tools are built over too.
	{Pattern: "PUT /config/roles/{id}"},
}

// realAnswers are real routes' answers, in the shapes their handlers write:
// the example company's document as GET /config encodes it, one of its roles
// as GET /config/{kind}/{id} answers the entity itself, the roster's rows, a
// page of the event log, and a project in full.
func realAnswers(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("../../../examples/nimbus.company.yaml")
	if err != nil {
		t.Fatal(err)
	}
	company, err := config.ParseCompanyDocument(raw)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip := func(v any) any {
		encoded, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		var out any
		if err := json.Unmarshal(encoded, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if len(company.Roles) == 0 {
		t.Fatal("the example company holds no role outside a unit to read")
	}
	return map[string]any{
		"GET /config":            roundTrip(company.Redact()),
		"GET /config/roles/{id}": roundTrip(company.Roles[0]),
		"GET /agents": []any{
			map[string]any{"id": "ceo", "agent_id": "8d1c2f0e-3b7a-5c41-9e6d-2a4b8f1c0d93",
				"role": "Chief Executive", "handle": "ceo", "status": "idle"},
			map[string]any{"id": "eng-lead", "agent_id": "1f9a3e57-6c2d-5b80-a4e1-7d3c9b2f6a08",
				"role": "Engineering Lead", "handle": "eng-lead", "status": "working",
				"live_call": map[string]any{"tool": "worklet_search", "round": 3}},
		},
		"GET /events": map[string]any{
			"events": []any{
				map[string]any{"id": "e1", "type": "agent.turn.completed", "source": "ceo",
					"time": "2026-10-24T09:00:00Z"},
				map[string]any{"id": "e2", "type": "work.item.created", "source": "eng-lead",
					"time": "2026-10-24T09:01:00Z"},
			},
			"next":      map[string]any{"before_time": "2026-10-24T09:01:00Z", "before_id": "e2"},
			"exhausted": false,
			"coverage":  map[string]any{"answered": []any{"node-a"}, "missing": []any{}},
		},
		"GET /worklet/projects/{key}": map[string]any{
			"key": "ENG", "name": "Engineering", "description": "Everything the platform team ships.",
			"statuses": []any{
				map[string]any{"key": "todo", "label": "To do", "group": "todo"},
				map[string]any{"key": "doing", "label": "In progress", "group": "active"},
				map[string]any{"key": "done", "label": "Done", "group": "done"},
			},
			"task_counts": map[string]any{"todo": 4, "active": 2, "done": 9, "closed": 1},
		},
	}
}

// A GENERIC READ IS CARDED AS THE THING IT READS, titled for it.
//
// The company document is ONE record: an entity card whose facts are its own
// values and whose list is its roles and its units, never "GET /config · 2
// rows" of whichever list was longer. One role is an entity titled "Role ·
// <handle>"; the roster is a table titled "Agents"; a page of the event log,
// a list beside a cursor, is the table of its events; a project in full,
// which names itself, is one record over its statuses. Mutation: draw a record
// by its largest list again, or title a card by its route pattern, and a case
// here fails.
func TestAGenericReadIsCardedAsTheThingItReads(t *testing.T) {
	t.Parallel()
	table, err := catalog.NewRouteTable(readRoutes)
	if err != nil {
		t.Fatal(err)
	}
	read := catalog.GenericTools(table, admin, nil)[1]
	answers := realAnswers(t)
	role := answers["GET /config/roles/{id}"].(map[string]any)
	handle, _ := role["handle"].(string)
	if handle == "" {
		handle, _ = role["name"].(string)
	}
	for _, c := range []struct {
		route string
		path  map[string]any
		kind  wire.CardKind
		title string
		check func(t *testing.T, card wire.Card)
	}{
		{route: "GET /config", kind: wire.CardEntity, title: "Company configuration",
			check: func(t *testing.T, card wire.Card) {
				e := card.Entity
				if !slices.ContainsFunc(e.Facts, func(f wire.Fact) bool { return f.Label == "Name" }) {
					t.Errorf("the company's name is not one of its facts: %+v", e.Facts)
				}
				lists := map[string]bool{}
				for _, item := range e.Items {
					lists[item.Fact] = true
				}
				if !lists["Roles"] || !lists["Units"] {
					t.Errorf("the document's list does not hold both its roles and its units: %+v",
						e.Items)
				}
			}},
		{route: "GET /config/roles/{id}", path: map[string]any{"id": handle},
			kind: wire.CardEntity, title: "Seat · " + handle,
			check: func(t *testing.T, card wire.Card) {
				if len(card.Entity.Facts) == 0 {
					t.Error("the role is drawn with no facts")
				}
			}},
		{route: "GET /agents", kind: wire.CardTable, title: "Agents",
			check: func(t *testing.T, card wire.Card) {
				if card.Table.RowCount != 2 {
					t.Errorf("the roster has %d rows, want 2", card.Table.RowCount)
				}
			}},
		{route: "GET /events", kind: wire.CardTable, title: "Events",
			check: func(t *testing.T, card wire.Card) {
				if card.Table.RowCount != 2 {
					t.Errorf("the page has %d rows, want its 2 events", card.Table.RowCount)
				}
			}},
		{route: "GET /worklet/projects/{key}", path: map[string]any{"key": "ENG"},
			kind: wire.CardEntity, title: "Project · ENG",
			check: func(t *testing.T, card wire.Card) {
				e := card.Entity
				if len(e.Items) != 3 || e.ItemsNoun == nil || e.ItemsNoun.One != "status" {
					t.Errorf("the project's statuses are not its list, counted as statuses: %+v %+v",
						e.Items, e.ItemsNoun)
				}
				if !slices.ContainsFunc(e.Facts, func(f wire.Fact) bool {
					return f.Label == "Task counts › active" && f.Value == "2"
				}) {
					t.Errorf("a nested record's values are not facts beneath its name: %+v", e.Facts)
				}
			}},
	} {
		t.Run(c.route, func(t *testing.T) {
			t.Parallel()
			body, err := json.Marshal(answers[c.route])
			if err != nil {
				t.Fatal(err)
			}
			args := catalog.Args{"route": c.route}
			if c.path != nil {
				args["path"] = c.path
			}
			res, err := catalog.Shape(read, args, exec.Response{Status: 200, Body: body},
				provenance.NewBoundary(), time.Now(), cards.Options{})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Cards) == 0 {
				t.Fatal("the answer drew no card")
			}
			card := res.Cards[0]
			if card.Type != c.kind {
				t.Fatalf("the answer is a %s card, want %s", card.Type, c.kind)
			}
			title := ""
			switch card.Type {
			case wire.CardTable:
				title = card.Table.Title
			case wire.CardEntity:
				title = card.Entity.Title
			}
			if title != c.title {
				t.Errorf("the card is titled %q, want %q", title, c.title)
			}
			if strings.Contains(title, "{") || strings.Contains(title, "GET ") {
				t.Errorf("the card is titled by its route pattern: %q", title)
			}
			if err := card.Validate(); err != nil {
				t.Fatal(err)
			}
			c.check(t, card)
		})
	}
}

// A GENERIC READ IS FENCED AT THE TRUST ITS ROUTE EARNS (ADR-0040): the event
// log, which carries what integrations and vendors' chats sent, is external,
// and the company's own document a colleague's, by the one rule for a surface
// the engine hosts; an answer with no body carries nobody's words, and is the
// read's own trust whatever its route. Mutation: drop the generic read's
// TrustOf, or read the event log by the hosted rule, and the event log reads as
// a colleague's; label the read itself external and the empty answer does.
func TestAGenericReadIsFencedAtTheTrustItsRouteEarns(t *testing.T) {
	t.Parallel()
	table, err := catalog.NewRouteTable(readRoutes)
	if err != nil {
		t.Fatal(err)
	}
	read := catalog.GenericTools(table, admin, nil)[1]
	answers := realAnswers(t)
	for _, c := range []struct {
		route string
		body  bool
		want  provenance.Trust
	}{
		{"GET /config", true, provenance.TrustColleague},
		{"GET /events", true, provenance.TrustExternal},
		{"HEAD /events", false, provenance.TrustColleague},
	} {
		resp := exec.Response{Status: 200}
		if c.body {
			body, err := json.Marshal(answers[c.route])
			if err != nil {
				t.Fatal(err)
			}
			resp.Body = body
		}
		res, err := catalog.Shape(read, catalog.Args{"route": c.route}, resp,
			provenance.NewBoundary(), time.Now(), cards.Options{})
		if err != nil {
			t.Fatalf("%s: %v", c.route, err)
		}
		if res.Trust != c.want {
			t.Errorf("%s is read at %q, want %q", c.route, res.Trust, c.want)
		}
		if c.body && !strings.Contains(res.Model, "trust="+string(c.want)) {
			t.Errorf("%s is not fenced at %q:\n%.300s", c.route, c.want, res.Model)
		}
	}
}

// A GENERIC REQUEST IS TITLED BY ITS PATH, by one rule for the card and the
// step: the last literal, singular before the wildcard that picks one of it,
// the literal above it for a fixed resource two deep, and the values the call
// filled in. Mutation: drop the singular, the qualifier or the values, or
// leave a credential in a value, and a case here fails.
func TestAGenericRequestIsTitledByWhatItReaches(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		pattern string
		path    map[string]string
		want    string
	}{
		{"GET /config", nil, "Company configuration"},
		{"GET /agents", nil, "Agents"},
		{"HEAD /agents", nil, "Agents"},
		{"GET /config/roles/{id}", map[string]string{"id": "engineer"}, "Seat · engineer"},
		{"PUT /config/roles/{id}", map[string]string{"id": "crewlet-swe"}, "Seat · crewlet-swe"},
		{"GET /agents/{id}/memory", map[string]string{"id": "ceo"}, "Memory · ceo"},
		{"GET /mcp-servers", nil, "MCP servers"},
		{"GET /tokens/breakdown", nil, "Tokens breakdown"},
		{"GET /config/revisions", nil, "Configuration revisions"},
		{"GET /artifacts/{id}/data/export.csv", map[string]string{"id": "a1"}, "Export CSV · a1"},
		{"GET /worklet/files/{project}/{path...}",
			map[string]string{"project": "ENG", "path": "docs/plan.md"}, "File · ENG › docs/plan.md"},
		{"GET /worklet/people/{handle}", map[string]string{"handle": "ada"}, "Person · ada"},
		{"GET /worklet/statuses/{key}", map[string]string{"key": "todo"}, "Status · todo"},
		{"GET /secrets/{name}", map[string]string{"name": secret}, "Secret · [REDACTED]"},
	} {
		got := catalog.RequestTitle(c.pattern, c.path)
		if c.want == "Secret · [REDACTED]" {
			if strings.Contains(got, secret) || !strings.HasPrefix(got, "Secret · ") {
				t.Errorf("RequestTitle(%q) = %q, which carries the credential", c.pattern, got)
			}
			continue
		}
		if got != c.want {
			t.Errorf("RequestTitle(%q, %v) = %q, want %q", c.pattern, c.path, got, c.want)
		}
	}
	_, read, change := generic(t, admin)
	args := map[string]any{"route": "GET /config/units/{id}", "path": map[string]any{"id": "eng"}}
	if got := read.StepLabel(args, cards.Options{}); got != "Reading unit · eng" {
		t.Errorf("a read's step is labelled %q, want it titled as its card is", got)
	}
	args["route"] = "PUT /config/units/{id}"
	if got := change.StepLabel(args, cards.Options{}); got != "Preparing unit · eng" {
		t.Errorf("a change's step is labelled %q, want it titled as its request", got)
	}
}
