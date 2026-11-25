package executor

import (
	"strings"
	"testing"
)

// A MACHINE'S CODEX REQUIREMENTS CANNOT SWITCH THE NESTED HOOK OFF UNSEEN.
// Codex 0.171.0's /etc/codex/requirements.toml runs managed hooks alone where
// its top-level `allow_managed_hooks_only` is true (read in the binary: a
// field of its requirements, root's file), so the guard's answer holding it
// refuses the run, naming the file and the key; false, the key under another
// table, no file and a file that is no TOML start it. Mutations, each red:
// skip the requirements; read the key under any table.
func TestACodexMachinesRequirementsCannotSwitchTheNestedHookOffUnseen(t *testing.T) {
	t.Parallel()
	read := `{"id":2,"result":{"config":{"features":{}},"layers":[{"name":{"type":"sessionFlags"},"config":{}}]}}`
	for name, tc := range map[string]struct {
		requirements string
		refused      bool
	}{
		"managed hooks alone":                 {"allow_managed_hooks_only = true\n", true},
		"managed hooks alone, no layers read": {"allow_managed_hooks_only = true\n", true},
		"the key false":                       {"allow_managed_hooks_only = false\n", false},
		"the key under another table":         {"[hooks]\nallow_managed_hooks_only = true\n", false},
		"no file":                             {"", false},
		"no TOML":                             {"allow_managed_hooks_only = \n", false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out := read + "\n\n" + codexRequirementsMark + "\n" + tc.requirements
			if strings.HasSuffix(name, "no layers read") {
				out = "\n" + codexRequirementsMark + "\n" + tc.requirements
			}
			err := codexHooksVerdict(out, "/home/box/.codex/config.toml")
			if (err != nil) != tc.refused {
				t.Fatalf("refused %v, want %t", err, tc.refused)
			}
			if tc.refused && !strings.Contains(err.Error(), "/etc/codex/requirements.toml sets allow_managed_hooks_only = true") {
				t.Errorf("the refusal does not name the file and the key: %v", err)
			}
		})
	}
}
