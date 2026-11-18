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
	"github.com/crewlet/crewlet/internal/configplane"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/version"
)

// ONE DECISION ABOUT THE TIER B FILE, taken before the engine starts: [decideSeed]
// makes it ONCE and yields a [seedPlan] both consumers take ([bootOptions] and
// [startReconciler] accept a plan and nothing else). Deciding separately built a
// month-old company.yaml before the first reconcile replaced it, and what a build
// does is not undone by that reconcile: shared tool servers started, credential
// bundles restored into empty logins, native halves started, the store opened at
// its embedding width. So the engine never boots on a file: it boots on the
// revision this node's store BOOTS on, or on nothing, and the import and first
// tick bring a file's company in before a seat is claimed. The import is refused
// after the engine is up, with the file's bundles already restored, if it boots
// first; it keeps what booting bought by building a file it will write before
// writing it ([engine.Engine.Buildable]). `-import-company` stays a repair.

// seedAct is what the seed does once the broker is up: the half of the
// decision that cannot run before the engine, because the fleet's activation
// pointer lives inside the process the engine starts.
type seedAct string

const (
	// seedNone imports nothing: no file was given, a managed deployment does
	// not read one, or a company this node knows outranks it. The node
	// publishes an offline change it boots on over the revision it was built
	// on, and restores the company it knows where the fleet's pointer is
	// gone ([publishBoot]).
	seedNone seedAct = "none"
	// seedBootstrap imports a -company file where no company exists: this
	// node's store holds none — it is empty, or a scratch one, which holds
	// nothing by design — and, decided once the broker is up, the fleet
	// runs none either.
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

	// company is the revision engine.New boots on — the one this node's own
	// store boots on — and activatedAt the instant of the activation it was
	// applied from, which the engine stamps the org chart and the knowledge
	// spaces with at boot ([engine.Options.ActivatedAt]): zero for an offline
	// offer, which nothing has activated yet. Nil when the node boots
	// unconfigured.
	company     *config.Company
	activatedAt time.Time

	// boot is the stored revision company was opened from, and fleets the
	// fleet's revision as this node last saw it — the two facts the boot
	// publish decides on ([publishBoot]) and the revision the reconciler
	// starts out serving ([engine.ReconcilerOptions.Booted]). Read with the
	// decision and never again: the store they were read from is CLAIMED
	// until the engine has opened it (held, below), so nothing can have
	// replaced them by the time the publish names them. Zero and nil where
	// the store was not read or holds neither.
	boot   store.Revision
	fleets *store.Revision

	// file is the document act imports: set for [seedBootstrap] and
	// [seedImport], zero for [seedNone].
	file tierBSeed

	// held is this process's claim on the node's own database, taken with
	// the read above and given back by [bootEngine] once the engine has
	// opened the same file. The zero value holds nothing: a scratch store
	// and -import-company read no store, and neither takes a claim on one;
	// nor does the plan [bootEngine] answers, whose claim it has given back.
	held heldStore
}

// heldStore is a claim on the node's own database ([store.Hold]) that outlives
// the handle it was read through.
//
// WHY THE BOOT KEEPS ONE. `crewlet run` reads the store, closes it, and the engine
// opens it again only once the broker is up, seconds later on a cluster join. An
// offline `crewlet config import` landing in that gap wrote the row the boot had
// already decided on, so the first tick served the fleet's revision and cleared
// the offer the operator had just made. Held from the read to the open
// ([bootEngine]), the claim makes the command meet the starting node as it meets
// the engine: refused naming it, or sent to its API (`503 starting` until it
// serves). The zero value holds nothing, so every path can release unconditionally.
type heldStore struct {
	// given is the claim's release, nil where there is no claim.
	// Idempotent ([store.Hold]), so releasing twice is releasing once.
	given func()
}

