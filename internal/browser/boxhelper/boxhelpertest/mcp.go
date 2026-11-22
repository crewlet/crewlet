package boxhelpertest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/browser/boxhelper"
)

// callWithin bounds one wait on an MCP server: past every bound the pinned
// server keeps (a navigation's minute and its load's five seconds), so past
// it the server hangs, never a slow machine.
const callWithin = 2 * time.Minute

// mcpLine is the longest line an MCP client reads, past the 32 MiB the shim
// and the supervisor pass each other.
const mcpLine = 40 << 20

// Message is one JSON-RPC message, as a case reads it, and its line.
type Message struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *RPCError       `json:"error,omitempty"`
	// Line is the message as it came, its newline aside.
	Line []byte `json:"-"`
}

// RPCError is a JSON-RPC error answer.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Content is one item of a tools/call result.
type Content struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
}

// Result is a tools/call's answer: its result, or the JSON-RPC error the
// server answered instead.
type Result struct {
	Content           []Content       `json:"content"`
	IsError           bool            `json:"isError"`
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
	Error             *RPCError       `json:"-"`
}

// Text is every text item of the result, joined by newlines.
func (r Result) Text() string {
	var texts []string
	for _, c := range r.Content {
		if c.Type == "text" {
			texts = append(texts, c.Text)
		}
	}
	return strings.Join(texts, "\n")
}

// Tool is one tool a tools/list result names.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// MCP is an MCP client over two pipes, as a harness is one, with no MCP SDK
// on either end: it writes JSON-RPC lines to the server and reads its lines
// back, answering a ping itself and any other request of the server's
// -32601, and failing the case on a line the server writes that is not a
// JSON-RPC message. A case reads every line each way ([MCP.Lines]).
type MCP struct {
	t        testing.TB
	toServer io.WriteCloser

	mu       sync.Mutex
	next     int
	waiting  map[string]chan Message
	sent     [][]byte
	received []Message

	// closed is closed once the server side's output ends.
	closed chan struct{}
	shim   *shimProcess
}

// MCPOver is an MCP client of the server whose input is toServer and whose
// output is fromServer: a shim the case started itself, or one another
// command started, in a container or a box of another cell.
func MCPOver(t testing.TB, toServer io.WriteCloser, fromServer io.Reader) *MCP {
	t.Helper()
	m := &MCP{t: t, toServer: toServer, waiting: map[string]chan Message{}, closed: make(chan struct{})}
	go m.read(fromServer)
	return m
}

