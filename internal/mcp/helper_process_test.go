package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A REAL MCP server, run as a REAL child process.
//
// Subprocess supervision is the half of this package that cannot be tested
// against an in-process fake: environment inheritance, a stderr pipe, a
// process group, a child that ignores SIGTERM, a grandchild that outlives its
// parent. All of those are properties of an operating system, not of an
// interface, and a fake transport would assert that the mock was called.
//
// So the test binary re-executes ITSELF in helper mode. The server speaks
// newline-delimited JSON-RPC by hand rather than through the SDK's server,
// which is deliberate: these tests exist to pin what the CLIENT does with wire
// shapes a real third-party server produces and an SDK-built server cannot —
// an out-of-spec empty cursor, a repeated cursor, an annotations object that
// omits a hint, a content block from a future spec version.

const (
	helperModeEnv = "CREWLET_MCP_TEST_HELPER"

	// helperTools is a JSON array of {name, description, annotations} where
	// annotations is spliced in RAW, so a test can send an object with any
	// subset of the hint keys — including none of them.
	helperToolsEnv = "CREWLET_MCP_TEST_TOOLS"

	// helperPagesEnv selects the pagination shape: "", "two", "empty-cursor",
	// "repeat", "endless", "hang".
	helperPagesEnv = "CREWLET_MCP_TEST_PAGES"

	// helperCallEnv selects tools/call behaviour: "", "error", "hang",
	// "empty", "blocks".
	helperCallEnv = "CREWLET_MCP_TEST_CALL"

	// helperStderrEnv is a \n-separated script written to stderr at startup.
	helperStderrEnv = "CREWLET_MCP_TEST_STDERR"

	// helperEchoEnv names environment variables the helper prints to stderr
	// as KEY=VALUE, so a test can see the child's actual environment.
	helperEchoEnv = "CREWLET_MCP_TEST_ECHO_ENV"

	// helperExitEnv makes the helper exit with this code after writing its
	// stderr script, without ever speaking MCP.
	helperExitEnv = "CREWLET_MCP_TEST_EXIT"
)

func TestMain(m *testing.M) {
	if mode := os.Getenv(helperModeEnv); mode != "" {
		os.Exit(runHelper(mode))
	}
	os.Exit(m.Run())
}

// helperSpec returns a Spec that launches this test binary in helper mode.
func helperSpec(t *testing.T, name, mode string, env map[string]string) Spec {
	t.Helper()
	full := map[string]string{helperModeEnv: mode}
	for k, v := range env {
		full[k] = v
	}
	return Spec{
		Name:           name,
		Transport:      TransportStdio,
		Command:        os.Args[0],
		Env:            full,
		StartupTimeout: 20 * time.Second,
		RequestTimeout: 20 * time.Second,
	}
}

