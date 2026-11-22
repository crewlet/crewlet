package googleads

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/growth"
)

// opOf is a catalog operation by ID.
func opOf(t *testing.T, id string) growth.Operation {
	t.Helper()
	for _, op := range operations() {
		if op.ID == id {
			return op
		}
	}
	t.Fatalf("no operation %s", id)
	return growth.Operation{}
}

// classOf is what a call of id with body comes to against l.
func classOf(t *testing.T, id string, body map[string]any, l live) growth.Class {
	t.Helper()
	return ruling(t, id, body, l).class
}

// ruling is what classify makes of a call of id with body against l.
func ruling(t *testing.T, id string, body map[string]any, l live) ruled {
	t.Helper()
	r, err := classify(opOf(t, id), body, l)
	if err != nil {
		t.Fatalf("%s %v: %v", id, body, err)
	}
	return r
}

// ops is a mutate body of operations.
func ops(o ...map[string]any) map[string]any {
	list := make([]any, len(o))
	for i, x := range o {
		list[i] = x
	}
	return map[string]any{"operations": list}
}

func update(name string, mask string, fields map[string]any) map[string]any {
	obj := map[string]any{"resourceName": name}
	for k, v := range fields {
		obj[k] = v
	}
	return map[string]any{"update": obj, "updateMask": mask}
}

const (
	campaigns = "googleads.customers.campaigns.mutate"
	budgets   = "googleads.customers.campaignBudgets.mutate"
	criteria  = "googleads.customers.adGroupCriteria.mutate"
	campaign1 = "customers/1234567890/campaigns/1"
	budgetA   = "customers/1234567890/campaignBudgets/10"
	budgetB   = "customers/1234567890/campaignBudgets/11"
)

// EVERY RULED METHOD NAMES A RULE THE BUILD HOLDS, and every operation type a
// mutate may carry is classed, by its family or its method's one class.
func TestEveryRuledMethodNamesARuleTheBuildHolds(t *testing.T) {
	t.Parallel()
	fams := families()
	special := map[string]bool{"FR-MUTATE": true, "FR-BATCH": true, "FR-RECOMMENDATION": true}
	for id, rule := range ruleOf {
		if _, ok := fams[rule]; !ok && !special[rule] {
			t.Errorf("%s names %s, which the build holds no family for", id, rule)
		}
	}
	for service, by := range services() {
		if by.family != "" {
			if _, ok := fams[by.family]; !ok {
				t.Errorf("operation type %s names %s, which the build holds no family for", service, by.family)
			}
		} else if !by.class.Valid() {
			t.Errorf("operation type %s has no class", service)
		}
	}
	for _, service := range []string{"campaign", "campaign_budget", "ad_group_ad", "ad_group_criterion",
		"campaign_criterion", "asset_group", "shared_set", "customer_negative_criterion", "book_campaigns"} {
		if _, ok := services()[service]; !ok {
			t.Errorf("a Mutate holding a %s operation would be refused", service)
		}
	}
}

// ENABLING SPENDS AND PAUSING OPERATES: a campaign made or set ENABLED (or made
// with no status, which Google serves) is spend; PAUSED is operate; removed,
// admin; a field no rule names, the highest the family allows.
//
// Mutation: read an unset status as paused, and a campaign made live goes
// unasked.
func TestEnablingSpendsAndPausingOperates(t *testing.T) {
	t.Parallel()
	enabled := map[string]any{"status": "ENABLED"}
	for _, tc := range []struct {
		name string
		body map[string]any
		want growth.Class
	}{
		{"made with no status", ops(map[string]any{"create": map[string]any{"name": "x"}}), spend},
		{"made paused", ops(map[string]any{"create": map[string]any{"status": "PAUSED"}}), operate},
		{"set enabled", ops(update(campaign1, "status", enabled)), spend},
		{"set paused", ops(update(campaign1, "status", map[string]any{"status": "PAUSED"})), operate},
		{"removed", ops(map[string]any{"remove": campaign1}), admin},
		{"renamed", ops(update(campaign1, "name", map[string]any{"name": "y"})), operate},
		{"a field no rule names", ops(update(campaign1, "someNewField", map[string]any{"someNewField": 1})), admin},
		{"a network turned on", ops(update(campaign1, "networkSettings.targetContentNetwork",
			map[string]any{"networkSettings": map[string]any{"targetContentNetwork": true}})), spend},
		{"made public", ops(update(campaign1, "brandGuidelines", map[string]any{"brandGuidelines": map[string]any{}})), public},
	} {
		l := live{campaign1: state{"status": "PAUSED", "network_settings": map[string]any{"target_content_network": false}}}
		if got := classOf(t, campaigns, tc.body, l); got != tc.want {
			t.Errorf("a campaign %s is %s, want %s", tc.name, got, tc.want)
		}
	}
}

