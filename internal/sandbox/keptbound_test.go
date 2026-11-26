package sandbox

import (
	"context"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/executorkeep"
)

// A TAKE KEEPS THE KEPT BOUND ONLY WHERE THE POLICY READS IT, AND ONLY BESIDE
// ITS MEDIA APART: a take under KeptBound and MediaApart admits its box's
// archive up to executorkeep.SessionKeptMaxBytes, paces its media to
// executorkeep.KeptPaceMiB, stamps its place at the kept bound and keeps within
// that bound's budget; under either alone it keeps the text bound, its
// archive within executorkeep.SessionMaxBytes, 4b's pace and 29 minutes.
//
// Mutation: read KeptBound without MediaApart, take or pace at one bound and
// keep at the other, or stamp no bound, and a case goes red.
func TestATakeKeepsTheKeptBoundOnlyWhereThePolicyReadsIt(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		apart, kept bool
		want        takeBound
	}{
		"every reader at the kept bound":     {apart: true, kept: true, want: keptBound},
		"media apart, a reader below it":     {apart: true, want: textBound},
		"the kept bound without media apart": {kept: true, want: textBound},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			inBubble(t, func(t *testing.T) {
				rig, _, keep := keepingRig(t)
				var room time.Duration
				keep.mu.Lock()
				keep.policy.MediaApart, keep.policy.KeptBound = tc.apart, tc.kept
				keep.uploadHook = func(ctx context.Context) {
					if by, ok := ctx.Deadline(); ok {
						room = time.Until(by)
					}
				}
				keep.mu.Unlock()
				spooled, taken := finalTake(t, rig, rig.launchKept(t, "t1"), rig.runner)
				if spooled == nil || taken == nil || taken.NotKept != "" {
					t.Fatalf("the take came to %+v, want its session spooled", taken)
				}
				takes := rig.runner.Takes()
				take := takes[len(takes)-1]
				if take.MaxBytes != tc.want.archive || (tc.apart && take.KeepPaceMiB != tc.want.paceMiB) {
					t.Errorf("the take admitted %d bytes paced at %d, want %d at %d", take.MaxBytes,
						take.KeepPaceMiB, tc.want.archive, tc.want.paceMiB)
				}
				if got := rig.get("t1").Capture; got == nil || got.KeptBound != (tc.want == keptBound) {
					t.Errorf("the capture was stamped %+v, want it at the kept bound %t", got, tc.want == keptBound)
				}
				rig.coordinator.keepCapture(t.Context(), spooled)
				if room != tc.want.keep {
					t.Errorf("the keep uploaded within %s, want its bound's %s", room, tc.want.keep)
				}
			})
		})
	}
}

// A KEPT-BOUND RECORD IS REFUSED ONCE A READER FALLS BELOW, AND ONLY THEN: a
// capture taken at the kept bound, naming no media, is read again before its
// record as one naming media is; a node that joined after the take and says a
// stamp version below the kept bound's refuses it as keep_failed, naming the
// node, nothing sent; readers unread let it land on its take's answer.
//
// Mutation: ask again only a capture naming media, or refuse an unknown
// answer, and a case goes red.
func TestAKeptBoundRecordIsRefusedOnceAReaderFallsBelow(t *testing.T) {
	t.Parallel()
	const below = "node seats-1 says capture stamp version 1, below the 2 a take at the kept bound is asked again at"
	for name, tc := range map[string]struct {
		unread string
		lands  bool
	}{
		"a stamp reader below the kept bound": {},
		"readers unread in their room":        {unread: "the position register could not be read", lands: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			inBubble(t, func(t *testing.T) {
				rig, writes, keep := keepingRig(t)
				keep.mu.Lock()
				keep.policy.MediaApart, keep.policy.KeptBound = true, true
				keep.mu.Unlock()
				spooled, taken := finalTake(t, rig, rig.launchKept(t, "t1"), rig.runner)
				if spooled == nil || !spooled.stamp.KeptBound || len(spooled.media) != 0 {
					t.Fatalf("the take came to %+v, want it spooled at the kept bound naming no media", taken)
				}
				keep.mu.Lock()
				keep.policy.KeptBound, keep.policy.MediaUnread, keep.policy.KeptBelow = false, tc.unread, ""
				if tc.unread == "" {
					keep.policy.KeptBelow = below
				} else {
					keep.policy.MediaApart = false
				}
				keep.mu.Unlock()
				kept := rig.coordinator.keepCapture(t.Context(), spooled)
				captured, stamp := writes.captured(), rig.get("t1").Capture
				switch {
				case tc.lands && (kept == nil || kept.NotKept != "" || len(captured) != 1):
					t.Errorf("the keep came to %+v after %d records, want it landed on its take's answer", kept,
						len(captured))
				case !tc.lands && (kept == nil || !strings.Contains(kept.NotKept, below) || len(captured) != 0 ||
					stamp == nil || stamp.Loss != executorkeep.LossKeepFailed):
					t.Errorf("the keep came to %+v after %d records, stamped %+v, want it refused unsent as "+
						"keep_failed, naming the node below", kept, len(captured), stamp)
				}
			})
		})
	}
}

