package livestate

import (
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
)

// ONE SEAT-STATE VOCABULARY, and the engine is what speaks it.
//
// A seat's state used to be three things at once, none of them whole. The
// projection sent a `state` word folded from the events alone (idle, working,
// afk, terminated); the dashboard's sidebar counted `working` off that word,
// its live screen folded the running-runs panel in on top of it, and the seat
// library folded it in a third way — so a seat parked on a coding run's
// question was "working" on one screen, "sandbox" on another and "needs you"
// on a third, and a run parked past the old twelve-hour age-out vanished from
// every ring at once. A seat a PEER held had no state at all, because the only
// placement this process could answer was its own, and the client drew the
// absence as offline: on a fleet, every seat another node ran read as down.
//
// So the answer is one word per seat, computed HERE, from every input that
// bears on it, and served on every `agents` row — the snapshot's, the REST
// roster's and every push — so no reader derives it and no two readers can
// disagree:
//
//	working — a turn is running on the seat (a parked turn's work is its
//	          run's), or a coding run it launched is launching or running;
//	needs   — a coding run it launched is waiting on a person
//	          (awaiting_clarification, or reseed: the box is gone and only the
//	          question survives);
//	stopped — the seat cannot take work, and `stopped_reason` says why;
//	idle    — none of the above: held somewhere, and waiting for work.
//
// IN THAT ORDER, and the order is the decision. A seat whose turn is still
// running is working even while a person's pause waits for the turn to end,
// or while an older run of its waits on a question: what it is doing now is
// the truer word, and `paused` and the running-runs panel say the rest. A
// FAILED LAST TURN IS NOT A STOP: it is `last_error`, which stays a separate
// flag, because a seat whose last turn failed is still taking work.
//
// THE RUNS ARE READ FROM THE DURABLE RECORD — the running-runs set, which the
// coordination store's run record is reconciled into every
// [ReconcileInterval] and which nothing ages out (sandbox.go) — and never from
// the turn's own stage. A parked turn says a run was launched; only the record
// says whether that run is still running or has stopped to ask somebody, and a
// run waiting on a question for thirteen hours is exactly the seat a person
// most needs to see.

// Activity is what a seat is doing, in the one vocabulary every surface reads.
//
// A NAMED STRING WITH Valid, the engine's enum idiom. The dashboard's copy is
// `SeatActivity` in `contract/wire.ts`, held to [Activities] by
// TestTheDashboardKnowsExactlyTheSeatStatesTheEngineSends.
type Activity string

const (
	// ActivityWorking — a turn is running on the seat, or a coding run it
	// launched is launching or running.
	ActivityWorking Activity = "working"
	// ActivityNeeds — a coding run the seat launched is waiting on a
	// person.
	ActivityNeeds Activity = "needs"
	// ActivityStopped — the seat cannot take work; [StoppedReason] says
	// why.
	ActivityStopped Activity = "stopped"
	// ActivityIdle — the seat is held and waiting for work.
	ActivityIdle Activity = "idle"
)

// Activities is the closed set, in precedence order.
var Activities = []Activity{ActivityWorking, ActivityNeeds, ActivityStopped, ActivityIdle}

// Valid reports whether a is an activity this build knows.
func (a Activity) Valid() bool { return slices.Contains(Activities, a) }

// StoppedReason is why a stopped seat cannot take work.
//
// The dashboard's copy is `StoppedReason` in `contract/wire.ts`, held to
// [StoppedReasons] by the same gate as [Activity].
type StoppedReason string

const (
	// StoppedPaused — a person paused the seat (coord.SeatPause). Its mail
	// waits on its inbox until somebody resumes it.
	StoppedPaused StoppedReason = "paused"
	// StoppedUnplaced — no node in the fleet holds the seat, so nothing
	// will run it however much mail it has. Read from the seat leases
	// (coord.ListLive of the seat class), never from which seats THIS node
	// runs: a seat a peer holds is placed.
	StoppedUnplaced StoppedReason = "unplaced"
	// StoppedBudget — a capped token window the seat is charged against,
	// its own or the company's, is refusing: the engine has parked the
	// seat until the window resets or the ceiling is raised.
	StoppedBudget StoppedReason = "budget"
	// StoppedProvider — the seat's model provider was unreachable
	// (`llm_unavailable`), and the seat has done no work since.
	StoppedProvider StoppedReason = "provider"
)

// StoppedReasons is the closed set, in precedence order: when more than one
// holds, the first is the one stated. A PAUSE FIRST, because it is a person's
// decision and nothing else moves until they reverse it; then PLACEMENT,
// because a seat no node holds runs nothing whatever its budget; then the
// BUDGET park, which lifts on its own at the window's end; then the PROVIDER,
// which lifts the moment the seat works again.
var StoppedReasons = []StoppedReason{StoppedPaused, StoppedUnplaced, StoppedBudget, StoppedProvider}

// Valid reports whether r is a reason this build knows.
func (r StoppedReason) Valid() bool { return slices.Contains(StoppedReasons, r) }

// seatState is one seat's word and, when it is stopped, why.
type seatState struct {
	activity Activity
	reason   StoppedReason
}

// stoppedReason renders the reason for the wire: null unless the seat is
// stopped, ALWAYS present, because a merged overlay with the key omitted would
// leave a resumed seat wearing its old reason.
func (st seatState) stoppedReason() *StoppedReason {
	if st.activity != ActivityStopped {
		return nil
	}
	r := st.reason
	return &r
}

// providerFailure is the engine-detected failure that stops a seat on its
// provider, as the failure hold records it.
const providerFailure = "llm_unavailable"

