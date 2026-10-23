package engine

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
	queuemem "github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// THE BOX IS LISTED THROUGH THE BOX, quoted, and bounded: the listing command
// runs under the workspace with every path a single shell word, its output is
// cut at what the entries asked for can weigh, and a cut answer reads as one
// entry too many rather than a short list.
func TestTheBoxIsListedThroughItselfAndBounded(t *testing.T) {
	t.Parallel()
	box := sandbox.NewFakeSandbox("box-1")
	var ran []string
	stdout := "dist/index.html\x00dist/a b/it's.js\x00"
	box.ExecFunc = func(_ context.Context, cmd string, _ sandbox.ExecOptions) (sandbox.ExecResult, error) {
		ran = append(ran, cmd)
		if strings.HasPrefix(cmd, "test -f") {
			return sandbox.ExecResult{ExitCode: 1}, nil
		}
		return sandbox.ExecResult{Stdout: stdout}, nil
	}
	b := &runBox{box: box, root: "/home/user/workspace", listed: map[string]bool{}}

	got, err := b.Files(t.Context(), "dist", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1] != "dist/a b/it's.js" {
		t.Fatalf("listed %q", got)
	}
	want := "cd '/home/user/workspace' && find 'dist' -type f -print0 | head -c "
	if !strings.HasPrefix(ran[0], want) {
		t.Errorf("ran %q, want it to start %q", ran[0], want)
	}

	// A LISTED FILE IS STREAMED; ONE THE LISTING DID NOT NAME is held to being
	// a regular file first, because a box streams a missing path as empty.
	box.Put("/home/user/workspace/dist/index.html", "<p>hi</p>")
	rc, err := b.Open(t.Context(), "dist/index.html")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(rc)
	if string(data) != "<p>hi</p>" {
		t.Errorf("streamed %q", data)
	}
	// AND IT IS REFUSED AS THE CALLER'S PATH, which the tool tells its
	// model to change, never as a fault of this node.
	if _, err := b.Open(t.Context(), "dist/missing.js"); !errors.Is(err, sandbox.ErrNotRegularFile) {
		t.Fatalf("a path that is not a regular file answered %v, want ErrNotRegularFile", err)
	}
	if last := ran[len(ran)-1]; !strings.Contains(last, "test ! -L") {
		t.Errorf("an unlisted path was opened without the check: %q", last)
	}

	// A CUT LISTING is one entry more than asked for, which the tool refuses.
	stdout = strings.Repeat("x", 1*listEntryBytes)
	got, err = b.Files(t.Context(), "dist", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("a cut listing answered %d entries, want 2", len(got))
	}
}

