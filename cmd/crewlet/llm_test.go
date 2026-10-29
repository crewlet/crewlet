package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent"
	"github.com/crewlet/crewlet/internal/secrets"
)

// bundleProvider is a provider with a credential directory of its own and
// nothing else — import and export never run the vendor's binary, so
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
	blob, err := source[0].provider.ExportBundle(t.Context())
	if err != nil {
		t.Fatalf("ExportBundle: %v", err)
	}

	target := bundleProvider(t)
	var out bytes.Buffer
	if err := importLLM(t.Context(), target, "sub", defaultCompanyPath, nil,
		strings.NewReader(blob+"\n"), &out); err != nil {
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
	if !strings.Contains(out.String(), "Verify it with `crewlet llm doctor sub`.") {
		t.Errorf("the run does not say how to verify it, with no flag it was not given: %s",
			out.String())
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
	err := importLLM(t.Context(), target, "sub", defaultCompanyPath, nil, strings.NewReader("anything"), &out)
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
	err := importLLM(t.Context(), bundleProvider(t), "sub", defaultCompanyPath, nil,
		strings.NewReader("   \n"), &out)
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

// --- doctor over every entry --------------------------------------------------

// doctorCompany writes a company whose providers are the given block, and
// returns the flags pointing doctor at it. The bootstrap path does not exist,
// so references resolve from the environment alone. Its one seat is a
// person's: the entries are `custom` stand-ins for a CLI this suite cannot
// install, which can run no seat's executor.
func doctorCompany(t *testing.T, providers string) []string {
	t.Helper()
	dir := t.TempDir()
	company := writeFile(t, dir, "company.yaml", "name: Acme\nproviders:\n  sandbox: {fake: true}\n  llm:\n"+providers+
		"roles:\n  - name: CEO\n    handle: ceo\n    kind: human\n")
	return []string{"-company", company, "-config", filepath.Join(dir, "absent.yaml")}
}

// healthyDoctorEntry is a providers.llm entry, under key, that doctor reports healthy
// without a billed call: a CLI that is on PATH and prints a version, reads its
// usage figures, and is handed a headless token — its model the given one,
// which may be a ${VAR}.
//
// `custom` over /bin/sh, because doctor runs the binary and no vendor's CLI is
// installed where this suite runs; the token is the one sign-in that needs no
// file and no login, and the token variable is the profile's own, so it is
// the CLI's sign-in rather than an env entry doctor would have to judge.
func healthyDoctorEntry(t *testing.T, key, model string) string {
	t.Helper()
	return fmt.Sprintf(`    %s:
      agent: custom
      model: %s
      state_dir: %q
      auth: {token: fake-headless-token}
      overrides:
        binary: /bin/sh
        version_args: ["-c", "echo fake-cli 1.0"]
        complete_args: ["-c", "exit 1"]
        model_args: ["--model", "{model}"]
        output: json
        text_paths: [["result"]]
        token_env: FAKE_CLI_TOKEN
        usage: {input: [["usage", "input_tokens"]], output: [["usage", "output_tokens"]]}
`, key, model, t.TempDir())
}

// AN ENTRY THAT DOES NOT BUILD IS REPORTED BESIDE THE OTHERS, not instead of
// them: the engine would refuse the document over it, and an operator wants
// that and every other entry's state from one run. And A PROBLEM IS A NON-ZERO
// EXIT, because doctor is what a deploy script gates on.
func TestDoctorReportsAnEntryThatDoesNotBuildBesideTheOthers(t *testing.T) {
	t.Parallel()
	args := append([]string{"doctor", "-no-smoke"}, doctorCompany(t,
		healthyDoctorEntry(t, "claude", "m")+
			healthyDoctorEntry(t, "broken", `"${CREWLET_DOCTOR_TEST_UNSET_MODEL}"`))...)
	var out, notes bytes.Buffer
	err := runLLM(noEnviron, args, &out, &notes)
	if err == nil || !strings.Contains(err.Error(), "1 of 2 providers have problems") {
		t.Fatalf("err = %v\n%s", err, out.String())
	}
	for _, want := range []string{
		"provider      : claude\n",
		"version       : fake-cli 1.0\n",
		"problems      : none\n",
		"provider      : broken\nproblems:\n  - it does not build",
		"CREWLET_DOCTOR_TEST_UNSET_MODEL resolved to nothing",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, out.String())
		}
	}
}

// THE DOCTOR NAMES THE SEATS THAT LAUNCH ON AN ENTRY, as the company resolves
// them: the seats whose executor chain holds it — and for a member whose CLI
// no box can run, that a launch passes over it. An entry no seat's executor
// chain holds says nothing of runners or a bridge it never needs.
//
// Mutation: hand Diagnose no seats and none of the launch lines is written.
func TestDoctorNamesTheSeatsThatLaunchOnAnEntry(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	coder := fmt.Sprintf("    coder:\n      agent: claude-code\n      model: sonnet\n      state_dir: %q\n"+
		"      overrides: {binary: %q}\n", t.TempDir(), filepath.Join(dir, "no-cli-here"))
	company := writeFile(t, dir, "company.yaml", "name: Acme\nproviders:\n  sandbox: {fake: true}\n  llm:\n"+
		healthyDoctorEntry(t, "plain", "m")+coder+healthyDoctorEntry(t, "spare", "m")+
		"roles:\n  - name: SWE\n    handle: swe\n    llm: [plain, coder]\n"+
		"  - name: QA\n    handle: qa\n    llm: [plain, coder]\n")
	args := []string{"doctor", "-no-smoke", "-company", company, "-config", filepath.Join(dir, "absent.yaml")}
	var out, notes bytes.Buffer
	// Problems are expected — the claude-code entry's CLI is not there, and
	// no bridge is set — so the exit is not the case; the report is.
	_ = runLLM(noEnviron, args, &out, &notes)
	reports := map[string]string{}
	for _, section := range strings.Split(out.String(), "provider      : ")[1:] {
		key, _, _ := strings.Cut(section, "\n")
		reports[key] = section
	}
	for key, wants := range map[string][]string{
		"plain": {"executor      : seats: swe, qa", `runner: none for "custom"`},
		"coder": {"executor      : seats: swe, qa", `runner: "claude-code" is registered`},
	} {
		for _, want := range wants {
			if !strings.Contains(reports[key], want) {
				t.Errorf("the report of %s, which both seats' executors launch through, lacks %q:\n%s",
					key, want, reports[key])
			}
		}
	}
	if spare, ok := reports["spare"]; !ok || strings.Contains(spare, "executor      :") ||
		strings.Contains(spare, "runner:") {
		t.Errorf("the report of an entry no seat launches on is missing or talks about "+
			"launching:\n%s", spare)
	}
}

