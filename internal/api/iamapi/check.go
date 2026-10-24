package iamapi

import (
	"context"
	"net/http"
	"slices"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
)

// FindingKind is one thing the directory report can say.
//
// A CLOSED SET with a row per kind, for internal/authz's reason: a finding
// with no kind ships looking deliberate, and a screen keyed on a string
// somebody typed filters to nothing the day it is spelled differently.
type FindingKind string

const (
	// KindNoAdminHolder is the one that locks a company out: no ACTIVE
	// PERSON holding an ENROLLED credential — a password, a second factor,
	// a token: a way in they have USED, so a link nobody has spent yet does
	// not count — holds admin, so nobody can invite, grant or revoke ever
	// again except through a Tier A token.
	//
	// PERSONS ONLY. A service account may hold admin, and its live token is
	// an enrolled credential — but no administrative identity record is
	// ever written through a machine token (internal/iamdomain refuses it,
	// and internal/authz's presence rows refuse the routes in front of it),
	// so a company whose only admin is a service account is one nobody can
	// invite into, grant in or revoke in. Counted, it would read as
	// administrable while it is locked.
	KindNoAdminHolder FindingKind = "no_admin_holder"

	// KindNoCredential is somebody active who holds no live way in at all:
	// a person whose first password link — from an administrator's create —
	// lapsed unspent or was revoked, or whose every credential has been;
	// or a service account holding no live token. A LIVE link — the first
	// one a create issued, or a reset link — is a way in, so a person
	// created a minute ago is not reported until it lapses. The repair is a
	// password reset link (`POST /iam/people/{id}/password-reset`) for a
	// person and a token (`POST /iam/credentials?person={id}`) for a service
	// account, or removing them. An invitation creates nobody until it is
	// redeemed, with the password its invitee chose, so it is never the
	// cause.
	KindNoCredential FindingKind = "person_without_credential"

	// KindDanglingBinding is somebody bound to a seat the company this node
	// runs does not hold as a human seat: removed, turned into an agent
	// seat, or — on a node that has not applied the revision yet — a hire.
	// A LEGAL RESIDUE rather than corruption — the directory and the
	// company are written apart, so a bind and a revision removing its seat
	// can both land — and the repair is one record: a person is moved to
	// another human seat or removed, a service account unbound or moved.
	// The rule's sentence says which ([Bindings]). This report is the one
	// place it is said: it is a fact about the company's content, which
	// every node would raise at once as an alarm and none could repair
	// (ADR-0015).
	KindDanglingBinding FindingKind = "binding_dangling"

	// KindAdminClamped is a row holding admin on a node whose
	// `api.auth.admin` is `refuse`: the row declares admin and this node
	// does not honour it — every app it includes, it still does. A LEGAL
	// state — the posture is applied at decision time and never written,
	// and a node published to the people who use the company refuses
	// admin by design — and worth saying, because the row and the
	// behaviour differ and nothing else would say so.
	KindAdminClamped FindingKind = "admin_clamped_by_node"
)

// FindingKinds are the four, in the order the report is SORTED in: what ends a
// company's ability to administer itself first, then somebody who cannot get
// in, a binding the chart lost, and an admin this node clamps away. The report
// sorts by it, stably — so within a kind the
// directory's own order holds — which is what makes the order this says true:
// it was declared "the order the report renders them" and read by nothing,
// while findings came out person by person.
var FindingKinds = []FindingKind{
	KindNoAdminHolder, KindNoCredential, KindDanglingBinding, KindAdminClamped,
}

// Finding is one row of the report.
type Finding struct {
	Kind   FindingKind `json:"kind"`
	Person string      `json:"person,omitempty"`
	Login  string      `json:"login,omitempty"`
	Seat   string      `json:"seat,omitempty"`
	Grant  iam.Grant   `json:"grant,omitempty"`
	Detail string      `json:"detail"`
}

// Bindings is what the report asks about one person's seat binding: whether it
// dangles, and if so the sentence that says which seat, why and what to do.
//
// ASKED, NEVER RESTATED. The rule is the request path's own seat table — a
// binding dangles exactly when that table would refuse the person or hold them
// off for want of the seat — and the engine applies it: internal/engine's
// Engine.DanglingBinding has this type's shape and is what a node wires. The
// predicate it replaced asked only whether the chart held a row by that handle,
// so a person bound to an AGENT seat, refused on every request they made, was
// one this report said nothing about.
//
// THREE-VALUED: an error is a node that cannot tell — one running no company
// yet — and the report counts it as unchecked rather than reporting a dangling
// binding it could not establish, which would send an administrator to move
// somebody whose seat is there.
//
// NIL-ABLE, AND THE ABSENCE IS THE THIRD VALUE too: a node that cannot ask
// skips the arm rather than guessing.
type Bindings func(ctx context.Context, row iamdomain.PersonRow) (
	dangling bool, detail string, err error)

