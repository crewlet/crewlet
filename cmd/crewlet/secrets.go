package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/crewlet/crewlet/internal/api/secretsapi"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/fleetsecrets"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/store"
)

// `crewlet secrets` — the operator's window onto the encrypted secret store.
//
// # Why a store and not just a .env
//
// Rotation. A secret written into the config is archived for ever, because
// every revision is an immutable copy and revisions are never scrubbed;
// rotating one here is an UPDATE of one row. And one name can have many
// pointers — a bot token referenced from an integration block and a per-role
// MCP env is ONE credential with two readers — which keying by var name
// keeps as one row rather than several that must update atomically.
//
// The engine resolves ${VAR} through the STORE FIRST and the environment
// behind it, so a value rotated here wins over a stale `.env` exported into
// the process months ago. That order is the whole point; without it a
// rotation appears to work and changes nothing.
//
// # There are two stores, and the running engine decides which one this is
//
// The company's rows live on the coordination KV, where every node reads
// them. On the default topology that KV is inside the
// engine's own process and listens on no socket, so this command cannot
// write it directly — it goes through the node's authenticated /secrets
// surface instead, which is the primary path and the one a fleet uses.
//
// When the engine is STOPPED there is no KV to reach and no API to call, so
// the command writes this node's own table. That is the bootstrap path: the
// engine migrates those rows onto the fleet at its next start and removes
// them, the later of its value and the fleet's winning a name both hold
// (fleetsecrets.Migrate). Which of the two is in play is not a guess — the store's file lock
// makes "the engine holds this database" an answer with a pid on it — and
// the command says which one it used after every write.
//
// ONLY `set` RUNS THERE WHEREVER THAT TABLE IS NOT THE WHOLE STORE. Before a
// sole member's first start its own table is the company's whole store, and
// every subcommand works on it. Anywhere else — once that member has run, and
// on a member of a cluster or a client of an external broker from the outset
// — the company's rows are the fleet's: `list` and `get` would answer for one
// node's leftovers, an `unset` of a name the fleet holds removes nothing (the
// migration copies rows and carries no removal), and a `rekey` re-seals rows
// that are not the ones a rotation is about. So those are refused there,
// naming the running engine and -api ([stoppedNodeError]), and `set`, whose
// value the next start carries onto the fleet later-wins, is the one write
// left.
//
// # The keyring is Tier A, and a node named by -api holds its own
//
// The store holds only ciphertext. The key material lives in the bootstrap
// config — on disk or in the environment, never in the database it opens —
// which is why these commands read Tier A and not the company document.
//
// Through a node `-api` names, this command seals and opens nothing: the node
// does both, under its own keyring. So a machine that is not a node, holding
// no Tier A file, runs every subcommand there but `rekey`, which judges the
// rows against the `secrets.active_key_id` that file names, and so needs it
// ([secretsBootstrap]).

const secretsUsage = `crewlet secrets — read and rotate the encrypted secret store

Usage:
  crewlet secrets list                     What is set, and when it was last written
  crewlet secrets set NAME [-value V]      Store or rotate one secret (reads stdin without -value)
  crewlet secrets get NAME -reveal         Print one secret to stdout (break-glass; logged)
  crewlet secrets unset NAME               Remove one secret
  crewlet secrets keygen [-key-id ID]      A fresh keyring key, with the config snippet to install it
  crewlet secrets rekey [-dry-run]         Re-seal every row, and every person's values, under the active key

Flags:
  -config PATH   Tier A config carrying the keyring (default %q)
  -api URL       The running node to go through; default is its own api.host:port

A running engine holds its database, so these commands go through its
authenticated /secrets surface — which is what puts a value on every node.
Export CREWLET_API_TOKEN to authenticate. It is the only source — the Tier A
api.auth.tokens list is what the node ACCEPTS, not a wallet this command helps
itself from.

With the engine stopped they work on this node's own table, which is the
company's whole store only on a single node before its first start. Anywhere
else — once the node has run, or on a member of a cluster (stream.cluster) or
a client of an external broker (stream.type: nats) — only set works there,
its value reaching the fleet at the next start, and list, get, unset and
rekey need the running engine or -api.

Through -api, a machine with no Tier A file at -config runs every command
but rekey, which reads the key it re-seals under from -config.
`

