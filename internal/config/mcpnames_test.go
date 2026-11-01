package config

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent/cliprofile"
)

// namedDoc is a company with one server whose entry lines the case writes,
// and a seat and a unit that declare a block for it.
func namedDoc(server, block string) string {
	return `
name: Acme
mcp_servers:
` + server + `
units:
  - name: Engineering
    roles:
      - name: SRE
        handle: sre
` + block
}

// A SERVER'S NAME IS ONE EVERY PLACE IT LANDS CAN SPELL: lowercase letters,
// digits and hyphens, starting and ending with a letter or a digit, at most
// 32 characters. Refused at the name, with what to write.
//
// Mutation: let mcpServerName take an uppercase letter, or drop the length
// case, and this is red.
func TestAServerNameIsOneEveryClientCanSpell(t *testing.T) {
	t.Parallel()
	entry := func(name string) string {
		return "  - name: \"" + name + "\"\n    command: server\n"
	}
	for _, ok := range []string{"github", "mcp-atlassian", "x", "s3", "2fa-vault",
		strings.Repeat("a", MaxMCPServerName)} {
		cfg := mustCompany(t, namedDoc(entry(ok), ""))
		if cfg.MCPServers[0].Name != ok {
			t.Errorf("%q read back as %q", ok, cfg.MCPServers[0].Name)
		}
	}
	for _, tc := range []struct {
		name string
		want error
	}{
		{"GitHub", ErrUnknownValue},
		{"my_server", ErrUnknownValue},
		{"my.server", ErrUnknownValue},
		{"my server", ErrUnknownValue},
		{"tools/github", ErrUnknownValue},
		{"-github", ErrUnknownValue},
		{"github-", ErrUnknownValue},
		{"handbook²", ErrUnknownValue},
		{strings.Repeat("a", MaxMCPServerName+1), ErrOutOfRange},
	} {
		err := rejects(t, namedDoc(entry(tc.name), ""), "mcp_servers[0].name")
		if !errors.Is(err, tc.want) {
			t.Errorf("%q: refused as %v, want %v", tc.name, err, tc.want)
		}
	}
}

// EVERY NAME THE ENGINE READS A SERVER BY IS ONE THE RULE TAKES: the vendor
// blocks the engine reads by name whether or not a server has it
// (org.EngineReadMCPEnv), and the integrations and access lanes a per-seat
// server is recognised by when it is named after one (SeatCredentialKind), so
// a server named after its vendor, as the docs name every one, is never
// refused for its name. The bridge's own name stays reserved.
//
// Mutation: bound a name below the longest of them (a MaxMCPServerName of 9
// refuses confluence), and this is red.
func TestEveryNameTheEngineReadsAServerByIsOneTheRuleTakes(t *testing.T) {
	t.Parallel()
	names := slices.Concat(org.EngineReadMCPEnv(), SeatCredentialKinds(), SeatAccessLanes())
	if len(names) < 6 {
		t.Fatalf("read %d names, so the case asserts nothing", len(names))
	}
	for _, name := range names {
		server := MCPServer{Name: name}
		var p problems
		server.validateName(&p, field("name"))
		if err := p.err(); err != nil {
			t.Errorf("the engine reads a server by %q, and the name rule refuses it: %v", name, err)
		}
	}
	if !strings.Contains(fmt.Sprint(rejects(t, namedDoc("  - name: crewlet\n    command: server\n", ""),
		"mcp_servers[0].name")), "reserved") {
		t.Error("crewlet is not refused as reserved")
	}
}

