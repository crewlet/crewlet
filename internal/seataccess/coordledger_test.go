package seataccess_test

import (
	"encoding/json"
	"testing"

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
