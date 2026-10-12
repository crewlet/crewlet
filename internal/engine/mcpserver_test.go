package engine_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
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

	// toolServerRequireEnv names an environment variable the server
	// REFUSES TO START without: empty, it says so on stderr and exits
	// non-zero before speaking MCP, which is what a vendor's server does
	// when the token it authenticates with is empty.
	toolServerRequireEnv = "CREWLET_ENGINE_TEST_REQUIRE"

	// toolServerHoldEnv is a path. A server about to refuse for want of its
	// required variable first writes <path>.started and then waits for
	// <path> to exist, so a case can land an apply while the start that is
	// going to fail is still in flight.
	toolServerHoldEnv = "CREWLET_ENGINE_TEST_HOLD"

	// toolServerReadyEnv is a path the server refuses to start without,
	// standing for a vendor that is not up yet: the failure passes when
	// the file appears, with nothing about the server's spec changing.
	toolServerReadyEnv = "CREWLET_ENGINE_TEST_READY"

	// toolServerLaunchesEnv is a directory every launch records itself in,
	// before anything else, so a case can count the starts the engine made.
	toolServerLaunchesEnv = "CREWLET_ENGINE_TEST_LAUNCHES"
)

func TestMain(m *testing.M) {
	if os.Getenv(toolServerModeEnv) != "" {
		os.Exit(runToolServer())
	}
	os.Exit(m.Run())
}

// runToolServer speaks newline-delimited JSON-RPC until stdin closes, or
// refuses to start the way the environment asks it to.
func runToolServer() int {
	if dir := os.Getenv(toolServerLaunchesEnv); dir != "" {
		if f, err := os.CreateTemp(dir, "launch-*"); err == nil {
			_ = f.Close()
		}
	}
	if key := os.Getenv(toolServerRequireEnv); key != "" && os.Getenv(key) == "" {
		if hold := os.Getenv(toolServerHoldEnv); hold != "" {
			holdUntil(hold)
		}
		fmt.Fprintf(os.Stderr, "%s is required and is empty\n", key)
		return 1
	}
	if ready := os.Getenv(toolServerReadyEnv); ready != "" {
		if _, err := os.Stat(ready); err != nil {
			fmt.Fprintln(os.Stderr, "the service this server fronts is not up")
			return 1
		}
	}
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

// holdUntil announces that this start is in flight and waits for the case to
// let it go. Bounded, so a case that never does cannot strand a child.
func holdUntil(path string) {
	if err := os.WriteFile(path+".started", nil, 0o600); err != nil {
		return
	}
	for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
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
