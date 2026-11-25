package seataccess

import (
	"context"
	"errors"
	"time"

	"github.com/crewlet/crewlet/internal/integration"
)

// IssuedTokens is the engine's short-lived tokens a session checks an
// identity with where the vendor issues them at use and nothing is sealed:
// a seat's Google growth identity's, the company's reader, and a product's
// admin sign-in (a consent grant, ADR-0178) that adds each seat's identity there.
type IssuedTokens interface {
	// SeatToken is handle's own token at kind with scopes, the account it is
	// the token of (an identity's email) and when that was made; [ErrNotIssued]
	// with whatever is known of the identity where it holds none usable yet.
	SeatToken(ctx context.Context, kind integration.Kind, handle string, scopes []string) (IssuedSeat, error)

	// ReaderToken is the company's own reader's token at kind, which lists
	// what a seat may be given; [ErrNotIssued] where the company has no reader.
	ReaderToken(ctx context.Context, kind integration.Kind) (string, error)

	// AdminToken is kind's admin sign-in token; [ErrNotIssued] where nobody
	// signed in, or the vendor refused the sign-in.
	AdminToken(ctx context.Context, kind integration.Kind) (string, error)
}

// IssuedSeat is a seat's own identity's token, and the identity it is of.
type IssuedSeat struct {
	// Token is "" where none was issued.
	Token string
	// Account is the identity's name at the vendor (an email), and Made when
	// the engine made it; "" and zero where it is not made yet.
	Account string
	Made    time.Time
}

// ErrNotIssued is a token the engine holds nothing to issue.
var ErrNotIssued = errors.New("seataccess: the engine holds nothing to issue this token from yet")
