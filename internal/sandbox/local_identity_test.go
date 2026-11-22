//go:build unix

package sandbox

import (
	"path/filepath"
	"testing"
)

// A BOX WRITES BACK ONLY THE ACCOUNT IT WAS SEEDED WITH: a login file whose
// identity keys ([Spec.CredentialIdentityKeys]) name another account at
// teardown, as a copy placed in the box's home during the run would, is no
// refresh of the shared login and leaves it as it was; a refresh of the
// seeded account is written back; a box handed no identity keys writes its
// changed copy back whole, as before.
//
// Mutation: drop the identity check from the write-back, or drop the keys
// from the box's record, and the other account's copy is written.
func TestABoxWritesBackOnlyTheAccountItWasSeededWith(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "credentials")
	files := map[string]string{".codex/auth.json": filepath.Join(dir, "auth.json")}
	identity := map[string][]string{".codex/auth.json": {"tokens.account_id", "OPENAI_API_KEY"}}
	run := func(spec Spec, seeded, body string) string {
		t.Helper()
		putLogin(t, dir, map[string]string{"auth.json": seeded})
		box, err := newDirect(t).Create(t.Context(), spec)
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		writeFile(t, filepath.Join(box.Home(), ".codex", "auth.json"), body)
		if err := box.Close(t.Context()); err != nil {
			t.Fatalf("Close: %v", err)
		}
		return sharedNow(t, dir)["auth.json"]
	}
	const login = `{"OPENAI_API_KEY":null,"tokens":{"account_id":"acct-A","access_token":"a1"}}`
	spec := Spec{CredentialFiles: files, CredentialIdentityKeys: identity}
	other := `{"OPENAI_API_KEY":null,"tokens":{"account_id":"acct-B","access_token":"b1"}}`
	if got := run(spec, login, other); got != login {
		t.Errorf("a copy naming another account left the shared login %s, want it as it was", got)
	}
	refreshed := `{"OPENAI_API_KEY":null,"tokens":{"account_id":"acct-A","access_token":"a2"}}`
	if got := run(spec, login, refreshed); got != refreshed {
		t.Errorf("a refresh of the seeded account left the shared login %s, want the refresh", got)
	}
	if got := run(Spec{CredentialFiles: files}, login, other); got != other {
		t.Errorf("a box handed no identity keys wrote back %s, want its copy whole", got)
	}
}
