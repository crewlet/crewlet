package mattermost

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
)

// Result is what one pass over the company's half of Mattermost found.
type Result struct {
	// Renamed names the bots whose display name was brought back in line
	// with the company document.
	Renamed []string

	// NotAdmin is the sentence naming a provisioning account this run
	// CONFIRMED holds no system_admin role. Empty when it holds one, and
	// empty when this run could not ask: "cannot tell" must never report
	// the same as "confirmed wrong". See [preflight].
	NotAdmin string
	// Disabled names the instance settings this run read as an explicit
	// false, each one something an administrator has to switch on before
	// any of this works.
	Disabled []DisabledSetting

	Notes []string
}

// Options are one pass's inputs.
type Options struct {
	// Client talks to the instance as a system administrator.
	Client *Client

	// Config is the company's mattermost block.
	Config *config.Mattermost

	// Org is the company, whose agent seats' display names the pass keeps
	// in step with their bots.
	Org *org.Organization

	// ReadOnly is a check: the pass reads and reports, and renames no bot.
	ReadOnly bool
}

// Reconcile runs one pass over the company's half of Mattermost: it checks
// that the instance will let this engine provision (the preflight), and keeps
// each agent's bot display name in step with the company document.
//
// IT CREATES, CHANGES AND DEACTIVATES NO BOT: each agent's own bot is the
// per-seat access sync's ([seataccess.Vendor], accessvendor.go), which reads
// every bot back on each pass. So a provisioning token that lost system_admin,
// or bot accounts switched off at the instance, is said here, in the setting
// an administrator has to change, before the sync's writes fail with a 403
// that names an endpoint.
func Reconcile(ctx context.Context, opts Options) (*Result, error) {
	// A CANCELLED PASS HAS OBSERVED NOTHING, and it says so here rather than
	// by happening to make a network call first: the reconcile loop treats
	// "no findings, no error" as ready and trusts it for a full settled
	// interval, so a node shutting down mid-pass would record Mattermost as
	// healthy on its way out.
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("mattermost: %w", err)
	}
	if opts.Client == nil {
		return nil, errors.New("mattermost: no client")
	}
	// REFUSED RATHER THAN DEREFERENCED: the reconcile worker is a fleet
	// singleton, so a nil here would take the node down where an error is
	// a fault the loop retries and reports.
	if opts.Config == nil {
		return nil, errors.New("mattermost: no company mattermost block")
	}
	if opts.Org == nil {
		return nil, errors.New(
			"mattermost: no organization, so there are no agent seats to name bots after")
	}

	// NAMED `pre` rather than `preflight`, which would shadow the type: the
	// struct it hands back is what carries this to both readers, the notes
	// and the findings the reconcile loop reads.
	pre := checkPreflight(ctx, opts)
	res := &Result{}
	listBots(ctx, opts, res)
	res.Notes = slices.Concat(pre.notes(), res.Notes)
	res.NotAdmin, res.Disabled = pre.notAdmin, pre.disabled
	return res, nil
}

// listBots reads the instance's bots once per run and sweeps the renames. The
// display name lives on the BOT record and not on its user, so without this
// the comparison has nothing true to read, and per-seat fetches would be a
// call each for a value one list already carries. A listing that fails is a
// note, and no name is compared against it.
func listBots(ctx context.Context, opts Options, res *Result) {
	bots, err := opts.Client.Bots(ctx)
	if err != nil {
		res.Notes = append(res.Notes, fmt.Sprintf(
			"could not list bots, so display names are not checked this run: %v", err))
		return
	}
	managed := make(map[string]Bot, len(bots))
	for _, bot := range bots {
		managed[strings.ToLower(bot.Username)] = bot
	}
	renameBots(ctx, opts, managed, res)
}

// renameBots brings every agent seat's bot display name in line with the
// company document.
//
// A RENAMED ROLE REACHES THE BOT. A create-only display name means the roster
// in Mattermost drifts from the org chart it mirrors, with no way back but
// editing every bot by hand. It sweeps the document's agent seats, each of
// whose bot the per-seat access sync creates.
//
// COMPARED ONLY AGAINST A LISTING THAT ANSWERED, which the caller guarantees
// by calling this only when one did: the bot record is the only thing carrying
// the current display name, so a missing entry would otherwise read as an
// empty name and rewrite every bot on every pass.
func renameBots(ctx context.Context, opts Options, managed map[string]Bot, res *Result) {
	for seat := range opts.Org.AllRoles() {
		if !seat.IsAgent() {
			continue
		}
		want := BotDisplayName(opts.Config.Provisioning, seat.Name)
		bot, known := managed[strings.ToLower(BotUsername(opts.Config.Provisioning, seat.Handle()))]
		if want == "" || !known || bot.DisplayName == want {
			continue
		}
		if opts.ReadOnly {
			res.Notes = append(res.Notes, fmt.Sprintf(
				"%s's bot is named %q and the company document names it %q; the "+
					"next pass renames it", seat.Handle(), bot.DisplayName, want))
			continue
		}
		if err := opts.Client.PatchBot(ctx, bot.UserID, want); err != nil {
			res.Notes = append(res.Notes, fmt.Sprintf(
				"%s: could not update the bot's display name to %q: %v",
				seat.Handle(), want, err))
			continue
		}
		res.Renamed = append(res.Renamed, seat.Handle())
	}
}

