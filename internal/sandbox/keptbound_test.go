package sandbox

import (
	"cmp"
	"context"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord/memory"
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
	const (
		below = "node seats-1 says capture stamp version 1, below the 2 a take at the kept bound is asked again at"
		log   = "node data-2 reads the executor log to record version 12, below the 13 a capture naming media " +
			"is stamped at"
	)
	for name, tc := range map[string]struct {
		unread, media, says string
		lands               bool
	}{
		"a stamp reader below the kept bound": {says: "no longer reads a capture kept past 48 MiB"},
		"a log reader below the media's version": {media: log,
			says: "no longer reads a capture taken at the kept bound, which keeps its media apart"},
		"readers unread in their room": {unread: "the position register could not be read", lands: true},
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
				switch {
				case tc.media != "":
					keep.policy.MediaApart, keep.policy.MediaBelow = false, tc.media
				case tc.unread == "":
					keep.policy.KeptBelow = below
				default:
					keep.policy.MediaApart = false
				}
				keep.mu.Unlock()
				kept := rig.coordinator.keepCapture(t.Context(), spooled)
				captured, stamp := writes.captured(), rig.get("t1").Capture
				switch {
				case tc.lands && (kept == nil || kept.NotKept != "" || len(captured) != 1):
					t.Errorf("the keep came to %+v after %d records, want it landed on its take's answer", kept,
						len(captured))
				case !tc.lands && (kept == nil || !strings.Contains(kept.NotKept, cmp.Or(tc.media, below)) ||
					!strings.Contains(kept.NotKept, tc.says) || len(captured) != 0 || stamp == nil ||
					stamp.Loss != executorkeep.LossKeepFailed):
					t.Errorf("the keep came to %+v after %d records, stamped %+v, want it refused unsent as "+
						"keep_failed, naming the node below and saying it %s", kept, len(captured), stamp, tc.says)
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

// EVERY TAKE IS GIVEN ITS OWN BOUND'S BUDGET, A COLLECTION'S AND A SAVE'S: the
// take runs under its bound's pack and scrub ([takeBound]) and its
// reservation's room beside it, 480 s at the text bound and 600 s only where
// the policy reads the kept bound, so a gated take never holds a v1 asker
// past the 480 s it allows.
//
// Mutation: size either path's take at the kept bound, or at the text bound
// under the kept policy, and a case goes red.
func TestEveryTakeIsGivenItsOwnBoundsBudget(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		apart, kept bool
		want        takeBound
	}{
		"every reader at the kept bound":     {apart: true, kept: true, want: keptBound},
		"media apart, a reader below it":     {apart: true, want: textBound},
		"the kept bound without media apart": {kept: true, want: textBound},
	} {
		for path, take := range map[string]func(t *testing.T, rig *coordRig){
			"a collection's": func(t *testing.T, rig *coordRig) {
				if spooled, _ := finalTake(t, rig, rig.launchKept(t, "t1"), rig.runner); spooled == nil {
					t.Fatal("the collection's take spooled nothing, want its session")
				}
			},
			"a save's": func(t *testing.T, rig *coordRig) {
				rig.launchKept(t, "t1")
				if out := saveOf(rig, "t1", nil); out.class != saveLanded {
					t.Fatalf("the save came to %+v, want it landed", out)
				}
			},
		} {
			t.Run(name+", "+path, func(t *testing.T) {
				t.Parallel()
				inBubble(t, func(t *testing.T) {
					rig, _, keep := savingRig(t, memory.NewFleet(), newTakerStore())
					keep.mu.Lock()
					keep.policy.MediaApart, keep.policy.KeptBound = tc.apart, tc.kept
					keep.mu.Unlock()
					var room time.Duration
					rig.runner.Session.Taking = func(ctx context.Context) {
						if by, ok := ctx.Deadline(); ok {
							room = time.Until(by)
						}
					}
					take(t, rig)
					// Less what the case's reads before the pack's end took on the
					// bubble's clock, far under the two bounds' two minutes apart.
					if most := tc.want.take + rig.coordinator.reserveRoom(); room > most ||
						room <= most-time.Minute {
						t.Errorf("the take ran within %s, want its bound's %s and its reservation's room, "+
							"%s", room, tc.want.take, most)
					}
				})
			})
		}
	}
}

// A FIGURE SIZED AT NO BOUND IS REFUSED, never read as either: a caller that
// named none is a bug, and either guess sizes its waits on another bound's
// figures ([captureBound]).
//
// Mutation: read the zero value, or any value past the two, as either bound,
// and a case goes red.
func TestAFigureSizedAtNoBoundIsRefused(t *testing.T) {
	t.Parallel()
	for name, figure := range map[string]func(captureBound){
		"a take's life":                    func(b captureBound) { takeLifeWith(b) },
		"a takeover's life":                func(b captureBound) { takeOverLifeWith(b) },
		"a capture's life":                 func(b captureBound) { captureLifeAt(b) },
		"a standby's wait":                 func(b captureBound) { standbyWaitFor(b) },
		"a standby's window, at its own":   func(b captureBound) { standbyWindowFor(b, atTextBound) },
		"a standby's window, at its stood": func(b captureBound) { standbyWindowFor(atTextBound, b) },
		"a relaunch's wait":                func(b captureBound) { captureRestFor(1, 0, b) },
		"a seat's count of a session":      func(b captureBound) { countedFor(1, b) },
		"a take's refusal for its time": func(b captureBound) {
			ended, cancel := context.WithCancel(t.Context())
			cancel()
			takeRefusal(ended, context.Canceled, b)
		},
	} {
		for _, bound := range []captureBound{0, atKeptBound + 1} {
			t.Run(fmt.Sprintf("%s at %d", name, bound), func(t *testing.T) {
				t.Parallel()
				defer func() {
					if recover() == nil {
						t.Errorf("%s was sized at bound %d, want it refused", name, bound)
					}
				}()
				figure(bound)
			})
		}
	}
}

// A READ STOOD BY AT NO BOUND IS REFUSED before it asks the store anything:
// stood by at none, its spool would read as a read holding its own place
// ([spooledCapture.stoodBy]), so nothing waits for the capture it stood by for.
//
// Mutation: drop standBy's check, and the read asks to stand by and is answered.
func TestAReadStoodByAtNoBoundIsRefused(t *testing.T) {
	t.Parallel()
	for _, bound := range []captureBound{0, atKeptBound + 1} {
		t.Run(fmt.Sprintf("at %d", bound), func(t *testing.T) {
			t.Parallel()
			inBubble(t, func(t *testing.T) {
				rig, _, _ := keepingRig(t)
				run := rig.launchKept(t, "t1")
				stamp := SessionCapture{LaunchSeq: run.LaunchSeq, CaptureSeq: 1, Launch: run.LaunchID,
					State: CaptureBegun, At: rig.coordinator.now(), Taker: takingProcess, TakerEpoch: 1,
					Harness: "claude-code", SessionID: "s-1"}
				defer func() {
					if recover() == nil {
						t.Errorf("a read stood by at bound %d, want it refused", bound)
					}
				}()
				rig.coordinator.standBy(t.Context(), time.Time{}, time.Time{}, run, stamp, TakenSession{},
					capturePoint{}, nil, nil, bound)
			})
		})
	}
}
