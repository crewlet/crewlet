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
// on one node would be invisible to every other. The company is the revision
// the fleet's activation pointer names, and a running node applies it.
//
// # Offline, a command chooses what this node boots on
//
// These commands run with the engine stopped, and the pointer lives in the
// coordination store, which they do not open. So what `import`, `activate` and
// `rekey` write is an OFFER: this node boots on it at its next start and
// publishes it to the fleet only while the fleet still runs the revision it
// was built on — the company as this node last knew it
// ([store.Configs.Restorable]) — so a change made on a node that was behind is
// never published over what the fleet moved on to (bootpublish.go). Through a
// running node (`import -api`, or the API), a write is the fleet's at once.
//
// # Revisions are immutable and the pointer is append-only
//
// Nothing here edits a revision. Importing writes a new one; activating
// appends to the pointer. That is what makes re-activating an unchanged
// revision a meaningful gesture — it mints a new epoch every node is
// watching, which is how a rotated secret reaches a running fleet.

const configUsage = `crewlet config: the company configuration in the store

Usage:
  crewlet config import FILE       Write a company document as a new revision to boot on
  crewlet config show              Print the company this node knows (secrets redacted)
  crewlet config export [-revision ID] [-redact]
                                   Print a revision as YAML
  crewlet config revisions [-limit N]
                                   List revisions, newest first
  crewlet config diff ID [-against ID|active]
                                   Compare two revisions
  crewlet config activate ID       Boot on a stored revision here, offered at the next start
  crewlet config rekey [-dry-run]  Re-seal the revision this node boots on under the active key

Flags:
  -config PATH   Tier A config naming the store and its keyring (default %q)
  -dry-run       Report what a rekey would do without writing

Offline, import, activate and rekey choose what this node boots on: it starts
on that revision and publishes it to the fleet only while the fleet still runs
the revision the change was built on. "active" names the company this node
knows: the fleet's revision as it last saw it, or an offline change built on it.
A keyring rotation needs BOTH halves: "crewlet config rekey" moves the company
document and "crewlet secrets rekey" moves the secret store. Run both before
dropping a retired key from secrets.keys, or whatever is still sealed under it
becomes unreadable.
An import never refuses a company file for its charter (a mission or policy
past its length, or on two lines): it prints each fault as a warning and
imports, offline or through a running node, which it reaches as an admin's
import (PUT /config?import=true) rather than as an edit.
`

// configSubcommands is every `crewlet config` subcommand. It is what the
// guard in [runConfig] checks a name against before any flag is registered,
// and the dispatch switch at the bottom of that function must name exactly
// these — TestEveryConfigSubcommandIsDispatchedAndDocumented asserts both
// directions, because the two lists are three screens apart and nothing else
// connects them.
var configSubcommands = []string{
	"import", "show", "export", "revisions", "diff", "activate", "rekey",
}

// defaultRevisionLimit is how many revisions `crewlet config revisions` lists.
//
// DELIBERATELY NOT the API's 50 (`GET /config/revisions`, whose default is
// sized for a client that pages and renders its own list). This output is a
// tabwriter table a person reads in a terminal, and 20 rows leaves the header
// and the STATE column's marks on screen together on a standard 24-line
// window. An operator who wants the whole history says `-limit`.
const defaultRevisionLimit = 20

