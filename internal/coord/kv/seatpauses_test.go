package kv

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
)

// AN UNREACHABLE STORE IS NOT UNPAUSED.
//
// "This seat has no pause" lets the seat take work; "the store could not be
// read" says nothing about it. A seat a person stopped because it was doing
// damage must not start again because a read failed, so every read answers the
// outage as an error that is not a false — the one listing included, which
// every node's cache is rebuilt from.
func TestAnUnreachableStoreIsNotUnpaused(t *testing.T) {
	t.Parallel()
	nc := embeddedNATS(t)
	store := openFleet(t, nc)
	swe := uuid.MustParse("7a1e4c90-2b3d-5f68-9a0c-4e5d6f7a8b9c")
	// BY A SEAT'S HANDLE, kind human, as a signed-in person is recorded:
	// every person holds a human seat (ADR-0032), and a login written as
	// kind human is no principal [iam.ActorFor] makes.
	if _, created, err := store.CreateSeatPause(t.Context(), coord.SeatPause{
		Seat: swe, By: "jane-doe", ByKind: iam.ActorHuman, At: time.Now(),
	}); err != nil || !created {
		t.Fatalf("CreateSeatPause = (%v, %v)", created, err)
	}

	dead, cancel := context.WithCancel(t.Context())
	cancel()
	p, found, err := store.SeatPause(dead, swe)
	if err == nil {
		t.Fatalf("SeatPause on a dead context = (%+v, %v) with no error: an outage "+
			"answered as a fact about the seat", p, found)
	}
	if !errors.Is(err, coord.ErrUnavailable) {
		t.Errorf("err = %v, want it to wrap ErrUnavailable so a caller can tell an "+
			"outage from a refusal", err)
	}
	if all, err := store.ListSeatPauses(dead); err == nil {
		t.Fatalf("ListSeatPauses on a dead context = %+v with no error: every node "+
			"rebuilding its cache from this would read the seat as free", all)
	}

	// And the same after the connection itself is gone.
	nc.Close()
	if _, found, err := store.SeatPause(t.Context(), swe); err == nil {
		t.Fatalf("SeatPause over a closed connection answered found=%v with no error", found)
	}
	if gone, err := store.DeleteSeatPause(t.Context(), swe, 1); err == nil || gone {
		t.Fatalf("DeleteSeatPause over a closed connection = (%v, %v), want an error "+
			"that is not a lost race", gone, err)
	}
}

// A PAUSE WATCH WHOSE PASS GOES QUIET STILL GIVES ITS FIRST ANSWER, WHOLE.
//
// The watch ended its first answer on the client's end marker and nothing
// else, and the client sends that marker on a delivery — so a watch whose
// pending messages were removed before it reached them, a resume's marker
// swept from the register, never ended it. The node's copy stayed unread, and
// it deferred every delivery until somebody next paused or resumed a seat. The
// pass here hands over one of three pauses and goes silent, as that consumer
// does: the answer must still come inside the caller's bounded context, and
// hold the two pauses the pass never delivered, read from the leader — one
// read each and no more.
func TestAPauseWatchWhosePassGoesQuietStillAnswersWhole(t *testing.T) {
	t.Parallel()
	nc := embeddedNATS(t)
	f := openFleet(t, nc)
	for _, name := range []string{"ops", "qa", "swe"} {
		pauseSeat(t, f, name)
	}
	pass := recordPass(t.Context(), t, f.positions)
	if len(pass) != 3 {
		t.Fatalf("setup: the recorded pass holds %d entries, want the 3 pauses", len(pass))
	}

	ctx, cancel := context.WithTimeout(t.Context(), 4*passIdle)
	defer cancel()
	wire := countWireReads(t, nc, f.positions)
	f.positions = quietKV{KeyValue: f.positions, pass: pass[:1]}
	started := time.Now()
	updates, err := f.WatchSeatPauses(ctx)
	if err != nil {
		t.Fatalf("a pause watch whose pass went quiet after one entry held its "+
			"caller for %v and failed: %v", time.Since(started), err)
	}
	answer := firstPauseAnswer(ctx, t, updates)
	if want := inSeatOrder("ops", "qa", "swe"); !slices.Equal(namesOf(answer), want) {
		t.Fatalf("the first answer paused %v, want %v: a pause missing from it is "+
			"a hold the node lifts and a seat it lets take work", namesOf(answer), want)
	}
	if n := wire.leaderReads(t); n != 2 {
		t.Errorf("the watch sent the leader %d reads, want 2 — one for each pause "+
			"the pass did not deliver, and none for the one it did", n)
	}
}

