package api_test

import (
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/artifactdataapi"
	"github.com/crewlet/crewlet/internal/api/artifactsapi"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/iamapi"
	"github.com/crewlet/crewlet/internal/api/llmapi"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/api/secretsapi"
	"github.com/crewlet/crewlet/internal/api/setupapi"
	"github.com/crewlet/crewlet/internal/api/workapi"
	"github.com/crewlet/crewlet/internal/assistant/assistanttest"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/authz"
)

// everySurfaceApp is an App with every surface it can mount mounted: each
// guarded surface as its real type (its zero value, since mounting reads
// nothing but the surface's own route table), the datasets surface and the
// operator surface, so the route table it keeps is the one a node serves.
//
// A SURFACE OPTIONS GAINS IS A SURFACE THIS FIXTURE MUST MOUNT: every
// interface field of api.Options that a guarded surface mounts through (a
// `Routes(authz.Mux)` method) must be set here, or the walks below would certify a table that
// is missing a surface's routes. Mutation: leave Work unset and this fails
// naming it.
func everySurfaceApp(t *testing.T) *api.App {
	t.Helper()
	operators, err := operator.New(operator.Options{
		Halves:    func() (operator.Halves, bool) { return operator.Halves{}, false },
		Authorize: builtin.Decide(authz.NoChart{}),
		Audit:     &recordedAudit{},
	})
	if err != nil {
		t.Fatalf("operator.New: %v", err)
	}
	opts := api.Options{
		Config: &configapi.Service{}, Secrets: &secretsapi.Service{}, Setup: &setupapi.Service{},
		Models: &llmapi.Service{},
		IAM:    &iamapi.Service{}, Work: &workapi.Service{}, Artifacts: &artifactsapi.Service{},
		ArtifactData: &artifactdataapi.Service{},
		Datasets: &api.DatasetsSource{Boards: &artifactsapi.Service{},
			Data: boardData{mu: &sync.Mutex{}, deals: &[]float64{}}},
		Operator: operators,
	}
	v := reflect.ValueOf(opts)
	for i := range v.NumField() {
		f, field := v.Type().Field(i), v.Field(i)
		if f.Type.Kind() != reflect.Interface {
			continue
		}
		routes, mounts := f.Type.MethodByName("Routes")
		if mounts && routes.Type.NumIn() == 1 &&
			routes.Type.In(0) == reflect.TypeFor[authz.Mux]() && field.IsNil() {
			t.Fatalf("api.Options.%s mounts a surface and this fixture does not set it, so the "+
				"assistant's route walks would not see its routes", f.Name)
		}
	}
	return newApp(t, opts)
}

// THE ASSISTANT'S ROUTE TABLE IS THE APP'S OWN, LESS WHAT IT NEVER TAKES.
//
// The generic API tools name the routes App.AssistantRoutes hands the
// catalogue, so that table is walked against the one the App mounted: every
// guarded route is in it with the verb it was mounted with, unless the edge
// refuses a grant its path (the credential surface, the socket, the MCP
// transports, the webhooks, the assistant's own routes); every route it
// names is mounted; and once bound, a route whose verb is on the human-only
// list is offered to no model (catalog.NewRouteTable leaves it out), while
// every other route is. Mutation: hand the catalogue a route the App does
// not mount, drop the guarded routes from AssistantRoutes, or keep a never
// verb in the bound table, and this fails.
func TestTheAssistantRouteTableIsTheAppsOwn(t *testing.T) {
	t.Parallel()
	app := everySurfaceApp(t)
	policies := app.Policies()
	if len(policies) < 100 {
		t.Fatalf("the route table holds %d guarded routes, so the walk is not reading "+
			"the table a node serves", len(policies))
	}
	mounted := app.Routes()
	routes := map[string]catalog.Route{}
	for _, r := range app.AssistantRoutes() {
		routes[r.Pattern] = r
		if _, guarded := policies[r.Pattern]; !guarded && !slices.Contains(mounted, r.Pattern) {
			t.Errorf("the assistant may name %s, which the App does not mount", r.Pattern)
		}
	}
	for pattern, policy := range policies {
		method, path, _ := strings.Cut(pattern, " ")
		if auth.AssistRefusedPath(path) || method == http.MethodOptions {
			if _, listed := routes[pattern]; listed {
				t.Errorf("%s is refused a grant at the edge and the assistant may name it",
					pattern)
			}
			continue
		}
		if got, listed := routes[pattern]; !listed || got.Action != policy.Action {
			t.Errorf("%s (%s) is not in the assistant's table as mounted: %+v", pattern,
				policy.Action, got)
		}
	}
	table, err := catalog.NewRouteTable(app.AssistantRoutes())
	if err != nil {
		t.Fatalf("the App's assistant routes do not make a table: %v", err)
	}
	for pattern, r := range routes {
		class, _ := authz.AssistOf(r.Action)
		_, bound := table.Lookup(pattern)
		if never := r.Action != "" && class == authz.AssistNever; bound == never {
			t.Errorf("%s (%s, %q to the assistant) is bound %v", pattern, r.Action, class, bound)
		}
	}
}

