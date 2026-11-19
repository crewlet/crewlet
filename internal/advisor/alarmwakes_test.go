package advisor_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/usage"
)

// watched is the stall shape with Dev and QA both holding the health grant,
// so an alarm wakes each of them.
func watched(t *testing.T) string {
	t.Helper()
	doc := replace(t, stallShape, "  - name: Dev\n    handle: dev\n    llm: p\n",
		"  - name: Dev\n    handle: dev\n    llm: p\n    health_tools: true\n")
	return replace(t, doc, "  - name: QA\n    handle: qa\n    llm: p\n",
		"  - name: QA\n    handle: qa\n    llm: p\n    health_tools: true\n")
}

// alarmTurns is a seat's started turns an alarm woke with outcome, at 1,000 tokens each.
func alarmTurns(seat, outcome string, turns int64) usage.TurnLedgerRow {
	r := started(seat, "alarm:fired", outcome, turns)
	r.Input = 1_000 * turns
	return r
}

// AN AGENT WOKEN ON ALARMS ANOTHER AGENT ANSWERS IS OFFERED THE GRANT OFF:
// ten alarm turns a week, half of them changing nothing, on a seat that is not
// the one whose alarm turns changed the most. The keeper is never raised, nor
// a company's only watcher. Mutation: count a turn another wake started, drop
// the keeper, or raise a seat whose grant is already off, and a row fails.
func TestAnAgentWokenOnAlarmsAnotherAnswersIsOfferedTheGrantOff(t *testing.T) {
	t.Parallel()
	doc := watched(t)
	noisy := []usage.TurnLedgerRow{
		alarmTurns("dev", "moved", 8), alarmTurns("dev", "nothing", 4),
		alarmTurns("qa", "nothing", 5), started("qa", "alarm:cleared", "nothing", 2), alarmTurns("qa", "words", 3),
		started("qa", "chat:message", "nothing", 40), absorbed("qa", 30),
	}
	found := stalls(t, doc, advisor.Evidence{Ledger: noisy}, advisor.ClassAlarmNoop)
	if len(found) != 1 || found[0].Subject.Seat != "qa" {
		t.Fatalf("raised %+v, want QA alone, Dev keeping the watch", found)
	}
	qa := found[0]
	if qa.Item != "7 of 10 alarm turns changed nothing, 8K tokens" || qa.Severity != advisor.SeverityCosts ||
		qa.Actor != advisor.ActorOwner || qa.Path != "roles[3].health_tools" {
		t.Errorf("qa = %+v", qa)
	}
	if fix := qa.Fix; fix.Kind != advisor.FixConfigChange || len(fix.Options) != 1 ||
		fix.Options[0].Change.ID != "qa" || fix.Options[0].Change.Set["health_tools"] != false ||
		fix.Unchanged != "Dev keeps watching the engine's health and wakes on its alarms." {
		t.Errorf("qa's fix = %+v, want its grant off with Dev keeping it", fix)
	}
	if err := advisor.CheckChange(company(t, doc), qa.Fix.Options[0].Change); err != nil {
		t.Errorf("the grant off does not build: %v", err)
	}
	for name, c := range map[string]struct {
		doc    string
		ledger []usage.TurnLedgerRow
	}{
		"9 alarm turns": {doc, append([]usage.TurnLedgerRow{alarmTurns("dev", "moved", 8)},
			alarmTurns("qa", "nothing", 5), alarmTurns("qa", "words", 4))},
		"4 of 10 changed nothing": {doc, append([]usage.TurnLedgerRow{alarmTurns("dev", "moved", 8)},
			alarmTurns("qa", "nothing", 4), alarmTurns("qa", "words", 6))},
		"QA's grant is off": {replace(t, doc, "    handle: qa\n    llm: p\n    health_tools: true\n",
			"    handle: qa\n    llm: p\n"), noisy},
		"QA keeps the watch": {doc, []usage.TurnLedgerRow{alarmTurns("qa", "nothing", 10), alarmTurns("qa", "moved", 1)}},
		"QA is the only watcher": {replace(t, doc, "    handle: dev\n    llm: p\n    health_tools: true\n",
			"    handle: dev\n    llm: p\n"), noisy},
	} {
		if got := stalls(t, c.doc, advisor.Evidence{Ledger: c.ledger}, advisor.ClassAlarmNoop); len(got) != 0 {
			t.Errorf("%s: raised %+v", name, got)
		}
	}
	if got := stalls(t, doc, advisor.Evidence{Ledger: noisy, LedgerUnread: true}, advisor.ClassAlarmNoop); len(got) != 0 {
		t.Errorf("an unread ledger raised %+v", got)
	}
}

// A CHANGE THAT TURNS A FIELD OFF HOLDS ONCE THE ENTITY LEAVES IT OUT: a role
// answers no health grant by omitting it, so a grant taken off is in the
// configuration then. Mutation: compare the change's own fields, and a role
// with the grant off never holds it.
func TestAGrantTurnedOffHoldsOnceTheRoleOmitsIt(t *testing.T) {
	t.Parallel()
	off := advisor.Change{Entity: advisor.EntityRoles, ID: "qa", Set: map[string]any{"health_tools": false}}
	if holds, err := advisor.Holds(&config.Role{Name: "QA", Handle: "qa"}, off); err != nil || !holds {
		t.Errorf("a role without the grant does not hold it off: %v (%v)", holds, err)
	}
	if holds, err := advisor.Holds(&config.Role{Name: "QA", Handle: "qa", HealthTools: true}, off); err != nil || holds {
		t.Errorf("a role holding the grant holds it off: %v (%v)", holds, err)
	}
}
