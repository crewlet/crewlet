package googlegrowth_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// grant is a consent product's sign-in: signed in, or not.
type grant struct{ signedIn bool }

func (grant) SeatToken(context.Context, integration.Kind, string, []string) (seataccess.IssuedSeat, error) {
	return seataccess.IssuedSeat{}, seataccess.ErrNotIssued
}

func (grant) ReaderToken(context.Context, integration.Kind) (string, error) {
	return "", seataccess.ErrNotIssued
}

func (g grant) AdminToken(context.Context, integration.Kind) (string, error) {
	if !g.signedIn {
		return "", seataccess.ErrNotIssued
	}
	return "ya29.grant", nil
}

// A CONSENT PRODUCT'S PICKER LISTS WHAT THE COMPANY'S SIGN-IN REACHES, by
// name, and says why it lists nothing where nobody signed in or Google
// refused the listing, rather than an empty list that looks like none.
//
// Mutation: list with no token, and AdMob's picker offers nothing while the
// sign-in reaches two accounts.
func TestAConsentPickerListsWhatTheSignInReaches(t *testing.T) {
	t.Parallel()
	accounts := []googlegrowth.Listed{{ID: "pub-0000000000000001", Name: "Acme"}, {ID: "pub-0000000000000002"}}
	g := googlegrowth.Granted{Product: integration.KindAdMob, Lane: seataccess.LaneAdMob, Name: "AdMob",
		Entries: "publisher accounts", Typed: `^pub-[0-9]{16}$`,
		List: func(_ context.Context, in googlegrowth.Lister) ([]googlegrowth.Listed, error) {
			if in.Token != "ya29.grant" {
				return nil, errors.New("no token")
			}
			return accounts, nil
		}}
	opts := googlegrowth.GrantOptions(t.Context(), g, grant{signedIn: true})
	if !slices.Equal(opts.Scope, []string{"pub-0000000000000001", "pub-0000000000000002"}) ||
		opts.ScopeNames["pub-0000000000000001"] != "Acme" || opts.ScopeUnlisted != "" || !opts.ScopeLive ||
		opts.ScopeTyped == "" {
		t.Errorf("signed in: %+v", opts)
	}
	opts = googlegrowth.GrantOptions(t.Context(), g, grant{})
	if len(opts.Scope) != 0 || !strings.Contains(opts.ScopeUnlisted, "Sign in with Google on AdMob") || opts.ScopeLive {
		t.Errorf("nobody signed in: %+v", opts)
	}
}
