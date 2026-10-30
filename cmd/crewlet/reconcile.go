package main

import (
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
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tracing"
)

// The company config a running node serves comes from the STORE, not from the
// file on the command line. That file is a SEED, reconciled into the store
// like any other change.
//
// Both halves of that are load-bearing. Without the seed, a first run has
// nothing to activate and the node serves a company no peer can see. Without
// the store, a PUT /config on one node is invisible to every other — which is
// the fan-out failure the whole control plane exists to remove.
//
// # A bootstrap seed does not overwrite a company that exists
//
// `-company` imports only where NO COMPANY EXISTS: none in this node's store
// and none the fleet runs, the fleet's half decided create-only at the
// pointer. It used to import whenever its content differed from the active
// revision, which quietly made a restart a write: an operator edits the
// company live, restarts a node whose file is a month old, and the file wins
// — a deleted role returns, a changed model reverts, and nothing says so. And
// it used to decide "no company" from this node's store alone, which a fresh
// node joining a fleet and every boot of a node without `data` find empty:
// such a node replaced the fleet's running company with its file, on every
// node at once.
//
// The rule that replaced it is not "silently ignore an edited file", which was
// the failure the old behaviour was itself guarding against. An edited file
// that is not imported SAYS SO, at warn, naming both revisions and the two
// ways to apply it. `-import-company` is the deliberate override, and
// `crewlet config import` changes a running fleet with no restart at all.
//
// WHICH of those a file is, and what the engine boots on, is decided once,
// before the engine starts — see seed.go. This file carries the decision out.
//
// The seed is compared against a revision's opened document, not its stored
// bytes — see [tierBSeed.is].

// startReconciler carries out the decided seed, converges this node on the
// pointer, and returns the loop for the caller to run.
//
// It takes the PLAN rather than the file: what the file is for was decided
// before the engine started, by the decision the engine booted on too
// ([decideSeed]), and a second reading of the file here is how the two once
// disagreed.
func startReconciler(ctx context.Context, e *engine.Engine, boot *config.Bootstrap,
	plan seedPlan, cipher secrets.Cipher, log *slog.Logger,
) (*engine.Reconciler, error) {
	db := e.Backends().Store
	plane := e.Backends().Fleet
	// THE NODE'S IDENTITY FIRST, because a seed is this node's own write and
	// the revision records which node made it.
	nodeID, err := config.ResolveNodeID(boot, nil)
	if err != nil {
		return nil, fmt.Errorf("node identity: %w", err)
	}
	if err = seedCompany(ctx, db, plane, e.Backends().Queue, plan, cipher,
		nodeID, e.Buildable, log); err != nil {
		return nil, err
	}
	reconciler, err := e.NewReconciler(ctx, engine.ReconcilerOptions{
		Store: db, Fleet: plane, Queue: e.Backends().Queue,
		NodeID: nodeID,
		// WHAT THE ENGINE STARTED ON, which is what it serves until an
		// apply succeeds: the fleet view's row says so while this node
		// refuses the fleet's revision, and the boot row moves only from it.
		Booted: plan.boot,
		OnApply: func(epoch int64, status configplane.ApplyStatus) {
			log.InfoContext(ctx, "config_revision_applied",
				"epoch", epoch, "status", string(status))
		},
	})
	if err != nil {
		return nil, err
	}
	// One tick now, so the node boots on the epoch the fleet is on — and on
	// a node that booted unconfigured to import its file, this is where
	// that file's company is built. Its failure is NOT fatal: a node that
	// cannot reach the current revision still serves the one it has, which
	// is the whole point of publishing an epoch rather than mutating one.
	if err := reconciler.Tick(ctx); err != nil {
		log.WarnContext(ctx, "initial_reconcile_failed", "error", err,
			"hint", reconcileHint(boot, e.Company() != nil))
	}
	return reconciler, nil
}

// reconcileHint is what a failed first reconcile tells the operator, which
// depends on whether the node has a company to fall back on.
//
// A NODE THAT BOOTED UNCONFIGURED HAS NONE — every node that imported its file
// at this boot, and every node without `data` — so "the configuration it
// booted with" would be a company it never had. What it runs instead is
// nothing, until an apply lands; a revision the import wrote was built before
// it was written ([engine.Engine.Buildable]), so what failed here is a part
// the apply starts, which the next tick starts again, or a fleet revision this
// node did not write. Where the document itself is what must change, the way
// to change it differs on a managed deployment, whose writers are named.
func reconcileHint(boot *config.Bootstrap, configured bool) string {
	if configured {
		return "this node is serving the configuration it booted with; " +
			"it will keep trying on its reconcile interval"
	}
	correct := "correct it with `crewlet config import` through any node's API"
	if boot.API.Auth.CompanyManaged() {
		correct = "the managing system (api.auth.company_writers: " +
			strings.Join(boot.API.Auth.CompanyWriters, ", ") + ") writes a corrected one"
	}
	return "this node booted with no company and serves none until the fleet's " +
		"revision applies here; it will keep trying on its reconcile interval. " +
		"If the error names the revision itself rather than something this node " +
		"could not start, " + correct
}

