package appstoreconnect_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/appstoreconnect"
	"github.com/crewlet/crewlet/internal/seataccess"
)

const agreementBody = `{"errors": [{"status": "403", "code": "FORBIDDEN.REQUIRED_AGREEMENTS_MISSING_OR_EXPIRED",
	"title": "A required agreement is missing or has expired.",
	"detail": "This request requires an in-effect agreement that has not been signed or has expired."}]}`

// APPLE'S DEFINITE ANSWER IS A REFUSAL A PERSON ACTS ON, never Crewlet's own
// fault: each code it tells apart says who does what, an unknown 4xx shows
// Apple's own words, and a 5xx or a rate limit stays Apple not answering.
//
// Mutation: answer nil for a 4xx, and the identity sheet says Crewlet could
// not finish the check for a team whose agreement lapsed.
func TestApplesRefusalsSayWhoActsAndWhere(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name         string
		err          appstoreconnect.APIError
		said, remedy string
	}{
		{"an unsigned agreement", appstoreconnect.APIError{Status: 403,
			Code: "FORBIDDEN.REQUIRED_AGREEMENTS_MISSING_OR_EXPIRED", Title: "A required agreement is missing"},
			"Apple refuses every API call for this team until its Account Holder accepts the latest agreement.",
			"Sign in to App Store Connect as the Account Holder, accept the agreement shown on the home page or " +
				"under Business, then check again."},
		{"a key Apple does not accept", appstoreconnect.APIError{Status: 401, Code: "NOT_AUTHORIZED"},
			"Apple does not accept the key: it was revoked, or its Key ID or the team's Issuer ID is wrong.",
			"Team Keys"},
		{"a role that may not", appstoreconnect.APIError{Status: 403, Code: "FORBIDDEN_ERROR"},
			"Apple refuses the call: the key's role does not allow it.", "cannot change a key's role"},
		{"a code Crewlet does not know", appstoreconnect.APIError{Status: 409, Code: "ENTITY_ERROR.NEW",
			Title: "The request cannot be completed.", Detail: "Something is missing"},
			"App Store Connect refused the call: The request cannot be completed. Something is missing.",
			"Do what Apple's answer asks"},
		{"no words at all", appstoreconnect.APIError{Status: 404},
			"App Store Connect refused the call (HTTP 404).", "Do what Apple's answer asks"},
	} {
		r := c.err.VendorRefusal()
		if r == nil {
			t.Fatalf("%s: no refusal", c.name)
		}
		if r.Said != c.said || !strings.Contains(r.Remedy, c.remedy) || r.Actor != seataccess.ByYou {
			t.Errorf("%s: said %q remedy %q actor %q", c.name, r.Said, r.Remedy, r.Actor)
		}
	}
	for _, status := range []int{500, 503, http.StatusTooManyRequests} {
		if r := (&appstoreconnect.APIError{Status: status}).VendorRefusal(); r != nil {
			t.Errorf("Apple not serving the call (%d) read as a refusal: %+v", status, r)
		}
	}
}

// A TEAM WITH AN UNSIGNED AGREEMENT FAILS THE FIRST STEP in Apple's plain
// terms: the check answers a verdict rather than a fault, its one read is
// the sign-in, failed with the reason, and nothing after it ran.
//
// Mutation: take the team's 403 as an accepted sign-in, and the check fails
// later at the apps, or as a fault the sheet calls unfinished.
func TestAnUnsignedAgreementFailsTheFirstStep(t *testing.T) {
	t.Parallel()
	rig := newIdentityRig(t, "operate")
	rig.apple.roles["7KQ4M2ZX9P"] = appstoreconnect.RoleAppManager
	rig.apple.fixed = map[string]answer{"*": {http.StatusForbidden, agreementBody}}
	kept, err := rig.keep(t, p8(t), "AuthKey_7KQ4M2ZX9P.p8", nil, "operate")
	if err != nil {
		t.Fatalf("Apple's refusal came back as a fault: %v", err)
	}
	c := kept.Check
	if c.Status != seataccess.IdentityRefused || !strings.Contains(c.Said, "Account Holder accepts") ||
		!strings.Contains(c.Remedy, "accept the agreement") {
		t.Fatalf("check %+v", c)
	}
	if len(c.Reads) != 1 || c.Reads[0].OK || !strings.HasPrefix(c.Reads[0].Label, "Sign in") || c.Reads[0].Said != c.Said {
		t.Fatalf("reads %+v, want the sign-in alone, failed with Apple's reason", c.Reads)
	}
}

