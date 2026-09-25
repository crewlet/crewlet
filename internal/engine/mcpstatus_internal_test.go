package engine

import (
	"errors"
	"reflect"
	"slices"
	"testing"

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
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["sk-ant-fake-mcp-status-key"]
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
// operator opening the settings screen is looking for. The bridge keeps
// nothing of a failed start, so a status read off the bridge would show this
// server as never configured — and a released seat's failures, and a server
// the next revision dropped, must leave the heartbeat with them.
func TestAFailedMCPStartReachesTheHeartbeatAndLeavesWithItsOwner(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	e := &Engine{mcp: mcp.NewBridge(nil)}
	company := mcpStatusCompany(t, true)

	e.startSharedServers(ctx, company)
	e.startSeatServers(ctx, company, "ceo")
	rows := e.nodeStatus(ctx).MCP
	if len(rows) != 2 {
		t.Fatalf("heartbeat carries %d mcp rows, want the shared and the per-role server: %+v", len(rows), rows)
	}
	search, tracker := rows[0], rows[1]
	if search.Server != "search" || !search.Shared || search.Failed != 1 || search.Started != 0 ||
		search.Error == "" || search.ErrorSeat != "" {
		t.Errorf("shared row = %+v, want search, shared, one failure with its reason", search)
	}
	if tracker.Server != "tracker" || tracker.Shared || tracker.Failed != 1 ||
		tracker.Error == "" || tracker.ErrorSeat != "ceo" {
		t.Errorf("per-role row = %+v, want tracker, per-role, one failure on ceo", tracker)
	}

	e.stopSeatServers(ctx, "ceo")
	if rows := e.nodeStatus(ctx).MCP; len(rows) != 1 || rows[0].Server != "search" {
		t.Errorf("after the seat was released the heartbeat carries %+v, want only the shared server", rows)
	}

	e.startSharedServers(ctx, mcpStatusCompany(t, false))
	if rows := e.nodeStatus(ctx).MCP; len(rows) != 0 {
		t.Errorf("after a revision dropped the shared server the heartbeat carries %+v", rows)
	}
}

// ONE ROW PER SERVER, its instances counted. A per-role template has a child
// per seat this node holds, and a row per child would put the company's seat
// count on a payload re-sent every heartbeat. The reported failure is the
// first by seat handle, so two beats with the same facts send the same bytes.
func TestMCPStatusIsOneRowPerServerWithItsInstancesCounted(t *testing.T) {
	t.Parallel()
	e := &Engine{
		mcpShared: []mcpOutcome{{server: "search", tools: 3}},
		mcpSeats: map[string][]mcpOutcome{
			"swe": {{server: "tracker", seat: "swe", err: errors.New("401 from swe's token")}},
			"ceo": {{server: "tracker", seat: "ceo", tools: 7}},
			"cto": {
				{server: "tracker", seat: "cto", tools: 9},
				{server: "chat", seat: "cto", err: errors.New("exec: not found")},
			},
			"pm": {{server: "tracker", seat: "pm", err: errors.New("401 from pm's token")}},
		},
	}
	want := []coord.MCPServerStatus{
		{Server: "chat", Failed: 1, Error: "exec: not found", ErrorSeat: "cto"},
		{Server: "search", Shared: true, Started: 1, Tools: 3},
		{Server: "tracker", Started: 2, Failed: 2, Tools: 9, Error: "401 from pm's token", ErrorSeat: "pm"},
	}
	for range 5 { // map order must not reach the payload
		if got := e.mcpStatus(); !reflect.DeepEqual(got, want) {
			t.Fatalf("mcp status = %+v\nwant %+v", got, want)
		}
	}
}

// A NODE ADVERTISES EXACTLY WHAT ITS BUILD HONOURS. The list is a claim every
// peer acts on — a gesture it gates is accepted once every node carries the
// name — so it is this build's own vocabulary, and nothing a read could change.
func TestANodeAdvertisesWhatItsBuildHonours(t *testing.T) {
	t.Parallel()
	got := (&Engine{}).nodeStatus(t.Context()).Features
	if !slices.Equal(got, coord.Features) {
		t.Errorf("features = %v, want this build's %v", got, coord.Features)
	}
	if !slices.Contains(got, coord.FeatureMCPStatus) {
		t.Error("a node publishing its mcp status does not say so, so an empty list reads as \"did not say\"")
	}
}
