package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/crewlet/crewlet/internal/configplane"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/store"
)

// WHAT A BOOT PUBLISHES, and over what.
//
// The fleet's activation pointer lives in the coordination store, which a
// command run while the engine is stopped does not open. So what an offline
// command chooses — `crewlet config import`, `activate`, `rekey` — is an OFFER:
// this node's store boots on it, and the next start publishes it. And a
// coordination store that was rebuilt has lost its pointer, which every node
// that boots after can restore from what it knows.
//
// Both used to be one rule: publish this node's active revision wherever the
// pointer was older by instant. It published an offline change over a fleet
// that had moved on — a change made on a node that was behind rolled every
// node back to the stale document underneath it (#321) — it published with no
// compare-and-set, so a write landing between the read and the publish was
// replaced (#319), and it published a re-activation of the fleet's own
// revision only when this node's clock said it was newer (#318).
//
// Now each publish is a compare-and-set against what it was built on, decided
// by [decideBootPublish] — a pure function of the facts, so every rule is a row
// of a table a test walks — and carried out by [publishBoot]. Each publish also
// says whether it RESTORES a company the fleet lost or CHOOSES one
// ([coord.ActivationRequest.Restore]), because only a restore is something a
// peer that knows better replaces (2b).
//
// AN OFFER (the boot row an offline command chose, built on β — its base, or
// the unconfirmed offer it replaced, [store.Offer.Over]):
//
//   - 1b: the fleet took the offer already — the pointer names it at the
//     instant this node planned for it (the publish landed before a crash),
//     or this node's reconciler saw the pointer name it. It is confirmed, not
//     published again, and the boot decides on as for a revision.
//   - 1a: the pointer names β, and is not a restore of a revision this node
//     saw the fleet move past. The offer is published expecting that very
//     activation (its epoch), at an instant later than the pointer's, so it
//     always mints an epoch — offering the fleet's own revision is the
//     credential-rotation gesture — and lands at exactly the instant planned.
//   - 1d: no pointer, and the fleet this node last saw ran β (or it saw none).
//     Published create-only.
//   - 1c: anything else. The fleet moved on from what the change was built
//     on, so it is not published — said at warn on every boot, and kept: this
//     node runs it until it applies the fleet's revision.
//   - 1e: 1c, where the pointer is gone or is a stale restore, and the fleet's
//     revision this node saw is put back as 2a or 2b would.
//
// NOT AN OFFER — R is the fleet's revision this node last saw, else what it
// boots on:
//
//   - 2a: no pointer. R is restored create-only, at its own instant.
//   - 2b: the pointer is a RESTORE of a revision this node holds, older than
//     R: another node put back the company it remembered, and this one
//     remembers the fleet moving on to R. R is restored over it, by
//     compare-and-set against that very restore. A pointer somebody CHOSE is
//     never replaced here, however old its instant reads: instants come from writers' clocks, and an offline
//     activation or an override on a node whose clock runs behind would
//     otherwise be reverted by the first peer to boot.
//   - 2c: anything else. The pointer is the company, and the reconciler
//     converges on it.
//
// A publish that loses its compare-and-set re-reads the pointer and decides
// again. That ends: every landing moves the pointer to a later instant, and
// every rule that publishes needs the pointer where this node left it.
//
// What no rule can do is tell a stale restore from the company once somebody
// has CHOSEN something over it: an edit made on a node that knew no better is
// a choice, and a peer that remembers a newer revision leaves it alone. A
// rebuilt coordination store is restored by the first nodes to boot, so the
// fleet converges on the newest revision any of them remembers only while
// they boot ahead of the first write.

// bootRule names a row of [decideBootPublish]'s table.
type bootRule string

const (
	ruleOfferLanded    bootRule = "1b"
	ruleOfferOverBase  bootRule = "1a"
	ruleOfferWithheld  bootRule = "1c"
	ruleOfferFirst     bootRule = "1d"
	ruleOfferRestore   bootRule = "1e"
	ruleRestore        bootRule = "2a"
	ruleRestoreNewer   bootRule = "2b"
	ruleFleetPublishes bootRule = "2c"
	ruleNothingToBoot  bootRule = "none"
)

