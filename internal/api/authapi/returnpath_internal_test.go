package authapi

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/oidc"
	"github.com/crewlet/crewlet/internal/secrets"
)

// cookieFloor is the size RFC 6265 §6.1 asks every browser to keep a cookie
// at — name, value and attributes — and the most it promises.
const cookieFloor = 4096

// A RETURN PATH IS BOUNDED BY THE COOKIE IT RIDES IN.
//
// The path is sealed into the flight cookie, and a browser DROPS a cookie past
// the floor without a word: the start answered, the person went round the
// provider, and the callback refused them for carrying no flight. So the
// longest path the start keeps is sealed at its WORST — every byte one the
// flight's JSON writes as six — beside the longest login, an invitation's id
// and a keyring id of sixty-four characters, through the real seal and the
// real cookie, and it must fit; and a path one byte longer takes the default,
// as any path this deployment will not honour does.
//
// Mutation: drop the bound, or raise it past what fits, and one of the two
// cases fails.
func TestAReturnPathIsBoundedByTheCookieItRidesIn(t *testing.T) {
	t.Parallel()
	material, err := secrets.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	keyID := strings.Repeat("k", 64)
	cipher, err := secrets.NewCipher(secrets.Keyring{
		ActiveID: keyID, Keys: map[string][]byte{keyID: material},
	})
	if err != nil {
		t.Fatal(err)
	}
	b := config.DefaultBootstrap()
	b.API.ExternalURL = "https://crewlet.example.com"
	s := &Service{boot: &b}
	provider := oidc.Config{
		Issuer: "https://idp.example.com", ClientID: "crewlet",
		ClientSecret: "not-a-real-secret",
		RedirectURI:  "https://crewlet.example.com/auth/oidc/callback",
	}
	at := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)

	// EVERY BYTE ONE JSON WRITES AS SIX, and three kinds of it, so no one
	// escape's size is what the case happens to measure.
	worst := "/" + strings.Repeat("<&\xff", maxReturnPath)[:maxReturnPath-1]
	if kept := returnPath(worst); kept != worst {
		t.Fatalf("a path of %d bytes, the bound, was replaced by %q", len(worst), kept)
	}
	_, sealed, err := provider.Start(cipher, "https://idp.example.com/authorize",
		oidc.Flight{
			Return: worst, Invite: uuid.New().String(),
			Login: strings.Repeat("a", iam.MaxLogin-2) + ".b",
		}, at)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if cookie := s.flightCookie(sealed, at.Add(oidc.FlightTTL)).String(); len(cookie) > cookieFloor {
		t.Errorf("the longest flight's cookie is %d bytes, past the %d a browser "+
			"keeps: it is dropped, and the callback refuses the round trip",
			len(cookie), cookieFloor)
	}
	if kept := returnPath(worst + "x"); kept != "/dashboard" {
		t.Errorf("a path past the bound was kept (%d bytes), want the default",
			len(kept))
	}
}
