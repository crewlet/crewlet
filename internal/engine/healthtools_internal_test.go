package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/authz/orgchart"
	"github.com/crewlet/crewlet/internal/code/host"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/eventfan"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/tools"
)

// healthToolsCompany is a company of two agent seats, one whose role carries
// the health grant and one whose role does not.
const healthToolsCompany = `
name: Acme
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

// A SEAT'S MODEL IS SHOWN THE HEALTH TOOLS ONLY WHERE ITS ROLE CARRIES THE
// GRANT. Every seat's registry holds them, as it holds every builtin; the
// runner the epoch builds for a turn leaves out what the authority table
// refuses the seat before anything is named — decided as the seat the turn
// acts as, over the epoch's own chart — so the seat whose role does not carry
// the grant is shown neither, and the seat whose role does is shown both. Read
// off what the executor sends its model, because the decision is the epoch's
// and the leaving out is the runner's, and either half alone could drop it.
//
// Mutation: RunnerFor not handing the runner what withheldFrom answers, or
// withheldFrom deciding as anybody but the seat, and the ungranted seat is
// shown both tools; withheldFrom deciding over a chart that grants nothing,
// and the granted seat is shown neither.
func TestASeatsModelIsShownTheHealthToolsOnlyWhereItsRoleIsGranted(t *testing.T) {
	t.Parallel()
	c, err := NewCompany(parseCompany(t, healthToolsCompany))
	if err != nil {
		t.Fatalf("NewCompany: %v", err)
	}
	// code_status registers on the code reads' terms: the code host, and who a
	// turn is acting for with the chart that decides them, composed off this
	// company's running chart as the engine composes them.
	e := &Engine{}
	e.epoch.current.Store(c)
	reg := tools.NewRegistry()
	if _, err := builtin.Register(reg, builtin.Deps{
		Authorize: builtin.Decide(orgchart.Of(c.Org)),
		Health: builtin.HealthDeps{Leases: coordmem.New(), Self: "node-a", Status: idleGitNodes{},
			Code: builtin.CodeDeps{Code: unreadCode{}, Requesters: turnRequesters{engine: e},
				Chart: orgchart.Of(c.Org)}},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	for _, name := range builtin.HealthToolNames() {
		if _, ok := reg.Lookup(name); !ok {
			t.Fatalf("the premise: the registry holds no %s", name)
		}
	}

	for _, seat := range []struct {
		handle  string
		granted bool
	}{{"sre", true}, {"dev", false}} {
		shown := shownTools(t, c, seat.handle, reg)
		if !slices.Contains(shown, builtin.LookupColleagueTool) {
			t.Fatalf("%s's model was shown %v: the recorder saw no surface at all", seat.handle, shown)
		}
		for _, name := range builtin.HealthToolNames() {
			if slices.Contains(shown, name) != seat.granted {
				t.Errorf("%s (granted %v) was shown %s: %v", seat.handle, seat.granted, name,
					slices.Contains(shown, name))
			}
		}
	}
}

// shownTools is the tools handle's executor run is handed on the first round
// of a turn c builds over reg, the run launching nothing.
func shownTools(t *testing.T, c *Company, handle string, reg *tools.Registry) []string {
	t.Helper()
	names, _ := shownExecutor(t, c, handle, reg)
	return names
}

// shownExecutor is [shownTools] and everything the executor's first run is
// launched with beside them — its standing instructions and its brief — the
// run launching nothing.
//
// THE SURFACE THE LAUNCH IS HANDED, since the executor is its coding CLI's own
// run: the bridge serves that surface and nothing else, so what is active on
// it is exactly what the run may call.
func shownExecutor(t *testing.T, c *Company, handle string, reg *tools.Registry) ([]string, string) {
	t.Helper()
	models, err := phase.NewRegistry([]phase.Entry{{Key: "only", Provider: refusingProvider{}}})
	if err != nil {
		t.Fatalf("phase registry: %v", err)
	}
	c.Models = models
	sent := &surfaceRecorder{}
	r, err := c.RunnerFor(t.Context(), handle, reg, RunnerInput{Task: "look at the fleet",
		Reply: turn.NoReply(), Launcher: sent, Sessions: testSessions(t)})
	if err != nil {
		t.Fatalf("RunnerFor(%s): %v", handle, err)
	}
	// The launch is refused: only what the run was handed is under test.
	_, _, _ = r.Execute(t.Context(), 1, "", nil)
	return sent.first()
}

// surfaceRecorder is an executor launcher that records the tool names and the
// prompt of the first run it is handed and launches nothing.
type surfaceRecorder struct {
	mu     sync.Mutex
	names  []string
	prompt string
	seen   bool
}

func (s *surfaceRecorder) LaunchExecutor(_ context.Context, req runner.AgentRunRequest) (runner.Launched, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.seen {
		s.seen = true
		if req.Surface != nil {
			s.names = req.Surface.Active()
		}
		s.prompt = req.System + "\n" + req.Brief + "\n"
	}
	return runner.Launched{}, errors.New("the recorder launches nothing")
}

func (s *surfaceRecorder) first() ([]string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.names), s.prompt
}

// idleGitNodes is a status scatter no case calls.
type idleGitNodes struct{}

func (idleGitNodes) CodeStatus(context.Context, string) (builtin.CodeStatusReport, error) {
	return builtin.CodeStatusReport{}, errors.New("no case asks the git nodes")
}

// unreadCode is a code host no case reads: registering code_status needs one.
type unreadCode struct{ builtin.CodeReader }

// A CODE STATUS REPORT NAMES EVERY GIT NODE IT ASKED: each git node the
// presence leases listed is answered or named with why not; a node that
// answered though the roster did not list it yet (one that joined since) is
// kept; one node is one status, however often its broker delivered it, and a
// later reply never unsays one already read; a reply at a version this build
// does not read is named rather than read, and one it cannot read at all
// names nobody. The coverage is complete only where the roster was read and
// every node on it answered — a roster that could not be read leaves it
// incomplete, since nobody can say who was missing.
//
// Mutation: leave the roster's silent nodes out of the coverage, read a reply
// at another version, or keep a second reply from one node, and a case goes
// red.
func TestACodeStatusReportNamesEveryGitNodeItAsked(t *testing.T) {
	t.Parallel()
	reply := func(node string, version int) []byte {
		raw, err := json.Marshal(codeStatusAnswer{Version: version, Status: host.CodeStatus{
			Node: node, At: time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)}})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	silent := fmt.Sprintf("did not answer inside %s", codeStatusBudget)
	for _, c := range []struct {
		name      string
		roster    []string
		rosterErr error
		replies   [][]byte
		statuses  []string
		nodes     []eventfan.NodeCoverage
		complete  bool
	}{
		{name: "every git node answered", roster: []string{"git-b", "git-a"},
			replies:  [][]byte{reply("git-a", codeStatusProtocol), reply("git-b", codeStatusProtocol)},
			statuses: []string{"git-a", "git-b"},
			nodes: []eventfan.NodeCoverage{{ID: "git-a", Answered: true},
				{ID: "git-b", Answered: true}},
			complete: true},
		{name: "a git node that did not answer is named", roster: []string{"git-a", "git-b"},
			replies:  [][]byte{reply("git-a", codeStatusProtocol)},
			statuses: []string{"git-a"},
			nodes: []eventfan.NodeCoverage{{ID: "git-a", Answered: true},
				{ID: "git-b", Error: silent}}},
		{name: "a node the roster did not list yet is kept", roster: []string{"git-a"},
			replies:  [][]byte{reply("git-c", codeStatusProtocol), reply("git-a", codeStatusProtocol)},
			statuses: []string{"git-a", "git-c"},
			nodes: []eventfan.NodeCoverage{{ID: "git-a", Answered: true},
				{ID: "git-c", Answered: true}},
			complete: true},
		{name: "one node is one status", roster: []string{"git-a"},
			replies: [][]byte{reply("git-a", codeStatusProtocol), reply("git-a", codeStatusProtocol),
				reply("git-a", codeStatusProtocol+1)},
			statuses: []string{"git-a"},
			nodes:    []eventfan.NodeCoverage{{ID: "git-a", Answered: true}},
			complete: true},
		{name: "a reply at another version is named, not read", roster: []string{"git-a"},
			replies: [][]byte{reply("git-a", codeStatusProtocol+1)},
			nodes: []eventfan.NodeCoverage{{ID: "git-a", Error: fmt.Sprintf(
				"answered at status protocol %d, and this build reads %d", codeStatusProtocol+1,
				codeStatusProtocol)}}},
		{name: "a reply this build cannot read names nobody", roster: []string{"git-a"},
			replies: [][]byte{[]byte("not a status"), reply("", codeStatusProtocol)},
			nodes:   []eventfan.NodeCoverage{{ID: "git-a", Error: silent}}},
		{name: "a roster that could not be read leaves it incomplete",
			rosterErr: errors.New("the coordination store is away"),
			replies:   [][]byte{reply("git-a", codeStatusProtocol)},
			statuses:  []string{"git-a"},
			nodes:     []eventfan.NodeCoverage{{ID: "git-a", Answered: true}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			report := codeStatusReport(c.roster, c.rosterErr, c.replies)
			var statuses []string
			for _, s := range report.Statuses {
				statuses = append(statuses, s.Node)
			}
			if !slices.Equal(statuses, c.statuses) {
				t.Errorf("statuses from %v, want from %v", statuses, c.statuses)
			}
			if !slices.Equal(report.Coverage.Nodes, c.nodes) {
				t.Errorf("coverage names %+v, want %+v", report.Coverage.Nodes, c.nodes)
			}
			if report.Coverage.Complete != c.complete {
				t.Errorf("coverage complete = %v, want %v", report.Coverage.Complete, c.complete)
			}
		})
	}
}

// A CODE STATUS ASK WAITS FOR EVERY GIT NODE THE ROSTER NAMES, BY NAME. Every
// git node's host serves the question, one the roster does not hold among
// them — a draining git node gives its presence back first and its host
// serves until it stops — and a scatter's replies carry no sender: an ask that
// ended at as many replies as the roster names ended on such a node's answer
// before a named one's, and the report called that node silent for its whole
// budget. Here git-d, draining, answers first, and git-b answers only once
// git-d's reply has reached the asker: the ask still waits for git-b, and the
// report is complete, git-d's status kept beside it.
//
// Mutation: end the ask at as many replies as the roster names nodes — git-d's
// answer fills it, and git-b is named silent, red.
func TestACodeStatusAskWaitsForEveryGitNodeTheRosterNames(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	asker, gitB, gitD := broker.Client(), broker.Client(), broker.Client()
	for _, q := range []*memory.Queue{asker, gitB, gitD} {
		if err := q.Start(t.Context()); err != nil {
			t.Fatalf("start a node's queue: %v", err)
		}
		t.Cleanup(func() { _ = q.Stop(context.Background()) })
	}
	status := func(node string) queue.AnswerFunc {
		return func(context.Context, []byte) ([]byte, error) {
			return json.Marshal(codeStatusAnswer{Version: codeStatusProtocol, Status: host.CodeStatus{Node: node}})
		}
	}
	gate := make(chan struct{})
	answerB := status("git-b")
	stopB, err := gitB.Serve(t.Context(), topics.CodeStatus, queue.ConcurrentAnswers,
		func(ctx context.Context, raw []byte) ([]byte, error) {
			select {
			case <-gate:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return answerB(ctx, raw)
		})
	if err != nil {
		t.Fatalf("serve git-b's status: %v", err)
	}
	t.Cleanup(func() { _ = stopB(context.Background()) })
	stopD, err := gitD.Serve(t.Context(), topics.CodeStatus, queue.ConcurrentAnswers, status("git-d"))
	if err != nil {
		t.Fatalf("serve git-d's status: %v", err)
	}
	t.Cleanup(func() { _ = stopD(context.Background()) })

	var once sync.Once
	askEach := func(ctx context.Context, subject string, request []byte, each func([]byte) bool) error {
		return asker.AskEach(ctx, subject, request, func(raw []byte) bool {
			more := each(raw)
			var a codeStatusAnswer
			if json.Unmarshal(raw, &a) == nil && a.Status.Node == "git-d" {
				once.Do(func() { close(gate) })
			}
			return more
		})
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	roster := []string{"git-b"}
	replies, err := askCodeStatus(ctx, askEach, []byte(`{}`), roster, nil)
	if err != nil {
		t.Fatalf("ask the git nodes: %v", err)
	}
	report := codeStatusReport(roster, nil, replies)
	want := []eventfan.NodeCoverage{{ID: "git-b", Answered: true}, {ID: "git-d", Answered: true}}
	if !report.Coverage.Complete || !slices.Equal(report.Coverage.Nodes, want) {
		t.Fatalf("coverage %+v, want complete with %+v: git-b answered, after git-d", report.Coverage, want)
	}
}

// THE HEALTH TOOLS ARE OFFERED WHERE THEIR READS ARE: fleet_health wherever
// the node reads the fleet's presence leases, which every node of a running
// fleet does, and code_status on the code tools' own terms — a node serving
// the code host holds it and one of a company hosting no code does not, whose
// fleet has no git node to ask. Through the running wiring: the seat's
// registry is equipped at the apply, and the operator's halves read the same
// deps. No engine, or one that runs nothing, offers neither.
//
// Mutation: HealthToolDeps handing Status where the code tools are not
// offered, or a code half without the requester ceiling's halves, or equip
// dropping deps.Health, and a case goes red.
func TestTheHealthToolsAreOfferedWhereTheirReadsAre(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, doc  string
		codeStatus bool
	}{
		{"hosting code", codeHostCompany, true},
		{"hosting none", nativeCleanupCompany, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newSandboxNode(t, parseCompany(t, c.doc))
			deps := HealthToolDeps(e)
			if deps.Leases == nil || deps.Self != e.id {
				t.Fatalf("a running node's health deps read no presence leases: %+v", deps)
			}
			if got := deps.Status != nil && deps.Code.Code != nil; got != c.codeStatus {
				t.Errorf("code_status's deps present = %v, want %v", got, c.codeStatus)
			}
			// ITS REPOSITORY READS ARE THE CODE READS', made for whoever woke
			// the turn: the requester ceiling's halves ride beside the code
			// host wherever code_status is offered.
			if c.codeStatus && (deps.Code.Requesters == nil || deps.Code.Chart == nil) {
				t.Errorf("code_status's code half wires no requester ceiling: Requesters %v, Chart %v",
					deps.Code.Requesters, deps.Code.Chart)
			}
			if _, ok := e.Company().Tools.Lookup(builtin.FleetHealthTool); !ok {
				t.Errorf("a seat's registry holds no %s", builtin.FleetHealthTool)
			}
			if _, ok := e.Company().Tools.Lookup(builtin.CodeStatusTool); ok != c.codeStatus {
				t.Errorf("a seat's registry holds %s = %v, want %v", builtin.CodeStatusTool, ok,
					c.codeStatus)
			}
		})
	}
	if deps := HealthToolDeps(nil); deps.Leases != nil || deps.Status != nil {
		t.Error("no engine offered the health tools")
	}
	if deps := HealthToolDeps(&Engine{}); deps.Leases != nil || deps.Status != nil {
		t.Error("an engine running nothing offered the health tools")
	}
	if got := strings.Join(builtin.HealthToolNames(), ","); got != "fleet_health,code_status" {
		t.Errorf("the health tools are %s, want fleet_health and code_status", got)
	}
}
