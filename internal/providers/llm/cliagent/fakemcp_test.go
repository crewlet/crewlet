package cliagent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeMCPEnv carries a [fakeScript] to the fake CLI: the variable that makes
// this test binary, re-executed, play Claude Code with tools — an MCP client
// that starts the relay its configuration names, lists the seat's tools, plays
// a scripted model against them over the real socket, and prints the
// stream-json record Claude Code prints, partial messages included.
//
// THE RECORD IS CLAUDE CODE'S SHAPE, measured on 2.1.310 (see the profile), so
// the shipped claude-code profile's requests block reads it unchanged: the
// suite drives the profile operators run, with only the binary replaced.
const fakeMCPEnv = "FAKE_MCP_SCRIPT"

// fakeScript is what one fake CLI run plays.
type fakeScript struct {
	// Responses are the model's, in order: each one's prose, then its calls.
	Responses []fakeResponse `json:"responses"`

	// Model names the model in the record.
	Model string `json:"model,omitempty"`

	// NoConnect never starts the relay: a CLI whose server configuration
	// did not take.
	NoConnect bool `json:"no_connect,omitempty"`

	// Exit is the exit status once the script ends; Result the final
	// result event's text; IsError and Subtype the event's flags.
	Exit    int    `json:"exit,omitempty"`
	Result  string `json:"result,omitempty"`
	IsError bool   `json:"is_error,omitempty"`
	Subtype string `json:"subtype,omitempty"`
	// NoResult prints no result event at all: a CLI stopped before its end.
	NoResult bool `json:"no_result,omitempty"`
	// ResultUsage, when set, is the result event's usage in place of the
	// sum of the responses'.
	ResultUsage *fakeUsage `json:"result_usage,omitempty"`

	// Stderr is written on stderr before the run ends.
	Stderr string `json:"stderr,omitempty"`

	// Hang holds the process after the responses until it is signalled:
	// a CLI whose run the host has to end. HangUnsignalled ignores SIGTERM.
	Hang            bool `json:"hang,omitempty"`
	HangUnsignalled bool `json:"hang_unsignalled,omitempty"`

	// PromptTo writes the prompt the fake read on stdin to this file.
	PromptTo string `json:"prompt_to,omitempty"`

	// TextAnswer is what a run with no tools — a text function on the same
	// entry — answers, as Claude Code's result event.
	TextAnswer string `json:"text_answer,omitempty"`

	// RecordFile writes the record — every event but the result — to this
	// file in the working directory instead of stdout: a CLI that keeps its
	// record in a file of its own.
	RecordFile string `json:"record_file,omitempty"`
}

// fakeResponse is one model response.
type fakeResponse struct {
	Text  string     `json:"text,omitempty"`
	Calls []fakeCall `json:"calls,omitempty"`
	Usage fakeUsage  `json:"usage"`
	// StopEarly ends the stream after message_start and the text, with no
	// message_delta: a response cut off mid-way.
	StopEarly bool `json:"stop_early,omitempty"`
	// SilentMS sleeps before the response, with no call in flight: the
	// model thinking.
	SilentMS int `json:"silent_ms,omitempty"`
	// RefreshLogin writes this into the home's login file before the
	// response's calls are made: a CLI that refreshed its token mid-run.
	RefreshLogin string `json:"refresh_login,omitempty"`
}

// fakeCall is one call a response makes.
type fakeCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args,omitempty"`
	// Own marks a call to one of the CLI's own tools, which never reaches
	// the server.
	Own bool `json:"own,omitempty"`
}

type fakeUsage struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	CacheRead  int `json:"cache_read,omitempty"`
	CacheWrite int `json:"cache_write,omitempty"`
}

