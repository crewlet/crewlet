package engine

import (
	"context"
	"errors"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/mcpreview"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
)

// WHAT EACH MCP TOOL'S LEVEL WAS DECIDED ON, WATCHED; AND A SERVER'S FIRST
// LISTING, ACCEPTED (ADR-0078, internal/coord's mcpaccepted.go).
//
// A tool's level holds only for the definition it was decided on. This node
// keeps a watched copy of every accepted definition, which every call to an
// MCP tool it files is decided against ([Engine.toolUnreviewed]), which a box
// is given a server by ([Engine.boxServers]) and which the alarm counts by; and
// at each beat of its listings' publisher it accepts the first listing of each
// server it lists that nobody has accepted a listing of yet.
//
// # Unanswered is unreviewed
//
// Once this node watches the copy, and until the watch's first answer is in,
// it cannot tell a changed tool from an accepted one, and it answers every
// allowed tool as unreviewed: a call asks a person, and a box is given no
// server. A node reads the copy within a moment of starting, and a node that
// cannot read its coordination store is not running its seats' work anyway. A
// node that never watches (one with no coordination store) decides nothing.

// mcpReviews is this node's watched copy of the fleet's accepted definitions.
type mcpReviews struct {
	mu sync.RWMutex
	// records is every accepted definition by its key, and index the same
	// by server and tool, rebuilt with every change.
	records map[string]coord.MCPAcceptedTool
	index   mcpreview.Index
	// watching is whether this node watches the copy at all; answered
	// whether the watch's first answer is in; answering the keys the
	// current watch's first answer has delivered, nil once its marker has
	// come.
	watching  bool
	answered  bool
	answering map[string]struct{}
}

// watchReviews marks this node as one that watches the accepted definitions:
// from here on, a call decided before the watch's first answer is held
// ("unanswered is unreviewed").
func (e *Engine) watchReviews() {
	e.reviews.mu.Lock()
	e.reviews.watching = true
	e.reviews.mu.Unlock()
}

// watchMCPAccepted keeps the watched copy until ctx ends.
func (e *Engine) watchMCPAccepted(ctx context.Context) {
	e.watchReviews()
	// EVERY WATCH'S FIRST ANSWER REPLACES THE COPY, as the listings'
	// does: a record removed while a lost watch was opened again is in no
	// answer, and goes at the marker.
	watch := func(ctx context.Context) (<-chan coord.MCPAcceptedUpdate, error) {
		updates, err := e.backends.Fleet.WatchMCPAccepted(ctx)
		if err == nil {
			e.reviews.mu.Lock()
			e.reviews.answering = map[string]struct{}{}
			e.reviews.mu.Unlock()
		}
		return updates, err
	}
	marker := func(u coord.MCPAcceptedUpdate) bool {
		if u.Current {
			e.mcpAcceptedAnswered()
		}
		return u.Current
	}
	watchRecords(ctx, "mcp_accepted", watch, marker, e.applyMCPAccepted)
}

// applyMCPAccepted takes one change off the watch into the watched copy.
func (e *Engine) applyMCPAccepted(_ context.Context, u coord.MCPAcceptedUpdate) {
	r := &e.reviews
	key := coord.MCPAcceptedKey(u.Server, u.Tool)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.records == nil {
		r.records = map[string]coord.MCPAcceptedTool{}
	}
	if u.Accepted != nil {
		r.records[key] = *u.Accepted
	} else {
		delete(r.records, key)
	}
	if r.answering != nil {
		r.answering[key] = struct{}{}
	}
	r.index = mcpreview.IndexOf(slices.Collect(maps.Values(r.records)))
}

// mcpAcceptedAnswered ends a watch's first answer: every record it did not
// deliver was removed since the copy was last current.
func (e *Engine) mcpAcceptedAnswered() {
	r := &e.reviews
	r.mu.Lock()
	defer r.mu.Unlock()
	for key := range r.records {
		if _, delivered := r.answering[key]; !delivered {
			delete(r.records, key)
		}
	}
	r.answering, r.answered = nil, true
	r.index = mcpreview.IndexOf(slices.Collect(maps.Values(r.records)))
}