// GetCheck is `GET /iam/check`.
//
// # It walks the directory, which is why it is a route and not a validation
//
// Everything here is a live-fleet question: who holds what, who has never
// signed in, whose seat went away, whose admin this node clamps away. None of
// it can be answered from a configuration file, which is why `crewlet
// validate` says nothing about people.
func (s *Service) GetCheck(w http.ResponseWriter, r *http.Request) {
	var findings []Finding
	admins := 0
	unchecked := 0
	var position string
	after := ""
	for {
		page, err := s.directory.People(r.Context(), iamdomain.PeopleQuery{
			After: after, Limit: iamdomain.MaxPageSize,
		})
		if err != nil {
			s.unavailable(w, r, "walk the directory", err)
			return
		}
		position = page.At.String()
		for _, row := range page.People {
			// ONLY AN ACTIVE ROW'S WAYS IN ARE ASKED ABOUT — by both
			// questions — so nobody else's credentials are read.
			var held ways
			if row.Stage == iam.StageActive {
				held = s.waysIn(r, row)
			}
			found, checked := s.findingsFor(r, row, held)
			findings = append(findings, found...)
			if !checked {
				unchecked++
			}
			// AN ACTIVE PERSON WHO HAS USED A WAY IN — see
			// [KindNoAdminHolder] for why a service account never
			// counts. Through [iam.Expand], the one comparison every
			// permission goes through.
			if row.Kind == iam.KindPerson && row.Stage == iam.StageActive &&
				slices.Contains(iam.Expand(row.Grants), iam.GrantAdmin) &&
				held.enrolled {
				admins++
			}
		}
		if page.Next == "" {
			break
		}
		after = page.Next
	}
	if admins == 0 {
		// The one that is not somebody to fix but nobody left to fix them
		// with — first in [FindingKinds], so first in the report.
		findings = append(findings, Finding{
			Kind: KindNoAdminHolder,
			Detail: "No active person holds " + string(iam.GrantAdmin) +
				". Only an API token can invite people or change access.",
		})
	}
	// STABLE, so within a kind the directory's order holds and two nodes
	// over the same rows print the same report.
	slices.SortStableFunc(findings, func(a, b Finding) int {
		return slices.Index(FindingKinds, a.Kind) - slices.Index(FindingKinds, b.Kind)
	})
	httpjson.Write(w, http.StatusOK, map[string]any{
		"findings": findings,
		"position": position,
		// PERSONS ONLY — see [KindNoAdminHolder].
		"people_with_admin": admins,
		// SAID RATHER THAN SILENT: "no dangling binding" and "this
		// node's org could not say" are different answers, and a
		// report that folded the second into the first would print
		// "nothing to report" during exactly the outage that hides a
		// residue.
		"bindings_unchecked": unchecked,
	})
}

// findingsFor is everything the report can say about one row, and whether its
// seat binding could be checked at all.
func (s *Service) findingsFor(r *http.Request, row iamdomain.PersonRow,
	held ways) (out []Finding, bindingChecked bool) {

	if row.Stage == iam.StageActive && !held.live {
		// THE REMEDY IS THE KIND'S: a service account has no password, so
		// the reset link a person's finding names is refused for it.
		detail := "This person can't sign in. Send them a password reset link " +
			"(POST /iam/people/" + row.ID + "/password-reset), or remove them."
		if row.Kind == iam.KindMachine {
			detail = "This service account has no working access token. " +
				"Create one (POST /iam/credentials?person=" + row.ID + "), or " +
				"remove it."
		}
		out = append(out, Finding{
			Kind: KindNoCredential, Person: row.ID, Login: row.Login,
			Detail: detail,
		})
	}
	bindingChecked = true
	if row.Seat != "" && s.bindings != nil {
		dangling, detail, err := s.bindings(r.Context(), row)
		switch {
		case err != nil:
			bindingChecked = false
			log.DebugContext(r.Context(), "api_iam_check_binding_unknown",
				"person", row.ID, "error", err)
		case dangling:
			out = append(out, Finding{
				Kind: KindDanglingBinding, Person: row.ID, Login: row.Login,
				Seat: row.Seat, Detail: detail,
			})
		}
	}
	// ONLY ADMIN IS EVER CLAMPED: a refusing node keeps every app, and a
	// name this build does not know is kept in the row and ignored by
	// every node alike, so neither is a difference between this node and
	// the row worth reporting.
	if s.admin == iam.AdminRefuse && slices.Contains(row.Grants, iam.GrantAdmin) {
		out = append(out, Finding{
			Kind: KindAdminClamped, Person: row.ID, Login: row.Login,
			Grant: iam.GrantAdmin,
			Detail: "Their admin access doesn't work on this node " +
				"(`api.auth.admin: refuse`). Sign in on a node that serves " +
				"admin to use it.",
		})
	}
	return out, bindingChecked
}

// ways is what somebody can prove themselves with, as the report's two
// questions about it ask.
type ways struct {
	// live is whether they can get in at all: any live credential, a link
	// nobody has spent yet included — a password, a second factor, a token,
	// a first password link or a reset link.
	live bool

	// enrolled is whether they hold a way in they have USED: any live
	// credential but a link. The administrator count asks this, because a
	// company whose only admin is somebody created a minute ago, link
	// unspent, is one nobody can administer yet — and the finding's own
	// sentence says "enrolled".
	enrolled bool
}

// waysIn reads one row's credentials once, for both of the report's questions.
//
// A SERVICE ACCOUNT holds no password at all, so a token is a way in like any
// other, and the question is never "does this row hold a password".
//
// AN UNREADABLE ANSWER IS BOTH, which is the direction that does not raise a
// false alarm: reporting somebody as credential-less because a read failed
// would send an administrator to issue a link to a person who is perfectly able
// to sign in, and reporting nobody able to administer the company over a read
// is the loudest finding there is.
func (s *Service) waysIn(r *http.Request, row iamdomain.PersonRow) ways {
	held, err := s.directory.Credentials(r.Context(), row.ID)
	if err != nil {
		log.WarnContext(r.Context(), "api_iam_check_credentials_unreadable",
			"person", row.ID, "error", err)
		return ways{live: true, enrolled: true}
	}
	now := s.now()
	var out ways
	for _, c := range held {
		if c.Revoked(now) {
			continue
		}
		out.live = true
		if c.Method != iamdomain.MethodReset {
			out.enrolled = true
		}
	}
	return out
}
