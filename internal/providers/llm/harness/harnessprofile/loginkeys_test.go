package harnessprofile

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// testJWT is an unsigned JWT carrying claims, the shape a harness's token
// takes; nothing here checks a signature.
func testJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(raw) + ".c2ln"
}

// A LOGIN'S ACCOUNT IS READ AT ITS IDENTITY KEYS: two copies are one account
// only where each identity key holds alike in both, or is absent from both; a
// refresh that changes its tokens and keeps the account is one, a copy naming
// another account, dropping it or naming a key is not, a token whose subject
// claims name another account is not, and a copy that does not read in its
// format names none. `*` holds every entry, `//` lines are set aside, `.toml`
// is TOML. identity_keys refuses a file no credential path has, an empty list,
// an empty part and `@subject` anywhere but last.
//
// Mutation: compare the whole file, read only the first key, take an absent
// key for a match, read `*` or `@subject` as a literal key, compare a token
// whole rather than its subject, or read TOML as JSON.
func TestALoginsAccountIsReadAtItsIdentityKeys(t *testing.T) {
	t.Parallel()
	seat := testJWT(t, map[string]any{"sub": "u-seat", "iat": 1})
	seatAgain := testJWT(t, map[string]any{"sub": "u-seat", "iat": 2})
	other := testJWT(t, map[string]any{"sub": "u-other", "iat": 2})
	for name, tc := range map[string]struct {
		file        string
		keys        []string
		seed, other string
		want        bool
	}{
		"a refresh keeping the account": {"auth.json", []string{"tokens.account_id", "OPENAI_API_KEY"},
			`{"OPENAI_API_KEY":null,"tokens":{"account_id":"acct-A","access_token":"a1"}}`,
			`{"OPENAI_API_KEY":null,"tokens":{"account_id":"acct-A","access_token":"a2"}}`, true},
		"another account": {"auth.json", []string{"tokens.account_id"},
			`{"tokens":{"account_id":"acct-A"}}`, `{"tokens":{"account_id":"acct-B"}}`, false},
		"the account dropped": {"auth.json", []string{"tokens.account_id"},
			`{"tokens":{"account_id":"acct-A"}}`, `{"tokens":{}}`, false},
		"the second key changed": {"auth.json", []string{"tokens.account_id", "OPENAI_API_KEY"},
			`{"OPENAI_API_KEY":null,"tokens":{"account_id":"acct-A"}}`,
			`{"OPENAI_API_KEY":"sk-other","tokens":{"account_id":"acct-A"}}`, false},
		"no JSON":          {"auth.json", []string{"tokens.account_id"}, `{"tokens":{"account_id":"acct-A"}}`, `not json`, false},
		"no keys, no JSON": {"auth.json", nil, `{"tokens":{"account_id":"acct-A"}}`, `not json`, true},
		"a key named twice": {"auth.json", []string{"user"},
			`{"user":"seat"}`, `{"user":"seat","user":"other"}`, false},
		"every entry's account kept": {"auth.json", []string{"*.user_id"},
			`{"https://a.example::c":{"user_id":"u1","key":"k1"}}`,
			`{"https://a.example::c":{"user_id":"u1","key":"k2"}}`, true},
		"an entry added": {"auth.json", []string{"*.user_id"},
			`{"https://a.example::c":{"user_id":"u1"}}`,
			`{"https://a.example::c":{"user_id":"u1"},"https://b.example::c":{"user_id":"u2"}}`, false},
		"an entry's account dropped": {"auth.json", []string{"*.user_id"},
			`{"https://a.example::c":{"user_id":"u1"}}`, `{"https://a.example::c":{"key":"k2"}}`, false},
		"a token refreshed for its subject": {"auth.json", []string{"accessToken.@subject"},
			`{"accessToken":"` + seat + `"}`, `{"accessToken":"` + seatAgain + `"}`, true},
		"a token of another subject": {"auth.json", []string{"accessToken.@subject"},
			`{"accessToken":"` + seat + `"}`, `{"accessToken":"` + other + `"}`, false},
		"a token no longer a JWT": {"auth.json", []string{"accessToken.@subject"},
			`{"accessToken":"` + seat + `"}`, `{"accessToken":"opaque"}`, false},
		"opaque tokens alike": {"auth.json", []string{"accessToken.@subject"},
			`{"accessToken":"opaque-1"}`, `{"accessToken":"opaque-2"}`, true},
		"comment lines set aside": {"config.json", []string{"lastLoggedInUser"},
			"// managed\n{\"lastLoggedInUser\":{\"login\":\"seat\"}}\n",
			"// managed\n{\"lastLoggedInUser\":{\"login\":\"seat\"},\"firstLaunchAt\":\"x\"}\n", true},
		"a TOML table kept": {"config.toml", []string{"providers"},
			"[providers.p]\nbase_url = \"https://a.example\"\n",
			"[providers.p]\nbase_url = \"https://a.example\"\n\n[[hooks]]\nname = \"x\"\n", true},
		"a TOML table changed inline": {"config.toml", []string{"providers"},
			"[providers.p]\nbase_url = \"https://a.example\"\n", "providers = { p = { base_url = \"https://b.example\" } }\n", false},
	} {
		// config.json stands for Copilot's, the one file declared with comment lines.
		rule := FileRule{Name: tc.file, IdentityKeys: tc.keys, CommentLines: tc.file == "config.json"}
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
		"a subject not last":            {map[string]any{"auth.json": []any{"tokens.@subject.x"}}, "is no key path"},
	} {
		if _, err := Load("codex", map[string]any{"identity_keys": tc.keys}); err == nil ||
			!strings.Contains(err.Error(), tc.says) {
			t.Errorf("%s: err = %v, want one saying %q", name, err, tc.says)
		}
	}
}

