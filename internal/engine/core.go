package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE CORE RUNTIME: what every node runs from its first second, with a company
// or without one.
//
// # The bug the split fixes
//
// The whole of a node's durable state used to come up with its FIRST COMPANY —
// at boot for a node started on one, at the apply that handed one to a node
// started without. So a node booted unconfigured had no state log at all, and
// with it no identity estate: no sign-in, no invitation, no /iam. That is the
// posture the quickstart starts from, and the one a company with nobody in it
// has to leave through — its first person is invited under a Tier A token like
// everybody after them — so the one node that most needed a way in had none,
// and the dashboard's own "create the company" path needed a session nothing
// could mint. Nor was the chart open, so a company could not be built from the
// org builder before a settings revision existed to start the runtime.
//
// # Split by what depends on a company, never by domain
//
// The CORE is everything whose meaning does not depend on a company: the state
// log for EVERY registered domain, the node gate over every identity log, the
// org chart's and the identity estate's two sides, and the loops that derive
// this node's views of both. It is published once, by [New], on every node and
// in every mode. The [native] half is what only a company can say anything
// about — whether the engine keeps its tracker and knowledge base at all,
// their read and write sides, the lexical index, the change feeds and the
// embedding duty — and it still waits for the first company.
//
// # Why the core runs EVERY domain, and not the chart and the identity estate
//
// Starting only the two a node with no company can use would be the obvious
// cut, and it is wrong four ways:
//
//   - A JOIN REPLACES THE WHOLE REPLICATED FILE and must run before any applier
//     does ([Engine.startStateLog]'s "every domain or none"). Starting the rest
//     of the register later would take a second join over a file whose chart
//     and identity rows are already being applied — a rejoin that halts both.
//   - A SNAPSHOT NAMES EVERY REGISTERED DOMAIN, or every joiner refuses it: a
//     node running part of the register could donate nothing.
//   - THE TRIM COUNTS NODES PER LOG from their position heartbeats, so a node
//     that did not run the tracker's log would pin that log's trim at zero for
//     as long as it was up.
//   - And a fleet would have two notions of a node "running the log", which
//     every alarm, readiness gate and seat-admission rule would have to be
//     taught to tell apart.
//
// So the tracker's and the knowledge base's logs are applied on a node with no
// company exactly as on any other: what waits for the company is the WRITERS
// and READERS over them, because which of them exist is the company's to say.

// core holds this node's always-on runtime.
//
// EVERY FIELD IS WRITTEN ONCE, by [Engine.startCore], before the struct is
// published to [Engine.core] — which is why there is no mutex here. What the
// running node mutates afterwards carries its own synchronisation: the wait
// group the view triggers are joined through.
type core struct {
	// nodeID is who this node is: the name its own domain consumers take,
	// the writer stamped on every record it publishes, and what the
	// eviction gate compares against. Held because the native half, the
	// maintenance jobs and the reanchor guard all need it after boot and
	// the bootstrap it came from is not kept.
	nodeID string

	// log is this node's state-log runtime: every registered domain, each
	// with its own apply loop and write authority. See the file comment for
	// why it is every domain.
	log *stateLog

	// gate is eviction and readmission over every identity-claiming log in
	// the register. PER NODE AND BUILT WITH THE LOG, never gated on a
	// company or a backend: the register runs every domain or none, so a
	// node that has met no company still has logs the trim counts it on.
	gate *NodeGate

	// chartWriter and chartReader are the org chart's two sides.
	//
	// BUILT ON EVERY NODE, with or without a company: there is no backend
	// setting for the chart and nowhere else to keep one, and a company is
	// BUILT through it — the org builder writes units and seats before any
	// settings revision exists. A node whose chart domain failed to come up
	// is a boot failure naming the domain rather than a nil to branch on.
	chartWriter *chart.Writer
	chartReader *chart.Reader

	// iamReader and iamWriter are the identity estate's two sides, on the
	// chart's terms: every node runs the domain, and a node with nobody in
	// its company is precisely the node that must be able to enrol its
	// first person.
	iamReader *iamdomain.Reader
	iamWriter *iamdomain.Writer

	// run is the context the view triggers run under, and stop is what
	// ends them.
	//
	// HELD, not re-derived: a trigger started under the CALLER's context
	// would be one [core.shutdown] could never end, and its wait would
	// block for ever.
	run  context.Context
	stop context.CancelFunc
	done sync.WaitGroup
}

