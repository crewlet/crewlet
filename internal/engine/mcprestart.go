package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/mcp"
)

// "RESTART" ON AN MCP SERVER'S PAGE, FLEET-WIDE.
//
// A server's instances are each node's own children: the shared servers' one
// child on every node, and each held seat's own on the node that holds it. So a
// restart a person asks for through one node is a coordination record
// ([coord.MCPRestart]), one per server and one per seat a person restarted one
// instance of, and every node keeps a watched copy of the class, as it does of
// the model retries (modelretry.go). On a record stamped later than the last
// one it applied for that entry, a node restarts every instance it runs whose
// state began BEFORE the stamp ([mcp.Bridge.RestartBefore]):
//
//   - a serving instance's session is ended, and its bridge's supervisor
//     starts a fresh one at once;
//   - an instance that is down is tried now, its restart schedule started
//     again, rather than at the schedule's next attempt (up to five minutes
//     away);
//   - a seat's child whose first start failed, which no bridge keeps (the
//     seat's runner retries it), gets a pass now that starts a new series.
//
// A record that names a seat is acted on by the node holding the seat alone:
// no other node runs that seat's child, and a node that cannot prove it holds
// the seat starts nothing for it (the supervision's own rule, mcpsupervise.go).
//
// Order-free and idempotent: a node that hears a record late, twice, or after a
// restart restarts exactly what it would have restarted on time, and nothing
// that started since. The record goes with its server: the apply that removes
// the server deletes it ([Engine.clearRemovedMCPRestarts]).

// The refusals of a restart request, each naming what the person can change.
var (
	// ErrNoMCPServer is a server the active configuration does not declare.
	ErrNoMCPServer = errors.New("engine: no such MCP server in the active configuration")
	// ErrMCPServerOff is a server the configuration turns off: nothing runs
	// it, so there is nothing to restart until it is turned back on.
	ErrMCPServerOff = errors.New("engine: the MCP server is turned off, so nothing runs it")
	// ErrMCPSeatOnShared is a seat named for a shared server, which runs one
	// instance for the company rather than one per seat.
	ErrMCPSeatOnShared = errors.New("engine: a shared MCP server runs one instance for the company, " +
		"not one per seat: restart it without naming a seat")
	// ErrNoAgentSeat is a seat the active organization does not have.
	ErrNoAgentSeat = errors.New("engine: no such agent seat")
	// ErrMCPSeatNotGranted is a seat the server is not granted to, which has
	// no instance of it to restart.
	ErrMCPSeatNotGranted = errors.New("engine: the seat is not granted the MCP server, so it has no instance of it")
)

// MCPRestartResult is what a restart request recorded.
type MCPRestartResult struct {
	// Server is the server, and Seat the seat's handle for one seat's
	// instance ("" for every instance).
	Server string
	Seat   string
	// At is the request's stamp: every node restarts what began before it,
	// so an instance whose state began after it has restarted.
	At time.Time
	// Node is the node that took the request.
	Node string
}

// mcpRestarts is this node's watched copy of the restart records: the last
// stamp it applied per entry, and the watch's lifetime.
type mcpRestarts struct {
	mu      sync.Mutex
	applied map[string]time.Time

	cancel context.CancelFunc
	done   chan struct{}
}

// startMCPRestarts opens the restart watch and keeps it open until
// stopMCPRestarts.
func (e *Engine) startMCPRestarts(ctx context.Context) {
	if e.backends == nil || e.backends.Fleet == nil {
		return
	}
	loopCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	e.restarts.cancel, e.restarts.done = cancel, make(chan struct{})
	go func() {
		defer close(e.restarts.done)
		watchRecords(loopCtx, "mcp_restart", e.backends.Fleet.WatchMCPRestarts,
			func(u coord.MCPRestartUpdate) bool { return u.Current },
			func(ctx context.Context, u coord.MCPRestartUpdate) {
				if u.Restart != nil {
					e.applyMCPRestart(ctx, *u.Restart)
					return
				}
				e.restarts.mu.Lock()
				delete(e.restarts.applied, u.Entry)
				e.restarts.mu.Unlock()
			})
	}()
}

// stopMCPRestarts ends the watch and waits for it.
func (e *Engine) stopMCPRestarts() {
	if e.restarts.cancel == nil {
		return
	}
	e.restarts.cancel()
	<-e.restarts.done
}