// A PAUSE WATCH'S FIRST ANSWER IS CERTIFIED, AND AN OLDER REVISION AFTER IT IS
// NOT A CHANGE.
//
// The client ends its initial values on a count guess, which on a register
// that keeps one revision per key can come while a pause live throughout is
// still ahead of the cursor — and the first answer REPLACES a node's copy, so
// a pause missing from it is a hold lifted and a seat let loose until the
// consumer reaches the key. The pass here ends before delivering either of
// two pauses, as such a guess can: the answer must still hold both, each as
// the leader holds it — the amended one with its amendment.
//
// The consumer then delivers what a pass behind the leader would: the pause as
// it stood BEFORE the amendment. That is not a change, and sent as one it
// would undo the amendment on every node until the consumer caught up. A pause
// taken after the answer is one, and is the next update.
func TestAPauseWatchsFirstAnswerIsCertifiedAndNotUndoneByAnOlderRevision(t *testing.T) {
	t.Parallel()
	nc := embeddedNATS(t)
	f := openFleet(t, nc)
	ctx, cancel := context.WithTimeout(t.Context(), 4*passIdle)
	defer cancel()

	pauseSeat(t, f, "ops")
	swe := pauseSeat(t, f, "swe")
	var stale jetstream.KeyValueEntry
	for _, kve := range recordPass(ctx, t, f.positions) {
		if kve.Key() == coord.SeatPauseKey(pausedSeat("swe")) {
			stale = kve
		}
	}
	if stale == nil || stale.Revision() != swe.Version {
		t.Fatalf("setup: the recorded pass holds swe as %v, want its pause at %d", stale, swe.Version)
	}
	swe.StopRunning = true
	amended, ok, err := f.UpdateSeatPause(ctx, swe)
	if err != nil || !ok {
		t.Fatalf("UpdateSeatPause = (%v, %v)", ok, err)
	}

	script := make(chan jetstream.KeyValueEntry, 3)
	script <- nil // the end of the initial values, before either pause
	f.positions = scriptedKV{KeyValue: f.positions, script: script}
	updates, err := f.WatchSeatPauses(ctx)
	if err != nil {
		t.Fatalf("WatchSeatPauses: %v", err)
	}
	answer := firstPauseAnswer(ctx, t, updates)
	if want := inSeatOrder("ops", "swe"); !slices.Equal(namesOf(answer), want) {
		t.Fatalf("the first answer paused %v, want %v: the client's marker came "+
			"before either pause, and an answer that trusted it holds neither",
			namesOf(answer), want)
	}
	for _, u := range answer {
		if u.Seat != pausedSeat("swe") {
			continue
		}
		if got := u.Pause; !got.StopRunning || got.Version != amended.Version {
			t.Errorf("swe answered as %+v, want the amended pause at %d", *got, amended.Version)
		}
	}

	qa := pauseSeat(t, f, "qa")
	fresh, err := getLatest(ctx, f.js, f.positions, coord.SeatPauseKey(pausedSeat("qa")))
	if err != nil {
		t.Fatalf("read qa's pause: %v", err)
	}
	script <- stale
	script <- fresh
	u := nextPauseUpdate(ctx, t, updates)
	if u.Seat != pausedSeat("qa") || u.Pause == nil || u.Pause.Version != qa.Version {
		t.Fatalf("the update after an older revision of swe and qa's new pause = %+v, "+
			"want qa's pause: swe's older revision is not a change, and sent as one "+
			"it undoes swe's amendment", u)
	}
}

