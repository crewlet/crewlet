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
	"time"

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
//
// Every server it runs is SUPERVISED from the moment it starts until it is
// stopped: a session that ends under the bridge is noticed and the server
// restarted, its tools refusing honestly meanwhile — see supervise.go, and
// [Supervision] for what the owner decides about it.
type Bridge struct {
	log *slog.Logger
	sup Supervision

	mu       sync.RWMutex
	servers  map[string]*server  // every server supervised: serving, or down and being restarted
	starting map[string]struct{} // names reserved by an in-flight Add
	tools    map[string]*Tool    // catalogue name -> the tool that won it
}

// NewBridge returns an empty bridge whose servers are supervised as sup says.
// A nil logger takes the package default.
//
// The supervision is an argument rather than an option with a default
// because one of its decisions has no safe default: whether this process may
// start a server on its own at all ([Supervision.MayStart]). A bridge for one
// seat's children that silently took "always" would restart a seat's identity
// on a node whose lease had lapsed.
func NewBridge(log *slog.Logger, sup Supervision) *Bridge {
	if log == nil {
		log = logging.Get("mcp.bridge")
	}
	return &Bridge{
		log:      log,
		sup:      sup,
		servers:  map[string]*server{},
		starting: map[string]struct{}{},
		tools:    map[string]*Tool{},
	}
}

// Add starts a server and discovers its tools.
//
// REGISTRATION HAPPENS ONLY ON SUCCESS. A server whose discovery fails is
// stopped and leaves nothing behind — not a tool, not a process, and not a
// "yes" from Has. Recording it first is what made a live subprocess with no
// tools answer Has("jira") with yes: a config edit read that as a healthy
// server, so the engine's own retry never fired and the child sat there until
// shutdown. On a bridge that retries failed starts
// ([Supervision.RetryFailedStarts]) the SPEC is kept, so the supervisor can
// try again — a name the bridge supervises without serving it, with no
// process and no tools.
//
// A name that is already supervised, or already being started, is refused
// rather than replaced — see ErrServerExists.
func (b *Bridge) Add(ctx context.Context, spec Spec) (Change, error) {
	if err := spec.validate(); err != nil {
		return Change{}, err
	}
	if err := b.reserve(spec.Name); err != nil {
		return Change{}, err
	}
	defer b.release(spec.Name)

	c, defs, err := b.launch(ctx, spec)
	if err != nil {
		// Not when the caller gave up: a start abandoned by its caller says
		// nothing about the server, and a caller that is going away has no
		// later apply to answer to.
		if b.sup.RetryFailedStarts && ctx.Err() == nil {
			b.retain(ctx, spec, err)
		}
		return Change{}, err
	}

	// Installing without re-checking the name is safe because Add — with
	// retain, which runs inside it — is the only writer of b.servers (Stop
	// and StopAll only delete), and the reservation
	// taken above is held across this whole function — so no second Add can
	// have installed this name in the meantime. A defensive re-check here
	// would be a branch nothing can reach and no test can reach either, which
	// is worse than the invariant written down.
	supervised, stop := supervision(ctx)
	s := b.newServer(spec, stop)
	b.mu.Lock()
	before := maps.Clone(b.tools)
	tools, refused := s.toolsFor(defs)
	s.tools, s.refused = tools, refused
	// RECORDED, NOT JUDGED. An override keyed on a name this instance does
	// not list changes nothing here, and this is the one place that holds
	// both the keys and the listing — but one instance's listing is not the
	// server's. A server may serve each instance a different set from the
	// same entry: mcp-atlassian lists no Confluence tool to a seat holding
	// only a Jira credential, so the entry's confluence_* key is right for
	// one seat and absent from another's. A warning here would fire on every
	// such start of a correct configuration. What is wrong is a key NO instance
	// lists, and only the heartbeat's per-server row (coord.MCPServerStatus)
	// and the fleet's status answer built from it see every instance.
	unlisted := unlistedOverrides(spec, defs)
	s.unlisted = unlisted
	s.up(c)
	b.servers[spec.Name] = s
	change := b.reindexLocked(before)
	b.mu.Unlock()
	go s.supervise(supervised)

	attrs := []any{"server", spec.Name, "tool_count", len(tools), "tool_names", toolNames(tools)}
	if len(unlisted) > 0 {
		attrs = append(attrs, "annotation_keys_unlisted", unlisted)
	}
	if len(refused) > 0 {
		attrs = append(attrs, "tools_refused", len(refused))
	}
	b.log.Info("tools_discovered", attrs...)
	return change, nil
}

