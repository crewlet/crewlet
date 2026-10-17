package engine

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/mcp"
)

// mcpStatusCompany is a company with one shared and one per-role server, both
// pointing at a binary that does not exist — so every start fails the way a
// vendor CLI missing from an image fails.
func mcpStatusCompany(t *testing.T, withShared bool) *Company {
	t.Helper()
	shared := ""
	if withShared {
		shared = `
  - name: search
    shared: true
    command: "/nonexistent/definitely-not-a-search-server"`
	}
	cfg, err := config.ParseCompany([]byte(`
name: Acme
providers:
  llm:
    zulu:
      model: claude-sonnet-5
mcp_servers:` + shared + `
  - name: tracker
    shared: false
    command: "/nonexistent/definitely-not-a-tracker-server"
roles:
  - name: CEO
    handle: ceo
    llm: zulu
    mcp_env:
      tracker:
        SEAT_TOKEN: "ceo-token"
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	company, err := NewCompany(cfg)
	if err != nil {
		t.Fatalf("NewCompany: %v", err)
	}
	return company
}

// A SERVER THAT WOULD NOT START IS ON THE HEARTBEAT, which is the one row an
// operator opening the settings screen is looking for. A seat's bridge keeps
// nothing of a failed start, so a status read off the bridge alone would show
// that server as never configured; the shared bridge keeps it to retry, and
// reports it down with why — and a released seat's failures, and a server the
// next revision dropped, must leave the heartbeat with them.
func TestAFailedMCPStartReachesTheHeartbeatAndLeavesWithItsOwner(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	e := &Engine{}
	e.mcp = mcp.NewBridge(nil, e.sharedSupervision())
	t.Cleanup(func() { _, _ = e.mcp.StopAll(context.Background()) })
	company := mcpStatusCompany(t, true)
	e.epoch.current.Store(company)

	e.startSharedServers(ctx, company)
	e.startSeatServers(ctx, "ceo")
	rows := statusOf(ctx, t, e).MCP
	if len(rows) != 2 {
		t.Fatalf("heartbeat carries %d mcp rows, want the shared and the per-role server: %+v", len(rows), rows)
	}
	search, tracker := rows[0], rows[1]
	if search.Server != "search" || !search.Shared || search.Failed != 1 || search.Started != 0 ||
		search.Error == "" || search.ErrorSeat != uuid.Nil {
		t.Errorf("shared row = %+v, want search, shared, one failure with its reason", search)
	}
	ceo, _ := company.Org.AgentIDFor(company.Org.AgentSeatByHandle("ceo"))
	if tracker.Server != "tracker" || tracker.Shared || tracker.Failed != 1 ||
		tracker.Error == "" || tracker.ErrorSeat != ceo || ceo == uuid.Nil {
		t.Errorf("per-role row = %+v, want tracker, per-role, one failure on ceo's id %s",
			tracker, ceo)
	}

	e.stopSeatServers(ctx, "ceo")
	if rows := statusOf(ctx, t, e).MCP; len(rows) != 1 || rows[0].Server != "search" {
		t.Errorf("after the seat was released the heartbeat carries %+v, want only the shared server", rows)
	}

	e.startSharedServers(ctx, mcpStatusCompany(t, false))
	if rows := statusOf(ctx, t, e).MCP; len(rows) != 0 {
		t.Errorf("after a revision dropped the shared server the heartbeat carries %+v", rows)
	}
}

// ONE ROW PER SERVER, its instances counted. A per-role template has a child
// per seat this node holds, and a row per child would put the company's seat
// count on a payload re-sent every heartbeat. The reported failure is the
// first by the seat's agent id, so two beats with the same facts send the same
// bytes.
func TestMCPStatusIsOneRowPerServerWithItsInstancesCounted(t *testing.T) {
	t.Parallel()
	// Ids ordered swe < pm, the reverse of their handles, so a first
	// failure picked by handle would name the other seat.
	swe := uuid.MustParse("00000000-0000-7000-8000-000000000001")
	pm := uuid.MustParse("00000000-0000-7000-8000-000000000002")
	ceo := uuid.MustParse("00000000-0000-7000-8000-000000000003")
	cto := uuid.MustParse("00000000-0000-7000-8000-000000000004")
	e := &Engine{
		mcpShared: []mcpOutcome{{server: "search", tools: 3}},
		mcpSeats: map[string][]mcpOutcome{
			"swe": {{server: "tracker", seat: swe, err: errors.New("401 from swe's token")}},
			"ceo": {{server: "tracker", seat: ceo, tools: 7}},
			"cto": {
				{server: "tracker", seat: cto, tools: 9},
				{server: "chat", seat: cto, err: errors.New("exec: not found")},
			},
			"pm": {{server: "tracker", seat: pm, err: errors.New("401 from pm's token")}},
		},
	}
	want := []coord.MCPServerStatus{
		{Server: "chat", Failed: 1, Error: "exec: not found", ErrorSeat: cto},
		{Server: "search", Shared: true, Started: 1, Tools: 3},
		{Server: "tracker", Started: 2, Failed: 2, Tools: 9, Error: "401 from swe's token", ErrorSeat: swe},
	}
	for range 5 { // map order must not reach the payload
		if got := e.mcpStatus(t.Context()); !reflect.DeepEqual(got, want) {
			t.Fatalf("mcp status = %+v\nwant %+v", got, want)
		}
	}
}

// A KEY IS THE SERVER'S UNLISTED KEY ONLY WHERE NO STARTED INSTANCE LISTS IT.
// Each seat's child serves what that seat's credentials reach — the jira-only
// seat's mcp-atlassian lists no Confluence tool — so its unlisted
// confluence_create_page is the entry being right for a sibling, and only a
// key every started child lacks (the typo) reaches the row. A failed start
// lists nothing and is not counted: it would make every key look unlisted.
func TestMCPStatusNamesOnlyKeysNoStartedInstanceLists(t *testing.T) {
	t.Parallel()
	swe := uuid.MustParse("00000000-0000-7000-8000-000000000001")
	pm := uuid.MustParse("00000000-0000-7000-8000-000000000002")
	ceo := uuid.MustParse("00000000-0000-7000-8000-000000000003")
	cto := uuid.MustParse("00000000-0000-7000-8000-000000000004")
	e := &Engine{
		mcpShared: []mcpOutcome{{server: "search", tools: 5, unlisted: []string{"search_web"}}},
		mcpSeats: map[string][]mcpOutcome{
			"pm":  {{server: "atlassian", seat: pm, tools: 63, unlisted: []string{"confluence_create_page", "jira_add_coment"}}},
			"cto": {{server: "atlassian", seat: cto, tools: 75, unlisted: []string{"jira_add_coment"}}},
			"swe": {{server: "atlassian", seat: swe, err: errors.New("401 from swe's token")}},
			"ceo": {{server: "chat", seat: ceo, tools: 4, unlisted: []string{"b_post", "a_post"}}},
		},
	}
	want := []coord.MCPServerStatus{
		{Server: "atlassian", Started: 2, Failed: 1, Tools: 75, Error: "401 from swe's token", ErrorSeat: swe,
			Unlisted: []string{"jira_add_coment"}},
		{Server: "chat", Started: 1, Tools: 4, Unlisted: []string{"a_post", "b_post"}},
		{Server: "search", Shared: true, Started: 1, Tools: 5, Unlisted: []string{"search_web"}},
	}
	for range 5 { // map order must not reach the payload
		if got := e.mcpStatus(t.Context()); !reflect.DeepEqual(got, want) {
			t.Fatalf("mcp status = %+v\nwant %+v", got, want)
		}
	}
}
