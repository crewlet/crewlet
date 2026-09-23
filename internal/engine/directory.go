package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE PARTY REGISTRY'S SECOND REBUILD TRIGGER: the identity directory.
//
// # Why a published company is not enough any more
//
// The registry was derived from the org view alone, so it was rebuilt when a
// company was published and at no other moment. Whether a human seat's contact
// identities may route now also depends on the person holding it (see
// internal/notify's directory.go) — and suspending that person is ONE record
// on the identity log, with no org-chart record and no config apply anywhere
// near it. Nothing on the publish path would ever see it.
//
// So the identity applier signals after a committed batch that moved a seat's
// standing, and this file turns the signal into a rebuild: read the directory
// once, and if its answer differs from the reading the live registry was built
// from, build a WHOLE new registry for the same company and swap it in. Never
// a diff — a patch applied to a fresh registry drops every identity it did not
// touch, and one applied to the live registry mutates a value a running turn
// may be reading.
//
// # Every node, from its own applier
//
// Every node that runs the identity domain applies every record, so each one
// rebuilds its OWN registry from its OWN rows within its own apply. The change
// feed would be the wrong carrier for the same reason it is for the chart view:
// it relays a record to one node, and the rest would go on attributing a
// suspended person's messages to their seat.
//
// # A node that runs no identity domain asks the fleet
//
// A seats-only satellite does not apply this domain, so it has no rows to read
// — and its empty copy of the tables must never be read as "nobody holds any
// seat", which would route every human seat by the chart whatever its holder's
// standing ELSEWHERE claimed. Nor may it route by the chart alone: it consumes
// inbound deliveries and runs seats like every other node, so a chart-only
// registry there attributed a suspended person's messages to their seat for
// every delivery it happened to win. It reads the FLEET's directory instead,
// over the broker — see fleetdirectory.go — on the periodic net, since it has
// no applier to signal it. The chart-only reading is left to an engine with no
// native runtime at all, which is `crewlet validate` and a test.
//
// # Serialised, because two triggers now rebuild one pointer
//
// A published company and a directory signal both rebuild the registry, and
// the second rebuilds for the SAME company the first built for — which is the
// one case [notifications.registryFor]'s identity test cannot tell apart. Two
// rebuilds interleaved publish whichever finished last, and that can be the one
// that read the directory first. So every whole rebuild, and every write of a
// vendor identity into a live registry, holds [notifications.rebuilding].

// iamDirectory is the identity estate's read side as the notify seam reads it.
//
// AN ADAPTER, and deliberately the engine's: internal/notify declares the seam
// it consumes and internal/iamdomain answers in its own vocabulary, and neither
// imports the other — the directory is a fact about people and the registry is
// a fact about delivery, and the engine is where the two are entangled.
type iamDirectory struct{ reader *iamdomain.Reader }

// SeatHolders is the directory's bindings in notify's vocabulary.
//
// THE SEAT IDENTITY EACH BINDING NAMES, passed through as the row stores it:
// the handle the seat was CREATED under (ADR-0020). It is internal/notify that
// finds the seat by it in the organization a registry is built from — as the
// request path does — because that is where the organization is, and a copy
// resolved here against some other company would be a second answer about
// whose seat a binding is.
func (d iamDirectory) SeatHolders(ctx context.Context) ([]notify.Holder, error) {
	holders, err := d.reader.SeatHolders(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]notify.Holder, 0, len(holders))
	for _, h := range holders {
		out = append(out, notify.Holder{Seat: h.Seat, Stage: h.Stage, Removed: h.Removed})
	}
	return out, nil
}

// directoryFor is the directory a native runtime's party registry reads, and
// the position its readings are taken at.
//
// THE NODE'S ROLES DECIDE, through what they made of the runtime: a node that
// runs the identity domain reads its own rows, gated on its own applier's
// position; one that does not asks the fleet, with no position to gate on —
// its reading is the fleet's, and a question costs a scatter only every
// [DirectoryRefresh].
func (e *Engine) directoryFor(ctx context.Context, n *native) (
	notify.Directory, func() statelog.Position, error) {

	if n.iamReader != nil {
		return iamDirectory{reader: n.iamReader}, n.iamReader.At, nil
	}
	host, ok := e.backends.Queue.(domainHost)
	if !ok {
		return nil, nil, fmt.Errorf("engine: the stream is %T, which cannot say "+
			"whether the identity log has records, so this node runs no identity "+
			"domain and has no way to read the fleet's", e.backends.Queue)
	}
	iamLog, err := host.DomainLog(ctx, iamdomain.Domain{}.Stream().Name)
	if err != nil {
		return nil, nil, fmt.Errorf("engine: open the identity log to read the "+
			"fleet's directory through: %w", err)
	}
	// THE FLEET'S OWN KEYRING, which is what the answers are signed under:
	// see fleetdirectory.go for why an unsigned answer is an instruction.
	verifier, err := statelog.NewVerifier(holdersSignatureLabel, n.log.ring)
	if err != nil {
		return nil, nil, fmt.Errorf("engine: build the verifier the fleet's "+
			"directory answers are opened with: %w", err)
	}
	return &fleetDirectory{
		ask: e.backends.Queue,
		answerers: func(ctx context.Context) (int, error) {
			return directoryAnswerers(ctx, e.backends.Coord)
		},
		head: func(ctx context.Context) (uint64, error) {
			stats, err := iamLog.Stats(ctx)
			if err != nil {
				return 0, err
			}
			return stats.LastSeq, nil
		},
		verifier: verifier,
	}, nil, nil
}