// A RAISE SPENDS AND A LOWERING OPERATES, against the live value: a budget's
// amount, a target CPA, and the other way round for a target ROAS; a value the
// engine could not read is the higher class.
//
// Mutation: compare against nothing, and a lowering is asked as a raise or
// a raise made unasked.
func TestARaiseSpendsAndALoweringOperatesAgainstTheLiveValue(t *testing.T) {
	t.Parallel()
	amount := func(v string) map[string]any {
		return ops(update(budgetA, "amountMicros", map[string]any{"amountMicros": v}))
	}
	read := live{budgetA: state{"amount_micros": "5000000"}}
	if got := classOf(t, budgets, amount("6000000"), read); got != spend {
		t.Errorf("a raised budget is %s", got)
	}
	if got := classOf(t, budgets, amount("4000000"), read); got != operate {
		t.Errorf("a lowered budget is %s", got)
	}
	if got := classOf(t, budgets, amount("4000000"), live{}); got != spend {
		t.Errorf("a budget the engine could not read is %s", got)
	}
	roas := func(v float64) map[string]any {
		return ops(update(campaign1, "targetRoas.targetRoas", map[string]any{"targetRoas": map[string]any{"targetRoas": v}}))
	}
	l := live{campaign1: state{"bidding_strategy_type": "TARGET_ROAS", "target_roas": map[string]any{"target_roas": 4.0}}}
	if got := classOf(t, campaigns, roas(3), l); got != spend {
		t.Errorf("a lowered target ROAS is %s", got)
	}
	if got := classOf(t, campaigns, roas(5), l); got != operate {
		t.Errorf("a raised target ROAS is %s", got)
	}
}

// AN EXCLUSION ADDED NARROWS AND REMOVED WIDENS: a negative criterion made is
// operate and removed (as the live criterion says) is spend; a positive one
// made is spend and removed admin; unread, a remove is the higher.
func TestAnExclusionAddedNarrowsAndRemovedWidens(t *testing.T) {
	t.Parallel()
	crit := "customers/1234567890/adGroupCriteria/5~6"
	if got := classOf(t, criteria, ops(map[string]any{"create": map[string]any{"negative": true}}), live{}); got != operate {
		t.Errorf("a negative made is %s", got)
	}
	if got := classOf(t, criteria, ops(map[string]any{"create": map[string]any{"keyword": map[string]any{}}}), live{}); got != spend {
		t.Errorf("a keyword made is %s", got)
	}
	remove := ops(map[string]any{"remove": crit})
	if got := classOf(t, criteria, remove, live{crit: state{"negative": true}}); got != spend {
		t.Errorf("a negative removed is %s", got)
	}
	if got := classOf(t, criteria, remove, live{crit: state{"negative": false}}); got != admin {
		t.Errorf("a positive removed is %s", got)
	}
	if got := classOf(t, criteria, remove, live{}); got != admin {
		t.Errorf("a criterion removed unread is %s", got)
	}
}

// A MUTATE IS ITS HIGHEST OPERATION: a new budget and an enabled campaign in
// one GoogleAdsService.Mutate is spend, a pause alone operate, and an
// operation type the engine does not class refuses the call.
func TestAMutateIsItsHighestOperation(t *testing.T) {
	t.Parallel()
	mutate := "googleads.customers.googleAds.mutate"
	bundle := map[string]any{"mutateOperations": []any{
		map[string]any{"campaignBudgetOperation": map[string]any{"create": map[string]any{"amountMicros": "1"}}},
		map[string]any{"campaignOperation": map[string]any{"create": map[string]any{"status": "ENABLED"}}},
	}}
	if got := classOf(t, mutate, bundle, live{}); got != spend {
		t.Errorf("a budget and an enabled campaign made at once are %s", got)
	}
	// The highest wherever it stands: a pause after it does not lower it.
	enableThenPause := map[string]any{"mutateOperations": []any{
		map[string]any{"campaignOperation": map[string]any{"create": map[string]any{"status": "ENABLED"}}},
		map[string]any{"campaignOperation": update(campaign1, "status", map[string]any{"status": "PAUSED"})},
	}}
	if got := classOf(t, mutate, enableThenPause, live{}); got != spend {
		t.Errorf("an enabled campaign made before a pause is %s", got)
	}
	pause := map[string]any{"mutateOperations": []any{map[string]any{"campaignOperation": update(campaign1, "status",
		map[string]any{"status": "PAUSED"})}}}
	if got := classOf(t, mutate, pause, live{}); got != operate {
		t.Errorf("a pause through Mutate is %s", got)
	}
	unknown := map[string]any{"mutateOperations": []any{map[string]any{"mysteryOperation": map[string]any{
		"create": map[string]any{}}}}}
	if _, err := classify(opOf(t, mutate), unknown, live{}); err == nil ||
		!strings.Contains(err.Error(), "mystery") {
		t.Errorf("an operation type nobody classes answered %v", err)
	}
}

