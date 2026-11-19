package gitlab

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/provision"
	"github.com/crewlet/crewlet/internal/setup"
	"github.com/crewlet/crewlet/internal/whsec"

	"github.com/crewlet/crewlet/internal/integration"
)

// Reconcile brings a GitLab instance in line with the company config, for
// what the company holds as a whole: the group it provisions into, the token
// the provisioning runs on, and the webhook that delivers to this deployment.
//
// Seats are not this pass's: each agent's account, memberships and token are
// per-seat access ([Vendor], internal/seataccess), read back every pass and
// applied by the shared sync. This pass never creates, re-roles, rotates or
// removes an account.
//
// Running it twice must be safe and quiet: a hook that already carries what
// this pass would write is left exactly as it is.

// Result is what one reconcile did, for the report.
type Result struct {
	// Hooked is the webhook target this run registered or re-pointed, or
	// empty when webhooks were not part of it.
	Hooked string
	// HookedOn names WHERE it was registered: the single element "group",
	// or one entry per project. Reported because the two are not
	// interchangeable: a group hook covers projects added later and a set
	// of project hooks does not.
	HookedOn []string

	// NoIngress says why this run left the instance with no delivery path
	// at all, and is empty when it registered one.
	//
	// A SEPARATE FIELD rather than an empty [Result.Hooked], because an
	// empty Hooked is also what a healthy run looks like from the outside,
	// and [integration.Classify] over no findings is READY: a company that
	// never set its webhook address would read as ready while not one merge
	// request, pipeline or comment reached a seat.
	NoIngress string

	// NoIngressKey is the Tier A key this deployment's webhook address is
	// read from, which NoIngress names as the setting to fill in. Empty is
	// [setup.BaseKey]'s.
	NoIngressKey string

	// CredentialExpires is when the provisioning token this run
	// authenticated with stops working, set ONLY when that is inside
	// [integration.ExpiryWarning] of the run's clock; zero otherwise, and
	// zero where GitLab published no expiry or the run could not read it.
	// It is the one credential here nothing rotates: a person pasted it in,
	// and the day it lapses every pass is refused.
	CredentialExpires time.Time

	// Notes carries anything the run found worth saying.
	Notes []string

	// Recorded counts the values this run wrote to the sink: a minted or
	// rotated signing secret. A count rather than names, because the names
	// hold live credentials; zero means a re-run that changed nothing.
	Recorded int
}

// Mode is where a run's service accounts live.
//
// Two ownerships, not two capabilities. A GROUP service account is owned by the
// top-level group the company provisions into: the default, the only shape
// GitLab.com offers, and a group Owner PAT creates one.
//
// An INSTANCE service account belongs to the instance: self-managed only, it
// needs an instance-admin PAT, and is a member of nothing until this run adds
// it, so one company's seats can span several top-level groups and survive
// their group's deletion. Everything downstream is identical.
type Mode string

const (
	// ModeGroup owns service accounts from the configured top-level group.
	ModeGroup Mode = "group"
	// ModeInstance owns them from the instance. Self-managed only.
	ModeInstance Mode = "instance"
)

// Valid reports a mode this build serves. The empty string is [ModeGroup]:
// a caller that says nothing means the default, and the alternative is every
// call site defaulting it separately until one of them forgets.
func (m Mode) Valid() bool {
	switch m {
	case "", ModeGroup, ModeInstance:
		return true
	}
	return false
}

// Or resolves the mode, folding the empty string to the default.
func (m Mode) Or() Mode {
	if m == "" {
		return ModeGroup
	}
	return m
}

// Modes is every mode, for an error that has to name them.
func Modes() []string { return []string{string(ModeGroup), string(ModeInstance)} }

// Options are one reconcile's inputs.
type Options struct {
	// Client talks to the instance with the provisioning token.
	Client *Client

	// Config is the company's gitlab block.
	Config *config.GitLab

	// Sink records a minted or rotated signing secret.
	Sink provision.TokenSink

	// WebhookBase is where the instance reaches this deployment
	// ([config.API.WebhookBase]), or empty to skip webhook registration.
	//
	// SKIPPED RATHER THAN GUESSED. A hook pointing at the wrong host is
	// worse than no hook: the instance reports a healthy integration, and
	// the deliveries go somewhere nobody is looking.
	WebhookBase string

	// WebhookBaseKey is the Tier A key WebhookBase is read from
	// ([config.API.WebhookBaseKey]), which a run with no address names as
	// the setting to fill in. Empty is [setup.BaseKey]'s.
	WebhookBaseKey string

	// SigningSecret is the value the hook is registered with, resolved.
	//
	// Empty means the config's ${VAR} answered nothing, and the run MINTS
	// one: see [signingSecret]. GitLab's signing token is caller-supplied
	// and write-only, so a hook registered with an empty one verifies
	// nothing and there is no way to read back what it should have been.
	SigningSecret string

	// SigningSecretVar is the variable a minted secret is recorded under,
	// empty when the config's signing_secret is not a whole ${VAR}.
	SigningSecretVar string

	// Rotate replaces the signing secret in the variable signing_secret
	// names ([PlanSigningSecret]).
	//
	// A FLAG RATHER THAN WHAT A RUN DOES: a new key re-points the hook at a
	// value the RUNNING engine does not hold yet, and every delivery is
	// refused until it does.
	Rotate bool

	// Now is the clock the token-expiry warning is judged by. Nil takes the
	// wall clock.
	Now func() time.Time
}

