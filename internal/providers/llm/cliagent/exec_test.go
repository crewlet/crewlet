package cliagent

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm"
)

// helperEnv marks a re-execution of this test binary as the fake CLI.
//
// The fake is this binary rather than a shell script because the engine ships
// for Windows too, and a suite that proved the exec path only on Unix would
// leave the platform where process handling differs most untested.
const helperEnv = "CREWLET_CLIAGENT_FAKE"

// TestCLIAgentFakeCLI is not a test: it is the fake CLI the exec tests drive.
// It exits before the testing framework prints anything, so the parent reads
// exactly the bytes the case asked for.
func TestCLIAgentFakeCLI(t *testing.T) {
	if os.Getenv(helperEnv) != "1" {
		t.Skip("this is the fake CLI, driven by the exec tests")
	}
	if ms := os.Getenv("FAKE_SLEEP_MS"); ms != "" {
		delay, _ := strconv.Atoi(ms)
		time.Sleep(time.Duration(delay) * time.Millisecond)
	}
	switch {
	case os.Getenv("FAKE_STUBBORN") == "1":
		// A CLI that ignores the polite signal and leaves a forking
		// descendant behind — a Node or Bun runtime under a launcher, which
		// is what every coding CLI in this package actually is. It announces
		// the grandchild's pid so the parent can watch for its death, then
		// holds its own process open for ever.
		//
		// The whole tree is signalled through the process GROUP, so an
		// engine that only signals the process it started leaves this
		// grandchild holding the seat's workspace and sockets.
		fakeStubborn()
	case os.Getenv("FAKE_GRANDCHILD") == "1":
		fakeGrandchild()
	case os.Getenv("FAKE_DUMP_ENV") == "1":
		env := os.Environ()
		slices.Sort(env)
		fmt.Print(strings.Join(env, "\n"))
	case os.Getenv("FAKE_ECHO_STDIN") == "1":
		// Base64 rather than verbatim: the prompt CONTAINS the response
		// contract's own fenced example, so echoing it raw would be
		// parsed as an envelope and the test would assert against the
		// example instead of the prompt.
		prompt, _ := io.ReadAll(os.Stdin)
		fmt.Print(base64.StdEncoding.EncodeToString(prompt))
	default:
		fmt.Print(os.Getenv("FAKE_STDOUT"))
	}
	if s := os.Getenv("FAKE_STDERR"); s != "" {
		fmt.Fprint(os.Stderr, s)
	}
	code, _ := strconv.Atoi(os.Getenv("FAKE_EXIT"))
	os.Exit(code)
}

