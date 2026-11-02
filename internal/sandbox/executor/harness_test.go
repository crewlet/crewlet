package executor_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// harnessOf reaches the harness under a runner, for the pure command and parse tests.
func claude() executor.Harness   { return executor.ClaudeCode{} }
func opencode() executor.Harness { return executor.OpenCode{} }

// commandOf is h's command for req, in the session h names for it as a
// run's start does ([executor.Harness.Session]).
func commandOf(h executor.Harness, req sandbox.RunRequest, paths executor.Paths, config string) string {
	return h.Command(req, h.Session(req), paths, config)
}

// ---------------------------------------------------------------------
// claude-code: the invocation
// ---------------------------------------------------------------------

func TestTheClaudeInvocationIsHeadlessAndMachineReadable(t *testing.T) {
	t.Parallel()
	cmd := scriptOf(t, commandOf(claude(), sandbox.RunRequest{Brief: "fix the flake"}, executor.Paths{}, ""))
	for _, want := range []string{
		"claude -p ", // headless
		// A STREAM, so a run says what it is doing while it does it; and
		// --verbose, without which print mode refuses stream-json.
		"--output-format stream-json --verbose",
		"--permission-mode bypassPermissions", // a prompt would hang a headless run
	} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("the invocation is missing %q:\n%s", want, cmd)
		}
	}
}

// A minimal call stays minimal: an unset cap must not become a flag.
func TestUnsetClaudeLimitsAppendNoFlags(t *testing.T) {
	t.Parallel()
	cmd := scriptOf(t, commandOf(claude(), sandbox.RunRequest{Brief: "x"}, executor.Paths{}, ""))
	for _, absent := range []string{"--max-turns", "--model", "--mcp-config", "--settings"} {
		if strings.Contains(cmd, absent) {
			t.Fatalf("%q was added for an unset value:\n%s", absent, cmd)
		}
	}
}

func TestSetClaudeLimitsBecomeFlags(t *testing.T) {
	t.Parallel()
	cmd := scriptOf(t, commandOf(claude(), sandbox.RunRequest{
		Brief:  "x",
		Limits: sandbox.Limits{MaxTurns: 30},
		Model:  "claude-opus-5",
	}, executor.Paths{}, "/box/.crewlet/mcp.json"))
	for _, want := range []string{
		"--max-turns 30", "--model 'claude-opus-5'",
		"--mcp-config '/box/.crewlet/mcp.json'",
	} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("the invocation is missing %q:\n%s", want, cmd)
		}
	}
}

// ---------------------------------------------------------------------
// claude-code: the parser
// ---------------------------------------------------------------------

func TestTheClaudeParserReadsTheEnvelope(t *testing.T) {
	t.Parallel()
	res := claude().Parse(`{
		"type": "result",
		"result": "Fixed it. Opened https://github.com/acme/api/pull/7",
		"subtype": "success",
		"is_error": false,
		"session_id": "sess-1",
		"total_cost_usd": 0.42,
		"num_turns": 23,
		"usage": {"input_tokens": 1200, "output_tokens": 300}
	}`)
	if !res.Success {
		t.Fatalf("a clean run read as failed: %+v", res)
	}
	if res.SessionID != "sess-1" || res.CostUSD != 0.42 {
		t.Fatalf("envelope fields lost: %+v", res)
	}
	// THE RUN'S OWN TURNS: the provider calls its loop made, which the
	// engine never saw and every spend figure counts the run's record by.
	if res.Turns != 23 {
		t.Fatalf("turns = %d, want the 23 the harness reported", res.Turns)
	}
	if res.InputTokens != 1200 || res.OutputTokens != 300 {
		t.Fatalf("usage lost: %+v", res)
	}
	if len(res.DeliveredRefs) != 1 {
		t.Fatalf("the pull request was not scraped: %v", res.DeliveredRefs)
	}
}

