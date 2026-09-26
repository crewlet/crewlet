package codingagent_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/codingagent"
)

// cliOf reaches the CLI under a runner, for the pure command and parse tests.
func claude() codingagent.CLI   { return codingagent.ClaudeCode{} }
func opencode() codingagent.CLI { return codingagent.OpenCode{} }

// ---------------------------------------------------------------------
// claude-code: the invocation
// ---------------------------------------------------------------------

func TestTheClaudeInvocationIsHeadlessAndMachineReadable(t *testing.T) {
	cmd := claude().Command(sandbox.RunRequest{Brief: "fix the flake"}, codingagent.Paths{}, "")
	for _, want := range []string{
		"claude -p ",                          // headless
		"--output-format json",                // parseable
		"--permission-mode bypassPermissions", // a prompt would hang a headless run
		"'fix the flake'",                     // the brief, quoted
	} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("the invocation is missing %q:\n%s", want, cmd)
		}
	}
}

// A minimal call stays minimal: an unset cap must not become a flag.
func TestUnsetClaudeLimitsAppendNoFlags(t *testing.T) {
	cmd := claude().Command(sandbox.RunRequest{Brief: "x"}, codingagent.Paths{}, "")
	for _, absent := range []string{"--max-turns", "--max-budget-usd", "--model", "--mcp-config"} {
		if strings.Contains(cmd, absent) {
			t.Fatalf("%q was added for an unset value:\n%s", absent, cmd)
		}
	}
}

func TestSetClaudeLimitsBecomeFlags(t *testing.T) {
	cmd := claude().Command(sandbox.RunRequest{
		Brief:  "x",
		Limits: sandbox.Limits{MaxTurns: 30, MaxBudgetUSD: 2.5},
		LLM:    &sandbox.AgentLLM{Model: "claude-opus-5"},
	}, codingagent.Paths{}, "/box/.crewlet/mcp.json")
	for _, want := range []string{
		"--max-turns 30", "--max-budget-usd 2.5", "--model 'claude-opus-5'",
		"--mcp-config '/box/.crewlet/mcp.json'",
		// Without strict mode the CLI ALSO loads whatever config the box's
		// home carries, which on a reused box is the previous run's.
		"--strict-mcp-config",
	} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("the invocation is missing %q:\n%s", want, cmd)
		}
	}
}

// The brief is operator- and model-authored text; it must not be able to end
// the quoting and append a command.
func TestABriefCannotEscapeItsQuoting(t *testing.T) {
	cmd := claude().Command(sandbox.RunRequest{
		Brief: `fix it'; rm -rf / #`,
	}, codingagent.Paths{}, "")
	if strings.Contains(cmd, "; rm -rf / #") && !strings.Contains(cmd, `'\''`) {
		t.Fatalf("the brief escaped its quoting:\n%s", cmd)
	}
}

// ---------------------------------------------------------------------
// claude-code: the parser
// ---------------------------------------------------------------------

func TestTheClaudeParserReadsTheEnvelope(t *testing.T) {
	res := claude().Parse(`{
		"result": "Fixed it. Opened https://github.com/acme/api/pull/7",
		"subtype": "success",
		"is_error": false,
		"session_id": "sess-1",
		"total_cost_usd": 0.42,
		"usage": {"input_tokens": 1200, "output_tokens": 300}
	}`)
	if !res.Success {
		t.Fatalf("a clean run read as failed: %+v", res)
	}
	if res.SessionID != "sess-1" || res.CostUSD != 0.42 {
		t.Fatalf("envelope fields lost: %+v", res)
	}
	if res.InputTokens != 1200 || res.OutputTokens != 300 {
		t.Fatalf("usage lost: %+v", res)
	}
	if len(res.DeliveredRefs) != 1 {
		t.Fatalf("the pull request was not scraped: %v", res.DeliveredRefs)
	}
}

