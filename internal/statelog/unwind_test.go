package statelog_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// WHAT A LOOP HOLDS PAST ITS CHECKPOINT WHEN ITS LOG MAY HAVE MOVED IS GIVEN
// BACK, NEVER MIXED IN.
//
// A broker restored under a running node — an R1 log on an interval fsync that
// crashed, or a copy brought back — ends somewhere below the records the node
// was handed, and its own next records reuse their sequences. The loop learns
// of it from a failed fetch or from an end read below what it holds, and
// before it applies anything past its checkpoint it compares the record there
// with the log's. Every case below ends one of the two ways that keeps a
// node's rows ONE history: the divergence stop, or only the restored log's
// records applied past the checkpoint — never the old history's records
// committed with the new one's after them, which no later reading can find.

// restoredAt is the broker instant of the restored log's record at seq: another
// record than the one first offered there.
func restoredAt(seq uint64) time.Time {
	return otherHistory.Add(time.Duration(seq) * time.Second)
}

// offerRestored offers, at each sequence, the restored log's own record: an
// edit stored at another instant than anything the node was handed there.
func offerRestored(h *applyHarness, seqs ...uint64) {
	for _, seq := range seqs {
		h.fetch.offerStored(seq, restoredAt(seq),
			env(seq, "edit", fmt.Sprintf("r%d", seq), fmt.Sprintf("op-restored-%d", seq), 1))
	}
}

// stopping runs h's applier for a case that expects it to stop by itself, and
// answers how it ended; the case's end cancels it and waits for it. A case
// holding a commit releases it before it returns: the loop waits for its
// transaction beside it.
func (h *applyHarness) stopping(t *testing.T) <-chan error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
	ended := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ended <- h.runner.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return ended
}

// stoppedWith waits for the loop ended reports to stop by itself, failing t if
// it does not within [heldBudget].
func stoppedWith(t *testing.T, ended <-chan error) error {
	t.Helper()
	select {
	case err := <-ended:
		return err
	case <-time.After(heldBudget):
		t.Fatalf("the loop never stopped: not within %s", heldBudget)
		return nil
	}
}

