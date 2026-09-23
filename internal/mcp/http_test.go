package mcp

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/httpx"
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
	// put a secret. This deliberately does not log it.
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

	for _, r := range rec.all() {
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
	if tail, dropped := c.stderrTail(); tail != nil || dropped != 0 {
		t.Fatalf("an HTTP server has no stderr to tail: %v, %d dropped", tail, dropped)
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

// AN OVERSIZED ERROR BODY IS NOT BUFFERED WHOLE, and is not truncated either.
//
// The body must be read to be logged and replayed, and handing the SDK a
// truncated one turns a server's clear JSON-RPC error into the bare status
// text, because the SDK reports the body only when it decodes. But
// "whole" was unbounded, so a remote server chose this process's allocation
// size. Past the cap the prefix is handed back in front of the still-open
// body, so the SDK reads every byte and nothing further is held.
func TestAnOversizedErrorBodyIsStreamedRatherThanBuffered(t *testing.T) {
	t.Parallel()
	const size = (1 << 20) + 4096 // comfortably over maxBufferedErrorBody
	payload := strings.Repeat("z", size)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, payload)
	}))
	t.Cleanup(srv.Close)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	// THE SERVER'S OWN POOL. This request goes straight to srv, so no
	// rewrite is needed — but the base must still not be the
	// process-global transport that every neighbour's t.Cleanup sweeps;
	// see [github.com/crewlet/crewlet/internal/httpx/httpxtest].
	rt := &httpIdentity{base: srv.Client().Transport, log: discardLogger()}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// THE SDK STILL SEES EVERY BYTE. A cap that silently truncated would
	// pass a length check and break the diagnostic this path exists for.
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read replayed body: %v", err)
	}
	if len(got) != size {
		t.Errorf("replayed %d bytes, want the whole %d", len(got), size)
	}
	if string(got) != payload {
		t.Error("the replayed body is not the one the server sent")
	}
}

