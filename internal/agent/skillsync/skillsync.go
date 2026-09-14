// Package skillsync keeps one node's tool-skill registry converged on the
// knowledge backend its company's skills live in, and a fleet's registries
// converged on each other.
//
// # Four ways a registry used to go stale, and the one loop that closes them
//
// The registry is per NODE and its content comes from a wiki, so it is only
// ever as current as the last thing that re-read the wiki on that node. Four
// paths left it behind:
//
//   - A PAGE WEBHOOK REACHES ONE NODE. Inbound deliveries are a fleet-wide
//     consumer group, so exactly one member parses each one, and every other
//     node kept the skill as it was until it restarted.
//   - A FAILED WALK WAS FINAL. The boot walk ran once; a wiki that was down
//     for that one request left the node with no skills until a page edit or
//     a restart.
//   - AN APPLY NEVER RE-WALKED. Connecting the knowledge backend live loaded
//     no skills, moving the skills container served the old one's, and
//     disconnecting it (or turning skills off) left every skill registered.
//   - A SINGLE-PAGE CHANGE RE-WALKED THE WHOLE CONTAINER on the node that heard
//     it, spending a request per page of the space on every edit.
//
// So one [Syncer] per node owns the registry's content, and everything that can
// change it is a request to that loop: [Syncer.SetSource] on every apply,
// [Syncer.PageChanged] for a delivery this node won, a [types.ToolSkillPageChanged]
// broadcast for one a peer won, [Syncer.Refresh] from a backend that can only say
// "something moved", a periodic walk behind all of them, and a bounded retry
// behind a walk that failed.
//
// # One goroutine does the reading, and that is what makes it correct
//
// Every walk and every page read runs on the loop's own goroutine, one at a
// time. A later operation therefore always reads later state than an earlier
// one, so a slow read can never land on top of a newer one, and nothing here
// needs a version comparison to know which answer is current.
//
// # A page update is the same answer as a walk
//
// A change names a page, and the registry keeps what each page holds (see
// [skills.Registry]), so reading the one page that moved ends exactly where a
// walk of the whole container would. That is what lets the periodic walk run
// behind the event path without moving guidance under a seat that already
// read it.
package skillsync

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/agent/skills"
	"github.com/crewlet/crewlet/internal/backoff"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/tracing"
)

var log = logging.Get("agent.skillsync")

const (
	// RewalkInterval is how often a node walks its skills container with
	// nothing asking it to.
	//
	// It is the convergence bound for everything the event path misses: a
	// nudge a node did not hear (a broker reconnect, a subscription that
	// failed to attach), a webhook the wiki never delivered, and the changes
	// no subscribed event names at all (a page moved into or out of the
	// container, a page restored from the trash). Ten minutes keeps a guidance
	// edit that took one of those paths inside the working session in which
	// somebody made it, and a walk is cheap at that rate: a skills container
	// is tens of pages, one listing request of up to a hundred, so a ten-node
	// fleet spends sixty requests an hour of the org credential that the
	// knowledge search already spends a request per turn on.
	RewalkInterval = 10 * time.Minute

	// RetryBase is how long a node waits before retrying a walk that failed,
	// doubled per consecutive failure up to [RewalkInterval].
	//
	// Five seconds is sized for the two failures that clear on their own: a
	// wiki whose API comes up seconds after the engine does (a compose stack
	// booting together), and a rate-limit refusal whose Retry-After is
	// seconds. Doubling reaches the ceiling on the eighth attempt, about ten
	// minutes after the first failure, and from there a source that stays
	// broken costs exactly what a healthy one does: one walk per interval.
	RetryBase = 5 * time.Second

	// jitterFraction spreads every scheduled walk across a fifth of its wait
	// either way, the spread the config reconcile loop and the sandbox
	// waiter use, so a fleet that booted together does not walk (or retry
	// against a wiki that is recovering) on the same second.
	jitterFraction = 0.2
)

