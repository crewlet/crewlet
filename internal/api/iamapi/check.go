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
	// KindNoManageHolder is the one that locks a company out: no ACTIVE
	// person holding a CREDENTIAL carries people:manage, so nobody can
	// invite, grant or revoke ever again except through a Tier A token.
	KindNoManageHolder FindingKind = "no_people_manage_holder"

	// KindNoCredential is somebody enrolled who has never proved
	// themselves: an invitation that was never redeemed into a password,
	// or an enrolment an administrator created and nobody completed.
	KindNoCredential FindingKind = "person_without_credential"

	// KindDanglingBinding is a person bound to a seat this node's chart
	// does not hold as a human seat: removed, tombstoned, turned into an
	// agent seat, or — on a node whose chart applier is behind — a hire
	// this node has not applied yet. A LEGAL RESIDUE rather than
	// corruption — two logs, two appliers, two anchors, so a bind and a
	// seat removal can both win — and the repair is an unbind or a
	// rebind, which is one record. The same evaluation raises the
	// `iam_binding_dangling` alarm once one has persisted past the stall
	// grace.
	KindDanglingBinding FindingKind = "binding_dangling"

	// KindShredded is a row whose key a removal destroyed. Reported as
	// INFORMATION rather than as a fault: it is what a removal is, and an
	// operator reading a directory with blank names needs to know why.
	KindShredded FindingKind = "identity_shredded"

	// KindClampedGrant is a grant a person's row declares that this
	// node's `api.auth.max_grants` withholds. A LEGAL state on a fleet
	// mid-rollout — the ceiling is applied at decision time and never
	// written — and worth saying, because the row and the behaviour
	// differ and nothing else would say so.
	KindClampedGrant FindingKind = "grant_clamped_by_ceiling"
)

// FindingKinds are the five, in the order the report renders them.
var FindingKinds = []FindingKind{
	KindNoManageHolder, KindNoCredential, KindDanglingBinding,
	KindShredded, KindClampedGrant,
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
// off for want of the seat — and the engine applies it once, for this report
// and for the `iam_binding_dangling` alarm alike. The predicate it replaced
// asked only whether the chart held a row by that handle, so a person bound to
// an AGENT seat, refused on every request they made, was one this report said
// nothing about.
//
// THREE-VALUED: an error is a node that cannot tell — a chart applier past the
// stall grace, an unreadable view — and the report counts it as unchecked
// rather than reporting a dangling binding it could not establish, which
// would send an administrator to unbind somebody whose seat is there.
//
// NIL-ABLE, AND THE ABSENCE IS THE THIRD VALUE too — the same shape
// internal/api/chartapi's `Held` takes, one estate the other way round. A node
// that cannot ask skips the arm rather than guessing.
type Bindings func(ctx context.Context, row iamdomain.PersonRow) (
	dangling bool, detail string, err error)

// GetCheck is `GET /iam/check`.
//
// # It walks the directory, which is why it is a route and not a validation
//
// Everything here is a live-fleet question: who holds what, who has never
// signed in, whose seat went away, which node's ceiling clamps whom. None of
// it can be answered from a configuration file, which is why `crewlet
// validate` says nothing about people.
func (s *Service) GetCheck(w http.ResponseWriter, r *http.Request) {
	var findings []Finding
	manage := 0
	unchecked := 0
	position := ""
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
			found, checked := s.findingsFor(r, row)
			findings = append(findings, found...)
			if !checked {
				unchecked++
			}
			if row.Stage == iam.StageActive &&
				slices.Contains(row.Grants, iam.GrantPeopleManage) &&
				s.hasCredential(r, row) {
				manage++
			}
		}
		if page.Next == "" {
			break
		}
		after = page.Next
	}
	if manage == 0 {
		// FIRST IN THE LIST, because it is the one that ends a company's
		// ability to administer itself: every other finding is somebody
		// to fix, and this one is nobody left to fix them with.
		findings = append([]Finding{{
			Kind: KindNoManageHolder,
			Detail: "no active person holding an enrolled credential carries " +
				string(iam.GrantPeopleManage) + ", so nobody can invite, " +
				"grant or revoke except through a Tier A token",
		}}, findings...)
	}
	httpjson.Write(w, http.StatusOK, map[string]any{
		"findings":                  findings,
		"position":                  position,
		"people_with_people_manage": manage,
		// SAID RATHER THAN SILENT: "no dangling binding" and "this
		// node's chart could not say" are different answers, and a
		// report that folded the second into the first would print
		// "nothing to report" during exactly the chart stall that
		// hides a residue.
		"bindings_unchecked": unchecked,
	})
}

// findingsFor is everything the report can say about one row, and whether its
// seat binding could be checked at all.
func (s *Service) findingsFor(r *http.Request, row iamdomain.PersonRow) (
	out []Finding, bindingChecked bool) {

	if row.Shredded {
		out = append(out, Finding{
			Kind: KindShredded, Person: row.ID, Login: row.Login,
			Detail: "this person was removed and their key destroyed, so " +
				"their name and address are unrecoverable everywhere",
		})
		return out, true
	}
	if row.Stage == iam.StageActive && !s.hasCredential(r, row) {
		out = append(out, Finding{
			Kind: KindNoCredential, Person: row.ID, Login: row.Login,
			Detail: "this person is active and holds no credential, so they " +
				"cannot sign in — an invitation that was never redeemed, or " +
				"an enrolment nobody completed",
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
	for _, g := range row.Grants {
		if !slices.Contains(s.ceiling, g) {
			out = append(out, Finding{
				Kind: KindClampedGrant, Person: row.ID, Login: row.Login,
				Grant: g,
				Detail: "this node's api.auth.max_grants withholds " +
					string(g) + ", so the row declares it and this node " +
					"does not honour it",
			})
		}
	}
	return out, bindingChecked
}

// hasCredential reports whether somebody can prove themselves at all.
//
// ANY LIVE METHOD COUNTS — a password, an identity provider binding, a token
// — because the question the report asks is "can this person get in", and a
// company signing in entirely through an IdP holds no passwords at all.
//
// AN UNREADABLE ANSWER IS `true`, which is the direction that does not raise
// a false alarm: reporting somebody as credential-less because a read failed
// would send an administrator to re-invite a person who is perfectly able to
// sign in.
func (s *Service) hasCredential(r *http.Request, row iamdomain.PersonRow) bool {
	held, err := s.directory.Credentials(r.Context(), row.ID)
	if err != nil {
		log.WarnContext(r.Context(), "api_iam_check_credentials_unreadable",
			"person", row.ID, "error", err)
		return true
	}
	now := s.now()
	for _, c := range held {
		if !c.Revoked(now) {
			return true
		}
	}
	return false
}
