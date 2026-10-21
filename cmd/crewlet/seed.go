package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/store"
)

// ONE DECISION ABOUT THE TIER B FILE, taken before the engine starts.
//
// A Tier B file on `crewlet run`'s command line has two consumers: the epoch
// engine.New boots on, and the import the seed makes once the broker is up.
// They used to decide separately. The boot took the file whenever one was
// loaded, while the import ignored a bootstrap seed the store's own company
// outranks and dropped every seed on a managed deployment — so a restart with
// a month-old company.yaml built, equipped and installed that file's company
// before the first reconcile replaced it, and a managed node with an empty
// store ran a stray file's company while logging that the file is never
// imported.
//
// What building a company does is not undone by the reconcile that replaces
// it: its shared tool servers started, its coding CLIs' credential bundles
// restored into empty logins (which every later revision's restore then
// keeps, because a login is present), its native halves started and the store
// opened at its embedding width (both decided once per process), so a live
// change to the width pinned every node carrying the file on the file's
// company for good.
//
// So the decision is [decideSeed], made ONCE, and it produces a [seedPlan]
// that both consumers take: [bootOptions] and [startReconciler] accept a plan
// and nothing else, so neither can be handed the file undecided.
//
// # The engine never boots on a file
//
// It boots on the revision this node's own store has ACTIVATED, with the
// instant it was activated at, or on nothing. Where the file is to be
// imported, the node boots unconfigured and the import and the reconciler's
// first tick bring the company in before a seat is claimed — the path a node
// without `data` takes on every boot, and the one every first revision pushed
// over the API takes. Booting on the file there was wrong as well, and not
// only on the cases the old rule got wrong: the import can still be refused
// after the engine is up (a file is held to every rule where it is written),
// and a boot that fails then has already restored the file's credential
// bundles into the logins its successor's seats share.
//
// Not booting on the file is also what keeps `-import-company` a repair: the
// store's revision is not read, so one this build cannot run does not refuse
// the boot that replaces it.

// seedAct is what the seed does once the broker is up: the half of the
// decision that cannot run before the engine, because the fleet's activation
// pointer lives inside the process the engine starts.
type seedAct string

const (
	// seedNone imports nothing: no file was given, a managed deployment does
	// not read one, or the store's own company outranks it. The node
	// publishes its own active revision where the fleet has none, or an
	// older one ([publishLocalActive]).
	seedNone seedAct = "none"
	// seedBootstrap imports a -company file into a node whose store holds
	// no company: an empty store, or a scratch one, which holds nothing by
	// design.
	seedBootstrap seedAct = "bootstrap"
	// seedImport is -import-company: the file replaces what the fleet runs.
	seedImport seedAct = "import"
)

// valid reports whether a is one of this build's acts. The zero value is not:
// a plan nobody decided is refused rather than read as one that imports
// nothing, which would boot a node unconfigured and silent.
func (a seedAct) valid() bool {
	switch a {
	case seedNone, seedBootstrap, seedImport:
		return true
	}
	return false
}

// errUndecidedSeed refuses a plan [decideSeed] did not make.
var errUndecidedSeed = errors.New("the Tier B seed was not decided before the " +
	"engine started; this is a defect in `crewlet run`, not in either config file")

// seedPlan is the decided seed: what engine.New boots on and what the seed
// does with the file once the broker is up.
//
// Only [decideSeed] makes one, and its fields are unexported so that nothing
// assembles a plan from a file it has not decided about.
type seedPlan struct {
	act seedAct

	// company is the revision engine.New boots on — this node's own active
	// revision — and activatedAt the instant it was activated at, which the
	// engine stamps the org chart and the knowledge spaces with at boot
	// ([engine.Options.ActivatedAt]). Nil when the node boots unconfigured.
	company     *config.Company
	activatedAt time.Time

	// file is the document act imports: set for [seedBootstrap] and
	// [seedImport], zero for [seedNone].
	file tierBSeed
}

// decided refuses a plan [decideSeed] did not make.
func (p seedPlan) decided() error {
	if !p.act.valid() {
		return errUndecidedSeed
	}
	return nil
}

// startsOn is what the boot line calls the company the engine starts on.
func (p seedPlan) startsOn() string {
	if p.company == nil {
		return "(unconfigured)"
	}
	return p.company.Name
}

// storedActive is the revision a node's own store marks active, opened under
// the keyring.
type storedActive struct {
	revision store.Revision
	document []byte
}

// storedReader reads the revision this node's store marks active, reporting
// whether there is one. A function rather than a store, because [decideSeed]
// reads the store only on the cases that need it, and `crewlet run` holds no
// open store at the point it decides: it opens one, reads and closes it
// ([storeAt]), so the engine owns the backends it opens.
type storedReader func(ctx context.Context) (storedActive, bool, error)