// read reads the server's lines until its output ends, handing each answer
// to the request waiting on it.
func (m *MCP) read(fromServer io.Reader) {
	defer close(m.closed)
	lines := bufio.NewScanner(fromServer)
	lines.Buffer(make([]byte, 0, 64<<10), mcpLine)
	for lines.Scan() {
		line := bytes.Clone(lines.Bytes())
		var msg Message
		if err := json.Unmarshal(line, &msg); err != nil || !jsonrpc(line) {
			m.t.Errorf("the MCP server wrote %q, which is no JSON-RPC message (%v)", clip(line), err)
			continue
		}
		msg.Line = line
		m.mu.Lock()
		m.received = append(m.received, msg)
		var answer chan Message
		if msg.Method == "" {
			answer = m.waiting[string(msg.ID)]
			delete(m.waiting, string(msg.ID))
		}
		m.mu.Unlock()
		switch {
		case msg.Method == "ping" && msg.ID != nil:
			m.write(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": map[string]any{}})
		case msg.Method != "" && msg.ID != nil:
			m.write(map[string]any{"jsonrpc": "2.0", "id": msg.ID,
				"error": map[string]any{"code": -32601, "message": "Method not found"}})
		case msg.Method == "" && answer != nil:
			answer <- msg
		}
	}
	if err := lines.Err(); err != nil {
		m.t.Errorf("reading the MCP server: %v", err)
	}
}

// jsonrpc is whether line is one JSON-RPC 2.0 object.
func jsonrpc(line []byte) bool {
	var head struct {
		Version string `json:"jsonrpc"`
	}
	return bytes.HasPrefix(bytes.TrimSpace(line), []byte("{")) && json.Unmarshal(line, &head) == nil &&
		head.Version == "2.0"
}

// clip is line as a failure quotes it.
func clip(line []byte) []byte {
	if len(line) > 512 {
		return append(bytes.Clone(line[:512]), "..."...)
	}
	return line
}

// Initialize starts the session as a harness does, declaring caps, and sends
// notifications/initialized after the server's answer, which it answers.
func (m *MCP) Initialize(version string, caps map[string]any) Message {
	m.t.Helper()
	answer := m.Request("initialize", map[string]any{"protocolVersion": version, "capabilities": caps,
		"clientInfo": map[string]any{"name": "boxhelper-suite", "version": "1"}})
	if answer.Error != nil {
		m.t.Fatalf("initialize was refused: %+v", answer.Error)
	}
	m.Notify("notifications/initialized", nil)
	return answer
}

// ListTools is the server's tools/list.
func (m *MCP) ListTools() []Tool {
	m.t.Helper()
	answer := m.Request("tools/list", map[string]any{})
	var listed struct{ Tools []Tool }
	if answer.Error != nil || json.Unmarshal(answer.Result, &listed) != nil {
		m.t.Fatalf("tools/list was answered %s", answer.Line)
	}
	return listed.Tools
}

// Call calls the tool name with args and answers what the server answered.
func (m *MCP) Call(name string, args map[string]any) Result {
	m.t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	answer := m.Request("tools/call", map[string]any{"name": name, "arguments": args})
	if answer.Error != nil {
		return Result{Error: answer.Error}
	}
	var r Result
	if err := json.Unmarshal(answer.Result, &r); err != nil {
		m.t.Fatalf("%s was answered %s: %v", name, clip(answer.Line), err)
	}
	return r
}

// Request sends a request of method with params and answers the server's
// answer to it, failing the case on none within callWithin.
func (m *MCP) Request(method string, params any) Message {
	m.t.Helper()
	m.mu.Lock()
	m.next++
	n := m.next
	answer := make(chan Message, 1)
	m.waiting[strconv.Itoa(n)] = answer
	m.mu.Unlock()
	m.write(map[string]any{"jsonrpc": "2.0", "id": n, "method": method, "params": params})
	ctx, cancel := context.WithTimeout(m.t.Context(), callWithin)
	defer cancel()
	select {
	case msg := <-answer:
		return msg
	case <-m.closed:
		m.t.Fatalf("the MCP server's output ended before it answered %s%s", method, m.shimSaid())
	case <-ctx.Done():
		m.t.Fatalf("the MCP server did not answer %s within %s%s", method, callWithin, m.shimSaid())
	}
	return Message{}
}

// Notify sends a notification of method with params, none where nil.
func (m *MCP) Notify(method string, params any) {
	m.t.Helper()
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		msg["params"] = params
	}
	m.write(msg)
}

// Send writes line to the server as it is, its newline added.
func (m *MCP) Send(line []byte) {
	m.t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, bytes.Clone(line))
	if _, err := m.toServer.Write(append(bytes.Clone(line), '\n')); err != nil {
		m.t.Errorf("writing to the MCP server: %v", err)
	}
}

func (m *MCP) write(msg any) {
	line, err := json.Marshal(msg)
	if err != nil {
		m.t.Errorf("encoding %v: %v", msg, err)
		return
	}
	m.Send(line)
}

// Lines is every line the client sent and every message the server sent, in
// the order each was sent.
func (m *MCP) Lines() (sent [][]byte, received []Message) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([][]byte(nil), m.sent...), append([]Message(nil), m.received...)
}

// Close ends the client's side: the server's input closes, if a reaped shim
// has not closed it already.
func (m *MCP) Close() {
	if err := m.toServer.Close(); err != nil && !errors.Is(err, os.ErrClosed) && !errors.Is(err, io.ErrClosedPipe) {
		m.t.Errorf("closing the MCP server's input: %v", err)
	}
}

// Closed is closed once the server's output ends.
func (m *MCP) Closed() <-chan struct{} { return m.closed }

