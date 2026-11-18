package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/api/secretsapi"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/secrets"
)

// The company's secret store, as one CLI run reads and writes it.
//
// Reads of the company's values, a provisioning run recording what it mints and
// reading a seat's token back, and `crewlet llm login` and `export` storing
// what they capture all use the store the engine resolves every `${VAR}`
// through, so all go the one way [routeSecretStore] decides, once per run
// ([companyStore.route]). Two decisions (reads, then the sink) had different
// edges: `-api` from a machine with no Tier A read the node and could not
// record, and an engine starting between them split one run across two ways in.
// A provisioning run whose sink is an env file asks the same route which names
// the store holds, since the store outranks that sink
// ([sinkFlags.refuseOutranked]).

// storeFlags is the `-config` / `-api` pair every command that reads the
// company's stored values takes.
type storeFlags struct {
	bootstrap *string
	api       *string
}

// addStoreFlags adds `-config` and `-api`.
//
// EVERY command that reads Tier B takes them, not only the ones that WRITE a
// credential: the store is where a rotated secret lives, so a command
// resolving a company's `${VAR}` without it reads an empty string for
// everything the operator has already rotated. And `-api` is the same flag
// `crewlet secrets` takes, for the same cases: a node that holds no data has
// no store of the company's to reach, a machine that is not a node has none at
// all, and a member of a cluster whose engine is stopped cannot reach the
// fleet's alone.
func addStoreFlags(fs *flag.FlagSet) storeFlags {
	return storeFlags{
		bootstrap: fs.String("config", defaultBootstrapPath,
			"Tier A config: this node's store and its secret keyring"),
		api: nodeAPIFlag(fs,
			"a running node to read the company's stored values through and "+
				"record into; default is this node's engine at the api.host:port "+
				"in -config when it runs, and the fleet's store itself when it is "+
				"stopped"),
	}
}

// companyStore is one run's route to the company's secret store, decided at
// most once and shared by every read and write the run makes.
//
// Through a node, the run lists names (`GET /secrets`, revealing nothing), then
// asks `POST /secrets/reveal` for the listed variables the command reads, plus
// a `-secret-store` sink's read-back names, at most [secretsapi.MaxRevealNames]
// a request. Only listed names are sent: the route refuses a whole request over
// one name outside the reference grammar, an unheld name costs no request, and
// the node logs every name asked. A listed name answered missing falls to this
// shell's environment. A read the node refuses, or a node that does not
// answer, FAILS THE RUN before it acts: filling in from the environment would
// be the stale export the store exists to outrank.
type companyStore struct {
	environ       config.Source
	bootstrapPath string
	apiURL        string
	command       string

	// boot is the Tier A document at bootstrapPath, nil where there is no
	// file there. Where a vendor reaches this deployment is Tier A's
	// ([webhookBase]), so a command building a webhook URL reads it from
	// here rather than loading the file a second time.
	boot *config.Bootstrap

	routed      bool
	target      *secretTarget
	closeTarget func()
	routeErr    error

	// values is what the run has read through its route, nil until it has
	// read; saidEnvOnly is whether the environment-only line was printed.
	values      *storedValues
	saidEnvOnly bool
}

// openCompanyStore loads the Tier A document the flags name — opening nothing
// else — for the command named, which a refusal names.
//
// A BOOTSTRAP THAT EXISTS AND CANNOT BE READ FAILS THE RUN, rather than
// sending it to the environment: someone who configured a store and did not
// get it must not have their secrets quietly resolved from a stale export.
// And Tier A refuses a file with no keyring, so one that loads always names
// the keys its store is sealed under.
func openCompanyStore(environ config.Source, flags storeFlags, command string) (*companyStore, error) {
	s := &companyStore{
		environ: environ, bootstrapPath: *flags.bootstrap,
		apiURL: strings.TrimSpace(*flags.api), command: command,
	}
	boot, err := companyBootstrap(s.bootstrapPath)
	if err != nil {
		return nil, err
	}
	s.boot = boot
	return s, nil
}

// companyBootstrap is the Tier A a command reading the company runs under,
// or nil when there is no file at this path — a supported way to run one,
// away from any node. A file that exists and does not load is the error.
func companyBootstrap(bootstrapPath string) (*config.Bootstrap, error) {
	if _, err := os.Stat(bootstrapPath); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return loadBootstrapForStore(bootstrapPath)
}