// THE PROMPT A RUN SENT IS ITS WHOLE PROMPT, cached share included.
//
// The vendor's input_tokens is only the UNCACHED remainder, and a run is
// almost entirely cached rounds — so reading it alone put a fraction of
// every run's prompt on the budget counter and the spend rollup. The cached
// share is carried beside it as a breakdown, which is the convention every
// engine provider reports in.
func TestTheClaudeParserCountsTheCachedPrompt(t *testing.T) {
	t.Parallel()
	res := claude().Parse(`{
		"type": "result", "result": "done", "subtype": "success",
		"usage": {"input_tokens": 40, "cache_read_input_tokens": 9000,
			"cache_creation_input_tokens": 1500, "output_tokens": 300}
	}`)
	if res.InputTokens != 10540 {
		t.Errorf("input = %d, want the whole prompt: 40 uncached + 9000 read + 1500 written", res.InputTokens)
	}
	if res.CacheReadTokens != 9000 || res.CacheWriteTokens != 1500 {
		t.Errorf("cache = %d read / %d written, want 9000 / 1500", res.CacheReadTokens, res.CacheWriteTokens)
	}
	if res.OutputTokens != 300 {
		t.Errorf("output = %d, want 300", res.OutputTokens)
	}
}

// A run that hit its turn cap sets no is_error but did not finish — and an
// error result carries no `result` text, so what it says is how it ended.
func TestARunThatHitItsCapIsNotASuccess(t *testing.T) {
	t.Parallel()
	res := claude().Parse(`{"type":"result","subtype":"error_max_turns","is_error":false,"num_turns":30}`)
	if res.Success {
		t.Fatal("a run that exhausted its turns read as success")
	}
	if res.Error != "error_max_turns" {
		t.Fatalf("the failure says %q; want how the run ended", res.Error)
	}
}

// A run whose model call failed is a SUCCESS subtype carrying is_error, and
// its result text is the failure.
func TestARunThatReportsAnErrorIsNotASuccess(t *testing.T) {
	t.Parallel()
	res := claude().Parse(`{"type":"result","subtype":"success","is_error":true,` +
		`"result":"API Error: 529 overloaded"}`)
	if res.Success || res.Error != "API Error: 529 overloaded" {
		t.Fatalf("Parse = success %v, error %q; want the failure it reported", res.Success, res.Error)
	}
}

// A harness that crashed should surface as "did not deliver" and let the
// turn continue, not blow the turn up.
func TestUnparseableOutputIsAFailedResultNotAnError(t *testing.T) {
	t.Parallel()
	res := claude().Parse("Segmentation fault (core dumped)")
	if res.Success {
		t.Fatal("a crash read as success")
	}
	if !strings.Contains(res.Error, "Segmentation fault") {
		t.Fatalf("the raw output was lost from the failure: %+v", res)
	}
}

func TestEmptyClaudeOutputIsAFailure(t *testing.T) {
	t.Parallel()
	if res := claude().Parse("   "); res.Success || res.Error == "" {
		t.Fatalf("empty output = %+v", res)
	}
}

// ---------------------------------------------------------------------
// opencode: the invocation
// ---------------------------------------------------------------------

// scriptOf is the script inside an OpenCode invocation: the command is
// `sh -c '<script>'`, quoted the way the runner quotes everything.
func scriptOf(t *testing.T, cmd string) string {
	t.Helper()
	quoted, ok := strings.CutPrefix(cmd, "sh -c '")
	if !ok || !strings.HasSuffix(quoted, "'") {
		t.Fatalf("the invocation is not one quoted script:\n%s", cmd)
	}
	return strings.ReplaceAll(strings.TrimSuffix(quoted, "'"), `'\''`, "'")
}