// EVERY ROUTE THE ASSISTANT MAY NAME IS DOCUMENTED, AND THE REFERENCE NAMES
// NOTHING THE APP DOES NOT MOUNT.
//
// describe_api answers a route's documentation from the API reference
// embedded in the binary (docs/reference/api-endpoints.md), so the reference
// is held to the route table both ways: every route the assistant may name has
// its row or its heading there, whatever its wildcards are called, and every
// route the reference documents on a path the assistant could reach is one the
// App mounts. Mutation: delete a route's row from the reference, or document a
// route nothing serves, and this names it.
func TestEveryAssistantRouteIsDocumented(t *testing.T) {
	t.Parallel()
	app := everySurfaceApp(t)
	mounted := slices.Concat(app.Routes(), slices.Collect(maps.Keys(app.Policies())))
	for _, r := range app.AssistantRoutes() {
		if !catalog.RouteDocumented(r.Pattern) {
			t.Errorf("%s is a route the assistant may name and docs/reference/"+
				"api-endpoints.md documents it nowhere", r.Pattern)
		}
	}
	for _, key := range catalog.DocumentedRoutes() {
		method, path, _ := strings.Cut(key, " ")
		if !slices.Contains([]string{http.MethodGet, http.MethodHead, http.MethodPost,
			http.MethodPut, http.MethodPatch, http.MethodDelete}, method) ||
			auth.AssistRefusedPath(path) {
			continue
		}
		if !slices.ContainsFunc(mounted, func(p string) bool { return catalog.Covers(key, p) }) {
			t.Errorf("docs/reference/api-endpoints.md documents %s, which the App does "+
				"not mount", key)
		}
	}
}

// EVERY ASSISTANT TOOL ROUTE IS MOUNTED, and a read reaches the answer the
// exposure table says it reads.
//
// A tool's Routes are what its Build may send, so a route the API does not
// mount is a tool that fails on every call while looking complete. For a read
// on a named read route the question behind it must be one the table reads
// AS THIS TOOL, which is what the registry's assist gate admits; on an
// overview route the route itself must be. Mutation: point get_task at
// `GET /worklet/{key}`, or move work_item's row to another tool, and this fails.
func TestEveryAssistantToolRouteIsMounted(t *testing.T) {
	t.Parallel()
	app := everySurfaceApp(t)
	mounted := app.Routes()
	policies := app.Policies()
	named := api.NamedReadRoutes()
	for _, tool := range catalog.Tools() {
		for _, route := range tool.Routes {
			if _, guarded := policies[route]; !guarded && !slices.Contains(mounted, route) {
				t.Errorf("%s may send %s, which the API does not mount", tool.Name, route)
			}
		}
		if tool.Kind != catalog.KindRead {
			continue
		}
		for _, route := range tool.Routes {
			question, isNamed := named[route]
			switch {
			case route == "GET /query/{what}":
				req, err := tool.Request(assistanttest.ExampleArgs(t, tool.Parameters))
				if err != nil {
					t.Fatalf("%s: %v", tool.Name, err)
				}
				question = strings.TrimPrefix(req.Path, "/query/")
			case isNamed:
			case slices.Contains(app.OverviewRoutes(), route):
				if !slices.ContainsFunc(catalog.Exposures(), func(e catalog.Exposure) bool {
					return e.Route == route && e.Name == tool.Name
				}) {
					t.Errorf("%s reads the overview route %s, which no exposure row names "+
						"it on", tool.Name, route)
				}
				continue
			default:
				policy, guarded := policies[route]
				if class, _ := authz.AssistOf(policy.Action); !guarded ||
					class != authz.AssistRead || !strings.HasPrefix(route, "GET ") {
					t.Errorf("%s reads %s, which is neither a named read nor a guarded "+
						"read the assistant may take", tool.Name, route)
				}
				continue
			}
			if !exposedAs(question, tool.Name) {
				t.Errorf("%s reads %s, the question %q, which the exposure table does not "+
					"read as %s", tool.Name, route, question, tool.Name)
			}
		}
	}
}

// exposedAs reports whether the table reads question as the tool name.
func exposedAs(question, name string) bool {
	return slices.ContainsFunc(catalog.Exposures(), func(e catalog.Exposure) bool {
		return e.Question == question && e.As == catalog.AsReadTool && e.Name == name
	})
}

// EVERY REPLACING ROUTE IS READ BEFORE IT IS PROPOSED (ADR-0055).
//
// A route that declares it replaces the whole object ([authz.Policy.Replacing])
// is one whose omitted fields the assistant's preview refuses unless the call
// names them in `removes`, which it can only do against the object as it is
// now: so every one is a PUT, its GET is in the assistant's table on the same
// path, and the declaration reaches the table as mounted. Every PUT on the
// configuration surface, whose GET round-trips into its PUT, declares it.
// Mutation: drop Replacing() from an entity route, or the GET of a replacing
// route from the table, and this names the route.
func TestEveryReplacingRouteIsReadBeforeItIsProposed(t *testing.T) {
	t.Parallel()
	app := everySurfaceApp(t)
	policies := app.Policies()
	listed := map[string]catalog.Route{}
	for _, r := range app.AssistantRoutes() {
		listed[r.Pattern] = r
	}
	replacing := 0
	for pattern, r := range listed {
		method, path, _ := strings.Cut(pattern, " ")
		if r.Replaces != policies[pattern].Replaces {
			t.Errorf("%s replaces %v in the assistant's table and %v as mounted", pattern,
				r.Replaces, policies[pattern].Replaces)
		}
		if method == http.MethodPut && strings.HasPrefix(path, "/config") && !r.Replaces {
			t.Errorf("%s replaces what /config holds at its address and does not declare it",
				pattern)
		}
		if !r.Replaces {
			continue
		}
		replacing++
		if method != http.MethodPut {
			t.Errorf("%s declares it replaces, which only a PUT does", pattern)
		}
		if _, read := listed[http.MethodGet+" "+path]; !read {
			t.Errorf("%s replaces what GET %s answers, which the assistant cannot read",
				pattern, path)
		}
	}
	if replacing < 3 {
		t.Errorf("only %d routes declare they replace; the walk is not reading the table",
			replacing)
	}
}
