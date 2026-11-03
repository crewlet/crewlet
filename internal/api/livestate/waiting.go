package livestate

import (
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
)

// WHAT WAITS IN A SEAT'S INBOX, folded from the reports of the nodes holding
// the seats ([types.SeatInboxWaiting]).
//
// PER REPORTER, NOT A WHOLE-FRAME SWEEP. A report covers only the seats its
// node holds, so a seat it does not list may be another node's: a seat a
// reporter listed before and lists no longer is cleared only if that reporter
// is still where its count came from. A seat that moved is taken over by its
// new holder's first report.
//
// SILENCE CLEARS. A reporter publishes at least every
// [types.SeatInboxWaitingHeartbeat]; one not heard from for [waitingExpiry]
// has gone away, and its counts are forgotten ([LiveState.ExpireWaiting]) so
// Live never shows a count nobody is refreshing. Heard is the projection's
// own clock at receipt, never the reporter's, so clock skew between nodes
// cannot keep a count alive or end one early.

// waitingExpiry is how long a reporter's counts are held without a report:
// three heartbeats, so one late or lost report never blanks a seat.
const waitingExpiry = 3 * types.SeatInboxWaitingHeartbeat

// Waiting is what waits in a seat's inbox that no turn is running on yet, per
// mailbox: Priority is a person waiting (the priority lane), Background the
// rest.
type Waiting struct {
	Priority   int `json:"priority"`
	Background int `json:"background"`
}

func (w *Waiting) clone() *Waiting {
	if w == nil {
		return nil
	}
	c := *w
	return &c
}

// waitingReporter is one reporting incarnation as the projection last heard
// it.
type waitingReporter struct {
	seq   int
	heard time.Time
	seats map[string]struct{}
}

// applyWaiting folds one reporter's snapshot.
func (s *LiveState) applyWaiting(payload map[string]any) (change Change) {
	var report types.SeatInboxWaiting
	if !decodePayload(payload, &report) || report.ReporterID == "" {
		return change
	}
	rep := s.reporters[report.ReporterID]
	if rep != nil && report.Seq <= rep.seq {
		// AN OLDER REPORT ARRIVING LATE: the broadcast reads across topics,
		// so order holds only within one reporter's sequence.
		return change
	}
	listed := make(map[string]struct{}, len(report.Seats))
	for _, seat := range report.Seats {
		if seat.AgentID == "" {
			continue
		}
		listed[seat.AgentID] = struct{}{}
		agent := s.ensureAgent(seat.AgentID)
		next := &Waiting{Priority: seat.Priority, Background: seat.Background}
		if agent.waiting == nil || *agent.waiting != *next || agent.waitingFrom != report.ReporterID {
			change.agentMoved(seat.AgentID)
		}
		agent.waiting, agent.waitingFrom = next, report.ReporterID
	}
	if rep != nil {
		for id := range rep.seats {
			if _, still := listed[id]; !still {
				s.clearWaiting(id, report.ReporterID, &change)
			}
		}
	}
	if s.reporters == nil {
		s.reporters = map[string]*waitingReporter{}
	}
	s.reporters[report.ReporterID] = &waitingReporter{
		seq: report.Seq, heard: s.clock(), seats: listed}
	return change
}

// ExpireWaiting forgets the counts of every reporter not heard from for
// [waitingExpiry], and reports the seats that moved. Called on the stream's
// own tick, beside the placement read, because a reporter that went away
// publishes nothing that could clear its counts.
func (s *LiveState) ExpireWaiting() Change {
	s.mu.Lock()
	defer s.mu.Unlock()
	var change Change
	now := s.clock()
	for id, rep := range s.reporters {
		if now.Sub(rep.heard) <= waitingExpiry {
			continue
		}
		for seat := range rep.seats {
			s.clearWaiting(seat, id, &change)
		}
		delete(s.reporters, id)
	}
	return change
}

// clearWaiting drops a seat's count if reporter is where it came from.
func (s *LiveState) clearWaiting(agentID, reporter string, change *Change) {
	agent := s.agents[agentID]
	if agent == nil || agent.waiting == nil || agent.waitingFrom != reporter {
		return
	}
	agent.waiting, agent.waitingFrom = nil, ""
	change.agentMoved(agentID)
}
