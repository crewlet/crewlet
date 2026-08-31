package engine

import (
	"context"
	"fmt"
	"maps"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/tools"
)

// Putting the company's MCP servers in front of its agents.
//
// This is the seam between three packages that each deliberately know nothing
// about the others: config parses `mcp_servers:` and validates it, mcp spawns
// and supervises children and hands back registrations, and tools holds a
// surface. Nothing joins them except this file, and until it existed the
// config surface was parsed, validated and published into the JSON Schema
// while doing nothing at all — every seat ran with the builtins alone, so no
// agent could post to chat, comment on an issue, or read a tracker.
//
// # Two lifetimes, because there are two kinds of server
//
// A SHARED server is one child for the company. It has no seat's credentials
// in it, every seat may call it, and it lives as long as the epoch — so it is
// started during the apply, into the epoch's own registry, and stopped when
// the next apply replaces that epoch.
//
// A PER-ROLE server is a TEMPLATE. Each role that declares mcp_env for it gets
// its own child carrying that seat's credentials, and those children are tied
// to the SEAT rather than to the epoch: this node spawns one only for a seat
// whose lease it holds, and kills it on release. Two reasons, and either alone
// would be enough. A fleet claims a slice of the company per node, so a node
// that spawned every seat's children would run the whole company's processes
// N times over. And the credentials in a child ARE one seat's identity, so a
// child that outlived its seat's lease would let this node keep acting as a
// seat a peer now runs.
//
// # Why a seat needs its own registry
//
// Two children of one template publish the SAME tool names, and that
// collides twice over — once in a registry and once in the bridge's own
// catalogue, which keys tools by name across every server it runs and
// SHADOWS the loser. Either collision has the same consequence: every seat
// calls whichever child won, acting under another seat's identity in the
// tracker or the chat backend, invisibly, because the call looks identical
// from the engine's side.
//
// So a claimed seat gets BOTH of its own — its own bridge, holding only its
// own children, and its own registry, cloned from the epoch's surface. That
// also makes teardown total: releasing a seat stops one bridge, and there is
// no way to leave a child of a seat this node no longer holds. [Company.ToolsFor]
// is what a turn is built against.
//
// # What a failure means
//
// A server that will not start does NOT fail the apply or the claim. It costs
// that server's tools and nothing else: the seat still has its builtins, the
// other servers still work, and the operator surface shows the group missing
// rather than the builtins mysteriously shrinking. Refusing the seat instead
// would take a company offline because one vendor's CLI was slow to install.

// startSharedServers brings up the company-wide children for an epoch.
//
// Called from the apply, so a revision that adds, removes or re-points a
// shared server takes effect on the next turn without a restart.
func (e *Engine) startSharedServers(ctx context.Context, c *Company) {
	if c == nil || e.mcp == nil {
		return
	}
	env := e.resolver()
	specs := make([]mcp.Spec, 0, len(c.Config.MCPServers))
	for _, server := range c.Config.MCPServers {
		if !server.IsShared() {
			continue
		}
		spec, err := serverSpec(server, server.Name, env, nil)
		if err != nil {
			log.ErrorContext(ctx, "mcp_server_unconfigured", "server", server.Name, "error", err,
				"detail", "this server contributes no tools; the rest of the company still starts")
			continue
		}
		specs = append(specs, spec)
	}
	// RECONCILED, not added. This runs on every apply against a bridge
	// whose children are already up — and the epoch it is filling is a
	// brand-new registry that has to be told about them again. Add refuses
	// a name it already runs, which cost the applied epoch every shared
	// server's tools; see Bridge.Reconcile.
	startAll(ctx, c.Tools, specs, e.mcp.Reconcile)
}

// stopSharedServers tears down what startSharedServers brought up.
//
// Every server, per-role children included: this runs when the engine stops,
// and a child left behind is a process holding a seat's credentials after the
// engine that vouched for it is gone.
func (e *Engine) stopSharedServers(ctx context.Context) {
	for _, bridge := range e.takeAllSeatBridges() {
		if _, err := bridge.StopAll(ctx); err != nil {
			log.WarnContext(ctx, "mcp_seat_stop_failed", "error", err)
		}
	}
	if e.mcp == nil {
		return
	}
	if _, err := e.mcp.StopAll(ctx); err != nil {
		log.WarnContext(ctx, "mcp_stop_failed", "error", err)
	}
}

