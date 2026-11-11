package mcpapi_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/mcpapi"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
)

// fakeToolDocuments records each write of a server's tool levels and answers
// the levels the case set, as the configuration would leave them.
type fakeToolDocuments struct {
	managed bool
	refuse  error
	leaves  config.MCPToolPolicy

	mu     sync.Mutex
	writes []configapi.MCPToolsWrite
}

func (f *fakeToolDocuments) Authorize(by iam.Actor) error {
	if f.managed {
		return &configapi.ManagedError{By: by, Writers: []string{"gitops"}}
	}
	return nil
}

func (f *fakeToolDocuments) SetMCPTools(_ context.Context, w configapi.MCPToolsWrite) (configapi.MCPToolsWritten, error) {
	f.mu.Lock()
	f.writes = append(f.writes, w)
	f.mu.Unlock()
	if f.refuse != nil {
		return configapi.MCPToolsWritten{}, f.refuse
	}
	if w.DryRun {
		return configapi.MCPToolsWritten{Tools: f.leaves, Answer: map[string]any{"valid": true,
			"base_revision_id": "rev-1", "warnings": []any{}, "derived": map[string]any{}}}, nil
	}
	return configapi.MCPToolsWritten{Tools: f.leaves, Applied: true, Answer: map[string]any{
		"revision_id": "rev-2", "epoch": 2, "warnings": []any{}, "derived": map[string]any{}}}, nil
}

func (f *fakeToolDocuments) RefuseMCPTools(w http.ResponseWriter, _ *http.Request, server string, err error) {
	if errors.Is(err, configapi.ErrNoSuchEntity) {
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNoSuchEntity, map[string]string{"hint": server})
		return
	}
	httpjson.Fail(w, http.StatusUnprocessableEntity, httpjson.CodeValidationError)
}

func (f *fakeToolDocuments) last(t *testing.T) configapi.MCPToolsWrite {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.writes) == 0 {
		t.Fatal("nothing was written")
	}
	return f.writes[len(f.writes)-1]
}

// listedDetail is a detail whose one live listing names the tools given, by
// the server's own names.
func listedDetail(raw ...string) func(context.Context, string) (queries.MCPServerAnswer, error) {
	return func(_ context.Context, name string) (queries.MCPServerAnswer, error) {
		if name != "tracker" {
			return queries.MCPServerAnswer{}, fmt.Errorf("%w: %s", queries.ErrNotFound, name)
		}
		answer := queries.MCPServerAnswer{Listed: []queries.MCPServerListing{{Node: "node-a", Tools: len(raw)}}}
		for _, r := range raw {
			answer.Tools = append(answer.Tools, queries.MCPToolView{Name: "tr_" + r, Raw: r})
		}
		return answer, nil
	}
}

