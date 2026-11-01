package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// Gemini CLI, as the fake plays it: the server from the call's workspace
// settings, no id of its own with a call, and the record in the chat file under
// the home's tmp directory — a `gemini` entry per response written as it
// arrives and again, whole, with its calls once they have run, then a `user`
// entry with their results — and the run's report on stdout as one document at
// the end (measured on 0.65.0; see the profile).
func init() {
	fakeHarnesses["gemini-cli"] = fakeHarness{
		server: func() (string, []string, error) {
			raw, err := os.ReadFile(filepath.Join(".gemini", "settings.json"))
			if err != nil {
				return "", nil, err
			}
			return serverIn(raw)
		},
		recorder: newGeminiRecorder,
	}
}

type geminiRecorder struct {
	chat    *json.Encoder
	session string
	model   string
	prompt  string
}

func newGeminiRecorder(script fakeScript, prompt []byte) (fakeRecorder, error) {
	tmp := filepath.Join(os.Getenv("HOME"), ".gemini", "tmp")
	if script.ForeignRecord {
		other, err := recordFile(filepath.Join(tmp, "another-project", "chats", "session-2026-10-25T00-00-aaaaaaaa.jsonl"))
		if err != nil {
			return nil, err
		}
		_ = other.Encode(map[string]any{"sessionId": "another-session", "kind": "main"})
		_ = other.Encode(geminiEntry("g-other", "FOREIGN", "fake-model-1", fakeUsage{Input: 999, Output: 999}, nil))
	}
	session := argAfter("--session-id")
	if len(session) < 8 {
		return nil, fmt.Errorf("--session-id %q", session)
	}
	cwd, _ := os.Getwd()
	chat, err := recordFile(filepath.Join(tmp, filepath.Base(cwd), "chats",
		"session-2026-10-25T00-00-"+session[:8]+".jsonl"))
	if err != nil {
		return nil, err
	}
	return &geminiRecorder{chat: chat, session: session, prompt: string(prompt)}, nil
}

// geminiEntry is one response as the chat records it: its whole prompt as
// `input`, the cached part inside it.
func geminiEntry(id, text, model string, u fakeUsage, calls []any) map[string]any {
	entry := map[string]any{"id": id, "type": "gemini", "content": text, "model": model,
		"tokens": map[string]any{"input": u.Input + u.CacheRead, "output": u.Output, "cached": u.CacheRead,
			"thoughts": 0, "tool": 0, "total": u.Input + u.CacheRead + u.Output}}
	if calls != nil {
		entry["toolCalls"] = calls
	}
	return entry
}

func (g *geminiRecorder) begin(model string, _ []string) {
	g.model = model
	_ = g.chat.Encode(map[string]any{"sessionId": g.session, "kind": "main"})
	_ = g.chat.Encode(map[string]any{"id": "u0", "type": "user", "content": []any{map[string]any{"text": g.prompt}}})
}

func (g *geminiRecorder) asked(i int, r fakeResponse, _ []string) {
	_ = g.chat.Encode(geminiEntry(fmt.Sprintf("g%d", i+1), r.Text, g.model, r.Usage, nil))
}

func (g *geminiRecorder) answered(i int, r fakeResponse, ids []string, results []string) {
	if len(r.Calls) == 0 {
		return
	}
	var calls, responses []any
	for j, c := range r.Calls {
		name := "mcp_crewlet_" + c.Name
		if c.Own {
			name = c.Name
		}
		calls = append(calls, map[string]any{"id": ids[j], "name": name, "args": orEmpty(c.Args),
			"status": "success", "result": []any{map[string]any{"functionResponse": map[string]any{
				"id": ids[j], "name": name, "response": map[string]any{"output": results[j]}}}}})
		responses = append(responses, map[string]any{"functionResponse": map[string]any{
			"id": ids[j], "name": name, "response": map[string]any{"output": results[j]}}})
	}
	_ = g.chat.Encode(geminiEntry(fmt.Sprintf("g%d", i+1), r.Text, g.model, r.Usage, calls))
	_ = g.chat.Encode(map[string]any{"id": fmt.Sprintf("u%d", i+1), "type": "user", "content": responses})
}

