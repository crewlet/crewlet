package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm"
)

// OpenCode 2.1, as the fake plays it: a server (`serve`) printing its address
// and a password, finding the crewlet server in the call's opencode.jsonc and
// connecting to it, listed by `api mcp.list`; a run (`run --server`) that is
// its client, printing the server's record — step_start, prose, each call once
// it has COMPLETED, step_finish with the step's counts, and nothing but the
// prose of the final step; and the session's total asked of the server
// (`api session.get`). Measured on 2.1.4; see the profile.
func init() {
	fakeHarnesses["opencode"] = fakeHarness{run: runFakeOpenCode}
}

// fakeTotalFile is where the fake server leaves the session's total for `api
// session.get`, in the working directory.
const fakeTotalFile = ".fake-total"

func runFakeOpenCode(script fakeScript) {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "serve":
			opencodeServer(script)
		case "api":
			switch {
			case slices.Contains(os.Args, "mcp.list"):
				opencodePoll()
			case slices.Contains(os.Args, "session.get"):
				opencodeReport(script)
			}
		case "run":
			opencodeClient()
		}
	}
	fmt.Fprintf(os.Stderr, "fake opencode: not a role: %q\n", os.Args[1:])
	os.Exit(70)
}

func opencodeServer(script fakeScript) {
	password := fmt.Sprintf("pw-%d", os.Getpid())
	url := fmt.Sprintf("http://fake-%d", os.Getpid())
	fmt.Printf("server listening on %s\n", url)
	_ = os.WriteFile(fakeServerFile, []byte(url+" "+password), 0o600)
	fmt.Printf("server password %s\n", password)
	raw, err := os.ReadFile(filepath.Join(os.Getenv("OPENCODE_CONFIG_DIR"), "opencode.jsonc"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake opencode: no opencode.jsonc:", err)
		os.Exit(74)
	}
	var config struct {
		MCP struct {
			Servers map[string]struct {
				Command []string `json:"command"`
			} `json:"servers"`
		} `json:"mcp"`
	}
	if err := json.Unmarshal(raw, &config); err != nil || len(config.MCP.Servers["crewlet"].Command) == 0 {
		fmt.Fprintln(os.Stderr, "fake opencode: no crewlet server in opencode.jsonc")
		os.Exit(74)
	}
	command := config.MCP.Servers["crewlet"].Command
	cs, err := connectStdio(command[0], command[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake opencode: connect:", err)
		os.Exit(71)
	}
	_ = os.WriteFile(fakeConnectedFile, nil, 0o600)
	for {
		if _, err := os.Stat(fakeGoFile); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	events, err := recordFile(fakeEventsFile)
	if err != nil {
		os.Exit(73)
	}
	code := playAs(script, &opencodeRecorder{out: events}, cs, nil)
	_ = os.WriteFile(fakeDoneFile, []byte(strconv.Itoa(code)), 0o600)
	park(false)
}

// opencodeServerOrExit checks a client was handed the server's address and its
// password, exiting 76 when it was not.
func opencodeServerOrExit() {
	raw, err := os.ReadFile(fakeServerFile)
	want := strings.Fields(string(raw))
	if err != nil || len(want) != 2 || argAfter("--server") != want[0] || os.Getenv("OPENCODE_PASSWORD") != want[1] {
		fmt.Fprintf(os.Stderr, "fake opencode: not this server's client: --server %q\n", argAfter("--server"))
		os.Exit(76)
	}
}

// opencodePoll lists the server's MCP servers: none until the crewlet server
// has connected, as the real one's first answers do.
func opencodePoll() {
	opencodeServerOrExit()
	if _, err := os.Stat(fakeConnectedFile); err != nil {
		fmt.Println(`{"data":[]}`)
		os.Exit(0)
	}
	fmt.Println(`{"data":[{"name":"crewlet","status":{"status":"connected"}}]}`)
	os.Exit(0)
}

// opencodeReport is the session as the server holds it, its tokens the run's
// total — asked of a server that is gone, refused.
func opencodeReport(script fakeScript) {
	opencodeServerOrExit()
	pid, _ := strconv.Atoi(strings.TrimPrefix(argAfter("--server"), "http://fake-"))
	if pid <= 0 || syscall.Kill(pid, 0) != nil {
		fmt.Fprintln(os.Stderr, "fake opencode: connection refused")
		os.Exit(4)
	}
	var total fakeUsage
	raw, err := os.ReadFile(fakeTotalFile)
	if err != nil || json.Unmarshal(raw, &total) != nil {
		os.Exit(3)
	}
	if script.Serve.ReportUsage != nil {
		total = *script.Serve.ReportUsage
	}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"data": map[string]any{"tokens": map[string]any{
		"input": total.Input, "output": total.Output, "reasoning": 0,
		"cache": map[string]any{"read": total.CacheRead, "write": total.CacheWrite}}}})
	os.Exit(0)
}

// opencodeClient is `run --server`: it hands the server the prompt and prints
// what the server records, exiting as its loop did.
func opencodeClient() {
	opencodeServerOrExit()
	prompt, _ := io.ReadAll(os.Stdin)
	_ = os.WriteFile(fakeGoFile+".tmp", prompt, 0o600)
	_ = os.Rename(fakeGoFile+".tmp", fakeGoFile)
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

type opencodeRecorder struct{ out *json.Encoder }

func opencodePart(kind, message string, part map[string]any) map[string]any {
	part["messageID"] = message
	return map[string]any{"type": kind, "part": part}
}

func (o *opencodeRecorder) begin(string, []string) {}

func (o *opencodeRecorder) asked(i int, r fakeResponse, _ []string) {
	message := fmt.Sprintf("msg_%d", i+1)
	if len(r.Calls) > 0 {
		// A step that will call tools begins with step_start; the final
		// step prints its prose alone.
		_ = o.out.Encode(opencodePart("step_start", message, map[string]any{"type": "step-start"}))
	}
	if r.Text != "" {
		_ = o.out.Encode(opencodePart("text", message, map[string]any{"type": "text", "text": r.Text}))
	}
}

func (o *opencodeRecorder) answered(i int, r fakeResponse, ids []string, results []string) {
	if len(r.Calls) == 0 {
		return
	}
	message := fmt.Sprintf("msg_%d", i+1)
	for j, c := range r.Calls {
		name := "crewlet_" + c.Name
		if c.Own {
			name = c.Name
		}
		_ = o.out.Encode(opencodePart("tool_use", message, map[string]any{"type": "tool", "id": ids[j], "tool": name,
			"state": map[string]any{"status": "completed", "input": orEmpty(c.Args), "output": results[j]}}))
	}
	_ = o.out.Encode(opencodePart("step_finish", message, map[string]any{"type": "step-finish", "reason": "tool-calls",
		"tokens": map[string]any{"input": r.Usage.Input, "output": r.Usage.Output, "reasoning": 0,
			"cache": map[string]any{"read": r.Usage.CacheRead, "write": r.Usage.CacheWrite}}}))
}

func (o *opencodeRecorder) end(script fakeScript, total fakeUsage, _ string) int {
	raw, _ := json.Marshal(total)
	_ = os.WriteFile(fakeTotalFile, raw, 0o600)
	if script.Exit != 0 {
		_ = o.out.Encode(map[string]any{"type": "error", "error": map[string]any{
			"type": "provider.unknown", "status": 500, "message": script.Result}})
	}
	return script.Exit
}

// OPENCODE'S RUN IS ITS SERVER'S, AND IS READ STEP BY STEP AS EACH STEP CLOSES.
// A run with tools is a client of a server of OpenCode's own, run only once the
// server lists the crewlet server as connected. Its stream prints a call only
// once it has completed, so a step is told to the host once it closes; and the
// final step prints neither its start nor its counts, which come from the
// session's total, asked of the server.
//
// Mutation: drop late_calls from the profile, and the first step is told before
// its call; drop the route's usage_args, and the final step is never charged.
func TestOpenCodesRunIsReadStepByStepFromItsServer(t *testing.T) {
	t.Parallel()
	p := fakeAs(t, "opencode", routeCase, nil)
	holdsTheRoute(t, p, llm.ServeOptions{
		Relists: true, ToolTimeout: 10 * time.Minute, ObservesRequests: true, Concurrency: llm.CallsAtOnce,
		RecordsCallsLate: true,
	}, llm.Usage{Input: 340, Output: 29, CacheRead: 100})
}

// AN OPENCODE STEP IS TOLD ONCE ITS CALLS HAVE RUN. Told as its prose is read,
// the host would hold the call it is running for a step it was told asked for
// none.
//
// Mutation: drop late_calls from the profile, and when the call runs the host
// has been told of the step with no calls.
func TestAnOpenCodeStepIsToldOnceItsCallsHaveRun(t *testing.T) {
	t.Parallel()
	p := fakeAs(t, "opencode", routeCase, nil)
	host := newTestHost(t, "search")
	var told []llm.Turn
	host.handle("search", func(context.Context, map[string]any) (string, bool) {
		time.Sleep(3 * recordPoll)
		told, _, _, _ = host.snapshot()
		return "3 results", false
	})
	if _, err := p.Complete(t.Context(), phase(host)); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(told) != 0 {
		t.Errorf("while the call ran the host had been told %+v, want no step before its calls showed", told)
	}
	if turns := host.latestTurns(); len(turns) != 2 || len(turns[0].Calls) != 1 {
		t.Errorf("the host was told %+v, want both steps, the first with its call", turns)
	}
}

// A FAILED OPENCODE RUN SAYS WHY BY ITS ERROR EVENT, by field, and quotes
// nothing else it printed — its calls are printed with their arguments and
// results.
//
// Mutation: drop the profile's failure events, and the error says only that
// the CLI exited 1.
func TestAFailedOpenCodeRunSaysWhyByItsErrorEvent(t *testing.T) {
	t.Parallel()
	p := fakeAs(t, "opencode", fakeScript{
		Responses: []fakeResponse{{Text: "posting", Calls: []fakeCall{
			{Name: "post", Args: map[string]any{"body": "ARGUMENT-MARKER"}},
		}, Usage: fakeUsage{Input: 5, Output: 5}}},
		Exit: 1, Result: "the vendor is overloaded",
	}, nil)
	host := newTestHost(t, "post")
	host.handle("post", func(context.Context, map[string]any) (string, bool) { return "RESULT-MARKER", false })
	_, err := p.Complete(t.Context(), phase(host))
	if llm.KindOf(err) != llm.KindServer {
		t.Fatalf("err = %v, want a server failure", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "error.type=provider.unknown error.status=500 error.message=the vendor is overloaded") {
		t.Errorf("the error does not say why by the error event's fields: %s", msg)
	}
	for _, leaked := range []string{"ARGUMENT-MARKER", "RESULT-MARKER"} {
		if strings.Contains(msg, leaked) {
			t.Errorf("the error quotes the run's record (%q): %s", leaked, msg)
		}
	}
}
