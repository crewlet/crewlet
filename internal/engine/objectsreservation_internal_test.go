package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
)

// THE BUCKET'S RESERVATION IS A QUARTER OF THE BROKER, AND NEVER EATS THE
// COORDINATION REMAINDER: the logs' share ([StreamBudgetShare]) and a gibibyte
// are always left, and the field's own bounds hold whatever the broker says.
func TestTheBucketReservationLeavesTheCoordinationRemainder(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name            string
		limit, declared int64
		want            int64
	}{
		{"a large broker gives a quarter", 400 * gib, 0, 100 * gib},
		{"a small broker keeps the floor beside the logs", 4 * gib, 0, gib},
		{"declared logs past the share take their bytes first", 100 * gib, 70 * gib, 25 * gib},
		{"declared logs that leave little shrink it", 100 * gib, 90 * gib, 9 * gib},
		{"a broker with no room still asks for the field's floor", 0, 0, config.NatsObjectsMaxBytesFloor},
		{"a vast broker is held to the field's ceiling", 1 << 50, 0, config.NatsObjectsMaxBytesCeiling},
	} {
		got := derivedObjectsReservation(c.limit, c.declared)
		if got != c.want {
			t.Errorf("%s: %d bytes, want %d", c.name, got, c.want)
		}
		share := int64(float64(c.limit) * StreamBudgetShare)
		if c.limit >= 8*gib && got+max(share, c.declared)+objectsCoordinationFloor > c.limit {
			t.Errorf("%s: %d bytes leaves no coordination remainder in a %d-byte broker",
				c.name, got, c.limit)
		}
	}
}

type budgetOf jetstream.StorageBudget

func (b budgetOf) StreamBudget(context.Context) (jetstream.StorageBudget, error) {
	return jetstream.StorageBudget(b), nil
}

// AN OPERATOR'S FIGURE IS TAKEN AS WRITTEN, and an unset one is derived from
// the limit the broker states.
func TestTheBucketReservationIsTheOperatorsWhereWritten(t *testing.T) {
	t.Parallel()
	b := &config.Bootstrap{}
	b.Store.Objects.NatsMaxBytes = 3 * gib
	if got, explicit := objectsReservation(t.Context(), b, budgetOf{Limit: 400 * gib}, unmeasured); got != 3*gib || !explicit {
		t.Fatalf("a written reservation came back as %d (explicit %v)", got, explicit)
	}
	b.Store.Objects.NatsMaxBytes = 0
	if got, explicit := objectsReservation(t.Context(), b, budgetOf{Limit: 400 * gib}, unmeasured); got != 100*gib || explicit {
		t.Fatalf("a derived reservation came back as %d (explicit %v), want a quarter of 400 GiB", got, explicit)
	}
}

// unmeasured is a volume no case should measure: the broker states its limit,
// or the operator wrote the figure.
func unmeasured(dir string) (int64, error) {
	return 0, errors.New("the volume was measured although the broker states its limit")
}

// unreadBudget is a broker whose budget cannot be read.
type unreadBudget struct{}

func (unreadBudget) StreamBudget(context.Context) (jetstream.StorageBudget, error) {
	return jetstream.StorageBudget{}, errors.New("no responders")
}

// A BROKER THAT STATES NO LIMIT IS RESERVED FROM THE CAP A SERVER RUNNING ITS
// DEFAULTS TAKES of the free space this node measured — three quarters
// ([unstatedLimit]), the figure the logs are sized against too — and not from
// the whole of it, which the server can never grant: from the whole, the
// bucket's quarter and the logs' share of what it left came within a
// thirty-second of a defaults server's cap (TestAnUnstatedServerHoldsTheBucketAndEveryLog).
// A budget that cannot be read is the same reading, and a volume that cannot
// be measured leaves the bucket at its smallest.
func TestAnUnstatedBrokersBucketIsReservedFromWhatItCanGrant(t *testing.T) {
	t.Parallel()
	const free = 40 * gib
	measured := func(string) (int64, error) { return free, nil }
	for _, c := range []struct {
		name   string
		broker any
		freeOf func(string) (int64, error)
		want   int64
	}{
		// A 30 GiB stand-in: a quarter is 7.5 GiB, inside what the logs'
		// share and the coordination floor leave (10.25 GiB). From the
		// whole 40 GiB it was 10.
		{"a broker that states no limit", budgetOf{Limit: -1, Source: jetstream.BudgetUnstated},
			measured, 30 * gib / 4},
		{"a budget that cannot be read", unreadBudget{}, measured, 30 * gib / 4},
		{"a queue with no budget to read", struct{}{}, measured, 30 * gib / 4},
		{"a volume that cannot be measured", budgetOf{Limit: -1, Source: jetstream.BudgetUnstated},
			func(string) (int64, error) { return 0, errors.New("statfs: no such file") },
			config.NatsObjectsMaxBytesFloor},
	} {
		b := &config.Bootstrap{}
		if got, explicit := objectsReservation(t.Context(), b, c.broker, c.freeOf); got != c.want || explicit {
			t.Errorf("%s: the bucket reserves %d bytes (explicit %v), want %d", c.name, got, explicit, c.want)
		}
	}
}
