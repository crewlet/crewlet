package executor

import (
	"path"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// cursorGuardServers is a run's servers: the bridge's own and a seat's.
var cursorGuardServers = map[string]sandbox.MCPServer{
	"crewlet": {Name: "crewlet", Transport: sandbox.TransportHTTP, URL: "http://127.0.0.1:9/mcp", AlwaysLoaded: true,
		Tools: []string{"submit_work", "ask_and_wait"}},
	"gitlab": {Name: "gitlab", Transport: sandbox.TransportHTTP, URL: "https://gitlab.example.com/mcp"},
}

// cursorGuardFiles is where a Cursor box keeps the two permissions files.
func cursorGuardFiles(b *sandbox.FakeSandbox) (project, home string) {
	return path.Join(b.Home(), sandbox.WorkspaceSubdir, cursorProjectConfig),
		PathsFor(b).CursorDir() + "/cli-config.json"
}

// cursorLaunch writes a Cursor run's configuration in a box holding the
// start directory's file (project) and the home's, each where not "".
func cursorLaunch(t *testing.T, project, home string) (string, error) {
	t.Helper()
	b := sandbox.NewFakeSandbox("box-1")
	p, h := cursorGuardFiles(b)
	if project != "" {
		b.Put(p, project)
	}
	if home != "" {
		b.Put(h, home)
	}
	_, err := CursorAgent{}.WriteConfig(t.Context(), b, sandbox.RunRequest{Brief: "go",
		MCPServers: cursorGuardServers}, Session{}, PathsFor(b))
	if err != nil {
		return "", err
	}
	raw, readErr := b.ReadFile(t.Context(), PathsFor(b).MCPConfig())
	if readErr != nil {
		t.Fatal(readErr)
	}
	return string(raw), nil
}

// homeDenies is a home cli-config.json denying rules.
func homeDenies(rules ...string) string {
	return `{"version":1,"permissions":{"allow":[],"deny":[` + cursorRuleList(rules) + `]}}`
}

// projectDenies is a start directory's cli.json denying rules.
func projectDenies(rules ...string) string {
	return `{"permissions":{"allow":[],"deny":[` + cursorRuleList(rules) + `]}}`
}

// cursorRuleList is rules as the items of a JSON list.
func cursorRuleList(rules []string) string {
	out := make([]string, len(rules))
	for i, r := range rules {
		out[i] = `"` + r + `"`
	}
	return strings.Join(out, ",")
}

// A PERSON'S DENY REACHING A CURSOR RUN'S BRIDGE REFUSES THE RUN, naming the
// file and the rule: Cursor 2026.11.22-e9042b2 honours every Mcp rule under
// --force, so the run would lose its bridge (measured). Whole wildcards, a
// prefix or mid-name glob, a tool pattern, and a rule written in another
// case or with spaces all reach the run's crewlet-<suffix> server.
//
// Mutation: drop the guard from WriteConfig, or take `*` literally: red.
func TestACursorDenyReachingTheBridgeRefusesTheRun(t *testing.T) {
	t.Parallel()
	for _, rule := range []string{"Mcp(*:*)", "Mcp(crewlet-*:*)", "Mcp(*:submit_work)", "Mcp(c*let-*:*_work)",
		" Mcp( CREWLET-* : ASK_AND_WAIT ) "} {
		t.Run(rule, func(t *testing.T) {
			t.Parallel()
			_, err := cursorLaunch(t, "", homeDenies(rule))
			if err == nil || !strings.Contains(err.Error(), "/.cursor/cli-config.json lists \""+rule+"\"") ||
				!strings.Contains(err.Error(), "lose its bridge") {
				t.Errorf("the launch returned %v, want it refused naming the home's file, the rule and the bridge", err)
			}
		})
	}
}

// A DENY THAT DOES NOT REACH THE BRIDGE LETS A CURSOR RUN START: one of a
// seat's own server, which a person may switch off as in their own session,
// another tool family, and the forms Cursor matches nothing by (measured): a
// family in another case, a server alone, `Mcp(*)`, a second colon.
//
// Mutation: hold every server of the run to the guard, compare the family
// without case, or take a server-only rule: red.
func TestACursorDenyOfAnotherServerLetsTheRunStart(t *testing.T) {
	t.Parallel()
	for _, rule := range []string{"Mcp(gitlab-*:*)", "Shell(*)", "mcp(crewlet-*:*)", "Mcp(crewlet-*)",
		"Mcp(*)", "Mcp(crewlet-*:*:x)"} {
		t.Run(rule, func(t *testing.T) {
			t.Parallel()
			if _, err := cursorLaunch(t, "", homeDenies(rule)); err != nil {
				t.Errorf("the launch was refused: %v", err)
			}
		})
	}
}

// A CURSOR START DIRECTORY'S PERMISSIONS BLOCK REPLACES THE HOME'S WHOLE
// (measured): a block allowing and denying nothing lets the run start under a
// home denying every MCP tool, a block denying the bridge refuses it, named,
// under a home denying nothing, and a file holding no block leaves the
// home's in force.
//
// Mutation: read the home's file whatever the start directory holds: red.
func TestACursorStartDirectorysBlockReplacesTheHomes(t *testing.T) {
	t.Parallel()
	if _, err := cursorLaunch(t, projectDenies(), homeDenies("Mcp(*:*)")); err != nil {
		t.Errorf("under the start directory's empty block the launch was refused: %v", err)
	}
	_, err := cursorLaunch(t, projectDenies("Mcp(crewlet-*:*)"), homeDenies())
	if err == nil || !strings.Contains(err.Error(), "/workspace/.cursor/cli.json lists \"Mcp(crewlet-*:*)\"") {
		t.Errorf("under the start directory's deny the launch returned %v, want it refused naming that file", err)
	}
	_, err = cursorLaunch(t, `{}`, homeDenies("Mcp(*:*)"))
	if err == nil || !strings.Contains(err.Error(), "/.cursor/cli-config.json lists") {
		t.Errorf("under a start directory's file with no block the launch returned %v, want the home's deny", err)
	}
}

// A PERMISSIONS FILE CURSOR REJECTS GETS ITS MEASURED VERDICT. A home file that
// is no JSON object, a deny entry no string, a deny or allow no list, or
// permissions no object, Cursor rewrites to its defaults, which deny nothing:
// the run starts. A home block without an allow list it takes. A start
// directory's file that is no JSON object, or a block without an allow list,
// stops Cursor before any call, its own error naming the file: the guard
// leaves it to Cursor.
//
// Mutation: hold the rules of a rejected home file, or ask the home for an
// allow list: red.
func TestACursorPermissionsFileCursorRejectsGetsItsMeasuredVerdict(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		project, home string
		refused       bool
	}{
		"a home file that is no JSON object": {home: `{"version":1,"permissions":{"allow":[],"deny":["Mcp(*:*)"],}}`},
		"a home deny entry no string":        {home: `{"version":1,"permissions":{"allow":[],"deny":["Mcp(*:*)",42]}}`},
		"a home deny no list":                {home: `{"version":1,"permissions":{"allow":[],"deny":"Mcp(*:*)"}}`},
		"a home allow no list":               {home: `{"version":1,"permissions":{"allow":"x","deny":["Mcp(*:*)"]}}`},
		"home permissions no object":         {home: `{"version":1,"permissions":"Mcp(*:*)"}`},
		"a home block without allow":         {home: `{"version":1,"permissions":{"deny":["Mcp(*:*)"]}}`, refused: true},
		"a start directory file no JSON object": {project: `{"permissions":{"allow":[],"deny":["Mcp(*:*)"],}}`,
			home: homeDenies("Mcp(*:*)")},
		"a start directory block without allow": {project: `{"permissions":{"deny":["Mcp(*:*)"]}}`,
			home: homeDenies("Mcp(*:*)")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := cursorLaunch(t, tc.project, tc.home)
			if refused := err != nil; refused != tc.refused {
				t.Errorf("the launch returned %v, want refused %v", err, tc.refused)
			}
		})
	}
}

