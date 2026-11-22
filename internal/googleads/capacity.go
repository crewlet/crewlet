package googleads

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/growth"
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
		if !serving(c.status) || (!c.end.IsZero() && c.end.Before(a.today)) || seen[c.budget] {
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

// serving reports whether a campaign of status serves: every status but
// PAUSED and REMOVED by name, since Google also takes a status's number.
func serving(status string) bool { return status != "PAUSED" && status != "REMOVED" }

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
// paused, ended, moved to another budget or removed. An update's field its
// mask names and its object leaves out is cleared, as Google clears it. An
// amount that is no whole number of micros as the rules read it ([micros]),
// or one cleared, is refused: what Google makes of it is not the engine's to
// guess, and a figure capacity cannot read would add nothing to it.
func (a account) applying(ops []operation) (account, error) {
	out := account{today: a.today, campaigns: maps.Clone(a.campaigns), budgets: maps.Clone(a.budgets)}
	for _, o := range ops {
		name := pick(o.kind == opCreate, str(o.object["resource_name"]), o.name)
		switch o.service {
		case "campaign_budget":
			if o.kind == opRemove {
				delete(out.budgets, name)
				continue
			}
			b := out.budgets[name]
			for field, into := range map[string]*int64{"amount_micros": &b.daily, "total_amount_micros": &b.total} {
				v, set := o.sets(field)
				if !set {
					continue
				}
				n, ok := micros(v)
				if !ok {
					return account{}, refusal(growth.RefusedInvalid, fmt.Sprintf("%s's %s is %s, which is no "+
						"whole number of micros: give it as one, such as \"25000000\". No vendor was reached.",
						cmp.Or(name, "a new budget"), field, googlegrowth.Words(v)))
				}
				*into = n
			}
			out.budgets[name] = b
		case "campaign":
			if o.kind == opRemove {
				delete(out.campaigns, name)
				continue
			}
			c := out.campaigns[name]
			if o.kind == opCreate {
				c.status = "ENABLED"
			}
			if v, set := o.sets("status"); set {
				c.status = str(v) // a number, or cleared, names no status and serves
			}
			if v, set := o.sets("campaign_budget"); set && str(v) != "" {
				c.budget = str(v)
			}
			if v, set := o.sets("end_date_time"); set || o.kind == opCreate {
				c.end, _ = when(v) // cleared, or unreadable, runs for ever
			}
			out.campaigns[name] = c
		}
	}
	return out, nil
}

// sets is what o sets field to and whether it sets it: a create's object, or
// an update's mask, where a field the object leaves out is cleared (nil).
func (o operation) sets(field string) (any, bool) {
	if o.kind == opCreate {
		v, ok := o.object[field]
		return v, ok
	}
	return lookup(o.object, field), slices.Contains(o.mask, field)
}

// micros reads an amount in micros as the rules read it ([number]): a JSON
// number, a json.Number (every tool transport decodes with UseNumber) or a
// decimal string, whole and in range; false for anything else.
func micros(v any) (int64, bool) {
	n, ok := number(v)
	if !ok || !n.IsInt() || !n.Num().IsInt64() {
		return 0, false
	}
	return n.Num().Int64(), true
}

// liveMicros is an amount Google answered: absent is none, and one that does
// not read is an error, since a figure read as 0 would let a change pass the
// daily cap.
func liveMicros(customer, field string, v any) (int64, error) {
	if v == nil {
		return 0, nil
	}
	n, ok := micros(v)
	if !ok {
		return 0, fmt.Errorf("googleads: %s answered %s as %s, which is no amount in micros", customer, field,
			googlegrowth.Words(v))
	}
	return n, nil
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
	a, err := c.read(ctx, customer, nil)
	if err != nil {
		return 0, err
	}
	return a.daily(), nil
}

// Delta is how much ops add to customer's daily capacity: every budget they
// name is read, so a budget no campaign uses yet counts its amount once one
// is moved to it.
func (c capacity) Delta(ctx context.Context, customer string, ops []operation) (int64, error) {
	a, err := c.read(ctx, customer, budgetsNamed(customer, ops))
	if err != nil {
		return 0, err
	}
	after, err := a.applying(ops)
	if err != nil {
		return 0, err
	}
	return after.daily() - a.daily(), nil
}

// budgetsNamed are the budgets of customer that ops change or move a campaign
// to, those they make aside.
func budgetsNamed(customer string, ops []operation) []string {
	var out, made []string
	for _, o := range ops {
		switch {
		case o.service == "campaign_budget" && o.kind == opCreate:
			made = append(made, str(o.object["resource_name"]))
		case o.service == "campaign_budget":
			out = append(out, o.name)
		case o.service == "campaign":
			out = append(out, str(o.object["campaign_budget"]))
		}
	}
	return slices.DeleteFunc(slices.Compact(slices.Sorted(slices.Values(out))), func(name string) bool {
		owner, kind, ok := kindOf(name)
		return !ok || owner != customer || kind != "campaign_budget" || slices.Contains(made, name)
	})
}

// read is what customer holds that spends, live, with the budgets named
// beside those its campaigns use.
func (c capacity) read(ctx context.Context, customer string, named []string) (account, error) {
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
		mm, _ := snaked(row["metrics"]).(map[string]any)
		end, _ := when(cm["end_date_time"])
		spent, rerr := liveMicros(customer, "metrics.cost_micros", mm["cost_micros"])
		if rerr != nil {
			return account{}, rerr
		}
		a.campaigns[str(cm["resource_name"])] = campaign{status: str(cm["status"]), budget: str(cm["campaign_budget"]),
			end: end, spent: spent}
		if rerr = a.readBudget(customer, row); rerr != nil {
			return account{}, rerr
		}
	}
	named = slices.DeleteFunc(slices.Clone(named), func(n string) bool { _, seen := a.budgets[n]; return seen })
	if len(named) == 0 {
		return a, nil
	}
	quoted := make([]string, len(named))
	for i, n := range named {
		quoted[i] = "'" + n + "'" // kindOf admits IDs alone, which hold no quote
	}
	rows, err = c.search(ctx, customer, "SELECT campaign_budget.resource_name, campaign_budget.amount_micros, "+
		"campaign_budget.total_amount_micros FROM campaign_budget WHERE campaign_budget.resource_name IN ("+
		strings.Join(quoted, ", ")+")")
	if err != nil {
		return account{}, fmt.Errorf("googleads: read %s's budgets: %w", customer, err)
	}
	for _, row := range rows {
		if err = a.readBudget(customer, row); err != nil {
			return account{}, err
		}
	}
	return a, nil
}

