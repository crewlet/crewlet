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
