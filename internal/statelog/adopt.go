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
)

// AdoptPartSuffix is what a fetched artefact is called while it is being
// verified.
//
// BESIDE THE LIVE FILE, in the SAME DIRECTORY, because the install is a rename
// — and a rename is atomic only within one filesystem. An artefact fetched to
// a temporary directory would have to be copied across at the end, which is
// the one moment an interrupted adoption must not be able to leave a mixture.
const AdoptPartSuffix = ".adopt.part"

// AdoptDeps is everything the join needs that it does not own.
type AdoptDeps struct {
	// Domains are every domain this build registers, by name. An artefact
	// names all of them or it is refused: adopted wholesale means a domain
	// it does not name is one this node would believe it was caught up on.
	Domains map[string]Registered

	// LivePath is the replicated estate this node is running, which the
	// artefact replaces.
	LivePath string

	// NodeID is who is joining.
	NodeID string

	// Conn is the transfer's own connection.
	Conn *nats.Conn

	// Need is this node's own acceptance test for an artefact, as the
	// request it would make: per domain, the lowest position an artefact
	// may name, the generation its live stream is on, and that stream's
	// creation instant.
	//
	// THE REQUEST ITSELF RATHER THAN A TUPLE OF MAPS, because the terms
	// grow: it was a sequence, then a sequence and a generation, and a
	// generation cannot tell a rebuilt stream from the one it replaced.
	// [OfferRequest.NodeID] is stamped by the adopter and whatever this
	// returns in it is overwritten.
	Need        func(ctx context.Context) (OfferRequest, error)
	StillUsable func(ctx context.Context, m Manifest) error

	// Hold pins the replay tail for the whole transfer and returns the
	// release.
	//
	// WITHOUT IT THE TRIM CAN PASS THE ARTEFACT while it is in flight, and
	// the node installs a snapshot whose tail is already gone — which is
	// the state it was adopting to escape.
	Hold func(ctx context.Context, at map[string]uint64) (release func(), err error)

	// Close and Reopen bracket the install: both databases must be CLOSED
	// when the rename happens, because this process's own claim is on the
	// path rather than the inode.
	//
	// A FAILED CLOSE IS UNWOUND BY A REOPEN, as a failed install is: a
	// close may take the database out of service before it reports its
	// failure — the engine's does — and the join cannot tell which kind
	// it got. So Reopen must do nothing to a database that is still open.
	Close  func(ctx context.Context) error
	Reopen func(ctx context.Context) error

	// Record writes this node's own adoption row at each phase, and the
	// join fails if it cannot: the row is this node's history of what it
	// installed, and a crash mid-adoption must read as one — see
	// [RecordAdoption].
	//
	// began is the adoption's start, the same at every phase of one join,
	// and the ADOPTER stamps it rather than the caller: it is also the
	// watermark a donor that scrubbed its ledger leaves the artefact
	// with, and what makes it a bound is that it follows every offer the
	// join collected — only [Adopter.Join] knows when the last one was in.
	Record func(ctx context.Context, began time.Time, donor string, m Manifest,
		phase AdoptionPhase) error

	// Logger is where this writes. Nil is the package's own component
	// logger, never silence: see loggerOr for what silence cost.
	Logger *slog.Logger
	Now    func() time.Time
}

// AdoptionPhase is how far a join has got, as [AdoptDeps.Record] is told it.
//
// THE ROW DOES NOT KEEP IT. [RecordAdoption] writes the same columns at every
// phase and stamps completed_at only at [AdoptionComplete], so what survives a
// crash is when the adoption began and whether it finished — not which side
// of the install it stopped on. Nothing on the write path needs to know: the
// ledger's watermark is inside the file, so whichever side of the rename a
// join stopped on, the live file says how far back its own ledger may have
// lost rows. Nothing refuses to serve on an incomplete row; the phase names
// the step in the error a failed Record returns.
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
	logger := loggerOr(d.Logger)
	now := d.Now
	if now == nil {
		now = time.Now
	}
	return &Adopter{deps: d, log: logger, now: now}, nil
}

