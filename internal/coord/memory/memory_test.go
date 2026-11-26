package memory_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/coordtest"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/iam"
)

// The twin is certified by the same suite as every other backend. A
// divergence between it and the store a fleet actually runs on is a failing
// test here rather than a production-only surprise on the one node that
// happens to run the other one.
func TestContract(t *testing.T) {
	coordtest.Run(t, func(t *testing.T) coord.Backend { return memory.New() })
}

// And through several handles on one store — the twin has one process, so the
// same store is every handle, which is the shape the shared cases hold a
// replicated backend to.
func TestSharedContract(t *testing.T) {
	coordtest.RunShared(t, func(t *testing.T) []coord.Backend {
		b := memory.New()
		return []coord.Backend{b, b, b}
	})
}

// A person's control of a run's browser, through three handles on one twin,
// the shape the suite's shared cases hold a replicated store to.
func TestBrowserControlContract(t *testing.T) {
	coordtest.RunBrowserControls(t, func(t *testing.T) []coordtest.BrowserControlStore {
		b := memory.New()
		return []coordtest.BrowserControlStore{b, b, b}
	})
}

// The zero value has to work: callers construct plain data without
// constructors, and a store that only functions when built by New would fail
// on a struct field somebody forgot to initialise.
func TestZeroValueIsUsable(t *testing.T) {
	t.Parallel()
	coordtest.Run(t, func(t *testing.T) coord.Backend { return new(memory.Backend) })
}

// Clock is what makes a caller's OWN test deterministic: it can put the
// store's clock wherever it needs a lease to lapse instead of waiting for one.
func TestInjectedClockDecidesExpiry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	b := &memory.Backend{Clock: func() time.Time { return now }}

	lease, _, err := b.TryAcquire(ctx, "seat:ceo", coord.AcquireOptions{
		Owner: "node-a:1", TTL: 30 * time.Second,
	})
	if err != nil || lease == nil {
		t.Fatalf("TryAcquire = (%v, %v)", lease, err)
	}
	if want := now.Add(30 * time.Second); !lease.ExpiresAt.Equal(want) {
		t.Fatalf("ExpiresAt = %v, want the store's clock plus the TTL (%v)", lease.ExpiresAt, want)
	}

	// Time does not pass on its own here — the store's clock is the only
	// clock, so the lease is still held however long the test takes.
	held, err := b.Get(ctx, "seat:ceo")
	if err != nil || held == nil {
		t.Fatalf("Get = (%v, %v), want the lease still held", held, err)
	}

	now = now.Add(31 * time.Second)
	if unheld, err := b.Get(ctx, "seat:ceo"); err != nil || unheld != nil {
		t.Fatalf("Get = (%v, %v) past the deadline, want (nil, nil)", unheld, err)
	}
}

// A dead context is the one way this store cannot answer. Callers branch on
// unknown-versus-lost, and that branch must be reachable in a memory-backed
// test or it is only ever exercised in production.
func TestDeadContextIsUnknownNotRefusal(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b := memory.New()
	take := controlTake(uuid.New(), "Ada Lovelace")

	// EVERY verb, because a dead context is a point in this store's life and
	// the suite's job is to send each verb at each such point. Three of the
	// eight used to be sent here, and a mutation dropping the guard from any
	// of the other five passed the whole package — the contract suite could
	// not cover it either, since its fault injector short-circuits ABOVE the
	// backend and so exercises the wrapper rather than this code.
	//
	// What each of them must not do is answer definitely: nil, false or an
	// empty slice with no error all read as facts about ownership, and a call
	// that never reached the store knows no facts about ownership.
	verbs := []struct {
		name string
		call func() error
	}{
		{"TryAcquire", func() error {
			lease, _, err := b.TryAcquire(ctx, "seat:ceo", coord.AcquireOptions{
				Owner: "node-a:1", TTL: time.Minute,
			})
			if lease != nil {
				t.Error("TryAcquire granted a lease on a cancelled context")
			}
			return err
		}},
		{"Renew", func() error {
			ok, err := b.Renew(ctx, "seat:ceo", "node-a:1", 1, time.Minute)
			if ok {
				t.Error("Renew reported success on a cancelled context")
			}
			return err
		}},
		{"Release", func() error {
			ok, err := b.Release(ctx, "seat:ceo", "node-a:1", 1)
			if ok {
				t.Error("Release reported success on a cancelled context")
			}
			return err
		}},
		{"Get", func() error { _, err := b.Get(ctx, "seat:ceo"); return err }},
		{"ListOwned", func() error { _, err := b.ListOwned(ctx, "node-a:1"); return err }},
		{"ListLive", func() error { _, err := b.ListLive(ctx, coord.ClassNode); return err }},
		{"Now", func() error { _, err := b.Now(ctx, coord.ClassNode); return err }},
		{"PreferredResources", func() error {
			_, err := b.PreferredResources(ctx, coord.ClassSeat, "node-a")
			return err
		}},
		{"EverPresent", func() error { _, err := b.EverPresent(ctx); return err }},
		{"FleetProtocolFloor", func() error {
			floor, any, err := b.FleetProtocolFloor(ctx)
			if any {
				t.Errorf("FleetProtocolFloor reported a floor of %d on a cancelled context", floor)
			}
			return err
		}},
		{"TakeBrowserControl", func() error {
			_, taken, err := b.TakeBrowserControl(ctx, take, time.Minute)
			if taken != "" {
				t.Errorf("TakeBrowserControl answered %q on a cancelled context", taken)
			}
			return err
		}},
		{"RenewBrowserControl", func() error {
			_, renewed, err := b.RenewBrowserControl(ctx, take.Launch, take.Owner, 1, time.Minute)
			if renewed {
				t.Error("RenewBrowserControl reported success on a cancelled context")
			}
			return err
		}},
		{"EndBrowserControl", func() error {
			_, ended, err := b.EndBrowserControl(ctx, take.Launch, take.Owner, 1, coord.ControlReleased)
			if ended {
				t.Error("EndBrowserControl reported success on a cancelled context")
			}
			return err
		}},
		{"BrowserControl", func() error {
			_, found, err := b.BrowserControl(ctx, take.Launch)
			if found {
				t.Error("BrowserControl found a record on a cancelled context")
			}
			return err
		}},
		{"WatchBrowserControls", func() error { _, err := b.WatchBrowserControls(ctx); return err }},
	}
	for _, v := range verbs {
		if err := v.call(); err == nil {
			t.Errorf("%s answered definitely on a cancelled context — an unreachable store "+
				"says nothing about ownership, and a caller reading that as fact sheds "+
				"seats it still holds", v.name)
		}
	}
}

