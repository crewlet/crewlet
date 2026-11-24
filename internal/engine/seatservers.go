package engine

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/tools"
)

// A held seat's per-role MCP children, kept converged on what the seat should
// run for as long as this node holds it. The children belong to the seat's
// LEASE (mcp.go); this file is everything between the claim and the release.
// A child is started from a SPEC (the template with the seat's `mcp_env` laid
// over it, every ${VAR} resolved through this node's secret snapshot), and that
// spec moves while the seat is held: a credential sealed AFTER the claim (a
// first boot claims seats before the provisioning pass mints their tokens), a
// credential ROTATED by re-activating an unchanged revision (a child keeps the
// environment it was exec'd with, so only a restart hands it the new value), a
// revision's edit to a template, `mcp_env` or grant, or a start that failed for
// a reason that passes (a vendor not up yet, a package fetch that timed out).

// So every pass RECONCILES each held seat's children against the current epoch
// and snapshot: an unchanged spec keeps running, a moved one is restarted, a
// missing one started (unless a value it starts with resolves to nothing, when
// it is held back, waiting rather than failed: package doc, "Tool server
// credentials"), and one the seat no longer declares, or whose credential
// stopped resolving, is stopped.
// A pass that left a child failed to START retries on [mcp.RestartWait] until it
// starts or the seat goes; a spec that can never launch and a child held back
// for its credential are not retried, since only a revision or a seal changes
// them, and each runs a pass of its own ([refreshingSink.Flush],
// [Engine.rebuildForSealedSecrets]).

// A child that DIES after it started is its bridge's to bring back, gated on the
// same proof of the lease (mcpsupervise.go): [mcp.Bridge.Runs] answers yes for
// it, so a pass never races the bridge to start it, and an apply's pass only
// asks it to try now. Unless its spec MOVED: the bridge would bring back the
// replaced spec, so the pass restarts it into the new one, and
// [mcp.Bridge.Reconcile] ends the old spec's supervision first.

// # One pass at a time per seat, and the last one is current
//
// A pass is asked for by the claim, every apply, a seal and the retry timer, and
// two over one bridge would race a restart against a start of the same child.
// So each seat has at most one RUNNER: whoever finds one there asks it to go
// AGAIN and returns, and it stops only after a pass nothing asked to repeat. An
// apply landing mid-claim thus makes the claim rerun against the apply's
// snapshot before the seat's mailbox opens. THE CLAIM IS ITS SEAT'S RUNNER FROM
// BEFORE IT IS REGISTERED: an apply that found the seat first (or while the
// claim retired a predecessor) and became its runner would leave the claim
// returning at once, its mailbox opening on children still starting.

// # The apply does not wait for a child
//
// An apply refiles each held seat's registry against the epoch it just
// published, then hands the child reconcile to the seat's runner WITHOUT
// WAITING: a start may take a whole startup timeout, and every later stage (a
// new role's mailbox, the mail held for want of a model, the scheduler, the
// sweep claiming new seats, later activations) would wait behind it. Each pass
// refiles its seat's surface when it ends, so the next turn sees its children.

// # Never a child for a seat this node cannot prove it holds
//
// A child IS the seat: its credentials are the agent's identity at the vendor.
// So a pass starts nothing unless [seat.Host.Provable] holds: an apply's or a
// retry's pass may land after the lease lapsed, before the heartbeat noticed,
// and would start a second copy of an agent a peer runs. Such a pass is retried
// like a failed start, and the release that follows a lost lease cancels it.

// # A seat the company no longer declares keeps no child
//
// Placement may hold a removed role's seat for a whole sweep interval
// ([seat.Host.Resweep]), so the apply's pass retires its children rather than
// leaving them to the release: one kept running would act at the vendor for an
// agent the company no longer has. A turn still running finds those tools gone
// (a call to one it holds is refused as unavailable) and keeps the builtins,
// its surface refiled from the current epoch until the release.

