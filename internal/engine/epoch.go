package engine

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/configplane"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// The epoch and how a new one replaces it.
//
// A CONFIG REVISION IS AN IMMUTABLE EPOCH, AND APPLYING ONE PUBLISHES A NEW
// EPOCH — nothing is ever mutated in place. Applying a revision by
// mutating the live objects keeps their identity, so that anything holding a
// reference keeps working. That is
// precisely the problem: anything holding a reference kept working, and kept
// reading, mid-turn, values from two different revisions. A turn that read the
// budget cap before the swap and the model chain after it ran under a company
// that never existed, and nothing raised.
//
// Here a turn PINS the epoch once at the top and reads only that for the rest
// of the turn. A revision published mid-turn is simply not observed by that
// turn — the guarantee, not a limitation. The next turn gets it.

// epoch holds the current company.
//
// An atomic pointer and nothing else: readers never block, and there is no
// lock because there is only ever one writer — see [Engine.Apply].
type epoch struct {
	current atomic.Pointer[Company]
}

// Company is the epoch this engine is running.
//
// Every caller that needs more than one fact from it must take it ONCE and
// read that value, not call this twice: two calls can straddle a publish, and
// a caller that read the org from one epoch and the models from the next is
// running a company that never existed. That is the whole hazard this design
// exists to remove, and it is removable only at the call site.
func (e *Engine) Company() *Company { return e.epoch.current.Load() }

// RecheckGitHub asks the reconcile loop to look at GitHub now rather than
// waiting out its cadence.
//
// FOR THE MOMENT A PERSON FINISHES SOMETHING THERE. Installing an agent's App
// is a click at GitHub that this engine cannot perform and cannot be told
// about — an agent's App is private, so it sends no webhook here — so the
// card asking for it is owed to an admin, and an admin-owed surface backs off
// from fifteen seconds to ten minutes ([integration.Schedule]). The instant
// somebody DOES it is therefore the instant the wait is longest and least
// deserved: measured at an install completed in about eight seconds, followed
// by minutes of a card still asking for it.
//
// What the redirect supplies is only the timing. Nothing about it is
// believed and no installation id travels with the ask — see
// [integration.Worker.Refresh] — so the pass that follows is the ordinary
// verified one.
func (e *Engine) RecheckGitHub() {
	e.integrations.Refresh(integration.KindGitHub)
}

// installEpoch publishes an epoch and tells the store anything derived from it
// that the store cannot work out for itself.
//
// ONE FUNCTION for the two places an epoch becomes current — boot and apply —
// because state set in one and forgotten in the other fails silently and only
// on the path nobody exercised.
//
// Five things.
//
// THE PARTY REGISTRY, indexed BEFORE the epoch is stored, so no reader can
// find a company through [Engine.Company] whose parties are not in
// [Engine.Registry]. An apply indexes earlier still, before it rebuilds the
// vendor wiring that registers into the new registry, and that index is kept
// rather than rebuilt here. Boot has nothing to rebuild in between, and
// indexes here, before any fleet duty is armed; see [Engine.Registry] for why
// no reader may find the one without the other.
//
// And a store opened with NO width, which is a node that booted with no active
// revision. It holds no rows, and its first epoch is what tells it how wide
// its vectors will be. A store that already has a width keeps it
// ([store.DB.LearnEmbeddingDim] only ever raises from 0), because the width
// belongs to the rows in the file rather than to the current config, and
// [Engine.buildEmbedder] has already refused any revision that would change
// it.
//
// And the TOOL-SKILL TRIGGER AUDIT, which reads the epoch that is current
// and so runs once this one is; see [Engine.auditSkills] for why it takes
// no epoch.
//
// And the INBOX COALESCING KNOBS, written into the batch options every seat
// this node holds reads ([Engine.tuneBatching]). Here and not while the epoch
// is equipped, because that value is the node's rather than the epoch's: an
// apply refused after equipping would otherwise leave every inbox coalescing
// by numbers from a revision the node is not serving.
//
// And the TOOL SKILLS' ${var} MAP ([Engine.refreshSkillVariables]), which is
// config and so refreshed per epoch — unlike the skills themselves, which come
// from the knowledge base and outlive one — and here for the coalescing
// knobs' reason: the registry it is written into is the node's, and a refused
// revision's map would render every seat's skills while the node serves the
// epoch before. Written BEFORE the epoch is stored, so no turn on this epoch
// renders a skill with the map of the one before.
//
// The EMBEDDING BACKEND needs nothing here beyond the store below: it is part
// of the epoch ([Company]), so storing the epoch is what publishes it.
//
// It also tells the OPERATOR one thing: that an epoch with no model is now
// current. See nomodels.go.
func (e *Engine) installEpoch(c *Company) {
	if c != nil && !e.indexes(c) {
		e.refreshParties(c)
	}
	e.tuneBatching(c)
	if c != nil {
		e.refreshSkillVariables(c)
	}
	e.epoch.current.Store(c)
	if c != nil && c.Models == nil {
		// Said here, once per epoch, because it is the only line that
		// reaches the operator of a company nobody has messaged yet: with
		// no traffic there is no held delivery to log about.
		log.Warn("company_has_no_models", "company", c.Config.Name,
			"seats", len(c.Seats()),
			"detail", "the company configures no model provider: its seats are "+
				"placed and keep what arrives on their inboxes, but none takes a "+
				"turn until a revision adds one under providers.llm")
	}
	// Backends are always present on a running engine; a `crewlet validate`
	// engine applies to nothing and has no store to tell.
	if e.backends != nil && e.backends.Store != nil {
		e.backends.Store.LearnEmbeddingDim(embeddingWidth(c))
	}
	e.auditSkills()
}

