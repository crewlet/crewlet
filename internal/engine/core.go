package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/assistantdomain"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE CORE RUNTIME: what every DATA node runs from its first second, with a
// company or without one.
//
// # The bug the split fixes
//
// The whole of a node's durable state used to come up with its FIRST COMPANY —
// at boot for a node started on one, at the apply that handed one to a node
// started without. So a node booted unconfigured had no state log at all, and
// with it no identity estate: no sign-in, no session, no /iam. That is the
// posture the quickstart starts from, and the one a company has to be created
// through — a Tier A token's session, exchanged on this node, is what the
// dashboard's own "create the company" path writes the first revision as, and
// the company's first person is invited under it once that revision declares
// the human seat they will hold (ADR-0032) — so the one node that most needed a
// way in had none, and that path needed a session nothing could mint.
//
// # Split by what depends on a company, never by domain
//
// The CORE is everything whose meaning does not depend on a company: the state
// log for EVERY registered domain, the node gate over every identity log, the
// identity estate's two sides, and the loop that derives this node's party
// registry from its directory. It is published once, by [New], on every node
// that holds the estate and in every mode. The [native] half is what only a
// company can say anything about — whether the engine keeps its tracker and
// knowledge base at all, their read and write sides, the lexical index, the
// change feeds and the embedding duty — and it still waits for the first
// company.
//
// # Why the core runs EVERY domain, and not only the identity estate
//
// Starting only the one a node with no company can use would be the obvious
// cut, and it is wrong four ways:
//
//   - A JOIN REPLACES THE WHOLE REPLICATED FILE and must run before any applier
//     does ([Engine.startStateLog]'s "every domain or none"). Starting the rest
//     of the register later would take a second join over a file whose
//     identity rows are already being applied — a rejoin that halts it.
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
//
// # Only where the node holds the estate
//
// A node without the `data` role (ADR-0025) holds no replicated estate, runs
// no log and starts no core: there is no copy for an applier to write and no
// file for a join to replace. What such a node still needs of the identity
// directory — whose record a login names, and who holds each human seat — it
// asks a data node through the estate's router ([routedIdentity]), and the
// chart and this node's own Tier A tokens are applied to the answer here, on
// the asking side, as they are to its own rows on a data node. Its party
// registry reads the directory on the periodic net alone ([DirectoryRefresh]),
// since there is no applier here to signal it. And it needs no more: the
// sign-in surface and the request guard run only where the `ingress` role does,
// which Tier A requires `data` beside, so every session this deployment
// validates is validated against rows the validating node holds — 503 and
// never 401 on a node that is behind, and no cache in front of either.