// release gives the claim back. Safe on a claim nobody took.
func (h heldStore) release() {
	if h.given != nil {
		h.given()
	}
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

// storedRevision is a revision a node's own store holds, and its document
// opened under the keyring — nil where it does not open, with unopened saying
// why, for the readers that can do without it.
type storedRevision struct {
	revision store.Revision
	document []byte
	unopened error
}

// storedState is what a node's own store says about its company: the revision
// it boots on, the fleet's revision as it last saw it, and the company it knows
// ([store.Configs.Restorable]). Nil for each one it does not hold.
type storedState struct {
	boot   *storedRevision
	fleets *storedRevision
	known  *storedRevision
}

// storedReader reads what this node's store says about its company, and hands
// back this process's claim on the file it read ([heldStore]) for the caller
// to hold until the engine has opened the same one. A function rather than a
// store, because [decideSeed] reads the store only on the cases that need it,
// and `crewlet run` holds no open store at the point it decides: it opens one,
// reads and closes it ([storeAt]), so the engine owns the backends it opens.
//
// A reader that fails holds nothing: it gives back whatever it took, so only a
// state a caller received carries a claim to release.
type storedReader func(ctx context.Context) (storedState, heldStore, error)

// storeAt is the reader `crewlet run` decides with: the node's own store, opened,
// read and closed again, with this process's CLAIM on the file kept across the
// close.
//
// The handle is closed rather than handed on: the engine owns its backends'
// lifetime, and a caller-supplied set would move that ownership to this
// function's caller for the whole run. Two opens at boot cost one migration pass
// over a migrated file; a split lifetime costs a leaked store on every error path
// that does not know it has one. The CLAIM is handed on, keeping the facts this
// read answers true when the engine opens the file ([heldStore]); it is taken
// while the handle still holds the file, so the store is never free in between,
// and the engine's open shares it ([store.Hold]).
func storeAt(bootstrapPath string) storedReader {
	return func(ctx context.Context) (storedState, heldStore, error) {
		cs, closeStore, err := openConfigStore(ctx, bootstrapPath,
			"Another `crewlet run` holds this node's store: stop it before "+
				"starting a second, since one store belongs to one process.")
		if err != nil {
			return storedState{}, heldStore{}, err
		}
		defer closeStore()
		release, err := store.Hold(cs.boot.Store.Path)
		if err != nil {
			return storedState{}, heldStore{}, err
		}
		held := heldStore{given: release}
		state, err := readStoredState(ctx, cs.configs, cs.cipher)
		if err != nil {
			held.release()
			return storedState{}, heldStore{}, err
		}
		return state, held, nil
	}
}

// readStoredState reads what configs says about the company, and opens each
// revision it names.
//
// A BOOT ROW that does not open — stored without a seal, or sealed under a key
// this keyring does not hold — refuses the boot rather than being seeded over:
// replacing a company nobody here can read with one this node happens to have
// on disk, silently, on a restart, is the failure the seal exists to prevent.
// The other two are not booted on, so one that does not open refuses nothing
// here: the fleet's row only tells a stray -company file from the fleet's own,
// the boot publish opens what it publishes again, and the company this node
// knows is refused by the one reader that needs its document.
func readStoredState(ctx context.Context, configs *store.Configs,
	cipher secrets.Cipher) (storedState, error) {

	var state storedState
	read := func(what string, row func(context.Context) (store.Revision, bool, error)) (
		*storedRevision, error) {

		revision, found, err := row(ctx)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", what, err)
		}
		if !found {
			return nil, nil
		}
		document, err := openStored(cipher, revision)
		return &storedRevision{revision: revision, document: document, unopened: err}, nil
	}
	var err error
	if state.boot, err = read("the revision this node boots on", configs.Boot); err != nil {
		return storedState{}, err
	}
	if state.boot != nil && state.boot.unopened != nil {
		return storedState{}, state.boot.unopened
	}
	if state.fleets, err = read("the fleet's revision as this node last saw it",
		configs.Fleets); err != nil {
		return storedState{}, err
	}
	if state.known, err = read("the company this node knows", configs.Restorable); err != nil {
		return storedState{}, err
	}
	return state, nil
}