// Reconcile runs one pass.
func Reconcile(ctx context.Context, opts Options) (*Result, error) {
	// A CANCELLED PASS HAS OBSERVED NOTHING, so it must RAISE rather than
	// answer with a Result: the loop reads a Result with no findings as
	// "this integration is ready" and trusts it for a full settled interval.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if opts.Client == nil {
		return nil, errors.New("gitlab: no client")
	}
	if opts.Sink == nil {
		// A run with nowhere to put a minted signing secret would register
		// a hook signed with a key nothing holds.
		return nil, provision.ErrNoSink
	}
	if opts.Config == nil || opts.Config.Provisioning == nil {
		return nil, errors.New("gitlab: integrations.gitlab.provisioning is unset, " +
			"so there is no group to reconcile; name one")
	}
	p := opts.Config.Provisioning
	group, found, err := opts.Client.GroupByPath(ctx, p.Group)
	if err != nil {
		// GitLab has no separate auth probe, so this first call is where
		// a bad token shows up, and a refusal is told from an unreachable
		// instance here.
		return nil, fmt.Errorf("gitlab: resolve group %q: %w", p.Group,
			integration.Reject(err, Status(err)))
	}
	if !found {
		return nil, fmt.Errorf(
			"gitlab: no group %q on this instance: every seat's access is "+
				"granted within it, so it has to exist", p.Group)
	}

	res := &Result{}

	// THE CREDENTIAL JUST PROVED IT WORKS, so this is the moment to ask how
	// long it will go on working. A NOTE, never a failure, when GitLab
	// cannot say: `/personal_access_tokens/self` answers only for a
	// personal (or group or project) access token and only on GitLab 15.5
	// and later.
	if self, serr := opts.Client.SelfToken(ctx); serr != nil {
		res.Notes = append(res.Notes, fmt.Sprintf(
			"could not read when this pass's own token expires, so no expiry "+
				"warning can be given: %v", serr))
	} else if integration.ExpiresSoon(self.ExpiresAt.Time, clock(opts)) {
		res.CredentialExpires = self.ExpiresAt.UTC()
	}

	// PROJECTS ARE RESOLVED BEFORE ANYTHING IS WRITTEN, and a missing one
	// is dropped rather than fatal: see [resolveProjects].
	projects, notes, err := resolveProjects(ctx, opts.Client, p.Projects)
	if err != nil {
		return nil, err
	}
	res.Notes = append(res.Notes, notes...)

	// THE WEBHOOK IS REGISTERED WHATEVER THE SEATS, which an earlier shape
	// of this pass did not do: it returned before this point when no seat
	// asked for an account, so a company whose seats were all off had no
	// hook at all, and nothing said so.
	if target := webhookTarget(opts.WebhookBase); target != "" {
		secret, note, freshSecret, err := signingSecret(ctx, opts)
		if err != nil {
			return nil, rollback(ctx, opts, err)
		}
		if note != "" {
			res.Notes = append(res.Notes, note)
		}
		if freshSecret {
			res.Recorded++
		}
		opts.SigningSecret = secret
		hooked, notes, err := ensureHooks(ctx, opts, group, projects, target)
		if err != nil {
			return nil, rollback(ctx, opts, err)
		}
		res.Hooked, res.HookedOn = target, hooked
		res.Notes = append(res.Notes, notes...)
	} else {
		// REPORTED, not just noted: a pass with no findings classifies
		// READY, on a company whose GitLab delivers nothing. See
		// [Result.NoIngress].
		res.NoIngress = "no webhook is registered on this GitLab, so merge " +
			"requests, pipelines, issues and comments reach no seat: set " +
			setup.BaseKey(opts.WebhookBaseKey) + " to the address this " +
			"instance reaches this deployment at and the next pass " +
			"registers one, or register it by hand"
		res.NoIngressKey = opts.WebhookBaseKey
		res.Notes = append(res.Notes, res.NoIngress)
	}

	if err := opts.Sink.Flush(ctx); err != nil {
		return nil, rollback(ctx, opts, err)
	}
	return res, nil
}