func toolsSurface(t *testing.T, docs *fakeToolDocuments, detail func(context.Context, string) (queries.MCPServerAnswer, error),
	audit *recordedAudit) *mcpapi.Service {
	t.Helper()
	svc, err := mcpapi.New(mcpapi.Options{Company: func() *config.Company { return nil },
		Registry: &fakeRegistry{}, Detail: detail, Restarter: &fakeRestarter{}, Checker: &fakeChecker{},
		Tools: docs, Values: valueStore(t), SignIns: &fakeSignIns{}, Audit: audit})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

// putWith is put with a header set.
func putWith(t *testing.T, svc *mcpapi.Service, as iam.Principal, path, body string, header http.Header) (int, string) {
	t.Helper()
	mux := http.NewServeMux()
	if err := svc.Routes(mux); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequestWithContext(iam.WithPrincipal(t.Context(), as), http.MethodPut, path,
		strings.NewReader(body))
	for k, v := range header {
		req.Header[k] = v
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

// A WRITE OF TOOL LEVELS SAYS WHICH LEVELS DECIDE NOTHING. The body names
// tools by the server's own names, a null removing one and a default left out
// left alone; the answer is the levels the write leaves, with every level the
// fleet's listing names no tool for named in `unlisted` and kept, beside the
// configuration's own answer, in exactly these keys. A check is said as one.
//
// Mutation: answer unlisted from the levels the body sent rather than the
// levels the write leaves, read `default` absent as a removal, or drop a key,
// and this is red.
func TestAWriteOfToolLevelsSaysWhichLevelsDecideNothing(t *testing.T) {
	t.Parallel()
	docs := &fakeToolDocuments{leaves: config.MCPToolPolicy{Default: config.MCPToolBlocked,
		Levels: map[string]config.MCPToolLevel{"list_projects": config.MCPToolAllow,
			"tr_delete_project": config.MCPToolBlocked}}}
	audit := &recordedAudit{}
	svc := toolsSurface(t, docs, listedDetail("list_projects", "delete_project"), audit)

	code, raw := putWith(t, svc, proved(time.Minute), "/mcp-servers/tracker/tools",
		`{"levels": {"tr_delete_project": "blocked", "merge_projects": null}, "_summary": "in the body"}`,
		http.Header{"X-Summary": {"tracker: block deletion"}})
	body := decoded(t, raw)
	if code != http.StatusOK {
		t.Fatalf("PUT tools = %d %s", code, raw)
	}
	if got, want := keysOf(body), []string{"default", "derived", "dry_run", "epoch", "levels",
		"listing", "revision_id", "server", "unlisted", "warnings"}; !slices.Equal(got, want) {
		t.Errorf("the answer's keys are %v, want exactly %v", got, want)
	}
	if body["server"] != "tracker" || body["dry_run"] != false || body["default"] != "blocked" ||
		body["listing"] != mcpapi.ListingRead {
		t.Errorf("the answer = %v", body)
	}
	if unlisted, _ := body["unlisted"].([]any); len(unlisted) != 1 || unlisted[0] != "tr_delete_project" {
		t.Errorf("unlisted = %v, want the prefixed name no listing names", body["unlisted"])
	}
	write := docs.last(t)
	if write.Server != "tracker" || write.DryRun || write.Summary != "tracker: block deletion" ||
		write.Change.SetDefault || len(write.Change.Levels) != 2 || write.Change.Levels["merge_projects"] != nil ||
		*write.Change.Levels["tr_delete_project"] != config.MCPToolBlocked || write.Principal == nil {
		t.Errorf("the write asked = %+v", write)
	}
	if row := audit.last(t); row.Action != types.MCPServerSetTools || row.Server != "tracker" ||
		row.Outcome != types.AuditApplied || row.DryRun {
		t.Errorf("audited %+v", row)
	}

	// A CHECK, at a proof older than the step-up window, writes nothing and
	// is said as a check; a default sent as null removes it.
	code, raw = putWith(t, svc, proved(3*time.Hour), "/mcp-servers/tracker/tools?dry_run=true",
		`{"default": null}`, nil)
	body = decoded(t, raw)
	if code != http.StatusOK || body["dry_run"] != true || body["valid"] != true {
		t.Fatalf("a check = %d %v", code, body)
	}
	if write := docs.last(t); !write.DryRun || !write.Change.SetDefault || write.Change.Default != nil {
		t.Errorf("the check asked = %+v, want the default removed", write)
	}
	if row := audit.last(t); row.Action != types.MCPServerSetTools || !row.DryRun {
		t.Errorf("the check audited %+v", row)
	}
}

// A WRITE OF TOOL LEVELS IS A CONFIGURATION WRITE: refused at a stale proof
// before anything is read, refused on a managed document, refused a body that
// changes nothing or that the route does not read, refused a parameter it does
// not take, and answered 404 for a server the document does not declare. A
// listing nobody can read leaves the written levels written, and says so.
func TestAWriteOfToolLevelsIsAConfigurationWrite(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, path, body string
		as               iam.Principal
		docs             *fakeToolDocuments
		code             int
		error            string
	}{
		{"a stale proof", "/mcp-servers/tracker/tools", `{"default": "allow"}`, proved(3 * time.Hour),
			&fakeToolDocuments{}, http.StatusForbidden, "step_up_required"},
		{"a managed document", "/mcp-servers/tracker/tools", `{"default": "allow"}`, proved(time.Minute),
			&fakeToolDocuments{managed: true}, http.StatusForbidden, "config_managed"},
		{"a body that changes nothing", "/mcp-servers/tracker/tools", `{"levels": {}}`, proved(time.Minute),
			&fakeToolDocuments{}, http.StatusBadRequest, "invalid_body"},
		{"a field the route does not read", "/mcp-servers/tracker/tools", `{"default": "allow", "access": {}}`,
			proved(time.Minute), &fakeToolDocuments{}, http.StatusBadRequest, "invalid_body"},
		{"a parameter it does not take", "/mcp-servers/tracker/tools?seat=dev", `{"default": "allow"}`,
			proved(time.Minute), &fakeToolDocuments{}, http.StatusBadRequest, "invalid_query"},
		{"a server nobody declares", "/mcp-servers/billing/tools", `{"default": "allow"}`, proved(time.Minute),
			&fakeToolDocuments{refuse: configapi.ErrNoSuchEntity}, http.StatusNotFound, "no_such_entity"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			audit := &recordedAudit{}
			svc := toolsSurface(t, c.docs, listedDetail(), audit)
			code, raw := putWith(t, svc, c.as, c.path, c.body, nil)
			if body := decoded(t, raw); code != c.code || body["error"] != c.error {
				t.Fatalf("PUT = %d %v, want %d %s", code, body, c.code, c.error)
			}
			if c.docs.refuse == nil && len(c.docs.writes) != 0 {
				t.Errorf("a refused request wrote %+v", c.docs.writes)
			}
			if row := audit.last(t); row.Action != types.MCPServerSetTools || row.Outcome == types.AuditApplied {
				t.Errorf("the refusal audited %+v", row)
			}
		})
	}

	unread := func(context.Context, string) (queries.MCPServerAnswer, error) {
		return queries.MCPServerAnswer{}, fmt.Errorf("the store did not answer")
	}
	docs := &fakeToolDocuments{leaves: config.MCPToolPolicy{Levels: map[string]config.MCPToolLevel{
		"nowhere": config.MCPToolBlocked}}}
	code, raw := putWith(t, toolsSurface(t, docs, unread, &recordedAudit{}), proved(time.Minute),
		"/mcp-servers/tracker/tools", `{"levels": {"nowhere": "blocked"}, "_summary": "x"}`, nil)
	if body := decoded(t, raw); code != http.StatusOK || body["listing"] != mcpapi.ListingUnavailable ||
		len(body["unlisted"].([]any)) != 0 || body["default"] != nil {
		t.Fatalf("a write whose listing was unread = %d %v", code, body)
	}
}
