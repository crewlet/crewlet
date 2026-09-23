package sandbox

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/textcut"
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
	runs  coord.SandboxRuns
	calls coord.BridgeCalls
	now   func() time.Time
}

// RunRecords is the slice of the fleet's coordination store a [CoordStore]
// reads and writes: the run records, and the log of the calls a bridged run
// makes. Declared here, by the consumer.
type RunRecords interface {
	coord.SandboxRuns
	coord.BridgeCalls
}

// NewCoordStore wraps the fleet's run records and bridged-call log.
func NewCoordStore(records RunRecords) *CoordStore {
	return &CoordStore{runs: records, calls: records}
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
	var replaced string
	_, reset, err := s.mutate(ctx, run.TurnID, func(existing *PendingRun) bool {
		if outranked(*existing, fence) {
			return false
		}
		replaced = existing.LaunchID
		existing.Status = StatusLaunching
		existing.LaunchID = run.LaunchID
		// The previous job's suspension is not this job's. Left in place
		// it is worse than absent: a completion claimed before the new
		// suspension lands would resume the conversation the LAST call
		// suspended, splicing this run's findings into a loop that has
		// already moved on.
		existing.ExecuteState = nil
		// And its question is answered, or was never asked — either way a
		// reply arriving now belongs to the new job, not the old one.
		existing.Question, existing.Audience = "", ""
		// NOR ARE THE PREVIOUS JOB'S TOOL CALLS THIS JOB'S. The bridged
		// log is what an agent-mode resume rebuilds its phase from, and a
		// second executor round under the same turn id — what a
		// reviewer's self_iterate produces — would otherwise replay the
		// FIRST round's submit_work: a round that in fact submitted
		// nothing would report the previous round's outcome instead of
		// being rescued, and its deliveries would satisfy this round's
		// delivery check. The per-call records are keyed by launch, so
		// the new launch id is already a new, empty log; the row's list
		// is older builds' view of the same thing and is cleared for
		// them.
		existing.BridgeCalls, existing.BridgeCallsElided = nil, 0
		// AND THE PREVIOUS JOB'S CHARGE IS NOT THIS JOB'S. Carried over,
		// it would tell this job's completion that its spend is already
		// counted, and the company would never be billed for it.
		existing.Charged = false
		return true
	})
	if err != nil || !reset || replaced == "" {
		return err
	}
	// The replaced launch's calls are read by nothing now: every reader
	// asks for the run's CURRENT launch. Purged here rather than left for
	// the sweep so a relaunching turn does not carry its previous rounds
	// until it finishes; a purge that fails costs only that wait, since
	// the sweep purges every launch no run names.
	s.purgeCalls(ctx, run.TurnID, replaced)
	return nil
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
// THE RECORD FIRST, AND IT IS THE CALL. The call is filed as its own record
// under the launch the run's row names at this moment, and once that lands
// the append has succeeded: every reader in this build reads the records.
// Only then is the row's bounded list written, for older builds that read
// nothing else (see [MaxBridgeCalls]). A failure there is logged and
// swallowed, because the call is already recorded everywhere this build looks,
// and failing the append would tell the bridge it was not. A record
// that fails to land is an error, and the row is then left alone, so the list
// never holds a call the records do not: that is what lets a reader tell a
// launch an older build recorded (no records at all) from one this build did.
//
// NO FENCE, and that is deliberate. Every other mutation here is an ownership
// decision — a claim, a status flip, a pause — and a node whose lease has
// moved must not make one. This is a LOG APPEND: the call already ran and its
// effect already happened, and refusing to record it because the seat moved
// mid-run would lose evidence of something that is true either way.
//
// A run whose row is gone is not an error: the run ended while a late call was
// in flight, which is the ordinary shape of a box shutting down. The append is
// simply dropped, and the caller — which must not fail the box's call over
// telemetry — treats false the same as true. A late call that read the row a
// moment before the run ended can still land a record under a launch nothing
// reads any more; [CoordStore.SweepBridgeCalls] is what purges it.
func (s *CoordStore) AppendBridgeCall(ctx context.Context, turnID string, call BridgeCall) (bool, error) {
	if call.At.IsZero() {
		call.At = s.clock()
	}
	run, _, found, err := s.read(ctx, turnID)
	if err != nil || !found {
		return false, err
	}
	if run.LaunchID == "" {
		// Every launch this build begins names itself, and only the
		// build that began a launch serves its bridge — so a row with no
		// launch is one this build did not launch and has no key to file
		// a call under. Said rather than filed somewhere a resume would
		// never look.
		return false, fmt.Errorf("sandbox: run %s names no launch, so its bridged call %q has "+
			"no log to be recorded in", turnID, call.Name)
	}
	fitted, raw, cut, err := fitBridgeCall(call)
	if err != nil {
		return false, fmt.Errorf("sandbox: record bridged call %q of run %s: %w", call.Name, turnID, err)
	}
	if cut != (bridgeCallCut{}) {
		log.WarnContext(ctx, "sandbox_bridge_call_fitted",
			"turn_id", turnID, "launch_id", run.LaunchID, "tool", call.Name,
			"output_bytes", cut.outputBytes, "args_bytes", cut.argsBytes,
			"record_limit_bytes", MaxBridgeCallBytes,
			"detail", "the call's record could not hold it whole; the coding agent was "+
				"handed the whole output, and the record keeps what fits, marked")
	}
	seq, err := s.calls.AppendBridgeCall(ctx, turnID, run.LaunchID, raw)
	if err != nil {
		return false, fmt.Errorf("sandbox: record bridged call %q of run %s: %w", call.Name, turnID, err)
	}
	fitted.Seq = seq

	// OLDER BUILDS' VIEW, and conditional on the SAME launch: a relaunch
	// between the record and this write has a new job on the row, and this
	// call belongs to the one before it.
	_, _, viewErr := s.mutate(ctx, turnID, func(latest *PendingRun) bool {
		if latest.LaunchID != run.LaunchID {
			return false
		}
		latest.BridgeCalls, latest.BridgeCallsElided = appendBounded(
			latest.BridgeCalls, latest.BridgeCallsElided, fitted)
		latest.BridgeCalls, latest.BridgeCallsElided = fitRowView(*latest)
		return true
	})
	if viewErr != nil {
		log.WarnContext(ctx, "sandbox_bridge_row_view_append_failed",
			"turn_id", turnID, "launch_id", run.LaunchID, "seq", seq, "tool", call.Name,
			"error", viewErr.Error(),
			"detail", "the call is recorded in its own record, which every reader in this "+
				"build reads; only an older build reading the run's row will not see it")
	}
	return true, nil
}

// fitBridgeCall encodes a call as its record, fitted to [MaxBridgeCallBytes],
// and reports what it did not keep whole.
//
// MEASURED ON THE ENCODED BYTES, not on the output's length: JSON escapes a
// control character to six bytes, so what a string costs in the record is a
// property of its bytes rather than of its length. HTML escaping is off, so
// '<', '>' and '&' — which a tool's output is full of — cost one byte each
// rather than six; the result is still JSON every decoder reads.
//
// THE ARGUMENTS ARE DECIDED FIRST, against an output of nothing but its mark:
// the most room the output can ever give back. Over the ceiling even then,
// they are replaced by [ArgsNotKept]; otherwise they stay whole. Deciding them
// after the output instead would cut the output for room the arguments were
// about to give back, and a small output — "created 123" beside a nine-megabyte
// page body — would be lost although the record had room for it.
//
// THE OUTPUT IS THEN FIT FROM THE WHOLE OF IT, cut by the encoded excess and
// re-measured until the record fits. No character encodes to fewer bytes than
// it has, so cutting the excess from the output removes at least the excess
// from the record, and what a pass adds back — the mark — is measured by the
// next. What is left when the output is down to its mark is the tool's name,
// which is one the seat's surface registered; the last check stays so that
// the size of what this returns is a guarantee rather than an assumption
// about names, and it refuses rather than cuts, because a cut name is a
// different tool.
func fitBridgeCall(call BridgeCall) (BridgeCall, []byte, bridgeCallCut, error) {
	raw, err := encodeBridgeCall(call)
	if err != nil || len(raw) <= MaxBridgeCallBytes {
		return call, raw, bridgeCallCut{}, err
	}
	var cut bridgeCallCut
	whole := call.Output
	if call.Args != "" {
		probe := call
		if probe.Output != "" {
			probe.Output = bridgeCallCutMarker
		}
		least, err := encodeBridgeCall(probe)
		if err != nil {
			return BridgeCall{}, nil, bridgeCallCut{}, err
		}
		if len(least) > MaxBridgeCallBytes {
			cut.argsBytes = len(call.Args)
			call.Args = ArgsNotKept(len(call.Args))
			if raw, err = encodeBridgeCall(call); err != nil {
				return BridgeCall{}, nil, bridgeCallCut{}, err
			}
		}
	}
	for len(raw) > MaxBridgeCallBytes && call.Output != "" && call.Output != bridgeCallCutMarker {
		over := len(raw) - MaxBridgeCallBytes
		call.Output = textcut.Within(call.Output, len(call.Output)-over)
		cut.outputBytes = len(whole)
		if raw, err = encodeBridgeCall(call); err != nil {
			return BridgeCall{}, nil, bridgeCallCut{}, err
		}
	}
	if len(raw) > MaxBridgeCallBytes {
		return BridgeCall{}, nil, bridgeCallCut{}, fmt.Errorf("the call is %d bytes as a record with "+
			"its output and arguments set aside, past the %d-byte ceiling one record may hold",
			len(raw), MaxBridgeCallBytes)
	}
	return call, raw, cut, nil
}

// bridgeCallCut is what [fitBridgeCall] did not keep whole: the whole length
// of an output it cut and of arguments it replaced, zero for each it kept.
// For the log line that says so; the record's own fields carry the marks.
type bridgeCallCut struct{ outputBytes, argsBytes int }

// bridgeCallCutMarker is what [textcut.Within] ends a cut output with.
const bridgeCallCutMarker = "…"

func encodeBridgeCall(call BridgeCall) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(call); err != nil {
		return nil, fmt.Errorf("encode the call: %w", err)
	}
	// The encoder ends every value with a newline, which is not part of it.
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// BridgeCalls returns the run's current launch's log. See the contract on
// [PendingStore].
func (s *CoordStore) BridgeCalls(ctx context.Context, run PendingRun) (BridgeLog, error) {
	if run.LaunchID == "" {
		// A launch no build of this one began: whatever was recorded of
		// it is on the row.
		return rowLog(run), nil
	}
	records, err := s.calls.BridgeCalls(ctx, run.TurnID, run.LaunchID)
	if err != nil {
		return BridgeLog{}, fmt.Errorf("sandbox: read the bridged calls of run %s: %w", run.TurnID, err)
	}
	if len(records) == 0 {
		// NO RECORDS IS AN OLDER BUILD'S LAUNCH, or a launch that made no
		// calls — and the row answers both: this build never writes the
		// row's list without the record, so a list here is one only the
		// row ever held.
		return rowLog(run), nil
	}
	calls, err := decodeBridgeCalls(records)
	if err != nil {
		return BridgeLog{}, err
	}
	return BridgeLog{Calls: calls}, nil
}

// rowLog is the log an older build kept on the run's row: the list, and the
// calls it dropped from between the list's two halves.
//
// The place is [appendBounded]'s rule, which the older builds' own is the
// same as: the first MaxBridgeCalls/2 calls are kept, and a drop is always
// after them.
func rowLog(run PendingRun) BridgeLog {
	log := BridgeLog{Calls: slices.Clone(run.BridgeCalls), Dropped: run.BridgeCallsElided}
	if log.Dropped > 0 {
		log.DroppedAfter = min(len(log.Calls), MaxBridgeCalls/2)
	}
	return log
}

// BridgeCallPage returns one page of the log [CoordStore.BridgeCalls] reads.
// See the contract on [PendingStore].
func (s *CoordStore) BridgeCallPage(ctx context.Context, run PendingRun, after uint64, limit int) (BridgeCallPage, error) {
	if limit < 1 {
		return BridgeCallPage{}, fmt.Errorf("sandbox: a page of bridged calls needs a limit of at least one, got %d", limit)
	}
	if run.LaunchID != "" {
		page, found, err := s.recordPage(ctx, run, after, limit)
		if err != nil || found {
			return page, err
		}
	}
	// The row's list, for a launch an older build recorded. It numbers
	// nothing, so its cursor is a POSITION in the list, counted from 1 the
	// way a Seq is — and the whole list is one row, so this is paging what
	// is already in hand rather than bounding a read.
	calls := run.BridgeCalls
	out := BridgeCallPage{Calls: []BridgeCall{}, Total: len(calls), Dropped: run.BridgeCallsElided}
	if after >= uint64(len(calls)) {
		return out, nil
	}
	stop := min(len(calls), int(after)+limit)
	out.Calls = slices.Clone(calls[after:stop])
	if stop < len(calls) {
		out.Next = uint64(stop)
		if after == 0 {
			from := max(stop, len(calls)-limit)
			out.End = slices.Clone(calls[from:])
			out.Between = from - stop
		}
	}
	return out, nil
}

// recordPage is [CoordStore.BridgeCallPage] over the per-call records,
// reporting false when the launch has none — the row then answers.
func (s *CoordStore) recordPage(ctx context.Context, run PendingRun, after uint64, limit int) (BridgeCallPage, bool, error) {
	q := coord.BridgeCallQuery{
		TurnID: run.TurnID, LaunchID: run.LaunchID,
		After: after, Limit: limit, MaxBytes: BridgeCallPageBytes,
	}
	page, err := s.calls.BridgeCallPage(ctx, q)
	if err != nil {
		return BridgeCallPage{}, false, fmt.Errorf("sandbox: read the bridged calls of run %s: %w", run.TurnID, err)
	}
	if page.Total == 0 {
		return BridgeCallPage{}, false, nil
	}
	out := BridgeCallPage{Total: page.Total}
	if out.Calls, err = decodeBridgeCalls(page.Calls); err != nil {
		return BridgeCallPage{}, false, err
	}
	if !page.More || len(out.Calls) == 0 {
		return out, true, nil
	}
	out.Next = out.Calls[len(out.Calls)-1].Seq
	if after != 0 {
		return out, true, nil
	}
	// THE END OF THE LOG, on the first page that does not reach it: the
	// newest calls past this page, read back from the newest.
	q.After, q.Last = out.Next, true
	end, err := s.calls.BridgeCallPage(ctx, q)
	if err != nil {
		return BridgeCallPage{}, false, fmt.Errorf("sandbox: read the bridged calls of run %s: %w", run.TurnID, err)
	}
	if out.End, err = decodeBridgeCalls(end.Calls); err != nil {
		return BridgeCallPage{}, false, err
	}
	if end.More {
		// Counted off the end read's own total, which is the later of
		// the two: a call that landed between the reads is newer than
		// the page, so it is in End or between, never in Calls.
		out.Between = max(0, end.Total-len(out.Calls)-len(out.End))
	}
	return out, true, nil
}

func decodeBridgeCalls(records []coord.BridgeCallRecord) ([]BridgeCall, error) {
	out := make([]BridgeCall, 0, len(records))
	for _, record := range records {
		call, err := decodeBridgeCall(record)
		if err != nil {
			return nil, err
		}
		out = append(out, call)
	}
	return out, nil
}

func decodeBridgeCall(record coord.BridgeCallRecord) (BridgeCall, error) {
	var call BridgeCall
	if err := json.Unmarshal(record.Value, &call); err != nil {
		// RAISED, never skipped: a call the resume cannot read is a call
		// the delivery check cannot count.
		return BridgeCall{}, fmt.Errorf("sandbox: decode bridged call %d of run %s: %w",
			record.Seq, record.TurnID, err)
	}
	// The KEY is the call's place, not anything the value says.
	call.Seq = record.Seq
	return call, nil
}

// SweepBridgeCalls purges the call log of every launch no run names, and
// reports how many launches it purged.
//
// What the lifecycle's own purges can miss: a run finished on a node that died
// before its purge, a purge that failed, a late call that landed after one, a
// run an older build finished (it knows nothing of the records). Each leaves
// records under a launch no reader will ever ask for.
//
// THE LAUNCHES ARE LISTED BEFORE THE RUNS, and the order is the whole of its
// safety. A launch's first record is filed only after its row names it, so a
// launch listed first was on a row before the listing — and if the rows read
// afterwards no longer name it, it has been replaced or finished, and nothing
// will read its calls again. Read the other way round, a launch begun between
// the two reads would be listed with no row to vouch for it, and a live run's
// log would be purged under it.
func (s *CoordStore) SweepBridgeCalls(ctx context.Context) (int64, error) {
	launches, err := s.calls.BridgeLaunches(ctx)
	if err != nil {
		return 0, fmt.Errorf("sandbox: list the bridged-call launches: %w", err)
	}
	if len(launches) == 0 {
		return 0, nil
	}
	runs, err := s.list(ctx, func(PendingRun) bool { return true })
	if err != nil {
		return 0, err
	}
	live := make(map[coord.BridgeLaunch]bool, len(runs))
	for _, run := range runs {
		live[coord.BridgeLaunch{TurnID: run.TurnID, LaunchID: run.LaunchID}] = true
	}
	var purged int64
	var errs []error
	for _, launch := range launches {
		if live[launch] {
			continue
		}
		if err := s.calls.PurgeBridgeCalls(ctx, launch.TurnID, launch.LaunchID); err != nil {
			errs = append(errs, fmt.Errorf("purge the calls of run %s launch %s: %w",
				launch.TurnID, launch.LaunchID, err))
			continue
		}
		purged++
	}
	return purged, errors.Join(errs...)
}

// purgeCalls purges one launch's calls on the lifecycle's own path, where a
// failure is a wait for the sweep rather than an error for the caller.
func (s *CoordStore) purgeCalls(ctx context.Context, turnID, launchID string) {
	if launchID == "" {
		return
	}
	// WITHOUT CANCEL, like every teardown: the caller's context may be the
	// very thing that is ending, and a purge that inherits it does nothing.
	if err := s.calls.PurgeBridgeCalls(context.WithoutCancel(ctx), turnID, launchID); err != nil {
		log.WarnContext(ctx, "sandbox_bridge_calls_purge_failed",
			"turn_id", turnID, "launch_id", launchID, "error", err.Error(),
			"detail", "the maintenance sweep purges the calls of every launch no run names")
	}
}

// rowViewBytes bounds what the run's row may weigh, encoded, once older builds'
// view of its calls is on it.
//
// HALF THE TRANSPORT'S CEILING ([queue.MaxPayloadBytes]), and the other half is
// the point. The row is one message, rewritten whole by every mutation, and the
// mutations that matter are the lifecycle's own — the claim that resumes the
// run, the park on a person's question, the release a failed resume hands back.
// A row the view had filled to the ceiling refuses every one of them, for every
// build: a run no node can claim or finish, beside a billed box nobody
// reclaims. So the view is kept to half, and the fields the lifecycle writes —
// a question, a branch, a suspended conversation — keep the rest.
const rowViewBytes = queue.MaxPayloadBytes / 2

// rowViewFraming is what the view's two fields cost around their contents on
// the encoded row: two keys, the list's brackets and a count, a few dozen
// bytes, which this covers with room.
const rowViewFraming = 128

// fitRowView keeps a row's list of calls within [rowViewBytes], dropping from
// the MIDDLE and counting what it drops, as [appendBounded] does for the count.
//
// Each entry is measured once, by the encoder that writes the row, so one
// pass decides the whole cut. Two calls left over the budget keep the newer,
// because a run ends by submitting and the submission is what an older
// build's resume replays; a lone call heavier than the budget is dropped and
// counted too.
func fitRowView(run PendingRun) ([]BridgeCall, int) {
	calls, elided := run.BridgeCalls, run.BridgeCallsElided
	bare := run
	bare.BridgeCalls, bare.BridgeCallsElided = nil, 0
	base, err := encodeRun(bare)
	if err != nil {
		// encodeRun fails only on a value JSON cannot hold, which the
		// mutation's own encode reports; nothing here can decide by it.
		return calls, elided
	}
	room := rowViewBytes - len(base) - rowViewFraming
	sizes := make([]int, len(calls))
	total := 0
	for i, call := range calls {
		raw, err := json.Marshal(call)
		if err != nil {
			return calls, elided
		}
		sizes[i] = len(raw) + len(",")
		total += sizes[i]
	}
	if total <= room {
		return calls, elided
	}
	kept := slices.Clone(calls)
	for total > room && len(kept) > 0 {
		drop := len(kept) / 2
		if len(kept) == 2 {
			drop = 0
		}
		total -= sizes[drop]
		kept = slices.Delete(kept, drop, drop+1)
		sizes = slices.Delete(sizes, drop, drop+1)
		elided++
	}
	return kept, elided
}

// appendBounded adds one call to the row's list and drops from the MIDDLE past
// [MaxBridgeCalls].
//
// That list is older builds' view (see [MaxBridgeCalls]); the start and the
// end are what explain a run to them, so a list truncated to its last N would
// lose the half a reader most often needs. The count of what was dropped rides
// along on the row.
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
func (s *CoordStore) MarkSuspended(ctx context.Context, turnID string, state map[string]any) (bool, error) {
	_, won, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		if run.Status != StatusLaunching {
			return false
		}
		run.ExecuteState = maps.Clone(state)
		run.Status = StatusRunning
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

// Finish ends a run by deleting its record. See the contract on
// [PendingStore].
//
// A read-decide-delete under the record's version, like every flip here: the
// fence is evaluated against what the store holds, and a lost race re-reads,
// so a claim that moved the lease in between is seen rather than deleted over.
func (s *CoordStore) Finish(ctx context.Context, turnID string, fence Fence) (bool, error) {
	for range casRetries {
		run, version, found, err := s.read(ctx, turnID)
		if err != nil {
			return false, err
		}
		if !found || outranked(run, fence) {
			return false, nil
		}
		gone, err := s.runs.DeleteSandboxRun(ctx, turnID, version)
		if err != nil {
			return false, fmt.Errorf("sandbox: finish run %s: %w", turnID, err)
		}
		if gone {
			// AFTER the record, never before: a run whose calls went
			// first would be one a resume could still claim and find
			// with an empty log.
			s.purgeCalls(ctx, turnID, run.LaunchID)
			return true, nil
		}
	}
	return false, fmt.Errorf("sandbox: finish run %s: the record kept changing under the delete", turnID)
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
