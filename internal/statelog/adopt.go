package statelog

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/words"
)

// AdoptPartSuffix names a fetched artefact while it is verified, BESIDE THE
// LIVE FILE in the same directory: the install is a rename, atomic only within
// one filesystem, and a copy across at the end is the one moment an
// interrupted adoption could leave a mixture.
const AdoptPartSuffix = ".adopt.part"

// AdoptDeps is everything the join needs that it does not own.
type AdoptDeps struct {
	// Domains are every log this build registers, by name. An artefact
	// names all of them or is refused: a domain it does not name is one this
	// node would believe it was caught up on.
	Domains map[string]Registered

	// LivePath is the replicated estate this node is running, which the
	// artefact replaces.
	LivePath string

	// NodeID is who is joining.
	NodeID string

	// Conn is the transfer's own connection.
	Conn *nats.Conn

	// Donors lists the donors this join expects, so collection ends once
	// each has answered rather than at [OfferWindow] ([CollectOffers]). It
	// must name EVERY node that can offer, as an offer after the last one
	// named is not waited for: the engine names every live data node and
	// every node whose fresh register row names an artefact, which covers a
	// DRAINING node (it drops its presence first and serves until its state
	// log stops).
	//
	// Nil, or an error, waits out the window: an expectation only stops
	// early. It is called WHILE offers are collected, on a context the
	// window ends, and must return once that context does.
	Donors func(ctx context.Context) ([]string, error)

	// Need is this node's acceptance test for an artefact, as the request it
	// would make: per domain, the lowest position an artefact may name, its
	// live stream's generation and that stream's creation instant. The
	// request rather than separate maps, because the terms grow. The adopter
	// overwrites [OfferRequest.NodeID].
	Need        func(ctx context.Context) (OfferRequest, error)
	StillUsable func(ctx context.Context, m Manifest) error

	// Hold pins the replay tail for the whole transfer and returns the
	// release; the pin must RENEW ITSELF ([HoldTail] does), as a transfer outlives
	// [TrimHoldStale]. Unpinned, the trim can pass the artefact in flight and
	// the node installs a snapshot whose tail is gone. The adopter releases it
	// only when nothing was installed; otherwise nothing in the fleet counts
	// that tail yet (a boot has no presence or published position, a running
	// node's row names the replaced checkpoint), so [Adoption] hands it on.
	// at is the ARTEFACT'S OWN positions by domain, in the manifest's
	// generation ([Offer.Usable] held it to the live stream's) and no other:
	// the trim reads a hold at a lower generation as an unknown term and
	// blocks the whole log, and a joiner's own checkpoint is such a generation
	// (none for a node that never ran, the old one after a reanchor).
	Hold func(ctx context.Context, at map[string]DomainPosition) (release func(), err error)

	// Close and Reopen bracket the install: both databases must be CLOSED for
	// the rename, because this process's claim is on the path, not the inode.
	// A failed close is unwound by a Reopen too, since a close may take the
	// database out of service before reporting failure (the engine's does):
	// so Reopen must do nothing to a database that is still open.
	Close  func(ctx context.Context) error
	Reopen func(ctx context.Context) error

	// Record writes this node's adoption row at each phase, so a crash
	// mid-adoption reads as one ([RecordAdoption]). A failed write before the
	// rename ([AdoptionScrubbed]) is this node's own store, which every offer
	// would meet, so the join ends as [ErrJoinerFault]. After it
	// ([AdoptionInstalled], [AdoptionComplete]) the artefact is live and open,
	// so the join ends ADOPTED, with [ErrAdoptionUnrecorded]: installing
	// another offer over it would be a second transfer onto an older artefact.
	//
	// began is the adoption's start, the same at every phase of one join. The
	// ADOPTER stamps it: it keys one join to one row, and only [Adopter.Join]
	// knows when the last offer was in.
	Record func(ctx context.Context, began time.Time, donor string, m Manifest,
		phase AdoptionPhase) error

	// Logger is where this writes. Nil is the package's component logger,
	// never silence (loggerOr).
	Logger *slog.Logger
	Now    func() time.Time
}

