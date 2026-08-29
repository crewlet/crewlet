package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm/cliagent"
)

// bundleProvider is a cli-agent provider with a credential directory of its
// own and nothing else — import and export never run the vendor's binary, so
// none has to exist.
func bundleProvider(t *testing.T) []cliAgentProvider {
	t.Helper()
	p, err := cliagent.New(cliagent.Config{
		Key: "sub", Agent: "custom", StateDir: t.TempDir(),
		Timeout: time.Second, MaxConcurrent: 1,
		Overrides: map[string]any{
			"binary": "true", "complete_args": []any{"-p"}, "model_args": []any{},
			"output":                "text",
			"credential_paths":      []any{".fake/creds.json"},
			"host_credential_paths": []any{".fake/creds.json"},
		},
	})
	if err != nil {
		t.Fatalf("cliagent.New: %v", err)
	}
	return []cliAgentProvider{{key: "sub", provider: p}}
}

// EXPORT WITHOUT IMPORT IS A DEAD END: the documented way to move a login
// onto another host ended at a blob in a terminal, because nothing read it
// back.
func TestALoginBundleRoundTripsThroughTheCLI(t *testing.T) {
	t.Parallel()
	source := bundleProvider(t)
	creds := filepath.Join(source[0].provider.Workspace().CredentialsDir(), "creds.json")
	if err := os.MkdirAll(filepath.Dir(creds), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(creds, []byte(`{"token":"moved"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	blob, err := source[0].provider.ExportBundle()
	if err != nil {
		t.Fatalf("ExportBundle: %v", err)
	}

	target := bundleProvider(t)
	var out bytes.Buffer
	if err := importLLM(target, "sub", strings.NewReader(blob+"\n"), &out); err != nil {
		t.Fatalf("importLLM: %v", err)
	}
	restored, err := os.ReadFile(
		filepath.Join(target[0].provider.Workspace().CredentialsDir(), "creds.json"))
	if err != nil {
		t.Fatalf("the bundle restored nothing: %v", err)
	}
	if string(restored) != `{"token":"moved"}` {
		t.Errorf("restored %q", restored)
	}
	if !strings.Contains(out.String(), "doctor sub") {
		t.Errorf("the run does not say how to verify it: %s", out.String())
	}
}

// AN EXISTING LOGIN IS NOT OVERWRITTEN, and the refusal is REPORTED rather
// than swallowed: a host that has been running holds the fresher refresh
// token, and "nothing happened" and "restored" look identical from outside.
func TestImportingOverAnExistingLoginIsRefusedLoudly(t *testing.T) {
	t.Parallel()
	target := bundleProvider(t)
	creds := filepath.Join(target[0].provider.Workspace().CredentialsDir(), "creds.json")
	if err := os.MkdirAll(filepath.Dir(creds), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(creds, []byte(`{"token":"already here"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := importLLM(target, "sub", strings.NewReader("anything"), &out)
	if err == nil {
		t.Fatal("a bundle was restored over a live login")
	}
	if !strings.Contains(err.Error(), "logout sub") {
		t.Errorf("the error does not name the way out: %v", err)
	}
	// AND THE LIVE CREDENTIAL IS UNTOUCHED.
	got, readErr := os.ReadFile(creds)
	if readErr != nil || string(got) != `{"token":"already here"}` {
		t.Errorf("the existing login was disturbed: %q %v", got, readErr)
	}
}

// AN EMPTY STDIN NAMES THE PIPE, because the natural mistake is running the
// command with nothing feeding it.
func TestImportingNothingSaysHowToPipeABundle(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	err := importLLM(bundleProvider(t), "sub", strings.NewReader("   \n"), &out)
	if err == nil {
		t.Fatal("an empty stdin imported")
	}
	if !strings.Contains(err.Error(), "llm export sub |") {
		t.Errorf("the error does not show the pipe: %v", err)
	}
}

// -print-token WRITES A CREDENTIAL TO STDOUT and must refuse a terminal: a
// token in a scrollback outlives the command, and a screen-share or a shell
// history outlives the scrollback.
func TestATerminalIsRecognisedSoACredentialIsNotPrintedIntoOne(t *testing.T) {
	t.Parallel()
	// /dev/null is a character device — the same class as a tty — so it
	// exercises the branch without needing one.
	dev, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Skipf("no %s on this platform: %v", os.DevNull, err)
	}
	t.Cleanup(func() { _ = dev.Close() })
	if !isTerminal(dev) {
		t.Error("a character device was not recognised as a terminal")
	}

	// A PIPE AND A BUFFER ARE NOT, or the flag would refuse the one use it
	// exists for.
	if isTerminal(&bytes.Buffer{}) {
		t.Error("a buffer was treated as a terminal")
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(); _ = w.Close() })
	if isTerminal(w) {
		t.Error("a pipe was treated as a terminal")
	}
	file := filepath.Join(t.TempDir(), "token")
	f, err := os.Create(file)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	if isTerminal(f) {
		t.Error("a regular file was treated as a terminal")
	}
}

// QUIET IS A DEFAULT, NOT A CEILING. A half-applied migration is exactly the
// run whose detail an operator needs.
func TestOperatorCommandsTakeTheirLevelFromTheEnvironment(t *testing.T) {
	for _, tc := range []struct {
		set  string
		want string
	}{
		{"", "WARN"},
		{"debug", "DEBUG"},
		{"error", "ERROR"},
		// A TYPO RESOLVES TO THE DEFAULT rather than failing: a bad log
		// level must never be why an operator cannot run a migration.
		{"shout", "WARN"},
	} {
		t.Run("CREWLET_LOG_LEVEL="+tc.set, func(t *testing.T) {
			t.Setenv("CREWLET_LOG_LEVEL", tc.set)
			if got := operatorLogLevel().String(); got != tc.want {
				t.Errorf("level = %s, want %s", got, tc.want)
			}
		})
	}
}
