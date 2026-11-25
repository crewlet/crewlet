package engine

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// failingAt is ops's Datadog child failing, as a pass in a process started at
// since dates it.
func failingAt(ops uuid.UUID, since time.Time) map[string][]mcpOutcome {
	return map[string][]mcpOutcome{"ops": {{server: "datadog", seat: ops,
		err: errors.New(`calling "initialize": Unauthorized`), since: since}}}
}

// heartbeat is the datadog row a node's status record carries, keeping every
// outage as statusRecordRows does.
func heartbeat(t *testing.T, e *Engine) coord.MCPServerStatus {
	t.Helper()
	rows := e.mcpStatus(t.Context())
	e.keepMCPDown(t.Context())
	for _, row := range rows {
		if row.Server == "datadog" {
			return row
		}
	}
	t.Fatalf("rows = %+v, want datadog's", rows)
	return coord.MCPServerStatus{}
}

// AN OUTAGE KEEPS ITS START ACROSS A RESTART, in the status row and the alarm,
// until the instance serves again or no pass starts it. Mutation: skip the
// recall, and the restarted node dates the outage from its own first pass.
func TestAnMCPOutageKeepsItsStartAcrossARestart(t *testing.T) {
	t.Parallel()
	db := storetest.OpenNode(t, filepath.Join(t.TempDir(), "node.db"), store.Options{})
	ops := uuid.MustParse("00000000-0000-7000-8000-000000000001")
	began := time.Now().Add(-22 * time.Hour).UTC().Truncate(time.Microsecond)
	booted := began.Add(21 * time.Hour)

	first := &Engine{backends: &Backends{Store: db}, mcpSeats: failingAt(ops, began),
		mcpShared: []mcpOutcome{{server: "search", err: errors.New("exit status 1"), since: began}}}
	if row := heartbeat(t, first); !row.DownSince.Equal(began) {
		t.Fatalf("before the restart the row is down since %s, want %s", row.DownSince, began)
	}
	first.mcpShared = nil

	// THE RESTART: a new process on the same file, whose first pass dates
	// the same failure at its own boot.
	restarted := &Engine{backends: &Backends{Store: db}, mcpSeats: failingAt(ops, booted)}
	row := heartbeat(t, restarted)
	if !row.DownSince.Equal(began) || row.Failed != 1 {
		t.Fatalf("after the restart the row is %+v, want down since %s", row, began)
	}
	now := began.Add(22 * time.Hour)
	failing := mcpEntries(statelog.Reading{MCPServersAt: now, MCPServers: []statelog.MCPServerCondition{{
		Server: row.Server, Failed: row.Failed, DownSince: row.DownSince}}}, statelog.KindMCPServerFailing)
	if len(failing) != 1 || !failing[0].Since.Equal(began) || !strings.Contains(failing[0].Detail, "22h") {
		t.Errorf("mcp_server_failing stands on %+v, want it down for 22h since %s", failing, began)
	}
	restarted.recallMCPDown(t.Context())
	restarted.mcpDown.keepOnly(uuid.Nil, nil)
	restarted.keepMCPDown(t.Context())

	// SERVING AGAIN resets it, kept across the next restart too.
	restarted.mcpSeats = map[string][]mcpOutcome{"ops": {{server: "datadog", seat: ops, tools: 3}}}
	if row := heartbeat(t, restarted); row.Started != 1 || !row.DownSince.IsZero() {
		t.Fatalf("a serving instance's row is %+v", row)
	}
	later := now.Add(time.Hour)
	again := &Engine{backends: &Backends{Store: db}, mcpSeats: failingAt(ops, later),
		mcpShared: []mcpOutcome{{server: "search", err: errors.New("exit status 1"), since: later}}}
	rows := again.mcpStatus(t.Context())
	for _, row := range rows {
		if !row.DownSince.Equal(later) {
			t.Errorf("%s is down since %s after it served or left the pass, want %s", row.Server,
				row.DownSince, later)
		}
	}
}

// mcpEntries is the entries kind stands on in a reading.
func mcpEntries(r statelog.Reading, kind statelog.Kind) []statelog.AlarmEntry {
	for _, a := range statelog.Evaluate(r) {
		if a.Kind == kind {
			return a.Entries
		}
	}
	return nil
}
