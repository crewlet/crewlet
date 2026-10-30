package github

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/words"
)

// GitHub's half of per-seat access ([seataccess.Vendor]).
//
// # The account is the app and its installation
//
// Each agent has its own GitHub App, so the "account" the ledger records is
// the app (its id), and it can act only through an installation. A seat is
// created by two people's clicks (the manifest and the install), switched off
// and on by suspending its installation as the app, narrowed at every token
// mint, and widened only when the app's owner and then the organization
// accept the new permissions (D5).
//
// # Read-only on the document
//
// The installation id the token issuer mints against and the app record are
// written by the GitHub pass ([AdoptSeatApps]), which adopts an installation a
// person made and clears a record GitHub no longer has. This session reads and acts at GitHub and leaves the document
// to that pass, so there is one writer of those fields.

func init() { seataccess.Register(vendor{}) }

type vendor struct{}

// Kind implements [seataccess.Vendor].
func (vendor) Kind() integration.Kind { return integration.KindGitHub }

// Catalog implements [seataccess.Vendor].
func (vendor) Catalog() seataccess.Catalog {
	return seataccess.Catalog{
		Kind:        integration.KindGitHub,
		Credentials: []string{FieldAppKey, FieldAppWebhookSecret},
		Lanes: []seataccess.LaneSpec{{
			Lane:        seataccess.LaneGitHub,
			Permissions: PermissionSpecs(),
			Scoped:      true,
			CreatedBy:   seataccess.ByYou,
			// UNSUSPENDED AS THE APP, so back on needs nobody (D1).
			EnabledBy: seataccess.ByCrewlet,
			// Tokens last an hour and are never stored, so there is
			// nothing to rotate.
			Rotatable: false,
		}},
	}
}

// PermissionSpecs is the catalogue as the access contract carries it.
func PermissionSpecs() []seataccess.PermissionSpec {
	out := make([]seataccess.PermissionSpec, 0, len(SeatPermissions))
	for _, perm := range SeatPermissions {
		out = append(out, seataccess.PermissionSpec{
			Key: perm.Key, Name: perm.Name,
			WriteOnly:          !perm.Offers(LevelRead),
			AdministersOnWrite: slices.Contains(AdministrationClass, perm.Key),
		})
	}
	return out
}