// seedCompany carries out the decided seed against the store and the fleet.
//
// Idempotent by CONTENT, not by a marker: an unchanged file imports nothing
// however many times the node boots. What happens to a CHANGED one is the
// seed's act: a bootstrap a company outranks imports nothing — the store's
// company, decided with the boot ([decideSeed]), or the fleet's, decided here
// where its pointer can be read — and an override imports it.
//
// A revision the seed writes is the NODE's, recorded under the node's own id:
// nobody ran a command, the node read its -company file at boot, and which
// node did it is the fact somebody reading the history later needs.
func seedCompany(ctx context.Context, db *store.DB, plane coord.Plane, pub queue.Publisher,
	plan seedPlan, cipher secrets.Cipher, nodeID string, build revisionBuild, log *slog.Logger,
) error {
	if build == nil {
		// REFUSED, never read as "no check": an import with nothing to build
		// the file first would store and activate a company this node may
		// not be able to run. See [importSeed].
		return errors.New("the Tier B seed has no build to hold its file to; " +
			"this is a defect in `crewlet run`, not in either config file")
	}
	configs := db.Configs()
	switch plan.act {
	case seedBootstrap:
		// ONLY WHERE NO COMPANY EXISTS, and this node's store is the half
		// of that the plan could read: it found it empty, or it is a
		// scratch one the engine discarded as it opened it. The FLEET is
		// the other half, and an empty store says nothing about it — a
		// fresh data node joining a running fleet has one, and so does
		// every boot of a node without `data` — so it is asked here, where
		// its pointer can be read, and the write is create-only.
		target, pointed, err := plane.Target(ctx)
		if err != nil {
			return fmt.Errorf("read the activation pointer: %w", err)
		}
		if pointed {
			fleetOutranks(ctx, plane, plan.file, target, cipher, log)
			return nil
		}
		return importSeed(ctx, configs, plane, pub, plan.file, store.Revision{}, cipher,
			nodeID, build, log)
	case seedImport:
		return importOverride(ctx, configs, plane, pub, plan.file, cipher, nodeID, build, log)
	case seedNone:
		// No file to import. The store is authoritative, so this is a
		// normal way to run a node — it serves whatever the fleet
		// activated, publishes an offline change it boots on over the
		// revision that change was built on, and restores the company it
		// knows where the fleet's pointer is gone.
		return publishBoot(ctx, plane, pub, configs, plan.boot, plan.fleets, cipher,
			time.Now, log)
	default:
		return errUndecidedSeed
	}
}