// rowsByHistory reads which history each probe row past through came from:
// its broker instant names it.
func rowsByHistory(t *testing.T, h *applyHarness, through uint64) map[uint64]string {
	t.Helper()
	out := map[uint64]string{}
	if err := h.estate.Read(t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(t.Context(), `SELECT position, stored_at FROM probe_rows`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var packed, stored int64
			if err := rows.Scan(&packed, &stored); err != nil {
				return err
			}
			for seq := uint64(1); seq <= through; seq++ {
				at := statelog.Position{Stream: probeStream, Generation: 1, Seq: seq}
				if at.Packed() != packed {
					continue
				}
				switch instant := store.DecodeTime(stored); {
				case instant.Equal(restoredAt(seq)):
					out[seq] = "restored"
				case instant.Equal(probeStoredAt(seq)):
					out[seq] = "original"
				default:
					out[seq] = instant.String()
				}
			}
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("read the probe rows: %v", err)
	}
	return out
}

// BARRIERS REACHED PAST THE CHECKPOINT ARE NOT THE BASE A RESTORED LOG IS
// ADMITTED AGAINST.
//
// Two barriers are reached and handed to the checkpoint transaction beside the
// loop, whose commit is held — a disk's stalled fsync. The broker is restored
// meanwhile: the loop's next fetch fails, and the log now holds edits of its
// own at the barriers' sequences and past them. The commit lands only after
// the loop has had time to look at what the restored broker delivers. Before,
// the loop verified the checkpoint's record as it stood before that commit,
// found it the log's, took the restored edits at the barriers' sequences for
// redeliveries of what it had reached — and skipped them once the barriers
// committed — and applied the edit after them on top: rows of two histories,
// and a checkpoint naming the restored log's own record, so neither the loop
// nor the heartbeat could find it again. Now the doubt sends the reached
// position back, the loop waits for the commit in flight before it looks, and
// the record that commit left at the checkpoint is the old history's, which
// the restored log does not hold: the divergence stop.
func TestBarriersReachedPastTheCheckpointAreNotTheBaseARestoredLogIsAdmittedAgainst(t *testing.T) {
	t.Parallel()
	hold := storetest.HoldCommits(t)
	h := newApplyHarnessOn(t, probeDomain{}, store.Options{WrapDriver: hold.Wrap}, nil)
	for seq := uint64(1); seq <= 3; seq++ {
		h.fetch.offer(seq, env(seq, "edit", fmt.Sprint(seq), fmt.Sprintf("op-%d", seq), 1))
	}
	ended := h.stopping(t)
	until(t, "the writes before the hold", func() bool { return h.runner.Committed().Seq >= 3 })

	hold.Arm()
	defer hold.Release()
	h.fetch.offer(4, barrierEnv())
	h.fetch.offer(5, barrierEnv())
	if err := hold.AwaitHeld(t.Context(), 1); err != nil {
		t.Fatalf("the barriers' checkpoint never reached its commit: %v", err)
	}
	until(t, "the loop reaching both barriers", func() bool { return h.runner.Reached().Seq == 5 })

	// THE RESTORE: a fetch fails, and the log holds edits of its own from 4.
	h.fetch.fail(1)
	offerRestored(h, 4, 5, 6)
	until(t, "the reached position back at the checkpoint after the failed fetch — a "+
		"read waiting there would be released on barriers the log may no longer hold",
		func() bool { return h.fetch.failing() == 0 && h.runner.Reached().Seq == 3 })
	// LONG ENOUGH for a loop that did not wait for the commit in flight to
	// fetch what the restored broker delivers and verify the checkpoint as it
	// stood before that commit — a back-off and a fetch, milliseconds.
	time.Sleep(time.Second)
	hold.Release()

	err := stoppedWith(t, ended)
	if !errors.Is(err, statelog.ErrLogDiverged) {
		t.Fatalf("the loop over a log restored under its held checkpoint returned %v, "+
			"want the divergence stop", err)
	}
	if appliedAt(h, 6) {
		t.Fatalf("the restored log's edit at 6 was applied on top of the barriers the "+
			"old history committed: the rows hold %v", rowsByHistory(t, h, 6))
	}
	if got := h.runner.Committed(); got.Seq != 5 {
		t.Fatalf("the checkpoint is at %s, want the barriers' 5, which the held commit "+
			"left", got)
	}
}

// A TAIL THE BUDGET SPLIT IS GIVEN BACK AT A FAILED FETCH.
//
// Each record writes a whole transaction's rows, so a run of three commits one
// and keeps the other two in hand. The broker is restored while the first
// commits: the loop's next fetch fails, and the log holds edits of its own at
// the two in hand and past them. Before, the loop kept its tail, verified the
// checkpoint's record — the restored log's own, since the restore point is at
// it — and applied the old history's two before the restored log's next edit,
// skipping the restored log's at their sequences as redeliveries. Now the tail
// goes back to the broker with the doubt, and only the restored log's records
// are applied past the checkpoint.
func TestATailTheBudgetSplitIsGivenBackAtAFailedFetch(t *testing.T) {
	t.Parallel()
	hold := storetest.HoldCommits(t)
	h := newApplyHarnessOn(t, probeDomain{}, store.Options{WrapDriver: hold.Wrap}, nil)
	for seq := uint64(1); seq <= 3; seq++ {
		h.fetch.offer(seq, env(seq, "edit", fmt.Sprint(seq), fmt.Sprintf("op-%d", seq), 1))
	}
	loop := h.looping(t)
	until(t, "the writes before the hold", func() bool { return h.runner.Committed().Seq >= 3 })

	h.applier.mu.Lock()
	h.applier.rowsPer = statelog.ApplyTxRowBudget
	h.applier.mu.Unlock()
	hold.Arm()
	defer hold.Release()
	for seq := uint64(4); seq <= 6; seq++ {
		h.fetch.offer(seq, env(seq, "edit", fmt.Sprint(seq), fmt.Sprintf("op-%d", seq), 1))
	}
	if err := hold.AwaitHeld(t.Context(), 1); err != nil {
		t.Fatalf("the first of the three never reached its commit: %v", err)
	}

	// THE RESTORE, at 4: the next fetch fails, and the log holds its own
	// edits at the two the loop has in hand, and one past them.
	h.fetch.fail(1)
	offerRestored(h, 5, 6, 7)
	hold.Release()

	until(t, "the restored log applied through 7", func() bool {
		return h.runner.Committed().Seq >= 7 || h.runner.Stopped() != nil
	})
	if err := loop.stop(); err != nil {
		t.Fatalf("the applier stopped with %v", err)
	}
	got := rowsByHistory(t, h, 7)
	for seq, want := range map[uint64]string{4: "original", 5: "restored", 6: "restored", 7: "restored"} {
		if got[seq] != want {
			t.Errorf("the row at %d is the %s history's, want the %s's — the rows hold %v",
				seq, got[seq], want, got)
		}
	}
	for _, seq := range []uint64{5, 6} {
		if h.fetch.releaseCount(seq) == 0 {
			t.Errorf("the old history's record at %d was never handed back — it waits "+
				"out an ack window on a healthy broker", seq)
		}
	}
}

// WHAT THE REORDER BUFFER HOLDS ABOVE A HOLE IS GIVEN BACK AT A FAILED FETCH.
//
// Two records arrive above a hole and wait in the reorder buffer for the one
// below them. The broker is restored: a fetch fails, and the log holds edits of
// its own from the hole up. Before, the buffer kept the old two, and when the
// restored log's records arrived it kept ONE record per sequence — the old
// ones, which arrived first — so the restored log's record at the hole was
// applied under the old history's two. Now the buffer is emptied with the
// doubt, and only the restored log's records are applied.
func TestWhatTheReorderBufferHoldsIsGivenBackAtAFailedFetch(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, probeDomain{})
	for seq := uint64(1); seq <= 3; seq++ {
		h.fetch.offer(seq, env(seq, "edit", fmt.Sprint(seq), fmt.Sprintf("op-%d", seq), 1))
	}
	loop := h.looping(t)
	until(t, "the writes before the hole", func() bool { return h.runner.Committed().Seq >= 3 })

	h.fetch.offer(5, env(5, "edit", "5", "op-5", 1))
	h.fetch.offer(6, env(6, "edit", "6", "op-6", 1))
	until(t, "the loop taking the two above the hole", func() bool {
		pending, _ := h.fetch.Pending(t.Context())
		return pending == 0
	})

	h.fetch.fail(1)
	offerRestored(h, 4, 5, 6)
	until(t, "the restored log applied through 6", func() bool {
		return h.runner.Committed().Seq >= 6 || h.runner.Stopped() != nil
	})
	if err := loop.stop(); err != nil {
		t.Fatalf("the applier stopped with %v", err)
	}
	got := rowsByHistory(t, h, 6)
	for _, seq := range []uint64{4, 5, 6} {
		if got[seq] != "restored" {
			t.Errorf("the row at %d is the %s history's, want the restored log's — the "+
				"rows hold %v", seq, got[seq], got)
		}
	}
}

