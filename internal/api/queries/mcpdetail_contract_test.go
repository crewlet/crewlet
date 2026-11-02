package queries_test

import (
	"reflect"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/mcp"
)

// THE SERVER PAGE READS WHAT THE DETAIL SENDS, in both readings.
//
// Settings › MCP & tools draws a server's page from `GET /mcp-servers/{name}`
// typed by `McpServerAnswer` and `McpMemberServerAnswer` and the shapes
// composed into them (`contract/mcp.ts`). Each answer is filled whole by
// reflection, so a field somebody adds to the engine's answer is sent here
// too, and every object in it is held to its declaration both ways: a field
// the page declares and the engine does not send, or one the engine sends and
// the page has no name for, fails. The closed sets the page words are held to
// the engine's own lists. The server's row, the removal's dependants and the
// calls' coverage are each held by the gate that owns their declaration.
//
// Mutation: drop `risk_declared` from McpToolView, or `wait` from
// McpInstanceView, or add a member usage row's `seats`, and this is red.
func TestTheServerPageReadsWhatTheDetailSends(t *testing.T) {
	t.Parallel()
	var detail queries.MCPServerAnswer
	fill(reflect.ValueOf(&detail).Elem())

	one := func(v any) []map[string]any {
		t.Helper()
		m, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("expected an object, the fill sent %T", v)
		}
		return []map[string]any{m}
	}
	at := func(m map[string]any, path ...string) any {
		t.Helper()
		var v any = m
		for _, key := range path {
			obj, ok := v.(map[string]any)
			if !ok {
				t.Fatalf("%v: %s is not under an object", path, key)
			}
			v = obj[key]
		}
		return v
	}

	admin := asMap(t, detail)
	holdShape(t, "McpServerAnswer", []map[string]any{admin}, false)
	holdShape(t, "McpServerListing", rowsOf(t, admin["listed"]), false)
	holdShape(t, "McpToolView", rowsOf(t, admin["tools"]), false)
	holdShape(t, "McpServerInfo", one(admin["server_info"]), false)
	instances := rowsOf(t, admin["instances"])
	holdShape(t, "McpInstanceView", instances, false)
	holdShape(t, "McpFailureView", slices.Concat(one(instances[0]["failure"]),
		one(instances[0]["last_failure"]), one(admin["last_failure"])), false)
	holdShape(t, "McpInstanceNodeView", rowsOf(t, admin["instance_nodes"]), false)
	holdShape(t, "McpResolved", one(admin["resolved"]), false)
	holdShape(t, "McpTimeouts", one(at(admin, "resolved", "timeouts")), false)
	holdShape(t, "McpBinding", one(at(admin, "resolved", "binding")), false)
	holdShape(t, "McpBoundKey", rowsOf(t, at(admin, "resolved", "binding", "keys")), false)
	holdShape(t, "McpBoxSeat", rowsOf(t, at(admin, "resolved", "box_seats")), false)
	holdShape(t, "McpPlacementWarning", rowsOf(t, at(admin, "resolved", "warnings")), false)
	holdShape(t, "McpServerUsage", one(admin["usage"]), false)
	holdShape(t, "McpToolUsage", rowsOf(t, at(admin, "usage", "tools")), false)
	holdShape(t, "McpServerCalls", one(admin["calls"]), false)
	holdShape(t, "McpCallRow", rowsOf(t, at(admin, "calls", "calls")), false)
	holdShape(t, "McpCheckView", one(admin["last_check"]), false)
	checked := rowsOf(t, at(admin, "last_check", "nodes"))
	holdShape(t, "McpCheckNodeView", checked, false)
	holdShape(t, "McpCheckStepView", rowsOf(t, checked[0]["steps"]), false)
	holdShape(t, "McpCheckFailure", one(checked[0]["failure"]), false)
	holdShape(t, "McpServerInfo", one(checked[0]["server_info"]), false)

	member := asMap(t, queries.MemberMCPServer(detail))
	holdShape(t, "McpMemberServerAnswer", []map[string]any{member}, false)
	holdShape(t, "McpToolView", rowsOf(t, member["tools"]), false)
	holdShape(t, "McpServerInfo", one(member["server_info"]), false)
	held := rowsOf(t, member["instances"])
	holdShape(t, "McpMemberInstanceView", held, false)
	holdShape(t, "McpMemberFailureView", slices.Concat(one(held[0]["failure"]),
		one(held[0]["last_failure"]), one(member["last_failure"])), false)
	holdShape(t, "McpMemberInstanceNodeView", rowsOf(t, member["instance_nodes"]), false)
	holdShape(t, "McpMemberResolved", one(member["resolved"]), false)
	holdShape(t, "McpMemberBinding", one(at(member, "resolved", "binding")), false)
	holdShape(t, "McpMemberServerUsage", one(member["usage"]), false)
	holdShape(t, "McpMemberToolUsage", rowsOf(t, at(member, "usage", "tools")), false)

	for union, want := range map[string][]string{
		"McpRisk":                 stringsOf(mcp.Risks),
		"McpInstanceState":        stringsOf(coord.MCPInstanceStates),
		"McpAuth":                 stringsOf(queries.MCPAuths),
		"McpPlacementWarningKind": stringsOf(queries.MCPPlacementWarningKinds),
		"McpCallOutcome":          stringsOf(mcp.CallOutcomes),
		"McpTestStage":            stringsOf(mcp.TestStages),
		"McpStepState":            stringsOf(mcp.StepStates),
	} {
		got, err := clientsource.Union(clientsource.Tree(t), union)
		if err != nil {
			t.Fatalf("%v: this gate cannot run without the client's declaration", err)
		}
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("the dashboard's %s is %v; the engine sends %v", union, got, want)
		}
	}
}