// useDirectory hands the party registry this node's identity directory, and
// the position its readings are taken at — nil for a directory that has none
// to gate on, which the periodic net then reads on every tick.
//
// Called once, by the native runtime, with what [Engine.directoryFor] chose. An
// engine with no native runtime never calls it and keeps the chart-only
// behaviour.
func (e *Engine) useDirectory(dir notify.Directory, at func() statelog.Position) {
	e.notify.mu.Lock()
	defer e.notify.mu.Unlock()
	e.notify.directory, e.notify.directoryAt = dir, at
}

// WithheldContacts reports, for the party registry live NOW, whether a human
// seat's contact identities are withheld — the reading everything an agent is
// shown takes, so a roster, a colleague lookup and a work assignment by
// somebody's Slack id all leave out exactly the people an inbound message from
// them would not be attributed to.
//
// PINNED: the answer is the registry live at the call, and a registry is fixed
// for its life (a directory that moved is a new registry), so a turn that
// captures this at its start keeps one reading for the whole turn, as it keeps
// one org. A node with no registry yet has published no company and runs no
// turn, so it withholds nothing.
func (e *Engine) WithheldContacts() func(handle string) bool {
	reg := e.Registry()
	if reg == nil {
		return func(string) bool { return false }
	}
	return func(handle string) bool {
		_, withheld := reg.Withholding(handle)
		return withheld
	}
}

// partyDirectory is the directory the registry reads, or nil for chart-only.
func (e *Engine) partyDirectory() (notify.Directory, func() statelog.Position) {
	e.notify.mu.Lock()
	defer e.notify.mu.Unlock()
	return e.notify.directory, e.notify.directoryAt
}

// readStandingLocked takes the one reading of the directory a rebuild is built
// from. The caller holds [notifications.rebuilding].
//
// # An unreadable directory CARRIES the live registry's reading forward
//
// Neither alternative is honest. Chart-only would hand every suspended
// person's seat back to the chart for the length of a store blip — the unsafe
// direction — and withholding every human seat would silence the whole company
// over the same blip. What this node last KNEW is the standing the live
// registry was built from, so that is what the next one is built from; the
// failure is logged, and the periodic net retries it until a reading lands.
//
// # And with nothing to carry, it FAILS CLOSED
//
// The first registry a node builds — at boot, before anything is published —
// has no live registry behind it, and what it used to carry was the zero
// Standing: chart-only, the unsafe direction, for as long as the directory
// stayed unreadable. So a node that has never read its directory builds from
// [notify.Unread] instead, which withholds every human seat until the net's
// first successful read replaces it. The same holds for a live registry that
// was itself built unread: carrying its reading forward carries the closed
// posture, never a chart-only one.
func (e *Engine) readStandingLocked(ctx context.Context) notify.Standing {
	dir, at := e.partyDirectory()
	if dir == nil {
		e.notify.directoryFailed = false
		return notify.Standing{}
	}
	// THE POSITION BEFORE THE READ, so it is a floor under what the read
	// saw: a record applied between the two is one the net re-reads for,
	// rather than one it believes it already has.
	var position statelog.Position
	if at != nil {
		position = at()
	}
	standing, err := notify.ReadStanding(ctx, dir)
	if err != nil {
		e.notify.directoryFailed = true
		live := e.Registry()
		if live == nil || !live.Standing().Consulted() {
			// NOTHING WAS EVER READ — see above. A live registry that
			// consulted no directory was built before this node had one
			// to read, which is the same absence of a reading.
			log.WarnContext(ctx, "party_directory_unreadable", "error", err,
				"detail", "this node has never read its identity directory, so "+
					"no human seat's contact identities route until it does; it "+
					"retries every "+DirectoryRefresh.String())
			return notify.Unread()
		}
		log.WarnContext(ctx, "party_directory_unreadable", "error", err,
			"withheld_seats", len(live.Withheld()),
			"detail", "this node could not read its identity directory, so its "+
				"contact routing keeps the standing it last read; it retries "+
				"every "+DirectoryRefresh.String())
		return live.Standing()
	}
	e.notify.directoryFailed, e.notify.readAt = false, position
	return standing
}

