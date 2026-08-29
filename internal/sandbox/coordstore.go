package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sort"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// CoordStore is the pending-run store, on the FLEET's coordination store.
//
// The one implementation, and the node's own database is deliberately not an
// option. A detached run OUTLIVES its turn, its process and sometimes its
// node: when the seat moves — a lease lapse, a drain, a rolling upgrade — the
// node that owns it next is the one whose recovery pass has to find the run.
// On a per-node store that pass found nothing, so the suspended Execute
// conversation was unreachable and the sandbox was neither resumed nor reaped:
// a billed box, running to its own TTL, handing its result to nobody. The
// release path's own comment states the contract this restores — "a detached
// run belongs to its row, not to this process, and the seat's next owner
// recovers it through RecoverSeat" — which was only ever true of a row the
// successor could read.
//
// A single-node company is not a special case: it runs the in-memory
// coordination twin, which is a real implementation of the same certified
// contract rather than a stub.
//
// # A mutex became a compare-and-swap
//
// Every mutation here is a CONDITIONAL FLIP whose condition is one of the
// run's own fields — the at-most-once tail claim on the status, the epoch
// fence on a write, the pause expiry on "is it still parked". Within one
// process a mutex made those atomic; across a fleet the condition has to be
// re-evaluated against what the store actually holds, so each is a
// read-decide-write under the record's version, retried on a lost race. A
// caller that loses re-reads, which is what makes the SECOND writer see the
// first one's decision instead of overwriting it.
type CoordStore struct {
	runs coord.SandboxRuns
	now  func() time.Time
}

// NewCoordStore wraps the fleet's run records.
func NewCoordStore(runs coord.SandboxRuns) *CoordStore {
	return &CoordStore{runs: runs}
}

var _ PendingStore = (*CoordStore)(nil)

// WithClock pins the clock, for a suite that asserts about ages.
func (s *CoordStore) WithClock(now func() time.Time) *CoordStore {
	s.now = now
	return s
}

func (s *CoordStore) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now().UTC()
}

// casRetries bounds one read-decide-write.
//
// Sixteen, matching the coordination store's own loops. Contention here is a
// seat's own turns plus a recovering peer — two or three writers at the very
// worst — so exhausting this many rounds is a store that is not settling
// rather than a queue of legitimate writers, and it is reported as an error
// instead of as a lost race: a caller told "somebody else got there" stops,
// while a caller told "no answer" retries.
const casRetries = 16

// Create records a new run row.
//
// Idempotent on the turn id: the id is the kick-off turn's, so a second create
// is a retried launch and the row already there is the correct one — possibly
// with a box already attached.
func (s *CoordStore) Create(ctx context.Context, run PendingRun) error {
	if run.TurnID == "" {
		return fmt.Errorf("sandbox: a pending run needs a turn id")
	}
	if run.Status == "" {
		run.Status = StatusRunning
	}
	now := s.clock()
	if run.CreatedAt.IsZero() {
		run.CreatedAt = now
	}
	run.UpdatedAt = now
	raw, err := encodeRun(run)
	if err != nil {
		return err
	}
	if _, err := s.runs.CreateSandboxRun(ctx, run.TurnID, raw); err != nil {
		return fmt.Errorf("sandbox: create run %s: %w", run.TurnID, err)
	}
	return nil
}

// Get returns one run by turn id.
func (s *CoordStore) Get(ctx context.Context, turnID string) (PendingRun, bool, error) {
	run, _, found, err := s.read(ctx, turnID)
	return run, found, err
}

// ClaimForResume flips a claimable run to resumed, reporting the row IFF THIS
// CALL WON.
//
// The at-most-once tail guard, and the reason the version matters: two nodes
// can be handed the same completion — a redelivery, a zombie finishing between
// fence checks — and exactly one must run the tail. The status check and the
// write are one compare-and-swap, so the loser sees `resumed` on its re-read
// and reports false.
func (s *CoordStore) ClaimForResume(ctx context.Context, turnID string) (PendingRun, bool, error) {
	var before string
	run, won, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		if !slices.Contains(Claimable, run.Status) {
			return false
		}
		before = run.Status
		run.Status = StatusResumed
		return true
	})
	if err != nil || !won {
		return PendingRun{}, false, err
	}
	// Carried back so a failed dispatch can put the row exactly where it
	// was. Never persisted — inferring it from the other fields is unsound,
	// because a reused run keeps its old question.
	run.ClaimedFrom = before
	return run, true, nil
}

// MarkAwaiting parks a run until a person answers.
func (s *CoordStore) MarkAwaiting(ctx context.Context, turnID string, q Clarification) error {
	_, _, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		run.Status = StatusAwaiting
		run.Question = q.Question
		run.Audience = q.Audience
		run.Branch = q.Branch
		run.SessionID = q.SessionID
		return true
	})
	return err
}