// A KEY NO ENTRY CARRIES IS REFUSED BY NAME, naming the ones doctor examines,
// rather than answered with a report of nothing — and a document with no
// entries at all, which is a company whose models are not added yet, says so.
func TestDoctorNamesTheEntriesItExamines(t *testing.T) {
	t.Parallel()
	var out, notes bytes.Buffer
	err := runLLM(noEnviron, append([]string{"doctor", "nobody"},
		doctorCompany(t, healthyDoctorEntry(t, "claude", "m"))...), &out, &notes)
	if err == nil || !strings.Contains(err.Error(), `no provider "nobody" (doctor examines claude)`) {
		t.Errorf("err = %v", err)
	}

	dir := t.TempDir()
	company := writeFile(t, dir, "company.yaml", "name: Acme\nroles:\n  - name: CEO\n    handle: ceo\n")
	err = runLLM(noEnviron, []string{"doctor", "-company", company, "-config", filepath.Join(dir, "absent.yaml")}, &out, &notes)
	if err == nil || !strings.Contains(err.Error(), "declares no providers.llm entries") {
		t.Errorf("err = %v", err)
	}
}

// A TOKEN PIPED TO -token-stdin IS HELD TO THE SIZE EVERY STORE KEEPS, before
// the rest of it is read: it was read whole, however much arrived, only for
// the store to refuse it — and a stream that never ended was never refused.
// The whitespace around a token is not part of it, and nothing but whitespace
// is no token.
func TestATokenPipedToBeStoredIsBounded(t *testing.T) {
	t.Parallel()
	if got, err := tokenFromReader(strings.NewReader("  sk-not-real\r\n")); err != nil || got != "sk-not-real" {
		t.Errorf("a token with whitespace around it read as %q, %v", got, err)
	}
	if _, err := tokenFromReader(strings.NewReader(" \n")); err == nil {
		t.Error("whitespace alone was taken as a token")
	}
	endless := &endlessReader{}
	if _, err := tokenFromReader(endless); !errors.Is(err, secrets.ErrValueTooLarge) {
		t.Fatalf("a stream that never ends answered %v, want ErrValueTooLarge", err)
	}
	if most := secrets.MaxValueBytes + 2; endless.read > most {
		t.Errorf("read %d bytes of a stream to refuse it, want at most %d", endless.read, most)
	}
	if _, err := tokenFromReader(strings.NewReader(strings.Repeat("t", secrets.MaxValueBytes+1))); !errors.Is(err, secrets.ErrValueTooLarge) {
		t.Errorf("a token a byte past the limit answered %v, want ErrValueTooLarge", err)
	}
}

// tokenLoginProvider is a claude-code entry under mode whose token mint is `sh`
// printing one, so a capture that ran would have a token to store.
func tokenLoginProvider(t *testing.T, mode cliagent.AuthMode) []cliAgentProvider {
	t.Helper()
	p, err := cliagent.New(cliagent.Config{
		Key: "claude", Agent: "claude-code", Model: "sonnet", StateDir: t.TempDir(),
		Timeout: 30 * time.Second, MaxConcurrent: 1,
		Overrides: map[string]any{
			"binary":             "sh",
			"capture_token_args": []any{"-c", "echo sk-ant-oat01-MINTED"},
		},
		Auth: cliagent.Auth{Mode: mode},
	})
	if err != nil {
		t.Fatalf("cliagent.New: %v", err)
	}
	return []cliAgentProvider{{key: "claude", provider: p}}
}