// route opens the run's store, once: nil where there is none (no Tier A file
// and no `-api`), the environment-only run, which says so on its first line. A
// store the run cannot reach is answered here, before it has read anything.
//
// With the engine stopped the run reaches the fleet's store itself
// ([openFleetStore]), read the same way as through a node: the listing, then
// the listed names in one
// [github.com/crewlet/crewlet/internal/fleetsecrets.Store.Values]. A cluster
// member is sent to a running member ([fleetStoreRemedy]); a node holding no
// data is refused naming `-api` ([refuseScratchStore]). A run reading no stored
// value decides no route and starts no broker.
func (s *companyStore) route(ctx context.Context) (*secretTarget, error) {
	if s.routed {
		return s.target, s.routeErr
	}
	s.routed = true
	if s.boot == nil && s.apiURL == "" {
		return nil, nil
	}
	// A RUN HERE REVEALS: the values a command resolves, and a seat
	// credential its sink reads back, are the node's break-glass read.
	target, closeTarget, err := routeSecretStore(ctx, s.environ, s.boot,
		s.bootstrapPath, s.apiURL,
		storeRun{command: s.command, reveals: true})
	if err != nil {
		s.routeErr = err
		return nil, err
	}
	s.target, s.closeTarget = target, closeTarget
	return target, nil
}

// Close releases whatever the route opened.
func (s *companyStore) Close() {
	if s.closeTarget != nil {
		s.closeTarget()
	}
}

// resolver is the chain the run resolves the company's `${VAR}` references
// through: the store's values for reads, read before the command does
// anything, the environment behind them — the engine's own order. See
// [companyStore.stored] for what is read and said.
func (s *companyStore) resolver(ctx context.Context, reads []string, notes io.Writer) (*config.Resolver, error) {
	values, err := s.stored(ctx, reads, notes)
	if err != nil {
		return nil, err
	}
	if values == nil {
		return config.EnvOnly(), nil
	}
	return config.WithStore(values), nil
}

// stored is the run's stored values: those of reads its store holds, read up
// front through the run's route, and every one read since — nil where the run
// has no store.
//
// The first call says on notes, on the run's first line, which way that is:
// the environment alone, the running node and how much it revealed, or the
// fleet's store reached directly and how much of it the command reads. A later
// call reads the names it adds, in one more batch, and says nothing.
func (s *companyStore) stored(ctx context.Context, reads []string, notes io.Writer) (*storedValues, error) {
	target, err := s.route(ctx)
	if err != nil {
		return nil, err
	}
	if target == nil {
		if !s.saidEnvOnly {
			// A NOTE RATHER THAN SILENCE: a mistyped -config resolving
			// nothing reads every stored value as empty, and an operator
			// has to be able to see which chain ran.
			fmt.Fprintf(notes, "no %s: resolving ${VAR} from the environment only.\n",
				s.bootstrapPath)
			s.saidEnvOnly = true
		}
		return nil, nil
	}
	if s.values != nil {
		s.values.mu.Lock()
		_, err = s.values.readLocked(reads)
		s.values.mu.Unlock()
		if err != nil {
			return nil, s.readFailure(target, err)
		}
		return s.values, nil
	}
	values, err := newStoredValues(ctx, target, s.where(target), notes)
	if err != nil {
		return nil, s.readFailure(target, err)
	}
	asked, err := values.readLocked(reads)
	if err != nil {
		return nil, s.readFailure(target, err)
	}
	s.values = values
	s.sayRoute(notes, target, values, asked)
	return values, nil
}

// where names the way to the store a run reads, for a failure part way
// through one.
func (s *companyStore) where(target *secretTarget) string {
	if target.throughNode {
		return "through the running node at " + target.node
	}
	return "from " + target.where
}

// readFailure is a read of the run's store that failed, in the terms of the
// route it took.
func (s *companyStore) readFailure(target *secretTarget, err error) error {
	remedy := revealRemedy(err)
	switch {
	case target.locked != nil:
		// BOTH FACTS, as a write's refusal gives them: the lock alone reads
		// as "blocked, no way forward", and the API's complaint alone as
		// though nothing were holding the store. Stopping the engine would
		// let this command reach the store itself, but it is not offered as
		// the way round: a node taken down to read a value is an outage to
		// save a token ([storeRun.offline]).
		return fmt.Errorf("%w\n\n%s's database is held by the process named "+
			"above. While that is its engine, the company's stored values are "+
			"read through its API, and they cannot be: %w%s%s", target.locked,
			s.bootstrapPath, err, remedy, heldByACommand)
	case target.throughNode:
		return fmt.Errorf("read the company's stored values through %s: %w%s",
			target.node, err, remedy)
	}
	return fmt.Errorf("read the company's stored values from %s: %w", target.where, err)
}