// TokenName is the name this tool mints under.
//
// It is the ONLY thing distinguishing a token this tool owns from one an
// administrator created by hand, and both the keep-or-mint decision and the
// retire step key on it — so it is a named function rather than a format
// string repeated at three call sites, where the three would eventually
// differ and rotation would quietly stop retiring anything.
func TokenName(handle string) string { return "crewlet-" + handle }

// verify asks the instance who a token authenticates as.
func (c *Client) verify(ctx context.Context, value string, wantID int) provision.Verdict {
	probe, err := NewClient(ClientOptions{URL: c.base, Token: value, HTTP: c.http})
	if err != nil {
		return provision.VerdictRejected
	}
	var who User
	err = probe.get(ctx, "/user", nil, &who)
	switch {
	case err == nil && who.ID == wantID:
		return provision.VerdictSelf
	case err == nil:
		return provision.VerdictOther
	case status(err) == http.StatusUnauthorized, status(err) == http.StatusForbidden:
		return provision.VerdictRejected
	default:
		// A 5xx or a dropped connection. NOT a rejection: re-minting on
		// "cannot tell" destroys a token that works.
		return provision.VerdictUnknown
	}
}

// status reads the HTTP status off an error, or 0 where there is none.
func status(err error) int {
	var api *APIError
	if errors.As(err, &api) {
		return api.Status
	}
	return 0
}

func clock(opts Options) time.Time {
	if opts.Now != nil {
		return opts.Now()
	}
	return time.Now()
}

// signingSecret is what the hook is registered with, minting one when the
// config's reference answered nothing.
//
// GitLab's signing token is CALLER-SUPPLIED and write-only. A hook registered
// with an empty one is accepted, looks healthy, and signs every delivery with
// nothing, which the engine refuses; the only trace is a
// `webhook_signature_invalid` log line. So an empty resolution is minted into
// the variable the config's ${VAR} names (as the seat tokens are) or refused,
// because a literal has nowhere to record it.
//
// The third result says a NEW secret exists as of this run, minted or rotated;
// it counts towards [Result.Recorded].
func signingSecret(ctx context.Context, opts Options) (secret, note string, fresh bool, err error) {
	plan := PlanSigningSecret(opts.SigningSecret, opts.SigningSecretVar, opts.Rotate,
		true, opts.WebhookBaseKey)
	switch plan.Action {
	case SigningReuse:
		return strings.TrimSpace(opts.SigningSecret), plan.Note, false, nil
	case SigningBlocked:
		return "", "", false, errors.New("gitlab: " + plan.Note)
	}
	// THE SINK IS ASKED BEFORE ANYTHING IS MINTED, which is
	// [provision.MintSecret]'s subject and was missing here. `secret`
	// above comes from the resolver, and the resolver answers from a
	// SNAPSHOT taken at apply time — so the variable a previous pass
	// minted into resolves to nothing until something rebuilds it, and
	// every pass in that window minted a SECOND key, sealed it over the
	// first and re-pointed the hook at it. The engine's own
	// /webhooks/gitlab route verifies with the snapshot, so each rotation
	// moved the instance further from the value the running process holds,
	// on the reconcile loop's timer.
	minted, err := provision.MintSecret(
		ctx, opts.Sink, plan.Var, plan.Action == SigningRotate, whsec.Mint)
	if err != nil {
		return "", "", false, fmt.Errorf("gitlab: %w", err)
	}
	if !minted.Minted {
		// ALREADY SEALED, by a pass whose value the resolver has not
		// caught up with. Nothing was minted, so the plan's note would
		// claim a rotation that did not happen.
		return minted.Value, "", false, nil
	}
	return minted.Value, plan.Note + " — " + opts.Sink.NextStep(), true, nil
}

// SigningAction is what a run will do about the webhook signing secret.
type SigningAction string

// The outcomes, named so the plan and the run cannot describe them
// differently.
const (
	// SigningUntouched: no hook is being registered, so the secret is not
	// this run's business.
	SigningUntouched SigningAction = "untouched"
	// SigningReuse: a usable secret already resolved.
	SigningReuse SigningAction = "reuse"
	// SigningMint: none resolved, and the config names a ${VAR} to put one in.
	SigningMint SigningAction = "mint"
	// SigningRotate: one resolved and -rotate was passed, so it is replaced.
	SigningRotate SigningAction = "rotate"
	// SigningBlocked: none resolved and nowhere to record one.
	SigningBlocked SigningAction = "blocked"
)

// SigningPlan is what a run intends to do about the webhook signing secret,
// decided BEFORE anything is touched so `-dry-run` can state it.
//
// The plan and the run read the same function, which is the rule the seat
// plan already follows: a dry run that re-derived this separately would be a
// second implementation, free to disagree with the real one about the most
// consequential thing a run can do — replacing the key a working hook signs
// with, which fails every delivery in flight until the new value reaches the
// engine.
type SigningPlan struct {
	Action SigningAction
	// Var is the ${VAR} a minted or rotated secret is recorded into.
	Var string
	// Note is the caveat this outcome carries, or empty.
	Note string
}