// Steps implements [seataccess.Vendor]. Pure: no call to GitHub.
//
// A STEP IS A SHORT LABEL, drawn under its agent and beside the change it
// makes, so it names neither the agent nor the permissions the change line
// already shows: "Update tokens", not the seat's whole permission set.
func (vendor) Steps(
	_ seataccess.SeatRef, _ seataccess.Lane, op seataccess.Op,
	want seataccess.Desired, have seataccess.Observed,
) []seataccess.Step {
	var steps []seataccess.Step
	add := func(step seataccess.Step) {
		step.Op, step.Index = op, len(steps)
		steps = append(steps, step)
	}
	switch op {
	case seataccess.OpCreate:
		add(seataccess.Step{
			Verb: seataccess.VerbCreate, By: seataccess.ByYou,
			Text: "Create its GitHub App",
			Note: "GitHub has no API for this, so a person submits the prepared manifest.",
		})
		add(seataccess.Step{
			Verb: seataccess.VerbApprove, By: seataccess.ByYou,
			Text: "Install the app on the organization",
			Note: "The repositories it covers are chosen during the install.",
		})
	case seataccess.OpEnable:
		// ON AT THE DOCUMENT'S SETTING, as a create is: the plan adds no
		// level or scope after an enable, so whatever the installation
		// must still grant or cover is waited on here.
		if have.DisabledBy == seataccess.DisabledByHand {
			// A PERSON SUSPENDED IT AT GITHUB, so it is lifted there:
			// that decision was made at GitHub, and this engine lifts only
			// a suspension it placed itself.
			add(seataccess.Step{
				Verb: seataccess.VerbEnable, By: seataccess.ByYou,
				Text: "Unsuspend the installation at GitHub",
				Note: "A person suspended it there, so it is lifted there.",
			})
		} else {
			add(seataccess.Step{
				Verb: seataccess.VerbEnable, By: seataccess.ByCrewlet,
				Text: "Turn the installation back on",
				Note: "An app that was never installed waits for a person to install it.",
			})
		}
		raiseSteps(want, have, add)
		scopeWait(want, have, add)
	case seataccess.OpLevel:
		add(seataccess.Step{
			Verb: seataccess.VerbRerole, By: seataccess.ByCrewlet,
			Text: "Update tokens",
			Note: tokensOnly,
		})
		raiseSteps(want, have, add)
	case seataccess.OpScope:
		add(seataccess.Step{
			Verb: seataccess.VerbScope, By: seataccess.ByCrewlet,
			Text: "Limit tokens to " + scopeText(want.Scope),
			Note: tokensOnly,
		})
		scopeWait(want, have, add)
	case seataccess.OpMint:
		if have.Credential != nil {
			// SEALED WHEN THE APP WAS CREATED, outside any sync: there
			// is nothing to mint, and tokens are issued at use.
			break
		}
		add(seataccess.Step{
			Verb: seataccess.VerbCreate, By: seataccess.ByYou,
			Text: "Delete the app at GitHub and create it again",
			Note: "Its key was never sealed, and GitHub issues a key only once.",
		})
	case seataccess.OpDisable:
		add(seataccess.Step{
			Verb: seataccess.VerbDisable, By: seataccess.ByCrewlet,
			Text: "Suspend the installation",
			Note: "The app and its key are kept, so turning it back on needs no one.",
		})
	case seataccess.OpRevoke:
		// NOTHING TO REVOKE. Off suspends the installation, and the app's
		// key is what turns it back on: GitHub issues a key once, so
		// clearing it would make off a removal. Tokens are issued at use
		// and refused while the installation is suspended.
	case seataccess.OpRemove:
		add(seataccess.Step{
			Verb: seataccess.VerbRemove, By: seataccess.ByCrewlet,
			Text: "Uninstall the app",
		})
		add(seataccess.Step{
			Verb: seataccess.VerbRemove, By: seataccess.ByYou,
			Text: "Delete the app at GitHub",
			Note: "GitHub has no API for this. Its sealed key is cleared once GitHub reports it gone.",
		})
	}
	return steps
}

// tokensOnly is the note under a step that changes only what this engine asks
// GitHub for when it mints a seat's token.
const tokensOnly = "Applies from the next token. Nothing changes at GitHub."

// raiseSteps draws what a raise waits for at GitHub: the app's owner where
// the registration lacks a permission, then the organization accepting it on
// the installation, in that order (D5). A narrowing draws nothing: every token
// is minted at the seat's permissions.
//
// THE ACCEPTANCE SAYS "THEM" where it accepts exactly what the step before it
// added, which is what a reader just read; otherwise it names its own.
func raiseSteps(want seataccess.Desired, have seataccess.Observed, add func(seataccess.Step)) {
	wanted := Permissions(want.Permissions)
	raised := Permissions(have.Permissions).Raises(wanted)
	if len(raised) == 0 {
		return
	}
	accept := "Accept " + Names(raised) + " on the installation"
	if beyond := wanted.Short(Permissions(have.Ceiling).Token()); len(beyond) > 0 {
		// GRANT, NOT APPROVE: the app's owner widens the registration,
		// which Apply tells from the organization's acceptance by verb.
		add(seataccess.Step{
			Verb: seataccess.VerbGrant, By: seataccess.ByYou,
			Text: "Add " + Names(beyond) + " to the app",
			Note: "Only the app's owner can widen it.",
		})
		if slices.Equal(beyond, raised) {
			accept = "Accept them on the installation"
		}
	}
	add(seataccess.Step{
		Verb: seataccess.VerbApprove, By: seataccess.ByYou,
		Text: accept,
		Note: "An organization owner accepts an app's new permissions.",
	})
}

