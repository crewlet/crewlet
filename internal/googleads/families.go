package googleads

import (
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/growth"
)

// The classes, short, for the tables below.
const (
	read    = growth.ClassRead
	operate = growth.ClassOperate
	public  = growth.ClassPublic
	spend   = growth.ClassSpend
	admin   = growth.ClassAdmin
)

// trackingFields are what a click's URL carries, which no customer reads.
var trackingFields = []string{"tracking_url_template", "final_url_suffix", "url_custom_parameters"}

// biddingSchemes are the bidding strategy oneofs a campaign or a portfolio
// holds; switching from one to another is spend.
var biddingSchemes = []string{"bidding_strategy", "commission", "manual_cpa", "manual_cpc", "manual_cpm",
	"manual_cpv", "maximize_conversions", "maximize_conversion_value", "target_cpa", "target_cpc",
	"target_cpm", "target_cpv", "fixed_cpm", "target_impression_share", "target_roas", "target_spend",
	"percent_cpc", "enhanced_cpc"}

// biddingRules are a bidding scheme's own fields, within one scheme: a raised
// target or ceiling, a cleared cap or a lowered target ROAS bids more.
func biddingRules(prefix string) []fieldRule {
	out := raises(prefix+"target_cpa.target_cpa_micros", prefix+"maximize_conversions.target_cpa_micros",
		prefix+"target_cpc.target_cpc_micros", prefix+"target_spend.target_spend_micros",
		prefix+"target_spend.cpc_bid_ceiling_micros", prefix+"target_cpa.cpc_bid_ceiling_micros",
		prefix+"target_cpa.cpc_bid_floor_micros", prefix+"target_roas.cpc_bid_ceiling_micros",
		prefix+"target_roas.cpc_bid_floor_micros", prefix+"target_impression_share.cpc_bid_ceiling_micros",
		prefix+"commission.commission_rate_micros", prefix+"target_impression_share.location_fraction_micros",
		prefix+"target_roas_tolerance_percent_millis")
	out = append(out, compared(lower, prefix+"target_roas.target_roas", prefix+"maximize_conversion_value.target_roas")...)
	out = append(out, fieldRule{path: prefix + "target_impression_share.location", how: toward, hi: spend, lo: operate,
		values: []string{"TOP_OF_PAGE", "ABSOLUTE_TOP_OF_PAGE"}})
	return append(out, turnsOn(prefix+"manual_cpc.enhanced_cpc_enabled")...)
}

// schemeSwitched is spend where an update sets a bidding scheme other than
// the one the target runs: a new strategy bids by other rules.
func schemeSwitched(typeField string) func(operation, live) growth.Class {
	return func(o operation, l live) growth.Class {
		running := strings.ToLower(str(l[o.name][typeField]))
		for _, path := range o.mask {
			scheme, _, _ := strings.Cut(path, ".")
			if slices.Contains(biddingSchemes, scheme) && (running == "" || scheme != running) &&
				(scheme != "bidding_strategy" || running != "") {
				return spend
			}
		}
		return read
	}
}

