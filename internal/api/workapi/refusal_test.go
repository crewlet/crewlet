package workapi_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/tools"
)

// ONE REFUSAL IS ONE DECISION ON ALL THREE SURFACES, IN EACH READER'S TERMS.
//
// The same verb is served three ways: a seat's own turn, the operator's
// assistant over MCP, and this HTTP surface. "You may not" must be one
// decision on all of them. The two that answer a MODEL answer the same
// sentence, byte for byte. This surface answers a PERSON (ADR-0060): it
// carries the same decision as values (the rule's reason and the grants that
// would have admitted the caller) and never the models' sentence, which names
// the tool and is written for a model to act on.
//
// So the three are asked the SAME question by a caller holding the same
// access (create an item, holding Doclet and not Worklet). The HTTP surface
// is the one that refuses at its ROUTE, before any tool runs, which is exactly
// why it is the one that could drift: its reason and grants are held to the
// decision the tool's own gate made.
func TestOneRefusalIsOneDecisionOnAllThreeSurfaces(t *testing.T) {
	t.Parallel()
	grants := []iam.Grant{iam.GrantDoclet}
	r := newRig(t, chart{})
	deps := r.halves().Work
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
	handler := r.operator(chart{}, serving(r.halves())).MCPHandler()
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
		Endpoint: server.URL + operator.MCPPath, HTTPClient: httpxtest.Pool(t),
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
	fromHTTP := r.do(as(caller), http.MethodPost, "/worklet/items", args)
	if fromHTTP.status != http.StatusForbidden {
		t.Fatalf("the route answered %d", fromHTTP.status)
	}

	if fromSeat.Output != fromMCP.String() {
		t.Errorf("one refusal reads two ways to a model:\nseat: %s\nmcp:  %s",
			fromSeat.Output, fromMCP.String())
	}
	var refused *builtin.DecisionRefused
	if !errors.As(fromSeat.Cause, &refused) {
		t.Fatalf("the seat's refusal carries no decision: %v", fromSeat.Cause)
	}
	_, _, want := authz.RefusalFields(refused.Decision)
	routeGrants, _ := fromHTTP.body[authz.DetailGrants].([]any)
	gateGrants, _ := want[authz.DetailGrants].([]string)
	if fromHTTP.body[authz.DetailReason] != want[authz.DetailReason] ||
		fmt.Sprint(routeGrants) != fmt.Sprint(gateGrants) {

		t.Errorf("the route decided %v, and the tool's gate %v", fromHTTP.body, want)
	}
	if detail := fromHTTP.detail(); detail != "" {
		t.Errorf("the route answered a person a sentence: %q", detail)
	}
	if strings.Contains(fmt.Sprint(fromHTTP.body), builtin.CreateWorkItemTool+" didn't run") {
		t.Errorf("the route answered the models' sentence: %v", fromHTTP.body)
	}
}
