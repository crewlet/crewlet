package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/setupapi"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/integration"
)

// consentSignIns is the engine as the setup surface's consent routes start,
// read and give up an integration's consent grant through, its refusals mapped
// onto setupapi's.
type consentSignIns struct{ e *engine.Engine }

func (c consentSignIns) StartConsent(ctx context.Context, kind integration.Kind, by iam.Actor) (setupapi.ConsentSignIn, error) {
	started, err := c.e.StartIntegrationSignIn(ctx, kind, by)
	if err != nil {
		return setupapi.ConsentSignIn{}, consentErr(err)
	}
	out := consentAnswer(started.SignIn)
	out.AuthorizeURL = started.AuthorizeURL
	return out, nil
}

func (c consentSignIns) ConsentSignIn(ctx context.Context, id uuid.UUID) (setupapi.ConsentSignIn, error) {
	s, err := c.e.IntegrationSignIn(ctx, id)
	if err != nil {
		return setupapi.ConsentSignIn{}, consentErr(err)
	}
	return consentAnswer(s), nil
}

func (c consentSignIns) SignOutConsent(ctx context.Context, kind integration.Kind, by iam.Actor) (setupapi.ConsentSignOut, error) {
	out, err := c.e.SignOutIntegration(ctx, kind, by)
	if err != nil {
		return setupapi.ConsentSignOut{}, consentErr(err)
	}
	return setupapi.ConsentSignOut{Kind: string(kind), Revocation: string(out.Revocation), Said: out.Sentence()}, nil
}

func (c consentSignIns) SignInCallback() string { return c.e.CallbackURL() }

// consentAnswer is a sign-in's record as the routes answer it: never its
// sealed half.
func consentAnswer(s coord.OAuthSignIn) setupapi.ConsentSignIn {
	return setupapi.ConsentSignIn{ID: s.ID, Kind: s.Name, Status: string(s.Status), Detail: s.Detail,
		StartedAt: s.StartedAt, ExpiresAt: s.ExpiresAt}
}

// consentErr is an engine refusal of a consent gesture as the routes answer it.
func consentErr(err error) error {
	var refused *engine.SignInRefusal
	if errors.As(err, &refused) {
		return &setupapi.ConsentRefusal{Status: refused.Status, Detail: refused.Reason + ". " + refused.Remedy}
	}
	for engineErr, routeErr := range map[error]error{
		engine.ErrNoConsentProduct:       setupapi.ErrNoConsent,
		engine.ErrNoConsentCallback:      setupapi.ErrNoConsentAddress,
		engine.ErrIntegrationNotSignedIn: setupapi.ErrConsentNotSignedIn,
		engine.ErrNoSignIn:               setupapi.ErrNoConsentSignIn,
	} {
		if errors.Is(err, engineErr) {
			return fmt.Errorf("%w: %w", routeErr, err)
		}
	}
	return err
}
