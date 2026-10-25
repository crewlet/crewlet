package api_test

import (
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/artifactdataapi"
	"github.com/crewlet/crewlet/internal/api/artifactsapi"
	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/iamapi"
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
		IAM: &iamapi.Service{}, Work: &workapi.Service{}, Artifacts: &artifactsapi.Service{},
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

// EVERY GUARDED ROUTE IS COVERED FOR THE ASSISTANT, by exactly one exposure
// decision, and a route whose verb the authority table keeps from the
// assistant can only be Never.
//
// The guarded routes are read from the API's own route table, each with the
// verb it was mounted with, so a route added to any surface is a red build
// that says "classify this route for the assistant"; the overview reads the
// stream service serves are walked beside them. Which decisions are forced is
// DERIVED from the table rather than listed: a verb whose row is AssistNever
// (every iam, secrets, setup, fleet, purge and transfer verb among them, held
// by internal/authz's own walk) or that needs a person present cannot be
// exposed. Mutation: expose `GET /secrets` as a read in the table, or delete a
// route's row, and this names it.
func TestEveryGuardedRouteIsCoveredForTheAssistant(t *testing.T) {
	t.Parallel()
	app := everySurfaceApp(t)
	policies := app.Policies()
	if len(policies) < 100 {
		t.Fatalf("the route table holds %d guarded routes, so the walk is not reading "+
			"the table a node serves", len(policies))
	}
	rows := map[string][]catalog.Exposure{}
	for _, e := range catalog.Exposures() {
		if e.Route != "" {
			rows[e.Route] = append(rows[e.Route], e)
		}
	}
	overview := app.OverviewRoutes()
	for pattern, policy := range policies {
		got := rows[pattern]
		if len(got) != 1 {
			t.Errorf("%s (%s) has %d assistant exposure rows; give it exactly one in "+
				"internal/assistant/catalog's routes", pattern, policy.Action, len(got))
			continue
		}
		class, _ := authz.AssistOf(policy.Action)
		presence, _ := authz.PresenceOf(policy.Action)
		if (class == authz.AssistNever || presence != authz.PresenceOptional) &&
			got[0].As != catalog.AsNever {
			t.Errorf("%s is decided by %s, which the authority table keeps from the "+
				"assistant, and the catalogue exposes it as %s", pattern, policy.Action, got[0].As)
		}
	}
	for _, pattern := range overview {
		if len(rows[pattern]) != 1 {
			t.Errorf("the overview read %s has %d assistant exposure rows, want one",
				pattern, len(rows[pattern]))
		}
	}
	for pattern := range rows {
		if _, guarded := policies[pattern]; !guarded && !slices.Contains(overview, pattern) {
			t.Errorf("the catalogue decides about %s, which this build does not mount", pattern)
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
// `GET /work/{key}`, or move work_item's row to another tool, and this fails.
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
				if !catalog.ReadsRoute(route) {
					t.Errorf("%s reads the overview route %s, which the table does not read",
						tool.Name, route)
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