// ErrNoOffer reports a join that found no artefact it could use.
//
// It is a statement about the FLEET, so it is never the answer to a join whose
// context ended: that comes back as an error wrapping ctx.Err() whichever step
// it interrupted, because a caller acts on this one by carrying on without a
// snapshot, and a caller that gave up is not carrying on at all. Nor is it ever
// the answer to a join that lost this node's own database — that is
// [ErrEstateNotRestored], for the same reason: there is nothing to carry on
// with.
var ErrNoOffer = errors.New("statelog: no usable snapshot was offered")

// ErrEstateNotRestored reports a join that closed the live database for an
// install and could not open it again.
//
// A STATEMENT ABOUT THIS NODE, and the one outcome of a join that must never
// reach its caller as [ErrNoOffer]. The engine answers that one by carrying on
// without a snapshot — a boot comes up on the history it has — and a node
// whose replicated estate is not open has no history to come up on: every
// read, every applier and every hold answers [store.ErrNoEstate] until
// something opens it again. So a boot must stop, naming why, and a running
// node must reopen it before it asks the fleet for anything.
//
// IT ENDS THE JOIN rather than moving on to the next offer. Each offer is
// installed through the same close-and-reopen bracket, and its hold — in the
// engine's wiring — reads this node's checkpoint out of the very estate that
// is gone, so every remaining donor would be logged as refusing a node that
// could not have taken anything from it.
var ErrEstateNotRestored = errors.New("statelog: the live replicated database " +
	"a join closed for its install is not open again")

