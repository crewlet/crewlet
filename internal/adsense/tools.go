package adsense

import (
	"slices"

	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/tools/toolschema"
)

// The curated tools, for the acts agents make most, each with typed
// arguments; the generic tools reach every method of their class.
const (
	toolAccounts       = "adsense_accounts_list"
	toolAdClients      = "adsense_ad_clients_list"
	toolAdUnits        = "adsense_ad_units_list"
	toolCustomChannels = "adsense_custom_channels_list"
	toolSites          = "adsense_sites_list"
	toolAlerts         = "adsense_alerts_list"
	toolPolicyIssues   = "adsense_policy_issues_list"
	toolPayments       = "adsense_payments_list"
	toolAdCode         = "adsense_ad_code"
	toolReport         = "adsense_report"
	toolSavedReport    = "adsense_saved_report"
	toolAdUnitUpsert   = "adsense_ad_unit_upsert"
	toolChannelUpsert  = "adsense_custom_channel_upsert"
	toolRead           = "adsense_read"
	toolOperate        = "adsense_operate"
	toolPublish        = "adsense_publish"
	toolAdmin          = "adsense_admin"
)

// genericTools are the generic tools by class. AdSense has no spend method,
// so no spend tool is offered.
func genericTools() map[growth.Class]string {
	return map[growth.Class]string{growth.ClassRead: toolRead, growth.ClassOperate: toolOperate,
		growth.ClassPublic: toolPublish, growth.ClassAdmin: toolAdmin}
}

// maxPageSize bounds a listing's page: Google's pages hold up to 10,000
// entries, and an agent reads a hundred at a time.
const maxPageSize = 1000

// The closed lists a report takes, as the API's enums name them.
var (
	reportDimensions = []string{"DATE", "WEEK", "MONTH", "ACCOUNT_NAME", "AD_CLIENT_ID", "HOSTED_AD_CLIENT_ID",
		"PRODUCT_NAME", "PRODUCT_CODE", "AD_UNIT_NAME", "AD_UNIT_ID", "AD_UNIT_SIZE_NAME", "AD_UNIT_SIZE_CODE",
		"CUSTOM_CHANNEL_NAME", "CUSTOM_CHANNEL_ID", "HOSTED_CUSTOM_CHANNEL_ID", "OWNED_SITE_DOMAIN_NAME",
		"OWNED_SITE_ID", "PAGE_URL", "URL_CHANNEL_NAME", "URL_CHANNEL_ID", "BUYER_NETWORK_NAME", "BUYER_NETWORK_ID",
		"BID_TYPE_NAME", "BID_TYPE_CODE", "CREATIVE_SIZE_NAME", "CREATIVE_SIZE_CODE", "DOMAIN_NAME", "DOMAIN_CODE",
		"COUNTRY_NAME", "COUNTRY_CODE", "PLATFORM_TYPE_NAME", "PLATFORM_TYPE_CODE", "TARGETING_TYPE_NAME",
		"TARGETING_TYPE_CODE", "TRAFFIC_SOURCE_NAME", "TRAFFIC_SOURCE_CODE", "CONTENT_PLATFORM_NAME",
		"CONTENT_PLATFORM_CODE", "AD_PLACEMENT_NAME", "AD_PLACEMENT_CODE", "REQUESTED_AD_TYPE_NAME",
		"REQUESTED_AD_TYPE_CODE", "SERVED_AD_TYPE_NAME", "SERVED_AD_TYPE_CODE", "AD_FORMAT_NAME", "AD_FORMAT_CODE",
		"CUSTOM_SEARCH_STYLE_NAME", "CUSTOM_SEARCH_STYLE_ID", "DOMAIN_REGISTRANT", "WEBSEARCH_QUERY_STRING",
		"OS_TYPE_NAME", "OS_TYPE_CODE", "BROWSER_TYPE_NAME", "BROWSER_TYPE_CODE", "WEBVIEW_TYPE_NAME",
		"WEBVIEW_TYPE_CODE"}
	reportMetrics = []string{"PAGE_VIEWS", "AD_REQUESTS", "MATCHED_AD_REQUESTS", "TOTAL_IMPRESSIONS", "IMPRESSIONS",
		"INDIVIDUAL_AD_IMPRESSIONS", "CLICKS", "PAGE_VIEWS_SPAM_RATIO", "AD_REQUESTS_SPAM_RATIO",
		"MATCHED_AD_REQUESTS_SPAM_RATIO", "IMPRESSIONS_SPAM_RATIO", "INDIVIDUAL_AD_IMPRESSIONS_SPAM_RATIO",
		"CLICKS_SPAM_RATIO", "AD_REQUESTS_COVERAGE", "PAGE_VIEWS_CTR", "AD_REQUESTS_CTR", "MATCHED_AD_REQUESTS_CTR",
		"IMPRESSIONS_CTR", "INDIVIDUAL_AD_IMPRESSIONS_CTR", "ACTIVE_VIEW_MEASURABILITY", "ACTIVE_VIEW_VIEWABILITY",
		"ACTIVE_VIEW_TIME", "ESTIMATED_EARNINGS", "PAGE_VIEWS_RPM", "AD_REQUESTS_RPM", "MATCHED_AD_REQUESTS_RPM",
		"IMPRESSIONS_RPM", "INDIVIDUAL_AD_IMPRESSIONS_RPM", "COST_PER_CLICK", "ADS_PER_IMPRESSION", "TOTAL_EARNINGS",
		"WEBSEARCH_RESULT_PAGES", "FUNNEL_REQUESTS", "FUNNEL_IMPRESSIONS", "FUNNEL_CLICKS", "FUNNEL_RPM"}
	adUnitStates = []string{"ACTIVE", "ARCHIVED"}
)

