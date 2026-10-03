package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
	"github.com/crewlet/crewlet/internal/usage"
)

// THE REGISTER: every domain this build runs, and everything the engine has to
// know to run one, in ONE fixed-order table.
//
// # Why one table rather than a list plus four switches
//
// A domain used to be declared in five places: the list itself, the applier
// switch, the write-authority switch, the barrier switch and the Tier A
// ceiling switch. Three of those refuse a domain they do not know, naming it,
// so forgetting them is a boot failure with an explanation. THE BARRIER SWITCH
// DOES NOT: a domain missing from it silently gets no read index, and
// therefore no `linearizable`, which is a correct answer for one domain
// (the vectors are derived and compacted, so "as of a position" is not a
// question about them) and a silent wrong one for every other. A reviewer
// cannot tell the two apart by reading the switch, because absent and
// deliberately-absent look identical.
//
// So the barrier decision moves into the entry and becomes IMPOSSIBLE TO
// OMIT: an entry states an encoder or states [registration.NoBarrier], and
// [checkRegister] refuses one that states neither or both. That is the whole
// reason the table exists — the other four collapse into it because a domain
// declared in one place is read in one place.
//
// THREE MORE WOULD HAVE BEEN SWITCHES, and are fields instead: the record a
// REANCHOR opens a new generation with ([registration.Generation]), the writer
// the NODE GATE publishes an eviction through ([registration.NewGate]) and the
// WAKE FEED over the log ([registration.Feed]). Each fails the way the barrier
// switch did — a switch that did not know a domain answered "no record", "no
// gate" or "no feed" for it, and the log nobody could re-anchor or evict, or
// the trim waiting for ever on a consumer nobody opens, was found by an
// operator mid-incident — so each is a field [checkRegister] refuses to see
// wrong: every entry states its generation encoder, every identity-claiming
// entry its gate, and every entry a feed exactly when its domain declares one.
//
// # The order is still load-bearing
//
// The register is a SLICE and its order is the order every operator surface
// reports in, the order streams are provisioned in and the order an offer's
// terms are compared in. A map would reorder all of them on a whim of the
// runtime, which is why this is a table rather than a registry each domain
// writes itself into from an init.
//
// # Every node runs every domain
//
// There is no per-domain participation and nothing here reads `node.roles`:
// whatever roles a node declares, it applies every log in this table. The
// identity estate once narrowed — a seats-only satellite skipped it, since no
// turn reads it — and the narrowing cost more than it saved. The satellite
// still consumed inbound deliveries and ran seats whose contact routing turns
// on who holds each seat, so it needed a second way to read the directory (a
// signed scatter to the nodes that held it, with nonces, a head check and a
// thirty-second poll); an adopted snapshot had to be stripped of the domains
// a node declined; the trim had to leave a decliner out of that log's counted
// set; and every reader of the estate carried an arm for "this node has no
// copy". What it saved was one small log that grows with the company's
// headcount — on a host that already holds the fleet keyring, which opens
// every company secret. A domain whose rows a role does not read still costs
// that role's nodes its disk and its applier, and that is the price this
// table pays for every node answering every question from its own rows.
//
// # Why the constructors are functions on the engine rather than methods on
// the domain
//
// [statelog.Domain] is the DECLARATION — what a snapshot, a claim and a sweep
// read — and it must be answerable by a build that cannot construct the
// applier or the seams at all. An applier holds this node's id, its store
// handle and, for the knowledge base, the skill detector and its nudge; none
// of those belong to a value a manifest carries. So the constructors take the
// engine's own state log and live here, next to the declaration that names
// them, rather than inside it.

