package googleads

import (
	"cmp"
	"context"
	"encoding/json"
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
	// noZone answers no customer row; amount answers every daily amount so.
	noZone bool
	amount any
}

func (f *fakeAccount) search(_ context.Context, _ string, gaql string) ([]map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case strings.Contains(gaql, "FROM customer") && f.noZone:
		return nil, nil
	case strings.Contains(gaql, "FROM customer"):
		return []map[string]any{{"customer": map[string]any{"timeZone": "Europe/Berlin"}}}, nil
	case strings.Contains(gaql, "FROM campaign_budget"):
		var rows []map[string]any
		for name, b := range f.budgets {
			if strings.Contains(gaql, "'"+name+"'") {
				rows = append(rows, map[string]any{"campaignBudget": map[string]any{"resourceName": name,
					"amountMicros": fmt.Sprint(b[0]), "totalAmountMicros": fmt.Sprint(b[1])}})
			}
		}
		return rows, nil
	}
	var rows []map[string]any
	for name, c := range f.campaigns {
		b := f.budgets[c[1]]
		rows = append(rows, map[string]any{
			"campaign": map[string]any{"resourceName": name, "status": c[0], "campaignBudget": c[1],
				"endDateTime": c[2]},
			"campaignBudget": map[string]any{"resourceName": c[1],
				"amountMicros": cmp.Or(f.amount, any(fmt.Sprint(b[0]))), "totalAmountMicros": fmt.Sprint(b[1])},
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
	a, err := a.applying(ops)
	if err != nil {
		panic(err)
	}
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
	enabled, _ := a.applying([]operation{{service: "campaign", kind: opUpdate, name: "c",
		object: map[string]any{"status": "ENABLED"}, mask: []string{"status"}}})
	if got := enabled.daily() - a.daily(); got != 1000 {
		t.Errorf("enabling the paused campaign adds %d, want its 1000", got)
	}
	// AN ENUM BY ITS NUMBER (2 is ENABLED) counts as serving: Google takes it.
	//
	// Mutation: read the status by its name alone, and enabling by number
	// adds nothing the caps hold.
	byNumber, _ := a.applying([]operation{{service: "campaign", kind: opUpdate, name: "c",
		object: map[string]any{"status": float64(2)}, mask: []string{"status"}}})
	if got := byNumber.daily() - a.daily(); got != 1000 {
		t.Errorf("enabling the paused campaign by its status's number adds %d, want its 1000", got)
	}
}

// EVERY BUDGET THE OPERATIONS NAME IS COUNTED, not only those a campaign
// uses: an enabled campaign made on a budget no campaign uses yet adds that
// budget, and one moved onto it adds the difference.
//
// Mutation: read only the budgets campaigns use, and both add nothing or
// less, so a budget made at once and filled later passes every cap.
func TestEveryBudgetTheOperationsNameIsCounted(t *testing.T) {
	t.Parallel()
	orphan := "customers/1234567890/campaignBudgets/77"
	f := &fakeAccount{campaigns: map[string][3]string{campaign1: {"ENABLED", budgetA, ""}},
		budgets: map[string][2]int64{budgetA: {10_000_000, 0}, orphan: {1_000_000_000, 0}}, spent: map[string]int64{}}
	c := capacity{search: f.search, now: time.Now}
	for name, tc := range map[string]struct {
		op   operation
		want int64
	}{
		"an enabled campaign made on it": {operation{service: "campaign", kind: opCreate,
			object: map[string]any{"status": "ENABLED", "campaign_budget": orphan}}, 1_000_000_000},
		"an enabled campaign moved onto it": {operation{service: "campaign", kind: opUpdate, name: campaign1,
			object: map[string]any{"campaign_budget": orphan}, mask: []string{"campaign_budget"}}, 990_000_000},
	} {
		if got, err := c.Delta(t.Context(), "1234567890", []operation{tc.op}); err != nil || got != tc.want {
			t.Errorf("%s adds %d (%v), want %d", name, got, err, tc.want)
		}
	}
}

// AN AMOUNT IS READ AS THE RULES READ IT, OR REFUSED: a whole number as a
// tool's arguments carry it (a json.Number), a float, or a string padded as
// Google reads it adds what it says; an amount that is no whole number of
// micros, or one cleared, fails the delta, and nothing the caps hold is 0.
//
// Mutation: read json.Number as no amount, or a failed read as unchanged,
// and a raise of 900,000 a day adds nothing the caps refuse.
func TestAnAmountIsReadAsTheRulesReadItOrRefused(t *testing.T) {
	t.Parallel()
	f := &fakeAccount{campaigns: map[string][3]string{campaign1: {"ENABLED", budgetA, ""}},
		budgets: map[string][2]int64{budgetA: {10_000_000, 0}}, spent: map[string]int64{}}
	c := capacity{search: f.search, now: time.Now}
	raise := func(v any, set bool) []operation {
		o := operation{service: "campaign_budget", kind: opUpdate, name: budgetA, object: map[string]any{},
			mask: []string{"amount_micros"}}
		if set {
			o.object["amount_micros"] = v
		}
		return []operation{o}
	}
	for _, v := range []any{json.Number("900000000000"), 900000000000.0, "900000000000", " 900000000000 "} {
		if got, err := c.Delta(t.Context(), "1234567890", raise(v, true)); err != nil || got != 899_990_000_000 {
			t.Errorf("a budget raised to %#v adds %d (%v), want 899990000000", v, got, err)
		}
	}
	for _, v := range []any{"lots", json.Number("9.5"), 9.5, true} {
		if got, err := c.Delta(t.Context(), "1234567890", raise(v, true)); err == nil {
			t.Errorf("a budget set to %#v adds %d, want it refused", v, got)
		}
	}
	if got, err := c.Delta(t.Context(), "1234567890", raise(nil, false)); err == nil {
		t.Errorf("a budget's amount cleared adds %d, want it refused", got)
	}
}

// A FIELD IS WHAT THE MASK SAYS OF IT: an ended campaign's end cleared by its
// mask serves again, and a budget an update's object names outside its mask
// is not the one the campaign runs on, as Google ignores it.
//
// Mutation: read an update's object whatever its mask, and clearing the end
// adds nothing while a budget the mask leaves out lowers the day.
func TestAFieldIsWhatTheMaskSaysOfIt(t *testing.T) {
	t.Parallel()
	small := "customers/1234567890/campaignBudgets/78"
	f := &fakeAccount{campaigns: map[string][3]string{campaign1: {"ENABLED", budgetA, "2020-01-01 00:00:00"}},
		budgets: map[string][2]int64{budgetA: {10_000_000, 0}, small: {1, 0}}, spent: map[string]int64{}}
	c := capacity{search: f.search, now: time.Now}
	cleared := operation{service: "campaign", kind: opUpdate, name: campaign1, object: map[string]any{},
		mask: []string{"end_date_time"}}
	if got, err := c.Delta(t.Context(), "1234567890", []operation{cleared}); err != nil || got != 10_000_000 {
		t.Errorf("an ended campaign's end cleared adds %d (%v), want its 10000000", got, err)
	}
	unmasked := operation{service: "campaign", kind: opUpdate, name: campaign1,
		object: map[string]any{"campaign_budget": small}, mask: []string{"end_date_time"}}
	if got, err := c.Delta(t.Context(), "1234567890", []operation{unmasked}); err != nil || got != 10_000_000 {
		t.Errorf("an end cleared beside a budget outside the mask adds %d (%v), want 10000000", got, err)
	}
}

// WHAT GOOGLE ANSWERS UNREADABLY IS NO FIGURE: an account answering no
// customer has no day to count a total budget's days in, and a budget whose
// amount does not read is not 0; both fail the read.
//
// Mutation: fall back to UTC, or read an unreadable amount as 0, and the
// caps compare a figure Google never gave.
func TestWhatGoogleAnswersUnreadablyIsNoFigure(t *testing.T) {
	t.Parallel()
	for name, f := range map[string]*fakeAccount{
		"no customer":          {noZone: true},
		"an unreadable budget": {amount: "lots"},
	} {
		f.campaigns = map[string][3]string{campaign1: {"ENABLED", budgetA, ""}}
		f.budgets, f.spent = map[string][2]int64{budgetA: {10_000_000, 0}}, map[string]int64{}
		if got, err := (capacity{search: f.search, now: time.Now}).Daily(t.Context(), "1234567890"); err == nil {
			t.Errorf("an account answering %s has a day's capacity of %d", name, got)
		}
	}
}
