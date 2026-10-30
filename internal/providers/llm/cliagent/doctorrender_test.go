package cliagent

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm/cliagent/cliprofile"
)

// renderCase is one entry the doctor reports on, as the doctor's own tests
// build it, and the strings in its report that name this run's directories
// rather than anything the report decides.
type renderCase struct {
	name  string
	build func(t *testing.T) (*Provider, DiagnoseOptions, map[string]string)
}

// THE TEXT REPORT IS WHAT IT WAS BEFORE THE CHECKS EXISTED. `crewlet llm doctor`
// prints from [Diagnosis.Checks] now, the structure the API answers with, and
// an operator's deploy script and their eyes read the text: the move from
// fields to checks had to leave every line, its label, its order and its
// words as they were, and every problem's wording. The golden files under
// testdata/doctor-render were rendered by the field-based report before the
// checks existed, over the cases the doctor's own tests build, with this run's
// directories and the profile's written_for as placeholders.
//
// ONE CHANGE, ON PURPOSE: the problems are listed in the order of the checks
// that raise them, which is the order of the report's lines. The field-based
// report listed the executor's first and "no sign-in" last, in the order its
// code happened to reach them; a person reading "no sign-in" after the smoke
// failure it causes reads the effect before the cause. The goldens of the six
// cases that moved differ from the old report by that order alone, which was
// checked item by item when they were written.
//
// Mutation: print the web line before the local tools line, list a check's
// problems out of check order, or drop one from Problems, and a case goes red.
func TestTheStructuredChecksRenderTheReportOperatorsAlreadyRead(t *testing.T) {
	for _, tc := range doctorRenderCases() {
		t.Run(tc.name, func(t *testing.T) {
			p, opts, placeholders := tc.build(t)
			var out bytes.Buffer
			p.Diagnose(t.Context(), opts).Render(&out)
			got := out.String()
			for path, placeholder := range placeholders {
				got = strings.ReplaceAll(got, path, placeholder)
			}
			golden := filepath.Join("testdata", "doctor-render", tc.name+".txt")
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("reading %s: %v", golden, err)
			}
			if got != string(want) {
				t.Errorf("the report changed.\n--- got\n%s\n--- want\n%s", got, want)
			}
		})
	}
}