// A REFRESH CARRIES ITS TOKENS ALONE AND FAILS CLOSED: a home is seeded with a
// mixed file's login fields alone; a refresh carries exactly its refresh keys'
// leaves the shared copy holds, of the same kind, in the shared copy's layout
// and never as the home's bytes; anything else stays as shared has it, a key
// ending in `*` carries nothing; and a copy either side cannot read (malformed,
// a key twice, anything after it, a `//` line where none is declared) carries
// nothing, saying why. refresh_keys refuses a path ending in `*`.
//
// Mutation: seed a mixed file whole, carry a copy whole or as its bytes, carry
// an added leaf or one of another kind, read `//` lines in every file, or take
// a trailing `*`.
func TestARefreshCarriesItsTokensAloneAndFailsClosed(t *testing.T) {
	t.Parallel()
	mixed := FileRule{Name: "config.toml", LoginKeys: []string{"default_provider", "providers"}}
	seed, err := mixed.Seed([]byte("default_provider = \"p\"\n\n[providers.p]\nbase_url = \"https://a.example\"\n\n" +
		"[[hooks]]\nname = \"SHARED-SETTING\"\n"))
	if err != nil || strings.Contains(string(seed), "SHARED-SETTING") || !strings.Contains(string(seed), "https://a.example") {
		t.Errorf("a mixed file seeded %q, %v; want its login alone", seed, err)
	}
	rule := FileRule{Name: "auth.json", RefreshKeys: []string{"token", "*.access", "pool.*.access", "n", "added", "x"}}
	shared := `{"token":"t1","n":1,"url":"https://a.example","x":{"access":"a1","type":"oauth"},"pool":[{"access":"p1"}]}`
	for name, tc := range map[string]struct{ home, want string }{
		"tokens alone, in the shared copy's layout": {`{"token":"t2","n":2,"url":"https://b.example",` +
			`"x":{"access":"a2","type":"key"},"y":{"access":"added"},"pool":[{"access":"p2"},{"access":"added"}],"added":1}`,
			`{"n":2,"pool":[{"access":"p2"}],"token":"t2","url":"https://a.example","x":{"access":"a2","type":"oauth"}}`},
		"a token dropped": {`{"n":1,"url":"https://a.example","x":{"access":"a1","type":"oauth"},"pool":[{"access":"p1"}]}`, shared},
		"its own copy":    {shared, shared},
		"a leaf of another kind": {`{"token":{"t":"t2"},"n":"2","url":"https://a.example","x":{"access":"a1","type":"oauth"},"pool":[{"access":"p1"}]}`,
			shared},
		"the home's bytes, every field one": {"{\"pool\":[{\"access\":\"p1\"}],\"token\":\"t2\",\"n\":1,\n\"url\":\"https://a.example\"," +
			"\"x\":{\"access\":\"a1\",\"type\":\"oauth\"}}",
			`{"n":1,"pool":[{"access":"p1"}],"token":"t2","url":"https://a.example","x":{"access":"a1","type":"oauth"}}`},
	} {
		if got, err := rule.Carry([]byte(tc.home), []byte(shared)); err != nil || string(got) != tc.want {
			t.Errorf("%s: carried\n%s\n%v; want\n%s", name, got, err, tc.want)
		}
	}
	indented := "{\n  \"token\": \"t1\"\n}\n"
	if got, err := rule.Carry([]byte(`{"token":"t2"}`), []byte(indented)); err != nil || string(got) != "{\n  \"token\": \"t2\"\n}\n" {
		t.Errorf("over an indented copy carried %q, %v; want it indented", got, err)
	}
	commented := FileRule{Name: "config.json", RefreshKeys: []string{"token"}, CommentLines: true}
	if got, err := commented.Carry([]byte("// box\n{\"token\":\"t2\"}\n"), []byte("// shared\n{\"token\":\"t1\"}\n")); err != nil ||
		string(got) != "// shared\n{\"token\":\"t2\"}\n" {
		t.Errorf("a file with comment lines carried %q, %v; want the shared header over the token", got, err)
	}
	if got, err := rule.Carry([]byte("// a line\n{\"token\":\"t2\"}"), []byte(shared)); err == nil || string(got) != shared {
		t.Errorf("a comment line in a file without them carried %s, %v; want nothing and why", got, err)
	}
	if got, err := (FileRule{Name: "auth.json", RefreshKeys: []string{"*"}}).Carry([]byte(`{"a":"2"}`), []byte(`{"a":"1"}`)); err != nil ||
		string(got) != `{"a":"1"}` {
		t.Errorf("a refresh key ending in * carried %s, %v; want nothing", got, err)
	}
	carryKey(map[string]any{"a": "1"}, map[string]any{"a": "2"}, nil)
	for name, home := range map[string]string{
		"malformed":         `{"token":`,
		"a key named twice": `{"token":"t2","token":"t3"}`,
		"after the object":  `{"token":"t2"} {"token":"t3"}`,
	} {
		if got, err := rule.Carry([]byte(home), []byte(shared)); err == nil || string(got) != shared {
			t.Errorf("%s: carried %s, %v; want the shared copy and why", name, got, err)
		}
	}
	if got, err := rule.Carry([]byte(`{"token":"t2"}`), []byte("not json")); err == nil || string(got) != "not json" {
		t.Errorf("over a shared copy that does not read: carried %s, %v; want it and why", got, err)
	}
	if got, err := (FileRule{Name: "auth.json"}).Carry([]byte(`{"token":"t2"}`), []byte(shared)); err != nil || string(got) != shared {
		t.Errorf("a file naming no refresh keys carried %s, %v; want nothing", got, err)
	}
	for name, tc := range map[string]struct {
		keys map[string]any
		says string
	}{
		"login_keys empty":        {map[string]any{"login_keys": map[string]any{"config.json": []any{}}}, "login_keys.config.json is empty"},
		"login_keys empty part":   {map[string]any{"login_keys": map[string]any{"config.json": []any{"a..b"}}}, "is no key path"},
		"refresh_keys elsewhere":  {map[string]any{"refresh_keys": map[string]any{"hosts.json": []any{"t"}}}, `refresh_keys names "hosts.json"`},
		"refresh_keys empty":      {map[string]any{"refresh_keys": map[string]any{"config.json": []any{}}}, "refresh_keys.config.json is empty"},
		"refresh_keys a subject":  {map[string]any{"refresh_keys": map[string]any{"config.json": []any{"t.@subject"}}}, "is no key path"},
		"refresh_keys ending *":   {map[string]any{"refresh_keys": map[string]any{"config.json": []any{"copilotTokens.*"}}}, "is no key path"},
		"refresh_keys a lone *":   {map[string]any{"refresh_keys": map[string]any{"config.json": []any{"*"}}}, "is no key path"},
		"comment_lines elsewhere": {map[string]any{"comment_lines": []any{"hosts.json"}}, `comment_lines[0] "hosts.json"`},
	} {
		if _, err := Load("copilot", tc.keys); err == nil || !strings.Contains(err.Error(), tc.says) {
			t.Errorf("%s: err = %v, want one saying %q", name, err, tc.says)
		}
	}
}

