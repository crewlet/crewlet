package seataccess_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/seataccess"
)

// EVERY SEAT A PASS READS BACK, ONCE AND SORTED: the document's seats and the
// seats only the ledger still records, so a departed seat is observed too.
func TestObserveInputHandles(t *testing.T) {
	t.Parallel()
	in := seataccess.ObserveInput{
		Seats: []seataccess.SeatRef{{Handle: "sre"}, {Handle: "analyst"}, {Handle: "sre"}},
		Records: map[string]seataccess.Record{
			"sre": {Handle: "sre"}, "old": {Handle: "old"},
		},
	}
	if got, want := in.Handles(), []string{"analyst", "old", "sre"}; !slices.Equal(got, want) {
		t.Errorf("handles = %q, want %q", got, want)
	}
	if got := (seataccess.ObserveInput{}).Handles(); len(got) != 0 {
		t.Errorf("handles of an empty input = %q", got)
	}
}

// A HANDLE'S LEDGER RECORD, or nil where the ledger holds none, so a vendor
// tells "never recorded" from "recorded with no account".
func TestObserveInputRecord(t *testing.T) {
	t.Parallel()
	in := seataccess.ObserveInput{Records: map[string]seataccess.Record{
		"sre": {Handle: "sre", Account: &seataccess.Account{ID: "u-1"}},
	}}
	rec := in.Record("sre")
	if rec == nil || rec.Account == nil || rec.Account.ID != "u-1" {
		t.Fatalf("record = %+v, want the ledger's", rec)
	}
	rec.Handle = "changed"
	if in.Records["sre"].Handle != "sre" {
		t.Error("the record handed out aliases the input's map entry")
	}
	if got := in.Record("fresh"); got != nil {
		t.Errorf("record of an unrecorded seat = %+v, want nil", got)
	}
}

// A STEP NAMES THE SEAT BY ITS NAME, trimmed, and by its handle where the
// name is blank.
func TestSeatRefLabel(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		seat seataccess.SeatRef
		want string
	}{
		{seataccess.SeatRef{Handle: "sre", Name: "Site Reliability"}, "Site Reliability"},
		{seataccess.SeatRef{Handle: "sre", Name: "  Site Reliability "}, "Site Reliability"},
		{seataccess.SeatRef{Handle: "sre", Name: "   "}, "sre"},
		{seataccess.SeatRef{Handle: "sre"}, "sre"},
	} {
		if got := tc.seat.Label(); got != tc.want {
			t.Errorf("%+v label = %q, want %q", tc.seat, got, tc.want)
		}
	}
}
