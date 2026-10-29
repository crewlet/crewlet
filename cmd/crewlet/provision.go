package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/gitlab"
	"github.com/crewlet/crewlet/internal/mattermost"
	"github.com/crewlet/crewlet/internal/org"
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
//
// # A sink the company's store outranks
//
// `-env-file` and `-print` hand what a run mints to the operator, for the
// engine to read from its environment — and the engine resolves every `${VAR}`
// from the company's secret store FIRST. So where the store holds a name the
// run mints into, what the run records is never read: the vendor holds the new
// value and the engine goes on with the stored one. For a seat's token that is
// the outage the re-run rule exists to prevent — GitLab retires a seat's
// earlier tokens once a new one is recorded, so a seat the engine runs on the
// stored token is locked out — and for a webhook secret it is a hook re-keyed
// with a key the engine does not hold, every delivery refused. A `gitlab
// provision -rotate -env-file`, the remedy for a leaked key, would re-key the
// hook while the engine went on verifying with the leaked one.
//
// So a run with one of those sinks that has a store to ask — the running node
// or the one -api names, or the fleet's store reached directly with this
// node's engine stopped — is refused before it reaches
// the vendor where the store holds any name it can mint into
// ([sinkFlags.refuseOutranked]): every planned seat's token, since whether a
// seat is kept or re-minted is settled against the vendor, part way through
// the run; and a webhook secret the run would mint, under its flag for a fresh
// one or where the reference resolves to nothing. Asking the sink instead
// would not do: an env file's value for a name the store also holds is not the
// one the engine runs on, so a seat kept because that value still
// authenticates is reported healthy on a token the engine does not use.
// `-secret-store` is the one sink the store does not outrank: it reads each
// stored value back, and records over it.

// sinkFlags is the shared --secret-store / --env-file / --print choice, and the
// store a run reads the company's values from and records into.
type sinkFlags struct {
	secretStore *bool
	envFile     *string
	print       *bool

	// store is the run's `-config` and `-api`: -secret-store records
	// through the route the run's reads take ([companyStore]).
	store storeFlags
}

func addSinkFlags(fs *flag.FlagSet) sinkFlags {
	return sinkFlags{
		secretStore: fs.Bool("secret-store", false,
			"record minted credentials in the encrypted secret store"),
		envFile: fs.String("env-file", "",
			"record minted credentials in this .env file"),
		print: fs.Bool("print", false,
			"print minted credentials to stdout and persist nothing"),
		store: addStoreFlags(fs),
	}
}

// recordsInStore reports whether the run records into the secret store, whose
// sink reads a seat's token back through the run's route — so the names it
// reads back go into the batch the run reads up front.
func (s sinkFlags) recordsInStore() bool { return *s.secretStore }

// open builds the chosen sink, refusing an ambiguous or absent choice.
// -secret-store records into the run's own store ([companyStore.recordInto]),
// which companySecrets owns and closes.
//
// stdout is threaded through rather than read from a package variable, and
// that is not tidiness: the variable it replaced was written by each of the
// three provision commands just before this call, so two running at once
// raced on it — which the race detector caught the moment the CLI's tests
// ran in parallel. A writer is an argument the caller already has.
func (s sinkFlags) open(ctx context.Context, companySecrets *companyStore, stdout io.Writer) (
	provision.TokenSink, error) {

	if err := s.check(); err != nil {
		return nil, err
	}
	switch {
	case *s.print:
		return provision.NewPrintSink(stdout)
	case *s.envFile != "":
		return provision.NewEnvFileSink(*s.envFile)
	}

	store, err := companySecrets.recordInto(ctx)
	if err != nil {
		return nil, err
	}
	sink := provision.NewSecretStoreSink(store, hostAuthor())
	if store.target.throughNode {
		client, _ := store.target.secretBackend.(*secretsClient)
		if client == nil {
			return nil, errors.New("a store reached through a node has no client to re-activate it with")
		}
		return &nodeSecretSink{SecretStoreSink: sink, reload: client.Reload,
			retries: flushRetries}, nil
	}
	return sink, nil
}

