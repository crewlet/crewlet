package mcp

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crewlet/crewlet/internal/backoff"
)

// Keeping a server up once it has started, and bringing back one that would
// not start.
//
// A server's session can end under the engine at any moment: a stdio child
// crashes, is killed for memory, or exits because its vendor's API answered
// something it could not handle; a remote server forgets the session. Before
// this file existed nothing noticed. The bridge's entry stayed "running", the
// tools stayed in every catalogue, and every call failed on a broken pipe
// until something restarted the server — for a shared server, the next config
// apply, which on a quiet company is never; for a seat's own child, the seat
// changing hands. A shared server whose START failed for a reason that passes
// — the vendor was down, a DNS blip — had the same wait, because nothing but
// the next apply ever asked for it again.
//
// So every server a bridge runs has a SUPERVISOR: one goroutine for the life
// of the server, which waits for its session to end (seen at the transport,
// see client.end) and then restarts it on the schedule in timeouts.go —
// at once for a server's first death and for a child that had been healthy,
// then with a doubling, jittered wait. While it is down its tools STAY in the catalogue and refuse, with
// [RefusalUnavailable] and a sentence saying what happened and that it is
// coming back: a turn that already holds them gets an honest answer rather
// than a broken pipe, and a catalogue that does not churn on every crash
// keeps the provider's prompt cache. The Tool objects survive the restart
// too — a Tool reaches its server through the server, never through one
// session — so a restart that lists the same tools changes nothing outside
// this package, and only one that lists different tools reports a change
// ([Supervision.CatalogueChanged]).
//
// What the supervisor does NOT do is decide who may run the server. A
// per-role child carries one seat's identity, and only the node holding that
// seat may start it; the bridge asks its owner ([Supervision.MayStart])
// before every start it makes on its own, and its owner stops it — ending the
// supervision with it — the moment the seat is let go.

// Supervision is what a bridge does on its own, between the calls its owner
// makes. The zero value restarts a server whose session ended, unconditionally,
// and leaves a failed start to its owner.
type Supervision struct {
	// RetryFailedStarts keeps a server whose start failed — for any reason
	// but a spec that can never launch ([ErrInvalidSpec]) — and tries it again
	// on the restart schedule, as it does a server whose session ended. The
	// start still fails to its caller, which reports it; what changes is that
	// the next apply is no longer the only thing that will ever ask again.
	//
	// For a bridge whose owner retries a failed start ITSELF it must be
	// false, or two retry loops would race to start one child.
	RetryFailedStarts bool

	// MayStart is asked before every start the bridge makes ON ITS OWN — a
	// restart or a retry — and never before one its owner asks for, whose
	// authority is the owner's. False holds the start back and asks again
	// after the schedule's base wait, without lengthening the schedule:
	// nothing was tried, so nothing failed. Nil starts unconditionally.
	//
	// It exists for a per-role child: the child IS its seat at the vendor,
	// so a node that cannot prove it still holds the seat must not start
	// one, or a seat whose lease lapsed would have a second copy of its
	// agent running beside the node that took it over.
	MayStart func() bool

	// CatalogueChanged is called after a start the bridge made on its own
	// changed the catalogue — a retried start that came up, a restart that
	// listed different tools — so the owner can refile the registries built
	// from it. A restart that lists the same tools does not call it.
	//
	// Called on the supervisor's goroutine, with the supervision's context,
	// outside every lock of the bridge's: it must not wait on anything a
	// caller of Stop, StopAll, Restart or Reconcile holds, because those wait
	// for the supervisor.
	CatalogueChanged func(context.Context)

	// RetryBase replaces [RestartBase] as the schedule's first wait, zero for
	// RestartBase, and SCALES THE WHOLE SCHEDULE with it: the ceiling, and so
	// how long a child must stay up to count as healthy, stay the same
	// multiple of the base. A deployment has no reason to set it; it exists
	// so a test can watch a schedule play out without waiting minutes for it.
	RetryBase time.Duration
}

// restartCeilingRatio is the ceiling's multiple of the base: twenty, the seat
// heartbeats in the undead alarm interval.
const restartCeilingRatio = RestartCeiling / RestartBase

