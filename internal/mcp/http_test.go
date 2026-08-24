package mcp

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// httpMCPServer is a minimal Streamable-HTTP MCP endpoint.
//
// Hand-rolled for the same reason the stdio helper is: these tests pin what
// the CLIENT sends and how it reads a reply, including wire shapes an
// SDK-built server cannot produce.
type httpMCPServer struct {
	mu       sync.Mutex
	requests []recordedRequest
	tools    string
	fail     int // when non-zero, answer every POST with this status
	failBody string
}

type recordedRequest struct {
	Method   string
	Headers  http.Header
	RPCMethd string
}

func newHTTPMCPServer(t *testing.T, tools string) (*httptest.Server, *httpMCPServer) {
	t.Helper()
	h := &httpMCPServer{tools: tools}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, h
}

func (h *httpMCPServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req rpcRequest
	_ = json.Unmarshal(body, &req)

	h.mu.Lock()
	h.requests = append(h.requests, recordedRequest{
		Method: r.Method, Headers: r.Header.Clone(), RPCMethd: req.Method,
	})
	failStatus, failBody := h.fail, h.failBody
	tools := h.tools
	h.mu.Unlock()

	if r.Method != http.MethodPost {
		// The standalone SSE stream is optional; refusing it is spec-compliant.
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	// Fail only the CALLS. Failing the handshake or the initialized
	// notification would make the connect fail, and these tests are about
	// what happens to a live session's requests.
	if failStatus != 0 && (req.Method == "tools/list" || req.Method == "tools/call") {
		w.WriteHeader(failStatus)
		_, _ = io.WriteString(w, failBody)
		return
	}
	if len(req.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	var result string
	switch req.Method {
	case "server/discover":
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":`+string(req.ID)+
			`,"error":{"code":-32601,"message":"no discover"}}`)
		return
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		result = `{"protocolVersion":"` + p.ProtocolVersion +
			`","capabilities":{"tools":{"listChanged":false}},` +
			`"serverInfo":{"name":"http-helper","version":"0.0.1"}}`
	case "tools/list":
		result = `{"tools":` + tools + `}`
	case "tools/call":
		result = `{"content":[{"type":"text","text":"remote ok"}]}`
	default:
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":`+string(req.ID)+
			`,"error":{"code":-32601,"message":"unknown"}}`)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Mcp-Session-Id", "session-1")
	_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":`+string(req.ID)+`,"result":`+result+`}`)
}

func (h *httpMCPServer) seen() []recordedRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]recordedRequest(nil), h.requests...)
}

func httpSpec(name, url string) Spec {
	return Spec{
		Name: name, Transport: TransportHTTP, URL: url,
		StartupTimeout: 20 * time.Second, RequestTimeout: 20 * time.Second,
	}
}

func TestHTTPServerRoundTrip(t *testing.T) {
	t.Parallel()
	srv, h := newHTTPMCPServer(t, toolsJSON(
		[3]string{"get_me", "Who am I", `{"readOnlyHint":true}`},
		[3]string{"create_pr", "Open a PR", ""},
	))
	spec := httpSpec(InstanceName("github", "Engineer"), srv.URL)
	// A per-role HTTP instance carries the seat's identity in a HEADER, where
	// a stdio child would get an environment variable. This is how a remote
	// server sees one agent rather than the company.
	spec.Headers = map[string]string{"Authorization": "Bearer eng-token"}

	c, err := connect(t.Context(), spec, discardLogger())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = c.stop(t.Context()) })

	defs, err := c.listTools(t.Context())
	if err != nil {
		t.Fatalf("listTools: %v", err)
	}
	if len(defs) != 2 {
		t.Fatalf("tools = %v", names(defs))
	}
	// The wire probe works over HTTP too: silent stays unknown, asserted
	// stays asserted.
	byName := map[string]Annotations{}
	for _, d := range defs {
		byName[d.Name] = d.Annotations
	}
	if byName["get_me"].ReadOnly != Yes {
		t.Fatalf("get_me annotations = %+v", byName["get_me"])
	}
	if byName["create_pr"] != (Annotations{}) {
		t.Fatalf("create_pr annotations = %+v, want all-unknown", byName["create_pr"])
	}

	blocks, err := c.callTool(t.Context(), "get_me", nil)
	if err != nil {
		t.Fatalf("callTool: %v", err)
	}
	if renderBlocks(blocks) != "remote ok" {
		t.Fatalf("output = %q", renderBlocks(blocks))
	}

	var sawAuth, sawVersionAfterInit bool
	for _, req := range h.seen() {
		if req.Headers.Get("Authorization") == "Bearer eng-token" {
			sawAuth = true
		}
		if req.RPCMethd == "tools/list" && req.Headers.Get("Mcp-Protocol-Version") != "" {
			sawVersionAfterInit = true
		}
	}
	if !sawAuth {
		t.Fatal("the configured Authorization header never reached the server")
	}
	// The probe's wrapper costs the SDK's unexported sessionUpdated hook,
	// which is what normally fills this in. If it stops being restored, a
	// remote server that enforces the header answers 400 for a reason nothing
	// in the engine could explain.
	if !sawVersionAfterInit {
		t.Fatal("Mcp-Protocol-Version was not sent after the handshake")
	}
}

