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
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/alarmwake"
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
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
)

// ---- fixtures ------------------------------------------------------------ //

// statusAsker is the Gitlet nodes' roster and status scatter as the health tools
// see them, over one fleet: report is every Gitlet node's status and its part in
// the coverage, the roster every node either names — listed in reverse and
// twice, so a reader that does not sort it reads it wrong — and each question
// is answered as the engine answers it, by the nodes it asks alone, each with
// its hot list only where asked, complete where every one answered. It keeps
// every repository it was asked about, and every question.
type statusAsker struct {
	mu        sync.Mutex
	report    builtin.CodeStatusReport
	err       error // CodeStatus's
	rosterErr error // GitletNodes'
	asked     []string
	asks      []builtin.CodeStatusAsk
}

var _ builtin.StatusAsker = (*statusAsker)(nil)

func (s *statusAsker) GitletNodes(context.Context) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rosterErr != nil {
		return nil, s.rosterErr
	}
	var ids []string
	for _, n := range s.report.Coverage.Nodes {
		ids = append(ids, n.ID)
	}
	for _, st := range s.report.Statuses {
		ids = append(ids, st.Node)
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	slices.Reverse(ids)
	return append(ids, ids...), nil
}

func (s *statusAsker) CodeStatus(_ context.Context, ask builtin.CodeStatusAsk) (builtin.CodeStatusReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, ask.Repo)
	s.asks = append(s.asks, builtin.CodeStatusAsk{Nodes: slices.Clone(ask.Nodes), Every: ask.Every,
		Repo: ask.Repo, Hot: ask.Hot})
	if s.err != nil {
		return builtin.CodeStatusReport{}, s.err
	}
	part := map[string]eventfan.NodeCoverage{}
	for _, n := range s.report.Coverage.Nodes {
		part[n.ID] = n
	}
	statusOf := map[string]host.CodeStatus{}
	for _, st := range s.report.Statuses {
		statusOf[st.Node] = st
	}
	out := builtin.CodeStatusReport{Coverage: eventfan.Coverage{Nodes: []eventfan.NodeCoverage{},
		Complete: true}}
	for _, id := range ask.Nodes {
		st, answered := statusOf[id]
		n, named := part[id]
		switch {
		case !named && answered:
			n = eventfan.NodeCoverage{ID: id, Answered: true}
		case !named:
			n = eventfan.NodeCoverage{ID: id, Error: "did not answer"}
		}
		out.Coverage.Nodes = append(out.Coverage.Nodes, n)
		out.Coverage.Complete = out.Coverage.Complete && n.Answered
		if answered {
			if !ask.Hot {
				st.Hot = nil
			}
			out.Statuses = append(out.Statuses, st)
		}
	}
	return out, nil
}

// questions is every question asked so far, and forgets them.
func (s *statusAsker) questions() []builtin.CodeStatusAsk {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.asks
	s.asks = nil
	return out
}

// grantHost is Gitlet answering a read of one repository by name as
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
  sandbox: {fake: true}
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

// healthRequesters is who a health tool's turn may be acting for, composed as
// the engine composes them: dev, a colleague seat; the founder, a person of
// the company who holds every app and not admin; and "departed", a person
// whose seat's holder was removed, who acts as nobody now. Any other handle is
// a requester this node cannot compose.
func healthRequesters() requesters {
	return requesters{
		"dev":     {Kind: iam.KindSeat, Seat: "dev", Stage: iam.StageActive, Grants: iam.Apps},
		"founder": founderPrincipal(iam.Apps...),
		"departed": {Kind: iam.KindPerson, Login: "departed@example.com", Seat: "departed",
			Stage: iam.StageRetired, Grants: iam.Apps},
	}
}

// healthFleet is fleet_health's deps over leases, wired as every surface wires
// them: who a turn may be acting for with the chart that decides them (the
// requester ceiling), without which no fleet_health is served.
func healthFleet(leases builtin.LeaseLister, chart authz.Chart) builtin.HealthDeps {
	return builtin.HealthDeps{Leases: leases, Self: "node-a", Requesters: healthRequesters(), Chart: chart}
}

