package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/objstore"
)

// THE SCRUB'S WAIT IS DERIVED FROM THE ARCHIVE'S BOUND. Its size term is the
// archive at executorkeep.SessionMaxBytes read twice out of its box at the
// box-read floor, a mebibyte begun a second, and the wait is that, the spool's
// room and the slowest plaintext, rounded up to the minute: 96 s and six
// minutes at today's bound, as docs/concepts/code-sandbox.md states them. A
// bound that moves moves both, and this case names what to restate.
//
// Mutation: write the size term as a literal, read the archive once, count a
// mebibyte begun as none, or round the wait down.
func TestTheScrubWaitIsDerivedFromTheArchivesBound(t *testing.T) {
	t.Parallel()
	for n, want := range map[int64]time.Duration{0: 0, 1: time.Second, objstore.MiB: time.Second,
		objstore.MiB + 1: 2 * time.Second} {
		if got := archiveReadFor(n); got != want {
			t.Errorf("%d bytes of archive are given %s to come out of a box, want %s", n, got, want)
		}
	}
	reads := 2 * archiveReadFor(executorkeep.SessionMaxBytes)
	if scrubArchiveReads != reads {
		t.Errorf("the scrub's size term is %s, want the archive at its bound read twice at the box-read floor, %s",
			scrubArchiveReads, reads)
	}
	sum := spoolRoomWait + reads + scrubPlainSlowest
	if SessionScrubWait < sum || SessionScrubWait-sum >= time.Minute || SessionScrubWait%time.Minute != 0 {
		t.Errorf("the scrub's wait is %s, want its terms, %s, rounded up to the minute", SessionScrubWait, sum)
	}
	if reads != 96*time.Second || SessionScrubWait != 6*time.Minute {
		t.Errorf("the scrub reads its archive for %s and waits %s at today's bound, and the docs state 96 s and "+
			"six minutes: restate them (code-sandbox.md, \"Six minutes for the scrub\"; ADR-0073)", reads,
			SessionScrubWait)
	}
}

// A RELAUNCH'S WAIT FOR A CAPTURE COUNTS WHAT ITS SIZE STILL NEEDS once its
// place is reserved, each step at its floor: the scrub's two readings of its
// archive and slowest plaintext, the digest and one keep of the sealed session
// the archive may become. Each is the docs' figure, to the second.
//
// Mutation: drop a term, read the archive once, or size the keep at the bound.
func TestARelaunchsWaitForACaptureCountsWhatItsSizeStillNeeds(t *testing.T) {
	t.Parallel()
	keep := func(paces time.Duration) time.Duration {
		return 2*sessionRecordBudget + paces*objstore.MiBPace + sessionRecordBudget
	}
	bound := executorkeep.SessionMaxBytes / objstore.MiB
	for _, c := range []struct {
		name      string
		got, want time.Duration
		docs      string
	}{
		{"half a mebibyte", captureRestFor(512 << 10),
			2*time.Second + scrubPlainSlowest + discardGrace + keep(1), "5m35s"},
		{"at the bound", captureRestFor(executorkeep.SessionMaxBytes),
			2*time.Duration(bound)*time.Second + scrubPlainSlowest + discardGrace + keep(time.Duration(bound)+1),
			"31m9s"},
		{"a keep at the bound", keepBudgetFor(executorkeep.SealedMaxBytes), SessionKeepBudget, "26m0s"},
	} {
		if c.got != c.want {
			t.Errorf("%s: %s, want its terms, %s", c.name, c.got, c.want)
		}
		if c.got.String() != c.docs {
			t.Errorf("%s is %s at today's bounds, and the docs state %s: restate them", c.name, c.got, c.docs)
		}
	}
}

