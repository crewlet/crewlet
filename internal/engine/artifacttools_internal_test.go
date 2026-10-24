package engine

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
			return sandbox.ExecResult{ExitCode: checkNotARegularFile}, nil
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
	want := "cd '/home/user/workspace' 2>/dev/null || exit 4; [ -d './dist' ] && " +
		"[ ! -L './dist' ] || exit 3; exec 3>&1; st=$( { { find './dist' -type f -print0; "
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

// A LISTING IS TOLD BY ITS EXIT, through a real shell, under both shells a box
// runs commands in (a local box's sh and an E2B box's bash): a dir the
// workspace holds lists its files, one it does not hold as a directory
// (missing, a file, a link) is the caller's to name again
// ([builtin.ErrNotADirectory]), and a workspace that is gone is said as that
// ([builtin.ErrNoWorkspace]). A missing dir listed as empty, because find's
// failure was lost in a pipeline whose status is head's, and a missing
// workspace was told as a dir the caller named wrong.
//
// FIND'S OWN EXIT IS READ: a walk that stops partway is
// [builtin.ErrListIncomplete] and never the part it printed, unless head cut
// the output first; a box with no find is its image. And a dir whose name is a
// find expression is a start point: `-delete` deleted the whole workspace.
//
// Mutation: drop the directory check, the workspace's own exit, the `./` before
// the start point or the carried find status, or read find's failure under a
// cut as a failure, and a row goes red.
func TestABoxListingIsToldByItsExitThroughAShell(t *testing.T) {
	t.Parallel()
	shells := []string{"sh"}
	if _, err := exec.LookPath("bash"); err == nil {
		shells = append(shells, "bash")
	}
	for _, shell := range shells {
		t.Run(shell, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			for name, body := range map[string]string{
				"dist/index.html": "<p>hi</p>", "dist/js/app.js": "x", "notes.txt": "n",
				"-delete/out.html": "o",
			} {
				full := filepath.Join(root, "workspace", name)
				if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(filepath.Join(root, "workspace", "dist"),
				filepath.Join(root, "workspace", "linked")); err != nil {
				t.Fatal(err)
			}
			// PATH is what a row swaps to stand a box's own tools in.
			path := os.Getenv("PATH")
			box := sandbox.NewFakeSandbox("box-1")
			box.ExecFunc = func(ctx context.Context, cmd string, _ sandbox.ExecOptions) (sandbox.ExecResult, error) {
				var stdout, stderr strings.Builder
				run := exec.CommandContext(ctx, shell, "-c", cmd)
				run.Env = append(os.Environ(), "PATH="+path)
				run.Stdout, run.Stderr = &stdout, &stderr
				err := run.Run()
				var exit *exec.ExitError
				switch {
				case errors.As(err, &exit):
					return sandbox.ExecResult{ExitCode: exit.ExitCode(), Stdout: stdout.String(),
						Stderr: stderr.String()}, nil
				case err != nil:
					return sandbox.ExecResult{}, err
				}
				return sandbox.ExecResult{Stdout: stdout.String(), Stderr: stderr.String()}, nil
			}
			b := &runBox{box: box, root: filepath.Join(root, "workspace"), listed: map[string]bool{}}
			got, err := b.Files(t.Context(), "dist", 10)
			slices.Sort(got)
			if err != nil || !slices.Equal(got, []string{"dist/index.html", "dist/js/app.js"}) {
				t.Fatalf("a dir the workspace holds listed %q, %v", got, err)
			}
			whole, err := b.Files(t.Context(), ".", 10)
			if err != nil || !slices.Contains(whole, "notes.txt") || !slices.Contains(whole, "dist/index.html") {
				t.Errorf("the workspace itself listed %q, %v", whole, err)
			}
			for _, dir := range []string{"missing", "notes.txt", "linked"} {
				if got, err := b.Files(t.Context(), dir, 10); !errors.Is(err, builtin.ErrNotADirectory) {
					t.Errorf("%s listed %q, %v; want ErrNotADirectory", dir, got, err)
				}
			}
			gone := &runBox{box: box, root: filepath.Join(root, "no-workspace"), listed: map[string]bool{}}
			if got, err := gone.Files(t.Context(), "dist", 10); !errors.Is(err, builtin.ErrNoWorkspace) ||
				errors.Is(err, builtin.ErrNotADirectory) {
				t.Errorf("a workspace that is gone listed %q, %v; want ErrNoWorkspace", got, err)
			}

			// A DIR NAMED LIKE A FIND EXPRESSION is listed, and nothing
			// beside it is touched.
			got, err = b.Files(t.Context(), "-delete", 10)
			if err != nil || !slices.Equal(got, []string{"-delete/out.html"}) {
				t.Errorf("a dir named -delete listed %q, %v", got, err)
			}
			for _, kept := range []string{"dist/index.html", "notes.txt", "-delete/out.html"} {
				if _, err := os.Stat(filepath.Join(root, "workspace", kept)); err != nil {
					t.Errorf("listing a dir named -delete removed %s: %v", kept, err)
				}
			}

			// THE BOX'S OWN TOOLS, stood in on PATH: a find that stops
			// partway, one that stops past what head keeps, and none at all.
			tools := t.TempDir()
			head, err := exec.LookPath("head")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(head, filepath.Join(tools, "head")); err != nil {
				t.Fatal(err)
			}
			path = tools
			if got, err := b.Files(t.Context(), "dist", 10); sandboxWhy(err) != sandbox.WhyConfiguration {
				t.Errorf("a box with no find listed %q, %v; want its image named", got, err)
			}
			writeTool(t, tools, "find", `printf './dist/index.html\0'; `+
				`echo "find: './dist/assets': Permission denied" >&2; exit 1`)
			if got, err := b.Files(t.Context(), "dist", 10); !errors.Is(err, builtin.ErrListIncomplete) {
				t.Errorf("a walk that stopped partway listed %q, %v; want ErrListIncomplete", got, err)
			}
			writeTool(t, tools, "find", `head -c 4000 /dev/zero; exit 1`)
			if got, err := b.Files(t.Context(), "dist", 1); err != nil || len(got) != 2 {
				t.Errorf("a walk head cut listed %d entries, %v; want one too many", len(got), err)
			}
			path = os.Getenv("PATH")
		})
	}
}