// bootFacts is everything the boot publish decides on.
type bootFacts struct {
	// boot is the revision the engine booted on, zero for none; fleets the
	// fleet's revision as this node last saw it, nil for none.
	boot   store.Revision
	fleets *store.Revision

	// target is the pointer as just read, pointed whether there is one, and
	// targetHeld whether this node's history holds the revision it names.
	target     coord.Activation
	pointed    bool
	targetHeld bool

	// now is this node's clock, in UTC at the store's microseconds.
	now time.Time
}

// bootPublish is what the boot does.
type bootPublish struct {
	rule bootRule

	// publish is the revision to activate, zero for none; expect,
	// expectEpoch and expectAbsent are its compare-and-set, at the instant it
	// asks for, and restore whether it puts back a company the fleet lost.
	publish      store.Revision
	expect       string
	expectEpoch  int64
	expectAbsent bool
	at           time.Time
	restore      bool

	// offer is whether publish is the boot's offer, which is planned before
	// and confirmed after, over the revision named; confirm is a landed offer
	// to confirm without publishing.
	offer   bool
	over    string
	confirm bool

	// withheld is an offer the fleet moved on from, said at warn.
	withheld bool
}

// decideBootPublish is the table in this file's opening comment, as a function
// of the facts alone.
func decideBootPublish(f bootFacts) bootPublish {
	if offer := f.boot.Offer; f.boot.ID != "" && offer != nil {
		if offerLanded(f) {
			return bootPublish{rule: ruleOfferLanded, confirm: true}
		}
		// THE FLEET THIS NODE SAW MOVED ON from what the change was built
		// on: the reconciler recorded a later pointer revision after the
		// command ran, and only failed to apply it.
		movedOn := f.fleets != nil && !offer.Over(f.fleets.ID)
		switch {
		case f.pointed && offer.Over(f.target.RevisionID) && (!movedOn || !f.target.Restored):
			// LATER THAN THE POINTER BY CONSTRUCTION, and pinned to the
			// activation read, so it lands at exactly the instant planned
			// and a crash after it lands is told from the pointer still
			// naming the base — which for a re-activation is the same id.
			// A RESTORED base this node saw the fleet move past is not the
			// company, only another node's stale memory of it.
			return bootPublish{rule: ruleOfferOverBase, publish: f.boot, offer: true,
				over: f.target.RevisionID, expect: f.target.RevisionID,
				expectEpoch: f.target.Epoch, at: coord.ActivationAt(f.now, f.target.At)}
		case !f.pointed && !movedOn:
			over := offer.Base
			if f.fleets != nil {
				over = f.fleets.ID
			}
			return bootPublish{rule: ruleOfferFirst, publish: f.boot, offer: true,
				over: over, expectAbsent: true, at: f.now}
		}
		if f.fleets != nil {
			if restore := decideKnown(f, *f.fleets); restore.publish.ID != "" {
				restore.rule, restore.withheld = ruleOfferRestore, true
				return restore
			}
		}
		return bootPublish{rule: ruleOfferWithheld, withheld: true}
	}
	known := f.fleets
	if known == nil && f.boot.ID != "" {
		known = &f.boot
	}
	if known == nil {
		return bootPublish{rule: ruleNothingToBoot}
	}
	return decideKnown(f, *known)
}

// offerLanded is rule 1b: the fleet took the boot's offer already.
//
// BY THE PLANNED INSTANT, or by the fleet's row naming it. A publish pinned to
// the epoch it read lands at exactly the instant planned, so the pointer naming
// the offer at it is that publish. And the reconciler records only what the
// pointer named, so a fleet's row naming the offer is a publish that landed and
// was seen — unless the offer is a re-activation built on itself, which the
// fleet's row names because the fleet ran it before the command chose it again.
func offerLanded(f bootFacts) bool {
	offer := f.boot.Offer
	switch {
	case f.pointed && f.target.RevisionID == f.boot.ID && !offer.PlannedAt.IsZero() &&
		store.EncodeTime(f.target.At) == store.EncodeTime(offer.PlannedAt):
		return true
	case f.fleets != nil && f.fleets.ID == f.boot.ID && !offer.Over(f.boot.ID):
		return true
	}
	return false
}

