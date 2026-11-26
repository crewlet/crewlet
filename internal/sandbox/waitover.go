package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
)

// THE ENGINE'S ANSWER IS NEVER RECORDED AS AN ANSWER. A run parked on what the
// engine answers ([PendingRun.EngineAnswers]) is carried on by its seat's holder
// FROM THE WAITING ROW: woken by its own type ([types.ExecutorWaitOver]), it
// rebuilds how the waited run left ([CoordinatorOptions.WaitedFor]) or which
// calls returned, and claims the run out of waiting ([PendingStore.ClaimWaitOver]).
// Recorded as an answer by turn, a build that predates it dropped the keys saying
// so and told the run a person had answered; now that build drops the wake and
// finds no answer on the row. The claim keeps its own take and revival
// ([WaitOver]): a holder stopping between claim and turn leaves the run claimable,
// one whose turn took it never runs it twice. The watch wakes the run again until
// it moves on, so nothing here retries.

// A MIXED FLEET'S ONE COST, ACCEPTED. A holder stopping after its claim
// ([PendingStore.ClaimWaitOver]) and before its turn takes it
// ([PendingStore.TakeWaitOver]) leaves the row `resumed` with no answer. A next
// holder on this build revives the claim, so a fleet wholly on it loses no run;
// one on a build without the carrying on reads the row as a finished run's
// tail, and its reaper ends the run as abandoned
// ([types.ExecutorFailureAbandoned]): announced, never silent. NOT GATED:
// internal/coord's ProtocolVersion stops only a newer node beside a live lower
// lease, and a lower build takes a lapsed lease over unchecked, so a bump would
// not close the window and would only cost the upgrade path.

// WaitOver is the claim of a run the engine carried on, its wait over
// ([PendingStore.ClaimWaitOver]): what the claim took it out of, whether its
// turn took it, and how many such claims died before one did. ITS OWN KEY,
// never [RecordedAnswer], for the reason the package doc's section above gives.
// Cleared with the question.
//
// A SECTION WITH AN EXTRA OF ITS OWN (wirekeys.go).
type WaitOver struct {
	// From is the status the claim took the run out of, one of [Awaiting],
	// which a revival restores.
	From string `json:"from"`

	// Job is the job whose wait the claim carried on ([PendingRun.LaunchID]). A
	// build without the claim keeps it in [PendingRun.Extra] through a relaunch
	// naming a new job, and a claim naming another job is read as none
	// ([PendingRun.forgetOtherJobs]): else a later job's completion, claimed,
	// would read as this claim and be revived to waiting.
	Job string `json:"job"`

	// TakenAt is when the resumed turn took the claim ([PendingStore.TakeWaitOver]),
	// zero until then and once a release gives the claim back: a claim whose
	// node stopped before it is revived, one after it is reaped as used.
	TakenAt time.Time `json:"taken_at,omitzero"`

	// LostClaims, FirstLostAt and LostOn count the claims that died before
	// their turn took them, as [RecordedAnswer.LostClaims] does, bounding the
	// revivals by [MaxAnswerRevivals] and the run's awaiting window.
	LostClaims  int        `json:"lost_claims,omitempty"`
	FirstLostAt time.Time  `json:"first_lost_at,omitzero"`
	LostOn      *RowWindow `json:"lost_on,omitempty"`

	// Extra is every key of the section this build does not know: see
	// [RunSteer.Extra].
	Extra map[string]json.RawMessage `json:"-"`
}

// Taken reports whether a turn took the claim.
func (w *WaitOver) Taken() bool { return w != nil && !w.TakenAt.IsZero() }

// MarshalJSON writes the section with the keys a later build added to it.
func (w WaitOver) MarshalJSON() ([]byte, error) {
	type plain WaitOver
	return marshalCarrying(plain(w), wireKeys(reflect.TypeFor[WaitOver]()), w.Extra)
}

// UnmarshalJSON reads the section and keeps the keys this build does not know.
func (w *WaitOver) UnmarshalJSON(raw []byte) error {
	type plain WaitOver
	var p plain
	extra, err := unmarshalCarrying(raw, &p, wireKeys(reflect.TypeFor[WaitOver]()))
	if err != nil {
		return err
	}
	p.Extra = extra
	*w = WaitOver(p)
	return nil
}

// ErrWaitOverTaken is a revival refused because a resumed turn took the claim
// ([WaitOver.TakenAt]): the run was carried on once, and is not again.
var ErrWaitOverTaken = errors.New("sandbox: a resumed turn already took the run's carrying on")