// A CAPTURE AT THE KEPT BOUND IS HELD TO ITS SEAT'S BUDGET: the kept bound
// admits a take of three payloads kept apart, and the seat's budget
// (max_bytes_per_seat) still decides whether it is kept: within a budget the
// case sets, it lands, all three uploaded; past one, it is refused naming the
// setting, nothing uploaded. A capture up to 94 MiB meets the 64 MiB default
// the same way.
//
// Mutation: leave a kept-bound capture's media out of the budget's count, and
// the refusal goes red.
func TestACaptureAtTheKeptBoundIsHeldToItsSeatsBudget(t *testing.T) {
	t.Parallel()
	raw := make([]byte, 96<<10)
	rand.NewChaCha8([32]byte{9}).Read(raw)
	whole, body := mediaBody(t, "text", mediaPart+string(raw[:32<<10]), "between",
		mediaPart+string(raw[32<<10:64<<10]), "and", mediaPart+string(raw[64<<10:]), "end")
	for name, tc := range map[string]struct {
		budget int64
		kept   bool
	}{
		"within the budget the case sets": {budget: 1 << 20, kept: true},
		"past it":                         {budget: 64 << 10},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			inBubble(t, func(t *testing.T) {
				rig, writes, keep := mediaRig(t, whole, body)
				keep.mu.Lock()
				keep.policy.KeptBound, keep.policy.SeatBudget = true, tc.budget
				keep.mu.Unlock()
				spooled, taken := finalTake(t, rig, rig.launchKept(t, "t1"), rig.runner)
				if spooled == nil || !spooled.stamp.KeptBound || len(spooled.media) != 3 {
					t.Fatalf("the take came to %+v, want it at the kept bound with three payloads apart", taken)
				}
				kept := rig.coordinator.keepCapture(t.Context(), spooled)
				switch {
				case tc.kept && (kept == nil || kept.NotKept != "" || len(keep.uploads()) != 4):
					t.Errorf("the keep came to %+v with %d uploads, want it landed, its text and three payloads",
						kept, len(keep.uploads()))
				case !tc.kept && (kept == nil || !strings.Contains(kept.NotKept, "max_bytes_per_seat") ||
					len(keep.uploads()) != 0 || len(writes.captured()) != 0):
					t.Errorf("the keep came to %+v, want it refused naming max_bytes_per_seat, nothing uploaded",
						kept)
				}
				if stamp := rig.get("t1").Capture; !tc.kept && (stamp == nil || stamp.State != CaptureRefused ||
					stamp.Loss != executorkeep.LossStoreRefused || !strings.Contains(stamp.Reason, "max_bytes_per_seat")) {
					t.Errorf("the row holds %+v, want the capture refused as the store's, its words naming "+
						"max_bytes_per_seat", stamp)
				}
			})
		})
	}
}

// AT THE DEFAULT BUDGET, A CAPTURE AT THE KEPT BOUND MEETS THE SEAT'S REFUSAL:
// the default max_bytes_per_seat is below what the kept bound admits, so a
// session just past it, taken at the kept bound by a fake take with no scrub,
// is refused naming the setting, nothing uploaded.
//
// Mutation: raise the default to 94 MiB, or skip the seat's budget for a
// kept-bound capture, and it goes red.
func TestACaptureAtTheKeptBoundMeetsTheDefaultSeatBudget(t *testing.T) {
	t.Parallel()
	if config.DefaultSessionBytesPerSeat >= executorkeep.KeptSealedMaxBytes {
		t.Fatalf("the default max_bytes_per_seat, %d bytes, holds a capture at the kept bound (%d sealed): a "+
			"kept-bound capture can no longer meet the seat's refusal at the default",
			config.DefaultSessionBytesPerSeat, executorkeep.KeptSealedMaxBytes)
	}
	inBubble(t, func(t *testing.T) {
		rig, writes, keep := keepingRig(t)
		// JUST PAST THE DEFAULT AS ITS DOC STATES IT, 64 MiB: a default
		// raised past the session lets it land.
		body := make([]byte, 64<<20+64<<10)
		keep.mu.Lock()
		keep.policy.MediaApart, keep.policy.KeptBound = true, true
		keep.policy.SeatBudget = config.DefaultSessionBytesPerSeat
		keep.mu.Unlock()
		rig.runner.mu.Lock()
		rig.runner.Session.Body, rig.runner.Session.Archive.Size = body, int64(len(body))
		rig.runner.mu.Unlock()
		spooled, taken := finalTake(t, rig, rig.launchKept(t, "t1"), rig.runner)
		if spooled == nil || !spooled.stamp.KeptBound {
			t.Fatalf("the take came to %+v, want it spooled at the kept bound", taken)
		}
		kept := rig.coordinator.keepCapture(t.Context(), spooled)
		stamp := rig.get("t1").Capture
		if kept == nil || !strings.Contains(kept.NotKept, "max_bytes_per_seat") || len(keep.uploads()) != 0 ||
			len(writes.captured()) != 0 || stamp == nil || stamp.State != CaptureRefused ||
			!strings.Contains(stamp.Reason, "max_bytes_per_seat") {
			t.Errorf("a %d-byte capture at the default budget came to %+v, stamped %+v, want it refused naming "+
				"max_bytes_per_seat, nothing uploaded", len(body), kept, stamp)
		}
	})
}