// A meta payload that cannot be encoded is refused, and refused BEFORE the
// record is written.
//
// Twin-only behaviour the contract suite cannot reach: every meta payload in
// coordtest is JSON-native, so no case there hands the store something a codec
// rejects, and a silent drop here would pass both packages.
//
// Pinned on the OBSERVABLE, not the error text: the claim must not appear to
// succeed with its profile quietly missing, since peers would make placement
// decisions against roles the node never published.
func TestUnencodableMetaIsRefusedNotDropped(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := memory.New()
	resource := coord.NodeResource("n1")

	// Both failure branches, because the second is reachable: it decodes
	// bytes the encoder just produced, yet json.Number("1e1000") marshals and
	// will not decode into map[string]any (it overflows float64). Meta built
	// from previously decoded JSON carries json.Number values routinely.
	for _, bad := range []struct {
		name  string
		value any
	}{
		{"a value no encoder accepts", make(chan int)},
		{"a value that encodes but will not decode", json.Number("1e1000")},
	} {
		lease, _, err := b.TryAcquire(ctx, resource, coord.AcquireOptions{
			Owner: "n1:a", TTL: time.Minute, Ungated: true,
			Meta: map[string]any{"roles": []any{"seats"}, "bad": bad.value},
		})
		if err == nil {
			t.Fatalf("%s: TryAcquire = (%v, nil) — a real backend's write would fail on "+
				"this, and answering success with the profile silently gone is the one "+
				"outcome a caller cannot detect", bad.name, lease)
		}
		if lease != nil {
			t.Fatalf("%s: TryAcquire granted a lease it could not record the meta for", bad.name)
		}
		// And nothing was written on the way to refusing.
		held, err := b.Get(ctx, resource)
		if err != nil {
			t.Fatalf("%s: Get after a refused claim: %v", bad.name, err)
		}
		if held != nil {
			t.Fatalf("%s: the resource is held by %q after a claim that was refused",
				bad.name, held.Owner)
		}
	}
}

// The fleet-shared state is certified by the same suite as the KV, for the
// same reason: this twin IS the coordination layer for a single-node company,
// so a divergence between it and the store a fleet runs on would be a
// behaviour that only appears on the second node. Every node of a case is
// handed the one twin, because the twin is the store as well as the handle:
// it serves the nodes of one process.
func TestFleetContract(t *testing.T) {
	coordtest.RunFleet(t, func(*testing.T) func() coord.Fleet {
		f := memory.NewFleet()
		return func() coord.Fleet { return f }
	})
}

// The nodes' status records, over the twin's lease store and its fleet store,
// as a single-node company runs both: the twin refuses the same rows the KV
// does, and keeps none of a removed node's.
//
// Mutation: drop the twin's HoldValue, read a row at any epoch, remove a
// node's own row alone, write over a later epoch's row, or decode a row's
// numbers as float64, and a case goes red.
func TestNodeStatusRecordContract(t *testing.T) {
	coordtest.RunNodeStatusRecords(t, func(*testing.T) (coord.Backend, coord.NodeStatusRecords) {
		return memory.New(), memory.NewFleet()
	})
}

