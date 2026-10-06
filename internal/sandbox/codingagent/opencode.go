package codingagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/textcut"
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

// WriteConfig renders opencode.json: the run's posture, the custom provider
// and the scoped MCP surface.
//
// THE API KEY IS NEVER IN THE PAYLOAD. It rides the run env and the config
// references it through OpenCode's own {env:VAR} interpolation, so the secret
// is not written into a file inside the box where the agent could read it back
// and echo it into its report.
//
// IT IS WRITTEN ON EVERY RUN, including one that names no provider and no
// server. The posture keys below are not decoration a minimal run can go
// without — and because this CLI takes no config flag, the file's LOCATION is
// the wiring, so a run that writes nothing does not run on the vendor's
// defaults: it runs on whatever the PREVIOUS run left in the checkout, which
// on a reused box is that run's MCP block with its dead per-run bridge URL in
// it.
func (OpenCode) WriteConfig(ctx context.Context, box sandbox.Sandbox, req sandbox.RunRequest, paths Paths) (string, error) {
	cfg := map[string]any{
		// Sharing is disabled: a run's transcript is company work, and
		// OpenCode's share feature publishes it to a URL.
		"share": "disabled",
		// ALLOW, STATED RATHER THAN INHERITED. `opencode run` is headless,
		// so a gate left at `ask` has nobody to answer it and is refused —
		// the run does not stop, it loses the call and carries on, which
		// reads as an agent that would not do the work. `external_directory`
		// is the one that bites without looking like a permission at all: a
		// checkout that is not the CLI's own cwd is gated behind it.
		//
		// The box is the boundary here exactly as it is for claude-code's
		// --permission-mode bypassPermissions (see [ClaudeCode.Command]):
		// what the agent may reach was decided engine-side, before it
		// started, by which MCP servers and credentials the run carries. And
		// a vendor default is not a posture to rest on — it is a value that
		// moves between releases, which is the assumption every profile in
		// internal/providers/llm/cliagent is built on.
		"permission": "allow",
		// A CLI that updates itself mid-run swaps the tool under the brief
		// between launch and result, needs network a box may not have, and
		// does both while a seat's turn is suspended waiting on it.
		"autoupdate": false,
	}
	if req.LLM != nil && req.LLM.BaseURL != "" && req.LLM.Model != "" {
		cfg["provider"] = openCodeProvider(*req.LLM)
	}
	if len(req.MCPServers) > 0 {
		cfg["mcp"] = openCodeMCP(req.MCPServers)
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
func openCodeMCP(servers map[string]sandbox.MCPServer) map[string]any {
	out := make(map[string]any, len(servers))
	for name, s := range servers {
		entry := map[string]any{"enabled": true}
		if s.Transport == sandbox.TransportHTTP {
			entry["type"] = "remote"
			entry["url"] = s.URL
			if len(s.Headers) > 0 {
				entry["headers"] = s.Headers
			}
			out[name] = entry
			continue
		}
		// ONE ARGV, not a command and its arguments: this CLI takes the
		// whole invocation as a single array, which is the one place its
		// vocabulary differs structurally rather than in spelling.
		cmd := make([]string, 0, len(s.Args)+1)
		if s.Command != "" {
			cmd = append(cmd, s.Command)
		}
		for _, a := range s.Args {
			if a != "" {
				cmd = append(cmd, a)
			}
		}
		entry["type"] = "local"
		entry["command"] = cmd
		if len(s.Env) > 0 {
			entry["environment"] = s.Env
		}
		out[name] = entry
	}
	return out
}

// Layout is OpenCode's one layout, the one it has always been launched with.
func (OpenCode) Layout() int { return 0 }

// Output is OpenCode's layout, whatever the job declared — it has only ever
// had one: its stdout IS its event stream and its result is derived from it,
// and the stream's end can say the agent is done before the process exits —
// which is the hang [OpenCode.Finished] exists for. The stream is the file
// every build's clear removes, so a reused box never shows a job another's.
func (OpenCode) Output(paths Paths, _ int) Output {
	return Output{Stdout: paths.Result(), Events: true, Terminal: true}
}

// Events is a fresh decoder for one read of the stream.
func (OpenCode) Events() Decoder { return &openCodeEvents{} }

// Finished reports whether the stream's end says the agent has stopped.
//
// THIS RUNNER NEEDS IT because `opencode run` finishes its work and hangs, so
// the shell wrapper never reaches the done-marker write. Three terminal
// signals, because the shape has moved across versions and a box runs whatever
// the operator's image has:
//
//   - a step_finish whose reason is "stop" — the assistant produced its final
//     message and asked for no further tools (intermediate steps carry
//     "tool-calls");
//   - a session.status event reporting idle, on builds that print it;
//   - an error event, which is also an ending.
//
// Asked of the stream's END only ([terminalWindow]): the CLI prints a
// terminal event when its session stops and nothing after it, so an error a
// session went on from, far up the stream, is no longer read as the end of
// the run.
func (OpenCode) Finished(lines string) bool {
	finished := false
	_ = eachLine(strings.NewReader(lines), lineFunc(func(line []byte) {
		ev, ok := decodeOpenCodeEvent(line)
		if !ok {
			return
		}
		switch ev.Type {
		case "error":
			finished = true
		case "step_finish":
			finished = finished || ev.Part.Reason == "stop"
		case "session.status":
			finished = finished || ev.Properties.Status.Type == "idle"
		}
	}))
	return finished
}

// Parse decodes a whole stream already in hand: the result of a run whose
// result IS its stream.
//
// OpenCode exposes no stable token or cost envelope, so those stay ZERO rather
// than being estimated: an invented number in the spend rollup is worse than a
// missing one, because a reader cannot tell it is invented.
func (OpenCode) Parse(stdout string) sandbox.Result {
	return decodeAll(&openCodeEvents{}, stdout)
}

// openCodeEvent is the part of one stream event this runner reads, and no
// more: the event carries the tool's whole state, its output included, and a
// generic map of every field was several times the line it came from.
//
// DECODED LOOSELY. A field whose type a CLI version changed is skipped rather
// than losing the event — see [decodeOpenCodeEvent] — because the nesting has
// moved across versions and a box runs whatever the operator's image has.
type openCodeEvent struct {
	Type  string          `json:"type"`
	Tool  string          `json:"tool"`
	Name  string          `json:"name"`
	Error json.RawMessage `json:"error"`
	Part  struct {
		Tool   string         `json:"tool"`
		Text   string         `json:"text"`
		Reason string         `json:"reason"`
		Input  *openCodeInput `json:"input"`
		State  struct {
			Status string         `json:"status"`
			Error  string         `json:"error"`
			Output string         `json:"output"`
			Input  *openCodeInput `json:"input"`
		} `json:"state"`
	} `json:"part"`
	Properties struct {
		Status struct {
			Type string `json:"type"`
		} `json:"status"`
	} `json:"properties"`
}

// openCodeInput is the part of a tool call's input a transcript line names.
type openCodeInput struct {
	Command     string `json:"command"`
	FilePath    string `json:"filePath"`
	Path        string `json:"path"`
	Pattern     string `json:"pattern"`
	Description string `json:"description"`
}

// decodeOpenCodeEvent reads one line as an event, or reports that it is not
// one: a line that is not a JSON object, or a partial one — the stream is
// flushed live and a read can end mid-line, and the next read has it whole.
//
// A TYPE MISMATCH IS NOT A LOST EVENT. encoding/json skips a field whose value
// does not fit its Go type, decodes the rest, and reports the first such
// mismatch — so the mismatch is ignored and the event kept, which is what the
// map lookups this replaced did for free.
func decodeOpenCodeEvent(line []byte) (openCodeEvent, bool) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || line[0] != '{' {
		return openCodeEvent{}, false
	}
	var ev openCodeEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		var mismatch *json.UnmarshalTypeError
		if !errors.As(err, &mismatch) {
			return openCodeEvent{}, false
		}
	}
	return ev, true
}

