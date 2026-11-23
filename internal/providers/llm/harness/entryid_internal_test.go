package harness

import (
	"testing"

	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// AN ENTRY'S ID MOVES WITH WHERE ITS CALLS GO, AND NEVER WITH A CREDENTIAL. A
// carried session continues only under the entry it ran under (internal/
// sandbox's EntryContinues), so the id must tell apart two entries of one
// harness, and one entry whose auth mode, route or model's provider moved; and
// a credential replaced under the same entry must keep it, as a refusal on a
// replaced sign-in relaunches its round in the same session.
//
// Mutation: drop the key, the auth mode, the env, or OpenCode's model provider
// from the digest, or digest a credential, and a row goes red.
func TestAnEntrysIDMovesWithWhereItsCallsGoAndNeverWithACredential(t *testing.T) {
	t.Parallel()
	claude, ok := harnessprofile.Builtin("claude-code")
	if !ok {
		t.Fatal("no built-in claude-code profile")
	}
	opencode, ok := harnessprofile.Builtin("opencode")
	if !ok {
		t.Fatal("no built-in opencode profile")
	}
	entry := func(key string, profile harnessprofile.Profile, harness, model string, auth Auth,
		env map[string]string) *Provider {
		return &Provider{key: key, harness: harness, model: model, profile: profile, auth: auth, env: env}
	}
	gateway := map[string]string{"ANTHROPIC_BASE_URL": "https://gateway-a.example.com",
		"ANTHROPIC_AUTH_TOKEN": "sk-ant-placeholder-a"}
	base := entry("primary", claude, "claude-code", "claude-sonnet-5", Auth{Mode: AuthEnv}, gateway)
	id := base.EntryID()
	if len(id) != entryIDChars {
		t.Fatalf("EntryID = %q, want %d hex characters", id, entryIDChars)
	}
	for name, tc := range map[string]struct {
		p    *Provider
		same bool
	}{
		"the same entry built again": {entry("primary", claude, "claude-code", "claude-sonnet-5",
			Auth{Mode: AuthEnv}, map[string]string{"ANTHROPIC_BASE_URL": "https://gateway-a.example.com",
				"ANTHROPIC_AUTH_TOKEN": "sk-ant-placeholder-a"}), true},
		"its gateway token replaced": {entry("primary", claude, "claude-code", "claude-sonnet-5",
			Auth{Mode: AuthEnv}, map[string]string{"ANTHROPIC_BASE_URL": "https://gateway-a.example.com",
				"ANTHROPIC_AUTH_TOKEN": "sk-ant-placeholder-b"}), true},
		"its model moved": {entry("primary", claude, "claude-code", "claude-opus-5", Auth{Mode: AuthEnv},
			gateway), true},
		"another entry on the same route": {entry("secondary", claude, "claude-code", "claude-sonnet-5",
			Auth{Mode: AuthEnv}, gateway), false},
		"its gateway moved": {entry("primary", claude, "claude-code", "claude-sonnet-5", Auth{Mode: AuthEnv},
			map[string]string{"ANTHROPIC_BASE_URL": "https://gateway-b.example.com",
				"ANTHROPIC_AUTH_TOKEN": "sk-ant-placeholder-a"}), false},
		"its auth mode moved": {entry("primary", claude, "claude-code", "claude-sonnet-5",
			Auth{Mode: AuthAPIKey, APIKey: "sk-ant-placeholder-a"}, gateway), false},
	} {
		if got := tc.p.EntryID(); (got == id) != tc.same {
			t.Errorf("%s: EntryID = %q beside %q, want the same: %v", name, got, id, tc.same)
		}
	}
	// THE PROVIDER OPENCODE'S MODEL NAMES is its route; the model past it is not.
	oc := entry("oc", opencode, "opencode", "openrouter/anthropic/claude-sonnet-5", Auth{}, nil)
	if moved := entry("oc", opencode, "opencode", "anthropic/claude-sonnet-5", Auth{}, nil); moved.EntryID() ==
		oc.EntryID() {
		t.Error("an OpenCode entry whose model names another provider keeps its id")
	}
	if kept := entry("oc", opencode, "opencode", "openrouter/openai/gpt-6", Auth{}, nil); kept.EntryID() !=
		oc.EntryID() {
		t.Error("an OpenCode entry whose model moved within its provider took a new id")
	}
}
