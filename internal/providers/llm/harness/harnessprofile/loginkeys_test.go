package harnessprofile

import (
	"encoding/json"
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
// a copy that is no JSON object names none it can be held to. Codex's profile
// names its account and stored key in auth.json, and identity_keys refuses a
// file no credential path has, an empty list and a key path with an empty part.
//
// Mutation: compare the whole file, read only the first key, take an absent
// key for a match against a present one, or drop codex's identity_keys.
func TestALoginsAccountIsReadAtItsIdentityKeys(t *testing.T) {
	t.Parallel()
	keys := []string{"tokens.account_id", "OPENAI_API_KEY"}
	seed := []byte(`{"OPENAI_API_KEY":null,"tokens":{"account_id":"acct-A","access_token":"a1"},"last_refresh":"1"}`)
	for copyOf, want := range map[string]bool{
		`{"OPENAI_API_KEY":null,"tokens":{"account_id":"acct-A","access_token":"a2"},"last_refresh":"2"}`: true,
		`{"OPENAI_API_KEY":null,"tokens":{"account_id":"acct-B","access_token":"b1"},"last_refresh":"2"}`: false,
		`{"OPENAI_API_KEY":null,"tokens":{"access_token":"a2"}}`:                                          false,
		`{"OPENAI_API_KEY":"sk-other","tokens":{"account_id":"acct-A","access_token":"a2"}}`:              false,
		`not json`: false,
	} {
		if got := SameLoginIdentity([]byte(copyOf), seed, keys); got != want {
			t.Errorf("SameLoginIdentity(%s) = %v, want %v", copyOf, got, want)
		}
	}
	if !SameLoginIdentity([]byte("not json"), seed, nil) {
		t.Error("a file with no identity keys is held to an account")
	}
	p, ok := Builtin("codex")
	if !ok {
		t.Fatal("no codex profile")
	}
	if !slices.Equal(p.IdentityKeys["auth.json"], keys) {
		t.Errorf("codex's identity_keys for auth.json are %v, want %v", p.IdentityKeys["auth.json"], keys)
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
