package iamapi

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// resetRoute is the dashboard's reset screen, as a fragment route of its shell.
const resetRoute = "#/reset/"

// errNoPassword refuses a reset link for a machine, which signs in with no
// password at all.
var errNoPassword = errors.New("service accounts don't have passwords. " +
	"Create a new access token instead")

// errNotResettable refuses a reset link for somebody whose stage a reset does
// not reach ([iamdomain.ResetStages]).
type errNotResettable struct{ stage iam.Stage }

func (e errNotResettable) Error() string {
	return fmt.Sprintf("this person is %s. Reactivate them before sending a "+
		"reset link", e.stage)
}

// PostPasswordReset is `POST /iam/people/{id}/password-reset`: a ONE-TIME LINK,
// a `reset` credential ([credential.ResetVerifier], [credential.ResetLinkLifetime]).
// Shown once, in this answer only. Issuing revokes the earlier one, so it also
// remedies a lapsed FIRST link. The route READS NO KEY: a replay would pair the first record with a secret that
// verifies against nothing, so an unknown outcome is issued again. The issuer
// could spend it to become the person, so it is held to
// [iamdomain.Writer.MayConfer] as if their grants were conferred from nothing
// (`403 unauthorized`; today only an unknown name on the row reaches it, `403
// forbidden`; ADR-0033). A grant the person GAINS meanwhile revokes it
// ([iamdomain.Writer.UpdatePerson]); a demoted ISSUER's link still opens until
// revoked. Spending it ends every session and signs nobody in.
func (s *Service) PostPasswordReset(w http.ResponseWriter, r *http.Request) {
	if !s.linksPoint(w, "a reset") {
		return
	}
	writer, ok := s.writerFor(r.Context())
	if !ok {
		httpjson.Fail(w, http.StatusUnauthorized, httpjson.CodeInvalidToken)
		return
	}
	person := r.PathValue("id")
	// NOBODY BY THAT ID is a 404, as an edit's is — the one question this
	// surface answers before the record does.
	if _, err := s.directory.Person(r.Context(), person); errors.Is(err,
		iamdomain.ErrNotFound) {
		httpjson.Fail(w, http.StatusNotFound, httpjson.CodeNotFound)
		return
	} else if err != nil {
		s.unavailable(w, r, "read a person", err)
		return
	}
	id := uuid.Must(uuid.NewV7()).String()
	secret, err := credential.NewResetSecret()
	if err != nil {
		log.ErrorContext(r.Context(), "api_iam_reset_mint_failed", "error", err)
		httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeInternalError)
		return
	}
	now := s.now()
	expires := now.Add(credential.ResetLinkLifetime)
	// A STEP OF THE CREDENTIAL IT ISSUES, as a token's mint is: fresh every
	// time, carrying the instant the credential's uuid7 was minted at.
	opID := statelog.StepOpID(id, "password-reset")
	const reason = "a password reset link was issued"
	issued, err := writer.UpdatePerson(r.Context(), iamdomain.PersonUpdate{
		PersonID: person,
		// IN THE SNAPSHOT THE LINK LANDS ON: a machine has no password, a
		// stage a reset does not reach is refused naming it, somebody
		// holding a grant the issuer does not is refused, and every
		// outstanding link the person holds is revoked by this record.
		Apply: func(p iamdomain.Person) (iamdomain.Person, error) {
			switch {
			case p.Kind != iam.KindPerson:
				return p, errNoPassword
			case !slices.Contains(iamdomain.ResetStages, p.Stage):
				return p, errNotResettable{stage: p.Stage}
			}
			// THE ISSUER SEES THE LINK, so issuing it is handing them
			// the person's grants: the link is the conferral rule's
			// to judge, as if every grant the person holds were being
			// given to them from nothing.
			if refused := writer.MayConfer(nil, p.Grants); refused != nil {
				return p, refused
			}
			p.Credentials = append(iamdomain.RevokeResetLinks(p.Credentials, now),
				iamdomain.Credential{
					V: iamdomain.DocumentVersion, ID: id,
					Method:    iamdomain.MethodReset,
					Verifier:  credential.ResetVerifier(id, secret),
					ExpiresAt: expires,
				})
			return p, nil
		},
		OpID: opID, Reason: reason,
	})
	var notResettable errNotResettable
	switch {
	case errors.Is(err, errNoPassword):
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodeBadParams,
			map[string]string{"detail": errNoPassword.Error()})
		return
	case errors.As(err, &notResettable):
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodeBadParams,
			map[string]string{"detail": notResettable.Error(),
				"stage": string(notResettable.stage)})
		return
	case err == nil && issued.Outcome == statelog.OutcomeUnknown:
		// NO LINK FOR AN ISSUE NOBODY CAN CONFIRM, and its retry is a new
		// issue — this route reads no key, for a token mint's reason.
		log.WarnContext(r.Context(), "api_iam_write_unresolved", "op_id", opID,
			"unvouched", issued.Unvouched)
		httpjson.UnknownOutcome(w, auth.RetryIdentity(nil), opID,
			issued.Unvouched, httpjson.Detail{"detail": resetUnknown})
		return
	case err != nil || !ownLanding(issued):
		s.answerWrite(w, r, opID, issued, err, map[string]any{"id": person})
		return
	}
	log.InfoContext(r.Context(), "iam_password_reset_issued",
		"person", person, "credential", id, "expires_at", expires)
	s.audit.Emit(r.Context(), types.IAMPasswordResetIssued{
		Person: person, Credential: id, ExpiresAt: expires,
		By: callerName(r.Context()), OperatorID: callerOperator(r.Context()),
		Reason: reason,
	})
	s.answer(w, r, opID, issued, nil, http.StatusCreated, map[string]any{
		"id": person, "credential": id, "url": s.resetURL(id, secret),
		"expires_at": expires,
		"detail": "This link is shown only once. It sets a new password, " +
			"signs them out everywhere, and expires at the time above.",
	})
}

// resetUnknown is what an unknown issue's 503 says to do: issue again.
const resetUnknown = "Couldn't confirm the link was created. Issue a new one."

// resetURL is the link a person follows, built as an invitation's is
// ([Service.inviteURL]): the dashboard's reset screen, with `<id>.<secret>` in
// the fragment a browser never sends. A reset link and a created person's first
// password link are one kind of credential spent on one screen, so both are
// built here.
func (s *Service) resetURL(id, secret string) string {
	return strings.TrimRight(s.external, "/") + auth.PathDashboard +
		resetRoute + id + "." + secret
}