// THE RUN IS AGAINST A SERVER OF ITS OWN, streaming JSON, under a session it
// names — and the brief is NOT on its argv, which OpenCode re-quotes: it
// arrives on stdin.
func TestTheOpenCodeInvocationRunsAgainstItsOwnServer(t *testing.T) {
	t.Parallel()
	h := opencode()
	req := sandbox.RunRequest{Brief: `fix "zebra-17"`}
	session := h.Session(req)
	script := scriptOf(t, h.Command(req, session, executor.Paths{Home: "/box"}, ""))
	for _, want := range []string{
		"sid='" + session.ID + "' k=no",
		"opencode serve --hostname 127.0.0.1 --port 0",
		`opencode run --server "$url" --format json --title crewlet --session "$sid"`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the script lacks %q:\n%s", want, script)
		}
	}
	if !strings.HasPrefix(session.ID, "ses_") || session.Continues || session.Fresh != "" {
		t.Errorf("a fresh run's session is %+v, want a new ses_ id of its own", session)
	}
	if strings.Contains(script, "zebra-17") {
		t.Errorf("the brief is on the run's argv, which OpenCode re-quotes:\n%s", script)
	}
	if !h.Output(executor.Paths{Home: "/box"}).BriefOnStdin {
		t.Error("the brief does not reach the run on stdin")
	}
	// A fresh session every run: OpenCode CONTINUES a session it already
	// holds, and a reused box holds the last run's.
	if again := h.Session(req); again.ID == session.ID {
		t.Error("two runs were given one session")
	}
}

// AN OPENCODE ENTRY'S MODEL REACHES THE BOX AS WRITTEN.
//
// It is the string every run on the engine host on the same entry passes to `opencode run
// --model`, which OpenCode splits at its first slash into a provider and a
// model. The runner used to put a family in front of it, so an entry naming
// `openrouter/anthropic/claude-sonnet-5` ran its box on
// `anthropic/openrouter/anthropic/claude-sonnet-5` — a provider-qualified id
// broke the agent run, and a bare one broke every run on the engine host.
func TestAnOpenCodeEntrysModelIsPassedAsWritten(t *testing.T) {
	t.Parallel()
	for _, model := range []string{
		"anthropic/claude-sonnet-5",
		"openrouter/anthropic/claude-sonnet-5",
	} {
		script := scriptOf(t, commandOf(opencode(), sandbox.RunRequest{Brief: "x", Model: model}, executor.Paths{}, ""))
		if !strings.Contains(script, "--model '"+model+"'") {
			t.Errorf("%q gave:\n%s\nwant the model as written", model, script)
		}
	}
}

// ---------------------------------------------------------------------
// opencode: the config
// ---------------------------------------------------------------------

// openCodeConfig is opencode.json as a run writes it, decoded.
type openCodeConfig struct {
	Schema    string `json:"$schema"`
	Share     string `json:"share"`
	Update    string `json:"update"`
	Snapshots *bool  `json:"snapshots"`
	Rules     []struct {
		Action, Resource, Effect string
	} `json:"permissions"`
	Providers    map[string]any `json:"providers"`
	Instructions []string       `json:"instructions"`
	MCP          *struct {
		Servers map[string]struct {
			Type        string            `json:"type"`
			Command     []string          `json:"command"`
			Environment map[string]string `json:"environment"`
			URL         string            `json:"url"`
			Headers     map[string]string `json:"headers"`
			OAuth       *bool             `json:"oauth"`
			Codemode    *bool             `json:"codemode"`
		} `json:"servers"`
	} `json:"mcp"`
}

func writeOpenCodeConfig(t *testing.T, req sandbox.RunRequest) (openCodeConfig, string) {
	t.Helper()
	cfg, body, _ := writeOpenCodeConfigIn(t, req)
	return cfg, body
}

// writeOpenCodeConfigIn is [writeOpenCodeConfig], with the box it wrote to.
func writeOpenCodeConfigIn(t *testing.T, req sandbox.RunRequest) (openCodeConfig, string, *sandbox.FakeSandbox) {
	t.Helper()
	b := sandbox.NewFakeSandbox("box-1")
	path, err := opencode().WriteConfig(t.Context(), b, req, executor.PathsFor(b))
	if err != nil || path == "" {
		t.Fatalf("WriteConfig = %q, %v", path, err)
	}
	blob, err := b.ReadFile(t.Context(), path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var cfg openCodeConfig
	if err := json.Unmarshal(blob, &cfg); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, blob)
	}
	return cfg, string(blob), b
}