// AdoptionPhase is how far a join has got, as [AdoptDeps.Record] is told it.
//
// THE ROW DOES NOT KEEP IT: [RecordAdoption] writes the same columns at every
// phase and stamps completed_at only at [AdoptionComplete], so a crash leaves
// when the adoption began and whether it finished. Nothing needs more: the
// ledger's watermark is inside the file, so the live file says how far back
// its own ledger may have lost rows, and nothing refuses to serve on an
// incomplete row. The phase names the step in a failed Record's error.
// An incomplete row is a join that stopped (a crash, a late refusal, a failed
// install) or a completed adoption whose record could not be advanced, which
// logged `statelog_adoption_unrecorded` ([ErrAdoptionUnrecorded]).
type AdoptionPhase string

const (
	// AdoptionScrubbed — the artefact is verified and its scrub checked,
	// and it has not been installed.
	AdoptionScrubbed AdoptionPhase = "scrubbed"

	// AdoptionInstalled — the file is in place and the node has not
	// finished bringing itself up on it.
	AdoptionInstalled AdoptionPhase = "installed"

	// AdoptionComplete — the node may serve.
	AdoptionComplete AdoptionPhase = "complete"
)

// Adopter runs the join: choose an artefact, fetch it, verify it, install it.
type Adopter struct {
	deps AdoptDeps
	log  *slog.Logger
	now  func() time.Time
}

// NewAdopter builds the join.
func NewAdopter(d AdoptDeps) (*Adopter, error) {
	switch {
	case len(d.Domains) == 0:
		return nil, fmt.Errorf("statelog: a join with no registered domain would " +
			"accept an artefact that names nothing")
	case d.LivePath == "":
		return nil, fmt.Errorf("statelog: a join has no database to replace")
	case d.Conn == nil:
		return nil, fmt.Errorf("statelog: a join has no connection to fetch over")
	case d.Need == nil:
		return nil, fmt.Errorf("statelog: a join cannot say what it needs")
	case d.Hold == nil:
		return nil, fmt.Errorf("statelog: a join cannot pin the replay tail, so " +
			"the trim could pass the artefact while it is in flight")
	case d.Close == nil || d.Reopen == nil:
		return nil, fmt.Errorf("statelog: a join cannot close the database it is " +
			"replacing, and the install is a rename over a path this process " +
			"holds a claim on")
	case d.Record == nil:
		return nil, fmt.Errorf("statelog: a join cannot record itself, so a crash " +
			"mid-adoption would be indistinguishable from a node that is caught up")
	}
	if err := checkRegistered(d.Domains); err != nil {
		return nil, err
	}
	for key, r := range d.Domains {
		if key != r.Domain.Name() {
			return nil, fmt.Errorf("statelog: a join registers the %s domain under "+
				"the key %q — a manifest names every log by its domain's name, so "+
				"this one would be read as missing from every artefact", r.Domain.Name(), key)
		}
	}
	logger := loggerOr(d.Logger)
	now := d.Now
	if now == nil {
		now = time.Now
	}
	return &Adopter{deps: d, log: logger, now: now}, nil
}

// Adoption is what a join installed, and the pin on the tail it is about to
// replay.
type Adoption struct {
	// Manifest is the artefact installed: zero when the join installed
	// nothing, and the installed one whenever the rename happened, whatever
	// error came beside it ([Adopter.Join]).
	Manifest Manifest

	// Release ends the pin [AdoptDeps.Hold] took on the installed artefact's
	// tail. The CALLER calls it once this node's published position is no
	// further along any log than the rows it now holds, when the trim counts
	// what the pin protects. Nil exactly when Manifest is zero: the adopter
	// released the pin itself.
	Release func()
}

// ErrNoOffer reports a join that found no artefact it could use: a statement
// about the FLEET (every offer refused, or nobody offered), which a caller
// answers by carrying on without a snapshot. So no other ending is reported
// as it:
//
//   - a cancelled join wraps ctx.Err(), whichever step it interrupted;
//   - a join that lost this node's own database is [ErrEstateNotRestored];
//   - this node's own fault before any install is [ErrJoinerFault], since
//     every other offer would meet it;
//   - a join that INSTALLED an artefact is an adoption ([ErrAdoptionUnrecorded]).
var ErrNoOffer = errors.New("statelog: no usable snapshot was offered")

