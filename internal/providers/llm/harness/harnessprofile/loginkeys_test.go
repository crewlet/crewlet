package harnessprofile

import (
	"encoding/json"
	"maps"
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
// only where each identity key holds alike in both, or is absent from both; a
// refresh that changes its tokens and keeps the account is one, a copy naming
// another account, dropping it or naming a key is not, and a copy that does not
// read in its format names none it can be held to. A `*` part holds every
// entry of the object there, its set of entries too, `//` lines are set aside,
// and a `.toml` file is read as TOML. identity_keys refuses a file no
// credential path has, an empty list and a key path with an empty part.
//
// Mutation: compare the whole file, read only the first key, take an absent
// key for a match against a present one, read `*` as a literal key, drop an
// entry's absence under `*`, read `//` lines as JSON, or read TOML as JSON.
func TestALoginsAccountIsReadAtItsIdentityKeys(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		file        string
		keys        []string
		seed, other string
		want        bool
	}{
		"a refresh keeping the account": {"auth.json", []string{"tokens.account_id", "OPENAI_API_KEY"},
			`{"OPENAI_API_KEY":null,"tokens":{"account_id":"acct-A","access_token":"a1"},"last_refresh":"1"}`,
			`{"OPENAI_API_KEY":null,"tokens":{"account_id":"acct-A","access_token":"a2"},"last_refresh":"2"}`, true},
		"another account": {"auth.json", []string{"tokens.account_id", "OPENAI_API_KEY"},
			`{"OPENAI_API_KEY":null,"tokens":{"account_id":"acct-A"}}`,
			`{"OPENAI_API_KEY":null,"tokens":{"account_id":"acct-B"}}`, false},
		"the account dropped": {"auth.json", []string{"tokens.account_id", "OPENAI_API_KEY"},
			`{"OPENAI_API_KEY":null,"tokens":{"account_id":"acct-A"}}`,
			`{"OPENAI_API_KEY":null,"tokens":{}}`, false},
		"the second key changed": {"auth.json", []string{"tokens.account_id", "OPENAI_API_KEY"},
			`{"OPENAI_API_KEY":null,"tokens":{"account_id":"acct-A"}}`,
			`{"OPENAI_API_KEY":"sk-other","tokens":{"account_id":"acct-A"}}`, false},
		"no JSON":          {"auth.json", []string{"tokens.account_id"}, `{"tokens":{"account_id":"acct-A"}}`, `not json`, false},
		"no keys, no JSON": {"auth.json", nil, `{"tokens":{"account_id":"acct-A"}}`, `not json`, true},
		"every entry's account kept": {"auth.json", []string{"*.user_id"},
			`{"https://a.example::c":{"user_id":"u1","key":"k1"}}`,
			`{"https://a.example::c":{"user_id":"u1","key":"k2"}}`, true},
		"an entry's account changed": {"auth.json", []string{"*.user_id"},
			`{"https://a.example::c":{"user_id":"u1"}}`, `{"https://a.example::c":{"user_id":"u2"}}`, false},
		"an entry added": {"auth.json", []string{"*.user_id"},
			`{"https://a.example::c":{"user_id":"u1"}}`,
			`{"https://a.example::c":{"user_id":"u1"},"https://b.example::c":{"user_id":"u2"}}`, false},
		"an entry's account dropped": {"auth.json", []string{"*.user_id"},
			`{"https://a.example::c":{"user_id":"u1"}}`, `{"https://a.example::c":{"key":"k2"}}`, false},
		"comment lines set aside": {"config.json", []string{"lastLoggedInUser"},
			"// managed\n{\"lastLoggedInUser\":{\"login\":\"seat\"}}\n",
			"// managed\n{\"lastLoggedInUser\":{\"login\":\"seat\"},\"firstLaunchAt\":\"x\"}\n", true},
		"a TOML table kept": {"config.toml", []string{"providers"},
			"[providers.p]\nbase_url = \"https://a.example\"\n",
			"[providers.p]\nbase_url = \"https://a.example\"\n\n[[hooks]]\nname = \"x\"\n", true},
		"a TOML table changed": {"config.toml", []string{"providers"},
			"[providers.p]\nbase_url = \"https://a.example\"\n", "[providers.p]\nbase_url = \"https://b.example\"\n", false},
	} {
		rule := FileRule{Name: tc.file, IdentityKeys: tc.keys}
		if got := rule.SameAccount([]byte(tc.other), []byte(tc.seed)); got != tc.want {
			t.Errorf("%s: SameAccount = %v, want %v", name, got, tc.want)
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

// A MIXED FILE TRAVELS AS ITS LOGIN ALONE: a home is seeded with only its
// login's fields, so no setting in the shared copy rides along; a refresh
// carries only those fields back, the shared copy's settings and `//` header
// kept; a setting a home wrote, beside a login it did not change, leaves the
// shared copy byte for byte; and a field the home dropped is dropped. A login
// file travels whole. login_keys refuses an empty list and an empty part.
//
// Mutation: seed a mixed file whole, carry a home's copy whole, take a
// setting from the home, drop the shared copy's settings or header, or keep a
// dropped login field.
func TestAMixedFileTravelsAsItsLoginAlone(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, shared, home string
		keys               []string
		carried            []string
	}{
		{"config.json", "// managed\n{\"tokens\":{\"a\":\"t1\"},\"user\":\"seat\",\"trusted\":[\"SHARED-SETTING\"]}\n",
			"{\"tokens\":{\"a\":\"t2\"},\"user\":\"seat\",\"trusted\":[\"PLANTED-SETTING\"]}\n",
			[]string{"tokens", "user"}, []string{"// managed\n{", `"a": "t2"`, "SHARED-SETTING"}},
		{"config.toml", "default_provider = \"p\"\n\n[providers.p]\nbase_url = \"https://a.example\"\n\n[[hooks]]\nname = \"SHARED-SETTING\"\n",
			"default_provider = \"q\"\n\n[providers.p]\nbase_url = \"https://a.example\"\n\n[[hooks]]\nname = \"PLANTED-SETTING\"\n",
			[]string{"default_provider", "providers"}, []string{`default_provider = "q"`, "https://a.example", "SHARED-SETTING"}},
	} {
		rule := FileRule{Name: tc.name, LoginKeys: tc.keys}
		seed, err := rule.Seed([]byte(tc.shared))
		if err != nil || strings.Contains(string(seed), "SHARED-SETTING") || !strings.Contains(string(seed), "seat") &&
			!strings.Contains(string(seed), "https://a.example") {
			t.Errorf("%s: seeded %q, %v; want its login alone", tc.name, seed, err)
		}
		got := string(rule.Carry([]byte(tc.home), []byte(tc.shared)))
		for _, part := range tc.carried {
			if !strings.Contains(got, part) {
				t.Errorf("%s: a refresh carried\n%s\nwant it holding %q", tc.name, got, part)
			}
		}
		if strings.Contains(got, "PLANTED-SETTING") {
			t.Errorf("%s: a refresh carried the home's setting:\n%s", tc.name, got)
		}
		if got := rule.Carry(seed, []byte(tc.shared)); string(got) != tc.shared {
			t.Errorf("%s: its own seed carried %q, want the shared copy as it is", tc.name, got)
		}
		planted := strings.Replace(tc.shared, "SHARED-SETTING", "PLANTED-SETTING", 1)
		if got := rule.Carry([]byte(planted), []byte(tc.shared)); string(got) != tc.shared {
			t.Errorf("%s: a setting the home wrote carried %q, want the shared copy as it is", tc.name, got)
		}
	}
	dropped := FileRule{Name: "config.json", LoginKeys: []string{"tokens", "user"}}
	if got := dropped.Carry([]byte(`{"user":"seat"}`), []byte(`{"tokens":{"a":"t1"},"user":"seat","x":1}`)); string(got) != "{\n  \"user\": \"seat\",\n  \"x\": 1\n}\n" {
		t.Errorf("a login field the home dropped carried %s, want it dropped", got)
	}
	whole := FileRule{Name: "auth.json"}
	if seed, _ := whole.Seed([]byte("anything")); string(seed) != "anything" {
		t.Errorf("a login file seeded %q, want it whole", seed)
	}
	for name, tc := range map[string]struct {
		keys map[string]any
		says string
	}{
		"an empty list": {map[string]any{"config.json": []any{}}, "login_keys.config.json is empty"},
		"an empty part": {map[string]any{"config.json": []any{"lastLoggedInUser..login"}}, "is no key path"},
	} {
		if _, err := Load("copilot", map[string]any{"login_keys": tc.keys}); err == nil ||
			!strings.Contains(err.Error(), tc.says) {
			t.Errorf("%s: err = %v, want one saying %q", name, err, tc.says)
		}
	}
}

// EVERY HARNESS SAYS WHICH OF ITS FILES ARE MIXED AND WHICH LOGIN IT IS: each
// built-in profile's login_keys are the login fields of each file it also
// keeps settings in, and its identity_keys the keys its measured login shape
// names a login at (or none, for a login of tokens alone); a new profile has
// to be placed here. On each harness's own login shapes (testdata/logins.json)
// a refresh of the seeded account is one login with the seed, another
// account's is not, and a mixed file is seeded without the shared copy's
// settings.
//
// Mutation: drop or misname any profile's login_keys or identity_keys, or add
// a profile without deciding them here.
func TestEveryHarnessSaysWhichLoginItsFilesAre(t *testing.T) {
	t.Parallel()
	kimiTables := []string{"default_model", "default_provider", "models", "providers", "services", "thinking"}
	want := map[string]struct{ login, identity map[string][]string }{
		"claude-code": {},
		"codex":       {identity: map[string][]string{"auth.json": {"tokens.account_id", "OPENAI_API_KEY"}}},
		"gemini-cli":  {identity: map[string][]string{"google_accounts.json": {"active"}}},
		"qwen-code":   {},
		"opencode":    {},
		"cursor-agent": {login: map[string][]string{"cli-config.json": {"authInfo"}},
			identity: map[string][]string{"cli-config.json": {"authInfo.userId"}}},
		"copilot": {
			login:    map[string][]string{"config.json": {"copilotTokens", "authTokens", "lastLoggedInUser", "loggedInUsers"}},
			identity: map[string][]string{"config.json": {"lastLoggedInUser", "loggedInUsers"}}},
		"grok":      {identity: map[string][]string{"auth.json": {"*.user_id"}}},
		"muse-code": {},
		"kimi-code": {login: map[string][]string{"config.toml": kimiTables},
			identity: map[string][]string{"config.toml": {"default_provider", "providers", "services"}}},
		"hermes": {},
		"pi":     {identity: map[string][]string{"auth.json": {"openai-codex.accountId"}}},
	}
	logins := testLogins(t, "testdata/logins.json")
	for _, name := range BuiltinNames() {
		w, decided := want[name]
		if !decided {
			t.Errorf("%s: no decision on its login_keys and identity_keys here; read its login shape and add it", name)
			continue
		}
		p, _ := Builtin(name)
		if name == "hermes" {
			// Its .env's keys are 0.23.3's provider registry, read whole: held
			// here to the keys a seat signs in with and to no base URL.
			env := p.LoginKeys[".env"]
			for _, key := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "OPENROUTER_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"} {
				if !slices.Contains(env, key) {
					t.Errorf("hermes: .env's login_keys lack %s", key)
				}
			}
			if slices.ContainsFunc(env, func(k string) bool { return !strings.HasSuffix(k, "_KEY") && !strings.HasSuffix(k, "_TOKEN") }) {
				t.Errorf("hermes: .env's login_keys %v name a variable that is no key", env)
			}
			w.login = map[string][]string{".env": env}
			w.identity = map[string][]string{".env": env}
		}
		if !reflect.DeepEqual(p.LoginKeys, w.login) {
			t.Errorf("%s: login_keys = %v, want %v", name, p.LoginKeys, w.login)
		}
		if !reflect.DeepEqual(p.IdentityKeys, w.identity) {
			t.Errorf("%s: identity_keys = %v, want %v", name, p.IdentityKeys, w.identity)
		}
		login, measured := logins[name]
		if (len(w.login) > 0 || len(w.identity) > 0) && !measured {
			t.Errorf("%s: names login or identity keys but has no login in testdata/logins.json", name)
			continue
		}
		if !measured {
			continue
		}
		if !sameAccount(p, login.Refresh, login.Seed) {
			t.Errorf("%s: a refresh of the seeded account reads as another login", name)
		}
		if sameAccount(p, login.Other, login.Seed) {
			t.Errorf("%s: another account's login reads as the seeded one", name)
		}
		for file, content := range login.Seed {
			seed, err := p.FileRule(file).Seed([]byte(content))
			if err != nil || strings.Contains(string(seed), "SHARED-SETTING") {
				t.Errorf("%s: %s seeded %q, %v; want no setting of the shared copy", name, file, seed, err)
			}
		}
	}
}

// sameAccount reports whether every file of login names seed's login at p's
// identity keys.
func sameAccount(p Profile, login, seed map[string]string) bool {
	for name, content := range login {
		if !p.FileRule(name).SameAccount([]byte(content), []byte(seed[name])) {
			return false
		}
	}
	return true
}

// testLogin is one harness's login shapes in testdata/logins.json, each a
// login by file name: as shared, refreshed in a home, another account's, and,
// for a mixed file, the seed's login with a setting a home wrote.
type testLogin struct {
	Seed    map[string]string `json:"seed"`
	Refresh map[string]string `json:"refresh"`
	Other   map[string]string `json:"other"`
	Planted map[string]string `json:"planted"`
}

// testLogins reads the login shapes at path by harness.
func testLogins(t *testing.T, path string) map[string]testLogin {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var logins map[string]testLogin
	if err := json.Unmarshal(raw, &logins); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return logins
}

// A .ENV IS READ AS HERMES READS IT, AND WRITTEN SO IT READS BACK: comments and
// blank lines skipped, an `export ` prefix, spaces around the name, a later
// line winning, double quotes with their escapes, single quotes as written, an
// unquoted value ending at a `#` after a space, a quoted one at its closing
// quote; what encodeDotenv writes parses to the same variables.
//
// Mutation: keep a comment line, keep the export prefix, cut an unquoted value
// at a `#` with no space before it, leave a double-quoted value's escapes in,
// or write a value with a space unquoted.
func TestADotenvIsReadAsHermesReadsIt(t *testing.T) {
	t.Parallel()
	got := parseDotenv([]byte("\ufeff# a comment\n\nexport A=1\n  B = two  \nC=\"x \\\"q\\\" \\\\ y\" # note\n" +
		"D='a # b'\nE=foo#bar\nF=val # trailing\nA=3\nnoequals\n=novalue\nG=\"open\n"))
	want := map[string]string{"A": "3", "B": "two", "C": `x "q" \ y`, "D": "a # b", "E": "foo#bar", "F": "val", "G": `"open`}
	if !maps.Equal(got, want) {
		t.Errorf("parseDotenv = %v, want %v", got, want)
	}
	vars := map[string]any{"PLAIN": "abc", "SPACED": "a b", "HASH": "a#b", "QUOTE": `say "hi" \ 'x'`, "EMPTY": ""}
	back := parseDotenv(encodeDotenv(vars))
	for key, value := range vars {
		if back[key] != value {
			t.Errorf("%s written and read back is %q, want %q", key, back[key], value)
		}
	}
}
