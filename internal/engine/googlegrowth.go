package engine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/googleauth"
	"github.com/crewlet/crewlet/internal/googlecloud"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/words"
)

// The Google growth products sign in as two identities the core owns
// (internal/googleauth): each seat's growth identity, held by the Google
// identity pass ([googlecloud.ConvergeGrowthIdentities], run with Google
// Cloud's company pass, which every Google lane's pass follows), and the
// company's reader, which the Cloud Shell block makes. Their tokens are
// minted here, inside the engine, with the product's own scopes, and never
// leave it: no growth token is ever a `${VAR}` or reaches a box.

// errGrowthIdentityNotReady is a seat whose growth identity is not made, on,
// or delegated yet: the pass's work, which a tool call waits on.
var errGrowthIdentityNotReady = errors.New("engine: the agent's Google identity is not ready yet; " +
	"Crewlet makes it on the Google Cloud connection's next pass")

// growthSeats is every agent seat of c, on where any Google growth lane of
// it is: the OR the seat's growth identity follows. Its Google Cloud lane
// counts for nothing, so turning Google Cloud off for a seat leaves its growth
// identity working.
func growthSeats(c *config.Company) []googlecloud.GrowthSeat {
	kinds := googleauth.GrowthKinds()
	var out []googlecloud.GrowthSeat
	for _, seat := range seataccess.SeatsOf(c) {
		out = append(out, googlecloud.GrowthSeat{Handle: seat.Handle, On: slices.ContainsFunc(kinds, seat.OnAt)})
	}
	return out
}

// convergeGrowthIdentities is the Google identity pass, on client, over the
// identities the pass read as held.
func (e *Engine) convergeGrowthIdentities(
	ctx context.Context, c *config.Company, client *googlecloud.Client, held []coord.GoogleIdentity,
) ([]integration.Finding, error) {
	return googlecloud.ConvergeGrowthIdentities(ctx, client, c.Name, growthSeats(c), held, e.backends.Fleet)
}

// GoogleGrowthToken is a token of handle's growth identity carrying scopes,
// good for at least life, through the company's Google identity as env
// resolves it. hc is nil in production. It answers
// [errGrowthIdentityNotReady] for a seat whose identity the pass has not made,
// turned on or delegated yet.
func (e *Engine) GoogleGrowthToken(
	ctx context.Context, handle string, scopes []string, life time.Duration, hc *http.Client,
) (googleauth.SeatToken, error) {
	company := e.Company()
	if company == nil {
		return googleauth.SeatToken{}, integration.ErrNotConfigured
	}
	conn, err := googleauth.For(company.Config, e.resolver(), hc)
	if err != nil {
		return googleauth.SeatToken{}, err
	}
	rec, found, err := e.backends.Fleet.GoogleIdentity(ctx, handle)
	switch {
	case err != nil:
		return googleauth.SeatToken{}, fmt.Errorf("engine: read %s's Google identity: %w", handle, err)
	case !found || !rec.Usable():
		return googleauth.SeatToken{}, errGrowthIdentityNotReady
	}
	return e.googleIssuer.Token(ctx, conn, rec.UniqueID, scopes, life)
}

// GoogleReaderToken is a token of the company's reader carrying scopes: the
// identity that reads growth figures for the whole company, which only this
// company's federated subject may act as.
func (e *Engine) GoogleReaderToken(ctx context.Context, scopes []string, hc *http.Client) (string, error) {
	company := e.Company()
	if company == nil {
		return "", integration.ErrNotConfigured
	}
	conn, err := googleauth.For(company.Config, e.resolver(), hc)
	if err != nil {
		return "", err
	}
	reader := googleauth.AccountEmail(googleauth.ReaderAccountID(company.Config.Name), conn.IdentityProject())
	return conn.FederatedToken(ctx, reader, scopes...)
}

// missingAPIs is the finding a Google growth product reports for its APIs not
// on in the identity project: a person runs the Cloud Shell block again,
// which turns them on; the pass enables nothing itself.
func missingAPIs(ctx context.Context, client *googlecloud.Client, kind integration.Kind) ([]integration.Finding, error) {
	product, ok := googleauth.ProductOf(kind)
	if !ok {
		return nil, fmt.Errorf("engine: %s registers no Google product", kind)
	}
	enabled, err := client.EnabledServices(ctx, client.IdentityProject())
	if err != nil {
		return nil, fmt.Errorf("engine: read the APIs on %s: %w", client.IdentityProject(), googleauth.Reject(err))
	}
	var off []string
	for _, api := range product.APIs {
		if !enabled[api] {
			off = append(off, api)
		}
	}
	if len(off) == 0 {
		return nil, nil
	}
	slices.Sort(off)
	return []integration.Finding{{
		Kind: integration.FindingApprovalRequired, Subject: "apis",
		Title:     "Turn on " + product.Name + "'s APIs",
		Detail:    "The identity project " + client.IdentityProject() + " does not have these on: " + words.And(off),
		Remedy:    "Run the Google Cloud connection's Cloud Shell block again; it turns them on",
		ActionURL: googlecloud.CloudShellURL, Subjects: off,
	}}, nil
}
