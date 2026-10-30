package slack

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/crewlet/crewlet/internal/provision"
)

// Each agent's Slack app, kept by the reconcile loop (D11).
//
// # What moved into the loop, and what stays a person's
//
// Creating an app, keeping its manifest in line and deleting it are API calls
// on the app-configuration token, which the company seals once
// ([ConfigTokenVar]), so the loop does them. Installing an app into a
// workspace is OAuth, and OAuth exists so that granting an app a workspace is
// a person's decision: that stays a click, offered as a waiting step with the
// link, and the redirect it ends in is what [CompleteSeatInstall] finishes.
//
// # Served once, so sealed before anything else
//
// Slack serves an app's client secret and signing secret once, at creation,
// and a bot token once, at install. A create therefore hands its values to the
// sync to seal, each with the undo that deletes the app again if it cannot be
// sealed, and an install seals its token before it reports anything, revoking
// the token when the seal fails (design 5.2).

// SeatApp is one agent's Slack app: the app the ledger records for it, and
// the values sealed under its engine-named variables, already resolved.
type SeatApp struct {
	Handle string

	// RoleName is the app's display name ([Manifest]).
	RoleName string

	// AppID is the app the ledger records ([seataccess.Account.ID]) and
	// ClientID its OAuth client id, sealed beside its secret.
	AppID    string
	ClientID string

	// BotToken, SigningSecret and ClientSecret are the sealed values
	// ([SeatVar]), "" where nothing is sealed.
	BotToken      string
	SigningSecret string
	ClientSecret  string
}

// ErrNoBaseURL refuses a step that would build an app pointing nowhere.
var ErrNoBaseURL = errors.New("slack: no public base URL: every app's request URL " +
	"and redirect URL are built from it, so an app made without one delivers " +
	"nowhere and cannot be installed")

// createApp validates the seat's manifest and creates its app. The caller
// seals what it returns before anything else.
func createApp(ctx context.Context, admin *Admin, configToken, base string, seat SeatApp) (AppCredentials, error) {
	manifest, err := Manifest(seat.RoleName, seat.Handle, base)
	if err != nil {
		return AppCredentials{}, err
	}
	// VALIDATED FIRST, for the reason the CLI does: the create is Tier 1,
	// and a malformed manifest found by it costs a minute per seat.
	if validateErr := admin.ValidateManifest(ctx, configToken, manifest, ""); validateErr != nil {
		return AppCredentials{}, validateErr
	}
	created, err := admin.CreateApp(ctx, configToken, manifest)
	if err != nil {
		return AppCredentials{}, err
	}
	if created.AppID == "" || created.ClientID == "" || created.ClientSecret == "" ||
		created.SigningSecret == "" {
		// AN APP SLACK DID NOT HAND THE VALUES FOR can never be installed
		// or verified, and the values cannot be asked for again, so it is
		// deleted rather than kept.
		deleteErr := error(nil)
		if created.AppID != "" {
			deleteErr = admin.DeleteApp(ctx, configToken, created.AppID)
		}
		return AppCredentials{}, errors.Join(
			errors.New("slack: apps.manifest.create answered without the app's credentials"),
			deleteErr)
	}
	return created, nil
}

// InstallLink is where a person approves a seat's install, carrying state, a
// value the caller sealed for this seat and its app so the landing can prove
// the redirect belongs to an install this engine began. Empty when the app
// has no OAuth client yet.
func InstallLink(seat SeatApp, base, state string) string {
	if strings.TrimSpace(seat.ClientID) == "" || strings.TrimSpace(base) == "" {
		return ""
	}
	return AuthorizeURLWithState(seat.ClientID, base, state)
}

