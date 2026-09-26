package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/gitlab"
	"github.com/crewlet/crewlet/internal/mattermost"
	"github.com/crewlet/crewlet/internal/provision"
)

// The provisioning commands, and the flags they share.
//
// # Why the sink is a required choice
//
// A run with nowhere to put what it mints creates live credentials at the
// third-party app and prints none of them, the worst outcome available,
// because every one has to be found and revoked by hand. So there is no
// default: the operator says where, up front, and a run with no answer is
// refused before it touches anything.

// sinkFlags is the shared --secret-store / --env-file / --print choice.
type sinkFlags struct {
	secretStore *bool
	envFile     *string
	print       *bool
	bootstrap   *string
	api         *string
}

func addSinkFlags(fs *flag.FlagSet) sinkFlags {
	return sinkFlags{
		secretStore: fs.Bool("secret-store", false,
			"record minted credentials in the encrypted secret store"),
		envFile: fs.String("env-file", "",
			"record minted credentials in this .env file"),
		print: fs.Bool("print", false,
			"print minted credentials to stdout and persist nothing"),
		bootstrap: bootstrapFlag(fs),
		api:       apiFlag(fs),
	}
}

// bootstrapFlag adds the -config flag: the Tier A document naming this
// node's store and the keyring that opens it.
//
// EVERY command that reads Tier B takes it, not only the ones that WRITE a
// credential. The store is where a rotated secret lives, so a command
// resolving a company's ${VAR} without it reads an empty string for
// everything the operator has already rotated. See [companyResolver].
func bootstrapFlag(fs *flag.FlagSet) *string {
	return fs.String("config", defaultBootstrapPath,
		"Tier A config: this node's store and its secret keyring")
}

// apiFlag adds the -api flag: the running node whose API the fleet's secret
// store is reached through.
//
// EVERY command that reads Tier B takes it, beside -config and for the same
// reason, and it is the flag `crewlet secrets` takes. The fleet's store is on
// the coordination KV, which on the default topology is inside the engine's
// own process, so a running node's /secrets surface is the only way in: the
// engine this host runs, found through -config, or the one this names — which
// is how a command reaches the fleet from a machine that is not a node.
func apiFlag(fs *flag.FlagSet) *string {
	return fs.String("api", "",
		"the running node to reach the fleet's secret store through, for "+
			"every ${VAR} this command reads and every credential it records; "+
			"default is the engine running on this host (api.host:port in -config)")
}

// open builds the chosen sink, refusing an ambiguous or absent choice, and a
// run whose fleet store could not be read ([fleetRead.recordable]).
//
// The sink is built over what the run read of the fleet's store, because a
// file or a printed sink is not that store and every node reads the store
// first: see [fleetShadowedSink].
//
// stdout is threaded through rather than read from a package variable, and
// that is not tidiness: the variable it replaced was written by each of the
// three provision commands just before this call, so two running at once
// raced on it — which the race detector caught the moment the CLI's tests
// ran in parallel. A writer is an argument the caller already has.
func (s sinkFlags) open(stdout io.Writer, fleet *fleetRead) (provision.TokenSink, error) {
	chosen := 0
	for _, on := range []bool{*s.secretStore, *s.envFile != "", *s.print} {
		if on {
			chosen++
		}
	}
	switch {
	case chosen == 0:
		return nil, fmt.Errorf("%w: pass -secret-store, -env-file PATH or -print",
			provision.ErrNoSink)
	case chosen > 1:
		// REFUSED RATHER THAN ORDERED. Writing to two places doubles the
		// number of copies of a live credential, and picking one by
		// precedence would put it somewhere the operator did not ask for.
		return nil, errors.New(
			"name exactly one of -secret-store, -env-file and -print")
	}

	// REFUSED BEFORE ANY SINK IS BUILT, like an absent or ambiguous choice:
	// see [fleetRead.recordable] for what a run whose fleet store is
	// out of reach would do with one.
	if refusal := fleet.recordable(); refusal != nil {
		return nil, refusal
	}
	switch {
	case *s.print:
		sink, err := provision.NewPrintSink(stdout)
		if err != nil {
			return nil, err
		}
		return fleet.shadowed(sink), nil
	case *s.envFile != "":
		sink, err := provision.NewEnvFileSink(*s.envFile)
		if err != nil {
			return nil, err
		}
		return fleet.shadowed(sink), nil
	}
	if fleet.node == nil {
		return nil, fmt.Errorf("-secret-store records into the fleet's secret "+
			"store, and there is none: %s. A store needs a keyring in Tier A "+
			"(`crewlet secrets keygen` prints one); or pass -env-file PATH or "+
			"-print instead", fleet.absent)
	}
	return provision.NewSecretStoreSink(fleet.node, currentOperator()), nil
}

// errFleetUnread marks a run whose Tier A declares a fleet secret store that
// the run could not read.
var errFleetUnread = errors.New("the fleet's secret store cannot be reached from here")

// fleetRead is how one run resolves the company document's ${VAR}s, and
// what it learned about the fleet's secret store doing so.
type fleetRead struct {
	// env resolves a reference: the fleet's values ahead of the environment
	// when they were read, the environment alone otherwise.
	env *config.Resolver

	// held is every value the fleet holds, as read through node. Nil when
	// nothing was read.
	held map[string]string

	// node is the running node held was read through, and the one a write
	// to the fleet's store goes through. Nil when none was reached.
	node *secretsClient

	// absent says why there is no fleet store at all: no bootstrap at the
	// path, or one declaring no keyring. Empty when there is one.
	absent string

	// unread is why a store this host's Tier A declares could not be read
	// from here, wrapping [errFleetUnread]. Nil when it was read, or when
	// there is none.
	unread error
}

