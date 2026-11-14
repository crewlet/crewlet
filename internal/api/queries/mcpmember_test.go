package queries_test

import (
	"context"
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/statelog"
)

// person is a signed-in person holding grants, proved an hour ago.
func person(stage iam.Stage, grants ...iam.Grant) iam.Principal {
	return iam.Principal{ID: uuid.New(), Login: "jane.doe", Kind: iam.KindPerson, Stage: stage,
		Grants: grants, ReauthAt: time.Now().Add(-time.Hour)}
}

// everyAppButAdmin is every grant there is but admin.
func everyAppButAdmin() []iam.Grant {
	return slices.DeleteFunc(slices.Clone(iam.AllGrants), func(g iam.Grant) bool { return g == iam.GrantAdmin })
}

// THE READING IS THE CONFIGURATION READER'S OR A MEMBER'S, decided by
// `config.read`'s own row: an administrator reads the whole answer, at the
// keyboard or through the assistant's read grant; a person holding every app
// but admin, a suspended administrator and a caller nobody resolved read a
// member's.
//
// Mutation: decide the reading by any app, or by the person alone without the
// stage, and this is red.
func TestTheReadingIsTheConfigurationReadersOrAMembers(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		ctx  context.Context
		want string
	}{
		{"an administrator", iam.WithPrincipal(t.Context(), person(iam.StageActive, iam.GrantAdmin)),
			queries.ReadingAdmin},
		{"an administrator through the assistant's read grant",
			through(person(iam.StageActive, iam.GrantAdmin)), queries.ReadingAdmin},
		{"a person holding every app but admin",
			iam.WithPrincipal(t.Context(), person(iam.StageActive, everyAppButAdmin()...)), queries.ReadingMember},
		{"a suspended administrator", iam.WithPrincipal(t.Context(), person(iam.StageSuspended, iam.GrantAdmin)),
			queries.ReadingMember},
		{"nobody resolved", t.Context(), queries.ReadingMember},
	} {
		if got := queries.MCPReading(tc.ctx); got != tc.want {
			t.Errorf("%s reads %q, want %q", tc.name, got, tc.want)
		}
	}
}

// THE SERVERS' LIST ADMITS WHOM ITS ROUTES' VERB DOES: `mcp_servers_status` is
// the overview's posture, and the detail and the catalogue beside it are
// `tools.read`, which must stay everybody's at any recency and the assistant's
// to read, or a member would read the list and be refused a server's page.
//
// Mutation: narrow tools.read to admin, or put the question back on admin, and
// this is red.
func TestTheServersListAdmitsWhomItsRoutesVerbDoes(t *testing.T) {
	t.Parallel()
	r := queries.NewRegistry()
	queries.Register(r, mcpSources(t, coordmemory.New()))
	if posture := r.PostureOf("mcp_servers_status"); !posture.IsEverybody() {
		t.Errorf("mcp_servers_status is asked as %s, want everybody", posture)
	}
	class, _ := authz.ClassOf(authz.ActionToolsRead)
	recency, _ := authz.RecencyOf(authz.ActionToolsRead)
	assist, _ := authz.AssistOf(authz.ActionToolsRead)
	if class != authz.ClassEverybody || recency != iam.RecencyAny || assist != authz.AssistRead {
		t.Errorf("tools.read is %s at %s, %s to the assistant; want everybody's at any recency, a read",
			class, recency, assist)
	}
}

