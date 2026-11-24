package seataccess_test

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// A SEAT'S RECORD READS PAST ANOTHER SEAT'S UNREADABLE ONE. Every seat's
// record of one integration shares one listing, and one record this build
// cannot decode is the whole listing's refusal for a sync; a read of one seat
// decodes that seat's alone, so another seat's record never takes it away.
//
// Mutation: answer SeatRecord through Records and the unreadable neighbour
// refuses the read.
func TestASeatsRecordReadsPastAnotherSeatsUnreadableOne(t *testing.T) {
	t.Parallel()
	fleet := memory.NewFleet()
	ledger, err := seataccess.NewCoordLedger(fleet)
	if err != nil {
		t.Fatal(err)
	}
	mine, err := json.Marshal(seataccess.Record{Kind: integration.KindGitHub, Handle: "ceo"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if err := fleet.PutSeatAccessRecord(ctx, string(integration.KindGitHub), "ceo", mine); err != nil {
		t.Fatal(err)
	}
	if err := fleet.PutSeatAccessRecord(ctx, string(integration.KindGitHub), "sre", []byte("{")); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Records(ctx, integration.KindGitHub); err == nil {
		t.Fatal("the premise: the listing reads past an unreadable record")
	}
	rec, found, err := ledger.SeatRecord(ctx, integration.KindGitHub, "ceo")
	if err != nil || !found || rec.Handle != "ceo" || rec.Kind != integration.KindGitHub {
		t.Fatalf("the seat's own record reads %+v, %v, %v", rec, found, err)
	}
	if _, found, err := ledger.SeatRecord(ctx, integration.KindGitHub, "nobody"); err != nil || found {
		t.Errorf("a seat with no record reads %v, %v", found, err)
	}
	if _, _, err := ledger.SeatRecord(ctx, integration.KindGitHub, "sre"); err == nil {
		t.Error("the unreadable record reads as a record")
	}
}

// listingsCounted is the seat access ledger counting its listings.
type listingsCounted struct {
	coord.SeatAccess
	listed atomic.Int32
}

func (l *listingsCounted) SeatAccessRecords(ctx context.Context, kind string) (map[string][]byte, error) {
	l.listed.Add(1)
	return l.SeatAccess.SeatAccessRecords(ctx, kind)
}

// A SEAT'S RECORD IS READ ALONE: a turn reads its seat's record on the seat's
// node, and a listing of the integration's ledger would carry every other
// seat's record across that node's link, each up to MaxSeatAccessBytes, in a
// time given to one.
//
// Mutation: answer SeatRecord through the listing again, and it is counted.
func TestASeatsRecordIsReadAlone(t *testing.T) {
	t.Parallel()
	fleet := memory.NewFleet()
	store := &listingsCounted{SeatAccess: fleet}
	ledger, err := seataccess.NewCoordLedger(store)
	if err != nil {
		t.Fatal(err)
	}
	for _, handle := range []string{"ceo", "sre"} {
		value, err := json.Marshal(seataccess.Record{Kind: integration.KindGitHub, Handle: handle})
		if err != nil {
			t.Fatal(err)
		}
		if err := fleet.PutSeatAccessRecord(t.Context(), string(integration.KindGitHub), handle, value); err != nil {
			t.Fatal(err)
		}
	}
	rec, found, err := ledger.SeatRecord(t.Context(), integration.KindGitHub, "sre")
	if err != nil || !found || rec.Handle != "sre" {
		t.Fatalf("the seat's record reads %+v, %v, %v", rec, found, err)
	}
	if n := store.listed.Load(); n != 0 {
		t.Errorf("reading one seat's record listed its integration's ledger %d times", n)
	}
}