// AN END READ BELOW THE REACHED POSITION OWES A VERIFICATION.
//
// The checkpoint is below the barriers the loop reached past it, so a reading
// of the log's end between the two finds nothing ahead — the checkpoint is
// inside the log — and refuses nothing. It is still a log that does not hold
// what the loop holds: here a restore that no fetch failure told the loop of,
// whose log is then written past those barriers in another history. The
// reading sends the reached position back at once and the loop verifies before
// it goes on, at the checkpoint the held commit leaves: the divergence stop.
// Paired only with the checkpoint, the reading found nothing, and the loop
// applied the restored log's next record on top of the barriers it committed.
func TestAnEndReadBelowTheReachedPositionOwesAVerification(t *testing.T) {
	t.Parallel()
	hold := storetest.HoldCommits(t)
	h := newApplyHarnessOn(t, probeDomain{}, store.Options{WrapDriver: hold.Wrap}, nil)
	for seq := uint64(1); seq <= 3; seq++ {
		h.fetch.offer(seq, env(seq, "edit", fmt.Sprint(seq), fmt.Sprintf("op-%d", seq), 1))
	}
	ended := h.stopping(t)
	until(t, "the writes before the hold", func() bool { return h.runner.Committed().Seq >= 3 })

	hold.Arm()
	defer hold.Release()
	h.fetch.offer(4, barrierEnv())
	h.fetch.offer(5, barrierEnv())
	if err := hold.AwaitHeld(t.Context(), 1); err != nil {
		t.Fatalf("the barriers' checkpoint never reached its commit: %v", err)
	}
	until(t, "the loop reaching both barriers", func() bool { return h.runner.Reached().Seq == 5 })

	// WHERE THE RUNNER STOOD, read before the end, as every reader reads it.
	stance := h.runner.Stance()
	if established, reached := h.runner.ObserveEnd(stance, 3); established || reached {
		t.Fatalf("an end at the checkpoint changed the end's verdict (established %v, "+
			"reached %v) — the checkpoint is inside the log, and writes must go on",
			established, reached)
	}
	if err := h.runner.StreamIdentity(); err != nil {
		t.Fatalf("an end between the checkpoint and the reached position refused: %v", err)
	}
	if got := h.runner.Reached().Seq; got != 3 {
		t.Errorf("the reached position is %d after the end was read at 3, want 3 — a "+
			"read would be released on barriers the log does not hold", got)
	}

	// AND THE RESTORED LOG IS WRITTEN PAST THEM, in another history.
	offerRestored(h, 4, 5, 6)
	hold.Release()
	err := stoppedWith(t, ended)
	if !errors.Is(err, statelog.ErrLogDiverged) {
		t.Fatalf("the loop over a log restored below its reached position returned %v, "+
			"want the divergence stop", err)
	}
	if appliedAt(h, 6) {
		t.Fatalf("the restored log's edit at 6 was applied on top of the barriers the "+
			"old history committed: the rows hold %v", rowsByHistory(t, h, 6))
	}
}

