package queries_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/secrets"
)

// heldRuns is a run store holding backend-dev's run parked on a call to
// github's merge tool, its arguments sealed under seal, and a run not parked
// on any call; and the seal that opens the arguments.
func heldRuns(t *testing.T) (fixedRuns, *sandbox.CallSeal) {
	t.Helper()
	cipher, err := secrets.NewCipher(secrets.Keyring{ActiveID: "k1", Keys: map[string][]byte{"k1": make([]byte, 32)}})
	if err != nil {
		t.Fatal(err)
	}
	seal := sandbox.NewCallSeal(cipher)
	held := sandbox.HeldCall{ID: "0190f3a8-0000-7000-8000-000000000001", LaunchID: "launch-1",
		Server: "github", Tool: "gh_merge_pull_request", Raw: "merge_pull_request", Digest: "sha256:d1",
		HeldAt: time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC),
		Acting: &sandbox.HeldAct{Item: "id-4", Key: "ENG-4", Ask: "q-2", Question: "Merge the release?",
			Choice: "Merge it", AnsweredBy: "cto", AnsweredAt: time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC)}}
	if held.Arguments, err = seal.SealArguments("t-held", held, map[string]any{"pull": float64(7)}); err != nil {
		t.Fatal(err)
	}
	return fixedRuns{
		{TurnID: "t-held", LaunchID: "launch-1", AgentHandle: "backend-dev", Status: sandbox.StatusAwaiting,
			Question:        "May backend-dev call gh_merge_pull_request on the github MCP server?",
			AudienceHandles: []string{"cto"}, HeldCall: &held,
			WorkItem: &types.WorkItem{Backend: types.WorkWorklet, ID: "id-4", Key: "ENG-4"}},
		{TurnID: "t-plain", LaunchID: "launch-2", AgentHandle: "backend-dev", Status: sandbox.StatusRunning},
	}, seal
}

// A HELD CALL IS THE ADMINISTRATOR'S TO READ WHOLE (ADR-0077): the MCP
// answer's `approvals` lists it, its arguments opened, for the configuration's
// reader alone, and a member's reading carries no approvals at all. A run not
// parked on a call is listed nowhere.
//
// Mutation: answer approvals on the member's reading, or list a run that holds
// no call, and this is red.
func TestAHeldCallIsListedForAnAdministratorAndNoMember(t *testing.T) {
	t.Parallel()
	r := queries.NewRegistry()
	src := mcpSources(t, mcpFleet(t))
	src.Sandbox, src.HeldCalls = heldRuns(t)
	queries.Register(r, src)
	raw, err := r.Answer(everyGrant(t), "mcp_servers_status", nil)
	if err != nil {
		t.Fatal(err)
	}
	approvals := raw.(queries.MCPStatusAnswer).Approvals
	if len(approvals) != 1 {
		t.Fatalf("approvals = %+v, want the one held call", approvals)
	}
	a := approvals[0]
	if a.Seat != "backend-dev" || a.Raw != "merge_pull_request" || a.Status != queries.MCPApprovalWaiting ||
		a.Args["pull"] != float64(7) || a.Item == nil || *a.Item != "ENG-4" || a.AnsweredAt != nil {
		t.Fatalf("the approval reads %+v", a)
	}
	member, err := r.Answer(iam.WithPrincipal(t.Context(), person(iam.StageActive, everyAppButAdmin()...)),
		"mcp_servers_status", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, has := asMap(t, member)["approvals"]; has {
		t.Fatal("a member's reading carries the calls held for approval")
	}
}