// scopeWait draws the repositories a person must add to the installation
// before a token naming them can be minted.
func scopeWait(want seataccess.Desired, have seataccess.Observed, add func(seataccess.Step)) {
	if missing := uncovered(want.Scope, have.Scope); len(missing) > 0 {
		add(seataccess.Step{
			Verb: seataccess.VerbScope, By: seataccess.ByYou,
			Text: "Add " + repoList(missing) + " to the installation",
			Note: "GitHub refuses a token for a repository the installation does not cover.",
		})
	}
}

// Open implements [seataccess.Vendor].
//
// NOT CONFIGURED UNLESS THE ENGINE PROVISIONS GITHUB (D4b): agents' apps made
// and handed in from outside Crewlet are bound to their tools and never
// suspended, uninstalled or adopted by a sync. Whether only an operator writes
// the document, which makes every app external whatever the block says, is
// the engine's to decide before it opens a session; this is the half the
// document alone answers.
func (vendor) Open(_ context.Context, in seataccess.OpenInput) (seataccess.Session, error) {
	if in.Config == nil || in.Config.Integrations.GitHub == nil ||
		in.Config.SeatCredentialMode(string(integration.KindGitHub), false) != config.SeatCredentialEngine {
		return nil, fmt.Errorf("github: %w", integration.ErrNotConfigured)
	}
	cfg := in.Config.Integrations.GitHub
	var lookup func(string) (string, bool)
	if in.Env != nil {
		lookup = in.Env.LookupOK
	}
	apiBase, webBase := cfg.Bases(lookup)
	s := &session{
		opts: SeatAppOptions{
			APIBase: apiBase, WebBase: webBase, Org: providedOrg(cfg),
		},
		env:    in.Env,
		seats:  map[string]seatDoc{},
		cached: map[string]SeatObservation{},
	}
	if cfg.Provisioning != nil {
		s.repos = slices.Clone(cfg.Provisioning.Repos)
	}
	for role := range in.Config.EachRole() {
		seat := role.Seat()
		if seat == nil || !seat.IsAgent() || role.Integrations.GitHub == nil {
			continue
		}
		s.seats[seat.Handle()] = seatDoc{name: seat.Name, block: role.Integrations.GitHub}
	}
	return s, nil
}

// providedOrg is the organization the seats' apps are installed on.
func providedOrg(cfg *config.GitHub) string {
	if cfg == nil || cfg.Provisioning == nil {
		return ""
	}
	return strings.TrimSpace(cfg.Provisioning.Org)
}

// session is one pass's connection to GitHub.
type session struct {
	opts  SeatAppOptions
	env   *config.Resolver
	repos []string

	// seats is each agent seat's block, by handle.
	seats map[string]seatDoc

	// cached is what Observe read, by handle, for Apply to act on without
	// reading GitHub twice.
	cached map[string]SeatObservation
}

// seatDoc is one seat's GitHub block as the document holds it.
type seatDoc struct {
	name  string
	block *config.RoleGitHub
}

// Options implements [seataccess.Session]: GitHub's permission set, and the
// repositories the company block lists, which a seat's scope must be within.
// A company that lists none reaches whatever its installations cover.
func (s *session) Options(context.Context, seataccess.Lane) (seataccess.Options, error) {
	return seataccess.Options{
		Lane:        seataccess.LaneGitHub,
		Permissions: PermissionSpecs(),
		Scope:       slices.Clone(s.repos),
		ScopeOpen:   len(s.repos) == 0,
	}, nil
}

