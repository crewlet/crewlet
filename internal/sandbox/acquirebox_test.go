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

// boxManager is a manager over the three cells, each its own backend, the
// catalogue's default the direct cell and its own setup one operator step —
// the shape a check's box is most easily given wrongly: a default no check
// may take, and steps no check may run.
func boxManager(t *testing.T, cells map[Placement]Provider) (*Manager, *FakeRunner) {
	t.Helper()
	runner := NewFakeRunner("claude-code")
	providers := map[Placement]Provider{Direct: NewFakeProvider(), Container: NewFakeProvider(), E2B: NewFakeProvider()}
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

// A BOX FOR A CHECK INSTALLS NO AGENT. AcquireBox makes the box in the cell its
// spec names, from the current catalogue, and applies exactly the steps it is
// handed, with the spec's environment: no runner's install and none of the
// catalogue's own setup — that provisions coding boxes, and a check runs a
// change's own code. A step that fails takes the box with it, as Acquire's
// does. What a coding box carries and this one must not — a coding agent, a
// login, a run's grants, the catalogue's default cell, the direct cell — is
// refused before any backend is asked for a box.
//
// Mutations: AcquireBox calling Acquire — the box has the agent installed, or
// the agent it names none of is refused, red; AcquireBox prepending
// DefaultSetup — the operator's step runs in the box, red; drop the direct
// refusal — a box is made on the engine host, red.
func TestABoxForACheckInstallsNoAgent(t *testing.T) {
	t.Parallel()
	steps := []SetupStep{GitVersionStep(), {Name: "checkout web", Commands: []string{"echo checked out"}}}
	env := map[string]string{"CREWLET_CHECK_NAME": "unit", "NPM_TOKEN": "a check's own secret"}

	t.Run("made", func(t *testing.T) {
		t.Parallel()
		container := newSpecsSeen()
		m, runner := boxManager(t, map[Placement]Provider{Container: container})
		box, err := m.AcquireBox(t.Context(), Spec{Placement: Container, TimeoutSec: 660, Env: env}, steps)
		if err != nil {
			t.Fatalf("AcquireBox: %v", err)
		}
		if runner.Installed(box.ID()) {
			t.Error("a box for a check had the coding agent installed")
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
		if specs := container.seen(); len(specs) != 1 || specs[0].CodingAgent != "" ||
			len(specs[0].CredentialFiles) != 0 || specs[0].TimeoutSec != 660 {
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
		box, err := m.AcquireBox(t.Context(), Spec{Placement: Container}, steps)
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
		"a coding agent":        {Spec{Placement: Container, CodingAgent: "claude-code"}, `names "claude-code"`},
		"a login":               {Spec{Placement: Container, CredentialFiles: map[string]string{".claude/.credentials.json": "/srv/login"}}, "credential files"},
		"a run's grants":        {Spec{Placement: Container, Repos: []RunRepo{{Repo: "web"}}}, "grants"},
		"the catalogue default": {Spec{Env: env}, "names its cell"},
		"the direct cell":       {Spec{Placement: Direct}, "engine's own user"},
	} {
		t.Run("refused: "+name, func(t *testing.T) {
			t.Parallel()
			cells := map[Placement]Provider{Direct: newSpecsSeen(), Container: newSpecsSeen(), E2B: newSpecsSeen()}
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

// A REMOTE BOX TAKES NO MOUNT, and neither does a direct one: a spec carrying a
// mount for either is refused where its box would be made, by AcquireBox and
// by Acquire alike, before the cell's backend is asked for anything — the
// remote backend reads no mount, so dropping one there would make a box told
// it reads a directory it does not have. A container box is made with its
// mounts, and a mount it cannot be given — a relative path, one in the box's
// home or at it, the root, a path a runtime's -v would split, two at one path
// — is refused too.
//
// Mutation: drop the cell's rule from provision — the e2b box is made without
// its mount, red.
func TestARemoteBoxTakesNoMount(t *testing.T) {
	t.Parallel()
	host := t.TempDir()
	mount := Mount{Host: host, Box: "/crewlet/borrowed/web/objects", ReadOnly: true}
	remote, direct, container := newSpecsSeen(), newSpecsSeen(), newSpecsSeen()
	m, _ := boxManager(t, map[Placement]Provider{E2B: remote, Direct: direct, Container: container})

	if box, err := m.AcquireBox(t.Context(), Spec{Placement: E2B, Mounts: []Mount{mount}}, nil); box != nil ||
		err == nil || !strings.Contains(err.Error(), "the e2b cell takes no mount") {
		t.Errorf("an e2b box with a mount: %v, %v; want it refused naming the cell", box, err)
	}
	if box, _, err := m.Acquire(t.Context(), Spec{Placement: Direct, CodingAgent: "claude-code",
		Mounts: []Mount{mount}}, nil); box != nil || err == nil || !strings.Contains(err.Error(), "the direct cell takes no mount") {
		t.Errorf("a direct coding box with a mount: %v, %v; want it refused naming the cell", box, err)
	}
	if specs := slices.Concat(remote.seen(), direct.seen()); len(specs) != 0 {
		t.Errorf("a backend was asked for a box its cell cannot mount into: %+v", specs)
	}

	box, err := m.AcquireBox(t.Context(), Spec{Placement: Container, Mounts: []Mount{mount}}, nil)
	if err != nil {
		t.Fatalf("a container box with its mount: %v", err)
	}
	if specs := container.seen(); len(specs) != 1 || !slices.Equal(specs[0].Mounts, []Mount{mount}) {
		t.Errorf("the container backend was asked for %+v, want one box given the mount", specs)
	}
	if box == nil {
		t.Fatal("no box")
	}

	for name, bad := range map[string][]Mount{
		"a relative box path":       {{Host: host, Box: "crewlet/objects"}},
		"a relative host path":      {{Host: "objects", Box: "/crewlet/objects"}},
		"an unclean box path":       {{Host: host, Box: "/crewlet/../objects"}},
		"the box's root":            {{Host: host, Box: "/"}},
		"the host's root":           {{Host: "/", Box: "/crewlet/objects"}},
		"the box's home":            {{Host: host, Box: DefaultHome}},
		"a path in the box's home":  {{Host: host, Box: DefaultHome + "/objects"}},
		"a path -v would split":     {{Host: host + ":/etc", Box: "/crewlet/objects"}},
		"a box path -v would split": {{Host: host, Box: "/crewlet/objects:ro"}},
		"a newline":                 {{Host: host, Box: "/crewlet/a\nb"}},
		"two at one path":           {{Host: host, Box: "/crewlet/objects"}, {Host: t.TempDir(), Box: "/crewlet/objects"}},
	} {
		before := len(container.seen())
		if box, err := m.AcquireBox(t.Context(), Spec{Placement: Container, Mounts: bad}, nil); box != nil || err == nil {
			t.Errorf("%s: a container box was made: %v, %v", name, box, err)
		}
		if after := len(container.seen()); after != before {
			t.Errorf("%s: the container backend was asked for a box", name)
		}
	}
}