// registration is one domain's complete entry in the register.
//
// Every field but [registration.Barrier], [registration.NoBarrier],
// [registration.NewGate], [registration.Feed], [registration.OpsRetention] and
// [registration.OpsKindRetention] is required, and [checkRegister] is what
// says so: an entry that omits one is a boot failure naming the domain and the
// field, never a node that runs a domain it cannot apply, cannot write to,
// cannot size a stream for or cannot re-anchor. NewGate is required exactly
// when the domain claims identity, and refused when it does not; Feed exactly
// when the domain declares a wake feed ([statelog.Domain.FeedGroup]), and
// refused when it does not; OpsRetention exactly when the domain keeps an
// operation ledger ([statelog.Domain.OpsTable]), and refused when it does not.
type registration struct {
	// Domain is the declaration itself.
	Domain statelog.Domain

	// NewApplier builds the state machine that turns this domain's records
	// into rows. Required: a domain with no applier would have its records
	// consumed and produce nothing on this node.
	NewApplier func(s *stateLog) (statelog.Applier, error)

	// NewSeams builds the write authority's three seams and the domain's
	// eviction reader. Required: a domain with no write authority is one
	// nothing could ever append to.
	//
	// THE LOG IS AN ARGUMENT because a strict domain's fence reads its two
	// ends ([stateLog.logEndsOf]) beside the published floor
	// ([stateLog.floorOf]) and the runner's committed position — the three
	// terms [statelog.ZeroFence] clears an expectation of zero against.
	NewSeams func(s *stateLog, appendTo *jetstream.DomainLog,
		runner *statelog.Runner) (writeSeams, error)

	// Generation is the domain's own record of a reanchor — what opens a
	// new generation on its log, in its own record format — or its
	// declaration that it keeps none, which the vectors make
	// ([search.GenerationRecord]). Required, for the reason
	// [statelog.GenerationEncoder] gives: "this domain keeps no record of
	// it" is a claim the domain makes about itself, and a nil here would be
	// a log nobody could re-anchor, found by an operator mid-incident.
	Generation statelog.GenerationEncoder

	// NewGate builds the writer the NODE GATE publishes this log's eviction
	// and readmission through ([NodeGate]).
	//
	// REQUIRED OF EVERY DOMAIN THAT CLAIMS IDENTITY and refused of every
	// other. The trim counts nodes on an identity-claiming log, so one the
	// gate cannot write keeps an evicted node counted there — the log grows
	// behind a machine that is not coming back, which is what the tracker
	// alone being written did to the pages log. A compacted log counts
	// nobody and has no gate record to write.
	//
	// The publisher is handed in rather than read off the running domain,
	// so a case can put the log behind a recorder and see exactly what the
	// gate appended.
	NewGate func(s *stateLog, running *runningDomain,
		publisher *statelog.Publisher) (gateWrite, error)

	// Feed builds the WAKE FEED over this domain's log: the translator that
	// decides whether a committed record wakes anybody, and the opener over
	// the log's own fleet-wide group ([domainFeed]). Nil for a domain whose
	// records wake nobody — the vectors, the org chart and the identity
	// estate.
	//
	// HELD TO THE DOMAIN'S OWN DECLARATION ([statelog.Domain.FeedGroup]),
	// by [checkRegister] at boot and by [Engine.feedFor] where a feed
	// starts, because two parties name a log's wake consumer: the feed that
	// advances it and the trim that waits on it ([retention.feedTerm]),
	// from a node that may build no feed at all. [checkFeed] says what each
	// way of disagreeing costs.
	//
	// THE ENGINE IS AN ARGUMENT because a translator is not part of the
	// declaration: the knowledge base's reads Tier B — the reserved skills
	// container — off the epoch current at each change it translates.
	Feed func(e *Engine, records domainFeed) (changefeed.Translator, changefeed.Opener)

	// Barrier encodes the framework's barrier append as one of this
	// domain's own records. A domain that declares one gets a read index
	// and therefore `linearizable`; one that declares [NoBarrier] instead
	// gets neither, and its reads make no freshness claim beyond `session`.
	//
	// EXACTLY ONE of Barrier and NoBarrier is stated, which is what makes
	// "this domain grants no linearizable read" a decision in the diff
	// rather than a line nobody wrote.
	Barrier func(statelog.Envelope) ([]byte, error)

	// NoBarrier is the explicit form of a nil Barrier. See Barrier.
	NoBarrier bool

	// Ceiling is where Tier A puts this domain's stream ceiling, and what
	// a refusal to reserve it names. Required: a domain Tier A declares no
	// ceiling for would reserve its own default outside the budget every
	// other state log is sized into.
	Ceiling func(stream config.Stream, free int64) domainCeiling

	// OpsRetention is how long this domain's operation ledger keeps a row.
	//
	// ON THE ENTRY rather than one constant every domain shares, because
	// the question the ledger answers is "did my operation land", asked by
	// a RETRYING CLIENT — a seat told to carry an op id forward and re-ask
	// on its next wake, after a weekend. Every domain's writers re-ask on
	// the same rhythm today, so every entry takes the framework's default;
	// what the field buys is that a domain whose writers do not says so
	// where it is declared, rather than by moving a constant the others
	// read.
	//
	// REQUIRED EXACTLY WHEN THE DOMAIN KEEPS A LEDGER
	// ([statelog.Domain.OpsTable]) and refused when it does not — see
	// [checkLedgerEntry]. A compacted domain declares none.
	OpsRetention time.Duration

	// OpsKindRetention is a SHORTER horizon for the ledger rows of a subject
	// kind no slow client writes, keyed on the kind.
	//
	// The domain's horizon is sized for the slowest client that re-asks an
	// op id, and a kind whose every op id is re-asked inside the
	// publisher's own resolve budget if at all — nobody carries it to a
	// next wake — keeps that horizon's worth of rows for a question nobody
	// asks. A declared kind is its own node-local sweep, and the domain
	// ships the `(subject, applied_at)` index it seeks on
	// ([statelog.Runner.PurgeOpsOfKind]). Nil is one horizon for the
	// whole ledger, which is every domain but the identity estate's.
	OpsKindRetention map[string]time.Duration
}

