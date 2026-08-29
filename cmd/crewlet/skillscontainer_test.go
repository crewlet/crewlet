package main

import "testing"

// THE PRECEDENCE IS THE CONTRACT: flag, then variable, then config. An
// operator who passes -space expects it to win over a variable they exported
// last week and forgot about.
func TestTheSkillsContainerPrefersTheFlagThenTheVariable(t *testing.T) {
	cases := []struct {
		name       string
		flag       string
		env        string
		fromConfig string
		want       string
	}{
		{"the flag wins", "one", "two", "THREE", "ONE"},
		{"the variable fills in for an absent flag", "", "two", "THREE", "TWO"},
		{"the config answers when neither is set", "", "", "THREE", "THREE"},
		{"an off config stays off", "", "", "", ""},
		{"the flag overrides an off config", "one", "", "", "ONE"},
		{"the variable overrides an off config", "", "two", "", "TWO"},
		{"a blank flag is not an answer", "   ", "two", "THREE", "TWO"},
		{"a blank variable is not an answer", "", "   ", "THREE", "THREE"},
		{"every source is upper-cased", "lower", "", "THREE", "LOWER"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// NOT PARALLEL: t.Setenv and parallel subtests are mutually
			// exclusive, and the variable is the thing under test.
			t.Setenv("CREWLET_TOOL_SKILLS_SPACE", tc.env)
			got := skillsContainer(tc.flag, "CREWLET_TOOL_SKILLS_SPACE", tc.fromConfig)
			if got != tc.want {
				t.Errorf("skillsContainer(%q, env=%q, %q) = %q, want %q",
					tc.flag, tc.env, tc.fromConfig, got, tc.want)
			}
		})
	}
}
