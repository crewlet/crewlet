package harnessprofile

import (
	"encoding/base64"
	"encoding/json"
	"reflect"
	"testing"
)

// accessFor is an access token issued to account: a JWT whose namespaced
// claim names it, as an OpenAI subscription's does, stamped iat.
func accessFor(account string, iat int) string {
	claims, _ := json.Marshal(map[string]any{
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": account, "user_id": "user-" + account},
		"iat":                         iat,
	})
	return "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(claims) + ".c2ln"
}

// storeDoc is a store document holding creds, as OpenCode's export prints one.
func storeDoc(t *testing.T, creds ...map[string]any) []byte {
	t.Helper()
	out, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func oauthCred(id, access, refresh string, expires int) map[string]any {
	return map[string]any{"id": id, "integrationID": "openai", "label": "subscription", "active": true,
		"value": map[string]any{"type": "oauth", "methodID": "chatgpt-browser", "access": access,
			"refresh": refresh, "expires": expires}}
}

func keyCred(id, k string) map[string]any {
	return map[string]any{"id": id, "integrationID": "anthropic", "label": "API key", "active": true,
		"value": map[string]any{"type": "key", "key": k}}
}

// A RUN CARRIES BACK ONLY THE LOGIN IT WAS HANDED: the handed credential's
// refresh, and nothing of a credential it gained, a key it changed, another
// account's tokens under the handed id, a changed field a refresh never
// rewrites, or a document that is no store's.
//
// Mutation: carry home whole, match by id alone, or drop the subject check,
// and a row fails.
func TestARunCarriesBackOnlyTheLoginItWasHanded(t *testing.T) {
	t.Parallel()
	handed := storeDoc(t, oauthCred("cred_o", accessFor("acct-a", 1), "rt-1", 100), keyCred("cred_k", "sk-ant-handed"))
	refreshedOnly := storeDoc(t, oauthCred("cred_o", accessFor("acct-a", 2), "rt-2", 200), keyCred("cred_k", "sk-ant-handed"))
	withMetadata := oauthCred("cred_o", accessFor("acct-a", 2), "rt-2", 200)
	withMetadata["value"].(map[string]any)["metadata"] = map[string]any{"accountID": "acct-a"}
	moved := oauthCred("cred_o", accessFor("acct-a", 2), "rt-2", 200)
	moved["value"].(map[string]any)["enterpriseUrl"] = "https://elsewhere.example.com"
	cases := []struct {
		name string
		home []byte
		want []byte
	}{
		{"a refresh of the handed login", refreshedOnly, refreshedOnly},
		{"a refresh that filled in metadata", storeDoc(t, withMetadata, keyCred("cred_k", "sk-ant-handed")),
			storeDoc(t, withMetadata, keyCred("cred_k", "sk-ant-handed"))},
		{"a credential the run gained", storeDoc(t, oauthCred("cred_o", accessFor("acct-a", 2), "rt-2", 200),
			keyCred("cred_k", "sk-ant-handed"), keyCred("cred_new", "sk-ant-other")), refreshedOnly},
		{"another account under the handed id", storeDoc(t, oauthCred("cred_o", accessFor("acct-b", 2), "rt-b", 200),
			keyCred("cred_k", "sk-ant-handed")), handed},
		{"an opaque token where a JWT was", storeDoc(t, oauthCred("cred_o", "opaque", "rt-2", 200),
			keyCred("cred_k", "sk-ant-handed")), handed},
		{"a changed key", storeDoc(t, oauthCred("cred_o", accessFor("acct-a", 1), "rt-1", 100),
			keyCred("cred_k", "sk-ant-other")), handed},
		{"a field a refresh never rewrites", storeDoc(t, moved, keyCred("cred_k", "sk-ant-handed")), handed},
		{"a database replaced by another login", storeDoc(t, keyCred("cred_x", "sk-ant-other")), handed},
		{"no store document", []byte(`{"id":"cred_o"}`), handed},
		{"a document naming an id twice", storeDoc(t, oauthCred("cred_o", accessFor("acct-a", 2), "rt-2", 200),
			oauthCred("cred_o", accessFor("acct-b", 2), "rt-b", 200)), handed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := KeepHandedCredentials(c.home, handed)
			var g, w any
			if json.Unmarshal(got, &g) != nil || json.Unmarshal(c.want, &w) != nil || !reflect.DeepEqual(g, w) {
				t.Errorf("carried back\n%s\nwant\n%s", got, c.want)
			}
		})
	}
	if got := KeepHandedCredentials(refreshedOnly, handed); string(got) != string(refreshedOnly) {
		t.Errorf("a plain refresh was rewritten rather than carried as the run exported it:\n%s", got)
	}
	if got := KeepHandedCredentials(handed, handed); string(got) != string(handed) {
		t.Errorf("an unchanged document came back as\n%s", got)
	}
}