// ONLY THE RUN WORKING IN ITS BOX READS IT. A seat's executor is its CLI's own
// run in the box, and every call naming a box source is that run's, made while
// it works: a row that is another seat's, has no box yet, is launching, parked
// on a question, collected or reaped is no box. And the run's own box, read
// while it works, is the one a publish reads.
//
// Mutation: admit the collected record again (StatusResumed), and the
// collected case reads a box; refuse the running record, and the run's own
// build is refused, which is what left every executor's `from_sandbox` dead.
func TestOnlyTheRunWorkingInItsBoxIsABox(t *testing.T) {
	t.Parallel()
	turn := turnFor(t, "analyst")
	e := &Engine{}
	b := artifactBoxes{engine: e}
	if _, err := b.Box(t.Context(), turn); !errors.Is(err, builtin.ErrNoBox) {
		t.Fatalf("a node with no sandbox answered %v, want ErrNoBox", err)
	}
	provider, manager := artifactBoxManager(t)
	box, err := provider.Create(t.Context(), sandbox.Spec{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		row  sandbox.PendingRun
		box  bool
	}{
		{"no box", sandbox.PendingRun{AgentHandle: "analyst", Status: sandbox.StatusRunning}, false},
		{"another seat", sandbox.PendingRun{AgentHandle: "jane", SandboxID: box.ID(), Status: sandbox.StatusRunning}, false},
		{"a launching job", sandbox.PendingRun{AgentHandle: "analyst", SandboxID: box.ID(), Status: sandbox.StatusLaunching}, false},
		{"a parked run", sandbox.PendingRun{AgentHandle: "analyst", SandboxID: box.ID(), Status: sandbox.StatusAwaiting}, false},
		{"a collected run", sandbox.PendingRun{AgentHandle: "analyst", SandboxID: box.ID(), Status: sandbox.StatusResumed}, false},
		{"a reaped pause", sandbox.PendingRun{AgentHandle: "analyst", SandboxID: box.ID(), Status: sandbox.StatusReseed}, false},
		{"the run working in it", sandbox.PendingRun{AgentHandle: "analyst", SandboxID: box.ID(), Status: sandbox.StatusRunning}, true},
	} {
		row := tc.row
		row.TurnID, row.Placement, row.CodingAgent = turn.RunID, string(sandbox.E2B), "claude-code"
		e := artifactBoxEngine(t, "node-a", row, manager)
		got, err := (artifactBoxes{engine: e}).Box(t.Context(), turn)
		switch {
		case tc.box && (err != nil || got == nil):
			t.Errorf("%s: %v; want its box", tc.name, err)
		case !tc.box && !errors.Is(err, builtin.ErrNoBox):
			t.Errorf("%s answered %v, want ErrNoBox", tc.name, err)
		}
	}
}

// A RUNNING RECORD'S BOX IS READ, NEVER WOKEN: the job is working in it, and a
// reconnect resumes a box and moves a remote one's kill timer to its own. A box
// paused under a running record — a collection that has not moved the record
// on yet — is no box, and is left paused.
//
// Mutation: read the box through Reconnect again, and the paused box is woken
// and handed to the publish.
func TestARunningRecordsBoxIsAttachedNeverWoken(t *testing.T) {
	t.Parallel()
	turn := turnFor(t, "analyst")
	provider, manager := artifactBoxManager(t)
	created, err := provider.Create(t.Context(), sandbox.Spec{})
	if err != nil {
		t.Fatal(err)
	}
	box := created.(*sandbox.FakeSandbox)
	if err := box.Pause(t.Context()); err != nil {
		t.Fatal(err)
	}
	row := sandbox.PendingRun{TurnID: turn.RunID, AgentHandle: "analyst", SandboxID: box.ID(),
		Placement: string(sandbox.E2B), CodingAgent: "claude-code", Status: sandbox.StatusRunning}
	e := artifactBoxEngine(t, "node-a", row, manager)
	if _, err := (artifactBoxes{engine: e}).Box(t.Context(), turn); !errors.Is(err, builtin.ErrNoBox) {
		t.Errorf("a paused box under a running record answered %v, want ErrNoBox", err)
	}
	if !box.Paused() {
		t.Error("reading the run's box woke it")
	}
}

// artifactBoxManager is a manager over one fake provider, in the e2b cell and
// the direct one.
func artifactBoxManager(t *testing.T) (*sandbox.FakeProvider, *sandbox.Manager) {
	t.Helper()
	provider := sandbox.NewFakeProvider()
	manager, err := sandbox.NewManager(sandbox.ManagerOptions{
		Providers: map[sandbox.Placement]sandbox.Provider{sandbox.Direct: provider, sandbox.E2B: provider},
		Runners:   map[string]sandbox.Runner{"claude-code": sandbox.NewFakeRunner("claude-code")},
	})
	if err != nil {
		t.Fatal(err)
	}
	return provider, manager
}

// artifactBoxEngine is node's engine holding row on a coordinator over
// manager, stopped with the test.
func artifactBoxEngine(t *testing.T, node string, row sandbox.PendingRun, manager *sandbox.Manager) *Engine {
	t.Helper()
	coordinator, err := sandbox.NewCoordinator(sandbox.CoordinatorOptions{
		Answers: testAnswers(),
		Queue:   queuemem.NewBroker().Client(), Pending: oneRow{row: row}, Manager: manager,
		Resume: nilResumer{}, Audience: nobodyAudience{}, Node: node,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(coordinator.Stop)
	e := &Engine{id: node}
	e.sandbox.Store(&sandboxRuntime{pending: oneRow{row: row}, coordinator: coordinator})
	return e
}

// oneRow is a pending store holding one run's row.
type oneRow struct {
	sandbox.PendingStore
	row sandbox.PendingRun
}

func (s oneRow) Get(_ context.Context, turnID string) (sandbox.PendingRun, bool, error) {
	return s.row, s.row.TurnID == turnID, nil
}

// turnFor is the analyst's turn in a one-seat company.
func turnFor(t *testing.T, handle string) *turnctx.Turn {
	t.Helper()
	o := &org.Organization{Name: "Acme", Roles: []*org.Role{{Name: "Analyst", DeclaredHandle: handle}}}
	o.Normalize()
	return &turnctx.Turn{RunID: "run-1", Seat: o.Roles[0], Org: o}
}

// WHO A TURN IS ACTING FOR is the seat the chart holds: an agent seat as the
// seat principal, never admin, and a seat the chart does not hold is unknown
// rather than a guess.
func TestARequesterIsComposedFromTheRunningChart(t *testing.T) {
	t.Parallel()
	o := &org.Organization{Name: "Acme", Roles: []*org.Role{
		{Name: "Analyst", DeclaredHandle: "analyst"},
		{Name: "Jane", DeclaredHandle: "jane", Kind: org.KindHuman},
	}}
	o.Normalize()
	e := &Engine{}
	e.epoch.current.Store(&Company{Org: o})
	r := artifactRequesters{engine: e}

	p, err := r.Requester(t.Context(), "analyst")
	if err != nil {
		t.Fatal(err)
	}
	if p.Kind != iam.KindSeat || p.Seat != "analyst" || p.Can(iam.GrantAdmin) {
		t.Errorf("an agent requester composed as %+v", p)
	}
	if _, err := r.Requester(t.Context(), "nobody"); err == nil {
		t.Error("a seat the chart does not hold composed a principal")
	}
	// A PERSON'S SEAT needs the identity directory, and a node that reaches
	// none cannot say who holds it.
	if _, err := r.Requester(t.Context(), "jane"); err == nil {
		t.Error("a person's seat composed with no identity directory")
	}
}

// THE LINK IS THE DASHBOARD'S ADDRESS on api.external_url, and nothing where
// the deployment names none.
func TestAnArtifactsLinkIsItsDashboardAddress(t *testing.T) {
	t.Parallel()
	id := artifacts.IDFor("op")
	e := &Engine{externalBase: "https://crewlet.example.com"}
	if got, want := e.artifactLink(id), "https://crewlet.example.com/dashboard#/artifacts/"+id.String(); got != want {
		t.Errorf("link = %q, want %q", got, want)
	}
	if got := (&Engine{}).artifactLink(id); got != "" {
		t.Errorf("a deployment with no external url linked %q", got)
	}
	if deps := ArtifactToolDeps(&Engine{}); deps.Reader != nil {
		t.Error("an engine with no router handed out artifact tools")
	}
}

// A RUN WHOSE BOX IS ON ANOTHER NODE'S HOST IS NO BOX HERE: the seat moved since
// the run launched, and a box in the direct cell is a process tree on the node
// that launched it, which only that node touches. The same run's box on this
// node's own host is read.
//
// Mutation: drop the host check, and the turn reads a box this node cannot
// reach — here, the double's, which every node of a test shares.
func TestARunsBoxOnAnotherNodesHostIsNoBoxHere(t *testing.T) {
	t.Parallel()
	turn := turnFor(t, "analyst")
	provider, manager := artifactBoxManager(t)
	box, err := provider.Create(t.Context(), sandbox.Spec{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		launchedBy string
		box        bool
	}{{"node-a", false}, {"node-b", true}} {
		row := sandbox.PendingRun{TurnID: turn.RunID, AgentHandle: "analyst", SandboxID: box.ID(),
			Placement: string(sandbox.Direct), CodingAgent: "claude-code",
			Status: sandbox.StatusRunning, LaunchNode: tc.launchedBy}
		e := artifactBoxEngine(t, "node-b", row, manager)
		got, err := (artifactBoxes{engine: e}).Box(t.Context(), turn)
		switch {
		case tc.box && (err != nil || got == nil):
			t.Errorf("the run launched by %s, on this node's host: %v; want its box", tc.launchedBy, err)
		case !tc.box && !errors.Is(err, builtin.ErrNoBox):
			t.Errorf("the run launched by %s, on another host: %v; want ErrNoBox", tc.launchedBy, err)
		}
	}
}
