package executor_test

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/net/websocket"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// The stand-in app-server: this test binary, run by a box's stand-in `codex
// app-server` with codexStandInSocket set, listens on that socket as Codex's
// does and answers the note helper's JSON-RPC over WebSocket, recording each
// turn/steer to codexStandInSteers. A process of its own, because the run's
// script waits for a socket its server made and stops that server with it.
const (
	codexStandInSocket = "CREWLET_TEST_CODEX_SOCKET"
	codexStandInSteers = "CREWLET_TEST_CODEX_STEERS"
	codexStandInOrder  = "CREWLET_TEST_CODEX_ORDER"
)

func TestMain(m *testing.M) {
	if socket := os.Getenv(codexStandInSocket); socket != "" {
		os.Exit(serveCodexStandIn(socket, os.Getenv(codexStandInSteers), os.Getenv(codexStandInOrder)))
	}
	os.Exit(m.Run())
}

// codexStandInTurn is the turn the stand-in says is in flight.
const codexStandInTurn = "turn-standin"

// serveCodexStandIn serves socket until a SECOND SIGTERM, as Codex's
// app-server finishes the turn in flight on the first (measured on 0.171.0),
// then records `server` in order and ends with 0.
func serveCodexStandIn(socket, steers, order string) int {
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
		if f, err := os.OpenFile(order, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil { //nolint:gosec // the case's own file
			_, _ = f.WriteString("server\n")
			_ = f.Close()
		}
		_ = ln.Close()
	}()
	server := &http.Server{ReadHeaderTimeout: time.Minute,
		Handler: websocket.Handler(func(ws *websocket.Conn) { answerCodexStandIn(ws, steers) })}
	_ = server.Serve(ln)
	return 0
}

// answerCodexStandIn answers one client: initialize, thread/resume with a turn
// in flight, and turn/steer, each recorded where steers names a file.
func answerCodexStandIn(ws *websocket.Conn, steers string) {
	dec := json.NewDecoder(ws)
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
		switch call.Method {
		case "thread/resume":
			result = map[string]any{"thread": map[string]any{
				"turns": []map[string]string{{"id": codexStandInTurn, "status": "inProgress"}}}}
		case "turn/steer":
			result = map[string]string{"turnId": codexStandInTurn}
			if steers != "" {
				f, err := os.OpenFile(steers, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // the case's own file
				if err == nil {
					_, _ = f.Write(append(call.Params, '\n'))
					_ = f.Close()
				}
			}
		}
		answer, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": result})
		if _, err := ws.Write(answer); err != nil {
			return
		}
	}
}

// codexStandInThread is the thread the stand-in `codex exec` reports for a
// session it starts.
const codexStandInThread = "019a1f62-fbda-7aa2-af57-6c0abeb8e50b"

// codexStandIn is a box's stand-in `codex`: `--version` prints release; `app-server`
// records its argv to record/server-argv and runs this binary as the stand-in
// app-server; `exec` records its argv, stdin and environment, prints a turn
// ending in the answer `done` on the thread it continues or
// [codexStandInThread], appends its rollout lines (one turn of 300 input, 200
// cached, 40 out, a window of 258400 and two plan windows) to the thread's
// rollout, and, where record/wait-steer exists, first waits for a steer; where
// record/block exists, it then works until SIGTERM, recording `exec` in
// record/order as it goes.
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
case "$1" in
--version) echo 'codex-cli %[2]s'; exit 0;;
app-server)
  printf '%%s\n' "$@" > "$rec/server-argv"
  for a; do case "$a" in unix://*) s=${a#unix://};; esac; done
  %[3]s="$s" %[4]s="$rec/steers" %[8]s="$rec/order" exec %[5]s;;
exec)
  printf '%%s\n' "$@" > "$rec/exec-argv"; env > "$rec/env"; cat > "$rec/stdin"
  tid=%[6]s; [ "$5" != resume ] || tid=$6
  printf '{"type":"thread.started","thread_id":"%%s"}\n{"type":"turn.started"}\n' "$tid"
  if [ -e "$rec/wait-steer" ]; then i=0; until [ -s "$rec/steers" ] || [ "$i" -ge 200 ]; do sleep 0.05; i=$((i+1)); done; fi
  f=$(find "$CODEX_HOME/sessions" -name "rollout-*-$tid.jsonl" | head -n 1)
  [ -n "$f" ] || { mkdir -p "$CODEX_HOME/sessions/2026/11/22"; f="$CODEX_HOME/sessions/2026/11/22/rollout-2026-11-22T09-00-00-$tid.jsonl"; }
  cat >> "$f" <<'EOF'
%[7]s
EOF
  if [ -e "$rec/block" ]; then trap 'echo exec >> "$rec/order"; exit 143' TERM; : > "$rec/working"; while :; do sleep 0.05; done; fi
  printf '%%s\n' '{"type":"item.completed","item":{"id":"item_1","type":"agent_message","text":"done"}}' '{"type":"turn.completed","usage":{"input_tokens":9999,"cached_input_tokens":0,"output_tokens":9999}}'
  exit 0;;
esac
exit 2
`, shQuote(record), release, codexStandInSocket, codexStandInSteers, shQuote(bin), codexStandInThread, rollout,
		codexStandInOrder)
	writeBoxStandIn(t, box, p.BinDir()+"/codex", script)
}

// codexBox is a real `direct` box with the Codex runner installed, and the
// directory its stand-in records to.
func codexBox(t *testing.T, release string) (*executor.Runner, sandbox.Sandbox, string) {
	t.Helper()
	local, err := sandbox.NewLocal(sandbox.LocalOptions{Placement: sandbox.Direct, StateDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	box, err := local.Create(t.Context(), sandbox.Spec{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { box.Close(t.Context()) })
	runner := executor.NewCodex()
	if err = runner.Install(t.Context(), box); err != nil {
		t.Fatalf("Install: %v", err)
	}
	record := t.TempDir()
	codexStandIn(t, box, release, record)
	return runner, box, record
}