// openCodeEvents decodes one read of the stream into the answer and a
// readable transcript.
type openCodeEvents struct {
	answers    []string
	transcript transcriptLines
	errText    string
	sawEvent   bool
	sawLine    bool

	// plain is the output while no event has been seen: an older CLI, or a
	// run captured before the format flag, printed its answer as text, and
	// the whole of it is the answer then. Held to [sandbox.MaxFileBytes],
	// the bound the whole read it replaced held it to, with what lies past
	// it counted rather than kept.
	plain        strings.Builder
	plainDropped int64

	// following is a decoder read through Entries, which keeps neither
	// the answer nor the plain output a Result would need: a live reading
	// holds it for as long as somebody watches.
	following bool
}

// Entries implements [Decoder].
func (d *openCodeEvents) Entries() []string {
	d.following = true
	d.answers = nil
	d.plain.Reset()
	return d.transcript.take()
}

func (d *openCodeEvents) Line(line []byte) {
	if len(bytes.TrimSpace(line)) == 0 {
		return
	}
	d.sawLine = true
	ev, ok := decodeOpenCodeEvent(line)
	if !ok {
		if !d.sawEvent {
			d.keepPlain(line)
		}
		return
	}
	if !d.sawEvent {
		// The first event: whatever plain text came before it was a banner,
		// not an answer.
		d.sawEvent = true
		d.plain.Reset()
		d.plainDropped = 0
	}
	switch ev.Type {
	case "text":
		if chunk := strings.TrimSpace(ev.Part.Text); chunk != "" {
			if !d.following {
				d.answers = append(d.answers, chunk)
			}
			d.transcript.add(chunk)
		}
	case "tool_use":
		d.transcript.add(toolLine(ev))
	case "error":
		d.errText = errorText(ev.Error)
		d.transcript.add("[error] " + d.errText)
	}
}

func (d *openCodeEvents) keepPlain(line []byte) {
	if d.following {
		return
	}
	if d.plainDropped > 0 || d.plain.Len()+len(line)+1 > sandbox.MaxFileBytes {
		d.plainDropped += int64(len(line) + 1)
		return
	}
	d.plain.Write(line)
	d.plain.WriteByte('\n')
}