// scriptedKV is a bucket whose watch delivers what the test sends it, in the
// order sent, and never ends — a nil is the client's end of the initial values.
// The index read, the certifying reads and every write are the broker's own.
type scriptedKV struct {
	jetstream.KeyValue
	script chan jetstream.KeyValueEntry
}

func (k scriptedKV) Watch(context.Context, string, ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	return stubWatcher{ch: k.script}, nil
}

// pausedSeat is the id of the seat a case calls name: a pause is filed by the
// seat's id, so a case that wants "the swe's pause" needs a uuid it can derive
// twice and read back the same.
func pausedSeat(name string) uuid.UUID {
	return uuid.NewSHA1(uuid.MustParse("0b6f3d2a-8e41-4c7a-9f5d-1a2b3c4d5e6f"), []byte(name))
}

// seatNames maps every id [pausedSeat] can mint in these cases back to its
// name, for a failure message a person can read.
var seatNames = func() map[uuid.UUID]string {
	out := map[uuid.UUID]string{}
	for _, name := range []string{"ops", "qa", "swe"} {
		out[pausedSeat(name)] = name
	}
	return out
}()

// inSeatOrder is names in the order the register lists their pauses: by
// seat id, since that is what each is keyed on.
func inSeatOrder(names ...string) []string {
	out := slices.Clone(names)
	slices.SortFunc(out, func(a, b string) int {
		return strings.Compare(pausedSeat(a).String(), pausedSeat(b).String())
	})
	return out
}

// pauseSeat pauses a free seat, failing the case if it was not free.
func pauseSeat(t *testing.T, f *FleetStore, name string) coord.SeatPause {
	t.Helper()
	p, created, err := f.CreateSeatPause(t.Context(), coord.SeatPause{
		Seat: pausedSeat(name), By: "jane-token", ByKind: iam.ActorOperator, At: time.Now(),
	})
	if err != nil || !created {
		t.Fatalf("CreateSeatPause(%s) = (%v, %v)", name, created, err)
	}
	return p
}

// firstPauseAnswer reads a pause watch through its Current marker and answers
// what came before it.
func firstPauseAnswer(ctx context.Context, t *testing.T, updates <-chan coord.SeatPauseUpdate) []coord.SeatPauseUpdate {
	t.Helper()
	var answer []coord.SeatPauseUpdate
	for {
		u := nextPauseUpdate(ctx, t, updates)
		if u.Current {
			return answer
		}
		answer = append(answer, u)
	}
}

// nextPauseUpdate reads one update, failing the case when the watch closes or
// ctx ends first.
func nextPauseUpdate(ctx context.Context, t *testing.T, updates <-chan coord.SeatPauseUpdate) coord.SeatPauseUpdate {
	t.Helper()
	select {
	case u, ok := <-updates:
		if !ok {
			t.Fatal("the pause watch closed while the store was healthy")
		}
		return u
	case <-ctx.Done():
		t.Fatalf("the pause watch delivered nothing before its caller gave up: %v", ctx.Err())
	}
	return coord.SeatPauseUpdate{}
}

// namesOf names the paused seats in an answer, in the order it gave them. An
// update that paused nothing is named as one, so it fails any comparison: a
// first answer holds the pauses that exist and nothing else.
func namesOf(answer []coord.SeatPauseUpdate) []string {
	names := make([]string, 0, len(answer))
	for _, u := range answer {
		name, known := seatNames[u.Seat]
		if !known {
			name = u.Seat.String()
		}
		if u.Pause == nil {
			name += " (no pause)"
		}
		names = append(names, name)
	}
	return names
}