// storeAt is the reader `crewlet run` decides with: the node's own store,
// opened, read and closed again.
//
// Closed rather than handed on: the engine opens its own backends and owns
// their lifetime, and a caller-supplied set would move that ownership into
// this function's caller for the whole run. Two sequential opens at boot cost
// one migration pass over an already-migrated file; a split lifetime costs a
// leaked store on every error path that does not know it now has one.
func storeAt(bootstrapPath string) storedReader {
	return func(ctx context.Context) (storedActive, bool, error) {
		cs, closeStore, err := openConfigStore(ctx, bootstrapPath,
			"Another `crewlet run` holds this node's store: stop it before "+
				"starting a second, since one store belongs to one process.")
		if err != nil {
			return storedActive{}, false, err
		}
		defer closeStore()
		return readStoredActive(ctx, cs.configs, cs.cipher)
	}
}

// readStoredActive reads and opens the active revision of configs.
//
// A revision that does not open — stored without a seal, or sealed under a key
// this keyring does not hold — refuses the boot rather than being seeded over:
// replacing a company nobody here can read with one this node happens to have
// on disk, silently, on a restart, is the failure the seal exists to prevent.
func readStoredActive(ctx context.Context, configs *store.Configs,
	cipher secrets.Cipher) (storedActive, bool, error) {

	active, found, err := configs.Active(ctx)
	if err != nil {
		return storedActive{}, false, fmt.Errorf("read the active revision: %w", err)
	}
	if !found {
		return storedActive{}, false, nil
	}
	document, err := openStored(cipher, active)
	if err != nil {
		return storedActive{}, false, err
	}
	return storedActive{revision: active, document: document}, true, nil
}

// decideSeed decides what the Tier B file on the command line is for, and
// what the engine boots on — once, before engine.New, for both consumers.
//
//   - A managed deployment's bootstrap seed was never read ([managedSeed]):
//     the node decides as one given no file, and the ignored file is said
//     once that decision holds. An override was refused before anything
//     started.
//   - -import-company boots UNCONFIGURED and imports the file. The store is
//     not read, so a stored revision this build cannot run does not refuse
//     the boot that replaces it.
//   - A scratch store is not read: what it holds is the previous run's, it is
//     deleted as the engine opens it, and opening it here would create the
//     replicated estate a node without `data` must not have. The node boots
//     unconfigured, and a -company file is a bootstrap.
//   - Otherwise the store's active revision is the boot, with the instant it
//     was activated at — a -company file beside it is either that revision
//     byte for byte, or one the store's company outranks, which is said. With
//     no active revision the node boots unconfigured, and a -company file is
//     a bootstrap.
//
// A nil company is the UNCONFIGURED case, a state rather than a failure: the
// node serves its API so an operator can push the first revision into it.
func decideSeed(ctx context.Context, boot *config.Bootstrap, seed tierBSeed,
	stored storedReader, log *slog.Logger) (seedPlan, error) {

	if !seed.Managed {
		return decideFile(ctx, boot, seed, stored, log)
	}
	plan, err := decideFile(ctx, boot, tierBSeed{Path: seed.Path}, stored, log)
	if err != nil {
		return seedPlan{}, err
	}
	// SAID ONCE THE DECISION HOLDS, and never beside a refusal of it: a
	// stored revision that cannot run, or a store that cannot be read, is
	// what the operator has to act on, and a paragraph about a file nobody
	// reads printed beside it is noise at the moment they read carefully.
	warnManagedSeed(ctx, boot, seed.Path, log)
	return plan, nil
}

// decideFile is [decideSeed] for a seed the deployment reads: a managed
// deployment's bootstrap seed reaches it as no file at all.
func decideFile(ctx context.Context, boot *config.Bootstrap, seed tierBSeed,
	stored storedReader, log *slog.Logger) (seedPlan, error) {

	if seed.Override {
		return seedPlan{act: seedImport, file: seed}, nil
	}
	bootstrap := seedPlan{act: seedNone}
	if seed.Company != nil {
		bootstrap = seedPlan{act: seedBootstrap, file: seed}
	}
	if boot.Store.Scratch {
		return bootstrap, nil
	}
	active, found, err := stored(ctx)
	if err != nil {
		return seedPlan{}, err
	}
	if !found {
		return bootstrap, nil
	}
	differs := false
	if seed.Company != nil {
		var same bool
		if same, err = seed.is(active.document); err != nil {
			return seedPlan{}, err
		}
		differs = !same
	}
	company, err := runnableRevision(boot, active, seed, differs)
	if err != nil {
		return seedPlan{}, err
	}
	if differs {
		// THE COMPANY EXISTS AND THIS FILE IS ONLY A BOOTSTRAP, so the store
		// wins. Loud, because the operator edited a file that is not going to
		// take effect, and the old behaviour's one virtue was that it always
		// did something.
		log.WarnContext(ctx, "company_seed_ignored",
			"file", seed.Path, "active_revision", active.revision.ID,
			"reason", "the store already holds a company, and -company "+
				"only bootstraps an empty one",
			"hint", "to make this file the company again, restart with "+
				"-import-company "+seed.Path+"; to change the running "+
				"fleet with no restart, run `crewlet config import "+
				seed.Path+"`")
	}
	return seedPlan{act: seedNone, company: company,
		activatedAt: active.revision.ActivatedAt}, nil
}

