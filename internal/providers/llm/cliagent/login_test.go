package cliagent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// loginProvider builds a provider over a temp state dir, with a profile whose
// credential files are the ones these tests write.
func loginProvider(t *testing.T) *Provider {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() { forgetWorkspace(dir) })
	p, err := New(Config{
		Key: "sub", Agent: "custom", StateDir: dir,
		Timeout: time.Second, MaxConcurrent: 1,
		Overrides: map[string]any{
			"binary": os.Args[0], "complete_args": []any{"-test.run=TestCLIAgentFakeCLI"},
			"model_args": []any{}, "output": "text",
			"credential_paths":      []any{".fake/creds.json"},
			"host_credential_paths": []any{".fake/creds.json"},
			"volatile_paths":        []any{".fake/sessions"},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// writeHostLogin puts a login in a fake human home directory.
func writeHostLogin(t *testing.T, home, body string) {
	t.Helper()
	path := filepath.Join(home, ".fake", "creds.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The usual starting point: the operator has been running the CLI on this box
// for months. Adopting is a COPY, so their own login is never written to by a
// fleet of agents refreshing tokens.
func TestAdoptingAHostLoginCopiesRatherThanMoves(t *testing.T) {
	p := loginProvider(t)
	home := t.TempDir()
	writeHostLogin(t, home, `{"token":"personal"}`)

	taken, err := p.AdoptHostLogin(home)
	if err != nil {
		t.Fatalf("AdoptHostLogin: %v", err)
	}
	if len(taken) != 1 {
		t.Fatalf("adopted %v", taken)
	}
	if _, err := os.Stat(filepath.Join(home, ".fake", "creds.json")); err != nil {
		t.Errorf("the operator's own login was moved, not copied: %v", err)
	}
	if !p.Workspace().HasLogin() {
		t.Error("the adopted login is not where seats read it from")
	}
}

// "No login found" must name the directory it looked in, or an operator
// running as a different user than the one who logged in has nothing to go on.
func TestAdoptingWithNoHostLoginSaysWhereItLooked(t *testing.T) {
	p := loginProvider(t)
	home := t.TempDir()
	_, err := p.AdoptHostLogin(home)
	if err == nil {
		t.Fatal("adopting a login that does not exist succeeded")
	}
	if !strings.Contains(err.Error(), home) {
		t.Errorf("error %q does not name the home it searched", err)
	}
	if !strings.Contains(err.Error(), "--home") {
		t.Errorf("error %q does not offer the flag for a different home", err)
	}
}

// A bundle carries a login onto another host, and must come back byte for
// byte — a credential that round-trips almost correctly is a login that fails
// at the vendor with no explanation.
func TestACredentialBundleRoundTrips(t *testing.T) {
	source := loginProvider(t)
	home := t.TempDir()
	writeHostLogin(t, home, `{"token":"exported","refresh":"r1"}`)
	if _, err := source.AdoptHostLogin(home); err != nil {
		t.Fatalf("AdoptHostLogin: %v", err)
	}

	blob, err := source.ExportBundle()
	if err != nil {
		t.Fatalf("ExportBundle: %v", err)
	}

	target := loginProvider(t)
	if err := target.RestoreBundle(blob); err != nil {
		t.Fatalf("RestoreBundle: %v", err)
	}
	restored, err := os.ReadFile(filepath.Join(target.Workspace().CredentialsDir(), "creds.json"))
	if err != nil {
		t.Fatalf("the bundle restored nothing: %v", err)
	}
	if string(restored) != `{"token":"exported","refresh":"r1"}` {
		t.Errorf("restored %q", restored)
	}
}

// An archive is an execution surface if it is unpacked on trust, and a bundle
// arrives from the secret store, which every node holding the keyring shares.
func TestARestoredBundleCannotEscapeTheCredentialDirectory(t *testing.T) {
	p := loginProvider(t)
	for _, name := range []string{
		"../../.ssh/authorized_keys",
		"/etc/passwd",
		"nested/creds.json",
		"unexpected.json",
	} {
		err := p.RestoreBundle(bundleOf(t, name, "pwned"))
		if err == nil {
			t.Errorf("RestoreBundle accepted %q", name)
			continue
		}
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error for %q does not name it: %v", name, err)
		}
	}
	if p.Workspace().HasLogin() {
		t.Error("a refused bundle still wrote a credential")
	}
}

// AN OVER-LARGE BUNDLE IS REFUSED, not clipped. io.LimitReader stops at its cap
// and hands the tar reader a clean io.EOF, which is indistinguishable from the
// end of a whole archive — so a bundle past the cap used to restore whatever
// files fitted and report success, leaving a partial credential directory that
// HasLogin then reads as a login and declines to repair.
func TestAnOversizedBundleIsRefusedRatherThanPartlyRestored(t *testing.T) {
	p := loginProvider(t)
	err := p.RestoreBundle(bundleOf(t, "creds.json", strings.Repeat("x", maxBundle+1)))
	if err == nil {
		t.Fatal("an oversized bundle was accepted")
	}
	if !strings.Contains(err.Error(), "larger than") {
		t.Errorf("the refusal does not say why: %v", err)
	}
	if p.Workspace().HasLogin() {
		t.Error("a refused bundle still wrote a credential")
	}
}

// A bundle is validated WHOLE before anything reaches the disk: the checks
// reject mid-archive, and writing as the loop went left the rejected bundle's
// earlier entries behind.
func TestARejectedBundleLeavesNothingBehind(t *testing.T) {
	p := loginProvider(t)
	// A good entry FIRST, then one the name check refuses.
	var raw bytes.Buffer
	zw := gzip.NewWriter(&raw)
	tw := tar.NewWriter(zw)
	for _, e := range []struct{ name, body string }{
		{"creds.json", `{"token":"real"}`},
		{"../../.ssh/authorized_keys", "pwned"},
	} {
		if err := tw.WriteHeader(&tar.Header{
			Name: e.name, Mode: 0o600, Size: int64(len(e.body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	if err := p.RestoreBundle(base64.StdEncoding.EncodeToString(raw.Bytes())); err == nil {
		t.Fatal("a bundle holding a path traversal was accepted")
	}
	if p.Workspace().HasLogin() {
		t.Error("the entry before the refused one was written anyway")
	}
}

// A node that has been running holds the FRESHER refresh token; restoring a
// boot-time blob over it is how a fleet logs itself out at the next expiry.
func TestABundleDoesNotOverwriteALiveLogin(t *testing.T) {
	p := loginProvider(t)
	home := t.TempDir()
	writeHostLogin(t, home, `{"token":"current"}`)
	if _, err := p.AdoptHostLogin(home); err != nil {
		t.Fatalf("AdoptHostLogin: %v", err)
	}
	if err := p.RestoreBundle(bundleOf(t, "creds.json", `{"token":"stale"}`)); err != nil {
		t.Fatalf("RestoreBundle: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(p.Workspace().CredentialsDir(), "creds.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"token":"current"}` {
		t.Errorf("a boot-time bundle overwrote the running node's login: %q", got)
	}
}

// Exporting nothing must say what to run, not produce an empty archive that
// looks like a working credential in the secret store.
func TestExportingWithNoLoginRefuses(t *testing.T) {
	p := loginProvider(t)
	_, err := p.ExportBundle()
	if err == nil {
		t.Fatal("an empty credential directory exported cleanly")
	}
	if !strings.Contains(err.Error(), "crewlet llm login") {
		t.Errorf("error %q does not say what to run", err)
	}
}

// Logout removes the local credential even when the vendor's own command
// fails: a login the operator believes they removed must not still work.
func TestLogoutRemovesTheCredentialEvenIfTheVendorCommandFails(t *testing.T) {
	p := loginProvider(t)
	home := t.TempDir()
	writeHostLogin(t, home, `{"token":"x"}`)
	if _, err := p.AdoptHostLogin(home); err != nil {
		t.Fatalf("AdoptHostLogin: %v", err)
	}
	// A logout command that cannot possibly succeed.
	p.profile.LogoutArgs = []string{"-test.run=TestCLIAgentFakeCLI"}
	p.env = map[string]string{helperEnv: "1", "FAKE_EXIT": "3"}

	err := p.Logout(t.Context(), &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil {
		t.Error("a failing vendor logout was reported as success")
	}
	if p.Workspace().HasLogin() {
		t.Error("the credential survived a logout the operator was told about")
	}
}

// The three CLIs whose login is browser OAuth must say so in a sentence an
// operator can act on, rather than hanging on a prompt or failing obscurely.
func TestACLIWithNoCredentialLoginExplainsItself(t *testing.T) {
	p := loginProvider(t)
	err := p.CredentialLogin(t.Context(), "ops@example.com", "hunter2",
		&bytes.Buffer{}, &bytes.Buffer{})
	if !errors.Is(err, ErrNoLoginCommand) {
		t.Fatalf("err = %v, want ErrNoLoginCommand", err)
	}
	for _, want := range []string{"crewlet llm login", "stdin_login", "browser"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the message does not mention %q: %v", want, err)
		}
	}
}

// The built-in opencode profile is the one with a credential login, and the
// three browser-OAuth profiles deliberately leave it unset. A profile that
// gained one by accident would send operators down a path their CLI cannot
// serve.
func TestOnlyOpencodeShipsACredentialLogin(t *testing.T) {
	t.Parallel()
	for _, name := range BuiltinNames() {
		if name == "custom" {
			continue
		}
		p, err := Load(name, nil)
		if err != nil {
			t.Fatalf("Load(%q): %v", name, err)
		}
		hasLogin := p.StdinLogin != nil
		if want := name == "opencode"; hasLogin != want {
			t.Errorf("%s: stdin_login present = %v, want %v", name, hasLogin, want)
		}
	}
}

// bundleOf builds a one-file bundle with an arbitrary entry name.
func bundleOf(t *testing.T, name, body string) string {
	t.Helper()
	var raw bytes.Buffer
	zw := gzip.NewWriter(&raw)
	tw := tar.NewWriter(zw)
	if err := tw.WriteHeader(&tar.Header{
		Name: name, Mode: 0o600, Size: int64(len(body)), Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(raw.Bytes())
}
