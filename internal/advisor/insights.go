package advisor

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/worklet"
)

// WHAT A SUPERVISOR READS OFF THE WEEK'S NUMBERS (ADR-0166), each one finding
// per cause with the seats inside it, its headline carrying the number:
// a kind of wake whose turns change nothing, seats whose turns cost several
// times the company's, and a seat whose schedules keep it busy while its own
// work waits to start.

// The classes.
const (
	// ClassWakeReasonWaste is the owner's: a kind of wake most of whose turns
	// changed nothing, across the seats it woke.
	ClassWakeReasonWaste Class = "wake.reason_waste"
	// ClassTurnCostOutlier is the owner's: seats whose turns cost several
	// times the company's median.
	ClassTurnCostOutlier Class = "cost.turn_outlier"
	// ClassScheduleLoad is the owner's: a seat whose schedules fire many
	// times a day while the work it holds waits to start.
	ClassScheduleLoad Class = "schedule.load"
)

// The bars each class clears, so a few turns or one busy morning are not one.
const (
	// minReasonTurns and reasonWastePercent: a wake reason that started 50
	// turns in the week, half of them changing nothing.
	minReasonTurns     = 50
	reasonWastePercent = 50
	// minCostTurns and costOutlierTimes: a seat with 10 turns in the week
	// whose tokens per turn are three times the company's median or more.
	minCostTurns     = 10
	costOutlierTimes = 3
	// minFiresADay and minDueHours: schedules firing 24 times a day or more
	// while the seat's work waited to start 24 item-hours over the week.
	minFiresADay = 24
	minDueHours  = 24 * time.Hour
)

// wakeReasonWaste is one finding per kind of wake that started [minReasonTurns]
// or more turns in the week, [reasonWastePercent] or more of them changing
// nothing, the seats it woke inside with each one's wasted turns.
func wakeReasonWaste(c *config.Company, ev Evidence) []Finding {
	type tally struct{ turns, nothing int64 }
	byReason := map[string]*tally{}
	bySeat := map[string]map[string]int64{}
	for _, r := range ev.Ledger {
		if types.WakeDisposition(r.Disposition) != types.DispositionStarted || r.Wake == "" ||
			strings.HasPrefix(r.Wake, scheduleWake) {
			continue
		}
		reason := wakeReason(r.Wake)
		t := byReason[reason]
		if t == nil {
			t = &tally{}
			byReason[reason] = t
		}
		t.turns += r.Turns
		if types.TurnOutcome(r.Outcome) == types.OutcomeNothing {
			t.nothing += r.Turns
			if bySeat[reason] == nil {
				bySeat[reason] = map[string]int64{}
			}
			bySeat[reason][cmp.Or(r.Handle, r.AgentID)] += r.Turns
		}
	}
	var out []Finding
	for reason, t := range byReason {
		if t.turns < minReasonTurns || t.nothing*100 < reasonWastePercent*t.turns {
			continue
		}
		label := WakeLabel(reason)
		f := stallFinding(ClassWakeReasonWaste, AreaWork, Subject{Kind: SubjectWake, Name: reason},
			fmt.Sprintf("%s wakes: %d of %d turns changed nothing", label, t.nothing, t.turns),
			[]Fact{{Key: "turns", Value: t.turns}, {Key: "nothing", Value: t.nothing}}, nil, Fix{Kind: FixAskCrewlet, Options: []Option{},
				Sentence: fmt.Sprintf("Find out why %s wakes change nothing, and route them so they reach a seat "+
					"only when it has something to do.", strings.ToLower(label)),
				Prompt: fmt.Sprintf("%s wakes started %d turns in the last week and %d of them changed nothing. "+
					"Which seats did they wake, what woke them, and what routing or rule change would stop the ones "+
					"that change nothing?", label, t.turns, t.nothing)})
		f.Headline = fmt.Sprintf("%s wakes changed nothing in %d of %d turns (%d%%) this week.", label,
			t.nothing, t.turns, percent(t.nothing, t.turns))
		f.Seats = seatShares(c, bySeat[reason], UnitTurns)
		out = append(out, f)
	}
	return out
}

// wakeReason is a wake as one reason: "chat:message:abc" is "chat:message".
func wakeReason(wake string) string {
	parts := strings.SplitN(wake, ":", 3)
	if len(parts) > 2 {
		return parts[0] + ":" + parts[1]
	}
	return wake
}

// percent is part of whole, in whole percent.
func percent(part, whole int64) int64 {
	if whole == 0 {
		return 0
	}
	return (part*100 + whole/2) / whole
}

