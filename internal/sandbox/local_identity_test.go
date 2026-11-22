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

// identityLogin is one harness's login shapes in harnessprofile's
// testdata/identity-logins.json, each a login by file name.
type identityLogin struct {
	Seed    map[string]string `json:"seed"`
	Refresh map[string]string `json:"refresh"`
	Other   map[string]string `json:"other"`
}

// A BOX WRITES BACK ONLY THE ACCOUNT IT WAS SEEDED WITH, for every harness
// whose profile names one: a login whose identity keys
// ([Spec.CredentialIdentityKeys], the profile's) name another account at
// teardown is no refresh of the shared login, which stays as it was, and the
// box says so by file, never by value; a refresh of the seeded account is
// written back. A box handed no identity keys writes its changed copy back
// whole. Not parallel: it captures the process-wide logger ([captureLogs]).
//
// Mutation: drop the identity check from the write-back, drop the keys from the
// box's record, or log the refusal as another outcome.
func TestABoxWritesBackOnlyTheAccountItWasSeededWith(t *testing.T) {
	raw, err := os.ReadFile("../providers/llm/harness/harnessprofile/testdata/identity-logins.json")
	if err != nil {
		t.Fatal(err)
	}
	var logins map[string]identityLogin
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
		run := func(keyed bool, copied map[string]string) map[string]string {
			t.Helper()
			dir := filepath.Join(t.TempDir(), "credentials")
			spec := Spec{CredentialFiles: map[string]string{}, CredentialIdentityKeys: map[string][]string{}}
			for _, rel := range profile.CredentialPaths {
				name := filepath.Base(rel)
				if _, held := login.Seed[name]; !held {
					continue
				}
				spec.CredentialFiles[rel] = filepath.Join(dir, name)
				if keys := profile.IdentityKeys[name]; keyed && len(keys) > 0 {
					spec.CredentialIdentityKeys[rel] = keys
				}
			}
			putLogin(t, dir, login.Seed)
			box, err := newDirect(t).Create(t.Context(), spec)
			if err != nil {
				t.Fatalf("%s: Create: %v", harness, err)
			}
			for rel, shared := range spec.CredentialFiles {
				writeFile(t, filepath.Join(box.Home(), rel), copied[filepath.Base(shared)])
			}
			if err := box.Close(t.Context()); err != nil {
				t.Fatalf("%s: Close: %v", harness, err)
			}
			return sharedLogin(t, dir, slices.Collect(maps.Keys(login.Seed)))
		}
		before := len(logs.String())
		if got := run(true, login.Other); !maps.Equal(got, login.Seed) {
			t.Errorf("%s: another account's copy left the shared login %v, want it as it was", harness, got)
		}
		said := logs.String()[before:]
		if !strings.Contains(said, "local_sandbox_credential_superseded") ||
			!strings.Contains(said, "shared=other_account") {
			t.Errorf("%s: the refused copy was not said as another account's:\n%s", harness, said)
		}
		if strings.Contains(said, "access-9") {
			t.Errorf("%s: the log carries a login's value:\n%s", harness, said)
		}
		if got := run(true, login.Refresh); !maps.Equal(got, login.Refresh) {
			t.Errorf("%s: a refresh of the seeded account left the shared login %v, want the refresh", harness, got)
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
		t.Errorf("a box handed no identity keys wrote back %v, want its copy whole", got)
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
