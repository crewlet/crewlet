package engine

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/maintenance"
	"github.com/crewlet/crewlet/internal/mcp"
)

// WHAT THIS NODE'S MCP SERVERS LIST, PUBLISHED; AND WHAT EVERY NODE'S LIST,
// WATCHED (ADR-0074, internal/coord's mcplisting.go).
//
// A per-agent server's children run only on the node holding each seat, so no
// node's registry is the company's catalogue. Each node therefore publishes,
// per server, the union of what its own instances list, and keeps a watched
// copy of every node's, which the tool catalogue and a server's detail are
// read from.
//
// # Derived at each beat, written when it changes
//
// The listing is read off the bridges at every beat, as the heartbeat's MCP
// rows are ([Engine.mcpStatus]): an apply, a claim, a release, a restart that
// lists differently and a seat's pass each move it, and a publisher hooked
// into each of them would be one hook short the day a sixth appears. What
// goes to the store is only a listing whose content moved since this node
// last wrote it, so a quiet company writes nothing.
//
// # Under the presence epoch
//
// Each listing carries the epoch of the presence this node held when it wrote
// it, and readers count it only while that presence holds at that epoch. A
// lapse is the one thing that moves the epoch under a running node, and the
// sweep may remove what was written under the old one, so a new epoch makes
// this node forget what it believed it had written and write it all again.
//
// # Each definition once, and before the listing that names it
//
// A tool's definition, its schemas and its words whole, is stored under its
// digest, create-only, before the listing naming the digest is written, so a
// reader never meets a listing whose definitions were never stored, only (for
// a moment, across a sweep) one whose definition was removed, which it answers
// as unknown. A definition waiting for a review is kept before any other, so a
// change is shown whole to whoever reviews it.
//
// # Each listing waits as long as its bytes take, on a pass of its own
//
// A listing is up to coord.MaxMCPListingBytes and its definitions up to
// coord.MaxMCPListingDefinitionBytes, and on a node whose connection crosses a
// link — a node without `data`, and every node on an external broker, data
// nodes included — they cross it at the link's pace, which the engine allows
// to be as slow as a mebibyte in thirty seconds: a listing at
// its bound alone takes about 26 seconds. The beat once held every step to one
// deadline of [coord.ReconcileInterval], fifteen seconds, so on any link below
// about 60 KiB a second a large listing was never acknowledged, and each beat
// sent the whole of it again while the earlier copies were still crossing.
// So a beat's writes of its listings, and of the definitions they name, carry
// NO deadline of the beat's: each waits as long as the coordination store's
// rule gives its bytes (internal/coord/kv's settled writes) — the request term
// and, across a link, their crossing at the floor for every send the client
// may make of them — and they are made on a pass of their own
// ([Engine.startListingPass]), one at a time, so a slow listing holds no beat:
// the next beat reads the fleet's presence and accepts first listings on time,
// and leaves its listings to the pass in flight, the next beat after it
// writing whatever moved meanwhile. The beat's own steps keep their bound
// ([mcpBeatBound]).

// mcpListingSweepPasses is how many consecutive sweeps must find a definition
// named by no listing before it is removed: TWO, a maintenance interval
// apart, because a node stores a definition before the listing that names it
// and a sweep reading the listings between the two would otherwise remove a
// definition about to be named.
const mcpListingSweepPasses = 2

// mcpBeatBound is how long one beat's own steps may take — reading the fleet's
// presence and the definitions a moved catalogue names, and accepting a
// server's first listing — before the beat gives them up for the next: the
// beat's interval, [coord.ReconcileInterval], so a beat whose broker does not
// answer ends before the next would start. Each carries small records. The
// listings' writes are not held to it ([Engine.startListingPass]).
const mcpBeatBound = coord.ReconcileInterval

