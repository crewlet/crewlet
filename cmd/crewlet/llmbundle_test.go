package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/providers/llm/cliagent"
)

// The cases here hold WHERE `crewlet llm` restores an entry's configured
// credential bundle. Every one sets the conventional variable the engine reads
// a bundle from, so none runs in parallel.

// bundledEntry writes a company whose one cli-agent entry, "sub", keeps its
// login in a credential file and runs `true` for every command, configures
// bundle through the conventional CREWLET_LLM_CLI_SUB_CREDENTIALS, and has an
// empty state directory. It returns the flags pointing a command at it and the
// path the entry's login is kept at.
func bundledEntry(t *testing.T, bundle string) (flags []string, login string) {
	t.Helper()
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	t.Setenv(cliagent.BundleVarName("sub"), bundle)
	company := writeFile(t, dir, "company.yaml", fmt.Sprintf(`name: Acme
providers:
  llm:
    sub:
      type: cli-agent
      model: m
      cli:
        agent: custom
        state_dir: %q
        overrides:
          binary: "true"
          complete_args: ["-p"]
          model_args: ["--model", "{model}"]
          output: text
          credential_paths: [".fake/creds.json"]
          status_args: ["status"]
          login_args: ["login"]
roles:
  - name: CEO
    handle: ceo
    llm: sub
`, state))
	return []string{"-company", company, "-config", filepath.Join(dir, "absent.yaml")},
		filepath.Join(state, "credentials", "creds.json")
}