// importOverride is -import-company: the file replaces whatever the fleet runs,
// unless the fleet already runs it.
//
// AGAINST THE REVISION THE FLEET RUNS, which is what the override replaces:
// compared with this node's own copy, a node that was down while a peer changed
// the company, and restarted with -import-company naming its old file, found
// the file equal to that stale copy and replaced nothing. With no pointer, the
// fleet's company is the one this node knows ([store.Configs.Restorable]), and
// a file that IS that revision is that revision published again rather than a
// copy of it imported.
func importOverride(ctx context.Context, configs *store.Configs, plane coord.Plane,
	pub queue.Publisher, seed tierBSeed, cipher secrets.Cipher, nodeID string,
	build revisionBuild, log *slog.Logger,
) error {
	target, pointed, err := plane.Target(ctx)
	if err != nil {
		return fmt.Errorf("read the activation pointer: %w", err)
	}
	known, knows, err := configs.Restorable(ctx)
	if err != nil {
		return fmt.Errorf("read the company this node knows: %w", err)
	}
	// BUILT ON WHAT IT REPLACES: the fleet's revision where the pointer can
	// be opened, kept as this node's history first, as an API write keeps
	// its base; otherwise the company this node knows.
	var base store.Revision
	if knows {
		base = known
	}
	if pointed {
		same, fleets, uncompared := seed.isFleets(ctx, plane, target, cipher)
		if uncompared != nil {
			// NOT SHOWN TO BE THE FILE, so it is replaced, as the
			// override asks — and said, because a fleet revision this
			// node cannot open is usually a keyring that differs from the
			// one the fleet sealed it under.
			log.WarnContext(ctx, "company_import_uncompared", "file", seed.Path,
				"fleet_revision", target.RevisionID, "epoch", target.Epoch,
				"error", uncompared,
				"detail", "the file replaces the fleet's revision without a comparison")
		} else {
			base = fleets
		}
		if same {
			log.InfoContext(ctx, "company_import_unchanged", "file", seed.Path,
				"fleet_revision", target.RevisionID, "epoch", target.Epoch,
				"detail", "the fleet already runs this file; nothing is imported")
			return nil
		}
		return importSeed(ctx, configs, plane, pub, seed, base, cipher, nodeID, build, log)
	}
	if knows {
		current, err := openStored(cipher, known)
		if err != nil {
			// NOT SHOWN TO BE THE FILE, so it is replaced, as the override
			// asks, exactly as a fleet revision this node cannot open is:
			// a company stored unsealed or under a key this keyring has
			// dropped is the state this flag exists to repair, and refusing
			// the boot over it left the operator no way out but the store.
			log.WarnContext(ctx, "company_import_uncompared", "file", seed.Path,
				"known_revision", known.ID, "error", err,
				"detail", "the file replaces the company this node knows without a comparison")
			return importSeed(ctx, configs, plane, pub, seed, base, cipher, nodeID, build, log)
		}
		same, err := seed.is(current)
		if err != nil {
			return err
		}
		if same {
			// THE FILE IS THE COMPANY THIS NODE KNOWS and the fleet has
			// no pointer, so there is nothing to import — only a pointer
			// this node owes the fleet, and the override's posture says
			// how: unconditionally.
			return publishOverride(ctx, plane, pub, configs, known, log)
		}
	}
	return importSeed(ctx, configs, plane, pub, seed, base, cipher, nodeID, build, log)
}

// publishOverride points the fleet at a revision this node holds,
// unconditionally: -import-company naming a file that is that revision.
//
// A CHOICE MADE NOW, at this node's clock, never at the revision's own instant.
// Stamped with the instant the revision last activated, the operator's choice
// read as the company the fleet ran back then: a peer that remembers the fleet
// moving on, restoring a pointer lost again later, found it older than what it
// remembered, and the override lost to that memory (bootpublish.go, 2b).
func publishOverride(ctx context.Context, plane coord.Plane, pub queue.Publisher,
	configs *store.Configs, revision store.Revision, log *slog.Logger,
) error {
	at := time.Now().UTC()
	published, err := plane.Activate(ctx, coord.ActivationRequest{
		RevisionID: revision.ID, Summary: revision.Summary, Payload: revision.Payload,
		At: at, Origin: originOf(revision),
	})
	if err != nil {
		return fmt.Errorf("publish revision %s: %w", revision.ID, err)
	}
	// AN OFFER IT BOOTS ON IS THE FLEET'S NOW, and stays what it boots on as
	// a revision rather than a choice; any other revision is history this
	// node noted the activation of.
	if revision.Offer != nil {
		if err := configs.ConfirmOffer(ctx, revision.ID); err != nil {
			log.WarnContext(ctx, "offline_revision_not_confirmed", "revision", revision.ID,
				"error", err, "detail", "the fleet runs it; this node boots on it once "+
					"its reconciler has applied it")
		}
	} else {
		landed(ctx, configs, revision, published, log)
	}
	nudge(ctx, pub, revision, log)
	log.InfoContext(ctx, "company_import_published", "revision", revision.ID,
		"epoch", published.Epoch,
		"detail", "the file is the company this node knows; it is published as it is")
	return nil
}

// fleetOutranks says that a -company file is not imported because the fleet
// already runs a company — unless the fleet runs this very file, which is no
// news: every node of a fleet started from one company.yaml carries it.
func fleetOutranks(ctx context.Context, plane coord.Plane, seed tierBSeed,
	target coord.Activation, cipher secrets.Cipher, log *slog.Logger) {

	same, _, err := seed.isFleets(ctx, plane, target, cipher)
	attrs := []any{"file", seed.Path, "fleet_revision", target.RevisionID,
		"epoch", target.Epoch,
		"reason", "the fleet already runs a company, and -company only " +
			"bootstraps a company that does not exist yet"}
	if err != nil {
		// NOT COMPARED, so not shown to be the file: said, with why.
		attrs = append(attrs, "compare_error", err)
	}
	seedOutranked(ctx, log, seed, same, attrs...)
}

