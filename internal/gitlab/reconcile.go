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
	"github.com/crewlet/crewlet/internal/whsec"

	"github.com/crewlet/crewlet/internal/integration"
)

// Reconcile brings a GitLab instance in line with the company config.
//
// # It is a reconcile, not a setup
//
// Running it twice must be safe and must be quiet: an account that exists is
// found rather than re-created, a membership that holds is left alone. What
// it does do every time is MINT A TOKEN, because a personal access token's
// value is returned once and GitLab will not show it again — so there is no
// "already correct" state to detect, and the honest thing is to rotate and
// say so.
//
// # A run that cannot record what it minted revokes it
//
// Between the third-party app creating a token and the sink recording it there is a
// window where the only copy of a live credential is in this process's
// memory. If recording fails, the token exists, nothing can use it, and
// nobody knows to remove it — so the run revokes what it minted and discards
// what it recorded, and reports both.

// Result is what one reconcile did, for the report.
type Result struct {
	// Created names the seats whose accounts this run created.
	Created []string
	// Rotated names the seats whose tokens this run minted.
	Rotated []string
	// Kept names the seats whose existing token was left alone — the
	// SUCCESSFUL outcome of a re-run, said out loud because a silent
	// report reads as a run that did nothing.
	Kept []string
	// Decommissioned names the accounts this run deleted.
	Decommissioned []string
	// Hooked is the webhook target this run registered or re-pointed, or
	// empty when webhooks were not part of it.
	Hooked string
	// HookedOn names WHERE it was registered: the single element "group",
	// or one entry per project. Reported because the two are not
	// interchangeable — a group hook covers projects added later and a set
	// of project hooks does not, and an operator reading "webhook
	// registered" cannot tell which they got.
	HookedOn []string

	// NoIngress says why this run left the instance with no delivery path
	// at all, and is empty when it registered one.
	//
	// A SEPARATE FIELD rather than an empty [Result.Hooked], for the same
	// reason [jira.Result.NoIngress] is one: an empty Hooked is also what a
	// perfectly healthy run looks like from the outside, and
	// [integration.Classify] over no findings is READY. So a company that
	// never set integrations.public_base_url — which the reconcile loop
	// feeds into every pass — had GitLab reported ready while the instance
	// had nowhere to deliver to and not one merge request, pipeline or
	// comment ever reached a seat. The note below said so to whoever ran
	// the CLI; the dashboard, which is where a running company is watched,
	// was told nothing.
	//
	// The zero value is "nothing to report", deliberately: a Result built
	// anywhere but [Reconcile] must not invent an ingress problem.
	NoIngress string

	// NoKeyring is a pass this node could not run at all because it has
	// nowhere to seal what provisioning creates.
	//
	// A STATE, not an error, and the distinction is what an operator is
	// told: a fault reports the engine working on it and is retried for
	// ever, where this never resolves until somebody sets secrets.keys.
	// See [provision.CanMint].
	NoKeyring bool

	// Unusable names the seats whose ACCOUNT GitLab will not let
	// authenticate — a token minted seconds earlier was refused.
	//
	// SEPARATE FROM A FAILED RUN, because nothing is wrong with the pass and
	// a retry changes nothing: the account needs a person at GitLab. Reported
	// as [integration.FindingIdentityFailed], which is that exact sentence in
	// the neutral vocabulary — "a seat's account could not be created or its
	// credential was refused".
	//
	// It is also the one state in which this pass deliberately leaves a seat
	// with NO credential at all. The alternative is the loop it replaced.
	Unusable []string

	// Notes carries the plan's notes plus anything the run itself found.
	Notes []string

	// Recorded counts the values this run wrote to the sink — seat tokens
	// plus a minted or rotated signing secret.
	//
	// A COUNT rather than the names, because the names are the variables
	// holding live credentials and this number's only job is deciding
	// whether the report tells the operator what still has to happen for
	// those values to reach a running engine. Zero means a re-run that
	// changed nothing, and instructing that operator to restart anything
	// would be noise.
	Recorded int
}

// Mode is where a run's service accounts live.
//
// # Two ownerships, not two capabilities
//
// A GROUP service account is owned by the top-level group the company
// provisions into. It is the default and the only shape GitLab.com offers,
// and a group Owner PAT is enough to create one.
//
// An INSTANCE service account belongs to the instance itself. It is a
// self-managed-only route that needs an instance-admin PAT, and what it buys
// is an account that is not a member of anything until this run adds it — so
// one company's seats can span several top-level groups, and an account
// survives its group being deleted. Everything downstream is identical:
// memberships are added the same way, and the token API is user-scoped on
// both, so a token minted on either kind is minted the same way.
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
	// Client talks to the instance as an administrator.
	Client *Client

	// Config is the company's gitlab block.
	Config *config.GitLab

	// Plan is what to do, from [PlanFor].
	Plan *provision.Plan

	// Sink records what is minted.
	Sink provision.TokenSink

	// WebhookBase is this deployment's public base URL, or empty to skip
	// webhook registration.
	//
	// SKIPPED RATHER THAN GUESSED. A hook pointing at the wrong host is
	// worse than no hook: the instance reports a healthy integration, and
	// the deliveries go somewhere nobody is looking.
	WebhookBase string

	// SigningSecret is the value the hook is registered with, resolved.
	//
	// Empty means the config's ${VAR} answered nothing, and the run MINTS
	// one — see [signingSecret]. GitLab's signing token is
	// caller-supplied and write-only: it is never returned, so a hook
	// registered with an empty one verifies nothing and there is no way
	// to read back what it should have been.
	SigningSecret string

	// SigningSecretVar is the variable the minted secret is recorded
	// under, empty when the config's signing_secret is not a whole ${VAR}
	// reference. Mirrors the seat tokens' mint-into-${VAR} contract: the
	// config's reference is what says where the value belongs.
	SigningSecretVar string

	// Mode is where service accounts are owned — see [Mode]. The zero
	// value is [ModeGroup], which is the only shape GitLab.com has.
	Mode Mode

	// Decommission deletes managed service accounts whose seats have left
	// the config. Off by default: it is the one destructive direction,
	// and a company mid-edit looks exactly like a company that removed a
	// seat.
	Decommission bool

	// Rotate forces a fresh token for every seat, including seats whose
	// current one still works.
	//
	// # Why it is a flag rather than what a run does
	//
	// GitLab returns a personal access token's value once, so a
	// provisioner cannot check that what it recorded last time still
	// matches. The tempting answer is to mint every run — and that is an
	// outage: the engine is running with the OLD value, and rotating
	// revokes the credential every agent is currently authenticating
	// with. An operator adding a tenth seat would take the other nine
	// down, from a command whose whole promise is that it is safe to
	// re-run.
	Rotate bool

	// ExpiryDays is the lifetime minted onto each token, or nil for the
	// instance default. A POINTER because zero is meaningful — it means
	// "send no expiry" — and an int's zero value would silently override
	// every caller that did not set it.
	ExpiryDays *int

	// Now is the clock expiry is computed from. Nil takes the wall clock.
	Now func() time.Time
}

