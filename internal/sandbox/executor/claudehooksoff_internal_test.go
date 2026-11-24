package executor

import (
	"strconv"
	"strings"
	"testing"
)

// A MACHINE'S POLICY CANNOT SWITCH A CLAUDE CODE RUN'S HOOKS OFF UNSEEN
// EITHER. Managed settings rank above every other file (measured on 2.1.330),
// a drop-in after the main file (read from its loader), so their
// `disableAllHooks` decides over a repository's false, and their
// `allowManagedHooksOnly` runs no hook of the run's; that key anywhere else is
// ignored (measured). The answer is read section by
// section, a file whose bytes hold the mark kept whole. Mutations, each red:
// read allowManagedHooksOnly outside managed files; let the first file decide;
// cut a section at a mark inside it; hold the lock to a run without notes;
// read a lock on other surfaces or outside managed files.
func TestAManagedPolicySwitchingClaudeCodesHooksOffRefusesTheRun(t *testing.T) {
	t.Parallel()
	answer := func(files ...[2]string) string {
		var b strings.Builder
		for _, f := range files {
			b.WriteString(boxFilesMark + strconv.Itoa(len(f[1])) + " " + f[0] + "\n" + f[1])
		}
		return b.String()
	}
	const repo = "/home/u/workspace/.claude/settings.json"
	const managed = "/etc/claude-code/managed-settings.json"
	const dropIn = "/etc/claude-code/managed-settings.d/50-hooks.json"
	for name, tc := range map[string]struct {
		files [][2]string
		notes bool
		names string
	}{
		"a managed true over the repository's false": {files: [][2]string{{repo, `{"disableAllHooks": false}`},
			{managed, `{"disableAllHooks": true}`}}, names: managed + " sets disableAllHooks to true"},
		"a drop-in's false after the managed file's true": {files: [][2]string{{managed, `{"disableAllHooks": true}`},
			{dropIn, `{"disableAllHooks": false}`}}},
		"managed hooks alone": {files: [][2]string{{dropIn, `{"allowManagedHooksOnly": true}`}},
			names: dropIn + " sets allowManagedHooksOnly to true"},
		"managed hooks alone, outside a managed file": {files: [][2]string{{repo, `{"allowManagedHooksOnly": true}`}}},
		"a managed lock on hooks, the notes riding them": {files: [][2]string{
			{dropIn, `{"strictPluginOnlyCustomization": ["mcp", "hooks"]}`}}, notes: true,
			names: dropIn + ` sets strictPluginOnlyCustomization to ["mcp","hooks"]`},
		"a managed lock Claude Code reads as on": {files: [][2]string{{managed, `{"strictPluginOnlyCustomization": "x"}`}},
			notes: true, names: managed + ` sets strictPluginOnlyCustomization to "x"`},
		"a managed lock on other surfaces": {files: [][2]string{
			{managed, `{"strictPluginOnlyCustomization": ["mcp"]}`}}, notes: true},
		"a managed lock on hooks, no notes riding them": {files: [][2]string{
			{managed, `{"strictPluginOnlyCustomization": true}`}}},
		"a lock outside managed settings": {files: [][2]string{{repo, `{"strictPluginOnlyCustomization": true}`}},
			notes: true},
		"a file holding the mark in its bytes": {files: [][2]string{
			{repo, `{"env": {"X": "` + boxFilesMark + `9 /x"}, "disableAllHooks": true}`}},
			names: repo + " sets disableAllHooks to true"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := claudeHooksOffVerdict(boxFilesParse(answer(tc.files...)), tc.notes)
			if tc.names == "" && got != "" {
				t.Fatalf("a run nothing switches off is refused: %s", got)
			}
			if !strings.Contains(got, tc.names) {
				t.Fatalf("the refusal does not say %q: %q", tc.names, got)
			}
		})
	}
}