// A VALUE A RULE CANNOT READ IS THE HIGHEST IT ALLOWS: an enum by its number
// (2 is ENABLED, 2 OPTED_IN), a switch cleared or set to what is no boolean,
// and a raise to what is no number never pass as the pause or lowering they
// could be read as.
//
// Mutation: compare an enum by its name alone, read a switch by its truth
// alone, or read an unreadable amount added where none was as a cap, and
// each lands at operate.
func TestAValueARuleCannotReadIsTheHighestItAllows(t *testing.T) {
	t.Parallel()
	crit := "customers/1234567890/campaignCriteria/5~6"
	subs := "googleads.customers.recommendationSubscriptions.mutateRecommendationSubscription"
	for name, tc := range map[string]struct {
		id   string
		body map[string]any
		l    live
		want growth.Class
	}{
		"a status by its number": {campaigns, ops(update(campaign1, "status", map[string]any{"status": 2.0})),
			live{campaign1: state{"status": "PAUSED"}}, spend},
		"a status cleared": {campaigns, ops(update(campaign1, "status", nil)),
			live{campaign1: state{"status": "PAUSED"}}, spend},
		"a criterion's status by its number": {"googleads.customers.campaignCriteria.mutate",
			ops(update(crit, "status", map[string]any{"status": 2.0})), live{crit: state{"negative": false}}, spend},
		"a subscription by its number": {subs, ops(map[string]any{"create": map[string]any{"type": "KEYWORD",
			"status": 2.0}}), live{}, spend},
		"a switch to a string": {campaigns, ops(update(campaign1, "networkSettings.targetContentNetwork",
			map[string]any{"networkSettings": map[string]any{"targetContentNetwork": "true"}})),
			live{campaign1: state{"network_settings": map[string]any{"target_content_network": false}}}, spend},
		"a switch cleared": {campaigns, ops(update(campaign1, "networkSettings.targetContentNetwork", nil)),
			live{campaign1: state{"network_settings": map[string]any{"target_content_network": false}}}, spend},
		"automation by its number": {campaigns, ops(update(campaign1, "assetAutomationSettings",
			map[string]any{"assetAutomationSettings": []any{map[string]any{"assetAutomationType": "TEXT_ASSET_AUTOMATION",
				"assetAutomationStatus": 2.0}}})), live{campaign1: state{}}, public},
		"a budget to what is no number": {budgets, ops(update(budgetA, "amountMicros",
			map[string]any{"amountMicros": "lots"})), live{budgetA: state{}}, spend},
	} {
		if got := classOf(t, tc.id, tc.body, tc.l); got != tc.want {
			t.Errorf("%s is %s, want %s", name, got, tc.want)
		}
	}
	// The same rules lower what they can read.
	if got := classOf(t, subs, ops(map[string]any{"create": map[string]any{"type": "CAMPAIGN_BUDGET",
		"status": "PAUSED"}}), live{}); got != operate {
		t.Errorf("a paused subscription is %s", got)
	}
	if got := classOf(t, "googleads.customers.mutate", map[string]any{"operation": update("customers/1234567890",
		"descriptiveName", map[string]any{"descriptiveName": "Acme"})}, live{}); got != operate {
		t.Errorf("an account renamed is %s", got)
	}
}

// A RESOURCE ANOTHER ACCOUNT HOLDS IS SEEN AT ANY DEPTH OF A BODY, for the
// scope check: what an operation acts on, what its fields point at, and what
// a method that is no mutate names.
func TestEveryResourceABodyNamesIsSeen(t *testing.T) {
	t.Parallel()
	names := namesIn(ops(map[string]any{"create": map[string]any{
		"campaign": "customers/9999999999/campaigns/7", "name": "customers/1"}},
		map[string]any{"remove": "customers/1234567890/adGroups/2"}))
	if len(names) != 2 || names[0] != "customers/9999999999/campaigns/7" ||
		names[1] != "customers/1234567890/adGroups/2" {
		t.Errorf("the names seen are %v", names)
	}
}

