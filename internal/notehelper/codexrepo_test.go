package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// repoRun is a Codex run's working directory, its repository state and its
// Codex home, as codex-launch and codex-nested find them.
type repoRun struct {
	work, state, agents string
}

// newRepoRun is a repoRun whose working directory holds files, by path
// relative to it (a path ending in "/" a directory), and nothing else yet.
func newRepoRun(t *testing.T, files map[string]string) repoRun {
	t.Helper()
	root := t.TempDir()
	r := repoRun{work: filepath.Join(root, "workspace"), state: filepath.Join(root, "codex-repo"),
		agents: filepath.Join(root, "codex", "AGENTS.md")}
	for _, dir := range []string{r.work, filepath.Dir(r.agents)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	r.add(t, files)
	return r
}

// add writes files under the working directory, as newRepoRun does.
func (r repoRun) add(t *testing.T, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		path := filepath.Join(r.work, rel)
		if strings.HasSuffix(rel, "/") {
			if err := os.MkdirAll(path, 0o700); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// launch runs codex-launch in the working directory with budget and first.
func (r repoRun) launch(t *testing.T, budget int, first ...string) {
	t.Helper()
	raw, _ := json.Marshal(launchConfig{State: r.state, Agents: r.agents, Budget: budget, First: first})
	config := filepath.Join(t.TempDir(), "launch.json")
	if err := os.WriteFile(config, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := codexLaunch(config, r.work); err != nil {
		t.Fatalf("the launch failed: %v", err)
	}
}

// fire runs one firing of the nested hook off the state the launch wrote, and
// is the context it hands, "" for none.
func (r repoRun) fire(t *testing.T) string {
	t.Helper()
	var out, errs bytes.Buffer
	code := runCodexNested(context.Background(), flags{config: filepath.Join(r.state, "nested.json")},
		strings.NewReader(`{"tool_name":"Bash","tool_input":{"command":"git clone x"}}`), &out, &errs, os.Getenv)
	if code != exitDone || errs.Len() > 0 {
		t.Fatalf("the firing exited %d: %s", code, errs.String())
	}
	if out.Len() == 0 {
		return ""
	}
	var answer struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out.Bytes(), &answer); err != nil || answer.HookSpecificOutput.HookEventName != "PostToolUse" {
		t.Fatalf("the firing answered %s (%v)", out.String(), err)
	}
	return answer.HookSpecificOutput.AdditionalContext
}

// read is a file the run holds, "" where there is none.
func read(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// workdir is the working directory as the launch resolves it.
func (r repoRun) workdir(t *testing.T) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(r.work)
	if err != nil {
		t.Fatal(err)
	}
	return real
}

// THE LAUNCH HANDS EACH CHECKOUT'S ROOT INSTRUCTIONS, THE REPOSITORIES THE RUN
// PUSHES TO FIRST: each repository checked out directly under the working
// directory hands its AGENTS.override.md, else AGENTS.md, labelled with its
// root, those first names in their order, the rest by name; a directory with
// no .git, a link to a checkout, a checkout under a dot-name and the working
// directory's own AGENTS.md hand nothing and are not handed. An earlier run's
// AGENTS.md is replaced.
//
// Mutation: read AGENTS.md before the override, ignore first, take a
// directory without .git, a link or a dot-name, or keep the earlier file.
func TestTheLaunchHandsEachCheckoutsInstructionsTheReposItPushesToFirst(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, map[string]string{
		"AGENTS.md":                "WORKDIR-OWN\n",
		"zeta/.git/":               "",
		"zeta/AGENTS.md":           "ZETA\n",
		"alpha/.git/":              "",
		"alpha/AGENTS.md":          "ALPHA\n",
		"alpha/AGENTS.override.md": "ALPHA-OVERRIDE\n",
		"beta/.git/":               "",
		"beta/AGENTS.md":           "BETA\n",
		"bare/.git/":               "",
		"plain/AGENTS.md":          "NOT-A-CHECKOUT\n",
		".hidden/.git/":            "",
		".hidden/AGENTS.md":        "HIDDEN\n",
	})
	if err := os.Symlink(filepath.Join(r.work, "zeta"), filepath.Join(r.work, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.agents, []byte("EARLIER-RUN\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.launch(t, 32<<10, "zeta", "missing", ".hidden", "alpha")
	work := r.workdir(t)
	want := "for `zeta` with root " + work + "/zeta\n\nZETA\n\n\nfor `alpha` with root " + work +
		"/alpha\n\nALPHA-OVERRIDE\n\n\nfor `beta` with root " + work + "/beta\n\nBETA\n"
	if got := read(t, r.agents); got != want {
		t.Errorf("CODEX_HOME/AGENTS.md is\n%q\nwant\n%q", got, want)
	}
	if got := read(t, filepath.Join(r.state, "handed")); got != "zeta\nalpha\nbare\nbeta\n" {
		t.Errorf("the checkouts handed are %q, want zeta, alpha, bare and beta in that order", got)
	}
	if got := read(t, filepath.Join(r.state, "nested.json")); !strings.Contains(got, `"workdir":"`+work+`"`) {
		t.Errorf("the nested hook's config is %s, want the working directory %s", got, work)
	}
}

// A RUN WITH NO CHECKOUT IS HANDED NO INSTRUCTIONS: CODEX_HOME/AGENTS.md is
// replaced by none, never kept from an earlier run, and the whole budget is
// left for a checkout the run clones.
//
// Mutation: keep the earlier file, or leave less than the budget.
func TestARunWithNoCheckoutIsHandedNoInstructions(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, nil)
	if err := os.WriteFile(r.agents, []byte("EARLIER-RUN\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.launch(t, 32<<10)
	if _, err := os.Lstat(r.agents); !os.IsNotExist(err) {
		t.Errorf("CODEX_HOME/AGENTS.md is still there (%v), want none", err)
	}
	if got := read(t, filepath.Join(r.state, "budget")); got != "32768\n" {
		t.Errorf("the budget left is %q, want the whole 32768", got)
	}
}

// A RUN'S INSTRUCTIONS ARE BOUND BY ONE BUDGET: a file past what is left is
// cut to it, a checkout found once nothing is left hands nothing but is
// still handed, and a checkout cloned during the run is held to what the
// launch left.
//
// Mutation: hand a file whole past the budget, let the next checkout use what
// is spent, or start the hook from a fresh budget.
func TestARunsInstructionsAreBoundByOneBudget(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, map[string]string{
		"a/.git/": "", "a/AGENTS.md": strings.Repeat("x", 6),
	})
	r.launch(t, 10)
	if got := read(t, r.agents); !strings.HasSuffix(got, "\n\nxxxxxx") {
		t.Errorf("a's instructions are %q, want its 6 bytes whole", got)
	}
	r.add(t, map[string]string{"b/.git/": "", "b/AGENTS.md": strings.Repeat("y", 25)})
	if got := r.fire(t); !strings.Contains(got, "\n<INSTRUCTIONS>\nyyyy\n</INSTRUCTIONS>") {
		t.Errorf("the clone's instructions are %q, want the 4 bytes the launch left", got)
	}
	r.add(t, map[string]string{"c/.git/": "", "c/AGENTS.md": "C\n"})
	if got := r.fire(t); got != "" {
		t.Errorf("the checkout after the budget was handed %q", got)
	}
	if got := read(t, filepath.Join(r.state, "handed")); got != "a\nb\nc\n" {
		t.Errorf("the checkouts handed are %q, want all three", got)
	}
	if got := read(t, filepath.Join(r.state, "budget")); got != "0\n" {
		t.Errorf("the budget left is %q, want 0", got)
	}
}

// INSTRUCTIONS ARE HANDED AS TEXT CODEX KEEPS WHOLE: a cut falls on a rune
// boundary, so a character the budget would split is left out and charged
// for nothing, and a file that is not UTF-8 is handed with each invalid byte
// replaced, never raw bytes Codex may drop the whole file over.
//
// Mutation: cut mid-rune, or hand the bytes as they are.
func TestInstructionsAreHandedAsValidText(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, map[string]string{"a/.git/": "", "a/AGENTS.md": "abcé"})
	r.launch(t, 4)
	if got := read(t, r.agents); !strings.HasSuffix(got, "\n\nabc") {
		t.Errorf("the multibyte cut hands %q, want abc and no half of é", got)
	}
	if got := read(t, filepath.Join(r.state, "budget")); got != "1\n" {
		t.Errorf("the budget left is %q, want the byte the split é did not take", got)
	}
	r = newRepoRun(t, map[string]string{"b/.git/": "", "b/AGENTS.md": "caf\xe9\n"})
	r.launch(t, 32<<10)
	got := read(t, r.agents)
	if !utf8.ValidString(got) || !strings.HasSuffix(got, "\n\ncaf�\n") {
		t.Errorf("a Latin-1 file is handed as %q, want valid UTF-8 with the byte replaced", got)
	}
}

// A FILE OF WHITESPACE ALONE HANDS NO SECTION AND COSTS NOTHING, as Codex
// reads one: the override that holds it is still the checkout's file, so its
// AGENTS.md is not read in its place.
//
// Mutation: hand an empty section, charge its bytes, or fall back to
// AGENTS.md.
func TestAFileOfWhitespaceHandsNothing(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, map[string]string{
		"a/.git/": "", "a/AGENTS.override.md": "  \n\t\n", "a/AGENTS.md": "A\n",
	})
	r.launch(t, 100)
	if got := read(t, r.agents); got != "" {
		t.Errorf("CODEX_HOME/AGENTS.md is %q, want none", got)
	}
	if got := read(t, filepath.Join(r.state, "budget")); got != "100\n" {
		t.Errorf("the budget left is %q, want all 100", got)
	}
}

// A CHECKOUT CLONED DURING THE RUN IS HANDED ITS ROOT INSTRUCTIONS ONCE: the
// first firing after it appears hands its AGENTS.override.md, else AGENTS.md,
// in Codex's own "# AGENTS.md instructions for <dir>" shape; a later firing
// hands it nothing; a checkout the launch handed, a directory with no .git
// and a checkout under a dot-name are handed nothing.
//
// Mutation: hand every firing, read AGENTS.md before the override, or take a
// directory with no .git, a dot-name or one the launch handed.
func TestACheckoutClonedDuringTheRunIsHandedItsRootInstructionsOnce(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, map[string]string{"launched/.git/": "", "launched/AGENTS.md": "LAUNCHED\n"})
	r.launch(t, 32<<10)
	r.add(t, map[string]string{"notes/AGENTS.md": "NOT-A-CHECKOUT\n", ".cache/.git/": "", ".cache/AGENTS.md": "HIDDEN\n"})
	if got := r.fire(t); got != "" {
		t.Errorf("a firing with no new checkout handed %q", got)
	}
	r.add(t, map[string]string{"cloned/.git/": "", "cloned/AGENTS.md": "CLONED\n", "cloned/AGENTS.override.md": "CLONED-OVERRIDE\n"})
	want := "# AGENTS.md instructions for " + filepath.Join(r.workdir(t), "cloned") + "\n\n<INSTRUCTIONS>\nCLONED-OVERRIDE\n\n</INSTRUCTIONS>"
	if got := r.fire(t); got != want {
		t.Errorf("the firing after the clone handed\n%q\nwant\n%q", got, want)
	}
	if got := r.fire(t); got != "" {
		t.Errorf("a later firing handed %q again", got)
	}
	if got := read(t, filepath.Join(r.state, "handed")); got != "launched\ncloned\n" {
		t.Errorf("the checkouts handed are %q, want launched and cloned", got)
	}
}

// ONLY A CHECKOUT'S OWN FILE IS HANDED: an AGENTS.md that is a link out of its
// checkout, and a link to a checkout in the working directory, hand nothing.
//
// Mutation: follow a link out of the checkout, or take a link for a checkout.
func TestOnlyACheckoutsOwnFileIsHanded(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, nil)
	r.launch(t, 32<<10)
	outside := filepath.Join(t.TempDir(), "elsewhere.md")
	if err := os.WriteFile(outside, []byte("OUTSIDE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.add(t, map[string]string{"linked-file/.git/": ""})
	if err := os.Symlink(outside, filepath.Join(r.work, "linked-file", "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	if err := os.MkdirAll(filepath.Join(other, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "AGENTS.md"), []byte("LINKED-DIR\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, filepath.Join(r.work, "linked-dir")); err != nil {
		t.Fatal(err)
	}
	if got := r.fire(t); got != "" {
		t.Errorf("a link handed %q", got)
	}
}
