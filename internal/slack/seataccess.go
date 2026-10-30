package slack

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/provision"
)

// Each agent's Slack app, kept by the reconcile loop (D11).
//
// # What moved into the loop, and what stays a person's
//
// Creating an app and keeping its manifest in line are API calls on the
// app-configuration token, which the company seals once ([ConfigTokenVar]),
// so the loop does them. Installing an app into a workspace is OAuth, and
// OAuth exists so that granting an app a workspace is a person's decision:
// that stays a click, offered as a waiting step with the link, and the
// redirect it ends in is what [CompleteSeatInstall] finishes.
//
// # The order every step keeps
//
// Slack serves an app's client secret and signing secret once, at creation,
// and a bot token once, at install. So each step seals what Slack served
// BEFORE it records anything in the company document, and a step that cannot
// finish undoes what it made at Slack rather than leaving a live app or token
// nothing holds (design 5.2: a failed record after a mint revokes the mint).

// SeatApp is one agent's Slack app as the loop holds it: the engine-written
// fields of its document block, and the values sealed under its
// engine-named variables, already resolved.
type SeatApp struct {
	Handle string

	// RoleName is the app's display name ([Manifest]).
	RoleName string

	// The engine-written fields of the seat's block.
	AppFields

	// BotToken, SigningSecret and ClientSecret are the sealed values
	// ([SeatVar]), "" where nothing is sealed.
	BotToken      string
	SigningSecret string
	ClientSecret  string
}

// AppFields is what the engine writes onto a seat's Slack block: the ids
// that name its app and its install. None of them is a secret.
type AppFields struct {
	AppID     string
	ClientID  string
	BotUserID string
	TeamID    string
}

// Installed reports a seat whose app a workspace install completed.
func (f AppFields) Installed() bool { return f.BotUserID != "" }

// LoopOptions is what the loop's Slack steps need.
type LoopOptions struct {
	Admin *Admin

	// ConfigToken is a usable app-configuration access token, from
	// [Admin.ConfigToken]. Empty means the company has sealed none, and
	// every step that needs it reports so rather than calling Slack.
	ConfigToken string

	// Sink seals and clears the seat's engine-named variables.
	Sink provision.TokenSink

	// BaseURL is the deployment's public base every app's request and
	// redirect URLs are built from.
	BaseURL string

	// Record writes the engine-written fields onto the seat's block. A
	// zero AppFields clears them.
	Record func(ctx context.Context, handle string, fields AppFields) error

	// HTTP is the client a seat's bot token is used through. Nil takes
	// the default.
	HTTP *http.Client
}

// ErrNoBaseURL refuses a step that would build an app pointing nowhere.
var ErrNoBaseURL = errors.New("slack: no public base URL: every app's request URL " +
	"and redirect URL are built from it, so an app made without one delivers " +
	"nowhere and cannot be installed")

// CreateSeatApp creates a seat's app from its manifest, seals the two
// secrets Slack serves once, and records the app on the seat.
//
// UNDONE IF IT CANNOT FINISH. An app whose secrets were not sealed can never
// be installed or verified, and one the document does not name is an app
// nothing will ever manage, so a failure after the create deletes the app
// again and clears whatever was sealed. Where even that fails, the error
// names the app to delete by hand.
func CreateSeatApp(ctx context.Context, opts LoopOptions, seat SeatApp) (AppFields, error) {
	if err := opts.ready(); err != nil {
		return AppFields{}, err
	}
	manifest, err := Manifest(seat.RoleName, seat.Handle, opts.BaseURL)
	if err != nil {
		return AppFields{}, err
	}
	// VALIDATED FIRST, for the reason the CLI does: the create is Tier 1,
	// and a malformed manifest found by it costs a minute per seat.
	if err := opts.Admin.ValidateManifest(ctx, opts.ConfigToken, manifest, ""); err != nil {
		return AppFields{}, err
	}
	created, err := opts.Admin.CreateApp(ctx, opts.ConfigToken, manifest)
	if err != nil {
		return AppFields{}, err
	}
	fields := AppFields{AppID: created.AppID, ClientID: created.ClientID}
	sealed, sealErr := opts.seal(ctx, map[string]string{
		SeatVar(seat.Handle, FieldClientSecret):  created.ClientSecret,
		SeatVar(seat.Handle, FieldSigningSecret): created.SigningSecret,
	})
	if sealErr == nil {
		sealErr = opts.Record(ctx, seat.Handle, fields)
	}
	if sealErr == nil {
		return fields, nil
	}
	return AppFields{}, opts.undoCreate(ctx, created.AppID, sealed, sealErr)
}

