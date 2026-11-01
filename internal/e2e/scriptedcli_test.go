package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"gopkg.in/yaml.v3"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/procgroup/procgrouptest"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/harness"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
	"github.com/crewlet/crewlet/internal/toolsession"
)

// THE SCRIPTED MODEL'S HARNESS: this test binary, re-executed, being to the engine
// what Claude Code is.
//
// Every model call the engine makes goes through a harness
// (internal/providers/llm/harness), so the golden company's model entry is a
// real one whose harness is this binary ([scriptedEntry]). Re-executed with
// [scriptedHarnessEnv] set, the binary does what Claude Code does in print mode:
// it reads the transcript on stdin and the system prompt from the file its
// channel names; on a call that offers tools it starts the `crewlet` MCP server
// the configuration file on `--mcp-config` names — `crewlet mcp-relay
// <socket>`, which under `go test` is this binary again ([TestMain] dispatches
// the relay first) — lists the seat's tools, and drives its own loop: ask the
// model, make the calls it asks for, ask again with their results, until a
// response calls nothing. Everything it does it prints as Claude Code's
// stream-json record, partial messages included, which is what the engine's
// request reader reads each model request and its cost off.
//
// The MODEL is the [scriptedModel]'s, asked over loopback once per model
// request, because the script — which phase this is, which tool the executor
// calls next — is state in the test process, and a child launched per harness run
// keeps nothing between runs. What the child holds is one run's conversation,
// which a real harness holds too.
//
// What the two halves agree on is everything in this file: the entry that
// launches the child, the one request it posts per model request, how the
// model reads the transcript the backend rendered, and the record the backend
// reads back.

// scriptedHarnessEnv makes a re-execution of this binary the scripted harness rather
// than the suite, and its value is the address of the model that answers.
//
// ONE VARIABLE FOR BOTH, so there is no way to launch a child that is the harness
// and does not know where its model is. It reaches the child through the
// entry's own `env`, which is the only route there is: the backend hands its
// child an allowlisted environment, never the engine's.
const scriptedHarnessEnv = "CREWLET_E2E_SCRIPTED_CLI"

// scriptedSystemFlag and scriptedAppendFlag are Claude Code's two channels for
// a system prompt's file — the first replacing the harness's own prompt, the
// second kept after it — which the claude-code profile the entry takes its
// channels from passes ([claudeCodeRun]). So the system prompt reaches the
// model on its own channel exactly as it does for the default agent, by the
// route the default agent's would take: a seat's phase on the append channel,
// a side pass on the replace one ([harnessprofile.Profile.RouteFor]).
const (
	scriptedSystemFlag = "--system-prompt-file"
	scriptedAppendFlag = "--append-system-prompt-file"
)

// The channels a [modelRequest] names its system prompt arrived on.
const (
	channelReplace = "replace"
	channelAppend  = "append"
)

// What Claude Code does that no profile field says, because the profile READS
// it: the name it shows its model a server's tool under, the `_meta` key it
// hands the server its own id for a call under, and the server it is told of.
// The claude-code profile's requests block reads the record by the first two
// (its host_prefix and call_id_meta), and the round trip below fails by name if
// they ever disagree.
const (
	claudeCodeToolPrefix  = "mcp__crewlet__"
	claudeCodeToolUseMeta = "claudecode/toolUseId"
	claudeCodeServer      = "crewlet"
)

// unnamedModel is the model the record names when the harness was handed none:
// Claude Code then runs its own default and names that, which is never the
// model the entry chose — so a run whose `--model` went missing reads as served
// by some other model, never as the entry's.
const unnamedModel = "the-cli-default-model"

// stoppedExit is the status the scripted harness exits with when the engine stops
// it: a process killed by SIGTERM, as a shell reports one.
const stoppedExit = 128 + int(syscall.SIGTERM)

// scriptedHarnessBinary is this test binary's own path, which the entry names as
// its harness. Set once by [TestMain], before any case runs.
//
// [os.Executable] rather than os.Args[0]: the backend runs its child in the
// call's own working directory, so a binary launched by a relative path —
// `./e2e.test` — would name nothing there.
var scriptedHarnessBinary string

// scriptedShape is the claude-code profile's run, as overrides
// ([claudeCodeRun]), set once by [TestMain] beside [scriptedHarnessBinary].
var scriptedShape map[string]any

// claudeCodeFields are the claude-code profile's fields that shape a run and
// how its record is read — the model and the system prompt on their flags, the
// transcript on stdin, the tool route, the record's requests, its answer, its
// counts and its cap — which the scripted entry takes WHOLE from the shipped
// profile.
//
// TAKEN, NOT RESTATED: a copy written here would go on describing the record
// Claude Code printed when it was copied, and the suite would pass against a
// profile no operator runs. Taken from the table, a change to how the engine
// reads Claude Code reaches every case here, and the scripted harness either
// prints what the changed profile reads or the round trip below fails.
//
// What is NOT taken is everything about a real Claude Code that a scripted one
// has none of: its sign-in, its configuration directory and the switches that
// confine it, its spent-plan sentinels, its complete_args — whose flags the
// scripted harness has no use for, and in whose place the entry passes the
// fork-bomb guard ([scriptedEntry]).
var claudeCodeFields = []string{
	"model_args", "system_prompt", "append_system_prompt", "prompt_mode",
	"output", "event_type_path", "text_events", "text_paths", "error_paths", "usage",
	"mcp", "requests", "cap",
}

