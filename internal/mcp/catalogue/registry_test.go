package catalogue_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/mcp/catalogue"
)

// listing is one server as the registry lists it, `server.json` and its
// registry metadata.
type listing map[string]any

// page is the v0.1 server list over listings.
func page(listings ...listing) map[string]any {
	servers := make([]any, 0, len(listings))
	for _, l := range listings {
		servers = append(servers, map[string]any(l))
	}
	return map[string]any{"servers": servers, "metadata": map[string]any{"count": len(listings)}}
}

// official wraps a server.json as the official registry answers it.
func official(server map[string]any, status string) listing {
	return listing{"server": server, "_meta": map[string]any{
		"io.modelcontextprotocol.registry/official": map[string]any{
			"status": status, "updatedAt": "2026-10-22T09:30:00Z", "isLatest": true}}}
}

func npmServer(name, identifier, version string) map[string]any {
	return map[string]any{
		"name": name, "description": "Drives a headless browser.", "version": version,
		"packages": []any{map[string]any{
			"registryType": "npm", "identifier": identifier, "version": version,
			"transport": map[string]any{"type": "stdio"},
		}},
	}
}

func remoteServer(name, address string) map[string]any {
	return map[string]any{
		"name": name, "description": "A remote server.", "version": "1.0.0",
		"remotes": []any{map[string]any{"type": "streamable-http", "url": address}},
	}
}

// registry serves body as the v0.1 list and records what it was asked.
type registry struct {
	*httptest.Server
	asked atomic.Pointer[http.Request]
	hits  atomic.Int32
}

func serve(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) *registry {
	t.Helper()
	r := &registry{}
	r.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.hits.Add(1)
		r.asked.Store(req)
		handler(w, req)
	}))
	t.Cleanup(r.Close)
	return r
}

func answering(t *testing.T, body any) *registry {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
	})
}

func search(t *testing.T, r *registry, base, q string) (catalogue.Found, error) {
	t.Helper()
	return catalogue.NewRegistry(catalogue.RegistryOptions{Client: r.Client()}).
		Search(context.Background(), base, q)
}

// A SEARCH ASKS THE v0.1 LIST FOR THE NEWEST OF EACH SERVER, one page, under
// whatever path the company's address carries.
func TestASearchAsksTheV01ListForTheNewestOfEachServer(t *testing.T) {
	t.Parallel()
	r := answering(t, page())
	if _, err := search(t, r, r.URL+"/mirror/", "  browser  "); err != nil {
		t.Fatal(err)
	}
	asked := r.asked.Load()
	if asked == nil {
		t.Fatal("the registry was not asked")
	}
	if asked.URL.Path != "/mirror/v0.1/servers" {
		t.Errorf("asked %s, want the API's path under the address", asked.URL.Path)
	}
	q := asked.URL.Query()
	if q.Get("search") != "browser" || q.Get("version") != "latest" || q.Get("limit") != "20" {
		t.Errorf("asked %s", asked.URL.RawQuery)
	}
	if asked.Header.Get("Accept") != "application/json" {
		t.Errorf("Accept %q", asked.Header.Get("Accept"))
	}
}

