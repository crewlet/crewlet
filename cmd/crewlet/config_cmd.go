package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"text/tabwriter"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/chart"
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
  crewlet config activate ID       Mark a revision active here; published at the next start
  crewlet config seal              Encrypt a plaintext active revision under the keyring
  crewlet config rekey [-dry-run]  Re-seal the active revision under the active key
  crewlet config scrub [ID] [-dry-run]
                                   Erase personal data from superseded revisions

Flags:
  -config PATH   Tier A config naming the store and its keyring (default %q)
  -dry-run       Report what a rekey or a scrub would do without writing

A keyring rotation needs BOTH halves: "crewlet config rekey" moves the company
document and "crewlet secrets rekey" moves the secret store. Run both before
dropping a retired key from secrets.keys, or whatever is still sealed under it
becomes unreadable.
`

// configSubcommands is every `crewlet config` subcommand. It is what the
// guard in [runConfig] checks a name against before any flag is registered,
// and the dispatch switch at the bottom of that function must name exactly
// these — TestEveryConfigSubcommandIsDispatchedAndDocumented asserts both
// directions, because the two lists are three screens apart and nothing else
// connects them.
var configSubcommands = []string{
	"import", "show", "export", "revisions", "diff", "activate", "seal", "rekey",
	"scrub",
}

// defaultRevisionLimit is how many revisions `crewlet config revisions` lists.
//
// DELIBERATELY NOT the API's 50 (`GET /config/revisions`, whose default is
// sized for a client that pages and renders its own list). This output is a
// tabwriter table a person reads in a terminal, and 20 rows leaves the header
// and the active-revision marker on screen together on a standard 24-line
// window. An operator who wants the whole history says `-limit`.
const defaultRevisionLimit = 20

func runConfig(args []string, stdout, stderr io.Writer) error {
	sub, rest := splitSubject(args)
	if sub == "" || sub == "help" {
		fmt.Fprintf(stdout, configUsage, defaultBootstrapPath)
		return flag.ErrHelp
	}
	// REFUSED BEFORE ANY FLAG IS REGISTERED, so an unknown subcommand is
	// reported as one. The sets below are per-subcommand, so parsing
	// `config nonesuch -limit 5` against the bare set would answer "flag
	// provided but not defined: -limit" and send the operator looking at
	// the flag rather than at the name they misspelled.
	if !slices.Contains(configSubcommands, sub) {
		fmt.Fprintf(stderr, configUsage, defaultBootstrapPath)
		return fmt.Errorf("unknown config command %q", sub)
	}
	// The subject, for the same reason `secrets` peels one: Go's flag
	// package stops at the first non-flag argument, so `config diff ID
	// -against active` would leave -against unparsed and defaulted.
	subject, rest := splitSubject(rest)

	fs := flag.NewFlagSet("config "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	bootstrapPath := fs.String("config", defaultBootstrapPath,
		"Tier A config: this node's store and its secret keyring")
	// EACH SUBCOMMAND REGISTERS ONLY THE FLAGS IT READS.
	//
	// One shared set is how `crewlet config import company.yaml -dry-run`
	// parsed cleanly and wrote the revision anyway: -dry-run is `rekey`'s,
	// and every other subcommand silently ignored it — an operator asking
	// for no write, being told nothing, and getting one. The same held for
	// -revision, -against, -limit and -redact on every command but their
	// own.
	//
	// Go's flag package refuses a flag it was not given, so registering per
	// subcommand turns each of those silent no-ops into a usage error. It is
	// the rule `run` already follows for a leftover positional: an argument
	// that cannot mean anything here is REFUSED rather than ignored.
	var (
		revision string
		against  = "active"
		limit    = defaultRevisionLimit
		redact   bool
		dryRun   bool
	)
	var (
		apiURL  string
		summary string
	)
	switch sub {
	case "import":
		fs.StringVar(&apiURL, "api", "",
			"import through a running node's API; default is the "+
				"api.host:port in -config when the engine holds the store")
		fs.StringVar(&summary, "summary", "",
			"the audit note recorded with the revision; "+
				"default \"imported from <file>\"")
	case "export":
		fs.StringVar(&revision, "revision", "",
			"which revision to print; default active")
		fs.BoolVar(&redact, "redact", false, "mask secret-shaped values")
	case "revisions":
		fs.IntVar(&limit, "limit", defaultRevisionLimit,
			"how many revisions to list")
	case "diff":
		fs.StringVar(&against, "against", "active",
			"what to compare with: a revision id, or active")
	case "rekey":
		fs.BoolVar(&dryRun, "dry-run", false,
			"report what would be re-sealed without writing")
	case "scrub":
		fs.BoolVar(&dryRun, "dry-run", false,
			"report which revisions hold personal data without erasing it")
	}
	if err := fs.Parse(rest); err != nil {
		return err
	}
	subject, given := onePositional(fs, subject)
	if given > 1 {
		return fmt.Errorf("config %s takes one argument, got %d", sub, given)
	}

	ctx := context.Background()

	// IMPORT CHOOSES ITS OWN TARGET, so it is dispatched before the store
	// is opened: against a running node it must NOT open the store at all,
	// and an explicit -api is an instruction to write through a node that
	// may not even be this machine.
	if sub == "import" {
		return importCompany(ctx, importTarget{
			bootstrapPath: *bootstrapPath,
			path:          subject,
			apiURL:        apiURL,
			summary:       summary,
		}, stdout)
	}

	cs, closeStore, err := openConfigStore(ctx, *bootstrapPath, lockedStoreRemedy(sub))
	if err != nil {
		return err
	}
	defer closeStore()

	switch sub {
	case "show":
		return exportConfig(ctx, cs, "", true, stdout)
	case "export":
		return exportConfig(ctx, cs, firstNonEmpty(subject, revision), redact, stdout)
	case "revisions":
		return listRevisions(ctx, cs, limit, stdout)
	case "diff":
		return diffRevisions(ctx, cs, subject, against, stdout)
	case "activate":
		return activateRevision(ctx, cs, subject, stdout)
	case "seal":
		return sealConfig(ctx, cs, stdout)
	case "rekey":
		return rekeyConfig(ctx, cs, dryRun, stdout)
	case "scrub":
		return scrubConfig(ctx, cs, subject, dryRun, stdout)
	default:
		// Unreachable: the guard above admits only configSubcommands,
		// `import` returned before the store was opened, and a test
		// asserts every name dispatches. It stays because the compiler
		// needs a terminating return and because a name added to the list
		// and not to this switch has to fail loudly.
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

	// staged is where an offline import leaves the chart half for the
	// next boot to publish. See [stageTheChart].
	staged *store.StagedCharts

	// events is where an erasure records that it happened.
	//
	// HELD HERE rather than reopened, because this handle is the one thing
	// in this process that already owns the store file: the driver refuses
	// a second opener, so a command that wanted an audit row and had no
	// handle would have no way to write one.
	events *store.EventLog

	// activeKeyID is the key a fresh seal uses, carried from the same Tier
	// A document the cipher was built from. Held rather than re-read: the
	// two disagreeing is how a rekey comes to report moving a revision
	// onto a key it did not use.
	activeKeyID string
}

// openConfigStore opens this node's store under the keyring Tier A names.
//
// remedy is what the caller's operator should do when a running engine holds
// the file ([engineHoldsTheStore]): every command has its own route around
// the lock, and one sentence for all of them named a route most of them do not
// have.
func openConfigStore(ctx context.Context, bootstrapPath, remedy string) (*configStore, func(), error) {
	boot, err := config.LoadBootstrap(bootstrapPath, config.EnvOnly())
	if err != nil {
		return nil, nil, err
	}
	// THE KEYRING IS NOT OPTIONAL HERE ANY MORE. It used to be — the
	// store's revisions had a plaintext mode for a node with no keyring —
	// and every node holds one now, so a revision this command writes is
	// sealed and one it reads must be (see [secrets.Open]).
	cipher, err := boot.Secrets.Cipher()
	if err != nil {
		return nil, nil, fmt.Errorf("secrets keyring: %w", err)
	}
	db, err := store.Open(ctx, boot.Store.Path, store.Options{
		MaxOpenConns: boot.Store.MaxOpenConns,
		BusyTimeout:  boot.Store.BusyTimeout(),
	})
	if err != nil {
		// A LOCKED STORE HAS A ROUTE AROUND IT, and naming it is the
		// difference between "you are blocked" and "do this instead" — but
		// the route is the caller's to name ([lockedStoreRemedy]).
		return nil, nil, engineHoldsTheStore(fmt.Errorf("open store: %w", err),
			bootstrapPath, remedy)
	}
	return &configStore{
		configs: db.Configs(), cipher: cipher, events: db.Events(),
		staged:      db.StagedCharts(),
		activeKeyID: boot.Secrets.ActiveKeyID,
	}, func() { _ = db.Close() }, nil
}

// importConfig writes a company document as a new active revision.
//
// IDEMPOTENT BY CONTENT, the same rule the boot seed follows: importing an
// unchanged file writes nothing and says so, while an edited one writes
// once. Silently ignoring an edited file would be the worst of the three —
// an operator changes a config, runs the command, and nothing happens.
func importConfig(ctx context.Context, cs *configStore, path string,
	company *config.Company, summary string, stdout io.Writer,
) error {
	// THE SETTINGS HALF, because that is what a revision holds. Storing the
	// whole file would store a revision every node then refuses to apply
	// ([config.DecodeSettings]).
	document, err := json.Marshal(config.SettingsOf(company).Company())
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
		current, err := openStored(cs.cipher, active)
		if err != nil {
			return err
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
		ParentID: parent, Source: "file", CreatedBy: hostActor().Name,
		CreatedByKind: hostActor().Kind, Summary: summary,
		Payload: payload,
	})
	if err != nil {
		return fmt.Errorf("import %s: %w", path, err)
	}
	// THE KEY IT WAS SEALED UNDER, read off the envelope: a `sealed=` flag
	// printed `true` on every import, since nothing is stored unsealed.
	sealedUnder, _ := secrets.EnvelopeKeyIDOf(payload)
	fmt.Fprintf(stdout, "imported %s as revision %s (sealed under %s)\n", path, id, sealedUnder)
	if err := stageTheChart(ctx, cs, path, company, stdout); err != nil {
		return err
	}
	fmt.Fprintln(stdout, importPublishNote)
	return nil
}