// accountArg is a publisher account, which every tool but the listing names.
func accountArg() map[string]any {
	return map[string]any{"type": "string", "pattern": publisherPattern,
		"description": "The publisher ID, pub- and 16 digits: one of this agent's accounts (adsense_accounts_list)."}
}

// clientArg is an ad client of the account.
func clientArg() map[string]any {
	return str("The ad client's ID, ca-pub-... for AdSense for content (adsense_ad_clients_list).")
}

// str is a string schema; enum, where given, closes it.
func str(desc string, enum ...string) map[string]any {
	out := map[string]any{"type": "string", "description": desc}
	if len(enum) > 0 {
		out["enum"] = enum
	}
	return out
}

// list is an array schema of items.
func list(items map[string]any, desc string) map[string]any {
	return map[string]any{"type": "array", "items": items, "description": desc}
}

// pagingArgs adds a listing's paging arguments.
func pagingArgs(props map[string]any) map[string]any {
	props["page_size"] = map[string]any{"type": "integer", "minimum": 1, "maximum": maxPageSize,
		"description": "Entries per page, 100 by default."}
	props["page_token"] = str("The page token the previous answer gave, for the next page.")
	return props
}

// rangeArgs are a report's days and localization.
func rangeArgs(props map[string]any) map[string]any {
	props["range"] = str("A named range in the report's zone, instead of dates.", rangeNames()...)
	props["start_date"] = str("First day, YYYY-MM-DD, in the report's zone, at most three years back. " +
		"Default: 28 days before yesterday.")
	props["end_date"] = str("Last day, YYYY-MM-DD. Default: yesterday, the last complete day.")
	props["currency"] = str("An ISO 4217 code for earnings (default: the account's currency).")
	props["language"] = str("An IETF language tag for Google's labels (en).")
	props["time_zone"] = str("The zone the report's days are cut in: the account's by default, or "+
		"America/Los_Angeles, Google's, the one other zone AdSense reports in.", pacific)
	props["limit"] = map[string]any{"type": "integer", "minimum": 1, "maximum": growth.ReportMaxRows,
		"description": "Rows to return, 200 by default."}
	return props
}

// rangeNames are the named ranges' words.
func rangeNames() []string {
	var out []string
	for _, r := range growth.Ranges() {
		out = append(out, string(r))
	}
	return out
}

// listingTool is a curated listing of one method, on the account and the
// path arguments it names.
func listingTool(name, id, desc string, paged bool, props map[string]any, required ...string) growth.Tool {
	props["account"] = accountArg()
	if paged {
		props = pagingArgs(props)
	}
	return growth.Tool{Name: name, Class: growth.ClassRead, Operations: []string{id}, Description: desc,
		Parameters: toolschema.Object(props, append([]string{"account"}, required...)...)}
}