// indexes reports whether the live party registry was built from exactly this
// company. By identity, because an epoch is published rather than mutated: a
// revision equal in every field is still a different epoch, with a registry of
// its own.
func (e *Engine) indexes(c *Company) bool {
	e.notify.mu.Lock()
	defer e.notify.mu.Unlock()
	return e.notify.registry != nil && e.notify.registryFor == c
}

// embeddingWidth is the vector width an epoch's embeddings provider produces,
// or 0 for a company that configures none.
//
// Zero is "no declared width to check against", not a width of zero and not an
// unknown: the store applies no dimension check at all against it. See
// [store.DB.LearnEmbeddingDim] for why a store that has one keeps it.
func embeddingWidth(c *Company) int {
	if c == nil || c.Config == nil || c.Config.Providers.Embeddings == nil {
		return 0
	}
	return c.Config.Providers.Embeddings.Width()
}

// Apply publishes a new epoch, reporting what happened to this node.
//
// TWO HALVES, and the line between them is the whole design. The BUILD
// constructs the new epoch — [NewCompanyWith] validates, resolves the org and
// constructs the providers without reaching the network; the sandbox manager
// and the epoch's own tools are built beside it — and touches nothing this
// node is serving. Every refusal a revision can earn happens there, so a
// refused revision leaves the previous epoch current and still correct, with
// no rollback because there was no mutation. The COMMIT changes the node:
// the party index, the inbound edge, the shared MCP children, the reflection
// workers, the sandbox manager, the epoch itself and everything read off it
// afterwards. Nothing in it refuses, with the one exception of a node's first
// company, whose two starts are refusals that take back what they started
// ([Engine.startEdge]).
//
// The three outcomes belong to the control plane, not to this function's
// convenience:
//
//   - ok      — published, and this node is serving it.
//   - error   — refused; this node still serves the PRIOR epoch correctly,
//     which is a legitimate degraded-but-correct state and safe to route to.
//   - degraded — the apply failed AFTER a subsystem that cannot be put back
//     was changed.
//
// DEGRADED IS NOT REACHABLE, and the ordering is what keeps it so. Every
// refusal a revision can earn on a node already serving a company happens in
// the build, before this node's state moves. A node's first company adds the
// two starts in [Engine.startEdge], each of which takes back what it started
// before it refuses. And every step after them that changes the node — a
// shared server stopped, a transport reconnected — runs where nothing can
// refuse any more. A step added to the commit that can fail must either be
// moved into the build or be one this function can take back, or degraded
// becomes reachable and this function has to report it.
//
// ONE CALLER: the reconciler's tick, which is synchronous. The API's write
// path does NOT reach here: it activates a revision and lets the tick apply
// it, because activation also has to move the pointer, record the outcome and
// reset the attempt budget, none of which this function does. So there is no
// second apply to exclude, and the lock taken below is not for one. It is for
// [Engine.Drain], the one other writer of what an apply builds: an apply that
// overlapped the drain and the teardown after it restarted the scheduler, the
// background passes and a first company's inbound edge after they had ended
// them. The drain waits for an apply in flight, and an apply that starts after
// it is refused with [errStopped].
//
// The second return is the stages this apply GOT THROUGH, in the order it
// went through them. A refused apply names the build stages it finished, and
// nothing a first company's start took back; it travels on
// ConfigRevisionApplied into the audit event log, where it outlives the fleet
// view's one-minute bucket.
func (e *Engine) Apply(ctx context.Context, cfg *config.Company) (configplane.ApplyStatus, []string, error) {
	e.applying.Lock()
	defer e.applying.Unlock()
	if e.stopped {
		return configplane.StatusError, nil, errStopped
	}
	var applied []string
	refuse := func(err error, detail string) (configplane.ApplyStatus, []string, error) {
		log.WarnContext(ctx, "config_apply_failed", "error", err, "detail", detail)
		return configplane.StatusError, applied, fmt.Errorf("engine: apply: %w", err)
	}

	// ---- THE BUILD: every refusal, and nothing this node serves moves ----

	// THE SNAPSHOT FIRST, because re-activating an unchanged revision is
	// the documented rotation gesture: the payload has not moved, so the
	// only thing that can have is what its ${VAR} references resolve to.
	// Rebuilding the epoch without re-reading the store would make that
	// gesture a no-op and rotation impossible without a restart. It is the
	// one write this half makes, and it is not the revision's: it is the
	// fleet's secret store as it stands, which the previous epoch resolves
	// through too, so a refusal has nothing of it to take back.
	e.refreshSecrets(ctx)
	applied = append(applied, "secrets")
	next, err := NewCompanyWith(cfg, e.resolver())
	if err != nil {
		return refuse(err, "the revision was refused before anything changed; "+
			"this node still serves the previous epoch")
	}
	applied = append(applied, "company")
	// THE SANDBOX MANAGER IS BUILT HERE AND INSTALLED WITH THE EPOCH. The
	// build is construction — it reaches no box and starts nothing — so a
	// revision whose provider block is broken is refused while nothing has
	// moved; the alternative serves a company whose sandbox-enabled seats
	// plan around a box that will never be minted. Only the manager: the
	// coordinator and the waiter hold this process's busy set and poll
	// loop, so rebuilding them would forget which seats are mid-run and
	// start a second loop against the same rows.
	var manager *sandbox.Manager
	if e.sandboxCoordinator != nil {
		if manager, err = buildSandbox(next.Config, e.resolver(), e.sandboxOtel); err != nil {
			return refuse(err, "the revision's providers.sandbox could not be "+
				"built; the previous epoch is still current")
		}
		applied = append(applied, "sandbox")
	}
	// EQUIPPED BEFORE IT IS PUBLISHED, for the same reason as at boot: a
	// turn can start the instant the pointer moves, and a revision that
	// silently dropped every builtin would look like a model that stopped
	// using its tools. The half that writes only into the epoch; the shared
	// MCP children, which are this node's processes, start in the commit.
	if err = e.equipEpoch(next); err != nil {
		return refuse(err, "the revision built but could not be equipped with "+
			"this node's tools; the previous epoch is still current")
	}
	applied = append(applied, "tools")

	// ---- THE COMMIT: this node's state moves, and nothing refuses but a
	// first company's two starts, which take back what they started ----

	// The party index is rebuilt BEFORE the epoch is published, and the
	// order is a choice between two brief windows. Refreshing first means
	// a seat the revision REMOVED stays addressable for an instant, which
	// costs a recorded skip. Refreshing after means a seat the revision
	// ADDED is unresolvable while the epoch that has it is already
	// current — and during a rollout the new company is the one being
	// adopted, so the window that favours it is the right one.
	attached, err := e.startEdge(ctx, next)
	if err != nil {
		return refuse(err, "this node's first company could not be started "+
			"here, and what the attempt started has been taken back; the "+
			"revision is not served here yet")
	}
	if attached {
		applied = append(applied, "learning")
	}
	applied = append(applied, "parties", "integrations")
	// AND WHAT THE LOOP LAST CONCLUDED IS NOW OLD NEWS. Its cadence is for
	// asking a third-party app again, not for asking this document again,
	// and the answer just changed here. See [integration.Worker.MarkStale].
	e.integrations.MarkStale()
	// The NATIVE backends' parsers are on the same edge and rebuilt for
	// the same reason. Their projectors, index, stores and feeds are NOT:
	// those follow a coordination family, which a company revision does
	// not change — see [Engine.reconcileNative].
	e.reconcileNative(ctx, next)
	// THE SHARED MCP SERVERS, into the new epoch's registry, because this is
	// the surface every seat's is cloned from. Here and not in the build:
	// reconciling them stops the children the revision dropped, and a
	// refusal after that would leave the served epoch's registry naming the
	// tools of servers that are gone. Per-role children are NOT here: they
	// belong to a seat's lease rather than to the epoch — see mcp.go.
	//
	// A server that will not start does not fail the apply. It costs that
	// server's tools; refusing the epoch over it would take a working
	// company down because one vendor's binary was missing.
	e.startSharedServers(ctx, next)
	applied = append(applied, "mcp_servers")
	// THE REFLECTION WORKERS, handed over immediately before the install. The
	// dispatcher reflects every completed turn with whatever workers it
	// holds, so a turn completing on the new epoch needs them the moment it
	// is current — and any earlier, a turn the served epoch completed would
	// be reflected by a revision it did not run. A first company's
	// dispatcher was attached with them by [Engine.startEdge].
	if !attached {
		// A SWAP, and it does not refuse. [Engine.startEdge] made this
		// node's attach attempt and refused the apply when it failed, so
		// what is left is handing a dispatcher this revision's workers —
		// and a worker set it will not take is logged inside the call,
		// with the previous revision's workers kept serving. An error
		// returned here can only be an attach, and it is logged rather
		// than refused because this node's state has already moved.
		if err = e.reconfigureReflection(ctx, next); err != nil {
			log.ErrorContext(ctx, "reflection_attach_failed", "error", err,
				"detail", "no reflect dispatcher is attached on this node, so "+
					"no completed turn is reflected on until an apply attaches one")
		}
		applied = append(applied, "learning")
	}
	if manager != nil {
		e.sandboxCoordinator.SetManager(manager)
	}

	previous := e.Company()
	e.installEpoch(next)
	applied = append(applied, "epoch")

	// THE TOOL SKILLS' SOURCE, after the knowledge base's own reconcile
	// above, because the Confluence source is read off the wiring it left
	// running — and after the install, because the walk the source change
	// asks for checks every skill against the ${var} map the install wrote
	// ([Engine.installEpoch]), which before it is the previous revision's.
	// See [Engine.reconcileSkills].
	e.reconcileSkills(next)
	applied = append(applied, "skills")

	// AFTER the epoch is published, because a seat registry is a clone of
	// the CURRENT company's surface: rebuilding from `next` before it is
	// current would hand every held seat the new revision's builtins while
	// its turns still read the outgoing epoch. The per-role CHILDREN are
	// deliberately untouched: they belong to the seat's lease, not to the
	// epoch.
	e.refileSeatTools(ctx, next)
	applied = append(applied, "seat_tools")

	// AFTER the epoch is published, because this reads the seat list off
	// the CURRENT company: a revision that adds a role adds a seat, and
	// until something creates its mailbox every event published to it is
	// dropped rather than retained. Nil on an engine built without a node
	// — `crewlet validate` applies to nothing.
	if e.node != nil {
		e.node.EnsureMailboxes(ctx)
		// AND THE MAIL A COMPANY WITH NO MODEL HELD BACK is let through
		// once this epoch has one. Here, after the seat tools are refiled,
		// because the first thing a released inbox does is run a turn, and
		// that turn must find everything it reads already current.
		if next.Models != nil {
			e.releaseModelHolds(ctx)
		}
		applied = append(applied, "mailboxes")
	}
	// THE BACKGROUND PASSES follow the revision too, and after the swap:
	// their loops walk the CURRENT epoch's roster, and the passes handed to
	// them hold this revision's models and knobs. The loops keep running
	// and keep their clocks. See [Engine.reconfigureLearningPasses].
	e.reconfigureLearningPasses(ctx, next)
	applied = append(applied, "learning_passes")
	// AFTER the epoch is published too, and for a sharper version of the
	// same reason: the tick reads schedules off the CURRENT company, so
	// arming from `next` before it is current would open a window in which
	// the loop fires the outgoing company's crons. A founder's first
	// schedule starts the loop here; their last one removed stops it.
	e.reconcileScheduler(ctx, next)
	applied = append(applied, "scheduler")

	log.InfoContext(ctx, "config_applied",
		"company", next.Config.Name, "seats", len(next.Seats()),
		"previous_seats", seatCount(previous))
	// LAST, after the epoch is current and everything derived from it has
	// been rebuilt, so a surface that reads the company on this signal
	// reads the one now serving rather than the one being replaced.
	e.notifyApplied(ctx)
	return configplane.StatusOK, applied, nil
}