// launch starts a server and lists its tools: one attempt, whoever makes it.
//
// A session that is up but serves nothing it can list is stopped here. That
// is the difference between a failed start and an orphaned child.
func (b *Bridge) launch(ctx context.Context, spec Spec) (*client, []toolDef, error) {
	c, err := connect(ctx, spec, b.log)
	if err != nil {
		return nil, nil, err
	}
	defs, err := c.listTools(ctx)
	if err != nil {
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
		defer cancel()
		if stopErr := c.stop(stopCtx); stopErr != nil {
			b.log.Warn("server_stop_after_failed_discovery",
				"server", spec.Name, "error", stopErr.Error())
		}
		return nil, nil, err
	}
	return c, defs, nil
}

// retain keeps a server whose start failed, down, for its supervisor to try
// again. The caller holds the name's reservation, so nothing can have
// installed it meanwhile.
//
// The FIRST retry waits [RestartBase]: the failed start was the first attempt
// of its series, and asking again at once would ask a vendor that has just
// refused.
func (b *Bridge) retain(ctx context.Context, spec Spec, cause error) {
	supervised, stop := supervision(ctx)
	s := b.newServer(spec, stop)
	now := time.Now()
	s.down = &outage{cause: "would not start: " + cause.Error(), class: ClassOf(cause), since: now}
	s.failure = &Failure{Class: s.down.class, Said: s.down.cause, At: now}
	s.attempt = 1
	b.mu.Lock()
	b.servers[spec.Name] = s
	b.mu.Unlock()
	go s.supervise(supervised)
	b.log.Info("server_start_retry_scheduled", "server", spec.Name,
		"error", cause.Error(), "first_retry_after_seconds", b.sup.base().Seconds(),
		"detail", "the bridge tries it again on the restart schedule, each wait spread "+
			"by a fifth, until it starts or a revision stops declaring it")
}

// revived installs a session the supervisor started for s, unless s was
// stopped meanwhile, and reports what the catalogue must change. Under the
// bridge's lock, which is what orders it against Stop: a server Stop has
// taken out of the index is never handed a session again.
func (b *Bridge) revived(s *server, c *client, defs []toolDef) (Change, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.servers[s.spec.Name] != s {
		return Change{}, false
	}
	before := maps.Clone(b.tools)
	s.tools, s.refused = s.toolsFor(defs)
	s.unlisted = unlistedOverrides(s.spec, defs)
	s.up(c)
	return b.reindexLocked(before), true
}

