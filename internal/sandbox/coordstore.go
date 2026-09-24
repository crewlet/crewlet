package sandbox

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
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
	runs     coord.SandboxRuns
	awaiting coord.AwaitingRuns
	calls    coord.BridgeCalls
	now      func() time.Time

	// log is where the store's own lines go: the package's logger, unless a
	// suite hands it one to read ([CoordStore.WithLogger]).
	log *slog.Logger
}

// RunRecords is the slice of the fleet's coordination store a [CoordStore]
// reads and writes: the run records, the index of the runs parked on an answer,
// and the log of the calls a bridged run makes. Declared here, by the consumer.
type RunRecords interface {
	coord.SandboxRuns
	coord.AwaitingRuns
	coord.BridgeCalls
}

// NewCoordStore wraps the fleet's run records, their awaiting index and the
// bridged-call log.
func NewCoordStore(records RunRecords) *CoordStore {
	return &CoordStore{runs: records, awaiting: records, calls: records}
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

// WithLogger sends the store's lines to logger, for a suite that asserts about
// what the node says about a call — without pointing the process-wide sink at
// a buffer that every parallel test would then share.
func (s *CoordStore) WithLogger(logger *slog.Logger) *CoordStore {
	s.log = logger
	return s
}

func (s *CoordStore) logger() *slog.Logger {
	if s.log != nil {
		return s.log
	}
	return log
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

// bridgeCallAddressRetries bounds how many numbers one call reserves for its
// record before its append gives up.
//
// ITS OWN BOUND, because this is not a compare-and-swap. A number is passed
// over only when a part or a record is already filed at it, and a launch's
// numbering hands out an address somebody holds in one situation only: the
// launch was purged while a call was still filing under it, so the numbering
// started again at 1, through numbers such late calls hold. A purged launch is
// one no run names and no reader asks for again — every purge follows the row's
// moving off the launch, and a launch id is never reused — so a call that meets
// a stray is itself late, and an exhausted bound fails a record nothing reads.
// Each pass costs a reservation and whatever the call filed at that number
// before meeting the stray, which stays behind for the sweep.
//
// SIXTEEN, and the exact number is not load-bearing: since nothing a reader
// sees depends on it, the bound has only to end a loop that is not converging,
// and sixteen strays in a row is sixteen of the box's own calls landing after
// the purge, ahead of this one in the new numbering.
const bridgeCallAddressRetries = 16

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
	// The row was already there: this run launched before, and this is its
	// next launch. Only the run's own resumed turn launches under its id
	// again — a dispatch, redelivered or not, mints a run id of its own —
	// whether it is a resumed loop calling run_sandbox again or an
	// agent-mode turn's next executor round. Only the LAUNCH-SCOPED state is
	// reset: the identity fields stay the existing row's, and so does the
	// box reference, which the caller is about to reattach to.
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
		// counted, and the company would never be billed for it. The
		// record that a phase record counted the carried spend goes for
		// the same reason: this launch's suspension carries forward only
		// what no record has counted, and a resume told it was counted
		// would report none of it.
		existing.Charged = false
		existing.CarriedCounted = false
		return true
	})
	if err != nil || !reset || replaced == "" {
		return err
	}
	// The replaced launch's calls, and the parts filed under them, are read
	// by nothing now: every reader asks for the run's CURRENT launch.
	// Purged here rather than left for the sweep so a relaunching turn does
	// not carry its previous rounds until it finishes; a purge that fails
	// costs only that wait, since the sweep purges every launch no run
	// names.
	//
	// WHERE THOSE CALLS WENT: into the resumed round's own record. The turn
	// relaunching is the one that collected the replaced launch, and it read
	// every one of that launch's calls whole before re-entering its loop
	// (the engine's resumeBridged); an agent-mode round replays them into
	// the record it publishes when it finishes (runner.resumeAgentRun), and
	// only a later round — after the review — launches again.
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
		run.CarriedCounted = run.CarriedCounted || release.CarriedCounted
		return true
	})
	return released, err
}

