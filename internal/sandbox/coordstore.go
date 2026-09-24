package sandbox

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

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

// BeginLaunch opens a launch on this turn's row. See the contract on
// [PendingStore].
func (s *CoordStore) BeginLaunch(ctx context.Context, run PendingRun, fence Fence) error {
	if run.TurnID == "" {
		return fmt.Errorf("sandbox: a pending run needs a turn id")
	}
	now := s.clock()
	if run.CreatedAt.IsZero() {
		run.CreatedAt = now
	}
	// Not the caller's to choose: a row exists to be launched into, and
	// the only status that can mean is launching.
	run.Status = StatusLaunching
	// Nor is the name of the job, and it is new on every launch, the
	// reset below included: a completion claims only the job it names.
	run.LaunchID = uuid.NewString()
	run.UpdatedAt = now
	// And the job's own record starts here, keyed on that name: the
	// instant the launch exists is the instant its phase began, and a
	// previous job's record — its start, its iteration, whether its phase
	// was published — is not this one's. Only the model is the caller's.
	run.Launch = LaunchRecord{ID: run.LaunchID, StartedAt: now, Model: run.Launch.Model}
	raw, err := encodeRun(run)
	if err != nil {
		return err
	}
	created, err := s.runs.CreateSandboxRun(ctx, run.TurnID, raw)
	if err != nil {
		return fmt.Errorf("sandbox: create run %s: %w", run.TurnID, err)
	}
	if created {
		return nil
	}
	// The row was already there — a second run_sandbox call in this turn,
	// or a redelivered kick-off. Only the LAUNCH-SCOPED state is reset: the
	// identity fields stay the existing row's, and so does the box
	// reference, which the caller is about to reattach to.
	_, _, err = s.mutate(ctx, run.TurnID, func(existing *PendingRun) bool {
		if outranked(*existing, fence) {
			return false
		}
		existing.Status = StatusLaunching
		existing.LaunchID = run.LaunchID
		existing.Launch = run.Launch
		// The previous job's suspension is not this job's. Left in place
		// it is worse than absent: a completion claimed before the new
		// suspension lands would resume the conversation the LAST call
		// suspended, splicing this run's findings into a loop that has
		// already moved on.
		existing.ExecuteState = nil
		// And its question is answered, or was never asked — either way a
		// reply arriving now belongs to the new job, not the old one.
		existing.Question, existing.Audience = "", ""
		// AND ITS COST: a parked job's tokens are paid by the resume its
		// answer drives, which has happened by the time a new job opens.
		existing.ParkedInputTokens, existing.ParkedOutputTokens = 0, 0
		// NOR ARE THE PREVIOUS JOB'S TOOL CALLS THIS JOB'S. The bridged
		// log is the whole record an agent-mode resume rebuilds its phase
		// from, and a second executor round under the same turn id — what
		// a reviewer's self_iterate produces — would otherwise replay the
		// FIRST round's submit_work: a round that in fact submitted
		// nothing would report the previous round's outcome instead of
		// being rescued, and its deliveries would satisfy this round's
		// delivery check.
		existing.BridgeCalls, existing.BridgeCallsElided = nil, 0
		// AND THE PREVIOUS JOB'S CHARGE IS NOT THIS JOB'S. Carried over,
		// it would tell this job's completion that its spend is already
		// counted, and the company would never be billed for it.
		existing.Charged = false
		return true
	})
	return err
}

// Get returns one run by turn id.
func (s *CoordStore) Get(ctx context.Context, turnID string) (PendingRun, bool, error) {
	run, _, found, err := s.read(ctx, turnID)
	return run, found, err
}

// ClaimForResume flips a run holding the tail's launch, in one of the tail's
// statuses, to resumed, reporting the row IFF THIS CALL WON.
//
// The at-most-once tail guard, and the reason the version matters: two nodes
// can be handed the same completion (a redelivery, a zombie finishing between
// fence checks) and exactly one must run the tail. The launch, the status and
// the write are one compare-and-swap, so the loser sees `resumed` on its
// re-read, or a job that is no longer its own, and reports false.
func (s *CoordStore) ClaimForResume(ctx context.Context, turnID string, tail Tail) (PendingRun, bool, error) {
	var before string
	run, won, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		if run.LaunchID != tail.Launch || !slices.Contains(tail.From, run.Status) ||
			!slices.Contains(Claimable, run.Status) {
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

// ReleaseClaim hands a claimed run back while the claim still stands. See the
// contract on [PendingStore].
func (s *CoordStore) ReleaseClaim(ctx context.Context, turnID string, release Release) (bool, error) {
	if !slices.Contains(Claimable, release.To) {
		return false, fmt.Errorf("sandbox: a claim is never taken out of %q, so it cannot be released to it",
			release.To)
	}
	_, released, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		if run.Status != StatusResumed || run.LaunchID != release.Launch || outranked(*run, release.Fence) {
			return false
		}
		run.Status = release.To
		run.Charged = run.Charged || release.Charged
		if release.Published {
			// Onto THIS job's record, starting one where the row carries
			// none of its own: a row an older build launched has no record,
			// and one it relaunched carries the previous job's.
			if run.Launch.ID != run.LaunchID {
				run.Launch = LaunchRecord{ID: run.LaunchID}
			}
			run.Launch.Published = true
		}
		return true
	})
	return released, err
}

