package builtin_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/authz/orgchart"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/host"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/eventfan"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
)

// ---- fixtures ------------------------------------------------------------ //

// statusAsker is the git nodes' status scatter as the health tools see it: one
// report, and every repository it was asked about.
type statusAsker struct {
	mu     sync.Mutex
	report builtin.CodeStatusReport
	err    error
	asked  []string
}

var _ builtin.StatusAsker = (*statusAsker)(nil)

func (s *statusAsker) CodeStatus(_ context.Context, repoID string) (builtin.CodeStatusReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, repoID)
	return s.report, s.err
}

// grantHost is a code host that answers a read of one repository by name as
// the repository's grant does: the repository, a refusal of the asker, or no
// such repository — and counts the reads it was asked.
type grantHost struct {
	mu      sync.Mutex
	repos   map[string]code.RepoRef // name -> the repository it names
	refused map[string]bool         // names whose repository the asker may not read
	reads   int
}

var _ builtin.CodeReader = (*grantHost)(nil)

func (h *grantHost) Read(_ context.Context, q code.ReadQuery) (code.ReadAnswer, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.reads++
	if q.Kind != code.ReadRepo {
		return code.ReadAnswer{}, fmt.Errorf("%w: kind %s", code.ErrInvalid, q.Kind)
	}
	if h.refused[q.Repo] {
		return code.ReadAnswer{}, fmt.Errorf("%w: %s", code.ErrNotPermitted, q.Repo)
	}
	ref, ok := h.repos[q.Repo]
	if !ok {
		return code.ReadAnswer{}, fmt.Errorf("%w: %s", code.ErrRepoNotFound, q.Repo)
	}
	return code.ReadAnswer{Repo: &code.RepoDetail{RepoSummary: code.RepoSummary{RepoRef: ref}}}, nil
}

func (*grantHost) ResolveRef(context.Context, code.RefQuery) (code.RefAnswer, error) {
	return code.RefAnswer{}, errors.New("grantHost resolves no ref")
}

func (*grantHost) GitRead(context.Context, code.GitQuery) (code.GitAnswer, error) {
	return code.GitAnswer{}, errors.New("grantHost reads no git")
}

func (*grantHost) FloorOf(string) statelog.Position { return statelog.Position{} }

// healthCompany is a company with two agent seats, one whose role carries the
// health grant and one whose role does not.
const healthCompany = `
name: Nimbus
providers:
  llm:
    zulu:
      model: claude-sonnet-5
roles:
  - name: SRE
    handle: sre
    llm: zulu
    health_tools: true
  - name: Dev
    handle: dev
    llm: zulu
`

// healthChart is the chart healthCompany draws.
func healthChart(t *testing.T) (*org.Organization, authz.Chart) {
	t.Helper()
	cfg, err := config.ParseCompany([]byte(healthCompany))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	o, err := cfg.Organization()
	if err != nil {
		t.Fatalf("organization: %v", err)
	}
	return o, orgchart.Of(o)
}

// asSeat is a context carrying the seat handle as a turn's principal: every
// app, never admin (internal/agent/turnctx).
func asSeat(handle string) context.Context {
	return iam.WithPrincipal(context.Background(), iam.Principal{
		Kind: iam.KindSeat, Seat: handle, Stage: iam.StageActive, Grants: iam.Apps,
	})
}

// node claims node's presence in backend, its heartbeat carrying status where
// it is not nil and nothing where it is.
func node(t *testing.T, backend *coordmem.Backend, id string, meta map[string]any,
	status *coord.NodeStatus) {

	t.Helper()
	if meta == nil {
		meta = map[string]any{}
	}
	if status != nil {
		meta[coord.StatusKey] = status.Meta()
	}
	if _, _, err := backend.TryAcquire(t.Context(), coord.NodeResource(id), coord.AcquireOptions{
		Owner: id + ":1", TTL: time.Minute, Meta: meta,
	}); err != nil {
		t.Fatal(err)
	}
}

// callTool calls a registered or catalogued tool as ctx's principal, in turn
// where it is a seat tool.
func callTool(t *testing.T, ctx context.Context, tool tools.Callable, args map[string]any) tools.Result {
	t.Helper()
	var (
		got tools.Result
		err error
	)
	if seated, ok := tool.(tools.SeatCallable); ok {
		got, err = seated.CallForTurn(ctx, nil, args)
	} else {
		got, err = tool.Call(ctx, args)
	}
	if err != nil {
		t.Fatalf("%s: %v", tool.Name(), err)
	}
	return got
}