// MarkAwaiting parks a run until a person answers.
//
// THE INDEX ENTRY FIRST, then the flip: the index may name a run that is not
// waiting — a reader confirms every entry against the run's record — but it
// must never miss one that is, or the answer is taken for a new turn and the
// run waits for it for good. See [CoordStore.FindAwaitingByConversation].
func (s *CoordStore) MarkAwaiting(ctx context.Context, turnID string, q Clarification) error {
	current, _, found, err := s.read(ctx, turnID)
	if err != nil || !found {
		return err
	}
	if entry, indexed := awaitingEntry(current); indexed {
		if fileErr := s.awaiting.FileAwaitingRun(ctx, entry); fileErr != nil {
			return fmt.Errorf("sandbox: index run %s as awaiting an answer on %s: %w",
				turnID, current.ConversationKey, fileErr)
		}
	}
	_, _, err = s.mutate(ctx, turnID, func(run *PendingRun) bool {
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
// moment before the run ended can still land a record, or parts, under a
// launch nothing reads any more; [CoordStore.SweepBridgeCalls] is what purges
// them.
func (s *CoordStore) AppendBridgeCall(ctx context.Context, turnID string, call BridgeCall) (bool, error) {
	if call.At.IsZero() {
		call.At = s.clock()
	}
	run, _, found, err := s.read(ctx, turnID)
	if err != nil || !found {
		return false, err
	}
	recorded, seq, err := s.record(ctx, run, call)
	if err != nil {
		return false, fmt.Errorf("sandbox: record bridged call %q of run %s: %w", call.Name, turnID, err)
	}
	recorded.Seq = seq

	// OLDER BUILDS' VIEW, and conditional on the SAME launch: a relaunch
	// between the record and this write has a new job on the row, and this
	// call belongs to the one before it.
	_, _, viewErr := s.mutate(ctx, turnID, func(latest *PendingRun) bool {
		if latest.LaunchID != run.LaunchID {
			return false
		}
		latest.BridgeCalls, latest.BridgeCallsElided = appendBounded(
			latest.BridgeCalls, latest.BridgeCallsElided, recorded)
		latest.BridgeCalls, latest.BridgeCallsElided = fitRowView(*latest)
		return true
	})
	if viewErr != nil {
		s.logger().WarnContext(ctx, "sandbox_bridge_row_view_append_failed",
			"turn_id", turnID, "launch_id", run.LaunchID, "seq", seq, "tool", call.Name,
			"error", viewErr.Error(),
			"detail", "the call is recorded in its own record, which every reader in this "+
				"build reads; only an older build reading the run's row will not see it")
	}
	return true, nil
}

// record files one call under the run's current launch, and returns it as its
// record holds it — whole, or cut to its fitted or least form with a reference
// to its parts, or cut and saying its whole was not kept — with the Seq it was
// filed at.
//
// A CALL ITS RECORD CAN HOLD WHOLE is appended as it is: one write, no parts.
// Only a call too large for the ceiling, or one a server below the ceiling
// refused, goes to [CoordStore.recordInParts].
func (s *CoordStore) record(ctx context.Context, run PendingRun, call BridgeCall) (BridgeCall, uint64, error) {
	whole, err := encodeBridgeCall(call)
	if err != nil {
		return BridgeCall{}, 0, err
	}
	refused := false
	if len(whole) <= MaxBridgeCallBytes {
		seq, err := s.calls.AppendBridgeCall(ctx, run.TurnID, run.LaunchID, whole)
		if !errors.Is(err, coord.ErrTooLarge) {
			return call, seq, err
		}
		// REFUSED ALTHOUGH IT FITS THE CEILING, which only a server
		// configured below the contract does: a node does not boot on one,
		// but a reconnect can reach one. No fit to the contract's number
		// answers that, and dropping the call would leave a gap in the one
		// log a resume reads with nothing to say it is there. So its whole
		// goes to parts split small enough for that server, and its record
		// is its LEAST form ([leastBridgeCall]).
		refused = true
		s.refusedWithinCeiling(ctx, run, call, len(whole), err)
	}
	return s.recordInParts(ctx, run, call, whole, refused)
}

// refusedWithinCeiling says that the server refused a record the contract's
// ceiling admits, and what to change.
//
// LOGGED AS IT HAPPENS, whatever becomes of the call afterwards: the refusal is
// a fact about a server's setting that an operator has to change either way,
// and a line that waited for the call's fate would say nothing when the store
// then failed outright. It says nothing about what is filed instead, which is
// [CoordStore.reportFiled]'s to say once a record has landed. Once per call:
// the refusal is carried into every later pass at the call's record
// ([CoordStore.recordInParts]).
func (s *CoordStore) refusedWithinCeiling(ctx context.Context, run PendingRun, call BridgeCall, bytes int, err error) {
	s.logger().ErrorContext(ctx, "sandbox_bridge_call_refused_within_ceiling",
		"turn_id", run.TurnID, "launch_id", run.LaunchID, "tool", call.Name,
		"record_bytes", bytes, "record_limit_bytes", MaxBridgeCallBytes, "error", err.Error(),
		"detail", "the NATS server this node is connected to refused a record the contract's "+
			"ceiling admits, so its max_payload is below queue.MaxPayloadBytes: set max_payload to "+
			"at least queue.MaxPayloadBytes on every server of the cluster")
}

// recordInParts keeps a call's whole in part records filed under the call's
// own record, then files the record: the call cut — its fitted form, or its
// least form once a server has refused a record the ceiling admits — and a
// reference to the parts; or, when the parts could not all be written, the
// call cut and saying its whole was not kept, with NO reference.
//
// A NUMBER OF ITS OWN FIRST, reserved with nothing filed at it, because the
// parts are filed under the call's number and BEFORE its record: a record
// naming parts is never visible without them, and one whose parts failed is
// written without naming any.
//
// AN ADDRESS ALREADY TAKEN — a part or the record at the reserved number — is
// a stray a late write left after this launch was purged. The call takes
// another number rather than share an address with it, at most
// [bridgeCallAddressRetries] times; the stray, and whatever this call filed at
// the number before meeting it, are left for the sweep. A REFUSAL IS CARRIED
// INTO THE NEXT PASS: a server that refused the fitted record at one number
// refuses it at the next, so that pass files the least form without asking
// again, or saying so again.
//
// WHAT THE RECORD HOLDS IS REPORTED ONCE IT HAS LANDED, and only then: a record
// that failed to land holds nothing, and a pass that met a stray is followed by
// one that may keep the whole after all.
func (s *CoordStore) recordInParts(ctx context.Context, run PendingRun, call BridgeCall,
	whole []byte, refused bool,
) (BridgeCall, uint64, error) {
	for range bridgeCallAddressRetries {
		seq, err := s.calls.ReserveBridgeCall(ctx, run.TurnID, run.LaunchID)
		if err != nil {
			return BridgeCall{}, 0, err
		}
		ref := wholeRef{bytes: len(whole)}
		parts, partsErr := fileParts(whole, func(part int, value []byte) (bool, error) {
			return s.calls.CreateBridgeCallPart(ctx, run.TurnID, run.LaunchID, seq, part, value)
		})
		switch {
		case errors.Is(partsErr, errAddressTaken):
			continue
		case partsErr == nil:
			ref.parts = parts
		}
		// A PARTS FAILURE IS NOT THE APPEND'S: the call ran, and its record
		// can still hold the form it is cut to, saying the whole was not kept.
		filed, err := s.fileRecord(ctx, run, call, seq, ref, refused)
		if err != nil {
			if partsErr != nil {
				err = fmt.Errorf("%w (and its whole had not been kept in parts: %w)", err, partsErr)
			}
			return BridgeCall{}, 0, err
		}
		refused = refused || filed.least
		if !filed.created {
			continue
		}
		s.reportFiled(ctx, run, call, seq, ref, filed, parts, partsErr)
		return filed.call, seq, nil
	}
	return BridgeCall{}, 0, fmt.Errorf("every number the launch handed out for the call's "+
		"record was already taken, %d times", bridgeCallAddressRetries)
}

// reportFiled says what a call's record holds, now that it has landed at seq:
// what its fitted form did not keep whole, its least form, and a whole that
// was not kept.
func (s *CoordStore) reportFiled(ctx context.Context, run PendingRun, call BridgeCall, seq uint64,
	ref wholeRef, filed filedRecord, partsWritten int, partsErr error,
) {
	where := "its whole is in the parts filed under it, which a resume reads"
	if !ref.kept() {
		where = "its whole was not kept, and its marks say so"
	}
	switch {
	case filed.least:
		s.logger().WarnContext(ctx, "sandbox_bridge_call_least_form_filed",
			"turn_id", run.TurnID, "launch_id", run.LaunchID, "seq", seq, "tool", call.Name,
			"whole_bytes", ref.bytes, "whole_parts", ref.parts,
			"detail", "a server refused the call's record although the contract's ceiling admits "+
				"it, so the record holds the call's least form: its name and outcome, each text "+
				"longer than its mark set aside and marked; "+where)
	case filed.cut != (bridgeCallCut{}):
		s.logger().WarnContext(ctx, "sandbox_bridge_call_fitted",
			"turn_id", run.TurnID, "launch_id", run.LaunchID, "seq", seq, "tool", call.Name,
			"output_bytes", filed.cut.outputBytes, "args_bytes", filed.cut.argsBytes,
			"whole_bytes", ref.bytes, "whole_parts", ref.parts,
			"record_limit_bytes", MaxBridgeCallBytes,
			"detail", "the call's record could not hold it whole and holds what fits, marked where "+
				"it was cut; "+where)
	}
	if partsErr != nil {
		// THE LOSS IS AN ERROR IN THE LOG rather than a quiet gap: the
		// record says so to every reader of the call, and this says so to
		// the operator, with why.
		s.logger().ErrorContext(ctx, "sandbox_bridge_call_whole_not_kept",
			"turn_id", run.TurnID, "launch_id", run.LaunchID, "seq", seq, "tool", call.Name,
			"whole_bytes", ref.bytes, "parts_written", partsWritten, "error", partsErr.Error(),
			"detail", "the call's record holds the form it was cut to, marked where it was cut, "+
				"and names no parts: the coding agent in the box was handed the whole output, and "+
				"no copy the engine can read back exists. The parts written before the failure are "+
				"named by nothing, and go with the launch's purge")
	}
}

// filedRecord is what [CoordStore.fileRecord] put at a reserved number.
type filedRecord struct {
	// call is the call as its record holds it.
	call BridgeCall

	// cut is what the fitted form did not keep whole: zero for a call it kept
	// whole, and on the least form.
	cut bridgeCallCut

	// least says the record is the call's least form: the server refused a
	// record the contract's ceiling admits, before this number or at it. Set
	// whether or not the least form was created, since the server refuses the
	// fitted record at every number alike.
	least bool

	// created is false when the address already held a record, and nothing
	// was filed.
	created bool
}

// fileRecord files a call's record at a reserved number, reporting false when
// that address is taken: the fitted form with ref, or the least form when the
// server has refused a record within the ceiling — at an earlier pass, or now.
//
// IT LOGS ONLY THE REFUSAL, a fact about the server's setting, and nothing
// about the record, which holds nothing until it is created:
// [CoordStore.reportFiled] reports it once it has landed.
func (s *CoordStore) fileRecord(ctx context.Context, run PendingRun, call BridgeCall, seq uint64,
	ref wholeRef, refused bool,
) (filedRecord, error) {
	if !refused {
		fitted, raw, cut, err := fitBridgeCall(call, ref)
		if err != nil {
			return filedRecord{}, err
		}
		created, err := s.calls.CreateBridgeCall(ctx, run.TurnID, run.LaunchID, seq, raw)
		if !errors.Is(err, coord.ErrTooLarge) {
			return filedRecord{call: fitted, cut: cut, created: created}, err
		}
		s.refusedWithinCeiling(ctx, run, call, len(raw), err)
	}
	least, raw, err := leastBridgeCall(call, ref)
	if err != nil {
		return filedRecord{least: true}, err
	}
	created, err := s.calls.CreateBridgeCall(ctx, run.TurnID, run.LaunchID, seq, raw)
	return filedRecord{call: least, least: true, created: created}, err
}

// errAddressTaken is a part or a record already filed at an address a caller
// took for itself: a stray a late write left after the launch was purged.
var errAddressTaken = errors.New("sandbox: the address already holds a record")

// fileParts files a whole as parts through create, in order from part 1, and
// reports how many it filed. It is how every whole a record cannot hold is kept
// — a bridged call's, and a suspension's — so the two split, refuse and stop
// by one rule.
//
// A PART REFUSED AS TOO LARGE IS RETRIED SMALLER — at half its size, or at
// [partFloorBytes] when half would be smaller — and so is every part after it,
// since the server that refused it will refuse their size too. Only a refused
// part of the floor or smaller ends the split. A part is a byte range of the
// whole's encoding rather than a cut of any text in it: no reader ever decodes
// a part alone, and the reassembled bytes are the whole exactly, so where a
// part boundary falls is invisible to everything that reads the whole.
//
// A PART ALREADY FILED is [errAddressTaken]: create never overwrites, and the
// part there is not this whole's.
func fileParts(whole []byte, create func(part int, value []byte) (bool, error)) (int, error) {
	size, filed := partBytes, 0
	for rest := whole; len(rest) > 0; {
		n := min(size, len(rest))
		created, err := create(filed+1, rest[:n])
		switch {
		case errors.Is(err, coord.ErrTooLarge):
			if n <= partFloorBytes {
				return filed, fmt.Errorf("a part of %d bytes was refused as too large, and a refused "+
					"part is not split below %d bytes: %w", n, partFloorBytes, err)
			}
			size = max((n+1)/2, partFloorBytes)
			continue
		case err != nil:
			return filed, err
		case !created:
			return filed, errAddressTaken
		}
		filed++
		rest = rest[n:]
	}
	return filed, nil
}

// wholeRef is where a call's whole is, when its record cannot hold it.
type wholeRef struct {
	// bytes is the length of the call's record encoded whole.
	bytes int

	// parts is how many part records hold those bytes, zero when they could
	// not all be written: the whole is then kept nowhere.
	parts int

	// refused says the record it goes with is the least form, filed because
	// a server refused a record the contract's ceiling admits: what that
	// record sets aside is set aside for that reason, not for its size, and
	// its marks say so.
	refused bool
}

func (w wholeRef) kept() bool { return w.parts > 0 }

// argsMark is what stands in a record for arguments it sets aside.
func (w wholeRef) argsMark(bytes int) string {
	switch {
	case w.refused && w.kept():
		return RefusedArgsInParts(bytes)
	case w.refused:
		return RefusedArgsNotKept(bytes)
	case w.kept():
		return ArgsInParts(bytes)
	}
	return ArgsNotKept(bytes)
}

// outputNote is what follows a cut output's mark: nothing when the whole is in
// the parts, whose reference says so, and a note saying it is nowhere when it
// is not ([WholeNotKept], [RefusedWholeNotKept]).
func (w wholeRef) outputNote() string {
	switch {
	case w.kept():
		return ""
	case w.refused:
		return RefusedWholeNotKept(w.bytes)
	}
	return WholeNotKept(w.bytes)
}

// wholeUnreadable is the note a call read back in its record's cut form ends
// with when its record names parts and they do not reassemble into it.
//
// WORDED AS WHAT HAPPENED, for the reason [ArgsInParts] is: it is carried into
// the resumed phase's record, which outlives the parts.
func wholeUnreadable(call BridgeCall, why string) string {
	return fmt.Sprintf("\n[the whole of this call, %d bytes filed in %d parts, could not be read "+
		"back: %s; this call is shown as its record kept it]", call.WholeBytes, call.WholeParts, why)
}

// fitBridgeCall encodes a call as its record, fitted to [MaxBridgeCallBytes],
// with whole's reference when the parts hold it, and reports what it did not
// keep whole.
//
// MEASURED ON THE ENCODED BYTES, not on the output's length: JSON escapes a
// control character to six bytes, so what a string costs in the record is a
// property of its bytes rather than of its length. HTML escaping is off, so
// '<', '>' and '&' — which a tool's output is full of — cost one byte each
// rather than six; the result is still JSON every decoder reads. The
// reference is set BEFORE anything is measured, so the room it takes is
// counted like any other field's.
//
// THE ARGUMENTS ARE DECIDED FIRST, against an output of nothing but its mark
// and note: the most room the output can ever give back. Over the ceiling even
// then, they are replaced by their marker ([wholeRef.argsMark]); otherwise they
// stay whole. Deciding them after the output instead would cut the output for
// room the arguments were about to give back, and a small output — "created
// 123" beside a nine-megabyte page body — would be lost although the record
// had room for it.
//
// THE OUTPUT IS THEN FIT FROM THE WHOLE OF IT, cut to a budget that shrinks by
// the encoded excess on every pass until the record fits. No character encodes
// to fewer bytes than it has, so a budget cut by the excess removes at least
// the excess from the record, and what a pass adds back — the mark, and the
// note when the whole is not kept — is measured by the next. What is left when
// the output is down to its mark is the tool's name, which is one the seat's
// surface registered; the last check stays so that the size of what this
// returns is a guarantee rather than an assumption about names, and it refuses
// rather than cuts, because a cut name is a different tool.
func fitBridgeCall(call BridgeCall, whole wholeRef) (BridgeCall, []byte, bridgeCallCut, error) {
	if whole.kept() {
		call.WholeBytes, call.WholeParts = whole.bytes, whole.parts
	}
	note := whole.outputNote()
	raw, err := encodeBridgeCall(call)
	if err != nil {
		return BridgeCall{}, nil, bridgeCallCut{}, err
	}
	var cut bridgeCallCut
	if call.Args != "" {
		probe := call
		if probe.Output != "" {
			probe.Output = bridgeCallCutMarker + note
		}
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		least, err := encodeBridgeCall(probe)
		if err != nil {
			return BridgeCall{}, nil, bridgeCallCut{}, err
		}
		if len(least) > MaxBridgeCallBytes {
			cut.argsBytes = len(call.Args)
			call.Args = whole.argsMark(len(call.Args))
			if raw, err = encodeBridgeCall(call); err != nil {
				return BridgeCall{}, nil, bridgeCallCut{}, err
			}
		}
	}
	output := call.Output
	for budget := len(output); len(raw) > MaxBridgeCallBytes && budget > 0; {
		budget -= len(raw) - MaxBridgeCallBytes
		call.Output = textcut.Within(output, budget) + note
		cut.outputBytes = len(output)
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

// leastBridgeCall is the smallest record a call is kept as, filed once a
// server has refused a record the contract's ceiling admits: its name and its
// outcome, beside whole's reference when the parts hold it, and each text
// LONGER THAN ITS MARK set aside for that mark. The arguments' mark is the
// refusal's own marker — [RefusedArgsInParts], or [RefusedArgsNotKept] when
// the whole was not kept. The output's is the cut's "…", followed, when the
// whole was not kept, by the refusal's own note ([RefusedWholeNotKept]).
//
// A TEXT NO LONGER THAN ITS MARK IS KEPT AS IT IS. Its mark would weigh as
// much and say less, and would claim it was set aside; arguments of a few
// bytes set aside with a whole that was not kept would be lost for nothing.
// Measured in the texts' own bytes, since the arguments and their marker are
// both JSON text encoded into one string field, and the least form is sized
// against no target that a byte would decide.
//
// A mark only where something was set aside, so a reader can still tell a
// call made with nothing from one whose texts were not kept here.
func leastBridgeCall(call BridgeCall, whole wholeRef) (BridgeCall, []byte, error) {
	whole.refused = true
	if whole.kept() {
		call.WholeBytes, call.WholeParts = whole.bytes, whole.parts
	}
	if mark := whole.argsMark(len(call.Args)); len(call.Args) > len(mark) {
		call.Args = mark
	}
	if mark := bridgeCallCutMarker + whole.outputNote(); len(call.Output) > len(mark) {
		call.Output = mark
	}
	raw, err := encodeBridgeCall(call)
	return call, raw, err
}

// bridgeCallCut is what [fitBridgeCall] did not keep whole in the record: the
// whole length of an output it cut and of arguments it replaced, zero for each
// it kept. For the log line that says so; the record's own fields carry the
// marks.
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
	calls := make([]BridgeCall, 0, len(records))
	for _, record := range records {
		call, err := decodeBridgeCall(record)
		if err != nil {
			return BridgeLog{}, err
		}
		if call.WholeParts > 0 {
			call = s.whole(ctx, run, call, record.Parts)
		}
		calls = append(calls, call)
	}
	return BridgeLog{Calls: calls}, nil
}

// whole reassembles a call from the parts filed under its record, or answers
// the record's cut form with a note saying why it could not.
//
// THE PARTS ARE CHECKED AGAINST THE REFERENCE before any is believed: every
// part the record names, in order, adding up to exactly the length it names,
// decoding to a call, and to THIS call — its name and its instant. A part
// missing (purged under a read, lost) or a whole that disagrees with its
// record would otherwise be handed to the resume as the call itself, shorter
// or other than what the run did, with nothing to say so. What is handed over
// instead is the record's cut form, which is honest about being cut
// ([unreadable]). A part past the count the record names belongs to no
// reference and is not read.
func (s *CoordStore) whole(ctx context.Context, run PendingRun, cut BridgeCall, parts []coord.Part) BridgeCall {
	whole, why := reassemble(cut, parts)
	if why == "" {
		return whole
	}
	s.logger().ErrorContext(ctx, "sandbox_bridge_call_whole_unreadable",
		"turn_id", run.TurnID, "launch_id", run.LaunchID, "seq", cut.Seq, "tool", cut.Name,
		"whole_bytes", cut.WholeBytes, "whole_parts", cut.WholeParts, "parts_found", len(parts),
		"reason", why,
		"detail", "the call is read back in the form its record was cut to: arguments the record set "+
			"aside are marked as not read back, and its output ends in a note saying why")
	return unreadable(cut, why)
}

// unreadable is a call's cut form — fitted or least — as the whole-log read
// hands it over when the parts it was filed in do not reassemble into it.
//
// NOTHING IN IT POINTS AT THE PARTS AS WHERE THE REST IS. Those parts are
// exactly what could not be read, while this form is what the resume carries
// into the phase's record and the reviewer's evidence, and the run's end
// purges the parts. So the record's marker saying its arguments went into the
// parts gives way to the unreadable one, with the same byte count
// ([ArgsUnreadable], [RefusedArgsUnreadable]); the reference is CLEARED, once
// the note has taken its length and count; and the output ends in that note,
// saying why. Arguments the record kept whole are left exactly as they are.
func unreadable(cut BridgeCall, why string) BridgeCall {
	if n, refused, ok := argsInPartsBytes(cut.Args); ok {
		cut.Args = ArgsUnreadable(n)
		if refused {
			cut.Args = RefusedArgsUnreadable(n)
		}
	}
	note := wholeUnreadable(cut, why)
	if cut.Output == "" {
		// A call that returned nothing has no text for the note to follow.
		note = strings.TrimPrefix(note, "\n")
	}
	cut.Output += note
	cut.WholeBytes, cut.WholeParts = 0, 0
	return cut
}

// reassemble is [CoordStore.whole]'s check and join, answering the reason in
// place of the call when the parts do not make the whole the record names.
func reassemble(cut BridgeCall, parts []coord.Part) (BridgeCall, string) {
	joined, why := joinParts(parts, cut.WholeParts, cut.WholeBytes)
	if why != "" {
		return BridgeCall{}, why
	}
	var whole BridgeCall
	if err := json.Unmarshal(joined, &whole); err != nil {
		return BridgeCall{}, "its parts do not decode as a call: " + err.Error()
	}
	if whole.Name != cut.Name || !whole.At.Equal(cut.At) {
		return BridgeCall{}, fmt.Sprintf("its parts hold a different call, %q at %s",
			whole.Name, whole.At.Format(time.RFC3339Nano))
	}
	// The KEY is the call's place, as for every record.
	whole.Seq = cut.Seq
	return whole, ""
}

// joinParts joins the first count parts into the whole a record names, or
// answers why they do not make it: a part missing or out of order, or a length
// other than size. The parts are checked against the record's reference before
// any byte is believed, and a part past count belongs to no reference and is
// not read — the one check and join every whole kept in parts is read back by.
func joinParts(parts []coord.Part, count, size int) ([]byte, string) {
	if count < 1 {
		return nil, fmt.Sprintf("its record names %d parts", count)
	}
	held := 0
	for i := range count {
		if i >= len(parts) || parts[i].Part != i+1 {
			return nil, fmt.Sprintf("part %d of %d is missing", i+1, count)
		}
		held += len(parts[i].Value)
	}
	if held != size {
		return nil, fmt.Sprintf("its parts hold %d bytes, not %d", held, size)
	}
	joined := make([]byte, 0, held)
	for _, part := range parts[:count] {
		joined = append(joined, part.Value...)
	}
	return joined, ""
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
	// The row's list, for a launch an older build recorded: ONE PAGE,
	// whatever the limit (see the contract). It hands out no cursor, so a
	// page asked for past the first has nothing to carry.
	log := rowLog(run)
	out := BridgeCallPage{Calls: []BridgeCall{}, Total: len(log.Calls)}
	if after > 0 {
		return out, nil
	}
	out.Calls = append(out.Calls, log.Calls...)
	out.Dropped, out.DroppedAfter = log.Dropped, log.DroppedAfter
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
	cursor := out.Calls[len(out.Calls)-1].Seq
	if after != 0 {
		out.Next = cursor
		return out, true, nil
	}
	// THE END OF THE LOG, on the first page that does not reach it: the
	// newest calls past this page, read back from the newest.
	q.After, q.Last = cursor, true
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
		// The cursor only when something is left to page to: an End that
		// reaches back to the page leaves nothing, and a cursor there would
		// send a reader to fetch End's calls a second time.
		out.Next = cursor
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

// SweepBridgeCalls purges the call log of every launch no run names — its
// records, the parts filed under them, its numbering — and reports how many
// launches it purged.
//
// What the lifecycle's own purges can miss: a run finished on a node that died
// before its purge, a purge that failed, a late call that landed after one —
// its record, or only some of its parts — and a run an older build finished
// (it knows nothing of the records). Each leaves keys under a launch no reader
// will ever ask for.
//
// THE LAUNCHES ARE LISTED BEFORE THE RUNS, and the order is the whole of its
// safety. A launch's first key — its numbering, a part or a record — is filed
// only after its row names it, since an append reads the row for the launch it
// files under, so a launch listed first was on a row before the listing — and
// if the rows read
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
		s.logger().WarnContext(ctx, "sandbox_bridge_calls_purge_failed",
			"turn_id", turnID, "launch_id", launchID, "error", err.Error(),
			"detail", "the maintenance sweep purges the calls of every launch no run names")
	}
}

// rowBytes bounds what the run's row may weigh, encoded, with what the store
// decides to keep on it: its suspended conversation, and older builds' view of
// its calls.
//
// HALF THE TRANSPORT'S CEILING ([queue.MaxPayloadBytes]), and the other half is
// the point. The row is one message, rewritten whole by every mutation, and the
// mutations that matter are the lifecycle's own — the claim that resumes the
// run, the park on a person's question, the release a failed resume hands back
// — and a park adds a question no fit bounds. A row the store had filled to
// the ceiling refuses every one of them, for every build: a run no node can
// claim or finish, beside a billed box nobody reclaims. So a suspension goes
// on the row only while the row holding it is within half the ceiling
// ([withinRow]), the view is fitted into what that leaves ([fitRowView]), and
// the other half is the lifecycle's.
const rowBytes = queue.MaxPayloadBytes / 2

// rowViewFraming is what the view's two fields cost around their contents on
// the encoded row: two keys, the list's brackets and a count, a few dozen
// bytes, which this covers with room.
const rowViewFraming = 128

// withinRow reports whether a run's row, without older builds' view of its
// calls, is within [rowBytes]: whether it can hold what it carries and leave
// the view room to be fitted into.
func withinRow(run PendingRun) bool {
	bare := run
	bare.BridgeCalls, bare.BridgeCallsElided = nil, 0
	raw, err := encodeRun(bare)
	// An encode that fails is reported by the write that encodes the row;
	// what it says here is only that the row cannot be shown to fit.
	return err == nil && len(raw) <= rowBytes
}

// fitRowView keeps a row's list of calls within [rowBytes], dropping from
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
	room := rowBytes - len(base) - rowViewFraming
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
//
// ON THE ROW WHEN IT FITS ([withinRow]): one write, and the one form every
// build can resume. A suspension the row cannot hold — past [rowBytes], or
// refused by a server set below the contract's ceiling — keeps its whole in
// parts filed under the run's launch ([CoordStore.suspendInParts]).
func (s *CoordStore) MarkSuspended(ctx context.Context, turnID string, state map[string]any) (bool, error) {
	var launch string
	overRow := false
	_, won, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		overRow = false
		if run.Status != StatusLaunching {
			return false
		}
		launch = run.LaunchID
		run.ExecuteState = maps.Clone(state)
		if !withinRow(*run) {
			overRow = true
			return false
		}
		run.Status = StatusRunning
		run.BridgeCalls, run.BridgeCallsElided = fitRowView(*run)
		return true
	})
	switch {
	case errors.Is(err, coord.ErrTooLarge):
		// REFUSED ALTHOUGH IT IS WITHIN THE ROW'S BUDGET, which only a server
		// configured below the contract does: a node does not boot on one,
		// but a reconnect can reach one. Its parts are split to what that
		// server takes.
		s.logger().ErrorContext(ctx, "sandbox_suspension_refused_within_ceiling",
			"turn_id", turnID, "launch_id", launch, "row_limit_bytes", rowBytes, "error", err.Error(),
			"detail", "the NATS server this node is connected to refused a run record the contract's "+
				"ceiling admits, so its max_payload is below queue.MaxPayloadBytes: set max_payload to "+
				"at least queue.MaxPayloadBytes on every server of the cluster. The suspended "+
				"conversation is kept in parts that server takes instead")
		return s.suspendInParts(ctx, turnID, launch, state, "a server refused the run's record holding it")
	case err != nil:
		return false, err
	case overRow:
		return s.suspendInParts(ctx, turnID, launch, state, fmt.Sprintf(
			"the run's record holding it is past the %d bytes a record keeps a suspension within", rowBytes))
	}
	return won, nil
}

// suspendInParts keeps a suspended conversation's whole in parts filed under
// the run's launch, then writes the row that names them and opens the run to
// the poll, in one write.
//
// THE PARTS FIRST, so a row naming them is never visible without them — the
// completion poll fires the moment the row says running. The row then holds
// the reference in execute_state ([suspensionInPartsKey]), because that is the
// one place a build that predates suspension parts carries whole through every
// write it makes to the row and refuses to resume from, rather than dropping
// it or reading it as no conversation at all.
//
// A WHOLE THAT CANNOT BE KEPT IS AN ERROR, and the run is left launching: the
// caller fails a run whose suspension has nowhere to go ([PendingStore.
// MarkSuspended]). Its error says why the row could not hold the suspension
// (why) and names the limit that refused its parts. Parts filed before a
// failure are named by nothing, and go with the purge of the launch when the
// run ends.
func (s *CoordStore) suspendInParts(ctx context.Context, turnID, launch string, state map[string]any, why string) (bool, error) {
	whole, err := json.Marshal(state)
	if err != nil {
		return false, fmt.Errorf("sandbox: encode the suspended conversation of run %s: %w", turnID, err)
	}
	parts, err := fileParts(whole, func(part int, value []byte) (bool, error) {
		return s.calls.CreateSuspensionPart(ctx, turnID, launch, part, value)
	})
	if err != nil {
		return false, fmt.Errorf("sandbox: the suspended conversation of run %s, %d bytes, could not be "+
			"kept: %s, and its parts could not all be filed (%d were): %w",
			turnID, len(whole), why, parts, err)
	}
	ref := suspensionRef{Bytes: len(whole), Parts: parts}
	_, won, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		// THE SAME LAUNCH, still launching: the parts are that launch's,
		// and a row that has moved on is left as it stands.
		if run.Status != StatusLaunching || run.LaunchID != launch {
			return false
		}
		run.ExecuteState = ref.stub()
		run.Status = StatusRunning
		run.BridgeCalls, run.BridgeCallsElided = fitRowView(*run)
		return true
	})
	if err != nil {
		return false, fmt.Errorf("sandbox: record the suspended conversation of run %s, kept in %d "+
			"parts: %w", turnID, parts, err)
	}
	if won {
		s.logger().InfoContext(ctx, "sandbox_suspension_kept_in_parts",
			"turn_id", turnID, "launch_id", launch, "suspension_bytes", ref.Bytes,
			"suspension_parts", ref.Parts, "row_limit_bytes", rowBytes,
			"detail", "the run's record names the parts its suspended conversation is kept in, and "+
				"the resume reads it back whole; a build that predates suspension parts hands the "+
				"resume back for a build that reads them")
	}
	return won, nil
}

