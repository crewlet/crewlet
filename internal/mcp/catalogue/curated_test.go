package catalogue_test

import (
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/datadog"
	"github.com/crewlet/crewlet/internal/mcp/catalogue"
)

// runners are the commands that fetch a package by name: a launch through one
// fetches whatever release it names, so a curated one names the release its
// facts were read against.
var runners = []string{"npx", "npm", "uvx", "uv", "pipx", "pip", "pip3"}

var (
	serverName  = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	readDay     = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	exactSemver = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	headerName  = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")
	envName     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// EVERY CURATED ENTRY IS PINNED WITH THE DAY ITS FACTS WERE READ, a launch
// through a package runner with the exact release it fetches, and each says
// only what it can stand behind: an address that is https, the sign-ins it
// offers each backed by what that sign-in needs, and no fixed value on a
// credential.
func TestEveryCuratedEntryIsPinnedWithTheDayItWasRead(t *testing.T) {
	t.Parallel()
	entries := catalogue.Curated()
	if len(entries) == 0 {
		t.Fatal("the curated catalogue is empty, so this certifies nothing")
	}
	ids := map[string]bool{}
	today := time.Now().UTC().Format(time.DateOnly)
	for _, e := range entries {
		if ids[e.ID] {
			t.Errorf("%s is in the catalogue twice", e.ID)
		}
		ids[e.ID] = true
		if e.Name != e.ID || !serverName.MatchString(e.Name) || len(e.Name) > 32 || e.Name == "crewlet" {
			t.Errorf("%s suggests the server name %q: a curated entry suggests its own key, a server "+
				"name a seat's mcp_env can be keyed by", e.ID, e.Name)
		}
		if e.Title == "" || e.Description == "" || strings.Contains(e.Description, "\n") {
			t.Errorf("%s needs a title and a one-line description", e.ID)
		}
		if !readDay.MatchString(e.ReadAt) {
			t.Errorf("%s was read at %q, which is not a day", e.ID, e.ReadAt)
		} else if _, err := time.Parse(time.DateOnly, e.ReadAt); err != nil || e.ReadAt > today {
			t.Errorf("%s was read at %q, which is not a day that has happened", e.ID, e.ReadAt)
		}
		if !strings.HasPrefix(e.Docs, "https://") {
			t.Errorf("%s links its docs at %q, which is not an https address", e.ID, e.Docs)
		}
		if e.Community || e.Publisher != "" || e.Version != "" || e.UpdatedAt != nil {
			t.Errorf("%s carries a community listing's fields", e.ID)
		}

		switch e.Transport {
		case catalogue.TransportHTTP:
			u, err := url.Parse(e.URL)
			if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
				t.Errorf("%s is remote at %q, which is not an https address", e.ID, e.URL)
			}
			if e.Command != "" || len(e.Args) > 0 || len(e.Env) > 0 || e.Package != "" {
				t.Errorf("%s is remote and carries a local launch", e.ID)
			}
		case catalogue.TransportStdio:
			if e.Command == "" || e.URL != "" || len(e.Headers) > 0 {
				t.Errorf("%s is local and carries no command, or a remote's address or headers", e.ID)
			}
		default:
			t.Errorf("%s has the transport %q", e.ID, e.Transport)
		}

		// A RUNNER FETCHES WHAT IT IS TOLD, so the launch names exactly
		// the release the entry pins.
		if slices.Contains(runners, path.Base(e.Command)) {
			fetched := 0
			for _, arg := range e.Args {
				if arg == e.Package+"@"+e.Release || arg == e.Package+"=="+e.Release {
					fetched++
				}
			}
			if e.Package == "" || !exactSemver.MatchString(e.Release) || fetched != 1 {
				t.Errorf("%s launches %s %q through a package runner: it must name its Package and "+
					"fetch it once at its exact Release (%q at %q)", e.ID, e.Command, e.Args, e.Package, e.Release)
			}
		} else if e.Package != "" || e.Release != "" {
			t.Errorf("%s pins %s %s and launches nothing that fetches it", e.ID, e.Package, e.Release)
		}

		holdAuth(t, e)
		holdInputs(t, e)

		// ITS OWN LAUNCH IS RECOGNISED AS IT, so a server added from the
		// entry is drawn as the entry.
		if known, ok := catalogue.Recognise(launchOf(e)); !ok || known.ID != e.ID {
			t.Errorf("%s's own launch is recognised as %q", e.ID, known.ID)
		}
	}
}