// ONLY A NAMED LOWERING LOWERS: Google also takes an enum's number, as a JSON
// number or a string ("2" is ENABLED), so a number, a name in another case or
// any other value is the higher class wherever a rule lowers on a name, and
// the spend guard counts such a campaign as serving.
//
// Mutation: lower on any string but the raising names, as the rules did, and
// "2" pauses a campaign, a keyword and a subscription and serves nothing.
func TestOnlyANamedLoweringLowers(t *testing.T) {
	t.Parallel()
	crit := "customers/1234567890/adGroupCriteria/5~6"
	excl := "customers/1234567890/campaignCriteria/5~7"
	subs := "googleads.customers.recommendationSubscriptions.mutateRecommendationSubscription"
	for _, v := range []any{"2", 2.0, "enabled", "paused", "Paused", "opted_in"} {
		for name, tc := range map[string]struct {
			id   string
			body map[string]any
			l    live
			want growth.Class
		}{
			"a campaign's status": {campaigns, ops(update(campaign1, "status", map[string]any{"status": v})),
				live{campaign1: state{"status": "PAUSED"}}, spend},
			"a keyword's status": {criteria, ops(update(crit, "status", map[string]any{"status": v})),
				live{crit: state{"negative": false}}, spend},
			"an exclusion's status": {"googleads.customers.campaignCriteria.mutate",
				ops(update(excl, "status", map[string]any{"status": v})), live{excl: state{"negative": true}}, spend},
			"a subscription made": {subs, ops(map[string]any{"create": map[string]any{"type": "KEYWORD",
				"status": v}}), live{}, recommendationClass("KEYWORD")},
			"a subscription set": {subs, ops(update("customers/1234567890/recommendationSubscriptions/KEYWORD",
				"status", map[string]any{"status": v})), live{}, recommendationClass("KEYWORD")},
			"a budget's delivery": {budgets, ops(update(budgetA, "deliveryMethod",
				map[string]any{"deliveryMethod": v})), live{budgetA: state{"delivery_method": "STANDARD"}}, spend},
			"a campaign's reach": {campaigns, ops(update(campaign1, "geoTargetTypeSetting.positiveGeoTargetType",
				map[string]any{"geoTargetTypeSetting": map[string]any{"positiveGeoTargetType": v}})),
				live{campaign1: state{}}, spend},
			"asset automation": {campaigns, ops(update(campaign1, "assetAutomationSettings",
				map[string]any{"assetAutomationSettings": []any{map[string]any{
					"assetAutomationType": "TEXT_ASSET_AUTOMATION", "assetAutomationStatus": v}}})),
				live{campaign1: state{}}, public},
		} {
			if got := classOf(t, tc.id, tc.body, tc.l); got != tc.want {
				t.Errorf("%s set to %#v is %s, want %s", name, v, got, tc.want)
			}
		}
		a := account{today: time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC),
			budgets:   map[string]budget{budgetA: {daily: 1000}},
			campaigns: map[string]campaign{campaign1: {status: "PAUSED", budget: budgetA}}}
		after, err := a.applying([]operation{{service: "campaign", kind: opUpdate, name: campaign1,
			object: map[string]any{"status": v}, mask: []string{"status"}}})
		if err != nil || after.daily() != 1000 {
			t.Errorf("a campaign set to %#v adds %d to the day (%v), want its 1000", v, after.daily(), err)
		}
	}
	// The names that lower still lower.
	for name, tc := range map[string]struct {
		id   string
		body map[string]any
		l    live
	}{
		"a campaign paused": {campaigns, ops(update(campaign1, "status", map[string]any{"status": "PAUSED"})),
			live{campaign1: state{"status": "ENABLED"}}},
		"a keyword paused": {criteria, ops(update(crit, "status", map[string]any{"status": "PAUSED"})),
			live{crit: state{"negative": false}}},
		"an exclusion enabled": {"googleads.customers.campaignCriteria.mutate",
			ops(update(excl, "status", map[string]any{"status": "ENABLED"})), live{excl: state{"negative": true}}},
		"a subscription paused": {subs, ops(update("customers/1234567890/recommendationSubscriptions/KEYWORD",
			"status", map[string]any{"status": "PAUSED"})), live{}},
		"automation opted out": {campaigns, ops(update(campaign1, "assetAutomationSettings",
			map[string]any{"assetAutomationSettings": []any{map[string]any{
				"assetAutomationType": "TEXT_ASSET_AUTOMATION", "assetAutomationStatus": "OPTED_OUT"}}})),
			live{campaign1: state{}}},
	} {
		if got := classOf(t, tc.id, tc.body, tc.l); got != operate {
			t.Errorf("%s is %s, want operate", name, got)
		}
	}
}

// A CRITERION'S NEGATIVE IS WHAT IT IS LIVE, never what an update sends:
// Google keeps the negative a criterion was made with and ignores one outside
// its mask, so a targeting keyword paused in a body that calls it negative is
// still a pause, and one enabled is still spend.
//
// Mutation: read negative from the update's body first, and a keyword enabled
// beside "negative": true is made at once as an exclusion enabled.
func TestACriterionsNegativeIsWhatItIsLive(t *testing.T) {
	t.Parallel()
	crit := "customers/1234567890/adGroupCriteria/5~6"
	positive := live{crit: state{"negative": false}}
	enable := ops(update(crit, "status", map[string]any{"status": "ENABLED", "negative": true}))
	if got := classOf(t, criteria, enable, positive); got != spend {
		t.Errorf("a keyword enabled in a body calling it negative is %s, want spend", got)
	}
	pause := ops(update(crit, "status", map[string]any{"status": "PAUSED", "negative": true}))
	if got := classOf(t, criteria, pause, positive); got != operate {
		t.Errorf("a keyword paused in a body calling it negative is %s, want operate", got)
	}
	if got := classOf(t, criteria, enable, live{}); got != spend {
		t.Errorf("a criterion nobody read, enabled in a body calling it negative, is %s, want spend", got)
	}
}