// campaignFamily is FR-CAMPAIGN.
func campaignFamily() family {
	switched := schemeSwitched("bidding_strategy_type")
	f := family{create: createStatus(spend), remove: fixed(admin), other: admin,
		update: func(o operation, l live) growth.Class { return higher(switched(o, l), budgetRepointed(o, l)) }}
	f.fields = append(f.fields, statusRule(spend),
		fieldRule{path: "campaign_budget", how: always, hi: read}, // decided by budgetRepointed
		fieldRule{path: "geo_target_type_setting.positive_geo_target_type", how: toward, hi: spend, lo: operate,
			values: []string{"PRESENCE_OR_INTEREST"}},
		fieldRule{path: "geo_target_type_setting.negative_geo_target_type", how: toward, hi: spend, lo: operate,
			values: []string{"PRESENCE"}},
		fieldRule{path: "keyword_match_type", how: toward, hi: spend, lo: operate, values: []string{"BROAD"}},
		fieldRule{path: "start_date_time", how: earlier, hi: spend, lo: operate},
		fieldRule{path: "end_date_time", how: later, hi: spend, lo: operate},
		fieldRule{path: "frequency_caps", how: grows, hi: spend, lo: operate},
		fieldRule{path: "targeting_setting", how: always, hi: spend},
		fieldRule{path: "dynamic_search_ads_setting.use_supplied_urls_only", how: off, hi: spend, lo: operate},
		fieldRule{path: "asset_automation_settings", how: optsIn, hi: public, lo: operate},
	)
	f.fields = append(f.fields, biddingRules("")...)
	f.fields = append(f.fields, turnsOn("network_settings.target_google_search", "network_settings.target_search_network",
		"network_settings.target_content_network", "network_settings.target_partner_search_network",
		"network_settings.target_youtube", "network_settings.target_google_tv_network",
		"ai_max_setting.enable_ai_max", "shopping_setting.enable_local",
		"shopping_setting.ignore_brand_exclusion_in_shopping_ads", "real_time_bidding_setting.opt_in")...)
	f.fields = append(f.fields, raises("shopping_setting.campaign_priority")...)
	// A channel or inventory added, or a category bid raised: what was added
	// is not told from what was narrowed without reading every list, so spend.
	f.fields = append(f.fields, classed(spend, "demand_gen_campaign_settings", "video_campaign_settings",
		"pmax_campaign_settings", "travel_campaign_settings", "hotel_setting", "app_campaign_setting",
		"local_campaign_setting", "local_services_campaign_settings", "payment_mode")...)
	f.fields = append(f.fields, classed(public, "brand_guidelines", "text_guidelines", "vanity_pharma",
		"excluded_parent_asset_field_types", "excluded_parent_asset_set_types")...)
	f.fields = append(f.fields, classed(admin, "contains_eu_political_advertising",
		"third_party_integration_partners")...)
	f.fields = append(f.fields, operates(append([]string{"name", "campaign_group", "ad_serving_optimization_status",
		"optimization_goal_setting", "selective_optimization", "view_through_conversion_optimization_enabled"},
		trackingFields...)...)...)
	return f
}

// budgetRepointed is a campaign moved to another budget: spend where the new
// budget is larger than the one it leaves, or either was not read.
func budgetRepointed(o operation, l live) growth.Class {
	if !slices.Contains(o.mask, "campaign_budget") {
		return read
	}
	from, to := l[str(l[o.name]["campaign_budget"])], l[str(o.object["campaign_budget"])]
	if from == nil || to == nil {
		return spend
	}
	return fieldRule{how: raise, hi: spend, lo: operate}.classOf(from["amount_micros"], to["amount_micros"], false)
}

// adGroupFamily is FR-AD-GROUP.
func adGroupFamily() family {
	f := family{create: createStatus(spend), remove: fixed(admin), other: admin}
	f.fields = append(f.fields, statusRule(spend))
	f.fields = append(f.fields, raises("cpc_bid_micros", "cpm_bid_micros", "cpv_bid_micros", "target_cpa_micros",
		"target_cpm_micros", "percent_cpc_bid_micros", "fixed_cpm_micros", "target_cpv_micros", "target_cpc_micros")...)
	f.fields = append(f.fields, compared(lower, "target_roas")...)
	f.fields = append(f.fields, turnsOn("optimized_targeting_enabled", "vertical_ads_format_setting.enable_booking_links",
		"vertical_ads_format_setting.enable_vertical_promotion_ads")...)
	f.fields = append(f.fields, compared(off, "exclude_demographic_expansion",
		"ai_max_ad_group_setting.disable_search_term_matching", "vertical_ads_format_setting.disable_text_ads")...)
	f.fields = append(f.fields, classed(spend, "targeting_setting", "demand_gen_ad_group_settings")...)
	f.fields = append(f.fields, classed(public, "excluded_parent_asset_field_types", "excluded_parent_asset_set_types")...)
	f.fields = append(f.fields, operates(append([]string{"name", "ad_rotation_mode", "display_custom_bid_dimension",
		"video_ad_group_settings"}, trackingFields...)...)...)
	return f
}

// negative reports whether a criterion excludes: what it sets, else what it
// is live; ok is false where neither says.
func negative(o operation, l live) (neg, ok bool) {
	if v, set := o.object["negative"]; set {
		return truthy(v), true
	}
	if s := l[o.name]; s != nil {
		return truthy(s["negative"]), true
	}
	return false, false
}

// criterionStatus is a criterion's status set: an exclusion enabled narrows,
// paused widens; a targeting criterion the other way round.
func criterionStatus(o operation, l live) growth.Class {
	if !slices.Contains(o.mask, "status") {
		return read
	}
	neg, ok := negative(o, l)
	enabling := str(o.object["status"]) == "ENABLED"
	switch {
	case !ok:
		return spend
	case neg:
		return pick(enabling, operate, spend)
	}
	return pick(enabling, spend, operate)
}

