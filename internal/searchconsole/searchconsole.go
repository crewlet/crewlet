// Package searchconsole is the Google Search Console integration: every
// method of the Search Console API, the Site Verification API and the
// Indexing API, offered to agent seats through engine tools built from one
// catalog (internal/growth, internal/googlegrowth; ADR-0170, ADR-0171).
//
// # Who the engine is at Search Console
//
// Each seat calls as its own Google growth identity (internal/googleauth,
// ADR-0172), which a person adds to each property as a Restricted user, a
// Full user or an Owner. That permission is the vendor's ceiling and the
// seat's level, sites and each operation's class the engine's floor: both
// must allow a call. Site Verification and the Indexing API answer only an
// owner, and relinquishing ownership only a verified one, whose own token is
// on the site; the catalog names those as roles of their own. Tokens are
// minted inside the engine with the scope the operation names, an hour each,
// and never leave it. A read Google accepts only with a managing scope (Site
// Verification's reads, Indexing's metadata) stays a read: its token is spent
// on that read alone.
//
// # Every method, in one table
//
// operations.go lists every method with its class, the roles that may call it
// and the scope minted for it; docs/integrations/google-search-console.md is
// the same table for people (TestTheDocsMatrixIsTheCatalog). Curated tools
// serve the common acts with typed arguments, and one generic tool per class
// names any method of its class. The retired Mobile-Friendly Test is the one
// method never offered.
//
// # Scope
//
// A seat names its sites, as Search Console writes them: a URL prefix
// (https://www.example.com/) or a domain (sc-domain:example.com). Every call
// is held to one of them before Google is reached: a site in its path or body,
// a page a URL prefix starts or a domain's host holds, or a Site Verification
// resource read back as the property it verifies (dns://example.com is
// sc-domain:example.com). A listing is cut to the seat's sites.
//
// # A write, under its site's lease
//
// Every write runs through googlegrowth's Client.Apply: an approved call's
// answer is recorded before anything is sent and it acts only on its one
// answer, a deny or a lapse is recorded as that answer, and under the site's
// write lease the engine holds the call to the seat's sites and the Indexing
// API's terms again and reads its target again (Product.before, which reads
// none for a property added or a notification), sending nothing where it
// moved since the approval.
//
// # The Indexing API's terms
//
// Google allows the Indexing API only for pages carrying JobPosting, or
// BroadcastEvent inside VideoObject, structured data, and takes a removal
// only for a page that is gone (404 or 410) or carries noindex. The engine
// reads the page itself, following no redirect, through the public-address
// guard (internal/httpx) before every notification, and refuses one it does
// not allow, so no agent's
// call puts the company's access at risk. Google's own 429 is the daily quota's
// guard: a count of its own would need the whole company's agreement and be
// wrong wherever Google grants more.
//
// # Somebody else's words
//
// Searchers write the queries a report groups by, and pages carry their own
// URLs, canonicals, item names and the issue text Google quotes from them; a
// verified resource lists its owners' emails. So every answer's vendor text
// is fenced.
package searchconsole

import (
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googleauth"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/integration"
)

// Kind is the integration's kind, config key and lane.
const Kind = integration.KindSearchConsole

// Google's hosts for the three APIs. Fixed, so a seat's token only ever goes
// to Google.
const (
	searchAPI   = "https://searchconsole.googleapis.com"
	verifyAPI   = "https://www.googleapis.com"
	indexingAPI = "https://indexing.googleapis.com"
)

// The scopes the engine mints, each only for an operation that accepts it.
const (
	scopeReadonly   = "https://www.googleapis.com/auth/webmasters.readonly"
	scopeManage     = "https://www.googleapis.com/auth/webmasters"
	scopeVerify     = "https://www.googleapis.com/auth/siteverification"
	scopeVerifyOnly = "https://www.googleapis.com/auth/siteverification.verify_only"
	scopeIndexing   = "https://www.googleapis.com/auth/indexing"
	apiVersionLabel = "Search Console API v1, Site Verification API v1, Indexing API v3"
)

// The roles an identity holds on a property, as the Search Console API's
// permissionLevel names them, least first. A verified owner is Site
// Verification's notion, which no API reports as a value: an owner whose own
// token is on the site.
const (
	roleUnverified    = "SITE_UNVERIFIED_USER"
	roleRestricted    = "SITE_RESTRICTED_USER"
	roleFull          = "SITE_FULL_USER"
	roleOwner         = "SITE_OWNER"
	roleVerifiedOwner = "verified_owner"
)

// roles is every role, as the setup card explains them
// (support.google.com/webmasters/answer/7687615).
func roles() []growth.Role {
	return []growth.Role{
		{ID: roleUnverified, Name: "Unverified", Note: "The property is in the identity's list with no " +
			"permission: it can verify ownership, or take the property off its list."},
		{ID: roleRestricted, Name: "Restricted user", Note: "Sees performance, sitemaps, URL inspection and " +
			"every report; submits nothing."},
		{ID: roleFull, Name: "Full user", Note: "Restricted user, plus submitting and removing sitemaps. " +
			"Cannot add users or owners."},
		{ID: roleOwner, Name: "Owner", Note: "Full user, plus Site Verification and the Indexing API, " +
			"verified or delegated by another owner."},
		{ID: roleVerifiedOwner, Name: "Verified owner", Note: "An owner whose own verification token is on " +
			"the site: the only one who can relinquish its ownership."},
	}
}

// permissions is every scope an operation may need.
func permissions() []growth.Permission {
	return []growth.Permission{
		{ID: scopeReadonly, Note: "Reads performance, sitemaps, properties and URL inspection."},
		{ID: scopeManage, Note: "Submits and removes sitemaps, and adds or removes a property in the " +
			"identity's list."},
		{ID: scopeVerify, Note: "Reads verified sites and changes their owners; Site Verification has no " +
			"read-only scope."},
		{ID: scopeVerifyOnly, Note: "Gets a verification token and verifies a new site, reading nothing else."},
		{ID: scopeIndexing, Note: "Notifies Google of an updated or removed page, and reads those " +
			"notifications."},
	}
}

// catalog is the whole integration as the engine offers it.
func catalog() growth.Catalog {
	ops := operations()
	return growth.Catalog{
		Kind: string(Kind), API: "Google Search Console", Version: apiVersionLabel,
		Roles: roles(), Permissions: permissions(), Operations: ops, Tools: toolDefs(ops),
	}
}

// Scopes per seat level, for the growth identity's registration: what each
// level's calls are minted with. Each call mints the scopes its operation
// names, within these.
func levelScopes() map[string][]string {
	read := []string{scopeReadonly, scopeVerify, scopeVerifyOnly, scopeIndexing}
	return map[string][]string{
		string(growth.LevelRead):       read,
		string(growth.LevelOperate):    append(read[:len(read):len(read)], scopeManage),
		string(growth.LevelAdminister): append(read[:len(read):len(read)], scopeManage),
	}
}

func init() {
	growth.RegisterFunc(string(Kind), catalog)
	googleauth.Register(googleauth.Product{
		Kind: Kind, Name: "Search Console",
		APIs: []string{"searchconsole.googleapis.com", "siteverification.googleapis.com",
			"indexing.googleapis.com"},
		Scopes:       levelScopes(),
		ReaderScopes: []string{scopeReadonly},
		On:           func(c *config.Company) bool { return c.Integrations.GrowthEnabled(string(Kind)) },
	})
}
