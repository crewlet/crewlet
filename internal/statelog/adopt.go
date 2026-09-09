package statelog

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/store"
	"github.com/nats-io/nats.go"
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

	// Need and Generations are this node's view of what an artefact must
	// name, per domain: one below the higher of the stream's first
	// surviving sequence and the published trim floor, and the generation
	// its live stream is on.
	Need        func(ctx context.Context) (map[string]uint64, map[string]uint32, error)
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
	Close  func(ctx context.Context) error
	Reopen func(ctx context.Context) error

	// Record writes this node's own adoption row, which must complete
	// before it serves.
	Record func(ctx context.Context, donor string, m Manifest, phase AdoptionPhase) error

	Logger *slog.Logger
	Now    func() time.Time
}

// AdoptionPhase is how far a join has got, and it is PERSISTED rather than
// held in memory.
//
// A node that crashed mid-adoption and came back looks, from its checkpoint
// alone, exactly like a node that is caught up: the checkpoint came from the
// artefact. The row is what tells them apart, and a row that has not completed
// blocks serving whatever the checkpoint says.
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
	logger := d.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	now := d.Now
	if now == nil {
		now = time.Now
	}
	return &Adopter{deps: d, log: logger, now: now}, nil
}

// ErrNoOffer reports a join that found no artefact it could use.
var ErrNoOffer = errors.New("statelog: no usable snapshot was offered")

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
//     file. A claim nobody checks is a claim.
//  7. RE-CHECK that every adopted position is still above the floor. The hold
//     makes this a belt; a fleet that trimmed anyway is one this node must not
//     follow into a hole.
//  8. INSTALL, which is the one place the engine replaces a live database.
//  9. COMPLETE, and only then release the hold.
func (a *Adopter) Join(ctx context.Context) (Manifest, error) {
	need, generations, err := a.deps.Need(ctx)
	if err != nil {
		return Manifest{}, fmt.Errorf("statelog: read what this node needs: %w", err)
	}

	offers, err := CollectOffers(ctx, a.deps.Conn, OfferRequest{
		NodeID:      a.deps.NodeID,
		Need:        need,
		Generations: generations,
	}, OfferWindow)
	if err != nil {
		return Manifest{}, err
	}

	var refusals []error
	for _, offer := range offers {
		req := OfferRequest{Need: need, Generations: generations}
		if err := offer.Usable(req, a.deps.Domains); err != nil {
			refusals = append(refusals, fmt.Errorf("%s: %w", offer.Manifest.NodeID, err))
			continue
		}
		m, err := a.adopt(ctx, offer)
		if err == nil {
			return m, nil
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

// adopt runs steps 1 and 3 through 9 for one chosen offer.
func (a *Adopter) adopt(ctx context.Context, offer Offer) (Manifest, error) {
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
	// A part file from a previous attempt is debris rather than a resume
	// point: only this path is written here, and a partial one that
	// survived would be refused as an existing destination for ever.
	_ = os.Remove(part)

	// 3. TRANSFER.
	if _, err := FetchArtefact(ctx, a.deps.Conn, offer, part); err != nil {
		return Manifest{}, err
	}
	defer func() {
		// Removed on every path that did not install it.
		if _, err := os.Stat(part); err == nil {
			_ = os.Remove(part)
		}
	}()

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
	if err := a.deps.Record(ctx, offer.Manifest.NodeID, offer.Manifest, AdoptionScrubbed); err != nil {
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
		return Manifest{}, fmt.Errorf("statelog: close the database being replaced: %w", err)
	}
	if err := store.AdoptFile(ctx, a.deps.LivePath, part); err != nil {
		// The database is closed and the rename did not happen, so the
		// live file is still the live file — reopening it is the
		// recovery rather than an extra step.
		_ = a.deps.Reopen(ctx)
		return Manifest{}, err
	}
	if err := a.deps.Reopen(ctx); err != nil {
		return Manifest{}, fmt.Errorf("statelog: reopen after the install: %w", err)
	}
	if err := a.deps.Record(ctx, offer.Manifest.NodeID, offer.Manifest, AdoptionInstalled); err != nil {
		return Manifest{}, fmt.Errorf("statelog: record the install: %w", err)
	}

	// 9. COMPLETE.
	if err := a.deps.Record(ctx, offer.Manifest.NodeID, offer.Manifest, AdoptionComplete); err != nil {
		return Manifest{}, fmt.Errorf("statelog: complete the adoption: %w", err)
	}
	a.log.InfoContext(ctx, "statelog_adopted",
		"node", a.deps.NodeID, "donor", offer.Manifest.NodeID,
		"bytes", offer.Manifest.Bytes, "domains", len(offer.Manifest.Domains))
	return offer.Manifest, nil
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
			return fmt.Errorf("statelog: the artefact's manifest names %s at "+
				"position %d and the file holds no checkpoint for it at all",
				name, want.Seq)
		}
		if got.Seq != want.Seq || got.Generation != want.Generation {
			return fmt.Errorf("statelog: the artefact's manifest names %s at "+
				"generation %d sequence %d and the file says %d/%d — a metadata "+
				"claim the file does not keep is a corrupt snapshot",
				name, want.Generation, want.Seq, got.Generation, got.Seq)
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
