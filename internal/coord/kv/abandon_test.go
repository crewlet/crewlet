package kv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/coordtest"
)

// faultyBucket fails the writes a case picks, optionally after letting them
// land — the shape of a write whose answer was lost on the way back.
type faultyBucket struct {
	jetstream.KeyValue
	fail func(ctx context.Context, value []byte) (fails, lands bool)
}

var errInjected = fmt.Errorf("injected: %w", nats.ErrTimeout)

func (b *faultyBucket) Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error) {
	fails, lands := b.fail(ctx, value)
	if !fails {
		return b.KeyValue.Update(ctx, key, value, revision)
	}
	if lands {
		if _, err := b.KeyValue.Update(ctx, key, value, revision); err != nil {
			return 0, err
		}
	}
	return 0, errInjected
}

// A CLAIM THAT FAILS PART WAY GIVES ITS RECORD BACK, SO THE RESOURCE IS
// CLAIMABLE AT ONCE — BY ITS OWNER AND BY A PEER — AND FENCING STAYS EXACT.
//
// A claim is three writes: the record in the claiming state, the counter, and
// the token committed into the record. A failure after the first used to leave
// the record claiming until its TTL, which every peer reads as held and the
// owner's own retries read as a sibling one round trip from committing — so
// the owner spun through every compare-and-swap round and answered UNKNOWN for
// a whole TTL, and the resource was dark. Each case fails one write, some
// after letting it land, and then claims again at once: the claim must be
// granted, at an epoch above any token the failed call could have taken, and a
// tenure the failed call did commit must survive it.
func TestAClaimThatFailsPartWayGivesItsRecordBack(t *testing.T) {
	t.Parallel()
	// Which write a case fails, and whether it lands first.
	type write int
	const (
		theClaim  write = iota // the record, in the claiming state
		theEpoch               // the counter
		theCommit              // the token, into the record
	)
	cases := []struct {
		name   string
		fails  write
		lands  bool
		cancel bool
		// ownerEpoch is the epoch the failed claimant's own next claim is
		// granted; peerHeld says a peer is refused, because the failed
		// call's tenure did commit.
		ownerEpoch int64
		peerHeld   bool
	}{
		{name: "the counter cannot be advanced", fails: theEpoch, ownerEpoch: 1},
		// The counter moved and nobody was told: that token is a gap, and
		// the next tenure is fenced above it.
		{name: "the counter moved and its answer was lost", fails: theEpoch, lands: true, ownerEpoch: 2},
		// The caller gave up mid-claim. The give-back is a teardown, and
		// one run on the dead context would do nothing at all.
		{name: "the caller gave up during the epoch step", fails: theEpoch, cancel: true, ownerEpoch: 1},
		// The claiming record landed and the claim was never told: only
		// the call's own claim can say the record is its.
		{name: "the claiming record landed unannounced", fails: theClaim, lands: true, ownerEpoch: 1},
		{name: "the commit never landed", fails: theCommit, ownerEpoch: 2},
		// The commit landed and its answer was lost: that is a TENURE,
		// and giving it back would hand a peer a resource its owner may
		// already be acting on. The owner's next claim renews it.
		{name: "the commit landed unannounced", fails: theCommit, lands: true, ownerEpoch: 1, peerHeld: true},
	}
	for _, tc := range cases {
		for _, next := range []string{"the owner", "a peer"} {
			t.Run(tc.name+"/then "+next+" claims", func(t *testing.T) {
				t.Parallel()
				s := openStore(t, embeddedNATS(t), coordtest.LongTTL)
				t.Cleanup(s.Close)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()

				// Exactly one write fails, the first of the kind the case
				// names; the give-back that follows is left alone.
				armed := true
				trip := func(w write) (fails, lands bool) {
					if !armed || w != tc.fails {
						return false, false
					}
					armed = false
					if tc.cancel {
						// The caller's context ends here, and the write
						// fails because it did.
						cancel()
					}
					return true, tc.lands
				}
				s.epochs = &faultyBucket{KeyValue: s.epochs,
					fail: func(context.Context, []byte) (bool, bool) { return trip(theEpoch) }}
				s.leases.kv = &faultyBucket{KeyValue: s.leases.kv,
					fail: func(_ context.Context, value []byte) (bool, bool) {
						var v leaseValue
						if err := json.Unmarshal(value, &v); err != nil {
							t.Errorf("a lease write that is not a lease record: %v", err)
							return false, false
						}
						switch {
						case v.Owner == "":
							// A tombstone: the give-back itself.
							return false, false
						case v.Epoch == claimingEpoch:
							return trip(theClaim)
						}
						return trip(theCommit)
					}}

				resource := coord.ClassSeat.Resource("ceo")
				const owner = "node-a/1"
				lease, _, err := s.TryAcquire(ctx, resource, coord.AcquireOptions{
					Owner: owner, TTL: coordtest.LongTTL, Ungated: true,
				})
				if err == nil || lease != nil {
					t.Fatalf("the failed claim answered (%v, %v), want an error", lease, err)
				}
				if armed {
					t.Fatal("the case's write was never attempted, so it tested nothing")
				}

				claimant := owner
				if next == "a peer" {
					claimant = "node-b/1"
				}
				lease, _, err = s.TryAcquire(t.Context(), resource, coord.AcquireOptions{
					Owner: claimant, TTL: coordtest.LongTTL, Ungated: true,
				})
				switch {
				case err != nil:
					t.Fatalf("%s's claim straight after answered %v — the failed claim's record "+
						"still holds the resource", next, err)
				case next == "a peer" && tc.peerHeld:
					if lease != nil {
						t.Fatalf("a peer took a tenure the failed call committed: %+v", *lease)
					}
				case lease == nil:
					t.Fatalf("%s's claim straight after was refused — the failed claim's "+
						"record still holds the resource", next)
				case next == "the owner" && lease.Epoch != tc.ownerEpoch:
					t.Fatalf("the owner's claim was granted at epoch %d, want %d", lease.Epoch,
						tc.ownerEpoch)
				case next == "a peer" && lease.Epoch != tc.ownerEpoch:
					t.Fatalf("the peer's claim was granted at epoch %d, want %d", lease.Epoch,
						tc.ownerEpoch)
				}
			})
		}
	}
}