// RestartWait is how long the restart schedule waits before attempt n of a
// series that waits at all, counting from one: base, then twice as long for
// each attempt after it up to twenty times base, each wait spread by a fifth
// either way. A base of zero is [RestartBase], which makes the ceiling
// [RestartCeiling]; any other base scales the whole schedule with it.
//
// EXPORTED because it is the ONE schedule a server that will not come back is
// retried on, whoever retries it: this package's supervisor for a server whose
// session ended, and the engine's seat runner for a per-role child whose start
// failed. Two copies of it, each anchored to the same seat cadence by its own
// comment, would be two schedules the next change moves one of.
func RestartWait(n int, base time.Duration) time.Duration {
	if base <= 0 {
		base = RestartBase
	}
	return backoff.Jitter(backoff.Doubling(n, base, base*restartCeilingRatio), restartJitter)
}

// wait is how long the schedule waits before attempt n of a series, counted
// from zero: nothing for the first attempt of a series that a healthy child's
// death or an owner's request began, and [RestartWait] for every other.
func (s Supervision) wait(n int) time.Duration {
	if n <= 0 {
		return 0
	}
	return RestartWait(n, s.RetryBase)
}

// ceiling is the schedule's longest wait before its spread, and how long a
// child must stay up for its death to begin a new series. See [RestartCeiling].
func (s Supervision) ceiling() time.Duration { return s.base() * restartCeilingRatio }

// base is the schedule's first wait before its spread.
func (s Supervision) base() time.Duration {
	if s.RetryBase > 0 {
		return s.RetryBase
	}
	return RestartBase
}

// Health is one server's condition as its bridge sees it now.
type Health struct {
	// Serving is true while a live session answers for the server.
	Serving bool

	// Tools is how many tools the server contributes. Kept while it is down:
	// they stay in the catalogue, refusing until it is back.
	Tools int

	// Down says why the server is not serving and that the bridge is
	// bringing it back; empty while it serves. The cause comes first —
	// "its process exited (exit status 1)", "would not start: …" — because
	// that is the part a reader of a clipped copy needs.
	Down string

	// Class is why the server is down ([FailureClass]): empty while it
	// serves, where no class fits, and while a restart somebody asked for
	// takes it down and brings it back, which is nobody's failure.
	Class FailureClass

	// Since is when the server's current state began: when the session
	// serving it started, or when it went down. Zero for a server stopped.
	Since time.Time

	// Restarts is how many times the bridge brought the server back since
	// its owner started it from this spec: a session that ended and a start
	// that came up on a later attempt each count once.
	Restarts int

	// LastFailure is the server's latest failure, KEPT AFTER IT COMES BACK,
	// since "it is up now" and "it went down an hour ago" are both true and
	// a person needs the second to see a server that flaps. Nil when it has
	// had none since its owner started it. A copy: changing it changes
	// nothing in the bridge.
	LastFailure *Failure

	// LastAnswer is when the server last answered: a start or restart that
	// listed its tools, or a tool call that came back. Zero before either.
	LastAnswer time.Time
}

// server is one server a bridge supervises: the spec it runs from, the tools
// it contributes, and the session serving it now.
type server struct {
	b    *Bridge
	spec Spec

	// tools is what this server contributes to the catalogue. Guarded by the
	// BRIDGE's mu, because the catalogue is derived from it there.
	tools []*Tool
	// unlisted are the AnnotationOverrides keys the listing tools was built
	// from names no tool for ([Bridge.UnlistedOverrides]): set with tools,
	// from the same listing, so a restart that lists a different set moves
	// both. Guarded by the BRIDGE's mu, like tools.
	unlisted []string
	// refused are the tools that listing named and that are not offered for
	// their name ([Bridge.RefusedTools]), set with tools from the same
	// listing and guarded by the same mu.
	refused []RefusedTool

	// answered is when the server last answered, in Unix nanoseconds: a
	// start that listed its tools, or a tool call that came back. ATOMIC,
	// because every tool call writes it and a lock on that path would put
	// every call behind every other.
	answered atomic.Int64

	mu sync.RWMutex
	// client is the session serving the server now, nil while it is down.
	client *client
	// last is the most recent session, live or dead: its stderr tail is the
	// server's last words, and outlives it.
	last *client
	// down is why the server is not serving, nil while it is.
	down *outage
	// stopped is set once the server's owner has stopped it.
	stopped bool
	// attempt is how many attempts the current restart series has made, and
	// so which wait the next one takes ([Supervision.wait]).
	attempt int
	// upSince is when the current session started.
	upSince time.Time
	// restarts is how many sessions after the first have come up
	// ([Health.Restarts]).
	restarts int
	// failure is the latest failure, kept after the server comes back
	// ([Health.LastFailure]).
	failure *Failure
	// requested is the cause of a restart somebody asked for
	// ([Bridge.RestartBefore]), set from the moment it ends the session
	// until the supervisor has taken it down: the end is theirs, so it is
	// neither logged nor recorded as the server's failure, and the restart
	// waits nothing.
	requested string

	// cancel ends the supervision, whose context it was made with; Stop
	// calls it and waits on done.
	cancel context.CancelFunc
	// nudged asks for an attempt now, from an owner whose reconcile found
	// the server down. One slot: two requests for "now" are one request.
	nudged chan struct{}
	done   chan struct{}
}

