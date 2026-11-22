package executor

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
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
	removes := func(list, path string) bool { return slices.Contains(strings.Fields(list), sandbox.ShellQuote(path)) }
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