// startCore brings this node's core runtime up and publishes it, once per
// process.
//
// ONE CALLER, [New], on EVERY node and in every mode — a node with no company,
// a node in a maintenance mode, a node on a vendor's tracker and wiki. What a
// maintenance node does not start is the duties that PUBLISH, which
// [Engine.startCoreDuties] arms behind New's publishing gate; the appliers
// and the view triggers run everywhere, because applying records and serving
// reads is exactly what a maintenance node still does.
//
// It returns without waiting for hydration: the reconcile is O(keys) and a
// node that blocked here would not serve its dashboard, answer a probe or run
// a duty until it finished. Seat acquisition is what waits, through
// [Engine.StateLogHydrated].
//
// # The epoch its appliers read
//
// NOT THE BOOT COMPANY'S, although this is where the runners are built: each
// asks [Engine.applyEpoch] per batch, so a node that booted with no company
// applies under its first one from the batch after that company is
// published, and a revision that moves a key an applier reads — the tracker's
// inbox retention — reaches the applier without a restart.
func (e *Engine) startCore(ctx context.Context, boot *config.Bootstrap) error {
	// AN IN-MEMORY STREAM NEVER GETS THIS FAR: [New] refused it before
	// anything was opened — see [config.Stream.Durable].
	//
	// THE RESOLVED ID, not the raw field. `node.id` may be absent, a
	// `${VAR}` reference, or come from the environment — and the value
	// three durable things are named after (this node's own consumers, the
	// writer stamped on its records, the eviction gate's key) must be the
	// same one the broker's server name and the presence row already use.
	//
	// READ BEFORE the context below rather than after, so the one failure
	// in this function that has started nothing has nothing to unwind.
	nodeID, err := config.ResolveNodeID(boot, config.EnvOnly())
	if err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	c := &core{nodeID: nodeID, run: runCtx, stop: cancel}
	// ONE FAILURE PATH FOR EVERYTHING BELOW, armed before the first thing
	// that outlives this call and stood down once the runtime is the
	// engine's. The state log's apply loops, its position heartbeat and its
	// snapshot donor run under a context of the LOG's own, so a bare
	// `cancel()` on the way out would reach none of them — and a return that
	// left them behind hands [New]'s failure path a store an applier is
	// still committing into.
	started := false
	defer func() {
		if !started {
			c.shutdown()
		}
	}()

	// THE LOG COMES UP BEFORE ANYTHING READS IT, once for the node: every
	// domain in the register or none — see [Engine.startStateLog].
	sl, err := e.startStateLog(ctx, boot, nodeID)
	if err != nil {
		return err
	}
	c.log = sl
	if c.gate, err = newNodeGate(sl, e.backends.Coord); err != nil {
		return err
	}
	// THE IDENTITY ESTATE AND THE CHART, which every node runs. There is no
	// backend setting for either and no second place to keep one, so each
	// one's absence is a boot failure naming the domain rather than a reader
	// that answers nil. The identity estate goes first because the chart's
	// writer consults it before a seat removal.
	if err = c.openIAM(e, sl, nodeID); err != nil {
		return err
	}
	if err = c.openChart(e, sl, nodeID); err != nil {
		return err
	}

	// PUBLISHED, whole: every field above is written before the store, which
	// is what lets every reader load it without a lock.
	e.core.Store(c)

	// THE CHART VIEW'S TWO TRIGGERS, started AFTER the runtime is published
	// because both reach the chart reader through it — a goroutine that
	// raced the publish would read a nil runtime and derive nothing.
	//
	// NEITHER IS A DUTY. A node's view is a derivation of its OWN rows, so
	// tying it to a fleet lease would mean a lease flap stopped a node
	// tracking its own state. They run on a node with no company too, where
	// a rebuild reads the rows and publishes nothing — there are no settings
	// to compose them with — so the company that arrives next is composed
	// with a view that is already current.
	c.done.Add(2)
	go func() {
		defer c.done.Done()
		e.watchChartNudges(runCtx)
	}()
	go func() {
		defer c.done.Done()
		e.watchChart(runCtx)
	}()
	// AND THE PARTY REGISTRY'S DIRECTORY TRIGGER, reading this node's own
	// identity rows. Handed over BEFORE the loop starts, so the first
	// rebuild it runs already reads the directory — and before the boot
	// publish, so the first registry does too.
	e.useDirectory(iamDirectory{reader: c.iamReader}, c.iamReader.At)
	c.done.Add(1)
	go func() {
		defer c.done.Done()
		e.watchDirectory(runCtx)
	}()
	// THE RUNTIME IS THE ENGINE'S FROM HERE, so the cleanup above stands
	// down and [Engine.stopCore] — the same shutdown — is what ends it.
	started = true
	log.InfoContext(ctx, "core_runtime_started", "node", nodeID,
		"domains", sl.order)
	return nil
}

