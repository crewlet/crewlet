package codingagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// ClaudeCodeName is this runner's config name.
const ClaudeCodeName = "claude-code"

// ClaudeCodeCredentialEnv is every variable that signs Claude Code in to a
// model from its environment: a Pro/Max plan's headless token, an Anthropic
// key or bearer token, or a toggle moving it onto a cloud provider's own
// credentials (Bedrock, Vertex AI, Microsoft Foundry).
//
// The engine reads it twice, and both readings need every member. One of them
// in a seat's own environment is what lets the seat run this runner on an
// entry that is not Anthropic's, and it is what stops an `anthropic` entry's
// key and endpoint being added beside it — Claude Code ranks an API key above
// the plan token, so a company key underlaid beside a seat's plan token moved
// that seat onto metered billing without a word.
var ClaudeCodeCredentialEnv = []string{
	"CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN",
	"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY",
}

// ClaudeCode drives Claude Code headless.
//
// It reaches its model through the run ENVIRONMENT rather than a config file,
// which is why WriteConfig here renders MCP servers and the run's telemetry
// and never the model's credential: that comes from providers.llm via the
// sandbox env, and duplicating it into a file inside the box would put the
// secret somewhere the agent can read it back.
type ClaudeCode struct {
	// uid is the engine process's user id, which is a direct box's
	// ([ClaudeCode.Admit]). Nil reads it from the process; a test states it.
	uid func() int
}

var _ CLI = ClaudeCode{}

// NewClaudeCode returns the runner.
func NewClaudeCode() *Runner { return New(ClaudeCode{}) }

// Name is the coding agent's key in the runner registry.
func (ClaudeCode) Name() string { return ClaudeCodeName }

// claudeSandboxedVar and claudeBubblewrapVar are the two ways Claude Code is
// told the process it runs in is a deliberate sandbox, which is what it
// requires before it will bypass permissions as root: IS_SANDBOX must be
// exactly "1", and CLAUDE_CODE_BUBBLEWRAP anything at all (measured against
// 2.1.300's own check).
const (
	claudeSandboxedVar  = "IS_SANDBOX"
	claudeBubblewrapVar = "CLAUDE_CODE_BUBBLEWRAP"
)

// Admit refuses a direct box when the engine runs as root.
//
// Claude Code will not run `--permission-mode bypassPermissions` — which every
// headless run needs, a prompt would hang it — as root, unless it is told the
// process is a deliberate sandbox: it exits at once, saying so on its error
// stream ("cannot be used with root/sudo privileges"), measured against
// 2.1.300. A CONTAINER is such a sandbox and is told so ([ClaudeCode.Command]).
// A DIRECT box is not: its job is a process tree of the engine's own user on
// the engine's own host (asEngine), and telling the CLI otherwise would be a
// lie about the one thing its refusal protects — a root process with every
// permission bypassed, on the host. So the launch is refused, by a sentence
// naming the two ways out, before a box exists. It is the BACKEND's answer
// that decides, not the cell's name: the in-process fake a demonstration runs
// on answers every cell and runs nothing, so it is refused nothing.
//
// THE CLI'S OWN RULE, mirrored rather than tightened: an operator who has made
// the engine's host itself a sandbox and said so in role.sandbox.env —
// IS_SANDBOX=1, or CLAUDE_CODE_BUBBLEWRAP — has made the claim themselves, and
// the CLI would accept it, so this does too. The engine never makes it for a
// direct box.
func (c ClaudeCode) Admit(spec sandbox.Spec, asEngine bool) error {
	if !asEngine || c.userID() != 0 {
		return nil
	}
	if spec.Env[claudeSandboxedVar] == "1" || spec.Env[claudeBubblewrapVar] != "" {
		return nil
	}
	return errors.New("the claude CLI refuses --permission-mode bypassPermissions to a " +
		"process running as root, and a direct box runs as the engine's own user, which " +
		"is root on this node: run the engine as a non-root user, or run this seat's code " +
		"work in a container box (role.sandbox.run_in: container, or cli.run_in on an " +
		"agent-mode entry)")
}

// userID is the engine process's user id, which a direct box runs as.
func (c ClaudeCode) userID() int {
	if c.uid != nil {
		return c.uid()
	}
	return os.Getuid()
}

