package googleads

import (
	"strings"
	"testing"

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

// noStaging is a batch job store holding nothing.
func noStaging() (growth.Class, bool) { return "", false }

// classOf is what a call of id with body comes to against l.
func classOf(t *testing.T, id string, body map[string]any, l live) growth.Class {
	t.Helper()
	r, err := classify(opOf(t, id), body, l, noStaging)
	if err != nil {
		t.Fatalf("%s %v: %v", id, body, err)
	}
	return r.class
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

// A CAMPAIGN MOVED TO A LARGER BUDGET SPENDS, to an equal or smaller one
// operates, and to one the engine did not read spends; switching how it bids
// spends whatever else changes.
func TestABudgetMoveAndABiddingSwitchAreClassedByWhatTheyChange(t *testing.T) {
	t.Parallel()
	move := ops(update(campaign1, "campaignBudget", map[string]any{"campaignBudget": budgetB}))
	base := func(b string) live {
		return live{campaign1: state{"campaign_budget": budgetA, "bidding_strategy_type": "MANUAL_CPC"},
			budgetA: state{"amount_micros": "5000000"}, budgetB: state{"amount_micros": b}}
	}
	if got := classOf(t, campaigns, move, base("9000000")); got != spend {
		t.Errorf("a move to a larger budget is %s", got)
	}
	if got := classOf(t, campaigns, move, base("1000000")); got != operate {
		t.Errorf("a move to a smaller budget is %s", got)
	}
	unread := base("1000000")
	delete(unread, budgetB)
	if got := classOf(t, campaigns, move, unread); got != spend {
		t.Errorf("a move to a budget nobody read is %s", got)
	}
	switched := ops(update(campaign1, "targetCpa.targetCpaMicros",
		map[string]any{"targetCpa": map[string]any{"targetCpaMicros": "1"}}))
	if got := classOf(t, campaigns, switched, base("1")); got != spend {
		t.Errorf("a switch from manual CPC to target CPA is %s", got)
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
	if _, err := classify(opOf(t, mutate), unknown, live{}, noStaging); err == nil ||
		!strings.Contains(err.Error(), "mystery") {
		t.Errorf("an operation type nobody classes answered %v", err)
	}
}

// RUNNING A BATCH JOB IS WHAT IT STAGED, and one the engine holds no staging
// of is refused rather than guessed.
func TestRunningABatchJobIsWhatItStaged(t *testing.T) {
	t.Parallel()
	run := opOf(t, "googleads.customers.batchJobs.run")
	if _, err := classify(run, nil, live{}, noStaging); err == nil {
		t.Error("a batch job with no staging recorded was classed")
	}
	r, err := classify(run, nil, live{}, func() (growth.Class, bool) { return spend, true })
	if err != nil || r.class != spend {
		t.Errorf("a batch job that staged spend is %v, %v", r.class, err)
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
