package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/accessdomain"
	"github.com/crewlet/crewlet/internal/advisordomain"
	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/assistantdomain"
	"github.com/crewlet/crewlet/internal/atlasdomain"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE CORE RUNTIME: what every DATA node runs from its first second, with a
// company or without one — the state log for EVERY registered domain, the node
// gate, the identity estate's two sides and the directory trigger — published
// once by [New]. The [native] half, whose existence only a company decides,
// waits for the first company. Split by what depends on a company, never by
// domain: a node with no company must still sign in the Tier A token's session
// the company is created through, and a node running part of the register
// could neither join, donate a snapshot nor be counted by the trim. A node
// without `data` starts no core and asks a data node through the estate's
// router ([routedIdentity]) (package doc, "The core runtime").

// core holds this node's always-on runtime.
//
// Every field is written once, by [Engine.startCore], before the struct is
// published to [Engine.core], so it needs no mutex; what runs afterwards
// synchronises itself (the wait group the directory trigger joins through).
type core struct {
	// nodeID names this node's own domain consumers, the writer stamped on its
	// records and the eviction gate's key; held because the native half, the
	// maintenance jobs and the reanchor guard need it and the bootstrap is
	// not kept.
	nodeID string

	// log is the state-log runtime for every registered domain (file comment).
	log *stateLog

	// gate is eviction and readmission over every identity-claiming log: per
	// node and built with the log, never gated on a company or a backend, since
	// a node that has met no company still has logs the trim counts it on.
	gate *NodeGate

	// iamReader and iamWriter are the identity estate's two sides, built on
	// every data node with or without a company: a node with no company must
	// sign in the Tier A token's session the company is created through, and a
	// domain that failed to come up is a boot failure, never a nil to branch
	// on. Only the seat a person binds to waits for the company; the writer
	// checks it.
	iamReader *iamdomain.Reader
	iamWriter *iamdomain.Writer

	// artifactsReader and artifactsWriter are the artifacts estate's two
	// sides, built on every data node for the identity estate's reason
	// ([core.openArtifacts]).
	artifactsReader *artifactsdomain.Reader
	artifactsWriter *artifactsdomain.Writer

	// artifactDataReader and artifactDataWriter are the datastore's two
	// sides, built on every data node for the same reason
	// ([core.openArtifactData]).
	artifactDataReader *artifactdata.Reader
	artifactDataWriter *artifactdata.Writer

	// assistantReader and assistantWriter are the Dashboard Assistant's
	// two sides, built on every data node for the same reason
	// ([core.openAssistant]).
	assistantReader *assistantdomain.Reader
	assistantWriter *assistantdomain.Writer

	// executorReader and executorWriter are the executor log's two sides,
	// built on every data node for the same reason ([core.openExecutor]).
	executorReader *executordomain.Reader
	executorWriter *executordomain.Writer

	// accessReader and accessWriter are the access log's two sides, built on
	// every data node for the same reason ([core.openAccess]).
	accessReader *accessdomain.Reader
	accessWriter *accessdomain.Writer
	// accessSealer opens a command's text for the SSH audit: one per node.
	accessSealer *accessdomain.Sealer

	// atlasReader and atlasWriter are the atlas log's two sides, built on
	// every data node for the same reason ([core.openAtlas]).
	atlasReader *atlasdomain.Reader
	atlasWriter *atlasdomain.Writer

	// advisorReader and advisorWriter are the advisor log's two sides, built
	// on every data node for the same reason ([core.openAdvisor]).
	advisorReader *advisordomain.Reader
	advisorWriter *advisordomain.Writer

	// run is the triggers' context and stop ends it. Held, not the caller's:
	// a trigger under the caller's context is one [core.shutdown] could never
	// end, and its wait would block for ever.
	run  context.Context
	stop context.CancelFunc
	done sync.WaitGroup
}

