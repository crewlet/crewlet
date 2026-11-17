package kv

import (
	"testing"
	"time"
)

// knownLanes are the two kinds of lane a record is known on or not: the seat
// lease bucket, whose age reaps a record taken at it, and the duty bucket,
// whose every record is judged by its own deadline.
func knownLanes() (seats, duties *lane) {
	return &lane{maxTTL: 45 * time.Second, reapsAtMax: true},
		&lane{maxTTL: 3 * time.Hour, reapsAtMax: false}
}

// steppedClock is a clock the case moves.
type steppedClock struct{ at time.Time }

func (c *steppedClock) now() time.Time { return c.at }

func heldValue(owner string, epoch int64, ttl time.Duration) leaseValue {
	return leaseValue{Resource: "seat:x", Owner: owner, Epoch: epoch, TTLNanos: int64(ttl)}
}

// ONLY A RECORD THAT CANNOT LAPSE WHILE IT CAN BE READ IS KNOWN: one on the
// seat lease bucket taken at its whole age. A seat lease shorter than the age,
// and every duty, is judged by its deadline, and a write at its revision would
// renew it after it lapsed. Such a write also forgets what was known before.
//
// Mutation: know every record (both refusals fail).
func TestOnlyARecordThatCannotLapseWhileItCanBeReadIsKnown(t *testing.T) {
	t.Parallel()
	seats, duties := knownLanes()
	for _, tc := range []struct {
		name  string
		lane  *lane
		value leaseValue
		known bool
	}{
		{"a seat lease at its bucket's age", seats, heldValue("a", 3, 45*time.Second), true},
		{"a seat lease shorter than its bucket's age", seats, heldValue("a", 3, 10*time.Second), false},
		{"a duty at the contract's ceiling", duties, heldValue("a", 3, 3*time.Hour), false},
		{"a duty", duties, heldValue("a", 3, time.Minute), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var k knownRecords
			// What was known before, at the bucket's age on either lane.
			k.remember(tc.lane, "seat:x", 1, heldValue("a", 3, tc.lane.maxTTL))
			k.remember(tc.lane, "seat:x", 2, tc.value)
			got, ok := k.lookup(tc.lane, "seat:x", "a")
			switch {
			case ok != tc.known:
				t.Fatalf("known = %v, want %v", ok, tc.known)
			case ok && got.revision != 2:
				t.Fatalf("known at revision %d, want 2", got.revision)
			}
		})
	}
}

// A KNOWN RECORD IS THE NEWEST WRITE, NAMING ITS OWNER, AND NOT PAST ITS
// BUCKET'S AGE: two of a store's own calls on one resource may answer out of
// order, so an older answer never replaces a newer one; a lookup for another
// owner finds nothing; and an entry as old as the bucket's age, on this node's
// clock, is forgotten, since the broker has reaped the record unless something
// renewed it.
//
// Mutation: let an older answer replace a newer one; drop the owner check; or
// the age check.
func TestAKnownRecordIsTheNewestWriteOfItsOwnerWithinItsBucketsAge(t *testing.T) {
	t.Parallel()
	seats, _ := knownLanes()
	clk := &steppedClock{at: time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)}
	k := knownRecords{now: clk.now}
	k.remember(seats, "seat:x", 7, heldValue("a", 3, seats.maxTTL))
	k.remember(seats, "seat:x", 5, heldValue("a", 2, seats.maxTTL))
	if got, ok := k.lookup(seats, "seat:x", "a"); !ok || got.revision != 7 || got.value.Epoch != 3 {
		t.Fatalf("after an older answer: known = (%+v, %v), want revision 7 at epoch 3", got, ok)
	}
	if _, ok := k.lookup(seats, "seat:x", "b"); ok {
		t.Fatal("a lookup for another owner found the record")
	}
	clk.at = clk.at.Add(seats.maxTTL - time.Nanosecond)
	if _, ok := k.lookup(seats, "seat:x", "a"); !ok {
		t.Fatal("the record was forgotten inside its bucket's age")
	}
	clk.at = clk.at.Add(time.Nanosecond)
	if _, ok := k.lookup(seats, "seat:x", "a"); ok {
		t.Fatal("the record was still known at its bucket's age")
	}
	if _, kept := k.last["seat:x"]; kept {
		t.Fatal("an entry past its bucket's age was kept")
	}
}

// A KNOWN RECORD IS FORGOTTEN ONLY FOR ITS OWN REVISION OR ITS OWN TENURE: a
// refused or unanswered write at an older revision, or a release or a false
// renewal of another tenure, leaves the newer write a concurrent call made.
//
// Mutation: forget whatever the revision; or whatever the tenure.
func TestAKnownRecordIsForgottenOnlyForItsOwnRevisionOrTenure(t *testing.T) {
	t.Parallel()
	seats, _ := knownLanes()
	var k knownRecords
	k.remember(seats, "seat:x", 7, heldValue("a", 3, seats.maxTTL))

	k.forgetAt("seat:x", 6)
	k.forgetTenure("seat:x", "a", 2)
	k.forgetTenure("seat:x", "b", 3)
	if _, ok := k.lookup(seats, "seat:x", "a"); !ok {
		t.Fatal("a forget of another revision or tenure forgot the record")
	}
	k.forgetAt("seat:x", 7)
	if _, ok := k.lookup(seats, "seat:x", "a"); ok {
		t.Fatal("a forget at the known revision kept the record")
	}
	k.remember(seats, "seat:x", 8, heldValue("a", 3, seats.maxTTL))
	k.forgetTenure("seat:x", "a", 3)
	if _, ok := k.lookup(seats, "seat:x", "a"); ok {
		t.Fatal("a forget of the known tenure kept the record")
	}
}
