package codingagent

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// ClaudeCodeName is this runner's config name.
const ClaudeCodeName = "claude-code"

// rawTextLimit bounds the unparseable output carried back as the result text.
//
// Enough to see a banner, a usage message or a stack's top — which is what
// unparseable output usually is — without putting a whole log through the
// event store as if it were a report.
const rawTextLimit = 2000

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
func (ClaudeCode) Command(req sandbox.RunRequest, _ Paths, configPath string) string {
	parts := []string{
		"claude", "-p", shellQuote(req.Brief),
		"--output-format", "json",
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
	blob, err := json.MarshalIndent(map[string]any{"mcpServers": req.MCPServers}, "", "  ")
	if err != nil {
		return "", err
	}
	if err := box.WriteFile(ctx, paths.MCPConfig(), blob); err != nil {
		return "", err
	}
	return paths.MCPConfig(), nil
}

// Finished is false: this CLI exits cleanly, so the done marker is the signal.
func (ClaudeCode) Finished(string) bool { return false }

// Parse maps the CLI's JSON output onto a result.
//
// TOLERANT BY DESIGN: non-JSON or partial output yields a FAILED result
// carrying the raw text, never an error. A coding agent that crashed should
// surface as "did not deliver" and let the turn continue, not blow the turn up
// — the executor can still report what happened, which is more use to the
// requester than a failed turn.
func (ClaudeCode) Parse(stdout string) sandbox.Result {
	text := strings.TrimSpace(stdout)
	if text == "" {
		return sandbox.Result{Error: "the coding agent produced no output"}
	}
	obj, ok := decodeObject(text)
	if !ok {
		return sandbox.Result{
			Text:  truncate(text, rawTextLimit),
			Error: "the coding agent's output could not be parsed",
		}
	}

	resultText := stringField(obj, "result")
	// subtype names HOW the run ended ("success", "error_max_turns", …), and
	// is_error whether it failed. Both must be right: a run that hit its
	// turn cap reports no is_error but did not finish.
	success := stringOr(obj, "subtype", "success") == "success" && !boolField(obj, "is_error")

	res := sandbox.Result{
		Text:          resultText,
		Success:       success,
		SessionID:     stringField(obj, "session_id"),
		CostUSD:       floatField(obj, "total_cost_usd"),
		DeliveredRefs: prPattern.FindAllString(resultText, -1),
	}
	if usage, ok := obj["usage"].(map[string]any); ok {
		res.InputTokens = intField(usage, "input_tokens")
		res.OutputTokens = intField(usage, "output_tokens")
	}
	if !success {
		res.Error = stringField(obj, "error")
		if res.Error == "" {
			res.Error = truncate(resultText, errorDetailLimit)
		}
	}
	return res
}

// decodeObject reads a JSON object, falling back to the LAST line.
//
// The CLI sometimes prints a banner or a warning before its JSON, so a whole-
// text parse fails on output that is perfectly good — and the object is always
// last, because it is the thing the CLI prints when it is done.
func decodeObject(text string) (map[string]any, bool) {
	var obj map[string]any
	if json.Unmarshal([]byte(text), &obj) == nil && obj != nil {
		return obj, true
	}
	lines := strings.Split(text, "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	if last == "" {
		return nil, false
	}
	if json.Unmarshal([]byte(last), &obj) == nil && obj != nil {
		return obj, true
	}
	return nil, false
}

func stringField(obj map[string]any, key string) string {
	s, _ := obj[key].(string)
	return s
}

func stringOr(obj map[string]any, key, fallback string) string {
	if s, ok := obj[key].(string); ok {
		return s
	}
	return fallback
}

func boolField(obj map[string]any, key string) bool {
	b, _ := obj[key].(bool)
	return b
}

func floatField(obj map[string]any, key string) float64 {
	switch v := obj[key].(type) {
	case float64:
		return v
	case json.Number:
		f, _ := v.Float64()
		return f
	}
	return 0
}

func intField(obj map[string]any, key string) int {
	switch v := obj[key].(type) {
	case float64:
		return int(v)
	case json.Number:
		n, _ := v.Int64()
		return int(n)
	}
	return 0
}