// waitsForEngine reports whether run waits, unanswered, on what the engine
// answers: the one state [PendingStore.ClaimWaitOver] claims out of.
func waitsForEngine(run PendingRun) bool {
	return run.EngineAnswers() && run.Answer == nil && run.Ending == nil &&
		slices.Contains(Awaiting, run.Status)
}

// forgetOtherJobs drops what r holds of a job it no longer runs, its parked
// calls ([CallWait.Job]) and its wait's claim ([WaitOver.Job]): a build
// without them carried them through its relaunch, unread. Every row this build
// reads goes through it ([decodeRun]), and its next write drops them.
func (r *PendingRun) forgetOtherJobs() {
	r.ParkedCalls = slices.DeleteFunc(r.ParkedCalls, func(c CallWait) bool { return c.Job != r.LaunchID })
	if len(r.ParkedCalls) == 0 {
		r.ParkedCalls = nil
	}
	if r.WaitOver != nil && r.WaitOver.Job != r.LaunchID {
		r.WaitOver = nil
	}
}

// claimedWaitOver reports whether a claimed run is the engine's carrying on.
func claimedWaitOver(run PendingRun) bool {
	return run.Status == StatusResumed && run.Answer == nil && run.WaitOver != nil
}

// ClaimWaitOver claims a run waiting on what the engine answers out of
// waiting. See the contract on [PendingStore].
func (s *CoordStore) ClaimWaitOver(ctx context.Context, turnID, launch string, fence Fence,
) (PendingRun, bool, error) {
	var before string
	now := s.clock()
	run, won, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		if run.LaunchID != launch || !waitsForEngine(*run) || superseded(*run, fence) || run.Held(now) {
			return false
		}
		before = run.Status
		over := WaitOver{From: before, Job: launch}
		if run.WaitOver != nil {
			over.LostClaims, over.FirstLostAt = run.WaitOver.LostClaims, run.WaitOver.FirstLostAt
			over.LostOn = run.WaitOver.LostOn
		}
		run.WaitOver = &over
		run.Status = StatusResumed
		if fence.Fenced() {
			run.Owner, run.OwnerEpoch = fence.Owner, fence.Epoch
		}
		return true
	})
	if err != nil || !won {
		return PendingRun{}, false, err
	}
	run.ClaimedFrom = before
	return run, true, nil
}

// TakeWaitOver records that the turn a wait's claim drives took it. See the
// contract on [PendingStore].
func (s *CoordStore) TakeWaitOver(ctx context.Context, turnID, launch string, fence Fence) (bool, error) {
	taken := false
	_, won, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		if !claimedWaitOver(*run) || run.LaunchID != launch || superseded(*run, fence) {
			return false
		}
		if taken = run.WaitOver.Taken(); taken {
			return false
		}
		over := *run.WaitOver
		over.TakenAt = s.clock()
		run.WaitOver = &over
		return true
	})
	return won || (err == nil && taken), err
}

// ReviveWaitOver gives a wait's claim no turn took back to waiting. See the
// contract on [PendingStore].
func (s *CoordStore) ReviveWaitOver(ctx context.Context, turnID string, revival Revival) (PendingRun, bool, error) {
	launch, fence := revival.Launch, revival.Fence
	var taken PendingRun
	wasTaken := false
	written, won, err := s.mutate(ctx, turnID, func(run *PendingRun) bool {
		if !claimedWaitOver(*run) || run.LaunchID != launch || outranked(*run, fence) {
			return false
		}
		if wasTaken = run.WaitOver.Taken(); wasTaken {
			taken = *run
			return false
		}
		over := *run.WaitOver
		if revival.Lost {
			now := s.clock()
			over.LostClaims++
			if over.FirstLostAt.IsZero() {
				over.FirstLostAt = now
			}
			over.LostOn = over.lostWait().recorded(revival.Node, now, over.FirstLostAt)
		}
		run.Status, run.WaitOver = over.From, &over
		if fence.Fenced() {
			run.Owner, run.OwnerEpoch = fence.Owner, fence.Epoch
		}
		return true
	})
	switch {
	case err != nil:
		return PendingRun{}, false, err
	case wasTaken:
		return taken, false, fmt.Errorf("sandbox: reviving run %s's carrying on: %w", turnID, ErrWaitOverTaken)
	}
	return written, won, nil
}

