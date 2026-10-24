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
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/gitlab"
	"github.com/crewlet/crewlet/internal/mattermost"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/provision"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/store"
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
		// THE SAME FLAG `crewlet secrets` takes, and for the same
		// reason: a running engine holds its database, so -secret-store
		// writes through its API — which is also what puts a minted
		// credential on every node rather than on this one.
		api: fs.String("api", "",
			"the node to read the company's stored values and record "+
				"credentials through; default is this node's store, or the "+
				"running engine at the api.host:port in -config"),
	}
}

// readAPIFlag adds -api to a command that reads the company and records
// nothing: the node its stored values are read through ([companyResolver]).
//
// THE SAME FLAG the provisioning commands, `crewlet llm` and `crewlet
// secrets` take, and needed for the same cases: a node that holds no data
// has no store of the company's to read, and a machine that is not a node has
// none at all, so the values are a node's that does.
func readAPIFlag(fs *flag.FlagSet) *string {
	return fs.String("api", "",
		"the node to read the company's stored values through; default is "+
			"this node's store, or the running engine holding it")
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

// open builds the chosen sink, refusing an ambiguous or absent choice.
//
// stdout is threaded through rather than read from a package variable, and
// that is not tidiness: the variable it replaced was written by each of the
// three provision commands just before this call, so two running at once
// raced on it — which the race detector caught the moment the CLI's tests
// ran in parallel. A writer is an argument the caller already has.
func (s sinkFlags) open(ctx context.Context, environ config.Source, stdout io.Writer) (
	provision.TokenSink, func(), error) {

	chosen := 0
	for _, on := range []bool{*s.secretStore, *s.envFile != "", *s.print} {
		if on {
			chosen++
		}
	}
	switch {
	case chosen == 0:
		return nil, nil, fmt.Errorf("%w: pass -secret-store, -env-file PATH or -print",
			provision.ErrNoSink)
	case chosen > 1:
		// REFUSED RATHER THAN ORDERED. Writing to two places doubles the
		// number of copies of a live credential, and picking one by
		// precedence would put it somewhere the operator did not ask for.
		return nil, nil, errors.New(
			"name exactly one of -secret-store, -env-file and -print")
	}

	switch {
	case *s.print:
		sink, err := provision.NewPrintSink(stdout)
		return sink, func() {}, err
	case *s.envFile != "":
		sink, err := provision.NewEnvFileSink(*s.envFile)
		return sink, func() {}, err
	}

	sv, closeStore, err := openSecretStore(ctx, environ, *s.bootstrap, *s.api)
	if err != nil {
		return nil, nil, err
	}
	return provision.NewSecretStoreSink(sinkStore{sv}, hostAuthor()), closeStore, nil
}

// sinkStore is a secret target as a provisioning sink's store. The author a
// row records is the store's to report and a sink has nobody to tell, so the
// answer is dropped here rather than widening the sink's own seam.
type sinkStore struct{ *secretTarget }

func (s sinkStore) Set(ctx context.Context, name, value string, by secrets.Author,
	source secrets.Source, now time.Time) error {

	_, err := s.secretTarget.Set(ctx, name, value, by, source, now)
	return err
}

// companyResolver builds the chain a run resolves Tier B ${VAR} references
// through: the company's secret store first, the environment behind it.
//
// THE SAME ORDER THE ENGINE USES, and it has to be the same one. Every
// command below reads the company's own values — the instance URL, the
// workspace, the webhook signing secret, the engine's read token — and a run
// that saw only the environment read an EMPTY STRING for every one an
// operator had already put in the store. That is not merely a missing value
// for the GitLab signing secret: empty is the signal to MINT, so the run
// replaced a working webhook secret at the third-party app with a fresh one and broke
// every delivery in flight until the config caught up. The store is where a
// rotated secret lives; a tool that provisions against it has to read it.
//
// # Which store, and the running engine decides it
//
// The same way [openSecretStore] decides where a write goes. A stopped node's
// own table is opened directly. A RUNNING engine holds that table under an OS
// lock ([store.ErrLocked]) for as long as it runs, and the company's values
// are then the fleet's, on the coordination KV inside the engine's own
// process, so they are read through its API ([storedThroughTheRunningNode]).
// Refusing instead made every one of these commands, and `crewlet llm`,
// unusable beside the node they exist to serve. An explicit apiURL is the
// command's instruction, and the read follows it rather than the local path,
// WHETHER OR NOT a Tier A is at this one: a command run against a named node
// from a machine that is not one would otherwise open, and create, an empty
// database at the Tier A path — or with no file there, read the environment
// alone — and resolve every stored value as unset.
//
// THROUGH A NODE, ONLY WHAT THIS COMMAND READS is fetched — the variables in
// reads, which each command takes from the fields it resolves and the tables
// its readers loop over — because each value comes back through
// `GET /secrets/{name}?reveal=true`: the one route that hands a credential
// out, logged by name against the caller and refused to a machine token.
// Revealing every value the company reads, for a command that reads two of
// them, put the whole credential set in this process and the node's log for
// nothing: `crewlet llm login` for one entry revealed every vendor's tokens,
// and a GitLab provision every model's. Names rather than document paths,
// because a path prefix cannot tell an entry `gpt-5` from `gpt-5.1`, nor a
// tool server `github` from `github.internal`. See [nodeValues] for why they
// are revealed before the command runs rather than one at a time as each is
// read.
//
// A node that holds no data (`store.scratch`) has no store of the company's
// to open, and is refused unless apiURL names one that does.
//
// # When there is no store
//
// A run with no bootstrap at this path resolves from the environment alone —
// the command is run somewhere other than a node, with `-public-url` naming
// the deployment — so it is a NOTE rather than a failure, and it is a note
// rather than silence: a mistyped -config resolving nothing has exactly the
// destructive outcome above, and an operator has to be able to see which
// chain ran. A bootstrap that exists always names a keyring, because Tier A
// refuses one that does not.
//
// A bootstrap that exists and cannot be read fails the run instead. Someone
// who configured a store and did not get it must not have their secrets
// quietly resolved from a stale export.
//
// IT ALSO HANDS BACK THE TIER A IT LOADED ([companyBootstrap]), which every
// caller now needs and none of them should load a second time. Nil when there
// is no Tier A file at all.
func companyResolver(ctx context.Context, environ config.Source, reads []string,
	bootstrapPath, apiURL string, notes io.Writer) (*config.Resolver, *config.Bootstrap, func(), error) {

	boot, err := companyBootstrap(bootstrapPath)
	if err != nil {
		return nil, nil, nil, err
	}
	if strings.TrimSpace(apiURL) != "" {
		client, cerr := newSecretsClient(environ, boot, apiURL)
		if cerr != nil {
			return nil, nil, nil, cerr
		}
		values, rerr := storedThrough(ctx, client, reads, notes)
		if rerr != nil {
			return nil, nil, nil, fmt.Errorf("read the company's stored values "+
				"through %s: %w%s", client.base, rerr, revealRemedy(rerr))
		}
		return config.WithStore(values), boot, func() {}, nil
	}
	if boot == nil {
		fmt.Fprintf(notes, "no %s: resolving ${VAR} from the environment only.\n",
			bootstrapPath)
		return config.EnvOnly(), nil, func() {}, nil
	}
	// A NODE THAT HOLDS NO DATA HAS NO STORE TO READ. Its file is scratch —
	// the previous run's, deleted at every boot — and the company's values
	// are a data node's, which nothing in its Tier A addresses. So the run
	// is told to name one, rather than resolve every stored value as unset:
	// it is a seats node, where the logins `crewlet llm` makes are made.
	if boot.Store.Scratch {
		return nil, nil, nil, fmt.Errorf("%s is a node that holds no data "+
			"(store.scratch), so the company's stored values are read through "+
			"a node that does: name one with -api, or run this on one",
			bootstrapPath)
	}
	sv, closeStore, err := openSecretValues(ctx, boot)
	if err != nil {
		values, rerr := storedThroughTheRunningNode(ctx, environ, boot, bootstrapPath,
			reads, err, notes)
		if rerr != nil {
			return nil, nil, nil, rerr
		}
		return config.WithStore(values), boot, func() {}, nil
	}
	values, err := sv.All(ctx)
	if err != nil {
		closeStore()
		return nil, nil, nil, fmt.Errorf("read the secret store: %w", err)
	}
	sayWhatAStoppedNodeCannotRead(notes, reads, values, boot.Store.Path)
	return config.WithStore(config.MapSource(values)), boot, closeStore, nil
}

// sayWhatAStoppedNodeCannotRead names each value the command reads that a
// stopped node's own table and the environment both leave unset.
//
// THE TABLE IS NOT THE FLEET'S STORE. It holds what was stored while the
// engine was stopped, and the engine moves those rows onto the fleet at its
// next start and deletes them here ([fleetsecrets.Migrate]); the fleet's own
// rows are on the coordination KV, which a stopped engine is not serving.
// So a name in neither place may be one the fleet holds, and this run
// cannot tell — it resolves it as unset, which for a key a provisioner
// mints when it resolves empty replaces a working one. Said on every run
// that meets one, naming the ways to the fleet's values, rather than
// refused: before a node's first start the table is the whole store, and a
// value nobody has stored yet is exactly what `crewlet llm login` and a
// first provisioning run are there to write.
func sayWhatAStoppedNodeCannotRead(notes io.Writer, reads []string,
	local map[string]string, storePath string) {

	var unread []string
	for _, name := range reads {
		if _, stored := local[name]; stored {
			continue
		}
		if _, exported := os.LookupEnv(name); exported {
			continue
		}
		unread = append(unread, "${"+name+"}")
	}
	if len(unread) == 0 {
		return
	}
	fmt.Fprintf(notes, "this node's engine is not running, so the fleet's stored "+
		"values cannot be read: %s — in neither %s, which holds only what was "+
		"stored while the engine was stopped, nor the environment — resolve as "+
		"unset here. If the fleet holds any of them, start the engine and re-run, "+
		"or name a running node with -api.\n",
		strings.Join(unread, ", "), storePath)
}

// companyBootstrap is the Tier A a command reading the company runs under,
// or nil when there is no file at this path — a supported way to run one,
// away from any node. A file that exists and does not load is the error.
//
// Its own function because one command needs the document and resolves
// nothing: `crewlet slack provision` reads only where vendors reach this
// deployment ([webhookBase]), and resolving the company's values for it would
// open the store, or reveal credentials through the running node, for a run
// that reads none of them.
func companyBootstrap(bootstrapPath string) (*config.Bootstrap, error) {
	if _, err := os.Stat(bootstrapPath); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return loadBootstrapForStore(bootstrapPath)
}

// revealRemedy is what a read through a node that failed tells the operator to
// change: the token, where the node refused it, and nothing more where it did
// not — a node that is down, draining or failing answers the same for any
// token, and naming the token then sends an operator to change one that works.
func revealRemedy(err error) string {
	if refusedTheToken(err) {
		return revealTakesATierAToken
	}
	return ""
}

// revealTakesATierAToken is what a read through a node that was refused
// tells the operator to change: reading a value is a reveal, which the
// authority table gives admin with a person present, so a machine token —
// which acts for its owner with nobody there — is refused it whatever it
// carries, and the token that reads is one of Tier A's.
const revealTakesATierAToken = "\n\nEach value is read with a reveal, which takes admin and " +
	"a person present: " + apiTokenEnv + " must hold one of Tier A's api.auth " +
	"tokens, since a machine token is refused a reveal whatever it carries"

// storedThroughTheRunningNode answers a locked store with the values the
// command reads, through the node holding it — the read half of
// [throughTheRunningNode], and [store.ErrLocked] is the only error it routes
// for the same reason: every other failure is a broken node, whose own
// message an HTTP attempt would replace with a connection refused.
func storedThroughTheRunningNode(ctx context.Context, environ config.Source,
	boot *config.Bootstrap, bootstrapPath string, reads []string,
	err error, notes io.Writer) (*nodeValues, error) {

	if !errors.Is(err, store.ErrLocked) {
		return nil, err
	}
	client, cerr := newSecretsClient(environ, boot, "")
	if cerr == nil {
		var values *nodeValues
		if values, cerr = storedThrough(ctx, client, reads, notes); cerr == nil {
			return values, nil
		}
	}
	// BOTH FACTS, as a write's refusal gives them: the lock alone reads as
	// "blocked, no way forward", and the API's complaint alone as though
	// this node's own table were an answer. It is not one while the engine
	// runs — the company's values are the fleet's then — so stopping the
	// engine is not offered as the way round.
	return nil, fmt.Errorf("%w\n\nthe engine for %s is running and holds its "+
		"database, so the company's stored values are read through its API — "+
		"and they cannot be: %w%s", err, bootstrapPath, cerr, revealRemedy(cerr))
}

// storedThrough reads, through a running node, the stored values a command
// reads, and says on notes that it did.
//
// THE LISTING FIRST, which reveals nothing: a name the command reads and the
// fleet does not hold is left to the environment without a request, exactly
// as the local table's absence leaves it. A name that went between the
// listing and its read is absent the same way.
func storedThrough(ctx context.Context, client *secretsClient, reads []string,
	notes io.Writer) (*nodeValues, error) {

	rows, err := client.List(ctx)
	if err != nil {
		return nil, err
	}
	values := &nodeValues{
		values: map[string]string{},
		held:   make(map[string]bool, len(rows)),
		reveal: func(name string) (string, error) { return client.Get(ctx, name) },
		where:  client.base,
		notes:  notes,
	}
	for _, row := range rows {
		values.held[row.Name] = true
	}
	for _, name := range unionReads(reads) {
		if _, _, err := values.revealLocked(name); err != nil {
			return nil, err
		}
	}
	// SAID EVERY TIME, as the environment-only chain is: which chain ran is
	// what an operator reads first when a value is not the one they set, and
	// each read here is a line in the node's log under their token.
	fmt.Fprintf(notes, "resolving ${VAR} through the running node at %s: %d of "+
		"the stored values this command reads revealed there, each logged by name.\n",
		client.base, len(values.values))
	return values, nil
}

// nodeValues is the stored values one command reads, through the running
// node that holds them: a [config.Source] over the fleet's rows that reveals
// only what is read.
//
// # The command's scope is revealed before it runs
//
// Not one value at a time as the command reads it, which would reveal no
// more and fail worse. A read is a [config.Source] lookup, which has no
// error to return: a reveal the node refused — a machine token, which is
// refused every one — could only come back as "unset", and unset is what
// tells a provisioning run to MINT, so a GitLab provision would replace the
// key a working hook signs with because its token could not read the one
// in the store. Revealed up front, the same refusal stops the command
// before it has done anything.
//
// # A read outside the scope is still answered from the fleet
//
// Revealed as it is made, and said on notes. A command whose scope missed a
// field it reads — a new one, say — then resolves the fleet's value rather
// than the environment's, which is the engine's answer; resolving it as
// unset would be the same mint. That reveal has no error to return either,
// so a refused one PANICS: the run stops where it is, as it would on a
// vendor's own failure part way through, rather than going on to act on a
// value it does not have.
type nodeValues struct {
	// mu guards values and held: a provisioner may read from more than one
	// goroutine, and a reveal is a request the lock holds across.
	mu sync.Mutex

	// values is every stored value revealed so far, by name.
	values map[string]string

	// held is every name the node listed, less those found gone since.
	held map[string]bool

	// reveal reads one value through the node.
	reveal func(name string) (string, error)

	// where is the node's address, for what is said on notes.
	where string
	notes io.Writer
}

// Lookup implements [config.Source].
func (n *nodeValues) Lookup(name string) (string, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if value, ok := n.values[name]; ok {
		return value, true
	}
	if !n.held[name] {
		return "", false
	}
	value, ok, err := n.revealLocked(name)
	if err != nil {
		panic(readFailed{fmt.Errorf("read ${%s} through the running node at %s: %w — "+
			"stopping rather than resolving a stored value as unset%s",
			name, n.where, err, revealRemedy(err))})
	}
	if ok {
		fmt.Fprintf(n.notes, "revealed ${%s} through the running node at %s as it "+
			"was read, beyond the values this command reveals before it runs.\n",
			name, n.where)
	}
	return value, ok
}

// readFailed is a read through a running node that failed part way through a
// command, carried by a panic out of the [nodeValues.Lookup] that has no error
// to return, and answered at the command's top as the error it is ([runIn]).
// A panic, because the alternative answer — unset — is the one that mints.
type readFailed struct{ err error }

func (r readFailed) Error() string { return r.err.Error() }
func (r readFailed) Unwrap() error { return r.err }

// revealLocked reveals one held name, reporting whether the node still held
// it. n.mu is held, or n is not yet shared.
func (n *nodeValues) revealLocked(name string) (string, bool, error) {
	if !n.held[name] {
		return "", false, nil
	}
	value, err := n.reveal(name)
	switch {
	case errors.Is(err, secrets.ErrNotFound):
		delete(n.held, name)
		return "", false, nil
	case err != nil:
		return "", false, err
	}
	n.values[name] = value
	return value, true, nil
}

// fieldReads is every variable these fields reference, each once, sorted:
// what a command reads behind the fields of the document it resolves.
func fieldReads(fields ...string) []string {
	var names []string
	for _, field := range fields {
		names = append(names, envref.Names(field)...)
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// unionReads is every name in these lists, each once, sorted.
func unionReads(lists ...[]string) []string {
	names := slices.Concat(lists...)
	slices.Sort(names)
	return slices.Compact(names)
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

// gitlabProvisionReads is every variable `crewlet gitlab provision` reads of
// the company: the instance, and the key its hooks sign with, which it mints
// when that resolves empty — the names a running node is asked to reveal
// ([companyResolver]). Not the engine's read token, which it never uses: it
// works with the operator's admin token, read from the environment alone
// ([operatorCredential]).
func gitlabProvisionReads(cfg *config.GitLab) []string {
	return fieldReads(cfg.URL, cfg.SigningSecret)
}

// runGitLabProvision is `crewlet gitlab provision`.
func runGitLabProvision(environ config.Source, args []string, stdout, stderr io.Writer) error {
	companyPath, args := splitSubject(args)

	fs := flag.NewFlagSet("gitlab provision", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sinks := addSinkFlags(fs)
	adminToken := fs.String("admin-token", "",
		"a GitLab token permitted to create service accounts; empty reads "+
			"GITLAB_ADMIN_TOKEN")
	publicURL := fs.String("public-url", "",
		"this deployment's public base URL, for registering the webhook; "+
			"defaults to "+publicURLDefault)
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

	company, err := config.LoadCompanyToRun(companyPath, nil)
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
	env, boot, closeEnv, err := companyResolver(ctx, environ,
		gitlabProvisionReads(cfg), *sinks.bootstrap,
		*sinks.api, stdout)
	if err != nil {
		return err
	}
	defer closeEnv()

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
		env.Value(cfg.SigningSecret), signingVar, *rotate, base != "", baseKey)

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

	sink, closeSink, err := sinks.open(ctx, environ, stdout)
	if err != nil {
		return err
	}
	defer closeSink()

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

// mattermostProvisionReads is every variable `crewlet mattermost provision`
// reads of the company: the instance's address alone — the name a running
// node is asked to reveal ([companyResolver]). The bots' tokens are what it
// mints, and it works with the operator's admin token.
func mattermostProvisionReads(cfg *config.Mattermost) []string {
	if cfg == nil {
		return nil
	}
	return fieldReads(cfg.URL)
}

// runMattermostProvision is `crewlet mattermost provision`.
func runMattermostProvision(environ config.Source, args []string, stdout, stderr io.Writer) error {
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

	company, err := config.LoadCompanyToRun(companyPath, nil)
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
	sink, closeSink, err := sinks.open(ctx, environ, stdout)
	if err != nil {
		return err
	}
	defer closeSink()

	env, _, closeEnv, err := companyResolver(ctx, environ,
		mattermostProvisionReads(cfg), *sinks.bootstrap,
		*sinks.api, stdout)
	if err != nil {
		return err
	}
	defer closeEnv()

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

// mattermostDoctorReads is every variable `crewlet mattermost doctor` reads
// of the company: the instance and its team, and each seat's bot token, which
// its checks authenticate with ([mattermost.SeatTokenReads]) — the names a
// running node is asked to reveal ([companyResolver]).
func mattermostDoctorReads(cfg *config.Mattermost, o *org.Organization) []string {
	return unionReads(fieldReads(cfg.URL, cfg.Team), mattermost.SeatTokenReads(o))
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
	bootstrap := bootstrapFlag(fs)
	apiURL := readAPIFlag(fs)
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

	company, err := config.LoadCompanyToRun(companyPath, nil)
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
	env, _, closeEnv, err := companyResolver(ctx, environ,
		mattermostDoctorReads(cfg, organization), *bootstrap, *apiURL, stdout)
	if err != nil {
		return err
	}
	defer closeEnv()

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
		SeatToken: mattermost.SeatTokens(env), BrowsedAt: browsedAt,
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