// offlinePublish is what an OFFLINE write that marks a revision active can
// and cannot do, and every such note opens with it.
//
// The fleet's activation pointer lives in the coordination store, and on the
// default embedded topology that store is inside the engine's own process —
// so a command run while the engine is stopped genuinely cannot move it.
// What it CAN do is mark the revision active in this node's database, which
// the node publishes at its next boot (see startReconciler).
//
// Said out loud rather than left to be discovered: an operator who imported a
// revision and saw nothing change would reasonably conclude the import
// failed, and the fix — restart, or reach a running node — is not guessable.
//
// WHAT REACHES A RUNNING FLEET IS EACH COMMAND'S OWN, which is why the notes
// below are four rather than one. There was one, and it sent every command to
// `PUT /config` — which refuses a whole company file by name (it carries the
// org chart, a log of its own), activates nothing that is already stored, and
// re-seals nothing.
const offlinePublish = "This node will publish it to the fleet at its next start."

// importPublishNote is an offline import's: the route to a running fleet is
// this same command against a running node, which divides the file between
// the settings and the chart — the one thing `PUT /config` cannot do.
const importPublishNote = offlinePublish + " To make the file the running " +
	"fleet's company without a restart, run `crewlet config import` again " +
	"while a node is up: with the engine holding this store it goes through " +
	"that node's API, and -api names any node."