// A RELAUNCH WAITS AS LONG AS THE LONGEST KEEP IT DEPENDS ON NEEDS: a
// capture's or a save's rest for its archive's size, the longer where both are
// being kept, and an ask's wait where neither is; a keep it does not depend on,
// an earlier job's beside a capture the turn kept, adds nothing.
//
// Mutation: size the wait by the newest stamp alone, count a stamp the
// relaunch does not depend on, or let a small capture cut an ask's wait short.
func TestARelaunchWaitsAsLongAsTheLongestKeepItDependsOnNeeds(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)
	coordinator := &Coordinator{relaunchWait: relaunchAskWait, captureRest: captureRestFor}
	begun := func(launch, place, size int64) *SessionCapture {
		return &SessionCapture{LaunchSeq: launch, CaptureSeq: place, Launch: fmt.Sprintf("launch-%d", launch),
			State: CaptureBegun, At: now, ArchiveSize: size}
	}
	kept := &SessionCapture{LaunchSeq: 1, CaptureSeq: 1, Launch: "launch-1", State: CaptureLanded, At: now}
	small, large := int64(512<<10), int64(executorkeep.SessionMaxBytes)
	for _, c := range []struct {
		name string
		run  PendingRun
		want time.Duration
	}{
		{"nothing being kept", PendingRun{LaunchSeq: 2, Kept: kept}, relaunchAskWait},
		{"the followed job's capture", PendingRun{LaunchSeq: 2, Kept: kept, Capture: begun(2, 2, small)},
			captureRestFor(small)},
		{"its save beside a smaller final take", PendingRun{LaunchSeq: 2, Kept: kept, Capture: begun(2, 3, small),
			Saving: begun(2, 2, large)}, captureRestFor(large)},
		{"an earlier job's capture beside one the turn kept",
			PendingRun{LaunchSeq: 3, Kept: kept, Capture: begun(2, 2, large)}, relaunchAskWait},
		{"an earlier job's capture, the turn having kept none",
			PendingRun{LaunchSeq: 3, Capture: begun(2, 1, large)}, captureRestFor(large)},
	} {
		if got := coordinator.relaunchWaitOf(c.run, now); got != c.want {
			t.Errorf("%s: the relaunch waits %s, want %s", c.name, got, c.want)
		}
	}
	if captureRestFor(0) <= relaunchAskWait {
		t.Errorf("a capture of no bytes is waited %s, within an ask's %s: an ask's wait would cut it short",
			captureRestFor(0), relaunchAskWait)
	}
}

// refusedPuts is a keep whose store refuses the first fails uploads with
// refusal, every one where fails is negative, noting each attempt's instant
// and the object it named; every other call is the keep underneath's.
type refusedPuts struct {
	*sessionKeep
	refusal error
	fails   int

	mu       sync.Mutex
	attempts []time.Time
	keys     []objstore.Key
}

func (k *refusedPuts) Upload(ctx context.Context, spool executorkeep.SealedSpool,
	sealed io.Reader) (executorkeep.SealedObject, error) {

	k.mu.Lock()
	k.attempts = append(k.attempts, time.Now())
	k.keys = append(k.keys, spool.Key)
	refused := k.fails < 0 || len(k.attempts) <= k.fails
	k.mu.Unlock()
	if refused {
		_, _ = io.Copy(io.Discard, sealed)
		return executorkeep.SealedObject{}, k.refusal
	}
	return k.sessionKeep.Upload(ctx, spool, sealed)
}

func (k *refusedPuts) made() ([]time.Time, []objstore.Key) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]time.Time(nil), k.attempts...), append([]objstore.Key(nil), k.keys...)
}

// tookNone is a put the object store took none of, as [objstore.Store]
// words one its backend did not answer.
var tookNone = fmt.Errorf("objstore: store the object: %w: %w", objstore.ErrUnanswered,
	errors.New("the broker did not answer"))