// outage is why a server is not serving.
type outage struct {
	// cause is what took it down, or why its start failed.
	cause string
	// failed is why the latest restart attempt failed, empty when none has.
	failed string
	// heldBack is set when the latest attempt was refused by MayStart.
	heldBack bool
	// class is why it is down ([FailureClass]), empty where no class fits
	// or a restart somebody asked for took it down.
	class FailureClass
	// since is when it went down.
	since time.Time
	// next is when the next attempt is due, zero while one is running.
	next time.Time
}

// describe is the outage for the heartbeat and the bridge's own record:
// the most recent reason first, and what is being done about it. Free of
// times, so a heartbeat re-sending the same facts sends the same bytes.
func (o *outage) describe() string {
	reason := o.cause
	if o.failed != "" {
		reason = "would not restart: " + o.failed + " (" + o.cause + ")"
	}
	if o.heldBack {
		return reason + "; its restart is held back until this node may start it"
	}
	return reason + "; the engine is restarting it"
}

// supervision is the context a server started under ctx is supervised on,
// and what ends it.
//
// ctx's VALUES — the logger and trace of whoever started it — WITHOUT ctx's
// cancellation: the start returns long before the server stops, and a
// supervision that ended with the call that started it would leave the server
// unwatched for the rest of its life. What ends it is the server's owner
// stopping it ([server.shutdown]).
func supervision(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithCancel(context.WithoutCancel(ctx))
}

// newServer builds the record for a server whose supervision cancel ends.
func (b *Bridge) newServer(spec Spec, cancel context.CancelFunc) *server {
	return &server{
		b:      b,
		spec:   spec,
		cancel: cancel,
		nudged: make(chan struct{}, 1),
		done:   make(chan struct{}),
	}
}

// live is the session serving the server now, or nil.
func (s *server) live() *client {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.client
}

// lastClient is the most recent session, live or dead, or nil before one
// ever started.
func (s *server) lastClient() *client {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.last
}

// health is the server's condition, with the tool count its bridge supplies.
func (s *server) health(tools int) Health {
	s.mu.RLock()
	defer s.mu.RUnlock()
	h := Health{Tools: tools, Restarts: s.restarts}
	if s.failure != nil {
		failure := *s.failure
		h.LastFailure = &failure
	}
	if at := s.answered.Load(); at != 0 {
		h.LastAnswer = time.Unix(0, at)
	}
	switch {
	case s.client != nil && !s.client.gone():
		h.Serving, h.Since = true, s.upSince
	case s.down != nil:
		h.Down, h.Class, h.Since = s.down.describe(), s.down.class, s.down.since
	case s.client != nil:
		// Dead, and the supervisor has not got to it yet.
		h.Down, h.Since = s.client.deathStory()+"; the engine is restarting it", s.upSince
		if s.requested == "" {
			h.Class = s.client.deathClass()
		}
	default:
		h.Down = "stopped"
	}
	return h
}

// noteAnswer records that the server answered now.
func (s *server) noteAnswer() { s.answered.Store(time.Now().UnixNano()) }

// RestartOutcome is what a restart request did to one server.
type RestartOutcome string

