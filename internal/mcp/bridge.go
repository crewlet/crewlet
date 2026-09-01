package mcp

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"

	"github.com/crewlet/crewlet/internal/logging"
)

// ErrServerExists is returned by Add for a name the bridge already runs.
//
// Refusing rather than replacing is the fix for a leak: indexing the new
// client over the old one leaves a second Add of the same name with the first
// subprocess still running, unreachable and unstoppable, for the life of the
// engine. Replacing a server is Restart, which stops the incumbent
// first and says so in its name.
var ErrServerExists = errors.New("mcp: server already added")

// ErrNoSuchServer is returned when a name the bridge does not run is stopped
// or inspected as though it did.
var ErrNoSuchServer = errors.New("mcp: no such server")

// Change is what one bridge operation did to the CATALOGUE — not to the
// process list.
//
// It exists because the shared tool registry lives outside this package and
// has no way to work out on its own what a stop or a restart took away. The
// names have to be captured BEFORE anything stops, because stopping drops the
// bridge's own index and anything asking afterwards gets an empty answer. That
// ordering used to be the caller's job to remember, in two places, and the one
// that forgot left a removed server's tools in every later turn's catalogue —
// dispatching into a stopped client for ever, as a soft failure the model
// burned rounds retrying with nothing in the logs to explain it.
//
// So the bridge computes the diff itself and hands it back. A caller cannot
// ask too late and cannot forget to ask.
type Change struct {
	// Added are tools that must now be registered. It is not simply "the new
	// server's tools": a tool of ANOTHER server that had been shadowed by a
	// name collision and is now the live one appears here too, because the
	// registry is keyed by name and needs to be told which object won.
	Added []*Tool

	// Removed are catalogue names that must be unregistered. Advertising one
	// of these offers the model an entry that can only fail.
	Removed []string
}

// Registrations pairs each added tool with its origin, so a registry cannot
// take one without the other. See origin.go.
func (c Change) Registrations() []Registration {
	out := make([]Registration, len(c.Added))
	for i, t := range c.Added {
		out[i] = Registration{Tool: t, Origin: t.Origin()}
	}
	return out
}

// Bridge runs MCP servers and keeps the catalogue they contribute.
//
// One bridge per engine. It is safe for concurrent use: seats acquire and
// release while live config edits add and remove servers, and those are
// different goroutines by construction.
type Bridge struct {
	log *slog.Logger

	mu       sync.RWMutex
	servers  map[string]*serverEntry
	starting map[string]struct{} // names reserved by an in-flight Add
	tools    map[string]*Tool    // catalogue name -> the tool that won it
}

type serverEntry struct {
	spec   Spec
	client *client
	tools  []*Tool
}

// NewBridge returns an empty bridge. A nil logger takes the package default.
func NewBridge(log *slog.Logger) *Bridge {
	if log == nil {
		log = logging.Get("mcp.bridge")
	}
	return &Bridge{
		log:      log,
		servers:  map[string]*serverEntry{},
		starting: map[string]struct{}{},
		tools:    map[string]*Tool{},
	}
}

// Add starts a server and discovers its tools.
//
// REGISTRATION HAPPENS ONLY ON SUCCESS. A server whose discovery fails is
// stopped and leaves nothing behind — not an index entry, not a tool, not a
// process. Recording it first is what made a live subprocess with no tools
// answer Has("jira") with yes: a config edit read that as a healthy server, so
// the engine's own retry never fired and the child sat there until shutdown.
//
// A name that is already running, or already being started, is refused rather
// than replaced — see ErrServerExists.
func (b *Bridge) Add(ctx context.Context, spec Spec) (Change, error) {
	if err := spec.validate(); err != nil {
		return Change{}, err
	}
	if err := b.reserve(spec.Name); err != nil {
		return Change{}, err
	}
	defer b.release(spec.Name)

	c, err := connect(ctx, spec, b.log)
	if err != nil {
		return Change{}, err
	}
	defs, err := c.listTools(ctx)
	if err != nil {
		// The session is up but serves nothing we can see. Stopping it here
		// is the difference between a failed start and an orphaned child.
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
		defer cancel()
		if stopErr := c.stop(stopCtx); stopErr != nil {
			b.log.Warn("server_stop_after_failed_discovery",
				"server", spec.Name, "error", stopErr.Error())
		}
		return Change{}, err
	}

	tools := make([]*Tool, 0, len(defs))
	excluded := make(map[string]struct{}, len(spec.ExcludeTools))
	for _, name := range spec.ExcludeTools {
		excluded[name] = struct{}{}
	}
	for _, def := range defs {
		// Excludes match the SERVER's name, before the prefix: an operator
		// writing them has the server's listing in front of them.
		if _, skip := excluded[def.Name]; skip {
			continue
		}
		tools = append(tools, newTool(c, def, spec))
	}

	// Installing without re-checking the name is safe because Add is the only
	// writer of b.servers (Stop and StopAll only delete), and the reservation
	// taken above is held across this whole function — so no second Add can
	// have installed this name in the meantime. A defensive re-check here
	// would be a branch nothing can reach and no test can reach either, which
	// is worse than the invariant written down.
	b.mu.Lock()
	defer b.mu.Unlock()
	before := maps.Clone(b.tools)
	b.servers[spec.Name] = &serverEntry{spec: spec, client: c, tools: tools}
	change := b.reindexLocked(before)

	b.log.Info("tools_discovered", "server", spec.Name,
		"tool_count", len(tools), "tool_names", toolNames(tools))
	return change, nil
}