// healthTools are the health tools over deps, decided by chart, by name.
func healthTools(deps builtin.HealthDeps, chart authz.Chart) map[string]tools.Callable {
	out := map[string]tools.Callable{}
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
		Health: deps, Authorize: builtin.Decide(chart),
	}) {
		out[tool.Name()] = tool
	}
	return out
}

// healthAnswer is a tool's answer as a JSON object, failing on a refusal.
func healthAnswer(t *testing.T, got tools.Result) map[string]any {
	t.Helper()
	if got.Failed {
		t.Fatalf("refused: %s", got.Output)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(got.Output), &out); err != nil {
		t.Fatalf("no JSON object: %v\n%s", err, got.Output)
	}
	return out
}

// keysOf is an object's keys, sorted.
func keysOf(t *testing.T, v any) []string {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%v is not an object", v)
	}
	return slices.Sorted(maps.Keys(m))
}

// ---- the cases ----------------------------------------------------------- //

// THE HEALTH TOOLS REPORT ONLY THE TABLE'S VERDICTS.
//
// fleet_health hands every node's alarm marks back exactly as its heartbeat
// carried them — each kind and since, in the node's order — reads a node that
// looked and found nothing as an empty list, and a node with no table it has
// evaluated as saying nothing, never as healthy; and beside the marks it
// carries the heartbeat's facts and no judgement of its own: a node running a
// thousand turns, draining and stuck on its config, with nothing firing, is
// reported with nothing firing. code_status hands each git node's own account
// back as the node gave it — a cache past its budget is two numbers, not a
// flag — with the coverage its asker heard. Every key either answer carries is
// one the tool declares, so a verdict added beside the facts fails here.
//
// Mutation: mark a draining node as firing, read an unevaluated table as an
// empty one, or drop a node's since, and fleet_health goes red; add an
// over-budget flag to a node's cache, and code_status does.
func TestTheHealthToolsReportOnlyTheTablesVerdicts(t *testing.T) {
	t.Parallel()
	backend := coordmem.New()
	since := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	node(t, backend, "node-a", map[string]any{"roles": []string{"data", "git"}}, &coord.NodeStatus{
		InFlight: 3, AlarmsEvaluated: true, Alarms: []coord.AlarmMark{
			{Kind: "backup_age", Since: since}, {Kind: "apply_lag", Since: since.Add(2 * time.Minute)},
		}})
	node(t, backend, "node-b", nil, &coord.NodeStatus{
		InFlight: 1000, Draining: true, Posture: "stuck", ProjectionsTotal: 5, AlarmsEvaluated: true})
	node(t, backend, "node-c", map[string]any{"roles": []string{"seats"}}, &coord.NodeStatus{InFlight: 2})
	node(t, backend, "node-d", nil, nil)
	_, chart := healthChart(t)
	catalogue := healthTools(builtin.HealthDeps{Leases: backend, Self: "node-a"}, chart)

	answer := healthAnswer(t, callTool(t, asAdmin(), catalogue[builtin.FleetHealthTool], nil))
	if got := keysOf(t, answer); !slices.Equal(got, []string{"nodes", "this_node"}) {
		t.Errorf("fleet_health answered the keys %v, want only nodes and this_node", got)
	}
	byID := map[string]map[string]any{}
	for _, n := range answer["nodes"].([]any) {
		row := n.(map[string]any)
		byID[row["id"].(string)] = row
		for _, key := range keysOf(t, row) {
			if !slices.Contains([]string{"broker", "expires_in", "id", "labels", "roles", "seats",
				"status"}, key) {
				t.Errorf("node %s carries %q, which the tool does not declare", row["id"], key)
			}
		}
		if status, ok := row["status"]; ok {
			for _, key := range keysOf(t, status) {
				if !slices.Contains([]string{"alarms", "alarms_evaluated", "draining", "in_flight",
					"posture", "projections_ready", "projections_total", "started_at"}, key) {
					t.Errorf("node %s's status carries %q, which the tool does not declare",
						row["id"], key)
				}
			}
		}
	}
	status := func(id string) map[string]any {
		s, _ := byID[id]["status"].(map[string]any)
		return s
	}
	wantMarks := []any{
		map[string]any{"kind": "backup_age", "since": "2026-03-04T05:06:07Z"},
		map[string]any{"kind": "apply_lag", "since": "2026-03-04T05:08:07Z"},
	}
	if got := status("node-a")["alarms"]; fmt.Sprint(got) != fmt.Sprint(wantMarks) {
		t.Errorf("node-a's alarms = %v, want the marks its heartbeat carried, in its order: %v",
			got, wantMarks)
	}
	if got, present := status("node-b")["alarms"]; !present || fmt.Sprint(got) != "[]" {
		t.Errorf("a node that looked and found nothing reads %v (present %v), want an empty list",
			got, present)
	}
	if got := status("node-b")["alarms_evaluated"]; got != true {
		t.Errorf("node-b's table reads evaluated %v, want true", got)
	}
	if got, present := status("node-c")["alarms"]; present || status("node-c")["alarms_evaluated"] != false {
		t.Errorf("a node with no table it evaluated reads alarms %v (present %v), evaluated %v — "+
			"one that cannot say must not read as healthy", got, present, status("node-c")["alarms_evaluated"])
	}
	if _, present := byID["node-d"]["status"]; present {
		t.Errorf("a node whose heartbeat carried no status reads %v, want none", byID["node-d"]["status"])
	}
	if got := answer["this_node"]; got != "node-a" {
		t.Errorf("this_node = %v, want node-a", got)
	}

	// code_status: each git node's own account, as the node gave it.
	over := cache.Status{Since: since, Bytes: 9 << 30, Budget: 8 << 30, Warm: 3,
		Hour: cache.Churn{Evictions: 7}}
	heat := cache.RepoHeat{ID: siteID, Name: "site", Hour: cache.Use{Fetches: 40, Clones: 2, BytesOut: 1 << 20},
		Day: cache.Use{Fetches: 400}}
	coverage := eventfan.Coverage{Nodes: []eventfan.NodeCoverage{{ID: "node-a", Answered: true},
		{ID: "node-e", Error: "did not answer inside the budget"}}}
	asker := &statusAsker{report: builtin.CodeStatusReport{
		Statuses: []host.CodeStatus{{Node: "node-a", At: since, Cache: over,
			Hot: []host.RepoStatus{{RepoHeat: heat}}}},
		Coverage: coverage,
	}}
	repos := &grantHost{repos: map[string]code.RepoRef{"site": {ID: siteID, Name: "site"}}}
	catalogue = healthTools(builtin.HealthDeps{Status: asker,
		Code: builtin.CodeDeps{Code: repos, Actor: builtin.PrincipalActor}}, chart)
	status2 := healthAnswer(t, callTool(t, asAdmin(), catalogue[builtin.CodeStatusTool], nil))
	if got := keysOf(t, status2); !slices.Equal(got, []string{"coverage", "hot", "nodes"}) {
		t.Errorf("code_status answered the keys %v, want only coverage, hot and nodes", got)
	}
	var got struct {
		Nodes []struct {
			Node  string       `json:"node"`
			Cache cache.Status `json:"cache"`
		} `json:"nodes"`
		Hot []struct {
			Node string `json:"node"`
			cache.RepoHeat
		} `json:"hot"`
		Coverage eventfan.Coverage `json:"coverage"`
	}
	raw, _ := json.Marshal(status2)
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Nodes) != 1 || got.Nodes[0].Cache != over {
		t.Errorf("the node's cache reads %+v, want exactly what it said: %+v", got.Nodes, over)
	}
	for _, key := range keysOf(t, status2["nodes"].([]any)[0].(map[string]any)["cache"]) {
		if !slices.Contains([]string{"budget", "bytes", "day", "hour", "since", "warm"}, key) {
			t.Errorf("a node's cache carries %q, which the node did not say", key)
		}
	}
	if len(got.Hot) != 1 || got.Hot[0].Node != "node-a" || got.Hot[0].Hour != heat.Hour ||
		got.Hot[0].Day != heat.Day {
		t.Errorf("the hot list reads %+v, want node-a's own count of site", got.Hot)
	}
	if fmt.Sprint(got.Coverage) != fmt.Sprint(coverage) {
		t.Errorf("coverage = %+v, want what the asker heard: %+v", got.Coverage, coverage)
	}
	if repos.reads != 0 {
		t.Errorf("a person's answer made %d grant reads; a person reads every repository", repos.reads)
	}
}