// recordable refuses a run that would record a credential while the fleet's
// secret store, which this host's Tier A declares, could not be read.
//
// A KEYRING IS WHAT MAKES THE STORE POSSIBLE. With none, no node can seal a
// value into it, so the company's credentials live in the environment and a
// file or printed sink is the whole story. With one, the company's
// credentials may be in that store, which every node reads BEFORE the
// environment, and a run that cannot read it cannot tell what it holds:
//
//   - a pass asks its sink whether a credential is already held before it
//     mints one, and a file is not the fleet's store — so a credential only
//     the fleet holds reads as absent and is minted anew;
//   - a -rotate run mints whatever is held;
//   - and wherever a new credential is recorded — a file or the printed
//     output — the fleet's copy of that name wins, because every node
//     resolves the store first.
//
// Each of those replaces a working credential at the third-party app while
// every node goes on authenticating with the one it replaced. A provisioning
// command asks this right after it resolves the company, before any check of
// a value that resolved empty — which is the refusal's real cause — and
// before anything touches the third-party app: a pass creates an account
// before it mints for it, and a run stopped half way leaves the accounts
// behind.
func (c *fleetRead) recordable() error {
	if c.unread == nil {
		return nil
	}
	return fmt.Errorf("%w. The company's credentials may be in that store, "+
		"which every node reads before the environment, so a credential this "+
		"run minted could replace one the fleet holds at the third-party app "+
		"while every node went on authenticating with the fleet's copy. %s",
		c.unread, reachTheFleet)
}

// reachTheFleet is what an operator does about a fleet store this run could
// not read: the two routes [companyResolver] reads it through.
const reachTheFleet = "Start `crewlet run` on this host, or pass -api naming " +
	"a node that is up, and re-run"

// unreadClause is what a refusal over a value that resolved empty adds when
// the fleet's store could not be read, and "" when it was: the value may be
// one the fleet holds, and a refusal naming only the field sends an operator
// to set what is already set.
func (c *fleetRead) unreadClause() string {
	if c.unread == nil {
		return ""
	}
	return " — and " + c.unread.Error() + ", so a value the fleet holds " +
		"resolves empty in this run. " + reachTheFleet
}

// shadowed is sink as this run has to use it: over the fleet's values when
// they were read, and as it is when there is no fleet store.
func (c *fleetRead) shadowed(sink provision.TokenSink) provision.TokenSink {
	if c.held == nil {
		return sink
	}
	return fleetShadowedSink{TokenSink: sink, held: c.held}
}

// fleetShadowedSink is a file or printed sink on a run that read the fleet's
// secret store.
//
// THE FLEET'S VALUE SHADOWS THE SINK'S, on every node: the engine resolves
// the store first and the environment behind it, so a name the fleet holds
// resolves to the fleet's value whatever a file or an export says. A pass
// asks its sink what it holds before it mints, and a file that has never
// seen a credential the fleet holds would answer absent and have the pass
// mint over the working one; a value recorded in the file under such a name
// would never be read by any node. So [fleetShadowedSink.Value] answers the
// fleet's value first, and [fleetShadowedSink.Record] refuses a name the
// fleet holds: -secret-store is the sink that replaces it.
type fleetShadowedSink struct {
	provision.TokenSink

	// held is every value the fleet holds, read once for the run.
	held map[string]string
}

// errFleetHolds refuses to record a name the fleet's secret store holds in a
// sink no node resolves it from.
var errFleetHolds = errors.New("the fleet's secret store holds this name")

// Value implements [provision.TokenSink]: the fleet's value when it holds the
// name, the sink's own otherwise. Trimmed and empty-as-absent, the way the
// secret-store sink answers, so a pass reads one store alike through either.
func (s fleetShadowedSink) Value(ctx context.Context, name string) (string, bool, error) {
	if value, held := s.held[name]; held {
		value = strings.TrimSpace(value)
		return value, value != "", nil
	}
	return s.TokenSink.Value(ctx, name)
}

// Record implements [provision.TokenSink], refusing a name the fleet holds.
//
// The refusal reaches the pass after the credential was minted, and the
// pass's own failure path answers it — the GitLab and Mattermost passes
// revoke what the run minted. What the refusal guarantees is that no run
// reports a credential recorded where no node will read it.
func (s fleetShadowedSink) Record(ctx context.Context, name, value string) error {
	if _, held := s.held[name]; held {
		return fmt.Errorf("%w: %s, which every node resolves before %s, so a "+
			"value recorded there would never be read. Re-run with "+
			"-secret-store, which replaces the fleet's copy",
			errFleetHolds, name, s.TokenSink.Describe())
	}
	return s.TokenSink.Record(ctx, name, value)
}

// Forget implements [provision.TokenSink]: the sink forgets every name, and a
// name the fleet holds is reported as still sealed there, because this sink
// cannot remove it and every node goes on resolving it.
func (s fleetShadowedSink) Forget(ctx context.Context, names ...string) error {
	err := s.TokenSink.Forget(ctx, names...)
	var sealed []string
	for _, name := range names {
		if _, held := s.held[name]; held {
			sealed = append(sealed, name)
		}
	}
	if len(sealed) == 0 {
		return err
	}
	return errors.Join(err, fmt.Errorf("%w: these belong to accounts that "+
		"have been removed and are still sealed in the fleet's secret store, "+
		"where every node goes on resolving them — remove each with `crewlet "+
		"secrets unset NAME`: %s", errFleetHolds, strings.Join(sealed, ", ")))
}