// toolUnreviewed is [mcpreview.Index.Unreviewed] by this node's copy: true for
// every tool until the copy's first answer is in (the file comment's
// "unanswered is unreviewed"). What every bridge this node runs decides a
// call by ([mcp.Supervision.Unreviewed]).
func (e *Engine) toolUnreviewed(server, tool, digest string) bool {
	r := &e.reviews
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !r.watching {
		return false
	}
	return !r.answered || r.index.Unreviewed(server, tool, digest)
}

// unreviewedDecider is what this node's bridges decide a call by: nil on a
// node with no coordination store, which has no fleet to agree with and holds
// no tool for a change.
func (e *Engine) unreviewedDecider() func(server, tool, digest string) bool {
	if e.backends == nil || e.backends.Fleet == nil {
		return nil
	}
	return e.toolUnreviewed
}

// errMCPAcceptedUnanswered is a read of the accepted definitions before this
// node's watch has answered.
var errMCPAcceptedUnanswered = errors.New("engine: this node has not yet read the fleet's accepted " +
	"MCP tool definitions; ask again in a moment")

// MCPAccepted is every accepted definition in this node's watched copy,
// ordered by server and tool. An error until the watch has given its first
// answer, which is not a fleet that has accepted nothing.
func (e *Engine) MCPAccepted(context.Context) ([]coord.MCPAcceptedTool, error) {
	r := &e.reviews
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !r.answered {
		return nil, errMCPAcceptedUnanswered
	}
	out := slices.Collect(maps.Values(r.records))
	coord.SortMCPAccepted(out)
	return out, nil
}

// reviewIndex is this node's copy by server and tool, and whether it has been
// answered.
func (e *Engine) reviewIndex() (mcpreview.Index, bool) {
	r := &e.reviews
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.index, r.answered
}

// reviewsWatched reports whether this node watches the accepted definitions.
func (e *Engine) reviewsWatched() bool {
	e.reviews.mu.RLock()
	defer e.reviews.mu.RUnlock()
	return e.reviews.watching
}

// baselineMCPTools accepts the first listing of every server in desired that
// no record names yet, as this node lists it (mcpreview's "a server's first
// listing is accepted as it is").
//
// READ FROM THE STORE BEFORE IT IS WRITTEN, not from the watched copy alone: a
// copy a moment behind a peer that baselined the server would take a tool the
// server added since for part of its first listing. Each record is created
// only where none is held, so two nodes listing a server first at once accept
// the union of their listings, and neither replaces the other's.
func (e *Engine) baselineMCPTools(ctx context.Context, desired map[string]nodeListing, now time.Time) {
	index, answered := e.reviewIndex()
	if !answered {
		return
	}
	var pending []string
	for _, server := range slices.Sorted(maps.Keys(desired)) {
		if len(desired[server].listing.Tools) > 0 && !index.Baselined(server) {
			pending = append(pending, server)
		}
	}
	if len(pending) == 0 {
		return
	}
	held, err := e.backends.Fleet.ListMCPAccepted(ctx)
	if err != nil {
		log.WarnContext(ctx, "mcp_tools_baseline_unread", "servers", pending, "error", err,
			"detail", "the first listing of each is accepted at the next beat")
		return
	}
	stored := mcpreview.IndexOf(held)
	for _, server := range pending {
		if stored.Baselined(server) {
			continue
		}
		// KEPT counts the records now in the store, written here or by a node
		// listing the server first at the same moment: one is enough to
		// baseline the server, so a write refused after it leaves the rest of
		// the listing to be read as added.
		written, kept := 0, 0
		var putErr error
		for _, a := range mcpreview.Baseline(desired[server].listing, now) {
			var wrote bool
			if _, wrote, putErr = e.backends.Fleet.PutMCPAccepted(ctx, a, 0); putErr != nil {
				break
			}
			kept++
			if wrote {
				written++
			}
		}
		switch {
		case putErr != nil && kept == 0:
			log.WarnContext(ctx, "mcp_tools_baseline_not_written", "server", server, "error", putErr,
				"detail", "nothing of its first listing is accepted yet; it is accepted at the next beat")
		case putErr != nil:
			log.WarnContext(ctx, "mcp_tools_baseline_not_written", "server", server, "tools", kept,
				"error", putErr, "detail", "its first listing is accepted in part: each tool of it left "+
					"out reads as added, and asks a person before each call until somebody accepts it")
		default:
			log.InfoContext(ctx, "mcp_tools_baselined", "server", server, "tools", written,
				"detail", "the server's first listing is accepted as it is: a tool it changes or "+
					"adds from now asks a person before each call until somebody accepts the change")
		}
	}
}

