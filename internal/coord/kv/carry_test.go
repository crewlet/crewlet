package kv

import (
	"context"
	"encoding/json"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/jsoncarry/jsoncarrytest"
)

// rowTypes is every row type this package declares and writes to a bucket.
var rowTypes = map[string]reflect.Type{
	"rate":       reflect.TypeFor[rateRecord](),
	"ledger":     reflect.TypeFor[ledgerRecord](),
	"budget":     reflect.TypeFor[budgetRecord](),
	"activation": reflect.TypeFor[activationRecord](),
	"payload":    reflect.TypeFor[payloadRecord](),
	"apply":      reflect.TypeFor[applyRecord](),
	"secret":     reflect.TypeFor[secretRecord](),
	"channel":    reflect.TypeFor[channelRecord](),
	"fire":       reflect.TypeFor[fireRecord](),
	"follow":     reflect.TypeFor[followRecord](),
	"mailbox":    reflect.TypeFor[mailboxRecord](),
	"lease":      reflect.TypeFor[leaseValue](),
	"resource":   reflect.TypeFor[resourceValue](),
}

// rowGolden is the bytes each row encodes to, filled so that every member it
// has is on the wire.
var rowGolden = map[string]string{
	"rate":       `{"count":7}`,
	"ledger":     `{"detail":"Detail","at":"2026-01-02T03:04:05Z"}`,
	"budget":     `{"used":7,"at":"2026-01-02T03:04:05Z","refused_at":"2026-01-02T03:04:05Z"}`,
	"activation": `{"revision_id":"RevisionID","at":"2026-01-02T03:04:05Z","summary":"Summary"}`,
	"payload":    `{"revision_id":"RevisionID","payload":"Bw=="}`,
	"apply":      `{"epoch":7,"revision_id":"RevisionID","status":"Status","error":"Error","updated_at":"2026-01-02T03:04:05Z"}`,
	"secret":     `{"name":"Name","value":"Value","key_id":"KeyID","updated_at":"2026-01-02T03:04:05Z","updated_by":"UpdatedBy","source":"Source"}`,
	"channel":    `{"requester":"Requester","target":"Target","messages":7,"opened_at":"2026-01-02T03:04:05Z","last_at":"2026-01-02T03:04:05Z","closed_at":"2026-01-02T03:04:05Z"}`,
	"fire":       `{"at":"2026-01-02T03:04:05Z"}`,
	"follow":     `{"reason":"Reason","at":"2026-01-02T03:04:05Z"}`,
	"mailbox":    `{"absent_since":"2026-01-02T03:04:05Z","retiring_since":"2026-01-02T03:04:05Z"}`,
	"lease":      `{"resource":"Resource","owner":"Owner","epoch":7,"ttl_ns":7,"preferred":"Preferred","protocol":7,"meta":{"key":null},"layout":7}`,
	"resource":   `{"resource":"Resource","epoch":7,"preferred":"Preferred"}`,
}

// A ROW THIS BUILD WRITES IS THE BYTES IT ALWAYS WAS.
//
// A row is one value in a bucket every build in the fleet reads, so its bytes
// are a contract between peers. A row carrying nothing this build does not
// know encodes exactly as its struct does, and decoding those bytes and
// encoding them again changes nothing.
func TestEveryRowEncodesAsItAlwaysHas(t *testing.T) {
	t.Parallel()
	for name, typ := range rowTypes {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			want, ok := rowGolden[name]
			if !ok {
				t.Fatalf("%s has no pinned encoding", name)
			}
			filled := jsoncarrytest.FilledOf(typ)
			got, err := json.Marshal(filled.Interface())
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if string(got) != want {
				t.Fatalf("%s encodes as\n  %s\nand every node holds\n  %s", name, got, want)
			}
			back := reflect.New(typ)
			if err := json.Unmarshal(got, back.Interface()); err != nil {
				t.Fatalf("decode: %v", err)
			}
			again, err := json.Marshal(back.Interface())
			if err != nil {
				t.Fatalf("encode again: %v", err)
			}
			if string(again) != want {
				t.Fatalf("%s decoded and encoded again is\n  %s\nnot\n  %s", name, again, want)
			}
		})
	}
}