// nodeSecretSink is the secret store reached through a running node: each
// value sealed the moment it is minted with its re-activation deferred, and
// ONE re-activation at the end of the run (ADR-0070), `POST /config/reload`,
// which seals nothing: a value somebody rotated while the run went on is never
// written back over.
//
// A RE-ACTIVATION THAT FAILS IS NEVER A ROLLBACK. The values are sealed and the
// accounts they belong to exist, so a run whose flush could not re-activate
// the company has done its work; the next step says what is left, and nothing
// is revoked.
type nodeSecretSink struct {
	*provision.SecretStoreSink

	// reload re-activates the company the node runs ([secretsClient.Reload]).
	reload func(ctx context.Context, summary string) (*configapi.Reloaded, error)

	// retries is how long Flush waits before each retry: [flushRetries],
	// and nothing in a test.
	retries []time.Duration

	mu       sync.Mutex
	recorded []string
	// outcome is what the flush's re-activation did, for the next step:
	// nil before a flush that had anything to re-activate.
	outcome *flushOutcome
}

// flushOutcome is what a flush's re-activation did.
type flushOutcome struct {
	revision string
	epoch    int64
	failed   error
}

// Record implements [provision.TokenSink]: sealed now, re-activated at Flush.
func (s *nodeSecretSink) Record(ctx context.Context, name, value string) error {
	if err := s.SecretStoreSink.Record(ctx, name, value); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !slices.Contains(s.recorded, name) {
		s.recorded = append(s.recorded, name)
	}
	return nil
}

// Discard implements [provision.TokenSink]: a rolled-back value is not
// re-activated at the end.
func (s *nodeSecretSink) Discard(ctx context.Context) error {
	s.mu.Lock()
	s.recorded = nil
	s.mu.Unlock()
	return s.SecretStoreSink.Discard(ctx)
}

// Flush implements [provision.TokenSink]: the run's one re-activation, for
// every value it sealed.
//
// IT RETURNS NO ERROR FOR A RE-ACTIVATION THAT FAILED, because an error from
// Flush rolls the run back, revoking credentials that are sealed and good: the
// outcome is the next step's to say ([nodeSecretSink.NextStep]). A transient
// failure is retried first ([flushRetries]). A run that dies before its flush
// leaves its values sealed and inactive until the company is next
// re-activated, by `POST /config/reload` or any write that activates it.
func (s *nodeSecretSink) Flush(ctx context.Context) error {
	s.mu.Lock()
	names := slices.Clone(s.recorded)
	s.mu.Unlock()
	if len(names) == 0 {
		return s.SecretStoreSink.Flush(ctx)
	}
	summary := "reload: provisioning sealed " + strings.Join(names, ", ")
	outcome := &flushOutcome{}
	for attempt := 0; ; attempt++ {
		reloaded, err := s.reload(ctx, summary)
		if err == nil {
			outcome = &flushOutcome{revision: reloaded.RevisionID, epoch: reloaded.Epoch}
			break
		}
		outcome.failed = err
		if attempt >= len(s.retries) || !transientFlush(err) {
			break
		}
		select {
		case <-ctx.Done():
			outcome.failed = errors.Join(err, ctx.Err())
		case <-time.After(s.retries[attempt]):
			continue
		}
		break
	}
	s.mu.Lock()
	s.outcome = outcome
	if outcome.failed == nil {
		s.recorded = nil
	}
	s.mu.Unlock()
	return s.SecretStoreSink.Flush(ctx)
}

// NextStep implements [provision.TokenSink]: nothing, once the run's
// re-activation landed; the re-activation to make, with why, when it did not.
func (s *nodeSecretSink) NextStep() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch out := s.outcome; {
	case out == nil:
		return "nothing more: the node re-activates the revision that reads the values " +
			"this run sealed at the end of the run"
	case out.failed != nil:
		return fmt.Sprintf("the values are sealed and the running company was NOT re-activated "+
			"(%v): POST /config/reload to a node that is up re-activates it", out.failed)
	default:
		return fmt.Sprintf("nothing more: the node re-activated the company once, as revision "+
			"%s, epoch %d, so the running fleet reads the values this run sealed", out.revision, out.epoch)
	}
}

// flushRetries is how long a provisioning run's flush waits before each
// retry of its re-activation: two retries, two then five seconds, which is a
// coordination store's leader election (the config surface answers one with
// Retry-After: 2) with room to spare, and short enough that a person at the
// terminal is told within ten seconds that it did not land. A sink built in a
// test waits none.
var flushRetries = []time.Duration{2 * time.Second, 5 * time.Second}

// transientFlush reports whether a flush's failure may pass on its own: a
// node that did not answer or answered 5xx, or a re-activation that lost a
// race with another write. A refusal is not.
func transientFlush(err error) bool {
	var refused *nodeRefusal
	if !errors.As(err, &refused) {
		return true
	}
	return refused.Status >= http.StatusInternalServerError ||
		refused.Code == string(httpjson.CodeRevisionAdvanced)
}