// companyResolver builds the chain a run resolves Tier B ${VAR} references
// through: the fleet's secret store first, the environment behind it.
//
// THE SAME ORDER THE ENGINE USES, and it has to be the same one. Every
// command below reads the company's own values — the instance URL, the
// workspace, the webhook signing secret, the engine's read token — and a run
// that saw only the environment reads an EMPTY STRING for every one an
// operator has put in the store. That is not merely a missing value for a
// webhook signing secret: empty is the signal to MINT, so the run would
// replace a working secret at the third-party app with a fresh one and break
// every delivery in flight until the config caught up.
//
// # Through a running node, and only through one
//
// The fleet's store is on the coordination KV, which on the default topology
// is inside the engine's own process and listens on no socket, so this
// command reaches it through a running node's /secrets surface: the one api
// names, or else the engine running on this host. This node's own secret
// table is NOT a way in. It holds only rows written while the engine was
// stopped, which the engine moves onto the fleet at its next start, so read as
// the store it answers "unset" for every credential the fleet holds.
//
// ONE READ OF EVERY VALUE, `GET /secrets?reveal=true`, rather than one per
// name. The run cannot say in advance which names it will look up — whichever
// of the document's references it reaches, through [config.Resolver.Value]
// and [config.Resolver.LookupOK], neither of which can return an error — so a
// read per name would be made inside a lookup, and a read that failed there
// would resolve exactly as a value that is not set. Read up front, a failure
// fails the run before anything is resolved. The node logs the read once,
// naming every name and the operator.
//
// # When there is no store to read
//
// With no api named, a bootstrap that is not at this path, or one declaring no
// keyring, resolves from the environment alone: with no keyring there is no
// fleet store, which is a supported deployment. So it is a NOTE rather than a
// failure, and a note rather than silence: a mistyped -config resolving
// nothing reads every stored credential as unset, and an operator has to be
// able to see which chain ran.
//
// A keyring with NO ENGINE RUNNING on this host is the other case, and the
// note says the fleet's values cannot be read from here. The run resolves
// from the environment — which is what a report or a dry run needs — and a
// run that would record a credential is refused ([fleetRead.recordable]),
// because a value that resolved empty here may be one the fleet holds.
//
// A bootstrap that exists and cannot be read fails the run instead. Someone
// who configured a store and did not get it must not have their secrets
// quietly resolved from a stale export.
func companyResolver(ctx context.Context, bootstrapPath, api string, notes io.Writer) (*fleetRead, error) {
	api = strings.TrimSpace(api)
	envOnly := func(why string) (*fleetRead, error) {
		fmt.Fprintf(notes, "%s: resolving ${VAR} from the environment only.\n", why)
		return &fleetRead{env: config.EnvOnly(), absent: why}, nil
	}
	// A NAMED NODE NEEDS NO BOOTSTRAP HERE: it holds its own keyring, and a
	// machine that is not a node has no Tier A of its own. What one here
	// can still supply is the bearer token; with none, the node is reached
	// with CREWLET_API_TOKEN alone (see [nodeAPIToken]).
	boot, err := optionalBootstrap(bootstrapPath)
	switch {
	case err != nil:
		return nil, err
	case boot == nil && api == "":
		return envOnly("no " + bootstrapPath)
	case boot != nil && api == "" && len(boot.Secrets.Keys) == 0:
		return envOnly(bootstrapPath + " declares no secrets.keys")
	}
	var node *secretsClient
	if api != "" {
		node, err = newSecretsClient(boot, api)
	} else {
		node, err = runningNode(ctx, boot, bootstrapPath)
	}
	return resolveThrough(ctx, bootstrapPath, node, err, notes)
}

// optionalBootstrap is the Tier A config at path, or nil when there is no file
// there — a machine that is not a node, which a command naming one with -api
// may run from. A file that is there and does not load is an error.
func optionalBootstrap(path string) (*config.Bootstrap, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return loadBootstrapForStore(path)
}

// resolveThrough is the chain [companyResolver] builds once it has asked for a
// running node: the node's values ahead of the environment, or the
// environment alone when this host runs no node, or the reason there is
// neither.
func resolveThrough(ctx context.Context, bootstrapPath string, node *secretsClient,
	reach error, notes io.Writer) (*fleetRead, error) {

	switch {
	case errors.Is(reach, errNoNodeHere):
		fmt.Fprintf(notes, "%s: resolving ${VAR} from the environment only.\n", reach)
		fmt.Fprintln(notes, "The fleet's secret store cannot be read from here, "+
			"so a value it holds resolves empty in this run, and a run that "+
			"would record a credential is refused rather than recording over "+
			"one the fleet may hold. "+reachTheFleet+" to read the fleet's values.")
		return &fleetRead{
			env: config.EnvOnly(),
			unread: fmt.Errorf("%w: %w, and %s declares a secret keyring",
				errFleetUnread, reach, bootstrapPath),
		}, nil
	case reach != nil:
		return nil, reach
	}
	values, err := node.Values(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the fleet's secret store: %w", err)
	}
	return &fleetRead{
		env: config.WithStore(config.MapSource(values)), held: values, node: node,
	}, nil
}