// Observe implements [seataccess.Session].
//
// ONE AGENT'S APP, ONE AGENT'S FAULT. Every seat authenticates with its own
// app's key, so a read GitHub refused for one seat says nothing about the
// others: it is reported as that seat's ([seataccess.SeatFaults]) and the rest
// are synced. A pass whose context is done is the kind's fault.
func (s *session) Observe(ctx context.Context, in seataccess.ObserveInput) (seataccess.Observation, error) {
	out := seataccess.Observation{}
	faults := seataccess.SeatFaults{}
	for _, handle := range in.Handles() {
		record := in.Record(handle)
		seat := s.seatApp(handle, record)
		obs, err := ObserveSeat(ctx, s.opts, seat)
		if err != nil {
			if stop := interrupted(ctx); stop != nil {
				return nil, stop
			}
			faults[handle] = fmt.Errorf("github: read %s's app back: %w", handle, err)
			continue
		}
		s.cached[handle] = obs
		out[handle] = map[seataccess.Lane]seataccess.Observed{
			seataccess.LaneGitHub: s.observed(seat, s.want(handle), obs, record),
		}
	}
	if len(faults) > 0 {
		return out, faults
	}
	return out, nil
}

// observed is one seat's observation in the contract's terms.
//
// PERMISSIONS ARE WHAT THE SEAT'S TOKENS CARRY: per permission, the lower of
// the seat's setting and the installation's grant. An installation wider
// than the seat is not drift (every token is narrowed at mint), and one
// narrower is exactly the shortfall a raise waits on. CEILING is the
// registration, which a raise past needs the app's owner for (D5).
func (s *session) observed(
	seat SeatApp, want seataccess.Desired, obs SeatObservation, record *seataccess.Record,
) seataccess.Observed {
	out := seataccess.Observed{Account: seat.Slug}
	if seat.AppID != 0 {
		out.AccountID = strconv.FormatInt(seat.AppID, 10)
	}
	if record != nil && record.Account != nil {
		out.CreatedAt = record.Account.CreatedAt
	}
	// THE KEY IS SEALED BY THE APP'S CREATION, not by a sync, so it is
	// reported where it resolves: the ledger's record of it is kept where
	// it has one, for when it was sealed.
	if strings.TrimSpace(seat.Key) != "" {
		out.Credential = &seataccess.Credential{
			Variable: SeatVar(seat.Handle, FieldAppKey),
			Others:   []string{SeatVar(seat.Handle, FieldAppWebhookSecret)},
		}
		if record != nil && record.Credential != nil {
			out.Credential = record.Credential
		}
	}
	if !obs.AppExists {
		// NO APP, or GitHub reports it deleted: the plan draws the create.
		return out
	}
	// THE APP EXISTS, so the account does, installed or not. Reported as
	// gone, an app somebody merely uninstalled would have its sealed key
	// cleared with the record, and GitHub issues that key once.
	out.Exists, out.Ceiling = true, catalogueOnly(obs.Registered)
	installation := obs.Installation
	if installation == nil {
		// NOT INSTALLED: off, and whose doing the ledger says (the sync reads
		// it there); back on is an install a person makes.
		return out
	}
	out.Enabled = !installation.Suspended()
	if !out.Enabled {
		// THE LEDGER SAYS WHETHER THIS ENGINE SUSPENDED IT, unless a person
		// has suspended it at GitHub since: that suspension is theirs, and
		// it stays until a person chooses Turn back on (D3).
		out.DisabledBy = seataccess.DisabledByHand
		if !installation.SuspendedByPerson() && !liftWaiting(record) && record != nil &&
			record.Account != nil && record.Account.State == seataccess.AccountDisabled {
			out.DisabledBy = seataccess.DisabledByCrewlet
		}
	}
	out.Permissions = effective(Permissions(want.Permissions), installation.Permissions)
	out.Scope = covered(seat.Repos, obs)
	return out
}

// effective is, per permission the seat asks for, the lower of the ask and
// what held grants.
func effective(want Permissions, held map[string]string) seataccess.Permissions {
	out := seataccess.Permissions{}
	for key, level := range want {
		got := Level(strings.TrimSpace(held[key]))
		if got.Rank() < level.Rank() {
			level = got
		}
		if level != LevelNone {
			out[key] = level
		}
	}
	return out
}

