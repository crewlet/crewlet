package engine

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/backoff"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/tools"
)

// A held seat's per-role MCP children, kept converged on what the seat should
// be running for as long as this node holds it.
//
// The children belong to the seat's LEASE (see mcp.go): started when this node
// claims the seat, stopped when it lets the seat go. What this file adds is
// everything in between. A child is started from a SPEC — the template's
// command and environment with the seat's `mcp_env` laid over it, every
// ${VAR} resolved through this node's secret snapshot — and that spec moves
// while the seat is held:
//
//   - A credential the child is started from can be sealed AFTER the claim.
//     On a first boot under an integration that provisions seat identities,
//     the engine claims its seats before the provisioning pass has minted
//     their tokens, so each child is spawned with the credential empty, the
//     vendor's server refuses to start, and the seat receives messages it
//     has no tool to answer — until it next changes hands, which on a single
//     node is the next restart. The pass re-activates the revision once it
//     has sealed ([Engine.rebuildForSealedSecrets]), and the apply that
//     follows is where the child has to come back.
//   - A credential is ROTATED, by the documented gesture of re-activating an
//     unchanged revision. A child keeps the environment it was exec'd with,
//     so only a restart hands it the new value.
//   - A revision edits the template or the seat's `mcp_env`, adds a grant or
//     takes one away.
//   - A start fails for a reason that passes: a vendor endpoint not up yet,
//     a cold package fetch that timed out.
//
// So every apply RECONCILES each held seat's children against the spec the
// new epoch and the snapshot it just refreshed produce, exactly as the shared
// servers are reconciled into each epoch: a child whose resolved spec is
// unchanged is left running and never learns an apply happened, one whose
// spec moved is restarted, one that is not running is started, and one the
// seat no longer declares is stopped. And a pass that leaves a child failed
// for any reason but a spec that can never launch tries again on its own,
// with a doubling delay up to a ceiling, until the child starts or the seat
// is let go.
//
// # One pass at a time per seat, and the last one is current
//
// A pass is asked for from three places — the claim, every apply, the retry
// timer — on three goroutines, and two passes over one bridge at once would
// race a restart against a start of the same child. They are SINGLE-FLIGHT:
// each seat has at most one RUNNER, whoever finds a runner already there asks
// it to go AGAIN and returns, and the runner does not stop until it has run a
// pass nothing asked it to repeat. That is what closes the race the claim used
// to lose. A claim's pass is spawning children from the snapshot it read when
// the apply that seals their credential lands; the apply could not rebuild
// what was not yet there, and the claim finished with the child it had
// started from nothing. Now the apply's request makes the claim go again
// before it returns — before the seat's mailbox opens — against the snapshot
// the apply installed.
//
// THE CLAIM IS ITS SEAT'S FIRST RUNNER FROM BEFORE IT IS REGISTERED, and that
// is the half that makes the promise hold for every apply rather than for the
// ones that land mid-pass. Registering the seat is what lets an apply find
// it; an apply that found it before the claim's first pass — or while the
// claim was retiring a predecessor — and became its runner would leave the
// claim asking that runner to go again and returning at once, with the mailbox
// opening on children still being started.
//
// # The apply does not wait for a child
//
// An apply refiles every held seat's registry against the epoch it has just
// published, in its own stage, and hands each seat's child reconcile to that
// seat's runner WITHOUT WAITING FOR IT. A start is a subprocess handshake that
// may take a whole startup timeout (two minutes by default) — a vendor
// endpoint that is down, a package fetch that hangs — and everything the
// apply does after this stage, with the new epoch already current, would wait
// behind it: a new role's mailbox, which drops what is published to it until
// it exists, the mail held back for want of a model, the scheduler, the sweep
// that claims the seats the revision added, and every later activation the
// node is asked to apply. Waiting would not even buy the consistency it
// suggests, since a seat whose claim or retry was already running its passes
// was never waited for. A seat's own surface is refiled again at the end of
// each of its passes, so its first turn after that pass sees the children it
// left.
//
// # Never a child for a seat this node cannot prove it holds
//
// A child IS the seat: its credentials are the agent's identity at the
// vendor. So every pass starts nothing unless the seat host can still prove
// this node's lease ([seat.Host.Provable]). A claim's passes ask it of a
// lease taken moments before; the question is for the passes that come later
// — an apply's, a retry's — which run at a moment of their own choosing, and
// one that landed after the lease lapsed, before the heartbeat noticed, would
// otherwise start a second copy of an agent a peer already runs. A pass
// refused that way is retried like a failed start, and the release that
// follows a lost lease cancels it.
//
// # The release always wins
//
// Releasing a seat cancels the pass in flight — its starts end at once rather
// than at a startup timeout — waits for it to return, disarms the retry, and
// only then stops the bridge. A pass that finishes after its seat was let go
// installs nothing, and one asked for afterwards does nothing, so no child is
// ever started for a seat after the release that was meant to end them.