func runSecrets(environ config.Source, args []string, stdout, stderr io.Writer) error {
	sub, rest := splitSubject(args)
	if sub == "" || sub == "help" {
		fmt.Fprintf(stdout, secretsUsage, defaultBootstrapPath)
		return flag.ErrHelp
	}

	// AND THE NAME, for the same reason the subcommand came off first:
	// `secrets get TOKEN -config path` is the natural spelling, and Go's
	// flag package stops at the first non-flag argument — so parsing
	// before peeling would leave every flag after the name unparsed and
	// silently defaulted.
	name, rest := splitSubject(rest)

	fs := flag.NewFlagSet("secrets "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	bootstrapPath := fs.String("config", defaultBootstrapPath,
		"Tier A config: this node's store and its secret keyring")
	value := fs.String("value", "",
		"the secret's value; omitted reads it from stdin (set only)")
	reveal := fs.Bool("reveal", false,
		"actually print the secret (get only); the access is logged by name")
	keyID := fs.String("key-id", defaultKeyID,
		"the id the generated key is installed under (keygen only)")
	dryRun := fs.Bool("dry-run", false,
		"report what would be re-sealed without writing (rekey only)")
	apiURL := fs.String("api", "",
		"the running node's API; default is the api.host:port in -config")
	if err := fs.Parse(rest); err != nil {
		return err
	}

	// KEYGEN NEEDS NOTHING — no store, no keyring, not even a config. It
	// is what an operator runs BEFORE any of this exists, so requiring the
	// config it is about to be written into would be a chicken and egg.
	if sub == "keygen" {
		// BOTH tails: `keygen extra` puts the stray word in the peeled
		// name, `keygen -key-id x extra` leaves it in the flag tail, and
		// a check on one of them lets the other through silently.
		return generateKey(*keyID, append(nonEmpty(name), fs.Args()...), stdout, stderr)
	}

	// The trailing form — `secrets get -config path TOKEN` — leaves the
	// name here instead. Anything beyond one is an error rather than a
	// silently ignored argument.
	name, given := onePositional(fs, name)
	if given > 1 {
		return fmt.Errorf("secrets %s takes one name, got %d", sub, given)
	}
	// THE ENGINE'S OWN KEYS ARE REFUSED BEFORE ANYTHING OPENS, whichever
	// store this would reach: the node refuses them too, and a stopped
	// node's own table never holds one, so there is no route on which
	// asking could do anything but fail later and less clearly.
	if secrets.Reserved(name) {
		return fmt.Errorf("%w: %s\n\nThe address index's key belongs to the "+
			"identity estate. `crewlet secrets rekey` moves it with everything "+
			"else and counts it", secrets.ErrReservedName, name)
	}

	ctx := context.Background()
	// boot is the run's Tier A, read once the command line is known to be
	// good ([secretsBootstrap]), which never leaves it nil for a rekey.
	var boot *config.Bootstrap
	commands := map[string]func(sv *secretTarget) error{
		"list": func(sv *secretTarget) error { return listSecrets(ctx, sv, stdout) },
		"set": func(sv *secretTarget) error {
			return setSecret(ctx, sv, name, *value, isFlagSet(fs, "value"), stdout, stderr)
		},
		"get":   func(sv *secretTarget) error { return getSecret(ctx, sv, name, stdout) },
		"unset": func(sv *secretTarget) error { return unsetSecret(ctx, sv, name, stdout) },
		"rekey": func(sv *secretTarget) error {
			return rekeySecrets(ctx, sv, boot.Secrets.ActiveKeyID, *dryRun, stdout)
		},
	}
	// EVERY USAGE ERROR BEFORE A ROUTE IS DECIDED. A route may refuse — on a
	// stopped node whose own table is not the whole store it refuses every
	// subcommand but set ([stoppedNodeError]) — and a mistyped subcommand, a
	// missing name or a `get` without -reveal answered with advice to start
	// the engine sends an operator to fix what is not broken.
	command, known := commands[sub]
	if !known {
		fmt.Fprintf(stderr, secretsUsage, defaultBootstrapPath)
		return fmt.Errorf("unknown secrets command %q", sub)
	}
	if err := secretsCallError(sub, name, *reveal); err != nil {
		return err
	}

	run := storeRun{command: "crewlet secrets " + sub, reveals: sub == "get"}
	if sub == "set" {
		run.use, run.offline = actsOnItsOwn, true
	}
	boot, err := secretsBootstrap(*bootstrapPath, *apiURL, run.command, sub == "rekey")
	if err != nil {
		return err
	}
	sv, closeStore, err := routeSecretStore(ctx, environ, boot, *bootstrapPath, *apiURL, run)
	if err != nil {
		return err
	}
	defer closeStore()
	return command(sv)
}

// secretsCallError is what is wrong with how a known subcommand was called —
// a name it needs and was not given, a `get` without -reveal — or nil. Each is
// an answer about the command line alone, so it is given before a store is
// routed ([runSecrets]).
func secretsCallError(sub, name string, reveal bool) error {
	switch {
	case name == "" && (sub == "set" || sub == "get" || sub == "unset"):
		return fmt.Errorf("secrets %s needs a name", sub)
	case sub == "get" && !reveal:
		// REFUSED WITHOUT AN EXPLICIT FLAG: see [getSecret].
		return fmt.Errorf(
			"refusing to print %s: pass -reveal to read a secret back, and "+
				"note the access is logged. `crewlet secrets list` shows "+
				"whether it is set without revealing it", name)
	}
	return nil
}

// isFlagSet reports whether a flag was named on the command line.
//
// Needed because an EMPTY secret is a legitimate value — clearing a token
// without removing the row — and `-value ""` must be distinguishable from
// omitting the flag, which reads stdin instead.
func isFlagSet(fs *flag.FlagSet, name string) bool {
	seen := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			seen = true
		}
	})
	return seen
}

// secretBackend is the operations this command performs, on whichever store
// it turns out to be talking to.
//
// AN INTERFACE DEFINED BY THE CALLER, and a small one, because there are two
// implementations with genuinely different reach: the fleet's store behind a
// running node's API, which every node reads, and this node's own table,
// which only it can see. Every subcommand below is written against this and
// none of them branches on which it got — the one place that difference is
// visible is the line printed after a write, which is [secretTarget.where].
type secretBackend interface {
	List(ctx context.Context) ([]secrets.Record, error)

	// Set stores one value and reports the author the row RECORDS, which
	// is not always the one offered: on the fleet's store the node stamps
	// the party its own guard authenticated, and this command printed the
	// shell's user as the author of a row that named the API token instead.
	// The source is offered the same way, and only this node's own table
	// keeps it: the node stamps its route's own word, whatever the command.
	Set(ctx context.Context, name, value string, by secrets.Author,
		source secrets.Source, now time.Time) (secrets.Author, error)
	Get(ctx context.Context, name string) (string, error)

	// Values reads the values held under the names asked for, and no
	// other: a name nothing holds is absent from the answer. It is how a
	// run reads the variables a command resolves ([companyStore]), and
	// through a node it is a request per batch of names rather than one per
	// name.
	Values(ctx context.Context, names []string) (map[string]string, error)

	Unset(ctx context.Context, name string) (bool, error)
	Rekey(ctx context.Context, activeKeyID string) (rotation, error)

	// EngineKeys counts what the keyring seals that no command names —
	// the engine's own keys in the store and the identity estate's
	// personal values — per keyring key, which is what a rotation has to
	// see moved before the old key can go.
	EngineKeys(ctx context.Context) (engineHeld, error)
}

// rotation is what one rekey moved: the store's rows, and the identity
// estate's personal values, counted.
//
// IDENTITY IS NIL WHERE NOTHING MOVED IT, which is a different answer from
// "nothing to move": a stopped node's own table holds no identity estate, and
// a node that answered `identity: null` was reached without one.
type rotation struct {
	fleetsecrets.Rekeyed
	Identity *secretsapi.IdentityResealed
}

// engineHeld is what the keyring seals that no command names: the engine's own
// keys in the store, and — where the backend holds one — the identity estate's
// values, both counted per keyring key.
type engineHeld struct {
	Keys     secrets.EngineKeys
	Identity *iamdomain.SealedCount
}

// localSecrets is this node's own table as a backend, read while its engine is
// stopped.
//
// IT HOLDS NO ENGINE KEYS, by construction: the engine keeps its key material
// on the fleet and the node-local table refuses every name outside the
// reference grammar at the write. So its count is honestly zero and its rekey
// moves the operator's rows alone.
//
// # A read never creates the database
//
// The table is OPENED only where the file already exists. A node whose engine
// has never run here — or a machine that is not the node at all, holding a
// copy of its Tier A file — has no rows to read, and opening the path to find
// that out would create an empty database there that nothing ever reads, and
// a lock file beside it. So every read of an absent file answers what an
// empty table answers, and only a WRITE opens it, creating it as the bootstrap
// path does: `crewlet secrets set` on a node that has never started is how a
// value reaches it before its first boot.
type localSecrets struct {
	boot          *config.Bootstrap
	bootstrapPath string

	// mu guards db and values, which the first write fills where the file
	// did not exist when the store was routed.
	mu     sync.Mutex
	db     *store.DB
	values *store.SecretValues
}

