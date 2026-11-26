package googleads

import (
	"slices"

	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/tools/toolschema"
)

// The curated tools, for the acts agents make most, each with typed
// arguments; the generic tools reach every other method.
const (
	toolListAccounts = "google_ads_list_accounts"
	toolQuery        = "google_ads_query"
	toolPause        = "google_ads_pause"
	toolSetBudget    = "google_ads_set_budget"
	toolRead         = "google_ads_read"
	toolOperate      = "google_ads_operate"
	toolPublish      = "google_ads_publish"
	toolSpend        = "google_ads_spend"
	toolAdmin        = "google_ads_admin"
)

// genericTools are the generic tools by class.
func genericTools() map[growth.Class]string {
	return map[growth.Class]string{
		growth.ClassRead: toolRead, growth.ClassOperate: toolOperate, growth.ClassPublic: toolPublish,
		growth.ClassSpend: toolSpend, growth.ClassAdmin: toolAdmin,
	}
}

// customerArg is a customer's ID, which every account-level tool names.
func customerArg() map[string]any {
	return map[string]any{"type": "string", "pattern": "^[0-9]{10}$",
		"description": "The 10-digit customer ID, without hyphens, one of this agent's accounts (google_ads_list_accounts)."}
}

// pausable are the collections google_ads_pause pauses resources of, as a
// resource name writes them, each through its own service's mutate
// (pauseOf).
var pausable = []string{"campaigns", "adGroups", "adGroupAds", "adGroupCriteria", "assetGroups", "campaignCriteria",
	"adGroupAssets", "campaignAssets", "customerAssets", "assetGroupAssets"}

// pauseOf is the mutate a pausable collection's resources are paused through.
func pauseOf(collection string) string { return "googleads.customers." + collection + ".mutate" }

// toolDefs are every tool, each in one class.
func toolDefs(ops []growth.Operation) []growth.Tool {
	out := []growth.Tool{
		{Name: toolListAccounts, Class: growth.ClassRead,
			Operations:  []string{"googleads.customers.listAccessibleCustomers", searchOp},
			Description: "List the Google Ads accounts this agent may reach, with each one's name, currency, time zone and status.",
			Parameters:  toolschema.Object(map[string]any{})},
		{Name: toolQuery, Class: growth.ClassRead, Operations: []string{searchOp},
			Description: "Run a Google Ads Query Language (GAQL) query on one account: campaigns, ad groups, ads, keywords, " +
				"search terms, assets and their metrics over a date range.",
			Parameters: toolschema.Object(map[string]any{
				"customer": customerArg(),
				"query": map[string]any{"type": "string", "description": "The GAQL query, such as SELECT campaign.name, " +
					"metrics.clicks FROM campaign WHERE segments.date DURING LAST_7_DAYS."},
				"page_token": map[string]any{"type": "string", "description": "The next page's token from a previous answer."},
			}, "customer", "query")},
		{Name: toolPause, Class: growth.ClassOperate, Operations: pauseOperations(),
			Description: "Pause campaigns, ad groups, ads, keywords, criteria, asset groups or asset links by resource name. " +
				"Pausing stops spend and is undone by enabling, which an administrator approves.",
			Parameters: toolschema.Object(map[string]any{
				"resources": map[string]any{"type": "array", "minItems": 1, "maxItems": 100,
					"items":       map[string]any{"type": "string", "pattern": "^customers/[0-9]{10}/[A-Za-z]+/[A-Za-z0-9_~-]+$"},
					"description": "Resource names to pause, all in one account, such as customers/1234567890/campaigns/111."},
			}, "resources")},
		{Name: toolSetBudget, Class: growth.ClassSpend, Operations: []string{"googleads.customers.campaignBudgets.mutate"},
			Description: "Ask an administrator to set a campaign budget's daily amount. Held to the account's spending caps " +
				"before anyone is asked and again when it is made.",
			Parameters: toolschema.Object(map[string]any{
				"budget": map[string]any{"type": "string", "pattern": "^customers/[0-9]{10}/campaignBudgets/[0-9]+$",
					"description": "The campaign budget's resource name."},
				"amount": map[string]any{"type": "string", "pattern": `^[0-9]+(\.[0-9]{1,6})?$`,
					"description": "The new daily amount in the account's currency, such as 25.50."},
			}, "budget", "amount")},
	}
	for _, class := range []growth.Class{growth.ClassRead, growth.ClassOperate, growth.ClassPublic, growth.ClassSpend,
		growth.ClassAdmin} {
		var ids []string
		for _, op := range ops {
			if comesTo(op, class) {
				ids = append(ids, op.ID)
			}
		}
		out = append(out, growth.Tool{
			Name: genericTools()[class], Class: class, Operations: ids,
			Description: genericDescription(class),
			Parameters: toolschema.Object(map[string]any{
				"operation": map[string]any{"type": "string", "enum": ids,
					"description": "The method's ID, as docs/integrations/google-ads.md lists it."},
				"path": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"},
					"description": "The path's parameters by name, such as {\"customersId\": \"1234567890\"}."},
				"query": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"},
					"description": "Query parameters, where the method takes any."},
				"body": map[string]any{"type": "object", "description": "The request body, as the method takes it in REST's " +
					"JSON (camelCase), its operations each a create, an update with its updateMask, or a remove."},
			}, "operation"),
		})
	}
	return out
}

// pauseOperations are the mutates google_ads_pause sends.
func pauseOperations() []string {
	out := make([]string, len(pausable))
	for i, collection := range pausable {
		out[i] = pauseOf(collection)
	}
	slices.Sort(out)
	return out
}

// genericDescription says what a generic tool of class does.
func genericDescription(class growth.Class) string {
	switch class {
	case growth.ClassRead:
		return "Call any Google Ads method that only reads, by its ID, on this agent's accounts."
	case growth.ClassOperate:
		return "Make any Google Ads change that spends nothing more, by its method's ID: pausing, lowering budgets and " +
			"bids, negatives, labels, audiences, conversions. Each operation is classed against its target as it reads " +
			"now; recorded in the access log, nobody is asked."
	case growth.ClassPublic:
		return "Ask an administrator to approve a Google Ads change that publishes or retracts ad text or assets, by " +
			"its method's ID; held until they answer."
	case growth.ClassSpend:
		return "Ask an administrator to approve a Google Ads change that can raise spend (enabling, raising a budget or " +
			"bid, widening reach), held to the account's caps before anyone is asked and again when it is made."
	default:
		return "Ask an administrator to approve a Google Ads removal, user, link, billing or account change, by its " +
			"method's ID; held until they answer, at a fresh step-up."
	}
}

// comesTo reports whether a call of op can be of class.
func comesTo(op growth.Operation, class growth.Class) bool {
	return op.Class == class || slices.Contains(op.Classes, class)
}
