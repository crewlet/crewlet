package engine

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
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/authz/orgchart"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/eventfan"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/host"
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
// GRANT. Every seat's registry holds them; the runner the epoch builds leaves
// out what the authority table refuses the seat, decided as the seat the turn
// acts as over the epoch's own chart. Read off what the executor sends its
// model, since the decision is the epoch's and the leaving out the runner's,
// and either half alone could drop it.
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
	// code_status registers on the code reads' terms: Gitlet, and who a
	// turn is acting for with the chart that decides them, composed off this
	// company's running chart as the engine composes them.
	e := &Engine{}
	e.epoch.current.Store(c)
	reg := tools.NewRegistry()
	if _, err := builtin.Register(reg, builtin.Deps{
		Authorize:  builtin.Decide(orgchart.Of(c.Org)),
		Requesters: turnRequesters{engine: e},
		Health: builtin.HealthDeps{Leases: coordmem.New(), Self: "node-a", Requesters: turnRequesters{engine: e},
			Chart: orgchart.Of(c.Org), Status: idleGitletNodes{},
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

// shownExecutor is [shownTools] plus the standing instructions and brief the
// executor's first run is launched with, the run launching nothing. The launch
// is the surface under test: the bridge serves exactly what the run was handed.
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

// idleGitletNodes is a status scatter no case calls.
type idleGitletNodes struct{}

func (idleGitletNodes) GitletNodes(context.Context) ([]string, error) {
	return nil, errors.New("no case reads the Gitlet nodes")
}

func (idleGitletNodes) CodeStatus(context.Context, builtin.CodeStatusAsk) (builtin.CodeStatusReport, error) {
	return builtin.CodeStatusReport{}, errors.New("no case asks the Gitlet nodes")
}

// unreadCode is Gitlet's reads as no case asks them: registering code_status needs a reader.
type unreadCode struct{ builtin.CodeReader }

// A CODE STATUS REPORT IS ABOUT THE GITLET NODES IT ASKED, EVERY ONE: each is
// answered or named with why not; a reply from a node not asked (an older build
// answers whatever a question names, a node off the roster a question naming
// nobody) is dropped; one node is one status however often its broker
// delivered it, and a later reply never unsays one already read; a reply at a
// version this build does not read is named rather than read, and one it
// cannot read at all names nobody. Coverage is complete only where every node
// asked answered.
//
// Mutation: leave the silent nodes out of the coverage, keep a reply from a
// node not asked, read a reply at another version, or keep a second reply
// from one node, and a case goes red.
func TestACodeStatusReportIsAboutTheGitletNodesItAsked(t *testing.T) {
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
		name     string
		asked    []string
		replies  [][]byte
		statuses []string
		nodes    []eventfan.NodeCoverage
		complete bool
	}{
		{name: "every Gitlet node answered", asked: []string{"git-b", "git-a"},
			replies:  [][]byte{reply("git-a", codeStatusProtocol), reply("git-b", codeStatusProtocol)},
			statuses: []string{"git-a", "git-b"},
			nodes: []eventfan.NodeCoverage{{ID: "git-a", Answered: true},
				{ID: "git-b", Answered: true}},
			complete: true},
		{name: "a Gitlet node that did not answer is named", asked: []string{"git-a", "git-b"},
			replies:  [][]byte{reply("git-a", codeStatusProtocol)},
			statuses: []string{"git-a"},
			nodes: []eventfan.NodeCoverage{{ID: "git-a", Answered: true},
				{ID: "git-b", Error: silent}}},
		{name: "a node not asked is dropped", asked: []string{"git-a"},
			replies:  [][]byte{reply("git-c", codeStatusProtocol), reply("git-a", codeStatusProtocol)},
			statuses: []string{"git-a"},
			nodes:    []eventfan.NodeCoverage{{ID: "git-a", Answered: true}},
			complete: true},
		{name: "one node is one status", asked: []string{"git-a"},
			replies: [][]byte{reply("git-a", codeStatusProtocol), reply("git-a", codeStatusProtocol),
				reply("git-a", codeStatusProtocol+1)},
			statuses: []string{"git-a"},
			nodes:    []eventfan.NodeCoverage{{ID: "git-a", Answered: true}},
			complete: true},
		{name: "a reply at another version is named, not read", asked: []string{"git-a"},
			replies: [][]byte{reply("git-a", codeStatusProtocol+1)},
			nodes: []eventfan.NodeCoverage{{ID: "git-a", Error: fmt.Sprintf(
				"answered at status protocol %d, and this build reads %d", codeStatusProtocol+1,
				codeStatusProtocol)}}},
		{name: "a reply this build cannot read names nobody", asked: []string{"git-a"},
			replies: [][]byte{[]byte("not a status"), reply("", codeStatusProtocol)},
			nodes:   []eventfan.NodeCoverage{{ID: "git-a", Error: silent}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			report := codeStatusReport(c.asked, c.replies)
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

// A CODE STATUS ASK WAITS FOR EVERY GITLET NODE IT ASKED, BY NAME. A node not
// asked can answer too (an older build answers whatever a question names; a
// draining node answers a question naming nobody until its host stops), and a
// scatter's replies carry no sender. Here git-d, not asked, answers first, and
// git-b only once git-d's reply has reached the asker: the ask still waits for
// git-b, and the report is complete, with git-b's status and not git-d's.
//
// Mutation: end the ask at as many replies as it asked nodes — git-d's answer
// fills it, and git-b is named silent, red.
func TestACodeStatusAskWaitsForEveryGitletNodeItAsked(t *testing.T) {
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
	asked := []string{"git-b"}
	replies, err := askCodeStatus(ctx, askEach, []byte(`{}`), asked)
	if err != nil {
		t.Fatalf("ask the Gitlet nodes: %v", err)
	}
	report := codeStatusReport(asked, replies)
	want := []eventfan.NodeCoverage{{ID: "git-b", Answered: true}}
	if !report.Coverage.Complete || !slices.Equal(report.Coverage.Nodes, want) ||
		len(report.Statuses) != 1 || report.Statuses[0].Node != "git-b" {
		t.Fatalf("coverage %+v with statuses %+v, want complete with %+v and git-b's status alone: git-b "+
			"answered, after git-d", report.Coverage, report.Statuses, want)
	}
}

// A GITLET NODE ANSWERS ONLY A CODE STATUS QUESTION THAT NAMES IT, OR NAMES
// NOBODY, and carries its hot list only where asked: a page of a few nodes is
// answered by those few, none sending a hot list the page would discard. A
// question naming other nodes gets nothing, before the status is read. The
// zero question (no nodes, no without_hot), which older builds ask, is answered
// whole, hot list and all.
//
// Mutation: answer a question that names other nodes, or read the hot list
// whatever without_hot says, and a case is red.
func TestAGitletNodeAnswersOnlyACodeStatusQuestionThatNamesIt(t *testing.T) {
	t.Parallel()
	var read []host.StatusAsk
	answer := answerCodeStatus("git-a", func(a host.StatusAsk) host.CodeStatus {
		read = append(read, a)
		s := host.CodeStatus{Node: "git-a"}
		if a.Hot {
			s.Hot = []host.RepoStatus{{RepoHeat: cache.RepoHeat{ID: "r1"}}}
		}
		return s
	})
	for _, c := range []struct {
		name     string
		question string
		answered bool
		read     host.StatusAsk
	}{
		{name: "a question naming nobody", question: `{"version":1}`, answered: true,
			read: host.StatusAsk{Hot: true}},
		{name: "a question naming the node", question: `{"version":1,"repo":"r1","nodes":["git-b","git-a"]}`,
			answered: true, read: host.StatusAsk{Repo: "r1", Hot: true}},
		{name: "a question asking no hot list", question: `{"version":1,"nodes":["git-a"],"without_hot":true}`,
			answered: true},
		{name: "a question naming other nodes", question: `{"version":1,"nodes":["git-b","git-c"]}`},
		{name: "a question at another version", question: `{"version":2,"nodes":["git-a"]}`},
	} {
		read = nil
		raw, err := answer(t.Context(), []byte(c.question))
		if !c.answered {
			if err == nil || raw != nil || len(read) != 0 {
				t.Errorf("%s: answered %s (%v) after reading %+v, want nothing and no read", c.name, raw, err, read)
			}
			continue
		}
		var a codeStatusAnswer
		if err != nil || json.Unmarshal(raw, &a) != nil {
			t.Errorf("%s: answered %s (%v), want git-a's status", c.name, raw, err)
			continue
		}
		if !slices.Equal(read, []host.StatusAsk{c.read}) || a.Version != codeStatusProtocol ||
			a.Status.Node != "git-a" || (a.Status.Hot != nil) != c.read.Hot {
			t.Errorf("%s: read the status as %+v and answered %s, want it read once as %+v and answered "+
				"with a hot list exactly where one was asked", c.name, read, raw, c.read)
		}
	}
}

// A PAGE OF CODE STATUS ASKS ITS OWN GITLET NODES AND WAITS FOR THOSE ALONE:
// Engine.CodeStatus names the page's nodes in its question, so a node off the
// page reads no status and sends none, and the ask ends once the page's nodes
// have answered, never held up by a node off the page. A page asks for no hot
// list it does not carry; an ask of every Gitlet node names none, and each
// answers with its hot list.
//
// Mutation: leave the page's nodes out of the question — git-c reads its
// status — or wait out the budget rather than end at the page's last answer —
// the page takes the whole budget — and a case is red.
func TestAPageOfCodeStatusAsksAndWaitsForItsOwnGitletNodesAlone(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	start := func() *memory.Queue {
		q := broker.Client()
		if err := q.Start(t.Context()); err != nil {
			t.Fatalf("start a node's queue: %v", err)
		}
		t.Cleanup(func() { _ = q.Stop(context.Background()) })
		return q
	}
	serve := func(answer queue.AnswerFunc) {
		stop, err := start().Serve(t.Context(), topics.CodeStatus, queue.ConcurrentAnswers, answer)
		if err != nil {
			t.Fatalf("serve a node's status: %v", err)
		}
		t.Cleanup(func() { _ = stop(context.Background()) })
	}
	var mu sync.Mutex
	reads := map[string][]host.StatusAsk{}
	for _, node := range []string{"git-a", "git-b", "git-c"} {
		serve(answerCodeStatus(node, func(a host.StatusAsk) host.CodeStatus {
			mu.Lock()
			defer mu.Unlock()
			reads[node] = append(reads[node], a)
			s := host.CodeStatus{Node: node}
			if a.Hot {
				s.Hot = []host.RepoStatus{{RepoHeat: cache.RepoHeat{ID: "r1"}}}
			}
			return s
		}))
	}
	// git-z NEVER ANSWERS, whatever it is asked: a stuck node of the fleet,
	// off every page here.
	serve(func(ctx context.Context, _ []byte) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	e := &Engine{backends: &Backends{Queue: start()}}

	ask := func(what string, a builtin.CodeStatusAsk) builtin.CodeStatusReport {
		t.Helper()
		began := time.Now()
		report, err := e.CodeStatus(t.Context(), a)
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if took := time.Since(began); took >= codeStatusBudget {
			t.Errorf("%s took %s, its whole budget: it waited on a node it did not ask", what, took)
		}
		var want []eventfan.NodeCoverage
		for _, id := range a.Nodes {
			want = append(want, eventfan.NodeCoverage{ID: id, Answered: true})
		}
		if !report.Coverage.Complete || !slices.Equal(report.Coverage.Nodes, want) {
			t.Errorf("%s covered %+v, want complete with %+v", what, report.Coverage, want)
		}
		for _, s := range report.Statuses {
			if (s.Hot != nil) != a.Hot {
				t.Errorf("%s: %s answered hot list %v, want one exactly where it was asked", what, s.Node, s.Hot)
			}
		}
		return report
	}
	ask("a page of git-a and git-b", builtin.CodeStatusAsk{Nodes: []string{"git-a", "git-b"}, Repo: "r1"})
	mu.Lock()
	page := map[string][]host.StatusAsk{"git-a": {{Repo: "r1"}}, "git-b": {{Repo: "r1"}}}
	if !maps.EqualFunc(reads, page, slices.Equal) {
		t.Errorf("the page read the Gitlet nodes' statuses as %+v, want git-a's and git-b's alone, without a "+
			"hot list: %+v", reads, page)
	}
	clear(reads)
	mu.Unlock()

	ask("every Gitlet node", builtin.CodeStatusAsk{Nodes: []string{"git-a", "git-b", "git-c"}, Every: true, Hot: true})
	mu.Lock()
	defer mu.Unlock()
	every := map[string][]host.StatusAsk{"git-a": {{Hot: true}}, "git-b": {{Hot: true}}, "git-c": {{Hot: true}}}
	if !maps.EqualFunc(reads, every, slices.Equal) {
		t.Errorf("an ask of every Gitlet node read their statuses as %+v, want each once with its hot list: %+v",
			reads, every)
	}
}

// THE HEALTH TOOLS ARE OFFERED WHERE THEIR READS ARE: fleet_health wherever
// the node reads the fleet's presence leases (every node of a running fleet),
// with the requester ceiling's halves beside them; code_status on the code
// tools' terms, held by a node serving Gitlet and not by one of a company
// hosting no code. Through the running wiring: the seat's registry is equipped
// at the apply and the operator's halves read the same deps. No engine, or one
// that runs nothing, offers neither.
//
// Mutation: HealthToolDeps handing Status where the code tools are not
// offered, the leases or a code half without the requester ceiling's halves,
// or equip dropping deps.Health, and a case goes red.
func TestTheHealthToolsAreOfferedWhereTheirReadsAre(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, doc  string
		codeStatus bool
	}{
		{"hosting code", gitletHostCompany, true},
		{"hosting none", nativeCleanupCompany, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newSandboxNode(t, parseCompany(t, c.doc))
			deps := HealthToolDeps(e)
			if deps.Leases == nil || deps.Self != e.id {
				t.Fatalf("a running node's health deps read no presence leases: %+v", deps)
			}
			// fleet_health READS FOR WHOEVER WOKE A TURN only where they may
			// act in the company: the requester ceiling's halves ride beside
			// the leases wherever it is offered.
			if deps.Requesters == nil || deps.Chart == nil {
				t.Errorf("fleet_health's deps wire no requester ceiling: Requesters %v, Chart %v",
					deps.Requesters, deps.Chart)
			}
			if got := deps.Status != nil && deps.Code.Code != nil; got != c.codeStatus {
				t.Errorf("code_status's deps present = %v, want %v", got, c.codeStatus)
			}
			// ITS REPOSITORY READS ARE THE CODE READS', made for whoever woke
			// the turn: the requester ceiling's halves ride beside
			// Gitlet wherever code_status is offered.
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