// check refuses an ambiguous or absent choice of sink, before anything reads
// or opens one.
func (s sinkFlags) check() error {
	chosen := 0
	for _, on := range []bool{*s.secretStore, *s.envFile != "", *s.print} {
		if on {
			chosen++
		}
	}
	switch {
	case chosen == 0:
		return fmt.Errorf("%w: pass -secret-store, -env-file PATH or -print",
			provision.ErrNoSink)
	case chosen > 1:
		// REFUSED RATHER THAN ORDERED. Writing to two places doubles the
		// number of copies of a live credential, and picking one by
		// precedence would put it somewhere the operator did not ask for.
		return errors.New(
			"name exactly one of -secret-store, -env-file and -print")
	}
	return nil
}

// describe names the sink a run with no `-secret-store` chose, as a refusal
// names it.
func (s sinkFlags) describe() string {
	if *s.print {
		return "this run's output (-print)"
	}
	return "the env file " + *s.envFile
}

// refuseOutranked refuses a run whose sink the company's store outranks — an
// env file, or the run's own output — where that store holds any of mintsInto,
// the variables the run can mint a value into; the vendor is named for what
// the refusal says it would hold. See the package doc's "A sink the company's
// store outranks" for why.
//
// It asks the store by its listing alone, through the run's route: a
// provisioner that has read the company has decided that route already, and
// one that reads nothing else — Slack with no `-secret-store` — decides it
// here, before it prints anything. A run with no store (no Tier A file and no
// -api) has nothing to be outranked by, and a run that mints into nothing has
// nothing to ask.
func (s sinkFlags) refuseOutranked(ctx context.Context, companySecrets *companyStore,
	vendor string, mintsInto []string, notes io.Writer) error {

	if *s.secretStore || len(mintsInto) == 0 {
		return nil
	}
	held, store, err := companySecrets.holding(ctx, mintsInto, notes)
	if err != nil || len(held) == 0 {
		return err
	}
	return &outrankedSinkError{command: companySecrets.command, vendor: vendor,
		store: store, sink: s.describe(), names: held}
}

// errOutrankedSink is the refusal of a provisioning run whose sink the
// company's secret store outranks, where the store holds a name the run can
// mint into ([sinkFlags.refuseOutranked]).
var errOutrankedSink = errors.New("the company's secret store outranks this run's sink")

// outrankedSinkError is that refusal: the store and the names it holds, the
// sink the run chose, and the two ways forward.
type outrankedSinkError struct {
	command, vendor string
	// store is where the run read the store's names, as said.
	store string
	// sink is the sink the run chose, as said.
	sink string
	// names is every variable the run can mint into that the store holds.
	names []string
}

// Is makes the refusal [errOutrankedSink].
func (e *outrankedSinkError) Is(target error) bool { return target == errOutrankedSink }

func (e *outrankedSinkError) Error() string {
	refs := make([]string, len(e.names))
	for i, name := range e.names {
		refs[i] = "${" + name + "}"
	}
	held := refs[0]
	if n := len(refs); n > 1 {
		held = strings.Join(refs[:n-1], ", ") + " and " + refs[n-1]
	}
	return fmt.Sprintf("%s: the company's secret store (%s) holds %s, which this run "+
		"can mint into, and the only place it would record a new value is %s. The "+
		"engine resolves every ${VAR} from that store before its environment, so what "+
		"the run minted would never be read: %s would hold it, and the engine would go "+
		"on with the stored value. Record with -secret-store instead, which reads the "+
		"stored value back and writes into that store, or remove the value from the "+
		"store first (crewlet secrets unset NAME). Nothing was sent to %s.",
		e.command, e.store, held, e.sink, e.vendor, e.vendor)
}

// freshSwitch is a provisioning command's flag for a fresh webhook secret —
// `-recreate-webhooks` on github and confluence, `-recreate-webhook` on jira —
// declared once, by name.
//
// THE FLAG AND ITS SPELLING ARE ONE DECLARATION. A pass refuses the switch
// over a secret the company document holds as a literal, naming it as the
// surface that set it spells it ([provision.FreshSwitch]), so the command
// hands its pass the name it parsed. A boolean flag beside a separate literal
// spelling could drift apart, and the refusal would then send the operator to
// a flag the command does not have.
type freshSwitch struct {
	name string
	on   *bool
}

