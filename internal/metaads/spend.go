package metaads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/spendguard"
)

// An ad account's daily capacity (package doc, "Spend") is read from its
// campaigns and ad sets as they are now; a change's delta is the capacity
// once its requests apply to that reading, less the capacity now, so enabling
// a campaign counts its active ad sets and enabling an ad or raising a bid
// counts nothing (and is still held).

// offsetOne are the currencies Meta writes amounts of in whole units: its
// currencies page gives every other one an offset of 100, in cents.
var offsetOne = map[string]bool{"CLP": true, "COP": true, "CRC": true, "HUF": true, "ISK": true, "IDR": true,
	"JPY": true, "KRW": true, "PYG": true, "TWD": true, "VND": true}

// offsetHundred are the other currencies Meta's currencies page lists.
var offsetHundred = map[string]bool{"DZD": true, "ARS": true, "AUD": true, "BHD": true, "BDT": true, "BOB": true,
	"BGN": true, "BRL": true, "GBP": true, "CAD": true, "CNY": true, "HRK": true, "CZK": true, "DKK": true,
	"EGP": true, "EUR": true, "GTQ": true, "HNL": true, "HKD": true, "INR": true, "ILS": true, "JOD": true,
	"KES": true, "LVL": true, "LTL": true, "MOP": true, "MYR": true, "MXN": true, "NZD": true, "NIO": true,
	"NGN": true, "NOK": true, "PKR": true, "PEN": true, "PHP": true, "PLN": true, "QAR": true, "RON": true,
	"RUB": true, "SAR": true, "RSD": true, "SGD": true, "SKK": true, "ZAR": true, "SEK": true, "CHF": true,
	"THB": true, "TRY": true, "AED": true, "UAH": true, "USD": true, "UYU": true, "VEF": true, "VES": true}

// errCurrency is a currency Meta's table does not list, whose amounts cannot
// be read exactly.
var errCurrency = errors.New("metaads: the account's currency is not in Meta's currency table")

// digitsOf is the decimal digits Meta's integer amounts carry in currency:
// 0 for an offset of 1, 2 for 100.
func digitsOf(currency string) (int, error) {
	switch {
	case offsetOne[currency]:
		return 0, nil
	case offsetHundred[currency]:
		return 2, nil
	}
	return 0, fmt.Errorf("%w: %s", errCurrency, currency)
}

// microsOf is a Meta amount (an integer of the currency's offset units, as a
// string or a number) in micros; "" and "0" are zero.
func microsOf(v any, currency string) (int64, error) {
	s := strings.TrimSpace(fmt.Sprint(v))
	if v == nil || s == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("metaads: %q is not an amount in offset units", s)
	}
	d, err := digitsOf(currency)
	if err != nil {
		return 0, err
	}
	return growth.FromMinor(n, d)
}

// minorOf is micros as the integer Meta takes in currency, rounded down so
// what is sent never exceeds what was allowed.
func minorOf(micros int64, currency string) (int64, error) {
	d, err := digitsOf(currency)
	if err != nil {
		return 0, err
	}
	return growth.ToMinor(micros, d)
}

// spendAct is a prepared write as the guard weighs it.
type spendAct struct {
	m       made
	befores []before
}

// budgeted is a campaign or ad set as capacity reads it.
type budgeted struct {
	campaign string
	status   string
	daily    int64
	lifetime int64
	left     int64
	end      time.Time
}

// ledger is an ad account's campaigns and ad sets, in micros of its currency.
type ledger struct {
	currency  string
	zone      *time.Location
	now       time.Time
	campaigns map[string]*budgeted
	adSets    map[string]*budgeted
}

func (l ledger) clone() ledger {
	out := l
	out.campaigns, out.adSets = map[string]*budgeted{}, map[string]*budgeted{}
	for k, v := range l.campaigns {
		c := *v
		out.campaigns[k] = &c
	}
	for k, v := range l.adSets {
		a := *v
		out.adSets[k] = &a
	}
	return out
}