// holdAuth holds an entry's sign-ins to what each needs.
func holdAuth(t *testing.T, e catalogue.Entry) {
	t.Helper()
	if len(e.Auth) == 0 {
		t.Errorf("%s offers no sign-in", e.ID)
	}
	seen := map[catalogue.AuthMethod]bool{}
	for _, a := range e.Auth {
		if !a.Valid() || seen[a] {
			t.Errorf("%s offers %q, which is not a sign-in or is offered twice", e.ID, a)
		}
		seen[a] = true
	}
	if seen[catalogue.AuthOAuth] != e.OAuthClient.Valid() {
		t.Errorf("%s offers OAuth %v with the client registration %q: one goes with the other",
			e.ID, seen[catalogue.AuthOAuth], e.OAuthClient)
	}
	if seen[catalogue.AuthIntegration] != (e.Integration != "") {
		t.Errorf("%s offers its integration %v and names %q", e.ID, seen[catalogue.AuthIntegration], e.Integration)
	}
	if seen[catalogue.AuthHeaders] && (e.Transport != catalogue.TransportHTTP || len(e.Headers) == 0) {
		t.Errorf("%s offers a header sign-in and names no header to send", e.ID)
	}
	if e.Integration != "" {
		if _, _, ok := catalogue.SeatKeysFor(launchOf(e)); !ok {
			t.Errorf("%s is its integration's server, and its own launch binds no seat credential", e.ID)
		}
	}
}

// holdInputs holds an entry's headers and variables to keys a server can read,
// each once, a credential never carrying a fixed value.
func holdInputs(t *testing.T, e catalogue.Entry) {
	t.Helper()
	seen := map[string]bool{}
	for _, in := range append(slices.Clone(e.Headers), e.Env...) {
		valid := envName.MatchString
		if e.Transport == catalogue.TransportHTTP {
			valid = headerName.MatchString
		}
		if !valid(in.Name) || seen[strings.ToLower(in.Name)] {
			t.Errorf("%s reads %q, which is not a key its transport reads or is read twice", e.ID, in.Name)
		}
		seen[strings.ToLower(in.Name)] = true
		if in.Description == "" {
			t.Errorf("%s says nothing about %s", e.ID, in.Name)
		}
		if in.Secret && in.Value != "" {
			t.Errorf("%s fixes a value for its credential %s", e.ID, in.Name)
		}
		if in.FromIntegration && e.Integration == "" {
			t.Errorf("%s says an integration hands %s over, and names none", e.ID, in.Name)
		}
	}
}

func launchOf(e catalogue.Entry) catalogue.Launch {
	return catalogue.Launch{Transport: e.Transport, URL: e.URL, Command: e.Command, Args: e.Args}
}

// A COPY IS A COPY: an entry a caller holds can be changed without changing
// what the next caller is offered or what a server is recognised as.
func TestTheCuratedTableCannotBeChangedThroughACopy(t *testing.T) {
	t.Parallel()
	held := catalogue.Curated()
	for i := range held {
		held[i].Title = "changed"
		if len(held[i].Args) > 0 {
			held[i].Args[0] = "changed"
		}
		if len(held[i].Env) > 0 {
			held[i].Env[0].Name = "CHANGED"
		}
		for tool, h := range held[i].ToolAnnotations {
			if h.ReadOnly != nil {
				*h.ReadOnly = false
			}
			held[i].ToolAnnotations[tool] = h
		}
	}
	for _, e := range catalogue.Curated() {
		if e.Title == "changed" || slices.Contains(e.Args, "changed") ||
			slices.ContainsFunc(e.Env, func(in catalogue.Input) bool { return in.Name == "CHANGED" }) {
			t.Errorf("%s changed through a copy a caller held", e.ID)
		}
		for tool, h := range e.ToolAnnotations {
			if h.ReadOnly != nil && !*h.ReadOnly {
				t.Errorf("%s's %s hint changed through a copy a caller held", e.ID, tool)
			}
		}
	}
}

// key is one seat credential as the binding table it replaces spelled it.
type key struct{ field, env, header, prefix string }