// AwaitClosed waits for the server's output to end, failing the case on an
// output still open after callWithin.
func (m *MCP) AwaitClosed() {
	m.t.Helper()
	ctx, cancel := context.WithTimeout(m.t.Context(), callWithin)
	defer cancel()
	select {
	case <-m.closed:
	case <-ctx.Done():
		m.t.Fatalf("the MCP server's output is still open after %s", callWithin)
	}
}

// Exited is how the shim [Box.MCP] started ended: its exit status and the end
// of its stderr, once its output has ended.
func (m *MCP) Exited() (code int, stderr string) {
	m.t.Helper()
	if m.shim == nil {
		m.t.Fatal("this client runs over pipes, not a shim Box.MCP started")
	}
	m.AwaitClosed()
	return m.shim.exit(m.t)
}

// shimSaid is the end of the shim's stderr, for a failure, where it runs one.
func (m *MCP) shimSaid() string {
	if m.shim == nil {
		return ""
	}
	return "; the shim's stderr:\n" + m.shim.stderr.String()
}

// Shim is the shim a case started as a harness starts it, its stdio in the
// case's hands.
type Shim struct {
	Stdin  io.WriteCloser
	Stdout io.Reader
	*shimProcess
}

// shimProcess is a shim's process: its stderr's end, and its exit once
// reaped.
type shimProcess struct {
	cmd    *exec.Cmd
	stderr *tail
	reaped sync.Once
	code   int
	err    error
}

// Shim starts the shim for job as a harness starts it, from
// boxhelper.ShimArgs, the box's workspace its root and working directory,
// under the box's node and environment. The case's end ends it.
func (b *Box) Shim(t testing.TB, job string) *Shim {
	t.Helper()
	command, args := boxhelper.ShimArgs(b.Home, job, b.Workspace)
	cmd := exec.CommandContext(t.Context(), filepath.Join(b.Home, "bin", command), args...)
	cmd.Env = b.Environ()
	cmd.Dir = b.Workspace
	cmd.WaitDelay = callWithin
	p := &shimProcess{cmd: cmd, stderr: &tail{}}
	cmd.Stderr = p.stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the shim: %v", err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		p.exit(t)
	})
	return &Shim{Stdin: stdin, Stdout: stdout, shimProcess: p}
}

// Exited is how the shim ended, once the case has read its stdout to its end:
// its exit status and the end of its stderr.
func (s *Shim) Exited(t testing.TB) (code int, stderr string) {
	t.Helper()
	return s.exit(t)
}

// killedAtTheEnd is the status of a shim the case's end killed, its context
// done before it exited.
const killedAtTheEnd = -1

// exit reaps the shim, once, and answers its status and stderr's end.
func (p *shimProcess) exit(t testing.TB) (int, string) {
	t.Helper()
	p.reaped.Do(func() {
		done := make(chan error, 1)
		go func() { done <- p.cmd.Wait() }()
		ctx, cancel := context.WithTimeout(context.Background(), callWithin)
		defer cancel()
		select {
		case err := <-done:
			var exit *exec.ExitError
			switch {
			case err == nil:
			case errors.As(err, &exit):
				p.code = exit.ExitCode()
			case errors.Is(err, context.Canceled):
				p.code = killedAtTheEnd
			default:
				p.err = err
			}
		case <-ctx.Done():
			p.err = fmt.Errorf("the shim, pid %d, is still running %s on", p.cmd.Process.Pid, callWithin)
		}
	})
	if p.err != nil {
		t.Errorf("reaping the shim: %v", p.err)
	}
	return p.code, p.stderr.String()
}

// MCP starts the shim for job as [Box.Shim] does and answers an MCP client of
// it, over its stdio.
func (b *Box) MCP(t testing.TB, job string) *MCP {
	t.Helper()
	s := b.Shim(t, job)
	m := MCPOver(t, s.Stdin, s.Stdout)
	m.shim = s.shimProcess
	// Before the shim is reaped, which closes its stdout: every read of it is
	// done once its output has ended.
	t.Cleanup(func() {
		m.Close()
		<-m.closed
	})
	return m
}
