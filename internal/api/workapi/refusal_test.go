package workapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/api/opsmcp"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/tools"
)

// ONE REFUSAL READS THE SAME ON ALL THREE SURFACES.
//
// The same verb is served three ways — a seat's own turn, the operator's
// assistant over MCP, and this HTTP surface — and "you may not" must be one
// sentence on all of them. A person told one thing by the dashboard and
// another by their assistant about the same verb asks which is wrong, and the
// answer is neither: the only way to keep that question from arising is one
// sentence and nowhere else to write one.
//
// So the three are asked the SAME question by a caller holding the same
// grants — create an item, with state:read and nothing else — and the three
// texts compared byte for byte. The HTTP surface is the one that refuses at
// its ROUTE, before any tool runs, which is exactly why it is the one that
// could drift.
//
// The control is the router's own plain wording, which is what this surface
// would say without the refusal seam: it is a different sentence, so the
// comparison above is one that can fail.
func TestOneRefusalWordingOnAllThreeSurfaces(t *testing.T) {
	t.Parallel()
	grants := []iam.Grant{iam.GrantStateRead}
	r := newRig(t, chart{})
	deps := r.options(chart{}).Work
	args := map[string]any{"title": "rotate the key", "project": "ENG"}

	// A SEAT'S OWN TURN.
	o := &org.Organization{Name: "Acme",
		Roles: []*org.Role{{Name: "Engineer", DeclaredHandle: "eng"}}}
	o.Normalize()
	turn := &turnctx.Turn{RunID: "run-1", WorkKey: "work-1", Seat: o.Roles[0], Org: o}
	reg := tools.NewRegistry()
	if _, err := builtin.Register(reg, builtin.Deps{
		Work: deps, Authorize: builtin.Decide(chart{}),
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	entry, ok := reg.Lookup(builtin.CreateWorkItemTool)
	if !ok {
		t.Fatal("create_work_item is not registered")
	}
	seatCtx := iam.WithPrincipal(context.Background(), iam.Principal{
		ID: uuid.New(), Login: "eng", Seat: "eng", Kind: iam.KindSeat,
		Stage: iam.StageActive, Grants: grants,
	})
	fromSeat, err := entry.Tool.(tools.SeatCallable).CallForTurn(seatCtx, turn, args)
	if err != nil || !fromSeat.Failed {
		t.Fatalf("the seat's call was not refused: %v %s", err, fromSeat.Output)
	}

	// THE OPERATOR'S ASSISTANT, over MCP as its client reaches it.
	caller := person("ana", grants...)
	operator := opsmcp.New(opsmcp.Options{
		Work: deps, Authorize: builtin.Decide(chart{}),
	})
	handler := operator.Handler()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,
		req *http.Request) {
		handler.ServeHTTP(w,
			req.WithContext(iam.WithPrincipal(req.Context(), caller)))
	}))
	t.Cleanup(server.Close)
	client := mcp.NewClient(&mcp.Implementation{Name: "assistant", Version: "1"}, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: server.URL, HTTPClient: httpxtest.Pool(t),
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: builtin.CreateWorkItemTool, Arguments: args,
	})
	if err != nil || !res.IsError {
		t.Fatalf("the assistant's call was not refused: %v", err)
	}
	var fromMCP strings.Builder
	for _, c := range res.Content {
		if text, ok := c.(*mcp.TextContent); ok {
			fromMCP.WriteString(text.Text)
		}
	}

	// AND THIS SURFACE, refusing at the route.
	fromHTTP := r.do(as(caller), http.MethodPost, "/work/items", args)
	if fromHTTP.status != http.StatusForbidden {
		t.Fatalf("the route answered %d", fromHTTP.status)
	}

	if fromSeat.Output != fromMCP.String() || fromMCP.String() != fromHTTP.detail() {
		t.Errorf("one refusal reads three ways:\nseat: %s\nmcp:  %s\nhttp: %s",
			fromSeat.Output, fromMCP.String(), fromHTTP.detail())
	}

	// THE CONTROL: the router's own wording for the same refusal.
	plain := http.NewServeMux()
	if err := authz.NewRouter(plain, func(req *http.Request, p authz.Policy) authz.Decision {
		return authz.Decide(req.Context(), caller, p.Action,
			authz.Object{Kind: authz.KindTask}, chart{}, time.Now())
	}).Handle("POST /work/items", authz.Policy{Action: authz.ActionWorkCreate},
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})); err != nil {
		t.Fatalf("mount the control: %v", err)
	}
	rec := httptest.NewRecorder()
	plain.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/work/items", nil))
	if strings.TrimSpace(rec.Body.String()) == fromHTTP.detail() {
		t.Fatal("the control read the same as the tools, so this test cannot " +
			"tell a surface that wrote its own sentence from one that did not")
	}
}