// errStopped refuses an apply that reaches a node after [Engine.Drain] began.
// The node is leaving, and nothing it would build for the revision could run.
var errStopped = errors.New("engine: apply: this node is stopping and applies no revision")

// seatCount reports a possibly-absent epoch's seat count, for the log line
// that says what changed. The first apply on a node has no previous epoch.
func seatCount(c *Company) int {
	if c == nil {
		return 0
	}
	return len(c.Seats())
}

// startEdge brings the party index, the inbound edge and — on a node's first
// company — the reflect dispatcher to the revision an apply is committing,
// reporting whether it attached the dispatcher.
//
// A NODE SERVING A COMPANY reconciles: the index is rebuilt and every inbound
// surface brought in line, and none of it refuses. A NODE'S FIRST COMPANY
// starts what later ones reconfigure — the dispatcher that reflects on
// completed turns, then the edge that routes deliveries — and either start
// can fail. Each failure refuses the apply like a refused build, because a
// company served without either looks healthy and learns or hears nothing;
// so a start that fails takes back what this call did before it, and the
// retry the refusal earns starts from nothing.
//
// THE DISPATCHER FIRST, because it is the one of the two that can be taken
// back: its subscription is one the queue detaches ([Engine.detachReflection]),
// while the edge's service, once started, runs for the life of the process.
// [Engine.startInbound] takes down what a start of its own that failed brought
// up.
//
// THE INDEX BEFORE THE EDGE, because the transports the edge starts register
// the identities they resolve into it — so it is rebuilt here for a first
// company too, and put back if the edge will not start.
func (e *Engine) startEdge(ctx context.Context, next *Company) (attached bool, err error) {
	if e.reflector == nil {
		if err := e.reconfigureReflection(ctx, next); err != nil {
			return false, err
		}
		attached = e.reflector != nil
	}
	index := e.partyIndex()
	e.refreshParties(next)
	if e.inboundStarted() {
		e.reconcileInbound(ctx, next)
		return attached, nil
	}
	if err := e.startInbound(ctx, next); err != nil {
		e.restoreParties(index)
		if attached {
			e.detachReflection(ctx)
		}
		return false, err
	}
	return attached, nil
}