// suspensionInPartsKey is the ONE key of the execute_state a run's row holds
// in place of a suspended conversation whose whole is kept in parts: its value
// is the [suspensionRef].
//
// EXACTLY ONE KEY, and not one an Execute state has: the agent layer's encoding
// always carries its version beside the conversation, so a map holding this
// key alone is this reference and nothing else. A build that predates the
// reference reads the map as a state of version zero, which no build writes,
// and hands the resume back for a node that can read it — where an empty
// execute_state would have been failed as a run with no conversation.
const suspensionInPartsKey = "suspension_in_parts"

// suspensionRef is where a suspension is, when its run's row cannot hold it:
// how long its whole is, encoded, and how many parts under the run's launch
// hold those bytes.
type suspensionRef struct {
	Bytes int `json:"bytes"`
	Parts int `json:"parts"`
}

// stub is the execute_state a row holds for this reference.
func (r suspensionRef) stub() map[string]any {
	return map[string]any{suspensionInPartsKey: map[string]any{"bytes": r.Bytes, "parts": r.Parts}}
}

// suspensionRefOf reports whether a row's execute_state is a reference to a
// suspension kept in parts, and the reference. A reference it cannot decode is
// still one — the row's suspension is in parts it cannot name — and comes back
// zero, which no whole was kept as.
func suspensionRefOf(state map[string]any) (suspensionRef, bool) {
	value, ok := state[suspensionInPartsKey]
	if !ok || len(state) != 1 {
		return suspensionRef{}, false
	}
	var ref suspensionRef
	raw, err := json.Marshal(value)
	if err != nil || json.Unmarshal(raw, &ref) != nil {
		return suspensionRef{}, true
	}
	return ref, true
}