// mcpListings is this node's half of the fleet's MCP listings.
type mcpListings struct {
	mu sync.Mutex

	// epoch is the presence epoch the published listings were written
	// under; published is what this node last wrote per server, and
	// ensured the digests whose definitions it knows are stored, kept to
	// the ones its published listings name.
	epoch     int64
	published map[string]publishedListing
	ensured   map[string]struct{}

	// fleet is the watched copy of every node's listing by key, answered
	// once the watch's first answer is in; presence is the live nodes as
	// this node last read them, for the catalogue's filter; notified is
	// the catalogue the surfaces were last told of.
	fleet    map[string]coord.MCPListing
	answered bool
	// answering is the keys the current watch's first answer has
	// delivered, nil once its marker has come.
	answering    map[string]struct{}
	presence     []coord.Lease
	presenceRead bool
	notified     string

	// definitions is every definition this node has read, by digest: a
	// digest names one definition, so an entry is never stale, and one no
	// listing and no accepted definition names any more is dropped.
	definitions map[string]coord.MCPToolDefinition

	// absent is every digest this node read and the store held nothing
	// under, until a listing naming it as kept arrives ([applyMCPListing]):
	// a node stores a definition before the listing that says so, so
	// nothing else makes a missing one appear. applied counts the updates
	// taken off the watch, so a read that overlapped one records no miss.
	absent  map[string]struct{}
	applied uint64

	// unnamed is how many consecutive sweeps this node ran found each
	// stored definition named by no listing ([mcpListingSweepPasses]).
	unnamed map[string]int

	// writing is a pass of this node's listing writes in flight
	// ([Engine.startListingPass]), which a beat leaves its listings to; pass
	// is the pass's goroutine, which the publisher waits for when it stops.
	writing bool
	pass    sync.WaitGroup

	cancel context.CancelFunc
	done   chan struct{}
}

// publishedListing is what this node last wrote for one server.
type publishedListing struct {
	fingerprint string
	version     uint64
}

// startMCPListings opens the watches of the fleet's listings and of what was
// accepted of them (mcpreviews.go), and starts this node's publisher, all kept
// until stopMCPListings.
func (e *Engine) startMCPListings(ctx context.Context) {
	if e.backends == nil || e.backends.Fleet == nil {
		return
	}
	loopCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	e.listings.cancel, e.listings.done = cancel, make(chan struct{})
	// WATCHED FROM HERE, not from whenever the watch's goroutine first runs,
	// so no call is decided in between as if nothing had changed.
	e.watchReviews()
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		// EVERY WATCH'S FIRST ANSWER REPLACES THE COPY: a listing removed
		// while a lost watch was being opened again is in no answer, and
		// is dropped at the marker rather than kept for ever.
		watch := func(ctx context.Context) (<-chan coord.MCPListingUpdate, error) {
			updates, err := e.backends.Fleet.WatchMCPListings(ctx)
			if err == nil {
				e.listings.mu.Lock()
				e.listings.answering = map[string]struct{}{}
				e.listings.mu.Unlock()
			}
			return updates, err
		}
		marker := func(u coord.MCPListingUpdate) bool {
			if u.Current {
				e.mcpListingsAnswered(loopCtx)
			}
			return u.Current
		}
		watchRecords(loopCtx, "mcp_listing", watch, marker, e.applyMCPListing)
	}()
	go func() {
		defer wg.Done()
		e.watchMCPAccepted(loopCtx)
	}()
	go func() {
		defer wg.Done()
		// A PASS IN FLIGHT ENDS WITH THE PUBLISHER: its writes carry the
		// loop's context, which stopping cancels, so the listings a stop
		// removes are never written again behind it.
		defer e.listings.pass.Wait()
		ticker := time.NewTicker(coord.ReconcileInterval)
		defer ticker.Stop()
		for {
			e.publishMCPListings(loopCtx)
			select {
			case <-loopCtx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	go func() {
		wg.Wait()
		close(e.listings.done)
	}()
}

// stopMCPListings ends the watch and the publisher, and removes this node's
// own listings: a node that is going says so rather than leaving the sweep to
// find out. Its readers stopped counting them when its presence went, so a
// removal that fails costs nothing but the sweep's work.
func (e *Engine) stopMCPListings(ctx context.Context) {
	if e.listings.cancel == nil {
		return
	}
	e.listings.cancel()
	<-e.listings.done
	e.listings.mu.Lock()
	published := maps.Clone(e.listings.published)
	e.listings.published = nil
	e.listings.mu.Unlock()
	node := e.nodeID()
	removeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), coord.ReconcileInterval)
	defer cancel()
	for _, server := range slices.Sorted(maps.Keys(published)) {
		if _, err := e.backends.Fleet.DeleteMCPListing(removeCtx, server, node,
			published[server].version); err != nil {
			log.WarnContext(removeCtx, "mcp_listing_not_removed", "server", server, "error", err,
				"detail", "readers stopped counting it with this node's presence; the "+
					"maintenance sweep removes it")
		}
	}
}

