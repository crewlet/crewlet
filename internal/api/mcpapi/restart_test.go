package mcpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/mcpapi"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
)

// fakeRestarter records each restart it is asked for, and refuses the ones
// refusals names.
type fakeRestarter struct {
	refusals map[string]error

	mu    sync.Mutex
	asked []string
}

func (f *fakeRestarter) RestartServer(_ context.Context, name, seat string, by iam.Actor) (mcpapi.Restart, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, name+"/"+seat+" by "+by.Name)
	if err, refused := f.refusals[name+"/"+seat]; refused {
		return mcpapi.Restart{}, err
	}
	return mcpapi.Restart{Server: name, Seat: seat, Node: "node-a",
		At: time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)}, nil
}

func (f *fakeRestarter) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.asked)
}

// recordedAudit keeps every MCP server gesture published.
type recordedAudit struct {
	mu   sync.Mutex
	rows []types.MCPServerActed
}

func (a *recordedAudit) Publish(_ context.Context, _ string, ev *events.Event) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if row, ok := ev.Data.(*types.MCPServerActed); ok {
		a.rows = append(a.rows, *row)
	}
	return nil
}

func (a *recordedAudit) last(t *testing.T) types.MCPServerActed {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.rows) == 0 {
		t.Fatal("nothing was audited")
	}
	return a.rows[len(a.rows)-1]
}

// restartSurface is the surface over restarter, auditing into audit.
func restartSurface(t *testing.T, restarter mcpapi.Restarter, audit *recordedAudit) *mcpapi.Service {
	t.Helper()
	company, err := config.ParseCompany([]byte("name: Acme\n"))
	if err != nil {
		t.Fatal(err)
	}
	svc, err := mcpapi.New(mcpapi.Options{Company: func() *config.Company { return company },
		Registry: &fakeRegistry{}, Detail: noDetail, Restarter: restarter, Audit: audit})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

// post makes one restart request as a principal, with body.
func post(t *testing.T, svc *mcpapi.Service, as iam.Principal, server, body string) (int, map[string]any) {
	t.Helper()
	mux := http.NewServeMux()
	if err := svc.Routes(mux); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequestWithContext(iam.WithPrincipal(t.Context(), as), http.MethodPost,
		"/mcp-servers/"+server+"/retry", strings.NewReader(body))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("a restart answered %d with %q", w.Code, w.Body.String())
	}
	return w.Code, out
}

