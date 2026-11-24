package executor_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/net/websocket"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/shellword"
)

// The stand-in app-server: this test binary, run by a box's stand-in `codex
// app-server` with codexStandInSocket set, listens on that socket as Codex's
// does and answers the note helper's JSON-RPC over WebSocket, recording what
// it was sent in the case's record directory (codexStandInRecord). A process
// of its own, because the run's script waits for a socket its server made and
// stops that server with it.
const (
	codexStandInSocket = "CREWLET_TEST_CODEX_SOCKET"
	codexStandInRecord = "CREWLET_TEST_CODEX_RECORD"
)

func TestMain(m *testing.M) {
	if socket := os.Getenv(codexStandInSocket); socket != "" {
		os.Exit(serveCodexStandIn(socket, os.Getenv(codexStandInRecord)))
	}
	os.Exit(m.Run())
}

// codexStandInTurn is the turn the stand-in says is in flight.
const codexStandInTurn = "turn-standin"

// standInNote appends line to the record's file name.
func standInNote(record, name string, line []byte) {
	f, err := os.OpenFile(filepath.Join(record, name), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // the case's own file
	if err == nil {
		_, _ = f.Write(append(line, '\n'))
		_ = f.Close()
	}
}

// serveCodexStandIn serves socket until a SECOND SIGTERM, as Codex's
// app-server finishes the turn in flight on the first (measured on 0.171.0),
// then records `server` in record/order and ends with 0.
func serveCodexStandIn(socket, record string) int {
	ln, err := net.Listen("unix", socket)
	if err != nil {
		fmt.Fprintln(os.Stderr, "stand-in app-server:", err)
		return 1
	}
	stop := make(chan os.Signal, 2)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-stop
		<-stop
		standInNote(record, "order", []byte("server"))
		_ = ln.Close()
	}()
	server := &http.Server{ReadHeaderTimeout: time.Minute,
		Handler: websocket.Handler(func(ws *websocket.Conn) { answerCodexStandIn(ws, record) })}
	_ = server.Serve(ln)
	return 0
}

// answerCodexStandIn answers one client: initialize, thread/resume and
// thread/turns/list with a turn in flight, then as many token-usage updates
// as record/steps says (each a model response); turn/steer, recorded to
// record/steers; and turn/interrupt, recorded to record/interrupted. Where
// record/idle exists no turn is in flight, and where record/final exists the
// turn completed before the interrupt reached it: either way an interrupt is
// refused as Codex refuses one of no turn, and recorded to
// record/interrupt-refused instead.
func answerCodexStandIn(ws *websocket.Conn, record string) {
	dec := json.NewDecoder(ws)
	thread := ""
	_, idleErr := os.Stat(filepath.Join(record, "idle"))
	_, finalErr := os.Stat(filepath.Join(record, "final"))
	idle, final := idleErr == nil, finalErr == nil
	for {
		var call struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if dec.Decode(&call) != nil {
			return
		}
		if len(call.ID) == 0 {
			continue
		}
		var result any = map[string]any{}
		var refusal map[string]any
		turns := []map[string]string{{"id": codexStandInTurn, "status": "inProgress"}}
		if idle {
			turns[0]["status"] = "completed"
		}
		switch call.Method {
		case "thread/resume":
			var p struct {
				ThreadID string `json:"threadId"`
			}
			_ = json.Unmarshal(call.Params, &p)
			thread = p.ThreadID
			result = map[string]any{"thread": map[string]any{"turns": turns}}
		case "thread/turns/list":
			result = map[string]any{"data": turns}
		case "turn/steer":
			result = map[string]string{"turnId": codexStandInTurn}
			standInNote(record, "steers", call.Params)
		case "turn/interrupt":
			if idle || final {
				standInNote(record, "interrupt-refused", call.Params)
				refusal = map[string]any{"code": -32600, "message": "no active turn to interrupt"}
				break
			}
			standInNote(record, "interrupted", call.Params)
		}
		reply := map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": result}
		if refusal != nil {
			delete(reply, "result")
			reply["error"] = refusal
		}
		answer, _ := json.Marshal(reply)
		if _, err := ws.Write(answer); err != nil {
			return
		}
		if call.Method != "thread/turns/list" {
			continue
		}
		raw, _ := os.ReadFile(filepath.Join(record, "steps")) //nolint:gosec // the case's own file
		steps, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
		if steps == 0 || idle {
			continue
		}
		// THE TURN STARTED FIRST, as Codex notifies a subscribed client, so
		// the updates after it read in order are of the turn in flight.
		started, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "turn/started",
			"params": map[string]any{"threadId": thread, "turn": map[string]string{"id": codexStandInTurn}}})
		if _, err := ws.Write(started); err != nil {
			return
		}
		for i := 1; i <= steps; i++ {
			update, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "thread/tokenUsage/updated",
				"params": map[string]any{"threadId": thread, "turnId": codexStandInTurn,
					"tokenUsage": map[string]any{"total": map[string]int{"totalTokens": 100 * i}}}})
			if _, err := ws.Write(update); err != nil {
				return
			}
		}
	}
}

