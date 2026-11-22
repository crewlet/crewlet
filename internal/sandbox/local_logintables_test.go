//go:build unix

package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A HOOK A RUN APPENDS TO A LOGIN'S CONFIG REACHES NO LATER RUN AND NO SHARED
// LOGIN. Kimi Code keeps its login in some tables of config.toml and reads
// hooks, which it runs, from the same file ([Spec.CredentialTables]): a local
// box is handed only the login's tables, again before every run, and never
// writes the file back, so a [[hooks]] table a run appended is gone at the next
// run and never reaches the shared login, which every seat on the engine host
// is seeded from; the run's refreshed token in the other file is still
// written back.
//
// Mutation: write the box's config.toml back, hand it whole, or skip the
// reseed, and a hook survives.
func TestAHookARunAppendsReachesNoLaterRunAndNoSharedLogin(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "credentials")
	config := "default_model = \"managed:kimi-code/k\"\n\n[providers.\"managed:kimi-code\"]\ntype = \"kimi\"\n\n" +
		"[tools]\ndisabled = [\"Bash\"]\n"
	putLogin(t, dir, map[string]string{"config.toml": config, "kimi-code.json": `{"token":"t1"}`})
	files := map[string]string{
		".kimi-code/config.toml":                filepath.Join(dir, "config.toml"),
		".kimi-code/credentials/kimi-code.json": filepath.Join(dir, "kimi-code.json"),
	}
	tables := map[string][]string{".kimi-code/config.toml": {"default_model", "providers"}}
	box, err := newDirect(t).Create(t.Context(), Spec{CredentialFiles: files, CredentialTables: tables})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	inBox := filepath.Join(box.Home(), ".kimi-code", "config.toml")
	read := func() string {
		t.Helper()
		raw, err := os.ReadFile(inBox) //nolint:gosec // the case's own box
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	if got := read(); strings.Contains(got, "tools") || !strings.Contains(got, "managed:kimi-code") {
		t.Fatalf("the box was handed %q, want the login's tables alone", got)
	}

	hook := "\n[[hooks]]\nevent = \"SessionStart\"\ncommand = \"touch /tmp/planted\"\n"
	writeFile(t, inBox, read()+hook)
	reseeder, ok := box.(LoginReseeder)
	if !ok {
		t.Fatal("a local box hands its login again to no run")
	}
	if err := reseeder.ReseedLogin(t.Context()); err != nil {
		t.Fatalf("ReseedLogin: %v", err)
	}
	if got := read(); strings.Contains(got, "hooks") || !strings.Contains(got, "managed:kimi-code") {
		t.Errorf("the next run is handed %q, want the login's tables alone", got)
	}

	writeFile(t, inBox, read()+hook)
	writeFile(t, filepath.Join(box.Home(), ".kimi-code", "credentials", "kimi-code.json"), `{"token":"t2"}`)
	if err := box.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	shared := sharedNow(t, dir)
	if shared["config.toml"] != config {
		t.Errorf("the shared login's config.toml is now %q, want it as it was", shared["config.toml"])
	}
	if shared["kimi-code.json"] != `{"token":"t2"}` {
		t.Errorf("the run's refresh was not written back: %q", shared["kimi-code.json"])
	}
}