// operatorCredential reads the human operator's own credential, from the
// ENVIRONMENT ALONE.
//
// Deliberately NOT through [companyResolver], which every company value goes
// through. This is the operator's credential rather than the company's, and
// the difference is not bookkeeping: a GitLab admin PAT carries `api` scope
// over the whole group, and the secret store is replicated to every node
// that holds the keyring. Crewlet never persists one, and reading one back
// from the store would imply it may be kept there — which is how the most
// powerful credential in the deployment ends up in the shared table beside
// the seat tokens it exists to mint.
func operatorCredential(name string) string {
	return strings.TrimSpace(config.EnvOnly().Lookup(name))
}

// runGitLabProvision is `crewlet gitlab provision`.
func runGitLabProvision(args []string, stdout, stderr io.Writer) error {
	companyPath, args := splitSubject(args)

	fs := flag.NewFlagSet("gitlab provision", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sinks := addSinkFlags(fs)
	adminToken := fs.String("admin-token", "",
		"a GitLab token permitted to create service accounts; empty reads "+
			"GITLAB_ADMIN_TOKEN")
	publicURL := fs.String("public-url", "",
		"this deployment's public base URL, for registering the webhook; "+
			"defaults to integrations.public_base_url")
	rotate := fs.Bool("rotate", false,
		"mint a fresh token for every seat, including seats whose current "+
			"one still works (the engine has to be restarted after)")
	decommission := fs.Bool("decommission", false,
		"delete managed service accounts whose seats have left the config")
	mode := fs.String("mode", "",
		"where service accounts are owned: group (the default, and all "+
			"GitLab.com offers) or instance (self-managed only; needs an "+
			"instance-administrator token). Defaults to "+
			"integrations.gitlab.provisioning.mode")
	expiryDays := fs.Int("token-expiry-days", 0,
		"lifetime for minted tokens; 0 sends none and lets the instance "+
			"policy decide")
	dryRun := fs.Bool("dry-run", false,
		"print what the run would do and touch nothing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// PASSED ONLY WHEN THE OPERATOR TYPED IT, so the flag's default
	// cannot be told from a deliberate zero.
	var expiry *int
	// AND THE MODE ONLY WHEN THEY TYPED IT, so the flag can be told from the
	// document's own answer. The document is where this lives now — the
	// engine's own passes read it and cannot read a flag — and a flag that
	// silently defaulted to "group" would put the command line back in
	// disagreement with the loop on the one input that decides which route
	// creates, mints for and deletes every account.
	var modeGiven bool
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "token-expiry-days":
			expiry = expiryDays
		case "mode":
			modeGiven = true
		}
	})
	companyPath, given := onePositional(fs, companyPath)
	if given != 1 {
		fmt.Fprintln(stderr,
			"usage: crewlet gitlab provision <company.yaml> "+
				"[-secret-store|-env-file PATH|-print] [-public-url URL] "+
				"[-mode group|instance] [-rotate] [-decommission] "+
				"[-token-expiry-days N] [-dry-run]")
		return errors.New("name exactly one company document")
	}
	if expiry != nil && *expiry < 0 {
		return errors.New("-token-expiry-days must not be negative")
	}
	// REFUSED BEFORE THE CONFIG IS EVEN LOADED. A typo here is the one
	// input that decides which endpoint every account is created on, and
	// discovering it from a 404 half way through a run leaves an operator
	// working out which seats landed.
	if modeGiven && !gitlab.Mode(*mode).Valid() {
		return fmt.Errorf("-mode %q is not one of %s",
			*mode, strings.Join(gitlab.Modes(), ", "))
	}

	company, err := config.LoadCompanyToRun(companyPath)
	if err != nil {
		return err
	}
	organization, err := company.Organization()
	if err != nil {
		return err
	}
	cfg := company.Integrations.GitLab
	plan, err := gitlab.PlanFor(organization, cfg)
	if err != nil {
		return err
	}

	// RESOLVED BEFORE THE PLAN IS PRINTED, because the plan's most
	// consequential line depends on it: whether this run will replace the
	// key a working hook signs with. Reading the store is not a mutation,
	// so a -dry-run does it too — a dry run that could not say this would
	// be silent about the one outcome an operator most needs warning of.
	ctx := context.Background()
	fleet, err := companyResolver(ctx, *sinks.bootstrap, *sinks.api, stdout)
	if err != nil {
		return err
	}
	env := fleet.env
	if !*dryRun {
		if refusal := fleet.recordable(); refusal != nil {
			return refusal
		}
	}

	// WHERE a minted secret belongs, from the config's own reference — the
	// same mint-into-${VAR} contract the seat tokens follow. Empty when
	// signing_secret is a literal, which the reconcile refuses rather than
	// half-configuring.
	// RESOLVED ONCE, for the reason the Slack command states: the value is
	// read in three places here and three separate reads of the flag is
	// how one of them disagrees with the others.
	base := webhookBase(*publicURL, &company.Integrations, env.LookupOK)

	signingVar := soleVarOf(cfg.SigningSecret)
	signing := gitlab.PlanSigningSecret(
		env.Value(cfg.SigningSecret), signingVar, *rotate, base != "")

	// THE PLAN IS PRINTED EITHER WAY, and it is the SAME plan the run
	// uses. A --dry-run that re-derived it separately would be a second
	// implementation that can disagree with the real one about what it
	// was going to do.
	printPlan(stdout, plan, "a GitLab credential (mcp_env."+gitlab.SeatEnv+"."+gitlab.CredentialKeys[0]+")")
	fmt.Fprintln(stdout, signing.Describe())
	if *dryRun {
		fmt.Fprintln(stdout, "\n-dry-run: nothing was created, minted or registered.")
		return nil
	}
	if plan.Empty() {
		return nil
	}

	sink, err := sinks.open(stdout, fleet)
	if err != nil {
		return err
	}

	token := strings.TrimSpace(*adminToken)
	if token == "" {
		token = operatorCredential("GITLAB_ADMIN_TOKEN")
	}
	if token == "" {
		return errors.New(
			"no administrator token: pass -admin-token or export " +
				"GITLAB_ADMIN_TOKEN. The seats' own tokens are what this run " +
				"MINTS, so it cannot bootstrap itself from them")
	}
	client, err := gitlab.NewClient(gitlab.ClientOptions{
		URL: env.Value(cfg.URL), Token: token,
	})
	if err != nil {
		return err
	}

	res, err := gitlab.Reconcile(ctx, gitlab.Options{
		Client: client, Config: cfg, Plan: plan, Sink: sink,
		WebhookBase:      base,
		SigningSecret:    env.Value(cfg.SigningSecret),
		SigningSecretVar: signingVar,
		Rotate:           *rotate, Decommission: *decommission, ExpiryDays: expiry,
		Mode: accountMode(cfg, modeGiven, *mode),
	})
	if err != nil {
		return err
	}
	printResult(stdout, res, sink)
	return nil
}

