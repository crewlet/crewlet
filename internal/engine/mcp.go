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

	awsint "github.com/crewlet/crewlet/internal/aws"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/tools"
)

// Putting the company's MCP servers in front of its agents: the one seam
// joining config (parses `mcp_servers:`), mcp (spawns and supervises children)
// and tools (holds a surface), which know nothing of each other.
//
// A SHARED server is one child for the company, tied to the epoch. A PER-ROLE
// server is a template with one child per granted seat, carrying that seat's
// credentials and tied to the seat's LEASE: spawned only for a seat this node
// holds, killed on release, reconciled on every apply while held
// (seatservers.go). A claimed seat gets its own bridge and registry, since two
// children of one template publish the same tool names. A server that will not
// start costs only its tools, never the apply or the claim (package doc, "MCP
// servers").

// startSharedServers brings up the company-wide children for an epoch. Called
// from the apply, so a revision that adds, removes or re-points a shared
// server, or rotates a credential one resolves, takes effect on the next turn.
func (e *Engine) startSharedServers(ctx context.Context, c *Company) {
	if c == nil || e.mcp == nil {
		return
	}
	env := e.resolver()
	specs := make([]mcp.Spec, 0, len(c.Config.MCPServers))
	var outcomes []mcpOutcome
	for i := range c.Config.MCPServers {
		server := &c.Config.MCPServers[i]
		// A SERVER TURNED OFF is not resolved either, so a reference of its
		// that does not resolve is no missing credential to report. Retired
		// below if running.
		if !server.IsShared() || !server.IsEnabled() {
			continue
		}
		// HELD TO ITS REFERENCES like a seat's child (package doc, "Tool
		// server credentials"): one whose key does not resolve is kept from
		// the bridge, whose retries would wait on a value nothing is about to
		// supply; the next apply (every snapshot change is one) starts it.
		values, missing := resolveServer(server, "", server.Name, nil, env, nil)
		if missing != nil {
			outcomes = append(outcomes, mcpOutcome{server: server.Name, err: missing})
			continue
		}
		spec, err := serverSpec(server, server.Name, values)
		if err != nil {
			log.ErrorContext(ctx, "mcp_server_unconfigured", "server", server.Name, "error", err,
				"detail", "this server contributes no tools; the rest of the company still starts")
			outcomes = append(outcomes, mcpOutcome{server: server.Name, err: err})
			continue
		}
		// SIGNED IN TO WITH OAUTH: under the company's grant, read afresh
		// each pass and held back while there is none (or its vendor refused
		// to renew it) until a person signs in (oauthgrant.go); or as its
		// service identity, tokens issued on use (mcpserviceidentity.go).
		// Retired below if running.
		if server.OAuth != nil {
			bearer, needed := e.tokenSourceFor(ctx, server, env)
			if needed != nil {
				outcomes = append(outcomes, mcpOutcome{server: server.Name, err: needed})
				continue
			}
			spec.Bearer = bearer
		}
		specs = append(specs, spec)
	}
	outcomes = e.withheldForCredentials(ctx, outcomes)
	// RETIRED FIRST: Reconcile visits only the servers the current config
	// names, so a child the revision DELETED (holding the company's
	// credentials) would run until the engine stops, and a RENAME would run
	// two. Before startAll, so a rename frees the name and its tools before
	// the replacement files its own. e.mcp holds only shared servers (a
	// seat's children have their own bridge), so every absentee is one this
	// revision dropped or whose credential stopped resolving.
	desired := make(map[string]struct{}, len(specs))
	for _, spec := range specs {
		desired[spec.Name] = struct{}{}
	}
	withheld := withheldByInstance(outcomes)
	off := serversTurnedOff(c.Config)
	for _, name := range e.mcp.Servers() {
		if _, keep := desired[name]; keep {
			continue
		}
		// The Change is discarded: c.Tools is the epoch's BRAND-NEW
		// registry, told of no shared server yet, so nothing is unregistered.
		if _, err := e.mcp.Stop(ctx, name); err != nil {
			log.WarnContext(ctx, "mcp_server_retire_failed", "server", name, "error", err,
				"detail", "its tools are gone from the catalogue; the child may have "+
					"survived and is reaped when the engine stops")
			continue
		}
		if missing, ok := withheld[name]; ok {
			// A STOPPED CREDENTIAL STOPS THE SERVER, by the rule that would
			// not start it: else a key deleted from the store would leave the
			// child running on the old value.
			log.InfoContext(ctx, "mcp_server_retired", "server", name,
				"detail", "a value the server is started with no longer resolves, so it "+
					"is not left running on the one it was started with: "+missing.reason())
			continue
		}
		if _, turnedOff := off[name]; turnedOff {
			log.InfoContext(ctx, "mcp_server_retired", "server", name, "detail", turnedOffDetail)
			continue
		}
		if needed := signInHeld(outcomes, name); needed != nil {
			// A GRANT GIVEN UP OR REFUSED STOPS THE SERVER, by the rule that
			// would not start it: nobody is signed in to it any more.
			log.InfoContext(ctx, "mcp_server_retired", "server", name, "detail", needed.Error())
			continue
		}
		log.InfoContext(ctx, "mcp_server_retired", "server", name,
			"detail", "the revision no longer declares this shared server")
	}
	// RECONCILED, not added: after the first apply the bridge's children are
	// already up, and the brand-new registry must be told of them again. Add
	// refuses a name it already runs, which would cost the epoch every shared
	// server's tools (Bridge.Reconcile).
	outcomes = append(outcomes, startAll(ctx, c.Tools, specs, e.mcp, e.mcp.Reconcile)...)
	// REPLACED WHOLE per apply, so a server the revision dropped leaves the
	// status record rather than reading as still started.
	e.mcpMu.Lock()
	stampSince(e.mcpShared, outcomes, e.now())
	e.mcpShared = outcomes
	e.mcpMu.Unlock()
}

