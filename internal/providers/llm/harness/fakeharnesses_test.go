package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/providers/llm"
)

// THE FAKE PLAYS EVERY CLI WITH A TOOL ROUTE, not Claude Code alone: a script
// whose CLI names another is played as that CLI is — the crewlet server found
// where the shipped profile tells that CLI it is, the calls made with the
// `_meta` that CLI sends, and the record written in the shape that CLI writes
// it, on stdout or in the file it keeps, as the real one does (measured; see
// each profile). So the suite drives each shipped profile with only the binary
// replaced, and a profile that stops reading its CLI's record fails here.

// fakeRecorder writes one CLI's record of a run as the run goes.
type fakeRecorder interface {
	// begin starts the record, once the CLI has its tools.
	begin(model string, tools []string)
	// asked records a response the way the CLI records it before it makes
	// the response's calls.
	asked(i int, r fakeResponse, ids []string)
	// answered records what the CLI records once the calls have run.
	answered(i int, r fakeResponse, ids []string, results []string)
	// end finishes the record and returns the run's exit status.
	end(script fakeScript, total fakeUsage, last string) int
}

// fakeHarness is how one CLI is played: where its configuration names the
// crewlet server, the `_meta` it sends with a call, and its recorder.
type fakeHarness struct {
	server   func() (command string, args []string, err error)
	meta     func(id string) sdk.Meta
	recorder func(script fakeScript, prompt []byte) (fakeRecorder, error)
	// run, where set, plays the CLI whole instead — one whose run is more
	// than one process, as OpenCode's serve lifecycle is. It never returns.
	run func(script fakeScript)
}

// fakeHarnesses are the CLIs the fake plays besides Claude Code, by profile name.
// Each profile's own test file adds its CLI.
var fakeHarnesses = map[string]fakeHarness{}

// runFakeOtherHarness plays script.CLI. It never returns.
func runFakeOtherHarness(script fakeScript) {
	fake, ok := fakeHarnesses[script.Harness]
	if !ok {
		fmt.Fprintf(os.Stderr, "fake: no CLI %q\n", script.Harness)
		os.Exit(70)
	}
	if fake.run != nil {
		fake.run(script)
	}
	prompt, _ := io.ReadAll(os.Stdin)
	if script.PromptTo != "" {
		_ = os.WriteFile(script.PromptTo, prompt, 0o600)
	}
	rec, err := fake.recorder(script, prompt)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake: record:", err)
		os.Exit(73)
	}
	var cs *sdk.ClientSession
	if !script.NoConnect {
		command, args, err := fake.server()
		if err != nil {
			fmt.Fprintln(os.Stderr, "fake: no crewlet server in the configuration:", err)
			os.Exit(74)
		}
		cs, err = connectStdio(command, args)
		if err != nil {
			fmt.Fprintln(os.Stderr, "fake: connect:", err)
			os.Exit(71)
		}
	}
	os.Exit(playAs(script, rec, cs, fake.meta))
}

// playAs plays the script against the server cs connects to — none for nil —
// writing the record through rec, and returns the exit status.
func playAs(script fakeScript, rec fakeRecorder, cs *sdk.ClientSession, meta func(string) sdk.Meta) int {
	model := script.Model
	if model == "" {
		model = "fake-model-1"
	}
	var tools []string
	if cs != nil {
		defer cs.Close()
		listed, err := cs.ListTools(context.Background(), nil)
		if err != nil {
			fmt.Fprintln(os.Stderr, "fake: list:", err)
			return 72
		}
		for _, tool := range listed.Tools {
			tools = append(tools, tool.Name)
		}
	}
	rec.begin(model, tools)
	var total fakeUsage
	last := ""
	for i, r := range script.Responses {
		if r.SilentMS > 0 {
			time.Sleep(time.Duration(r.SilentMS) * time.Millisecond)
		}
		ids := make([]string, len(r.Calls))
		for j := range r.Calls {
			ids[j] = fmt.Sprintf("call_%d_%d", i+1, j)
		}
		total.Input += r.Usage.Input
		total.Output += r.Usage.Output
		total.CacheRead += r.Usage.CacheRead
		total.CacheWrite += r.Usage.CacheWrite
		rec.asked(i, r, ids)
		if r.Text != "" {
			last = r.Text
		}
		if r.StopEarly {
			break
		}
		results := make([]string, len(r.Calls))
		for j, c := range r.Calls {
			if c.Own || cs == nil {
				results[j] = "the CLI's own"
				continue
			}
			raw, _ := json.Marshal(orEmpty(c.Args))
			params := &sdk.CallToolParams{Name: c.Name, Arguments: json.RawMessage(raw)}
			if meta != nil {
				params.Meta = meta(ids[j])
			}
			res, err := cs.CallTool(context.Background(), params)
			results[j] = "error: " + fmt.Sprint(err)
			if err == nil {
				results[j] = resultText(res)
			}
		}
		rec.answered(i, r, ids, results)
	}
	if script.Stderr != "" {
		fmt.Fprint(os.Stderr, script.Stderr)
	}
	if script.Hang || script.HangUnsignalled {
		park(script.HangUnsignalled)
	}
	if script.ResultUsage != nil {
		total = *script.ResultUsage
	}
	return rec.end(script, total, last)
}