// printPlan renders what a run intends to do.
// The `what` names the credential a seat would have to reference, because
// an empty plan is almost always a config that names none — and "nothing to
// do" without saying what was looked for sends an operator to the third-party app.
func printPlan(w io.Writer, plan *provision.Plan, what string) {
	if plan.Empty() {
		fmt.Fprintf(w, "No seat references %s, so there is nothing to provision.\n", what)
	} else {
		fmt.Fprintf(w, "%d seat(s) to provision:\n", len(plan.Seats))
		for _, seat := range plan.Seats {
			fmt.Fprintf(w, "  %-16s %s → %s\n", seat.Handle, seat.Role, seat.TokenVar)
		}
	}
	printNotes(w, plan.Notes)
}

// printResult renders what a run did.
//
// THE REPORT ENDS WITH THE NOTES, because they are the part an operator has
// to act on: the run itself either worked or returned an error, while a note
// is a seat that was skipped and will keep being skipped.
func printResult(w io.Writer, res *gitlab.Result, sink provision.TokenSink) {
	fmt.Fprintf(w, "\nRecorded in %s.\n", sink.Describe())
	printNextStep(w, res.Recorded, sink)
	if len(res.Created) > 0 {
		fmt.Fprintf(w, "Created %d account(s): %s\n",
			len(res.Created), strings.Join(res.Created, ", "))
	}
	if len(res.Rotated) > 0 {
		fmt.Fprintf(w, "Minted a token for %d seat(s): %s\n",
			len(res.Rotated), strings.Join(res.Rotated, ", "))
	}
	printKept(w, res.Kept)
	if len(res.Decommissioned) > 0 {
		fmt.Fprintf(w, "Deleted %d account(s) whose seats have left: %s\n",
			len(res.Decommissioned), strings.Join(res.Decommissioned, ", "))
	}
	if res.Hooked != "" {
		// WHERE, not just whether. A group hook covers every project in
		// the group including ones added later; a set of project hooks
		// covers exactly what was listed, and the difference only shows
		// up the day somebody adds a repository.
		where := "on the group"
		if len(res.HookedOn) != 1 || res.HookedOn[0] != "group" {
			where = fmt.Sprintf("on %d project(s): %s",
				len(res.HookedOn), strings.Join(res.HookedOn, ", "))
		}
		fmt.Fprintf(w, "Webhook registered at %s %s\n", res.Hooked, where)
	}
	printNotes(w, res.Notes)
}

// printNextStep says what still has to happen for the values a run recorded
// to reach a RUNNING engine.
//
// Only when something was recorded: a re-run that changed nothing has no
// next step, and telling that operator to restart anything is noise they
// learn to skip past — which is how they miss the run that did.
//
// The sink answers, because the answer differs per sink and only one of the
// three is "source a file". The secret store is the trap: it needs no file,
// so a report that stopped at "recorded in the encrypted secret store" read
// as finished — while the engine went on resolving from the snapshot it
// built at its last apply.
func printNextStep(w io.Writer, recorded int, sink provision.TokenSink) {
	if recorded == 0 {
		return
	}
	fmt.Fprintf(w, "Next: %s\n", sink.NextStep())
}

// printKept reports the seats a run deliberately did not touch.
//
// SAID OUT LOUD, because it is the successful outcome of a re-run: a report
// that mentioned only what changed would read as a run that did nothing,
// and the operator's next move would be to reach for -rotate — which is
// exactly the outage this behaviour exists to prevent.
func printKept(w io.Writer, kept []string) {
	if len(kept) == 0 {
		return
	}
	fmt.Fprintf(w, "Left %d seat(s) alone — their credential still works, "+
		"and rotating it would revoke what the running engine is using: %s\n"+
		"  (pass -rotate to mint fresh ones, and restart the engine after)\n",
		len(kept), strings.Join(kept, ", "))
}

