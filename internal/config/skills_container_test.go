package config

import "testing"

// THE OFF SWITCH IS THE WHOLE POINT of the field being a pointer: a company
// whose ordinary work container happens to be named TS would otherwise have it
// silently dropped from every knowledge search and every routing decision,
// with no way in the config to say so.
func TestTheSkillsContainerIsThreeValued(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{"unset takes the reserved default", "", DefaultSkillsContainer},
		{"a named container is upper-cased", "knowledge:\n  skills_container: skills\n", "SKILLS"},
		{"an explicit empty string is off", "knowledge:\n  skills_container: \"\"\n", ""},
		{"whitespace is the same answer as empty", "knowledge:\n  skills_container: \"   \"\n", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := mustCompany(t, "name: Acme\n"+tc.yaml)
			if got := cfg.SkillsContainerKey(); got != tc.want {
				t.Errorf("skills container = %q, want %q", got, tc.want)
			}
		})
	}

	// NO KNOWLEDGE BASE AT ALL IS NOT THE DEFAULT. A company that switched
	// the backend off has no container to name, and answering "TS" would
	// have the engine watching a container nothing holds.
	t.Run("no backend answers nothing", func(t *testing.T) {
		t.Parallel()
		cfg := mustCompany(t, "name: Acme\nknowledge:\n  backend: none\n")
		if got := cfg.SkillsContainerKey(); got != "" {
			t.Errorf("a company with no knowledge base answered %q", got)
		}
		if got := cfg.RootSpaceKey(); got != "" {
			t.Errorf("a company with no knowledge base has a root space %q", got)
		}
	})
}
