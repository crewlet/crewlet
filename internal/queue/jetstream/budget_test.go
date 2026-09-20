package jetstream

import (
	"context"
	"errors"
	"strings"
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

// AND A REFUSED RESERVATION IS NOT READ BACK, because nothing was placed.
//
// The peer-race read-back exists for a create the broker may still be
// committing. A refused reservation is the opposite: the limits check runs
// only once no peer holds an assignment for the name, so there is no object to
// become visible. Left to fall through it spent the whole read-back window per
// refused log and then wrapped the one error a refused boot needs — the bytes,
// the limit and the Tier A field — in "(and it is not there: stream not
// found)", which reads as the cause.
func TestARefusedReservationIsNotReadBack(t *testing.T) {
	t.Parallel()
	q := fileQueue(t)
	budget, err := q.StreamBudget(t.Context())
	if err != nil {
		t.Fatalf("StreamBudget: %v", err)
	}
	err = q.EnsureDomainStream(t.Context(), budgetLog("REFUSED", budget.Available()+1))
	if !errors.Is(err, ErrInsufficientStorage) {
		t.Fatalf("EnsureDomainStream = %v, want the refusal", err)
	}
	// THE READ-BACK'S OWN WORDS, which only a fall-through can produce.
	for _, unwanted := range []string{"and it is not there", "stream not found"} {
		if strings.Contains(err.Error(), unwanted) {
			t.Errorf("the refusal carries %q, so it was read back after a create "+
				"that placed nothing:\n%v", unwanted, err)
		}
	}
	if errors.Is(err, jetstream.ErrStreamNotFound) {
		t.Errorf("the refusal wraps a not-found from the read-back: %v", err)
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

// ONLY A CREATE'S RESERVATION CHECK IS READ AS ONE.
//
// The refusal is named with the numbers of a reservation, so a code the broker
// uses for anything else would be reported as a ceiling that did not fit:
// placement is about members this node cannot see, and `insufficient
// resources` answers a publish or a consumer, never a stream's create.
func TestOnlyAReservationCheckIsReadAsARefusedReservation(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"the file store's limit":   {&jetstream.APIError{ErrorCode: 10047}, true},
		"the memory store's limit": {&jetstream.APIError{ErrorCode: 10028}, true},
		"no suitable peers":        {&jetstream.APIError{ErrorCode: 10005}, false},
		"insufficient resources":   {&jetstream.APIError{ErrorCode: 10023}, false},
		"not an API error":         {errors.New("connection closed"), false},
		"no error":                 {nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := refusedStorage(tc.err); got != tc.want {
				t.Errorf("refusedStorage(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
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
		// UNDER ITS OWN SOURCE, which is the half a bare zero could not
		// carry. The server refuses every create on such an account
		// before it compares a byte, so the answer is not a capacity
		// that happens to be spent and what clears it is a setting
		// rather than room.
		"tiered, no tier for this replica count": {
			info: jetstream.AccountInfo{Tiers: map[string]jetstream.Tier{
				"R1": limited(30*gib, 0, 0, 0),
				"R5": limited(90*gib, 0, 0, 0),
			}},
			replicas: 3,
			want:     StorageBudget{Limit: 0, Source: BudgetAccountNoTier},
		},
		// A TIER THAT IS THERE AND STATES NOTHING is the same refusal
		// wearing the other shape. The account's report lists a class it
		// merely holds OBJECTS in, with no limit ever set for it, so
		// "present" does not mean "declared".
		"tiered, this node's class present with no limit on it": {
			info: jetstream.AccountInfo{Tiers: map[string]jetstream.Tier{
				"R1": limited(30*gib, 0, 0, 0),
				"R3": {ReservedStore: uint64(gib)},
			}},
			replicas: 3,
			want: StorageBudget{Limit: 0, Committed: gib,
				Source: BudgetAccountTierNoLimit},
		},
		// AND ZERO IS NOT THE SAME TEST AS BELOW ZERO. A tier declared
		// unlimited reports its limit negative and the broker creates
		// against it, so a `<= 0` reading would refuse the tiered
		// account with the most room of all.
		"tiered, and this node's class is unlimited": {
			info: jetstream.AccountInfo{Tiers: map[string]jetstream.Tier{
				"R3": limited(-1, -1, 0, 0),
			}},
			replicas: 3,
			want:     StorageBudget{Limit: -1, Source: BudgetUnstated},
		},
		// AND THE DISCRIMINATOR IS THE PRESENCE OF TIERS, NOT OF THIS
		// ONE: asked the second question, the case above fell through
		// to the un-tiered branch and read that account's unset
		// top-level MaxStore as a limit somebody had set.
		"tiered, and this node's class is the only one": {
			info: jetstream.AccountInfo{Tiers: map[string]jetstream.Tier{
				"R3": limited(30*gib, 0, uint64(gib), 0),
			}},
			replicas: 3,
			want:     StorageBudget{Limit: 30 * gib, Committed: gib, Source: BudgetAccount},
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