// readBudget keeps the campaign budget a search row holds.
func (a account) readBudget(customer string, row map[string]any) error {
	bm, _ := snaked(row["campaignBudget"]).(map[string]any)
	daily, err := liveMicros(customer, "campaign_budget.amount_micros", bm["amount_micros"])
	if err != nil {
		return err
	}
	total, err := liveMicros(customer, "campaign_budget.total_amount_micros", bm["total_amount_micros"])
	if err != nil {
		return err
	}
	a.budgets[str(bm["resource_name"])] = budget{daily: daily, total: total}
	return nil
}

// zone is customer's own time zone, the day a budget is spent in.
func (c capacity) zone(ctx context.Context, customer string) (*time.Location, error) {
	rows, err := c.search(ctx, customer, "SELECT customer.time_zone FROM customer")
	if err != nil {
		return nil, fmt.Errorf("googleads: read %s's time zone: %w", customer, err)
	}
	if len(rows) == 0 {
		// NO ZONE IS NO DAY: a guessed one moves where a total budget's days end.
		return nil, fmt.Errorf("googleads: %s answered no customer, so its day is not known", customer)
	}
	m, _ := snaked(rows[0]["customer"]).(map[string]any)
	loc, err := time.LoadLocation(str(m["time_zone"]))
	if err != nil {
		return nil, fmt.Errorf("googleads: %s's time zone %q is not one this build loads: %w", customer,
			str(m["time_zone"]), err)
	}
	return loc, nil
}
