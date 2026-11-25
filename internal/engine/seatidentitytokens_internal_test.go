package engine

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// A SEAT ACCESS SESSION IS ISSUED A SEAT'S TOKEN ONLY FOR ITS USABLE GROWTH
// IDENTITY, named by the email the record keeps, and an admin token only
// where somebody signed in: anything else is ErrNotIssued, which the check
// reads as a person's or the pass's step, never a fault.
//
// Mutation: answer the identity's name rule rather than the record's email,
// and an identity made under another name is checked as one nobody added.
func TestASessionIsIssuedTokensOnlyForWhatTheEngineHolds(t *testing.T) {
	t.Parallel()
	g := newSeatGoogle(t)
	e := googleCloudEngine(t, googleCloudCompany(t), g, time.Now().Add(-time.Hour))
	tokens := seatTokens{engine: e, hc: g.client}
	if _, err := tokens.SeatToken(t.Context(), integration.KindGooglePlay, "builder",
		[]string{analyticsReadonly}); !errors.Is(err, seataccess.ErrNotIssued) {
		t.Fatalf("a seat with no identity: %v", err)
	}
	rec := coord.GoogleIdentity{Handle: "builder", UniqueID: "100000000000000000077", Delegated: true,
		Email: "crewlet-g-builder-0a1b@acme-crewlet-id.iam.gserviceaccount.com", CreatedAt: time.Now()}
	if _, _, err := e.backends.Fleet.CreateGoogleIdentity(t.Context(), rec); err != nil {
		t.Fatal(err)
	}
	seat, err := tokens.SeatToken(t.Context(), integration.KindGooglePlay, "builder",
		[]string{analyticsReadonly})
	if err != nil || !strings.Contains(seat.Token, rec.UniqueID) || seat.Account != rec.Email ||
		!seat.Made.Equal(rec.CreatedAt) {
		t.Fatalf("a usable identity: %+v %v", seat, err)
	}
	if _, err := tokens.SeatToken(t.Context(), integration.KindSlack, "builder",
		[]string{analyticsReadonly}); !errors.Is(err, seataccess.ErrNotIssued) {
		t.Errorf("a kind with no growth identity: %v", err)
	}
	for _, kind := range []integration.Kind{integration.KindGooglePlay, integration.KindSearchConsole} {
		if _, err := tokens.AdminToken(t.Context(), kind); !errors.Is(err, seataccess.ErrNotIssued) {
			t.Errorf("%s with nobody signed in: %v", kind, err)
		}
	}
}

// THE AGENTS TAB'S OFFER IS READ WITH THE ENGINE'S TOKENS, so a Google growth
// lane lists what the company's reader sees rather than nothing.
//
// Mutation: open the offer's session without Tokens, and the property chooser
// lists nothing and says nothing.
func TestTheAgentsTabsOfferIsReadWithTheEnginesTokens(t *testing.T) {
	t.Parallel()
	e := googleCloudEngine(t, googleCloudCompany(t), newSeatGoogle(t), time.Now().Add(-time.Hour))
	company := e.Company()
	if open := e.offerInput(company.Config, company.Org); open.Tokens == nil {
		t.Error("the offer's session holds no token issuer")
	}
}
