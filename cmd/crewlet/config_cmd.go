package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/store"
)

// `crewlet config` — the Tier B company configuration, in the store.
//
// # The store is authoritative at runtime; the file is a seed
//
// Both halves matter. Without the seed a first run has nothing to activate
// and the node serves a company no peer can see; without the store a change
// on one node would be invisible to every other. A running node serves the
// revision the activation pointer names, and these commands operate on that
// pointer and the revisions behind it.
//
// # Revisions are immutable and the pointer is append-only
//
// Nothing here edits a revision. Importing writes a new one; activating
// appends to the pointer. That is what makes re-activating an unchanged
// revision a meaningful gesture — it mints a new epoch every node is
// watching, which is how a rotated secret reaches a running fleet.

const configUsage = `crewlet config — the company configuration in the store

Usage:
  crewlet config import FILE       Write a company document as a new active revision
  crewlet config show              Print the active revision (secrets redacted)
  crewlet config export [-revision ID] [-redact]
                                   Print a revision as YAML
  crewlet config revisions [-limit N]
                                   List revisions, newest first
  crewlet config diff ID [-against ID|active]
                                   Compare two revisions
  crewlet config activate ID       Re-point the fleet at a revision
  crewlet config seal              Encrypt a plaintext active revision under the keyring
  crewlet config rekey [-dry-run]  Re-seal the active revision under the active key

Flags:
  -config PATH   Tier A config naming the store and its keyring (default %q)
  -dry-run       Report what a rekey would do without writing

A keyring rotation needs BOTH halves: "crewlet config rekey" moves the company
document and "crewlet secrets rekey" moves the secret store. Run both before
dropping a retired key from secrets.keys, or whatever is still sealed under it
becomes unreadable.
`

