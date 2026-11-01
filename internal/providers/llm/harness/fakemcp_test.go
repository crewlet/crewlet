package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeMCPEnv carries a [fakeScript] to the fake harness: the variable that makes
// this test binary, re-executed, play Claude Code with tools — an MCP client
// that starts the relay its configuration names, lists the seat's tools, plays
// a scripted model against them over the real socket, and prints the
// stream-json record Claude Code prints, partial messages included.
//
// THE RECORD IS CLAUDE CODE'S SHAPE, measured on 2.1.310 (see the profile), so
// the shipped claude-code profile's requests block reads it unchanged: the
// suite drives the profile operators run, with only the binary replaced.
const fakeMCPEnv = "FAKE_MCP_SCRIPT"

// fakeScript is what one fake harness run plays.
type fakeScript struct {
	// Responses are the model's, in order: each one's prose, then its calls.
	Responses []fakeResponse `json:"responses"`

	// Model names the model in the record.
	Model string `json:"model,omitempty"`

	// NoConnect never starts the relay: a harness whose server configuration
	// did not take.
	NoConnect bool `json:"no_connect,omitempty"`

	// Exit is the exit status once the script ends; Result the final
	// result event's text; IsError and Subtype the event's flags.
	Exit    int    `json:"exit,omitempty"`
	Result  string `json:"result,omitempty"`
	IsError bool   `json:"is_error,omitempty"`
	Subtype string `json:"subtype,omitempty"`
	// NoResult prints no result event at all: a harness stopped before its end.
	NoResult bool `json:"no_result,omitempty"`
	// ResultUsage, when set, is the result event's usage in place of the
	// sum of the responses'.
	ResultUsage *fakeUsage `json:"result_usage,omitempty"`

	// Stderr is written on stderr before the run ends.
	Stderr string `json:"stderr,omitempty"`

	// Hang holds the process after the responses until it is signalled:
	// a harness whose run the host has to end. HangUnsignalled ignores SIGTERM.
	Hang            bool `json:"hang,omitempty"`
	HangUnsignalled bool `json:"hang_unsignalled,omitempty"`

	// PromptTo writes the prompt the fake read on stdin to this file.
	PromptTo string `json:"prompt_to,omitempty"`

	// TextAnswer is what a run with no tools — a text function on the same
	// entry — answers, as Claude Code's result event.
	TextAnswer string `json:"text_answer,omitempty"`

	// RecordFile writes the record — every event but the result — to this
	// file in the working directory instead of stdout: a harness that keeps its
	// record in a file of its own.
	RecordFile string `json:"record_file,omitempty"`

	// Serve is the serve lifecycle's fake server: see [runFakeServer].
	Serve fakeServe `json:"serve,omitzero"`

	// Harness plays another Harness than Claude Code, by its profile's name — see
	// [fakeHarnesses].
	Harness string `json:"harness,omitempty"`

	// ForeignRecord writes, before the run's own, a record of ANOTHER run
	// where this harness keeps its records — the seat's home, which every
	// concurrent call of the seat shares — its one response saying FOREIGN.
	ForeignRecord bool `json:"foreign_record,omitempty"`
}

// fakeServe is how the fake plays a harness whose loop runs in a server of its own,
// as OpenCode 2.1's does: `serve` starts the server, which prints its address
// and a password, connects to the crewlet server and plays the script once a
// run hands it the prompt; `api … mcp.list` polls it; `run --server <url>` is
// a run, its client, printing what the server's loop records; `report` asks it
// for the run's usage. They meet in files in the call's working directory.
type fakeServe struct {
	// ConnectDelayMS holds the server's connection to the crewlet server
	// that long after it is up, so a run started before the poll says so
	// finds it unconnected — and refuses to go on, as the real one goes on
	// with no tools.
	ConnectDelayMS int `json:"connect_delay_ms,omitempty"`
	// ExitBeforeUp exits 9 before printing anything.
	ExitBeforeUp bool `json:"exit_before_up,omitempty"`
	// NeverUp prints its address and never its password.
	NeverUp bool `json:"never_up,omitempty"`
	// ExitAfter exits 9 once that many responses have been played.
	ExitAfter int `json:"exit_after,omitempty"`
	// PidTo writes the server's pid here, outside the working directory the
	// call's release removes.
	PidTo string `json:"pid_to,omitempty"`
	// ReportUsage is what `report` prints as the run's usage.
	ReportUsage *fakeUsage `json:"report_usage,omitempty"`
	// ReportTo writes the arguments and the password `report` was run with.
	ReportTo string `json:"report_to,omitempty"`
	// Say is written on the server's stderr once it is up.
	Say string `json:"say,omitempty"`
}

