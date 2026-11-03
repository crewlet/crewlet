package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// WHETHER A PARK HOLDS A SEAT'S INBOX IS ASKED OF THE NODE HOLDING THE SEAT. A
// park lives in its holder's process, so a reader on another node — the idle
// sweep, wherever the maintenance duty is — asks the incarnation the seat's
// lease names, and is answered for each seat with that node's own parks, never
// the reader's. A reader that holds the seat answers itself. A seat no node
// holds is in no park. And what cannot be said is an error, never "not
// parked": a lease table that cannot be read, or a holder that does not
// answer. A question addressed to another incarnation is not answered.
//
// Mutation: answer from the reader's own parks, read an unreadable lease or a
// silent holder as a seat in no park, or answer a question addressed
// elsewhere, and a case goes red.
func TestWhetherAParkHoldsASeatsInboxIsAskedOfItsHolder(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	q := memory.New()
	if err := q.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(ctx)) })
	parkedSeat, freeSeat := uuid.New(), uuid.New()
	holder := heldParks{owner: "node-a:1", local: func(seat uuid.UUID) bool { return seat == parkedSeat }}
	stop, err := q.Serve(ctx, topics.HeldPark, func(_ context.Context, raw []byte) ([]byte, error) {
		return holder.answer(raw)
	})
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	t.Cleanup(func() { _ = stop(context.WithoutCancel(ctx)) })
	live := func(owner string) leaseTable {
		return leaseTable{lease: &coord.Lease{Owner: owner, ExpiresAt: time.Now().Add(time.Minute)}}
	}
	// THE READER'S OWN PARKS SAY THE OPPOSITE of the holder's, so an answer
	// read off them shows.
	reader := func(leases leaseTable) heldParks {
		return heldParks{owner: "node-b:1", queue: q, leases: leases,
			local: func(seat uuid.UUID) bool { return seat != parkedSeat }, budget: 2 * time.Second,
			now: time.Now}
	}

	for name, tc := range map[string]struct {
		leases  leaseTable
		budget  time.Duration
		seat    uuid.UUID
		parked  bool
		unknown string
	}{
		"a seat the holder parked":       {leases: live("node-a:1"), seat: parkedSeat, parked: true},
		"a seat the holder did not park": {leases: live("node-a:1"), seat: freeSeat},
		"a seat the reader holds":        {leases: live("node-b:1"), seat: freeSeat, parked: true},
		"a seat no node holds": {leases: leaseTable{lease: &coord.Lease{Owner: "node-a:1",
			ExpiresAt: time.Now().Add(-time.Minute)}}, seat: parkedSeat},
		"a lease table that cannot be read": {leases: leaseTable{err: errors.New("the store is down")},
			seat: parkedSeat, unknown: "could not be read"},
		"a holder that does not answer": {leases: live("node-c:1"), budget: 200 * time.Millisecond,
			seat: parkedSeat, unknown: "said nothing"},
	} {
		r := reader(tc.leases)
		if tc.budget > 0 {
			r.budget = tc.budget
		}
		parked, err := r.parked(ctx, tc.seat)
		switch {
		case tc.unknown != "" && (err == nil || !strings.Contains(err.Error(), tc.unknown)):
			t.Errorf("%s: read as parked %v (%v), want an error saying %q", name, parked, err, tc.unknown)
		case tc.unknown == "" && (err != nil || parked != tc.parked):
			t.Errorf("%s: read as parked %v (%v), want %v", name, parked, err, tc.parked)
		}
	}

	elsewhere, err := json.Marshal(heldParkAsk{Version: heldParkWireVersion, Owner: "node-c:1",
		Seat: parkedSeat.String()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := holder.answer(elsewhere); !errors.Is(err, errHeldParkNotAddressed) {
		t.Errorf("a question addressed to another incarnation was answered (%v)", err)
	}
}
