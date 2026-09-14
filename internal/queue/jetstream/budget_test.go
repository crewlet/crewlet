package jetstream

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// fileQueue opens an embedded broker with a file store, which is the one a
// deployment runs and the one whose cap is taken from a volume.
func fileQueue(t *testing.T) *Queue {
	t.Helper()
	q, err := Open(t.Context(), Config{StoreDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := q.Stop(context.WithoutCancel(t.Context())); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})
	return q
}

// budgetLog is a domain stream reserving ceiling bytes.
func budgetLog(name string, ceiling int64) DomainStream {
	return DomainStream{
		Name:       name,
		Subjects:   []string{"crewlet.budget." + name + ".>"},
		MaxBytes:   ceiling,
		Duplicates: 2 * time.Minute,
	}
}

// AN EMBEDDED BROKER'S BUDGET IS ITS OWN CAP, AND RESERVATIONS SPEND IT.
//
// Its account states no limit at all, so a budget read from the account alone
// is "unlimited" on exactly the topology that refused a boot for want of room.
// And the broker compares a new ceiling against the ceilings it has granted,
// not against what they store: a stream reserving a gibibyte and holding
// nothing has spent a gibibyte.
func TestAnEmbeddedBrokersBudgetIsItsOwnCap(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		open func(t *testing.T) *Queue
		want BudgetSource
	}{
		"a file store":        {fileQueue, BudgetServerStore},
		"an in-memory broker": {newQueue, BudgetServerMemory},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			q := tc.open(t)
			before, err := q.StreamBudget(t.Context())
			if err != nil {
				t.Fatalf("StreamBudget: %v", err)
			}
			if before.Source != tc.want || before.Limit <= 0 {
				t.Fatalf("budget = %+v, want a stated limit from %s", before, tc.want)
			}

			const ceiling = int64(1) << 30
			if err := q.EnsureDomainStream(t.Context(), budgetLog("SPEND", ceiling)); err != nil {
				t.Fatalf("EnsureDomainStream: %v", err)
			}
			after, err := q.StreamBudget(t.Context())
			if err != nil {
				t.Fatalf("StreamBudget: %v", err)
			}
			if after.Committed-before.Committed != ceiling {
				t.Errorf("a %d-byte reservation moved the committed figure from %d to %d, "+
					"so the budget is counting what streams hold rather than what "+
					"they reserve", ceiling, before.Committed, after.Committed)
			}
		})
	}
}

// A RESERVATION THE BROKER REFUSES FOR WANT OF ROOM IS NAMED, and one it can
// grant is granted.
//
// The broker says `insufficient storage resources available` and nothing
// else, so a caller that has to say what was needed and what to change must
// be able to recognise the refusal without speaking this backend's codes.
func TestARefusedReservationIsNamed(t *testing.T) {
	t.Parallel()
	q := fileQueue(t)
	budget, err := q.StreamBudget(t.Context())
	if err != nil {
		t.Fatalf("StreamBudget: %v", err)
	}
	err = q.EnsureDomainStream(t.Context(), budgetLog("TOO_BIG", budget.Available()+1))
	if !errors.Is(err, ErrInsufficientStorage) {
		t.Fatalf("a ceiling one byte past what the broker had left returned %v, "+
			"want %v", err, ErrInsufficientStorage)
	}
	if err := q.EnsureDomainStream(t.Context(), budgetLog("FITS", budget.Available())); err != nil {
		t.Fatalf("a ceiling of exactly what was left was refused: %v", err)
	}
}

// A LONE BROKER'S GROWTH BUDGET IS EXACTLY THE ROOM IT HOLDS AN UPDATE TO.
//
// The capacity verb refuses a raise past it before a window opens, so a figure
// that understated the room would refuse raises the broker grants, and one that
// overstated it would let through the refusal the check exists to catch. Both
// directions are the broker's own answer here, one byte apart.
func TestAGrowthBudgetIsTheRoomAnUpdateIsHeldTo(t *testing.T) {
	t.Parallel()
	q := fileQueue(t)
	const ceiling = int64(1) << 30
	if err := q.EnsureDomainStream(t.Context(), budgetLog("GROW", ceiling)); err != nil {
		t.Fatalf("EnsureDomainStream: %v", err)
	}
	room, err := q.GrowthBudget(t.Context())
	if err != nil {
		t.Fatalf("GrowthBudget: %v", err)
	}
	if room.Source != BudgetServerStore || room.Available() <= 0 {
		t.Fatalf("a lone file-store broker's growth budget = %+v, want its own stated room", room)
	}
	grow, err := q.DomainLog(t.Context(), "GROW")
	if err != nil {
		t.Fatalf("DomainLog: %v", err)
	}
	err = grow.SetMaxBytes(t.Context(), uint64(ceiling+room.Available()+1))
	if !refusedStorage(err) {
		t.Fatalf("a raise one byte past the stated room returned %v, want the "+
			"broker's refusal: the budget understates what it grants", err)
	}
	if err := grow.SetMaxBytes(t.Context(), uint64(ceiling+room.Available())); err != nil {
		t.Fatalf("a raise of exactly the stated room was refused, so the budget "+
			"overstates it: %v", err)
	}
}

