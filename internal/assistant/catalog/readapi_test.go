package catalog_test

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/assistant/cards"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/evals"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/provenance"
)

// listedRoutes are reads on verbs of every kind the listing decides by: an
// app's, an administrator's, a seat's own, one the assistant only proposes,
// and one decided inside its handler, beside a change.
var listedRoutes = []catalog.Route{
	{Pattern: "GET /worklet/{id}", Action: authz.ActionWorkRead},
	{Pattern: "GET /worklet/projects/{key}", Action: authz.ActionProjectRead},
	{Pattern: "GET /worklet/odd", Action: authz.ActionWorkCreate},
	{Pattern: "GET /chatlet/channels", Action: authz.ActionChatRead},
	{Pattern: "GET /config", Action: authz.ActionConfigRead},
	{Pattern: "GET /iam/audit", Action: authz.ActionAuditRead},
	{Pattern: "GET /agents/{id}/sessions", Action: authz.ActionSessionsList},
	{Pattern: "GET /query/{what}"},
	{Pattern: "POST /worklet/items", Action: authz.ActionWorkCreate, Effect: authz.EffectCreates},
}

// person is a person at their keyboard holding grants.
func person(grants ...iam.Grant) iam.Principal {
	return iam.Principal{Kind: iam.KindPerson, Stage: iam.StageActive, Grants: grants}
}

// DESCRIBE_API LISTS ONLY WHAT THE PERSON MAY READ: a read on an app they do not
// hold or an administrator's read to anybody else is left out, a verb only a
// seat takes or one the assistant only proposes is never listed as a read, and
// the domain map names only domains holding a read they may make. Mutation:
// list every read route, or drop the assist class check, and this fails.
func TestDescribeAPIListsOnlyWhatThePersonMayRead(t *testing.T) {
	t.Parallel()
	table, err := catalog.NewRouteTable(listedRoutes)
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		p       iam.Principal
		reads   []string
		domains []string
		hidden  []string
	}{
		"a tracker": {person(iam.GrantWorklet), []string{"GET /query/{what}",
			"GET /worklet/projects/{key}", "GET /worklet/{id}"},
			[]string{"query", "worklet"}, []string{"config", "chatlet", "iam", "agents"}},
		"a chat user": {person(iam.GrantChatlet), []string{"GET /chatlet/channels",
			"GET /query/{what}"}, []string{"chatlet", "query"},
			[]string{"config", "worklet", "iam", "agents"}},
		"an administrator": {person(iam.GrantAdmin), []string{"GET /chatlet/channels",
			"GET /config", "GET /iam/audit", "GET /query/{what}", "GET /worklet/projects/{key}",
			"GET /worklet/{id}"}, []string{"chatlet", "config", "iam", "query", "worklet"},
			[]string{"agents"}},
	} {
		if got := catalog.ListedReads(table, c.p); !slices.Equal(got, c.reads) {
			t.Errorf("%s is listed the reads %v, want %v", name, got, c.reads)
		}
		docs := catalog.GenericTools(table, c.p, nil)[0]
		domainMap, err := docs.Docs(catalog.Args{})
		if err != nil {
			t.Fatal(err)
		}
		for _, domain := range c.domains {
			if !strings.Contains(domainMap, "- "+domain+": ") ||
				!strings.Contains(docs.Description, "- "+domain+": ") {
				t.Errorf("%s's domain map does not name %s:\n%s", name, domain, domainMap)
			}
		}
		for _, domain := range c.hidden {
			if strings.Contains(domainMap, "- "+domain+": ") ||
				strings.Contains(docs.Description, "- "+domain+": ") {
				t.Errorf("%s's domain map names %s:\n%s", name, domain, domainMap)
			}
		}
		for _, never := range []string{"GET /worklet/odd", "GET /agents/{id}/sessions"} {
			listing, _ := docs.Docs(catalog.Args{"domain": catalog.DomainOf(never)})
			if strings.Contains(listing, never) {
				t.Errorf("%s is listed %s:\n%s", name, never, listing)
			}
		}
	}

	docs := catalog.GenericTools(table, person(iam.GrantWorklet), nil)[0]
	listing, _ := docs.Docs(catalog.Args{"domain": "worklet"})
	if !strings.Contains(listing, "Reads:\nGET /worklet/projects/{key} (reads") ||
		!strings.Contains(listing, "Changes:\nPOST /worklet/items (a change") {
		t.Errorf("the tracker's work domain lists:\n%s", listing)
	}
	if listing, _ := docs.Docs(catalog.Args{"domain": "config"}); !strings.Contains(listing,
		"No route matches.") {
		t.Errorf("the tracker's configuration domain lists:\n%s", listing)
	}
	found, _ := docs.Docs(catalog.Args{"query": "projects"})
	if !strings.Contains(found, "GET /worklet/projects/{key}") ||
		strings.Contains(found, "GET /worklet/{id} (") {
		t.Errorf("a query for projects lists:\n%s", found)
	}
}