// catalogueOnly is held narrowed to the catalogue's keys.
func catalogueOnly(held map[string]string) seataccess.Permissions {
	out := seataccess.Permissions{}
	for _, perm := range SeatPermissions {
		if level := Level(strings.TrimSpace(held[perm.Key])); level != LevelNone {
			out[perm.Key] = level
		}
	}
	return out
}

// covered is the seat's repositories its installation covers, in the seat's
// spelling, so a seat whose every repository is covered observes exactly its
// own scope.
func covered(repos []string, obs SeatObservation) []string {
	missing := obs.UncoveredRepositories(SeatApp{Repos: repos})
	var out []string
	for _, repo := range repos {
		if name := strings.TrimSpace(repo); name != "" && !slices.Contains(missing, name) {
			out = append(out, name)
		}
	}
	return out
}

// Apply implements [seataccess.Session].
func (s *session) Apply(ctx context.Context, in seataccess.ApplyInput) (seataccess.StepResult, error) {
	handle := in.Seat.Handle
	seat := s.seatApp(handle, in.Record)
	switch in.Step.Op {
	case seataccess.OpCreate:
		return s.applyCreate(ctx, in, seat)
	case seataccess.OpEnable, seataccess.OpLevel, seataccess.OpScope:
		if in.Step.By == seataccess.ByYou {
			return s.awaitPerson(ctx, in, seat)
		}
		if in.Step.Op != seataccess.OpEnable {
			// THE TOKEN IS THE CHANGE: the next mint carries it.
			return seataccess.StepResult{}, nil
		}
		// READ AGAIN: back on is decided by the installation as it is now,
		// and this pass may have suspended it since it was observed.
		obs, err := s.observe(ctx, seat, true)
		if err != nil {
			return seataccess.StepResult{}, err
		}
		res := s.adopted(seat, in.Record)
		if obs.Installation == nil {
			res.Waiting = s.installWait(seat)
			return res, nil
		}
		if !obs.Installation.Suspended() {
			return res, nil
		}
		if obs.Installation.SuspendedByPerson() {
			// SUSPENDED AT GITHUB BY A PERSON since this pass planned:
			// theirs to lift, on the installation's page.
			res.Waiting = s.personSuspended(seat, obs)
			return res, nil
		}
		seat.InstallationID = obs.Installation.ID
		if err := UnsuspendSeat(ctx, s.opts, seat); err != nil {
			if liftRefused(err) {
				// NOT OURS TO LIFT AFTER ALL: a person's step, never a
				// fault retried every pass. The next pass reads this wait
				// from the ledger and asks GitHub nothing ([liftWaiting]).
				res.Waiting = s.personSuspended(seat, obs)
				return res, nil
			}
			return seataccess.StepResult{}, s.refusal(err, seat, seataccess.VerbEnable)
		}
		// READ AGAIN, NOW THAT IT IS ON: a suspended installation's
		// repositories could not be read, and the steps after this one
		// act on what it covers.
		if _, err := s.observe(ctx, seat, true); err != nil {
			return seataccess.StepResult{}, fmt.Errorf(
				"github: read %s's installation back after turning it on: %w", seat.Handle, err)
		}
		return res, nil
	case seataccess.OpMint:
		if strings.TrimSpace(seat.Key) != "" {
			return s.adopted(seat, in.Record), nil
		}
		return seataccess.StepResult{Waiting: &seataccess.Waiting{
			ActionURL: ManageURL(s.opts.WebBase, s.opts.Org, seat.Slug),
			Detail: "Delete " + seat.Label() + "'s app at GitHub, then create it again: " +
				"GitHub issues its key only once",
		}}, nil
	case seataccess.OpDisable:
		obs, err := s.observe(ctx, seat, false)
		if err != nil {
			return seataccess.StepResult{}, err
		}
		if obs.Installation == nil {
			// NOTHING INSTALLED IS NOTHING TO SWITCH OFF: no token can
			// be minted, which is what off means.
			return seataccess.StepResult{Disabled: true}, nil
		}
		seat.InstallationID = obs.Installation.ID
		if err := SuspendSeat(ctx, s.opts, seat); err != nil {
			return seataccess.StepResult{}, s.refusal(err, seat, seataccess.VerbDisable)
		}
		return seataccess.StepResult{Disabled: true}, nil
	case seataccess.OpRemove:
		return s.applyRemove(ctx, in, seat)
	}
	return seataccess.StepResult{}, fmt.Errorf("github: no step for %q", in.Step.Op)
}