// writeSeams is what one domain's write authority is built from: the three
// seams [statelog.Deps] takes, and the eviction reader readiness asks through.
//
// EVICTED MAY BE NIL, and that is the domain rather than an omission — the
// vectors are derived and compacted, so there is no tombstone table to read
// and nothing an evicted node could serve that a re-embed would not replace.
// It is not paired with a "no eviction reader" flag the way the barrier is,
// because a nil one refuses nothing silently: readiness asks the fence it was
// built from, and a domain with no fence has no answer to give either way.
type writeSeams struct {
	Rows    statelog.Rows
	Fence   statelog.Fence
	Gates   statelog.Gates
	Evicted func(context.Context) (bool, error)
}

// gateWrite publishes one identity-claiming log's gate record — an eviction,
// or with readmit its inverse — AS the principal who ran the gesture, under
// the operation id the gate derived for this log ([domainOpID]).
//
// THE PRINCIPAL WHOLE, not a name: each log's record carries the author, the
// kind of party and the credential ([iam.ActorFor]), and the org chart's and
// the identity estate's writers judge the gesture's authority —
// `fleet:operate` — on the party's own grants, at the record rather than only
// at the route, because a gate record can be published by a CLI, a test and a
// duty as well as through one.
type gateWrite func(ctx context.Context, by iam.Principal, opID, node string,
	readmit bool) (statelog.Result, error)