// Reconcile runs one pass.
func Reconcile(ctx context.Context, opts Options) (*Result, error) {
	// A CANCELLED PASS HAS OBSERVED NOTHING, so it must RAISE rather than
	// answer with a Result.
	//
	// Checked here, at the top, because every early return below hands back
	// a Result — and the loop reads a Result with no findings as "this
	// integration is ready" and trusts it for a full settled interval. The
	// reachable shape was an empty plan: [PlanFor] skips every seat whose
	// mcp_env.gitlab token is a literal rather than a ${VAR}, so a company
	// that manages its GitLab credentials by hand plans nothing, and the
	// empty-plan return below never touched ctx at all. A node shutting
	// down would then record GitLab as converged on its way out, having
	// made not one request.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if opts.Client == nil {
		return nil, errors.New("gitlab: no client")
	}
	if opts.Sink == nil {
		// THE COMMAND LINE'S CASE, and it stays a refusal: a run with
		// nowhere to put what it mints would create live credentials and
		// print none of them, which is the worst outcome available.
		return nil, provision.ErrNoSink
	}
	if !provision.CanMint(opts.Sink) {
		// A NODE THAT CANNOT SEAL DOES NOT CREATE. This pass makes an
		// ACCOUNT before it mints a token, and the rollback can only
		// revoke the token — so reaching the first Record on a sink that
		// cannot record leaves an identity at the third-party app that
		// nobody asked for and nothing recorded.
		//
		// REPORTED, NOT RAISED. An error is a fault the loop retries
		// while telling an operator the engine is working on it, which is
		// the one thing that is certainly not happening: no pass will
		// ever succeed until somebody sets secrets.keys. See
		// [Result.Findings].
		return &Result{Notes: notesOf(opts.Plan), NoKeyring: true}, nil
	}
	if opts.Plan == nil || opts.Plan.Empty() {
		return &Result{Notes: notesOf(opts.Plan)}, nil
	}
	if !opts.Mode.Valid() {
		return nil, fmt.Errorf("gitlab: unknown mode %q — it is one of %s",
			opts.Mode, strings.Join(Modes(), ", "))
	}
	p := opts.Config.Provisioning
	// THE GROUP IS RESOLVED IN BOTH MODES. An instance service account is
	// not OWNED by the group, but it is still made a member of it — that
	// is what gives a seat access to the company's repositories — so a
	// group that does not exist is fatal either way, before anything is
	// created.
	group, found, err := opts.Client.GroupByPath(ctx, p.Group)
	if err != nil {
		// GitLab has no separate auth probe, so this first call is where
		// a bad token shows up. Distinguishing a refusal from an
		// unreachable instance here is what stops a revoked token
		// reporting as "the engine is working on it" forever.
		return nil, fmt.Errorf("gitlab: resolve group %q: %w", p.Group,
			integration.Reject(err, Status(err)))
	}
	if !found {
		return nil, fmt.Errorf(
			"gitlab: no group %q on this instance — every seat is made a "+
				"member of it, so it has to exist before seats can be "+
				"provisioned", p.Group)
	}

	res := &Result{Notes: notesOf(opts.Plan)}

	// PROJECTS ARE RESOLVED BEFORE ANYTHING IS MUTATED, and a missing one
	// is dropped rather than fatal — see [resolveProjects].
	projects, notes, err := resolveProjects(ctx, opts.Client, p.Projects)
	if err != nil {
		return nil, err
	}
	res.Notes = append(res.Notes, notes...)

	// AND THE MEMBERSHIPS ARE READ BEFORE ANY OF THEM IS WRITTEN — see
	// [readMemberships]. One listing for the group and one per surviving
	// project, whatever the seat count, replacing a POST per seat and a
	// POST per seat per project on every pass for ever.
	have, err := readMemberships(ctx, opts.Client, group.ID, projects)
	if err != nil {
		return nil, err
	}

	// MINTED IDS ARE TRACKED so a failure can revoke them. Held here
	// rather than looked up on the way out, because the account whose
	// token needs revoking may be one this run just created.
	minted := map[string]mintedToken{}

	for _, seat := range opts.Plan.Seats {
		user, created, err := ensureAccount(ctx, opts, group.ID, seat)
		if err != nil {
			return nil, rollback(ctx, opts, minted,
				fmt.Errorf("gitlab: %s: %w", seat.Handle, err))
		}
		if created {
			res.Created = append(res.Created, seat.Handle)
		}
		level := accessLevel(p, seat.Handle)
		if err = have.ensureGroup(ctx, opts.Client, group.ID, user.ID, level); err != nil {
			return nil, rollback(ctx, opts, minted,
				fmt.Errorf("gitlab: %s: group membership: %w", seat.Handle, err))
		}
		for _, project := range projects {
			if err = have.ensureProject(ctx, opts.Client, project, user.ID, level); err != nil {
				return nil, rollback(ctx, opts, minted,
					fmt.Errorf("gitlab: %s: membership of %s: %w",
						seat.Handle, project, err))
			}
		}

		verdict, held := provision.VerdictRejected, true
		if !created && !opts.Rotate {
			verdict, held, err = credentialFor(ctx, opts, user.ID, seat)
			if err != nil {
				return nil, rollback(ctx, opts, minted,
					fmt.Errorf("gitlab: %s: %w", seat.Handle, err))
			}
		}
		switch verdict {
		case provision.VerdictSelf:
			res.Kept = append(res.Kept, seat.Handle)
			continue
		case provision.VerdictOther:
			// A COPY-PASTED VARIABLE. Minting over it hands this seat a
			// second identity while the other keeps authenticating as
			// one account from two places, and nothing reports it.
			return nil, rollback(ctx, opts, minted, fmt.Errorf(
				"gitlab: %s: %s holds a token that authenticates as a "+
					"different account — give this seat its own variable",
				seat.Handle, seat.TokenVar))
		case provision.VerdictUnknown:
			// LEFT EXACTLY AS IT WAS. Re-minting on "cannot tell"
			// destroys a token that works; the recovery for one that
			// does not is a -rotate away.
			res.Kept = append(res.Kept, seat.Handle)
			res.Notes = append(res.Notes, fmt.Sprintf(
				"%s: could not check whether the token in %s still works, so "+
					"it was left alone — re-run with -rotate if this seat is "+
					"failing to authenticate", seat.Handle, seat.TokenVar))
			continue
		}

		// THROUGH THE GROUP THAT OWNS THE ACCOUNT, which is what a group
		// Owner may do: the instance route is admin only and 403s on
		// gitlab.com for every seat. Zero on the instance path, where the
		// credential is an admin token and no group owns the account.
		token, err := opts.Client.CreateToken(ctx, mintGroup(opts, group.ID), user.ID,
			TokenName(seat.Handle), tokenScopes(p), expiry(opts))
		if err != nil {
			return nil, rollback(ctx, opts, minted,
				fmt.Errorf("gitlab: %s: mint token: %w", seat.Handle, err))
		}
		minted[seat.Handle] = mintedToken{
			// THE GROUP IT WAS MINTED THROUGH, carried rather than
			// recomputed: a rollback that reached for the other route
			// would fail to revoke exactly the credential it just
			// created, which is the one moment a live token is loose.
			groupID: mintGroup(opts, group.ID),
			userID:  user.ID, tokenID: token.ID, createdAccount: created,
		}

		// A TOKEN THIS PASS JUST MINTED IS CHECKED BEFORE IT IS KEPT, and
		// a refusal here means something a rotation can never fix.
		//
		// The mint above is reached because the PREVIOUS credential was
		// refused, and the whole design reads that as "the token is
		// stale" — which is right, except when the account itself cannot
		// authenticate at all. Then the new token is refused for the same
		// reason the old one was, the next pass reads that as stale
		// again, and the run mints for ever: 144 live `api`-scoped tokens
		// over one connect, none of which ever worked. The account's
		// address was unconfirmable (see provision.go), but the loop is
		// not specific to that cause and neither is this guard — any
		// account GitLab will not let authenticate produces it.
		//
		// So the fresh token is the probe. It is a credential that is
		// AS NEW AS ONE CAN BE: if GitLab refuses it, the seat's problem
		// is its account, nothing this pass can do will change that, and
		// minting another is the loop rather than the recovery.
		if verdict := opts.Client.verify(ctx, token.Value, user.ID); verdict == provision.VerdictRejected {
			// SWEPT, NOT LEFT. keep is 0, which no token id ever is, so
			// this retires the one just minted along with every earlier
			// one this tool owns — exactly the pile a previous build of
			// this loop left behind. An administrator's own tokens are
			// named differently and are never touched.
			swept, rerr := retirePrevious(ctx, opts,
				mintGroup(opts, group.ID), user.ID, seat, 0)
			delete(minted, seat.Handle)
			if rerr != nil {
				// A LIVE CREDENTIAL IS LOOSE and the operator has to be
				// told in the strongest terms this pass has.
				return nil, rollback(ctx, opts, minted, fmt.Errorf(
					"gitlab: %s: this account cannot authenticate, and the "+
						"token just minted for it could not be revoked — "+
						"revoke tokens named %q on user %d at GitLab: %w",
					seat.Handle, TokenName(seat.Handle), user.ID, rerr))
			}
			res.Unusable = append(res.Unusable, seat.Handle)
			res.Notes = append(res.Notes, fmt.Sprintf(
				"%s: GitLab refused a token minted seconds earlier, so the "+
					"account itself cannot authenticate — %d token(s) this "+
					"tool had minted for it were revoked and none was kept",
				seat.Handle, swept))
			continue
		}

		// RECORDED IMMEDIATELY. The value above is the only copy there
		// will ever be.
		if err = opts.Sink.Record(ctx, seat.TokenVar, token.Value); err != nil {
			return nil, rollback(ctx, opts, minted,
				fmt.Errorf("gitlab: %s: record %s: %w", seat.Handle, seat.TokenVar, err))
		}
		res.Rotated = append(res.Rotated, seat.Handle)
		res.Recorded++
		if created {
			continue
		}
		// RETIRED AFTER THE RECORD, and only this tool's own: an
		// administrator may have minted a token on this account by hand,
		// and revoking it would break whatever is using it — silently,
		// since nothing here knows what that is.
		retired, err := retirePrevious(ctx, opts, mintGroup(opts, group.ID), user.ID, seat, token.ID)
		if err != nil {
			return nil, rollback(ctx, opts, minted,
				fmt.Errorf("gitlab: %s: %w", seat.Handle, err))
		}
		if !held && retired > 0 {
			// THE SURPRISING CASE, and the only one that earns a note:
			// the operator asked for nothing, but the variable was empty
			// on this machine while a live token existed on the account
			// — so a rotation happened anyway, and whatever is running
			// with the old value is now failing to authenticate.
			res.Notes = append(res.Notes, fmt.Sprintf(
				"%s: the account held a working token but %s did not, so a "+
					"fresh one was minted and the old one retired — a running "+
					"engine holding the old value has to be restarted",
				seat.Handle, seat.TokenVar))
		}
	}

	if opts.Decommission {
		removed, notes, err := decommission(ctx, opts, group.ID, have.group)
		if err != nil {
			return nil, rollback(ctx, opts, minted, err)
		}
		res.Decommissioned = removed
		res.Notes = append(res.Notes, notes...)
	}

	if target := webhookTarget(opts.WebhookBase); target != "" {
		// freshSecret is this run's own record of whether it minted or
		// rotated the key, and all it drives now is the Recorded count.
		// It used to force the hook re-write as well, because GitLab
		// never returns a signing token and nothing else could tell a
		// hook carrying the current key from one carrying the value this
		// run had just replaced. [HookDigest] answers that from the
		// instance's own listing, for every key change rather than only
		// the ones this process made, so the flag stops here.
		secret, note, freshSecret, err := signingSecret(ctx, opts)
		if err != nil {
			return nil, rollback(ctx, opts, minted, err)
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
			return nil, rollback(ctx, opts, minted, err)
		}
		res.Hooked, res.HookedOn = target, hooked
		res.Notes = append(res.Notes, notes...)
	} else {
		// REPORTED, not just noted. The note is what the CLI prints and it
		// already said the right words — "the integration looks idle rather
		// than unconfigured" — about a pass whose own answer then made it
		// look exactly that way: no findings, so [integration.Classify]
		// reported READY on a company whose GitLab delivers nothing. See
		// [Result.NoIngress].
		res.NoIngress = "no webhook is registered on this GitLab, so merge " +
			"requests, pipelines, issues and comments reach no seat: set " +
			"integrations.public_base_url to this deployment's public address " +
			"and the next pass registers one, or register it by hand"
		res.Notes = append(res.Notes, res.NoIngress)
	}

	if err := opts.Sink.Flush(ctx); err != nil {
		return nil, rollback(ctx, opts, minted, err)
	}
	return res, nil
}