// A HEADLESS TOKEN IS STORED ONLY WHERE THE ENTRY'S AUTH MODE READS ONE. Under
// api-key the token variable is removed from the CLI's environment, and under
// inherit-env it is forwarded from the engine's own environment, never the
// secret store — yet `-capture-token` and `-token-stdin` minted and stored one
// under both, and printed an auth.token snippet that validation refuses under
// either. Each is now refused before anything is minted, read or stored,
// naming what the mode reads; inherit-env keeps -print-token, whose token
// reaches the CLI once exported in the engine's environment. Subscription is
// the control: it stores, so "nothing stored" below is a refusal and not a
// store the harness could not reach.
//
// Mutation: drop the refusal, and every api-key and inherit-env case stores a
// token (or prints one under api-key) and goes red.
func TestATokenIsStoredOnlyWhereTheEntrysAuthModeReadsIt(t *testing.T) {
	t.Parallel()
	const tokenVar = "CLAUDE_CODE_OAUTH_TOKEN"
	for _, tc := range []struct {
		name    string
		mode    cliagent.AuthMode
		req     loginRequest
		stdin   string
		stored  string
		printed string
		// refused is what the error must name, empty for a run that
		// succeeds; never is what the error and the output must not.
		refused []string
		hint    string
		never   []string
	}{
		{
			name: "subscription mints and stores", mode: cliagent.AuthSubscription,
			req: loginRequest{captureToken: true}, stored: "sk-ant-oat01-MINTED",
		},
		{
			name: "subscription stores a token it is handed", mode: cliagent.AuthSubscription,
			req: loginRequest{tokenStdin: true}, stdin: "sk-ant-oat01-PASTED\n", stored: "sk-ant-oat01-PASTED",
		},
		{
			name: "api-key refuses a mint", mode: cliagent.AuthAPIKey,
			req: loginRequest{captureToken: true},
			refused: []string{"auth.mode api-key", "removes " + tokenVar,
				"providers.llm.claude.auth.api_key, else ANTHROPIC_API_KEY", "`crewlet secrets set`"},
			never: []string{"${" + tokenVar + "}"},
		},
		{
			name: "api-key refuses a token it is handed", mode: cliagent.AuthAPIKey,
			req: loginRequest{tokenStdin: true}, stdin: "sk-ant-oat01-PASTED\n",
			refused: []string{"auth.mode api-key", "ANTHROPIC_API_KEY"},
			never:   []string{"sk-ant-oat01-PASTED"},
		},
		{
			name: "api-key refuses a mint even to stdout", mode: cliagent.AuthAPIKey,
			req:     loginRequest{captureToken: true, printToken: true},
			refused: []string{"auth.mode api-key"},
			never:   []string{"sk-ant-oat01-MINTED"},
		},
		{
			name: "inherit-env refuses to store a mint", mode: cliagent.AuthInheritEnv,
			req: loginRequest{captureToken: true},
			refused: []string{"auth.mode inherit-env", "never from the secret store",
				"`crewlet llm login claude -capture-token -print-token`", "export it as " + tokenVar},
			never: []string{"${" + tokenVar + "}"},
		},
		{
			name: "inherit-env refuses to store a token it is handed", mode: cliagent.AuthInheritEnv,
			req: loginRequest{tokenStdin: true}, stdin: "sk-ant-oat01-PASTED\n",
			refused: []string{"auth.mode inherit-env", "export the token you have as " + tokenVar},
			never:   []string{"sk-ant-oat01-PASTED"},
		},
		{
			name: "inherit-env prints a mint to export", mode: cliagent.AuthInheritEnv,
			req: loginRequest{captureToken: true, printToken: true}, printed: "sk-ant-oat01-MINTED",
			hint:  "Export it as " + tokenVar + " in the engine's environment",
			never: []string{"${" + tokenVar + "}"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := bootstrapWithKeyring(t, "k1")
			req := tc.req
			req.providers, req.key = tokenLoginProvider(t, tc.mode), "claude"
			req.companySecrets = llmStoreAt(t, noEnviron, "login", "-config", cfg)
			stdin := strings.NewReader(tc.stdin)
			req.stdin = stdin
			var out, errOut bytes.Buffer
			err := loginLLM(t.Context(), req, &out, &errOut)

			switch {
			case len(tc.refused) > 0 && err == nil:
				t.Fatalf("a token the mode never reads was accepted:\n%s%s", out.String(), errOut.String())
			case len(tc.refused) == 0 && err != nil:
				t.Fatalf("loginLLM: %v\n%s", err, errOut.String())
			}
			for _, want := range tc.refused {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not name %q: %v", want, err)
				}
			}
			if len(tc.refused) > 0 && stdin.Len() != len(tc.stdin) {
				t.Error("the token was read from stdin before the refusal")
			}
			if tc.hint != "" && !strings.Contains(errOut.String(), tc.hint) {
				t.Errorf("stderr does not say %q:\n%s", tc.hint, errOut.String())
			}
			if got := strings.TrimSpace(out.String()); tc.printed != "" && got != tc.printed {
				t.Errorf("stdout = %q, want the token %q alone", got, tc.printed)
			}
			said := out.String() + errOut.String()
			if err != nil {
				said += err.Error()
			}
			for _, bad := range tc.never {
				if strings.Contains(said, bad) {
					t.Errorf("the command said %q, which this mode never reads:\n%s", bad, said)
				}
			}

			// The login's run is over before the store is read: its broker
			// holds the store directory until it closes.
			req.companySecrets.Close()
			got, _, getErr := secretsCmd(t, cfg, "get", tokenVar, "-reveal")
			switch {
			case tc.stored == "" && !errors.Is(getErr, secrets.ErrNotFound):
				t.Errorf("%s read back as %q (%v), where this run must store nothing",
					tokenVar, got, getErr)
			case tc.stored != "" && got != tc.stored:
				t.Errorf("%s = %q (%v), want %q", tokenVar, got, getErr, tc.stored)
			}
		})
	}
}

