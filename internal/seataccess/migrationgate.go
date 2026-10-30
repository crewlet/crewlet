package seataccess

import (
	"strings"

	"github.com/crewlet/crewlet/internal/integration"
)

// KindName is an integration as its vendor names itself, for a sentence a
// person reads: Atlassian for the account Jira and Confluence share.
func KindName(kind integration.Kind) string {
	if kind == integration.KindAtlassian {
		return "Atlassian"
	}
	if lanes := LanesOf(kind); len(lanes) == 1 {
		return LaneName(lanes[0])
	}
	return string(kind)
}

// UnmigratedFinding is what the access pass reports for an integration the
// company connects whose seats the migration onto per-seat access has not
// moved yet: which integration, that its access migration has not finished,
// and why where the migration said why (a vendor that did not answer, a role
// it does not list). Its agents' accounts are left exactly as they are.
func UnmigratedFinding(kind integration.Kind, reason string) integration.Finding {
	name := KindName(kind)
	detail := name + "'s access migration has not finished, so its agents' accounts " +
		"are left exactly as they are"
	if reason = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(reason), ".")); reason != "" {
		detail += ": " + reason
	}
	return integration.Finding{
		Kind: integration.FindingIdentityMissing, Subject: string(kind), Detail: detail,
		Remedy: "The migration is tried again every minute. Where it names a vendor that " +
			"did not answer or a role it does not list, settle that at " + name,
	}
}