// applyMCPListing takes one change off the watch into the watched copy, and
// tells the surfaces that render the catalogue when what they would render
// moved.
func (e *Engine) applyMCPListing(ctx context.Context, u coord.MCPListingUpdate) {
	l := &e.listings
	key := coord.MCPListingKey(u.Server, u.Node)
	l.mu.Lock()
	if l.fleet == nil {
		l.fleet = map[string]coord.MCPListing{}
	}
	if u.Listing != nil {
		l.fleet[key] = *u.Listing
		for _, t := range u.Listing.Tools {
			if t.DefinitionKept {
				delete(l.absent, t.Digest)
			}
		}
	} else {
		delete(l.fleet, key)
	}
	l.applied++
	if l.answering != nil {
		l.answering[key] = struct{}{}
	}
	settled := l.answered && l.answering == nil
	l.mu.Unlock()
	if settled {
		e.fleetCatalogueMoved(ctx)
	}
}

// mcpListingsAnswered ends a watch's first answer: every listing it did not
// deliver is one removed since the copy was last current, and goes.
func (e *Engine) mcpListingsAnswered(ctx context.Context) {
	l := &e.listings
	l.mu.Lock()
	for key := range l.fleet {
		if _, delivered := l.answering[key]; !delivered {
			delete(l.fleet, key)
		}
	}
	l.answering, l.answered = nil, true
	l.mu.Unlock()
	e.fleetCatalogueMoved(ctx)
}

// fleetCatalogueMoved reads the definitions the live listings name that this node
// has not read, and tells the surfaces rendering the catalogue when the live
// listings differ from the ones they were last told of.
func (e *Engine) fleetCatalogueMoved(ctx context.Context) {
	live, ok := e.LiveMCPListings()
	if !ok {
		return
	}
	if _, err := e.MCPToolDefinitions(ctx, namedDigests(live)); err != nil {
		log.WarnContext(ctx, "mcp_tool_definitions_unread", "error", err,
			"detail", "the catalogue lists these tools without their arguments until "+
				"the next change reads them")
	}
	fingerprint := listingsFingerprint(live)
	l := &e.listings
	l.mu.Lock()
	moved := fingerprint != l.notified
	l.notified = fingerprint
	l.mu.Unlock()
	if moved {
		e.notifyCatalogueChanged(ctx)
	}
}

// LiveMCPListings is every listing a live node's current incarnation wrote,
// by this node's watched copy and its last reading of the fleet's presence
// ([coord.LiveMCPListings]), ordered by server and node; false until the watch
// has given its first answer and the presence has been read.
func (e *Engine) LiveMCPListings() ([]coord.MCPListing, bool) {
	l := &e.listings
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.answered || !l.presenceRead {
		return nil, false
	}
	all := slices.Collect(maps.Values(l.fleet))
	coord.SortMCPListings(all)
	return coord.LiveMCPListings(all, l.presence), true
}

// MCPListings is every listing in this node's watched copy, whatever its
// node's presence, ordered by server and node: a caller that reads the
// presence itself filters it with [coord.LiveMCPListings]. An error until the
// watch has given its first answer, which is not a fleet that lists nothing.
func (e *Engine) MCPListings(context.Context) ([]coord.MCPListing, error) {
	l := &e.listings
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.answered {
		return nil, errMCPListingsUnanswered
	}
	all := slices.Collect(maps.Values(l.fleet))
	coord.SortMCPListings(all)
	return all, nil
}

// errMCPListingsUnanswered is a read of the fleet's listings before this
// node's watch has answered.
var errMCPListingsUnanswered = errors.New("engine: this node has not yet read the fleet's MCP " +
	"listings; ask again in a moment")

// HeldMCPToolDefinitions is every definition this node has read, by digest, from
// memory alone: for a caller with no context to read the store under.
func (e *Engine) HeldMCPToolDefinitions() map[string]coord.MCPToolDefinition {
	e.listings.mu.Lock()
	defer e.listings.mu.Unlock()
	return maps.Clone(e.listings.definitions)
}