// reconcileInbound brings every surface of a running inbound edge in line with
// a revision. None of them refuses: a surface whose new wiring does not build
// keeps its previous one and says so in its own log line.
func (e *Engine) reconcileInbound(ctx context.Context, next *Company) {
	// The TRACKER is rebuilt on the same edge as the party index and for the
	// same reason: its lead map is derived from the org, so a node that kept
	// its boot-time parser would route the new revision's work items by the
	// old company's org chart.
	e.reconcileConfluence(next)
	e.reconcileDatadog(ctx, next)
	e.reconcileJira(ctx, next)
	e.reconcileGitLab(ctx, next)
	e.reconcileGitHub(ctx, next)
	// AND THE TWO CHAT SURFACES: their parsers are assembled when the edge
	// starts, so without these a company that connected either one after
	// its node started would have every delivery verified at the edge and
	// routed to nobody until the process restarted. See
	// [Engine.reconcileSlack] for why one rebuilds unconditionally and the
	// other does not.
	e.reconcileSlack(ctx, next)
	e.reconcileMattermost(ctx, next)
}

// partyIndex is the party registry as it stands, and the company it indexes.
type partyIndex struct {
	registry *notify.Registry
	of       *Company
}

// partyIndex reads the live index, for a first company's start to put back if
// it refuses.
func (e *Engine) partyIndex() partyIndex {
	e.notify.mu.Lock()
	defer e.notify.mu.Unlock()
	return partyIndex{registry: e.notify.registry, of: e.notify.registryFor}
}