// openCodeValue is the value a `{file:<path>}` reference in a run's
// configuration names: the content of the file it names, which OpenCode reads
// in its place.
func openCodeValue(t *testing.T, b sandbox.Sandbox, ref string) string {
	t.Helper()
	path, ok := referencedPath(ref)
	if !ok {
		t.Fatalf("%q is not a reference to a file", ref)
	}
	value, err := b.ReadFile(t.Context(), path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(value)
}

// NO CREDENTIAL IS WRITTEN INTO THE BOX, where the agent could read it back
// and echo it into its report. The key rides the run env under the variable
// of the provider the model names, which OpenCode's own provider reads — so
// the config declares no provider at all, and a run naming a model is
// written exactly as one naming none.
func TestTheOpenCodeConfigCarriesNoCredential(t *testing.T) {
	t.Parallel()
	cfg, body := writeOpenCodeConfig(t, sandbox.RunRequest{
		Model: "anthropic/claude-sonnet-5",
		Env:   map[string]string{"ANTHROPIC_API_KEY": "sk-ant-secret-value"},
	})
	if strings.Contains(body, "sk-ant-secret-value") {
		t.Fatalf("the key was written into the box:\n%s", body)
	}
	if cfg.Providers != nil {
		t.Fatalf("the run declared a provider of its own, which would shadow the "+
			"one the model names and the key variable it reads:\n%s", body)
	}
}

// BOTH TRANSPORTS IN OPENCODE 2's SHAPE: a seat's servers left to Code Mode,
// OpenCode's own way of keeping a large tool list out of each request, the
// engine's own server offered directly — what a run reports and asks through
// is a call on every model — and no remote server starting an OAuth flow a
// headless run cannot complete.
//
// Mutation: write `codemode: false` on every server, as the runner used to, or
// on none, and this goes red.
func TestTheOpenCodeConfigTranslatesBothMcpTransports(t *testing.T) {
	t.Parallel()
	cfg, body, b := writeOpenCodeConfigIn(t, sandbox.RunRequest{
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
			"crewlet": {
				Name: "crewlet", Transport: sandbox.TransportHTTP,
				URL: "https://engine.example.com/mcp/tok", AlwaysLoaded: true,
			},
		},
	})
	if cfg.MCP == nil {
		t.Fatalf("no mcp.servers block:\n%s", body)
	}
	// ONE ARGV: this harness takes the whole invocation as a single array, so
	// the binary and its arguments are joined rather than kept apart.
	got := cfg.MCP.Servers["files"]
	if got.Type != "local" || !slices.Equal(got.Command, []string{"mcp-files", "--root", "/src"}) {
		t.Fatalf("stdio server = %+v", got)
	}
	// The CREDENTIAL reaches the box under this harness's own key. It is
	// `environment` here and `env` in Claude Code's file, which is the
	// whole reason each runner renders its own — here by a reference to the
	// file holding it, which OpenCode reads in its place.
	if got.Environment["K"] == "" || openCodeValue(t, b, got.Environment["K"]) != "v" {
		t.Errorf("stdio env = %v, want the seat's credential under `environment`", got.Environment)
	}
	if got.Codemode != nil {
		t.Errorf("a seat's stdio server is kept from Code Mode:\n%s", body)
	}
	got = cfg.MCP.Servers["linear"]
	if got.Type != "remote" || openCodeValue(t, b, got.URL) != "https://example.com/mcp" {
		t.Fatalf("http server = %+v", got)
	}
	if got.Headers["H"] == "" || openCodeValue(t, b, got.Headers["H"]) != "v" {
		t.Errorf("http headers = %v, want the seat's credential", got.Headers)
	}
	if got.OAuth == nil || *got.OAuth {
		t.Errorf("the remote server may start OAuth, which nobody can complete:\n%s", body)
	}
	if got.Codemode != nil {
		t.Errorf("a seat's remote server is kept from Code Mode:\n%s", body)
	}
	if own := cfg.MCP.Servers["crewlet"]; own.Codemode == nil || *own.Codemode {
		t.Errorf("the engine's own tools are behind Code Mode:\n%s", body)
	}
}