// addFreshSwitch declares the flag named name on fs.
func addFreshSwitch(fs *flag.FlagSet, name, usage string) freshSwitch {
	return freshSwitch{name: name, on: fs.Bool(name, false, usage)}
}

// value is the flag as a pass takes it: spelled as the command declares it
// (`-recreate-webhooks`, the spelling its usage prints, whichever form the
// operator typed it in), and empty where the run did not pass it, which is
// [provision.FreshSwitch]'s "not asked".
func (f freshSwitch) value() provision.FreshSwitch {
	if f.on == nil || !*f.on {
		return ""
	}
	return provision.FreshSwitch("-" + f.name)
}

// webhookMintsInto is the variable a run can mint a webhook secret into: the
// whole `${VAR}` ref names, where the run registers a hook at all, and either
// asked for a fresh secret or ref resolves to nothing — a secret that resolves
// is registered as it is. base is the address the run registers its hooks at,
// and empty where it registers none: no webhook address, or nothing to
// register one on (GitHub with no `integrations.github.provisioning`), which
// its caller knows and this does not.
func webhookMintsInto(ref string, fresh bool, value func(string) string, base string) []string {
	name := soleVarOf(ref)
	if name == "" || base == "" || !fresh && strings.TrimSpace(value(ref)) != "" {
		return nil
	}
	return []string{name}
}

// gitlabMintsInto is every variable a `crewlet gitlab provision` run can mint
// into: each planned seat's token — a seat is kept or re-minted on what the
// vendor says of the token the sink holds, which the run learns only part way
// through — and the signing secret where its plan mints or rotates one.
func gitlabMintsInto(plan *provision.Plan, signing gitlab.SigningPlan) []string {
	names := seatTokenVars(plan, nil)
	if signing.Action == gitlab.SigningMint || signing.Action == gitlab.SigningRotate {
		names = append(names, signing.Var)
	}
	return unionReads(names)
}

// sinkStore is the run's store as a provisioning sink's: writes go to its
// route, and a read back is answered from the values the run read up front
// ([storedValues.get]), with a one-name read for a held name nothing read
// yet — so a provisioning run through a node reveals the seats' tokens it
// checks in its first batch rather than one request each, part way through.
//
// The author a row records is the store's to report and a sink has nobody to
// tell, so the answer is dropped here rather than widening the sink's own
// seam.
type sinkStore struct {
	target *secretTarget
	values *storedValues
}

func (s sinkStore) Set(ctx context.Context, name, value string, by secrets.Author,
	source secrets.Source, now time.Time) error {

	// DURABLE NOW, RE-ACTIVATED ONCE: a minted credential is sealed the
	// moment it exists (provision.TokenSink.Record), and the run's last
	// write re-activates for every value it sealed ([nodeSecretSink.Flush]),
	// so a run that mints N credentials moves a running fleet onto one
	// epoch rather than N.
	if _, err := s.target.SetMany(ctx, map[string]string{name: value}, by, source, now,
		true); err != nil {
		return err
	}
	s.values.recorded(name, value)
	return nil
}

func (s sinkStore) Get(_ context.Context, name string) (string, error) {
	return s.values.get(name)
}

func (s sinkStore) Unset(ctx context.Context, name string) (bool, error) {
	gone, err := s.target.Unset(ctx, name)
	if err != nil {
		return false, err
	}
	s.values.forgot(name)
	return gone, nil
}