// fakeProvider builds a provider whose CLI is this test binary.
func fakeProvider(t *testing.T, env map[string]string, overrides map[string]any) *Provider {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() { forgetWorkspace(dir) })

	base := map[string]any{
		"binary":        os.Args[0],
		"complete_args": []any{"-test.run=TestCLIAgentFakeCLI"},
		"model_args":    []any{},
		"output":        "text",
		"prompt_mode":   "stdin",
	}
	for k, v := range overrides {
		base[k] = v
	}
	child := map[string]string{helperEnv: "1"}
	for k, v := range env {
		child[k] = v
	}
	p, err := New(Config{
		Key: "sub", Agent: "custom", StateDir: dir, Overrides: base,
		Timeout: 20 * time.Second, MaxConcurrent: 2, Env: child,
		Auth: Auth{Mode: AuthSubscription},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func ask(t *testing.T, p *Provider, req llm.Request) (*llm.Completion, error) {
	t.Helper()
	if len(req.Messages) == 0 {
		req.Messages = []llm.Message{{Role: llm.RoleUser, Content: "hello"}}
	}
	return p.Complete(t.Context(), req)
}

// The whole point of the backend: a CLI's prose comes back as a completion.
func TestAPlainReplyBecomesACompletion(t *testing.T) {
	p := fakeProvider(t, map[string]string{"FAKE_STDOUT": "the answer"}, nil)
	comp, err := ask(t, p, llm.Request{})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if comp.Content != "the answer" {
		t.Errorf("Content = %q", comp.Content)
	}
	if comp.Model != p.Model() {
		t.Errorf("Model = %q, want the configured %q", comp.Model, p.Model())
	}
	if comp.InputTokens == 0 || comp.OutputTokens == 0 {
		t.Errorf("a CLI reporting no usage must still be estimated, got in=%d out=%d",
			comp.InputTokens, comp.OutputTokens)
	}
}

// The tool channel rides the prompt, and the reply parses back into real tool
// calls — otherwise the turn engine sees prose and never runs a tool.
func TestAnEnvelopeReplyBecomesToolCalls(t *testing.T) {
	reply := "```json\n{\"message\":\"reading\",\"tool_calls\":" +
		"[{\"name\":\"read_file\",\"arguments\":{\"path\":\"/etc/hostname\"}}]}\n```"
	p := fakeProvider(t, map[string]string{"FAKE_STDOUT": reply}, nil)
	comp, err := ask(t, p, llm.Request{
		Tools: []llm.ToolDef{{Name: "read_file", Description: "read a file"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(comp.ToolCalls) != 1 || comp.ToolCalls[0].Name != "read_file" {
		t.Fatalf("tool calls = %+v", comp.ToolCalls)
	}
	if comp.ToolCalls[0].ID == "" {
		t.Error("a tool call with no id cannot be paired with its result")
	}
	if comp.FinishReason != "tool_calls" {
		t.Errorf("FinishReason = %q, want tool_calls", comp.FinishReason)
	}
	if comp.Content != "reading" {
		t.Errorf("Content = %q", comp.Content)
	}
}

// A spent subscription arrives as prose on a SUCCESSFUL exit. Classifying it
// RATE_LIMIT is what carries the role onto its metered fallback for the rest
// of the window and back again afterwards, with no operator intervention.
func TestASpentSubscriptionIsRateLimitedWithItsOwnResetTime(t *testing.T) {
	reset := time.Now().Add(37 * time.Minute).Unix()
	p := fakeProvider(t, map[string]string{
		"FAKE_STDOUT": fmt.Sprintf("Claude AI usage limit reached|%d", reset),
	}, map[string]any{
		"limit_markers": []any{map[string]any{
			"sentinel": "Claude AI usage limit reached", "reset_separator": "|", "reset_unit": "epoch",
		}},
	})
	_, err := ask(t, p, llm.Request{})
	if err == nil {
		t.Fatal("a spent subscription was returned as a successful completion")
	}
	if got := llm.KindOf(err); got != llm.KindRateLimit {
		t.Fatalf("kind = %v, want rate_limit", got)
	}
	var classified *llm.Error
	if !errors.As(err, &classified) {
		t.Fatalf("error is not classified: %v", err)
	}
	if classified.RetryAfter < 30*time.Minute || classified.RetryAfter > 40*time.Minute {
		t.Errorf("RetryAfter = %v, want the vendor's own reset instant", classified.RetryAfter)
	}
}

// Structure, not keywords. A model ANSWERING a question about usage limits
// writes the phrase itself, and throwing that answer away as a spent plan is
// the bug this classification is shaped to avoid.
func TestAModelWritingAboutUsageLimitsIsNotASpentSubscription(t *testing.T) {
	p := fakeProvider(t, map[string]string{
		"FAKE_STDOUT": "A rate limit is when the usage limit for your plan is reached.",
	}, map[string]any{
		"limit_markers": []any{map[string]any{
			"sentinel": "Claude AI usage limit reached", "reset_separator": "|", "reset_unit": "epoch",
		}},
	})
	comp, err := ask(t, p, llm.Request{})
	if err != nil {
		t.Fatalf("an answer about rate limits was classified as one: %v", err)
	}
	if !strings.Contains(comp.Content, "rate limit") {
		t.Errorf("Content = %q", comp.Content)
	}
}

// An expired login must classify AUTH, which is retryable — so the chain
// keeps the seat working off a metered key while the operator re-logs in.
func TestAnExpiredLoginIsClassifiedAuth(t *testing.T) {
	p := fakeProvider(t, map[string]string{
		"FAKE_STDERR": "Error: OAuth token has expired\n", "FAKE_EXIT": "1",
	}, map[string]any{
		"auth_markers": []any{map[string]any{"sentinel": "OAuth token has expired"}},
	})
	_, err := ask(t, p, llm.Request{})
	if got := llm.KindOf(err); got != llm.KindAuth {
		t.Fatalf("kind = %v, want auth (err %v)", got, err)
	}
	if !llm.KindOf(err).Retryable() {
		t.Error("an expired login must be retryable across the chain")
	}
}

// A wall-clock breach reports TIMEOUT and returns at the deadline, not when
// the child eventually finishes: the cap exists so a wedged CLI cannot hold a
// seat's concurrency slot.
func TestTheWallClockCapEndsACall(t *testing.T) {
	p := fakeProvider(t, map[string]string{"FAKE_SLEEP_MS": "60000"}, nil)
	p.timeout = 300 * time.Millisecond

	started := time.Now()
	_, err := ask(t, p, llm.Request{})
	elapsed := time.Since(started)

	if got := llm.KindOf(err); got != llm.KindTimeout {
		t.Fatalf("kind = %v, want timeout (err %v)", got, err)
	}
	if elapsed > 20*time.Second {
		t.Fatalf("the call took %v — the cap did not end it", elapsed)
	}
	if !strings.Contains(err.Error(), "timeout_seconds") {
		t.Errorf("the timeout error does not say which knob to raise: %v", err)
	}
}

// Exit zero with nothing on stdout is a failed call, but not a FATAL one:
// nothing about the request was refused, so the chain must be free to try
// another member and the credential must not be cooled.
func TestAnEmptyAnswerIsRetryableRatherThanFatal(t *testing.T) {
	p := fakeProvider(t, map[string]string{"FAKE_STDOUT": ""}, nil)
	_, err := ask(t, p, llm.Request{})
	if got := llm.KindOf(err); got != llm.KindServer {
		t.Fatalf("kind = %v, want server", got)
	}
	if llm.KindOf(err).ExhaustsCredential() {
		t.Error("an empty answer must not cool the credential")
	}
}

// The environment is an ALLOWLIST. A child that inherited os.Environ() would
// hand every seat the org's chat token and its database DSN, and would
// silently bill a metered key that happened to be exported.
func TestTheChildEnvironmentIsAnAllowlist(t *testing.T) {
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-not-for-the-child")
	t.Setenv("CREWLET_STORE_DSN", "file:/var/lib/crewlet/state.db")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-should-not-be-billed")

	p := fakeProvider(t, map[string]string{"FAKE_DUMP_ENV": "1"}, nil)
	comp, err := ask(t, p, llm.Request{})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	for _, leaked := range []string{"SLACK_BOT_TOKEN", "CREWLET_STORE_DSN", "ANTHROPIC_API_KEY"} {
		if strings.Contains(comp.Content, leaked+"=") {
			t.Errorf("%s reached the child:\n%s", leaked, comp.Content)
		}
	}
	if !strings.Contains(comp.Content, "PATH=") {
		t.Error("PATH did not reach the child, so the CLI cannot find its own helpers")
	}
	home := envValue(comp.Content, "HOME")
	if !strings.Contains(home, filepath.Join("seats")) {
		t.Errorf("HOME = %q, want a path inside the seat's own home", home)
	}
	if envValue(comp.Content, "XDG_CONFIG_HOME") == "" {
		t.Error("XDG_CONFIG_HOME was not redirected, so the CLI reads the engine user's config")
	}
}

// The default mode must never let a metered key through, even one an operator
// put in cli.env — that is the flat-rate-plan-billed-anyway failure the mode
// exists to prevent.
func TestSubscriptionModeStripsAMeteredKey(t *testing.T) {
	p := fakeProvider(t,
		map[string]string{"FAKE_DUMP_ENV": "1", "ANTHROPIC_API_KEY": "sk-ant-oops"},
		map[string]any{"api_key_env": "ANTHROPIC_API_KEY", "token_env": "CLAUDE_CODE_OAUTH_TOKEN"})
	comp, err := ask(t, p, llm.Request{})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if strings.Contains(comp.Content, "ANTHROPIC_API_KEY=") {
		t.Errorf("a metered key survived subscription mode:\n%s", comp.Content)
	}
}

// api-key mode is the deliberate opposite, and must actually deliver the key
// or the mode does nothing.
func TestAPIKeyModeDeliversTheKey(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(func() { forgetWorkspace(dir) })
	p, err := New(Config{
		Key: "metered", Agent: "custom", StateDir: dir,
		Overrides: map[string]any{
			"binary": os.Args[0], "complete_args": []any{"-test.run=TestCLIAgentFakeCLI"},
			"model_args": []any{}, "output": "text", "api_key_env": "VENDOR_API_KEY",
		},
		Timeout: 20 * time.Second, MaxConcurrent: 1,
		Env:  map[string]string{helperEnv: "1", "FAKE_DUMP_ENV": "1"},
		Auth: Auth{Mode: AuthAPIKey, APIKey: "sk-metered"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	comp, err := ask(t, p, llm.Request{})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if envValue(comp.Content, "VENDOR_API_KEY") != "sk-metered" {
		t.Errorf("api-key mode did not deliver the key:\n%s", comp.Content)
	}
}

// Each call runs in an EMPTY per-call directory, so a CLI that reads
// AGENTS.md or CLAUDE.md from cwd finds nothing from anyone else.
func TestTheWorkingDirectoryIsEmptyAndPerCall(t *testing.T) {
	p := fakeProvider(t, map[string]string{"FAKE_DUMP_ENV": "1"}, nil)
	comp, err := ask(t, p, llm.Request{})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	dir := envValue(comp.Content, "PWD")
	if dir == "" {
		// Not every platform exports PWD to a child; the directory is
		// asserted through the workspace suite instead.
		t.Skip("this platform does not export PWD to a child process")
	}
	entries, err := os.ReadDir(dir)
	if err == nil && len(entries) != 0 {
		t.Errorf("the working directory %q was not empty: %v", dir, entries)
	}
}

// The prompt must actually reach the CLI, and the transcript must carry the
// tool catalogue and the response contract or the model has no protocol.
func TestThePromptReachesTheCLIWithItsContract(t *testing.T) {
	p := fakeProvider(t, map[string]string{"FAKE_ECHO_STDIN": "1"}, nil)
	comp, err := ask(t, p, llm.Request{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: "you are Dev"},
			{Role: llm.RoleUser, Content: "read the file"},
		},
		Tools:      []llm.ToolDef{{Name: "read_file", Description: "read a file"}},
		ToolChoice: "required",
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	prompt := decodePrompt(t, comp.Content)
	for _, want := range []string{"## system", "you are Dev", "## user", "read the file",
		"Available tools", "read_file", "Response contract", "MUST"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the prompt is missing %q:\n%s", want, prompt)
		}
	}
}

// A call with NO tools gets no contract: auxiliary work sends a plain prompt
// and reads a plain answer, with no envelope to get wrong.
func TestACallWithNoToolsGetsNoContract(t *testing.T) {
	p := fakeProvider(t, map[string]string{"FAKE_ECHO_STDIN": "1"}, nil)
	comp, err := ask(t, p, llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "summarise this"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	prompt := decodePrompt(t, comp.Content)
	if strings.Contains(prompt, "Response contract") {
		t.Errorf("a tool-free call carried the envelope contract:\n%s", prompt)
	}
	if !strings.Contains(prompt, "summarise this") {
		t.Errorf("the prompt did not reach the CLI:\n%s", prompt)
	}
}

// The cap is what keeps a fleet of seats entering Plan together from
// exhausting the engine host — each CLI is a full runtime at 200-400 MB.
func TestConcurrencyIsCapped(t *testing.T) {
	p := fakeProvider(t, map[string]string{"FAKE_STDOUT": "ok", "FAKE_SLEEP_MS": "250"}, nil)
	if cap(p.slots) != 2 {
		t.Fatalf("slots = %d, want the configured 2", cap(p.slots))
	}
	started := time.Now()
	done := make(chan error, 4)
	for i := range 4 {
		go func() {
			ctx := llm.WithSeat(t.Context(), fmt.Sprintf("seat-%d", i))
			_, err := p.Complete(ctx, llm.Request{
				Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
			})
			done <- err
		}()
	}
	for range 4 {
		if err := <-done; err != nil {
			t.Fatalf("Complete: %v", err)
		}
	}
	// Four calls of 250 ms through two slots cannot finish in one batch.
	if elapsed := time.Since(started); elapsed < 400*time.Millisecond {
		t.Errorf("four calls through two slots took %v — the cap is not holding", elapsed)
	}
}

// A caller that gave up while queueing must not go on to launch a process
// nobody is waiting for.
func TestACancelledCallerDoesNotLaunchAProcess(t *testing.T) {
	p := fakeProvider(t, map[string]string{"FAKE_STDOUT": "ok"}, nil)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	// Fill the slots so the cancelled caller queues.
	p.slots <- struct{}{}
	p.slots <- struct{}{}
	_, err := p.Complete(ctx, llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}}})
	if got := llm.KindOf(err); got != llm.KindTimeout {
		t.Fatalf("kind = %v, want timeout (err %v)", got, err)
	}
}

// decodePrompt reads back a prompt the fake CLI echoed.
func decodePrompt(t *testing.T, encoded string) string {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		t.Fatalf("the fake CLI did not echo a prompt: %v (%q)", err, encoded)
	}
	return string(raw)
}

// envValue reads one variable out of a dumped environment.
func envValue(dump, name string) string {
	for line := range strings.SplitSeq(dump, "\n") {
		if after, ok := strings.CutPrefix(line, name+"="); ok {
			return after
		}
	}
	return ""
}