// # The release always wins
//
// Releasing a seat cancels the pass in flight (its starts end at once, not at a
// startup timeout), waits for it, disarms the retry, and only then stops the
// bridge ([Engine.retireSeatChildren]). A pass that finishes or is asked for
// after the release installs nothing.

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

	// lease ends when the seat is released. A retry or renewal runs under it,
	// being the one pass nobody calls with a context of their own, so one that
	// fires as the seat is let go finds its context already over.
	lease  context.Context
	cancel context.CancelFunc

	// mu guards everything below; idle is signalled when a pass ends.
	mu   sync.Mutex
	idle sync.Cond

	// running is set while the seat has a RUNNER, from the moment one is
	// chosen (before its goroutine starts, so a release waiting for it to
	// clear waits for that goroutine too); again, when a pass was asked for
	// that the runner has not yet run.
	running bool
	again   bool

	// endPass cancels the pass in flight, nil between passes. Set under mu in
	// the step that checks released, so the release either finds it to cancel
	// or the runner starts nothing: a release never waits out a startup timeout.
	endPass context.CancelFunc

	// released is set by the release, after which nothing runs.
	released bool

	// failures counts the passes in a row that left a child to retry, which
	// the retry's delay doubles on. An apply resets it: it changes what a
	// child is started from, so it begins a new series.
	failures int

	// retryNow is set by an apply and taken by the next pass, which asks every
	// child its bridge is bringing back to try now ([mcp.Bridge.RetryNow]), as
	// an apply does of a down shared server: it is an operator's gesture once a
	// vendor is back.
	retryNow bool

	// retry is the armed retry, nil when none is due.
	retry *time.Timer

	// renew is the pass armed to run at the earliest renew time of the
	// credentials the seat's children were issued at use, nil when no child
	// holds one ([Engine.armRenewal]).
	renew *time.Timer
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
		held:   func() bool { return e.seatProvable(handle) },
		lease:  lease,
		cancel: cancel,
	}
	// A child that dies is restarted only while this node can prove the
	// seat's lease, the predicate the passes ask (mcpsupervise.go).
	s.bridge = mcp.NewBridge(nil, e.seatSupervision(s))
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
// It returns once a pass has run that nothing asked to repeat, because the
// claim's contract is that everything the first turn needs is up before the
// mailbox opens; an apply landing meanwhile is folded in (the file comment).
func (e *Engine) startSeatServers(ctx context.Context, handle string) {
	e.runSeatPasses(ctx, e.claimSeatChildren(ctx, handle))
}

// claimSeatChildren builds and registers the state for a seat this node has
// just claimed, retiring whatever this node still kept for it, and returns it
// with the CALLER AS ITS RUNNER: a pass already asked for, which the caller
// must run ([Engine.runSeatPasses]).
//
// REGISTERED BEFORE THE FIRST PASS, so an apply landing during the claim (one
// sealing the children's credential, say) finds the seat, and OWNED BEFORE IT
// IS REGISTERED, so that apply can only ask the claim to go again (the file
// comment). Set without the lock: nothing else can see the state yet.
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
		// A failed acquire releases the seat, and a re-claim can arrive
		// before that teardown ran. Stopped BEFORE the new children start, so
		// two copies of the seat's identity never run here at once, on a
		// context free of the claim's: the cancellation is often what is
		// being undone.
		e.retireSeatChildren(context.WithoutCancel(ctx), previous)
	}
	return s
}

// reconcileSeatServers brings every seat this node holds onto the epoch an
// apply has just published: the `seat_tools` step of [Engine.followCompany],
// so a boot runs it too, holding no seat yet.
//
// TWO STEPS, and the apply waits for the first alone: each seat's registry, a
// clone of the epoch's surface, is REFILED at once (cheap, touching no process)
// or a turn would run the new revision with the old builtins and knobs; then
// each seat's own runner CONVERGES its children, one goroutine per seat
// ([Engine.askSeatPasses], the file comment).
//
// Called AFTER the pointer moves, like the mailbox and scheduler steps: a later
// refusal must not leave seat surfaces built from an epoch never current.
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
	e.askSeatPasses(ctx, held)
}