// Join runs the whole sequence and reports the manifest it adopted.
//
// # The order, and what each step is for
//
//  1. HOLD the replay tail at the artefact's own position, for every domain,
//     before anything is fetched — the trim's minimum-hold term then covers
//     the tail for the whole transfer.
//  2. SELECT from what the fleet offers, refusing every unusable one from its
//     manifest alone: a gigabyte-scale transfer that ends in a refusal is one
//     nobody needed.
//  3. TRANSFER beside the live file, so the install is a same-filesystem
//     rename.
//  4. INSPECT read-only, BEFORE anything migrates the file — a recipient
//     refuses migrations this binary does not carry, and migrating first would
//     answer the question by changing it.
//  5. VERIFY the checksum and the positions the file actually keeps, because
//     the checkpoint commits with the rows and a metadata claim the file does
//     not keep is a corrupt snapshot.
//  6. VERIFY THE SCRUB against the manifest's own list, on the still-temporary
//     file. A claim nobody checks is a claim. And where the donor scrubbed a
//     domain's operation ledger — a build from before the ledger travelled —
//     WRITE THE LOSS INTO THE ARTEFACT, so the file that lost the rows and the
//     watermark that says so are installed by one rename.
//  7. RE-CHECK that every adopted position is still above the floor. The hold
//     makes this a belt; a fleet that trimmed anyway is one this node must not
//     follow into a hole.
//  8. INSTALL, which is the one place the engine replaces a live database.
//  9. COMPLETE, and only then release the hold.
func (a *Adopter) Join(ctx context.Context) (Manifest, error) {
	req, err := a.deps.Need(ctx)
	if err != nil {
		return Manifest{}, fmt.Errorf("statelog: read what this node needs: %w", err)
	}
	req.NodeID = a.deps.NodeID

	offers, err := CollectOffers(ctx, a.deps.Conn, req, OfferWindow)
	if err != nil {
		return Manifest{}, err
	}
	// THE ADOPTION BEGINS HERE, once every offer is in, and this is the
	// instant its row keeps — and the watermark an artefact whose donor
	// scrubbed its ledger is installed with. It is one instant for every
	// offer this join tries, so one join writes one row.
	//
	// IT FOLLOWS EVERY DONOR'S ANSWER, and that is the whole of why it is a
	// bound. A donor offers the artefact it holds WHEN IT ANSWERS, so every
	// artefact this join can install was finished before its donor
	// answered, and so before now. Stamped when the join began, it would
	// precede the ask itself: a donor's snapshotter can finish an artefact
	// between the ask and its answer, and an operation this node published
	// in between would then sit inside that artefact with its ledger row
	// scrubbed by a donor that scrubs, minted after the bound.
	began := a.now().UTC()

	var refusals []error
	for _, offer := range offers {
		if err := offer.Usable(req, a.deps.Domains); err != nil {
			refusals = append(refusals, fmt.Errorf("%s: %w", offer.Manifest.NodeID, err))
			continue
		}
		m, err := a.adopt(ctx, offer, began)
		if err == nil {
			return m, nil
		}
		// A CALLER THAT GAVE UP IS NOT A DONOR THAT FAILED. Filed as a
		// refusal, the cancellation would end every remaining offer the
		// same way, log each donor as refused, and come back as
		// [ErrNoOffer] — which the engine reads as "nobody could donate"
		// and answers by bringing a boot up on the history it has or
		// scheduling a rejoin's retry, where the node is in fact being
		// stopped. Returned rather than joined to the refusals, so
		// [ErrNoOffer] never wraps it.
		if ctx.Err() != nil {
			return Manifest{}, fmt.Errorf("statelog: the join was abandoned "+
				"adopting %s's snapshot: %w: %w", offer.Manifest.NodeID, ctx.Err(), err)
		}
		// NOR IS A NODE THAT LOST ITS OWN DATABASE — see
		// [ErrEstateNotRestored]. Filed as a refusal it would try every
		// remaining offer against an estate that is not open and come
		// back as [ErrNoOffer], which a boot answers by coming up with
		// no replicated estate at all.
		if errors.Is(err, ErrEstateNotRestored) {
			return Manifest{}, fmt.Errorf("statelog: adopting %s's snapshot: %w",
				offer.Manifest.NodeID, err)
		}
		refusals = append(refusals, fmt.Errorf("%s: %w", offer.Manifest.NodeID, err))
		a.log.WarnContext(ctx, "statelog_adoption_refused",
			"node", a.deps.NodeID, "donor", offer.Manifest.NodeID, "error", err.Error())
	}
	if len(refusals) == 0 {
		return Manifest{}, fmt.Errorf("%w: nobody answered", ErrNoOffer)
	}
	return Manifest{}, fmt.Errorf("%w: %w", ErrNoOffer, errors.Join(refusals...))
}