// seatServerRetryBase and seatServerRetryCeiling space the retries of a seat
// child that would not start: fifteen seconds after the first failed pass,
// twice as long after each one after it, up to five minutes. Those are the
// waits [seatServerRetryJitter] then spreads by a fifth either way, so the
// longest is six minutes, and each is what its waits AVERAGE — the reading
// [backoff.Jitter] gives the number its caller justified. Clamping the spread
// waits to the ceiling instead would put half of a fleet's seats back on the
// same instant at it, which is the lockstep the spread is there to break.
//
// THE BASE IS ONE SEAT HEARTBEAT (internal/seat's HeartbeatInterval), the
// cadence at which this node re-proves it holds the seat at all. A retry
// sooner than that would mostly re-run a start against the same vendor
// moments after it refused, forking a process each time; and a credential
// that is merely late needs no retry at all, since the apply that seals it
// runs a pass of its own.
//
// THE CEILING IS THE SEAT HOST'S UNDEAD ALARM INTERVAL (twenty heartbeats),
// the interval at which a seat's other persistent failure is said again. Each
// failed retry logs `mcp_server_failed` with the child's last words, so the
// ceiling is how often, on average, a child that will not come back repeats
// itself; and it bounds how long a vendor that has recovered waits to be
// noticed, at six minutes with the spread. A deployment has no honest reason
// to want either differently, so neither is a knob.
const (
	seatServerRetryBase    = 15 * time.Second
	seatServerRetryCeiling = 5 * time.Minute
)

// seatServerRetryJitter spreads each retry by a fifth either way, so the seats
// of a fleet that booted together, all refused by one vendor, do not retry it
// in lockstep for the life of the deployment.
const seatServerRetryJitter = 0.2

// seatChildren is one held seat's own bridge and the state that keeps it
// converged — see the file comment.
type seatChildren struct {
	handle string

	// bridge holds this seat's children and nothing else's: two children of
	// one template publish the same tool names, and the bridge keys its
	// catalogue by name across every server it runs (see mcp.go).
	bridge *mcp.Bridge

	// held reports whether this node can still prove it holds the seat.
	held func() bool

	// lease ends when the seat is released. It is the context a retry runs
	// under, which is why it is kept here: a retry is the one pass nobody
	// calls with a context of their own, and one that fired as the seat
	// was let go must find its context already over.
	lease  context.Context
	cancel context.CancelFunc

	// mu guards everything below; idle is signalled when a pass ends.
	mu   sync.Mutex
	idle sync.Cond

	// running is set while the seat has a RUNNER — from the moment one is
	// chosen, before its goroutine is even started, so a release that waits
	// for it to clear waits for that goroutine too — and again when a pass
	// was asked for that the runner has not yet run.
	running bool
	again   bool

	// endPass cancels the pass in flight, nil between passes. Set by the
	// runner under mu in the same step that checks released, so the release
	// either finds it to cancel or the runner finds the seat released and
	// starts nothing: a release never waits out a child's startup timeout.
	endPass context.CancelFunc

	// released is set by the release, after which nothing runs.
	released bool

	// failures counts the passes in a row that left a child to retry, which
	// is what the retry's delay doubles on. An apply resets it: it is the
	// event that changes what a child is started from, so the pass after it
	// is the first of a new series rather than the next of the old one.
	failures int

	// retry is the armed retry, nil when none is due.
	retry *time.Timer
}