// THE PER-MODEL ACCOUNT IS THE WHOLE RUN. `modelUsage` covers every model call
// the run made — its subagents and its own internal calls included — and its
// input is counted with what the cache served and wrote, as the engine counts
// its own completions: a cached run billed as its uncached remainder is billed
// a sliver of what it spent.
func TestTheClaudeParserReadsThePerModelAccount(t *testing.T) {
	res := claude().Parse(`{"type":"result","subtype":"success","is_error":false,
		"result":"done","total_cost_usd":0.9,
		"usage":{"input_tokens":2,"cache_creation_input_tokens":10,"cache_read_input_tokens":20,"output_tokens":5},
		"modelUsage":{
			"claude-sonnet":{"inputTokens":100,"outputTokens":40,"cacheReadInputTokens":800,
				"cacheCreationInputTokens":60,"webSearchRequests":0,"costUSD":0.75},
			"claude-haiku":{"inputTokens":30,"outputTokens":6,"cacheReadInputTokens":0,
				"cacheCreationInputTokens":0,"webSearchRequests":0,"costUSD":0.15}}}`)
	if !res.UsageWhole {
		t.Fatal("a per-model account was not reported as the whole run")
	}
	want := []types.ModelSpend{
		{Model: "claude-haiku", InputTokens: 30, OutputTokens: 6, CostUSD: 0.15},
		{Model: "claude-sonnet", InputTokens: 960, OutputTokens: 40, CostUSD: 0.75},
	}
	if !slices.Equal(res.Models, want) {
		t.Errorf("models = %+v, want %+v", res.Models, want)
	}
	if res.InputTokens != 990 || res.OutputTokens != 46 || res.CostUSD != 0.9 {
		t.Errorf("totals = %d/%d/$%v, want the split's 990/46/$0.90",
			res.InputTokens, res.OutputTokens, res.CostUSD)
	}
}

// WITHOUT A PER-MODEL ACCOUNT, THE MAIN LOOP IS A FLOOR. `usage` counts the
// run's main loop and none of its subagents, so it is reported — cache
// included — and the run says the figures are not its whole spend. The sample
// is a real answer's usage, whose uncached remainder is two tokens of 42,823.
func TestTheClaudeMainLoopUsageIsAFloor(t *testing.T) {
	res := claude().Parse(`{"is_error":false,"subtype":"success","result":"PONG",
		"total_cost_usd":0.0373944,
		"usage":{"input_tokens":2,"cache_creation_input_tokens":7319,
		"cache_read_input_tokens":35502,"output_tokens":5,"service_tier":"standard"}}`)
	if res.InputTokens != 42823 || res.OutputTokens != 5 {
		t.Errorf("tokens = %d/%d, want 42823/5 — the cache is input the run was billed for",
			res.InputTokens, res.OutputTokens)
	}
	if res.UsageWhole {
		t.Error("the main loop's usage was reported as the whole run")
	}
	if res.CostUSD != 0.0373944 {
		t.Errorf("cost = %v, want the envelope's total", res.CostUSD)
	}
}

// A run that hit its turn cap sets no is_error but did not finish.
func TestARunThatHitItsCapIsNotASuccess(t *testing.T) {
	res := claude().Parse(`{"result":"ran out of turns","subtype":"error_max_turns","is_error":false}`)
	if res.Success {
		t.Fatal("a run that exhausted its turns read as success")
	}
	if res.Error == "" {
		t.Fatal("the failure says nothing")
	}
}

// The CLI sometimes prints a banner before its JSON, and the object is always
// last because it is what the CLI prints when it is done.
func TestTheClaudeParserFindsTheEnvelopeAfterABanner(t *testing.T) {
	res := claude().Parse("Welcome to Claude Code\nchecking for updates…\n" +
		`{"result":"done","subtype":"success"}`)
	if !res.Success || res.Text != "done" {
		t.Fatalf("the envelope after a banner was missed: %+v", res)
	}
}

