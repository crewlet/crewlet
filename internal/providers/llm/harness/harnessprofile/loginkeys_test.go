package harnessprofile

import (
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// A RUN'S OWN KEYS TAKE THE SHARED LOGIN'S VALUES, AND NOTHING ELSE MOVES: each
// key [Profile.RunKeys] names is as shared holds it (absent where shared holds
// none), every other key as home has it; a home that differed only there is
// shared, byte for byte; a file that is no JSON object is home as it stands.
//
// Mutation: keep home's value of a run key, drop a key home changed, or
// re-encode a home that equals the shared login.
func TestARunsOwnKeysTakeTheSharedLoginsValues(t *testing.T) {
	t.Parallel()
	keys := []string{"providers.meta.api_key", "providers.meta.api_base_url", "providers.meta.subs_active"}
	shared := []byte(`{"schema_version":1,"providers":{"meta":{"access_token":"a1","api_key":"LOGIN",` +
		`"api_base_url":"https://login","expires_at":1795365318}}}`)
	minted := []byte(`{"schema_version":1,"providers":{"meta":{"access_token":"a1","api_key":"MINTED",` +
		`"api_base_url":"https://minted","expires_at":1795365318,"subs_active":true}}}`)
	if got := KeepLoginKeys(minted, shared, keys); string(got) != string(shared) {
		t.Errorf("a home that differs only in run keys reads %s, want the shared login as it is", got)
	}
	refreshed := []byte(`{"schema_version":1,"providers":{"meta":{"access_token":"a2","api_key":"MINTED",` +
		`"api_base_url":"https://minted","expires_at":1795369999}}}`)
	var got, want any
	_ = json.Unmarshal(KeepLoginKeys(refreshed, shared, keys), &got)
	_ = json.Unmarshal([]byte(`{"schema_version":1,"providers":{"meta":{"access_token":"a2","api_key":"LOGIN",`+
		`"api_base_url":"https://login","expires_at":1795369999}}}`), &want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("a refreshed home reads %v, want its refresh with the shared login's key", got)
	}
	if got := KeepLoginKeys([]byte("not json"), shared, keys); string(got) != "not json" {
		t.Errorf("a home that is no JSON reads %q, want it as it stands", got)
	}
	if got := KeepLoginKeys(minted, nil, nil); string(got) != string(minted) {
		t.Errorf("no run keys reads %s, want home as it stands", got)
	}
}

// MUSE CODE'S PROFILE NAMES THE KEYS ITS CALLS MINT: the key, its base URL and
// the plan flag of auth.json (measured on 1.4.25), and run_keys refuses a file
// no credential path has and a key path with an empty part.
//
// Mutation: drop muse-code's run_keys, or validate none.
func TestMuseCodesProfileNamesTheKeysItsCallsMint(t *testing.T) {
	t.Parallel()
	p, ok := Builtin("muse-code")
	if !ok {
		t.Fatal("no muse-code profile")
	}
	for _, key := range []string{"providers.meta.api_key", "providers.meta.api_base_url"} {
		if !slices.Contains(p.RunKeys["auth.json"], key) {
			t.Errorf("muse-code's run_keys for auth.json are %v, want %s among them", p.RunKeys["auth.json"], key)
		}
	}
	for name, tc := range map[string]struct {
		keys map[string]any
		says string
	}{
		"a file no credential path has": {map[string]any{"hosts.json": []any{"token"}}, `run_keys names "hosts.json"`},
		"an empty part":                 {map[string]any{"auth.json": []any{"providers..api_key"}}, "is no key path"},
	} {
		if _, err := Load("muse-code", map[string]any{"run_keys": tc.keys}); err == nil ||
			!strings.Contains(err.Error(), tc.says) {
			t.Errorf("%s: err = %v, want one saying %q", name, err, tc.says)
		}
	}
}

// A LOGIN'S ACCOUNT IS READ AT ITS IDENTITY KEYS: two copies are one account
// only where each key [Profile.IdentityKeys] names holds alike in both, or is
// absent from both; a refresh that changes its tokens and keeps the account is
// one, a copy naming another account, dropping it or naming a key is not, and
// a copy that is no JSON object names none it can be held to. A `*` part holds
// every entry of the object there, its set of entries too, and `//` lines are
// set aside. identity_keys refuses a file no credential path has, an empty
// list and a key path with an empty part.
//
// Mutation: compare the whole file, read only the first key, take an absent
// key for a match against a present one, read `*` as a literal key, drop an
// entry's absence under `*`, or read `//` lines as JSON.
func TestALoginsAccountIsReadAtItsIdentityKeys(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		keys        []string
		seed, other string
		want        bool
	}{
		"a refresh keeping the account": {[]string{"tokens.account_id", "OPENAI_API_KEY"},
			`{"OPENAI_API_KEY":null,"tokens":{"account_id":"acct-A","access_token":"a1"},"last_refresh":"1"}`,
			`{"OPENAI_API_KEY":null,"tokens":{"account_id":"acct-A","access_token":"a2"},"last_refresh":"2"}`, true},
		"another account": {[]string{"tokens.account_id", "OPENAI_API_KEY"},
			`{"OPENAI_API_KEY":null,"tokens":{"account_id":"acct-A"}}`,
			`{"OPENAI_API_KEY":null,"tokens":{"account_id":"acct-B"}}`, false},
		"the account dropped": {[]string{"tokens.account_id", "OPENAI_API_KEY"},
			`{"OPENAI_API_KEY":null,"tokens":{"account_id":"acct-A"}}`,
			`{"OPENAI_API_KEY":null,"tokens":{}}`, false},
		"the second key changed": {[]string{"tokens.account_id", "OPENAI_API_KEY"},
			`{"OPENAI_API_KEY":null,"tokens":{"account_id":"acct-A"}}`,
			`{"OPENAI_API_KEY":"sk-other","tokens":{"account_id":"acct-A"}}`, false},
		"no JSON":          {[]string{"tokens.account_id"}, `{"tokens":{"account_id":"acct-A"}}`, `not json`, false},
		"no keys, no JSON": {nil, `{"tokens":{"account_id":"acct-A"}}`, `not json`, true},
		"every entry's account kept": {[]string{"*.user_id"},
			`{"https://a.example::c":{"user_id":"u1","key":"k1"}}`,
			`{"https://a.example::c":{"user_id":"u1","key":"k2"}}`, true},
		"an entry's account changed": {[]string{"*.user_id"},
			`{"https://a.example::c":{"user_id":"u1"}}`, `{"https://a.example::c":{"user_id":"u2"}}`, false},
		"an entry added": {[]string{"*.user_id"},
			`{"https://a.example::c":{"user_id":"u1"}}`,
			`{"https://a.example::c":{"user_id":"u1"},"https://b.example::c":{"user_id":"u2"}}`, false},
		"an entry's account dropped": {[]string{"*.user_id"},
			`{"https://a.example::c":{"user_id":"u1"}}`, `{"https://a.example::c":{"key":"k2"}}`, false},
		"comment lines set aside": {[]string{"lastLoggedInUser"},
			"// managed\n{\"lastLoggedInUser\":{\"login\":\"seat\"}}\n",
			"// managed\n{\"lastLoggedInUser\":{\"login\":\"seat\"},\"firstLaunchAt\":\"x\"}\n", true},
	} {
		if got := SameLoginIdentity([]byte(tc.other), []byte(tc.seed), tc.keys); got != tc.want {
			t.Errorf("%s: SameLoginIdentity = %v, want %v", name, got, tc.want)
		}
	}
	for name, tc := range map[string]struct {
		keys map[string]any
		says string
	}{
		"a file no credential path has": {map[string]any{"hosts.json": []any{"token"}}, `identity_keys names "hosts.json"`},
		"an empty list":                 {map[string]any{"auth.json": []any{}}, "identity_keys.auth.json is empty"},
		"an empty part":                 {map[string]any{"auth.json": []any{"tokens..account_id"}}, "is no key path"},
	} {
		if _, err := Load("codex", map[string]any{"identity_keys": tc.keys}); err == nil ||
			!strings.Contains(err.Error(), tc.says) {
			t.Errorf("%s: err = %v, want one saying %q", name, err, tc.says)
		}
	}
}