// Stop shuts one server down and reports what left the catalogue.
//
// The Change is populated even when the stop itself errors: the server is
// gone from this bridge either way, and a caller that skipped unregistering
// because the stop reported a problem would leave tools pointing at a client
// nothing can reach.
func (b *Bridge) Stop(ctx context.Context, name string) (Change, error) {
	b.mu.Lock()
	entry, ok := b.servers[name]
	if !ok {
		b.mu.Unlock()
		return Change{}, fmt.Errorf("%w: %s", ErrNoSuchServer, name)
	}
	before := maps.Clone(b.tools)
	delete(b.servers, name)
	change := b.reindexLocked(before)
	b.mu.Unlock()

	if err := entry.client.stop(ctx); err != nil {
		b.log.Error("server_stop_failed", "server", name, "error", err.Error())
		return change, err
	}
	return change, nil
}

// StopAll stops every server CONCURRENTLY.
//
// Sequentially, one server that would not die consumed the whole budget the
// engine allows this step and every server after it was never stopped at all —
// their subprocesses outlived the engine. They are independent processes, so
// stopping them together turns "the first slow one strands the rest" into "the
// slowest one bounds the step", which is what the caller's timeout is for.
//
// The index is dropped before the stops run, so a bridge that is restarted
// after a failed stop does not believe those servers are still live.
func (b *Bridge) StopAll(ctx context.Context) (Change, error) {
	b.mu.Lock()
	entries := make([]*serverEntry, 0, len(b.servers))
	for _, e := range b.servers {
		entries = append(entries, e)
	}
	before := maps.Clone(b.tools)
	b.servers = map[string]*serverEntry{}
	change := b.reindexLocked(before)
	b.mu.Unlock()

	errs := make([]error, len(entries))
	var wg sync.WaitGroup
	for i, e := range entries {
		wg.Go(func() {
			if err := e.client.stop(ctx); err != nil {
				b.log.Error("server_stop_failed", "server", e.spec.Name, "error", err.Error())
				errs[i] = err
			}
		})
	}
	wg.Wait()

	b.log.Info("all_servers_stopped", "servers", len(entries))
	return change, errors.Join(errs...)
}

// Restart stops a server and brings it back from spec, UNCONDITIONALLY.
//
// It does not consult Spec.SameProcess, because the caller that needs this
// most is credential rotation, where the config payload is byte-identical by
// construction and only the resolved ${VAR} values moved. A restart that
// compared the payload would do nothing there, and the children would go on
// running with the credential the operator had just revoked — while the
// rotation reported success.
//
// On failure the Change still names everything the stopped server used to
// contribute. The server is not coming back on this attempt, and advertising
// its tools would offer the model a catalogue entry that can only fail.
func (b *Bridge) Restart(ctx context.Context, spec Spec) (Change, error) {
	if err := spec.validate(); err != nil {
		return Change{}, err
	}
	stopChange, stopErr := b.Stop(ctx, spec.Name)
	if stopErr != nil && !errors.Is(stopErr, ErrNoSuchServer) {
		// Logged by Stop. Carry on: a server that would not shut down
		// cleanly still has to be replaced, and its catalogue entry is
		// already gone.
		b.log.Warn("server_restart_after_unclean_stop", "server", spec.Name)
	}

	addChange, err := b.Add(ctx, spec)
	if err != nil {
		return stopChange, err
	}
	return mergeChanges(stopChange, addChange), nil
}