// applyCreate checks the two clicks a create is made of: the app exists (the
// manifest was submitted and the key sealed), and it is installed.
func (s *session) applyCreate(ctx context.Context, in seataccess.ApplyInput, seat SeatApp) (seataccess.StepResult, error) {
	if seat.AppID == 0 || strings.TrimSpace(seat.Key) == "" {
		return seataccess.StepResult{Waiting: &seataccess.Waiting{
			Detail: "Create " + in.Seat.Label() + "'s GitHub App",
		}}, nil
	}
	// THE INSTALL IS A PERSON'S CLICK, so its check re-reads GitHub.
	obs, err := s.observe(ctx, seat, in.Step.Index > 0)
	if err != nil {
		return seataccess.StepResult{}, err
	}
	if !obs.AppExists {
		return seataccess.StepResult{Gone: true}, nil
	}
	created := seataccess.StepResult{
		AccountID: strconv.FormatInt(seat.AppID, 10), Account: seat.Slug,
	}
	if in.Step.Index == 0 || obs.Installation != nil {
		return created, nil
	}
	created.Waiting = s.installWait(seat)
	return created, nil
}

// awaitPerson checks a step only a person at GitHub can take, reading GitHub
// again, so "Done, check again" is a read: a raise waits on the app's owner
// (grant) and then the organization (approve), and a scope on the
// repositories the installation must cover.
func (s *session) awaitPerson(ctx context.Context, in seataccess.ApplyInput, seat SeatApp) (seataccess.StepResult, error) {
	seat.Repos = in.Want.Scope
	obs, err := s.observe(ctx, seat, true)
	if err != nil {
		return seataccess.StepResult{}, err
	}
	if in.Step.Verb == seataccess.VerbEnable {
		// A PERSON'S UNSUSPEND: done once GitHub shows it on.
		switch {
		case obs.Installation == nil:
			return seataccess.StepResult{Waiting: s.installWait(seat)}, nil
		case obs.Installation.Suspended():
			return seataccess.StepResult{Waiting: s.personSuspended(seat, obs)}, nil
		}
		return s.adopted(seat, in.Record), nil
	}
	if in.Step.Verb == seataccess.VerbScope {
		if missing := obs.UncoveredRepositories(seat); len(missing) > 0 {
			return seataccess.StepResult{Waiting: &seataccess.Waiting{
				ActionURL: s.installationURL(seat, obs),
				Detail: "Add " + repoList(missing) + " to " + seat.Label() +
					"'s installation, or narrow the agent to repositories it covers",
			}}, nil
		}
		return seataccess.StepResult{}, nil
	}
	wait := RaiseWait(s.opts.WebBase, s.opts.Org, seat, Permissions(in.Want.Permissions), obs)
	// THE OWNER'S STEP IS DONE once the registration holds the ask, even
	// while the organization's acceptance is still outstanding.
	if wait == nil || (!wait.Owner && in.Step.Verb == seataccess.VerbGrant) {
		return seataccess.StepResult{}, nil
	}
	return seataccess.StepResult{Waiting: &seataccess.Waiting{ActionURL: wait.URL, Detail: wait.Detail}}, nil
}