// EVERY HARNESS SAYS HOW EACH LOGIN FILE TRAVELS: each built-in profile's
// login_keys (the login fields of a file it also keeps settings in),
// refresh_keys (the token fields a refresh carries back) and identity_keys
// (what names the login) are decided here, a new profile included. On each
// harness's own login shapes (testdata/logins.json) a refresh of the seeded
// account is one login with the seed and carries exactly what the fixture
// writes out, another account's or a swapped token's is not one, and a mixed
// file is seeded without the shared copy's settings.
//
// Mutation: drop or misname any profile's keys, or add a profile without
// deciding them here.
func TestEveryHarnessSaysHowEachLoginFileTravels(t *testing.T) {
	t.Parallel()
	type keys = map[string][]string
	kimiTables := []string{"default_model", "default_provider", "models", "providers", "services", "thinking"}
	want := map[string]struct{ login, refresh, identity keys }{
		"claude-code": {refresh: keys{".credentials.json": {"claudeAiOauth.accessToken", "claudeAiOauth.refreshToken", "claudeAiOauth.expiresAt"}}},
		"codex": {refresh: keys{"auth.json": {"tokens.id_token", "tokens.access_token", "tokens.refresh_token", "last_refresh"}},
			identity: keys{"auth.json": {"tokens.account_id", "OPENAI_API_KEY", "tokens.id_token.@subject"}}},
		"gemini-cli": {refresh: keys{"oauth_creds.json": {"access_token", "refresh_token", "expiry_date", "id_token"}},
			identity: keys{"google_accounts.json": {"active"}, "oauth_creds.json": {"id_token.@subject"}}},
		"qwen-code": {refresh: keys{"subscription_credentials.json": {"tokens.access_token", "tokens.refresh_token", "tokens.expiresAt", "tokens.expires_in"}},
			identity: keys{"subscription_credentials.json": {"tokens.access_token.@subject"}}},
		"opencode": {},
		"custom":   {},
		"cursor-agent": {login: keys{"cli-config.json": {"authInfo"}}, refresh: keys{"auth.json": {"accessToken", "refreshToken"}},
			identity: keys{"cli-config.json": {"authInfo.userId"}, "auth.json": {"accessToken.@subject"}}},
		"copilot": {login: keys{"config.json": {"copilotTokens", "authTokens", "lastLoggedInUser", "loggedInUsers"}},
			identity: keys{"config.json": {"lastLoggedInUser", "loggedInUsers"}}},
		"grok": {refresh: keys{"auth.json": {"*.key", "*.refresh_token", "*.expires_at"}},
			identity: keys{"auth.json": {"*.user_id", "*.key.@subject"}}},
		"muse-code": {refresh: keys{"auth.json": {"providers.meta.access_token", "providers.meta.refresh_token", "providers.meta.expires_at"}},
			identity: keys{"auth.json": {"providers.meta.access_token.@subject"}}},
		"kimi-code": {login: keys{"config.toml": kimiTables}, refresh: keys{"kimi-code.json": {"access_token", "refresh_token", "expires_at", "expires_in"}},
			identity: keys{"config.toml": {"default_provider", "providers", "services"}, "kimi-code.json": {"access_token.@subject"}}},
		"hermes": {refresh: keys{
			"auth.json": {"providers.nous.access_token", "providers.nous.refresh_token", "providers.nous.obtained_at",
				"providers.nous.expires_at", "providers.nous.expires_in"},
			"nous_auth.json": {"access_token", "refresh_token", "obtained_at", "expires_at"}},
			identity: keys{"auth.json": {"providers.nous.user_id", "providers.nous.access_token.@subject"},
				"nous_auth.json": {"user_id", "access_token.@subject"}}},
		"pi": {refresh: keys{"auth.json": {"*.access", "*.refresh", "*.expires"}},
			identity: keys{"auth.json": {"openai-codex.accountId", "*.access.@subject"}}},
	}
	logins := testLogins(t, "testdata/logins.json")
	for _, name := range BuiltinNames() {
		w, decided := want[name]
		if !decided {
			t.Errorf("%s: no decision on its login, refresh and identity keys here; read its login shape and add it", name)
			continue
		}
		p, _ := Builtin(name)
		if name == "hermes" {
			// Its .env's keys are 0.23.4's provider registry, read whole: held
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
			w.login = keys{".env": env}
			w.identity[".env"] = env
		}
		for field, pair := range map[string][2]keys{
			"login_keys": {p.LoginKeys, w.login}, "refresh_keys": {p.RefreshKeys, w.refresh}, "identity_keys": {p.IdentityKeys, w.identity},
		} {
			if !reflect.DeepEqual(pair[0], pair[1]) {
				t.Errorf("%s: %s = %v, want %v", name, field, pair[0], pair[1])
			}
		}
		login, measured := logins[name]
		if !measured {
			// A profile naming no login file (a credential store's, the
			// operator's own template) has no file shapes to hold.
			if len(p.CredentialPaths) > 0 {
				t.Errorf("%s: no login shapes in testdata/logins.json", name)
			}
			continue
		}
		if !sameAccount(p, login.Refresh, login.Seed) {
			t.Errorf("%s: a refresh of the seeded account reads as another login", name)
		}
		for _, foreign := range []map[string]string{login.Other, login.Swap} {
			if foreign != nil && sameAccount(p, foreign, login.Seed) {
				t.Errorf("%s: another account's login reads as the seeded one", name)
			}
		}
		for file, content := range login.Seed {
			seed, err := p.FileRule(file).Seed([]byte(content))
			if err != nil || strings.Contains(string(seed), "SHARED-SETTING") {
				t.Errorf("%s: %s seeded %q, %v; want no setting of the shared copy", name, file, seed, err)
			}
			got, err := p.FileRule(file).Carry([]byte(login.Refresh[file]), []byte(content))
			if err != nil || string(got) != login.Carried[file] {
				t.Errorf("%s: %s's refresh carried\n%s\n%v; want\n%s", name, file, got, err, login.Carried[file])
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
// login by file name: as shared, refreshed in a home and the shared login
// that refresh carries to (written out), another account's, one whose token
// file alone names another, the seed's login beside a setting a home wrote,
// and copies that carry nothing.
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