// undoCreate deletes an app a create could not finish, and clears what it
// sealed for it.
func (o LoopOptions) undoCreate(ctx context.Context, appID string, sealed []string, cause error) error {
	deleteErr := o.Admin.DeleteApp(ctx, o.ConfigToken, appID)
	var forgetErr error
	if len(sealed) > 0 {
		forgetErr = o.Sink.Forget(ctx, sealed...)
	}
	if deleteErr != nil {
		return fmt.Errorf(
			"slack: app %s was created and could not be recorded (%w), and deleting "+
				"it again failed too, so delete it by hand at %s: %w",
			appID, cause, ManageURL(appID), errors.Join(deleteErr, forgetErr))
	}
	if forgetErr != nil {
		return fmt.Errorf("slack: app %s could not be recorded (%w) and was deleted "+
			"again; clearing its sealed secrets failed: %w", appID, cause, forgetErr)
	}
	return fmt.Errorf("slack: app %s could not be recorded and was deleted again: %w",
		appID, cause)
}

// InstallLink is where a person approves a seat's install, carrying the
// sealed state the landing verifies. Empty when the app is not created yet.
func InstallLink(seat SeatApp, base, state string) string {
	if strings.TrimSpace(seat.ClientID) == "" {
		return ""
	}
	return AuthorizeURLWithState(seat.ClientID, base, state)
}

// CompleteSeatInstall exchanges the code a person's install returned, seals
// the bot token under the seat's own variable and records the install.
//
// THE CODE MUST BELONG TO THIS SEAT'S APP. The landing serves every agent's
// app, so a code from another agent's install would otherwise mint that
// agent's bot token into this seat, and the seat would post as a colleague.
// Nothing is sealed or recorded for a code that names another app.
//
// A RECORD THAT FAILS REVOKES THE TOKEN it would have recorded, and clears
// it, so no bot token is live that the document does not account for.
func CompleteSeatInstall(ctx context.Context, opts LoopOptions, seat SeatApp, code string) (AppFields, error) {
	if err := opts.ready(); err != nil {
		return AppFields{}, err
	}
	if seat.AppID == "" || seat.ClientID == "" || seat.ClientSecret == "" {
		return AppFields{}, fmt.Errorf(
			"slack: %s has no app to install yet, or its client secret is not sealed",
			seat.Handle)
	}
	installed, err := opts.Admin.Exchange(ctx, seat.ClientID, seat.ClientSecret,
		strings.TrimSpace(code), opts.BaseURL)
	if err != nil {
		return AppFields{}, err
	}
	if got := strings.TrimSpace(installed.AppID); got != "" && got != seat.AppID {
		// THE TOKEN IS REVOKED, not merely left unrecorded: it is a live
		// credential for another agent's app that nothing asked for.
		revokeErr := opts.revoke(ctx, installed.BotToken)
		return AppFields{}, errors.Join(fmt.Errorf(
			"slack: that install belongs to app %s, not %s's app %s; nothing was "+
				"recorded", got, seat.Handle, seat.AppID), revokeErr)
	}
	tokenVar := SeatVar(seat.Handle, FieldBotToken)
	fields := seat.AppFields
	fields.BotUserID, fields.TeamID = installed.BotUserID, installed.TeamID
	if err := opts.Sink.Record(ctx, tokenVar, installed.BotToken); err != nil {
		return AppFields{}, errors.Join(
			fmt.Errorf("slack: seal %s: %w", tokenVar, err),
			opts.revoke(ctx, installed.BotToken))
	}
	if err := opts.Record(ctx, seat.Handle, fields); err != nil {
		return AppFields{}, errors.Join(
			fmt.Errorf("slack: record %s's install: %w", seat.Handle, err),
			opts.revoke(ctx, installed.BotToken),
			opts.Sink.Forget(ctx, tokenVar))
	}
	return fields, nil
}

// Observed is what Slack shows for one seat's app now.
type Observed struct {
	// AppExists is false when Slack no longer has the app the seat names.
	AppExists bool

	// Diff is how the app's manifest at Slack differs from this engine's.
	Diff ManifestDiff

	// TokenWorks reports a sealed bot token Slack still accepts, which is
	// what "installed and on" means at Slack.
	TokenWorks bool

	// BotUserID and TeamID are who the working token authenticates as.
	BotUserID string
	TeamID    string
}

// ObserveSeat reads one seat's app and token back from Slack.
//
// AN ERROR IS "SLACK COULD NOT SAY", never a finding: the caller leaves the
// seat as it was, because reporting an app gone on a timeout sends a person
// to recreate something that exists.
func ObserveSeat(ctx context.Context, opts LoopOptions, seat SeatApp) (Observed, error) {
	var out Observed
	if seat.AppID != "" && opts.ConfigToken != "" {
		held, err := opts.Admin.ExportManifest(ctx, opts.ConfigToken, seat.AppID)
		switch {
		case appGone(err):
			return out, nil
		case err != nil:
			return out, err
		}
		out.AppExists = true
		want, err := Manifest(seat.RoleName, seat.Handle, opts.BaseURL)
		if err != nil {
			return out, err
		}
		out.Diff = DiffManifest(held, want)
	} else if seat.AppID != "" {
		// NO CONFIGURATION TOKEN, so the app cannot be read: it is taken
		// to exist, which is the document's claim, and the token below
		// is still checked.
		out.AppExists = true
	}
	if strings.TrimSpace(seat.BotToken) == "" {
		return out, nil
	}
	client, err := NewClient(seat.BotToken, opts.HTTP)
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
	out.TokenWorks, out.BotUserID, out.TeamID = true, id.UserID, id.TeamID
	return out, nil
}

