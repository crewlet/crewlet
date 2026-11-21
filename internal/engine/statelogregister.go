package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/inbox"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/recordseal"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/usage"
	"github.com/crewlet/crewlet/internal/worklet"
)

// THE REGISTER: every domain this build runs, and all the engine needs to run
// one, in one table. A per-domain switch that does not know a domain answers
// "none" silently (no barrier and so no `linearizable`, no generation record,
// no gate, no feed, no trimmed record), and absent reads like deliberately
// absent; so each is a field [checkRegister] holds to the domain's own
// declaration, and a domain is declared in one place.
//
// Every data node runs every domain (ADR-0025): it holds the whole estate or
// none of it, so no reader carries an arm for one missing domain. The
// constructors live here and take the engine's state log because
// [statelog.Domain] is the declaration, which a build that cannot construct
// an applier must still answer.

// registration is one domain's complete entry in the register. [checkRegister]
// refuses an entry missing a required field, naming the domain and the field,
// so no node runs a domain it cannot apply, append to, size or re-anchor. The
// optional fields are each required exactly when the domain's declaration
// calls for them and refused otherwise: Trimmed when its log writes anchors,
// NewGate when it claims identity, Feed when it declares a wake feed,
// OpsRetention when it keeps an operation ledger; OpsKindRetention is optional
// beside a ledger and refused without one; and exactly one of Barrier and
// NoBarrier is stated.
type registration struct {
	// Domain is the declaration itself.
	Domain statelog.Domain

	// NewApplier builds the state machine that turns this domain's records
	// into rows. Required: without one its records are consumed and produce
	// nothing on this node.
	NewApplier func(s *stateLog) (statelog.Applier, error)

	// NewSeams builds the write authority's three seams and the eviction
	// reader. It takes the log because a strict domain's fence reads its ends
	// ([stateLog.logEndsOf]) beside the floor ([stateLog.floorOf]) and the
	// runner's committed position, the terms [statelog.ZeroFence] clears
	// against; and the spec because the rows' ledger is keyed to the stream
	// it names ([statelog.Rows]).
	NewSeams func(s *stateLog, spec statelog.StreamSpec, appendTo *jetstream.DomainLog,
		runner *statelog.Runner) (writeSeams, error)

	// Generation is the record a reanchor opens a new generation with, or the
	// domain's own declaration that it keeps none ([search.GenerationRecord],
	// [usage.GenerationRecord]). Never nil ([statelog.GenerationEncoder]): a
	// log nobody could re-anchor is found by an operator mid-incident.
	Generation statelog.GenerationEncoder

	// NewGate builds the writer the node gate ([NodeGate]) publishes this
	// log's eviction and readmission through. Required exactly when the domain
	// claims identity: the trim counts nodes on such a log, so one the gate
	// cannot write keeps an evicted node counted and the log grows behind it.
	// The publisher is an argument so a case can record what the gate wrote.
	NewGate func(s *stateLog, running *runningLog,
		publisher *statelog.Publisher) (gateWrite, error)

	// Feed builds the wake feed over this log: the translator deciding whether
	// a committed record wakes anybody, and the opener over the log's
	// fleet-wide group ([domainFeed]); nil for a domain whose records wake
	// nobody. Held to [statelog.Domain.FeedGroup] by [checkRegister] and
	// [Engine.feedFor], because the feed and the trim ([retention.feedTerm])
	// both name the consumer ([checkFeed] has the costs). The engine is an
	// argument because a translator may read Tier B live, as the knowledge
	// base's reads the skills container.
	Feed func(e *Engine, records domainFeed) (changefeed.Translator, changefeed.Opener)

	// Inbox marks a domain whose changes are a person's news: its applier
	// tells [stateLog.inboxFor]'s listener whose inbox each committed batch
	// moved. The mark is the wiring: inboxFor hands the listener, and the
	// relay follows ([inboxSources]), exactly the marked domains, so the two
	// cannot disagree.
	Inbox bool

	// Barrier encodes the barrier append as one of this domain's records: a
	// domain with one gets a read index and so `linearizable`; one stating
	// [NoBarrier] reads at `session` at best. Exactly one of the two is
	// stated, so "no linearizable read" is a decision in the diff.
	Barrier func(statelog.Envelope) ([]byte, error)

	// NoBarrier is the explicit form of a nil Barrier. See Barrier.
	NoBarrier bool

	// Trimmed encodes the trim's TRIMMED record (ADR-0080), which removes the
	// log's anchors below its first surviving record in the transaction that
	// applies it ([statelog.SweepAnchors]). Required exactly when the log
	// writes anchors ([statelog.WritesAnchors]): without it every subject
	// ever written keeps an anchor on every data node for good, and on a log
	// without anchors it would remove nothing each tick.
	Trimmed func(statelog.Envelope) ([]byte, error)

	// Reads is the domain's read model ([statelog.ReadModel]): linearizable
	// reads per agent seat per day, the census `census_drift` holds the log
	// to and a term in its Tier A ceiling. Required exactly beside a Barrier
	// ([checkReadModel]). A pointer because zero is a declaration (the
	// identity estate's) and nil is a domain that forgot.
	Reads *statelog.ReadModel

	// Ceiling is where Tier A puts this domain's stream ceiling, and what a
	// refusal to reserve it names. Required: without it the stream reserves
	// its own default outside the budget every state log is sized into.
	Ceiling func(stream config.Stream, free int64) domainCeiling

	// OpsRetention is how long this domain's operation ledger keeps a row,
	// for a retrying client that carries an op id to its next wake, possibly
	// days later. Per entry rather than one shared constant, so a domain
	// whose writers re-ask on another rhythm says so where it is declared.
	// Required exactly when the domain keeps a ledger
	// ([statelog.Domain.OpsTable], [checkLedgerEntry]).
	OpsRetention time.Duration

	// OpsKindRetention is a shorter horizon, keyed on subject kind, for
	// ledger rows whose op ids are re-asked only inside the publisher's
	// resolve budget, never at a next wake. Each kind is its own node-local
	// sweep on the `(subject, applied_at)` index the domain ships
	// ([statelog.Runner.PurgeOpsOfKind]). Nil is one horizon for the ledger.
	OpsKindRetention map[string]time.Duration
}

