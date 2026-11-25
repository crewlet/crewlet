package engine

import (
	"context"
	"database/sql"
	"maps"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/store"
)

// mcpInstanceKey is one MCP instance this node holds: a shared server's
// (seat uuid.Nil) or a seat's child.
type mcpInstanceKey struct {
	server string
	seat   uuid.UUID
}

// mcpDown is when each MCP instance this node holds stopped serving, kept in
// the node's own file (`mcp_instance_down`) so an outage keeps its start across
// a restart, where [stampSince] and a bridge's own record start empty.
type mcpDown struct {
	mu     sync.Mutex
	loaded bool
	// since is each not-serving instance's start; kept is what the file
	// holds; read is the instances the latest reading saw.
	since map[mcpInstanceKey]time.Time
	kept  map[mcpInstanceKey]time.Time
	read  map[mcpInstanceKey]bool
}

// recallMCPDown reads the file's outages once, before the first reading
// dates an instance.
func (e *Engine) recallMCPDown(ctx context.Context) {
	d := &e.mcpDown
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.loaded || e.backends == nil || e.backends.Store == nil {
		return
	}
	kept := map[mcpInstanceKey]time.Time{}
	err := e.backends.Store.Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT server, seat, since FROM mcp_instance_down`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var server, seat string
			var since int64
			if err := rows.Scan(&server, &seat, &since); err != nil {
				return err
			}
			key := mcpInstanceKey{server: server}
			if seat != "" {
				id, err := uuid.Parse(seat)
				if err != nil {
					continue
				}
				key.seat = id
			}
			kept[key] = store.DecodeTime(since)
		}
		return rows.Err()
	})
	if err != nil {
		log.WarnContext(ctx, "mcp_outages_unread", "error", err.Error(),
			"detail", "an instance down since before this start is dated from its first failure now")
		return
	}
	d.loaded, d.kept, d.since = true, kept, maps.Clone(kept)
}

// date is the start of the outage of the instance at key: the earlier of since
// (this process's) and the one kept, unless the instance answered after the
// kept one began. A serving instance's is forgotten.
func (d *mcpDown) date(key mcpInstanceKey, serving bool, since, answered time.Time) time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.read == nil {
		d.read = map[mcpInstanceKey]bool{}
	}
	d.read[key] = true
	if serving {
		delete(d.since, key)
		return since
	}
	if kept, ok := d.since[key]; ok && !answered.After(kept) && (since.IsZero() || kept.Before(since)) {
		return kept
	}
	if !since.IsZero() {
		if d.since == nil {
			d.since = map[mcpInstanceKey]time.Time{}
		}
		d.since[key] = since
	}
	return since
}

// began starts a reading: the instances it reads replace the last one's.
func (d *mcpDown) began() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.read = map[mcpInstanceKey]bool{}
}

// keepOnly forgets seat's instances (uuid.Nil: the shared servers') whose
// server a pass no longer starts on this node. The caller recalled the file.
func (d *mcpDown) keepOnly(seat uuid.UUID, outcomes []mcpOutcome) {
	started := map[string]bool{}
	for _, o := range outcomes {
		started[o.server] = true
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for key := range d.since {
		if key.seat == seat && !started[key.server] {
			delete(d.since, key)
		}
	}
}

// keepMCPDown writes what changed since the last keep to the file, first
// forgetting a seat's instance no reading saw whose seat another node holds:
// the seat moved, and its outage here ended with it.
func (e *Engine) keepMCPDown(ctx context.Context) {
	if e.backends == nil || e.backends.Store == nil {
		return
	}
	d := &e.mcpDown
	d.mu.Lock()
	var unread []mcpInstanceKey
	for key := range d.since {
		if key.seat != uuid.Nil && !d.read[key] {
			unread = append(unread, key)
		}
	}
	d.mu.Unlock()
	if e.backends.Coord != nil {
		for _, key := range unread {
			holder, err := coord.Holder(ctx, e.backends.Coord, coord.SeatResource(key.seat))
			if err == nil && holder != "" && holder != e.selfNode() {
				d.mu.Lock()
				delete(d.since, key)
				d.mu.Unlock()
			}
		}
	}
	d.mu.Lock()
	since := maps.Clone(d.since)
	kept := d.kept
	d.mu.Unlock()
	if maps.Equal(since, kept) {
		return
	}
	err := e.backends.Store.Tx(ctx, func(tx *sql.Tx) error {
		for key := range kept {
			if _, ok := since[key]; ok {
				continue
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM mcp_instance_down WHERE server = ? AND seat = ?`,
				key.server, seatColumn(key.seat)); err != nil {
				return err
			}
		}
		for key, at := range since {
			if was, ok := kept[key]; ok && was.Equal(at) {
				continue
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO mcp_instance_down (server, seat, since)
				VALUES (?, ?, ?) ON CONFLICT (server, seat) DO UPDATE SET since = excluded.since`,
				key.server, seatColumn(key.seat), store.EncodeTime(at)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		log.WarnContext(ctx, "mcp_outages_unkept", "error", err.Error(),
			"detail", "a restart before the next heartbeat dates these outages from their next failure")
		return
	}
	d.mu.Lock()
	d.kept = since
	d.mu.Unlock()
}

// seatColumn is a seat's id as the file keys it, "" for a shared server's.
func seatColumn(seat uuid.UUID) string {
	if seat == uuid.Nil {
		return ""
	}
	return seat.String()
}
