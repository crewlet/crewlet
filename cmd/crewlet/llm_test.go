package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent"
	"github.com/crewlet/crewlet/internal/sandbox"
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
	blob, err := source[0].provider.ExportBundle()
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

// THE DOCTOR READS THE BRIDGE URL AS THE ENGINE DOES, through the one grammar
// of a URL a box dials (sandbox.ParseBoxURL). It printed the variable as it
// was, so a value with no scheme — which every box would have been handed, and
// which the engine now refuses its boot on — read as a working bridge, and a
// deploy script gating on doctor passed a node the next boot refuses.
//
// The variable travels in the environment the invocation is HANDED, which is
// where the doctor reads it, as the engine reads its own.
func TestTheLLMDoctorReportsARefusedBridgeURL(t *testing.T) {
	t.Parallel()
	environ := config.MapSource{mcpbridge.BaseURLVar: "engine.example.com:8000"}
	dir := t.TempDir()
	// The claude-code profile over a binary no host has, so the report
	// runs nothing on the machine the suite runs on.
	company := writeFile(t, dir, "company.yaml", "name: Acme\nproviders:\n"+
		"  sandbox: {fake: true}\n  llm:\n    sub:\n      agent: claude-code\n      model: sonnet\n"+
		"      mode: agent\n"+
		"      state_dir: "+filepath.Join(dir, "sub")+"\n"+
		"      overrides: {binary: crewlet-no-such-cli-binary}\n"+
		"roles:\n  - name: CEO\n    handle: ceo\n    llm: sub\n")
	var out, notes bytes.Buffer
	err := runLLM(environ, []string{"doctor", "sub", "-no-smoke", "-company", company,
		"-config", filepath.Join(dir, "absent.yaml")}, &out, &notes)
	if err == nil || !strings.Contains(err.Error(), "1 of 1 providers have problems") {
		t.Fatalf("err = %v\n%s", err, out.String())
	}
	report := out.String()
	if !strings.Contains(report, "tool bridge: refused — "+mcpbridge.BaseURLVar) {
		t.Errorf("the bridge line does not carry the refusal:\n%s", report)
	}
	if !strings.Contains(report, "The engine refuses to boot on this value") {
		t.Errorf("no problem says the engine refuses to boot on it:\n%s", report)
	}
	if strings.Contains(report, "engine.example.com:8000") {
		t.Errorf("the report prints the refused value as the bridge:\n%s", report)
	}
}

// A BOX URL THE ENGINE REFUSES ITS BOOT ON FAILS THE DOCTOR, WHATEVER MODE THE
// COMPANY'S ENTRIES RUN IN. The doctor read the bridge's URL only for an
// agent-mode entry and the receiver's never, so a company of text-mode entries
// — this one — got a clean report and a zero exit beside a value the next boot
// refuses, and a deploy script gating on doctor passed a node that then ran no
// seat at all. Every URL a box dials back to the engine — the receiver's, the
// bridge's and the code host's — is the NODE's, so the refusal is reported as
// the node's, ahead of the entries, and never repeats the value.
//
// Mutation: the doctor reading the box URLs other than through
// engine.ReadBoxURLs → the code host's row passes, red.
//
// The variables travel in the environment the invocation is HANDED, which is
// where the doctor reads them, as the engine reads its own.
func TestTheLLMDoctorRefusesABoxURLTheBootRefuses(t *testing.T) {
	t.Parallel()
	for _, variable := range []string{sandbox.OtelReceiverURLVar, mcpbridge.BaseURLVar, sandbox.BoxGitURLVar} {
		t.Run(variable, func(t *testing.T) {
			t.Parallel()
			environ := config.MapSource{variable: "https://ops:hunter2@engine.example.com"}
			var out, notes bytes.Buffer
			err := runLLM(environ, append([]string{"doctor", "-no-smoke"},
				doctorCompany(t, healthyDoctorEntry(t, "claude", "m"))...), &out, &notes)
			report := out.String()
			if err == nil || !strings.Contains(err.Error(), "the engine refuses to boot on 1 of this node's box URLs") {
				t.Fatalf("err = %v, want the node's refusal\n%s", err, report)
			}
			if strings.Contains(err.Error(), "providers have problems") {
				t.Errorf("the refusal is counted against a provider: %v", err)
			}
			for _, want := range []string{
				"node          : this process's environment, as `crewlet run` reads it at boot\n" +
					"problems:\n  - " + variable + " carries user information",
				"The engine refuses to boot on this value",
				"provider      : claude\n",
				"problems      : none\n",
			} {
				if !strings.Contains(report, want) {
					t.Errorf("report lacks %q:\n%s", want, report)
				}
			}
			if strings.Contains(report, "hunter2") || strings.Contains(err.Error(), "hunter2") {
				t.Errorf("the doctor repeats the URL's password:\n%s", report)
			}
		})
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

// -FROM-HOST RECOMMENDS A TOKEN ONLY WHERE THE ENTRY WOULD READ ONE. After an
// adopted login it says to prefer `-capture-token`, which avoids the shared
// refresh token — advice that, under api-key or inherit-env, leads to the
// refusal above. Subscription is the control: it is told.
//
// Mutation: print the preference whatever the mode, and the api-key and
// inherit-env cases go red.
func TestAdoptingAHostLoginRecommendsATokenOnlyWhereOneIsRead(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		mode   cliagent.AuthMode
		prefer bool
	}{
		{cliagent.AuthSubscription, true},
		{cliagent.AuthAPIKey, false},
		{cliagent.AuthInheritEnv, false},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			t.Parallel()
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
				providers: tokenLoginProvider(t, tc.mode), key: "claude", home: home,
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