// share is what one budget adds to a day: its daily budget, or its lifetime
// budget's remainder over its remaining days in the account's zone, rounded up.
func (l ledger) share(b *budgeted) int64 {
	if b.status != "ACTIVE" || (!b.end.IsZero() && !b.end.After(l.now)) {
		return 0
	}
	if b.daily > 0 {
		return b.daily
	}
	if b.lifetime <= 0 {
		return 0
	}
	if b.end.IsZero() {
		return b.left
	}
	days := int64(b.end.In(l.zone).Sub(l.now.In(l.zone)).Hours()/24) + 1
	return (b.left + days - 1) / days
}

// daily is the account's capacity: each active, unended campaign's budget, or
// its active ad sets' where the campaign holds none.
func (l ledger) daily() int64 {
	var sum int64
	for id, c := range l.campaigns {
		if c.status != "ACTIVE" || (!c.end.IsZero() && !c.end.After(l.now)) {
			continue
		}
		if c.daily > 0 || c.lifetime > 0 {
			sum += l.share(c)
			continue
		}
		for _, a := range l.adSets {
			if a.campaign == id {
				sum += l.share(a)
			}
		}
	}
	return sum
}

// maxLedgerPages bounds a capacity read: 50 pages of 200 campaigns or ad sets,
// past which capacity is not certain and spend is refused.
const maxLedgerPages = 50

// read is the account's ledger as Meta answers it now.
func (p *Product) read(ctx context.Context, c *conn, account, currency, zone string) (ledger, error) {
	loc, err := time.LoadLocation(zone)
	if err != nil {
		loc = time.UTC
	}
	l := ledger{currency: currency, zone: loc, now: p.deps.Now(), campaigns: map[string]*budgeted{},
		adSets: map[string]*budgeted{}}
	tok, err := p.tokenFor(ctx, c, nil, growth.ClassRead, []string{"ads_read"})
	if err != nil {
		return ledger{}, err
	}
	for _, list := range []struct {
		edge, fields string
		into         map[string]*budgeted
	}{
		{"/campaigns", "id,status,daily_budget,lifetime_budget,budget_remaining,stop_time", l.campaigns},
		{"/adsets", "id,campaign_id,status,daily_budget,lifetime_budget,budget_remaining,end_time", l.adSets},
	} {
		err := pages(ctx, c.client, tok.valueNow(), "/"+account+list.edge, url.Values{"fields": {list.fields}, "limit": {"200"}},
			objectOf(account), true, maxLedgerPages, func(raw json.RawMessage) error {
				var d map[string]any
				if err := decode(raw, &d); err != nil {
					return fmt.Errorf("metaads: read %s's %s: %w", account, strings.TrimPrefix(list.edge, "/"), err)
				}
				b, err := budgetOf(d, currency)
				if err != nil {
					return err
				}
				id, _ := d["id"].(string)
				list.into[id] = b
				return nil
			})
		if err != nil {
			return ledger{}, err
		}
	}
	return l, nil
}

// budgetOf reads a campaign or ad set's budget; an amount that does not read
// is an error, never zero.
func budgetOf(d map[string]any, currency string) (*budgeted, error) {
	b := &budgeted{status: fmt.Sprint(d["status"])}
	b.campaign, _ = d["campaign_id"].(string)
	var err error
	if b.daily, err = microsOf(d["daily_budget"], currency); err != nil {
		return nil, err
	}
	if b.lifetime, err = microsOf(d["lifetime_budget"], currency); err != nil {
		return nil, err
	}
	if b.left, err = microsOf(d["budget_remaining"], currency); err != nil {
		return nil, err
	}
	if b.left == 0 {
		b.left = b.lifetime
	}
	for _, k := range []string{"stop_time", "end_time"} {
		if t, ok := timeOf(d[k]); ok {
			b.end = t
		}
	}
	return b, nil
}