// signInHeld is the sign-in a pass held server back for, nil where it did not.
func signInHeld(outcomes []mcpOutcome, server string) *signInNeeded {
	for _, o := range outcomes {
		var needed *signInNeeded
		if o.server == server && errors.As(o.err, &needed) {
			return needed
		}
	}
	return nil
}

// serversTurnedOff names the servers cfg declares and turns off
// ([config.MCPServer.IsEnabled]), by their configured name.
//
// ONLY FOR THE RETIREMENT'S LOG LINE: what a pass starts is decided where each
// spec is built ([config.MCPServer.Grants]); this keeps a retired child from
// being logged as "no longer declared" while its entry is still there.
func serversTurnedOff(cfg *config.Company) map[string]struct{} {
	if cfg == nil {
		return nil
	}
	off := map[string]struct{}{}
	for i := range cfg.MCPServers {
		if !cfg.MCPServers[i].IsEnabled() {
			off[cfg.MCPServers[i].Name] = struct{}{}
		}
	}
	return off
}

// serverDescriptions is what each server cfg declares is for, by its name, for
// the epoch's registry to stamp on the server's tools
// ([tools.Registry.DescribeServers]); a server with no description is left
// out.
func serverDescriptions(cfg *config.Company) map[string]string {
	out := map[string]string{}
	for i := range cfg.MCPServers {
		if d := cfg.MCPServers[i].Description; d != "" {
			out[cfg.MCPServers[i].Name] = d
		}
	}
	return out
}

// turnedOffDetail is what a child retired because its server was turned off
// is logged with.
const turnedOffDetail = "the revision turns this server off: its entry and everything " +
	"naming it are kept, and nothing runs it until it is turned back on"