// over is what a change made on this node now is built on, as `crewlet config
// import` builds one ([offlineBase]) and the boot publish compares it with the
// pointer ([offerOn]). Zero for a node that knows no company.
func (s storedState) over() store.Offer {
	if s.known == nil {
		return store.Offer{}
	}
	return offerOn(s.known.revision)
}

// offerOn is what a change made on known is built on, as an offline command offers
// it ([store.Configs.Offer]):
//
//   - a revision, or an offer the fleet's row names other than one built on itself
//     (the fleet took it, only its record is missing): the revision itself.
//   - an offer an earlier start SENT and never confirmed: its base, or itself. The
//     change is built on it and whether the fleet runs it is unknown here, so the
//     boot accepts either ([store.Offer.Unconfirmed]); refusing would leave an
//     operator whose node cannot start no offline way to correct what it boots on.
//   - an offer nobody sent: what that one was built on, since the fleet has never
//     seen it.
func offerOn(known store.Revision) store.Offer {
	offer := known.Offer
	switch {
	case offer == nil || (known.Fleets && !offer.Over(known.ID)):
		return store.Offer{Base: known.ID}
	case !offer.PlannedAt.IsZero() && offer.Base != known.ID:
		return store.Offer{Base: offer.Base, Unconfirmed: known.ID}
	}
	return store.Offer{Base: offer.Base, Unconfirmed: offer.Unconfirmed}
}

// publishedWhile is when the boot publishes a change built on over, as every
// offline command and refusal says it.
func publishedWhile(over store.Offer) string {
	when := "only while the fleet still runs " + over.Base + ", the revision it is built on"
	if over.Base == "" {
		when = "only if the fleet has no company yet"
	}
	if over.Unconfirmed != "" {
		when += ", or " + over.Unconfirmed + ", the offline change it replaced, which an " +
			"earlier start of this node published and could not confirm"
	}
	return when
}

// decideSeed decides what the Tier B file on the command line is for, and what
// the engine boots on, once, before engine.New. A nil company is the UNCONFIGURED
// case, not a failure: the node serves its API for a first push.
//
//   - A managed deployment's seed was never read ([managedSeed]): no file, said once.
//   - -import-company boots UNCONFIGURED and imports the file, reading no store, so
//     a stored revision this build cannot run does not refuse that boot.
//   - A scratch store is not read (it would create the estate a node without `data`
//     must not have): unconfigured, -company a bootstrap.
//   - Otherwise the store's boot row is the boot, a -company file beside it that
//     revision or outranked (said); with no row, the file is a bootstrap unless the
//     node SAW the fleet's company.
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
	state, held, err := stored(ctx)
	if err != nil {
		return seedPlan{}, err
	}
	plan, err := decideStored(ctx, boot, seed, bootstrap, state, log)
	if err != nil {
		// THE CLAIM GOES BACK WITH THE REFUSAL. No engine starts on a
		// decision that failed, so nothing else would ever release it —
		// and the way out of a boot row this build cannot run is an
		// offline command against this very store ([runnableRevision]),
		// which a claim nobody holds any more would refuse.
		held.release()
		return seedPlan{}, err
	}
	// EVERY PLAN A READ PRODUCED CARRIES THE CLAIM, whether or not it boots
	// on anything: a store with no boot row is as replaceable in that gap as
	// one with one, and the seed's own import writes into it.
	plan.held = held
	return plan, nil
}

