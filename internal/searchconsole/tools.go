package searchconsole

import (
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/tools/toolschema"
)

// The curated tools, for the acts agents make most, each with typed
// arguments; the generic tools reach every other method.
const (
	toolSitesList         = "search_console_sites_list"
	toolSearchAnalytics   = "search_console_search_analytics"
	toolSitemapsList      = "search_console_sitemaps_list"
	toolInspectURL        = "search_console_inspect_url"
	toolVerificationToken = "search_console_verification_token"
	toolSitemapSubmit     = "search_console_sitemap_submit"
	toolIndexNotify       = "search_console_index_notify"
	toolVerifySite        = "search_console_verify_site"
	toolOwnersUpdate      = "search_console_owners_update"
	toolRead              = "search_console_read"
	toolOperate           = "search_console_operate"
	toolAdmin             = "search_console_admin"
)

// genericTools are the generic tools by class: no Search Console method
// spends or publishes, so neither class has one.
func genericTools() map[growth.Class]string {
	return map[growth.Class]string{growth.ClassRead: toolRead, growth.ClassOperate: toolOperate,
		growth.ClassAdmin: toolAdmin}
}

// siteArg is a property, which every site-level tool names.
func siteArg() map[string]any {
	return map[string]any{"type": "string", "description": "The Search Console property, one of this agent's " +
		"sites (search_console_sites_list), as Search Console writes it: https://www.example.com/ or " +
		"sc-domain:example.com."}
}

// urlArg is a page's absolute URL.
func urlArg(desc string) map[string]any {
	return map[string]any{"type": "string", "pattern": "^https?://", "description": desc}
}

// stringEnum is a string from values.
func stringEnum(desc string, values ...string) map[string]any {
	return map[string]any{"type": "string", "enum": values, "description": desc}
}

// stringList is an array of strings.
func stringList(desc string) map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": desc}
}

// verificationMethods are the ways Site Verification checks a token, by the
// kind of property: a URL prefix's, then a domain's.
var verificationMethods = []string{"META", "FILE", "ANALYTICS", "TAG_MANAGER", "DNS_TXT", "DNS_CNAME", "DNS"}