// stopSharedServers tears down what startSharedServers brought up, per-role
// children included: this runs when the engine stops, and a child left behind
// holds a seat's credentials after the engine that vouched for it is gone.
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
// node's status record ([coord.NodeStatusRecord.MCP]).
//
// Recorded at the start because a FAILED start can leave nothing in a bridge
// to ask (a seat's bridge keeps nothing of a failed discovery, [mcp.Bridge.Add];
// an unresolved ${VAR} never reaches a bridge), and off the bridges alone such
// a server would read as never configured: the row an operator looks for.
type mcpOutcome struct {
	server string    // the config's own name, never an instance name
	seat   uuid.UUID // the seat's agent id, uuid.Nil for a shared server
	tools  int
	// unlisted are the entry's tool_annotations keys this instance's
	// listing names no tool for ([mcp.Bridge.UnlistedOverrides]).
	unlisted []string
	err      error

	// since is when the instance stopped serving, or first failed or was
	// held back, carried across retries while it stays down ([stampSince]).
	// Zero for a successful start, whose bridge dates its serving.
	since time.Time

	// bridge and instance name where this start was handed, whose
	// supervision then owns it (a started child can die, a failed start be
	// retried up): [Engine.mcpStatus] reports the bridge's view while it
	// supervises the instance, this conclusion otherwise. Nil for a spec
	// that never reached a bridge.
	bridge   *mcp.Bridge
	instance string
}

// stampSince dates each unsuccessful outcome of next (failed or held back) by
// prev's unsuccessful one for the same server and seat, else now, so an
// instance down across passes and applies keeps the instant it went down.
func stampSince(prev, next []mcpOutcome, now time.Time) {
	for i := range next {
		if next[i].err == nil {
			continue
		}
		next[i].since = now
		for _, p := range prev {
			if p.server == next[i].server && p.seat == next[i].seat && p.err != nil && !p.since.IsZero() {
				next[i].since = p.since
				break
			}
		}
	}
}

// observe records what a started instance serves, off the bridge it runs on.
func (o *mcpOutcome) observe(bridge *mcp.Bridge, instance string) {
	o.tools = len(bridge.ServerTools(instance))
	o.unlisted = bridge.UnlistedOverrides(instance)
}

// mcpStatus is this node's MCP servers for its status record, sorted by
// server: a row per configured server off the same [mcpReading]s as
// [Engine.MCPInstances], with restarts summed, tools lost to names ([unfiled])
// and the DownSince and WaitingSince the MCP alarms measure from; every choice
// follows the readings' order (server, then seat), so same facts, same bytes.
//
// A child held back for a credential not sealed yet is WAITING, not failed (a
// fault on the screen an operator connecting the integration watches), JUDGED
// AGAIN EACH READING ([credentialMissing.judged]): its pass ran at the claim,
// on a first boot before the minting integration's, so it turns failed while
// that integration reports its credential missing or the seat's account off;
// an unreadable record keeps the pass's judgement ([Engine.mintingStates]).
func (e *Engine) mcpStatus(ctx context.Context) []coord.MCPServerStatus {
	var out []coord.MCPServerStatus
	// unlistedBy counts, per key, the started instances of the current row
	// that do not list it, and notes gathers what the row's instances lost
	// to their names.
	var unlistedBy map[string]int
	var notes toolNotes
	settle := func() {
		if len(out) == 0 {
			return
		}
		row := &out[len(out)-1]
		notes.settle(row)
		// A KEY IS UNLISTED BY THE SERVER only where NO started instance
		// lists it: a child serves what its seat's credentials reach, so one
		// seat's listing is not the server's.
		for key, n := range unlistedBy {
			if n == row.Started {
				row.Unlisted = append(row.Unlisted, key)
			}
		}
		slices.Sort(row.Unlisted)
	}
	for _, r := range e.mcpReadings(ctx) {
		if n := len(out); n == 0 || out[n-1].Server != r.server {
			settle()
			out = append(out, coord.MCPServerStatus{Server: r.server, Shared: r.seat == uuid.Nil})
			unlistedBy = map[string]int{}
			notes = toolNotes{}
		}
		row := &out[len(out)-1]
		notes.add(r.refused, r.shadowed)
		row.Restarts += r.restarts
		switch r.state {
		case coord.MCPInstanceWaiting:
			row.Waiting++
			// THE LONGEST WAIT names the row, dated as an outage is
			// ([stampSince]), so `mcp_server_waiting_long` measures the whole
			// wait and the wait, its seat and WaitingSince are one instance's.
			if row.Wait == "" || datedBefore(r.since, row.WaitingSince) {
				row.Wait, row.WaitSeat, row.WaitingSince = r.wait.Error(), r.seat, r.since
			}
		case coord.MCPInstanceFailed:
			row.Failed++
			// THE LONGEST OUTAGE names the row, so the error, its seat, class
			// and DownSince are one instance's facts and an alarm quoting them
			// describes a real instance. Ties and undated instances go to the
			// lower seat id, the readings' order.
			if row.Error == "" || datedBefore(r.since, row.DownSince) {
				// REDACTED, as a page's instance is: the row is on a lease
				// every node reads, and a failure can carry what a
				// server's process printed.
				row.Error, row.ErrorSeat, row.ErrorClass = redact.Secrets(r.failure.Error()), r.seat, string(r.class)
				row.DownSince = r.since
			}
		default:
			row.Started++
			row.Tools = max(row.Tools, r.tools)
			for _, key := range r.unlisted {
				unlistedBy[key]++
			}
		}
	}
	settle()
	return out
}