// CURSOR MATCHES AN MCP DENY AS MEASURED on 2026.11.22-e9042b2: the exact server
// and tool, `*` as a whole part, a prefix or mid-name, names without case,
// spaces trimmed; `?` and `[` literal; nothing for a family in another case,
// a server alone or a second colon; and a server whose tools are not listed
// only by a pattern matching every name.
//
// Mutation: compare with case, trim nothing, or take `?` as one character: red.
func TestCursorMatchesAnMCPDenyAsMeasured(t *testing.T) {
	t.Parallel()
	tools := []string{"submit_work"}
	for _, tc := range []struct {
		rule  string
		tools []string
		want  bool
	}{
		{"Mcp(crewlet-ab12cd34:submit_work)", tools, true},
		{"Mcp(CREWLET-AB12CD34:SUBMIT_WORK)", tools, true},
		{"Mcp( crewlet-ab12cd34 : submit_work ) ", tools, true},
		{"Mcp(crewlet-*:*)", tools, true},
		{"Mcp(*-ab12cd34:submit*)", tools, true},
		{"Mcp(crewlet-ab12cd3?:*)", tools, false},
		{"Mcp(crewlet-[a]b12cd34:*)", tools, false},
		{"MCP(crewlet-ab12cd34:*)", tools, false},
		{"Mcp(crewlet-ab12cd34)", tools, false},
		{"Mcp(crewlet-ab12cd34:submit_work:x)", tools, false},
		{"Mcp(crewlet-ab12cd34:other)", tools, false},
		{"Mcp(crewlet-ab12cd34:*)", nil, true},
		{"Mcp(crewlet-ab12cd34:submit_work)", nil, false},
	} {
		if _, got := cursorDeniedTool(tc.rule, "crewlet-ab12cd34", tc.tools); got != tc.want {
			t.Errorf("%q on crewlet-ab12cd34 (tools %v) matches %v, want %v", tc.rule, tc.tools, got, tc.want)
		}
	}
}