// A VALUE SET WHERE NONE WAS NARROWS ONLY WHERE NONE MEANS NO LIMIT: a
// ceiling, a maximize-conversions target CPA or a target ROAS added is
// operate, while a keyword's own bid where it inherits one, a modifier where
// none was, a floor added, and a portfolio campaign given its own target are
// spend.
//
// Mutation: read every value added where none was as a cap, or compare a
// portfolio campaign's scheme by its type alone, and each spend is operate.
func TestAValueSetWhereNoneWasNarrowsOnlyWhereNoneMeansNoLimit(t *testing.T) {
	t.Parallel()
	crit := "customers/1234567890/adGroupCriteria/5~6"
	standard := live{campaign1: state{"bidding_strategy_type": "TARGET_CPA", "target_cpa": map[string]any{}}}
	for name, tc := range map[string]struct {
		id   string
		body map[string]any
		l    live
		want growth.Class
	}{
		"a keyword's own bid": {criteria, ops(update(crit, "cpcBidMicros", map[string]any{"cpcBidMicros": "1"})),
			live{crit: state{"negative": false}}, spend},
		"a keyword's modifier": {criteria, ops(update(crit, "bidModifier", map[string]any{"bidModifier": 0.5})),
			live{crit: state{"negative": false}}, spend},
		"a floor": {campaigns, ops(update(campaign1, "targetCpa.cpcBidFloorMicros",
			map[string]any{"targetCpa": map[string]any{"cpcBidFloorMicros": "1"}})), standard, spend},
		"a ceiling": {campaigns, ops(update(campaign1, "targetCpa.cpcBidCeilingMicros",
			map[string]any{"targetCpa": map[string]any{"cpcBidCeilingMicros": "1"}})), standard, operate},
		"a target CPA on maximize conversions": {campaigns, ops(update(campaign1,
			"maximizeConversions.targetCpaMicros", map[string]any{"maximizeConversions": map[string]any{
				"targetCpaMicros": "1"}})), live{campaign1: state{"bidding_strategy_type": "MAXIMIZE_CONVERSIONS"}},
			operate},
		"a target ROAS": {campaigns, ops(update(campaign1, "targetRoas.targetRoas",
			map[string]any{"targetRoas": map[string]any{"targetRoas": 4.0}})),
			live{campaign1: state{"bidding_strategy_type": "TARGET_ROAS"}}, operate},
		"a portfolio campaign's own lower target": {campaigns, ops(update(campaign1, "targetCpa.targetCpaMicros",
			map[string]any{"targetCpa": map[string]any{"targetCpaMicros": "1"}})),
			live{campaign1: state{"bidding_strategy_type": "TARGET_CPA", "target_cpa": map[string]any{
				"target_cpa_micros": "5000000"}, "bidding_strategy": "customers/1234567890/biddingStrategies/3"}}, spend},
		"a standard campaign's lower target": {campaigns, ops(update(campaign1, "targetCpa.targetCpaMicros",
			map[string]any{"targetCpa": map[string]any{"targetCpaMicros": "1"}})),
			live{campaign1: state{"bidding_strategy_type": "TARGET_CPA", "target_cpa": map[string]any{
				"target_cpa_micros": "5000000"}}}, operate},
		"a portfolio on a campaign nobody read": {campaigns, ops(update(campaign1, "biddingStrategy",
			map[string]any{"biddingStrategy": "customers/1234567890/biddingStrategies/3"})), live{}, spend},
	} {
		if got := classOf(t, tc.id, tc.body, tc.l); got != tc.want {
			t.Errorf("%s set where none was is %s, want %s", name, got, tc.want)
		}
	}
}

