package kv

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/coordtest"
	"github.com/crewlet/crewlet/internal/iam"
)

// controlTake is a take of launch by a person of their own.
func controlTake(launch uuid.UUID) coord.BrowserControlTake {
	return coord.BrowserControlTake{
		Launch: launch, Turn: uuid.New(), Seat: uuid.New(),
		Owner: coord.BrowserControlOwner{
			Person: uuid.New(), Credential: iam.SessionName(uuid.NewString()), Take: uuid.New(),
		},
		Holder: coord.BrowserControlHolder{Seat: uuid.New(), Name: "Ada Lovelace"},
	}
}

// A BROWSER CONTROL'S KEY AGES OUT, AND ITS TOKENS COME FROM ONE COUNTER, so
// nothing a launch leaves here accumulates: its key is in the duty bucket,
// whose age removes it, and its tokens are the class's one counter's in the
// ageless epochs bucket (coord.CounterResource). Three launches, one ended and
// two lapsed, leave one counter record, no lease key once the age passes, and
// a next first take above every epoch they held. The duty bucket is the case's
// own, aged a second, as TestALapsedTakerLeasesKeyIsRemoved's is.
//
// Mutation: count a launch on a counter of its own, and the epochs bucket holds
// a record per launch.
func TestABrowserControlsKeyAgesOutAndItsTokensComeFromOneCounter(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	js := jsOf(embeddedNATS(t))
	s := openStoreVia(t, js, InProcess, time.Minute)

	launches := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}
	status, err := s.laneFor(coord.BrowserControlResource(launches[0])).kv.Status(ctx)
	if err != nil {
		t.Fatalf("the browser control lane's bucket status: %v", err)
	}
	if status.TTL() != coord.MaxDutyTTL {
		t.Fatalf("a browser control is kept in a bucket aged %v, want the duty bucket's %v: a bucket with "+
			"no age keeps every launch's key for as long as the deployment runs", status.TTL(), coord.MaxDutyTTL)
	}

	const age = time.Second
	shortenDuties(ctx, t, js, s, age)
	var highest int64
	for i, launch := range launches {
		take := controlTake(launch)
		control, taken, err := s.TakeBrowserControl(ctx, take, age)
		if err != nil || taken != coord.ControlTaken {
			t.Fatalf("take launch %d's browser = (%q, %v)", i, taken, err)
		}
		highest = max(highest, control.Current.Epoch)
		if i == 0 {
			if _, ended, err := s.EndBrowserControl(ctx, launch, take.Owner, control.Current.Epoch,
				coord.ControlReleased); err != nil || !ended {
				t.Fatalf("end launch 0's control = (%v, %v)", ended, err)
			}
		}
	}

	counters, err := keysUnder(ctx, js, s.counter.kv, jetstream.AllKeys)
	if err != nil {
		t.Fatalf("list the epochs bucket: %v", err)
	}
	if want := []string{encodeResource(string(coord.ClassBrowserControl))}; !slices.Equal(counters, want) {
		t.Fatalf("the epochs bucket holds %v after three launches were taken, want the class's one counter %v",
			counters, want)
	}

	// THE AGE REMOVES EVERY KEY: polled, since the broker reaps on a timer of
	// its own, and bounded far past the age.
	deadline := time.Now().Add(10 * age)
	for {
		left, err := keysUnder(ctx, js, s.duties.kv, jetstream.AllKeys)
		if err != nil {
			t.Fatalf("list the duty bucket: %v", err)
		}
		if len(left) == 0 {
			break
		}
		if time.Now().After(deadline) { //fixedwait:measure a poll's own deadline: it only ends the wait for the bucket to empty, which decides
			t.Fatalf("browser control keys %v outlived the duty bucket's %v age by nine times over", left, age)
		}
		time.Sleep(age / 10) //fixedwait:liveness
	}

	next, taken, err := s.TakeBrowserControl(ctx, controlTake(uuid.New()), age)
	if err != nil || taken != coord.ControlTaken {
		t.Fatalf("the next launch's take = (%q, %v)", taken, err)
	}
	if next.Current.Epoch <= highest {
		t.Fatalf("the next launch's first take holds %d once every key was gone, at or below the %d an "+
			"earlier launch held: the class's counter went with the keys", next.Current.Epoch, highest)
	}
}