// seatShares is each seat's number, named as c names it, the most first.
func seatShares(c *config.Company, by map[string]int64, unit Unit) []SeatShare {
	out := make([]SeatShare, 0, len(by))
	for seat, n := range by {
		name := seat
		if role, _ := roleOf(c, seat); role != nil && role.Name != "" {
			name = role.Name
		}
		out = append(out, SeatShare{Seat: seat, Name: name, Value: float64(n), Unit: unit})
	}
	slices.SortFunc(out, func(a, b SeatShare) int { return cmp.Or(cmp.Compare(b.Value, a.Value), cmp.Compare(a.Seat, b.Seat)) })
	return out
}

// turnCostOutlier is one finding naming every seat whose turns cost
// [costOutlierTimes] the company's median tokens per turn or more over the
// week, each with its tokens per turn.
func turnCostOutlier(c *config.Company, ev Evidence) []Finding {
	seats := seatsOf(ev.Ledger)
	var per []int64
	for _, s := range seats {
		if s.started >= minCostTurns {
			per = append(per, s.tokens/s.started)
		}
	}
	if len(per) < 3 {
		return nil
	}
	slices.Sort(per)
	median := per[len(per)/2]
	if median == 0 {
		return nil
	}
	outliers := map[string]int64{}
	var worst *seatTurns
	for _, s := range seats {
		if s.started < minCostTurns || s.tokens/s.started < costOutlierTimes*median {
			continue
		}
		outliers[s.handle] = s.tokens / s.started
		if worst == nil || s.tokens/s.started > worst.tokens/worst.started {
			worst = s
		}
	}
	if worst == nil {
		return nil
	}
	name := worst.handle
	if role, _ := roleOf(c, worst.handle); role != nil && role.Name != "" {
		name = role.Name
	}
	f := stallFinding(ClassTurnCostOutlier, AreaWork, Subject{Kind: SubjectCompany},
		fmt.Sprintf("%s at %s tokens a turn against the company's %s", name,
			groupedInt(worst.tokens/worst.started), groupedInt(median)),
		[]Fact{{Key: "tokens_per_turn", Value: worst.tokens / worst.started}, {Key: "median_per_turn", Value: median},
			{Key: "seats", Value: int64(len(outliers))}}, nil,
		Fix{Kind: FixOpenPage, Page: PageSeat, Target: worst.handle, Options: []Option{},
			Sentence: "Open the seat's turns: a long context, a large tool answer read again and again, or a run " +
				"that loops costs a turn many times the others'."})
	f.Headline = fmt.Sprintf("%s spends %s tokens a turn, %d times the company's median of %s.", name,
		groupedInt(worst.tokens/worst.started), worst.tokens/worst.started/median, groupedInt(median))
	f.Seats = seatShares(c, outliers, UnitTokens)
	return []Finding{f}
}

// scheduleLoad is one finding per agent seat whose schedules fired
// [minFiresADay] or more times a day over the week while the work it holds
// waited [minDueHours] or more to start.
func scheduleLoad(c *config.Company, ev Evidence) []Finding {
	if ev.FlowUnread {
		return nil
	}
	days := Window.Hours() / 24
	fires := map[string]map[string]int{}
	for _, f := range ev.Fires {
		if f.Seat == "" {
			continue
		}
		if fires[f.Seat] == nil {
			fires[f.Seat] = map[string]int{}
		}
		fires[f.Seat][f.Name]++
	}
	due := map[string]time.Duration{}
	items := map[string]int{}
	for _, h := range ev.Flow {
		if h.State == worklet.FlowDue {
			due[h.Seat] += h.Stood
			items[h.Seat] += h.Items
		}
	}
	var out []Finding
	for _, role := range agentSeats(c) {
		handle := role.Seat().Handle()
		total := 0
		for _, n := range fires[handle] {
			total += n
		}
		aDay := int64(float64(total)/days + 0.5)
		if aDay < minFiresADay || due[handle] < minDueHours {
			continue
		}
		f := stallFinding(ClassScheduleLoad, AreaSchedules, Subject{Kind: SubjectSeat, Seat: handle, Name: role.Name},
			fmt.Sprintf("%d schedule runs a day; %s item-hours waited to start", aDay, hoursSaid(due[handle])),
			[]Fact{{Key: "fires_a_day", Value: aDay}, {Key: "schedules", Value: int64(len(fires[handle]))},
				{Key: "due_minutes", Value: int64(due[handle] / time.Minute)}, {Key: "items", Value: int64(items[handle])}},
			nil, Fix{Kind: FixOpenPage, Page: PageSeat, Target: handle, Options: []Option{},
				Sentence: "Open the seat: run its checks less often, batch what they start, or move work to a seat " +
					"with room."})
		f.Headline = fmt.Sprintf("%s's %s fired %d times a day while %s of its work waited %s to start.",
			role.Name, countSaid(len(fires[handle]), "schedule", "schedules"), aDay,
			countSaid(items[handle], "item", "items"), hoursSaid(due[handle]))
		out = append(out, f)
	}
	return out
}

// SubjectWake is a kind of wake, by its reason in Name.
const SubjectWake SubjectKind = "wake"