// newSeatChildren builds the state for a seat, with no pass asked for and no
// runner.
//
// The lease context keeps the caller's values — its logger and trace — and
// drops its cancellation: a claim's context ends with the claim, and the
// children outlive it for as long as the seat is held.
func (e *Engine) newSeatChildren(ctx context.Context, handle string) *seatChildren {
	lease, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s := &seatChildren{
		handle: handle,
		bridge: mcp.NewBridge(nil),
		held:   func() bool { return e.seatProvable(handle) },
		lease:  lease,
		cancel: cancel,
	}
	s.idle.L = &s.mu
	return s
}

// seatProvable reports whether this node can prove it holds a seat's lease.
//
// An engine with no node places nothing — `crewlet validate`, or a case
// driving the seat hooks by hand — so nothing else can hold the seat either.
func (e *Engine) seatProvable(handle string) bool {
	if e.node == nil {
		return true
	}
	return e.node.Host().Provable(handle)
}

// startSeatServers spawns a claimed seat's own children and files the surface
// its turns run against, which [Engine.ToolsFor] then answers.
//
// It returns once the seat's children have converged — once a pass has run
// that nothing asked to repeat — because the claim's whole contract is that
// everything the first turn needs is up before the mailbox opens. An apply
// that lands while the children are starting, or before the first of them
// is, is folded into the same call rather than lost (see the file comment).
func (e *Engine) startSeatServers(ctx context.Context, handle string) {
	e.runSeatPasses(ctx, e.claimSeatChildren(ctx, handle))
}

// claimSeatChildren builds and registers the state for a seat this node has
// just claimed, retiring whatever this node still kept for it, and returns it
// with the CALLER AS ITS RUNNER: a pass already asked for, which the caller
// must run ([Engine.runSeatPasses]).
//
// REGISTERED BEFORE THE FIRST PASS, so an apply that lands during the claim
// finds the seat; a seat whose children were only recorded once they had
// started was invisible to the apply that sealed their credential. And
// OWNED BEFORE IT IS REGISTERED, so an apply that finds it can only ask the
// claim to go again — see the file comment. Set without the lock because
// nothing else can see the state yet.
func (e *Engine) claimSeatChildren(ctx context.Context, handle string) *seatChildren {
	s := e.newSeatChildren(ctx, handle)
	s.running, s.again = true, true
	e.mcpMu.Lock()
	if e.seatMCP == nil {
		e.seatMCP = make(map[string]*seatChildren)
	}
	previous := e.seatMCP[handle]
	e.seatMCP[handle] = s
	e.mcpMu.Unlock()
	if previous != nil {
		// A PREDECESSOR is not hypothetical: a failed acquire releases the
		// same seat, and a re-claim can arrive before that teardown ran.
		// Stopped BEFORE the new children start, so two copies of the
		// seat's identity never run on this node at once — and on a
		// context free of the claim's, because the thing being undone is
		// often the cancellation itself.
		e.retireSeatChildren(context.WithoutCancel(ctx), previous)
	}
	return s
}

// reconcileSeatServers brings every seat this node holds onto the epoch an
// apply has just published.
//
// TWO STEPS, and the apply waits for the first alone. Each seat's registry is
// a clone of the epoch's surface, so it is REFILED at once against the new
// one — its builtins and shared servers, with the seat's live catalogue on
// top — or a turn would run under the new revision with the previous one's
// builtins and knobs. That is cheap and touches no process. Then each seat's
// children are CONVERGED, by the seat's own runner and not by the apply (see
// the file comment): a child whose resolved spec moved — a revision's edit,
// or a credential this apply's snapshot sealed or rotated — is restarted, and
// every other child is left alone. A seat whose runner is already there (its
// claim, or a retry) is asked to go again; any other gets a runner of its
// own, one goroutine per seat because each start is a subprocess handshake
// that has nothing to do with another seat's.
//
// Each runner's goroutine belongs to the seat rather than to this call: the
// release cancels its pass and waits for it ([Engine.retireSeatChildren]),
// and so does the engine's stop. Its context is the apply's values without
// the apply's cancellation, for the same reason as the lease's: the apply is
// over long before a slow start is.
//
// Called AFTER the pointer moves, like the mailbox and scheduler steps and
// for the same reason: an apply refused later must not have rebuilt the seat
// surfaces of an epoch that never became current.
func (e *Engine) reconcileSeatServers(ctx context.Context) {
	e.mcpMu.Lock()
	held := make([]*seatChildren, 0, len(e.seatMCP))
	for _, s := range e.seatMCP {
		held = append(held, s)
	}
	if c := e.Company(); c != nil && c.Tools != nil && len(held) > 0 {
		if e.seatTools == nil {
			e.seatTools = make(map[string]*tools.Registry, len(held))
		}
		for _, s := range held {
			e.seatTools[s.handle] = seatSurface(ctx, c, s)
		}
	}
	e.mcpMu.Unlock()

	runner := context.WithoutCancel(ctx)
	for _, s := range held {
		if s.ask(true) {
			go e.runSeatPasses(runner, s)
		}
	}
}

