package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/crewlet/crewlet/internal/config"
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
// them. Which of the two is in play is not a guess — the store's file lock
// makes "the engine holds this database" an answer with a pid on it — and
// the command says which one it used after every write.
//
// # Every command here needs the keyring, and the keyring is Tier A
//
// The store holds only ciphertext. The key material lives in the bootstrap
// config — on disk or in the environment, never in the database it opens —
// which is why these commands read Tier A and not the company document.

const secretsUsage = `crewlet secrets — read and rotate the encrypted secret store

Usage:
  crewlet secrets list                     What is set, and when it was last written
  crewlet secrets set NAME [-value V]      Store or rotate one secret (reads stdin without -value)
  crewlet secrets get NAME -reveal         Print one secret to stdout (break-glass; logged)
  crewlet secrets unset NAME               Remove one secret
  crewlet secrets keygen [-key-id ID]      A fresh keyring key, with the config snippet to install it
  crewlet secrets rekey [-dry-run]         Re-seal every row under the keyring's active key

Flags:
  -config PATH   Tier A config carrying the keyring (default %q)
  -api URL       The running node to write through; default is its own api.host:port

A running engine holds its database, so these commands go through its
authenticated /secrets surface — which is what puts a value on every node.
Export CREWLET_API_TOKEN to authenticate as a specific operator; without it
the first api.auth.tokens entry in the Tier A config is used.
`

func runSecrets(args []string, stdout, stderr io.Writer) error {
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
	source := fs.String("source", "cli",
		"provenance recorded with the row (set only)")
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

	ctx := context.Background()
	sv, closeStore, err := openSecretStore(ctx, *bootstrapPath, *apiURL)
	if err != nil {
		return err
	}
	defer closeStore()

	// The trailing form — `secrets get -config path TOKEN` — leaves the
	// name here instead. Anything beyond one is an error rather than a
	// silently ignored argument.
	tail := fs.Args()
	if name == "" && len(tail) == 1 {
		name, tail = tail[0], nil
	}
	if len(tail) > 0 {
		return fmt.Errorf("secrets %s takes one name, got %d", sub, len(tail)+1)
	}

	switch sub {
	case "list":
		return listSecrets(ctx, sv, stdout)
	case "set":
		return setSecret(ctx, sv, name, *value, isFlagSet(fs, "value"), *source,
			stdout, stderr)
	case "get":
		return getSecret(ctx, sv, name, *reveal, stdout)
	case "unset":
		return unsetSecret(ctx, sv, name, stdout)
	case "rekey":
		return rekeySecrets(ctx, sv, *bootstrapPath, *dryRun, stdout)
	default:
		fmt.Fprintf(stderr, secretsUsage, defaultBootstrapPath)
		return fmt.Errorf("unknown secrets command %q", sub)
	}
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
	Set(ctx context.Context, name, value, by, source string, now time.Time) error
	Get(ctx context.Context, name string) (string, error)
	Unset(ctx context.Context, name string) (bool, error)
	Rekey(ctx context.Context, activeKeyID, by string, now time.Time) ([]string, error)
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
}

