package googlecloud_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/googlecloud"
	"github.com/crewlet/crewlet/internal/integration"
)

// clientWorld is a client reaching the fake as the provisioner, on the fake's
// clock.
type clientWorld struct {
	f     *fakeGoogle
	c     *googlecloud.Client
	clock *testClock
}

func newClientWorld(t *testing.T) *clientWorld {
	t.Helper()
	key, pemText := fakeKey(t)
	clock := newTestClock()
	f := newFakeGoogle(t, &key.PublicKey, clock)
	c, err := googlecloud.NewClient(googlecloud.ClientOptions{
		Company: testCompany, Provisioner: provisioner, Provider: testProvider, FederationKey: pemText,
		HTTP: f.server(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	googlecloud.SetClock(c, clock.now, clock.sleep)
	return &clientWorld{f: f, c: c, clock: clock}
}

// THE ENGINE REACHES THE PROVISIONER WITH ITS OWN KEY: the fake verifies the
// signed token against the key's public half as the provider does, one
// exchange serves every call for the hour, and the token is renewed five
// minutes before it ends. Every call goes to Google alone.
func TestTheProvisionerIsReachedByFederationAndRenewedBeforeItEnds(t *testing.T) {
	t.Parallel()
	w := newClientWorld(t)
	for range 3 {
		if _, found, err := w.c.ServiceAccount(t.Context(), provisioner); err != nil || !found {
			t.Fatalf("read as the provisioner: %v, %v", found, err)
		}
	}
	if w.f.exchange != 1 {
		t.Fatalf("%d exchanges for three calls in the hour, want 1", w.f.exchange)
	}
	w.clock.advance(54 * time.Minute)
	if _, _, err := w.c.ServiceAccount(t.Context(), provisioner); err != nil || w.f.exchange != 1 {
		t.Fatalf("six minutes before the end: %d exchanges, %v; want the cached token", w.f.exchange, err)
	}
	w.clock.advance(2 * time.Minute)
	if _, _, err := w.c.ServiceAccount(t.Context(), provisioner); err != nil || w.f.exchange != 2 {
		t.Fatalf("four minutes before the end: %d exchanges, %v; want a renewal", w.f.exchange, err)
	}
	w.f.onlyGoogle(t)
}

// A TOKEN GOOGLE STOPPED ACCEPTING IS MINTED AGAIN ONCE, never retried with.
func TestARefusedProvisionerTokenIsMintedAgainOnce(t *testing.T) {
	t.Parallel()
	w := newClientWorld(t)
	if _, _, err := w.c.ServiceAccount(t.Context(), provisioner); err != nil {
		t.Fatal(err)
	}
	w.f.edit(func(f *fakeGoogle) { f.tokens = map[string]fakeToken{} })
	if _, _, err := w.c.ServiceAccount(t.Context(), provisioner); err != nil {
		t.Fatalf("after Google dropped the token: %v", err)
	}
	if w.f.exchange != 2 {
		t.Fatalf("%d exchanges, want a second after the refusal", w.f.exchange)
	}
}

// A KEY THE PROVIDER DOES NOT HOLD IS THE PROVISIONER'S CREDENTIAL REFUSED, at
// federation, and Google's echo of the signed token never reaches the error.
func TestAnUntrustedKeyIsRefusedAtFederationWithoutTheToken(t *testing.T) {
	t.Parallel()
	w := newClientWorld(t)
	other, _ := fakeKey(t)
	w.f.edit(func(f *fakeGoogle) { f.pub = &other.PublicKey })
	_, _, err := w.c.ServiceAccount(t.Context(), provisioner)
	if err == nil {
		t.Fatal("a key the provider does not hold was accepted")
	}
	if !strings.Contains(err.Error(), "federation") || strings.Contains(err.Error(), "eyJ") {
		t.Fatalf("error %q does not name federation, or carries the token", err)
	}
	if googlecloud.Status(err) != http.StatusBadRequest {
		t.Errorf("status = %d, want STS's 400", googlecloud.Status(err))
	}
}

// AN AGENT'S TOKEN IS MINTED ONLY THROUGH THE PROVISIONER'S GRANT ON THAT
// ACCOUNT, for at most an hour.
func TestASeatTokenNeedsTheProvisionersGrantOnTheAccount(t *testing.T) {
	t.Parallel()
	w := newClientWorld(t)
	sa, err := w.c.CreateServiceAccount(t.Context(), "crewlet-sre-1a2b", "Crewlet Acme sre", "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.c.SeatToken(t.Context(), sa.UniqueID, time.Hour); googlecloud.Status(err) != http.StatusForbidden {
		t.Fatalf("a mint before the grant: %v, want 403", err)
	}
	if err := w.c.EditAccountPolicy(t.Context(), sa.Email, func(p *googlecloud.Policy) bool {
		return googlecloud.Grant(p, "serviceAccount:"+provisioner, []string{"roles/iam.serviceAccountTokenCreator"})
	}); err != nil {
		t.Fatal(err)
	}
	value, expires, err := w.c.SeatToken(t.Context(), sa.UniqueID, time.Hour)
	if err != nil || value == "" || !expires.Equal(w.clock.now().Add(time.Hour)) {
		t.Fatalf("seat token held %v, expires %s, %v", value != "", expires, err)
	}
	for _, life := range []time.Duration{0, time.Hour + time.Second} {
		if _, _, err := w.c.SeatToken(t.Context(), sa.UniqueID, life); err == nil {
			t.Errorf("a %s token was minted", life)
		}
	}
}

// A PROJECT WRITE IS VERSION 3 WITH THE ETAG, keeps every other binding and
// the provisioner's own conditions, re-reads a policy written in between, and
// writes nothing where nothing changes.
func TestAProjectWriteKeepsVersionThreeAndEveryOtherBinding(t *testing.T) {
	t.Parallel()
	w := newClientWorld(t)
	before := w.f.policy(projectWeb)
	w.f.edit(func(f *fakeGoogle) { f.conflicts[projectWeb] = 1 })
	seat := "serviceAccount:crewlet-sre-1a2b@" + identity + ".iam.gserviceaccount.com"
	list := googlecloud.Lists(projectWeb)[0]
	edit := func(p *googlecloud.Policy) bool { return googlecloud.Grant(p, seat, list) }
	if err := w.c.EditProjectPolicy(t.Context(), projectWeb, edit); err != nil {
		t.Fatal(err)
	}
	after := w.f.policy(projectWeb)
	for _, b := range before.Bindings {
		if !slices.ContainsFunc(after.Bindings, func(a googlecloud.Binding) bool {
			return a.Role == b.Role && slices.Equal(a.Members, b.Members) && (a.Condition == nil) == (b.Condition == nil)
		}) {
			t.Errorf("the write lost %+v", b)
		}
	}
	if plain, _ := googlecloud.HeldBy(after, seat); !slices.Equal(plain, slices.Sorted(slices.Values(list))) {
		t.Errorf("the seat holds %v, want %v", plain, list)
	}
	for _, v := range w.f.versions {
		if v != 3 {
			t.Errorf("a project write carried version %d", v)
		}
	}
	writes := len(w.f.wrote())
	if err := w.c.EditProjectPolicy(t.Context(), projectWeb, edit); err != nil || len(w.f.wrote()) != writes {
		t.Errorf("an edit that changes nothing wrote: %v", err)
	}
}

// A LISTING READS EVERY PAGE.
func TestAListingReadsEveryPage(t *testing.T) {
	t.Parallel()
	w := newClientWorld(t)
	w.f.edit(func(f *fakeGoogle) { f.page = 2 })
	projects, err := w.c.SearchProjects(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 5 {
		t.Fatalf("read %d projects across pages, want 5", len(projects))
	}
}

// THE FEDERATION KEY'S PUBLIC HALF IS ALL ITS JWKS HOLDS, and the kid every
// signed token names is the key's thumbprint in it.
func TestTheJWKSHoldsThePublicHalfAlone(t *testing.T) {
	t.Parallel()
	key, pemText := fakeKey(t)
	jwks, err := googlecloud.JWKS(pemText)
	if err != nil {
		t.Fatal(err)
	}
	var set struct {
		Keys []map[string]string `json:"keys"`
	}
	if err := json.Unmarshal([]byte(jwks), &set); err != nil || len(set.Keys) != 1 {
		t.Fatalf("jwks %q: %v", jwks, err)
	}
	jwk := set.Keys[0]
	if _, private := jwk["d"]; private || strings.Contains(jwks, "PRIVATE") {
		t.Fatal("the JWKS carries the private half")
	}
	x, _ := base64.RawURLEncoding.DecodeString(jwk["x"])
	if jwk["alg"] != "ES256" || jwk["kty"] != "EC" || !slices.Equal(key.PublicKey.X.FillBytes(make([]byte, 32)), x) {
		t.Fatalf("jwk %v is not the key's public half", jwk)
	}
	want, _ := googlecloud.PublicJWK(&key.PublicKey)
	if jwk["kid"] != want.Kid || want.Kid == "" {
		t.Errorf("kid %q, want the thumbprint %q", jwk["kid"], want.Kid)
	}
}

// ONLY A PKCS#8 P-256 KEY IS A FEDERATION KEY, and no refusal quotes it.
func TestOnlyAnES256KeyIsAFederationKey(t *testing.T) {
	t.Parallel()
	p384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	encode := func(k any) string {
		der, err := x509.MarshalPKCS8PrivateKey(k)
		if err != nil {
			t.Fatal(err)
		}
		return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	}
	for name, text := range map[string]string{
		"P-384": encode(p384), "RSA": encode(rsaKey), "not a PEM": "secret-not-a-key",
		"a certificate block": "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n",
	} {
		_, err := googlecloud.ParseFederationKey(text)
		if err == nil {
			t.Errorf("%s was taken as a federation key", name)
			continue
		}
		if strings.Contains(err.Error(), "secret-not-a-key") {
			t.Errorf("%s: the refusal quotes the value", name)
		}
	}
}

// THE ISSUER, THE SUBJECT AND THE PRINCIPAL ARE THE COMPANY'S NAME ALONE, so
// the Cloud Shell block, the provider and the engine agree.
func TestTheFederationIdentityIsAPureFunctionOfTheCompany(t *testing.T) {
	t.Parallel()
	if got := googlecloud.Issuer(" Acme Corp "); got != "https://crewlet.invalid/acme-corp" {
		t.Errorf("issuer %q", got)
	}
	if got := googlecloud.Subject("Acme Corp"); got != "crewlet:acme-corp" {
		t.Errorf("subject %q", got)
	}
	want := "principal://iam.googleapis.com/projects/123456789012/locations/global/workloadIdentityPools/crewlet/" +
		"subject/crewlet:acme"
	if got := googlecloud.Principal(testProvider, "Acme"); got != want {
		t.Errorf("principal %q, want %q", got, want)
	}
	if got := googlecloud.Audience(testProvider); got != "//iam.googleapis.com/"+testProvider {
		t.Errorf("audience %q", got)
	}
}

// A CLIENT THAT WOULD FAIL EVERY CALL IS REFUSED, naming the field.
func TestAClientThatWouldFailEveryCallIsRefused(t *testing.T) {
	t.Parallel()
	_, pemText := fakeKey(t)
	ok := googlecloud.ClientOptions{Company: testCompany, Provisioner: provisioner, Provider: testProvider,
		FederationKey: pemText}
	for _, tc := range []struct {
		name  string
		edit  func(*googlecloud.ClientOptions)
		field string
	}{
		{"a person's email", func(o *googlecloud.ClientOptions) { o.Provisioner = "ops@acme.example" }, "provisioner"},
		{"a pool as the provider", func(o *googlecloud.ClientOptions) {
			o.Provider = "projects/1/locations/global/workloadIdentityPools/crewlet"
		}, "provider"},
		{"no key", func(o *googlecloud.ClientOptions) { o.FederationKey = "" }, "federation_key"},
		{"a nameless company", func(o *googlecloud.ClientOptions) { o.Company = "!!" }, "company"},
	} {
		opts := ok
		tc.edit(&opts)
		if _, err := googlecloud.NewClient(opts); err == nil || !strings.Contains(err.Error(), tc.field) {
			t.Errorf("%s: %v, want a refusal naming %s", tc.name, err, tc.field)
		}
	}
	if _, err := googlecloud.NewClient(ok); err != nil {
		t.Fatalf("a whole set of options was refused: %v", err)
	}
}

// EVERY READ-BACK WAITS OUT A STALE ANSWER WITHIN ONE BOUND, whichever write it
// confirms: a stale-then-fresh read passes, a read that never settles is
// Google still settling past the bound with the seat's reason, and a final
// error ends it at once.
func TestEveryReadBackWaitsOutAStaleAnswer(t *testing.T) {
	t.Parallel()
	for _, what := range googlecloud.ReadBacks() {
		clock := newTestClock()
		reads := 0
		took, err := googlecloud.AwaitObserved(t.Context(), clock.now, clock.sleep, what, func() (bool, error) {
			reads++
			return reads == 3, nil
		})
		if err != nil || reads != 3 || took != 6*time.Second {
			t.Fatalf("%s: a stale-then-fresh read: %v after %d reads and %s, want 2s then 4s", what, err, reads, took)
		}
		start := clock.now()
		_, err = googlecloud.AwaitObserved(t.Context(), clock.now, clock.sleep, what, func() (bool, error) {
			return false, errors.New("still the old state")
		})
		reason, settling := googlecloud.SettlingReason(err)
		if !settling || clock.now().Sub(start) > googlecloud.ObserveBudget {
			t.Fatalf("%s: a read that never settles answered %v after %s", what, err, clock.now().Sub(start))
		}
		wantReason := googlecloud.ReasonApplying
		if strings.Contains(what, "new account") || strings.Contains(what, "Token Creator") {
			wantReason = googlecloud.ReasonCreating
		}
		if reason != wantReason || !strings.Contains(err.Error(), "still the old state") {
			t.Errorf("%s: reason %q in %q, want %q", what, reason, err, wantReason)
		}
		final := &googlecloud.APIError{Status: http.StatusForbidden, Message: "Policy update access denied."}
		reads = 0
		_, err = googlecloud.AwaitObserved(t.Context(), clock.now, clock.sleep, what, func() (bool, error) {
			reads++
			return true, final
		})
		if !errors.Is(err, final) || reads != 1 {
			t.Errorf("%s: a final error was waited on: %v after %d reads", what, err, reads)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	clock := newTestClock()
	_, err := googlecloud.AwaitObserved(ctx, clock.now, clock.sleep, googlecloud.ReadBacks()[0],
		func() (bool, error) { return false, nil })
	if !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled wait answered %v", err)
	}
}

// A NEW ACCOUNT'S ABSENCE IS WAITED OUT FOR THAT ACCOUNT ALONE: any other
// refusal of the same write is final.
func TestOnlyTheNewAccountsOwnNotYetIsRetried(t *testing.T) {
	t.Parallel()
	email := "crewlet-sre-2baf@" + identity + ".iam.gserviceaccount.com"
	notYet := &googlecloud.APIError{Status: http.StatusBadRequest, Code: "INVALID_ARGUMENT",
		Message: "Service account " + email + " does not exist."}
	if !googlecloud.AccountNotYet(notYet, email) {
		t.Fatal("Google's live answer for a new account was not recognised")
	}
	if googlecloud.AccountNotYet(notYet, "crewlet-qa-0000@"+identity+".iam.gserviceaccount.com") {
		t.Fatal("another account's absence was waited on")
	}
	denied := &googlecloud.APIError{Status: http.StatusForbidden, Code: "PERMISSION_DENIED",
		Message: "Policy update access denied."}
	if googlecloud.AccountNotYet(denied, email) || googlecloud.AccountNotYet(nil, email) {
		t.Fatal("a refusal or a success was waited on")
	}
}

// A POLICY EDIT TOUCHES ITS MEMBER ALONE: other members and every condition
// stay, and a binding left empty goes.
func TestPolicyEditsKeepOtherMembersAndConditions(t *testing.T) {
	t.Parallel()
	cond := &googlecloud.Condition{Expression: "x"}
	pol := googlecloud.Policy{Bindings: []googlecloud.Binding{
		{Role: "roles/browser", Members: []string{"user:a@acme.example"}},
		{Role: "roles/resourcemanager.projectIamAdmin", Members: []string{"serviceAccount:" + provisioner}, Condition: cond},
		{Role: "roles/dns.reader", Members: []string{"user:b@acme.example"}, Condition: cond},
	}}
	seat := "serviceAccount:s@acme.example"
	if !googlecloud.Grant(&pol, seat, []string{"roles/browser", "roles/run.viewer"}) {
		t.Fatal("a grant reported no change")
	}
	if googlecloud.Grant(&pol, seat, []string{"roles/browser"}) {
		t.Error("a grant of what is held reported a change")
	}
	if plain, _ := googlecloud.HeldBy(pol, seat); !slices.Equal(plain, []string{"roles/browser", "roles/run.viewer"}) {
		t.Fatalf("held %v", plain)
	}
	if !googlecloud.Revoke(&pol, seat, []string{"roles/browser", "roles/run.viewer"}) {
		t.Fatal("a revoke reported no change")
	}
	if plain, cond := googlecloud.HeldBy(pol, seat); len(plain)+len(cond) != 0 || len(pol.Bindings) != 3 ||
		pol.Bindings[1].Condition == nil || !slices.Equal(pol.Bindings[0].Members, []string{"user:a@acme.example"}) {
		t.Fatalf("the revoke touched other bindings: %+v", pol.Bindings)
	}
}

// A LEVEL'S CONDITION IS READ ONLY IN ITS ONE SHAPE: a hasOnly over constants
// and nothing joined to it, since anything more could grant anything.
func TestALevelConditionIsReadOnlyInItsOneShape(t *testing.T) {
	t.Parallel()
	canonical := condition(googlecloud.Lists(projectWeb)[1]).Expression
	got, ok := googlecloud.ConditionRoles(canonical)
	if want := slices.Sorted(slices.Values(googlecloud.Lists(projectWeb)[1])); !ok || !slices.Equal(got, want) {
		t.Fatalf("the canonical condition read as %v, %v", got, ok)
	}
	if got, ok := googlecloud.ConditionRoles(`api.getAttribute("iam.googleapis.com/modifiedGrantsByRole", [])` +
		`.hasOnly(["roles/browser"])`); ok {
		t.Errorf("a double-quoted attribute name read as %v", got)
	}
	if got, ok := googlecloud.ConditionRoles(`api.getAttribute('iam.googleapis.com/modifiedGrantsByRole', [])` +
		`.hasOnly(["roles/browser", 'roles/dns.reader'])`); !ok || len(got) != 2 {
		t.Errorf("either quote around a role was refused: %v, %v", got, ok)
	}
	for _, expr := range []string{
		canonical + " || true",
		"true || " + canonical,
		canonical + " && request.time < timestamp('2030-01-01T00:00:00Z')",
		"api.getAttribute('iam.googleapis.com/modifiedGrantsByRole', []).hasOnly([])",
		"api.getAttribute('iam.googleapis.com/modifiedGrantsByRole', []).hasOnly([roles])",
		"resource.name.startsWith('projects/acme-web')",
	} {
		if got, ok := googlecloud.ConditionRoles(expr); ok {
			t.Errorf("%q read as the list %v", expr, got)
		}
	}
}

// A REFUSED PROVISIONER IS THE CREDENTIAL'S REFUSAL, which the loop reports as
// a person's to fix rather than a fault to retry.
func TestARefusedProvisionerIsTheCredentialsRefusal(t *testing.T) {
	t.Parallel()
	w := newClientWorld(t)
	w.f.edit(func(f *fakeGoogle) {
		f.refuse["POST /v1/projects/-/serviceAccounts/"+provisioner+":generateAccessToken"] = http.StatusForbidden
	})
	_, _, err := w.c.ServiceAccount(t.Context(), provisioner)
	if err == nil || !strings.Contains(err.Error(), "impersonation") {
		t.Fatalf("a refused impersonation answered %v", err)
	}
	if !errors.Is(googlecloud.Reject(err), integration.ErrCredentialRejected) {
		t.Errorf("%v is not read as the credential refused", err)
	}
}
