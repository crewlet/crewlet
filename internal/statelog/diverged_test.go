package statelog_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// ---- a log that diverged from the rows -------------------------------- //
//
// A broker restored from an older copy keeps its stream, creation instant and
// all. A node whose rows are newer than the copy sees its checkpoint past the
// log's end — until anything writes the restored log past that checkpoint,
// when the end is an ordinary end again and nothing observable about it
// separates the log from the history the node applied. What does is the
// RECORD at the checkpoint's sequence: the checkpoint names it, and another
// record there is a second history these rows must never be applied on top of.

// otherHistory is the broker instant of a record written to a restored log after
// the restore — at a sequence a node had already consumed a different record at.
var otherHistory = time.Date(2031, 4, 2, 3, 0, 0, 418_226_000, time.UTC)

// appliedThrough runs a fresh harness through records 1..n.
func appliedThrough(t *testing.T, n uint64) *applyHarness {
	t.Helper()
	h := newApplyHarness(t, probeDomain{})
	for seq := uint64(1); seq <= n; seq++ {
		h.fetch.offer(seq, env(seq, "edit", fmt.Sprint(seq), fmt.Sprintf("op-%d", seq), 1))
	}
	if err := h.run(n); err != nil {
		t.Fatalf("run through %d: %v", n, err)
	}
	return h
}

// storedAtOf reads the record the committed checkpoint row names.
func storedAtOf(t *testing.T, db *store.DB) time.Time {
	t.Helper()
	cp, found, err := statelog.CheckpointOf(t.Context(), db.Replicated(), probeStream)
	if err != nil || !found {
		t.Fatalf("read the checkpoint: %v (found %v)", err, found)
	}
	return cp.StoredAt
}

// appliedAt reports whether the probe applier ever applied a record at seq.
func appliedAt(h *applyHarness, seq uint64) bool {
	for _, p := range h.applier.seen() {
		if p.Seq == seq {
			return true
		}
	}
	return false
}