// startSeatServers spawns a claimed seat's own children and returns the
// surface its turns run against.
//
// The returned registry is never nil. A seat whose company declares no
// per-role server gets the epoch's shared surface unchanged, which is the
// correct answer rather than a special case.
func (e *Engine) startSeatServers(ctx context.Context, c *Company, handle string) *tools.Registry {
	if c == nil {
		return nil
	}
	seat := c.Org.AgentSeatByHandle(handle)
	if seat == nil {
		return c.Tools
	}
	specs := seatSpecs(c, seat, e.resolver())
	if len(specs) == 0 {
		return c.Tools
	}
	// A BRIDGE OF ITS OWN, because the bridge's catalogue is keyed by tool
	// name across every server it runs: two seats' children of one template
	// publish the same names, and a shared bridge would hand the second
	// seat nothing while the first served both.
	bridge := mcp.NewBridge(nil)

	// CLONED, not shared: the seat's own tools must not reach a peer seat,
	// and the epoch's registry is read by every other seat on this node.
	reg := c.Tools.Clone()
	startAll(ctx, reg, specs, bridge.Add)
	e.setSeatBridge(ctx, handle, bridge, reg)
	return reg
}

// ToolsFor is the surface one seat's turns run against on this node.
//
// On the [Engine] rather than the [Company] because the answer depends on
// which seats THIS node holds: a fleet claims a slice of the company each,
// and only the node holding a seat's lease has its per-role children.
func (e *Engine) ToolsFor(handle string) *tools.Registry {
	return e.seatRegistry(e.Company(), handle)
}

// seatRegistry is [Engine.ToolsFor] for a caller that already holds the
// epoch, so the surface and the company a turn is built from are the same
// pair rather than two reads an apply can land between.
//
// The seat's own registry when this node has claimed it and started its
// per-role children, and the epoch's shared one otherwise. Never nil for a
// live epoch: a seat with no per-role children still has the builtins, and
// answering nil would fail the turn rather than run it with the tools it
// does have.
func (e *Engine) seatRegistry(c *Company, handle string) *tools.Registry {
	e.mcpMu.Lock()
	reg := e.seatTools[handle]
	e.mcpMu.Unlock()
	if reg != nil {
		return reg
	}
	if c == nil {
		return nil
	}
	return c.Tools
}

// refileSeatTools rebuilds every held seat's registry against a new epoch.
//
// An apply replaces the [Company], and with it the builtins and the shared
// MCP surface every seat registry was cloned from. The per-role CHILDREN are
// not replaced — they belong to the seat's lease, and restarting them on an
// apply would hand the company a credential re-handshake for every seat on
// every revision — so what goes stale is the registry, not the processes.
// Left alone it serves the previous revision's builtins and knobs to turns
// running under the new one.
//
// Rebuilt rather than carried forward, and rebuilt rather than restarted:
// clone the NEW epoch's surface, then re-file the live bridge's existing
// catalogue into it. The children never learn an apply happened.
//
// Called AFTER the pointer moves, like the mailbox and scheduler steps and
// for the same reason: an apply that is refused later must not have swapped
// the seat surfaces of an epoch that never became current.
func (e *Engine) refileSeatTools(ctx context.Context, c *Company) {
	if c == nil || c.Tools == nil {
		return
	}
	e.mcpMu.Lock()
	defer e.mcpMu.Unlock()
	for handle, bridge := range e.seatMCP {
		if bridge == nil {
			continue
		}
		reg := c.Tools.Clone()
		for _, registration := range bridge.Registrations() {
			if err := reg.RegisterMCP(registration); err != nil {
				log.WarnContext(ctx, "mcp_tool_refused", "seat", handle,
					"tool", registration.Tool.Name(), "error", err)
				continue
			}
		}
		e.seatTools[handle] = reg
	}
}

// startAll brings a set of servers up CONCURRENTLY and files what they
// contribute into reg in SPEC ORDER.
//
// Concurrent because each one is a subprocess spawn, a protocol handshake and
// a tools/list — hundreds of milliseconds at best and seconds against a vendor
// that is slow or absent — and they have nothing to do with each other. Done
// one at a time, a seat with three servers took three times as long to attach
// as its slowest one, and a seat is not consuming its mailbox until it does.
//
// Filed in SPEC ORDER regardless of which finished first, because the registry
// is keyed by tool name and a collision is resolved by who registered last.
// Filing in completion order would hand a seat a different surface depending
// on which vendor happened to answer first — a company whose behaviour changes
// between restarts for no reason anybody can see.
func startAll(ctx context.Context, reg *tools.Registry, specs []mcp.Spec,
	start func(context.Context, mcp.Spec) (mcp.Change, error),
) {
	type outcome struct {
		change mcp.Change
		err    error
	}
	out := make([]outcome, len(specs))
	var wg sync.WaitGroup
	for i, spec := range specs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i].change, out[i].err = start(ctx, spec)
		}()
	}
	wg.Wait()
	for i, spec := range specs {
		file(ctx, reg, spec.Name, out[i].change, out[i].err)
	}
}