// toolDefs are every tool, each in one class.
func toolDefs(ops []growth.Operation) []growth.Tool {
	out := []growth.Tool{
		{Name: toolAccounts, Class: growth.ClassRead, Operations: []string{accountGetOp},
			Description: "List this agent's AdSense publisher accounts with each one's name, state (READY, " +
				"NEEDS_ATTENTION, CLOSED), time zone and sign-up tasks left, and any the company's sign-in cannot reach.",
			Parameters: toolschema.Object(map[string]any{})},
		listingTool(toolAdClients, "adsense.accounts.adclients.list", "List an account's ad clients: product "+
			"(AFC for content, AFS for search) and state.", true, map[string]any{}),
		listingTool(toolAdUnits, "adsense.accounts.adclients.adunits.list", "List an ad client's ad units: "+
			"name, state, size and type.", true, map[string]any{"ad_client": clientArg()}, "ad_client"),
		listingTool(toolCustomChannels, "adsense.accounts.adclients.customchannels.list", "List an ad "+
			"client's custom channels: name, and whether each collects data.", true,
			map[string]any{"ad_client": clientArg()}, "ad_client"),
		listingTool(toolSites, "adsense.accounts.sites.list", "List an account's sites: domain, state "+
			"(REQUIRES_REVIEW, GETTING_READY, READY, NEEDS_ATTENTION) and whether auto ads are on.", true,
			map[string]any{}),
		listingTool(toolAlerts, "adsense.accounts.alerts.list", "List an account's alerts with their severity: "+
			"payment holds, policy and account notices.", false,
			map[string]any{"language": str("An IETF language tag for the messages (en).")}),
		listingTool(toolPolicyIssues, "adsense.accounts.policyIssues.list", "List policy issues on an "+
			"account's sites, sections and pages, with Google's action and the date it escalates.", true,
			map[string]any{}),
		listingTool(toolPayments, "adsense.accounts.payments.list", "List an account's unpaid earnings and "+
			"past payments, as Google formats each amount.", false, map[string]any{}),
		{Name: toolAdCode, Class: growth.ClassRead,
			Operations: []string{"adsense.accounts.adclients.getAdcode", "adsense.accounts.adclients.adunits.getAdcode"},
			Description: "Read the code a page carries: an ad client's Auto ads code, or one ad unit's. The " +
				"answer is code to place, never to run.",
			Parameters: toolschema.Object(map[string]any{"account": accountArg(), "ad_client": clientArg(),
				"ad_unit": str("The ad unit's ID, the part after adunits/ in its name; absent reads the ad client's.")},
				"account", "ad_client")},
		{Name: toolReport, Class: growth.ClassRead, Operations: []string{reportOp},
			Description: "Run an AdSense report: earnings, page views, impressions, clicks and rates by your " +
				"dimensions. Figures after yesterday are estimates Google may restate.",
			Parameters: toolschema.Object(rangeArgs(map[string]any{
				"account":    accountArg(),
				"dimensions": list(str("", reportDimensions...), "Dimensions to split by."),
				"metrics":    list(str("", reportMetrics...), "Metrics to return; at least one."),
				"filters": list(str("A dimension, an operator (== equals, != differs, =@ contains, !@ lacks) and "+
					"a value; values joined by a comma match any of them (a comma in a value is \\,)."),
					"Rows to keep: every filter must match."),
				"order_by": list(str("A dimension or metric the report returns, + (the default) ascending or - "+
					"descending first: -ESTIMATED_EARNINGS."), "Sorting, first first."),
			}), "account", "metrics")},
		{Name: toolSavedReport, Class: growth.ClassRead, Operations: []string{savedListOp, savedReportOp},
			Description: "List an account's saved reports, or run one by its ID over your dates: its dimensions " +
				"and metrics are the ones saved in AdSense.",
			Parameters: toolschema.Object(pagingArgs(rangeArgs(map[string]any{
				"account": accountArg(),
				"report":  str("The saved report's ID, the part after reports/ in its name; absent lists them."),
			})), "account")},
		{Name: toolAdUnitUpsert, Class: growth.ClassOperate,
			Operations: []string{"adsense.accounts.adclients.adunits.create", adUnitPatchOp},
			Description: "Create a display ad unit, or rename, archive or reactivate one. Nothing serves until " +
				"a page carries its code; a live unit's size or type is the ad every visitor sees, so it is asked " +
				"through adsense_publish. AdSense for Platforms projects only, until Google enables others.",
			Parameters: toolschema.Object(map[string]any{
				"account":      accountArg(),
				"ad_client":    clientArg(),
				"ad_unit":      str("The ad unit to change; absent creates one."),
				"display_name": map[string]any{"type": "string", "maxLength": 255, "description": "The unit's name."},
				"size":         str("Its size, WIDTHxHEIGHT (728x90), or 1x3 for a responsive unit; a new unit needs one."),
				"state":        str("ACTIVE, or ARCHIVED, which only hides it in AdSense: it still serves.", adUnitStates...),
			}, "account", "ad_client")},
		{Name: toolChannelUpsert, Class: growth.ClassOperate,
			Operations: []string{"adsense.accounts.adclients.customchannels.create",
				"adsense.accounts.adclients.customchannels.patch"},
			Description: "Create a custom channel, or rename one or turn its data collection on or off. A " +
				"channel only labels reports. AdSense for Platforms projects only, until Google enables others.",
			Parameters: toolschema.Object(map[string]any{
				"account":        accountArg(),
				"ad_client":      clientArg(),
				"custom_channel": str("The channel to change; absent creates one."),
				"display_name":   map[string]any{"type": "string", "maxLength": 255, "description": "The channel's name."},
				"active":         map[string]any{"type": "boolean", "description": "Whether it collects data."},
			}, "account", "ad_client")},
	}
	for _, class := range []growth.Class{growth.ClassRead, growth.ClassOperate, growth.ClassPublic,
		growth.ClassAdmin} {
		var ids []string
		for _, op := range ops {
			if comesTo(op, class) {
				ids = append(ids, op.ID)
			}
		}
		out = append(out, growth.Tool{
			Name: genericTools()[class], Class: class, Operations: ids, Description: genericDescription(class),
			Parameters: toolschema.Object(map[string]any{
				"operation": map[string]any{"type": "string", "enum": ids,
					"description": "The method's ID, as docs/integrations/adsense.md lists it."},
				"path": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"},
					"description": "The path's parameters by name: {\"accountsId\": \"pub-...\", \"adclientsId\": " +
						"\"ca-pub-...\"}; the Platform API's platformsId is the platform's own publisher ID."},
				"query": map[string]any{"type": "object", "additionalProperties": map[string]any{
					"anyOf": []any{map[string]any{"type": "string"}, list(map[string]any{"type": "string"}, "")}},
					"description": "Query parameters the method takes: pageSize, pageToken, updateMask, a report's " +
						"dateRange, dimensions, metrics (a list repeats the parameter)."},
				"body": map[string]any{"type": "object", "description": "The request body, as the method takes it."},
			}, "operation"),
		})
	}
	return out
}