// MCPToolDefinitions reads the definitions stored under digests: the ones this
// node read before from memory, the rest from the store, but for a digest this
// node already found the store holds nothing under ([mcpListings.absent]). A
// digest the store holds nothing under is left out. An error is a store that
// could not be read; the definitions read before it are kept for the next call.
func (e *Engine) MCPToolDefinitions(ctx context.Context, digests []string) (map[string]coord.MCPToolDefinition, error) {
	out := make(map[string]coord.MCPToolDefinition, len(digests))
	var missing []string
	l := &e.listings
	l.mu.Lock()
	for _, d := range digests {
		if s, ok := l.definitions[d]; ok {
			out[d] = s
		} else if _, gone := l.absent[d]; !gone {
			missing = append(missing, d)
		}
	}
	applied := l.applied
	l.mu.Unlock()
	if len(missing) == 0 || e.backends == nil || e.backends.Fleet == nil {
		return out, nil
	}
	var err error
	read := map[string]coord.MCPToolDefinition{}
	var absent []string
	for _, d := range slices.Compact(slices.Sorted(slices.Values(missing))) {
		s, found, readErr := e.backends.Fleet.MCPToolDefinition(ctx, d)
		if readErr != nil {
			err = readErr
			break
		}
		if found {
			read[d] = s
			out[d] = s
		} else {
			absent = append(absent, d)
		}
	}
	accepted := acceptedDigests(e.acceptedRecords())
	l.mu.Lock()
	if l.definitions == nil {
		l.definitions = map[string]coord.MCPToolDefinition{}
	}
	if l.absent == nil {
		l.absent = map[string]struct{}{}
	}
	maps.Copy(l.definitions, read)
	if l.applied == applied {
		// NO LISTING ARRIVED DURING THE READ that could say one of these
		// was stored since.
		for _, d := range absent {
			l.absent[d] = struct{}{}
		}
	}
	// KEPT TO WHAT SOME LISTING OR ACCEPTED DEFINITION NAMES, so each copy
	// is bounded by the fleet's catalogue rather than by everything it
	// ever listed.
	named := digestSet(slices.Collect(maps.Values(l.fleet)))
	maps.Copy(named, accepted)
	for d := range l.definitions {
		if _, ok := named[d]; !ok {
			delete(l.definitions, d)
		}
	}
	for _, d := range listedDigests(slices.Collect(maps.Values(l.fleet))) {
		named[d] = struct{}{}
	}
	for d := range l.absent {
		if _, ok := named[d]; !ok {
			delete(l.absent, d)
		}
	}
	l.mu.Unlock()
	return out, err
}

// listedDigests is every digest the listings name, kept or not.
func listedDigests(listings []coord.MCPListing) []string {
	var out []string
	for _, l := range listings {
		for _, t := range l.Tools {
			out = append(out, t.Digest)
		}
	}
	return out
}

// publishMCPListings is one beat of this node's publisher: read the fleet's
// presence, derive this node's listings, start a pass writing each that moved
// and removing each for a server this node no longer runs
// ([Engine.startListingPass]), and accept the first listing of each server
// nobody has accepted a listing of ([Engine.baselineMCPTools]). Its own steps
// are held to [mcpBeatBound]; the pass's writes are not.
func (e *Engine) publishMCPListings(ctx context.Context) {
	beatCtx, cancel := context.WithTimeout(ctx, mcpBeatBound)
	defer cancel()
	if presence, err := e.backends.Coord.ListLive(beatCtx, coord.ClassNode); err == nil {
		e.listings.mu.Lock()
		e.listings.presence, e.listings.presenceRead = presence, true
		answered := e.listings.answered
		e.listings.mu.Unlock()
		if answered {
			// A NODE THAT WENT is a catalogue that moved, with nothing
			// on the watch to say so: its listing stays until the
			// sweep, and stopped counting with its presence.
			e.fleetCatalogueMoved(beatCtx)
		}
	} else if ctx.Err() == nil {
		log.WarnContext(ctx, "mcp_listing_presence_unread", "error", err,
			"detail", "the catalogue keeps the fleet's presence as last read")
	}
	if e.node == nil || e.node.Host() == nil {
		return
	}
	// NOT COUNTED, NOT WRITTEN: a node its fleet cannot see would write a
	// listing every reader ignores, under an epoch that is about to move.
	epoch, held := e.node.Host().PresenceEpoch()
	if !held {
		return
	}
	node := e.nodeID()
	now := time.Now()
	desired := e.nodeMCPListings(node, epoch, now)
	e.startListingPass(ctx, node, epoch, desired)
	e.baselineMCPTools(beatCtx, desired, now)
}

