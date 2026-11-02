package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/api/opkey"
	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A CALL HELD FOR APPROVAL, END TO END (ADR-0077): an executor's run calls a
// tool at Needs approval on a real stdio MCP server over its bridge, the call is
// not made and the run parks on it; an administrator reads it with its
// arguments and allows it once; the run carries on in its own session told what
// the call returned; the server saw exactly one call; and a second answer is
// refused.

// The stand-in MCP server this binary becomes ([runHeldToolServer]).
const (
	// heldToolServerEnv makes a re-execution of this binary a stdio MCP
	// server listing one tool, [heldToolName], with no hints, so it starts
	// at Needs approval.
	heldToolServerEnv = "CREWLET_E2E_TOOL_SERVER"
	// heldToolCallsEnv names a file every tools/call the server answers is
	// appended to, one line each, so a case counts the calls made.
	heldToolCallsEnv = "CREWLET_E2E_TOOL_CALLS"
	// heldToolName is the server's one tool.
	heldToolName = "close_issue"
	// heldServer is the server's name in the company.
	heldServer = "tracker"
)

// What the run carrying a held call on is told, as internal/agent/runner
// writes it: in its own session, or as a fresh run.
const (
	heldAnswered = "## Your call was answered"
	heldReseeded = "called a tool that asks a person first"
)

// runHeldToolServer speaks newline-delimited JSON-RPC until stdin closes: one
// tool, whose calls it records and answers `closed issue <id>`.
func runHeldToolServer() int {
	in := bufio.NewReaderSize(os.Stdin, 1<<20)
	for {
		line, err := in.ReadString('\n')
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			answerHeldRPC(trimmed)
		}
		if err != nil {
			return 0
		}
	}
}

func answerHeldRPC(line string) {
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal([]byte(line), &req); err != nil || len(req.ID) == 0 {
		return
	}
	reply := func(result, rpcErr string) {
		body := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":%s}`, req.ID, result)
		if rpcErr != "" {
			body = fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"error":%s}`, req.ID, rpcErr)
		}
		fmt.Fprintln(os.Stdout, body)
	}
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		reply(fmt.Sprintf(`{"protocolVersion":%q,"capabilities":{"tools":{"listChanged":false}},`+
			`"serverInfo":{"name":"crewlet-e2e-tracker","version":"0.0.1"}}`, p.ProtocolVersion), "")
	case "ping":
		reply(`{}`, "")
	case "tools/list":
		reply(fmt.Sprintf(`{"tools":[{"name":%q,"description":"Close an issue.",`+
			`"inputSchema":{"type":"object","properties":{"id":{"type":"string"}}}}]}`, heldToolName), "")
	case "tools/call":
		var p struct {
			Arguments map[string]any `json:"arguments"`
		}
		_ = json.Unmarshal(req.Params, &p)
		if path := os.Getenv(heldToolCallsEnv); path != "" {
			raw, _ := json.Marshal(p.Arguments)
			if f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
				_, _ = f.Write(append(raw, '\n'))
				_ = f.Close()
			}
		}
		reply(fmt.Sprintf(`{"content":[{"type":"text","text":"closed issue %v"}]}`, p.Arguments["id"]), "")
	default:
		reply("", `{"code":-32601,"message":"method not found"}`)
	}
}

// codingCallHeld is a [codingModeApproval] run's calls: the tool at Needs
// approval over the server's own bridge endpoint, which answers it held; the
// submission the bridge refuses it after; and the Stop hook its harness runs as
// it ends its turn.
func codingCallHeld(ctx context.Context, args codingArgs, hooks codingHooks, stream codingStream) error {
	server, err := bridgedServerURL(args.mcpConfig, heldServer)
	if err != nil {
		return err
	}
	tracker, err := mcp.NewClient(&mcp.Implementation{Name: "claude", Version: "e2e"}, nil).
		Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server, HTTPClient: httpx.Client(0)}, nil)
	if err != nil {
		return fmt.Errorf("connecting to the bridge's %s endpoint at %s: %w", heldServer, server, err)
	}
	defer func() { _ = tracker.Close() }()
	stream.init([]string{heldToolName, runner.SubmitWorkTool})
	id := claudeID("toolu_")
	input := map[string]any{"id": "7"}
	stream.toolUse(id, heldToolName, input)
	out, failed := callOverBridge(ctx, tracker, id, heldToolName, input)
	stream.toolResult(id, out, failed)
	if !failed || !strings.Contains(out, "was NOT called") {
		return fmt.Errorf("a call to a tool at Needs approval answered %q (failed %v), want it held", out, failed)
	}
	bridge, err := bridgeURL(args.mcpConfig)
	if err != nil {
		return err
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "claude", Version: "e2e"}, nil).
		Connect(ctx, &mcp.StreamableClientTransport{Endpoint: bridge, HTTPClient: httpx.Client(0)}, nil)
	if err != nil {
		return fmt.Errorf("connecting to the bridge at %s: %w", bridge, err)
	}
	defer func() { _ = session.Close() }()
	id = claudeID("toolu_")
	submission := map[string]any{"outcome": "delivered", "summary": "Closed before the answer."}
	stream.toolUse(id, runner.SubmitWorkTool, submission)
	out, failed = callOverBridge(ctx, session, id, runner.SubmitWorkTool, submission)
	stream.toolResult(id, out, failed)
	if !failed || !strings.Contains(out, "parked on their answer") {
		return fmt.Errorf("a submission after the held call answered %q (failed %v), want it refused as parked",
			out, failed)
	}
	_, err = hooks.play(ctx, session, codingEventFinish, stream)
	return err
}

// bridgedServerURL is where the run's MCP config serves server over the bridge.
func bridgedServerURL(path, server string) (string, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // the run's own config, in its box
	if err != nil {
		return "", fmt.Errorf("reading the MCP config: %w", err)
	}
	var doc struct {
		Servers map[string]struct {
			URL string `json:"url"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", fmt.Errorf("reading the MCP config: %w", err)
	}
	s, ok := doc.Servers[server]
	if !ok || s.URL == "" {
		return "", fmt.Errorf("the MCP config serves no %q over the bridge: %s", server, raw)
	}
	return s.URL, nil
}