// datedBefore reports whether since dates an instance before the one at named,
// where a dated instance comes before one nothing dated (zero).
func datedBefore(since, named time.Time) bool {
	return !since.IsZero() && (named.IsZero() || since.Before(named))
}

// filedOutcome is a start's outcome beside the surface its instance's tools
// are filed into, for [Engine.mcpStatus].
type filedOutcome struct {
	mcpOutcome
	surface *tools.Registry
}

// unfiled is every tool of o's instance not offered under its name: those its
// bridge refused ([mcp.Bridge.RefusedTools]) or shadowed for another server's
// ([mcp.Bridge.ShadowedTools]), and those surface (the registry it is filed
// into) holds another tool under: a built-in ([mcp.NameBuiltin], built-ins
// file first) or another server's tool, which shadows it.
//
// READ AT THE BEAT, never kept: an apply, a restart or a catalogue change
// refiles a registry. A name surface holds for the same server's tool is that
// tool not refiled yet, not a collision.
func unfiled(o mcpOutcome, surface *tools.Registry) ([]mcp.RefusedTool, []mcp.ShadowedTool) {
	refused := o.bridge.RefusedTools(o.instance)
	shadowed := o.bridge.ShadowedTools(o.instance)
	if surface == nil {
		return refused, shadowed
	}
	onBridge := make(map[string]struct{}, len(shadowed))
	for _, s := range shadowed {
		onBridge[s.Name] = struct{}{}
	}
	for _, t := range o.bridge.ServerTools(o.instance) {
		if _, lost := onBridge[t.Name()]; lost {
			continue
		}
		entry, filed := surface.Lookup(t.Name())
		if !filed || entry.Tool == mcp.Callable(t) {
			continue
		}
		server, fromMCP := entry.FromMCP()
		switch {
		case !fromMCP:
			refused = append(refused, mcp.RefusedTool{Name: t.Name(), Reason: mcp.NameBuiltin})
		case server != o.server:
			shadowed = append(shadowed, mcp.ShadowedTool{Name: t.Name(), By: server})
		}
	}
	return refused, shadowed
}

// toolNotes gathers what one row's instances lost to their names, each tool
// once per reason or server however many instances lost it.
type toolNotes struct {
	refused  map[mcp.RefusedTool]struct{}
	shadowed map[mcp.ShadowedTool]struct{}
}

func (n *toolNotes) add(refused []mcp.RefusedTool, shadowed []mcp.ShadowedTool) {
	for _, r := range refused {
		if n.refused == nil {
			n.refused = map[mcp.RefusedTool]struct{}{}
		}
		n.refused[r] = struct{}{}
	}
	for _, s := range shadowed {
		if n.shadowed == nil {
			n.shadowed = map[mcp.ShadowedTool]struct{}{}
		}
		n.shadowed[s] = struct{}{}
	}
}

