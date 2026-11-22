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
	Carried map[string]string `json:"carried"`
	Other   map[string]string `json:"other"`
	Swap    map[string]string `json:"swap"`
	Planted map[string]string `json:"planted"`
	Broken  []struct {
		Copy   map[string]string `json:"copy"`
		Logged bool              `json:"logged"`
	} `json:"broken"`
}

// A BOX CARRIES ONLY ITS TOKENS, AND ONLY THE ACCOUNT IT WAS SEEDED WITH, for
// every harness: it is seeded with a mixed file's login fields alone
// ([Spec.CredentialRules]); a refresh writes back exactly the shared login the
// fixture writes out, its tokens alone; a setting written beside an unchanged
// login, and a copy that does not read, a key named twice or a commented
// change, leave the shared login as it was; another account's copy, or a token
// file swapped alone, is refused and said by file, never by value. A box
// handed no rule writes nothing back. Not parallel: it captures the logger.
//
// Mutation: seed a mixed file whole, write a copy back whole, read a copy that
// does not parse as nothing changed, check the account on changed files alone,
// or write back a file with no rule.
func TestABoxCarriesOnlyItsTokensAndOnlyItsAccount(t *testing.T) {
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
		// run seeds a box with the login, puts copied in its home and tears it
		// down, answering the shared login after and what the teardown said.
		run := func(copied map[string]string) (map[string]string, string) {
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
			before := len(logs.String())
			if err := box.Close(t.Context()); err != nil {
				t.Fatalf("%s: Close: %v", harness, err)
			}
			return sharedLogin(t, dir, names), logs.String()[before:]
		}
		if got, _ := run(login.Refresh); !maps.Equal(got, login.Carried) {
			t.Errorf("%s: a refresh of the seeded account left the shared login %v, want %v", harness, got, login.Carried)
		}
		for what, copied := range map[string]map[string]string{"another account's copy": login.Other, "a swapped token": login.Swap} {
			if copied == nil {
				continue
			}
			got, said := run(copied)
			if !maps.Equal(got, login.Seed) {
				t.Errorf("%s: %s left the shared login %v, want it as it was", harness, what, got)
			}
			if !strings.Contains(said, "local_sandbox_credential_superseded") || !strings.Contains(said, "shared=other_account") {
				t.Errorf("%s: %s was not said as another account's:\n%s", harness, what, said)
			}
			if strings.Contains(said, "access-9") || strings.Contains(said, "token-9") {
				t.Errorf("%s: the log carries a login's value:\n%s", harness, said)
			}
		}
		if login.Planted != nil {
			if got, _ := run(login.Planted); !maps.Equal(got, login.Seed) {
				t.Errorf("%s: a setting the box wrote left the shared login %v, want it as it was", harness, got)
			}
		}
		for i, broken := range login.Broken {
			got, said := run(broken.Copy)
			if !maps.Equal(got, login.Seed) {
				t.Errorf("%s: broken copy %d left the shared login %v, want it as it was", harness, i, got)
			}
			if logged := strings.Contains(said, "local_sandbox_credential_superseded"); logged != broken.Logged {
				t.Errorf("%s: broken copy %d logged = %v, want %v:\n%s", harness, i, logged, broken.Logged, said)
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
	writeFile(t, filepath.Join(box.Home(), ".codex", "auth.json"), codex.Refresh["auth.json"])
	if err := box.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := sharedLogin(t, dir, []string{"auth.json"}); got["auth.json"] != codex.Seed["auth.json"] {
		t.Errorf("a box handed no rule wrote back %v, want nothing", got)
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
