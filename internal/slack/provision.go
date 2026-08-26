package slack

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/provision"
)

// Provisioning: one Slack app per agent seat.
//
// # Every other vendor creates ACCOUNTS; this one creates APPS
//
// A Slack bot is not a user somebody administers into existence — it is an
// app, defined by a manifest, created by the operator's own
// app-configuration token, and then INSTALLED into the workspace by a human
// clicking Allow. That last step cannot be automated and must not be
// pretended away: OAuth exists precisely so that granting an app access to a
// workspace is a person's decision.
//
// So a run here does everything up to the click, hands the operator one URL
// per seat, and takes the code back. What it never does is claim to have
// installed something it did not.

// SeatPlan is one agent seat as provisioning sees it.
type SeatPlan struct {
	Handle string
	// RoleName becomes the app's display name in the workspace's app
	// directory, where the handle would read as a slug.
	RoleName string

	// TokenVar is the variable role.integrations.slack.bot_token points
	// at, and SigningVar the one signing_secret points at. Empty means a
	// literal, which cannot be minted into — reported and skipped, never
	// rewritten: editing the company config from a provisioning run is
	// not this command's job.
	TokenVar   string
	SigningVar string

	// Notes carry why a seat is only half provisionable.
	Notes []string
}

// Provisionable reports a seat this run can carry all the way through.
func (s SeatPlan) Provisionable() bool { return s.TokenVar != "" && s.SigningVar != "" }

// PlanFor walks the org for seats that declare a Slack app.
//
// A seat with a LITERAL credential is one an operator manages by hand. That
// is a supported choice — it just cannot be provisioned, because there is no
// variable to mint into. So it is reported and skipped, and the run
// continues for the seats that can be.
func PlanFor(o *org.Organization) []SeatPlan {
	if o == nil {
		return nil
	}
	var out []SeatPlan
	for role := range o.AllRoles() {
		if role.IsHuman() || role.Slack.IsZero() {
			continue
		}
		plan := SeatPlan{Handle: role.Handle(), RoleName: role.Name}
		plan.TokenVar, _ = provision.SoleVar(role.Slack.BotToken)
		plan.SigningVar, _ = provision.SoleVar(role.Slack.SigningSecret)
		if plan.TokenVar == "" {
			plan.Notes = append(plan.Notes,
				"integrations.slack.bot_token is not a whole ${VAR} reference, "+
					"so the install has nowhere to put the token Slack mints")
		}
		if plan.SigningVar == "" {
			plan.Notes = append(plan.Notes,
				"integrations.slack.signing_secret is not a whole ${VAR} "+
					"reference, so the secret Slack serves once at app creation "+
					"has nowhere to go — and it cannot be read back")
		}
		out = append(out, plan)
	}
	return out
}

// Installer asks the operator to approve one app and returns the code Slack
// gave them.
//
// A SEAM, because the click is genuinely a person's: the run cannot proceed
// without it, and a package that opened a browser would be untestable and
// wrong on a headless host. Returning an empty code with no error means the
// operator chose to skip this seat, which is an ordinary outcome — the app
// exists and can be installed later.
type Installer func(ctx context.Context, handle, authorizeURL string) (string, error)

// Options are one provisioning run's inputs.
type Options struct {
	Admin  *Admin
	Seats  []SeatPlan
	Ledger *Ledger

	// LedgerPath is where the ledger is written. The run saves it the
	// moment an app is created, before anything else — those credentials
	// are served once and cannot be read back.
	LedgerPath string

	// Sink records the bot token and the signing secret into the config's
	// own `${VAR}` references.
	Sink provision.TokenSink

	// BaseURL is the deployment's public HTTPS base. Every app's request
	// URL and redirect URL are built from it, so it is REQUIRED: an app
	// whose request URL points at the wrong host is one Slack reports as
	// healthy and that delivers nowhere.
	BaseURL string

	// ConfigRefreshToken is the operator's app-configuration refresh
	// token, used when the ledger holds no unexpired access token.
	ConfigRefreshToken string

	// Install asks for the OAuth code. Nil skips every install and
	// reports the authorize URLs instead, which is what -dry-run and a
	// non-interactive run both want.
	Install Installer

	// Reinstall forces the OAuth exchange even for a seat whose token the
	// sink already holds.
	//
	// What it is FOR is a scope change: a bot token carries only the
	// scopes it was minted with, so pushing a new manifest does not give
	// an existing token the new scope — the app has to be installed
	// again. It is destructive on its own, because the new install
	// revokes the token every running node is authenticating with.
	Reinstall bool

	// Only narrows the run to these handles. Empty runs every seat.
	//
	// Worth having because the manifest methods are rate limited to
	// roughly one request a minute: fixing one seat in a company of
	// twenty should not cost twenty minutes of waiting for nineteen
	// unchanged apps.
	Only []string

	Now func() time.Time
}