// settle writes the notes onto row: sorted by tool, at most
// [coord.MaxMCPToolNotes] of each, and how many there are in all.
func (n *toolNotes) settle(row *coord.MCPServerStatus) {
	refused := slices.SortedFunc(maps.Keys(n.refused), func(a, b mcp.RefusedTool) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.Reason, b.Reason))
	})
	for _, r := range refused[:min(len(refused), coord.MaxMCPToolNotes)] {
		row.Refused = append(row.Refused, coord.MCPToolRefusal{Tool: r.Name, Reason: string(r.Reason)})
	}
	row.RefusedCount = len(refused)
	shadowed := slices.SortedFunc(maps.Keys(n.shadowed), func(a, b mcp.ShadowedTool) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.By, b.By))
	})
	for _, s := range shadowed[:min(len(shadowed), coord.MaxMCPToolNotes)] {
		row.Shadowed = append(row.Shadowed, coord.MCPToolShadow{Tool: s.Name, By: s.By})
	}
	row.ShadowedCount = len(shadowed)
}

// SharedServers names the company-wide MCP servers this node's bridge
// supervises: serving, or down and being brought back (a dead child, or a
// failed start being retried, which has no process).
//
// The BRIDGE's record, not the config's: only it says whether a server a
// revision removed was actually stopped. Whether one answers is
// [mcp.Bridge.Health]'s.
func (e *Engine) SharedServers() []string {
	if e.mcp == nil {
		return nil
	}
	return e.mcp.Servers()
}

// ToolsFor is the surface one seat's turns run against on this node: on the
// [Engine] rather than the [Company] because only the node holding a seat's
// lease has its per-role children.
func (e *Engine) ToolsFor(handle string) *tools.Registry {
	return e.seatRegistry(e.Company(), handle)
}

// seatRegistry is [Engine.ToolsFor] for a caller already holding the epoch,
// so a turn's surface and company are one pair, not two reads an apply can
// land between.
//
// The seat's own registry once this node has claimed it and started its
// per-role children, else the epoch's shared one as the seat holds it
// ([grantedSurface]). Never nil for a live epoch: a seat with no per-role
// children still has the builtins, and nil would fail the turn.
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
	return grantedSurface(c, c.Org.AgentSeatByHandle(handle))
}

// grantedSurface is the epoch's surface as seat holds it: a clone without the
// tools of each shared server the seat is not granted
// ([config.MCPServer.Grants]), held to the seat's limits ([limitSurface]).
//
// Cut here because a shared server runs and files one instance for the
// company, so one granted to chosen seats is removed from every other seat's
// copy. Every surface a seat's turn is built against comes through this: a
// held seat's ([seatSurface]) and the fallback for a seat with no children
// here. A seat the epoch does not have (nil) holds no shared server.
func grantedSurface(c *Company, seat *org.Role) *tools.Registry {
	reg := sharedSurface(c, seat)
	limitSurface(reg, seat)
	return reg
}

// sharedSurface is the epoch's surface without the tools of each shared server
// seat is not granted: [grantedSurface] before the seat's limits are applied,
// for [seatSurface] to file the seat's own children into first.
func sharedSurface(c *Company, seat *org.Role) *tools.Registry {
	reg := c.Tools.Clone()
	for i := range c.Config.MCPServers {
		server := &c.Config.MCPServers[i]
		if server.IsShared() && !server.Grants(seat) {
			reg.UnregisterOrigin(tools.Origin(server.Name))
		}
	}
	return reg
}

