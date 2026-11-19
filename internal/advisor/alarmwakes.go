package advisor

import (
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/usage"
	"github.com/crewlet/crewlet/internal/words"
)

// ClassAlarmNoop is the owner's: an agent woken on the engine's alarms, most
// of whose alarm turns changed nothing.
const ClassAlarmNoop Class = "wake.alarm_noop"

// minAlarmTurns and alarmNoopPercent: ten alarm-woken turns a week, half of
// them committing nothing, is a seat paying for alarms another seat already
// answers, not one bad night.
const (
	minAlarmTurns    = 10
	alarmNoopPercent = 50
)

// alarmWake is the ledger's wake for a turn an alarm started, by itself or
// as the prefix of the change that woke it (fired, cleared).
const alarmWake = types.OriginAlarm

// alarmTurns is one seat's started turns an alarm woke, how many of them
// committed nothing, and what they spent.
type alarmTurns struct {
	turns, nothing, tokens int64
}

// changed is how many of the seat's alarm turns moved something.
func (a alarmTurns) changed() int64 { return a.turns - a.nothing }

// alarmTurnsOf folds the ledger's alarm-woken started turns by seat handle.
func alarmTurnsOf(ledger []usage.TurnLedgerRow) map[string]alarmTurns {
	out := map[string]alarmTurns{}
	for _, r := range ledger {
		if types.WakeDisposition(r.Disposition) != types.DispositionStarted ||
			(r.Wake != alarmWake && !strings.HasPrefix(r.Wake, alarmWake+":")) {
			continue
		}
		handle := r.Handle
		if handle == "" {
			handle = r.AgentID
		}
		a := out[handle]
		a.turns += r.Turns
		a.tokens += r.Tokens()
		if types.TurnOutcome(r.Outcome) == types.OutcomeNothing {
			a.nothing += r.Turns
		}
		out[handle] = a
	}
	return out
}

// healthSeats is every agent seat whose role holds the health grant now, in
// the chart's order: the seats an alarm wakes (alarmwake.ReasonHealthGrant).
func healthSeats(c *config.Company) []*config.Role {
	var out []*config.Role
	for _, role := range agentSeats(c) {
		if role.HealthTools {
			out = append(out, role)
		}
	}
	return out
}

// alarmNoop is one finding per agent seat holding the health grant whose
// alarm-woken turns reached [minAlarmTurns], [alarmNoopPercent] of them
// committing nothing, while another seat holding the grant keeps the watch.
// A seat whose grant was taken off since is fixed.
//
// ONE SEAT KEEPS THE WATCH: the grant holder whose alarm turns changed the
// most (then the fewest that changed nothing, then the chart's order). An
// alarm wakes every holder and one of them answering it is enough, so every
// other noisy holder is offered the grant off, one write of its own role. The
// keeper is never raised: a company whose only watcher stops watching hears
// of no alarm at all, and an alarm that fires too often is its own finding.
func alarmNoop(c *config.Company, ev Evidence) []Finding {
	granted := healthSeats(c)
	if len(granted) < 2 {
		return nil
	}
	tally := alarmTurnsOf(ev.Ledger)
	keeper := granted[0]
	for _, role := range granted[1:] {
		a, k := tally[role.Seat().Handle()], tally[keeper.Seat().Handle()]
		if a.changed() > k.changed() || (a.changed() == k.changed() && a.nothing < k.nothing) {
			keeper = role
		}
	}
	var out []Finding
	for _, role := range granted {
		handle := role.Seat().Handle()
		a := tally[handle]
		if role == keeper || a.turns < minAlarmTurns || a.nothing*100 < alarmNoopPercent*a.turns {
			continue
		}
		_, path := roleOf(c, handle)
		f := stallFinding(ClassAlarmNoop, AreaSetup, Subject{Kind: SubjectSeat, Seat: handle},
			fmt.Sprintf("%d of %s changed nothing, %s tokens", a.nothing,
				words.Count(int(a.turns), "alarm turn", "alarm turns"), tokensSaid(a.tokens)),
			[]Fact{{Key: "alarm_turns", Value: a.turns}, {Key: "nothing", Value: a.nothing},
				{Key: "tokens", Value: a.tokens}},
			nil, alarmNoopFix(role, keeper))
		f.Path = at(path, "health_tools")
		out = append(out, f)
	}
	return out
}

// alarmNoopFix takes the health grant off role, keeper keeping it.
func alarmNoopFix(role, keeper *config.Role) Fix {
	handle := role.Seat().Handle()
	return Fix{Kind: FixConfigChange,
		Sentence:  fmt.Sprintf("Leave the engine's alarms to %s, whose alarm turns changed the most.", keeper.Name),
		Unchanged: fmt.Sprintf("%s keeps watching the engine's health and wakes on its alarms.", keeper.Name),
		Options: []Option{{Label: fmt.Sprintf("Stop waking %s on the engine's alarms", role.Name),
			Change: Change{Entity: EntityRoles, ID: handle, Set: map[string]any{"health_tools": false},
				Summary: fmt.Sprintf("Stop waking %s on the engine's alarms", handle)}}}}
}
