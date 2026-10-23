package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/gitlab"
	"github.com/crewlet/crewlet/internal/mattermost"
	"github.com/crewlet/crewlet/internal/provision"
	"github.com/crewlet/crewlet/internal/secrets"
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

// sinkFlags is the shared --secret-store / --env-file / --print choice, and the
// -config / -api pair naming the store the run reads and records through.
type sinkFlags struct {
	secretStore *bool
	envFile     *string
	print       *bool
	store       storeFlags
}

// addSinkFlags adds the sink choice and the store flags for a command that
// takes the decisions named ([sinkDecisions]), which its note on a stopped
// node says what becomes of.
func addSinkFlags(fs *flag.FlagSet, decides sinkDecisions) sinkFlags {
	sinks := sinkFlags{
		secretStore: fs.Bool("secret-store", false,
			"record minted credentials in the encrypted secret store"),
		envFile: fs.String("env-file", "",
			"record minted credentials in this .env file"),
		print: fs.Bool("print", false,
			"print minted credentials to stdout and persist nothing"),
		store: addStoreFlags(fs),
	}
	sinks.store.decides = decides
	return sinks
}

// open builds the chosen sink, refusing an ambiguous or absent choice.
//
// -secret-store records through the run's own store ([companyStore]) — the
// very route its values were read through, so a run cannot read one store and
// write another — and reads back through it too, so on the route where a
// miss is unknown its "not held" is unknown as well ([sinkStore]).
//
// stdout is threaded through rather than read from a package variable, and
// that is not tidiness: the variable it replaced was written by each of the
// three provision commands just before this call, so two running at once
// raced on it — which the race detector caught the moment the CLI's tests
// ran in parallel. A writer is an argument the caller already has.
func (s sinkFlags) open(ctx context.Context, companySecrets *companyStore, stdout io.Writer) (
	provision.TokenSink, error) {

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

	var sink provision.TokenSink
	switch {
	case *s.print:
		printed, err := provision.NewPrintSink(stdout)
		if err != nil {
			return nil, err
		}
		sink = printed
	case *s.envFile != "":
		file, err := provision.NewEnvFileSink(*s.envFile)
		if err != nil {
			return nil, err
		}
		sink = file
	default:
		target, err := companySecrets.route(ctx)
		if err != nil {
			return nil, err
		}
		if target == nil {
			return nil, fmt.Errorf("-secret-store has no store to record into: "+
				"there is no Tier A file at %s naming this node's store and "+
				"keyring. Point -config at it, or pass -api URL naming a "+
				"running node", companySecrets.bootstrapPath)
		}
		sink = provision.NewSecretStoreSink(
			sinkStore{secretTarget: target, unvouched: companySecrets.unvouched()}, hostAuthor())
	}
	return sink, nil
}

// sinkStore is a secret target as a provisioning sink's store. The author a
// row records is the store's to report and a sink has nobody to tell, so the
// answer is dropped here rather than widening the sink's own seam.
type sinkStore struct {
	*secretTarget

	// unvouched answers a name the store does not hold, where that
	// absence is unknown rather than a fact: nil through a node, whose
	// miss is the fleet's; on this node's own table while its engine is
	// stopped, every miss ([companyStore.unvouched]).
	unvouched provision.Unvouched
}

func (s sinkStore) Set(ctx context.Context, name, value string, by secrets.Author,
	source secrets.Source, now time.Time) error {

	_, err := s.secretTarget.Set(ctx, name, value, by, source, now)
	return err
}

// Get reads one value back, three-valued.
//
// EVERY DECISION A SINK'S READ FEEDS TURNS ON "NOT HELD" — a seat whose token
// is not held gets a new one, an app whose bot token is not held is installed
// again, a webhook secret not held is minted — and each of those revokes or
// replaces whatever the fleet holds. A stopped node's own table not holding a
// name says nothing about the fleet, so on that route the miss is the error a
// store that cannot say gives, which every one of those callers already
// refuses on, rather than [secrets.ErrNotFound].
func (s sinkStore) Get(ctx context.Context, name string) (string, error) {
	value, err := s.secretTarget.Get(ctx, name)
	if errors.Is(err, secrets.ErrNotFound) {
		if unknown := s.unvouched.Of(name); unknown != nil {
			return "", unknown
		}
	}
	return value, err
}