// A TOOL PREFIX IS A MODEL'S TOOL-NAME CHARACTERS AND LEAVES ROOM FOR A NAME:
// every tool of the server is filed under the prefix and its own name run
// together, held to cliprofile.ToolNameMax, so a prefix that a model's name
// cannot hold, or that leaves no room, would refuse every tool the server
// lists.
//
// Mutation: let validateToolPrefix take a dot, or compare against
// ToolNameMax rather than one less, and this is red.
func TestAToolPrefixLeavesRoomForAName(t *testing.T) {
	t.Parallel()
	prefixed := func(prefix string) string {
		return "  - name: docs\n    command: server\n    tool_prefix: \"" + prefix + "\"\n"
	}
	room := cliprofile.ToolNameMax() - 1
	if room != MaxMCPToolPrefix() {
		t.Fatalf("MaxMCPToolPrefix() = %d, want %d: one less than the longest tool name", MaxMCPToolPrefix(), room)
	}
	for _, ok := range []string{"docs_", "Mattermost-", "gh", strings.Repeat("p", room)} {
		mustCompany(t, namedDoc(prefixed(ok), ""))
	}
	for _, tc := range []struct {
		prefix string
		want   error
	}{
		{"docs.", ErrUnknownValue},
		{"docs:", ErrUnknownValue},
		{"my docs_", ErrUnknownValue},
		{strings.Repeat("p", room+1), ErrOutOfRange},
	} {
		err := rejects(t, namedDoc(prefixed(tc.prefix), ""), "mcp_servers[0].tool_prefix")
		if !errors.Is(err, tc.want) {
			t.Errorf("%q: refused as %v, want %v", tc.prefix, err, tc.want)
		}
	}
}

// A VALUE'S KEY IS WHAT ITS SERVER IS STARTED WITH IT AS: an http server's a
// request header (RFC 9110's token), a stdio server's an environment
// variable. Held wherever a key for the server is written: its own env or
// headers, its seat_credentials, and every seat's and unit's mcp_env block for
// it.
//
// Mutation: hold an http server's keys to the variable grammar, skip the
// seats' blocks or the units', or skip seat_credentials, and this is red.
func TestAValuesKeyIsNamedAsItsServerReadsIt(t *testing.T) {
	t.Parallel()
	const http = "  - name: docs\n    transport: http\n    shared: false\n    url: https://docs.example.com/mcp\n"
	const stdio = "  - name: docs\n    command: server\n    shared: false\n"
	seat := func(key string) string {
		return "        mcp_env:\n          docs:\n            \"" + key + "\": \"${SRE_DOCS}\"\n"
	}
	// Readable as written: a header named with a token's punctuation, and a
	// variable with an underscore first.
	mustCompany(t, namedDoc(http+"    headers:\n      X-Api.Key_v2: \"${DOCS}\"\n", seat("Authorization")))
	mustCompany(t, namedDoc(stdio+"    env:\n      _DOCS_TOKEN: \"${DOCS}\"\n", seat("SRE_TOKEN")))

	for _, tc := range []struct {
		name, server, block, path string
	}{
		{"a header with a space", http + "    headers:\n      \"X Api Key\": \"${DOCS}\"\n", "",
			"mcp_servers[0].headers.X Api Key"},
		{"a header with a colon", http + "    headers:\n      \"Authorization:\": \"${DOCS}\"\n", "",
			"mcp_servers[0].headers.Authorization:"},
		{"a variable with a dot", stdio + "    env:\n      docs.token: \"${DOCS}\"\n", "",
			"mcp_servers[0].env.docs.token"},
		{"a variable with a hyphen", stdio + "    env:\n      DOCS-TOKEN: \"${DOCS}\"\n", "",
			"mcp_servers[0].env.DOCS-TOKEN"},
		{"a variable starting with a digit", stdio + "    env:\n      1TOKEN: \"${DOCS}\"\n", "",
			"mcp_servers[0].env.1TOKEN"},
		{"a seat's header", http, seat("X Seat Token"), "roles[0].mcp_env.docs.X Seat Token"},
		{"a seat's variable", stdio, seat("SRE-TOKEN"), "roles[0].mcp_env.docs.SRE-TOKEN"},
		{"a seat credential's header", http +
			"    seat_credentials:\n      - kind: github\n        key: \"Bearer Token\"\n", "",
			"mcp_servers[0].seat_credentials[0].key"},
		{"a seat credential's variable", stdio +
			"    seat_credentials:\n      - kind: github\n        key: GITHUB.TOKEN\n", "",
			"mcp_servers[0].seat_credentials[0].key"},
	} {
		err := rejects(t, namedDoc(tc.server, tc.block), tc.path)
		if !errors.Is(err, ErrUnknownValue) {
			t.Errorf("%s: refused as %v, want ErrUnknownValue", tc.name, err)
		}
	}

	unit := namedDoc(stdio, "") + "    mcp_env:\n      docs:\n        TEAM.TOKEN: \"${ENG_DOCS}\"\n"
	if err := rejects(t, unit, "units[0].mcp_env.docs.TEAM.TOKEN"); !errors.Is(err, ErrUnknownValue) {
		t.Errorf("a unit's variable: refused as %v, want ErrUnknownValue", err)
	}
}