// acceptedDigests is every digest an accepted definition names: a definition
// the sweep keeps for as long as a change is shown beside it.
func acceptedDigests(accepted []coord.MCPAcceptedTool) map[string]struct{} {
	out := make(map[string]struct{}, len(accepted))
	for _, a := range accepted {
		if !a.Removed {
			out[a.Digest] = struct{}{}
		}
	}
	return out
}

// sweepMCPAccepted removes every accepted definition of a server the applied
// revision does not declare and no live listing names, reporting how many
// went: a server removed from the company takes what was accepted of it, so a
// server added later under its name lists for the first time.
//
// DECLARED OR LISTED KEEPS IT: a node on an older revision than the one that
// added a server would otherwise remove the server's baseline while its
// listings are live, and the next beat would accept whatever it lists then.
// Nothing is removed on a node with no applied revision to judge by.
func (e *Engine) sweepMCPAccepted(ctx context.Context, accepted []coord.MCPAcceptedTool,
	live []coord.MCPListing) (int64, error) {

	c := e.Company()
	if c == nil || c.Config == nil {
		return 0, nil
	}
	kept := map[string]struct{}{}
	for i := range c.Config.MCPServers {
		kept[c.Config.MCPServers[i].Name] = struct{}{}
	}
	for _, l := range live {
		kept[l.Server] = struct{}{}
	}
	var removed int64
	for _, a := range accepted {
		if _, ok := kept[a.Server]; ok {
			continue
		}
		gone, err := e.backends.Fleet.DeleteMCPAccepted(ctx, a.Server, a.Tool, a.Version)
		if err != nil {
			return removed, err
		}
		if gone {
			removed++
		}
	}
	return removed, nil
}

// changedConditions lays each server's tool changes over its condition
// (ADR-0078): how many changes hold a tool at Needs approval, how many of
// those claim less risk than before, the change seen first and when. By the
// live listings and this node's watched copy of what was accepted; a server
// whose listings or copy this node has not read, or that no live node lists, is
// marked unread ([statelog.MCPServerCondition.ChangesUnread]): it raises
// nothing, rather than every allowed tool at once, and clears nothing either.
//
// A REMOVAL OR A CHANGE THAT HOLDS NOTHING (a tool that already asks, or is
// blocked) is not counted: it waits on a person's review on the server's
// page, and nothing waits on it.
func (e *Engine) changedConditions(ctx context.Context, c *Company, conds []statelog.MCPServerCondition) {
	live, ok := e.LiveMCPListings()
	index, answered := e.reviewIndex()
	if !ok || !answered {
		for i := range conds {
			conds[i].ChangesUnread = true
		}
		return
	}
	at := make(map[string]int, len(conds))
	for i := range conds {
		at[conds[i].Server] = i
	}
	byServer := map[string][]coord.MCPListing{}
	for _, l := range live {
		byServer[l.Server] = append(byServer[l.Server], l)
	}
	var digests []string
	type pending struct {
		cond   *statelog.MCPServerCondition
		change mcpreview.Change
	}
	var holding []pending
	for i := range c.Config.MCPServers {
		entry := &c.Config.MCPServers[i]
		j, ok := at[entry.Name]
		if !ok {
			continue
		}
		if len(byServer[entry.Name]) == 0 {
			// NO LIVE NODE LISTS IT: a server stopped or not yet started, not
			// one whose changes were accepted.
			conds[j].ChangesUnread = true
			continue
		}
		for _, change := range mcpreview.Changes(byServer[entry.Name], index[entry.Name]) {
			if !mcpreview.Holds(entry.Tools, change) {
				continue
			}
			holding = append(holding, pending{&conds[j], change})
			if change.After.DefinitionKept {
				digests = append(digests, change.After.Digest)
			}
		}
	}
	seen := e.seenAt(ctx, digests)
	for _, p := range holding {
		cond := p.cond
		cond.Changes++
		if p.change.Loosened {
			cond.Loosened++
		}
		since := seen[p.change.After.Digest]
		if cond.Change.Tool == "" || datedBefore(since, cond.ChangeSince) {
			cond.ChangeSince = since
			cond.Change = statelog.MCPChangeNote{Tool: p.change.Tool, Kind: string(p.change.Kind),
				Loosened: p.change.Loosened}
		}
	}
}