// COMMUNITY RESULTS NEVER SKIP THE ADD FLOW. A listing is drafted as an entry
// marked community, with nothing a reviewed entry vouches for: no day it was
// read, no integration that would hand it an agent's credential, no value
// for a key a person has not typed, and a vendor's mark only for the vendor's
// own address. Its launch is the public package at its exact release through
// the runner this build spells, never one the listing names.
func TestCommunityResultsNeverSkipTheAddFlow(t *testing.T) {
	t.Parallel()
	playwright := npmServer("io.github.microsoft/playwright-mcp", "@playwright/mcp", "0.0.41")
	playwright["title"] = "Playwright"
	playwright["websiteUrl"] = "https://github.com/microsoft/playwright-mcp"
	playwright["packages"].([]any)[0].(map[string]any)["environmentVariables"] = []any{
		map[string]any{"name": "PLAYWRIGHT_TOKEN", "description": "A token.", "isRequired": true,
			"isSecret": true, "value": "{token}"},
	}
	playwright["packages"].([]any)[0].(map[string]any)["packageArguments"] = []any{
		map[string]any{"type": "named", "name": "--headless", "value": "true"},
		map[string]any{"type": "positional", "valueHint": "directory"},
	}
	pypi := map[string]any{
		"name": "io.github.crystaldba/postgres-mcp", "description": "Read a database.", "version": "0.3.0",
		"packages": []any{map[string]any{"registryType": "pypi", "registryBaseUrl": "https://pypi.org",
			"identifier": "postgres-mcp", "version": "0.3.0", "transport": map[string]any{"type": "stdio"}}},
	}
	linear := remoteServer("app.linear/linear", "https://mcp.linear.app/mcp")
	linear["remotes"].([]any)[0].(map[string]any)["headers"] = []any{
		map[string]any{"name": "Authorization", "description": "Your API key.", "isRequired": true,
			"value": "Bearer {api_key}"},
	}
	// A local listing that names a vetted vendor's package is still the
	// listing's word, and gets no mark.
	lookalike := npmServer("io.github.someone/slack", "slack-mcp-server", "9.9.9")
	r := answering(t, page(official(playwright, "active"), listing{"server": pypi},
		official(linear, "active"), official(lookalike, "active")))

	found, err := search(t, r, r.URL, "server")
	if err != nil {
		t.Fatal(err)
	}
	if found.LeftOut != 0 || len(found.Entries) != 4 {
		t.Fatalf("found %d, left %d out", len(found.Entries), found.LeftOut)
	}
	for _, e := range found.Entries {
		if !e.Community || e.ReadAt != "" || e.Integration != "" || e.Prerequisite != "" || e.OAuthClient != "" {
			t.Errorf("%s is drafted as more than a community listing: %+v", e.ID, e)
		}
		for _, in := range append(slices.Clone(e.Headers), e.Env...) {
			if in.Value != "" || in.FromIntegration {
				t.Errorf("%s carries a value for %s that nobody typed", e.ID, in.Name)
			}
		}
	}

	pw := found.Entries[0]
	updated := time.Date(2026, 10, 22, 9, 30, 0, 0, time.UTC)
	if pw.ID != "io.github.microsoft/playwright-mcp" || pw.Name != "playwright-mcp" || pw.Title != "Playwright" ||
		pw.Publisher != "io.github.microsoft" || pw.Version != "0.0.41" || pw.UpdatedAt == nil ||
		!pw.UpdatedAt.Equal(updated) || pw.Docs != "https://github.com/microsoft/playwright-mcp" || pw.Vendor != "" {
		t.Errorf("playwright = %+v", pw)
	}
	if pw.Transport != catalogue.TransportStdio || pw.Command != "npx" ||
		!slices.Equal(pw.Args, []string{"-y", "@playwright/mcp@0.0.41", "--headless=true"}) ||
		pw.Package != "@playwright/mcp" || pw.Release != "0.0.41" {
		t.Errorf("playwright launches %s %q", pw.Command, pw.Args)
	}
	if len(pw.Env) != 1 || pw.Env[0].Name != "PLAYWRIGHT_TOKEN" || !pw.Env[0].Secret || !pw.Env[0].Required {
		t.Errorf("playwright reads %+v", pw.Env)
	}

	pg := found.Entries[1]
	if pg.Command != "uvx" || !slices.Equal(pg.Args, []string{"postgres-mcp==0.3.0"}) || pg.UpdatedAt != nil ||
		pg.Title != "postgres-mcp" || pg.Vendor != "" {
		t.Errorf("postgres = %+v", pg)
	}

	ln := found.Entries[2]
	if ln.Transport != catalogue.TransportHTTP || ln.URL != "https://mcp.linear.app/mcp" || ln.Vendor != "linear" ||
		!slices.Equal(ln.Auth, []catalogue.AuthMethod{catalogue.AuthHeaders}) {
		t.Errorf("linear = %+v", ln)
	}
	if len(ln.Headers) != 1 || ln.Headers[0].Format != "Bearer {api_key}" || !ln.Headers[0].Secret {
		t.Errorf("linear reads %+v: an Authorization header is a credential whatever the listing says", ln.Headers)
	}

	if found.Entries[3].Vendor != "" {
		t.Errorf("a local listing naming slack-mcp-server is drawn with %q's mark", found.Entries[3].Vendor)
	}
}