// AN ADDRESS IS HTTPS, OR PLAIN HTTP TO A HOST NO PUBLIC NETWORK ROUTES TO,
// and never carries a user name or password. Judged by what the address says
// rather than what it resolves to, and never quoting it.
//
// Mutation: drop the user check, count every name with a dot as public, or
// let CGNAT's 100.64.0.0/10 count as private, and this is red.
func TestAnAddressIsHTTPSOrPrivate(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{
		"https://mcp.example.com/mcp",
		"https://mcp.example.com:8443/v1/mcp?region=eu",
		"http://localhost:3000/mcp",
		"http://127.0.0.1:8080/mcp",
		"http://[::1]:8080/mcp",
		"http://10.2.3.4/mcp",
		"http://172.16.0.9/mcp",
		"http://192.168.1.20:8000/mcp",
		"http://169.254.10.10/mcp",
		"http://[fd12:3456::1]/mcp",
		"http://github-mcp:8080/mcp",
		"http://github-mcp.tools.svc.cluster.local:8080/mcp",
		"http://dev.localhost:3000/mcp",
		"http://nas.home.arpa/mcp",
		"http://wiki.corp.internal/mcp",
		"http://printer.local/mcp",
	} {
		if problem := MCPAddressProblem(ok); problem != "" {
			t.Errorf("%s was refused: %s", ok, problem)
		}
	}
	for _, refused := range []string{
		"http://mcp.example.com/mcp",
		"http://100.64.1.1/mcp",
		"http://8.8.8.8/mcp",
		"http://mcp.example.local.example.com/mcp",
		"https://user:secret-password@mcp.example.com/mcp",
		"https://token@mcp.example.com/mcp",
		"ftp://mcp.example.com/mcp",
		"mcp.example.com/mcp",
		"https:///mcp",
		" https://mcp.example.com/mcp",
		"",
	} {
		problem := MCPAddressProblem(refused)
		if problem == "" {
			t.Errorf("%q was accepted", refused)
			continue
		}
		for _, part := range []string{"secret-password", "token@", "mcp.example.com"} {
			if strings.Contains(refused, part) && strings.Contains(problem, part) {
				t.Errorf("the refusal of %q quotes the address: %s", refused, problem)
			}
		}
	}

	server := func(url string) string {
		return "  - name: docs\n    transport: http\n    url: \"" + url + "\"\n"
	}
	for _, url := range []string{"http://mcp.example.com/mcp", "https://me:pw@mcp.example.com/mcp"} {
		if err := rejects(t, namedDoc(server(url), ""), "mcp_servers[0].url"); !errors.Is(err, ErrUnknownValue) {
			t.Errorf("%s: refused as %v, want ErrUnknownValue", url, err)
		}
	}
	// A ${VAR} in the address is judged once it resolves, where the engine
	// dials it: the document cannot show what it will be.
	for _, url := range []string{"${DOCS_URL}", "http://${DOCS_HOST}/mcp"} {
		mustCompany(t, namedDoc(server(url), ""))
	}
}