// Result is what one run did.
type Result struct {
	// Created names the seats whose apps this run created.
	Created []string
	// Updated names the seats whose manifests this run pushed.
	Updated []string
	// Installed names the seats whose workspace install this run
	// completed, minting a bot token.
	Installed []string
	// Kept names the seats this run left alone — the SUCCESSFUL outcome
	// of a re-run, which has to be visible or a quiet report reads as a
	// run that did nothing.
	Kept []string
	// Pending names the seats that still need an operator's click, with
	// the URL to click.
	Pending map[string]string

	// attempted is how many seats the run tried, for the failure summary.
	attempted int

	// Failed names the seats this run could not finish, and why.
	//
	// A SEAT AT A TIME, because aborting the run at the first failure is
	// far more expensive here than elsewhere: the manifest methods allow
	// about one request a minute, so a run that stops at seat three of
	// seven has spent minutes of rate-limit waiting and leaves the
	// operator to redo it. Everything completed is already durable — the
	// ledger is written after every mutation — so a re-run resumes.
	Failed map[string]string

	Notes []string
}

// Err reports the run's failures as one error, or nil.
//
// The COMMAND still exits non-zero: a run that provisioned five seats and
// could not provision two is not a success, and reporting it as one is how
// an operator discovers the gap when an agent never answers.
func (r *Result) Err() error {
	if len(r.Failed) == 0 {
		return nil
	}
	handles := make([]string, 0, len(r.Failed))
	for handle := range r.Failed {
		handles = append(handles, handle)
	}
	sort.Strings(handles)
	var b strings.Builder
	fmt.Fprintf(&b, "slack: %d of %d seat(s) could not be provisioned:", len(handles), r.attempted)
	for _, handle := range handles {
		fmt.Fprintf(&b, "\n  %s: %s", handle, r.Failed[handle])
	}
	b.WriteString("\n\nEverything that completed is recorded — re-run to resume.")
	return errors.New(b.String())
}

// Reconcile runs one pass.
func Reconcile(ctx context.Context, opts Options) (*Result, error) {
	if opts.Admin == nil {
		return nil, errors.New("slack: no admin client")
	}
	if opts.Sink == nil {
		return nil, provision.ErrNoSink
	}
	if opts.Ledger == nil || opts.LedgerPath == "" {
		return nil, errors.New("slack: no ledger — Slack serves an app's " +
			"credentials once, so a run with nowhere to record them would " +
			"create apps nobody can install")
	}
	if strings.TrimSpace(opts.BaseURL) == "" {
		return nil, errors.New("slack: no public base URL — every app's " +
			"request URL and redirect URL are built from it, so an app made " +
			"without one delivers nowhere and cannot be installed")
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}

	configToken, err := opts.configToken(ctx, now())
	if err != nil {
		return nil, err
	}

	res := &Result{Pending: map[string]string{}, Failed: map[string]string{}}
	only := map[string]bool{}
	for _, handle := range opts.Only {
		only[handle] = true
	}
	for _, seat := range opts.Seats {
		if len(only) > 0 && !only[seat.Handle] {
			continue
		}
		res.Notes = append(res.Notes, seatNotes(seat)...)
		if !seat.Provisionable() {
			continue
		}
		res.attempted++
		if err := opts.seat(ctx, configToken, seat, res); err != nil {
			// ONE SEAT AT A TIME. A mistyped code paste or an invalid
			// manifest must not cost the seats that would have worked
			// — see [Result.Failed].
			log.Error("slack_seat_provisioning_failed", "handle", seat.Handle,
				"error", err.Error())
			res.Failed[seat.Handle] = err.Error()
		}
	}
	if err := opts.Sink.Flush(ctx); err != nil {
		return res, fmt.Errorf("slack: %w", err)
	}
	return res, res.Err()
}

// configToken is the app-configuration token this run manages apps with.
//
// REUSED WHILE IT IS STILL GOOD, because Slack's rotation is single-use in
// both directions: each rotate invalidates the refresh token it was given,
// so rotating on every run turns a lost write into an operator locked out of
// their own apps. A token within a minute of expiry is treated as expired —
// a run that started with fifty seconds left would fail half way through.
func (o Options) configToken(ctx context.Context, at time.Time) (string, error) {
	held := o.Ledger.ConfigToken
	if held.Token != "" && (held.ExpiresAt.IsZero() || held.ExpiresAt.After(at.Add(time.Minute))) {
		return held.Token, nil
	}
	refresh := strings.TrimSpace(o.ConfigRefreshToken)
	if refresh == "" {
		refresh = held.RefreshToken
	}
	if refresh == "" {
		return "", errors.New(
			"slack: no app-configuration token: create one at " +
				"https://api.slack.com/reference/manifests#config-tokens and " +
				"pass its refresh token, which this run exchanges for a " +
				"12-hour access token")
	}
	rotated, err := o.Admin.RotateConfigToken(ctx, refresh)
	if err != nil {
		return "", err
	}
	// PERSISTED BEFORE USE. The rotate call already invalidated the
	// refresh token it was given, so this pair is now the only way back
	// into the operator's apps — losing it here would cost them every app
	// this ledger describes.
	o.Ledger.ConfigToken.Token = rotated.Token
	o.Ledger.ConfigToken.RefreshToken = rotated.RefreshToken
	o.Ledger.ConfigToken.ExpiresAt = rotated.ExpiresAt
	if err := o.Ledger.Save(o.LedgerPath); err != nil {
		return "", fmt.Errorf(
			"slack: the app-configuration token was rotated and could not be "+
				"recorded, which invalidates the refresh token you supplied: %w", err)
	}
	return rotated.Token, nil
}