// claudeCodeContentSwitches are Claude Code's switches for putting CONTENT on
// what it exports — the prompt, a tool's arguments and output, the model's
// answer, the raw API bodies. Each defaults to off, and each is declared off
// anyway, because a box image or template can bake one on and a run's content
// belongs in the engine's event store rather than in a telemetry backend.
var claudeCodeContentSwitches = []string{
	"OTEL_LOG_USER_PROMPTS", "OTEL_LOG_TOOL_DETAILS", "OTEL_LOG_TOOL_CONTENT",
	"OTEL_LOG_ASSISTANT_RESPONSES", "OTEL_LOG_RAW_API_BODIES",
}

// Telemetry switches Claude Code's own TRACES on, and only its traces.
//
// THREE SWITCHES, because Claude Code exports spans only with all three: its
// telemetry gate (CLAUDE_CODE_ENABLE_TELEMETRY), the session-tracing beta its
// spans sit behind (CLAUDE_CODE_ENHANCED_TELEMETRY_BETA), and a traces
// exporter, which has no default — an empty exporter list exports nothing.
// CLAUDE_CODE_PROPAGATE_TRACEPARENT sends each Messages call's `traceparent`
// to a gateway or a cloud platform too, as the engine's own SDK does whatever
// its base URL; with the first-party API it is sent regardless.
//
// TRACES ONLY. Its log events are its content channel — prompts, tool
// arguments and tool output once their switches are on — and its metrics
// count the tokens and cost the engine already records for the run, so a
// collector summing both would count a coding run twice. Both are switched
// off by name rather than left unset, because an image can set them.
//
// THE SAMPLER IS PARENT-BASED, because the decision is the launching turn's
// and it arrives on TRACEPARENT: an image-baked `always_on` would export the
// spans of a turn the operator's sampler decided against, under a parent
// nobody exported.
//
// ITS SHUTDOWN WAITS AS LONG AS THE RECEIVER MAY TAKE TO ANSWER. Claude Code
// flushes its last batch on exit and gives up after two seconds by default,
// while the receiver forwards that batch before it answers, for up to
// [sandbox.OtelForwardTimeout] — so against a backend slower than two
// seconds, every run lost the spans of its own ending, which is the part that
// says how it ended. The exit is delayed only by an upstream that slow.
//
// AND IT IS PINNED ABOVE THE BOX'S OWN SETTINGS FILES. Claude Code reads the
// `env` of its settings files over its process environment, so an
// environment is the lowest layer it has: a ~/.claude/settings.json an E2B
// template carries, or one a previous run wrote into a reused box, switched
// the prompt capture back on and sent the whole export, prompt included, to
// another collector — measured against 2.1.300, as was the rest of this
// paragraph. So every variable the launch added for the run's telemetry —
// this declaration and the receiver's endpoint, at the values the run
// environment carries, an operator's own included ([sandbox.RunRequest]'s
// Telemetry) — is also handed to the CLI as FLAG SETTINGS (`--settings` and a
// file), which Claude Code layers over a user settings file and over its
// environment. A FILE, not the JSON on the command line, because the
// endpoint carries the run's token and a process's argv is listed by every ps
// on the host of a direct box or a container; the file sits in the box beside
// the environment that already holds the same token. A project's settings
// cannot set the destination at all. What NOTHING here can outrank is a
// MANAGED settings file (/etc/claude-code/managed-settings.json on Linux),
// which Claude Code layers over every other source, flags included: an image
// that ships one, or the engine host's own for a direct box, decides this
// agent's telemetry whatever the engine sets.
func (ClaudeCode) Telemetry() map[string]string {
	env := map[string]string{
		"CLAUDE_CODE_ENABLE_TELEMETRY":         "1",
		"CLAUDE_CODE_ENHANCED_TELEMETRY_BETA":  "1",
		"CLAUDE_CODE_PROPAGATE_TRACEPARENT":    "1",
		"OTEL_TRACES_EXPORTER":                 "otlp",
		"OTEL_METRICS_EXPORTER":                "none",
		"OTEL_LOGS_EXPORTER":                   "none",
		"OTEL_TRACES_SAMPLER":                  "parentbased_always_on",
		"CLAUDE_CODE_OTEL_SHUTDOWN_TIMEOUT_MS": strconv.FormatInt(sandbox.OtelAnswerWithin.Milliseconds(), 10),
	}
	for _, name := range claudeCodeContentSwitches {
		env[name] = "false"
	}
	return env
}