// codexStandInClone is the stand-in `exec`'s clone during its turn: where
// record/clone-src names a directory it is copied to record/clone-dst, and
// the PostToolUse hook the server was declared runs twice, as Codex runs it
// after a tool call, its answers in record/nested-1 and nested-2.
const codexStandInClone = `  if [ -e "$rec/clone-src" ]; then
    cp -R "$(cat "$rec/clone-src")" "$(cat "$rec/clone-dst")"
    hc=$(sed -n 's/^hooks\.PostToolUse=.*command="\([^"]*\)".*/\1/p' "$rec/server-argv")
    for i in 1 2; do printf '%s\n' '{"hook_event_name":"PostToolUse","tool_name":"exec_command"}' | sh -c "$hc" > "$rec/nested-$i" 2> "$rec/nested-$i.err"; done
  fi`

// codexStandInThread is the thread the stand-in `codex exec` reports for a
// session it starts.
const codexStandInThread = "019a1f62-fbda-7aa2-af57-6c0abeb8e50b"

// codexStandIn is a box's stand-in `codex`: `--version` prints release;
// `app-server` over stdio (the launch's guard) records its argv, directory,
// home and requests and prints record/config-read, else the run's own layers;
// `app-server --listen` records its argv and runs this binary as the server;
// `exec` records argv, stdin and environment, clones ([codexStandInClone]),
// prints a turn answering `done` on its thread (or [codexStandInThread]) and
// appends one turn's rollout lines (300 in, 200 cached, 40 out, a 258400
// window, two plan windows), waiting first for a steer (record/wait-steer) or
// a refused interrupt (record/final); record/block works until interrupted
// (exit 1, turn_aborted), its server stops (exit 1) or SIGTERM (record/order);
// record/own-group gives it a group of its own, only the script's `end`
// reaching it, since a shell traps two SIGTERMs in one command once.
func codexStandIn(t *testing.T, box sandbox.Sandbox, release, record string) {
	t.Helper()
	bin, err := os.Executable()
	if err != nil {
		t.Fatalf("the test binary: %v", err)
	}
	p := executor.PathsFor(box)
	rollout := strings.Join([]string{
		`{"timestamp":"2026-11-22T09:00:00.000Z","ordinal":90,"type":"turn_context","payload":{"turn_id":"t9","model":"gpt-test"}}`,
		`{"timestamp":"2026-11-22T09:00:01.000Z","ordinal":91,"type":"token_usage_record","payload":{"turn_id":"t9","turn_token_usage":{"input_tokens":300,"cached_input_tokens":200,"cache_write_input_tokens":0,"output_tokens":40,"reasoning_output_tokens":4,"total_tokens":340}}}`,
		`{"timestamp":"2026-11-22T09:00:02.000Z","ordinal":92,"type":"event_msg","payload":{"type":"token_count","info":{"model_context_window":258400},"rate_limits":{"limit_id":"codex","primary":{"used_percent":37.5,"window_minutes":300,"resets_at":1795287240},"secondary":{"used_percent":12,"window_minutes":10080,"resets_at":1795372440}}}}`,
	}, "\n")
	script := fmt.Sprintf(`#!/bin/sh
rec=%[1]s
if [ "$1" = app-server ] && [ "$2" != --listen ]; then
  printf '%%s\n' "$@" > "$rec/guard-argv"; pwd -P > "$rec/guard-cwd"; printf '%%s\n' "$CODEX_HOME" > "$rec/guard-home"
  cp "$CODEX_HOME/config.toml" "$rec/guard-config.toml" 2> /dev/null
  i=0; while [ "$i" -lt 3 ] && IFS= read -r l; do printf '%%s\n' "$l" >> "$rec/guard-stdin"; i=$((i+1)); done
  if [ -e "$rec/config-read" ]; then cat "$rec/config-read"; else printf '%%s\n' '{"id":2,"result":{"config":{},"layers":[{"name":{"type":"sessionFlags"},"config":{}},{"name":{"type":"user","file":"/h/config.toml"},"config":{}}]}}'; fi; exit 0
fi
case "$1" in
--version) echo 'codex-cli %[2]s'; exit 0;;
app-server)
  printf '%%s\n' "$@" > "$rec/server-argv"; pwd > "$rec/server-cwd"
  cp "$CODEX_HOME/config.toml" "$rec/config.toml" 2> /dev/null; cp "$CODEX_HOME/AGENTS.md" "$rec/AGENTS.md" 2> /dev/null; stat -c %%a "$CODEX_HOME/config.toml" > "$rec/config-mode" 2> /dev/null
  for a; do case "$a" in unix://*) s=${a#unix://};; esac; done
  %[3]s="$s" %[4]s="$rec" exec %[5]s;;
exec)
  if [ -e "$rec/own-group" ] && [ -z "$CREWLET_TEST_OWN_GROUP" ]; then CREWLET_TEST_OWN_GROUP=1 exec setsid "$0" "$@"; fi
  printf '%%s\n' "$@" > "$rec/exec-argv"; pwd > "$rec/exec-cwd"; env > "$rec/env"; cat > "$rec/stdin"
%[9]s
  tid=%[6]s; [ "$5" != resume ] || tid=$6
  printf '{"type":"thread.started","thread_id":"%%s"}\n{"type":"turn.started"}\n' "$tid"
  if [ -e "$rec/wait-steer" ]; then i=0; until [ -s "$rec/steers" ] || [ "$i" -ge 200 ]; do sleep 0.05; i=$((i+1)); done; fi
  if [ -e "$rec/final" ]; then i=0; until [ -s "$rec/interrupt-refused" ] || [ "$i" -ge 400 ]; do sleep 0.05; i=$((i+1)); done; fi
  f=$(find "$CODEX_HOME/sessions" -name "rollout-*-$tid.jsonl" | head -n 1)
  [ -z "$f" ] || cp "$f" "$rec/rollout-resumed"
  [ -n "$f" ] || { mkdir -p "$CODEX_HOME/sessions/2026/11/22"; f="$CODEX_HOME/sessions/2026/11/22/rollout-2026-11-22T09-00-00-$tid.jsonl"; printf '%%s\n' %[8]s > "$f"; }
  cat >> "$f" <<'EOF'
%[7]s
EOF
  if [ -e "$rec/block" ]; then trap 'echo exec >> "$rec/order"; exit 143' TERM; : > "$rec/working"; until [ -s "$rec/interrupted" ] || grep -qx server "$rec/order" 2> /dev/null; do sleep 0.05; done; [ ! -s "$rec/interrupted" ] || printf '%%s\n' '{"timestamp":"2026-11-22T09:00:03.000Z","ordinal":93,"type":"event_msg","payload":{"type":"turn_aborted","turn_id":"t9","reason":"interrupted"}}' >> "$f"; exit 1; fi
  printf '%%s\n' '{"type":"item.completed","item":{"id":"item_1","type":"agent_message","text":"done"}}' '{"type":"turn.completed","usage":{"input_tokens":9999,"cached_input_tokens":0,"output_tokens":9999}}'
  exit 0;;
esac
exit 2
`, shellword.Quote(record), release, codexStandInSocket, codexStandInRecord, shellword.Quote(bin), codexStandInThread, rollout,
		shellword.Quote(codexInstructionsLine("the stand-in's first launch")), codexStandInClone)
	writeBoxStandIn(t, box, p.BinDir()+"/codex", script)
}