// memberships is what the instance already has, read once per pass.
//
// # Why the pass reads before it writes
//
// It did not. Every seat got a POST to the group's members and a POST to
// every project's, on every pass, and GitLab answers a second one with a 409
// that [Client.AddGroupMember] treats as success. The loop runs every few
// minutes for the life of the deployment, so a converged company of ten seats
// and four projects sent fifty writes to somebody's GitLab for ever — and the
// contract this pass is certified against ([integration.Reconciler]) says a
// converged pass writes nothing, because that is what makes the loop safe to
// leave switched on.
//
// It was also the whole of the access-level bug. Adding was the only thing
// the pass could do and 409 was read as done, so editing
// provisioning.access_level or an access_levels override changed nothing for
// a seat that already had a membership: the company document said maintainer,
// the instance kept developer, and every pass reported converged. Reading
// first is what makes the difference visible, and [Client.SetGroupMember] is
// what acts on it — which is why this is the root-cause fix rather than the
// cheap way to make a write counter read zero.
type memberships struct {
	// group is the company's top-level group's roster, IN THE INSTANCE'S
	// OWN ORDER, because a decommission sweep reports in that order and a
	// map's iteration would make one pass's report differ from the next's
	// over an unchanged group.
	group []Member
	// groupLevel indexes that roster by user id, which is the question the
	// seat loop asks: at what level, if at all, is this account a member.
	groupLevel map[int]int
	// projectLevel is the same index per `provisioning.projects` entry.
	// No roster beside it: nothing decommissions a project membership.
	projectLevel map[string]map[int]int
}