// startCore brings this node's core runtime up and publishes it, once per
// process. Its one caller, [New], runs it on every data node in every mode and
// on no node without `data` ([HoldsEstate]); a maintenance node still applies
// and serves reads, and only the duties that publish wait behind New's gate
// ([Engine.startCoreDuties]).
//
// It does not wait for hydration (a node blocked here would serve no dashboard
// or probe); seat acquisition waits instead ([Engine.StateLogHydrated]). Its
// appliers ask [Engine.applyEpoch] per batch rather than taking the boot
// company's, so a node's first company, and a revision moving a key an applier
// reads (the tracker's inbox retention), reach them without a restart.
func (e *Engine) startCore(ctx context.Context, boot *config.Bootstrap) error {
	// [New] already refused an in-memory stream ([config.Stream.Durable]).
	// The RESOLVED id, not the raw field (absent, `${VAR}`, or from the
	// environment): the consumers, the record writer and the eviction gate's
	// key must match the broker's server name and the presence row. Read
	// before the context so this failure has nothing to unwind.
	nodeID, err := config.ResolveNodeID(boot, config.EnvOnly())
	if err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	c := &core{nodeID: nodeID, run: runCtx, stop: cancel}
	// The appliers' signals, made before any applier exists to send on them
	// ([Engine.sampleDataFootprint]).
	e.dataFootprint = make(chan struct{}, 1)
	e.assistantErasure = make(chan struct{}, 1)
	// ONE FAILURE PATH for everything below, stood down once the runtime is
	// the engine's. The log's apply loops, heartbeat and snapshot donor run
	// under the LOG's own context, so a bare `cancel()` would reach none of
	// them and hand [New]'s failure path a store an applier still commits into.
	started := false
	defer func() {
		if !started {
			c.shutdown()
		}
	}()

	// The log comes up before anything reads it: every domain or none
	// ([Engine.startStateLog]).
	sl, err := e.startStateLog(ctx, boot, nodeID)
	if err != nil {
		return err
	}
	c.log = sl
	if c.gate, err = newNodeGate(sl, e.backends.Coord); err != nil {
		return err
	}
	// The identity estate has no backend setting and no other home, so its
	// absence is a boot failure naming the domain, not a nil reader.
	if err = c.openIAM(e, sl, nodeID); err != nil {
		return err
	}
	if err = c.openArtifacts(e, sl); err != nil {
		return err
	}
	if err = c.openArtifactData(e, sl); err != nil {
		return err
	}
	if err = c.openAssistant(e, sl); err != nil {
		return err
	}
	if err = c.openExecutor(sl); err != nil {
		return err
	}
	if err = c.openAccess(e, sl); err != nil {
		return err
	}
	if err = c.openAtlas(sl); err != nil {
		return err
	}
	if err = c.openAdvisor(sl); err != nil {
		return err
	}

	// Published whole: every field is written before the store, so readers
	// load it without a lock.
	e.core.Store(c)

	// THE DIRECTORY TRIGGER, started after the publish since it reads through
	// it, and handed the directory before its loop and the boot publish, so
	// the first registry already reads it. Not a duty: the registry derives
	// from this node's own rows, and a lease flap must not stop that.
	e.useDirectory(iamDirectory{reader: c.iamReader}, c.iamReader.At)
	c.done.Add(1)
	go func() {
		defer c.done.Done()
		e.watchDirectory(runCtx)
	}()
	// The watch that says this node stopped vouching for its identity rows,
	// which no committed batch says (identityvouch.go), over the domain
	// openIAM proved running.
	vouch := identityVouching(e, sl.Domain(iamdomain.Domain{}.Name()))
	c.done.Add(1)
	go func() {
		defer c.done.Done()
		vouch.run(runCtx)
	}()
	// AND THE ASSISTANT'S ERASURE OF A REMOVED PERSON, signalled by the
	// identity applier and run off its loop ([Engine.runAssistantErasure]).
	c.done.Add(1)
	go func() {
		defer c.done.Done()
		e.runAssistantErasure(runCtx)
	}()
	// AND THE DATASTORE'S FOOTPRINT GAUGE, sampled off the apply loop each
	// time a committed batch signals.
	c.done.Add(1)
	go func() {
		defer c.done.Done()
		e.runDataFootprint(runCtx)
	}()
	// The runtime is the engine's from here: [Engine.stopCore], the same
	// shutdown, ends it.
	started = true
	log.InfoContext(ctx, "core_runtime_started", "node", nodeID,
		"domains", sl.held().order)
	return nil
}

