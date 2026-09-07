package github

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/integration"
)

// Keeping each agent's own GitHub App in line with the company.
//
// # What this pass can and cannot do
//
// It CREATES NOTHING. An app is created by a person POSTing a manifest from a
// browser, and installed by a person too, so the two acts this integration
// depends on are outside any loop. What the loop can do is everything after
// them: find the installation, check that what it holds still matches the
// tier the seat is set to, and notice when somebody has uninstalled it.
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

// SeatApp is one agent's own app, as the company document holds it.
type SeatApp struct {
	Handle string
	Name   string
	Tier   Tier
	Repos  []string

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

	// Client builds the app client for one seat. Nil takes the real one.
	// Injectable because every seat has a different key, so there is no
	// single client to hand in.
	Client func(appID int64, pem, apiBase string) (*AppClient, error)
}

// SeatAppResult is what one pass concluded.
type SeatAppResult struct {
	// Ready is the seats whose app is installed and whose token can be
	// minted, which is the only state an agent can act from.
	Ready []string

	// Adopted is the seats whose installation this pass discovered.
	Adopted []string

	// Findings is what is outstanding, per seat.
	Findings []integration.Finding
}

// ReconcileSeatApps brings every agent's own app in line.
func ReconcileSeatApps(ctx context.Context, opts SeatAppOptions) (*SeatAppResult, error) {
	build := opts.Client
	if build == nil {
		build = func(appID int64, pem, apiBase string) (*AppClient, error) {
			return NewAppClient(appID, pem, apiBase, nil)
		}
	}
	out := &SeatAppResult{Findings: []integration.Finding{}}
	for _, seat := range opts.Seats {
		out.reconcileSeat(ctx, opts, seat, build)
	}
	return out, nil
}

func (r *SeatAppResult) reconcileSeat(
	ctx context.Context, opts SeatAppOptions, seat SeatApp,
	build func(int64, string, string) (*AppClient, error),
) {
	switch {
	case seat.AppID == 0:
		// A STEP, NOT A FAULT, and the wording says so: nobody has done
		// anything wrong, there is a click outstanding. The action URL is
		// empty because creating an app is a form POST from a page and
		// not an address anybody can be sent to.
		r.Findings = append(r.Findings, integration.Finding{
			Kind:    integration.FindingIdentityMissing,
			Subject: seat.Handle,
			Detail: seat.Handle + " has no GitHub App of its own, so nothing it " +
				"does on GitHub is its own: create one from the Integrations screen",
		})
		return

	case strings.TrimSpace(seat.Key) == "":
		// THE KEY IS GONE AND CANNOT BE REISSUED. GitHub hands it over
		// once, at creation, so this is not a credential to re-enter: the
		// app has to be created again.
		r.Findings = append(r.Findings, integration.Finding{
			Kind:    integration.FindingCredentialMissing,
			Subject: seat.Handle,
			Detail: "the private key for " + seat.Handle + "'s app did not resolve, " +
				"and GitHub issues it once: delete the app at GitHub and create it again",
		})
		return
	}

	client, err := build(seat.AppID, seat.Key, opts.APIBase)
	if err != nil {
		r.Findings = append(r.Findings, integration.Finding{
			Kind:    integration.FindingCredentialRejected,
			Subject: seat.Handle,
			Detail:  "the app key for " + seat.Handle + " could not be read: " + err.Error(),
		})
		return
	}

	installation, adopted, err := r.installationFor(ctx, opts, seat, client)
	switch {
	case err != nil && seat.InstallationID != 0:
		// LEFT ALONE, and only where the document already claims an
		// installation. See the package note: a failed read is not
		// evidence that anything was undone, and saying otherwise sends
		// an operator to redo a click nobody reversed.
		return
	case err != nil, installation == nil:
		// NOT INSTALLED IS A FACT THE DOCUMENT HOLDS, not one GitHub has
		// to confirm. A seat with no installation recorded has a click
		// outstanding whether or not GitHub can be reached, and staying
		// silent about it because a read failed left the card reporting
		// Connected over an agent that could do nothing.
		r.Findings = append(r.Findings, integration.Finding{
			Kind:      integration.FindingApprovalRequired,
			Subject:   seat.Handle,
			ActionURL: InstallURL(opts.WebBase, opts.Org, seat.Slug),
			Detail: seat.Handle + "'s app exists and nothing has installed it, so it " +
				"sees no repository: install it on " + orgLabel(opts.Org),
		})
		return
	}
	if adopted {
		r.Adopted = append(r.Adopted, seat.Handle)
	}

	if installation.Suspended() {
		r.Findings = append(r.Findings, integration.Finding{
			Kind:      integration.FindingApprovalRequired,
			Subject:   seat.Handle,
			ActionURL: InstallURL(opts.WebBase, opts.Org, seat.Slug),
			Detail: seat.Handle + "'s installation is suspended, so every token it " +
				"mints is refused: unsuspend it at GitHub",
		})
		return
	}

	// WHAT IT HOLDS, against what the tier asks for. The manifest asked
	// for one thing and a person approved another: a manifest can be
	// edited before it is submitted, and an installation widened after.
	if excess := Excess(installation.Permissions); len(excess) > 0 {
		r.Findings = append(r.Findings, integration.Finding{
			Kind:    integration.FindingGrantExcess,
			Subject: seat.Handle,
			Detail: seat.Handle + "'s app holds " + strings.Join(excess, ", ") +
				", which no tier asks for and which is how an agent would widen " +
				"its own access. Its tokens never carry these, and the grant is " +
				"still worth removing at GitHub",
		})
	}
	if short := Shortfall(seat.Tier, installation.Permissions); len(short) > 0 {
		r.Findings = append(r.Findings, integration.Finding{
			Kind:      integration.FindingGrantShort,
			Subject:   seat.Handle,
			ActionURL: InstallURL(opts.WebBase, opts.Org, seat.Slug),
			Detail: seat.Handle + " is set to " + seat.Tier.Label() + " and its app " +
				"lacks " + strings.Join(short, ", ") + ", so those calls are refused " +
				"at the call site with nothing naming the tier",
		})
		return
	}
	r.Ready = append(r.Ready, seat.Handle)
}

