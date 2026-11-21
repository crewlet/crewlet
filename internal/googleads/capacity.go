package googleads

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/crewlet/crewlet/internal/spendguard"
)

// The spend guard's figures for Google Ads (internal/spendguard): an
// account's daily capacity is the daily budget of every enabled, unended
// campaign, a shared budget once, and a total budget as what is left of it
// over its remaining days, rounded up; an operation's delta is that figure
// once the request's operations are applied to what the account holds now.

// budget is one campaign budget as capacity reads it.
type budget struct {
	daily, total int64
}

// campaign is one campaign as capacity reads it.
type campaign struct {
	status, budget string
	end            time.Time
	spent          int64
}

// account is what an account holds that spends: its campaigns and budgets,
// and the day in its own zone.
type account struct {
	campaigns map[string]campaign
	budgets   map[string]budget
	today     time.Time
}

// daily is the account's daily capacity in micros.
func (a account) daily() int64 {
	seen := map[string]bool{}
	var total int64
	for _, c := range a.campaigns {
		if c.status != "ENABLED" || (!c.end.IsZero() && c.end.Before(a.today)) || seen[c.budget] {
			continue
		}
		seen[c.budget] = true
		b := a.budgets[c.budget]
		if b.total > 0 {
			total += remainderPerDay(b.total-c.spent, c.end, a.today)
			continue
		}
		total += b.daily
	}
	return total
}

// remainderPerDay is what is left of a total budget over the days left to its
// end, rounded up; a total with no end date counts whole today.
func remainderPerDay(left int64, end, today time.Time) int64 {
	if left <= 0 {
		return 0
	}
	if end.IsZero() {
		return left
	}
	days := int64(end.Sub(today).Hours()/24) + 1
	if days < 1 {
		days = 1
	}
	return (left + days - 1) / days
}

// applying is a after ops: budgets changed or made, campaigns made, enabled,
// paused, ended, moved to another budget or removed.
func (a account) applying(ops []operation) account {
	out := account{today: a.today, campaigns: map[string]campaign{}, budgets: map[string]budget{}}
	for k, v := range a.campaigns {
		out.campaigns[k] = v
	}
	for k, v := range a.budgets {
		out.budgets[k] = v
	}
	for _, o := range ops {
		switch o.service {
		case "campaign_budget":
			name := pick(o.kind == opCreate, str(o.object["resource_name"]), o.name)
			b := out.budgets[name]
			if v, ok := micros(o.object["amount_micros"]); ok {
				b.daily = v
			}
			if v, ok := micros(o.object["total_amount_micros"]); ok {
				b.total = v
			}
			if o.kind == opRemove {
				delete(out.budgets, name)
				continue
			}
			out.budgets[name] = b
		case "campaign":
			name := pick(o.kind == opCreate, str(o.object["resource_name"]), o.name)
			if o.kind == opRemove {
				delete(out.campaigns, name)
				continue
			}
			c := out.campaigns[name]
			if o.kind == opCreate {
				c.status = "ENABLED"
			}
			if s := str(o.object["status"]); s != "" {
				c.status = s
			}
			if b := str(o.object["campaign_budget"]); b != "" {
				c.budget = b
			}
			if _, set := o.object["end_date_time"]; set || o.kind == opCreate {
				c.end, _ = when(o.object["end_date_time"])
			}
			out.campaigns[name] = c
		}
	}
	return out
}

// micros reads an int64 amount JSON carries as a string or a number.
func micros(v any) (int64, bool) {
	switch t := v.(type) {
	case string:
		n, err := strconv.ParseInt(t, 10, 64)
		return n, err == nil
	case float64:
		return int64(t), true
	}
	return 0, false
}

// capacity is the spend guard's view of Google Ads accounts, read as one
// seat's identity through search.
type capacity struct {
	search func(ctx context.Context, customer, gaql string) ([]map[string]any, error)
	now    func() time.Time
}

var _ spendguard.Capacity[[]operation] = capacity{}

// Daily is customer's daily capacity now.
func (c capacity) Daily(ctx context.Context, customer string) (int64, error) {
	a, err := c.read(ctx, customer)
	if err != nil {
		return 0, err
	}
	return a.daily(), nil
}

// Delta is how much ops add to customer's daily capacity.
func (c capacity) Delta(ctx context.Context, customer string, ops []operation) (int64, error) {
	a, err := c.read(ctx, customer)
	if err != nil {
		return 0, err
	}
	return a.applying(ops).daily() - a.daily(), nil
}

// read is what customer holds that spends, live.
func (c capacity) read(ctx context.Context, customer string) (account, error) {
	zone, err := c.zone(ctx, customer)
	if err != nil {
		return account{}, err
	}
	rows, err := c.search(ctx, customer, "SELECT campaign.resource_name, campaign.status, campaign.end_date_time, "+
		"campaign.campaign_budget, campaign_budget.resource_name, campaign_budget.amount_micros, "+
		"campaign_budget.total_amount_micros, metrics.cost_micros FROM campaign WHERE campaign.status != 'REMOVED'")
	if err != nil {
		return account{}, fmt.Errorf("googleads: read %s's campaigns: %w", customer, err)
	}
	now := c.now().In(zone)
	a := account{campaigns: map[string]campaign{}, budgets: map[string]budget{},
		today: time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)}
	for _, row := range rows {
		cm, _ := snaked(row["campaign"]).(map[string]any)
		bm, _ := snaked(row["campaignBudget"]).(map[string]any)
		mm, _ := snaked(row["metrics"]).(map[string]any)
		end, _ := when(cm["end_date_time"])
		spent, _ := micros(mm["cost_micros"])
		a.campaigns[str(cm["resource_name"])] = campaign{status: str(cm["status"]), budget: str(cm["campaign_budget"]),
			end: end, spent: spent}
		daily, _ := micros(bm["amount_micros"])
		total, _ := micros(bm["total_amount_micros"])
		a.budgets[str(bm["resource_name"])] = budget{daily: daily, total: total}
	}
	return a, nil
}

// zone is customer's own time zone, the day a budget is spent in.
func (c capacity) zone(ctx context.Context, customer string) (*time.Location, error) {
	rows, err := c.search(ctx, customer, "SELECT customer.time_zone FROM customer")
	if err != nil {
		return nil, fmt.Errorf("googleads: read %s's time zone: %w", customer, err)
	}
	if len(rows) == 0 {
		return time.UTC, nil
	}
	m, _ := snaked(rows[0]["customer"]).(map[string]any)
	loc, err := time.LoadLocation(str(m["time_zone"]))
	if err != nil {
		return nil, fmt.Errorf("googleads: %s's time zone %q is not one this build loads: %w", customer,
			str(m["time_zone"]), err)
	}
	return loc, nil
}