// A CAMPAIGN CRITERION IS CLASSED BY WHAT IT TARGETS: a device's modifier at
// or below 1.0 lowers bids, while a country or any other criterion made with
// a low modifier still widens where the campaign serves.
//
// Mutation: class any criterion holding a modifier by the modifier alone, and
// a whole country is added at once as operate.
func TestACampaignCriterionIsClassedByWhatItTargets(t *testing.T) {
	t.Parallel()
	made := func(fields map[string]any) map[string]any {
		obj := map[string]any{"campaign": campaign1}
		for k, v := range fields {
			obj[k] = v
		}
		return ops(map[string]any{"create": obj})
	}
	id := "googleads.customers.campaignCriteria.mutate"
	for name, tc := range map[string]struct {
		body map[string]any
		want growth.Class
	}{
		"a country at 0.9": {made(map[string]any{"location": map[string]any{
			"geoTargetConstant": "geoTargetConstants/2840"}, "bidModifier": 0.9}), spend},
		"a language at 1.0": {made(map[string]any{"language": map[string]any{
			"languageConstant": "languageConstants/1000"}, "bidModifier": 1.0}), spend},
		"a device at 0.5":       {made(map[string]any{"device": map[string]any{"type": "MOBILE"}, "bidModifier": 0.5}), operate},
		"a device at 1.2":       {made(map[string]any{"device": map[string]any{"type": "MOBILE"}, "bidModifier": 1.2}), spend},
		"a device and a place":  {made(map[string]any{"device": map[string]any{"type": "MOBILE"}, "bidModifier": 0.5, "location": map[string]any{}}), spend},
		"a country excluded":    {made(map[string]any{"location": map[string]any{}, "negative": true}), operate},
		"a device with no rate": {made(map[string]any{"device": map[string]any{"type": "MOBILE"}}), spend},
	} {
		if got := classOf(t, id, tc.body, live{}); got != tc.want {
			t.Errorf("%s made is %s, want %s", name, got, tc.want)
		}
	}
}

// A CALL THAT SPENDS IS HELD TO THE CAPS WHATEVER ELSE IT HOLDS: a budget
// raise beside an ad group removed comes to admin and still spends, as does
// an enabled campaign beside a field no rule names, and an ad group enabled
// beside one; a pause or a removal alone spends nothing.
//
// Mutation: read spends from the call's top class alone, and the admin call
// holding a raise reaches no cap.
func TestACallThatSpendsIsHeldToTheCapsWhateverElseItHolds(t *testing.T) {
	t.Parallel()
	mutate := "googleads.customers.googleAds.mutate"
	adGroup := "customers/1234567890/adGroups/2"
	for name, tc := range map[string]struct {
		id     string
		body   map[string]any
		class  growth.Class
		spends bool
	}{
		"a raise beside a removal": {mutate, map[string]any{"mutateOperations": []any{
			map[string]any{"campaignBudgetOperation": update(budgetA, "amountMicros",
				map[string]any{"amountMicros": "900000000000"})},
			map[string]any{"adGroupOperation": map[string]any{"remove": adGroup}}}}, admin, true},
		"a campaign enabled beside a field no rule names": {campaigns, ops(update(campaign1, "status,someNewField",
			map[string]any{"status": "ENABLED", "someNewField": 1})), admin, true},
		"an ad group enabled beside a field no rule names": {"googleads.customers.adGroups.mutate",
			ops(update(adGroup, "status,someNewField", map[string]any{"status": "ENABLED", "someNewField": 1})),
			admin, true},
		"a pause":   {campaigns, ops(update(campaign1, "status", map[string]any{"status": "PAUSED"})), operate, false},
		"a removal": {campaigns, ops(map[string]any{"remove": campaign1}), admin, false},
	} {
		r := ruling(t, tc.id, tc.body, live{budgetA: state{"amount_micros": "1000000"},
			campaign1: state{"status": "PAUSED"}, adGroup: state{"status": "PAUSED"}})
		if r.class != tc.class || r.spends != tc.spends {
			t.Errorf("%s is %s spending %v, want %s spending %v", name, r.class, r.spends, tc.class, tc.spends)
		}
	}
}

// EVERY SPEND NAMES WHAT THE CAPS MEASURE OF IT: a method of one class,
// spend, says what it adds (a graduation, the budgets its mappings name),
// and no operation type a mutate carries is spend without a family that
// says what it changes; a method whose request names no amount is admin.
//
// Mutation: class applyIncentive or a batch run spend again, or drop the
// graduation's measure, and a spend counts as adding nothing.
func TestEverySpendNamesWhatTheCapsMeasureOfIt(t *testing.T) {
	t.Parallel()
	for _, op := range operations() {
		if _, ok := measures(op.ID); op.Class == spend && ruleOf[op.ID] == "" && !ok {
			t.Errorf("%s is spend and names nothing the caps measure of it", op.ID)
		}
	}
	for service, by := range services() {
		if by.family == "" && by.class == spend {
			t.Errorf("operation type %s is spend and names nothing the caps measure of it", service)
		}
	}
	experiment := "customers/1234567890/campaigns/77"
	r := ruling(t, graduateOp, map[string]any{"campaignBudgetMappings": []any{map[string]any{
		"experimentCampaign": experiment, "campaignBudget": budgetA}}}, live{})
	if !r.spends || len(r.ops) != 1 || r.ops[0].name != experiment || r.ops[0].object["campaign_budget"] != budgetA ||
		r.ops[0].object["status"] != "ENABLED" {
		t.Errorf("a graduation is measured as %+v", r)
	}
	if _, err := classify(opOf(t, graduateOp), map[string]any{}, live{}); err == nil {
		t.Error("a graduation naming no budget was classed")
	}
	for _, id := range []string{"googleads.customers.batchJobs.run", "googleads.customers.incentives.applyIncentive",
		"googleads.customers.reservations.bookCampaigns", "googleads.customers.campaignDrafts.promote",
		"googleads.customers.experiments.scheduleExperiment", "googleads.customers.experiments.promoteExperiment",
		"googleads.customers.localServicesLeads.provideLeadFeedback"} {
		if r := ruling(t, id, map[string]any{}, live{}); r.class != admin || r.spends {
			t.Errorf("%s, whose request names no amount, is %s spending %v, want admin", id, r.class, r.spends)
		}
	}
	if r := ruling(t, "googleads.customers.batchJobs.run", nil, live{}); !r.danger {
		t.Error("running a batch job, whose operations nobody can read back, is not marked as undone by nothing")
	}
}

