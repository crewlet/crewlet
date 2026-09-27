package oidc

import (
	"crypto/sha256"
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
	flight := func(verifier string, expires time.Time) Flight {
		return Flight{Verifier: verifier, ExpiresAt: expires}
	}

	r := newRedemptions(4)
	first := flight("f1", at.Add(FlightTTL))
	if !r.Redeem(first, at) {
		t.Fatal("a flight nobody redeemed was refused")
	}
	if r.Redeem(first, at.Add(time.Minute)) {
		t.Error("a flight redeemed a minute earlier was redeemed again")
	}

	// PAST ITS EXPIRY the entry makes way, and a flight of that name is a
	// different flight — Open refused the old one.
	if !r.Redeem(flight("f1", at.Add(2*FlightTTL)), at.Add(FlightTTL)) {
		t.Error("a name whose flight had expired was refused")
	}

	// AT THE BOUND, the oldest goes first and the newest are kept.
	for i := range 6 {
		r.Redeem(flight(fmt.Sprintf("n%d", i), at.Add(2*FlightTTL)),
			at.Add(FlightTTL))
	}
	if n := r.order.Len(); n != 4 || len(r.flights) != 4 {
		t.Errorf("the set holds %d entries (%d names), past its bound of 4", n,
			len(r.flights))
	}
	if r.Redeem(flight("n5", at.Add(2*FlightTTL)), at.Add(FlightTTL)) {
		t.Error("the newest redemption was evicted before the oldest")
	}
}

// A FLIGHT ANY BUILD SEALED OPENS, AND IS EXCHANGED ONCE.
//
// The flight is a cookie one node seals and whichever node the callback reaches
// opens, so during a rolling upgrade the two are different builds — and a
// replay defence keyed on a field this build added would refuse every sign-in
// whose start reached the build before it. So a flight is named by the one
// value every build has sealed, its verifier: sealed here exactly as that
// build sealed it, with no field it never wrote, the flight opens, is redeemed
// once, and its second presentation is refused. And the name is not the S256
// challenge — the digest of the verifier alone, which travelled in the
// authorization request's URL.
//
// Mutation: require a field the previous build never sealed and the flight does
// not open; key the set on anything the replay does not repeat and it is
// redeemed twice; drop the label and the name is the challenge.
func TestAFlightAnyBuildSealedIsExchangedOnce(t *testing.T) {
	t.Parallel()
	cipher, err := secrets.NewCipher(secrets.Keyring{
		ActiveID: "k1", Keys: map[string][]byte{"k1": []byte(strings.Repeat("k", 32))},
	})
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)
	// THE PREVIOUS BUILD'S FLIGHT, field for field.
	sealed, err := cipher.Encrypt(`{"state":"s","nonce":"n",`+
		`"verifier":"a-verifier-the-previous-build-minted","return":"/work",`+
		`"expires_at":"`+at.Add(FlightTTL).Format(time.RFC3339Nano)+`"}`, flightAAD)
	if err != nil {
		t.Fatal(err)
	}
	r := NewRedemptions()
	for i, want := range []bool{true, false} {
		flight, err := Open(cipher, sealed, at)
		if err != nil {
			t.Fatalf("a flight the previous build sealed did not open: %v", err)
		}
		if got := r.Redeem(flight, at); got != want {
			t.Errorf("presentation %d was redeemed = %v, want %v", i+1, got, want)
		}
	}
	other := Flight{Verifier: "another-flight's-verifier", ExpiresAt: at.Add(FlightTTL)}
	if !r.Redeem(other, at) {
		t.Error("a different flight was refused as a replay of the first")
	}
	if nameOf(other) == flightName(sha256.Sum256([]byte(other.Verifier))) {
		t.Error("a flight is named by its S256 challenge, a value its " +
			"authorization request carried in a URL")
	}
}

// ASKING WHETHER A FLIGHT WAS REDEEMED SPENDS NOTHING.
//
// The sign-in surface asks before it waits for a turn at the token endpoint,
// so a known replay never queues for one — and asks for every flight, the
// first presentation of each included. So the question must record nothing: a
// flight it was asked about is still redeemed once, inside its turn, and it
// answers true only for a flight redeemed and not yet expired — past its
// expiry a flight of that name is a different one, which [Redemptions.Redeem]
// makes way for.
//
// Mutation: record the flight in Seen and its own first redemption is refused;
// answer from the name alone and an expired entry reads as a replay.
func TestAskingWhetherAFlightWasRedeemedSpendsNothing(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)
	flight := Flight{Verifier: "a-verifier", ExpiresAt: at.Add(FlightTTL)}

	r := NewRedemptions()
	if r.Seen(flight, at) {
		t.Fatal("a flight nobody redeemed was seen as redeemed")
	}
	if !r.Redeem(flight, at) {
		t.Fatal("a flight that was only asked about was refused its " +
			"redemption, so asking spent it")
	}
	if !r.Seen(flight, at.Add(time.Minute)) {
		t.Error("a flight redeemed a minute earlier was not seen as redeemed")
	}
	if r.Seen(flight, at.Add(FlightTTL)) {
		t.Error("a flight past its expiry was seen as redeemed, though Open " +
			"refuses it and a flight of that name is a different one")
	}
}