// startCoreDuties arms the core's fleet-singleton duties — the log's trim,
// without which every log grows to its ceiling, and the identity estate's,
// without which a removal's failed key delete lives for ever, the trail is
// never swept and a restore's duplicate never named — and this node's usage
// publisher ([Engine.startUsage]).
//
// On every data node that publishes, company or not: an unconfigured fleet
// still writes its identity log. Usage is the core's though no singleton:
// every data node publishes its own days from its own event log, whatever
// tracker or wiki the company keeps; a node without `data` publishes none, as
// its events are in its keeper's day ([observe.Custody]).
func (e *Engine) startCoreDuties(ctx context.Context) {
	c := e.core.Load()
	if c == nil {
		return
	}
	e.startRetention(ctx, e.boot, c.log)
	e.startIdentityDuties(ctx, e.boot)
	e.startUsage(ctx, c.log)
}

// stopViewTriggers ends this node's view triggers — the directory trigger and,
// on a data node, the identity vouch watch — and waits for a rebuild in
// flight. Called at the top of the teardown, apart from [Engine.stopCore],
// because a trigger WRITES what derives from the log (the registry, closed
// sockets): left to the end, an identity record landing mid-teardown would
// rebuild a registry over vendor wiring already stopped.
func (e *Engine) stopViewTriggers() {
	if c := e.core.Load(); c != nil {
		c.stopTriggers()
	}
	e.routedDirectory.end()
}

// stopCore ends this node's core runtime: its triggers, if the teardown has
// not already, then every domain's apply loop. Nil-safe, for an engine built
// by hand.
func (e *Engine) stopCore() { e.core.Load().shutdown() }

// stopTriggers ends the directory trigger and waits for it. Idempotent: a second
// call finds the context cancelled and the group drained.
func (c *core) stopTriggers() {
	if c == nil || c.stop == nil {
		return
	}
	c.stop()
	c.done.Wait()
}

// shutdown ends everything this runtime started and WAITS for it: one
// implementation for [Engine.stopCore] and [Engine.startCore]'s failure path,
// since a half-built runtime leaks what a built one does. Nil-safe, as the
// failure path may arrive before the log exists. The apply loops stop last,
// after the triggers reading what they write, or the shutdown logs like a
// stall.
func (c *core) shutdown() {
	if c == nil {
		return
	}
	c.stopTriggers()
	c.log.Stop()
}

