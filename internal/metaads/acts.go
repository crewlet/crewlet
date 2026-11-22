package metaads

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/words"
)

// The curated tools build the requests the generic tools would send, so
// every one is classed, bound and recorded alike; what they add is a check
// before anybody is asked, in the words of the act.

// createIn is a create on account, its parameters set.
func createIn(opID, account string, params map[string]any) target {
	r := rowOf()[opID]
	return target{row: r, path: strings.Replace(r.op.Path, "act_{ad_account_id}", account, 1), node: account,
		params: params}
}

// moneyArg is a decimal amount a curated tool names, in the account's offset
// units, as Meta takes it.
func moneyArg(raw any, currency string) (int64, error) {
	s, _ := raw.(string)
	micros, err := growth.ParseAmount(s)
	if err != nil {
		return 0, invalid("%q is not an amount: a decimal such as 50.00 in the account's currency.", s)
	}
	minor, err := minorOf(micros, currency)
	if err != nil {
		return 0, invalid("ad account's currency %s is not in Meta's currency table.", currency)
	}
	return minor, nil
}

// accountCurrency is the call's ad account, held to the seat's, and its currency.
func (p *Product) accountCurrency(ctx context.Context, m *made, args map[string]any) (string, adAccount, error) {
	account, err := accountNamed(m.seat, args)
	if err != nil {
		return "", adAccount{}, err
	}
	a, err := p.accountOf(ctx, m.conn, account)
	if err != nil {
		return "", adAccount{}, said(err)
	}
	return account, a, nil
}

// budget is the budget tools' update of a campaign's or ad set's budget, at
// the level that holds it; the lower tool refuses a raise and an amount under
// the account's minimum daily budget.
func (p *Product) budget(ctx context.Context, m *made, args map[string]any) error {
	_, a, err := p.accountCurrency(ctx, m, args)
	if err != nil {
		return err
	}
	level, _ := args["level"].(string)
	id, _ := args["id"].(string)
	kind, _ := args["kind"].(string)
	if kind == "" {
		kind = "daily"
	}
	if kind != "daily" && kind != "lifetime" {
		return invalid("`kind` is daily or lifetime.")
	}
	field := kind + "_budget"
	minor, err := moneyArg(args["amount"], a.Currency)
	if err != nil {
		return err
	}
	t, err := update(level, id, map[string]any{field: strconv.FormatInt(minor, 10)})
	if err != nil {
		return err
	}
	if level == levelAd {
		return invalid("an ad holds no budget: name its ad set or campaign.")
	}
	cur, err := p.readObject(ctx, m.conn, id, []string{"daily_budget", "lifetime_budget"})
	if err != nil {
		return said(err)
	}
	was, _ := strconv.ParseInt(fmt.Sprint(cur[field]), 10, 64)
	other := "lifetime_budget"
	if field == other {
		other = "daily_budget"
	}
	otherHeld, _ := strconv.ParseInt(fmt.Sprint(cur[other]), 10, 64)
	switch {
	case was <= 0 && otherHeld > 0:
		return invalid("%s %s holds a %s, not a %s budget.", level, id, strings.ReplaceAll(other, "_", " "), kind)
	case was <= 0:
		holder := "its ad sets"
		if level == levelAdSet {
			holder = "its campaign"
		}
		return invalid("%s %s holds no budget: %s does. Change it there.", level, id, holder)
	}
	if m.tool == ToolLowerBudget {
		switch {
		case minor >= was:
			return invalid("%s is not lower than the budget now (%s): ask with %s.", args["amount"],
				amount(was, a.Currency), ToolRaiseBudget)
		case kind == "daily" && a.MinDaily > 0 && minor < a.MinDaily:
			return invalid("%s is under the ad account's minimum daily budget of %s.", args["amount"],
				amount(a.MinDaily, a.Currency))
		}
	}
	m.targets = []target{t}
	return nil
}

