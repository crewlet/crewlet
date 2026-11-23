package harness

import (
	"maps"
	"testing"

	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// AN ENTRY'S ID MOVES WITH WHERE ITS CALLS GO, AND WITH NOTHING ELSE OF ITS
// ENV. A carried session continues only under the entry it ran under
// (internal/sandbox's EntryContinues), so the id tells apart two entries of one
// harness, and one entry whose auth mode, route, cloud switch or model's
// provider moved. Its env is secret whole, so a credential under any name — a
// header, a URL's userinfo or query, a gateway's oddly named key — and a
// tuning variable leave the id as it was: the id is built from the route and
// sign-in mode alone.
//
// Mutation: drop the key, the auth mode, the route variables or OpenCode's model
// provider from the digest, keep a URL's userinfo or query, or digest any other
// variable of the env, and a row goes red.
func TestAnEntrysIDMovesWithWhereItsCallsGoAndWithNothingElseOfItsEnv(t *testing.T) {
	t.Parallel()
	claude, ok := harnessprofile.Builtin("claude-code")
	if !ok {
		t.Fatal("no built-in claude-code profile")
	}
	opencode, ok := harnessprofile.Builtin("opencode")
	if !ok {
		t.Fatal("no built-in opencode profile")
	}
	gateway := map[string]string{"ANTHROPIC_BASE_URL": "https://gateway-a.example.com/v1",
		"ANTHROPIC_AUTH_TOKEN": "sk-ant-placeholder-a"}
	entry := func(key string, mode AuthMode, edit func(map[string]string)) *Provider {
		env := maps.Clone(gateway)
		if edit != nil {
			edit(env)
		}
		return &Provider{key: key, harness: "claude-code", model: "claude-sonnet-5", profile: claude,
			auth: Auth{Mode: mode, APIKey: "sk-ant-placeholder-key"}, env: env}
	}
	id := entry("primary", AuthEnv, nil).EntryID()
	if len(id) != entryIDChars {
		t.Fatalf("EntryID = %q, want %d hex characters", id, entryIDChars)
	}
	set := func(name, value string) func(map[string]string) {
		return func(env map[string]string) { env[name] = value }
	}
	for name, tc := range map[string]struct {
		p    *Provider
		same bool
	}{
		"the same entry built again":        {entry("primary", AuthEnv, nil), true},
		"its gateway token replaced":        {entry("primary", AuthEnv, set("ANTHROPIC_AUTH_TOKEN", "sk-ant-b")), true},
		"a bearer in its custom headers":    {entry("primary", AuthEnv, set("ANTHROPIC_CUSTOM_HEADERS", "Authorization: Bearer planted-1")), true},
		"a bearer in its telemetry headers": {entry("primary", AuthEnv, set("OTEL_EXPORTER_OTLP_HEADERS", "authorization=Bearer planted-2")), true},
		"its gateway's address with userinfo": {entry("primary", AuthEnv,
			set("ANTHROPIC_BASE_URL", "https://user:planted-3@gateway-a.example.com/v1")), true},
		"its gateway's address with a key in its query": {entry("primary", AuthEnv,
			set("ANTHROPIC_BASE_URL", "https://gateway-a.example.com/v1?key=planted-4")), true},
		"an oddly named gateway credential": {entry("primary", AuthEnv, set("GW_SESAME", "planted-5")), true},
		"a tuning variable":                 {entry("primary", AuthEnv, set("MAX_THINKING_TOKENS", "8000")), true},
		"another entry on the same route":   {entry("secondary", AuthEnv, nil), false},
		"its gateway moved": {entry("primary", AuthEnv,
			set("ANTHROPIC_BASE_URL", "https://gateway-b.example.com/v1")), false},
		"its region moved":        {entry("primary", AuthEnv, set("AWS_REGION", "eu-west-1")), false},
		"its cloud switch thrown": {entry("primary", AuthEnv, set("CLAUDE_CODE_USE_BEDROCK", "1")), false},
		"its auth mode moved":     {entry("primary", AuthAPIKey, nil), false},
	} {
		if got := tc.p.EntryID(); (got == id) != tc.same {
			t.Errorf("%s: EntryID = %q beside %q, want the same: %v", name, got, id, tc.same)
		}
	}
	// THE PROVIDER OPENCODE'S MODEL NAMES is its route; the model past it is not.
	oc := func(model string) *Provider {
		return &Provider{key: "oc", harness: "opencode", model: model, profile: opencode}
	}
	if oc("anthropic/claude-sonnet-5").EntryID() == oc("openrouter/anthropic/claude-sonnet-5").EntryID() {
		t.Error("an OpenCode entry whose model names another provider keeps its id")
	}
	if oc("openrouter/openai/gpt-6").EntryID() != oc("openrouter/anthropic/claude-sonnet-5").EntryID() {
		t.Error("an OpenCode entry whose model moved within its provider took a new id")
	}
}
