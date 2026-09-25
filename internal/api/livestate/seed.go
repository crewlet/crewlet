package livestate

import (
	"cmp"
	"maps"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/eventfan"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tokens"
)

// History is what a projection is seeded with when its process starts.
//
// Without it every surface built from the projection started EMPTY in every
// process: the activity feed, the spend rollup and the per-agent rows folded
// from it all described only what had happened since this process booted, so
// a restart, a deploy or a node joining a fleet showed an operator a company
// that had apparently done nothing, beside a store that said otherwise.
//
// EVERY HALF IS THE FLEET'S, read from every live node's event store
// (internal/eventfan): each node's store holds what that node published and
// nothing else, so a seed read from this node alone described a third of a
// three-node fleet, and a node that joined showed nothing its peers had done.
// Which nodes answered is [History.Coverage], reported as the seed's
// coverage rather than hidden.
type History struct {
	// Events are persisted feed rows, in any order.
	Events []FeedRow

	// Spend is the per-phase spend records inside [LiveSpendWindow], in any
	// order. Past [SpendRecordLimit] only the newest are kept.
	Spend []tokens.Record

	// Turns are each seat's newest turns, in any order: what a seat's last
	// turn is seeded from, and a turn it left PARKED on a detached coding
	// run, which is still its turn after the restart.
	Turns []store.Turn

	// Coverage is which nodes the reads above were answered by. Its
	// Complete is false when any read failed, since what that read would
	// have said is then missing whoever answered the others.
	Coverage eventfan.Coverage
}

// Seed folds stored history into the projection and reports what moved.
//
// SAFE AGAINST THE LIVE STREAM IN EITHER ORDER, which is what lets the caller
// subscribe FIRST and read the store second, so no event published between
// the two is lost. An event that arrives both ways is recognised by its id and
// listed and counted once, whichever way reached the projection first, and
// history lands behind the live rows it predates rather than after them. A
// turn the stream already moved is left as the stream left it.
func (s *LiveState) Seed(h History) Change {
	s.mu.Lock()
	defer s.mu.Unlock()
	var change Change
	change.Events = s.seedFeed(h.Events)
	change.Tokens = s.seedSpend(h.Spend)
	for _, role := range s.seedTurns(h.Turns) {
		change.agentMoved(role)
	}
	coverage := h.Coverage
	coverage.Nodes = slices.Clone(coverage.Nodes)
	s.seededFrom = &coverage
	return change
}

// SeededFrom is which nodes the startup seed read, and false before a seed
// ran. It is what says, beside screens that start from history, whether that
// history is the whole fleet's or is missing a node that did not answer.
func (s *LiveState) SeededFrom() (eventfan.Coverage, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seededFrom == nil {
		return eventfan.Coverage{}, false
	}
	c := *s.seededFrom
	c.Nodes = slices.Clone(c.Nodes)
	return c, true
}

// seedTurns sets each seat's last turn and parked turn from its stored turns,
// returning the roles it moved.
//
// Through the same guards the stream moves them under: a last turn replaces
// only an older one, and a parked turn lands only on a seat the stream has not
// already put on a turn, and never for a turn the stream has already seen end.
func (s *LiveState) seedTurns(turns []store.Turn) []string {
	byRole := map[string][]store.Turn{}
	for _, t := range turns {
		if t.AgentRole != "" && t.TurnID != "" {
			byRole[t.AgentRole] = append(byRole[t.AgentRole], t)
		}
	}
	var moved []string
	for _, role := range slices.Sorted(maps.Keys(byRole)) {
		list := byRole[role]
		slices.SortFunc(list, func(a, b store.Turn) int {
			return cmp.Or(b.StartedAt.Compare(a.StartedAt), cmp.Compare(b.TurnID, a.TurnID))
		})
		agent := s.ensureAgent(role)
		before := agent.overlay()
		for _, t := range list {
			if !t.Complete {
				continue
			}
			outcome := OutcomeCompleted
			if t.Failed {
				outcome = OutcomeFailed
			}
			ended := t.EndedAt.UTC().Format(time.RFC3339Nano)
			s.setLastTurn(agent, LastTurn{TurnID: t.TurnID, EndedAt: ended, Outcome: outcome},
				newStamp(ended))
			break
		}
		// THE NEWEST TURN, ONLY IF IT IS PARKED. A turn with no completion
		// at all is running or died mid-flight, and from the store those
		// look identical — so it is not claimed as either.
		if newest := list[0]; newest.Parked && agent.turn == nil {
			if _, ended := s.endedTurns.get(newest.TurnID); !ended {
				agent.turn = &LiveTurn{
					TurnID:    newest.TurnID,
					WorkItem:  cloneItem(newest.WorkItem),
					StartedAt: newest.StartedAt.UTC().Format(time.RFC3339Nano),
					Stage:     StageParked,
					failed:    newest.Failed,
				}
				agent.turnAt = newStamp(newest.EndedAt.UTC().Format(time.RFC3339Nano))
			}
		}
		if !sameTurnOverlay(before, agent.overlay()) {
			moved = append(moved, role)
		}
	}
	return moved
}