// A SEAT GRANT ADMITS ONLY ITS GRANTED SEATS, and a surface offers the health
// tools exactly where the table admits.
//
// One decision, three questions of it: the gate on a call (the seat whose role
// carries `health_tools` is answered, the seat whose role does not is refused),
// Admits (which a surface asks before anything is named), and Withheld (what a
// seat's surfaces leave out: both health tools for the ungranted seat, nothing
// for the granted one, and never any other builtin). A person is the class's:
// admin admitted, everybody else refused.
//
// Mutation: decide a seat on a granted row by the class instead of its role's
// grant (ignore the grant), and the granted seat is refused and offered
// nothing; admit every seat on such a row, and the ungranted one is offered
// and answered.
func TestASeatGrantAdmitsOnlyItsGrantedSeats(t *testing.T) {
	t.Parallel()
	_, chart := healthChart(t)
	backend := coordmem.New()
	node(t, backend, "node-a", nil, &coord.NodeStatus{AlarmsEvaluated: true})
	deps := fullDeps(t)
	deps.Authorize = builtin.Decide(chart)
	deps.Health = builtin.HealthDeps{Leases: backend, Self: "node-a", Status: &statusAsker{},
		Code: builtin.CodeDeps{Code: deps.Code.Code}}
	reg := tools.NewRegistry()
	if _, err := builtin.Register(reg, deps); err != nil {
		t.Fatalf("register: %v", err)
	}
	snapshot := reg.Snapshot()
	health := builtin.HealthToolNames()
	for _, name := range health {
		if _, ok := snapshot.Lookup(name); !ok {
			t.Fatalf("%s is not registered, so this case asks nothing about it", name)
		}
	}

	for _, c := range []struct {
		who      string
		ctx      context.Context
		admitted bool
	}{
		{"the seat whose role carries the grant", asSeat("sre"), true},
		{"a seat whose role does not", asSeat("dev"), false},
		{"a seat the chart does not hold", asSeat("ghost"), false},
		{"a person holding admin", asAdmin(), true},
		{"a person holding every app and not admin", personHolding(iam.Apps...), false},
	} {
		for _, name := range health {
			err := builtin.Admits(c.ctx, deps.Authorize, name)
			if (err == nil) != c.admitted {
				t.Errorf("%s: Admits(%s) = %v, want admitted %v", c.who, name, err, c.admitted)
			}
			if err != nil && !errors.Is(err, builtin.ErrRefused) {
				t.Errorf("%s: Admits(%s) = %v, want a refusal rather than an undecidable answer",
					c.who, name, err)
			}
		}
		entry, _ := snapshot.Lookup(builtin.FleetHealthTool)
		if got := callTool(t, c.ctx, entry.Tool, nil); got.Failed == c.admitted {
			t.Errorf("%s: fleet_health answered failed=%v (%s), want admitted %v", c.who,
				got.Failed, got.Output, c.admitted)
		}
	}

	if got := builtin.Withheld(asSeat("sre"), deps.Authorize, snapshot); len(got) != 0 {
		t.Errorf("the granted seat is withheld %v, want nothing", got)
	}
	want := slices.Sorted(slices.Values(health))
	if got := builtin.Withheld(asSeat("dev"), deps.Authorize, snapshot); !slices.Equal(got, want) {
		t.Errorf("the ungranted seat is withheld %v, want exactly the health tools %v", got, want)
	}
	// A CHART THAT CANNOT ANSWER WITHHOLDS NOTHING: the call decides it again,
	// and hiding a tool from a seat that holds it is the wrong silence.
	if got := builtin.Withheld(asSeat("sre"), builtin.Decide(authz.NoChart{}), snapshot); len(got) != 0 {
		t.Errorf("a node with no chart withholds %v, want nothing", got)
	}
}