// startCoreDuties arms the core runtime's fleet-singleton duties: the log's
// trim, without which every domain's log only grows to its ceiling, and the
// identity estate's, without which a removal's failed key delete lives for
// ever, the trail is never swept and a duplicate a restore made is never
// named.
//
// ON EVERY NODE THAT PUBLISHES, company or not: a fleet nobody has configured
// yet still writes its identity log — the first person's invitation and
// sign-in — and its chart log, and without the trim those logs only grow.
func (e *Engine) startCoreDuties(ctx context.Context) {
	c := e.core.Load()
	if c == nil {
		return
	}
	e.startRetention(ctx, e.boot, c.log)
	e.startIdentityDuties(ctx, e.boot)
}

// stopViewTriggers ends the core's view triggers — the chart view's two and the
// party registry's directory trigger — and waits for a rebuild in flight.
//
// SEPARATE FROM [Engine.stopCore], and called at the very top of the teardown,
// because a trigger is not a reader of the log so much as a WRITER of
// everything derived from it: a rebuild ends in [Engine.convergeOn], which
// re-arms the scheduler, re-ensures the mailboxes and rebuilds the party
// registry. Ended with the log at the bottom of the teardown, a chart record
// landing after the scheduler had been stopped re-armed a loop nothing would
// ever stop again, ticking against a store and a broker the teardown then
// closed.
func (e *Engine) stopViewTriggers() {
	if c := e.core.Load(); c != nil {
		c.stopTriggers()
	}
}

// stopCore ends this node's core runtime: its view triggers, if the teardown
// has not already, and then every domain's apply loop. Nil-safe, which is an
// engine built by hand.
func (e *Engine) stopCore() { e.core.Load().shutdown() }

// stopTriggers ends the view triggers and waits for them. Idempotent: a second
// call finds the context cancelled and the group drained.
func (c *core) stopTriggers() {
	if c == nil || c.stop == nil {
		return
	}
	c.stop()
	c.done.Wait()
}

// shutdown ends everything this runtime started and WAITS for it.
//
// ONE IMPLEMENTATION FOR TWO CALLERS — [Engine.stopCore] and the failure path
// in [Engine.startCore] — because a half-built runtime leaks precisely what a
// built one does: the log's apply loops do not know their node never finished
// booting. Nil-safe throughout, because the failure path can reach it with the
// log not yet built.
//
// THE APPLY LOOPS LAST, after the triggers that read what they write. A loop
// stopped first leaves a trigger deriving from a log nothing is applying,
// which is not wrong so much as a shutdown that looks like a stall in every
// log line it produces on the way out.
func (c *core) shutdown() {
	if c == nil {
		return
	}
	c.stopTriggers()
	c.log.Stop()
}