// writeSeams is what one domain's write authority is built from: the three
// seams [statelog.Deps] takes, and the eviction reader readiness asks.
// Evicted may be nil for a domain with no tombstones (the vectors). Unlike the
// barrier it needs no explicit flag: readiness asks the fence it was built
// from, and a domain with no fence has no answer either way.
type writeSeams struct {
	Rows    statelog.Rows
	Fence   statelog.Fence
	Gates   statelog.Gates
	Evicted func(context.Context) (bool, error)
}

// gateWrite publishes one identity-claiming log's gate record (an eviction, or
// with readmit its inverse) as the principal who ran the gesture, under the op
// id the gate derived for this log ([domainOpID]). The whole principal, not a
// name: each record carries author, party kind and credential ([iam.ActorFor]),
// and the identity estate judges admin authority at the record, not only the
// route, because a harness, a test or a duty can publish one too.
type gateWrite func(ctx context.Context, by iam.Principal, opID, node string,
	readmit bool) (statelog.Result, error)

// register is every domain this build runs, in a FIXED order: the order every
// operator surface reports in, streams are provisioned in and an offer's terms
// are compared in, so a slice rather than a map or an init-time registry.
// Later domains are appended in the order they joined, from entries declared
// beside their wiring (the *register.go files; Gitlet's below), so no
// existing position moves.
func register() []registration {
	return slices.Concat([]registration{
		{
			Domain: worklet.Domain{},
			// A PERSON'S WORK: the apply sees every notice on every data
			// node and each tells its own sockets; a node without `data`
			// serves none, since `ingress` requires it. See internal/inbox's
			// movement.go.
			Inbox: true,
			NewApplier: func(s *stateLog) (statelog.Applier, error) {
				return worklet.NewApplier(s.inboxFor(worklet.Domain{})), nil
			},
			NewSeams: func(s *stateLog, spec statelog.StreamSpec, appendTo *jetstream.DomainLog,
				runner *statelog.Runner) (writeSeams, error) {

				rows, err := worklet.NewRows(s.estate().Reader(), spec)
				if err != nil {
					return writeSeams{}, err
				}
				fence := worklet.NewFence(s.estate().Reader(), s.nodeID)
				name := worklet.Domain{}.Name()
				fence.Floor = s.floorOf(name)
				fence.Ends = s.logEndsOf(name, appendTo, runner)
				fence.Committed = runner.Committed
				return writeSeams{Rows: rows, Fence: fence,
					Gates: worklet.NewGates(s.estate().Reader()), Evicted: fence.Evicted}, nil
			},
			Barrier: worklet.EncodeBarrier,
			Trimmed: worklet.EncodeTrimmed,
			// THE REFERENCE COMPANY'S WHOLE RATE: tracker tools read at
			// `linearizable` (statelog.ReadLevelDefaults), and the census does
			// not split a company's reads between this log and the knowledge
			// base's, so each declares all of them.
			Reads:      &statelog.ReadModel{PerSeatDay: statelog.LinearizableReadsPerSeatDay},
			Generation: worklet.GenerationRecord{},
			NewGate: func(s *stateLog, running *runningLog,
				publisher *statelog.Publisher) (gateWrite, error) {

				w, err := worklet.NewWriter(worklet.WriterDeps{
					Publisher: publisher, DB: s.estate().Reader(), NodeID: s.nodeID,
					// The node's own identity, replaced per gesture by the
					// operator who ran it ([worklet.Writer.As]).
					Actor: s.nodeID, ActorKind: worklet.AuthorSystem,
				})
				if err != nil {
					return nil, err
				}
				return func(ctx context.Context, by iam.Principal, opID, node string,
					readmit bool) (statelog.Result, error) {

					a := iam.ActorFor(by)
					as := w.As(a.Name, worklet.AuthorKind(a.Kind),
						worklet.Provenance{OperatorID: a.OperatorID})
					gate := as.EvictNode
					if readmit {
						gate = as.ReadmitNode
					}
					res, err := gate(ctx, opID, node)
					return res.Result, err
				}, nil
			},
			// Wakes (an assignment, a mention, a question) derive from the
			// committed log, never the writer's goroutine; see domainfeed.go.
			Feed: func(_ *Engine, records domainFeed) (changefeed.Translator, changefeed.Opener) {
				return worklet.NewTranslator(), worklet.FeedSource{Log: records}
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
			NewSeams: func(s *stateLog, spec statelog.StreamSpec, _ *jetstream.DomainLog,
				_ *statelog.Runner) (writeSeams, error) {

				rows, err := search.NewRows(s.estate().Reader(), spec)
				if err != nil {
					return writeSeams{}, err
				}
				// No eviction reader: the vectors are derived and compacted,
				// so there is no tombstone table, and a re-embed replaces
				// anything an evicted node served.
				return writeSeams{Rows: rows, Fence: search.NewFence(),
					Gates: search.NewGates()}, nil
			},
			// The vectors are derived from other domains' sources and
			// compacted per source: a read claims no position for a barrier
			// to prove.
			NoBarrier: true,
			// The domain's encoder declares no record: every node re-anchors
			// its own copy of a compacted log.
			Generation: search.GenerationRecord{},
			// No ops table ([search.Domain.OpsTable]), so no OpsRetention.
			Ceiling: func(stream config.Stream, free int64) domainCeiling {
				bytes, derived := stream.VectorsMaxBytes(free)
				return domainCeiling{Bytes: bytes,
					Field: "stream.tracker_vectors_max_bytes", Explicit: !derived,
					Floor: config.TrackerVectorsMaxBytesFloor}
			},
		},
		{
			Domain: doclet.Domain{},
			NewApplier: func(s *stateLog) (statelog.Applier, error) {
				// The parser and nudge are wired here because the apply is
				// the only thing that sees a tool-skill page arrive or leave.
				// The nudge is a non-blocking send, so it is safe before the
				// knowledge base's native half exists.
				return doclet.NewApplier(s.skills, s.nudgeSkills), nil
			},
			NewSeams: func(s *stateLog, spec statelog.StreamSpec, appendTo *jetstream.DomainLog,
				runner *statelog.Runner) (writeSeams, error) {

				rows, err := doclet.NewRows(s.estate().Reader(), spec)
				if err != nil {
					return writeSeams{}, err
				}
				fence := doclet.NewFence(s.estate().Reader(), s.nodeID)
				name := doclet.Domain{}.Name()
				fence.Floor = s.floorOf(name)
				fence.Ends = s.logEndsOf(name, appendTo, runner)
				fence.Committed = runner.Committed
				return writeSeams{Rows: rows, Fence: fence,
					Gates: doclet.NewGates(s.estate().Reader()), Evicted: fence.Evicted}, nil
			},
			Barrier: doclet.EncodeBarrier,
			Trimmed: doclet.EncodeTrimmed,
			// The whole rate, for the tracker's reason: knowledge-base reads
			// are `linearizable` too.
			Reads:      &statelog.ReadModel{PerSeatDay: statelog.LinearizableReadsPerSeatDay},
			Generation: doclet.GenerationRecord{},
			NewGate: func(s *stateLog, _ *runningLog,
				publisher *statelog.Publisher) (gateWrite, error) {

				kb, err := doclet.NewStore(doclet.Options{Publisher: publisher,
					DB: s.estate().Reader()})
				if err != nil {
					return nil, err
				}
				return func(ctx context.Context, by iam.Principal, opID, node string,
					readmit bool) (statelog.Result, error) {

					// The same author, kind and credential the tracker's
					// record carries, so both logs name the same person.
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
			// Reads the skills container live off the epoch:
			// `knowledge.skills_container` is Tier B, and the feed outlives
			// every revision.
			Feed: func(e *Engine, records domainFeed) (changefeed.Translator, changefeed.Opener) {
				return doclet.NewTranslator(e.skillsContainer), doclet.FeedSource{Log: records}
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
			Domain: iamdomain.Domain{},
			NewApplier: func(s *stateLog) (statelog.Applier, error) {
				// What each committed batch moved rebuilds the party
				// registry, since a suspension withdraws contact identities
				// with no config change (directory.go), and tells open
				// connections their credential may have ended
				// (identitymoved.go).
				return iamdomain.NewApplier(s.nodeID, s.identityMoved), nil
			},
			NewSeams: func(s *stateLog, spec statelog.StreamSpec, appendTo *jetstream.DomainLog,
				runner *statelog.Runner) (writeSeams, error) {

				rows, err := iamdomain.NewRows(s.estate().Reader(), spec)
				if err != nil {
					return writeSeams{}, err
				}
				fence := iamdomain.NewFence(s.estate().Reader(), s.nodeID)
				name := iamdomain.Domain{}.Name()
				fence.Floor = s.floorOf(name)
				fence.Ends = s.logEndsOf(name, appendTo, runner)
				fence.Committed = runner.Committed
				return writeSeams{Rows: rows, Fence: fence,
					Gates: iamdomain.NewGates(s.estate().Reader()), Evicted: fence.Evicted}, nil
			},
			Barrier: iamdomain.EncodeBarrier,
			Trimmed: iamdomain.EncodeTrimmed,
			// ZERO, COUNTED: only a `linearizable` read appends a barrier,
			// and nothing reads this domain that way. [iamdomain.Reader]
			// answers from this node's rows with their lag, and an unapplied
			// session is waited for at its bearer's stated position.
			// [config.DefaultIamLogMaxBytes] assumes the same; a reader that
			// starts taking one meets `census_drift`.
			Reads:      &statelog.ReadModel{PerSeatDay: 0},
			Generation: iamdomain.GenerationRecord{},
			NewGate: func(s *stateLog, _ *runningLog,
				publisher *statelog.Publisher) (gateWrite, error) {

				// The node's own kind, which no record carries: each gate
				// record is published as `by` ([iamdomain.Writer.As]).
				w, err := iamdomain.NewWriter(iamdomain.WriterDeps{
					Publisher: publisher, DB: s.estate().Reader(),
					Actor: s.nodeID, ActorKind: iamdomain.NodeKind,
				})
				if err != nil {
					return nil, err
				}
				return func(ctx context.Context, by iam.Principal, opID, node string,
					readmit bool) (statelog.Result, error) {

					// The domain checks the principal's grants for admin at
					// the record.
					as := w.As(by)
					if readmit {
						return as.ReadmitNode(ctx, opID, node)
					}
					return as.EvictNode(ctx, opID, node)
				}, nil
			},
			OpsRetention: statelog.OpsRetention,
			// Session rows (one per sign-in and sign-out) go in an hour:
			// nobody re-asks their op ids ([iamdomain.SessionOpsRetention]).
			OpsKindRetention: map[string]time.Duration{
				string(iamdomain.KindSession): iamdomain.SessionOpsRetention,
			},
			// NOT DERIVED FROM THE DISK: this log grows with headcount and
			// sign-ins, which a volume says nothing about
			// ([config.Stream.IamMaxBytes]). The parameter keeps one shape.
			Ceiling: func(stream config.Stream, _ int64) domainCeiling {
				bytes, derived := stream.IamMaxBytes()
				return domainCeiling{Bytes: bytes,
					Field: "stream.iam_log_max_bytes", Explicit: !derived,
					Floor: config.IamLogMaxBytesFloor}
			},
		},
		{
			// What each node's seats and schedules did each company day
			// (ADR-0020): compacted, written by every node for its own days
			// and summed by every reader.
			Domain: usage.Domain{},
			NewApplier: func(*stateLog) (statelog.Applier, error) {
				return usage.NewApplier(), nil
			},
			NewSeams: func(s *stateLog, spec statelog.StreamSpec, _ *jetstream.DomainLog,
				_ *statelog.Runner) (writeSeams, error) {

				rows, err := usage.NewRows(s.estate().Reader(), spec)
				if err != nil {
					return writeSeams{}, err
				}
				// No eviction reader, for the vectors' reason and because an
				// evicted node's usage is still what its seats did: a fence
				// would erase it from every peer.
				return writeSeams{Rows: rows, Fence: usage.NewFence(),
					Gates: usage.NewGates()}, nil
			},
			// One writer per object and a reader that sums across nodes: a
			// read claims no position for a barrier to prove.
			NoBarrier: true,
			// No record, for the vectors' reason.
			Generation: usage.GenerationRecord{},
			// No ops table ([usage.Domain.OpsTable]), so no OpsRetention.
			//
			// NOT DERIVED FROM THE DISK: the log is a census of node-day
			// objects over the domain's horizon ([config.Stream.UsageMaxBytes]).
			Ceiling: func(stream config.Stream, _ int64) domainCeiling {
				bytes, derived := stream.UsageMaxBytes()
				return domainCeiling{Bytes: bytes,
					Field: "stream.usage_log_max_bytes", Explicit: !derived,
					Floor: config.UsageLogMaxBytesFloor}
			},
		},
	}, artifactsRegistrations(), artifactDataRegistrations(), assistantRegistrations(),
		chatRegistrations(), executorRegistrations(), codeRegistrations(), accessRegistrations(),
		atlasRegistrations(), advisorRegistrations())
}

// codeRegistrations is Gitlet's entry in the register: the company's git
// repositories (ADR-0084), registered after the executor log's.
func codeRegistrations() []registration {
	return []registration{
		{
			// Strict and identity-claiming: settings, protections, refs,
			// packs and pushes are rows only its applier writes; a pack's
			// bytes are one object in the object store.
			Domain: gitlet.Domain{},
			// A CHANGE'S NEWS IS A PERSON'S TOO (a review asked, a change
			// merged or ready): the applier writes a notice per recipient
			// (code_notifications) and reports whose inbox each batch moved.
			Inbox: true,
			NewApplier: func(s *stateLog) (statelog.Applier, error) {
				return gitlet.NewApplier(s.inboxFor(gitlet.Domain{})), nil
			},
			NewSeams: func(s *stateLog, spec statelog.StreamSpec, appendTo *jetstream.DomainLog,
				runner *statelog.Runner) (writeSeams, error) {

				rows, err := gitlet.NewRows(s.estate().Reader(), spec)
				if err != nil {
					return writeSeams{}, err
				}
				fence := gitlet.NewFence(s.estate().Reader(), s.nodeID)
				name := gitlet.Domain{}.Name()
				fence.Floor = s.floorOf(name)
				fence.Ends = s.logEndsOf(name, appendTo, runner)
				fence.Committed = runner.Committed
				return writeSeams{Rows: rows, Fence: fence,
					Gates: gitlet.NewGates(s.estate().Reader()), Evicted: fence.Evicted}, nil
			},
			Barrier: gitlet.EncodeBarrier,
			Trimmed: gitlet.EncodeTrimmed,
			// The whole rate, for the tracker's reason. Git clients' reads
			// are not a seat's; the log's sizing adds them ([clientBarriers]).
			Reads:      &statelog.ReadModel{PerSeatDay: statelog.LinearizableReadsPerSeatDay},
			Generation: gitlet.GenerationRecord{},
			NewGate: func(s *stateLog, _ *runningLog,
				publisher *statelog.Publisher) (gateWrite, error) {

				host, err := gitlet.NewStore(gitlet.StoreOptions{Publisher: publisher,
					DB: s.estate().Reader(), NodeID: s.nodeID})
				if err != nil {
					return nil, err
				}
				return func(ctx context.Context, by iam.Principal, opID, node string,
					readmit bool) (statelog.Result, error) {

					// The operator as every gate record names them
					// ([iam.ActorFor]). An operator's code record always
					// names a credential, so a principal without one is
					// named by its author.
					a := iam.ActorFor(by)
					operator := a.OperatorID
					if operator == "" {
						operator = a.Name
					}
					actor := gitlet.Actor{ID: a.Name, Kind: gitlet.ActorOperator, OperatorID: operator}
					if readmit {
						return host.ReadmitNode(ctx, actor, opID, node)
					}
					return host.EvictNode(ctx, actor, opID, node)
				}, nil
			},
			// A seat's wakes (review asked, change reviewed, finding
			// answered) derive from the committed record and the routing its
			// writer stamped, never a writer's goroutine (ADR-0012). A person
			// hears through the applier's notice rows, never from here.
			Feed: func(_ *Engine, records domainFeed) (changefeed.Translator, changefeed.Opener) {
				return gitlet.NewTranslator(), gitlet.FeedSource{Log: records}
			},
			OpsRetention: statelog.OpsRetention,
			Ceiling: func(stream config.Stream, free int64) domainCeiling {
				bytes, derived := stream.CodeMaxBytes(free)
				return domainCeiling{Bytes: bytes,
					Field: "stream.code_log_max_bytes", Explicit: !derived,
					Floor: config.CodeLogMaxBytesFloor}
			},
		},
	}
}

// checkRegister refuses a register whose entries are incomplete, BEFORE
// anything is provisioned or started, so the boot names the real fault: this
// build declares a domain it cannot run.
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
		if anchors := statelog.WritesAnchors(entry.Domain.Stream()); anchors != (entry.Trimmed != nil) {
			return fmt.Errorf("engine: the state-log register's entry for %q declares "+
				"a trimmed-record encoder=%v on a log that writes anchors=%v — a log "+
				"with anchors and no record keeps one for every subject it ever "+
				"wrote, and a record on a log with none removes nothing (ADR-0080)",
				name, entry.Trimmed != nil, anchors)
		}
		if err := checkReadModel(entry); err != nil {
			return err
		}
	}
	if len(entries) == 0 {
		return errors.New("engine: the state-log register is empty, so this node would " +
			"provision no log, apply no record and serve no domain's rows")
	}
	return nil
}

// checkLedgerEntry is [checkRegister]'s check of an entry's operation-ledger
// horizons: present exactly when the domain keeps a ledger
// ([statelog.Domain.OpsTable]). Both directions fail silently otherwise: a
// ledger without a horizon grows for good, and a horizon without a ledger is
// a sweep that purges nothing every tick.
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

// checkIdentityEntry is [checkRegister]'s check of the fleet's counted set,
// which only an identity-claiming log keeps. Such a domain lists the evictions
// on its own rows, or the trim never stops counting an evicted node and the
// log silently grows to its ceiling; answers [statelog.EvictionProbe], since a
// node a peer re-anchored past never applies that peer's eviction
// ([statelog.EvictedOnLog], [stateLog.evictedOn]); has a gate writer
// ([registration.NewGate]), or an eviction leaves the node counted here; and
// keeps a generation record, the first-writer-wins between two operators
// re-anchoring the log at once ([statelog.GenerationEncoder]). A domain
// claiming none declares neither gate nor record, which nothing would read.
// Checked at boot rather than at a reanchor, which is mid-incident.
func checkIdentityEntry(entry registration) error {
	domain := entry.Domain
	name := domain.Name()
	_, keeps := entry.Generation.GenerationSubject(1)
	if !domain.ClaimsIdentity() {
		if entry.NewGate != nil {
			return fmt.Errorf("engine: the state-log register's entry for %q "+
				"declares a node-gate writer, and the domain claims no identity — "+
				"the trim counts nobody on its log, so an eviction written there "+
				"would be a record nothing reads", name)
		}
		if keeps {
			return fmt.Errorf("engine: the state-log register's entry for %q keeps "+
				"a generation record, and the domain claims no identity — a "+
				"reanchor of it would write an audit row nobody reads; a domain "+
				"that keeps none says so through its own encoder, as the vectors do",
				name)
		}
		return nil
	}
	if !keeps {
		return fmt.Errorf("engine: domain %q claims identity and its generation "+
			"encoder keeps no record, so two operators re-anchoring its log at "+
			"once could each open the same generation with nothing to say so — "+
			"the record is the first-writer-wins between them", name)
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

// checkFeedEntry is [checkRegister]'s check that an entry runs a wake feed
// exactly when its domain declares one, under the declared group
// ([checkFeed]). At boot on every data node, because [Engine.feedFor] checks
// only where a feed starts, which a node in maintenance or before its first
// company never reaches. A zero engine and empty log suffice: the group is the
// translator's own constant, and nothing is started or read.
func checkFeedEntry(entry registration) error {
	var translator changefeed.Translator
	if entry.Feed != nil {
		translator, _ = entry.Feed(&Engine{}, domainFeed{})
	}
	return checkFeed(entry.Domain, translator)
}

// checkReadModel is [checkRegister]'s check that an entry declares a read
// model exactly when it declares a barrier. Both directions are a census
// nobody stated: a barrier without one leaves `census_drift` holding the log
// to an unchosen figure, and one beside NoBarrier counts reads none can make.
func checkReadModel(entry registration) error {
	name := entry.Domain.Name()
	switch {
	case entry.Barrier != nil && entry.Reads == nil:
		return fmt.Errorf("engine: the state-log register's entry for %q declares a "+
			"barrier and no read model, so its log has no census — state how many "+
			"linearizable reads a day each agent seat puts on it, zero included, "+
			"since that figure is what `census_drift` holds the log to and a term "+
			"in its ceiling", name)
	case entry.Barrier == nil && entry.Reads != nil:
		return fmt.Errorf("engine: the state-log register's entry for %q declares a "+
			"read model and no barrier — no read can append to its log, so a "+
			"census for it is a figure nothing is held to", name)
	case entry.Reads != nil && entry.Reads.PerSeatDay < 0:
		return fmt.Errorf("engine: the state-log register's entry for %q declares %d "+
			"linearizable reads a day per agent seat, and a read rate is never "+
			"below zero", name, entry.Reads.PerSeatDay)
	}
	return nil
}

// readModel is the read model the register declares for domain, and false for
// a domain with no barrier — whose log no read appends to.
func readModel(domain statelog.Domain) (statelog.ReadModel, bool) {
	entry, _ := registrationFor(domain.Name())
	if entry.Reads == nil {
		return statelog.ReadModel{}, false
	}
	return *entry.Reads, true
}

// statedBarrier is what an entry said about its barrier, for the refusal.
func statedBarrier(entry registration) string {
	if entry.Barrier != nil {
		return "both"
	}
	return "neither"
}

// registrationFor is one domain's entry, and false for a name the register
// does not hold.
func registrationFor(name string) (registration, bool) {
	for _, entry := range register() {
		if entry.Domain.Name() == name {
			return entry, true
		}
	}
	return registration{}, false
}

// registeredDomains is every domain this build knows, in the register's order:
// every domain a data node runs. Derived from the table, so the table stays the
// one place a domain is declared.
func registeredDomains() []statelog.Domain {
	entries := register()
	domains := make([]statelog.Domain, 0, len(entries))
	for _, entry := range entries {
		domains = append(domains, entry.Domain)
	}
	return domains
}

// inboxSources is every domain the register marks [registration.Inbox], by
// name and in the register's order: the sources whose movements a data node's
// relay streams and follows, one stream and one follower each.
func inboxSources() []string {
	var out []string
	for _, entry := range register() {
		if entry.Inbox {
			out = append(out, entry.Domain.Name())
		}
	}
	return out
}

// inboxFor is the listener a domain's applier announces its inbox movements to:
// this engine's ([Engine.inboxMoved]) for a domain marked [registration.Inbox],
// nil for any other, so the mark alone wires an applier to the sockets.
func (s *stateLog) inboxFor(domain statelog.Domain) func([]inbox.Movement) {
	if entry, ok := registrationFor(domain.Name()); !ok || !entry.Inbox {
		return nil
	}
	return s.inboxMoved
}

// registeredLogs is every registered domain's stream, in the register's order:
// what a backup's manifest names, a log with no checkpoint yet included.
func registeredLogs() []string {
	domains := registeredDomains()
	out := make([]string, 0, len(domains))
	for _, d := range domains {
		out = append(out, d.Stream().Name)
	}
	return out
}

// registeredNames is every registered domain's name, in the register's order —
// the same derivation, for the readers that key on the name alone.
func registeredNames() []string {
	domains := registeredDomains()
	out := make([]string, 0, len(domains))
	for _, d := range domains {
		out = append(out, d.Name())
	}
	return out
}

// Domains is every state-log domain this build registers, in the register's
// order: what every data node runs, a snapshot must name and two members'
// replicated estates are compared over. Exported so the whole-node gates keep
// no list of their own; a package function because the answer is this
// build's, not a running engine's.
func Domains() []statelog.Domain { return registeredDomains() }

// LogDomain is the name of the registered domain whose records live on
// stream, or "" for a stream no registered domain owns: how a write's landing
// position (`<stream>@<generation>:<seq>`) names the domain a screen's read
// floor is kept under.
func LogDomain(stream string) string {
	for _, d := range registeredDomains() {
		if d.Stream().Name == stream {
			return d.Name()
		}
	}
	return ""
}

// signerFor and verifierFor are one domain's halves of the record signature,
// per domain because the derivation binds the domain's name in: a record
// replayed from one log onto another must not verify.
func (s *stateLog) signerFor(domain statelog.Domain) (*recordseal.Signer, error) {
	return recordseal.NewSigner(domain.Name(), s.ring)
}

func (s *stateLog) verifierFor(domain statelog.Domain) (*recordseal.Verifier, error) {
	return recordseal.NewVerifier(domain.Name(), s.ring)
}

// recordKeyring is the Tier A keyring as the record seal reads it
// ([recordseal.Keyring]): every state-log record and signed channel (estate
// envelopes, broker asks, steers, presence, the relay, box reads, parked-run
// answers) derives its keys from it, each in its own key domain. The same
// material as the per-run tokens, deliberately: one keyring is one thing to
// rotate, and each derivation binds its own label, so a record MAC and a
// token MAC never match. The material is already resolved, as
// [config.Secrets.TokenMaterial]'s is: Tier A expands its document from the
// environment (never the secret store, whose key this is) before decoding, so
// nodes spelling one key as `${K}` and as the literal derive the same record
// key. The embedded broker's credentials derive the same way (ADR-0094).
func recordKeyring(boot *config.Bootstrap) recordseal.Keyring {
	if boot == nil {
		return recordseal.Keyring{}
	}
	ring := recordseal.Keyring{
		ActiveID: boot.Secrets.ActiveKeyID,
		Keys:     make([]recordseal.Key, 0, len(boot.Secrets.Keys)),
	}
	for _, key := range boot.Secrets.Keys {
		ring.Keys = append(ring.Keys, recordseal.Key{ID: key.ID, Material: key.Material})
	}
	return ring
}

// RecordKeyring is the keyring this node's state-log records are signed and
// verified under ([recordKeyring] over its Tier A), for the backup, the one
// reader outside the engine that opens a log's frames: it reads the records a
// restore replays for the files they name (internal/backup, ADR-0082). Handed
// over rather than re-read, so the backup and the appliers share one keyring.
func (e *Engine) RecordKeyring() recordseal.Keyring { return recordKeyring(e.boot) }