// EVERY ROW THIS PACKAGE DECLARES CARRIES WHAT IT DOES NOT KNOW, and so does
// every object inside one.
//
// Mutation: drop any one of carry.go's UnmarshalJSON methods and that row is
// named here.
func TestEveryRowCarriesWhatItDoesNotKnow(t *testing.T) {
	t.Parallel()
	roots := make([]reflect.Type, 0, len(rowTypes))
	for _, typ := range rowTypes {
		roots = append(roots, typ)
	}
	for _, missing := range jsoncarrytest.Uncarried(nil, roots...) {
		t.Error(missing)
	}
}

// A MEMBER A NEWER BUILD ADDED TO ANY ROW SURVIVES THIS BUILD'S DECODE AND
// ENCODE of it, byte for byte. The writes that decode a row and write it back
// are walked one by one in
// TestAMemberANewerBuildAddedSurvivesEveryWriteThatChangesARow.
func TestAMemberANewerBuildAddedSurvivesEveryRow(t *testing.T) {
	t.Parallel()
	for name, typ := range rowTypes {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			jsoncarrytest.Survives(t, typ, []byte(rowGolden[name]), func(raw []byte) ([]byte, error) {
				back := reflect.New(typ)
				if err := json.Unmarshal(raw, back.Interface()); err != nil {
					return nil, err
				}
				return json.Marshal(back.Interface())
			})
		})
	}
}