// healthCode is the code tools' deps code_status reads through, wired as every
// surface wires them: Gitlet, the caller named by the request's
// principal, and who a turn may be acting for with the chart that decides them
// (the requester ceiling), without which no code read — and no code_status —
// is served.
func healthCode(host builtin.CodeReader, chart authz.Chart) builtin.CodeDeps {
	return builtin.CodeDeps{Code: host, Actor: builtin.PrincipalActor, Chart: chart,
		Requesters: healthRequesters()}
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
func callTool(ctx context.Context, t *testing.T, tool tools.Callable, args map[string]any) tools.Result {
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

// seatHealthTools are the health tools over deps as a seat's registry offers
// them, decided by chart, by name. A turn somebody woke reaches a seat's
// registry alone, whose gate binds the turn's requester ceiling from the
// composer the registry is handed (builtin.Deps.Requesters) — never the
// operator catalogue, which no seat calls through — so a case about whoever
// woke a turn takes its tool from here.
func seatHealthTools(t *testing.T, deps builtin.HealthDeps, chart authz.Chart) map[string]tools.Callable {
	t.Helper()
	composer := deps.Requesters
	if composer == nil {
		composer = deps.Code.Requesters
	}
	reg := tools.NewRegistry()
	if _, err := builtin.Register(reg, builtin.Deps{Health: deps, Requesters: composer,
		Authorize: builtin.Decide(chart)}); err != nil {
		t.Fatalf("register: %v", err)
	}
	out := map[string]tools.Callable{}
	for _, name := range builtin.HealthToolNames() {
		if entry, ok := reg.Lookup(name); ok {
			out[name] = entry.Tool
		}
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
// reported with nothing firing. code_status hands each Gitlet node's own account
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
	node(t, backend, "node-a", map[string]any{"roles": []string{"data", "gitlet"}}, &coord.NodeStatus{
		InFlight: 3, AlarmsEvaluated: true, Alarms: []coord.AlarmMark{
			{Kind: "backup_age", Since: since}, {Kind: "apply_lag", Since: since.Add(2 * time.Minute)},
		}})
	node(t, backend, "node-b", nil, &coord.NodeStatus{
		InFlight: 1000, Draining: true, Posture: "stuck", AlarmsEvaluated: true})
	node(t, backend, "node-c", map[string]any{"roles": []string{"seats"}}, &coord.NodeStatus{InFlight: 2})
	node(t, backend, "node-d", nil, nil)
	_, chart := healthChart(t)
	catalogue := healthTools(healthFleet(backend, chart), chart)

	answer := healthAnswer(t, callTool(asAdmin(), t, catalogue[builtin.FleetHealthTool], nil))
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
					"posture", "started_at"}, key) {
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

	// code_status: each Gitlet node's own account, as the node gave it.
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
	status2 := healthAnswer(t, callTool(asAdmin(), t, catalogue[builtin.CodeStatusTool], nil))
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
// human-only — code_report_check, a person's own CI's,
// code_purge_unreachable, a person's purge, and code_import_repo, a person's
// import — which it refuses every seat,
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
	deps.Health = healthFleet(backend, chart)
	// THE SSH TOOLS' OWN GRANTS are their own test's: this chart grants none.
	deps.SSH = builtin.SSHDeps{}
	deps.Health.Status, deps.Health.Code = &statusAsker{}, deps.Code
	deps.Requesters = deps.Health.Requesters
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
		if got := callTool(c.ctx, t, entry.Tool, nil); got.Failed == c.admitted {
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
// seat's own grant does not read (ADR-0088): of the hottest repositories the
// Gitlet nodes name, a seat's answer keeps those its grant reads, leaves out one
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
	seat := healthAnswer(t, callTool(asSeat("sre"), t, tool, nil))
	if got, want := hotOf(seat), []string{"node-a/" + siteID, "node-b/" + siteID}; !slices.Equal(got, want) {
		t.Errorf("the seat's hot list = %v, want only what its grant reads: %v", got, want)
	}
	if got := seat["withheld"]; got != float64(3) {
		t.Errorf("the seat's answer withheld %v, want 3 (refused, gone, renamed)", got)
	}
	person := healthAnswer(t, callTool(asAdmin(), t, tool, nil))
	if got := hotOf(person); len(got) != 5 {
		t.Errorf("a person's hot list = %v, want all five entries", got)
	}
	if _, present := person["withheld"]; present {
		t.Errorf("a person's answer withheld %v, want nothing left out", person["withheld"])
	}

	refused := callTool(asSeat("sre"), t, tool, map[string]any{"repo": "vault"})
	if !refused.Failed || !strings.Contains(refused.Output, "vault") {
		t.Errorf("a seat asking about a repository it may not read was answered %q, want refused",
			refused.Output)
	}
	if slices.Contains(asker.asked, vaultID) {
		t.Errorf("the Gitlet nodes were asked about a repository the seat was refused: %v", asker.asked)
	}
	one := healthAnswer(t, callTool(asSeat("sre"), t, tool, map[string]any{"repo": "site"}))
	if one["id"] != siteID || one["repo"] != "site" {
		t.Errorf("the one-repository answer names %v (%v), want site (%s)", one["repo"], one["id"], siteID)
	}
}

// manyHotRepos is a report of nodes Gitlet nodes naming per repositories each,
// every repository on one node alone, as repository homes spread them: the
// fleet's r-th is "repo-<r>" with the r-th smallest id, the hour's use falling
// with r and the day's rising — so the hottest of the hour is the first and
// the hottest of the day the last — and the Gitlet nodes take them in turn. And
// every repository it names, by name, for Gitlet resolving them all.
func manyHotRepos(nodes, per int) (builtin.CodeStatusReport, map[string]code.RepoRef) {
	report := builtin.CodeStatusReport{Statuses: make([]host.CodeStatus, nodes)}
	repos := map[string]code.RepoRef{}
	total := nodes * per
	for r := range total {
		ref := code.RepoRef{ID: fmt.Sprintf("0192f6c1-0000-7000-8000-%012d", r), Name: fmt.Sprintf("repo-%03d", r)}
		repos[ref.Name] = ref
		s := &report.Statuses[r%nodes]
		s.Node = fmt.Sprintf("git-%d", r%nodes)
		s.Hot = append(s.Hot, host.RepoStatus{RepoHeat: cache.RepoHeat{ID: ref.ID, Name: ref.Name,
			Hour: cache.Use{Fetches: int64(total - r)}, Day: cache.Use{Fetches: int64(r + 1)}}})
	}
	return report, repos
}

// stalledGrants is Gitlet, whose read of one repository fails at once and
// whose every other read holds until its context ends — the code log out of
// quorum, where a linearizable read fails only once its barrier's budget is
// spent — counting every read it is asked.
type stalledGrants struct {
	failing string // the name whose read fails at once
	reads   atomic.Int64
}

var _ builtin.CodeReader = (*stalledGrants)(nil)

func (h *stalledGrants) Read(ctx context.Context, q code.ReadQuery) (code.ReadAnswer, error) {
	h.reads.Add(1)
	if q.Repo == h.failing {
		return code.ReadAnswer{}, fmt.Errorf("%w: the chart this node judges by is behind", code.ErrNotServing)
	}
	<-ctx.Done()
	return code.ReadAnswer{}, ctx.Err()
}

func (*stalledGrants) ResolveRef(context.Context, code.RefQuery) (code.RefAnswer, error) {
	return code.RefAnswer{}, errors.New("stalledGrants resolves no ref")
}

func (*stalledGrants) GitRead(context.Context, code.GitQuery) (code.GitAnswer, error) {
	return code.GitAnswer{}, errors.New("stalledGrants reads no git")
}

func (*stalledGrants) FloorOf(string) statelog.Position { return statelog.Position{} }

// A CODE STATUS JUDGEMENT ENDS AT ITS FIRST FAILED READ.
//
// A failure answers the whole call unavailable, so nothing read after it can
// change the answer: with three Gitlet nodes naming forty repositories each, the
// hottest one's grant read failing at once and every other read holding until
// its context ends — the code log out of quorum, whose reads fail only once
// their barrier's budget is spent — the call answers unavailable as soon as
// that one read has failed, having made at most codeStatusJudges reads, and
// names no repository: the one it could not judge may be one the seat may not
// read. Run in a bubble, so "as soon as" is every goroutine the call started
// being done or blocked, never a clock.
//
// Mutation: judge in a group whose first failure cancels nothing, and the
// reads in flight hold the call; start the reads still waiting once one has
// failed, and the call makes every one of them.
func TestACodeStatusJudgementEndsAtItsFirstFailedRead(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		report, _ := manyHotRepos(3, 2*cache.HotRepos)
		stalled := &stalledGrants{failing: "repo-000"}
		_, chart := healthChart(t)
		tool, ok := healthTools(builtin.HealthDeps{Status: &statusAsker{report: report},
			Code: healthCode(stalled, chart)}, chart)[builtin.CodeStatusTool].(tools.SeatCallable)
		if !ok {
			t.Fatal("code_status is not seat-callable")
		}
		ctx, cancel := context.WithCancel(asSeat("sre"))
		defer cancel()
		type outcome struct {
			got tools.Result
			err error
		}
		done := make(chan outcome, 1)
		go func() {
			got, err := tool.CallForTurn(ctx, nil, nil)
			done <- outcome{got, err}
		}()
		synctest.Wait()
		var out outcome
		select {
		case out = <-done:
		default:
			held := stalled.reads.Load()
			cancel()
			<-done
			t.Fatalf("code_status had not answered once its read of the hottest repository failed: "+
				"the %d reads it had made held it", held)
		}
		if out.err != nil {
			t.Fatalf("code_status: %v", out.err)
		}
		if tools.RefusalOf(out.got) != tools.RefusalUnavailable || !errors.Is(out.got.Cause, code.ErrNotServing) {
			t.Errorf("answered %s %q (cause %v), want unavailable over the failed read",
				tools.RefusalOf(out.got), out.got.Output, out.got.Cause)
		}
		if strings.Contains(out.got.Output, "repo-") {
			t.Errorf("the refusal names a repository the seat may not read: %q", out.got.Output)
		}
		if got := stalled.reads.Load(); got > builtin.CodeStatusJudges {
			t.Errorf("the call made %d grant reads, want at most %d — none after the first failed",
				got, builtin.CodeStatusJudges)
		}
	})
}

// A CODE STATUS ANSWER COSTS THE READS IT ANSWERS, NOT THE FLEET'S.
//
// A seat's hot list keeps at most cache.HotRepos of each window, and judging
// every repository the Gitlet nodes name made a grant read apiece — a barrier on
// the code log a wave — that grew with the Gitlet nodes and not with the list. So
// ten Gitlet nodes naming forty repositories each, four hundred in all, are
// judged hottest first and only to the list's cut: the seat is answered
// exactly the list a judgement of every repository answers — the twenty
// hottest of the hour and of the day that it may read — having read only the
// repositories that list keeps and the ones above its cut it may not read, and
// is told how many of those it left out: never one below the cut, which is not
// in the list whoever reads it.
//
// Mutation: judge every repository the nodes name, and the reads count the
// fleet and the cut's withheld count takes in repo-200; stop judging after the
// first wave, and a repository the seat may not read is listed on a guess.
func TestACodeStatusAnswerCostsTheReadsItAnswers(t *testing.T) {
	t.Parallel()
	report, repos := manyHotRepos(10, 2*cache.HotRepos)
	refused := map[string]bool{}
	for _, name := range []string{"repo-000", "repo-001", "repo-002", "repo-003", "repo-004",
		"repo-010", "repo-390", "repo-200"} {
		refused[name] = true
	}
	// EVERY ONE BUT repo-200 is above the cut in one window or the other:
	// repo-200 trails nearly two hundred repositories the seat reads in each.
	const above = 7
	grants := &grantHost{repos: repos, refused: refused}
	_, chart := healthChart(t)
	tool := healthTools(builtin.HealthDeps{Status: &statusAsker{report: report},
		Code: healthCode(grants, chart)}, chart)[builtin.CodeStatusTool]
	answer := healthAnswer(t, callTool(asSeat("sre"), t, tool, nil))

	// WHAT A JUDGEMENT OF EVERY REPOSITORY ANSWERS: the one rule's cut of
	// every entry the seat may read.
	var readable []cache.RepoHeat
	var nodeOf []string
	for _, s := range report.Statuses {
		for _, r := range s.Hot {
			if !refused[r.Name] {
				readable = append(readable, r.RepoHeat)
				nodeOf = append(nodeOf, s.Node)
			}
		}
	}
	var want []string
	for _, i := range cache.Hottest(readable, cache.HotRepos) {
		want = append(want, nodeOf[i]+"/"+readable[i].Name)
	}
	var got []string
	for _, h := range answer["hot"].([]any) {
		row := h.(map[string]any)
		got = append(got, row["node"].(string)+"/"+row["name"].(string))
	}
	if len(want) != 2*cache.HotRepos {
		t.Fatalf("the premise: the list of what the seat reads keeps %d, want both windows' %d",
			len(want), 2*cache.HotRepos)
	}
	if !slices.Equal(got, want) {
		t.Errorf("the seat's hot list = %v, want what judging every repository answers: %v", got, want)
	}
	if got := answer["withheld"]; got != float64(above) {
		t.Errorf("the answer withheld %v, want %d — the repositories above the cut the seat may not read",
			got, above)
	}
	if grants.reads > len(want)+above {
		t.Errorf("the call made %d grant reads for a list of %d, want at most %d: the list's own and the "+
			"%d above its cut — never the %d repositories the fleet names", grants.reads, len(want),
			len(want)+above, above, len(repos))
	}
}

// THE HEALTH TOOLS ARE ONE LIST: a seat's registry and the operator catalogue,
// built over the same deps, hold the same two tools with byte-identical
// descriptions and schemas, each a proven read; deps that reach no lease
// table and nothing of Gitlet serve neither; and each is served only with the
// requester ceiling's halves beside it — fleet_health never over the leases
// alone, and code_status on the code reads' own terms, never over Gitlet
// wired without them — since either would read for whoever woke a turn.
//
// Mutation: OperatorTools omitting healthCandidates, a health tool annotated
// as a write, fleet_health served over the leases alone or code_status over
// Gitlet alone → red.
func TestTheHealthToolsAreOneList(t *testing.T) {
	t.Parallel()
	deps := builtin.HealthDeps{Leases: coordmem.New(), Requesters: codeRequesters(), Chart: chartLeads,
		Status: &statusAsker{}, Code: codeDeps(&grantHost{})}
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
	fleetServed := func(deps builtin.HealthDeps) bool {
		for _, tool := range builtin.HealthTools(deps) {
			if tool.Name() == builtin.FleetHealthTool {
				return true
			}
		}
		return false
	}
	if !fleetServed(builtin.HealthDeps{Leases: deps.Leases, Requesters: deps.Requesters, Chart: deps.Chart}) {
		t.Fatalf("fleet_health is not served over the leases and the ceiling's halves, so this case proves nothing")
	}
	for name, half := range map[string]builtin.HealthDeps{
		"the leases alone":              {Leases: deps.Leases},
		"the leases without Chart":      {Leases: deps.Leases, Requesters: deps.Requesters},
		"the leases without Requesters": {Leases: deps.Leases, Chart: deps.Chart},
	} {
		if fleetServed(half) {
			t.Errorf("fleet_health is served over %s, which would read the deployment for whoever woke "+
				"a turn", name)
		}
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
		"Gitlet alone":              {Code: wired.Code},
		"Gitlet without Chart":      {Code: wired.Code, Requesters: wired.Requesters},
		"Gitlet without Requesters": {Code: wired.Code, Chart: wired.Chart},
	} {
		if served(half) {
			t.Errorf("code_status is served over %s, which serves no code read", name)
		}
	}
}

// A CODE STATUS ANSWER IN A TURN SOMEBODY WOKE IS HELD TO THEIR GRANT TOO.
//
// Its repository reads are code reads, and the requester ceiling holds every
// code read (ADR-0088): in a turn a colleague seat woke, each grant read names
// that seat, so the hot list also leaves out a repository the seat may read
// and the colleague may not — counted withheld beside the one the seat itself
// may not read — and that repository asked by name is refused as the code
// tools refuse a read above the ceiling, forbidden with the ceiling's reason
// over code.ErrAboveCeiling, and never asked of the Gitlet nodes. A turn nobody
// woke, and one a person woke, read for nobody but the seat — a person bounds
// nothing, since every person reads every repository — and the person who
// woke it need not hold admin: whoever woke the turn is decided on
// Gitlet's read verb, not on the health row, which the ceiling does not hold.
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
		tool := seatHealthTools(t, builtin.HealthDeps{Status: s.asker, Code: healthCode(s.repos, chart)},
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
			t.Errorf("the Gitlet nodes were asked about %v for a repository refused the turn's colleague",
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
				t.Errorf("the refused calls read %d repositories and asked the Gitlet nodes %v, want nothing",
					s.repos.reads, s.asker.asked)
			}
		})
	}
}

