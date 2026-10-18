package engine

import (
	"context"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
)


// THE BUCKET'S RESERVATION IS A QUARTER OF THE BROKER, AND NEVER EATS THE
// COORDINATION REMAINDER: the logs' half and a gibibyte are always left, and
// the field's own bounds hold whatever the broker says.
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
		if c.limit >= 8*gib && got+max(c.limit/2, c.declared)+objectsCoordinationFloor > c.limit {
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
	if got, explicit := objectsReservation(t.Context(), b, budgetOf{Limit: 400 * gib}); got != 3*gib || !explicit {
		t.Fatalf("a written reservation came back as %d (explicit %v)", got, explicit)
	}
	b.Store.Objects.NatsMaxBytes = 0
	if got, explicit := objectsReservation(t.Context(), b, budgetOf{Limit: 400 * gib}); got != 100*gib || explicit {
		t.Fatalf("a derived reservation came back as %d (explicit %v), want a quarter of 400 GiB", got, explicit)
	}
}