// A coding agent that crashed should surface as "did not deliver" and let the
// turn continue, not blow the turn up.
func TestUnparseableOutputIsAFailedResultNotAnError(t *testing.T) {
	res := claude().Parse("Segmentation fault (core dumped)")
	if res.Success {
		t.Fatal("a crash read as success")
	}
	if !strings.Contains(res.Text, "Segmentation fault") {
		t.Fatalf("the raw output was lost: %+v", res)
	}
	if res.Error == "" {
		t.Fatal("the failure says nothing")
	}
}

func TestEmptyClaudeOutputIsAFailure(t *testing.T) {
	if res := claude().Parse("   "); res.Success || res.Error == "" {
		t.Fatalf("empty output = %+v", res)
	}
}

// claude-code exits cleanly, so the done marker is the signal.
func TestClaudeRelisOnTheDoneMarker(t *testing.T) {
	if claude().Finished(`{"result":"done","subtype":"success"}`) {
		t.Fatal("claude-code claimed a streamed terminal signal it does not emit")
	}
}

// ---------------------------------------------------------------------
// opencode: the invocation
// ---------------------------------------------------------------------

// --format json is load-bearing: `opencode run` finishes and hangs, and in
// default mode the final summary is buffered and lost.
func TestTheOpenCodeInvocationAlwaysStreamsJson(t *testing.T) {
	cmd := opencode().Command(sandbox.RunRequest{Brief: "fix it"}, codingagent.Paths{}, "")
	if !strings.Contains(cmd, "--format json") {
		t.Fatalf("the invocation does not stream JSON:\n%s", cmd)
	}
	if !strings.HasPrefix(cmd, "opencode run ") {
		t.Fatalf("not a non-interactive run:\n%s", cmd)
	}
}

// A custom base URL means the run's own declared provider; otherwise the model
// is addressed under its vendor family.
func TestTheOpenCodeModelIsAddressedUnderTheRightProvider(t *testing.T) {
	cases := []struct {
		llm  sandbox.AgentLLM
		want string
	}{
		{sandbox.AgentLLM{Model: "claude-opus-5", ProviderType: "anthropic"}, "anthropic/claude-opus-5"},
		{sandbox.AgentLLM{Model: "gpt-5", ProviderType: "openai"}, "openai/gpt-5"},
		{sandbox.AgentLLM{Model: "gemini-3", ProviderType: "google"}, "google/gemini-3"},
		// A gateway: the catalogue cannot resolve it, so the run declares
		// its own provider and addresses the model there.
		{sandbox.AgentLLM{Model: "house-model", ProviderType: "openai", BaseURL: "https://example.com/v1"},
			codingagent.OpenCodeProviderID + "/house-model"},
	}
	for _, c := range cases {
		cmd := opencode().Command(sandbox.RunRequest{Brief: "x", LLM: &c.llm}, codingagent.Paths{}, "")
		if !strings.Contains(cmd, "--model '"+c.want+"'") {
			t.Fatalf("%+v gave:\n%s\nwant --model %q", c.llm, cmd, c.want)
		}
	}
}

// A subscription entry's provider type is the same for every vendor, so
// reading it would address a Claude subscription's model as an OpenAI one —
// which is why the family comes from the declared type, not from the entry.
func TestAnUnknownProviderTypeFallsBackToTheOpenAiFamily(t *testing.T) {
	cmd := opencode().Command(sandbox.RunRequest{
		Brief: "x", LLM: &sandbox.AgentLLM{Model: "m", ProviderType: "something-new"},
	}, codingagent.Paths{}, "")
	if !strings.Contains(cmd, "--model 'openai/m'") {
		t.Fatalf("cmd = %s", cmd)
	}
}

// ---------------------------------------------------------------------
// opencode: the config
// ---------------------------------------------------------------------

