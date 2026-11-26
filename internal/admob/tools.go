package admob

import (
	"slices"

	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/tools/toolschema"
)

// The curated tools, for the acts agents make most, each with typed
// arguments; the generic tools reach every method of their class.
const (
	toolAccounts        = "admob_accounts_list"
	toolApps            = "admob_apps_list"
	toolAdUnits         = "admob_ad_units_list"
	toolMediationGroups = "admob_mediation_groups_list"
	toolNetworkReport   = "admob_network_report"
	toolMediationReport = "admob_mediation_report"
	toolCampaignReport  = "admob_campaign_report"
	toolAppCreate       = "admob_app_create"
	toolAdUnitCreate    = "admob_ad_unit_create"
	toolMappingCreate   = "admob_ad_unit_mapping_create"
	toolGroupUpsert     = "admob_mediation_group_upsert"
	toolABExperiment    = "admob_ab_experiment"
	toolRead            = "admob_read"
	toolOperate         = "admob_operate"
	toolAdmin           = "admob_admin"
)

// genericTools are the generic tools by class. AdMob has no public or spend
// method, so neither tool is offered.
func genericTools() map[growth.Class]string {
	return map[growth.Class]string{growth.ClassRead: toolRead, growth.ClassOperate: toolOperate,
		growth.ClassAdmin: toolAdmin}
}

// Report bounds: Google returns at most 100,000 rows, and a campaign report
// spans at most 30 days.
const (
	maxBatchMappings = 100
	maxCampaignDays  = 30
)

// The closed lists a report takes, as the API's enums name them.
var (
	networkDimensions = []string{"DATE", "MONTH", "WEEK", "AD_UNIT", "APP", "AD_TYPE", "COUNTRY", "FORMAT",
		"PLATFORM", "MOBILE_OS_VERSION", "GMA_SDK_VERSION", "APP_VERSION_NAME", "SERVING_RESTRICTION"}
	networkMetrics = []string{"AD_REQUESTS", "CLICKS", "ESTIMATED_EARNINGS", "IMPRESSIONS", "IMPRESSION_CTR",
		"IMPRESSION_RPM", "MATCHED_REQUESTS", "MATCH_RATE", "SHOW_RATE"}
	mediationDimensions = []string{"DATE", "MONTH", "WEEK", "AD_SOURCE", "AD_SOURCE_INSTANCE", "AD_UNIT", "APP",
		"MEDIATION_GROUP", "COUNTRY", "FORMAT", "PLATFORM", "MOBILE_OS_VERSION", "GMA_SDK_VERSION",
		"APP_VERSION_NAME", "SERVING_RESTRICTION"}
	mediationMetrics = []string{"AD_REQUESTS", "CLICKS", "ESTIMATED_EARNINGS", "IMPRESSIONS", "IMPRESSION_CTR",
		"MATCHED_REQUESTS", "MATCH_RATE", "OBSERVED_ECPM", "AD_LOAD_LATENCY"}
	campaignDimensions = []string{"DATE", "CAMPAIGN_ID", "CAMPAIGN_NAME", "AD_ID", "AD_NAME", "PLACEMENT_ID",
		"PLACEMENT_NAME", "PLACEMENT_PLATFORM", "COUNTRY", "FORMAT"}
	campaignMetrics = []string{"IMPRESSIONS", "CLICKS", "CLICK_THROUGH_RATE", "INSTALLS", "ESTIMATED_COST",
		"AVERAGE_CPI", "INTERACTIONS"}
	adFormats = []string{"APP_OPEN", "BANNER", "INTERSTITIAL", "NATIVE", "REWARDED", "REWARDED_INTERSTITIAL"}
	appStores = []string{"GOOGLE_PLAY_APP_STORE", "AMAZON_APP_STORE", "OPPO_APP_STORE", "SAMSUNG_APP_STORE",
		"VIVO_APP_STORE", "XIAOMI_APP_STORE"}
)

