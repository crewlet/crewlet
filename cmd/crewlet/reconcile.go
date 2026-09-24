package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
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
// `-company` imports only into an EMPTY store. It used to import whenever its
// content differed from the active revision, which quietly made a restart a
// write: an operator edits the company live, restarts a node whose file is a
// month old, and the file wins — a deleted role returns, a changed model
// reverts, and nothing says so.
//
// The rule that replaced it is not "silently ignore an edited file", which was
// the failure the old behaviour was itself guarding against. An edited file
// that is not imported SAYS SO, at warn, naming both revisions and the two
// ways to apply it. `-import-company` is the deliberate override, and
// `crewlet config import` changes a running fleet with no restart at all.
//
// # Why the comparison is against the opened document
//
// The seed is compared against the ACTIVE revision's opened document, not
// against its stored bytes: with a keyring configured the stored form is
// ciphertext and differs on every seal, so a byte comparison would import a
// fresh revision on every single boot.

// startReconciler seeds the store from the file, converges this node on the
// pointer, and returns the loop for the caller to run.
func startReconciler(ctx context.Context, e *engine.Engine, boot *config.Bootstrap,
	seed tierBSeed, cipher secrets.Cipher, log *slog.Logger,
) (*engine.Reconciler, error) {
	db := e.Backends().Store
	plane := e.Backends().Fleet
	if err := seedCompany(ctx, db, plane, e.Backends().Queue, seed, cipher, log); err != nil {
		return nil, err
	}

	nodeID, err := config.ResolveNodeID(boot, nil)
	if err != nil {
		return nil, fmt.Errorf("node identity: %w", err)
	}
	reconciler, err := e.NewReconciler(engine.ReconcilerOptions{
		Store: db, Fleet: plane, Queue: e.Backends().Queue,
		NodeID: nodeID, Cipher: cipher,
		OnApply: func(epoch int64, status configplane.ApplyStatus) {
			log.InfoContext(ctx, "config_revision_applied",
				"epoch", epoch, "status", string(status))
		},
	})
	if err != nil {
		return nil, err
	}
	// One tick now, so the node boots on the epoch the fleet is on. Its
	// failure is NOT fatal: a node that cannot reach the current revision
	// still serves the one it has, which is the whole point of publishing
	// an epoch rather than mutating one.
	if err := reconciler.Tick(ctx); err != nil {
		log.WarnContext(ctx, "initial_reconcile_failed", "error", err,
			"hint", "this node is serving the configuration it booted with; "+
				"it will keep trying on its reconcile interval")
	}
	return reconciler, nil
}