// activatePublishNote is an offline activation's: a running node activates a
// stored revision by storing its document again as a new one, which is the
// append-only history's way of moving the pointer back.
func activatePublishNote(revisionID string) string {
	return offlinePublish + " To activate it on a running fleet without a " +
		"restart, POST /config/revisions/" + revisionID + "/revert to the node " +
		"holding this store while it is up."
}

// rekeyPublishNote is an offline rekey's: a running node re-seals the active
// document under its own active key whenever it stores it again, which is
// what a reload does.
const rekeyPublishNote = offlinePublish + " On a running fleet, POST " +
	"/config/reload to a node that is up stores the active document again, " +
	"sealed under that node's active key, and activates it."

// lockedStoreRemedy is what a `crewlet config` subcommand tells an operator
// whose engine holds the store: the route to the same answer through a
// running node, where there is one.
//
// `import` is absent because it never asks: it goes through the node's API on
// a locked store by itself ([importCompany]).
func lockedStoreRemedy(sub string) string {
	const stop = "stop `crewlet run` on this node and re-run."
	switch sub {
	case "show", "export":
		return "Read it through the running node instead — GET /config " +
			"serves the active revision with its credentials masked, and " +
			"GET /config/revisions/<id> any other — or " + stop
	case "revisions":
		return "Read them through the running node instead — GET " +
			"/config/revisions — or " + stop
	case "diff":
		return "Compare them through the running node instead — GET " +
			"/config/revisions/<id>/diff?against=<id|active> — or " + stop
	case "activate":
		return "Activate it through the running node instead — POST " +
			"/config/revisions/<id>/revert stores that revision's document " +
			"again and activates it fleet-wide — or " + stop
	case "rekey":
		return "Re-seal through the running node instead — POST " +
			"/config/reload stores the active document again under that " +
			"node's active key — or " + stop
	default:
		// seal and scrub rewrite the store's own rows and have no route
		// through the API: a running node refuses a plaintext revision,
		// and an erasure is this command's alone.
		return "This rewrites the store's own rows and has no route through " +
			"the API: " + stop
	}
}