// adopt runs steps 1 and 3 through 9 for one chosen offer, recording each
// phase under the adoption's start, began.
func (a *Adopter) adopt(ctx context.Context, offer Offer, began time.Time) (Manifest, error) {
	at := make(map[string]uint64, len(offer.Manifest.Domains))
	for name, pos := range offer.Manifest.Domains {
		at[name] = pos.Seq
	}
	// 1. THE HOLD, BEFORE THE FETCH. Without it the trim can pass the
	// artefact's own position while it is in flight, and this node
	// installs a snapshot whose replay tail is already gone — which is
	// exactly the state it is adopting to escape.
	release, err := a.deps.Hold(ctx, at)
	if err != nil {
		return Manifest{}, fmt.Errorf("statelog: pin the replay tail: %w", err)
	}
	defer release()

	part := a.deps.LivePath + AdoptPartSuffix
	// A PART FILE FROM A PREVIOUS ATTEMPT IS DEBRIS rather than a resume
	// point, and so are its sidecars: only this path is written here, a
	// partial one that survived would be refused as an existing destination
	// for ever, and a stale -wal beside a fresh artefact is applied to it
	// the moment step 4 opens it. What a copy grows beside itself is the
	// store's list, so [store.RemoveCopy] clears it rather than a name here.
	if err = store.RemoveCopy(part); err != nil {
		return Manifest{}, fmt.Errorf("statelog: clear an earlier attempt's "+
			"artefact: %w", err)
	}

	// 3. TRANSFER.
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if _, err := FetchArtefact(ctx, a.deps.Conn, offer, part); err != nil {
		return Manifest{}, err
	}
	// Discarded on every path, the install's included: the rename moves the
	// file and leaves nothing of it behind for this to find, and every
	// other path leaves the artefact and whatever step 4's reads opened
	// beside it. A failure here is not the caller's — the next attempt's
	// clear above reports it by name, and refuses to fetch over it.
	defer func() { _ = store.RemoveCopy(part) }()

	// 4. INSPECT, READ-ONLY, before anything migrates it.
	schema, err := store.PendingEstate(ctx, store.EstateReplicated, part, store.Options{})
	if err != nil {
		return Manifest{}, fmt.Errorf("statelog: inspect the artefact: %w", err)
	}
	for _, applied := range offer.Manifest.Migrations {
		if !slices.Contains(schema.Applied, applied) {
			return Manifest{}, fmt.Errorf("statelog: the artefact's manifest "+
				"claims migration %q and the file does not have it — a metadata "+
				"claim the file does not keep is a corrupt snapshot", applied)
		}
	}
	known, err := store.KnownMigrations(store.EstateReplicated)
	if err != nil {
		return Manifest{}, fmt.Errorf("statelog: read this binary's schema: %w", err)
	}
	// A MIGRATION THE FILE HAS AND THIS BINARY DOES NOT is the refusal
	// that matters, and it is the opposite question to the one the pending
	// list answers: the artefact's rows are shaped by code this node does
	// not run, so it cannot reason about them and cannot migrate them
	// forward either.
	if ahead := aheadOf(schema.Applied, known); len(ahead) > 0 {
		return Manifest{}, fmt.Errorf("statelog: the artefact carries migrations "+
			"this binary does not: %v — its rows are shaped by code this node "+
			"does not run", ahead)
	}

	// 5. VERIFY the bytes and the positions the file actually keeps.
	digest, err := store.FileDigest(part)
	if err != nil {
		return Manifest{}, err
	}
	if digest != offer.Manifest.SHA256 {
		return Manifest{}, fmt.Errorf("statelog: the artefact hashes to %s and "+
			"its manifest says %s — a transfer that dropped or reordered a chunk "+
			"arrives looking exactly like one that did not", digest, offer.Manifest.SHA256)
	}
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if err := a.verifyPositions(ctx, part, offer.Manifest); err != nil {
		return Manifest{}, err
	}

	// 6. VERIFY THE SCRUB, on the still-temporary file.
	empty, err := store.EmptyTables(ctx, part, offer.Manifest.Scrubbed)
	if err != nil {
		return Manifest{}, err
	}
	if len(empty) != len(offer.Manifest.Scrubbed) {
		return Manifest{}, fmt.Errorf("statelog: the artefact claims %v scrubbed "+
			"and only %v are empty — the safety argument for accepting it at all "+
			"is that everything still in it is fleet-visible",
			offer.Manifest.Scrubbed, empty)
	}
	if err := a.recordScrubbedLedgers(ctx, part, offer.Manifest, began); err != nil {
		return Manifest{}, err
	}
	if err := a.deps.Record(ctx, began, offer.Manifest.NodeID, offer.Manifest, AdoptionScrubbed); err != nil {
		return Manifest{}, fmt.Errorf("statelog: record the adoption: %w", err)
	}

	// 7. RE-CHECK. The hold makes this a belt rather than the mechanism,
	// and a fleet that trimmed past the artefact anyway is one this node
	// must not follow into a hole.
	if a.deps.StillUsable != nil {
		if err := a.deps.StillUsable(ctx, offer.Manifest); err != nil {
			return Manifest{}, fmt.Errorf("statelog: the artefact stopped being "+
				"usable during the transfer: %w", err)
		}
	}

	// 8. INSTALL. Both databases must be closed: this process's claim is
	// on the PATH rather than the inode, so an open handle would be
	// writing into a file that is no longer at that name.
	if err := a.deps.Close(ctx); err != nil {
		// A CLOSE THAT FAILED MAY STILL HAVE CLOSED — the engine's takes
		// the handle out of service before it closes it — and nothing
		// was renamed, so it unwinds exactly as a failed install does.
		return Manifest{}, a.rollback(ctx, part, fmt.Errorf("statelog: close "+
			"the database being replaced: %w", err))
	}
	if err := store.AdoptFile(ctx, a.deps.LivePath, part); err != nil {
		// The database is closed and the rename did not happen, so the
		// live file is still the live file — reopening it is the
		// recovery rather than an extra step.
		return Manifest{}, a.rollback(ctx, part, err)
	}
	if err := a.deps.Reopen(ctx); err != nil {
		// FORWARD PROGRESS, NOT A ROLLBACK, so it keeps the caller's
		// context: the artefact is the live file now, and whatever opens
		// the estate next — a running node's restore, or the next start
		// of one being stopped — opens the artefact. The row stays as
		// [AdoptionScrubbed] left it, incomplete, which is what happened,
		// and the file carries its own ledger's watermark whichever side
		// of the install it stopped on. But the estate is not open, and
		// that is what the caller has to be told.
		return Manifest{}, fmt.Errorf("%w: the artefact is installed and "+
			"opening it failed: %w", ErrEstateNotRestored, err)
	}
	if err := a.deps.Record(ctx, began, offer.Manifest.NodeID, offer.Manifest, AdoptionInstalled); err != nil {
		return Manifest{}, fmt.Errorf("statelog: record the install: %w", err)
	}

	// 9. COMPLETE.
	if err := a.deps.Record(ctx, began, offer.Manifest.NodeID, offer.Manifest, AdoptionComplete); err != nil {
		return Manifest{}, fmt.Errorf("statelog: complete the adoption: %w", err)
	}
	// THE ONE LINE AN ADOPTION WRITES, carrying which artefact it was: the
	// checksum is what matches it to the donor's `statelog_snapshot_sent`,
	// and the instant it was taken is how old the history it installed is.
	// The engine wrote a second `statelog_adopted` holding those two, and
	// one adoption read as two.
	a.log.InfoContext(ctx, "statelog_adopted",
		"node", a.deps.NodeID, "donor", offer.Manifest.NodeID,
		"sha256", offer.Manifest.SHA256, "taken_at", offer.Manifest.TakenAt,
		"bytes", offer.Manifest.Bytes, "domains", len(offer.Manifest.Domains))
	return offer.Manifest, nil
}

