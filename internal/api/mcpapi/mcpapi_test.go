package mcpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/mcpapi"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
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
		Registry: search})
	if err != nil {
		t.Fatal(err)
	}
	return svc
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

// THE ROUTE IS THE CONFIGURATION'S READER'S: an entry is the draft of a launch
// line every engine host runs. A read asks no fresh proof, and search text the
// engine will not send is the caller's to change.
func TestTheCatalogueRouteIsTheConfigurationReaders(t *testing.T) {
	t.Parallel()
	svc := surface(t, registryURL, &fakeRegistry{})
	if code, body := get(t, svc, caller(iam.GrantAdmin), "/mcp-servers/catalogue"); code != http.StatusOK ||
		len(body["curated"].([]any)) != len(catalogue.Curated()) {
		t.Errorf("an admin whose proof is an hour old read %d %v", code, body)
	}
	if code, body := get(t, svc, caller(slices.DeleteFunc(slices.Clone(iam.AllGrants),
		func(g iam.Grant) bool { return g == iam.GrantAdmin })...), "/mcp-servers/catalogue"); code !=
		http.StatusForbidden {
		t.Errorf("a member holding every app read %d %v", code, body)
	}
	code, body := get(t, svc, caller(iam.GrantAdmin), "/mcp-servers/catalogue?q="+strings.Repeat("a", 201))
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
	code, body := get(t, svc, caller(iam.GrantAdmin), "/mcp-servers/catalogue?q=browser")
	if code != http.StatusOK {
		t.Fatalf("answered %d %v", code, body)
	}
	holdKeys(t, "the answer", body, "curated", "community", "registry", "reason", "left_out")
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
