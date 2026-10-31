package engine

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
)

// refExpiryDoc is a company hosting web, owned by the Engineering unit, whose
// one seat writes code at full_access — so its own namespace is its to push.
func refExpiryDoc() string {
	return nativeCleanupCompany + `units:
  - name: Engineering
    project: ENG
    roles:
      - name: SWE
        handle: swe
        llm: zulu
        code: {access: full_access}
code:
  native: true
  repos:
` + webRepo("")
}

// THE CODE-RETENTION DUTY EXPIRES A SEAT'S IDLE BRANCH THROUGH THE ROUTER, AND
// KEEPS ONE A PARKED RUN'S ROW HOLDS — the ref lifecycle as a node wires it:
// the duty's pass proposes each deletion to the repository's committer through
// the estate router (code.Retention.Propose) under the core's node id
// (code.Retention.Node), and asks the engine's own reader of the live turns'
// holds — every run row whatever its status — which branches to keep
// ([Engine.codeHolds]).
//
// Mutations: build the duty with no Propose or no Node → the pass refuses to
// run, red; hand it a Holds that answers nothing → the held branch is
// expired, red.
func TestTheRetentionDutyExpiresAnIdleSeatBranchNoRunHolds(t *testing.T) {
	t.Parallel()
	e := newSandboxNode(t, parseCompany(t, refExpiryDoc()))
	servesCodeReads(t, e)
	reconciled(t, e)
	web := mustRepo(t, e, "web")
	const (
		idle = "refs/heads/seats/swe/idle"
		held = "refs/heads/seats/swe/held"
	)
	// THE SEAT'S TWO BRANCHES, each an engine-built write of its turn.
	for i, ref := range []string{idle, held} {
		push := code.Proposal{PushID: uuid.Must(uuid.NewV7()).String(), RepoID: web.ID,
			Principal: code.Principal{Kind: code.PrincipalAgent, Seat: "swe",
				Run: code.RunProvenance{TurnID: "turn-1"}},
			Via: code.ViaEngine, Node: e.id, ReceivedAt: time.Now().UTC(),
			Commands: []code.Command{{Ref: ref, Old: code.ZeroOID, New: oidAt(i)}}}
		res, err := e.router.Code().Propose(t.Context(), push)
		if err != nil || res.Outcome != statelog.OutcomeApplied || res.Answer.Commands[0].Result != code.ResultOK {
			t.Fatalf("the seat's write of %s = %+v, %v", ref, res, err)
		}
	}
	// A PARKED RUN'S ROW HOLDS ONE OF THEM.
	row := sandbox.PendingRun{TurnID: "run-parked", AgentHandle: "elsewhere", Status: sandbox.StatusAwaiting,
		CodeHolds: []string{code.RefHoldKey(web.ID, held)}}
	raw, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	if created, err := e.backends.Fleet.CreateSandboxRun(t.Context(), row.TurnID, raw); err != nil || !created {
		t.Fatalf("seed the parked run's row: %v %v", created, err)
	}

	d := e.newCodeRetentionDuty(e.writeSide().code, e.native.Load().codeReader, e.core.Load().nodeID)
	if d.pass.Node == "" || d.pass.Propose == nil {
		t.Fatalf("the duty's pass names node %q and proposes through %v", d.pass.Node, d.pass.Propose != nil)
	}
	// A FORTNIGHT AND AN HOUR ON, by the pass's clock alone: the branches'
	// broker instants cannot be moved.
	d.pass.Now = func() time.Time { return time.Now().Add(code.RefExpiry + time.Hour) }
	report, err := d.pass.Pass(t.Context())
	if err != nil || report.RefsExpired != 1 || report.RefsHeld != 1 || len(report.ExpiryFailed) != 0 {
		t.Fatalf("the pass = %+v, %v; want one branch expired and the held one kept", report, err)
	}
	answer, err := e.router.Code().ResolveRef(t.Context(), code.RefQuery{Repo: "web", Refs: []string{idle, held},
		Fresh: statelog.Freshness{Level: statelog.ReadLinearizable},
		Asker: code.Principal{Kind: code.PrincipalOperator, OperatorID: "op-1", Seat: "ceo"}})
	if err != nil || len(answer.Refs) != 2 {
		t.Fatalf("resolve the seat's branches: %+v, %v", answer, err)
	}
	if answer.Refs[0].Missing == "" || answer.Refs[1].Missing != "" {
		t.Errorf("after the pass %s resolves %+v and %s %+v, want the idle one gone and the held one kept", idle,
			answer.Refs[0], held, answer.Refs[1])
	}
}

// oidAt is a fixture commit id: 40 hex digits, never the zero id.
func oidAt(i int) string { return fmt.Sprintf("%040x", i+1) }