// A LISTING THIS BUILD CANNOT OFFER WHOLE IS LEFT OUT, and counted: one whose
// fields are past their bounds, could make a person read something other
// than what the bytes say, or whose launch it cannot spell without trusting
// the listing's words about what to fetch.
func TestAListingThisBuildCannotOfferWholeIsLeftOut(t *testing.T) {
	t.Parallel()
	good := func() map[string]any { return npmServer("io.github.x/good", "good-mcp", "1.2.3") }
	pkg := func(s map[string]any) map[string]any { return s["packages"].([]any)[0].(map[string]any) }
	cases := map[string]func() any{
		"a name with no namespace": func() any { s := good(); s["name"] = "good"; return s },
		"a name with a space":      func() any { s := good(); s["name"] = "io.github.x/go od"; return s },
		"no description":           func() any { s := good(); delete(s, "description"); return s },
		"a description past 100": func() any {
			s := good()
			s["description"] = strings.Repeat("a", 101)
			return s
		},
		"a direction override": func() any { s := good(); s["description"] = "safe \u202egnp.exe"; return s },
		"a control character":  func() any { s := good(); s["title"] = "bell\a"; return s },
		"no version":           func() any { s := good(); delete(s, "version"); return s },
		"a templated address": func() any {
			return remoteServer("io.github.x/r", "https://{tenant}.example.com/mcp")
		},
		"a templated path": func() any {
			return remoteServer("io.github.x/r", "https://mcp.example.com/{tenant}/mcp")
		},
		"a plain http address":      func() any { return remoteServer("io.github.x/r", "http://mcp.example.com/") },
		"credentials in an address": func() any { return remoteServer("io.github.x/r", "https://u:p@mcp.example.com/") },
		"a legacy SSE remote": func() any {
			s := remoteServer("io.github.x/r", "https://mcp.example.com/sse")
			s["remotes"].([]any)[0].(map[string]any)["type"] = "sse"
			return s
		},
		"a header that is not a token": func() any {
			s := remoteServer("io.github.x/r", "https://mcp.example.com/")
			s["remotes"].([]any)[0].(map[string]any)["headers"] = []any{map[string]any{"name": "X Key"}}
			return s
		},
		"a header named twice": func() any {
			s := remoteServer("io.github.x/r", "https://mcp.example.com/")
			s["remotes"].([]any)[0].(map[string]any)["headers"] = []any{
				map[string]any{"name": "X-Key"}, map[string]any{"name": "x-key"}}
			return s
		},
		"a package from another npm registry": func() any {
			s := good()
			pkg(s)["registryBaseUrl"] = "https://npm.example.com"
			return s
		},
		"runner arguments": func() any {
			s := good()
			pkg(s)["runtimeArguments"] = []any{map[string]any{"type": "named", "name": "--registry",
				"value": "https://npm.example.com"}}
			return s
		},
		"the newest release": func() any { s := good(); pkg(s)["version"] = "latest"; return s },
		"a release range":    func() any { s := good(); pkg(s)["version"] = "^1.2.0"; return s },
		"a package spelled as a flag": func() any {
			s := good()
			pkg(s)["identifier"] = "--package=evil"
			return s
		},
		"a container image": func() any { s := good(); pkg(s)["registryType"] = "oci"; return s },
		"an http package":   func() any { s := good(); pkg(s)["transport"] = map[string]any{"type": "sse"}; return s },
		"a positional spelled as a flag": func() any {
			s := good()
			pkg(s)["packageArguments"] = []any{map[string]any{"type": "positional", "value": "--registry=x"}}
			return s
		},
		"a variable that is not a name": func() any {
			s := good()
			pkg(s)["environmentVariables"] = []any{map[string]any{"name": "1KEY"}}
			return s
		},
		"more variables than a server takes": func() any {
			s := good()
			vars := []any{}
			for i := range 33 {
				vars = append(vars, map[string]any{"name": "KEY_" + strings.Repeat("A", i+1)})
			}
			pkg(s)["environmentVariables"] = vars
			return s
		},
		"a value of the wrong kind": func() any { s := good(); s["description"] = 7; return s },
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// ITS OWN NAME, unless the name is what the case breaks: a
			// listing named like the good one is left out as a duplicate,
			// whatever else is wrong with it, and would prove nothing.
			bad := build()
			if s, ok := bad.(map[string]any); ok && s["name"] == "io.github.x/good" {
				s["name"] = "io.github.x/bad"
			}
			r := answering(t, page(official(good(), "active"), listing{"server": bad}))
			found, err := search(t, r, r.URL, "x")
			if err != nil {
				t.Fatal(err)
			}
			if len(found.Entries) != 1 || found.Entries[0].ID != "io.github.x/good" || found.LeftOut != 1 {
				t.Errorf("offered %d, left %d out; want the good listing alone and this one left out",
					len(found.Entries), found.LeftOut)
			}
		})
	}

	// DEPRECATED, DELETED OR TWICE: left out as well.
	r := answering(t, page(official(good(), "active"), official(npmServer("io.github.x/old", "old", "1.0.0"),
		"deprecated"), official(npmServer("io.github.x/gone", "gone", "1.0.0"), "deleted"),
		official(good(), "active")))
	found, err := search(t, r, r.URL, "x")
	if err != nil {
		t.Fatal(err)
	}
	if len(found.Entries) != 1 || found.LeftOut != 3 {
		t.Errorf("offered %d, left %d out; want one offered and three left out", len(found.Entries), found.LeftOut)
	}
}