// recordScrubbedLedgers writes, into the artefact at part, that every domain
// ledger its donor scrubbed may have lost every row applied before began.
//
// ONLY WHERE THE MANIFEST SAYS SO. A donor of this build or later scrubs no
// ledger — it travels, with the donor's own watermark beside it — and the
// artefact is installed exactly as it arrived. A donor from before scrubbed
// every ledger, and the manifest's list is its own account of which: the
// adopter holds that list checked against the file already (step 6), so a
// ledger it names is empty and one it does not name is the donor's.
//
// INTO THE ARTEFACT, BEFORE IT IS INSTALLED, never into the live estate after:
// a live estate reopens into service with a publisher already able to read
// it, and a write that landed a moment later would leave a window in which
// the scrubbed ledger's silence reads as conclusive. The artefact is migrated
// by the open here — it may come from a build whose schema predates the
// watermark's table — which is the same migration the reopen would have run.
func (a *Adopter) recordScrubbedLedgers(ctx context.Context, part string, m Manifest,
	began time.Time) error {

	var lost []Domain
	for _, reg := range a.deps.Domains {
		if ops := reg.Domain.OpsTable(); ops != "" && slices.Contains(m.Scrubbed, ops) {
			lost = append(lost, reg.Domain)
		}
	}
	if len(lost) == 0 {
		return nil
	}
	db, err := store.OpenEstate(ctx, store.EstateReplicated, part, store.Options{})
	if err != nil {
		return fmt.Errorf("statelog: open the artefact to record the ledger its "+
			"donor scrubbed: %w", err)
	}
	for _, d := range lost {
		if err := RecordLedgerLoss(ctx, db, d, began); err != nil {
			_ = db.Close()
			return err
		}
	}
	if err := db.Close(); err != nil {
		return fmt.Errorf("statelog: close the artefact after recording the "+
			"ledger its donor scrubbed: %w", err)
	}
	return nil
}