// MarkAwaiting parks a run until a person answers.
func (s *CoordStore) MarkAwaiting(ctx context.Context, turnID string, q Clarification) error {
	_, _, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		run.Status = StatusAwaiting
		run.Question = q.Question
		run.Audience = q.Audience
		run.Branch = q.Branch
		run.SessionID = q.SessionID
		run.ParkedInputTokens, run.ParkedOutputTokens = q.InputTokens, q.OutputTokens
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
	if !slices.Contains(Active, status) {
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
		// A box being attached is a box that is RUNNING, so the snapshot
		// stamp goes with it. A reused box is attached while its row still
		// carried the paused_at from the collect that snapshotted it, and
		// paused_at is half of what the operator board draws a held box
		// from — so a live second run rendered as a paused one, being
		// billed for, for the rest of the turn.
		run.PausedAt = time.Time{}
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

// AppendBridgeCall records one tool call a bridged run made.
//
// NO FENCE, and that is deliberate. Every other mutation here is an ownership
// decision — a claim, a status flip, a pause — and a node whose lease has
// moved must not make one. This is a LOG APPEND: the call already ran and its
// effect already happened, and refusing to record it because the seat moved
// mid-run would lose evidence of something that is true either way. The row's
// own version still guards the write, so two concurrent appends serialise
// rather than clobbering each other.
//
// A run whose row is gone is not an error: the run ended while a late call was
// in flight, which is the ordinary shape of a box shutting down. The append is
// simply dropped, and the caller — which must not fail the box's call over
// telemetry — treats false the same as true.
func (s *CoordStore) AppendBridgeCall(ctx context.Context, turnID string, call BridgeCall) (bool, error) {
	if call.At.IsZero() {
		call.At = s.clock()
	}
	_, won, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		run.BridgeCalls, run.BridgeCallsElided = appendBounded(
			run.BridgeCalls, run.BridgeCallsElided, call)
		return true
	})
	return won, err
}

// appendBounded adds one call and drops from the MIDDLE past the cap.
//
// The start and the end are what explain a run — how it set about the work and
// how it finished — so a log truncated to its last N loses the half a reader
// most often needs. The count of what was dropped rides along, because a log
// that silently skips is a log that lies about what the run did.
func appendBounded(calls []BridgeCall, elided int, next BridgeCall) ([]BridgeCall, int) {
	calls = append(calls, next)
	if len(calls) <= MaxBridgeCalls {
		return calls, elided
	}
	head := MaxBridgeCalls / 2
	tail := MaxBridgeCalls - head
	dropped := len(calls) - MaxBridgeCalls
	kept := make([]BridgeCall, 0, MaxBridgeCalls)
	kept = append(kept, calls[:head]...)
	kept = append(kept, calls[len(calls)-tail:]...)
	return kept, elided + dropped
}

// MarkSuspended writes the suspended Execute loop and opens the run to the
// completion poll. See the contract on [PendingStore].
func (s *CoordStore) MarkSuspended(ctx context.Context, turnID string, suspension Suspension) (bool, error) {
	_, won, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		if run.Status != StatusLaunching {
			return false
		}
		run.ExecuteState = maps.Clone(suspension.State)
		run.Status = StatusRunning
		// Keyed on the job like the rest of its record — see ReleaseClaim.
		if run.Launch.ID != run.LaunchID {
			run.Launch = LaunchRecord{ID: run.LaunchID}
		}
		run.Launch.Iteration = suspension.Iteration
		return true
	})
	return won, err
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
//
// MATCHED BY [ConversationRef.Best] — which is where the whole rule lives:
// which parked runs a delivery may answer, which of them it answers when
// several may, and what either does with a row written before the conversation
// identity existed. This store contributes the CANDIDATES and nothing else: a
// store that decided any of it its own way would be a second opinion about
// which question a person just replied to, and the two would disagree the
// first time one of them was changed.
func (s *CoordStore) FindAwaitingByConversation(ctx context.Context, handle string, conv ConversationRef) (PendingRun, bool, error) {
	if conv.Identity == "" && conv.Partition == "" {
		// A delivery that names no conversation must never match, or
		// every parked run answers every wake that could not name one.
		return PendingRun{}, false, nil
	}
	parked, err := s.list(ctx, func(r PendingRun) bool {
		return r.AgentHandle == handle && slices.Contains(Awaiting, r.Status)
	})
	if err != nil {
		return PendingRun{}, false, err
	}
	run, found := conv.Best(parked)
	return run, found, nil
}