// Stop shuts one server down and reports what left the catalogue.
//
// The Change is populated even when the stop itself errors: the server is
// gone from this bridge either way, and a caller that skipped unregistering
// because the stop reported a problem would leave tools pointing at a client
// nothing can reach.
//
// It also ends the server's supervision, and waits for it: once Stop returns
// nothing of this server is running and nothing will start one — not a
// restart that was due, not one already in flight.
func (b *Bridge) Stop(ctx context.Context, name string) (Change, error) {
	b.mu.Lock()
	s, ok := b.servers[name]
	if !ok {
		b.mu.Unlock()
		return Change{}, fmt.Errorf("%w: %s", ErrNoSuchServer, name)
	}
	before := maps.Clone(b.tools)
	delete(b.servers, name)
	change := b.reindexLocked(before)
	b.mu.Unlock()

	if err := s.shutdown(ctx); err != nil {
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
	entries := make([]*server, 0, len(b.servers))
	for _, s := range b.servers {
		entries = append(entries, s)
	}
	before := maps.Clone(b.tools)
	b.servers = map[string]*server{}
	change := b.reindexLocked(before)
	b.mu.Unlock()

	errs := make([]error, len(entries))
	var wg sync.WaitGroup
	for i, s := range entries {
		wg.Go(func() {
			if err := s.shutdown(ctx); err != nil {
				b.log.Error("server_stop_failed", "server", s.spec.Name, "error", err.Error())
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
//   - not supervised        -> Add
//   - supervised, same spec -> the tools it contributes, no restart
//   - supervised, new spec  -> Restart, so a config edit actually takes effect
//
// The third is the other half of the same bug: without it an operator could
// change a shared server's command, args or environment and the apply would
// report success while the old child kept serving.
//
// A server with the same spec that is DOWN — its child died, or its start
// failed on a bridge that retries — is not restarted here and not left to
// its schedule either: its supervisor is asked to try now ([Bridge.RetryNow]),
// because whoever reconciles has a reason to believe now is different. The
// call does not wait for that attempt. A start can take a whole startup
// timeout, and an apply that waited on one for every server already being
// brought back would hold the epoch up behind it. It answers [ErrNotRunning],
// saying why, BESIDE the change a serving server would have answered: the
// tools such a server contributes stay in this bridge's catalogue, refusing
// until it is back, and a fresh registry that left them out would list them in
// a different order from the same revision applied while the server was up —
// or not at all. The caller files the change and reports the error.
func (b *Bridge) Reconcile(ctx context.Context, spec Spec) (Change, error) {
	if err := spec.validate(); err != nil {
		return Change{}, err
	}
	b.mu.RLock()
	s, supervised := b.servers[spec.Name]
	var same bool
	if supervised {
		same = s.spec.equal(spec)
	}
	b.mu.RUnlock()

	switch {
	case !supervised:
		return b.Add(ctx, spec)
	case same:
		// Added, not Removed: this is a fresh registry being told what
		// already exists, not the catalogue changing. Anything this
		// server contributes that another server currently shadows is
		// left out, so the registry ends up with the same surface the
		// bridge is actually serving.
		change := Change{Added: b.liveToolsOf(spec.Name)}
		if h := s.health(0); !h.Serving {
			s.nudge()
			return change, fmt.Errorf("%w: %s: %s", ErrNotRunning, spec.Name, h.Down)
		}
		return change, nil
	default:
		return b.Restart(ctx, spec)
	}
}

// Runs reports whether the bridge runs a server from exactly this spec:
// supervises one under its name, from a spec equal to it in every field.
//
// The question a caller asks BEFORE Reconcile, when what it needs to know is
// whether reconciling would touch a process at all — to leave a healthy
// child out of a pass that restarts its neighbours, and to tell the operator
// which servers it actually started rather than claiming all of them. It is
// the same comparison Reconcile makes, so a spec this answers true for is one
// Reconcile leaves alone, and the values compared are the RESOLVED ones: a
// rotated credential, the config entry byte-identical, answers false.
//
// A server that is DOWN answers true: its child died and its supervisor is
// bringing it back, from this spec, on its own schedule. A caller that read it
// as a server to start would be a second retry loop over one child, racing
// the supervisor's; one that has a reason to want it sooner asks
// [Bridge.RetryNow]. A server still starting is not in the bridge yet, and on
// a bridge that leaves failed starts to its owner nor is a start that failed,
// so both answer false — which is what lets that owner retry them through the
// same call.
func (b *Bridge) Runs(spec Spec) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	s, ok := b.servers[spec.Name]
	return ok && s.spec.equal(spec)
}

// RetryNow asks the supervisor of a server that is down for an attempt now,
// starting its schedule again, and reports whether the server was down. A
// server that is serving, or that the bridge does not supervise, is left
// alone.
//
// For an owner that has a reason to believe now is different — an apply, the
// gesture an operator makes once a vendor is back — and would otherwise leave
// the server to a wait of up to six minutes. It does not wait for the
// attempt, and it does not lift [Supervision.MayStart]: an attempt the owner
// may not make now is still held back.
func (b *Bridge) RetryNow(name string) bool {
	b.mu.RLock()
	s, ok := b.servers[name]
	b.mu.RUnlock()
	if !ok || s.health(0).Serving {
		return false
	}
	s.nudge()
	return true
}

// RestartBefore restarts one server whose current state began before the
// instant before, and reports what it did ([RestartOutcome]): a serving
// server's session is ended and a fresh one started at once, a down server's
// supervisor is asked for an attempt now, and either way its restart schedule
// starts again. A server that came up, or went down, at or after before is
// left alone, as is a name the bridge does not supervise.
//
// AN INSTANT, NOT A COMMAND, so a request is idempotent and order-free: a
// caller that hands the same instant twice, or late, restarts exactly what it
// would have restarted on time, and nothing that started since, which is
// what lets the fleet's record of a request (internal/coord's MCPRestart) stand for
// ever without restarting a server again on every node that reads it.
//
// cause is what the end is told as, in the server's down sentence while it
// comes back. It does not wait for the restart, and it does not lift
// [Supervision.MayStart]: an attempt the owner may not make now is still held
// back.
func (b *Bridge) RestartBefore(name string, before time.Time, cause string) RestartOutcome {
	b.mu.RLock()
	s, ok := b.servers[name]
	b.mu.RUnlock()
	if !ok {
		return RestartAbsent
	}
	return s.restartBefore(before, cause)
}

// liveToolsOf is one server's contribution to the LIVE catalogue — its tools
// minus any that another server currently wins the name for.
func (b *Bridge) liveToolsOf(name string) []*Tool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	s, ok := b.servers[name]
	if !ok {
		return nil
	}
	out := make([]*Tool, 0, len(s.tools))
	for _, t := range s.tools {
		if b.tools[t.Name()] == t {
			out = append(out, t)
		}
	}
	return out
}

// Has reports whether the bridge is serving a server by this name.
//
// A server that is still starting is NOT one the bridge has, and nor is one
// that is down — a child that died and is being restarted, a start being
// retried. That is the whole point of the distinction: "yes" here is what a
// live config edit reads as healthy, so it must mean tools are being served,
// not that a process exists or that a name is remembered.
func (b *Bridge) Has(name string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	s, ok := b.servers[name]
	return ok && s.health(0).Serving
}

// Health reports one server's condition, and false for a name the bridge does
// not supervise.
//
// It is what a status report should read rather than what a start concluded:
// a server that started can die, and one whose start failed can come back,
// long after the start that recorded otherwise.
func (b *Bridge) Health(name string) (Health, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	s, ok := b.servers[name]
	if !ok {
		return Health{}, false
	}
	return s.health(len(s.tools)), true
}

// Servers lists every instance the bridge supervises, sorted: serving, or
// down and being brought back. A name here is one Stop must be asked to end —
// which is why a server that is down is listed — and not a promise that it
// is answering; [Bridge.Health] says that.
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
	s, ok := b.servers[name]
	if !ok {
		return nil
	}
	return slices.Clone(s.tools)
}

// UnlistedOverrides is every tool_annotations key of the running server
// instance name that names no tool its listing carries — by the server's own
// name or with the entry's tool_prefix — sorted, or nil when every key names
// one, the instance has never listed (a start the bridge is still retrying),
// or the bridge does not hold it. A server that is down keeps what its last
// listing left out until a restart lists again; whether it is serving is
// [Bridge.Health]'s.
//
// A FACT ABOUT ONE INSTANCE, never a verdict on the key. Such a key overrides
// nothing on this instance, which is harmless where the server simply does
// not serve that tool here (a product this seat holds no credential for, a
// read-only switch) and a typo where no instance serves it — and only a view
// of every instance can tell the two apart, which is why the bridge records
// it and the per-server rows built from every instance judge it.
func (b *Bridge) UnlistedOverrides(name string) []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	entry, ok := b.servers[name]
	if !ok {
		return nil
	}
	return slices.Clone(entry.unlisted)
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
// in this path to show a failure message to. A server that is down is
// [ErrNotRunning], with what took it down.
func (b *Bridge) Call(ctx context.Context, instance, tool string, args map[string]any) ([]Block, error) {
	b.mu.RLock()
	s, ok := b.servers[instance]
	b.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNoSuchServer, instance)
	}
	// A session already known to be over is not sent the call, as
	// [Tool.Call] does not send it: the answer is the server's state, not
	// the pipe error a send into a dead session would come back with.
	c := s.live()
	if c == nil || c.gone() {
		return nil, fmt.Errorf("%w: %s: %s", ErrNotRunning, instance, s.health(0).Down)
	}
	blocks, err := c.callTool(ctx, tool, args)
	if err == nil {
		s.noteAnswer()
	}
	return blocks, err
}

// StderrTail returns a stdio server's last words, for diagnostics — the
// session serving it now, or the last one, for a server that is down. Empty
// for an HTTP server, one that never started, or an unknown name.
func (b *Bridge) StderrTail(name string) []string {
	b.mu.RLock()
	s, ok := b.servers[name]
	b.mu.RUnlock()
	if !ok {
		return nil
	}
	if c := s.lastClient(); c != nil {
		return c.stderrTail()
	}
	return nil
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
					"serving", incumbent.Instance(), "shadowed", t.Instance(),
					"detail", ShadowSentence(t.Name(), ServerName(incumbent.Instance())))
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

// unlistedOverrides is every AnnotationOverrides key, sorted, that names no
// tool in the listing under either spelling newTool accepts: the server's own
// name or the prefixed one. An excluded tool still counts as listed — the
// server serves it, and the key is spelled right.
func unlistedOverrides(spec Spec, defs []toolDef) []string {
	listed := make(map[string]struct{}, 2*len(defs))
	for _, def := range defs {
		listed[def.Name] = struct{}{}
		listed[spec.ToolPrefix+def.Name] = struct{}{}
	}
	var out []string
	for key := range spec.AnnotationOverrides {
		if _, ok := listed[key]; !ok {
			out = append(out, key)
		}
	}
	slices.Sort(out)
	return out
}

func toolNames(tools []*Tool) []string {
	out := make([]string, len(tools))
	for i, t := range tools {
		out[i] = t.Name()
	}
	return out
}