// applyRemove uninstalls, then waits for a person to delete the
// registration, and hands the sealed key back for clearing once GitHub
// reports it deleted.
//
// THE KEY OUTLIVES THE UNINSTALL until then, because it is the only way to
// ask GitHub whether the registration is gone: cleared first, the wait could
// never be confirmed, and the tab would ask a person to delete an app nobody
// could check on. An uninstalled app's key mints nothing.
func (s *session) applyRemove(ctx context.Context, in seataccess.ApplyInput, seat SeatApp) (seataccess.StepResult, error) {
	gone := seataccess.StepResult{Gone: true, Revoked: []string{FieldAppKey, FieldAppWebhookSecret}}
	if seat.AppID == 0 || strings.TrimSpace(seat.Key) == "" {
		return gone, nil
	}
	obs, err := s.observe(ctx, seat, in.Step.Index > 0)
	if err != nil {
		return seataccess.StepResult{}, err
	}
	if !obs.AppExists {
		return gone, nil
	}
	if in.Step.Index == 0 {
		if obs.Installation != nil {
			seat.InstallationID = obs.Installation.ID
			if err := UninstallSeat(ctx, s.opts, seat); err != nil {
				return seataccess.StepResult{}, s.refusal(err, seat, seataccess.VerbRemove)
			}
		}
		return seataccess.StepResult{}, nil
	}
	return seataccess.StepResult{Waiting: &seataccess.Waiting{
		ActionURL: ManageURL(s.opts.WebBase, s.opts.Org, seat.Slug),
		Detail:    "Delete " + in.Seat.Label() + "'s app registration at GitHub",
	}}, nil
}

// adopted is a step result that records the seat's app in the ledger where
// it has no account yet: the app was created by a person's manifest, outside
// any sync.
func (s *session) adopted(seat SeatApp, record *seataccess.Record) seataccess.StepResult {
	if seat.AppID == 0 || (record != nil && record.Account != nil && record.Account.State != seataccess.AccountClosed) {
		return seataccess.StepResult{}
	}
	return seataccess.StepResult{AccountID: strconv.FormatInt(seat.AppID, 10), Account: seat.Slug}
}

// observe answers the cached observation, or reads GitHub when there is none
// or fresh is asked for.
func (s *session) observe(ctx context.Context, seat SeatApp, fresh bool) (SeatObservation, error) {
	if obs, ok := s.cached[seat.Handle]; ok && !fresh {
		return obs, nil
	}
	obs, err := ObserveSeat(ctx, s.opts, seat)
	if err != nil {
		return obs, err
	}
	s.cached[seat.Handle] = obs
	return obs, nil
}

// installWait is the install a person still owes.
func (s *session) installWait(seat SeatApp) *seataccess.Waiting {
	return &seataccess.Waiting{
		ActionURL: InstallURL(s.opts.WebBase, s.opts.Org, seat.Slug),
		Detail:    "Install " + seat.Label() + "'s app on " + orgLabel(s.opts.Org),
	}
}

// liftRefused reports GitHub refusing this engine's unsuspend: a 403 or a 422,
// which is GitHub saying the suspension is not this app's to lift.
func liftRefused(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && (apiErr.Status == 403 || apiErr.Status == 422)
}

// liftWaiting reports a seat whose last sync stopped at turning it back on,
// waiting for a person to unsuspend it at GitHub. Its suspension is then a
// person's to lift whoever placed it, so no pass asks GitHub to lift it again.
func liftWaiting(record *seataccess.Record) bool {
	if record == nil {
		return false
	}
	sync := record.Lanes[seataccess.LaneGitHub].Sync
	return sync != nil && sync.Outcome == seataccess.OutcomeWaiting &&
		sync.Error != nil && sync.Error.Step == seataccess.VerbEnable
}

// personSuspended is the unsuspend a person still owes at GitHub.
func (s *session) personSuspended(seat SeatApp, obs SeatObservation) *seataccess.Waiting {
	by := ""
	if obs.Installation != nil && obs.Installation.SuspendedBy != nil {
		if login := strings.TrimSpace(obs.Installation.SuspendedBy.Login); login != "" {
			by = ", which " + login + " suspended"
		}
	}
	return &seataccess.Waiting{
		ActionURL: s.installationURL(seat, obs),
		Detail:    "Unsuspend " + seat.Label() + "'s installation at GitHub" + by,
	}
}