// passHeldSeats asks every seat this node holds for a pass over its children,
// for a snapshot that moved with no apply behind it on this node: the one a
// provisioning pass refreshes the moment it has sealed
// ([refreshingSink.Flush]).
//
// AN APPLY'S KIND OF PASS, a new retry series included, since it changes what
// a child is started from; it refiles no surface, since the epoch did not move.
// Needed because the re-activation a seal requests is a WRITE that may not
// happen (no config surface, a refused write, a store blip), and a child held
// back for its credential has no retry of its own to notice the seal.
func (e *Engine) passHeldSeats(ctx context.Context) {
	e.mcpMu.Lock()
	held := make([]*seatChildren, 0, len(e.seatMCP))
	for _, s := range e.seatMCP {
		held = append(held, s)
	}
	e.mcpMu.Unlock()
	e.askSeatPasses(ctx, held)
}

// askSeatPasses asks each seat for an apply's kind of pass, and runs it on a
// goroutine of the seat's own where nobody is running its passes already.
//
// Each runner's goroutine belongs to the seat, not the caller: the release
// and the engine's stop cancel its pass and wait for it
// ([Engine.retireSeatChildren]). Its context drops the caller's cancellation,
// as the lease's does: the caller is done long before a slow start is.
func (e *Engine) askSeatPasses(ctx context.Context, held []*seatChildren) {
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
		s.failures, s.retryNow = 0, true
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
// What the last pass left decides what happens next: a retry, armed here and
// disarmed by the next pass or by the release, when a child it tried to start
// did not — and otherwise nothing. A child it did not try to start is no
// reason to retry: one held back for its credential, or one whose spec can
// never launch, waits for the snapshot or a revision to change, and each of
// those runs a pass of its own.
func (e *Engine) runSeatPasses(ctx context.Context, s *seatChildren) {
	s.mu.Lock()
	retry := false
	for s.again && !s.released {
		s.again = false
		retryNow := s.retryNow
		s.retryNow = false
		pass, end := context.WithCancel(ctx)
		s.endPass = end
		s.mu.Unlock()
		retry = e.seatPass(pass, s, retryNow)
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
// child to retry waits (failures counts from one): the restart schedule every
// MCP server is retried on ([mcp.RestartWait]), from the engine's base.
func (e *Engine) seatRetryDelay(failures int) time.Duration {
	return mcp.RestartWait(failures, e.mcpRetryBase)
}

// seatPass brings one seat's children to what the current epoch and this
// node's snapshot say it should run, files the seat's surface, and reports
// whether a child was left to retry. retryNow is an apply's request that
// every child the bridge is bringing back try now ([seatChildren.retryNow]).
//
// ctx is the pass's own, which the release cancels ([seatChildren.endPass]).
func (e *Engine) seatPass(ctx context.Context, s *seatChildren, retryNow bool) bool {
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
	var (
		specs    []mcp.Spec
		outcomes []mcpOutcome
		id       uuid.UUID
		token    *seatToken
	)
	if seat := c.Org.AgentSeatByHandle(s.handle); seat != nil {
		// A SEAT THE EPOCH NO LONGER HAS skips this and declares nothing,
		// so every child goes (the file comment).
		var env *config.Resolver
		env, token = e.seatResolver(ctx, c, seat, toolTokenLife)
		specs, outcomes = seatSpecs(c, seat, env,
			&seatMint{kinds: e.mintedFor(c, s.handle), token: token})
		outcomes = e.withheldForCredentials(ctx, outcomes)
		// THE SEAT'S ID rides every outcome: the heartbeat names a failing
		// instance by it, as a lease names its seat.
		id, _ = c.Org.AgentIDFor(seat)
	}
	retireUndeclared(ctx, s, specs, outcomes, serversTurnedOff(c.Config))
	started, retry := bringUp(ctx, s, specs, retryNow)
	outcomes = append(outcomes, started...)
	// A TOKEN ITS VENDOR WOULD NOT ISSUE IS RETRIED like a child that would
	// not start: it is the vendor's answer this time, so waiting for a
	// revision or a seal would wait for nothing. One it refused waits on a
	// person's step ([seatIssuer.refusal]), which the retry notices; one the
	// integration is still settling clears with nobody acting
	// ([seatToken.settling]). An issued token is renewed ([Engine.armRenewal]).
	if token.refused() || token.settling() {
		retry = true
	}
	e.armRenewal(s, token)

	e.recallMCPDown(ctx)
	e.mcpMu.Lock()
	defer e.mcpMu.Unlock()
	if e.seatMCP[s.handle] != s {
		// RELEASED, or replaced by a later claim, while this pass ran.
		return false
	}
	if current := e.Company(); current != nil && current.Tools != nil {
		// THE EPOCH CURRENT NOW, not the pass's: an apply meanwhile has
		// asked for another pass, but until it runs the builtins must
		// already be the new revision's.
		if e.seatTools == nil {
			e.seatTools = make(map[string]*tools.Registry)
		}
		e.seatTools[s.handle] = seatSurface(ctx, current, s)
	}
	for i := range outcomes {
		outcomes[i].seat = id
	}
	stampSince(e.mcpSeats[s.handle], outcomes, e.now())
	if id != uuid.Nil {
		e.mcpDown.keepOnly(id, outcomes)
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

// armRenewal arms the pass that renews the credentials the seat's children
// were just issued at use, or disarms it where none was issued.
//
// AN ISSUED TOKEN LIVES ABOUT AN HOUR and a child holds the one it started
// with (a stdio server reads its environment once, an http one sends its
// configured header). So the pass reruns at the EARLIEST renew time across
// what was issued ([seatToken.renewAt]), is issued new values, and a changed
// value restarts its child as a rotated secret does: no child holds a token
// past its expiry.
//
// ON THE LEASE'S CONTEXT, as a retry is: one that fires as the seat is let go
// finds its context over, and the release disarms it.
func (e *Engine) armRenewal(s *seatChildren, token *seatToken) {
	at, ok := token.renewAt()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.renew != nil {
		s.renew.Stop()
		s.renew = nil
	}
	if !ok || s.released {
		return
	}
	wait := max(time.Until(at), 0)
	s.renew = time.AfterFunc(wait, func() { e.convergeSeat(s.lease, s, false) })
}

// retireUndeclared stops every child of the seat that specs no longer names: a
// template removed, renamed or turned off (off names those), a grant lost, a
// server turned shared, or one whose credential stopped resolving (held lists
// those among the pass's outcomes).
//
// FIRST, before anything starts, as the shared servers retire first: a rename
// frees the name and its tools before the replacement files its own.
//
// A CHILD WHOSE CREDENTIAL WENT is stopped by the rule that would not start it
// (package doc, "Tool server credentials"): a deleted sealed token leaves it
// acting at the vendor on the value it started with. Its log names what is
// missing, not the config, which still declares the server.
func retireUndeclared(ctx context.Context, s *seatChildren, specs []mcp.Spec, held []mcpOutcome,
	off map[string]struct{},
) {
	desired := make(map[string]struct{}, len(specs))
	for _, spec := range specs {
		desired[spec.Name] = struct{}{}
	}
	withheld := withheldByInstance(held)
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
		if missing, ok := withheld[name]; ok {
			log.InfoContext(ctx, "mcp_server_retired", "server", name, "seat", s.handle,
				"detail", "a value the child is started with no longer resolves, so it "+
					"is not left acting on the one it was started with: "+missing.reason())
			continue
		}
		if _, turnedOff := off[mcp.ServerName(name)]; turnedOff {
			log.InfoContext(ctx, "mcp_server_retired", "server", name, "seat", s.handle,
				"detail", turnedOffDetail)
			continue
		}
		log.InfoContext(ctx, "mcp_server_retired", "server", name, "seat", s.handle,
			"detail", "the seat is no longer granted this per-role server, or the revision no longer declares it")
	}
}

// bringUp starts or restarts, CONCURRENTLY, every child whose spec the bridge
// is not already running, and reports what each declared child concluded, in
// spec order, and whether one was left to retry.
//
// A child already running its spec's process is NOT restarted, which keeps an
// apply from re-handshaking every vendor; a tool's level, prefix or hints refile it.
// Nor is one that DIED from this very spec ([mcp.Bridge.Runs] answers yes, its
// bridge bringing it back, and a second start would race that loop); an apply's
// pass (retryNow) asks it to try now. Concurrent for [startAll]'s reason. A spec
// that can never launch ([mcp.ErrInvalidSpec]) is not retried: only a revision
// changes it. Each outcome names the bridge and instance, so the heartbeat
// reports the child as it is at each beat.
func bringUp(ctx context.Context, s *seatChildren, specs []mcp.Spec, retryNow bool) ([]mcpOutcome, bool) {
	outcomes := make([]mcpOutcome, len(specs))
	var wg sync.WaitGroup
	for i, spec := range specs {
		outcomes[i].server = mcp.ServerName(spec.Name)
		outcomes[i].bridge, outcomes[i].instance = s.bridge, spec.Name
		if s.bridge.Runs(spec) {
			s.bridge.Refile(spec)
			if retryNow {
				s.bridge.RetryNow(spec.Name)
			}
			outcomes[i].observe(s.bridge, spec.Name)
			continue
		}
		wg.Go(func() {
			replacing := s.bridge.Has(spec.Name)
			if _, err := s.bridge.Reconcile(ctx, spec); err != nil {
				outcomes[i].err = err
				if ctx.Err() != nil {
					// ENDED mid-start on purpose, by the release or
					// the claim's stop: nothing to log.
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
// surface as the seat holds it ([grantedSurface]: the shared servers it is
// granted, and no other), with the seat's own live catalogue filed into it,
// and every tool on it held to the seat's limits ([limitSurface]) once both
// are in.
//
// CLONED, not shared: the seat's own tools must not reach a peer seat. Filed
// from the bridge's live catalogue rather than from what one pass started, so
// every pass builds the same surface from the same children.
func seatSurface(ctx context.Context, c *Company, s *seatChildren) *tools.Registry {
	seat := c.Org.AgentSeatByHandle(s.handle)
	reg := sharedSurface(c, seat)
	for _, registration := range s.bridge.Registrations() {
		if err := reg.RegisterMCP(registration); err != nil {
			log.WarnContext(ctx, "mcp_tool_refused", "seat", s.handle,
				"tool", registration.Tool.Name(), "error", err)
		}
	}
	limitSurface(reg, seat)
	return reg
}

// stopSeatServers kills a released seat's children.
//
// TOTAL, and idempotent: the seat's whole bridge goes, and a seat that never
// got one (a failed acquire's) is a no-op.
func (e *Engine) stopSeatServers(ctx context.Context, handle string) {
	// FORGOTTEN FIRST: a registry advertising children being stopped offers
	// the model tools that can only fail, and a released seat's failures are
	// no longer this node's to report.
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
// IN THIS ORDER: released, so no pass starts; the pass in flight and the lease
// cancelled, so its starts end at once and a retry that already fired finds its
// context over; the timers disarmed; that pass waited for; and only then the
// bridge stopped, with nothing left that could start a child behind it.
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
	if s.renew != nil {
		s.renew.Stop()
		s.renew = nil
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