// ask asks for a pass over the seat's children and reports whether the caller
// is now the seat's runner, which must run it ([Engine.runSeatPasses]). A seat
// that already has a runner is asked to go again instead, and a released seat
// runs nothing. fresh marks a pass an apply asked for, which starts a new
// retry series.
func (s *seatChildren) ask(fresh bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.released {
		return false
	}
	if fresh {
		s.failures = 0
	}
	s.again = true
	if s.running {
		return false
	}
	s.running = true
	if s.retry != nil {
		// Superseded: this pass is the attempt the retry was waiting for.
		s.retry.Stop()
		s.retry = nil
	}
	return true
}

// convergeSeat asks for a pass and, when the seat has no runner, runs it on
// the calling goroutine — the retry timer's call, which has a goroutine of
// its own to spend.
func (e *Engine) convergeSeat(ctx context.Context, s *seatChildren, fresh bool) {
	if s.ask(fresh) {
		e.runSeatPasses(ctx, s)
	}
}

// runSeatPasses runs passes over one seat's children until it has run one that
// nothing asked to repeat. The caller is the seat's runner: [seatChildren.ask]
// said so, or the claim built the seat that way ([Engine.claimSeatChildren]).
//
// What the last pass left decides what happens next: nothing, when every
// child the seat declares is running, and otherwise a retry, armed here and
// disarmed by the next pass or by the release.
func (e *Engine) runSeatPasses(ctx context.Context, s *seatChildren) {
	s.mu.Lock()
	retry := false
	for s.again && !s.released {
		s.again = false
		pass, end := context.WithCancel(ctx)
		s.endPass = end
		s.mu.Unlock()
		retry = e.seatPass(pass, s)
		end()
		s.mu.Lock()
		s.endPass = nil
	}
	s.running = false
	s.idle.Broadcast()
	if s.released {
		s.mu.Unlock()
		return
	}
	if !retry {
		s.failures = 0
		s.mu.Unlock()
		return
	}
	s.failures++
	delay := e.seatRetryDelay(s.failures)
	failures := s.failures
	// ON THE LEASE'S CONTEXT, the only one that lives as long as the retry
	// may wait and ends with the seat: a claim's ends with the claim, and an
	// apply's runner carries no cancellation at all.
	s.retry = time.AfterFunc(delay, func() { e.convergeSeat(s.lease, s, false) })
	s.mu.Unlock()
	log.InfoContext(ctx, "mcp_seat_retry_scheduled", "seat", s.handle,
		"attempt", failures, "delay_seconds", delay.Seconds(),
		"detail", "a child this seat declares is not running, or this node could "+
			"not prove it holds the seat to start one; the pass runs again then, "+
			"or at the next apply, whichever comes first")
}

// seatRetryDelay is how long the retry after the nth pass in a row that left a
// child to retry waits; failures counts from one.
func (e *Engine) seatRetryDelay(failures int) time.Duration {
	base := e.seatRetryBase
	if base <= 0 {
		base = seatServerRetryBase
	}
	return backoff.Jitter(backoff.Doubling(failures, base, seatServerRetryCeiling),
		seatServerRetryJitter)
}