// decideStored is [decideFile] for a node whose store was read: the plan that
// read produces, with the claim on the file its caller attaches.
func decideStored(ctx context.Context, boot *config.Bootstrap, seed tierBSeed,
	bootstrap seedPlan, state storedState, log *slog.Logger) (seedPlan, error) {

	var fleets *store.Revision
	if state.fleets != nil {
		fleets = &state.fleets.revision
	}
	if state.boot == nil {
		if state.fleets == nil {
			return bootstrap, nil
		}
		// A NODE THAT HAS SEEN THE FLEET'S COMPANY AND BOOTS ON NOTHING —
		// it joined and refused the fleet's revision, or never finished an
		// apply — knows a company exists, so the file is no bootstrap.
		// Booting on it would run the file's company until the first tick,
		// and importing it would replace the fleet's create-only write with
		// nothing the operator asked for.
		if seed.Company != nil {
			same := false
			if state.fleets.document != nil {
				var err error
				if same, err = seed.is(state.fleets.document); err != nil {
					return seedPlan{}, err
				}
			}
			if !same {
				log.WarnContext(ctx, "company_seed_ignored",
					"file", seed.Path, "fleet_revision", state.fleets.revision.ID,
					"reason", "this node has seen the fleet's company "+
						state.fleets.revision.ID+"; -company only bootstraps a "+
						"company that does not exist yet",
					"hint", seedIgnoredHint(seed.Path))
			}
		}
		return seedPlan{act: seedNone, fleets: fleets}, nil
	}
	booted := *state.boot
	differs := false
	if seed.Company != nil {
		same, err := seed.is(booted.document)
		if err != nil {
			return seedPlan{}, err
		}
		differs = !same
	}
	company, err := runnableRevision(boot, booted, state.over())
	if err != nil {
		return seedPlan{}, err
	}
	if differs {
		// THE COMPANY EXISTS AND THIS FILE IS ONLY A BOOTSTRAP, so the store
		// wins. Loud, because the operator edited a file that is not going to
		// take effect, and the old behaviour's one virtue was that it always
		// did something.
		log.WarnContext(ctx, "company_seed_ignored",
			"file", seed.Path, "boot_revision", booted.revision.ID,
			"reason", "the store already holds a company, and -company "+
				"only bootstraps a company that does not exist yet",
			"hint", seedIgnoredHint(seed.Path))
	}
	// AT THE INSTANT IT WAS APPLIED FROM, never the latest the fleet gave it:
	// a re-activation this node refused raises that one without having been
	// applied here. An offer has none — nothing has applied it — and the
	// chart is stamped once the activation that publishes it is applied.
	return seedPlan{act: seedNone, company: company, activatedAt: booted.revision.ServedAt,
		boot: booted.revision, fleets: fleets}, nil
}