// codexInstructionsLine is a rollout line as Codex 0.171.0 records a turn's
// standing instructions, text, beside its permissions (measured: the
// developer message a first turn's context opens with).
func codexInstructionsLine(text string) string {
	line, _ := json.Marshal(map[string]any{"timestamp": "2026-11-22T09:00:00.000Z", "ordinal": 2, "type": "response_item",
		"payload": map[string]any{"type": "message", "id": "msg_dev", "role": "developer",
			"content": []map[string]string{{"type": "input_text", "text": text},
				{"type": "input_text", "text": "<permissions instructions>\n[trimmed]\n</permissions instructions>"}},
			"internal_chat_message_metadata_passthrough": map[string]any{
				"content_item_kinds": []string{"generic.developer_instructions", "permissions.instructions"},
				"content_item_metadata": []any{
					map[string]any{"provenance": map[string]any{"type": "developer_instructions", "from_additional_requirements": false}},
					map[string]any{"provenance": map[string]any{"type": "harness", "hook": false}}}}}})
	return string(line)
}

// codexBox is a real `direct` box with the Codex runner installed, and the
// directory its stand-in records to.
func codexBox(t *testing.T, release string) (*executor.Runner, sandbox.Sandbox, string) {
	t.Helper()
	// A STATE DIRECTORY OF ITS OWN under the system's temp root, never the
	// test's, whose name runs the box's socket past the 107 bytes a unix
	// socket's path holds, which the runner rightly refuses.
	state, err := os.MkdirTemp("", "codex") //nolint:usetesting // t.TempDir is named for the test, past a socket path's bound
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(state) })
	local, err := sandbox.NewLocal(sandbox.LocalOptions{Placement: sandbox.Direct, StateDir: state})
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	box, err := local.Create(t.Context(), sandbox.Spec{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// A teardown's context outlives the test's, which ends before cleanups run.
	t.Cleanup(func() { box.Close(context.WithoutCancel(t.Context())) })
	runner := executor.NewCodex()
	if err = runner.Install(t.Context(), box); err != nil {
		t.Fatalf("Install: %v", err)
	}
	record := t.TempDir()
	codexStandIn(t, box, release, record)
	return runner, box, record
}