// countingLeases is a lease table that counts the listings it is asked.
type countingLeases struct {
	builtin.LeaseLister
	lists atomic.Int64
}

func (l *countingLeases) ListLive(ctx context.Context, class coord.Class) ([]coord.Lease, error) {
	l.lists.Add(1)
	return l.LeaseLister.ListLive(ctx, class)
}

// A FLEET HEALTH ANSWER IN A TURN SOMEBODY WOKE IS READ ONLY FOR THE COMPANY.
//
// The seat's grant opens an administrator's view of the deployment, and a seat
// that read it out for whoever messaged it would hand that view to anybody on
// a chat surface it listens on: in a turn a stranger woke, or a person who may
// not act in the company now, fleet_health answers nothing — refused with the
// ceiling's reason, before the presence leases are read — and a requester this
// node cannot compose is undecidable, never a turn nobody woke. A turn nobody
// woke, a colleague seat's and that of a person of the company who holds no
// admin are answered the seat's whole view: whoever woke the turn is decided
// on fleet.health.read, which everybody of the company who may act holds, never
// on the health row, which no requester holds.
//
// Mutation: drop the ceiling, and the stranger's turn is answered; decide the
// requester on fleet_health's own row, and the colleague's and the person's
// turns are refused.
func TestAFleetHealthInATurnSomebodyWokeIsReadOnlyForTheCompany(t *testing.T) {
	t.Parallel()
	o, chart := healthChart(t)
	backend := coordmem.New()
	node(t, backend, "node-a", nil, &coord.NodeStatus{AlarmsEvaluated: true})
	unknown := []types.CanonicalIdentity{{ExternalID: "U-out", Platform: "slack"}}
	speaker := func(handle string) []types.CanonicalIdentity {
		return []types.CanonicalIdentity{{Handle: handle}}
	}
	for _, c := range []struct {
		name      string
		requester string
		senders   []types.CanonicalIdentity
		refusal   tools.Refusal // none where the turn is answered
		says      string
	}{
		{name: "a turn nobody woke"},
		{name: "a turn a colleague seat woke", requester: "dev", senders: speaker("dev")},
		{name: "a turn a person woke who holds no admin", requester: "founder", senders: speaker("founder")},
		{name: "a turn a stranger woke", senders: unknown, refusal: tools.RefusalForbidden,
			says: "woken by somebody the company does not know"},
		{name: "a turn a departed person woke", requester: "departed", senders: speaker("departed"),
			refusal: tools.RefusalForbidden, says: "may not act in the company now"},
		{name: "a turn whose requester this node cannot compose", requester: "ghost",
			senders: speaker("ghost"), refusal: tools.RefusalUnavailable,
			says: "could not establish who this turn's work is for"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			leases := &countingLeases{LeaseLister: backend}
			tool, ok := seatHealthTools(t, healthFleet(leases, chart), chart)[builtin.FleetHealthTool].(tools.SeatCallable)
			if !ok {
				t.Fatal("fleet_health is not seat-callable, so no turn's requester reaches it")
			}
			turn := &turnctx.Turn{RunID: "run-1", WorkKey: "wk-1", Seat: o.AgentSeatByHandle("sre"), Org: o,
				Lookups: turnctx.NewLookups(), Requester: c.requester, Senders: c.senders}
			got, err := tool.CallForTurn(asSeat("sre"), turn, nil)
			if err != nil {
				t.Fatalf("fleet_health: %v", err)
			}
			if c.refusal == "" {
				if nodes, _ := healthAnswer(t, got)["nodes"].([]any); len(nodes) != 1 {
					t.Errorf("answered %d nodes, want the one live node", len(nodes))
				}
				return
			}
			if tools.RefusalOf(got) != c.refusal || !strings.Contains(got.Output, c.says) {
				t.Errorf("answered (%v) %q, want %v saying %q", tools.RefusalOf(got), got.Output,
					c.refusal, c.says)
			}
			var refused *builtin.DecisionRefused
			if c.refusal == tools.RefusalForbidden &&
				(!errors.As(got.Cause, &refused) || refused.Decision.Reason != authz.ReasonCeiling) {
				t.Errorf("the refusal's cause %v carries no ceiling reason", got.Cause)
			}
			if c.refusal == tools.RefusalUnavailable && !errors.Is(got.Cause, builtin.ErrUndecidable) {
				t.Errorf("the refusal's cause %v is not undecidable", got.Cause)
			}
			if n := leases.lists.Load(); n != 0 {
				t.Errorf("a refused call read the presence leases %d times, want none", n)
			}
		})
	}
}

