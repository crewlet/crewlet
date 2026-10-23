package engine

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/words"
)

// A RETENTION GESTURE'S REFUSAL IS TOLD, AND A FAULT UNDER IT IS NOT.
//
// The retention routes answer a refusal the engine tells ([statelog.Told]) in
// its sentence and keep the error for their log, so every refusal a capacity
// change, a reanchor or a node gate makes has to be one, in words that carry
// neither the package that wrote them nor the broker's own text. Mutation:
// return any of these as the `engine: ...` error it was and its case fails.
func TestAGestureRefusalIsToldWithoutWhatItStandsOn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e, _, stream := capacityNode(t, budgetHost{room: unstatedRoom})

	clean := func(t *testing.T, err error, says string) {
		t.Helper()
		said, told := statelog.Tell(err)
		if !told {
			t.Fatalf("%v tells its operator nothing", err)
		}
		if !strings.Contains(said, says) {
			t.Errorf("it tells %q, want it to say %q", said, says)
		}
		for _, leak := range []string{"engine:", "statelog:", "nats:", "—"} {
			if strings.Contains(said, leak) {
				t.Errorf("it tells %q, carrying %q", said, leak)
			}
		}
	}

	t.Run("a target past int64", func(t *testing.T) {
		_, err := e.SetCapacity(ctx, CapacityRequest{Stream: stream,
			TargetMaxBytes: math.MaxUint64, By: "ops-3"})
		clean(t, err, "unbounded")
	})
	t.Run("a stream this node does not keep", func(t *testing.T) {
		_, err := e.SetCapacity(ctx, CapacityRequest{Stream: "CREWLET_NOPE_LOG",
			TargetMaxBytes: 1 << 30, By: "ops-3"})
		if !errors.Is(err, ErrUnknownStream) {
			t.Fatalf("an unknown stream answered %v, want ErrUnknownStream", err)
		}
		clean(t, err, `"CREWLET_NOPE_LOG" isn't a log this node keeps. Use `+
			words.Or(maintenanceStreams()))
	})
	t.Run("a raise the broker refused", func(t *testing.T) {
		err := e.applyFailure(ctx, coord.MaintenanceOperation{Stream: stream,
			TargetMaxBytes: 7 << 30}, fmt.Errorf("nats: %w",
			jetstream.ErrInsufficientStorage))
		clean(t, err, "the broker refused")
		if !strings.Contains(err.Error(), "nats:") {
			t.Errorf("the error %q lost the broker's words, which the log keeps", err)
		}
	})
	// AND WITH THE BROKER'S LIMIT STATED, which is the ordinary embedded
	// case and the one that splices what the broker had left into the
	// sentence: with no stated room the clause never appeared, so the case
	// above could not see what it said. Mutation: put the em dash back in
	// capacityRefusal and this fails.
	t.Run("a raise the broker refused, its limit stated", func(t *testing.T) {
		stated, _, statedStream := capacityNode(t, budgetHost{room: jetstream.StorageBudget{
			Limit: 8 << 30, Committed: 7 << 30, Source: jetstream.BudgetServerStore}})
		err := stated.applyFailure(ctx, coord.MaintenanceOperation{Stream: statedStream,
			TargetMaxBytes: 7 << 30}, fmt.Errorf("nats: %w",
			jetstream.ErrInsufficientStorage))
		clean(t, err, "It asked to reserve 7516192768 bytes, and the broker had")
	})
	t.Run("a raise whose outcome is unknown", func(t *testing.T) {
		err := e.applyFailure(ctx, coord.MaintenanceOperation{Stream: stream,
			TargetMaxBytes: 7 << 30}, errors.New("nats: timeout"))
		clean(t, err, "the outcome is unknown")
	})
	// A STREAM NO WINDOW CAN BE OPEN ON is one somebody mistyped, told as
	// that by every capacity verb rather than as a log with no window:
	// "there's no capacity operation on X" sent an operator looking for a
	// window that could never exist. Mutation: drop windowStream from any of
	// the three and its case fails.
	t.Run("a window verb on a stream no window can be open on", func(t *testing.T) {
		const nope = "CREWLET_NOPE_LOG"
		_, abandonErr := e.AbandonCapacity(ctx, nope, "capacity:open", iam.Actor{Name: "ops-3"})
		_, excludeErr := e.ExcludeParticipant(ctx, nope, "node-2", iam.Actor{Name: "ops-3"})
		_, _, _, _, statusErr := e.CapacityStatus(ctx, nope)
		for verb, err := range map[string]error{"abandonment": abandonErr,
			"exclusion": excludeErr, "status": statusErr} {
			if !errors.Is(err, ErrUnknownStream) {
				t.Errorf("the %s of %s answered %v, want ErrUnknownStream", verb, nope, err)
				continue
			}
			clean(t, err, `"CREWLET_NOPE_LOG" isn't a log this node keeps. Use `+
				words.Or(maintenanceStreams()))
		}
	})
	// AN ABANDONMENT CONFIRMING ANOTHER WINDOW is told what is open, so the
	// operator repeats the right id. Mutation: drop its Tell and this fails.
	t.Run("an abandonment confirming another window", func(t *testing.T) {
		clean(t, &ConfirmMismatchError{Stream: stream, Open: "capacity:open",
			Named: "capacity:other"}, "so nothing was abandoned")
	})
	t.Run("an abandon of nothing", func(t *testing.T) {
		_, err := e.AbandonCapacity(ctx, stream, "capacity:open", iam.Actor{Name: "ops-3"})
		clean(t, err, "no capacity operation")
	})
	t.Run("a gesture in a mode that appends nothing", func(t *testing.T) {
		err := NotPublishing("an eviction", statelog.ModeSeal)
		if !errors.Is(err, ErrNotPublishing) {
			t.Fatalf("%v is not ErrNotPublishing", err)
		}
		clean(t, err, "normal mode")
	})
	t.Run("a gate on a node id nothing runs under", func(t *testing.T) {
		err := GateRequest{Node: "node*", OpID: statelog.NewOpID(time.Now(), "evict"),
			By: iam.Principal{Login: "ops-3"}}.valid()
		if !errors.Is(err, ErrInvalidGate) {
			t.Fatalf("%v is not ErrInvalidGate", err)
		}
		clean(t, err, `"node*" isn't a node id`)
	})
	t.Run("an eviction nobody could judge", func(t *testing.T) {
		clean(t, &GateUnjudged{Node: "node-4", Err: errors.New("nats: timeout")},
			"node-4 is still running")
	})
	t.Run("a readmission nobody could judge", func(t *testing.T) {
		clean(t, &ReadmissionUnjudged{Node: "node-4", Log: "tracker",
			Err: errors.New("nats: timeout")}, "on tracker")
	})
	// A LOG THE FLEET RE-ANCHORED PAST THIS NODE is a condition another
	// node clears, told as one: it was a plain error, answered as a 500
	// fault with the one remedy that works (ask a node that has caught up)
	// in this node's log alone.
	t.Run("a readmission on a log this node is behind on", func(t *testing.T) {
		err := &ReadmissionUnjudged{Node: "node-4", Log: "tracker", Behind: true,
			Err: &statelog.ReanchorNotApplied{Node: "node-4", Domain: "tracker",
				Reported: 3, Runs: 2}}
		clean(t, err, "re-anchored that log and this node hasn't applied it yet")
		if remedy := err.Remedy(); !strings.Contains(remedy.Detail, "ask a node that has applied it") {
			t.Errorf("the remedy %q doesn't send the operator to another node", remedy.Detail)
		}
	})
}