// restoreParties puts back an index [Engine.partyIndex] read.
func (e *Engine) restoreParties(p partyIndex) {
	e.notify.mu.Lock()
	defer e.notify.mu.Unlock()
	e.notify.registry, e.notify.registryFor = p.registry, p.of
}

// detachReflection takes back a reflect dispatcher [Engine.startEdge] attached
// for a first company whose edge then would not start.
//
// DETACHED, not deleted: the subscription is the fleet's group, which every
// node's dispatcher consumes, so only this process's consumer on it is
// closed. The dispatcher is dropped whatever the queue answers, because both
// queue backends refuse a detach only when the broker is not running, which
// holds no consumer to leave behind — and a dispatcher kept after its
// subscription went would have the retry swap workers into something nothing
// feeds.
func (e *Engine) detachReflection(ctx context.Context) {
	if e.reflector == nil {
		return
	}
	e.reflector = nil
	if e.backends == nil || e.backends.Queue == nil {
		return
	}
	if _, err := e.backends.Queue.Detach(context.WithoutCancel(ctx),
		topics.Event(types.TurnCompleted{}.EventType()), learning.ReflectGroup); err != nil {
		log.WarnContext(ctx, "reflection_detach_failed", "error", err,
			"detail", "the dispatcher attached for a company this node then "+
				"refused could not be detached")
	}
}