// A RESTART IS ASKED OF THE FLEET AND AUDITED, REFUSALS INCLUDED. An admin
// restarts every instance with an empty body, or one seat's with its handle,
// whatever the age of their proof, and is answered the request as recorded; a
// member holding every app is refused by the guard; each refusal of the
// engine's is answered with what the person can change; and every call, the
// guard's refusal included, is one `mcp_server_acted` naming the server and
// the seat, never anything a server is started with.
//
// Mutation: mount the route on config.write, answer a refusal as a success,
// or leave a refused call out of the audit, and this is red.
func TestARestartIsAskedOfTheFleetAndAuditedRefusalsIncluded(t *testing.T) {
	t.Parallel()
	restarter := &fakeRestarter{refusals: map[string]error{
		"nothing/":       fmt.Errorf("%w: nothing", mcpapi.ErrNoSuchServer),
		"wiki/":          fmt.Errorf("%w: wiki", mcpapi.ErrServerOff),
		"search/ceo":     fmt.Errorf("%w: search", mcpapi.ErrSeatOnShared),
		"tracker/nobody": fmt.Errorf("%w: nobody", mcpapi.ErrNoSuchSeat),
		"tracker/cto":    fmt.Errorf("%w: cto", mcpapi.ErrSeatNotGranted),
		"broken/":        errors.New("coord: the store could not be reached"),
	}}
	audit := &recordedAudit{}
	svc := restartSurface(t, restarter, audit)
	admin := caller(iam.GrantAdmin)

	code, body := post(t, svc, admin, "tracker", "")
	if code != http.StatusAccepted || body["server"] != "tracker" || body["seat"] != "" ||
		body["node"] != "node-a" || body["at"] != "2026-11-01T09:00:00Z" {
		t.Fatalf("a restart of every instance answered %d %v", code, body)
	}
	holdKeys(t, "the restart's answer", body, "server", "seat", "at", "node")
	if row := audit.last(t); row.Action != types.MCPServerRestart || row.Server != "tracker" || row.Seat != "" ||
		row.Outcome != types.AuditApplied || row.ActorName != "jane.doe" {
		t.Errorf("audited %+v, want jane's applied restart of tracker", row)
	}
	if code, body := post(t, svc, admin, "tracker", `{"seat": "ceo"}`); code != http.StatusAccepted ||
		body["seat"] != "ceo" {
		t.Errorf("one seat's restart answered %d %v", code, body)
	}
	if row := audit.last(t); row.Seat != "ceo" || row.Outcome != types.AuditApplied {
		t.Errorf("one seat's restart audited %+v", row)
	}

	member := caller(slices.DeleteFunc(slices.Clone(iam.AllGrants), func(g iam.Grant) bool { return g == iam.GrantAdmin })...)
	if code, body := post(t, svc, member, "tracker", ""); code != http.StatusForbidden {
		t.Errorf("a member holding every app answered %d %v", code, body)
	}
	if row := audit.last(t); row.Outcome != types.AuditRefused || row.Refusal == "" || row.Server != "tracker" {
		t.Errorf("the guard's refusal audited %+v, want refused with its code", row)
	}

	for _, tc := range []struct {
		server, body string
		status       int
		code         string
	}{
		{"nothing", "", http.StatusNotFound, "not_found"},
		{"wiki", "", http.StatusConflict, "conflict"},
		{"search", `{"seat":"ceo"}`, http.StatusBadRequest, "invalid_body"},
		{"tracker", `{"seat":"nobody"}`, http.StatusNotFound, "no_such_seat"},
		{"tracker", `{"seat":"cto"}`, http.StatusConflict, "conflict"},
	} {
		code, body := post(t, svc, admin, tc.server, tc.body)
		if code != tc.status || body["error"] != tc.code || body["detail"] == "" {
			t.Errorf("restart %s %s answered %d %v, want %d %s with why", tc.server, tc.body, code, body,
				tc.status, tc.code)
		}
		if row := audit.last(t); row.Outcome != types.AuditRefused || row.Refusal != tc.code {
			t.Errorf("restart %s %s audited %+v, want refused %s", tc.server, tc.body, row, tc.code)
		}
	}
	if code, _ := post(t, svc, admin, "broken", ""); code != http.StatusServiceUnavailable {
		t.Errorf("a store that could not be written answered %d, want 503", code)
	}
	if row := audit.last(t); row.Outcome != types.AuditFailed {
		t.Errorf("a failed restart audited %+v", row)
	}

	before := len(restarter.calls())
	if code, body := post(t, svc, admin, "tracker", `{"seat":"ceo","force":true}`); code != http.StatusBadRequest ||
		body["error"] != "invalid_body" {
		t.Errorf("a body with a field the route does not read answered %d %v", code, body)
	}
	if len(restarter.calls()) != before {
		t.Error("a body the route refused still asked for a restart")
	}
}

// EVERY ROUTE IS DECIDED ON ITS OWN VERB: the catalogue and the detail are the
// configuration's reader's, and a restart is `tools.retry`, declared as acting
// on the server its path names and keeping it.
func TestEveryMCPRouteTakesItsVerb(t *testing.T) {
	t.Parallel()
	svc := restartSurface(t, &fakeRestarter{}, &recordedAudit{})
	mux := &recordingMux{ServeMux: http.NewServeMux(), policies: map[string]authz.Policy{}}
	if err := svc.Routes(mux); err != nil {
		t.Fatal(err)
	}
	want := map[string]authz.Action{
		"GET /mcp-catalogue":             authz.ActionConfigRead,
		"GET /mcp-servers/{name}":        authz.ActionConfigRead,
		"POST /mcp-servers/{name}/retry": authz.ActionToolsRetry,
	}
	for pattern, action := range want {
		if got, ok := mux.policies[pattern]; !ok || got.Action != action {
			t.Errorf("%s is mounted with %+v, want %s", pattern, got, action)
		}
	}
	if got := mux.policies["POST /mcp-servers/{name}/retry"]; got.Effect != authz.EffectRewrites {
		t.Errorf("the restart declares effect %q, want it acts on its server and keeps it", got.Effect)
	}
	if len(mux.policies) != len(want) {
		t.Errorf("mounted %d routes, want %d", len(mux.policies), len(want))
	}
	if _, err := mcpapi.New(mcpapi.Options{Company: func() *config.Company { return nil },
		Detail: noDetail, Audit: &recordedAudit{}}); err == nil {
		t.Error("a surface with no restarter was built")
	}
	if _, err := mcpapi.New(mcpapi.Options{Company: func() *config.Company { return nil },
		Detail: noDetail, Restarter: &fakeRestarter{}}); err == nil {
		t.Error("a surface with no audit was built")
	}
}

type recordingMux struct {
	*http.ServeMux
	policies map[string]authz.Policy
}

func (m *recordingMux) MountedWith(pattern string, p authz.Policy) { m.policies[pattern] = p }
