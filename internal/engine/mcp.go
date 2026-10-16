package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
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
// reconciled during the apply, into the epoch's own registry, and stopped when
// a revision no longer declares it.
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
// Tied to the lease is not FROZEN at the claim, though. While the seat is held
// every apply reconciles its children too — against the spec the new epoch
// and the secret snapshot that apply refreshed produce — and a child that
// would not start is retried; see seatservers.go. Both kinds of server
// therefore follow one rule: a child whose resolved spec is unchanged is left
// running, and one whose spec moved, a sealed or rotated credential included,
// is restarted.
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
// no way to leave a child of a seat this node no longer holds. [Engine.ToolsFor]
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
// shared server — or rotates a credential one resolves — takes effect on the
// next turn without a restart.
func (e *Engine) startSharedServers(ctx context.Context, c *Company) {
	if c == nil || e.mcp == nil {
		return
	}
	env := e.resolver()
	specs := make([]mcp.Spec, 0, len(c.Config.MCPServers))
	var outcomes []mcpOutcome
	for _, server := range c.Config.MCPServers {
		if !server.IsShared() {
			continue
		}
		spec, err := serverSpec(server, server.Name, env, nil)
		if err != nil {
			log.ErrorContext(ctx, "mcp_server_unconfigured", "server", server.Name, "error", err,
				"detail", "this server contributes no tools; the rest of the company still starts")
			outcomes = append(outcomes, mcpOutcome{server: server.Name, err: err})
			continue
		}
		specs = append(specs, spec)
	}
	// RETIRED FIRST, because Reconcile is driven by the specs the CURRENT
	// config names and therefore never visits a server the revision
	// DELETED. Left alone, that child — holding the company's credentials
	// — runs until the engine stops, and a RENAME runs two of them. Both
	// contradict this function's own contract: an apply that removes a
	// shared server is supposed to take effect on the next turn.
	//
	// Before startAll rather than after, so a rename frees the name and
	// the tools it published before the replacement files its own.
	// e.mcp holds only shared servers — a seat's children live on their
	// own bridge — so every absentee here is genuinely one this revision
	// dropped.
	desired := make(map[string]struct{}, len(specs))
	for _, spec := range specs {
		desired[spec.Name] = struct{}{}
	}
	for _, name := range e.mcp.Servers() {
		if _, keep := desired[name]; keep {
			continue
		}
		// The Change is discarded rather than filed: c.Tools is the epoch's
		// BRAND-NEW registry, which has not been told about any shared
		// server yet, so there is nothing here to unregister. What startAll
		// files below is the surviving servers' catalogue.
		if _, err := e.mcp.Stop(ctx, name); err != nil {
			log.WarnContext(ctx, "mcp_server_retire_failed", "server", name, "error", err,
				"detail", "its tools are gone from the catalogue; the child may have "+
					"survived and is reaped when the engine stops")
			continue
		}
		log.InfoContext(ctx, "mcp_server_retired", "server", name,
			"detail", "the revision no longer declares this shared server")
	}
	// RECONCILED, not added. This runs on every apply against a bridge
	// whose children are already up — and the epoch it is filling is a
	// brand-new registry that has to be told about them again. Add refuses
	// a name it already runs, which cost the applied epoch every shared
	// server's tools; see Bridge.Reconcile.
	outcomes = append(outcomes, startAll(ctx, c.Tools, specs, e.mcp, e.mcp.Reconcile)...)
	// REPLACED WHOLE, per apply: the outcomes describe the servers THIS
	// revision declares, and a server a revision dropped must leave the
	// heartbeat with it rather than be reported as still started.
	e.mcpMu.Lock()
	e.mcpShared = outcomes
	e.mcpMu.Unlock()
}

// stopSharedServers tears down what startSharedServers brought up.
//
// Every server, per-role children included: this runs when the engine stops,
// and a child left behind is a process holding a seat's credentials after the
// engine that vouched for it is gone.
func (e *Engine) stopSharedServers(ctx context.Context) {
	for _, s := range e.takeAllSeatChildren() {
		e.retireSeatChildren(ctx, s)
	}
	if e.mcp == nil {
		return
	}
	if _, err := e.mcp.StopAll(ctx); err != nil {
		log.WarnContext(ctx, "mcp_stop_failed", "error", err)
	}
}

