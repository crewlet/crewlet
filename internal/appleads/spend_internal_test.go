package appleads

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/spendguard"
	"github.com/crewlet/crewlet/internal/spendguard/spendguardtest"
)

// budgets is a fake Apple holding one ad account's campaigns.
type budgets struct {
	mu        sync.Mutex
	campaigns []map[string]any
}

func (b *budgets) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/token":
		_, _ = io.WriteString(w, `{"access_token": "tok", "expires_in": 3600}`)
	case r.URL.Path == "/v1/acls":
		_, _ = io.WriteString(w, `{"result": {"acls": [{"adAccount": {"id": 111, "orgId": 9}, "roles": ["Admin"]}]}}`)
	case r.URL.Path == "/v1/campaigns/query":
		_ = json.NewEncoder(w).Encode(map[string]any{"result": b.campaigns})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/campaigns":
		var c map[string]any
		_ = json.Unmarshal(body, &c)
		c["id"] = strconv.Itoa(len(b.campaigns) + 1)
		if c["status"] == nil {
			c["status"] = statusEnabled
		}
		b.campaigns = append(b.campaigns, c)
		_ = json.NewEncoder(w).Encode(map[string]any{"result": c})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func budget(amount int64, status string) map[string]any {
	s, _ := growth.FormatAmount(amount, 6)
	return map[string]any{"adAccountId": 111, "status": status,
		"dailyBudget": map[string]any{"value": map[string]any{"amount": s, "currency": "USD"}}}
}

// THE CONTRACT EVERY CAPACITY RUNS: Apple Ads' capacity reads an account's
// enabled campaigns' daily budgets, a new enabled campaign adds its own, and
// the guard holds both to the caps, once per account at the apply.
//
// Mutation: count a paused campaign, or read a new campaign as adding
// nothing, and the suite's raises pass caps they must not.
func TestTheCapacityKeepsTheSpendContract(t *testing.T) {
	t.Parallel()
	priv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalECPrivateKey(priv)
	key := string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}))
	// THE SUITE'S CASES RUN ONE AFTER ANOTHER, each raising on the account its
	// New made.
	var current *Product
	spendguardtest.Run(t, spendguardtest.Harness[spendAct]{
		Kind: Kind,
		New: func(t *testing.T, daily int64) (spendguard.Capacity[spendAct], spendguard.Account) {
			fake := &budgets{campaigns: []map[string]any{budget(daily, statusEnabled), budget(500_000_000, statusPaused)}}
			srv := httptest.NewServer(fake)
			t.Cleanup(srv.Close)
			c := &config.Company{Integrations: config.Integrations{AppleAds: &config.AppleAds{Enabled: true,
				Connections: map[string]config.AppleAdsConnection{"main": {ClientID: "SEARCHADS.c", TeamID: "SEARCHADS.t",
					KeyID: "k", PrivateKey: "${K}"}}}}}
			p := New(Deps{Company: func() *config.Company { return c }, HTTP: srv.Client(), Base: srv.URL,
				TokenURL: srv.URL + "/token", Now: func() time.Time { return time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC) },
				Resolve: func(v string) string {
					if v == "${K}" {
						return key
					}
					return v
				}})
			current = p
			return capacity{p: p}, spendguard.Account{ID: "111", Currency: "USD", Serving: true, State: "ACTIVE"}
		},
		Raise: func(t *testing.T, account string, delta int64) spendAct {
			op, _ := Catalog().Operation("campaignsPost")
			b := budget(delta, statusEnabled)
			delete(b, "status")
			conns, err := current.connsNow()
			if err != nil {
				t.Fatal(err)
			}
			return spendAct{m: made{account: account, conn: conns[0], targets: []target{{op: op, path: op.Path, body: b}}},
				befores: []before{{}}}
		},
		Apply: func(ctx context.Context, account string, act spendAct) error {
			t := act.m.targets[0]
			_, err := act.m.conn.client.Do(ctx, Request{Method: t.op.Method, Path: t.path, Body: t.body, Account: account})
			return err
		},
	})
}