// ONLY APPLE'S ROLE DENIAL SAYS A ROLE MAY NOT: a probe Apple answers 404 or a
// 403 that is not FORBIDDEN_ERROR says nothing of the role, so an App Manager
// key still reads as App Manager, and each probe's answer is on the role read.
//
// Mutation: count any 403 as "may not" or a 404 as "may", and the App
// Manager key fits no role, which the sheet calls a role Crewlet cannot tell.
func TestAnAnswerThatSaysNothingOfTheRoleIsLeftOut(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, said string
		fixed      answer
	}{
		{"a 404", "404 NOT_FOUND", answer{http.StatusNotFound, `{"errors": [{"status": "404", "code": "NOT_FOUND"}]}`}},
		{"another 403's code", "403 FORBIDDEN.REQUIRED_AGREEMENTS_MISSING_OR_EXPIRED",
			answer{http.StatusForbidden, agreementBody}},
		{"a 409", "409 ENTITY_ERROR", answer{http.StatusConflict, `{"errors": [{"status": "409", "code": "ENTITY_ERROR"}]}`}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			rig := newIdentityRig(t, "operate")
			rig.apple.roles["7KQ4M2ZX9P"] = appstoreconnect.RoleAppManager
			rig.apple.fixed = map[string]answer{"/v1/apps/111/analyticsReportRequests": c.fixed}
			kept, err := rig.keep(t, p8(t), "AuthKey_7KQ4M2ZX9P.p8", nil, "operate")
			if err != nil || kept.Check.Status != seataccess.IdentityReady || kept.Check.Role != "App Manager" {
				t.Fatalf("check %+v, %v", kept.Check, err)
			}
			role := kept.Check.Reads[len(kept.Check.Reads)-1]
			if !strings.Contains(role.Detail, "apps_analyticsReportRequests_getToManyRelated "+c.said) ||
				!strings.Contains(role.Detail, "apps_backgroundAssets_getToManyRelated 200") {
				t.Fatalf("the role read does not show Apple's answers: %q", role.Detail)
			}
			if strings.Contains(role.Detail, "eyJ") || strings.Contains(role.Detail, "PRIVATE") {
				t.Fatalf("the role read carries a credential: %q", role.Detail)
			}
		})
	}
}

// SEVERAL ROLES THE ANSWERS LEAVE ARE ACCEPTED ONLY WHERE EACH FITS: with the
// one probe that tells App Manager from Marketing unanswered, a read agent's
// key is kept and named as either, and an operate agent's is refused, naming
// both and where to check the key's Access.
//
// Mutation: accept any several, and a Marketing key runs an operate agent.
func TestSeveralRolesAreKeptOnlyWhereEachFits(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		level  string
		status seataccess.IdentityStatus
		said   string
	}{
		{"read", seataccess.IdentityReady, "holds the App Manager or Marketing role"},
		{"operate", seataccess.IdentityWrongRole, "one of App Manager or Marketing"},
	} {
		t.Run(c.level, func(t *testing.T) {
			t.Parallel()
			rig := newIdentityRig(t, c.level)
			rig.apple.roles["7KQ4M2ZX9P"] = appstoreconnect.RoleAppManager
			rig.apple.fixed = map[string]answer{"/v1/apps/111/backgroundAssets": {http.StatusNotFound,
				`{"errors": [{"status": "404", "code": "NOT_FOUND"}]}`}}
			kept, err := rig.keep(t, p8(t), "AuthKey_7KQ4M2ZX9P.p8", nil, c.level)
			if err != nil || kept.Check.Status != c.status || !strings.Contains(kept.Check.Said, c.said) {
				t.Fatalf("check %+v, %v", kept.Check, err)
			}
			if c.status != seataccess.IdentityReady && !strings.Contains(kept.Check.Remedy, "Team Keys") {
				t.Fatalf("remedy %q does not say where to check the key's Access", kept.Check.Remedy)
			}
		})
	}
}

// ANSWERS THAT FIT NO ROLE SAY WHICH PROBES EACH ROLE DISAGREES ON, and the
// role read is the failed one: the apps read before it passed.
//
// Mutation: drop the disagreements, and a mismatch is a mystery again.
func TestAnswersThatFitNoRoleSayWhereEachDisagrees(t *testing.T) {
	t.Parallel()
	rig := newIdentityRig(t, "operate")
	rig.apple.roles["7KQ4M2ZX9P"] = appstoreconnect.RoleAppManager
	rig.apple.fixed = map[string]answer{"/v1/financeReports": {http.StatusOK, `{"data": []}`}}
	kept, err := rig.keep(t, p8(t), "AuthKey_7KQ4M2ZX9P.p8", nil, "operate")
	if err != nil || kept.Check.Status != seataccess.IdentityWrongRole ||
		!strings.Contains(kept.Check.Said, `App Manager on "financeReports_getCollection"`) {
		t.Fatalf("check %+v, %v", kept.Check, err)
	}
	reads := kept.Check.Reads
	if len(reads) != 4 || !reads[2].OK || reads[3].OK || !strings.HasPrefix(reads[3].Label, "Read its role") {
		t.Fatalf("reads %+v, want the role read last and failed, after the apps read passed", reads)
	}
}

// THE CHECK READS IN THE ORDER THE SHEET LISTS: a key whose team lacks the
// agent's apps fails at the apps read, and no role read follows it.
//
// Mutation: probe the role before the apps, and the sheet draws a role step
// done that the verdict never reports.
func TestTheAppsReadComesBeforeTheRole(t *testing.T) {
	t.Parallel()
	rig := newIdentityRig(t, "operate")
	rig.apple.roles["7KQ4M2ZX9P"] = appstoreconnect.RoleAppManager
	rig.apple.fixed = map[string]answer{"/v1/apps": {http.StatusOK, `{"data": []}`}}
	kept, err := rig.keep(t, p8(t), "AuthKey_7KQ4M2ZX9P.p8", nil, "operate")
	if err != nil || kept.Check.Status != seataccess.IdentityWrongTeam {
		t.Fatalf("check %+v, %v", kept.Check, err)
	}
	reads := kept.Check.Reads
	if len(reads) != 3 || reads[2].OK || !strings.HasPrefix(reads[2].Label, "Find the agent's apps") {
		t.Fatalf("reads %+v, want the apps read last and failed", reads)
	}
	for _, call := range rig.apple.calls {
		if strings.Contains(call, "financeReports") {
			t.Fatalf("the role was probed after the apps read failed: %v", rig.apple.calls)
		}
	}
}