// register is every domain this build runs, in a FIXED order.
func register() []registration {
	return []registration{
		{
			Domain: tracker.Domain{},
			NewApplier: func(s *stateLog) (statelog.Applier, error) {
				// THE INBOX LISTENER, which is how a person learns they
				// have work: the apply is the one thing that sees every
				// notice on every node, and each node tells its own
				// sockets. See internal/tracker/inboxmove.go.
				return tracker.NewApplier(s.nodeID, s.inboxMoved), nil
			},
			NewSeams: func(s *stateLog, appendTo *jetstream.DomainLog,
				runner *statelog.Runner) (writeSeams, error) {

				rows, err := tracker.NewRows(s.db)
				if err != nil {
					return writeSeams{}, err
				}
				fence := tracker.NewFence(s.db, s.nodeID)
				name := tracker.Domain{}.Name()
				fence.Floor = s.floorOf(name)
				fence.Ends = s.logEndsOf(name, appendTo, runner)
				fence.Committed = runner.Committed
				return writeSeams{Rows: rows, Fence: fence,
					Gates: tracker.NewGates(s.db), Evicted: fence.Evicted}, nil
			},
			Barrier:    tracker.EncodeBarrier,
			Generation: tracker.GenerationRecord{},
			NewGate: func(s *stateLog, running *runningDomain,
				publisher *statelog.Publisher) (gateWrite, error) {

				w, err := tracker.NewWriter(tracker.WriterDeps{
					Publisher: publisher, DB: s.db, NodeID: s.nodeID,
					Drain: running.runner.Drain, Metrics: s.metrics,
					// THE NODE'S OWN IDENTITY, replaced per gesture with
					// the operator who ran it — see [tracker.Writer.As].
					Actor: s.nodeID, ActorKind: tracker.AuthorSystem,
				})
				if err != nil {
					return nil, err
				}
				return func(ctx context.Context, by iam.Principal, opID, node string,
					readmit bool) (statelog.Result, error) {

					a := iam.ActorFor(by)
					as := w.As(a.Name, tracker.AuthorKind(a.Kind),
						tracker.Provenance{OperatorID: a.OperatorID})
					gate := as.EvictNode
					if readmit {
						gate = as.ReadmitNode
					}
					res, err := gate(ctx, opID, node)
					return res.Result, err
				}, nil
			},
			// THE WAKE A COMMITTED RECORD SENDS: an assignment, a mention,
			// a question — derived from the log rather than published by
			// the writer's goroutine. See domainfeed.go.
			Feed: func(_ *Engine, records domainFeed) (changefeed.Translator, changefeed.Opener) {
				return tracker.NewTranslator(), tracker.FeedSource{Log: records}
			},
			OpsRetention: statelog.OpsRetention,
			Ceiling: func(stream config.Stream, free int64) domainCeiling {
				bytes, derived := stream.LogMaxBytes(free)
				return domainCeiling{Bytes: bytes,
					Field: "stream.tracker_log_max_bytes", Explicit: !derived,
					Floor: config.TrackerLogMaxBytesFloor}
			},
		},
		{
			Domain: search.Domain{},
			NewApplier: func(*stateLog) (statelog.Applier, error) {
				return search.NewApplier(), nil
			},
			NewSeams: func(s *stateLog, _ *jetstream.DomainLog,
				_ *statelog.Runner) (writeSeams, error) {

				rows, err := search.NewRows(s.db)
				if err != nil {
					return writeSeams{}, err
				}
				// NO EVICTION READER, and that is the domain rather than
				// an omission: the vectors are DERIVED and compacted, so
				// there is no tombstone table to read and nothing an
				// evicted node could serve that a re-embed would not
				// replace.
				return writeSeams{Rows: rows, Fence: search.NewFence(),
					Gates: search.NewGates()}, nil
			},
			// NO BARRIER, DECLARED: the vectors are derived from sources
			// another domain owns and compacted to one message per
			// source, so a read of them makes no claim about a position
			// and there is nothing a barrier could prove.
			NoBarrier: true,
			// AND NO GENERATION RECORD, declared by the domain's own
			// encoder rather than by a nil here: every node re-anchors its
			// own copy of a compacted log, so there is no fleet question a
			// record on it could answer.
			Generation: search.GenerationRecord{},
			// NO OPERATION LEDGER, like the usage log: the domain declares
			// no ops table ([search.Domain.OpsTable]).
			Ceiling: func(stream config.Stream, free int64) domainCeiling {
				bytes, _ := stream.VectorsMaxBytes(free)
				return domainCeiling{Bytes: bytes,
					Field:    "stream.tracker_vectors_max_bytes",
					Explicit: stream.TrackerVectorsMaxBytes > 0,
					Floor:    config.TrackerVectorsMaxBytesFloor}
			},
		},
		{
			Domain: pages.Domain{},
			NewApplier: func(s *stateLog) (statelog.Applier, error) {
				// THE PARSER AND THE NUDGE COME FROM HERE, because the
				// apply is what notices a tool-skill page arriving or
				// leaving and there is no other delivery to hang the
				// resync off. The nudge is safe to take before the
				// knowledge base's native half exists: it is a
				// non-blocking send that returns when there is nothing
				// to send to.
				return pages.NewApplier(s.nodeID, s.skills, s.nudgeSkills), nil
			},
			NewSeams: func(s *stateLog, appendTo *jetstream.DomainLog,
				runner *statelog.Runner) (writeSeams, error) {

				rows, err := pages.NewRows(s.db)
				if err != nil {
					return writeSeams{}, err
				}
				fence := pages.NewFence(s.db, s.nodeID)
				name := pages.Domain{}.Name()
				fence.Floor = s.floorOf(name)
				fence.Ends = s.logEndsOf(name, appendTo, runner)
				fence.Committed = runner.Committed
				return writeSeams{Rows: rows, Fence: fence,
					Gates: pages.NewGates(s.db), Evicted: fence.Evicted}, nil
			},
			Barrier:    pages.EncodeBarrier,
			Generation: pages.GenerationRecord{},
			NewGate: func(s *stateLog, _ *runningDomain,
				publisher *statelog.Publisher) (gateWrite, error) {

				kb, err := pages.NewStore(pages.Options{Publisher: publisher, DB: s.db})
				if err != nil {
					return nil, err
				}
				return func(ctx context.Context, by iam.Principal, opID, node string,
					readmit bool) (statelog.Result, error) {

					// THE OPERATOR AS THE PAGE STORE NAMES ONE — the same
					// author, kind and credential the tracker's record
					// carries, so both logs name the same person.
					actor, err := builtin.PageActorOf(by)
					if err != nil {
						return statelog.Result{}, err
					}
					if readmit {
						return kb.ReadmitNode(ctx, actor, opID, node)
					}
					return kb.EvictNode(ctx, actor, opID, node)
				}, nil
			},
			// AND THE KNOWLEDGE BASE'S, which reads the reserved skills
			// container LIVE off the epoch: `knowledge.skills_container`
			// is Tier B, and the feed outlives every revision.
			Feed: func(e *Engine, records domainFeed) (changefeed.Translator, changefeed.Opener) {
				return pages.NewTranslator(e.skillsContainer), pages.FeedSource{Log: records}
			},
			OpsRetention: statelog.OpsRetention,
			Ceiling: func(stream config.Stream, free int64) domainCeiling {
				bytes, derived := stream.PagesMaxBytes(free)
				return domainCeiling{Bytes: bytes,
					Field: "stream.pages_log_max_bytes", Explicit: !derived,
					Floor: config.PagesLogMaxBytesFloor}
			},
		},
		{
			Domain: chart.Domain{},
			NewApplier: func(s *stateLog) (statelog.Applier, error) {
				// THE VIEW'S TRIGGER. Every committed batch nudges the
				// rebuild — on this node, which is the only node whose
				// view these rows are. The object list is not read: a
				// rebuild derives the whole tree, because lead
				// inheritance and manages expansion make one seat's move
				// a fact about its descendants.
				//
				// It cannot be the change feed instead: a feed relays a
				// record to ONE node, and a derived view is held by
				// every node — so the rest would go on serving a chart
				// they had already applied and could not see they had.
				//
				// AND THE RECORDER, so a change the apply declines
				// rather than writes is counted as well as logged.
				return chart.NewApplier(s.nodeID, func([]chart.ObjectRef) {
					if s.nudgeChart != nil {
						s.nudgeChart()
					}
				}).WithMetrics(s.metrics), nil
			},
			NewSeams: func(s *stateLog, appendTo *jetstream.DomainLog,
				runner *statelog.Runner) (writeSeams, error) {

				rows, err := chart.NewRows(s.db)
				if err != nil {
					return writeSeams{}, err
				}
				fence := chart.NewFence(s.db, s.nodeID)
				name := chart.Domain{}.Name()
				fence.Floor = s.floorOf(name)
				fence.Ends = s.logEndsOf(name, appendTo, runner)
				fence.Committed = runner.Committed
				return writeSeams{Rows: rows, Fence: fence,
					Gates: chart.NewGates(s.db), Evicted: fence.Evicted}, nil
			},
			Barrier:    chart.EncodeBarrier,
			Generation: chart.GenerationRecord{},
			NewGate: func(s *stateLog, _ *runningDomain,
				publisher *statelog.Publisher) (gateWrite, error) {

				w, err := chart.NewWriter(chart.WriterDeps{
					Publisher: publisher, DB: s.db,
					// NOTHING A GATE WRITES IS SEALED, but the writer
					// refuses to exist without the shape it would seal by.
					Runtime: org.RuntimeShape{},
					Actor:   s.nodeID, ActorKind: chart.AuthorOperator,
				})
				if err != nil {
					return nil, err
				}
				return func(ctx context.Context, by iam.Principal, opID, node string,
					readmit bool) (statelog.Result, error) {

					// THE OPERATOR'S OWN GRANTS, which the chart asks
					// `fleet:operate` of ([chart.ClassNodeGate]) — never
					// the node's, which would admit any party the route
					// had let through.
					a := iam.ActorFor(by)
					as := w.As(a.Name, chart.AuthorKindOf(a.Kind), by.Grants,
						chart.Provenance{OperatorID: a.OperatorID})
					if readmit {
						return as.ReadmitNode(ctx, opID, node)
					}
					return as.EvictNode(ctx, opID, node)
				}, nil
			},
			OpsRetention: statelog.OpsRetention,
			// THE ONE CEILING THAT IGNORES THE FREE BYTES, and the
			// signature keeps the parameter so the table stays one
			// shape: a chart is sized from the corpus rather than from
			// the operator's disk — see [config.Stream.ChartMaxBytes].
			Ceiling: func(stream config.Stream, _ int64) domainCeiling {
				bytes, derived := stream.ChartMaxBytes()
				return domainCeiling{Bytes: bytes,
					Field: "stream.chart_log_max_bytes", Explicit: !derived,
					Floor: config.ChartLogMaxBytesFloor}
			},
		},
		{
			Domain: iamdomain.Domain{},
			NewApplier: func(s *stateLog) (statelog.Applier, error) {
				// THE DIRECTORY'S SIGNAL, the party registry's second
				// rebuild trigger beside the chart view's: a suspension
				// withdraws a seat's contact identities with no chart
				// record, so nothing on the publish path would ever see
				// it. See directory.go.
				return iamdomain.NewApplier(s.nodeID, s.nudgeDirectory), nil
			},
			NewSeams: func(s *stateLog, appendTo *jetstream.DomainLog,
				runner *statelog.Runner) (writeSeams, error) {

				rows, err := iamdomain.NewRows(s.db)
				if err != nil {
					return writeSeams{}, err
				}
				fence := iamdomain.NewFence(s.db, s.nodeID)
				name := iamdomain.Domain{}.Name()
				fence.Floor = s.floorOf(name)
				fence.Ends = s.logEndsOf(name, appendTo, runner)
				fence.Committed = runner.Committed
				return writeSeams{Rows: rows, Fence: fence,
					Gates: iamdomain.NewGates(s.db), Evicted: fence.Evicted}, nil
			},
			Barrier:    iamdomain.EncodeBarrier,
			Generation: iamdomain.GenerationRecord{},
			NewGate: func(s *stateLog, _ *runningDomain,
				publisher *statelog.Publisher) (gateWrite, error) {

				w, err := iamdomain.NewWriter(iamdomain.WriterDeps{
					Publisher: publisher, DB: s.db,
					Actor: s.nodeID, ActorKind: iam.KindMachine,
				})
				if err != nil {
					return nil, err
				}
				return func(ctx context.Context, by iam.Principal, opID, node string,
					readmit bool) (statelog.Result, error) {

					// AS THE PRINCIPAL, whose grants the domain asks
					// `fleet:operate` of at the record.
					as := w.As(by)
					if readmit {
						return as.ReadmitNode(ctx, opID, node)
					}
					return as.EvictNode(ctx, opID, node)
				}, nil
			},
			OpsRetention: statelog.OpsRetention,
			// SESSIONS GO IN AN HOUR: a row per sign-in and per
			// sign-out, whose op ids nobody re-asks after the
			// request that wrote them — see
			// [iamdomain.SessionOpsRetention].
			OpsKindRetention: map[string]time.Duration{
				string(iamdomain.KindSession): iamdomain.SessionOpsRetention,
			},
			// NOT DERIVED FROM THE DISK either, like the org chart's and
			// for a different reason: this log grows with the company's
			// HEADCOUNT and how often people sign in, and a volume has
			// nothing to say about either. The parameter stays so the
			// table is one shape — see [config.Stream.IamMaxBytes].
			Ceiling: func(stream config.Stream, _ int64) domainCeiling {
				bytes, derived := stream.IamMaxBytes()
				return domainCeiling{Bytes: bytes,
					Field: "stream.iam_log_max_bytes", Explicit: !derived,
					Floor: config.IamLogMaxBytesFloor}
			},
		},
		{
			// WHAT EACH NODE'S SEATS AND SCHEDULES DID EACH COMPANY DAY
			// (ADR-0020): the second compacted domain, written by every
			// node for its own days and summed by every reader.
			Domain: usage.Domain{},
			NewApplier: func(*stateLog) (statelog.Applier, error) {
				return usage.NewApplier(), nil
			},
			NewSeams: func(s *stateLog, _ *jetstream.DomainLog,
				_ *statelog.Runner) (writeSeams, error) {

				rows, err := usage.NewRows(s.db)
				if err != nil {
					return writeSeams{}, err
				}
				// NO EVICTION READER, for the vectors' reason and one of
				// its own: an evicted node's usage is still what its seats
				// did, and a fence here would erase that from every peer.
				return writeSeams{Rows: rows, Fence: usage.NewFence(),
					Gates: usage.NewGates()}, nil
			},
			// NO BARRIER, DECLARED: one writer per object and a reader
			// that sums across nodes, so a read makes no claim about a
			// position and there is nothing a barrier could prove.
			NoBarrier: true,
			// AND NO GENERATION RECORD, declared by the domain's own
			// encoder, for the vectors' reason: every node re-anchors its
			// own copy of a compacted log.
			Generation: usage.GenerationRecord{},
			// NO OPERATION LEDGER and so no horizon for one: the domain
			// declares no ops table ([usage.Domain.OpsTable]), and
			// [checkRegister] refuses a horizon for a ledger nobody keeps.
			//
			// NOT DERIVED FROM THE DISK, like the org chart's and the
			// identity estate's: the log is a census of node-day objects
			// over the domain's horizon, and a larger volume buys none
			// of them — see [config.Stream.UsageMaxBytes].
			Ceiling: func(stream config.Stream, _ int64) domainCeiling {
				bytes, derived := stream.UsageMaxBytes()
				return domainCeiling{Bytes: bytes,
					Field: "stream.usage_log_max_bytes", Explicit: !derived,
					Floor: config.UsageLogMaxBytesFloor}
			},
		},
	}
}