// is reports whether the seed's file is, byte for byte, the opened document of
// a stored revision.
//
// AGAINST THE OPENED DOCUMENT, never the stored bytes: every revision is
// sealed under the keyring, and a fresh nonce makes the ciphertext differ on
// every seal, so a byte comparison would call every file edited and import a
// fresh revision on every single boot.
func (s tierBSeed) is(opened []byte) (bool, error) {
	document, err := json.Marshal(s.Company)
	if err != nil {
		return false, fmt.Errorf("encode the company config: %w", err)
	}
	return bytes.Equal(document, opened), nil
}

// runnableRevision decodes the active revision for the engine to boot on.
//
// VALIDATED HERE, naming the revision, because booting is applying and the
// stored-form decode holds a revision to no rule. The engine checks the same
// rules again as it builds the epoch, but only this frame knows which revision
// it is and what the way out is: the node is not serving its API yet, and its
// listener answers every route it will serve `503 starting`.
//
// The RUNNABLE rules only, like every apply: an admission rule is one an apply
// does not enforce, because a newer peer may have admitted the revision under
// rules this build does not share. The reconciler warns about it once it
// applies the epoch (see [config.Company.ValidateRunnable]).
//
// THE WAY OUT HAS TO REACH THIS NODE'S OWN STORE. The boot reads nothing
// else, and a revision written through another node reaches that store only
// through this node's reconciler, which a refused boot never starts — so a
// corrected revision written through another node fixes the fleet and leaves
// this node refused on every restart, however many nodes are serving. What
// writes the store directly is an offline `crewlet config import`, which is
// the way out off a managed deployment: the next start boots on it and
// publishes it to the fleet ([publishLocalActive]).
//
// On a managed deployment (`api.auth.company_writers`, ADR-0030) the offline
// import and `-import-company` are both refused, so naming either alone sends
// the operator to another refusal, and naming the managing system's write
// through a serving node sends them to a write that never reaches this store.
// What clears the refusal is taking the document back on this node for one
// start: the list removed from its Tier A, a corrected document imported, the
// node started — which publishes the import — and the list restored. The
// document to import is the managing system's own correction where it has
// written one already, so the start publishes what it wrote rather than
// replacing it with something else.
func runnableRevision(boot *config.Bootstrap, active storedActive, seed tierBSeed,
	differs bool) (*config.Company, error) {

	company, err := config.DecodeCompany(active.document)
	if err != nil {
		return nil, fmt.Errorf("parse the active revision %s: %w", active.revision.ID, err)
	}
	if err := company.ValidateRunnable(); err != nil {
		if boot.API.Auth.CompanyManaged() {
			return nil, fmt.Errorf("the active revision %s cannot run on this build, "+
				"and this node starts only on the revision its own store holds: a "+
				"corrected one written through another node reaches the fleet, not "+
				"this store. The company config is managed by another system "+
				"(`api.auth.company_writers`: %s), which refuses an offline import, "+
				"so take the document back on this node for one start: remove "+
				"api.auth.company_writers from this node's Tier A, import a corrected "+
				"document with `crewlet config import` (the one the managing system "+
				"wrote, if it has corrected the fleet already), start the node, which "+
				"publishes the import to a fleet running nothing newer, then restore "+
				"api.auth.company_writers and restart: %w", active.revision.ID,
				strings.Join(boot.API.Auth.CompanyWriters, ", "), err)
		}
		// A DIFFERENT FILE ON THE COMMAND LINE IS A WAY OUT, and the one
		// the operator is holding: it would only have been ignored, so it
		// is named as the override that makes it the company instead.
		instead := ""
		if differs {
			instead = ", or restart with -import-company " + seed.Path +
				" to make that file the company"
		}
		return nil, fmt.Errorf("the active revision %s cannot run on this build; "+
			"import a corrected document with `crewlet config import`%s and start "+
			"again: %w", active.revision.ID, instead, err)
	}
	return company, nil
}

// bootOptions is what `crewlet run` builds its engine from: the Tier A it
// loaded, and the company the decided plan boots on with the instant it was
// activated at ([engine.Options.ActivatedAt]) — which the engine stamps the
// chart and the knowledge spaces with at boot: a boot that dropped it would
// stamp nothing at all, and the company's projects would wait for the next
// apply.
//
// ONE VALUE, handed to the engine WHOLE, so no field of it can be decided here
// and dropped at the call site.
func bootOptions(boot *config.Bootstrap, plan seedPlan) (engine.Options, error) {
	if err := plan.decided(); err != nil {
		return engine.Options{}, err
	}
	return engine.Options{Bootstrap: boot, Company: plan.company,
		ActivatedAt: plan.activatedAt}, nil
}
