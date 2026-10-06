package iamapi

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
)

// THE INVITATIONS THIS ESTATE HOLDS: listed, and withdrawn.
//
// # A listing that never carries a link
//
// What an administrator needs of an outstanding invitation is who it was sent
// to, what it offers, who sent it and until when — and the one thing they may
// not have is the link, which the issue showed once and the estate keeps only
// a verifier of. So a row here is those facts and its state, and the link is
// re-issued rather than read back: cancel the invitation and invite again.

// invitationState is where one invitation stands, which a screen sorts and
// labels by.
type invitationState string

const (
	// invitationOpen is redeemable now: not redeemed, deadline ahead.
	invitationOpen invitationState = "open"

	// invitationExpired aged out unredeemed; the sweep collects it.
	invitationExpired invitationState = "expired"

	// invitationRedeemed created somebody — [invitationView.Person].
	invitationRedeemed invitationState = "redeemed"
)

// stateOf is one invitation's state at now — the same two predicates
// [iamdomain.InvitationRow.Spent] folds into one, told apart here for an
// administrator, who is entitled to know which.
func stateOf(row iamdomain.InvitationRow, now time.Time) invitationState {
	switch {
	case !row.RedeemedAt.IsZero():
		return invitationRedeemed
	case row.Spent(now):
		return invitationExpired
	}
	return invitationOpen
}

// invitationView is one invitation as this surface renders it.
//
// NO LINK, NO SECRET, NO VERIFIER — the view type is what makes their absence
// structural, as [credentialView]'s is for a credential's.
type invitationView struct {
	ID string `json:"id"`

	// Email is the address, opened with this node's keyring for this one
	// answer; Sealed reports one this keyring cannot open, for
	// [personView.Sealed]'s reason.
	Email  string `json:"email,omitempty"`
	Sealed bool   `json:"sealed,omitempty"`

	// Seat is the handle of the seat redeeming it binds, or empty.
	Seat string `json:"seat,omitempty"`

	Grants    []iam.Grant `json:"grants"`
	InvitedBy string      `json:"invited_by,omitempty"`

	CreatedAt  time.Time `json:"created_at,omitzero"`
	ExpiresAt  time.Time `json:"expires_at,omitzero"`
	RedeemedAt time.Time `json:"redeemed_at,omitzero"`

	// Person is who a redemption created, and empty until one did.
	Person string `json:"person,omitempty"`

	State invitationState `json:"state"`
}

// GetInvitations is `GET /iam/invitations`: the OPEN invitations, paged as
// `GET /iam/people` pages, and with `?all=true` the expired and redeemed ones
// this estate still holds as well.
func (s *Service) GetInvitations(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	now := s.now()
	q := iamdomain.InvitationsQuery{
		After: query.Get("after"), All: query.Get("all") == "true", Now: now,
	}
	if raw := query.Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil {
			httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeBadParams,
				map[string]string{"detail": "limit is not a number"})
			return
		}
		q.Limit = limit
	}
	page, err := s.directory.Invitations(r.Context(), q)
	if err != nil {
		s.unavailable(w, r, "read the invitations", err)
		return
	}
	out := make([]invitationView, 0, len(page.Invitations))
	for _, row := range page.Invitations {
		view := invitationView{
			ID: row.ID, Seat: row.Seat, Grants: row.Grants,
			InvitedBy: row.InvitedBy, CreatedAt: row.CreatedAt,
			ExpiresAt: row.ExpiresAt, RedeemedAt: row.RedeemedAt,
			Person: row.Person, State: stateOf(row, now),
		}
		if view.Grants == nil {
			view.Grants = []iam.Grant{}
		}
		view.Email, view.Sealed = s.openInvitation(r, row)
		out = append(out, view)
	}
	httpjson.Write(w, http.StatusOK, map[string]any{
		"invitations": out,
		"next":        page.Next,
		"position":    page.At.String(),
	})
}

// openInvitation opens an invitation's address for this one answer, and
// reports one this node's keyring cannot open as sealed — for [Service.open]'s
// reason, as the invitation's own, since there is no person to bind it to.
func (s *Service) openInvitation(r *http.Request, row iamdomain.InvitationRow) (
	string, bool) {

	if row.Sealed == "" {
		return "", false
	}
	email, err := s.opener.OpenInvitation(row.ID, row.Sealed)
	if err != nil {
		log.WarnContext(r.Context(), "api_iam_unseal_failed",
			"invitation", row.ID, "error", err)
		return "", true
	}
	return email, false
}

// DeleteInvitation is `DELETE /iam/invitations/{id}`: an invitation nobody has
// redeemed, withdrawn — its link opens nothing from now on, exactly as an id
// nobody issued, and the address it held is free for a new one.
//
// A REDEEMED ONE IS 409 `stale`, NAMING WHOM IT CREATED: the link is spent,
// and what undoes it is removing that person. `stale` because the caller is
// acting on a reading the redemption has overtaken — the remedy is to read the
// directory again, where the person now is — and in a sentence of the
// surface's own, because the domain's error is written for a log, package
// prefix and ids included, and the dashboard shows a detail verbatim. It was
// `bad_params`, whose sentence is about a query parameter, beside that error.
// An id the estate does not hold is 404.
func (s *Service) DeleteInvitation(w http.ResponseWriter, r *http.Request) {
	writer, ok := s.writerFor(r.Context())
	if !ok {
		httpjson.Fail(w, http.StatusUnauthorized, httpjson.CodeInvalidToken)
		return
	}
	id := r.PathValue("id")
	op, ok := s.opIDFor(w, r, "invitations-cancel", nil)
	if !ok {
		return
	}
	reason := reasonOr(r.URL.Query().Get("reason"),
		byCaller(r.Context(), "cancelled"))
	cancelled, err := writer.CancelInvitation(r.Context(), id, op.id, reason)
	var redeemed *iamdomain.InvitationRedeemed
	switch {
	case errors.Is(err, iamdomain.ErrNoInvitation):
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNotFound,
			map[string]string{"detail": "this estate holds no invitation " +
				"with that id: never issued, cancelled already, or collected " +
				"once it was redeemed or aged out"})
		return
	case errors.As(err, &redeemed):
		httpjson.FailWithFields(w, http.StatusConflict, httpjson.CodeStale,
			httpjson.Detail{"detail": "this invitation has already been " +
				"redeemed: the person it invited has joined, so there is no " +
				"link left to cancel — remove them instead if they should " +
				"not have access", "person": redeemed.Person, "id": id})
		return
	}
	if err == nil && ownLanding(cancelled) {
		s.audit.Emit(r.Context(), types.IAMInvitationCancelled{
			Invitation: id, By: callerName(r.Context()),
			OperatorID: callerOperator(r.Context()), Reason: reason,
		})
	}
	s.answerWrite(w, r, op.key, cancelled, err, map[string]any{"id": id})
}
