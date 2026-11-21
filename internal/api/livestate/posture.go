package livestate

import (
	"cmp"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/worklet"
)

// THE SEATS' POSTURE, as an item's flow reads it (ADR-0162): which Worklet
// items each seat's turn and runs are on, the item a run of it waits on a
// person over, and why the seat cannot take a turn. The stop is [stopOf], the
// one ranking the seat's own state reads, so an item never says a seat is held
// for a reason its seat's row does not.
//
// LIVE ONLY ONCE EVERY RECORD IT READS HAS LANDED: the placement read, the
// pause record, the durable run record and a budget report. Before then a
// stopped seat would read as free, which is the answer `flow_live: false`
// exists to refuse. A provider park has no record to read, only its event,
// so a projection that started while a seat was parked learns it at the
// engine's next park or probe, as the seat's row does.

// stopCauses is each stop reason as an item's flow names it.
var stopCauses = map[StoppedReason]worklet.FlowCause{
	StoppedPaused:   worklet.FlowCausePaused,
	StoppedUnplaced: worklet.FlowCauseUnplaced,
	StoppedBudget:   worklet.FlowCauseBudget,
	StoppedProvider: worklet.FlowCauseLimit,
}

// Posture answers the seats' posture now. seats names the company's agent
// seats, agent id to handle: the flow keys a seat by the handle an item is
// assigned to, the projection by the id derived from it (ADR-0013). A seat
// with nothing to state is left out, which the flow reads as free.
func (s *LiveState) Posture(seats map[string]string) worklet.Posture {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.placement == nil || !s.pausesRead || !s.runsRead || s.budget == nil {
		return worklet.Posture{}
	}
	out := worklet.Posture{Live: true, Seats: map[string]worklet.SeatPosture{}}
	for id, handle := range seats {
		if handle == "" {
			continue
		}
		if seat, ok := s.postureOf(id); ok {
			out.Seats[handle] = seat
		}
	}
	return out
}

// postureOf is one seat's posture, and whether it states anything.
//
// Called under s.mu.
func (s *LiveState) postureOf(agentID string) (worklet.SeatPosture, bool) {
	agent := s.agents[agentID]
	var out worklet.SeatPosture
	if turnWorks(agent) {
		if item := worklistItem(agent.turn.WorkItem); item != "" {
			out.On = append(out.On, worklet.PostureItem{Item: item,
				Cause: worklet.FlowCauseTurn, Since: instant(agent.turn.StartedAt)})
		}
	}
	// THE OLDEST RUN WAITING ON A PERSON is the one the seat is parked on:
	// a seat whose run works takes no other work, so a second is a
	// straggler the next reconcile settles.
	runs := s.runsHeldBy(agentID)
	for _, run := range runs {
		item := worklistItem(run.WorkItem)
		if item == "" {
			continue
		}
		switch run.Status {
		case ExecutorLaunching, ExecutorRunning, ExecutorAnswered:
			out.On = append(out.On, worklet.PostureItem{Item: item,
				Cause: worklet.FlowCauseRun, Since: instant(run.StartedAt)})
		case ExecutorAwaiting, ExecutorReseed:
			if out.Parked == "" {
				out.Parked, out.ParkedSince = item, instant(run.PausedAt)
			}
		}
	}
	if reason, stopped := s.stopOf(agentID, agent); stopped {
		out.Stop = stopCauses[reason]
		out.StopSince, out.StopUntil = s.stopBounds(reason, agent)
	}
	return out, len(out.On) > 0 || out.Parked != "" || out.Stop != ""
}

// stopBounds is when a stop began and when it lifts, where the projection
// knows: a pause from its instant and until a person resumes it, a model
// limit from its report until the vendor's reset, a budget until its last
// refusing window turns over. A seat no node holds says neither.
//
// Called under s.mu.
func (s *LiveState) stopBounds(reason StoppedReason, agent *agentLive) (since, until time.Time) {
	switch reason {
	case StoppedPaused:
		return instant(agent.paused.At), time.Time{}
	case StoppedProvider:
		limit := agent.providerLimit
		if limit.ResetsAt != nil {
			until = instant(*limit.ResetsAt)
		}
		return instant(limit.Since), until
	case StoppedBudget:
		var windows []WindowMeter
		if agent != nil && agent.budget != nil {
			windows = agent.budget.Windows
		}
		if s.budget != nil {
			windows = append(slices.Clone(windows), s.budget.Org.Windows...)
		}
		return time.Time{}, refusedUntil(windows, s.clock())
	}
	return time.Time{}, time.Time{}
}

// refusedUntil is when every window refusing now has turned over: the seat
// takes a turn again only once none refuses.
func refusedUntil(windows []WindowMeter, now time.Time) time.Time {
	var until time.Time
	for _, w := range windows {
		if !refusing([]WindowMeter{w}, now) {
			continue
		}
		if at := instant(w.ResetsAt); at.After(until) {
			until = at
		}
	}
	return until
}

// runsHeldBy is the seat's runs in flight, oldest first.
//
// Called under s.mu.
func (s *LiveState) runsHeldBy(agentID string) []ExecutorEntry {
	var out []ExecutorEntry
	for _, held := range s.executors {
		if held.entry.AgentID == agentID {
			out = append(out, held.entry)
		}
	}
	slices.SortFunc(out, func(a, b ExecutorEntry) int {
		return cmp.Or(newStamp(a.StartedAt).chronological(newStamp(b.StartedAt)),
			cmp.Compare(a.TurnID, b.TurnID))
	})
	return out
}

// worklistItem is a Worklet item's id, empty for an item of another tracker
// or none: the flow is read over Worklet's own items.
func worklistItem(item *types.WorkItem) string {
	if item == nil || item.Backend != types.WorkWorklet {
		return ""
	}
	return item.ID
}

// instant is a wire instant, zero where it is absent or does not parse.
func instant(raw string) time.Time {
	at := newStamp(raw)
	if !at.valid {
		return time.Time{}
	}
	return at.t
}