// openLocalSecrets is this node's own table as Tier A names it, opened where
// the file exists — the open's own error, [store.ErrLocked] included, comes
// back unwrapped for the caller to route — and left closed where it does not.
func openLocalSecrets(ctx context.Context, boot *config.Bootstrap, bootstrapPath string) (*localSecrets, error) {
	l := &localSecrets{boot: boot, bootstrapPath: bootstrapPath}
	switch _, err := os.Stat(boot.Store.Path); {
	case errors.Is(err, os.ErrNotExist):
		return l, nil
	case err != nil:
		return nil, fmt.Errorf("open store: %w", err)
	}
	if err := l.open(ctx); err != nil {
		return nil, err
	}
	return l, nil
}

// open opens the node's file under its keyring. The caller holds mu, or is the
// constructor nothing else can reach yet.
func (l *localSecrets) open(ctx context.Context) error {
	cipher, err := l.boot.Secrets.Cipher()
	if err != nil {
		return fmt.Errorf("secrets keyring: %w", err)
	}
	// NO EMBEDDING WIDTH, which is deliberate: opening the store for a
	// secret read must not depend on the company document, and the vector
	// columns are only sized when a migration actually runs — which a
	// running node has already done.
	// THE NODE'S OWN FILE ALONE: revisions and the bootstrap secret store
	// are the node's own, and this command reads nothing a state log
	// applies, so it opens no replicated estate.
	db, err := store.OpenNode(ctx, l.boot.Store.Path, store.Options{
		MaxOpenConns: l.boot.Store.MaxOpenConns,
		BusyTimeout:  l.boot.Store.BusyTimeout(),
	})
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	l.db, l.values = db, db.SecretValues(cipher)
	return nil
}

// read is the table to read, or nil where the file does not exist.
func (l *localSecrets) read() *store.SecretValues {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.values
}

// write is the table to write, opened — and so created — where the file did
// not exist when this run was routed.
//
// A LOCK MET HERE is an engine that started on this node after the run was
// routed to its stopped table, which is answered rather than routed: the run
// has already read the table it is writing, and a write sent to the node now
// would land beside reads it did not make.
func (l *localSecrets) write(ctx context.Context) (*store.SecretValues, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.values != nil {
		return l.values, nil
	}
	if err := l.open(ctx); err != nil {
		return nil, engineHoldsTheStore(err, l.bootstrapPath,
			"It started while this ran: re-run it, and the run goes through "+
				"its API.")
	}
	return l.values, nil
}

// close releases the file, where a read or a write opened it.
func (l *localSecrets) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.db != nil {
		_ = l.db.Close()
		l.db, l.values = nil, nil
	}
}

// List implements the listing half of the backend.
func (l *localSecrets) List(ctx context.Context) ([]secrets.Record, error) {
	if sv := l.read(); sv != nil {
		return sv.List(ctx)
	}
	return nil, nil
}

// Set writes the row under the author and source offered, which on this node's
// own table are what it records.
//
// A WRITE THE TABLE WOULD REFUSE OPENS NOTHING: the name, the source and the
// value are held to the table's own rules before the file is opened, so a
// refused write on a node that has never started leaves no database behind,
// as a read does not.
func (l *localSecrets) Set(ctx context.Context, name, value string, by secrets.Author,
	source secrets.Source, now time.Time) (secrets.Author, error) {

	if err := errors.Join(secrets.CheckName(name), secrets.CheckSource(source),
		secrets.CheckValue(value)); err != nil {
		return secrets.Author{}, err
	}
	sv, err := l.write(ctx)
	if err != nil {
		return secrets.Author{}, err
	}
	return by, sv.Set(ctx, name, value, by, source, now)
}

// Get reads one value back.
func (l *localSecrets) Get(ctx context.Context, name string) (string, error) {
	if sv := l.read(); sv != nil {
		return sv.Get(ctx, name)
	}
	return "", fmt.Errorf("%w: %s", secrets.ErrNotFound, name)
}

// Values reads every name asked from the table, one local read each, and
// fails on any read that is not a plain miss: an unreadable row is not an
// absent one.
func (l *localSecrets) Values(ctx context.Context, names []string) (map[string]string, error) {
	out := make(map[string]string, len(names))
	sv := l.read()
	if sv == nil {
		return out, nil
	}
	for _, name := range names {
		value, err := sv.Get(ctx, name)
		switch {
		case errors.Is(err, secrets.ErrNotFound):
		case err != nil:
			return nil, err
		default:
			out[name] = value
		}
	}
	return out, nil
}

// Unset removes one value, reporting whether it was there.
func (l *localSecrets) Unset(ctx context.Context, name string) (bool, error) {
	if sv := l.read(); sv != nil {
		return sv.Unset(ctx, name)
	}
	return false, nil
}

// Rekey re-seals the table's stale rows under activeKeyID.
func (l *localSecrets) Rekey(ctx context.Context, activeKeyID string) (rotation, error) {
	sv := l.read()
	if sv == nil {
		return rotation{}, nil
	}
	moved, err := sv.Rekey(ctx, activeKeyID)
	return rotation{Rekeyed: fleetsecrets.Rekeyed{Moved: moved}}, err
}

func (*localSecrets) EngineKeys(context.Context) (engineHeld, error) {
	return engineHeld{}, nil
}

// secretTarget is the backend plus what to tell the operator about it.
type secretTarget struct {
	secretBackend

	// fleet reports whether a write here reaches every node.
	fleet bool

	// where names the store, for the line printed after a write. Said
	// EVERY TIME rather than only when it is the node-local one: an
	// operator who cannot tell which of the two a rotation landed in has
	// to find out by watching a vendor accept or reject a credential.
	where string

	// node is the address a fleet target reaches the store through, for the
	// line a run that reads through it prints; empty on this node's own
	// table.
	node string

	// locked is the store's own refusal where the run was sent through the
	// node because the engine holds this node's database — which a failed
	// read through it names beside its own failure — and nil where -api
	// named the node, or the target is this node's own table.
	locked error

	// stopped is why this node's own rows are not the company's whole store,
	// on a local target a run that acts on its own was let through
	// ([actsOnItsOwn]); nil where they are — a sole member whose broker has
	// never started — and on every fleet target.
	stopped *stoppedNodeError
}