// adGroupCriterionFamily is FR-AD-GROUP-CRITERION.
func adGroupCriterionFamily() family {
	f := family{other: admin, update: criterionStatus}
	f.create = func(o operation, _ live) growth.Class {
		if lg, ok := o.object["listing_group"].(map[string]any); ok {
			return pick(str(lg["type"]) == "UNIT" && truthy(o.object["negative"]), operate, spend)
		}
		// A negative excludes; a positive keyword or targeting criterion widens
		// unless observation-only, which the engine does not confirm.
		return pick(truthy(o.object["negative"]), operate, spend)
	}
	f.remove = func(o operation, l live) growth.Class {
		neg, ok := negative(o, l)
		return pick(ok && neg, spend, admin)
	}
	f.fields = append(f.fields, raises("cpc_bid_micros", "cpm_bid_micros", "cpv_bid_micros", "percent_cpc_bid_micros",
		"bid_modifier")...)
	f.fields = append(f.fields, classed(public, "final_urls", "final_mobile_urls")...)
	f.fields = append(f.fields, operates(trackingFields...)...)
	f.fields = append(f.fields, fieldRule{path: "status", how: always, hi: read}) // decided by criterionStatus
	return f
}

// widensWhenRemoved are the campaign criterion types whose removal can widen
// targeting to everything: the last one of its dimension gone.
var widensWhenRemoved = []string{"LOCATION", "PROXIMITY", "LOCATION_GROUP", "LANGUAGE", "AD_SCHEDULE", "AUDIENCE",
	"LISTING_SCOPE", "BRAND_LIST"}

// campaignCriterionFamily is FR-CAMPAIGN-CRITERION.
func campaignCriterionFamily() family {
	f := family{other: admin, update: criterionStatus}
	f.create = func(o operation, _ live) growth.Class {
		if truthy(o.object["negative"]) {
			return operate
		}
		if m, ok := o.object["bid_modifier"]; ok && len(o.object) <= 3 {
			// A device or other criterion holding only a bid modifier.
			return pick(above(m, 1), spend, operate)
		}
		return spend // a second criterion of a dimension widens it
	}
	f.remove = func(o operation, l live) growth.Class {
		s := l[o.name]
		switch {
		case s == nil:
			return admin
		case truthy(s["negative"]), slices.Contains(widensWhenRemoved, str(s["type"])):
			return spend
		}
		return admin
	}
	f.fields = append(f.fields, raises("bid_modifier")...)
	f.fields = append(f.fields, fieldRule{path: "status", how: always, hi: read})
	return f
}

// above reports whether a number is above x; unreadable is above, the
// higher class.
func above(v any, x float64) bool {
	n, ok := number(v)
	if !ok {
		return true
	}
	f, _ := n.Float64()
	return f > x
}

// budgetFamily is FR-BUDGET.
func budgetFamily() family {
	f := family{create: fixed(operate), remove: fixed(admin), other: admin}
	f.fields = append(f.fields, raises("amount_micros", "total_amount_micros")...)
	f.fields = append(f.fields, fieldRule{path: "delivery_method", how: toward, hi: spend, lo: operate,
		values: []string{"ACCELERATED"}})
	f.fields = append(f.fields, classed(spend, "budget_network_settings")...)
	f.fields = append(f.fields, operates("name", "explicitly_shared", "aligned_bidding_strategy_id")...)
	return f
}

// biddingStrategyFamily is FR-BIDDING-STRATEGY.
func biddingStrategyFamily() family {
	f := family{create: fixed(operate), remove: fixed(admin), other: admin, update: schemeSwitched("type")}
	f.fields = append(f.fields, biddingRules("")...)
	f.fields = append(f.fields, operates("name", "aligned_campaign_budget_id", "currency_code")...)
	return f
}

// bidModifierFamily is FR-BID-MODIFIER: a modifier above 1.0 bids more, and
// removing one below 1.0 raises bids to 1.0x.
func bidModifierFamily() family {
	return family{
		create: func(o operation, _ live) growth.Class {
			return pick(above(o.object["bid_modifier"], 1), spend, operate)
		},
		remove: func(o operation, l live) growth.Class {
			s := l[o.name]
			return pick(s == nil || !above(s["bid_modifier"], 0.999999), spend, operate)
		},
		fields: raises("bid_modifier"), other: spend,
	}
}