// RestoreManifest pushes this engine's manifest over the app, which is the
// drift restore for a scope or setting changed in Slack's console (D2).
//
// A scope ADDED back needs a reinstall before the bot token carries it
// ([ManifestDiff.NeedsReinstall]), which the caller offers as a waiting step.
func RestoreManifest(ctx context.Context, opts LoopOptions, seat SeatApp) error {
	if err := opts.ready(); err != nil {
		return err
	}
	manifest, err := Manifest(seat.RoleName, seat.Handle, opts.BaseURL)
	if err != nil {
		return err
	}
	return opts.Admin.UpdateApp(ctx, opts.ConfigToken, seat.AppID, manifest)
}

// TurnOffSeat revokes the seat's bot token and clears it (D1). The app and
// its install record stay, so the seat's history keeps its author; turning
// it back on is a fresh install a person approves.
//
// REVOKED BEFORE CLEARED. A token cleared first and then not revoked would be
// live with nothing holding it; a revoke that fails leaves the sealed value
// for the retry to revoke.
func TurnOffSeat(ctx context.Context, opts LoopOptions, seat SeatApp) error {
	if strings.TrimSpace(seat.BotToken) != "" {
		if err := opts.revoke(ctx, seat.BotToken); err != nil {
			return err
		}
	}
	fields := seat.AppFields
	fields.BotUserID, fields.TeamID = "", ""
	if err := opts.Sink.Forget(ctx, SeatVar(seat.Handle, FieldBotToken)); err != nil {
		return fmt.Errorf("slack: clear %s's bot token: %w", seat.Handle, err)
	}
	return opts.Record(ctx, seat.Handle, fields)
}

// RotateSeat revokes the seat's bot token so the next install mints a fresh
// one. Slack hands a reinstalled app the token it already holds, so a rotate
// that did not revoke first would change nothing; the seat is off until a
// person approves the reinstall.
func RotateSeat(ctx context.Context, opts LoopOptions, seat SeatApp) error {
	return TurnOffSeat(ctx, opts, seat)
}

// RemoveSeatApp deletes a seat's app at Slack and clears everything sealed
// and recorded for it. Called only for a seat that is off and whose removal
// a person confirmed (design 5.3).
//
// It answers the link a person deletes the app at when it cannot: no
// configuration token is sealed, or Slack refused the delete. The sealed
// values and the record are cleared only once the app is gone, so a person's
// link still names it.
func RemoveSeatApp(ctx context.Context, opts LoopOptions, seat SeatApp) (string, error) {
	if seat.AppID != "" {
		if opts.ConfigToken == "" {
			return ManageURL(seat.AppID), nil
		}
		if err := opts.Admin.DeleteApp(ctx, opts.ConfigToken, seat.AppID); err != nil {
			return ManageURL(seat.AppID), err
		}
	}
	if strings.TrimSpace(seat.BotToken) != "" {
		if err := opts.revoke(ctx, seat.BotToken); err != nil {
			return "", err
		}
	}
	if err := opts.Sink.Forget(ctx, SeatVars(seat.Handle)...); err != nil {
		return "", fmt.Errorf("slack: clear %s's sealed values: %w", seat.Handle, err)
	}
	return "", opts.Record(ctx, seat.Handle, AppFields{})
}

// ready refuses a step that has nothing to act with.
func (o LoopOptions) ready() error {
	switch {
	case o.Admin == nil:
		return errors.New("slack: no admin client")
	case o.Sink == nil:
		return provision.ErrNoSink
	case o.Record == nil:
		return errors.New("slack: no document writer")
	case strings.TrimSpace(o.BaseURL) == "":
		return ErrNoBaseURL
	case strings.TrimSpace(o.ConfigToken) == "":
		return ErrNoConfigToken
	}
	return nil
}

// seal records each value under its name, in name order, and answers the
// names it sealed so a failure can clear exactly those.
func (o LoopOptions) seal(ctx context.Context, values map[string]string) ([]string, error) {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	slices.Sort(names)
	var sealed []string
	for _, name := range names {
		if strings.TrimSpace(values[name]) == "" {
			return sealed, fmt.Errorf("slack: Slack served no value for %s", name)
		}
		if err := o.Sink.Record(ctx, name, values[name]); err != nil {
			return sealed, fmt.Errorf("slack: seal %s: %w", name, err)
		}
		sealed = append(sealed, name)
	}
	return sealed, nil
}

// revoke withdraws a bot token. A token Slack already refuses is revoked.
func (o LoopOptions) revoke(ctx context.Context, token string) error {
	if strings.TrimSpace(token) == "" {
		return nil
	}
	client, err := NewClient(token, o.HTTP)
	if err != nil {
		return err
	}
	if _, err := client.Revoke(ctx); err != nil {
		return fmt.Errorf("slack: revoke the bot token: %w", err)
	}
	return nil
}