// apply is l with act's requests applied as Meta would apply them.
func (l ledger) apply(act spendAct) (ledger, error) {
	out := l.clone()
	for i, t := range act.m.targets {
		set := func(b *budgeted) error {
			merged := maps.Clone(act.befores[i].attrs)
			if merged == nil {
				merged = map[string]any{}
			}
			for k, v := range t.params {
				merged[k] = plain(v)
			}
			if _, ok := t.params["status"]; !ok && b.status != "" {
				merged["status"] = b.status
			}
			nb, err := budgetOf(merged, l.currency)
			if err != nil {
				return err
			}
			if nb.campaign == "" {
				nb.campaign = b.campaign
			}
			if _, set := t.params["lifetime_budget"]; set {
				nb.left = nb.lifetime
			}
			*b = *nb
			return nil
		}
		key := fmt.Sprintf("new:%d", i)
		switch t.row.op.ID {
		case opCampaignUpdate:
			c := out.campaigns[t.node]
			if c == nil {
				c = &budgeted{}
				out.campaigns[t.node] = c
			}
			if err := set(c); err != nil {
				return ledger{}, err
			}
		case opAdSetUpdate:
			a := out.adSets[t.node]
			if a == nil {
				a = &budgeted{}
				out.adSets[t.node] = a
			}
			if err := set(a); err != nil {
				return ledger{}, err
			}
		case opCampaignCreate:
			c := &budgeted{status: "ACTIVE"}
			if err := set(c); err != nil {
				return ledger{}, err
			}
			out.campaigns[key] = c
		case opAdSetCreate:
			a := &budgeted{status: "ACTIVE"}
			if err := set(a); err != nil {
				return ledger{}, err
			}
			if from, ok := t.chain["campaign_id"]; ok {
				a.campaign = fmt.Sprintf("new:%d", from)
			}
			out.adSets[key] = a
		case "campaign.copies.update", "ad_set.copies.update":
			out.copyOf(t)
		}
	}
	return out, nil
}

// copyOf adds a copy of t's object as Meta makes it: in the status its
// status_option names (PAUSED where it names none), with its ad sets where
// deep_copy is set.
func (l ledger) copyOf(t target) {
	status := fmt.Sprint(plain(t.params["status_option"]))
	if t.params["status_option"] == nil {
		status = "PAUSED"
	}
	copied := func(b *budgeted) *budgeted {
		c := *b
		if status != "INHERITED_FROM_SOURCE" {
			c.status = status
		}
		return &c
	}
	if src := l.campaigns[t.node]; src != nil {
		id := "copy:" + t.node
		l.campaigns[id] = copied(src)
		if deep, _ := plain(t.params["deep_copy"]).(bool); deep {
			for k, a := range l.adSets {
				if a.campaign == t.node {
					c := copied(a)
					c.campaign = id
					l.adSets["copy:"+k] = c
				}
			}
		}
	}
	if src := l.adSets[t.node]; src != nil {
		l.adSets["copy:"+t.node] = copied(src)
	}
}

// capacity is the guard's [spendguard.Capacity] over Meta.
type capacity struct{ p *Product }

// Daily is account's capacity now.
func (c capacity) Daily(ctx context.Context, account string) (int64, error) {
	conn, err := c.p.connNow()
	if err != nil {
		return 0, err
	}
	a, err := c.p.freshAccount(ctx, conn, account)
	if err != nil {
		return 0, err
	}
	l, err := c.p.read(ctx, conn, account, a.Currency, a.TimeZone)
	if err != nil {
		return 0, err
	}
	return l.daily(), nil
}

// Delta is what act adds to account's capacity if applied now.
func (c capacity) Delta(ctx context.Context, account string, act spendAct) (int64, error) {
	conn, err := c.p.connNow()
	if err != nil {
		return 0, err
	}
	a, err := c.p.freshAccount(ctx, conn, account)
	if err != nil {
		return 0, err
	}
	l, err := c.p.read(ctx, conn, account, a.Currency, a.TimeZone)
	if err != nil {
		return 0, err
	}
	after, err := l.apply(act)
	if err != nil {
		return 0, err
	}
	return after.daily() - l.daily(), nil
}