// checkRegister refuses a register whose entries are not complete, BEFORE
// anything is provisioned or started.
//
// Three of these were already boot failures, each raised at the moment its
// switch was consulted and naming only itself. Taken together and taken first,
// they say what is actually wrong: this build declares a domain it cannot run.
// The fourth, the barrier, was not a failure at all.
func checkRegister(entries []registration) error {
	seen := map[string]bool{}
	for i, entry := range entries {
		if entry.Domain == nil {
			return fmt.Errorf("engine: the state-log register's entry %d names no domain", i)
		}
		name := entry.Domain.Name()
		if seen[name] {
			return fmt.Errorf("engine: the state-log register declares domain %q twice, "+
				"so which applier, write authority and ceiling it runs would depend "+
				"on which entry a lookup reached first", name)
		}
		seen[name] = true
		if entry.NewApplier == nil {
			return fmt.Errorf("engine: the state-log register's entry for %q declares no "+
				"applier, so its records would be consumed and produce no rows on "+
				"this node", name)
		}
		if entry.NewSeams == nil {
			return fmt.Errorf("engine: the state-log register's entry for %q declares no "+
				"write authority, so nothing could ever append to its log", name)
		}
		if entry.Ceiling == nil {
			return fmt.Errorf("engine: the state-log register's entry for %q declares no "+
				"Tier A ceiling for its stream, so it would reserve its own default "+
				"outside the budget every other state log is sized into", name)
		}
		if floor := entry.Ceiling(config.Stream{}, 0).Floor; floor <= 0 {
			return fmt.Errorf("engine: the state-log register's entry for %q declares "+
				"no floor for its ceiling's field, so a boot the broker refuses could "+
				"not say how small a ceiling the field accepts", name)
		}
		if err := checkLedgerEntry(entry); err != nil {
			return err
		}
		if entry.Generation == nil {
			return fmt.Errorf("engine: the state-log register's entry for %q declares "+
				"no generation record, so a recreated log of it could never be "+
				"re-anchored — a domain that keeps none says so through its own "+
				"encoder, as the vectors do", name)
		}
		if err := checkIdentityEntry(entry); err != nil {
			return err
		}
		if err := checkFeedEntry(entry); err != nil {
			return err
		}
		if (entry.Barrier == nil) == !entry.NoBarrier {
			return fmt.Errorf("engine: the state-log register's entry for %q must state "+
				"either a barrier encoder or NoBarrier, and states %s — a domain "+
				"that grants no linearizable read says so, because an entry that "+
				"merely omits the encoder is indistinguishable from one that "+
				"forgot it", name, statedBarrier(entry))
		}
	}
	if len(entries) == 0 {
		return errors.New("engine: the state-log register is empty, so this node would " +
			"provision no log, apply no record and serve no domain's rows")
	}
	return nil
}