// ErrAdoptionUnrecorded reports a join that installed and opened an artefact
// and could not advance this node's adoption row past the install.
//
// AN ADOPTION, NOT A FAILURE: the one error [Adopter.Join] returns beside the
// manifest it installed. From the rename the artefact is the live, open
// database; what failed is the HISTORY row in the node estate
// ([RecordAdoption]), which nothing on the write path reads. As a donor's
// refusal, the join would install a no-newer offer over the live one or report
// that nobody could donate, and a running node would skip what an adoption
// owes its consumers. So a caller acts on the manifest; the row stays
// incomplete, logged `statelog_adoption_unrecorded` beside `statelog_adopted`.
var ErrAdoptionUnrecorded = errors.New("statelog: the artefact is installed " +
	"and open, and this node's record of the adoption could not be advanced")

// ErrJoinerFault reports a join that ended before anything was installed, on a
// fault of THIS NODE'S OWN rather than the offer's: its trim-hold register
// refusing the pin, its disk refusing the copy or the stamp, its store refusing
// the first adoption row, or a close or install rolled back to the live file.
//
// Not a donor's refusal nor [ErrNoOffer]: every other offer would meet the same
// fault. Nor [ErrEstateNotRestored]: the live database is open and untouched,
// so a caller carries on as from [ErrNoOffer], naming whose fault it was (a
// boot comes up behind, a running node asks again on its interval). Some faults
// pass; the rest, such as a full volume, are an operator's, named in the
// wrapped error.
var ErrJoinerFault = errors.New("statelog: this node could not adopt a snapshot, " +
	"for a fault of its own")

// ErrEstateNotRestored reports a join that closed the live database for an
// install and could not open it again.
//
// It must never reach a caller as [ErrNoOffer], which the engine answers by
// carrying on with the history it has: with the estate closed, every read,
// applier and hold answers [store.ErrNoEstate], so a boot must stop and a
// running node must reopen it before asking the fleet for anything. It ends
// the join, as every remaining offer would meet the same bracket around a
// missing estate. When the rename had happened, [Adopter.Join] returns the
// installed manifest beside it: whatever opens the estate next opens that.
var ErrEstateNotRestored = errors.New("statelog: the live replicated database " +
	"a join closed for its install is not open again")

// nodeFault marks a join failure that is THIS NODE'S rather than the offer's
// (its disk, its store, its trim-hold register), before anything was
// installed. Unexported: a caller branches on the join's answer, which
// [Adopter.Join] files as [ErrJoinerFault] rather than as a donor's refusal.
type nodeFault struct{ err error }

func (f *nodeFault) Error() string { return f.err.Error() }
func (f *nodeFault) Unwrap() error { return f.err }

// ownFault marks err as this node's own ([nodeFault]), and nil as nil.
func ownFault(err error) error {
	if err == nil {
		return nil
	}
	return &nodeFault{err: err}
}