// seatPass brings one seat's children to what the current epoch and this
// node's snapshot say it should run, files the seat's surface, and reports
// whether a child was left to retry.
//
// ctx is the pass's own, which the release cancels ([seatChildren.endPass]).
func (e *Engine) seatPass(ctx context.Context, s *seatChildren) bool {
	if !s.held() {
		log.WarnContext(ctx, "mcp_seat_unprovable", "seat", s.handle,
			"detail", "this node cannot prove it still holds the seat, so it "+
				"starts none of the seat's children: a peer may hold it now. "+
				"Retried, unless the seat is released first")
		return true
	}
	c := e.Company()
	if c == nil {
		return false
	}
	var specs []mcp.Spec
	var outcomes []mcpOutcome
	if seat := c.Org.AgentSeatByHandle(s.handle); seat != nil {
		// A SEAT THE EPOCH NO LONGER HAS declares nothing, so every child
		// goes: the revision that removed the role is current, the sweep
		// is about to release the seat, and a child left running until
		// then is an identity the company no longer has.
		specs, outcomes = seatSpecs(c, seat, e.resolver())
	}
	retireUndeclared(ctx, s, specs)
	started, retry := bringUp(ctx, s, specs)
	outcomes = append(outcomes, started...)

	e.mcpMu.Lock()
	defer e.mcpMu.Unlock()
	if e.seatMCP[s.handle] != s {
		// RELEASED, or replaced by a later claim, while this pass ran:
		// the surface and the outcomes are no longer this pass's to file.
		return false
	}
	if current := e.Company(); current != nil && current.Tools != nil {
		// THE EPOCH CURRENT NOW rather than the one the pass started
		// from: an apply that published one meanwhile has asked for
		// another pass, but until it runs, the builtins it serves must
		// already be the new revision's.
		if e.seatTools == nil {
			e.seatTools = make(map[string]*tools.Registry)
		}
		e.seatTools[s.handle] = seatSurface(ctx, current, s)
	}
	for i := range outcomes {
		outcomes[i].seat = s.handle
	}
	if e.mcpSeats == nil {
		e.mcpSeats = make(map[string][]mcpOutcome)
	}
	if len(outcomes) == 0 {
		delete(e.mcpSeats, s.handle)
	} else {
		e.mcpSeats[s.handle] = outcomes
	}
	return retry
}

// retireUndeclared stops every child of the seat that specs no longer names: a
// template the revision removed or renamed, a grant the seat lost, a server
// turned shared.
//
// FIRST, before anything starts, for the reason the shared servers retire
// first: a rename frees the name and the tools it published before the
// replacement files its own.
func retireUndeclared(ctx context.Context, s *seatChildren, specs []mcp.Spec) {
	desired := make(map[string]struct{}, len(specs))
	for _, spec := range specs {
		desired[spec.Name] = struct{}{}
	}
	for _, name := range s.bridge.Servers() {
		if _, keep := desired[name]; keep {
			continue
		}
		if _, err := s.bridge.Stop(ctx, name); err != nil {
			log.WarnContext(ctx, "mcp_server_retire_failed", "server", name, "seat", s.handle,
				"error", err, "detail", "its tools are gone from the seat's catalogue; "+
					"the child may have survived and is reaped when the seat is released")
			continue
		}
		log.InfoContext(ctx, "mcp_server_retired", "server", name, "seat", s.handle,
			"detail", "the seat no longer declares this per-role server")
	}
}

// bringUp starts or restarts, CONCURRENTLY, every child whose spec the bridge
// is not already running, and reports what each declared child concluded, in
// spec order, and whether one was left to retry.
//
// A child already running its spec is NOT handed to the bridge at all: it is
// a process holding a working credential, and the pass that leaves it alone
// is what keeps an apply from re-handshaking every seat's vendor on every
// revision. Concurrent for the reason [startAll] is: each start is a spawn and
// a handshake with nothing to do with the others, and a seat is not consuming
// its mailbox until the slowest one is done.
//
// A spec that can never launch ([mcp.ErrInvalidSpec]) is not retried: nothing
// about it changes until a revision does, and an apply runs a pass of its own.
func bringUp(ctx context.Context, s *seatChildren, specs []mcp.Spec) ([]mcpOutcome, bool) {
	outcomes := make([]mcpOutcome, len(specs))
	var wg sync.WaitGroup
	for i, spec := range specs {
		outcomes[i].server = mcp.ServerName(spec.Name)
		if s.bridge.Runs(spec) {
			outcomes[i].observe(s.bridge, spec.Name)
			continue
		}
		wg.Go(func() {
			replacing := s.bridge.Has(spec.Name)
			if _, err := s.bridge.Reconcile(ctx, spec); err != nil {
				outcomes[i].err = err
				if ctx.Err() != nil {
					// ENDED mid-start — by the release, or by the
					// stop the claim belongs to — on purpose, and a
					// released seat files nothing of this pass.
					return
				}
				log.ErrorContext(ctx, "mcp_server_failed", "server", spec.Name, "seat", s.handle,
					"error", err, "detail", "this server's tools are absent; its group is "+
						"missing from the seat's catalogue rather than the builtins shrinking")
				return
			}
			outcomes[i].observe(s.bridge, spec.Name)
			if replacing {
				log.InfoContext(ctx, "mcp_server_restarted", "server", spec.Name,
					"seat", s.handle, "tools", outcomes[i].tools,
					"detail", "the spec it runs from moved: a revision edited it, or a "+
						"credential it resolves was sealed or rotated")
				return
			}
			log.InfoContext(ctx, "mcp_server_started", "server", spec.Name,
				"seat", s.handle, "tools", outcomes[i].tools)
		})
	}
	wg.Wait()
	retry := false
	for _, o := range outcomes {
		if o.err != nil && !errors.Is(o.err, mcp.ErrInvalidSpec) {
			retry = true
		}
	}
	return outcomes, retry
}

