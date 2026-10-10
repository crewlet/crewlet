package codingagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
// One of them in a seat's own environment is what stops an `anthropic`
// entry's key and endpoint being added beside it: Claude Code ranks an API key
// above the plan token, so a company key underlaid beside a seat's plan token
// moved that seat onto metered billing without a word.
var ClaudeCodeCredentialEnv = []string{
	"CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN",
	"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY",
}

// ClaudeCode drives Claude Code headless.
//
// It reaches its model through the run ENVIRONMENT rather than a config file,
// which is why WriteConfig here only ever renders MCP servers: the credential
// comes from providers.llm via the sandbox env, and duplicating it into a file
// inside the box would put the secret somewhere the agent can read it back.
type ClaudeCode struct{}

var _ CLI = ClaudeCode{}

// NewClaudeCode returns the runner.
func NewClaudeCode() *Runner { return New(ClaudeCode{}) }

// Name is the coding agent's key in the runner registry.
func (ClaudeCode) Name() string { return ClaudeCodeName }

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
func (ClaudeCode) Command(req sandbox.RunRequest, _ Paths, configPath string) string {
	parts := []string{
		"claude", "-p", shellQuote(req.Brief),
		"--output-format", "stream-json", "--verbose",
		"--permission-mode", "bypassPermissions",
	}
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
	return strings.Join(parts, " ")
}

// WriteConfig renders the scoped MCP surface, or nothing when there is none.
func (ClaudeCode) WriteConfig(ctx context.Context, box sandbox.Sandbox, req sandbox.RunRequest, paths Paths) (string, error) {
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