// selectedText is the text a read's model result hands the model, unfenced.
func selectedText(t *testing.T, b provenance.Boundary, model string) string {
	t.Helper()
	var outer struct {
		Answer string `json:"answer"`
	}
	if err := json.Unmarshal([]byte(model), &outer); err != nil || outer.Answer == "" {
		t.Fatalf("the model's text is no fenced answer (%v): %s", err, model)
	}
	return provenance.Unfence(b, outer.Answer)
}

// A READ SELECTS ONLY THE FIELDS ASKED: dotted paths with `*` over a list or an
// object, a field of a list's elements walked implicitly, and an index each
// reach the model in the answer's own shape and nothing beside them; a
// selection naming nothing says so with the answer's fields; the card is drawn
// from the selection; trust is still the whole answer's; and a selection past
// its bounds is refused before anything is sent. Mutation: hand the model the
// whole answer, or decide trust on the selection, and this fails.
func TestAReadSelectsOnlyTheFieldsAsked(t *testing.T) {
	t.Parallel()
	b := provenance.NewBoundary()
	read := readAPI(t)
	answer := []byte(`{"items":[{"key":"ENG-1","title":"Ship","flow":{"state":"open",` +
		`"owner":"qa"}},{"key":"ENG-2","title":"Test","flow":{"state":"done"}}],` +
		`"next_cursor":"bmV4dA","total":2}`)
	shape := func(selected ...any) catalog.Result {
		t.Helper()
		args := readArgs("GET /worklet/search")
		args[catalog.SelectArgument] = selected
		if _, err := read.Request(args); err != nil {
			t.Fatalf("select %v: %v", selected, err)
		}
		res, err := catalog.Shape(read, args, exec.Response{Status: 200, Body: answer}, b,
			evals.Epoch, cards.Options{})
		if err != nil {
			t.Fatalf("select %v: %v", selected, err)
		}
		return res
	}
	for _, c := range []struct {
		selected []any
		want     string
	}{
		{[]any{"items.*.key", "items.*.flow.state", "next_cursor"},
			`{"items":[{"flow":{"state":"open"},"key":"ENG-1"},{"flow":{"state":"done"},` +
				`"key":"ENG-2"}],"next_cursor":"bmV4dA"}`},
		{[]any{"items.key"}, `{"items":[{"key":"ENG-1"},{"key":"ENG-2"}]}`},
		{[]any{"items.1.title"}, `{"items":[{"title":"Test"}]}`},
		{[]any{"items.0.flow.*"}, `{"items":[{"flow":{"owner":"qa","state":"open"}}]}`},
		{[]any{"total"}, `{"total":2}`},
	} {
		res := shape(c.selected...)
		if got := selectedText(t, b, res.Model); got != c.want {
			t.Errorf("select %v hands the model %s, want %s", c.selected, got, c.want)
		}
	}
	res := shape("items.*.key")
	if len(res.Cards) != 1 || res.Cards[0].Table == nil ||
		slices.ContainsFunc(res.Cards[0].Table.Columns, func(col wire.Column) bool {
			return col.Key == "title"
		}) {
		t.Errorf("a selection of keys is carded as %+v", res.Cards)
	}
	res = shape("nope.x")
	if got := selectedText(t, b, res.Model); !res.Empty || len(res.Cards) != 0 ||
		!strings.Contains(got, "selected_nothing") ||
		!strings.Contains(got, `"fields":["items","next_cursor","total"]`) {
		t.Errorf("a selection naming nothing hands the model %s (cards %d)", got, len(res.Cards))
	}

	entry := readArgs("GET /atlas/{id}")
	entry[catalog.SelectArgument] = []any{"entry.name"}
	res, err := catalog.Shape(read, entry, exec.Response{Status: 200, Body: []byte(
		`{"entry":{"name":"API 5xx","sources":[{"kind":"integration"}]}}`)}, b, evals.Epoch,
		cards.Options{})
	if err != nil || res.Trust != provenance.TrustExternal ||
		strings.Contains(res.Model, "integration") {
		t.Errorf("an entry an integration named, read without its sources, is %q: %s (%v)",
			res.Trust, res.Model, err)
	}

	many := make([]any, catalog.SelectMax+1)
	for i := range many {
		many[i] = "items.key"
	}
	for name, selected := range map[string]any{
		"too many paths":   many,
		"a bad segment":    []any{"items.$where"},
		"an empty segment": []any{"items..key"},
		"not a list":       "items.key",
		"not a path":       []any{7.0},
	} {
		args := readArgs("GET /worklet/search")
		args[catalog.SelectArgument] = selected
		if _, err := read.Request(args); !errors.Is(err, catalog.ErrInvalidArguments) {
			t.Errorf("%s was sent: %v", name, err)
		}
	}
}