// PlanSigningSecret decides what a run will do about the signing secret.
//
// secret is the RESOLVED value of integrations.gitlab.signing_secret, varName
// the whole ${VAR} it references (empty when it is a literal),
// registeringHooks whether this run has a public URL to point a hook at, and
// baseKey the Tier A key that URL is read from ([Options.WebhookBaseKey]),
// which a refusal names as the setting to clear for a hook registered by hand.
func PlanSigningSecret(secret, varName string, rotate, registeringHooks bool, baseKey string) SigningPlan {
	if !registeringHooks {
		return SigningPlan{Action: SigningUntouched}
	}
	// -rotate REACHES THE SIGNING SECRET, not just the seat tokens, so an
	// operator who believes the key leaked, or rotates on a schedule, has
	// one command for it rather than editing environment variables by hand
	// to recover.
	//
	// Deliberately gated on the flag rather than done on every run: minting
	// a signing secret every time would re-point the hook at a key the
	// RUNNING engine does not have yet, refusing every delivery until the
	// new value reaches it (sourced and restarted from -env-file or -print,
	// or the POST /config/reload a -secret-store run names) — the same
	// outage the seat tokens' Rotate doc explains at length.
	rotating := rotate && varName != ""
	if trimmed := strings.TrimSpace(secret); trimmed != "" && !rotating {
		if rotate {
			// REPORTED, NOT REFUSED. -rotate replaces the seat tokens as
			// well as a signing secret a variable names, and failing the
			// whole run would stop an operator rotating those because of
			// a signing secret they manage by hand. They do need to hear
			// it, though — so it is a note, on the plan a dry run prints
			// ([SigningPlan.Describe]) and on a run that otherwise
			// succeeded, rather than silence. It says how to replace the
			// literal and nothing about why one would, which is the
			// operator's to know.
			return SigningPlan{Action: SigningReuse, Note: "-rotate leaves the " +
				"webhook signing secret alone: integrations.gitlab.signing_secret " +
				"is a literal, so there is nowhere to record a new one. To replace " +
				"it, point the field at a whole ${VAR} and re-run with -rotate, or " +
				"replace the literal by hand and re-run"}
		}
		return SigningPlan{Action: SigningReuse}
	}
	if varName == "" {
		return SigningPlan{Action: SigningBlocked, Note: "integrations.gitlab." +
			"signing_secret resolved to nothing and is not a whole ${VAR} " +
			"reference, so there is nowhere to record a minted one. Point it at " +
			"a variable, export a whsec_ value yourself, or clear both -public-url " +
			"and " + setup.BaseKey(baseKey) + " and register the hook by hand"}
	}
	action, what := SigningMint, "minted"
	if rotating {
		action, what = SigningRotate, "rotated"
	}
	return SigningPlan{Action: action, Var: varName, Note: fmt.Sprintf(
		"%s a webhook signing secret into %s", what, varName)}
}

// Describe renders the plan as the line a run prints before it acts.
func (p SigningPlan) Describe() string {
	switch p.Action {
	case SigningUntouched:
		return "webhook signing secret: untouched (no webhook address — no " +
			"-public-url and none in Tier A — so no hook is registered)"
	case SigningReuse:
		// A REUSE -ROTATE COULD NOT REACH SAYS SO BEFORE THE RUN: printed
		// as a plain reuse, a dry run with -rotate said nothing of the one
		// secret the flag was going to leave in place, and only a real
		// run's notes did. The note is the sentence those notes carry, so
		// the plan and the run cannot tell it two ways.
		if p.Note != "" {
			return "webhook signing secret: reused — " + p.Note
		}
		return "webhook signing secret: reused — the configured one already resolves"
	case SigningMint:
		return "webhook signing secret: WILL BE MINTED into " + p.Var
	case SigningRotate:
		return "webhook signing secret: WILL BE ROTATED into " + p.Var +
			" — the hook stops verifying against the old key the moment this runs"
	case SigningBlocked:
		return "webhook signing secret: THIS RUN WILL FAIL — " + p.Note
	}
	return ""
}

// SigningSecretPrefix is what GitLab's Standard-Webhooks implementation
// expects a signing token to start with.
const SigningSecretPrefix = whsec.Prefix

// MintSigningSecret generates a Standard-Webhooks signing secret.
//
// The FORMAT lives in [whsec], with the three readers that must agree on it.
// This is the vendor-facing name for it, because a caller here is asking
// GitLab a question and should not have to know which spec answers it.
func MintSigningSecret() (string, error) { return whsec.Mint() }

