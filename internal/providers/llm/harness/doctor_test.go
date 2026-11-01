package harness

import (
	"bytes"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
	"github.com/crewlet/crewlet/internal/toolsession"
)

// The CLI runs on the ENGINE host. A provider whose binary is missing there
// must say exactly that, because the config looks correct and the failure
// otherwise surfaces as an unexplained turn failure much later.
func TestDoctorReportsAMissingBinary(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	t.Cleanup(func() { forgetWorkspace(dir) })
	p, err := New(Config{
		Key: "sub", Harness: "custom", StateDir: dir, Timeout: time.Second, MaxConcurrent: 1,
		Overrides: map[string]any{
			"binary": "crewlet-no-such-cli-binary", "complete_args": []any{"-p"},
			"output": "text", "model_args": []any{},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d := p.Diagnose(t.Context(), DiagnoseOptions{Smoke: true})
	if d.Healthy() {
		t.Fatal("a missing binary was reported as healthy")
	}
	if d.BinaryPath != "not on PATH" {
		t.Errorf("BinaryPath = %q", d.BinaryPath)
	}
	joined := strings.Join(d.Problems, "\n")
	if !strings.Contains(joined, "ENGINE host") {
		t.Errorf("the problem does not say where the CLI must be installed:\n%s", joined)
	}
	if d.Smoke != "skipped: no binary to run" {
		t.Errorf("Smoke = %q, want a skip rather than a second failure", d.Smoke)
	}
}

// "No sign-in" on a machine where the CLI plainly works must explain itself, or
// an operator goes looking for a bug that is a missing -from-host.
func TestDoctorNamesAnUnadoptedHostLogin(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(func() { forgetWorkspace(dir) })
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeHostLogin(t, home, `{"token":"personal"}`)

	p, err := New(Config{
		Key: "sub", Harness: "custom", StateDir: dir, Timeout: time.Second, MaxConcurrent: 1,
		Overrides: map[string]any{
			"binary": os.Args[0], "complete_args": []any{"-p"}, "output": "text",
			"model_args": []any{}, "version_args": []any{"-test.run=TestFakeHarness"},
			"credential_paths":      []any{".fake/creds.json"},
			"host_credential_paths": []any{".fake/creds.json"},
			"token_env":             "FAKE_OAUTH_TOKEN",
			"capture_token_args":    []any{"setup-token"},
		},
		// The version probe runs the binary, so it is the fake CLI answering
		// as a real one would — not this suite re-run with no test selected,
		// whose testing-package warning was being read as a version.
		Env: fakeChildEnv(map[string]string{"FAKE_STDOUT": "fake-cli 1.0.0"}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d := p.Diagnose(t.Context(), DiagnoseOptions{Smoke: false})
	if d.Credentials != "none on disk" {
		t.Fatalf("Credentials = %q", d.Credentials)
	}
	if len(d.HostLogin) == 0 {
		t.Fatal("the host login was not found, so the report cannot explain itself")
	}
	joined := strings.Join(d.Problems, "\n")
	for _, want := range []string{"login sub -from-host`", "with `-capture-token`", "FAKE_OAUTH_TOKEN"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the problem does not offer %q:\n%s", want, joined)
		}
	}
}

// THE DOCTOR'S ROUTES FOLLOW THE ENTRY'S AUTH MODE, not only its CLI. On a
// machine with a personal Claude login, every entry with no sign-in was told to
// adopt it with -from-host or — preferred — to mint a headless token with
// -capture-token. An api-key entry never reads that token (the mode removes it
// from the CLI's environment) and should not be handed a plan login either,
// which the CLI would bill beside its key by the vendor's precedence; an
// inherit-env entry never looks in the secret store the token is stored in.
// Each is told what its mode reads, worded as the remote run's refusal and
// `crewlet llm login` word it ([Provider.ModeSignIn]).
//
// The subscription case is the control: it proves the host login was found, so
// the routes missing from the other two are missing by mode, not by accident.
// Not parallel: HOME is the fake human's, and inherit-env reads the process
// environment, emptied here of both variables it forwards.
//
// Mutation: offer the host-login routes whatever the mode, and the api-key and
// inherit-env cases name -capture-token and go red.
func TestTheDoctorsRoutesFollowTheEntrysAuthMode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	path := filepath.Join(home, ".claude", ".credentials.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"token":"personal"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		mode    AuthMode
		want    []string
		notWant []string
	}{
		{
			mode: AuthSubscription,
			want: []string{"`crewlet llm login sub -from-host`", "CLAUDE_CODE_OAUTH_TOKEN with `-capture-token`"},
		},
		{
			mode: AuthAPIKey,
			want: []string{"auth.mode is api-key", "providers.llm.sub.auth.api_key, else ANTHROPIC_API_KEY",
				"`crewlet secrets set`"},
			notWant: []string{"capture-token", "-from-host", "crewlet llm login", "CLAUDE_CODE_OAUTH_TOKEN"},
		},
		{
			mode: AuthInheritEnv,
			want: []string{"auth.mode is inherit-env", "CLAUDE_CODE_OAUTH_TOKEN or ANTHROPIC_API_KEY",
				"engine's own process environment, never from the secret store",
				"`crewlet llm login sub -from-host`"},
			notWant: []string{"capture-token", "crewlet secrets set"},
		},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			dir := t.TempDir()
			t.Cleanup(func() { forgetWorkspace(dir) })
			p, err := New(Config{
				Key: "sub", Harness: "claude-code", StateDir: dir, Timeout: time.Second, MaxConcurrent: 1,
				// No binary to run: the version probe is skipped and the
				// report is judged on configuration, files and HOME.
				Overrides: map[string]any{"binary": "crewlet-no-such-cli-binary"},
				Auth:      Auth{Mode: tc.mode},
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			d := p.Diagnose(t.Context(), DiagnoseOptions{})
			if len(d.HostLogin) == 0 {
				t.Fatal("the premise: the host login was not found")
			}
			var problem string
			for _, line := range d.Problems {
				if strings.HasPrefix(line, "no sign-in") {
					problem = line
				}
			}
			if problem == "" {
				t.Fatalf("an entry nothing signs in raised no problem: %v", d.Problems)
			}
			for _, want := range tc.want {
				if !strings.Contains(problem, want) {
					t.Errorf("the problem does not name %q:\n%s", want, problem)
				}
			}
			for _, bad := range tc.notWant {
				if strings.Contains(problem, bad) {
					t.Errorf("the problem names %q, which this mode does not sign in with:\n%s", bad, problem)
				}
			}
		})
	}
}

// THE DOCTOR OFFERS NO ROUTE THROUGH A VARIABLE NO STORE CAN HOLD. overrides
// can set token_env or api_key_env to a name outside the reference grammar —
// MY-TOKEN, MY=KEY — which every write of the company document refuses, and
// which an entry loaded to run still carries. Nothing that keeps a credential
// can hold such a name, yet the doctor told a subscription entry on a machine
// with a personal login to mint a headless MY-TOKEN with `-capture-token`
// (preferred, even), a token that could be neither stored nor referenced, and
// which `crewlet llm login` refuses before it mints; and an api-key entry to
// store or export a key handed over under MY=KEY. Each is now a problem naming
// the field, and the routes leave the variable out: an inherit-env entry is
// told to export only the variable a shell can.
//
// The control is the same subscription entry with its token variable in the
// grammar, told to mint, so the missing advice is missing by the name.
// Not parallel: HOME is the fake human's, and inherit-env reads the process
// environment, emptied here of the variable it can forward.
//
// Mutation: let MintsHeadlessToken ignore the grammar, and the subscription
// case offers `-capture-token` again; give ModeSignIn's api-key branch no
// misnamed case, and that case is told `crewlet secrets set`; drop the field's
// problem, and every case misses the path.
func TestTheDoctorOffersNoRouteThroughAVariableNoStoreCanHold(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ANTHROPIC_API_KEY", "")
	path := filepath.Join(home, ".claude", ".credentials.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"token":"personal"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		mode      AuthMode
		overrides map[string]any
		field     string
		// want and notWant are judged on the "no sign-in" problem.
		want, notWant []string
	}{
		{
			name: "control: a token variable in the grammar", mode: AuthSubscription,
			overrides: map[string]any{"token_env": "MY_TOKEN"},
			want:      []string{"`crewlet llm login sub -from-host`", "MY_TOKEN with `-capture-token`"},
		},
		{
			name: "subscription", mode: AuthSubscription,
			overrides: map[string]any{"token_env": "MY-TOKEN"},
			field:     `providers.llm.sub.overrides.token_env is "MY-TOKEN"`,
			want:      []string{"`crewlet llm login sub -from-host`"},
			notWant:   []string{"capture-token", "MY-TOKEN"},
		},
		{
			name: "api-key", mode: AuthAPIKey,
			overrides: map[string]any{"api_key_env": "MY=KEY"},
			field:     `providers.llm.sub.overrides.api_key_env is "MY=KEY"`,
			want: []string{"auth.mode is api-key",
				"name the variable your build of the CLI reads in providers.llm.sub.overrides.api_key_env"},
			notWant: []string{"crewlet secrets set", "export it", "capture-token"},
		},
		{
			name: "inherit-env", mode: AuthInheritEnv,
			overrides: map[string]any{"token_env": "MY-TOKEN"},
			field:     `providers.llm.sub.overrides.token_env is "MY-TOKEN"`,
			want: []string{"auth.mode is inherit-env",
				`export ANTHROPIC_API_KEY in the engine's environment, since no shell can export "MY-TOKEN"`},
			notWant: []string{"capture-token", "export one in"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Cleanup(func() { forgetWorkspace(dir) })
			overrides := map[string]any{"binary": "crewlet-no-such-cli-binary"}
			maps.Copy(overrides, tc.overrides)
			p, err := New(Config{
				Key: "sub", Harness: "claude-code", StateDir: dir, Timeout: time.Second, MaxConcurrent: 1,
				Overrides: overrides, Auth: Auth{Mode: tc.mode},
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			d := p.Diagnose(t.Context(), DiagnoseOptions{})
			if len(d.HostLogin) == 0 {
				t.Fatal("the premise: the host login was not found")
			}
			var problem string
			for _, line := range d.Problems {
				if strings.HasPrefix(line, "no sign-in") {
					problem = line
				}
			}
			if problem == "" {
				t.Fatalf("an entry nothing signs in raised no problem: %v", d.Problems)
			}
			for _, want := range tc.want {
				if !strings.Contains(problem, want) {
					t.Errorf("the problem does not name %q:\n%s", want, problem)
				}
			}
			for _, bad := range tc.notWant {
				if strings.Contains(problem, bad) {
					t.Errorf("the problem names %q, a route through a variable no store can hold:\n%s",
						bad, problem)
				}
			}
			joined := strings.Join(d.Problems, "\n")
			switch {
			case tc.field == "" && strings.Contains(joined, "not an environment variable name"):
				t.Errorf("a variable in the grammar was reported as outside it:\n%s", joined)
			case tc.field != "" && !strings.Contains(joined, tc.field):
				t.Errorf("no problem names %q:\n%s", tc.field, joined)
			}
		})
	}
}

// A budget built on estimates is a different promise from one built on the
// vendor's own counts, so the report must not let the difference pass
// silently.
func TestDoctorSaysWhenTokenCountsAreEstimated(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	t.Cleanup(func() { forgetWorkspace(dir) })
	p, err := New(Config{
		Key: "sub", Harness: "custom", StateDir: dir, Timeout: time.Second, MaxConcurrent: 1,
		Overrides: map[string]any{
			"binary": os.Args[0], "complete_args": []any{"-p"}, "output": "text",
			"model_args": []any{}, "credential_paths": []any{".fake/creds.json"},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// A login on disk, so the estimate is the only finding left.
	credentials := p.Workspace().CredentialsDir()
	if err := os.MkdirAll(credentials, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(credentials, "creds.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	d := p.Diagnose(t.Context(), DiagnoseOptions{Smoke: false})
	if !strings.Contains(d.TokenUsage, "estimated") {
		t.Errorf("TokenUsage = %q", d.TokenUsage)
	}
	if !strings.Contains(strings.Join(d.Problems, "\n"), "estimates") {
		t.Errorf("an estimating profile was reported without a problem: %v", d.Problems)
	}
}

// THE SMOKE TEST ASKS THE WAY A PHASE ASKS. It certifies that a call arrives
// when the conversation names the tool and nothing forces it — the shape every
// phase, and the tool loop's finishing corrective, relies on — with the tool
// served as a seat's are, on a host of its own, offering it and nothing else.
func TestTheSmokeTestAsksTheWayAPhaseAsks(t *testing.T) {
	t.Parallel()
	sessions, err := toolsession.NewHost()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sessions.Close() })
	host := newSmokeHost(sessions)
	req := smokeRequest(host)
	if !req.Agent {
		t.Error("the smoke test is not asked as a seat's call")
	}
	prompt, err := RenderPrompt(req)
	if err != nil {
		t.Fatalf("RenderPrompt: %v", err)
	}
	if strings.Contains(prompt, "MUST") {
		t.Errorf("the smoke prompt demands a call no phase demands:\n%s", prompt)
	}
	last := req.Messages[len(req.Messages)-1]
	if last.Role != llm.RoleUser || !strings.Contains(last.Content, smokeTool) {
		t.Errorf("the instruction does not name the tool: %+v", last)
	}
	offered := host.Offered()
	if len(offered) != 1 || offered[0].Def.Name != smokeTool {
		t.Errorf("tools = %+v, want %s offered alone", offered, smokeTool)
	}
}

// The report is what an operator reads before a deploy, so every line the
// docs promise has to be in it.
func TestTheReportCarriesEveryLineTheDocsPromise(t *testing.T) {
	t.Parallel()
	// The measured lines are the checks' details.
	d := Diagnosis{
		Provider: "subscription", Harness: "claude-code", Binary: "/usr/local/bin/claude",
		WrittenFor: "Claude Code CLI 2.x", StateDir: "/var/lib/crewlet/llm-harness/subscription",
		Credentials: "present", TokenEnv: "set",
		Checks: []Check{
			{Name: CheckBinary, OK: true, Detail: "/usr/local/bin/claude"},
			{Name: CheckVersion, OK: true, Detail: "2.0.31 (Claude Code)"},
			{Name: CheckSignIn, OK: true, Detail: "credential files in /var/lib"},
			{Name: CheckUsage, OK: true, Detail: "reported by CLI"},
			{Name: CheckSmoke, OK: true, Detail: "ok: 812 in / 34 out"},
		},
	}
	var out strings.Builder
	d.Render(&out)
	for _, want := range []string{
		"provider", "harness", "binary        : /usr/local/bin/claude", "version       : 2.0.31",
		"written for", "state dir", "credentials", "token env", "sign-in       : credential files",
		"token usage   : reported by CLI", "smoke test    : ok: 812", "problems",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the report is missing the %q line:\n%s", want, out.String())
		}
	}
	if !strings.Contains(out.String(), "problems      : none") {
		t.Errorf("a healthy report does not say so:\n%s", out.String())
	}
}

// A LOGGED-OUT ENTRY'S DOCTOR SAYS WHY ITS BUNDLE DID NOT RESTORE. `crewlet llm
// doctor` restores the configured bundle first, as the engine would, and after
// a logout that restore declines ([ErrLoggedOut]); the report has to say so,
// or "no credential files" on an entry configured with a bundle reads as a
// bundle that is broken.
//
// Mutation: drop the logout from the credentials line or the no-sign-in
// problem, and this is red.
func TestDoctorNamesTheLogoutThatKeepsABundleOut(t *testing.T) {
	t.Parallel()
	p := loginProvider(t)
	p.bundle = bundleOf(t, p.harness, "creds.json", `{"token":"k-bundled"}`)
	if err := p.RestoreConfiguredBundle(t.Context()); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if err := p.Logout(t.Context(), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	d := p.Diagnose(t.Context(), DiagnoseOptions{})
	if !strings.Contains(d.Credentials, "`crewlet llm logout`") || !strings.Contains(d.Credentials, "bundle") {
		t.Errorf("credentials = %q, want the logout that keeps the bundle out named", d.Credentials)
	}
	joined := strings.Join(d.Problems, "\n")
	// The way back is one this profile has: it declares no login command,
	// so `crewlet llm login sub` would refuse, and adopting a login is the
	// route that writes one.
	if !strings.Contains(joined, "removed its login") || !strings.Contains(joined, "`crewlet llm login sub -from-host`") {
		t.Errorf("the no-sign-in problem names neither the logout nor the way back:\n%s", joined)
	}
}

// THE DOCTOR'S PROBES ASK AS A SEAT ASKS. They prove what a seat runs under —
// that its CLI answers, that the shell is denied, that the web is reached — so
// each is sent as a seat's call is: an agent call with a system prompt, which
// takes the channel a seat's prompt takes. Sent with none, most profiles
// passed no channel at all, the vendor's default prompt ran, and the report
// described a configuration no seat ever had.
//
// A profile with no tool route, so the smoke test's own call is the plain
// one it makes to learn whether the CLI answers — a completion like the two
// probes (smoke_test.go covers the route).
//
// Mutation: drop Agent from probeRequest, or its system message, and the
// probes leave the appending channel and this is red.
func TestTheDoctorsProbesAskAsASeatAsks(t *testing.T) {
	t.Parallel()
	rec := t.TempDir()
	p := fakeProvider(t, map[string]string{
		"FAKE_STDOUT":             "ok",
		"FAKE_SHELL_PROBE_STDOUT": noLocalToolsReply,
		"FAKE_WEB_PROBE_STDOUT":   "ts=" + nowEpoch(),
		"FAKE_RECORD_ARGV":        rec,
	}, map[string]any{
		"local_tools":          string(harnessprofile.LocalToolsDenied),
		"version_args":         []any{"-test.run=NoSuchTest"},
		"system_prompt":        map[string]any{"args": []any{"--replace-system-prompt", "{system}"}},
		"append_system_prompt": map[string]any{"args": []any{"--append-system-prompt", "{system}"}},
	})
	d := p.Diagnose(t.Context(), DiagnoseOptions{Smoke: true})
	// The web line leads with the entry's stance, then the probe's verdict.
	if !strings.HasPrefix(d.Web, "allowed; ok") {
		t.Fatalf("web = %q — the fake did not answer the probe it was asked", d.Web)
	}
	var probes [][]string
	for _, argv := range recordedArgv(t, rec) {
		// The version probe runs the binary too, with argv of its own.
		if len(argv) > 0 && argv[0] == "-test.run=TestFakeHarness" {
			probes = append(probes, argv)
		}
	}
	if len(probes) != 4 {
		t.Fatalf("recorded %d completions, want the smoke test and the three isolation "+
			"probes (shell, file read, web): %q", len(probes), probes)
	}
	for _, argv := range probes {
		system, ok := valueAfter(argv, "--append-system-prompt")
		if !ok {
			t.Errorf("argv = %q — a probe did not take the channel a seat's prompt takes", argv)
			continue
		}
		if !strings.HasPrefix(system, harnessNote) || !strings.HasSuffix(system, "\n\n"+probeSystemPrompt) {
			t.Errorf("a probe carried %q, want the probe's system prompt under the harness note", system)
		}
		if slices.Contains(argv, "--replace-system-prompt") {
			t.Errorf("argv = %q — a probe replaced the CLI's prompt, which no seat does here", argv)
		}
	}
}

// THE REPORT SAYS WHICH PROMPT A SEAT RUNS UNDER, and which a side pass does:
// beside the CLI's own, in place of it, or in the transcript under it, naming
// the profile field an operator would override — so the vendor prompt a seat
// answers under is read off the report rather than out of the profile.
func TestTheReportSaysWhichPromptASeatRunsUnder(t *testing.T) {
	t.Parallel()
	replace := map[string]any{"args": []any{"--replace-system-prompt", "{system}"}}
	appendTo := map[string]any{"args": []any{"--append-system-prompt", "{system}"}}
	for name, tc := range map[string]struct {
		overrides   map[string]any
		seats, side []string // substrings of each line
	}{
		"both channels": {
			overrides: map[string]any{"system_prompt": replace, "append_system_prompt": appendTo},
			seats:     []string{"added to the CLI's own", "(append_system_prompt)"},
			side:      []string{"replaces the CLI's own", "(system_prompt)"},
		},
		"only the replacing one": {
			overrides: map[string]any{"system_prompt": replace},
			seats:     []string{"replaces the CLI's own", "(system_prompt)"},
			side:      []string{"replaces the CLI's own", "(system_prompt)"},
		},
		"neither": {
			overrides: nil,
			seats:     []string{"in the transcript, under the CLI's own"},
			side:      []string{"in the transcript, under the CLI's own"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := fakeProvider(t, map[string]string{"FAKE_STDOUT": "fake-cli 1.0.0"}, tc.overrides)
			d := p.Diagnose(t.Context(), DiagnoseOptions{Smoke: false})
			if len(d.SystemPrompt) != 2 {
				t.Fatalf("SystemPrompt = %q, want a line for seats and one for side passes", d.SystemPrompt)
			}
			for i, want := range []struct {
				prefix string
				parts  []string
			}{{"seats: ", tc.seats}, {"side passes: ", tc.side}} {
				line := d.SystemPrompt[i]
				if !strings.HasPrefix(line, want.prefix) {
					t.Errorf("line %d = %q, want it to start %q", i, line, want.prefix)
				}
				for _, part := range want.parts {
					if !strings.Contains(line, part) {
						t.Errorf("line %d = %q, want %q in it", i, line, part)
					}
				}
			}
		})
	}

	// Rendered under one label, the second line continuing the first.
	d := Diagnosis{SystemPrompt: []string{"seats: A", "side passes: B"}}
	var out strings.Builder
	d.Render(&out)
	if want := "system prompt : seats: A\n" + strings.Repeat(" ", 14) + ": side passes: B\n"; !strings.Contains(out.String(), want) {
		t.Errorf("the report does not carry the two lines under \"system prompt\":\n%s", out.String())
	}
}
