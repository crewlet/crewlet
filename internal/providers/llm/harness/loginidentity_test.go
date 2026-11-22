package harness

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// identityLogin is one harness's login shapes in harnessprofile's
// testdata/identity-logins.json, each a login by file name.
type identityLogin struct {
	Seed    map[string]string `json:"seed"`
	Refresh map[string]string `json:"refresh"`
	Other   map[string]string `json:"other"`
}

// A SEAT CARRIES BACK ONLY THE ACCOUNT ITS HOME WAS SEEDED WITH, for every
// harness whose profile names one: a refresh whose identity keys
// ([harnessprofile.Profile.IdentityKeys]) name another account is no refresh of
// the shared login, which stays as it was, and is logged
// harness_login_superseded with shared=other_account, never a value; a refresh
// of the seeded account is carried. A login command takes the whole login,
// whatever account it names. Not parallel: it captures the process-wide logger.
//
// Mutation: drop the identity check from the refresh carry, apply it to a
// login command's, or log the refusal as another outcome.
func TestASeatCarriesBackOnlyTheAccountItsHomeWasSeededWith(t *testing.T) {
	raw, err := os.ReadFile("harnessprofile/testdata/identity-logins.json")
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
		profile, err := harnessprofile.Load(harness, nil)
		if err != nil {
			t.Fatalf("%s: %v", harness, err)
		}
		carry := func(mode syncMode, copied map[string]string) map[string]string {
			t.Helper()
			dir := t.TempDir()
			t.Cleanup(func() { forgetWorkspace(dir) })
			ws, err := Shared(dir, harness, profile)
			if err != nil {
				t.Fatal(err)
			}
			seeded := map[string]string{}
			home := t.TempDir()
			for _, rel := range profile.CredentialPaths {
				name := filepath.Base(rel)
				if content, held := login.Seed[name]; held {
					writeFile(t, filepath.Join(ws.CredentialsDir(), name), content)
					writeFile(t, filepath.Join(home, rel), copied[name])
					seeded[name] = digestOf([]byte(content))
				}
			}
			if _, err := ws.syncCredentialsOut(t.Context(), home, t.TempDir(), nil,
				generation{Mode: mode, Seeded: seeded}); err != nil {
				t.Fatalf("%s: syncCredentialsOut: %v", harness, err)
			}
			return readLogin(t, ws)
		}
		before := len(logs.String())
		if got := carry(syncRefresh, login.Other); !maps.Equal(got, login.Seed) {
			t.Errorf("%s: another account's refresh left the shared login %v, want it as it was", harness, got)
		}
		said := logs.String()[before:]
		if !strings.Contains(said, "harness_login_superseded") || !strings.Contains(said, "shared=other_account") {
			t.Errorf("%s: the refused refresh was not said as another account's:\n%s", harness, said)
		}
		if strings.Contains(said, "access-9") {
			t.Errorf("%s: the log carries a login's value:\n%s", harness, said)
		}
		if got := carry(syncRefresh, login.Refresh); !maps.Equal(got, login.Refresh) {
			t.Errorf("%s: a refresh of the seeded account left the shared login %v, want the refresh", harness, got)
		}
		if got := carry(syncLogin, login.Other); !maps.Equal(got, login.Other) {
			t.Errorf("%s: a login command left the shared login %v, want the login it made", harness, got)
		}
	}
}