// seedCompany reconciles the command line's Tier B file into the store.
//
// Idempotent by CONTENT, not by a marker: an unchanged file imports nothing
// however many times the node boots. What happens to a CHANGED one is the
// seed's mode — see the package note above. A bootstrap seed reports the
// difference and leaves the store alone; an override imports it.
func seedCompany(ctx context.Context, db *store.DB, plane coord.Plane, pub queue.Publisher,
	seed tierBSeed, cipher secrets.Cipher, log *slog.Logger,
) error {
	configs := db.Configs()
	active, found, err := configs.Active(ctx)
	if err != nil {
		return fmt.Errorf("read the active revision: %w", err)
	}
	if seed.Company == nil {
		// No file at all. The store is authoritative, so this is a normal
		// way to run a node — it serves whatever the fleet activated.
		if !found {
			return nil
		}
		return publishLocalActive(ctx, plane, pub, active, log)
	}
	// THE SETTINGS HALF, because that is what a revision holds. The file
	// carries both and always will — an operator authors one document
	// describing a company — and the two halves land on this node by two
	// paths: the settings here, and the org chart from the SAME file
	// through the engine's boot seed, which publishes it to the chart log
	// when that log is empty.
	//
	// Storing the whole file instead would store a revision every node
	// then refuses to apply ([config.DecodeSettings]), including this one
	// on its next restart — a company that boots once and never again.
	document, err := json.Marshal(config.SettingsOf(seed.Company).Company())
	if err != nil {
		return fmt.Errorf("encode the company config: %w", err)
	}
	parent := ""
	if found {
		//nolint:govet // shadow: scoped to this block; see .golangci.yml
		current, err := secrets.Open(cipher, active.Payload)
		if err != nil {
			return fmt.Errorf("open the active revision: %w", err)
		}
		if bytes.Equal(current, document) {
			// The file has not changed, so there is nothing to import.
			// The node may still owe the fleet a POINTER — see
			// publishLocalActive for the case that puts it there.
			return publishLocalActive(ctx, plane, pub, active, log)
		}
		if !seed.Override {
			// THE COMPANY EXISTS AND THIS FILE IS ONLY A BOOTSTRAP, so
			// the store wins. Loud, because the operator edited a file
			// that is not going to take effect and the old behaviour's
			// one virtue was that it always did something.
			log.WarnContext(ctx, "company_seed_ignored",
				"file", seed.Path, "active_revision", active.ID,
				"reason", "the store already holds a company, and -company "+
					"only bootstraps an empty one",
				"hint", "to make this file the company again, restart with "+
					"-import-company "+seed.Path+"; to change the running "+
					"fleet with no restart, run `crewlet config import "+
					seed.Path+"`")
			return publishLocalActive(ctx, plane, pub, active, log)
		}
		parent = active.ID
	}

	// THE FILE IS BEING WRITTEN, so it meets every rule a written document
	// meets. It was loaded against the runnable rules only, because the
	// frames above return without writing it: unchanged from the active
	// revision, or a bootstrap the store's own company outranks. Past them
	// this is a new revision, the same act as `crewlet config import`, and
	// admitting a duplicate here would store exactly what every other
	// write refuses. Nothing has been stored or activated yet.
	if invalid := seed.Company.ValidateAdmission(); invalid != nil {
		return fmt.Errorf("company config %s cannot be imported into the store: "+
			"a document written as a new revision must meet every rule, and "+
			"this one breaks an admission rule. Correct the file and start "+
			"again: %w", seed.Path, invalid)
	}
	payload, err := secrets.Seal(cipher, document)
	if err != nil {
		return err
	}
	summary := "seeded from " + seed.Path
	if seed.Override {
		summary = "imported from " + seed.Path + " at boot"
	}
	// STORED FIRST, then pointed at: a crash between the two leaves a
	// revision nothing points at, which the next boot re-seeds over. The
	// other order would point the fleet at a payload no node can read.
	// THE ENGINE IS THE AUTHOR: a boot imported this file, and no
	// credential made the write.
	seeded := store.Revision{
		ParentID: parent, Source: "file", CreatedBy: seedAuthor,
		CreatedByKind: string(iam.ActorSystem), Summary: summary, Payload: payload,
	}
	id, err := configs.InsertActive(ctx, seeded)
	if err != nil {
		return fmt.Errorf("seed the company config: %w", err)
	}
	seeded.ID = id
	// UNCONDITIONAL, and that is the seed's whole posture: it is asserting
	// what this node booted with, not editing something it read. There is
	// no earlier revision it derived from, so there is nothing it could
	// have raced with — and an expectation here would make a first boot
	// fail against a fleet that had moved on for perfectly good reasons.
	published, err := plane.Activate(ctx, coord.ActivationRequest{
		RevisionID: id, Summary: summary, Payload: payload, At: time.Now().UTC(),
		CreatedBy: seeded.CreatedBy, CreatedByKind: seeded.CreatedByKind,
	})
	if err != nil {
		return fmt.Errorf("activate the seeded company config: %w", err)
	}
	nudge(ctx, pub, seeded, log)
	log.InfoContext(ctx, "company_config_seeded", "revision", id, "epoch", published.Epoch,
		"parent", parent, "sealed", cipher != nil)
	return nil
}