// openIAM builds the identity estate's two sides over its running domain, on
// every data node: a missing domain is a boot failure naming it, never a node
// quietly serving no sign-in.
//
// The writer acts as THE NODE, under its id as the ENGINE
// ([iamdomain.NodeKind]) so the trail draws it as the system, for what only
// the node does: redeeming an invitation into a person not yet existing, the
// sweeping duty, a keyring rotation's re-seal. Every surface narrows it with
// [iamdomain.Writer.As]; a person's sign-in acts as that person.
func (c *core) openIAM(e *Engine, sl *stateLog, nodeID string) error {
	running := sl.Domain(iamdomain.Domain{}.Name())
	if running == nil {
		return fmt.Errorf("engine: this node runs no identity domain, so it " +
			"cannot say who anybody is — the domain is in the register and its " +
			"stream failed to come up")
	}
	reader, err := iamdomain.NewReader(iamdomain.ReaderOptions{
		DB:      sl.estate().Reader(),
		Reached: running.runner.Reached,
		// The lag makes the session table's `stalled` row reachable: past
		// the stall grace a node answers 503 to every arm, where without it a
		// node behind answered as caught up.
		Lag: running.Lag,
		// A sign-in answers before its session's start applies, so a write
		// presenting a session not applied here waits for the bearer's
		// stated position rather than being refused.
		Await: running.runner.WaitReached,
	})
	if err != nil {
		return fmt.Errorf("engine: iam reader: %w", err)
	}
	writer, err := iamdomain.NewWriter(iamdomain.WriterDeps{
		Publisher: running.publisher, DB: sl.estate().Reader(),
		// The blinder is a SOURCE, resolved per write: its key is minted by
		// whichever node needs one first, and a node with no company secret
		// store refuses the writes needing a blind by name at the call, not
		// at boot. The sealer is nil only on an engine built by hand without
		// a keyring, whose sealing writes are refused by name too.
		Blinds: e.PersonBlinder(),
		Sealer: e.PersonSealer(),
		// EVERY binding the directory writes (a create, an invitation, a
		// redemption, an identity edit) is checked per call against this
		// view's seat KIND as a HUMAN seat (ADR-0032), so a node with no
		// company refuses naming the remedy (iamdomain.ErrNoCompany, from
		// [session.ErrNoCompany]) rather than binding unchecked.
		Seats: SeatViewOf(e),
		// What a landed record decided (a grant delta, a session
		// generation) reaches the audit feed from here, since only the
		// decide holds it; every surface's [Writer.As] keeps it.
		Events: e.authEvents,
		// The engine's own housekeeping, never a credential's.
		Actor:     nodeID,
		ActorKind: iamdomain.NodeKind,
		// THE NODE IS THE DEPLOYMENT, so it writes as administrator: sweeps,
		// evictions, re-seals, and writes about people with no principal in
		// the gesture (an invitation's redeemed person, the company's first
		// included; every person a re-seal rewrites). Every surface replaces
		// it with [iamdomain.Writer.As].
		Grants: nodeWriterGrants,
	})
	if err != nil {
		return fmt.Errorf("engine: iam writer: %w", err)
	}
	c.iamReader, c.iamWriter = reader, writer
	return nil
}

// nodeWriterGrants is what the NODE itself authors identity records as:
// admin, which is every app beside it ([iam.Expand]).
//
// Nothing narrower: the domain decides every record the node writes as an
// administrator's, and with less nobody could redeem an invitation, the
// company's first person included. A package-level value so a test can pin it
// against what the domain's records ask of their writer.
var nodeWriterGrants = []iam.Grant{iam.GrantAdmin}

// errNoCoreRuntime is what a reader of the core refuses with on an engine that
// has none — a node without `data`, or one built by hand, since [New] starts it
// on every data node.
var errNoCoreRuntime = errors.New("engine: this engine runs no core runtime, " +
	"so it has no state log — it is not a data node")

// IAM is this node's identity read side, or nil on an engine with no core
// runtime — a node without `data`, which reaches the directory through a data
// node ([Engine.identityDirectory]), or one built by hand.
func (e *Engine) IAM() *iamdomain.Reader {
	c := e.core.Load()
	if c == nil {
		return nil
	}
	return c.iamReader
}

// AnyPerson reports whether this company has a person enrolled (a service
// account alone does not count) — `/health`'s `identity` and the boot's
// `iam_unclaimed` line — with "nobody" PROVED against the identity log's end,
// read first ([iamdomain.Reader.AnyPerson]).
//
// Proved, because a node that just joined holds empty rows until its applier
// catches up, and read bare would report that nobody works there. An
// unreadable log end is unknown too: /health answers `unknown`, the boot says
// nothing.
func (e *Engine) AnyPerson(ctx context.Context) (bool, error) {
	c := e.core.Load()
	if c == nil {
		return false, errNoCoreRuntime
	}
	end, err := e.IdentityLogEnd(ctx)
	if err != nil {
		return false, fmt.Errorf("%w: %w", statelog.ErrUnavailable, err)
	}
	return c.iamReader.AnyPerson(ctx, end)
}