// claudeCodeRun is [claudeCodeFields] out of the built-in claude-code profile,
// as an entry's `overrides` takes them, and binary the harness they run.
//
// By the profile's own YAML, which is how [harnessprofile.Load] merges overrides
// into a profile, so what an override carries is what the loader reads.
func claudeCodeRun(binary string) (map[string]any, error) {
	profile, ok := harnessprofile.Builtin("claude-code")
	if !ok {
		return nil, errors.New("this build ships no claude-code profile")
	}
	raw, err := yaml.Marshal(profile)
	if err != nil {
		return nil, fmt.Errorf("encoding the claude-code profile: %w", err)
	}
	var fields map[string]any
	if err := yaml.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("re-reading the claude-code profile: %w", err)
	}
	// complete_args NAMES NO TEST, as a FORK-BOMB GUARD rather than a
	// selector: [TestMain] dispatches on [scriptedHarnessEnv] alone, so a child
	// launched without it falls through to the suite, and a filter matching
	// nothing ends that child at once instead of running every case of the
	// package again, once per call.
	run := map[string]any{"binary": binary, "complete_args": []string{"-test.run=^$"}}
	for _, name := range claudeCodeFields {
		v, ok := fields[name]
		if !ok {
			// A FIELD THE PROFILE DROPPED is a change to how the engine
			// runs Claude Code, and the scripted harness was written against
			// the profile that had it: said here, by name, rather than as
			// every case waiting out its budget for a run read some other
			// way.
			return nil, fmt.Errorf("the claude-code profile no longer sets %s, which the "+
				"scripted harness is written against: read what replaced it, and move "+
				"claudeCodeFields and the scripted harness with it", name)
		}
		run[name] = v
	}
	return run, nil
}

// scriptedEntry is the providers.llm entry, under key, whose harness is this binary
// answering from the model at endpoint and keeping its homes under stateDir —
// indented to sit under `providers: llm:`.
//
// `custom` RATHER THAN claude-code WITH ITS BINARY SWAPPED, because `custom`
// ships nothing: every field the scripted harness is driven by is handed it here,
// and nothing else a vendor's profile carries — its sign-in, its spent-plan
// sentinels, its configuration directory — stands between the engine and a harness
// that has none of them. What it is handed is Claude Code's own run
// ([claudeCodeRun]), so the backend reads this harness on the paths the default
// agent's are read on, and the usage it charges is the figure each response
// names ([replyUsage]), never an estimate a budget case could not reason about.
//
// GORACE is [procgrouptest.StandInRaceOptions]: a race-built child that exits 0
// otherwise sleeps a second on its way out, which every model call of every
// case would pay — the relay the child starts included, which inherits it.
//
// max_concurrent and timeout_seconds are NOT set: a node here runs its model
// calls under the caps a deployment gets by default, and no case is about
// either.
//
// Every scalar is written with %q: a Go-quoted string is a YAML double-quoted
// scalar, every escape strconv.Quote writes being one YAML reads. The overrides
// are YAML's own encoding of [scriptedShape], indented into place.
func scriptedEntry(key, endpoint, stateDir string) string {
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	// A map of strings, lists and the profile's own decoded YAML always
	// encodes.
	_ = enc.Encode(scriptedShape)
	var overrides strings.Builder
	for line := range strings.SplitSeq(strings.TrimRight(b.String(), "\n"), "\n") {
		overrides.WriteString("        " + line + "\n")
	}
	return fmt.Sprintf(`    %s:
      harness: custom
      model: claude-golden
      state_dir: %q
      env:
        %s: %q
        %s: %q
      overrides:
%s`, key, stateDir,
		scriptedHarnessEnv, endpoint,
		procgrouptest.GORACE, procgrouptest.StandInRaceOptions(),
		overrides.String())
}

// --- what the harness asks the model, and what it is answered ------------------ //

// modelRequest is what the scripted harness posts for one model request: the
// system prompt it was handed on its own channel and which channel that was,
// the transcript it read on stdin, the seat's tools it offers its model, and
// the run so far.
type modelRequest struct {
	System string `json:"system"`
	// Channel is [channelReplace] or [channelAppend], or empty where the
	// call carried no system prompt.
	Channel string `json:"channel,omitempty"`
	Prompt  string `json:"prompt"`

	// Tools are the crewlet server's tools, by the server's own names, as
	// the harness last listed them. Empty on a call with no server — a text
	// function — and on a run whose server offered nothing.
	Tools []string `json:"tools,omitempty"`

	// Run is what this harness run did before this request: each response the
	// model gave and what its calls answered, in order — the conversation a
	// real harness sends its model, past the transcript it was started with.
	Run []exchange `json:"run,omitempty"`
}

// exchange is one response of a run and what its calls answered.
type exchange struct {
	Reply   modelReply   `json:"reply"`
	Results []callResult `json:"results,omitempty"`

	// Notes are what an executor's hooks handed its model after this
	// response — a person's notes, after its calls' results or as it was
	// finishing ([codingHooks]). A harness on the engine host is handed none:
	// the engine's own loop puts a note in its conversation.
	Notes []string `json:"notes,omitempty"`
}

// modelReply is the model's answer to one request: its prose, the calls it
// asks for, and what it cost.
type modelReply struct {
	Text  string      `json:"text,omitempty"`
	Calls []modelCall `json:"calls,omitempty"`
	Usage replyUsage  `json:"usage"`
}

// modelCall is one call a response asks for, by the server's name for the
// tool.
type modelCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args,omitempty"`
}

// callResult is what one call answered, as the harness hands it its model.
type callResult struct {
	Name    string `json:"name"`
	Content string `json:"content"`
	Failed  bool   `json:"failed,omitempty"`
}

// offers is the request's tools as a set.
func (r modelRequest) offers() map[string]bool {
	out := make(map[string]bool, len(r.Tools))
	for _, name := range r.Tools {
		out[name] = true
	}
	return out
}

// toolResults is every tool result the request hands the model, in order: the
// earlier runs' the transcript carries ([toolResultsIn]), then this run's.
func (r modelRequest) toolResults() []string {
	out := toolResultsIn(r.Prompt)
	for _, ex := range r.Run {
		for _, res := range ex.Results {
			out = append(out, res.Content)
		}
	}
	return out
}

// --- the harness ---------------------------------------------------------------- //

// harnessRun is one run of the scripted harness.
type harnessRun struct {
	endpoint string
	model    string
	out      *json.Encoder

	// turn is held from a request's post to the model until its record is
	// printed, and the stop waits for it: see [harnessRun.handleStop].
	turn     sync.Mutex
	stopping atomic.Bool

	// stale is set when the server says its tool list changed, and the
	// list is read again before the next request.
	stale atomic.Bool

	total replyUsage
	last  string
}

