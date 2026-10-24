package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm/cliagent"
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
	if err := importLLM(t.Context(), target, "sub", strings.NewReader(blob+"\n"), &out); err != nil {
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
	err := importLLM(t.Context(), target, "sub", strings.NewReader("anything"), &out)
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
	err := importLLM(t.Context(), bundleProvider(t), "sub", strings.NewReader("   \n"), &out)
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
// so references resolve from the environment alone.
func doctorCompany(t *testing.T, providers string) []string {
	t.Helper()
	dir := t.TempDir()
	company := writeFile(t, dir, "company.yaml", "name: Acme\nproviders:\n  llm:\n"+providers+
		"roles:\n  - name: CEO\n    handle: ceo\n    llm: claude\n")
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
			req.providers, req.key, req.bootstrapPath = tokenLoginProvider(t, tc.mode), "claude", cfg
			stdin := strings.NewReader(tc.stdin)
			req.stdin = stdin
			var out, errOut bytes.Buffer
			err := loginLLM(t.Context(), noEnviron, req, &out, &errOut)

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

			got, _, getErr := secretsCmd(t, cfg, "get", tokenVar, "-reveal")
			switch {
			case tc.stored == "" && getErr == nil:
				t.Errorf("%s was stored as %q, where this run must store nothing", tokenVar, got)
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
// doctor` names the field (which routes it then offers is cliagent's suite).
// And no run names ${MY-TOKEN} among the values a stopped node cannot read: no
// store can hold it, so there is nothing for the fleet to hold.
//
// Mutation: move the check after the mint, and the marker appears and stdin
// is read; drop it, and every case is accepted or fails at the store instead;
// stop leaving such a name out of the command's reads, and stderr names it.
func TestATokenVariableNoStoreCanHoldMintsNothing(t *testing.T) {
	t.Parallel()
	company := func(t *testing.T, marker string) string {
		t.Helper()
		return writeFile(t, t.TempDir(), "company.yaml", fmt.Sprintf(`name: Acme
providers:
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
		if strings.Contains(errOut.String(), "${MY-TOKEN}") {
			t.Errorf("stderr names ${MY-TOKEN} as a value the fleet may hold:\n%s", errOut.String())
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
			cfg := bootstrapWithKeyring(t, "k1")
			var out, errOut bytes.Buffer
			doc, keys, resolver, closeResolver, err := loadLLMConfig(t.Context(), noEnviron,
				company(t, filepath.Join(t.TempDir(), "minted")), cfg, "", "login", "claude", &errOut)
			if err != nil {
				t.Fatalf("loadLLMConfig: %v", err)
			}
			defer closeResolver()
			providers, err := cliAgents(doc, keys, resolver)
			if err != nil {
				t.Fatalf("cliAgents: %v", err)
			}
			stdin := &recordedReader{r: strings.NewReader("sk-ant-oat01-PASTED\n")}
			err = loginLLM(t.Context(), noEnviron, loginRequest{
				providers: providers, key: "claude", tokenStdin: true, printToken: printToken,
				bootstrapPath: cfg, stdin: stdin,
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
		if strings.Contains(errOut.String(), "${MY-TOKEN}") {
			t.Errorf("stderr names ${MY-TOKEN} as a value the fleet may hold:\n%s", errOut.String())
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
			req.bootstrapPath = bootstrapWithKeyring(t, "k1")
			stdin := &recordedReader{r: strings.NewReader("sk-PASTED\n")}
			req.stdin = stdin
			var out, errOut bytes.Buffer
			err = loginLLM(t.Context(), noEnviron, req, &out, &errOut)
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
				"providers.llm.sub.auth.api_key, else OPENAI_API_KEY", "`crewlet secrets set`",
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
				req.bootstrapPath = bootstrapWithKeyring(t, "k1")
				stdin := &recordedReader{r: strings.NewReader("sk-PASTED\n")}
				req.stdin = stdin
				var out, errOut bytes.Buffer
				err = loginLLM(t.Context(), noEnviron, req, &out, &errOut)
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
			err := loginLLM(t.Context(), noEnviron, loginRequest{
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