func (g *geminiRecorder) end(script fakeScript, total fakeUsage, last string) int {
	if script.Exit != 0 {
		raw, _ := json.Marshal(map[string]any{"session_id": g.session, "error": map[string]any{
			"type": "FatalTurnLimitedError", "message": "Reached max session turns for this session.",
			"code": script.Exit}})
		fmt.Fprintln(os.Stderr, string(raw))
		return script.Exit
	}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"session_id": g.session, "response": last,
		"stats": map[string]any{"models": map[string]any{g.model: map[string]any{"tokens": map[string]any{
			"prompt": total.Input + total.CacheRead, "input": total.Input, "cached": total.CacheRead,
			"candidates": total.Output, "thoughts": 0, "tool": 0,
		}}}}})
	return 0
}

// GEMINI CLI'S RUN IS READ FROM ITS OWN CHAT. Its output is one document at the
// end, so each request is read from the chat it keeps under the home's tmp
// directory, by the entry the CLI writes as each response arrives — and,
// written again whole once the response's calls have run, read for its calls
// without its prose twice. The chat is named after eight characters of the
// session id, so it is told from a concurrent run's in another project's
// directory by its first line.
//
// Mutation: drop whole from the entry's rule, and the first request's prose is
// told twice; drop the owner, and the other run's FOREIGN response is this
// run's.
func TestGeminisRunIsReadFromItsOwnChat(t *testing.T) {
	t.Parallel()
	script := routeCase
	script.ForeignRecord = true
	p := fakeAs(t, "gemini-cli", script, nil)
	host := holdsTheRoute(t, p, llm.ServeOptions{
		Relists: true, ToolTimeout: 10 * time.Minute, ObservesRequests: true, Concurrency: llm.CallsAtOnce,
		RecordsCallsLate: true,
	}, llm.Usage{Input: 340, Output: 29, CacheRead: 100})
	for _, turn := range host.latestTurns() {
		if strings.Contains(turn.Text, "FOREIGN") {
			t.Errorf("another run's chat was read as this one's: %+v", turn)
		}
	}
}

// A GEMINI REQUEST IS TOLD ONCE ITS CALLS HAVE RUN, AND CHARGED BEFORE. The
// chat shows a response's calls only in the entry it writes again once they
// have run, so the host is told the request when the record has moved past it
// — told on its first writing, it would hold the call it is running for a
// request it was told asked for none — while the request's counts are charged
// the moment they are read.
//
// Mutation: drop late_calls from the profile, and when the call runs the host
// has been told of the request with no calls.
func TestAGeminiRequestIsToldOnceItsCallsHaveRun(t *testing.T) {
	t.Parallel()
	p := fakeAs(t, "gemini-cli", routeCase, nil)
	host := newTestHost(t, "search")
	var told []llm.Turn
	var spent llm.Usage
	host.handle("search", func(context.Context, map[string]any) (string, bool) {
		// The record and the call race by a poll; a request shown without
		// its call within it would be one the host had misplaced.
		time.Sleep(3 * recordPoll)
		told, _, spent, _ = host.snapshot()
		return "3 results", false
	})
	if _, err := p.Complete(t.Context(), phase(host)); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(told) != 0 {
		t.Errorf("while the call ran the host had been told %+v, want no request before its calls showed", told)
	}
	if spent.Input != 140 {
		t.Errorf("while the call ran the host had been charged %+v, want the request's 140 input", spent)
	}
	if turns := host.latestTurns(); len(turns) != 2 || len(turns[0].Calls) != 1 {
		t.Errorf("the host was told %+v, want both requests, the first with its call", turns)
	}
}