// llmBundle packs one creds.json the way `crewlet llm export` does.
func llmBundle(t *testing.T, body string) string {
	t.Helper()
	var raw bytes.Buffer
	zw := gzip.NewWriter(&raw)
	tw := tar.NewWriter(zw)
	if err := tw.WriteHeader(&tar.Header{
		Name: "creds.json", Mode: 0o600, Size: int64(len(body)), Typeflag: tar.TypeReg,
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

// runSub runs `crewlet llm <sub> sub` with extra flags against the company.
func runSub(t *testing.T, flags []string, sub string, extra ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	args := append(append([]string{sub, "sub"}, extra...), flags...)
	err = runLLM(noEnviron, args, &out, &errOut)
	return out.String(), errOut.String(), err
}

// readLogin is the entry's login, "" where there is none.
func readLogin(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // the test's own state directory
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// A LOGOUT OUTLASTS EVERY COMMAND THAT RESTORES THE BUNDLE. `status`, `export`
// and `doctor` restore the configured bundle before they answer, so the first
// of them after a logout put back the very login the operator had just removed,
// and said nothing. Now each of them restores nothing and names the logout and
// the way back, and the logout itself says what keeps the bundle out and what
// retires it.
//
// Mutation: drop the logout's record, or the restore's check of it, and the
// login is back after the status here.
func TestALogoutOutlastsTheCommandsThatRestoreTheBundle(t *testing.T) {
	flags, login := bundledEntry(t, llmBundle(t, `{"token":"k-bundled"}`))
	if _, stderr, err := runSub(t, flags, "status"); err != nil {
		t.Fatalf("status: %v (%s)", err, stderr)
	}
	if got := readLogin(t, login); got != `{"token":"k-bundled"}` {
		t.Fatalf("the premise: status did not restore the bundle (%q)", got)
	}

	stdout, stderr, err := runSub(t, flags, "logout")
	if err != nil {
		t.Fatalf("logout: %v (%s)", err, stderr)
	}
	for _, want := range []string{"Logged sub out", cliagent.BundleVarName("sub"), "fresh volume",
		"`crewlet llm login sub`"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the logout does not say %q about the configured bundle:\n%s", want, stdout)
		}
	}

	_, stderr, err = runSub(t, flags, "status")
	if err != nil {
		t.Fatalf("status after the logout: %v (%s)", err, stderr)
	}
	if got := readLogin(t, login); got != "" {
		t.Fatalf("status after a logout restored the login it removed: %q", got)
	}
	for _, want := range []string{"crewlet llm logout", "`crewlet llm login sub`"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("status does not say why it restored nothing (%q):\n%s", want, stderr)
		}
	}
	if strings.Contains(stderr, "did not restore") {
		t.Errorf("status reports a logout as a bundle that failed to restore:\n%s", stderr)
	}

	if _, _, err := runSub(t, flags, "export"); err == nil ||
		!strings.Contains(err.Error(), "logged out") {
		t.Errorf("export after a logout = %v, want no login to export, naming the logout", err)
	}
	stdout, _, err = runSub(t, flags, "doctor", "-no-smoke")
	if err == nil {
		t.Error("doctor called a logged-out entry with no sign-in healthy")
	}
	if !strings.Contains(stdout, "removed by `crewlet llm logout`") || strings.Contains(stdout, "export the bundle again") {
		t.Errorf("doctor reports the logout as something else:\n%s", stdout)
	}
	if got := readLogin(t, login); got != "" {
		t.Errorf("a command after the logout restored the login it removed: %q", got)
	}
}

// THE COMMANDS THAT ANSWER AS THE ENGINE WOULD RESTORE THE BUNDLE FIRST, on an
// entry whose state directory is empty — a fresh container before its engine
// first boots. Each reports on the login the engine would run with, and the
// engine restores the configured bundle before it runs anything, so a command
// that looked without restoring reported a different entry: `list` said
// `none` where `doctor` reported credential files, against the docs' promise
// that the two cannot disagree.
//
// Mutation: drop the restore from any one of them, and its case is red.
func TestTheCommandsThatAnswerAsTheEngineRestoreTheBundleFirst(t *testing.T) {
	for _, c := range []struct {
		command string
		extra   []string
		// fails is whether the command reports a problem anyway: doctor
		// measures `true`, which is no CLI.
		fails bool
		say   string
	}{
		{command: "status"},
		{command: "export"},
		{command: "doctor", extra: []string{"-no-smoke"}, fails: true, say: "credential files in"},
		{command: "list", say: "credentials"},
	} {
		t.Run(c.command, func(t *testing.T) {
			flags, login := bundledEntry(t, llmBundle(t, `{"token":"k-bundled"}`))
			var out, errOut bytes.Buffer
			args := []string{c.command}
			if c.command != "list" {
				args = append(args, "sub")
			}
			args = append(append(args, c.extra...), flags...)
			err := runLLM(noEnviron, args, &out, &errOut)
			if (err != nil) != c.fails {
				t.Fatalf("%s: %v\n%s%s", c.command, err, out.String(), errOut.String())
			}
			if got := readLogin(t, login); got != `{"token":"k-bundled"}` {
				t.Errorf("%s left %q, want the configured bundle restored before it answered", c.command, got)
			}
			if c.say != "" && !strings.Contains(out.String(), c.say) {
				t.Errorf("%s does not report the restored login (%q):\n%s", c.command, c.say, out.String())
			}
		})
	}
}

// THE COMMANDS THAT REPLACE OR REMOVE THE LOGIN DO NOT RESTORE IT FIRST. A
// login restored before `login` ran would be seeded into the sign-in and
// carried back as the operator's own, so a sign-in the operator backed out of
// would report a login written; one restored before `import` would refuse the
// operator's bundle as a login already present. A logout leaves no login
// whatever it began from.
//
// Mutation: restore the bundle before `login` or `import`, and its case is red.
func TestTheCommandsThatReplaceTheLoginDoNotRestoreItFirst(t *testing.T) {
	t.Run("login", func(t *testing.T) {
		flags, login := bundledEntry(t, llmBundle(t, `{"token":"k-bundled"}`))
		_, _, err := runSub(t, flags, "login")
		if err == nil || !strings.Contains(err.Error(), "without leaving a login") {
			t.Errorf("a login whose CLI stored nothing = %v, want it refused as leaving no login", err)
		}
		if got := readLogin(t, login); got != "" {
			t.Errorf("login restored the configured bundle: %q", got)
		}
	})
	t.Run("import", func(t *testing.T) {
		flags, login := bundledEntry(t, llmBundle(t, `{"token":"k-bundled"}`))
		stdin := filepath.Join(t.TempDir(), "bundle")
		if err := os.WriteFile(stdin, []byte(llmBundle(t, `{"token":"k-imported"}`)), 0o600); err != nil {
			t.Fatal(err)
		}
		f, err := os.Open(stdin) //nolint:gosec // the test's own file
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = f.Close() }()
		was := os.Stdin
		os.Stdin = f
		defer func() { os.Stdin = was }()
		if _, stderr, err := runSub(t, flags, "import"); err != nil {
			t.Fatalf("import: %v (%s)", err, stderr)
		}
		if got := readLogin(t, login); got != `{"token":"k-imported"}` {
			t.Errorf("import left %q, want the operator's bundle rather than the configured one", got)
		}
	})
	t.Run("logout", func(t *testing.T) {
		flags, login := bundledEntry(t, llmBundle(t, `{"token":"k-bundled"}`))
		if _, stderr, err := runSub(t, flags, "logout"); err != nil {
			t.Fatalf("logout: %v (%s)", err, stderr)
		}
		if got := readLogin(t, login); got != "" {
			t.Errorf("logout left a login: %q", got)
		}
	})
}
