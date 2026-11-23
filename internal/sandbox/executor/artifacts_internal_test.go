package executor

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// A REUSED BOX KEEPS NO HARNESS'S ARTIFACTS, whichever runner clears it: the
// clear removes every registered harness's files one by one (Codex's
// configuration, socket and routes among them) and every harness's
// directories whole (Claude Code's skills plugin, OpenCode's values and
// plugins), each from the interface that names it, a harness naming none
// (Codex) leaving nothing to remove whole.
//
// Mutation: skip artifactDirHolder in ClearArtifacts, skip artifactHolder, or
// remove a directory with `rm -f`, and a case goes red.
func TestAReusedBoxKeepsNoHarnesssFilesOrDirectories(t *testing.T) {
	t.Parallel()
	box := sandbox.NewFakeSandbox("box-1")
	p := PathsFor(box)
	if err := NewCodex().ClearArtifacts(t.Context(), box); err != nil {
		t.Fatalf("ClearArtifacts: %v", err)
	}
	execs := box.Execs()
	if len(execs) == 0 {
		t.Fatal("the clear ran nothing in the box")
	}
	clear := execs[len(execs)-1].Command
	files, rest, ok := strings.Cut(strings.TrimPrefix(clear, "rm -f "), "; rm -rf ")
	if !ok {
		t.Fatalf("the clear is not `rm -f <files>; rm -rf <dirs>; …`:\n%s", clear)
	}
	dirs, _, _ := strings.Cut(rest, "; ")
	removes := func(list, path string) bool { return slices.Contains(strings.Fields(list), shellword.Quote(path)) }
	for _, path := range (Codex{}).artifactFiles(p) {
		if !removes(files, path) {
			t.Errorf("the clear leaves Codex's %s:\n%s", path, clear)
		}
	}
	for _, path := range append((ClaudeCode{}).artifactDirs(p), (OpenCode{}).artifactDirs(p)...) {
		if !removes(dirs, path) {
			t.Errorf("the clear does not remove the directory %s whole:\n%s", path, clear)
		}
	}
	if _, names := any(Codex{}).(artifactDirHolder); names {
		t.Error("Codex names directories to clear, which its skills' shared root must never be")
	}
}

// KIMI CODE'S, COPILOT'S, MUSE CODE'S, QWEN CODE'S AND HERMES'S ARTIFACTS ARE
// CLEARED, each split as the contract names them: their files one by one
// (Kimi's driver and settings, Copilot's instructions and tool environment,
// Muse's note helper and take configs, Qwen's settings and helper, Hermes's
// overlay and records) and their directories whole (Kimi's profile directory,
// Copilot's extension, Muse's MCP servers and plugin, Qwen's skills, Hermes's
// staging), whichever runner clears the box.
// Named through the old (files, dirs) method, none satisfied the contract and
// the clear left their files behind.
//
// Mutation: drop any of their artifactFiles or artifactDirs, and its row goes
// red. A directory among its files is TestARunnersArtifactFilesAndDirectoriesAreDisjoint.
func TestAReusedBoxKeepsNoneOfASplitRunnersArtifacts(t *testing.T) {
	t.Parallel()
	box := sandbox.NewFakeSandbox("box-1")
	p := PathsFor(box)
	if err := NewClaudeCode().ClearArtifacts(t.Context(), box); err != nil {
		t.Fatalf("ClearArtifacts: %v", err)
	}
	execs := box.Execs()
	clear := execs[len(execs)-1].Command
	files, rest, _ := strings.Cut(strings.TrimPrefix(clear, "rm -f "), "; rm -rf ")
	dirs, _, _ := strings.Cut(rest, "; ")
	removes := func(list, path string) bool { return slices.Contains(strings.Fields(list), shellword.Quote(path)) }
	for _, c := range []struct {
		name  string
		files []string
		dirs  []string
	}{
		{"kimi-code", (Kimi{}).artifactFiles(p), (Kimi{}).artifactDirs(p)},
		{"copilot", (Copilot{}).artifactFiles(p), (Copilot{}).artifactDirs(p)},
		{"muse-code", (Muse{}).artifactFiles(p), (Muse{}).artifactDirs(p)},
		{"qwen-code", (QwenCode{}).artifactFiles(p), (QwenCode{}).artifactDirs(p)},
		{"hermes", (Hermes{}).artifactFiles(p), (Hermes{}).artifactDirs(p)},
	} {
		if len(c.files) == 0 || len(c.dirs) == 0 {
			t.Errorf("%s names files %v and directories %v, want both", c.name, c.files, c.dirs)
		}
		for _, path := range c.files {
			if !removes(files, path) {
				t.Errorf("the clear leaves %s's file %s", c.name, path)
			}
		}
		for _, path := range c.dirs {
			if !removes(dirs, path) {
				t.Errorf("the clear does not remove %s's directory %s whole", c.name, path)
			}
		}
	}
}

// A RUNNER NAMES EACH ARTIFACT ONCE, A FILE OR A DIRECTORY: the clear removes
// its files one by one and its directories whole, so a directory listed among
// its files too is cleared by the wrong rule beside the right one, and a test
// of either list alone passes it. Every registered runner that names both
// keeps them disjoint.
//
// Mutation: list one of any runner's directories among its files, and its
// row goes red.
func TestARunnersArtifactFilesAndDirectoriesAreDisjoint(t *testing.T) {
	t.Parallel()
	p := PathsFor(sandbox.NewFakeSandbox("box-1"))
	both := 0
	for _, h := range Registered() {
		files, ok := h.(artifactHolder)
		dirs, okDirs := h.(artifactDirHolder)
		if !ok || !okDirs {
			continue
		}
		both++
		for _, dir := range dirs.artifactDirs(p) {
			if slices.Contains(files.artifactFiles(p), dir) {
				t.Errorf("%s names %s both a file and a directory", h.Name(), dir)
			}
		}
	}
	if both == 0 {
		t.Fatal("no registered runner names both files and directories, so nothing was held")
	}
}
