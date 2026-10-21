package main

import (
	"context"
	"encoding/json"
	"errors"
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
// The seed is compared against the ACTIVE revision's opened document, not its
// stored bytes — see [tierBSeed.is].

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
		nodeID, log); err != nil {
		return nil, err
	}
	reconciler, err := e.NewReconciler(engine.ReconcilerOptions{
		Store: db, Fleet: plane, Queue: e.Backends().Queue,
		NodeID: nodeID,
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
			"hint", "this node is serving the configuration it booted with; "+
				"it will keep trying on its reconcile interval")
	}
	return reconciler, nil
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
	plan seedPlan, cipher secrets.Cipher, nodeID string, log *slog.Logger,
) error {
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
		return importSeed(ctx, configs, plane, pub, plan.file, "", cipher, nodeID, log)
	case seedImport:
		target, pointed, err := plane.Target(ctx)
		if err != nil {
			return fmt.Errorf("read the activation pointer: %w", err)
		}
		active, found, err := configs.Active(ctx)
		if err != nil {
			return fmt.Errorf("read the active revision: %w", err)
		}
		switch {
		case pointed:
			// AGAINST THE REVISION THE FLEET RUNS, which is what the
			// override replaces: compared with this node's own copy, a
			// node that was down while a peer changed the company, and
			// restarted with -import-company naming its old file, found
			// the file equal to that stale copy and replaced nothing.
			same, uncompared := plan.file.isFleets(ctx, plane, target, cipher)
			if uncompared != nil {
				// NOT SHOWN TO BE THE FILE, so it is replaced, as the
				// override asks — and said, because a fleet revision
				// this node cannot open is usually a keyring that
				// differs from the one the fleet sealed it under.
				log.WarnContext(ctx, "company_import_uncompared", "file", plan.file.Path,
					"fleet_revision", target.RevisionID, "epoch", target.Epoch,
					"error", uncompared,
					"detail", "the file replaces the fleet's revision without a comparison")
			}
			if same {
				log.InfoContext(ctx, "company_import_unchanged", "file", plan.file.Path,
					"fleet_revision", target.RevisionID, "epoch", target.Epoch,
					"detail", "the fleet already runs this file; nothing is imported")
				return nil
			}
		case found:
			current, err := openStored(cipher, active)
			if err != nil {
				return err
			}
			same, err := plan.file.is(current)
			if err != nil {
				return err
			}
			if same {
				// The file IS this node's revision and the fleet has
				// none, so there is nothing to import — only a pointer
				// this node owes the fleet. See publishLocalActive.
				return publishLocalActive(ctx, plane, pub, configs, active, cipher, log)
			}
		}
		// CHAINED TO THE REVISION THIS NODE HOLDS, as an edit through its
		// API is: the parent is a foreign key into this node's own
		// history, which may not hold the fleet's newer revision yet.
		parent := ""
		if found {
			parent = active.ID
		}
		return importSeed(ctx, configs, plane, pub, plan.file, parent, cipher, nodeID, log)
	case seedNone:
		// No file to import. The store is authoritative, so this is a
		// normal way to run a node — it serves whatever the fleet
		// activated, and publishes what it holds where the fleet has
		// nothing newer.
		active, found, err := configs.Active(ctx)
		if err != nil {
			return fmt.Errorf("read the active revision: %w", err)
		}
		if !found {
			return nil
		}
		return publishLocalActive(ctx, plane, pub, configs, active, cipher, log)
	default:
		return errUndecidedSeed
	}
}

// fleetOutranks says that a -company file is not imported because the fleet
// already runs a company — unless the fleet runs this very file, which is no
// news: every node of a fleet started from one company.yaml carries it.
func fleetOutranks(ctx context.Context, plane coord.Plane, seed tierBSeed,
	target coord.Activation, cipher secrets.Cipher, log *slog.Logger) {

	same, err := seed.isFleets(ctx, plane, target, cipher)
	if same {
		return
	}
	attrs := []any{"file", seed.Path, "fleet_revision", target.RevisionID,
		"epoch", target.Epoch,
		"reason", "the fleet already runs a company, and -company only " +
			"bootstraps a company that does not exist yet",
		"hint", "to make this file the company, restart with -import-company " +
			seed.Path + "; to change the running fleet with no restart, run " +
			"`crewlet config import " + seed.Path + "`"}
	if err != nil {
		// NOT COMPARED, so not shown to be the file: said, with why.
		attrs = append(attrs, "compare_error", err)
	}
	log.WarnContext(ctx, "company_seed_ignored", attrs...)
}