// readMemberships lists the group's members and each project's, once.
//
// A FAILURE IS FATAL, unlike a missing project. [resolveProjects] drops a
// project the instance does not have because a config naming a repository
// that was renamed is an ordinary state; a listing that is REFUSED is the
// pass being unable to see what it is about to change, and guessing from
// there is what produced the fifty-writes-a-pass behaviour this replaces.
func readMemberships(ctx context.Context, c *Client, groupID int, projects []string) (*memberships, error) {
	members, err := c.GroupMembers(ctx, groupID)
	if err != nil {
		return nil, fmt.Errorf("gitlab: list group members: %w", err)
	}
	have := &memberships{
		group:        members,
		groupLevel:   make(map[int]int, len(members)),
		projectLevel: make(map[string]map[int]int, len(projects)),
	}
	for _, member := range members {
		have.groupLevel[member.ID] = member.AccessLevel
	}
	for _, project := range projects {
		members, err := c.ProjectMembers(ctx, project)
		if err != nil {
			return nil, fmt.Errorf("gitlab: list %s's members: %w", project, err)
		}
		levels := make(map[int]int, len(members))
		for _, member := range members {
			levels[member.ID] = member.AccessLevel
		}
		have.projectLevel[project] = levels
	}
	return have, nil
}

// ensureGroup brings one seat's group membership to the configured level.
//
// THREE OUTCOMES, and only two of them write: absent is added, present at
// another level is moved, present at the right level is left entirely alone.
// The third is the steady state and it is the one the loop spends its life
// in.
func (m *memberships) ensureGroup(ctx context.Context, c *Client, groupID, userID, level int) error {
	switch current, member := m.groupLevel[userID]; {
	case !member:
		if err := c.AddGroupMember(ctx, groupID, userID, level); err != nil {
			return err
		}
	case current != level:
		if err := c.SetGroupMember(ctx, groupID, userID, level); err != nil {
			return err
		}
	default:
		return nil
	}
	// RECORDED SO THE PICTURE STAYS IN STEP with the instance for the rest
	// of the pass. Nothing here reads it twice today; a second reader that
	// appeared and did not see this run's own writes would repeat them.
	m.groupLevel[userID] = level
	return nil
}

// ensureProject is the same decision for one project.
func (m *memberships) ensureProject(ctx context.Context, c *Client, project string, userID, level int) error {
	levels := m.projectLevel[project]
	switch current, member := levels[userID]; {
	case !member:
		if err := c.AddProjectMember(ctx, project, userID, level); err != nil {
			return err
		}
	case current != level:
		if err := c.SetProjectMember(ctx, project, userID, level); err != nil {
			return err
		}
	default:
		return nil
	}
	if levels == nil {
		levels = map[int]int{}
		m.projectLevel[project] = levels
	}
	levels[userID] = level
	return nil
}

// mintedToken is one credential this run created, as its rollback needs it.
type mintedToken struct {
	// groupID is the group the token was minted THROUGH, or zero for the
	// instance route. Carried rather than recomputed: a rollback reaching
	// for the other route would fail to revoke exactly the credential it
	// just created, which is the one moment a live token is loose.
	groupID int
	userID  int
	tokenID int
	// createdAccount says the account is this run's, which decides HOW
	// the rollback undoes it: everything on an account nothing else ever
	// minted on, versus exactly the one token on an account that already
	// existed. Sweeping the second would take an administrator's own
	// token with no way to tell that it had.
	createdAccount bool
}

// TokenName is the name this tool mints under.
//
// It is the ONLY thing distinguishing a token this tool owns from one an
// administrator created by hand, and both the keep-or-mint decision and the
// retire step key on it — so it is a named function rather than a format
// string repeated at three call sites, where the three would eventually
// differ and rotation would quietly stop retiring anything.
func TokenName(handle string) string { return "crewlet-" + handle }

// credentialFor decides whether this seat already has a working token.
//
// # It PROVES it, rather than inferring it
//
// The weaker test — "the variable has a value and the account has some
// token" — reads as provisioned in exactly the case that matters: an
// operator who restored an older env file has a stale value sitting beside
// a live token that is not it, and the seat then authenticates with
// nothing, on every run, forever. So the run takes the value the variable
// actually holds and asks the instance who it is.
func credentialFor(ctx context.Context, opts Options, userID int, seat provision.Seat) (provision.Verdict, bool, error) {
	value, held, err := opts.Sink.Value(ctx, seat.TokenVar)
	if err != nil {
		// UNREADABLE IS NOT ABSENT. Treating it as absent would rotate
		// every live credential in the company because a store blinked.
		return provision.VerdictUnknown, false, fmt.Errorf(
			"cannot read %s, and guessing would either rotate a live token or "+
				"leave a seat with none: %w", seat.TokenVar, err)
	}
	if !held {
		return provision.VerdictRejected, false, nil
	}
	return opts.Client.verify(ctx, value, userID), true, nil
}

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