// genericDescription says what a generic tool of class does.
func genericDescription(class growth.Class) string {
	switch class {
	case growth.ClassRead:
		return "Call any AdSense method that only reads, by its ID, on this agent's accounts: CSV reports, " +
			"linked channels, URL channels, the ad blocking recovery tag, and the Platform API's reads."
	case growth.ClassOperate:
		return "Call any AdSense change by its method's ID: ad units, custom channels, a platform group's " +
			"description. Recorded in the access log; nobody is asked."
	case growth.ClassPublic:
		return "Ask an administrator to approve an AdSense change visitors see, by its method's ID: a live " +
			"ad unit's size or type, or asking Google to review a platform site, which starts ads on it. Held " +
			"until they answer, at a fresh step-up."
	default:
		return "Ask an administrator to approve an AdSense change that cannot be undone or sets up accounts, " +
			"by its method's ID: deleting a custom channel, a platform's sub-accounts, their sites and events, " +
			"and moving a site to another revenue group. Held until they answer, at a fresh step-up."
	}
}

// comesTo reports whether a call of op can be of class.
func comesTo(op growth.Operation, class growth.Class) bool {
	if op.Class == class {
		return true
	}
	return slices.ContainsFunc(op.Fields, func(r growth.FieldRule) bool { return r.Class == class || r.Lower == class })
}