// limitSurface holds each MCP tool on reg to the seat's limits
// ([org.Role.LimitOn]): a tool a limit blocks is taken off, and one it holds
// at approve waits for a person's answer before each call.
//
// THE STRICTER OF THE TWO: the entry's level was applied where the tools were
// filed, and a limit only removes a tool or adds the wait, so a seat never
// gets more than its entry gives. HERE, on the seat's own copy, because one
// shared instance is filed once for every seat and limits are per seat; every
// surface a seat's turn, executor bridge and held calls use is cut here
// ([grantedSurface], [seatSurface]), its own children's tools included. A
// server a limit turns off is not on the surface ([config.MCPServer.Grants]).
func limitSurface(reg *tools.Registry, seat *org.Role) {
	if seat == nil || len(seat.Limits) == 0 {
		return
	}
	for _, entry := range reg.List() {
		server, fromMCP := entry.FromMCP()
		if !fromMCP {
			continue
		}
		tool, ok := entry.Tool.(*mcp.Tool)
		if !ok {
			continue
		}
		switch level, limited := seat.LimitOn(server).Tool(tool.RawName()); {
		case !limited:
		case level == org.ToolApprove:
			reg.RequireApproval(entry.Name())
		default:
			reg.Unregister(entry.Name())
		}
	}
}

// startAll brings a set of servers up CONCURRENTLY and files what they
// contribute into reg in SPEC ORDER, returning each start's outcome in spec
// order for the status record.
//
// Concurrent because each is a spawn, handshake and tools/list (seconds
// against a slow vendor), independent of the others, and no turn is built on
// the epoch until the apply is done; a seat's children start the same way
// ([bringUp]). In spec order because a name collision goes to whoever
// registered last, so completion order would change a seat's surface between
// restarts. The tool count is the BRIDGE's for the instance, not the change's,
// which also names other servers' tools a start un-shadowed.
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