// A FLEET PAST ONE ANSWER IS PAGED, NEVER REFUSED.
//
// fleet_health's answer grows with the fleet — some three hundred bytes a node
// as it is sent, more for its labels and its alarms — so three hundred nodes,
// half of them with long labels and well past where the whole fleet in one
// answer was refused, are read a page at a time by id: twenty-five by default,
// up to the hundred `limit` may ask for, fewer where the labels would make a
// page outgrow one answer, every page compact and within ToolAnswerBytes, and
// every node exactly once across the pages, in id order. `firing` narrows the
// fleet to the nodes whose own table has an alarm firing, counting the nodes
// that cannot say (`unevaluated`) rather than listing them; `node` reads one
// node, a node that is not live is not found, and arguments that cannot be
// read are refused.
//
// Mutation: render a page indented, ignore `cursor`, cap a page by `limit`
// alone, or let `firing` keep a node with nothing firing or one that cannot
// say, and a case is red.
func TestAFleetPastOneAnswerIsPagedNeverRefused(t *testing.T) {
	t.Parallel()
	const nodes = 300
	backend := coordmem.New()
	since := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	wide := map[string]any{} // labels long enough that a hundred rows outgrow one answer
	for i := range 20 {
		wide[fmt.Sprintf("label-%02d", i)] = strings.Repeat("v", 60)
	}
	var all, firing []string
	unevaluated := 0
	for i := range nodes {
		id := fmt.Sprintf("node-%03d", i)
		all = append(all, id)
		meta := map[string]any{"roles": []string{"seats"}}
		if i%2 == 0 {
			meta["labels"] = wide
		}
		var status *coord.NodeStatus
		switch i % 10 {
		case 0: // a data node with an alarm firing
			status = &coord.NodeStatus{AlarmsEvaluated: true,
				Alarms: []coord.AlarmMark{{Kind: "backup_age", Since: since}}}
			firing = append(firing, id)
		case 1: // a data node that looked and found nothing
			status = &coord.NodeStatus{AlarmsEvaluated: true}
		case 2: // a node whose heartbeat carried no status
			unevaluated++
		default: // a node with no table to evaluate
			status = &coord.NodeStatus{InFlight: 1}
			unevaluated++
		}
		node(t, backend, id, meta, status)
	}
	_, chart := healthChart(t)
	tool := healthTools(healthFleet(backend, chart), chart)[builtin.FleetHealthTool]

	type page struct {
		Nodes []struct {
			ID string `json:"id"`
		} `json:"nodes"`
		Next        string `json:"next"`
		Unevaluated *int   `json:"unevaluated"`
	}
	read := func(args map[string]any) page {
		t.Helper()
		got := callTool(asAdmin(), t, tool, args)
		switch {
		case got.Failed:
			t.Fatalf("%v was refused: %s", args, got.Output)
		case len(got.Output) > builtin.ToolAnswerBytes:
			t.Fatalf("%v answered %d bytes, past one tool answer's %d", args, len(got.Output),
				builtin.ToolAnswerBytes)
		case strings.Contains(got.Output, "\n"):
			t.Fatalf("%v was answered indented; a page goes out compact, as it was measured", args)
		}
		var p page
		if err := json.Unmarshal([]byte(got.Output), &p); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return p
	}
	ids := func(p page) []string {
		var out []string
		for _, n := range p.Nodes {
			out = append(out, n.ID)
		}
		return out
	}
	walk := func(args map[string]any, most int) []string {
		t.Helper()
		var seen []string
		for pages := 0; ; pages++ {
			if pages > nodes {
				t.Fatalf("paging %v never ended", args)
			}
			p := read(args)
			if len(p.Nodes) > most {
				t.Errorf("a page of %v listed %d nodes, want at most %d", args, len(p.Nodes), most)
			}
			seen = append(seen, ids(p)...)
			if p.Next == "" {
				return seen
			}
			if len(p.Nodes) == 0 || p.Next != p.Nodes[len(p.Nodes)-1].ID {
				t.Fatalf("next %q is not the last node of its page %v", p.Next, ids(p))
			}
			args = maps.Clone(args)
			args["cursor"] = p.Next
		}
	}

	if first := read(nil); len(first.Nodes) != 25 || first.Next != "node-024" || first.Unevaluated != nil {
		t.Errorf("the first page lists %d nodes and next %q (unevaluated %v), want the default "+
			"twenty-five and node-024", len(first.Nodes), first.Next, first.Unevaluated)
	}
	if got := walk(map[string]any{}, 25); !slices.Equal(got, all) {
		t.Errorf("paging by twenty-five read %d nodes, want every one of %d once, in id order", len(got), nodes)
	}
	if full := read(map[string]any{"limit": 100}); len(full.Nodes) == 0 || len(full.Nodes) >= 100 {
		t.Errorf("a hundred rows half with long labels listed %d, want them cut whole-row to fit one "+
			"answer", len(full.Nodes))
	}
	if got := walk(map[string]any{"limit": 100}, 100); !slices.Equal(got, all) {
		t.Errorf("paging by a hundred read %d nodes, want every one of %d once, in id order", len(got), nodes)
	}

	f := read(map[string]any{"firing": true, "limit": 100})
	if !slices.Equal(ids(f), firing) || f.Next != "" {
		t.Errorf("firing listed %v (next %q), want exactly the nodes with an alarm firing: %v",
			ids(f), f.Next, firing)
	}
	if f.Unevaluated == nil || *f.Unevaluated != unevaluated {
		t.Errorf("firing counts %v nodes that cannot say, want %d", f.Unevaluated, unevaluated)
	}
	if got := walk(map[string]any{"firing": true, "limit": 7}, 7); !slices.Equal(got, firing) {
		t.Errorf("paging the firing nodes by seven read %v, want %v", got, firing)
	}
	if one := read(map[string]any{"node": "node-007"}); !slices.Equal(ids(one), []string{"node-007"}) {
		t.Errorf("node read %v, want node-007 alone", ids(one))
	}

	for _, c := range []struct {
		args    map[string]any
		refusal tools.Refusal
	}{
		{map[string]any{"node": "node-999"}, tools.RefusalNotFound},
		{map[string]any{"node": "node-007", "firing": true}, tools.RefusalInvalid},
		{map[string]any{"node": "node-007", "cursor": "node-003"}, tools.RefusalInvalid},
		{map[string]any{"limit": 0}, tools.RefusalInvalid},
		{map[string]any{"limit": 101}, tools.RefusalInvalid},
		{map[string]any{"firing": "yes"}, tools.RefusalInvalid},
	} {
		if got := callTool(asAdmin(), t, tool, c.args); tools.RefusalOf(got) != c.refusal {
			t.Errorf("%v answered (%v) %q, want %v", c.args, tools.RefusalOf(got), got.Output, c.refusal)
		}
	}
}