// openSecretStore picks the store this invocation can actually write.
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
// up" with a pid attached, and a successful open means it is not. Probing the
// API instead would confuse "the node is stopped" with "the node is up but
// its HTTP port is bound elsewhere", and those need opposite answers.
func openSecretStore(ctx context.Context, bootstrapPath, apiURL string) (*secretTarget, func(), error) {
	boot, err := loadBootstrapForStore(bootstrapPath)
	if err != nil {
		return nil, nil, err
	}
	if len(boot.Secrets.Keys) == 0 {
		return nil, nil, fmt.Errorf(
			"%s declares no secrets.keys, so there is no keyring to open the "+
				"store with; run `crewlet secrets keygen` and add one",
			bootstrapPath)
	}
	// AN EXPLICIT -api SKIPS THE PROBE ENTIRELY. Naming a node is an
	// instruction to write through it, and it is also how this command
	// works from a machine that is not the node at all — where the local
	// database in the Tier A file does not exist and opening it would
	// create an empty one nothing ever reads.
	if strings.TrimSpace(apiURL) != "" {
		client, cerr := newSecretsClient(boot, apiURL)
		if cerr != nil {
			return nil, nil, cerr
		}
		return &secretTarget{secretBackend: client, fleet: true,
			where: client.Describe()}, func() {}, nil
	}

	sv, closeStore, err := openSecretValues(ctx, boot)
	if err == nil {
		return &secretTarget{
			secretBackend: sv, fleet: false,
			where: boot.Store.Path + " — this node's own rows, which no peer " +
				"can see until the engine migrates them at its next start",
		}, closeStore, nil
	}
	target, rerr := throughTheRunningNode(boot, bootstrapPath, err)
	if rerr != nil {
		return nil, nil, rerr
	}
	return target, func() {}, nil
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
func throughTheRunningNode(boot *config.Bootstrap, bootstrapPath string, err error) (*secretTarget, error) {
	if !errors.Is(err, store.ErrLocked) {
		return nil, err
	}
	client, cerr := newSecretsClient(boot, "")
	if cerr != nil {
		// BOTH FACTS, because either alone is misleading. "The engine
		// holds the database" without "and here is why I could not go
		// through its API" reads as a refusal with no way forward, and
		// the API's own complaint without the lock reads as though the
		// local store were never an option.
		return nil, fmt.Errorf("%w\n\nthe engine for %s is running and holds "+
			"its database, so this has to go through its API — and it cannot: "+
			"%w\n\nEither stop `crewlet run` on this node and re-run, or "+
			"supply the value through the process environment instead: the "+
			"resolver falls back to it, so a rotation needs no downtime that way",
			err, bootstrapPath, cerr)
	}
	return &secretTarget{secretBackend: client, fleet: true,
		where: client.Describe()}, nil
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

// openSecretValues opens the store a loaded bootstrap names, under its
// keyring. The caller has already established that a keyring exists.
func openSecretValues(ctx context.Context, boot *config.Bootstrap) (*store.SecretValues, func(), error) {
	cipher, err := boot.Secrets.Cipher()
	if err != nil {
		return nil, nil, fmt.Errorf("secrets keyring: %w", err)
	}
	// NO EMBEDDING WIDTH, which is deliberate: opening the store for a
	// secret read must not depend on the company document, and the vector
	// columns are only sized when a migration actually runs — which a
	// running node has already done.
	db, err := store.Open(ctx, boot.Store.Path, store.Options{
		MaxOpenConns: boot.Store.MaxOpenConns,
		BusyTimeout:  boot.Store.BusyTimeout(),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("open store: %w", err)
	}
	return db.SecretValues(cipher), func() { _ = db.Close() }, nil
}

func listSecrets(ctx context.Context, sv *secretTarget, stdout io.Writer) error {
	rows, err := sv.List(ctx)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Fprintf(stdout, "no secrets are stored in %s\n", sv.where)
		return nil
	}
	// WHICH STORE, above the table. The two hold different rows, and a
	// listing that did not say which one it read is one an operator can
	// misread as "the fleet has nothing" when they are looking at a
	// stopped node's own empty table.
	fmt.Fprintf(stdout, "%s\n\n", sv.where)
	// NO VALUES, ever. This is what an operator reads to answer "is X set",
	// and a listing that printed plaintext would put a company's whole
	// credential set on one screen — and into one scrollback buffer.
	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tKEY\tUPDATED\tBY\tSOURCE")
	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", r.Name, r.KeyID,
			r.UpdatedAt.Format(time.RFC3339), r.UpdatedBy, r.Source)
	}
	return w.Flush()
}

func setSecret(ctx context.Context, sv *secretTarget, name, value string,
	valueGiven bool, source string, stdout, stderr io.Writer,
) error {
	if name == "" {
		return errors.New("secrets set needs a name")
	}
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
	who := currentOperator()
	if err := sv.Set(ctx, name, value, who, source, time.Now().UTC()); err != nil {
		return err
	}
	// The NAME and nothing else. Confirming the value would undo the whole
	// reason it was read from stdin.
	fmt.Fprintf(stdout, "stored %s (%d bytes) as %s\n", name, len(value), who)
	// SAID EVERY TIME, because the difference between the two stores is
	// invisible afterwards and decides whether a fleet has the value: an
	// operator who cannot tell which one they wrote finds out when a vendor
	// rejects a credential on the one node that never got it.
	fmt.Fprintf(stdout, "written to %s\n", sv.where)
	if !sv.fleet {
		fmt.Fprintln(stdout, secretsLocalNote)
	}
	return nil
}