// retirePrevious revokes this tool's earlier tokens on an existing account.
func retirePrevious(
	ctx context.Context, opts Options, groupID, userID int, seat provision.Seat, keep int,
) (int, error) {
	tokens, err := opts.Client.Tokens(ctx, groupID, userID)
	if err != nil {
		return 0, fmt.Errorf("list tokens: %w", err)
	}
	retired := 0
	for _, token := range tokens {
		// ALREADY-REVOKED ROWS ARE SKIPPED: revoking one again changes
		// nothing and every rotation leaves another behind, so a run
		// without this issues one more pointless request than the run
		// before it, for ever.
		if token.ID == keep || token.Revoked || token.Name != TokenName(seat.Handle) {
			continue
		}
		if err := opts.Client.RevokeToken(ctx, groupID, userID, token.ID); err != nil {
			return retired, fmt.Errorf("revoke the previous token: %w", err)
		}
		retired++
	}
	return retired, nil
}

// expiry is the date a minted token dies, or the zero time for the
// instance default.
//
// NO EXPIRY BY DEFAULT, deliberately: nothing in Crewlet renews a
// credential on a schedule, so a lifetime nobody renews is an outage with a
// date on it. GitLab.com caps personal access tokens at a year regardless,
// which is the instance enforcing its own policy rather than this tool
// choosing one — and a company whose policy needs a shorter window sets
// -token-expiry-days and owns the re-run.
func expiry(opts Options) time.Time {
	if opts.ExpiryDays == nil || *opts.ExpiryDays <= 0 {
		return time.Time{}
	}
	return clock(opts).AddDate(0, 0, *opts.ExpiryDays)
}

func clock(opts Options) time.Time {
	if opts.Now != nil {
		return opts.Now()
	}
	return time.Now()
}

// ensureAccount finds or creates a seat's service account.
//
// THE LOOKUP IS MODE-INDEPENDENT: /users?username= sees every account on the
// instance whatever owns it, which is what makes switching modes safe — an
// operator who moves a company from group to instance finds the accounts it
// already has rather than colliding with their usernames.
func ensureAccount(ctx context.Context, opts Options, groupID int,
	seat provision.Seat,
) (User, bool, error) {
	p := opts.Config.Provisioning
	username := Username(p, seat.Handle)
	user, found, err := opts.Client.UserByUsername(ctx, username)
	if err != nil {
		return User{}, false, err
	}
	if found {
		return user, false, nil
	}
	if opts.Mode.Or() == ModeInstance {
		user, err = opts.Client.CreateInstanceServiceAccount(ctx, seat.Role, username)
	} else {
		user, err = opts.Client.CreateServiceAccount(ctx, groupID, seat.Role, username)
	}
	if err != nil {
		return User{}, false, modeError(opts.Mode, err)
	}
	return user, true, nil
}

// mintGroup is the group a token is minted through, or zero for the instance
// path.
//
// THE SAME SPLIT [ensureAccount] MAKES, and it has to be: an account created
// through the group route is owned by that group and its tokens are minted
// there, while an instance service account belongs to nobody and takes the
// admin route. Reading the mode once in each place is what keeps a run from
// creating an account one way and reaching for its tokens the other.
func mintGroup(opts Options, groupID int) int {
	if opts.Mode.Or() == ModeInstance {
		return 0
	}
	return groupID
}

// ErrNameReserved reports a username or email GitLab is still releasing from
// an account it is deleting.
//
// A STATE THAT CLEARS ITSELF, and the whole reason it is named: the pass has
// nothing to fix and nobody to tell, it simply has to be run again once
// GitLab's own deletion finishes. The caller reports it as work in progress
// rather than as a failure to read the integration.
var ErrNameReserved = errors.New(
	"gitlab is still releasing the name of an account it is deleting")

// modeError turns a refusal into the sentence that names the credential the
// chosen mode actually needs.
//
// A 403 IS UNREADABLE ON ITS OWN here: group creation is satisfied by a group
// Owner and instance creation is not, so the same status means two different
// remedies and an operator reading "403 Forbidden" has no way to tell which
// they hit. A 404 on the instance route means the same thing a third way —
// GitLab.com does not serve it at all.
func modeError(mode Mode, err error) error {
	var api *APIError
	if !errors.As(err, &api) {
		return err
	}
	// A NAME GITLAB HAS NOT RELEASED YET, which is a deletion still running
	// rather than anything wrong with this run.
	//
	// GitLab removes a user asynchronously: the account is gone from every
	// listing the moment the delete is accepted, and its username and email
	// stay reserved until a background job finishes. So a disconnect
	// followed by a reconnect inside that window looks up the account,
	// honestly does not find it, creates one, and is refused with "has
	// already been taken".
	//
	// Reported as an error, that reads as "the last pass could not read this
	// integration", which sends an operator looking for an outage over a
	// state that clears itself on the next tick. See [ErrNameReserved].
	if api.Status == http.StatusBadRequest && strings.Contains(api.Detail, "already been taken") {
		return fmt.Errorf("%w: %w", ErrNameReserved, err)
	}
	if mode.Or() != ModeInstance {
		if api.Forbidden() {
			return fmt.Errorf(
				"%w — creating a group service account needs a token whose "+
					"owner is an Owner of provisioning.group (or an instance "+
					"administrator)", err)
		}
		return err
	}
	switch {
	case api.Forbidden():
		return fmt.Errorf(
			"%w — -mode instance creates service accounts the instance owns, "+
				"which only an INSTANCE ADMINISTRATOR may do. Use an admin "+
				"PAT, or drop -mode instance to create them under "+
				"provisioning.group instead", err)
	case api.Status == http.StatusNotFound:
		return fmt.Errorf(
			"%w — this deployment does not serve the instance service-account "+
				"route, which is how GitLab.com answers: instance service "+
				"accounts are self-managed only. Drop -mode instance", err)
	}
	return err
}

