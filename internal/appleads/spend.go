package appleads

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/spendguard"
)

// An ad account's daily capacity is the daily budget of every campaign whose
// status is ENABLED, not deleted and not ended, whether it serves now or is
// on hold: a hold that clears spends at once with no change for the guard to
// see, so it counts now. A change's delta is what it adds to that sum. A bid
// or target CPA raise adds nothing (the daily budget still caps it) and is
// still held. Apple writes money as decimal strings, read exactly into
// micros of the account's own currency (internal/spendguard).

// spendAct is a prepared write as the guard weighs it: its requests and their
// targets as read.
type spendAct struct {
	m       made
	befores []before
}

// capacity is the guard's [spendguard.Capacity] over Apple.
type capacity struct{ p *Product }

// maxCampaignPages bounds a capacity read: 50 pages of 1,000 campaigns, past
// which capacity is not certain and spend is refused.
const maxCampaignPages = 50

// errCapacity is a capacity the engine could not read whole.
var errCapacity = errors.New("appleads: the ad account's campaigns could not be read whole")

// Daily is the sum of account's enabled, unended campaigns' daily budgets.
func (c capacity) Daily(ctx context.Context, account string) (int64, error) {
	conn, _, err := c.p.reach(ctx, account)
	if err != nil {
		return 0, err
	}
	const pageSize = 1000
	var sum int64
	for page := range maxCampaignPages {
		resp, err := conn.client.Do(ctx, Request{Method: http.MethodPost, Path: "/v1/campaigns/query", Account: account,
			Read: true, Reserved: true, Body: map[string]any{
				"filters":    []any{map[string]any{"field": "status", "operator": "EQUALS", "value": statusEnabled}},
				"pagination": map[string]any{"offset": page * pageSize, "pageSize": pageSize}}})
		if err != nil {
			return 0, err
		}
		var doc struct {
			Result []map[string]any `json:"result"`
		}
		if err := decode(resp.Body, &doc); err != nil {
			return 0, fmt.Errorf("appleads: read ad account %s's campaigns: %w", account, err)
		}
		for _, campaign := range doc.Result {
			if v, ok := campaign["adAccountId"]; ok && idText(v) != account {
				// ANOTHER ACCOUNT'S CAMPAIGN is never counted: the answer is held
				// to the account it was asked for, as every answer is.
				continue
			}
			n, err := c.p.contribution(campaign)
			if err != nil {
				return 0, err
			}
			sum += n
		}
		if len(doc.Result) < pageSize {
			return sum, nil
		}
	}
	return 0, errCapacity
}

// Delta is what act adds to its account's capacity.
func (c capacity) Delta(ctx context.Context, account string, act spendAct) (int64, error) {
	var sum int64
	for i, t := range act.m.targets {
		var n int64
		var err error
		switch t.op.ID {
		case opCampaignPut:
			merged := merge(act.befores[i].attrs, t.body)
			var was, now int64
			if was, err = c.p.contribution(act.befores[i].attrs); err == nil {
				now, err = c.p.contribution(merged)
			}
			n = now - was
		case "campaignsPost":
			body, _ := t.body.(map[string]any)
			created := merge(map[string]any{"status": statusEnabled}, body)
			n, err = c.p.contribution(created)
		case "applyDailyBudgetRecommendations":
			n, err = c.p.recommendedDelta(ctx, act.m, t)
		}
		if err != nil {
			return 0, err
		}
		sum += n
	}
	return sum, nil
}

// contribution is what campaign adds to capacity: its daily budget while it
// is enabled, not deleted and not ended. A budget that does not read as an
// amount is an error, never zero.
func (p *Product) contribution(campaign map[string]any) (int64, error) {
	if campaign == nil {
		return 0, nil
	}
	if s, _ := campaign["status"].(string); s != statusEnabled {
		return 0, nil
	}
	if deleted, _ := campaign["deleted"].(bool); deleted {
		return 0, nil
	}
	if end, _ := campaign["endTime"].(string); end != "" {
		if t, err := time.Parse("2006-01-02T15:04:05.000", end); err == nil && !t.After(p.deps.Now().UTC()) {
			return 0, nil
		}
	}
	amount := fmt.Sprint(lookup(campaign, fieldDailyBudget))
	micros, err := growth.ParseAmount(amount)
	if err != nil {
		return 0, fmt.Errorf("appleads: campaign %v's daily budget %q is not an amount", campaign["id"], amount)
	}
	return micros, nil
}

// merge is base with patch's fields set over it, as Apple's partial update
// applies them: objects field by field, a list or a value whole.
func merge(base map[string]any, patch any) map[string]any {
	out := map[string]any{}
	for k, v := range base {
		out[k] = v
	}
	p, _ := patch.(map[string]any)
	for k, v := range p {
		if child, ok := v.(map[string]any); ok {
			was, _ := out[k].(map[string]any)
			out[k] = merge(was, child)
			continue
		}
		out[k] = v
	}
	return out
}