// SessionStanding is who holds one named session and whether this node's rows
// hold it live (a named sign-out's question), with "nobody holds it" PROVED
// against the identity log's end, read first ([iamdomain.Reader.SessionStanding]).
//
// Proved for [Engine.AnyPerson]'s reason: a session opened through a peer is
// absent here until the applier catches up, and read bare it would tell an
// administrator a stolen laptop's session had ended. An unreadable log end is
// unknown too.
func (e *Engine) SessionStanding(ctx context.Context, lineage string,
	now time.Time) (string, bool, error) {

	c := e.core.Load()
	if c == nil {
		return "", false, errNoCoreRuntime
	}
	end, err := e.IdentityLogEnd(ctx)
	if err != nil {
		return "", false, fmt.Errorf("%w: %w", statelog.ErrUnavailable, err)
	}
	return c.iamReader.SessionStanding(ctx, lineage, now, end)
}

// ResetByID is one password reset link as this node's rows hold it, with
// whether they vouch for it at the identity log's end, read first
// ([iamdomain.Reader.ResetByID]); proved for [Engine.SessionStanding]'s
// reason, or a link issued through a peer would tell its holder it was dead.
func (e *Engine) ResetByID(ctx context.Context, id string) (iamdomain.ResetRow, error) {
	c := e.core.Load()
	if c == nil {
		return iamdomain.ResetRow{}, errNoCoreRuntime
	}
	end, err := e.IdentityLogEnd(ctx)
	if err != nil {
		return iamdomain.ResetRow{}, fmt.Errorf("%w: %w", statelog.ErrUnavailable, err)
	}
	return c.iamReader.ResetByID(ctx, id, end)
}

// InvitationByID is [Engine.ResetByID] for an invitation's link
// ([iamdomain.Reader.InvitationByID]).
func (e *Engine) InvitationByID(ctx context.Context, id string) (
	iamdomain.InvitationRow, error) {

	c := e.core.Load()
	if c == nil {
		return iamdomain.InvitationRow{}, errNoCoreRuntime
	}
	end, err := e.IdentityLogEnd(ctx)
	if err != nil {
		return iamdomain.InvitationRow{}, fmt.Errorf("%w: %w", statelog.ErrUnavailable, err)
	}
	return c.iamReader.InvitationByID(ctx, id, end)
}

// IAMWriter is this node's identity write side, or nil on an engine with no
// core runtime.
func (e *Engine) IAMWriter() *iamdomain.Writer {
	c := e.core.Load()
	if c == nil {
		return nil
	}
	return c.iamWriter
}

// NodeGate is eviction and readmission over every identity-claiming log, or
// nil on an engine with no core runtime.
func (e *Engine) NodeGate() *NodeGate {
	c := e.core.Load()
	if c == nil {
		return nil
	}
	return c.gate
}

// StateLogHydrated reports whether this node may claim a NEW seat: the one
// gate on seat acquisition, since a seat attached to a copy that is behind
// answers "no such item" to its own tools and acts on that. Two halves, in
// order: on a data node its own logs, every seat-gating domain
// ([statelog.Domain.GatesSeatAdmission]) established STRICTLY (a node inside
// the trim floor still serves rows that are behind); then the copy that will
// serve the seat's tools ([estate.Router.Serves]), for every native half the
// company runs (tracker, knowledge base, chat). A wrong seat-gating copy
// serves no seat ([estate.SeatCopy]), so a sound peer answers (ADR-0079), and
// a fleet view that never listed is refused. With no native half the second
// half asks nothing. The context bounds the routed request, which a sweep
// shutting down must not wait on.
func (e *Engine) StateLogHydrated(ctx context.Context) bool {
	if c := e.core.Load(); c != nil {
		if ok, refusal := c.log.Established(ctx, true); !ok {
			if refusal != "" {
				log.DebugContext(ctx, "seat_admission_withheld",
					"reason", string(refusal))
			}
			return false
		}
	}
	run := e.nativeHalves()
	if !run.any() || e.router == nil {
		return true
	}
	if e.dataView != nil && e.dataView.ListedAt().IsZero() {
		log.DebugContext(ctx, "seat_admission_withheld", "reason", "unroutable",
			"detail", "this node's view of the fleet has not listed it yet")
		return false
	}
	served, err := e.router.Serves(ctx)
	if err != nil || (run.tracker && !served.Tracker) || (run.wiki && !served.Pages) ||
		(run.chat && !served.Chat) {
		detail := "no copy of the estate admits a seat yet"
		if err != nil {
			detail = err.Error()
		}
		log.DebugContext(ctx, "seat_admission_withheld", "reason", "no_serving_copy",
			"detail", detail)
		return false
	}
	return true
}