// openIAM builds the identity estate's two sides over its running domain.
//
// EVERY NODE RUNS IT, whatever its roles and whether or not it has a company,
// so a node whose identity domain did not come up is a boot failure naming the
// domain — exactly as the chart's is — rather than a node that quietly serves
// no sign-in surface.
//
// # The writer acts as THE NODE, and every surface narrows it
//
// Exactly as the chart's does. What is left acting as the node is what the
// node itself does: redeeming an invitation into a person who does not exist
// yet, the duty that sweeps, and the re-seal a keyring rotation moves every
// person's values with. A person's own sign-in acts as that person.
func (c *core) openIAM(e *Engine, sl *stateLog, nodeID string) error {
	running := sl.Domain(iamdomain.Domain{}.Name())
	if running == nil {
		return fmt.Errorf("engine: this node runs no identity domain, so it " +
			"cannot say who anybody is — the domain is in the register and its " +
			"stream failed to come up")
	}
	reader, err := iamdomain.NewReader(iamdomain.ReaderOptions{
		DB: e.backends.Store, Log: running.reader,
		Committed: running.runner.Committed,
		// AND THE LAG, which is what makes the session table's
		// `stalled` row reachable at all: a node past the stall grace
		// answers 503 to every arm, and with no lag to read it
		// answered as a caught-up node for as long as it was behind.
		Lag: running.Lag,
		// AND THE WAIT, which a write presenting a session this node
		// has not applied yet takes: a sign-in answers before its
		// session's start applies, and the request guard waits for the
		// position the bearer states rather than refusing it.
		Await: running.runner.WaitCommitted,
	})
	if err != nil {
		return fmt.Errorf("engine: iam reader: %w", err)
	}
	writer, err := iamdomain.NewWriter(iamdomain.WriterDeps{
		Publisher: running.publisher, DB: e.backends.Store,
		// THE BLINDER IS A SOURCE, resolved at the write that needs it,
		// because the key behind it is minted by whichever node needs
		// one first; a node with no company secret store cannot derive
		// a blind, and the writes that need one are refused BY NAME at
		// the call rather than at boot. THE SEALER is over the keyring
		// every node holds, so it is nil only on an engine built by hand
		// without one — and the writes that seal are refused by name
		// there too.
		Blinds: e.PersonBlinder(),
		Sealer: e.PersonSealer(),
		// WHAT A LANDED RECORD DECIDED — a grant delta, a session
		// generation — goes on the node's audit feed from here, since
		// only the decide holds it. Every surface's [Writer.As] keeps it.
		Events:    e.authEvents,
		Actor:     nodeID,
		ActorKind: iam.KindMachine,
		// THE NODE IS THE DEPLOYMENT, so it authors the classes only the
		// deployment has: the sweeps, and the re-seal a keyring rotation
		// moves everybody's values with. Every surface replaces these
		// with [iamdomain.Writer.As].
		//
		// AND people:manage BESIDE IT, because two of the node's writes
		// are about people with no principal of their own in the
		// gesture: the person an invitation redeems into, who does not
		// exist until the enrolment lands — the company's first person
		// included — and every person a re-seal rewrites. Without it
		// the sign-in surface's own writer was refused by the domain, so
		// nobody could redeem an invitation at all.
		Grants: nodeWriterGrants,
	})
	if err != nil {
		return fmt.Errorf("engine: iam writer: %w", err)
	}
	c.iamReader, c.iamWriter = reader, writer
	return nil
}

