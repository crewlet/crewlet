// Package googleplay is the Google Play integration: every method of the
// Google Play Android Developer API, the Play Developer Reporting API and the
// monthly reports bucket, offered to agent seats through engine tools built
// from one catalog (internal/growth; ADR-0170, ADR-0171).
//
// # Who the engine is at Google Play
//
// Each seat calls as its own Google growth identity (internal/googleauth,
// ADR-0172), which a person invites in Play Console with app permissions. The
// permissions are Google's ceiling and the seat's level, apps and each
// method's class the engine's floor: both must allow a call. Play has one
// scope for everything, so the permissions a person grants are the only
// vendor bound, and a refusal names the permission the call needs. The
// monthly reports bucket needs account-wide access no seat should hold, so it
// is read as the company's reader alone, and cut to the seat's apps.
//
// # Every method, in one table
//
// operations.go lists every method with its class, the permissions that may
// call it and its scope; docs/integrations/google-play.md is the same table
// for people (TestTheDocsMatrixIsTheCatalog). Curated tools serve the common
// acts with typed arguments, and one generic tool per class names any method
// of its class. A write whose class depends on its target is classed per call
// from the target as read now: a release that only drafts or halts is
// operate, one that reaches users public; a product or offer still in draft
// is operate, a live one's price public; a tax setting is admin.
//
// # Edits are the engine's
//
// Listing, release, tester and media changes go through a Play edit, which
// any other commit to the app invalidates. So the engine opens, stages,
// validates and commits or discards an edit inside one call, under the app's
// write lease, and an agent never holds an edit open: it names the staged
// changes (one, or several as `steps` of a commit). A commit never cancels a
// submission a person sent for review unless the approved call says so.
//
// # Somebody else's words
//
// Reviews, listings written by anyone with Console access, error reports and
// the reports bucket's files carry words other people wrote, so every answer
// is fenced, and a review reply a person reads in full before it is
// published.
//
// The request, its sending, the held call's binding and the access log's
// records are internal/googlegrowth's, which every Google growth product shares.
package googleplay

import (
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googleauth"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/integration"
)

// Kind is the integration's kind, config key and lane.
const Kind = integration.KindGooglePlay

// Google's hosts for the three APIs. Fixed, so a seat's token only ever goes
// to Google.
const (
	publisherAPI = "https://androidpublisher.googleapis.com"
	reportingAPI = "https://playdeveloperreporting.googleapis.com"
	storageAPI   = "https://storage.googleapis.com"
)

// The scopes the engine mints, each only for an operation that accepts it.
const (
	scopePublisher   = "https://www.googleapis.com/auth/androidpublisher"
	scopeReporting   = "https://www.googleapis.com/auth/playdeveloperreporting"
	scopeStorageRead = "https://www.googleapis.com/auth/devstorage.read_only"
	apiVersionLabel  = "Android Developer API v3, Developer Reporting API v1beta1, Cloud Storage JSON API v1"
)

// The Play Console permissions, as the API's app-level grants name them (an
// account-wide grant adds _GLOBAL), plus the program and ownership roles that
// no grant gives (support.google.com/googleplay/android-developer/answer/9844686).
const (
	permAdmin           = "CAN_MANAGE_PERMISSIONS"
	permView            = "CAN_VIEW_NON_FINANCIAL_DATA"
	permQuality         = "CAN_VIEW_APP_QUALITY"
	permFinancial       = "CAN_VIEW_FINANCIAL_DATA"
	permOrders          = "CAN_MANAGE_ORDERS"
	permStore           = "CAN_MANAGE_PUBLIC_LISTING"
	permProduction      = "CAN_MANAGE_PUBLIC_APKS"
	permTesting         = "CAN_MANAGE_TRACK_APKS"
	permTesters         = "CAN_MANAGE_TRACK_USERS"
	permReply           = "CAN_REPLY_TO_REVIEWS"
	permPolicy          = "CAN_MANAGE_APP_CONTENT"
	permDraft           = "CAN_MANAGE_DRAFT_APPS"
	permManaged         = "CAN_CREATE_MANAGED_PLAY_APPS_GLOBAL"
	roleThirdPartyStore = "THIRD_PARTY_APP_STORE"
)