// A CODE STATUS ANSWER IS HELD TO THE SEAT'S REPOSITORY GRANT.
//
// The health grant opens the deployment's view and never a repository the
// seat's own grant does not read (ADR-0058): of the hottest repositories the
// git nodes name, a seat's answer keeps those its grant reads, leaves out one
// it may not read, one whose name now names nothing and one whose name now
// names another repository, and says how many it left out; one repository
// asked by name is refused the seat exactly as the code tools refuse it. A
// person's answer leaves nothing out and asks no grant at all.
//
// Mutation: skip the judgement for a seat, or keep a repository whose name
// now names another id, and the seat's answer names what it may not read.
func TestACodeStatusAnswerIsHeldToTheSeatsRepositoryGrant(t *testing.T) {
	t.Parallel()
	const vaultID, oldID, renamedID = "0192f6c1-0000-7000-8000-000000000001",
		"0192f6c1-0000-7000-8000-000000000002", "0192f6c1-0000-7000-8000-000000000003"
	use := func(fetches int64) cache.Use { return cache.Use{Fetches: fetches} }
	asker := &statusAsker{report: builtin.CodeStatusReport{Statuses: []host.CodeStatus{
		{Node: "node-a", Hot: []host.RepoStatus{
			{RepoHeat: cache.RepoHeat{ID: siteID, Name: "site", Hour: use(9), Day: use(9)}},
			{RepoHeat: cache.RepoHeat{ID: vaultID, Name: "vault", Hour: use(8), Day: use(8)}},
		}},
		{Node: "node-b", Hot: []host.RepoStatus{
			{RepoHeat: cache.RepoHeat{ID: oldID, Name: "gone", Hour: use(7), Day: use(7)}},
			{RepoHeat: cache.RepoHeat{ID: renamedID, Name: "site", Hour: use(6), Day: use(6)}},
			{RepoHeat: cache.RepoHeat{ID: siteID, Name: "site", Hour: use(5), Day: use(5)}},
		}},
	}}}
	repos := &grantHost{
		repos:   map[string]code.RepoRef{"site": {ID: siteID, Name: "site"}, "vault": {ID: vaultID, Name: "vault"}},
		refused: map[string]bool{"vault": true},
	}
	_, chart := healthChart(t)
	tool := healthTools(builtin.HealthDeps{Status: asker,
		Code: builtin.CodeDeps{Code: repos, Actor: builtin.PrincipalActor}}, chart)[builtin.CodeStatusTool]

	hotOf := func(answer map[string]any) []string {
		var out []string
		for _, h := range answer["hot"].([]any) {
			row := h.(map[string]any)
			out = append(out, row["node"].(string)+"/"+row["id"].(string))
		}
		return out
	}
	seat := healthAnswer(t, callTool(t, asSeat("sre"), tool, nil))
	if got, want := hotOf(seat), []string{"node-a/" + siteID, "node-b/" + siteID}; !slices.Equal(got, want) {
		t.Errorf("the seat's hot list = %v, want only what its grant reads: %v", got, want)
	}
	if got := seat["withheld"]; got != float64(3) {
		t.Errorf("the seat's answer withheld %v, want 3 (refused, gone, renamed)", got)
	}
	person := healthAnswer(t, callTool(t, asAdmin(), tool, nil))
	if got := hotOf(person); len(got) != 5 {
		t.Errorf("a person's hot list = %v, want all five entries", got)
	}
	if _, present := person["withheld"]; present {
		t.Errorf("a person's answer withheld %v, want nothing left out", person["withheld"])
	}

	refused := callTool(t, asSeat("sre"), tool, map[string]any{"repo": "vault"})
	if !refused.Failed || !strings.Contains(refused.Output, "vault") {
		t.Errorf("a seat asking about a repository it may not read was answered %q, want refused",
			refused.Output)
	}
	if slices.Contains(asker.asked, vaultID) {
		t.Errorf("the git nodes were asked about a repository the seat was refused: %v", asker.asked)
	}
	one := healthAnswer(t, callTool(t, asSeat("sre"), tool, map[string]any{"repo": "site"}))
	if one["id"] != siteID || one["repo"] != "site" {
		t.Errorf("the one-repository answer names %v (%v), want site (%s)", one["repo"], one["id"], siteID)
	}
}