// startListingPass starts a pass writing desired under epoch
// ([Engine.writeMCPListings]) on a goroutine of its own, and reports whether
// it did: a pass already in flight is left to finish, and the beat that found
// it starts none, since the pass after it compares what it wrote with what the
// node lists then and writes what moved.
//
// ITS WRITES CARRY NO DEADLINE OF THE BEAT'S: ctx is the publisher's own, which
// stopping cancels and nothing else ends, so each write waits as long as the
// coordination store's rule gives the bytes it carries — the request term and,
// across a link, their crossing at the floor for every send the client may
// make of them (internal/coord/kv's settled writes). A listing at its bound is
// about 26 seconds a send across a link at the floor, which the beat's fifteen
// seconds could never give it; across a node's own connection, the request
// term. ONE PASS AT A TIME, so a slow listing holds no beat and no two passes
// write this node's listings over each other.
func (e *Engine) startListingPass(ctx context.Context, node string, epoch int64,
	desired map[string]nodeListing) bool {

	l := &e.listings
	l.mu.Lock()
	if l.writing {
		l.mu.Unlock()
		return false
	}
	l.writing = true
	l.pass.Add(1)
	l.mu.Unlock()
	go func() {
		defer l.pass.Done()
		defer func() {
			l.mu.Lock()
			l.writing = false
			l.mu.Unlock()
		}()
		e.writeMCPListings(ctx, node, epoch, desired)
	}()
	return true
}

// writeMCPListings brings the fleet's record of this node's listings to
// desired, written under epoch: each listing whose content moved since this
// node last wrote it, its definitions first, and the removal of each for a server
// it no longer lists.
func (e *Engine) writeMCPListings(ctx context.Context, node string, epoch int64,
	desired map[string]nodeListing) {

	fleet := e.backends.Fleet
	l := &e.listings
	l.mu.Lock()
	if l.epoch != epoch {
		// A NEW INCARNATION: what was written under the last epoch may
		// have been swept, and is not counted either way.
		l.epoch, l.published, l.ensured = epoch, map[string]publishedListing{}, map[string]struct{}{}
	}
	if l.published == nil {
		l.published, l.ensured = map[string]publishedListing{}, map[string]struct{}{}
	}
	published, ensured := maps.Clone(l.published), maps.Clone(l.ensured)
	l.mu.Unlock()

	fingerprints := make(map[string]string, len(desired))
	for _, server := range slices.Sorted(maps.Keys(desired)) {
		listing := desired[server]
		fingerprint := listingFingerprint(listing.listing)
		fingerprints[server] = fingerprint
		if published[server].fingerprint == fingerprint {
			continue
		}
		if !e.storeToolDefinitions(ctx, listing, ensured) {
			continue
		}
		stored, err := fleet.PutMCPListing(ctx, listing.listing)
		if err != nil {
			log.WarnContext(ctx, "mcp_listing_not_written", "server", server, "error", err,
				"detail", "the fleet keeps this node's last listing of it; the next beat writes it")
			continue
		}
		published[server] = publishedListing{fingerprint: fingerprint, version: stored.Version}
	}
	for _, server := range slices.Sorted(maps.Keys(published)) {
		if _, still := desired[server]; still {
			continue
		}
		if _, err := fleet.DeleteMCPListing(ctx, server, node, published[server].version); err != nil {
			log.WarnContext(ctx, "mcp_listing_not_removed", "server", server, "error", err,
				"detail", "the next beat removes it")
			continue
		}
		delete(published, server)
	}
	// ENSURED IS KEPT TO WHAT THE STORED LISTINGS NAME: a digest only a
	// listing the store does not hold names (one this node stopped naming,
	// or one a failed write never named there) is one a sweep may remove,
	// so the next write naming it stores it again rather than trusting it.
	var stored []coord.MCPListing
	for server, p := range published {
		if d, ok := desired[server]; ok && p.fingerprint == fingerprints[server] {
			stored = append(stored, d.listing)
		}
	}
	named := digestSet(stored)
	for d := range ensured {
		if _, ok := named[d]; !ok {
			delete(ensured, d)
		}
	}
	l.mu.Lock()
	if l.epoch == epoch {
		l.published, l.ensured = published, ensured
	}
	l.mu.Unlock()
}