func runConfig(environ config.Source, args []string, stdout, stderr io.Writer) error {
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
		apiURL  = new(string)
		summary string
	)
	switch sub {
	case "import":
		apiURL = nodeAPIFlag(fs,
			"import through a running node's API; default is the "+
				"api.host:port in -config when the engine holds the store")
		fs.StringVar(&summary, "summary", "",
			"the audit note recorded with the revision; "+
				"default \"imported from <file>\"")
	case "export":
		fs.StringVar(&revision, "revision", "",
			"which revision to print; default the company this node knows")
		fs.BoolVar(&redact, "redact", false, "mask secret-shaped values")
	case "revisions":
		fs.IntVar(&limit, "limit", defaultRevisionLimit,
			"how many revisions to list")
	case "diff":
		fs.StringVar(&against, "against", "active",
			"what to compare with: a revision id, or active for the company this node knows")
	case "rekey":
		fs.BoolVar(&dryRun, "dry-run", false,
			"report what would be re-sealed without writing")
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
			apiURL:        *apiURL,
			summary:       summary,
			environ:       environ,
		}, stdout)
	}

	cs, closeStore, err := openConfigStore(ctx, *bootstrapPath, lockedStoreRemedy(sub))
	if err != nil {
		return err
	}
	defer closeStore()

	switch sub {
	case "show":
		return exportConfig(ctx, cs, "", true, stdout, stderr)
	case "export":
		return exportConfig(ctx, cs, firstNonEmpty(subject, revision), redact, stdout, stderr)
	case "revisions":
		return listRevisions(ctx, cs, limit, stdout)
	case "diff":
		return diffRevisions(ctx, cs, subject, against, stdout)
	case "activate":
		return activateRevision(ctx, cs, subject, stdout)
	case "rekey":
		return rekeyConfig(ctx, cs, dryRun, stdout)
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

	// activeKeyID is the key a fresh seal uses, carried from the same Tier
	// A document the cipher was built from. Held rather than re-read: the
	// two disagreeing is how a rekey comes to report moving a revision
	// onto a key it did not use.
	activeKeyID string

	// boot is that Tier A document, for the one more thing a command asks
	// of it: whether the company document is managed ([refuseManagedOffline]).
	boot *config.Bootstrap
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
	if err = refuseScratchStore(boot, "crewlet config",
		"Use the API of a node that holds data: PUT /config stores the revision "+
			"and activates it on every node, this one included."); err != nil {
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
	// THE NODE'S OWN FILE ALONE: revisions are the node's own, and this
	// command reads nothing a state log applies, so it opens no replicated
	// estate.
	db, err := store.OpenNode(ctx, boot.Store.Path, store.Options{
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
		configs: db.Configs(), cipher: cipher,
		activeKeyID: boot.Secrets.ActiveKeyID, boot: boot,
	}, func() { _ = db.Close() }, nil
}

// importConfig writes a company document as a new revision this node boots on,
// offered to the fleet over the company this node knows ([offlineBase]).
//
// IDEMPOTENT BY CONTENT, the same rule the boot seed follows: importing an
// unchanged file writes nothing and says so, while an edited one writes
// once. Silently ignoring an edited file would be the worst of the three —
// an operator changes a config, runs the command, and nothing happens.
func importConfig(ctx context.Context, cs *configStore, path string,
	company *config.Company, summary string, stdout io.Writer,
) error {
	document, err := json.Marshal(company)
	if err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}

	known, over, found, err := offlineBase(ctx, cs)
	if err != nil {
		return err
	}
	if found {
		//nolint:govet // shadow: scoped to this block; see .golangci.yml
		current, err := openStored(cs.cipher, known)
		switch {
		case errors.Is(err, secrets.ErrUnsealedWithKey):
			// AN UNSEALED REVISION IS REPLACED, NEVER COMPARED: this import
			// is the remedy every reader's refusal of one names
			// ([refuseUnsealed]; configapi's UnreadableRevisionError.Remedy
			// names it beside PUT /config), so refusing it here too left a
			// store whose company is plaintext with no way out at all.
			// Nothing is read from it — it is only the new one's parent.
			// PUT /config is the same rule held online, and it is also where
			// this command goes while a node holds the store (configapi's
			// prepare reads nothing from an unsealed base either). A revision
			// SEALED in a form this build does not read is not this:
			// [openStored] refuses it, and the import with it, since it is
			// somebody's document and only the build that sealed it reads it.
		case err != nil:
			return err
		case bytes.Equal(current, document):
			fmt.Fprintf(stdout, "%s already matches revision %s, the company this node "+
				"knows; nothing imported\n", path, known.ID)
			sayWhatBoots(ctx, cs, known.ID, stdout)
			return nil
		}
	}

	payload, err := secrets.Seal(cs.cipher, document)
	if err != nil {
		return err
	}
	id, err := cs.configs.Offer(ctx, over, store.Revision{
		ParentID: known.ID, Source: "file", CreatedBy: hostActor().Name,
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
	fmt.Fprintln(stdout, importPublishNote(over))
	return nil
}

// offlineBase is what an offline change is built on: its parent, the company
// this node knows ([store.Configs.Restorable]), and what the boot publishes it
// over — the fleet's revision that company is, or is built on ([offerOn]).
// found is false on a node that knows no company, whose change is built on
// nothing.
func offlineBase(ctx context.Context, cs *configStore) (store.Revision, store.Offer, bool, error) {
	known, found, err := cs.configs.Restorable(ctx)
	if err != nil {
		return store.Revision{}, store.Offer{}, false,
			fmt.Errorf("read the company this node knows: %w", err)
	}
	if !found {
		return store.Revision{}, store.Offer{}, false, nil
	}
	return known, offerOn(known), true, nil
}

// sayWhatBoots notes that this node boots on another revision than the one a
// command answered about, which is a node that has not applied the company it
// knows — refused it, or not yet reached it.
func sayWhatBoots(ctx context.Context, cs *configStore, about string, out io.Writer) {
	boot, found, err := cs.configs.Boot(ctx)
	if err != nil || !found || boot.ID == about {
		return
	}
	fmt.Fprintf(out, "this node boots on revision %s: it runs that revision until it "+
		"applies %s\n", boot.ID, about)
}

// offlinePublishNote is what an OFFLINE write that chooses what this node boots
// on can and cannot do, and every such note opens with it.
//
// The fleet's activation pointer lives in the coordination store, which a
// command run while the engine is stopped does not open for it: a revision is
// this node's to hold, in its own database, and the node publishes the one it
// boots on at its next boot, over the revision it was built on (bootpublish.go)
// — so the command starts no broker, and needs none. (`crewlet secrets` is
// different: a secret has no home on the node, so that command reaches the
// fleet's store itself.)
//
// Said out loud rather than left to be discovered: an operator who imported a
// revision and saw nothing change would reasonably conclude the import
// failed, and one whose node was behind its fleet would not guess that the
// change is withheld rather than published over what the fleet moved on to.
//
// WHAT REACHES A RUNNING FLEET IS EACH COMMAND'S OWN, which is why the notes
// below add to this one: `PUT /config` stores and activates a document, but it
// activates nothing that is already stored and re-seals nothing.
func offlinePublishNote(over store.Offer) string {
	return "This node boots on it at its next start and publishes it to the fleet " +
		publishedWhile(over) + "; if the fleet has moved on, it runs the fleet's " +
		"revision once it can and says it did not publish this one."
}

// importPublishNote is an offline import's: the route to a running fleet is
// `PUT /config` on a node that is up, which this same command takes while a
// node holds the store.
func importPublishNote(over store.Offer) string {
	return offlinePublishNote(over) + " To make the file the running fleet's " +
		"company without a restart, PUT /config to a node that is up, or run " +
		"`crewlet config import` while a node is up: with the engine holding this " +
		"store it goes through that node's API, and -api names any node."
}

// activatePublishNote is an offline activation's: a running node activates a
// stored revision by storing its document again as a new one, which is the
// append-only history's way of moving the pointer back.
func activatePublishNote(revisionID string, over store.Offer) string {
	return offlinePublishNote(over) + " To activate it on a running fleet without a " +
		"restart, POST /config/revisions/" + revisionID + "/revert to the node " +
		"holding this store while it is up."
}

// rekeyPublishNote is an offline rekey's: a running node re-seals the fleet's
// document under its own active key whenever it stores it again, which is what
// a reload does.
func rekeyPublishNote(over store.Offer) string {
	return offlinePublishNote(over) + " On a running fleet, POST /config/reload " +
		"to a node that is up stores the fleet's document again, sealed under that " +
		"node's active key, and activates it."
}

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
		return "Read it through the running node instead (GET /config " +
			"serves the fleet's revision with its credentials masked, and " +
			"GET /config/revisions/<id> any other), or " + stop
	case "revisions":
		return "Read them through the running node instead (GET " +
			"/config/revisions), or " + stop
	case "diff":
		return "Compare them through the running node instead (GET " +
			"/config/revisions/<id>/diff?against=<id|active>), or " + stop
	case "activate":
		return "Activate it through the running node instead (POST " +
			"/config/revisions/<id>/revert stores that revision's document " +
			"again and activates it fleet-wide), or " + stop
	case "rekey":
		return "Re-seal through the running node instead (POST " +
			"/config/reload stores the fleet's document again under that " +
			"node's active key), or " + stop
	default:
		// Every subcommand that opens the store is named above; a new one
		// still gets the stop, which is always true.
		return "S" + stop[1:]
	}
}

// exportConfig prints one revision as YAML: the one named, or the company this
// node knows, with a note on stderr where this node boots on another.
func exportConfig(ctx context.Context, cs *configStore, revisionID string,
	redact bool, stdout, stderr io.Writer,
) error {
	rev, company, err := storedCompany(ctx, cs, revisionID)
	if err != nil {
		return err
	}
	if revisionID == "" {
		sayWhatBoots(ctx, cs, rev.ID, stderr)
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
	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "REVISION\tSTATE\tCREATED\tBY\tSOURCE\tSUMMARY")
	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", r.ID, revisionState(r),
			r.CreatedAt.Format(time.RFC3339),
			describeAuthor(r.CreatedBy, r.OperatorID), r.Source, r.Summary)
	}
	return w.Flush()
}