// A TOKEN VARIABLE NO STORE CAN HOLD MINTS NOTHING. overrides.token_env can
// name a variable outside the reference grammar — MY-TOKEN — and `crewlet llm`
// loads its document only to run, so the admission rule that refuses one on
// every write never sees this file. The token was minted all the same, and
// then lost: the store refused the name after the vendor had issued it, and
// -print-token's next step, a ${MY-TOKEN} or an export of it, cannot be
// written; -token-stdin read the token and failed at the same write. Each is
// now refused before the mint or the read, naming the field; the capture
// script leaves a marker and the token's stdin records a read, so either is
// seen whatever became of the token. -from-host no longer recommends
// -capture-token for such an entry, since that is the refusal, and `llm
// doctor` names the field (which routes it then offers is cliagent's suite)
// and exits non-zero. And no store can hold the name, so no run treats it as a
// stored value: through a running node the run's batch never asks for it — the
// node refuses a whole request over one such name, which would fail the doctor
// on a value that cannot exist — and with the engine stopped the fleet's store,
// which refuses a read naming one the same way, is never asked for it either.
//
// Mutation: move the check after the mint, and the marker appears and stdin
// is read; drop it, and every case is accepted or fails at the store instead;
// stop leaving such a name out of the command's reads, and they name it; send
// it to a node that lists it — a stand-in, since no store can hold one — and
// the node refuses the doctor's read, which fails on it instead of the field.
func TestATokenVariableNoStoreCanHoldMintsNothing(t *testing.T) {
	t.Parallel()
	company := func(t *testing.T, marker string) string {
		t.Helper()
		return writeFile(t, t.TempDir(), "company.yaml", fmt.Sprintf(`name: Acme
providers:
  sandbox: {fake: true}
  llm:
    claude:
      agent: claude-code
      model: sonnet
      state_dir: %q
      overrides:
        binary: /bin/sh
        capture_token_args: ["-c", %q]
        token_env: MY-TOKEN
roles:
  - name: CEO
    handle: ceo
    llm: claude
`, t.TempDir(), fmt.Sprintf("touch '%s' && echo sk-ant-oat01-MINTED", marker)))
	}

	// refused holds a refusal of a token to the field and to having taken
	// nothing, and the run's output to naming no ${MY-TOKEN} and no token.
	refused := func(t *testing.T, err error, out, errOut *bytes.Buffer) {
		t.Helper()
		if err == nil {
			t.Fatalf("a token for MY-TOKEN was accepted:\n%s%s", out.String(), errOut.String())
		}
		for _, want := range []string{
			`providers.llm.claude.overrides.token_env is "MY-TOKEN"`,
			"none was minted, read or stored",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal does not say %q: %v", want, err)
			}
		}
		for _, bad := range []string{"sk-ant-oat01-MINTED", "sk-ant-oat01-PASTED"} {
			if strings.Contains(out.String()+errOut.String(), bad) {
				t.Errorf("a token was written:\n%s%s", out.String(), errOut.String())
			}
		}
		if strings.Contains(errOut.String(), "MY-TOKEN") {
			t.Errorf("the run's notes name MY-TOKEN as a stored value:\n%s", errOut.String())
		}
	}

	for _, tc := range []struct {
		name  string
		flags []string
	}{
		{"into the store", []string{"-capture-token"}},
		{"to stdout", []string{"-capture-token", "-print-token"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			marker := filepath.Join(t.TempDir(), "minted")
			args := append([]string{"login", "claude", "-company", company(t, marker),
				"-config", bootstrapWithKeyring(t, "k1")}, tc.flags...)
			var out, errOut bytes.Buffer
			err := runLLM(noEnviron, args, &out, &errOut)
			refused(t, err, &out, &errOut)
			if _, statErr := os.Stat(marker); !errors.Is(statErr, fs.ErrNotExist) {
				t.Errorf("the token was minted before the refusal (marker: %v)", statErr)
			}
		})
	}

	// -token-stdin through loginLLM, the one way to hand it a stdin of the
	// test's own: runLLM reads the process's. The entry is built the way
	// runLLM builds it, from the same run-only file.
	for _, printToken := range []bool{false, true} {
		t.Run(fmt.Sprintf("-token-stdin, -print-token=%v", printToken), func(t *testing.T) {
			t.Parallel()
			store := llmStoreAt(t, noEnviron, "login", "-config", bootstrapWithKeyring(t, "k1"))
			var out, errOut bytes.Buffer
			doc, keys, resolver, err := loadLLMConfig(t.Context(),
				company(t, filepath.Join(t.TempDir(), "minted")), store, "login", "claude", &errOut)
			if err != nil {
				t.Fatalf("loadLLMConfig: %v", err)
			}
			providers, err := cliAgents(doc, keys, resolver)
			if err != nil {
				t.Fatalf("cliAgents: %v", err)
			}
			stdin := &recordedReader{r: strings.NewReader("sk-ant-oat01-PASTED\n")}
			err = loginLLM(t.Context(), loginRequest{
				providers: providers, key: "claude", tokenStdin: true, printToken: printToken,
				companySecrets: store, stdin: stdin,
			}, &out, &errOut)
			refused(t, err, &out, &errOut)
			if stdin.read {
				t.Error("the token was read from stdin before the refusal")
			}
		})
	}

	t.Run("llm doctor names the field", func(t *testing.T) {
		t.Parallel()
		var out, errOut bytes.Buffer
		err := runLLM(noEnviron, []string{"doctor", "claude", "-no-smoke",
			"-company", company(t, filepath.Join(t.TempDir(), "minted")),
			"-config", bootstrapWithKeyring(t, "k1")}, &out, &errOut)
		if err == nil {
			t.Fatalf("the doctor found no problem with MY-TOKEN:\n%s", out.String())
		}
		if !strings.Contains(out.String(), `providers.llm.claude.overrides.token_env is "MY-TOKEN"`) {
			t.Errorf("the report does not name the field:\n%s", out.String())
		}
		if strings.Contains(errOut.String(), "MY-TOKEN") {
			t.Errorf("the run's notes name MY-TOKEN as a stored value:\n%s", errOut.String())
		}
	})

	t.Run("the command's reads leave the name out", func(t *testing.T) {
		t.Parallel()
		doc, err := config.LoadCompanyToRun(company(t, filepath.Join(t.TempDir(), "minted")), nil)
		if err != nil {
			t.Fatal(err)
		}
		if reads := llmEntryReads(doc, []string{"claude"}); slices.Contains(reads, "MY-TOKEN") {
			t.Errorf("llm reads %v, a name no store can hold among them", reads)
		}
	})

	t.Run("llm doctor through a running node asks it for no such name", func(t *testing.T) {
		t.Parallel()
		// A STAND-IN THAT LISTS THE NAME, which no store can: the node's own
		// listing filter would otherwise keep it out of the batch, and this
		// case is about the command's.
		node := newRevealNode(t, map[string]string{"MY-TOKEN": "sk-ant-oat01-HELD"})
		var out, errOut bytes.Buffer
		err := runLLM(tokenEnviron(cliFixtureToken), []string{"doctor", "claude", "-no-smoke",
			"-company", company(t, filepath.Join(t.TempDir(), "minted")),
			"-config", filepath.Join(t.TempDir(), "absent.yaml"), "-api", node.server.URL},
			&out, &errOut)
		if err == nil || !strings.Contains(err.Error(), "1 of 1 providers have problems") {
			t.Fatalf("the doctor = %v, want the entry's problem and a non-zero exit\n%s%s",
				err, out.String(), errOut.String())
		}
		if !strings.Contains(out.String(), `providers.llm.claude.overrides.token_env is "MY-TOKEN"`) {
			t.Errorf("the report does not name the field:\n%s", out.String())
		}
		if slices.Contains(node.reveals(), "MY-TOKEN") {
			t.Errorf("the node was asked for MY-TOKEN: %v", node.reveals())
		}
	})

	t.Run("llm doctor with the engine stopped asks the fleet's store for no such name", func(t *testing.T) {
		t.Parallel()
		// THE STORE REFUSES A WHOLE READ over one name outside the grammar,
		// as the node's reveal does, so the command must never ask: the
		// doctor reports the entry's problem rather than the store's.
		var out, errOut bytes.Buffer
		err := runLLM(noEnviron, []string{"doctor", "claude", "-no-smoke",
			"-company", company(t, filepath.Join(t.TempDir(), "minted")),
			"-config", bootstrapWithKeyring(t, "k1")}, &out, &errOut)
		if err == nil || !strings.Contains(err.Error(), "1 of 1 providers have problems") {
			t.Fatalf("the doctor = %v, want the entry's problem and a non-zero exit\n%s%s",
				err, out.String(), errOut.String())
		}
		if !strings.Contains(errOut.String(), "the fleet's store") {
			t.Errorf("the premise: the doctor did not reach the fleet's store:\n%s",
				errOut.String())
		}
		if !strings.Contains(out.String(), `providers.llm.claude.overrides.token_env is "MY-TOKEN"`) {
			t.Errorf("the report does not name the field:\n%s", out.String())
		}
	})

	t.Run("-from-host recommends no capture", func(t *testing.T) {
		t.Parallel()
		home := t.TempDir()
		path := filepath.Join(home, ".claude", ".credentials.json")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(`{"token":"personal"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(t.TempDir(), "minted")
		var out, errOut bytes.Buffer
		err := runLLM(noEnviron, []string{"login", "claude", "-from-host", "-home", home,
			"-company", company(t, marker), "-config", bootstrapWithKeyring(t, "k1")}, &out, &errOut)
		if err != nil {
			t.Fatalf("-from-host: %v\n%s", err, errOut.String())
		}
		if !strings.Contains(out.String(), "Adopted") {
			t.Fatalf("the premise: nothing was adopted:\n%s", out.String())
		}
		if strings.Contains(out.String(), "-capture-token") {
			t.Errorf("recommends -capture-token, which this entry refuses:\n%s", out.String())
		}
	})
}

// A TOKEN FOR A CLI THAT TAKES NONE IS NEITHER MINTED NOR READ. A profile that
// names no token_env — codex, like every built-in one but claude-code — hands
// its CLI no headless token, and `-token-stdin` read the token from stdin
// before it found that out, while a minting command a custom profile declared
// ran, and the vendor issued a token, before the refusal. Each is now refused
// first, in every mode and with -print-token too; the minting command leaves a
// marker and stdin records a read, so either is seen whatever became of the
// token.
//
// Mutation: drop the check, or move it after the read and the mint, and stdin
// is read, the marker appears and the refusal names no token_env.
func TestATokenForACLIThatTakesNoneIsNeitherMintedNorRead(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		req  loginRequest
	}{
		{"-token-stdin", loginRequest{tokenStdin: true}},
		{"-token-stdin -print-token", loginRequest{tokenStdin: true, printToken: true}},
		{"-capture-token", loginRequest{captureToken: true}},
		{"-capture-token -print-token", loginRequest{captureToken: true, printToken: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			marker := filepath.Join(t.TempDir(), "minted")
			p, err := cliagent.New(cliagent.Config{
				Key: "codex", Agent: "codex", StateDir: t.TempDir(),
				Timeout: 30 * time.Second, MaxConcurrent: 1,
				Overrides: map[string]any{
					"binary": "/bin/sh",
					"capture_token_args": []any{"-c",
						fmt.Sprintf("touch '%s' && echo sk-MINTED", marker)},
				},
			})
			if err != nil {
				t.Fatalf("cliagent.New: %v", err)
			}
			req := tc.req
			req.providers, req.key = []cliAgentProvider{{key: "codex", provider: p}}, "codex"
			req.companySecrets = llmStoreAt(t, noEnviron, "login", "-config", bootstrapWithKeyring(t, "k1"))
			stdin := &recordedReader{r: strings.NewReader("sk-PASTED\n")}
			req.stdin = stdin
			var out, errOut bytes.Buffer
			err = loginLLM(t.Context(), req, &out, &errOut)
			if err == nil {
				t.Fatalf("a token for a CLI that takes none was accepted:\n%s%s",
					out.String(), errOut.String())
			}
			for _, want := range []string{
				`the "codex" profile names no token_env`, "none was minted, read or stored",
				"`crewlet llm doctor codex`", "providers.llm.codex.overrides.token_env",
			} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not say %q: %v", want, err)
				}
			}
			if stdin.read {
				t.Error("the token was read from stdin before the refusal")
			}
			if _, statErr := os.Stat(marker); !errors.Is(statErr, fs.ErrNotExist) {
				t.Errorf("the token was minted before the refusal (marker: %v)", statErr)
			}
			if said := out.String() + errOut.String(); strings.Contains(said, "sk-") {
				t.Errorf("a token was written:\n%s", said)
			}
		})
	}
}

// AN API-KEY ENTRY IS TOLD ITS KEY BEFORE ANYTHING ABOUT ITS TOKEN VARIABLE.
// The mode removes the token variable from the CLI's environment, so a token
// reaches nothing under any variable — yet the refusal of a CLI that names
// none told api-key codex to sign in with a login or to name a token
// variable, and that of a name outside the grammar told api-key MY-TOKEN to
// name the variable its CLI reads, as if a token would then reach it. Both
// are now the api-key refusal, naming the key the mode reads, and offer no
// subscription where no token could be read under it; MY-TOKEN's names the
// field as well. inherit-env is judged as before: the refusal of a CLI that
// names no token variable, which a login signs in under that mode too.
//
// Mutation: judge the token variable before the mode, and the api-key cases
// are told a login or the variable alone; offer subscription whatever the
// profile names, and they are told it.
func TestAnAPIKeyEntryIsToldItsKeyBeforeItsTokenVariable(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, agent   string
		mode          cliagent.AuthMode
		overrides     map[string]any
		want, notWant []string
	}{
		{
			name: "codex under api-key", agent: "codex", mode: cliagent.AuthAPIKey,
			want: []string{"auth.mode api-key", `the "codex" profile names no token_env`,
				"providers.llm.sub.auth.api_key, else CODEX_API_KEY", "`crewlet secrets set`",
				"none was minted or stored"},
			notWant: []string{"auth.mode: subscription", "crewlet llm doctor", "overrides.token_env",
				"a login of its own"},
		},
		{
			name: "MY-TOKEN under api-key", agent: "claude-code", mode: cliagent.AuthAPIKey,
			overrides: map[string]any{"token_env": "MY-TOKEN"},
			want: []string{"auth.mode api-key", `removes "MY-TOKEN"`,
				"providers.llm.sub.auth.api_key, else ANTHROPIC_API_KEY", "`crewlet secrets set`",
				`providers.llm.sub.overrides.token_env is "MY-TOKEN"`, "none was minted or stored"},
			notWant: []string{"auth.mode: subscription"},
		},
		{
			name: "codex under inherit-env", agent: "codex", mode: cliagent.AuthInheritEnv,
			want: []string{`the "codex" profile names no token_env`, "`crewlet llm doctor sub`",
				"providers.llm.sub.overrides.token_env", "none was minted, read or stored"},
			notWant: []string{"auth.mode api-key"},
		},
	} {
		for _, flag := range []struct {
			name string
			req  loginRequest
		}{
			{"-capture-token", loginRequest{captureToken: true}},
			{"-token-stdin", loginRequest{tokenStdin: true}},
		} {
			t.Run(tc.name+" "+flag.name, func(t *testing.T) {
				t.Parallel()
				marker := filepath.Join(t.TempDir(), "minted")
				overrides := map[string]any{
					"binary": "/bin/sh",
					"capture_token_args": []any{"-c",
						fmt.Sprintf("touch '%s' && echo sk-MINTED", marker)},
				}
				for name, value := range tc.overrides {
					overrides[name] = value
				}
				p, err := cliagent.New(cliagent.Config{
					Key: "sub", Agent: tc.agent, StateDir: t.TempDir(),
					Timeout: 30 * time.Second, MaxConcurrent: 1,
					Overrides: overrides, Auth: cliagent.Auth{Mode: tc.mode},
				})
				if err != nil {
					t.Fatalf("cliagent.New: %v", err)
				}
				req := flag.req
				req.providers, req.key = []cliAgentProvider{{key: "sub", provider: p}}, "sub"
				req.companySecrets = llmStoreAt(t, noEnviron, "login", "-config", bootstrapWithKeyring(t, "k1"))
				stdin := &recordedReader{r: strings.NewReader("sk-PASTED\n")}
				req.stdin = stdin
				var out, errOut bytes.Buffer
				err = loginLLM(t.Context(), req, &out, &errOut)
				if err == nil {
					t.Fatalf("a token the entry never reads was accepted:\n%s%s", out.String(), errOut.String())
				}
				for _, want := range tc.want {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("the refusal does not say %q: %v", want, err)
					}
				}
				for _, bad := range tc.notWant {
					if strings.Contains(err.Error(), bad) {
						t.Errorf("the refusal says %q, a route this entry cannot take: %v", bad, err)
					}
				}
				if stdin.read {
					t.Error("the token was read from stdin before the refusal")
				}
				if _, statErr := os.Stat(marker); !errors.Is(statErr, fs.ErrNotExist) {
					t.Errorf("the token was minted before the refusal (marker: %v)", statErr)
				}
			})
		}
	}
}

// recordedReader is a stdin that records whether anything read it.
type recordedReader struct {
	r    io.Reader
	read bool
}

func (r *recordedReader) Read(p []byte) (int, error) {
	r.read = true
	return r.r.Read(p)
}

// -FROM-HOST RECOMMENDS A TOKEN ONLY WHERE THE ENTRY WOULD READ ONE. After an
// adopted login it says to prefer `-capture-token`, which avoids the shared
// refresh token — advice that, under api-key or inherit-env, leads to the
// refusal above, and that a CLI with a token variable but no minting command
// cannot follow at all: `-capture-token` refuses for it. Subscription on a CLI
// that mints is the control: it is told.
//
// Mutation: print the preference whatever the mode, and the api-key and
// inherit-env cases go red; whenever the CLI names a token variable, and the
// case with no minting command does.
func TestAdoptingAHostLoginRecommendsATokenOnlyWhereOneIsRead(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		mode   cliagent.AuthMode
		noMint bool
		prefer bool
	}{
		{name: string(cliagent.AuthSubscription), mode: cliagent.AuthSubscription, prefer: true},
		{name: string(cliagent.AuthAPIKey), mode: cliagent.AuthAPIKey},
		{name: string(cliagent.AuthInheritEnv), mode: cliagent.AuthInheritEnv},
		{name: "subscription with no minting command", mode: cliagent.AuthSubscription, noMint: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			providers := tokenLoginProvider(t, tc.mode)
			if tc.noMint {
				p, err := cliagent.New(cliagent.Config{
					Key: "claude", Agent: "claude-code", Model: "sonnet", StateDir: t.TempDir(),
					Timeout: 30 * time.Second, MaxConcurrent: 1,
					Overrides: map[string]any{"capture_token_args": []any{}},
				})
				if err != nil {
					t.Fatalf("cliagent.New: %v", err)
				}
				if p.MintsHeadlessToken() || p.Profile().TokenEnv == "" {
					t.Fatalf("the premise: a token variable and no minting command, got %+v",
						p.Profile().CaptureTokenArgs)
				}
				providers = []cliAgentProvider{{key: "claude", provider: p}}
			}
			home := t.TempDir()
			path := filepath.Join(home, ".claude", ".credentials.json")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(`{"token":"personal"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			var out, errOut bytes.Buffer
			err := loginLLM(t.Context(), loginRequest{
				providers: providers, key: "claude", home: home,
				fromHost: true, stdin: strings.NewReader(""),
			}, &out, &errOut)
			if err != nil {
				t.Fatalf("-from-host: %v\n%s", err, errOut.String())
			}
			if !strings.Contains(out.String(), "Adopted") {
				t.Fatalf("the premise: nothing was adopted:\n%s", out.String())
			}
			if got := strings.Contains(out.String(), "-capture-token"); got != tc.prefer {
				t.Errorf("recommends -capture-token = %v, want %v:\n%s", got, tc.prefer, out.String())
			}
		})
	}
}