// Source is where one epoch's skills come from.
//
// Its IDENTITY is Backend, Container and Location, and nothing else: an
// apply that rebuilt the backend's client around a rotated credential names
// the same source with fresh functions, and must not re-walk or empty
// anything for it.
type Source struct {
	// Backend names the knowledge backend ("confluence", "native").
	Backend string

	// Container is the skills container's key. Empty turns tool skills
	// off: a company with no knowledge backend, or one that set its skills
	// container to the empty string.
	Container string

	// Location tells two instances of one backend apart (a Confluence
	// base URL). Empty for a backend there is only ever one of.
	Location string

	// Walk reads every page in the container, and must fail rather than
	// return a partial list: the registry replaces wholesale, so a
	// truncated walk silently deletes every skill it did not reach. It may
	// block until the backend is able to answer, and it is cancelled when
	// the source changes or the loop stops.
	//
	// Nil means this node cannot read the source at all, with Unreadable
	// saying why.
	Walk func(ctx context.Context, container string) ([]skills.Page, error)

	// Page reads one page. Nil means the backend has no single-page read,
	// and a page change then walks the container instead.
	Page func(ctx context.Context, id string) (PageRead, error)

	// Unreadable says why Walk is nil, in words an operator can act on.
	Unreadable string
}

// same reports whether two sources name the same skills.
func (s Source) same(other Source) bool {
	return strings.EqualFold(s.Backend, other.Backend) &&
		strings.EqualFold(s.Container, other.Container) &&
		s.Location == other.Location
}

// off reports whether the source turns tool skills off.
func (s Source) off() bool { return strings.TrimSpace(s.Container) == "" }

// walkable reports whether a walk of the source can run on this node.
func (s Source) walkable() bool { return !s.off() && s.Walk != nil }

// PageRead is one page as a single-page read found it.
type PageRead struct {
	// Page is the page's content. Its ID is overwritten with the id that
	// was asked for, which is the one the change named.
	Page skills.Page

	// Container is where the page lives now. A page that moved out of the
	// skills container is dropped.
	Container string

	// Exists is false for a page the backend no longer has, deleted or in
	// the trash.
	Exists bool
}

// Change is one page that moved in a knowledge backend.
type Change struct {
	// Backend is the backend the page lives in.
	Backend string

	// Container is the container the delivery named the page in, or empty
	// when it did not say.
	Container string

	// PageID is the backend's id for the page.
	PageID string

	// Removed is true for a page that was deleted or trashed.
	Removed bool
}

// Stream is the event-stream surface a fleet nudge needs: publishing one, and
// hearing everybody else's.
//
// The subscription is an EPHEMERAL BROADCAST, never a consumer group: every
// node has to hear every nudge, and a competing group would hand each one to
// exactly one node, which is the delivery shape that caused the divergence.
type Stream interface {
	queue.Publisher
	SubscribeStream(ctx context.Context, topicPattern string, h queue.StreamHandler) (queue.Unsubscribe, error)
}

// Options configure a [Syncer].
type Options struct {
	// Registry is the registry this loop keeps. Required.
	Registry *skills.Registry

	// Node is this node's id, stamped on the nudges it publishes so it can
	// recognise, and skip, its own.
	Node string

	// Stream carries the fleet nudge. Nil is a node with no broker, which
	// holds the only registry there is and has nobody to tell.
	Stream Stream

	// OnChange runs after the loop changed the registry, for the audit that
	// checks every skill's trigger against the company's tools. Nil does
	// nothing.
	OnChange func()
}

// Syncer is one node's tool-skill sync loop.
type Syncer struct {
	registry *skills.Registry
	node     string
	stream   Stream
	onChange func()

	// interval and retryBase are [RewalkInterval] and [RetryBase]. Fields
	// only so this package's own tests can run a retry in milliseconds.
	interval  time.Duration
	retryBase time.Duration

	mu sync.Mutex

	// source is the one the current epoch names, and generation counts
	// changes of its identity. A read that started under an older
	// generation answers for a source nobody asked about any more, and is
	// discarded.
	source     Source
	generation uint64

	// walkDue asks the loop for a complete walk. pending is page changes
	// the loop has not read yet, one per page, the latest winning.
	walkDue bool
	pending map[string]Change

	// synced is whether the last walk of this generation succeeded, and
	// failures how many walks have failed in a row. A registry that has not
	// synced is not trusted to converge one page at a time: only a
	// complete walk can, so page changes wait for (or are covered by) it.
	synced   bool
	failures int

	// cancelRead ends the read in flight, which a source change does.
	cancelRead context.CancelFunc

	// readable remembers whether the source could be read when it was last
	// set, so an unreadable source is reported when it BECOMES unreadable
	// rather than on every apply.
	readable bool

	wake        chan struct{}
	stop        context.CancelFunc
	done        chan struct{}
	unsubscribe queue.Unsubscribe
}