// decommission deletes the managed accounts whose seats have left.
//
// # The prefix and the group are both the safety property
//
// "Managed" means an account whose username starts with the configured
// prefix AND which is a member of the group this company provisions into.
// Either alone is too broad: a prefix matches whatever an operator chose to
// call things elsewhere on the instance, and a group holds people. The
// prefix can never be empty — [Username] defaults one — and that default is
// what stops an unscoped sweep.
//
// A group member the instance refuses to delete because it is not a service
// account is left alone and reported: that refusal is GitLab catching what
// this scan should not have proposed, so it is a signal about the prefix
// rather than an error to abort on.
//
// # The GROUP scan is used in both modes, and that is deliberate
//
// An instance service account is not owned by the group, so the obvious
// enumeration for -mode instance is the instance's own service-account
// listing. It is the wrong one: every seat this run provisions is made a
// member of provisioning.group whatever mode created it, so a managed
// account of THIS company is always in that group — while the instance
// listing also holds the accounts of every other company on the box, which a
// shared username prefix would then sweep. The only account the group scan
// misses is one somebody removed from the group by hand, and leaving that
// alone is the right answer. Only the DELETE route differs by mode.
//
// # It scans the roster the pass already read
//
// members comes from [readMemberships], taken BEFORE the seat loop, so the
// whole pass acts on ONE picture of the group's membership rather than two
// that can disagree — and a decommission costs no second walk of a group that
// may run to thousands of rows. It cannot miss a candidate: nothing between
// the two points removes a member, and the only additions are this run's own
// seats, every one of which is in the plan and therefore in `keep`.
func decommission(ctx context.Context, opts Options, groupID int, members []Member) ([]string, []string, error) {
	p := opts.Config.Provisioning
	prefix := strings.ToLower(Username(p, ""))
	keep := make(map[string]bool, len(opts.Plan.Seats))
	for _, seat := range opts.Plan.Seats {
		keep[strings.ToLower(Username(p, seat.Handle))] = true
	}
	var removed, notes []string
	for _, member := range members {
		username := strings.ToLower(member.Username)
		if !strings.HasPrefix(username, prefix) || keep[username] {
			continue
		}
		if err := deleteAccount(ctx, opts, groupID, member.ID); err != nil {
			var api *APIError
			if errors.As(err, &api) && api.Status == http.StatusBadRequest {
				notes = append(notes, fmt.Sprintf(
					"%s matches the managed prefix but the instance refuses "+
						"to delete it, which is what it does for an account "+
						"that is not a service account — check that "+
						"provisioning.username_prefix is not catching people",
					member.Username))
				continue
			}
			return removed, notes, fmt.Errorf("gitlab: decommission %s: %w",
				member.Username, err)
		}
		removed = append(removed, member.Username)
	}
	return removed, notes, nil
}