// The memory twin, held mid-mint through its epoch counter.
func TestMintingContract(t *testing.T) {
	coordtest.RunMinting(t, func(t *testing.T, hold coordtest.MintHold) coordtest.BrowserControlStore {
		return &memory.Backend{Epochs: coordtest.NewMintCounter(hold)}
	})
}

// controlTake is a take of launch by a person of their own, shown as name.
func controlTake(launch uuid.UUID, name string) coord.BrowserControlTake {
	return coord.BrowserControlTake{
		Launch: launch, Turn: uuid.New(), Seat: uuid.New(),
		Owner: coord.BrowserControlOwner{
			Person: uuid.New(), Credential: iam.SessionName(uuid.NewString()), Take: uuid.New(),
		},
		Holder: coord.BrowserControlHolder{Seat: uuid.New(), Name: name},
	}
}

// A BROWSER CONTROL LAPSES ON THE STORE'S CLOCK, NOT THE CALLER'S: live a
// nanosecond before its TTL is out by the twin's clock and lapsed at it, ended
// at exactly its deadline, however long the test itself takes.
//
// Mutation: judge the row with time.Now(), and it is still live at 30 s.
func TestABrowserControlLapsesOnTheStoresClockNotTheCallers(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	start := time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC)
	now := start
	b := &memory.Backend{Clock: func() time.Time { return now }}
	take := controlTake(uuid.New(), "Ada Lovelace")
	control, taken, err := b.TakeBrowserControl(ctx, take, coord.BrowserControlTTL)
	if err != nil || taken != coord.ControlTaken {
		t.Fatalf("TakeBrowserControl = (%q, %v)", taken, err)
	}
	epoch := control.Current.Epoch

	now = start.Add(coord.BrowserControlTTL - time.Nanosecond)
	read, _, err := b.BrowserControl(ctx, take.Launch)
	if held, ok := read.Held(); err != nil || !ok || held.Epoch != epoch {
		t.Fatalf("a nanosecond before its TTL by the store's clock the control reads (%+v, %v), want live",
			read.Current, err)
	}

	now = start.Add(coord.BrowserControlTTL)
	read, _, err = b.BrowserControl(ctx, take.Launch)
	switch lapsed, ok := read.Of(epoch); {
	case err != nil:
		t.Fatal(err)
	case !ok || lapsed.EndedBy != coord.ControlLapsed:
		t.Fatalf("at its deadline by the store's clock the control reads %+v, want lapsed", read.Current)
	case !lapsed.EndedAt.Equal(start.Add(coord.BrowserControlTTL)):
		t.Fatalf("the control lapsed at %v, want its deadline %v", lapsed.EndedAt, start.Add(coord.BrowserControlTTL))
	case !lapsed.TakenAt.Equal(start):
		t.Fatalf("the lapsed tenure began at %v, want %v", lapsed.TakenAt, start)
	}
}

// THE TWIN FORGETS AN ENDED BROWSER CONTROL AT THE DUTY BUCKET'S AGE PAST ITS
// END, on the next take of the class, and keeps every live one: on a single
// node it is the production lease store, and a row per launch anybody took
// would otherwise grow for the life of the process. An end is readable until
// then, as the KV duty bucket keeps a key for as long past its last write.
//
// Mutation: never remove a row, and the ended one is still read at its age.
func TestTheTwinForgetsAnEndedBrowserControlAtTheDutyBucketsAge(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	start := time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC)
	now := start
	b := &memory.Backend{Clock: func() time.Time { return now }}
	takeAt := func(take coord.BrowserControlTake) int64 {
		t.Helper()
		control, taken, err := b.TakeBrowserControl(ctx, take, coord.BrowserControlTTL)
		if err != nil || taken != coord.ControlTaken {
			t.Fatalf("TakeBrowserControl = (%q, %v)", taken, err)
		}
		return control.Current.Epoch
	}
	found := func(launch uuid.UUID) bool {
		t.Helper()
		_, found, err := b.BrowserControl(ctx, launch)
		if err != nil {
			t.Fatal(err)
		}
		return found
	}

	ended := controlTake(uuid.New(), "Ada Lovelace")
	epoch := takeAt(ended)
	if _, ok, err := b.EndBrowserControl(ctx, ended.Launch, ended.Owner, epoch, coord.ControlReleased); err != nil || !ok {
		t.Fatalf("EndBrowserControl = (%v, %v)", ok, err)
	}

	now = start.Add(coord.MaxDutyTTL - time.Nanosecond)
	live := controlTake(uuid.New(), "Grace Hopper")
	takeAt(live)
	if !found(ended.Launch) {
		t.Fatal("an ended control was forgotten a nanosecond before the duty bucket's age past its end")
	}

	now = start.Add(coord.MaxDutyTTL)
	takeAt(controlTake(uuid.New(), "Katherine Johnson"))
	if found(ended.Launch) {
		t.Fatal("an ended control is still read the duty bucket's age past its end")
	}
	if !found(live.Launch) {
		t.Fatal("a live control was forgotten")
	}
}