// memberCompany declares a local server boxes are given (one an e2b box runs
// outside the company's network), a remote server on a private address with a
// header, and a per-agent server the engine binds each seat's credential to.
const memberCompany = `
name: Acme
providers:
  sandbox:
    local: {image: "ghcr.io/example/box:1"}
    e2b: {api_key: "${E2B_API_KEY}"}
    default_run_in: container
  llm:
    main: {model: sonnet}
    cloud: {model: sonnet, run_in: e2b}
mcp_servers:
  - name: docs
    command: docs-mcp
    args: ["--read-only"]
    tools: {default: allow}
    env:
      DOCS_TOKEN: "${DOCS_TOKEN}"
  - name: intranet
    transport: http
    url: http://10.0.0.5:8080/mcp
    tools: {default: allow}
    headers:
      X-Intranet-Key: "${INTRANET_KEY}"
  - name: github
    shared: false
    command: github-mcp-server
    args: ["stdio"]
roles:
  - name: SWE
    handle: swe
    llm: main
    sandbox: {mcp: {servers: [docs]}}
  - name: Researcher
    handle: researcher
    llm: cloud
    sandbox: {mcp: {servers: [intranet, docs]}}
`

// A MEMBER'S READING NAMES NO LAUNCH LINE, HEADER, VARIABLE, ACCOUNT OR CALL
// DETAIL, in the list or a server's detail: no command, argument or address,
// no failure's sentence (the node's, an instance's, or why a node did not
// answer), no wait's sentence (the variables it waits for), no header or
// variable a seat's credential goes in, no box or placement warning, no
// instructions and nothing removing the server would take with it. It still
// reads each server's state, its failures by class, its tools and how it
// signs in. Every sentinel below is in the administrator's reading of the same
// fleet, so the test can fail.
//
// Mutation: answer a member the admin's row, keep `said` on a member's
// failure, or copy the binding's keys, and this is red.
func TestAMembersReadingNamesNoLaunchLineHeaderVariableAccountOrCallDetail(t *testing.T) {
	t.Parallel()
	cfg, err := config.ParseCompany([]byte(memberCompany))
	if err != nil {
		t.Fatal(err)
	}
	organization, err := cfg.Organization()
	if err != nil {
		t.Fatal(err)
	}
	swe, ok := organization.AgentIDFor(organization.AgentSeatByHandle("swe"))
	if !ok {
		t.Fatal("the fixture has no swe seat")
	}
	down := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	backend := coordmemory.New()
	presentNode(t, backend, "node-a", &coord.NodeStatus{MCP: []coord.MCPServerStatus{
		{Server: "github", Started: 1, Failed: 1, Waiting: 1, Tools: 1, ErrorSeat: swe, WaitSeat: swe,
			Error:      "exec: github-mcp-server not found under /opt/launch-sentinel",
			ErrorClass: string(mcp.FailCommandNotFound), DownSince: down, Restarts: 2,
			Wait: "waits for GITHUB_TOKEN, which the github integration mints"},
		{Server: "intranet", Failed: 1, Error: "401 from the server: Bearer rejected",
			ErrorClass: string(mcp.FailCredentialRejected), DownSince: down},
	}})
	surface, _ := configSurface(t, memberCompany)
	s := queries.Sources{Coord: backend, Company: companySource(t, cfg), Config: surface,
		Builtins: catalogueOffer,
		MCPListings: fakeListings{listings: []coord.MCPListing{{Server: "github", Node: "node-a",
			Epoch: epochs(t, backend)["node-a"], At: down, Instances: 1,
			Info:         &coord.MCPServerInfo{Name: "github-mcp", Version: "1.4.0"},
			Instructions: "instructions-sentinel: search before you create",
			Tools:        []coord.MCPListedTool{listed("gh_get_issue", "Read one issue", readOnly, 1)}}}},
		MCPInstances: &fakeInstances{read: coord.MCPInstanceRead{
			Instances: []coord.MCPInstance{
				{Server: "github", Seat: swe, Node: "node-a", State: coord.MCPInstanceFailed, Since: down,
					Restarts: 2, Failure: &coord.MCPInstanceFailure{Class: string(mcp.FailCredentialRejected),
						Said: "said-sentinel: Bad credentials", At: down},
					LastFailure: &coord.MCPInstanceFailure{Class: string(mcp.FailCredentialRejected),
						Said: "said-sentinel: Bad credentials", At: down}},
				{Server: "github", Seat: swe, Node: "node-a", State: coord.MCPInstanceWaiting, Since: down,
					Wait: "wait-sentinel: GITHUB_TOKEN"},
			},
			Nodes: []coord.MCPInstanceNode{{Node: "node-a", Answered: true, Count: 2},
				{Node: "node-b", Reason: "reason-sentinel: no answer within 2s"}},
		}},
		// AND ITS LATEST TEST, whose stages and failure quote what the
		// server said and where it was dialled.
		MCPChecks: fakeChecks{"github": {Server: "github", Seat: swe, At: down, By: "jane.doe",
			ByKind: iam.ActorHuman, Nodes: []coord.MCPCheckNode{{Node: "node-a", Answered: true,
				Steps: []coord.MCPCheckStep{{Stage: string(mcp.StageLaunch), State: string(mcp.StepFailed),
					Detail: "check-sentinel: exited"}},
				Failure: &coord.MCPInstanceFailure{Class: string(mcp.FailExited), Said: "check-sentinel: exited"}}}}},
	}
	// AND THE MCP ALARMS STANDING ON THE NODE, whose details quote the
	// failure and the wait a member's reading leaves out.
	s.Alarms = func() []statelog.Alarm {
		return []statelog.Alarm{{Kind: statelog.KindMCPServerPartial, Detail: "alarm-sentinel: github is down",
			Remedy: "Fix it.", Entries: []statelog.AlarmEntry{{Key: "github", Since: down, Seat: "swe",
				Detail: "alarm-sentinel: github is down"}}}}
	}
	// AND THE HISTORY the detail reads its usage and recent calls from, which
	// a member's reading leaves out whole.
	s, _ = withHistory(t, s)
	r := queries.NewRegistry()
	queries.Register(r, s)
	// read is what ctx's principal is answered: the list, and each server's
	// detail as the route answers it.
	read := func(ctx context.Context) map[string]string {
		t.Helper()
		list, err := r.Answer(ctx, "mcp_servers_status", nil)
		if err != nil {
			t.Fatalf("mcp_servers_status: %v", err)
		}
		raw, _ := json.Marshal(list)
		out := map[string]string{"the list": string(raw)}
		for _, server := range []string{"github", "docs"} {
			detail, err := s.MCPServer(ctx, server)
			if err != nil {
				t.Fatalf("MCPServer(%s): %v", server, err)
			}
			var shown any = detail
			if queries.MCPReading(ctx) != queries.ReadingAdmin {
				shown = queries.MemberMCPServer(detail)
			}
			raw, _ := json.Marshal(shown)
			out[server+"'s detail"] = string(raw)
		}
		return out
	}
	admin := read(iam.WithPrincipal(t.Context(), person(iam.StageActive, iam.GrantAdmin)))
	member := read(iam.WithPrincipal(t.Context(), person(iam.StageActive, everyAppButAdmin()...)))

	github, err := s.MCPServer(t.Context(), "github")
	if err != nil {
		t.Fatal(err)
	}
	if github.Resolved == nil || github.Resolved.Binding == nil || len(github.Resolved.Binding.Keys) == 0 {
		t.Fatalf("github resolves no bound credential (%+v), so this proves nothing about the keys", github.Resolved)
	}
	leaks := map[string][]string{
		"the list": {`"command":`, `"args":`, `"url":`, `"error":`, `"error_seat":`, `"wait":`,
			`"wait_seat":`, `"unlisted_annotations":`, `"alarms":`, `"approvals":`, "docs-mcp", "--read-only", "github-mcp-server",
			"10.0.0.5", "launch-sentinel", "Bearer rejected", "GITHUB_TOKEN", "alarm-sentinel"},
		"github's detail": {`"command":`, `"args":`, `"url":`, `"error":`, `"wait":`, `"said":`, `"reason":`,
			`"instructions":`, `"dependants":`, `"keys":`, `"last_check":`, `"values":`, `"bound_by":`,
			"github-mcp-server", "launch-sentinel",
			"GITHUB_TOKEN", "said-sentinel", "wait-sentinel", "reason-sentinel", "instructions-sentinel",
			"check-sentinel", github.Resolved.Binding.Keys[0].Key},
		"docs's detail": {`"command":`, `"args":`, `"box_seats":`, `"warnings":`, `"dependants":`, "docs-mcp",
			"--read-only", "said-sentinel", "reason-sentinel"},
	}
	for what, words := range leaks {
		if !strings.Contains(admin[what], `"reading":"admin"`) || !strings.Contains(member[what], `"reading":"member"`) {
			t.Errorf("%s does not say which reading it is:\nadmin %s\nmember %s", what, admin[what], member[what])
		}
		for _, leak := range words {
			if !strings.Contains(admin[what], leak) {
				t.Errorf("the administrator's reading of %s lacks %s, so its absence proves nothing", what, leak)
			}
			if strings.Contains(member[what], leak) {
				t.Errorf("a member's reading of %s carries %s: %s", what, leak, member[what])
			}
		}
	}
	for what, facts := range map[string][]string{
		"the list": {`"name":"github"`, `"state":"partial"`, `"error_class":"command_not_found"`,
			`"restarts":2`, `"state":"failing"`, `"error_class":"credential_rejected"`,
			`"down_since":"2026-11-01T09:00:00Z"`},
		"github's detail": {`"name":"gh_get_issue"`, `"class":"credential_rejected"`, `"seat":"swe"`,
			`"state":"waiting"`, `"answered":false`, `"count":2`, `"auth":"integration"`, `"vendor":"github"`,
			`"name":"github-mcp"`, `"version":"1.4.0"`},
		"docs's detail": {`"auth":"headers"`, `"runs_on":"engine_host"`, `"startup_seconds":120`},
	} {
		for _, fact := range facts {
			if !strings.Contains(member[what], fact) {
				t.Errorf("a member's reading of %s lacks %s: %s", what, fact, member[what])
			}
		}
	}
}

