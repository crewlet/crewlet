package api

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/colleague"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// engineOperatorWriter is the tracker's writer acting as one operator, through
// this node's estate router ([engine.OperatorWorkWriter]).
//
// ONE HELPER FOR THE TEN SEAMS BELOW, because turning a tool-layer actor
// into a writer is a single rule and ten hand-copied spellings of it are
// ten chances for one seam to carry an identity the other nine do not —
// which is exactly what happened to [tracker.Provenance.Seat], the field that
// decides whose person record a write lands on.
func engineOperatorWriter(as func(estate.Actor) estate.WorkWriter, actor builtin.Actor) estate.WorkWriter {
	// THE CREDENTIAL AND THE PERSON IT NAMES, which are two different
	// facts: the author stays the token, and the seat is only ever the
	// subject of that person's own state. Through the actor's own
	// provenance, the one construction a seat's writers use too, so the two
	// surfaces cannot carry different trails for one actor.
	return as(estate.Actor{Handle: actor.Handle, Kind: actor.Kind, Provenance: actor.Provenance()})
}

// EngineOperators is the operator surface over a running engine — the one tool
// catalogue both operator transports serve ([OperatorSurface]) — BUILT AGAIN
// whenever what it is built from changes.
//
// HERE, beside the seam it fills and beside [NewEngineRuntime], rather than in
// the command that wires the node, for that constructor's reason: `crewlet run`
// and the end-to-end suite must serve `/operator/act` and `/operator/mcp` from
// the same code. A harness that assembled its own catalogue would certify what
// it wired rather than what a node serves — and the replay's session floor is
// read off exactly this surface's answer.
//
// # Built again, rather than once
//
// It was built once, when the API was assembled, from whatever the engine ran
// at that instant. On a node that booted with no company that was no tracker,
// no knowledge base and no knowledge backend — those arrive with the first
// apply, after the API exists — so its people were offered the fleet's own
// gestures and nothing else until a restart, and the person the live bootstrap
// binds could not file a task from the dashboard. A knowledge backend can also
// come and go with any revision. So every request asks [EngineOperators.Operator],
// which reads the facts the catalogue is gated on ([operatorKey]) and builds a
// new catalogue when they differ from the ones the current one was built from.
// That costs a request one reading of the engine — a few atomic loads, and one
// short lock where the company's knowledge backend is a vendor's — and only
// when a fact moves a second reading and a build: at a node's first apply, and
// when a revision adds or removes the company's knowledge backend or renames
// the company. A knowledge backend a revision RECONFIGURES — another
// Confluence site or space, a rotated credential — builds nothing, since only
// whether there is one is a fact the catalogue is built from: its two knowledge
// tools reach the searcher the engine holds at each call ([engineKnowledge]).
//
// ONE CATALOGUE PER SERVER, both transports over it, swapped together, so a
// request is served by one whole catalogue and never by half of two. A swap
// ends no client's connection: the MCP transport keeps no session (see
// [operator.MCPPattern]), so a client connected before it is answered by the
// new catalogue at its next request. What a client can leave on a catalogue
// is the stream a 2026-07-28 client holds open to hear that its tools changed,
// and a swap says so on every such stream on the node ([operator.Listeners])
// AND ON EVERY OTHER NODE ([EngineOperators.tell]) — and, since nothing would
// otherwise build again until a request came, every apply asks for the swap
// ([EngineOperators.Refresh]).
//
// # Told by the fleet, not by one node
//
// Every node applies a revision on its own clock — the activation reaches
// each by a nudge and a poll, and a company's first apply builds the native
// runtime, which takes seconds — while an assistant's next listing, with no
// session to pin it, reaches whichever ingress node a load balancer picks.
// Told only by the node holding its stream, an assistant listed again at once
// and was often answered by a peer that had not applied yet; that peer's own
// rebuild later told only ITS listeners, so the assistant kept the old tools
// until the next change: after a knowledge backend was removed it went on
// offering `search_knowledge`, answered "unknown tool". So each swap is
// announced to the fleet (`operator_tools_changed`), and every node that hears
// a peer's tells every stream it holds, on whichever catalogue — the current
// one included, since a client connected after this node's own swap may have
// listed from a peer that had not swapped. The last thing an assistant hears
// therefore follows the last node's rebuild, and the listing it makes then is
// the new catalogue wherever it lands — for as long as its stream is open and
// the broker carries the nudge, which is best effort like every nudge.
//
// THE SAME DEPS A SEAT'S TOOLS GET, with one field different: the actor. That
// is what makes this one implementation of every tool rather than two — see
// [builtin.WorkDeps.Actor].
//
// The DEFAULTS are deliberately absent. A seat files into its unit's project
// when it names none, because a seat HAS a unit; an operator does not, so the
// argument is required and the tool refuses naming it rather than guessing a
// project on a person's behalf.
//
// Which UNIT the work is filed into is not a default of this surface and no
// longer needs one: the tracker reads it off the project's own row at the
// write, so an operator's item belongs to the team that owns the project it
// named. It used to be stamped from the caller's own team, which an operator
// has not got — so every item filed here read "Filed into: no unit" beside a
// project page naming its unit.
//
// THROUGH THIS NODE'S ESTATE ROUTER, every read and every write, as a seat's
// tools are ([engine.OperatorWork]) — so an operator's assistant on a node
// whose copy of an operation's domain is out of service is answered from a
// peer's copy of it (ADR-0031), not from the copy this node stopped serving.
type EngineOperators struct {
	// basis reads, at this instant, everything a catalogue is built from,
	// and build builds one from it: over the engine, [operatorBasisOf] and
	// [operator.New] of [operatorBasis.options]. Two functions rather than
	// the engine itself, so the rule below — when a catalogue is built again,
	// and what a failed build leaves — is held by a test that moves the facts
	// without standing an engine up.
	basis func() operatorBasis
	build func(operatorBasis, *operator.Listeners) (*operator.Server, error)

	// current is the catalogue serving now and the key it was built under,
	// one value swapped whole, so a request reads a server and the facts it
	// was built from together, with one atomic load and no lock.
	current atomic.Pointer[builtOperators]

	// rebuild serialises building: a request that finds the facts moved
	// builds, and every other request finding the same takes the catalogue
	// it built rather than building its own.
	rebuild sync.Mutex

	// listeners is every stream an MCP client listens on at this node,
	// whichever catalogue answered it: one registry, handed to every
	// catalogue this surface builds.
	listeners *operator.Listeners

	// fleet carries this node's swaps to every other node and theirs to
	// this one. The zero value is a node alone: its own swaps are told to
	// its own listeners and to nobody else.
	fleet operatorFleet
}

