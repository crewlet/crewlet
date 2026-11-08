package engine

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A HELD CALL IS PUT TO WHO MAY APPROVE IT, NEVER AN AGENT. An SSH operate
// command waits on the approvers integrations.ssh.approvers names and every
// administrator holding a seat, not on the asking seat's manager, an agent
// here, whatever label the run named; any other held call waits on the
// people among the seat's leads and the administrators. An agent is never in
// either, even one an administrator's list would name.
//
// Mutation: resolve a held call's label as any other question's, and the
// manager agent is its whole audience.
func TestAHeldCallIsPutToWhoMayApproveIt(t *testing.T) {
	t.Parallel()
	company := &Company{Org: audienceChart(t), Config: &config.Company{
		Integrations: config.Integrations{SSH: &config.SSH{Approvers: []string{"ada", "qa"}}}}}
	operate := sandbox.PendingRun{TurnID: "t1", AgentHandle: "swe", Audience: sandbox.AskToManager,
		HeldCall: &sandbox.HeldCall{ID: "h1", Tool: "ssh_operate", Fixed: true, Approvers: sandbox.HeldApproversSSH}}
	mcp := sandbox.PendingRun{TurnID: "t2", AgentHandle: "swe", Audience: sandbox.AskToManager,
		HeldCall: &sandbox.HeldCall{ID: "h2", Server: "tracker", Tool: "tr_close_issue"}}
	for _, c := range []struct {
		name   string
		run    sandbox.PendingRun
		admins []string
		want   []string
	}{
		{"an operate command", operate, []string{"founder", "cto"}, []string{"ada", "founder"}},
		{"an MCP tool's call", mcp, []string{"ada"}, []string{"founder", "ada"}},
	} {
		got := heldAudience(company, c.run, c.admins)
		if got.Fallback || !slices.Equal(got.Handles, c.want) {
			t.Errorf("%s is put to %+v, want %v", c.name, got, c.want)
		}
		for _, handle := range got.Handles {
			if !company.Org.Role(handle).IsHuman() {
				t.Errorf("%s is put to the agent %s", c.name, handle)
			}
		}
	}
	e := &Engine{}
	e.epoch.current.Store(company)
	if got := (audienceResolver{engine: e}).ResolveAudience(operate, sandbox.AskToManager); got.Fallback ||
		!slices.Equal(got.Handles, []string{"ada"}) {
		t.Errorf("the park puts an operate command to %+v, want its named approver", got)
	}
}
