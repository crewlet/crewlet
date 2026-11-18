package engine

import (
	"context"
	"time"

	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE PARTY REGISTRY'S SECOND REBUILD TRIGGER, the identity directory: a
// suspension is one identity-log record with no publish, so the identity
// applier signals after a batch that moved a seat's standing, and a changed
// reading builds a WHOLE new registry for the same company. Never a diff: one
// on a fresh registry drops every identity it did not touch, one on the live
// registry mutates what a running turn reads. Each data node rebuilds from its
// own rows in its own apply (the change feed reaches only one); a node without
// `data` reads through one ([routedIdentity]) on the net ([DirectoryRefresh]);
// only `crewlet validate` and tests stay chart-only. Every whole rebuild and
// vendor-identity write holds [notifications.rebuilding]: both triggers rebuild
// for one company, which [notifications.registryFor] cannot tell apart, so two
// interleaved could publish the one that read the directory first.

// iamDirectory is the identity estate's read side as the notify seam reads it:
// the engine's adapter, since internal/notify (delivery) and internal/iamdomain
// (people) import neither the other and the engine is where they meet.
type iamDirectory struct{ reader *iamdomain.Reader }

// SeatHolders is the directory's bindings in notify's vocabulary, each seat
// passed through as the row stores it, by handle (ADR-0013): internal/notify
// resolves it in the organization a registry is built from, and resolving it
// here against another company would be a second answer about whose seat it is.
func (d iamDirectory) SeatHolders(ctx context.Context) ([]notify.Holder, error) {
	holders, err := d.reader.SeatHolders(ctx)
	if err != nil {
		return nil, err
	}
	return notifyHolders(holders), nil
}

// notifyHolders is ONE translation for both readings of the directory, this
// node's rows and a data node's ([routedIdentity]), so the two cannot disagree
// about one binding.
func notifyHolders(holders []iamdomain.SeatHolder) []notify.Holder {
	out := make([]notify.Holder, 0, len(holders))
	for _, h := range holders {
		out = append(out, notify.Holder{Seat: h.Seat, Stage: h.Stage, Removed: h.Removed})
	}
	return out
}

// useDirectory hands the party registry this node's identity directory and the
// position its readings are taken at, the identity applier's, so the periodic
// net reads only when the applier moved; a nil position (a data node's rows
// through the router) is read every tick. Called once at boot: by the core
// runtime on a data node, by [Engine.startRoutedDirectory] on one without
// `data`; an engine that calls neither stays chart-only.
func (e *Engine) useDirectory(dir notify.Directory, at func() statelog.Position) {
	e.notify.mu.Lock()
	defer e.notify.mu.Unlock()
	e.notify.directory, e.notify.directoryAt = dir, at
}

// WithheldContacts reports, for the party registry live NOW, whether a human
// seat's contact identities are withheld, so everything an agent is shown (a
// roster, a colleague lookup, an assignment by Slack id) leaves out exactly the
// people an inbound message would not be attributed to. A registry is fixed for
// its life, so a turn that captures this at its start keeps one reading, as it
// keeps one org. A node with no registry yet runs no turn: it withholds nothing.
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
// from; the caller holds [notifications.rebuilding]. An unreadable directory
// CARRIES the live registry's reading forward until the net's retry lands:
// chart-only would hand suspended people's seats back over a store blip, and
// withholding every human seat would silence the company over it. With nothing
// read to carry (the boot registry, or a live one itself built unread) it FAILS
// CLOSED with [notify.Unread], withholding every human seat until the first
// successful read.
func (e *Engine) readStandingLocked(ctx context.Context) notify.Standing {
	dir, at := e.partyDirectory()
	if dir == nil {
		e.notify.directoryFailed = false
		return notify.Standing{}
	}
	// THE POSITION BEFORE THE READ, a floor under what it saw: a record
	// applied between the two is re-read by the net, not assumed held.
	var position statelog.Position
	if at != nil {
		position = at()
	}
	standing, err := notify.ReadStanding(ctx, dir)
	if err != nil {
		e.notify.directoryFailed = true
		live := e.Registry()
		if live == nil || !live.Standing().Consulted() {
			// NOTHING WAS EVER READ: a live registry that consulted no
			// directory was built before this node had one.
			log.WarnContext(ctx, "party_directory_unreadable", "error", err,
				"detail", "this node hasn't read the people directory yet, so "+
					"chat messages aren't matched to seats. Retrying every "+
					DirectoryRefresh.String())
			return notify.Unread()
		}
		log.WarnContext(ctx, "party_directory_unreadable", "error", err,
			"withheld_seats", len(live.Withheld()),
			"detail", "this node couldn't read the identity directory, so chat "+
				"routing uses the copy it last read; it retries every "+
				DirectoryRefresh.String())
		return live.Standing()
	}
	e.notify.directoryFailed, e.notify.readAt = false, position
	return standing
}

// refreshDirectory rebuilds the live registry for the company it was built
// from, if the directory's answer moved. force (the committed hook) reads
// whatever the position says; the periodic net skips the read when the applier
// committed nothing since a read that landed. A registry not yet built is left
// alone: the first index reads the directory itself.
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
		// UNREADABLE keeps what is live; IDENTICAL would rebuild the one serving.
		return
	}
	// FOR THE COMPANY THE LIVE REGISTRY WAS BUILT FROM, not [Engine.Company]:
	// an apply indexes its company before publishing it, and in that window
	// the published one is the outgoing company.
	e.rebuildPartiesLocked(c, standing)
}

// intoLiveRegistry runs register against the live registry, serialised with
// every whole rebuild, and answers what it registered. Unserialised, a rebuild
// that read the vendor cache before a start path resolved a seat's account and
// swapped after its write would publish a registry without that account.
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
// that moved a seat's standing: a NON-BLOCKING signal into one slot, never the
// rebuild, since it runs on the apply loop with the next batch waiting; a burst
// collapses into one rebuild, which reads the directory when it starts, so it
// sees everything committed before the last signal.
func (e *Engine) nudgeDirectory() {
	select {
	case e.directoryNudge <- struct{}{}:
	default:
	}
}

// DirectoryRefresh is how often a node re-reads its directory with nothing
// having signalled: the safety net for a failed reading or rows moved with no
// signal, the committed hook and [stateLog.estateReplaced] being the mechanism.
// Thirty seconds is what the alarm table calls a stall, so a slower net would
// report what it was not fixing. A data node reads only when the identity
// applier's position moved; a node without `data` reads through a data node on
// every tick.
const DirectoryRefresh = 30 * time.Second

// watchDirectory is the directory trigger's loop: the committed hook's signal
// and the periodic net. Not a duty: a node's registry derives from the
// directory IT reads, and under a fleet lease a flap would stop a node
// withdrawing a suspended person's identities.
func (e *Engine) watchDirectory(ctx context.Context) {
	every := DirectoryRefresh
	if e.directoryRefresh > 0 {
		every = e.directoryRefresh
	}
	ticker := time.NewTicker(every)
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