// The other runner's vocabulary, from the same typed input — which is the
// point of RenderMCP answering in server shape rather than in one harness's keys.
// The engine's own server is always loaded, and a seat's left to Claude Code's
// tool search.
func TestTheClaudeCodeConfigTranslatesBothMcpTransports(t *testing.T) {
	t.Parallel()
	b := sandbox.NewFakeSandbox("box-1")
	path, err := executor.ClaudeCode{}.WriteConfig(t.Context(), b, sandbox.RunRequest{
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
			"crewlet": {
				Name: "crewlet", Transport: sandbox.TransportHTTP,
				URL: "https://engine.example.com/mcp/tok", AlwaysLoaded: true,
			},
		},
	}, executor.PathsFor(b))
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
			// AlwaysLoad is Claude Code's exemption from its tool search.
			AlwaysLoad *bool `json:"alwaysLoad"`
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
	// Mutation: always load every server, or none, and this goes red.
	for name, server := range cfg.Servers {
		if loaded := server.AlwaysLoad != nil && *server.AlwaysLoad; loaded != (name == "crewlet") {
			t.Errorf("server %s always loaded = %v, want the engine's own alone", name, loaded)
		}
	}
}

// A headless run has nobody to answer a permission prompt, so a gate left at
// `ask` is refused and the agent carries on without the tool — which reads as
// an agent that would not do the work. The box is the boundary instead, the
// same stance claude-code takes with --permission-mode bypassPermissions.
// `question` alone is denied: it asks a person through a form a headless run
// cancels, and a run with something to ask asks through its bridge's tool.
func TestTheOpenCodeConfigLeavesNoPermissionGateForNobodyToAnswer(t *testing.T) {
	t.Parallel()
	cfg, body := writeOpenCodeConfig(t, sandbox.RunRequest{
		MCPServers: map[string]sandbox.MCPServer{
			"crewlet": {Name: "crewlet", Transport: sandbox.TransportHTTP, URL: "https://example.com/mcp"},
		},
	})
	// THE ORDER IS THE RULE: the last match wins, so the allow comes first
	// and the one denial after it.
	want := []string{"* * allow", "question * deny"}
	var got []string
	for _, r := range cfg.Rules {
		got = append(got, r.Action+" "+r.Resource+" "+r.Effect)
	}
	if !slices.Equal(got, want) {
		t.Errorf("permissions = %v, want %v — a gate nobody can answer is a lost tool call:\n%s",
			got, want, body)
	}
	// A harness that updates itself mid-run swaps the tool under the brief while
	// the seat's turn is suspended waiting on it.
	if cfg.Update != "disable" {
		t.Errorf("update = %q, want disable:\n%s", cfg.Update, body)
	}
	// A pointer, so an ABSENT key is told apart from one written false: the
	// harness's own default is on.
	if cfg.Snapshots == nil || *cfg.Snapshots {
		t.Errorf("snapshots = %v, want a written false:\n%s", derefOr(cfg.Snapshots, "(absent)"), body)
	}
	// 1.x's keys, which 2.0 translates rather than reads: none of them.
	for _, v1 := range []string{`"permission"`, `"autoupdate"`, `"provider"`, `"npm"`, `"enabled"`} {
		if strings.Contains(body, v1) {
			t.Errorf("the config carries 1.x's %s:\n%s", v1, body)
		}
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

// THE RUN'S TURN CAP REACHES OPENCODE, on the agent the run runs as. It is the
// one bound on an executor's work — Claude Code carries it as --max-turns — so
// a run on this harness that dropped it ran uncapped while the setting read as
// wired. Zero is uncapped, and states no cap at all: OpenCode refuses a step
// count below one.
func TestTheOpenCodeConfigCarriesTheRunsTurnCap(t *testing.T) {
	t.Parallel()
	type capped struct {
		DefaultAgent string `json:"default_agent"`
		Agents       map[string]struct {
			Steps *int `json:"steps"`
		} `json:"agents"`
	}
	read := func(body string) capped {
		var c capped
		if err := json.Unmarshal([]byte(body), &c); err != nil {
			t.Fatalf("unmarshal: %v\n%s", err, body)
		}
		return c
	}

	_, body := writeOpenCodeConfig(t, sandbox.RunRequest{Limits: sandbox.Limits{MaxTurns: 40}})
	got := read(body)
	agent, ok := got.Agents[got.DefaultAgent]
	if got.DefaultAgent == "" || !ok || agent.Steps == nil || *agent.Steps != 40 {
		t.Errorf("the run's agent %q carries steps %v, want the cap of 40 on the agent the "+
			"run runs as:\n%s", got.DefaultAgent, derefOr(agent.Steps, "(absent)"), body)
	}

	_, body = writeOpenCodeConfig(t, sandbox.RunRequest{})
	if got := read(body); len(got.Agents) != 0 {
		t.Errorf("an uncapped run stated a step count:\n%s", body)
	}
}

// The config carries the run's POSTURE, not just its servers, so it is written
// on every run: the run's script refuses to start a server it has no
// configuration for, and one handed none would run on whatever documents the
// image and the checkout hold.
func TestAnOpenCodeConfigIsWrittenEvenWithNoModelAndNoServers(t *testing.T) {
	t.Parallel()
	cfg, body := writeOpenCodeConfig(t, sandbox.RunRequest{})
	if len(cfg.Rules) == 0 || cfg.Share != "disabled" || cfg.Schema == "" {
		t.Errorf("a minimal run lost its posture:\n%s", body)
	}
	// And it states ONLY the posture: a provider block for a run that named
	// no model would point the agent at nothing.
	if cfg.Providers != nil {
		t.Errorf("a run with no model declared a provider:\n%s", body)
	}
	if cfg.MCP != nil {
		t.Errorf("a run with no servers declared an mcp block:\n%s", body)
	}
}

// ---------------------------------------------------------------------
// opencode: completion and parsing
// ---------------------------------------------------------------------

// The stream is flushed live, so a read routinely meets it mid-line.
func TestAPartialLineIsSkippedRatherThanFailingTheRead(t *testing.T) {
	t.Parallel()
	stream := `{"type":"text","part":{"text":"working"}}
{"type":"step_fin`
	res := opencode().Parse(stream)
	if !strings.Contains(res.Text, "working") {
		t.Fatalf("the complete events were lost with the partial one: %+v", res)
	}
}

func TestTheOpenCodeParserRebuildsTheAnswerAndTheTranscript(t *testing.T) {
	t.Parallel()
	stream := strings.Join([]string{
		`{"type":"tool_use","part":{"tool":"shell","state":{"input":{"command":"pytest -q"},"status":"completed"}}}`,
		`{"type":"text","part":{"text":"Fixed the flake."}}`,
		`{"type":"text","part":{"text":"Opened https://github.com/acme/api/pull/9"}}`,
		`{"type":"step_finish","part":{"reason":"stop"}}`,
	}, "\n")
	res := opencode().Parse(stream)
	if !res.Success {
		t.Fatalf("a clean stream read as failed: %+v", res)
	}
	if !strings.Contains(res.Text, "Fixed the flake.") || !strings.Contains(res.Text, "pull/9") {
		t.Fatalf("the answer was not rebuilt: %q", res.Text)
	}
	if len(res.DeliveredRefs) != 1 {
		t.Fatalf("refs = %v", res.DeliveredRefs)
	}
	// A bare "[tool] shell" is useless when a run fails — the transcript has
	// to say WHAT ran.
	if !strings.Contains(res.Transcript, "pytest -q") {
		t.Fatalf("the transcript does not say what ran:\n%s", res.Transcript)
	}
}

// THE ANSWER IS THE LAST MESSAGE'S TEXT. Each step of an OpenCode run is a
// message of its own, and a run handed a person's note after it had answered
// answers again in a later one — the stream prints nothing for the note, so it
// reads as two answers in a row (below in the shape OpenCode 2.0.32 prints, its
// other fields left out and its ids shortened). The result is the later answer
// alone, and the run's
// transcript keeps both. A message's text in two events is one answer.
//
// Mutation: join every text event, as the decoder did, and the superseded
// answer leads the result.
func TestTheAnswerIsTheLastMessagesText(t *testing.T) {
	t.Parallel()
	stream := strings.Join([]string{
		`{"type":"step_start","part":{"messageID":"msg_1","type":"step-start"}}`,
		`{"type":"tool_use","part":{"messageID":"msg_1","tool":"shell","state":{"input":{"command":"go test ./..."},"status":"completed"}}}`,
		`{"type":"text","part":{"messageID":"msg_1","type":"text","text":"Running the tests first."}}`,
		`{"type":"step_finish","part":{"messageID":"msg_1","type":"step-finish","reason":"tool-calls"}}`,
		`{"type":"text","part":{"messageID":"msg_2","type":"text","text":"answer-1: it targets main."}}`,
		`{"type":"step_finish","part":{"messageID":"msg_2","type":"step-finish","reason":"stop"}}`,
		`{"type":"text","part":{"messageID":"msg_3","type":"text","text":"answer-2: it targets release,"}}`,
		`{"type":"text","part":{"messageID":"msg_3","type":"text","text":"as the note asked. Opened https://github.com/acme/api/pull/9"}}`,
	}, "\n")
	res := opencode().Parse(stream)
	if want := "answer-2: it targets release,\nas the note asked. Opened https://github.com/acme/api/pull/9"; res.Text != want {
		t.Errorf("the answer is %q, want the last message's text %q", res.Text, want)
	}
	if !res.Success || len(res.DeliveredRefs) != 1 {
		t.Errorf("the last message's answer read as success %v with refs %v", res.Success, res.DeliveredRefs)
	}
	for _, kept := range []string{"Running the tests first.", "answer-1: it targets main.", "answer-2: it targets release,"} {
		if !strings.Contains(res.Transcript, kept) {
			t.Errorf("the transcript lost %q:\n%s", kept, res.Transcript)
		}
	}
}

func TestAFailedToolIsMarkedInTheTranscript(t *testing.T) {
	t.Parallel()
	stream := `{"type":"tool_use","part":{"tool":"shell","state":{"input":{"command":"pytest"},"status":"error","error":"2 failed"}}}
{"type":"text","part":{"text":"could not fix it"}}`
	res := opencode().Parse(stream)
	if !strings.Contains(res.Transcript, "error: 2 failed") {
		t.Fatalf("a failed tool is invisible:\n%s", res.Transcript)
	}
}

// THE RUN'S TOKENS ARE THE SESSION'S OWN TOTALS, from the usage report the
// script appends: the whole prompt (what the cache served and stored
// included), and the output with the reasoning 2.0 reports beside it added
// back, as every provider bills it.
func TestOpenCodeReadsTheSessionsUsageReport(t *testing.T) {
	t.Parallel()
	stream := strings.Join([]string{
		`{"type":"text","part":{"text":"done"}}`,
		`{"type":"crewlet.usage","report":{"data":{"id":"ses_abc","cost":0.42,` +
			`"tokens":{"input":100,"output":20,"reasoning":5,"cache":{"read":300,"write":7}}}}}`,
	}, "\n")
	res := opencode().Parse(stream)
	if res.InputTokens != 407 || res.OutputTokens != 25 {
		t.Errorf("tokens = in %d out %d, want in 407 (100+300+7) out 25 (20+5)",
			res.InputTokens, res.OutputTokens)
	}
	if res.CacheReadTokens != 300 || res.CacheWriteTokens != 7 {
		t.Errorf("cache = %d/%d, want 300/7", res.CacheReadTokens, res.CacheWriteTokens)
	}
	if res.CostUSD != 0.42 || res.SessionID != "ses_abc" {
		t.Errorf("cost %v session %q, want 0.42 and ses_abc", res.CostUSD, res.SessionID)
	}
	if res.Text != "done" {
		t.Errorf("the usage line leaked into the answer: %q", res.Text)
	}
}

// An invented token count in the spend rollup is worse than a missing one,
// because a reader cannot tell it is invented — so a run with no usage report
// reports none.
func TestOpenCodeReportsNoTokensRatherThanEstimatingThem(t *testing.T) {
	t.Parallel()
	res := opencode().Parse(`{"type":"text","part":{"text":"done"}}`)
	if res.InputTokens != 0 || res.OutputTokens != 0 || res.CostUSD != 0 {
		t.Fatalf("tokens were invented: %+v", res)
	}
}

// A NEGATIVE PRICE IS PASSED ON, as a negative count is: the coordinator floors
// both where every collected result enters it and says so
// (sandbox_usage_negative). Zeroed here, an OpenCode price below zero was
// floored with nothing logged while a negative count beside it was reported.
//
// Mutation: keep the price only when it is above zero, and the result reads 0.
func TestOpenCodeHandsANegativePriceOnForTheCoordinatorToFloor(t *testing.T) {
	t.Parallel()
	res := opencode().Parse(`{"type":"text","part":{"text":"done"}}` + "\n" +
		`{"type":"crewlet.usage","report":{"data":{"cost":-0.25,"tokens":{"input":-3,"output":1}}}}`)
	if res.CostUSD != -0.25 || res.InputTokens != -3 {
		t.Errorf("cost %v input %d, want both as reported (-0.25, -3) for the coordinator's floor",
			res.CostUSD, res.InputTokens)
	}
}

func TestAnErrorEventIsAFailure(t *testing.T) {
	t.Parallel()
	res := opencode().Parse(`{"type":"error","error":{"type":"provider.quota","message":"quota exceeded"}}`)
	if res.Success {
		t.Fatal("an error event read as success")
	}
	if !strings.Contains(res.Error, "quota exceeded") {
		t.Fatalf("error = %q", res.Error)
	}
}

// Output that is not the event stream is still an answer.
func TestNonJsonOpenCodeOutputIsTakenAsTheAnswer(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	res := opencode().Parse(`{"type":"tool_use","part":{"tool":"shell","state":{"input":{"command":"ls"}}}}`)
	if res.Success {
		t.Fatal("a run that said nothing read as success")
	}
	if res.Error == "" {
		t.Fatal("the failure says nothing")
	}
}

// A RUN THAT CARRIES ON AN ASKING RUN'S WORK CONTINUES ITS OWN CONVERSATION.
// The run that parked on a person's question knew what it had read, tried and
// decided; a new conversation told only the answer starts that over. So where
// the launch names the asking run's session, each runner continues it — Claude
// Code with `--resume`, OpenCode by working in that session, which it
// continues when it already holds it — and where it names none, each starts a
// conversation of its own.
//
// Mutation: drop the flag, or mint OpenCode a fresh session whatever the
// request, and the continued row goes red.
func TestAContinuedRunResumesTheAskingRunsSession(t *testing.T) {
	t.Parallel()
	continued := sandbox.RunRequest{Brief: "the answer: main", Resume: "ses_0123456789abcdef0123456789abcdef"}

	// A SCRIPT, which decides the session where the box holds it
	// (TestAContinuedRunWhoseSessionIsMissingStartsANewOneAndSaysSo).
	cmd := scriptOf(t, commandOf(claude(), continued, executor.Paths{}, ""))
	if !strings.Contains(cmd, "--resume 'ses_0123456789abcdef0123456789abcdef'") {
		t.Errorf("Claude Code does not resume the asking run's session: %s", cmd)
	}
	if fresh := commandOf(claude(), sandbox.RunRequest{Brief: "x"}, executor.Paths{}, ""); strings.Contains(fresh, "--resume") {
		t.Errorf("a fresh Claude Code run resumes something: %s", fresh)
	}

	script := scriptOf(t, commandOf(opencode(), continued, executor.Paths{}, ""))
	if !strings.Contains(script, "sid='ses_0123456789abcdef0123456789abcdef' k=yes") {
		t.Errorf("OpenCode does not work in the asking run's session:\n%s", script)
	}
	if fresh := scriptOf(t, commandOf(opencode(), sandbox.RunRequest{Brief: "x"}, executor.Paths{}, "")); strings.Contains(fresh, "0123456789abcdef0123456789abcdef") {
		t.Errorf("a fresh OpenCode run works in the asking run's session:\n%s", fresh)
	}
}
