package e2e

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// cutsSeen is a sandbox.HomeMeter keeping every cut it is told.
type cutsSeen struct {
	mu      sync.Mutex
	reasons []string
}

func (c *cutsSeen) HomeCut(reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reasons = append(c.reasons, reason)
}

func (c *cutsSeen) seen() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.reasons)
}

// A CONTAINER BOX STARTS FROM ITS SEAT'S CACHED HOME, against a real runtime:
// the first box builds the home once, in a build handed no credential, cutting
// the cached steps at the first one that changes the box outside its home; the
// next box copies the same home, built nothing again, and cut nothing again.
// What the cached home holds is in each box; what was cut, or runs after the
// cut, is not, for the caller to run.
func TestAContainerBoxStartsFromItsSeatsCachedHome(t *testing.T) {
	t.Parallel()
	runtime := usableContainerRuntime(t)
	if runtime == "" {
		t.Skip("no usable container runtime; the cache's own suite covers it without one")
	}
	cuts := &cutsSeen{}
	local, err := sandbox.NewLocal(sandbox.LocalOptions{
		Placement: sandbox.Container, StateDir: t.TempDir(),
		Image: sandboxImage(), Runtime: filepath.Base(runtime), Homes: cuts,
	})
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	const secret = "ghs_0123456789abcdefSECRET"
	spec := sandbox.Spec{
		Placement: sandbox.Container,
		Env:       map[string]string{"CREWLET_AGENT_HANDLE": "sre", "GH_TOKEN": secret},
		Secrets:   []string{secret},
	}
	steps := []sandbox.SetupStep{
		{Name: "tools", Commands: []string{`mkdir -p "$HOME/.local/bin" && env > "$HOME/.local/bin/built-with" ` +
			`&& cat /proc/sys/kernel/random/uuid > "$HOME/.local/bin/build"`}},
		{Name: "system", Commands: []string{"touch /var/tmp/installed"}},
		{Name: "after", Commands: []string{`touch "$HOME/after"`}},
	}
	read := func(box sandbox.Sandbox, cmd string) (string, int) {
		t.Helper()
		res, err := box.Exec(t.Context(), cmd, sandbox.ExecOptions{})
		if err != nil {
			t.Fatalf("Exec %q: %v", cmd, err)
		}
		return strings.TrimSpace(res.Stdout), res.ExitCode
	}
	var builds []string
	for i := range 2 {
		box, cached, err := local.CreateProvisioned(t.Context(), spec, steps)
		if err != nil {
			t.Fatalf("box %d: CreateProvisioned: %v", i+1, err)
		}
		t.Cleanup(func() { box.Close(context.Background()) })
		if cached != 1 {
			t.Fatalf("box %d holds %d cached steps, want the one before the step leaving the home", i+1, cached)
		}
		build, _ := read(box, `cat "$HOME/.local/bin/build"`)
		builds = append(builds, build)
		env, _ := read(box, `cat "$HOME/.local/bin/built-with"`)
		if !strings.Contains(env, "CREWLET_AGENT_HANDLE=sre") || strings.Contains(env, secret) {
			t.Errorf("box %d: the build was handed %q", i+1, env)
		}
		for _, cmd := range []string{"test -e /var/tmp/installed", `test -e "$HOME/after"`} {
			if _, code := read(box, cmd); code == 0 {
				t.Errorf("box %d: `%s` holds, a step its caller runs", i+1, cmd)
			}
		}
	}
	if builds[0] == "" || builds[0] != builds[1] {
		t.Errorf("the second box was built again: %q, %q", builds[0], builds[1])
	}
	if got := cuts.seen(); !slices.Equal(got, []string{"outside_home"}) {
		t.Errorf("cuts counted %v, want one outside_home", got)
	}
}
