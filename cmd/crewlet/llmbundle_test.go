package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm/cliagent"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent/cliprofile"
)

// The cases here hold WHERE `crewlet llm` restores an entry's configured
// credential bundle. Every one sets the conventional variable the engine reads
// a bundle from, so none runs in parallel.

// bundledEntry writes a company whose one entry, "sub", keeps its
// login in a credential file and runs `true` for every command, configures
// bundle through the conventional CREWLET_LLM_CLI_SUB_CREDENTIALS, and has an
// empty state directory. It returns the flags pointing a command at it and the
// path the entry's login is kept at.
func bundledEntry(t *testing.T, bundle string) (flags []string, login string) {
	t.Helper()
	return bundledEntryWithLogout(t, bundle, "")
}

// bundledEntryWithLogout is [bundledEntry] whose CLI is `sh`, whose status
// and login succeed and whose own logout runs `sh -c logout` in the login
// home, where a seeded login sits at .fake/creds.json — "" declares no vendor
// logout and keeps `true` for every command.
func bundledEntryWithLogout(t *testing.T, bundle, logout string) (flags []string, login string) {
	t.Helper()
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	t.Setenv(cliprofile.BundleVarName("sub"), bundle)
	binary, status, signIn, logoutArgs := "true", `["status"]`, `["login"]`, ""
	if logout != "" {
		binary, status, signIn = "sh", `["-c", "exit 0"]`, `["-c", "exit 0"]`
		logoutArgs = fmt.Sprintf("\n        logout_args: [\"-c\", %q]", logout)
	}
	company := writeFile(t, dir, "company.yaml", fmt.Sprintf(`name: Acme
providers:
  sandbox: {fake: true}
  llm:
    sub:
      agent: custom
      model: m
      state_dir: %q
      overrides:
        binary: %q
        complete_args: ["-p"]
        model_args: ["--model", "{model}"]
        output: text
        credential_paths: [".fake/creds.json"]
        status_args: %s
        login_args: %s%s
roles:
  # A person's seat: the custom entry stands in for a CLI this suite
  # cannot install, and it can run no seat's executor.
  - name: CEO
    handle: ceo
    kind: human
`, state, binary, status, signIn, logoutArgs))
	return []string{"-company", company, "-config", filepath.Join(dir, "absent.yaml")},
		filepath.Join(state, "custom", "credentials", "creds.json")
}

