//go:build unix

package sandbox

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
)

// A BOX WRITES BACK NO KEY ITS RUN MINTED: a login file whose run keys
// ([Spec.CredentialRunKeys]) the harness rewrote during the run, as Muse Code
// writes a key it minted into auth.json, is written back with the shared
// login's value of each: nothing at all where the run changed nothing else,
// the run's refresh beside the shared key where it did. A box handed no run
// keys writes the file back whole.
//
// Mutation: write the box's copy back as it stands, or drop the keys from
// the box's record.
func TestABoxWritesBackNoKeyItsRunMinted(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "credentials")
	files := map[string]string{".fake/auth.json": filepath.Join(dir, "auth.json")}
	keys := map[string][]string{".fake/auth.json": {"api_key"}}
	login := `{"token":"t1","api_key":"LOGIN"}`
	putLogin(t, dir, map[string]string{"auth.json": login})
	run := func(spec Spec, body string) string {
		t.Helper()
		box, err := newDirect(t).Create(t.Context(), spec)
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		writeFile(t, filepath.Join(box.Home(), ".fake", "auth.json"), body)
		if err := box.Close(t.Context()); err != nil {
			t.Fatalf("Close: %v", err)
		}
		return sharedNow(t, dir)["auth.json"]
	}
	same := func(a, b string) bool {
		var x, y any
		return json.Unmarshal([]byte(a), &x) == nil && json.Unmarshal([]byte(b), &y) == nil && reflect.DeepEqual(x, y)
	}
	spec := Spec{CredentialFiles: files, CredentialRunKeys: keys}
	if got := run(spec, `{"token":"t1","api_key":"MINTED"}`); got != login {
		t.Errorf("a run that minted a key alone left the shared login %s, want it as it was", got)
	}
	if got := run(spec, `{"token":"t2","api_key":"MINTED"}`); !same(got, `{"token":"t2","api_key":"LOGIN"}`) {
		t.Errorf("a run that refreshed beside its minted key left the shared login %s, want its "+
			"refresh with the shared key", got)
	}
	if got := run(Spec{CredentialFiles: files}, `{"token":"t3","api_key":"OTHER"}`); got != `{"token":"t3","api_key":"OTHER"}` {
		t.Errorf("a box handed no run keys wrote back %s, want its copy whole", got)
	}
}