func runConfig(args []string, stdout, stderr io.Writer) error {
	sub, rest := splitSubject(args)
	if sub == "" || sub == "help" {
		fmt.Fprintf(stdout, configUsage, defaultBootstrapPath)
		return flag.ErrHelp
	}
	// The subject, for the same reason `secrets` peels one: Go's flag
	// package stops at the first non-flag argument, so `config diff ID
	// -against active` would leave -against unparsed and defaulted.
	subject, rest := splitSubject(rest)

	fs := flag.NewFlagSet("config "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	bootstrapPath := fs.String("config", defaultBootstrapPath,
		"Tier A config: this node's store and its secret keyring")
	revision := fs.String("revision", "", "which revision (export); default active")
	against := fs.String("against", "active", "what to compare with (diff)")
	limit := fs.Int("limit", 20, "how many revisions to list")
	redact := fs.Bool("redact", false, "mask secret-shaped values (export)")
	dryRun := fs.Bool("dry-run", false,
		"report what would be re-sealed without writing (rekey only)")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	tail := fs.Args()
	if subject == "" && len(tail) == 1 {
		subject, tail = tail[0], nil
	}
	if len(tail) > 0 {
		return fmt.Errorf("config %s takes one argument, got %d", sub, len(tail)+1)
	}

	ctx := context.Background()
	cs, closeStore, err := openConfigStore(ctx, *bootstrapPath)
	if err != nil {
		return err
	}
	defer closeStore()

	switch sub {
	case "import":
		return importConfig(ctx, cs, subject, stdout)
	case "show":
		return exportConfig(ctx, cs, "", true, stdout)
	case "export":
		return exportConfig(ctx, cs, firstNonEmpty(subject, *revision), *redact, stdout)
	case "revisions":
		return listRevisions(ctx, cs, *limit, stdout)
	case "diff":
		return diffRevisions(ctx, cs, subject, *against, stdout)
	case "activate":
		return activateRevision(ctx, cs, subject, stdout)
	case "seal":
		return sealConfig(ctx, cs, *bootstrapPath, stdout)
	case "rekey":
		return rekeyConfig(ctx, cs, *bootstrapPath, *dryRun, stdout)
	default:
		fmt.Fprintf(stderr, configUsage, defaultBootstrapPath)
		return fmt.Errorf("unknown config command %q", sub)
	}
}

// configStore is the store plus the keyring that opens what it holds.
//
// The two travel together because every revision is sealed: a handle to the
// table without the cipher can list ids and read nothing.
type configStore struct {
	configs *store.Configs
	cipher  secrets.Cipher

	// activeKeyID is the key a fresh seal uses, carried from the same Tier
	// A document the cipher was built from. Held rather than re-read: the
	// two disagreeing is how a rekey comes to report moving a revision
	// onto a key it did not use.
	activeKeyID string
}

func openConfigStore(ctx context.Context, bootstrapPath string) (*configStore, func(), error) {
	boot, err := config.LoadBootstrap(bootstrapPath, config.EnvOnly())
	if err != nil {
		return nil, nil, err
	}
	// A NIL CIPHER IS VALID HERE, unlike for `secrets`: company_config
	// supports a plaintext mode so pre-encryption deployments keep working,
	// and secrets.Open passes an unsealed payload straight through.
	var cipher secrets.Cipher
	if len(boot.Secrets.Keys) > 0 {
		if cipher, err = boot.Secrets.Cipher(); err != nil {
			return nil, nil, fmt.Errorf("secrets keyring: %w", err)
		}
	}
	db, err := store.Open(ctx, boot.Store.Path, store.Options{
		Driver:       store.Driver(boot.Store.Driver),
		MaxOpenConns: boot.Store.MaxOpenConns,
		BusyTimeout: time.Duration(
			boot.Store.BusyTimeoutSeconds * float64(time.Second)),
	})
	if err != nil {
		// A LOCKED STORE HAS A ROUTE AROUND IT, and naming it here is the
		// difference between "you are blocked" and "do this instead": the
		// API writes the same revision and activates it on every node,
		// which is what an operator wanted from `config import` anyway.
		return nil, nil, engineHoldsTheStore(fmt.Errorf("open store: %w", err),
			bootstrapPath, "Use the API against the running node instead — "+
				"PUT /config stores the revision AND activates it fleet-wide, "+
				"which this offline path cannot do; or stop `crewlet run` on "+
				"this node and re-run.")
	}
	return &configStore{
		configs: db.Configs(), cipher: cipher,
		activeKeyID: boot.Secrets.ActiveKeyID,
	}, func() { _ = db.Close() }, nil
}

// importConfig writes a company document as a new active revision.
//
// IDEMPOTENT BY CONTENT, the same rule the boot seed follows: importing an
// unchanged file writes nothing and says so, while an edited one writes
// once. Silently ignoring an edited file would be the worst of the three —
// an operator changes a config, runs the command, and nothing happens.
func importConfig(ctx context.Context, cs *configStore, path string, stdout io.Writer) error {
	if path == "" {
		return errors.New("config import needs a company document to read")
	}
	// VALIDATED BEFORE IT IS STORED. A revision that cannot be built is one
	// every node in the fleet will refuse, one after another, each reporting
	// its own failure — which is a fleet-wide incident produced by a typo
	// that could have been caught here.
	company, err := config.LoadCompany(path)
	if err != nil {
		return err
	}
	document, err := json.Marshal(company)
	if err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}

	active, found, err := cs.configs.Active(ctx)
	if err != nil {
		return fmt.Errorf("read the active revision: %w", err)
	}
	parent := ""
	if found {
		//nolint:govet // shadow: scoped to this block; see .golangci.yml
		current, err := secrets.Open(cs.cipher, active.Payload)
		if err != nil {
			return fmt.Errorf("open the active revision: %w", err)
		}
		if bytes.Equal(current, document) {
			fmt.Fprintf(stdout,
				"%s already matches the active revision %s; nothing imported\n",
				path, active.ID)
			return nil
		}
		parent = active.ID
	}

	payload, err := secrets.Seal(cs.cipher, document)
	if err != nil {
		return err
	}
	id, err := cs.configs.InsertActive(ctx, store.Revision{
		ParentID: parent, Source: "file", CreatedBy: currentOperator(),
		Summary: "imported from " + path,
		Payload: payload,
	})
	if err != nil {
		return fmt.Errorf("import %s: %w", path, err)
	}
	fmt.Fprintf(stdout, "imported %s as revision %s (sealed=%t)\n", path, id, cs.cipher != nil)
	fmt.Fprintln(stdout, publishNote)
	return nil
}

