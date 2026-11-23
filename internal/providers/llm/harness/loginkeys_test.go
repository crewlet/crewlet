package harness

import (
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
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
		Key: "sub", Harness: "custom", StateDir: dir,
		Timeout: time.Second, MaxConcurrent: 1,
		Overrides: map[string]any{
			"binary": os.Args[0], "complete_args": []any{"-test.run=TestFakeHarness"},
			"model_args": []any{}, "output": "text",
			"credential_paths": []any{".vendor/config.json"},
			"login_keys":       map[string]any{"config.json": []any{"tokens"}},
			"comment_lines":    []any{"config.json"},
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
	if has, err := p.Workspace().HasLogin(t.Context()); err != nil || has {
		t.Errorf("a settings file holding no login reads as a login: %v, %v", has, err)
	}

	login := `{"firstLaunchAt": "2026-10-24T21:08:22.632Z", "tokens": {"https://example.com:someone": "placeholder"}}`
	if err := p.RestoreBundle(t.Context(), bundleOf(t, p.harness, "config.json", login)); err != nil {
		t.Fatalf("RestoreBundle over a settings file holding no login: %v", err)
	}
	if got, _ := os.ReadFile(shared); string(got) != login {
		t.Errorf("the restored bundle left %q, want the login", got)
	}
	if held, err := p.Workspace().Login(t.Context()); err != nil || len(held) != 1 ||
		string(held["config.json"]) != login {
		t.Errorf("Login = %v, %v, want the restored login alone", held, err)
	}
}

// A LOGIN COMMAND THAT LEFT ONLY THE HARNESS'S SETTINGS SIGNED NOBODY IN. The
// carry-out answers whether a home holds a login, which is how `crewlet llm
// login` tells a sign-in from a command that ended with none — and a Copilot
// login whose token went to a keychain, or whose plaintext storage was
// declined, leaves config.json holding firstLaunchAt alone.
func TestACarryOutOfSettingsAloneHoldsNoLogin(t *testing.T) {
	t.Parallel()
	profile := testProfile(t)
	profile.CredentialPaths = []string{".vendor/config.json"}
	profile.LoginKeys = map[string][]string{"config.json": {"tokens"}}
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

// A SETTINGS FILE HOLDING NO LOGIN IS NEITHER EXPORTED NOR REMOVED BY A
// LOGOUT, nor in an import's way. The shared login is read and written as one
// unit, settings file and all, so each of those reads the unit and has to set
// such a file apart itself: a bundle that packed it would restore one host's
// record of the harness onto every volume, a logout that wrote the empty login
// would take the harness's settings with the sign-in, and an import that wrote
// exactly the bundle would do the same.
//
// Mutation: export, log out or import over the whole of [Workspace.Login]
// rather than [Workspace.loginIn], and its half of this is red.
func TestASettingsFileIsNeitherExportedNorLoggedOut(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	t.Cleanup(func() { forgetWorkspace(dir) })
	p, err := New(Config{
		Key: "sub", Harness: "custom", StateDir: dir,
		Timeout: time.Second, MaxConcurrent: 1,
		Overrides: map[string]any{
			"binary": os.Args[0], "complete_args": []any{"-test.run=TestFakeHarness"},
			"model_args": []any{}, "output": "text",
			"credential_paths": []any{".vendor/config.json", ".vendor/token.json"},
			"login_keys":       map[string]any{"config.json": []any{"tokens"}},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	const settings = `{"firstLaunchAt": "2026-10-24T21:08:22.632Z"}`
	const token = `{"access": "placeholder"}`
	shared := p.Workspace().CredentialsDir()
	writeFile(t, filepath.Join(shared, "config.json"), settings)
	writeFile(t, filepath.Join(shared, "token.json"), token)

	blob, err := p.ExportBundle(t.Context())
	if err != nil {
		t.Fatalf("ExportBundle: %v", err)
	}
	packed, _, err := p.stageBundle(blob)
	if err != nil {
		t.Fatalf("stageBundle: %v", err)
	}
	if len(packed) != 1 || string(packed["token.json"]) != token {
		t.Errorf("the bundle packs %v, want token.json alone", slices.Sorted(maps.Keys(packed)))
	}

	if err := p.Logout(t.Context(), io.Discard, io.Discard); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if got := readLogin(t, p.Workspace()); len(got) != 1 || got["config.json"] != settings {
		t.Errorf("after the logout the shared login is %v, want the settings file alone", got)
	}

	if err := p.ImportBundle(t.Context(), blob); err != nil {
		t.Fatalf("ImportBundle over the settings file: %v", err)
	}
	if got := readLogin(t, p.Workspace()); len(got) != 2 || got["config.json"] != settings ||
		got["token.json"] != token {
		t.Errorf("after the import the shared login is %v, want the settings file and the token", got)
	}
}

// BOTH OF COPILOT'S LOGIN ENTRIES ARE ITS LOGIN. On its first use of a login
// under copilotTokens, Copilot 1.0.114 adds the same token under authTokens,
// and signs in from authTokens alone (measured, see the profile). A config.json
// holding either is carried out as a login, and is no settings file a logout
// keeps; holding neither, it is the harness's settings alone.
//
// Mutation: drop authTokens from the copilot profile's login_keys, and the
// second copy is kept by a logout and carries out no login.
func TestBothOfCopilotsLoginEntriesAreItsLogin(t *testing.T) {
	t.Parallel()
	profile, err := harnessprofile.Load("copilot", nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	t.Cleanup(func() { forgetWorkspace(dir) })
	ws, err := Shared(dir, "copilot", profile)
	if err != nil {
		t.Fatal(err)
	}
	const header = "// User settings belong in settings.json.\n// This file is managed automatically.\n"
	for name, tc := range map[string]struct {
		body  string
		login bool
	}{
		"its first entry": {`{"copilotTokens": {"https://github.com:someone": "placeholder"}}`, true},
		"its second entry alone": {`{"copilotTokens": {}, "authTokens": ` +
			`{"https://github.com:someone:github": {"token": "placeholder"}}}`, true},
		"both, as 1.0.114 leaves it": {`{"copilotTokens": {"https://github.com:someone": "placeholder"}, ` +
			`"authTokens": {"https://github.com:someone:github": {"token": "placeholder"}}}`, true},
		"neither": {`{"firstLaunchAt": "2026-11-22T00:00:00.000Z", "authTokens": {}}`, false},
	} {
		t.Run(name, func(t *testing.T) {
			content := []byte(header + tc.body)
			files := map[string][]byte{"config.json": content}
			if _, kept := ws.notLoginIn(files)["config.json"]; kept == tc.login {
				t.Errorf("a logout keeps the file = %v, want %v", kept, !tc.login)
			}
			if _, carried := ws.loginIn(files)["config.json"]; carried != tc.login {
				t.Errorf("the file is a login to export = %v, want %v", carried, tc.login)
			}
			home := t.TempDir()
			writeFile(t, filepath.Join(home, ".copilot", "config.json"), string(content))
			holds, err := ws.syncCredentialsOut(t.Context(), home, t.TempDir(), nil,
				generation{Mode: syncLogin, Seeded: map[string]string{}})
			if err != nil {
				t.Fatalf("syncCredentialsOut: %v", err)
			}
			if holds != tc.login {
				t.Errorf("the carried-out home holds a login = %v, want %v", holds, tc.login)
			}
		})
	}
}