// THE SEARCH BOUNDS WHAT IT READS: a page past its size is refused rather than
// read, a registry that ignores the page size is cut to it, and an answer that
// is not the list is an error that says so.
func TestTheSearchBoundsWhatItReads(t *testing.T) {
	t.Parallel()
	huge := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"servers":[`))
		_, _ = w.Write([]byte(strings.Repeat(" ", catalogue.MaxSearchBytes)))
		_, _ = w.Write([]byte(`]}`))
	})
	if _, err := search(t, huge, huge.URL, "x"); err == nil || !strings.Contains(err.Error(), "KiB") {
		t.Errorf("an oversized page answered %v", err)
	}

	var many []listing
	for i := range 2 * catalogue.MaxListings {
		many = append(many, official(npmServer("io.github.x/s"+strings.Repeat("a", i+1), "s", "1.0.0"), "active"))
	}
	r := answering(t, page(many...))
	found, err := search(t, r, r.URL, "x")
	if err != nil {
		t.Fatal(err)
	}
	if len(found.Entries) != catalogue.MaxListings || found.LeftOut != 0 {
		t.Errorf("offered %d and left %d out of a page twice the size asked for", len(found.Entries), found.LeftOut)
	}

	for name, handler := range map[string]func(w http.ResponseWriter, r *http.Request){
		"a refusal": func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "down for maintenance", http.StatusServiceUnavailable)
		},
		"not the list": func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>")) },
	} {
		r := serve(t, handler)
		if _, err := search(t, r, r.URL, "x"); err == nil {
			t.Errorf("%s answered no error", name)
		}
	}
}

// THE SEARCH DIALS THE REGISTRY AND NOTHING A LISTING NAMES: not its site, its
// repository, its remote or an icon, and not the address a redirect names,
// which the operator writes instead.
func TestTheSearchDialsNothingAListingNames(t *testing.T) {
	t.Parallel()
	elsewhere := serve(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"servers":[]}`)) })
	s := remoteServer("io.github.x/r", elsewhere.URL+"/mcp")
	s["websiteUrl"] = elsewhere.URL + "/site"
	s["repository"] = map[string]any{"url": elsewhere.URL + "/repo", "source": "github"}
	s["icons"] = []any{map[string]any{"src": elsewhere.URL + "/icon.svg"}}
	r := answering(t, page(official(s, "active")))
	found, err := search(t, r, r.URL, "x")
	if err != nil {
		t.Fatal(err)
	}
	if len(found.Entries) != 1 {
		t.Fatalf("offered %d", len(found.Entries))
	}

	moved := serve(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/v0.1/servers", http.StatusMovedPermanently)
	})
	_, err = search(t, moved, moved.URL, "x")
	if err == nil || !strings.Contains(err.Error(), elsewhere.URL) {
		t.Errorf("a redirect answered %v, want an error naming where it pointed", err)
	}
	if hits := elsewhere.hits.Load(); hits != 0 {
		t.Errorf("an address a listing or a redirect named was dialled %d times", hits)
	}
}