// ClaimOwnership moves a run to this node, refusing to steal a newer lease.
func (s *CoordStore) ClaimOwnership(ctx context.Context, turnID, owner string, epoch int64) (bool, error) {
	_, won, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		if run.OwnerEpoch > epoch {
			// A newer lease already owns it; taking the run would put
			// two engines on one box.
			return false
		}
		run.Owner, run.OwnerEpoch = owner, epoch
		return true
	})
	return won, err
}

// SetStatus moves a run to a new lifecycle state, fenced on the epoch.
func (s *CoordStore) SetStatus(ctx context.Context, turnID, status string, fence Fence) error {
	if !slices.Contains(allStatuses, status) {
		return fmt.Errorf("sandbox: unknown status %q", status)
	}
	_, _, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		if outranked(*run, fence) {
			return false
		}
		run.Status = status
		return true
	})
	return err
}

// ExpirePause flips a parked run to reseed and clears its box record. See the
// contract on [PendingStore].
func (s *CoordStore) ExpirePause(ctx context.Context, turnID string) (bool, error) {
	_, won, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		if run.Status != StatusAwaiting {
			return false
		}
		run.Status = StatusReseed
		// Cleared in the SAME write as the flip: two writes leave a
		// state a reader can see, in which a reseeded run still names
		// the box an arriving answer would be told to continue in.
		run.SandboxID, run.CommandID = "", ""
		run.PausedAt = time.Time{}
		return true
	})
	return won, err
}

// AttachSandbox records which box a run is using, fenced on the epoch.
func (s *CoordStore) AttachSandbox(ctx context.Context, turnID string, box BoxRef, fence Fence) error {
	_, _, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		if outranked(*run, fence) {
			return false
		}
		run.SandboxID = box.SandboxID
		run.CommandID = box.CommandID
		run.CodingAgent = box.CodingAgent
		run.SessionID = box.SessionID
		run.PauseTTLSeconds = box.PauseTTLSec
		return true
	})
	return err
}

// MarkBoxPaused stamps the box as snapshotted, with the instant the pause TTL
// runs from.
func (s *CoordStore) MarkBoxPaused(ctx context.Context, turnID string, at time.Time) error {
	_, _, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		if at.IsZero() {
			at = s.clock()
		}
		run.PausedAt = at
		return true
	})
	return err
}

// ReleaseBox forgets the box a run was using.
func (s *CoordStore) ReleaseBox(ctx context.Context, turnID string) error {
	_, _, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		// Both together — a paused_at pointing at no box is a snapshot
		// the reaper looks for every tick and never finds.
		run.SandboxID, run.CommandID, run.PausedAt = "", "", time.Time{}
		return true
	})
	return err
}

// SaveExecuteState persists the suspended Execute loop a resume splices the
// coding agent's answer back into.
func (s *CoordStore) SaveExecuteState(ctx context.Context, turnID string, state map[string]any) error {
	_, _, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		run.ExecuteState = maps.Clone(state)
		return true
	})
	return err
}

// ListActive returns every run that has not finished.
func (s *CoordStore) ListActive(ctx context.Context) ([]PendingRun, error) {
	return s.list(ctx, func(r PendingRun) bool { return slices.Contains(Active, r.Status) })
}

// ListActiveForSeat returns one seat's unfinished runs.
//
// The read a seat's new owner makes inside on_acquire, and the whole reason
// this store is the fleet's: per-node it answered empty for every run the
// previous owner had launched.
func (s *CoordStore) ListActiveForSeat(ctx context.Context, handle string) ([]PendingRun, error) {
	return s.list(ctx, func(r PendingRun) bool {
		return r.AgentHandle == handle && slices.Contains(Active, r.Status)
	})
}

// FindAwaitingByConversation finds the parked run a reply belongs to.
func (s *CoordStore) FindAwaitingByConversation(ctx context.Context, handle, conversation string) (PendingRun, bool, error) {
	if conversation == "" {
		return PendingRun{}, false, nil
	}
	got, err := s.list(ctx, func(r PendingRun) bool {
		return r.AgentHandle == handle && r.ConversationKey == conversation &&
			slices.Contains(Awaiting, r.Status)
	})
	if err != nil || len(got) == 0 {
		return PendingRun{}, false, err
	}
	// Newest first: a seat can have parked more than one question on one
	// thread, and the answer belongs to what the person was just asked.
	return got[len(got)-1], true, nil
}

