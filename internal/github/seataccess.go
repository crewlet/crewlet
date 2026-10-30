package github

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/provision"
)

// Applying one seat's access at GitHub: observe, switch off, switch back on,
// remove, and say what a raise is waiting for.
//
// # What each switch is, at GitHub
//
// Off suspends the seat's installation, as the app (D1): the app, its key and
// the installation stay, every token is refused at once, and back on lifts
// the suspension with no person involved. Remove uninstalls and clears the
// sealed key; GitHub offers no endpoint that deletes an app registration, so
// the last step is a link (design 5.2). Narrowing permissions or repositories
// needs nothing at GitHub at all: every token is minted with exactly the
// seat's permissions and repositories, so the next mint carries the change.

// SeatObservation is what GitHub shows for one seat's app now.
type SeatObservation struct {
	// AppExists is false when GitHub no longer has the app the seat names.
	AppExists bool

	// Registered is what the app's registration asks every installation
	// for, nil where the app does not exist.
	Registered map[string]string

	// Installation is the seat's installation, nil where none exists.
	Installation *Installation

	// Adopted reports an installation this observation discovered and
	// recorded.
	Adopted bool

	// Repositories is what the installation covers, as `owner/name`. It
	// is read only where it can matter: the seat narrows to repositories
	// and the installation covers selected ones. Nil otherwise.
	Repositories []string

	// RepositoriesUnread reports an installation whose repositories could
	// matter and were not read because it is suspended: listing them takes
	// an installation token, and a suspended installation is issued none.
	// What it covers is then unknown, never missing.
	RepositoriesUnread bool
}

// ObserveSeat reads one seat's app, registration and installation back from
// GitHub, adopting an installation a person made since the last pass.
//
// AN ERROR IS "GITHUB COULD NOT SAY", never a finding, for the reason the
// seat-app pass gives: an operator told to redo a click nobody reversed has
// been misled. A seat with no app or no key is not observed at all; the
// caller already knows its state from the document.
func ObserveSeat(ctx context.Context, opts SeatAppOptions, seat SeatApp) (SeatObservation, error) {
	var out SeatObservation
	if seat.AppID == 0 || strings.TrimSpace(seat.Key) == "" {
		return out, nil
	}
	client, err := opts.client(seat)
	if err != nil {
		return out, err
	}
	registration, err := client.Registration(ctx)
	switch {
	case notFound(err):
		return out, nil
	case err != nil:
		return out, err
	}
	out.AppExists, out.Registered = true, registration.Permissions
	installation, adopted, err := installationFor(ctx, opts, seat, client)
	if err != nil {
		return out, err
	}
	out.Installation, out.Adopted = installation, adopted
	if installation == nil || installation.RepositorySelection == RepositorySelectionAll ||
		len(repoNames(seat.Repos)) == 0 {
		return out, nil
	}
	if installation.Suspended() {
		// NOT ASKED WHILE SUSPENDED: the listing mints a token GitHub
		// refuses for a suspended installation, and that refusal is not
		// a fact about this seat's repositories. The seat's turn back on
		// reads them again.
		out.RepositoriesUnread = true
		return out, nil
	}
	repos, err := client.InstallationRepositories(ctx, installation.ID)
	if err != nil {
		return out, err
	}
	out.Repositories = repos
	return out, nil
}