// New builds a stopped loop over a registry. [Syncer.SetSource] and the other
// requests are safe before [Syncer.Start]; they take effect once it runs.
func New(opts Options) (*Syncer, error) {
	if opts.Registry == nil {
		return nil, errors.New("skillsync: a sync loop needs the registry it keeps")
	}
	return &Syncer{
		registry: opts.Registry, node: opts.Node, stream: opts.Stream,
		onChange: opts.OnChange,
		interval: RewalkInterval, retryBase: RetryBase,
		pending: map[string]Change{},
		wake:    make(chan struct{}, 1),
	}, nil
}

// nudgeTopic is where a page change is broadcast.
var nudgeTopic = topics.Event(types.ToolSkillPageChanged{}.EventType())

// Start runs the loop and attaches the fleet nudge.
//
// A NUDGE THAT WILL NOT ATTACH IS NOT FATAL, and saying so is the design: the
// periodic walk is the authoritative path, so a node that cannot hear its
// peers converges one interval later rather than not at all.
//
// The loop runs DETACHED from ctx, like every long-running loop on a node: one
// bound to a signal context would stop at SIGTERM, while seats that still read
// the registry are draining. [Syncer.Stop] is what ends it.
func (s *Syncer) Start(ctx context.Context) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.done != nil {
		s.mu.Unlock()
		return
	}
	loop, stop := context.WithCancel(context.WithoutCancel(ctx))
	s.stop, s.done = stop, make(chan struct{})
	s.mu.Unlock()

	if s.stream != nil {
		unsubscribe, err := s.stream.SubscribeStream(loop, nudgeTopic, s.hear)
		if err != nil {
			log.WarnContext(ctx, "tool_skill_nudge_unavailable", "error", err.Error(),
				"detail", "this node converges on its periodic skill walk instead "+
					"of on its peers' page changes")
		} else {
			s.mu.Lock()
			s.unsubscribe = unsubscribe
			s.mu.Unlock()
		}
	}
	go s.run(loop)
}

// Stop ends the loop, waiting for a read in flight to observe the cancellation.
func (s *Syncer) Stop(ctx context.Context) {
	if s == nil {
		return
	}
	s.mu.Lock()
	stop, done, unsubscribe := s.stop, s.done, s.unsubscribe
	s.unsubscribe = nil
	s.mu.Unlock()
	if stop == nil {
		return
	}
	if unsubscribe != nil {
		// WithoutCancel: a stop is routinely driven by the cancellation it
		// is cleaning up after, and an unsubscribe that inherited it would
		// leave the subscription behind on the broker.
		if err := unsubscribe(context.WithoutCancel(ctx)); err != nil {
			log.WarnContext(ctx, "tool_skill_nudge_unsubscribe_failed", "error", err.Error())
		}
	}
	stop()
	<-done
}