// ensureHooks registers this deployment's webhook at ONE level, never both: a
// group and a project hook on the same events BOTH fire, and the inbox does
// not deduplicate that.
//
// THE GROUP HOOKS API IS NOT EVERYWHERE: Premium on gitlab.com, absent from
// Community Edition, hidden as a 404. Group-only registration failed AFTER
// minting, so the rollback revoked every credential just created. The
// unlicensed `gitlab-ee` 19.3.0 compose image DOES serve it, so the local loop
// takes the group path; use `false` to exercise the fallback.
//
// provisioning.group_webhook: auto (default) tries the group and falls back,
// true demands it, false goes to the projects. A [Hook.Converged] one is left.
func ensureHooks(ctx context.Context, opts Options, group Group, projects []string,
	target string,
) ([]string, []string, error) {
	mode := config.ContainerWebhookAuto
	if pv := opts.Config.Provisioning; pv != nil && pv.GroupWebhook != "" {
		mode = pv.GroupWebhook
	}
	secret := opts.SigningSecret
	name := opts.Config.WebhookNameOrDefault()

	// A FREE GROUP TAKES THE REGISTRATION AND NEVER DELIVERS, which no
	// error can tell you.
	//
	// The fallback below turns on the create call FAILING, and on gitlab.com's
	// free tier it does not: POST /groups/:id/hooks answers 201 and the hook
	// never delivers, so the tier is READ, not inferred from a refusal.
	//
	// GET /groups/:path omits `plan` for a free group exactly as a self-managed
	// instance does, while /namespaces/:path answers `plan: "free"`.
	// [Client.TierOf] therefore asks the namespace when the group says nothing;
	// a self-managed instance still answers unknown at both and keeps its
	// behaviour.
	tier, err := opts.Client.TierOf(ctx, group)
	if err != nil {
		// A TIER THAT COULD NOT BE READ IS NOT FREE. Concluding one from a
		// failed request would move a working group hook to per-project
		// hooks over a blip, and back on the next pass — somebody's
		// webhooks rewritten on a timer.
		return nil, nil, err
	}
	if mode == config.ContainerWebhookAuto && tier == TierFree {
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		hooked, err := ensureProjectHooks(ctx, opts.Client, projects, name, target, secret)
		if err != nil {
			return nil, nil, err
		}
		if err := sweepGroupHooks(ctx, opts.Client, group.ID, name); err != nil {
			return nil, nil, err
		}
		return hooked, []string{
			"this group is on GitLab's free tier, where a group webhook is " +
				"accepted and never delivered, so one hook was registered per " +
				"provisioning.projects entry instead; a project added to the " +
				"group later will NOT be covered until this runs again",
		}, nil
	}

	if mode != config.ContainerWebhookNever {
		// ASSIGNED, NOT REDECLARED. The tier read above is done with its
		// error, and a fresh `err :=` here would be a second variable the
		// switch below reads instead of the one this line wrote — the
		// exact shape govet's shadow check earns its place on.
		err = ensureGroupHook(ctx, opts.Client, group.ID, name, target, secret)
		switch {
		case err == nil:
			return []string{"group"}, nil, sweepProjectHooks(ctx, opts.Client, projects, name)
		case mode == config.ContainerWebhookRequire:
			return nil, nil, fmt.Errorf(
				"%w\n\ngroup_webhook is \"true\", so no per-project fallback was "+
					"tried. Group webhooks are a GitLab Premium feature: on Free "+
					"the endpoint answers 404. Set group_webhook: false (or auto) "+
					"to register one hook per provisioning.projects entry", err)
		case !gatedByTier(err):
			return nil, nil, err
		}
		// The tier gate, on auto. Fall through to the projects, and say
		// so — an operator who expected one group hook and got four
		// project hooks should learn it here rather than from the
		// instance's settings pages.
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		hooked, err := ensureProjectHooks(ctx, opts.Client, projects, name, target, secret)
		if err != nil {
			return nil, nil, err
		}
		return hooked, []string{
			"this instance does not serve the group webhooks API (it is a " +
				"GitLab Premium feature), so one hook was registered per " +
				"project instead of one for the group; a project added to the " +
				"group later will NOT be covered until this runs again",
		}, nil
	}

	hooked, err := ensureProjectHooks(ctx, opts.Client, projects, name, target, secret)
	if err != nil {
		return nil, nil, err
	}
	return hooked, nil, sweepGroupHooks(ctx, opts.Client, group.ID, name)
}