// decideKnown is rules 2a to 2c for the revision this node knows the company to
// be.
func decideKnown(f bootFacts, known store.Revision) bootPublish {
	switch {
	case !f.pointed:
		return restoreKnown(known, f.now)
	case f.target.Restored && f.targetHeld && f.target.RevisionID != known.ID &&
		store.EncodeTime(known.ActivatedAt) > store.EncodeTime(f.target.At):
		// PINNED TO THE RESTORE READ, not only its revision: somebody who
		// chose that same revision since is the company, not a restore.
		return bootPublish{rule: ruleRestoreNewer, publish: known, restore: true,
			expect: f.target.RevisionID, expectEpoch: f.target.Epoch, at: known.ActivatedAt}
	}
	return bootPublish{rule: ruleFleetPublishes}
}

// restoreKnown is rule 2a: a revision this node knows, restored create-only at
// its own instant, or now where nothing has activated it.
func restoreKnown(known store.Revision, now time.Time) bootPublish {
	at := known.ActivatedAt
	if at.IsZero() {
		at = now
	}
	return bootPublish{rule: ruleRestore, publish: known, restore: true, expectAbsent: true, at: at}
}

// bootRecords is what the boot publish asks of this node's config history.
type bootRecords interface {
	Get(ctx context.Context, revisionID string) (store.Revision, bool, error)
	PlanOffer(ctx context.Context, revisionID string, at time.Time, over string) error
	ConfirmOffer(ctx context.Context, revisionID string) error
	NoteActivation(ctx context.Context, revisionID string, at time.Time) error
}

// publishBoot carries out [decideBootPublish] for the revision the engine
// booted on and the fleet's revision this node last saw, both as the boot read
// them ([seedPlan]).
//
// # AUTHENTICATED BEFORE IT IS PUBLISHED
//
// What this publishes, every peer applies — and every peer refuses a body that
// is not sealed, because the seal is what says a node of this fleet wrote it.
// So a revision this node holds in the clear is refused HERE, where this node's
// operator can act on it, rather than published to be refused on every node.
func publishBoot(ctx context.Context, plane coord.Plane, pub queue.Publisher,
	configs bootRecords, boot store.Revision, fleets *store.Revision,
	cipher secrets.Cipher, now func() time.Time, log *slog.Logger,
) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		target, pointed, err := plane.Target(ctx)
		if err != nil {
			return fmt.Errorf("read the activation pointer: %w", err)
		}
		held := false
		if pointed {
			if _, held, err = configs.Get(ctx, target.RevisionID); err != nil {
				return fmt.Errorf("read the fleet's revision %s: %w", target.RevisionID, err)
			}
		}
		decision := decideBootPublish(bootFacts{
			boot: boot, fleets: fleets, target: target, pointed: pointed,
			targetHeld: held, now: now().UTC().Truncate(time.Microsecond),
		})
		if decision.withheld {
			// ON EVERY BOOT, for as long as the offer stands: a change an
			// operator made that the fleet is not running is the thing they
			// must not find out about by accident.
			log.WarnContext(ctx, "offline_revision_not_published", "revision", boot.ID,
				"built_on", boot.Offer.String(), "fleet_runs", fleetRuns(target, pointed, fleets),
				"reason", configplane.WithheldOfferReason(*boot.Offer),
				"detail", "this node runs it until it applies the fleet's revision",
				"hint", configplane.WithheldOfferRemedy(boot.ID, boot.Offer.Base))
		}
		if decision.confirm {
			if confirmErr := confirmOffer(ctx, configs, boot, log); confirmErr != nil {
				return confirmErr
			}
			// A REVISION NOW, and the boot decides on as for one: the
			// pointer may still be gone, or a stale restore, and the
			// company this node knows is owed to the fleet as any is.
			boot.Offer = nil
			continue
		}
		if decision.publish.ID == "" {
			return nil
		}
		raced, err := publishDecided(ctx, plane, pub, configs, decision, cipher, log)
		if err != nil || !raced {
			return err
		}
		if decision.offer {
			// WHAT THE PLAN SETTLED goes into the next decision with it:
			// the store now holds the base the pointer named and the
			// instant asked for ([store.Configs.PlanOffer]).
			boot.Offer = &store.Offer{Base: decision.over, PlannedAt: decision.at}
		}
		log.InfoContext(ctx, "boot_publish_raced", "revision", decision.publish.ID,
			"rule", string(decision.rule),
			"detail", "another write moved the pointer; deciding again against it")
	}
}