// seat brings one agent's app in line.
func (o Options) seat(ctx context.Context, configToken string, seat SeatPlan, res *Result) error {
	manifest := Manifest(seat.RoleName, seat.Handle, o.BaseURL)
	fingerprint := Fingerprint(manifest)
	record := o.Ledger.Apps[seat.Handle]

	switch {
	case record.AppID == "":
		created, err := o.Admin.CreateApp(ctx, configToken, manifest)
		if err != nil {
			return fmt.Errorf("slack: %s: %w", seat.Handle, err)
		}
		record = AppRecord{
			AppID: created.AppID, ClientID: created.ClientID,
			ClientSecret: created.ClientSecret, SigningSecret: created.SigningSecret,
			ManifestHash: fingerprint,
		}
		o.Ledger.Apps[seat.Handle] = record
		// SAVED IMMEDIATELY, before the secret reaches the sink and
		// before the install is attempted. Slack serves these four
		// values once; a process that died in the next instruction
		// would leave an app nobody can install, verify or delete.
		if err := o.Ledger.Save(o.LedgerPath); err != nil {
			return fmt.Errorf(
				"slack: %s: the app was created and its credentials could not "+
					"be recorded — Slack serves them once, so this app has to "+
					"be deleted by hand at https://api.slack.com/apps/%s: %w",
				seat.Handle, created.AppID, err)
		}
		res.Created = append(res.Created, seat.Handle)

	case record.ManifestHash != fingerprint:
		if err := o.Admin.UpdateApp(ctx, configToken, record.AppID, manifest); err != nil {
			return fmt.Errorf("slack: %s: %w", seat.Handle, err)
		}
		record.ManifestHash = fingerprint
		o.Ledger.Apps[seat.Handle] = record
		if err := o.Ledger.Save(o.LedgerPath); err != nil {
			return fmt.Errorf("slack: %s: %w", seat.Handle, err)
		}
		res.Updated = append(res.Updated, seat.Handle)

	default:
		res.Kept = append(res.Kept, seat.Handle)
	}

	if record.SigningSecret != "" {
		if err := o.Sink.Record(ctx, seat.SigningVar, record.SigningSecret); err != nil {
			return fmt.Errorf("slack: %s: record %s: %w", seat.Handle, seat.SigningVar, err)
		}
	}
	return o.install(ctx, seat, record, res)
}

// install completes the workspace install, or reports what is left to click.
func (o Options) install(ctx context.Context, seat SeatPlan, record AppRecord, res *Result) error {
	if !o.Reinstall {
		held, ok, err := o.Sink.Value(ctx, seat.TokenVar)
		if err != nil {
			// UNREADABLE IS NOT UNHELD. Treating it as "no token" would
			// send the operator through an install they did not need,
			// and a second install revokes the first token — taking a
			// working seat down to fix nothing.
			return fmt.Errorf("slack: %s: could not read %s: %w",
				seat.Handle, seat.TokenVar, err)
		}
		if ok && held != "" && record.Installed() {
			return nil
		}
	}
	authorize := AuthorizeURL(record.ClientID, o.BaseURL, seat.Handle)
	if o.Install == nil {
		res.Pending[seat.Handle] = authorize
		return nil
	}
	code, err := o.Install(ctx, seat.Handle, authorize)
	if err != nil {
		return fmt.Errorf("slack: %s: %w", seat.Handle, err)
	}
	if strings.TrimSpace(code) == "" {
		// The operator skipped this seat. The app exists and can be
		// installed on the next run, so this is a note rather than a
		// failure.
		res.Pending[seat.Handle] = authorize
		return nil
	}
	installed, err := o.Admin.Exchange(ctx, record.ClientID, record.ClientSecret,
		strings.TrimSpace(code), o.BaseURL)
	if err != nil {
		return fmt.Errorf("slack: %s: %w", seat.Handle, err)
	}
	if err := o.Sink.Record(ctx, seat.TokenVar, installed.BotToken); err != nil {
		return fmt.Errorf("slack: %s: record %s: %w", seat.Handle, seat.TokenVar, err)
	}
	record.BotUserID = installed.BotUserID
	record.TeamID = installed.TeamID
	o.Ledger.Apps[seat.Handle] = record
	if err := o.Ledger.Save(o.LedgerPath); err != nil {
		return fmt.Errorf("slack: %s: %w", seat.Handle, err)
	}
	res.Installed = append(res.Installed, seat.Handle)
	return nil
}

// seatNotes renders a seat's plan notes with its handle attached.
func seatNotes(seat SeatPlan) []string {
	out := make([]string, 0, len(seat.Notes))
	for _, note := range seat.Notes {
		out = append(out, seat.Handle+": "+note)
	}
	return out
}