// publishNote is what an OFFLINE import or activation can and cannot do.
//
// The fleet's activation pointer lives in the coordination store, and on the
// default embedded topology that store is inside the engine's own process —
// so a command run while the engine is stopped genuinely cannot move it.
// What it CAN do is mark the revision active in this node's database, which
// the node publishes at its next boot (see startReconciler).
//
// Said out loud rather than left to be discovered: an operator who imported a
// revision and saw nothing change would reasonably conclude the import
// failed, and the fix — restart, or use the API — is not guessable.
const publishNote = "This node will publish it to the fleet at its next start. " +
	"To activate it on a RUNNING fleet without a restart, use the API: " +
	"PUT /config to a node that is up."

// exportConfig prints one revision as YAML.
func exportConfig(ctx context.Context, cs *configStore, revisionID string,
	redact bool, stdout io.Writer,
) error {
	rev, err := revisionOrActive(ctx, cs, revisionID)
	if err != nil {
		return err
	}
	document, err := secrets.Open(cs.cipher, rev.Payload)
	if err != nil {
		return fmt.Errorf("open revision %s: %w", rev.ID, err)
	}
	company, err := config.DecodeCompany(document)
	if err != nil {
		return fmt.Errorf("parse revision %s: %w", rev.ID, err)
	}
	if redact {
		// STRUCTURAL, not a regex over the text: it masks the fields the
		// config types declare as secret-bearing, so a new one is masked
		// by declaring it rather than by remembering to add a pattern.
		company = company.Redact()
	}
	body, err := config.EncodeCompanyYAML(company)
	if err != nil {
		return fmt.Errorf("render revision %s: %w", rev.ID, err)
	}
	_, err = stdout.Write(body)
	return err
}

func listRevisions(ctx context.Context, cs *configStore, limit int, stdout io.Writer) error {
	if limit <= 0 {
		return errors.New("config revisions needs a positive limit")
	}
	rows, err := cs.configs.List(ctx, limit, 0)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Fprintln(stdout, "no revisions are stored; run `crewlet config import`")
		return nil
	}
	active, found, err := cs.configs.Active(ctx)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "\tREVISION\tCREATED\tBY\tSOURCE\tSUMMARY")
	for _, r := range rows {
		marker := " "
		if found && r.ID == active.ID {
			// THE ACTIVE ONE IS MARKED, because "which is running" is the
			// question this list is opened to answer and an id alone
			// cannot answer it.
			marker = "*"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", marker, r.ID,
			r.CreatedAt.Format(time.RFC3339), r.CreatedBy, r.Source, r.Summary)
	}
	return w.Flush()
}

// diffRevisions compares two revisions, REDACTED on both sides.
//
// Always redacted, with no flag to turn it off: a diff is what an operator
// pastes into a ticket or a chat thread to ask a colleague whether a change
// looks right, and that is the single most likely way a credential leaves
// the machine. `export -revision ID` is there for the rare case that needs
// the real values, and it takes a deliberate act.
//
// # Paths and values, not lines
//
// The same differ the API's /config/revisions/{id}/diff serves, and for the
// reason d-505 records: the stored form is JSON produced by marshalling a
// struct, so re-ordering a map or adding a field with a default rewrites
// lines that mean nothing. What an operator asks is "what changed about the
// company", and that is answered by paths and values.
//
// The CLI rendered a unified line diff over YAML instead — the shape the
// project had recorded a decision against, while the same binary's HTTP
// surface answered structurally. One product cannot ship both answers to one
// question.
func diffRevisions(ctx context.Context, cs *configStore, revisionID, against string,
	stdout io.Writer,
) error {
	if revisionID == "" {
		return errors.New("config diff needs a revision to compare")
	}
	left, err := redactedCompany(ctx, cs, revisionID)
	if err != nil {
		return err
	}
	other := against
	if other == "active" {
		other = ""
	}
	right, err := redactedCompany(ctx, cs, other)
	if err != nil {
		return err
	}
	// OLDEST FIRST: the diff reads as "what `against` became", so a change
	// rendered the other way round would name the new value as the old.
	changes, err := configapi.Changes(right, left)
	if err != nil {
		return err
	}
	if len(changes) == 0 {
		fmt.Fprintf(stdout, "%s and %s are identical\n", revisionID, against)
		return nil
	}
	return writeChanges(stdout, against, revisionID, changes)
}