// roles is every permission and role a Play identity can hold, as the setup
// card explains them.
func roles() []growth.Role {
	return []growth.Role{
		{ID: permAdmin, Name: "Admin (all permissions)", Note: "Every permission, plus inviting, " +
			"removing and managing users and their access."},
		{ID: permView, Name: "View app information", Note: "Reads apps, listings, releases, " +
			"reviews, vitals and statistics, financial data excepted; Play Console adds it to every other " +
			"permission. The account-wide form also downloads bulk reports."},
		{ID: permQuality, Name: "View app quality information", Note: "Android vitals, pre-launch " +
			"reports and basic track and release information."},
		{ID: permFinancial, Name: "View financial data", Note: "Orders, purchases, voided purchases " +
			"and the financial reports."},
		{ID: permOrders, Name: "Manage orders and subscriptions", Note: "Refunds orders, cancels, defers " +
			"and revokes subscriptions, acknowledges purchases."},
		{ID: permStore, Name: "Manage store presence", Note: "Store listings and images, " +
			"in-app products, subscriptions, prices and offers."},
		{ID: permProduction, Name: "Release to production", Note: "Production releases and rollouts, " +
			"device exclusion, app recovery and Play App Signing."},
		{ID: permTesting, Name: "Release to testing tracks", Note: "Testing-track releases, uploads, " +
			"expansion and mapping files, and internal app sharing."},
		{ID: permTesters, Name: "Manage testing tracks and tester lists", Note: "Tester groups and " +
			"new closed testing tracks."},
		{ID: permReply, Name: "Reply to reviews", Note: "Replies to reviews on the store page."},
		{ID: permPolicy, Name: "Manage policy declarations", Note: "The Data safety declaration and " +
			"other policy declarations."},
		{ID: permDraft, Name: "Edit and delete draft apps", Note: "Changes to apps never published, " +
			"draft releases only."},
		{ID: permManaged, Name: "Create and publish private apps", Note: "Managed " +
			"Google Play apps private to an organization; account-wide only."},
		{ID: roleThirdPartyStore, Name: "Third-party app store on Play", Note: "A store enrolled in Google's " +
			"program, calling as its own service account: the hosted-app review and catalog methods."},
	}
}

// permissions is every scope an operation may need.
func permissions() []growth.Permission {
	return []growth.Permission{
		{ID: scopePublisher, Note: "Every Android Developer API method; Play has no narrower scope, so the Play " +
			"Console permissions bound what it reaches."},
		{ID: scopeReporting, Note: "Every Developer Reporting API method: vitals, errors and anomalies."},
		{ID: scopeStorageRead, Note: "Reads the monthly reports bucket, as the company's reader only."},
	}
}

// catalog is the whole integration as the engine offers it.
func catalog() growth.Catalog {
	ops := operations()
	return growth.Catalog{
		Kind: string(Kind), API: "Google Play", Version: apiVersionLabel,
		Roles: roles(), Permissions: permissions(), Operations: ops, Tools: toolDefs(ops),
	}
}

// levelScopes are the scopes per seat level, for the growth identity's
// registration: Play has no read-only scope, so every level mints the same
// two, and the permissions a person grants are the vendor's bound.
func levelScopes() map[string][]string {
	both := []string{scopePublisher, scopeReporting}
	return map[string][]string{
		string(growth.LevelRead): both, string(growth.LevelOperate): both, string(growth.LevelAdminister): both,
	}
}

func init() {
	growth.RegisterFunc(string(Kind), catalog)
	googleauth.Register(googleauth.Product{
		Kind: Kind, Name: "Google Play",
		APIs:         []string{"androidpublisher.googleapis.com", "playdeveloperreporting.googleapis.com"},
		Scopes:       levelScopes(),
		ReaderScopes: []string{scopePublisher, scopeReporting, scopeStorageRead},
		On:           func(c *config.Company) bool { return c.Integrations.GrowthEnabled(string(Kind)) },
	})
}