// A FLEET OF GITLET NODES PAST ONE ANSWER IS PAGED, NEVER REFUSED — code_status's
// answer about the fleet and its answer about one repository alike — AND A
// PAGE ASKS ITS OWN GITLET NODES ALONE.
//
// Four hundred Gitlet nodes, half with ids long enough that a hundred rows outgrow
// one answer, and a tenth that did not answer: whole, either answer is far past
// ToolAnswerBytes. Read a page at a time, every page fits one answer and goes
// out compact; walking the pages by `next` lists every Gitlet node once and in id
// order — each answered node's row, and every node's part in the coverage, an
// unanswered node's included — the roster sorted however it was listed; a
// page of a hundred is cut whole-row; the hot list rides the fleet's first page
// alone; `node` reads one Gitlet node, one that did not answer as its coverage;
// and arguments that cannot be read are refused.
//
// Every call asks the Gitlet nodes exactly one question, and only the fleet's
// first page asks every Gitlet node, each for its hot list: a later page — and
// every page of a repository — asks the `limit` nodes after its cursor, and no
// hot list, and `node` asks that node alone, so walking N Gitlet nodes costs the
// N answers of its pages plus one fleet-wide first page, never N answers a
// page. A page's `complete` is whether every node it lists answered — on the
// fleet's first page, whether every Gitlet node did, its hot list drawn from all
// of them. A refused call, a node that is no live Gitlet node among them, asks
// nobody; and a roster that cannot be read is refused, unavailable, before
// anybody is asked.
//
// Mutation: send the page `limit` asks whole rather than cut to one answer,
// ignore `cursor`, send the hot list on every page, ask every Gitlet node for
// every page, read the roster in the order it was listed, or give the fleet's
// first page its listed nodes' `complete`, and a case is red.
func TestAGitFleetPastOneAnswerIsPagedNeverRefused(t *testing.T) {
	t.Parallel()
	const nodes = 400
	at := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	wide := strings.Repeat("g", 600) // an id long enough that a hundred rows outgrow one answer
	var all, answered, silent []string
	report := builtin.CodeStatusReport{Coverage: eventfan.Coverage{Nodes: []eventfan.NodeCoverage{}}}
	for i := range nodes {
		id := fmt.Sprintf("git-%03d", i)
		if i%2 == 0 {
			id += "-" + wide
		}
		all = append(all, id)
		if i%10 == 3 {
			silent = append(silent, id)
			report.Coverage.Nodes = append(report.Coverage.Nodes, eventfan.NodeCoverage{ID: id,
				Error: "did not answer inside 5s"})
			continue
		}
		answered = append(answered, id)
		report.Coverage.Nodes = append(report.Coverage.Nodes, eventfan.NodeCoverage{ID: id, Answered: true})
		s := host.CodeStatus{Node: id, At: at, Cache: cache.Status{Since: at, Bytes: int64(i) << 20,
			Budget: 8 << 30, Warm: int64(i)},
			Repo: &host.RepoStatus{RepoHeat: cache.RepoHeat{ID: siteID, Name: "site",
				Hour: cache.Use{Fetches: int64(i)}}}}
		if i < 40 {
			s.Hot = []host.RepoStatus{{RepoHeat: cache.RepoHeat{
				ID: fmt.Sprintf("0192f6c1-0000-7000-8000-%012d", i), Name: fmt.Sprintf("repo-%03d", i),
				Hour: cache.Use{Fetches: int64(100 - i)}, Day: cache.Use{Fetches: int64(i + 1)}}}}
		}
		report.Statuses = append(report.Statuses, s)
	}
	for _, list := range [][]string{all, answered, silent} {
		slices.Sort(list)
	}
	slices.SortFunc(report.Coverage.Nodes, func(a, b eventfan.NodeCoverage) int {
		return strings.Compare(a.ID, b.ID)
	})
	if whole, _ := json.Marshal(report); len(whole) <= 4*builtin.ToolAnswerBytes {
		t.Fatalf("the fleet's report is %d bytes whole; the case needs one far past an answer", len(whole))
	}
	_, chart := healthChart(t)
	repos := &grantHost{repos: map[string]code.RepoRef{"site": {ID: siteID, Name: "site"}}}
	asker := &statusAsker{report: report}
	tool := healthTools(builtin.HealthDeps{Status: asker, Code: healthCode(repos, chart)},
		chart)[builtin.CodeStatusTool]

	// question is the one question a call with args asks the Gitlet nodes.
	question := func(args map[string]any) builtin.CodeStatusAsk {
		fleet, repo := args["repo"] == nil, ""
		if !fleet {
			repo = siteID
		}
		if node, ok := args["node"].(string); ok {
			return builtin.CodeStatusAsk{Nodes: []string{node}, Repo: repo, Hot: fleet}
		}
		cursor, _ := args["cursor"].(string)
		if fleet && cursor == "" {
			return builtin.CodeStatusAsk{Nodes: all, Every: true, Hot: true}
		}
		limit := 25
		if l, ok := args["limit"].(int); ok {
			limit = l
		}
		rest := all[afterCursor(all, cursor):]
		return builtin.CodeStatusAsk{Nodes: rest[:min(limit, len(rest))], Repo: repo}
	}
	asked := func(args map[string]any, want []builtin.CodeStatusAsk) {
		t.Helper()
		got := asker.questions()
		same := len(got) == len(want)
		for i := 0; same && i < len(got); i++ {
			same = slices.Equal(got[i].Nodes, want[i].Nodes) && got[i].Every == want[i].Every &&
				got[i].Repo == want[i].Repo && got[i].Hot == want[i].Hot
		}
		if !same {
			t.Errorf("%v asked the Gitlet nodes %s, want %s", args, describeAsks(got), describeAsks(want))
		}
	}

	type page struct {
		Nodes []struct {
			Node string `json:"node"`
		} `json:"nodes"`
		Hot *[]struct {
			Node string `json:"node"`
		} `json:"hot"`
		Next     string            `json:"next"`
		Coverage eventfan.Coverage `json:"coverage"`
	}
	read := func(args map[string]any) page {
		t.Helper()
		got := callTool(asAdmin(), t, tool, args)
		switch {
		case got.Failed:
			t.Fatalf("%v was refused: %s", args, got.Output)
		case len(got.Output) > builtin.ToolAnswerBytes:
			t.Fatalf("%v answered %d bytes, past one tool answer's %d", args, len(got.Output),
				builtin.ToolAnswerBytes)
		case strings.Contains(got.Output, "\n"):
			t.Fatalf("%v was answered indented; a page goes out compact, as it was measured", args)
		}
		asked(args, []builtin.CodeStatusAsk{question(args)})
		var p page
		if err := json.Unmarshal([]byte(got.Output), &p); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return p
	}
	rows := func(p page) []string {
		var out []string
		for _, n := range p.Nodes {
			out = append(out, n.Node)
		}
		return out
	}
	covered := func(p page) []string {
		var out []string
		for _, n := range p.Coverage.Nodes {
			out = append(out, n.ID)
		}
		return out
	}
	// listedAnswered is whether every node p lists answered.
	listedAnswered := func(p page) bool {
		for _, n := range p.Coverage.Nodes {
			if !n.Answered {
				return false
			}
		}
		return true
	}
	with := func(form map[string]any, key string, v any) map[string]any {
		args := maps.Clone(form)
		args[key] = v
		return args
	}
	// walk reads every page args asks for, each at most `most` nodes, and
	// answers the answered nodes' rows and every node's coverage it listed.
	walk := func(args map[string]any, most int) (gotRows, gotCovered []string) {
		t.Helper()
		for pages := 0; ; pages++ {
			if pages > nodes {
				t.Fatalf("paging %v never ended", args)
			}
			p := read(args)
			if len(p.Coverage.Nodes) > most {
				t.Errorf("a page of %v listed %d nodes, want at most %d", args, len(p.Coverage.Nodes), most)
			}
			fleetFirst := pages == 0 && args["repo"] == nil
			if want := listedAnswered(p) && !fleetFirst; p.Coverage.Complete != want {
				t.Errorf("page %d of %v reads complete %v, want %v: whether every node it lists answered, "+
					"and on the fleet's first page whether every Gitlet node did", pages, args,
					p.Coverage.Complete, want)
			}
			if (p.Hot != nil) != fleetFirst {
				t.Errorf("page %d of %v carries a hot list: %v; it rides the fleet's first page alone",
					pages, args, p.Hot != nil)
			}
			gotRows, gotCovered = append(gotRows, rows(p)...), append(gotCovered, covered(p)...)
			if p.Next == "" {
				return gotRows, gotCovered
			}
			if c := covered(p); len(c) == 0 || p.Next != c[len(c)-1] {
				t.Fatalf("next %q is not the last node of its page", p.Next)
			}
			args = with(args, "cursor", p.Next)
		}
	}

	for _, form := range []map[string]any{{}, {"repo": "site"}} {
		first := read(form)
		if len(first.Coverage.Nodes) != 25 || first.Next != all[24] {
			t.Errorf("%v's first page lists %d nodes and next %q, want the default twenty-five and %q",
				form, len(first.Coverage.Nodes), first.Next, all[24])
		}
		if form["repo"] == nil && (first.Hot == nil || len(*first.Hot) == 0) {
			t.Errorf("the fleet's first page carries no hot list")
		}
		// THE FIRST TWO GITLET NODES BOTH ANSWERED: the fleet's first page is
		// still incomplete — its hot list is every Gitlet node's, and a tenth
		// of them did not answer — and the repository's is complete.
		if two := read(with(form, "limit", 2)); !slices.Equal(covered(two), all[:2]) ||
			two.Coverage.Complete != (form["repo"] != nil) {
			t.Errorf("%v: a first page of two answered nodes covered %v complete %v, want %v complete %v",
				form, covered(two), two.Coverage.Complete, all[:2], form["repo"] != nil)
		}
		for _, most := range []int{25, 100} {
			args := form
			if most != 25 {
				args = with(form, "limit", most)
			}
			gotRows, gotCovered := walk(args, most)
			if !slices.Equal(gotRows, answered) || !slices.Equal(gotCovered, all) {
				t.Errorf("paging %v by %d read %d rows and %d nodes' coverage, want every answered node's "+
					"row (%d) and every node's coverage (%d) once, in id order", form, most, len(gotRows),
					len(gotCovered), len(answered), len(all))
			}
		}
		if full := read(with(form, "limit", 100)); len(full.Coverage.Nodes) == 0 ||
			len(full.Coverage.Nodes) >= 100 {
			t.Errorf("%v: a hundred rows half with long ids listed %d, want them cut whole-row to fit one "+
				"answer", form, len(full.Coverage.Nodes))
		}
		if one := read(with(form, "node", answered[5])); !slices.Equal(rows(one), answered[5:6]) ||
			!slices.Equal(covered(one), answered[5:6]) || one.Next != "" || !one.Coverage.Complete {
			t.Errorf("%v: node read %v (coverage %+v, next %q), want %s alone, complete", form, rows(one),
				one.Coverage, one.Next, answered[5])
		}
		if quiet := read(with(form, "node", silent[0])); len(quiet.Nodes) != 0 ||
			!slices.Equal(covered(quiet), silent[:1]) || quiet.Coverage.Nodes[0].Answered ||
			quiet.Coverage.Complete {
			t.Errorf("%v: a node that did not answer read rows %v and coverage %+v, want no row and its "+
				"coverage, incomplete", form, rows(quiet), quiet.Coverage)
		}
		for _, c := range []struct {
			args    map[string]any
			refusal tools.Refusal
		}{
			{with(form, "node", "git-999"), tools.RefusalNotFound},
			{with(with(form, "node", answered[5]), "cursor", all[3]), tools.RefusalInvalid},
			{with(with(form, "node", answered[5]), "limit", 10), tools.RefusalInvalid},
			{with(form, "limit", 0), tools.RefusalInvalid},
			{with(form, "limit", 101), tools.RefusalInvalid},
		} {
			if got := callTool(asAdmin(), t, tool, c.args); tools.RefusalOf(got) != c.refusal {
				t.Errorf("%v answered (%v) %q, want %v", c.args, tools.RefusalOf(got), got.Output, c.refusal)
			}
			asked(c.args, nil)
		}
	}

	asker.mu.Lock()
	asker.rosterErr = errors.New("the coordination store is away")
	asker.mu.Unlock()
	for _, form := range []map[string]any{{}, {"repo": "site"}, {"node": answered[5]}} {
		if got := callTool(asAdmin(), t, tool, form); tools.RefusalOf(got) != tools.RefusalUnavailable {
			t.Errorf("%v with no roster answered (%v) %q, want unavailable", form, tools.RefusalOf(got), got.Output)
		}
		asked(form, nil)
	}
}