// seasonalityFamily is FR-SEASONALITY.
func seasonalityFamily() family {
	return family{
		create: func(o operation, _ live) growth.Class {
			return pick(above(o.object["conversion_rate_modifier"], 1), spend, operate)
		},
		remove: func(o operation, l live) growth.Class {
			s := l[o.name]
			return pick(s != nil && !above(s["conversion_rate_modifier"], 0.999999), spend, admin)
		},
		fields: append(raises("conversion_rate_modifier"), operates("name", "description")...),
		// A longer window or more campaigns or channels in scope bids up more.
		other: spend,
	}
}

// adGroupAdFamily is FR-AD-GROUP-AD: enabling an ad spends and publishes its
// text, spend being the higher; removing one retracts what was published.
func adGroupAdFamily() family {
	f := family{create: createStatus(spend), remove: fixed(public), other: public}
	f.fields = append(f.fields, statusRule(spend),
		fieldRule{path: "start_date_time", how: earlier, hi: spend, lo: operate},
		fieldRule{path: "end_date_time", how: later, hi: spend, lo: operate},
		fieldRule{path: "ad_group_ad_asset_automation_settings", how: optsIn, hi: public, lo: operate})
	f.fields = append(f.fields, classed(public, "policy_validation_parameter", "ad")...)
	return f
}

// adFamily is FR-AD (updates only): an ad's content is what customers read.
func adFamily() family {
	f := family{create: fixed(public), remove: fixed(public), other: public}
	f.fields = append(f.fields, operates(append([]string{"name"}, trackingFields...)...)...)
	return f
}

// assetFamily is FR-ASSET: an asset made is shown by nothing until linked; one
// changed may be serving, which the engine does not confirm, so public.
func assetFamily() family {
	return family{create: fixed(operate), remove: fixed(admin), other: public,
		fields: operates(append([]string{"name"}, trackingFields...)...)}
}

// assetLinkFamily is FR-ASSET-LINK: a link enabled starts showing its asset.
func assetLinkFamily() family {
	return family{create: createStatus(public), remove: fixed(public), other: public,
		fields: []fieldRule{statusRule(public)}}
}

// assetGroupFamily is FR-ASSET-GROUP.
func assetGroupFamily() family {
	f := family{create: createStatus(spend), remove: fixed(admin), other: admin}
	f.fields = append(f.fields, statusRule(spend))
	f.fields = append(f.fields, classed(public, "final_urls", "final_mobile_urls", "path1", "path2",
		"google_local_services_info")...)
	f.fields = append(f.fields, operates(append([]string{"name"}, trackingFields...)...)...)
	return f
}

// assetGroupSignalFamily is FR-ASSET-GROUP-SIGNAL.
func assetGroupSignalFamily() family {
	return family{
		create: func(o operation, _ live) growth.Class {
			switch {
			case o.exempt:
				return public
			case o.object["search_theme"] != nil, o.object["local_services_id"] != nil:
				return spend
			}
			return operate
		},
		remove: fixed(operate), other: spend,
	}
}

// listingIncluded reports whether a listing group brings products in.
func listingIncluded(m map[string]any) bool { return str(m["type"]) != "UNIT_EXCLUDED" }

// listingGroupFamily is FR-LISTING-GROUP.
func listingGroupFamily() family {
	return family{
		create: func(o operation, _ live) growth.Class { return pick(listingIncluded(o.object), spend, operate) },
		remove: func(o operation, l live) growth.Class {
			s := l[o.name]
			return pick(s == nil || !listingIncluded(s), spend, operate)
		},
		update: func(o operation, _ live) growth.Class { return pick(listingIncluded(o.object), spend, operate) },
		other:  operate,
	}
}

// pageFeed reports whether an asset set link or member is a page feed's, read
// off the asset set the engine read; ok is false where it read none.
func pageFeed(o operation, l live) (feed, ok bool) {
	for _, key := range []string{"asset_set"} {
		if name := str(o.object[key]); name != "" {
			if s := l[name]; s != nil {
				return str(s["type"]) == "PAGE_FEED", true
			}
		}
	}
	if s := l[o.name]; s != nil {
		if set, ok := s["asset_set_type"]; ok {
			return str(set) == "PAGE_FEED", true
		}
	}
	return false, false
}

// assetSetLinkFamily is FR-ASSET-SET-LINK and FR-ASSET-SET-MEMBER: a page feed
// widens where its URLs serve, every other set shows its items.
func assetSetLinkFamily() family {
	return family{
		create: func(o operation, l live) growth.Class {
			feed, ok := pageFeed(o, l)
			return pick(!ok || feed, spend, public)
		},
		remove: func(o operation, l live) growth.Class {
			feed, ok := pageFeed(o, l)
			return pick(ok && feed, operate, public)
		},
		other: public,
	}
}