// wholeStore reports whether this target is this node's own table and that
// table is the company's whole secret store: a sole member's, before its
// broker's first start ([stoppedNode]).
func (t *secretTarget) wholeStore() bool { return !t.fleet && t.stopped == nil }

// secretsBootstrap is the Tier A document a `crewlet secrets` run is routed
// under ([routeSecretStore]): the file at bootstrapPath, or nil where there is
// none and apiURL names the node to go through — a machine that is not a node,
// which needs none. command names the run for a refusal, and needsKeyID is
// whether it needs Tier A's `secrets.active_key_id`, which `rekey` does.
//
// NO KEYRING CHECK OF ITS OWN: Tier A refuses a file without one, so a
// document that loaded carries it, and the refusal an operator sees names
// `secrets.keys` in the file's own terms. And a file that exists and does not
// load fails the run, -api or not, as it fails every command that reads the
// company ([companyBootstrap]): someone who configured a node and did not get
// it must not have the run quietly go somewhere else.
//
// # Where no file is no answer
//
// With no -api the file is the only way to the store: it names this node's
// table, the keyring that table is sealed under, and the address of the
// engine that holds it. A command reading the company resolves from the
// environment alone there ([companyStore.route]); this one has nothing to fall
// back to, since the store is its whole subject, so it is refused, naming both
// ways to a store — or, for a rekey, naming only -config, since -api would
// not spare it the file.
//
// A rekey needs the file through -api too: it judges the rows against the
// active key id it expects, Tier A's `secrets.active_key_id`. Its -dry-run
// lists, from the node's listing, what is sealed under any other key. Its
// real pass also sends that id, and the node refuses a rotation onto a key it
// does not seal with: that check is what keeps a rekey from reporting a
// rotation the fleet did not make, and sending the node's own key in its place
// would check nothing.
func secretsBootstrap(bootstrapPath, apiURL, command string, needsKeyID bool) (*config.Bootstrap, error) {
	if strings.TrimSpace(apiURL) == "" {
		boot, err := loadBootstrapForStore(bootstrapPath)
		if errors.Is(err, os.ErrNotExist) {
			remedy := "Point -config at it, or pass -api URL naming a running node"
			if needsKeyID {
				remedy = "Point -config at it: a rekey needs the file even through " +
					"-api, since it judges the rows against the secrets.active_key_id " +
					"the file names"
			}
			return nil, fmt.Errorf("%s: %w\n\nThat file names this node's store and "+
				"the keyring it is sealed under. %s", command, err, remedy)
		}
		return boot, err
	}
	boot, err := companyBootstrap(bootstrapPath)
	if err != nil || boot != nil || !needsKeyID {
		return boot, err
	}
	return nil, fmt.Errorf("%s: there is no Tier A file at %s, and a rekey reads from "+
		"it the key it re-seals under (secrets.active_key_id): the rows are judged "+
		"against that key, -dry-run's included, and the pass sends it for the node to "+
		"check against the key it seals with. Point -config at a crewlet.yaml carrying "+
		"the fleet's keyring; -api still names the node to go through", command, bootstrapPath)
}

// storeUse is what a run does with the company's stored values, which decides
// whether a stopped node's own rows can serve it ([stoppedNode]).
type storeUse int

const (
	// readsTheCompany is a run whose answer or act depends on the values the
	// company's store holds: every command that resolves the company — each
	// vendor command, `crewlet llm list|status|export|doctor` — and `crewlet
	// secrets list|get|unset|rekey`, which read, remove or re-seal them.
	//
	// THE ZERO VALUE, so a run that declares nothing is held to the rule
	// rather than let through it: on a stopped node whose rows are not the
	// company's whole store, such a run is refused before it reads anything.
	readsTheCompany storeUse = iota

	// actsOnItsOwn is a run whose act is right whatever the fleet holds,
	// because what it acts on is its own: a new value it writes (`crewlet
	// secrets set`, a token `crewlet llm login` captures), or this host's
	// login (`crewlet llm logout`, `import`, which write no row). It runs
	// on any stopped node — a value it writes there reaches the fleet at the
	// node's next start, the later of it and the fleet's winning
	// ([fleetsecrets.Migrate]) — and reads the entry it works on from the
	// rows and the environment there.
	actsOnItsOwn
)

// storeRun is one command's use of the company's secret store, which is what a
// refusal to reach the store has to answer in that command's own terms.
//
// THE WAY PAST A REFUSAL DEPENDS ON WHAT THE RUN DOES WITH THE STORE, and one
// sentence written for `crewlet secrets set` sent every other caller somewhere
// wrong: a provisioner was told to stop the engine, which on a node that has
// run leaves it rows the fleet's store outranks; it was offered "an access
// token", which the node's reveal refuses; and it was told the value could
// come from the environment, which a routed read never falls back to by
// design. That last one was wrong for `secrets set` too: the store outranks
// the environment in the engine's resolver, so an exported value never
// replaces one the store holds — which a rotation's always does.
type storeRun struct {
	// command is the command line that was run, which a refusal names.
	command string

	// use is what the run does with the stored values, which decides
	// whether a stopped node's own rows can serve it ([storeUse]).
	use storeUse

	// reveals reports whether the run reads values back through a node —
	// the values a command resolves, a seat credential a provisioning sink
	// reads back, `crewlet secrets get -reveal`. That is the node's
	// break-glass read, `secrets.reveal`: admin, a recent proof and a person
	// present, which refuses a machine token from `crewlet iam token` on
	// every arm — so the bearer such a run needs is one of the node's own
	// Tier A tokens.
	reveals bool

	// offline reports whether stopping the engine is a way to do what the
	// run does: `crewlet secrets set` alone, whose row the next start moves
	// onto the fleet over an earlier value of the fleet's. Every other run
	// is either refused on a stopped node whose own table is not the whole
	// store ([stoppedNode]), or — `crewlet llm login|logout|import` — works
	// on a login whose entry it would then build from one node's rows, so
	// stopping the engine is offered to none of them.
	offline bool
}

// token is the bearer the run presents to the node, or the refusal naming the
// one it needs.
func (r storeRun) token(environ config.Source) (string, error) {
	if !r.reveals {
		return nodeAPIToken(environ, "/secrets")
	}
	if token := nodeTokenOrEmpty(environ); token != "" {
		return token, nil
	}
	return "", fmt.Errorf("no token to reach this node's /secrets API, which %s "+
		"reads the company's stored values through.%s", r.command, revealTakesATierAToken)
}