// core holds this node's always-on runtime.
//
// EVERY FIELD IS WRITTEN ONCE, by [Engine.startCore], before the struct is
// published to [Engine.core] — which is why there is no mutex here. What the
// running node mutates afterwards carries its own synchronisation: the wait
// group the directory trigger is joined through.
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

	// iamReader and iamWriter are the identity estate's two sides. BUILT
	// ON EVERY DATA NODE, with or without a company: a node with no company is
	// precisely the node that must sign a Tier A token's session in so the
	// company can be created, and one whose identity domain failed to come
	// up is a boot failure naming the domain rather than a nil to branch
	// on. What waits for the company is only the SEAT a person is invited
	// or created onto, which the writer checks against it.
	iamReader *iamdomain.Reader
	iamWriter *iamdomain.Writer

	// artifactsReader and artifactsWriter are the artifacts estate's two
	// sides, built on every data node for the identity estate's reason:
	// the register runs every domain, so one that did not come up is a
	// boot failure naming it ([core.openArtifacts]).
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

	// run is the context the directory trigger runs under, and stop is
	// what ends it.
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
// ONE CALLER, [New], on EVERY DATA NODE and in every mode — a node with no
// company, a node in a maintenance mode, a node on a vendor's tracker and wiki
// — and on no node without `data`, which holds no estate to run a log over
// ([HoldsEstate]). What a
// maintenance node does not start is the duties that PUBLISH, which
// [Engine.startCoreDuties] arms behind New's publishing gate; the appliers
// and the directory trigger run everywhere, because applying records and
// serving reads is exactly what a maintenance node still does.
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
	// THE FOOTPRINT SAMPLER'S SIGNAL, made before any applier exists to
	// send on it ([Engine.sampleDataFootprint]).
	e.dataFootprint = make(chan struct{}, 1)
	e.assistantErasure = make(chan struct{}, 1)
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
	// THE IDENTITY ESTATE, which every data node runs. There is no backend
	// setting for it and no second place to keep one, so its absence is a
	// boot failure naming the domain rather than a reader that answers nil.
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

	// PUBLISHED, whole: every field above is written before the store, which
	// is what lets every reader load it without a lock.
	e.core.Store(c)

	// THE PARTY REGISTRY'S DIRECTORY TRIGGER, reading this node's own
	// identity rows — started AFTER the runtime is published, because it
	// reaches the reader through it. Handed over BEFORE the loop starts, so
	// the first rebuild it runs already reads the directory — and before
	// the boot publish, so the first registry does too.
	//
	// NOT A DUTY. A node's registry is a derivation of its OWN rows, so
	// tying it to a fleet lease would mean a lease flap stopped a node
	// tracking its own state.
	e.useDirectory(iamDirectory{reader: c.iamReader}, c.iamReader.At)
	c.done.Add(1)
	go func() {
		defer c.done.Done()
		e.watchDirectory(runCtx)
	}()
	// AND THE WATCH THAT SAYS THIS NODE STOPPED VOUCHING FOR ITS IDENTITY
	// ROWS, which no committed batch ever says — see identityvouch.go. Over
	// the identity domain openIAM has already proved is running.
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
	// THE RUNTIME IS THE ENGINE'S FROM HERE, so the cleanup above stands
	// down and [Engine.stopCore] — the same shutdown — is what ends it.
	started = true
	log.InfoContext(ctx, "core_runtime_started", "node", nodeID,
		"domains", sl.held().order)
	return nil
}

// startCoreDuties arms the core runtime's fleet-singleton duties — the log's
// trim, without which every domain's log only grows to its ceiling, and the
// identity estate's, without which a removal's failed key delete lives for
// ever, the trail is never swept and a duplicate a restore made is never
// named — and this node's own usage publisher ([Engine.startUsage]).
//
// ON EVERY DATA NODE THAT PUBLISHES, company or not: a fleet nobody has
// configured yet still writes its identity log — the Tier A token's session the
// company is created through — and without the trim that log only grows.
//
// THE USAGE PUBLISHER IS THE CORE'S TOO, and not a native duty, although it
// is no singleton: every data node publishes its own days, from its own event
// log, whether or not its company keeps Worklet or Doclet. Armed with
// the native half it used to be, a company on Jira and Confluence never
// replicated a day of its spend, and a node handed its first company by an
// apply published none until it restarted. A node without `data` keeps no event
// log of its own — its events are kept by a data node's custody
// ([observe.Custody]) — so its spend is in its keeper's day and it publishes
// none.
func (e *Engine) startCoreDuties(ctx context.Context) {
	c := e.core.Load()
	if c == nil {
		return
	}
	e.startRetention(ctx, e.boot, c.log)
	e.startIdentityDuties(ctx, e.boot)
	e.startUsage(ctx, c.log)
}

// stopViewTriggers ends this node's view triggers — the party registry's
// directory trigger and, on a data node, the identity vouch watch — and waits
// for a rebuild in flight.
//
// SEPARATE FROM [Engine.stopCore], and called at the very top of the teardown,
// because a trigger is not a reader of the log so much as a WRITER of what is
// derived from it: the directory trigger rebuilds the party registry and the
// vouch watch closes sockets. Ended with the log at the bottom of the teardown,
// an identity record landing during it would rebuild a registry over vendor
// wiring the teardown had already stopped.
func (e *Engine) stopViewTriggers() {
	if c := e.core.Load(); c != nil {
		c.stopTriggers()
	}
	e.routedDirectory.end()
}

// stopCore ends this node's core runtime: its directory trigger, if the
// teardown has not already, and then every domain's apply loop. Nil-safe, which is an
// engine built by hand.
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

