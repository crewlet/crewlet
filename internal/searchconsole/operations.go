package searchconsole

import "github.com/crewlet/crewlet/internal/growth"

// The role sets a method's least role admits, each with every role above it.
var (
	anyUser      = []string{roleUnverified, roleRestricted, roleFull, roleOwner, roleVerifiedOwner}
	restrictedUp = []string{roleRestricted, roleFull, roleOwner, roleVerifiedOwner}
	fullUp       = []string{roleFull, roleOwner, roleVerifiedOwner}
	owners       = []string{roleOwner, roleVerifiedOwner}
	verified     = []string{roleVerifiedOwner}
)

// operations is every method of the Search Console, Site Verification and
// Indexing APIs, each with its class, the roles that may call it and the scope
// the engine mints for it. docs/integrations/google-search-console.md lists
// the same rows, held to this table by TestTheDocsMatrixIsTheCatalog.
func operations() []growth.Operation {
	const sites = searchAPI + "/webmasters/v3/sites"
	const resources = verifyAPI + "/siteVerification/v1/webResource"
	return []growth.Operation{
		{ID: "webmasters.searchanalytics.query", Method: "POST", Path: sites + "/{siteUrl}/searchAnalytics/query",
			Resource: "searchanalytics", Class: growth.ClassRead, Reads: true, Roles: restrictedUp,
			Permissions: []string{scopeReadonly}},
		{ID: "webmasters.sitemaps.list", Method: "GET", Path: sites + "/{siteUrl}/sitemaps", Resource: "sitemaps",
			Class: growth.ClassRead, Roles: restrictedUp, Permissions: []string{scopeReadonly}},
		{ID: "webmasters.sitemaps.get", Method: "GET", Path: sites + "/{siteUrl}/sitemaps/{feedpath}",
			Resource: "sitemaps", Class: growth.ClassRead, Roles: restrictedUp, Permissions: []string{scopeReadonly}},
		{ID: "webmasters.sitemaps.submit", Method: "PUT", Path: sites + "/{siteUrl}/sitemaps/{feedpath}",
			Resource: "sitemaps", Class: growth.ClassOperate, Roles: fullUp, Permissions: []string{scopeManage}},
		{ID: "webmasters.sitemaps.delete", Method: "DELETE", Path: sites + "/{siteUrl}/sitemaps/{feedpath}",
			Resource: "sitemaps", Class: growth.ClassOperate, Roles: fullUp, Permissions: []string{scopeManage},
			Reason: "It takes the sitemap off the Sitemaps report only; Google keeps crawling it, and " +
				"submitting it again restores it."},
		{ID: "webmasters.sites.list", Method: "GET", Path: sites, Resource: "sites", Class: growth.ClassRead,
			Permissions: []string{scopeReadonly}},
		{ID: "webmasters.sites.get", Method: "GET", Path: sites + "/{siteUrl}", Resource: "sites",
			Class: growth.ClassRead, Roles: anyUser, Permissions: []string{scopeReadonly}},
		{ID: "webmasters.sites.add", Method: "PUT", Path: sites + "/{siteUrl}", Resource: "sites",
			Class: growth.ClassAdmin, Permissions: []string{scopeManage}},
		{ID: "webmasters.sites.delete", Method: "DELETE", Path: sites + "/{siteUrl}", Resource: "sites",
			Class: growth.ClassAdmin, Danger: true, Roles: anyUser, Permissions: []string{scopeManage},
			Reason: "A user's access goes with it, and only an owner can give it back."},
		{ID: "searchconsole.urlInspection.index.inspect", Method: "POST",
			Path: searchAPI + "/v1/urlInspection/index:inspect", Resource: "urlInspection.index",
			Class: growth.ClassRead, Reads: true, Roles: restrictedUp, Permissions: []string{scopeReadonly}},
		{ID: "searchconsole.urlTestingTools.mobileFriendlyTest.run", Method: "POST",
			Path: searchAPI + "/v1/urlTestingTools/mobileFriendlyTest:run", Resource: "urlTestingTools.mobileFriendlyTest",
			Class: growth.ClassNever, Reads: true,
			Reason: "Google retired the Mobile-Friendly Test on December 1, 2023, and the method answers " +
				"403 Permission denied."},
		{ID: "siteVerification.webResource.getToken", Method: "POST", Path: verifyAPI + "/siteVerification/v1/token",
			Resource: "webResource", Class: growth.ClassRead, Reads: true, Permissions: []string{scopeVerifyOnly}},
		{ID: "siteVerification.webResource.insert", Method: "POST", Path: resources, Resource: "webResource",
			Class: growth.ClassAdmin, Permissions: []string{scopeVerifyOnly},
			Reason: "Verifying makes the identity an owner of the property, in every Google product that " +
				"reads verification."},
		{ID: "siteVerification.webResource.list", Method: "GET", Path: resources, Resource: "webResource",
			Class: growth.ClassRead, Roles: owners, Permissions: []string{scopeVerify}},
		{ID: "siteVerification.webResource.get", Method: "GET", Path: resources + "/{id}", Resource: "webResource",
			Class: growth.ClassRead, Roles: owners, Permissions: []string{scopeVerify}},
		{ID: "siteVerification.webResource.update", Method: "PUT", Path: resources + "/{id}", Resource: "webResource",
			Class: growth.ClassAdmin, Roles: owners, Permissions: []string{scopeVerify},
			Reason: "Adding an owner grants full control of the property, and removing one revokes it."},
		{ID: "siteVerification.webResource.patch", Method: "PATCH", Path: resources + "/{id}", Resource: "webResource",
			Class: growth.ClassAdmin, Roles: owners, Permissions: []string{scopeVerify},
			Reason: "Adding an owner grants full control of the property, and removing one revokes it."},
		{ID: "siteVerification.webResource.delete", Method: "DELETE", Path: resources + "/{id}",
			Resource: "webResource", Class: growth.ClassAdmin, Danger: true, Roles: verified,
			Permissions: []string{scopeVerify},
			Reason:      "The identity relinquishes its own ownership; only verifying again restores it."},
		{ID: "indexing.urlNotifications.publish", Method: "POST", Path: indexingAPI + "/v3/urlNotifications:publish",
			Resource: "urlNotifications", Class: growth.ClassOperate, Roles: owners, Permissions: []string{scopeIndexing},
			Reason: "Only for a page Google's terms allow, which the engine reads first."},
		{ID: "indexing.urlNotifications.getMetadata", Method: "GET", Path: indexingAPI + "/v3/urlNotifications/metadata",
			Resource: "urlNotifications", Class: growth.ClassRead, Roles: owners, Permissions: []string{scopeIndexing}},
	}
}