// storeToolDefinitions stores, create-only, the definition of every tool in
// listing that ensured does not already name, reporting whether all of them are
// now stored: a listing is written only after the definitions it names.
func (e *Engine) storeToolDefinitions(ctx context.Context, listing nodeListing,
	ensured map[string]struct{}) bool {

	for _, t := range listing.listing.Tools {
		if !t.DefinitionKept {
			continue
		}
		if _, ok := ensured[t.Digest]; ok {
			continue
		}
		definition := listing.definitions[t.Digest]
		if _, err := e.backends.Fleet.CreateMCPToolDefinition(ctx, definition); err != nil {
			log.WarnContext(ctx, "mcp_tool_definition_not_stored", "server", listing.listing.Server,
				"tool", t.Name, "error", err,
				"detail", "the listing naming it waits for the next beat")
			return false
		}
		ensured[t.Digest] = struct{}{}
	}
	return true
}

// nodeListing is one server's listing as this node derives it, beside the
// definitions its tools name.
type nodeListing struct {
	listing     coord.MCPListing
	definitions map[string]coord.MCPToolDefinition
}

// listedInstance is one instance whose listing a server's is the union of.
type listedInstance struct {
	server   string
	seat     uuid.UUID
	bridge   *mcp.Bridge
	instance string
}

// nodeMCPListings is this node's listing of every server it runs an instance
// of that has listed, by server: the union of its instances' tools, each
// counted by the instances listing it.
//
// INSTANCES IN ORDER, a shared one first and then by seat, so a node that
// re-derives the same facts writes the same bytes: the first instance to list
// a tool gives its definition, and the first to give a name gives the
// server's identity.
func (e *Engine) nodeMCPListings(node string, epoch int64, now time.Time) map[string]nodeListing {
	e.mcpMu.Lock()
	var instances []listedInstance
	for _, o := range e.mcpShared {
		if o.bridge != nil {
			instances = append(instances, listedInstance{o.server, o.seat, o.bridge, o.instance})
		}
	}
	for _, outcomes := range e.mcpSeats {
		for _, o := range outcomes {
			if o.bridge != nil {
				instances = append(instances, listedInstance{o.server, o.seat, o.bridge, o.instance})
			}
		}
	}
	e.mcpMu.Unlock()
	slices.SortFunc(instances, func(a, b listedInstance) int {
		return cmp.Or(cmp.Compare(a.server, b.server), cmp.Compare(a.seat.String(), b.seat.String()))
	})

	out := map[string]nodeListing{}
	for _, in := range instances {
		listed := in.bridge.ServerTools(in.instance)
		if listed == nil {
			// NEVER LISTED: a start the bridge is still retrying.
			continue
		}
		entry, ok := out[in.server]
		if !ok {
			entry = nodeListing{
				listing: coord.MCPListing{Server: in.server, Node: node, Epoch: epoch, At: now,
					Shared: in.seat == uuid.Nil},
				definitions: map[string]coord.MCPToolDefinition{},
			}
		}
		entry.listing.Instances++
		if entry.listing.Info == nil {
			if id, ok := in.bridge.Identity(in.instance); ok && id.Name != "" {
				entry.listing.Info = &coord.MCPServerInfo{Name: id.Name, Title: id.Title,
					Version: id.Version, Protocol: id.Protocol}
				entry.listing.Instructions = id.Instructions
			}
		}
		// A BLOCKED TOOL IS LISTED, flagged: the instance never filed it, and
		// the operator who blocked it reads it here to let it through again.
		definitions := make([]*mcp.Definition, 0, len(listed))
		for _, t := range listed {
			definitions = append(definitions, &t.Definition)
		}
		blocked := in.bridge.BlockedTools(in.instance)
		for i, d := range slices.Concat(definitions, blocked) {
			at := slices.IndexFunc(entry.listing.Tools, func(l coord.MCPListedTool) bool {
				return l.Name == d.Name()
			})
			if at >= 0 {
				entry.listing.Tools[at].Instances++
				continue
			}
			row, definition := listedTool(d, i >= len(definitions), now)
			entry.listing.Tools = append(entry.listing.Tools, row)
			if row.DefinitionKept {
				entry.definitions[row.Digest] = definition
			}
		}
		out[in.server] = entry
	}
	for server, entry := range out {
		waiting := func(tool, digest string) bool { return e.toolUnreviewed(server, tool, digest) }
		out[server] = entry.bounded(coord.MaxMCPListingDefinitionBytes, waiting)
	}
	return out
}

