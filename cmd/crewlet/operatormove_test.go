package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
)

// THE OPERATOR SURFACE FOLLOWS A LIVE MOVE, BOTH WAYS, AS THIS BINARY WIRES IT.
//
// The native backends start with the node and keep running through a revision
// that moves the company off them, so a surface built from what the node holds
// would go on writing native pages after a move to Confluence — where no search
// of the company reads them — and would never offer search to a company that
// gained a knowledge base after the process started. One session for the whole
// case, as an operator's assistant holds one across an apply; the seat half of
// the same rule is internal/engine's TestTheKnowledgeBaseFollowsALiveMoveBothWays.
func TestTheOperatorSurfaceFollowsALiveMove(t *testing.T) {
	t.Parallel()
	e := testEngine(t)
	for deadline := time.Now().Add(15 * time.Second); !e.NativeHydrated(); {
		if time.Now().After(deadline) {
			t.Fatal("the native backends never hydrated")
		}
		time.Sleep(20 * time.Millisecond)
	}
	surface := operatorMCP(e)
	if surface == nil {
		t.Fatal("a company on both native backends got no operator surface")
	}
	handler := surface.Handler()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// THE APP'S GUARD, reduced to what it leaves on the context.
		handler.ServeHTTP(w, r.WithContext(auth.WithOperator(r.Context(), "founder")))
	}))
	t.Cleanup(server.Close)

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "assistant", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: server.URL, HTTPClient: httpxtest.Pool(t),
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	apply := func(doc string) {
		t.Helper()
		company, err := config.ParseCompany([]byte(doc))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if _, _, err := e.Apply(ctx, company); err != nil {
			t.Fatalf("Apply: %v", err)
		}
	}
	// check lists the catalogue and holds each tool to whether it is served.
	check := func(step string, want map[string]bool) {
		t.Helper()
		res, err := session.ListTools(ctx, nil)
		if err != nil {
			t.Fatalf("%s: list tools: %v", step, err)
		}
		var names []string
		for _, tool := range res.Tools {
			names = append(names, tool.Name)
		}
		for name, served := range want {
			if slices.Contains(names, name) != served {
				t.Errorf("%s: %s is listed = %v, want %v", step, name, !served, served)
			}
		}
	}
	// refused calls a tool the surface no longer serves and wants the refusal
	// to name the setting that closed it.
	refused := func(step, name, field string, args map[string]any) {
		t.Helper()
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %s failed as a protocol error: %v", step, name, err)
		}
		var text strings.Builder
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				text.WriteString(tc.Text)
			}
		}
		if !res.IsError || !strings.Contains(text.String(), field) {
			t.Errorf("%s: %s answered %q (error %v), want a refusal naming %s",
				step, name, text.String(), res.IsError, field)
		}
	}
	// answered calls a tool the surface serves and wants its answer to say
	// every one of the phrases.
	answered := func(step, name string, args map[string]any, phrases ...string) {
		t.Helper()
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %s failed as a protocol error: %v", step, name, err)
		}
		var text strings.Builder
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				text.WriteString(tc.Text)
			}
		}
		for _, phrase := range phrases {
			if !strings.Contains(text.String(), phrase) {
				t.Errorf("%s: %s answered %q, which does not say %q",
					step, name, text.String(), phrase)
			}
		}
	}
	const (
		write  = builtin.WritePageTool
		create = builtin.CreateWorkItemTool
		search = builtin.SearchKnowledgeTool
	)

	check("booted on the native backends", map[string]bool{write: true, create: true, search: true})

	apply(companyYAML + `
integrations:
  confluence:
    url: https://wiki.example.com
    token: t
    webhook_secret: cf
  jira:
    url: https://jira.example.com
    token: t
    webhook_secret: js
`)
	// SEARCH STAYS: a Confluence wiki is searched exactly as a native one
	// is, and the tool itself says when this node is not serving it.
	check("moved to Confluence and Jira", map[string]bool{write: false, create: false, search: true})
	refused("moved to Confluence and Jira", write, "knowledge.backend",
		map[string]any{"title": "Runbook", "body": "Steps.", "container": "ENG"})
	refused("moved to Confluence and Jira", create, "tracker.backend",
		map[string]any{"title": "Ship it", "project": "ENG"})

	// A CONFLUENCE WHOSE ORG TOKEN RESOLVES TO NOTHING: the company runs a
	// knowledge base this node is not serving, since no searcher is built
	// without that credential. Search stays offered — it is registered the
	// seat way, on the company running a knowledge base at all — and the
	// tool names the setting to fix rather than answering that nothing
	// matched. The variable is one nothing sets.
	apply(companyYAML + `
integrations:
  confluence:
    url: https://wiki.example.com
    token: "${CREWLET_OPERATOR_TEST_NO_SUCH_CONFLUENCE_TOKEN}"
    webhook_secret: cf
`)
	check("moved to a Confluence it cannot search", map[string]bool{write: false, create: true, search: true})
	answered("moved to a Confluence it cannot search", search,
		map[string]any{"query": "deploy runbook"},
		"not searchable here", "integrations.confluence.token")

	apply(companyYAML + `
knowledge:
  backend: none
`)
	check("knowledge turned off", map[string]bool{write: false, create: true, search: false})
	refused("knowledge turned off", search, "knowledge.backend",
		map[string]any{"query": "deploy runbook"})

	apply(companyYAML)
	check("moved back to the native backends", map[string]bool{write: true, create: true, search: true})
}
