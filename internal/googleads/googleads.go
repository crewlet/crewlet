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
// classes the change as the highest it allows, and only an enum value a rule
// names as lowering (PAUSED, OPTED_OUT) lowers: Google also takes an enum's
// number, as a JSON number or a string, which may be any value. A value set
// where the live read holds none is a raise, but for a limit whose absence is
// no limit (a ceiling, a maximize-conversions target CPA, a target ROAS),
// since an absent bid, floor or modifier is one Google inherits or applies.
//
// The rules name fields in snake_case and read a body's keys in either
// spelling, as Google does; a body naming one field in both with two values is
// refused, since which one Google keeps is not the engine's to guess. Each
// method's operations are read from its own field: mutate_operations for
// GoogleAdsService.Mutate and a batch job's staging, operations for a
// service's mutate.
//
// # A call acts in one account
//
// A call acts in the customer its path names (the operation's template's) or,
// where it names none, its body's customer_id; every resource name its body
// holds, and every customer it names alone, at any depth, is that customer's,
// but in the one field a method exists to name another account by (a
// manager's new client, a client's new manager, a product link's account),
// each admin.
//
// # An approval binds what the rules read
//
// What an approval binds is the live state its rules read (googlegrowth's
// Client.BeforeRead), so the class and the binding are on one reading; a
// batch job's run binds the job's operation count. Running one is admin:
// Google gives no read of a job's operations and the access log keeps them
// only as digests, so nothing can class them, or cap them, as the account
// stands when the job runs (ADR-0171).
//
// # Spend is held to the company's caps
//
// A call that spends is checked against the account's caps before a person
// is asked and again when it is applied, whatever its class (an admin
// operation beside a budget raise), inside the one write lease googlegrowth
// holds on the account for every write, by spendguard's Recheck, so no act
// holds two leases: capacity.go reads the account's daily capacity live with
// GAQL, every budget the operations name, and what they add to it, reading
// an amount as the rules do and failing on one it cannot. A spend method that
// carries no operations names what it adds (classify.go's measures); one
// whose request names no amount a cap could hold is admin (ADR-0171).
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

// The registration's lists, laid out by the compiler rather than made at init,
// which every push's hook pays for: the one API, and the one scope every
// level and the company's reader take.
var (
	adsAPIs   = []string{"googleads.googleapis.com"}
	adwordsOf = []string{scopeAdwords}
)

func init() {
	growth.RegisterFunc(string(Kind), catalog)
	googleauth.Register(googleauth.Product{
		Kind: Kind, Name: "Google Ads", APIs: adsAPIs, ReaderScopes: adwordsOf, On: enabled,
		Scopes: map[string][]string{string(growth.LevelRead): adwordsOf, string(growth.LevelOperate): adwordsOf,
			string(growth.LevelAdminister): adwordsOf},
	})
}

// enabled reports whether c turns Google Ads on.
func enabled(c *config.Company) bool { return c.Integrations.GrowthEnabled(string(Kind)) }