const (
	// RestartedNow is a serving server whose session began before the
	// request: the session was ended and a fresh one is starting, at once.
	RestartedNow RestartOutcome = "restarted"
	// RetriedNow is a server that was down since before the request: its
	// supervisor was asked for an attempt now, its schedule started again.
	RetriedNow RestartOutcome = "retried"
	// RestartCurrent is a server whose session began, or which went down, at
	// or after the request: it is already what the request asked for.
	RestartCurrent RestartOutcome = "current"
	// RestartAbsent is a name the bridge does not supervise, or one being
	// stopped.
	RestartAbsent RestartOutcome = "absent"
)

// restartBefore restarts the server if its current state began before
// before: a serving server's session is ended so its supervisor starts a
// fresh one at once, and a down server's supervisor is asked to try now. The
// schedule starts again either way. cause is what the end is told as.
func (s *server) restartBefore(before time.Time, cause string) RestartOutcome {
	s.mu.Lock()
	switch {
	case s.stopped:
		s.mu.Unlock()
		return RestartAbsent
	case s.client != nil && !s.client.gone():
		if !s.upSince.Before(before) {
			s.mu.Unlock()
			return RestartCurrent
		}
		c := s.client
		s.attempt, s.requested = 0, cause
		s.mu.Unlock()
		// ENDED THE WAY A DEATH IS, so the one supervisor that owns the
		// server takes it down and brings it back: a restart made here
		// beside it would be a second loop over one child.
		c.markEnded(cause)
		return RestartedNow
	case s.down != nil && !s.down.since.Before(before):
		s.mu.Unlock()
		return RestartCurrent
	}
	s.mu.Unlock()
	s.nudge()
	return RetriedNow
}

// nudge asks the supervisor of a server that is down for an attempt now. It
// starts the schedule again, as a healthy child's death does: whoever asked
// has a reason to believe now is different.
func (s *server) nudge() {
	select {
	case s.nudged <- struct{}{}:
	default:
	}
}

// supervise is the server's supervisor, for its whole life: wait for the
// session to end, bring the server back, repeat — until Stop, which ends ctx
// ([supervision]).
func (s *server) supervise(ctx context.Context) {
	defer close(s.done)
	for {
		if c := s.live(); c != nil {
			select {
			case <-ctx.Done():
				return
			case <-c.ended():
			}
			// A stop and a death at the same instant is a stop: the
			// owner's shutdown clears the client away itself.
			if ctx.Err() != nil {
				return
			}
			s.died(ctx, c)
		}
		if !s.revive(ctx) {
			return
		}
	}
}

// died takes a dead session out of service and clears it away.
//
// OUT OF SERVICE FIRST, so every call from here is refused for what it is,
// and only then the cleanup — which can take a shutdown ladder — so the dead
// session is closed and its process TREE reaped before anything is started
// in its place: a package runner's grandchild that outlived the server holds
// the credentials and the port the replacement is about to need.
func (s *server) died(ctx context.Context, c *client) {
	now := time.Now()
	s.mu.Lock()
	lived := now.Sub(s.upSince)
	requested := s.requested
	s.requested = ""
	if lived >= s.b.sup.ceiling() || requested != "" {
		// A child that had been up this long was healthy: its death begins a
		// new series, and the first attempt of a series waits nothing. So
		// does a restart somebody asked for, which is a fresh start by
		// definition.
		s.attempt = 0
	}
	s.client = nil
	s.down = &outage{cause: c.deathStory(), since: now}
	s.mu.Unlock()

	_, _ = c.close(ctx)

	// Told again now that the process has been reaped: a child that exited
	// on its own has its status to tell, which the first sign of the end may
	// have come before. One the cleanup ended has none (client.deathStory).
	cause := c.deathStory()
	s.mu.Lock()
	s.down.cause = cause
	if requested == "" {
		s.down.class = c.deathClass()
		s.failure = &Failure{Class: s.down.class, Said: cause, At: now}
	}
	s.mu.Unlock()
	if requested != "" {
		s.b.log.Info("server_restart_requested", "server", s.spec.Name, "cause", cause,
			"up_seconds", lived.Seconds(),
			"detail", "its session was ended on request and a fresh one starts now; its "+
				"tools stay in the catalogue and refuse until it is back")
		return
	}
	s.b.log.Error("server_exited", "server", s.spec.Name, "cause", cause,
		"up_seconds", lived.Seconds(), "stable_after_seconds", s.b.sup.ceiling().Seconds(),
		"stderr_tail", c.stderrTail(),
		"detail", "its tools stay in the catalogue and refuse until it is back; the "+
			"engine restarts it on its own — at once if it had been up stable_after_seconds, "+
			"and on the restart schedule otherwise")
}