// connectStdio starts the stdio server command names — the relay — and
// connects to it.
func connectStdio(command string, args []string) (*sdk.ClientSession, error) {
	client := sdk.NewClient(&sdk.Implementation{Name: "fake-cli", Version: "1"}, nil)
	//nolint:gosec // the relay the engine named
	return client.Connect(context.Background(),
		&sdk.CommandTransport{Command: exec.Command(command, args...)}, nil)
}

// serverIn reads the crewlet server out of an mcpServers document.
func serverIn(raw []byte) (string, []string, error) {
	var config struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		return "", nil, err
	}
	server, ok := config.MCPServers["crewlet"]
	if !ok {
		return "", nil, fmt.Errorf("no crewlet server")
	}
	return server.Command, server.Args, nil
}

// recordFile creates a record file and the encoder its lines are written
// with, one write a line, as a CLI appends to its own.
func recordFile(path string) (*json.Encoder, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.Create(path) //nolint:gosec // the fake's own record
	if err != nil {
		return nil, err
	}
	return json.NewEncoder(f), nil
}

// fakeAs is an entry on the shipped profile agent names whose CLI is this
// test binary playing script — the profile's tool route and request record,
// with only the binary replaced. Signed in with a fake key: the fake reaches
// no vendor.
func fakeAs(t *testing.T, agent string, script fakeScript, tune func(*Config)) *Provider {
	t.Helper()
	script.Harness = agent
	return fakeClaude(t, script, func(c *Config) {
		c.Harness = agent
		c.Auth = Auth{Mode: AuthAPIKey, APIKey: "test-not-a-key"}
		if tune != nil {
			tune(c)
		}
	})
}

// routeCase is the run every profile's route is held to: a response with prose
// and a call to the seat's search, then the answer — with cache reads and a
// usage of each request's own.
var routeCase = fakeScript{Model: "fake-model-7", Responses: []fakeResponse{
	{Text: "Looking it up.", Calls: []fakeCall{{Name: "search", Args: map[string]any{"q": "release notes"}}},
		Usage: fakeUsage{Input: 100, Output: 20, CacheRead: 40}},
	{Text: "Done: the notes are in.", Usage: fakeUsage{Input: 140, Output: 9, CacheRead: 60}},
}}

// holdsTheRoute runs routeCase on p and holds it to what every route owes the
// host: the call reaches the host, the record tells it each request in order
// with the call joined under the host's own name, each request is charged as
// it is read and the run's own total settles the rest, and the answer is the
// last response's prose. It returns the host, for what a profile adds.
func holdsTheRoute(t *testing.T, p *Provider, wantOpts llm.ServeOptions, wantSpent llm.Usage) *testHost {
	t.Helper()
	host := newTestHost(t, "search")
	host.handle("search", func(context.Context, map[string]any) (string, bool) { return "3 results", false })
	comp, err := p.Complete(t.Context(), phase(host))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if comp.Content != "Done: the notes are in." || comp.StopReason != llm.StopEnd {
		t.Errorf("completion = %q (%q), want the last response's prose, ended", comp.Content, comp.StopReason)
	}
	_, calls, spent, spends := host.snapshot()
	if len(calls) != 1 || calls[0].Name != "search" || calls[0].Args["q"] != "release notes" {
		t.Fatalf("the host was called %+v, want the one search", calls)
	}
	turns := host.latestTurns()
	if len(turns) != 2 {
		t.Fatalf("the host was told of %d requests, want 2: %+v", len(turns), turns)
	}
	first := turns[0]
	if first.Text != "Looking it up." || len(first.Calls) != 1 || first.Calls[0].Name != "search" ||
		!first.Calls[0].Host || first.Calls[0].Arguments["q"] != "release notes" {
		t.Errorf("first request = %+v, want the prose and the host's search", first)
	}
	if turns[1].Text != "Done: the notes are in." || len(turns[1].Calls) != 0 {
		t.Errorf("second request = %+v", turns[1])
	}
	if spent != wantSpent {
		t.Errorf("the run was charged %+v in all (%+v), want the CLI's own total %+v", spent, spends, wantSpent)
	}
	if len(spends) < 2 || spends[0].Estimated {
		t.Errorf("charged %+v, want each request as the record read it", spends)
	}
	host.mu.Lock()
	opts := host.opts[0]
	host.mu.Unlock()
	if opts.Relists != wantOpts.Relists || opts.ToolTimeout != wantOpts.ToolTimeout ||
		opts.ObservesRequests != wantOpts.ObservesRequests || opts.Concurrency != wantOpts.Concurrency ||
		opts.CallIDMeta != wantOpts.CallIDMeta || opts.RecordsCallsLate != wantOpts.RecordsCallsLate {
		t.Errorf("the host was told relists %v, tool timeout %v, observes %v, concurrency %q, call id "+
			"meta %q, calls recorded late %v; want %+v", opts.Relists, opts.ToolTimeout, opts.ObservesRequests,
			opts.Concurrency, opts.CallIDMeta, opts.RecordsCallsLate, wantOpts)
	}
	return host
}

// argAfterPrefix is the value of the argument after a `-c` whose value begins
// with prefix — a codex `-c key=value` — with the prefix cut off.
func argAfterPrefix(prefix string) (string, bool) {
	for i, arg := range os.Args {
		if arg == "-c" && i+1 < len(os.Args) && strings.HasPrefix(os.Args[i+1], prefix) {
			return strings.TrimPrefix(os.Args[i+1], prefix), true
		}
	}
	return "", false
}
