package googleads

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/spendguard"
	"github.com/crewlet/crewlet/internal/spendguard/spendguardtest"
)

// fakeAccount is one account's campaigns and budgets, answering the two
// searches capacity makes.
type fakeAccount struct {
	mu        sync.Mutex
	campaigns map[string][3]string // status, budget, end
	budgets   map[string][2]int64  // daily, total
	spent     map[string]int64
	made      int
}

func (f *fakeAccount) search(_ context.Context, _ string, gaql string) ([]map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.Contains(gaql, "FROM customer") {
		return []map[string]any{{"customer": map[string]any{"timeZone": "Europe/Berlin"}}}, nil
	}
	var rows []map[string]any
	for name, c := range f.campaigns {
		b := f.budgets[c[1]]
		rows = append(rows, map[string]any{
			"campaign": map[string]any{"resourceName": name, "status": c[0], "campaignBudget": c[1],
				"endDateTime": c[2]},
			"campaignBudget": map[string]any{"resourceName": c[1], "amountMicros": fmt.Sprint(b[0]),
				"totalAmountMicros": fmt.Sprint(b[1])},
			"metrics": map[string]any{"costMicros": fmt.Sprint(f.spent[name])},
		})
	}
	return rows, nil
}

// apply makes ops as Google would: budgets and campaigns made or changed.
func (f *fakeAccount) apply(ops []operation) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := account{campaigns: map[string]campaign{}, budgets: map[string]budget{}}
	for k, v := range f.campaigns {
		a.campaigns[k] = campaign{status: v[0], budget: v[1]}
	}
	for k, v := range f.budgets {
		a.budgets[k] = budget{daily: v[0], total: v[1]}
	}
	a = a.applying(ops)
	for k, v := range a.campaigns {
		f.campaigns[k] = [3]string{v.status, v.budget, f.campaigns[k][2]}
	}
	for k, v := range a.budgets {
		f.budgets[k] = [2]int64{v.daily, v.total}
	}
}

// THE SPEND GUARD'S CONTRACT HOLDS FOR GOOGLE ADS (spendguardtest): an
// account's daily capacity is read live and an operation's delta is what it
// adds once applied, so two raises read on one capacity cannot both pass.
func TestGoogleAdsCapacityKeepsTheSpendGuardsContract(t *testing.T) {
	t.Parallel()
	accounts := map[string]*fakeAccount{}
	var mu sync.Mutex
	spendguardtest.Run(t, spendguardtest.Harness[[]operation]{
		Kind: string(Kind),
		New: func(t *testing.T, daily int64) (spendguard.Capacity[[]operation], spendguard.Account) {
			f := &fakeAccount{campaigns: map[string][3]string{"customers/1/campaigns/1": {"ENABLED", "customers/1/campaignBudgets/1", ""}},
				budgets: map[string][2]int64{"customers/1/campaignBudgets/1": {daily, 0}}, spent: map[string]int64{}}
			mu.Lock()
			id := fmt.Sprintf("%010d", len(accounts)+1)
			accounts[id] = f
			mu.Unlock()
			return capacity{search: f.search, now: time.Now}, spendguard.Account{ID: id, Currency: "EUR", Serving: true}
		},
		Raise: func(t *testing.T, acct string, delta int64) []operation {
			mu.Lock()
			f := accounts[acct]
			mu.Unlock()
			f.mu.Lock()
			f.made++
			n := f.made
			f.mu.Unlock()
			b := fmt.Sprintf("customers/1/campaignBudgets/-%d", n)
			return []operation{
				{service: "campaign_budget", kind: opCreate, object: map[string]any{"resource_name": b,
					"amount_micros": fmt.Sprint(delta)}},
				{service: "campaign", kind: opCreate, object: map[string]any{"resource_name": fmt.Sprintf("customers/1/campaigns/-%d", n),
					"status": "ENABLED", "campaign_budget": b}},
			}
		},
		Apply: func(_ context.Context, acct string, ops []operation) error {
			mu.Lock()
			f := accounts[acct]
			mu.Unlock()
			f.apply(ops)
			return nil
		},
	})
}

// A DAY'S CAPACITY COUNTS EVERY ENABLED, UNENDED CAMPAIGN'S BUDGET ONCE: a
// shared budget once, a paused or ended campaign not at all, and a total
// budget as what is left of it over its remaining days, rounded up.
func TestADaysCapacityCountsEachBudgetOnce(t *testing.T) {
	t.Parallel()
	today := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	a := account{today: today,
		budgets: map[string]budget{"shared": {daily: 10}, "paused": {daily: 1000}, "total": {total: 100}, "ended": {daily: 7}},
		campaigns: map[string]campaign{
			"a": {status: "ENABLED", budget: "shared"}, "b": {status: "ENABLED", budget: "shared"},
			"c": {status: "PAUSED", budget: "paused"},
			"d": {status: "ENABLED", budget: "total", end: today.AddDate(0, 0, 2), spent: 70},
			"e": {status: "ENABLED", budget: "ended", end: today.AddDate(0, 0, -1)},
		}}
	// shared 10 once, total (100-70)/3 days = 10, paused and ended nothing.
	if got := a.daily(); got != 20 {
		t.Errorf("the day's capacity is %d, want 20", got)
	}
	enabled := a.applying([]operation{{service: "campaign", kind: opUpdate, name: "c",
		object: map[string]any{"status": "ENABLED"}, mask: []string{"status"}}})
	if got := enabled.daily() - a.daily(); got != 1000 {
		t.Errorf("enabling the paused campaign adds %d, want its 1000", got)
	}
	// AN ENUM BY ITS NUMBER (2 is ENABLED) counts as serving: Google takes it.
	//
	// Mutation: read the status by its name alone, and enabling by number
	// adds nothing the caps hold.
	byNumber := a.applying([]operation{{service: "campaign", kind: opUpdate, name: "c",
		object: map[string]any{"status": float64(2)}, mask: []string{"status"}}})
	if got := byNumber.daily() - a.daily(); got != 1000 {
		t.Errorf("enabling the paused campaign by its status's number adds %d, want its 1000", got)
	}
}
