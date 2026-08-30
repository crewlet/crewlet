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
	// Validated names the seats whose manifest Slack accepted on a dry
	// run. Only a dry run fills it; a real run's create and update are
	// the validation.
	Validated []string
	// Pending names the seats that still need an operator's click, with
	// the URL to click.
	Pending map[string]string

	// Recorded counts the values this run wrote to the sink — a seat's
	// signing secret and its bot token are two.
	//
	// It exists so the report can say what still has to happen for those
	// values to reach a RUNNING engine — see [provision.TokenSink.NextStep].
	// Without it the report stopped at "recorded in the encrypted secret
	// store", which reads as finished while the engine goes on resolving
	// from the snapshot it built at its last apply.
	Recorded int

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
			log.ErrorContext(ctx, "slack_seat_provisioning_failed", "handle", seat.Handle,
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
	// THE LEDGER'S PAIR BEATS THE OPERATOR'S SHELL, which is the reverse
	// of the usual "an explicit input wins" rule and the reverse is the
	// point. Slack's rotation is single-use in BOTH directions: every
	// successful rotate invalidates the refresh token it was given, so the
	// value in a shell export is dead the moment this command first used
	// it. Preferring it would take the ledger's live pair — the only way
	// back into the operator's apps — and trade it for a token Slack has
	// already retired, on every run after the first, for ever.
	//
	// The flag and the variable are therefore a BOOTSTRAP: they seed a
	// ledger that holds nothing, and are ignored once it does.
	refresh := strings.TrimSpace(held.RefreshToken)
	if refresh == "" {
		refresh = strings.TrimSpace(o.ConfigRefreshToken)
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

// appGone reports a Slack error meaning the recorded app is not there any
// more — deleted in the console, or moved to a workspace this credential
// cannot see.
//
// # Why it is a set of codes rather than a status
//
// Every Web API answer is a 200 carrying `"ok": false`, so the only signal is
// the code string. These three are what the app-management methods answer for
// an id that does not resolve, and they are not interchangeable with a
// permission refusal: an app this operator may not touch still EXISTS, and
// replacing it would leave two.
func appGone(err error) bool {
	var api *APIError
	if !errors.As(err, &api) {
		return false
	}
	switch api.Code {
	case "app_not_found", "invalid_app_id", "invalid_app":
		return true
	}
	return false
}

// adoptable reports a hand-seeded ledger entry that cannot be installed, and
// says what is missing.
//
// ADOPTION IS A SUPPORTED PATH: an operator who created an app in the Slack
// console by hand pastes its four values into the ledger and this command
// takes over from there. What it must not do is proceed on a HALF-seeded
// entry — an app id alone produces an authorize URL with an empty client_id,
// which Slack answers with a page saying nothing useful, or an
// `invalid_client_id` from the exchange minutes later.
func adoptable(record AppRecord) []string {
	var missing []string
	if strings.TrimSpace(record.ClientID) == "" {
		missing = append(missing, "client_id")
	}
	if strings.TrimSpace(record.ClientSecret) == "" {
		missing = append(missing, "client_secret")
	}
	return missing
}

// seat brings one agent's app in line.
func (o Options) seat(ctx context.Context, configToken string, seat SeatPlan, res *Result) error {
	manifest := Manifest(seat.RoleName, seat.Handle, o.BaseURL)
	fingerprint := Fingerprint(manifest)
	record := o.Ledger.Apps[seat.Handle]

	// A LEDGER ENTRY THAT NAMES A DEAD APP IS WORSE THAN NO ENTRY: the
	// seat reads as provisioned, its recorded manifest hash matches, and
	// the run reports it kept — while the app it names was deleted in the
	// console months ago and the token in its ${VAR} authenticates as
	// nothing. Probing costs one validate call against an app id this run
	// already holds, and the recovery is to fall into the create branch.
	if record.AppID != "" {
		if err := o.Admin.ValidateManifest(ctx, configToken, manifest, record.AppID); err != nil {
			if !appGone(err) {
				return fmt.Errorf("slack: %s: %w", seat.Handle, err)
			}
			res.Notes = append(res.Notes, fmt.Sprintf(
				"%s: the recorded app %s no longer exists in Slack, so a "+
					"replacement was created — anything holding the old app's "+
					"bot token has to be restarted with the new one",
				seat.Handle, record.AppID))
			// THE HELD TOKEN IS A LEFTOVER. It belongs to the dead app
			// and it satisfies the sink's half of the "already
			// installed" check — but the replacement record carries no
			// bot user id, and that is the half that can only come from
			// a completed OAuth exchange. So the install runs, and the
			// stale value is overwritten rather than trusted.
			record = AppRecord{}
			delete(o.Ledger.Apps, seat.Handle)
		}
	}
	if record.AppID != "" {
		if missing := adoptable(record); len(missing) > 0 {
			return fmt.Errorf(
				"slack: %s: the ledger entry for app %s has no %s, so the "+
					"OAuth install cannot run — for an app adopted by hand, "+
					"copy both from its Basic Information page into %s",
				seat.Handle, record.AppID, strings.Join(missing, "/"),
				o.LedgerPath)
		}
	}

	switch {
	case record.AppID == "":
		// VALIDATED FIRST. apps.manifest.create is Tier 1 — roughly one
		// request a minute — so discovering a malformed manifest from
		// the create means a company of seven spends seven minutes
		// finding out seven times.
		if err := o.Admin.ValidateManifest(ctx, configToken, manifest, ""); err != nil {
			return fmt.Errorf("slack: %s: %w", seat.Handle, err)
		}
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
		res.Recorded++
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
	// THE CODE HAS TO BELONG TO THIS SEAT'S APP. One authorize URL is
	// printed per seat and they look alike; pasting the wrong one mints
	// another app's bot token into this seat's ${VAR}, and the seat then
	// posts as a colleague with nothing anywhere reporting it. The whole
	// point of an app per seat is that identities do not cross.
	if got := strings.TrimSpace(installed.AppID); got != "" && got != record.AppID {
		return fmt.Errorf(
			"slack: %s: that approve code belongs to app %s, not this seat's "+
				"%s — the click used another agent's authorize URL. Re-run "+
				"and use the URL printed for %s. Nothing was recorded",
			seat.Handle, got, record.AppID, seat.Handle)
	}
	if err := o.Sink.Record(ctx, seat.TokenVar, installed.BotToken); err != nil {
		return fmt.Errorf("slack: %s: record %s: %w", seat.Handle, seat.TokenVar, err)
	}
	res.Recorded++
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

// Validate checks every planned seat's manifest against Slack and changes
// nothing.
//
// # Why a dry run makes network calls at all
//
// Because the alternative is discovering a malformed manifest from
// apps.manifest.create, which is Tier 1 — about one request a minute — so a
// company of seven finds out seven times over seven minutes, having already
// created the apps for the seats before the bad one. `apps.manifest.validate`
// is the method Slack provides for exactly this, and it writes nothing.
//
// It still needs an app-configuration token, so a dry run with no ledger and
// no refresh token cannot validate. That is a NOTE rather than a failure: the
// plan is the more valuable half of a dry run, and an operator who has not
// yet made a config token is exactly the person reading the plan.
func Validate(ctx context.Context, opts Options) (*Result, error) {
	if opts.Admin == nil {
		return nil, errors.New("slack: no admin client")
	}
	if opts.Ledger == nil {
		return nil, errors.New("slack: no ledger")
	}
	res := &Result{Pending: map[string]string{}, Failed: map[string]string{}}
	if strings.TrimSpace(opts.BaseURL) == "" {
		res.Notes = append(res.Notes,
			"no -public-url, so no manifest could be checked: every app's "+
				"request URL and redirect URL are built from it, and a "+
				"manifest without one is not the manifest a real run sends")
		return res, nil
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	// THE TOKEN IS TAKEN THE SAME WAY A REAL RUN TAKES IT, rotation and
	// all: a dry run that could not reach the config token is a dry run
	// that has not tested the thing most likely to be wrong. The rotate
	// persists, which is the one write a dry run does make — and it must,
	// because Slack invalidated the refresh token the moment it answered.
	configToken, err := opts.configToken(ctx, now())
	if err != nil {
		res.Notes = append(res.Notes, fmt.Sprintf(
			"no manifest could be checked: %v", err))
		return res, nil
	}
	only := map[string]bool{}
	for _, handle := range opts.Only {
		only[handle] = true
	}
	for _, seat := range opts.Seats {
		if len(only) > 0 && !only[seat.Handle] {
			continue
		}
		if !seat.Provisionable() {
			continue
		}
		res.attempted++
		// AGAINST THE RECORDED APP ID where there is one, because Slack
		// validates an update differently from a create: a manifest that
		// is fine for a new app can still be refused as a change to an
		// existing one (a scope an installed app may not drop, say).
		manifest := Manifest(seat.RoleName, seat.Handle, opts.BaseURL)
		if err := opts.Admin.ValidateManifest(ctx, configToken, manifest,
			opts.Ledger.Apps[seat.Handle].AppID); err != nil {
			res.Failed[seat.Handle] = err.Error()
			continue
		}
		res.Validated = append(res.Validated, seat.Handle)
	}
	return res, res.Err()
}
