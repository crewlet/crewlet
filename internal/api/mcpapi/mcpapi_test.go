package mcpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/mcpapi"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/mcp/catalogue"
)

// fakeRegistry answers a search with found or err, and records what it was
// asked.
type fakeRegistry struct {
	found catalogue.Found
	err   error

	mu    sync.Mutex
	asked []string
}

func (f *fakeRegistry) Search(_ context.Context, base, q string) (catalogue.Found, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, base+" "+q)
	return f.found, f.err
}

func (f *fakeRegistry) searches() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.asked)
}

const registryURL = "https://registry.example.com/mirror"

// surface is the service over a company naming registry (none for ""),
// searching search.
func surface(t *testing.T, registry string, search mcpapi.Searcher) *mcpapi.Service {
	t.Helper()
	doc := "name: Acme\n"
	if registry != "" {
		doc += "mcp_registry:\n  url: " + registry + "\n"
	}
	company, err := config.ParseCompany([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	svc, err := mcpapi.New(mcpapi.Options{Company: func() *config.Company { return company },
		Registry: search, Detail: noDetail, Restarter: &fakeRestarter{}, Checker: &fakeChecker{}, Values: valueStore(t),
		SignIns: &fakeSignIns{}, Audit: &recordedAudit{}})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

// noDetail answers every server as one nobody configures, reports or lists.
func noDetail(_ context.Context, name string) (queries.MCPServerAnswer, error) {
	return queries.MCPServerAnswer{}, fmt.Errorf("%w: %s", queries.ErrNotFound, name)
}

// THE CURATED CATALOGUE IS ANSWERED WHOLE, AND THE REGISTRY IS ASKED ONLY FOR
// A SEARCH: opening the add flow costs no request to anybody, a company that
// names no registry is told why a search found nothing, and a registry that
// fails is a reason beside the curated half rather than an error in its place.
func TestTheCatalogueSearchesTheRegistryOnlyWhenAsked(t *testing.T) {
	t.Parallel()
	listing := catalogue.Entry{ID: "io.github.x/browser", Name: "browser", Community: true}
	registry := &fakeRegistry{found: catalogue.Found{Entries: []catalogue.Entry{listing}, LeftOut: 2}}
	curated := len(catalogue.Curated())

	opened, err := surface(t, registryURL, registry).Catalogue(t.Context(), "  ")
	if err != nil {
		t.Fatal(err)
	}
	if len(opened.Curated) != curated || len(opened.Community) != 0 || opened.Registry != registryURL ||
		opened.Reason != "" || len(registry.searches()) != 0 {
		t.Errorf("opening the catalogue answered %+v and searched %v", opened, registry.searches())
	}

	found, err := surface(t, registryURL, registry).Catalogue(t.Context(), " browser ")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(registry.searches(), []string{registryURL + " browser"}) {
		t.Errorf("searched %v", registry.searches())
	}
	if len(found.Community) != 1 || found.Community[0].ID != listing.ID || found.LeftOut != 2 ||
		len(found.Curated) != curated {
		t.Errorf("a search answered %+v", found)
	}

	unnamed := &fakeRegistry{}
	none, err := surface(t, "", unnamed).Catalogue(t.Context(), "browser")
	if err != nil {
		t.Fatal(err)
	}
	if none.Registry != "" || !strings.Contains(none.Reason, "mcp_registry.url") || len(unnamed.searches()) != 0 {
		t.Errorf("a company naming no registry answered %+v and searched %v", none, unnamed.searches())
	}

	failing := &fakeRegistry{err: errors.New("the registry at " + registryURL + " answered 503")}
	down, err := surface(t, registryURL, failing).Catalogue(t.Context(), "browser")
	if err != nil {
		t.Fatalf("a registry that failed failed the catalogue: %v", err)
	}
	if !strings.Contains(down.Reason, "503") || len(down.Community) != 0 || len(down.Curated) != curated {
		t.Errorf("a registry that failed answered %+v", down)
	}

	_, err = surface(t, registryURL, registry).Catalogue(t.Context(), strings.Repeat("a", catalogue.MaxQuery+1))
	var refused *catalogue.QueryError
	if !errors.As(err, &refused) {
		t.Errorf("search text past its bound answered %v, want the query refused", err)
	}

	if _, err := mcpapi.New(mcpapi.Options{}); err == nil {
		t.Error("a surface with no company reader was built")
	}
}

func caller(grants ...iam.Grant) iam.Principal {
	return iam.Principal{ID: uuid.New(), Login: "jane.doe", Kind: iam.KindPerson, Stage: iam.StageActive,
		Grants: grants, ReauthAt: time.Now().Add(-time.Hour)}
}

func get(t *testing.T, svc *mcpapi.Service, as iam.Principal, path string) (int, map[string]any) {
	t.Helper()
	mux := http.NewServeMux()
	if err := svc.Routes(mux); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequestWithContext(iam.WithPrincipal(t.Context(), as), http.MethodGet, path, nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s answered %d with %q", path, w.Code, w.Body.String())
	}
	return w.Code, body
}

// everyAppButAdmin is a member holding every grant there is but admin.
func everyAppButAdmin() iam.Principal {
	return caller(slices.DeleteFunc(slices.Clone(iam.AllGrants), func(g iam.Grant) bool { return g == iam.GrantAdmin })...)
}

// THE CATALOGUE IS EVERYBODY'S, AND ONLY THE CONFIGURATION'S READER SEARCHES
// THE REGISTRY: an admin reads the curated half and the community registry the
// company names, searched for `q`; a member holding every app reads the curated
// half alone, whatever `q` says, so the registry's address is never named to
// them and no search is made for them. A read asks no fresh proof, and search
// text the engine will not send is the admin's to change.
//
// Mutation: mount the catalogue on config.read, or answer a member the admin's
// reading, and this is red.
func TestTheCatalogueIsEverybodysAndOnlyTheConfigurationsReaderSearches(t *testing.T) {
	t.Parallel()
	registry := &fakeRegistry{found: catalogue.Found{Entries: []catalogue.Entry{}}}
	svc := surface(t, registryURL, registry)
	if code, body := get(t, svc, caller(iam.GrantAdmin), "/mcp-catalogue"); code != http.StatusOK ||
		body["reading"] != "admin" || body["registry"] != registryURL ||
		len(body["curated"].([]any)) != len(catalogue.Curated()) {
		t.Errorf("an admin whose proof is an hour old read %d %v", code, body)
	}
	for _, path := range []string{"/mcp-catalogue", "/mcp-catalogue?q=browser",
		"/mcp-catalogue?q=" + strings.Repeat("a", 201)} {
		code, body := get(t, svc, everyAppButAdmin(), path)
		if code != http.StatusOK {
			t.Fatalf("a member holding every app read %s: %d %v", path, code, body)
		}
		holdKeys(t, "a member's catalogue", body, "reading", "curated")
		if body["reading"] != "member" || len(body["curated"].([]any)) != len(catalogue.Curated()) {
			t.Errorf("a member read %s as %v", path, body)
		}
	}
	if searched := registry.searches(); len(searched) != 0 {
		t.Errorf("a member's read searched the registry: %v", searched)
	}
	code, body := get(t, svc, caller(iam.GrantAdmin), "/mcp-catalogue?q="+strings.Repeat("a", 201))
	if code != http.StatusBadRequest || body["error"] != "bad_params" ||
		!strings.Contains(body["detail"].(string), "200 characters") {
		t.Errorf("search text past its bound answered %d %v", code, body)
	}
}

// THE ANSWER'S SHAPE IS HELD HERE UNTIL THE ADD FLOW DECLARES IT: every field
// on every entry, curated or community, and every input, each list a list and
// never null. The dashboard's declaration of this answer lands with the first
// screen that reads it, whose contract gate then holds both directions.
func TestTheCatalogueAnswersEveryFieldOfEveryEntry(t *testing.T) {
	t.Parallel()
	updated := time.Date(2026, 10, 22, 9, 30, 0, 0, time.UTC)
	listing := catalogue.Entry{ID: "io.github.x/browser", Name: "browser", Title: "Browser",
		Description: "Drives a browser.", Transport: catalogue.TransportStdio, Command: "npx",
		Args: []string{"-y", "browser@1.0.0"}, Headers: []catalogue.Input{},
		Env: []catalogue.Input{{Name: "BROWSER_KEY", Secret: true}}, ToolAnnotations: map[string]catalogue.Hints{},
		Auth: []catalogue.AuthMethod{catalogue.AuthNone}, Package: "browser", Release: "1.0.0", Community: true,
		Publisher: "io.github.x", Version: "1.0.0", UpdatedAt: &updated}
	svc := surface(t, registryURL, &fakeRegistry{found: catalogue.Found{Entries: []catalogue.Entry{listing}}})
	code, body := get(t, svc, caller(iam.GrantAdmin), "/mcp-catalogue?q=browser")
	if code != http.StatusOK {
		t.Fatalf("answered %d %v", code, body)
	}
	holdKeys(t, "the answer", body, "reading", "curated", "community", "registry", "reason", "left_out")
	entries := append(body["curated"].([]any), body["community"].([]any)...)
	inputs := 0
	for _, raw := range entries {
		e := raw.(map[string]any)
		holdKeys(t, "an entry", e, "id", "name", "title", "vendor", "description", "transport", "url", "command",
			"args", "headers", "env", "tool_prefix", "tool_annotations", "auth", "oauth_client", "integration",
			"prerequisite", "docs", "read_at", "package", "release", "community", "publisher", "version",
			"updated_at")
		for _, list := range []string{"args", "headers", "env", "auth"} {
			if _, ok := e[list].([]any); !ok {
				t.Errorf("%s's %s is %v, not a list", e["id"], list, e[list])
			}
		}
		if _, ok := e["tool_annotations"].(map[string]any); !ok {
			t.Errorf("%s's tool_annotations is %v, not an object", e["id"], e["tool_annotations"])
		}
		for _, in := range append(e["headers"].([]any), e["env"].([]any)...) {
			inputs++
			holdKeys(t, "an input", in.(map[string]any), "name", "description", "required", "secret", "value",
				"format", "from_integration")
		}
	}
	if inputs == 0 || len(body["community"].([]any)) != 1 {
		t.Fatal("the answer carried no input or no community listing, so this proves nothing about them")
	}
}

func holdKeys(t *testing.T, what string, got map[string]any, want ...string) {
	t.Helper()
	keys := slices.Sorted(maps.Keys(got))
	if slices.Sort(want); !slices.Equal(keys, want) {
		t.Errorf("%s carries %v, want exactly %v", what, keys, want)
	}
}

// detailSurface is the surface over a detail that answers github whole and a
// server named "catalogue", says nothing is called "nothing", and cannot read
// anything else.
func detailSurface(t *testing.T) *mcpapi.Service {
	t.Helper()
	company, err := config.ParseCompany([]byte("name: Acme\n"))
	if err != nil {
		t.Fatal(err)
	}
	seen := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	failed := queries.MCPFailureView{Class: mcp.FailExited, Said: "its process exited (exit status 1)",
		At: seen.Add(-time.Minute), Node: "node-a", Seat: "backend-dev"}
	lastCall, callMS := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC), int64(120)
	detail := func(_ context.Context, name string) (queries.MCPServerAnswer, error) {
		switch name {
		case "github":
			return queries.MCPServerAnswer{Reading: queries.ReadingAdmin,
				Server: queries.MCPServerRow{Name: "github", Command: "npx", Args: []string{"-y", "launch-sentinel"},
					UnlistedAnnotations: []string{}, RefusedTools: []queries.MCPRefusedTool{},
					ShadowedTools: []queries.MCPShadowedTool{}, Nodes: []queries.MCPServerNode{}},
				Nodes: []queries.MCPStatusNode{}, Listed: []queries.MCPServerListing{},
				Tools: []queries.MCPToolView{{Name: "get_issue", Raw: "get_issue", Risk: mcp.RiskReadOnly,
					Digest: strings.Repeat("a", 64), Definitions: 1, Instances: 1, Nodes: []string{"node-a"},
					InputSchema: map[string]any{"type": "object"}}},
				ServerInfo: &coord.MCPServerInfo{Name: "github-mcp"},
				Dependants: configapi.MCPDependants{MCPCleared: configapi.MCPCleared{
					MCPEnv: []configapi.MCPBlockDependant{}, Sandbox: []configapi.MCPSandboxDependant{},
					MCPLimits: []configapi.MCPBlockDependant{}},
					Secrets: []string{}},
				Instances: []queries.MCPInstanceView{{Seat: "backend-dev", Node: "node-a",
					State: coord.MCPInstanceRunning, Since: &seen, Restarts: 1, Tools: 1,
					LastFailure: &failed, LastAnswer: &seen}},
				InstanceNodes: []queries.MCPInstanceNodeView{{Node: "node-a", Answered: true, Count: 1}},
				LastSuccess:   &seen, LastFailure: &failed,
				Resolved: &queries.MCPResolved{Auth: queries.MCPAuthIntegration, RunsOn: queries.MCPRunsOnEngineHost,
					Timeouts: queries.MCPTimeouts{StartupSeconds: 120, RequestSeconds: 300, StartupDefault: true,
						RequestDefault: true},
					Binding: &queries.MCPBinding{Vendor: "github", By: queries.MCPBindingRecognised,
						Keys: []queries.MCPBoundKey{{Field: "TOKEN", Key: "GITHUB_TOKEN"}}},
					BoxSeats: []queries.MCPBoxSeat{{Seat: "backend-dev", Cell: "container"}},
					Warnings: []queries.MCPPlacementWarning{{Kind: queries.MCPWarnCommandInBox,
						Seat: "backend-dev", Cell: "container", Message: "the image must provide npx"}}},
				Usage: queries.MCPServerUsage{Since: "2026-10-26", Until: "2026-11-01", Days: 7,
					Tools: []queries.MCPToolUsage{{Tool: "get_issue", Calls: 3, Failed: 1,
						TotalMS: 300, LastAt: &lastCall, Seats: []string{"backend-dev"}}}},
				Calls: queries.MCPServerCalls{Calls: []queries.MCPCallRow{{At: lastCall,
					Seat: "backend-dev", TurnID: "t-1", Tool: "get_issue", Outcome: mcp.CallAnswered,
					DurationMS: &callMS}}},
			}, nil
		case "catalogue":
			return queries.MCPServerAnswer{Server: queries.MCPServerRow{Name: "catalogue"}}, nil
		case "nothing":
			return queries.MCPServerAnswer{}, fmt.Errorf("%w: nothing", queries.ErrNotFound)
		}
		return queries.MCPServerAnswer{}, queries.ErrMCPDetailUnavailable
	}
	svc, err := mcpapi.New(mcpapi.Options{Company: func() *config.Company { return company },
		Registry: &fakeRegistry{}, Detail: detail, Restarter: &fakeRestarter{}, Checker: &fakeChecker{}, Values: valueStore(t),
		SignIns: &fakeSignIns{}, Audit: &recordedAudit{}})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

// ONE SERVER'S DETAIL IS EVERYBODY'S IN TWO READINGS, AND SAYS WHY IT CANNOT
// ANSWER: an admin reads it whole, a member holding every app reads a member's
// reading with no launch line, failure sentence, bound key, box or warning, a
// server nobody configures, reports or lists is 404, and a node that could not
// read the fleet's listings answers 503 with the wait to retry after, never an
// empty server. The catalogue lives outside the servers' namespace, so a
// server named "catalogue" has a detail of its own.
//
// Mutation: mount the detail on config.read, answer a member the admin's
// reading, answer an unreadable listing as a server with no tools, or serve
// the catalogue under /mcp-servers/, and this is red.
func TestAServersDetailIsEverybodysInTwoReadingsAndSaysWhyItCannotAnswer(t *testing.T) {
	t.Parallel()
	svc := detailSurface(t)
	code, body := get(t, svc, caller(iam.GrantAdmin), "/mcp-servers/github")
	if code != http.StatusOK {
		t.Fatalf("an admin read %d %v", code, body)
	}
	holdKeys(t, "the detail", body, "reading", "server", "nodes", "listed", "tools",
		"baseline", "unlisted_levels", "server_info", "instructions", "dependants", "instances",
		"instance_nodes", "last_success", "last_failure", "resolved", "usage", "calls", "last_check", "values",
		"reaches", "changes")
	instance := body["instances"].([]any)[0].(map[string]any)
	holdKeys(t, "an instance", instance, "seat", "node", "state", "since", "restarts", "tools", "failure",
		"wait", "last_failure", "last_answer")
	if instance["failure"] != nil {
		t.Errorf("a running instance answered a failure: %v", instance)
	}
	holdKeys(t, "a failure", instance["last_failure"].(map[string]any), "class", "said", "at", "node", "seat")
	holdKeys(t, "a node asked", body["instance_nodes"].([]any)[0].(map[string]any),
		"node", "answered", "reason", "count")
	resolved := body["resolved"].(map[string]any)
	holdKeys(t, "the resolved facts", resolved, "auth", "runs_on", "timeouts", "binding", "box_seats", "warnings")
	holdKeys(t, "the timeouts", resolved["timeouts"].(map[string]any),
		"startup_seconds", "request_seconds", "startup_default", "request_default")
	binding := resolved["binding"].(map[string]any)
	holdKeys(t, "the binding", binding, "vendor", "by", "keys")
	holdKeys(t, "a bound key", binding["keys"].([]any)[0].(map[string]any), "field", "key")
	holdKeys(t, "a box seat", resolved["box_seats"].([]any)[0].(map[string]any), "seat", "cell")
	holdKeys(t, "a warning", resolved["warnings"].([]any)[0].(map[string]any), "kind", "seat", "cell", "message")
	tool := body["tools"].([]any)[0].(map[string]any)
	holdKeys(t, "a tool", tool, "name", "raw", "title", "description", "annotations", "risk",
		"risk_declared", "external", "digest", "definitions", "instances", "nodes", "input_schema",
		"output_schema", "level", "custom", "held", "removed")
	if tool["risk"] != "read_only" || tool["output_schema"] != nil {
		t.Errorf("the tool answered %v", tool)
	}
	holdKeys(t, "the dependants", body["dependants"].(map[string]any), "mcp_env", "mcp_limits", "oauth", "sandbox",
		"secrets")
	// THE SERVER'S ACTIVITY, key for key: counts per tool, and each recent
	// call's who, when, which and how it ended, never its arguments or its
	// result.
	activity := body["usage"].(map[string]any)
	holdKeys(t, "the usage", activity, "since", "until", "days", "tools", "elided")
	holdKeys(t, "a tool's usage", activity["tools"].([]any)[0].(map[string]any), "tool", "calls",
		"failed", "unavailable", "refused", "total_ms", "last_at", "seats")
	calls := body["calls"].(map[string]any)
	holdKeys(t, "the calls", calls, "calls", "coverage")
	holdKeys(t, "a call", calls["calls"].([]any)[0].(map[string]any), "at", "seat", "turn_id",
		"tool", "outcome", "refusal", "duration_ms")

	req := httptest.NewRequestWithContext(iam.WithPrincipal(t.Context(), everyAppButAdmin()),
		http.MethodGet, "/mcp-servers/github", nil)
	w := httptest.NewRecorder()
	mux := http.NewServeMux()
	if err := svc.Routes(mux); err != nil {
		t.Fatal(err)
	}
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("a member holding every app read %d %s", w.Code, w.Body.String())
	}
	for _, leak := range []string{"launch-sentinel", "npx", "exit status 1", "GITHUB_TOKEN",
		"the image must provide", `"said"`, `"keys"`, `"box_seats"`, `"warnings"`, `"dependants"`,
		`"instructions"`, `"reason"`, `"seats"`, `"calls":{`, `"turn_id"`} {
		if strings.Contains(w.Body.String(), leak) {
			t.Errorf("a member's reading of the detail carries %s: %s", leak, w.Body.String())
		}
	}
	var member map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &member); err != nil {
		t.Fatal(err)
	}
	holdKeys(t, "a member's detail", member, "reading", "server", "nodes", "listed", "tools",
		"server_info", "instances", "instance_nodes", "last_success", "last_failure", "resolved", "usage")
	holdKeys(t, "a member's usage", member["usage"].(map[string]any), "since", "until", "days", "tools",
		"elided")
	if member["reading"] != "member" {
		t.Errorf("a member's detail says it is %v", member["reading"])
	}
	held := member["instances"].([]any)[0].(map[string]any)
	holdKeys(t, "a member's instance", held, "seat", "node", "state", "since", "restarts", "tools", "failure",
		"last_failure", "last_answer")
	holdKeys(t, "a member's failure", held["last_failure"].(map[string]any), "class", "at", "node", "seat")
	holdKeys(t, "a member's node asked", member["instance_nodes"].([]any)[0].(map[string]any),
		"node", "answered", "count")
	held = member["resolved"].(map[string]any)
	holdKeys(t, "a member's resolved facts", held, "auth", "runs_on", "timeouts", "binding")
	holdKeys(t, "a member's binding", held["binding"].(map[string]any), "vendor", "by")
	if code, body := get(t, svc, caller(iam.GrantAdmin), "/mcp-servers/nothing"); code != http.StatusNotFound ||
		body["error"] != "not_found" {
		t.Errorf("an unknown server answered %d %v", code, body)
	}
	req = httptest.NewRequestWithContext(iam.WithPrincipal(t.Context(), caller(iam.GrantAdmin)),
		http.MethodGet, "/mcp-servers/unreadable", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != "15" {
		t.Errorf("an unreadable listing answered %d, Retry-After %q", w.Code, w.Header().Get("Retry-After"))
	}
	if code, body := get(t, svc, caller(iam.GrantAdmin), "/mcp-catalogue"); code != http.StatusOK ||
		body["curated"] == nil {
		t.Errorf("the catalogue answered %d %v", code, body)
	}
	if code, body := get(t, svc, caller(iam.GrantAdmin), "/mcp-servers/catalogue"); code != http.StatusOK ||
		body["server"] == nil || body["server"].(map[string]any)["name"] != "catalogue" {
		t.Errorf("a server named catalogue answered %d %v, want its own detail", code, body)
	}
	if _, err := mcpapi.New(mcpapi.Options{Company: func() *config.Company { return nil }}); err == nil {
		t.Error("a surface with no detail was built")
	}
}