// installationFor finds the installation this seat's app is on.
//
// Three answers, and the middle one is why this is not a bool: the
// installation, nil for "there is none and that is the truth", and an error
// for "GitHub could not say".
func (r *SeatAppResult) installationFor(
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
					return nil, false, clearErr
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
				return nil, false, recordErr
			}
		}
		return &installation, true, nil
	}
	return nil, false, nil
}

// TokenFor mints the credential one seat acts with.
//
// SCOPED TO THE TIER at mint time, which is where the tier stops being a word
// in a document: the app may hold more than the tier asks for, and the token
// carries only what the tier lists.
func TokenFor(
	ctx context.Context, opts SeatAppOptions, seat SeatApp,
) (*InstallationToken, error) {
	if seat.AppID == 0 || seat.InstallationID == 0 {
		return nil, fmt.Errorf(
			"github: %s has no installed app, so there is nothing to mint against",
			seat.Handle)
	}
	build := opts.Client
	if build == nil {
		build = func(appID int64, pem, apiBase string) (*AppClient, error) {
			return NewAppClient(appID, pem, apiBase, nil)
		}
	}
	client, err := build(seat.AppID, seat.Key, opts.APIBase)
	if err != nil {
		return nil, err
	}
	return client.MintToken(ctx, seat.InstallationID, seat.Tier, seat.Repos)
}

// UninstallSeat removes one agent's installation, which is what actually
// revokes its access. Deleting the record and leaving the installation would
// leave an app able to act with nothing watching it.
func UninstallSeat(ctx context.Context, opts SeatAppOptions, seat SeatApp) error {
	if seat.AppID == 0 || seat.InstallationID == 0 {
		return nil
	}
	build := opts.Client
	if build == nil {
		build = func(appID int64, pem, apiBase string) (*AppClient, error) {
			return NewAppClient(appID, pem, apiBase, nil)
		}
	}
	client, err := build(seat.AppID, seat.Key, opts.APIBase)
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