// recommendedDelta is what applying daily budget recommendations adds: each
// one's applied (or suggested) budget less the campaign's budget Apple gives
// beside it. One the engine cannot find is refused, never counted as zero.
func (p *Product) recommendedDelta(ctx context.Context, m made, t target) (int64, error) {
	items, _ := t.body.([]any)
	var sum int64
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		id := idText(item["id"])
		resp, err := m.conn.client.Do(ctx, Request{Method: http.MethodPost, Path: "/v1/recommendations/daily-budgets/query",
			Account: m.account, Read: true, Reserved: true, Body: map[string]any{
				"filters": []any{
					map[string]any{"field": "promotedObjectId", "operator": "EQUALS", "value": []any{idText(item["promotedObjectId"])}},
					map[string]any{"field": "promotedObjectType", "operator": "EQUALS", "value": []any{fmt.Sprint(item["promotedObjectType"])}}},
				"pagination": map[string]any{"offset": 0, "pageSize": 1000}}})
		if err != nil {
			return 0, err
		}
		var doc struct {
			Result []map[string]any `json:"result"`
		}
		if err := decode(resp.Body, &doc); err != nil {
			return 0, fmt.Errorf("appleads: read the daily budget recommendations: %w", err)
		}
		var rec map[string]any
		for _, r := range doc.Result {
			if idText(r["id"]) == id {
				rec = r
			}
		}
		if rec == nil {
			return 0, growth.Refuse(growth.RefusedNotFound, fmt.Sprintf("recommendation %s is not among ad account %s's "+
				"daily budget recommendations, so what it adds cannot be counted. No call was made.", id, m.account))
		}
		target := lookup(item, "appliedDailyBudget.amount")
		if target == nil {
			target = lookup(rec, "suggestedDailyBudgetAmount.amount")
		}
		after, err1 := growth.ParseAmount(fmt.Sprint(target))
		was, err2 := growth.ParseAmount(fmt.Sprint(lookup(rec, "dailyBudget.amount")))
		if err1 != nil || err2 != nil {
			return 0, fmt.Errorf("appleads: recommendation %s's budgets are not amounts", id)
		}
		sum += after - was
	}
	return sum, nil
}

// guard is the product's spend guard.
func (p *Product) guard() *spendguard.Guard[spendAct] {
	return spendguard.New[spendAct](Kind, capacity{p: p}, p.deps.Leases)
}

// guardAccounts are the ad accounts a spend act is held to: the call's, or
// for a budget order every ad account it names, before and after.
func guardAccounts(act spendAct) []string {
	if act.m.account != "" {
		return []string{act.m.account}
	}
	var out []string
	add := func(v any) {
		ids, _ := v.([]any)
		for _, id := range ids {
			if s := idText(id); !containsString(out, s) {
				out = append(out, s)
			}
		}
	}
	for i, t := range act.m.targets {
		if body, ok := t.body.(map[string]any); ok {
			add(body["adAccountIds"])
		}
		add(act.befores[i].attrs["adAccountIds"])
	}
	return out
}

func containsString(set []string, s string) bool {
	for _, v := range set {
		if v == s {
			return true
		}
	}
	return false
}

// guardRead is the account and caps as they stand now, for a check.
func (p *Product) guardRead(ctx context.Context, account string) (spendguard.Account, spendguard.Caps, error) {
	conn, _, err := p.reach(ctx, account)
	if err != nil {
		return spendguard.Account{}, spendguard.Caps{}, err
	}
	info, err := p.accountOf(ctx, conn, account)
	if err != nil {
		return spendguard.Account{}, spendguard.Caps{}, err
	}
	caps, ok := p.deps.Company().Integrations.GrowthCaps(Kind, account)
	if !ok {
		return spendguard.Account{}, spendguard.Caps{}, growth.Refuse(growth.RefusedForbidden, fmt.Sprintf("ad account "+
			"%s has no spending caps: a person sets both, per change and daily, under integrations.apple_ads.spend "+
			"before any spend is asked. No call was made.", account))
	}
	return spendguard.Account{ID: account, Currency: info.Currency, Serving: info.serving(),
		State: strings.Join(append([]string{info.Status}, info.Reasons...), ", ")}, caps, nil
}

// refusedBy words a guard refusal for the agent, in the account's currency.
func refusedBy(err error) error {
	var r *spendguard.Refusal
	if !errors.As(err, &r) {
		return err
	}
	amount := func(m int64) string {
		s, _ := growth.FormatAmount(m, 2)
		return s + " " + r.Figures.Currency
	}
	var said string
	switch r.Code {
	case spendguard.CodeOverPerChange:
		said = fmt.Sprintf("the change adds %s a day to ad account %s, over its per-change cap of %s",
			amount(r.Figures.Delta), r.Account.ID, amount(r.Figures.PerChangeCap))
	case spendguard.CodeOverDaily:
		said = fmt.Sprintf("ad account %s's daily capacity would be %s, over its daily cap of %s", r.Account.ID,
			amount(r.Figures.DailyAfter), amount(r.Figures.DailyCap))
	default:
		said = r.Error()
	}
	return growth.Refuse(growth.RefusedForbidden, "the spend guard refused it: "+said+". Nobody was asked, and no "+
		"call was made.")
}

// cardMoney is a check's figures as a card and the access log show them.
func cardMoney(f spendguard.Figures) *growth.CardMoney {
	return &growth.CardMoney{Currency: f.Currency, DeltaMicros: f.Delta, DailyMicros: f.Daily,
		DailyAfterMicros: f.DailyAfter, PerChangeCapMicros: f.PerChangeCap, DailyCapMicros: f.DailyCap}
}

// checkSpend holds act to every account's caps now, answering the figures of
// the account the act is placed in (a budget order's first).
func (p *Product) checkSpend(ctx context.Context, act spendAct) (*growth.CardMoney, error) {
	accounts := guardAccounts(act)
	if len(accounts) == 0 {
		return nil, invalid("a spend act names the ad accounts it spends on.")
	}
	var money *growth.CardMoney
	for _, account := range accounts {
		acct, caps, err := p.guardRead(ctx, account)
		if err != nil {
			return nil, said(err)
		}
		f, err := p.guard().Check(ctx, acct, caps, act)
		if err != nil {
			return nil, said(refusedBy(err))
		}
		if money == nil {
			money = cardMoney(f)
		}
	}
	return money, nil
}
