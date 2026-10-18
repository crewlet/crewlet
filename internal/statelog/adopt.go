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
	// Domains are every log this build registers, by name — every data
	// node runs them all. An artefact names all of them or it is refused:
	// adopted wholesale means a domain it does not name is one this node
	// would believe it was caught up on.
	Domains map[string]Registered

	// LivePath is the replicated estate this node is running, which the
	// artefact replaces.
	LivePath string

	// NodeID is who is joining.
	NodeID string

	// Conn is the transfer's own connection.
	Conn *nats.Conn

	// Donors answers the donors this join expects to hear from, so its
	// collection ends once each has answered rather than at the
	// [OfferWindow] ([CollectOffers]). It must name EVERY node that can
	// offer, because the collection stops on the last one it names and an
	// offer arriving after that is not waited for: the engine names every
	// live data node — each runs a donor — and every node whose fresh
	// register row names an artefact, which is how a DRAINING node is
	// named, since it gives up its presence first and goes on serving its
	// donor until its state log stops.
	//
	// Nil, or an error, waits out the window: an expectation is a way to
	// stop early and never a reason not to ask. It is called WHILE the
	// offers are collected, on a context the window ends, and must return
	// once that context does — a listing still running when the window
	// closes is abandoned rather than waited for.
	Donors func(ctx context.Context) ([]string, error)

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
	// release. The pin must RENEW ITSELF until released — the trim ignores
	// one older than [TrimHoldStale], and a transfer outlives that many
	// times over — which is what [HoldTail] does.
	//
	// WITHOUT IT THE TRIM CAN PASS THE ARTEFACT while it is in flight, and
	// the node installs a snapshot whose tail is already gone — which is
	// the state it was adopting to escape.
	//
	// THE ADOPTER RELEASES IT ONLY WHEN NOTHING WAS INSTALLED — a refused
	// offer, an abandoned join, a fault before the rename. Once the
	// artefact is installed, the tail it pins is the one this node is about
	// to replay, and nothing in the fleet counts that yet: a boot's join runs
	// before the node holds a presence lease and before it has published a
	// position, and a running node's published row still names the
	// checkpoint the artefact replaced. So the release is handed to the
	// caller in the [Adoption], to call once this node's own published
	// position counts the tail — released at the end of the join, the pin
	// left the installed artefact's tail unprotected until a row of the
	// node's own did.
	//
	// at is the ARTEFACT'S OWN POSITIONS, by domain, each with the
	// generation its sequence belongs to — the manifest's, which
	// [Offer.Usable] has held equal to the live stream's. The pin must be
	// stated in that number space and no other: a hold at a generation
	// below the log's is read by the trim as an unknown term and blocks the
	// whole log, and a joiner's own checkpoint is exactly such a
	// generation — none at all for a node that never ran, the old one for a
	// node a reanchor passed.
	Hold func(ctx context.Context, at map[string]DomainPosition) (release func(), err error)

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

	// Record writes this node's own adoption row at each phase: the row is
	// this node's history of what it installed, and a crash mid-adoption
	// must read as one — see [RecordAdoption].
	//
	// WHAT A FAILED WRITE MEANS DEPENDS ON WHICH SIDE OF THE INSTALL IT
	// FELL. Before the rename ([AdoptionScrubbed]) nothing is installed and
	// the fault is this node's own store, which every other offer would meet
	// too — so the join ENDS there, as [ErrJoinerFault], naming the fault and
	// blaming no donor.
	// After it ([AdoptionInstalled], [AdoptionComplete]) the artefact IS
	// the live database and is open, so the join ends ADOPTED, with
	// [ErrAdoptionUnrecorded] beside the manifest it installed: fetching and
	// installing another offer over it would be a second transfer to land on
	// an older artefact, and nobody could donate is false.
	//
	// began is the adoption's start, the same at every phase of one join,
	// and the ADOPTER stamps it rather than the caller: it is the key that
	// makes one join one row, it follows every offer the join collected,
	// and only [Adopter.Join] knows when the last one was in.
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
//
// AN INCOMPLETE ROW IS ONE OF TWO THINGS, and the log says which: a join that
// stopped before it finished — a crash, a refusal of the artefact after it was
// verified, a failed install — or an install whose own record could not be
// advanced, which is a completed adoption the node serves on and which logged
// `statelog_adoption_unrecorded` ([ErrAdoptionUnrecorded]).
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
	// replay tail. It is the CALLER'S to call — once this node's own
	// published position is no further along any log than the rows it now
	// holds, which is the moment the trim counts what the pin protects — and
	// nil exactly when Manifest is zero, the adopter having released the pin
	// itself. See [AdoptDeps.Hold].
	Release func()
}