// runScriptedHarness is the scripted harness. It returns the exit status.
//
// A FAILURE IS THE HARNESS'S OWN, on stderr with a non-zero exit, which the
// backend reports as the harness failing — the reason reaches the engine's log
// rather than a wait in some case timing out with nothing said.
func runScriptedHarness(endpoint string) int {
	run := &harnessRun{endpoint: endpoint, model: argAfter("--model"), out: json.NewEncoder(os.Stdout)}
	if run.model == "" {
		run.model = unnamedModel
	}
	run.handleStop()
	prompt, err := io.ReadAll(os.Stdin)
	if err != nil {
		return failHarness("reading the prompt: %v", err)
	}
	system, channel, err := systemPrompt()
	if err != nil {
		return failHarness("%v", err)
	}
	req := modelRequest{System: system, Channel: channel, Prompt: string(prompt)}

	config := argAfter("--mcp-config")
	if config == "" {
		// A TEXT FUNCTION: no server, one request, its answer the result.
		reply, _, err := run.respond(req)
		if err != nil {
			return failHarness("%v", err)
		}
		run.last = reply.Text
		run.result()
		return 0
	}
	return run.loop(req, config)
}

// loop is a run with tools: the server its configuration names started and
// listed, then the model asked, its calls made and their results handed back,
// until a response calls nothing.
func (c *harnessRun) loop(req modelRequest, config string) int {
	cs, err := c.connect(config)
	if err != nil {
		return failHarness("starting the %s MCP server %s names: %v", claudeCodeServer, config, err)
	}
	// Closing the session closes the relay's stdin, which the engine reads as
	// the harness being done: it answers what is in flight and closes its end.
	defer cs.Close()
	if req.Tools, err = listTools(cs); err != nil {
		return failHarness("listing the %s server's tools: %v", claudeCodeServer, err)
	}
	offered := []string{"WebFetch", "WebSearch"}
	for _, name := range req.Tools {
		offered = append(offered, claudeCodeToolPrefix+name)
	}
	c.emit(map[string]any{"type": "system", "subtype": "init", "model": c.model, "tools": offered,
		"mcp_servers": []any{map[string]any{"name": claudeCodeServer, "status": "connected"}}})

	for {
		if c.stale.Swap(false) {
			if req.Tools, err = listTools(cs); err != nil {
				return failHarness("listing the %s server's tools again: %v", claudeCodeServer, err)
			}
		}
		reply, ids, err := c.respond(req)
		if err != nil {
			return failHarness("%v", err)
		}
		c.last = reply.Text
		if len(reply.Calls) == 0 {
			c.result()
			return 0
		}
		// ONE AT A TIME, IN ORDER — Claude Code's own rule for every call
		// that is not to a read-only tool, and the scripted model asks for
		// one call per response, so no response here has two to put beside
		// each other.
		ex := exchange{Reply: reply}
		for i, call := range reply.Calls {
			res, err := c.call(cs, call, ids[i])
			if err != nil {
				if c.stopping.Load() {
					// The stop took the relay down with this process's
					// group: the run is over, and nothing failed.
					os.Exit(stoppedExit)
				}
				// THE SESSION IS GONE under a call: the engine retired the
				// run, or its end failed. A harness whose server died has no
				// tools to go on with, and asking its model again would be
				// a request the engine never sees the end of.
				return failHarness("calling %s: %v", call.Name, err)
			}
			ex.Results = append(ex.Results, res)
		}
		req.Run = append(req.Run, ex)
	}
}

// handleStop makes the engine's stop end the run AT A REQUEST'S EDGE.
//
// The backend stops a run it is done with by signalling the harness's group, and a
// real Claude Code goes at once. What a real one cannot do is be asked by its
// vendor for a response it never prints, because the vendor's answer and its
// record are one stream. Here they are two steps — the post to the model, then
// the printing — and a stop landing between them would leave a request the
// model answered and the engine never read, which every case that holds the
// counter to the model's log ("every completion the model answered is on the
// counter, once") would wait on for ever.
//
// So the stop waits for a request in flight to be printed, and no request is
// posted once it has come. Everywhere else — a call the engine is holding its
// answer to, above all — it ends the run at once.
func (c *harnessRun) handleStop() {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-signals
		c.stopping.Store(true)
		// SAID, as a harness says it was signalled, and BEFORE the wait: a case
		// holding the stop to the request in flight reads this line to know
		// the stop is in hand rather than racing it.
		fmt.Fprintln(os.Stderr, stopNotice)
		c.turn.Lock()
		os.Exit(stoppedExit)
	}()
}

// stopNotice is what the scripted harness says on stderr when it is stopped.
const stopNotice = "scripted harness: stopped; ending at the edge of the request in flight"

// respond asks the model for its next response and prints it as Claude Code's
// record of one request, as one step a stop cannot split ([harnessRun.handleStop]),
// returning the ids it gave the response's calls.
func (c *harnessRun) respond(req modelRequest) (modelReply, []string, error) {
	c.turn.Lock()
	defer c.turn.Unlock()
	if c.stopping.Load() {
		os.Exit(stoppedExit)
	}
	reply, err := c.ask(req)
	if err != nil {
		return modelReply{}, nil, err
	}
	ids := make([]string, len(reply.Calls))
	for i := range ids {
		ids[i] = claudeID("toolu_")
	}
	c.record(reply, ids)
	return reply, ids, nil
}

// ask posts one request to the model and reads its reply.
//
// NO DEADLINE OF ITS OWN: the request is bounded by the run's, since the
// backend stops this whole process when its clock runs out.
func (c *harnessRun) ask(req modelRequest) (modelReply, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return modelReply{}, fmt.Errorf("encoding the request: %w", err)
	}
	resp, err := httpx.Client(0).Post(c.endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		return modelReply{}, fmt.Errorf("asking the model: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return modelReply{}, fmt.Errorf("reading the model's answer: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return modelReply{}, fmt.Errorf("the model answered %d: %s", resp.StatusCode, bytes.TrimSpace(raw))
	}
	var reply modelReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		return modelReply{}, fmt.Errorf("the model's answer does not decode: %w", err)
	}
	return reply, nil
}