// restrictionFamily is a list that narrows reach: adding narrows (operate),
// removing widens (spend). FR-SHARED-SET, -SHARED-SET-LINK and -NEGATIVE.
func restrictionFamily() family {
	return family{create: fixed(operate), remove: fixed(spend), other: operate}
}

// negativeListFamily is FR-NEGATIVE-LIST: a brand in an inclusion list works
// the other way, so a brand entry is spend both ways.
func negativeListFamily() family {
	return family{
		create: func(o operation, _ live) growth.Class { return pick(o.object["brand"] != nil, spend, operate) },
		remove: fixed(spend), other: operate,
	}
}

// entityFamily is FR-ENTITY: configuration made and changed is operate, and
// removed for good is admin.
func entityFamily() family {
	return family{create: fixed(operate), remove: fixed(admin), other: operate}
}

// goalFamily is FR-GOAL: a higher value makes value bidding bid more.
func goalFamily() family {
	valued := func(m map[string]any) bool {
		for _, key := range []string{"value_multiplier", "additional_value", "high_lifetime_value_multiplier",
			"additional_high_lifetime_value", "target_option", "enable_bid_adjustments_for_loyalty_members"} {
			if containsKey(m, key) {
				return true
			}
		}
		return false
	}
	f := family{remove: fixed(operate), other: spend}
	f.create = func(o operation, _ live) growth.Class {
		switch {
		case valued(o.object):
			return spend
		case containsKey(o.object, "show_targeted_loyalty_member_benefits_in_pla"):
			return public
		}
		return operate
	}
	for _, scope := range []string{"new_customer_acquisition_goal_settings", "retention_goal_settings",
		"loyalty_retention_goal_settings", "value_settings_override"} {
		f.fields = append(f.fields, raises(scope+".value_multiplier", scope+".additional_value",
			scope+".high_lifetime_value_multiplier", scope+".additional_high_lifetime_value")...)
	}
	f.fields = append(f.fields, turnsOn("enable_bid_adjustments_for_loyalty_members")...)
	f.fields = append(f.fields, fieldRule{path: "show_targeted_loyalty_member_benefits_in_pla", how: on, hi: public,
		lo: operate})
	return f
}

// containsKey reports whether key is set anywhere in m.
func containsKey(m map[string]any, key string) bool {
	for k, v := range m {
		if k == key {
			return true
		}
		if inner, ok := v.(map[string]any); ok && containsKey(inner, key) {
			return true
		}
	}
	return false
}

// customerFamily is FR-CUSTOMER (updates only).
func customerFamily() family {
	f := family{create: fixed(admin), remove: fixed(admin), other: admin}
	f.fields = append(f.fields, operates("descriptive_name", "tracking_url_template", "final_url_suffix",
		"auto_tagging_enabled", "call_reporting_setting")...)
	f.fields = append(f.fields, fieldRule{path: "video_brand_safety_suitability", how: toward, hi: spend, lo: operate,
		values: []string{"EXPANDED_INVENTORY", "STANDARD_INVENTORY"}})
	f.fields = append(f.fields, classed(admin, "local_services_settings.redemption_provider_info",
		"video_customer.third_party_integration_partners")...)
	return f
}

// accountBudgetFamily is FR-ACCOUNT-BUDGET: a proposal creates, raises or
// extends a spending limit (spend), lowers or shortens it (operate), or ends
// or removes one (admin).
func accountBudgetFamily() family {
	return family{
		create: func(o operation, l live) growth.Class {
			switch str(o.object["proposal_type"]) {
			case "CREATE":
				return spend
			case "END", "REMOVE":
				return admin
			case "UPDATE":
				current := l[str(o.object["account_budget"])]
				if current == nil || str(o.object["proposed_spending_limit_type"]) == "INFINITE" ||
					str(o.object["proposed_end_time_type"]) == "FOREVER" {
					return spend
				}
				limit := fieldRule{how: raise, hi: spend, lo: operate}.classOf(
					current["approved_spending_limit_micros"], o.object["proposed_spending_limit_micros"], false)
				end := fieldRule{how: later, hi: spend, lo: operate}.classOf(
					current["approved_end_date_time"], o.object["proposed_end_date_time"], false)
				start := pick(o.object["proposed_start_date_time"] != nil, spend, operate)
				return higher(higher(limit, end), start)
			}
			return admin
		},
		remove: fixed(admin), other: admin,
	}
}