// seedIgnoredHint is what a -company file a company outranks is told: the two
// ways to make it the company, the one that respects what the fleet has moved
// on to first.
func seedIgnoredHint(path string) string {
	return "to make this file the company, run `crewlet config import " + path +
		"`: with this node stopped it boots on the import and publishes it at its " +
		"next start only while the fleet still runs the revision it is built on, " +
		"and with -api it is written through a running node now. " +
		"-import-company " + path + " at the next start replaces whatever the " +
		"fleet runs"
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

// isFleets reports whether the seed's file is the revision the fleet's pointer
// names, read from the pointer's own payload as a peer's reconciler reads it,
// and answers that revision as this node's history would keep it.
//
// An error is a comparison that could not be made — the payload is gone with
// a pointer that moved since it was read, or it does not open under this
// node's keyring — and never a claim that the two differ.
func (s tierBSeed) isFleets(ctx context.Context, plane coord.Plane,
	target coord.Activation, cipher secrets.Cipher) (bool, store.Revision, error) {

	payload, found, err := plane.Payload(ctx, target.RevisionID)
	if err != nil {
		return false, store.Revision{}, fmt.Errorf("read the fleet's revision %s: %w",
			target.RevisionID, err)
	}
	if !found {
		return false, store.Revision{}, fmt.Errorf("the fleet's revision %s was "+
			"replaced while it was read", target.RevisionID)
	}
	document, err := secrets.Open(cipher, payload)
	if err != nil {
		return false, store.Revision{}, fmt.Errorf("open the fleet's revision %s: %w",
			target.RevisionID, err)
	}
	same, err := s.is(document)
	if err != nil {
		return false, store.Revision{}, err
	}
	return same, configplane.RevisionOf(target, payload), nil
}

// runnableRevision decodes the revision this node boots on, for the engine.
//
// VALIDATED HERE, naming the revision, because booting is applying and the stored
// decode holds a revision to no rule; only this frame knows which revision it is
// and what the way out is. The RUNNABLE rules only, like every apply: a newer peer
// may have admitted it under rules this build does not share. A boot row this
// build cannot run came from another build or store, so the way out must reach
// THIS NODE'S OWN STORE: an offline `crewlet config import`, booted on next start
// and published only over its base. Not `config activate` (it boots on a stored
// revision this check refuses) nor `-import-company` (it replaces whatever the
// fleet runs). Where the import is refused (ADR-0030) the list leaves Tier A for
// one start.
func runnableRevision(boot *config.Bootstrap, booted storedRevision, over store.Offer) (*config.Company, error) {
	company, err := config.DecodeCompany(booted.document)
	if err == nil {
		err = company.ValidateRunnable()
	}
	if err == nil {
		return company, nil
	}
	published := publishedWhile(over)
	const written = "revision %s, which this node boots on, cannot run on this build. A " +
		"node's store names only a revision it applied or one an offline command " +
		"chose, so this one was written by another build or another store: start " +
		"the build that wrote it, or a newer one, again. "
	if boot.API.Auth.CompanyManaged() {
		return nil, fmt.Errorf(written+"The company config is managed by another system "+
			"(`api.auth.company_writers`: %s), which refuses an offline import, so to "+
			"start this build on another document take it back on this node for one "+
			"start: remove api.auth.company_writers from this node's Tier A, import the "+
			"managing system's current document with `crewlet config import`, start "+
			"the node, which publishes the import %s and so never replaces a newer "+
			"revision, then restore api.auth.company_writers and restart: %w",
			booted.revision.ID, strings.Join(boot.API.Auth.CompanyWriters, ", "),
			published, err)
	}
	return nil, fmt.Errorf(written+"To start this build on another document instead, "+
		"import a corrected one with `crewlet config import` and start again: the "+
		"node boots on the import and publishes it to the fleet %s; if the fleet has "+
		"moved on, the import is this node's alone until it applies the fleet's "+
		"revision: %w", booted.revision.ID, published, err)
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

// engineOpener opens the engine a decided boot starts: [engine.New], handed
// the options [bootOptions] built with whatever `crewlet run` adds to them that
// is not the seed's to decide.
type engineOpener func(ctx context.Context, opts engine.Options) (*engine.Engine, error)

// bootEngine decides what the engine boots on ([decideSeed]) and opens it as ONE
// STEP: this node's store is claimed from the read the decision is made on until
// the engine has opened the same file ([heldStore]), and the claim is given back
// on every way out: after the open on success, the engine's open having shared it,
// and at once on any earlier failure. The plan it answers carries no claim.
//
// ONE FUNCTION, because the claim's life is the stretch between decision and open.
// Spread across `crewlet run`'s body, a release moved one line up would reopen the
// gap with nothing to show it: every check in this process shares the claim it
// checks, so only a second process in the gap sees it go. open is a parameter so a
// case can put one there ([TestTheBootHoldsItsStoreUntilTheEngineHasOpenedIt]).
func bootEngine(ctx context.Context, boot *config.Bootstrap, seed tierBSeed,
	stored storedReader, open engineOpener, log *slog.Logger) (*engine.Engine, seedPlan, error) {

	plan, err := decideSeed(ctx, boot, seed, stored, log)
	if err != nil {
		return nil, seedPlan{}, err
	}
	held := plan.held
	plan.held = heldStore{}
	defer held.release()
	// The options go to the engine WHOLE, so no field of them can be
	// decided there and dropped here.
	opts, err := bootOptions(boot, plan)
	if err != nil {
		return nil, seedPlan{}, err
	}
	log.InfoContext(ctx, "engine_starting", "version", version.String(),
		"company", plan.startsOn(), "seed", string(plan.act), "seed_file", plan.file.Path)
	e, err := open(ctx, opts)
	if err != nil {
		return nil, seedPlan{}, err
	}
	return e, plan, nil
}