// bid is the bid tools' update of an ad set's or ad's bid amount, or an ad
// set's bid strategy; the lower tool refuses a raise and a strategy change.
func (p *Product) bid(ctx context.Context, m *made, args map[string]any) error {
	_, a, err := p.accountCurrency(ctx, m, args)
	if err != nil {
		return err
	}
	level, _ := args["level"].(string)
	id, _ := args["id"].(string)
	params := map[string]any{}
	var minor int64
	if args["amount"] != nil {
		if minor, err = moneyArg(args["amount"], a.Currency); err != nil {
			return err
		}
		params["bid_amount"] = strconv.FormatInt(minor, 10)
	}
	if s, _ := args["bid_strategy"].(string); s != "" {
		if level != levelAdSet || !slices.Contains(bidStrategies, s) || m.tool == ToolLowerBid {
			return invalid("`bid_strategy` is an ad set's, one of %s, and only %s changes it.",
				words.Or(bidStrategies), ToolRaiseBid)
		}
		params["bid_strategy"] = s
	}
	if len(params) == 0 {
		return invalid("name the new `amount` or `bid_strategy`.")
	}
	if level != levelAdSet && level != levelAd {
		return invalid("`level` is ad_set or ad.")
	}
	t, err := update(level, id, params)
	if err != nil {
		return err
	}
	if m.tool == ToolLowerBid {
		cur, err := p.readObject(ctx, m.conn, id, []string{"bid_amount"})
		if err != nil {
			return said(err)
		}
		was, _ := strconv.ParseInt(fmt.Sprint(cur["bid_amount"]), 10, 64)
		if was <= 0 || minor >= was {
			return invalid("%v is not lower than the bid amount now: ask with %s.", args["amount"], ToolRaiseBid)
		}
	}
	m.targets = []target{t}
	return nil
}

