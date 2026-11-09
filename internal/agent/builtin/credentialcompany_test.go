package builtin_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/host"
	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	sshint "github.com/crewlet/crewlet/internal/ssh"
	"github.com/crewlet/crewlet/internal/tools"
)

// credentialTurn is a turn of seat that a credential acting as no seat woke,
// by its login, its writes carrying apps.
func credentialTurn(turn *turnctx.Turn, login string, apps []iam.Grant) *turnctx.Turn {
	turn.Requester, turn.RequesterLogin, turn.RequesterApps = "", login, slices.Clone(apps)
	turn.Senders = []types.CanonicalIdentity{{ExternalID: login, Platform: "worklet", Login: login}}
	return turn
}

// refusedForACredential reports a tool refused, with the requester ceiling's
// reason, for a turn a credential acting as no seat woke.
func refusedForACredential(got tools.Result) bool {
	var refused *builtin.DecisionRefused
	return got.Failed && tools.RefusalOf(got) == tools.RefusalForbidden &&
		strings.Contains(got.Output, "working for a credential acting as no seat") &&
		errors.As(got.Cause, &refused) && refused.Decision.Reason == authz.ReasonCeiling
}

// A TURN A CREDENTIAL WOKE READS NO FLEET HEALTH AND NO SERVER (ADR-0109).
//
// A token or a service account acting as no seat is somebody of the company
// for the apps its grants name alone, its reach stated at its pin, and the
// deployment's health and a server are no app. So in a turn a `[worklet]` CI
// token's task woke, and in one an `[admin]` token's woke — a credential's
// requester carries apps and never admin — fleet_health and code_status read
// nothing of the deployment, ssh_read reaches no server and ssh_operate asks
// nobody, each refused with the requester ceiling's reason before anything is
// read, as before a credential was a requester at all. A person's turn is
// still answered.
//
// Mutation: let a credential through companyRequester — the tokens' turns read
// the deployment and the server; through code_status's forCompany — the tokens'
// turns read every Gitlet node's cache.
func TestATurnACredentialWokeReadsNoFleetHealthAndNoServer(t *testing.T) {
	t.Parallel()
	credentials := []struct {
		name, login string
		apps        []iam.Grant
	}{
		{"a [worklet] token", "token:ci", []iam.Grant{iam.GrantWorklet}},
		{"an [admin] token", "token:ops", iam.AppsOf([]iam.Grant{iam.GrantAdmin})},
	}

	t.Run("fleet_health", func(t *testing.T) {
		t.Parallel()
		o, chart := healthChart(t)
		backend := coordmem.New()
		node(t, backend, "node-a", nil, &coord.NodeStatus{AlarmsEvaluated: true})
		leases := &countingLeases{LeaseLister: backend}
		tool := seatHealthTools(t, healthFleet(leases, chart), chart)[builtin.FleetHealthTool].(tools.SeatCallable)
		turnOf := func() *turnctx.Turn {
			return &turnctx.Turn{RunID: "run-1", WorkKey: "wk-1", Seat: o.AgentSeatByHandle("sre"), Org: o,
				Lookups: turnctx.NewLookups()}
		}
		for _, c := range credentials {
			got, err := tool.CallForTurn(asSeat("sre"), credentialTurn(turnOf(), c.login, c.apps), nil)
			if err != nil || !refusedForACredential(got) {
				t.Errorf("fleet_health for %s answered %+v (%v), want the ceiling's refusal", c.name, got, err)
			}
		}
		if n := leases.lists.Load(); n != 0 {
			t.Errorf("the presence leases were read %d times for a credential's turn", n)
		}
		person := turnOf()
		person.Requester, person.Senders = "founder", []types.CanonicalIdentity{{Handle: "founder"}}
		got, err := tool.CallForTurn(asSeat("sre"), person, nil)
		if err != nil || got.Failed {
			t.Errorf("fleet_health for a person of the company answered %+v (%v), want the view", got, err)
		}
	})

	t.Run("code_status", func(t *testing.T) {
		t.Parallel()
		o, chart := healthChart(t)
		asker := &statusAsker{report: builtin.CodeStatusReport{Statuses: []host.CodeStatus{{Node: "node-a",
			Hot: []host.RepoStatus{{RepoHeat: cache.RepoHeat{ID: siteID, Name: "site",
				Hour: cache.Use{Fetches: 1}, Day: cache.Use{Fetches: 1}}}}}}}}
		repos := &grantHost{repos: map[string]code.RepoRef{"site": {ID: siteID, Name: "site"}}}
		tool := seatHealthTools(t, builtin.HealthDeps{Status: asker, Code: healthCode(repos, chart)},
			chart)[builtin.CodeStatusTool].(tools.SeatCallable)
		turnOf := func() *turnctx.Turn {
			return &turnctx.Turn{RunID: "run-1", WorkKey: "wk-1", Seat: o.AgentSeatByHandle("sre"), Org: o,
				Lookups: turnctx.NewLookups()}
		}
		for _, c := range credentials {
			got, err := tool.CallForTurn(asSeat("sre"), credentialTurn(turnOf(), c.login, c.apps), nil)
			if err != nil || !refusedForACredential(got) {
				t.Errorf("code_status for %s answered %+v (%v), want the ceiling's refusal", c.name, got, err)
			}
		}
		asker.mu.Lock()
		asked := len(asker.asked)
		asker.mu.Unlock()
		if asked != 0 {
			t.Errorf("the Gitlet nodes were asked %d times for a credential's turn", asked)
		}
		person := turnOf()
		person.Requester, person.Senders = "founder", []types.CanonicalIdentity{{Handle: "founder"}}
		got, err := tool.CallForTurn(asSeat("sre"), person, nil)
		if err != nil || got.Failed {
			t.Errorf("code_status for a person of the company answered %+v (%v), want the view", got, err)
		}
	})

	t.Run("ssh", func(t *testing.T) {
		t.Parallel()
		cfg, o, chart := sshFixture(t)
		read := map[string]any{"host": "api-1", "command": "uptime"}
		operate := map[string]any{"host": "api-1", "command": "sudo systemctl restart api",
			"reason": "the API is stuck"}
		servers := &fakeServers{cfg: cfg, res: sshint.Result{Stdout: []byte("up 3 days\n"), Started: true}}
		byName := sshTools(t, servers, chart)
		for _, c := range credentials {
			turn := credentialTurn(sshTurn(o, "sre", "", nil), c.login, c.apps)
			got, err := byName[builtin.SSHReadTool].CallForTurn(asExecutor("sre"), turn, read)
			if err != nil || !refusedForACredential(got) {
				t.Errorf("ssh_read for %s answered %+v (%v), want the ceiling's refusal", c.name, got, err)
			}
			refusal := byName[builtin.SSHOperateTool].(beforeHold).BeforeHold(asSeat("sre"), turn, operate)
			if refusal == nil || !refusedForACredential(*refusal) {
				t.Errorf("ssh_operate for %s was put to a person (%+v), want refused before anybody is asked",
					c.name, refusal)
			}
		}
		if len(servers.reads) != 0 {
			t.Errorf("a server was reached %d times for a credential's turn", len(servers.reads))
		}
		got, err := byName[builtin.SSHReadTool].CallForTurn(asExecutor("sre"),
			sshTurn(o, "sre", "founder", []types.CanonicalIdentity{{Handle: "founder"}}), read)
		if err != nil || got.Failed || len(servers.reads) != 1 {
			t.Errorf("ssh_read for a person of the company answered %+v (%v), want the server read once", got, err)
		}
	})
}