// Reconcile brings one server to spec, whichever state it is already in, and
// reports what the catalogue must change.
//
// THIS IS THE VERB A CONFIG APPLY WANTS, and Add is not. An apply builds a
// FRESH tool registry for the new epoch and then equips it, so it has to be
// told this server's tools again even when nothing about the server changed —
// while the CHILD must keep running, because it is a process and restarting
// every one of them on every apply would tear down working servers to arrive
// back where it started.
//
// Add cannot do that. It refuses a name it already runs, and the caller that
// treated the refusal as a failed server got an epoch whose catalogue silently
// lacked every shared server's tools. Measured on the Nimbus example: the
// engine seeds its company at boot and immediately applies it, so the shared
// servers reached exactly one epoch — the one that was replaced a second
// later — and no seat ever saw them.
//
// Three cases, one call:
//
//   - not running        -> Add
//   - running, same spec -> the live tools it contributes, no restart
//   - running, new spec  -> Restart, so a config edit actually takes effect
//
// The third is the other half of the same bug: without it an operator could
// change a shared server's command, args or environment and the apply would
// report success while the old child kept serving.
func (b *Bridge) Reconcile(ctx context.Context, spec Spec) (Change, error) {
	if err := spec.validate(); err != nil {
		return Change{}, err
	}
	b.mu.RLock()
	entry, running := b.servers[spec.Name]
	var same bool
	if running {
		same = entry.spec.equal(spec)
	}
	b.mu.RUnlock()

	switch {
	case !running:
		return b.Add(ctx, spec)
	case same:
		// Added, not Removed: this is a fresh registry being told what
		// already exists, not the catalogue changing. Anything this
		// server contributes that another server currently shadows is
		// left out, so the registry ends up with the same surface the
		// bridge is actually serving.
		return Change{Added: b.liveToolsOf(spec.Name)}, nil
	default:
		return b.Restart(ctx, spec)
	}
}

// liveToolsOf is one server's contribution to the LIVE catalogue — its tools
// minus any that another server currently wins the name for.
func (b *Bridge) liveToolsOf(name string) []*Tool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	entry, ok := b.servers[name]
	if !ok {
		return nil
	}
	out := make([]*Tool, 0, len(entry.tools))
	for _, t := range entry.tools {
		if b.tools[t.Name()] == t {
			out = append(out, t)
		}
	}
	return out
}

// Has reports whether the bridge runs a server by this name.
//
// A server that is still starting is NOT one the bridge has. That is the whole
// point of the distinction: "yes" here is what a live config edit reads as
// healthy, so it must mean tools are being served, not that a process exists.
func (b *Bridge) Has(name string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	_, ok := b.servers[name]
	return ok
}

// Servers lists the running instance names, sorted.
func (b *Bridge) Servers() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := slices.Sorted(maps.Keys(b.servers))
	return out
}

// Tools returns the live catalogue, sorted by name.
func (b *Bridge) Tools() []*Tool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]*Tool, 0, len(b.tools))
	for _, t := range b.tools {
		out = append(out, t)
	}
	sortTools(out)
	return out
}

// Tool looks one up by catalogue name.
func (b *Bridge) Tool(name string) (*Tool, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	t, ok := b.tools[name]
	return t, ok
}

// ServerTools returns what one INSTANCE contributes, including any tool
// currently shadowed by a name collision — this answers "what does this server
// serve?", not "what is live under that name?".
func (b *Bridge) ServerTools(name string) []*Tool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	entry, ok := b.servers[name]
	if !ok {
		return nil
	}
	return slices.Clone(entry.tools)
}

// Entries projects the whole catalogue for the discovery meta-tools.
func (b *Bridge) Entries() []Entry { return EntriesOf(b.Tools()) }

// Registrations pairs every live tool with its origin.
func (b *Bridge) Registrations() []Registration {
	tools := b.Tools()
	out := make([]Registration, len(tools))
	for i, t := range tools {
		out[i] = Registration{Tool: t, Origin: t.Origin()}
	}
	return out
}

