package advisor_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/worklet"
)

// heldShape is a company whose agents run on p, with q on another account
// and r on p's own account and harness.
const heldShape = `name: Acme
providers:
  llm:
    p: {model: m, state_dir: /var/lib/acme/a}
    q: {model: m, state_dir: /var/lib/acme/b}
    r: {model: n, state_dir: /var/lib/acme/a}
  sandbox: {fake: true}
roles:
  - name: Owner
    handle: owner
    kind: human
    contact: {slack_user_id: U0OWNER}
  - name: Dev
    handle: dev
    llm: p
  - name: QA
    handle: qa
    llm: p
`

// flowOf is a seat's item-hours in one state and cause.
func flowOf(seat string, state worklet.FlowState, cause worklet.FlowCause, hours int) worklet.FlowHours {
	return worklet.FlowHours{Seat: seat, State: state, Cause: cause, Stood: time.Duration(hours) * time.Hour, Items: 1}
}

// AN AGENT WHOSE WORK WAITED ON ITS MODEL'S LIMIT IS OFFERED A FALLBACK ON
// ANOTHER ACCOUNT: two item-hours a day held on the limit, a quarter of the
// time its work stood wanting it or held; the fallback is an entry on another
// account or harness, never one sharing the limit. Mutation: count blocked
// hours as wanting the seat, or offer r, and a check fails.
func TestAnAgentWhoseWorkWaitedOnItsModelsLimitIsOfferedAFallback(t *testing.T) {
	t.Parallel()
	ev := advisor.Evidence{Flow: []worklet.FlowHours{
		flowOf("dev", worklet.FlowHeld, worklet.FlowCauseLimit, 20),
		flowOf("dev", worklet.FlowDue, "", 10),
		flowOf("dev", worklet.FlowBlocked, "", 500),
		flowOf("qa", worklet.FlowHeld, worklet.FlowCauseLimit, 20),
		flowOf("qa", worklet.FlowMoving, "", 70),
	}}
	found := stalls(t, heldShape, ev, advisor.ClassModelHeld)
	if len(found) != 1 || found[0].Subject.Seat != "dev" {
		t.Fatalf("raised %+v, want Dev alone (QA's limit is under a quarter of its work's time)", found)
	}
	dev := found[0]
	if dev.Item != "20h of 30h item-hours held on p's limit" || dev.Path != "roles[1].llm" || dev.Severity != advisor.SeverityCosts {
		t.Errorf("dev = %+v", dev)
	}
	if fix := dev.Fix; fix.Kind != advisor.FixConfigChange || len(fix.Options) != 1 ||
		fix.Options[0].Label != "Fall back to q" {
		t.Fatalf("dev's fix = %+v, want q alone (r shares p's account)", fix)
	}
	chain := dev.Fix.Options[0].Change.Merge["llm"].(map[string]any)["default"].([]string)
	if len(chain) != 2 || chain[0] != "p" || chain[1] != "q" {
		t.Errorf("the change writes %v, want p then q", chain)
	}
	if got := stalls(t, heldShape, advisor.Evidence{Flow: ev.Flow, FlowUnread: true}, advisor.ClassModelHeld); len(got) != 0 {
		t.Errorf("an unread flow raised %+v", got)
	}
}
