package github

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Keeping the document's record of each agent's own GitHub App in step with
// GitHub.
//
// # What this half can and cannot do
//
// It CREATES NOTHING. An app is created by a person POSTing a manifest from a
// browser, and installed by a person too, so the two acts this integration
// depends on are outside any loop. What the loop does here is record what
// those acts left behind: the installation a person made, an installation
// GitHub no longer has, an app somebody deleted. What each agent may do, and
// every finding about an agent, is the access sync's ([vendor]).
//
// # The installation is read back, never assumed
//
// A stored installation id is a claim about somebody else's account, and the
// operator can revoke it at GitHub without telling anyone. So the authority
// is `GET /app/installations`: an id that no longer appears there has been
// uninstalled, and the seat is back to needing a click. Reading it back is
// also the only way to learn what the installation actually GRANTS, which is
// not what the manifest asked for whenever a person edited it or widened it
// afterwards.
//
// # A failed read is not a finding
//
// Every conclusion here is drawn from an answer GitHub gave. Where GitHub
// could not be reached the seat is left exactly as it was, because reporting
// "no installation" on a timeout would tell an operator to redo a click that
// was never undone.
//
// # A cancelled pass is not a failed read
//
// That rule is right for a timeout and a 5xx (GitHub was asked and could not
// say) and wrong for a cancellation, which means this pass never asked at
// all. A pass whose context is done RAISES, and the caller decides, which for
// the loop means retrying rather than believing.

// SeatApp is one agent's own app, as the company document holds it.
type SeatApp struct {
	Handle string
	Name   string

	// Permissions are what this seat's tokens carry, in GitHub's terms
	// ([Permissions.Token] adds Metadata). The app is registered at these
	// (D5), so a raise past them waits for its owner.
	Permissions Permissions
	Repos       []string

	// Off is a seat switched off on GitHub: its installation is suspended
	// by the access sync and it is issued no token. Its record is still kept
	// in step, so a teardown reaches its suspended installation by id.
	Off bool

	// AppID and Slug are the app GitHub created. Zero means the operator
	// has not created one yet, which is a step rather than a fault.
	AppID int64
	Slug  string

	// InstallationID is what the last pass discovered, or zero.
	InstallationID int64

	// Key is the app's PEM, ALREADY RESOLVED. Empty means the document
	// names a variable nothing answers, which is a different fault from
	// naming none.
	Key string
}

// SeatAppOptions is what one pass over the seats needs.
type SeatAppOptions struct {
	// APIBase is the REST base, for Enterprise Server.
	APIBase string

	// WebBase is where a person's browser goes, for the install link a
	// finding carries.
	WebBase string

	// Org is the organization these apps are installed on. It is what an
	// installation's account is matched against, so an app installed on
	// somebody's personal account is not adopted by mistake.
	Org string

	// Seats are the agents to reconcile.
	Seats []SeatApp

	// Record persists an installation this pass discovered, or clears one
	// it found gone. Nil is a DRY RUN: the pass reads and reports and
	// writes nothing, which is what a check is.
	Record func(ctx context.Context, handle string, installationID int64) error

	// Forget clears the app a seat names when GitHub says that app no
	// longer exists. Nil is a DRY RUN, exactly as Record's is.
	//
	// THE DOCUMENT IS WRONG AND NOTHING ELSE CAN CORRECT IT. An app is
	// deleted from its settings page by a person, and GitHub tells the
	// engine nothing: the record left behind names an app id that answers
	// 404 to everything, so the seat mints no token, the install link
	// GitHub itself 404s, and every surface reports a step that cannot be
	// taken. Clearing it puts the seat back to the one thing that can be
	// done, which is creating another.
	//
	// The sealed key is left alone. It is named per seat, so creating the
	// next app overwrites it, and deleting it here would be a destructive
	// answer to a question GitHub is the only authority on.
	Forget func(ctx context.Context, handle string) error

	// Client builds the app client for one seat. Nil takes the real one.
	// Injectable because every seat has a different key, so there is no
	// single client to hand in.
	Client func(appID int64, pem, apiBase string) (*AppClient, error)
}

