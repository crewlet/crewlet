package harnessprofile

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
)

// THE EXECUTOR BLOCK IS BUILT-IN ONLY: an entry's `overrides` that sets it is
// refused, since which harnesses run a seat's executor is this build's own.
//
// Mutation: drop the refusal in Load, and the override loads.
func TestAnOverrideCannotSetTheExecutorBlock(t *testing.T) {
	t.Parallel()
	_, err := Load("claude-code", map[string]any{"executor": map[string]any{"runner": true, "e2b_template": "base"}})
	if !errors.Is(err, ErrExecutorOverride) {
		t.Errorf("an override setting executor loaded: %v", err)
	}
	if _, err := Load("claude-code", nil); err != nil {
		t.Errorf("the built-in profile does not load: %v", err)
	}
}

// AN EXECUTOR BLOCK SAYS IT IS A RUNNER AND NAMES A TEMPLATE, and the blocks
// are read in the table's order, whatever its names sort to.
//
// Mutation: read the blocks in sorted order, or take a block with runner unset
// or no template, and a case goes red.
func TestAnExecutorBlockSaysItIsARunnerAndNamesATemplate(t *testing.T) {
	t.Parallel()
	got, err := decodeExecutors([]byte("zed:\n  executor: {runner: true, e2b_template: z}\nplain:\n  binary: p\n" +
		"alpha:\n  executor: {runner: true, e2b_template: a, temp_env: A_TMP}\n"))
	if err != nil {
		t.Fatalf("a table of two runners: %v", err)
	}
	var names []string
	for _, e := range got {
		names = append(names, e.name)
	}
	if !slices.Equal(names, []string{"zed", "alpha"}) || got[1].block.TempEnv != "A_TMP" {
		t.Errorf("the runners read as %+v, want zed then alpha, alpha's temp variable kept", got)
	}
	for name, table := range map[string]string{
		"runner unset": "x:\n  executor: {e2b_template: x}\n",
		"no template":  "x:\n  executor: {runner: true}\n",
	} {
		if _, err := decodeExecutors([]byte(table)); err == nil {
			t.Errorf("%s: the block was taken", name)
		}
	}
	for _, name := range []string{"claude-code", "codex", "opencode"} {
		if runners := ExecutorRunners(); !slices.Contains(runners, name) {
			t.Errorf("the built-in runners are %v, without %s", runners, name)
		}
	}
}

// THE DOCS SAY WHICH HARNESSES RUN A SEAT'S EXECUTOR, as the profiles list
// them: llm-backends.md's "Runs a seat's executor" column says yes for exactly
// the runners ([ExecutorRunners]), so a runner added to profiles.yaml is
// documented in the same change.
//
// Mutation: list a runner in profiles.yaml that the table says no to, or say
// yes for a harness no profile lists, and this goes red.
func TestTheDocsSayWhichHarnessesRunASeatsExecutor(t *testing.T) {
	t.Parallel()
	page := filepath.Join(sourcetree.Root(t), "docs", "concepts", "llm-backends.md")
	raw, err := os.ReadFile(page) //nolint:gosec // a fixed path under the module root
	if err != nil {
		t.Fatal(err)
	}
	_, table, found := strings.Cut(string(raw), "| Models | `harness` | Signs in with | Runs a seat's executor |\n")
	if !found {
		t.Fatal(`llm-backends.md has no table with a "Runs a seat's executor" column`)
	}
	table, _, _ = strings.Cut(table, "\n\n")
	name := regexp.MustCompile("^`([a-z0-9-]+)`")
	var yes []string
	for _, row := range strings.Split(table, "\n") {
		cells := strings.Split(strings.Trim(row, "|"), "|")
		if len(cells) < 4 {
			continue
		}
		harness := name.FindStringSubmatch(strings.TrimSpace(cells[1]))
		if harness != nil && strings.HasPrefix(strings.TrimSpace(cells[len(cells)-1]), "yes") {
			yes = append(yes, harness[1])
		}
	}
	if !slices.Equal(yes, ExecutorRunners()) {
		t.Errorf("the docs say %v run a seat's executor, the profiles list %v", yes, ExecutorRunners())
	}
}