// revive brings a down server back, attempt by attempt on the schedule, and
// reports whether it did. False means the supervision ended first.
//
// An attempt the owner HOLDS BACK ([Supervision.MayStart]) is not one of the
// series: nothing was tried, so nothing failed, and the schedule does not
// lengthen for it. It is asked again after the schedule's base wait, which is
// when the answer can next change.
func (s *server) revive(ctx context.Context) bool {
	s.mu.RLock()
	delay := s.b.sup.wait(s.attempt)
	s.mu.RUnlock()
	for {
		s.mu.Lock()
		s.down.next = time.Now().Add(delay)
		s.mu.Unlock()
		if !s.sleep(ctx, delay) {
			return false
		}

		if mayStart := s.b.sup.MayStart; mayStart != nil && !mayStart() {
			delay = s.b.sup.wait(1)
			s.mu.Lock()
			s.down.heldBack = true
			s.mu.Unlock()
			s.b.log.Warn("server_restart_held_back", "server", s.spec.Name,
				"retry_in_seconds", delay.Seconds(),
				"detail", "its owner may not start it on this node now — a per-role "+
					"child whose node cannot prove it still holds the seat; asked "+
					"again until it may, or until the server is stopped")
			continue
		}

		s.mu.Lock()
		s.attempt++
		attempt := s.attempt
		s.down.next, s.down.heldBack = time.Time{}, false
		s.mu.Unlock()

		c, defs, err := s.b.launch(ctx, s.spec)
		if err != nil {
			if ctx.Err() != nil {
				return false
			}
			delay = s.b.sup.wait(attempt)
			s.mu.Lock()
			s.down.failed, s.down.class = err.Error(), ClassOf(err)
			s.failure = &Failure{Class: s.down.class, Said: err.Error(), At: time.Now()}
			s.mu.Unlock()
			s.b.log.Error("server_restart_failed", "server", s.spec.Name, "attempt", attempt,
				"error", err.Error(), "retry_in_seconds", delay.Seconds())
			continue
		}

		s.mu.RLock()
		was, downFor := s.down.cause, time.Since(s.down.since)
		s.mu.RUnlock()
		change, installed := s.b.revived(s, c, defs)
		if !installed {
			// Stopped while it was starting: the replacement goes too.
			_, _ = c.close(context.WithoutCancel(ctx))
			return false
		}
		s.b.log.Info("server_restarted", "server", s.spec.Name, "attempt", attempt,
			"down_seconds", downFor.Seconds(), "was", was, "tool_count", len(defs))
		if len(change.Added) > 0 || len(change.Removed) > 0 {
			s.b.log.Info("server_catalogue_changed", "server", s.spec.Name,
				"added", toolNames(change.Added), "removed", change.Removed)
			if changed := s.b.sup.CatalogueChanged; changed != nil {
				changed(ctx)
			}
		}
		return true
	}
}

// sleep waits d, and reports whether the supervision is still on. An owner's
// nudge ends the wait early and starts the schedule again.
func (s *server) sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	case <-s.nudged:
		s.mu.Lock()
		s.attempt = 0
		s.mu.Unlock()
		return true
	}
}

// up records a session that now serves the server. The caller holds the
// bridge's lock, which is what orders this against a Stop.
func (s *server) up(c *client) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last == nil {
		// The server's FIRST session, which a retried start can bring up as
		// well as its owner's. The attempts behind it — a start that failed
		// and the retries after it — were attempts to START the server, a
		// series that ends here; carried on, the first death of a server that
		// came up on its third try would wait the schedule's third step, where
		// one that came up on its first is restarted at once. Its first death
		// is its first either way.
		s.attempt = 0
	}
	if s.last != nil || s.failure != nil {
		// BROUGHT BACK: a session after the first, or a first one that
		// came up on a later attempt than the start that failed.
		s.restarts++
	}
	now := time.Now()
	s.client, s.last, s.down, s.upSince = c, c, nil, now
	// It listed its tools to come up: an answer.
	s.answered.Store(now.UnixNano())
	// A request for "now" that arrived while this start was running has
	// been answered by it.
	select {
	case <-s.nudged:
	default:
	}
}

