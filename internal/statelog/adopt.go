package statelog

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
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
	// Domains are every domain this node RUNS, by name. An artefact names
	// all of them or it is refused: adopted wholesale means a domain it
	// does not name is one this node would believe it was caught up on.
	Domains map[string]Registered

	// Unrun is every domain this BUILD registers and this NODE does not,
	// which is the other direction of the same fact and has the opposite
	// disposition: an artefact naming one of these is adopted, and the
	// domain is STRIPPED OUT of the staged file before it is installed.
	//
	// Refusing such an artefact instead would make a snapshot useless in
	// exactly the topology this exists for — a node that runs every domain
	// is the one with an artefact to give, and a satellite running a
	// subset is the one that needs it. Adopting it whole is the other
	// failure: the satellite would hold, on its own disk, every row of a
	// domain it was deliberately not given, which for a directory of
	// people is the entire point of not running it.
	//
	// The rows AND the checkpoint both go. A checkpoint left behind says
	// this node applied up to a position in a file whose rows are gone, so
	// the day an operator adds the role the applier resumes above every
	// record it needed and never sees them again.
	Unrun []Domain

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
	// A DOMAIN CANNOT BE BOTH, and the two answers are opposite: one makes
	// an artefact that omits it unusable, the other strips it out of one
	// that carries it. A register that said both would scrub a domain this
	// node is about to start an applier for, which is the worst reachable
	// outcome here and is silent — the node comes up on an empty table at
	// position zero and looks merely new.
	for _, domain := range d.Unrun {
		if _, both := d.Domains[domain.Name()]; both {
			return nil, fmt.Errorf("statelog: %q is declared both run and not run "+
				"on this node — an artefact carrying it would be stripped of the "+
				"rows an applier is about to start reading", domain.Name())
		}
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
	req, err := a.deps.Need(ctx)
	if err != nil {
		return Manifest{}, fmt.Errorf("statelog: read what this node needs: %w", err)
	}
	req.NodeID = a.deps.NodeID

	offers, err := CollectOffers(ctx, a.deps.Conn, req, OfferWindow)
	if err != nil {
		return Manifest{}, err
	}

	var refusals []error
	for _, offer := range offers {
		if err := offer.Usable(req, a.deps.Domains); err != nil {
			refusals = append(refusals, fmt.Errorf("%s: %w", offer.Manifest.NodeID, err))
			// LOGGED HERE TOO, not only below. A manifest-level
			// refusal is the CHEAP one — decided from what the peer
			// said about itself, before a byte moves — and it was the
			// one nobody could see: the reason went into the joined
			// error, and the engine's no-offer branch reports that a
			// join found nothing without saying what it turned down.
			//
			// It is also the refusal a fleet reaches by design rather
			// than by accident, now that a node's roles decide which
			// domains it runs: a donor running fewer than this node
			// is refused on every tick, for ever, and an operator
			// with no line naming the peer and the domain has nothing
			// to act on.
			a.log.WarnContext(ctx, "statelog_adoption_refused",
				"node", a.deps.NodeID, "donor", offer.Manifest.NodeID,
				"error", err.Error(), "stage", "manifest",
				"detail", "refused from the manifest alone, before any transfer")
			continue
		}
		m, err := a.adopt(ctx, offer)
		if err == nil {
			return m, nil
		}
		refusals = append(refusals, fmt.Errorf("%s: %w", offer.Manifest.NodeID, err))
		a.log.WarnContext(ctx, "statelog_adoption_refused",
			"node", a.deps.NodeID, "donor", offer.Manifest.NodeID, "error", err.Error(),
			"stage", "transfer",
			"detail", "the artefact was fetched or inspected and did not survive it")
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
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if _, err := FetchArtefact(ctx, a.deps.Conn, offer, part); err != nil {
		return Manifest{}, err
	}
	defer func() {
		// Removed on every path that did not install it.
		//nolint:govet // shadow: scoped to this block; see .golangci.yml
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
	if err := a.deps.Record(ctx, offer.Manifest.NodeID, offer.Manifest, AdoptionScrubbed); err != nil {
		return Manifest{}, fmt.Errorf("statelog: record the adoption: %w", err)
	}

	// 6b. STRIP WHAT THIS NODE DOES NOT RUN, on the staged file, BEFORE
	// the rename — which is the whole reason this step sits here rather
	// than after the install. The install closes both databases precisely
	// because nothing may hold the path while it moves, and the appliers
	// are relaunched onto the new file the moment it does; a delete issued
	// after that is a write into a file a loop is already reading.
	//
	// It is also after the digest and the scrub check deliberately: those
	// verify what the DONOR sent, and a file this node has already edited
	// hashes to something the manifest never claimed.
	stripped, err := a.stripUnrun(ctx, part)
	if err != nil {
		return Manifest{}, err
	}
	if len(stripped) > 0 {
		a.log.InfoContext(ctx, "statelog_artefact_stripped",
			"node", a.deps.NodeID, "donor", offer.Manifest.NodeID,
			"domains", stripped,
			"detail", "the donor runs domains this node does not, so their rows "+
				"and their checkpoints were removed from the copy before it was "+
				"installed")
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

// stripUnrun removes every domain this node does not run from a staged
// artefact: its rows, and its row in the shared checkpoint table.
//
// BOTH HALVES OR NEITHER. Rows without a checkpoint are rows an applier will
// rewrite from the beginning of the log, which is merely slow. A CHECKPOINT
// WITHOUT ROWS is the dangerous residue: it says this node applied up to a
// position, so the day an operator gives the node that role its applier
// resumes above every record whose rows were just deleted and never reads them
// again. The tables go first for that reason — a crash between the two leaves
// the recoverable order.
//
// The rows are DELETED and the tables are KEPT. Dropping them would be the
// obvious reading of "this node does not have that domain", and it is wrong
// here for a reason the schema decides rather than this package: migrations
// key on their FILENAME, so a table dropped out of a file is a table no
// migration will ever recreate — the node that later declares the role would
// find the migration already applied and the table gone, for good. What a
// fresh node has is the tables, empty, and that is what this leaves.
func (a *Adopter) stripUnrun(ctx context.Context, path string) ([]string, error) {
	if len(a.deps.Unrun) == 0 {
		return nil, nil
	}
	var tables, streams, names []string
	for _, domain := range a.deps.Unrun {
		// EVERY TABLE THE DOMAIN DECLARES, whatever its class. Its
		// local ones are already empty — the donor scrubbed them and
		// step 6 checked that — so the delete is a no-op, and naming
		// them anyway means a class that is later reclassified cannot
		// quietly start travelling.
		for table := range domain.Tables() {
			tables = append(tables, table)
		}
		streams = append(streams, domain.Stream().Name)
		names = append(names, domain.Name())
	}
	slices.Sort(tables)
	slices.Sort(names)
	if _, err := store.ScrubFile(ctx, path, tables); err != nil {
		return nil, fmt.Errorf("statelog: strip %v from the artefact: %w", names, err)
	}
	// `statelog_cursor` spelled here as it is in every query that reads it:
	// a constant naming it would be one reference against four literals,
	// and a grep finds all five either way.
	if _, err := store.ScrubRowsIn(ctx, path, "statelog_cursor", "stream", streams); err != nil {
		return nil, fmt.Errorf("statelog: strip %v's checkpoints from the "+
			"artefact: %w", names, err)
	}
	return names, nil
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