// sweepProjectHooks removes the per-project hooks this engine left behind
// when the company moved UP to a group hook.
//
// THE OTHER HALF OF [sweepGroupHooks]: a group hook fires for every event in
// every project of the group, so a project hook beside it delivers each one
// twice, and this engine registered both.
//
// The declared projects only, which is every project this engine has hooked:
// [ensureProjectHooks] refuses to run without `provisioning.projects`. A
// project dropped from the list keeps its hook, as with [resolveProjects]'
// membership answer, because a company mid-edit looks exactly like one that
// removed a project.
func sweepProjectHooks(ctx context.Context, c *Client, projects []string, name string) error {
	for _, project := range projects {
		hooks, err := c.ProjectHooks(ctx, project)
		if err != nil {
			return fmt.Errorf("gitlab: list %s's hooks to remove ours: %w", project, err)
		}
		for _, hook := range mine(hooks, name) {
			if err := c.DeleteProjectHook(ctx, project, hook.ID); err != nil {
				return fmt.Errorf(
					"gitlab: remove the hook this engine left on %s at %s, which "+
						"delivers everything the group hook already does: %w",
					project, hook.URL, err)
			}
		}
	}
	return nil
}

// sweepGroupHooks removes the group hook this engine left behind when the
// company moved to per-project hooks.
//
// THE LEVEL IS A CHOICE THAT MOVES: `group_webhook` is live config and `auto`
// depends on the group's PLAN, so a company that flips it or whose paid group
// lapses registers per project from then on and never looks at the level it
// stopped writing, whose hook delivers every event a second time for ever.
// [Teardown] sweeps both levels for the same reason.
//
// A TIER GATE IS NOT A FAILURE: the endpoint 404s where the tier does not
// serve group hooks, which means nothing to sweep. See [gatedByTier].
func sweepGroupHooks(ctx context.Context, c *Client, groupID int, name string) error {
	hooks, err := c.GroupHooks(ctx, groupID)
	if err != nil {
		if gatedByTier(err) {
			return nil
		}
		return fmt.Errorf("gitlab: list group hooks to remove ours: %w", err)
	}
	for _, hook := range mine(hooks, name) {
		if err := c.DeleteGroupHook(ctx, groupID, hook.ID); err != nil {
			return fmt.Errorf(
				"gitlab: remove the group hook this engine left at %s, which "+
					"delivers everything the project hooks already do: %w",
				hook.URL, err)
		}
	}
	return nil
}

// resolveProjects keeps the declared projects this instance actually has.
//
// A missing one is dropped, not fatal: a renamed, moved or not-yet-created
// repository is an ordinary config state, not a reason to refuse the other
// nine. Aborting on the first 404 happened MID-LOOP, after minting, and the
// rollback revoked the credentials the run had just created.
//
// It runs BEFORE any mutation, like the group's resolution: a check halfway
// through leaves half a reconcile. Unlike a knowledge-base importer, where
// half an import looks like a complete base with holes, memberships and hooks
// are additive and independent, so re-running adds the rest.
func resolveProjects(ctx context.Context, c *Client, declared []string) ([]string, []string, error) {
	kept := make([]string, 0, len(declared))
	var missing []string
	for _, project := range declared {
		exists, err := c.ProjectExists(ctx, project)
		if err != nil {
			return nil, nil, fmt.Errorf("gitlab: check project %s: %w", project, err)
		}
		if !exists {
			missing = append(missing, project)
			continue
		}
		kept = append(kept, project)
	}
	if len(missing) == 0 {
		return kept, nil, nil
	}
	return kept, []string{fmt.Sprintf(
		"these provisioning.projects are not on this instance and were "+
			"skipped: %s — create them (or drop them from the config) and "+
			"re-run; everything else reconciled",
		strings.Join(missing, ", "))}, nil
}

// webhookPath is where every GitLab delivery arrives, whatever base carries
// it.
const webhookPath = "/webhooks/gitlab"

// DefaultWebhookName is the name this engine's hooks carry when the company
// document does not choose one.
//
// Restated by [config.GitLab.WebhookNameOrDefault], which is where the rest
// of the engine reads it from — config is the leaf every vendor package
// depends on, so the constant cannot live only here. A test asserts the two
// agree.
const DefaultWebhookName = "crewlet"

// ours reports a hook this deployment registered.
//
// THE NAME IS THE IDENTITY, the only field that survives a change of public
// base: matching on the URL left a live orphan at every level delivering to an
// address that no longer answers.
//
// THE DELIVERY PATH GUARDS against somebody else's hooks: ours all end in
// /webhooks/gitlab, so one that merely shares the name is left alone. A HOOK
// WITH NO NAME IS NOT OURS ([hookBody], [confirmHook]).
//
// Two DEPLOYMENTS watching one instance set distinct
// `integrations.gitlab.webhook_name` values, as they would for Jira's.
func ours(hook Hook, name string) bool {
	return strings.HasSuffix(hook.URL, webhookPath) && hook.Name == name
}

// mine is every hook at one container that this deployment registered, in the
// order the instance listed them.
func mine(hooks []Hook, name string) []Hook {
	out := make([]Hook, 0, len(hooks))
	for _, hook := range hooks {
		if ours(hook, name) {
			out = append(out, hook)
		}
	}
	return out
}

