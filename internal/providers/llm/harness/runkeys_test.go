package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// museLogin is a Muse Code auth.json, as `muse login` stores one: its OIDC
// access token beside the key and base URL its own mint answered with.
func museLogin(access, key, base string) string {
	raw, _ := json.Marshal(map[string]any{"schema_version": 1, "providers": map[string]any{"meta": map[string]any{
		"access_token": access, "refresh_token": "r1", "mechanism": "oauth",
		"api_key": key, "api_base_url": base}}})
	return string(raw)
}

// sameJSON reports whether a and b decode to the same JSON value.
func sameJSON(t *testing.T, a, b []byte) bool {
	t.Helper()
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

// A KEY A CALL MINTED NEVER ENTERS THE SHARED LOGIN: a seat's home is seeded
// with the login and its harness writes a key and base URL of its own into
// the file during the call ([harnessprofile.Profile.RunKeys]), as Muse Code
// does. Carried out, the shared login keeps its own key: untouched where the
// call changed nothing else, and with the call's refreshed token beside its
// own key where it did. A login command takes the whole file, the key its
// sign-in minted included.
//
// Mutation: carry a refresh out whole, or keep the run keys on a login.
func TestAKeyACallMintedNeverEntersTheSharedLogin(t *testing.T) {
	t.Parallel()
	profile := testProfile(t)
	profile.CredentialPaths = []string{".vendor/auth.json"}
	profile.RunKeys = map[string][]string{"auth.json": {"providers.meta.api_key", "providers.meta.api_base_url"}}
	dir := t.TempDir()
	t.Cleanup(func() { forgetWorkspace(dir) })
	ws, err := Shared(dir, "fake-cli", profile)
	if err != nil {
		t.Fatal(err)
	}
	shared := filepath.Join(ws.CredentialsDir(), "auth.json")
	login := museLogin("a1", "LOGIN-KEY", "https://login.example.com")
	writeFile(t, shared, login)
	for _, c := range []struct {
		name, home, want string
	}{
		{"minted alone", museLogin("a1", "MINTED", "https://minted.example.com"), login},
		{"refreshed beside it", museLogin("a2", "MINTED", "https://minted.example.com"),
			museLogin("a2", "LOGIN-KEY", "https://login.example.com")},
	} {
		before, _ := os.ReadFile(shared)
		checkout, err := ws.Acquire(t.Context(), "dev", "call", nil)
		if err != nil {
			t.Fatalf("%s: Acquire: %v", c.name, err)
		}
		writeFile(t, filepath.Join(checkout.Home, ".vendor", "auth.json"), c.home)
		if err := checkout.Release(t.Context()); err != nil {
			t.Fatalf("%s: Release: %v", c.name, err)
		}
		after, err := os.ReadFile(shared)
		if err != nil {
			t.Fatal(err)
		}
		if !sameJSON(t, after, []byte(c.want)) {
			t.Errorf("%s: the shared login is %s, want %s", c.name, after, c.want)
		}
		if c.want == login && string(after) != string(before) {
			t.Errorf("%s: the shared login was rewritten though the call changed nothing of it", c.name)
		}
	}
	home := t.TempDir()
	signedIn := museLogin("a3", "SIGN-IN-KEY", "https://sign-in.example.com")
	writeFile(t, filepath.Join(home, ".vendor", "auth.json"), signedIn)
	if _, err := ws.syncCredentialsOut(t.Context(), home, t.TempDir(), nil,
		generation{Mode: syncLogin, Seeded: map[string]string{}}); err != nil {
		t.Fatalf("syncCredentialsOut: %v", err)
	}
	if after, _ := os.ReadFile(shared); !sameJSON(t, after, []byte(signedIn)) {
		t.Errorf("a login command's carry wrote %s, want the whole login it signed in, %s", after, signedIn)
	}
}
