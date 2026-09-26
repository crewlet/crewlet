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
		return publishLocalActive(ctx, plane, pub, configs, active, log)
	}
	document, err := json.Marshal(seed.Company)
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
			return publishLocalActive(ctx, plane, pub, configs, active, log)
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
			return publishLocalActive(ctx, plane, pub, configs, active, log)
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
	// ONE INSTANT FOR BOTH HALVES. The local row's `activated_at` is what
	// this node boots on next time, and the chart apply stamps every
	// project with it — so it has to be the instant the pointer carries,
	// or a restart reads a different epoch from the one the fleet applied.
	at := time.Now().UTC()
	// STORED FIRST, then pointed at: a crash between the two leaves a
	// revision nothing points at, which the next boot re-seeds over. The
	// other order would point the fleet at a payload no node can read.
	id, err := configs.InsertActive(ctx, store.Revision{
		ParentID: parent, Source: "file", CreatedBy: "node",
		Summary: summary, Payload: payload, CreatedAt: at,
	})
	if err != nil {
		return fmt.Errorf("seed the company config: %w", err)
	}
	// UNCONDITIONAL, and that is the seed's whole posture: it is asserting
	// what this node booted with, not editing something it read. There is
	// no earlier revision it derived from, so there is nothing it could
	// have raced with — and an expectation here would make a first boot
	// fail against a fleet that had moved on for perfectly good reasons.
	published, err := plane.Activate(ctx, coord.ActivationRequest{
		RevisionID: id, Summary: summary, Payload: payload, At: at,
	})
	if err != nil {
		return fmt.Errorf("activate the seeded company config: %w", err)
	}
	keepPointersInstant(ctx, configs, id, at, published, log)
	nudge(ctx, pub, id, summary, log)
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
	configs localActivator, active store.Revision, log *slog.Logger,
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
	published, err := plane.Activate(ctx, coord.ActivationRequest{
		RevisionID: active.ID, Summary: active.Summary,
		Payload: active.Payload, At: active.ActivatedAt,
	})
	if err != nil {
		return fmt.Errorf("publish the active revision: %w", err)
	}
	keepPointersInstant(ctx, configs, active.ID, active.ActivatedAt, published, log)
	nudge(ctx, pub, active.ID, active.Summary, log)
	log.InfoContext(ctx, "local_revision_published", "revision", active.ID, "epoch", published.Epoch)
	return nil
}

// keepPointersInstant makes this node's local copy of a revision it just
// published carry the instant the pointer does.
//
// The pointer publishes an instant later than the one it replaces
// ([coord.ActivationAt]), so it can differ from the one this node asked for —
// and the local row's `activated_at` is what this node boots its chart with
// next time, which has to be the instant the fleet applied.
//
// BEST EFFORT, because the activation has landed and cannot be taken back:
// the reconciler realigns the local copy with the pointer on every tick, so a
// failure here costs one tick of a stale local instant.
func keepPointersInstant(ctx context.Context, configs localActivator, id string,
	asked time.Time, published coord.Activation, log *slog.Logger) {

	if store.EncodeTime(asked) == store.EncodeTime(published.At) {
		return
	}
	if _, err := configs.Activate(ctx, id, published.At); err != nil {
		log.WarnContext(ctx, "local_revision_instant_not_aligned",
			"revision", id, "asked", asked, "published", published.At, "error", err,
			"detail", "the pointer carries a later instant than this node's copy; "+
				"the reconciler aligns the copy on its next tick")
	}
}

// localActivator is the one thing [keepPointersInstant] asks of the node's
// config history.
type localActivator interface {
	Activate(ctx context.Context, revisionID string, at time.Time) (string, error)
}

