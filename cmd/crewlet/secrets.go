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
	sv, closeStore, err := openSecretStore(ctx, *bootstrapPath)
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
			*bootstrapPath, stdout, stderr)
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

// openSecretStore opens the node's store under its Tier A keyring.
//
// ONE NODE'S rows. There is no fleet secret store: a value set here reaches
// the engine that runs on this `crewlet.yaml`'s database and no peer, so a
// fleet rotation is this command once per node — or the value in each node's
// process environment, which every resolver falls back to. Said in the CLI's
// own output after each write, because the failure otherwise is a credential
// that works on the node an operator tested and nowhere else.
func openSecretStore(ctx context.Context, bootstrapPath string) (*store.SecretValues, func(), error) {
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
	sv, closeStore, err := openSecretValues(ctx, boot)
	if err != nil {
		return nil, nil, engineHasTheStore(err, bootstrapPath)
	}
	return sv, closeStore, nil
}

// engineHasTheStore turns the store's lock refusal into a remediation.
//
// [store.ErrLocked] already says which file and which pid. What it cannot say
// is what an operator of THIS command should do about it, because the store
// does not know it was opened by `crewlet secrets` rather than by a second
// engine. So the sentinel is caught here and answered in this command's own
// terms.
//
// A REFUSAL RATHER THAN A WARNING, which is the whole change: before the lock
// existed this command printed a caution and opened the file anyway, so an
// operator who did not read it corrupted the database. Now it cannot.
func engineHasTheStore(err error, bootstrapPath string) error {
	if !errors.Is(err, store.ErrLocked) {
		return err
	}
	return fmt.Errorf("%w\n\nthe engine for %s is running and holds its "+
		"database; the driver allows only one process on a file. Either stop "+
		"`crewlet run` on this node and re-run this, or supply the value "+
		"through the process environment instead — the resolver falls back to "+
		"it, so a rotation needs no downtime that way", err, bootstrapPath)
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
		Driver:       store.Driver(boot.Store.Driver),
		MaxOpenConns: boot.Store.MaxOpenConns,
		BusyTimeout: time.Duration(
			boot.Store.BusyTimeoutSeconds * float64(time.Second)),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("open store: %w", err)
	}
	return db.SecretValues(cipher), func() { _ = db.Close() }, nil
}

func listSecrets(ctx context.Context, sv *store.SecretValues, stdout io.Writer) error {
	rows, err := sv.List(ctx)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Fprintln(stdout, "no secrets are stored")
		return nil
	}
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

func setSecret(ctx context.Context, sv *store.SecretValues, name, value string,
	valueGiven bool, source, bootstrapPath string, stdout, stderr io.Writer,
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
	// SAID EVERY TIME, because the failure is silent otherwise: a rotation
	// that works on the node an operator tested and nowhere else. The store
	// is this node's database; the activation epoch propagates, the value
	// does not.
	fmt.Fprintf(stdout, "this is %s's own store — on a fleet, run this once per node\n",
		bootstrapPath)
	return nil
}

// getSecret is the ONLY read-back, and it is break-glass.
//
// There is no HTTP route that returns a secret value, by design, and this
// refuses without an explicit flag for the same reason: the overwhelmingly
// common need is "is X set and when did it change", which the listing
// answers without putting a credential into a terminal, a scrollback buffer
// and a screen-share.
//
// The access is LOGGED BY NAME. A break-glass read that leaves no trace is
// indistinguishable from an exfiltration, and the name is the whole of what
// can be logged — logging the value would be the leak.
func getSecret(ctx context.Context, sv *store.SecretValues, name string, reveal bool, stdout io.Writer) error {
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

func unsetSecret(ctx context.Context, sv *store.SecretValues, name string, stdout io.Writer) error {
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

func rekeySecrets(ctx context.Context, sv *store.SecretValues, bootstrapPath string,
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