// sayRoute is the run's first line on a store it read: which way in, and
// through a node how much it revealed there, or reached directly how much of
// what the command reads the store holds.
func (s *companyStore) sayRoute(notes io.Writer, target *secretTarget, values *storedValues, asked int) {
	switch {
	case target.throughNode && asked == 0:
		fmt.Fprintf(notes, "resolving ${VAR} through the running node at %s: it holds "+
			"none of the values this command reads, so none was revealed.\n", target.node)
	case target.throughNode:
		// SAID EVERY TIME, as the environment-only chain is: which chain
		// ran is what an operator reads first when a value is not the one
		// they set, and each name asked is a line in the node's log under
		// their token.
		fmt.Fprintf(notes, "resolving ${VAR} through the running node at %s: %d of the "+
			"%d stored values this command reads revealed there in %s, each logged "+
			"by name.\n", target.node, values.revealed(), asked, requests(asked))
	default:
		// SAID TOO, and naming the broker: a run that reached the store of
		// a node the operator thought was up, or through the wrong Tier A —
		// every path in it is relative to the working directory — is
		// visible on the first line.
		fmt.Fprintf(notes, "resolving ${VAR} from %s: %d of the stored values this "+
			"command reads held there.\n", target.where, values.revealed())
	}
}

// requests is how many reveal requests asking for n names takes, said.
func requests(n int) string {
	count := (n + secretsapi.MaxRevealNames - 1) / secretsapi.MaxRevealNames
	if count == 1 {
		return "1 request"
	}
	return fmt.Sprintf("%d requests", count)
}

// recordInto is the store a `-secret-store` sink records into and reads back
// from: the run's route, and its stored values, so a seat's token the sink
// reads back is answered from what the run read up front.
func (s *companyStore) recordInto(ctx context.Context) (sinkStore, error) {
	target, err := s.route(ctx)
	if err != nil {
		return sinkStore{}, err
	}
	if target == nil {
		return sinkStore{}, fmt.Errorf("-secret-store has no store to record into: "+
			"there is no Tier A file at %s naming this node's store and keyring. "+
			"Point -config at it, or pass -api URL naming a running node", s.bootstrapPath)
	}
	values, err := s.stored(ctx, nil, io.Discard)
	if err != nil {
		return sinkStore{}, err
	}
	return sinkStore{target: target, values: values}, nil
}

// holding is which of names the run's store holds, by its listing alone — no
// value is read — each once and sorted, and that store as a refusal names it;
// nil where the run has no store (no Tier A file and no -api: the
// environment-only run), which then holds nothing. A store the run cannot
// reach is the route's refusal, and the answer: a miss nobody could look for
// says nothing of what the fleet holds.
func (s *companyStore) holding(ctx context.Context, names []string, notes io.Writer) (
	held []string, store string, err error) {

	target, err := s.route(ctx)
	if err != nil || target == nil {
		return nil, "", err
	}
	values, err := s.stored(ctx, nil, notes)
	if err != nil {
		return nil, "", err
	}
	store = target.where
	if target.throughNode {
		store = "the fleet's, read through the running node at " + target.node
	}
	return values.holding(names), store, nil
}

// storedValues is the stored values one run reads, through the store its route
// reached: a [config.Source] over that store's rows that reads only what is
// asked of it, and the cache a `-secret-store` sink reads back from.
//
// The command's scope is read BEFORE it runs. A [config.Source] lookup has no
// error to return, so a failed read (a reveal the node refuses a machine
// token, a row sealed under a retired key) could only come back "unset", which
// tells a provisioning run to MINT: a GitLab provision would replace the key a
// working hook signs with. Read up front, the failure stops the command before
// it acts. A read outside the scope is answered from the store one name per
// request, said on notes, and a failed one PANICS rather than act on a value
// it does not have.
type storedValues struct {
	// mu guards values and held: a provisioner may read from more than one
	// goroutine, and a read is a request the lock holds across.
	mu sync.Mutex

	// values is every stored value read so far, and every value the run's
	// sink has recorded since, by name.
	values map[string]string

	// held is every name the store listed, less those found gone since.
	held map[string]bool

	// read reads the values of names the store listed, in as few requests
	// as its route allows.
	read func(names []string) (map[string]string, error)

	// where names the way to the store for a failure part way through the
	// run, and node is the address of the node a read is a reveal through,
	// which is said — empty on the store reached directly.
	where string
	node  string
	notes io.Writer
}