// revisionBuild builds a company as this node's apply would build it before
// changing anything, and discards it ([engine.Engine.Buildable]).
type revisionBuild func(*config.Company) error

// importSeed writes the seed's file as a new revision built on base, and points
// the fleet at it. A zero base is a company built on nothing: a bootstrap, or
// an override on a node that knows no company.
func importSeed(ctx context.Context, configs *store.Configs, plane coord.Plane,
	pub queue.Publisher, seed tierBSeed, base store.Revision, cipher secrets.Cipher,
	nodeID string, build revisionBuild, log *slog.Logger,
) error {
	// A FILE STILL CARRYING A KEY THIS BUILD RETIRED IS NEVER WRITTEN, and
	// this is the one place a seed's file is. What it was read as is the
	// file without those keys, and a revision stored from it would drop
	// what they said without a word. Refused as every other door refuses
	// it, with the refusal the file earned when it was read, before
	// anything is built or stored.
	if seed.Retired != nil {
		return seed.Retired
	}
	document, err := json.Marshal(seed.Company)
	if err != nil {
		return fmt.Errorf("encode the company config: %w", err)
	}

	// THE FILE IS BEING WRITTEN, so it meets every rule a written document
	// meets. It was loaded against the runnable rules only, because most
	// boots never write it: unchanged from the revision this node boots on,
	// or a bootstrap a company that exists outranks. Here it is a new
	// revision, the same act as `crewlet config import`, and admitting a
	// duplicate here would store exactly what every other write refuses.
	// Nothing has been stored or activated yet.
	if invalid := seed.Company.ValidateAdmission(); invalid != nil {
		return fmt.Errorf("company config %s cannot be imported into the store: "+
			"a document written as a new revision must meet every rule, and "+
			"this one breaks an admission rule. Correct the file and start "+
			"again: %w", seed.Path, invalid)
	}
	// AND THIS NODE MUST BE ABLE TO BUILD IT, before anything is written:
	// the activation below is the whole fleet's, and nothing builds the
	// file until an apply does, after it has landed. A company valid on
	// paper can still fail to build here — a ${VAR} that resolves to
	// nothing where a backend refuses an empty value (an E2B key), a coding
	// CLI whose profile or directory cannot be had, a sandbox cell this
	// host cannot run — and written first it would be the fleet's company,
	// refused by this node's every apply. The engine booted on the file
	// once, which made the same refusal the boot's with nothing written;
	// this is that order kept.
	if err = build(seed.Company); err != nil {
		return fmt.Errorf("company config %s cannot be imported: this node cannot "+
			"build it, so nothing was stored or activated. Correct the file and "+
			"start again: %w", seed.Path, err)
	}
	payload, err := secrets.Seal(cipher, document)
	if err != nil {
		return err
	}
	summary := "seeded from " + seed.Path
	if seed.Override {
		summary = "imported from " + seed.Path + " at boot"
	}
	at := time.Now().UTC()
	// THE NODE IS THE AUTHOR: a boot imported this file, and no credential
	// made the write. Named by the node's own id, because which node read
	// its -company file is the fact somebody reading the history later
	// needs, and of the system kind, since nobody ran a command.
	by := iam.Actor{Name: nodeID, Kind: iam.ActorSystem}
	seeded := store.Revision{
		ParentID: base.ID, Source: "file", CreatedBy: by.Name,
		CreatedByKind: by.Kind, Summary: summary, Payload: payload,
		CreatedAt: at,
	}
	// STORED AS HISTORY FIRST, and what this node boots on only once its
	// engine has applied it — the order an edit through the API keeps. A
	// crash between the store and the activation leaves a revision nothing
	// points at, inert; one the fleet refused stays inert for good.
	id, err := configs.InsertOn(ctx, base, seeded)
	if err != nil {
		return fmt.Errorf("seed the company config: %w", err)
	}
	seeded.ID = id
	published, err := plane.Activate(ctx, coord.ActivationRequest{
		RevisionID: id, Summary: summary, Payload: payload, At: at,
		// THE ORIGIN RIDES THE POINTER, so every peer that keeps this
		// revision records the node that seeded it, from `file`, at the
		// instant it was written — rather than itself, at the instant it
		// happened to apply it.
		Origin: originOf(seeded),
		// A BOOTSTRAP IS CREATE-ONLY: it lands only while the fleet has no
		// activation at all, so of two nodes booting a fleet's first
		// company at once one wins and the other defers to it, and a node
		// that read no pointer an instant before a peer wrote one does not
		// replace that peer's company. An OVERRIDE is unconditional, and
		// that is its whole posture: it asserts what this node was told to
		// make the company, over whatever the fleet is running. It is the
		// only unconditional activation this build makes.
		ExpectAbsent: !seed.Override,
	})
	if errors.Is(err, coord.ErrActivationRaced) {
		// THE REVISION IS KEPT, inert, as history: the fleet's first
		// company is whichever landed, and this node converges on it.
		current, _, terr := plane.Target(ctx)
		if terr != nil {
			return fmt.Errorf("activate the seeded company config: %w; "+
				"and the pointer that won could not be read: %w", err, terr)
		}
		fleetOutranks(ctx, plane, seed, current, cipher, log)
		return nil
	}
	if err != nil {
		return fmt.Errorf("activate the seeded company config: %w", err)
	}
	landed(ctx, configs, seeded, published, log)
	nudge(ctx, pub, seeded, log)
	// THE KEY IT WAS SEALED UNDER, read off the envelope: a `sealed` flag
	// said `true` on every line, since nothing is stored unsealed.
	sealedUnder, _ := secrets.EnvelopeKeyIDOf(payload)
	log.InfoContext(ctx, "company_config_seeded", "revision", id, "epoch", published.Epoch,
		"parent", base.ID, "sealed_under", sealedUnder)
	return nil
}