// seatSurface is the registry one seat's turns are built against: the epoch's
// surface cloned, with the seat's own live catalogue filed into it.
//
// CLONED, not shared: the seat's own tools must not reach a peer seat, and
// the epoch's registry is read by every other seat on this node. Filed from
// the bridge's live catalogue rather than from what one pass started, so the
// claim and every apply build the same surface from the same children —
// whichever pass happened to start them.
func seatSurface(ctx context.Context, c *Company, s *seatChildren) *tools.Registry {
	reg := c.Tools.Clone()
	for _, registration := range s.bridge.Registrations() {
		if err := reg.RegisterMCP(registration); err != nil {
			log.WarnContext(ctx, "mcp_tool_refused", "seat", s.handle,
				"tool", registration.Tool.Name(), "error", err)
		}
	}
	return reg
}

// stopSeatServers kills a released seat's children.
//
// TOTAL, and idempotent: the seat's whole bridge goes, so there is no way to
// leave a child of a seat this node no longer holds — and running for a seat
// that never got one, which a failed acquire does, is a no-op.
func (e *Engine) stopSeatServers(ctx context.Context, handle string) {
	// FORGOTTEN FIRST, surface and outcomes with the bridge: a registry
	// advertising the tools of children being stopped offers the model
	// entries that can only fail, and a released seat's failures are no
	// longer this node's to report.
	e.mcpMu.Lock()
	s := e.seatMCP[handle]
	delete(e.seatMCP, handle)
	delete(e.seatTools, handle)
	delete(e.mcpSeats, handle)
	e.mcpMu.Unlock()
	if s == nil {
		return
	}
	if e.retireSeatChildren(ctx, s) {
		log.InfoContext(ctx, "mcp_seat_stopped", "seat", handle)
	}
}

// retireSeatChildren ends one seat's children for good, reporting whether its
// bridge stopped cleanly. The caller has already taken s out of the engine's
// map, so nothing new can find it.
//
// IN THIS ORDER, which is the whole of it: released, so no pass starts; the
// pass in flight and the lease cancelled, so the pass gives up its starts at
// once and a retry that already fired finds its context over; the retry
// disarmed; that pass waited for, so nothing is still adding to the bridge;
// and only then the bridge stopped, with nothing left that could start a
// child behind it.
func (e *Engine) retireSeatChildren(ctx context.Context, s *seatChildren) bool {
	s.mu.Lock()
	s.released = true
	s.cancel()
	if s.endPass != nil {
		s.endPass()
	}
	if s.retry != nil {
		s.retry.Stop()
		s.retry = nil
	}
	for s.running {
		s.idle.Wait()
	}
	s.mu.Unlock()
	if _, err := s.bridge.StopAll(ctx); err != nil {
		log.WarnContext(ctx, "mcp_seat_stop_failed", "seat", s.handle, "error", err)
		return false
	}
	return true
}

// takeAllSeatChildren empties the engine's per-seat state, for shutdown.
func (e *Engine) takeAllSeatChildren() []*seatChildren {
	e.mcpMu.Lock()
	defer e.mcpMu.Unlock()
	out := make([]*seatChildren, 0, len(e.seatMCP))
	for _, s := range e.seatMCP {
		out = append(out, s)
	}
	clear(e.seatMCP)
	clear(e.seatTools)
	clear(e.mcpSeats)
	return out
}
