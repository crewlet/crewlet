package setupapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/integration"
)

// AN INTEGRATION'S CONSENT GRANT, for a product that refuses service accounts
// (ADR-0178): a `consent` requirement's three routes.
//
//   - `POST /setup/integrations/{kind}/consent` starts a sign-in and answers
//     the address the person's browser is sent to (201). The connect verb,
//     at its step-up, and `secrets.write` on top: the grant it makes is a
//     credential the engine seals.
//   - `GET /setup/consent-sign-ins/{id}` is the polling page's read.
//   - `DELETE /setup/integrations/{kind}/consent` gives the grant up.
//
// The vendor sends the person back to `GET /webhooks/mcp-oauth`, which
// finishes MCP servers' and integrations' sign-ins alike.

// ConsentSignIns is the engine as the consent routes start, read and give up
// an integration's consent grant through.
type ConsentSignIns interface {
	StartConsent(ctx context.Context, kind integration.Kind, by iam.Actor) (ConsentSignIn, error)
	ConsentSignIn(ctx context.Context, id uuid.UUID) (ConsentSignIn, error)
	SignOutConsent(ctx context.Context, kind integration.Kind, by iam.Actor) (ConsentSignOut, error)
	// SignInCallback is where the vendor sends the person back, the redirect
	// URI a consent product's client registers; "" with no public address.
	SignInCallback() string
}

// ConsentSignIn is a sign-in as the routes answer it: never its sealed half.
// AuthorizeURL is set on the start's answer alone.
type ConsentSignIn struct {
	ID           uuid.UUID `json:"id"`
	Kind         string    `json:"kind"`
	Status       string    `json:"status"`
	Detail       string    `json:"detail,omitempty"`
	StartedAt    time.Time `json:"started_at"`
	ExpiresAt    time.Time `json:"expires_at"`
	AuthorizeURL string    `json:"authorize_url,omitempty"`
}

// ConsentSignOut is a grant given up: what the vendor was asked
// (`revoked`, `not_offered`, `failed`, `shared`) and the sentence a person
// reads about it.
type ConsentSignOut struct {
	Kind       string `json:"kind"`
	Revocation string `json:"revocation"`
	Said       string `json:"said"`
}

// ConsentRefusal is a sign-in the engine refused, in words a person acts on.
type ConsentRefusal struct {
	Status int
	Detail string
}

func (r *ConsentRefusal) Error() string { return "setupapi: " + r.Detail }

// The refusals the consent routes map to their own answers.
var (
	// ErrNoConsent is a kind no consent product is.
	ErrNoConsent = errors.New("setupapi: this integration is not signed in to with a person's consent")
	// ErrNoConsentAddress is an engine with no public address to come back to.
	ErrNoConsentAddress = errors.New("setupapi: this engine has no public address to sign in through")
	// ErrConsentNotSignedIn is a sign-out of a grant nobody holds.
	ErrConsentNotSignedIn = errors.New("setupapi: nobody is signed in to the integration")
	// ErrNoConsentSignIn is a sign-in id no record holds, or one that lapsed.
	ErrNoConsentSignIn = errors.New("setupapi: this sign-in is not known any more; start it again")
)

// startConsent serves `POST /setup/integrations/{kind}/consent`.
func (s *Service) startConsent(w http.ResponseWriter, r *http.Request) {
	if !s.consentWired(w) || !s.mayWriteCredentials(w, r) {
		return
	}
	started, err := s.consents.StartConsent(r.Context(), integration.Kind(r.PathValue("kind")), attributionOf(r))
	if err != nil {
		refuseConsent(w, err)
		return
	}
	httpjson.Write(w, http.StatusCreated, started)
}

// consentSignIn serves `GET /setup/consent-sign-ins/{id}`.
func (s *Service) consentSignIn(w http.ResponseWriter, r *http.Request) {
	if !s.consentWired(w) {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		refuseConsent(w, ErrNoConsentSignIn)
		return
	}
	got, err := s.consents.ConsentSignIn(r.Context(), id)
	if err != nil {
		refuseConsent(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, got)
}

// signOutConsent serves `DELETE /setup/integrations/{kind}/consent`.
func (s *Service) signOutConsent(w http.ResponseWriter, r *http.Request) {
	if !s.consentWired(w) || !s.mayWriteCredentials(w, r) {
		return
	}
	out, err := s.consents.SignOutConsent(r.Context(), integration.Kind(r.PathValue("kind")), attributionOf(r))
	if err != nil {
		refuseConsent(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, out)
}

// consentWired answers 501 where this surface signs no integration in.
func (s *Service) consentWired(w http.ResponseWriter) bool {
	if s.consents != nil {
		return true
	}
	httpjson.FailWith(w, http.StatusNotImplemented, httpjson.CodeUnavailable, map[string]string{
		"detail": "this engine does not sign integrations in with a person's consent"})
	return false
}

// refuseConsent maps a consent route's failure onto its answer.
func refuseConsent(w http.ResponseWriter, err error) {
	var refused *ConsentRefusal
	switch {
	case errors.As(err, &refused):
		httpjson.FailWith(w, refused.Status, httpjson.CodeVendorRefused, map[string]string{"detail": refused.Detail})
	case errors.Is(err, ErrNoConsent):
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeUnknownKind, map[string]string{"detail": err.Error()})
	case errors.Is(err, ErrNoConsentAddress):
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodeNoExternalURL, map[string]string{"detail": err.Error()})
	case errors.Is(err, ErrConsentNotSignedIn):
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNotConnected, map[string]string{"detail": err.Error()})
	case errors.Is(err, ErrNoConsentSignIn):
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNotFound, map[string]string{"detail": err.Error()})
	default:
		log.Warn("setup_consent_failed", "error", err)
		httpjson.UnavailableWith(w, httpjson.CodeUnavailable, authz.RetryUndecidedSeconds, httpjson.Detail{
			"detail": "the sign-in could not be read or written; try again in a moment"})
	}
}