// nodeWriterGrants is what the NODE itself authors identity records as.
//
// A PACKAGE-LEVEL VALUE so a test can hold it against
// [iamdomain.AdminGrant] rather than a reader having to remember the pairing:
// the two halves live in different packages, nothing else compares them, and
// the failure when they drift is that nobody can redeem an invitation — the
// company's first person included, since they are invited like anybody else.
var nodeWriterGrants = []iam.Grant{iam.GrantFleetOperate, iamdomain.AdminGrant}

// openChart builds the org chart's two sides over its running domain.
//
// THE WRITER ACTS AS THE NODE ITSELF, and every surface derives its own from
// it with [chart.Writer.As]: an operator's session acts as that credential, a
// founder's as that person. What is left acting as the node is what the node
// itself does — an import applying a config revision, a duty tidying a
// tombstone — and attributing those to a person would make a machine's
// housekeeping indistinguishable from somebody's decision.
func (c *core) openChart(e *Engine, sl *stateLog, nodeID string) error {
	running := sl.Domain(chart.Domain{}.Name())
	if running == nil {
		return fmt.Errorf("engine: this node runs no chart domain, so it " +
			"cannot say who reports to whom — the domain is in the register " +
			"and its stream failed to come up")
	}
	writer, err := chart.NewWriter(chart.WriterDeps{
		Publisher: running.publisher, DB: e.backends.Store,
		// THE COMPANY'S OWN SECRET STORE, so a literal credential
		// written to a seat is sealed rather than put on a log every
		// node applies. Nil is a real configuration — a company with no
		// store — and a write that needs one is then refused by name.
		Seal: e.chartSealer(),
		// AND WHERE A RUNTIME HALF KEEPS ITS CREDENTIALS, which the
		// chart cannot read: the organization model's own types, whose
		// tags say which of a seat's or a unit's values are sealed.
		Runtime:   org.RuntimeShape{},
		Actor:     nodeID,
		ActorKind: chart.AuthorOperator,
		// THE NODE ITSELF IS THE DEPLOYMENT, so it authors every class
		// the chart has: the seeding import, the structural tidying a
		// duty does, the runtime half of every seat a revision
		// describes — and a removal, which takes the deployment's grant
		// beside the company's. Every surface then narrows it with
		// [chart.Writer.As], which REPLACES these rather than adding to
		// them — a caller's party is never this one.
		Grants: []iam.Grant{iam.GrantConfigWrite, iam.GrantFleetOperate},
	})
	if err != nil {
		return fmt.Errorf("engine: chart writer: %w", err)
	}
	// THE DIRECTORY THE SEAT REMOVAL CONSULTS — this node's own identity
	// rows, which openIAM has just established. See [chart.Holders].
	c.chartWriter = writer.WithHolders(c.iamReader)
	// THROUGH THE DOMAIN'S OWN READ AUTHORITY, so a level asked for is a
	// level served: the refusal ladder, the coverage probe and the barrier
	// a linearizable read waits through.
	if c.chartReader, err = chart.NewReader(chart.ReaderOptions{
		DB: e.backends.Store, Log: running.reader,
		Committed: running.runner.Committed,
		// FOR THE SEAT BINDING, which compares it against the stall
		// grace before it reads a row: see [SeatView].
		Lag: running.Lag,
	}); err != nil {
		return fmt.Errorf("engine: chart reader: %w", err)
	}
	return nil
}

// errNoCoreRuntime is what a reader of the core refuses with on an engine that
// has none — one built by hand, since [New] starts it on every node.
var errNoCoreRuntime = errors.New("engine: this engine runs no core runtime, " +
	"so it has no state log")

// IAM is this node's identity read side, or nil on an engine with no core
// runtime — one built by hand, since [New] starts it on every node.
func (e *Engine) IAM() *iamdomain.Reader {
	c := e.core.Load()
	if c == nil {
		return nil
	}
	return c.iamReader
}