// Join runs the whole sequence and reports what it adopted. Its steps, each
// commented where it is taken: 1 hold the replay tail, 2 select offers by
// manifest alone (a transfer ending in a refusal was not needed), 3 transfer
// beside the live file, 4 inspect read-only, 5 verify checksum and positions,
// 6 verify the scrub, 7 re-check the floor, 8 name the copy, 9 install (the
// one place the engine replaces a live database), 10 complete.
//
// THE RENAME IS THE POINT OF NO RETURN. Before it an offer's failure is that
// donor's refusal and the join moves on, and this node's own ends it as
// [ErrJoinerFault]. After it the join is an adoption, never followed by another
// install: a NON-ZERO MANIFEST MEANS INSTALLED, error or not, and carries the
// pin ([Adoption.Release]); see [ErrAdoptionUnrecorded], [ErrEstateNotRestored].
func (a *Adopter) Join(ctx context.Context) (Adoption, error) {
	req, err := a.deps.Need(ctx)
	if err != nil {
		return Adoption{}, fmt.Errorf("statelog: read what this node needs: %w", err)
	}
	req.NodeID = a.deps.NodeID

	// Donors are listed inside the collection's window ([collectOffers]), not
	// before it: a slow coordination store would hold this node, refusing
	// reads and writes while it joins, for the listing plus the window.
	var expect func(context.Context) ([]string, error)
	if a.deps.Donors != nil {
		expect = func(listing context.Context) ([]string, error) {
			named, listErr := a.deps.Donors(listing)
			if listErr != nil && ctx.Err() == nil {
				a.log.WarnContext(ctx, "statelog_donors_unknown",
					"node", a.deps.NodeID, "error", listErr.Error(),
					"detail", "which donors to wait for could not be read within "+
						"the offer window, so the join collected offers for all of it")
			}
			return named, listErr
		}
	}
	offers, err := collectOffers(ctx, a.deps.Conn, req, OfferWindow, expect)
	if err != nil {
		return Adoption{}, err
	}
	// The adoption begins once every offer is in: one instant for every offer
	// this join tries, so one join writes one row.
	began := a.now().UTC()

	known, err := store.KnownMigrations(store.EstateReplicated)
	if err != nil {
		return Adoption{}, fmt.Errorf("statelog: read this binary's schema: %w", err)
	}
	var refusals []error
	for _, offer := range offers {
		if err := offer.Usable(req, a.deps.Domains, known); err != nil {
			refusals = append(refusals, fmt.Errorf("%s: %w", offer.Manifest.NodeID, err))
			// LOGGED as well as joined: the engine's no-offer branch
			// does not say what it turned down. This step-2 refusal is
			// what a rolling upgrade reaches (a donor running fewer
			// domains is refused every tick until upgraded), and an
			// operator needs the peer and the domain named.
			a.log.WarnContext(ctx, "statelog_adoption_refused",
				"node", a.deps.NodeID, "donor", offer.Manifest.NodeID,
				"error", err.Error(), "stage", "manifest",
				"detail", "refused from the manifest alone, before any transfer")
			continue
		}
		release, err := a.adopt(ctx, offer, began)
		if release != nil {
			// THE ARTEFACT IS THE LIVE DATABASE. Checked before the
			// cancellation below: a caller that gave up after the rename
			// still runs the donor's file and must not be told otherwise.
			adopted := Adoption{Manifest: offer.Manifest, Release: release}
			if err != nil {
				return adopted, fmt.Errorf("statelog: adopting %s's "+
					"snapshot: %w", offer.Manifest.NodeID, err)
			}
			return adopted, nil
		}
		// A CALLER THAT GAVE UP IS NOT A DONOR THAT FAILED: as a refusal
		// it would end every offer and come back as [ErrNoOffer], which
		// the engine answers by carrying on while the node is being
		// stopped. Returned, not joined, so [ErrNoOffer] never wraps it.
		if ctx.Err() != nil {
			return Adoption{}, fmt.Errorf("statelog: the join was abandoned "+
				"adopting %s's snapshot: %w: %w", offer.Manifest.NodeID, ctx.Err(), err)
		}
		// Nor is a node that lost its own database ([ErrEstateNotRestored]).
		if errors.Is(err, ErrEstateNotRestored) {
			return Adoption{}, fmt.Errorf("statelog: adopting %s's snapshot: %w",
				offer.Manifest.NodeID, err)
		}
		// Nor a fault of this node's own ([nodeFault]): every remaining
		// offer would be fetched in full only to meet it.
		if fault := (*nodeFault)(nil); errors.As(err, &fault) {
			return Adoption{}, fmt.Errorf("%w — met adopting %s's snapshot, and "+
				"every other offer would meet it too: %w", ErrJoinerFault,
				offer.Manifest.NodeID, err)
		}
		refusals = append(refusals, fmt.Errorf("%s: %w", offer.Manifest.NodeID, err))
		a.log.WarnContext(ctx, "statelog_adoption_refused",
			"node", a.deps.NodeID, "donor", offer.Manifest.NodeID, "error", err.Error(),
			"stage", "transfer",
			"detail", "the artefact was fetched or inspected and did not survive it")
	}
	if len(refusals) == 0 {
		return Adoption{}, fmt.Errorf("%w: nobody answered", ErrNoOffer)
	}
	return Adoption{}, fmt.Errorf("%w: %w", ErrNoOffer, errors.Join(refusals...))
}