// applyMCPRestart restarts what a restart record asks this node to restart,
// once per stamp.
func (e *Engine) applyMCPRestart(ctx context.Context, r coord.MCPRestart) {
	entry := r.Entry()
	e.restarts.mu.Lock()
	if last, seen := e.restarts.applied[entry]; seen && !r.At.After(last) {
		e.restarts.mu.Unlock()
		return
	}
	if e.restarts.applied == nil {
		e.restarts.applied = map[string]time.Time{}
	}
	e.restarts.applied[entry] = r.At
	e.restarts.mu.Unlock()

	cause := "restarted at the request of " + r.By
	var done []mcp.RestartOutcome
	if r.Seat == uuid.Nil {
		if e.mcp != nil {
			done = append(done, e.mcp.RestartBefore(r.Server, r.At, cause))
		}
		for _, s := range e.heldSeatChildren() {
			done = append(done, e.restartSeatChild(s, r, cause))
		}
	} else {
		// THE SEAT'S HOLDER ALONE: every node hears the record, and only the
		// one that runs the seat's children, and can prove it holds the
		// seat, acts on it.
		s := e.seatChildrenOf(r.Seat)
		if s == nil || !s.held() {
			return
		}
		done = append(done, e.restartSeatChild(s, r, cause))
	}
	restarted := slices.ContainsFunc(done, func(o mcp.RestartOutcome) bool {
		return o == mcp.RestartedNow || o == mcp.RetriedNow
	})
	if !restarted {
		return
	}
	attrs := []any{"server", r.Server, "at", r.At.UTC().Format(time.RFC3339), "by", r.By,
		"by_kind", string(r.ByKind), "restarted", count(done, mcp.RestartedNow),
		"retried", count(done, mcp.RetriedNow),
		"detail", "a person asked for this server to be restarted: every instance this node " +
			"runs whose state began before the request was restarted, or tried now if it was down"}
	if r.Seat != uuid.Nil {
		attrs = append(attrs, "seat_id", r.Seat.String())
	}
	log.InfoContext(ctx, "mcp_restart_applied", attrs...)
}

// count is how many of outcomes are o.
func count(outcomes []mcp.RestartOutcome, o mcp.RestartOutcome) int {
	n := 0
	for _, got := range outcomes {
		if got == o {
			n++
		}
	}
	return n
}

// heldSeatChildren is every seat this node holds children for, now.
func (e *Engine) heldSeatChildren() []*seatChildren {
	e.mcpMu.Lock()
	defer e.mcpMu.Unlock()
	out := make([]*seatChildren, 0, len(e.seatMCP))
	for _, s := range e.seatMCP {
		out = append(out, s)
	}
	return out
}

// seatChildrenOf is the children this node holds for the seat whose agent id
// is seat, nil where it holds none: a seat the active organization does not
// have, or one held elsewhere.
func (e *Engine) seatChildrenOf(seat uuid.UUID) *seatChildren {
	c := e.Company()
	if c == nil || c.Org == nil {
		return nil
	}
	role := c.Org.AgentSeatByID(seat)
	if role == nil {
		return nil
	}
	e.mcpMu.Lock()
	defer e.mcpMu.Unlock()
	return e.seatMCP[role.Handle()]
}

// restartSeatChild restarts one seat's child of the server r names, if its
// state began before r's stamp: on the seat's bridge where the bridge keeps
// it, and otherwise (a first start that failed, which the seat's runner
// retries) with a pass now that starts a new retry series.
func (e *Engine) restartSeatChild(s *seatChildren, r coord.MCPRestart, cause string) mcp.RestartOutcome {
	for _, instance := range s.bridge.Servers() {
		if mcp.ServerName(instance) == r.Server {
			return s.bridge.RestartBefore(instance, r.At, cause)
		}
	}
	e.mcpMu.Lock()
	failedBefore := slices.ContainsFunc(e.mcpSeats[s.handle], func(o mcpOutcome) bool {
		return o.server == r.Server && o.err != nil && o.since.Before(r.At)
	})
	e.mcpMu.Unlock()
	if !failedBefore {
		return mcp.RestartAbsent
	}
	// ON THE SEAT'S OWN GOROUTINE, as its retry runs: a pass waits out its
	// children's starts, and the watch delivering this record must not wait
	// with it. The lease's context ends it with the seat.
	go e.convergeSeat(s.lease, s, true)
	return mcp.RetriedNow
}