// AdoptSeatApps keeps the document's record of each seat's app in step with
// GitHub, and reports nothing: what a seat still owes is the access sync's to
// say ([seataccess.Sync] through this package's vendor).
//
// THE DOCUMENT WRITES THE TOKEN ISSUER DEPENDS ON, and only those. An
// installation a person made since the last pass is recorded on the seat, an
// installation GitHub no longer has is cleared, and an app GitHub no longer
// has is forgotten, so the issuer never mints against an id GitHub has
// forgotten. Off seats are kept in step too: a disconnect uninstalls their
// suspended installations by the recorded id.
//
// A DRY RUN (Record or Forget nil) writes nothing and does nothing. A read
// GitHub could not answer leaves the seat as it was; a write the store
// refused is returned, joined with the rest, after every seat is tried.
func AdoptSeatApps(ctx context.Context, opts SeatAppOptions) error {
	if opts.Record == nil || opts.Forget == nil {
		return nil
	}
	var failures []error
	for _, seat := range opts.Seats {
		if err := interrupted(ctx); err != nil {
			return err
		}
		if seat.AppID == 0 || strings.TrimSpace(seat.Key) == "" {
			continue
		}
		client, err := opts.client(seat)
		if err != nil {
			// A KEY THIS ENGINE CANNOT READ is the access sync's to
			// report; there is nothing here to keep in step.
			continue
		}
		installation, _, err := installationFor(ctx, opts, seat, client)
		var notRecorded *errNotRecorded
		switch {
		case errors.As(err, &notRecorded):
			failures = append(failures, errors.New(notRecorded.detail(seat.Handle)))
		case notFound(err) || (err == nil && installation == nil):
			if exists, existsErr := client.Exists(ctx); existsErr == nil && !exists {
				if forgetErr := opts.Forget(ctx, seat.Handle); forgetErr != nil {
					failures = append(failures, fmt.Errorf(
						"github: clear %s's record of an app GitHub no longer has: %w",
						seat.Handle, forgetErr))
				}
			}
		}
	}
	return errors.Join(failures...)
}

// notFound reports GitHub answering 404, which is the only status that can
// mean an app or an installation is absent.
func notFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.NotFound()
}

// installationFor finds the installation this seat's app is on.
//
// Three answers, and the middle one is why this is not a bool: the
// installation, nil for "there is none and that is the truth", and an error
// for "GitHub could not say".
//
// A FOURTH THING CAN GO WRONG and it is not any of those: the installation is
// found and cannot be WRITTEN DOWN. Read as a failed read, that would tell an
// operator to redo a click nobody reversed while the real failure, this
// engine's own write, went unreported. [errNotRecorded] keeps it apart.
func installationFor(
	ctx context.Context, opts SeatAppOptions, seat SeatApp, client *AppClient,
) (*Installation, bool, error) {
	if seat.InstallationID != 0 {
		installation, err := client.Installation(ctx, seat.InstallationID)
		var apiErr *APIError
		switch {
		case err == nil:
			return installation, false, nil
		case errors.As(err, &apiErr) && apiErr.NotFound():
			// UNINSTALLED BEHIND THE ENGINE'S BACK, which is a thing an
			// operator does at GitHub and tells nobody about. Clearing
			// the id is what puts the seat back to needing a click,
			// rather than retrying an id that will never answer again.
			if opts.Record != nil {
				if clearErr := opts.Record(ctx, seat.Handle, 0); clearErr != nil {
					// THE STALE ID SURVIVES, so the seat is not
					// merely uninstalled: the document still names
					// an installation GitHub has forgotten, and the
					// next pass will read it and 404 again. Named
					// rather than folded into the read failure, for
					// the same reason as the discovery write.
					return nil, false, &errNotRecorded{err: clearErr}
				}
			}
			return nil, false, nil
		default:
			return nil, false, err
		}
	}

	// NOT YET DISCOVERED. The app was created and then installed, and the
	// install does not tell the engine anything: it redirects a browser.
	// So the installation is found by asking the app where it lives.
	installations, err := client.Installations(ctx)
	if err != nil {
		return nil, false, err
	}
	want := strings.ToLower(strings.TrimSpace(opts.Org))
	for _, installation := range installations {
		// MATCHED ON THE ACCOUNT, so an app a person installed on their
		// own profile is not adopted as the organization's. Where the
		// company names no organization the only installation is taken,
		// because there is nothing to disagree with.
		login := strings.ToLower(strings.TrimSpace(installation.Account.Login))
		if want != "" && login != want {
			continue
		}
		if opts.Record != nil {
			if recordErr := opts.Record(ctx, seat.Handle, installation.ID); recordErr != nil {
				return nil, false, &errNotRecorded{
					id: installation.ID, err: recordErr,
				}
			}
		}
		return &installation, true, nil
	}
	return nil, false, nil
}