// publishDecided activates one decision's revision, reporting whether it lost
// its compare-and-set.
func publishDecided(ctx context.Context, plane coord.Plane, pub queue.Publisher,
	configs bootRecords, decision bootPublish, cipher secrets.Cipher, log *slog.Logger,
) (bool, error) {
	revision := decision.publish
	if _, err := openStored(cipher, revision); err != nil {
		return false, fmt.Errorf("revision %s is not published to the fleet: %w", revision.ID, err)
	}
	if decision.offer {
		if err := configs.PlanOffer(ctx, revision.ID, decision.at, decision.over); err != nil {
			return false, noLongerOffered(revision, "plan the publish of", err)
		}
	}
	// The payload goes up with it, and the revision's OWN origin, never this
	// node's: an offline `crewlet config import` is an operator's write that
	// only reaches the fleet here, so every peer that keeps it records who
	// wrote it, through which credential, from where and when.
	published, err := plane.Activate(ctx, coord.ActivationRequest{
		RevisionID: revision.ID, Summary: revision.Summary, Payload: revision.Payload,
		At: decision.at, Origin: originOf(revision),
		Expect: decision.expect, ExpectEpoch: decision.expectEpoch,
		ExpectAbsent: decision.expectAbsent, Restore: decision.restore,
	})
	if errors.Is(err, coord.ErrActivationRaced) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("publish revision %s: %w", revision.ID, err)
	}
	if decision.offer {
		// THE FLEET TOOK IT: it stays what this node boots on, as a
		// revision rather than a choice, and its instant is the one the
		// reconciler's first apply of this epoch serves it at.
		if err := configs.ConfirmOffer(ctx, revision.ID); err != nil {
			log.WarnContext(ctx, "offline_revision_not_confirmed", "revision", revision.ID,
				"epoch", published.Epoch, "error", err,
				"detail", "the fleet runs it; the next boot tells the landed publish "+
					"from the pointer by its planned instant and confirms it then")
		}
	} else {
		landed(ctx, configs, revision, published, log)
	}
	nudge(ctx, pub, revision, log)
	log.InfoContext(ctx, "boot_revision_published", "revision", revision.ID,
		"epoch", published.Epoch, "rule", string(decision.rule),
		"expected", orNothing(decision.expect))
	return false, nil
}

// confirmOffer is rule 1b: the offer landed before a crash, and is recorded as
// taken.
func confirmOffer(ctx context.Context, configs bootRecords, boot store.Revision,
	log *slog.Logger) error {

	if err := configs.ConfirmOffer(ctx, boot.ID); err != nil {
		return noLongerOffered(boot, "confirm the landed publish of", err)
	}
	log.InfoContext(ctx, "offline_revision_confirmed", "revision", boot.ID,
		"detail", "the fleet took this offline change at an earlier start")
	return nil
}

// noLongerOffered is a plan or a confirm that found no offer where the boot
// read one, and refuses the boot over it.
//
// A DEFECT RATHER THAN A RACE, since the boot holds this node's store from the
// read it decided on until the engine opens the same file ([heldStore]): no
// offline command can have replaced the offer in between, and the one online
// writer of the boot row is a reconciler that does not exist until this
// publish has returned. So the store says something the engine's company
// contradicts, and going on would publish, serve and report against a row
// nobody can account for.
func noLongerOffered(offer store.Revision, doing string, err error) error {
	if !errors.Is(err, store.ErrNotOffered) {
		return fmt.Errorf("%s revision %s: %w", doing, offer.ID, err)
	}
	return fmt.Errorf("%s revision %s, which this node booted on: it is no longer the "+
		"unpublished offline choice its store holds, though nothing could have "+
		"replaced it while the boot held the store (%w). Nothing was published for "+
		"it. Start the node again, which decides on the store as it now stands; if "+
		"that start says the same, this is a defect in `crewlet run` rather than in "+
		"either config file", doing, offer.ID, err)
}

// fleetRuns names what the fleet runs for a withheld offer's warning: the
// pointer's revision, or with none the one this node last saw.
func fleetRuns(target coord.Activation, pointed bool, fleets *store.Revision) string {
	switch {
	case pointed:
		return target.RevisionID
	case fleets != nil:
		return fleets.ID
	}
	return "(nothing)"
}

// orNothing is an id as a log line says it, with "(nothing)" for none.
func orNothing(id string) string {
	if id == "" {
		return "(nothing)"
	}
	return id
}
