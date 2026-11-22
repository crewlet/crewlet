package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeMCP is a bridge's MCP endpoint as a take sees it: it answers
// initialize, initialized and one tools/call of steer_take with answer (or
// with reply whole, where it is set), as an event stream with a progress
// notice before the result, and records each call's arguments and the skill
// route's posts.
type fakeMCP struct {
	srv    *httptest.Server
	answer string
	reply  string
	mu     sync.Mutex
	calls  []map[string]string
	skills []string
	auth   []string
}

func newFakeMCP(t *testing.T, answer string) *fakeMCP {
	t.Helper()
	m := &fakeMCP{answer: answer}
	m.srv = httptest.NewServer(http.HandlerFunc(m.serve))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *fakeMCP) serve(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r.URL.Path == "/skills" {
		var body struct {
			Skill string `json:"skill"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		m.skills = append(m.skills, body.Skill)
		return
	}
	if r.Method != http.MethodPost {
		return
	}
	m.auth = append(m.auth, r.Header.Get("Authorization"))
	var msg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Name      string            `json:"name"`
			Arguments map[string]string `json:"arguments"`
		} `json:"params"`
	}
	_ = json.NewDecoder(r.Body).Decode(&msg)
	w.Header().Set("Mcp-Session-Id", "s-1")
	switch msg.Method {
	case "initialize":
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18"}}`)
	case "tools/call":
		m.calls = append(m.calls, msg.Params.Arguments)
		result, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 2,
			"result": map[string]any{"content": []any{map[string]string{"type": "text", "text": m.answer}}}})
		if m.reply != "" {
			result = []byte(m.reply)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n"+
			"event: message\ndata: "+string(result)+"\n\n")
	default:
		w.WriteHeader(http.StatusAccepted)
	}
}