// accountArg is a publisher account, which every tool but the listing names.
func accountArg() map[string]any {
	return map[string]any{"type": "string", "pattern": publisherPattern,
		"description": "The publisher ID, pub- and 16 digits: one of this agent's accounts (admob_accounts_list)."}
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

// paging are a listing's paging arguments.
func paging(props map[string]any) map[string]any {
	props["page_size"] = map[string]any{"type": "integer", "minimum": 1, "maximum": maxPageSize,
		"description": "Entries per page, 100 by default."}
	props["page_token"] = str("The page token the previous answer gave, for the next page.")
	return props
}

// reportArgs are a report tool's arguments over its closed dimensions and
// metrics; network and mediation reports also take filters, sorting and a
// currency.
func reportArgs(dimensions, metrics []string, streamed bool) map[string]any {
	props := map[string]any{
		"account":    accountArg(),
		"range":      str("A named range in the report's zone, instead of dates.", rangeNames()...),
		"start_date": str("First day, YYYY-MM-DD, in the report's zone. Default: 28 days before yesterday."),
		"end_date":   str("Last day, YYYY-MM-DD. Default: yesterday, the last complete day."),
		"dimensions": list(str("", dimensions...), "Dimensions to split by."),
		"metrics":    list(str("", metrics...), "Metrics to return; at least one."),
		"language":   str("An IETF language tag for localized labels (en-US)."),
		"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": growth.ReportMaxRows,
			"description": "Rows to return, 200 by default."},
	}
	if streamed {
		props["dimension_filters"] = list(toolschema.Object(map[string]any{
			"dimension":   str("", dimensions...),
			"matches_any": list(str(""), "Values the dimension matches, any of them."),
		}, "dimension", "matches_any"), "Rows to keep, by dimension values.")
		props["sort"] = list(toolschema.Object(map[string]any{
			"dimension": str("Sort by this dimension; or give metric.", dimensions...),
			"metric":    str("Sort by this metric; or give dimension.", metrics...),
			"order":     str("", "ASCENDING", "DESCENDING"),
		}, "order"), "Sorting, first condition first.")
		props["currency"] = str("An ISO 4217 code for earnings (default: the account's currency).")
		props["time_zone"] = str("The zone the report's days are cut in: the account's reporting zone by "+
			"default, or America/Los_Angeles, the one other zone Google accepts.", pacific)
	}
	return toolschema.Object(props, "account", "metrics")
}

// rangeNames are the named ranges' words.
func rangeNames() []string {
	var out []string
	for _, r := range growth.Ranges() {
		out = append(out, string(r))
	}
	return out
}