// bounded is the listing held to its bounds ([coord.MCPListing.Bounded]), every
// tool on it, and its definitions kept within budget bytes in all: first each
// tool whose definition waits for a review (waiting, nil for none), so the
// change a person is asked about is kept whole to show them, then the rest in
// name order while what remains of the budget holds them. A tool past it is
// listed with [coord.MCPListedTool.DefinitionKept] false and its definition
// not stored.
func (n nodeListing) bounded(budget int, waiting func(tool, digest string) bool) nodeListing {
	n.listing = n.listing.Bounded()
	kept := make(map[string]coord.MCPToolDefinition, len(n.listing.Tools))
	spent := 0
	for _, pass := range []bool{true, false} {
		for i := range n.listing.Tools {
			t := &n.listing.Tools[i]
			if !t.DefinitionKept || (waiting != nil && waiting(t.Raw, t.Digest)) != pass {
				continue
			}
			if _, counted := kept[t.Digest]; counted {
				continue
			}
			definition, ok := n.definitions[t.Digest]
			if !ok || spent+definition.Size() > budget {
				t.DefinitionKept = false
				continue
			}
			spent += definition.Size()
			kept[t.Digest] = definition
		}
	}
	n.definitions = kept
	return n
}

// listedTool is one tool as a listing names it, blocked or filed, and its
// definition as it is stored: kept only within its bounds
// ([coord.MCPToolDefinition.Fits]).
func listedTool(t *mcp.Definition, blocked bool, now time.Time) (coord.MCPListedTool, coord.MCPToolDefinition) {
	parts := t.Parts()
	row := coord.MCPListedTool{
		Name: t.Name(), Raw: t.RawName(), Title: t.Title(), Description: t.Description(),
		Hints: hintWords(t.Annotations()), Declared: hintWords(t.Declared()),
		Digest: t.Digest(), Instances: 1, Blocked: blocked,
		Parts: coord.MCPToolParts{Title: parts.Title, Description: parts.Description,
			Schema: parts.Schema, Hints: parts.Hints},
	}
	input, inErr := json.Marshal(t.Parameters())
	var output []byte
	var outErr error
	if t.OutputSchema() != nil {
		output, outErr = json.Marshal(t.OutputSchema())
	}
	definition := coord.MCPToolDefinition{Digest: row.Digest, Title: t.OwnTitle(),
		HintsTitle: t.Declared().Title, Description: t.Description(), Input: input, Output: output,
		At: now.UTC()}
	if inErr != nil || outErr != nil || !definition.Fits() {
		return row, coord.MCPToolDefinition{}
	}
	row.DefinitionKept = true
	return row, definition
}

// hintWords is a tool's four hints as a listing writes them.
func hintWords(h mcp.Annotations) coord.MCPToolHints {
	return coord.MCPToolHints{
		ReadOnly: h.ReadOnly.String(), Destructive: h.Destructive.String(),
		Idempotent: h.Idempotent.String(), OpenWorld: h.OpenWorld.String(),
	}
}