// Suspension returns a run's suspended conversation whole. See the contract on
// [PendingStore].
func (s *CoordStore) Suspension(ctx context.Context, run PendingRun) (map[string]any, error) {
	ref, inParts := suspensionRefOf(run.ExecuteState)
	if !inParts {
		return run.ExecuteState, nil
	}
	if run.LaunchID == "" {
		// A reference with no launch names parts no address can hold: no
		// write files a reference without the launch its parts are under.
		// Refused here as the permanent fault it is, because asked of the
		// store it would be refused as an address and read as a failed
		// read, which the resume hands back for a retry that meets the
		// same refusal every time.
		return nil, fmt.Errorf("%w: run %s names no launch its parts could be filed under",
			ErrSuspensionUnreadable, run.TurnID)
	}
	parts, err := s.calls.SuspensionParts(ctx, run.TurnID, run.LaunchID)
	if err != nil {
		return nil, fmt.Errorf("sandbox: read the suspended conversation of run %s: %w", run.TurnID, err)
	}
	whole, why := joinParts(parts, ref.Parts, ref.Bytes)
	if why != "" {
		return nil, fmt.Errorf("%w: run %s's suspended conversation, %d bytes kept in %d parts: %s",
			ErrSuspensionUnreadable, run.TurnID, ref.Bytes, ref.Parts, why)
	}
	var state map[string]any
	if err := json.Unmarshal(whole, &state); err != nil {
		return nil, fmt.Errorf("%w: run %s's suspended conversation does not decode from its %d parts: %w",
			ErrSuspensionUnreadable, run.TurnID, ref.Parts, err)
	}
	return state, nil
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
// ONE KEYED READ OF THE INDEX, and a read of each run it names — not a read of
// every run the fleet holds, which this is asked on every delivery a seat
// receives on a conversation ([coord.AwaitingRuns] says why the index exists).
//
// EVERY ENTRY IS CONFIRMED against the run's own record, because an entry is a
// hint: it is filed before the park and dropped after the finish, so it can
// name a run that was claimed, relaunched or ended since. Only a run of this
// seat, on this conversation, in one of the [Awaiting] statuses is an answer's
// run, and an entry whose run is gone is dropped here — the finish that should
// have dropped it failed.
//
// A RUN AN OLDER BUILD PARKED HAS NO ENTRY: a build that predates the index
// flips the row and files nothing. The entry is filed when a build that has
// the index lists the run — the node that acquires the seat does, before the
// seat's mailbox opens, and so does the completion poll on every tick
// ([CoordStore.IndexAwaiting]). An answer that reaches a seat before either
// has listed its run is not matched to it.
func (s *CoordStore) FindAwaitingByConversation(ctx context.Context, handle, conversation string) (PendingRun, bool, error) {
	if handle == "" || conversation == "" {
		return PendingRun{}, false, nil
	}
	turnIDs, err := s.awaiting.AwaitingRunsOn(ctx, handle, conversation)
	if err != nil {
		return PendingRun{}, false, fmt.Errorf("sandbox: read the runs awaiting an answer on %s: %w",
			conversation, err)
	}
	var waiting []PendingRun
	for _, turnID := range turnIDs {
		run, _, found, err := s.read(ctx, turnID)
		if err != nil {
			return PendingRun{}, false, err
		}
		if !found {
			s.dropAwaiting(ctx, coord.AwaitingRun{Handle: handle, Conversation: conversation, TurnID: turnID})
			continue
		}
		if run.AgentHandle == handle && run.ConversationKey == conversation &&
			slices.Contains(Awaiting, run.Status) {
			waiting = append(waiting, run)
		}
	}
	if len(waiting) == 0 {
		return PendingRun{}, false, nil
	}
	// Newest first: a seat can have parked more than one question on one
	// thread, and the answer belongs to what the person was just asked. The
	// order is the listings' own, oldest first.
	slices.SortStableFunc(waiting, olderFirst)
	return waiting[len(waiting)-1], true, nil
}

// IndexAwaiting brings the index of runs parked on an answer into line with a
// listing of every active run of one seat, or of every seat when handle is
// empty. See [PendingStore.IndexAwaiting].
func (s *CoordStore) IndexAwaiting(ctx context.Context, handle string, runs []PendingRun) error {
	filed, err := s.awaiting.AllAwaitingRuns(ctx)
	if err != nil {
		return fmt.Errorf("sandbox: read the runs awaiting an answer: %w", err)
	}
	have := make(map[coord.AwaitingRun]bool, len(filed))
	for _, entry := range filed {
		have[entry] = true
	}
	listed := make(map[string]bool, len(runs))
	var errs []error
	for _, run := range runs {
		listed[run.TurnID] = true
		entry, indexed := awaitingEntry(run)
		if !indexed || !slices.Contains(Awaiting, run.Status) || have[entry] {
			continue
		}
		// A run parked by a build that files no entry, or by a park whose
		// entry was lost: without one no answer finds it.
		if err := s.awaiting.FileAwaitingRun(ctx, entry); err != nil {
			errs = append(errs, fmt.Errorf("sandbox: index run %s as awaiting an answer: %w", run.TurnID, err))
			continue
		}
		s.logger().InfoContext(ctx, "sandbox_awaiting_run_indexed", "turn_id", run.TurnID,
			"agent", run.AgentHandle, "conversation_key", run.ConversationKey,
			"detail", "a run parked on a question had no entry in the index an answer is "+
				"matched through, which is how a build that predates the index parks one; "+
				"it has one now")
	}
	for _, entry := range filed {
		if (handle != "" && entry.Handle != handle) || listed[entry.TurnID] {
			continue
		}
		// NAMED BY NO RUN THE LISTING HOLDS, and the listing is a snapshot:
		// the run is read again, and only one that is gone loses its entry.
		_, _, found, err := s.read(ctx, entry.TurnID)
		switch {
		case err != nil:
			errs = append(errs, err)
		case !found:
			if err := s.awaiting.DropAwaitingRun(ctx, entry); err != nil {
				errs = append(errs, fmt.Errorf("sandbox: drop the index entry of ended run %s: %w",
					entry.TurnID, err))
			}
		}
	}
	return errors.Join(errs...)
}

// awaitingEntry is the index entry a run is filed under while it waits on an
// answer, and false for a run no answer can be matched to: one naming no seat
// or no conversation.
func awaitingEntry(run PendingRun) (coord.AwaitingRun, bool) {
	if run.AgentHandle == "" || run.ConversationKey == "" {
		return coord.AwaitingRun{}, false
	}
	return coord.AwaitingRun{Handle: run.AgentHandle, Conversation: run.ConversationKey, TurnID: run.TurnID}, true
}

// dropAwaiting drops a run's index entry, best effort: an entry left behind
// names a run that is gone, which every reader confirms before acting, and
// the next [CoordStore.IndexAwaiting] over its seat drops it.
func (s *CoordStore) dropAwaiting(ctx context.Context, entry coord.AwaitingRun) {
	if err := s.awaiting.DropAwaitingRun(ctx, entry); err != nil {
		s.logger().WarnContext(ctx, "sandbox_awaiting_entry_not_dropped", "turn_id", entry.TurnID,
			"agent", entry.Handle, "error", err.Error(),
			"detail", "the index names a run that has ended; readers skip it, and the next "+
				"listing of its seat drops it")
	}
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
			// with an empty log. The purge takes every part filed under
			// the calls with them; where the calls themselves went — a
			// resumed phase's record, or nowhere — is each caller's to
			// say (see [PendingStore.Finish]).
			s.purgeCalls(ctx, turnID, run.LaunchID)
			// The index entry after the record too, for the index's own
			// rule: an entry may outlive its run, never the reverse.
			if entry, indexed := awaitingEntry(run); indexed {
				s.dropAwaiting(ctx, entry)
			}
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
// read — the seat and the status. The set is bounded by the seats that can be
// mid-run at once, which is what makes one read and a local filter the right
// shape for a pass that runs per poll or per seat acquired. It is the wrong
// shape for a question asked per DELIVERY, which is why an answer is matched
// through an index instead ([CoordStore.FindAwaitingByConversation]).
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
	slices.SortStableFunc(out, olderFirst)
	return out, nil
}

// olderFirst orders runs by when they were launched, and by turn id among runs
// launched at one instant.
func olderFirst(a, b PendingRun) int {
	return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.TurnID, b.TurnID))
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