// landed records an activation this node made in its history, at the
// pointer's instant, which may be later than the one it asked for
// ([coord.ActivationAt]).
//
// History only: the revision becomes what this node boots on when its engine
// applies it ([store.Configs.Serve]). BEST EFFORT, because the activation has
// landed and cannot be taken back, and the reconciler records the same instant
// when its tick meets the pointer.
func landed(ctx context.Context, configs activationNoter, revision store.Revision,
	published coord.Activation, log *slog.Logger) {

	if err := configs.NoteActivation(ctx, revision.ID, published.At); err != nil {
		log.WarnContext(ctx, "config_activation_not_noted",
			"revision", revision.ID, "epoch", published.Epoch, "error", err,
			"detail", "the fleet is running this revision; this node's history "+
				"shows the activation once its reconciler meets the pointer")
	}
}

// activationNoter is what [landed] asks of this node's config history.
type activationNoter interface {
	NoteActivation(ctx context.Context, revisionID string, at time.Time) error
}

// originOf is a stored revision's own record, as the pointer carries it: the
// author, its kind and the credential beside it, where the revision came from
// and when it was written.
func originOf(r store.Revision) coord.RevisionOrigin {
	return coord.RevisionOrigin{
		Author: r.CreatedBy, AuthorKind: r.CreatedByKind, OperatorID: r.OperatorID,
		Source: r.Source, CreatedAt: r.CreatedAt,
	}
}

// nudge announces an activation this node made, so peers converge in
// milliseconds instead of at their next poll.
//
// BEST EFFORT, and thin by design: the event carries no payload, because the
// authoritative path is the pointer and a node acts by re-reading it. Losing
// one costs a reconcile interval and never a revision.
//
// THE REVISION'S AUTHOR, not the node's: this said `node` for every
// activation it announced, including an operator's offline import published
// at the next start, so the audit feed credited the engine with a change a
// person made.
func nudge(ctx context.Context, pub queue.Publisher, revision store.Revision,
	log *slog.Logger) {

	if pub == nil {
		return
	}
	ev := events.New(types.ConfigRevisionActivated{
		RevisionID: revision.ID, RevisionSummary: revision.Summary,
		CreatedBy: revision.CreatedBy, CreatedByKind: string(revision.CreatedByKind),
		OperatorID: revision.OperatorID,
	}, tracing.TraceOf(ctx))
	if err := pub.Publish(ctx, topics.ConfigRevisionActivated, ev); err != nil {
		log.WarnContext(ctx, "activation_nudge_not_published", "revision", revision.ID,
			"error", err, "detail", "peers converge on their reconcile interval instead")
	}
}

// refuseScratchStore refuses an OFFLINE command against a node whose store is
// scratch, naming what to do instead.
//
// The store such a node keeps is deleted at its next boot, so anything written
// into it offline is lost without a word, and a read answers about a previous
// run nobody is running. Opening it would also create the replicated estate a
// node without `data` must not have.
func refuseScratchStore(boot *config.Bootstrap, command, instead string) error {
	if boot == nil || !boot.Store.Scratch {
		return nil
	}
	return fmt.Errorf("%s: this node's store is scratch (store.scratch): it holds "+
		"no data and is deleted at every boot, so nothing written into it offline "+
		"survives the next start. %s", command, instead)
}