// A GIVE-BACK NEVER TAKES A SIBLING'S CLAIM.
//
// One owner runs several claims of one resource at once, and a record
// claiming under that owner may be a sibling's, one round trip from
// committing. The failed call gives back only the record carrying its OWN
// claim: here a sibling's claiming record stands in the bucket when the failed
// call looks, and it must still be there afterwards.
func TestAGiveBackLeavesASiblingsClaim(t *testing.T) {
	t.Parallel()
	s := openStore(t, embeddedNATS(t), coordtest.LongTTL)
	t.Cleanup(s.Close)
	resource := coord.ClassSeat.Resource("ceo")
	sibling := leaseValue{
		Resource: resource, Owner: "node-a/1", Epoch: claimingEpoch,
		TTLNanos: int64(coordtest.LongTTL), Protocol: coord.ProtocolVersion,
		Layout: layoutDutyLane, Claim: "the-sibling",
	}
	data, err := encodeValue(sibling)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.create(t.Context(), s.leases.kv, encodeResource(resource), data); err != nil {
		t.Fatalf("write the sibling's claiming record: %v", err)
	}
	mine := sibling
	mine.Claim = "this-call"
	s.abandon(t.Context(), s.leases, resource, mine, nil, errors.New("the epoch step failed"))

	e, err := s.readOne(t.Context(), s.leases, resource)
	if err != nil {
		t.Fatal(err)
	}
	if e == nil || e.value.Owner != sibling.Owner || e.value.Claim != sibling.Claim {
		t.Fatalf("a give-back of this call's claim took the sibling's record: now %+v", e)
	}
}

// latePublish holds back the claiming write and answers its caller as a
// cancelled request is answered — with an error, before the server has seen
// it — so the case can deliver it AFTER the claim gave up.
type latePublish struct {
	jetstream.KeyValue
	mu       sync.Mutex
	key      string
	value    []byte
	revision uint64
	held     bool
}

func (b *latePublish) Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error) {
	var v leaseValue
	if err := json.Unmarshal(value, &v); err == nil && v.Epoch == claimingEpoch && v.Owner != "" {
		b.mu.Lock()
		defer b.mu.Unlock()
		if !b.held {
			b.key, b.value, b.revision, b.held = key, value, revision, true
			return 0, fmt.Errorf("injected: %w", context.Canceled)
		}
	}
	return b.KeyValue.Update(ctx, key, value, revision)
}

// A CLAIMING WRITE THAT LANDS AFTER ITS CLAIM GAVE UP IS REFUSED.
//
// A caller that gives up mid-claim stops waiting for a request the server has
// not yet processed, and the request lands after the give-back looked for it:
// a record carrying the claim that nothing would ever take back, held against
// its own owner — told unknown — for the record's whole TTL. So the give-back
// FENCES the key against that write where it has not landed, and the late
// write must be refused by the expectation it carries; the owner's next claim
// is granted. Both shapes of the claiming write: a create on a key with no
// record, and a takeover of a released one.
func TestAClaimingWriteThatLandsAfterItsClaimGaveUpIsRefused(t *testing.T) {
	t.Parallel()
	for _, takeover := range []bool{false, true} {
		name := "a create"
		if takeover {
			name = "a takeover"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := openStore(t, embeddedNATS(t), coordtest.LongTTL)
			t.Cleanup(s.Close)
			resource := coord.ClassSeat.Resource("ceo")
			const owner = "node-a/1"
			opts := coord.AcquireOptions{Owner: owner, TTL: coordtest.LongTTL, Ungated: true}
			if takeover {
				first, _, err := s.TryAcquire(t.Context(), resource, opts)
				if err != nil || first == nil {
					t.Fatalf("the first tenure: (%v, %v)", first, err)
				}
				if ok, err := s.Release(t.Context(), resource, owner, first.Epoch); err != nil || !ok {
					t.Fatalf("release it: (%v, %v)", ok, err)
				}
			}
			late := &latePublish{KeyValue: s.leases.kv}
			s.leases.kv = late

			if lease, _, err := s.TryAcquire(t.Context(), resource, opts); err == nil || lease != nil {
				t.Fatalf("the claim whose write was held back answered (%v, %v), want an error",
					lease, err)
			}
			late.mu.Lock()
			held := late.held
			late.mu.Unlock()
			if !held {
				t.Fatal("no claiming write was held back, so this case tested nothing")
			}
			// THE REQUEST REACHES THE SERVER NOW, after the claim gave up.
			if _, err := late.KeyValue.Update(t.Context(), late.key, late.value, late.revision); err == nil {
				t.Fatal("a claiming write that landed after its claim gave up was accepted — " +
					"a record carrying the claim that nothing will ever take back")
			}
			lease, _, err := s.TryAcquire(t.Context(), resource, opts)
			if err != nil || lease == nil {
				t.Fatalf("the owner's next claim answered (%v, %v), want it granted", lease, err)
			}
		})
	}
}