// mcpOutcome is what one start of one MCP instance concluded, kept for the
// heartbeat's [coord.NodeStatus.MCP].
//
// Recorded at the start, and not only asked of the bridge on each beat,
// because a FAILED start can leave nothing in the bridge to ask: a seat's
// bridge keeps nothing of a start whose discovery failed (see [mcp.Bridge.Add];
// the seat's runner retries it), and a spec whose ${VAR} references did not
// resolve never reached a bridge at all. A report read off the bridges alone
// would show such a server as one that was never configured — the one row an
// operator opening the settings screen is looking for.
type mcpOutcome struct {
	server string    // the config's own name, never an instance name
	seat   uuid.UUID // the seat's agent id, uuid.Nil for a shared server
	tools  int
	// unlisted are the entry's tool_annotations keys this instance's
	// listing names no tool for ([mcp.Bridge.UnlistedOverrides]).
	unlisted []string
	err      error

	// bridge and instance name the server this start handed to a bridge,
	// whose supervision then owns what happens to it: a child that started
	// can die, and a start that failed can be retried into one that came
	// up. [Engine.mcpStatus] reports what the bridge says NOW wherever it
	// still supervises the instance, and this start's conclusion only where
	// it does not. Nil for a spec that never reached a bridge.
	bridge   *mcp.Bridge
	instance string
}

// observe records what a started instance serves, off the bridge it runs on.
func (o *mcpOutcome) observe(bridge *mcp.Bridge, instance string) {
	o.tools = len(bridge.ServerTools(instance))
	o.unlisted = bridge.UnlistedOverrides(instance)
}

// mcpStatus is this node's MCP servers as the heartbeat publishes them: one
// row per configured server, its instances counted.
//
// AS THEY ARE, not as they started: an instance its bridge still supervises
// is reported by the bridge's own record, so a child that died since its
// start counts as failed, with what took it down, until it is back.
//
// Sorted by server, and a server's reported error is its first failure by the
// seat's agent id, so a beat that re-sends the same facts sends the same
// bytes.
func (e *Engine) mcpStatus() []coord.MCPServerStatus {
	e.mcpMu.Lock()
	all := slices.Clone(e.mcpShared)
	for _, outcomes := range e.mcpSeats {
		all = append(all, outcomes...)
	}
	e.mcpMu.Unlock()
	slices.SortFunc(all, func(a, b mcpOutcome) int {
		return cmp.Or(cmp.Compare(a.server, b.server), cmp.Compare(a.seat.String(), b.seat.String()))
	})
	var out []coord.MCPServerStatus
	// unlistedBy counts, per key, the started instances of the current row
	// that do not list it.
	var unlistedBy map[string]int
	settle := func() {
		if len(out) == 0 {
			return
		}
		row := &out[len(out)-1]
		// A KEY IS UNLISTED BY THE SERVER only where NO started instance
		// lists it. One instance's listing is not the server's: a child
		// serves what its seat's credentials reach, so a key one seat's
		// child does not list may be exactly right for the next.
		for key, n := range unlistedBy {
			if n == row.Started {
				row.Unlisted = append(row.Unlisted, key)
			}
		}
		slices.Sort(row.Unlisted)
	}
	for _, o := range all {
		if n := len(out); n == 0 || out[n-1].Server != o.server {
			settle()
			out = append(out, coord.MCPServerStatus{Server: o.server, Shared: o.seat == uuid.Nil})
			unlistedBy = map[string]int{}
		}
		row := &out[len(out)-1]
		tools, unlisted, failure := o.tools, o.unlisted, o.err
		if health, ok := mcpHealth(o); ok {
			// THE LISTING NOW, like its tools: a restart can come back
			// listing a different set, so the keys it leaves out are read
			// off the same record.
			tools, unlisted, failure = health.Tools, o.bridge.UnlistedOverrides(o.instance), nil
			if !health.Serving {
				failure = errors.New(health.Down)
			}
		}
		if failure != nil {
			row.Failed++
			if row.Error == "" {
				row.Error, row.ErrorSeat = failure.Error(), o.seat
			}
			continue
		}
		row.Started++
		row.Tools = max(row.Tools, tools)
		for _, key := range unlisted {
			unlistedBy[key]++
		}
	}
	settle()
	return out
}

