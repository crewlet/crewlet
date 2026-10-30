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
// written by the GitHub pass's own seat-app reconcile ([ReconcileSeatApps]),
// which adopts an installation a person made and clears a record GitHub no
// longer has. This session reads and acts at GitHub and leaves the document
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
		// THE TOKEN THE SEAT ACTS WITH, issued at use and never sealed
		// ([Issuer]); the company's GitHub server reads it as a bearer.
		Tool: &seataccess.ToolBinding{
			Server: ToolServer,
			Keys: []seataccess.BoundKey{{
				Field: FieldAppToken, Header: "Authorization", Prefix: "Bearer ",
			}},
		},
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

// ToolServer is the `mcp_servers` entry a seat's GitHub token is bound to.
const ToolServer = "github"

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
func (vendor) Steps(
	seat seataccess.SeatRef, _ seataccess.Lane, op seataccess.Op,
	want seataccess.Desired, have seataccess.Observed,
) []seataccess.Step {
	name := seatName(seat)
	var steps []seataccess.Step
	add := func(step seataccess.Step) {
		step.Op, step.Index = op, len(steps)
		steps = append(steps, step)
	}
	switch op {
	case seataccess.OpCreate:
		add(seataccess.Step{
			Verb: seataccess.VerbCreate, By: seataccess.ByYou,
			Text: "Create " + name + "'s GitHub App from the prepared manifest, with " +
				Permissions(want.Permissions).Describe(),
			Note: "GitHub offers no API for creating an app, so a person submits the " +
				"manifest. The engine seals the app's key when GitHub returns it.",
		})
		add(seataccess.Step{
			Verb: seataccess.VerbApprove, By: seataccess.ByYou,
			Text: "Install the app on the organization",
			Note: "Which repositories it covers is chosen at GitHub during the install.",
		})
	case seataccess.OpEnable:
		add(seataccess.Step{
			Verb: seataccess.VerbEnable, By: seataccess.ByCrewlet,
			Text: "Unsuspend " + name + "'s installation",
		})
	case seataccess.OpLevel:
		levelSteps(want, have, add)
	case seataccess.OpScope:
		add(seataccess.Step{
			Verb: seataccess.VerbScope, By: seataccess.ByCrewlet,
			Text: "Narrow " + name + "'s tokens to " + scopeText(want.Scope),
			Note: "Applies to the next token; nothing changes at GitHub.",
		})
		if missing := uncovered(want.Scope, have.Scope); len(missing) > 0 {
			add(seataccess.Step{
				Verb: seataccess.VerbApprove, By: seataccess.ByYou,
				Text: "Add " + strings.Join(missing, ", ") + " to the installation's repositories",
				Note: "GitHub refuses a token naming a repository the installation does not cover.",
			})
		}
	case seataccess.OpMint:
		add(seataccess.Step{
			Verb: seataccess.VerbCreate, By: seataccess.ByYou,
			Text: "Delete " + name + "'s app at GitHub and create it again",
			Note: "Its private key is not sealed, and GitHub issues a key once, at creation.",
		})
	case seataccess.OpDisable:
		add(seataccess.Step{
			Verb: seataccess.VerbDisable, By: seataccess.ByCrewlet,
			Text: "Suspend " + name + "'s installation",
			Note: "The app, its key and its installation are kept, so turning it back on needs no one.",
		})
	case seataccess.OpRemove:
		add(seataccess.Step{
			Verb: seataccess.VerbRemove, By: seataccess.ByCrewlet,
			Text: "Uninstall " + name + "'s app and clear its sealed key",
		})
		add(seataccess.Step{
			Verb: seataccess.VerbRemove, By: seataccess.ByYou,
			Text: "Delete the app registration at GitHub",
			Note: "GitHub offers no API for deleting an app.",
		})
	}
	return steps
}