// checkLedgerEntry is what [checkRegister] asks of an entry's operation-ledger
// horizons: one exactly when the domain keeps a ledger
// ([statelog.Domain.OpsTable]), and none when it does not.
//
// BOTH DIRECTIONS, because each fails silently. A ledger with no horizon is a
// table that grows for the life of the deployment, and a horizon on a domain
// with no ledger is a sweep job named after a table that does not exist —
// which is what every node ran against `vectors_ops`, purging nothing every
// tick and listed beside the real sweeps as though it were one.
func checkLedgerEntry(entry registration) error {
	name := entry.Domain.Name()
	if entry.Domain.OpsTable() == "" {
		if entry.OpsRetention != 0 || len(entry.OpsKindRetention) > 0 {
			return fmt.Errorf("engine: the state-log register's entry for %q declares "+
				"an operation-ledger horizon, and the domain keeps no ledger — the "+
				"sweep would run against a table that does not exist", name)
		}
		return nil
	}
	if entry.OpsRetention <= 0 {
		return fmt.Errorf("engine: the state-log register's entry for %q declares an "+
			"operation-ledger horizon of %s — a domain's ledger row answers a "+
			"retrying client, and a horizon of zero or less would sweep a row "+
			"the client has not had a chance to re-ask with",
			name, entry.OpsRetention)
	}
	for kind, horizon := range entry.OpsKindRetention {
		if horizon <= 0 || horizon >= entry.OpsRetention {
			return fmt.Errorf("engine: the state-log register's entry for %q "+
				"declares a %s horizon for its %q ledger rows beside a domain "+
				"horizon of %s — a kind's own horizon exists to be SHORTER, and "+
				"one at or past the domain's would never be what sweeps a row, "+
				"while zero or less sweeps a row before its writer can re-ask",
				name, horizon, kind, entry.OpsRetention)
		}
	}
	return nil
}