// ErrNoOffer reports a join that found no artefact it could use.
//
// It is a statement about the FLEET — every offer was refused for something
// about the offer or its artefact, or nobody offered — so it is the answer to
// nothing else a join can end in:
//
//   - A join whose context ended comes back as an error wrapping ctx.Err()
//     whichever step it interrupted, because a caller acts on this one by
//     carrying on without a snapshot, and a caller that gave up is not
//     carrying on at all.
//   - A join that lost this node's own database is [ErrEstateNotRestored], for
//     the same reason: there is nothing to carry on with.
//   - A join that failed on THIS NODE'S OWN FAULT before anything was
//     installed — its trim hold refused, its disk or its own store failing a
//     write — ends at that offer as [ErrJoinerFault], naming the fault,
//     because every other offer would meet the same fault and each donor would
//     be logged as refusing a node that could take nothing.
//   - A join that INSTALLED an artefact is an adoption, whatever failed after
//     the rename: see [ErrAdoptionUnrecorded].
var ErrNoOffer = errors.New("statelog: no usable snapshot was offered")

// ErrAdoptionUnrecorded reports a join that installed an artefact and opened
// it, and could not advance this node's own adoption row past the install.
//
// AN ADOPTION, NOT A FAILURE, and the one error [Adopter.Join] returns beside
// the manifest it installed. The rename is the join's point of no return: from
// it the artefact is the live database, open, with the checkpoint every
// applier resumes from. What failed is the HISTORY row in the node estate
// ([RecordAdoption]) — a separate file whose write can be refused for reasons
// of its own (the store's write lock not granted in time, a full disk) — and
// nothing on the write path reads that row. Filed as the donor's refusal it was
// worse than wrong: the join fetched and installed the next offer over the
// artefact already live — a second transfer, landing on an artefact no newer
// than the first, since offers are ranked newest first — or, with none left,
// reported that nobody could donate while a donor's database was live, and a
// running node then skipped everything an adoption owes its consumers and its
// open sockets.
//
// So a caller treats it as adopted and acts on the manifest. The row stays as
// the last phase that was written left it — incomplete — and the adopter logs
// `statelog_adoption_unrecorded` naming why, beside its `statelog_adopted`.
var ErrAdoptionUnrecorded = errors.New("statelog: the artefact is installed " +
	"and open, and this node's record of the adoption could not be advanced")

// ErrJoinerFault reports a join that ended before anything was installed, on a
// fault of THIS NODE'S OWN rather than of the offer — the trim-hold register it
// pins through refusing the pin, its disk refusing the copy or the stamp that
// names it, its own store refusing the adoption's first row, a close or an
// install that failed and was rolled back to the live file.
//
// NEITHER A DONOR'S REFUSAL NOR [ErrNoOffer], and the join ends at the offer
// that met it: every other offer would be fetched in full only to meet the same
// fault, each donor logged as refusing a node that could take nothing, and the
// whole join reported as a fleet with nothing to give.
//
// NOR [ErrEstateNotRestored]: the live database was never closed, or was
// reopened by the rollback, so it is open and untouched. The node is exactly as
// it was before it asked — behind, on its own intact rows — and a caller
// carries on from there as it does from [ErrNoOffer], saying whose fault it was:
// a boot comes up behind and asks again, a running node asks again on its
// interval. Some of these faults pass (a coordination store that refused one
// write), and the rest (a volume with no room for the copy) are for an operator
// to fix, which the wrapped error names.
var ErrJoinerFault = errors.New("statelog: this node could not adopt a snapshot, " +
	"for a fault of its own")

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
// installed through the same close-and-reopen bracket around an estate that is
// not there, so every remaining donor would be logged as refusing a node that
// could not have taken anything from it. When the rename had happened, [Adopter.Join]
// returns the manifest it installed beside this error: the artefact is the
// live file, and whatever opens the estate next opens it.
var ErrEstateNotRestored = errors.New("statelog: the live replicated database " +
	"a join closed for its install is not open again")

