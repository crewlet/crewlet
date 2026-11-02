package mcpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/mcpapi"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
)

// fakeSignIns starts a sign-in to any server but the ones it refuses, and
// records who asked.
type fakeSignIns struct {
	mu       sync.Mutex
	refusals map[string]error
	started  []string
	ended    []string
}

var signInID = uuid.MustParse("018f3a9c-0000-7000-8000-0000000000aa")

func (f *fakeSignIns) StartSignIn(_ context.Context, server string, by iam.Actor) (mcpapi.SignInAnswer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.refusals[server]; err != nil {
		return mcpapi.SignInAnswer{}, err
	}
	f.started = append(f.started, server+" by "+by.Name)
	now := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	return mcpapi.SignInAnswer{ID: signInID, Server: server, Status: "pending", StartedAt: now,
		ExpiresAt:    now.Add(15 * time.Minute),
		AuthorizeURL: "https://auth.example.com/authorize?client_id=x&state=signed"}, nil
}

func (f *fakeSignIns) SignIn(_ context.Context, id uuid.UUID) (mcpapi.SignInAnswer, error) {
	if id != signInID {
		return mcpapi.SignInAnswer{}, mcpapi.ErrNoSignIn
	}
	now := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	return mcpapi.SignInAnswer{ID: signInID, Server: "linear", Status: "approved", StartedAt: now,
		ExpiresAt: now.Add(15 * time.Minute)}, nil
}

func (f *fakeSignIns) SignOut(_ context.Context, server string, by iam.Actor) (mcpapi.SignOutAnswer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.refusals[server]; err != nil {
		return mcpapi.SignOutAnswer{}, err
	}
	f.ended = append(f.ended, server+" by "+by.Name)
	return mcpapi.SignOutAnswer{Server: server, Revocation: "revoked"}, nil
}

func (f *fakeSignIns) calls() ([]string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.started), slices.Clone(f.ended)
}