// refreshDirectory rebuilds the live registry for the company it was built
// from, if the directory's answer moved.
//
// force is the committed hook's: a batch that moved a seat's standing is read
// whatever the position says. The periodic net passes false and skips the read
// when the identity applier has committed nothing since the last one and that
// one landed.
//
// A registry nobody has built yet is left alone: the first index reads the
// directory itself, so a signal that arrives during boot has nothing to do.
func (e *Engine) refreshDirectory(ctx context.Context, force bool) {
	e.notify.rebuilding.Lock()
	defer e.notify.rebuilding.Unlock()
	dir, at := e.partyDirectory()
	if dir == nil {
		return
	}
	e.notify.mu.Lock()
	c, live := e.notify.registryFor, e.notify.registry
	e.notify.mu.Unlock()
	if c == nil || live == nil {
		return
	}
	if !force && !e.notify.directoryFailed && at != nil && at() == e.notify.readAt {
		return
	}
	standing := e.readStandingLocked(ctx)
	if e.notify.directoryFailed || standing.Equal(live.Standing()) {
		// UNREADABLE keeps what is live, and IDENTICAL would build a
		// registry byte-for-byte the one already serving.
		return
	}
	// FOR THE COMPANY THE LIVE REGISTRY WAS BUILT FROM, which is not always
	// the published one: an apply indexes its company before it publishes
	// it, and rebuilding for [Engine.Company] in that window would index
	// the outgoing company over the one about to be published.
	e.rebuildPartiesLocked(c, standing)
}

// intoLiveRegistry runs register against the live registry, serialised with
// every whole rebuild, and answers what it registered.
//
// A vendor's start path resolves a seat's account and then writes it into the
// registry. Unserialised, a directory rebuild that read the vendor cache just
// before the resolution landed and swapped just after the write would publish
// a registry without that account — routed to nobody until the next rewire.
func (e *Engine) intoLiveRegistry(register func(*notify.Registry) int) int {
	e.notify.rebuilding.Lock()
	defer e.notify.rebuilding.Unlock()
	reg := e.Registry()
	if reg == nil {
		return 0
	}
	return register(reg)
}

// intoRegistryOf is [Engine.intoLiveRegistry] for a registry built from c and
// no other, reporting whether it was — see [Engine.registryOf].
func (e *Engine) intoRegistryOf(c *Company, register func(*notify.Registry) int) (int, bool) {
	e.notify.rebuilding.Lock()
	defer e.notify.rebuilding.Unlock()
	reg := e.registryOf(c)
	if reg == nil {
		return 0, false
	}
	return register(reg), true
}

// nudgeDirectory is what the identity applier calls after a committed batch
// that moved a seat's standing.
//
// A NON-BLOCKING SIGNAL into one slot, never the rebuild itself, on
// [Engine.nudgeChart]'s terms: it runs on the apply loop's own goroutine with
// the next batch waiting, and a burst of suspensions collapses into the one
// rebuild that follows it — which reads the directory when it starts, so it
// sees everything committed before the last signal.
func (e *Engine) nudgeDirectory() {
	select {
	case e.directoryNudge <- struct{}{}:
	default:
	}
}

// DirectoryRefresh is how often a node re-reads its directory with nothing
// having signalled.
//
// THE SAFETY NET RATHER THAN THE MECHANISM, and the chart view's own figure
// for the chart view's own reasons ([ViewRefresh]): the committed hook is what
// makes a suspension reach contact routing within one apply, and this covers
// the ways the rows move with no hook at all — an adoption that replaces the
// replicated file wholesale, a node that rejoined, a reading that failed. Thirty
// seconds, because that is what the alarm table already calls a stall: a
// registry behind its own directory for longer than that is a fault an
// operator is being told about, so a slower net would report what it was not
// fixing. It costs nothing when idle on a node that runs the domain — the
// identity applier's position is compared first, and the directory is read
// only when it moved. On a node that runs none it is the ONLY trigger, and each
// tick costs one presence listing and one scatter the answering nodes serve
// from a local read: that is how long a suspension takes to reach a satellite.
const DirectoryRefresh = ViewRefresh

// watchDirectory is the directory trigger's loop: the committed hook's signal,
// and the periodic net behind it.
//
// NOT A DUTY, for [Engine.watchChart]'s reason: a node's registry is a
// derivation of its OWN rows, and tying it to a fleet lease would mean a lease
// flap stopped a node withdrawing a suspended person's identities.
func (e *Engine) watchDirectory(ctx context.Context) {
	ticker := time.NewTicker(DirectoryRefresh)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-e.directoryNudge:
			e.refreshDirectory(ctx, true)
		case <-ticker.C:
			e.refreshDirectory(ctx, false)
		}
	}
}