// A GIVE-BACK KEEPS HOW THE TENURE IT COVERS ENDED. A take that fails part way
// gives its record back, and where its own write never landed the give-back
// writes a tombstone over the record the take was made over, clearing its
// owner and moving its stamp. Over a lapsed tenure that record would then read
// as released, and over an ended one at the give-back's instant, so the take's
// mark writes the end the record held, as the store judged it, into it.
//
// Mutation: drop the mark from giveBack, and the lapse reads as a release.
func TestAGiveBackKeepsHowTheTenureItCoversEnded(t *testing.T) {
	t.Parallel()
	for _, lapsed := range []bool{true, false} {
		name := "an ended tenure"
		if lapsed {
			name = "a lapsed tenure"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			s := openStore(t, embeddedNATS(t), coordtest.LongTTL)
			take := controlTake(uuid.New())
			resource := coord.BrowserControlResource(take.Launch)
			meta, err := coord.BrowserControlTakeMeta(take, nil, coord.BrowserControlJudged{})
			if err != nil {
				t.Fatal(err)
			}
			// THE TENURE THE CLAIM IS MADE OVER, at epoch 7: its owner set and
			// its TTL a nanosecond, lapsed by the store's clock as soon as it is
			// read; or ended by a hand-back, its owner cleared.
			value := leaseValue{
				Resource: resource, Owner: take.Owner.Key(), Epoch: 7, TTLNanos: 1,
				Protocol: coord.ProtocolVersion, AcquiredAt: time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC),
				Meta: meta,
			}
			if !lapsed {
				value.Owner, value.TTLNanos = "", int64(s.duties.maxTTL)
				if value.Meta, err = coord.BrowserControlEndMeta(meta, coord.ControlHandedBack); err != nil {
					t.Fatal(err)
				}
			}
			data, err := encodeValue(value)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.create(ctx, s.duties.kv, encodeResource(resource), data); err != nil {
				t.Fatalf("write the tenure the claim is made over: %v", err)
			}
			over, err := s.readOne(ctx, s.duties, resource)
			if err != nil || over == nil {
				t.Fatalf("read it back: (%v, %v)", over, err)
			}
			before := endOf(t, s, take.Launch, value.Epoch)

			// THE CLAIM'S OWN WRITE NEVER LANDED: its give-back fences the
			// record it was made over.
			claiming := leaseValue{
				Resource: resource, Owner: controlTake(take.Launch).Owner.Key(), Epoch: claimingEpoch,
				TTLNanos: int64(coord.BrowserControlTTL), Protocol: coord.ProtocolVersion, Claim: "the-failed-take",
			}
			gaveBack, err := s.giveBack(ctx, s.duties, resource, claiming, over, keepEnd)
			if err != nil || !gaveBack {
				t.Fatalf("give the failed take's record back = (%v, %v), want it written", gaveBack, err)
			}
			if e, err := s.readOne(ctx, s.duties, resource); err != nil || e == nil || e.revision == over.revision ||
				e.value.Owner != "" {
				t.Fatalf("the give-back left (%+v, %v), want a tombstone over the record", e, err)
			}
			after := endOf(t, s, take.Launch, value.Epoch)
			if after.EndedBy != before.EndedBy || !after.EndedAt.Equal(before.EndedAt) {
				t.Fatalf("epoch %d ended as %s at %v before the give-back and reads %s at %v after it",
					value.Epoch, before.EndedBy, before.EndedAt, after.EndedBy, after.EndedAt)
			}
		})
	}
}