// The secret is never written into a file inside the box where the agent
// could read it back and echo it into its report.
func TestTheOpenCodeConfigReferencesTheKeyRatherThanInliningIt(t *testing.T) {
	b := sandbox.NewFakeSandbox("box-1")
	path, err := opencode().WriteConfig(t.Context(), b, sandbox.RunRequest{
		LLM: &sandbox.AgentLLM{
			Model: "house-model", ProviderType: "anthropic",
			BaseURL: "https://example.com/v1",
		},
	}, codingagent.PathsFor(b))
	if err != nil || path == "" {
		t.Fatalf("WriteConfig = %q, %v", path, err)
	}
	blob, err := b.ReadFile(t.Context(), path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(blob)
	if !strings.Contains(body, "{env:ANTHROPIC_API_KEY}") {
		t.Fatalf("the key is not referenced through the environment:\n%s", body)
	}
	if !strings.Contains(body, "https://example.com/v1") {
		t.Fatalf("the endpoint was not declared:\n%s", body)
	}
	if !strings.Contains(body, `"share": "disabled"`) {
		t.Fatalf("sharing was left on — a run's transcript is company work:\n%s", body)
	}
}

func TestTheOpenCodeConfigTranslatesBothMcpTransports(t *testing.T) {
	b := sandbox.NewFakeSandbox("box-1")
	path, err := opencode().WriteConfig(t.Context(), b, sandbox.RunRequest{
		MCPServers: map[string]sandbox.MCPServer{
			"files": {
				Name: "files", Transport: sandbox.TransportStdio,
				Command: "mcp-files", Args: []string{"--root", "/src"},
				Env: map[string]string{"K": "v"},
			},
			"linear": {
				Name: "linear", Transport: sandbox.TransportHTTP,
				URL: "https://example.com/mcp", Headers: map[string]string{"H": "v"},
			},
		},
	}, codingagent.PathsFor(b))
	if err != nil || path == "" {
		t.Fatalf("WriteConfig = %q, %v", path, err)
	}
	blob, _ := b.ReadFile(t.Context(), path)
	var cfg struct {
		MCP map[string]struct {
			Type        string            `json:"type"`
			Command     []string          `json:"command"`
			Environment map[string]string `json:"environment"`
			URL         string            `json:"url"`
			Headers     map[string]string `json:"headers"`
			Enabled     bool              `json:"enabled"`
		} `json:"mcp"`
	}
	if err := json.Unmarshal(blob, &cfg); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, blob)
	}
	// ONE ARGV: this CLI takes the whole invocation as a single array, so
	// the binary and its arguments are joined rather than kept apart.
	got := cfg.MCP["files"]
	if got.Type != "local" || !got.Enabled ||
		!slices.Equal(got.Command, []string{"mcp-files", "--root", "/src"}) {
		t.Fatalf("stdio server = %+v", got)
	}
	// The CREDENTIAL reaches the box under this CLI's own key. It is
	// `environment` here and `env` in Claude Code's file, which is the
	// whole reason each runner renders its own.
	if got.Environment["K"] != "v" {
		t.Errorf("stdio env = %v, want the seat's credential under `environment`",
			got.Environment)
	}
	got = cfg.MCP["linear"]
	if got.Type != "remote" || got.URL != "https://example.com/mcp" || !got.Enabled {
		t.Fatalf("http server = %+v", got)
	}
	if got.Headers["H"] != "v" {
		t.Errorf("http headers = %v, want the seat's credential", got.Headers)
	}
}

