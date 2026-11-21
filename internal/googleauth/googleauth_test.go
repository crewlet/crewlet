package googleauth_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/googleauth"
	"github.com/crewlet/crewlet/internal/integration"
)

// THE EXCHANGE IS ALWAYS CLOUD-PLATFORM AND ONLY THE IMPERSONATION CARRIES A
// PRODUCT'S SCOPES: STS is asked for cloud-platform whatever the token is for,
// the provisioner is impersonated at cloud-platform, and the seat's account at
// exactly the scopes asked.
//
// Mutation: hard-wire the impersonation to cloud-platform again, and the seat
// is minted a token no product API accepts.
func TestTheExchangeIsCloudPlatformAndOnlyTheImpersonationCarriesProductScopes(t *testing.T) {
	t.Parallel()
	f, conn := newFake(t)
	token, expires, err := conn.SeatToken(t.Context(), seatAccount, []string{analytics}, time.Hour)
	if err != nil || token == "" || !expires.Equal(f.clock.now().Add(time.Hour)) {
		t.Fatalf("seat token held %v, expires %s, %v", token != "", expires, err)
	}
	for _, form := range f.exchanges {
		if got := form["scope"]; !slices.Equal(got, []string{googleauth.ScopeCloudPlatform}) {
			t.Errorf("STS was asked for %v, want cloud-platform alone", got)
		}
	}
	if got := f.mintsFor(provisioner); len(got) != 1 || !slices.Equal(got[0], []string{googleauth.ScopeCloudPlatform}) {
		t.Errorf("the provisioner was minted %v, want one cloud-platform token", got)
	}
	if got := f.mintsFor(seatAccount); len(got) != 1 || !slices.Equal(got[0], []string{analytics}) {
		t.Errorf("the seat was minted %v, want the analytics scope alone", got)
	}
	if err := conn.Do(t.Context(), http.MethodGet, "https://analyticsdata.googleapis.com/v1beta/x", token, nil, nil); err != nil {
		t.Errorf("the product refused a token minted at its scope: %v", err)
	}
}

