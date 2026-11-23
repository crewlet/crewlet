package seataccess

import (
	"context"
	"errors"

	"github.com/crewlet/crewlet/internal/integration"
)

// IssuedTokens is the engine's short-lived tokens a session checks an
// identity with where the vendor issues them at use and nothing is sealed:
// a seat's Google growth identity's, and a product's admin sign-in (a consent
// grant, ADR-0178) that adds each seat's identity there.
type IssuedTokens interface {
	// SeatToken is handle's own token at kind with scopes, and the account it
	// is the token of (an identity's email); [ErrNotIssued] where the seat
	// holds no identity yet.
	SeatToken(ctx context.Context, kind integration.Kind, handle string, scopes []string) (token, account string,
		err error)

	// AdminToken is kind's admin sign-in token; [ErrNotIssued] where nobody
	// signed in, or the vendor refused the sign-in.
	AdminToken(ctx context.Context, kind integration.Kind) (string, error)
}

// ErrNotIssued is a token the engine holds nothing to issue.
var ErrNotIssued = errors.New("seataccess: the engine holds nothing to issue this token from yet")