// file applies one bridge change to one registry. The registrations carry
// their own origin, so a server's tool is never filed as a builtin (which
// would make a failed server look like a missing builtin).
//
// A server that is DOWN ([mcp.ErrNotRunning]: its child died or its start
// failed, and its bridge is bringing it back) still has its change filed, so
// its tools stay listed and refuse until it is back; filed here, in spec
// order, so the registry renders the prompt as it would with the server up.
func file(ctx context.Context, reg *tools.Registry, server string, change mcp.Change, err error) {
	down := errors.Is(err, mcp.ErrNotRunning)
	if err != nil && !down {
		// Logged, not returned: one vendor's failing server costs its
		// tools, not the whole company's apply.
		log.ErrorContext(ctx, "mcp_server_failed", "server", server, "error", err,
			"detail", "this server's tools are absent; its group is missing from the "+
				"catalogue rather than the builtins shrinking")
		return
	}
	// REMOVED FIRST: a name that moved between servers is in both lists, and
	// registering first would file the new tool and then delete it. And ONLY
	// AN MCP TOOL: a server's tool that collided with a builtin was refused,
	// so the name here is the builtin's, and removing by name would drop it.
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

// seatSpecs is every per-role child this seat needs, keyed off
// [config.MCPServer.Grants] as the org projection is: a `shared: false` server
// gets a child for each agent seat its grant reaches, whatever `mcp_env`
// blocks the seat holds, and a server turned off gets none.
//
// A child with a value (the seat's block or the template's, under a key not
// declared optional) whose variable resolves to nothing gets no spec but an
// outcome carrying a [credentialMissing], judged and logged by
// [Engine.withheldForCredentials] (package doc, "Tool server credentials"); so
// does a template that resolves to no launchable spec. Either way the status
// record still reports the server.
func seatSpecs(
	c *Company, seat *org.Role, env *config.Resolver, mint *seatMint,
) ([]mcp.Spec, []mcpOutcome) {
	var out []mcp.Spec
	var unconfigured []mcpOutcome
	for i := range c.Config.MCPServers {
		server := &c.Config.MCPServers[i]
		// The epoch runs the shared ones; an ungranted template is not this
		// seat's, and one turned off is granted to nobody.
		if server.IsShared() || !server.Grants(seat) {
			continue
		}
		instance := mcp.InstanceName(server.Name, seat.Name)
		values, missing := resolveServer(server, seat.Handle(), instance,
			seat.MCPEnv[server.Name], env, mint)
		if missing != nil {
			unconfigured = append(unconfigured, mcpOutcome{server: server.Name, err: missing})
			continue
		}
		spec, err := serverSpec(server, instance, values)
		if err != nil {
			log.Error("mcp_server_unconfigured", "server", server.Name, "seat", seat.Handle(),
				"error", err, "detail", "this seat gets no child for it")
			unconfigured = append(unconfigured, mcpOutcome{server: server.Name, err: err})
			continue
		}
		if k, signed := server.SignedBinding(); signed {
			signer, missing := seatSigner(c, seat, k, env, mint)
			if missing != nil {
				missing.server, missing.instance = server.Name, instance
				unconfigured = append(unconfigured, mcpOutcome{server: server.Name, err: missing})
				continue
			}
			if signer == nil {
				err := fmt.Errorf("%s signs with the %s integration's credential, which this build cannot sign with",
					server.Name, k.Kind)
				log.Error("mcp_server_unconfigured", "server", server.Name, "seat", seat.Handle(),
					"error", err, "detail", "this seat gets no child for it")
				unconfigured = append(unconfigured, mcpOutcome{server: server.Name, err: err})
				continue
			}
			spec.Signer = signer
		}
		out = append(out, spec)
	}
	return out, unconfigured
}

// seatSigner signs a seat's requests to a server bound by a signed credential
// ([config.MCPServer.SignedBinding]): the seat's issued value, resolved by the
// seat's own resolution, as the integration signs with it. A value not issued
// yet is a missing credential the pass judges like any other; nil, nil is an
// integration this build signs for none of.
func seatSigner(
	c *Company, seat *org.Role, k config.BoundSeatKey, env *config.Resolver, mint *seatMint,
) (mcp.RequestSigner, *credentialMissing) {
	if k.Kind != string(integration.KindAWS) || c.Config.Integrations.AWS == nil ||
		c.Config.Integrations.AWS.Provisioning == nil {
		return nil, nil
	}
	name := awsint.SeatVariable(seat.Handle())
	session := env.Value("${" + name + "}")
	if session == "" {
		return nil, credentialGap(seat.Handle(), []string{name}, env, mint)
	}
	signer, err := awsint.NewMCPSigner(session, c.Config.Integrations.AWS.Provisioning.HomeRegion)
	if err != nil {
		return nil, credentialGap(seat.Handle(), []string{name}, env, mint)
	}
	return signer, nil
}

// withheldForCredentials judges every server a pass held back for its
// credentials (a seat's children, or an apply's shared servers) against what
// the minting integrations last recorded, logs each, and returns the outcomes
// judged.
//
// NAMES ONLY: an operator acts on a variable, and its value is the credential.
// WAITING IS INFO, once per pass: a pass runs at the claim, at each apply and
// when a seal refreshes this node's snapshot, and a held child arms no retry,
// so this logs only as often as something could have changed. A minting
// integration that cannot say is a WARNING; the rest is an ERROR: the
// operator's secret is missing, or the minting integration waits on a person.
func (e *Engine) withheldForCredentials(ctx context.Context, outcomes []mcpOutcome) []mcpOutcome {
	var missing []*credentialMissing
	for _, o := range outcomes {
		var m *credentialMissing
		if errors.As(o.err, &m) {
			missing = append(missing, m)
		}
	}
	if len(missing) == 0 {
		return outcomes
	}
	states := e.mintingStates(ctx, missing)
	for i, o := range outcomes {
		var m *credentialMissing
		if !errors.As(o.err, &m) {
			continue
		}
		m = m.judged(states)
		outcomes[i].err = m
		attrs := []any{"server", m.server}
		if m.seat != "" {
			attrs = append(attrs, "seat", m.seat)
		}
		attrs = append(attrs, m.logAttrs()...)
		attrs = append(attrs, "detail", m.Error())
		switch {
		case m.waiting():
			log.InfoContext(ctx, "mcp_server_awaiting_credential", attrs...)
		case m.reading():
			log.InfoContext(ctx, "mcp_server_credential_unsure", attrs...)
		case m.unsureAlone():
			log.WarnContext(ctx, "mcp_server_credential_unsure", attrs...)
		default:
			log.ErrorContext(ctx, "mcp_server_credential_missing", attrs...)
		}
	}
	return outcomes
}

// serverSpec turns one config entry into a launchable spec, from the values
// [resolveServer] resolved for this start of it.
//
// TAKEN RESOLVED so ${VAR} references resolve at the edge, never inside
// internal/mcp, which must not read the secret store: a tool server's
// credentials keep one path and one audit point. They go where the transport
// authenticates: a stdio child's environment, an http connection's headers
// (which read no environment, [mcp.Spec.Headers]); a harness's box gets the
// same server from the same values ([sandboxMCP]). The resolved address is
// held to [config.MCPAddressProblem], since a ${VAR} in it is only seen here,
// so credentials never travel plain http to a public host; the error omits the
// address, which can carry a secret once resolved.
func serverSpec(server *config.MCPServer, instance string, v serverValues) (mcp.Spec, error) {
	if instance == "" {
		return mcp.Spec{}, fmt.Errorf("engine: mcp server %q has no instance name", server.Name)
	}
	if server.Kind() == config.TransportHTTP {
		if problem := config.MCPAddressProblem(v.url); problem != "" {
			return mcp.Spec{}, fmt.Errorf("engine: mcp server %q is not dialled: its url %s",
				server.Name, problem)
		}
	}
	spec := mcp.Spec{
		Name:                instance,
		Transport:           mcp.TransportKind(server.Transport),
		Command:             server.Command,
		Args:                server.Args,
		URL:                 v.url,
		ToolPrefix:          server.ToolPrefix,
		Levels:              toolLevels(server.Tools),
		AnnotationOverrides: annotationOverrides(server.ToolAnnotations),
		StartupTimeout:      mcpTimeout(server.StartupTimeoutSeconds),
		RequestTimeout:      mcpTimeout(server.RequestTimeoutSeconds),
	}
	if server.Kind() == config.TransportHTTP {
		spec.Headers = v.values
	} else {
		spec.Env = v.values
	}
	return spec, nil
}

// withheldByInstance indexes a pass's outcomes held back for their
// credentials by the instance each would have started as.
func withheldByInstance(outcomes []mcpOutcome) map[string]*credentialMissing {
	withheld := make(map[string]*credentialMissing, len(outcomes))
	for _, o := range outcomes {
		var missing *credentialMissing
		if errors.As(o.err, &missing) {
			withheld[missing.instance] = missing
		}
	}
	return withheld
}

// toolLevels converts the entry's tool levels ([config.MCPToolPolicy]) into
// what the MCP client files the server's tools by, on every instance: a
// blocked tool is never filed, one at approve asks a person before each call.
//
// AN UNSET DEFAULT STAYS UNSET: an unnamed tool starts at the level its hints
// give, which only the server's listing says, so it is decided where the tools
// are filed ([mcp.Levels.Of]).
func toolLevels(p config.MCPToolPolicy) mcp.Levels {
	out := mcp.Levels{Default: mcp.Level(p.Default)}
	if len(p.Levels) > 0 {
		out.Tools = make(map[string]mcp.Level, len(p.Levels))
		for tool, level := range p.Levels {
			out.Tools[tool] = mcp.Level(level)
		}
	}
	return out
}

// annotationOverrides converts the operator's per-tool hints, tri-state on
// both sides: an UNSET toggle must arrive as Unknown, not No, because "the
// operator said nothing" and "said not read-only" send the sub-agent guard
// opposite ways.
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
// NOT engine.seconds: a literal zero must pass through, because zero is how a
// Spec asks for the package's default rather than no timeout.
func mcpTimeout(v float64) time.Duration {
	if v <= 0 {
		return 0
	}
	return time.Duration(v * float64(time.Second))
}