// A BARRIER WAITING BESIDE THE LOOP AT A DOUBT IS GIVEN BACK, NOT COMMITTED.
//
// Two barriers are in the checkpoint transaction beside the loop, held at its
// commit, and a third waits behind them for the next. The broker is restored
// with its end at the second — the third and what follows it are the restored
// log's own — and a fetch fails. The held commit is the old history's and the
// log's alike, so the checkpoint it leaves is one the restored log holds; the
// third, from before the failure, is not committed after it but given back,
// and the loop follows the restored log from the checkpoint without a stop.
// Committed under the doubt, it named a record the restored log does not hold
// at its sequence, and the node stopped on a divergence its own rows never had.
func TestABarrierWaitingBesideTheLoopAtADoubtIsGivenBack(t *testing.T) {
	t.Parallel()
	hold := storetest.HoldCommits(t)
	h := newApplyHarnessOn(t, probeDomain{}, store.Options{WrapDriver: hold.Wrap}, nil)
	for seq := uint64(1); seq <= 3; seq++ {
		h.fetch.offer(seq, env(seq, "edit", fmt.Sprint(seq), fmt.Sprintf("op-%d", seq), 1))
	}
	loop := h.looping(t)
	until(t, "the writes before the hold", func() bool { return h.runner.Committed().Seq >= 3 })

	hold.Arm()
	defer hold.Release()
	h.fetch.offer(4, barrierEnv())
	h.fetch.offer(5, barrierEnv())
	if err := hold.AwaitHeld(t.Context(), 1); err != nil {
		t.Fatalf("the barriers' checkpoint never reached its commit: %v", err)
	}
	h.fetch.offer(6, barrierEnv())
	until(t, "the loop reaching the third barrier behind the held two", func() bool {
		return h.runner.Reached().Seq == 6
	})

	// THE RESTORE, its end at 5: the log's own records from 6.
	h.fetch.fail(1)
	offerRestored(h, 6, 7)
	until(t, "the loop meeting the failed fetch", func() bool {
		return h.fetch.failing() == 0 && h.runner.Reached().Seq == 3
	})
	hold.Release()
	until(t, "the restored log applied through 7", func() bool {
		return h.runner.Committed().Seq >= 7 || h.runner.Stopped() != nil
	})
	if err := loop.stop(); err != nil {
		t.Fatalf("the loop over a log restored at the checkpoint its held commit left "+
			"stopped with %v — the barrier waiting beside the loop was committed "+
			"under the doubt", err)
	}
	if got := rowsByHistory(t, h, 7); got[6] != "restored" || got[7] != "restored" {
		t.Errorf("the rows at 6 and 7 are %v, want the restored log's", got)
	}
	if h.fetch.releaseCount(6) == 0 {
		t.Error("the old history's barrier at 6 was never handed back")
	}
}

// A DOUBT WHILE THE START VERIFIES IS NOT SETTLED BY IT.
//
// A start compares the record at the checkpoint with the log's before the loop
// applies anything past it. Here the broker is restored the moment that
// comparison has read the log: the heartbeat beside the start reads the end
// below the checkpoint, and the restored log is then written past it in
// another history. What the start read was the node's own record, but the
// doubt came after it read, so the loop owes a look of its own, finds another
// record at the checkpoint, and stops on the divergence. Settled by the start,
// the doubt was cleared unasked and the restored log's records were applied on
// top of these rows, past a checkpoint that then named the restored log's own
// record — which neither the loop nor the heartbeat could find again.
func TestADoubtWhileTheStartVerifiesIsNotSettledByIt(t *testing.T) {
	t.Parallel()
	h := appliedThrough(t, 3)
	h.fetch.afterAt(func() {
		// THE RESTORE, as the heartbeat beside the start reads it.
		h.fetch.endAt(2)
		h.runner.ObserveEnd(h.runner.Stance(), 2)
		// AND THE RESTORED LOG WRITTEN PAST THE CHECKPOINT.
		h.fetch.rewrite(3, otherHistory)
		offerRestored(h, 4, 5)
		h.fetch.endAt(5)
	})
	ended := h.stopping(t)
	until(t, "the loop stopping, or applying the restored log", func() bool {
		return h.runner.Stopped() != nil || appliedAt(h, 4)
	})
	if appliedAt(h, 4) {
		t.Fatalf("the restored log's record at 4 was applied past a checkpoint the "+
			"log no longer holds: the rows hold %v", rowsByHistory(t, h, 5))
	}
	if err := stoppedWith(t, ended); !errors.Is(err, statelog.ErrLogDiverged) {
		t.Fatalf("the loop over a log restored while its start verified returned %v, "+
			"want the divergence stop", err)
	}
}