// Call runs one tool on a NAMED SERVER INSTANCE, by the server's OWN tool name.
//
// This is not the path a model takes — that is Tool.Call, through the
// catalogue, under the catalogue's name. This exists for engine code that has
// to ask one seat's server a question about that seat before any turn runs:
// resolving an agent's account id on its own per-role Jira instance, for
// instance. Such a caller has the instance name and the server's tool name and
// no catalogue entry to look up, and it deliberately bypasses tool_prefix and
// the exclusion list, neither of which is about what the ENGINE may ask.
//
// A tool the server refuses is an error here, not a Result: there is no model
// in this path to show a failure message to.
func (b *Bridge) Call(ctx context.Context, instance, tool string, args map[string]any) ([]Block, error) {
	b.mu.RLock()
	entry, ok := b.servers[instance]
	b.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNoSuchServer, instance)
	}
	return entry.client.callTool(ctx, tool, args)
}

// StderrTail returns a stdio server's last words, for diagnostics. Empty for
// an HTTP server or an unknown name.
func (b *Bridge) StderrTail(name string) []string {
	b.mu.RLock()
	entry, ok := b.servers[name]
	b.mu.RUnlock()
	if !ok {
		return nil
	}
	return entry.client.stderrTail()
}

func (b *Bridge) reserve(name string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.servers[name]; ok {
		return fmt.Errorf("%w: %s", ErrServerExists, name)
	}
	if _, ok := b.starting[name]; ok {
		return fmt.Errorf("%w: %s (start in flight)", ErrServerExists, name)
	}
	b.starting[name] = struct{}{}
	return nil
}

func (b *Bridge) release(name string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.starting, name)
}

// reindexLocked rebuilds the flat catalogue from the per-server lists and
// returns the diff against before.
//
// The per-server lists are the source of truth and the flat index is derived,
// which is what makes a NAME COLLISION survivable. Two servers exposing the
// same tool name with no tool_prefix used to overwrite each other in one flat
// map: stopping the winner deleted the name outright, and the loser's tool —
// still served, still live — never came back. Deriving the index means the
// loser simply wins the next rebuild, and the Change says so.
//
// The winner is the first server by name, so it does not depend on the order
// servers happened to be added.
func (b *Bridge) reindexLocked(before map[string]*Tool) Change {
	names := slices.Sorted(maps.Keys(b.servers))

	next := make(map[string]*Tool, len(b.tools))
	for _, server := range names {
		for _, t := range b.servers[server].tools {
			if incumbent, taken := next[t.Name()]; taken {
				b.log.Warn("tool_name_collision", "tool", t.Name(),
					"serving", incumbent.Instance(), "shadowed", t.Instance())
				continue
			}
			next[t.Name()] = t
		}
	}
	b.tools = next

	var change Change
	for name, t := range next {
		if before[name] != t {
			change.Added = append(change.Added, t)
		}
	}
	for name := range before {
		if _, still := next[name]; !still {
			change.Removed = append(change.Removed, name)
		}
	}
	sortTools(change.Added)
	slices.Sort(change.Removed)
	return change
}

// mergeChanges folds a stop's diff into the add that followed it, so a restart
// reports one net change rather than a removal the caller must then undo.
//
// A name that went away and came back stays in ADDED and drops out of removed.
// It has to: the name survived but the tool behind it did not — it is a
// different object on a different child — and a caller that read the survivor
// as unchanged would go on dispatching into the process that just died.
func mergeChanges(first, second Change) Change {
	added := map[string]*Tool{}
	for _, t := range first.Added {
		added[t.Name()] = t
	}
	for _, t := range second.Added {
		added[t.Name()] = t
	}
	removed := map[string]struct{}{}
	for _, name := range first.Removed {
		removed[name] = struct{}{}
	}
	for _, name := range second.Removed {
		removed[name] = struct{}{}
	}

	var out Change
	for _, t := range added {
		out.Added = append(out.Added, t)
	}
	for name := range removed {
		if _, back := added[name]; !back {
			out.Removed = append(out.Removed, name)
		}
	}
	sortTools(out.Added)
	slices.Sort(out.Removed)
	return out
}

func sortTools(tools []*Tool) {
	slices.SortFunc(tools, func(a, b *Tool) int { return cmp.Compare(a.Name(), b.Name()) })
}

func toolNames(tools []*Tool) []string {
	out := make([]string, len(tools))
	for i, t := range tools {
		out[i] = t.Name()
	}
	return out
}
