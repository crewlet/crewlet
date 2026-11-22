package adsense

import (
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/growth"
)

// method is one row of the catalog: its operation, and the query parameters
// it takes, from the discovery document (anything else is refused).
type method struct {
	op    growth.Operation
	query []string
}

// The query parameters methods share.
var (
	paging     = []string{"pageSize", "pageToken"}
	mask       = []string{"updateMask"}
	savedQuery = []string{"dateRange", "startDate.year", "startDate.month", "startDate.day", "endDate.year",
		"endDate.month", "endDate.day", "currencyCode", "languageCode", "reportingTimeZone"}
	reportQuery = append(append([]string{}, savedQuery...), "dimensions", "metrics", "filters", "orderBy", "limit")
)

// repeated are the query parameters Google repeats, a report's alone; every
// other parameter takes one value, since the engine reads one.
var repeated = []string{"dimensions", "metrics", "filters", "orderBy"}

// The kinds of call that need more than the account (operations' Reason).
const (
	restrictedNote = "Restricted: Google answers it only for projects it enables for AdSense for Platforms, " +
		"which the account manager asks for."
	platformNote = "AdSense for Platforms: answers only a platform's parent account, once Google turns on the " +
		"Platform API for the project."
)

// row builds one method: reads need adsense.readonly and writes adsense.
func row(id, verb, path string, class growth.Class, note string, query ...string) method {
	resource := strings.TrimPrefix(strings.TrimPrefix(id, "adsense."), "adsenseplatform.")
	resource = resource[:strings.LastIndex(resource, ".")]
	perms := []string{scopeReadonly}
	if class != growth.ClassRead {
		perms = []string{scopeManage}
	}
	return method{op: growth.Operation{ID: id, Method: verb, Path: path, Resource: resource, Class: class,
		Permissions: perms, Reason: note}, query: query}
}