func printNotes(w io.Writer, notes []string) {
	if len(notes) == 0 {
		return
	}
	fmt.Fprintf(w, "\n%d note(s):\n", len(notes))
	for _, note := range notes {
		fmt.Fprintf(w, "  - %s\n", note)
	}
}

// vendorCommand is one `crewlet <vendor> <sub>`.
type vendorCommand struct {
	sub  string
	args string // the operand spelling shown in usage, without the vendor
	run  func(args []string, stdout, stderr io.Writer) error
}

// vendorCommands is the whole vendor CLI surface, and it is the ONLY list.
//
// Dispatch and usage both read this table because they were once two
// hand-maintained lists, and vendors added since have drifted between them in
// both directions: one shipped with import and resync working and advertised
// nowhere, and Confluence — which has an import and no provision — was
// advertised with a `provision` subcommand that does not exist. Both are the
// same defect, in opposite directions, and both are invisible to anyone not
// reading the source. A table cannot drift from itself.
//
// Ordered per vendor, because the printed usage is this slice.
var vendorCommands = map[string][]vendorCommand{
	"gitlab": {
		{"provision", "<company.yaml>", runGitLabProvision},
	},
	"github": {
		{"provision", "<company.yaml>", runGitHubProvision},
	},
	"jira": {
		{"provision", "<company.yaml>", runJiraProvision},
	},
	"slack": {
		{"provision", "<company.yaml>", runSlackProvision},
	},
	"confluence": {
		{"provision", "<company.yaml>", runConfluenceProvision},
		{"import", "<company.yaml> <directory>", runConfluenceImport},
		{"resync", "<company.yaml>", runConfluenceResync},
	},
	"mattermost": {
		{"provision", "<company.yaml>", runMattermostProvision},
		{"doctor", "<company.yaml>", runMattermostDoctor},
	},
}

// errUnknownSub is `crewlet <vendor> <typo>`.
//
// A SENTINEL because the alternative for the caller is matching on the
// message, and the one caller that has to tell "this vendor does not have
// that command" from "that command ran and failed" is the test holding the
// usage text and the dispatch table together.
var errUnknownSub = errors.New("unknown command")

// runIntegration dispatches `crewlet <vendor> <command>`.
func runIntegration(vendor string, args []string, stdout, stderr io.Writer) error {
	sub, rest := splitSubject(args)
	commands := vendorCommands[vendor]
	if len(commands) == 0 {
		// Unreachable through run(), whose case list and this table are
		// asserted equal. Answered rather than panicked because a caller
		// inside the process is not an operator to be crashed at.
		return fmt.Errorf("no commands for %q", vendor)
	}
	for _, command := range commands {
		if command.sub == sub {
			return command.run(rest, stdout, stderr)
		}
	}
	if sub != "" && sub != "help" {
		return fmt.Errorf("%w: %s %q", errUnknownSub, vendor, sub)
	}
	printVendorUsage(stderr, vendor, commands)
	return flag.ErrHelp
}

// printVendorUsage writes one vendor's subcommands, aligned under `usage:`.
func printVendorUsage(w io.Writer, vendor string, commands []vendorCommand) {
	for i, command := range commands {
		lead := "usage:"
		if i > 0 {
			lead = "      "
		}
		fmt.Fprintf(w, "%s crewlet %s %s %s\n", lead, vendor, command.sub, command.args)
	}
}

// soleVarOf is the variable a whole ${VAR} reference names, or empty.
func soleVarOf(value string) string {
	name, ok := provision.SoleVar(value)
	if !ok {
		return ""
	}
	return name
}