// record prints one response as Claude Code's stream-json record of a request
// with partial messages: message_start opening it with its id, its model and
// its input counts; its prose as a text delta and an assistant event; each
// call as an assistant event of its own; and message_delta closing it with its
// stop reason and its true output count — every figure the claude-code
// profile's requests block reads.
func (c *harnessRun) record(reply modelReply, ids []string) {
	id := claudeID("msg_")
	u := reply.Usage
	c.total = c.total.plus(u)
	c.emit(streamEvent(map[string]any{"type": "message_start", "message": map[string]any{
		"id": id, "type": "message", "role": "assistant", "model": c.model, "content": []any{},
		"usage": map[string]any{
			"input_tokens": u.Input, "cache_read_input_tokens": u.CacheRead,
			"cache_creation_input_tokens": u.CacheWrite,
			// What Claude Code's message_start says before a token is
			// written; the true count is message_delta's.
			"output_tokens": 1,
		},
	}}))
	if reply.Text != "" {
		c.emit(streamEvent(map[string]any{"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "text_delta", "text": reply.Text}}))
		c.emit(c.assistant(id, map[string]any{"type": "text", "text": reply.Text}))
	}
	for i, call := range reply.Calls {
		c.emit(c.assistant(id, map[string]any{"type": "tool_use", "id": ids[i],
			"name": claudeCodeToolPrefix + call.Name, "input": orEmptyArgs(call.Args)}))
	}
	stop := "end_turn"
	if len(reply.Calls) > 0 {
		stop = "tool_use"
	}
	c.emit(streamEvent(map[string]any{"type": "message_delta",
		"delta": map[string]any{"stop_reason": stop}, "usage": map[string]any{"output_tokens": u.Output}}))
	c.emit(streamEvent(map[string]any{"type": "message_stop"}))
}

// call makes one call to the server, under the id the record gave it in
// `_meta` as Claude Code does, prints its result as the user event Claude Code
// prints, and returns it for the model. An error is the session failing; a
// call the server refused is a result.
func (c *harnessRun) call(cs *mcpsdk.ClientSession, call modelCall, id string) (callResult, error) {
	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name: call.Name, Arguments: orEmptyArgs(call.Args),
		Meta: mcpsdk.Meta{claudeCodeToolUseMeta: id},
	})
	out := callResult{Name: call.Name}
	var wire *jsonrpc.Error
	switch {
	case errors.As(err, &wire):
		// The server answered the request with a protocol error, which
		// Claude Code hands its model as a failed result.
		out.Content, out.Failed = wire.Message, true
	case err != nil:
		return callResult{}, err
	default:
		out.Content, out.Failed = callText(res), res.IsError
	}
	c.emit(map[string]any{"type": "user", "message": map[string]any{"role": "user",
		"content": []any{map[string]any{"type": "tool_result", "tool_use_id": id,
			"content": out.Content, "is_error": out.Failed}}}})
	return out, nil
}

// result prints the run's last event, Claude Code's result: the last
// response's prose and the run's counts, every request summed. A run already
// stopped prints none: it did not finish.
func (c *harnessRun) result() {
	c.turn.Lock()
	defer c.turn.Unlock()
	if c.stopping.Load() {
		os.Exit(stoppedExit)
	}
	c.emit(map[string]any{"type": "result", "subtype": "success", "is_error": false,
		"result": c.last, "usage": c.total.claudeUsage()})
}

// emit prints one event of the record, as one line.
func (c *harnessRun) emit(event map[string]any) {
	// A map of strings, numbers and decoded JSON always encodes, and a write
	// to a stdout the backend has closed is a run it already ended.
	_ = c.out.Encode(event)
}

func (c *harnessRun) assistant(id string, block map[string]any) map[string]any {
	return map[string]any{"type": "assistant", "parent_tool_use_id": nil, "message": map[string]any{
		"id": id, "type": "message", "role": "assistant", "model": c.model,
		"content": []any{block},
		// The undercount Claude Code's assistant events carry with partial
		// messages on; the record's true counts are the stream events'.
		"usage": map[string]any{"input_tokens": 1, "output_tokens": 1},
	}}
}

// connect starts the server the configuration file names and connects to it, as
// Claude Code does with an `--mcp-config` file: its `mcpServers.crewlet` entry's
// command and arguments, over stdio — re-reading the tool list when the server
// says it changed ([harnessRun.stale]), which the profile declares it does.
func (c *harnessRun) connect(path string) (*mcpsdk.ClientSession, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // the configuration the engine wrote for this run
	if err != nil {
		return nil, err
	}
	var config struct {
		MCPServers map[string]struct {
			Type    string   `json:"type"`
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		return nil, fmt.Errorf("the configuration does not decode: %w", err)
	}
	server, ok := config.MCPServers[claudeCodeServer]
	if !ok || server.Command == "" {
		return nil, fmt.Errorf("the configuration names no %q server with a command", claudeCodeServer)
	}
	if server.Type != "" && server.Type != "stdio" {
		return nil, fmt.Errorf("the %q server is of type %q, and only stdio starts a command",
			claudeCodeServer, server.Type)
	}
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "crewlet-e2e-scripted-cli", Version: "1"},
		&mcpsdk.ClientOptions{ToolListChangedHandler: func(context.Context, *mcpsdk.ToolListChangedRequest) {
			c.stale.Store(true)
		}})
	//nolint:gosec // the relay the engine named
	return client.Connect(context.Background(),
		&mcpsdk.CommandTransport{Command: exec.Command(server.Command, server.Args...)}, nil)
}

// listTools is every tool the server offers, by its own names, every page read.
func listTools(cs *mcpsdk.ClientSession) ([]string, error) {
	var names []string
	for tool, err := range cs.Tools(context.Background(), nil) {
		if err != nil {
			return nil, err
		}
		names = append(names, tool.Name)
	}
	return names, nil
}

