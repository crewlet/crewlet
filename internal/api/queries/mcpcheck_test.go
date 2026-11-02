package queries_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/org"
)

// fakeChecks is each server's latest test, by name.
type fakeChecks map[string]coord.MCPCheck

func (f fakeChecks) LatestMCPCheck(_ context.Context, server string) (coord.MCPCheck, bool, error) {
	c, ok := f[server]
	return c, ok, nil
}

// A SERVER'S LATEST TEST IS READ BACK AS IT WAS RUN: its seat by the chart's
// handle, each node's stages and failure, its own identity, and a node that
// found the server serving read as one that did, `empty_tools` included; a
// node that did not answer says why.
func TestAServersLatestTestIsReadBackAsItWasRun(t *testing.T) {
	t.Parallel()
	organization := &org.Organization{}
	at := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	seat := uuid.MustParse("00000000-0000-7000-8000-00000000000c")
	view := queries.MCPCheckOf(coord.MCPCheck{Server: "tracker", Seat: seat, At: at, By: "jane.doe",
		ByKind: iam.ActorHuman, Nodes: []coord.MCPCheckNode{
			{Node: "node-a", Answered: true, Tools: 0, Took: 1500 * time.Millisecond,
				Server: coord.MCPServerInfo{Name: "tracker-mcp", Version: "1.2.0"},
				Steps: []coord.MCPCheckStep{{Stage: "list", State: "passed", Took: 30 * time.Millisecond,
					Detail: "0 tools"}},
				Failure: &coord.MCPInstanceFailure{Class: string(mcp.FailEmptyTools), Said: "lists no tool"}},
			{Node: "node-b", Reason: "did not answer within 4m15s"},
		}}, organization)
	if view.Seat != seat.String() || !view.At.Equal(at) || view.By != "jane.doe" || len(view.Nodes) != 2 {
		t.Fatalf("view = %+v", view)
	}
	served := view.Nodes[0]
	if !served.OK || served.TookMS != 1500 || served.ServerInfo == nil || served.ServerInfo.Name != "tracker-mcp" ||
		served.Failure == nil || served.Failure.Class != mcp.FailEmptyTools || served.Steps[0].TookMS != 30 {
		t.Errorf("a node that found the server listing nothing = %+v", served)
	}
	if missed := view.Nodes[1]; missed.OK || missed.Answered || missed.ServerInfo != nil ||
		missed.Reason == "" || missed.Steps == nil {
		t.Errorf("a node that did not answer = %+v", missed)
	}
}