// RestartMCPServer asks every node to restart the server name now: every
// instance of it, or with seat (a handle) that seat's instance on the node
// holding the seat. It writes the request and returns; each node restarts what
// it runs as it hears it ([Engine.applyMCPRestart]), this one included.
//
// [ErrNoMCPServer] for a server the active configuration does not declare,
// [ErrMCPServerOff] for one it turns off, and with a seat [ErrMCPSeatOnShared],
// [ErrNoAgentSeat] or [ErrMCPSeatNotGranted]. Any other error is a store that
// could not be written.
func (e *Engine) RestartMCPServer(ctx context.Context, name, seat string, by iam.Actor) (MCPRestartResult, error) {
	c := e.Company()
	if c == nil || c.Config == nil {
		return MCPRestartResult{}, fmt.Errorf("%w: %q", ErrNoMCPServer, name)
	}
	i := slices.IndexFunc(c.Config.MCPServers, func(s config.MCPServer) bool { return s.Name == name })
	if i < 0 {
		return MCPRestartResult{}, fmt.Errorf("%w: %q", ErrNoMCPServer, name)
	}
	server := &c.Config.MCPServers[i]
	if !server.IsEnabled() {
		return MCPRestartResult{}, fmt.Errorf("%w: %q", ErrMCPServerOff, name)
	}
	target := uuid.Nil
	if seat = strings.TrimSpace(seat); seat != "" {
		if server.IsShared() {
			return MCPRestartResult{}, fmt.Errorf("%w: %q", ErrMCPSeatOnShared, name)
		}
		role := c.Org.AgentSeatByHandle(seat)
		if role == nil {
			return MCPRestartResult{}, fmt.Errorf("%w: %q", ErrNoAgentSeat, seat)
		}
		if !server.Grants(role) {
			return MCPRestartResult{}, fmt.Errorf("%w: %q is not granted %q", ErrMCPSeatNotGranted, seat, name)
		}
		id, ok := c.Org.AgentIDFor(role)
		if !ok {
			return MCPRestartResult{}, fmt.Errorf("%w: %q", ErrNoAgentSeat, seat)
		}
		target = id
	}
	if e.backends == nil || e.backends.Fleet == nil {
		return MCPRestartResult{}, errors.New("engine: this node has no coordination store to tell the fleet")
	}
	stored, err := e.backends.Fleet.PutMCPRestart(ctx, coord.MCPRestart{
		Server: name, Seat: target, At: e.now(), By: by.Name, ByKind: by.Kind, OperatorID: by.OperatorID,
	})
	if err != nil {
		return MCPRestartResult{}, err
	}
	return MCPRestartResult{Server: name, Seat: seat, At: stored.At, Node: e.selfNode()}, nil
}

// clearRemovedMCPRestarts deletes the restart of every server the next
// revision removed, and every seat's restart whose seat it no longer has: a
// restart goes with what it restarted.
func (e *Engine) clearRemovedMCPRestarts(ctx context.Context, next *Company) {
	if next == nil || next.Config == nil || e.backends == nil || e.backends.Fleet == nil {
		return
	}
	if _, err := e.fleetCompany(ctx, next); err != nil {
		log.DebugContext(ctx, "mcp_restarts_not_swept", "reason", err.Error(),
			"detail", "a removed server's restart is cleared by the company the fleet is pointed "+
				"at; this one is not it yet")
		return
	}
	restarts, err := e.backends.Fleet.ListMCPRestarts(ctx)
	if err != nil {
		log.WarnContext(ctx, "mcp_restarts_not_swept", "error", err,
			"detail", "a removed server's restart record stays until the next apply clears it")
		return
	}
	declared := make(map[string]struct{}, len(next.Config.MCPServers))
	for i := range next.Config.MCPServers {
		declared[next.Config.MCPServers[i].Name] = struct{}{}
	}
	for _, r := range restarts {
		_, still := declared[r.Server]
		if still && (r.Seat == uuid.Nil || (next.Org != nil && next.Org.AgentSeatByID(r.Seat) != nil)) {
			continue
		}
		if _, err := e.backends.Fleet.DeleteMCPRestart(ctx, r.Entry(), r.Version); err != nil {
			log.WarnContext(ctx, "mcp_restart_not_cleared", "server", r.Server, "error", err)
		}
	}
}