// installationURL is where an organization owner changes an installation.
func (s *session) installationURL(seat SeatApp, obs SeatObservation) string {
	if obs.Installation != nil {
		if url := strings.TrimSpace(obs.Installation.HTMLURL); url != "" {
			return url
		}
		return InstallationSettingsURL(s.opts.WebBase, s.opts.Org, obs.Installation.ID)
	}
	return InstallURL(s.opts.WebBase, s.opts.Org, seat.Slug)
}

// seatApp is one seat's app as the document and the sealed store hold it. A
// seat that left the document is addressed by the ledger's app id and the
// engine-named key.
func (s *session) seatApp(handle string, record *seataccess.Record) SeatApp {
	seat := SeatApp{Handle: handle}
	keyRef := "${" + SeatVar(handle, FieldAppKey) + "}"
	if doc, ok := s.seats[handle]; ok {
		block := doc.block
		seat.Name, seat.Repos = doc.name, slices.Clone(block.Repos)
		seat.AppID, seat.Slug, seat.InstallationID = block.AppID, block.AppSlug, block.InstallationID
		if ref := strings.TrimSpace(block.PrivateKey); ref != "" {
			keyRef = ref
		}
	} else if record != nil && record.Account != nil {
		seat.AppID, _ = strconv.ParseInt(record.Account.ID, 10, 64)
		seat.Slug = record.Account.Name
	}
	if s.env != nil {
		seat.Key = strings.TrimSpace(s.env.Value(keyRef))
	}
	return seat
}

// want is the document's permissions for a seat; a seat that left the
// document asks for nothing.
func (s *session) want(handle string) seataccess.Desired {
	doc, ok := s.seats[handle]
	if !ok {
		return seataccess.Desired{}
	}
	return seataccess.Desired{
		Enabled:     config.SeatAccessOn(doc.block.Enabled),
		Permissions: seataccess.Permissions(doc.block.Permissions),
		Scope:       slices.Clone(doc.block.Repos),
	}
}

// refusal turns GitHub's answer to a step into the vendor's reason, the
// remedy and who acts. A fault GitHub did not answer (a transport failure, a
// 5xx) is returned as it is, to be retried.
func (s *session) refusal(err error, seat SeatApp, verb seataccess.StepVerb) error {
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status >= 500 {
		return err
	}
	said := strings.TrimSpace(apiErr.Detail)
	if said == "" {
		said = fmt.Sprintf("GitHub answered %d", apiErr.Status)
	}
	refusal := &seataccess.Refusal{Said: said, Actor: seataccess.ByYou}
	switch {
	case apiErr.Status == 401:
		refusal.Remedy = "GitHub refused " + seat.Handle + "'s app key. Delete the app at " +
			"GitHub and create it again from the Agents tab"
		refusal.ActionURL = ManageURL(s.opts.WebBase, s.opts.Org, seat.Slug)
	case apiErr.NotFound():
		refusal.Remedy = "GitHub no longer has this installation. Install " + seat.Handle +
			"'s app again, then check again"
		refusal.ActionURL = InstallURL(s.opts.WebBase, s.opts.Org, seat.Slug)
	default:
		refusal.Remedy = "Check " + seat.Handle + "'s app at GitHub, then retry the " +
			string(verb)
		refusal.ActionURL = InstallURL(s.opts.WebBase, s.opts.Org, seat.Slug)
	}
	return refusal
}

// scopeText is a scope for a sentence.
func scopeText(scope []string) string {
	if len(scope) == 0 {
		return "every repository the installation covers"
	}
	return repoList(scope)
}

// repoList is repositories as a sentence lists them: "acme/api and acme/web".
func repoList(repos []string) string {
	said := make([]words.Said, len(repos))
	for i, repo := range repos {
		said[i] = words.Said(repo)
	}
	return words.And(said)
}

// uncovered is what want names that have does not.
func uncovered(want, have []string) []string {
	var out []string
	for _, item := range want {
		if !slices.Contains(have, item) {
			out = append(out, item)
		}
	}
	slices.Sort(out)
	return out
}
