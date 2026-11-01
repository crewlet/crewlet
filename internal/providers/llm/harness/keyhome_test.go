package harness

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm"
)

// keySessionProvider is an entry on dir whose harness is the fake writing its key
// session into its login's own file ([runFakeHarness], FAKE_KEY_SESSION), under
// auth.
func keySessionProvider(t *testing.T, dir string, auth Auth) *Provider {
	t.Helper()
	p, err := New(Config{
		Key: "sub", Harness: "custom", StateDir: dir,
		Overrides: map[string]any{
			"binary":           os.Args[0],
			"complete_args":    []any{"-test.run=TestFakeHarness"},
			"model_args":       []any{},
			"output":           "text",
			"prompt_mode":      "stdin",
			"api_key_env":      "FAKE_VENDOR_API_KEY",
			"credential_paths": []any{".vendor/auth.json"},
		},
		Timeout: 20 * time.Second, MaxConcurrent: 2,
		Env:  fakeChildEnv(map[string]string{"FAKE_KEY_SESSION": ".vendor/auth.json"}),
		Auth: auth,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// A KEY NEVER BECOMES THE LOGIN OF RECORD, AND NEVER OUTLIVES ITS CALLS. Cursor
// Agent writes what CURSOR_API_KEY signed it in to — the key among it — into
// the auth.json its login lives in, and prefers that file to the variable on
// its next start. Run in the login home, that file was carried back over the
// shared login, and every subscription seat on the workspace signed in with the
// key; left in the home, it went on signing the seat in after the entry moved
// to subscription. A call under api-key runs in a home of its own, with no
// login seeded in it and nothing carried out of it.
func TestAKeyNeverBecomesTheLoginOfRecord(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	t.Cleanup(func() { forgetWorkspace(dir) })
	keyed := keySessionProvider(t, dir, Auth{Mode: AuthAPIKey, APIKey: "sk-test-key"})
	plan := keySessionProvider(t, dir, Auth{Mode: AuthSubscription})

	shared := filepath.Join(keyed.ws.CredentialsDir(), "auth.json")
	if err := os.MkdirAll(filepath.Dir(shared), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shared, []byte("the login of record"), 0o600); err != nil {
		t.Fatal(err)
	}

	// What a process that ended mid-call left: no release ran to drop it.
	left := filepath.Join(keyed.ws.KeySeatDir("shared"), "home", ".vendor", "auth.json")
	if err := os.MkdirAll(filepath.Dir(left), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(left, []byte("a session an earlier engine left"), 0o600); err != nil {
		t.Fatal(err)
	}

	for i := range 2 {
		comp, err := ask(t, keyed, llm.Request{})
		if err != nil {
			t.Fatalf("key call %d: %v", i, err)
		}
		if comp.Content != "nothing" {
			// The first: the login seeded beside the key, or what the
			// earlier engine left. The second: the key the first call
			// wrote down, kept.
			t.Errorf("key call %d started on %q, want no file at all", i, comp.Content)
		}
	}
	if got, _ := os.ReadFile(shared); string(got) != "the login of record" {
		t.Errorf("the shared login holds %q after the key calls", got)
	}
	if _, err := os.Stat(left); !os.IsNotExist(err) {
		t.Errorf("the key session outlived its generation at %s (stat: %v)", left, err)
	}

	comp, err := ask(t, plan, llm.Request{})
	if err != nil {
		t.Fatalf("subscription call: %v", err)
	}
	if comp.Content != "the login of record" {
		t.Errorf("the subscription call started on %q, want the login of record", comp.Content)
	}
}

// INHERIT-ENV SIGNS IN WITH A KEY WHERE THE ENGINE'S ENVIRONMENT HOLDS ONE, and
// runs where the login is otherwise: the mode forwards the variable, and the
// call's home follows what it was handed rather than the mode's name.
func TestInheritEnvRunsWhereWhatItForwardsSignsItIn(t *testing.T) {
	// Not parallel: it sets the engine's own environment.
	dir := t.TempDir()
	t.Cleanup(func() { forgetWorkspace(dir) })
	p := keySessionProvider(t, dir, Auth{Mode: AuthInheritEnv})
	t.Setenv("FAKE_VENDOR_API_KEY", "")
	if p.signsInWithKey() {
		t.Error("inherit-env with no key in the engine's environment runs in the key home")
	}
	t.Setenv("FAKE_VENDOR_API_KEY", "sk-test-host-key")
	if !p.signsInWithKey() {
		t.Error("inherit-env forwarding the engine's key runs in the login home")
	}
	for mode, want := range map[AuthMode]bool{AuthAPIKey: true, AuthSubscription: false} {
		if got := keySessionProvider(t, t.TempDir(), Auth{Mode: mode}).signsInWithKey(); got != want {
			t.Errorf("%s: signs in with a key = %v, want %v", mode, got, want)
		}
	}
}