// secretsLocalNote is what a write to the node-local table did and did not do.
//
// Said out loud rather than left to be discovered, for the same reason
// `config import` says it: an operator who wrote a value while the engine was
// stopped and saw nothing propagate would reasonably conclude the write
// failed, and the fix — start the node, or point -api at one that is up — is
// not guessable.
const secretsLocalNote = "This node will put it on the fleet at its next start. " +
	"To reach a RUNNING fleet now, re-run against a node that is up."

// getSecret is the ONLY read-back, and it is break-glass.
//
// It refuses without an explicit flag because the overwhelmingly common need
// is "is X set and when did it change", which the listing answers without
// putting a credential into a terminal, a scrollback buffer and a
// screen-share. The one HTTP route that returns a value is gated the same
// way, by a ?reveal=true a crawl cannot reach by accident.
//
// The access is LOGGED BY NAME, here and — when this goes through a running
// node — there too, against the operator its own guard authenticated. A
// break-glass read that leaves no trace is indistinguishable from an
// exfiltration, and the name is the whole of what can be logged: logging the
// value would be the leak.
func getSecret(ctx context.Context, sv *secretTarget, name string, reveal bool, stdout io.Writer) error {
	if name == "" {
		return errors.New("secrets get needs a name")
	}
	if !reveal {
		return fmt.Errorf(
			"refusing to print %s: pass -reveal to read a secret back, and "+
				"note the access is logged. `crewlet secrets list` shows "+
				"whether it is set without revealing it", name)
	}
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
	if name == "" {
		return errors.New("secrets unset needs a name")
	}
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

func rekeySecrets(ctx context.Context, sv *secretTarget, bootstrapPath string,
	dryRun bool, stdout io.Writer,
) error {
	boot, err := config.LoadBootstrap(bootstrapPath, config.EnvOnly())
	if err != nil {
		return err
	}
	if dryRun {
		// LISTED FROM THE KEY ID COLUMN, which is denormalised out of the
		// envelope for exactly this: reporting what a rekey would touch
		// must not decrypt anything, or the dry run is a bigger exposure
		// than the pass it is previewing.
		//nolint:govet // shadow: scoped to this block; see .golangci.yml
		rows, err := sv.List(ctx)
		if err != nil {
			return err
		}
		stale := 0
		for _, r := range rows {
			if r.KeyID == boot.Secrets.ActiveKeyID {
				continue
			}
			stale++
			fmt.Fprintf(stdout, "  %s (sealed under %s)\n", r.Name, r.KeyID)
		}
		if stale == 0 {
			fmt.Fprintf(stdout, "every secret is already sealed under %s\n",
				boot.Secrets.ActiveKeyID)
			return nil
		}
		fmt.Fprintf(stdout, "%d secrets would be re-sealed under %s\n",
			stale, boot.Secrets.ActiveKeyID)
		return nil
	}
	moved, err := sv.Rekey(ctx, boot.Secrets.ActiveKeyID, currentOperator(), time.Now().UTC())
	if err != nil {
		return err
	}
	if len(moved) == 0 {
		fmt.Fprintf(stdout, "every secret is already sealed under %s\n",
			boot.Secrets.ActiveKeyID)
		return nil
	}
	// THE NAMES, not a count: a pass that moved 12 of 13 rows raises a
	// question a number cannot answer, and this is the last chance to see
	// which rows are now safe to retire the old key over.
	fmt.Fprintf(stdout, "re-sealed %d secrets under %s:\n", len(moved), boot.Secrets.ActiveKeyID)
	for _, name := range moved {
		fmt.Fprintf(stdout, "  %s\n", name)
	}
	return nil
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
func keyEnvVar(keyID string) string {
	upper := strings.ToUpper(keyID)
	upper = strings.ReplaceAll(upper, "-", "_")
	upper = strings.ReplaceAll(upper, ".", "_")
	return "CREWLET_SECRET_" + upper
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
func secretFromReader(r io.Reader) (string, error) {
	body, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(string(body), "\n"), nil
}

// nonEmpty wraps a peeled positional as a slice, empty when there was none.
func nonEmpty(s string) []string {
	if s == "" {
		return nil
	}
	return []string{s}
}