// toolDefs are every tool, each in one class.
func toolDefs(ops []growth.Operation) []growth.Tool {
	out := []growth.Tool{
		{Name: toolAccounts, Class: growth.ClassRead, Operations: []string{"admob.accounts.get"},
			Description: "List this agent's AdMob publisher accounts with each one's currency and reporting time " +
				"zone, and any the company's sign-in cannot reach.",
			Parameters: toolschema.Object(map[string]any{})},
		{Name: toolApps, Class: growth.ClassRead, Operations: []string{"admob.accounts.apps.list"},
			Description: "List an account's apps: platform, store link or manual name, and approval state.",
			Parameters:  toolschema.Object(paging(map[string]any{"account": accountArg()}), "account")},
		{Name: toolAdUnits, Class: growth.ClassRead, Operations: []string{"admob.accounts.adUnits.list"},
			Description: "List an account's ad units: app, format, media types and reward settings.",
			Parameters:  toolschema.Object(paging(map[string]any{"account": accountArg()}), "account")},
		{Name: toolMediationGroups, Class: growth.ClassRead, Operations: []string{"admob.accounts.mediationGroups.list"},
			Description: "List an account's mediation groups with their targeting, lines and A/B experiment state.",
			Parameters: toolschema.Object(paging(map[string]any{"account": accountArg(),
				"filter": str("Google's filter: IN(FIELD, \"v\", ...) or CONTAINS_ANY(FIELD, ...), joined by " +
					"AND, over AD_SOURCE_IDS, AD_UNIT_IDS, APP_IDS, DISPLAY_NAME, FORMAT, MEDIATION_GROUP_ID, " +
					"PLATFORM, STATE and TARGETED_REGION_CODES.")}), "account")},
		{Name: toolNetworkReport, Class: growth.ClassRead, Operations: []string{networkReportOp},
			Description: "Run an AdMob Network report: earnings, requests, impressions and clicks of the AdMob " +
				"Network by your dimensions. Figures after yesterday are estimates Google may restate.",
			Parameters: reportArgs(networkDimensions, networkMetrics, true)},
		{Name: toolMediationReport, Class: growth.ClassRead, Operations: []string{mediationReportOp},
			Description: "Run an AdMob Mediation report: every ad source's earnings, eCPM and latency by your " +
				"dimensions. Figures after yesterday are estimates.",
			Parameters: reportArgs(mediationDimensions, mediationMetrics, true)},
		{Name: toolCampaignReport, Class: growth.ClassRead, Operations: []string{campaignReportOp},
			Description: "Run an AdMob campaign report over at most 30 days: house and cross-promotion " +
				"campaigns' impressions, clicks, installs and estimated cost.",
			Parameters: reportArgs(campaignDimensions, campaignMetrics, false)},
		{Name: toolAppCreate, Class: growth.ClassOperate, Operations: []string{"admob.accounts.apps.create"},
			Description: "Add an app to an account by the name you give it, not linked to a store. Google " +
				"reviews it; no API removes it. Linking an app to its store listing cannot be undone, so it is " +
				"asked through admob_admin.",
			Parameters: toolschema.Object(map[string]any{
				"account":            accountArg(),
				"platform":           str("", "IOS", "ANDROID"),
				"display_name":       map[string]any{"type": "string", "maxLength": 80, "description": "The app's name in AdMob."},
				"android_app_stores": list(str("", appStores...), "Android stores the app is on, for an ANDROID app."),
			}, "account", "platform", "display_name")},
		{Name: toolAdUnitCreate, Class: growth.ClassOperate, Operations: []string{"admob.accounts.adUnits.create"},
			Description: "Create an ad unit for an app. Nothing serves until the app ships its ID; no API " +
				"removes it, and archiving it in AdMob is permanent.",
			Parameters: toolschema.Object(map[string]any{
				"account":       accountArg(),
				"app_id":        str("The app's ID, ca-app-pub-...~..., from admob_apps_list."),
				"display_name":  map[string]any{"type": "string", "maxLength": 80, "description": "The ad unit's name."},
				"ad_format":     str("", adFormats...),
				"ad_types":      list(str("", "RICH_MEDIA", "VIDEO"), "Media the unit serves."),
				"reward_amount": str("The reward's amount, for a REWARDED unit."),
				"reward_item":   str("The reward's item name, for a REWARDED unit."),
			}, "account", "app_id", "display_name", "ad_format")},
		{Name: toolMappingCreate, Class: growth.ClassOperate,
			Operations: []string{"admob.accounts.adUnits.adUnitMappings.create", "admob.accounts.adUnitMappings.batchCreate"},
			Description: "Map ad units to a third-party ad source's adapter, one or up to 100 at once. A " +
				"mapping serves only once a mediation line names it; no API removes it.",
			Parameters: toolschema.Object(map[string]any{
				"account": accountArg(),
				"mappings": map[string]any{"type": "array", "minItems": 1, "maxItems": maxBatchMappings,
					"items": toolschema.Object(map[string]any{
						"ad_unit":      str("The ad unit's ID fragment, the part after adUnits/ in its name."),
						"adapter_id":   str("The adapter's ID (admob_read adSources.adapters.list)."),
						"display_name": str("The mapping's name."),
						"configurations": map[string]any{"type": "object",
							"additionalProperties": map[string]any{"type": "string"},
							"description":          "The adapter's configuration keys and the network's values for them."},
					}, "ad_unit", "adapter_id", "configurations"),
					"description": "The mappings to create."},
			}, "account", "mappings")},
		{Name: toolGroupUpsert, Class: growth.ClassOperate,
			Operations: []string{"admob.accounts.mediationGroups.create", "admob.accounts.mediationGroups.patch"},
			Description: "Create a mediation group, or change one: its lines' ad sources, eCPM, mode and " +
				"state, and its ad units. Removing a line cannot be undone, so it is asked through admob_admin.",
			Parameters: toolschema.Object(map[string]any{
				"account":            accountArg(),
				"mediation_group_id": str("The group to change; absent creates one."),
				"update_mask": str("For a change: the fields it sets, comma-separated, as Google names them " +
					"(mediation_group_lines[\"123\"].state, targeting.ad_unit_ids)."),
				"mediation_group": map[string]any{"type": "object",
					"description": "The MediationGroup as the API takes it: new lines keyed by distinct negative " +
						"IDs, each line's id its key, and no experimentVariant, which Google alone sets."},
			}, "account", "mediation_group")},
		{Name: toolABExperiment, Class: growth.ClassOperate,
			Operations: []string{"admob.accounts.mediationGroups.mediationAbExperiments.create",
				"admob.accounts.mediationGroups.mediationAbExperiments.stop"},
			Description: "Start a mediation A/B experiment on a group, or stop the running one keeping the " +
				"control lines (VARIANT_CHOICE_A). Keeping the treatment's (VARIANT_CHOICE_B) removes every control " +
				"line it lacks, so it is asked through admob_admin.",
			Parameters: toolschema.Object(map[string]any{
				"account":            accountArg(),
				"mediation_group_id": str("The mediation group."),
				"action":             str("", "start", "stop"),
				"display_name":       str("The experiment's name, to start one."),
				"treatment_traffic_percentage": map[string]any{"type": "integer", "minimum": 1, "maximum": 99,
					"description": "The share of traffic the treatment gets, to start one."},
				"treatment_mediation_lines": list(map[string]any{"type": "object"},
					"The treatment's lines, each {\"mediationGroupLine\": {...}}, to start one."),
				"variant_choice": str("The variant to keep, to stop one.", "VARIANT_CHOICE_A", "VARIANT_CHOICE_B"),
			}, "account", "mediation_group_id", "action")},
	}
	for _, class := range []growth.Class{growth.ClassRead, growth.ClassOperate, growth.ClassAdmin} {
		var ids []string
		for _, op := range ops {
			if comesTo(op, class) {
				ids = append(ids, op.ID)
			}
		}
		out = append(out, growth.Tool{
			Name: genericTools()[class], Class: class, Operations: ids, Description: genericDescription(class), Covers: genericCovers(class),
			Parameters: toolschema.Object(map[string]any{
				"operation": map[string]any{"type": "string", "enum": ids,
					"description": "The method's ID, as docs/integrations/admob.md lists it."},
				"path": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"},
					"description": "The path's parameters by name: {\"accountsId\": \"pub-...\", \"mediationGroupsId\": \"123\"}."},
				"query": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"},
					"description": "Query parameters: pageSize, pageToken, filter, updateMask."},
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
		return "Call any AdMob method that only reads, by its ID, on this agent's accounts."
	case growth.ClassOperate:
		return "Call any AdMob change by its method's ID: apps, ad units, mappings, mediation groups and " +
			"A/B experiments. Recorded in the access log; nobody is asked."
	default:
		return "Ask an administrator to approve an AdMob change that cannot be undone, by its method's ID: " +
			"linking a new app to its store listing, removing a mediation line, or stopping an A/B experiment " +
			"keeping the treatment's lines. Held until they answer, at a fresh step-up."
	}
}

// genericCovers is what a generic tool of class above read changes, which the
// tools below it name (growth.Catalog.Above).
func genericCovers(class growth.Class) string {
	switch class {
	case growth.ClassRead:
		return ""
	case growth.ClassOperate:
		return "apps, ad units, mappings, mediation groups and A/B experiments"
	default:
		return "linking an app to its store listing, removing a mediation line and stopping an A/B experiment"
	}
}

// comesTo reports whether a call of op can be of class.
func comesTo(op growth.Operation, class growth.Class) bool {
	if op.Class == class {
		return true
	}
	return slices.ContainsFunc(op.Fields, func(r growth.FieldRule) bool { return r.Class == class || r.Lower == class })
}