// THE NAME RULES ARE ADMISSION RULES: a stored revision that breaks one still
// applies, and says so in its warnings, located exactly where a write keeping
// it is refused. Nothing about running depends on a name being well formed,
// and refusing to apply one would take a working company down over a rule
// its author never saw.
//
// Mutation: call validateMCPNames from the runnable rules, and this is red.
func TestANameNoClientCanUseIsRefusedAtAdmissionAndWarnedAfter(t *testing.T) {
	t.Parallel()
	doc := namedDoc("  - name: Docs_Server\n    transport: http\n    url: http://docs.example.com/mcp\n"+
		"    tool_prefix: \"docs.\"\n    headers:\n      \"X Token\": \"${DOCS}\"\n", "")
	cfg, err := ParseCompanyDocument([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.ValidateRunnable(); err != nil {
		t.Fatalf("a stored revision with names no client can use was refused to apply: %v", err)
	}
	var paths []string
	for _, w := range cfg.AdmissionWarnings() {
		if w.Kind == WarningAdmission && strings.HasPrefix(w.Path, "mcp_servers[0].") {
			paths = append(paths, w.Path)
		}
	}
	slices.Sort(paths)
	want := []string{
		"mcp_servers[0].headers.X Token",
		"mcp_servers[0].name",
		"mcp_servers[0].tool_prefix",
		"mcp_servers[0].url",
	}
	if !slices.Equal(paths, want) {
		t.Errorf("warned at %v, want %v", paths, want)
	}
}

// EVERY SERVER THE DOCS, THE EXAMPLES AND THE SKILLS SPELL OUT KEEPS THE
// ENTRY RULES: a name, a prefix, an address and the keys of its values, and of
// every seat's and unit's block for it, that a reader copies and the engine
// takes. A page holding a fragment the loader would refuse for some other
// reason is still held to these.
func TestEveryDocumentedServerKeepsTheEntryRules(t *testing.T) {
	t.Parallel()
	servers := 0
	for _, block := range documentedBlocks(t) {
		for i := range block.MCPServers {
			server := &block.MCPServers[i]
			if server.Name == "" {
				continue
			}
			servers++
			doc := Company{MCPServers: []MCPServer{*server}}
			for _, problem := range Problems(doc.validateMCPNames()) {
				t.Errorf("%s: %s: %s", block.where, problem.Path, problem.Message)
			}
			var blocks []map[string]string
			for _, seat := range block.Roles {
				blocks = append(blocks, seat.MCPEnv[server.Name])
			}
			var walk func([]docUnit)
			walk = func(units []docUnit) {
				for _, unit := range units {
					blocks = append(blocks, unit.MCPEnv[server.Name])
					for _, seat := range unit.Roles {
						blocks = append(blocks, seat.MCPEnv[server.Name])
					}
					walk(unit.Children)
				}
			}
			walk(block.Units)
			for _, values := range blocks {
				for key := range values {
					if problem := server.keyProblem(key); problem != "" {
						t.Errorf("%s: an mcp_env block for %s: %s", block.where, server.Name, problem)
					}
				}
			}
		}
	}
	if servers < 20 {
		t.Fatalf("read %d documented servers, so the sweep asserts nothing", servers)
	}
}

// THE SCHEMA STATES A NAME AND A PREFIX AS THE VALIDATOR HOLDS THEM: the js
// tags on MCPServer.name and tool_prefix are a second spelling of the rules,
// for an editor to underline what `crewlet validate` would refuse. A prefix's
// bound follows the longest CLI prefix, which a tag cannot compute, so this is
// what moves it: a profile with a longer prefix fails here until the tag does.
//
// Mutation: loosen either tag's pattern or bound, and this is red.
func TestTheSchemaStatesANameAndAPrefixAsTheValidatorDoes(t *testing.T) {
	t.Parallel()
	defs, _ := schemaDoc(t, TierCompany)["$defs"].(map[string]any)
	def, ok := defs["MCPServer"].(map[string]any)
	if !ok {
		t.Fatal("the company schema's $defs has no MCPServer")
	}
	props, _ := def["properties"].(map[string]any)
	for _, tc := range []struct {
		field, pattern string
		bound          int
	}{
		{"name", mcpServerName.String(), MaxMCPServerName},
		{"tool_prefix", "^[A-Za-z0-9_-]+$", MaxMCPToolPrefix()},
	} {
		field, ok := props[tc.field].(map[string]any)
		if !ok {
			t.Fatalf("MCPServer has no %s", tc.field)
		}
		if got := ownPattern(t, field); got != tc.pattern {
			t.Errorf("%s pattern = %q, the validator's is %q", tc.field, got, tc.pattern)
		}
		var bound any
		for _, branch := range anyOf(field) {
			if own, _ := branch.(map[string]any); own["pattern"] != nil {
				bound = own["maxLength"]
			}
		}
		if fmt.Sprint(bound) != fmt.Sprint(tc.bound) {
			t.Errorf("%s maxLength = %v, the validator's bound is %d", tc.field, bound, tc.bound)
		}
	}
	for _, prefix := range []string{"docs_", "Mattermost-", "gh"} {
		if !cliprofile.ToolNameChars(prefix) {
			t.Errorf("the schema's prefix pattern admits %q and the validator does not", prefix)
		}
	}
}