// unreachable is what the run is told to do where the engine holds this
// node's store and its API cannot be used.
//
// A NODE THAT SERVES ADMIN, WHATEVER THE RUN DOES: every /secrets route — the
// listing, a write, a reveal — is admin's, and a node whose api.auth.admin is
// refuse clamps admin away from every token it is shown, so it serves none of
// them. Which token the run needs there is the token's own refusal to say
// ([storeRun.token]): a reveal's alone takes one of Tier A's.
func (r storeRun) unreachable() string {
	node := "pass -api URL naming a node of this company whose api.auth.admin is serve"
	switch {
	case r.offline:
		return "Either fix that, " + node + ", or stop `crewlet run` on this " +
			"node and re-run: the value lands in this node's own table, and its " +
			"next start moves it onto the fleet over the value the fleet holds"
	case r.use == actsOnItsOwn:
		return "Fix that, or " + node
	}
	return "Fix that, or " + node + ". Stopping `crewlet run` here is no way " +
		"around it: once this node's engine has run, the company's stored values " +
		"are the fleet's, which only a running engine serves"
}

// revealTakesATierAToken is what a read through a node that was refused, or
// has no token to send, tells the operator to change: reading a value is a
// reveal, which the authority table gives admin with a person present, so a
// machine token — which acts for its owner with nobody there — is refused it
// whatever it carries, and the token that reads is one of Tier A's.
const revealTakesATierAToken = "\n\nEach value is read with a reveal, which takes admin and " +
	"a person present: " + apiTokenEnv + " must hold one of Tier A's api.auth " +
	"tokens, since a machine token is refused a reveal whatever it carries"

// routeSecretStore picks the store a run can actually reach: the running
// node's API, or this node's own table while its engine is stopped — and on a
// stopped node, whether that table can serve the run at all. boot may be nil
// only where apiURL names the node: a machine that is not a node, holding no
// Tier A file. run names the command and what it does with the store, for a
// refusal an operator reads.
//
// # The running engine decides, and it decides unambiguously
//
// The fleet's rows live on the coordination KV, which on the default topology
// is inside the engine's process and listens on no socket — so while the
// engine is up, the only way in is its API. While it is down there is no KV
// at all, and this node's own table is the only place a value can go until
// the engine migrates it at the next start.
//
// The STORE LOCK is what tells the two apart, and it is a fact rather than a
// guess: the engine holds an OS advisory lock on its database file for
// exactly as long as it is running, so [store.ErrLocked] means "the engine is
// up" with a pid attached, and a successful open — or a probe of the lock
// that finds it free, below — means it is not. Probing the API instead would
// confuse "the node is stopped" with "the node is up but its HTTP port is
// bound elsewhere", and those need opposite answers. A file that does not
// exist is held by nothing, and is not created to find that out (see
// [localSecrets]).
//
// # A stopped node's table serves a reader only where it is the whole store
//
// Which [stoppedNode] decides from Tier A and the broker's own directory, and
// a run that reads the company is refused on any other stopped node before it
// reads anything ([storeRun.use]). Where its file exists, the table stays open
// for the rest of a run it serves, so `crewlet run` cannot start beneath it
// and change the answer. Where none exists yet, nothing is open and no lock is
// held, so an engine can start part way through the run; the run's first
// write then meets its lock and is refused ([localSecrets.write]).
//
// A RUN THAT VERDICT REFUSES NEVER OPENS THE FILE. Whether the engine holds it
// is asked of the lock alone ([store.ProbeLock]) — held, the run goes through
// the engine as any other does; free, it is refused — because an open applies
// any pending schema, and a refusal that says nothing was changed must not
// have migrated the node's database on its way to saying so.
func routeSecretStore(ctx context.Context, environ config.Source, boot *config.Bootstrap,
	bootstrapPath, apiURL string, run storeRun) (*secretTarget, func(), error) {

	// AN EXPLICIT -api SKIPS THE PROBE ENTIRELY. Naming a node is an
	// instruction to go through it, and it is also how a run works from a
	// machine that is not the node at all — where the local database in
	// the Tier A file does not exist.
	if strings.TrimSpace(apiURL) != "" {
		client, err := newSecretsClient(environ, boot, apiURL, run)
		if err != nil {
			return nil, nil, err
		}
		return &secretTarget{secretBackend: client, fleet: true,
			where: client.Describe(), node: client.base}, func() {}, nil
	}
	if err := refuseScratchStore(boot, run.command,
		"Pass -api URL naming a node that holds data: its /secrets surface "+
			"reads and writes the company's credential store, which every "+
			"node reads."); err != nil {
		return nil, nil, err
	}
	stopped := stoppedNode(boot, bootstrapPath, run.command)
	if stopped != nil && run.use == readsTheCompany {
		if err := store.ProbeLock(boot.Store.Path); err != nil {
			target, rerr := throughTheRunningNode(environ, boot, bootstrapPath, run, err)
			if rerr != nil {
				return nil, nil, rerr
			}
			return target, func() {}, nil
		}
		return nil, nil, stopped
	}
	local, err := openLocalSecrets(ctx, boot, bootstrapPath)
	if err != nil {
		target, rerr := throughTheRunningNode(environ, boot, bootstrapPath, run, err)
		if rerr != nil {
			return nil, nil, rerr
		}
		return target, func() {}, nil
	}
	return &secretTarget{
		secretBackend: local, fleet: false, stopped: stopped,
		where: boot.Store.Path + " — this node's own rows, which no peer " +
			"can see until the engine migrates them at its next start",
	}, local.close, nil
}

// throughTheRunningNode answers a locked store with a client for the node
// that is holding it.
//
// [store.ErrLocked] is the ONLY error routed this way, and the distinction
// matters: a locked file means the engine is up, which is precisely when its
// API is the way in. Every other failure — a missing keyring, an unreadable
// path, a driver that will not load — is a broken node, and answering one of
// those by silently trying HTTP would replace an accurate message with a
// connection refused.
//
// The way past a node the run cannot use is the run's own ([storeRun]).
func throughTheRunningNode(environ config.Source, boot *config.Bootstrap, bootstrapPath string,
	run storeRun, err error) (*secretTarget, error) {

	if !errors.Is(err, store.ErrLocked) {
		return nil, err
	}
	client, cerr := newSecretsClient(environ, boot, "", run)
	if cerr != nil {
		// BOTH FACTS, because either alone is misleading. "The engine
		// holds the database" without "and here is why I could not go
		// through its API" reads as a refusal with no way forward, and
		// the API's own complaint without the lock reads as though the
		// local store were never an option.
		return nil, fmt.Errorf("%w\n\nthe engine for %s is running and holds "+
			"its database, so %s has to go through its API — and it cannot: "+
			"%w\n\n%s", err, bootstrapPath, run.command, cerr, run.unreachable())
	}
	return &secretTarget{secretBackend: client, fleet: true,
		where: client.Describe(), node: client.base, locked: err}, nil
}