// ensureGroupHook registers the one group webhook, or re-points it.
//
// MATCHED ON THE NAME — see [ours] — and converged to ONE. Converging the
// first match and stopping still leaves every hook a previous address
// created: this engine's own registrations, live, two of them delivering
// somewhere that no longer answers. What "converged" has to mean is one.
func ensureGroupHook(ctx context.Context, c *Client, groupID int, name, target, secret string) error {
	hooks, err := c.GroupHooks(ctx, groupID)
	if err != nil {
		return fmt.Errorf("gitlab: list group hooks: %w", err)
	}
	held := mine(hooks, name)
	for _, extra := range held[min(1, len(held)):] {
		if err := c.DeleteGroupHook(ctx, groupID, extra.ID); err != nil {
			return fmt.Errorf(
				"gitlab: remove the group hook this engine left at %s: %w",
				extra.URL, err)
		}
	}
	if len(held) > 0 {
		hook := held[0]
		if hook.Converged(name, target, HookDigest(secret)) {
			// NOTHING TO WRITE, and the listing above is also the
			// confirmation. A rotated secret is caught by the digest in
			// the description, so an unconditional PUT would only re-send
			// this identical body every few minutes.
			//
			// No re-read either: [confirmHook] guards a WRITE (a GitLab
			// older than 19.1 ignores a signing token and answers 200),
			// and [Hook.Converged] already required this listing to
			// report a signing token.
			return nil
		}
		if err := c.UpdateGroupHook(ctx, groupID, hook.ID, name, target, secret); err != nil {
			return fmt.Errorf("gitlab: update group hook: %w", err)
		}
		return confirmHook("group hook", func() ([]Hook, error) {
			return c.GroupHooks(ctx, groupID)
		}, name, target)
	}
	created, createErr := c.CreateGroupHook(ctx, groupID, name, target, secret)
	if createErr != nil {
		return fmt.Errorf("gitlab: create group hook: %w", createErr)
	}
	return removeIfUnnamed(ctx, confirmHook("group hook", func() ([]Hook, error) {
		return c.GroupHooks(ctx, groupID)
	}, name, target), func(ctx context.Context) error {
		return c.DeleteGroupHook(ctx, groupID, created.ID)
	})
}

// errUnnamed is a hook read back without the name this engine wrote: an
// instance older than GitLab 17.1, which takes the attribute, ignores it and
// answers 200.
var errUnnamed = errors.New("the instance did not keep the hook's name")

// confirmHook reads the hook back and refuses to call the run a success unless
// GitLab says it carries this deployment's name and now holds a signing token.
//
// THE NAME FIRST, because every later pass finds the hook by it ([ours]). An
// unnamed hook is [errUnnamed], and the create path removes it rather than
// have it created again every pass ([removeIfUnnamed]).
//
// THE WRITE SUCCEEDING PROVES NOTHING. `signing_token` arrived in GitLab 19.0
// (GA 19.1); an older instance ignores it and answers 200, so the hook looks
// healthy but delivers unsigned to an engine whose verification is mandatory.
// `signing_token_present` is enough: what is confirmed is that a token EXISTS,
// not which one.
func confirmHook(what string, list func() ([]Hook, error), name, target string) error {
	hooks, err := list()
	if err != nil {
		return fmt.Errorf("gitlab: re-read the %s to confirm it can sign: %w", what, err)
	}
	unnamed := false
	for _, hook := range hooks {
		if hook.URL != target {
			continue
		}
		if hook.Name != name {
			unnamed = unnamed || hook.Name == ""
			continue
		}
		if !hook.SigningTokenPresent {
			return fmt.Errorf(
				"gitlab: the %s at %s reports no signing token, so this "+
					"instance would deliver unsigned and the engine refuses "+
					"unsigned deliveries. Signing tokens need GitLab 19.1 or "+
					"newer (19.0 behind the webhook_signing_token flag)",
				what, target)
		}
		return nil
	}
	if unnamed {
		return fmt.Errorf("gitlab: the %s at %s came back with no name, so no later "+
			"pass could recognise it as this deployment's. Hook names need GitLab "+
			"17.1 or newer, and signing tokens 19.1: %w", what, target, errUnnamed)
	}
	return fmt.Errorf("gitlab: the %s at %s is not there after writing it", what, target)
}

// removeIfUnnamed removes a hook this pass has just CREATED when [confirmHook]
// found the instance did not keep its name, and returns the confirmation's own
// answer with any failure of the removal appended.
//
// Left in place, the hook is nobody's: [ours] will not adopt it, so the next
// pass creates another, and an instance that ignores the name would gather one
// unsigned hook per pass for as long as the integration is enabled. DETACHED,
// because the failure being undone may be a cancelled context.
func removeIfUnnamed(ctx context.Context, confirmed error, remove func(context.Context) error) error {
	if !errors.Is(confirmed, errUnnamed) {
		return confirmed
	}
	if err := remove(context.WithoutCancel(ctx)); err != nil {
		return fmt.Errorf("%w (and removing the hook it created failed: %w)", confirmed, err)
	}
	return confirmed
}