// WHETHER A DOMAIN STREAM EXISTS IS THREE ANSWERS, and the ceiling it holds is
// the one it was created with.
func TestADomainStreamsCeilingIsReadBack(t *testing.T) {
	t.Parallel()
	q := fileQueue(t)
	if held, found, err := q.DomainStreamCeiling(t.Context(), "ABSENT"); err != nil || found || held != 0 {
		t.Fatalf("an absent stream = (%d, %v, %v), want (0, false, nil)", held, found, err)
	}
	const ceiling = int64(3) << 30
	if err := q.EnsureDomainStream(t.Context(), budgetLog("PRESENT", ceiling)); err != nil {
		t.Fatalf("EnsureDomainStream: %v", err)
	}
	if held, found, err := q.DomainStreamCeiling(t.Context(), "PRESENT"); err != nil || !found || held != ceiling {
		t.Fatalf("a present stream = (%d, %v, %v), want (%d, true, nil)", held, found, err, ceiling)
	}
}

// AN ACCOUNT'S BUDGET IS IN CEILING UNITS, from whichever shape the account
// has.
//
// An untiered limit counts every replica of a ceiling, a tiered one is per
// replica count and leaves its top-level limits at zero, and "no tier for this
// replica count" grants nothing rather than everything.
func TestAnAccountsBudgetIsInCeilingUnits(t *testing.T) {
	t.Parallel()
	const gib = int64(1) << 30
	limited := func(store, memory int64, reservedStore, reservedMemory uint64) jetstream.Tier {
		return jetstream.Tier{
			ReservedStore: reservedStore, ReservedMemory: reservedMemory,
			Limits: jetstream.AccountLimits{MaxStore: store, MaxMemory: memory},
		}
	}
	for name, tc := range map[string]struct {
		info     jetstream.AccountInfo
		replicas int
		memory   bool
		want     StorageBudget
	}{
		"untiered, one replica": {
			info:     jetstream.AccountInfo{Tier: limited(30*gib, 0, uint64(4*gib), 0)},
			replicas: 1,
			want:     StorageBudget{Limit: 30 * gib, Committed: 4 * gib, Source: BudgetAccount},
		},
		"untiered, three replicas": {
			info:     jetstream.AccountInfo{Tier: limited(30*gib, 0, uint64(4*gib), 0)},
			replicas: 3,
			want:     StorageBudget{Limit: 10 * gib, Committed: 4 * gib, Source: BudgetAccount},
		},
		"tiered, the replica count's own tier": {
			info: jetstream.AccountInfo{Tiers: map[string]jetstream.Tier{
				"R1": limited(2*gib, 0, 0, 0),
				"R3": limited(30*gib, 0, uint64(4*gib), 0),
			}},
			replicas: 3,
			want:     StorageBudget{Limit: 30 * gib, Committed: 4 * gib, Source: BudgetAccount},
		},
		"tiered, no tier for this replica count": {
			info: jetstream.AccountInfo{Tiers: map[string]jetstream.Tier{
				"R1": limited(30*gib, 0, 0, 0),
			}},
			replicas: 3,
			want:     StorageBudget{Limit: 0, Source: BudgetAccount},
		},
		"no limit stated": {
			info:     jetstream.AccountInfo{Tier: limited(-1, -1, 0, 0)},
			replicas: 1,
			want:     StorageBudget{Limit: -1, Source: BudgetUnstated},
		},
		"memory storage reads the memory limit": {
			info:     jetstream.AccountInfo{Tier: limited(30*gib, 6*gib, 0, uint64(2*gib))},
			replicas: 1, memory: true,
			want: StorageBudget{Limit: 6 * gib, Committed: 2 * gib, Source: BudgetAccount},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := accountBudget(&tc.info, tc.replicas, tc.memory)
			if got != tc.want {
				t.Fatalf("accountBudget = %+v, want %+v", got, tc.want)
			}
			if !got.Source.Valid() {
				t.Errorf("source %q is not one this build knows", got.Source)
			}
		})
	}
	if (StorageBudget{Limit: 4 * gib, Committed: 5 * gib}).Available() != 0 {
		t.Error("an overspent limit reports room")
	}
	if (StorageBudget{Limit: -1}).Available() != -1 {
		t.Error("an unstated limit reports a number")
	}
}
