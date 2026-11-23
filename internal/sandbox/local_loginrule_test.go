//go:build unix

package sandbox

import (
	"bytes"
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
	Dropped map[string]string `json:"dropped"`
	Broken  []struct {
		Copy   map[string]string `json:"copy"`
		Logged bool              `json:"logged"`
	} `json:"broken"`
}

// A BOX CARRIES ONLY ITS TOKENS, AND ONLY THE ACCOUNT IT WAS SEEDED WITH, for
// every harness: it is seeded with each file whole, a mixed one's settings
// with its login ([Spec.CredentialRules]); a refresh writes back exactly the shared login the
// fixture writes out, its tokens alone; a setting written beside an unchanged
// login, and a copy that does not read, a key named twice or a commented
// change, leave the shared login as it was; a copy whose account fields or named
// token name another account, or that dropped a file holding them, is refused
// and said by file, never by value. A box with no rule writes nothing back.
//
// Mutation: seed a mixed file as its login alone, write a copy back whole, read a copy that
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
		carries := slices.ContainsFunc(names, func(name string) bool { return len(profile.RefreshKeys[name]) > 0 })
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
				if whole, _ := os.ReadFile(shared); !bytes.Equal(handed, whole) {
					t.Errorf("%s: the box was seeded with\n%s\nwant the shared copy whole:\n%s", harness, handed, whole)
				}
				// A FILE THE COPY LACKS IS ABSENT from the box, not empty.
				if content, held := copied[filepath.Base(shared)]; held {
					writeFile(t, filepath.Join(box.Home(), rel), content)
				} else if err := os.Remove(filepath.Join(box.Home(), rel)); err != nil && !os.IsNotExist(err) {
					t.Fatal(err)
				}
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
		for what, copied := range map[string]map[string]string{
			"another account's copy": login.Other, "a swapped token": login.Swap, "a dropped account file": login.Dropped,
		} {
			if copied == nil {
				continue
			}
			got, said := run(copied)
			if !maps.Equal(got, login.Seed) {
				t.Errorf("%s: %s left the shared login %v, want it as it was", harness, what, got)
			}
			// A login none of whose files carries a token back writes nothing
			// whatever the copy says, so there is no refusal to tell.
			if refused := strings.Contains(said, "local_sandbox_credential_superseded") && strings.Contains(said, "shared=other_account"); refused != carries {
				t.Errorf("%s: %s said as another account's = %v, want %v:\n%s", harness, what, !carries, carries, said)
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