// AN OPAQUE TOKEN'S REFRESH IS CARRIED where every other field agrees: it
// names no account to compare, and a login whose tokens never refresh back
// logs the fleet out at its next expiry.
//
// Mutation: refuse a token that is no JWT.
func TestAnOpaqueLoginsRefreshIsCarried(t *testing.T) {
	t.Parallel()
	handed := storeDoc(t, oauthCred("cred_g", "gho_one", "gho_one", 0))
	home := storeDoc(t, oauthCred("cred_g", "gho_two", "gho_two", 5))
	if got := KeepHandedCredentials(home, handed); string(got) != string(home) {
		t.Errorf("carried back\n%s\nwant the refresh\n%s", got, home)
	}
}

// A CREDENTIAL OF ANY OTHER SHAPE IS CARRIED WHERE IT IS STILL THE SAME LOGIN:
// its refresh, whatever field the store keeps its tokens in, but not another
// integration's or label's credential under the handed id, a JWT naming
// another account, or a credential the run added.
//
// Mutation: hold an untyped credential to OpenCode's fields, or carry it whole
// whatever its names and subjects.
func TestAnUntypedCredentialsRefreshIsCarried(t *testing.T) {
	t.Parallel()
	handed := []byte(`[{"id":"c1","integrationID":"x","key":"k-old","token":"` + accessFor("acct-a", 1) + `"}]` + "\n")
	cases := []struct {
		name, home string
		carried    bool
	}{
		{"a refresh", `[{"id":"c1","integrationID":"x","key":"k-new","token":"` + accessFor("acct-a", 2) + `"}]`, true},
		{"another account", `[{"id":"c1","integrationID":"x","key":"k-new","token":"` + accessFor("acct-b", 2) + `"}]`, false},
		{"another integration", `[{"id":"c1","integrationID":"y","key":"k-new","token":"` + accessFor("acct-a", 2) + `"}]`, false},
		{"an added credential", `[{"id":"c1","integrationID":"x","key":"k-old","token":"` + accessFor("acct-a", 1) + `"},{"id":"c2","key":"k-x"}]`, false},
	}
	for _, c := range cases {
		got := KeepHandedCredentials([]byte(c.home), handed)
		if carried := string(got) == c.home; carried != c.carried {
			t.Errorf("%s: carried back\n%s\nwant carried=%t", c.name, got, c.carried)
		}
		if !c.carried && string(got) != string(handed) {
			t.Errorf("%s: carried back\n%s\nwant the handed document", c.name, got)
		}
	}
	refresh := []byte(`[{"id":"c","refresh":"R1"}]`)
	if got := KeepHandedCredentials(refresh, []byte(`[{"id":"c","refresh":"R0"}]`)); string(got) != string(refresh) {
		t.Errorf("a store keeping its token at the top level carried back %s, want its refresh", got)
	}
}

// ONLY A STORE'S DOCUMENT IS HELD TO THE HANDED LOGIN: Profile.KeepLogin
// filters the file named CredentialStoreDocument, a name no credential path
// may take; any other file carries only its refresh keys, and one naming none
// carries nothing.
//
// Mutation: filter every file, or none.
func TestOnlyAStoresDocumentIsHeldToTheHandedLogin(t *testing.T) {
	t.Parallel()
	handed := storeDoc(t, keyCred("cred_k", "sk-ant-handed"))
	home := storeDoc(t, keyCred("cred_k", "sk-ant-handed"), keyCred("cred_new", "sk-ant-other"))
	store := &Profile{CredentialStore: &CredentialStore{}}
	if got, err := store.KeepLogin(CredentialStoreDocument, home, handed); err != nil || string(got) != string(handed) {
		t.Errorf("a store's document carried back %s, %v", got, err)
	}
	if got, err := store.KeepLogin("auth.json", home, handed); err != nil || string(got) != string(handed) {
		t.Errorf("a file naming no refresh keys carried back %s, %v; want nothing", got, err)
	}
}