// llmStoreAt is the store a `crewlet llm SUB` run routes to under the store
// flags given, as [runLLM] opens it. With no -config among them the run has no
// Tier A file, and goes through -api alone.
func llmStoreAt(t *testing.T, environ config.Source, sub string, flags ...string) *companyStore {
	t.Helper()
	fs := flag.NewFlagSet("llm "+sub, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	at := addStoreFlags(fs)
	if err := fs.Parse(flags); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(flags, "-config") {
		*at.bootstrap = filepath.Join(t.TempDir(), "absent.yaml")
	}
	store, err := openCompanyStore(environ, at, "crewlet llm "+sub)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	return store
}

// AN LLM WRITE SAYS WHERE IT WENT AND WHEN A RUNNING ENGINE READS IT, as
// `crewlet secrets set` says both. The engine resolves a token from the
// snapshot it builds at each apply, and restores a bundle only when it builds
// a company to run — at boot and at every apply — so a write through a running
// node signs nothing in until the next apply, and one made directly with this
// node's engine stopped is read by it at its start, and by a node already
// running on the same store at its next apply. A login or an export that
// said only where the value went read as finished while the fleet went on
// without it. The token is also said to be read under its conventional name,
// so the company document needs no reference to it.
//
// Mutation: print the old line, and every case misses its note.
func TestAnLLMWriteSaysWhenARunningEngineReadsIt(t *testing.T) {
	t.Parallel()
	for _, route := range []struct {
		name string
		// open is the run's store on this route for a subcommand, and held
		// answers what it then holds under a name, once the run is over.
		open func(t *testing.T, sub string) (*companyStore, func(name string) bool)
		note string
	}{
		{"through a running node, with no Tier A here", func(t *testing.T, sub string) (*companyStore, func(string) bool) {
			node := newRevealNode(t, nil)
			return llmStoreAt(t, tokenEnviron(cliFixtureToken), sub, "-api", node.server.URL),
				func(name string) bool { _, ok := node.holds(name); return ok }
		}, reloadNote(&configapi.Reloaded{RevisionID: "r-2", Epoch: 7})},
		{"the fleet's store, with the engine stopped", func(t *testing.T, sub string) (*companyStore, func(string) bool) {
			cfg := bootstrapWithKeyring(t, "k1")
			store := llmStoreAt(t, noEnviron, sub, "-config", cfg)
			return store, func(name string) bool {
				store.Close()
				_, ok := fleetRowsOf(t, cfg)[name]
				return ok
			}
		}, secretsStoppedNote},
	} {
		t.Run(route.name, func(t *testing.T) {
			t.Parallel()
			t.Run("login -capture-token", func(t *testing.T) {
				t.Parallel()
				store, held := route.open(t, "login")
				var stdout, stderr bytes.Buffer
				err := loginLLM(t.Context(), loginRequest{
					providers: tokenLoginProvider(t, cliagent.AuthSubscription), key: "claude",
					captureToken: true, companySecrets: store, stdin: strings.NewReader(""),
				}, &stdout, &stderr)
				if err != nil {
					t.Fatalf("login: %v\n%s", err, stderr.String())
				}
				if !held("CLAUDE_CODE_OAUTH_TOKEN") {
					t.Fatal("the token was not written to the run's store")
				}
				for _, want := range []string{route.note, "read under that name by convention",
					`token: "${CLAUDE_CODE_OAUTH_TOKEN}"`} {
					if !strings.Contains(stdout.String(), want) {
						t.Errorf("the write does not say %q:\n%s", want, stdout.String())
					}
				}
			})
			t.Run("export -secret-store", func(t *testing.T) {
				t.Parallel()
				store, held := route.open(t, "export")
				providers := bundleProvider(t)
				creds := filepath.Join(providers[0].provider.Workspace().CredentialsDir(),
					"creds.json")
				if err := os.MkdirAll(filepath.Dir(creds), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(creds, []byte(`{"token":"t"}`), 0o600); err != nil {
					t.Fatal(err)
				}
				var stdout bytes.Buffer
				if err := exportLLM(t.Context(), providers, "sub", true, store, &stdout); err != nil {
					t.Fatalf("export: %v", err)
				}
				if !held("CREWLET_LLM_CLI_SUB_CREDENTIALS") {
					t.Fatal("the bundle was not written to the run's store")
				}
				out := stdout.String()
				for _, want := range []string{route.note,
					"A node restores it when it builds a company to run, at boot and at every\napply",
					"read under that name by\nconvention"} {
					if !strings.Contains(out, want) {
						t.Errorf("the export does not say %q:\n%s", want, out)
					}
				}
			})
		})
	}
}

// A TOKEN PIPED TO `crewlet llm login -token-stdin` IS REFUSED HAVING READ NO
// MORE THAN THE SIZE EVERY STORE KEEPS, AND NOTHING IS STORED.
//
// The login read its stdin to the end before the store saw the token, so a
// pipe pointed at the wrong file was read whole only to be refused, and one
// that never ended was never refused at all. [TestATokenPipedToBeStoredIsBounded]
// holds the token's reader to the bound; this holds the login to that reader.
//
// Mutation: read the login's stdin whole again, and the endless stream runs
// into its reader's guard and is answered as a failed read, not as a token
// too long, having read far past the bound.
func TestATokenPipedToLoginIsBoundedBeforeItIsStored(t *testing.T) {
	t.Parallel()
	cfg := bootstrapWithKeyring(t, "k1")
	endless := &endlessReader{}
	store := llmStoreAt(t, noEnviron, "login", "-config", cfg)
	var out, errOut bytes.Buffer
	err := loginLLM(t.Context(), loginRequest{
		providers: tokenLoginProvider(t, cliagent.AuthSubscription), key: "claude",
		tokenStdin: true, companySecrets: store, stdin: endless,
	}, &out, &errOut)
	if !errors.Is(err, secrets.ErrValueTooLarge) {
		t.Fatalf("a token on a stream that never ends answered %v, want ErrValueTooLarge", err)
	}
	if most := secrets.MaxValueBytes + 2; endless.read > most {
		t.Errorf("read %d bytes of the stream to refuse it, want at most %d", endless.read, most)
	}
	if out.Len() != 0 {
		t.Errorf("the refused login said:\n%s", out.String())
	}
	store.Close()
	if got, _, getErr := secretsCmd(t, cfg, "get", "CLAUDE_CODE_OAUTH_TOKEN",
		"-reveal"); !errors.Is(getErr, secrets.ErrNotFound) {
		t.Errorf("the refused token read back as %d bytes (%v), want nothing stored",
			len(got), getErr)
	}
}

// A CREDENTIAL BUNDLE NO STORE KEEPS IS REFUSED BEFORE IT IS WRITTEN, NAMING
// THE ROUTES THAT DO CARRY ONE.
//
// Every store holds a value to [secrets.MaxValueBytes], and its refusal says
// a credential is a few kilobytes, so the input is most likely the wrong one.
// A bundle is not a pasted credential but the entry's whole credentials
// directory, packed as it is, so an operator told to find the right input
// had none to find. `crewlet llm export -secret-store` now refuses it itself,
// on either route, naming what carries a bundle that size — stdout into the
// operator's own secret manager and `crewlet llm import` on each host, or a
// login on each host — and stores nothing.
//
// Mutation: drop the export's own check, and the store's refusal names none
// of those routes.
func TestABundleNoStoreKeepsIsRefusedInABundlesWords(t *testing.T) {
	t.Parallel()
	const name = "CREWLET_LLM_CLI_SUB_CREDENTIALS"
	for _, route := range []struct {
		name string
		// open is the export's store on this route, and held answers whether
		// it then holds the bundle's name, once the export is over.
		open func(t *testing.T) (*companyStore, func() bool)
	}{
		{"through a running node", func(t *testing.T) (*companyStore, func() bool) {
			node := newRevealNode(t, nil)
			return llmStoreAt(t, tokenEnviron(cliFixtureToken), "export", "-api", node.server.URL),
				func() bool { _, ok := node.holds(name); return ok }
		}},
		{"the fleet's store, with the engine stopped", func(t *testing.T) (*companyStore, func() bool) {
			cfg := bootstrapWithKeyring(t, "k1")
			store := llmStoreAt(t, noEnviron, "export", "-config", cfg)
			return store, func() bool {
				store.Close()
				_, ok := fleetRowsOf(t, cfg)[name]
				return ok
			}
		}},
	} {
		t.Run(route.name, func(t *testing.T) {
			t.Parallel()
			store, held := route.open(t)
			providers := bundleProvider(t)
			creds := filepath.Join(providers[0].provider.Workspace().CredentialsDir(), "creds.json")
			if err := os.MkdirAll(filepath.Dir(creds), 0o700); err != nil {
				t.Fatal(err)
			}
			// INCOMPRESSIBLE, so the packed bundle is longer than the file:
			// gzip cannot shrink it and base64 spells it a third longer.
			login := make([]byte, secrets.MaxValueBytes)
			_, _ = rand.NewChaCha8([32]byte{}).Read(login)
			if err := os.WriteFile(creds, login, 0o600); err != nil {
				t.Fatal(err)
			}
			var stdout bytes.Buffer
			err := exportLLM(t.Context(), providers, "sub", true, store, &stdout)
			if !errors.Is(err, secrets.ErrValueTooLarge) {
				t.Fatalf("an export past the limit answered %v, want ErrValueTooLarge", err)
			}
			for _, want := range []string{"credential bundle", "without -secret-store",
				"`crewlet llm import sub`", "`crewlet llm login sub`"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not say %q: %v", want, err)
				}
			}
			if strings.Contains(err.Error(), "wrong input") {
				t.Errorf("the refusal calls a bundle the wrong input: %v", err)
			}
			if stdout.Len() != 0 {
				t.Errorf("the refused export said:\n%s", stdout.String())
			}
			if held() {
				t.Errorf("the refused bundle was stored as %s", name)
			}
		})
	}
}

// A PASSWORD PIPED TO `crewlet llm login -username U -password-stdin` IS READ
// NO FURTHER THAN A PASSWORD IS, and refused before the CLI's login runs.
//
// The command read its stdin to the end and handed the vendor's login
// whatever arrived, so a file piped by mistake was read whole into memory and
// a stream that never ended was a login that never began. A password is never
// stored, so the bound is a password's ([maxPipedPasswordBytes]), not the size
// every store keeps; the login script below leaves a marker if it ever runs.
//
// Mutation: read the login's stdin whole again, and the endless stream runs
// into its reader's guard and is answered as a failed read.
func TestAPasswordPipedToLLMLoginIsBoundedBeforeTheCLIRuns(t *testing.T) {
	t.Parallel()
	marker := filepath.Join(t.TempDir(), "login-ran")
	p, err := cliagent.New(cliagent.Config{
		Key: "gateway", Agent: "custom", StateDir: t.TempDir(),
		Timeout: 30 * time.Second, MaxConcurrent: 1,
		Overrides: map[string]any{
			"binary": "sh", "complete_args": []any{"-p"}, "model_args": []any{},
			"output":           "text",
			"credential_paths": []any{".fake/creds.json"},
			"stdin_login": map[string]any{
				"args":           []any{"-c", "touch " + marker},
				"stdin_template": "{password}\n",
			},
		},
	})
	if err != nil {
		t.Fatalf("cliagent.New: %v", err)
	}
	endless := &endlessReader{}
	var out, errOut bytes.Buffer
	err = loginLLM(t.Context(), loginRequest{
		providers: []cliAgentProvider{{key: "gateway", provider: p}}, key: "gateway",
		username: "ops@example.com", passwordStdin: true, stdin: endless,
	}, &out, &errOut)
	if !errors.Is(err, errPipedPasswordTooLong) {
		t.Errorf("a password on a stream that never ends answered %v, want errPipedPasswordTooLong", err)
	} else if strings.Contains(err.Error(), "second-factor") {
		t.Errorf("the refusal speaks of a second-factor code this login never reads: %v", err)
	}
	if most := maxPipedPasswordBytes + len("\r\n") + 1; endless.read > most {
		t.Errorf("read %d bytes of the stream to refuse it, want at most %d", endless.read, most)
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("the CLI's login ran for a refused password (%v)", statErr)
	}
}