// shutdown ends everything this runtime started and WAITS for it.
//
// ONE IMPLEMENTATION FOR TWO CALLERS — [Engine.stopCore] and the failure path
// in [Engine.startCore] — because a half-built runtime leaks precisely what a
// built one does: the log's apply loops do not know their node never finished
// booting. Nil-safe throughout, because the failure path can reach it with the
// log not yet built.
//
// THE APPLY LOOPS LAST, after the trigger that reads what they write. A loop
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
// EVERY DATA NODE RUNS IT, whatever its other roles and whether or not it has
// a company, so a node whose identity domain did not come up is a boot failure
// naming the domain rather than a node that quietly serves no sign-in surface.
//
// # The writer acts as THE NODE, and every surface narrows it
//
// Every surface derives its own with [iamdomain.Writer.As]. What is left
// acting as the node is what the node itself does: redeeming an invitation
// into a person who does not exist yet, the duty that sweeps, and the re-seal
// a keyring rotation moves every person's values with. A person's own sign-in
// acts as that person. What the node writes for itself names it under its id
// as the ENGINE ([iamdomain.NodeKind]), so the trail draws it as the system
// rather than as a credential's login.
func (c *core) openIAM(e *Engine, sl *stateLog, nodeID string) error {
	running := sl.Domain(iamdomain.Domain{}.Name())
	if running == nil {
		return fmt.Errorf("engine: this node runs no identity domain, so it " +
			"cannot say who anybody is — the domain is in the register and its " +
			"stream failed to come up")
	}
	reader, err := iamdomain.NewReader(iamdomain.ReaderOptions{
		DB:        sl.estate().Reader(),
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
		Publisher: running.publisher, DB: sl.estate().Reader(),
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
		// THE ORGANISATION THIS NODE RUNS, which EVERY binding the
		// directory writes is checked against as a HUMAN seat — a
		// create's, an invitation's, a redemption's and an identity
		// edit's (ADR-0032) — read per call through the seat's KIND
		// this view answers, so a node with no company yet refuses a
		// bind naming the remedy ([session.ErrNoCompany], which the
		// writer answers as iamdomain.ErrNoCompany) rather than binding
		// unchecked.
		Seats: SeatViewOf(e),
		// WHAT A LANDED RECORD DECIDED — a grant delta, a session
		// generation — goes on the node's audit feed from here, since
		// only the decide holds it. Every surface's [Writer.As] keeps it.
		Events: e.authEvents,
		// AS THE ENGINE, under the node's id: what this writer authors
		// for itself is the engine's housekeeping, never a credential's
		// ([iamdomain.NodeKind]).
		Actor:     nodeID,
		ActorKind: iamdomain.NodeKind,
		// THE NODE IS THE DEPLOYMENT, so it writes as an administrator:
		// the sweeps and evictions, the re-seal a keyring rotation moves
		// everybody's values with, and the two writes about people with
		// no principal of their own in the gesture — the person an
		// invitation redeems into, who does not exist until the
		// redemption lands (the company's first person included), and
		// every person a re-seal rewrites. Every surface replaces this
		// with [iamdomain.Writer.As].
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
// ADMIN AND NOTHING NARROWER, because every record the node writes is one the
// domain decides as an administrator's — a redemption's enrolment, a sweep,
// an eviction, a re-seal — and the failure when the node holds less is that
// nobody can redeem an invitation, the company's first person included,
// since they are invited like anybody else. A PACKAGE-LEVEL VALUE so a test
// can pin it: nothing else compares the node's own authority with what the
// domain's records ask of their writer.
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

// AnyPerson reports whether this company has a person enrolled — `/health`'s
// `identity` and the boot's `iam_unclaimed` line; a service account alone
// leaves it waiting for its first person — with "nobody" PROVED
// against the identity log's end, which is read first
// ([iamdomain.Reader.AnyPerson]).
//
// PROVED, because every data node applies the identity log from boot, company
// or none: a node that has just joined a fleet with people in it holds empty rows
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

// SessionStanding is who holds one named session and whether this node's rows
// hold it live — the question a named sign-out asks — with "nobody holds it"
// PROVED against the identity log's end, which is read first
// ([iamdomain.Reader.SessionStanding]).
//
// PROVED for [Engine.AnyPerson]'s reason: a session opened through a peer is a
// row this node holds only once its applier catches up, and read bare its
// absence told an administrator ending a stolen laptop's session that it had
// ended. A log end this node cannot read is the unknown arm too.
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
// whether they vouch for it at the identity log's end, which is read first
// ([iamdomain.Reader.ResetByID]).
//
// PROVED for [Engine.SessionStanding]'s reason: a link issued through a peer is
// a row this node holds only once its applier catches up, and read bare its
// absence answered the link's holder that it was dead.
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

// StateLogHydrated reports whether this node may claim a NEW seat: whether the
// copies a seat attaching here would act on have caught up.
//
// THE GATE ON SEAT ACQUISITION, and the ONE gate, on every node. A seat whose
// mailbox attached first would answer "there is no such item" to its own tools
// — an answer it acts on by filing a duplicate or abandoning work it was told
// to do. Two halves, asked in this order:
//
//   - THIS NODE'S OWN LOGS, on a data node: every domain whose health gates
//     admission ([statelog.Domain.ReadinessInput]) established STRICTLY,
//     because this is admission rather than a read — a node merely inside the
//     trim floor still serves rows that are behind, and a seat attaching to
//     one acts on them. The core's logs and not the native half's, because the
//     log is the core's: the tracker's and the knowledge base's domains are
//     applied from boot whether or not a company has asked for their writers
//     yet. A node without `data` runs no log and has none to wait for.
//   - THE COPY THAT WILL SERVE THE SEAT'S TOOLS, asked once through the
//     estate's router ([estate.Router.Serves]): this node's own where its copy
//     serves, and otherwise the first data node that answers, which admits
//     only once its own copy is established — the same gate, one hop away. A
//     node whose own copy is WRONG serves nothing ([localEstate.For]), so it
//     is answered on a sound data node's word, like a node holding no data;
//     every half the company runs natively — the tracker, the knowledge base
//     and chat — must be served there, since a seat's tools reach each of
//     them, and a company whose only native half is chat gates on chat alone
//     rather than admitting every seat unconditionally. And the router's view
//     must have listed the fleet at least once: a node that must ask a data
//     node is refused by the router itself when its view cannot name one.
//
// A node running no native half — no company yet, or one on Jira, Confluence
// and a vendor's chat — has nothing for the second half to ask, and an engine
// with neither is trivially hydrated.
//
// IT TAKES A CONTEXT because where this node's copy does not serve the answer
// is a request, and a sweep that is shutting down must not wait on it.
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
	runs, ok := e.nativeHalves()
	if !ok || !runs.any() || e.router == nil {
		return true
	}
	if e.dataView != nil && e.dataView.ListedAt().IsZero() {
		log.DebugContext(ctx, "seat_admission_withheld", "reason", "unroutable",
			"detail", "this node's view of the fleet has not listed it yet")
		return false
	}
	served, err := e.router.Serves(ctx)
	if err != nil || (runs.tracker && !served.Tracker) || (runs.wiki && !served.Pages) ||
		(runs.chat && !served.Chat) {
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

// SeatsServiceable reports whether this node may KEEP the seats it holds: it
// may while it can ROUTE its seats' calls to a copy of the estate, and sheds
// them only once it cannot.
//
// # A wrong copy is not a reason to shed
//
// This used to shed on a copy that was WRONG rather than behind — an applier
// halted at a record it cannot decode, an eviction, rows below the log, a
// checkpoint on another stream, a frozen checkpoint, a record held past the
// deferral grace — because a seat left running on one answered its own tools
// out of a copy the fleet had abandoned. The router makes that the copy's
// problem and not the seats': a wrong copy goes out of service
// ([localEstate.For]), so every call this node's seats make is answered by a
// data node whose copy is sound, exactly as a node holding no data is
// answered. Moving the seats instead cost each of them its processes and its
// memory to be served by the very same peers — and on a single node it stopped
// the company, which is still what happens: an estate no data node serves
// refuses its calls, saying so.
//
// A LAG WAS NEVER ONE OF THEM, and it is not now: being behind is the admission
// gate's business ([Engine.StateLogHydrated]), and when this read "has this
// node's copy ever been whole" from "is it level this instant", one unapplied
// tracker record made a solo node unfit for a heartbeat and moved all seven of
// its seats.
//
// # What a node cannot survive is not being able to route
//
// A node that answers from its OWN copy needs nothing else to route its seats'
// calls — the router asks this node first and asks nobody where it answers —
// so every data node whose copy is sound keeps its seats whatever its view of
// the fleet says. Any other node routes by the watched presence view, and a
// view that answers unknown ([coord.LeaseView.Leases]) and last listed longer
// ago than [statelog.FloorCacheStale] — the age past which nothing may decide
// from a cached coordination fact, so no node sheds sooner than any other
// decider stops trusting the same view — is a node that can no longer say
// where its seats' calls go, which no copy of its own makes up for; its seats
// go to a peer that can. A company whose halves are all on a vendor routes
// nothing, and a view that has never listed sheds nothing: admission claims no
// seat before it does.
//
// THE CONTEXT IS THE SEAT HOST'S, and nothing here waits on it: the verdict a
// sound copy is judged by is the one already held ([localEstate.servesUnasked]),
// never a broker round trip on the sweep. It is passed on all the same, because
// a verdict nobody has read yet starts its first read here, and that read —
// detached from the sweep's cancellation — keeps the sweep's values rather
// than starting from nothing.
func (e *Engine) SeatsServiceable(ctx context.Context) (bool, string) {
	return e.serviceable(ctx, time.Now())
}

// serviceable is [Engine.SeatsServiceable] at now.
func (e *Engine) serviceable(ctx context.Context, now time.Time) (bool, string) {
	runs, ok := e.nativeHalves()
	if !ok || !runs.any() {
		return true, ""
	}
	if e.local != nil && e.local.servesUnasked(ctx) {
		return true, ""
	}
	return routable(e.dataView, now)
}

// ReplicationStatus is one of this node's replication loops, as the fleet view
// counts them.
//
// # Why the shape is the question and not the mechanism
//
// Every loop this node runs is a state-log APPLIER, so every row is one
// domain's. The type is shaped by what the fleet view asks rather than by how
// an applier works, because the question is what an operator reads.
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
// the fleet view: one row per registered domain, on every data node — a node
// that has met no company included, since its state log runs from boot. Empty
// on a node without `data`, which runs no loop, and on an engine with no core
// runtime.
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
// has consumed through it. It returns at once where this node has nothing to
// wait on: a zero position, a node that applies no log (one holding no data,
// whose reads are a data node's), or a log this node does not run.
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
	// THE POSITION NAMES ITS OWN STREAM, so this resolves the log from it
	// rather than taking one. That is what a bucket revision could never
	// do — it was a number on a family, and the caller had to say which —
	// and it is why every domain settles through one primitive.
	running := c.log.logOf(at.Stream)
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

// identityDirectory is the identity directory as THIS node reads a login's
// binding: its own rows on a data node, a data node's through the estate's
// router on one without `data` ([routedIdentity]) — false on an engine with
// neither, which is one built by hand.
//
// ONE ANSWER FOR BOTH KINDS OF NODE, because the callers are the same tools:
// whose record a login names ([Engine.HolderRecord]) is asked by a seat's
// tools on whichever node runs the seat, and a data-less seats node runs
// seats. What the caller then applies — the chart, this node's own Tier A
// tokens, how far the answering rows could vouch — is the caller's, on this
// node, whichever rows answered.
func (e *Engine) identityDirectory() (bindingDirectory, bool) {
	if c := e.core.Load(); c != nil && c.iamReader != nil {
		return c.iamReader, true
	}
	if e.router != nil && e.local == nil {
		return routedIdentity{identity: e.router.Identity()}, true
	}
	return nil, false
}

// routedIdentity is the identity directory on a node without `data`: the two
// questions such a node puts to it — the row a login binds, and who holds each
// seat — asked of a data node through the estate's router, floored on the
// identity log like any read of rows a log derives (internal/estate).
//
// FACTS AND NEVER VERDICTS cross: the serving node answers the row, narrowed
// to the binding, and how far its own rows could vouch for it; the verdict
// formed from them — through this node's chart and this node's Tier A tokens —
// is formed here, by the very functions a data node forms it with over its own
// rows ([holderRecordOf], [boundRowOf], [notify.Standing]).
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
// node without `data` reads it, and starts the net that re-reads it.
//
// HANDED OVER BEFORE THE FIRST REGISTRY IS BUILT, for the core's reason: a
// node that has never read its directory withholds every human seat until it
// does, and a registry built before the hand-over would be the chart-only one,
// which routes a suspended person's word to their seat.
//
// NO POSITION, because there is no applier here whose position could say the
// rows moved: the net reads the directory on every tick of
// [DirectoryRefresh], which the alarm table already calls a stall — a data
// node's registry is no fresher than that when its applier's signal is lost.
// A data node never calls this; its core starts the trigger over its own rows.
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
