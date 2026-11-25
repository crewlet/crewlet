package sandbox

import (
	"context"
	"fmt"
	"slices"
	"time"
)

// hookVerdictWait is the reason one verdict on a job's hooks' serves is given
// ([Coordinator.settleReceipts]): a store's slow moment, ten seconds, as
// every row write's reason; [Coordinator.rowContext] adds the row's crossing
// ([BoxReadBudget] counts both for a read that asks receipts).
const hookVerdictWait = 10 * time.Second

// hookVerdictRetry spaces the attempts at a verdict within its room: a tenth of
// the reason, so a store's brief refusal is retried several times before the
// read is handed back.
const hookVerdictRetry = time.Second

// settleReceipts settles run's job's serves by what the job's record showed of
// them ([PendingStore.SettleHook]) and publishes every note and wake a serve
// handed over ([CoordinatorOptions.SteersConfirmed]) on every read of a job
// whose runner confirms off that record ([ThreadTraits.Receipts]).
// A verdict the store does not answer within its room is [ErrStoreSilent],
// returned before the read's take; the package doc's "A HOOK'S NOTE" says why.
func (c *Coordinator) settleReceipts(ctx context.Context, run PendingRun, receipts TakeReceipts,
	read BoxRead) error {
	if !c.threadTraits(run.Harness).Receipts {
		return nil
	}
	notes, asked := run.Steer.Notes, askedTakes(run)
	switch {
	case len(asked) == 0:
	case !receipts.decided():
		log.InfoContext(ctx, "steer_receipt_unread", "turn_id", run.TurnID, "launch_id", run.LaunchID,
			"point", string(read), "takes", len(asked), "detail", receipts.Why,
			"outcome", "the job's session decided none of its hooks' serves, which stay unconfirmed")
	default:
		answered, err := c.settleVerdict(ctx, run, receipts)
		if err != nil {
			log.WarnContext(ctx, "steer_receipt_unsettled", "turn_id", run.TurnID, "launch_id", run.LaunchID,
				"point", string(read), "error", err.Error(),
				"detail", "the coordination store did not answer the verdict on the job's served notes "+
					"within its room; the read is handed back before its take, and the read made again "+
					"finds the verdict wherever it landed")
			return storeSilent(ctx, fmt.Errorf("sandbox: settling run %s's hooks' serves by its job's "+
				"session: %w", run.TurnID, err))
		}
		notes = withAnswered(notes, answered)
		warnCancelUnheard(ctx, run, receipts.Absent)
	}
	if c.steersConfirmed == nil {
		return nil
	}
	var confirmed []RunNote
	for _, n := range notes {
		if (n.For == "" || n.IsWake()) && readByServeOf(n, run.LaunchID) {
			confirmed = append(confirmed, n)
		}
	}
	c.steersConfirmed(ctx, run, confirmed)
	return nil
}

// settleVerdict asks the store for the verdict on run's job's serves, again
// [Coordinator.verdictRetry] after each error, until its room (the reason and the
// row's crossing, [Coordinator.rowContext]) is spent. Each attempt decides on the
// record as it stands, so one finding an earlier write landed answers the same.
func (c *Coordinator) settleVerdict(ctx context.Context, run PendingRun, receipts TakeReceipts) ([]RunNote,
	error) {
	row, cancel := c.rowContext(ctx, c.verdictWait)
	defer cancel()
	settle := HookSettle{Launch: run.LaunchID, Held: receipts.Held, Restarted: receipts.Restarted,
		Absent: receipts.Absent}
	for {
		answered, err := c.pending.SettleHook(row, run.TurnID, settle)
		if err == nil {
			return answered, nil
		}
		retry := time.NewTimer(c.verdictRetry)
		select {
		case <-row.Done():
			retry.Stop()
			return nil, err
		case <-retry.C:
		}
	}
}

// withAnswered is notes with each note a verdict answered in its place, by id.
func withAnswered(notes, answered []RunNote) []RunNote {
	out := slices.Clone(notes)
	for _, a := range answered {
		if i := slices.IndexFunc(out, func(n RunNote) bool { return n.ID == a.ID }); i >= 0 {
			out[i] = a
		}
	}
	return out
}

// warnCancelUnheard says, as steer_cancel_unheard, each serve to run's job no
// cancel lost that its session proved absent: the harness gave an answer up
// and the bridge heard no cancel of it.
func warnCancelUnheard(ctx context.Context, run PendingRun, absent []string) {
	for _, n := range run.Steer.Notes {
		for _, h := range n.Serves {
			if h.Launch == run.LaunchID && h.Lost == "" && slices.Contains(absent, h.Take) {
				log.WarnContext(ctx, "steer_cancel_unheard", "turn_id", run.TurnID, "launch_id", run.LaunchID,
					"take", h.Take, "note_id", n.ID,
					"detail", "the job's session never took the note its hook was served, and the bridge "+
						"heard no cancel of the call: a release that stops sending cancels shows so")
			}
		}
	}
}