// startCodingWithHeldTool is [startCoding] in [codingModeApproval], the company
// running this binary's stdio MCP server, whose calls land in the file it
// answers.
func startCodingWithHeldTool(t *testing.T) (*codingNode, string) {
	t.Helper()
	stateDir := t.TempDir()
	argv := filepath.Join(stateDir, "argv.txt")
	calls := filepath.Join(stateDir, "calls.jsonl")
	env := map[string]string{codingModeEnv: codingModeApproval, codingArgvEnv: argv}
	model := newScriptedModel(t)
	server := fmt.Sprintf("mcp_servers:\n  - name: %s\n    command: %q\n    env:\n      %s: \"1\"\n      %s: %q\n",
		heldServer, scriptedHarnessBinary, heldToolServerEnv, heldToolCallsEnv, calls)
	doc := strings.Replace(sandboxCompany(t, model, stateDir), "roles:\n", server+"roles:\n", 1)
	n := bootCompany(t, doc, model, env)
	return &codingNode{node: n, stateDir: stateDir, argvPath: argv}, calls
}

// callsIn is every call the stand-in server recorded.
func callsIn(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // the case's own file
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("reading the server's calls: %v", err)
	}
	return strings.Fields(string(raw))
}

func TestAHeldCallIsMadeOnceAfterItsApprovalAndTheRunCarriesOn(t *testing.T) {
	t.Parallel()
	n, calls := startCodingWithHeldTool(t)
	waitFor(t, "the seat to be claimed", func() bool {
		return slices.Contains(n.engine.Node().Host().Held(), "swe")
	})
	n.wakeInConversation(t, "swe", "close issue 7, it is fixed", codingConversation)
	waitFor(t, "the run to park on its held call", func() bool {
		for _, run := range n.activeRuns(t) {
			if run.Status == sandbox.StatusAwaiting && run.HeldCall != nil {
				return true
			}
		}
		return false
	}, n.diagnose)
	parked := n.runFor(t, sandbox.StatusAwaiting)
	if got := callsIn(t, calls); len(got) != 0 {
		t.Fatalf("the server was called %v before anybody approved the call", got)
	}

	// AN ADMINISTRATOR READS IT WITH ITS ARGUMENTS.
	status, raw := bearer(t, n.node, http.MethodGet, "/mcp-servers", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /mcp-servers answered %d: %s", status, raw)
	}
	var answer struct {
		Approvals []struct {
			ID   string         `json:"id"`
			Tool string         `json:"tool"`
			Args map[string]any `json:"args"`
		} `json:"approvals"`
	}
	if err := json.Unmarshal(raw, &answer); err != nil {
		t.Fatal(err)
	}
	if len(answer.Approvals) != 1 || answer.Approvals[0].ID != parked.HeldCall.ID ||
		answer.Approvals[0].Tool != heldToolName || answer.Approvals[0].Args["id"] != "7" {
		t.Fatalf("approvals = %+v, want the held call with its arguments", answer.Approvals)
	}

	path := "/mcp-approvals/" + parked.HeldCall.ID
	status, raw = bearer(t, n.node, http.MethodPost, path, map[string]any{"answer": sandbox.HeldOnce},
		map[string]string{opkey.Header: uuid.Must(uuid.NewV7()).String()})
	if status != http.StatusAccepted {
		t.Fatalf("POST %s answered %d: %s", path, status, raw)
	}
	waitFor(t, "the approved run to settle", func() bool {
		return len(n.activeRuns(t)) == 0
	}, n.diagnose)
	if got := callsIn(t, calls); len(got) != 1 || !strings.Contains(got[0], `"7"`) {
		t.Fatalf("the server recorded %v, want exactly the one call approved", got)
	}
	if argv := n.agentArgv(t); !slices.Contains(argv, "--resume") {
		t.Fatalf("the approved run was not carried on in its own session: argv = %v", argv)
	}
	if brief := n.agentBrief(t); !strings.Contains(brief, heldAnswered) || !strings.Contains(brief, "closed issue 7") {
		t.Fatalf("the carried-on run was not told what the call returned: its prompt = %q", brief)
	}
	if outcome := n.settledOutcome(t, parked.TurnID); outcome != "delivered" {
		t.Fatalf("the approved turn settled with outcome %q, want the run's own delivered submission", outcome)
	}

	// A SECOND ANSWER IS REFUSED, and makes no second call.
	status, raw = bearer(t, n.node, http.MethodPost, path, map[string]any{"answer": sandbox.HeldOnce},
		map[string]string{opkey.Header: uuid.Must(uuid.NewV7()).String()})
	if status != http.StatusNotFound && status != http.StatusConflict {
		t.Fatalf("a second answer = %d %s, want it refused", status, raw)
	}
	if got := callsIn(t, calls); len(got) != 1 {
		t.Fatalf("the server recorded %d calls after a second answer, want one", len(got))
	}
}