// levelSteps draws a permission change: a narrowing applies at the next mint,
// and a raise waits for the app's owner (past the registration) and then the
// organization (past the installation), in that order (D5).
func levelSteps(want seataccess.Desired, have seataccess.Observed, add func(seataccess.Step)) {
	wanted := Permissions(want.Permissions)
	add(seataccess.Step{
		Verb: seataccess.VerbRerole, By: seataccess.ByCrewlet,
		Text: "Set the seat's tokens to " + wanted.Describe(),
		Note: "Applies to the next token; nothing changes at GitHub.",
	})
	raised := Permissions(have.Permissions).Raises(wanted)
	if len(raised) == 0 {
		return
	}
	if beyond := wanted.Short(Permissions(have.Ceiling).Strings()); len(beyond) > 0 {
		// GRANT, NOT APPROVE: the app's owner widens the registration,
		// which Apply tells from the organization's acceptance by verb.
		add(seataccess.Step{
			Verb: seataccess.VerbGrant, By: seataccess.ByYou,
			Text: "Add " + Names(beyond) + " to the app's permissions",
			Note: "Only the app's owner can widen an app at GitHub.",
		})
	}
	add(seataccess.Step{
		Verb: seataccess.VerbApprove, By: seataccess.ByYou,
		Text: "Accept " + Names(raised) + " on the installation",
		Note: "GitHub asks an organization owner to accept an app's new permissions.",
	})
}