// nudge announces an activation this node made, so peers converge in
// milliseconds instead of at their next poll.
//
// BEST EFFORT, and thin by design: the event carries no payload, because the
// authoritative path is the pointer and a node acts by re-reading it. Losing
// one costs a reconcile interval and never a revision.
func nudge(ctx context.Context, pub queue.Publisher, revisionID, summary string, log *slog.Logger) {
	if pub == nil {
		return
	}
	ev := events.New(types.ConfigRevisionActivated{
		RevisionID: revisionID, RevisionSummary: summary, CreatedBy: "node",
	}, tracing.TraceOf(ctx))
	if err := pub.Publish(ctx, topics.ConfigRevisionActivated, ev); err != nil {
		log.WarnContext(ctx, "activation_nudge_not_published", "revision", revisionID,
			"error", err, "detail", "peers converge on their reconcile interval instead")
	}
}

// bootOptions is what `crewlet run` builds its engine from: the Tier A it
// loaded, the company this node starts on, and the instant that company was
// activated at.
//
// With a Tier B file the company is that file's, which the reconcile converges
// onto the fleet's before anything is claimed — and a file's company has NO
// ACTIVATION YET, so its instant stays zero and the engine leaves its chart to
// the reconciler's first tick, which publishes the file and applies it with
// the pointer's own instant. Without one it is whatever this node's store has
// marked active, WITH the instant it was activated at ([companyFromStore]),
// which is what the engine stamps the chart and the knowledge spaces with at
// boot ([engine.Options.ActivatedAt]): a boot that dropped it would stamp
// nothing at all, and the company's projects would wait for the next apply.
//
// Read BEFORE the engine, in its own open-and-close, so the engine still owns
// the backends it opens.
func bootOptions(ctx context.Context, bootstrapPath string, boot *config.Bootstrap,
	file *config.Company) (engine.Options, error) {

	opts := engine.Options{Bootstrap: boot, Company: file}
	if file != nil {
		return opts, nil
	}
	// A SCRATCH STORE IS NOT READ: what it holds is the previous run's, it is
	// deleted as the engine opens it, and opening it here would create the
	// replicated estate a node without `data` must not have. Such a node
	// boots unconfigured and its reconciler fetches the fleet's active
	// revision off the coordination store before any seat is claimed.
	if boot.Store.Scratch {
		return opts, nil
	}
	company, activatedAt, err := companyFromStore(ctx, bootstrapPath)
	if err != nil {
		return engine.Options{}, err
	}
	opts.Company, opts.ActivatedAt = company, activatedAt
	return opts, nil
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
	return fmt.Errorf("%s: this node's store is scratch (store.scratch) — it holds "+
		"no data and is deleted at every boot, so nothing written into it offline "+
		"survives the next start. %s", command, instead)
}

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
// It also returns WHEN the revision was activated, which the engine stamps
// the company's chart with at boot ([engine.Options.ActivatedAt]): the
// revision's own `activated_at`, which the reconciler keeps equal to the
// activation pointer's instant for the revision the fleet is on.
//
// It opens the store, reads, and closes it again, rather than handing the open
// handle on: the engine opens its own backends and owns their lifetime, and a
// caller-supplied set would move that ownership into this function's caller
// for the whole run. Two sequential opens at boot cost one migration pass over
// an already-migrated file; a split lifetime costs a leaked store on every
// error path that does not know it now has one.
func companyFromStore(ctx context.Context, bootstrapPath string) (*config.Company, time.Time, error) {
	cs, closeStore, err := openConfigStore(ctx, bootstrapPath)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer closeStore()

	active, found, err := cs.configs.Active(ctx)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("read the active revision: %w", err)
	}
	if !found {
		return nil, time.Time{}, nil
	}
	document, err := secrets.Open(cs.cipher, active.Payload)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("open the active revision %s: %w", active.ID, err)
	}
	company, err := config.DecodeCompany(document)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("parse the active revision %s: %w", active.ID, err)
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
		return nil, time.Time{}, fmt.Errorf("the active revision %s cannot run on this build; "+
			"import a corrected document with `crewlet config import` and start "+
			"again: %w", active.ID, err)
	}
	return company, active.ActivatedAt, nil
}
