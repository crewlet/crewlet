// Package googleanalytics is the Google Analytics 4 integration: every method
// of the Data API, the Admin API (v1beta and v1alpha) and the Measurement
// Protocol, offered to agent seats through engine tools built from one catalog
// (internal/growth; ADR-0170, ADR-0171).
//
// # Who the engine is at Google Analytics
//
// Each seat calls as its own Google growth identity (internal/googleauth,
// ADR-0172), which a person adds to each property at a role: Viewer, Analyst,
// Marketer, Editor or Administrator. The role is the vendor's ceiling and the
// seat's level, scope and each operation's class the engine's floor: both
// must allow a call. Tokens are minted inside the engine with the narrowest
// scope the operation accepts, an hour each, and never leave it.
//
// # Every method, in one table
//
// operations.go lists every method with its class, the roles that may call it
// and the scope minted for it; docs/integrations/google-analytics.md is the
// same table for people (TestTheDocsMatrixIsTheCatalog). Curated tools serve
// the common acts with typed arguments, and one generic tool per class names
// any method of its class, so nothing the API offers is out of reach. A write
// whose class depends on what it changes (a custom metric's restriction, a
// BigQuery link's exports) is classed per call from its update mask and its
// values ([growth.Classify]), and a call above its tool's class is refused
// with the tool that may make it.
//
// # Scope
//
// A seat names its properties. A call naming a property outside them is
// refused before Google is reached; an account-level call is made only for an
// account whose every property the seat may reach, since it changes or reads
// all of them; and a listing is cut to the seat's properties.
//
// # Somebody else's words
//
// Anyone on the internet writes into a property (page titles, referrers,
// search terms, campaign names, event parameters), and property users write
// names and descriptions. So every answer's vendor text is fenced, and a
// credential Google returns (a Measurement Protocol secret, a webhook channel
// token) is replaced before an agent reads it.
//
// The request, its sending, the held call's binding and the access log's
// records are internal/googlegrowth's, which every Google growth product shares.
package googleanalytics

import (
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googleauth"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/integration"
)

// Kind is the integration's kind, config key and lane.
const Kind = integration.KindGoogleAnalytics

// Google's hosts for the three APIs. Fixed, so a seat's token only ever goes
// to Google.
const (
	dataAPI        = "https://analyticsdata.googleapis.com"
	adminAPI       = "https://analyticsadmin.googleapis.com"
	measurementAPI = "https://www.google-analytics.com"
)

// The scopes the engine mints, each only for an operation that accepts it.
const (
	scopeReadonly   = "https://www.googleapis.com/auth/analytics.readonly"
	scopeEdit       = "https://www.googleapis.com/auth/analytics.edit"
	scopeUsers      = "https://www.googleapis.com/auth/analytics.manage.users"
	scopeUsersRead  = "https://www.googleapis.com/auth/analytics.manage.users.readonly"
	scopeChatbot    = "https://www.googleapis.com/auth/analytics.chatbot.read"
	apiVersionLabel = "Data API v1beta and v1alpha, Admin API v1beta and v1alpha"
)

// The roles a person gives a seat's identity, least first, as the Admin API
// names them without their `predefinedRoles/` prefix. Marketer has no API
// value: a person sets it in Google Analytics.
const (
	roleViewer        = "viewer"
	roleAnalyst       = "analyst"
	roleMarketer      = "marketer"
	roleEditor        = "editor"
	roleAdministrator = "admin"
	roleNoCost        = "no-cost-data"
	roleNoRevenue     = "no-revenue-data"
)

// roles is every role and restriction, as the setup card explains them
// (support.google.com/analytics/answer/9305587).
func roles() []growth.Role {
	return []growth.Role{
		{ID: roleViewer, Name: "Viewer", Note: "Sees settings and data and runs every report; changes nothing."},
		{ID: roleAnalyst, Name: "Analyst", Note: "Viewer, plus annotations and shared explorations."},
		{ID: roleMarketer, Name: "Marketer", Note: "Analyst, plus key events, audiences, event create rules " +
			"and attribution settings. Set in Google Analytics; its API cannot grant it."},
		{ID: roleEditor, Name: "Editor", Note: "Marketer, plus every property and account setting, data stream, " +
			"product link, key and deletion; cannot manage users."},
		{ID: roleAdministrator, Name: "Administrator", Note: "Editor, plus users and access, and the access report."},
		{ID: roleNoCost, Name: "No Cost Metrics", Note: "A restriction beside a role: cost metrics read as zero."},
		{ID: roleNoRevenue, Name: "No Revenue Metrics", Note: "A restriction beside a role: revenue metrics read as zero."},
	}
}

// permissions is every scope an operation may need.
func permissions() []growth.Permission {
	return []growth.Permission{
		{ID: scopeReadonly, Note: "Reports, and every setting read."},
		{ID: scopeEdit, Note: "Every change but users; also the only scope change history accepts."},
		{ID: scopeUsersRead, Note: "Reads who has access to an account or property."},
		{ID: scopeUsers, Note: "Grants, changes and removes access to an account or property."},
		{ID: scopeChatbot, Note: "Asks Google Analytics questions in words (the one scope its chat accepts)."},
	}
}

// catalog is the whole integration as the engine offers it.
func catalog() growth.Catalog {
	ops := operations()
	return growth.Catalog{
		Kind: string(Kind), API: "Google Analytics", Version: apiVersionLabel,
		Roles: roles(), Permissions: permissions(), Operations: ops, Tools: toolDefs(ops),
	}
}

// Scopes per seat level, for the growth identity's registration: the
// narrowest each level calls with. Each call mints the scopes its operation
// names, within these.
func levelScopes() map[string][]string {
	return map[string][]string{
		string(growth.LevelRead):       {scopeReadonly, scopeEdit, scopeUsersRead, scopeChatbot},
		string(growth.LevelOperate):    {scopeReadonly, scopeEdit, scopeUsersRead, scopeChatbot},
		string(growth.LevelAdminister): {scopeReadonly, scopeEdit, scopeUsersRead, scopeUsers, scopeChatbot},
	}
}

func init() {
	growth.Register(catalog())
	googleauth.Register(googleauth.Product{
		Kind: Kind, Name: "Google Analytics",
		APIs:         []string{"analyticsdata.googleapis.com", "analyticsadmin.googleapis.com"},
		Scopes:       levelScopes(),
		ReaderScopes: []string{scopeReadonly},
		On:           func(c *config.Company) bool { return c.Integrations.GrowthEnabled(string(Kind)) },
	})
}
