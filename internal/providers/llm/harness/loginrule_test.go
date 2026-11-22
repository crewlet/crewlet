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
	Other   map[string]string `json:"other"`
	Planted map[string]string `json:"planted"`
}

// A SEAT CARRIES ONLY ITS LOGIN, AND ONLY THE ACCOUNT ITS HOME WAS SEEDED WITH,
// for every harness with a mixed file or an account to name: a home holds a
// mixed file's login fields alone; a setting written beside an unchanged login
// reaches neither the shared login nor a later home; another account's refresh
// is refused and logged with shared=other_account, never a value; the seeded
// account's refresh is carried, its login fields alone. A login command takes
// the whole login. Not parallel: it captures the process-wide logger.
//
// Mutation: seed or carry a mixed file whole, judge a refresh on the whole
// file, drop the account check from the refresh carry, apply it to a login
// command's, or log the refusal as another outcome.
func TestASeatCarriesOnlyItsLoginAndOnlyItsAccount(t *testing.T) {
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
		carry := func(mode syncMode, copied map[string]string) map[string]string {
			t.Helper()
			for name, content := range login.Seed {
				writeFile(t, filepath.Join(ws.CredentialsDir(), name), content)
			}
			home, gen := seeded()
			gen.Mode = mode
			for _, rel := range profile.CredentialPaths {
				if content, held := copied[filepath.Base(rel)]; held {
					writeFile(t, filepath.Join(home, rel), content)
				}
			}
			if _, err := ws.syncCredentialsOut(t.Context(), home, t.TempDir(), nil, gen); err != nil {
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
		if strings.Contains(said, "access-9") || strings.Contains(said, "token-9") {
			t.Errorf("%s: the log carries a login's value:\n%s", harness, said)
		}
		want := map[string]string{}
		for name, content := range login.Seed {
			want[name] = string(profile.FileRule(name).Carry([]byte(login.Refresh[name]), []byte(content)))
		}
		if got := carry(syncRefresh, login.Refresh); !maps.Equal(got, want) {
			t.Errorf("%s: a refresh of the seeded account left the shared login %v, want %v", harness, got, want)
		}
		if login.Planted != nil {
			if got := carry(syncRefresh, login.Planted); !maps.Equal(got, login.Seed) {
				t.Errorf("%s: a setting the harness wrote left the shared login %v, want it as it was", harness, got)
			}
			seeded()
		}
		if got := carry(syncLogin, login.Other); !maps.Equal(got, login.Other) {
			t.Errorf("%s: a login command left the shared login %v, want the login it made", harness, got)
		}
	}
}