// rollback undoes an install that stopped before its rename: the live file is
// still the live file, so discarding the artefact at part and reopening the
// live file is the whole recovery.
//
// A ROLLBACK, so it takes [context.WithoutCancel]: the failure it undoes is
// routinely the cancellation itself — a Stop or a signal landing while the
// install checkpoints — and a reopen under that dead context fails too,
// leaving a node whose install never happened with no replicated estate open
// at all.
//
// AND ITS OWN FAILURE IS [ErrEstateNotRestored], carrying both causes, because
// that outcome is the worse of the two and the one the caller has to act on:
// it is told the install failed, and must also be told the live database did
// not come back.
//
// THE ABANDONED ARTEFACT GOES FIRST. It is a whole copy of the estate on the
// live file's own volume, and nothing will ever install it, so a reopen run
// beside it competes with it for room — on a full disk, exactly the room the
// reopen needed. In the other order the reopen fails, the artefact is deleted
// a moment later on the way out, and the node has lost its database to a file
// it had already given up on.
func (a *Adopter) rollback(ctx context.Context, part string, cause error) error {
	// A failure to discard is not this unwind's to report: the reopen is
	// what it is for, and the next attempt's clear names what it could not
	// remove.
	_ = store.RemoveCopy(part)
	if err := a.deps.Reopen(context.WithoutCancel(ctx)); err != nil {
		return fmt.Errorf("%w — %w; and reopening it failed: %w",
			ErrEstateNotRestored, cause, err)
	}
	return cause
}

// verifyPositions re-reads every checkpoint FROM THE FILE and compares it with
// the manifest.
//
// The checkpoint commits in the same transaction as the rows, so the position
// inside the file is the only one that describes the file — and a manifest
// that names a different one is describing a different artefact.
func (a *Adopter) verifyPositions(ctx context.Context, path string, m Manifest) error {
	// ONE READ FOR BOTH ARTEFACTS. A snapshot and a backup both name their
	// positions and both must read them from the file rather than from the
	// live database, so the query lives once — see [CursorsInFile].
	cursors, err := CursorsInFile(ctx, path)
	if err != nil {
		return fmt.Errorf("statelog: open the artefact: %w", err)
	}
	for name, want := range m.Domains {
		got, ok := cursors[want.Stream]
		if !ok {
			// NO CHECKPOINT ROW IS THE ZERO POSITION, not a missing
			// one: a domain whose log nobody has written to has
			// applied nothing, and the donor stamps it at zero for
			// that reason. A manifest naming anything else for it is
			// describing a file this is not.
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
		// AND THE IDENTITY, which is a checkpoint field like the other
		// two. A manifest naming a stream instance the file was not
		// applying is describing a different artefact just as surely as
		// one naming the wrong sequence — and it is the shape a donor
		// produced for as long as the position came from the file and
		// the identity came from the donor's live broker handle.
		//
		// A ZERO IN EITHER IS NO CLAIM rather than a mismatch: the
		// column post-dates some rows, and a domain at the zero position
		// was applying nothing.
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