// DeliverWaitOver puts the wake carrying run on, its wait over
// ([types.ExecutorWaitOver]), on the control topic of the seat holding it,
// addressed by the row's seat id (ADR-0013), as an answer by turn is.
func (d AnswerDesk) DeliverWaitOver(ctx context.Context, run PendingRun) error {
	control := controlSubject(run.AgentID)
	if control == "" {
		return fmt.Errorf("sandbox: run %s names no seat id (%q) to carry it on through", run.TurnID, run.AgentID)
	}
	ev := events.New(types.ExecutorWaitOver{TurnID: run.TurnID, LaunchID: run.LaunchID,
		Agent: run.AgentID, AgentHandle: run.AgentHandle}, events.TraceContext{})
	ev.Source = run.Role
	if err := d.Queue.Publish(ctx, control, ev); err != nil {
		return fmt.Errorf("sandbox: carrying run %s on through seat %s (%s): %w", run.TurnID,
			run.AgentHandle, run.AgentID, err)
	}
	return nil
}

// OnWaitOver carries on the run a wake off the seat's control topic names,
// where it still waits, on the job named, for what the engine answers and that
// is over: nil spends the wake, an error hands it back. A run that moved on,
// or whose wait is not over, spends it: the watch wakes it again while it waits.
func (c *Coordinator) OnWaitOver(ctx context.Context, over types.ExecutorWaitOver, trigger *events.Event) error {
	run, found, err := c.pending.Get(ctx, over.TurnID)
	switch {
	case err != nil:
		return fmt.Errorf("sandbox: reading run %s to carry it on: %w", over.TurnID, err)
	case !found || run.AgentID != over.Agent || run.LaunchID != over.LaunchID:
		return nil
	}
	if claimedWaitOver(run) && !run.WaitOver.Taken() && !c.liveHere(run.TurnID) {
		// THIS NODE'S OWN CLAIM, whose write reported a failure and landed with
		// no turn holding it: given back uncounted, no node having stopped.
		if run, err = c.giveBackWaitOver(ctx, run); err != nil {
			return err
		}
	}
	if !waitsForEngine(run) {
		return nil
	}
	fence, held := c.leaseOf(run.AgentHandle)
	if !held {
		return notHeld(ctx, run.AgentHandle, run.TurnID, "carrying the run on")
	}
	asked, ok := c.carriedOn(ctx, run)
	if !ok {
		return nil
	}
	adm, err := c.admitTurn(ctx, run.AgentHandle, run.ConversationKey, ResumeAnswer)
	if err != nil {
		return err
	}
	defer adm.release()
	return c.carryOnAdmitted(adm, run, asked, fence, trigger)
}

// carriedOn is what run is carried on with, its wait over: how the run it
// waited for left, as the engine reads it now, or that its own calls returned;
// false where its run has not left. Never a person's answer.
func (c *Coordinator) carriedOn(ctx context.Context, run PendingRun) (*AskedRun, bool) {
	asked := &AskedRun{Question: run.Question, Branch: run.Branch, SessionID: run.SessionRan(),
		BoxKept: run.SandboxID != "", Called: run.CallsReturned()}
	if run.WaitsOnOwnCalls() {
		asked.CallsReturned = true
		return asked, true
	}
	if c.waitedFor == nil {
		return nil, false
	}
	read, left := c.waitedFor(ctx, run)
	if !left {
		return nil, false
	}
	waited, err := waitedOf(run, &read)
	if err != nil {
		log.ErrorContext(ctx, "sandbox_wait_unread", "turn_id", run.TurnID, "error", err.Error(),
			"detail", "how the run waited for left was read as something this run does not wait on; "+
				"the run waits on, and is read again at the next wake")
		return nil, false
	}
	asked.Waited = waited
	return asked, true
}

