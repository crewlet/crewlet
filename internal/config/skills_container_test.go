package config

import "testing"

// THE OFF SWITCH IS THE WHOLE POINT of the field being a pointer: a company
// whose ordinary work project happens to be named TS would otherwise have it
// silently dropped from every knowledge search and every routing decision,
// with no way in the config to say so.
func TestTheSkillsContainerIsThreeValued(t *testing.T) {
	t.Parallel()
	t.Run("plane", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name string
			yaml string
			want string
		}{
			{"unset takes the reserved default", "", DefaultSkillsProject},
			{"a named project is upper-cased", "\n    skills_project: skills", "SKILLS"},
			{"an explicit empty string is off", `
    skills_project: ""`, ""},
			{"whitespace is the same answer as empty", "\n    skills_project: \"   \"", ""},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				cfg := mustCompany(t, `
name: Acme
integrations:
  plane:
    url: "https://plane.example.com"
    workspace: acme
    token: "${T}"
    webhook_secret: "${S}"`+tc.yaml+"\n")
				if got := cfg.Integrations.Plane.SkillsProjectKey(); got != tc.want {
					t.Errorf("skills project = %q, want %q", got, tc.want)
				}
			})
		}
	})

	// THE TWO BACKENDS HAVE TO AGREE, or a company that migrated from one to
	// the other would lose its off switch in the move and not find out until
	// a planner quoted a tool skill at it.
	t.Run("confluence", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name string
			yaml string
			want string
		}{
			{"unset takes the reserved default", "", DefaultSkillsSpace},
			{"a named space is upper-cased", "\n    skills_space: skills", "SKILLS"},
			{"an explicit empty string is off", `
    skills_space: ""`, ""},
			{"whitespace is the same answer as empty", "\n    skills_space: \"   \"", ""},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				cfg := mustCompany(t, `
name: Acme
integrations:
  confluence:
    url: "https://wiki.example.com"
    token: "${T}"
    webhook_secret: "${S}"`+tc.yaml+"\n")
				if got := cfg.Integrations.Confluence.SkillsSpaceKey(); got != tc.want {
					t.Errorf("skills space = %q, want %q", got, tc.want)
				}
			})
		}
	})

	// NO BACKEND AT ALL IS NOT THE DEFAULT. A nil block has no container to
	// name, and answering "TS" would have the engine watching a project on an
	// instance the company does not run.
	t.Run("no backend answers nothing", func(t *testing.T) {
		t.Parallel()
		var plane *Plane
		if got := plane.SkillsProjectKey(); got != "" {
			t.Errorf("a nil plane answered %q", got)
		}
		var cf *Confluence
		if got := cf.SkillsSpaceKey(); got != "" {
			t.Errorf("a nil confluence answered %q", got)
		}
	})
}
