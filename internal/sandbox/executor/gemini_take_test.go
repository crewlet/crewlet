package executor_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// takeBridge is a stand-in for the run's bridge as the take script dials it:
// streamable HTTP answering initialize, the initialized notification and one
// tools/call with answer, and the skill route; it records every call and
// skill report.
type takeBridge struct {
	answer string
	mu     sync.Mutex
	calls  []map[string]any
	skills []string
}

func (b *takeBridge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if r.URL.Path == "/skills" {
		b.mu.Lock()
		b.skills = append(b.skills, string(body))
		b.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var msg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params map[string]any  `json:"params"`
	}
	if r.Method != http.MethodPost || json.Unmarshal(body, &msg) != nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	var result any
	switch msg.Method {
	case "initialize":
		w.Header().Set("mcp-session-id", "take-session")
		result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
			"serverInfo": map[string]any{"name": "crewlet", "version": "1"}}
	case "tools/call":
		b.mu.Lock()
		b.calls = append(b.calls, msg.Params)
		b.mu.Unlock()
		result = map[string]any{"content": []any{map[string]any{"type": "text", "text": b.answer}}}
	default:
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set("content-type", "text/event-stream")
	reply, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
	_, _ = w.Write([]byte("event: message\ndata: " + string(reply) + "\n\n"))
}

// THE TAKE SCRIPT TAKES AT THE FINISH ALONE AND PRINTS THE ENGINE'S ANSWER AS
// IT CAME. Driven under node against a stand-in bridge with the configuration
// WriteConfig writes: an AfterAgent firing takes as Stop with no agent,
// records the take's token beside its text's digest and prints the answer
// whole but the token, a finish continuing an earlier block included; an
// AfterTool firing takes nothing; and an activated skill is reported by the
// name it was handed under.
//
// Mutations, each red: take at AfterTool; take nothing while stop_hook_active;
// report the skill by Gemini CLI's spelling; hand Gemini CLI the token;
// record no take.
func TestTheTakeScriptTakesAtTheFinishAlone(t *testing.T) {
	t.Parallel()
	node := geminiNode(t)
	// THE ENGINE'S ANSWER CARRIES THE TAKE'S TOKEN, which the script records
	// beside the text and leaves out of what Gemini CLI is handed.
	const answer = `{"decision":"block","reason":"## A note","crewlet_take":"take-1"}`
	const handed = `{"decision":"block","reason":"## A note"}`
	for name, tc := range map[string]struct {
		input map[string]any
		event string // the boundary taken, "" for none
		skill string // the skill reported, "" for none
	}{
		"after a call": {input: map[string]any{"hook_event_name": "AfterTool", "tool_name": "mcp_crewlet_echo",
			"tool_response": map[string]any{"llmContent": []any{map[string]any{"text": "echo: hello"}}}}},
		"at the finish": {input: map[string]any{"hook_event_name": "AfterAgent", "stop_hook_active": false},
			event: "Stop"},
		"at a finish continuing on a block": {input: map[string]any{"hook_event_name": "AfterAgent",
			"stop_hook_active": true}, event: "Stop"},
		"after a skill's activation": {input: map[string]any{"hook_event_name": "AfterTool",
			"tool_name": "activate_skill", "tool_input": map[string]any{"name": "crewlet-probe"},
			"tool_response": map[string]any{"llmContent": "<activated_skill name=\"crewlet-probe\">"}},
			skill: `{"skill":"crewlet:probe"}`},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			bridge := &takeBridge{answer: answer}
			server := httptest.NewServer(bridge)
			t.Cleanup(server.Close)
			dir := t.TempDir()
			record := filepath.Join(dir, "takes.jsonl")
			config, _ := json.Marshal(map[string]any{"endpoint": server.URL + "/mcp", "tool": "steer_take",
				"skills": server.URL + "/skills", "namespace": "crewlet"})
			path := filepath.Join(dir, "take.json")
			if err := os.WriteFile(path, config, 0o600); err != nil {
				t.Fatal(err)
			}
			input, _ := json.Marshal(tc.input)
			cmd := exec.CommandContext(t.Context(), node, "gemini_take.mjs", path)
			cmd.Env = append(os.Environ(), executor.GeminiTakesEnv+"="+record)
			cmd.Stdin = strings.NewReader(string(input))
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("the take script: %v", err)
			}
			want, wantRecord := "{}", ""
			if tc.event != "" {
				want, wantRecord = handed, `{"take":"take-1","digest":"`+executor.ItemDigest("## A note")+`"}`+"\n"
			}
			if got, _ := os.ReadFile(record); string(got) != wantRecord {
				t.Errorf("the script recorded %q, want %q", got, wantRecord)
			}
			if string(out) != want {
				t.Errorf("the hook answered %q, want %q", out, want)
			}
			bridge.mu.Lock()
			defer bridge.mu.Unlock()
			switch {
			case tc.event == "" && len(bridge.calls) != 0:
				t.Errorf("the script took %v, want no take", bridge.calls)
			case tc.event != "" && (len(bridge.calls) != 1 || bridge.calls[0]["name"] != "steer_take"):
				t.Errorf("the script called %v, want one steer_take", bridge.calls)
			case tc.event != "":
				args, _ := bridge.calls[0]["arguments"].(map[string]any)
				if args["event"] != tc.event || args["agent"] != "" {
					t.Errorf("the take passed %v, want event %s and no agent", args, tc.event)
				}
			}
			if got := strings.Join(bridge.skills, "|"); got != tc.skill {
				t.Errorf("the script reported skills %q, want %q", got, tc.skill)
			}
		})
	}
}