// runMattermostProvision is `crewlet mattermost provision`.
func runMattermostProvision(args []string, stdout, stderr io.Writer) error {
	companyPath, args := splitSubject(args)

	fs := flag.NewFlagSet("mattermost provision", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sinks := addSinkFlags(fs)
	adminToken := fs.String("admin-token", "",
		"a Mattermost token for a system administrator; empty reads MATTERMOST_ADMIN_TOKEN")
	rotate := fs.Bool("rotate", false,
		"mint a fresh token for every bot, including bots whose current "+
			"one still works (the engine has to be restarted after)")
	only := fs.String("handles", "",
		"provision only these seat handles, comma-separated; empty does all")
	decommission := fs.Bool("decommission", false,
		"disable managed bot accounts whose seats have left the config")
	dryRun := fs.Bool("dry-run", false,
		"print what the run would do and touch nothing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	companyPath, given := onePositional(fs, companyPath)
	if given != 1 {
		fmt.Fprintln(stderr,
			"usage: crewlet mattermost provision <company.yaml> "+
				"[-secret-store|-env-file PATH|-print] [-handles a,b] "+
				"[-rotate] [-decommission] [-dry-run]")
		return errors.New("name exactly one company document")
	}

	company, err := config.LoadCompanyToRun(companyPath)
	if err != nil {
		return err
	}
	organization, err := company.Organization()
	if err != nil {
		return err
	}
	cfg := company.Integrations.Mattermost
	plan, err := mattermost.PlanFor(organization, cfg)
	if err != nil {
		return err
	}

	printPlan(stdout, plan, "a Mattermost bot token (its role's mattermost.bot_token)")
	if *dryRun {
		fmt.Fprintln(stdout, "\n-dry-run: nothing was created, joined or minted.")
		return nil
	}
	if plan.Empty() {
		return nil
	}

	ctx := context.Background()
	// RESOLVED BEFORE THE SINK OPENS, because the sink is built over what
	// the run read of the fleet's store ([sinkFlags.open]), and a store this
	// run could not read refuses it there.
	fleet, err := companyResolver(ctx, *sinks.bootstrap, *sinks.api, stdout)
	if err != nil {
		return err
	}
	env := fleet.env
	sink, err := sinks.open(stdout, fleet)
	if err != nil {
		return err
	}

	token := strings.TrimSpace(*adminToken)
	if token == "" {
		token = operatorCredential("MATTERMOST_ADMIN_TOKEN")
	}
	if token == "" {
		return errors.New(
			"no administrator token: pass -admin-token or export " +
				"MATTERMOST_ADMIN_TOKEN. The bots' own tokens are what this " +
				"run MINTS, so it cannot bootstrap itself from them")
	}
	client, err := mattermost.NewClient(mattermost.ClientOptions{
		URL: env.Value(cfg.URL), Token: token,
	})
	if err != nil {
		return err
	}

	res, err := mattermost.Reconcile(ctx, mattermost.Options{
		Client: client, Config: cfg, Org: organization, Plan: plan, Sink: sink,
		Rotate: *rotate, Decommission: *decommission, Only: splitHandles(*only),
	})
	if err != nil {
		return err
	}
	printChatResult(stdout, res, sink.Describe())
	return nil
}

// printChatResult renders what a Mattermost run did.
func printChatResult(w io.Writer, res *mattermost.Result, where string) {
	fmt.Fprintf(w, "\nRecorded in %s.\n", where)
	if len(res.Created) > 0 {
		fmt.Fprintf(w, "Created %d bot(s): %s\n",
			len(res.Created), strings.Join(res.Created, ", "))
	}
	if len(res.Renamed) > 0 {
		fmt.Fprintf(w, "Renamed %d bot(s) to match the company document: %s\n",
			len(res.Renamed), strings.Join(res.Renamed, ", "))
	}
	if len(res.Rotated) > 0 {
		fmt.Fprintf(w, "Minted a token for %d bot(s): %s\n",
			len(res.Rotated), strings.Join(res.Rotated, ", "))
	}
	if len(res.Decommissioned) > 0 {
		fmt.Fprintf(w, "Disabled %d departed bot(s): %s\n",
			len(res.Decommissioned), strings.Join(res.Decommissioned, ", "))
	}
	printKept(w, res.Kept)
	// THE CHANNELS ARE THE PART TO CHECK. A bot receives only what its
	// channels deliver, so this line is the difference between an agent
	// that wakes and one that never does.
	for _, seat := range sortedKeys(res.Joined) {
		fmt.Fprintf(w, "  %-16s channels: %s\n", seat,
			strings.Join(res.Joined[seat], ", "))
	}
	printNotes(w, res.Notes)
}

func sortedKeys(m map[string][]string) []string {
	out := slices.Sorted(maps.Keys(m))
	return out
}

// runMattermostDoctor is `crewlet mattermost doctor`.
func runMattermostDoctor(args []string, stdout, stderr io.Writer) error {
	companyPath, args := splitSubject(args)

	fs := flag.NewFlagSet("mattermost doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	adminToken := fs.String("admin-token", "",
		"a Mattermost token to run the checks as; empty reads "+
			"MATTERMOST_ADMIN_TOKEN, and failing that borrows a seat's own")
	bootstrap := bootstrapFlag(fs)
	api := apiFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	companyPath, given := onePositional(fs, companyPath)
	if given != 1 {
		fmt.Fprintln(stderr,
			"usage: crewlet mattermost doctor <company.yaml> [-admin-token TOKEN]")
		return errors.New("name exactly one company document")
	}

	company, err := config.LoadCompanyToRun(companyPath)
	if err != nil {
		return err
	}
	organization, err := company.Organization()
	if err != nil {
		return err
	}
	cfg := company.Integrations.Mattermost
	if cfg == nil || !cfg.Enabled {
		return errors.New("mattermost: the company config does not enable mattermost")
	}

	ctx := context.Background()
	fleet, err := companyResolver(ctx, *bootstrap, *api, stdout)
	if err != nil {
		return err
	}
	env := fleet.env

	token := strings.TrimSpace(*adminToken)
	if token == "" {
		token = operatorCredential("MATTERMOST_ADMIN_TOKEN")
	}
	// AN EMPTY TOKEN IS FINE. The checks that need one borrow a seat's,
	// because those are the credentials the engine authenticates with —
	// and asking an operator to mint an admin token to find out whether
	// their company works is a step that exists only to be skipped.
	//
	// THE CONFIGURED URL, resolved but NOT normalised away: the whole
	// point is comparing what this company believes against what the
	// server reports, so the check has to see the operator's own value.
	resolved := *cfg
	resolved.URL = env.Value(cfg.URL)
	resolved.Team = env.Value(cfg.Team)
	client, err := mattermost.NewClient(mattermost.ClientOptions{
		URL: resolved.URL, Token: token,
	})
	if err != nil {
		return fmt.Errorf("%w%s", err, fleet.unreadClause())
	}

	report, err := mattermost.Doctor(ctx, mattermost.DoctorOptions{
		Client: client, Config: &resolved, Org: organization,
		SeatToken: mattermost.SeatTokens(env),
	})
	if err != nil {
		return fmt.Errorf("%w%s", err, fleet.unreadClause())
	}
	printDoctor(stdout, report)
	if !report.Healthy() {
		// THE CAUSE A FINDING CANNOT NAME: a seat's token this run could
		// not read from the fleet's store reads as a seat with none.
		return fmt.Errorf("mattermost: the instance is not healthy for this "+
			"company%s", fleet.unreadClause())
	}
	return nil
}

// printDoctor renders a health report.
func printDoctor(w io.Writer, report *mattermost.Report) {
	for _, finding := range report.Findings {
		mark := "ok  "
		if !finding.OK {
			mark = "FAIL"
		}
		fmt.Fprintf(w, "%s  %-16s %s\n", mark, finding.Check, finding.Detail)
	}
	switch {
	case report.Stopped():
		// SAID OUT LOUD: one failing line with nothing after it reads
		// as "one thing is wrong", when what it means is "nothing else
		// was even asked".
		fmt.Fprintln(w, "\nThe checks stopped here — everything below this "+
			"would have reported a consequence rather than a cause. Fix it "+
			"and run again.")
	case report.Healthy():
		fmt.Fprintln(w, "\nEverything this command can check is working.")
	}
}

// skillsContainer resolves the tool-skills container one CLI run writes to
// or reads from.
//
// Three sources, most specific first: the -space flag, then the environment
// variable, then the company config — which is itself three-valued,
// distinguishing "unset, take the reserved default" from an explicit empty
// string meaning tool skills are OFF for this company.
//
// # The variable is a FLAG DEFAULT, and nothing more
//
// The engine never reads it. A running node's watched container comes from
// the versioned company document and only from there, because a fleet whose
// nodes each read a variable out of whoever's shell started them would
// disagree about which space holds the skills — and the symptom is agents on
// one node following guidance the others have never heard of. Here, in a
// command an operator types, a variable is just a way not to retype a flag.
func skillsContainer(flagValue, envVar, fromConfig string) string {
	if v := strings.TrimSpace(flagValue); v != "" {
		return strings.ToUpper(v)
	}
	if v := strings.TrimSpace(config.EnvOnly().Lookup(envVar)); v != "" {
		return strings.ToUpper(v)
	}
	return fromConfig
}

// noPublicBase says what to change when a run needs an address and has none.
//
// THREE WAYS TO BE EMPTY, and they are not the same work. The flag was not
// given AND the company names no public base; or it names one that this
// process cannot resolve, because [config.Integrations.WebhookBase] turns a
// ${VAR} it cannot see into "" rather than into the text of the variable. A
// message naming only the flag sent an operator who had already set the field
// looking for something they did not need.
//
// A ${VAR} RESOLVES THE WAY [companyResolver] does — the fleet's secret store,
// then this process's environment — and through nothing else: the file a
// -env-file sink writes is where this run records what it mints, and is not
// read to resolve anything.
func noPublicBase(in *config.Integrations) string {
	const why = "every app's Events API request URL and OAuth redirect URL " +
		"are built from it, so an app created without one delivers nowhere " +
		"and cannot be installed"
	raw := ""
	if in != nil {
		raw = strings.TrimSpace(in.PublicBaseURL)
	}
	if raw == "" {
		return "no public base URL: pass -public-url, or set " +
			"integrations.public_base_url in the company document. " + why
	}
	if name, ok := provision.SoleVar(raw); ok {
		return fmt.Sprintf(
			"integrations.public_base_url points at ${%s} and this process "+
				"resolved nothing for it — set %s in the fleet's secret store "+
				"or in this process's environment, or pass -public-url to "+
				"override it for this run. %s", name, name, why)
	}
	return fmt.Sprintf(
		"integrations.public_base_url is %q, which resolved to nothing — "+
			"correct it in the company document, or pass -public-url to "+
			"override it for this run. %s", raw, why)
}

// webhookBase is the address a third-party app reaches this deployment on: the flag
// when one was passed, and the company document's own value otherwise.
//
// THE FLAG WINS, and only when it is non-empty. It is the one-off override —
// a staging tunnel, a run against a second site — while the document is what
// every other reader of this value sees, the reconcile loop included. A flag
// that won even when unset would make an operator who simply forgot it
// silently re-point a working hook at "".
//
// The flag is taken AS TYPED and the document's value is RESOLVED, which is
// the difference between the two: somebody typing `-public-url` typed an
// address, while `public_base_url` may be a whole `${VAR}` this run has to
// read before it can build anything a third-party app will hold. See
// [config.Integrations.WebhookBase].
func webhookBase(flagValue string, in *config.Integrations, resolve func(string) (string, bool)) string {
	if v := strings.TrimSpace(flagValue); v != "" {
		return strings.TrimRight(v, "/")
	}
	if in == nil {
		return ""
	}
	return in.WebhookBase(resolve)
}

// accountMode is where this run owns the accounts it creates.
//
// THE DOCUMENT IS THE DEFAULT and the flag is a one-invocation override, the
// way `-public-url` overrides `integrations.public_base_url`. The flag used to
// be the only source, which made this a fact only the person who typed it
// knew — and the engine provisions the same company from the same document
// with no flag to read, so the two disagreed on the one input that decides
// which route creates an account, mints its tokens and deletes it.
func accountMode(cfg *config.GitLab, given bool, flagged string) gitlab.Mode {
	if given {
		return gitlab.Mode(flagged)
	}
	if cfg == nil {
		return gitlab.ModeGroup
	}
	return gitlab.Mode(cfg.Provisioning.ModeOrDefault())
}
