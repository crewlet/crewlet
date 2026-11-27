package sandbox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/shellword"
)

// cachedHomes is a FakeProvider whose run boxes start from a cached home
// holding the first cached of the steps it is offered.
type cachedHomes struct {
	*FakeProvider
	cached  int
	offered [][]SetupStep
}

func (p *cachedHomes) CreateProvisioned(ctx context.Context, spec Spec, steps []SetupStep) (Sandbox, int, error) {
	p.offered = append(p.offered, slices.Clone(steps))
	box, err := p.FakeProvider.Create(ctx, spec)
	return box, p.cached, err
}

func names(steps []SetupStep) []string {
	var out []string
	for _, s := range steps {
		out = append(out, s.Name)
	}
	return out
}

// A RUN'S BOX FROM A CACHED HOME RUNS WHAT THE HOME DOES NOT HOLD, IN ORDER:
// the cache is offered every step but the launch's credential files, which are
// written first as ever, and the steps after the cached ones run after them. A
// check's box is never offered the cache.
func TestABoxFromACachedHomeRunsTheRestOfItsSetupInOrder(t *testing.T) {
	homes := &cachedHomes{FakeProvider: NewFakeProvider(), cached: 2}
	m, _ := boxManager(t, map[Placement]Provider{Container: homes})
	setup := []SetupStep{
		{Name: "integration-files", Files: map[string]string{".config/tool/token": "tok"}, Credentials: true},
		{Name: "operator", Commands: []string{"install the operator's tools"}},
		{Name: "tools", Commands: []string{"install the seat's tools"}},
		{Name: "token", Commands: []string{"write the token"}},
	}
	box, _, err := m.Acquire(t.Context(), Spec{Placement: Container, Harness: "claude-code"}, setup)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if len(homes.offered) != 1 || !slices.Equal(names(homes.offered[0]), []string{"operator", "tools", "token"}) {
		t.Fatalf("the cache was offered %v, want every step but the credential files", homes.offered)
	}
	fake, ok := box.(*FakeSandbox)
	if !ok {
		t.Fatalf("box is a %T", box)
	}
	ran := strings.Join(fake.Commands(), "\n")
	for _, cached := range []string{"install the operator's tools", "install the seat's tools"} {
		if strings.Contains(ran, cached) {
			t.Errorf("a step the cached home holds ran again: %q", cached)
		}
	}
	if !strings.Contains(ran, "write the token") {
		t.Errorf("the step after the cached ones did not run: %v", fake.Commands())
	}
	if got, err := fake.ReadFile(t.Context(), ".config/tool/token"); err != nil || string(got) != "tok" {
		t.Errorf("the credential files were not written: %q, %v", got, err)
	}

	if _, err := m.AcquireBox(t.Context(), Spec{Placement: Container, Check: "check-1"},
		[]SetupStep{{Name: "floor", Commands: []string{"git --version"}}}); err != nil {
		t.Fatalf("AcquireBox: %v", err)
	}
	if len(homes.offered) != 1 {
		t.Errorf("a check's box was offered the cache: %v", homes.offered)
	}
}

// What a build is handed of a launch's env: the seat's identity facts and each
// step's literal env the launch carries as written, never a value a reference
// resolved or a seat's own env replaced; everything else is withheld.
func TestABuildIsHandedOnlyLiteralsAndTheSeatsIdentity(t *testing.T) {
	env := map[string]string{
		"CREWLET_AGENT_HANDLE": "sre", "CREWLET_AGENT_EMAIL": "sre@example.com",
		"GIT_TERMINAL_PROMPT": "0", "NPM_TOKEN": "npm_resolvedvalue", "REGION": "eu-west-1",
		"ANTHROPIC_API_KEY": "sk-ant-resolved",
	}
	steps := []SetupStep{{Name: "a", Env: map[string]string{
		"GIT_TERMINAL_PROMPT": "0", "NPM_TOKEN": "${NPM_TOKEN}", "REGION": "us-east-1",
	}}}
	got := buildEnv(env, steps)
	want := map[string]string{"CREWLET_AGENT_HANDLE": "sre", "CREWLET_AGENT_EMAIL": "sre@example.com",
		"GIT_TERMINAL_PROMPT": "0"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("buildEnv = %v, want %v", got, want)
	}
	if w := withheld(env, got); !slices.Equal(w, []string{"ANTHROPIC_API_KEY", "NPM_TOKEN", "REGION"}) {
		t.Errorf("withheld = %v", w)
	}
}

// A step's escape is what it changed outside the home and /tmp that the
// container had not changed before it, named at most maxEscapedPaths times.
func TestAStepsEscapeIsItsOwnChangeOutsideTheHome(t *testing.T) {
	before := map[string]bool{"/home": true, "/home/user": true}
	after := map[string]bool{"/home": true, "/home/user": true, "/home/user/.npm-global": true,
		"/tmp/npm-1": true, "/usr": true, "/usr/local/bin/tool": true}
	if got := escaped(before, after); !slices.Equal(got, []string{"/usr", "/usr/local/bin/tool"}) {
		t.Errorf("escaped = %v", got)
	}
	if got := escaped(before, before); len(got) != 0 {
		t.Errorf("a step that changed nothing escaped: %v", got)
	}
	many := map[string]bool{}
	for i := range 40 {
		many[fmt.Sprintf("/opt/%02d", i)] = true
	}
	if got := escaped(nil, many); len(got) != maxEscapedPaths {
		t.Errorf("an escape named %d paths, want %d", len(got), maxEscapedPaths)
	}
}

// A NODE THAT HAS NEVER RUN THE BOX IMAGE KEYS ITS FIRST BOX'S HOME TOO: the
// image is pulled, as the box's own run would pull it, and its ID read after,
// so a fresh node's first box is cached rather than running every step. An
// image the node holds is not pulled again.
func TestAFreshNodesFirstBoxKeysTheImageItPulls(t *testing.T) {
	dir := t.TempDir()
	runtime := filepath.Join(dir, "docker")
	writeExecutable(t, runtime, `#!/bin/sh
d=`+shellword.Quote(dir)+`
echo "$1" >> "$d/calls"
case "$1 $2" in
"image inspect")
	[ -e "$d/pulled" ] || { echo "Error response from daemon: No such image: alpine:3.24.1" >&2; exit 1; }
	echo sha256:feedface;;
"pull alpine:3.24.1") : > "$d/pulled";;
*) exit 2;;
esac
`)
	l := &Local{opts: LocalOptions{Placement: Container, Image: "alpine:3.24.1"}, cli: containerCLI{path: runtime}}
	for range 2 {
		if id, err := l.imageID(t.Context()); err != nil || id != "sha256:feedface" {
			t.Fatalf("imageID on a node without the image = %q, %v", id, err)
		}
	}
	calls, err := os.ReadFile(filepath.Join(dir, "calls"))
	if err != nil {
		t.Fatal(err)
	}
	if pulls := strings.Count(string(calls), "pull\n"); pulls != 1 {
		t.Errorf("the image was pulled %d times, want once: %q", pulls, calls)
	}
}
