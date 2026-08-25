package cliagent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The CLI runs on the ENGINE host. A provider whose binary is missing there
// must say exactly that, because the config looks correct and the failure
// otherwise surfaces as an unexplained turn failure much later.
func TestDoctorReportsAMissingBinary(t *testing.T) {
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
	d := p.Diagnose(t.Context(), true)
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
	if d.Smoke != "skipped — no binary to run" {
		t.Errorf("Smoke = %q, want a skip rather than a second failure", d.Smoke)
	}
}

// "No login" on a machine where the CLI plainly works must explain itself, or
// an operator goes looking for a bug that is a missing --from-host.
func TestDoctorNamesAnUnadoptedHostLogin(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(func() { forgetWorkspace(dir) })
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeHostLogin(t, home, `{"token":"personal"}`)

	p, err := New(Config{
		Key: "sub", Agent: "custom", StateDir: dir, Timeout: time.Second, MaxConcurrent: 1,
		Overrides: map[string]any{
			"binary": os.Args[0], "complete_args": []any{"-p"}, "output": "text",
			"model_args": []any{}, "version_args": []any{"-test.run=NoSuchTest"},
			"credential_paths":      []any{".fake/creds.json"},
			"host_credential_paths": []any{".fake/creds.json"},
			"token_env":             "FAKE_OAUTH_TOKEN",
			"capture_token_args":    []any{"setup-token"},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d := p.Diagnose(t.Context(), false)
	if d.Credentials != "none on disk" {
		t.Fatalf("Credentials = %q", d.Credentials)
	}
	if len(d.HostLogin) == 0 {
		t.Fatal("the host login was not found, so the report cannot explain itself")
	}
	joined := strings.Join(d.Problems, "\n")
	for _, want := range []string{"--from-host", "--capture-token", "FAKE_OAUTH_TOKEN"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the problem does not offer %q:\n%s", want, joined)
		}
	}
}

// A budget built on estimates is a different promise from one built on the
// vendor's own counts, so the report must not let the difference pass
// silently.
func TestDoctorSaysWhenTokenCountsAreEstimated(t *testing.T) {
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
	// A login on disk, so the estimate is the only finding left.
	credentials := p.Workspace().CredentialsDir()
	if err := os.MkdirAll(credentials, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(credentials, "creds.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	d := p.Diagnose(t.Context(), false)
	if !strings.Contains(d.TokenUsage, "estimated") {
		t.Errorf("TokenUsage = %q", d.TokenUsage)
	}
	if !strings.Contains(strings.Join(d.Problems, "\n"), "estimates") {
		t.Errorf("an estimating profile was reported without a problem: %v", d.Problems)
	}
}

// The one failure nothing else catches: the CLI answers, and answers with
// PROSE. A seat on such a provider burns a corrective round every single
// turn, and the config looks perfect.
func TestTheSmokeTestCatchesACLIThatCannotProduceAToolCall(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(func() { forgetWorkspace(dir) })
	p, err := New(Config{
		Key: "sub", Agent: "custom", StateDir: dir, Timeout: 20 * time.Second, MaxConcurrent: 1,
		Overrides: map[string]any{
			"binary": os.Args[0], "complete_args": []any{"-test.run=TestCLIAgentFakeCLI"},
			"model_args": []any{}, "output": "text",
		},
		Env: map[string]string{helperEnv: "1", "FAKE_STDOUT": "I would read the file."},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d := p.Diagnose(t.Context(), true)
	if !strings.HasPrefix(d.Smoke, "failed") {
		t.Fatalf("Smoke = %q, want a failure", d.Smoke)
	}
	if !strings.Contains(d.Smoke, "corrective round") {
		t.Errorf("the smoke failure does not say what it costs: %q", d.Smoke)
	}
	if d.Healthy() {
		t.Error("a provider that cannot produce a tool call was reported healthy")
	}
}

// And it passes on one that can, or the check is a permanent red light.
func TestTheSmokeTestPassesOnAWorkingEnvelope(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(func() { forgetWorkspace(dir) })
	reply := "```json\n{\"message\":\"\",\"tool_calls\":" +
		"[{\"name\":\"crewlet_smoke\",\"arguments\":{\"ok\":true}}]}\n```"
	p, err := New(Config{
		Key: "sub", Agent: "custom", StateDir: dir, Timeout: 20 * time.Second, MaxConcurrent: 1,
		Overrides: map[string]any{
			"binary": os.Args[0], "complete_args": []any{"-test.run=TestCLIAgentFakeCLI"},
			"model_args": []any{}, "output": "text",
			"usage": map[string]any{"input": []any{[]any{"in"}}},
		},
		Env: map[string]string{helperEnv: "1", "FAKE_STDOUT": reply},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d := p.Diagnose(t.Context(), true)
	if !strings.HasPrefix(d.Smoke, "ok") {
		t.Fatalf("Smoke = %q", d.Smoke)
	}
}

// The report is what an operator reads before a deploy, so every line the
// docs promise has to be in it.
func TestTheReportCarriesEveryLineTheDocsPromise(t *testing.T) {
	t.Parallel()
	d := Diagnosis{
		Provider: "subscription", Agent: "claude-code", Binary: "/usr/local/bin/claude",
		BinaryPath: "/usr/local/bin/claude", Version: "2.0.31 (Claude Code)",
		WrittenFor: "Claude Code CLI 2.x", StateDir: "/var/lib/crewlet/llm-cli/subscription",
		Credentials: "present", TokenEnv: "set", TokenUsage: "reported by CLI",
		Smoke: "ok — 812 in / 34 out",
	}
	var out strings.Builder
	d.Render(&out)
	for _, want := range []string{
		"provider", "cli agent", "binary", "version", "written for",
		"state dir", "credentials", "token env", "token usage", "smoke test", "problems",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the report is missing the %q line:\n%s", want, out.String())
		}
	}
	if !strings.Contains(out.String(), "problems      : none") {
		t.Errorf("a healthy report does not say so:\n%s", out.String())
	}
}
