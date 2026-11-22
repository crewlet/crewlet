package googlegrowth_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
)

// renewingGrant hands out one token, and another once Google refuses it.
type renewingGrant struct {
	mu      sync.Mutex
	renewed int
}

func (g *renewingGrant) Token(context.Context) (string, error) { return "ya29.old", nil }

func (g *renewingGrant) Renewed(_ context.Context, refused string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.renewed++
	return "ya29.new-after-" + refused, nil
}

// A CONSENT PRODUCT SENDS AS THE COMPANY'S GRANT, RENEWED ONCE: with no
// Google identity configured at all, a call goes with the grant's token, a
// 401 renews it once and the call is made again, and the access log names the
// consent connection, never Google Cloud's.
//
// Mutation: drop the retry, or send with a seat token, and the call fails.
func TestAConsentProductSendsAsTheGrantRenewedOnce(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var bearers []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		bearers = append(bearers, bearer)
		w.Header().Set("Content-Type", "application/json")
		if bearer == "ya29.old" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":401,"status":"UNAUTHENTICATED","message":"expired"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"name":"accounts/1/items/2"}`))
	}))
	t.Cleanup(srv.Close)
	grant := &renewingGrant{}
	c := &googlegrowth.Client{Kind: "x", Name: "X", Grant: grant,
		Company: func() *config.Company { return &config.Company{Name: "Acme"} },
		Env:     func() *config.Resolver { return nil }, HTTP: &http.Client{Transport: httpxtest.Rewrite(t, srv)}}
	get := growth.Operation{ID: "x.items.get", Method: "GET", Resource: "items", Class: growth.ClassRead,
		Path: "https://x.googleapis.com/v1/accounts/{accountsId}/items/{itemsId}"}
	req, err := googlegrowth.NewRequest(get, map[string]string{"accountsId": "1", "itemsId": "2"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Send(context.Background(), "finance", req); err != nil {
		t.Fatalf("a call after one refused token failed: %v", err)
	}
	if grant.renewed != 1 || len(bearers) != 2 || bearers[1] != "ya29.new-after-ya29.old" {
		t.Errorf("renewed %d times, Google saw %v; want one renewal and the new token second", grant.renewed, bearers)
	}
	if r := c.Record(growth.Call{Handle: "finance", Tool: "x_read"}, req, growth.ClassRead); r.Connection != googlegrowth.GrantConnection {
		t.Errorf("the access log names connection %q, want %q", r.Connection, googlegrowth.GrantConnection)
	}
}
