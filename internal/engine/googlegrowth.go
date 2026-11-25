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
	"github.com/crewlet/crewlet/internal/setup"
	"github.com/crewlet/crewlet/internal/words"
)

// The Google growth products sign in as two identities the core owns
// (internal/googleauth): each seat's growth identity, held by the Google
// identity pass ([googlecloud.ConvergeGrowthIdentities], run with Google
// Cloud's company pass, which every Google lane's pass follows), and the
// company's reader, which Google Cloud's pass makes. Their tokens are
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
	token, err := e.googleIssuer.Token(ctx, conn, rec.UniqueID, scopes, life)
	token.Account = rec.Email
	return token, err
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
// on in the identity project. Google Cloud's pass, which runs first, turns
// them on ([googlecloud.ConvergeGrowthProducts]); one still off is Google
// settling, or a provisioner the block has not let turn APIs on, which only
// running the block again fixes.
func missingAPIs(ctx context.Context, client *googlecloud.Client, kind integration.Kind) ([]integration.Finding, error) {
	product, ok := googleauth.ProductOf(kind)
	if !ok {
		return nil, fmt.Errorf("engine: %s registers no Google product", kind)
	}
	off, err := googlecloud.MissingAPIs(ctx, client, product)
	if err != nil {
		return nil, fmt.Errorf("engine: read the APIs on %s: %w", client.IdentityProject(), googleauth.Reject(err))
	}
	if len(off) == 0 {
		return nil, nil
	}
	may, err := googlecloud.MayEnableAPIs(ctx, client)
	if err != nil {
		return nil, fmt.Errorf("engine: read whether the provisioner may turn APIs on: %w", googleauth.Reject(err))
	}
	if may {
		return []integration.Finding{{
			Kind: integration.FindingGrantPending, Subject: "apis", Subjects: off,
			Detail: "Crewlet is turning on " + product.Name + "'s APIs in " + client.IdentityProject() + ": " +
				words.And(off),
		}}, nil
	}
	f := googlecloud.RunBlockAgain(client.IdentityProject())
	f.Subject, f.Subjects = "apis", off
	f.Detail = product.Name + " needs " + words.And(off) + " on in " + client.IdentityProject() +
		", and the Google Cloud connection's block did not let Crewlet turn APIs on"
	return []integration.Finding{f}, nil
}

// googleGrowthPass checks the company's half of one Google growth product:
// the Google identity it signs in through, and its APIs on the identity
// project, which the Cloud Shell block turns on. Then it runs the agents'
// access through the access sync, which takes no step: a seat's level and
// scope are the document alone, its identity the Google identity pass's. No
// Teardown: nothing is registered at Google.
type googleGrowthPass struct {
	engine *Engine
	kind   integration.Kind
	name   string
}

func (p *googleGrowthPass) Kind() integration.Kind { return p.kind }

// Needs is nil: the integration holds no credential.
func (*googleGrowthPass) Needs() *setup.Requirement { return nil }

// Run is the company-level pass, then the agents' own access ([Engine.withSeatAccess]).
func (p *googleGrowthPass) Run(ctx context.Context, in setup.PassInput) ([]integration.Finding, error) {
	return p.engine.withSeatAccess(ctx, p.Kind(), in, p.run)
}

func (p *googleGrowthPass) run(ctx context.Context, _ setup.PassInput) ([]integration.Finding, error) {
	company := p.engine.Company()
	if company == nil || !company.Config.Integrations.GrowthEnabled(string(p.kind)) {
		return nil, integration.ErrNotConfigured
	}
	client, err := googlecloud.ClientFor(company.Config, p.engine.resolver(), nil)
	if err != nil {
		//nolint:nilerr // Deliberate: the Google identity's absence is the person's finding.
		return []integration.Finding{{
			Kind: integration.FindingCredentialMissing, Subject: "integrations.google_cloud.provisioning",
			Detail: p.name + " signs in through the Google Cloud connection's identity: " + googleauth.Sentence(err),
			Remedy: "Connect Google Cloud first; no project needs preparing",
		}}, nil
	}
	return missingAPIs(ctx, client, p.kind)
}
