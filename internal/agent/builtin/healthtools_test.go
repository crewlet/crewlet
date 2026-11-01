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
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/authz/orgchart"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/host"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/eventfan"
	"github.com/crewlet/crewlet/internal/events/types"
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
// the repository's grant does: the repository, a refusal of the asker, a
// refusal of the seat the read is made for, or no such repository — the asker
// judged first, as code.JudgeRead does — and counts the reads it was asked and
// whom each was made for.
type grantHost struct {
	mu      sync.Mutex
	repos   map[string]code.RepoRef // name -> the repository it names
	refused map[string]bool         // names whose repository the asker may not read
	above   map[string]bool         // names whose repository a ceiling seat may not read
	reads   int
	madeFor []string // each read's ceiling seat, in the order asked
}

var _ builtin.CodeReader = (*grantHost)(nil)

func (h *grantHost) Read(_ context.Context, q code.ReadQuery) (code.ReadAnswer, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.reads++
	h.madeFor = append(h.madeFor, q.CeilingSeat)
	if q.Kind != code.ReadRepo {
		return code.ReadAnswer{}, fmt.Errorf("%w: kind %s", code.ErrInvalid, q.Kind)
	}
	if h.refused[q.Repo] {
		return code.ReadAnswer{}, fmt.Errorf("%w: %s", code.ErrNotPermitted, q.Repo)
	}
	if q.CeilingSeat != "" && h.above[q.Repo] {
		return code.ReadAnswer{}, fmt.Errorf("%w: seat %q holds no access to repository %q",
			code.ErrAboveCeiling, q.CeilingSeat, q.Repo)
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

// healthCode is the code tools' deps code_status reads through, wired as every
// surface wires them: the code host, the caller named by the request's
// principal, and who a turn may be acting for with the chart that decides them
// (the requester ceiling), without which no code read — and no code_status —
// is served. The requesters: dev, a colleague seat; the founder, a person of
// the company who holds every app and not admin; and "departed", a person
// whose seat's holder was removed, who acts as nobody now.
func healthCode(host builtin.CodeReader, chart authz.Chart) builtin.CodeDeps {
	return builtin.CodeDeps{Code: host, Actor: builtin.PrincipalActor, Chart: chart,
		Requesters: requesters{
			"dev":     {Kind: iam.KindSeat, Seat: "dev", Stage: iam.StageActive, Grants: iam.Apps},
			"founder": founderPrincipal(iam.Apps...),
			"departed": {Kind: iam.KindPerson, Login: "departed@example.com", Seat: "departed",
				Stage: iam.StageRetired, Grants: iam.Apps},
		}}
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
	catalogue = healthTools(builtin.HealthDeps{Status: asker, Code: healthCode(repos, chart)}, chart)
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
// of them for the granted one, and no other builtin but those the table holds
// human-only — code_report_check, a person's own CI's, and
// code_purge_unreachable, a person's purge — which it refuses every seat,
// chart or none). A person is the class's: admin admitted, everybody
// else refused.
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
		Code: deps.Code}
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

	// THE HUMAN-ONLY BUILTINS are withheld from every seat whatever its grant:
	// the table refuses a seat them before it reads any chart.
	var humanOnly []string
	for _, name := range reg.Names() {
		if human, _ := authz.HumanOnlyOf(authz.Action(name)); human {
			humanOnly = append(humanOnly, name)
		}
	}
	if !slices.Contains(humanOnly, builtin.CodeReportCheckTool) {
		t.Fatalf("the registry holds human-only %v, want code_report_check among them", humanOnly)
	}
	slices.Sort(humanOnly)
	if got := builtin.Withheld(asSeat("sre"), deps.Authorize, snapshot); !slices.Equal(got, humanOnly) {
		t.Errorf("the granted seat is withheld %v, want the human-only %v alone", got, humanOnly)
	}
	want := slices.Sorted(slices.Values(append(slices.Clone(health), humanOnly...)))
	if got := builtin.Withheld(asSeat("dev"), deps.Authorize, snapshot); !slices.Equal(got, want) {
		t.Errorf("the ungranted seat is withheld %v, want exactly the health tools and the human-only %v",
			got, want)
	}
	// A CHART THAT CANNOT ANSWER WITHHOLDS NOTHING IT WOULD HAVE TO READ: the
	// call decides it again, and hiding a tool from a seat that holds it is
	// the wrong silence — a human-only builtin aside, which no chart decides.
	if got := builtin.Withheld(asSeat("sre"), builtin.Decide(authz.NoChart{}), snapshot); !slices.Equal(got,
		humanOnly) {
		t.Errorf("a node with no chart withholds %v, want the human-only %v alone", got, humanOnly)
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
	tool := healthTools(builtin.HealthDeps{Status: asker, Code: healthCode(repos, chart)},
		chart)[builtin.CodeStatusTool]

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
// descriptions and schemas, each a proven read; deps that reach no lease
// table and no code host serve neither; and code_status is served on the code
// reads' own terms — never over a code host wired without the requester
// ceiling's halves, whose reads would be made for nobody whoever woke a turn.
//
// Mutation: OperatorTools omitting healthCandidates, a health tool annotated
// as a write, or code_status served over the code host alone → red.
func TestTheHealthToolsAreOneList(t *testing.T) {
	t.Parallel()
	deps := builtin.HealthDeps{Leases: coordmem.New(), Status: &statusAsker{},
		Code: codeDeps(&grantHost{})}
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
	served := func(code builtin.CodeDeps) bool {
		for _, tool := range builtin.HealthTools(builtin.HealthDeps{Status: &statusAsker{}, Code: code}) {
			if tool.Name() == builtin.CodeStatusTool {
				return true
			}
		}
		return false
	}
	wired := codeDeps(&grantHost{})
	if !served(wired) {
		t.Fatalf("code_status is not served over the code reads' whole wiring, so this case proves nothing")
	}
	for name, half := range map[string]builtin.CodeDeps{
		"the code host alone":              {Code: wired.Code},
		"the code host without Chart":      {Code: wired.Code, Requesters: wired.Requesters},
		"the code host without Requesters": {Code: wired.Code, Chart: wired.Chart},
	} {
		if served(half) {
			t.Errorf("code_status is served over %s, which serves no code read", name)
		}
	}
}

// A CODE STATUS ANSWER IN A TURN SOMEBODY WOKE IS HELD TO THEIR GRANT TOO.
//
// Its repository reads are code reads, and the requester ceiling holds every
// code read (ADR-0058): in a turn a colleague seat woke, each grant read names
// that seat, so the hot list also leaves out a repository the seat may read
// and the colleague may not — counted withheld beside the one the seat itself
// may not read — and that repository asked by name is refused as the code
// tools refuse a read above the ceiling, forbidden with the ceiling's reason
// over code.ErrAboveCeiling, and never asked of the git nodes. A turn nobody
// woke, and one a person woke, read for nobody but the seat — a person bounds
// nothing, since every person reads every repository — and the person who
// woke it need not hold admin: whoever woke the turn is decided on the code
// host's read verb, not on the health row, which the ceiling does not hold.
// A turn a stranger woke, or a person who may not act in the company now, is
// answered nothing, and nothing is read or asked.
//
// Mutation: read the hot list for the seat alone (no ceiling seat on the
// reads), keep a repository answered ErrAboveCeiling, or decide whoever woke
// the turn on code_status's own row → red.
func TestACodeStatusInATurnSomebodyWokeIsHeldToTheirGrantToo(t *testing.T) {
	t.Parallel()
	const vaultID, infraID = "0192f6c1-0000-7000-8000-000000000001", "0192f6c1-0000-7000-8000-000000000004"
	use := func(fetches int64) cache.Use { return cache.Use{Fetches: fetches} }
	report := builtin.CodeStatusReport{Statuses: []host.CodeStatus{{Node: "node-a", Hot: []host.RepoStatus{
		{RepoHeat: cache.RepoHeat{ID: siteID, Name: "site", Hour: use(9), Day: use(9)}},
		{RepoHeat: cache.RepoHeat{ID: infraID, Name: "infra", Hour: use(8), Day: use(8)}},
		{RepoHeat: cache.RepoHeat{ID: vaultID, Name: "vault", Hour: use(7), Day: use(7)}},
	}}}}
	o, chart := healthChart(t)
	unknown := []types.CanonicalIdentity{{ExternalID: "U-out", Platform: "slack"}}
	speaker := func(handle string) []types.CanonicalIdentity {
		return []types.CanonicalIdentity{{Handle: handle}}
	}
	type setup struct {
		asker *statusAsker
		repos *grantHost
		tool  tools.SeatCallable
		turn  *turnctx.Turn
	}
	build := func(t *testing.T, requester string, senders []types.CanonicalIdentity) setup {
		t.Helper()
		s := setup{asker: &statusAsker{report: report}, repos: &grantHost{
			repos: map[string]code.RepoRef{"site": {ID: siteID, Name: "site"},
				"infra": {ID: infraID, Name: "infra"}, "vault": {ID: vaultID, Name: "vault"}},
			refused: map[string]bool{"vault": true},
			above:   map[string]bool{"infra": true},
		}}
		tool := healthTools(builtin.HealthDeps{Status: s.asker, Code: healthCode(s.repos, chart)},
			chart)[builtin.CodeStatusTool]
		seated, ok := tool.(tools.SeatCallable)
		if !ok {
			t.Fatalf("code_status is not seat-callable")
		}
		s.tool = seated
		s.turn = &turnctx.Turn{RunID: "run-1", WorkKey: "wk-1", Seat: o.AgentSeatByHandle("sre"), Org: o,
			Lookups: turnctx.NewLookups(), Requester: requester, Senders: senders}
		return s
	}
	call := func(t *testing.T, s setup, args map[string]any) tools.Result {
		t.Helper()
		got, err := s.tool.CallForTurn(asSeat("sre"), s.turn, args)
		if err != nil {
			t.Fatalf("code_status: %v", err)
		}
		return got
	}
	hotOf := func(answer map[string]any) []string {
		var out []string
		for _, h := range answer["hot"].([]any) {
			out = append(out, h.(map[string]any)["id"].(string))
		}
		return out
	}

	for _, c := range []struct {
		name      string
		requester string
		senders   []types.CanonicalIdentity
		madeFor   string
		hot       []string
		withheld  float64
	}{
		{"a turn a colleague seat woke", "dev", speaker("dev"), "dev", []string{siteID}, 2},
		{"a turn nobody woke", "", nil, "", []string{siteID, infraID}, 1},
		{"a turn a person woke who holds no admin", "founder", speaker("founder"), "",
			[]string{siteID, infraID}, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := build(t, c.requester, c.senders)
			answer := healthAnswer(t, call(t, s, nil))
			if got := hotOf(answer); !slices.Equal(got, c.hot) {
				t.Errorf("the hot list = %v, want %v", got, c.hot)
			}
			if got := answer["withheld"]; got != c.withheld {
				t.Errorf("the answer withheld %v, want %v", got, c.withheld)
			}
			if len(s.repos.madeFor) == 0 {
				t.Fatalf("the hot list was judged without a read, so this case proves nothing")
			}
			for _, seat := range s.repos.madeFor {
				if seat != c.madeFor {
					t.Errorf("a grant read was made for %q, want %q", seat, c.madeFor)
				}
			}
		})
	}

	t.Run("a repository the colleague may not read, asked by name", func(t *testing.T) {
		t.Parallel()
		s := build(t, "dev", speaker("dev"))
		got := call(t, s, map[string]any{"repo": "infra"})
		if tools.RefusalOf(got) != tools.RefusalForbidden {
			t.Fatalf("answered %s %q, want refused forbidden", tools.RefusalOf(got), got.Output)
		}
		var refused *builtin.DecisionRefused
		if !errors.As(got.Cause, &refused) || refused.Decision.Reason != authz.ReasonCeiling ||
			!errors.Is(got.Cause, code.ErrAboveCeiling) || errors.Is(got.Cause, code.ErrNotPermitted) {
			t.Errorf("the refusal's cause is %v, want the ceiling's reason over ErrAboveCeiling", got.Cause)
		}
		if len(s.asker.asked) != 0 {
			t.Errorf("the git nodes were asked about %v for a repository refused the turn's colleague",
				s.asker.asked)
		}
	})

	for _, c := range []struct {
		name      string
		requester string
		senders   []types.CanonicalIdentity
		says      string
	}{
		{"a turn a stranger woke", "", unknown, "woken by somebody the company does not know"},
		{"a turn a departed person woke", "departed", speaker("departed"), "may not act in the company now"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := build(t, c.requester, c.senders)
			for _, args := range []map[string]any{nil, {"repo": "site"}} {
				got := call(t, s, args)
				if tools.RefusalOf(got) != tools.RefusalForbidden || !strings.Contains(got.Output, c.says) {
					t.Errorf("code_status %v answered %s %q, want refused forbidden saying %q", args,
						tools.RefusalOf(got), got.Output, c.says)
				}
				var refused *builtin.DecisionRefused
				if !errors.As(got.Cause, &refused) || refused.Decision.Reason != authz.ReasonCeiling {
					t.Errorf("code_status %v's refusal %v carries no ceiling reason", args, got.Cause)
				}
			}
			if s.repos.reads != 0 || len(s.asker.asked) != 0 {
				t.Errorf("the refused calls read %d repositories and asked the git nodes %v, want nothing",
					s.repos.reads, s.asker.asked)
			}
		})
	}
}