// methods is every method of the two APIs, each once: the Management API's
// 33 on v2, the Platform API's 11 stable ones on v1 and its 8 others on
// v1alpha, where alone they are.
var methods = sync.OnceValue(func() []method {
	const (
		acct    = management + "/accounts/{accountsId}"
		client  = acct + "/adclients/{adclientsId}"
		unit    = client + "/adunits/{adunitsId}"
		channel = client + "/customchannels/{customchannelsId}"
		sub     = platform + "/platforms/{platformsId}/accounts/{accountsId}"
		plat    = alpha + "/accounts/{accountsId}/platforms/{platformsId}"
		child   = plat + "/childAccounts/{childAccountsId}"
		read    = growth.ClassRead
	)
	out := []method{
		row("adsense.accounts.list", "GET", management+"/accounts", read, "", paging...),
		row("adsense.accounts.get", "GET", acct, read, ""),
		row("adsense.accounts.listChildAccounts", "GET", acct+":listChildAccounts", read, "", paging...),
		row("adsense.accounts.getAdBlockingRecoveryTag", "GET", acct+"/adBlockingRecoveryTag", read, ""),
		row("adsense.accounts.adclients.list", "GET", acct+"/adclients", read, "", paging...),
		row("adsense.accounts.adclients.get", "GET", client, read, ""),
		row("adsense.accounts.adclients.getAdcode", "GET", client+"/adcode", read, ""),
		row("adsense.accounts.adclients.adunits.list", "GET", client+"/adunits", read, "", paging...),
		row("adsense.accounts.adclients.adunits.get", "GET", unit, read, ""),
		row("adsense.accounts.adclients.adunits.getAdcode", "GET", unit+"/adcode", read, ""),
		row("adsense.accounts.adclients.adunits.listLinkedCustomChannels", "GET", unit+":listLinkedCustomChannels",
			read, "", paging...),
		row("adsense.accounts.adclients.adunits.create", "POST", client+"/adunits", growth.ClassOperate,
			restrictedNote),
		row("adsense.accounts.adclients.adunits.patch", "PATCH", unit, growth.ClassOperate, restrictedNote, mask...),
		row("adsense.accounts.adclients.customchannels.list", "GET", client+"/customchannels", read, "", paging...),
		row("adsense.accounts.adclients.customchannels.get", "GET", channel, read, ""),
		row("adsense.accounts.adclients.customchannels.listLinkedAdUnits", "GET", channel+":listLinkedAdUnits",
			read, "", paging...),
		row("adsense.accounts.adclients.customchannels.create", "POST", client+"/customchannels",
			growth.ClassOperate, restrictedNote),
		row("adsense.accounts.adclients.customchannels.patch", "PATCH", channel, growth.ClassOperate,
			restrictedNote, mask...),
		row("adsense.accounts.adclients.customchannels.delete", "DELETE", channel, growth.ClassAdmin,
			restrictedNote),
		row("adsense.accounts.adclients.urlchannels.list", "GET", client+"/urlchannels", read, "", paging...),
		row("adsense.accounts.adclients.urlchannels.get", "GET", client+"/urlchannels/{urlchannelsId}", read, ""),
		row("adsense.accounts.alerts.list", "GET", acct+"/alerts", read, "", "languageCode"),
		row("adsense.accounts.payments.list", "GET", acct+"/payments", read, ""),
		row("adsense.accounts.policyIssues.list", "GET", acct+"/policyIssues", read, "", paging...),
		row("adsense.accounts.policyIssues.get", "GET", acct+"/policyIssues/{policyIssuesId}", read, ""),
		row(reportOp, "GET", acct+"/reports:generate", read, "", reportQuery...),
		row("adsense.accounts.reports.generateCsv", "GET", acct+"/reports:generateCsv", read, "", reportQuery...),
		row("adsense.accounts.reports.getSaved", "GET", acct+"/reports/{reportsId}/saved", read, ""),
		row(savedListOp, "GET", acct+"/reports/saved", read, "", paging...),
		row(savedReportOp, "GET", acct+"/reports/{reportsId}/saved:generate", read, "", savedQuery...),
		row("adsense.accounts.reports.saved.generateCsv", "GET", acct+"/reports/{reportsId}/saved:generateCsv",
			read, "", savedQuery...),
		row("adsense.accounts.sites.list", "GET", acct+"/sites", read, "", paging...),
		row("adsense.accounts.sites.get", "GET", acct+"/sites/{sitesId}", read, ""),

		row("adsenseplatform.platforms.accounts.list", "GET", platform+"/platforms/{platformsId}/accounts", read,
			platformNote, paging...),
		row("adsenseplatform.platforms.accounts.get", "GET", sub, read, platformNote),
		row("adsenseplatform.platforms.accounts.lookup", "GET", platform+"/platforms/{platformsId}/accounts:lookup",
			read, platformNote, "creationRequestId"),
		row("adsenseplatform.platforms.accounts.create", "POST", platform+"/platforms/{platformsId}/accounts",
			growth.ClassAdmin, platformNote),
		row("adsenseplatform.platforms.accounts.close", "POST", sub+":close", growth.ClassAdmin, platformNote),
		row("adsenseplatform.platforms.accounts.events.create", "POST", sub+"/events", growth.ClassAdmin,
			platformNote),
		row("adsenseplatform.platforms.accounts.sites.list", "GET", sub+"/sites", read, platformNote, paging...),
		row("adsenseplatform.platforms.accounts.sites.get", "GET", sub+"/sites/{sitesId}", read, platformNote),
		row("adsenseplatform.platforms.accounts.sites.create", "POST", sub+"/sites", growth.ClassAdmin, platformNote),
		row("adsenseplatform.platforms.accounts.sites.delete", "DELETE", sub+"/sites/{sitesId}", growth.ClassAdmin,
			platformNote),
		row("adsenseplatform.platforms.accounts.sites.requestReview", "POST", sub+"/sites/{sitesId}:requestReview",
			growth.ClassPublic, platformNote),

		row("adsenseplatform.accounts.platforms.list", "GET", alpha+"/accounts/{accountsId}/platforms", read,
			platformNote, paging...),
		row("adsenseplatform.accounts.platforms.get", "GET", plat, read, platformNote),
		row("adsenseplatform.accounts.platforms.groups.list", "GET", plat+"/groups", read, platformNote, paging...),
		row("adsenseplatform.accounts.platforms.groups.get", "GET", plat+"/groups/{groupsId}", read, platformNote),
		row("adsenseplatform.accounts.platforms.groups.patch", "PATCH", plat+"/groups/{groupsId}",
			growth.ClassOperate, platformNote, mask...),
		row("adsenseplatform.accounts.platforms.childAccounts.sites.list", "GET", child+"/sites", read,
			platformNote, paging...),
		row("adsenseplatform.accounts.platforms.childAccounts.sites.get", "GET", child+"/sites/{sitesId}", read,
			platformNote),
		row("adsenseplatform.accounts.platforms.childAccounts.sites.patch", "PATCH", child+"/sites/{sitesId}",
			growth.ClassAdmin, platformNote, mask...),
	}
	for i := range out {
		switch m := &out[i].op; m.ID {
		case adUnitPatchOp:
			// A LIVE AD UNIT'S SIZE OR TYPE IS THE SLOT EVERY VISITOR SEES on
			// the pages carrying its code: public. Its name and state are not.
			m.Fields = []growth.FieldRule{{Field: adSettingsField, Class: growth.ClassPublic}}
		case "adsense.accounts.adclients.customchannels.delete", "adsenseplatform.platforms.accounts.close",
			"adsenseplatform.platforms.accounts.events.create", "adsenseplatform.platforms.accounts.sites.delete":
			// NO UNDO: a deleted channel's ID is gone from reports, a closed
			// account stays closed, and a sent event cannot be withdrawn.
			m.Danger = true
		}
	}
	return out
})