// Finish ends a run by deleting its record. See the contract on
// [PendingStore].
//
// A read-decide-delete under the record's version, like every flip here: the
// fence AND the status license are evaluated against what the store holds, and
// a lost race re-reads, so a claim that moved the lease — or a relaunch that
// moved the status — in between is seen rather than deleted over. That is what
// lets a caller that could not read the row hand the decision here instead.
func (s *CoordStore) Finish(ctx context.Context, turnID string, fence Fence, whileIn []string,
) (PendingRun, bool, error) {
	for range casRetries {
		run, version, found, err := s.read(ctx, turnID)
		if err != nil {
			return PendingRun{}, false, err
		}
		if !found || outranked(run, fence) || !slices.Contains(whileIn, run.Status) {
			return PendingRun{}, false, nil
		}
		gone, err := s.runs.DeleteSandboxRun(ctx, turnID, version)
		if err != nil {
			return PendingRun{}, false, fmt.Errorf("sandbox: finish run %s: %w", turnID, err)
		}
		if gone {
			return run, true, nil
		}
	}
	return PendingRun{}, false, fmt.Errorf(
		"sandbox: finish run %s: the record kept changing under the delete", turnID)
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
	slices.SortStableFunc(out, func(a, b PendingRun) int {
		return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.TurnID, b.TurnID))
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

// encodeRun writes a run back whole, keys this build does not know included.
//
// THE KNOWN FIELDS WIN. [PendingRun.Extra] holds only what the decode could
// not place in the struct, so a key in both can only mean a caller put it
// there by hand, and the value this build decided is the one that lands.
// "Known" is what the struct DECLARES ([pendingRunKeys]), never what the
// marshal happened to emit: an omitempty field left at its zero value emits
// nothing, and testing presence would let a carried key of the same name
// bring back the value this build just cleared.
func encodeRun(run PendingRun) ([]byte, error) {
	raw, err := json.Marshal(run)
	if err != nil {
		return nil, fmt.Errorf("sandbox: encode run %s: %w", run.TurnID, err)
	}
	if len(run.Extra) == 0 {
		return raw, nil
	}
	fields := map[string]json.RawMessage{}
	if err = json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("sandbox: encode run %s: %w", run.TurnID, err)
	}
	declared := pendingRunKeys()
	for key, value := range run.Extra {
		if !declared[key] {
			fields[key] = value
		}
	}
	raw, err = json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("sandbox: encode run %s: %w", run.TurnID, err)
	}
	return raw, nil
}

// decodeRun reads a run and KEEPS what it cannot read — see
// [PendingRun.Extra] for the rolling upgrade that loses it otherwise.
func decodeRun(record coord.Record) (PendingRun, error) {
	var run PendingRun
	if err := json.Unmarshal(record.Value, &run); err != nil {
		return PendingRun{}, fmt.Errorf("sandbox: decode run %s: %w", record.Key, err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(record.Value, &fields); err != nil {
		return PendingRun{}, fmt.Errorf("sandbox: decode run %s: %w", record.Key, err)
	}
	known := pendingRunKeys()
	for key, value := range fields {
		if known[key] {
			continue
		}
		if run.Extra == nil {
			run.Extra = map[string]json.RawMessage{}
		}
		run.Extra[key] = value
	}
	// The KEY is the identity, not the field: a record whose body somehow
	// disagrees with the key it is stored under would hand a caller a run
	// it cannot then write back.
	run.TurnID = record.Key
	return run, nil
}

// pendingRunKeys is every wire key [PendingRun] declares, read off its own
// tags so a field added to the struct is known here the moment it exists — a
// hand-kept list would be one more place a new field could be forgotten, and
// a forgotten one would be carried in Extra AND written from the struct.
var pendingRunKeys = sync.OnceValue(func() map[string]bool {
	keys := map[string]bool{}
	typ := reflect.TypeFor[PendingRun]()
	for field := range typ.Fields() {
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		switch {
		case name == "-" || !field.IsExported():
			continue
		case name == "":
			name = field.Name
		}
		keys[name] = true
	}
	return keys
})
