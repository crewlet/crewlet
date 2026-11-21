package googleauth_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/googleauth"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
)

// The fake's fixed identities. Obviously fake values throughout.
const (
	testCompany  = "Acme"
	provisioner  = "crewlet-provisioner@acme-crewlet-id.iam.gserviceaccount.com"
	testProvider = "projects/123456789012/locations/global/workloadIdentityPools/crewlet/providers/crewlet-engine"
	seatAccount  = "112233445566778899001"
	analytics    = "https://www.googleapis.com/auth/analytics.readonly"
	adwords      = "https://www.googleapis.com/auth/adwords"
)

// testClock is a world's time, which a wait moves forward rather than spends.
type testClock struct {
	mu     sync.Mutex
	t      time.Time
	waited []time.Duration
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// waits is every wait the clock was asked for, in order.
func (c *testClock) waits() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.waited)
}

func (c *testClock) resetWaits() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.waited = nil
}

func (c *testClock) sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.waited = append(c.waited, d)
	c.mu.Unlock()
	c.advance(d)
	return nil
}

// mint is one generateAccessToken the fake answered: who asked, for which
// account and with which scopes.
type mint struct {
	bearer, account string
	scopes          []string
}

// fakeGoogle is STS, the IAM Credentials API and one product endpoint, each
// enforcing what Google does: the exchange's signature and scope, and who may
// impersonate whom.
type fakeGoogle struct {
	mu    sync.Mutex
	pub   *ecdsa.PublicKey
	clock *testClock

	// exchanges is each exchange's form, as STS read it.
	exchanges []map[string][]string
	mints     []mint
	issued    int
	// seatTokenCreator is whether the provisioner may mint for seatAccount.
	seatTokenCreator bool
	// limited is how many 429s the product endpoint answers first.
	limited int
	// echo is what the product endpoint quotes in a 403.
	echo string
	// seen is the headers of each product call, as Google read them.
	seen []http.Header
}

func newFake(t *testing.T) (*fakeGoogle, *googleauth.Connection) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	clk := &testClock{t: time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)}
	f := &fakeGoogle{pub: &key.PublicKey, clock: clk, seatTokenCreator: true}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	conn, err := googleauth.New(googleauth.Options{
		Company: testCompany, Provisioner: provisioner, Provider: testProvider,
		FederationKey: string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		HTTP:          &http.Client{Transport: httpxtest.Rewrite(t, srv)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return f, conn.WithClock(googleauth.Clock{Now: clk.now, Sleep: clk.sleep})
}

func (f *fakeGoogle) edit(change func(*fakeGoogle)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(f)
}

func (f *fakeGoogle) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// THE REWRITER SENDS EVERY HOST HERE, so each API is told by its path.
	switch {
	case r.URL.Path == "/v1/token":
		f.exchange(w, r)
	case strings.HasSuffix(r.URL.Path, ":generateAccessToken"):
		f.impersonate(w, r)
	case r.URL.Path == "/v1beta/x":
		f.product(w, r)
	default:
		refuse(w, http.StatusNotFound, "NOT_FOUND", "no such path "+r.URL.Path)
	}
}

func refuse(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
		"code": status, "status": code, "message": message,
	}})
}

func answer(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

// exchange is STS: a JWT the provider's key signed, for the provider, at
// cloud-platform.
func (f *fakeGoogle) exchange(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		refuse(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	f.exchanges = append(f.exchanges, r.PostForm)
	if !f.verified(r.PostForm.Get("subject_token")) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant",
			"error_description": "token " + r.PostForm.Get("subject_token") + " is not trusted"})
		return
	}
	f.issued++
	answer(w, map[string]any{"access_token": fmt.Sprintf("federated-%d", f.issued)})
}

// verified reports a federation token the provider's key signed, naming the
// company's subject and the provider's audience.
func (f *fakeGoogle) verified(jwt string) bool {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(sig) != 64 {
		return false
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(f.pub, digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		return false
	}
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims struct {
		Sub string `json:"sub"`
		Aud string `json:"aud"`
	}
	return json.Unmarshal(raw, &claims) == nil && claims.Sub == googleauth.Subject(testCompany) &&
		claims.Aud == "https:"+googleauth.Audience(testProvider)
}

// impersonate is generateAccessToken: the provisioner by the federated
// token, and the seat's account by the provisioner's token where it may.
func (f *fakeGoogle) impersonate(w http.ResponseWriter, r *http.Request) {
	account := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/projects/-/serviceAccounts/"),
		":generateAccessToken")
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	var body struct {
		Scope    []string `json:"scope"`
		Lifetime string   `json:"lifetime"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		refuse(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
		return
	}
	f.mints = append(f.mints, mint{bearer: bearer, account: account, scopes: body.Scope})
	switch {
	case account == provisioner && strings.HasPrefix(bearer, "federated-"):
	case account == seatAccount && strings.HasPrefix(bearer, "provisioner-") && f.seatTokenCreator:
	default:
		refuse(w, http.StatusForbidden, "PERMISSION_DENIED", "Permission 'iam.serviceAccounts.getAccessToken' denied")
		return
	}
	f.issued++
	prefix := "seat-"
	if account == provisioner {
		prefix = "provisioner-"
	}
	answer(w, map[string]any{
		"accessToken": fmt.Sprintf("%s%d:%s", prefix, f.issued, strings.Join(body.Scope, ",")),
		"expireTime":  f.clock.now().Add(time.Hour).Format(time.RFC3339),
	})
}

// product is one product API: it serves a token carrying the analytics scope,
// answers its first limited calls 429, and quotes echo in a refusal.
func (f *fakeGoogle) product(w http.ResponseWriter, r *http.Request) {
	f.seen = append(f.seen, r.Header.Clone())
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	switch {
	case f.limited > 0:
		f.limited--
		w.Header().Set("Retry-After", "3")
		refuse(w, http.StatusTooManyRequests, "RESOURCE_EXHAUSTED", "slow down")
	case f.echo != "":
		refuse(w, http.StatusForbidden, "PERMISSION_DENIED", "refused "+f.echo)
	case !strings.Contains(bearer, analytics):
		refuse(w, http.StatusForbidden, "PERMISSION_DENIED", "Request had insufficient authentication scopes.")
	default:
		answer(w, map[string]any{"rowCount": 1})
	}
}

// mintsFor is the scopes of every mint the fake answered for account.
func (f *fakeGoogle) mintsFor(account string) [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][]string
	for _, m := range f.mints {
		if m.account == account {
			out = append(out, slices.Clone(m.scopes))
		}
	}
	return out
}