// replaced is the binding table recognition replaces (SeatToolBindings in
// internal/config, before this package held it), integration by integration
// in its own order: the conventional server's name and the keys it reads.
var replaced = []struct {
	vendor, server string
	keys           []key
}{
	{"datadog", "datadog", []key{{"APP_KEY", "DD_APPLICATION_KEY", "DD_APPLICATION_KEY", ""}}},
	{"github", "github", []key{{"APP_TOKEN", "GITHUB_PERSONAL_ACCESS_TOKEN", "Authorization", "Bearer "}}},
	{"gitlab", "gitlab", []key{{"TOKEN", "GITLAB_TOKEN", "Private-Token", ""}}},
	{"gitlab", "gitlab", []key{{"TOKEN", "GITLAB_PERSONAL_ACCESS_TOKEN", "Private-Token", ""}}},
	{"slack", "slack", []key{{"BOT_TOKEN", "SLACK_MCP_XOXB_TOKEN", "Authorization", "Bearer "}}},
	{"mattermost", "mattermost", []key{{"BOT_TOKEN", "MATTERMOST_TOKEN", "", ""}}},
	{"atlassian", "atlassian", []key{{"TOKEN", "JIRA_API_TOKEN", "", ""}, {"EMAIL", "JIRA_USERNAME", "", ""},
		{"TOKEN", "CONFLUENCE_API_TOKEN", "", ""}, {"EMAIL", "CONFLUENCE_USERNAME", "", ""}}},
}

func keysOf(in []catalogue.SeatKey) []key {
	out := make([]key, 0, len(in))
	for _, k := range in {
		out = append(out, key{k.Field, k.Env, k.Header, k.Prefix})
	}
	return out
}

func stdio(command string, args ...string) catalogue.Launch {
	return catalogue.Launch{Transport: catalogue.TransportStdio, Command: command, Args: args}
}

func remote(address string) catalogue.Launch {
	return catalogue.Launch{Transport: catalogue.TransportHTTP, URL: address}
}

// RECOGNITION MATCHES THE SEAT BINDINGS IT REPLACES, launch by launch: every
// server the table it replaces bound a seat's credential to is bound to the
// same keys, and every one it refused, a lookalike host or a launch on the
// wrong transport, is still refused. A seat's credential is handed to
// whatever this matches, so a recognition wider than the table's would hand
// it to a server nobody declared as reading it.
func TestRecognitionMatchesTheSeatBindingsItReplaces(t *testing.T) {
	t.Parallel()
	bound := func(i int) []key { return replaced[i].keys }
	cases := []struct {
		name   string
		launch catalogue.Launch
		vendor string
		keys   []key
	}{
		{"Datadog's US1 server", remote("https://mcp.datadoghq.com/v1/mcp"), "datadog", bound(0)},
		{"Datadog's US5 server", remote("https://mcp.us5.datadoghq.com/v1/mcp"), "datadog", bound(0)},
		{"Datadog's EU1 server", remote("https://mcp.datadoghq.eu/v1/mcp"), "datadog", bound(0)},
		{"a host under ddog-gov.com", remote("https://mcp.ddog-gov.com/"), "datadog", bound(0)},
		{"Datadog's site itself", remote("https://datadoghq.com/"), "datadog", bound(0)},
		{"a host whose name ends in datadoghq.com", remote("https://evil-datadoghq.com/"), "", nil},
		{"a host under someone else's domain", remote("https://datadoghq.com.example.net/"), "", nil},
		{"a Datadog host spelled on stdio", stdio("curl", "https://mcp.datadoghq.com"), "", nil},
		{"GitHub's remote server", remote("https://api.githubcopilot.com/mcp/"), "github", bound(1)},
		{"GitHub's remote server in capitals", remote("https://API.GITHUBCOPILOT.COM/mcp/"), "github", bound(1)},
		{"a host under GitHub's", remote("https://x.api.githubcopilot.com/mcp/"), "", nil},
		{"GitHub's local server", stdio("github-mcp-server", "stdio"), "github", bound(1)},
		{"GitHub's image", stdio("docker", "run", "-i", "--rm", "ghcr.io/github/github-mcp-server"), "github", bound(1)},
		{"glab serving MCP", stdio("glab", "mcp", "serve"), "gitlab", bound(2)},
		{"glab by its path", stdio("/usr/local/bin/glab", "mcp", "serve"), "gitlab", bound(2)},
		{"glab doing something else", stdio("glab", "issue", "list"), "", nil},
		{"GitLab's reference server", stdio("npx", "-y", "@modelcontextprotocol/server-gitlab"), "gitlab", bound(3)},
		{"a gitlab-mcp server", stdio("npx", "-y", "@zereight/gitlab-mcp"), "gitlab", bound(3)},
		{"slack-mcp-server", stdio("npm", "exec", "--yes", "--", "slack-mcp-server@1.3.0"), "slack", bound(4)},
		{"slack-mcp-server's image", stdio("docker", "run", "ghcr.io/korotovsky/slack-mcp-server"), "slack", bound(4)},
		{"slack-mcp-server named in an address", remote("https://example.com/slack-mcp-server"), "", nil},
		{"mcp-server-mattermost", stdio("uvx", "mcp-server-mattermost==0.6.1"), "mattermost", bound(5)},
		{"mcp-atlassian", stdio("uvx", "mcp-atlassian==0.23.1"), "atlassian", bound(6)},
		{"mcp-atlassian's image", stdio("docker", "run", "ghcr.io/sooperset/mcp-atlassian:latest"), "atlassian", bound(6)},
		{"a server nobody recognises", stdio("uvx", "some-server==1.0.0"), "", nil},
		{"an address nobody recognises", remote("https://mcp.example.com/"), "", nil},
		{"an address that does not parse", remote("://mcp.datadoghq.com"), "", nil},
	}
	for _, tc := range cases {
		vendor, keys, ok := catalogue.SeatKeysFor(tc.launch)
		if ok != (tc.vendor != "") || vendor != tc.vendor || !slices.Equal(keysOf(keys), tc.keys) {
			t.Errorf("%s: bound to %q with %v (%v), want %q with %v", tc.name, vendor, keysOf(keys), ok,
				tc.vendor, tc.keys)
		}
	}

	// THE CONVENTIONAL SERVER PER INTEGRATION, and every binding, in the
	// order the table it replaces kept within an integration.
	byVendor := map[string][][]key{}
	for _, b := range catalogue.Bindings() {
		byVendor[b.Vendor] = append(byVendor[b.Vendor], keysOf(b.Keys))
	}
	wantByVendor := map[string][][]key{}
	for _, r := range replaced {
		wantByVendor[r.vendor] = append(wantByVendor[r.vendor], r.keys)
		b, ok := catalogue.BindingFor(r.vendor)
		if !ok || b.Server != r.server || b.Vendor != r.vendor {
			t.Errorf("the conventional %s server is %+v (%v), want %q", r.vendor, b, ok, r.server)
		}
	}
	for vendor, want := range wantByVendor {
		got := byVendor[vendor]
		if len(got) != len(want) {
			t.Errorf("%s has %d bindings, want %d", vendor, len(got), len(want))
			continue
		}
		for i := range want {
			if !slices.Equal(got[i], want[i]) {
				t.Errorf("%s's binding %d reads %v, want %v", vendor, i, got[i], want[i])
			}
		}
	}
	if len(byVendor) != len(wantByVendor) {
		t.Errorf("bindings for %d integrations, want %d", len(byVendor), len(wantByVendor))
	}
	if _, ok := catalogue.BindingFor("jira"); ok {
		t.Error("a lane is not an integration: Jira's server is Atlassian's")
	}
}

