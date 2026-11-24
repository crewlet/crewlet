package harness

import (
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// A KIMI CODE KEY GOES BESIDE ITS MODEL: under api-key the harness is signed in
// by a model of its own (KIMI_MODEL_NAME with KIMI_MODEL_API_KEY), and a
// --model beside it fails, so the entry's model goes in the variable and not on
// argv. Under a login the model stays on argv and neither variable reaches the
// harness, whatever the entry's env carried.
//
// Mutation: keep model_args under api-key, and every keyed call fails; drop
// applyAuth's removal, and an env pair swaps a login entry's model.
func TestAKimiCodeKeyGoesBesideItsModel(t *testing.T) {
	t.Parallel()
	home := func() *Checkout { return &Checkout{Home: t.TempDir(), Cache: t.TempDir(), Work: t.TempDir()} }

	keyed, err := New(Config{Key: "e", Harness: "kimi-code", Model: "kimi-k3", Effort: "high",
		Auth: Auth{Mode: AuthAPIKey, APIKey: "sk-test"}, StateDir: t.TempDir(), Timeout: time.Minute})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, tools := range []bool{false, true} {
		if argv := keyed.argv(tools); slices.Contains(argv, "--model") {
			t.Errorf("api-key (tools %v): argv carries --model: %q", tools, argv)
		}
	}
	env := buildEnv(keyed.profile, home(), keyed.callEnv(), keyed.auth)
	for _, want := range []string{"KIMI_MODEL_NAME=kimi-k3", "KIMI_MODEL_API_KEY=sk-test", "KIMI_MODEL_THINKING_EFFORT=high"} {
		if !slices.Contains(env, want) {
			t.Errorf("api-key: the call's environment lacks %s", want)
		}
	}

	login, err := New(Config{Key: "e", Harness: "kimi-code", Model: "kimi-k3",
		Env:      map[string]string{"KIMI_MODEL_NAME": "other", "KIMI_MODEL_API_KEY": "sk-planted"},
		StateDir: t.TempDir(), Timeout: time.Minute})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if argv := login.argv(false); !slices.Equal(argv[len(argv)-2:], []string{"--model", "kimi-k3"}) {
		t.Errorf("subscription: argv ends %q, want --model kimi-k3", argv[max(0, len(argv)-2):])
	}
	for _, kv := range buildEnv(login.profile, home(), login.callEnv(), login.auth) {
		if slices.Contains([]string{"KIMI_MODEL_NAME=other", "KIMI_MODEL_API_KEY=sk-planted"}, kv) {
			t.Errorf("subscription: the call's environment carries %s", kv)
		}
	}
}

// A KIMI CODE LOGIN IS WHERE 2.7.3 KEEPS IT: `kimi login` files its token under
// its storage key's last part, credentials/kimi-code.json, and a run reads it
// there and writes a refreshed token back over it (measured on 2.7.3: a token
// at credentials/oauth/kimi-code.json is "no credential configured"). The
// login is carried both ways at that path, beside config.toml.
//
// Mutation: name credentials/oauth/kimi-code.json, or drop either file from
// either list — the case fails.
func TestAKimiCodeLoginIsCarriedWhereTheHarnessKeepsIt(t *testing.T) {
	t.Parallel()
	p, ok := harnessprofile.Builtin("kimi-code")
	if !ok {
		t.Fatal("no built-in kimi-code profile")
	}
	want := []string{".kimi-code/credentials/kimi-code.json", ".kimi-code/config.toml"}
	if !slices.Equal(p.CredentialPaths, want) || !slices.Equal(p.HostCredentialPaths, want) {
		t.Errorf("credential_paths = %q, host_credential_paths = %q, want %q", p.CredentialPaths,
			p.HostCredentialPaths, want)
	}
}