// A SEARCH ENDS AT ITS BUDGET OR ITS ASKER'S, whichever is first: a registry
// that never answers costs the person waiting no more than that.
func TestASearchEndsWhenItsAskerGivesUp(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	stuck := serve(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := catalogue.NewRegistry(catalogue.RegistryOptions{Client: stuck.Client()}).Search(ctx, stuck.URL, "x")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a registry that never answered gave %v", err)
	}
}

// A REGISTRY ADDRESS IS HTTPS WITH NOTHING BUT A PATH: the launch lines it
// answers are worth what the channel they came over is, and the address is
// never where a credential goes.
func TestARegistryAddressIsHTTPSWithNothingButAPath(t *testing.T) {
	t.Parallel()
	for address, ok := range map[string]bool{
		"https://registry.modelcontextprotocol.io":   true,
		"https://mcp.example.com/registry/":          true,
		"https://10.0.0.7:8443/mirror":               true,
		"http://registry.example.com":                false,
		"https://user:secret@registry.example.com":   false,
		"https://registry.example.com/?token=secret": false,
		"https://registry.example.com/#v0.1":         false,
		"https://":                                   false,
		"registry.example.com":                       false,
		"":                                           false,
		"https://registry.example.com/%zz":           false,
	} {
		err := catalogue.CheckRegistryAddress(address)
		var refused *catalogue.AddressError
		if (err == nil) != ok || (err != nil && (!errors.As(err, &refused) || refused.Reason == "")) {
			t.Errorf("%q: %v", address, err)
		}
	}
	r := answering(t, page())
	if _, err := search(t, r, "http://"+strings.TrimPrefix(r.URL, "https://"), "x"); !errors.Is(err,
		catalogue.ErrRegistryAddress) || r.hits.Load() != 0 {
		t.Errorf("a plain http registry was searched: %v", err)
	}
}

// SEARCH TEXT IS SOME TEXT, BOUNDED, with no control character.
func TestSearchTextIsBounded(t *testing.T) {
	t.Parallel()
	for q, ok := range map[string]bool{
		"browser":                                 true,
		strings.Repeat("é", catalogue.MaxQuery):   true,
		strings.Repeat("a", catalogue.MaxQuery+1): false,
		"   ":         false,
		"line\nbreak": false,
	} {
		err := catalogue.CheckQuery(q)
		var refused *catalogue.QueryError
		if (err == nil) != ok || (err != nil && !errors.As(err, &refused)) {
			t.Errorf("%q: %v", q, err)
		}
	}
}

// THE SUGGESTED NAME IS A SERVER NAME, whatever the listing's: lowercase
// letters, digits and single hyphens, at most 32 characters.
func TestTheSuggestedNameIsAServerName(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]string{
		"io.github.x/Playwright_MCP":               "playwright-mcp",
		"io.github.x/a..b--c":                      "a-b-c",
		"io.github.x/_._":                          "server",
		"io.github.x/" + strings.Repeat("ab-", 20): "ab-ab-ab-ab-ab-ab-ab-ab-ab-ab-ab",
	} {
		r := answering(t, page(listing{"server": npmServer(name, "x", "1.0.0")}))
		found, err := search(t, r, r.URL, "x")
		if err != nil || len(found.Entries) != 1 {
			t.Fatalf("%s: %v %+v", name, err, found)
		}
		if got := found.Entries[0].Name; got != want {
			t.Errorf("%s suggests %q, want %q", name, got, want)
		}
	}
}