// ensureProjectHooks registers one hook per declared project.
//
// It refuses an empty list rather than registering nothing: a run that
// quietly hooked no project leaves the instance reporting a healthy
// integration that delivers to nobody, which is the exact failure the
// skipped-rather-than-guessed rule above exists to prevent.
func ensureProjectHooks(ctx context.Context, c *Client, projects []string,
	name, target, secret string,
) ([]string, error) {
	if len(projects) == 0 {
		return nil, errors.New(
			"gitlab: per-project webhooks need provisioning.projects, and none " +
				"are declared — either list the projects to hook, or use a " +
				"Premium instance where one group hook covers them all")
	}
	hooked := make([]string, 0, len(projects))
	for _, project := range projects {
		if err := ensureProjectHook(ctx, c, project, name, target, secret); err != nil {
			return nil, err
		}
		hooked = append(hooked, project)
	}
	return hooked, nil
}

func ensureProjectHook(ctx context.Context, c *Client, project, name, target, secret string) error {
	hooks, err := c.ProjectHooks(ctx, project)
	if err != nil {
		return fmt.Errorf("gitlab: list hooks on %s: %w", project, err)
	}
	held := mine(hooks, name)
	for _, extra := range held[min(1, len(held)):] {
		if err := c.DeleteProjectHook(ctx, project, extra.ID); err != nil {
			return fmt.Errorf(
				"gitlab: remove the hook this engine left on %s at %s: %w",
				project, extra.URL, err)
		}
	}
	if len(held) > 0 {
		hook := held[0]
		// THE SAME SKIP THE GROUP PATH TAKES, and it has to be here too:
		// this branch runs once per declared project, so an unconditional
		// re-write cost one write per project per pass rather than one.
		// See [ensureGroupHook] for the whole reasoning.
		if hook.Converged(name, target, HookDigest(secret)) {
			return nil
		}
		if err := c.UpdateProjectHook(ctx, project, hook.ID, name, target, secret); err != nil {
			return fmt.Errorf("gitlab: update hook on %s: %w", project, err)
		}
		return confirmHook("hook on "+project, func() ([]Hook, error) {
			return c.ProjectHooks(ctx, project)
		}, name, target)
	}
	created, createErr := c.CreateProjectHook(ctx, project, name, target, secret)
	if createErr != nil {
		return fmt.Errorf("gitlab: create hook on %s: %w", project, createErr)
	}
	return removeIfUnnamed(ctx, confirmHook("hook on "+project, func() ([]Hook, error) {
		return c.ProjectHooks(ctx, project)
	}, name, target), func(ctx context.Context) error {
		return c.DeleteProjectHook(ctx, project, created.ID)
	})
}

// gatedByTier reports whether a failure is GitLab withholding a licensed
// endpoint rather than refusing this request.
//
// 404 is the one that matters and the one that reads wrong: GitLab hides an
// unavailable endpoint rather than answering 402, so "not found" is what an
// instance says about a feature its tier does not serve. 403 is the same
// answer from one that surfaces the endpoint and refuses the call. Anything
// else —
// a 401, a 5xx, a transport failure — is a real problem, and falling back on
// it would paper over a broken credential with four project hooks.
func gatedByTier(err error) bool {
	switch Status(err) {
	case http.StatusNotFound, http.StatusForbidden:
		return true
	}
	return false
}

// rollback discards what this run recorded and returns the ORIGINAL failure,
// with the discard's own problem appended, never in place of it: the reason
// the run stopped is what an operator has to fix.
//
// THE SIGNING SECRET IS WHY IT EXISTS. [signingSecret] seals a fresh secret
// before [ensureHooks] writes the hook, so a run that then failed to write the
// hook would leave a new key sealed while GitLab went on signing with the old
// one, and the next pass, resolving the sealed value, would never mint again
// or re-point the hook. Discard on a run that recorded nothing is a no-op by
// contract ([provision.TokenSink]).
func rollback(ctx context.Context, opts Options, cause error) error {
	// DETACHED, because the failure may BE a cancelled context, and a
	// rollback that inherits it does nothing at all.
	if err := opts.Sink.Discard(context.WithoutCancel(ctx)); err != nil {
		return fmt.Errorf("%w (and discarding what this run recorded failed: %w)", cause, err)
	}
	return cause
}

// webhookTarget is the endpoint GitLab delivers to.
func webhookTarget(base string) string {
	if base = strings.TrimRight(strings.TrimSpace(base), "/"); base == "" {
		return ""
	}
	return base + "/webhooks/gitlab"
}