// seatTokenVars is the variable each planned seat's token is recorded under,
// for the seats a run provisions — every one, or those only names.
func seatTokenVars(plan *provision.Plan, only []string) []string {
	var names []string
	for _, seat := range plan.Seats {
		if len(only) == 0 || slices.Contains(only, seat.Handle) {
			names = append(names, seat.TokenVar)
		}
	}
	return unionReads(names)
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
// invocation's ENVIRONMENT ALONE ([runIn]): the process's, for every run of
// the binary.
//
// Deliberately NOT through [companyStore.resolver], which every company value
// goes through. This is the operator's credential rather than the company's, and
// the difference is not bookkeeping: a GitLab admin PAT carries `api` scope
// over the whole group, and the secret store is replicated to every node
// that holds the keyring. Crewlet never persists one, and reading one back
// from the store would imply it may be kept there — which is how the most
// powerful credential in the deployment ends up in the shared table beside
// the seat tokens it exists to mint.
//
// THE INVOCATION'S, NOT THE PROCESS'S, as the node credential is: a case that
// reads the process's runs on whatever the machine running the suite exports,
// and a Slack refresh token is spent by the rotation a run makes with it.
func operatorCredential(environ config.Source, name string) string {
	value, _ := environ.Lookup(name)
	return strings.TrimSpace(value)
}

// gitlabProvisionReads is every variable `crewlet gitlab provision` reads of
// the company: the instance, and the key its hooks sign with, which it mints
// when that resolves empty — the names a running node is asked to reveal
// ([companyStore.resolver]). Not the engine's read token, which it never uses: it
// works with the operator's admin token, read from the environment alone
// ([operatorCredential]).
func gitlabProvisionReads(cfg *config.GitLab) []string {
	return fieldReads(cfg.URL, cfg.SigningSecret)
}

// runGitLabProvision is `crewlet gitlab provision`.
func runGitLabProvision(inv invocation, args []string, stdout, stderr io.Writer) error {
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
			"one still works, and a fresh webhook signing secret into the "+
			"variable signing_secret names wherever the hook is registered "+
			"(the run then says what the engine needs to read them)")
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
	// THE SINK IS CHOSEN BEFORE THE ROUTE IS DECIDED, wherever the run will
	// open one: a run naming none, or two, is a mistake in its command line,
	// and it is told so — not to start the engine or name a node, which
	// would not fix it, nor after a broker has started to read the store.
	if !*dryRun && !plan.Empty() {
		if err = sinks.check(); err != nil {
			return err
		}
	}

	// RESOLVED BEFORE THE PLAN IS PRINTED, because the plan's most
	// consequential line depends on it: whether this run will replace the
	// key a working hook signs with. Reading the store is not a mutation of
	// it, so a -dry-run does it too — a dry run that could not say this
	// would be silent about the one outcome an operator most needs warning
	// of — and is refused like a real run where the store cannot be read.
	// With the engine stopped that read starts this node's broker, which
	// on a node that has never booted creates the fleet's buckets as the
	// engine's first boot would (engine.OpenFleetStore): the store is made,
	// not changed.
	ctx := context.Background()
	companySecrets, err := openCompanyStore(inv.environ, sinks.store,
		"crewlet gitlab provision")
	if err != nil {
		return err
	}
	defer companySecrets.Close()
	reads := gitlabProvisionReads(cfg)
	if sinks.recordsInStore() && !*dryRun && !*rotate {
		// AND EACH SEAT'S TOKEN, IN THE SAME BATCH: a seat whose account
		// exists is kept or re-minted on what the sink reads back for it,
		// and -rotate reads none of them back.
		reads = unionReads(reads, seatTokenVars(plan, nil))
	}
	env, err := companySecrets.resolver(ctx, reads, stdout)
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
		env.Value(cfg.SigningSecret), signingVar, *rotate, base != "", baseKey)

	// A REAL RUN THE STORE OUTRANKS IS REFUSED BEFORE ITS PLAN PRINTS: the
	// plan says what it would mint, and none of it happens.
	if !*dryRun && !plan.Empty() {
		if err = sinks.refuseOutranked(ctx, companySecrets, "GitLab",
			gitlabMintsInto(plan, signing), stdout); err != nil {
			return err
		}
	}

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

	sink, err := sinks.open(ctx, companySecrets, stdout)
	if err != nil {
		return err
	}

	token := strings.TrimSpace(*adminToken)
	if token == "" {
		token = operatorCredential(inv.environ, "GITLAB_ADMIN_TOKEN")
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
	printKept(w, res.Kept, "-rotate")
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

// printKept reports the seats a run deliberately did not touch, naming fresh,
// the command's own flag for minting their credentials anyway.
//
// SAID OUT LOUD, because it is the successful outcome of a re-run: a report
// that mentioned only what changed would read as a run that did nothing,
// and the operator's next move would be to reach for that flag — which is
// exactly the outage this behaviour exists to prevent.
//
// THE FLAG IS THE CALLER'S, because the commands sharing this line do not
// share one: GitLab and Mattermost mint afresh with -rotate, Slack with
// -reinstall, and a line naming -rotate to a Slack operator named a flag
// that command refuses. And the step after it is the sink's, said on that
// run's own Next: line ([printNextStep]): a restart is what -env-file and
// -print need, while -secret-store needs a reload of a running node.
func printKept(w io.Writer, kept []string, fresh string) {
	if len(kept) == 0 {
		return
	}
	fmt.Fprintf(w, "Left %d seat(s) alone — their credential still works, "+
		"and rotating it would revoke what the running engine is using: %s\n"+
		"  (pass %s to mint fresh ones; that run's Next: line says what the "+
		"engine then needs)\n",
		len(kept), strings.Join(kept, ", "), fresh)
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
	// run takes the invocation ([runIn]) first, because a command that
	// stores what it mints may reach the running node's /secrets with the
	// credential read from its environment, a command that takes an
	// operator credential (GitLab's, Mattermost's, Slack's) reads it from
	// there ([operatorCredential]), and Slack's reaches its Web API through
	// the invocation's client.
	run func(inv invocation, args []string, stdout, stderr io.Writer) error
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
func runIntegration(inv invocation, vendor string, args []string, stdout, stderr io.Writer) error {
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
			return command.run(inv, rest, stdout, stderr)
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
// node is asked to reveal ([companyStore.resolver]). The bots' tokens are what
// it mints, and it works with the operator's admin token.
func mattermostProvisionReads(cfg *config.Mattermost) []string {
	if cfg == nil {
		return nil
	}
	return fieldReads(cfg.URL)
}

// runMattermostProvision is `crewlet mattermost provision`.
func runMattermostProvision(inv invocation, args []string, stdout, stderr io.Writer) error {
	companyPath, args := splitSubject(args)

	fs := flag.NewFlagSet("mattermost provision", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sinks := addSinkFlags(fs)
	adminToken := fs.String("admin-token", "",
		"a Mattermost token for a system administrator; empty reads MATTERMOST_ADMIN_TOKEN")
	rotate := fs.Bool("rotate", false,
		"mint a fresh token for every bot, including bots whose current "+
			"one still works (the run then says what the engine needs to "+
			"read them)")
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

	// A DRY RUN AND AN EMPTY PLAN READ NO STORED VALUE, so they decide no
	// route and run on any node. A real run decides its route before it
	// prints anything, so a run the store refuses prints only the refusal.
	what := "a Mattermost bot token (its role's mattermost.bot_token)"
	if *dryRun || plan.Empty() {
		printPlan(stdout, plan, what)
		if *dryRun {
			fmt.Fprintln(stdout, "\n-dry-run: nothing was created, joined or minted.")
		}
		return nil
	}

	if err = sinks.check(); err != nil {
		return err
	}
	ctx := context.Background()
	companySecrets, err := openCompanyStore(inv.environ, sinks.store,
		"crewlet mattermost provision")
	if err != nil {
		return err
	}
	defer companySecrets.Close()
	reads := mattermostProvisionReads(cfg)
	if sinks.recordsInStore() && !*rotate {
		// AND EACH BOT'S TOKEN, IN THE SAME BATCH: a bot that exists is
		// kept or re-minted on what the sink reads back for it.
		reads = unionReads(reads, seatTokenVars(plan, splitHandles(*only)))
	}
	env, err := companySecrets.resolver(ctx, reads, stdout)
	if err != nil {
		return err
	}
	// EVERY PLANNED BOT'S TOKEN is a name the run can mint into: a bot is
	// kept or re-minted on what the server says of the token its sink holds.
	if err = sinks.refuseOutranked(ctx, companySecrets, "Mattermost",
		seatTokenVars(plan, splitHandles(*only)), stdout); err != nil {
		return err
	}
	printPlan(stdout, plan, what)
	sink, err := sinks.open(ctx, companySecrets, stdout)
	if err != nil {
		return err
	}

	token := strings.TrimSpace(*adminToken)
	if token == "" {
		token = operatorCredential(inv.environ, "MATTERMOST_ADMIN_TOKEN")
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
	printKept(w, res.Kept, "-rotate")
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
// running node is asked to reveal ([companyStore.resolver]).
func mattermostDoctorReads(cfg *config.Mattermost, o *org.Organization) []string {
	return unionReads(fieldReads(cfg.URL, cfg.Team), mattermost.SeatTokenReads(o))
}

// runMattermostDoctor is `crewlet mattermost doctor`.
func runMattermostDoctor(inv invocation, args []string, stdout, stderr io.Writer) error {
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
	companySecrets, err := openCompanyStore(inv.environ, storeAt,
		"crewlet mattermost doctor")
	if err != nil {
		return err
	}
	defer companySecrets.Close()
	env, err := companySecrets.resolver(ctx, mattermostDoctorReads(cfg, organization), stdout)
	if err != nil {
		return err
	}

	token := strings.TrimSpace(*adminToken)
	if token == "" {
		token = operatorCredential(inv.environ, "MATTERMOST_ADMIN_TOKEN")
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
