package engine

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/configplane"
	"github.com/crewlet/crewlet/internal/providers/llm/harness"
)

// AN ENTRY MOVED TO ANOTHER HARNESS IS AN ORDINARY REVISION, APPLIED LIVE.
//
// A key keeps its state directory when its agent changes (it is derived from
// the key), so each harness keeps its own share (<state dir>/<harness>): the
// new harness opens a login directory of its own and moving back finds the
// old one, rather than running nodes refusing a move a restarted node applies
// and the fleet splitting across two revisions.
//
// NOT PARALLEL: the derived case points the harness home at a directory of its own.
//
// Mutation: refuse a second harness on a directory Shared already holds, and
// the move to opencode is refused on the running node.
func TestAnEntryMovedToAnotherHarnessAppliesLive(t *testing.T) {
	for _, tc := range []struct {
		name     string
		stateDir func(t *testing.T) (field, dir string)
	}{
		{
			name: "a derived state_dir",
			stateDir: func(t *testing.T) (string, string) {
				root := t.TempDir()
				t.Setenv(config.HarnessHomeEnv, root)
				return "", filepath.Join(root, "k")
			},
		},
		{
			name: "an explicit state_dir",
			stateDir: func(t *testing.T) (string, string) {
				dir := t.TempDir()
				return fmt.Sprintf("state_dir: %q", dir), dir
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			field, dir := tc.stateDir(t)
			revision := func(agent, model string) *config.Company {
				return parseBundled(t, fmt.Sprintf(`
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    k:
      harness: %s
      model: %s
      %s
roles:
  - name: CEO
    handle: ceo
    llm: k
`, agent, model, field))
			}
			e := bundleNode(t, revision("claude-code", "sonnet"))
			assertEntryOn(t, e, "claude-code", dir)

			status, stages, err := e.Apply(t.Context(), revision("opencode", "anthropic/claude-sonnet-5"), time.Now())
			if err != nil || status != configplane.StatusOK {
				t.Fatalf("moving k to opencode: Apply = (%s, %v, %v), want ok on the running node", status, stages, err)
			}
			assertEntryOn(t, e, "opencode", dir)

			status, stages, err = e.Apply(t.Context(), revision("claude-code", "sonnet"), time.Now())
			if err != nil || status != configplane.StatusOK {
				t.Fatalf("moving k back: Apply = (%s, %v, %v), want ok", status, stages, err)
			}
			assertEntryOn(t, e, "claude-code", dir)
		})
	}
}

// assertEntryOn checks that the running epoch's entry k drives agent, keeping
// its files in that harness's own share of dir.
func assertEntryOn(t *testing.T, e *Engine, agent, dir string) {
	t.Helper()
	c := e.Company()
	if c == nil || c.Models == nil {
		t.Fatal("the node runs no company")
	}
	provider, found := c.Models.Provider("k")
	if !found {
		t.Fatal("the running epoch has no entry k")
	}
	cli, ok := provider.(*harness.Provider)
	if !ok {
		t.Fatalf("entry k is %T, want a harness", provider)
	}
	if cli.Harness() != agent {
		t.Fatalf("entry k drives %q, want %q", cli.Harness(), agent)
	}
	if got, want := cli.Workspace().Root(), filepath.Join(dir, agent); got != want {
		t.Errorf("entry k on %s keeps its files in %q, want %q", agent, got, want)
	}
}
