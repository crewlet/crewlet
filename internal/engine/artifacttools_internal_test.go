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

// A ROW THAT IS NOT THIS TURN'S, OR A JOB STILL WORKING, IS NO BOX: a turn
// reads its own run's finished build and nothing else.
func TestOnlyATurnsOwnFinishedRunIsABox(t *testing.T) {
	t.Parallel()
	turn := turnFor(t, "analyst")
	e := &Engine{}
	b := artifactBoxes{engine: e}
	if _, err := b.Box(t.Context(), turn); !errors.Is(err, builtin.ErrNoBox) {
		t.Fatalf("a node with no sandbox answered %v, want ErrNoBox", err)
	}
	for name, row := range map[string]sandbox.PendingRun{
		"no box":         {TurnID: turn.RunID, AgentHandle: "analyst", Status: sandbox.StatusResumed},
		"another seat":   {TurnID: turn.RunID, AgentHandle: "jane", SandboxID: "b", Status: sandbox.StatusResumed},
		"a running job":  {TurnID: turn.RunID, AgentHandle: "analyst", SandboxID: "b", Status: sandbox.StatusRunning},
		"a parked run":   {TurnID: turn.RunID, AgentHandle: "analyst", SandboxID: "b", Status: sandbox.StatusAwaiting},
		"a reaped pause": {TurnID: turn.RunID, AgentHandle: "analyst", SandboxID: "b", Status: sandbox.StatusReseed},
	} {
		e := &Engine{}
		e.sandbox.Store(&sandboxRuntime{pending: oneRow{row: row}})
		if _, err := (artifactBoxes{engine: e}).Box(t.Context(), turn); !errors.Is(err, builtin.ErrNoBox) {
			t.Errorf("%s answered %v, want ErrNoBox", name, err)
		}
	}
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
