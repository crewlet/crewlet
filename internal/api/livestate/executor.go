package livestate

import (
	"cmp"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/decision"
)

// The in-flight coding runs.
//
// TWO SOURCES, and neither is enough alone. The lifecycle EVENTS are what make
// the panel live — a run appears the moment it starts and flips the moment it
// asks a question — but the stream is lossy in both directions and in-memory:
// a process that came up mid-run never saw its start, one that missed a
// completion kept a ghost for good, and a restarted node started with an empty
// panel while runs parked on questions waited, sometimes for days. The DURABLE
// RUN RECORD in the coordination store is the truth about which runs exist, so
// it is read at boot and every [ReconcileInterval] after, and what it says
// replaces what the events built — except where an event is newer than the read.
//
// This replaced a twelve-hour age-out, which was wrong both ways: a lost
// completion still showed a finished run as running for up to twelve hours, and
// a run parked on a question past twelve hours — the case that most needs a
// person — silently vanished from the one panel built to show it.

// ReconcileInterval is how often the durable run record is read back over the
// live set.
//
// THIRTY SECONDS, because it bounds only the rare case: the events keep the
// panel current to the moment, and the reconcile exists for the event that
// never arrived. What it costs is one listing of the fleet's run records per
// node per interval — a single coordination-store read, the same one the
// `executors` question makes on every request. What it bounds is how long a
// run whose completion was lost can be drawn as running, which was twelve
// hours; half a minute is inside the time a person watching the panel would
// take to act on it.
const ReconcileInterval = 30 * time.Second

// heldExecutor is one run in the live set, and when THIS PROCESS last learned
// something about it — on its own clock, so the reconcile compares two instants
// read off one clock rather than a publisher's against a store's.
type heldExecutor struct {
	entry ExecutorEntry
	seen  time.Time
}

// runEnd is how a run ended: the job a completion named, and when the ending
// event was published.
type runEnd struct {
	launch string
	at     stamp
}

// applySandbox maintains the in-flight sandbox set from one lifecycle event.
func (s *LiveState) applySandbox(env Envelope, payload map[string]any) {
	turnID := str(payload, "turn_id")
	if turnID == "" {
		return
	}
	switch env.Type {
	case "executor_finished", "executor_failed", "executor_stopped":
		// A FAILED RUN LEAVES THE SET TOO. It used to be read by nothing
		// here, so a run lost to an unreachable box or a stranded claim
		// was drawn as running until the age-out took it twelve hours
		// later. And so does a STOPPED one, at once: a person who paused
		// the seat to stop it is looking at the panel, and a run drawn as
		// running until the next reconcile is a stop that seems not to
		// have worked.
		delete(s.executors, turnID)
		s.endedRuns.put(turnID, runEnd{launch: str(payload, "launch_id"), at: newStamp(env.Timestamp)})

	case "executor_launched":
		s.executors[turnID] = &heldExecutor{seen: s.clock(), entry: ExecutorEntry{
			TurnID:      turnID,
			Role:        str(payload, "role"),
			AgentHandle: str(payload, "agent_handle"),
			AgentID:     str(payload, "agent_id"),
			CodingAgent: str(payload, "coding_agent"),
			ProviderKey: str(payload, "provider_key"),
			Model:       str(payload, "model"),
			SandboxID:   str(payload, "sandbox_id"),
			Task:        str(payload, "task"),
			Status:      ExecutorRunning,
			StartedAt:   env.Timestamp,
			WorkItem:    workItemOf(payload),
			// The node that announced the start is the one that
			// launched the run and drives it.
			Owner: str(payload, "node"),
		}}

	default:
		// A clarification request. The start may have been missed — the
		// API can come up mid-run — so a minimal entry is synthesized
		// rather than the signal dropped.
		held := s.executors[turnID]
		if held == nil {
			held = &heldExecutor{entry: ExecutorEntry{
				TurnID:      turnID,
				Role:        str(payload, "role"),
				AgentHandle: str(payload, "agent_handle"),
				AgentID:     str(payload, "agent_id"),
				CodingAgent: str(payload, "coding_agent"),
				SandboxID:   str(payload, "sandbox_id"),
				StartedAt:   env.Timestamp,
				WorkItem:    workItemOf(payload),
				Owner:       str(payload, "node"),
			}}
			s.executors[turnID] = held
		}
		held.seen = s.clock()
		held.entry.Status = ExecutorAwaiting
		held.entry.Question = str(payload, "question")
		held.entry.QuestionContext = str(payload, "question_context")
		held.entry.Decision = decisionOf(payload)
		held.entry.Audience = str(payload, "audience")
		// The run collected its question and paused its box to wait: the
		// question is published once the box is held, so its instant is
		// the pause's until the record's own reading replaces it.
		held.entry.PausedAt = env.Timestamp
	}
}