// freshAccount is ad account id read now, never from the list a pass keeps:
// its status and currency are what the guard holds a change to.
func (p *Product) freshAccount(ctx context.Context, c *conn, id string) (adAccount, error) {
	tok, err := p.tokenFor(ctx, c, nil, growth.ClassRead, []string{"ads_read"})
	if err != nil {
		return adAccount{}, err
	}
	resp, err := c.client.Do(ctx, tok.valueNow(), Request{Method: "GET", Path: "/" + id,
		Params: url.Values{"fields": {accountFields}}, Object: objectOf(id), Read: true, Reserved: true})
	if err != nil {
		return adAccount{}, err
	}
	var a adAccount
	if err := json.Unmarshal(resp.Body, &a); err != nil || a.ID != id {
		return adAccount{}, fmt.Errorf("%w: ad account %s", errAnswer, id)
	}
	return a, nil
}

// guard is the product's spend guard.
func (p *Product) guard() *spendguard.Guard[spendAct] {
	return spendguard.New[spendAct](Kind, capacity{p: p}, p.deps.Leases)
}

// guardRead is the account and its caps as they stand now, for a check.
func (p *Product) guardRead(ctx context.Context, c *conn, account string) (spendguard.Account, spendguard.Caps, error) {
	a, err := p.freshAccount(ctx, c, account)
	if err != nil {
		return spendguard.Account{}, spendguard.Caps{}, err
	}
	caps, ok := p.deps.Company().Integrations.GrowthCaps(Kind, account)
	if !ok {
		return spendguard.Account{}, spendguard.Caps{}, growth.Refuse(growth.RefusedForbidden, fmt.Sprintf("ad account "+
			"%s has no spending caps: a person sets both, per change and daily, under integrations.meta_ads.spend "+
			"before any spend is asked. No call was made.", account))
	}
	return spendguard.Account{ID: account, Currency: a.Currency, Serving: a.serving(), State: a.statusText()}, caps, nil
}

// checkSpend holds act to its account's caps now.
func (p *Product) checkSpend(ctx context.Context, act spendAct) (*growth.CardMoney, error) {
	if act.m.account == "" {
		return nil, invalid("a spend act names the ad account it spends on.")
	}
	acct, caps, err := p.guardRead(ctx, act.m.conn, act.m.account)
	if err != nil {
		return nil, said(err)
	}
	f, err := p.guard().Check(ctx, acct, caps, act)
	if err != nil {
		return nil, said(refusedBy(err))
	}
	return cardMoney(f), nil
}

// refusedBy words a guard refusal for the agent, in the account's currency.
func refusedBy(err error) error {
	var r *spendguard.Refusal
	if !errors.As(err, &r) {
		return err
	}
	amount := func(m int64) string {
		d, derr := digitsOf(r.Figures.Currency)
		if derr != nil {
			d = 2
		}
		s, _ := growth.FormatAmount(m, d)
		return s + " " + r.Figures.Currency
	}
	var words string
	switch r.Code {
	case spendguard.CodeOverPerChange:
		words = fmt.Sprintf("the change adds %s a day to ad account %s, over its per-change cap of %s",
			amount(r.Figures.Delta), r.Account.ID, amount(r.Figures.PerChangeCap))
	case spendguard.CodeOverDaily:
		words = fmt.Sprintf("ad account %s's daily capacity would be %s, over its daily cap of %s", r.Account.ID,
			amount(r.Figures.DailyAfter), amount(r.Figures.DailyCap))
	default:
		words = r.Error()
	}
	return growth.Refuse(growth.RefusedForbidden, "the spend guard refused it: "+words+". Nobody was asked, and no "+
		"call was made.")
}

// cardMoney is a check's figures as a card and the access log show them.
func cardMoney(f spendguard.Figures) *growth.CardMoney {
	return &growth.CardMoney{Currency: f.Currency, DeltaMicros: f.Delta, DailyMicros: f.Daily,
		DailyAfterMicros: f.DailyAfter, PerChangeCapMicros: f.PerChangeCap, DailyCapMicros: f.DailyCap,
		Overdelivery: overdelivery}
}

// overdelivery is Meta's rule on spending past a daily budget, as the card
// says it, without a figure: Meta's help center states the share and has
// changed it, so a figure here would go stale unseen.
const overdelivery = "Caps limit the budgets agents may set. Meta may spend more than a daily budget on a given day."