// CompleteSeatInstall exchanges the code a person's install returned and
// seals the bot token under the seat's own variable.
//
// THE CODE MUST BELONG TO THIS SEAT'S APP. The landing serves every agent's
// app, so a code from another agent's install would otherwise mint that
// agent's bot token into this seat, and the seat would post as a colleague.
// A token for another app is revoked and nothing is sealed.
//
// A SEAL THAT FAILS REVOKES THE TOKEN, so no bot token is live that nothing
// holds.
func CompleteSeatInstall(
	ctx context.Context, admin *Admin, sink provision.TokenSink, httpClient *http.Client,
	seat SeatApp, code, base string,
) (Install, error) {
	switch {
	case admin == nil:
		return Install{}, errors.New("slack: no admin client")
	case sink == nil:
		return Install{}, provision.ErrNoSink
	case strings.TrimSpace(base) == "":
		return Install{}, ErrNoBaseURL
	case seat.AppID == "" || seat.ClientID == "" || seat.ClientSecret == "":
		return Install{}, fmt.Errorf(
			"slack: %s has no app to install yet, or its client secret is not sealed",
			seat.Handle)
	}
	// THE REDIRECT IS THE ONE THE APP REGISTERED: [Manifest] and the
	// exchange both build it with [OAuthRedirectURL] from the same base,
	// and Slack refuses an exchange whose redirect differs from the
	// authorize request's.
	installed, err := admin.Exchange(ctx, seat.ClientID, seat.ClientSecret,
		strings.TrimSpace(code), base)
	if err != nil {
		return Install{}, err
	}
	if got := strings.TrimSpace(installed.AppID); got != seat.AppID {
		// AN ANSWER NAMING NO APP is refused too: the check is what keeps
		// one agent's token out of another's seat, and an exchange that
		// cannot say whose token it minted has not passed it.
		return Install{}, errors.Join(fmt.Errorf(
			"%w: the install answered app %q, not %s's app %s; nothing was recorded",
			errOtherApp, got, seat.Handle, seat.AppID),
			revokeToken(ctx, httpClient, installed.BotToken))
	}
	tokenVar := SeatVar(seat.Handle, FieldBotToken)
	if err := sink.Record(ctx, tokenVar, installed.BotToken); err != nil {
		return Install{}, errors.Join(
			fmt.Errorf("slack: seal %s: %w", tokenVar, err),
			revokeToken(ctx, httpClient, installed.BotToken))
	}
	return installed, nil
}

// errOtherApp marks an install whose token belongs to an app other than the
// seat's.
var errOtherApp = errors.New("slack: the install belongs to another app")

// Observed is what Slack shows for one seat's app now.
type Observed struct {
	// AppExists is false when Slack no longer has the app the seat names.
	// Taken from the ledger's claim where no configuration token can read
	// it ([Observed.ManifestRead] false).
	AppExists bool

	// ManifestRead reports that the app's manifest was read, so Diff is
	// Slack's answer rather than an assumption.
	ManifestRead bool

	// Diff is how the app's manifest at Slack differs from this engine's.
	Diff ManifestDiff

	// TokenWorks reports a sealed bot token Slack still accepts, which is
	// what "installed and on" means at Slack.
	TokenWorks bool

	// BotUserID and BotName are who the working token authenticates as.
	BotUserID string
	BotName   string
}

// ObserveSeat reads one seat's app and token back from Slack.
//
// AN ERROR IS "SLACK COULD NOT SAY", never a finding: the caller leaves the
// seat as it was, because reporting an app gone on a timeout sends a person
// to recreate something that exists.
func ObserveSeat(
	ctx context.Context, admin *Admin, configToken, base string, httpClient *http.Client, seat SeatApp,
) (Observed, error) {
	var out Observed
	switch {
	case seat.AppID == "":
	case configToken != "" && admin != nil && strings.TrimSpace(base) != "":
		held, err := admin.ExportManifest(ctx, configToken, seat.AppID)
		switch {
		case appGone(err):
			return out, nil
		case err != nil:
			return out, err
		}
		want, err := Manifest(seat.RoleName, seat.Handle, base)
		if err != nil {
			return out, err
		}
		out.AppExists, out.ManifestRead, out.Diff = true, true, DiffManifest(held, want)
	default:
		// NO CONFIGURATION TOKEN, so the app cannot be read: it is taken
		// to exist, which is the ledger's claim, and the token below is
		// still checked.
		out.AppExists = true
	}
	if strings.TrimSpace(seat.BotToken) == "" {
		return out, nil
	}
	client, err := NewClient(seat.BotToken, httpClient)
	if err != nil {
		return out, err
	}
	id, err := client.AuthTest(ctx)
	switch {
	case TokenDead(err):
		return out, nil
	case err != nil:
		return out, err
	}
	out.TokenWorks, out.BotUserID, out.BotName = true, id.UserID, id.UserName
	return out, nil
}

// restoreManifest pushes this engine's manifest over the app, which is the
// drift restore for a scope or setting changed in Slack's console (D2).
func restoreManifest(ctx context.Context, admin *Admin, configToken, base string, seat SeatApp) error {
	manifest, err := Manifest(seat.RoleName, seat.Handle, base)
	if err != nil {
		return err
	}
	return admin.UpdateApp(ctx, configToken, seat.AppID, manifest)
}

// revokeToken withdraws a bot token. A token Slack already refuses is
// revoked; an empty one is nothing to revoke.
func revokeToken(ctx context.Context, httpClient *http.Client, token string) error {
	if strings.TrimSpace(token) == "" {
		return nil
	}
	client, err := NewClient(token, httpClient)
	if err != nil {
		return err
	}
	if _, err := client.Revoke(ctx); err != nil {
		return fmt.Errorf("slack: revoke the bot token: %w", err)
	}
	return nil
}