// systemPrompt is the system prompt the harness was handed and the channel it came
// on.
//
// ABSENT IS NO SYSTEM PROMPT, not a fault: the backend passes no flag for a
// request that carries none, since a harness handed an empty one would replace its
// default with nothing. BOTH IS: a call takes one route.
func systemPrompt() (string, string, error) {
	var system []byte
	channel := ""
	for _, ch := range []struct{ flag, name string }{
		{scriptedSystemFlag, channelReplace}, {scriptedAppendFlag, channelAppend},
	} {
		i := slices.Index(os.Args, ch.flag)
		if i < 0 {
			continue
		}
		if channel != "" {
			return "", "", errors.New("handed a system prompt on both channels")
		}
		if i+1 >= len(os.Args) {
			return "", "", fmt.Errorf("%s names no file", ch.flag)
		}
		var err error
		if system, err = os.ReadFile(os.Args[i+1]); err != nil {
			return "", "", fmt.Errorf("reading the system prompt: %w", err)
		}
		channel = ch.name
	}
	return string(system), channel, nil
}

func failHarness(format string, args ...any) int {
	fmt.Fprintf(os.Stderr, "scripted harness: "+format+"\n", args...)
	return 1
}

func argAfter(flag string) string {
	if i := slices.Index(os.Args, flag); i >= 0 && i+1 < len(os.Args) {
		return os.Args[i+1]
	}
	return ""
}

func streamEvent(event map[string]any) map[string]any {
	return map[string]any{"type": "stream_event", "event": event, "parent_tool_use_id": nil}
}

// claudeID is an id in the shape Claude Code's record carries: its prefix and
// a random tail, unique across every run of a phase — the host keys a request
// by its id, and a phase's later runs must never name an earlier run's.
func claudeID(prefix string) string {
	var raw [12]byte
	// crypto/rand.Read never fails on the platforms this builds for.
	_, _ = rand.Read(raw[:])
	return prefix + hex.EncodeToString(raw[:])
}

func callText(res *mcpsdk.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcpsdk.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func orEmptyArgs(args map[string]any) map[string]any {
	if args == nil {
		return map[string]any{}
	}
	return args
}

// --- the model's half -------------------------------------------------------- //

// readRequest reads the one request the scripted harness posts for a model
// request, answering the harness with why, and reporting false, when it cannot be
// read. The harness prints that on stderr and exits non-zero, so the reason reaches
// the engine's log as the run's failure.
func readRequest(w http.ResponseWriter, r *http.Request) (modelRequest, bool) {
	var req modelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "the scripted harness posted no request: "+err.Error(), http.StatusBadRequest)
		return modelRequest{}, false
	}
	return req, true
}

// answer writes the model's reply.
func answer(w http.ResponseWriter, reply modelReply) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(reply)
}

// partHeading is a line that opens a part of the transcript a run with tools is
// handed (harness's renderTranscript): `=== <nonce> <heading> ===`, the nonce
// drawn for that one render.
var partHeading = regexp.MustCompile(`(?m)^=== ([0-9a-f]+) (.+) ===$`)

// resultHeading opens a part that is a tool's result.
const resultHeading = "result of "

// toolResultsIn is every tool result a run's transcript hands the model back —
// the results of the phase's earlier runs, a resumed phase's spliced answer
// among them — in order: the body of each `result of <tool>` part, whole.
//
// FENCED BY THE RENDER'S NONCE, the one the first part opens with: a result
// carrying a line shaped like a heading under any other value is text inside
// the part it is in, exactly as the transcript's preamble tells the model. A
// prompt with no parts — a text function's — hands back no results.
func toolResultsIn(prompt string) []string {
	heads := partHeading.FindAllStringSubmatchIndex(prompt, -1)
	if len(heads) == 0 {
		return nil
	}
	nonce := prompt[heads[0][2]:heads[0][3]]
	heads = slices.DeleteFunc(heads, func(h []int) bool { return prompt[h[2]:h[3]] != nonce })
	var out []string
	for i, h := range heads {
		if !strings.HasPrefix(prompt[h[4]:h[5]], resultHeading) {
			continue
		}
		end := len(prompt)
		if i+1 < len(heads) {
			end = heads[i+1][0]
		}
		out = append(out, strings.TrimSpace(prompt[h[1]:end]))
	}
	return out
}

// replyUsage is the usage one scripted response reports, as Anthropic's usage
// counts it: Input the uncached part of the prompt, the cache figures beside
// it.
type replyUsage struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	CacheRead  int `json:"cache_read"`
	CacheWrite int `json:"cache_write"`
}

// The usage each scripted response reports. NAMED rather than inlined in a
// reply, because a case that reasons about a budget has to know what each
// request costs, and a figure restated in a comment beside a literal goes stale
// the first time the literal moves — the budget case's own comment said 150
// and 130 long after the fixture began reporting cache tokens.
var (
	toolUseUsage   = replyUsage{Input: 120, Output: 30, CacheRead: 80, CacheWrite: 15}
	textReplyUsage = replyUsage{Input: 90, Output: 40, CacheRead: 60, CacheWrite: 25}
)

// tokens is what the engine charges for one such response: the harness backend
// counts the prompt whole, both cache figures folded into its input, and the
// output beside it — so every one of them is counted (the profile's usage
// arithmetic, [harnessprofile.UsagePaths.PromptTokens]).
func (u replyUsage) tokens() int { return u.prompt() + u.Output }

// prompt is the whole prompt one such response read.
func (u replyUsage) prompt() int { return u.Input + u.CacheRead + u.CacheWrite }

func (u replyUsage) plus(v replyUsage) replyUsage {
	return replyUsage{Input: u.Input + v.Input, Output: u.Output + v.Output,
		CacheRead: u.CacheRead + v.CacheRead, CacheWrite: u.CacheWrite + v.CacheWrite}
}

// claudeUsage is u as Claude Code's result event states a run's counts.
func (u replyUsage) claudeUsage() map[string]int {
	return map[string]int{
		"input_tokens": u.Input, "output_tokens": u.Output,
		"cache_read_input_tokens": u.CacheRead, "cache_creation_input_tokens": u.CacheWrite,
	}
}