// Command builds the headless `claude -p` invocation.
//
// Pure and deterministic so the exact flags can be pinned by a test. Budget
// caps are appended only when set, so a minimal call stays minimal.
//
// It runs with permissions bypassed, and that is a deliberate consequence of
// the scoping model rather than an oversight: the box IS the boundary, and
// there is no per-tool allowlist inside it. What the agent may reach is
// decided by which MCP servers were rendered into its config and which
// credentials the run env carries — both engine-side, both before the agent
// starts. A permission prompt would simply hang a headless run.
//
// STREAM-JSON, AND --verbose WITH IT. `--output-format json` prints ONE object
// when the run ends and nothing before it, and in print mode this CLI writes
// nothing to stderr either — so a run on the engine's default coding agent
// showed a person nothing while it worked and left no transcript when it was
// done. stream-json prints every message as it happens, one JSON object a
// line, ending — as the vendor documents the stream — in the result message,
// the same object json printed alone. In print mode the CLI refuses
// stream-json without --verbose ("When using --print,
// --output-format=stream-json requires --verbose").
func (ClaudeCode) Command(req sandbox.RunRequest, paths Paths, configPath string) string {
	var parts []string
	if req.Placement == sandbox.Container {
		// A CONTAINER IS A DELIBERATE SANDBOX, and Claude Code is told so,
		// because it refuses bypassPermissions to a process running as
		// root unless it is (see [ClaudeCode.Admit]) — and a container's
		// process IS root under a rootless runtime, and under any runtime
		// on a host whose engine runs as root. Set in the invocation, not
		// the run's environment, so a profile the login shell sources
		// cannot take it away; and in no other cell, because it is a claim
		// about where the process runs: a direct box runs on the engine's
		// own host as the engine's own user, and an E2B box runs as the
		// template's unprivileged user, where the claim buys nothing and
		// would change how the CLI behaves for nothing (it also governs
		// whether an overloaded API is retried or given up on).
		parts = append(parts, claudeSandboxedVar+"=1")
	}
	parts = append(parts,
		"claude", "-p", shellQuote(req.Brief),
		"--output-format", "stream-json", "--verbose",
		"--permission-mode", "bypassPermissions",
	)
	// VERBATIM, because whichever entry reached this runner already names
	// the model in Claude Code's own terms: an `anthropic` entry's model id,
	// or a claude-code cli-agent entry's alias exactly as its text calls
	// pass it. The engine refuses every other cli-agent entry here, and a
	// run on the seat's own sign-in over an OpenAI-wire entry carries no
	// model, so Claude Code's own choice stands.
	if req.LLM != nil && req.LLM.Model != "" {
		parts = append(parts, "--model", shellQuote(req.LLM.Model))
	}
	if req.Limits.MaxTurns > 0 {
		parts = append(parts, "--max-turns", strconv.Itoa(req.Limits.MaxTurns))
	}
	if req.Limits.MaxBudgetUSD > 0 {
		parts = append(parts, "--max-budget-usd",
			strconv.FormatFloat(req.Limits.MaxBudgetUSD, 'f', -1, 64))
	}
	if configPath != "" {
		// --strict-mcp-config so the agent gets EXACTLY the scoped surface:
		// without it the CLI also loads whatever config the box's home
		// happens to carry, which on a reused box is the previous run's.
		parts = append(parts, "--mcp-config", shellQuote(configPath), "--strict-mcp-config")
	}
	if len(req.Telemetry) > 0 {
		// The run's telemetry, over every settings file the box carries
		// but a managed one ([ClaudeCode.Telemetry]); WriteConfig wrote it
		// under this same condition.
		parts = append(parts, "--settings", shellQuote(paths.Settings()))
	}
	return strings.Join(parts, " ")
}