// The other runner's vocabulary, from the same typed input — which is the
// point of RenderMCP answering in server shape rather than in one CLI's keys.
func TestTheClaudeCodeConfigTranslatesBothMcpTransports(t *testing.T) {
	b := sandbox.NewFakeSandbox("box-1")
	path, err := codingagent.ClaudeCode{}.WriteConfig(t.Context(), b, sandbox.RunRequest{
		MCPServers: map[string]sandbox.MCPServer{
			"files": {
				Name: "files", Transport: sandbox.TransportStdio,
				Command: "mcp-files", Args: []string{"--root", "/src"},
				Env: map[string]string{"K": "v"},
			},
			"linear": {
				Name: "linear", Transport: sandbox.TransportHTTP,
				URL: "https://example.com/mcp", Headers: map[string]string{"H": "v"},
			},
		},
	}, codingagent.PathsFor(b))
	if err != nil || path == "" {
		t.Fatalf("WriteConfig = %q, %v", path, err)
	}
	blob, _ := b.ReadFile(t.Context(), path)
	var cfg struct {
		Servers map[string]struct {
			Type    string            `json:"type"`
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(blob, &cfg); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, blob)
	}
	// COMMAND AND ARGS STAY APART here, where OpenCode joins them.
	got := cfg.Servers["files"]
	if got.Command != "mcp-files" || !slices.Equal(got.Args, []string{"--root", "/src"}) {
		t.Fatalf("stdio server = %+v", got)
	}
	if got.Env["K"] != "v" {
		t.Errorf("stdio env = %v, want the seat's credential under `env`", got.Env)
	}
	got = cfg.Servers["linear"]
	if got.Type != "http" || got.URL != "https://example.com/mcp" {
		t.Fatalf("http server = %+v", got)
	}
	if got.Headers["H"] != "v" {
		t.Errorf("http headers = %v, want the seat's credential", got.Headers)
	}
	// A stdio server carries NO url or headers and an http one no command:
	// a transport's own fields only.
	if got.Command != "" || len(got.Env) > 0 {
		t.Errorf("the http server carried stdio fields: %+v", got)
	}
}

// A headless run has nobody to answer a permission prompt, so a gate left at
// `ask` is refused and the agent carries on without the tool — which reads as
// an agent that would not do the work. The box is the boundary instead, the
// same stance claude-code takes with --permission-mode bypassPermissions.
func TestTheOpenCodeConfigLeavesNoPermissionGateForNobodyToAnswer(t *testing.T) {
	b := sandbox.NewFakeSandbox("box-1")
	path, err := opencode().WriteConfig(t.Context(), b, sandbox.RunRequest{
		MCPServers: map[string]sandbox.MCPServer{
			"crewlet": {Name: "crewlet", Transport: sandbox.TransportHTTP, URL: "https://example.com/mcp"},
		},
	}, codingagent.PathsFor(b))
	if err != nil || path == "" {
		t.Fatalf("WriteConfig = %q, %v", path, err)
	}
	blob, _ := b.ReadFile(t.Context(), path)
	// BOTH ARE POINTERS, so an ABSENT key is distinguishable from one written
	// false. A plain bool decodes a missing `autoupdate` to false and passes —
	// which is the exact regression this pins, since the CLI then takes its own
	// default and nothing says so.
	var cfg struct {
		Permission *string `json:"permission"`
		Autoupdate *bool   `json:"autoupdate"`
	}
	if err := json.Unmarshal(blob, &cfg); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, blob)
	}
	if cfg.Permission == nil || *cfg.Permission != "allow" {
		t.Errorf("permission = %v, want \"allow\" — a gate nobody can answer is a lost tool call:\n%s",
			derefOr(cfg.Permission, "(absent)"), blob)
	}
	// A CLI that updates itself mid-run swaps the tool under the brief while
	// the seat's turn is suspended waiting on it.
	if cfg.Autoupdate == nil || *cfg.Autoupdate {
		t.Errorf("autoupdate = %v, want a written false:\n%s",
			derefOr(cfg.Autoupdate, "(absent)"), blob)
	}
}

// derefOr renders a pointer field for a failure message, naming an absent key
// as absent rather than as its zero value — which is the distinction the
// assertions above turn on.
func derefOr[T any](p *T, absent string) any {
	if p == nil {
		return absent
	}
	return *p
}