// Open implements [seataccess.Vendor].
func (vendor) Open(_ context.Context, in seataccess.OpenInput) (seataccess.Session, error) {
	if in.Config == nil || in.Config.Integrations.GitHub == nil || !in.Config.Integrations.GitHub.Enabled {
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
func (s *session) Observe(ctx context.Context, in seataccess.ObserveInput) (seataccess.Observation, error) {
	out := seataccess.Observation{}
	for _, handle := range observedHandles(in) {
		record := recordOf(in.Records, handle)
		seat := s.seatApp(handle, record)
		obs, err := ObserveSeat(ctx, s.opts, seat)
		if err != nil {
			return nil, fmt.Errorf("github: observe %s: %w", handle, err)
		}
		s.cached[handle] = obs
		out[handle] = map[seataccess.Lane]seataccess.Observed{
			seataccess.LaneGitHub: s.observed(seat, s.want(handle), obs, record),
		}
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
	if record != nil {
		out.Credential = record.Credential
		if record.Account != nil {
			out.CreatedAt = record.Account.CreatedAt
		}
	}
	if strings.TrimSpace(seat.Key) == "" {
		out.Credential = nil
	} else if out.Credential == nil {
		out.Credential = &seataccess.Credential{Variable: SeatVar(seat.Handle, FieldAppKey)}
	}
	if !obs.AppExists || obs.Installation == nil {
		// NOT INSTALLED reads as no account, so the plan draws the
		// create, whose first step is already done and whose second is
		// the install a person still owes.
		return out
	}
	installation := obs.Installation
	out.Exists = true
	out.Enabled = !installation.Suspended()
	if !out.Enabled {
		out.DisabledBy = seataccess.DisabledByHand
		if record != nil && record.Account != nil && record.Account.State == seataccess.AccountDisabled {
			out.DisabledBy = seataccess.DisabledByCrewlet
		}
	}
	out.Permissions = effective(Permissions(want.Permissions), installation.Permissions)
	out.Ceiling = catalogueOnly(obs.Registered)
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
	case seataccess.OpEnable:
		obs, err := s.observe(ctx, seat, false)
		if err != nil {
			return seataccess.StepResult{}, err
		}
		if obs.Installation == nil {
			return seataccess.StepResult{Waiting: s.installWait(seat)}, nil
		}
		seat.InstallationID = obs.Installation.ID
		if err := UnsuspendSeat(ctx, s.opts, seat); err != nil {
			return seataccess.StepResult{}, s.refusal(err, seat, seataccess.VerbEnable)
		}
		return seataccess.StepResult{}, nil
	case seataccess.OpLevel:
		return s.applyLevel(ctx, in, seat)
	case seataccess.OpScope:
		if in.Step.By == seataccess.ByCrewlet {
			return seataccess.StepResult{}, nil
		}
		seat.Repos = in.Want.Scope
		obs, err := s.observe(ctx, seat, true)
		if err != nil {
			return seataccess.StepResult{}, err
		}
		if missing := obs.UncoveredRepositories(seat); len(missing) > 0 {
			return seataccess.StepResult{Waiting: &seataccess.Waiting{
				ActionURL: s.installationURL(seat, obs),
				Detail: "Add " + strings.Join(missing, ", ") + " to " + handle +
					"'s installation, or narrow the seat to repositories it covers",
			}}, nil
		}
		return seataccess.StepResult{}, nil
	case seataccess.OpMint:
		return seataccess.StepResult{Waiting: &seataccess.Waiting{
			ActionURL: ManageURL(s.opts.WebBase, s.opts.Org, seat.Slug),
			Detail: handle + "'s app key is not sealed, and GitHub issues it once: delete " +
				"the app at GitHub, then create it again from the Agents tab",
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
			Detail: "Create " + in.Seat.Handle + "'s GitHub App from the Agents tab; " +
				"GitHub offers no API for it, so a person submits the prepared manifest",
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

// applyLevel takes a permission change. The first step needs nothing at
// GitHub; each approval re-reads GitHub, so "Done, check again" is a read.
func (s *session) applyLevel(ctx context.Context, in seataccess.ApplyInput, seat SeatApp) (seataccess.StepResult, error) {
	if in.Step.By == seataccess.ByCrewlet {
		return seataccess.StepResult{}, nil
	}
	obs, err := s.observe(ctx, seat, true)
	if err != nil {
		return seataccess.StepResult{}, err
	}
	wait := RaiseWait(s.opts.WebBase, s.opts.Org, seat, Permissions(in.Want.Permissions), obs)
	if wait == nil {
		return seataccess.StepResult{}, nil
	}
	// THE OWNER'S STEP IS DONE once the registration holds the ask, even
	// while the organization's acceptance is still outstanding.
	if !wait.Owner && in.Step.Verb == seataccess.VerbGrant {
		return seataccess.StepResult{}, nil
	}
	return seataccess.StepResult{Waiting: &seataccess.Waiting{ActionURL: wait.URL, Detail: wait.Detail}}, nil
}

// applyRemove uninstalls, then waits for the registration to be deleted.
func (s *session) applyRemove(ctx context.Context, in seataccess.ApplyInput, seat SeatApp) (seataccess.StepResult, error) {
	manage := ManageURL(s.opts.WebBase, s.opts.Org, seat.Slug)
	if in.Step.Index == 0 {
		if seat.AppID != 0 && strings.TrimSpace(seat.Key) != "" {
			obs, err := s.observe(ctx, seat, false)
			if err != nil {
				return seataccess.StepResult{}, err
			}
			if obs.Installation != nil {
				seat.InstallationID = obs.Installation.ID
				if err := UninstallSeat(ctx, s.opts, seat); err != nil {
					return seataccess.StepResult{}, s.refusal(err, seat, seataccess.VerbRemove)
				}
			}
		}
		return seataccess.StepResult{Revoked: []string{FieldAppKey, FieldAppWebhookSecret}}, nil
	}
	if seat.AppID == 0 {
		return seataccess.StepResult{Gone: true}, nil
	}
	// THE KEY IS CLEARED BY NOW, so the registration cannot be asked
	// after; a person confirms the delete with Done, check again, and the
	// seat-app reconcile clears the record once GitHub reports the app
	// gone.
	if strings.TrimSpace(seat.Key) != "" {
		if obs, err := s.observe(ctx, seat, true); err == nil && !obs.AppExists {
			return seataccess.StepResult{Gone: true}, nil
		}
	}
	return seataccess.StepResult{Waiting: &seataccess.Waiting{
		ActionURL: manage,
		Detail: "Delete " + in.Seat.Handle + "'s app registration at GitHub: " +
			ManagePath() + ". GitHub offers no API for it",
	}}, nil
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
		Detail: seat.Handle + "'s app exists and nothing has installed it: install it on " +
			orgLabel(s.opts.Org),
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

// observedHandles is every seat a pass reads back: the document's, and every
// one the ledger still records, sorted.
func observedHandles(in seataccess.ObserveInput) []string {
	var out []string
	for _, seat := range in.Seats {
		out = append(out, seat.Handle)
	}
	for handle := range in.Records {
		out = append(out, handle)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// recordOf is a handle's ledger record, or nil.
func recordOf(records map[string]seataccess.Record, handle string) *seataccess.Record {
	if record, ok := records[handle]; ok {
		return &record
	}
	return nil
}

// seatName is how a step names the seat.
func seatName(seat seataccess.SeatRef) string {
	if name := strings.TrimSpace(seat.Name); name != "" {
		return name
	}
	return seat.Handle
}

// scopeText is a scope for a sentence.
func scopeText(scope []string) string {
	if len(scope) == 0 {
		return "every repository the installation covers"
	}
	return strings.Join(scope, ", ")
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