// errNotRecorded is a discovery this pass could not write down.
//
// The installation IS at GitHub (this pass read it), and what failed was
// persisting WHICH one. That is neither "GitHub could not say" nor "there is
// none", and collapsing it into either produces a sentence that contradicts
// what the pass just established.
//
// id is the installation that was found, and is zero when what failed was
// CLEARING a stale one instead.
type errNotRecorded struct {
	id  int64
	err error
}

func (e *errNotRecorded) Error() string { return e.err.Error() }
func (e *errNotRecorded) Unwrap() error { return e.err }

// detail is the sentence an operator reads, and it says which write failed
// rather than what the pass found: the failure is this engine's, not
// GitHub's.
func (e *errNotRecorded) detail(handle string) string {
	if e.id == 0 {
		return handle + "'s app was uninstalled at GitHub and this engine could " +
			"not clear the installation it still has recorded, so the seat " +
			"keeps trying to mint against an installation that no longer " +
			"exists: " + e.err.Error()
	}
	return handle + "'s app IS installed at GitHub and this engine could not " +
		"record which installation, so the seat mints no token until the " +
		"write succeeds. Nothing at GitHub needs changing: " + e.err.Error()
}

// TokenFor mints the credential one seat acts with.
//
// SCOPED TO THE SEAT'S PERMISSIONS at mint time, which is where they stop
// being words in a document: the app may hold more than the seat asks for,
// and the token carries only what the seat lists.
func TokenFor(
	ctx context.Context, opts SeatAppOptions, seat SeatApp,
) (*InstallationToken, error) {
	if seat.AppID == 0 || seat.InstallationID == 0 {
		return nil, fmt.Errorf(
			"github: %s has no installed app, so there is nothing to mint against",
			seat.Handle)
	}
	client, err := opts.client(seat)
	if err != nil {
		return nil, err
	}
	return client.MintToken(ctx, seat.InstallationID, seat.Permissions, seat.Repos)
}

// UninstallSeat removes one agent's installation, which is what actually
// revokes its access. Deleting the record and leaving the installation would
// leave an app able to act with nothing watching it.
func UninstallSeat(ctx context.Context, opts SeatAppOptions, seat SeatApp) error {
	if seat.AppID == 0 || seat.InstallationID == 0 {
		return nil
	}
	client, err := opts.client(seat)
	if err != nil {
		return err
	}
	err = client.Uninstall(ctx, seat.InstallationID)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.NotFound() {
		// ALREADY GONE IS THE OUTCOME ASKED FOR. A teardown that failed
		// because the thing was already removed would leave the record
		// behind for ever.
		return nil
	}
	return err
}

// builder is [SeatAppOptions.Client], or the real constructor when it is nil.
func (o SeatAppOptions) builder() func(int64, string, string) (*AppClient, error) {
	if o.Client != nil {
		return o.Client
	}
	return func(appID int64, pem, apiBase string) (*AppClient, error) {
		return NewAppClient(appID, pem, apiBase, nil)
	}
}

// client builds the app client one seat acts through.
func (o SeatAppOptions) client(seat SeatApp) (*AppClient, error) {
	return o.builder()(seat.AppID, seat.Key, o.APIBase)
}

// orgLabel names the account an app is installed on, for a sentence.
func orgLabel(org string) string {
	if name := strings.TrimSpace(org); name != "" {
		return name
	}
	return "your organization"
}

// SeatsFrom reads the seats out of what a caller has already gathered,
// keeping them in a stable order so two passes report the same seat first.
func SeatsFrom(seats []SeatApp) []SeatApp {
	out := slices.Clone(seats)
	slices.SortFunc(out, func(a, b SeatApp) int { return strings.Compare(a.Handle, b.Handle) })
	return out
}