// The config carries the run's POSTURE, not just its provider and servers, and
// this CLI reads it from the checkout rather than from a flag — so a run that
// wrote none would not fall back to the vendor's defaults. It would inherit
// whatever the previous run left on a reused box, MCP block and dead per-run
// bridge URL included.
func TestAnOpenCodeConfigIsWrittenEvenWithNoProviderAndNoServers(t *testing.T) {
	b := sandbox.NewFakeSandbox("box-1")
	path, err := opencode().WriteConfig(t.Context(), b, sandbox.RunRequest{}, codingagent.PathsFor(b))
	if err != nil || path == "" {
		t.Fatalf("WriteConfig = %q, %v; want the posture written anyway", path, err)
	}
	blob, _ := b.ReadFile(t.Context(), path)
	var cfg map[string]any
	if err := json.Unmarshal(blob, &cfg); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, blob)
	}
	if cfg["permission"] != "allow" || cfg["share"] != "disabled" {
		t.Errorf("a minimal run lost its posture:\n%s", blob)
	}
	// And it states ONLY the posture: a provider block for a run that named
	// no endpoint would point the agent at nothing.
	if _, ok := cfg["provider"]; ok {
		t.Errorf("a run with no base URL declared a provider:\n%s", blob)
	}
	if _, ok := cfg["mcp"]; ok {
		t.Errorf("a run with no servers declared an mcp block:\n%s", blob)
	}
}

// ---------------------------------------------------------------------
// opencode: completion and parsing
// ---------------------------------------------------------------------

// The shape has moved across versions and a box runs whatever the operator's
// image has, so all three terminal signals must be recognised.
func TestOpenCodeRecognisesEveryTerminalSignal(t *testing.T) {
	cases := map[string]string{
		"step_finish stop":   `{"type":"step_finish","part":{"reason":"stop"}}`,
		"session idle":       `{"type":"session.status","properties":{"status":{"type":"idle"}}}`,
		"an error is an end": `{"type":"error","error":"boom"}`,
	}
	for name, line := range cases {
		if !opencode().Finished(line) {
			t.Fatalf("%s was not recognised as terminal", name)
		}
	}
}

// An intermediate step carries "tool-calls" and must not end the run.
func TestAnIntermediateStepIsNotATerminalSignal(t *testing.T) {
	stream := `{"type":"step_finish","part":{"reason":"tool-calls"}}
{"type":"text","part":{"text":"still working"}}`
	if opencode().Finished(stream) {
		t.Fatal("an intermediate step ended the run early")
	}
}

// The stream is flushed live, so a poll routinely reads it mid-line.
func TestAPartialLineIsSkippedRatherThanFailingThePoll(t *testing.T) {
	stream := `{"type":"text","part":{"text":"working"}}
{"type":"step_fin`
	if opencode().Finished(stream) {
		t.Fatal("a half-written line was read as terminal")
	}
	res := opencode().Parse(stream)
	if !strings.Contains(res.Text, "working") {
		t.Fatalf("the complete events were lost with the partial one: %+v", res)
	}
}