// adopt runs steps 1 and 3 through 10 for one offer, recording each phase
// under began. A non-nil keep means the rename happened, and is the release of
// the pin on its tail, beside any failure; nil means nothing was installed and
// the pin is released. A failure before the rename that is this node's own is
// marked [ownFault], which is how [Adopter.Join] tells it from the offer's.
func (a *Adopter) adopt(ctx context.Context, offer Offer, began time.Time) (keep func(), err error) {
	// 1. THE HOLD, BEFORE THE FETCH ([AdoptDeps.Hold]).
	release, err := a.deps.Hold(ctx, offer.Manifest.Domains)
	if err != nil {
		return nil, ownFault(fmt.Errorf("statelog: pin the replay tail: %w", err))
	}
	// Released here only if nothing was installed ([AdoptDeps.Hold]).
	installed := false
	defer func() {
		if !installed {
			release()
		}
	}()

	part := a.deps.LivePath + AdoptPartSuffix
	// AN EARLIER ATTEMPT'S PART FILE IS DEBRIS, not a resume point, and so
	// are its sidecars: one left would be refused as an existing destination
	// for ever, and a stale -wal is applied to a fresh artefact once step 4
	// opens it. [store.RemoveCopy] owns the list of what a copy grows.
	if err = store.RemoveCopy(part); err != nil {
		return nil, ownFault(fmt.Errorf("statelog: clear an earlier attempt's "+
			"artefact: %w", err))
	}

	// 3. TRANSFER. The fetch marks a failure writing the copy as this node's;
	// anything else is the donor's or the link's.
	fetched, err := FetchArtefact(ctx, a.deps.Conn, offer, part, a.log)
	if err != nil {
		return nil, err
	}
	// Discarded on every path, the install's included (the rename leaves
	// nothing behind). A failure here is reported by the next attempt's
	// clear above.
	defer func() { _ = store.RemoveCopy(part) }()

	// 4. INSPECT, READ-ONLY, before anything migrates it: migrating first
	// would answer the question by changing it.
	schema, err := store.PendingEstate(ctx, store.EstateReplicated, part, store.Options{})
	if err != nil {
		return nil, fmt.Errorf("statelog: inspect the artefact: %w", err)
	}
	for _, applied := range offer.Manifest.Migrations {
		if !slices.Contains(schema.Applied, applied) {
			return nil, fmt.Errorf("statelog: the artefact's manifest "+
				"claims migration %q and the file does not have it — a metadata "+
				"claim the file does not keep is a corrupt snapshot", applied)
		}
	}
	known, err := store.KnownMigrations(store.EstateReplicated)
	if err != nil {
		return nil, ownFault(fmt.Errorf("statelog: read this binary's schema: %w", err))
	}
	// A MIGRATION THE FILE HAS AND THIS BINARY DOES NOT is the refusal that
	// matters (the pending list answers the opposite question): this node can
	// neither reason about those rows nor migrate them forward.
	if ahead := aheadOf(schema.Applied, known); len(ahead) > 0 {
		return nil, fmt.Errorf("statelog: the artefact carries migrations "+
			"this binary does not: %s — its rows are shaped by code this node "+
			"does not run", words.And(ahead))
	}

	// 5. VERIFY the bytes, hashed as the transfer wrote them (a read-back
	// is as long as the transfer, inside a boot's recovery, and cannot be
	// cancelled), and the positions the file actually keeps.
	if fetched.SHA256 != offer.Manifest.SHA256 {
		return nil, fmt.Errorf("statelog: the artefact hashes to %s and "+
			"its manifest says %s — a transfer that dropped or reordered a chunk "+
			"arrives looking exactly like one that did not", fetched.SHA256, offer.Manifest.SHA256)
	}
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if err := a.verifyPositions(ctx, part, offer.Manifest); err != nil {
		return nil, err
	}

	// 6. VERIFY THE SCRUB, on the still-temporary file.
	empty, err := store.EmptyTables(ctx, part, offer.Manifest.Scrubbed)
	if err != nil {
		return nil, err
	}
	if len(empty) != len(offer.Manifest.Scrubbed) {
		return nil, fmt.Errorf("statelog: the artefact claims %s scrubbed "+
			"and only %s are empty — the safety argument for accepting it at all "+
			"is that everything still in it is fleet-visible",
			words.And(offer.Manifest.Scrubbed), words.And(empty))
	}
	// This node's own store, before any install: a refusal here meets every
	// offer, so the join ends rather than fetching the next.
	if err := a.deps.Record(ctx, began, offer.Manifest.NodeID, offer.Manifest, AdoptionScrubbed); err != nil {
		return nil, ownFault(fmt.Errorf("statelog: record the adoption: %w", err))
	}

	// 7. RE-CHECK the floor: a belt behind the hold, since a fleet that
	// trimmed past the artefact anyway must not be followed into a hole.
	if a.deps.StillUsable != nil {
		if err := a.deps.StillUsable(ctx, offer.Manifest); err != nil {
			return nil, fmt.Errorf("statelog: the artefact stopped being "+
				"usable during the transfer: %w", err)
		}
	}

	// 8. NAME THE COPY, after every check of the file and before the
	// rename — see [stampEstateInstance] for why on both counts.
	instance, stampErr := stampEstateInstance(ctx, part, a.now())
	if stampErr != nil {
		return nil, stampErr
	}

	// 9. INSTALL, both databases closed ([AdoptDeps.Close]). A failed close
	// or install is this node's and nothing was renamed, so each unwinds to
	// the live file and ends the join: the next offer would meet the same.
	if err := a.deps.Close(ctx); err != nil {
		// A failed close may still have closed, so it unwinds as a failed
		// install does.
		return nil, a.rollback(ctx, part, ownFault(fmt.Errorf("statelog: close "+
			"the database being replaced: %w", err)))
	}
	if err := store.AdoptFile(ctx, a.deps.LivePath, part); err != nil {
		// Not renamed: reopening the live file is the whole recovery.
		return nil, a.rollback(ctx, part, ownFault(err))
	}
	// INSTALLED: every return below is an adoption ([Adopter.Join]) and
	// carries the pin out.
	installed = true
	if err := a.deps.Reopen(ctx); err != nil {
		// FORWARD PROGRESS, NOT A ROLLBACK, so it keeps the caller's
		// context: the artefact is the live file, and whatever opens the
		// estate next opens it. The row stays incomplete, as
		// [AdoptionScrubbed] left it; the caller must hear the estate is shut.
		return release, fmt.Errorf("%w: the artefact is installed and "+
			"opening it failed: %w", ErrEstateNotRestored, err)
	}
	// 10. COMPLETE, through the installed phase. A refusal here is the
	// history row's; the database is live and open, so the adoption stands.
	phase, recordErr := AdoptionInstalled, a.deps.Record(ctx, began,
		offer.Manifest.NodeID, offer.Manifest, AdoptionInstalled)
	if recordErr == nil {
		phase, recordErr = AdoptionComplete, a.deps.Record(ctx, began,
			offer.Manifest.NodeID, offer.Manifest, AdoptionComplete)
	}
	// THE ONE LINE AN ADOPTION WRITES, whether or not its row advanced: the
	// checksum matches it to the donor's `statelog_snapshot_sent`, taken_at
	// is how old the installed history is, and instance is the copy's new
	// name, which a reader rebuilding on seeing it logs beside its own line.
	a.log.InfoContext(ctx, "statelog_adopted",
		"node", a.deps.NodeID, "donor", offer.Manifest.NodeID,
		"sha256", offer.Manifest.SHA256, "taken_at", offer.Manifest.TakenAt,
		"bytes", offer.Manifest.Bytes, "domains", len(offer.Manifest.Domains),
		"instance", instance)
	if recordErr != nil {
		a.log.ErrorContext(ctx, "statelog_adoption_unrecorded",
			"node", a.deps.NodeID, "donor", offer.Manifest.NodeID,
			"sha256", offer.Manifest.SHA256, "phase", string(phase),
			"error", recordErr.Error(),
			"detail", "the artefact is installed and open and this node serves "+
				"on it; its row in statelog_adoption stays incomplete because "+
				"advancing it failed, so the row reads as an adoption that "+
				"stopped partway when it did not")
		return release, fmt.Errorf("%w at phase %q: %w", ErrAdoptionUnrecorded,
			phase, recordErr)
	}
	return release, nil
}