// shutdown ends the supervision and the session, for good.
//
// IN THIS ORDER: stopped, so a call refuses for the right reason; the
// supervision cancelled, so an attempt in flight gives up its start at once
// rather than at a startup timeout; the supervisor waited for, so nothing can
// still be starting a child behind this call; and only then the session
// closed. Waiting for the supervisor is bounded by its own steps — a start
// that sees its context end, a dead session's shutdown ladder — never by the
// schedule.
func (s *server) shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.stopped = true
	s.mu.Unlock()
	s.cancel()
	<-s.done
	s.mu.Lock()
	c := s.client
	s.client = nil
	s.mu.Unlock()
	if c == nil {
		return nil
	}
	return c.stop(ctx)
}

// refusal is the answer to a call this server cannot take now.
//
// [RefusalUnavailable], although an MCP server's own failures carry no class,
// because this one is not the server's: the engine knows its session is gone,
// says so in its own words, and a person's surface may act on the class as on
// any other first-party refusal — not now, rather than no such thing.
//
// duringCall marks a call that was sent and got no answer because the session
// ended under it — whether the server died or its owner stopped it, which ends
// the session under a call in flight just the same. Whether such a call took
// effect nobody can say, so a tool not known to be a read is caused by
// [ErrOutcomeUnknown], and the sentence tells the model to look before it calls
// again rather than make a write twice. That comes FIRST, before why the server
// is not running: a stopped server's own sentence says the tool "cannot be
// called", which told a call that was already sent as one that never left the
// engine, and so invited the write again.
func (s *server) refusal(t *Tool, dead *client, duringCall bool) Result {
	s.mu.RLock()
	stopped, down := s.stopped, s.down
	var next time.Time
	var reason string
	if down != nil {
		next = down.next
		reason = down.cause
		if down.failed != "" {
			reason = "its restart failed: " + down.failed
		}
	}
	s.mu.RUnlock()

	name := fmt.Sprintf("%s/%s", s.spec.Name, t.raw)
	server := t.Server()
	const gone = "a configuration change replaced or removed it, or the seat it ran for was let go"
	if reason == "" && dead != nil {
		reason = dead.deathStory()
	}
	when := "it is being restarted now"
	if wait := time.Until(next); !next.IsZero() && wait > 0 {
		when = "its next restart is in about " + wait.Round(time.Second).String()
	}
	if duringCall {
		output := fmt.Sprintf("MCP tool %s got no answer: the %q server is not running — it "+
			"went down during this call (%s). The engine restarts it on its own; %s.",
			name, server, reason, when)
		if stopped {
			output = fmt.Sprintf("MCP tool %s got no answer: the %q server was stopped during "+
				"this call and is not running on this node any more — %s. Its tools are not "+
				"available in this turn.", name, server, gone)
		}
		res := Result{Output: output, Failed: true, Cause: RefusalUnavailable}
		if t.annotations.ReadOnly != Yes {
			res.Cause = ErrOutcomeUnknown
			res.Output += " The call was sent and no answer came back, so whether it took " +
				"effect is unknown: check before making it again."
		}
		return res
	}
	if stopped {
		return Result{
			Output: fmt.Sprintf("MCP tool %s cannot be called: the %q server is not running on "+
				"this node any more — %s. Its tools are not available in this turn.",
				name, server, gone),
			Failed: true,
			Cause:  RefusalUnavailable,
		}
	}
	return Result{
		Output: fmt.Sprintf("MCP tool %s cannot be called right now: the %q server is not "+
			"running (%s). The engine restarts it on its own; %s. None of its tools work until "+
			"it is back — carry on without them, or try again later.", name, server, reason, when),
		Failed: true,
		Cause:  RefusalUnavailable,
	}
}