// exportConfig prints one revision as YAML.
func exportConfig(ctx context.Context, cs *configStore, revisionID string,
	redact bool, stdout io.Writer,
) error {
	rev, company, err := storedCompany(ctx, cs, revisionID)
	if err != nil {
		return err
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
			r.CreatedAt.Format(time.RFC3339),
			describeAuthor(r.CreatedBy, r.OperatorID), r.Source, r.Summary)
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
// The same differ the API's /config/revisions/{id}/diff serves, and for one
// reason: the stored form is JSON produced by marshalling a
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
	// UNREDACTED, and still never printed that way: Changes compares the
	// stored values and reports every value redacted. Handing it redacted
	// documents made a rotated literal credential diff to nothing, since
	// both sides mask to the same marker.
	_, left, err := storedCompany(ctx, cs, revisionID)
	if err != nil {
		return err
	}
	other := against
	if other == "active" {
		other = ""
	}
	_, right, err := storedCompany(ctx, cs, other)
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
//
// EVERY change, however many there are. The API's own answer is cut at
// configapi.MaxChanges because a response body and a socket frame have a
// size budget; a terminal has none, and this is the one reader who can pipe
// the output into a pager, a file or a grep. A diff that stopped at the same
// 500 lines here would hide the rest from exactly the caller equipped to
// read them — and it stopped with a PATHLESS entry every renderer then had
// to recognise as not-a-change.
func writeChanges(stdout io.Writer, from, to string, changes []configapi.Change) error {
	fmt.Fprintf(stdout, "--- %s\n+++ %s\n", from, to)
	for _, c := range changes {
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

// openStored opens a revision this node's store holds, and a refusal says what
// brings it back — which depends on the revision, so the keyring's own refusal
// names nothing.
//
// An UNSEALED revision was written by a build older than the mandatory
// keyring. The ACTIVE one is sealed by `crewlet config seal`, which re-seals
// the active revision and nothing else; a SUPERSEDED one is sealed in place by
// nothing at all, so it can be neither shown nor reverted to, and its document
// comes back only by importing it again from the operator's own copy. A
// revision sealed under a key the keyring does not hold needs that key back.
func openStored(cipher secrets.Cipher, rev store.Revision) ([]byte, error) {
	document, err := secrets.Open(cipher, rev.Payload)
	switch {
	case err == nil:
		return document, nil
	case errors.Is(err, secrets.ErrUnsealedWithKey) && rev.Active:
		return nil, fmt.Errorf("revision %s is this node's active revision, and "+
			"a build older than the mandatory keyring stored it without a seal; "+
			"seal it with `crewlet config seal`, which stores it sealed and "+
			"activates it: %w", rev.ID, err)
	case errors.Is(err, secrets.ErrUnsealedWithKey):
		return nil, fmt.Errorf("revision %s is a superseded revision a build "+
			"older than the mandatory keyring stored without a seal, and nothing "+
			"seals a superseded revision in place, so it can be neither shown nor "+
			"reverted to; to have its document again, import it from your own "+
			"copy with `crewlet config import`, which stores it sealed: %w",
			rev.ID, err)
	default:
		return nil, fmt.Errorf("revision %s does not open under this node's "+
			"keyring; restore the key it was sealed under to secrets.keys: %w",
			rev.ID, err)
	}
}

// storedCompany opens one revision, or the active one, as the stored form.
//
// THE STORED-FORM READER, never the authored one. A revision is JSON a build
// marshalled, possibly a NEWER build whose fields this one does not know, and
// the authored reader refuses an unknown key: `crewlet config diff` used to
// read revisions that way and failed on any revision a newer peer had
// written. It holds the revision to no rule either, because showing,
// exporting and comparing a document never runs it, and a revision this build
// would refuse is exactly the one an operator needs to look at.
func storedCompany(ctx context.Context, cs *configStore, revisionID string) (store.Revision, *config.Company, error) {
	rev, err := revisionOrActive(ctx, cs, revisionID)
	if err != nil {
		return store.Revision{}, nil, err
	}
	document, err := openStored(cs.cipher, rev)
	if err != nil {
		return store.Revision{}, nil, err
	}
	company, err := config.DecodeCompany(document)
	if err != nil {
		return store.Revision{}, nil, fmt.Errorf("parse revision %s: %w", rev.ID, err)
	}
	return rev, company, nil
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
	fmt.Fprintln(stdout, activatePublishNote(revisionID))
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

// importTarget is everything `crewlet config import` needs to decide where the
// revision lands.
type importTarget struct {
	bootstrapPath string
	path          string
	apiURL        string
	summary       string
}

// importCompany writes a company document as a new active revision, through
// whichever route can actually reach the fleet.
//
// # Two routes, and the difference is not cosmetic
//
// The OFFLINE route opens this node's store directly and marks the revision
// active in its own table. It cannot move the fleet's activation pointer,
// because on the default topology that pointer lives inside the engine's own
// process — so it takes effect when this node next starts, and only if this
// node is the one that starts.
//
// The API route is PUT /config on a running node, which stores the revision
// AND activates it, so every node converges with no restart. That is what an
// operator editing a live company wants, and until now the CLI had no way to
// do it: the store is exclusive to one process, so `config import` against a
// running engine simply refused and told them to write the curl themselves.
//
// # Which one it picks
//
// An explicit -api is an instruction and skips the store entirely — it is also
// how this works from a machine that is not the node, where opening the local
// path would create an empty database nothing ever reads. Otherwise it tries
// the store, and falls through to the API only on [store.ErrLocked], which
// means the engine is up, which is precisely when its API is the way in. Every
// other failure — a missing keyring, an unreadable path — is a broken node,
// and answering one of those with HTTP would replace an accurate message with
// a connection refused.
func importCompany(ctx context.Context, t importTarget, stdout io.Writer) error {
	if t.path == "" {
		return errors.New("config import needs a company document to read")
	}
	// VALIDATED HERE, WHICHEVER ROUTE IT TAKES. A revision that cannot be
	// built is one every node in the fleet will refuse, one after another,
	// each reporting its own failure — a fleet-wide incident from a typo
	// that could have been caught before it left this machine.
	company, err := config.LoadCompany(t.path)
	if err != nil {
		return err
	}
	summary := strings.TrimSpace(t.summary)
	if summary == "" {
		summary = "imported from " + t.path
	}

	boot, err := loadBootstrapForStore(t.bootstrapPath)
	if err != nil {
		return err
	}
	if strings.TrimSpace(t.apiURL) != "" {
		return importThroughNode(ctx, boot, t, company, summary, stdout)
	}

	cs, closeStore, err := openConfigStore(ctx, t.bootstrapPath, "")
	if err != nil {
		if !errors.Is(err, store.ErrLocked) {
			return err
		}
		// The engine holds its database, so this is the live case rather
		// than a failure: go through the node that is holding it.
		return importThroughNode(ctx, boot, t, company, summary, stdout)
	}
	defer closeStore()
	return importConfig(ctx, cs, t.path, company, summary, stdout)
}

// importThroughNode divides one file between the two surfaces that own its
// halves, and writes both.
//
// # One file, two estates, and this is the only place that knows
//
// A company file carries the settings and the org chart, and always will: an
// operator authors one document describing a company. The engine keeps them
// apart — a revision is a stored document, a chart is an ordered log — and
// `PUT /config` refuses a body carrying `roles:` or `units:` by name. So
// somebody has to divide the file, and it is this command: the last place
// that holds both halves and knows they arrived together.
//
// # THE SETTINGS TRAVEL AS THE OPERATOR'S OWN BYTES, MINUS TWO KEYS
//
// The file is parsed ONCE into a YAML node tree and the two chart keys are
// DELETED from its root mapping. What is sent is what is left — comments,
// anchors, `${VAR}` pointers and all — rather than a re-encoding of the
// parsed Go value, because Tier B's secrets are pointers stored verbatim and
// a round trip through the types would be a second opinion about a document
// the node is about to form its own.
//
// # THE ORDER IS LOAD-BEARING, AND IT IS STRUCTURE FIRST
//
// The placement lands as ONE record on the chart's structure subject, and
// each object's content follows on its own. That order is not a preference: a
// content write STATES the unit it believes a seat sits in, and the domain
// refuses a value that disagrees with the row — so content written before the
// placement names a unit no row has yet.
//
// The SETTINGS go first of all, because a seat whose model chain names a
// provider is only valid once that provider exists: the other order leaves a
// window in which every seat the chart just created resolves to no model.
func importThroughNode(ctx context.Context, boot *config.Bootstrap, t importTarget,
	company *config.Company, summary string, stdout io.Writer,
) error {
	settings, err := settingsHalfOf(t.path)
	if err != nil {
		return err
	}
	client, err := newConfigClient(boot, t.apiURL)
	if err != nil {
		return err
	}
	id, epoch, err := client.Import(ctx, settings, summary)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "imported %s as revision %s, active on epoch %d\n",
		t.path, id, epoch)
	fmt.Fprintf(stdout, "wrote to %s\n", client.Describe())

	authored := config.AuthoredChart(company)
	if len(authored.Edges()) == 0 {
		// A COMPANY WITH NO UNITS AND NO SEATS is a real authoring state,
		// and importing nothing would write a ledger row saying an empty
		// structure had landed — which the next edited file would then
		// have to be told apart from.
		return nil
	}
	return publishChart(ctx, boot, t, authored, stdout)
}

// publishChart writes the chart half: the structure, then each object.
func publishChart(ctx context.Context, boot *config.Bootstrap, t importTarget,
	authored chart.Authored, stdout io.Writer,
) error {
	client, err := newChartClient(boot, t.apiURL)
	if err != nil {
		return err
	}
	// THE DOMAIN'S OWN KEY, which the boot seed computes the same way: two
	// importers of one file must agree, or each rewrites every row the
	// other already wrote and wakes everybody a second time.
	revision := chart.ImportKey(authored)
	at, err := client.ImportStructure(ctx, revision, authored.Edges())
	if err != nil {
		return fmt.Errorf("publish the org chart from %s: %w", t.path, err)
	}
	fmt.Fprintf(stdout, "published %d unit(s) and %d seat(s) at %s\n",
		len(authored.Units), len(authored.Seats), at)

	// EACH OBJECT'S CONTENT, on its own subject. The import record
	// deliberately carries none: a chart of five hundred seats at this
	// domain's prose bound is megabytes, past the chart log's declared
	// largest record (chart.ChartMaxRecordBytes) — so an import that carried
	// content would be refused on exactly the companies large enough to need
	// it.
	writes := make([]contentWrite, 0, len(authored.Units)+len(authored.Seats))
	for _, unit := range authored.Units {
		writes = append(writes, contentWrite{what: "the unit " + unit.Key,
			write: func(ctx context.Context) error { return client.WriteUnit(ctx, unit) }})
	}
	for _, seat := range authored.Seats {
		writes = append(writes, contentWrite{what: "the seat " + seat.Handle,
			write: func(ctx context.Context) error { return client.WriteSeat(ctx, seat) }})
	}
	if err := writeContent(ctx, writes, chartContentWriters); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "wrote to %s\n", client.Describe())
	return nil
}

// chartContentWriters is how many of an import's content writes are in flight
// at once.
//
// NOT ONE, because each write is answered only once the node has APPLIED it
// (a 200 means the next read there sees it), so one at a time the import paid
// a round trip plus a wait on the node's applier per object, in series — on
// the order of half a minute for sixty seats. The writes are independent:
// every one arbitrates on its own object's subject, and the structure they
// state was placed by the import record before the first of them went out.
//
// ONE DASHBOARD TAB'S QUERY ALLOWANCE, and not more: an import is one caller
// against one node, and internal/store sizes that node's reader pool, at its
// floor, as one tab's stream.MaxInFlightQueries in the sockets' half and the
// same again for everything else. Each write takes one snapshot to decide in,
// so at this bound an import holds no more of the pool than one tab may, and
// leaves the rest for the seats and whoever is watching while it runs. What a
// wider bound would buy is shorter imports of very large companies; what it
// would cost is the engine's own reads and the dashboard queueing behind the
// import on a small host.
const chartContentWriters = stream.MaxInFlightQueries

// contentWrite is one object's content write in an import.
type contentWrite struct {
	what  string
	write func(context.Context) error
}

// writeContent runs writes with at most limit in flight, in the order given,
// and reports the first failure in that order.
//
// A FAILURE STOPS WHAT HAS NOT STARTED and lets what has started finish, which
// is the sequential loop's behaviour widened to limit: nothing is sent after
// the import is known to have failed, and nothing already sent is abandoned
// half-answered. Every write before the failing one was STARTED, since they
// are started in order, so the one reported is the first failure in the file's
// own order however the in-flight writes happened to interleave — the same
// file against the same node reports the same object.
func writeContent(ctx context.Context, writes []contentWrite, limit int) error {
	failures := make([]error, len(writes))
	var (
		wg     sync.WaitGroup
		failed atomic.Bool
	)
	slots := make(chan struct{}, max(limit, 1))
	for i, w := range writes {
		slots <- struct{}{}
		if failed.Load() {
			<-slots
			break
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			if err := w.write(ctx); err != nil {
				failures[i] = fmt.Errorf("write %s: %w", w.what, err)
				failed.Store(true)
			}
		}()
	}
	wg.Wait()
	var first error
	more := 0
	for _, err := range failures {
		switch {
		case err == nil:
		case first == nil:
			first = err
		default:
			more++
		}
	}
	if more > 0 {
		return fmt.Errorf("%w\n\n(%d more content write(s) failed beside it)",
			first, more)
	}
	return first
}

// settingsHalfOf is the file with its two chart keys removed.
//
// THE NODE TREE RATHER THAN THE PARSED VALUE, for the reason
// [importThroughNode] gives: what travels is what the operator wrote. A
// re-encoding would drop every comment, resolve every anchor and re-quote
// every scalar, and the diff an operator reads afterwards would be of a
// document nobody authored.
func settingsHalfOf(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("company config %s: %w", path, err)
	}
	var doc yaml.Node
	if err = yaml.Unmarshal(raw, &doc); err != nil {
		// THE PARSE FAILURE IS THE LOADER'S TO REPORT, with its own line
		// numbers: the caller has already read this file into a company,
		// so reaching here at all means something changed underneath.
		return nil, fmt.Errorf("company config %s: %w", path, err)
	}
	root := &doc
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		root = root.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return raw, nil
	}
	kept := make([]*yaml.Node, 0, len(root.Content))
	// A MAPPING'S CONTENT IS KEY, VALUE, KEY, VALUE, so the step is two.
	for i := 0; i+1 < len(root.Content); i += 2 {
		if slices.Contains(config.ChartKeys(), root.Content[i].Value) {
			continue
		}
		kept = append(kept, root.Content[i], root.Content[i+1])
	}
	root.Content = kept
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return nil, fmt.Errorf("company config %s: %w", path, err)
	}
	return out, nil
}