// ReconcileExecutors replaces the live set with what the durable run record
// holds, read at `asOf` on this process's clock, and reports what moved.
//
// THE RECORD WINS, with two exceptions, each an event this process learned
// AFTER the read began and the record therefore could not reflect:
//
//   - a run the set holds and the record does not is dropped — unless its last
//     event arrived after the read began, which is a run started while the
//     record was being listed;
//   - a run the record holds is put in the set as the record states it —
//     unless its entry changed after the read began (it keeps the newer state),
//     or its ENDING was already seen: a completion that named the job the
//     record still describes, or a failure published after the record's last
//     write. A record lags its run's end by the collection the end starts, and
//     re-adding the run there would draw a finished job as running until the
//     next read.
//
// The record of runs that ended is pruned here to the runs the record still
// lists, which is what bounds it: an ending that the record has caught up with
// has nothing left to guard.
func (s *LiveState) ReconcileExecutors(records []ExecutorRecord, asOf time.Time) (change Change) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// THE RECORD IS WHAT A SEAT'S RUNS ARE READ FROM, so a reconcile that
	// moves a run moves its seat's state too — a run that waited on a
	// question for longer than this process has been up is found here.
	before := s.states()
	defer s.noteMoved(before, &change)

	listed := make(map[string]ExecutorRecord, len(records))
	for _, rec := range records {
		if rec.Entry.TurnID != "" && rec.Entry.Status.Valid() {
			listed[rec.Entry.TurnID] = rec
		}
	}
	moved := false
	for turnID, held := range s.executors {
		if _, ok := listed[turnID]; ok || !held.seen.Before(asOf) {
			continue
		}
		delete(s.executors, turnID)
		moved = true
	}
	for turnID, rec := range listed {
		if end, ok := s.endedRuns.get(turnID); ok && end.covers(rec) {
			continue
		}
		held := s.executors[turnID]
		if held != nil && !held.seen.Before(asOf) {
			continue
		}
		entry := rec.Entry
		if held != nil {
			// What only the announcement carries survives the record:
			// the start event's one-line summary of the brief, and the
			// instant it was published.
			entry.Task = cmp.Or(held.entry.Task, entry.Task)
			entry.StartedAt = cmp.Or(held.entry.StartedAt, entry.StartedAt)
			if sameEntry(held.entry, entry) {
				continue
			}
		}
		s.executors[turnID] = &heldExecutor{entry: entry, seen: asOf}
		moved = true
	}
	s.endedRuns.retain(func(turnID string) bool {
		_, ok := listed[turnID]
		return ok
	})
	change.Executors = moved
	return change
}

// covers reports whether this ending is the end of the run a record describes.
func (e runEnd) covers(rec ExecutorRecord) bool {
	if e.launch != "" {
		return e.launch == rec.LaunchID
	}
	if !e.at.valid || rec.WrittenAt.IsZero() {
		return true
	}
	return !rec.WrittenAt.After(e.at.t)
}

// ActiveExecutors returns in-flight detached jobs, oldest-first.
//
// Oldest-first so the longest-running job — the one most likely to need
// attention, such as one blocked on a clarification — sorts to the top of the
// panel. NOTHING IS AGED OUT: a run leaves the set when it ends or when the
// durable record stops holding it, never because it has been running long,
// since a run parked on a question can rightly wait for days.
func (s *LiveState) ActiveExecutors() []ExecutorEntry {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]ExecutorEntry, 0, len(s.executors))
	for _, held := range s.executors {
		entry := held.entry
		entry.WorkItem = cloneItem(entry.WorkItem)
		entry.AudienceHandles = slices.Clone(entry.AudienceHandles)
		out = append(out, entry)
	}
	slices.SortFunc(out, func(a, b ExecutorEntry) int {
		return cmp.Or(newStamp(a.StartedAt).chronological(newStamp(b.StartedAt)),
			strings.Compare(a.TurnID, b.TurnID))
	})
	return out
}

// sameEntry reports whether two entries say the same thing: the item compared
// by value rather than by pointer, and the question's audience as a list, an
// empty one the same as none.
//
// FIELD BY FIELD THROUGH reflect, because the audience is a slice and a struct
// holding one cannot be compared with `==`: a hand-written list of fields here
// would be a second list of the entry's fields, which a field added later
// would be missing from — and a field missing from this comparison is a change
// the reconcile never pushes.
func sameEntry(a, b ExecutorEntry) bool {
	if len(a.AudienceHandles) == 0 {
		a.AudienceHandles = nil
	}
	if len(b.AudienceHandles) == 0 {
		b.AudienceHandles = nil
	}
	return reflect.DeepEqual(a, b)
}

// decisionOf reads the choice a run's question offers off its announcement,
// nil for an open question or one this build cannot read as a decision: the
// question and its context still reach the screen, and the record the next
// reconcile reads supplies the rest.
func decisionOf(payload map[string]any) *decision.Decision {
	raw, ok := payload["decision"]
	if !ok || raw == nil {
		return nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var d decision.Decision
	if json.Unmarshal(encoded, &d) != nil || len(d.Options) == 0 {
		return nil
	}
	return &d
}
