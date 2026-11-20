package api_test

import (
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/iam"
)

// TestEveryScreenReadIsAnAssistantRead: WHAT A SCREEN READS, ASK CREWLET CAN
// READ.
//
// Every question the dashboard asks (useQuery, useSearchQuery, useCodeQuery
// and query, as the contract gates read them) is read through its named route
// where it has one and through `GET /query/{what}` otherwise, and every
// literal path the dashboard hands rest.get is matched to the route the App
// mounts for it. Each such route must be in the assistant's table, unless its
// verb stays with the person (ADR-0052's AssistNever), the edge refuses every
// grant its path, or it is the node's probe. Mutation: refuse a route the
// dashboard reads to the assistant in AssistantRoutes, and this names it.
func TestEveryScreenReadIsAnAssistantRead(t *testing.T) {
	t.Parallel()
	app := everySurfaceApp(t)
	routes := app.AssistantRoutes()
	table, err := catalog.NewRouteTable(routes)
	if err != nil {
		t.Fatalf("the App's assistant routes: %v", err)
	}
	readable := map[string]bool{}
	for _, r := range table.Routes() {
		if r.Method() == http.MethodGet {
			readable[r.Pattern] = true
		}
	}
	kept := map[string]bool{}
	for _, r := range routes {
		if class, _ := authz.AssistOf(r.Action); r.Action != "" && class == authz.AssistNever {
			kept[r.Pattern] = true
		}
	}
	// THE PROBES answer a load balancer about this node, not a person about
	// the company.
	probes := map[string]bool{"GET /health": true, "GET /ready": true}
	covered := func(pattern string) bool {
		_, path, _ := strings.Cut(pattern, " ")
		return readable[pattern] || kept[pattern] || probes[pattern] ||
			auth.AssistRefusedPath(path)
	}

	tree := clientsource.Tree(t)
	questions, err := clientsource.Calls(tree, "useQuery", "useSearchQuery", "useCodeQuery",
		"query")
	if err != nil {
		t.Fatal(err)
	}
	named := map[string]string{}
	for pattern, what := range api.NamedReadRoutes() {
		named[what] = pattern
	}
	for what, files := range questions {
		route := "GET /query/{what}"
		if pattern, ok := named[what]; ok {
			route = pattern
		}
		if !covered(route) {
			t.Errorf("%v asks %q, read through %s, which Ask Crewlet cannot read", files, what,
				route)
		}
	}

	paths, err := clientsource.Calls(tree, "get")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	for _, pattern := range app.Routes() {
		if method, _, _ := strings.Cut(pattern, " "); method == http.MethodGet {
			mux.Handle(pattern, http.NotFoundHandler())
		}
	}
	matched := 0
	for path, files := range paths {
		if !strings.HasPrefix(path, "/") {
			continue
		}
		u, err := url.Parse(path)
		if err != nil {
			t.Errorf("%v reads %q, which is not a path", files, path)
			continue
		}
		_, pattern := mux.Handler(&http.Request{Method: http.MethodGet, URL: u, Host: "x"})
		if pattern == "" {
			continue
		}
		matched++
		if !covered(pattern) {
			t.Errorf("%v reads %s, the route %s, which Ask Crewlet cannot read", files, path,
				pattern)
		}
	}
	if len(questions) < 50 || matched < 10 {
		t.Fatalf("walked %d questions and %d rest reads; the dashboard's reads are not "+
			"being found", len(questions), matched)
	}
	if !readable["GET /query/{what}"] {
		t.Fatal("GET /query/{what} is not in the assistant's table")
	}
}

// TestAGenericReadIsTaggedWithTheAppsItDrewOn: a read_api answer carries the
// apps its route draws on, so a person who later loses one is no longer shown
// it. A named read takes its question's registered posture, the question
// route the posture of the question it names, a guarded route its verb's row.
// Mutation: drop AppsOf from the generic read, or QuestionApps from
// AssistantRoutes, and this fails.
func TestAGenericReadIsTaggedWithTheAppsItDrewOn(t *testing.T) {
	t.Parallel()
	fx := newAssistAppWith(t, []iam.Grant{iam.GrantAdmin}, parityWorld(t).mount)
	table, err := catalog.NewRouteTable(fx.app.AssistantRoutes())
	if err != nil {
		t.Fatal(err)
	}
	reader := func(p iam.Principal) catalog.Tool {
		for _, tool := range catalog.GenericTools(table, p, nil) {
			if tool.Name == catalog.ReadAPITool {
				return tool
			}
		}
		t.Fatal("no read_api")
		return catalog.Tool{}
	}
	tracker := reader(iam.Principal{Grants: []iam.Grant{iam.GrantWorklet}})
	admin := reader(iam.Principal{Grants: []iam.Grant{iam.GrantAdmin}})
	for _, c := range []struct {
		name string
		tool catalog.Tool
		args catalog.Args
		want []iam.Grant
	}{
		{"a named read", tracker, catalog.Args{"route": "GET /worklet/{id}",
			"path": map[string]any{"id": "ENG-1"}}, []iam.Grant{iam.GrantWorklet}},
		{"a question by name", tracker, catalog.Args{"route": catalog.QuestionRoute,
			"path": map[string]any{"what": "work_items"}}, []iam.Grant{iam.GrantWorklet}},
		{"an administrator's read", admin, catalog.Args{"route": "GET /integrations"},
			[]iam.Grant{iam.GrantAdmin}},
		{"an app the reader does not hold", tracker, catalog.Args{"route": catalog.QuestionRoute,
			"path": map[string]any{"what": "integrations"}}, nil},
	} {
		if got := c.tool.AppsFor(c.args); !slices.Equal(got, c.want) {
			t.Errorf("%s: %v is tagged %v, want %v", c.name, c.args, got, c.want)
		}
	}
}