// UncoveredRepositories is the seat's repositories its installation does not
// cover, sorted. A token naming one of them is refused outright, so the seat
// mints nothing until a person adds them to the installation or the seat
// stops naming them.
func (o SeatObservation) UncoveredRepositories(seat SeatApp) []string {
	if o.Installation == nil || o.Installation.RepositorySelection == RepositorySelectionAll ||
		o.RepositoriesUnread {
		return nil
	}
	covered := make([]string, 0, len(o.Repositories))
	for _, repo := range o.Repositories {
		covered = append(covered, strings.ToLower(repo))
	}
	var out []string
	for _, repo := range seat.Repos {
		name := strings.TrimSpace(repo)
		if name != "" && !slices.Contains(covered, strings.ToLower(name)) {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// Wait is a step only a person at GitHub can take, with where to take it.
type Wait struct {
	// Missing is the permission keys still outstanding.
	Missing []string

	// URL is the page the person acts on.
	URL string

	// Owner reports who acts: true for the app's owner, who adds the
	// permission to the registration; false for an organization owner,
	// who accepts the request on the installation.
	Owner bool

	// Detail is the sentence the tab shows.
	Detail string
}

// RaiseWait is what a seat's permissions are waiting for at GitHub, or nil
// when the installation already grants them (D5).
//
// TWO PEOPLE, IN ORDER. An app asks every installation for what its
// registration lists, and the registration is edited only on its own
// permissions page by the app's owner. Once it lists the permission, GitHub
// asks the organization to accept the change on the installation, and only
// then can a token carry it. So the first wait names the permissions page,
// and the second names the installation's page.
func RaiseWait(webBase, org string, seat SeatApp, want Permissions, obs SeatObservation) *Wait {
	if !obs.AppExists || obs.Installation == nil {
		return nil
	}
	if missing := want.Short(obs.Registered); len(missing) > 0 {
		return &Wait{
			Missing: missing,
			URL:     PermissionsURL(webBase, org, seat.Slug),
			Owner:   true,
			Detail: seat.Handle + "'s app is registered without " + Names(missing) +
				" at the level this seat is set to. Add " + Names(missing) +
				" on the app's permissions page; GitHub then asks the organization " +
				"to accept the change",
		}
	}
	missing := want.Short(obs.Installation.Permissions)
	if len(missing) == 0 {
		return nil
	}
	url := strings.TrimSpace(obs.Installation.HTMLURL)
	if url == "" {
		url = InstallationSettingsURL(webBase, org, obs.Installation.ID)
	}
	return &Wait{
		Missing: missing,
		URL:     url,
		Detail: "GitHub is waiting for an organization owner to accept " +
			Names(missing) + " for " + seat.Handle + "'s app on its installation. " +
			"Until then GitHub refuses every token this seat asks for",
	}
}

// SuspendSeat switches a seat's installation off (D1). A seat with no
// installation has nothing to switch off, and an installation GitHub no
// longer has is off already.
func SuspendSeat(ctx context.Context, opts SeatAppOptions, seat SeatApp) error {
	if seat.AppID == 0 || seat.InstallationID == 0 {
		return nil
	}
	client, err := opts.client(seat)
	if err != nil {
		return err
	}
	if err := client.Suspend(ctx, seat.InstallationID); err != nil && !notFound(err) {
		return err
	}
	return nil
}

// UnsuspendSeat switches a seat's installation back on.
//
// ONLY FOR A SUSPENSION THIS ENGINE MADE. The caller reads the account
// ledger first; a suspension somebody placed at GitHub stays until a person
// chooses Turn back on (D3), and that choice arrives here the same way.
func UnsuspendSeat(ctx context.Context, opts SeatAppOptions, seat SeatApp) error {
	if seat.AppID == 0 || seat.InstallationID == 0 {
		return fmt.Errorf("github: %s has no installation to switch back on", seat.Handle)
	}
	client, err := opts.client(seat)
	if err != nil {
		return err
	}
	return client.Unsuspend(ctx, seat.InstallationID)
}

// RemoveOptions is what a removal needs beyond the seat-app options.
type RemoveOptions struct {
	// Sink clears the seat's sealed key and webhook secret.
	Sink provision.TokenSink

	// Forget clears the app recorded on the seat's block.
	Forget func(ctx context.Context, handle string) error
}

// RemoveSeat uninstalls a seat's app, clears its sealed key and webhook
// secret and the app recorded on the seat, and answers the page where a
// person deletes the registration, which GitHub offers no endpoint for.
//
// Called only for a seat that is off and whose removal a person confirmed
// (design 5.3). UNINSTALLED FIRST: clearing the key before the uninstall
// would leave an installation able to act that nothing can reach any more.
func RemoveSeat(ctx context.Context, opts SeatAppOptions, remove RemoveOptions, seat SeatApp) (string, error) {
	if remove.Sink == nil {
		return "", provision.ErrNoSink
	}
	if remove.Forget == nil {
		return "", errors.New("github: no document writer")
	}
	manage := ManageURL(opts.WebBase, opts.Org, seat.Slug)
	if err := UninstallSeat(ctx, opts, seat); err != nil {
		return manage, err
	}
	if err := remove.Sink.Forget(ctx,
		SeatVar(seat.Handle, FieldAppKey),
		SeatVar(seat.Handle, FieldAppWebhookSecret)); err != nil {
		return manage, fmt.Errorf("github: clear %s's sealed app key: %w", seat.Handle, err)
	}
	if err := remove.Forget(ctx, seat.Handle); err != nil {
		return manage, fmt.Errorf("github: clear %s's app record: %w", seat.Handle, err)
	}
	return manage, nil
}
