package engine_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

// A REAL MCP server, run as a REAL child process, for the ENGINE's wiring.
//
// internal/mcp already certifies the protocol and the subprocess supervision
// against its own helper. What is untested there — and was untested anywhere,
// because nothing joined the packages — is the engine's half: which servers
// get spawned, which seat's credentials go into which child, and whose tool
// surface the result lands in. That half cannot be checked with a fake bridge:
// the property under test is that TWO CHILDREN OF ONE TEMPLATE hold two
// different seats' identities, and a fake would assert that the fake was
// called with them.
//
// So the test binary re-executes itself as a minimal MCP server. It publishes
// one tool whose name comes from the environment, and — the load-bearing part
// — writes the value of a named environment variable into that tool's
// DESCRIPTION, so a test can read back what credential the child actually
// received rather than what the engine intended to send.

const (
	toolServerModeEnv = "CREWLET_ENGINE_TEST_MCP"
	// toolServerToolEnv names the single tool this server publishes.
	toolServerToolEnv = "CREWLET_ENGINE_TEST_TOOL"
	// toolServerEchoEnv names an environment variable whose value is
	// reported back in the tool's description.
	toolServerEchoEnv = "CREWLET_ENGINE_TEST_ECHO"
)

func TestMain(m *testing.M) {
	if os.Getenv(toolServerModeEnv) != "" {
		os.Exit(runToolServer())
	}
	os.Exit(m.Run())
}

// runToolServer speaks newline-delimited JSON-RPC until stdin closes.
func runToolServer() int {
	in := bufio.NewReaderSize(os.Stdin, 1<<20)
	for {
		line, err := in.ReadString('\n')
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			answerRPC(trimmed)
		}
		if err != nil {
			return 0
		}
	}
}

func answerRPC(line string) {
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal([]byte(line), &req); err != nil || len(req.ID) == 0 {
		// Unparseable, or a notification: either way there is nothing to
		// answer. A reply to a notification is a protocol violation.
		return
	}
	switch req.Method {
	case "server/discover":
		// Refused, so the client falls back to the legacy handshake — what
		// most servers in the wild still do.
		reply(req.ID, "", `{"code":-32601,"message":"unsupported"}`)
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		reply(req.ID, fmt.Sprintf(
			`{"protocolVersion":%q,"capabilities":{"tools":{"listChanged":false}},`+
				`"serverInfo":{"name":"crewlet-engine-test","version":"0.0.1"}}`,
			p.ProtocolVersion), "")
	case "ping":
		reply(req.ID, `{}`, "")
	case "tools/list":
		name := os.Getenv(toolServerToolEnv)
		if name == "" {
			name = "probe"
		}
		// THE ECHO. The description carries what this child's environment
		// actually holds, which is the only way a test can tell one seat's
		// child from another's from the outside.
		desc := "a test tool"
		if key := os.Getenv(toolServerEchoEnv); key != "" {
			desc = key + "=" + os.Getenv(key)
		}
		reply(req.ID, fmt.Sprintf(
			`{"tools":[{"name":%q,"description":%q,"inputSchema":{"type":"object"}}]}`,
			name, desc), "")
	case "tools/call":
		reply(req.ID, `{"content":[{"type":"text","text":"ok"}]}`, "")
	default:
		reply(req.ID, "", `{"code":-32601,"message":"method not found"}`)
	}
}

// reply writes one JSON-RPC response. Exactly one of result and rpcErr is set.
func reply(id json.RawMessage, result, rpcErr string) {
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":%s}`, id, result)
	if rpcErr != "" {
		body = fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"error":%s}`, id, rpcErr)
	}
	fmt.Fprintln(os.Stdout, body)
}