// setSeatBridge installs a seat's bridge and the registry filed from it,
// stopping any predecessor.
//
// A predecessor is not hypothetical: a failed acquire releases the same seat,
// and a re-claim arrives while the previous teardown may not have run.
//
// BOTH under one lock, because a registry advertising the tools of a bridge
// this node no longer runs offers the model entries that can only fail.
func (e *Engine) setSeatBridge(ctx context.Context, handle string, bridge *mcp.Bridge,
	reg *tools.Registry,
) {
	e.mcpMu.Lock()
	previous, existed := e.seatMCP[handle]
	if e.seatMCP == nil {
		e.seatMCP = make(map[string]*mcp.Bridge)
	}
	if e.seatTools == nil {
		e.seatTools = make(map[string]*tools.Registry)
	}
	e.seatMCP[handle] = bridge
	e.seatTools[handle] = reg
	e.mcpMu.Unlock()
	if existed && previous != nil {
		// Detached from the map first, so nothing can reach it while it
		// is being shut down.
		// WithoutCancel: this is a teardown, and the thing being undone
		// is often the cancellation itself. A cleanup that inherited a
		// dead context would leave the predecessor's children running.
		if _, err := previous.StopAll(context.WithoutCancel(ctx)); err != nil {
			log.WarnContext(ctx, "mcp_seat_stop_failed", "seat", handle, "error", err)
		}
	}
}

// takeSeatBridge removes and returns a seat's bridge, forgetting its
// registry with it — a surface naming children this node has stopped is
// worse than the shared one it falls back to.
func (e *Engine) takeSeatBridge(handle string) *mcp.Bridge {
	e.mcpMu.Lock()
	defer e.mcpMu.Unlock()
	bridge := e.seatMCP[handle]
	delete(e.seatMCP, handle)
	delete(e.seatTools, handle)
	return bridge
}

// takeAllSeatBridges empties both maps, for shutdown.
func (e *Engine) takeAllSeatBridges() []*mcp.Bridge {
	e.mcpMu.Lock()
	defer e.mcpMu.Unlock()
	out := make([]*mcp.Bridge, 0, len(e.seatMCP))
	for _, b := range e.seatMCP {
		out = append(out, b)
	}
	clear(e.seatMCP)
	clear(e.seatTools)
	return out
}

// stopSeatServers kills a released seat's children.
//
// TOTAL, and idempotent: the seat's whole bridge goes, so there is no way to
// leave a child of a seat this node no longer holds — and running for a seat
// that never got one, which a failed acquire does, is a no-op.
func (e *Engine) stopSeatServers(ctx context.Context, handle string) {
	bridge := e.takeSeatBridge(handle)
	if bridge == nil {
		return
	}
	if _, err := bridge.StopAll(ctx); err != nil {
		log.WarnContext(ctx, "mcp_seat_stop_failed", "seat", handle, "error", err)
		return
	}
	log.InfoContext(ctx, "mcp_seat_stopped", "seat", handle)
}

// register starts one server and files what it serves.
//
// The registrations carry their own origin, so nothing here can file a
// server's tool as a builtin — which is the reading that makes a failed
// server look like a missing builtin instead.
// file applies one bridge change to one registry.
func file(ctx context.Context, reg *tools.Registry, server string, change mcp.Change, err error) {
	if err != nil {
		// Logged, not returned. One vendor's server failing to start
		// costs that server's tools; failing the apply over it would
		// take the whole company down with it.
		log.ErrorContext(ctx, "mcp_server_failed", "server", server, "error", err,
			"detail", "this server's tools are absent; its group is missing from the "+
				"catalogue rather than the builtins shrinking")
		return
	}
	// REMOVED FIRST. A name that moved between servers appears in both
	// lists, and registering before unregistering would file the new
	// object and then delete it by name.
	for _, name := range change.Removed {
		reg.Unregister(name)
	}
	filed := 0
	for _, registration := range change.Registrations() {
		if err := reg.RegisterMCP(registration); err != nil {
			log.WarnContext(ctx, "mcp_tool_refused",
				"server", server, "tool", registration.Tool.Name(), "error", err)
			continue
		}
		filed++
	}
	log.InfoContext(ctx, "mcp_server_started", "server", server, "tools", filed)
}

