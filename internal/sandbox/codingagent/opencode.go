package codingagent

import (
	"context"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// OpenCodeName is this runner's config name.
const OpenCodeName = "opencode"

// OpenCodeProviderID is the provider a run's config declares.
//
// Its own id rather than one of the vendor names, because it points at the
// SEAT's endpoint and model, which may be neither vendor's default.
const OpenCodeProviderID = "crewlet"

// transcriptDetailLimit caps one transcript line's echoed command or path.
//
// A bare "[tool] bash" is useless when a run fails — you cannot see WHAT ran —
// but a heredoc echoed whole would blow up the phase event. 160 characters is
// a readable command line.
const transcriptDetailLimit = 160

// OpenCode drives the OpenCode CLI headless.
//
// Provider-agnostic, which is why it configures its own LLM rather than
// reading the env: pointing it at a seat's model means declaring a custom
// provider with an explicit base URL and the exact model id, because
// OpenCode otherwise resolves a bare "<provider>/<model>" against a catalogue
// AND the vendor's default endpoint — so a custom gateway plus an unlisted
// model either fails to resolve or silently hits the wrong host.
type OpenCode struct{}

var _ CLI = OpenCode{}

// NewOpenCode returns the runner.
func NewOpenCode() *Runner { return New(OpenCode{}) }

// Name is the coding agent's key in the runner registry.
func (OpenCode) Name() string { return OpenCodeName }

// Command builds the non-interactive `opencode run` invocation.
//
// ALWAYS --format json, and that is load-bearing rather than a preference.
// `opencode run` is known to finish its work and never exit — it leaves
// handles open — and in its default mode the final summary is buffered and
// lost when the process hangs. With JSON output the assistant's text and a
// terminal event are flushed per line as they happen, so the result is
// captured and the completion is detectable even though the process never
// returns. See [OpenCode.Finished].
func (OpenCode) Command(req sandbox.RunRequest, _ Paths, _ string) string {
	parts := []string{"opencode", "run", shellQuote(req.Brief)}
	if model := openCodeModelArg(req.LLM); model != "" {
		parts = append(parts, "--model", shellQuote(model))
	}
	return strings.Join(append(parts, "--format", "json"), " ")
}

// openCodeModelArg is the fully-formed --model value.
//
// A custom base URL means the run's own declared provider; otherwise the model
// is addressed under its vendor FAMILY. The family comes from the provider
// type rather than being assumed, because a subscription entry's type is the
// same for every vendor — reading it would address a Claude subscription's
// model as an OpenAI one.
func openCodeModelArg(llm *sandbox.AgentLLM) string {
	if llm == nil || llm.Model == "" {
		return ""
	}
	if llm.BaseURL != "" {
		return OpenCodeProviderID + "/" + llm.Model
	}
	return openCodeFamily(llm.ProviderType) + "/" + llm.Model
}

func openCodeFamily(providerType string) string {
	switch providerType {
	case "anthropic", "claude":
		return "anthropic"
	case "google", "gemini":
		return "google"
	default:
		return "openai"
	}
}

// WriteConfig renders opencode.json: the custom provider and the scoped MCP
// surface.
//
// THE API KEY IS NEVER IN THE PAYLOAD. It rides the run env and the config
// references it through OpenCode's own {env:VAR} interpolation, so the secret
// is not written into a file inside the box where the agent could read it back
// and echo it into its report.
func (OpenCode) WriteConfig(ctx context.Context, box sandbox.Sandbox, req sandbox.RunRequest, paths Paths) (string, error) {
	cfg := map[string]any{
		// Sharing is disabled: a run's transcript is company work, and
		// OpenCode's share feature publishes it to a URL.
		"share": "disabled",
	}
	if req.LLM != nil && req.LLM.BaseURL != "" && req.LLM.Model != "" {
		cfg["provider"] = openCodeProvider(*req.LLM)
	}
	if len(req.MCPServers) > 0 {
		cfg["mcp"] = openCodeMCP(req.MCPServers)
	}
	if len(cfg) == 1 {
		// Only the share setting, which the CLI defaults sensibly enough:
		// writing a config for it alone would put a file on a reused box
		// that a later run with real settings then has to overwrite.
		return "", nil
	}
	blob, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", err
	}
	// opencode.json in the WORKING DIRECTORY, which is the checkout: the CLI
	// takes no --config flag, so the file's location is the wiring.
	path := paths.Home + "/" + sandbox.WorkspaceSubdir + "/opencode.json"
	if err := box.WriteFile(ctx, path, blob); err != nil {
		return "", err
	}
	return path, nil
}

