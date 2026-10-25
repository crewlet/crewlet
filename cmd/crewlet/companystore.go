package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/api/secretsapi"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/seat/placement"
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
// It used to be decided twice: once for the reads, by a resolver that opened
// this node's table itself or asked the running node, and once for the sink
// and the llm writes, by the route `crewlet secrets` takes. The two had
// different edges. `-api` from a machine with no Tier A read the node and then
// could not record; an engine starting or stopping between the two decisions
// split one run across two stores; and a read on a stopped node created an
// empty database and a lock file where none had been.
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
// # A stopped node: the whole store, or refused
//
// The fleet's store is the coordination KV on the stream's broker — on a
// sole member, that member's own JetStream under `stream.store_dir`, whose
// directory nats-server creates at the broker's first start
// ([jetstream.StoreHasRun]). So before a sole member's first start there is
// no fleet store anywhere, and this node's own rows are the company's whole
// secret store: a row is held, and no row is definitively not held. A run
// there reads them, and says so on its first line, naming both paths, so a run
// from the wrong working directory — every path in Tier A is relative to it —
// is visible.
//
// On every other stopped node — one whose broker has run, a member of a
// cluster (`stream.cluster`), a client of an external one (`stream.type:
// nats`) — the company's values are on a store only a running engine serves,
// and a miss in this node's rows says nothing about them. A run there that
// reads the company is REFUSED before it reads, plans or changes anything
// ([stoppedNodeError]), naming the running engine and `-api`; no flag for a
// fresh value is a way past it. Noting the gap and carrying on, as this used
// to, is not safe for any run that acts: unset is the signal a provisioner
// mints on, so a miss read as unset minted over the fleet's value — a GitHub
// hook re-keyed, a GitLab seat's live token retired — and the engine's next
// start took the later of the two writes, the run's, over the fleet's
// ([github.com/crewlet/crewlet/internal/fleetsecrets.Migrate]). A value
// minted into an env file or printed never reaches the table at all, so
// nothing reconciles it, and the store outranks it in the engine. And the
// reports that look rather than act — `crewlet llm list` restores the
// configured bundle into an empty credentials directory, which the engine then
// refuses to overwrite — act too.
//
// A run that acts on its own ([actsOnItsOwn]) still runs there: what it
// writes is new — `crewlet secrets set`, a token `crewlet llm login` captures
// — which the migration carries onto the fleet later-wins, the semantics the
// store has always given an offline write, or it works on this host's login
// alone. It reads the entry it works on from the rows and the environment, and
// says nothing of them: they are not the whole store, and it reads them only
// to find the entry.
//
// A run that reads no stored value decides no route at all and runs
// anywhere: a dry run that resolves nothing, a plan with nothing in it.
//
// # A run that only asks which names the store holds
//
// A provisioning run whose sink is an env file or its own output asks its
// store which of the names the run can mint into it holds, since the store
// outranks that sink ([sinkFlags.refuseOutranked]). That is a read of the
// company's store like any other — a stopped node that has run cannot answer
// it, so the run is refused there — but where it is all the run reads (a
// Slack run whose sink is no store), the listing is the whole of it, and the
// route takes the token a listing takes rather than a reveal's
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
// no store of the company's to read, a machine that is not a node has none at
// all, and a stopped node whose own rows are not the whole store — one that
// has run, a cluster member, an external broker's client — reads nothing of
// the fleet's.
func addStoreFlags(fs *flag.FlagSet) storeFlags {
	return storeFlags{
		bootstrap: fs.String("config", defaultBootstrapPath,
			"Tier A config: this node's store and its secret keyring"),
		api: fs.String("api", "",
			"the node to read the company's stored values through and record "+
				"into; default is this node's store, or the running engine at the "+
				"api.host:port in -config"),
	}
}