// checkIdentityEntry is what [checkRegister] asks of an entry about the
// fleet's counted set — which only an identity-claiming log keeps.
//
// THREE ANSWERS A DOMAIN THAT CLAIMS IDENTITY MUST GIVE, each about a node the
// fleet has evicted:
//
//   - it lists the evictions on its own rows, or the trim can never stop
//     counting an evicted node there, and the log grows behind a machine that
//     is not coming back until its ceiling refuses writes — silently, since a
//     log with nobody evicted and one that cannot say look the same;
//   - it lets its LOG be asked directly ([statelog.EvictionProbe]), because a
//     node a peer re-anchored past never applies an eviction written after its
//     applier stopped, and the eviction of that peer is what releases it —
//     see [statelog.EvictedOnLog] and [stateLog.evictedOn];
//   - and the node gate can WRITE its eviction ([registration.NewGate]), or an
//     eviction lifts every other log's pin and leaves the node counted here.
//
// And a domain that claims none declares no gate writer: nobody is counted on
// its log, so a gate record there would be a record nothing reads.
func checkIdentityEntry(entry registration) error {
	domain := entry.Domain
	name := domain.Name()
	if !domain.ClaimsIdentity() {
		if entry.NewGate != nil {
			return fmt.Errorf("engine: the state-log register's entry for %q "+
				"declares a node-gate writer, and the domain claims no identity — "+
				"the trim counts nobody on its log, so an eviction written there "+
				"would be a record nothing reads", name)
		}
		return nil
	}
	if _, lists := domain.(evictionLister); !lists {
		return fmt.Errorf("engine: domain %q claims identity and cannot list the "+
			"evictions on its own log, so the trim could never stop counting an "+
			"evicted node there", name)
	}
	if _, probes := domain.(statelog.EvictionProbe); !probes {
		return fmt.Errorf("engine: domain %q claims identity and cannot say who "+
			"is evicted on its log without applying it, so a node a decommissioned "+
			"peer re-anchored past could never see that peer's eviction", name)
	}
	if entry.NewGate == nil {
		return fmt.Errorf("engine: domain %q claims identity and the state-log "+
			"register declares no node-gate writer for its log — an eviction "+
			"would lift every other log's pin and leave the node counted on "+
			"this one", name)
	}
	return nil
}