// engineHoldsTheStore turns the store's lock refusal into a remediation.
//
// [store.ErrLocked] already says which file and which pid. What it cannot say
// is what an operator of THIS command should do about it, because the store
// does not know which command opened it second. So the sentinel is caught at
// each call site and answered in that command's own terms — `remedy` is the
// route around the lock, and every caller has a different one.
//
// A REFUSAL RATHER THAN A WARNING, which is the whole reason the lock exists:
// before it, these commands printed a caution and opened the file anyway, so
// an operator who did not read it corrupted the database.
func engineHoldsTheStore(err error, bootstrapPath, remedy string) error {
	if !errors.Is(err, store.ErrLocked) {
		return err
	}
	return fmt.Errorf("%w\n\nthe engine for %s is running and holds its "+
		"database; the driver allows only one process on a file. %s",
		err, bootstrapPath, remedy)
}

// loadBootstrapForStore reads the Tier A document that names the store.
func loadBootstrapForStore(bootstrapPath string) (*config.Bootstrap, error) {
	// TIER A RESOLVES FROM THE ENVIRONMENT ALONE, and that is structural
	// rather than a default: this file carries the store's address and the
	// keys that open it, so a resolver reaching the store would have Tier A
	// reading from the thing it describes.
	return config.LoadBootstrap(bootstrapPath, config.EnvOnly())
}

func listSecrets(ctx context.Context, sv *secretTarget, stdout io.Writer) error {
	rows, err := sv.List(ctx)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Fprintf(stdout, "no secrets are stored in %s\n", sv.where)
	} else {
		// WHICH STORE, above the table. The two hold different rows, and a
		// listing that did not say which one it read is one an operator can
		// misread as "the fleet has nothing" when they are looking at a
		// stopped node's own empty table.
		fmt.Fprintf(stdout, "%s\n\n", sv.where)
		// NO VALUES, ever. This is what an operator reads to answer "is X
		// set", and a listing that printed plaintext would put a company's
		// whole credential set on one screen — and into one scrollback
		// buffer.
		w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tKEY\tUPDATED\tBY\tSOURCE")
		for _, r := range rows {
			// THE AUTHOR, AND THE CREDENTIAL BESIDE IT where one made the
			// write: a row a person wrote through their machine token is
			// theirs, and saying which token is what tells it from one
			// they wrote themselves.
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", r.Name, r.KeyID,
				r.UpdatedAt.Format(time.RFC3339),
				describeAuthor(r.UpdatedBy, r.OperatorID), r.Source)
		}
		if err = w.Flush(); err != nil {
			return err
		}
	}
	held, err := sv.EngineKeys(ctx)
	if err != nil {
		return err
	}
	if held.Keys.Total > 0 {
		// COUNTED, NEVER NAMED, and said at all because a rotation has to
		// move them: a listing that stayed silent about them would read
		// as a store holding nothing else.
		fmt.Fprintf(stdout, "\nand %d of the engine's own keys (the identity "+
			"estate's), which no command reads or writes and a rekey moves\n",
			held.Keys.Total)
	}
	if held.Identity != nil {
		if values := total(held.Identity.People); values > 0 {
			fmt.Fprintf(stdout, "and %d personal values in the identity estate "+
				"(names, addresses, second factors), sealed under the same "+
				"keyring, which a rekey re-seals\n", values)
		}
	}
	return nil
}

// total sums a per-key count.
func total(byKey map[string]int) int {
	n := 0
	for _, count := range byKey {
		n += count
	}
	return n
}

func setSecret(ctx context.Context, sv *secretTarget, name, value string,
	valueGiven bool, stdout, stderr io.Writer,
) error {
	if !valueGiven {
		// STDIN IS THE DEFAULT, not the fallback: a secret passed as an
		// argument is in the shell history, in `ps` output, and in any
		// process listing a colleague runs while it is executing.
		fmt.Fprintf(stderr, "reading %s from stdin...\n", name)
		read, err := secretFromReader(os.Stdin)
		if err != nil {
			return fmt.Errorf("read %s from stdin: %w", name, err)
		}
		value = read
	}
	// THIS COMMAND'S OWN WORD, and no flag offers another: a stopped node's
	// table records `cli`, and a running node records its route's `api`.
	recorded, err := sv.Set(ctx, name, value, hostAuthor(), secrets.SourceCLI,
		time.Now().UTC())
	if err != nil {
		return err
	}
	// The NAME and nothing else. Confirming the value would undo the whole
	// reason it was read from stdin. The author is the one the ROW records,
	// and the credential beside it where one made the write.
	fmt.Fprintf(stdout, "stored %s (%d bytes) as %s\n", name, len(value),
		describeAuthor(recorded.Name, recorded.OperatorID))
	// SAID EVERY TIME, because the difference between the two stores is
	// invisible afterwards and decides whether a fleet has the value: an
	// operator who cannot tell which one they wrote finds out when a vendor
	// rejects a credential on the one node that never got it.
	fmt.Fprintf(stdout, "written to %s\n", sv.where)
	fmt.Fprintln(stdout, sv.writtenNote())
	return nil
}

// writtenNote is what a write to this store has not done yet, said after every
// write of a value a running engine reads: [secretsFleetNote] through a node,
// [secretsLocalNote] on this node's own table.
//
// ONE ANSWER FOR EVERY COMMAND THAT WRITES ONE, because each of them leaves
// the same gap: `crewlet llm login -capture-token` and `-token-stdin`, and
// `crewlet llm export -secret-store`, once said only where the value went, and
// a running fleet kept signing in without the new token until something
// applied the company.
func (t *secretTarget) writtenNote() string {
	if t.fleet {
		return secretsFleetNote
	}
	return secretsLocalNote
}

// secretsFleetNote is what a write to the FLEET's store does not do yet.
//
// The engine resolves a `${VAR}` through a snapshot of the store taken at each
// configuration apply, so a value written to a running fleet is read at its
// next apply and not before — and an operator who rotated a key and saw the
// old one still in use would conclude the write failed. Re-activating the
// current revision is the gesture, and on a running fleet it is the node's
// reload.
const secretsFleetNote = "The running fleet reads it at its next configuration " +
	"apply; POST /config/reload to a node that is up re-activates the current " +
	"revision now."