// A MEMBER'S READING CARRIES EXACTLY THE KEYS ITS ALLOW-LIST NAMES.
//
// Each admin answer is filled whole, every field of every struct it holds set
// by reflection (so a field somebody adds is set too), read as a member, and
// every key of the JSON a member is sent is walked against the allow-list: a
// key it does not name fails here, so a field added to a type both readings
// share cannot reach a member by default, and a key it names that the reading
// does not carry fails too, so the list never claims what is not sent. The
// built-in groups are everybody's on `GET /tools` and a tool's schemas are the
// server's own, so neither is walked.
//
// Mutation: copy `wait` onto a member's node cell, add a field to
// MCPStatusNode, or drop `count` from a node asked, and this is red.
func TestAMembersReadingCarriesExactlyTheKeysItsAllowListNames(t *testing.T) {
	t.Parallel()
	var status queries.MCPStatusAnswer
	fill(reflect.ValueOf(&status).Elem())
	var detail queries.MCPServerAnswer
	fill(reflect.ValueOf(&detail).Elem())

	row := []string{"name", "title", "catalogue", "vendor", "configured", "shared", "enabled", "description",
		"transport", "state", "started", "failed", "waiting", "tools", "refused_tools", "refused_tools[].tool",
		"refused_tools[].reason", "refused_tools[].message", "refused_count", "shadowed_tools",
		"shadowed_tools[].tool", "shadowed_tools[].by", "shadowed_tools[].message", "shadowed_count", "nodes",
		"nodes[].node", "nodes[].reported", "nodes[].started", "nodes[].failed", "nodes[].waiting",
		"nodes[].tools", "nodes[].error_class", "nodes[].down_since", "nodes[].restarts"}
	failure := []string{"class", "at", "node", "seat"}
	under := func(prefix string, keys []string) []string {
		out := []string{prefix}
		for _, k := range keys {
			out = append(out, prefix+"."+k)
		}
		return out
	}
	statusKeys := slices.Concat([]string{"reading", "servers", "nodes", "nodes[].id", "nodes[].reported",
		"builtin"}, under("servers[]", row)[1:])
	detailKeys := slices.Concat([]string{"reading", "nodes", "nodes[].id", "nodes[].reported",
		"last_success"},
		under("server", row),
		under("listed", nil), under("listed[]", []string{"node", "at", "instances", "tools"})[1:],
		under("tools", nil), under("tools[]", []string{"name", "raw", "title", "description", "annotations",
			"annotations.read_only", "annotations.destructive", "annotations.idempotent",
			"annotations.open_world", "risk", "risk_declared", "external", "digest", "definitions",
			"instances", "nodes", "input_schema", "output_schema", "level", "custom", "held", "removed"})[1:],
		under("server_info", []string{"name", "title", "version", "protocol"}),
		under("instances", nil), under("instances[]", []string{"seat", "node", "state", "since", "restarts",
			"tools", "last_answer"})[1:],
		under("instances[].failure", failure), under("instances[].last_failure", failure),
		under("instance_nodes", nil), under("instance_nodes[]", []string{"node", "answered", "count"})[1:],
		under("last_failure", failure),
		under("resolved", []string{"auth", "runs_on", "timeouts", "timeouts.startup_seconds",
			"timeouts.request_seconds", "timeouts.startup_default", "timeouts.request_default", "binding",
			"binding.vendor", "binding.by"}),
		under("usage", []string{"since", "until", "days", "elided", "tools"}),
		under("usage.tools[]", []string{"tool", "calls", "failed", "unavailable", "refused", "total_ms",
			"last_at"})[1:])

	for _, tc := range []struct {
		name    string
		admin   any
		member  any
		allowed []string
		without []string
	}{
		{"mcp_servers_status", status, queries.MemberMCPStatus(status), statusKeys,
			[]string{"servers[].command", "servers[].nodes[].error", "servers[].nodes[].wait", "alarms",
				"alarms[].entries[].detail", "approvals", "agents", "agents[].servers[].server",
				"agents[].mcp_tools", "tool_budget"}},
		{"a server's detail", detail, queries.MemberMCPServer(detail), detailKeys,
			[]string{"dependants", "instructions", "resolved.box_seats", "resolved.binding.keys",
				"instances[].failure.said", "instance_nodes[].reason", "usage.tools[].seats", "calls",
				"reaches", "changes", "changes[].before.accepted_by"}},
	} {
		admin, member := keyPaths(asMap(t, tc.admin)), keyPaths(asMap(t, tc.member))
		for _, key := range tc.without {
			if !admin[key] {
				t.Fatalf("%s: the admin's answer filled whole carries no %s, so the fill proves nothing",
					tc.name, key)
			}
		}
		allowed := map[string]bool{}
		for _, key := range tc.allowed {
			allowed[key] = true
		}
		for _, key := range slices.Sorted(maps.Keys(member)) {
			if !allowed[key] {
				t.Errorf("%s: a member's reading carries %s, which its allow-list does not name", tc.name, key)
			}
		}
		for _, key := range slices.Sorted(maps.Keys(allowed)) {
			if !member[key] {
				t.Errorf("%s: the allow-list names %s, which a member's reading of a whole answer does not "+
					"carry", tc.name, key)
			}
		}
		if got := asMap(t, tc.member)["reading"]; got != queries.ReadingMember {
			t.Errorf("%s: a member's reading says it is %v", tc.name, got)
		}
	}
}