// checkFeedEntry is what [checkRegister] asks of an entry's wake feed: that it
// runs one exactly when its domain declares one, under the group the domain
// declares ([checkFeed]).
//
// AT BOOT, ON EVERY NODE, before anything is provisioned. [Engine.feedFor] asks
// the same where a feed starts, but only a node that PUBLISHES starts one, so
// a node in maintenance — or any node before its first company — would boot
// a build whose feed and trim disagree without a word.
//
// A ZERO ENGINE AND AN EMPTY LOG, because what is judged is the group the
// translator runs under, which is the translator's own constant: nothing is
// started, read or subscribed to here.
func checkFeedEntry(entry registration) error {
	var translator changefeed.Translator
	if entry.Feed != nil {
		translator, _ = entry.Feed(&Engine{}, domainFeed{})
	}
	return checkFeed(entry.Domain, translator)
}

// statedBarrier is what an entry said about its barrier, for the refusal.
func statedBarrier(entry registration) string {
	if entry.Barrier != nil {
		return "both"
	}
	return "neither"
}

// registrationFor is one domain's entry.
//
// It cannot fail on an unknown name where the caller walked the register to
// get there, which every caller does; the boolean is for the one that did not.
func registrationFor(name string) (registration, bool) {
	for _, entry := range register() {
		if entry.Domain.Name() == name {
			return entry, true
		}
	}
	return registration{}, false
}

// registeredDomains is every domain this build knows, in the register's order —
// which is every domain this node runs, since every node runs every one.
//
// Kept as a derivation rather than folded into every caller, because most of
// them want exactly this — the list — and reading it off the table is what
// makes the table the single place a domain is declared.
func registeredDomains() []statelog.Domain {
	entries := register()
	domains := make([]statelog.Domain, 0, len(entries))
	for _, entry := range entries {
		domains = append(domains, entry.Domain)
	}
	return domains
}

// Domains is every state-log domain this build registers, in the register's
// order: what every node runs, and so what a snapshot must name and what two
// members' replicated estates are compared over.
//
// EXPORTED FOR THE GATES THAT CHECK A WHOLE NODE, which must not keep a list of
// their own: a literal list of three outlived the org chart and the identity
// directory joining the register, and certified neither.
func Domains() []statelog.Domain { return registeredDomains() }

// signerFor and verifierFor are one domain's halves of the record signature.
//
// PER DOMAIN, because the derivation binds the domain's own name in: a record
// replayed from one log onto another must not verify, and a signer that did
// not know which log it was writing to could not promise that.
func (s *stateLog) signerFor(domain statelog.Domain) (*statelog.Signer, error) {
	return statelog.NewSigner(domain.Name(), s.ring)
}

func (s *stateLog) verifierFor(domain statelog.Domain) (*statelog.Verifier, error) {
	return statelog.NewVerifier(domain.Name(), s.ring)
}

// recordKeyring is the Tier A keyring as the state log's signatures read it.
//
// THE SAME MATERIAL THE PER-RUN TOKENS DERIVE FROM, and deliberately: a
// deployment has one keyring, and a second one for records would be a second
// thing to rotate, a second thing to get wrong and a second thing an operator
// has to know exists. The two derive different keys from it, each binding its
// own label in, so a record MAC and a token MAC over one key are never the
// same bytes.
//
// THE REFERENCES ARE NOT RESOLVED, for [config.Secrets.TokenMaterial]'s
// reason: this is read before the secret store whose own key would resolve
// them is open, and what matters is that two nodes agree rather than that the
// material is plaintext.
func recordKeyring(boot *config.Bootstrap) statelog.Keyring {
	if boot == nil {
		return statelog.Keyring{}
	}
	ring := statelog.Keyring{
		ActiveID: boot.Secrets.ActiveKeyID,
		Keys:     make([]statelog.Key, 0, len(boot.Secrets.Keys)),
	}
	for _, key := range boot.Secrets.Keys {
		ring.Keys = append(ring.Keys, statelog.Key{ID: key.ID, Material: key.Material})
	}
	return ring
}