// importSeed writes the seed's file as a new revision, chained to parent, and
// points the fleet at it.
func importSeed(ctx context.Context, configs *store.Configs, plane coord.Plane,
	pub queue.Publisher, seed tierBSeed, parent string, cipher secrets.Cipher,
	nodeID string, log *slog.Logger,
) error {
	document, err := json.Marshal(seed.Company)
	if err != nil {
		return fmt.Errorf("encode the company config: %w", err)
	}

	// THE FILE IS BEING WRITTEN, so it meets every rule a written document
	// meets. It was loaded against the runnable rules only, because most
	// boots never write it: unchanged from the active revision, or a
	// bootstrap a company that exists outranks. Here it is a new
	// revision, the same act as `crewlet config import`, and admitting a
	// duplicate here would store exactly what every other write refuses.
	// Nothing has been stored or activated yet.
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
	at := time.Now().UTC()
	// THE NODE IS THE AUTHOR: a boot imported this file, and no credential
	// made the write. Named by the node's own id, because which node read
	// its -company file is the fact somebody reading the history later
	// needs, and of the system kind, since nobody ran a command.
	by := iam.Actor{Name: nodeID, Kind: iam.ActorSystem}
	seeded := store.Revision{
		ParentID: parent, Source: "file", CreatedBy: by.Name,
		CreatedByKind: by.Kind, Summary: summary, Payload: payload,
		CreatedAt: at,
	}
	// STORED AS HISTORY FIRST, and made this node's active revision only
	// once the fleet has taken it — the order an edit through the API
	// keeps, for its reason: this node's active revision is what its next
	// boot publishes over the fleet's whenever it reads as the newer, so a
	// bootstrap that lost to a peer's first company and stayed active here
	// would be published over that company one restart later. A crash
	// between the two leaves a revision nothing points at, inert.
	id, err := configs.Insert(ctx, seeded)
	if err != nil {
		return fmt.Errorf("seed the company config: %w", err)
	}
	seeded.ID = id
	published, err := plane.Activate(ctx, coord.ActivationRequest{
		RevisionID: id, Summary: summary, Payload: payload, At: at,
		// THE ORIGIN RIDES THE POINTER, so every peer that adopts this
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
		// make the company, over whatever the fleet is running.
		ExpectAbsent: !seed.Override,
	})
	if errors.Is(err, coord.ErrActivationRaced) {
		// THE REVISION IS KEPT, inactive, as history: the fleet's first
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
	// THE FLEET TOOK IT, so it is this node's company too — AT THE
	// POINTER'S INSTANT, which may be later than the one asked for
	// ([coord.ActivationAt]): the local row's `activated_at` is what this
	// node's config history shows for the activation, what the reconciler
	// holds against the pointer's on every tick, and what the next boot
	// compares with the pointer to decide whether this node holds a newer
	// revision to publish. BEST EFFORT, because the activation has landed
	// and cannot be taken back: the reconciler marks the fleet's revision
	// active here when it applies the epoch.
	if _, err := configs.Activate(ctx, id, published.At); err != nil {
		log.WarnContext(ctx, "local_revision_not_marked_active",
			"revision", id, "epoch", published.Epoch, "error", err,
			"detail", "the fleet is running this revision; this node marks "+
				"it active when its reconciler applies the epoch")
	}
	nudge(ctx, pub, seeded, log)
	// THE KEY IT WAS SEALED UNDER, read off the envelope: a `sealed` flag
	// said `true` on every line, since nothing is stored unsealed.
	sealedUnder, _ := secrets.EnvelopeKeyIDOf(payload)
	log.InfoContext(ctx, "company_config_seeded", "revision", id, "epoch", published.Epoch,
		"parent", parent, "sealed_under", sealedUnder)
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
//
// # AUTHENTICATED BEFORE IT IS PUBLISHED
//
// What this publishes, every peer applies — and every peer refuses a body that
// is not sealed, because the seal is what says a node of this fleet wrote it.
// So a revision this node holds in the clear is refused HERE, where this
// node's operator can act on it, rather than published to be refused on every
// node.
func publishLocalActive(ctx context.Context, plane coord.Plane, pub queue.Publisher,
	configs localActivator, active store.Revision, cipher secrets.Cipher, log *slog.Logger,
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
	if _, err = openStored(cipher, active); err != nil {
		return fmt.Errorf("this node's active revision is not published to the "+
			"fleet: %w", err)
	}
	// The payload goes up with it. This node is telling the fleet to serve
	// a revision only it holds, so the body has to travel or every peer
	// converges on a pointer whose revision it cannot read.
	//
	// UNCONDITIONAL, unlike a bootstrap seed's create-only write, because
	// what it offers is a company somebody ACTIVATED here — an offline
	// `crewlet config import`, which is the case this exists for, or what
	// the fleet itself once pointed this node at — and it has just read
	// that the fleet holds nothing newer. A seed proposes a first company
	// from a file nobody activated, so it must not land over one that
	// exists; this asserts one that was. Two nodes booting at once may both
	// decide to publish, and last-write-wins is the right answer there:
	// each is offering the revision IT holds, both are legitimate, and
	// every node converges on whichever landed. A compare-and-set would
	// turn that into a boot-time failure to retry for nothing. It is the
	// EDIT path that must not lose a write.
	//
	// THE REVISION'S OWN ORIGIN goes up with it, never this node's: an
	// offline `crewlet config import` is an operator's write that only
	// reaches the fleet here, so every peer that adopts it records who
	// wrote it, through which credential, from where and when — rather
	// than the node that happened to publish it.
	published, err := plane.Activate(ctx, coord.ActivationRequest{
		RevisionID: active.ID, Summary: active.Summary,
		Payload: active.Payload, At: active.ActivatedAt,
		Origin: originOf(active),
	})
	if err != nil {
		return fmt.Errorf("publish the active revision: %w", err)
	}
	keepPointersInstant(ctx, configs, active.ID, active.ActivatedAt, published, log)
	nudge(ctx, pub, active, log)
	log.InfoContext(ctx, "local_revision_published", "revision", active.ID, "epoch", published.Epoch)
	return nil
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

// keepPointersInstant makes this node's local copy of a revision it just
// published carry the instant the pointer does.
//
// The pointer publishes an instant later than the one it replaces
// ([coord.ActivationAt]), so it can differ from the one this node asked for —
// and the local row's `activated_at` is the instant this node's config history
// shows for the activation and the one the reconciler holds against the
// pointer's. Left at the instant this node asked for, the row names an
// activation the fleet never made: the history dates it wrongly, and the
// reconciler's next tick reads the same revision at another instant — which
// is the re-activation gesture — and re-activates it locally to realign it.
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
	return fmt.Errorf("%s: this node's store is scratch (store.scratch) — it holds "+
		"no data and is deleted at every boot, so nothing written into it offline "+
		"survives the next start. %s", command, instead)
}