// SetSource installs the source the current epoch names. Called at boot and on
// every apply.
//
// A CHANGED SOURCE EMPTIES THE REGISTRY AT ONCE, before the new one is walked.
// The registry never serves a source the epoch no longer names: the old
// container's pages are ordinary pages now (no longer excluded from search or
// routing), a disconnected wiki's skills describe a stack the company stopped
// running, and keeping them until a walk of the new source succeeds would keep
// them indefinitely whenever it cannot, which is the exact shape of the bug
// this replaced. A walk of the new source starts immediately, so the gap is
// one request long on the ordinary path.
//
// AN UNCHANGED SOURCE RE-WALKS ONLY IF THE LAST WALK FAILED, OR IF THIS NODE
// COULD NOT READ IT UNTIL NOW. An apply is the gesture that fixes a broken
// credential, so it is the moment to try again; an apply that changed a seat's
// model has nothing to say about skills. A source that was unreadable is
// walked even when its last walk succeeded, because nothing else would bring
// it up to date: every page change that arrived while it could not be read was
// dropped, and the periodic walk is re-armed only by a walk, so one that came
// due while there was nothing to read is not due again until something walks.
func (s *Syncer) SetSource(src Source) {
	if s == nil {
		return
	}
	s.mu.Lock()
	previous := s.source
	changed := !previous.same(src)
	wasReadable := s.readable
	s.source = src
	s.readable = src.walkable()
	if changed {
		s.generation++
		if s.cancelRead != nil {
			s.cancelRead()
			s.cancelRead = nil
		}
		s.synced, s.failures = false, 0
		clear(s.pending)
		if s.registry.Len() > 0 {
			s.registry.Replace(nil)
			log.Info("tool_skills_retired",
				"previous_backend", previous.Backend,
				"previous_container", previous.Container,
				"backend", src.Backend, "container", src.Container,
				"detail", "the applied company names a different skills source, "+
					"so the previous source's skills are no longer served")
		}
	}
	switch {
	case src.off():
		s.walkDue = false
		if changed {
			log.Info("tool_skills_off",
				"detail", "the applied company has no tool-skills container, so "+
					"no skill is served")
		}
	case src.Walk == nil:
		s.walkDue = false
		if changed || wasReadable {
			log.Error("tool_skill_source_unreadable", "backend", src.Backend,
				"container", src.Container, "reason", src.Unreadable,
				"detail", "this node cannot read its company's tool skills: it "+
					"keeps serving what it last read (nothing, for a source it "+
					"never read) and takes no change until the reason above is "+
					"fixed and the configuration is applied again")
		}
	case changed || !s.synced || !wasReadable:
		s.walkDue = true
	}
	s.mu.Unlock()
	s.signal()
}

// Refresh asks for a complete walk of the current source, from a backend that
// can say something moved in its container but not which page.
//
// A REFRESH FROM ANOTHER BACKEND IS IGNORED. The native page projection runs
// for the life of the node, so a company that moved its skills to Confluence
// by an apply still has a projection reporting native skill pages, and a
// native commit is no reason to walk a different wiki. Coalesced: however
// many arrive before the loop runs, it walks once.
func (s *Syncer) Refresh(backend string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.source.walkable() && strings.EqualFold(s.source.Backend, backend) {
		s.walkDue = true
	}
	s.mu.Unlock()
	s.signal()
}

// PageChanged records a page change this node heard from the backend, and
// tells the fleet.
//
// Called by the node that won a delivery. It applies the change locally rather
// than waiting to hear its own broadcast, so a broker that is down costs the
// fleet its nudge and never this node its own edit.
//
// A change that cannot concern the registry is dropped here, before anything
// is read or published: a wiki's every edit in every space reaches this call,
// and only the skills container's (or a page that just left it) matter.
//
// The error is the publish's, and it is informational: the change has already
// been applied here, and a peer that missed the nudge converges on its
// periodic walk.
func (s *Syncer) PageChanged(ctx context.Context, change Change) error {
	if s == nil || !s.concerns(change) {
		return nil
	}
	s.note(change)
	if s.stream == nil {
		return nil
	}
	ev := events.New(types.ToolSkillPageChanged{
		Backend: change.Backend, Container: change.Container,
		PageID: change.PageID, Removed: change.Removed,
	}, tracing.TraceOf(ctx))
	ev.Source = s.node
	if err := s.stream.Publish(ctx, nudgeTopic, ev); err != nil {
		return fmt.Errorf("skillsync: page %s changed here and the fleet was not "+
			"told, so peers pick it up on their next periodic walk: %w",
			change.PageID, err)
	}
	return nil
}

// hear is the fleet nudge's handler.
func (s *Syncer) hear(ctx context.Context, _ string, ev *events.Event) {
	if ev == nil || (s.node != "" && ev.Source == s.node) {
		// OUR OWN, already applied by PageChanged before it was published.
		return
	}
	payload, ok := events.DataAs[*types.ToolSkillPageChanged](ev)
	if !ok {
		log.DebugContext(ctx, "tool_skill_nudge_undecodable", "event", ev.ID.String(),
			"detail", "the nudge carried no page, so it is ignored and the "+
				"periodic walk covers whatever it was about")
		return
	}
	change := Change{
		Backend: payload.Backend, Container: payload.Container,
		PageID: payload.PageID, Removed: payload.Removed,
	}
	if s.concerns(change) {
		s.note(change)
	}
}

