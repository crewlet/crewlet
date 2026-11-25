package executor

import (
	"strings"
	"testing"
)

// A MACHINE'S CODEX REQUIREMENTS SWITCH THE NESTED HOOK OFF, NAMED. Codex
// 0.171.0's /etc/codex/requirements.toml runs managed hooks alone where its
// top-level `allow_managed_hooks_only` is true (read in the binary: a field of
// its requirements, root's file), so the guard's answer holding it names the
// file and the key as switching the hook off ([codexHooksSwitchedOff]);
// false, the key under another table, no file and a file that is no TOML
// switch nothing. Mutations, each red: skip the requirements; read the key
// under any table.
func TestACodexMachinesRequirementsSwitchTheNestedHookOffNamed(t *testing.T) {
	t.Parallel()
	read := `{"id":2,"result":{"config":{"features":{}},"layers":[{"name":{"type":"sessionFlags"},"config":{}}]}}`
	for name, tc := range map[string]struct {
		requirements string
		off          bool
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
			off := codexHooksSwitchedOff(out, "/home/box/.codex/config.toml")
			if (len(off) > 0) != tc.off {
				t.Fatalf("switched off by %q, want %t", off, tc.off)
			}
			if tc.off && !strings.Contains(strings.Join(off, "; "), "/etc/codex/requirements.toml sets allow_managed_hooks_only = true") {
				t.Errorf("the switch does not name the file and the key: %q", off)
			}
		})
	}
}