// A TOKEN IS NEVER HANDED TO A CALLER THAT ASKED FOR OTHER SCOPES: the issuer
// keys its cache by the sorted scope set, so the same set in another order is
// one mint, and another set is a mint of its own.
//
// Mutation: key the cache by account alone, and an analytics token is handed
// to a caller that asked for adwords.
func TestAnIssuedTokenIsKeyedByItsScopeSet(t *testing.T) {
	t.Parallel()
	f, conn := newFake(t)
	issuer := &googleauth.SeatIssuer{Now: f.clock.now}
	both, err := issuer.Token(t.Context(), conn, seatAccount, []string{analytics, adwords}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	reordered, err := issuer.Token(t.Context(), conn, seatAccount, []string{adwords, analytics}, time.Minute)
	if err != nil || reordered.Token != both.Token {
		t.Errorf("the same scopes in another order were minted afresh (%v)", err)
	}
	one, err := issuer.Token(t.Context(), conn, seatAccount, []string{adwords}, time.Minute)
	if err != nil || one.Token == both.Token {
		t.Errorf("a caller asking for adwords alone was handed the two-scope token (%v)", err)
	}
	if got := f.mintsFor(seatAccount); len(got) != 2 {
		t.Errorf("%d mints for the seat, want one per scope set: %v", len(got), got)
	}
	if _, err := issuer.Token(t.Context(), conn, seatAccount, nil, time.Minute); err == nil {
		t.Error("a token was issued with no scope named")
	}
}

// A FEDERATED ACCOUNT'S TOKEN IS CACHED PER SCOPE SET AND RENEWED BEFORE IT
// ENDS: a second call within the hour is the cached token, a call within five
// minutes of its end mints afresh, and another scope set never shares it.
func TestAFederatedTokenIsHeldPerScopeSetAndRenewedEarly(t *testing.T) {
	t.Parallel()
	f, conn := newFake(t)
	first, err := conn.FederatedToken(t.Context(), provisioner)
	if err != nil {
		t.Fatal(err)
	}
	f.clock.advance(50 * time.Minute)
	if again, _ := conn.FederatedToken(t.Context(), provisioner); again != first {
		t.Error("a token with ten minutes left was minted afresh")
	}
	other, err := conn.FederatedToken(t.Context(), provisioner, analytics)
	if err != nil || other == first {
		t.Errorf("another scope set shared the cloud-platform token (%v)", err)
	}
	f.clock.advance(6 * time.Minute)
	if renewed, _ := conn.FederatedToken(t.Context(), provisioner); renewed == first {
		t.Error("a token four minutes from its end was handed out")
	}
}

// A SEAT TOKEN NAMES ITS SCOPES AND LIVES AT MOST AN HOUR, and a refusal of
// the mint is Google's status, so an issuer can tell a grant still applying
// from a fault.
func TestASeatTokenNamesItsScopesAndLivesAtMostAnHour(t *testing.T) {
	t.Parallel()
	f, conn := newFake(t)
	for _, life := range []time.Duration{0, time.Hour + time.Second} {
		if _, _, err := conn.SeatToken(t.Context(), seatAccount, []string{analytics}, life); err == nil {
			t.Errorf("a %s token was minted", life)
		}
	}
	if _, _, err := conn.SeatToken(t.Context(), seatAccount, nil, time.Hour); err == nil {
		t.Error("a token was minted with no scope")
	}
	f.edit(func(f *fakeGoogle) { f.seatTokenCreator = false })
	_, _, err := conn.SeatToken(t.Context(), seatAccount, []string{analytics}, time.Hour)
	if googleauth.Status(err) != http.StatusForbidden || googleauth.IsStage(err) {
		t.Errorf("a mint the provisioner may not make answered %v, want Google's 403 for the seat", err)
	}
}

// A REFUSAL NEVER CARRIES A TOKEN ON: Google quoting an access token or the
// federation token in its answer reaches no error.
//
// Mutation: drop the redaction in refusalOf, and the token is in the error.
func TestARefusalNeverCarriesATokenOn(t *testing.T) {
	t.Parallel()
	f, conn := newFake(t)
	const quoted = "ya29.a0AfB_byC1example-token-value-0123456789abcdef"
	f.edit(func(f *fakeGoogle) { f.echo = quoted })
	err := conn.Do(t.Context(), http.MethodGet, "https://analyticsdata.googleapis.com/v1beta/x", "seat-1", nil, nil)
	if googleauth.Status(err) != http.StatusForbidden || strings.Contains(err.Error(), "a0AfB_byC1") {
		t.Fatalf("the refusal %q is not Google's 403 without the token", err)
	}
	other, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f.edit(func(f *fakeGoogle) { f.pub = &other.PublicKey })
	_, err = conn.FederatedToken(t.Context(), "crewlet-atlas@acme-crewlet-id.iam.gserviceaccount.com")
	if !errors.Is(googleauth.Reject(err), integration.ErrCredentialRejected) || strings.Contains(err.Error(), "eyJ") {
		t.Fatalf("an untrusted federation token answered %q, want the credential refused without the JWT", err)
	}
	if st, ok := errors.AsType[*googleauth.StageError](err); !ok || st.Stage != googleauth.StageFederation {
		t.Errorf("the refusal %v does not name the federation stage", err)
	}
}

// A READ WAITS OUT A SHORT RATE LIMIT AS GOOGLE ASKS AND A WRITE NEVER DOES.
func TestAReadWaitsOutARateLimitAndAWriteDoesNot(t *testing.T) {
	t.Parallel()
	f, conn := newFake(t)
	token, _, err := conn.SeatToken(t.Context(), seatAccount, []string{analytics}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	f.edit(func(f *fakeGoogle) { f.limited = 2 })
	if err := conn.Do(t.Context(), http.MethodGet, "https://analyticsdata.googleapis.com/v1beta/x", token, nil, nil); err != nil {
		t.Fatalf("a read limited twice answered %v", err)
	}
	if !slices.Equal(f.clock.waits(), []time.Duration{3 * time.Second, 3 * time.Second}) {
		t.Errorf("the read waited %v, want the two waits Google named", f.clock.waits())
	}
	f.edit(func(f *fakeGoogle) { f.limited = 1 })
	f.clock.resetWaits()
	err = conn.Do(t.Context(), http.MethodPost, "https://analyticsdata.googleapis.com/v1beta/x", token, map[string]any{}, nil)
	if googleauth.Status(err) != http.StatusTooManyRequests || len(f.clock.waits()) != 0 {
		t.Errorf("a limited write answered %v after waits %v, want its 429 at once", err, f.clock.waits())
	}
}
