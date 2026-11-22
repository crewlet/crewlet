package appstoreconnect_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/appstoreconnect"
)

// fakeApple answers as App Store Connect would, recording each token's claims.
type fakeApple struct {
	mu      sync.Mutex
	claims  []map[string]any
	status  int
	header  http.Header
	body    string
	retries int
	calls   int
}

func (f *fakeApple) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	parts := strings.Split(tok, ".")
	if len(parts) == 3 {
		raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
		var c map[string]any
		_ = json.Unmarshal(raw, &c)
		f.claims = append(f.claims, c)
	}
	for k, v := range f.header {
		w.Header()[k] = v
	}
	if f.retries > 0 {
		f.retries--
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	if f.status != 0 {
		w.WriteHeader(f.status)
	}
	_, _ = w.Write([]byte(f.body))
}

func testClient(t *testing.T, f *fakeApple) *appstoreconnect.Client {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	priv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	return appstoreconnect.NewClient(srv.URL, "57246542-96fe-1a63-e053-0824d011072a",
		map[string]appstoreconnect.Key{"marketing": {ID: "2X9R4HXF34", Key: priv}}, srv.Client())
}

// Every request carries a token scoped to it alone (limit, cursor and sort
// aside), for the team's issuer and Apple's audience, living ten minutes.
//
// Mutation: sign an unscoped token, or scope it with the cursor, and a token
// that leaks repeats any call, or a list's next page is refused.
func TestEveryRequestCarriesATokenScopedToIt(t *testing.T) {
	t.Parallel()
	f := &fakeApple{body: `{"data": []}`}
	c := testClient(t, f)
	q := url.Values{"filter[rating]": {"5"}, "limit": {"200"}, "cursor": {"abc"}}
	if _, err := c.Do(context.Background(), "marketing", appstoreconnect.Request{Method: "GET",
		Path: "/v1/apps/123/customerReviews", Query: q, Read: true}); err != nil {
		t.Fatal(err)
	}
	claims := f.claims[0]
	scope, _ := claims["scope"].([]any)
	if len(scope) != 1 || scope[0] != "GET /v1/apps/123/customerReviews?filter[rating]=5" ||
		claims["aud"] != "appstoreconnect-v1" || claims["iss"] != "57246542-96fe-1a63-e053-0824d011072a" {
		t.Errorf("the token's claims are %v", claims)
	}
	if exp, iat := claims["exp"].(float64), claims["iat"].(float64); exp-iat != 600 {
		t.Errorf("the token lives %v seconds, want 600", exp-iat)
	}
	if _, err := c.Do(context.Background(), "finance", appstoreconnect.Request{Method: "GET", Path: "/v1/apps",
		Read: true}); !errors.Is(err, appstoreconnect.ErrNoKey) {
		t.Errorf("a call on a key not connected answered %v", err)
	}
}

// A read waits out Apple's 429 and is sent again; a write never is.
//
// Mutation: retry writes, and an approved reply is posted twice.
func TestAReadIsRetriedAfterARateLimitAndAWriteNever(t *testing.T) {
	t.Parallel()
	f := &fakeApple{retries: 1, body: `{"data": {}}`}
	c := testClient(t, f)
	if _, err := c.Do(context.Background(), "marketing", appstoreconnect.Request{Method: "GET", Path: "/v1/apps",
		Read: true}); err != nil || f.calls != 2 {
		t.Fatalf("a read answered %v after %d calls", err, f.calls)
	}
	f.retries, f.calls = 1, 0
	_, err := c.Do(context.Background(), "marketing", appstoreconnect.Request{Method: "POST",
		Path: "/v1/customerReviewResponses", Body: map[string]any{"data": map[string]any{}}})
	var apiErr *appstoreconnect.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusTooManyRequests || f.calls != 1 {
		t.Errorf("a write answered %v after %d calls", err, f.calls)
	}
}

// Agents' reads stop at a key's reserve, a tenth of its hourly limit, while an
// approved act and the pass still run; Apple's errors are read and bounded.
//
// Mutation: drop the reserve, and agents' reads spend the calls an approved
// reply needs.
func TestReadsStopAtTheKeysReserve(t *testing.T) {
	t.Parallel()
	f := &fakeApple{header: http.Header{"X-Rate-Limit": {"user-hour-lim:3500;user-hour-rem:350;"}}, body: `{}`}
	c := testClient(t, f)
	ctx := context.Background()
	if _, err := c.Do(ctx, "marketing", appstoreconnect.Request{Method: "GET", Path: "/v1/apps", Read: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Do(ctx, "marketing", appstoreconnect.Request{Method: "GET", Path: "/v1/apps", Read: true}); !errors.Is(err, appstoreconnect.ErrReserve) {
		t.Errorf("a read at the reserve answered %v", err)
	}
	if _, err := c.Do(ctx, "marketing", appstoreconnect.Request{Method: "GET", Path: "/v1/apps", Read: true,
		Reserved: true}); err != nil {
		t.Errorf("the pass's read at the reserve answered %v", err)
	}
	f.status = http.StatusForbidden
	f.body = `{"errors": [{"code": "FORBIDDEN_ERROR", "title": "The API key in use does not allow this request",
		"detail": "` + strings.Repeat("x", 5000) + `"}]}`
	_, err := c.Do(ctx, "marketing", appstoreconnect.Request{Method: "GET", Path: "/v1/users", Read: true, Reserved: true})
	var apiErr *appstoreconnect.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "FORBIDDEN_ERROR" || len(apiErr.Detail) > 700 {
		t.Errorf("Apple's refusal read as %v", err)
	}
}