// operatorFleet is how one node's operator surface hears of, and tells, every
// other node's rebuilds ([types.OperatorToolsChanged]).
type operatorFleet struct {
	// stream is where the nudge is published and heard; nil for a node
	// alone.
	stream announcements
	// node is this node's id — the origin its queue stamps on every event it
	// publishes ([events.Event.Node]) — so a node does not hear its own.
	node string
	// detach ends the subscription; nil where none was made.
	detach queue.Unsubscribe
	// telling is every publish of this node's nudge still in flight.
	telling sync.WaitGroup
}

// announcements is the event stream the fleet's rebuilds travel on: the
// engine's queue.
type announcements interface {
	Publish(ctx context.Context, topic string, ev *events.Event) error
	SubscribeStream(ctx context.Context, pattern string, h queue.StreamHandler) (queue.Unsubscribe, error)
}

// operatorToolsTopic is where a node's rebuild is announced to the fleet.
var operatorToolsTopic = topics.Event(types.OperatorToolsChanged{}.EventType())

// fleetTellBudget bounds the publish of one rebuild's nudge.
//
// FIVE SECONDS, the bound the engine gives a lifecycle announcement on the
// same event stream: a publish a healthy broker acknowledges in milliseconds,
// and one a broker that will not acknowledge must not hold a goroutine for
// longer than a person would wait for the dashboard beside it. A nudge lost to
// it costs listeners elsewhere this one rebuild, never a node's catalogue.
const fleetTellBudget = 5 * time.Second

// builtOperators is one catalogue and the facts it was built from.
type builtOperators struct {
	key operatorKey
	// server is the catalogue, nil where it serves nothing — which
	// [operator.New] answers only for a node offering no tool at all, never
	// one beside an engine, since the fleet's own gestures are always served.
	server *operator.Server
}