// runFakeMCPCLI is the fake. It never returns.
func runFakeMCPCLI() {
	var script fakeScript
	if err := json.Unmarshal([]byte(os.Getenv(fakeMCPEnv)), &script); err != nil {
		fmt.Fprintln(os.Stderr, "fake: bad script:", err)
		os.Exit(70)
	}
	prompt, _ := io.ReadAll(os.Stdin)
	if script.PromptTo != "" {
		_ = os.WriteFile(script.PromptTo, prompt, 0o600)
	}
	out := json.NewEncoder(os.Stdout)
	record := out
	if script.RecordFile != "" {
		f, err := os.Create(script.RecordFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "fake: record:", err)
			os.Exit(73)
		}
		defer f.Close()
		record = json.NewEncoder(f)
	}
	emit := func(v map[string]any) {
		if v["type"] == "result" {
			_ = out.Encode(v)
			return
		}
		_ = record.Encode(v)
	}
	model := script.Model
	if model == "" {
		model = "claude-test-1"
	}

	config := argAfter("--mcp-config")
	if config == "" {
		// A TEXT FUNCTION: no server, one answer.
		emit(map[string]any{"type": "result", "subtype": "success", "is_error": false,
			"result": script.TextAnswer, "usage": map[string]any{"input_tokens": 11, "output_tokens": 3}})
		os.Exit(0)
	}
	var cs *sdk.ClientSession
	tools := []string{"WebFetch", "WebSearch"}
	if !script.NoConnect {
		var err error
		cs, err = connectFake(config)
		if err != nil {
			fmt.Fprintln(os.Stderr, "fake: connect:", err)
			os.Exit(71)
		}
		defer cs.Close()
		listed, err := cs.ListTools(context.Background(), nil)
		if err != nil {
			fmt.Fprintln(os.Stderr, "fake: list:", err)
			os.Exit(72)
		}
		for _, tool := range listed.Tools {
			tools = append(tools, "mcp__crewlet__"+tool.Name)
		}
	}
	emit(map[string]any{"type": "system", "subtype": "init", "tools": tools, "model": model})

	var total fakeUsage
	last := ""
	for i, r := range script.Responses {
		if r.SilentMS > 0 {
			time.Sleep(time.Duration(r.SilentMS) * time.Millisecond)
		}
		id := fmt.Sprintf("msg_%d", i+1)
		total.Input += r.Usage.Input
		total.Output += r.Usage.Output
		total.CacheRead += r.Usage.CacheRead
		total.CacheWrite += r.Usage.CacheWrite
		emit(streamEvent(map[string]any{"type": "message_start", "message": map[string]any{
			"id": id, "model": model, "usage": map[string]any{
				"input_tokens": r.Usage.Input, "output_tokens": 1,
				"cache_read_input_tokens": r.Usage.CacheRead, "cache_creation_input_tokens": r.Usage.CacheWrite,
			}}}))
		if r.Text != "" {
			emit(streamEvent(map[string]any{"type": "content_block_delta", "index": 0,
				"delta": map[string]any{"type": "text_delta", "text": r.Text}}))
			emit(assistantEvent(id, model, map[string]any{"type": "text", "text": r.Text}))
			last = r.Text
		}
		if r.StopEarly {
			break
		}
		ids := make([]string, len(r.Calls))
		for j, c := range r.Calls {
			ids[j] = fmt.Sprintf("toolu_%d_%d", i+1, j)
			name := "mcp__crewlet__" + c.Name
			if c.Own {
				name = c.Name
			}
			emit(assistantEvent(id, model, map[string]any{"type": "tool_use", "id": ids[j],
				"name": name, "input": orEmpty(c.Args)}))
		}
		stop := "end_turn"
		if len(r.Calls) > 0 {
			stop = "tool_use"
		}
		// The assistant events undercount output, as Claude Code's do with
		// partial messages: the true count is message_delta's.
		emit(streamEvent(map[string]any{"type": "message_delta",
			"delta": map[string]any{"stop_reason": stop}, "usage": map[string]any{"output_tokens": r.Usage.Output}}))
		emit(streamEvent(map[string]any{"type": "message_stop"}))
		if r.RefreshLogin != "" {
			path := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), ".credentials.json")
			_ = os.WriteFile(path, []byte(r.RefreshLogin), 0o600)
		}
		for j, c := range r.Calls {
			if c.Own || cs == nil {
				continue
			}
			raw, _ := json.Marshal(orEmpty(c.Args))
			res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{
				Name: c.Name, Arguments: json.RawMessage(raw),
				Meta: sdk.Meta{"claudecode/toolUseId": ids[j]},
			})
			result := "error: " + fmt.Sprint(err)
			if err == nil {
				result = resultText(res)
			}
			emit(map[string]any{"type": "user", "message": map[string]any{"role": "user",
				"content": []any{map[string]any{"type": "tool_result", "tool_use_id": ids[j], "content": result}}}})
		}
	}
	if script.Stderr != "" {
		fmt.Fprint(os.Stderr, script.Stderr)
	}
	if script.Hang || script.HangUnsignalled {
		if script.HangUnsignalled {
			signal.Ignore(syscall.SIGTERM, syscall.SIGINT)
		}
		select {}
	}
	if !script.NoResult {
		usage := total
		if script.ResultUsage != nil {
			usage = *script.ResultUsage
		}
		subtype := script.Subtype
		if subtype == "" {
			subtype = "success"
		}
		result := script.Result
		if result == "" {
			result = last
		}
		emit(map[string]any{"type": "result", "subtype": subtype, "is_error": script.IsError,
			"result": result, "usage": map[string]any{
				"input_tokens": usage.Input, "output_tokens": usage.Output,
				"cache_read_input_tokens": usage.CacheRead, "cache_creation_input_tokens": usage.CacheWrite,
			}})
	}
	os.Exit(script.Exit)
}

// connectFake starts the server its configuration names — the relay — and
// connects to it, as Claude Code does with an `--mcp-config` file.
func connectFake(path string) (*sdk.ClientSession, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // the fake's own configuration
	if err != nil {
		return nil, err
	}
	var config struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		return nil, err
	}
	server, ok := config.MCPServers["crewlet"]
	if !ok {
		return nil, fmt.Errorf("no crewlet server in %s", path)
	}
	client := sdk.NewClient(&sdk.Implementation{Name: "fake-claude", Version: "1"}, nil)
	//nolint:gosec // the relay the engine named
	return client.Connect(context.Background(),
		&sdk.CommandTransport{Command: exec.Command(server.Command, server.Args...)}, nil)
}

func argAfter(flag string) string {
	for i, arg := range os.Args {
		if arg == flag && i+1 < len(os.Args) {
			return os.Args[i+1]
		}
	}
	return ""
}

func streamEvent(event map[string]any) map[string]any {
	return map[string]any{"type": "stream_event", "event": event, "parent_tool_use_id": nil}
}

func assistantEvent(id, model string, block map[string]any) map[string]any {
	return map[string]any{"type": "assistant", "parent_tool_use_id": nil, "message": map[string]any{
		"id": id, "model": model, "role": "assistant", "content": []any{block},
		"usage": map[string]any{"input_tokens": 1, "output_tokens": 1},
	}}
}

func resultText(res *sdk.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func orEmpty(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}