// A RECOMMENDATION APPLIED IS HELD BY THE BUDGET IT SETS: a budget
// recommendation naming its new amount is that budget's change, raised or
// lowered against the budget as read; a spend type that sets no budget spends
// and adds nothing; one whose budget the request does not name, or that the
// engine did not read, is admin.
//
// Mutation: class a budget recommendation by its type alone, and its new
// amount reaches no cap.
func TestARecommendationAppliedIsHeldByTheBudgetItSets(t *testing.T) {
	t.Parallel()
	rec := "customers/1234567890/recommendations/abc"
	apply := "googleads.customers.recommendations.apply"
	with := func(params map[string]any) map[string]any {
		item := map[string]any{"resourceName": rec}
		for k, v := range params {
			item[k] = v
		}
		return map[string]any{"operations": []any{item}}
	}
	budgetRec := live{rec: state{"type": "CAMPAIGN_BUDGET", "campaign_budget": budgetA},
		budgetA: state{"amount_micros": "5000000"}}
	newAmount := func(v any) map[string]any {
		return with(map[string]any{"campaignBudget": map[string]any{"newBudgetAmountMicros": v}})
	}
	r := ruling(t, apply, newAmount("900000000000"), budgetRec)
	if r.class != spend || !r.spends || len(r.ops) != 1 || r.ops[0].name != budgetA ||
		r.ops[0].object["amount_micros"] != "900000000000" {
		t.Errorf("a budget recommendation raising its budget is %+v", r)
	}
	if r := ruling(t, apply, newAmount("1000000"), budgetRec); r.class != operate || r.spends || len(r.ops) != 1 {
		t.Errorf("a budget recommendation lowering its budget is %+v", r)
	}
	for name, tc := range map[string]struct {
		body   map[string]any
		l      live
		class  growth.Class
		spends bool
	}{
		"a budget recommendation at Google's amount": {with(nil), budgetRec, admin, false},
		"a budget recommendation with no budget read": {newAmount("1"),
			live{rec: state{"type": "CAMPAIGN_BUDGET"}}, admin, false},
		"a keyword": {with(nil), live{rec: state{"type": "KEYWORD"}}, spend, true},
		"a keyword naming a budget": {with(map[string]any{"campaignBudget": map[string]any{
			"newBudgetAmountMicros": "1"}}), live{rec: state{"type": "KEYWORD"}}, admin, false},
		"unused budget moved": {with(map[string]any{"moveUnusedBudget": map[string]any{"budgetMicrosToMove": "1"}}),
			live{rec: state{"type": "MOVE_UNUSED_BUDGET", "campaign_budget": budgetA}}, admin, false},
		"a type nobody names":          {with(nil), live{rec: state{"type": "SOMETHING_NEW"}}, admin, false},
		"a recommendation nobody read": {with(nil), live{}, admin, false},
		"an ad":                        {with(nil), live{rec: state{"type": "RESPONSIVE_SEARCH_AD"}}, public, false},
	} {
		if r := ruling(t, apply, tc.body, tc.l); r.class != tc.class || r.spends != tc.spends {
			t.Errorf("%s applied is %s spending %v, want %s spending %v", name, r.class, r.spends, tc.class,
				tc.spends)
		}
	}
}

// A SUBSCRIPTION IS THE CLASS OF WHAT GOOGLE MAY APPLY UNASKED: a type that
// publishes or operates is its own, a spend type that sets no budget spends,
// and one that may set a budget is admin, since Google picks the amount.
//
// Mutation: class a subscription by its type's class alone, and Google may
// raise budgets unasked on a spend approval no cap holds.
func TestASubscriptionIsTheClassOfWhatGoogleMayApplyUnasked(t *testing.T) {
	t.Parallel()
	subs := "googleads.customers.recommendationSubscriptions.mutateRecommendationSubscription"
	for kind, want := range map[string]growth.Class{"CAMPAIGN_BUDGET": admin, "TARGET_CPA_OPT_IN": admin,
		"KEYWORD": spend, "RESPONSIVE_SEARCH_AD": public, "OPTIMIZE_AD_ROTATION": operate, "SOMETHING_NEW": admin} {
		made := ops(map[string]any{"create": map[string]any{"type": kind, "status": "ENABLED"}})
		if got := classOf(t, subs, made, live{}); got != want {
			t.Errorf("a %s subscription enabled is %s, want %s", kind, got, want)
		}
	}
}