// revisionState is what this node knows a revision to be, as the listing's
// STATE column says it.
//
// TWO FACTS, because "which is running" has two answers on a node that refused
// the fleet's revision: what it BOOTS on, and the FLEET's revision as it last
// saw it. One marker could say only one of them, and whichever it said, the
// operator was left to guess the other. An offline change says what it was
// built on, because that is what the next start publishes it over, and the
// unconfirmed offer it replaced where there is one, the one other revision the
// start accepts in its place ([store.Offer.Over]).
func revisionState(r store.Revision) string {
	var state []string
	if r.Boots {
		state = append(state, "boots")
	}
	if r.Fleets {
		state = append(state, "fleet's")
	}
	if r.Offer != nil {
		state = append(state, "offered over "+r.Offer.String())
	}
	if len(state) == 0 {
		return "-"
	}
	return strings.Join(state, ", ")
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
// brings it back ([storedRefusal]).
func openStored(cipher secrets.Cipher, rev store.Revision) ([]byte, error) {
	document, err := secrets.Open(cipher, rev.Payload)
	if err != nil {
		return nil, storedRefusal(rev.ID, err)
	}
	return document, nil
}

// storedRefusal is why a revision this node's store holds does not open, and
// what brings it back, for every command that meets one, a rekey's check of
// its seal included.
//
// An UNSEALED revision is refused: nothing vouches for who wrote it, and its
// document comes back only by importing it again from the operator's own
// copy. A revision SEALED IN A FORM THIS BUILD DOES NOT READ is somebody's
// document, most likely a newer build's, which that build reads and nothing
// here replaces. A revision sealed under a key the keyring does not hold needs
// that key back.
func storedRefusal(revisionID string, err error) error {
	switch {
	case errors.Is(err, secrets.ErrUnsealedWithKey):
		return refuseUnsealed(revisionID)
	case errors.Is(err, secrets.ErrUnknownEnvelope):
		return fmt.Errorf("revision %s is sealed in a form this build of crewlet "+
			"doesn't read, most likely by a newer build, so nothing here reads or "+
			"replaces it; run the build that sealed it: %w", revisionID, err)
	default:
		return fmt.Errorf("revision %s does not open under this node's "+
			"keyring; restore the key it was sealed under to secrets.keys: %w",
			revisionID, err)
	}
}

// refuseUnsealed is the refusal of a revision stored without a seal, whichever
// command met it, and it names the one gesture that brings the document back.
func refuseUnsealed(revisionID string) error {
	return fmt.Errorf("revision %s is stored without a seal, so it is not "+
		"opened; to have its document again, import it from your own copy with "+
		"`crewlet config import`, which stores it sealed: %w",
		revisionID, secrets.ErrUnsealedWithKey)
}

// storedCompany opens one revision, or the company this node knows, as the
// stored form.
//
// THE STORED-FORM READER, never the authored one. A revision is JSON a build
// marshalled, possibly a NEWER build whose fields this one does not know, and
// the authored reader refuses an unknown key: `crewlet config diff` used to
// read revisions that way and failed on any revision a newer peer had
// written. It holds the revision to no rule either, because showing,
// exporting and comparing a document never runs it, and a revision this build
// would refuse is exactly the one an operator needs to look at.
func storedCompany(ctx context.Context, cs *configStore, revisionID string) (store.Revision, *config.Company, error) {
	rev, err := revisionOrKnown(ctx, cs, revisionID)
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

// activateRevision makes a stored revision what this node boots on, offered to
// the fleet over the company this node knows.
//
// HELD TO THE RUNNABLE RULES FIRST, because what this writes is what the next
// start boots on: a revision this build cannot run would refuse that start, on
// a node whose operator asked for nothing but a different document.
func activateRevision(ctx context.Context, cs *configStore, revisionID string, stdout io.Writer) error {
	if revisionID == "" {
		return errors.New("config activate needs a revision id")
	}
	revision, found, err := cs.configs.Get(ctx, revisionID)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("no revision %s", revisionID)
	}
	// RE-POINTING THE FLEET AT ANOTHER REVISION IS A CHANGE OF DOCUMENT, and
	// this route is offline: no credential, so a managed document refuses it.
	// Re-activating the company's own revision changes nothing in it — it is
	// the offline twin of POST /config/reload, the gesture a rotated
	// credential needs — and stays open. AFTER the lookup, so a missing id is
	// told that, not that the document is managed.
	if err = refuseManagedActivation(ctx, cs, revisionID); err != nil {
		return err
	}
	document, err := openStored(cs.cipher, revision)
	if err != nil {
		return err
	}
	company, err := config.DecodeCompany(document)
	if err == nil {
		err = company.ValidateRunnable()
	}
	if err != nil {
		return fmt.Errorf("revision %s cannot run on this build, so this node could "+
			"not start on it: %w", revisionID, err)
	}
	_, over, _, err := offlineBase(ctx, cs)
	if err != nil {
		return err
	}
	if err := cs.configs.OfferStored(ctx, revisionID, over); err != nil {
		return err
	}
	// RE-ACTIVATING THE FLEET'S OWN REVISION IS NOT A NO-OP: the boot
	// publishes it over itself, which mints a new epoch every node is
	// watching — how a rotated secret reaches a running fleet without a
	// restart of every node.
	fmt.Fprintf(stdout, "marked %s to boot on this node; it is offered to the fleet over %s\n",
		revisionID, over.String())
	fmt.Fprintln(stdout, activatePublishNote(revisionID, over))
	return nil
}

// revisionOrKnown resolves an id, or the company this node knows for an empty
// one ([store.Configs.Restorable]).
func revisionOrKnown(ctx context.Context, cs *configStore, revisionID string) (store.Revision, error) {
	if revisionID == "" {
		rev, found, err := cs.configs.Restorable(ctx)
		if err != nil {
			return store.Revision{}, err
		}
		if !found {
			return store.Revision{}, errors.New(
				"this node knows no company; run `crewlet config import`")
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

	// env is the invocation's environment ([runIn]), which the API route
	// reads its credential from.
	environ config.Source
}

// importCompany writes a company document as a new revision, through whichever
// route can actually reach the fleet.
//
// # Two routes, and the difference is not cosmetic
//
// The OFFLINE route opens this node's store directly and makes the revision
// what this node boots on, as an offer built on the company this node knows. It
// cannot move the fleet's activation pointer, because on the default topology
// that pointer lives inside the engine's own process — so it takes effect when
// this node next starts, and reaches the fleet only while the fleet still runs
// what it was built on.
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
//
// # A redacted read imports over the company it was read from
//
// `GET /config`, the dashboard's Copy as YAML and `crewlet config show` mask
// every inline credential, and a file kept in sync from them is written back
// with this command. Each route restores the masks from the revision the
// import replaces, as `PUT /config` restores a document sent back to it: the
// API route IS that PUT, and the offline route restores from the company this
// node knows, the one it parents the new revision on ([config.LoadCompany]). Both validate after the restore, because a mask is
// judged as the credential it stands for — the import's own check before
// either route used to refuse every redacted copy, on both, for carrying the
// marker in a field nobody touched.
func importCompany(ctx context.Context, t importTarget, stdout io.Writer) error {
	if t.path == "" {
		return errors.New("config import needs a company document to read")
	}
	summary := strings.TrimSpace(t.summary)
	if summary == "" {
		summary = "imported from " + t.path
	}

	// THE CHARTER'S FAULTS ARE SAID, NEVER REFUSED HERE. An import is how a
	// company is restored and migrated, and a file that ran yesterday under a
	// build without the charter's rules must import today; what an editor is
	// refused is only a change that leaves a part at fault
	// ([config.Company.ValidateCharterChange]). Said up front, whichever route
	// the import takes, so the operator reads what every node will warn about.
	// From the file as written, parsed and not yet validated: a redacted copy
	// validates only once each route has restored its masks, and the charter
	// holds no credential for a mask to stand in for.
	data, err := os.ReadFile(t.path)
	if err != nil {
		return fmt.Errorf("company config %s: %w", t.path, err)
	}
	written, err := config.ParseCompanyDocument(data)
	if err != nil {
		return fmt.Errorf("company config %s: %w", t.path, err)
	}
	for _, w := range written.CharterWarnings() {
		fmt.Fprintln(stdout, warningLine(w))
	}

	boot, err := loadBootstrapForStore(t.bootstrapPath)
	if err != nil {
		return err
	}
	if strings.TrimSpace(t.apiURL) != "" {
		return importThroughNode(ctx, boot, t, summary, stdout)
	}

	cs, closeStore, err := openConfigStore(ctx, t.bootstrapPath, "")
	if err != nil {
		if !errors.Is(err, store.ErrLocked) {
			return err
		}
		// The engine holds its database, so this is the live case rather
		// than a failure: go through the node that is holding it.
		return importThroughNode(ctx, boot, t, summary, stdout)
	}
	defer closeStore()
	// THE OFFLINE ROUTE PRESENTS NO CREDENTIAL, so on a managed document
	// it is refused here; the API route is judged by the node it reaches.
	if err = refuseManagedOffline(cs.boot, "an offline `crewlet config import`"); err != nil {
		return err
	}
	// VALIDATED HERE, before anything is stored. Offline nothing else ever
	// checks the revision before every node in the fleet is handed it, and
	// one that cannot be built is refused by each of them, one after
	// another — a fleet-wide incident from a typo that could have been
	// caught on this machine.
	company, err := config.LoadCompany(t.path, knownRevision(ctx, cs))
	if err != nil {
		return err
	}
	return importConfig(ctx, cs, t.path, company, summary, stdout)
}

// knownRevision is the company this node knows ([store.Configs.Restorable]),
// as the revision a redacted import is restored against: nil where it knows
// none, and read as the STORED form, because a newer peer may have written it.
func knownRevision(ctx context.Context, cs *configStore) config.PriorRevision {
	return func() (*config.Company, error) {
		known, found, err := cs.configs.Restorable(ctx)
		if err != nil {
			return nil, fmt.Errorf("read the company this node knows: %w", err)
		}
		if !found {
			return nil, nil
		}
		document, err := openStored(cs.cipher, known)
		if err != nil {
			return nil, err
		}
		company, err := config.DecodeCompany(document)
		if err != nil {
			return nil, fmt.Errorf("parse revision %s, the company this node knows: %w",
				known.ID, err)
		}
		return company, nil
	}
}

// importThroughNode PUTs the document at a running node.
//
// THE NODE JUDGES IT, and nothing here does first. It restores what a
// redacted read masked from the revision it is replacing and only then holds
// the document to every rule, the same rules a local check would apply
// before the restore and so refuse every redacted copy for its masks — and
// what it refuses it refuses before storing anything, naming each field.
func importThroughNode(ctx context.Context, boot *config.Bootstrap, t importTarget,
	summary string, stdout io.Writer,
) error {
	client, err := newConfigClient(t.environ, boot, t.apiURL)
	if err != nil {
		return err
	}
	// THE FILE'S OWN BYTES travel, not a re-encoding of the parsed
	// document: Tier B's secrets are `${VAR}` pointers stored verbatim, and
	// the node forms its own opinion of the document anyway.
	doc, err := os.ReadFile(t.path)
	if err != nil {
		return fmt.Errorf("company config %s: %w", t.path, err)
	}
	id, epoch, err := client.Import(ctx, doc, summary)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "imported %s as revision %s, active on epoch %d\n",
		t.path, id, epoch)
	fmt.Fprintf(stdout, "wrote to %s\n", client.Describe())
	return nil
}
