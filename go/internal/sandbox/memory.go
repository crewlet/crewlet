package sandbox

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sort"
	"sync"
	"time"
)

// MemoryStore is the pending-run store's in-process twin.
//
// It exists to be run against THE SAME contract suite as the SQL store, which
// is the only thing that makes it trustworthy: a twin nobody holds to the
// contract is a twin that models the store wrongly and certifies the bug.
//
// One mutex over everything rather than per-row locking. The hard property
// here is the at-most-once claim, which is a whole-store invariant, and a
// finer lock would be a second concurrency story to get right for a store
// whose contention is one seat's own turns.
type MemoryStore struct {
	mu   sync.Mutex
	runs map[string]PendingRun
	now  func() time.Time
}

// NewMemoryStore builds an empty twin.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{runs: map[string]PendingRun{}}
}

var _ PendingStore = (*MemoryStore)(nil)

// WithClock pins the clock, for a suite that asserts about ages.
func (m *MemoryStore) WithClock(now func() time.Time) *MemoryStore {
	m.now = now
	return m
}

func (m *MemoryStore) clock() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now().UTC()
}

func (m *MemoryStore) Create(_ context.Context, run PendingRun) error {
	if run.TurnID == "" {
		return fmt.Errorf("sandbox: a pending run needs a turn id")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.runs[run.TurnID]; exists {
		// Idempotent, matching the SQL store's DO NOTHING: the row that is
		// already there is the correct one, possibly with a box attached.
		return nil
	}
	if run.Status == "" {
		run.Status = StatusRunning
	}
	now := m.clock()
	if run.CreatedAt.IsZero() {
		run.CreatedAt = now
	}
	run.UpdatedAt = now
	m.runs[run.TurnID] = clone(run)
	return nil
}

func (m *MemoryStore) Get(_ context.Context, turnID string) (PendingRun, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	run, ok := m.runs[turnID]
	if !ok {
		return PendingRun{}, false, nil
	}
	return clone(run), true, nil
}

func (m *MemoryStore) ClaimForResume(_ context.Context, turnID string) (PendingRun, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	run, ok := m.runs[turnID]
	if !ok || !slices.Contains(Claimable, run.Status) {
		return PendingRun{}, false, nil
	}
	before := run.Status
	run.Status = StatusResumed
	run.UpdatedAt = m.clock()
	m.runs[turnID] = run

	out := clone(run)
	out.ClaimedFrom = before
	return out, true, nil
}

func (m *MemoryStore) MarkAwaiting(_ context.Context, turnID string, q Clarification) error {
	return m.mutate(turnID, func(run *PendingRun) {
		run.Status = StatusAwaiting
		run.Question = q.Question
		run.Audience = q.Audience
		run.Branch = q.Branch
		run.SessionID = q.SessionID
	})
}

func (m *MemoryStore) ClaimOwnership(_ context.Context, turnID, owner string, epoch int64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	run, ok := m.runs[turnID]
	if !ok || run.OwnerEpoch > epoch {
		// A newer lease already owns it; taking the run would put two
		// engines on one box.
		return false, nil
	}
	run.Owner, run.OwnerEpoch, run.UpdatedAt = owner, epoch, m.clock()
	m.runs[turnID] = run
	return true, nil
}

func (m *MemoryStore) SetStatus(_ context.Context, turnID, status string, fence Fence) error {
	if !slices.Contains(allStatuses, status) {
		return fmt.Errorf("sandbox: unknown status %q", status)
	}
	return m.fenced(turnID, fence, func(run *PendingRun) { run.Status = status })
}

func (m *MemoryStore) AttachSandbox(_ context.Context, turnID string, box BoxRef, fence Fence) error {
	return m.fenced(turnID, fence, func(run *PendingRun) {
		run.SandboxID = box.SandboxID
		run.CommandID = box.CommandID
		run.CodingAgent = box.CodingAgent
		run.SessionID = box.SessionID
		run.PauseTTLSeconds = box.PauseTTLSec
	})
}

func (m *MemoryStore) MarkBoxPaused(_ context.Context, turnID string, at time.Time) error {
	return m.mutate(turnID, func(run *PendingRun) {
		if at.IsZero() {
			at = m.clock()
		}
		run.PausedAt = at
	})
}

func (m *MemoryStore) ReleaseBox(_ context.Context, turnID string) error {
	return m.mutate(turnID, func(run *PendingRun) {
		// Both together — a paused_at pointing at no box is a snapshot the
		// reaper looks for every tick and never finds.
		run.SandboxID, run.CommandID, run.PausedAt = "", "", time.Time{}
	})
}

func (m *MemoryStore) SaveExecuteState(_ context.Context, turnID string, state map[string]any) error {
	return m.mutate(turnID, func(run *PendingRun) {
		run.ExecuteState = maps.Clone(state)
	})
}

func (m *MemoryStore) ListActive(_ context.Context) ([]PendingRun, error) {
	return m.list(func(r PendingRun) bool { return slices.Contains(Active, r.Status) }), nil
}

func (m *MemoryStore) ListActiveForSeat(_ context.Context, handle string) ([]PendingRun, error) {
	return m.list(func(r PendingRun) bool {
		return r.AgentHandle == handle && slices.Contains(Active, r.Status)
	}), nil
}

func (m *MemoryStore) FindAwaitingByConversation(_ context.Context, handle, conversation string) (PendingRun, bool, error) {
	if conversation == "" {
		return PendingRun{}, false, nil
	}
	got := m.list(func(r PendingRun) bool {
		return r.AgentHandle == handle && r.ConversationKey == conversation &&
			slices.Contains(Awaiting, r.Status)
	})
	if len(got) == 0 {
		return PendingRun{}, false, nil
	}
	// Newest first: a seat can have parked more than one question on one
	// thread, and the answer belongs to what the person was just asked.
	return got[len(got)-1], true, nil
}

func (m *MemoryStore) ListPausedBefore(_ context.Context, cutoff time.Time) ([]PendingRun, error) {
	got := m.list(func(r PendingRun) bool {
		return r.Paused() && r.HasBox() && r.PausedAt.Before(cutoff)
	})
	sort.SliceStable(got, func(i, j int) bool { return got[i].PausedAt.Before(got[j].PausedAt) })
	return got, nil
}

func (m *MemoryStore) Delete(_ context.Context, turnID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.runs, turnID)
	return nil
}