// THE HEALTH TOOLS ARE ONE LIST: a seat's registry and the operator catalogue,
// built over the same deps, hold the same two tools with byte-identical
// descriptions and schemas, each a proven read; and deps that reach no lease
// table and no code host serve neither.
//
// Mutation: OperatorTools omitting healthCandidates, or a health tool
// annotated as a write → red.
func TestTheHealthToolsAreOneList(t *testing.T) {
	t.Parallel()
	deps := builtin.HealthDeps{Leases: coordmem.New(), Status: &statusAsker{},
		Code: builtin.CodeDeps{Code: &grantHost{}}}
	reg := tools.NewRegistry()
	if _, err := builtin.Register(reg, gated(builtin.Deps{Health: deps})); err != nil {
		t.Fatalf("register: %v", err)
	}
	catalogue := map[string]tools.Callable{}
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{Health: deps,
		Authorize: builtin.Decide(authz.NoChart{})}) {
		catalogue[tool.Name()] = tool
	}
	for _, name := range builtin.HealthToolNames() {
		entry, ok := reg.Lookup(name)
		if !ok {
			t.Fatalf("a seat's registry has no %s", name)
		}
		op, ok := catalogue[name]
		if !ok {
			t.Fatalf("the operator catalogue has no %s", name)
		}
		seatSchema, _ := json.Marshal(entry.Tool.Parameters())
		opSchema, _ := json.Marshal(op.Parameters())
		if entry.Tool.Description() != op.Description() || string(seatSchema) != string(opSchema) {
			t.Errorf("%s is described or shaped differently on the two surfaces", name)
		}
		if a := builtin.AnnotationsFor(name); a.ReadOnly != mcp.Yes || a.Idempotent != mcp.Yes {
			t.Errorf("%s is annotated %+v, want a proven, idempotent read", name, a)
		}
	}
	if got := builtin.HealthTools(builtin.HealthDeps{}); len(got) != 0 {
		t.Errorf("deps reaching nothing serve %d health tools, want none", len(got))
	}
}