// stateOf computes one seat's state from everything the projection holds about
// it. `agent` may be nil: a seat the events never mentioned still has a
// placement, runs, a company budget and therefore a state.
//
// Called under s.mu.
func (s *LiveState) stateOf(role string, agent *agentLive) seatState {
	if agent != nil && agent.turn != nil && agent.turn.Stage != StageParked {
		return seatState{activity: ActivityWorking}
	}
	running, waiting := s.runsOf(role)
	// A PARKED TURN IS NOT WORK OF ITS OWN: its run is, and the run's
	// record says whether that run is running, waiting on somebody or gone.
	// Reading the park as working would keep a seat busy for good over a
	// run whose loss no event reported — the turn is ended only by events,
	// and the record that stopped listing the run is the one thing that
	// noticed.
	switch {
	case running:
		return seatState{activity: ActivityWorking}
	case waiting:
		return seatState{activity: ActivityNeeds}
	}
	stopped := func(r StoppedReason) seatState {
		return seatState{activity: ActivityStopped, reason: r}
	}
	switch {
	case agent != nil && agent.paused != nil:
		return stopped(StoppedPaused)
	case s.unplaced(role):
		return stopped(StoppedUnplaced)
	case agent != nil && agent.budget != nil && refusing(agent.budget.Windows, s.clock()),
		refusing(s.budget.Org.Windows, s.clock()):
		return stopped(StoppedBudget)
	case agent != nil && agent.failure == providerFailure:
		return stopped(StoppedProvider)
	}
	return seatState{activity: ActivityIdle}
}

// unplaced reports whether the last placement read LISTED the seat and found no
// node holding it. A seat the read did not list — before any read, or one the
// company no longer has — is not claimed either way: "no node holds it" is a
// fact about a seat the lease table was asked about.
func (s *LiveState) unplaced(role string) bool {
	held, listed := s.placement[role]
	return listed && !held
}

// runsOf reports whether the seat has a coding run in flight and whether it has
// one waiting on a person, from the running-runs set.
func (s *LiveState) runsOf(role string) (running, waiting bool) {
	if role == "" {
		return false, false
	}
	for _, held := range s.sandboxes {
		if held.entry.Role != role {
			continue
		}
		switch held.entry.Status {
		case SandboxLaunching, SandboxRunning:
			running = true
		case SandboxAwaiting, SandboxReseed:
			waiting = true
		}
	}
	return running, waiting
}

// refusing reports whether any window is refusing NOW.
//
// THE ENGINE'S OWN JUDGEMENT, bounded by the window it judged: a frame read
// `refusing` of the window it was in, and once that window's end has passed
// the park it described has lifted whether or not the next frame has arrived
// yet. Without the bound a day window refused at 23:50 would hold the seat
// stopped past midnight until the next report, which is the same instant the
// park itself released it.
func refusing(windows []WindowMeter, now time.Time) bool {
	for _, w := range windows {
		if w.State != types.BudgetRefusing {
			continue
		}
		if ends, err := time.Parse(time.RFC3339Nano, w.ResetsAt); err == nil && !now.Before(ends) {
			continue
		}
		return true
	}
	return false
}

// knownRoles is every role the projection can state a seat for: the company's
// agent seats as the last placement named them, every seat an event named, and
// every seat a run in flight belongs to.
//
// Called under s.mu.
func (s *LiveState) knownRoles() map[string]struct{} {
	out := make(map[string]struct{}, len(s.agents)+len(s.placement))
	for role := range s.placement {
		out[role] = struct{}{}
	}
	for role := range s.agents {
		out[role] = struct{}{}
	}
	for _, held := range s.sandboxes {
		if held.entry.Role != "" {
			out[held.entry.Role] = struct{}{}
		}
	}
	return out
}

// states reads every known seat's state, so a change that can move many of them
// at once — a run, a placement, the company's budget — can say which it moved.
//
// Called under s.mu.
func (s *LiveState) states() map[string]seatState {
	roles := s.knownRoles()
	out := make(map[string]seatState, len(roles))
	for role := range roles {
		out[role] = s.stateOf(role, s.agents[role])
	}
	return out
}

// noteMoved marks every seat whose state differs from `before` as moved.
//
// ONLY A SEAT THE COMPANY HAS, or one the events already gave an entry: a run
// or a meter can name a role the last placement read did not list — a seat a
// revision removed — and minting an entry for it would push a row the client
// appends as a new card. A seat of the company's that has no entry yet is given
// one, so its row is pushed rather than skipped.
//
// Called under s.mu.
func (s *LiveState) noteMoved(before map[string]seatState, change *Change) {
	for role, now := range s.states() {
		if was, ok := before[role]; ok && was == now {
			continue
		}
		if _, ours := s.placement[role]; !ours && s.agents[role] == nil {
			continue
		}
		s.ensureAgent(role)
		change.agentMoved(role)
	}
}

// SetPlacement lands a read of which of the company's agent seats some node in
// the fleet holds, keyed by role: every agent seat in the company, true where a
// node holds it. It replaces the previous read whole — a seat the company no
// longer has is no longer a seat anything is placed for — and reports the seats
// whose state it moved.
//
// A READ THAT FAILED IS NOT PASSED HERE. Until the first read lands the
// projection claims no seat is unplaced: "no node holds it" is a fact about the
// lease table, and a lease table nobody has read says nothing either way.
func (s *LiveState) SetPlacement(placed map[string]bool) Change {
	s.mu.Lock()
	defer s.mu.Unlock()
	before := s.states()
	next := make(map[string]bool, len(placed))
	for role, held := range placed {
		if role != "" {
			next[role] = held
		}
	}
	s.placement = next
	var change Change
	s.noteMoved(before, &change)
	return change
}
