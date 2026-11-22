package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// museLine is one envelope of Muse Code's `exec --json` stream.
func museLine(t *testing.T, payloadType string, payload map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"schema_version": 1, "record_type": "event",
		"payload_type": payloadType, "payload": payload})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// A MUSE CODE RUN IS CHARGED THE COUNTS ITS STREAM REPORTS. Every run prints a
// `run.usage` event before its terminal one, its counts per model: the prompt
// with its cached part in it, and the output with its reasoning in it, beside
// each part's own count (measured on 1.4.25). Read as the whole prompt, the
// cached part is not charged twice; read with the reasoning added, the
// reasoning is.
//
// Mutation: drop the usage paths, and the run is estimated; read the prompt
// as less its cached part, and it is charged 120 tokens more than it spent;
// read the reasoning beside the output, and 21 more.
func TestAMuseCodeRunIsChargedTheCountsItsStreamReports(t *testing.T) {
	t.Parallel()
	p, ok := harnessprofile.Builtin("muse-code")
	if !ok {
		t.Fatal("no built-in muse-code profile")
	}
	stdout := strings.Join([]string{
		museLine(t, "run.output.delta", map[string]any{"text": "The build "}),
		museLine(t, "run.output.delta", map[string]any{"text": "is green."}),
		museLine(t, "run.usage", map[string]any{"kind": "run_usage", "calls": 3, "models": []any{
			map[string]any{"model": "a-model", "calls": 2, "input_tokens": 205, "cached_tokens": 80,
				"output_tokens": 24, "reasoning_tokens": 14},
			map[string]any{"model": "another", "calls": 1, "input_tokens": 104, "cached_tokens": 40,
				"output_tokens": 12, "reasoning_tokens": 7},
		}}),
		museLine(t, "run.terminal.completed", map[string]any{"terminal": "completed", "text": "The build is green."}),
	}, "\n")
	got := extract(p, stdout)
	if got.text != "The build is green." {
		t.Errorf("answer = %q, want the terminal event's", got.text)
	}
	if !got.reported {
		t.Fatal("the run's counts were not read, so it would be charged an estimate")
	}
	input := p.Usage.PromptTokens(got.input, got.cacheRead, got.cacheWrite)
	if input != 309 || got.cacheRead != 120 || got.output+got.reasoning != 36 {
		t.Errorf("charged %d in (%d cached) and %d out, want the run's 309 in (120 cached) and 36 out",
			input, got.cacheRead, got.output+got.reasoning)
	}
}

// A MUSE CODE CALL IS TOLD NOTHING FROM ITS HOME'S SKILLS OR INSTRUCTIONS:
// Muse reads the skills under .agents/skills and .config/muse/skills, the
// personal .config/muse/AGENTS.md and an installed plugin's skills with this
// profile's flags (measured on 1.4.25), so all four go with the prune, and
// the login and the seeded settings stay.
//
// Mutation: drop any of the three from volatile_paths, and it outlives the
// prune.
func TestAMuseCodeCallIsToldNothingFromItsHomesSkills(t *testing.T) {
	t.Parallel()
	p, ok := harnessprofile.Builtin("muse-code")
	if !ok {
		t.Fatal("no built-in muse-code profile")
	}
	dir := t.TempDir()
	t.Cleanup(func() { forgetWorkspace(dir) })
	ws, err := Shared(dir, "muse-code", p)
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	left := []string{
		".agents/skills/planted/SKILL.md",
		".config/muse/skills/planted/SKILL.md",
		".config/muse/AGENTS.md",
		".local/share/muse/plugins/installed.json",
	}
	kept := []string{".config/muse/auth.json", ".config/muse/settings.json"}
	for _, rel := range append(slices.Clone(left), kept...) {
		path := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := ws.prune(home); err != nil {
		t.Fatalf("prune: %v", err)
	}
	for _, rel := range left {
		if _, err := os.Stat(filepath.Join(home, rel)); !os.IsNotExist(err) {
			t.Errorf("%s outlived the prune (%v): the seat's next call would be told it", rel, err)
		}
	}
	for _, rel := range kept {
		if _, err := os.Stat(filepath.Join(home, rel)); err != nil {
			t.Errorf("%s went with the prune: %v", rel, err)
		}
	}
}