// The files the serve lifecycle's fake roles meet in, in the working directory.
const (
	fakeServerFile    = ".fake-server"
	fakeConnectedFile = ".fake-connected"
	fakePolledFile    = ".fake-polled"
	fakeGoFile        = ".fake-go"
	fakeEventsFile    = ".fake-events.jsonl"
	fakeDoneFile      = ".fake-done"
)

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
	// response's calls are made: a harness that refreshed its token mid-run.
	RefreshLogin string `json:"refresh_login,omitempty"`
}

// fakeCall is one call a response makes.
type fakeCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args,omitempty"`
	// Own marks a call to one of the harness's own tools, which never reaches
	// the server.
	Own bool `json:"own,omitempty"`
	// Asked is the name the model wrote, where the harness repaired it to Name
	// before making the call — Hermes Agent's fuzzy repair of an unknown
	// name. A recorder that shows what the model asked writes it.
	Asked string `json:"asked,omitempty"`
}

type fakeUsage struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	CacheRead  int `json:"cache_read,omitempty"`
	CacheWrite int `json:"cache_write,omitempty"`
}

// runFakeMCPHarness is the fake. It never returns.
func runFakeMCPHarness() {
	var script fakeScript
	if err := json.Unmarshal([]byte(os.Getenv(fakeMCPEnv)), &script); err != nil {
		fmt.Fprintln(os.Stderr, "fake: bad script:", err)
		os.Exit(70)
	}
	if script.Harness != "" {
		runFakeOtherHarness(script)
	}
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "serve":
			runFakeServer(script)
		case "api":
			runFakePoll()
		case "run":
			runFakeClient()
		case "report":
			runFakeReport(script)
		}
	}
	config := argAfter("--mcp-config")
	if config == "" {
		// A TEXT FUNCTION: no server, one answer.
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"type": "result", "subtype": "success",
			"is_error": false, "result": script.TextAnswer,
			"usage": map[string]any{"input_tokens": 11, "output_tokens": 3}})
		os.Exit(0)
	}
	prompt, _ := io.ReadAll(os.Stdin)
	os.Exit(playFake(script, prompt, os.Stdout, connectOrExit(script, config)))
}

// connectOrExit connects to the server the configuration names, or nil for a
// script that never does.
func connectOrExit(script fakeScript, config string) *sdk.ClientSession {
	if script.NoConnect {
		return nil
	}
	cs, err := connectFake(config)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake: connect:", err)
		os.Exit(71)
	}
	return cs
}

