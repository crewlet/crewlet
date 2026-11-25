// Package admob is the AdMob integration: every method of the AdMob API (v1
// and v1beta), offered to agent seats through engine tools built from one
// catalog (internal/growth; ADR-0170, ADR-0171).
//
// # Who the engine is at AdMob
//
// Google refuses service accounts at AdMob, so the company reaches it through
// one consent grant (ADR-0178): a person who manages the AdMob account signs
// in with Google once, the engine seals the refresh token and renews it, and
// every seat's call goes as that grant (internal/googlegrowth's Client.Grant). The
// grant's scopes are the company's ceiling: admob.readonly and admob.report
// for reading, and admob.monetization once any seat may change settings. The
// signing person's AdMob role is Google's: Administrator, Manager, or a
// custom role whose permission groups decide what the API returns. The API
// reports no role, so the engine names the roles each method needs and turns
// Google's refusal into the role, the scope or the allowlisting to ask for.
//
// # The seat's level and scope are the engine's
//
// One grant serves every seat, so nothing at Google tells two agents apart:
// a seat's level (read, operate, administer) and its publisher accounts are
// held by the engine, SSH's shape. A call naming another account is refused
// before Google is reached, and the account listing is cut to the seat's.
//
// # Every method, in one table
//
// operations.go lists every method with its class, the roles that may call it
// and the scope it needs; docs/integrations/admob.md is the same table for
// people (TestTheDocsMatrixIsTheCatalog). Curated tools serve the common acts
// with typed arguments, and one generic tool per class names any method of
// its class. Nothing in AdMob spends money or publishes: AdMob pays the
// publisher, and every write configures monetization, so each is operate,
// except the irreversible changes classed admin per call: linking a new app
// to its store listing, removing a mediation line, and stopping an A/B
// experiment keeping the treatment's lines, which removes the control lines it
// lacks. A patch is classed by its update mask, so a change names one, and
// every spelling Google reads that the engine would not (a proto-named body
// field, an unknown query key, a map key of another shape) is refused. Ten
// methods are what Google calls limited access; they stay offered, and
// Google's 403 says how to get them allowed.
//
// # Somebody else's words
//
// App, ad unit, mediation group and line names are typed by the account's
// users, store listing names by whoever owns the listing, and ad source,
// adapter and campaign names by third parties, so every answer's vendor text
// is fenced. An ad unit mapping's configuration values are a third-party
// network's identifiers, some of them keys, so they are withheld from every
// answer and the access log keeps their digest alone.
package admob

import (
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googleauth"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/integration"
)

// Kind is the integration's kind, config key and lane.
const Kind = integration.KindAdMob

// api is Google's host for the AdMob API. Fixed, so the grant's token only
// ever goes to Google.
const api = "https://admob.googleapis.com"

// The scopes a sign-in asks for (https://developers.google.com/admob/api/v1/auth).
const (
	scopeReadonly     = "https://www.googleapis.com/auth/admob.readonly"
	scopeReport       = "https://www.googleapis.com/auth/admob.report"
	scopeMonetization = "https://www.googleapis.com/auth/admob.monetization"
	apiVersionLabel   = "v1 and v1beta"
)

// ReadScopes are what a sign-in asks for while every seat reads: every read,
// and the report scope, which Google names beside readonly for campaign data.
func ReadScopes() []string { return []string{scopeReadonly, scopeReport} }

// OperateScopes are what a sign-in asks for once a seat may change settings:
// every write needs admob.monetization.
func OperateScopes() []string { return []string{scopeReadonly, scopeReport, scopeMonetization} }

// The roles and custom-role permission levels an AdMob user holds
// (support.google.com/admob/answer/13067211). Custom roles pick one level per
// permission group; the groups the API does not reach are listed so setup
// explains them.
const (
	roleAdministrator    = "administrator"
	roleManager          = "manager"
	roleInventoryView    = "inventory-view"
	roleInventoryManage  = "inventory-manage"
	roleReportsView      = "reports-view"
	roleReportsManage    = "reports-manage"
	roleAccountManage    = "account-manage"
	rolePaymentsManage   = "payments-manage"
	roleAppRestricted    = "app-restricted"
	consoleURL           = "https://admob.google.com/"
	usersURL             = "https://apps.admob.com/v2/settings/users"
	allowlistingHelpPage = "https://developers.google.com/admob/api/v1/getting-started"
)

// roles is every role and permission level, as the setup card explains them.
func roles() []growth.Role {
	return []growth.Role{
		{ID: roleAdministrator, Name: "Administrator", Note: "The whole account through the API, plus the " +
			"administrator tasks no API offers: users, terms, MCM and closing the account."},
		{ID: roleManager, Name: "Manager", Note: "The whole account through the API; no administrator tasks."},
		{ID: roleInventoryView, Name: "Custom role: Inventory management, View", Note: "Apps, ad units, " +
			"mappings, ad sources and mediation groups, read only. Every custom role holds at least this."},
		{ID: roleInventoryManage, Name: "Custom role: Inventory management, View and manage", Note: "Also " +
			"creates apps, ad units and mappings, and changes mediation groups and A/B experiments."},
		{ID: roleReportsView, Name: "Custom role: Reports and performance data, View", Note: "Network, " +
			"mediation and campaign reports."},
		{ID: roleReportsManage, Name: "Custom role: Reports and performance data, View and manage",
			Note: "The same reports through the API; managing saved reports is the AdMob console's."},
		{ID: roleAccountManage, Name: "Custom role: Account management, View and manage", Note: "Blocking " +
			"controls, linked services, privacy and messaging: the AdMob console's alone, no API method."},
		{ID: rolePaymentsManage, Name: "Custom role: Payments, View and manage", Note: "Payment details: " +
			"the AdMob console's alone, no API method."},
		{ID: roleAppRestricted, Name: "Custom role limited to some apps", Note: "Lists and reports show " +
			"only the apps the role names, and changes to other apps are refused."},
	}
}

// permissions is every scope a sign-in may ask for.
func permissions() []growth.Permission {
	return []growth.Permission{
		{ID: scopeReadonly, Note: "Every read: accounts, apps, ad units, mappings, ad sources, mediation and reports."},
		{ID: scopeReport, Note: "Accounts and reports, campaign figures among them."},
		{ID: scopeMonetization, Note: "Every change: apps, ad units, mappings, mediation groups and A/B experiments."},
	}
}

// catalog is the whole integration as the engine offers it.
func catalog() growth.Catalog {
	ops := operations()
	return growth.Catalog{
		Kind: string(Kind), API: "AdMob API", Version: apiVersionLabel,
		Roles: roles(), Permissions: permissions(), Operations: ops, Tools: toolDefs(ops),
	}
}

func init() {
	growth.RegisterFunc(string(Kind), catalog)
	googleauth.RegisterConsent(googleauth.Consent{Kind: Kind, Name: "AdMob", Scopes: GrantScopes,
		Client: oauthClient})
}

// oauthClient is the OAuth client AdMob signs in as (googleauth.SignInClient),
// false where AdMob is off.
func oauthClient(c *config.Company) (googleauth.ConsentClient, bool) {
	b := c.Integrations.AdMob
	if !b.GrowthEnabled() {
		return googleauth.ConsentClient{}, false
	}
	return googleauth.SignInClient(c, b.OAuth), true
}