// AN UPLOAD THE STORE TOOK NONE OF IS MADE AGAIN WITHIN THE KEEP'S BUDGET, into
// the same object, and lands; one it never takes is made again for as long as
// the budget left holds the session at the store's slowest pace and its
// record's room, then refused, with no record sent; a refusal that is not the
// store's silence (its room spent) is final at once. Exact, in a bubble.
//
// Mutation: upload once, retry every refusal, retry past the record's room,
// stop retrying while the budget still holds another attempt, or mint another
// object, and a case goes red.
func TestAnUploadTheStoreTookNoneOfIsMadeAgainWithinTheKeepsBudget(t *testing.T) {
	t.Parallel()
	keepWith := func(t *testing.T, refusal error, fails int) (*coordRig, *sessionWrites, *refusedPuts,
		*spooledCapture) {

		t.Helper()
		rig, writes, base := keepingRig(t)
		run := rig.launchKept(t, "t1")
		spooled, _ := rig.coordinator.takeCapture(t.Context(), run, rig.provider.Box(run.SandboxID), rig.runner,
			readCapture(BoxReadStop, Result{}, true))
		if spooled == nil || spooled.standby {
			t.Fatalf("the stop's read spooled %+v, want the capture's own place", spooled)
		}
		puts := &refusedPuts{sessionKeep: base, refusal: refusal, fails: fails}
		rig.coordinator.keep = puts
		return rig, writes, puts, spooled
	}
	t.Run("taken on the third attempt", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			rig, writes, puts, spooled := keepWith(t, tookNone, 2)
			if kept := rig.coordinator.keepCapture(t.Context(), spooled); kept == nil || kept.NotKept != "" {
				t.Fatalf("the keep came to %+v, want the session kept on the attempt the store took", kept)
			}
			attempts, keys := puts.made()
			if len(attempts) != 3 {
				t.Errorf("uploaded %d times, want the two the store took none of and the one it took", len(attempts))
			}
			for _, key := range keys {
				if key != spooled.spool.Key {
					t.Errorf("an attempt named %s, want every one the object the session was sealed for, %s", key,
						spooled.spool.Key)
				}
			}
			if c := rig.get("t1").Capture; c == nil || c.State != CaptureLanded {
				t.Errorf("the row holds %+v, want the capture landed", c)
			}
			if captured := writes.captured(); len(captured) != 1 {
				t.Errorf("recorded %d captures, want the one", len(captured))
			}
		})
	})
	t.Run("never taken", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			rig, writes, puts, spooled := keepWith(t, tookNone, -1)
			began := time.Now()
			kept := rig.coordinator.keepCapture(t.Context(), spooled)
			ended := time.Now()
			if kept == nil || kept.Loss != executorkeep.LossStoreRefused || !strings.Contains(kept.NotKept, "attempt") {
				t.Fatalf("the keep came to %+v, want the capture refused as the store's, naming its attempts", kept)
			}
			attempts, _ := puts.made()
			room := objstore.PaceFor(spooled.spool.Size) + rig.coordinator.recordBudget
			last, lastBy := attempts[len(attempts)-1], began.Add(SessionKeepBudget-room)
			if len(attempts) < 2 || last.After(lastBy) || !last.After(lastBy.Add(-sessionRetryCeiling)) {
				t.Errorf("uploaded %d times, the last %s into the keep, want attempts until the budget left could "+
					"not hold another at the store's pace and the record's room, the last within %s before %s",
					len(attempts), last.Sub(began), sessionRetryCeiling, lastBy.Sub(began))
			}
			if ended.Sub(began) > SessionKeepBudget {
				t.Errorf("the keep ended %s after it began, past its budget, %s", ended.Sub(began), SessionKeepBudget)
			}
			if c := rig.get("t1").Capture; c == nil || c.State != CaptureRefused {
				t.Errorf("the row holds %+v, want the capture refused", c)
			}
			if captured := writes.captured(); len(captured) != 0 {
				t.Errorf("recorded %d captures of a session never uploaded", len(captured))
			}
		})
	})
	t.Run("refused for the store's room", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			rig, _, puts, spooled := keepWith(t, &objstore.Full{Err: errors.New("the bucket's quota is spent")}, -1)
			kept := rig.coordinator.keepCapture(t.Context(), spooled)
			if kept == nil || kept.Loss != executorkeep.LossStoreRefused || strings.Contains(kept.NotKept, "attempt") {
				t.Fatalf("the keep came to %+v, want the capture refused for the store's room at once", kept)
			}
			if attempts, _ := puts.made(); len(attempts) != 1 {
				t.Errorf("uploaded %d times into a full store, want once", len(attempts))
			}
			if c := rig.get("t1").Capture; c == nil || c.State != CaptureRefused {
				t.Errorf("the row holds %+v, want the capture refused", c)
			}
		})
	})
}