// EVERY HARNESS NAMES THE ACCOUNT ITS LOGIN IS, OR SAYS IT HAS NONE TO NAME:
// each built-in profile's identity_keys are the keys its measured login shape
// names an account at (or none, for a login of tokens alone), a new profile
// has to be placed here, and on each harness's own login shape
// (testdata/identity-logins.json) a refresh of the seeded account is one
// account with the seed, and another account's login is not.
//
// Mutation: drop or misname any profile's identity_keys, or add a profile
// without deciding its keys here.
func TestEveryHarnessNamesTheAccountItsLoginIs(t *testing.T) {
	t.Parallel()
	want := map[string]map[string][]string{
		"claude-code":  nil,
		"codex":        {"auth.json": {"tokens.account_id", "OPENAI_API_KEY"}},
		"gemini-cli":   {"google_accounts.json": {"active"}},
		"qwen-code":    nil,
		"opencode":     nil,
		"cursor-agent": {"cli-config.json": {"authInfo.userId"}},
		"copilot":      {"config.json": {"lastLoggedInUser", "loggedInUsers"}},
		"grok":         {"auth.json": {"*.user_id"}},
		"muse-code":    nil,
		"kimi-code":    nil,
		"hermes":       nil,
		"pi":           {"auth.json": {"openai-codex.accountId"}},
	}
	logins := identityLogins(t, "testdata/identity-logins.json")
	for _, name := range BuiltinNames() {
		keys, decided := want[name]
		if !decided {
			t.Errorf("%s: no decision on its identity_keys here; read its login shape and add it", name)
			continue
		}
		p, _ := Builtin(name)
		if !reflect.DeepEqual(p.IdentityKeys, keys) {
			t.Errorf("%s: identity_keys = %v, want %v", name, p.IdentityKeys, keys)
		}
		login, measured := logins[name]
		if len(keys) > 0 && !measured {
			t.Errorf("%s: names identity keys but has no login in testdata/identity-logins.json", name)
			continue
		}
		if same := sameAccount(p, login.Refresh, login.Seed); measured && !same {
			t.Errorf("%s: a refresh of the seeded account reads as another account", name)
		}
		if same := sameAccount(p, login.Other, login.Seed); measured && same {
			t.Errorf("%s: another account's login reads as the seeded one", name)
		}
	}
}

// sameAccount reports whether every file of login names seed's account at p's
// identity keys.
func sameAccount(p Profile, login, seed map[string]string) bool {
	for name, content := range login {
		if !SameLoginIdentity([]byte(content), []byte(seed[name]), p.IdentityKeys[name]) {
			return false
		}
	}
	return true
}

// identityLogin is one harness's login shapes in testdata/identity-logins.json,
// each a login by file name: as seeded, refreshed, and another account's.
type identityLogin struct {
	Seed    map[string]string `json:"seed"`
	Refresh map[string]string `json:"refresh"`
	Other   map[string]string `json:"other"`
}

// identityLogins reads the login shapes at path by harness.
func identityLogins(t *testing.T, path string) map[string]identityLogin {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var logins map[string]identityLogin
	if err := json.Unmarshal(raw, &logins); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return logins
}