// NewEngineOperators builds the operator surface over e, refusing a catalogue
// [operator.New] refuses — a wiring mistake ([operator.ErrNoAudit]) that no
// fact about the company can change, and so is the node's to refuse at boot
// rather than a request's to discover — and joins it to the fleet's
// announcements on the engine's queue ([EngineOperators.join]).
//
// The caller owns what it starts: [EngineOperators.EndListens] as the API
// listener shuts down, then [EngineOperators.Close] before the engine closes
// its queue.
func NewEngineOperators(ctx context.Context, e *engine.Engine) (*EngineOperators, error) {
	o, err := newOperators(
		func() operatorBasis { return operatorBasisOf(e) },
		func(basis operatorBasis, listeners *operator.Listeners) (*operator.Server, error) {
			opts := basis.options(e)
			opts.Listeners = listeners
			return operator.New(opts)
		})
	if err != nil {
		return nil, err
	}
	o.join(ctx, e.Backends().Queue, e.Node().ID())
	return o, nil
}

// newOperators is [NewEngineOperators] over any reading and builder, alone:
// a surface that tells nobody else of its swaps until [EngineOperators.join].
func newOperators(basis func() operatorBasis,
	build func(operatorBasis, *operator.Listeners) (*operator.Server, error),
) (*EngineOperators, error) {
	listeners := operator.NewListeners()
	read := basis()
	server, err := build(read, listeners)
	if err != nil {
		return nil, err
	}
	o := &EngineOperators{basis: basis, build: build, listeners: listeners}
	o.current.Store(&builtOperators{key: read.key(), server: server})
	logOperatorsBuilt(server)
	return o, nil
}

// join attaches this surface to the fleet's announcements on stream, as the
// node named node: its own swaps are published there, and every other node's
// are heard ([EngineOperators.hear]).
//
// A SUBSCRIPTION THAT WILL NOT ATTACH IS NOT FATAL, and saying so is the
// design, as for every nudge between nodes: this node still tells its own
// listeners of its own swaps and still tells its peers of them, and what it
// costs is that a listener here is not told of a peer's rebuild.
func (o *EngineOperators) join(ctx context.Context, stream announcements, node string) {
	o.fleet.stream, o.fleet.node = stream, node
	detach, err := stream.SubscribeStream(ctx, operatorToolsTopic, o.hear)
	if err != nil {
		log.WarnContext(ctx, "operator_tools_nudge_unavailable", "error", err,
			"detail", "an MCP client listening on this node is not told when another "+
				"node rebuilds its operator tools, only when this one does")
		return
	}
	o.fleet.detach = detach
}

