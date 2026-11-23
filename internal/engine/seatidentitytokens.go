package engine

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/crewlet/crewlet/internal/googleauth"
	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// seatTokens is the engine's issued tokens for a seat access session's
// identity check ([seataccess.IssuedTokens]): a seat's Google growth
// identity's, minted as the passes and the tools mint it, and a product's
// admin sign-in, the product's consent grant.
type seatTokens struct {
	engine *Engine
	// hc is the transport Google is reached through; nil in production.
	hc *http.Client
}

// SeatToken is handle's growth identity's token and its email, for a Google
// growth product; ErrNotIssued where the identity pass has not made it.
func (t seatTokens) SeatToken(ctx context.Context, kind integration.Kind, handle string, scopes []string) (
	string, string, error) {
	if _, ok := googleauth.ProductOf(kind); !ok {
		return "", "", fmt.Errorf("engine: %s issues no seat token: %w", kind, seataccess.ErrNotIssued)
	}
	rec, found, err := t.engine.backends.Fleet.GoogleIdentity(ctx, handle)
	switch {
	case err != nil:
		return "", "", fmt.Errorf("engine: read %s's Google identity: %w", handle, err)
	case !found || !rec.Usable():
		return "", rec.Email, seataccess.ErrNotIssued
	}
	token, err := t.engine.GoogleGrowthToken(ctx, handle, scopes, googlegrowth.TokenLife, t.hc)
	if errors.Is(err, errGrowthIdentityNotReady) {
		return "", rec.Email, seataccess.ErrNotIssued
	}
	return token.Token, rec.Email, err
}

// AdminToken is kind's consent grant's token; ErrNotIssued where nobody
// signed in or Google refused the grant.
func (t seatTokens) AdminToken(ctx context.Context, kind integration.Kind) (string, error) {
	if _, ok := googleauth.ConsentOf(kind); !ok {
		return "", fmt.Errorf("engine: %s takes no admin sign-in: %w", kind, seataccess.ErrNotIssued)
	}
	token, err := consentGrant{engine: t.engine, kind: kind}.Token(ctx)
	if errors.Is(err, ErrIntegrationSignInNeeded) {
		return "", fmt.Errorf("%w: %w", seataccess.ErrNotIssued, err)
	}
	return token, err
}