func openCodeProvider(llm sandbox.AgentLLM) map[string]any {
	anthropic := openCodeFamily(llm.ProviderType) == "anthropic"
	npm := "@ai-sdk/openai-compatible"
	keyEnv := "OPENAI_API_KEY"
	if anthropic {
		npm = "@ai-sdk/anthropic"
		keyEnv = "ANTHROPIC_API_KEY"
	}
	return map[string]any{
		OpenCodeProviderID: map[string]any{
			"npm":  npm,
			"name": "Crewlet role LLM",
			"options": map[string]any{
				"baseURL": llm.BaseURL,
				"apiKey":  "{env:" + keyEnv + "}",
			},
			"models": map[string]any{llm.Model: map[string]any{}},
		},
	}
}

// openCodeMCP translates the generic launch specs into OpenCode's schema.
func openCodeMCP(servers map[string]sandbox.LaunchSpec) map[string]any {
	out := make(map[string]any, len(servers))
	for name, spec := range servers {
		entry := map[string]any{"enabled": true}
		if kind, _ := spec["type"].(string); kind == "http" {
			entry["type"] = "remote"
			entry["url"], _ = spec["url"].(string)
			if headers, ok := spec["headers"]; ok {
				entry["headers"] = headers
			}
		} else {
			cmd := []string{}
			if c, ok := spec["command"].(string); ok && c != "" {
				cmd = append(cmd, c)
			}
			if args, ok := spec["args"].([]string); ok {
				cmd = append(cmd, args...)
			} else if args, ok := spec["args"].([]any); ok {
				for _, a := range args {
					if s, ok := a.(string); ok && s != "" {
						cmd = append(cmd, s)
					}
				}
			}
			entry["type"] = "local"
			entry["command"] = cmd
			if env, ok := spec["env"]; ok {
				entry["environment"] = env
			}
		}
		out[name] = entry
	}
	return out
}

// Finished reports whether the streamed output says the agent has stopped.
//
// THIS RUNNER NEEDS IT because `opencode run` finishes its work and hangs, so
// the shell wrapper never reaches the done-marker write. Three terminal
// signals, because the shape has moved across versions and a box runs whatever
// the operator's image has:
//
//   - a step_finish whose reason is "stop" — the assistant produced its final
//     message and asked for no further tools (intermediate steps carry
//     "tool-calls");
//   - a session.status event reporting idle, on newer builds;
//   - an error event, which is also an ending.
func (OpenCode) Finished(stdout string) bool {
	for _, obj := range streamEvents(stdout) {
		switch eventType(obj) {
		case "error":
			return true
		case "step_finish":
			if part, ok := obj["part"].(map[string]any); ok {
				if reason, _ := part["reason"].(string); reason == "stop" {
					return true
				}
			}
		case "session.status":
			props, _ := obj["properties"].(map[string]any)
			status, _ := props["status"].(map[string]any)
			if kind, _ := status["type"].(string); kind == "idle" {
				return true
			}
		}
	}
	return false
}