// concerns reports whether a change could move this node's registry.
//
// The container a delivery names is where the page is NOW, so a page that
// just left the skills container is announced from somewhere else; the page
// being registered is how that announcement is still recognised.
func (s *Syncer) concerns(change Change) bool {
	if strings.TrimSpace(change.PageID) == "" {
		return false
	}
	s.mu.Lock()
	src := s.source
	s.mu.Unlock()
	if src.off() || !strings.EqualFold(change.Backend, src.Backend) {
		return false
	}
	if change.Container == "" || strings.EqualFold(change.Container, src.Container) {
		return true
	}
	return s.registry.HoldsPage(change.PageID)
}

// note queues a page change for the loop.
func (s *Syncer) note(change Change) {
	s.mu.Lock()
	s.pending[change.PageID] = change
	s.mu.Unlock()
	s.signal()
}

// signal wakes the loop. A wake already pending is enough: the loop drains
// every request it finds, so a second wake would find nothing new.
func (s *Syncer) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Syncer) run(ctx context.Context) {
	defer close(s.done)
	timer := time.NewTimer(s.interval)
	timer.Stop()
	defer timer.Stop()
	for {
		if next, ok := s.drain(ctx); ok {
			// Stopped but NOT drained before the reset. Since Go 1.23 a
			// timer's channel is synchronised with Stop and Reset, so no
			// stale fire can be sitting in it.
			timer.Reset(next)
		}
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		case <-timer.C:
			s.mu.Lock()
			if s.source.walkable() {
				s.walkDue = true
			}
			s.mu.Unlock()
		}
	}
}

// drain does every request the loop holds, and reports when the next walk is
// due if anything it did scheduled one.
func (s *Syncer) drain(ctx context.Context) (time.Duration, bool) {
	var next time.Duration
	scheduled := false
	for ctx.Err() == nil {
		s.mu.Lock()
		src, generation := s.source, s.generation
		switch {
		case s.walkDue && src.walkable():
			s.walkDue = false
			// EVERY PAGE CHANGE NOTED SO FAR IS COVERED by a walk that
			// starts after it. One noted while the walk is running is
			// not, which is why this clears here and not when the walk
			// returns.
			clear(s.pending)
			read, cancel := context.WithCancel(ctx)
			s.cancelRead = cancel
			s.mu.Unlock()

			pages, err := src.Walk(read, src.Container)
			cancel()
			if wait, ok := s.walked(ctx, generation, src, pages, err); ok {
				next, scheduled = wait, true
			}

		case len(s.pending) > 0 && s.synced:
			id := slices.Min(slices.Collect(maps.Keys(s.pending)))
			change := s.pending[id]
			delete(s.pending, id)
			read, cancel := context.WithCancel(ctx)
			s.cancelRead = cancel
			s.mu.Unlock()

			s.applyChange(read, generation, src, change)
			cancel()

		default:
			// NOTHING THIS LOOP CAN ACT ON. Page changes held by a
			// registry that has not synced are covered by the walk that
			// is scheduled (or by the next apply, for a source this node
			// cannot read), so they are dropped rather than left to be
			// read one at a time against a registry that is not current.
			clear(s.pending)
			s.cancelRead = nil
			s.mu.Unlock()
			return next, scheduled
		}
	}
	return next, scheduled
}