// THE TAKE SCRIPT DIGESTS A TAKE'S TEXT AS THE ENGINE DOES: the digest it
// records beside each take, computed in node, is executor.ItemDigest of the
// block's reason, byte for byte, for ASCII, CJK, characters outside the BMP,
// and the escapes JSON writes, so the session's user messages, digested in
// Go, find the takes the script recorded.
//
// Mutations, each red: digest the reason as Latin-1 in node, or print the
// digest in base64, or digest the reason's JSON rather than its text.
func TestTheTakeScriptDigestsATakesTextAsTheEngineDoes(t *testing.T) {
	t.Parallel()
	node := geminiNode(t)
	for _, reason := range []string{"## A note from ada", "## 注意 — 日本語のメモ", "## 🙂 𝄞 astral",
		"line\nbreak\ttab \"quote\" \\ back\u2028slash"} {
		t.Run(reason, func(t *testing.T) {
			t.Parallel()
			answer, _ := json.Marshal(map[string]string{"decision": "block", "reason": reason, "crewlet_take": "t-1"})
			bridge := &takeBridge{answer: string(answer)}
			server := httptest.NewServer(bridge)
			t.Cleanup(server.Close)
			dir := t.TempDir()
			record := filepath.Join(dir, "takes.jsonl")
			config, _ := json.Marshal(map[string]any{"endpoint": server.URL + "/mcp", "tool": "steer_take",
				"namespace": "crewlet"})
			path := filepath.Join(dir, "take.json")
			if err := os.WriteFile(path, config, 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.CommandContext(t.Context(), node, "gemini_take.mjs", path)
			cmd.Env = append(os.Environ(), executor.GeminiTakesEnv+"="+record)
			cmd.Stdin = strings.NewReader(`{"hook_event_name":"AfterAgent","stop_hook_active":false}`)
			if out, err := cmd.Output(); err != nil {
				t.Fatalf("the take script: %v (%s)", err, out)
			}
			var got struct{ Take, Digest string }
			raw, err := os.ReadFile(record)
			if err != nil || json.Unmarshal(raw, &got) != nil {
				t.Fatalf("the script recorded %q (%v)", raw, err)
			}
			if want := executor.ItemDigest(reason); got.Take != "t-1" || got.Digest != want {
				t.Errorf("the script recorded %+v, want take t-1 and the engine's digest %s", got, want)
			}
		})
	}
}