// countryCode is a two-letter country code as Meta's targeting writes it.
var countryCode = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[A-Z]{2}$`) })

// platforms are the publisher platforms an ad set may name.
var platforms = []string{"facebook", "instagram", "messenger", "audience_network"}

// targeting is the targeting tool's update of one ad set's countries, ages
// and placements, the rest of its targeting kept as it reads now.
func (p *Product) targeting(ctx context.Context, m *made, args map[string]any) error {
	id, _ := args["ad_set"].(string)
	cur, err := p.readObject(ctx, m.conn, id, []string{"targeting"})
	if err != nil {
		return said(err)
	}
	tg, _ := cur["targeting"].(map[string]any)
	if tg == nil {
		tg = map[string]any{}
	}
	if raw, ok := args["countries"].([]any); ok {
		var countries []any
		for _, c := range raw {
			s, _ := c.(string)
			if !countryCode().MatchString(s) {
				return invalid("%q is not a two-letter country code.", s)
			}
			countries = append(countries, s)
		}
		geo, _ := tg["geo_locations"].(map[string]any)
		if geo == nil {
			geo = map[string]any{}
		}
		geo["countries"] = countries
		tg["geo_locations"] = geo
	}
	for _, k := range []string{"age_min", "age_max"} {
		if v, ok := args[k]; ok {
			n, nerr := limitOf(v)
			if nerr != nil || n < 13 || n > 65 {
				return invalid("`%s` is 13 to 65.", k)
			}
			tg[k] = n
		}
	}
	if raw, ok := args["platforms"].([]any); ok {
		var list []any
		for _, x := range raw {
			s, _ := x.(string)
			if !slices.Contains(platforms, s) {
				return invalid("%q is not one of %s.", s, words.Or(platforms))
			}
			list = append(list, s)
		}
		if len(list) == 0 {
			delete(tg, "publisher_platforms")
		} else {
			tg["publisher_platforms"] = list
		}
	}
	t, err := update(levelAdSet, id, map[string]any{"targeting": tg})
	if err != nil {
		return err
	}
	m.targets = []target{t}
	return nil
}

// createAd is the create-ad tool's requests: an ad from a creative in the
// account, or a creative from an image in its library with new text and the
// ad made from it.
func (m *made) createAd(args map[string]any) error {
	account, err := accountNamed(m.seat, args)
	if err != nil {
		return err
	}
	adSet, _ := args["ad_set"].(string)
	name, _ := args["name"].(string)
	status, _ := args["status"].(string)
	if status == "" {
		status = "ACTIVE"
	}
	if !digitsOnly().MatchString(adSet) || strings.TrimSpace(name) == "" || (status != "ACTIVE" && status != "PAUSED") {
		return invalid("`ad_set` is its ID, `name` the ad's name and `status` ACTIVE or PAUSED.")
	}
	ad := map[string]any{"name": name, "adset_id": adSet, "status": status}
	if creative, _ := args["creative"].(string); creative != "" {
		if !digitsOnly().MatchString(creative) {
			return invalid("%q is not a creative's ID.", creative)
		}
		ad["creative"] = map[string]any{"creative_id": creative}
		m.targets = []target{createIn(opAdCreate, account, ad)}
		return nil
	}
	hash, _ := args["image_hash"].(string)
	page, _ := args["page_id"].(string)
	message, _ := args["message"].(string)
	link, _ := args["link"].(string)
	if hash == "" || !digitsOnly().MatchString(page) || strings.TrimSpace(message) == "" || link == "" {
		return invalid("without `creative`, give `image_hash`, `page_id`, `message` and `link`.")
	}
	data := map[string]any{"image_hash": hash, "message": message, "link": link}
	if h, _ := args["headline"].(string); h != "" {
		data["name"] = h
	}
	creative := createIn(opCreativeCreate, account, map[string]any{"name": name,
		"object_story_spec": map[string]any{"page_id": page, "link_data": data}})
	made := createIn(opAdCreate, account, ad)
	made.chain = map[string]int{"creative": 0}
	m.targets = []target{creative, made}
	return nil
}

// createCampaign is the app campaign tool's requests: a campaign, its ad set
// and its ads, every one PAUSED, each made in the one made before it.
func (p *Product) createCampaign(ctx context.Context, m *made, args map[string]any) error {
	account, a, err := p.accountCurrency(ctx, m, args)
	if err != nil {
		return err
	}
	name, _ := args["name"].(string)
	app, _ := args["application_id"].(string)
	store, _ := args["object_store_url"].(string)
	if strings.TrimSpace(name) == "" || !digitsOnly().MatchString(app) || !strings.HasPrefix(store, "https://") {
		return invalid("`name` is the campaign's name, `application_id` the Meta app's ID and `object_store_url` " +
			"the app's https store address.")
	}
	budget, err := moneyArg(args["daily_budget"], a.Currency)
	if err != nil {
		return err
	}
	if a.MinDaily > 0 && budget < a.MinDaily {
		return invalid("%v is under the ad account's minimum daily budget of %s.", args["daily_budget"],
			amount(a.MinDaily, a.Currency))
	}
	var countries []any
	raw, _ := args["countries"].([]any)
	for _, c := range raw {
		s, _ := c.(string)
		if !countryCode().MatchString(s) {
			return invalid("%q is not a two-letter country code.", s)
		}
		countries = append(countries, s)
	}
	creatives, _ := args["creatives"].([]any)
	if len(countries) == 0 || len(creatives) == 0 || len(creatives) > 10 {
		return invalid("name 1 or more `countries` and 1 to 10 `creatives`.")
	}
	campaign := createIn(opCampaignCreate, account, map[string]any{"name": name, "objective": "OUTCOME_APP_PROMOTION",
		"special_ad_categories": []any{}, "daily_budget": strconv.FormatInt(budget, 10),
		"bid_strategy": "LOWEST_COST_WITHOUT_CAP", "status": "PAUSED"})
	adSet := createIn(opAdSetCreate, account, map[string]any{"name": name + " ad set",
		"optimization_goal": "APP_INSTALLS", "billing_event": "IMPRESSIONS", "status": "PAUSED",
		"promoted_object": map[string]any{"application_id": app, "object_store_url": store},
		"targeting":       map[string]any{"geo_locations": map[string]any{"countries": countries}}})
	adSet.chain = map[string]int{"campaign_id": 0}
	m.targets = []target{campaign, adSet}
	for i, c := range creatives {
		id, _ := c.(string)
		if !digitsOnly().MatchString(id) {
			return invalid("%q is not a creative's ID.", id)
		}
		ad := createIn(opAdCreate, account, map[string]any{"name": fmt.Sprintf("%s ad %d", name, i+1),
			"status": "PAUSED", "creative": map[string]any{"creative_id": id}})
		ad.chain = map[string]int{"adset_id": 1}
		m.targets = append(m.targets, ad)
	}
	return nil
}
