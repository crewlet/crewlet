package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// launchImporting runs checkout-launch under a Claude Code spec that imports
// its instruction files, and is what it wrote for the home and said.
func (r repoRun) launchImporting(t *testing.T) (string, string) {
	t.Helper()
	spec := checkoutSpec{Instructions: []instructionSlot{{Names: []string{"CLAUDE.md"}},
		{Names: []string{".claude/CLAUDE.md"}}, {Names: []string{"CLAUDE.local.md"}}}, ImportInstructions: true}
	raw, _ := json.Marshal(launchConfig{State: r.state, Agents: r.agents, checkoutSpec: spec})
	config := filepath.Join(t.TempDir(), "launch.json")
	if err := os.WriteFile(config, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	var errs bytes.Buffer
	if err := checkoutLaunch(config, r.work, strings.NewReader(""), &errs); err != nil {
		t.Fatalf("the launch: %v\n%s", err, errs.String())
	}
	got, err := os.ReadFile(r.agents)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(got), errs.String()
}

// A SPEC THAT IMPORTS ITS INSTRUCTIONS HANDS EACH FILE AS AN IMPORT, NEVER ITS
// TEXT: under each checkout's header, one `@<absolute path>` line per slot's
// regular file, in slot order, so Claude Code reads the file itself and
// resolves its own `@AGENTS.md` against the checkout (measured on 2.1.310 and
// 2.1.330, B2A-C2-MEASURE rows ref-agents and agents). A space is written `\ `
// (measured: `my repo_x`); a link within the checkout is named by its target.
//
// Mutation: copy the text, skip the escape, or name the link itself, and a
// case goes red.
func TestASpecThatImportsItsInstructionsHandsEachFileAsAnImport(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, map[string]string{"repo/.git/": "", "repo/CLAUDE.md": "REPO-TEXT\n@AGENTS.md\n",
		"repo/docs/local.md": "LOCAL-TEXT\n", "my repo/.git/": "", "my repo/CLAUDE.md": "SPACED\n"})
	if err := os.Symlink("docs/local.md", filepath.Join(r.work, "repo", "CLAUDE.local.md")); err != nil {
		t.Fatal(err)
	}
	got, said := r.launchImporting(t)
	work := r.workdir(t)
	want := "for `my repo` with root " + filepath.Join(work, "my repo") + "\n\n@" + strings.ReplaceAll(work, " ", `\ `) +
		`/my\ repo/CLAUDE.md` + "\n\nfor `repo` with root " + filepath.Join(work, "repo") + "\n\n@" + work +
		"/repo/CLAUDE.md\n@" + work + "/repo/docs/local.md"
	if got != want || said != "" {
		t.Errorf("the launch wrote\n%q\nand said %q, want\n%q", got, said, want)
	}
	for _, text := range []string{"REPO-TEXT", "LOCAL-TEXT", "SPACED"} {
		if strings.Contains(got, text) {
			t.Errorf("the launch copied %s into the home's file", text)
		}
	}
}

// A FILE AN IMPORT MUST NOT NAME GETS NO IMPORT LINE: a directory or a FIFO
// is no slot's file, as on the text path; a link leading out of its checkout
// is left out said, and so is a path an import cannot name (`#` cuts it); the
// checkout's other files are still imported.
//
// Mutation: follow a link out of the checkout, or name a path holding `#`,
// and a case goes red.
func TestAFileAnImportMustNotNameGetsNoImportLine(t *testing.T) {
	t.Parallel()
	outside, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(outside, "out.md"), []byte("OUT\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := newRepoRun(t, map[string]string{"repo/.git/": "", "repo/CLAUDE.md/": "", "repo/CLAUDE.local.md": "L\n",
		"a#b/.git/": "", "a#b/CLAUDE.md": "HASH\n", "fifo/.git/": "", "fifo/CLAUDE.local.md": "F\n"})
	if err = syscall.Mkfifo(filepath.Join(r.work, "fifo", "CLAUDE.md"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Join(r.work, "repo", ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(filepath.Join(outside, "out.md"), filepath.Join(r.work, "repo", ".claude", "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	got, said := r.launchImporting(t)
	work := r.workdir(t)
	want := "for `fifo` with root " + filepath.Join(work, "fifo") + "\n\n@" + work + "/fifo/CLAUDE.local.md\n\n" +
		"for `repo` with root " + filepath.Join(work, "repo") + "\n\n@" + work + "/repo/CLAUDE.local.md"
	if got != want {
		t.Errorf("the launch wrote\n%q\nwant\n%q", got, want)
	}
	for _, line := range []string{
		"notehelper: the instructions " + filepath.Join(work, "repo", ".claude", "CLAUDE.md") +
			" are left out: it leads out of its checkout, to " + filepath.Join(outside, "out.md") + "\n",
		"notehelper: the instructions " + filepath.Join(work, "a#b", "CLAUDE.md") + " are left out: an import cannot name its path\n",
	} {
		if !strings.Contains(said, line) {
			t.Errorf("the launch said\n%s\nwithout\n%s", said, line)
		}
	}
}

// AN IMPORT NAMES A PATH ONLY WHERE CLAUDE CODE READS IT WHOLE: an escaped
// space and an underscore between letters or digits are kept in one text
// token; a backslash, `#`, another space, a control character, or what opens
// an inline markdown token mid-path is not.
//
// Mutation: allow any of them, and a row goes red.
func TestAnImportNamesAPathOnlyWhereClaudeCodeReadsItWhole(t *testing.T) {
	t.Parallel()
	for path, ok := range map[string]bool{
		"/w/my repo_x/CLAUDE.md": true, "/w/a-b.c+d@e,f=g%h:i(j)/CLAUDE.md": true, "/w/ré/CLAUDE.md": true,
		"/w/a\\b/CLAUDE.md": false, "/w/a#b/CLAUDE.md": false, "/w/a\tb/CLAUDE.md": false, "/w/a b/CLAUDE.md": false,
		"/w/a\x01b/CLAUDE.md": false, "/w/a`b/CLAUDE.md": false, "/w/a*b/CLAUDE.md": false, "/w/a~b/CLAUDE.md": false,
		"/w/a[b]/CLAUDE.md": false, "/w/a<b>/CLAUDE.md": false, "/w/a&b/CLAUDE.md": false, "/w/a!b/CLAUDE.md": false,
		"/w/_a_/CLAUDE.md": false, "/w/a_/CLAUDE.md": false, "/w/a_b/CLAUDE.md": true,
	} {
		if got := importable(path); got != ok {
			t.Errorf("importable(%q) = %v, want %v", path, got, ok)
		}
	}
}
