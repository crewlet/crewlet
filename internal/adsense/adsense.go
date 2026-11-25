// Package adsense is the AdSense integration: every method of the AdSense
// Management API (v2) and the AdSense Platform API (v1 and v1alpha), offered
// to agent seats through engine tools built from one catalog
// (internal/growth; ADR-0170, ADR-0171).
//
// # Who the engine is at AdSense
//
// Google refuses service accounts at AdSense, so the company reaches it
// through one consent grant (ADR-0178): a person who manages the AdSense
// account signs in with Google once, the engine seals the refresh token and
// renews it, and every seat's call goes as that grant (internal/googlegrowth's
// Client.Grant). The grant's scopes are the company's ceiling: adsense.readonly
// while every seat reads, and adsense once any seat may change settings. The
// signing person is an Administrator or a Standard user of the account, which
// both reach every method; the API reports no role, so the engine turns
// Google's refusal into the scope, the allowlisting or the account to ask for.
//
// # The seat's level and scope are the engine's
//
// One grant serves every seat, so nothing at Google tells two agents apart:
// a seat's level (read, operate, administer) and its publisher accounts are
// held by the engine, SSH's shape. A call is held to the account its path
// names (a platform's account, for the Platform API's sub-account methods),
// every publisher ID in its path and body is that account's or the
// sub-account the path names, and the account listing is cut to the seat's.
//
// # Every method, in one table
//
// operations.go lists every method with its class, the scope it needs and the
// query parameters it takes; docs/integrations/adsense.md is the same table
// for people (TestTheDocsMatrixIsTheCatalog). Curated tools serve the common
// acts with typed arguments, and one generic tool per class names any method
// of its class. Nothing in AdSense spends money: AdSense pays the publisher.
// An ad unit's size or type is public, since it changes the ad slot on live
// pages, and so is asking Google to review a platform site, which starts ads
// on it once approved. Deleting a custom channel, a platform's sub-accounts,
// their sites and events, and moving a site to another revenue group are
// admin; every other write is operate. A patch is classed by its update mask
// against its target read first, so a change names a mask Google documents,
// and a body field spelled as its proto name or a query parameter given twice,
// which Google reads and the engine would not, is refused. A site moved to
// another revenue group is asked, and bound, as the revenue share it moves.
// Five Management writes and every Platform API method answer only projects
// Google enables for AdSense for Platforms; they stay offered, and Google's
// 403 says how to get them enabled.
//
// # Somebody else's words
//
// Account, ad unit, channel and saved report names, site domains, alert and
// policy texts, ad code and report cells (page URLs, search queries, buyer
// networks) are words other people wrote, so every answer's vendor text is
// fenced. A platform event's person (an email and a billing address) is a
// third party's personal data: a change, a question and the access log keep
// its digest alone.
package adsense

import (
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googleauth"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/integration"
)

// Kind is the integration's kind, config key and lane.
const Kind = integration.KindAdSense

// Google's hosts and versions: the Management API, and the Platform API's
// stable and alpha surfaces. Fixed, so the grant's token only goes to Google.
const (
	management = "https://adsense.googleapis.com/v2"
	platform   = "https://adsenseplatform.googleapis.com/v1"
	alpha      = "https://adsenseplatform.googleapis.com/v1alpha"
)

// The scopes a sign-in asks for (https://developers.google.com/adsense/management/reference/rest).
const (
	scopeReadonly   = "https://www.googleapis.com/auth/adsense.readonly"
	scopeManage     = "https://www.googleapis.com/auth/adsense"
	apiVersionLabel = "Management v2, Platform v1 and v1alpha"
)

// ReadScopes are what a sign-in asks for while every seat reads.
func ReadScopes() []string { return []string{scopeReadonly} }

// OperateScopes are what a sign-in asks for once a seat may change settings:
// every write needs adsense.
func OperateScopes() []string { return []string{scopeReadonly, scopeManage} }

// The roles an AdSense user holds (support.google.com/adsense/answer/2659101),
// and the AdMob roles a linked account maps onto them
// (support.google.com/admob/answer/11213923).
const (
	roleAdministrator      = "administrator"
	roleStandard           = "standard"
	roleAdMobAdministrator = "admob-administrator"
	roleAdMobManager       = "admob-manager"
	roleAdMobCustom        = "admob-custom"
	consoleURL             = "https://adsense.google.com/"
)

// roles is every role, as the setup card explains them.
func roles() []growth.Role {
	return []growth.Role{
		{ID: roleAdministrator, Name: "Administrator", Note: "Every API method, plus the administrator tasks no " +
			"API offers: users, terms, payment settings and closing the account."},
		{ID: roleStandard, Name: "Standard user", Note: "Every API method; no administrator tasks."},
		{ID: roleAdMobAdministrator, Name: "AdMob Administrator, linked account", Note: "Holds Administrator " +
			"on the AdSense account linked to the AdMob account."},
		{ID: roleAdMobManager, Name: "AdMob Manager, linked account", Note: "Holds Standard user on the " +
			"AdSense account linked to the AdMob account."},
		{ID: roleAdMobCustom, Name: "AdMob custom role", Note: "No AdSense access: a custom role is AdMob's alone."},
	}
}

// permissions is every scope a sign-in may ask for.
func permissions() []growth.Permission {
	return []growth.Permission{
		{ID: scopeReadonly, Note: "Every read: accounts, sites, ad clients, ad units, channels, alerts, policy " +
			"issues, payments, ad code, reports and the Platform API's reads."},
		{ID: scopeManage, Note: "Every read and every change: ad units, custom channels, and the Platform " +
			"API's sub-accounts, sites, events and groups."},
	}
}

// catalog is the whole integration as the engine offers it.
func catalog() growth.Catalog {
	ops := operations()
	return growth.Catalog{
		Kind: string(Kind), API: "AdSense Management and Platform APIs", Version: apiVersionLabel,
		Roles: roles(), Permissions: permissions(), Operations: ops, Tools: toolDefs(ops),
	}
}

func init() {
	growth.RegisterFunc(string(Kind), catalog)
	googleauth.RegisterConsent(googleauth.Consent{Kind: Kind, Name: "AdSense", Scopes: GrantScopes,
		Client: oauthClient})
}

// oauthClient is the OAuth client AdSense signs in as (googleauth.SignInClient),
// false where AdSense is off.
func oauthClient(c *config.Company) (googleauth.ConsentClient, bool) {
	b := c.Integrations.AdSense
	if !b.GrowthEnabled() {
		return googleauth.ConsentClient{}, false
	}
	return googleauth.SignInClient(c, b.OAuth), true
}