// publishLocalActive points the fleet at this node's active revision when the
// fleet has no pointer, or an OLDER one.
//
// This is what makes an offline `crewlet config import` reach the fleet. The
// activation pointer lives in the coordination store, and on the default
// embedded topology that store is inside the engine's own process — so a
// command run while the engine is stopped cannot move it. It marks the
// revision active in the node's own database instead, and this publishes it
// at the next start.
//
// # The pointer wins when it is NEWER
//
// A node booting into a running fleet must converge on what the fleet already
// decided, not overwrite it with whatever its local database happens to say —
// that would let one restarted node roll a company back. So the local
// revision is published only when the fleet has nothing, or when it was
// activated AFTER the pointer was.
//
// Both instants are written by the same node in the offline case, which is
// the case this exists for. Across a fleet they come from different clocks,
// and the consequence is bounded and stated: a node whose clock is ahead can
// republish its own revision once, which every peer then converges on — the
// same outcome as an operator re-activating it deliberately.
func publishLocalActive(ctx context.Context, plane coord.Plane, pub queue.Publisher,
	active store.Revision, log *slog.Logger,
) error {
	target, found, err := plane.Target(ctx)
	if err != nil {
		return fmt.Errorf("read the activation pointer: %w", err)
	}
	switch {
	case found && target.RevisionID == active.ID:
		return nil
	case found && !active.ActivatedAt.After(target.At):
		// The fleet is on something else and decided it later. Converging
		// on the pointer is the reconciler's job, not this function's.
		log.InfoContext(ctx, "local_revision_superseded", "local", active.ID,
			"fleet", target.RevisionID, "epoch", target.Epoch,
			"detail", "this node converges on the fleet's revision")
		return nil
	}
	// The payload goes up with it. This node is telling the fleet to serve
	// a revision only it holds, so the body has to travel or every peer
	// converges on a pointer whose revision it cannot read.
	// UNCONDITIONAL for the same reason as the seed. Two nodes booting at
	// once may both decide to publish, and last-write-wins is the right
	// answer there: each is offering the revision IT holds, both are
	// legitimate, and every node converges on whichever landed. A
	// compare-and-set would turn that into a boot-time failure to retry
	// for nothing. It is the EDIT path that must not lose a write.
	// THE REVISION'S OWN AUTHOR travels with it — the operator who ran the
	// offline import, most often — so every peer that adopts it records who
	// wrote it rather than the node that happened to publish it.
	published, err := plane.Activate(ctx, coord.ActivationRequest{
		RevisionID: active.ID, Summary: active.Summary,
		Payload: active.Payload, At: active.ActivatedAt,
		CreatedBy: active.CreatedBy, CreatedByKind: active.CreatedByKind,
		OperatorID: active.OperatorID,
	})
	if err != nil {
		return fmt.Errorf("publish the active revision: %w", err)
	}
	nudge(ctx, pub, active, log)
	log.InfoContext(ctx, "local_revision_published", "revision", active.ID, "epoch", published.Epoch)
	return nil
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
		CreatedBy: revision.CreatedBy, CreatedByKind: revision.CreatedByKind,
		OperatorID: revision.OperatorID,
	}, tracing.TraceOf(ctx))
	if err := pub.Publish(ctx, topics.ConfigRevisionActivated, ev); err != nil {
		log.WarnContext(ctx, "activation_nudge_not_published", "revision", revision.ID,
			"error", err, "detail", "peers converge on their reconcile interval instead")
	}
}

// seedAuthor is who a revision this node imported from its own command line
// at boot records as its author: the engine, of the system kind, with no
// credential beside it — none made the write.
const seedAuthor = "node"

// companyFromStore is the epoch a node with no Tier B file boots on.
//
// The store is authoritative at runtime, so a node whose company already lives
// there needs no file — and the documented bootstrap path (start the node,
// then PUT /config) requires that it can start without one. What it cannot do
// is guess: it reads the revision the node's own database marks active, which
// is what the reconciler is about to converge from anyway.
//
// A nil company with a nil error is the UNCONFIGURED case — no file and no
// revision — and it is a state, not a failure. The node serves its API so an
// operator can push the first revision into it.
//
// It opens the store, reads, and closes it again, rather than handing the open
// handle on: the engine opens its own backends and owns their lifetime, and a
// caller-supplied set would move that ownership into this function's caller
// for the whole run. Two sequential opens at boot cost one migration pass over
// an already-migrated file; a split lifetime costs a leaked store on every
// error path that does not know it now has one.
func companyFromStore(ctx context.Context, bootstrapPath string) (*config.Company, error) {
	cs, closeStore, err := openConfigStore(ctx, bootstrapPath)
	if err != nil {
		return nil, err
	}
	defer closeStore()

	active, found, err := cs.configs.Active(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the active revision: %w", err)
	}
	if !found {
		return nil, nil
	}
	document, err := secrets.Open(cs.cipher, active.Payload)
	if err != nil {
		return nil, fmt.Errorf("open the active revision %s: %w", active.ID, err)
	}
	// AS SETTINGS: a stored revision is the company's settings and the org
	// chart is the state log's own domain. A revision written before the
	// split is refused rather than applied with its chart dropped — see
	// [config.DecodeSettings], whose refusal names the repair.
	company, err := config.DecodeSettingsAsCompany(document)
	if err != nil {
		return nil, fmt.Errorf("parse the active revision %s: %w", active.ID, err)
	}
	// VALIDATED HERE, naming the revision, because booting is applying and
	// the stored-form decode holds a revision to no rule. The engine checks
	// the same rules again as it builds the epoch, but only this frame knows
	// which revision it is and that an offline import is the way out: the
	// node is not serving its API yet.
	//
	// The RUNNABLE rules only: a stored revision that breaks an admission
	// rule added after it was written still runs, and the reconciler warns
	// about it once it applies the epoch (see
	// [config.Company.ValidateRunnable]).
	if err := company.ValidateRunnable(); err != nil {
		return nil, fmt.Errorf("the active revision %s cannot run on this build; "+
			"import a corrected document with `crewlet config import` and start "+
			"again: %w", active.ID, err)
	}
	return company, nil
}
