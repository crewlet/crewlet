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

// A SEAT CARRIES ONLY ITS TOKENS, AND ONLY THE ACCOUNT ITS HOME WAS SEEDED
// WITH, for every harness: a home holds a mixed file's login fields alone; a
// refresh writes exactly the shared login the fixture writes out; a setting
// written beside an unchanged login, or a copy that does not read, leaves the
// shared login as it was and reaches no later home; a copy whose account
// fields or ID token name another account, or that dropped a file holding
// them, is refused and logged with shared=other_account, never a value. A
// login command takes the whole login. Not parallel: it captures the logger.
//
// Mutation: seed or carry a mixed file whole, judge a refresh on the whole
// file, check the account on changed files alone, or apply it to a login
// command's carry.
func TestASeatCarriesOnlyItsTokensAndOnlyItsAccount(t *testing.T) {
	raw, err := os.ReadFile("harnessprofile/testdata/logins.json")
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
		profile, err := harnessprofile.Load(harness, nil)
		if err != nil {
			t.Fatalf("%s: %v", harness, err)
		}
		dir := t.TempDir()
		t.Cleanup(func() { forgetWorkspace(dir) })
		ws, err := Shared(dir, harness, profile)
		if err != nil {
			t.Fatal(err)
		}
		// seeded hands a fresh home the shared login, failing the case on any
		// setting of the shared copy or of a home before it.
		seeded := func() (string, generation) {
			t.Helper()
			home := t.TempDir()
			gen, err := ws.seed(t.Context(), home, t.TempDir(), nil, syncRefresh)
			if err != nil {
				t.Fatalf("%s: seed: %v", harness, err)
			}
			for _, rel := range profile.CredentialPaths {
				handed, _ := os.ReadFile(filepath.Join(home, rel))
				if strings.Contains(string(handed), "SHARED-SETTING") ||
					strings.Contains(string(handed), "PLANTED-SETTING") {
					t.Errorf("%s: a home was seeded with a setting:\n%s", harness, handed)
				}
			}
			return home, gen
		}
		carry := func(mode syncMode, copied map[string]string) (map[string]string, string) {
			t.Helper()
			for name, content := range login.Seed {
				writeFile(t, filepath.Join(ws.CredentialsDir(), name), content)
			}
			home, gen := seeded()
			gen.Mode = mode
			for _, rel := range profile.CredentialPaths {
				// A FILE THE COPY LACKS IS ABSENT from the home, not as seeded.
				if content, held := copied[filepath.Base(rel)]; held {
					writeFile(t, filepath.Join(home, rel), content)
				} else if err := os.Remove(filepath.Join(home, rel)); err != nil && !os.IsNotExist(err) {
					t.Fatal(err)
				}
			}
			before := len(logs.String())
			if _, err := ws.syncCredentialsOut(t.Context(), home, t.TempDir(), nil, gen); err != nil {
				t.Fatalf("%s: syncCredentialsOut: %v", harness, err)
			}
			return readLogin(t, ws), logs.String()[before:]
		}
		if got, _ := carry(syncRefresh, login.Refresh); !maps.Equal(got, login.Carried) {
			t.Errorf("%s: a refresh of the seeded account left the shared login %v, want %v", harness, got, login.Carried)
		}
		for what, copied := range map[string]map[string]string{
			"another account's refresh": login.Other, "a swapped token": login.Swap, "a dropped account file": login.Dropped,
		} {
			if copied == nil {
				continue
			}
			got, said := carry(syncRefresh, copied)
			if !maps.Equal(got, login.Seed) {
				t.Errorf("%s: %s left the shared login %v, want it as it was", harness, what, got)
			}
			if !strings.Contains(said, "harness_login_superseded") || !strings.Contains(said, "shared=other_account") {
				t.Errorf("%s: %s was not said as another account's:\n%s", harness, what, said)
			}
			if strings.Contains(said, "access-9") || strings.Contains(said, "token-9") {
				t.Errorf("%s: the log carries a login's value:\n%s", harness, said)
			}
		}
		if login.Planted != nil {
			if got, _ := carry(syncRefresh, login.Planted); !maps.Equal(got, login.Seed) {
				t.Errorf("%s: a setting the harness wrote left the shared login %v, want it as it was", harness, got)
			}
			seeded()
		}
		for i, broken := range login.Broken {
			got, said := carry(syncRefresh, broken.Copy)
			if !maps.Equal(got, login.Seed) {
				t.Errorf("%s: broken copy %d left the shared login %v, want it as it was", harness, i, got)
			}
			if logged := strings.Contains(said, "harness_login_superseded"); logged != broken.Logged {
				t.Errorf("%s: broken copy %d logged = %v, want %v:\n%s", harness, i, logged, broken.Logged, said)
			}
		}
		if other := login.Other; other != nil {
			if got, _ := carry(syncLogin, other); !maps.Equal(got, other) {
				t.Errorf("%s: a login command left the shared login %v, want the login it made", harness, got)
			}
		}
	}
}