// afterCursor is where the ids after cursor begin in sorted ids.
func afterCursor(ids []string, cursor string) int {
	i, found := slices.BinarySearch(ids, cursor)
	if found {
		i++
	}
	return i
}

// describeAsks is asks as a failure message reads them: each question's node
// count, its first and last node, and its flags.
func describeAsks(asks []builtin.CodeStatusAsk) string {
	var out []string
	for _, a := range asks {
		first, last := "", ""
		if len(a.Nodes) > 0 {
			first, last = clipID(a.Nodes[0]), clipID(a.Nodes[len(a.Nodes)-1])
		}
		out = append(out, fmt.Sprintf("{%d nodes %s..%s every=%v repo=%q hot=%v}", len(a.Nodes), first, last,
			a.Every, a.Repo, a.Hot))
	}
	return "[" + strings.Join(out, " ") + "]"
}

// clipID is an id short enough for a failure message.
func clipID(id string) string {
	if len(id) > 12 {
		return id[:12] + "…"
	}
	return id
}

// AN ALARM'S WAKE ASKS FLEET_HEALTH ONLY FOR WHAT IS FIRING.
//
// Every turn an alarm starts reads fleet_health, so the wake sends its seat
// there narrowed to the nodes whose own table has an alarm firing — an answer
// that stays small however many nodes the fleet has — in an argument the tool
// declares, so the wake's advice is never one the tool refuses as an argument
// it does not read.
//
// Mutation: rename the tool's `firing`, or drop it from the prompt → red.
func TestAnAlarmWakeAsksFleetHealthOnlyForWhatIsFiring(t *testing.T) {
	t.Parallel()
	body := alarmwake.Prompt{}.Build(notify.Inbound{Metadata: map[string]string{
		alarmwake.AlarmField: "backup_age", alarmwake.NodeField: "node-a",
		alarmwake.ChangeField: string(statelog.TransitionFired)}}, nil)
	if want := "`" + builtin.FleetHealthTool + "` with `firing: true`"; !strings.Contains(body, want) {
		t.Errorf("the wake's prompt does not say %q:\n%s", want, body)
	}
	_, chart := healthChart(t)
	tool := healthTools(healthFleet(coordmem.New(), chart), chart)[builtin.FleetHealthTool]
	props, _ := tool.Parameters()["properties"].(map[string]any)
	if firing, _ := props["firing"].(map[string]any); firing["type"] != "boolean" {
		t.Errorf("fleet_health declares no boolean `firing`: %v", props["firing"])
	}
}