// playFake plays the script against the server cs connects to — none for nil —
// printing the record on stdout, and returns the exit status.
func playFake(script fakeScript, prompt []byte, stdout io.Writer, cs *sdk.ClientSession) int {
	if script.PromptTo != "" {
		_ = os.WriteFile(script.PromptTo, prompt, 0o600)
	}
	out := json.NewEncoder(stdout)
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

	tools := []string{"WebFetch", "WebSearch"}
	if cs != nil {
		defer cs.Close()
		listed, err := cs.ListTools(context.Background(), nil)
		if err != nil {
			fmt.Fprintln(os.Stderr, "fake: list:", err)
			return 72
		}
		for _, tool := range listed.Tools {
			tools = append(tools, "mcp__crewlet__"+tool.Name)
		}
	}
	emit(map[string]any{"type": "system", "subtype": "init", "tools": tools, "model": model})

	var total fakeUsage
	last := ""
	for i, r := range script.Responses {
		if script.Serve.ExitAfter > 0 && i == script.Serve.ExitAfter {
			fmt.Fprintln(os.Stderr, "fake server: lost its footing")
			os.Exit(9)
		}
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
		park(script.HangUnsignalled)
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
	return script.Exit
}

// runFakeServer is the serve lifecycle's server: up, connected to the crewlet
// server, then the script played once a run hands it the prompt, its record
// written for the run to print. It lives until it is stopped.
func runFakeServer(script fakeScript) {
	srv := script.Serve
	if srv.PidTo != "" {
		_ = os.WriteFile(srv.PidTo, []byte(strconv.Itoa(os.Getpid())), 0o600)
	}
	fmt.Fprintln(os.Stderr, "fake server: starting")
	if srv.ExitBeforeUp {
		fmt.Fprintln(os.Stderr, "fake server: the port is taken")
		os.Exit(9)
	}
	password := fmt.Sprintf("pw-%d", os.Getpid())
	// A log line first, which is no ready value.
	fmt.Println("INFO loading configuration")
	fmt.Printf("server listening on http://fake-%d\n", os.Getpid())
	if srv.NeverUp {
		park(false)
	}
	_ = os.WriteFile(fakeServerFile, []byte(fmt.Sprintf("http://fake-%d %s", os.Getpid(), password)), 0o600)
	fmt.Printf("server password %s\n", password)
	if srv.Say != "" {
		fmt.Fprintln(os.Stderr, srv.Say)
	}
	time.Sleep(time.Duration(srv.ConnectDelayMS) * time.Millisecond)
	cs := connectOrExit(script, argAfter("--mcp-config"))
	_ = os.WriteFile(fakeConnectedFile, nil, 0o600)
	var prompt []byte
	for {
		raw, err := os.ReadFile(fakeGoFile)
		if err == nil {
			prompt = raw
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	f, err := os.Create(fakeEventsFile)
	if err != nil {
		os.Exit(73)
	}
	code := playFake(script, prompt, f, cs)
	_ = f.Close()
	_ = os.WriteFile(fakeDoneFile, []byte(strconv.Itoa(code)), 0o600)
	park(false)
}

// park holds a fake until it is signalled, as the process it stands in for
// waits to be stopped, and lets the signal end it under its default action;
// ignoring holds it through SIGTERM and SIGINT until it is killed. A bare
// select{} is no such wait: once the fake has no connection open and no timer
// pending, the runtime ends it with "all goroutines are asleep - deadlock!" and
// exit status 2, at a moment its own timers decide, so a fake meant to be
// stopped sometimes exits by itself. A process watching for a signal is never
// deadlocked.
func park(ignoring bool) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	for got := range sig {
		if ignoring {
			continue
		}
		signal.Reset(got)
		_ = syscall.Kill(os.Getpid(), got.(syscall.Signal))
	}
}

// fakeServerOrExit checks a client was handed the server's address and its
// password, exiting 76 when it was not.
func fakeServerOrExit() {
	raw, err := os.ReadFile(fakeServerFile)
	want := strings.Fields(string(raw))
	if err != nil || len(want) != 2 || argAfter("--server") != want[0] ||
		os.Getenv("FAKE_SERVER_PASSWORD") != want[1] {
		fmt.Fprintf(os.Stderr, "fake: not this server's client: --server %q\n", argAfter("--server"))
		os.Exit(76)
	}
}

// runFakePoll lists the server's MCP servers: none at first, as OpenCode's
// first answer does, then the crewlet server, pending until it connects.
func runFakePoll() {
	fakeServerOrExit()
	if _, err := os.Stat(fakePolledFile); err != nil {
		_ = os.WriteFile(fakePolledFile, nil, 0o600)
		fmt.Println(`{"data":[]}`)
		os.Exit(0)
	}
	status := "pending"
	if _, err := os.Stat(fakeConnectedFile); err == nil {
		status = "connected"
	}
	fmt.Printf(`{"data":[{"name":"other","status":{"status":"connected"}},`+
		`{"name":"crewlet","status":{"status":%q}}]}`+"\n", status)
	os.Exit(0)
}

// runFakeClient is a run against the server: it hands it the prompt and prints
// the server's record as the server writes it, exiting as the server's loop
// did. Started before the server connected to the crewlet server, it refuses —
// where the real harness goes on, with none of the seat's tools.
func runFakeClient() {
	fakeServerOrExit()
	if _, err := os.Stat(fakeConnectedFile); err != nil {
		fmt.Fprintln(os.Stderr, "fake: asked before the server connected to its tools")
		os.Exit(75)
	}
	prompt, _ := io.ReadAll(os.Stdin)
	_ = os.WriteFile(fakeGoFile+".tmp", prompt, 0o600)
	_ = os.Rename(fakeGoFile+".tmp", fakeGoFile)
	// Signalled, it prints what the server recorded so far and exits, as a
	// client does: its record is what it relayed, and polling leaves it a
	// beat behind the server's.
	term := make(chan os.Signal, 1)
	signal.Notify(term, syscall.SIGTERM)
	offset := 0
	relay := func() {
		data, _ := os.ReadFile(fakeEventsFile)
		if len(data) > offset {
			_, _ = os.Stdout.Write(data[offset:])
			offset = len(data)
		}
	}
	for {
		done, doneErr := os.ReadFile(fakeDoneFile)
		relay()
		if doneErr == nil {
			code, _ := strconv.Atoi(string(done))
			os.Exit(code)
		}
		select {
		case <-term:
			relay()
			os.Exit(143)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// runFakeReport asks the server for the run's usage, refusing when the server
// is gone.
func runFakeReport(script fakeScript) {
	if argAfter("--server") == "" {
		// A text function's report, which asks no server: its one answer.
		fmt.Println(`{"usage":{"input_tokens":11,"output_tokens":3}}`)
		os.Exit(0)
	}
	fakeServerOrExit()
	if to := script.Serve.ReportTo; to != "" {
		_ = os.WriteFile(to, []byte(strings.Join(os.Args[1:], " ")), 0o600)
	}
	pid, _ := strconv.Atoi(strings.TrimPrefix(argAfter("--server"), "http://fake-"))
	if pid <= 0 || syscall.Kill(pid, 0) != nil {
		fmt.Fprintln(os.Stderr, "fake: connection refused")
		os.Exit(4)
	}
	u := script.Serve.ReportUsage
	if u == nil {
		os.Exit(3)
	}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"usage": map[string]any{
		"input_tokens": u.Input, "output_tokens": u.Output,
	}})
	os.Exit(0)
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