// Close detaches this surface from the fleet's announcements and waits for
// the nudges it is still publishing, bounded by ctx. The node runs it as its
// API comes down, before the engine closes the queue both ride.
func (o *EngineOperators) Close(ctx context.Context) {
	if o.fleet.detach != nil {
		// WithoutCancel: a stop is routinely driven by the cancellation it is
		// cleaning up after, and a detach that inherited it would leave the
		// subscription behind on the broker.
		if err := o.fleet.detach(context.WithoutCancel(ctx)); err != nil {
			log.WarnContext(ctx, "operator_tools_nudge_detach_failed", "error", err)
		}
	}
	done := make(chan struct{})
	go func() {
		o.fleet.telling.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// EndListens ends every stream an MCP client listens on at this node
// ([operator.Listeners.End]). The node runs it as its API listener begins to
// shut down: a stream is one request held open, and the listener's graceful
// shutdown would otherwise wait for every assistant listening until its grace
// ran out and then cut them with a warning meant for a request that was stuck.
func (o *EngineOperators) EndListens() { o.listeners.End() }

// Operator answers the catalogue that serves this request: the current one
// where the facts it was built from still hold, and otherwise one built now
// from the facts as they are.
//
// THE FACTS ARE READ AGAIN UNDER THE REBUILD LOCK, and a build is made from
// that reading, never from the one that sent the request there. The first
// reading may be held between taking the facts and the lock while they move
// again and another request builds for where they moved to: built from it, the
// held request put a catalogue of the OLDER facts back over the newer one, and
// the next request built the newer one a second time. Read under the lock,
// where builds are serialised, a catalogue is only ever replaced by one built
// from facts read after it was.
//
// A BUILD THAT FAILS keeps the previous catalogue, which is still a correct
// one for what it offers, and is recorded under the new facts so that the next
// request does not try again (and log again) until they move once more:
// [operator.New] refuses only a wiring mistake, which [NewEngineOperators]
// already refused at boot, so this is a path no node is expected to take.
//
// IT TAKES NO CONTEXT, because what it may set off is owed to everybody but
// its caller: a swap it makes is told to every listener and to the fleet in
// the background, whatever becomes of the request that found the facts moved.
func (o *EngineOperators) Operator() (*operator.Server, bool) {
	built, _ := o.serve(context.Background())
	return built.server, built.server != nil
}

// serve is [EngineOperators.Operator], saying whether it swapped.
//
// A SWAP IS TOLD AT ONCE, to every stream on this node and to the fleet: a
// swap whose new catalogue offers the same tools as the old — a rename only
// retitles the server — is told all the same, for the price of one listing per
// listener, and swaps are a node's applies. ctx lends what it carries to the
// telling and never its cancellation ([operator.Listeners.Announce]).
func (o *EngineOperators) serve(ctx context.Context) (*builtOperators, bool) {
	if built := o.current.Load(); built.key == o.basis().key() {
		return built, false
	}
	o.rebuild.Lock()
	defer o.rebuild.Unlock()
	basis := o.basis()
	key := basis.key()
	previous := o.current.Load()
	if previous.key == key {
		return previous, false
	}
	server, err := o.build(basis, o.listeners)
	if err != nil {
		log.Error("operator_surface_not_built", "error", err,
			"detail", "the operator's catalogue could not be built for what this "+
				"node now runs; the previous one is still served")
		built := &builtOperators{key: key, server: previous.server}
		o.current.Store(built)
		return built, false
	}
	built := &builtOperators{key: key, server: server}
	o.current.Store(built)
	logOperatorsBuilt(server)
	o.listeners.Announce(ctx)
	o.tell(ctx)
	return built, true
}

// Refresh builds the catalogue again where a fact it is built from has moved,
// and so tells every MCP client listening that its tools changed. The node
// runs it after every config apply ([engine.Engine.SetOnApplied]), which is
// where each of those facts moves: the native runtime arriving at a node's
// first company, the knowledge backend, the company's name. A node's mode,
// which gates the writers, is fixed at boot.
//
// [EngineOperators.Operator] alone builds only when a REQUEST finds the facts
// moved, and a connected assistant makes none while it waits: its stream is
// held open on the catalogue it connected to, and nothing told it anything
// until some other caller happened to ask — the dashboard's `viewer`, a press,
// another assistant's call. Built here, the apply that changes what the node
// offers is the moment its listeners hear of it.
func (o *EngineOperators) Refresh(ctx context.Context) { o.serve(ctx) }

// tell announces this node's swap to every other node, in the background, so
// no request that found the facts moved waits on the broker — on ctx's values
// and never its cancellation, for [operator.Listeners.Announce]'s reason. Best
// effort, like every nudge: a publish that fails costs listeners elsewhere
// this one swap.
func (o *EngineOperators) tell(ctx context.Context) {
	if o.fleet.stream == nil {
		return
	}
	detached := context.WithoutCancel(ctx)
	o.fleet.telling.Go(func() {
		ctx, cancel := context.WithTimeout(detached, fleetTellBudget)
		defer cancel()
		// The ORIGIN is the queue's stamp (events.Event.Node), which is what
		// a hearer compares with its own; Source says who, as every nudge's
		// does.
		ev := events.New(types.OperatorToolsChanged{}, events.TraceContext{})
		ev.Source = o.fleet.node
		if err := o.fleet.stream.Publish(ctx, operatorToolsTopic, ev); err != nil {
			log.WarnContext(ctx, "operator_tools_nudge_not_published", "error", err,
				"detail", "MCP clients listening on other nodes are not told of this "+
					"node's new operator tools until the next rebuild anywhere")
		}
	})
}

// hear is a peer's swap arriving: every stream on this node is told, whichever
// catalogue it is on — the current one too, since a client connected here
// after this node's own swap may have listed from the peer before IT swapped.
//
// THIS NODE'S OWN FACTS ARE READ FIRST, so a node that has applied the same
// revision and not yet run its apply hook swaps now rather than telling its
// listeners to list a catalogue it is about to replace; such a swap tells them
// itself.
func (o *EngineOperators) hear(ctx context.Context, _ string, ev *events.Event) {
	if ev == nil || ev.Node == o.fleet.node {
		return
	}
	if _, swapped := o.serve(ctx); !swapped {
		o.listeners.Announce(ctx)
	}
}

// Acts names the tools the act transport serves NOW — [operator.Server.Acts]
// of the catalogue a request would be served by — for the `viewer` answer, so
// a screen enables exactly the controls a press of would reach.
func (o *EngineOperators) Acts() []string {
	server, ok := o.Operator()
	if !ok {
		return nil
	}
	return server.Acts()
}

// logOperatorsBuilt says what a catalogue offers, once, where it is built.
func logOperatorsBuilt(server *operator.Server) {
	log.Info("operator_surface_built", "tools", server.Tools(), "acts", server.Acts(),
		"detail", "the operator's MCP endpoint and the dashboard's act transport "+
			"serve this catalogue")
}

// operatorKey is every fact the operator catalogue is BUILT from rather than
// asking on each call: which of the company's halves this node serves, reads
// and writes apart, whether the ranked item search is served, whether the
// company has a knowledge backend, and the company's name, which titles the
// MCP server. Everything else the catalogue's tools read — the chart, the
// clock, the roster, the mentions, the knowledge searcher itself — is resolved
// per call, and the object store and the queues are the process's.
//
// THE ONE PREDICATE both readers read: [operatorBasis.key] is what a request
// compares, and [operatorBasis.options] gates every seam on the same reading,
// so the catalogue a key names is exactly the one built under it.
type operatorKey struct {
	tracker, trackerWrites bool
	search                 bool
	wiki, wikiWrites       bool
	knowledge              bool
	company                string
}

// operatorBasis is ONE READING of everything [operatorKey] names, with the
// seams themselves beside the facts.
type operatorBasis struct {
	// reads is the tracker's and the knowledge base's read seams and which
	// of them this node serves, the ranked search among them.
	reads engine.OperatorReads
	// workWriter writes the tracker as one operator, nil where this node
	// hands out no tracker writer.
	workWriter func(estate.Actor) estate.WorkWriter
	// pageWriter writes the knowledge base, where pagesWritable.
	pageWriter    estate.Pages
	pagesWritable bool
	// knowledge says the company has a knowledge backend, native or not.
	knowledge bool
	// company is the company's name, "" where none is applied.
	company string
}

// operatorBasisOf reads e once for every fact the catalogue is built from.
func operatorBasisOf(e *engine.Engine) operatorBasis {
	basis := operatorBasis{reads: engine.OperatorNative(e), knowledge: e.Knowledge() != nil}
	if writer, writable := engine.OperatorWorkWriter(e); writable {
		basis.workWriter = writer
	}
	basis.pageWriter, basis.pagesWritable = engine.OperatorPageWriter(e)
	if c := e.Company(); c != nil && c.Config != nil {
		basis.company = c.Config.Name
	}
	return basis
}

// key is the facts b holds, for a request to compare with the catalogue's.
func (b operatorBasis) key() operatorKey {
	return operatorKey{
		tracker: b.reads.Tracker, trackerWrites: b.reads.Tracker && b.workWriter != nil,
		search: b.reads.Search,
		wiki:   b.reads.Wiki, wikiWrites: b.reads.Wiki && b.pagesWritable,
		knowledge: b.knowledge,
		company:   b.company,
	}
}

// EngineOperatorOptions is what the catalogue over e is built from at this
// instant — exported so a test can see which seams the operator's transports
// are handed.
func EngineOperatorOptions(e *engine.Engine) operator.Options {
	return operatorBasisOf(e).options(e)
}

// options is the catalogue's deps from this one reading of e: every seam
// gated on the fact [operatorBasis.key] carries for it.
func (b operatorBasis) options(e *engine.Engine) operator.Options {
	key := b.key()
	// EVERY CALL THAT MAY WRITE is audited onto this node's own queue, so
	// the event store here holds who did what through either transport.
	opts := operator.Options{Audit: e.Backends().Queue, Company: key.company}
	// THE CHART, resolved per call because a config apply replaces it —
	// and wired UNCONDITIONALLY, which it was not. It used to be set only
	// where the company had a knowledge backend, because `search_knowledge`
	// was the only tool that read it. WHO THE CALLER IS reads it on every
	// call now: the same chart says which seat a token is bound to, and
	// without it a bound founder's own marks, pins and queue were written
	// under their credential's name instead of theirs. The engine's own
	// reading of it ([engine.Engine.Chart]), which the code host's persons
	// read too, so a token names one person on every surface.
	opts.Org = e.Chart
	// AND WHAT A `${VAR}` IN IT RESOLVES TO, through this node's own chain:
	// a binding sealed in the secret store binds its person here as it does
	// for the seat's tools and the dashboard's reads.
	opts.Env = e.LookupSecret
	// A PARKED CODING RUN IS ANSWERABLE BY ITS TURN from here, on every
	// node and whatever this company's sandbox configuration: the record is
	// the fleet's, and the answer is carried out by the node holding the
	// run's seat, which reads it off that seat's inbox. The person answering
	// is the one the credential names, resolved as every other write here.
	opts.Runs = builtin.RunDeps{
		Desk: sandbox.AnswerDesk{
			Pending: sandbox.NewCoordStore(e.Backends().Fleet),
			Queue:   e.Backends().Queue,
		},
		Actor: operator.WorkActor(opts.Org, opts.Env),
	}
	// AND A SEAT IS PAUSED OR RESUMED from here, on every node: the pause
	// is the fleet's record, and each node holding or later acquiring the
	// seat carries it out from its own watched copy. The change is
	// announced onto this node's queue by whichever caller's write won.
	opts.Pauses = builtin.SeatPauseDeps{
		Pauses:   e.Backends().Fleet,
		Announce: e.Backends().Queue,
		Org:      opts.Org,
		Actor:    operator.WorkActor(opts.Org, opts.Env),
	}
	// AND A NOTE REACHES A RUNNING TURN from here, on every node: it is
	// scattered to the fleet and answered by the node running the turn,
	// which is the only one that can hand it to the turn's next round.
	opts.Steer = builtin.SteerDeps{
		Asker: e.Backends().Queue,
		Actor: operator.WorkActor(opts.Org, opts.Env),
	}
	// THE READS WHEREVER THIS NODE SERVES THEM, THE WRITES ONLY WHERE IT
	// HANDS OUT A WRITER: two facts, so two gates. A node in a maintenance
	// mode serves the tracker's and the knowledge base's reads and hands no
	// writer to any surface ([engine.OperatorWorkWriter], behind the engine's
	// one writer gate). Gated on both at once, this catalogue offered such a
	// node's operator no read at all, so for the whole of a capacity window
	// their assistant could not read a board the dashboard beside it was
	// drawing, nor list a project's files the REST route was answering. Every
	// tool is gated on its own seam ([builtin.OperatorTools]), so a reader
	// handed without a writer serves exactly the reads.
	if key.tracker {
		reader := b.reads.Work
		opts.Work = builtin.WorkDeps{
			Reader: reader,
			// AND THE PROJECT'S FILES — the rows through the estate
			// router, the bytes through this node's object client.
			Files:   reader,
			Objects: e.ObjectStore(),
			// AND THE INBOX READ. It takes no actor for the reason
			// Search takes none — it reads, and whose inbox is an
			// argument rather than an identity — and it is this
			// surface's alone beside the person writer, because a
			// seat has a mailbox rather than an inbox.
			Inbox: reader,
			// THE ROSTER, so an operator's assistant is refused a
			// handle nobody has rather than silently filing work for
			// one — the same check every seat's tools make.
			Seats: func() []colleague.Seat {
				c := e.Company()
				if c == nil {
					return nil
				}
				return builtin.Corpus(c.Org, opts.Env)
			},
			// AND THE PARTY BEHIND A HANDLE, which the roster above
			// deliberately does not carry: an operator reading a
			// person's inbox or their own state is answered about
			// BOTH the names that person's rows may be filed under,
			// and the credential is an attribution key rather than
			// somewhere an agent could mention them.
			Party: builtin.Parties(opts.Org, opts.Env),
			// AND THE THREE CHART SEAMS THE SEAT SURFACE HAS AND THIS
			// ONE WENT WITHOUT. Their absence was invisible and not
			// harmless: with no Leads, an operator filing an unassigned
			// task woke nobody at all — the lead fallback is what
			// catches exactly that task — and with no Units every
			// project this surface listed read as belonging to no team.
			Leads: engine.LiveLeads(e),
			Units: engine.LiveUnits(e),
			// A PERSON'S CREATE THAT NAMES NO PROJECT lands where their
			// seat's work does — the derivation every seat's surface and
			// the dashboard's `viewer` use, read per call against the
			// current chart because this surface outlives every apply.
			DefaultProject: engine.LiveDefaultProject(e),
			// THE SAME CHART DECIDES WHO IS WRITING: a token bound to
			// a human seat writes that PERSON's own state, while the
			// author on the record stays the token.
			Actor: operator.WorkActor(opts.Org, opts.Env),
			// THE MENTION RESOLVER, which this surface went without: a
			// comment's @-mention is turned into a wake by the tracker's
			// recipients only when the writer resolved it, so an
			// operator writing "@alice can you take this" reached her
			// watchers and never her — while the tool's own description,
			// which their assistant reads, promised it would.
			Mentions: engine.LiveMentions(e),
			// AND THE COMPANY'S CLOCK, which this surface went without
			// entirely: every `due=friday`, `due=overdue` and relative date
			// an operator's assistant wrote resolved on UTC while a seat's
			// own resolved on the company's zone. Read per call, because
			// a catalogue outlives every apply that leaves what it is
			// built from alone, and an apply can move the clock
			// (ADR-0018).
			Zone: e.Zone,
			// THE ROUTER'S SESSION FLOOR, which every write raises
			// ([operatorWorkWriters]) — never this node's own applier,
			// which is not the one that answers the next read once its
			// copy is out of service.
			Await: e.AwaitEstate,
		}
		// AND THE RANKED SEARCH, where this node serves it
		// ([engine.OperatorReads.Search]: the tracker and an index) —
		// through the same reader, which is the seam it is. It reads, so
		// it takes no actor: the corpus is the same for everybody and
		// there is nothing to attribute. Set only here, never as a typed
		// nil, since the catalogue offers the search wherever it is set.
		if key.search {
			opts.Work.Search = reader
		}
		if key.trackerWrites {
			operatorWorkWriters(&opts.Work, b.workWriter)
		}
	}
	// THE KNOWLEDGE BASE ON THE TRACKER'S RULE: its reads wherever this node
	// serves them, its writer only beside them where one is handed out.
	if key.wiki {
		opts.Pages = builtin.PageDeps{
			Reader:   b.reads.Pages,
			Actor:    operator.PageActor,
			Mentions: engine.LiveMentions(e),
			// THE CONTAINERS NO WRITE MAY TARGET, read off the company
			// at each write: both are Tier B, and this catalogue is not
			// rebuilt for a new company document.
			Reserved: engine.LiveReserved(e),
			Await:    e.AwaitEstate,
		}
		if key.wikiWrites {
			opts.Pages.Writer = b.pageWriter
		}
	}
	// SEARCH IS OFFERED WHENEVER THE COMPANY HAS A BACKEND, native or not:
	// unlike the ten write tools, ranked search over the company's own
	// wiki is exactly as useful to an operator's assistant on Confluence.
	if key.knowledge {
		// THE CHART THE SEARCH IS SCOPED AGAINST is already wired
		// above, for every company rather than only this one.
		opts.Knowledge = engineKnowledge{engine: e}
		// AND A PERSON'S QUESTION ANSWERED FROM IT, on the auxiliary model
		// of their own seat and charged to the company's windows — a
		// person has no seat budget. The answers are cached at this node's
		// corpus position, which a company on an external wiki does not
		// have, so there they are never cached.
		opts.Answer = builtin.AnswerDeps{
			Models: engine.AnswerModels(e),
			Budget: engine.AnswerBudget(e),
			Corpus: e.KnowledgeCorpus,
			Actor:  operator.WorkActor(opts.Org, opts.Env),
			// A source too long to read whole is condensed for the
			// question, in the node's one rewrite cache.
			Rewrites: e.Rewrites(),
		}
	}
	// THE LEAD RELATION, which the tracker deliberately does not derive:
	// it holds no org chart, and one it derived would be a second opinion
	// about the hierarchy.
	opts.Leads = engineLeads(e)
	// AND THE PROJECT'S OWN LEAD, which is a different question: one is
	// about a person's line, the other about who plans a container's work.
	opts.LeadsProject = engine.LeadsProjectOf(e)
	return opts
}

// operatorWorkWriters hands deps every tracker seam that WRITES — each one
// resolved for the one operator a call names, through [engineOperatorWriter] —
// and nothing that reads, which [EngineOperatorOptions] hands on a fact of
// its own: a node serves the reads where it hands out no writer.
func operatorWorkWriters(deps *builtin.WorkDeps, writer func(estate.Actor) estate.WorkWriter) {
	// THE OPERATOR'S OWN CREDENTIAL IS THE PARTY, and it comes from the
	// request's context rather than from the call: a tracker whose author
	// field is chosen by the writer is not an audit trail, and there is
	// deliberately no way to name a seat to act as.
	deps.Writer = func(actor builtin.Actor) builtin.WorkWriter {
		return engineOperatorWriter(writer, actor)
	}
	// AND THE TWO SEQUENCES, which this surface went without — so an
	// operator's assistant was refused `waiting_on` and `blocking` by name
	// on a tool whose own description offers them, and would not have been
	// served the fold at all. Both need the replicated estate, which this
	// writer has; nothing else about them differs from a seat's.
	deps.Dependencies = func(actor builtin.Actor) builtin.WorkDepender {
		return engineOperatorWriter(writer, actor)
	}
	deps.Merges = func(actor builtin.Actor) builtin.WorkMerger {
		return engineOperatorWriter(writer, actor)
	}
	// AND THE CROSS-PROJECT MOVE, a sequence of its own: a top-level item
	// and its subtree re-keyed into another project. A seat holds it too,
	// behind the project lead's gate; see internal/agent/builtin/workmove.go.
	deps.Moves = func(actor builtin.Actor) builtin.WorkMover {
		return engineOperatorWriter(writer, actor)
	}
	// AND A PROJECT FILE'S UPLOAD AND REMOVAL: the row through the router,
	// the bytes through the object client the reads already hold.
	deps.FileWriter = func(actor builtin.Actor) builtin.FileWriter {
		return engineOperatorWriter(writer, actor)
	}
	// THE SAVED-VIEW WRITER, which only this surface has: a view is
	// furniture a person arranges, and no seat is given the tools that
	// reach it.
	deps.ViewWriter = func(actor builtin.Actor) builtin.ViewWriter {
		return engineOperatorWriter(writer, actor)
	}
	// AND THE CATALOGUE WRITER: the company's own vocabulary is a person's
	// to set, never a seat's to widen so its own create succeeds.
	deps.CatalogueWriter = func(actor builtin.Actor) builtin.CatalogueWriter {
		return engineOperatorWriter(writer, actor)
	}
	// AND THE PERSON WRITER. Who may write what is the tracker's own rule;
	// what this surface supplies is the identity it is judged against.
	deps.PersonWriter = func(actor builtin.Actor) builtin.PersonWriter {
		return engineOperatorWriter(writer, actor)
	}
	// AND THE TRASH. A removal takes an item off every board in the company
	// and a restore puts it back at any age; neither destroys anything,
	// which is what separates both from the purge the CLI guards with a
	// typed confirmation. No seat holds either — see
	// internal/agent/builtin/worktrash.go.
	deps.TrashWriter = func(actor builtin.Actor) builtin.TrashWriter {
		return engineOperatorWriter(writer, actor)
	}
	// AND THE BOARD DRAG. A card's place in its project's order is a
	// person's arrangement, so no seat holds it either — see
	// internal/agent/builtin/workplace.go.
	deps.Placer = func(actor builtin.Actor) builtin.WorkPlacer {
		return engineOperatorWriter(writer, actor)
	}
	// AND A PROJECT'S OWN SETTINGS. Unlike the five above, this one is on
	// every surface — declaring a tag is open to every seat — and what an
	// operator adds here is the credential the archive facet asks for.
	deps.ProjectWriter = func(actor builtin.Actor) builtin.ProjectWriter {
		return engineOperatorWriter(writer, actor)
	}
}

// engineLeads answers whether one handle leads another, walking the chart's own
// management chain.
//
// ANY ANCESTOR, not just the direct manager: a founder leads everybody, and an
// authority that stopped at one level would make "a lead may set what somebody
// in their line does next" mean "a lead may, for the people directly under
// them" — which is not what a line is.
//
// A HANDLE THIS BUILD CANNOT RESOLVE ANSWERS FALSE, which is the conservative
// direction: the write is then refused unless it is the person's own.
func engineLeads(e *engine.Engine) builtin.Leads {
	return func(_ context.Context, actor, handle string) bool {
		c := e.Company()
		if c == nil || c.Org == nil || actor == "" || actor == handle {
			return false
		}
		seat := c.Org.SeatByHandle(handle)
		if seat == nil {
			return false
		}
		for _, manager := range c.Org.Ancestors(seat) {
			if manager.Handle() == actor {
				return true
			}
		}
		return false
	}
}

// engineKnowledge resolves the node's searcher per call, for the reason the
// engine's own liveKnowledge does: an apply REPLACES it, and a value captured
// when the API was assembled searches with a rotated credential's predecessor.
type engineKnowledge struct{ engine *engine.Engine }

func (k engineKnowledge) CanSearch(seat *org.Role, o *org.Organization) bool {
	s := k.engine.Knowledge()
	return s != nil && s.CanSearch(seat, o)
}

func (k engineKnowledge) Search(ctx context.Context, q knowledge.Query) knowledge.Result {
	s := k.engine.Knowledge()
	if s == nil {
		return knowledge.Result{}
	}
	return s.Search(ctx, q)
}