// SeatsServiceable reports whether this node may KEEP its seats: it may while
// it can ROUTE their calls to a copy of the estate. A wrong copy is no reason
// to shed, since that domain's calls go to a sound peer ([localEstate.For],
// ADR-0079), and a lag never is: being behind is admission's business
// ([Engine.StateLogHydrated]). A data node whose seat copy is sound
// ([estate.SeatCopy]) keeps its seats whatever its fleet view says; any other
// sheds once its presence view answers unknown ([coord.LeaseView.Leases]) and
// last listed longer ago than [statelog.FloorCacheStale], when no decider
// trusts that view either. An all-vendor company and a never-listed view shed
// nothing. A sound copy is judged by the verdict already held
// ([localEstate.servesUnasked]), never a broker round trip; ctx only carries
// the sweep's values into a verdict's first read.
func (e *Engine) SeatsServiceable(ctx context.Context) (bool, string) {
	return e.serviceable(ctx, time.Now())
}

// serviceable is [Engine.SeatsServiceable] at now.
func (e *Engine) serviceable(ctx context.Context, now time.Time) (bool, string) {
	if run := e.nativeHalves(); !run.any() {
		return true, ""
	}
	if e.local != nil && e.local.servesUnasked(ctx) {
		return true, ""
	}
	return routable(e.dataView, now)
}

// ReplicationStatus is one of this node's replication loops (a state-log
// applier, so one domain's), shaped by the fleet view's question — how many of
// this node's copies have caught up — rather than by how an applier works: a
// count leaving any domain out would read "all caught up" for a node whose
// tracker is hours behind.
type ReplicationStatus struct {
	// Name is the domain, which is what an operator sees.
	Name string

	// Ready is this loop's copy being one a seat's tools may be attached to;
	// not the strict readiness that gates admission
	// ([Engine.StateLogHydrated]).
	Ready bool

	// Detail is why it is not ready, in the loop's own words. Empty for a
	// loop that is.
	Detail string
}

// StateLogStatus is one row per registered domain for the fleet view, on every
// data node, a company or not; empty on a node without `data` and on an engine
// with no core runtime.
//
// It takes the CALLER's context, the heartbeat's budget: each row costs two
// network calls (the broker's bounds, the fleet's floor), and under the node's
// run context one unreachable broker would hold the heartbeat past its
// deadline, and the node would silently stop advertising itself.
func (e *Engine) StateLogStatus(ctx context.Context) []ReplicationStatus {
	c := e.core.Load()
	if c == nil {
		return nil
	}
	return c.log.Status(ctx)
}

// WaitReached blocks until this node's applier for the log a position names has
// reached it: committed every row-writing record through it and decoded every
// barrier ([statelog.Runner.WaitReached]). It returns at once for a zero
// position, a node applying no log (its reads are a data node's), or a log
// this node does not run.
//
// The read-your-writes primitive: it takes the POSITION a log write answers
// with, comparable only on the same stream and generation. A read answers one
// too, possibly a barrier, and a caller carrying it waits for no barrier's
// commit.
func (e *Engine) WaitReached(ctx context.Context, at statelog.Position) error {
	c := e.core.Load()
	if c == nil || c.log == nil || at.Seq == 0 {
		return nil
	}
	// The position names its own stream, so every domain settles through
	// this one primitive.
	running := c.log.logOf(at.Stream)
	if running == nil {
		return nil
	}
	return running.runner.WaitReached(ctx, at)
}

