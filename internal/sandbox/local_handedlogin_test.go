//go:build unix

package sandbox

import (
	"encoding/base64"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/hostbox"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// A BOX CARRIES BACK ONLY THE LOGIN IT WAS HANDED: a credential store's
// document the box's run left is written back holding the handed login's
// refresh alone. Another account's tokens under the handed id, a login the
// run added, and a database replaced by another login whole leave the shared
// login as it was; a refresh beside an added login is written without it.
//
// Mutation: write the box's document back as it stands.
func TestABoxCarriesBackOnlyTheLoginItWasHanded(t *testing.T) {
	t.Parallel()
	jwt := func(account string, iat int) string {
		claims := `{"https://api.openai.com/auth":{"chatgpt_account_id":"` + account + `"},"iat":` +
			strconv.Itoa(iat) + `}`
		return "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString([]byte(claims)) + ".c2ln"
	}
	// Keys in order, on one line: the layout a carry writes the handed one in.
	cred := func(id, access, refresh string) string {
		return `{"active":true,"id":"` + id + `","integrationID":"openai","label":"subscription",` +
			`"value":{"access":"` + access + `","expires":1,"methodID":"chatgpt-browser","refresh":"` +
			refresh + `","type":"oauth"}}`
	}
	name := harnessprofile.CredentialStoreDocument
	dir := filepath.Join(t.TempDir(), "credentials")
	files := map[string]string{".crewlet/" + name: filepath.Join(dir, name)}
	handed := "[" + cred("cred_o", jwt("acct-a", 1), "rt-1") + "]"
	run := func(body string) string {
		t.Helper()
		putLogin(t, dir, map[string]string{name: handed})
		box, err := newDirect(t).Create(t.Context(), Spec{CredentialFiles: files, CredentialRules: map[string]harnessprofile.FileRule{
			".crewlet/" + name: {Name: name},
		}})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		writeFile(t, filepath.Join(box.Home(), ".crewlet", name), body)
		if err := box.Close(t.Context()); err != nil {
			t.Fatalf("Close: %v", err)
		}
		login, err := hostbox.ReadCredentials(dir, []string{name})
		if err != nil {
			t.Fatalf("ReadCredentials: %v", err)
		}
		return string(login[name])
	}
	refresh := "[" + cred("cred_o", jwt("acct-a", 2), "rt-2") + "]"
	if got := run(refresh); got != refresh {
		t.Errorf("the handed login's refresh left the shared login %s, want %s", got, refresh)
	}
	for what, body := range map[string]string{
		"another account under the handed id":  "[" + cred("cred_o", jwt("acct-b", 2), "rt-b") + "]",
		"a login the run added":                "[" + cred("cred_o", jwt("acct-a", 1), "rt-1") + "," + cred("cred_x", jwt("acct-b", 1), "rt-x") + "]",
		"a database replaced by another login": "[" + cred("cred_x", jwt("acct-b", 1), "rt-x") + "]",
	} {
		if got := run(body); got != handed {
			t.Errorf("%s left the shared login %s, want it as handed", what, got)
		}
	}
	added := "[" + cred("cred_o", jwt("acct-a", 2), "rt-2") + "," + cred("cred_x", jwt("acct-b", 1), "rt-x") + "]"
	if got := run(added); strings.Contains(got, "cred_x") || !strings.Contains(got, "rt-2") {
		t.Errorf("a refresh beside an added login left the shared login %s, want the refresh alone", got)
	}
}