// SharedServers names the company-wide MCP servers this node's bridge
// supervises: serving, or down and being brought back — a child that died, or
// a start that failed and is being retried, which has no process at all.
//
// The BRIDGE's own record rather than the config's, which is the whole point:
// what a revision declares and what this process supervises are different
// facts, and only the second one says whether a server a revision removed was
// actually stopped. Whether one is answering is [mcp.Bridge.Health]'s.
func (e *Engine) SharedServers() []string {
	if e.mcp == nil {
		return nil
	}
	return e.mcp.Servers()
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

// startAll brings a set of servers up CONCURRENTLY and files what they
// contribute into reg in SPEC ORDER.
//
// Concurrent because each one is a subprocess spawn, a protocol handshake and
// a tools/list (hundreds of milliseconds at best and seconds against a vendor
// that is slow or absent), and they have nothing to do with each other. Done
// one at a time, an apply with three servers took three times as long as its
// slowest one, and no turn is built on the epoch until the apply is done. A
// seat's own children are brought up the same way, by [bringUp].
//
// Filed in SPEC ORDER regardless of which finished first, because the registry
// is keyed by tool name and a collision is resolved by who registered last.
// Filing in completion order would hand a seat a different surface depending
// on which vendor happened to answer first (a company whose behaviour changes
// between restarts for no reason anybody can see).
//
// What each start concluded is returned, in spec order, for the heartbeat.
// The tool count is the BRIDGE's for that instance rather than the change's:
// a change also names tools of other servers that a start un-shadowed, and
// the question the count answers is what this server serves.
func startAll(ctx context.Context, reg *tools.Registry, specs []mcp.Spec, bridge *mcp.Bridge,
	start func(context.Context, mcp.Spec) (mcp.Change, error),
) []mcpOutcome {
	type outcome struct {
		change mcp.Change
		err    error
	}
	out := make([]outcome, len(specs))
	var wg sync.WaitGroup
	for i, spec := range specs {
		wg.Go(func() {
			out[i].change, out[i].err = start(ctx, spec)
		})
	}
	wg.Wait()
	outcomes := make([]mcpOutcome, len(specs))
	for i, spec := range specs {
		file(ctx, reg, spec.Name, out[i].change, out[i].err)
		outcomes[i] = mcpOutcome{server: mcp.ServerName(spec.Name), err: out[i].err,
			bridge: bridge, instance: spec.Name}
		if out[i].err == nil {
			outcomes[i].observe(bridge, spec.Name)
		}
	}
	return outcomes
}

// file applies one bridge change to one registry.
//
// The registrations carry their own origin, so nothing here can file a
// server's tool as a builtin — which is the reading that makes a failed
// server look like a missing builtin instead.
//
// A server that is DOWN ([mcp.ErrNotRunning]: its child died, or its start
// failed, and its bridge is bringing it back) has its change filed BESIDE the
// error. Its tools stay listed and refuse until it is back, and they are filed
// here, in spec order, rather than appended when the epoch is published: a
// registry renders a prompt in the order it was filed, and the same revision
// applied while the server was up must list the same tools in the same order.
func file(ctx context.Context, reg *tools.Registry, server string, change mcp.Change, err error) {
	down := errors.Is(err, mcp.ErrNotRunning)
	if err != nil && !down {
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
	// object and then delete it by name. And ONLY AN MCP TOOL: a server's
	// tool that collided with a builtin was refused when it was filed, so
	// the name in this registry is the builtin's, and unregistering by name
	// alone took the builtin with it when the server stopped offering it.
	for _, name := range change.Removed {
		if entry, ok := reg.Lookup(name); ok {
			if _, fromMCP := entry.FromMCP(); fromMCP {
				reg.Unregister(name)
			}
		}
	}
	filed := 0
	for _, registration := range change.Registrations() {
		if refused := reg.RegisterMCP(registration); refused != nil {
			log.WarnContext(ctx, "mcp_tool_refused",
				"server", server, "tool", registration.Tool.Name(), "error", refused)
			continue
		}
		filed++
	}
	if down {
		log.ErrorContext(ctx, "mcp_server_failed", "server", server, "error", err, "tools", filed,
			"detail", "this server is down and its bridge is bringing it back; its tools "+
				"stay listed and refuse until it is")
		return
	}
	log.InfoContext(ctx, "mcp_server_started", "server", server, "tools", filed)
}

// seatSpecs is every per-role child this seat needs.
//
// Keyed off [config.MCPServer.Grants], the rule the org projection publishes
// too: a `shared: false` server the seat declares no credentials for gets no
// child, because a template with nobody's identity in it is a server nobody
// can act through.
//
// A template whose ${VAR} references do not resolve into a launchable spec
// comes back as an outcome instead, so the heartbeat reports it failed rather
// than leaving it out as though the seat never declared it.
func seatSpecs(c *Company, seat *org.Role, env *config.Resolver) ([]mcp.Spec, []mcpOutcome) {
	var out []mcp.Spec
	var unconfigured []mcpOutcome
	for _, server := range c.Config.MCPServers {
		// The epoch runs the shared ones; a template this seat is not
		// granted has nobody's identity to run as.
		if server.IsShared() || !server.Grants(seat) {
			continue
		}
		values := seat.MCPEnv[server.Name]
		spec, err := serverSpec(server, mcp.InstanceName(server.Name, seat.Name), env, values)
		if err != nil {
			log.Error("mcp_server_unconfigured", "server", server.Name, "seat", seat.Handle(),
				"error", err, "detail", "this seat gets no child for it")
			unconfigured = append(unconfigured, mcpOutcome{server: server.Name, err: err})
			continue
		}
		out = append(out, spec)
	}
	return out, unconfigured
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
