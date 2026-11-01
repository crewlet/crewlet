package mcpbridge_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/iam"
	crewletmcp "github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/runtoken"
)

// A SESSION THAT IS NOT A SEAT'S: the bridge serves any [mcpbridge.Surface]
// under the [mcpbridge.Caller] it is handed, and holds that caller's token to
// the run alone (ADR-0054's deep work). Every case here is about what a token
// the model could leak is worth.

// probeSurface is one tool that answers who its call was decided as.
type probeSurface struct{ calls atomic.Int32 }

func (*probeSurface) Revision() uint64 { return 1 }
func (*probeSurface) ToolDefs() []llm.ToolDef {
	return []llm.ToolDef{{Name: "probe", Description: "who am I",
		Parameters: map[string]any{"type": "object"}}}
}
func (*probeSurface) AnnotationsOf(string) crewletmcp.Annotations { return crewletmcp.Annotations{} }
func (p *probeSurface) Admit(string, map[string]any) toolloop.Admitted {
	return func(ctx context.Context, _ llm.ToolCall) (toolloop.ToolResult, error) {
		p.calls.Add(1)
		_, how := iam.From(ctx)
		return toolloop.ToolResult{Output: string(how)}, nil
	}
}

// nobody decides every call as no principal at all: the caller's surface
// carries its own authority, and the request's identity must reach none of it.
type nobody struct{ bound bool }

var errNobody = errors.New("decided by the surface's own grant")

func (n nobody) Acting(ctx context.Context) context.Context {
	return iam.WithUnresolved(ctx, errNobody)
}
func (n nobody) Bound() bool { return n.bound }

// served is a bridge over one probe session, its clock settable.
type served struct {
	bridge  *mcpbridge.Bridge
	server  *httptest.Server
	surface *probeSurface
	clock   *atomic.Int64
}

func serve(t *testing.T) *served {
	t.Helper()
	s := &served{surface: &probeSurface{}, clock: &atomic.Int64{}}
	s.clock.Store(time.Now().UnixNano())
	s.server = httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(s.server.Close)
	s.bridge = bridgeFor(t, mcpbridge.Options{
		Material: runtoken.OneKey("k", "test-key"), BaseURL: s.server.URL,
		Now: func() time.Time { return time.Unix(0, s.clock.Load()) },
	})
	mux := http.NewServeMux()
	mux.Handle(mcpbridge.PathPrefix+"{token}", s.bridge.Handler())
	s.server.Config.Handler = mux
	return s
}

func ping(t *testing.T, s *served, url string) int {
	t.Helper()
	res, err := s.server.Client().Post(url, "application/json",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer res.Body.Close()
	return res.StatusCode
}

func TestACallerBoundToNobodyOpensNoSession(t *testing.T) {
	t.Parallel()
	s := serve(t)
	if url := s.bridge.Open(t.Context(), &mcpbridge.Session{RunID: "deep-1", Launch: "job-1", Surface: s.surface,
		Caller: nobody{bound: false}}); url != "" {
		t.Fatalf("a session with nobody to act as was opened: %s", url)
	}
	if url := s.bridge.Open(t.Context(), &mcpbridge.Session{RunID: "deep-1", Launch: "job-1", Surface: s.surface}); url != "" {
		t.Fatalf("a session with no caller was opened: %s", url)
	}
}

// THE REQUEST'S OWN IDENTITY REACHES NO CALL: whatever the box presented,
// the call is decided as the caller says, here as nobody at all.
func TestACallIsDecidedAsItsCallerNeverAsTheRequest(t *testing.T) {
	t.Parallel()
	s := serve(t)
	url := s.bridge.Open(t.Context(), &mcpbridge.Session{RunID: "deep-1", Launch: "job-1", Surface: s.surface,
		Caller: nobody{bound: true}})
	sess := dial(t, url)
	if got := text(callTool(t, sess, "probe", nil)); got != string(iam.Unknown) {
		t.Fatalf("the call was decided as %q, want the caller's %q", got, iam.Unknown)
	}
}

// A SESSION'S TTL CAPS ITS TOKEN below the bridge's four hours, so a token a
// deep run leaks dies with the run's wall clock even if nothing closed it.
func TestASessionsTTLCapsItsToken(t *testing.T) {
	t.Parallel()
	s := serve(t)
	url := s.bridge.Open(t.Context(), &mcpbridge.Session{RunID: "deep-1", Launch: "job-1", Surface: s.surface,
		Caller: nobody{bound: true}, TTL: time.Minute})
	if got := ping(t, s, url); got == http.StatusUnauthorized {
		t.Fatal("a fresh token was refused")
	}
	s.clock.Add(int64(2 * time.Minute))
	if got := ping(t, s, url); got != http.StatusUnauthorized {
		t.Fatalf("a token past its session's TTL answered %d", got)
	}
}

// A CLOSED RUN'S TOKEN IS WORTH NOTHING, at once.
func TestAClosedDeepRunsTokenIsRefused(t *testing.T) {
	t.Parallel()
	s := serve(t)
	url := s.bridge.Open(t.Context(), &mcpbridge.Session{RunID: "deep-1", Launch: "job-1", Surface: s.surface,
		Caller: nobody{bound: true}})
	s.bridge.Close("deep-1")
	if got := ping(t, s, url); got != http.StatusUnauthorized {
		t.Fatalf("a closed run's token answered %d", got)
	}
	if n := s.surface.calls.Load(); n != 0 {
		t.Fatalf("%d calls reached a closed run's surface", n)
	}
}

// A LOOPBACK SESSION ADMITS ONLY THIS HOST: the token a confined host run
// holds is refused from any other address, so a leaked copy is useless off
// the engine host. Every call names the loopback host a confined run dials,
// so the address is all that differs; the Host such a session admits is
// host_test.go's.
func TestALoopbackSessionRefusesAnotherHost(t *testing.T) {
	t.Parallel()
	s := serve(t)
	url := s.bridge.Open(t.Context(), &mcpbridge.Session{RunID: "deep-1", Launch: "job-1", Surface: s.surface,
		Caller: nobody{bound: true}, Loopback: true})
	path := strings.TrimPrefix(url, s.server.URL)
	for addr, want := range map[string]bool{"203.0.113.9:4711": false, "127.0.0.1:4711": true,
		"[::1]:4711": true} {
		req := httptest.NewRequest(http.MethodPost, path,
			strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Host = "127.0.0.1:8000"
		req.RemoteAddr = addr
		rec := httptest.NewRecorder()
		s.server.Config.Handler.ServeHTTP(rec, req)
		if admitted := rec.Code != http.StatusUnauthorized; admitted != want {
			t.Errorf("from %s: admitted=%v (status %d), want %v", addr, admitted, rec.Code, want)
		}
	}
}
