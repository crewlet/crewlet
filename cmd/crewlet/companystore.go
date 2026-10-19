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
// # One route for both sides of a run
//
// A command that reads the company READS its values — an instance's address, a
// webhook signing secret, an entry's token — and a provisioning run RECORDS
// what it mints and reads a seat's token back to decide whether to keep it,
// while `crewlet llm login` and `export` store what they capture. All of it is
// the company's secret store, the one the engine resolves every `${VAR}`
// through, so all of it goes the one way [routeSecretStore] decides, once per
// run ([companyStore.route]).
//
// It used to be decided twice: once for the reads, by a resolver that read the
// store itself or asked the running node, and once for the sink and the llm
// writes, by the route `crewlet secrets` takes. The two had different edges:
// `-api` from a machine with no Tier A read the node and then could not
// record, and an engine starting or stopping between the two decisions split
// one run across two ways in, each with its own idea of what the company
// held.
//
// # Through a node: the names it lists, in batches
//
// The run lists the node's names first (`GET /secrets`, which reveals
// nothing), then asks `POST /secrets/reveal` for the variables the command
// reads that the node lists — and, where a `-secret-store` sink will read a
// seat's token back, those names in the same batch — at most
// [secretsapi.MaxRevealNames] a request. Only listed names are sent, for three
// reasons. The route refuses a whole request over one name outside the
// reference grammar, and a file loaded only to run can name one
// (`overrides.token_env: MY-TOKEN`) that no store can hold. A name the node
// does not hold costs no request. And the node logs every name it is asked
// for, so its log names only what it holds. A listed name answered missing —
// removed since the listing — falls to the environment as an unheld one does,
// and that environment is this shell's, not the engine's.
//
// A read the node refuses, or a node that does not answer, FAILS THE RUN
// before it acts, and nothing is filled in from the environment in its place:
// a value missing because the node could not be asked is not one the fleet
// does not hold, and the environment's answer would be the stale export the
// store exists to outrank.
//
// # With the engine stopped: the fleet's store itself
//
// While this node's engine is stopped, the run reaches the fleet's store
// itself ([openFleetStore]) — the same store a running node serves through its
// API, so a value is held or not held the same way whichever way the run went
// in. It is read the same way too: the listing, then the values of the names
// the command reads that the store lists, in one read of the store
// ([github.com/crewlet/crewlet/internal/fleetsecrets.Store.Values]), which
// opens no row it was not asked for. The first line says so, naming the broker
// it was reached through. A member of a cluster cannot reach the store alone,
// and is sent to a running member ([fleetStoreRemedy]); nor can a node that
// holds no data, which is refused naming `-api` ([refuseScratchStore]).
//
// A run that reads no stored value decides no route at all and runs
// anywhere: a dry run that resolves nothing, a plan with nothing in it — and
// starts no broker for a store it never reads.
//
// # A run that only asks which names the store holds
//
// A provisioning run whose sink is an env file or its own output asks its
// store which of the names the run can mint into it holds, since the store
// outranks that sink ([sinkFlags.refuseOutranked]). That is a read of the
// company's store like any other, by the run's one route, but where it is all
// the run reads (a Slack run whose sink is no store), the listing is the whole
// of it, and the route takes the token a listing takes rather than a reveal's
// ([companyStore.onlyLists]).
//
// # No Tier A, and no -api
//
// With no Tier A file at `-config` and no `-api`, there is no store to read:
// the run resolves from the environment alone and says so on its first line,
// which is how these commands run on a machine that is not a node.

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

	// listsOnly is whether the run reveals nothing through its route and
	// only asks it which names it holds ([companyStore.onlyLists]).
	listsOnly bool

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

// route opens the run's store, once: nil where there is none — no Tier A file
// and no `-api` — which is the environment-only run. A store the run cannot
// reach — a cluster member with its engine stopped, a running node whose API
// it cannot use — is answered here, before it has read anything.
func (s *companyStore) route(ctx context.Context) (*secretTarget, error) {
	if s.routed {
		return s.target, s.routeErr
	}
	s.routed = true
	if s.boot == nil && s.apiURL == "" {
		return nil, nil
	}
	// A RUN HERE REVEALS — the values a command resolves, and a seat
	// credential its sink reads back, are the node's break-glass read —
	// unless all it asks is which names the store holds.
	target, closeTarget, err := routeSecretStore(ctx, s.environ, s.boot,
		s.bootstrapPath, s.apiURL,
		storeRun{command: s.command, reveals: !s.listsOnly})
	if err != nil {
		s.routeErr = err
		return nil, err
	}
	s.target, s.closeTarget = target, closeTarget
	return target, nil
}

// onlyLists makes the run one that reveals nothing through its route: all it
// asks of the store is which names it holds ([companyStore.holding]). Through
// a node that is the listing, `secrets.list`, which takes admin and no person
// present — so a machine token serves it, and the route asks for no Tier A
// token and a refused listing names none ([storeRun.reveals]).
//
// CALLED BEFORE ANYTHING DECIDES THE ROUTE, which is decided once: the token a
// run presents is chosen with it.
func (s *companyStore) onlyLists() { s.listsOnly = true }

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
	if s.listsOnly {
		// A LISTING IS NO REVEAL, so the token a reveal takes is no remedy
		// for a refused one.
		remedy = ""
	}
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
			"read through its API — and they cannot be: %w%s%s", target.locked,
			s.bootstrapPath, err, remedy, heldByACommand)
	case target.throughNode:
		return fmt.Errorf("read the company's stored values through %s: %w%s",
			target.node, err, remedy)
	}
	return fmt.Errorf("read the company's stored values from %s: %w", target.where, err)
}

// sayRoute is the run's first line on a store it read: which way in, and
// through a node how much it revealed there, or reached directly how much of
// what the command reads the store holds — or, for a run that only asks which
// names the store holds, that it reads none of the values.
func (s *companyStore) sayRoute(notes io.Writer, target *secretTarget, values *storedValues, asked int) {
	switch {
	case target.throughNode && s.listsOnly:
		fmt.Fprintf(notes, "checking which names the running node at %s holds: this "+
			"command reads no stored value, so none was revealed.\n", target.node)
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
	case s.listsOnly:
		fmt.Fprintf(notes, "checking which names %s holds: this command reads no "+
			"stored value.\n", target.where)
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
// # The command's scope is read before it runs
//
// Not one value at a time as the command reads it, which would read no more
// and fail worse. A read is a [config.Source] lookup, which has no error to
// return: a read that failed — a reveal the node refused, which it refuses a
// machine token every one; a row sealed under a key the ring no longer holds —
// could only come back as "unset", and unset is what tells a provisioning run
// to MINT, so a GitLab provision would replace the key a working hook signs
// with because it could not read the one in the store. Read up front, the same
// failure stops the command before it has done anything. And only those names
// are read, either way in: reached directly, a row sealed under a retired key
// that the command never reads cannot fail it.
//
// # A read outside the scope is still answered from the store
//
// Read as it is made, one name in one request, and said on notes. A command
// whose scope missed a field it reads — a new one, say — then resolves the
// store's value rather than the environment's, which is the engine's answer;
// resolving it as unset would be the same mint. That read has no error to
// return either, so a failed one PANICS: the run stops where it is, as it would
// on a vendor's own failure part way through, rather than going on to act on a
// value it does not have.
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
		panic(readFailed{fmt.Errorf("read ${%s} %s: %w — stopping rather than "+
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