// seenAt is when the fleet first stored each definition digest names, by its
// definition record's stamp: what a change's `seen_at` is. A digest whose
// definition is not stored is left out.
func (e *Engine) seenAt(ctx context.Context, digests []string) map[string]time.Time {
	out := make(map[string]time.Time, len(digests))
	if len(digests) == 0 {
		return out
	}
	definitions, err := e.MCPToolDefinitions(ctx, slices.Compact(slices.Sorted(slices.Values(digests))))
	if err != nil {
		log.WarnContext(ctx, "mcp_change_definitions_unread", "error", err,
			"detail", "the changes are dated by the definitions this node holds")
	}
	for digest, s := range definitions {
		out[digest] = s.At
	}
	return out
}

// boxHoldsChange reports whether a box may not be given server because a tool
// of it is listed under a definition nobody accepted, and which: a box reaches
// a server whole, so no call it makes could be held for a person. True while
// this node has not read the fleet's listings or what was accepted, since it
// cannot tell then.
func (e *Engine) boxHoldsChange(server string) (bool, string) {
	live, ok := e.LiveMCPListings()
	index, answered := e.reviewIndex()
	if !ok || !answered {
		return true, ""
	}
	for _, l := range live {
		if l.Server != server {
			continue
		}
		for _, t := range l.Tools {
			if !t.Blocked && index.Unreviewed(server, t.Raw, t.Digest) {
				return true, t.Raw
			}
		}
	}
	return false, ""
}

// withoutChangedServers takes out of a box's scoped servers every one a tool
// of which is held for a change ([Engine.boxHoldsChange]), saying so: the seat
// reaches the server's tools over the bridge instead, where each call is
// decided.
func (e *Engine) withoutChangedServers(ctx context.Context, seat string,
	servers map[string]sandbox.MCPServer) map[string]sandbox.MCPServer {

	if !e.reviewsWatched() {
		return servers
	}
	for _, name := range slices.Sorted(maps.Keys(servers)) {
		held, tool := e.boxHoldsChange(name)
		if !held {
			continue
		}
		delete(servers, name)
		detail := "the executor is not given this server in its box: this node has not yet read " +
			"which of its tools' definitions were accepted, and a box reaches a server whole"
		if tool != "" {
			detail = "the executor is not given this server in its box: " + tool + " changed since " +
				"its level was decided (or was added), and asks a person before each call until " +
				"somebody accepts the change, which a box reaching the server whole could not; the " +
				"seat reaches its tools over the bridge"
		}
		log.WarnContext(ctx, "sandbox_mcp_server_changed", "seat", seat, "server", name,
			"tool", tool, "detail", detail)
	}
	if len(servers) == 0 {
		return nil
	}
	return servers
}