// AND A BODY WITHIN THE CAP IS REPLAYED EXACTLY, so the bound changes nothing
// for the ordinary case it was added to protect.
func TestAnOrdinaryErrorBodyIsReplayedWhole(t *testing.T) {
	t.Parallel()
	const payload = `{"error":"insufficient_scope","detail":"needs repo:write"}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, payload)
	}))
	t.Cleanup(srv.Close)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	// THE SERVER'S OWN POOL. This request goes straight to srv, so no
	// rewrite is needed — but the base must still not be the
	// process-global transport that every neighbour's t.Cleanup sweeps;
	// see [github.com/crewlet/crewlet/internal/httpx/httpxtest].
	rt := &httpIdentity{base: srv.Client().Transport, log: discardLogger()}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read replayed body: %v", err)
	}
	if string(got) != payload {
		t.Errorf("replayed %q, want %q", got, payload)
	}
}

// TestIdentityWrapsTheSharedTransport is the round-tripper case internal/httpx
// names: a wrapper carries no *http.Client of its own, so the shape that
// signals a nil Transport elsewhere — an &http.Client{} with no field — never
// appears, and http.DefaultTransport sits at the base looking deliberate.
//
// It is the worst place in the tree for two idle connections per host: a
// company runs one of these per remote server PER SEAT, so every seat sharing
// one vendor endpoint contends on the same two.
func TestIdentityWrapsTheSharedTransport(t *testing.T) {
	t.Parallel()
	_, ident := newHTTPTransport(Spec{
		Name: "remote", URL: "https://mcp.example.com/",
	}, slog.New(slog.DiscardHandler))
	if ident.base != httpx.Transport() {
		t.Errorf("identity base = %T, want the one httpx shares", ident.base)
	}
}

// A LOGGED ERROR BODY IS NEVER CUT INSIDE A RUNE.
//
// A remote server's error text is prose — a workspace name, a scope, a quoted
// header — so the byte at maxLoggedErrorBody lands inside a multi-byte rune as
// soon as the text stops being ASCII. A bare body[:maxLoggedErrorBody] hands
// slog invalid UTF-8, whose JSON handler writes U+FFFD into the one line an
// operator has to diagnose a 403 from.
//
// The over-cap cases also pin that the field stays MARKED and that the cut is
// taken from a bounded HEAD of the body rather than the whole of it: the last
// case is a mebibyte with the straddling rune at the ceiling, and it must yield
// the identical answer to the short one.
func TestALoggedErrorBodyIsCutOnARuneBoundary(t *testing.T) {
	t.Parallel()
	// The straddling rune starts one byte before the ceiling and runs past it,
	// so a correct cut keeps exactly the ASCII prefix before it.
	const straddler = "\u20ac" // 3 bytes
	prefix := strings.Repeat("x", maxLoggedErrorBody-1)

	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"empty", "", "(empty)"},
		{"short bodies are verbatim", "{\"error\":\"no such scope: r\u00e9pertoire\"}", "{\"error\":\"no such scope: r\u00e9pertoire\"}"},
		{
			// Exactly at the ceiling is not over it: nothing was dropped, so
			// nothing may claim it was.
			"exactly at the ceiling is unmarked",
			strings.Repeat("x", maxLoggedErrorBody),
			strings.Repeat("x", maxLoggedErrorBody),
		},
		{
			"one byte over is marked",
			strings.Repeat("x", maxLoggedErrorBody+1),
			strings.Repeat("x", maxLoggedErrorBody) + truncationMarker,
		},
		{
			"a rune straddling the ceiling is walked back over",
			prefix + straddler + strings.Repeat("y", 32),
			prefix + truncationMarker,
		},
		{
			// Same straddle, past the bounded head boundedBody converts: the
			// answer may not depend on how much of the body it looked at.
			"the same straddle in a mebibyte body",
			prefix + straddler + strings.Repeat("y", 1<<20),
			prefix + truncationMarker,
		},
	} {
		got := boundedBody([]byte(tc.body))
		if !utf8.ValidString(got) {
			t.Errorf("%s: the logged field is not valid UTF-8 (last bytes %x)",
				tc.name, got[max(0, len(got)-8):])
		}
		if got != tc.want {
			t.Errorf("%s: boundedBody kept %d bytes, want %d",
				tc.name, len(got), len(tc.want))
		}
		if over := len(got) - len(truncationMarker); over > maxLoggedErrorBody {
			t.Errorf("%s: kept %d bytes of content, past the %d-byte ceiling",
				tc.name, over, maxLoggedErrorBody)
		}
	}
}

// THE SDK REPORTS SOME FAILURES WITHOUT THEIR BODY, which is what makes the
// http_error log line the ONLY copy of what the server said for them — and
// what [maxLoggedErrorBody]'s doc says about where the rest of a cut body is.
//
// Pinned against the SDK rather than believed: a release that starts
// surfacing these bodies turns this red, and the doc is rewritten with it. The
// JSON-RPC case is the control, proving the check can see a body in the
// caller's error when the SDK does pass one through.
func TestTheSDKReportsATransientStatusWithoutItsBody(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, body, said string
		status           int
		reachesCaller    bool
	}{
		{
			name: "a transient status", status: http.StatusServiceUnavailable,
			body: "maintenance until 17:00 UTC", said: "maintenance until 17:00 UTC",
		},
		{
			name: "a body that is not JSON-RPC", status: http.StatusForbidden,
			body: `{"error":"token lacks the repo scope"}`, said: "repo scope",
		},
		{
			name: "a JSON-RPC error, the control", status: http.StatusForbidden,
			body: `{"jsonrpc":"2.0","id":2,"error":{"code":-32001,"message":"workspace suspended"}}`,
			said: "workspace suspended", reachesCaller: true,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			srv, h := newHTTPMCPServer(t, toolsJSON([3]string{"get_me", "d", ""}))
			h.mu.Lock()
			h.fail, h.failBody = c.status, c.body
			h.mu.Unlock()

			log, rec := recorder()
			spec := httpSpec("remote", srv.URL)
			spec.StartupTimeout = 5 * time.Second
			cl, err := connect(t.Context(), spec, log)
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			t.Cleanup(func() { _ = cl.stop(t.Context()) })

			_, err = cl.listTools(t.Context())
			if err == nil {
				t.Fatalf("a %d on tools/list must fail discovery", c.status)
			}
			if got := strings.Contains(err.Error(), c.said); got != c.reachesCaller {
				t.Errorf("the caller's error %q carries the server's words: %v, "+
					"want %v", err, got, c.reachesCaller)
			}
			var logged bool
			for _, r := range rec.find("http_error") {
				body, _ := r.Attrs["response_body"].(string)
				logged = logged || strings.Contains(body, c.said)
			}
			if !logged {
				t.Error("the http_error line does not carry what the server said")
			}
		})
	}
}