// WHAT A CONVERSION IS WORTH IS SPEND, as FR-GOAL classes it: a value rule
// made or enabled, or its value raised, and a conversion action's value
// settings, are spend; a rule paused or lowered, or an action renamed,
// operates.
//
// Mutation: class value rules or an action's value settings as FR-ENTITY
// again, and a value raised is made at once.
func TestWhatAConversionIsWorthIsSpend(t *testing.T) {
	t.Parallel()
	rule := "customers/1234567890/conversionValueRules/3"
	action := "customers/1234567890/conversionActions/4"
	rules, actions := "googleads.customers.conversionValueRules.mutate", "googleads.customers.conversionActions.mutate"
	value := func(v float64) map[string]any {
		return ops(update(rule, "action.value", map[string]any{"action": map[string]any{"value": v}}))
	}
	ruleLive := live{rule: state{"action": map[string]any{"value": 2.0}}}
	actionLive := live{action: state{"value_settings": map[string]any{"default_value": 10.0}}}
	for name, tc := range map[string]struct {
		id   string
		body map[string]any
		l    live
		want growth.Class
	}{
		"a rule made": {rules, ops(map[string]any{"create": map[string]any{"action": map[string]any{}}}), live{}, spend},
		"a rule made paused": {rules, ops(map[string]any{"create": map[string]any{"status": "PAUSED"}}), live{},
			operate},
		"a value raised":  {rules, value(3), ruleLive, spend},
		"a value lowered": {rules, value(1), ruleLive, operate},
		"a rule enabled": {rules, ops(update(rule, "status", map[string]any{"status": "ENABLED"})), ruleLive,
			spend},
		"a rule paused": {rules, ops(update(rule, "status", map[string]any{"status": "PAUSED"})), ruleLive, operate},
		"a set's rules changed": {"googleads.customers.conversionValueRuleSets.mutate",
			ops(update("customers/1234567890/conversionValueRuleSets/5", "conversionValueRules",
				map[string]any{"conversionValueRules": []any{rule}})), live{}, spend},
		"an action's default value raised": {actions, ops(update(action, "valueSettings.defaultValue",
			map[string]any{"valueSettings": map[string]any{"defaultValue": 20.0}})), actionLive, spend},
		"an action's default value lowered": {actions, ops(update(action, "valueSettings.defaultValue",
			map[string]any{"valueSettings": map[string]any{"defaultValue": 5.0}})), actionLive, operate},
		"an action always using its default": {actions, ops(update(action, "valueSettings.alwaysUseDefaultValue",
			map[string]any{"valueSettings": map[string]any{"alwaysUseDefaultValue": true}})), actionLive, spend},
		"an action made with a value": {actions, ops(map[string]any{"create": map[string]any{
			"valueSettings": map[string]any{"defaultValue": 1.0}}}), live{}, spend},
		"an action renamed": {actions, ops(update(action, "name", map[string]any{"name": "x"})), actionLive, operate},
	} {
		if got := classOf(t, tc.id, tc.body, tc.l); got != tc.want {
			t.Errorf("%s is %s, want %s", name, got, tc.want)
		}
	}
}

// AN AUTOMATION A LIST LEAVES OUT TAKES GOOGLE'S DEFAULT, which may be
// OPTED_IN: a list dropping a type that was opted out is public, while one
// keeping it opted out operates.
//
// Mutation: compare the entries a list holds alone, and dropping an opt-out
// is made at once.
func TestAnAutomationAListLeavesOutTakesGooglesDefault(t *testing.T) {
	t.Parallel()
	optedOut := live{campaign1: state{"asset_automation_settings": []any{map[string]any{
		"asset_automation_type": "TEXT_ASSET_AUTOMATION", "asset_automation_status": "OPTED_OUT"}}}}
	set := func(entries ...any) map[string]any {
		return ops(update(campaign1, "assetAutomationSettings", map[string]any{"assetAutomationSettings": entries}))
	}
	if got := classOf(t, campaigns, set(), optedOut); got != public {
		t.Errorf("a list dropping an opted-out type is %s, want public", got)
	}
	kept := map[string]any{"assetAutomationType": "TEXT_ASSET_AUTOMATION", "assetAutomationStatus": "OPTED_OUT"}
	if got := classOf(t, campaigns, set(kept), optedOut); got != operate {
		t.Errorf("a list keeping its opt-out is %s, want operate", got)
	}
}
