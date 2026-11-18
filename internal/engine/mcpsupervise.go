package engine

import (
	"context"

	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/tools"
)

// What the engine decides about the MCP bridges' supervision, beyond the
// bridge restarting a dead server itself (internal/mcp): who may start a
// server on this node, and what a changed catalogue means for its registries.
//
// For a held seat it never contends with the seat's runner (seatservers.go).
// A FIRST start that failed is the runner's: its bridge keeps nothing of it.
// A child that DIED is its bridge's ([mcp.Bridge.Runs] answers yes): a pass
// leaves it, and an apply's pass only asks [mcp.Bridge.RetryNow]. Both are
// gated on the runner's predicate, [seat.Host.Provable]; a release stops the
// bridge. A restart that changed the catalogue refiles only the seat's surface
// (refileSeat): a runner pass would reconcile every child for nothing.

// sharedSupervision has the bridge retry a failed start, since otherwise only
// the next apply would ask again, and on a company nobody edits that is never.
// Nothing gates its starts: a shared server carries no seat's identity.
func (e *Engine) sharedSupervision() mcp.Supervision {
	return mcp.Supervision{
		RetryFailedStarts: true,
		CatalogueChanged:  e.sharedCatalogueChanged,
		RetryBase:         e.mcpRetryBase,
		Unreviewed:        e.unreviewedDecider(),
		RelistBase:        e.mcpRelistBase,
	}
}

// seatSupervision is one held seat's bridge's (the file comment). held is read
// at every call, never captured, so the supervisor asks the runner's predicate.
func (e *Engine) seatSupervision(s *seatChildren) mcp.Supervision {
	return mcp.Supervision{
		MayStart:         func() bool { return s.held() },
		CatalogueChanged: func(ctx context.Context) { e.refileSeat(ctx, s) },
		RetryBase:        e.mcpRetryBase,
		Unreviewed:       e.unreviewedDecider(),
		RelistBase:       e.mcpRelistBase,
	}
}

// sharedCatalogueChanged refiles what the shared bridge's supervision changed
// into the current epoch's registry and every held seat's surface, re-audits
// the tool skills (the catalogue is an input), and tells
// [Engine.SetOnCatalogueChanged], which nothing else would until the next
// published company.
//
// Against the epoch current NOW: an apply building the next one refiles it
// from the bridge when it publishes it ([Engine.installEpoch]), under the same
// lock, so the change reaches the successor whichever takes the lock first.
func (e *Engine) sharedCatalogueChanged(ctx context.Context) {
	e.mcpFileMu.Lock()
	c := e.Company()
	if c == nil || c.Tools == nil {
		e.mcpFileMu.Unlock()
		return
	}
	e.fileSharedTools(ctx, c, true)
	e.mcpFileMu.Unlock()
	e.refileSeatSurfaces(ctx)
	e.auditSkills()
	e.notifyCatalogueChanged(ctx)
}

// refileSeatSurfaces rebuilds every held seat's surface against the current
// epoch, as the apply's seat_tools stage does: each is a clone of the epoch's
// registry, so a change to it reaches a seat only through a new clone.
func (e *Engine) refileSeatSurfaces(ctx context.Context) {
	e.mcpMu.Lock()
	defer e.mcpMu.Unlock()
	c := e.Company()
	if c == nil || c.Tools == nil || len(e.seatMCP) == 0 {
		return
	}
	if e.seatTools == nil {
		e.seatTools = make(map[string]*tools.Registry, len(e.seatMCP))
	}
	for handle, s := range e.seatMCP {
		e.seatTools[handle] = seatSurface(ctx, c, s)
	}
}

// refileSeat rebuilds one seat's surface after its bridge's supervision changed
// the catalogue; nothing once the seat was released or reclaimed since, as the
// surface is no longer this bridge's to file.
func (e *Engine) refileSeat(ctx context.Context, s *seatChildren) {
	e.mcpMu.Lock()
	defer e.mcpMu.Unlock()
	if e.seatMCP[s.handle] != s {
		return
	}
	c := e.Company()
	if c == nil || c.Tools == nil {
		return
	}
	if e.seatTools == nil {
		e.seatTools = make(map[string]*tools.Registry)
	}
	e.seatTools[s.handle] = seatSurface(ctx, c, s)
}

// fileSharedTools makes an epoch's registry hold exactly the shared bridge's
// live catalogue. Only MCP tools are touched: a server tool that collided with
// a builtin was refused, and unregistering by name would take the builtin.
// report logs such a refusal; a publish passes false, its apply having logged
// it, so an operator reads each collision once per apply.
//
// The caller holds mcpFileMu.
func (e *Engine) fileSharedTools(ctx context.Context, c *Company, report bool) {
	if e.mcp == nil || c == nil || c.Tools == nil {
		return
	}
	// One read of the bridge, so removals and filings are of one catalogue.
	registrations := e.mcp.Registrations()
	live := make(map[string]mcp.Callable, len(registrations))
	for _, registration := range registrations {
		live[registration.Tool.Name()] = registration.Tool
	}
	for _, name := range c.Tools.MCPNames() {
		if entry, ok := c.Tools.Lookup(name); ok && live[name] != entry.Tool {
			c.Tools.Unregister(name)
		}
	}
	for _, registration := range registrations {
		name := registration.Tool.Name()
		if entry, ok := c.Tools.Lookup(name); ok {
			if entry.Tool != registration.Tool && report {
				log.WarnContext(ctx, "mcp_tool_refused", "tool", name,
					"detail", "a tool the engine already offers holds the name")
			}
			continue
		}
		if err := c.Tools.RegisterMCP(registration); err != nil && report {
			log.WarnContext(ctx, "mcp_tool_refused", "tool", name, "error", err)
		}
	}
}

// mcpHealth is a started server's condition by its bridge's own record; false
// when no bridge supervises it.
func mcpHealth(o mcpOutcome) (mcp.Health, bool) {
	if o.bridge == nil {
		return mcp.Health{}, false
	}
	return o.bridge.Health(o.instance)
}