// toolDefs are every tool, each in one class.
func toolDefs(ops []growth.Operation) []growth.Tool {
	method := stringEnum("How Google checks the token: META, FILE, ANALYTICS or TAG_MANAGER for a URL "+
		"prefix; DNS_TXT, DNS_CNAME or DNS for a domain.", verificationMethods...)
	out := []growth.Tool{
		{Name: toolSitesList, Class: growth.ClassRead, Operations: []string{"webmasters.sites.list"},
			Description: "List the Search Console properties this agent may reach, with its identity's " +
				"permission on each, and the ones of its sites its identity is not added to yet.",
			Parameters: toolschema.Object(map[string]any{})},
		{Name: toolSearchAnalytics, Class: growth.ClassRead, Operations: []string{"webmasters.searchanalytics.query"},
			Description: "Query a property's Google Search performance: clicks, impressions, CTR and " +
				"position, grouped by query, page, country, device, date or search appearance, in Pacific time.",
			Parameters: toolschema.Object(map[string]any{
				"site": siteArg(),
				"range": stringEnum("A named range instead of dates, cut in Pacific time; last_28_days by default.",
					rangeNames()...),
				"start_date": map[string]any{"type": "string", "pattern": `^\d{4}-\d{2}-\d{2}$`,
					"description": "First day, YYYY-MM-DD in Pacific time; with end_date, instead of range."},
				"end_date": map[string]any{"type": "string", "pattern": `^\d{4}-\d{2}-\d{2}$`,
					"description": "Last day, YYYY-MM-DD in Pacific time."},
				"dimensions": stringList("What to group by, in order: date, hour, query, page, country, device, " +
					"searchAppearance."),
				"type": map[string]any{"type": "string", "description": "The search type: web (the default), " +
					"image, video, news, discover or googleNews."},
				"data_state": map[string]any{"type": "string", "description": "final (the default) for " +
					"finalized days only, all to include fresh data, hourly_all with the hour dimension."},
				"aggregation_type": map[string]any{"type": "string", "description": "auto (the default), " +
					"byProperty or byPage."},
				"dimension_filter_groups": map[string]any{"type": "array", "items": map[string]any{"type": "object"},
					"description": "Filter groups as the Search Analytics API takes them."},
				"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": growth.ReportMaxRows,
					"description": "Rows to return, 200 by default."},
				"start_row": map[string]any{"type": "integer", "minimum": 0,
					"description": "Rows to skip, for the next page."},
			}, "site")},
		{Name: toolSitemapsList, Class: growth.ClassRead, Operations: []string{"webmasters.sitemaps.list"},
			Description: "List a property's sitemaps with their errors, warnings and submitted counts.",
			Parameters: toolschema.Object(map[string]any{"site": siteArg(),
				"sitemap_index": urlArg("A sitemap index file, to list its children instead.")}, "site")},
		{Name: toolInspectURL, Class: growth.ClassRead, Operations: []string{"searchconsole.urlInspection.index.inspect"},
			Description: "Inspect one page as Google has indexed it: verdict, coverage, crawl, canonicals, " +
				"rich results. It cannot test the live page or request indexing.",
			Parameters: toolschema.Object(map[string]any{"site": siteArg(),
				"url":      urlArg("The page, under the property."),
				"language": map[string]any{"type": "string", "description": "An IETF language tag for Google's issue text."},
			}, "site", "url")},
		{Name: toolVerificationToken, Class: growth.ClassRead, Operations: []string{"siteVerification.webResource.getToken"},
			Description: "Get the token that proves this agent's Google identity owns a property, and where " +
				"to place it; then search_console_verify_site.",
			Parameters: toolschema.Object(map[string]any{"site": siteArg(), "method": method}, "site", "method")},
		{Name: toolSitemapSubmit, Class: growth.ClassOperate, Operations: []string{"webmasters.sitemaps.submit"},
			Description: "Submit a sitemap to a property, or submit it again so Google fetches it again. " +
				"Recorded; nobody is asked.",
			Parameters: toolschema.Object(map[string]any{"site": siteArg(),
				"sitemap": urlArg("The sitemap's URL.")}, "site", "sitemap")},
		{Name: toolIndexNotify, Class: growth.ClassOperate, Operations: []string{"indexing.urlNotifications.publish"},
			Description: "Tell Google a job posting or livestream page was updated or removed. Google allows it " +
				"only for pages carrying JobPosting, or BroadcastEvent in VideoObject, structured data, which " +
				"the engine reads first.",
			Parameters: toolschema.Object(map[string]any{
				"url": urlArg("The page, under one of this agent's sites."),
				"type": stringEnum("URL_UPDATED (the default) for a new or changed page; URL_DELETED for one "+
					"that answers 404 or 410 or carries noindex.", "URL_UPDATED", "URL_DELETED"),
			}, "url")},
		{Name: toolVerifySite, Class: growth.ClassAdmin, Operations: []string{"siteVerification.webResource.insert"},
			Description: "Ask an administrator to let this agent's Google identity verify ownership of a " +
				"property, once its token is placed (search_console_verification_token); held until they answer.",
			Parameters: toolschema.Object(map[string]any{"site": siteArg(), "method": method}, "site", "method")},
		{Name: toolOwnersUpdate, Class: growth.ClassAdmin, Operations: []string{"siteVerification.webResource.update"},
			Description: "Ask an administrator to add or remove owners of a verified property; held until " +
				"they answer. Removing one's own ownership is search_console_admin's webResource.delete.",
			Parameters: toolschema.Object(map[string]any{"site": siteArg(),
				"add":    stringList("Emails to make owners."),
				"remove": stringList("Emails to remove as owners; only owners without their own token."),
			}, "site")},
	}
	for _, class := range []growth.Class{growth.ClassRead, growth.ClassOperate, growth.ClassAdmin} {
		var ids []string
		for _, op := range ops {
			if op.Class == class {
				ids = append(ids, op.ID)
			}
		}
		out = append(out, growth.Tool{
			Name: genericTools()[class], Class: class, Operations: ids,
			Description: genericDescription(class), Covers: genericCovers(class),
			Parameters: toolschema.Object(map[string]any{
				"operation": map[string]any{"type": "string", "enum": ids,
					"description": "The method's ID, as docs/integrations/google-search-console.md lists it."},
				"path": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"},
					"description": "The path's parameters by name: siteUrl (a property), feedpath (a sitemap's " +
						"URL), id (a verified resource, as Site Verification names it)."},
				"query": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"},
					"description": "Query parameters, such as sitemapIndex, verificationMethod or url."},
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
		return "Call any Search Console, Site Verification or Indexing API method that only reads, by its ID, " +
			"on this agent's sites."
	case growth.ClassOperate:
		return "Call any reversible Search Console change by its method's ID: submitting or removing a " +
			"sitemap, an Indexing API notification. Recorded in the access log; nobody is asked."
	default:
		return "Ask an administrator to approve adding or removing a property, verifying one, or changing " +
			"or relinquishing its owners, by its method's ID; held until they answer, at a fresh step-up."
	}
}

// genericCovers is what a generic tool of class above read changes, which the
// tools below it name (growth.Catalog.Above).
func genericCovers(class growth.Class) string {
	switch class {
	case growth.ClassRead:
		return ""
	case growth.ClassOperate:
		return "sitemaps and Indexing API notifications"
	default:
		return "adding, removing and verifying a property, and its owners"
	}
}

// rangeNames are the named ranges a report may give.
func rangeNames() []string {
	out := make([]string, 0, len(growth.Ranges()))
	for _, r := range growth.Ranges() {
		out = append(out, string(r))
	}
	return out
}
