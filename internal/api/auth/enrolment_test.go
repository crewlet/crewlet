package auth_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/session"
)

// A SESSION THAT MAY ONLY ENROL A SECOND FACTOR REACHES THE ROUTES THAT LET ITS
// PERSON IN OR OUT, AND NOTHING ELSE.
//
// `api.auth.local.totp: required` was validated, documented and enforced by
// nothing, so a password alone reached every surface on a deployment that
// required a second factor. A sign-in that proves only a password there now
// opens a session marked enrolment-only, and the guard answers every guarded
// route but three `403 second_factor_enrolment_required` — the /auth surface
// included, whose other routes mint recovery codes and end every session the
// person holds, and the socket too. The sign-out of this session is not
// guarded at all, so it is reached too. The person stays RESOLVED on every
// admitted route, which need to know who they are.
//
// The CONTROL is the same session unmarked, which reaches every route — so the
// refusals are the mark's and nothing else's. Mutation: drop the guard's check
// and every refused row reaches its handler; widen the three to the /auth
// prefix and the recovery and sign-out-everywhere rows do; guard the sign-out
// again and its row is refused.
func TestASessionThatMayOnlyEnrolReachesOnlyTheRoutesThatLetItIn(t *testing.T) {
	t.Parallel()
	routes := []struct {
		method, path string
		admitted     bool
	}{
		{http.MethodGet, auth.PathAuthSession, true},
		{http.MethodPost, auth.PathAuthTOTP, true},
		{http.MethodPost, auth.PathAuthStepUp, true},
		{http.MethodPost, auth.PathAuthLogout, true},
		{http.MethodPost, "/auth/totp/recovery", false},
		{http.MethodPost, "/auth/logout/all", false},
		{http.MethodPost, "/auth/logout/018f3a9c-0000-7000-8000-0000000000bb", false},
		{http.MethodPost, "/auth/token", false},
		{http.MethodGet, "/auth/totp", false},
		{http.MethodGet, "/iam/people", false},
		{http.MethodPost, "/work/items", false},
		{http.MethodGet, "/agents", false},
		{http.MethodGet, auth.SocketPath, false},
	}
	for _, restricted := range []bool{true, false} {
		s := newSignedIn(t)
		s.dir.identity.Session.EnrolmentOnly = restricted
		g := s.guard()
		for _, route := range routes {
			got := s.call(g, route.method, route.path, s.withCookie)
			refused := restricted && !route.admitted
			switch {
			case refused && (got.status != http.StatusForbidden ||
				got.body["error"] != string(httpjson.CodeSecondFactorEnrolmentRequired)):
				t.Errorf("an enrolment-only session reached %s %s: %d %v",
					route.method, route.path, got.status, got.body)
			case !refused && (got.status != http.StatusOK || got.how != iam.Resolved):
				t.Errorf("restricted=%v: %s %s answered %d (%v), want it served "+
					"to the resolved person", restricted, route.method,
					route.path, got.status, got.body)
			case !refused && got.principal.Login != "sarah.chen":
				t.Errorf("%s %s was served to %q, want the person the session "+
					"names", route.method, route.path, got.principal.Login)
			}
		}
	}
}

// AN ENROLMENT-ONLY SESSION WHOSE SEAT IS ALSO GONE IS REFUSED AS THE FIRST.
//
// Two refusals meet on one person and they leave different routes: a lost
// seat leaves the whole /auth surface, an enrolment-only session four routes of
// it. The narrower is the one that holds, so a person who must enrol first is
// never handed recovery codes because their seat happened to move — and the
// enrolment itself is still reachable. Mutation: let the seat refusal win and
// the recovery row is served.
func TestAnEnrolmentOnlySessionWithoutItsSeatIsRefusedAsTheNarrowerOfTheTwo(t *testing.T) {
	t.Parallel()
	s := newSignedIn(t)
	s.dir.identity.Session.EnrolmentOnly = true
	s.chart.seats = map[string]session.Seat{}
	g := s.guard()
	for _, route := range []struct {
		method, path string
		status       int
	}{
		{http.MethodGet, "/iam/people", http.StatusForbidden},
		{http.MethodPost, "/auth/totp/recovery", http.StatusForbidden},
		{http.MethodPost, auth.PathAuthTOTP, http.StatusOK},
	} {
		got := s.call(g, route.method, route.path, s.withCookie)
		if got.status != route.status {
			t.Errorf("%s %s answered %d (%v), want %d", route.method, route.path,
				got.status, got.body, route.status)
			continue
		}
		if route.status == http.StatusForbidden &&
			got.body["error"] != string(httpjson.CodeSecondFactorEnrolmentRequired) {
			t.Errorf("%s %s was refused as %v, want the enrolment refusal",
				route.method, route.path, got.body["error"])
		}
	}
}

// AN ENROLMENT-ONLY SESSION IS RESTRICTED ON A NODE THAT HAS NOT APPLIED IT.
//
// The restriction was read off the session's ROW, and a node below the
// bearer's start position has no row: it serves reads on the signature and the
// epoch alone ([session.RowBehind]), with the zero row beside them. Every
// sign-in answers before any node applies its session, so for the apply
// latency after every password sign-in — on every node, the one that answered
// it included — a password alone read everything its person's grants reach,
// the socket's snapshot among it, on a deployment that requires a second
// factor. The sign-in's own bearer carries the restriction now, signed, and
// the guard reads it there as well.
//
// The CONTROL is a whole bearer on the same lagging node, which is served — so
// the refusals are the bearer's mark and not the lag's. Mutation: read the
// mark off the row alone and every refused row reaches its handler.
func TestAnEnrolmentOnlySessionIsRestrictedOnANodeThatHasNotAppliedIt(t *testing.T) {
	t.Parallel()
	for _, restricted := range []bool{true, false} {
		s := newSignedIn(t)
		cookie, err := s.signer.Mint(session.Mint{
			Lineage: s.lineage, Person: sessionPerson, Epoch: 3, Generation: 1,
			StartPosition: sessionStart, AbsoluteExpiresAt: s.at.Add(8 * time.Hour),
			EnrolmentOnly: restricted,
		})
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		// THE NODE HAS NOT APPLIED THE SESSION'S START: no row, its
		// position below the bearer's.
		s.dir.identity.Session = session.LineageRow{}
		s.dir.identity.Applied = sessionStart - 1
		present := func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: session.CookieBaseName, Value: cookie})
		}
		g := s.guard()
		for _, route := range []struct {
			method, path string
			admitted     bool
		}{
			{http.MethodGet, "/iam/people", false},
			{http.MethodGet, "/agents", false},
			{http.MethodGet, "/config", false},
			// THE SOCKET'S HANDSHAKE IS A GET, which a lagging node
			// serves — and its snapshot goes out the moment it upgrades.
			{http.MethodGet, auth.SocketPath, false},
			{http.MethodGet, auth.PathAuthSession, true},
		} {
			got := s.call(g, route.method, route.path, present)
			refused := restricted && !route.admitted
			switch {
			case refused && (got.status != http.StatusForbidden ||
				got.body["error"] != string(httpjson.CodeSecondFactorEnrolmentRequired)):
				t.Errorf("an enrolment-only session reached %s %s on a node "+
					"that has not applied it: %d %v", route.method, route.path,
					got.status, got.body)
			case !refused && (got.status != http.StatusOK || got.how != iam.Resolved):
				t.Errorf("restricted=%v: %s %s answered %d (%v), want it served",
					restricted, route.method, route.path, got.status, got.body)
			}
		}
	}
}