// A MEMBER READS HOW OFTEN EACH TOOL WAS CALLED AND NEVER WHO CALLED IT: a
// server's usage reaches a member's reading figure by figure, so the page can
// say which tools are in use, while the seats that made the calls and the calls
// themselves (each naming a seat and a turn) stay the administrator's.
//
// Mutation: copy a tool's seats, or one call, onto a member's reading, or drop
// a tool's failures from it, and this is red.
func TestAMembersReadingCountsEachToolsCallsAndNamesNobodyWhoCalled(t *testing.T) {
	t.Parallel()
	last := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	ran := int64(25)
	admin := queries.MCPServerAnswer{Reading: queries.ReadingAdmin,
		Usage: queries.MCPServerUsage{Since: "2026-10-26", Until: "2026-11-01", Days: queries.MCPUsageDays,
			Elided: 2, Tools: []queries.MCPToolUsage{{Tool: "gh_get_issue", Calls: 5, Failed: 1, Unavailable: 1,
				Refused: 1, TotalMS: 350, LastAt: &last, Seats: []string{"seat-sentinel"}}}},
		Calls: queries.MCPServerCalls{Calls: []queries.MCPCallRow{{At: last, Seat: "seat-sentinel",
			TurnID: "turn-sentinel", Tool: "gh_get_issue", Outcome: mcp.CallAnswered, DurationMS: &ran}}},
	}
	member := queries.MemberMCPServer(admin)
	want := queries.MemberMCPServerUsage{Since: "2026-10-26", Until: "2026-11-01", Days: queries.MCPUsageDays,
		Elided: 2, Tools: []queries.MemberMCPToolUsage{{Tool: "gh_get_issue", Calls: 5, Failed: 1,
			Unavailable: 1, Refused: 1, TotalMS: 350, LastAt: &last}}}
	if !reflect.DeepEqual(member.Usage, want) {
		t.Errorf("a member's usage is %+v, want every figure of the administrator's: %+v", member.Usage, want)
	}
	raw, err := json.Marshal(member)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"seat-sentinel", "turn-sentinel", `"seats":`, `"calls":{`} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("a member's reading carries %s: %s", leak, raw)
		}
	}
}