// preflight is what this run learned about the instance, in the shape BOTH
// readers of it need: the notes, and [Result.Findings], which is all the
// reconcile loop reads. A preflight that spoke only in notes would tell the
// loop that Mattermost was ready while the provisioning token could not
// write, and the first anybody would learn of it is a seat that fails to
// turn on.
//
// ONE SENTENCE PER FINDING, built once here and used for both, so two
// spellings of it cannot drift.
type preflight struct {
	// unreadable is the account this run could not ask about.
	//
	// A NOTE AND NEVER A FINDING: "cannot tell" is not "confirmed wrong",
	// and the run is about to make real calls that report the same
	// reachability problem with far more context. Reporting it would put
	// a company behind a blinking instance into a blocked phase a person
	// is asked to go and fix.
	unreadable string

	// notAdmin is the sentence naming an account this run CONFIRMED is not
	// a system administrator, empty when it is one or could not be asked.
	//
	// The sentence rather than the roles, because the roles can legitimately
	// be empty — orNone renders that "(none)" — and a field keyed on them
	// would then read as "fine" for the worst account of all.
	notAdmin string

	// disabled names the instance settings this run read as an EXPLICIT
	// false, in the order they are checked.
	disabled []DisabledSetting
}

// DisabledSetting is one instance setting an administrator has switched off
// that this engine needs on.
type DisabledSetting struct {
	// Key is the ServiceSettings key, which is what somebody types into
	// the System Console's search box to find it.
	Key string
	// Stops is what it prevents, as the second half of Detail's sentence.
	Stops string
}

// Detail is the one sentence both the printed note and the finding carry.
func (d DisabledSetting) Detail() string {
	return fmt.Sprintf(
		"ServiceSettings.%s is false on this instance, so %s — an "+
			"administrator has to enable it in the System Console", d.Key, d.Stops)
}

// notes is the preflight's caveats, in the order they are checked.
func (p preflight) notes() []string {
	var out []string
	if p.unreadable != "" {
		out = append(out, p.unreadable)
	}
	if p.notAdmin != "" {
		out = append(out, p.notAdmin)
	}
	for _, setting := range p.disabled {
		out = append(out, setting.Detail())
	}
	return out
}

// checkPreflight reports what would make this run fail on its first write.
//
// NOT ERRORS, and the distinction is deliberate: the two settings are read
// from a config endpoint whose exact key set varies by server version, so an
// absent key means "this server did not say", not "it is off". Refusing on
// silence would make the provisioner unusable against a version this engine
// has not seen; reporting an explicit false is the honest half.
//
// The admin role IS checked hard enough to be worth naming, because a token
// without it fails every single write.
func checkPreflight(ctx context.Context, opts Options) preflight {
	var out preflight
	if me, err := opts.Client.Me(ctx); err != nil {
		out.unreadable = fmt.Sprintf(
			"could not confirm the provisioning account: %v", err)
	} else if !me.SystemAdmin() {
		out.notAdmin = fmt.Sprintf(
			"%s is not a system administrator (roles: %s) — creating a bot, "+
				"minting an access token and adding a team member all require "+
				"it, so this run will fail on its first write",
			me.Username, orNone(me.Roles))
	}

	cfg, err := opts.Client.ClientConfig(ctx)
	if err != nil {
		// Not worth a note of its own: the run is about to make real calls
		// that will report the same reachability problem with more context.
		return out
	}
	for _, setting := range []DisabledSetting{
		{"EnableBotAccountCreation", "no bot account can be created"},
		{"EnableUserAccessTokens", "no bot can be given a token, so none of " +
			"them can connect"},
	} {
		if cfg[setting.Key] == "false" {
			out.disabled = append(out.disabled, setting)
		}
	}
	return out
}

// orNone renders an empty role list readably.
func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(none)"
	}
	return s
}

// TokenDescription is the description every token this engine mints for a
// seat carries.
//
// It is the ONLY thing distinguishing a token this engine owns from one an
// administrator created by hand: whether a bot still holds the seat's
// credential, which tokens a rotation retires, and which tokens turning a
// seat off revokes.
func TokenDescription(handle string) string {
	return "crewlet-" + handle
}