// seatSpecs is every per-role child this seat needs.
//
// Keyed off the seat's own mcp_env: a `shared: false` server the seat declares
// no credentials for gets no child, because a template with nobody's identity
// in it is a server nobody can act through.
func seatSpecs(c *Company, seat *org.Role, env *config.Resolver) []mcp.Spec {
	var out []mcp.Spec
	for _, server := range c.Config.MCPServers {
		if server.IsShared() {
			continue
		}
		values, declared := seat.MCPEnv[server.Name]
		if !declared {
			continue
		}
		spec, err := serverSpec(server, mcp.InstanceName(server.Name, seat.Name), env, values)
		if err != nil {
			log.Error("mcp_server_unconfigured", "server", server.Name, "seat", seat.Handle(),
				"error", err, "detail", "this seat gets no child for it")
			continue
		}
		out = append(out, spec)
	}
	return out
}

// serverSpec turns one config entry into a launchable spec.
//
// The ${VAR} references are resolved HERE, at the edge, and never inside
// internal/mcp: that package must not be able to read the secret store, which
// is what keeps a tool server's credentials on one path with one audit point.
func serverSpec(
	server config.MCPServer, instance string, env *config.Resolver, seatEnv map[string]string,
) (mcp.Spec, error) {
	if instance == "" {
		return mcp.Spec{}, fmt.Errorf("engine: mcp server %q has no instance name", server.Name)
	}
	spec := mcp.Spec{
		Name:                instance,
		Transport:           mcp.TransportKind(server.Transport),
		Command:             server.Command,
		Args:                server.Args,
		Env:                 resolveMap(env, server.Env),
		URL:                 env.Value(server.URL),
		Headers:             resolveMap(env, server.Headers),
		ToolPrefix:          server.ToolPrefix,
		AnnotationOverrides: annotationOverrides(server.ToolAnnotations),
		StartupTimeout:      mcpTimeout(server.StartupTimeoutSeconds),
		RequestTimeout:      mcpTimeout(server.RequestTimeoutSeconds),
	}
	// THE SEAT'S OWN VALUES WIN. A template's env is the shape every child
	// shares — a base URL, a workspace — and the seat's block is the
	// identity inside it, so a seat that names the same key is naming its
	// own credential rather than a second default.
	if len(seatEnv) > 0 {
		if spec.Env == nil {
			spec.Env = make(map[string]string, len(seatEnv))
		}
		maps.Copy(spec.Env, resolveMap(env, seatEnv))
	}
	return spec, nil
}

// resolveMap expands every ${VAR} in a config map.
func resolveMap(env *config.Resolver, in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = env.Value(v)
	}
	return out
}

// annotationOverrides converts the operator's per-tool hints.
//
// Tri-state on both sides and deliberately so: an UNSET toggle must arrive as
// Unknown rather than as No, because "the operator said nothing" and "the
// operator said this is not read-only" send the sub-agent guard opposite ways.
func annotationOverrides(in map[string]config.ToolAnnotations) map[string]mcp.Annotations {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]mcp.Annotations, len(in))
	for name, a := range in {
		out[name] = mcp.Annotations{
			ReadOnly:    hint(a.ReadOnly),
			Destructive: hint(a.Destructive),
			Idempotent:  hint(a.Idempotent),
			OpenWorld:   hint(a.OpenWorld),
		}
	}
	return out
}

func hint(t config.Toggle) mcp.Hint {
	if !t.IsSet() {
		return mcp.Unknown
	}
	if t.Or(false) {
		return mcp.Yes
	}
	return mcp.No
}

// mcpTimeout converts a config duration, treating 0 as "take mcp's default".
//
// NOT engine.seconds, which floors at nothing: this one has to hand a literal
// zero through, because zero is how a Spec asks for the package's own default
// rather than for no timeout at all.
func mcpTimeout(v float64) time.Duration {
	if v <= 0 {
		return 0
	}
	return time.Duration(v * float64(time.Second))
}
