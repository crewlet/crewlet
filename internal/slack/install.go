package slack

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/provision"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// A seat's install link, and the landing that finishes it.
//
// # Why the state carries proof
//
// The landing Slack returns a person's browser to is unauthenticated: a
// redirect carries no engine credential. It exchanges the code and seals a bot
// token into a seat, so the only thing between it and anybody who can reach
// the engine is the state. It is therefore an HMAC over the seat's handle, its
// app, a random nonce and an expiry, signed under the fleet keyring through
// [runtoken] (the signer every per-run token uses, with its own domain, its
// key tags and its constant-time compare), so the node Slack returns the
// browser to verifies what another node minted, and a keyring rotation leaves
// a link minted under the previous key valid until it lapses.
//
// # Single use
//
// The nonce is recorded when the link is minted and spent when the install
// completes, both on the fleet's claim registry, so a link that reached a log
// or a browser history cannot be presented twice, on this node or any other,
// even inside its fifteen minutes. A registry that cannot answer refuses: this
// is an authorization check, and refusing costs a person one more click.
//
// # One completion
//
// [CompleteSeatInstall] is the only code that exchanges a code and seals a bot
// token. The landing reaches it through [Installs.Complete], and the
// provisioning command's paste reaches it directly, so there is one place that
// checks the app and revokes a token it could not seal.

// InstallTTL is how long an install link stays good: a browser round trip to
// Slack and one click, with room for a person to read the approval page.
const InstallTTL = 15 * time.Minute

// installDomain separates install states from every other token the keyring
// signs, so a per-run token never opens as an install state.
const installDomain = "slack-install"

// stateSep joins the state's subject fields. No handle, app id or base64url
// nonce holds it, and it is not a separator the signer's grammar reserves.
const stateSep = "~"

// Claims records nonces, first claim wins. The fleet's coord.Claims
// satisfies it.
type Claims interface {
	Claim(ctx context.Context, key string, ttl time.Duration, now time.Time) (bool, error)
	Release(ctx context.Context, key string) error
}

// InstallOptions build an [Installs].
type InstallOptions struct {
	// Material is the fleet keyring the state is signed under.
	Material runtoken.Material

	// Claims records each link's nonce when it is minted and spends it
	// when the install completes.
	Claims Claims

	// Base is the deployment's public webhook base, read at use: every
	// app's redirect is built from it, and the exchange must name the
	// same redirect the app registered.
	Base func() string

	// Resolve reads a sealed value by reference (`${NAME}`), for the
	// seat's OAuth client.
	Resolve func(ref string) string

	// Sink builds the recorder the bot token is sealed through, for one
	// completion.
	Sink func(ctx context.Context) (provision.TokenSink, error)

	// Admin and HTTP reach Slack. Nil takes the defaults.
	Admin *Admin
	HTTP  *http.Client

	// Now is the clock. Nil takes wall-clock time.
	Now func() time.Time
}

// Installs mints install links and completes the installs they return.
type Installs struct {
	opts   InstallOptions
	signer *runtoken.Signer
}

