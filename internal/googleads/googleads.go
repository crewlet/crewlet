// Package googleads is the Google Ads integration: every method of the Google
// Ads API offered to agent seats through engine tools built from one catalog
// (internal/growth; ADR-0170, ADR-0171), on the request, holding and recording
// every Google growth product shares (internal/googlegrowth).
//
// # Who the engine is at Google Ads
//
// Each seat calls as its own Google growth identity (internal/googleauth,
// ADR-0172), which a person adds to each Google Ads account at an access
// level: Read-only, Standard or Admin (Email-only and Billing reach no API
// method a seat calls). Access through a manager account names it in the
// `login-customer-id` header, which the company's block sets per account. The
// level is the vendor's ceiling and the seat's level, accounts and each call's
// class the engine's floor: both must allow a call. Google retired developer
// tokens, so none is sent.
//
// # Every method, in one table
//
// operations.go lists every method with its class, the access levels that may
// call it and the scope minted for it; docs/integrations/google-ads.md is the
// same table for people (TestTheDocsMatrixIsTheCatalog). One generic tool per
// class names any method of its class, so nothing the API offers is out of
// reach, and curated tools serve the common acts with typed arguments.
//
// # A mutate is classed by every operation it holds
//
// A mutate request holds operations, each a create, an update with its field
// mask or a remove, and its class is the highest any of them comes to under
// its service's rule (rules.go), read against the target's live values where
// the rule compares: enabling, raising a budget or a bid, widening reach and
// removing a restriction are spend; pausing, lowering and adding a negative
// are operate; new or changed ad text is public; removals of containers,
// links, users and billing are admin. A rule that cannot read what it compares
// classes the change as the highest it allows.
//
// # Spend is held to the company's caps
//
// A spend call is checked against the account's caps before a person is asked
// and again when it is applied, under the account's lease (internal/spendguard):
// capacity.go reads the account's daily capacity live with GAQL and what an
// operation adds to it.
//
// # Somebody else's words
//
// The public types search terms, other advertisers and publishers name
// placements, consumers write leads, and Google generates ad text. Every
// answer's vendor text is fenced.
package googleads

import (
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googleauth"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/integration"
)

// Kind is the integration's kind, config key and lane.
const Kind = integration.KindGoogleAds

// adsAPI is Google Ads' one host. Fixed, so a seat's token only goes to Google.
const adsAPI = "https://googleads.googleapis.com"

// scopeAdwords is the one scope every method takes; what a call may do is
// the identity's access level on the account.
const scopeAdwords = "https://www.googleapis.com/auth/adwords"

// apiVersion is the version the catalog calls.
const apiVersion = "v26"

// sunset is when Google retires v26: announced for November 2027, the month
// alone, so its first day, which the version notice counts down to.
var sunset = time.Date(2027, time.November, 1, 0, 0, 0, 0, time.UTC)

// The access levels a person gives a seat's identity on an account, least
// first (support.google.com/google-ads/answer/9978556).
const (
	roleEmailOnly = "email_only"
	roleBilling   = "billing"
	roleReadOnly  = "read_only"
	roleStandard  = "standard"
	roleAdmin     = "admin"
)

// roles is every access level, as the setup card explains them.
func roles() []growth.Role {
	return []growth.Role{
		{ID: roleEmailOnly, Name: "Email only", Note: "Receives notification emails and reports; calls no API method."},
		{ID: roleBilling, Name: "Billing", Note: "Sees and edits billing in Google Ads; the API offers it nothing else."},
		{ID: roleReadOnly, Name: "Read-only", Note: "Reads campaigns, reports, billing and planning tools; changes nothing."},
		{ID: roleStandard, Name: "Standard", Note: "Read-only, plus every change to campaigns, ads, budgets, bids, " +
			"audiences, conversions and experiments; not users, links or billing setup."},
		{ID: roleAdmin, Name: "Admin", Note: "Standard, plus users and their access, product and manager links, " +
			"billing setup and account budgets."},
	}
}

// permissions is the one scope.
func permissions() []growth.Permission {
	return []growth.Permission{{ID: scopeAdwords, Note: "Every method; the access level decides what it may do."}}
}

// catalog is the whole integration as the engine offers it.
func catalog() growth.Catalog {
	ops := operations()
	return growth.Catalog{
		Kind: string(Kind), API: "Google Ads API", Version: apiVersion, Retires: sunset,
		Roles: roles(), Permissions: permissions(), Operations: ops, Tools: toolDefs(ops),
	}
}

func init() {
	growth.Register(string(Kind), catalog)
	googleauth.Register(googleauth.Product{
		Kind: Kind, Name: "Google Ads",
		APIs: []string{"googleads.googleapis.com"},
		Scopes: map[string][]string{
			string(growth.LevelRead): {scopeAdwords}, string(growth.LevelOperate): {scopeAdwords},
			string(growth.LevelAdminister): {scopeAdwords},
		},
		ReaderScopes: []string{scopeAdwords},
		On:           func(c *config.Company) bool { return c.Integrations.GrowthEnabled(string(Kind)) },
	})
}
