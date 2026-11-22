package admob

import "github.com/crewlet/crewlet/internal/growth"

// The roles that may call each kind of method (roles in admob.go).
var (
	inventoryReaders  = []string{roleAdministrator, roleManager, roleInventoryView, roleInventoryManage}
	inventoryManagers = []string{roleAdministrator, roleManager, roleInventoryManage}
	reportReaders     = []string{roleAdministrator, roleManager, roleReportsView, roleReportsManage}
	readScopes        = []string{scopeReadonly}
	writeScopes       = []string{scopeMonetization}
)

// operations is every method of the AdMob API, v1 and v1beta, each once: the
// six stable reads on v1, every other method on v1beta, where alone it is.
func operations() []growth.Operation {
	return []growth.Operation{
		{ID: "admob.accounts.list", Method: "GET", Path: api + "/v1/accounts", Resource: "accounts",
			Class: growth.ClassRead, Permissions: readScopes},
		{ID: "admob.accounts.get", Method: "GET", Path: api + "/v1/accounts/{accountsId}", Resource: "accounts",
			Class: growth.ClassRead, Permissions: readScopes},
		{ID: "admob.accounts.apps.list", Method: "GET", Path: api + "/v1/accounts/{accountsId}/apps",
			Resource: "accounts.apps", Class: growth.ClassRead, Roles: inventoryReaders, Permissions: readScopes},
		// AN APP LINKED TO A STORE STAYS LINKED: Google says setting its store ID
		// "is considered an irreversible action", so that field is admin. No app
		// can be deleted through the API, which a held card asks to confirm.
		{ID: "admob.accounts.apps.create", Method: "POST", Path: api + "/v1beta/accounts/{accountsId}/apps",
			Resource: "accounts.apps", Class: growth.ClassOperate, Danger: true,
			Fields: []growth.FieldRule{{Field: "linkedAppInfo.appStoreId", Class: growth.ClassAdmin}},
			Roles:  inventoryManagers, Permissions: writeScopes,
			Reason: limitedNote},
		{ID: "admob.accounts.adUnits.list", Method: "GET", Path: api + "/v1/accounts/{accountsId}/adUnits",
			Resource: "accounts.adUnits", Class: growth.ClassRead, Roles: inventoryReaders, Permissions: readScopes},
		{ID: "admob.accounts.adUnits.create", Method: "POST", Path: api + "/v1beta/accounts/{accountsId}/adUnits",
			Resource: "accounts.adUnits", Class: growth.ClassOperate, Roles: inventoryManagers,
			Permissions: writeScopes, Reason: limitedNote},
		{ID: "admob.accounts.adUnits.adUnitMappings.list", Method: "GET",
			Path:     api + "/v1beta/accounts/{accountsId}/adUnits/{adUnitsId}/adUnitMappings",
			Resource: "accounts.adUnits.adUnitMappings", Class: growth.ClassRead, Roles: inventoryReaders,
			Permissions: readScopes, Reason: limitedNote},
		{ID: "admob.accounts.adUnits.adUnitMappings.create", Method: "POST",
			Path:     api + "/v1beta/accounts/{accountsId}/adUnits/{adUnitsId}/adUnitMappings",
			Resource: "accounts.adUnits.adUnitMappings", Class: growth.ClassOperate, Roles: inventoryManagers,
			Permissions: writeScopes, Reason: limitedNote},
		{ID: "admob.accounts.adUnitMappings.batchCreate", Method: "POST",
			Path:     api + "/v1beta/accounts/{accountsId}/adUnitMappings:batchCreate",
			Resource: "accounts.adUnitMappings", Class: growth.ClassOperate, Roles: inventoryManagers,
			Permissions: writeScopes, Reason: limitedNote},
		{ID: "admob.accounts.adSources.list", Method: "GET", Path: api + "/v1beta/accounts/{accountsId}/adSources",
			Resource: "accounts.adSources", Class: growth.ClassRead, Roles: inventoryReaders, Permissions: readScopes},
		{ID: "admob.accounts.adSources.adapters.list", Method: "GET",
			Path:     api + "/v1beta/accounts/{accountsId}/adSources/{adSourcesId}/adapters",
			Resource: "accounts.adSources.adapters", Class: growth.ClassRead, Roles: inventoryReaders,
			Permissions: readScopes},
		{ID: "admob.accounts.mediationGroups.list", Method: "GET",
			Path:     api + "/v1beta/accounts/{accountsId}/mediationGroups",
			Resource: "accounts.mediationGroups", Class: growth.ClassRead, Roles: inventoryReaders,
			Permissions: readScopes, Reason: limitedNote},
		{ID: "admob.accounts.mediationGroups.create", Method: "POST",
			Path:     api + "/v1beta/accounts/{accountsId}/mediationGroups",
			Resource: "accounts.mediationGroups", Class: growth.ClassOperate, Roles: inventoryManagers,
			Permissions: writeScopes, Reason: limitedNote},
		// A LINE SET TO REMOVED CANNOT BE ENABLED AGAIN, only added anew, so that
		// value is admin; enabling and disabling a line stay operate (Pausing
		// names the values that stay operate). AdMob pays the publisher, so no
		// line, eCPM or targeting change spends.
		{ID: "admob.accounts.mediationGroups.patch", Method: "PATCH",
			Path:     api + "/v1beta/accounts/{accountsId}/mediationGroups/{mediationGroupsId}",
			Resource: "accounts.mediationGroups", Class: growth.ClassOperate, Danger: true,
			Fields: []growth.FieldRule{{Field: lineStateField, Class: growth.ClassAdmin,
				Pausing: []any{lineEnabled, lineDisabled}}},
			Roles: inventoryManagers, Permissions: writeScopes,
			Reason: limitedNote},
		{ID: "admob.accounts.mediationGroups.mediationAbExperiments.create", Method: "POST",
			Path:     api + "/v1beta/accounts/{accountsId}/mediationGroups/{mediationGroupsId}/mediationAbExperiments",
			Resource: "accounts.mediationGroups.mediationAbExperiments", Class: growth.ClassOperate,
			Roles: inventoryManagers, Permissions: writeScopes,
			Reason: limitedNote},
		// KEEPING THE TREATMENT REPLACES THE GROUP'S LINES with its own, so
		// every control line it lacks is removed, as a line set to REMOVED is:
		// admin. Keeping the control changes no line (Pausing names it).
		{ID: "admob.accounts.mediationGroups.mediationAbExperiments.stop", Method: "POST",
			Path:     api + "/v1beta/accounts/{accountsId}/mediationGroups/{mediationGroupsId}/mediationAbExperiments:stop",
			Resource: "accounts.mediationGroups.mediationAbExperiments", Class: growth.ClassOperate, Danger: true,
			Fields: []growth.FieldRule{{Field: "variantChoice", Class: growth.ClassAdmin, Pausing: []any{variantA}}},
			Roles:  inventoryManagers, Permissions: writeScopes,
			Reason: limitedNote},
		{ID: networkReportOp, Method: "POST", Path: api + "/v1/accounts/{accountsId}/networkReport:generate",
			Resource: "accounts.networkReport", Class: growth.ClassRead, Reads: true, Roles: reportReaders,
			Permissions: readScopes},
		{ID: mediationReportOp, Method: "POST", Path: api + "/v1/accounts/{accountsId}/mediationReport:generate",
			Resource: "accounts.mediationReport", Class: growth.ClassRead, Reads: true, Roles: reportReaders,
			Permissions: readScopes},
		{ID: campaignReportOp, Method: "POST", Path: api + "/v1beta/accounts/{accountsId}/campaignReport:generate",
			Resource: "accounts.campaignReport", Class: growth.ClassRead, Reads: true, Roles: reportReaders,
			Permissions: readScopes},
	}
}

// The variants an experiment is stopped keeping.
const (
	variantA = "VARIANT_CHOICE_A"
	variantB = "VARIANT_CHOICE_B"
)

// The report operations, which the report tools call.
const (
	networkReportOp   = "admob.accounts.networkReport.generate"
	mediationReportOp = "admob.accounts.mediationReport.generate"
	campaignReportOp  = "admob.accounts.campaignReport.generate"
)

// limitedNote is the note of a method Google calls limited access: it answers
// 403 until Google allows the account, which its account manager asks for.
const limitedNote = "Limited access: Google allows it once the account's AdMob account manager asks."

// limitedAccess reports a method Google answers 403 until it allows the account.
func limitedAccess(op growth.Operation) bool { return op.Reason == limitedNote }