func signInSurface(t *testing.T, signIns *fakeSignIns, audit *recordedAudit) *mcpapi.Service {
	t.Helper()
	company, err := config.ParseCompany([]byte("name: Acme\n"))
	if err != nil {
		t.Fatal(err)
	}
	svc, err := mcpapi.New(mcpapi.Options{Company: func() *config.Company { return company },
		Registry: &fakeRegistry{}, Detail: noDetail, Restarter: &fakeRestarter{}, Checker: &fakeChecker{},
		Values: valueStore(t), SignIns: signIns, Audit: audit})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

// call makes one request as a principal, answering its status and body.
func call(t *testing.T, svc *mcpapi.Service, as iam.Principal, method, path string) (int, map[string]any) {
	t.Helper()
	mux := http.NewServeMux()
	if err := svc.Routes(mux); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequestWithContext(iam.WithPrincipal(t.Context(), as), method, path, nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s %s answered %d with %q", method, path, w.Code, w.Body.String())
	}
	return w.Code, out
}

// assistant is proof's person acting through their Dashboard Assistant, on the
// one request they approved.
func assistant(proof iam.Principal) iam.Principal {
	proof.Via, proof.AssistScope = iam.AssistName(uuid.New()), iam.AssistScopeExec
	return proof
}

// A SIGN-IN IS STARTED AND ENDED ONLY AS THE SECRET'S WRITE IT IS: an
// administrator who stepped up within the hour, never a member, never a stale
// proof and never the Dashboard Assistant, whatever was approved; and every
// call, refused ones included, is one `mcp_server_acted` naming the server.
// The start answers the authorization address once; a read of the sign-in
// never does.
//
// Mutation: mount either gesture on config.write or tools.check, or answer the
// address on the read, and this is red.
func TestASignInIsStartedAndEndedOnlyAsTheSecretsWriteItIs(t *testing.T) {
	t.Parallel()
	signIns := &fakeSignIns{}
	audit := &recordedAudit{}
	svc := signInSurface(t, signIns, audit)
	fresh := proved(time.Minute)

	for name, who := range map[string]iam.Principal{
		"a stale proof":   proved(2 * time.Hour),
		"a member":        everyAppButAdmin(),
		"their assistant": assistant(fresh),
	} {
		for _, method := range []string{http.MethodPost, http.MethodDelete} {
			code, body := call(t, svc, who, method, "/mcp-servers/linear/oauth")
			if code != http.StatusForbidden {
				t.Errorf("%s by %s = %d %v, want 403", method, name, code, body)
			}
		}
	}
	if started, ended := signIns.calls(); len(started)+len(ended) != 0 {
		t.Fatalf("a refused caller reached the engine: %v %v", started, ended)
	}
	if rows := audit.all(); len(rows) != 6 || rows[0].Outcome != types.AuditRefused ||
		rows[0].Server != "linear" {
		t.Fatalf("the refusals audited %+v, want six refused rows naming the server", rows)
	}

	code, body := call(t, svc, fresh, http.MethodPost, "/mcp-servers/linear/oauth")
	if code != http.StatusCreated || body["authorize_url"] == "" || body["status"] != "pending" {
		t.Fatalf("a start = %d %v", code, body)
	}
	if got := keysOf(body); !slices.Equal(got, []string{"authorize_url", "detail", "expires_at", "id",
		"server", "started_at", "status"}) {
		t.Errorf("the answer's keys are %v", got)
	}
	if row := audit.last(t); row.Action != types.MCPServerSignIn || row.Outcome != types.AuditApplied {
		t.Errorf("the start audited %+v", row)
	}
	code, body = call(t, svc, proved(3*time.Hour), http.MethodGet, "/mcp-sign-ins/"+signInID.String())
	if code != http.StatusOK || body["status"] != "approved" || body["authorize_url"] != "" {
		t.Fatalf("a read = %d %v, want the status and no address", code, body)
	}
	if code, _ := call(t, svc, everyAppButAdmin(), http.MethodGet, "/mcp-sign-ins/"+signInID.String()); code != http.StatusForbidden {
		t.Errorf("a member read a sign-in: %d", code)
	}
	if code, _ := call(t, svc, fresh, http.MethodGet, "/mcp-sign-ins/"+uuid.NewString()); code != http.StatusNotFound {
		t.Errorf("an unknown sign-in answered %d", code)
	}
	code, body = call(t, svc, fresh, http.MethodDelete, "/mcp-servers/linear/oauth")
	if code != http.StatusOK || body["revocation"] != "revoked" {
		t.Fatalf("a sign-out = %d %v", code, body)
	}
	if got := keysOf(body); !slices.Equal(got, []string{"revocation", "said", "server"}) {
		t.Errorf("the sign-out's keys are %v", got)
	}
	if row := audit.last(t); row.Action != types.MCPServerSignOut || row.Outcome != types.AuditApplied {
		t.Errorf("the sign-out audited %+v", row)
	}
	if started, ended := signIns.calls(); len(started) != 1 || len(ended) != 1 ||
		!strings.HasSuffix(started[0], "by "+fresh.Login) {
		t.Errorf("the engine was asked %v %v", started, ended)
	}
}

// EACH REFUSAL SAYS WHAT TO CHANGE: a server nobody declares, one with no oauth
// block, an engine with no public address, nobody signed in, and a refusal of
// the server's metadata or its vendor, in the engine's words.
func TestASignInsRefusalSaysWhatToChange(t *testing.T) {
	t.Parallel()
	signIns := &fakeSignIns{refusals: map[string]error{
		"ghost":   mcpapi.ErrNoSuchServer,
		"plain":   mcpapi.ErrNoOAuth,
		"local":   mcpapi.ErrNoCallback,
		"idle":    mcpapi.ErrNotSignedIn,
		"no-pkce": &mcpapi.SignInRefusal{Status: http.StatusConflict, Detail: "the authorization server does not offer PKCE with S256"},
		"down":    &mcpapi.SignInRefusal{Status: http.StatusBadGateway, Detail: "the server could not be reached"},
	}}
	svc := signInSurface(t, signIns, &recordedAudit{})
	fresh := proved(time.Minute)
	for server, want := range map[string]struct {
		method, code string
		status       int
		says         string
	}{
		"ghost":   {http.MethodPost, "not_found", http.StatusNotFound, "declares no MCP server"},
		"plain":   {http.MethodPost, "conflict", http.StatusConflict, "oauth block"},
		"local":   {http.MethodPost, "conflict", http.StatusConflict, "api.external_url"},
		"idle":    {http.MethodDelete, "not_found", http.StatusNotFound, "nobody is signed in"},
		"no-pkce": {http.MethodPost, "conflict", http.StatusConflict, "S256"},
		"down":    {http.MethodPost, "vendor_refused", http.StatusBadGateway, "could not be reached"},
	} {
		status, body := call(t, svc, fresh, want.method, "/mcp-servers/"+server+"/oauth")
		detail, _ := body["detail"].(string)
		if status != want.status || body["error"] != want.code || !strings.Contains(strings.ToLower(detail), strings.ToLower(want.says)) {
			t.Errorf("%s = %d %v, want %d %s saying %q", server, status, body, want.status, want.code, want.says)
		}
	}
}

// A SURFACE NEEDS THE ENGINE'S SIGN-INS, since only the engine holds a grant.
func TestASurfaceNeedsSignIns(t *testing.T) {
	t.Parallel()
	if _, err := mcpapi.New(mcpapi.Options{Company: func() *config.Company { return nil },
		Detail: noDetail, Restarter: &fakeRestarter{}, Checker: &fakeChecker{},
		Values: valueStore(t), Audit: &recordedAudit{}}); err == nil {
		t.Error("a surface with no sign-ins was built")
	}
}

// all is every row the audit was given, in order.
func (a *recordedAudit) all() []types.MCPServerActed {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.rows)
}