// WriteConfig renders the run's telemetry as flag settings when the launch
// added any ([ClaudeCode.Telemetry]), and the scoped MCP surface, whose path
// it returns — or "" when there is none.
func (ClaudeCode) WriteConfig(ctx context.Context, box sandbox.Sandbox, req sandbox.RunRequest, paths Paths) (string, error) {
	if len(req.Telemetry) > 0 {
		blob, err := json.Marshal(map[string]any{"env": req.Telemetry})
		if err != nil {
			return "", err
		}
		if err := box.WriteFile(ctx, paths.Settings(), blob); err != nil {
			return "", fmt.Errorf("codingagent: writing the run's telemetry settings: %w", err)
		}
	}
	if len(req.MCPServers) == 0 {
		return "", nil
	}
	servers := make(map[string]any, len(req.MCPServers))
	for name, s := range req.MCPServers {
		servers[name] = claudeCodeMCP(s)
	}
	blob, err := json.MarshalIndent(map[string]any{"mcpServers": servers}, "", "  ")
	if err != nil {
		return "", err
	}
	if err := box.WriteFile(ctx, paths.MCPConfig(), blob); err != nil {
		return "", err
	}
	return paths.MCPConfig(), nil
}

// claudeCodeMCP is one server in this CLI's own .mcp.json vocabulary.
//
// THE KEY NAMES ARE THIS RUNNER'S, which is why they live here: they are what
// `claude --mcp-config` reads, and nothing above this file should have to know
// them. internal/sandbox used to spell them out and then say in its own doc
// that they belonged to a runner.
//
// An empty `env` or `headers` is OMITTED rather than written as an empty
// object, which is what the CLI's own examples show and what this wrote
// before.
func claudeCodeMCP(s sandbox.MCPServer) map[string]any {
	if s.Transport == sandbox.TransportHTTP {
		out := map[string]any{"type": string(sandbox.TransportHTTP), "url": s.URL}
		if len(s.Headers) > 0 {
			out["headers"] = s.Headers
		}
		return out
	}
	out := map[string]any{"command": s.Command, "args": s.Args}
	if len(s.Env) > 0 {
		out["env"] = s.Env
	}
	return out
}

// Output is where a job writes under stream-json.
//
// The EVENT STREAM goes to its own file, read as a stream and from its end,
// and the RESULT is the stream's last line, which the wrapper copies to the
// result file after the CLI exits — so the result is read whole from a file of
// one line however long the stream grew. It exits cleanly, so the done marker
// is its only completion signal and the poll reads nothing of the stream.
func (ClaudeCode) Output(paths Paths) Output {
	return Output{Stdout: paths.Stream(), Events: true, Result: paths.Result()}
}

// Events is a new decoder for one stream, read whole or followed ([Decoder]).
func (ClaudeCode) Events() Decoder { return &claudeEvents{tools: map[string]string{}} }

// Finished is false: this CLI exits cleanly, so the done marker is the signal.
func (ClaudeCode) Finished(string) bool { return false }

// Parse maps the CLI's result — the stream's last line — onto a result.
//
// TOLERANT BY DESIGN: non-JSON or partial output yields a FAILED result
// carrying an account of it, never an error. A coding agent that crashed should
// surface as "did not deliver" and let the turn continue, not blow the turn up
// — the executor can still report what happened, which is more use to the
// requester than a failed turn.
//
// A RESULT IS AN OBJECT OF TYPE `result`, and nothing else is. Under the
// stream the result file holds the stream's LAST LINE, whatever it was: a run
// that died part-way leaves an assistant message or a tool result there, and
// an object of another type has no subtype and no is_error — read as a
// result, it was a run that SUCCEEDED and said nothing.
func (ClaudeCode) Parse(stdout string) sandbox.Result {
	text := strings.TrimSpace(stdout)
	if text == "" {
		return sandbox.Result{Error: "the coding agent produced no output"}
	}
	msg, ok := decodeClaudeResult(text)
	if !ok {
		if last := lastLine(text); strings.HasPrefix(last, "{") {
			// A LINE THAT STARTS AN OBJECT AND IS NOT ONE is a message the
			// process stopped writing part-way, not an account of anything:
			// its fragment — of a file it read, as like as not — is no use to
			// a reader, and it is what the error stream beside it explains.
			return sandbox.Result{Error: fmt.Sprintf("the coding agent's output ends in a line "+
				"that is not a whole JSON object (%s), so it never reported how its run ended",
				humanSize(int64(len(last))))}
		}
		// THE OUTPUT IS THE FAILURE'S DETAIL, WHOLE. Unparseable output is
		// the case where the text IS the account — there is no structured
		// field to fall back to — and the useful part of it (the actual
		// error, after the banner and the warnings) is at the END, which a
		// head cut discarded and a tail cut kept only by luck of size. It
		// is carried as the Error rather than as a report, because it is
		// not one: the coordinator condenses a failure past the record's
		// bound keeping its cause, where a report keeps its findings.
		return sandbox.Result{
			Error: "the coding agent's output could not be parsed:\n" + text,
		}
	}
	if msg.Type != "result" {
		what := "an object with no type"
		if msg.Type != "" {
			what = fmt.Sprintf("a %q message", msg.Type)
		}
		return sandbox.Result{Error: "the coding agent stopped before it reported how its run ended: " +
			"the last thing it printed was " + what + ", not its result"}
	}

	res := sandbox.Result{
		Text:          msg.Result,
		Success:       msg.succeeded(),
		SessionID:     msg.SessionID,
		CostUSD:       msg.TotalCostUSD,
		DeliveredRefs: deliveredRefs(msg.Result),
	}
	if u := msg.Usage; u != nil {
		// INPUT TOKENS ARE A SUM, for the reason the engine's own Anthropic
		// provider states: the vendor's input_tokens is only the UNCACHED
		// remainder, and a coding run is almost entirely cached rounds —
		// so reading it alone put a fraction of every run's prompt on the
		// budget counter and the spend rollup.
		res.CacheReadTokens = u.CacheReadInputTokens
		res.CacheWriteTokens = u.CacheCreationInputTokens
		res.InputTokens = u.InputTokens + res.CacheReadTokens + res.CacheWriteTokens
		res.OutputTokens = u.OutputTokens
	}
	if !res.Success {
		res.Error = msg.failure()
	}
	return res
}

