package oidc

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/secrets"
)

// A FLIGHT IS REDEEMED ONCE, FORGOTTEN WHEN IT EXPIRES, AND THE SET IS BOUNDED.
//
// The set is what keeps a stockpiled cookie from being an exchange at the
// provider per request, so its three properties are the whole of it: a second
// redemption of one flight is refused while that flight could still open; an
// entry outlives nothing — past its flight's expiry [Open] refuses the flight
// anyway, and holding it would only crowd the live ones; and it never holds
// more than its bound, so a flood of flights costs this node a fixed amount of
// memory and evicts the OLDEST first.
//
// Mutation: skip the lookup and the replay is redeemed; drop the bound and the
// set grows with the flood.
func TestAFlightIsRedeemedOnceAndTheSetIsBounded(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)
	flight := func(id string, expires time.Time) Flight {
		return Flight{ID: id, ExpiresAt: expires}
	}

	r := newRedemptions(4)
	first := flight("f1", at.Add(FlightTTL))
	if !r.Redeem(first, at) {
		t.Fatal("a flight nobody redeemed was refused")
	}
	if r.Redeem(first, at.Add(time.Minute)) {
		t.Error("a flight redeemed a minute earlier was redeemed again")
	}

	// PAST ITS EXPIRY the entry makes way, and a flight bearing the id is
	// a different flight — Open refused the old one.
	if !r.Redeem(flight("f1", at.Add(2*FlightTTL)), at.Add(FlightTTL)) {
		t.Error("an id whose flight had expired was refused")
	}

	// AT THE BOUND, the oldest goes first and the newest are kept.
	for i := range 6 {
		r.Redeem(flight(fmt.Sprintf("n%d", i), at.Add(2*FlightTTL)),
			at.Add(FlightTTL))
	}
	if n := r.order.Len(); n != 4 || len(r.ids) != 4 {
		t.Errorf("the set holds %d entries (%d ids), past its bound of 4", n,
			len(r.ids))
	}
	if r.Redeem(flight("n5", at.Add(2*FlightTTL)), at.Add(FlightTTL)) {
		t.Error("the newest redemption was evicted before the oldest")
	}
}

// A FLIGHT WITHOUT AN ID DOES NOT OPEN.
//
// The id is what a replay is refused on, so a flight carrying none could be
// presented for as long as it lived: [Open] refuses it with the other values a
// round trip cannot do without.
//
// Mutation: drop the id from Open's check and the id-less flight opens.
func TestAFlightWithoutAnIDDoesNotOpen(t *testing.T) {
	t.Parallel()
	cipher, err := secrets.NewCipher(secrets.Keyring{
		ActiveID: "k1", Keys: map[string][]byte{"k1": []byte(strings.Repeat("k", 32))},
	})
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)
	seal := func(f Flight) string {
		t.Helper()
		body, err := json.Marshal(f)
		if err != nil {
			t.Fatal(err)
		}
		sealed, err := cipher.Encrypt(string(body), flightAAD)
		if err != nil {
			t.Fatal(err)
		}
		return sealed
	}
	whole := Flight{ID: "an-id", State: "s", Nonce: "n", Verifier: "v",
		ExpiresAt: at.Add(FlightTTL)}
	if _, err := Open(cipher, seal(whole), at); err != nil {
		t.Fatalf("a whole flight did not open: %v", err)
	}
	whole.ID = ""
	if _, err := Open(cipher, seal(whole), at); err == nil {
		t.Error("a flight carrying no id opened, so nothing could refuse its replay")
	}
}
