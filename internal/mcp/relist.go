package mcp

import (
	"context"
	"time"
)

// LISTING A RUNNING SERVER AGAIN (ADR-0078).
//
// A server lists its tools when it starts, and is free to change them while
// it runs: a new release behind the same address, a tool's description
// reworded, a hint dropped. The engine sees what a server lists only when it
// asks, so a server it never asked again could change a tool under a level
// decided on the old definition, and nothing would hold the tool for a person
// to look at. Each session is therefore listed again on a schedule the server
// states, bounded both ways, and whenever the server says its list changed.
//
// # On the server's own schedule, bounded both ways
//
// A `tools/list` result may say how long it stays fresh (`ttlMs`, the
// specification's cache hint). The engine lists the session again when that
// runs out, held between [RelistFloor] and [RelistCeiling], and at the ceiling
// when the server states none. The bounds are the engine's, not the server's,
// because the server is the party whose changes are being watched for.
//
// # A notification is a request, not an order
//
// A server that says its tool list changed (`notifications/tools/list_changed`)
// is listed again, but never sooner than [RelistFloor] after the session was
// last listed: the notification moves the next listing earlier, and a server
// sending a thousand of them gets the one listing a minute it could have had
// by stating a short `ttlMs`.
//
// # Never from the SDK's cache
//
// The SDK keeps a listing for the `ttlMs` the server stated, and would answer
// a listing the engine asks for inside it from that copy, without asking the
// server: a server stating a day would be listed again from a copy it sent a
// day before. The probe reads the hint off the wire and hands the SDK a
// listing that is never fresh (probe.go), so every listing the engine asks
// for reaches the server.

const (
	// RelistFloor is the soonest a running session is listed again after
	// its last listing, however short a `ttlMs` its server states and
	// however many list-changed notifications it sends. A JUDGEMENT CALL
	// (the series plan's): a minute keeps a server from making the engine
	// list it constantly (a per-agent server's every seat's child lists on
	// the same schedule, and a listing that moves is a write to the fleet's
	// coordination store, so a server setting `ttlMs` to zero would set the
	// fleet's write rate), while a change the server announces is still
	// seen within a minute of the announcement.
	RelistFloor = time.Minute

	// RelistCeiling is the longest a running session goes without being
	// listed again, however long a `ttlMs` its server states, and how often
	// a server stating none is listed. A JUDGEMENT CALL (the series
	// plan's): a quarter of an hour bounds how long a server can serve a
	// changed tool at the level decided on its old definition before the
	// tool is held for a person, so no `ttlMs` opts a server out of being
	// watched, while a server that states nothing (every server before the
	// 2026-07-28 revision of the specification) costs one listing per
	// session per quarter of an hour.
	RelistCeiling = 15 * time.Minute
)

// relistRatio is [RelistCeiling] over [RelistFloor], which a test's
// [Supervision.RelistBase] scales the schedule by.
const relistRatio = RelistCeiling / RelistFloor

// relistFloor is the floor the bridge's schedule runs at: [RelistFloor], or a
// test's [Supervision.RelistBase].
func (s Supervision) relistFloor() time.Duration {
	if s.RelistBase > 0 {
		return s.RelistBase
	}
	return RelistFloor
}

// RelistAfter is how long after a listing its session is listed again: the
// `ttlMs` its server stated, held between floor and the ceiling [RelistCeiling]
// is to [RelistFloor], and that ceiling where it stated none.
func RelistAfter(ttl time.Duration, stated bool, floor time.Duration) time.Duration {
	ceiling := floor * relistRatio
	if !stated {
		return ceiling
	}
	return min(max(ttl, floor), ceiling)
}

// serve waits out a live session, listing it again on its schedule
// (relist.go's file comment) until it ends; false when the supervision ended
// first, which is a stop.
func (s *server) serve(ctx context.Context, c *client) bool {
	floor := s.b.sup.relistFloor()
	listed := time.Now()
	due := listed.Add(c.relistAfter(floor))
	for {
		timer := time.NewTimer(time.Until(due))
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-c.ended():
			timer.Stop()
			// A stop and a death at the same instant is a stop: the
			// owner's shutdown clears the client away itself.
			return ctx.Err() == nil
		case <-c.listChanged:
			timer.Stop()
			// SOONER, NEVER BEFORE THE FLOOR: a burst of notifications
			// moves the next listing to the floor once, and no further.
			soonest := listed.Add(floor)
			if now := time.Now(); now.After(soonest) {
				soonest = now
			}
			if soonest.Before(due) {
				due = soonest
			}
			continue
		case <-timer.C:
		}
		listed = time.Now()
		if s.relist(ctx, c) {
			due = listed.Add(c.relistAfter(floor))
		} else {
			due = listed.Add(floor)
		}
	}
}

// relist lists a live session's tools again and files what it lists,
// reporting whether the listing was read. A listing that changed the
// catalogue is told to the owner ([Supervision.CatalogueChanged]), as a
// restart that lists differently is.
//
// A LISTING THAT FAILS CHANGES NOTHING: the tools the session listed last stay
// filed, and it is asked again a floor later. A session that ended under it
// is the supervisor's to notice, which it does at its next wait.
func (s *server) relist(ctx context.Context, c *client) bool {
	defs, err := c.listTools(ctx)
	if err != nil {
		if ctx.Err() == nil && !c.gone() {
			s.b.log.Warn("server_relist_failed", "server", s.spec.Name, "error", err.Error(),
				"retry_in_seconds", s.b.sup.relistFloor().Seconds(),
				"detail", "the tools it listed last stay filed; it is listed again a minute later")
		}
		return false
	}
	change, installed := s.b.relisted(s, c, defs)
	if !installed {
		return true
	}
	s.noteAnswer()
	if len(change.Added) > 0 || len(change.Removed) > 0 {
		s.b.log.Info("server_catalogue_changed", "server", s.spec.Name, "cause", "relisted",
			"added", toolNames(change.Added), "removed", change.Removed)
		if changed := s.b.sup.CatalogueChanged; changed != nil {
			changed(ctx)
		}
	}
	return true
}