// THE CHECKPOINT NAMES THE RECORD IT STANDS ON, in the same transaction as the
// rows — and a batch of nothing but redeliveries leaves it naming the same one.
//
// Without it nothing can tell a restored log written past this node's rows from
// the history they came from, because every sequence term reads healthy.
func TestACheckpointNamesTheRecordItStandsOn(t *testing.T) {
	t.Parallel()
	h := appliedThrough(t, 3)
	if got := storedAtOf(t, h.db); !got.Equal(probeStoredAt(3)) {
		t.Fatalf("the checkpoint at 3 names the record stored at %s, want the one "+
			"consumed there, %s", got, probeStoredAt(3))
	}
	// REDELIVERIES ONLY: the checkpoint does not move, and neither does the
	// record it names — the run commits nothing past it, so what it names
	// is still the record consumed at 3, never the run's own tail or none.
	h.fetch.offer(2, env(2, "edit", "2", "op-2", 1))
	h.fetch.offer(3, env(3, "edit", "3", "op-3", 1))
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	errs := make(chan error, 1)
	go func() { errs <- h.runner.Run(ctx) }()
	for h.fetch.ackCount(3) < 2 && ctx.Err() == nil {
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	<-errs
	if h.fetch.ackCount(3) < 2 {
		t.Fatal("the redelivery was never consumed")
	}
	if got := storedAtOf(t, h.db); !got.Equal(probeStoredAt(3)) {
		t.Fatalf("after a run of redeliveries the checkpoint at 3 names %s, want %s",
			got, probeStoredAt(3))
	}
	// AND A RUN PAST IT names the record it moved to.
	h.fetch.offer(4, env(4, "edit", "4", "op-4", 1))
	if err := h.run(4); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := storedAtOf(t, h.db); !got.Equal(probeStoredAt(4)) {
		t.Fatalf("the checkpoint at 4 names %s, want %s", got, probeStoredAt(4))
	}
}

// A RESTORED LOG WRITTEN PAST A RUNNING NODE'S CHECKPOINT IS NEVER APPLIED.
//
// The node saw its checkpoint past the restored log's end and refused; then a
// node whose rows were not ahead wrote the log past it. The end reaching the
// checkpoint again used to clear the refusal and the loop went on applying the
// other history on top of these rows. Now the loop verifies the record at the
// checkpoint before it applies anything past it, finds another one, and stops
// — and the verdict does not clear on any later reading of the end.
func TestARestoredLogWrittenPastTheCheckpointIsNeverApplied(t *testing.T) {
	t.Parallel()
	h := appliedThrough(t, 3)
	at := h.runner.Committed()

	// ONE LOOP, RUNNING ACROSS THE RESTORE: its start verified the checkpoint
	// against a log that was still its own history, so nothing but the
	// reading below leaves it owing another look.
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	errs := make(chan error, 1)
	fetched := h.fetch.fetchCount()
	go func() { errs <- h.runner.Run(ctx) }()
	for h.fetch.fetchCount() <= fetched && ctx.Err() == nil {
		time.Sleep(time.Millisecond)
	}

	// THE RESTORE: the log ends at 2, below the checkpoint.
	h.fetch.endAt(2)
	if established, _ := h.runner.ObserveEnd(at, 2); !established {
		t.Fatal("a log ending below the checkpoint established nothing")
	}
	// AND THE WRITE PAST IT, by a node whose rows were at 2: another record
	// at 3, and one at 4.
	h.fetch.rewrite(3, otherHistory)
	h.fetch.endAt(4)
	if _, reached := h.runner.ObserveEnd(at, 4); !reached {
		t.Fatal("the end reaching the checkpoint did not clear the end's verdict")
	}
	h.fetch.offerStored(4, otherHistory.Add(time.Second), env(4, "edit", "4", "op-other-4", 1))

	var err error
	select {
	case err = <-errs:
	case <-ctx.Done():
		t.Fatal("the loop never stopped on the other history")
	}
	if !errors.Is(err, statelog.ErrLogDiverged) {
		t.Fatalf("the loop over a log written past its checkpoint in another "+
			"history returned %v, want the divergence stop", err)
	}
	if appliedAt(h, 4) {
		t.Fatal("the record the other history wrote at 4 was applied on top of " +
			"these rows")
	}
	if got := h.runner.Committed(); got != at {
		t.Fatalf("the checkpoint moved to %s, want it held at %s", got, at)
	}
	if !errors.Is(h.runner.StreamIdentity(), statelog.ErrLogDiverged) || !h.runner.Diverged() {
		t.Fatalf("the identity is %v — reads and writes must refuse on the "+
			"divergence", h.runner.StreamIdentity())
	}
	for _, want := range []string{"at sequence 3", "reanchor", "restored"} {
		if msg := h.runner.StreamIdentity().Error(); !strings.Contains(msg, want) {
			t.Errorf("the refusal %q does not name %q", msg, want)
		}
	}

	// STICKY: no reading of the end clears it, and a re-run over the same
	// rows stops at once — even one whose own reading of the log comes from
	// a member that does not reach the checkpoint, and so could not find the
	// divergence again for itself.
	h.runner.ObserveEnd(at, 10)
	if !errors.Is(h.runner.StreamIdentity(), statelog.ErrLogDiverged) {
		t.Fatal("a later reading of the end cleared the divergence")
	}
	h.fetch.endAt(2)
	again, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	if err := h.runner.Run(again); !errors.Is(err, statelog.ErrLogDiverged) {
		t.Fatalf("a re-run over the same rows returned %v, want the divergence stop", err)
	}
	if appliedAt(h, 4) {
		t.Fatal("a re-run applied the other history")
	}
}

// A STALE MEMBER'S END THAT CATCHES UP ON THE SAME RECORD RESUMES.
//
// The refusal of a checkpoint past the end must lift when the end was only one
// member's view: the loop verifies the record at the checkpoint, finds the one
// it consumed, and applies what follows.
func TestAStaleEndThatCatchesUpOnTheSameRecordResumes(t *testing.T) {
	t.Parallel()
	h := appliedThrough(t, 3)
	at := h.runner.Committed()
	if established, _ := h.runner.ObserveEnd(at, 2); !established {
		t.Fatal("a stale end established nothing")
	}
	h.fetch.offer(4, env(4, "edit", "4", "op-4", 1))
	h.runner.ObserveEnd(at, 4)
	if err := h.run(4); err != nil {
		t.Fatalf("the loop over the same history: %v", err)
	}
	if !appliedAt(h, 4) {
		t.Fatal("the next record of the same history was not applied")
	}
	if err := h.runner.StreamIdentity(); err != nil {
		t.Fatalf("the identity is %v after the same history resumed", err)
	}
}

// A BOOT ON A LOG ALREADY WRITTEN PAST THE CHECKPOINT STOPS BEFORE APPLYING.
//
// The embedded broker runs in the engine's process, so bringing its store back
// IS a restart — and a node that booted after a peer had written the restored
// log past its rows never saw its checkpoint past any end at all. The loop's
// start verifies the record at the checkpoint before it fetches a thing.
func TestABootOnALogAlreadyWrittenPastTheCheckpointStopsBeforeApplying(t *testing.T) {
	t.Parallel()
	h := appliedThrough(t, 3)
	at := h.runner.Committed()

	// THE NEXT BOOT: the same rows, and a log holding another record at 3
	// and one past it.
	h.rebuild(probeDomain{}, time.Time{})
	h.fetch.offerStored(3, otherHistory, env(3, "edit", "3", "op-other-3", 1))
	h.fetch.offerStored(4, otherHistory.Add(time.Second), env(4, "edit", "4", "op-other-4", 1))

	if err := h.run(4); !errors.Is(err, statelog.ErrLogDiverged) {
		t.Fatalf("a boot on a log written past its checkpoint returned %v, want "+
			"the divergence stop", err)
	}
	if appliedAt(h, 4) {
		t.Fatal("the boot applied the other history's record at 4")
	}
	if got := h.runner.Committed(); got != at {
		t.Fatalf("the checkpoint moved to %s, want %s", got, at)
	}
}

// A BOOT ON A LOG WRITTEN EXACTLY TO THE CHECKPOINT REFUSES AT ONCE.
//
// Nothing past the checkpoint is delivered, so a loop that waited for the first
// record to verify before would sit idle over rows the log diverged from, and
// every read and write would be answered from them until the heartbeat looked.
// Its start verifies instead.
func TestABootOnALogWrittenExactlyToTheCheckpointRefusesAtOnce(t *testing.T) {
	t.Parallel()
	h := appliedThrough(t, 3)
	h.rebuild(probeDomain{}, time.Time{})
	h.fetch.offerStored(1, probeStoredAt(1), env(1, "edit", "1", "op-1", 1))
	h.fetch.offerStored(3, otherHistory, env(3, "edit", "3", "op-other-3", 1))

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	if err := h.runner.Run(ctx); !errors.Is(err, statelog.ErrLogDiverged) {
		t.Fatalf("a boot on a log written exactly to its checkpoint returned %v, "+
			"want the divergence stop", err)
	}
	if !errors.Is(h.runner.StreamIdentity(), statelog.ErrLogDiverged) {
		t.Fatalf("the identity is %v, want the divergence", h.runner.StreamIdentity())
	}
}

// A LOG WRITTEN EXACTLY TO THE CHECKPOINT IS FOUND WITHOUT A RECORD PAST IT.
//
// Nothing past the checkpoint means nothing for the loop to verify before, so
// the verification the heartbeat asks every interval is what finds it — once:
// a line written on every beat would bury the one that matters.
func TestALogWrittenExactlyToTheCheckpointIsFoundByTheHeartbeat(t *testing.T) {
	t.Parallel()
	h := appliedThrough(t, 3)
	h.fetch.rewrite(3, otherHistory)

	established, err := h.runner.VerifyCheckpoint(t.Context())
	if err != nil || !established {
		t.Fatalf("VerifyCheckpoint = (%v, %v), want the divergence established", established, err)
	}
	if !errors.Is(h.runner.StreamIdentity(), statelog.ErrLogDiverged) {
		t.Fatalf("the identity is %v, want the divergence", h.runner.StreamIdentity())
	}
	if established, _ := h.runner.VerifyCheckpoint(t.Context()); established {
		t.Fatal("the divergence was established twice, so its line would repeat on every beat")
	}

	// AND THE SAME RECORD IS NOTHING: a node whose log is its own history
	// is never told otherwise.
	clean := appliedThrough(t, 3)
	if established, err := clean.runner.VerifyCheckpoint(t.Context()); established || err != nil {
		t.Fatalf("VerifyCheckpoint on the node's own history = (%v, %v)", established, err)
	}
	if err := clean.runner.StreamIdentity(); err != nil {
		t.Fatalf("the identity of a node on its own history is %v", err)
	}
}

// A FETCH THAT FAILED IS FOLLOWED BY A VERIFICATION.
//
// A broker restored under a RUNNING node takes the node's consumer with it, so
// the loop sees a failed fetch — and whatever it is handed past the checkpoint
// afterwards may be the other history, whether or not any reading ever found
// the end below the checkpoint.
func TestAFetchThatFailedIsFollowedByAVerification(t *testing.T) {
	t.Parallel()
	h := appliedThrough(t, 3)
	at := h.runner.Committed()

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	errs := make(chan error, 1)
	fetched := h.fetch.fetchCount()
	go func() { errs <- h.runner.Run(ctx) }()
	// THE LOOP IS FETCHING, so its start has verified the checkpoint
	// against the log as it was, and only the failure below can leave it
	// owing another look.
	for h.fetch.fetchCount() <= fetched && ctx.Err() == nil {
		time.Sleep(time.Millisecond)
	}
	// THE FAILURE FIRST, so the loop meets it before it meets the record:
	// the probe broker answers its next fetch with an error.
	h.fetch.mu.Lock()
	h.fetch.failures = 1
	h.fetch.mu.Unlock()
	h.fetch.rewrite(3, otherHistory)
	h.fetch.offerStored(4, otherHistory.Add(time.Second), env(4, "edit", "4", "op-other-4", 1))

	select {
	case err := <-errs:
		if !errors.Is(err, statelog.ErrLogDiverged) {
			t.Fatalf("the loop returned %v, want the divergence stop", err)
		}
	case <-ctx.Done():
		t.Fatal("the loop never stopped on the other history")
	}
	if appliedAt(h, 4) || h.runner.Committed() != at {
		t.Fatalf("the loop applied past its checkpoint after a failed fetch (at %s)",
			h.runner.Committed())
	}
}

// A REANCHOR ENDS THE DIVERGENCE, AND A JOIN THAT REPLACED NOTHING DOES NOT.
//
// The reanchor's checkpoint names the log's own record, so the rows and the log
// are one history again from there. A join that installed a donor's rows moves
// the checkpoint and is judged again by the loop; one that installed nothing
// leaves the rows the log diverged from — and clearing the verdict there would
// let a write through before the loop had looked again.
func TestAReanchorEndsTheDivergenceAndAJoinThatReplacedNothingDoesNot(t *testing.T) {
	t.Parallel()
	live := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	diverge := func(t *testing.T) *applyHarness {
		t.Helper()
		h := newApplyHarness(t, probeDomain{})
		h.rebuild(probeDomain{}, live)
		for seq := uint64(1); seq <= 3; seq++ {
			h.fetch.offer(seq, env(seq, "edit", fmt.Sprint(seq), fmt.Sprintf("op-%d", seq), 1))
		}
		if err := h.run(3); err != nil {
			t.Fatalf("run: %v", err)
		}
		h.fetch.rewrite(3, otherHistory)
		if established, err := h.runner.VerifyCheckpoint(t.Context()); !established || err != nil {
			t.Fatalf("VerifyCheckpoint = (%v, %v)", established, err)
		}
		return h
	}

	t.Run("a join that replaced nothing", func(t *testing.T) {
		t.Parallel()
		h := diverge(t)
		if err := h.runner.Rejoined(h.runner.Committed(), live, live); err != nil {
			t.Fatalf("Rejoined: %v", err)
		}
		if !errors.Is(h.runner.StreamIdentity(), statelog.ErrLogDiverged) {
			t.Fatalf("a join that left the same checkpoint cleared the divergence: %v",
				h.runner.StreamIdentity())
		}
	})
	t.Run("a join that installed a donor's checkpoint", func(t *testing.T) {
		t.Parallel()
		h := diverge(t)
		donor := h.runner.Committed()
		donor.Seq = 2
		if err := h.runner.Rejoined(donor, live, live); err != nil {
			t.Fatalf("Rejoined: %v", err)
		}
		if err := h.runner.StreamIdentity(); err != nil {
			t.Fatalf("a join onto another checkpoint kept a verdict about the old "+
				"one: %v", err)
		}
	})
	t.Run("a reanchor", func(t *testing.T) {
		t.Parallel()
		h := diverge(t)
		at := statelog.Position{Stream: probeStream, Generation: 2, Seq: 3}
		if err := h.runner.Reanchored(at, live, otherHistory); err != nil {
			t.Fatalf("Reanchored: %v", err)
		}
		if err := h.runner.StreamIdentity(); err != nil || h.runner.Diverged() {
			t.Fatalf("after the reanchor the identity is %v", err)
		}
		// THE NEW CHECKPOINT NAMES THE LOG'S RECORD, so verifying it is
		// the log's own history.
		if established, err := h.runner.VerifyCheckpoint(t.Context()); established || err != nil {
			t.Fatalf("VerifyCheckpoint after the reanchor = (%v, %v)", established, err)
		}
	})
}

// A CHECKPOINT THAT NAMES NO RECORD IS NEVER CALLED DIVERGED.
//
// A row older than the column names nothing, and comparing nothing with the
// log's record would stop every node the first time it booted this build.
func TestACheckpointThatNamesNoRecordIsNeverCalledDiverged(t *testing.T) {
	t.Parallel()
	h := appliedThrough(t, 3)
	if err := h.db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(),
			`UPDATE statelog_cursor SET stored_at = 0 WHERE stream = ?`, probeStream)
		return err
	}); err != nil {
		t.Fatalf("clear the record the checkpoint names: %v", err)
	}
	h.rebuild(probeDomain{}, time.Time{})
	h.fetch.offerStored(3, otherHistory, env(3, "edit", "3", "op-3", 1))
	h.fetch.offer(4, env(4, "edit", "4", "op-4", 1))
	if err := h.run(4); err != nil {
		t.Fatalf("a checkpoint naming no record stopped: %v", err)
	}
	// AND THE BATCH IT COMMITS NAMES ONE FROM THEN ON.
	if got := storedAtOf(t, h.db); !got.Equal(probeStoredAt(4)) {
		t.Fatalf("the checkpoint names %s after a batch, want %s", got, probeStoredAt(4))
	}
}

