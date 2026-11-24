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
// cut a section at a mark inside it.
func TestAManagedPolicySwitchingClaudeCodesHooksOffRefusesTheRun(t *testing.T) {
	t.Parallel()
	answer := func(files ...[2]string) string {
		var b strings.Builder
		for _, f := range files {
			b.WriteString(claudeHooksOffMark + strconv.Itoa(len(f[1])) + " " + f[0] + "\n" + f[1])
		}
		return b.String()
	}
	const repo = "/home/u/workspace/.claude/settings.json"
	const managed = "/etc/claude-code/managed-settings.json"
	const dropIn = "/etc/claude-code/managed-settings.d/50-hooks.json"
	for name, tc := range map[string]struct {
		files [][2]string
		names string
	}{
		"a managed true over the repository's false": {[][2]string{{repo, `{"disableAllHooks": false}`},
			{managed, `{"disableAllHooks": true}`}}, managed + " sets disableAllHooks to true"},
		"a drop-in's false after the managed file's true": {[][2]string{{managed, `{"disableAllHooks": true}`},
			{dropIn, `{"disableAllHooks": false}`}}, ""},
		"managed hooks alone": {[][2]string{{dropIn, `{"allowManagedHooksOnly": true}`}},
			dropIn + " sets allowManagedHooksOnly to true"},
		"managed hooks alone, outside a managed file": {[][2]string{{repo, `{"allowManagedHooksOnly": true}`}}, ""},
		"a file holding the mark in its bytes": {[][2]string{
			{repo, `{"env": {"X": "` + claudeHooksOffMark + `9 /x"}, "disableAllHooks": true}`}},
			repo + " sets disableAllHooks to true"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := claudeHooksOffVerdict(claudeHooksOffParse(answer(tc.files...)))
			if tc.names == "" && got != "" {
				t.Fatalf("a run nothing switches off is refused: %s", got)
			}
			if !strings.Contains(got, tc.names) {
				t.Fatalf("the refusal does not say %q: %q", tc.names, got)
			}
		})
	}
}