// takeConfigFile writes cfg for m and is its path.
func takeConfigFile(t *testing.T, m *fakeMCP) string {
	t.Helper()
	raw, _ := json.Marshal(takeConfig{Endpoint: m.srv.URL + "/mcp", Tool: "steer_take",
		Headers: map[string]string{"Authorization": "Bearer " + testToken}, Skills: m.srv.URL + "/skills"})
	path := filepath.Join(t.TempDir(), "take.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// museEnv is a main thread's hook environment.
func museEnv(name string) string {
	switch name {
	case "MUSE_THREAD_ID", "MUSE_SESSION_ID":
		return "s-main"
	}
	return ""
}

// fire runs one firing of the Muse Code adapter on input.
func fire(t *testing.T, args []string, input string, getenv func(string) string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errs bytes.Buffer
	code, once := runOnce(context.Background(), args, getenv, strings.NewReader(input), &out, &errs)
	if !once {
		t.Fatalf("%v ran no one-shot adapter", args)
	}
	return code, out.String(), errs.String()
}

// A FIRING HANDS THE ANSWER ON STDOUT AND ITS TOKEN ON STDERR ALONE: the
// answer without the token's field, every other byte as the bridge wrote it
// (raw UTF-8, `<` and `&` unescaped), and the take asked at the bridge's
// boundary for the hook's event, for the main agent.
//
// Mutation: leave the token's field in the answer, or re-escape it.
func TestAMuseFiringHandsTheAnswerOnStdoutAndTheTokenOnStderr(t *testing.T) {
	t.Parallel()
	text := "## A note from ada\n\nUse <main> & ünï 🚀"
	var answer bytes.Buffer
	enc := json.NewEncoder(&answer)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(map[string]any{"crewlet_take": "[crewlet take t-1]",
		"hookSpecificOutput": map[string]string{"hookEventName": "PostToolUse", "additionalContext": text}})
	withToken := strings.TrimSpace(answer.String())
	m := newFakeMCP(t, withToken)
	cfg := takeConfigFile(t, m)
	code, stdout, stderr := fire(t, []string{"-adapter", "muse-hook", "-config", cfg, "-slot", "1"},
		`{"hook_event_name":"PostToolUse","tool_name":"bash"}`, museEnv)
	if code != exitDone || stderr != "[crewlet take t-1]\n" {
		t.Fatalf("exit %d, stderr %q; want 0 and the token alone", code, stderr)
	}
	if strings.Contains(stdout, "crewlet_take") || !strings.Contains(stdout, "<main> & ünï 🚀") ||
		strings.Contains(stdout, `\u`) {
		t.Errorf("stdout %q: want the answer without its token, raw UTF-8", stdout)
	}
	var got struct {
		HookSpecificOutput struct {
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if json.Unmarshal([]byte(stdout), &got) != nil || got.HookSpecificOutput.AdditionalContext != text {
		t.Errorf("stdout %q does not hand the note whole", stdout)
	}
	if len(m.calls) != 1 || m.calls[0]["event"] != boundaryBatch || m.calls[0]["agent"] != "" ||
		m.auth[0] != "Bearer "+testToken {
		t.Errorf("calls %v (auth %v), want one take at %s for the main agent", m.calls, m.auth, boundaryBatch)
	}
	_, _, _ = fire(t, []string{"-adapter", "muse-hook", "-config", cfg}, `{"hook_event_name":"Stop"}`, museEnv)
	if len(m.calls) != 2 || m.calls[1]["event"] != boundaryFinish {
		t.Errorf("a Stop firing took at %v, want %s", m.calls, boundaryFinish)
	}
}

// THE WIDEST ITEM PASSES THROUGH AS ITS RAW BYTES: an answer whose text is
// 4,505 units of U+2028, written raw by the engine, reaches stdout raw and
// within Muse Code's 16,384 bytes, never re-escaped at six bytes a unit as
// encoding/json writes U+2028 in a string it encodes.
//
// Mutation: decode the answer's text and encode it again.
func TestAMuseFiringKeepsTheWidestItemRaw(t *testing.T) {
	t.Parallel()
	text := strings.Repeat("\u2028", 4505)
	m := newFakeMCP(t, `{"crewlet_take":"[crewlet take t-1]",`+
		`"hookSpecificOutput":{"hookEventName":"PostToolUse","additionalContext":"`+text+`"}}`)
	_, stdout, stderr := fire(t, []string{"-adapter", "muse-hook", "-config", takeConfigFile(t, m), "-slot", "1"},
		`{"hook_event_name":"PostToolUse","tool_name":"bash"}`, museEnv)
	if stderr != "[crewlet take t-1]\n" || !strings.Contains(stdout, text) || len(stdout) > museAnswerBytes {
		t.Errorf("stdout is %d bytes (raw %v), stderr %q: want the text raw within %d", len(stdout),
			strings.Contains(stdout, text), stderr, museAnswerBytes)
	}
}

// A TAKE THE BRIDGE DID NOT ANSWER WITH A RESULT HANDS NOTHING OVER AND SAYS
// WHY: a tool error, or a JSON-RPC error with no result, writes nothing on
// stdout and no token on stderr, so nothing is confirmed and the take's notes
// are told again, and the hook's stderr names the answer.
//
// Mutation: read either answer as an empty take.
func TestATakeAnsweredWithNoResultHandsNothingOver(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct{ reply, says string }{
		"tool error": {`{"jsonrpc":"2.0","id":2,"result":{"isError":true,"content":[{"type":"text",` +
			`"text":"{\"crewlet_take\":\"[crewlet take t-1]\"}"}]}}`, "with a tool error"},
		"no result": {`{"jsonrpc":"2.0","id":2,"error":{"code":-32000,"message":"down"}}`, "with no tool result"},
	} {
		m := newFakeMCP(t, "")
		m.mu.Lock()
		m.reply = c.reply
		m.mu.Unlock()
		code, stdout, stderr := fire(t, []string{"-adapter", "muse-hook", "-config", takeConfigFile(t, m), "-slot", "1"},
			`{"hook_event_name":"PostToolUse","tool_name":"bash"}`, museEnv)
		if code != exitDone || stdout != "" || strings.Contains(stderr, "[crewlet take") ||
			!strings.Contains(stderr, c.says) {
			t.Errorf("%s: exit %d, stdout %q, stderr %q; want nothing handed over and %q said", name, code,
				stdout, stderr, c.says)
		}
	}
}

// A SUBAGENT'S FIRING TAKES NOTHING: a note handed to it would bind one
// delegated piece of work and be lost with it.
//
// Mutation: drop the thread check.
func TestASubagentsFiringTakesNothing(t *testing.T) {
	t.Parallel()
	m := newFakeMCP(t, `{"x":1,"crewlet_take":"[crewlet take t-1]"}`)
	child := func(name string) string {
		if name == "MUSE_THREAD_ID" {
			return "s-child"
		}
		return museEnv(name)
	}
	_, stdout, stderr := fire(t, []string{"-adapter", "muse-hook", "-config", takeConfigFile(t, m)},
		`{"hook_event_name":"PostToolUse"}`, child)
	if stdout != "" || stderr != "" || len(m.calls) != 0 {
		t.Fatalf("a subagent's firing answered %q / %q after %d takes", stdout, stderr, len(m.calls))
	}
}

// AN ANSWER PAST MUSE CODE'S STDOUT CEILING IS NEVER HANDED OVER, nor its
// token said: Muse kills a hook past 16,384 bytes and parses nothing, so the
// take is told again.
//
// Mutation: write the answer whatever its size.
func TestAnAnswerPastTheCeilingIsNotHandedOver(t *testing.T) {
	t.Parallel()
	big := `{"x":"` + strings.Repeat("y", museAnswerBytes) + `","crewlet_take":"[crewlet take t-1]"}`
	m := newFakeMCP(t, big)
	_, stdout, stderr := fire(t, []string{"-adapter", "muse-hook", "-config", takeConfigFile(t, m)},
		`{"hook_event_name":"PostToolUse"}`, museEnv)
	if stdout != "" || strings.Contains(stderr, "[crewlet take") {
		t.Fatalf("an answer past the ceiling was handed over: %d bytes, stderr %q", len(stdout), stderr)
	}
}

// A SKILL HOOK REPORTS A LOAD THE HARNESS MADE, by the name it was read under,
// and a loads-only one takes nothing; a read that failed reports nothing.
//
// Mutation: report a failed read, or take a note at a loads-only hook.
func TestASkillHookReportsALoadAndTakesNothing(t *testing.T) {
	t.Parallel()
	m := newFakeMCP(t, `{"x":1,"crewlet_take":"[crewlet take t-1]"}`)
	cfg := takeConfigFile(t, m)
	for _, response := range []string{`"<read-skill-result name=\"crewlet:fix\" status=\"ok\">body"`,
		`"<read-skill-result name=\"crewlet:fix\" status=\"error\">gone"`} {
		_, stdout, _ := fire(t, []string{"-adapter", "muse-hook", "-config", cfg, "-skill-loads", "-loads-only"},
			`{"hook_event_name":"PostToolUse","tool_name":"read_skill","tool_input":{"name":"crewlet:fix"},"tool_response":`+
				response+`}`, museEnv)
		if stdout != "" {
			t.Errorf("a skill hook answered %q", stdout)
		}
	}
	if len(m.skills) != 1 || m.skills[0] != "crewlet:fix" || len(m.calls) != 0 {
		t.Fatalf("skills %v, takes %d; want the one load reported and no take", m.skills, len(m.calls))
	}
}

// A FIRING THE BRIDGE CANNOT ANSWER HANDS NOTHING OVER AND EXITS 0, so the
// harness goes on and the note is told again; the bridge's address is never
// printed.
//
// Mutation: exit non-zero, or print the address in the error.
func TestAFiringTheBridgeCannotAnswerHandsNothingOver(t *testing.T) {
	t.Parallel()
	m := newFakeMCP(t, "")
	m.srv.Close()
	cfg := takeConfigFile(t, m)
	code, stdout, stderr := fire(t, []string{"-adapter", "muse-hook", "-config", cfg},
		`{"hook_event_name":"PostToolUse"}`, museEnv)
	if code != exitDone || stdout != "" || strings.Contains(stderr, m.srv.URL) || strings.Contains(stderr, "[crewlet take") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}
