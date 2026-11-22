//go:build unix

package sandbox

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/hostbox"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// testLogin is one harness's login shapes in harnessprofile's
// testdata/logins.json, each a login by file name.
type testLogin struct {
	Seed    map[string]string `json:"seed"`
	Refresh map[string]string `json:"refresh"`
	Other   map[string]string `json:"other"`
	Planted map[string]string `json:"planted"`
}

// A BOX CARRIES ONLY ITS LOGIN, AND ONLY THE ACCOUNT IT WAS SEEDED WITH, for
// every harness with a mixed file or an account to name: it is seeded with a
// mixed file's login fields alone ([Spec.CredentialRules]), none of the shared
// copy's settings; a setting it writes beside an unchanged login leaves the
// shared login as it was; a copy naming another account is refused and said
// by file, never by value; and a refresh of the seeded account is written,
// only its login fields taken. A box handed no rules writes its copy whole.
// Not parallel: it captures the process-wide logger ([captureLogs]).
//
// Mutation: seed or write back a mixed file whole, drop the account check or
// the rules from the box's record, or log the refusal as another outcome.
func TestABoxCarriesOnlyItsLoginAndOnlyItsAccount(t *testing.T) {
	raw, err := os.ReadFile("../providers/llm/harness/harnessprofile/testdata/logins.json")
	if err != nil {
		t.Fatal(err)
	}
	var logins map[string]testLogin
	if err := json.Unmarshal(raw, &logins); err != nil {
		t.Fatal(err)
	}
	logs := captureLogs(t)
	for _, harness := range slices.Sorted(maps.Keys(logins)) {
		login := logins[harness]
		profile, ok := harnessprofile.Builtin(harness)
		if !ok {
			t.Fatalf("no %s profile", harness)
		}
		names := slices.Sorted(maps.Keys(login.Seed))
		run := func(copied map[string]string) map[string]string {
			t.Helper()
			dir := filepath.Join(t.TempDir(), "credentials")
			spec := Spec{CredentialFiles: map[string]string{}, CredentialRules: map[string]harnessprofile.FileRule{}}
			for _, rel := range profile.CredentialPaths {
				if name := filepath.Base(rel); slices.Contains(names, name) {
					spec.CredentialFiles[rel] = filepath.Join(dir, name)
					spec.CredentialRules[rel] = profile.FileRule(name)
				}
			}
			putLogin(t, dir, login.Seed)
			box, err := newDirect(t).Create(t.Context(), spec)
			if err != nil {
				t.Fatalf("%s: Create: %v", harness, err)
			}
			for rel, shared := range spec.CredentialFiles {
				handed, _ := os.ReadFile(filepath.Join(box.Home(), rel))
				if strings.Contains(string(handed), "SHARED-SETTING") {
					t.Errorf("%s: the box was seeded with the shared copy's settings:\n%s", harness, handed)
				}
				writeFile(t, filepath.Join(box.Home(), rel), copied[filepath.Base(shared)])
			}
			if err := box.Close(t.Context()); err != nil {
				t.Fatalf("%s: Close: %v", harness, err)
			}
			return sharedLogin(t, dir, names)
		}
		before := len(logs.String())
		if got := run(login.Other); !maps.Equal(got, login.Seed) {
			t.Errorf("%s: another account's copy left the shared login %v, want it as it was", harness, got)
		}
		said := logs.String()[before:]
		if !strings.Contains(said, "local_sandbox_credential_superseded") ||
			!strings.Contains(said, "shared=other_account") {
			t.Errorf("%s: the refused copy was not said as another account's:\n%s", harness, said)
		}
		if strings.Contains(said, "access-9") || strings.Contains(said, "token-9") {
			t.Errorf("%s: the log carries a login's value:\n%s", harness, said)
		}
		want := map[string]string{}
		for _, name := range names {
			want[name] = string(profile.FileRule(name).Carry([]byte(login.Refresh[name]), []byte(login.Seed[name])))
		}
		if got := run(login.Refresh); !maps.Equal(got, want) {
			t.Errorf("%s: a refresh of the seeded account left the shared login %v, want %v", harness, got, want)
		}
		if login.Planted != nil {
			if got := run(login.Planted); !maps.Equal(got, login.Seed) {
				t.Errorf("%s: a setting the box wrote left the shared login %v, want it as it was", harness, got)
			}
		}
	}
	codex := logins["codex"]
	dir := filepath.Join(t.TempDir(), "credentials")
	putLogin(t, dir, codex.Seed)
	box, err := newDirect(t).Create(t.Context(), Spec{
		CredentialFiles: map[string]string{".codex/auth.json": filepath.Join(dir, "auth.json")},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	writeFile(t, filepath.Join(box.Home(), ".codex", "auth.json"), codex.Other["auth.json"])
	if err := box.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := sharedLogin(t, dir, []string{"auth.json"}); got["auth.json"] != codex.Other["auth.json"] {
		t.Errorf("a box handed no rules wrote back %v, want its copy whole", got)
	}
}

// sharedLogin reads the named files of a shared directory's login as the next
// reader would.
func sharedLogin(t *testing.T, dir string, names []string) map[string]string {
	t.Helper()
	release, err := hostbox.LockCredentials(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	login, err := hostbox.ReadCredentials(dir, names)
	if err != nil {
		t.Fatalf("ReadCredentials: %v", err)
	}
	out := map[string]string{}
	for name, content := range login {
		out[name] = string(content)
	}
	return out
}