// rollback undoes an install that stopped before its rename: discarding the
// artefact at part and reopening the live file is the whole recovery. It takes
// [context.WithoutCancel], as the failure it undoes is often the cancellation
// itself and a reopen under it would leave no estate open. Its own failure is
// [ErrEstateNotRestored] carrying both causes, the worse outcome and the one
// the caller must act on.
//
// THE ABANDONED ARTEFACT GOES FIRST: a whole copy of the estate on the live
// file's volume, it would take exactly the room a reopen on a full disk needs,
// losing the database to a file already given up on.
func (a *Adopter) rollback(ctx context.Context, part string, cause error) error {
	// Not this unwind's to report: the next attempt's clear names it.
	_ = store.RemoveCopy(part)
	if err := a.deps.Reopen(context.WithoutCancel(ctx)); err != nil {
		return fmt.Errorf("%w — %w; and reopening it failed: %w",
			ErrEstateNotRestored, cause, err)
	}
	return cause
}

// verifyPositions re-reads every checkpoint FROM THE FILE and compares it with
// the manifest: the checkpoint commits with the rows, so only the file's
// position describes the file.
func (a *Adopter) verifyPositions(ctx context.Context, path string, m Manifest) error {
	// Read from the file, never the live database, by the one query
	// snapshots and backups share ([CursorsInFile]).
	cursors, err := CursorsInFile(ctx, path)
	if err != nil {
		return fmt.Errorf("statelog: open the artefact: %w", err)
	}
	for name, want := range m.Domains {
		got, ok := cursors[want.Stream]
		if !ok {
			// NO CHECKPOINT ROW IS THE ZERO POSITION: a domain nobody
			// has written to applied nothing, and the donor stamps it
			// at zero. Any other claim describes another file.
			if want.Seq == 0 && want.Generation == 0 {
				continue
			}
			return fmt.Errorf("statelog: the artefact's manifest names %s at "+
				"generation %d sequence %d and the file holds no checkpoint for "+
				"it at all — a metadata claim the file does not keep is a corrupt "+
				"snapshot", name, want.Generation, want.Seq)
		}
		if got.Position.Seq != want.Seq || got.Position.Generation != want.Generation {
			return fmt.Errorf("statelog: the artefact's manifest names %s at "+
				"generation %d sequence %d and the file says %d/%d — a metadata "+
				"claim the file does not keep is a corrupt snapshot",
				name, want.Generation, want.Seq,
				got.Position.Generation, got.Position.Seq)
		}
		// AND THE IDENTITY, a checkpoint field like the other two: a
		// stream instance the file was not applying is another artefact,
		// the shape of a donor taking its position from the file and its
		// identity from its live broker handle. A ZERO IN EITHER IS NO
		// CLAIM: the column post-dates some rows, and a domain at the zero
		// position applied nothing.
		if IdentityOf(got.StreamCreatedAt, want.StreamCreatedAt, true) == StreamRecreated {
			return fmt.Errorf("statelog: the artefact's manifest says %s was "+
				"applying the stream created at %s and the file says %s — a "+
				"metadata claim the file does not keep is a corrupt snapshot",
				name, want.StreamCreatedAt.UTC().Format(time.RFC3339Nano),
				got.StreamCreatedAt.UTC().Format(time.RFC3339Nano))
		}
	}
	return nil
}

// aheadOf is what the artefact has and this binary does not.
func aheadOf(applied, known []string) []string {
	var out []string
	for _, v := range applied {
		if !slices.Contains(known, v) {
			out = append(out, v)
		}
	}
	return out
}
