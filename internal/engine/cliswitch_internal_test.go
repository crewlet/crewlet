package engine

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/configplane"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent"
)

// AN ENTRY MOVED TO ANOTHER CLI IS AN ORDINARY REVISION, APPLIED LIVE.
//
// A key keeps its state directory when its agent changes — the directory is
// derived from the key, and the operator renders one per provider name — and
// the process-wide workspace registry held that directory under the CLI that
// first opened it, for the life of the process. So moving `k` from claude-code
// to opencode was admitted by the API and then refused by every running node
// (`state_dir ... is already driving "claude-code"`), while a node that
// restarted applied it: the fleet split across two revisions until every node
// had restarted. Each CLI now keeps its own share of the directory
// (<state dir>/<harness>), so the new CLI opens a login directory of its own
// and moving back finds the old one.
//
// NOT PARALLEL: the derived case points the CLI home at a directory of its own.
//
// Mutation: refuse a second CLI on a directory Shared already holds, as it
// did, and the move to opencode is refused on the running node.
func TestAnEntryMovedToAnotherCLIAppliesLive(t *testing.T) {
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
// its files in that CLI's own share of dir.
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
	cli, ok := provider.(*cliagent.Provider)
	if !ok {
		t.Fatalf("entry k is %T, want a CLI", provider)
	}
	if cli.Agent() != agent {
		t.Fatalf("entry k drives %q, want %q", cli.Agent(), agent)
	}
	if got, want := cli.Workspace().Root(), filepath.Join(dir, agent); got != want {
		t.Errorf("entry k on %s keeps its files in %q, want %q", agent, got, want)
	}
}
