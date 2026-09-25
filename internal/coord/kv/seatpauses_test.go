package kv

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// AN UNREACHABLE STORE IS NOT UNPAUSED.
//
// "This seat has no pause" lets the seat take work; "the store could not be
// read" says nothing about it. A seat a person stopped because it was doing
// damage must not start again because a read failed, so every read answers the
// outage as an error that is not a false — the one listing included, which
// every node's cache is rebuilt from.
func TestAnUnreachableStoreIsNotUnpaused(t *testing.T) {
	nc := embeddedNATS(t)
	store := openFleet(t, nc)
	if _, created, err := store.CreateSeatPause(t.Context(), coord.SeatPause{
		Handle: "swe", By: "jane-token", At: time.Now(),
	}); err != nil || !created {
		t.Fatalf("CreateSeatPause = (%v, %v)", created, err)
	}

	dead, cancel := context.WithCancel(t.Context())
	cancel()
	p, found, err := store.SeatPause(dead, "swe")
	if err == nil {
		t.Fatalf("SeatPause on a dead context = (%+v, %v) with no error: an outage "+
			"answered as a fact about the seat", p, found)
	}
	if !errors.Is(err, coord.ErrUnavailable) {
		t.Errorf("err = %v, want it to wrap ErrUnavailable so a caller can tell an "+
			"outage from a refusal", err)
	}
	if all, err := store.ListSeatPauses(dead); err == nil {
		t.Fatalf("ListSeatPauses on a dead context = %+v with no error: every node "+
			"rebuilding its cache from this would read the seat as free", all)
	}

	// And the same after the connection itself is gone.
	nc.Close()
	if _, found, err := store.SeatPause(t.Context(), "swe"); err == nil {
		t.Fatalf("SeatPause over a closed connection answered found=%v with no error", found)
	}
	if gone, err := store.DeleteSeatPause(t.Context(), "swe", 1); err == nil || gone {
		t.Fatalf("DeleteSeatPause over a closed connection = (%v, %v), want an error "+
			"that is not a lost race", gone, err)
	}
}