// llmBundle is a bundle of the entry's CLI, `custom`, holding one creds.json:
// exported by `crewlet llm export`'s own writer from a state directory of its
// own, so it is a bundle the export writes — the CLI it names included —
// rather than this file's copy of one.
func llmBundle(t *testing.T, body string) string {
	t.Helper()
	p, err := cliagent.New(cliagent.Config{
		Key: "sub", Agent: "custom", StateDir: t.TempDir(), Timeout: time.Second, MaxConcurrent: 1,
		Overrides: map[string]any{
			"binary": "true", "complete_args": []any{"-p"}, "output": "text",
			"credential_paths": []any{".fake/creds.json"},
		},
	})
	if err != nil {
		t.Fatalf("building the exporting entry: %v", err)
	}
	login := filepath.Join(p.Workspace().CredentialsDir(), "creds.json")
	if err := os.MkdirAll(filepath.Dir(login), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(login, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	blob, err := p.ExportBundle(t.Context())
	if err != nil {
		t.Fatalf("ExportBundle: %v", err)
	}
	return blob
}

// codexBundle is a bundle `crewlet llm export` writes from a codex login, from
// a state directory of its own: another CLI's, to the `custom` entry above.
func codexBundle(t *testing.T, body string) string {
	t.Helper()
	p, err := cliagent.New(cliagent.Config{
		Key: "sub", Agent: "codex", StateDir: t.TempDir(), Timeout: time.Second, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatalf("building the exporting entry: %v", err)
	}
	login := filepath.Join(p.Workspace().CredentialsDir(), "auth.json")
	if err := os.MkdirAll(filepath.Dir(login), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(login, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	blob, err := p.ExportBundle(t.Context())
	if err != nil {
		t.Fatalf("ExportBundle: %v", err)
	}
	return blob
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
	for _, want := range []string{"Logged sub out", cliprofile.BundleVarName("sub"), "fresh volume",
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

// A BUNDLE FROM ANOTHER CLI IS REFUSED BY EVERY COMMAND THAT RESTORES IT, as
// the engine refuses it: an entry moved to another CLI keeps the bundle it was
// configured with, which names the CLI it was exported from. `status` says so
// on stderr and restores nothing, and `doctor` reports it with the export that
// replaces it — named with the entry's key, where it named a bare `crewlet llm
// export`, which a company of more than one entry refuses for want of one.
//
// Mutation: drop the key from doctor's remedy, or the manifest's check from the
// restore, and this is red.
func TestABundleFromAnotherCLIIsRefusedByTheCommandsThatRestoreIt(t *testing.T) {
	flags, login := bundledEntry(t, codexBundle(t, `{"token":"k-codex"}`))

	stdout, _, err := runSub(t, flags, "doctor", "-no-smoke")
	if err == nil {
		t.Error("doctor called an entry whose bundle is another CLI's healthy")
	}
	for _, want := range []string{`it names "codex"`, "export the bundle again (`crewlet llm export sub`)"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("doctor does not say %q:\n%s", want, stdout)
		}
	}

	_, stderr, err := runSub(t, flags, "status")
	if err != nil {
		t.Fatalf("status: %v (%s)", err, stderr)
	}
	for _, want := range []string{"did not restore", `it names "codex"`, `"sub" moved to "custom"`} {
		if !strings.Contains(stderr, want) {
			t.Errorf("status does not say %q:\n%s", want, stderr)
		}
	}
	if got := readLogin(t, login); got != "" {
		t.Errorf("another CLI's bundle was restored as the entry's login: %q", got)
	}
}

// THE COMMANDS THAT REPLACE OR REMOVE THE LOGIN DO NOT RESTORE IT FIRST. A
// login restored before `login` ran would be seeded into the sign-in and
// carried back as the operator's own, so a sign-in the operator backed out of
// would report a login written; one restored before `import` would refuse the
// operator's bundle as a login already present; and one restored before
// `logout` would be seeded into the login home the vendor's own logout runs
// in, handing it the configured bundle's login to REVOKE at the vendor — the
// login every fresh container signs in with — although the logout leaves no
// login on disk either way. So that case's vendor logout fails exactly when a
// login was seeded for it.
//
// Mutation: restore the bundle before `login`, `import` or `logout`, and its
// case is red.
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
		flags, login := bundledEntryWithLogout(t, llmBundle(t, `{"token":"k-bundled"}`),
			"test ! -e .fake/creds.json")
		if _, stderr, err := runSub(t, flags, "logout"); err != nil {
			t.Fatalf("logout handed the vendor's own logout a login to revoke, which only a restore "+
				"before it could have put there: %v (%s)", err, stderr)
		}
		if got := readLogin(t, login); got != "" {
			t.Errorf("logout left a login: %q", got)
		}
	})
}

// A LOGOUT WHOSE VENDOR HALF FAILED STILL SAYS WHAT HOLDS THE BUNDLE BACK. The
// login is removed and the logout recorded whatever the vendor's own logout
// does, so the bundle stays out — and the command, failing over the vendor's
// half, returned before it said so: the operator revoking a compromised login
// never heard that the bundle is now withheld here, nor what retires it for a
// fresh volume. The notice follows the record, not the command's success.
//
// Mutation: print the notice only when the logout succeeded, and this is red.
func TestALogoutWhoseVendorHalfFailedStillNamesTheBundle(t *testing.T) {
	flags, login := bundledEntryWithLogout(t, llmBundle(t, `{"token":"k-bundled"}`), "exit 3")
	if _, stderr, err := runSub(t, flags, "status"); err != nil {
		t.Fatalf("status: %v (%s)", err, stderr)
	}
	if got := readLogin(t, login); got == "" {
		t.Fatal("the premise: status did not restore the bundle")
	}

	stdout, stderr, err := runSub(t, flags, "logout")
	if err == nil || !strings.Contains(err.Error(), "did not finish") {
		t.Fatalf("logout = %v, want the vendor's own logout reported unfinished (%s)", err, stderr)
	}
	if got := readLogin(t, login); got != "" {
		t.Fatalf("the premise: a failed vendor logout left the login: %q", got)
	}
	if strings.Contains(stdout, "Logged sub out") {
		t.Errorf("a logout whose vendor half failed reported success:\n%s", stdout)
	}
	for _, want := range []string{cliprofile.BundleVarName("sub"), "fresh volume", "`crewlet llm login sub`"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the failed logout does not say %q about the bundle it holds back:\n%s", want, stdout)
		}
	}
	// And it does hold it back.
	if _, _, err := runSub(t, flags, "status"); err != nil {
		t.Fatalf("status after the logout: %v", err)
	}
	if got := readLogin(t, login); got != "" {
		t.Errorf("status after a logout whose vendor half failed restored the login: %q", got)
	}
}