// NewInstalls builds the minter and completer, refusing one that cannot sign
// for the fleet or record a nonce.
func NewInstalls(opts InstallOptions) (*Installs, error) {
	switch {
	case opts.Claims == nil:
		return nil, errors.New("slack: no claim registry to record install links in")
	case opts.Base == nil || opts.Resolve == nil || opts.Sink == nil:
		return nil, errors.New("slack: an install needs the public base, the sealed store and a sink")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Admin == nil {
		opts.Admin = NewAdmin(nil)
	}
	signer, err := runtoken.New(runtoken.Options{
		Domain: installDomain, Material: opts.Material, Now: opts.Now,
	})
	if err != nil {
		return nil, fmt.Errorf("slack: %w", err)
	}
	return &Installs{opts: opts, signer: signer}, nil
}

// InstallRefusal is why an install link could not be minted or completed, in
// words a person acts on. It never carries the code or a token.
type InstallRefusal struct {
	// Status is the HTTP status the landing answers with.
	Status int

	// Reason is what went wrong, and Remedy what to do about it.
	Reason string
	Remedy string

	// RetryAfter is how long to wait before trying again, for a refusal
	// that clears on its own; zero for one a person has to act on.
	RetryAfter time.Duration
}

// registryRetry is how long a person waits before retrying an install the
// claim registry could not vouch for: long enough for a store blink to pass.
const registryRetry = 30 * time.Second

func (r *InstallRefusal) Error() string { return "slack: " + r.Reason + ". " + r.Remedy }

// ErrNoApp refuses a link for a seat whose app is not created yet, or whose
// OAuth client is not sealed: there is nothing to install.
var ErrNoApp = &InstallRefusal{
	Status: http.StatusConflict,
	Reason: "this agent has no Slack app to install yet",
	Remedy: "Turn the agent on in Slack's Agents tab and let the next sync create its app",
}

// refusedLink is the one answer for a state that is forged, edited, lapsed,
// signed under a key this fleet no longer holds, never minted, or already
// spent: the only move for any of them is a new link, and telling a caller
// which tells an attacker the same.
var refusedLink = &InstallRefusal{
	Status: http.StatusBadRequest,
	Reason: "this install link is not one this engine issued, has expired, or was already used",
	Remedy: "Start the install again with Install on the agent's row in Slack's Agents tab",
}

// Link mints a single-use install link for one seat's app.
//
// appID is the app the ledger records for the seat ([seataccess.Account.ID]);
// the OAuth client is read from the seat's sealed values. The link is minted
// on request and never stored: it is a bearer credential for its fifteen
// minutes.
func (i *Installs) Link(ctx context.Context, handle, appID string) (string, error) {
	handle, appID = strings.TrimSpace(handle), strings.TrimSpace(appID)
	clientID := i.sealed(handle, FieldClientID)
	if handle == "" || appID == "" || clientID == "" || i.sealed(handle, FieldClientSecret) == "" {
		return "", ErrNoApp
	}
	base := strings.TrimSpace(i.opts.Base())
	if base == "" {
		return "", ErrNoBaseURL
	}
	nonce, err := newNonce()
	if err != nil {
		return "", err
	}
	// RECORDED WHEN MINTED, so a state the signer would accept but this
	// fleet never handed out is still refused at the landing.
	first, err := i.opts.Claims.Claim(ctx, issuedKey(nonce), InstallTTL, i.opts.Now())
	if err != nil {
		return "", registryDown("this engine could not record the install link", err)
	}
	if !first {
		return "", errors.New("slack: a fresh install nonce was already recorded")
	}
	state := i.signer.Mint(strings.Join([]string{handle, appID, nonce}, stateSep), InstallTTL)
	return InstallLink(SeatApp{Handle: handle, ClientID: clientID}, base, state), nil
}

// Complete finishes the install a link began: it verifies and spends the
// state, exchanges the code and seals the bot token under the seat's own
// name. It answers the seat's handle.
//
// Every refusal is an [*InstallRefusal] naming the reason and the remedy;
// none quotes the code or a token.
func (i *Installs) Complete(ctx context.Context, state, code string) (string, error) {
	handle, appID, nonce, ok := i.open(state)
	if !ok {
		return "", refusedLink
	}
	if err := i.spend(ctx, nonce); err != nil {
		return "", err
	}
	if strings.TrimSpace(code) == "" {
		return handle, &InstallRefusal{
			Status: http.StatusBadRequest,
			Reason: "Slack returned no approval code",
			Remedy: "Start the install again with Install on the agent's row in Slack's Agents tab",
		}
	}
	sink, err := i.opts.Sink(ctx)
	if err != nil {
		return handle, err
	}
	seat := SeatApp{
		Handle: handle, AppID: appID,
		ClientID: i.sealed(handle, FieldClientID), ClientSecret: i.sealed(handle, FieldClientSecret),
	}
	installed, err := CompleteSeatInstall(ctx, i.opts.Admin, sink, i.opts.HTTP, seat, code,
		i.opts.Base())
	if err != nil {
		return handle, installRefusal(handle, err)
	}
	if err := sink.Flush(ctx); err != nil {
		// NOT SEALED UNTIL FLUSHED, so the token is revoked as an unsealed
		// one is, under a context the request's end cannot cancel.
		return handle, errors.Join(
			fmt.Errorf("slack: finish sealing %s's bot token: %w", handle, err),
			revokeToken(context.WithoutCancel(ctx), i.opts.HTTP, installed.BotToken))
	}
	return handle, nil
}

// open verifies a state's signature and expiry and reads its fields.
func (i *Installs) open(state string) (handle, appID, nonce string, ok bool) {
	subject := i.signer.Validate(strings.TrimSpace(state))
	parts := strings.Split(subject, stateSep)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

// spend consumes a nonce: it must have been recorded when its link was
// minted, and must not have been spent. FAILS CLOSED on a registry that
// cannot answer.
func (i *Installs) spend(ctx context.Context, nonce string) error {
	now := i.opts.Now()
	unissued, err := i.opts.Claims.Claim(ctx, issuedKey(nonce), InstallTTL, now)
	if err != nil {
		return registryDown(unusedUnconfirmed, err)
	}
	if unissued {
		// THIS CLAIM JUST MADE THE RECORD, so the fleet never minted
		// the link. It is released again so it stands for nothing.
		if releaseErr := i.opts.Claims.Release(ctx, issuedKey(nonce)); releaseErr != nil {
			return registryDown(unusedUnconfirmed, releaseErr)
		}
		return refusedLink
	}
	first, err := i.opts.Claims.Claim(ctx, spentKey(nonce), InstallTTL, now)
	if err != nil {
		return registryDown(unusedUnconfirmed, err)
	}
	if !first {
		return refusedLink
	}
	return nil
}

// unusedUnconfirmed is the reason a landing gives when the claim registry
// could not say whether its link was spent.
const unusedUnconfirmed = "this engine could not confirm the install link was unused"

// registryDown is a refusal for a claim registry that could not answer: a 503
// with the time to retry after, at the landing and at the mint alike.
func registryDown(reason string, err error) error {
	return errors.Join(&InstallRefusal{
		Status:     http.StatusServiceUnavailable,
		Reason:     reason,
		Remedy:     "Try the install again in a moment; if it keeps failing, check the engine's coordination store",
		RetryAfter: registryRetry,
	}, err)
}

// installRefusal says why a completion failed, without quoting anything Slack
// or the person sent.
func installRefusal(handle string, err error) error {
	var api *APIError
	switch {
	case errors.Is(err, errOtherApp):
		return &InstallRefusal{
			Status: http.StatusConflict,
			Reason: "that approval belongs to another agent's Slack app, so nothing was recorded for " + handle,
			Remedy: "Use Install on " + handle + "'s own row in Slack's Agents tab",
		}
	case errors.As(err, &api):
		return &InstallRefusal{
			Status: http.StatusBadGateway,
			Reason: "Slack refused the install (" + api.Code + ")",
			Remedy: "Start the install again with Install on " + handle + "'s row in Slack's Agents tab",
		}
	default:
		return errors.Join(&InstallRefusal{
			Status: http.StatusInternalServerError,
			Reason: "the install could not be finished for " + handle,
			Remedy: "Start the install again with Install on " + handle + "'s row; nothing was kept from this one",
		}, err)
	}
}

// sealed reads one of the seat's sealed values.
func (i *Installs) sealed(handle, field string) string {
	return strings.TrimSpace(i.opts.Resolve("${" + SeatVar(handle, field) + "}"))
}

// AwaitsInstall reports a seat whose Slack lane is waiting for a person's
// install, which is the only time an install link is minted: its last sync
// stopped at the approve step.
func AwaitsInstall(rec seataccess.Record) bool {
	if !rec.Held() {
		return false
	}
	lane, ok := rec.Lanes[seataccess.LaneSlack]
	return ok && lane.Sync != nil && lane.Sync.Outcome == seataccess.OutcomeWaiting &&
		lane.Sync.Error != nil && lane.Sync.Error.Step == seataccess.VerbApprove
}

func issuedKey(nonce string) string { return "slack-install/issued/" + nonce }
func spentKey(nonce string) string  { return "slack-install/spent/" + nonce }

// newNonce is 128 random bits, base64url.
func newNonce() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("slack: draw an install nonce: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

// IsInstallState reports a state minted by [Installs.Link], so the landing can
// tell an install the engine finishes from the provisioning command's, whose
// state is the bare handle. A handle never holds a dot, and a signed state
// always does.
func IsInstallState(state string) bool {
	return strings.Contains(strings.TrimSpace(state), ".")
}