// claudeResult is the CLI's result message — the stream's last line, and the
// one object `--output-format json` printed — as the vendor's Agent SDK
// documents it: `subtype` names HOW the run ended ("success",
// "error_max_turns", "error_during_execution", "error_max_budget_usd", …),
// `is_error` whether it failed, `result` the final text of a run that got as
// far as one, and `errors` what went wrong on a subtype that is not success,
// which carries no `result` at all.
//
// Decoded into named fields so the stream's decoder and [ClaudeCode.Parse] read
// one shape, and TOLERANTLY (a field of an unexpected type is left zero and
// the rest still read), because a field the vendor reshapes must cost that
// field rather than the run's whole account.
type claudeResult struct {
	Type         string   `json:"type"`
	Subtype      string   `json:"subtype"`
	IsError      bool     `json:"is_error"`
	Result       string   `json:"result"`
	Errors       []string `json:"errors"`
	SessionID    string   `json:"session_id"`
	TotalCostUSD float64  `json:"total_cost_usd"`
	Usage        *struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

// succeeded is whether the run finished its work. BOTH FIELDS MUST SAY SO: a
// run that hit its turn cap reports no is_error but did not finish, and one
// whose model call failed is a success subtype carrying is_error.
func (r claudeResult) succeeded() bool {
	return (r.Subtype == "" || r.Subtype == "success") && !r.IsError
}

// failure says how a run that did not succeed ended: the subtype that names
// how, and whatever the message says about why — its errors, or the final
// text it got as far as.
func (r claudeResult) failure() string {
	how := r.Subtype
	if how == "success" {
		how = ""
	}
	why := strings.TrimSpace(firstNonBlank(strings.Join(r.Errors, "\n"), r.Result))
	switch {
	case how != "" && why != "":
		return how + ": " + why
	case how != "":
		return how
	case why != "":
		return why
	}
	return "it reported an error and said nothing about it"
}

// decodeClaudeResult reads the result message: the one line the result file
// holds, which the wrapper copied from the end of the stream.
func decodeClaudeResult(text string) (claudeResult, bool) {
	if !strings.HasPrefix(text, "{") {
		return claudeResult{}, false
	}
	var msg claudeResult
	err := json.Unmarshal([]byte(text), &msg)
	var mismatch *json.UnmarshalTypeError
	// A syntax error is reported before any field is decoded, so a mismatch
	// means a whole object with one field of another type.
	if err == nil || errors.As(err, &mismatch) {
		return msg, true
	}
	return claudeResult{}, false
}

// lastLine is the text's last line, trimmed.
func lastLine(text string) string {
	if i := strings.LastIndexByte(text, '\n'); i >= 0 {
		text = text[i+1:]
	}
	return strings.TrimSpace(text)
}