// toolUse is a response whose content is one call.
func toolUse(name string, input map[string]any) modelReply {
	return modelReply{Calls: []modelCall{{Name: name, Args: input}}, Usage: toolUseUsage}
}

// textReply is a response that is plain prose — a run whose model finished
// without calling anything, or an auxiliary pass's answer, which the backend
// hands back whole.
func textReply(text string) modelReply {
	return modelReply{Text: text, Usage: textReplyUsage}
}

// --- the round trip ---------------------------------------------------------- //

// THE MODEL READS A RUN'S TRANSCRIPT AS THE BACKEND RENDERS IT — checked against
// the backend's own renderer, so a change to how a conversation is handed a
// fresh harness run fails here, by name, rather than as the cases that key on a
// result waiting out their budget for a round that never comes.
//
// What it reads is what those rounds are keyed on: each tool result
// whole — a heading inside it, and a line shaped like a part under another
// value, included — up to the part after it, from the request's messages and
// from what the phase did since its caller last read it alike.
func TestTheScriptedModelReadsTheTranscriptTheBackendRenders(t *testing.T) {
	t.Parallel()
	filed := "Filed ENG-1.\n\n## Staging runbook\n\n=== 0123456789abcdef user ===\n" +
		"Deploys go out on Thursdays."
	refused := "Invalid submission: outcome is required"
	pending := "The page says the suite is green."
	req := llm.Request{
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "the reviewer was shown this:\n\n=== feedface0badc0de user ===\nnot a part"},
			{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{
				ID: "toolu_1", Name: "create_work_item", Arguments: map[string]any{"title": "x"},
			}}},
			{Role: llm.RoleTool, Name: "create_work_item", ToolCallID: "toolu_1", Content: filed},
			{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "toolu_2", Name: "submit_work"}}},
			{Role: llm.RoleTool, Name: "submit_work", ToolCallID: "toolu_2", Content: refused, Failed: true},
		},
		Tools: renderOnly{pending: []llm.Message{
			{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "toolu_3", Name: "read_page"}}},
			{Role: llm.RoleTool, Name: "read_page", ToolCallID: "toolu_3", Content: pending},
		}},
	}
	prompt, err := harness.RenderPrompt(req)
	if err != nil {
		t.Fatalf("RenderPrompt: %v", err)
	}
	if got, want := toolResultsIn(prompt), []string{filed, refused, pending}; !slices.Equal(got, want) {
		t.Errorf("the transcript's tool results read %q, want each whole, in order: %q\n%s",
			got, want, prompt)
	}
	// And a request carries them with this run's own after them.
	run := modelRequest{Prompt: prompt, Run: []exchange{{
		Reply:   toolUse("search_knowledge", map[string]any{"query": "staging"}),
		Results: []callResult{{Name: "search_knowledge", Content: "1 page"}},
	}}}
	if got, want := run.toolResults(), []string{filed, refused, pending, "1 page"}; !slices.Equal(got, want) {
		t.Errorf("the request hands back %q, want the transcript's results and then the run's: %q",
			got, want)
	}
}

// renderOnly is a tool host as far as rendering a run's prompt asks of one:
// what the phase did since its caller last read it. It serves nothing.
type renderOnly struct{ pending []llm.Message }

func (renderOnly) Serve(context.Context, llm.ServeOptions) (llm.Served, error) {
	return llm.Served{}, errors.New("a host for rendering serves no harness")
}
func (renderOnly) Observe(llm.Turn)         {}
func (renderOnly) Spent(llm.Usage)          {}
func (renderOnly) Done() <-chan struct{}    { return nil }
func (renderOnly) Halted() bool             { return false }
func (h renderOnly) Pending() []llm.Message { return h.pending }

