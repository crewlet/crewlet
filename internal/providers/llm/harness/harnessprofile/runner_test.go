package harnessprofile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
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
	_, err := Load("claude-code", map[string]any{"executor": map[string]any{"runner": true, "temp_env": "X_TMP"}})
	if !errors.Is(err, ErrExecutorOverride) {
		t.Errorf("an override setting executor loaded: %v", err)
	}
	if _, err := Load("claude-code", nil); err != nil {
		t.Errorf("the built-in profile does not load: %v", err)
	}
}

// AN EXECUTOR BLOCK SAYS IT IS A RUNNER, and the blocks are read in the
// table's order, whatever its names sort to. Every other fact of a block is
// optional: a runner whose harness refuses no root flag, roots no temporary
// directory of its own and states no floor says nothing more.
//
// Mutation: read the blocks in sorted order, take a block with runner unset,
// or refuse one saying nothing but runner, and a case goes red.
func TestAnExecutorBlockSaysItIsARunner(t *testing.T) {
	t.Parallel()
	got, err := decodeExecutors([]byte("zed:\n  executor: {runner: true, floor: 1.0.0}\nplain:\n  binary: p\n" +
		"alpha:\n  executor: {runner: true, temp_env: A_TMP}\n"))
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
	if _, err := decodeExecutors([]byte("x:\n  executor: {temp_env: X_TMP}\n")); err == nil {
		t.Error("a block with runner unset was taken")
	}
	if got, err := decodeExecutors([]byte("x:\n  executor: {runner: true}\n")); err != nil ||
		len(got) != 1 || !reflect.DeepEqual(got[0].block, Executor{Runner: true}) {
		t.Errorf("a runner saying nothing more reads as %+v, %v; want it, with every other fact unset", got, err)
	}
	for _, name := range []string{"claude-code", "codex", "opencode"} {
		if runners := ExecutorRunners(); !slices.Contains(runners, name) {
			t.Errorf("the built-in runners are %v, without %s", runners, name)
		}
	}
}

// AN EXECUTOR BLOCK'S FAULT NAMES ITS PROFILE AND ITS FIELD, whatever reads
// the table first: the block's one reader names both, and the profiles'
// decode passes the block by rather than refuse it with a line number alone.
//
// Mutations, each red: decode the block whole (the field goes unnamed); drop
// the profile from the error; decode the block in the profiles' decode (its
// fault is refused there, unnamed).
func TestAnExecutorBlocksFaultNamesItsProfileAndField(t *testing.T) {
	t.Parallel()
	for block, want := range map[string]string{
		"{temp_env: t}":                 `profile "x": executor.runner: must be true where the block is set`,
		"{runner: 3}":                   `profile "x": executor.runner: yaml: unmarshal errors`,
		"{runner: true, temp_env: [a]}": `profile "x": executor.temp_env: yaml: unmarshal errors`,
		"{runner: true, template: t}": `profile "x": executor.template: is no field of the block: use "floor", ` +
			`"probe_args", "refuses_root", "runner" or "temp_env"`,
		"true": `profile "x": executor: is not a mapping of the block's fields`,
	} {
		raw := []byte("x:\n  binary: x\n  executor: " + block + "\n")
		if _, err := decodeExecutors(raw); err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Errorf("the block %s is refused with %v, want %q", block, err, want)
		}
		if _, err := decodeTable(raw, "linux"); err != nil {
			t.Errorf("the profiles' decode refused the block %s itself: %v", block, err)
		}
	}
	defer func() {
		const want = `harnessprofile: profile "x": executor.runner: must be true where the block is set`
		if got := fmt.Sprint(recover()); got != want {
			t.Errorf("the built-in table's fault panics with %q, want %q", got, want)
		}
	}()
	mustExecutors([]byte("x:\n  executor: {temp_env: T}\n"))
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

// A RELEASE IS A BARE MAJOR.MINOR.PATCH, read off what `--version` printed:
// the first word starting with a digit, less a "v" and one closing ".", is
// one only where it is exactly three runs of digits. A pre-release or build
// whose third part runs on past its digits ("0.172.0-rc", "0.172.0+b"), a
// part that is no number ("1.2.x"), a fourth part or a doubled dot names
// none, as the run's script reads it.
//
// Mutation: hold the word to three dot-separated parts without checking that
// each is digits alone, or read the first three numbers of it, and a row goes
// red.
func TestAReleaseIsABareMajorMinorPatch(t *testing.T) {
	t.Parallel()
	for printed, want := range map[string]string{
		"codex-cli 0.171.0": "0.171.0", "2.1.310 (Claude Code)": "2.1.310", "opencode v2.3.2": "2.3.2",
		"GitHub Copilot CLI 1.0.114.": "1.0.114", "Muse Code 1.4.25 (1.4.25-R9361.1)": "1.4.25",
		"0.172.0-rc": "", "0.172.0+b": "", "1.2.x": "", "codex-cli 0.172.0-rc": "", "1.4.25.1": "",
		"0.171.0..": "", "1.2": "", "codex-cli dev": "", "": "",
	} {
		if got := Release(printed); got != want {
			t.Errorf("Release(%q) = %q, want %q", printed, got, want)
		}
	}
}