// toolsJSON builds the helperToolsEnv payload. annotations is raw JSON or ""
// for a tool with no annotations key at all.
func toolsJSON(tools ...[3]string) string {
	parts := make([]string, len(tools))
	for i, t := range tools {
		name, desc, ann := t[0], t[1], t[2]
		if ann == "" {
			ann = "null"
		}
		parts[i] = fmt.Sprintf(`{"name":%q,"description":%q,"annotations":%s}`, name, desc, ann)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// --- the helper process ----------------------------------------------------

func runHelper(mode string) int {
	if script := os.Getenv(helperStderrEnv); script != "" {
		for _, line := range strings.Split(script, "\n") {
			fmt.Fprintln(os.Stderr, line)
		}
	}
	if names := os.Getenv(helperEchoEnv); names != "" {
		for _, k := range strings.Split(names, ",") {
			fmt.Fprintf(os.Stderr, "ENV %s=%s\n", k, os.Getenv(k))
		}
	}
	if code := os.Getenv(helperExitEnv); code != "" {
		n, _ := strconv.Atoi(code)
		return n
	}

	switch mode {
	case "mute":
		// Connected, holding both pipes open, saying nothing for ever. This
		// is the failure the startup deadline exists for: nothing raises.
		select {}
	case "ignore-term":
		// A server that ignores the polite signal but still exits when its
		// stdin closes: the ordinary well-behaved case, kept as a control.
		signal.Ignore(syscall.SIGTERM)
		serve()
	case "stubborn":
		// The expensive case: it neither exits on stdin EOF nor honours
		// SIGTERM, so the transport walks its whole shutdown ladder and only
		// SIGKILL ends it. This is what makes a SEQUENTIAL shutdown strand
		// every server behind the first one.
		signal.Ignore(syscall.SIGTERM)
		serve()
		select {}
	case "grandchild":
		// Announce the pid on the INHERITED stderr, then hold it open for
		// ever. This is the descendant that keeps the pipe from reaching EOF
		// after its parent is gone.
		fmt.Fprintf(os.Stderr, "GRANDCHILD %d\n", os.Getpid())
		select {}
	case "spawn-grandchild":
		// A stand-in for `uvx`/`npx`: fork something that inherits stderr and
		// outlives us, then serve normally.
		grandchild := exec.Command(os.Args[0])
		grandchild.Env = append(os.Environ(),
			helperModeEnv+"=grandchild",
			helperStderrEnv+"=",
			helperEchoEnv+"=",
			helperExitEnv+"=")
		grandchild.Stderr = os.Stderr
		if err := grandchild.Start(); err != nil {
			fmt.Fprintln(os.Stderr, "grandchild failed:", err)
		}
		serve()
	default:
		serve()
	}
	return 0
}

type rpcRequest struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// serve is a newline-delimited JSON-RPC MCP server, hand-rolled so a test can
// put any wire shape on the pipe.
func serve() {
	in := bufio.NewReaderSize(os.Stdin, 1<<20)
	out := os.Stdout
	page := 0
	for {
		line, err := in.ReadString('\n')
		if line != "" {
			handleRPC(out, strings.TrimSpace(line), &page)
		}
		if err != nil {
			return
		}
	}
}

func handleRPC(out *os.File, line string, page *int) {
	if line == "" {
		return
	}
	var req rpcRequest
	if err := json.Unmarshal([]byte(line), &req); err != nil {
		return
	}
	if len(req.ID) == 0 {
		return // a notification: no reply
	}
	switch req.Method {
	case "server/discover":
		// Refuse the modern stateless probe so the client falls back to the
		// legacy initialize handshake, which is what most servers in the wild
		// still do.
		writeError(out, req.ID, -32601, "server/discover not supported")
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &params)
		// Echo the client's version: whatever it asked for is by definition
		// one it supports.
		writeResult(out, req.ID, fmt.Sprintf(
			`{"protocolVersion":%q,"capabilities":{"tools":{"listChanged":false}},`+
				`"serverInfo":{"name":"crewlet-test-helper","version":"0.0.1"}}`,
			params.ProtocolVersion))
	case "ping":
		writeResult(out, req.ID, `{}`)
	case "tools/list":
		result := listToolsResult(req.Params, page)
		if result == "" {
			return // answer nothing, for ever
		}
		writeResult(out, req.ID, result)
	case "tools/call":
		callResult(out, req.ID, req.Params)
	default:
		writeError(out, req.ID, -32601, "method not found: "+req.Method)
	}
}

func listToolsResult(params json.RawMessage, page *int) string {
	tools := os.Getenv(helperToolsEnv)
	if tools == "" {
		tools = toolsJSON([3]string{"search", "Search for items", ""})
	}
	var p struct {
		Cursor string `json:"cursor"`
	}
	_ = json.Unmarshal(params, &p)
	*page++

	switch os.Getenv(helperPagesEnv) {
	case "two":
		if p.Cursor == "" {
			return fmt.Sprintf(`{"tools":%s,"nextCursor":"page-2"}`,
				toolsJSON([3]string{"alpha", "First", ""}))
		}
		return fmt.Sprintf(`{"tools":%s}`, toolsJSON([3]string{"beta", "Second", ""}))
	case "empty-cursor":
		// Out of spec (nextCursor should be omitted) but unambiguous: some
		// serializers emit "" for "no more pages".
		return fmt.Sprintf(`{"tools":%s,"nextCursor":""}`, tools)
	case "hang":
		// Connected, then silent on discovery. The same outage as a server
		// that never connects, arriving after connect has returned.
		return ""
	case "repeat":
		return fmt.Sprintf(`{"tools":%s,"nextCursor":"SAME"}`, tools)
	case "endless":
		return fmt.Sprintf(`{"tools":%s,"nextCursor":"page-%d"}`,
			toolsJSON([3]string{fmt.Sprintf("t%d", *page), "generated", ""}), *page)
	default:
		return fmt.Sprintf(`{"tools":%s}`, tools)
	}
}

func callResult(out *os.File, id json.RawMessage, params json.RawMessage) {
	var p struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	_ = json.Unmarshal(params, &p)

	switch os.Getenv(helperCallEnv) {
	case "hang":
		// Answer nothing, for ever. The per-call deadline is the only thing
		// between this and a wedged phase.
		return
	case "error":
		writeResult(out, id,
			`{"content":[{"type":"text","text":"permission denied"}],"isError":true}`)
	case "empty":
		writeResult(out, id, `{"content":[]}`)
	case "blocks":
		writeResult(out, id, `{"content":[`+
			`{"type":"text","text":"lead"},`+
			`{"type":"resource","resource":{"uri":"file:///app/main.py",`+
			`"mimeType":"text/x-python","text":"print('hello')\n"}},`+
			`{"type":"resource","resource":{"uri":"file:///app/logo.png",`+
			`"mimeType":"image/png","blob":"QUJD"}},`+
			`{"type":"resource_link","uri":"https://example/file.txt",`+
			`"name":"file.txt","mimeType":"text/plain"},`+
			`{"type":"image","mimeType":"image/png","data":"YWJj"},`+
			`{"type":"audio","mimeType":"audio/wav","data":"YWJj"}]}`)
	default:
		// A real server refuses a tool it does not have, and it refuses it as
		// an MCP tool error rather than a protocol error — so the model can
		// see it and self-correct.
		if !strings.Contains(os.Getenv(helperToolsEnv)+`"search"`, `"`+p.Name+`"`) {
			writeResult(out, id, fmt.Sprintf(
				`{"content":[{"type":"text","text":%q}],"isError":true}`,
				"no such tool: "+p.Name))
			return
		}
		args, _ := json.Marshal(p.Arguments)
		writeResult(out, id, fmt.Sprintf(
			`{"content":[{"type":"text","text":%q}]}`,
			fmt.Sprintf("Result of %s %s", p.Name, args)))
	}
}

func writeResult(out *os.File, id json.RawMessage, result string) {
	fmt.Fprintf(out, `{"jsonrpc":"2.0","id":%s,"result":%s}`+"\n", id, result)
}

func writeError(out *os.File, id json.RawMessage, code int, message string) {
	fmt.Fprintf(out, `{"jsonrpc":"2.0","id":%s,"error":{"code":%d,"message":%q}}`+"\n",
		id, code, message)
}
