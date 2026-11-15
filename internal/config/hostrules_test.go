package config

import (
	"strings"
	"testing"
)

// A CLAUDE CODE EXECUTOR THAT RUNS AS ROOT NEVER LAUNCHES, so a root engine
// refuses the document, in the cells whose box runs as the engine's own user
// and in no other: Claude Code rejects bypassPermissions for uid 0, so every
// launch on the entry fails at its seat's first work.
func TestARootEngineRefusesAClaudeCodeExecutorThatRunsAsRoot(t *testing.T) {
	t.Parallel()
	const roles = "roles:\n  - name: Founder\n    kind: human\n    contact: {slack_user_id: U0FOUNDER}\n  - name: Agent\n"
	for _, tc := range []struct {
		name, doc string
		euid      int
		refused   bool
	}{
		{"direct as root", "providers:\n  llm:\n    default: {harness: claude-code, run_in: direct}\n  sandbox: {local: {image: img}}\n", 0, true},
		{"container as root", "providers:\n  llm:\n    default: {harness: claude-code, run_in: container}\n  sandbox: {local: {image: img}}\n", 0, true},
		{"cell from the default", "providers:\n  llm:\n    default: {harness: claude-code}\n  sandbox: {local: {}, default_run_in: direct}\n", 0, true},
		{"direct as a user", "providers:\n  llm:\n    default: {harness: claude-code, run_in: direct}\n  sandbox: {local: {}}\n", 1000, false},
		{"e2b as root", "providers:\n  llm:\n    default: {harness: claude-code, run_in: e2b}\n  sandbox: {e2b: {}}\n", 0, false},
		{"opencode as root", "providers:\n  llm:\n    default: {harness: opencode, run_in: direct}\n  sandbox: {local: {}}\n", 0, false},
		{"double as root", "providers:\n  llm:\n    default: {harness: claude-code}\n  sandbox: {fake: true}\n", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := ParseCompanyDocument([]byte("name: Acme\n" + tc.doc + roles))
			if err != nil {
				t.Fatal(err)
			}
			err = cfg.ValidateHost(tc.euid)
			switch {
			case tc.refused && err == nil:
				t.Fatal("a root engine accepted a Claude Code executor that runs as root")
			case tc.refused && !strings.Contains(err.Error(), "providers.llm.default.run_in"):
				t.Fatalf("the refusal does not name the field to change: %v", err)
			case !tc.refused && err != nil:
				t.Fatalf("refused a launch that works: %v", err)
			}
		})
	}
}
