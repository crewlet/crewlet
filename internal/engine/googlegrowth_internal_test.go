package engine

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

const analyticsReadonly = "https://www.googleapis.com/auth/analytics.readonly"

// A GROWTH TOKEN IS MINTED ONLY FOR A USABLE RECORDED IDENTITY, with the
// product's scopes: a seat with no record, or one the pass turned off or has
// not delegated yet, is told to wait, and Google is asked for nothing.
//
// Mutation: mint for a record that is not Usable, and a turned-off seat is
// handed a token for an account Google refuses.
func TestAGrowthTokenIsMintedOnlyForAUsableRecordedIdentityWithItsScopes(t *testing.T) {
	t.Parallel()
	g := newSeatGoogle(t)
	c := googleCloudCompany(t)
	e := googleCloudEngine(t, c, g, time.Now().Add(-time.Hour))
	ask := func() error {
		_, err := e.GoogleGrowthToken(t.Context(), "builder", []string{analyticsReadonly}, time.Minute, g.client)
		return err
	}
	if err := ask(); !errors.Is(err, errGrowthIdentityNotReady) {
		t.Fatalf("a seat with no identity answered %v", err)
	}
	rec := coord.GoogleIdentity{Handle: "builder", UniqueID: "100000000000000000077",
		Email: "crewlet-builder-0a1b2c@acme-crewlet-id.iam.gserviceaccount.com", CreatedAt: time.Now()}
	for _, state := range []struct {
		name                string
		disabled, delegated bool
	}{{"undelegated", false, false}, {"turned off", true, true}} {
		rec.Disabled, rec.Delegated = state.disabled, state.delegated
		stored, _, err := e.backends.Fleet.GoogleIdentity(t.Context(), "builder")
		if err != nil {
			t.Fatal(err)
		}
		if stored.Version == 0 {
			if _, _, err := e.backends.Fleet.CreateGoogleIdentity(t.Context(), rec); err != nil {
				t.Fatal(err)
			}
		} else {
			rec.Version = stored.Version
			if _, _, err := e.backends.Fleet.UpdateGoogleIdentity(t.Context(), rec); err != nil {
				t.Fatal(err)
			}
		}
		if err := ask(); !errors.Is(err, errGrowthIdentityNotReady) {
			t.Errorf("an %s identity answered %v", state.name, err)
		}
	}
	if g.minted() != 0 {
		t.Fatalf("Google was asked for %d tokens for an identity not ready", g.minted())
	}
	stored, _, _ := e.backends.Fleet.GoogleIdentity(t.Context(), "builder")
	stored.Disabled, stored.Delegated = false, true
	if _, _, err := e.backends.Fleet.UpdateGoogleIdentity(t.Context(), stored); err != nil {
		t.Fatal(err)
	}
	token, err := e.GoogleGrowthToken(t.Context(), "builder", []string{analyticsReadonly}, time.Minute, g.client)
	if err != nil || !strings.Contains(token.Token, rec.UniqueID) {
		t.Fatalf("a usable identity's token: %+v, %v", token, err)
	}
	g.mu.Lock()
	scopes := slices.Clone(g.scopes)
	g.mu.Unlock()
	if len(scopes) != 1 || !slices.Equal(scopes[0], []string{analyticsReadonly}) {
		t.Errorf("minted with %v, want the product's scope alone", scopes)
	}
}

// TURNING GOOGLE CLOUD ON FOR A SEAT DOES NOT TURN ITS GROWTH IDENTITY ON: the
// two identities follow different lanes, so neither lane's on or off moves
// the other's account.
func TestGoogleCloudsLaneNeverCountsTowardTheGrowthIdentity(t *testing.T) {
	t.Parallel()
	c := googleCloudCompany(t)
	seats := growthSeats(c.Config)
	if len(seats) == 0 {
		t.Fatal("no agent seat was read")
	}
	for _, seat := range seats {
		if seat.On {
			t.Errorf("%s's growth identity is on with only Google Cloud on", seat.Handle)
		}
	}
}