// Parse reconstructs the answer and a readable transcript from the stream.
//
// OpenCode exposes no stable token or cost envelope, so those stay ZERO rather
// than being estimated: an invented number in the spend rollup is worse than a
// missing one, because a reader cannot tell it is invented.
func (OpenCode) Parse(stdout string) sandbox.Result {
	text := strings.TrimSpace(stdout)
	if text == "" {
		return sandbox.Result{Error: "the coding agent produced no output"}
	}
	events := streamEvents(text)
	if len(events) == 0 {
		// Non-JSON output — an older CLI, or a run captured before the
		// format flag. The whole blob is the answer.
		return sandbox.Result{
			Text: text, Success: true, DeliveredRefs: prPattern.FindAllString(text, -1),
		}
	}

	var answers, transcript []string
	errText := ""
	for _, obj := range events {
		part, _ := obj["part"].(map[string]any)
		switch eventType(obj) {
		case "text":
			chunk := strings.TrimSpace(stringField(part, "text"))
			if chunk != "" {
				answers = append(answers, chunk)
				transcript = append(transcript, chunk)
			}
		case "tool_use":
			transcript = append(transcript, toolLine(part, obj))
		case "error":
			errText = errorText(obj["error"])
			transcript = append(transcript, "[error] "+errText)
		}
	}
	body := strings.TrimSpace(strings.Join(answers, "\n"))
	success := body != "" && errText == ""
	res := sandbox.Result{
		Text:          body,
		Success:       success,
		Transcript:    strings.TrimSpace(strings.Join(transcript, "\n")),
		DeliveredRefs: prPattern.FindAllString(body, -1),
	}
	if !success {
		res.Error = errText
		if res.Error == "" {
			res.Error = "the coding agent produced no answer"
		}
	}
	return res
}

// toolLine renders one tool event for the transcript.
//
// Enriched with the call's own input, because a bare tool name is useless when
// a run fails: what a reader needs is the command that ran. The nesting varies
// across versions, so every lookup is defensive and falls back to the name.
// Transcript only — never fed to a model — so a little extra length is fine.
func toolLine(part, obj map[string]any) string {
	name := firstString(part["tool"], obj["tool"], obj["name"])
	if name == "" {
		name = "tool"
	}
	state, _ := part["state"].(map[string]any)
	input, _ := state["input"].(map[string]any)
	if input == nil {
		input, _ = part["input"].(map[string]any)
	}

	line := "[tool] " + name
	if detail := firstString(
		input["command"], input["filePath"], input["path"],
		input["pattern"], input["description"],
	); detail != "" {
		line += ": " + firstLine(detail, transcriptDetailLimit)
	}

	status, _ := state["status"].(string)
	failure := firstString(state["error"])
	if failure == "" && status == "error" {
		failure = firstString(state["output"])
	}
	if status == "error" || failure != "" {
		if failure == "" {
			failure = "failed"
		}
		line += " → error: " + firstLine(failure, transcriptDetailLimit)
	}
	return line
}

// streamEvents decodes the newline-delimited JSON objects, skipping anything
// that is not one.
//
// Tolerant on purpose: the stream is flushed live and a poll can read it
// mid-line, so the last entry is routinely a partial object. Dropping it is
// correct — the next read has it whole.
func streamEvents(text string) []map[string]any {
	var out []map[string]any
	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] != '{' {
			continue
		}
		var obj map[string]any
		if json.Unmarshal([]byte(line), &obj) == nil && obj != nil {
			out = append(out, obj)
		}
	}
	return out
}

func eventType(obj map[string]any) string {
	s, _ := obj["type"].(string)
	return s
}

func errorText(v any) string {
	switch e := v.(type) {
	case string:
		return e
	case nil:
		return ""
	default:
		if blob, err := json.Marshal(e); err == nil {
			return string(blob)
		}
		return ""
	}
}

func firstString(values ...any) string {
	for _, v := range values {
		if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// firstLine is one transcript line's echoed command or path, bounded.
//
// The bound is real — a heredoc echoed whole would blow up the phase event —
// and the cut is marked. Two defects it used to carry: `line[:limit-1] + "…"`
// emits limit+2 BYTES (limit-1 of content plus a three-byte ellipsis), so the
// constant bounded nothing it named; and the byte slice split whatever
// multi-byte character straddled the cut, which reaches the event store as
// invalid UTF-8.
func firstLine(s string, limit int) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	if limit <= 0 || len(line) <= limit {
		return line
	}
	cut := max(limit-len("…"), 0)
	for cut > 0 && !utf8.RuneStart(line[cut]) {
		cut--
	}
	return line[:cut] + "…"
}