// THE SCRIPTED HARNESS ANSWERS THE BACKEND THE GOLDEN COMPANY BUILDS — built from
// the document as every node here builds it ([engine.BuildHarness]) and driven
// as a phase and an auxiliary pass drive it. Every case in this package stands
// on this round trip, and a break in it — the marker not reaching the child,
// the relay not starting, the seat's tools not listed, a call not reaching the
// phase, the system prompt off its channel, a request or a count the backend no
// longer reads — reaches them only as a turn that never ends, so it is held
// here, where it can say which.
//
// THE PHASE RUNS THROUGH THE REAL LOOP AND THE REAL SESSION (toolloop over a
// toolsession host), because that is the only way a harness reaches a phase's
// tools now: the relay this binary starts, the socket, the listing, the call
// joined to the request the record shows.
func TestTheScriptedHarnessAnswersTheBackendTheCompanyBuilds(t *testing.T) {
	t.Parallel()
	// WHAT THE HARNESS DOES IS WHAT THE PROFILE READS: the name it shows its
	// model a seat's tool under, and the `_meta` key it hands the server its
	// own id for a call under. A call joins its request by name and
	// arguments where the key is not read, so the phase below would pass
	// with the two out of step, and every call in a response of two alike
	// would be joined by guesswork.
	claude, _ := harnessprofile.Builtin("claude-code")
	if r := claude.Requests; r == nil || r.HostPrefix != claudeCodeToolPrefix ||
		r.CallIDMeta != claudeCodeToolUseMeta {
		t.Fatalf("the claude-code profile reads the record by %+v, and the scripted harness "+
			"prints tools as %q and call ids under %q", r, claudeCodeToolPrefix, claudeCodeToolUseMeta)
	}

	model := newScriptedModel(t)
	cfg, err := config.ParseCompany([]byte(goldenCompany(model)))
	if err != nil {
		t.Fatalf("company config: %v", err)
	}
	provider, err := engine.BuildHarness("scripted", cfg.Providers.LLM["scripted"],
		config.NewResolver(nodeEnvironment(nil)), nil)
	if err != nil {
		t.Fatalf("BuildHarness: %v", err)
	}
	host, err := toolsession.NewHost()
	if err != nil {
		t.Fatalf("toolsession.NewHost: %v", err)
	}
	t.Cleanup(func() { _ = host.Close() })
	ctx := llm.WithSeat(t.Context(), "ceo")

	// A PHASE: a system prompt, a conversation, a tool to call and a tool to
	// finish with, run as the runner runs a seat's executor — and a model
	// that calls the one, reads its answer, and finishes with the other: the
	// harness's own loop, across two model requests of one run.
	const system = "You are the CEO of Nimbus. You answer to the founder."
	model.callOnExecute("search_knowledge", map[string]any{"query": "staging runbook"})
	work := &phaseTools{names: []string{"submit_work", "search_knowledge"}}
	res, err := toolloop.Run(ctx, toolloop.Config{
		Provider: provider, Surface: work, Host: host, MaxRounds: 3,
		TerminateAfter: []string{"submit_work"},
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: system},
			{Role: llm.RoleUser, Content: "How did the week go?"},
		},
	})
	if err != nil {
		t.Fatalf("an executor phase: %v", err)
	}
	if got := work.calls(); len(got) != 2 || got[0].Name != "search_knowledge" ||
		got[0].Arguments["query"] != "staging runbook" || got[1].Name != "submit_work" ||
		got[1].Arguments["outcome"] != "delivered" {
		t.Errorf("the phase's tools were called %+v, want the scripted search and then "+
			"submit_work", got)
	}
	// WHAT THE CALL ANSWERED REACHED THE MODEL, in the run's next request.
	if got := model.toolResults(); !slices.Equal(got, []string{"search_knowledge recorded"}) {
		t.Errorf("the model was handed back %q, want the search's answer once", got)
	}
	// TWO ROUNDS, ONE RUN, JOINED: each request the record showed is one of
	// the phase's rounds, each call its round's, under the id Claude Code
	// gave it, and the one harness run made both.
	if res.RoundsUsed != 2 || len(res.Rounds) != 2 {
		t.Errorf("the phase used %d rounds and timed %d, want its two model requests",
			res.RoundsUsed, len(res.Rounds))
	}
	if len(res.Executions) != 2 {
		t.Fatalf("the phase recorded %+v, want the search and the submission", res.Executions)
	}
	for i, exec := range res.Executions {
		if exec.Round != i+1 || !strings.HasPrefix(exec.CallID, "toolu_") {
			t.Errorf("the phase recorded %s in round %d under call id %q, want round %d under "+
				"its tool_use id", exec.Name, exec.Round, exec.CallID, i+1)
		}
	}
	if res.Executions[0].CallID == res.Executions[1].CallID {
		t.Errorf("both calls carry the id %q: a call is joined to its request by it",
			res.Executions[0].CallID)
	}
	// AND ENDED BY THE ENGINE: the submission finished the phase, so the run
	// was stopped there rather than asked on — the engine holds the answer
	// to the call that ends a phase until its harness is gone.
	if len(res.Invocations) != 1 || !res.Invocations[0].RequestsObserved ||
		res.Invocations[0].FirstRound != 1 || res.Invocations[0].LastRound != 2 ||
		res.Invocations[0].Model != "claude-golden" || res.Invocations[0].Outcome != toolloop.OutcomeStopped {
		t.Errorf("the phase recorded the harness runs %+v, want one by claude-golden that showed "+
			"both its requests and was stopped once the phase submitted", res.Invocations)
	}
	if res.Model != "claude-golden" {
		t.Errorf("the phase names model %q, want the one the record names", res.Model)
	}
	chargedAs(t, "two tool-use responses", res.InputTokens, res.OutputTokens, res.CacheRead,
		res.CacheWrite, toolUseUsage.plus(toolUseUsage))
	if got := model.seen(); !slices.Equal(got, []string{"execute", "execute"}) {
		t.Errorf("the model recorded %v for two executor requests", got)
	}
	// THE SEAT'S TOOLS, as the server listed them: what the phases are keyed
	// on, never a list the prompt mentions.
	model.mu.Lock()
	offered, transcript := slices.Clone(model.offered), model.prompts[0]
	model.mu.Unlock()
	if want := "search_knowledge+submit_work"; !slices.Equal(offered, []string{want, want}) {
		t.Errorf("the model was offered %q, want the phase's two tools on each request", offered)
	}
	// ON ITS OWN CHANNEL, WHOLE, and nowhere else: the system prompt every
	// prompt case here reads is the one the harness was handed apart from the
	// transcript — on the append channel, after the note that says the harness's
	// own prompt stays in force and the seat's is the role.
	got := model.systemPrompts()
	if len(got) != 2 || !strings.HasSuffix(got[0], system) || got[0] == system || got[1] != got[0] {
		t.Errorf("the model was shown the system prompts %q, want %q after the harness note on "+
			"each request", got, system)
	}
	if strings.Contains(transcript, system) {
		t.Errorf("the system prompt rode the transcript as well as its own channel:\n%s", transcript)
	}

	// AN AUXILIARY PASS: no tools, and an answer handed back whole — a JSON
	// object included, which is the persist decider's whole contract.
	persist, err := provider.Complete(ctx, llm.Request{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: learning.PersistSystemPrompt},
			{Role: llm.RoleUser, Content: "The founder asked for the weekly numbers, plainly."},
		},
	})
	if err != nil {
		t.Fatalf("an auxiliary pass: %v", err)
	}
	if want := auxiliaryAnswer("persist"); persist.Content != want {
		t.Errorf("the persist pass came back as %q, want %q whole", persist.Content, want)
	}
	charged(t, "a text answer", persist, textReplyUsage)
	if got := model.seen(); !slices.Equal(got, []string{"execute", "execute", "aux:persist"}) {
		t.Errorf("the model recorded %v, want the executor's two requests and the persist pass", got)
	}
	// Whole and alone, on the replace channel: a side pass has no use for
	// the harness's own prompt.
	if got := model.systemPrompts(); len(got) != 3 || got[2] != strings.TrimSpace(learning.PersistSystemPrompt) {
		t.Errorf("the persist pass was shown %q, want its own prompt alone", got)
	}
	if bad := model.misrouted(); len(bad) > 0 {
		t.Errorf("system prompts on the wrong channel: %s", strings.Join(bad, "; "))
	}
}