// operations is every method's operation.
func operations() []growth.Operation {
	out := make([]growth.Operation, 0, len(methods()))
	for _, m := range methods() {
		out = append(out, m.op)
	}
	return out
}

// queryKeys are the query parameters op takes.
func queryKeys(op growth.Operation) []string {
	for _, m := range methods() {
		if m.op.ID == op.ID {
			return m.query
		}
	}
	return nil
}

// The operations the curated tools and the engine's own reads call.
const (
	accountGetOp     = "adsense.accounts.get"
	reportOp         = "adsense.accounts.reports.generate"
	savedListOp      = "adsense.accounts.reports.saved.list"
	savedReportOp    = "adsense.accounts.reports.saved.generate"
	adUnitPatchOp    = "adsense.accounts.adclients.adunits.patch"
	childSitePatchOp = "adsenseplatform.accounts.platforms.childAccounts.sites.patch"
)

// adSettingsField is an ad unit's size and type, which its field rule classes.
const adSettingsField = "contentAdsSettings"

// restricted reports a method Google answers only for projects it enables.
func restricted(op growth.Operation) bool { return op.Reason == restrictedNote || platformOnly(op) }

// platformOnly reports a method of the Platform API.
func platformOnly(op growth.Operation) bool { return op.Reason == platformNote }

// getters are the reads of what each write acts on, by the write: its
// target's live state, which a write is classed against and a held call
// binds to. A create acts on nothing that exists yet, so it has none, but
// for a platform sub-account's, which binds to the sub-account it is made in.
var getters = map[string]string{
	adUnitPatchOp: "adsense.accounts.adclients.adunits.get",
	"adsense.accounts.adclients.customchannels.patch":  "adsense.accounts.adclients.customchannels.get",
	"adsense.accounts.adclients.customchannels.delete": "adsense.accounts.adclients.customchannels.get",
	"adsenseplatform.accounts.platforms.groups.patch":  "adsenseplatform.accounts.platforms.groups.get",
	childSitePatchOp: "adsenseplatform.accounts.platforms.childAccounts.sites.get",
	"adsenseplatform.platforms.accounts.close":               "adsenseplatform.platforms.accounts.get",
	"adsenseplatform.platforms.accounts.sites.delete":        "adsenseplatform.platforms.accounts.sites.get",
	"adsenseplatform.platforms.accounts.sites.requestReview": "adsenseplatform.platforms.accounts.sites.get",
	"adsenseplatform.platforms.accounts.sites.create":        "adsenseplatform.platforms.accounts.get",
	"adsenseplatform.platforms.accounts.events.create":       "adsenseplatform.platforms.accounts.get",
}
