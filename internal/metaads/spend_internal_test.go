package metaads

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/spendguard"
	"github.com/crewlet/crewlet/internal/spendguard/spendguardtest"
)

// budgets is a fake Graph API holding one ad account's campaigns, in cents.
type budgets struct {
	mu        sync.Mutex
	campaigns []map[string]any
}

func (b *budgets) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	form, _ := url.ParseQuery(string(body))
	w.Header().Set("Content-Type", "application/json")
	path := strings.TrimPrefix(r.URL.Path, "/"+Version)
	switch {
	case path == "/debug_token":
		_, _ = io.WriteString(w, `{"data": {"app_id": "1", "is_valid": true, "scopes": ["ads_read"], "user_id": "42"}}`)
	case path == "/act_111" && r.Method == http.MethodGet:
		_, _ = io.WriteString(w, `{"id": "act_111", "currency": "USD", "timezone_name": "UTC", "account_status": 1}`)
	case path == "/act_111/campaigns" && r.Method == http.MethodGet:
		_ = json.NewEncoder(w).Encode(map[string]any{"data": b.campaigns})
	case path == "/act_111/adsets":
		_, _ = io.WriteString(w, `{"data": []}`)
	case path == "/act_111/campaigns" && r.Method == http.MethodPost:
		b.campaigns = append(b.campaigns, map[string]any{"id": fmt.Sprint(len(b.campaigns) + 1),
			"status": "ACTIVE", "daily_budget": form.Get("daily_budget")})
		_, _ = io.WriteString(w, `{"id": "1"}`)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// cents is micros of USD as Meta writes them, a part of a cent rounded up: a
// raise of 50 units and a micro is 50.01, which is over a 50 cap too.
func cents(micros int64) string { return fmt.Sprint((micros + 9_999) / 10_000) }

// THE CONTRACT EVERY CAPACITY RUNS: Meta Ads' capacity reads an account's
// active campaigns' daily budgets, a paused one adding nothing, a new active
// campaign adds its own, and the guard holds both to the caps, once per
// account at the apply.
//
// Mutation: count a paused campaign, or read a new campaign as adding nothing,
// and the suite's raises pass caps they must not.
func TestTheCapacityKeepsTheSpendContract(t *testing.T) {
	t.Parallel()
	var current *Product
	spendguardtest.Run(t, spendguardtest.Harness[spendAct]{
		Kind: Kind,
		New: func(t *testing.T, daily int64) (spendguard.Capacity[spendAct], spendguard.Account) {
			fake := &budgets{campaigns: []map[string]any{
				{"id": "a", "status": "ACTIVE", "daily_budget": cents(daily)},
				{"id": "b", "status": "PAUSED", "daily_budget": cents(500_000_000)},
			}}
			srv := httptest.NewServer(fake)
			t.Cleanup(srv.Close)
			c := &config.Company{Integrations: config.Integrations{MetaAds: &config.MetaAds{Enabled: true,
				AppID: "1", AppSecret: "${S}", ReadToken: "${R}"}}}
			p := New(Deps{Company: func() *config.Company { return c }, HTTP: srv.Client(), Base: srv.URL,
				Now: func() time.Time { return time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC) },
				Resolve: func(v string) string {
					if r, ok := map[string]string{"${S}": "s", "${R}": "EAAread"}[v]; ok {
						return r
					}
					return v
				}})
			current = p
			return capacity{p: p}, spendguard.Account{ID: "act_111", Currency: "USD", Serving: true, State: "active"}
		},
		Raise: func(t *testing.T, account string, delta int64) spendAct {
			c, err := current.connNow()
			if err != nil {
				t.Fatal(err)
			}
			created := createIn(opCampaignCreate, account, map[string]any{"name": "x", "status": "ACTIVE",
				"daily_budget": cents(delta)})
			return spendAct{m: made{account: account, conn: c, targets: []target{created}}, befores: []before{{}}}
		},
		Apply: func(ctx context.Context, account string, act spendAct) error {
			t := act.m.targets[0]
			params := url.Values{}
			for k, v := range t.params {
				params.Set(k, formValue(v))
			}
			_, err := act.m.conn.client.Do(ctx, "EAAread", Request{Method: t.row.op.Method, Path: t.path,
				Params: params, Object: objectOf(account)})
			return err
		},
	})
}

// Money is read in Meta's own offsets, never ISO 4217's: cents for USD and
// for BHD (three decimals in ISO), whole units for JPY and HUF (two in ISO),
// and a currency Meta's table does not list is refused rather than guessed.
//
// Mutation: read HUF as cents or BHD as thousandths, and a budget reads 100
// or 10 times wrong.
func TestMoneyIsReadInMetasOwnOffsets(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		currency string
		minor    string
		micros   int64
	}{
		{"USD", "5000", 50_000_000}, {"BHD", "5000", 50_000_000}, {"JPY", "5000", 5_000_000_000},
		{"HUF", "5000", 5_000_000_000}, {"TWD", "1", 1_000_000},
	} {
		got, err := microsOf(tc.minor, tc.currency)
		if err != nil || got != tc.micros {
			t.Errorf("%s %s read as %d micros (%v), want %d", tc.minor, tc.currency, got, err, tc.micros)
		}
		back, err := minorOf(tc.micros, tc.currency)
		if err != nil || fmt.Sprint(back) != tc.minor {
			t.Errorf("%d micros of %s sent as %d (%v), want %s", tc.micros, tc.currency, back, err, tc.minor)
		}
	}
	if _, err := microsOf("5000", "KWD"); err == nil {
		t.Error("a currency Meta does not list was read")
	}
	if _, err := microsOf("12.5", "USD"); err == nil {
		t.Error("an amount that is not whole offset units was read")
	}
}

// A lifetime budget counts as its remainder over its remaining days, rounded
// up, and an ended or paused campaign counts nothing, its ad sets' budgets
// included; a campaign holding no budget counts its active ad sets'.
//
// Mutation: count the whole lifetime budget, or an ended campaign, and the
// day's capacity is wrong.
func TestTheDayCountsALifetimeBudgetsRemainder(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	l := ledger{currency: "USD", zone: time.UTC, now: at, campaigns: map[string]*budgeted{
		"life":  {status: "ACTIVE", lifetime: 1_000_000_000, left: 100_000_000, end: at.Add(72 * time.Hour)},
		"ended": {status: "ACTIVE", daily: 500_000_000, end: at.Add(-time.Hour)},
		"held":  {status: "PAUSED", daily: 900_000_000},
		"sets":  {status: "ACTIVE"},
		"off":   {status: "PAUSED"},
	}, adSets: map[string]*budgeted{
		"s1": {campaign: "sets", status: "ACTIVE", daily: 7_000_000},
		"s2": {campaign: "sets", status: "PAUSED", daily: 9_000_000},
		"s3": {campaign: "off", status: "ACTIVE", daily: 40_000_000},
	}}
	// 100 left over 4 days (three days ahead, today counted) is 25, and the
	// active ad set adds 7.
	if got := l.daily(); got != 32_000_000 {
		t.Errorf("the day's capacity is %d micros, want 32 units", got)
	}
}