// writeChanges renders a structural diff.
//
// ONE LINE PER PATH, with the marker in the first column so the shape is
// scannable the way a line diff was — an operator reading this in a terminal
// is looking for "what moved", and a block per change buries that under
// formatting.
func writeChanges(stdout io.Writer, from, to string, changes []configapi.Change) error {
	fmt.Fprintf(stdout, "--- %s\n+++ %s\n", from, to)
	for _, c := range changes {
		if c.Path == "" {
			// The truncation marker Changes appends when a diff exceeds
			// its cap. Reported rather than silent: a diff that quietly
			// stopped would be read as "that is all that changed".
			fmt.Fprintf(stdout, "... %v\n", c.To)
			continue
		}
		switch c.Kind {
		case configapi.KindAdded:
			fmt.Fprintf(stdout, "+ %s = %s\n", c.Path, renderValue(c.To))
		case configapi.KindRemoved:
			fmt.Fprintf(stdout, "- %s (was %s)\n", c.Path, renderValue(c.From))
		default:
			fmt.Fprintf(stdout, "~ %s: %s -> %s\n",
				c.Path, renderValue(c.From), renderValue(c.To))
		}
	}
	return nil
}

// renderValue prints a leaf as an operator reads it.
//
// A STRING IS QUOTED and everything else is not, which is the one distinction
// that matters here: `"true"` and `true` are different settings, and a
// renderer that printed both as `true` would show a type change as no change
// at all.
func renderValue(v any) string {
	switch value := v.(type) {
	case nil:
		return "null"
	case string:
		return strconv.Quote(value)
	default:
		return fmt.Sprint(value)
	}
}

// redactedCompany opens a revision and redacts it, for comparison.
func redactedCompany(ctx context.Context, cs *configStore, revisionID string) (*config.Company, error) {
	rev, err := revisionOrActive(ctx, cs, revisionID)
	if err != nil {
		return nil, err
	}
	document, err := secrets.Open(cs.cipher, rev.Payload)
	if err != nil {
		return nil, err
	}
	company, err := config.ParseCompanyDocument(document)
	if err != nil {
		return nil, err
	}
	return company.Redact(), nil
}

func activateRevision(ctx context.Context, cs *configStore, revisionID string, stdout io.Writer) error {
	if revisionID == "" {
		return errors.New("config activate needs a revision id")
	}
	if _, found, err := cs.configs.Get(ctx, revisionID); err != nil {
		return err
	} else if !found {
		return fmt.Errorf("no revision %s", revisionID)
	}
	if _, err := cs.configs.Activate(ctx, revisionID, time.Now().UTC()); err != nil {
		return err
	}
	// RE-ACTIVATING THE CURRENT REVISION IS NOT A NO-OP, and that is worth
	// keeping in mind here: publishing mints a new epoch every node is
	// watching, which is how a rotated secret reaches a running fleet
	// without a restart.
	fmt.Fprintf(stdout, "marked %s active on this node\n", revisionID)
	fmt.Fprintln(stdout, publishNote)
	return nil
}

// revisionOrActive resolves an id, or the active revision for an empty one.
func revisionOrActive(ctx context.Context, cs *configStore, revisionID string) (store.Revision, error) {
	if revisionID == "" {
		rev, found, err := cs.configs.Active(ctx)
		if err != nil {
			return store.Revision{}, err
		}
		if !found {
			return store.Revision{}, errors.New(
				"no revision is active; run `crewlet config import`")
		}
		return rev, nil
	}
	rev, found, err := cs.configs.Get(ctx, revisionID)
	if err != nil {
		return store.Revision{}, err
	}
	if !found {
		return store.Revision{}, fmt.Errorf("no revision %s", revisionID)
	}
	return rev, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