// signingSecret is what the hook is registered with, minting one when the
// config's reference answered nothing.
//
// # Why an empty one cannot just be registered
//
// GitLab's signing token is CALLER-SUPPLIED and write-only: the instance
// never returns it. A hook registered with an empty one is accepted, shows
// as healthy in the settings page, and signs every delivery with nothing —
// which the engine then refuses. Measured against a real instance: the
// issue was created, the hook fired, and the only trace was one
// `webhook_signature_invalid` line in a log nobody was watching.
//
// So an empty resolution is not passed through. It is either minted — into
// the variable the config's ${VAR} names, the same mint-into-${VAR} contract
// the seat tokens follow — or refused, because a literal has nowhere to
// record it and half-configuring is the failure above.
// The third result says a NEW secret exists as of this run — minted or
// rotated. It counts towards [Result.Recorded] AND it is what tells
// [ensureHooks] to write a hook that already reports a signing token, because
// GitLab never says WHICH key a hook holds, so the run's own record is the
// only thing that can.
func signingSecret(ctx context.Context, opts Options) (secret, note string, fresh bool, err error) {
	plan := PlanSigningSecret(opts.SigningSecret, opts.SigningSecretVar, opts.Rotate, true)
	switch plan.Action {
	case SigningReuse:
		return strings.TrimSpace(opts.SigningSecret), plan.Note, false, nil
	case SigningBlocked:
		return "", "", false, errors.New("gitlab: " + plan.Note)
	}
	secret, err = whsec.Mint()
	if err != nil {
		return "", "", false, err
	}
	if err := opts.Sink.Record(ctx, plan.Var, secret); err != nil {
		return "", "", false, fmt.Errorf("gitlab: record %s: %w", plan.Var, err)
	}
	return secret, plan.Note + " — " + opts.Sink.NextStep(), true, nil
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
// the whole ${VAR} it references (empty when it is a literal), and
// registeringHooks whether this run has a public URL to point a hook at.
func PlanSigningSecret(secret, varName string, rotate, registeringHooks bool) SigningPlan {
	if !registeringHooks {
		return SigningPlan{Action: SigningUntouched}
	}
	// -rotate REACHES THE SIGNING SECRET, not just the seat tokens.
	//
	// It has to. Until the provisioning fix, the minted key went into
	// GitLab's plaintext `token` attribute, so the instance echoed it back
	// in cleartext on every delivery — into request logs, into any proxy in
	// front of the engine, and into the stored delivery headers. Every key
	// installed by an older Crewlet is therefore compromised, and a
	// provisioner that could not replace one would leave the operator
	// editing environment variables by hand to recover.
	//
	// Deliberately gated on the flag rather than done on every run: minting
	// a signing secret every time would re-point the hook at a key the
	// RUNNING engine does not have yet, refusing every delivery until the
	// new value is sourced and the process restarted — the same outage the
	// seat tokens' Rotate doc explains at length.
	rotating := rotate && varName != ""
	if trimmed := strings.TrimSpace(secret); trimmed != "" && !rotating {
		if rotate {
			// REPORTED, NOT REFUSED. -rotate is about the seat tokens, and
			// failing the whole run would stop an operator rotating those
			// because of a signing secret they manage by hand. They do
			// need to hear it, though — so it is a note on a run that
			// otherwise succeeded rather than silence.
			return SigningPlan{Action: SigningReuse, Note: "the webhook signing " +
				"secret was left alone: integrations.gitlab.signing_secret is a " +
				"literal, so there is nowhere to record a new one. Replace it by " +
				"hand and re-run — a key installed before the signing_token fix " +
				"was echoed back in cleartext on every delivery, so it is " +
				"compromised"}
		}
		return SigningPlan{Action: SigningReuse}
	}
	if varName == "" {
		return SigningPlan{Action: SigningBlocked, Note: "integrations.gitlab." +
			"signing_secret resolved to nothing and is not a whole ${VAR} " +
			"reference, so there is nowhere to record a minted one. Point it at " +
			"a variable, export a whsec_ value yourself, or clear both -public-url " +
			"and integrations.public_base_url and " +
			"register the hook by hand"}
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
		return "webhook signing secret: untouched (no -public-url, so no hook is registered)"
	case SigningReuse:
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

// ensureHooks registers this deployment's webhook, at ONE level.
//
// # One level, never both
//
// A group hook already fires for every issue, merge request, note and
// pipeline event in every project of the group and its subgroups. GitLab is
// explicit that a group hook and a project hook subscribed to the same
// events BOTH fire for an in-project event — double delivery, which the
// engine's ledger deduplicates and its inbox does not. So this picks a
// level and registers there.
//
// # Why there is a choice at all
//
// THE GROUP HOOKS API IS NOT EVERYWHERE. It is a Premium feature on
// gitlab.com and it does not exist in Community Edition at all, and GitLab
// hides an unavailable endpoint as a 404 rather than a 402 — so "not found"
// is what an instance says about a feature it does not serve. Registering
// only at the group level therefore failed the whole reconcile there, and
// failed it AFTER minting, so the rollback revoked every credential the run
// had just created.
//
// MEASURED, because the obvious guess is wrong in both directions: the
// unlicensed `gitlab-ee` image this repository's compose stack runs (19.3.0,
// no license) DOES serve `GET /groups/:id/hooks`, so the local loop takes
// the group path and never exercises the fallback. Reach for `false` to
// exercise it, and do not assume "unlicensed" means "no group hooks".
//
// Modes come from provisioning.group_webhook — auto (default) tries the
// group and falls back, true demands the group, false goes straight to the
// projects.
//
// A hook that already carries the right subscription is left alone
// ([Hook.Converged]), and "the right subscription" now includes WHICH KEY it
// is signed with: [HookDigest] publishes that in the one field this engine
// controls and GitLab gives back. Nothing about the key is passed down here.
// It used to be — a `freshSecret` flag saying whether THIS RUN minted or
// rotated the secret — which was the strongest fact available while the
// listing could only say that some token was set, and was silently blind to
// every rotation another process made: `crewlet secrets set`, the setup
// form's signing-secret field, a peer's apply. The digest is read off the
// same listing every other converge test is read off, so there is one answer
// rather than a run-local one beside an instance-side one.
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
	// The fallback below turns on the create call FAILING, and on
	// gitlab.com's free tier it does not fail: POST /groups/:id/hooks
	// answers 201, the hook is listed in the group's settings, and its own
	// event log stays empty for ever. Measured on a live free group, where
	// the pass reported ready and not one delivery had ever arrived.
	//
	// So the tier is READ rather than inferred from a refusal. Only
	// gitlab.com answers with a plan at all, and [Group.PaidPlan] reads
	// silence as "cannot tell": a self-managed instance keeps the behaviour
	// it has, and the one case caught is a group that says it is free.
	if mode == config.ContainerWebhookAuto && !group.PaidPlan() {
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
		err := ensureGroupHook(ctx, opts.Client, group.ID, name, target, secret)
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
// THE OTHER HALF OF [sweepGroupHooks], and the direction that was written
// down as permanent: "if a prior run created per-project hooks and a later run
// establishes a group hook, the reconcile does not remove the old per-project
// hooks — you would get double delivery until you delete them. Deleting a
// redundant project hook is a manual step." It is not manual now. A group
// hook fires for every event in every project of the group, so a project hook
// beside it delivers each one twice, and this engine registered both.
//
// The declared projects only, which is every project this engine has ever
// hooked: [ensureProjectHooks] refuses to run at all without
// `provisioning.projects`, so there is no project outside the list carrying a
// registration of ours. A project dropped from the list keeps its hook, which
// is the same answer [resolveProjects] gives about membership and for the
// same reason — a company mid-edit looks exactly like one that removed a
// project.
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
// THE LEVEL IS A CHOICE THAT MOVES. `group_webhook` is a live config field
// and the `auto` answer depends on the group's PLAN, so a company that flips
// the field — or whose paid group lapses — has the engine register per
// project from then on and never look at the level it stopped writing. The
// group hook goes on delivering every one of those events a second time, for
// ever, and no pass has any reason to mention it: [Teardown] sweeps both
// levels for exactly this reason and the reconcile did not.
//
// A TIER GATE IS NOT A FAILURE. Group hooks are a Premium feature and the
// endpoint 404s where the tier does not serve it — which is most of the
// instances that take this path at all — so that answer means there is
// nothing to sweep rather than that the sweep failed. See [gatedByTier].
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
// # A missing one is dropped, not fatal
//
// `provisioning.projects` names a company's real repositories, and one of
// them being renamed, moved or not created yet is an ordinary state of a
// config — not a reason to refuse to provision the other nine. Aborting on
// the first 404 did exactly that, and it aborted MID-LOOP, after minting, so
// the rollback then revoked the credentials the run had already created.
//
// It runs BEFORE any mutation for the same reason the group is resolved up
// front: a check that happens halfway through leaves half a reconcile
// behind, and the operator's fix — create the project, re-run — is what the
// note tells them to do.
//
// It is the opposite call from a knowledge-base importer, deliberately.
// There a missing container aborts before a single page is written, because
// half an import looks like a complete knowledge base with holes in it.
// Memberships and hooks are additive and independent: the seats that could
// be added were added, and re-running adds the rest.
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
// THE NAME IS THE IDENTITY, because it is the only field that survives a
// change of public base. Matching on the URL instead meant a deployment that
// moved created a second hook and left the first: one live orphan per change,
// at every level — the group hook and one per project — each still enabled,
// each still signed, each delivering to an address that no longer answers.
// Measured on a real deployment behind a tunnel: a group hook and two project
// hooks, all pointing at dead addresses, none of which any pass could see.
//
// THE DELIVERY PATH IS THE GUARD that the old comment's concern deserves. It
// said the URL was matched "because an instance may carry hooks somebody else
// registered", which is a real risk and the wrong answer to it: every hook
// this engine registers ends in /webhooks/gitlab whatever base it was
// registered against, so a hook that merely shares the name and points
// somewhere else is not this engine's and is left alone. That is the whole of
// what the URL match was protecting, kept without the orphans.
//
// A HOOK WITH NO NAME IS ADOPTED, and that is the one arm worth arguing.
// GitLab has taken a name since 17.1 and this engine never sent one, so every
// hook it has ever registered is nameless — including the orphans this change
// exists to sweep up. Refusing to touch them would leave exactly those behind
// for ever, which is the bug rather than the fix. What it costs is the case
// the name is there to settle: two deployments of one company on one instance,
// BOTH still nameless, where the first pass after this change adopts whatever
// it finds, keeps one and removes the rest. That resolves itself on the
// following pass — the survivor now carries a name, the other deployment
// re-creates its own under its own name, and from then on neither can see the
// other's. One flap, once, against orphans that otherwise accumulate for ever.
//
// Two DEPLOYMENTS watching one instance is what the name settles from then
// on, because they share this document: they set
// `integrations.gitlab.webhook_name` to two values, exactly as they would
// Jira's or Datadog's.
func ours(hook Hook, name string) bool {
	if !strings.HasSuffix(hook.URL, webhookPath) {
		return false
	}
	return hook.Name == name || hook.Name == ""
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
			// confirmation.
			//
			// This used to PUT unconditionally, on the reasoning that
			// the signing secret may have rotated — true, and now
			// answered by the digest [hookBody] wrote into the hook's
			// description. What the unconditional write cost was the
			// steady state: the loop re-sent this identical body, all
			// nineteen event flags of it, every few minutes for the life
			// of the deployment.
			//
			// No re-read either. [confirmSigned] exists because a GitLab
			// older than 19.1 takes a signing token, ignores it and
			// answers 200 — a claim about a WRITE. Nothing was written,
			// and [Hook.Converged] already required the instance to
			// report a signing token on this very listing, which is the
			// same fact confirmSigned would go back for.
			return nil
		}
		if err := c.UpdateGroupHook(ctx, groupID, hook.ID, name, target, secret); err != nil {
			return fmt.Errorf("gitlab: update group hook: %w", err)
		}
		return confirmSigned("group hook", func() ([]Hook, error) {
			return c.GroupHooks(ctx, groupID)
		}, target)
	}
	if _, err := c.CreateGroupHook(ctx, groupID, name, target, secret); err != nil {
		return fmt.Errorf("gitlab: create group hook: %w", err)
	}
	return confirmSigned("group hook", func() ([]Hook, error) {
		return c.GroupHooks(ctx, groupID)
	}, target)
}

// confirmSigned reads the hook back and refuses to call the run a success
// unless GitLab says it now holds a signing token.
//
// THE WRITE SUCCEEDING PROVES NOTHING. `signing_token` arrived in GitLab
// 19.0 and went generally available in 19.1; an older instance takes the
// attribute, ignores it, and answers 200. The hook then exists, GitLab's
// settings page calls it healthy, and it delivers unsigned to an engine
// whose verification is mandatory — so every delivery is refused, and the
// only place that could have said why is this function.
//
// `signing_token_present` is the only thing GitLab will say about it: the
// token itself is never returned. That is enough, because what is being
// confirmed is that a signing token EXISTS, not which one.
func confirmSigned(what string, list func() ([]Hook, error), target string) error {
	hooks, err := list()
	if err != nil {
		return fmt.Errorf("gitlab: re-read the %s to confirm it can sign: %w", what, err)
	}
	for _, hook := range hooks {
		if hook.URL != target {
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
	return fmt.Errorf("gitlab: the %s at %s is not there after writing it", what, target)
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
		return confirmSigned("hook on "+project, func() ([]Hook, error) {
			return c.ProjectHooks(ctx, project)
		}, target)
	}
	if _, err := c.CreateProjectHook(ctx, project, name, target, secret); err != nil {
		return fmt.Errorf("gitlab: create hook on %s: %w", project, err)
	}
	return confirmSigned("hook on "+project, func() ([]Hook, error) {
		return c.ProjectHooks(ctx, project)
	}, target)
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

// rollback revokes what this run minted and clears what it recorded.
//
// It returns the ORIGINAL failure with the cleanup's own problems appended,
// never in place of it: the reason the run stopped is what an operator has
// to fix, and a cleanup error that replaced it would hide the cause behind
// its consequence.
func rollback(ctx context.Context, opts Options, minted map[string]mintedToken, cause error) error {
	// DETACHED, because the failure may BE a cancelled context — and a
	// rollback that inherits it does nothing at all, leaving every minted
	// credential live.
	ctx = context.WithoutCancel(ctx)
	var problems []string
	for handle, m := range minted {
		var err error
		if m.createdAccount {
			// Nothing else has ever minted on an account this run made,
			// so taking everything takes exactly what this run caused.
			err = opts.Client.RevokeTokens(ctx, m.groupID, m.userID)
		} else {
			err = opts.Client.RevokeToken(ctx, m.groupID, m.userID, m.tokenID)
		}
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", handle, err))
		}
	}
	// DISCARDED WHETHER OR NOT A SEAT TOKEN WAS MINTED, which is the whole
	// of what this used to get wrong: it returned early on an empty `minted`
	// map, and the seat tokens are not the only thing a pass records.
	//
	// [signingSecret] MINTS A WEBHOOK SIGNING SECRET and Records it before
	// [ensureHooks] runs. So a pass over a company whose seats all Kept
	// their tokens — the steady state, every few minutes, for ever — that
	// then failed to write the hook left a FRESH secret sealed in the
	// fleet's store while GitLab went on signing with the old one. Nothing
	// ever recovered: the next pass resolves that sealed value, sees a
	// non-empty secret and takes [SigningReuse], so it never mints again
	// and never re-points the hook. Every delivery after the next config
	// apply fails verification, from a pass that reported an error once and
	// then looked healthy.
	//
	// Discard on a run that recorded nothing is a no-op by contract
	// ([provision.TokenSink]), so this costs a run that failed before its
	// first Record exactly nothing.
	if err := opts.Sink.Discard(ctx); err != nil {
		problems = append(problems, err.Error())
	}
	if len(problems) == 0 {
		if len(minted) == 0 {
			return cause
		}
		return fmt.Errorf("%w (the %d token(s) this run minted were revoked)",
			cause, len(minted))
	}
	return fmt.Errorf("%w\n\nAND THE CLEANUP DID NOT FINISH — these credentials "+
		"may still be live and must be revoked by hand:\n  %s",
		cause, strings.Join(problems, "\n  "))
}

// webhookTarget is the endpoint GitLab delivers to.
func webhookTarget(base string) string {
	if base = strings.TrimRight(strings.TrimSpace(base), "/"); base == "" {
		return ""
	}
	return base + "/webhooks/gitlab"
}

// accessLevel is a seat's membership level, as GitLab's numbers.
func accessLevel(p *config.GitLabProvisioning, handle string) int {
	level := p.AccessLevel
	if override, ok := p.AccessLevels[handle]; ok {
		level = override
	}
	if level == config.GitLabMaintainer {
		return gitlabMaintainer
	}
	return gitlabDeveloper
}

// GitLab's access levels, which the API takes as integers.
const (
	gitlabDeveloper  = 30
	gitlabMaintainer = 40
)

// tokenScopes are the scopes minted on a seat's token.
func tokenScopes(p *config.GitLabProvisioning) []string {
	if len(p.TokenScopes) > 0 {
		return p.TokenScopes
	}
	// `api` is what an agent needs to comment, review and push through the
	// MCP surface. It is broad, which is why it is a config field: a
	// company that runs read-only agents narrows it there, and one that
	// says nothing gets the scope its agents will actually use rather than
	// a run that succeeds and produces tokens nothing can act with.
	return []string{"api"}
}

func notesOf(p *provision.Plan) []string {
	if p == nil {
		return nil
	}
	return p.Notes
}

// deleteAccount removes a service account by whichever route owns it.
//
// The routes are not interchangeable: the group route 404s on an account the
// instance owns, and the instance route needs an administrator. Sending a
// delete down the wrong one reports "already gone" for an account that is
// still live — a decommission that silently kept every credential it was
// asked to destroy.
func deleteAccount(ctx context.Context, opts Options, groupID, userID int) error {
	if opts.Mode.Or() == ModeInstance {
		return opts.Client.DeleteInstanceServiceAccount(ctx, userID)
	}
	return opts.Client.DeleteServiceAccount(ctx, groupID, userID)
}