// newStoredValues lists target's names, which reads no value.
func newStoredValues(ctx context.Context, target *secretTarget, where string,
	notes io.Writer) (*storedValues, error) {

	rows, err := target.List(ctx)
	if err != nil {
		return nil, err
	}
	v := &storedValues{
		values: map[string]string{},
		held:   make(map[string]bool, len(rows)),
		read: func(names []string) (map[string]string, error) {
			return target.Values(ctx, names)
		},
		where: where, node: target.node, notes: notes,
	}
	for _, row := range rows {
		v.held[row.Name] = true
	}
	return v, nil
}

// Lookup implements [config.Source].
func (v *storedValues) Lookup(name string) (string, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if value, ok := v.values[name]; ok {
		return value, true
	}
	if !v.held[name] {
		return "", false
	}
	if _, err := v.readLocked([]string{name}); err != nil {
		panic(readFailed{fmt.Errorf("read ${%s} %s: %w, so this stops rather than "+
			"resolving a stored value as unset%s", name, v.where, err, revealRemedy(err))})
	}
	value, ok := v.values[name]
	switch {
	case !ok:
	case v.node != "":
		// THE SAME WORDS THE COMMAND'S FIRST LINE USED for its way in: a
		// read through a node is a reveal, logged there by name against
		// the caller, and the operator is told so for this one as for the
		// rest.
		fmt.Fprintf(v.notes, "revealed ${%s} through the running node at %s as it "+
			"was read, beyond the values this command reveals before it runs; "+
			"logged there by name.\n", name, v.node)
	default:
		fmt.Fprintf(v.notes, "read ${%s} %s as it was read, beyond the values "+
			"this command reads before it runs.\n", name, v.where)
	}
	return value, ok
}

// readLocked reads the values of the names asked that the store holds and the
// run has not read yet, in one call, reporting how many it asked for; a held
// name the answer lacks is gone since the listing, and is held no longer.
// v.mu is held, or v is not yet shared.
//
// NEVER A NAME NO STORE CAN HOLD, whatever a listing said: a node refuses the
// whole of a reveal one such name is in, and a read beyond the scope can be
// made of any variable an entry signs in from — an `overrides.token_env` of
// MY-TOKEN among them — so the name is answered unset here, as the engine,
// which can hold no such row either, answers it.
func (v *storedValues) readLocked(names []string) (int, error) {
	var ask []string
	for _, name := range names {
		if _, read := v.values[name]; read || !v.held[name] ||
			secrets.CheckName(name) != nil || secrets.Reserved(name) {
			continue
		}
		ask = append(ask, name)
	}
	slices.Sort(ask)
	ask = slices.Compact(ask)
	if len(ask) == 0 {
		return 0, nil
	}
	got, err := v.read(ask)
	if err != nil {
		return 0, err
	}
	for _, name := range ask {
		if value, held := got[name]; held {
			v.values[name] = value
		} else {
			delete(v.held, name)
		}
	}
	return len(ask), nil
}

// holding is which of names the store holds — listed, or recorded by the
// run's sink since — each once, sorted.
func (v *storedValues) holding(names []string) []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	var held []string
	for _, name := range names {
		if v.held[name] {
			held = append(held, name)
		}
	}
	slices.Sort(held)
	return slices.Compact(held)
}

// revealed is how many values the run has read.
func (v *storedValues) revealed() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.values)
}

// get is the value the store holds under name, for a sink reading one back:
// what the run read or recorded, a one-name read where the store listed it
// and nothing has read it yet, and [secrets.ErrNotFound] with no request where
// it did not.
func (v *storedValues) get(name string) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if value, ok := v.values[name]; ok {
		return value, nil
	}
	if v.held[name] {
		if _, err := v.readLocked([]string{name}); err != nil {
			return "", err
		}
		if value, ok := v.values[name]; ok {
			return value, nil
		}
	}
	return "", fmt.Errorf("%w: %s", secrets.ErrNotFound, name)
}

// recorded notes a value the run's sink wrote, which a read of it answers from
// then on: a value recorded a moment ago is held.
func (v *storedValues) recorded(name, value string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.values[name], v.held[name] = value, true
}

// forgot notes a value the run's sink removed.
func (v *storedValues) forgot(name string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.values, name)
	delete(v.held, name)
}

// readFailed is a read of the run's store that failed part way through a
// command, carried by a panic out of the [storedValues.Lookup] that has no
// error to return, and answered at the command's top as the error it is
// ([runIn]). A panic, because the alternative answer — unset — is the one that
// mints.
type readFailed struct{ err error }

func (r readFailed) Error() string { return r.err.Error() }
func (r readFailed) Unwrap() error { return r.err }

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