// seedFeed merges stored rows into the feed ring, reporting whether any landed.
//
// Through the same id index the live path lists by, which is what makes the
// overlap safe in BOTH orders: a row the stream delivered first is skipped
// here, and an envelope that arrives after its row was seeded is skipped by
// [LiveState.recordEvent].
func (s *LiveState) seedFeed(rows []FeedRow) bool {
	added := false
	for _, row := range rows {
		if !s.admitFeedID(row.ID) {
			continue
		}
		s.feed = append(s.feed, row)
		added = true
	}
	if !added {
		return false
	}
	// The ring is chronological and a snapshot reads it newest-first, so
	// history that arrived after the live rows is put back behind them.
	// Stable, so rows sharing an instant keep the order they arrived in.
	slices.SortStableFunc(s.feed, func(a, b FeedRow) int {
		return newStamp(a.Timestamp).chronological(newStamp(b.Timestamp))
	})
	s.trimFeed()
	return true
}

// seedSpend adds stored spend records to the live window, reporting whether
// any counted.
func (s *LiveState) seedSpend(records []tokens.Record) bool {
	if len(records) == 0 {
		return false
	}
	entries := make([]spendEntry, 0, len(records))
	for _, r := range records {
		entries = append(entries, spendEntry{at: newStamp(r.Timestamp), Record: r})
	}
	// OLDEST FIRST, before a single id is recorded. The dedupe set evicts
	// its oldest insertions past its cap, and the store answers newest
	// first: inserted in that order, a day busier than the cap would evict
	// the ids of the NEWEST records, which are exactly the ones the live
	// stream can still redeliver.
	slices.SortStableFunc(entries, func(a, b spendEntry) int { return a.at.chronological(b.at) })
	// Only the newest records the window can hold are worth an id: the
	// count cap below would drop the rest from the front anyway. The seed's
	// own read already stops at the cap; this holds for any other caller.
	if len(entries) > SpendRecordLimit {
		entries = entries[len(entries)-SpendRecordLimit:]
	}

	counted := false
	for _, entry := range entries {
		// A RECORD WITH NO ID IS DROPPED rather than counted. Unlike a feed
		// row, a spend record is SUMMED, and the dedupe has nothing to hold
		// this one by — so a second seed would add it again and the rollup
		// would grow on every one. A rollup slightly short is better than a
		// rollup that climbs on its own.
		if entry.EventID == "" {
			continue
		}
		if _, held := s.spendIDs[entry.EventID]; held {
			continue
		}
		s.spendIDs[entry.EventID] = struct{}{}
		s.spend = append(s.spend, entry)
		counted = true
	}
	if !counted {
		return false
	}
	// The same order the count cap assumes, restored across the live
	// records already held and the history appended behind them.
	slices.SortStableFunc(s.spend, func(a, b spendEntry) int { return a.at.chronological(b.at) })
	// Pruned against the CLOCK rather than an event's own stamp: nothing is
	// arriving here, and the window a seed has to respect is the one ending
	// now.
	s.pruneSpend(s.clock().Format(time.RFC3339Nano))
	return true
}