// companyStore is one run's route to the company's secret store, decided at
// most once and shared by every read and write the run makes.
type companyStore struct {
	environ       config.Source
	bootstrapPath string
	apiURL        string
	command       string
	use           storeUse

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
// else — for the command named, which a refusal names, and what that command
// does with the stored values.
//
// A BOOTSTRAP THAT EXISTS AND CANNOT BE READ FAILS THE RUN, rather than
// sending it to the environment: someone who configured a store and did not
// get it must not have their secrets quietly resolved from a stale export.
// And Tier A refuses a file with no keyring, so one that loads always names
// the keys its store is sealed under.
func openCompanyStore(environ config.Source, flags storeFlags, command string,
	use storeUse) (*companyStore, error) {

	s := &companyStore{
		environ: environ, bootstrapPath: *flags.bootstrap,
		apiURL: strings.TrimSpace(*flags.api), command: command, use: use,
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
// and no `-api` — which is the environment-only run. On a stopped node whose
// own rows are not the company's whole store, a run that reads the company is
// answered with the [stoppedNodeError] here, before it has read anything.
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
		storeRun{command: s.command, use: s.use, reveals: !s.listsOnly})
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
// The first call says on notes, on the run's first line, which store that is:
// the environment alone, the running node and how much it revealed, or this
// node's own rows where they are the whole store. A later call reads the names
// it adds, in one more batch, and says nothing.
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

// where names the store a run reads, for a failure part way through one.
func (s *companyStore) where(target *secretTarget) string {
	if target.fleet {
		return "through the running node at " + target.node
	}
	return "from " + s.boot.Store.Path
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
		// though this node's own table were an answer. It is not one while
		// the engine runs, so stopping the engine is not offered.
		return fmt.Errorf("%w\n\nthe engine for %s is running and holds its "+
			"database, so the company's stored values are read through its API — "+
			"and they cannot be: %w%s", target.locked, s.bootstrapPath, err, remedy)
	case target.fleet:
		return fmt.Errorf("read the company's stored values through %s: %w%s",
			target.node, err, remedy)
	}
	return fmt.Errorf("read %s: %w", s.boot.Store.Path, err)
}

// sayRoute is the run's first line on a store it read: which one, and through
// a node how much it revealed there — or, for a run that only asks which names
// the store holds, that it reads none of the values. A run that acts on its
// own on rows that are not the whole store says nothing of them
// ([actsOnItsOwn]).
func (s *companyStore) sayRoute(notes io.Writer, target *secretTarget, values *storedValues, asked int) {
	switch {
	case target.fleet && s.listsOnly:
		fmt.Fprintf(notes, "checking which names the running node at %s holds: this "+
			"command reads no stored value, so none was revealed.\n", target.node)
	case target.fleet && asked == 0:
		fmt.Fprintf(notes, "resolving ${VAR} through the running node at %s: it holds "+
			"none of the values this command reads, so none was revealed.\n", target.node)
	case target.fleet:
		// SAID EVERY TIME, as the environment-only chain is: which chain
		// ran is what an operator reads first when a value is not the one
		// they set, and each name asked is a line in the node's log under
		// their token.
		fmt.Fprintf(notes, "resolving ${VAR} through the running node at %s: %d of the "+
			"%d stored values this command reads revealed there in %s, each logged "+
			"by name.\n", target.node, values.revealed(), asked, requests(asked))
	case target.wholeStore():
		store, storeDir := absolute(s.boot.Store.Path), absolute(s.boot.Stream.StoreDir)
		absent := ""
		if !values.fileExists {
			absent = ", which do not exist yet"
		}
		reads := "resolving ${VAR} from this node's own rows (%s%s) and the environment"
		if s.listsOnly {
			reads = "checking which names this node's own rows (%s%s) hold"
		}
		fmt.Fprintf(notes, reads+": its broker has never started (%s holds no state) "+
			"and %s names no fleet, so these rows are the company's whole secret store.\n",
			store, absent, storeDir, s.bootstrapPath)
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

// absolute is path as the line an operator reads names it: from the root,
// since the working directory it is relative to is the one thing a run from
// the wrong place gets wrong.
func absolute(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
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
// environment-only run), which then holds nothing. On a stopped node whose own
// rows are not the company's whole store the route's refusal is the answer
// ([stoppedNodeError]): a miss in those rows says nothing of the fleet's.
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
	if target.fleet {
		store = "the fleet's, read through the running node at " + target.node
	} else {
		store = "this node's own rows at " + absolute(s.boot.Store.Path) +
			", which are the whole of it until the node's broker first starts"
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
// return: a reveal the node refused — a machine token, which is refused every
// one — could only come back as "unset", and unset is what tells a
// provisioning run to MINT, so a GitLab provision would replace the key a
// working hook signs with because its token could not read the one in the
// store. Read up front, the same refusal stops the command before it has done
// anything.
//
// # A read outside the scope is still answered from the store
//
// Read as it is made, one name in one request, and said on notes through a
// node. A command whose scope missed a field it reads — a new one, say — then
// resolves the store's value rather than the environment's, which is the
// engine's answer; resolving it as unset would be the same mint. That read has
// no error to return either, so a failed one PANICS: the run stops where it
// is, as it would on a vendor's own failure part way through, rather than
// going on to act on a value it does not have.
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

	// where names the store for a failure part way through the run, and
	// node is the address of the node a read is a reveal through, which is
	// said — empty on this node's own table.
	where string
	node  string
	notes io.Writer

	// fileExists is whether this node's own table had a file when the run
	// listed it; always true through a node.
	fileExists bool
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
		where: where, node: target.node, notes: notes, fileExists: true,
	}
	if local, ok := target.secretBackend.(*localSecrets); ok {
		v.fileExists = local.read() != nil
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
	if ok && v.node != "" {
		fmt.Fprintf(v.notes, "revealed ${%s} through the running node at %s as it "+
			"was read, beyond the values this command reveals before it runs.\n",
			name, v.node)
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

// errStoppedNode is the refusal of a run that reads the company's stored values
// on a stopped node whose own rows are not the company's whole secret store
// ([stoppedNodeError]).
var errStoppedNode = errors.New("this node's engine is stopped and its own rows " +
	"are not the company's whole secret store")

// stoppedReason is why a stopped node's own rows are not the company's whole
// secret store.
type stoppedReason int

const (
	// brokerStateUnknown: whether the node's broker has ever run cannot be
	// told. THE ZERO VALUE, so a reason nobody set reads as the answer that
	// refuses.
	brokerStateUnknown stoppedReason = iota

	// brokerHasRun: the sole member's broker has started on its store
	// directory, which holds its state and the fleet's store with it.
	brokerHasRun

	// fleetMember: Tier A makes the node one member of a cluster.
	fleetMember

	// externalBroker: Tier A dials a broker somebody else runs.
	externalBroker
)

// stoppedNodeError is the refusal of a run that reads the company's stored
// values on a stopped node whose own rows are not the company's whole store:
// what the node is, the two ways to the company's values, and that nothing
// was read or changed.
type stoppedNodeError struct {
	command  string
	config   string
	storeDir string
	reason   stoppedReason

	// stat is why the broker's state could not be looked at, for
	// [brokerStateUnknown].
	stat error

	// noAPI is why this node's Tier A gives it no API to read the values
	// through, as read, empty where it gives it one ([servesNoAPI]).
	noAPI string
}

// Is makes the refusal [errStoppedNode].
func (e *stoppedNodeError) Is(target error) bool { return target == errStoppedNode }

// Unwrap is the stat that could not tell whether the broker has run, if any.
func (e *stoppedNodeError) Unwrap() error { return e.stat }

func (e *stoppedNodeError) Error() string {
	var why, start string
	switch e.reason {
	case brokerHasRun:
		why = fmt.Sprintf("this node's engine is stopped and has run before (%s holds its "+
			"broker's state), so the company's stored values are on the fleet's store, "+
			"which only a running engine serves.", e.storeDir)
		start = "Start it"
	case fleetMember:
		why = fmt.Sprintf("%s makes this node a member of a fleet (stream.cluster), so the "+
			"company's stored values are the fleet's, which this node cannot read while "+
			"its engine is stopped.", e.config)
	case externalBroker:
		why = fmt.Sprintf("%s dials an external broker (stream.type: nats), so the "+
			"company's stored values are on that cluster's store, which this node reads "+
			"only through a running engine.", e.config)
	default:
		why = fmt.Sprintf("whether this node's broker has ever run cannot be told (%v), so "+
			"its own rows cannot be taken for the company's whole secret store.", e.stat)
	}
	if start == "" {
		start = "start this node's engine"
	}
	run := fmt.Sprintf("(crewlet run -config %s) and re-run with %s set to one of %s's "+
		"api.auth tokens", e.config, apiTokenEnv, e.config)
	var remedy string
	switch {
	case e.noAPI != "":
		remedy = fmt.Sprintf("Tier A gives this node no API to read them through — %s — "+
			"so pass -api URL naming a running node that does.", e.noAPI)
	case e.reason == fleetMember:
		remedy = fmt.Sprintf("Pass -api URL naming a running member, or %s %s.", start, run)
	default:
		remedy = fmt.Sprintf("%s %s, or pass -api URL naming a running node.",
			strings.ToUpper(start[:1])+start[1:], run)
	}
	return e.command + ": " + why + " " + remedy + " Nothing was read or changed."
}

// stoppedNode is why this stopped node's own rows are not the company's whole
// secret store, for the command named — or nil where they are: a sole member
// ([config.Bootstrap.SoleMember]) whose broker has never started on its store
// directory ([jetstream.StoreHasRun]).
//
// FROM TIER A AND THE BROKER'S OWN DIRECTORY, both of which this node has: a
// node joining a fleet says so in Tier A (`stream.cluster`, or `stream.type:
// nats` for a cluster somebody else runs) before its first start, and a sole
// member's store is under its store directory from the moment its broker
// first ran. Not a marker the node writes, which a wiped or restored store
// directory would leave saying the opposite of what is there. A store
// directory that cannot be looked at answers "cannot be told", which refuses.
func stoppedNode(boot *config.Bootstrap, bootstrapPath, command string) *stoppedNodeError {
	e := &stoppedNodeError{command: command, config: bootstrapPath,
		noAPI: servesNoAPI(boot, bootstrapPath)}
	switch {
	case boot.Stream.Type == config.StreamNATS:
		e.reason = externalBroker
	case !boot.SoleMember():
		e.reason = fleetMember
	default:
		dir := boot.Stream.StoreDir
		if strings.TrimSpace(dir) != "" {
			dir = absolute(dir)
		}
		e.storeDir = dir
		ran, err := jetstream.StoreHasRun(dir)
		switch {
		case err != nil:
			e.reason, e.stat = brokerStateUnknown, err
		case ran:
			e.reason = brokerHasRun
		default:
			return nil
		}
	}
	return e
}

// servesNoAPI is why the node this Tier A describes serves no API a run could
// read the company's values through, or "" where it serves one: an api.port
// of 0, or node.roles without ingress, which is the only role that mounts
// REST. SAID AS READ, since `crewlet run -api-port` and `-roles` override
// both and this command cannot know whether the engine was started with one.
func servesNoAPI(boot *config.Bootstrap, bootstrapPath string) string {
	if boot.API.Port <= 0 {
		return bootstrapPath + "'s api.port is 0 (a `crewlet run -api-port` override is " +
			"not known here)"
	}
	if roles, err := boot.Node.RoleSet(); err == nil && !roles.Has(placement.RoleIngress) {
		return "its node.roles in " + bootstrapPath + " leave out ingress (a `crewlet " +
			"run -roles` override is not known here)"
	}
	return ""
}
