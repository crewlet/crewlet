package sandbox

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
)

// specsSeen is a FakeProvider keeping every spec a box was made from: what the
// cases about what reaches a backend read, since a fake box keeps none.
type specsSeen struct {
	*FakeProvider
	mu    sync.Mutex
	specs []Spec
}

func newSpecsSeen() *specsSeen { return &specsSeen{FakeProvider: NewFakeProvider()} }

func (p *specsSeen) Create(ctx context.Context, spec Spec) (Sandbox, error) {
	p.mu.Lock()
	p.specs = append(p.specs, spec)
	p.mu.Unlock()
	return p.FakeProvider.Create(ctx, spec)
}

// seen is every spec a box was made from, in order.
func (p *specsSeen) seen() []Spec {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.specs)
}

// boxManager is a manager over the two cells, each its own backend, with the
// direct cell as the catalogue's default and one operator setup step: a default
// no check may take and steps no check may run.
func boxManager(t *testing.T, cells map[Placement]Provider) (*Manager, *FakeRunner) {
	t.Helper()
	runner := NewFakeRunner("claude-code")
	providers := map[Placement]Provider{Direct: NewFakeProvider(), Container: NewFakeProvider()}
	maps.Copy(providers, cells)
	m, err := NewManager(ManagerOptions{
		Providers:        providers,
		DefaultPlacement: Direct,
		Runners:          map[string]Runner{"claude-code": runner},
		DefaultSetup:     []SetupStep{{Name: "operator", Commands: []string{"install the operator's tools"}}},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return m, runner
}

// A BOX FOR A CHECK INSTALLS NO AGENT. AcquireBox makes the box in its spec's
// cell from the current catalogue and applies exactly the steps it is handed,
// with the spec's environment: no runner's install and none of the catalogue's
// setup, which provisions coding boxes while a check runs a change's own code.
// A failing step tears the box down, as Acquire's does. What a coding box
// carries and this one must not (a harness, a login, a run's grants or turn,
// the catalogue's default cell, the direct cell), or a box naming no attempt
// its runner could reclaim it by, is refused before any backend is asked.
//
// Mutations, each red: AcquireBox calling Acquire (the agent is installed, or a
// spec naming none refused); prepending DefaultSetup (the operator's step
// runs); dropping the direct or the attempt's refusal (a box is made anyway).
func TestABoxForACheckInstallsNoAgent(t *testing.T) {
	t.Parallel()
	steps := []SetupStep{GitVersionStep(), {Name: "checkout web", Commands: []string{"echo checked out"}}}
	env := map[string]string{"CREWLET_CHECK_NAME": "unit", "NPM_TOKEN": "a check's own secret"}

	t.Run("made", func(t *testing.T) {
		t.Parallel()
		container := newSpecsSeen()
		m, runner := boxManager(t, map[Placement]Provider{Container: container})
		box, err := m.AcquireBox(t.Context(), Spec{Placement: Container, Check: "attempt-1", Env: env}, steps)
		if err != nil {
			t.Fatalf("AcquireBox: %v", err)
		}
		if runner.Installed(box.ID()) {
			t.Error("a box for a check had the harness installed")
		}
		var ran []string
		for _, exec := range container.Box(box.ID()).Execs() {
			ran = append(ran, exec.Command)
			if !maps.Equal(exec.Env, env) {
				t.Errorf("%q ran with %v, want the spec's environment %v", firstLine(exec.Command), exec.Env, env)
			}
		}
		if want := slices.Concat(steps[0].Commands, steps[1].Commands); !slices.Equal(ran, want) {
			t.Errorf("the box ran %q, want exactly the steps it was handed %q", ran, want)
		}
		if container.Box(box.ID()).Closed() {
			t.Error("a box that provisioned whole was torn down")
		}
		want := []CheckBox{{SandboxID: box.ID(), Check: "attempt-1", Placement: Container}}
		if listed, err := m.CheckBoxes(); err != nil || !slices.Equal(listed, want) {
			t.Errorf("the node lists its check boxes as %+v (%v), want %+v: the box records its attempt",
				listed, err, want)
		}
		if specs := container.seen(); len(specs) != 1 || specs[0].Harness != "" ||
			len(specs[0].CredentialFiles) != 0 {
			t.Errorf("the backend was asked for %+v, want one box naming no agent and seeded no login", specs)
		}
	})

	t.Run("torn down", func(t *testing.T) {
		t.Parallel()
		failing := &scriptedBoxes{FakeProvider: NewFakeProvider(), exec: func(cmd string) ExecResult {
			if cmd == "echo checked out" {
				return ExecResult{ExitCode: 128, Stderr: "fatal: reference is not a tree"}
			}
			return ExecResult{}
		}}
		m, _ := boxManager(t, map[Placement]Provider{Container: failing})
		box, err := m.AcquireBox(t.Context(), Spec{Placement: Container, Check: "attempt-1"}, steps)
		var setup *SetupError
		if box != nil || !errors.As(err, &setup) || setup.Step != "checkout web" || setup.Exit != 128 {
			t.Fatalf("AcquireBox = %v, %v; want the checkout step's failure and no box", box, err)
		}
		if made := failing.Box("box-1"); made == nil || !made.Closed() {
			t.Error("the box whose step failed was not torn down")
		}
	})

	for name, tc := range map[string]struct {
		spec Spec
		says string
	}{
		"a harness":             {Spec{Placement: Container, Check: "attempt-1", Harness: "claude-code"}, `names "claude-code"`},
		"a login":               {Spec{Placement: Container, Check: "attempt-1", CredentialFiles: map[string]string{".claude/.credentials.json": "/srv/login"}}, "credential files"},
		"a run's grants":        {Spec{Placement: Container, Check: "attempt-1", Repos: []RunRepo{{Repo: "web"}}}, "grants"},
		"a run's turn":          {Spec{Placement: Container, Check: "attempt-1", TurnID: "t1"}, "serves no turn"},
		"no attempt":            {Spec{Placement: Container}, "names the check attempt"},
		"the catalogue default": {Spec{Check: "attempt-1", Env: env}, "names its cell"},
		"the direct cell":       {Spec{Placement: Direct, Check: "attempt-1"}, "engine's own user"},
	} {
		t.Run("refused: "+name, func(t *testing.T) {
			t.Parallel()
			cells := map[Placement]Provider{Direct: newSpecsSeen(), Container: newSpecsSeen()}
			m, runner := boxManager(t, cells)
			box, err := m.AcquireBox(t.Context(), tc.spec, steps)
			if box != nil || err == nil || !strings.Contains(err.Error(), tc.says) {
				t.Fatalf("AcquireBox = %v, %v; want it refused saying %q", box, err, tc.says)
			}
			for cell, provider := range cells {
				if specs := provider.(*specsSeen).seen(); len(specs) != 0 {
					t.Errorf("the %s backend was asked for a box: %+v", cell, specs)
				}
			}
			if len(runner.Started()) != 0 {
				t.Error("a refused box started a run")
			}
		})
	}
}