// ListPausedBefore returns the boxes whose pause TTL has expired.
func (s *CoordStore) ListPausedBefore(ctx context.Context, cutoff time.Time) ([]PendingRun, error) {
	got, err := s.list(ctx, func(r PendingRun) bool {
		return r.Paused() && r.HasBox() && r.PausedAt.Before(cutoff)
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(got, func(i, j int) bool { return got[i].PausedAt.Before(got[j].PausedAt) })
	return got, nil
}

// Delete removes a run record.
//
// Conditional on the version it read, and retried: a terminal delete racing a
// write that reopened the run must not take the reopened record with it.
// Deleting a run that is already gone is not an error — both parties reaching
// the end of one run is ordinary.
func (s *CoordStore) Delete(ctx context.Context, turnID string) error {
	for range casRetries {
		_, version, found, err := s.read(ctx, turnID)
		if err != nil {
			return err
		}
		if !found {
			return nil
		}
		gone, err := s.runs.DeleteSandboxRun(ctx, turnID, version)
		if err != nil {
			return fmt.Errorf("sandbox: delete run %s: %w", turnID, err)
		}
		if gone {
			return nil
		}
	}
	return fmt.Errorf("sandbox: delete run %s: the record kept changing under the delete", turnID)
}

// read decodes one record and the version it was read at.
func (s *CoordStore) read(ctx context.Context, turnID string) (PendingRun, uint64, bool, error) {
	record, found, err := s.runs.SandboxRun(ctx, turnID)
	if err != nil {
		return PendingRun{}, 0, false, fmt.Errorf("sandbox: read run %s: %w", turnID, err)
	}
	if !found {
		return PendingRun{}, 0, false, nil
	}
	run, err := decodeRun(record)
	if err != nil {
		return PendingRun{}, 0, false, err
	}
	return run, record.Version, true, nil
}

// mutate is the read-decide-write every conditional flip runs through.
//
// decide returns whether the change should be written. FALSE IS NOT A FAILURE
// — it is the condition not holding, which is the answer for a claim somebody
// else won, a fence a moved lease outranks, or a pause the reaper is too late
// for. A run that does not exist is the same non-answer, matching the SQL
// store this replaces, where an UPDATE that matched no row was never an error.
func (s *CoordStore) mutate(ctx context.Context, turnID string, decide func(*PendingRun) bool) (PendingRun, bool, error) {
	for range casRetries {
		run, version, found, err := s.read(ctx, turnID)
		if err != nil {
			return PendingRun{}, false, err
		}
		if !found {
			return PendingRun{}, false, nil
		}
		next := run
		if !decide(&next) {
			return PendingRun{}, false, nil
		}
		next.UpdatedAt = s.clock()
		raw, err := encodeRun(next)
		if err != nil {
			return PendingRun{}, false, err
		}
		won, err := s.runs.UpdateSandboxRun(ctx, turnID, raw, version)
		if err != nil {
			return PendingRun{}, false, fmt.Errorf("sandbox: update run %s: %w", turnID, err)
		}
		if won {
			return next, true, nil
		}
		// Lost the version. RE-READ AND RE-DECIDE rather than re-writing
		// what we computed: the other writer may have taken the claim,
		// moved the lease or unparked the run, and every condition above
		// is evaluated against fields it could have changed.
	}
	return PendingRun{}, false, fmt.Errorf(
		"sandbox: update run %s: the record kept changing under the write", turnID)
}

// list decodes every record and returns the ones that match, oldest first.
//
// The filters are the caller's because coordination cannot see the fields they
// read — the seat, the status, the conversation key, the pause instant. The
// set is bounded by the seats that can be mid-run at once, which is what makes
// one read and a local filter the right shape rather than a scan to apologise
// for.
func (s *CoordStore) list(ctx context.Context, match func(PendingRun) bool) ([]PendingRun, error) {
	records, err := s.runs.SandboxRuns(ctx)
	if err != nil {
		return nil, fmt.Errorf("sandbox: list runs: %w", err)
	}
	var out []PendingRun
	for _, record := range records {
		run, err := decodeRun(record)
		if err != nil {
			return nil, err
		}
		if match(run) {
			out = append(out, run)
		}
	}
	// Oldest first, so a recovery pass works its runs in the order they
	// were launched: a listing that reordered every boot would make two
	// runs of the same failure look like different failures.
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].TurnID < out[j].TurnID
	})
	return out, nil
}

// outranked reports whether a fence has been overtaken by a newer lease.
//
// A zero fence constrains nothing, deliberately: recovery writes and the boot
// pass legitimately hold no lease yet. What must never happen is a node
// writing under a lease it has LOST.
func outranked(run PendingRun, fence Fence) bool {
	return fence.Fenced() && run.OwnerEpoch > fence.Epoch
}

func encodeRun(run PendingRun) ([]byte, error) {
	raw, err := json.Marshal(run)
	if err != nil {
		return nil, fmt.Errorf("sandbox: encode run %s: %w", run.TurnID, err)
	}
	return raw, nil
}

func decodeRun(record coord.Record) (PendingRun, error) {
	var run PendingRun
	if err := json.Unmarshal(record.Value, &run); err != nil {
		return PendingRun{}, fmt.Errorf("sandbox: decode run %s: %w", record.Key, err)
	}
	// The KEY is the identity, not the field: a record whose body somehow
	// disagrees with the key it is stored under would hand a caller a run
	// it cannot then write back.
	run.TurnID = record.Key
	return run, nil
}