// walked installs a walk's result, and reports how long until the next walk.
func (s *Syncer) walked(ctx context.Context, generation uint64, src Source,
	pages []skills.Page, err error,
) (time.Duration, bool) {
	s.mu.Lock()
	if generation != s.generation {
		// SUPERSEDED. The source changed while this walk ran, so its answer
		// is for a container the epoch no longer names, and the change
		// already asked for a walk of the one it does.
		s.mu.Unlock()
		return 0, false
	}
	s.cancelRead = nil
	if err != nil {
		if ctx.Err() != nil {
			s.mu.Unlock()
			return 0, false
		}
		s.synced = false
		s.failures++
		failures := s.failures
		wait := backoff.Doubling(failures, s.retryBase, s.interval)
		s.mu.Unlock()
		log.ErrorContext(ctx, "tool_skill_sync_failed", "backend", src.Backend,
			"container", src.Container, "attempt", failures,
			"retry_in", wait.String(), "error", err.Error(),
			"detail", "the registry keeps what it already held, and the walk is "+
				"retried; seats run without any skill it has not loaded yet")
		return backoff.Jitter(wait, jitterFraction), true
	}
	// UNDER THE LOCK, so a source change cannot land between the generation
	// check above and the replace: it would empty the registry and then
	// watch this walk put the retired source's skills back.
	admitted, report := skills.Admit(pages)
	s.registry.Replace(admitted)
	recovered := s.failures > 0
	s.synced, s.failures = true, 0
	s.mu.Unlock()

	log.InfoContext(ctx, "tool_skills_synced", "backend", src.Backend,
		"container", src.Container, "skills", len(admitted),
		"pages", report.Pages, "not_skills", report.Ordinary,
		"undecodable", len(report.Undecodable), "recovered", recovered)
	s.changed()
	return backoff.Jitter(s.interval, jitterFraction), true
}

// applyChange reads one changed page into the registry.
func (s *Syncer) applyChange(ctx context.Context, generation uint64, src Source, change Change) {
	if !change.Removed && src.Page == nil {
		s.walkInstead(generation)
		return
	}
	read := PageRead{Exists: false}
	if !change.Removed {
		var err error
		if read, err = src.Page(ctx, change.PageID); err != nil {
			if ctx.Err() != nil {
				return
			}
			// A WALK, not a retry of this page. A read that failed is
			// most often a wiki that is down, and a walk is what the
			// failure path already knows how to back off; it also covers
			// every other change that arrives before the wiki answers.
			log.WarnContext(ctx, "tool_skill_page_read_failed", "page", change.PageID,
				"error", err.Error(),
				"detail", "the whole container is walked instead")
			s.walkInstead(generation)
			return
		}
	}

	s.mu.Lock()
	if generation != s.generation {
		s.mu.Unlock()
		return
	}
	var result skills.PageChange
	if read.Exists && strings.EqualFold(read.Container, src.Container) {
		read.Page.ID = change.PageID
		if skill, verdict := skills.AdmitPage(read.Page); verdict == skills.PageAdmitted {
			var err error
			if result, err = s.registry.PutPage(skill); err != nil {
				// ADMISSION ACCEPTED A SKILL THE REGISTRY REFUSES, which
				// no page can cause (a parsed skill is validated, and its
				// page id is the non-empty one this change named): the two
				// tests disagreeing is a defect in this build. The page is
				// dropped, as a walk's replace drops the same skill, and
				// said so loudly rather than served half-checked.
				log.ErrorContext(ctx, "tool_skill_page_refused", "page", change.PageID,
					"skill", skill.Key, "error", err.Error(),
					"detail", "the page was admitted as a skill and the registry "+
						"refused it, so it is not served; report this as a bug")
				result = s.registry.DropPage(change.PageID)
			}
		} else {
			result = s.registry.DropPage(change.PageID)
		}
	} else {
		result = s.registry.DropPage(change.PageID)
	}
	s.mu.Unlock()

	log.InfoContext(ctx, "tool_skill_page_synced", "backend", src.Backend,
		"container", src.Container, "page", change.PageID,
		"removed", change.Removed, "skill_before", result.Before,
		"skill_after", result.After, "shadowed", result.Shadowed)
	s.changed()
}

// walkInstead asks for a walk on behalf of a page change that could not be
// read on its own.
func (s *Syncer) walkInstead(generation uint64) {
	s.mu.Lock()
	if generation == s.generation && s.source.walkable() {
		s.walkDue = true
	}
	s.mu.Unlock()
}

// changed runs the caller's hook after the registry moved.
func (s *Syncer) changed() {
	if s.onChange != nil {
		s.onChange()
	}
}
