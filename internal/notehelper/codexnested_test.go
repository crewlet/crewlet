package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// nestedRun is a run's repository state as the launch leaves it (the
// checkouts it handed and the budget left) beside a working directory, and
// the config file the nested hook names.
type nestedRun struct {
	work, state, config string
}

// newNestedRun is a nestedRun whose launch handed handed and left budget.
func newNestedRun(t *testing.T, budget int, handed ...string) nestedRun {
	t.Helper()
	root := t.TempDir()
	r := nestedRun{work: filepath.Join(root, "workspace"), state: filepath.Join(root, "codex-repo")}
	for _, dir := range []string{r.work, r.state} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	lines := ""
	for _, h := range handed {
		lines += h + "\n"
	}
	r.config = filepath.Join(r.state, "nested.json")
	raw, _ := json.Marshal(nestedConfig{State: r.state, Workdir: r.work})
	for path, body := range map[string]string{filepath.Join(r.state, "handed"): lines,
		filepath.Join(r.state, "budget"): strconv.Itoa(budget) + "\n",
		r.config:                         string(raw)} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

// checkout makes a repository checked out at name under the working
// directory, holding files by path relative to it.
func (r nestedRun) checkout(t *testing.T, name string, files map[string]string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(r.work, name, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	for rel, body := range files {
		if err := os.WriteFile(filepath.Join(r.work, name, rel), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// fire runs one firing of the hook, and is the context it hands, "" for none.
func (r nestedRun) fire(t *testing.T) string {
	t.Helper()
	var out, errs bytes.Buffer
	code := runCodexNested(context.Background(), flags{config: r.config},
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

// A CHECKOUT CLONED DURING THE RUN IS HANDED ITS ROOT INSTRUCTIONS ONCE: the
// first firing after it appears hands its AGENTS.override.md, else AGENTS.md,
// in Codex's own "# AGENTS.md instructions for <dir>" shape; a later firing
// hands it nothing; a checkout the launch handed, and a directory with no
// .git, are handed nothing.
//
// Mutation: hand every firing, read AGENTS.md before the override, or take a
// directory with no .git or one the launch handed, and a check goes red.
func TestACheckoutClonedDuringTheRunIsHandedItsRootInstructionsOnce(t *testing.T) {
	r := newNestedRun(t, 32<<10, "launched")
	r.checkout(t, "launched", map[string]string{"AGENTS.md": "LAUNCHED\n"})
	if err := os.MkdirAll(filepath.Join(r.work, "notes"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := r.fire(t); got != "" {
		t.Errorf("a firing with no new checkout handed %q", got)
	}
	r.checkout(t, "cloned", map[string]string{"AGENTS.md": "CLONED\n", "AGENTS.override.md": "CLONED-OVERRIDE\n"})
	want := "# AGENTS.md instructions for " + filepath.Join(r.work, "cloned") + "\n\n<INSTRUCTIONS>\nCLONED-OVERRIDE\n\n</INSTRUCTIONS>"
	if got := r.fire(t); got != want {
		t.Errorf("the firing after the clone handed\n%q\nwant\n%q", got, want)
	}
	if got := r.fire(t); got != "" {
		t.Errorf("a later firing handed %q again", got)
	}
}

// A SECOND CHECKOUT AFTER THE BUDGET IS HANDED NOTHING: the run's instructions
// hold at most what the launch left; a file past it is cut to it, and a
// checkout cloned once nothing is left is handed nothing, never again.
//
// Mutation: hand a file whole past the budget, or let a later checkout use
// what is spent.
func TestASecondCheckoutAfterTheBudgetIsHandedNothing(t *testing.T) {
	r := newNestedRun(t, 10)
	r.checkout(t, "first", map[string]string{"AGENTS.md": strings.Repeat("y", 25)})
	if got := r.fire(t); !strings.Contains(got, "\n<INSTRUCTIONS>\n"+strings.Repeat("y", 10)+"\n</INSTRUCTIONS>") {
		t.Errorf("the first checkout's instructions are %q, want its first 10 bytes", got)
	}
	r.checkout(t, "second", map[string]string{"AGENTS.md": "SECOND\n"})
	if got := r.fire(t); got != "" {
		t.Errorf("the checkout after the budget was handed %q", got)
	}
	raw, _ := os.ReadFile(filepath.Join(r.state, "handed"))
	if string(raw) != "first\nsecond\n" {
		t.Errorf("the checkouts handed are %q, want both", raw)
	}
}

// ONLY A CHECKOUT'S OWN FILE IS HANDED: an AGENTS.md that is a link out of its
// checkout, and a link to a checkout in the working directory, hand nothing.
//
// Mutation: follow a link out of the checkout, or take a link for a checkout.
func TestOnlyACheckoutsOwnFileIsHanded(t *testing.T) {
	r := newNestedRun(t, 32<<10)
	outside := filepath.Join(t.TempDir(), "elsewhere.md")
	if err := os.WriteFile(outside, []byte("OUTSIDE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.checkout(t, "linked-file", nil)
	if err := os.Symlink(outside, filepath.Join(r.work, "linked-file", "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	r2 := t.TempDir()
	if err := os.MkdirAll(filepath.Join(r2, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r2, "AGENTS.md"), []byte("LINKED-DIR\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(r2, filepath.Join(r.work, "linked-dir")); err != nil {
		t.Fatal(err)
	}
	if got := r.fire(t); got != "" {
		t.Errorf("a link handed %q", got)
	}
}