// AnyPerson reports whether this company has anybody enrolled — `/health`'s
// `identity` and the boot's `iam_unclaimed` line — with "nobody" PROVED
// against the identity log's end, which is read first
// ([iamdomain.Reader.AnyPerson]).
//
// PROVED, because every node applies the identity log from boot, company or
// none: a node that has just joined a fleet with people in it holds empty rows
// until its applier catches up, and read bare they told /health and the boot
// log that nobody works there. A log end this node cannot read is the unknown
// arm too, never a "nobody" — /health answers `unknown`, and the boot says
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

// IAMWriter is this node's identity write side, or nil on an engine with no
// core runtime.
func (e *Engine) IAMWriter() *iamdomain.Writer {
	c := e.core.Load()
	if c == nil {
		return nil
	}
	return c.iamWriter
}

// Chart is this node's org chart read side, or nil on an engine with no core
// runtime.
func (e *Engine) Chart() *chart.Reader {
	c := e.core.Load()
	if c == nil {
		return nil
	}
	return c.chartReader
}

// ChartWriter is this node's org chart write side, or nil on an engine with no
// core runtime.
func (e *Engine) ChartWriter() *chart.Writer {
	c := e.core.Load()
	if c == nil {
		return nil
	}
	return c.chartWriter
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

// StateLogHydrated reports whether every domain this node's state log runs has
// caught up.
//
// THE GATE ON SEAT ACQUISITION. A seat whose mailbox attached first would
// answer "there is no such item" to its own tools — an answer it acts on by
// filing a duplicate or abandoning work it was told to do. An engine with no
// core runtime is trivially hydrated; every engine [New] builds has one, and a
// node that has met no company holds no seat anyway.
//
// THE CORE'S LOG, not the native half's, because the log is the core's: the
// tracker's and the knowledge base's domains are applied from boot whether or
// not a company has asked for their writers yet, and a seat attaching over a
// tracker log still behind would read exactly the stale answer this gate
// exists for.
func (e *Engine) StateLogHydrated() bool {
	c := e.core.Load()
	if c == nil {
		return true
	}
	// STRICT, because this is seat admission rather than a read: a node
	// that is merely inside the trim floor still serves rows that are
	// behind, and a seat attaching to one acts on them.
	ok, refusal := c.log.Established(c.run, true)
	if !ok && refusal != "" {
		log.DebugContext(c.run, "seat_admission_withheld",
			"reason", string(refusal))
	}
	return ok
}

// SeatsServiceable reports whether this node may KEEP the seats it holds.
//
// THE OPPOSITE DIRECTION FROM [Engine.StateLogHydrated], and they fire on
// different classes of fault. Hydration is about a copy that is BEHIND: it
// catches up, so withholding claims is the whole remedy and dropping work in
// hand would be pure loss. This is about a copy that is WRONG — an applier
// halted at a record it cannot decode, an eviction whose peers are dropping
// everything this node writes, rows below the log with a hole nothing will
// fill (or a trim floor nobody could read), a checkpoint naming a stream that
// is not this one, an applied prefix frozen past [statelog.StallGrace], or a
// record held past [statelog.DeferralGrace]. A seat left running on any of
// those answers its own tools out of a copy the fleet has already abandoned,
// and D122 is the rule that says it must not.
//
// A LAG IS NEVER ONE OF THEM, which is what the log line below means by
// "wrong rather than behind" — and for as long as the health underneath
// derived "has this node's copy ever been whole" from "is it level this
// instant", that line was false on every firing: one unapplied tracker record
// made a solo node unfit for a heartbeat and moved all seven of its seats.
//
// An engine with no core runtime is trivially serviceable, for
// [Engine.StateLogHydrated]'s reason.
func (e *Engine) SeatsServiceable(ctx context.Context) (bool, string) {
	c := e.core.Load()
	if c == nil {
		return true, ""
	}
	// THE CALLER'S CONTEXT, because this read reaches the broker for every
	// domain's stream bounds. Bounded by the engine's own run context
	// instead, a /ready probe waited on a slow broker for as long as the
	// process had left to live — past its own deadline, past the
	// orchestrator's, and the one caller that most needs a timely answer is
	// the one asking whether to send this node traffic.
	ok, domain := c.log.Healthy(ctx)
	if !ok {
		log.WarnContext(ctx, "seats_unserviceable",
			"domain", domain,
			"hint", "this node's copy of that domain is wrong rather than "+
				"behind; its seats move to a peer until it recovers")
		return false, domain
	}
	return true, ""
}

// ReplicationStatus is one of this node's replication loops, as the fleet view
// counts them.
//
// # Why the shape is the question and not the mechanism
//
// Every loop this node runs is a state-log APPLIER — the wiki's projector, the
// one loop of another kind, went when the knowledge base became a domain — so
// every row is a domain's ([ReplicationStatus.Kind] is always `domain`). The
// type is still shaped by what the fleet view asks rather than by how an
// applier works, because that is what outlived the projector.
//
// What the fleet view asks is not about the mechanism. It asks "how many of
// this node's copies of the company's state have caught up" — the question
// behind an operator's "why is the new node holding no seats" — and a count
// that left any domain out would answer "all caught up" for a node whose
// tracker is hours behind. So the shared thing is the QUESTION, and this is
// its shape: a name, whether it is ready, and the detail an operator reads
// when it is not.
type ReplicationStatus struct {
	// Name is the domain, which is what an operator sees.
	Name string

	// Kind is the loop's mechanism: `domain` on every row this build
	// reports, the `projection` rows having gone with the projector.
	Kind string

	// Ready is this loop's copy being one a seat's
	// tools may be attached to. It is NOT strict readiness — see
	// [Engine.StateLogHydrated], which asks the stricter question that
	// actually gates admission.
	Ready bool

	// Detail is why it is not ready, in the loop's own words. Empty for a
	// loop that is.
	Detail string
}

// StateLogStatus is what this node reports about its replication loops, for
// the fleet view: one row per registered domain, on every node — a node that
// has met no company included, since its state log runs from boot. Empty only
// on an engine with no core runtime.
//
// IT TAKES THE CALLER'S CONTEXT, and that is load-bearing rather than
// idiomatic tidiness: a domain's row is assembled from the broker's own bounds
// and the fleet's published floor, which are two network calls per domain, and
// the caller is a heartbeat with a budget. Reading them under this node's
// long-lived run context instead would let one unreachable broker hold the
// heartbeat open past its own deadline — which is a node that stops
// advertising itself at all, reported by nothing, because it was assembling a
// report about being behind.
func (e *Engine) StateLogStatus(ctx context.Context) []ReplicationStatus {
	c := e.core.Load()
	if c == nil {
		return nil
	}
	return c.log.Status(ctx)
}

// WaitCommitted blocks until this node's applier for the log a position names
// has consumed through it.
//
// THE READ-YOUR-WRITES PRIMITIVE ON THE LOG, and it takes a POSITION rather
// than a revision because that is what a write answers with: a bucket write
// returns a revision on one family, and a log write returns a place on a
// stream that only compares against the same stream and the same generation.
func (e *Engine) WaitCommitted(ctx context.Context, at statelog.Position) error {
	c := e.core.Load()
	if c == nil || c.log == nil || at.Seq == 0 {
		return nil
	}
	// THE POSITION NAMES ITS OWN STREAM, so this resolves the domain from
	// it rather than taking one. That is what a bucket revision could never
	// do — it was a number on a family, and the caller had to say which —
	// and it is why every domain settles through one primitive.
	running := c.log.Domain(c.log.domainOf(at.Stream))
	if running == nil {
		return nil
	}
	return running.runner.WaitCommitted(ctx, at)
}

// stateLogOf is the core runtime's state log, or [errNoCoreRuntime].
func (e *Engine) stateLogOf() (*stateLog, error) {
	c := e.core.Load()
	if c == nil || c.log == nil {
		return nil, errNoCoreRuntime
	}
	return c.log, nil
}