// A STOPPED HARNESS PRINTS THE REQUEST ITS MODEL ANSWERED, and nothing after it.
//
// The engine stops a run it is done with — a phase decided, a budget refused —
// and every case that holds the counter to the model's log reads one fact
// across that stop: a request the model answered is a request the engine read
// and charged. A real harness's answer and its record are one stream; the scripted
// harness's are two steps, the post and the printing, so the fact holds only
// because a stop landing between them waits for the printing
// ([harnessRun.handleStop]). Held here, where the stop can be put exactly there,
// rather than only in the budget cases, which would wait out their budget on
// its absence and say nothing of why.
func TestAStoppedScriptedHarnessPrintsTheRequestItsModelAnswered(t *testing.T) {
	t.Parallel()
	asked, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	model := &scriptedModel{}
	model.listen(t, func(w http.ResponseWriter, r *http.Request) {
		if _, ok := readRequest(w, r); !ok {
			return
		}
		once.Do(func() { close(asked) })
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		answer(w, textReply("Three PRs merged, one incident, zero regressions."))
	})

	// A TEXT FUNCTION'S RUN, launched as the backend launches one: the
	// fork-bomb guard, the model, the transcript on stdin, and the entry's
	// environment alone.
	cmd := exec.Command(scriptedHarnessBinary, "-test.run=^$", "--model", "claude-golden") //nolint:gosec // this test binary
	cmd.Env = []string{scriptedHarnessEnv + "=" + model.url,
		procgrouptest.GORACE + "=" + procgrouptest.StandInRaceOptions()}
	cmd.Stdin = strings.NewReader("## user\n\nHow did the week go?\n")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the scripted harness: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	noticed, read := make(chan struct{}), make(chan string, 1)
	go func() {
		var said strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := stderr.Read(buf)
			said.Write(buf[:n])
			if strings.Contains(said.String(), stopNotice) {
				select {
				case <-noticed:
				default:
					close(noticed)
				}
			}
			if err != nil {
				read <- said.String()
				return
			}
		}
	}()
	await := func(what string, ch <-chan struct{}) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(waitBudget):
			t.Fatalf("timed out waiting for %s; stdout so far:\n%s", what, stdout.String())
		}
	}

	// THE STOP LANDS WHILE THE MODEL IS ANSWERING: after the request was
	// posted, before its record could be printed.
	await("the harness to ask its model", asked)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("stop the scripted harness: %v", err)
	}
	await("the harness to take the stop", noticed)
	close(release)
	said := <-read
	err = cmd.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != stoppedExit {
		t.Errorf("the stopped harness exited %v, want %d; it said:\n%s", err, stoppedExit, said)
	}

	// THE ANSWERED REQUEST IS ON THE RECORD, whole — opened with its input
	// counts and closed with its output count, which is what the backend
	// charges — and NOTHING AFTER IT: a stopped run did not finish.
	var kinds []string
	output := -1
	for line := range strings.Lines(stdout.String()) {
		var event struct {
			Type  string `json:"type"`
			Event struct {
				Type  string `json:"type"`
				Usage struct {
					Output int `json:"output_tokens"`
				} `json:"usage"`
			} `json:"event"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("the record carries a line that is not an event: %q", line)
		}
		kind := event.Type
		if event.Type == "stream_event" {
			kind = event.Event.Type
			if kind == "message_delta" {
				output = event.Event.Usage.Output
			}
		}
		kinds = append(kinds, kind)
	}
	if !slices.Contains(kinds, "message_start") || output != textReplyUsage.Output {
		t.Errorf("the stopped harness's record is %v with output %d, want the answered request "+
			"whole, its output %d counted", kinds, output, textReplyUsage.Output)
	}
	if slices.Contains(kinds, "result") {
		t.Errorf("the stopped harness printed a result, as though its run had finished: %v", kinds)
	}
}

// phaseTools is a phase's tool surface as far as the round trip asks of one:
// the tools it offers, each answering that it was recorded, and the calls that
// reached it.
type phaseTools struct {
	names []string

	mu  sync.Mutex
	got []llm.ToolCall
}

func (s *phaseTools) ToolDefs() []llm.ToolDef {
	defs := make([]llm.ToolDef, 0, len(s.names))
	for _, name := range s.names {
		defs = append(defs, llm.ToolDef{Name: name, Description: name,
			Parameters: map[string]any{"type": "object"}})
	}
	return defs
}
func (s *phaseTools) Revision() uint64                     { return 0 }
func (s *phaseTools) AnnotationsOf(string) mcp.Annotations { return mcp.Annotations{} }
func (s *phaseTools) Phase() string                        { return "execute" }

// Admit admits a call to a surface that keeps no call log: it runs as it is.
func (s *phaseTools) Admit(string, map[string]any) toolloop.Admitted { return s.execute }

func (s *phaseTools) execute(_ context.Context, call llm.ToolCall) (toolloop.ToolResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, call)
	return toolloop.ToolResult{Output: call.Name + " recorded", Origin: "builtin"}, nil
}

func (s *phaseTools) calls() []llm.ToolCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.got)
}

// charged fails the test unless completion carries exactly what the scripted
// response reported: the counts every budget case here reasons from.
func charged(t *testing.T, what string, completion *llm.Completion, usage replyUsage) {
	t.Helper()
	chargedAs(t, what, completion.InputTokens, completion.OutputTokens,
		completion.CacheRead, completion.CacheWrite, usage)
}

// chargedAs is [charged] over the four counts, wherever they are carried.
func chargedAs(t *testing.T, what string, input, output, cacheRead, cacheWrite int, usage replyUsage) {
	t.Helper()
	if input != usage.prompt() || output != usage.Output ||
		cacheRead != usage.CacheRead || cacheWrite != usage.CacheWrite {
		t.Errorf("%s was counted in=%d out=%d cache_read=%d cache_write=%d, want the "+
			"%+v the harness reported", what, input, output, cacheRead, cacheWrite, usage)
	}
	if got := input + output; got != usage.tokens() {
		t.Errorf("%s costs %d tokens, want the %d the budget cases count it as",
			what, got, usage.tokens())
	}
}