// listingFingerprint is a listing's content, for telling whether it moved: the
// whole record but when and under which epoch it was written, which every
// write restamps.
func listingFingerprint(l coord.MCPListing) string {
	l.At, l.Epoch, l.Version = time.Time{}, 0, 0
	raw, err := json.Marshal(l)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// listingsFingerprint names a set of listings by each one's key and version:
// what the catalogue rendered from them depends on nothing else.
func listingsFingerprint(listings []coord.MCPListing) string {
	h := sha256.New()
	for _, l := range listings {
		raw, _ := json.Marshal([]any{l.Server, l.Node, l.Epoch, l.Version})
		h.Write(raw)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// namedDigests is every digest the listings name for a stored definition, sorted.
func namedDigests(listings []coord.MCPListing) []string {
	return slices.Sorted(maps.Keys(digestSet(listings)))
}

// digestSet is every digest the listings name for a stored definition, as a set.
func digestSet(listings []coord.MCPListing) map[string]struct{} {
	out := map[string]struct{}{}
	for _, l := range listings {
		for _, t := range l.Tools {
			if t.DefinitionKept {
				out[t.Digest] = struct{}{}
			}
		}
	}
	return out
}

// nodeID is the id this node's presence and its listings are filed under.
func (e *Engine) nodeID() string {
	if e.node != nil {
		return e.node.ID()
	}
	return e.testNode
}

// mcpListingJobs is the maintenance sweep of the fleet's MCP listings: every
// listing no live node's current incarnation wrote, every definition no listing
// and no accepted definition has named for [mcpListingSweepPasses] sweeps, and
// every accepted definition of a server nothing declares or lists.
func (e *Engine) mcpListingJobs() []maintenance.Job {
	if e.backends == nil || e.backends.Fleet == nil || e.backends.Coord == nil {
		return nil
	}
	return []maintenance.Job{{
		Name: "mcp_listings", Scope: maintenance.Fleet,
		Run: func(ctx context.Context, _, _ time.Time) (int64, error) {
			return e.sweepMCPListings(ctx)
		},
	}}
}

// sweepMCPListings removes what readers no longer count, reporting how many
// records went.
//
// THE LISTINGS ARE READ BEFORE THE PRESENCE, and each is removed at the version
// read. A listing a node writes after its presence moved is then either one
// this pass did not read, or one the presence read after it covers; and a
// listing rewritten since it was read is not removed, since its version moved.
// Read the other way round, a node that claimed its presence again and wrote
// its listings between the two reads would have them removed, with nothing
// telling it to write them again.
func (e *Engine) sweepMCPListings(ctx context.Context) (int64, error) {
	fleet := e.backends.Fleet
	listings, err := fleet.ListMCPListings(ctx)
	if err != nil {
		return 0, err
	}
	presence, err := e.backends.Coord.ListLive(ctx, coord.ClassNode)
	if err != nil {
		return 0, err
	}
	live := coord.LiveMCPListings(listings, presence)
	counted := make(map[string]struct{}, len(live))
	for _, l := range live {
		counted[coord.MCPListingKey(l.Server, l.Node)] = struct{}{}
	}
	var removed int64
	for _, l := range listings {
		if _, ok := counted[coord.MCPListingKey(l.Server, l.Node)]; ok {
			continue
		}
		gone, delErr := fleet.DeleteMCPListing(ctx, l.Server, l.Node, l.Version)
		if delErr != nil {
			return removed, delErr
		}
		if gone {
			removed++
		}
	}

	// WHAT WAS ACCEPTED KEEPS ITS DEFINITION: a change is shown beside the
	// definition its level was decided on, which no listing names once the
	// server changed it (ADR-0078).
	accepted, err := fleet.ListMCPAccepted(ctx)
	if err != nil {
		return removed, err
	}
	gone, err := e.sweepMCPAccepted(ctx, accepted, live)
	removed += gone
	if err != nil {
		return removed, err
	}
	definitions, err := fleet.ListMCPToolDefinitions(ctx)
	if err != nil {
		return removed, err
	}
	named := digestSet(live)
	maps.Copy(named, acceptedDigests(accepted))
	l := &e.listings
	l.mu.Lock()
	before := l.unnamed
	l.mu.Unlock()
	unnamed := map[string]int{}
	for _, s := range definitions {
		if _, ok := named[s.Digest]; ok {
			continue
		}
		if passes := before[s.Digest] + 1; passes < mcpListingSweepPasses {
			unnamed[s.Digest] = passes
			continue
		}
		gone, err := fleet.DeleteMCPToolDefinition(ctx, s.Digest, s.Version)
		if err != nil {
			return removed, err
		}
		if gone {
			removed++
		}
	}
	l.mu.Lock()
	l.unnamed = unnamed
	l.mu.Unlock()
	return removed, nil
}