// A GEMINI RUN WHOSE POLICY FILE DID NOT LOAD IS STOPPED THERE, AND FAILS. The
// CLI reports a policy file it cannot read and carries on without its rules —
// the deny-all of its own tools, or the seat's allow — so the shipped profile
// stops it at that line.
//
// Mutation: drop failure_markers from the profile, and the CLI below is left
// to run (it holds until the test's own deadline).
func TestAGeminiRunWhosePolicyDidNotLoadIsStopped(t *testing.T) {
	t.Parallel()
	p := fakeAs(t, "gemini-cli", fakeScript{
		Stderr: "[USER] Policy file error in crewlet-mcp.toml:\n  TOML parsing failed\n", Hang: true,
	}, nil)
	p.termGrace = 300 * time.Millisecond
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, err := p.Complete(ctx, phase(newTestHost(t, "search")))
	if llm.KindOf(err) != llm.KindServer || !strings.Contains(err.Error(), "Policy file error") {
		t.Fatalf("err = %v, want a server failure naming the CLI's own line", err)
	}
}

// A GEMINI RUN ITS OWN TURN LIMIT ENDED IS AN ANSWER STOPPED AT THE CAP: exit
// 53, the CLI's code for its FatalTurnLimitedError and nothing else, its report
// on stderr.
//
// Mutation: drop the profile's cap, and this is a server failure.
func TestAGeminiRunItsTurnLimitEndedStopsAtTheCap(t *testing.T) {
	t.Parallel()
	p := fakeAs(t, "gemini-cli", fakeScript{Exit: 53, Responses: []fakeResponse{
		{Text: "still going", Calls: []fakeCall{{Name: "search"}}, Usage: fakeUsage{Input: 5, Output: 5}},
	}}, nil)
	comp, err := p.Complete(t.Context(), phase(newTestHost(t, "search")))
	if err != nil {
		t.Fatalf("a capped run came back as a failure: %v", err)
	}
	if comp.StopReason != llm.StopCap {
		t.Errorf("stop = %q, want the cap", comp.StopReason)
	}
}

// THE SEAT'S TOOLS ARE ALLOWED IN A POLICY FILE OF THEIR OWN, IN THE HOME, and
// no run names a policy on argv: --policy replaces the home's policies rather
// than adding to it, and a run handed the seat's allow that way lost the
// deny-all of the CLI's own tools (measured on 0.65.0: its file, plan,
// sub-agent and topic tools offered beside the seat's).
//
// Mutation: hand the allow with --policy, or fold it into the deny-all's file
// without its own, and this names it.
func TestGeminisSeatAllowIsAPolicyFileBesideTheDenial(t *testing.T) {
	t.Parallel()
	p, _ := harnessprofile.Builtin("gemini-cli")
	if slices.Contains(slices.Concat(p.CompleteArgs, p.MCP.Args, p.MCP.AllowArgs), "--policy") {
		t.Errorf("a run names a policy on argv (%q, %q), which drops the home's deny-all", p.MCP.Args, p.MCP.AllowArgs)
	}
	var deny, allow bool
	for _, f := range p.SeedFiles {
		switch {
		case f.Path == ".gemini/policies/crewlet.toml":
			deny = strings.Contains(f.Content, `toolName = "*"`) && strings.Contains(f.Content, `decision = "deny"`)
		case f.Path == ".gemini/policies/crewlet-mcp.toml":
			allow = strings.Contains(f.Content, `mcpName = "crewlet"`) && strings.Contains(f.Content, `decision = "allow"`)
		}
	}
	if !deny || !allow {
		t.Errorf("the home's policies hold the deny-all %v and the seat's allow %v, want both, each its own file", deny, allow)
	}
	if !slices.Contains(p.MCP.Args, "--allowed-mcp-server-names") {
		t.Errorf("a run admits every MCP server it finds (%q): a home extension's started beside the seat's", p.MCP.Args)
	}
}