// operatorCredential reads the human operator's own credential, from the
// ENVIRONMENT ALONE.
//
// Deliberately NOT through [companyStore], which every company value goes
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
func runGitLabProvision(environ config.Source, args []string, stdout, stderr io.Writer) error {
	companyPath, args := splitSubject(args)

	fs := flag.NewFlagSet("gitlab provision", flag.ContinueOnError)
	fs.SetOutput(stderr)
	// THE SIGNING SECRET AND EVERY SEAT'S TOKEN, and -rotate a fresh one
	// of each.
	rotate := addFreshFlag(fs, "rotate",
		"mint a fresh token for every seat, including seats whose current "+
			"one still works, and a fresh webhook signing secret into the "+
			"variable signing_secret names wherever the hook is registered "+
			"(the engine has to be restarted after)")
	sinks := addSinkFlags(fs, sinkDecisions{secrets: true, seatTokens: true, fresh: rotate})
	adminToken := fs.String("admin-token", "",
		"a GitLab token permitted to create service accounts; empty reads "+
			"GITLAB_ADMIN_TOKEN")
	publicURL := fs.String("public-url", "",
		"this deployment's public base URL, for registering the webhook; "+
			"defaults to "+publicURLDefault)
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
				"[-token-expiry-days N] [-dry-run] [-config PATH] [-api URL]")
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
	companySecrets, err := openCompanyStore(environ, sinks.store, "crewlet gitlab provision")
	if err != nil {
		return err
	}
	defer companySecrets.Close()
	env, err := companySecrets.values(ctx, company, stdout)
	if err != nil {
		return err
	}
	boot := companySecrets.boot

	// WHERE a minted secret belongs, from the config's own reference — the
	// same mint-into-${VAR} contract the seat tokens follow. Empty when
	// signing_secret is a literal, which the reconcile refuses rather than
	// half-configuring.
	// RESOLVED ONCE, for the reason the Slack command states: the value is
	// read in three places here and three separate reads of the flag is
	// how one of them disagrees with the others.
	base, baseKey := webhookBase(*publicURL, boot), webhookBaseKey(boot)

	signingVar := soleVarOf(cfg.SigningSecret)
	signing := gitlab.PlanSigningSecret(
		env.Value(cfg.SigningSecret), signingVar, rotate.asked(), base != "", baseKey)
	// A MINT OR A REUSE WHOSE ONLY REASON IS AN ANSWER THIS RUN COULD NOT
	// VOUCH FOR is no plan at all: on a stopped node the variable may be the
	// fleet's, and minting over it — or registering what this shell exports
	// for it — re-points a working hook at a key no node holds. So the plan
	// says it is unknown rather than promising either. An explicit -rotate
	// is a decision, not a reason, and plans one regardless.
	unknownSigning := (signing.Action == gitlab.SigningMint ||
		signing.Action == gitlab.SigningReuse) &&
		env.Unvouched.Ref(cfg.SigningSecret) != nil

	// THE PLAN IS PRINTED EITHER WAY, and it is the SAME plan the run
	// uses. A --dry-run that re-derived it separately would be a second
	// implementation that can disagree with the real one about what it
	// was going to do.
	printPlan(stdout, plan, "a GitLab credential (mcp_env."+gitlab.SeatEnv+"."+gitlab.CredentialKeys[0]+")")
	if unknownSigning {
		fmt.Fprintln(stdout, "webhook signing secret: UNKNOWN — "+
			"integrations.gitlab.signing_secret reads a variable this node's own "+
			"secret rows do not hold, and the fleet's store is out of reach, so a "+
			"run neither mints into it nor registers what this shell exports for "+
			"it: it uses a value its sink already holds, or refuses before "+
			"anything is touched")
	} else {
		fmt.Fprintln(stdout, signing.Describe())
	}
	if *dryRun {
		fmt.Fprintln(stdout, "\n-dry-run: nothing was created, minted or registered.")
		return nil
	}
	if plan.Empty() {
		return nil
	}

	sink, err := sinks.open(ctx, companySecrets, stdout)
	if err != nil {
		return err
	}
	if unknownSigning {
		// REFUSED UP FRONT, on the terms the pass itself settles the
		// answer on ([provision.Resolved]): the pass settles the signing
		// secret only after it has created and minted for every seat, and
		// a refusal there is a rollback of all of it at GitLab.
		if _, _, unknown := provision.Resolved(ctx, sink, cfg.SigningSecret,
			env.Value(cfg.SigningSecret), env.Unvouched); unknown != nil {
			return fmt.Errorf("gitlab: refused before anything was touched: %w", unknown)
		}
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
		WebhookBaseKey:   baseKey,
		SigningSecret:    env.Value(cfg.SigningSecret),
		SigningSecretVar: signingVar,
		Unvouched:        env.Unvouched,
		Rotate:           rotate.asked(), Decommission: *decommission, ExpiryDays: expiry,
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
	// run takes the invocation's environment ([runIn]) first, because a
	// command that stores what it mints may reach the running node's
	// /secrets with the credential read from it.
	run func(environ config.Source, args []string, stdout, stderr io.Writer) error
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
func runIntegration(environ config.Source, vendor string, args []string, stdout, stderr io.Writer) error {
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
			return command.run(environ, rest, stdout, stderr)
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
func runMattermostProvision(environ config.Source, args []string, stdout, stderr io.Writer) error {
	companyPath, args := splitSubject(args)

	fs := flag.NewFlagSet("mattermost provision", flag.ContinueOnError)
	fs.SetOutput(stderr)
	// EVERY BOT'S TOKEN, and no webhook: Mattermost is reached over the
	// bots' own websockets. -rotate mints every bot a fresh one.
	rotate := addFreshFlag(fs, "rotate",
		"mint a fresh token for every bot, including bots whose current "+
			"one still works (the engine has to be restarted after)")
	sinks := addSinkFlags(fs, sinkDecisions{seatTokens: true, fresh: rotate})
	adminToken := fs.String("admin-token", "",
		"a Mattermost token for a system administrator; empty reads MATTERMOST_ADMIN_TOKEN")
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
				"[-rotate] [-decommission] [-dry-run] [-config PATH] [-api URL]")
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
	companySecrets, err := openCompanyStore(environ, sinks.store, "crewlet mattermost provision")
	if err != nil {
		return err
	}
	defer companySecrets.Close()
	env, err := companySecrets.values(ctx, company, stdout)
	if err != nil {
		return err
	}
	sink, err := sinks.open(ctx, companySecrets, stdout)
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
		Rotate: rotate.asked(), Decommission: *decommission, Only: splitHandles(*only),
	})
	if err != nil {
		return err
	}
	printChatResult(stdout, res, sink)
	return nil
}