// A PEER'S TRUNCATION IS THE RUNNER'S WRITE FENCE, NOT ITS IDENTITY.
//
// A reading of the register that finds a peer holding records the log lost is
// established once and cleared once, so each of its two lines is written once;
// it refuses writes (Truncated) and leaves reads alone (StreamIdentity), since
// this node's rows are the log's own history; and a reanchor, which puts this
// node's rows at the head of a generation of their own, ends it.
func TestAPeersTruncationIsTheRunnersWriteFenceNotItsIdentity(t *testing.T) {
	t.Parallel()
	h := appliedThrough(t, 3)
	peer := &statelog.Truncation{Peer: "node-newer", Seq: 9, Last: 3}

	if established, cleared := h.runner.ObserveTruncation(peer); !established || cleared {
		t.Fatalf("the first reading = (established %v, cleared %v)", established, cleared)
	}
	if established, _ := h.runner.ObserveTruncation(peer); established {
		t.Fatal("the same reading established the truncation twice")
	}
	err := h.runner.Truncated()
	if !errors.Is(err, statelog.ErrLogTruncated) {
		t.Fatalf("Truncated = %v, want the truncation", err)
	}
	for _, want := range []string{"node-newer", "sequence 9", "ends at 3", "reanchor", "evict"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not name %q", err, want)
		}
	}
	if err := h.runner.StreamIdentity(); err != nil {
		t.Fatalf("a peer's truncation refused this node's reads: %v", err)
	}

	diverged := &statelog.Truncation{Peer: "node-newer", Seq: 3, Last: 5, Diverged: true}
	h.runner.ObserveTruncation(diverged)
	if err := h.runner.Truncated(); err == nil || !strings.Contains(err.Error(), "another record") {
		t.Fatalf("a diverged peer's truncation reads %v", err)
	}

	if _, cleared := h.runner.ObserveTruncation(nil); !cleared {
		t.Fatal("a reading that found no such peer did not clear the truncation")
	}
	if _, cleared := h.runner.ObserveTruncation(nil); cleared {
		t.Fatal("the truncation was cleared twice")
	}
	if err := h.runner.Truncated(); err != nil {
		t.Fatalf("Truncated after clearing = %v", err)
	}

	h.runner.ObserveTruncation(peer)
	at := statelog.Position{Stream: probeStream, Generation: 2, Seq: 3}
	if err := h.runner.Reanchored(at, time.Now(), probeStoredAt(3)); err != nil {
		t.Fatalf("Reanchored: %v", err)
	}
	if err := h.runner.Truncated(); err != nil {
		t.Fatalf("after a reanchor the truncation still refuses: %v", err)
	}
}