func (d *openCodeEvents) Skipped(n int64) {
	d.sawLine = true
	if !d.sawEvent {
		d.plainDropped += n
		return
	}
	d.transcript.skip(n)
}

func (d *openCodeEvents) Result() sandbox.Result {
	if !d.sawLine {
		return sandbox.Result{Error: "the coding agent produced no output"}
	}
	if !d.sawEvent {
		// Non-JSON output — an older CLI, or a run captured before the
		// format flag. The whole of it is the answer.
		text := strings.TrimSpace(d.plain.String())
		if d.plainDropped > 0 {
			text += fmt.Sprintf("\n(%s more of the output not read: past the %s a run's plain "+
				"output is read to)", humanSize(d.plainDropped), humanSize(sandbox.MaxFileBytes))
		}
		return sandbox.Result{Text: text, Success: true, DeliveredRefs: prPattern.FindAllString(text, -1)}
	}
	body := strings.TrimSpace(strings.Join(d.answers, "\n"))
	success := body != "" && d.errText == ""
	res := sandbox.Result{
		Text:          body,
		Success:       success,
		Transcript:    d.transcript.String(),
		DeliveredRefs: prPattern.FindAllString(body, -1),
	}
	if !success {
		res.Error = d.errText
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
// across versions, so every lookup falls back to the name. NEVER the tool's
// output: that is the file a read returned or everything a command printed,
// which is what the transcript is a summary of — only a failed call's first
// line, which says why it failed.
func toolLine(ev openCodeEvent) string {
	name := firstNonBlank(ev.Part.Tool, ev.Tool, ev.Name)
	if name == "" {
		name = "tool"
	}
	input := ev.Part.State.Input
	if input == nil {
		input = ev.Part.Input
	}
	if input == nil {
		input = &openCodeInput{}
	}

	line := "[tool] " + name
	if detail := firstNonBlank(
		input.Command, input.FilePath, input.Path, input.Pattern, input.Description,
	); detail != "" {
		line += ": " + firstLine(detail, transcriptDetailLimit)
	}

	state := ev.Part.State
	failure := firstNonBlank(state.Error)
	if failure == "" && state.Status == "error" {
		failure = firstNonBlank(state.Output)
	}
	if state.Status == "error" || failure != "" {
		if failure == "" {
			failure = "failed"
		}
		line += " → error: " + firstLine(failure, transcriptDetailLimit)
	}
	return line
}

func errorText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	// An object: its JSON, compacted, which is how the generic decode this
	// replaced rendered it.
	var compacted bytes.Buffer
	if json.Compact(&compacted, raw) == nil {
		return compacted.String()
	}
	return string(raw)
}

// firstNonBlank is the first value that is not blank.
func firstNonBlank(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// lineFunc is a [Decoder] that only looks at lines — for a question asked of
// a stream's end, where nothing is built.
type lineFunc func(line []byte)

func (f lineFunc) Line(line []byte)     { f(line) }
func (lineFunc) Skipped(int64)          {}
func (lineFunc) Result() sandbox.Result { return sandbox.Result{} }
func (lineFunc) Entries() []string      { return nil }

// firstLine is one transcript line's echoed command or path, bounded — a
// PREVIEW, for a person scanning what the run did, and marked as one.
//
// The bound is real — a heredoc echoed whole would make one entry of a
// line-structured log read as many — and every cut says so: the line itself
// with an ellipsis, and the lines after it with a count. Only the first was
// ever marked, so a heredoc read as the one command on its first line. Two
// older defects: `line[:limit-1] + "…"` emitted limit+2 BYTES (limit-1 of
// content plus a three-byte ellipsis), so the constant bounded nothing it
// named; and the byte slice split whatever multi-byte character straddled the
// cut, which reaches the event store as invalid UTF-8.
//
// REDACTED WHOLE BEFORE IT IS CUT, and here because this is the one place
// every preview cut goes through — a tool's subject, a failed call's error and
// a run's ending, for both decoders and so for the record and the live view
// alike. The transcript is redacted again downstream, but by then the cut has
// already happened: a token whose start fell in the last few dozen bytes
// before the limit survived as a fragment shorter than its rule's length floor
// (`ghp_` and 28 of a GitHub token's 36 characters), which no pattern
// recognises and which identifies the token to anybody holding its checksum.
// The whole value, not just its first line, because a private key's block
// runs from a BEGIN line on the first line into the lines below it.
func firstLine(s string, limit int) string {
	line, rest, more := strings.Cut(strings.TrimSpace(redact.Secrets(s)), "\n")
	suffix := ""
	if more {
		suffix = fmt.Sprintf(" (+%d more line(s))", strings.Count(rest, "\n")+1)
	}
	if limit <= 0 || len(line)+len(suffix) <= limit {
		return line + suffix
	}
	// [textcut.Within] rather than Ellipsis: that one does not count its
	// marker against max, and this limit bounds what reaches the phase
	// event, markers included.
	return textcut.Within(line, max(limit-len(suffix), 0)) + suffix
}
