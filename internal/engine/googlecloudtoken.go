package engine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googlecloud"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// An agent's own Google Cloud access token, issued where the seat's values are
// read.
//
// A seat on at Google Cloud acts as its own service account, which the access
// sync makes and the ledger records by unique ID, and the only credential it
// holds is an access token the engine mints for that account by impersonating
// it as the provisioner ([googlecloud.Client.SeatToken]). Configuration names
// it as `${GOOGLE_CLOUD_ACCESS_TOKEN_<HANDLE>}`, and the engine issues it
// through the contract every credential issued at use shares (issued.go):
// [googleCloudSeatIssuer] is Google Cloud's implementation of [seatIssuer].
//
// Google Cloud's stories:
//
//   - THE AGENT'S ACCOUNT IS NOT READY YET: the ledger records none, since the
//     sync is still making it, or Google refuses the token with a 403 or a 404
//     within [googlecloud.GrantSettle] of Crewlet's grant. Nobody owes a step,
//     so the value waits, and the seat asks again on the restart schedule
//     until it clears ([errIssueSettling], [seatToken.settling]).
//   - GOOGLE REFUSED THE TOKEN after that, or the connection holds a value the
//     client refuses: a person's step, named ([googlecloud.TokenRefusal]).
//   - GOOGLE DID NOT ANSWER, or the ledger could not be read: a wait.
//   - THE SEAT IS OFF, or the integration is not connected yet: nothing issues
//     anything.

// errGoogleCloudSettling is an issue that waits on nobody: the agent's account
// not made yet, or a grant Google is still applying ([errIssueSettling]).
var errGoogleCloudSettling = fmt.Errorf("engine: Google is still making the agent's service account ready: %w",
	errIssueSettling)

// errGoogleCloudUnread is an issue that could not read which account the
// ledger records for the agent.
var errGoogleCloudUnread = errors.New("engine: the seat access ledger could not be read")

// googleCloudSeatIssuer issues each agent its own service account's token
// through the process's one [googlecloud.SeatIssuer], so an account costs one mint
// an hour however many readers ask.
type googleCloudSeatIssuer struct {
	engine *Engine
	tokens *googlecloud.SeatIssuer
	// http is the transport Google is reached through; nil in production.
	http *http.Client
}

func (googleCloudSeatIssuer) kind() integration.Kind { return integration.KindGoogleCloud }

// variable is [googleCloudIssued].
func (googleCloudSeatIssuer) variable(c *Company, handle string) string {
	return googleCloudIssued(c, handle)
}

// issuing mints seat's token for the account the ledger records for it, as
// the provisioner the company's connection reaches through env.
//
// THE ACCOUNT IS THE LEDGER'S, by unique ID, never one found by its name: a
// deleted name made again is a different principal, which Crewlet did not
// grant.
func (g googleCloudSeatIssuer) issuing(c *Company, seat *org.Role, env *config.Resolver) issueFunc {
	// AN OFF SEAT IS ISSUED NOTHING: its account is disabled, and a token
	// minted before the disable landed must not be handed on.
	handle := seat.Handle()
	if !googleCloudSeatOn(c, handle) {
		return nil
	}
	client, err := googlecloud.ClientFor(c.Config, env, g.http)
	if errors.Is(err, integration.ErrNotConfigured) {
		return nil
	}
	return func(ctx context.Context, life time.Duration) (issuedToken, error) {
		if err != nil {
			return issuedToken{}, err
		}
		rec, held, err := g.record(ctx, handle)
		switch {
		case err != nil:
			return issuedToken{}, err
		case !held:
			return issuedToken{}, errGoogleCloudSettling
		}
		token, err := g.tokens.Token(ctx, client, rec.Account.ID, life)
		switch {
		case err != nil && googlecloud.StillGranting(err) && time.Now().Before(googlecloud.GrantSettled(rec)):
			return issuedToken{}, fmt.Errorf("%w: %w", errGoogleCloudSettling, err)
		case err != nil:
			return issuedToken{}, err
		}
		return issuedToken{Token: token.Token, ExpiresAt: token.ExpiresAt,
			RenewAt: token.ExpiresAt.Add(-toolTokenLife)}, nil
	}
}

// record is the ledger's record of the seat with handle, and whether it holds
// an account to issue for: one Crewlet made and has not turned off. An on seat
// whose account Crewlet turned off is the sync's to turn back on, and one gone
// is the sync's to make again, so neither is issued meanwhile.
func (g googleCloudSeatIssuer) record(ctx context.Context, handle string) (seataccess.Record, bool, error) {
	ledger, err := g.engine.SeatAccessLedger()
	if err != nil {
		return seataccess.Record{}, false, fmt.Errorf("%w: %w", errGoogleCloudUnread, err)
	}
	records, err := ledger.Records(ctx, integration.KindGoogleCloud)
	if err != nil {
		return seataccess.Record{}, false, fmt.Errorf("%w: %w", errGoogleCloudUnread, err)
	}
	rec, ok := records[handle]
	return rec, ok && rec.Held() && rec.Account.State != seataccess.AccountDisabled && rec.Account.ID != "", nil
}

// refusal is "" for Google still making the account ready, a wait that clears
// it; the ledger not answering, a wait; and otherwise Google's answer, which
// [googlecloud.TokenRefusal] tells apart.
func (googleCloudSeatIssuer) refusal(err error) (string, bool) {
	switch {
	case errors.Is(err, errIssueSettling):
		return "", true
	case errors.Is(err, errGoogleCloudUnread):
		return err.Error(), true
	}
	return googlecloud.TokenRefusal(err)
}

func (googleCloudSeatIssuer) words() issuedWords {
	return issuedWords{
		source:  "the Google Cloud access token the engine issues for this seat",
		verb:    "issue",
		arrival: "the agent is on at Google Cloud and Google has applied Crewlet's grant on its service account",
		refused: "it is not issued until somebody fixes what Google names, on the Google Cloud connection " +
			"or the agent's row of its Agents tab",
		// NOT "at the next attempt": the issuer holds a refused mint for the
		// restart schedule's wait ([googlecloud.SeatIssuer.RetryWait]).
		retried: "Google is asked for it again on the restart schedule",
	}
}

// googleCloudIssued is the variable the Google Cloud integration issues for
// handle's seat, or "" where it issues none: the company does not run Google
// Cloud, or the handle is not an agent's. Declared for EVERY agent seat while
// it runs, on or not, since a seat naming its token early waits on its account.
func googleCloudIssued(c *Company, handle string) string {
	if c == nil {
		return ""
	}
	if cfg := c.Config.Integrations.GoogleCloud; cfg == nil || !cfg.Enabled {
		return ""
	}
	if c.Org == nil || c.Org.AgentSeatByHandle(handle) == nil {
		return ""
	}
	return seataccess.SinkName(integration.KindGoogleCloud, googlecloud.AccessTokenField, handle)
}

// googleCloudSeatOn reports whether the seat with handle is on at Google Cloud.
func googleCloudSeatOn(c *Company, handle string) bool {
	for _, seat := range seataccess.SeatsOf(c.Config) {
		if seat.Handle == handle {
			return seat.OnAt(integration.KindGoogleCloud)
		}
	}
	return false
}