// A REFUSED TAKE IS HELD ONLY BY ANOTHER OWNER'S LIVE TENURE. A take refused
// over a record answers held naming the tenure that record holds, read from it
// with no second read; every other refusing record, a shape only the claim's
// read-back meets, holds nobody a held answer could name, so it is unknown and
// the panel asks again: another owner's claim still minting, its tenure that
// lapsed or ended inside the round trip, the taker's own, or no record at all.
//
// Mutation: answer held whatever the refusing record holds, and the ended
// tenures are named holders.
func TestATakeIsHeldOnlyByAnotherOwnersLiveTenure(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := openStore(t, embeddedNATS(t), coordtest.LongTTL)
	at := time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		record func(take, other coord.BrowserControlTake) leaseValue
		held   bool
	}{
		{"another owner's live tenure", func(_, other coord.BrowserControlTake) leaseValue {
			return tenureOf(t, other, other.Owner.Key(), 3, time.Hour, at)
		}, true},
		{"another owner's claim still minting", func(_, other coord.BrowserControlTake) leaseValue {
			v := tenureOf(t, other, other.Owner.Key(), claimingEpoch, time.Hour, time.Time{})
			v.Claim = "the-minting-claim"
			return v
		}, false},
		{"another owner's lapsed tenure", func(_, other coord.BrowserControlTake) leaseValue {
			return tenureOf(t, other, other.Owner.Key(), 3, time.Nanosecond, at)
		}, false},
		{"another owner's ended tenure", func(_, other coord.BrowserControlTake) leaseValue {
			return tenureOf(t, other, "", 3, coord.MaxDutyTTL, at)
		}, false},
		{"the taker's own tenure", func(take, _ coord.BrowserControlTake) leaseValue {
			return tenureOf(t, take, take.Owner.Key(), 3, time.Hour, at)
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			take := controlTake(uuid.New())
			resource := coord.BrowserControlResource(take.Launch)
			data, err := encodeValue(tc.record(take, controlTake(take.Launch)))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.create(ctx, s.duties.kv, encodeResource(resource), data); err != nil {
				t.Fatalf("write the refusing record: %v", err)
			}
			snap, err := s.readForClaim(ctx, s.duties, resource)
			if err != nil {
				t.Fatal(err)
			}
			requireHeldAgainst(ctx, t, s, take, snap, tc.held)
		})
	}
	t.Run("no record", func(t *testing.T) {
		requireHeldAgainst(ctx, t, s, controlTake(uuid.New()), snapshot{clock: s.newClock()}, false)
	})
}

// requireHeldAgainst fails unless a take refused over snap is answered held by
// the record's owner where held, and unknown otherwise.
func requireHeldAgainst(ctx context.Context, t *testing.T, s *Store, take coord.BrowserControlTake,
	snap snapshot, held bool) {

	t.Helper()
	control, taken, err := s.heldAgainst(ctx, take, snap)
	if !held {
		if taken != "" || !errors.Is(err, coord.ErrUnavailable) {
			t.Fatalf("answered (%+v, %q, %v), want only an error wrapping coord.ErrUnavailable", control.Current,
				taken, err)
		}
		return
	}
	tenure, ok := control.Held()
	if err != nil || taken != coord.ControlHeld || !ok || tenure.Owner.Key() != snap.mine.value.Owner {
		t.Fatalf("answered (%+v, %q, %v), want held by %s", control.Current, taken, err, snap.mine.value.Owner)
	}
}

// tenureOf is a browser control's record of take at epoch, owned by owner (a
// tombstone where it is empty), its TTL ttl and its tenure begun at at.
func tenureOf(t *testing.T, take coord.BrowserControlTake, owner string, epoch int64, ttl time.Duration,
	at time.Time) leaseValue {

	t.Helper()
	meta, err := coord.BrowserControlTakeMeta(take, nil, coord.BrowserControlJudged{})
	if err != nil {
		t.Fatal(err)
	}
	if owner == "" {
		if meta, err = coord.BrowserControlEndMeta(meta, coord.ControlReleased); err != nil {
			t.Fatal(err)
		}
	}
	return leaseValue{
		Resource: coord.BrowserControlResource(take.Launch), Owner: owner, Epoch: epoch, TTLNanos: int64(ttl),
		Protocol: coord.ProtocolVersion, AcquiredAt: at, Meta: meta,
	}
}

// endOf is how epoch of launch's control ended, as the store reads it.
func endOf(t *testing.T, s *Store, launch uuid.UUID, epoch int64) coord.BrowserControlTenure {
	t.Helper()
	control, found, err := s.BrowserControl(t.Context(), launch)
	if err != nil || !found {
		t.Fatalf("read launch %s's control: (%v, %v)", launch, found, err)
	}
	tenure, ok := control.Of(epoch)
	if !ok || !tenure.Ended() {
		t.Fatalf("epoch %d of launch %s reads as %+v, want ended", epoch, launch, tenure)
	}
	return tenure
}