// secretsLocalNote is what a write to the node-local table did and did not do.
//
// Said out loud rather than left to be discovered, for the same reason
// `config import` says it: an operator who wrote a value while the engine was
// stopped and saw nothing propagate would reasonably conclude the write
// failed, and the fix — start the node, or point -api at one that is up — is
// not guessable.
const secretsLocalNote = "This node will put it on the fleet at its next start, " +
	"unless the fleet holds a value written after this one. " +
	"To reach a RUNNING fleet now, re-run against a node that is up."

// getSecret is the ONLY command that prints a value, and it is break-glass.
//
// It is refused without an explicit flag — before any store is routed
// ([secretsCallError]) — because the overwhelmingly common need is "is X set
// and when did it change", which the listing answers without putting a
// credential into a terminal, a scrollback buffer and a screen-share. The HTTP
// route it reads through a node is gated the same way,
// by a ?reveal=true a crawl cannot reach by accident; the node's other route
// that returns values, POST /secrets/reveal, is a POST no crawl can make.
//
// The access is LOGGED BY NAME, here and — when this goes through a running
// node — there too, against the operator its own guard authenticated. A
// break-glass read that leaves no trace is indistinguishable from an
// exfiltration, and the name is the whole of what can be logged: logging the
// value would be the leak.
func getSecret(ctx context.Context, sv *secretTarget, name string, stdout io.Writer) error {
	value, err := sv.Get(ctx, name)
	if err != nil {
		return err
	}
	logging.Get("cli").Warn("secret_revealed", "name", name, "operator", currentOperator())
	// NO TRAILING NEWLINE and no decoration: this is what gets piped into
	// another command, and a newline would be part of the token.
	_, err = io.WriteString(stdout, value)
	return err
}

func unsetSecret(ctx context.Context, sv *secretTarget, name string, stdout io.Writer) error {
	gone, err := sv.Unset(ctx, name)
	if err != nil {
		return err
	}
	if !gone {
		fmt.Fprintf(stdout, "%s was not set\n", name)
		return nil
	}
	fmt.Fprintf(stdout, "removed %s\n", name)
	return nil
}

// rekeySecrets re-seals what sv holds under active, the run's Tier A
// `secrets.active_key_id` ([secretsBootstrap]), or with dryRun reports what it
// would re-seal.
func rekeySecrets(ctx context.Context, sv *secretTarget, active string,
	dryRun bool, stdout io.Writer,
) error {
	if dryRun {
		// LISTED FROM THE KEY ID COLUMN, which is denormalised out of the
		// envelope for exactly this: reporting what a rekey would touch
		// must not decrypt anything, or the dry run is a bigger exposure
		// than the pass it is previewing.
		rows, err := sv.List(ctx)
		if err != nil {
			return err
		}
		stale := 0
		for _, r := range rows {
			if r.KeyID == active {
				continue
			}
			stale++
			fmt.Fprintf(stdout, "  %s (sealed under %s)\n", r.Name, r.KeyID)
		}
		// AND WHAT NO COMMAND NAMES, counted: a dry run that said "nothing
		// to do" while the address index's key or every person's name was
		// still under the old key would be the reading an operator retires
		// that key on.
		held, err := sv.EngineKeys(ctx)
		if err != nil {
			return err
		}
		engineStale := held.Keys.StaleUnder(active)
		if engineStale > 0 {
			fmt.Fprintf(stdout, "  %d of the engine's own keys (sealed under "+
				"another key)\n", engineStale)
		}
		identityStale := 0
		if held.Identity != nil {
			identityStale = held.Identity.Outside(active)
			if identityStale > 0 {
				fmt.Fprintf(stdout, "  %d personal values in the identity estate "+
					"(sealed under another key)\n", identityStale)
			}
		}
		waitOutInvitations(stdout, held, active)
		if stale == 0 && engineStale == 0 && identityStale == 0 {
			fmt.Fprintf(stdout, "every secret is already sealed under %s\n", active)
			noIdentityHere(stdout, sv, held.Identity == nil)
			return nil
		}
		fmt.Fprintf(stdout, "%d secrets, %d engine keys and %d personal values "+
			"would be re-sealed under %s\n", stale, engineStale, identityStale, active)
		noIdentityHere(stdout, sv, held.Identity == nil)
		return nil
	}
	rekeyed, err := sv.Rekey(ctx, active)
	if err != nil {
		return err
	}
	identityMoved := 0
	if rekeyed.Identity != nil {
		identityMoved = rekeyed.Identity.Values
	}
	if len(rekeyed.Moved) == 0 && rekeyed.EngineKeys == 0 && identityMoved == 0 {
		fmt.Fprintf(stdout, "every secret is already sealed under %s\n", active)
	} else {
		// THE NAMES, not a count: a pass that moved 12 of 13 rows raises
		// a question a number cannot answer, and this is the last chance
		// to see which rows are now safe to retire the old key over. The
		// engine's own keys and the identity estate's values are the
		// exception, counted for the listing's reason.
		fmt.Fprintf(stdout, "re-sealed %d secrets under %s:\n", len(rekeyed.Moved), active)
		for _, name := range rekeyed.Moved {
			fmt.Fprintf(stdout, "  %s\n", name)
		}
		if rekeyed.EngineKeys > 0 {
			fmt.Fprintf(stdout, "and %d of the engine's own keys\n", rekeyed.EngineKeys)
		}
		if identityMoved > 0 {
			fmt.Fprintf(stdout, "and %d personal values of %d people in the "+
				"identity estate\n", identityMoved, rekeyed.Identity.People)
		}
	}
	noIdentityHere(stdout, sv, rekeyed.Identity == nil)
	// AND WHAT A REKEY CANNOT MOVE, read after it: the answer the operator
	// drops the old key on has to include it.
	held, err := sv.EngineKeys(ctx)
	if err != nil {
		return err
	}
	waitOutInvitations(stdout, held, active)
	return nil
}

// waitOutInvitations says how many live invitations are still sealed under
// another key — which no rekey moves, because only a re-issue could carry the
// address again and its own claim refuses one while the invitation is open.
// The old key must stay on the ring until each is redeemed or lapses.
func waitOutInvitations(stdout io.Writer, held engineHeld, active string) {
	if held.Identity == nil {
		return
	}
	if n := held.Identity.InvitationsOutside(active); n > 0 {
		fmt.Fprintf(stdout, "%d outstanding invitations are sealed under another "+
			"key and are not re-sealed: keep that key on the ring until each is "+
			"redeemed or expires (at most %s after it was issued)\n",
			n, credential.EnrolmentLinkLifetime)
	}
}