// mutate applies a change to a run that exists, ignoring one that does not —
// matching the SQL store, where an UPDATE that matches no row is not an error.
func (m *MemoryStore) mutate(turnID string, fn func(*PendingRun)) error {
	return m.fenced(turnID, Fence{}, fn)
}

func (m *MemoryStore) fenced(turnID string, fence Fence, fn func(*PendingRun)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	run, ok := m.runs[turnID]
	if !ok {
		return nil
	}
	if fence.Fenced() && run.OwnerEpoch > fence.Epoch {
		// The lease moved. Silently, like the SQL store's WHERE: the write
		// is refused and the caller finds out by reading, which is what a
		// fence is — not an error path, an unreachable one.
		return nil
	}
	fn(&run)
	run.UpdatedAt = m.clock()
	m.runs[turnID] = run
	return nil
}

func (m *MemoryStore) list(match func(PendingRun) bool) []PendingRun {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []PendingRun
	for _, run := range m.runs {
		if match(run) {
			out = append(out, clone(run))
		}
	}
	// Sorted, so a listing is stable across the map's randomised iteration
	// — a recovery pass that reordered its work every boot would make two
	// runs of the same failure look like different failures.
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].TurnID < out[j].TurnID
	})
	return out
}

// clone deep-copies the reference fields, so a caller that mutates what it read
// has not also mutated the store — the one way a memory twin diverges from a
// SQL store that hands back rows by value.
func clone(r PendingRun) PendingRun {
	r.Plan = maps.Clone(r.Plan)
	r.NotificationMetadata = maps.Clone(r.NotificationMetadata)
	r.ExecuteState = maps.Clone(r.ExecuteState)
	r.SuccessCriteria = slices.Clone(r.SuccessCriteria)
	r.DelegationChain = slices.Clone(r.DelegationChain)
	return r
}