// sandboxWhy is err's class, or none.
func sandboxWhy(err error) sandbox.Why {
	why, _ := sandbox.WhyOf(err)
	return why
}

// writeTool writes an executable shell script named name into dir.
func writeTool(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// ANY OTHER EXIT IS THE BOX'S, classed by what clears it: a box that cannot run
// the shell tools every box has is its image, a setup somebody changes, and
// one that timed the command out, killed it or answered an exit the command
// never gives is the box failing on its side. A file check the box timed out
// is never a path the caller named wrong, and neither is a 1: test's own
// false, but also what a container runtime's CLI exits with when it fails
// itself. Unclassed, each was told as a fault of this node.
//
// Mutation: answer every exit as configuration, read a timed-out check or a 1
// as a path that names no file, or drop the class, and a row goes red.
func TestABoxCommandsOtherExitsAreTheBoxs(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		exit int
		why  sandbox.Why
	}{
		{1, sandbox.WhyProviderUnavailable},
		{124, sandbox.WhyProviderUnavailable},
		{137, sandbox.WhyProviderUnavailable},
		{2, sandbox.WhyProviderUnavailable},
		{126, sandbox.WhyConfiguration},
		{127, sandbox.WhyConfiguration},
	} {
		box := sandbox.NewFakeSandbox("box-1")
		box.ExecFunc = func(context.Context, string, sandbox.ExecOptions) (sandbox.ExecResult, error) {
			return sandbox.ExecResult{ExitCode: c.exit, Stderr: "timed out after 60s"}, nil
		}
		b := &runBox{box: box, root: "/home/user/workspace", listed: map[string]bool{}}
		_, listErr := b.Files(t.Context(), "dist", 3)
		_, openErr := b.Open(t.Context(), "dist/unlisted.js")
		for what, err := range map[string]error{"a listing": listErr, "a file check": openErr} {
			why, classed := sandbox.WhyOf(err)
			switch {
			case !classed || why != c.why:
				t.Errorf("%s that exited %d is classed %q (%v); want %q", what, c.exit, why,
					classed, c.why)
			case errors.Is(err, builtin.ErrNotADirectory), errors.Is(err, sandbox.ErrNotRegularFile):
				t.Errorf("%s that exited %d was told as the caller's path: %v", what, c.exit, err)
			}
		}
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