// printChatResult renders what a Mattermost run did.
//
// WITH THE NEXT STEP, as GitLab's report has it ([printNextStep]): a token
// minted into the secret store reaches a running engine at its next apply and
// not before, and a report that stopped at "Recorded in the encrypted secret
// store" read as finished while every new bot stayed offline.
func printChatResult(w io.Writer, res *mattermost.Result, sink provision.TokenSink) {
	fmt.Fprintf(w, "\nRecorded in %s.\n", sink.Describe())
	printNextStep(w, res.Recorded, sink)
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
func runMattermostDoctor(environ config.Source, args []string, stdout, stderr io.Writer) error {
	companyPath, args := splitSubject(args)

	fs := flag.NewFlagSet("mattermost doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	adminToken := fs.String("admin-token", "",
		"a Mattermost token to run the checks as; empty reads "+
			"MATTERMOST_ADMIN_TOKEN, and failing that borrows a seat's own")
	browserURL := fs.String("browser-url", "",
		"the address people type into their browser to reach Mattermost: the "+
			"Site URL is held to it and the browser probe is sent there. Empty "+
			"reads MATTERMOST_PUBLIC_URL; with neither, the Site URL is judged "+
			"on its own")
	storeAt := addStoreFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	companyPath, given := onePositional(fs, companyPath)
	if given != 1 {
		fmt.Fprintln(stderr,
			"usage: crewlet mattermost doctor <company.yaml> [-admin-token TOKEN] "+
				"[-browser-url URL] [-config PATH] [-api URL]")
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
	companySecrets, err := openCompanyStore(environ, storeAt, "crewlet mattermost doctor")
	if err != nil {
		return err
	}
	defer companySecrets.Close()
	env, err := companySecrets.values(ctx, company, stdout)
	if err != nil {
		return err
	}

	token := strings.TrimSpace(*adminToken)
	if token == "" {
		token = operatorCredential("MATTERMOST_ADMIN_TOKEN")
	}
	// AN EMPTY TOKEN IS FINE. The checks that need one borrow a seat's,
	// because those are the credentials the engine authenticates with —
	// and asking an operator to mint an admin token to find out whether
	// their company works is a step that exists only to be skipped.
	//
	// THE CONFIGURED URL, resolved but NOT normalised away: the checks
	// dial it and hold its path to the one the server reports, so they
	// have to see the operator's own value.
	resolved := *cfg
	resolved.URL = env.Value(cfg.URL)
	resolved.Team = env.Value(cfg.Team)
	client, err := mattermost.NewClient(mattermost.ClientOptions{
		URL: resolved.URL, Token: token,
	})
	if err != nil {
		return err
	}

	// WHERE PEOPLE BROWSE IS THE OPERATOR'S TO SAY, and the variable is only
	// a way not to retype the flag: MATTERMOST_PUBLIC_URL is what the
	// compose stack sets the Site URL from and the bootstrap writes to .env,
	// so a shell that sourced it asks the question without being told to.
	browsedAt := strings.TrimSpace(*browserURL)
	if browsedAt == "" {
		browsedAt = strings.TrimSpace(config.EnvOnly().Lookup("MATTERMOST_PUBLIC_URL"))
	}
	report, err := mattermost.Doctor(ctx, mattermost.DoctorOptions{
		Client: client, Config: &resolved, Org: organization,
		SeatToken: mattermost.SeatTokens(env.Resolver), BrowsedAt: browsedAt,
	})
	if err != nil {
		return err
	}
	printDoctor(stdout, report)
	if !report.Healthy() {
		return errors.New("mattermost: the instance is not healthy for this company")
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
		fmt.Fprintln(w, "\nEverything this command checked is working.")
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

// publicURLDefault is how every `-public-url` flag's help names what it
// overrides: Tier A's webhook address, which is one key or the other depending
// on whether the file sets a public listener ([config.API.WebhookBaseKey]).
// One string for the five commands, so no help line names one key of the two.
const publicURLDefault = "api.public.external_url where api.public is set, " +
	"and api.external_url otherwise"

// noPublicBase says what to change when a run needs an address and has none.
//
// TWO WAYS TO BE EMPTY NOW, WHERE THERE WERE THREE. While the address was a
// Tier B pointer, a company could also name one that this process could not
// resolve — a `${VAR}` nothing exported, which read back as "" rather than as
// the text of the variable — so the message had to tell an operator who had
// already set the field that their variable was the problem. Tier A is
// resolved before the document is decoded, so what is left is the honest pair:
// no Tier A file was read, or it names no webhook address.
//
// THE KEY THE FILE WOULD BE READ FROM, never a literal: with `api.public` set
// the address is `api.public.external_url`, and naming `api.external_url`
// there sent an operator to set a key that was already set and that nothing
// building a vendor URL reads.
func noPublicBase(boot *config.Bootstrap) string {
	const why = "every app's Events API request URL and OAuth redirect URL " +
		"are built from it, so an app created without one delivers nowhere " +
		"and cannot be installed"
	if boot == nil {
		return "no address for this deployment: pass -public-url, or point " +
			"-config at the Tier A file that sets its webhook address (" +
			publicURLDefault + "). " + why
	}
	return "`" + boot.API.WebhookBaseKey() + "` is unset in this deployment's " +
		"Tier A file: set it there, or pass -public-url to override it for " +
		"this run. " + why
}

// webhookBase is the address a third-party app reaches this deployment on,
// which this run builds vendor URLs on: the `-public-url` flag when it was
// given, and otherwise Tier A's webhook address ([config.API.WebhookBase]) —
// the public listener's `api.public.external_url` once `api.public` is set,
// and `api.external_url` otherwise.
//
// THE VENDORS' ADDRESS, NEVER THE PERSON'S. Once `api.public` is set every
// webhook route is served only on that listener and `api.external_url` answers
// 404 for each of them, so a hook, a manifest or a redirect built on the
// person's address delivers nowhere — and the reconcile loop, which builds on
// this same accessor, re-pointed every hook this command registered on its
// next tick.
//
// THE FLAG WINS ONLY WHEN IT WAS TYPED. It is the one-off override — a staging
// tunnel, a run against a second site — while Tier A's address is what every
// other reader of it sees, the reconcile loop included. One that won even when
// unset would let an operator who simply forgot it silently re-point a working
// hook at "".
//
// NOTHING IS RESOLVED HERE, for either: somebody typing `-public-url` typed an
// address, and Tier A's `${VAR}` references are expanded before the file is
// decoded — so by the time this reads it, it is an address too. With no Tier A
// file at all there is only the flag.
func webhookBase(flagValue string, boot *config.Bootstrap) string {
	if v := strings.TrimSpace(flagValue); v != "" {
		return strings.TrimRight(v, "/")
	}
	if boot == nil {
		return ""
	}
	return boot.API.WebhookBase()
}

// webhookBaseKey is the Tier A key [webhookBase] falls back to, which a pass
// handed no address names as the setting to fill in ([config.API.WebhookBaseKey]).
//
// FROM THE SAME BLOCK AS THE ADDRESS, so the two cannot name different
// listeners. With no Tier A file there is no public listener to name, and the
// zero [config.API] answers exactly that — the key [setup.BaseKey] reads empty
// as, said here rather than left empty so a command never leans on a default.
func webhookBaseKey(boot *config.Bootstrap) string {
	var api config.API
	if boot != nil {
		api = boot.API
	}
	return api.WebhookBaseKey()
}

// accountMode is where this run owns the accounts it creates.
//
// THE DOCUMENT IS THE DEFAULT and the flag is a one-invocation override, the
// way `-public-url` overrides Tier A's webhook address. The flag used to
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