// stateLogOf is the core runtime's state log, or [errNoCoreRuntime].
func (e *Engine) stateLogOf() (*stateLog, error) {
	c := e.core.Load()
	if c == nil || c.log == nil {
		return nil, errNoCoreRuntime
	}
	return c.log, nil
}

// identityDirectory is the identity directory as THIS node reads a login's
// binding: its own rows on a data node, a data node's through the estate's
// router on one without `data` ([routedIdentity]); false on an engine with
// neither, one built by hand. One answer for both, because its callers — a
// seat's tools ([Engine.HolderRecord]) and the request guard
// ([Engine.BoundSeat]) — run on either kind; the chart, this node's Tier A
// tokens and the vouch are applied by the caller, here.
func (e *Engine) identityDirectory() (bindingDirectory, bool) {
	if c := e.core.Load(); c != nil && c.iamReader != nil {
		return c.iamReader, true
	}
	if e.router != nil && e.local == nil {
		return routedIdentity{identity: e.router.Identity()}, true
	}
	return nil, false
}

// routedIdentity is the identity directory on a node without `data`: the row
// a login binds and who holds each seat, asked of a data node through the
// estate's router, floored on the identity log (internal/estate). FACTS, never
// verdicts, cross: the row narrowed to the binding and its vouch; the verdict
// is formed here, through this node's chart and Tier A tokens, by the
// functions a data node uses on its own rows ([holderRecordOf], [boundRowOf],
// [notify.Standing]).
type routedIdentity struct{ identity estate.Identity }

// PersonByLoginVouched is the row a login binds, as the serving data node
// holds it, and its vouch. A login nobody holds is the zero row and no error,
// as the directory answers it.
func (r routedIdentity) PersonByLoginVouched(ctx context.Context, login string) (
	iamdomain.Sighting, iamdomain.Vouch, error) {

	bound, vouch, err := r.identity.BindingByLogin(ctx, login)
	if err != nil {
		return iamdomain.Sighting{}, iamdomain.Vouch{}, err
	}
	return iamdomain.Sighting{ID: bound.ID, Kind: bound.Kind, Stage: bound.Stage,
		Login: bound.Login, Seat: bound.Seat}, vouch, nil
}

// SeatHolders is every seat binding the directory holds, in notify's
// vocabulary, read in one snapshot on the data node that answered. An error is
// UNKNOWN, never an empty directory — see [Engine.readStandingLocked].
func (r routedIdentity) SeatHolders(ctx context.Context) ([]notify.Holder, error) {
	holders, err := r.identity.SeatHolders(ctx)
	if err != nil {
		return nil, err
	}
	return notifyHolders(holders), nil
}

// directoryWatch is the party registry's directory trigger on a node without
// `data` — the periodic net alone, since no applier here could signal it — and
// what ends it.
type directoryWatch struct {
	stop context.CancelFunc
	done sync.WaitGroup
}

// end stops the trigger and waits for a rebuild in flight. Nil-safe, and
// idempotent.
func (w *directoryWatch) end() {
	if w == nil {
		return
	}
	w.stop()
	w.done.Wait()
}

// startRoutedDirectory hands the party registry the identity directory as a
// node without `data` reads it, and starts the net that re-reads it. Handed
// over before the first registry is built: one built first is the chart-only
// registry, which routes a suspended person's word to their seat. With no
// applier to signal it, the net re-reads every [DirectoryRefresh], the
// interval the alarm table already calls a stall. A data node never calls
// this; its core runs the trigger over its own rows.
func (e *Engine) startRoutedDirectory(ctx context.Context) {
	if e.router == nil || e.local != nil || e.routedDirectory != nil {
		return
	}
	e.useDirectory(routedIdentity{identity: e.router.Identity()}, nil)
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	w := &directoryWatch{stop: cancel}
	w.done.Add(1)
	go func() {
		defer w.done.Done()
		e.watchDirectory(runCtx)
	}()
	e.routedDirectory = w
}
