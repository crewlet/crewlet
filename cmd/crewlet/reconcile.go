package main

import (
	"bytes"
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
// `-company` imports only into an EMPTY store, and only while the FLEET runs no
// company either: a node's store is empty at every boot when it is scratch,
// and on the day it joins a running fleet. It used to import whenever its
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
// against its stored bytes: every revision is sealed under the keyring every
// node holds, so the stored form is ciphertext that differs on every seal, and
// a byte comparison would import a fresh revision on every single boot.

// startReconciler seeds the store from the file, converges this node on the
// pointer, and returns the loop for the caller to run.
//
// The seed arrives DECIDED by [bootOptions] — a managed deployment's
// bootstrap file already dropped ([managedSeed]) — because the engine was
// booted from the same decision.
func startReconciler(ctx context.Context, e *engine.Engine, boot *config.Bootstrap,
	seed tierBSeed, cipher secrets.Cipher, log *slog.Logger,
) (*engine.Reconciler, error) {
	db := e.Backends().Store
	plane := e.Backends().Fleet
	// THE NODE'S IDENTITY FIRST, because a seed is this node's own write and
	// the revision records which node made it.
	nodeID, err := config.ResolveNodeID(boot, nil)
	if err != nil {
		return nil, fmt.Errorf("node identity: %w", err)
	}
	if err = seedCompany(ctx, db, plane, e.Backends().Queue, seed, cipher,
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
// difference and leaves the store alone, and into an empty store it stands
// down for a company the fleet already runs ([fleetOutranksSeed]); an
// override imports it.
//
// A revision the seed writes is the NODE's, recorded under the node's own id:
// nobody ran a command, the node read its -company file at boot, and which
// node did it is the fact somebody reading the history later needs.
func seedCompany(ctx context.Context, db *store.DB, plane coord.Plane, pub queue.Publisher,
	seed tierBSeed, cipher secrets.Cipher, nodeID string, log *slog.Logger,
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
		return publishLocalActive(ctx, plane, pub, configs, active, cipher, log)
	}
	document, err := json.Marshal(seed.Company)
	if err != nil {
		return fmt.Errorf("encode the company config: %w", err)
	}
	parent := ""
	if found {
		//nolint:govet // shadow: scoped to this block; see .golangci.yml
		current, err := openStored(cipher, active)
		if err != nil {
			return err
		}
		if bytes.Equal(current, document) {
			// The file has not changed, so there is nothing to import.
			// The node may still owe the fleet a POINTER — see
			// publishLocalActive for the case that puts it there.
			return publishLocalActive(ctx, plane, pub, configs, active, cipher, log)
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
			return publishLocalActive(ctx, plane, pub, configs, active, cipher, log)
		}
		parent = active.ID
	} else if !seed.Override {
		// AN EMPTY STORE IS NOT AN EMPTY COMPANY: see [fleetOutranksSeed].
		var outranked bool
		if outranked, err = fleetOutranksSeed(ctx, plane, cipher, seed, document,
			log); err != nil || outranked {
			return err
		}
	}

	// THE FILE IS BEING WRITTEN, so it meets every rule a written document
	// meets. It was loaded against the runnable rules only, because the
	// frames above return without writing it: unchanged from the active
	// revision, or a bootstrap the store's or the fleet's own company
	// outranks. Past them this is a new revision, the same act as `crewlet
	// config import`, and admitting a duplicate here would store exactly
	// what every other write refuses. Nothing has been stored or activated
	// yet.
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
	// STORED FIRST, then pointed at: a crash between the two leaves a
	// revision nothing points at, which the next boot re-seeds over. The
	// other order would point the fleet at a payload no node can read.
	//
	// STORED AS HISTORY ([store.Configs.Insert]), and made this node's
	// company only once the fleet has taken it — the API's write path, for
	// the API's reason: this node's active revision is what it boots on
	// next time and what it OFFERS THE FLEET whenever it is newer than the
	// pointer ([publishLocalActive]). Stored active, a seed the fleet
	// refused or never heard of was both: a bootstrap that lost the race
	// below was booted on at the next restart and published over the
	// company the peer had won with — the overwrite its create-only
	// activation had just refused — and an activation that failed
	// outright left the same claim behind. [store.Configs.InsertActive]
	// is for the OFFLINE commands, which have no fleet to ask.
	//
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
	id, err := configs.Insert(ctx, seeded)
	if err != nil {
		return fmt.Errorf("seed the company config: %w", err)
	}
	seeded.ID = id
	// AN OVERRIDE IS UNCONDITIONAL, and that is its whole posture: it is
	// asserting what this node booted with, not editing something it read.
	// There is no earlier revision it derived from, so there is nothing it
	// could have raced with — and an expectation here would make a boot
	// fail against a fleet that had moved on for perfectly good reasons.
	//
	// A BOOTSTRAP is create-only ([coord.ActivationRequest.ExpectAbsent]):
	// it read no pointer above, and a peer that activated one since has
	// made the company exist, which is exactly when a bootstrap stands
	// down. Not a failure, and nothing to undo: the revision it stored is
	// inert history and this node holds no company of its own — exactly
	// the state [fleetOutranksSeed] leaves. The reconciler adopts the
	// peer's revision at its first tick, and a restart before then finds
	// the store empty again and stands down for the peer's pointer there.
	published, err := plane.Activate(ctx, coord.ActivationRequest{
		RevisionID: id, Summary: summary, Payload: payload, At: at,
		// THE ORIGIN RIDES THE POINTER, so every peer that adopts this
		// revision records the node that seeded it, from `file`, at the
		// instant it was written — rather than itself, at the instant it
		// happened to apply it.
		Origin:       originOf(seeded),
		ExpectAbsent: !seed.Override,
	})
	if !seed.Override && errors.Is(err, coord.ErrActivationRaced) {
		log.WarnContext(ctx, "company_seed_ignored", "file", seed.Path,
			"reason", "the fleet activated a company while this node was seeding "+
				"it, and -company only bootstraps a company that does not exist yet",
			"hint", "this node converges on the fleet's company; to make this file "+
				"the company, run `crewlet config import "+seed.Path+"`")
		return nil
	}
	if err != nil {
		return fmt.Errorf("activate the seeded company config: %w", err)
	}
	// THE FLEET TOOK IT, so it is this node's company too — AT THE
	// POINTER'S INSTANT, which may be later than the one asked for
	// ([coord.ActivationAt]). ONE INSTANT FOR THE ROW AND THE POINTER: the
	// local row's `activated_at` is what this node's config history shows
	// for the activation, what the reconciler holds against the pointer's
	// instant on every tick, what the next boot compares with the pointer
	// to decide whether this node holds a newer revision to publish, and
	// what this node boots its chart with next time — the chart apply
	// stamps every project with it — so it is the instant the fleet
	// applied, never a second reading of the clock. Best effort, because
	// the activation has landed and cannot be taken back: the reconciler
	// marks the fleet's revision active here when it applies the epoch.
	if _, aerr := configs.Activate(ctx, id, published.At); aerr != nil {
		log.WarnContext(ctx, "config_revision_not_marked_active",
			"revision", id, "epoch", published.Epoch, "error", aerr,
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

// fleetOutranksSeed reports whether a bootstrap seed into this node's EMPTY
// store meets a company the fleet already runs — which outranks it exactly as
// a company in the store would — and says so when the file is not that
// company's own document.
//
// # An empty store is not an empty company
//
// Two ordinary nodes have one: a node without `data`, whose scratch store is
// deleted at every boot, and a data node joining a fleet that is already
// running. The seed used to ask only its own store, find nothing and activate
// the file over the fleet's pointer — so a stateless node carrying the default
// ./company.yaml reverted every node's live edits at each of its restarts, and
// a node added with the quickstart's command reverted them once. Every seat id
// is derived from the company's name, so a file that also renamed the company
// orphaned every seat's memory.
//
// QUIET when the file is the fleet's own document — a node joining with the
// file the fleet was seeded from has nothing to be told — and said whenever
// this node cannot show that it is, including when the fleet's body cannot be
// read or opened here: whatever the reason, the file is not being imported.
// The reconciler fetches and adopts the fleet's revision at its first tick, as
// it does on a node with no file at all.
func fleetOutranksSeed(ctx context.Context, plane coord.Plane, cipher secrets.Cipher,
	seed tierBSeed, document []byte, log *slog.Logger,
) (bool, error) {
	target, found, err := plane.Target(ctx)
	if err != nil {
		return false, fmt.Errorf("read the activation pointer: %w", err)
	}
	if !found {
		return false, nil
	}
	same := false
	if payload, held, err := plane.Payload(ctx, target.RevisionID); err == nil && held {
		if current, err := secrets.Open(cipher, payload); err == nil {
			same = bytes.Equal(current, document)
		}
	}
	if !same {
		log.WarnContext(ctx, "company_seed_ignored",
			"file", seed.Path, "active_revision", target.RevisionID,
			"reason", "this node's store is empty, but the fleet already runs a "+
				"company, and -company only bootstraps a company that does not "+
				"exist yet",
			"hint", "this node converges on the fleet's company; to make this "+
				"file the company again, run `crewlet config import "+seed.Path+
				"`, or restart with -import-company "+seed.Path)
	}
	return true, nil
}

// publishLocalActive points the fleet at this node's active revision when the
// fleet has no pointer, or an OLDER one.
//
// This is what makes an offline `crewlet config import` reach the fleet. The
// activation pointer lives in the coordination store, which that command does
// not open: it marks the revision active in the node's own database, where
// the revision itself lives, and this publishes it at the next start.
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
	// UNCONDITIONAL for the same reason as the seed. Two nodes booting at
	// once may both decide to publish, and last-write-wins is the right
	// answer there: each is offering the revision IT holds, both are
	// legitimate, and every node converges on whichever landed. A
	// compare-and-set would turn that into a boot-time failure to retry
	// for nothing. It is the EDIT path that must not lose a write.
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

// bootOptions is what `crewlet run` builds its engine from: the Tier A it
// loaded, the company this node starts on, and the instant that company was
// activated at — and the seed the reconciler is then handed, DECIDED here
// rather than beside the import, because the two are readings of one
// decision.
//
// # The company the node is going to serve, never merely the one it was handed
//
// The engine builds several things ONCE PER PROCESS from the company it boots
// on — the native runtime and which of its halves exist, the code sandbox's
// runtime, the store's embedding width — and the reconciler's first tick
// changes none of them. So the boot company has to be the one [seedCompany]
// is about to leave active, decided by the same rule:
//
//   - a managed deployment never imports a bootstrap seed, so the seed is
//     dropped first ([managedSeed]) and everything below reads what is left;
//   - an OVERRIDE (`-import-company`) is about to become the active revision,
//     so it is the file's;
//   - otherwise, a store that holds a company is what the node serves — a
//     `-company` bootstrap seed is ignored over it — so it is the store's,
//     WITH the instant it was activated at ([companyFromStore]);
//   - and a store that holds none boots on the bootstrap seed, which is about
//     to be imported into it unless the fleet already runs a company
//     ([fleetOutranksSeed]), or on nothing at all — and a SCRATCH store, empty
//     at every boot, boots on nothing whatever its seed (below).
//
// It used to take the file whenever there was one. `-company` defaults to
// ./company.yaml and the quickstart passes it on every start, so a node whose
// company had been edited live booted on the stale file at every restart: its
// native runtime was built from a document the seed then ignored, and a
// tracker turned native through the dashboard came back as no tracker after
// each restart, for ever. And the managed drop was made only at the import,
// after the engine had booted on the file, so a managed deployment's empty
// store served a company nobody had activated.
//
// A file's company has NO ACTIVATION YET, so its instant stays zero and the
// engine leaves its chart to the reconciler's first tick, which publishes the
// file and applies it with the pointer's own instant. The store's instant is
// what the engine stamps the chart and the knowledge spaces with at boot
// ([engine.Options.ActivatedAt]): a boot that dropped it would stamp nothing
// at all, and the company's projects would wait for the next apply.
//
// What this cannot see is the FLEET's pointer: on the embedded topology the
// coordination store is inside the engine this is building. So two nodes boot
// on a company the first tick then moves them off — one that was down while a
// newer revision was activated boots on its own store's older one, and a data
// node whose store is empty, joining a fleet that already runs a company,
// boots on the bootstrap seed the fleet then outranks. Each serves the fleet's
// epoch from the first tick on, and what it built once per process from the
// other company is rebuilt at its next restart, which boots on the revision
// that tick left in its store.
//
// ONE VALUE, handed to the engine WHOLE, so no field of it can be decided here
// and dropped at the call site. Read BEFORE the engine, in its own
// open-and-close, so the engine still owns the backends it opens.
func bootOptions(ctx context.Context, bootstrapPath string, boot *config.Bootstrap,
	seed tierBSeed, log *slog.Logger) (engine.Options, tierBSeed, error) {

	seed = managedSeed(ctx, boot, seed, log)
	opts := engine.Options{Bootstrap: boot, Company: seed.Company}
	if seed.Override {
		return opts, seed, nil
	}
	// A SCRATCH STORE IS NOT READ: what it holds is the previous run's, it is
	// deleted as the engine opens it, and opening it here would create the
	// replicated estate a node without `data` must not have.
	//
	// AND IT BOOTS UNCONFIGURED, its bootstrap seed included. The store is
	// empty at every boot, so whether the seed is imported is the FLEET's
	// answer ([fleetOutranksSeed]), which nothing can read before the engine
	// exists — and a node that booted on the file whenever the fleet already
	// ran a company would build its runtime from a document it then ignored,
	// at every boot, for ever. Unconfigured, the reconciler's first tick
	// applies whichever company the seed left active — the fleet's, or the
	// file it imported into an empty fleet — before any seat is claimed.
	if boot.Store.Scratch {
		opts.Company = nil
		return opts, seed, nil
	}
	company, activatedAt, err := companyFromStore(ctx, bootstrapPath)
	if err != nil {
		return engine.Options{}, tierBSeed{}, err
	}
	if company == nil {
		// AN EMPTY STORE, which a bootstrap seed is about to fill.
		return opts, seed, nil
	}
	opts.Company, opts.ActivatedAt = company, activatedAt
	return opts, seed, nil
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
// It also answers WHEN the revision was activated — the instant this node's
// store recorded with its active copy, which is the fleet pointer's own
// instant for a revision the fleet activated. The engine stamps the org
// chart's projects with it, so a restart re-applying the same activation is
// recognised as one and writes nothing (see [engine.Options.ActivatedAt]).
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
func companyFromStore(ctx context.Context, bootstrapPath string) (*config.Company, time.Time, error) {
	cs, closeStore, err := openConfigStore(ctx, bootstrapPath,
		"Another `crewlet run` holds this node's store: stop it before "+
			"starting a second, since one store belongs to one process.")
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
	document, err := openStored(cs.cipher, active)
	if err != nil {
		return nil, time.Time{}, err
	}
	company, err := config.DecodeCompany(document)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("parse the active revision %s: %w", active.ID, err)
	}
	// VALIDATED HERE, naming the revision, because booting is applying and
	// the stored-form decode holds a revision to no rule. The engine checks
	// the same rules again as it builds the epoch, but only this frame knows
	// which revision it is and that an offline import is the way out: the
	// node is not serving its API yet, and its listener answers every route
	// it will serve `503 starting`.
	//
	// The RUNNABLE rules only, like every apply: an admission rule is one
	// an apply does not enforce, because a newer peer may have admitted the
	// revision under rules this build does not share. The reconciler warns
	// about it once it applies the epoch (see
	// [config.Company.ValidateRunnable]).
	if err := company.ValidateRunnable(); err != nil {
		return nil, time.Time{}, fmt.Errorf("the active revision %s cannot run on this build; "+
			"import a corrected document with `crewlet config import` and start "+
			"again: %w", active.ID, err)
	}
	return company, active.ActivatedAt, nil
}