// EVERY DATADOG SITE'S SERVER IS RECOGNISED AS DATADOG'S, so a company on any
// site gets its seats' application keys bound, whichever site config admits.
func TestEveryDatadogSitesServerIsRecognised(t *testing.T) {
	t.Parallel()
	for _, site := range datadog.Sites() {
		vendor, _, ok := catalogue.SeatKeysFor(remote("https://mcp." + site + "/v1/mcp"))
		if !ok || vendor != "datadog" {
			t.Errorf("Datadog's server on %s is not recognised as Datadog's", site)
		}
	}
}

// A SERVER IS RECOGNISED AS THE ENTRY IT WAS ADDED FROM, by address or
// command, and only a launch on the entry's own transport.
func TestAServerIsRecognisedByItsAddressOrItsCommand(t *testing.T) {
	t.Parallel()
	cases := []struct {
		launch catalogue.Launch
		id     string
	}{
		{remote("https://mcp.linear.app/mcp"), "linear"},
		{remote("https://mcp.atlassian.com/v1/mcp/authv2"), "rovo"},
		{remote("https://mcp.sentry.dev/mcp/my-org"), "sentry"},
		{remote("https://docs.mcp.cloudflare.com/mcp"), ""},
		{stdio("uvx", "postgres-mcp==0.3.0", "--access-mode=restricted"), "postgres"},
		{stdio("npx", "-y", "tavily-mcp@0.2.22"), "tavily"},
		{stdio("mcp.linear.app"), ""},
	}
	for _, tc := range cases {
		e, ok := catalogue.Recognise(tc.launch)
		if ok != (tc.id != "") || e.ID != tc.id {
			t.Errorf("%+v is recognised as %q (%v), want %q", tc.launch, e.ID, ok, tc.id)
		}
	}
}
