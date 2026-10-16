package engine

import (
	"context"

	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/tools"
)

// What the engine decides about the MCP bridges' supervision.
//
// A bridge keeps its own servers alive — it notices a session that ended and
// restarts the server on a schedule (internal/mcp, supervise.go). Two things
// it cannot decide alone are decided here: WHO MAY START a server on this
// node, which for a per-role child is a question about the seat's lease; and
// WHAT A CHANGED CATALOGUE MEANS, which is a question about the registries
// this package builds from it.
//
// # Who restarts what, for a seat
//
// A held seat's children already have an owner that starts them: the seat's
// runner (seatservers.go), whose passes are single-flight and start nothing
// for a seat this node cannot prove it holds. The supervision is drawn so that
// it never contends with that runner for one child:
//
//   - A child whose FIRST start failed is the runner's. Its bridge keeps
//     nothing of it ([mcp.Supervision.RetryFailedStarts] is off), so the
//     runner's own retry is the only loop over it.
//   - A child that DIED after starting is its bridge's. The bridge keeps it,
//     down, and [mcp.Bridge.Runs] answers yes for it, so a pass leaves it to
//     the supervisor rather than starting a second copy beside it; an apply's
//     pass only asks it to try now ([mcp.Bridge.RetryNow]), as an apply asks
//     a shared server.
//   - Both are gated on the SAME predicate, the runner's own: the supervisor
//     restarts a child only while the seat host can prove this node holds the
//     seat ([seat.Host.Provable]), and a release stops the bridge and its
//     supervision with it.
//
// And a restart that changed the seat's catalogue refiles the seat's surface
// itself rather than asking the runner for a pass: a pass is a reconcile of
// every child the seat declares, and the change needs none — only the surface
// filed from the bridge.

// sharedSupervision is the company-wide bridge's: a start that fails is
// RETRIED by the bridge, because nothing else would ask again — the next
// apply is the only other caller, and on a company nobody is editing that
// is never. Nothing gates its starts: a shared server carries no seat's
// identity, so any node running the company may run it.
func (e *Engine) sharedSupervision() mcp.Supervision {
	return mcp.Supervision{
		RetryFailedStarts: true,
		CatalogueChanged:  e.sharedCatalogueChanged,
		RetryBase:         e.mcpRetryBase,
	}
}

// seatSupervision is one held seat's bridge's — see the file comment.
//
// held is read at every call rather than captured, so the predicate the
// supervisor asks is always the one the seat's runner asks.
func (e *Engine) seatSupervision(s *seatChildren) mcp.Supervision {
	return mcp.Supervision{
		MayStart:         func() bool { return s.held() },
		CatalogueChanged: func(ctx context.Context) { e.refileSeat(ctx, s) },
		RetryBase:        e.mcpRetryBase,
	}
}

// sharedCatalogueChanged refiles what the shared bridge's own supervision
// changed — a retried start that came up, a server that restarted with
// different tools — into the current epoch's registry and, from it, every held
// seat's surface; audits the tool skills against the result, since the
// catalogue is one of the audit's two inputs; and tells the surfaces that
// render the catalogue ([Engine.SetOnCatalogueChanged]), which nothing else
// would until the next published company.
//
// AGAINST THE EPOCH CURRENT NOW. An apply may be building the next one; it
// read the bridge when it filed that epoch's registry, and refiles it from the
// bridge again when it publishes it ([Engine.installEpoch]), under the same
// lock — so a change this call filed into the epoch being replaced reaches its
// successor too, whichever of the two takes the lock first.
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
// registry, so a change to that registry reaches a held seat only through a
// new clone.
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

// refileSeat rebuilds one seat's surface after its bridge's own supervision
// changed the catalogue: a child that restarted with different tools.
//
// Nothing for a seat that was released, or replaced by a later claim, since
// the restart: its surface is no longer this bridge's to file.
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
// live catalogue: every MCP tool in it that the bridge no longer serves goes,
// and every one the bridge serves that it lacks is filed.
//
// ONLY MCP TOOLS ARE TOUCHED. The epoch's registry holds the builtins too,
// and a server's tool that collided with one was refused when it was filed;
// unregistering by name alone would take the builtin with it. report says
// whether such a refusal is logged — not on a publish, whose apply already
// logged it, so an operator reads each collision once per apply.
//
// The caller holds mcpFileMu.
func (e *Engine) fileSharedTools(ctx context.Context, c *Company, report bool) {
	if e.mcp == nil || c == nil || c.Tools == nil {
		return
	}
	// ONE READ of the bridge, so the tools removed and the tools filed are
	// two halves of the same catalogue.
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

// mcpHealth is a server's condition by its bridge's own record, for an
// outcome recorded when it was started — false when no bridge supervises it.
func mcpHealth(o mcpOutcome) (mcp.Health, bool) {
	if o.bridge == nil {
		return mcp.Health{}, false
	}
	return o.bridge.Health(o.instance)
}