// opaque are the keys whose values are not walked: the built-in groups, whose
// every row `GET /tools` answers everybody, and a tool's schemas, whose keys
// are the server's.
var opaque = map[string]bool{"builtin": true, "input_schema": true, "output_schema": true}

// keyPaths is every key of an answer as a path: `servers[].nodes[].error_class`.
func keyPaths(answer map[string]any) map[string]bool {
	out := map[string]bool{}
	var walk func(v any, prefix string)
	walk = func(v any, prefix string) {
		switch x := v.(type) {
		case map[string]any:
			for k, child := range x {
				path := k
				if prefix != "" {
					path = prefix + "." + k
				}
				out[path] = true
				if !opaque[k] {
					walk(child, path)
				}
			}
		case []any:
			for _, e := range x {
				walk(e, prefix+"[]")
			}
		}
	}
	walk(answer, "")
	return out
}

// fill sets every exported field reachable from v: a string to a word, a
// number to one, a flag to true, a pointer to a filled value, a list and a map
// to one filled element, and an instant to a fixed one.
func fill(v reflect.Value) {
	switch {
	case v.Type() == reflect.TypeFor[time.Time]():
		v.Set(reflect.ValueOf(time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)))
		return
	case v.Type() == reflect.TypeFor[json.RawMessage]():
		v.SetBytes([]byte(`{}`))
		return
	}
	switch v.Kind() {
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		fill(v.Elem())
	case reflect.Struct:
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				fill(v.Field(i))
			}
		}
	case reflect.Slice:
		list := reflect.MakeSlice(v.Type(), 1, 1)
		fill(list.Index(0))
		v.Set(list)
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		key, elem := reflect.New(v.Type().Key()).Elem(), reflect.New(v.Type().Elem()).Elem()
		fill(key)
		fill(elem)
		m.SetMapIndex(key, elem)
		v.Set(m)
	case reflect.Interface:
		v.Set(reflect.ValueOf("word"))
	case reflect.String:
		v.SetString("word")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(1)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(1)
	}
}