// subscriptionFamily is FR-REC-SUBSCRIPTION: an enabled subscription lets
// Google make its type's changes unasked, so it is that type's class.
func subscriptionFamily() family {
	byStatus := func(o operation, _ live) growth.Class {
		if str(o.object["status"]) != "ENABLED" {
			return operate
		}
		return recommendationClass(str(o.object["type"]))
	}
	return family{create: byStatus, update: byStatus, remove: fixed(operate), other: spend}
}

// recommendationClass is a recommendation type's class (FR-RECOMMENDATION); a
// type the table does not name is spend, the highest it allows.
func recommendationClass(kind string) growth.Class {
	switch {
	case slices.Contains(publicRecommendations, kind):
		return public
	case slices.Contains(operateRecommendations, kind):
		return operate
	}
	return spend
}

// The recommendation types that publish or only operate; every other is spend.
var (
	publicRecommendations = []string{"TEXT_AD", "RESPONSIVE_SEARCH_AD", "RESPONSIVE_SEARCH_AD_ASSET",
		"RESPONSIVE_SEARCH_AD_IMPROVE_AD_STRENGTH", "IMPROVE_PERFORMANCE_MAX_AD_STRENGTH",
		"IMPROVE_DEMAND_GEN_AD_STRENGTH", "CALLOUT_ASSET", "SITELINK_ASSET", "CALL_ASSET", "LEAD_FORM_ASSET",
		"DYNAMIC_IMAGE_EXTENSION_OPT_IN"}
	operateRecommendations = []string{"OPTIMIZE_AD_ROTATION", "CAMPAIGN_SPECIFIC_APP_GOAL",
		"REFRESH_CUSTOMER_MATCH_LIST", "IMPROVE_GOOGLE_TAG_COVERAGE", "SHOPPING_ADD_AGE_GROUP", "SHOPPING_ADD_COLOR",
		"SHOPPING_ADD_GENDER", "SHOPPING_ADD_GTIN", "SHOPPING_ADD_MORE_IDENTIFIERS", "SHOPPING_ADD_SIZE",
		"SHOPPING_FIX_DISAPPROVED_PRODUCTS", "SHOPPING_FIX_SUSPENDED_MERCHANT_CENTER_ACCOUNT",
		"SHOPPING_FIX_MERCHANT_CENTER_ACCOUNT_SUSPENSION_WARNING"}
)

// families is every rule family by its matrix name.
func families() map[string]family {
	return map[string]family{
		"FR-CAMPAIGN": campaignFamily(), "FR-AD-GROUP": adGroupFamily(),
		"FR-AD-GROUP-CRITERION": adGroupCriterionFamily(), "FR-CAMPAIGN-CRITERION": campaignCriterionFamily(),
		"FR-BUDGET": budgetFamily(), "FR-BIDDING-STRATEGY": biddingStrategyFamily(),
		"FR-BID-MODIFIER": bidModifierFamily(), "FR-SEASONALITY": seasonalityFamily(),
		"FR-AD-GROUP-AD": adGroupAdFamily(), "FR-AD": adFamily(), "FR-ASSET": assetFamily(),
		"FR-ASSET-LINK": assetLinkFamily(), "FR-ASSET-GROUP": assetGroupFamily(),
		"FR-ASSET-GROUP-SIGNAL": assetGroupSignalFamily(), "FR-LISTING-GROUP": listingGroupFamily(),
		"FR-ASSET-SET":      {create: fixed(operate), remove: fixed(admin), other: public},
		"FR-ASSET-SET-LINK": assetSetLinkFamily(), "FR-ASSET-SET-MEMBER": assetSetLinkFamily(),
		"FR-SHARED-SET": restrictionFamily(), "FR-SHARED-SET-LINK": restrictionFamily(),
		"FR-NEGATIVE": restrictionFamily(), "FR-NEGATIVE-LIST": negativeListFamily(),
		"FR-ENTITY": entityFamily(), "FR-GOAL": goalFamily(), "FR-CUSTOMER": customerFamily(),
		"FR-ACCOUNT-BUDGET": accountBudgetFamily(), "FR-REC-SUBSCRIPTION": subscriptionFamily(),
		// Budget network settings compare with the account's effective
		// defaults, which the engine does not read: spend.
		"FR-BUDGET-NETWORK": {create: fixed(spend), remove: fixed(spend), other: spend},
	}
}