func TestHTTPErrorBodyIsLoggedBeforeItIsLost(t *testing.T) {
	t.Parallel()
	srv, h := newHTTPMCPServer(t, toolsJSON([3]string{"get_me", "d", ""}))
	h.mu.Lock()
	h.fail, h.failBody = http.StatusForbidden, `{"error":"token lacks the repo scope"}`
	h.mu.Unlock()

	log, rec := recorder()
	spec := httpSpec("remote", srv.URL)
	spec.StartupTimeout = 5 * time.Second
	c, err := connect(t.Context(), spec, log)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = c.stop(t.Context()) })

	if _, err := c.listTools(t.Context()); err == nil {
		t.Fatal("a 403 on tools/list must fail discovery")
	}
	// The SDK surfaces an HTTP failure only after its task group unwinds, by
	// which point the body is gone — and the body is the only place the
	// remote server says why.
	errs := rec.find("http_error")
	if len(errs) == 0 {
		t.Fatal("no http_error logged: the operator gets a status code and no reason")
	}
	body, _ := errs[0].Attrs["response_body"].(string)
	if !strings.Contains(body, "repo scope") {
		t.Fatalf("logged body %q lost the server's reason", body)
	}
	// slog renders an int attribute as an int64; comparing against an
	// untyped 403 silently never matches.
	if got, _ := errs[0].Attrs["status_code"].(int64); got != 403 {
		t.Fatalf("status = %v (%T)", errs[0].Attrs["status_code"], errs[0].Attrs["status_code"])
	}
}

func TestHTTPRequestBodyIsNotLogged(t *testing.T) {
	t.Parallel()
	// A JSON-RPC request body is tool ARGUMENTS an agent composed, which can
	// carry a credential it was handed to pass along. There is no redaction
	// pass on this side of the engine, and a log line is a permanent place to
	// put a secret. The Python client logged it; this deliberately does not.
	srv, h := newHTTPMCPServer(t, toolsJSON([3]string{"leaky", "d", ""}))
	h.mu.Lock()
	h.fail, h.failBody = http.StatusInternalServerError, "boom"
	h.mu.Unlock()

	log, rec := recorder()
	spec := httpSpec("remote", srv.URL)
	spec.StartupTimeout = 5 * time.Second
	c, err := connect(t.Context(), spec, log)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = c.stop(t.Context()) })
	_, _ = c.callTool(t.Context(), "leaky", map[string]any{"token": "sk-super-secret"})

	for _, r := range rec.records {
		for k, v := range r.Attrs {
			if s, ok := v.(string); ok && strings.Contains(s, "sk-super-secret") {
				t.Fatalf("a tool argument reached the logs under %q: %q", k, s)
			}
		}
	}
	if !rec.has("http_error") {
		t.Fatal("the 500 was not reported at all")
	}
}

func TestHTTPDoesNotOpenTheStandaloneStream(t *testing.T) {
	t.Parallel()
	// The standalone GET stream delivers server-initiated notifications the
	// engine registers no handler for, and it is the other thing the probe's
	// wrapper costs. Disabled explicitly, so the connection does what it says.
	srv, h := newHTTPMCPServer(t, toolsJSON([3]string{"get_me", "d", ""}))
	c, err := connect(t.Context(), httpSpec("remote", srv.URL), discardLogger())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = c.stop(t.Context()) })
	if _, err := c.listTools(t.Context()); err != nil {
		t.Fatalf("listTools: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	for _, req := range h.seen() {
		if req.Method == http.MethodGet {
			t.Fatal("a standalone SSE stream was opened despite DisableStandaloneSSE")
		}
	}
}

func TestHTTPConnectDeadline(t *testing.T) {
	t.Parallel()
	// A remote endpoint that accepts the connection and never answers is the
	// HTTP form of the mute stdio server, and it needs the same ceiling: the
	// transport's own read timeout resets on every byte.
	// The handler must be releasable, or httptest's Close waits for every
	// hung connection and the whole test binary times out. That is a property
	// of the instrument, not of the code under test.
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() {
		close(release)
		slow.Close()
	})

	spec := httpSpec("slow", slow.URL)
	spec.StartupTimeout = 300 * time.Millisecond
	start := time.Now()
	_, err := connect(t.Context(), spec, discardLogger())
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("a remote endpoint that never answers must fail, not hang")
	}
	if !strings.Contains(err.Error(), "did not connect within") {
		t.Fatalf("error %q does not name the startup deadline", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("connect took %s", elapsed)
	}
}

func TestHTTPServerNeedsNoChildSupervision(t *testing.T) {
	t.Parallel()
	srv, _ := newHTTPMCPServer(t, toolsJSON([3]string{"get_me", "d", ""}))
	log, rec := recorder()
	c, err := connect(t.Context(), httpSpec("remote", srv.URL), log)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if c.stderrTail() != nil {
		t.Fatal("an HTTP server has no stderr to tail")
	}
	if err := c.stop(t.Context()); err != nil {
		t.Fatalf("stop: %v", err)
	}
	// There is no process group and no pipe, so the reap must be a clean
	// no-op rather than a signal aimed at pid 0.
	if rec.has("server_tree_reaped") || rec.has("server_group_kill_failed") {
		t.Fatal("the HTTP path went looking for a child process")
	}
	if _, err := c.callTool(t.Context(), "get_me", nil); err == nil {
		t.Fatal("calls succeeded after stop")
	}
}
