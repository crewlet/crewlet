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
var errNoPassword = errors.New("iamapi: a machine has no password to reset — " +
	"it authenticates with a token; mint it a new one instead")

// errNotResettable refuses a reset link for somebody whose stage a reset does
// not reach ([iamdomain.ResetStages]).
type errNotResettable struct{ stage iam.Stage }

func (e errNotResettable) Error() string {
	return fmt.Sprintf("iamapi: this person is %s, and a reset link would hand "+
		"back an account somebody stopped — reactivate them first", e.stage)
}

// PostPasswordReset is `POST /iam/people/{id}/password-reset`: a ONE-TIME LINK
// that sets the person a new password, shown once.
//
// # The link is a credential on the person
//
// A `reset` credential, stored as every credential is — a SHA-256 of a
// crypto/rand secret ([credential.ResetVerifier]) and an expiry
// [credential.ResetLinkLifetime] away — so it is listed among the person's
// credentials, revoked by `DELETE /iam/credentials/{id}`, and collected by the
// sweep once it is spent, revoked or aged out. Issuing one revokes the person's
// earlier outstanding link in the same record: a person holds at most one.
//
// # Shown once, like a token's value
//
// The link — `<api.external_url>/dashboard#/reset/<credential id>.<secret>` —
// is in this answer and nowhere else, and this route READS NO KEY: a replay of
// the issue would answer the first attempt's record beside a secret that
// verifies against nothing, so an issue whose outcome was unknown is issued
// again, and the link that may have landed is one nobody holds, which expires.
// Issuing again revokes it anyway.
//
// # It signs nobody in
//
// Spending it sets the password and ends every session the person held; they
// then sign in with it, where a second factor they hold still applies. See
// internal/api/authapi's reset routes.
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
		// stage a reset does not reach is refused naming it, and every
		// outstanding link the person holds is revoked by this record.
		Apply: func(p iamdomain.Person) (iamdomain.Person, error) {
			switch {
			case p.Kind != iam.KindPerson:
				return p, errNoPassword
			case !slices.Contains(iamdomain.ResetStages, p.Stage):
				return p, errNotResettable{stage: p.Stage}
			}
			held := slices.Clone(p.Credentials)
			for i, c := range held {
				if c.Method == iamdomain.MethodReset && c.RevokedAt.IsZero() {
					held[i].RevokedAt = now
				}
			}
			held = append(held, iamdomain.Credential{
				V: iamdomain.DocumentVersion, ID: id,
				Method:    iamdomain.MethodReset,
				Verifier:  credential.ResetVerifier(id, secret),
				ExpiresAt: expires,
			})
			p.Credentials = held
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
		"detail": "this link is shown once and cannot be read back; it sets " +
			"a new password once, ends every session the person holds, and " +
			"expires at the instant above",
	})
}

// resetUnknown is what an unknown issue's 503 says to do: issue again.
const resetUnknown = "no link was issued: this node cannot establish whether " +
	"the issue landed, and if it did, its link was never shown — one nobody " +
	"holds, which expires, and which issuing again revokes. Issue again; this " +
	"route reads no key, so a retry is a new link."

// resetURL is the link a person follows, built as an invitation's is
// ([Service.inviteURL]): the dashboard's reset screen, with `<id>.<secret>` in
// the fragment a browser never sends.
func (s *Service) resetURL(id, secret string) string {
	return strings.TrimRight(s.external, "/") + auth.PathDashboard +
		resetRoute + id + "." + secret
}