// noIdentityHere says that a rotation through this backend did not reach the
// identity estate, where it could not: a stopped node's own table holds none,
// and a running node answering `identity: null` served its /secrets without
// one — never a node `crewlet run` builds, which holds the estate from boot,
// company or none, but an answer the wire can carry. SAID, because the silence
// reads as an estate with nothing to move.
func noIdentityHere(stdout io.Writer, sv *secretTarget, absent bool) {
	if !absent {
		return
	}
	if sv.fleet {
		fmt.Fprintln(stdout, "the node answered with no identity estate, so no "+
			"person's values were counted or re-sealed through it")
		return
	}
	fmt.Fprintln(stdout, "a stopped node's own table holds no identity estate: "+
		"run the rekey again through the running node to re-seal every "+
		"person's values")
}

// generateKey prints one fresh keyring key.
//
// BASE64 of 32 raw bytes, which is the form the config's `material` field
// takes — so the output is pasteable rather than something to convert.
func generateKey(keyID string, rest []string, stdout, stderr io.Writer) error {
	if len(rest) > 0 {
		fmt.Fprintln(stderr, "usage: crewlet secrets keygen [-key-id ID]")
		return errors.New("secrets keygen takes no positional arguments")
	}
	if strings.TrimSpace(keyID) == "" {
		return errors.New("secrets keygen needs a key id")
	}
	key, err := secrets.GenerateKey()
	if err != nil {
		return fmt.Errorf("generate key: %w", err)
	}
	material := secrets.EncodeKey(key)
	// THE SNIPPET REFERENCES A VARIABLE rather than inlining the key,
	// because config.yaml is the file people commit. Pasting a raw key
	// into it is the single most likely way this key ends up in a
	// repository, and the shape that avoids it should be the one handed
	// over rather than one an operator has to know to write.
	fmt.Fprintf(stdout, `%s

Add this to your Tier A config, and export the key rather than inlining it:

secrets:
  active_key_id: %s
  keys:
    - id: %s
      material: "${%s}"

  export %s=%s

Key generation is always explicit — a silently generated key that nobody
captured makes every encrypted row unrecoverable. Keep the id stable across
restarts: it is stamped into every envelope this key seals, and only a
rotation should introduce a new one.
`, material, keyID, keyID, keyEnvVar(keyID), keyEnvVar(keyID), material)
	return nil
}

// defaultKeyID is the id a generated key is installed under.
//
// A NAME, not a timestamp or a uuid: it is stamped into every envelope the
// key seals and an operator reads it in a listing to see which rows are
// stale, so it has to be something a person recognises.
const defaultKeyID = "key-1"

// keyEnvVar is the variable the generated snippet reads the key from.
//
// CREWLET_SECRET_KEY_<ID>, the name every document and example uses for a
// keyring key. The snippet printed CREWLET_SECRET_<ID> while the docs told an
// operator to export CREWLET_SECRET_KEY_<ID>, so following the page beside the
// command's own output exported a variable the pasted config never read, and
// the node refused to open its sealed revision naming a variable that was
// "set".
func keyEnvVar(keyID string) string {
	upper := strings.ToUpper(keyID)
	upper = strings.ReplaceAll(upper, "-", "_")
	upper = strings.ReplaceAll(upper, ".", "_")
	return "CREWLET_SECRET_KEY_" + upper
}

// hostAuthor is who a command run on this host records as a row's author: the
// shell's account, of the operator kind, and NO credential — none made the
// write. A write through a running node is attributed by the node instead.
func hostAuthor() secrets.Author {
	return secrets.Author{Name: currentOperator(), Kind: string(iam.ActorOperator)}
}

// hostActor is [hostAuthor] for a configuration revision.
func hostActor() iam.Actor {
	return iam.Actor{Name: currentOperator(), Kind: iam.ActorOperator}
}

// describeAuthor renders a recorded author for a line an operator reads: the
// name, and the credential beside it when one made the write and it is not
// the name itself — a Tier A token's is.
func describeAuthor(name, operatorID string) string {
	if operatorID == "" || operatorID == name {
		return name
	}
	return name + " (through " + operatorID + ")"
}

// currentOperator names who wrote a row, for the provenance column.
//
// Best effort by design: it answers "where did this credential come from"
// months later, and an unattributed row is far better than refusing the
// write because the environment named nobody.
func currentOperator() string {
	for _, key := range []string{"CREWLET_OPERATOR", "USER", "LOGNAME"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
	}
	return "unknown"
}

// secretFromReader reads a piped secret.
//
// ONE trailing newline is stripped and no more, because `echo secret | ...`
// is how this is used: a token carrying a newline fails at the vendor with a
// 401 that names neither the newline nor this command. Stripping MORE would
// be wrong in the other direction — a secret may legitimately end in
// whitespace, and silently altering it is a failure nobody can see.
//
// A VALUE LONGER THAN ANY STORE KEEPS is refused here, before the rest of it
// is read: see [readPipedValue].
func secretFromReader(r io.Reader) (string, error) {
	body, err := readPipedValue(r)
	if err != nil {
		return "", err
	}
	value := strings.TrimSuffix(body, "\n")
	if err := secrets.CheckValue(value); err != nil {
		return "", err
	}
	return value, nil
}

// readPipedValue reads a value piped to a command to be stored, and refuses —
// without reading the rest — one longer than [secrets.MaxValueBytes] and the
// one trailing newline a pipe adds.
//
// BOUNDED, because what arrives on a pipe is whatever the operator pointed at
// it: `crewlet secrets set X < backup.tar` would otherwise be read whole into
// memory only for the store to refuse it, and a stream that never ends would
// never be refused at all. Every store refuses such a value anyway
// ([secrets.CheckValue]); this is where the command that read it stops.
func readPipedValue(r io.Reader) (string, error) {
	const most = secrets.MaxValueBytes + 1
	body, err := io.ReadAll(io.LimitReader(r, most+1))
	if err != nil {
		return "", err
	}
	if len(body) > most {
		return "", fmt.Errorf("%w: more than %d bytes arrived, so the rest was "+
			"not read; a value is at most %d (%d KiB)", secrets.ErrValueTooLarge,
			most, secrets.MaxValueBytes, secrets.MaxValueBytes>>10)
	}
	return string(body), nil
}

// nonEmpty wraps a peeled positional as a slice, empty when there was none.
func nonEmpty(s string) []string {
	if s == "" {
		return nil
	}
	return []string{s}
}