// doctorRenderCases are the entries the doctor's tests diagnose, rebuilt so
// their reports can be compared whole.
func doctorRenderCases() []renderCase {
	// The profile's written_for too: it is the profile's sentence, updated
	// with every measured build, and not the report's.
	stateOf := func(p *Provider) map[string]string {
		places := map[string]string{p.ws.StateDir(): "<state>", os.Args[0]: "<binary>"}
		if p.profile.WrittenFor != "" {
			places[p.profile.WrittenFor] = "<written-for>"
		}
		return places
	}
	personalLogin := func(t *testing.T) string {
		home := t.TempDir()
		t.Setenv("HOME", home)
		path := filepath.Join(home, ".claude", ".credentials.json")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(`{"token":"personal"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		return home
	}
	claudeWithNoBinary := func(t *testing.T, mode AuthMode) *Provider {
		dir := t.TempDir()
		t.Cleanup(func() { forgetWorkspace(dir) })
		p, err := New(Config{
			Key: "sub", Agent: "claude-code", StateDir: dir, Timeout: time.Second, MaxConcurrent: 1,
			Overrides: map[string]any{"binary": "crewlet-no-such-cli-binary"},
			Auth:      Auth{Mode: mode},
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		return p
	}
	cases := []renderCase{
		{name: "missing-binary", build: func(t *testing.T) (*Provider, DiagnoseOptions, map[string]string) {
			dir := t.TempDir()
			t.Cleanup(func() { forgetWorkspace(dir) })
			p, err := New(Config{
				Key: "sub", Agent: "custom", StateDir: dir, Timeout: time.Second, MaxConcurrent: 1,
				Overrides: map[string]any{
					"binary": "crewlet-no-such-cli-binary", "complete_args": []any{"-p"},
					"output": "text", "model_args": []any{},
				},
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			return p, DiagnoseOptions{Smoke: true}, stateOf(p)
		}},
		{name: "unadopted-host-login", build: func(t *testing.T) (*Provider, DiagnoseOptions, map[string]string) {
			dir := t.TempDir()
			t.Cleanup(func() { forgetWorkspace(dir) })
			home := t.TempDir()
			t.Setenv("HOME", home)
			writeHostLogin(t, home, `{"token":"personal"}`)
			p, err := New(Config{
				Key: "sub", Agent: "custom", StateDir: dir, Timeout: time.Second, MaxConcurrent: 1,
				Overrides: map[string]any{
					"binary": os.Args[0], "complete_args": []any{"-p"}, "output": "text",
					"model_args": []any{}, "version_args": []any{"-test.run=TestCLIAgentFakeCLI"},
					"credential_paths":      []any{".fake/creds.json"},
					"host_credential_paths": []any{".fake/creds.json"},
					"token_env":             "FAKE_OAUTH_TOKEN",
					"capture_token_args":    []any{"setup-token"},
				},
				Env: fakeChildEnv(map[string]string{"FAKE_STDOUT": "fake-cli 1.0.0"}),
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			places := stateOf(p)
			places[home] = "<home>"
			return p, DiagnoseOptions{Smoke: false}, places
		}},
		{name: "estimated-usage", build: func(t *testing.T) (*Provider, DiagnoseOptions, map[string]string) {
			dir := t.TempDir()
			t.Cleanup(func() { forgetWorkspace(dir) })
			p, err := New(Config{
				Key: "sub", Agent: "custom", StateDir: dir, Timeout: time.Second, MaxConcurrent: 1,
				Overrides: map[string]any{
					"binary": os.Args[0], "complete_args": []any{"-p"}, "output": "text",
					"model_args": []any{}, "credential_paths": []any{".fake/creds.json"},
				},
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			credentials := p.Workspace().CredentialsDir()
			if err := os.MkdirAll(credentials, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(credentials, "creds.json"), []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
			return p, DiagnoseOptions{Smoke: false}, stateOf(p)
		}},
		{name: "logged-out-bundle", build: func(t *testing.T) (*Provider, DiagnoseOptions, map[string]string) {
			t.Setenv("HOME", t.TempDir())
			p := loginProvider(t)
			p.bundle = bundleOf(t, p.agent, "creds.json", `{"token":"k-bundled"}`)
			if err := p.RestoreConfiguredBundle(t.Context()); err != nil {
				t.Fatalf("restore: %v", err)
			}
			if err := p.Logout(t.Context(), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
				t.Fatalf("Logout: %v", err)
			}
			return p, DiagnoseOptions{}, stateOf(p)
		}},
		{name: "probes-refused", build: func(t *testing.T) (*Provider, DiagnoseOptions, map[string]string) {
			p := fakeProvider(t, map[string]string{
				"FAKE_STDOUT":             "ok",
				"FAKE_SHELL_PROBE_STDOUT": noLocalToolsReply,
				"FAKE_WEB_PROBE_STDOUT":   noWebReply,
			}, map[string]any{
				"local_tools":  string(cliprofile.LocalToolsDenied),
				"version_args": []any{"-test.run=NoSuchTest"},
			})
			return p, DiagnoseOptions{Smoke: true}, stateOf(p)
		}},
		{name: "shell-ran-web-failed", build: func(t *testing.T) (*Provider, DiagnoseOptions, map[string]string) {
			p := fakeProvider(t, map[string]string{
				"FAKE_STDOUT":             "fake-cli 1.0.0",
				"FAKE_SHELL_PROBE_STDOUT": "it is " + nowEpoch(),
				"FAKE_WEB_PROBE_STDOUT":   "The fetch was blocked by a proxy.",
			}, map[string]any{
				"local_tools":  string(cliprofile.LocalToolsDenied),
				"version_args": []any{"-test.run=TestCLIAgentFakeCLI"},
			})
			return p, DiagnoseOptions{Smoke: true}, stateOf(p)
		}},
		{name: "nothing-answers", build: func(t *testing.T) (*Provider, DiagnoseOptions, map[string]string) {
			p := fakeProvider(t, map[string]string{
				"FAKE_STDERR": "Error: not signed in\n", "FAKE_EXIT": "1",
			}, map[string]any{"version_args": []any{"-test.run=NoSuchTest"}})
			return p, DiagnoseOptions{Smoke: true}, stateOf(p)
		}},
		{name: "executor-without-bridge", build: func(t *testing.T) (*Provider, DiagnoseOptions, map[string]string) {
			p := fakeProvider(t, map[string]string{"FAKE_STDOUT": "fake-cli 1.0.0"},
				map[string]any{"version_args": []any{"-test.run=TestCLIAgentFakeCLI"}})
			return p, DiagnoseOptions{
				ExecutorSeats: []string{"swe", "qa"}, AgentRunners: []string{"custom"}, Cell: "e2b",
			}, stateOf(p)
		}},
		{name: "executor-no-runner", build: func(t *testing.T) (*Provider, DiagnoseOptions, map[string]string) {
			p := fakeProvider(t, map[string]string{"FAKE_STDOUT": "fake-cli 1.0.0"},
				map[string]any{"version_args": []any{"-test.run=TestCLIAgentFakeCLI"}})
			return p, DiagnoseOptions{
				ExecutorSeats: []string{"swe"}, AgentRunners: []string{"claude-code"}, Cell: "direct",
			}, stateOf(p)
		}},
	}
	for _, mode := range []AuthMode{AuthSubscription, AuthAPIKey, AuthInheritEnv} {
		cases = append(cases, renderCase{
			name: "routes-" + string(mode),
			build: func(t *testing.T) (*Provider, DiagnoseOptions, map[string]string) {
				home := personalLogin(t)
				t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
				t.Setenv("ANTHROPIC_API_KEY", "")
				p := claudeWithNoBinary(t, mode)
				places := stateOf(p)
				places[home] = "<home>"
				return p, DiagnoseOptions{}, places
			},
		})
	}
	cases = append(cases, renderCase{
		name: "misnamed-token-variable",
		build: func(t *testing.T) (*Provider, DiagnoseOptions, map[string]string) {
			home := personalLogin(t)
			dir := t.TempDir()
			t.Cleanup(func() { forgetWorkspace(dir) })
			p, err := New(Config{
				Key: "sub", Agent: "claude-code", StateDir: dir, Timeout: time.Second, MaxConcurrent: 1,
				Overrides: map[string]any{"binary": "crewlet-no-such-cli-binary", "token_env": "MY-TOKEN"},
				Auth:      Auth{Mode: AuthSubscription},
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			places := stateOf(p)
			places[home] = "<home>"
			return p, DiagnoseOptions{}, places
		},
	})
	return cases
}