// nodeFault marks a join failure that is THIS NODE'S rather than the offer's —
// its own disk, its own store, the trim-hold register it pins through — and
// that happened before anything was installed.
//
// UNEXPORTED, because it marks a step's failure inside the adopter and the
// fetch, and what a caller branches on is the join's own answer:
// [Adopter.Join] files a failure carrying it as [ErrJoinerFault]. Filed as a
// refusal it was the donor's — logged `statelog_adoption_refused` against a
// peer whose artefact was fine — and every remaining offer was then fetched in
// full only to meet the same fault, ending in [ErrNoOffer], "nobody could
// donate", for a fleet that had donated perfectly well.
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

// Join runs the whole sequence and reports the manifest it adopted.
//
// # The order, and what each step is for
//
//  1. HOLD the replay tail at the artefact's own position, for every domain,
//     before anything is fetched — the trim's minimum-hold term then covers
//     the tail for as long as the pin is held, since it renews itself.
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
//  8. NAME THE COPY: mint the artefact a fresh estate instance, so whatever
//     this node derives from the rows by a forward walk sees the replacement
//     from the file itself — whether or not the process that installed it is
//     still running when it next looks ([EstateInstance]).
//  9. INSTALL, which is the one place the engine replaces a live database.
//  10. COMPLETE, and hand the hold to the caller, who releases it once this
//     node's own position counts the tail ([AdoptDeps.Hold]).
//
// # Which side of the install a failure fell on
//
// THE RENAME IS THE POINT OF NO RETURN, and every failure is filed by which
// side of it it fell on. Before it, a failure about the OFFER — its transfer,
// its bytes, its schema, its positions, its scrub, a trim that passed it — is
// that donor's refusal and the join moves on to the next offer, while a
// failure of THIS NODE'S OWN — its hold, its disk, its own store — ends the
// join there as [ErrJoinerFault], naming the fault, because every other offer
// would meet it too.
// After it, the join is an adoption whatever failed next: Join returns the
// manifest it installed, beside [ErrAdoptionUnrecorded] when only the history
// row could not be advanced and [ErrEstateNotRestored] when the artefact
// could not be opened, and never moves on to install another offer over it.
// A NON-ZERO MANIFEST MEANS INSTALLED, error or not — and an installed
// adoption carries the pin on its tail, for the caller to release
// ([Adoption.Release]).
func (a *Adopter) Join(ctx context.Context) (Adoption, error) {
	req, err := a.deps.Need(ctx)
	if err != nil {
		return Adoption{}, fmt.Errorf("statelog: read what this node needs: %w", err)
	}
	req.NodeID = a.deps.NodeID

	// THE DONORS IT EXPECTS ARE LISTED BESIDE THE COLLECTION, inside its
	// window ([collectOffers]): listed first, a coordination store that was
	// slow to answer held this node — refusing every read and write while it
	// joins — for the whole listing and then the whole window.
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
	// THE ADOPTION BEGINS HERE, once every offer is in, and this is the
	// instant its row keeps. It is one instant for every offer this join
	// tries, so one join writes one row.
	began := a.now().UTC()

	known, err := store.KnownMigrations(store.EstateReplicated)
	if err != nil {
		return Adoption{}, fmt.Errorf("statelog: read this binary's schema: %w", err)
	}
	var refusals []error
	for _, offer := range offers {
		if err := offer.Usable(req, a.deps.Domains, known); err != nil {
			refusals = append(refusals, fmt.Errorf("%s: %w", offer.Manifest.NodeID, err))
			// LOGGED HERE TOO, not only below. A manifest-level
			// refusal is the CHEAP one — decided from what the peer
			// said about itself, before a byte moves — and it was the
			// one nobody could see: the reason went into the joined
			// error, and the engine's no-offer branch reports that a
			// join found nothing without saying what it turned down.
			//
			// It is also the refusal a rolling upgrade reaches: a
			// donor on a build that runs fewer domains than this one
			// is refused on every tick until it is upgraded, and an
			// operator with no line naming the peer and the domain
			// has nothing to act on.
			a.log.WarnContext(ctx, "statelog_adoption_refused",
				"node", a.deps.NodeID, "donor", offer.Manifest.NodeID,
				"error", err.Error(), "stage", "manifest",
				"detail", "refused from the manifest alone, before any transfer")
			continue
		}
		release, err := a.adopt(ctx, offer, began)
		if release != nil {
			// THE ARTEFACT IS THE LIVE DATABASE — see the doc above.
			// Checked before the cancellation below, because a caller
			// that gave up after the rename is still a node running the
			// donor's file, and an answer saying the join was abandoned
			// would send it on as though its estate were its own.
			adopted := Adoption{Manifest: offer.Manifest, Release: release}
			if err != nil {
				return adopted, fmt.Errorf("statelog: adopting %s's "+
					"snapshot: %w", offer.Manifest.NodeID, err)
			}
			return adopted, nil
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
			return Adoption{}, fmt.Errorf("statelog: the join was abandoned "+
				"adopting %s's snapshot: %w: %w", offer.Manifest.NodeID, ctx.Err(), err)
		}
		// NOR IS A NODE THAT LOST ITS OWN DATABASE — see
		// [ErrEstateNotRestored]. Filed as a refusal it would try every
		// remaining offer against an estate that is not open and come
		// back as [ErrNoOffer], which a boot answers by coming up with
		// no replicated estate at all.
		if errors.Is(err, ErrEstateNotRestored) {
			return Adoption{}, fmt.Errorf("statelog: adopting %s's snapshot: %w",
				offer.Manifest.NodeID, err)
		}
		// NOR A FAULT OF THIS NODE'S OWN ([nodeFault]): nothing was
		// installed, and every remaining offer would be fetched in full
		// only to meet the same disk, the same store or the same hold
		// register — each donor logged as refusing, and the whole join
		// reported as a fleet with nothing to give.
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

// adopt runs steps 1 and 3 through 10 for one chosen offer, recording each
// phase under the adoption's start, began. It reports whether the artefact was
// installed — the rename happened — by handing back the release of the pin on
// its tail, beside any failure; nil means nothing was installed, and the pin
// has been released.
//
// A failure before the rename that is this node's own rather than the offer's
// is marked as one ([ownFault]), which is how [Adopter.Join] tells the two
// apart.
func (a *Adopter) adopt(ctx context.Context, offer Offer, began time.Time) (keep func(), err error) {
	// 1. THE HOLD, BEFORE THE FETCH. Without it the trim can pass the
	// artefact's own position while it is in flight, and this node
	// installs a snapshot whose replay tail is already gone — which is
	// exactly the state it is adopting to escape.
	release, err := a.deps.Hold(ctx, offer.Manifest.Domains)
	if err != nil {
		return nil, ownFault(fmt.Errorf("statelog: pin the replay tail: %w", err))
	}
	// RELEASED HERE ONLY IF NOTHING WAS INSTALLED — see [AdoptDeps.Hold].
	installed := false
	defer func() {
		if !installed {
			release()
		}
	}()

	part := a.deps.LivePath + AdoptPartSuffix
	// A PART FILE FROM A PREVIOUS ATTEMPT IS DEBRIS rather than a resume
	// point, and so are its sidecars: only this path is written here, a
	// partial one that survived would be refused as an existing destination
	// for ever, and a stale -wal beside a fresh artefact is applied to it
	// the moment step 4 opens it. What a copy grows beside itself is the
	// store's list, so [store.RemoveCopy] clears it rather than a name here.
	if err = store.RemoveCopy(part); err != nil {
		return nil, ownFault(fmt.Errorf("statelog: clear an earlier attempt's "+
			"artefact: %w", err))
	}

	// 3. TRANSFER. A failure writing the copy is marked as this node's
	// inside the fetch, and everything else it can fail on is the donor's
	// or the link's.
	fetched, err := FetchArtefact(ctx, a.deps.Conn, offer, part, a.log)
	if err != nil {
		return nil, err
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
	// A MIGRATION THE FILE HAS AND THIS BINARY DOES NOT is the refusal
	// that matters, and it is the opposite question to the one the pending
	// list answers: the artefact's rows are shaped by code this node does
	// not run, so it cannot reason about them and cannot migrate them
	// forward either.
	if ahead := aheadOf(schema.Applied, known); len(ahead) > 0 {
		return nil, fmt.Errorf("statelog: the artefact carries migrations "+
			"this binary does not: %v — its rows are shaped by code this node "+
			"does not run", ahead)
	}

	// 5. VERIFY the bytes — hashed as the transfer wrote them, rather than
	// read back whole: that second read was as long as the transfer again,
	// inside a boot's recovery, and nothing could cancel it — and the
	// positions the file actually keeps.
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
		return nil, fmt.Errorf("statelog: the artefact claims %v scrubbed "+
			"and only %v are empty — the safety argument for accepting it at all "+
			"is that everything still in it is fleet-visible",
			offer.Manifest.Scrubbed, empty)
	}
	// THIS NODE'S OWN STORE, and nothing is installed yet: a write it
	// refuses is refused for every offer, so the join ends here rather than
	// fetching the next one to meet the same refusal.
	if err := a.deps.Record(ctx, began, offer.Manifest.NodeID, offer.Manifest, AdoptionScrubbed); err != nil {
		return nil, ownFault(fmt.Errorf("statelog: record the adoption: %w", err))
	}

	// 7. RE-CHECK. The hold makes this a belt rather than the mechanism,
	// and a fleet that trimmed past the artefact anyway is one this node
	// must not follow into a hole.
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

	// 9. INSTALL. Both databases must be closed: this process's claim is
	// on the PATH rather than the inode, so an open handle would be
	// writing into a file that is no longer at that name.
	// A CLOSE OR AN INSTALL THAT FAILED IS THIS NODE'S — its own database
	// handle, its own disk — and the rename did not happen, so each unwinds
	// to the live file and ends the join: the next offer would be closed
	// and renamed through the same bracket on the same volume.
	if err := a.deps.Close(ctx); err != nil {
		// A CLOSE THAT FAILED MAY STILL HAVE CLOSED — the engine's takes
		// the handle out of service before it closes it — and nothing
		// was renamed, so it unwinds exactly as a failed install does.
		return nil, a.rollback(ctx, part, ownFault(fmt.Errorf("statelog: close "+
			"the database being replaced: %w", err)))
	}
	if err := store.AdoptFile(ctx, a.deps.LivePath, part); err != nil {
		// The database is closed and the rename did not happen, so the
		// live file is still the live file — reopening it is the
		// recovery rather than an extra step.
		return nil, a.rollback(ctx, part, ownFault(err))
	}
	// INSTALLED. Everything below reports an adoption, whatever it reports
	// beside it — see [Adopter.Join] — and carries the pin out with it.
	installed = true
	if err := a.deps.Reopen(ctx); err != nil {
		// FORWARD PROGRESS, NOT A ROLLBACK, so it keeps the caller's
		// context: the artefact is the live file now, and whatever opens
		// the estate next — a running node's restore, or the next start
		// of one being stopped — opens the artefact. The row stays as
		// [AdoptionScrubbed] left it, incomplete, which is what happened.
		// But the estate is not open, and that is what the caller has to
		// be told.
		return release, fmt.Errorf("%w: the artefact is installed and "+
			"opening it failed: %w", ErrEstateNotRestored, err)
	}
	// 10. COMPLETE, through the installed phase. A write refused here is the
	// node estate's — the history row's — and the database it describes is
	// live and open, so the adoption stands and only its record is short.
	phase, recordErr := AdoptionInstalled, a.deps.Record(ctx, began,
		offer.Manifest.NodeID, offer.Manifest, AdoptionInstalled)
	if recordErr == nil {
		phase, recordErr = AdoptionComplete, a.deps.Record(ctx, began,
			offer.Manifest.NodeID, offer.Manifest, AdoptionComplete)
	}
	// THE ONE LINE AN ADOPTION WRITES, carrying which artefact it was: the
	// checksum is what matches it to the donor's `statelog_snapshot_sent`,
	// and the instant it was taken is how old the history it installed is.
	// The engine wrote a second `statelog_adopted` holding those two, and
	// one adoption read as two. Written whether or not the row could be
	// advanced, because the adoption happened either way. The instance is the
	// copy's new name, which a reader that rebuilds on seeing it will log
	// beside its own line.
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
