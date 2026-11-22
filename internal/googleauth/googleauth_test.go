package googleauth_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"net"
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
	// A READ TAKEN IN A POST BODY (a report) changes nothing, so it waits as a GET does.
	f.edit(func(f *fakeGoogle) { f.limited = 1 })
	f.clock.resetWaits()
	err = conn.DoRead(t.Context(), http.MethodPost, "https://analyticsdata.googleapis.com/v1beta/x", token, map[string]any{}, nil)
	if err != nil || !slices.Equal(f.clock.waits(), []time.Duration{3 * time.Second}) {
		t.Errorf("a limited report answered %v after waits %v, want one wait and its answer", err, f.clock.waits())
	}
}

// A CALLER'S OWN MISTAKE IS NOT HELD AS GOOGLE'S REFUSAL: a token asked for
// with no scope is answered at once, Google is asked nothing, and the next
// proper ask is minted rather than handed a held refusal.
//
// Mutation: let the issuer mint before checking its inputs, and the mistake
// is held for the restart schedule's wait as if Google had refused.
func TestACallersMistakeIsNeverHeldAsAGoogleRefusal(t *testing.T) {
	t.Parallel()
	f, conn := newFake(t)
	issuer := &googleauth.SeatIssuer{Now: f.clock.now, RetryWait: func(int) time.Duration { return time.Hour }}
	if _, err := issuer.Token(t.Context(), conn, seatAccount, nil, time.Minute); err == nil {
		t.Fatal("a token with no scope was issued")
	}
	if got := f.mintsFor(seatAccount); len(got) != 0 {
		t.Fatalf("Google was asked %d times for a token with no scope", len(got))
	}
	if _, err := issuer.Token(t.Context(), conn, seatAccount, []string{analytics}, time.Minute); err != nil {
		t.Errorf("a proper ask after the mistake answered %v, want a token", err)
	}
}

// AN API'S OWN HEADER RIDES THE CALL AND NEVER STANDS IN FOR THE ENGINE'S: a
// manager's login-customer-id reaches Google, and an Authorization header a
// product sets is dropped for the bearer the engine minted.
//
// Mutation: set the product's headers after the engine's, and the forged
// bearer is sent.
func TestAnAPIsHeaderRidesTheCallButNeverTheBearer(t *testing.T) {
	t.Parallel()
	f, conn := newFake(t)
	token, _, err := conn.SeatToken(t.Context(), seatAccount, []string{analytics}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	err = conn.Send(t.Context(), googleauth.Call{Method: http.MethodGet,
		Address: "https://analyticsdata.googleapis.com/v1beta/x", Bearer: token, Reads: true,
		Header: http.Header{"Login-Customer-Id": {"9876543210"}, "Authorization": {"Bearer forged"}}}, nil)
	if err != nil {
		t.Fatalf("a call carrying an API's header answered %v", err)
	}
	// A CALL WITH NO BEARER sends no Authorization, whatever the API's headers say.
	_ = conn.Send(t.Context(), googleauth.Call{Method: http.MethodGet,
		Address: "https://analyticsdata.googleapis.com/v1beta/x", Reads: true,
		Header: http.Header{"Authorization": {"Bearer forged"}}}, nil)
	f.mu.Lock()
	defer f.mu.Unlock()
	seen := f.seen[len(f.seen)-2]
	if seen.Get("Login-Customer-Id") != "9876543210" || seen.Get("Authorization") != "Bearer "+token {
		t.Errorf("Google read login-customer-id %q and authorization %q", seen.Get("Login-Customer-Id"),
			seen.Get("Authorization"))
	}
	if bare := f.seen[len(f.seen)-1]; bare.Get("Authorization") != "" {
		t.Errorf("a call with no bearer sent authorization %q", bare.Get("Authorization"))
	}
}

// roundTrip is a transport answering every request with what it says.
type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// A REQUEST THAT FAILED NAMES ITS PATH ALONE: a credential in the address's
// query (a Measurement Protocol secret, which no redaction knows by name)
// reaches no error, whether the request was never built or never answered,
// and the failure's cause is kept, so a deadline is still told.
//
// Mutation: format the transport's error whole, or drop its cause, and the
// secret is in the error or the deadline is lost.
func TestAFailedRequestNeverQuotesItsQuery(t *testing.T) {
	t.Parallel()
	const secret = "Zx8kQ2pL4mN7vB1cR5tY9w"
	const address = "https://www.google-analytics.com/mp/collect?measurement_id=G-1&api_secret=" + secret
	reset := &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")}
	conn, _ := connectionOver(t, roundTrip(func(*http.Request) (*http.Response, error) { return nil, reset }))
	err := conn.Do(t.Context(), http.MethodPost, address, "", map[string]any{"client_id": "1.2"}, nil)
	if err == nil || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "api_secret") ||
		!strings.Contains(err.Error(), "www.google-analytics.com/mp/collect") ||
		!errors.Is(err, googleauth.ErrUnreached) || !errors.Is(err, reset) {
		t.Errorf("a reset request answered %q, want its path alone and the reset kept", err)
	}
	expired, cancel := context.WithDeadline(t.Context(), time.Unix(0, 0))
	defer cancel()
	waits := roundTrip(func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	conn, _ = connectionOver(t, waits)
	err = conn.Do(expired, http.MethodPost, address, "", map[string]any{}, nil)
	if err == nil || strings.Contains(err.Error(), secret) || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a request past its deadline answered %q, want the deadline told without the secret", err)
	}
	err = conn.Do(t.Context(), http.MethodGet, address+"\x7f", "", nil, nil)
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Errorf("a request that could not be built answered %q, which quotes the secret", err)
	}
}

// A FILE RIDES AS ITSELF AND A DOWNLOAD IS STREAMED: a Bytes body reaches
// Google with its own content type and nothing re-encoded, and an answer read
// into a writer arrives there whole, never decoded.
//
// Mutation: JSON-encode every body, and the upload's content type is
// application/json.
func TestAFileRidesAsItselfAndADownloadIsStreamed(t *testing.T) {
	t.Parallel()
	f, conn := newFake(t)
	token, _, err := conn.SeatToken(t.Context(), seatAccount, []string{analytics}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	err = conn.Send(t.Context(), googleauth.Call{Method: http.MethodPost,
		Address: "https://analyticsdata.googleapis.com/v1beta/x?uploadType=media", Bearer: token,
		Body: googleauth.Bytes{ContentType: "image/png", Data: []byte("\x89PNG")}}, &got)
	if err != nil {
		t.Fatalf("an upload answered %v", err)
	}
	f.mu.Lock()
	seen := f.seen[len(f.seen)-1]
	f.mu.Unlock()
	if seen.Get("Content-Type") != "image/png" {
		t.Errorf("Google read content type %q", seen.Get("Content-Type"))
	}
	if !strings.Contains(got.String(), `"rowCount":1`) {
		t.Errorf("the streamed answer is %q", got.String())
	}
}