// plant writes a newer build's member into the row at key, beside every member
// the row already holds.
func plant(ctx context.Context, t *testing.T, kv jetstream.KeyValue, key string) {
	t.Helper()
	entry, err := kv.Get(ctx, key)
	if err != nil {
		t.Fatalf("read the row at %s: %v", key, err)
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(entry.Value(), &members); err != nil {
		t.Fatalf("decode the row at %s: %v", key, err)
	}
	members[jsoncarrytest.PlantedName] = json.RawMessage(jsoncarrytest.PlantedValue)
	raw, err := json.Marshal(members)
	if err != nil {
		t.Fatalf("encode the planted row: %v", err)
	}
	if _, err := kv.Put(ctx, key, raw); err != nil {
		t.Fatalf("write the planted row at %s: %v", key, err)
	}
}

// planted is the newer build's member in the row at key, or nil.
func planted(ctx context.Context, t *testing.T, kv jetstream.KeyValue, key string) json.RawMessage {
	t.Helper()
	entry, err := kv.Get(ctx, key)
	if err != nil {
		t.Fatalf("read the row at %s: %v", key, err)
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(entry.Value(), &members); err != nil {
		t.Fatalf("decode the row at %s: %v", key, err)
	}
	return members[jsoncarrytest.PlantedName]
}

// A MEMBER A NEWER BUILD ADDED TO A ROW SURVIVES EVERY WRITE THAT CHANGES THE
// ROW — an increment, a charge, a refusal stamped and cleared, a message
// counted and a channel closed, a lease renewed, re-claimed and released, and
// a resource's epoch moved and its hint pinned. Each is a compare-and-set by
// whichever build gets there, and a build that re-encoded only what it knows
// would erase the member from the fleet with its first write.
//
// Mutation: build any one of these writes' rows anew rather than from the
// decoded row, and its case names the write.
func TestAMemberANewerBuildAddedSurvivesEveryWriteThatChangesARow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	nc := embeddedNATS(t)
	f := openFleet(t, nc)
	s := openStore(t, nc, time.Minute)
	survives := func(t *testing.T, kv jetstream.KeyValue, key, write string) {
		t.Helper()
		if got := planted(ctx, t, kv, key); string(got) != jsoncarrytest.PlantedValue {
			t.Errorf("%s left the row at %s with %s = %s, want %s — the member a newer "+
				"build wrote is lost to this build's write", write, key,
				jsoncarrytest.PlantedName, got, jsoncarrytest.PlantedValue)
		}
	}

	t.Run("rate window", func(t *testing.T) {
		now := time.Now()
		if ok, err := f.Allow(ctx, "carry", 10, time.Minute, now); err != nil || !ok {
			t.Fatalf("open the window: %v, %v", ok, err)
		}
		key := encodeKey("carry|" + strconv.FormatInt(now.Truncate(time.Minute).UnixNano(), 10))
		plant(ctx, t, f.rate, key)
		if ok, err := f.Allow(ctx, "carry", 10, time.Minute, now); err != nil || !ok {
			t.Fatalf("increment the window: %v, %v", ok, err)
		}
		survives(t, f.rate, key, "an increment")
	})

	t.Run("budget", func(t *testing.T) {
		const seat = "seat-carry"
		if spend, err := f.Charge(ctx, seat, 5, 0, 0); err != nil || !spend.OK {
			t.Fatalf("open the counters: %+v, %v", spend, err)
		}
		org, agent := encodeKey(coord.OrgScope), encodeKey(seat)
		plant(ctx, t, f.budgets, org)
		plant(ctx, t, f.budgets, agent)
		if spend, err := f.Charge(ctx, seat, 5, 0, 0); err != nil || !spend.OK {
			t.Fatalf("charge: %+v, %v", spend, err)
		}
		survives(t, f.budgets, org, "a charge")
		survives(t, f.budgets, agent, "a charge")
		// A charge past the seat's cap stamps a refusal on it; the next
		// one that fits clears it.
		if spend, err := f.Charge(ctx, seat, 5, 0, 12); err != nil || spend.OK {
			t.Fatalf("a charge past the seat's cap: %+v, %v", spend, err)
		}
		survives(t, f.budgets, agent, "a refusal's stamp")
		if spend, err := f.Charge(ctx, seat, 1, 0, 12); err != nil || !spend.OK {
			t.Fatalf("a charge that fits: %+v, %v", spend, err)
		}
		if usage, err := f.Usage(ctx); err != nil {
			t.Fatalf("read the counters: %v", err)
		} else {
			for _, u := range usage {
				if u.Scope == seat && !u.RefusedAt.IsZero() {
					t.Fatalf("the refusal was not cleared, so the clear was not exercised: %+v", u)
				}
			}
		}
		survives(t, f.budgets, agent, "a refusal's clear")
	})

	t.Run("channel", func(t *testing.T) {
		at := time.Now().UTC()
		if err := f.OpenChannel(ctx, coord.Channel{
			ID: "a2a-carry", Requester: "ana", Target: "bo", OpenedAt: at, LastAt: at,
		}); err != nil {
			t.Fatalf("open: %v", err)
		}
		key := encodeKey("a2a-carry")
		plant(ctx, t, f.channels, key)
		if _, ok, err := f.CountChannelMessage(ctx, "a2a-carry", at); err != nil || !ok {
			t.Fatalf("count a message: %v, %v", ok, err)
		}
		survives(t, f.channels, key, "a message counted")
		if _, ok, err := f.CloseChannel(ctx, "a2a-carry", at); err != nil || !ok {
			t.Fatalf("close: %v, %v", ok, err)
		}
		survives(t, f.channels, key, "a close")
	})

	t.Run("lease", func(t *testing.T) {
		resource := coord.SeatResource("carry")
		key := encodeResource(resource)
		lease, err := s.TryAcquire(ctx, resource, coord.AcquireOptions{
			Owner: "n1:a", TTL: time.Minute, Ungated: true,
		})
		if err != nil || lease == nil {
			t.Fatalf("claim: %v, %v", lease, err)
		}
		plant(ctx, t, s.leases.kv, key)
		if ok, err := s.Renew(ctx, resource, "n1:a", lease.Epoch, time.Minute); err != nil || !ok {
			t.Fatalf("renew: %v, %v", ok, err)
		}
		survives(t, s.leases.kv, key, "a renewal")
		if again, err := s.TryAcquire(ctx, resource, coord.AcquireOptions{
			Owner: "n1:a", TTL: time.Minute, Ungated: true, Preferred: "n1",
		}); err != nil || again == nil || again.Epoch != lease.Epoch {
			t.Fatalf("re-claim by the holder: %v, %v", again, err)
		}
		survives(t, s.leases.kv, key, "a re-claim by the holder")
		if ok, err := s.Release(ctx, resource, "n1:a", lease.Epoch); err != nil || !ok {
			t.Fatalf("release: %v, %v", ok, err)
		}
		survives(t, s.leases.kv, key, "a release")
	})

	t.Run("resource", func(t *testing.T) {
		resource := coord.SeatResource("carry-epoch")
		key := encodeResource(resource)
		first, err := s.TryAcquire(ctx, resource, coord.AcquireOptions{
			Owner: "n1:a", TTL: time.Minute, Ungated: true, Preferred: "n1",
		})
		if err != nil || first == nil {
			t.Fatalf("claim: %v, %v", first, err)
		}
		plant(ctx, t, s.epochs, key)
		if again, err := s.TryAcquire(ctx, resource, coord.AcquireOptions{
			Owner: "n1:a", TTL: time.Minute, Ungated: true, Preferred: "n2",
		}); err != nil || again == nil {
			t.Fatalf("re-place the holder: %v, %v", again, err)
		}
		survives(t, s.epochs, key, "a hint pinned")
		if ok, err := s.Release(ctx, resource, "n1:a", first.Epoch); err != nil || !ok {
			t.Fatalf("release: %v, %v", ok, err)
		}
		next, err := s.TryAcquire(ctx, resource, coord.AcquireOptions{
			Owner: "n2:b", TTL: time.Minute, Ungated: true,
		})
		if err != nil || next == nil || next.Epoch <= first.Epoch {
			t.Fatalf("a new tenure: %v, %v", next, err)
		}
		survives(t, s.epochs, key, "an epoch moved")
	})
}

// A NEW TENURE OF A LEASE CARRIES NOTHING THE PREVIOUS ONE DID. The record is
// built from the claim, because a member a previous holder's build wrote on
// its lease describes that holder — the way the layout does — and one carried
// onto the next holder's record would describe somebody it is not true of.
//
// Mutation: carry the lapsed record's members into a takeover and the member
// is found on the new holder's record.
func TestANewTenureCarriesNothingThePreviousOneDid(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, embeddedNATS(t), time.Minute)
	resource := coord.SeatResource("tenure")
	key := encodeResource(resource)
	first, err := s.TryAcquire(ctx, resource, coord.AcquireOptions{
		Owner: "n1:a", TTL: time.Minute, Ungated: true,
	})
	if err != nil || first == nil {
		t.Fatalf("claim: %v, %v", first, err)
	}
	if ok, err := s.Release(ctx, resource, "n1:a", first.Epoch); err != nil || !ok {
		t.Fatalf("release: %v, %v", ok, err)
	}
	plant(ctx, t, s.leases.kv, key)
	next, err := s.TryAcquire(ctx, resource, coord.AcquireOptions{
		Owner: "n2:b", TTL: time.Minute, Ungated: true,
	})
	if err != nil || next == nil {
		t.Fatalf("take over: %v, %v", next, err)
	}
	if got := planted(ctx, t, s.leases.kv, key); got != nil {
		t.Errorf("the new holder's record carries the previous holder's %s = %s",
			jsoncarrytest.PlantedName, got)
	}
}