// carryOnAdmitted claims run out of waiting and resumes it with asked, under
// the admission [Coordinator.OnWaitOver] took, as [Coordinator.resumeOwedAdmitted]
// resumes a recorded answer: through the seat's recovery gate, its slot
// reserved in the claim's step. A resume deferred gives the claim back to
// waiting ([Coordinator.resumeAndSettle]): a failure hands the wake back, and a
// refusal on a condition re-arms the watch, for its next wake.
func (c *Coordinator) carryOnAdmitted(adm admitted, run PendingRun, asked *AskedRun, fence Fence,
	trigger *events.Event,
) error {
	ctx := adm.ctx
	c.mu.Lock()
	done, gated := c.claimGateLocked(run.AgentHandle, run.TurnID)
	c.mu.Unlock()
	if gated != nil {
		return gated
	}
	defer done()
	write, err := c.reservingCount(run, run.Status, StatusResumed)
	if err != nil {
		return err
	}
	defer c.settleWrite(write)
	claimed, won, err := c.pending.ClaimWaitOver(ctx, run.TurnID, run.LaunchID, fence)
	c.mu.Lock()
	moved := c.wroteCountLocked(write, err == nil && won)
	c.mu.Unlock()
	moved()
	switch {
	case err != nil:
		// THE CLAIM MAY HAVE LANDED: the wake goes back, and its next delivery
		// gives a landed claim back before claiming again ([Coordinator.OnWaitOver]).
		return fmt.Errorf("sandbox: claiming run %s to carry it on: %w", run.TurnID, err)
	case !won:
		return nil
	}
	_, err = c.resumeAndSettle(adm, claimed, "", true, trigger, runOutcome{
		InputTokens: claimed.ParkedInputTokens, OutputTokens: claimed.ParkedOutputTokens,
		CacheReadTokens:  claimed.ParkedCacheReadTokens,
		CacheWriteTokens: claimed.ParkedCacheWriteTokens,
		Engine:           parkedEngineSpend(claimed),
		Asked:            asked,
	}, nil)
	if refusal, refused := waitingRefusal(err); refused {
		// A WAIT, its claim given back to waiting and its watch re-armed
		// ([CoordinatorOptions.Parked]), as a revival's is: the watch ends at a
		// read finding nothing waiting, which a read while the claim stood was,
		// and nothing else would wake the run again.
		log.InfoContext(ctx, "sandbox_wait_over_refused", "turn_id", run.TurnID,
			"condition", string(refusal.Condition), "reason", refusal.Reason)
		if c.parked != nil {
			c.parked(ctx, run)
		}
		return nil
	}
	return err
}

// giveBackWaitOver gives this node's own untaken claim of run back to waiting,
// uncounted ([PendingStore.ReviveWaitOver]), and reads the row again.
func (c *Coordinator) giveBackWaitOver(ctx context.Context, run PendingRun) (PendingRun, error) {
	fence, held := c.leaseOf(run.AgentHandle)
	if !held || outranked(run, fence) || (fence.Fenced() && run.OwnerEpoch != fence.Epoch) {
		return run, nil
	}
	given, won, err := c.pending.ReviveWaitOver(ctx, run.TurnID, Revival{Launch: run.LaunchID, Fence: fence})
	c.syncSeat(ctx, run.AgentHandle)
	switch {
	case errors.Is(err, ErrWaitOverTaken):
		return given, nil
	case err != nil:
		return run, fmt.Errorf("sandbox: giving run %s's claim back to its wait: %w", run.TurnID, err)
	case won:
		return given, nil
	}
	latest, found, err := c.pending.Get(ctx, run.TurnID)
	if err != nil || !found {
		return run, err
	}
	return latest, nil
}

// reviveWaitOver is [Coordinator.reapTail] for the claim of a run the engine
// carried on whose node stopped before its turn took it: revived to waiting under
// this lease and counted, the seat's watch carrying it on again; a revival the
// store did not confirm is retried ([Coordinator.oweRevival]). handled is false
// where run is not that claim, is past its revivals ([Coordinator.revivableAfter])
// or a turn took it: reaped as used.
func (c *Coordinator) reviveWaitOver(ctx context.Context, run PendingRun, fence Fence,
) (moved *PendingRun, did reaped, handled bool) {
	over := run.WaitOver
	if !claimedWaitOver(run) || over.Taken() {
		return nil, reapLeft, false
	}
	if why := c.revivableAfter(run, "this run's carrying on", over.LostClaims, over.lostWait()); why != "" {
		log.WarnContext(ctx, "sandbox_wait_over_not_revived", "turn_id", run.TurnID,
			"agent", run.AgentHandle, "reason", why)
		return nil, reapLeft, false
	}
	revived, won, err := c.pending.ReviveWaitOver(ctx, run.TurnID, Revival{Launch: run.LaunchID, Fence: fence,
		Lost: true, Node: c.node})
	switch {
	case errors.Is(err, ErrWaitOverTaken):
		return nil, reapLeft, false
	case err != nil:
		c.oweRevival(ctx, run, fence, err)
		return nil, reapOwed, true
	case !won:
		moved, did = c.rereadClaim(ctx, run, fence)
		return moved, did, true
	}
	log.InfoContext(ctx, "sandbox_wait_over_revived", "turn_id", run.TurnID, "agent", run.AgentHandle,
		"lost_claims", revived.WaitOver.LostClaims,
		"detail", "the node holding this seat stopped before the run it carried on took its turn; the "+
			"run waits again, and the seat's watch carries it on")
	if c.parked != nil {
		c.parked(ctx, revived)
	}
	return &revived, reapRevived, true
}