// stageTheChart leaves the file's org chart for this node's next boot to
// publish.
//
// # Why this command cannot publish it here
//
// A company file carries both halves, and this route runs OFFLINE, against
// the node's own store file with no broker open. The settings are a row it
// can write; the chart is a record on an ordered log, which needs the stream
// this process did not open.
//
// # And why saying so is not enough on its own
//
// It used to say so and stop, which is the right half of the answer and not
// the whole of it. The gesture an operator is performing is "make this file
// the company", and half of it silently did not happen until they went and
// found a second, different command — on a node they had deliberately
// stopped.
//
// So the chart is STAGED: written to this node's own database, sealed with
// the same keyring the revision beside it uses, and published by the next
// boot. Nothing is lost if the node never starts, and nothing is published
// twice if it starts more than once — the import ledger is keyed on the
// chart's own content, so a second landing is a no-op every node reaches the
// same way.
//
// A COMPANY WITH NO CHART STAGES NOTHING, and that is not the same as staging
// an empty one: an operator who wrote providers and no people has not asked
// for every seat to be removed.
func stageTheChart(ctx context.Context, cs *configStore, path string,
	company *config.Company, stdout io.Writer) error {

	if company == nil || !company.CarriesChart() {
		return nil
	}
	authored := config.AuthoredChart(company)
	if len(authored.Edges()) == 0 {
		return nil
	}
	body, err := json.Marshal(authored)
	if err != nil {
		return fmt.Errorf("encode the org chart in %s: %w", path, err)
	}
	// SEALED, for the reason the revision beside it is: an authored chart
	// carries every seat's runtime half, and an offline import has NOT been
	// through the chart writer — which is what turns a literal credential
	// into a sealed reference. A file holding one would otherwise put it in
	// this table in plaintext, where a backup copies it.
	payload, err := secrets.Seal(cs.cipher, body)
	if err != nil {
		return fmt.Errorf("seal the org chart in %s: %w", path, err)
	}
	if err := cs.staged.Stage(ctx, store.StagedChart{
		ID: chart.ImportKey(authored), Payload: payload,
		SourcePath: path, StagedBy: currentOperator(),
	}); err != nil {
		return err
	}
	units, seats := countChart(company)
	fmt.Fprintf(stdout,
		"staged %d unit(s) and %d seat(s) for this node to publish at its next "+
			"start — an org chart is a log of its own and this command has no "+
			"broker open. The chart this company runs is unchanged until then; "+
			"to publish it now, run the same command against a running node "+
			"(`-api`).\n", units, seats)
	return nil
}

// countChart is how many units and seats a file declares, AT ANY DEPTH.
//
// Both walk, because both nest: a company's units are a tree and its seats sit
// at the root and inside any unit of it. Counting only the top level would
// report "1 unit and 1 seat" for a document holding forty of each, which is
// the one number an operator reads to decide whether the note is about
// anything.
func countChart(company *config.Company) (units, seats int) {
	seats = len(company.Roles)
	var walk func([]config.Unit)
	walk = func(in []config.Unit) {
		for i := range in {
			units++
			seats += len(in[i].Roles)
			walk(in[i].Children)
		}
	}
	walk(company.Units)
	return units, seats
}