// The answer is what the agent SAID: its text events, in order. A tool call is
// what it did, and is not part of the answer.
func TestTheOpenCodeParserRebuildsTheAnswer(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"tool_use","part":{"tool":"bash","state":{"input":{"command":"pytest -q"},"status":"completed"}}}`,
		`{"type":"text","part":{"text":"Fixed the flake."}}`,
		`{"type":"text","part":{"text":"Opened https://github.com/acme/api/pull/9"}}`,
		`{"type":"step_finish","part":{"reason":"stop"}}`,
	}, "\n")
	res := opencode().Parse(stream)
	if !res.Success {
		t.Fatalf("a clean stream read as failed: %+v", res)
	}
	if res.Text != "Fixed the flake.\nOpened https://github.com/acme/api/pull/9" {
		t.Fatalf("the answer was not rebuilt from the text events alone: %q", res.Text)
	}
	if len(res.DeliveredRefs) != 1 {
		t.Fatalf("refs = %v", res.DeliveredRefs)
	}
}

// An invented token count in the spend rollup is worse than a missing one,
// because a reader cannot tell it is invented — and a missing one must SAY it
// is missing, or it reads as a run that cost nothing.
func TestOpenCodeReportsNoTokensRatherThanEstimatingThem(t *testing.T) {
	res := opencode().Parse(`{"type":"text","part":{"text":"done"}}`)
	if res.InputTokens != 0 || res.OutputTokens != 0 || res.CostUSD != 0 {
		t.Fatalf("tokens were invented: %+v", res)
	}
	if res.UsageWhole {
		t.Fatal("a stream that reported no step's spend claims to account for the whole run")
	}
}

// EACH STEP REPORTS ITS OWN SPEND, and a step that carries the provider's own
// total is counted exactly: its input with what the cache served and wrote,
// and its output as the rest of the total — reasoning included, which a
// release that reports `output` without it would otherwise leave out. A step
// reported twice is billed once.
func TestOpenCodeCountsEveryStepsSpendOnce(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"step_finish","part":{"id":"p1","reason":"tool-calls","cost":0.25,` +
			`"tokens":{"total":1100,"input":100,"output":40,"reasoning":10,"cache":{"read":900,"write":50}}}}`,
		`{"type":"step_finish","part":{"id":"p1","reason":"tool-calls","cost":0.25,` +
			`"tokens":{"total":1100,"input":100,"output":40,"reasoning":10,"cache":{"read":900,"write":50}}}}`,
		`{"type":"text","part":{"text":"done"}}`,
		`{"type":"step_finish","part":{"id":"p2","reason":"stop","cost":0.5,` +
			`"tokens":{"total":230,"input":200,"output":30,"reasoning":0,"cache":{"read":0,"write":0}}}}`,
	}, "\n")
	res := opencode().Parse(stream)
	if res.InputTokens != 1250 || res.OutputTokens != 80 {
		t.Errorf("tokens = %d/%d, want 1250/80 — each step once, cache counted as input, "+
			"reasoning inside the output", res.InputTokens, res.OutputTokens)
	}
	if res.CostUSD != 0.75 {
		t.Errorf("cost = %v, want the two steps' 0.75", res.CostUSD)
	}
	if !res.UsageWhole {
		t.Error("a run whose every step carried its total is not reported whole")
	}
}

// A STEP WITHOUT A TOTAL IS A FLOOR. What `input` and `output` include has
// moved across OpenCode releases, and those two alone overstate nothing in any
// of them — so they are counted, and the run says its figures are not the
// whole.
func TestAnOpenCodeStepWithoutATotalIsCountedAsAFloor(t *testing.T) {
	res := opencode().Parse(strings.Join([]string{
		`{"type":"step_finish","part":{"id":"p1","reason":"stop","cost":0.1,` +
			`"tokens":{"input":300,"output":20,"reasoning":5,"cache":{"read":700,"write":0}}}}`,
		`{"type":"text","part":{"text":"done"}}`,
	}, "\n"))
	if res.InputTokens != 300 || res.OutputTokens != 20 {
		t.Errorf("tokens = %d/%d, want the step's own 300/20", res.InputTokens, res.OutputTokens)
	}
	if res.UsageWhole {
		t.Error("a step with no total was reported as the run's whole spend")
	}
}

func TestAnErrorEventIsAFailure(t *testing.T) {
	res := opencode().Parse(`{"type":"error","error":"model refused"}`)
	if res.Success {
		t.Fatal("an error event read as success")
	}
	if !strings.Contains(res.Error, "model refused") {
		t.Fatalf("error = %q", res.Error)
	}
}

// An older CLI, or a run captured before the format flag.
func TestNonJsonOpenCodeOutputIsTakenAsTheAnswer(t *testing.T) {
	res := opencode().Parse("I fixed it and opened https://github.com/acme/api/pull/3")
	if !res.Success || !strings.Contains(res.Text, "fixed it") {
		t.Fatalf("plain output = %+v", res)
	}
	if len(res.DeliveredRefs) != 1 {
		t.Fatalf("refs = %v", res.DeliveredRefs)
	}
}

// A tool-only run leaves no assistant text, which is exactly the case the
// findings file exists for — but the parser must still report it honestly.
func TestAToolOnlyStreamReportsNoAnswer(t *testing.T) {
	res := opencode().Parse(`{"type":"tool_use","part":{"tool":"bash","state":{"input":{"command":"ls"}}}}`)
	if res.Success {
		t.Fatal("a run that said nothing read as success")
	}
	if res.Error == "" {
		t.Fatal("the failure says nothing")
	}
}
