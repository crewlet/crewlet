package cliagent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A SETTINGS FILE IS A LOGIN ONLY ONCE IT HOLDS ONE. Copilot CLI writes
// `firstLaunchAt` into the config.json its plaintext login lives in on its
// first start, signed in or not, and a seat's home carries that file out like
// any credential file it changed. Counted as a login, it had the doctor report
// a sign-in nobody made, and kept a configured bundle — the real login — from
// being restored over it.
func TestASettingsFileIsALoginOnlyOnceItHoldsOne(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	t.Cleanup(func() { forgetWorkspace(dir) })
	p, err := New(Config{
		Key: "sub", Agent: "custom", StateDir: dir,
		Timeout: time.Second, MaxConcurrent: 1,
		Overrides: map[string]any{
			"binary": os.Args[0], "complete_args": []any{"-test.run=TestCLIAgentFakeCLI"},
			"model_args": []any{}, "output": "text",
			"credential_paths": []any{".vendor/config.json"},
			"login_keys":       map[string]any{"config.json": "tokens"},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	shared := filepath.Join(p.Workspace().CredentialsDir(), "config.json")
	if err := os.MkdirAll(filepath.Dir(shared), 0o700); err != nil {
		t.Fatal(err)
	}
	firstLaunch := "// User settings belong in settings.json.\n// This file is managed automatically.\n" +
		`{"firstLaunchAt": "2026-10-24T21:08:22.632Z"}` + "\n"
	if err := os.WriteFile(shared, []byte(firstLaunch), 0o600); err != nil {
		t.Fatal(err)
	}
	if p.Workspace().HasLogin() {
		t.Error("a settings file holding no login reads as a login")
	}

	login := `{"firstLaunchAt": "2026-10-24T21:08:22.632Z", "tokens": {"https://example.com:someone": "placeholder"}}`
	if err := p.RestoreBundle(t.Context(), bundleOf(t, p.agent, "config.json", login)); err != nil {
		t.Fatalf("RestoreBundle over a settings file holding no login: %v", err)
	}
	if got, _ := os.ReadFile(shared); string(got) != login {
		t.Errorf("the restored bundle left %q, want the login", got)
	}
	if files := p.Workspace().LoginFiles(); len(files) != 1 || files[0] != shared {
		t.Errorf("LoginFiles = %v, want the restored login", files)
	}
}

// A LOGIN COMMAND THAT LEFT ONLY THE CLI'S SETTINGS SIGNED NOBODY IN. The
// carry-out answers whether a home holds a login, which is how `crewlet llm
// login` tells a sign-in from a command that ended with none — and a Copilot
// login whose token went to a keychain, or whose plaintext storage was
// declined, leaves config.json holding firstLaunchAt alone.
func TestACarryOutOfSettingsAloneHoldsNoLogin(t *testing.T) {
	t.Parallel()
	profile := testProfile(t)
	profile.CredentialPaths = []string{".vendor/config.json"}
	profile.LoginKeys = map[string]string{"config.json": "tokens"}
	dir := t.TempDir()
	t.Cleanup(func() { forgetWorkspace(dir) })
	ws, err := Shared(dir, "fake-cli", profile)
	if err != nil {
		t.Fatal(err)
	}
	for body, want := range map[string]bool{
		`{"firstLaunchAt": "2026-10-24T21:08:22.632Z"}`:    false,
		`{"tokens": {"https://example.com:someone": "x"}}`: true,
	} {
		home := t.TempDir()
		writeFile(t, filepath.Join(home, ".vendor", "config.json"), body)
		holds, err := ws.syncCredentialsOut(t.Context(), home, t.TempDir(), nil,
			generation{Mode: syncLogin, Seeded: map[string]string{}})
		if err != nil {
			t.Fatalf("syncCredentialsOut: %v", err)
		}
		if holds != want {
			t.Errorf("a home holding %s holds a login = %v, want %v", body, holds, want)
		}
	}
}
