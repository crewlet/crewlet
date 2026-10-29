package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/colleague"
	"github.com/crewlet/crewlet/internal/agent/skills"
	"github.com/crewlet/crewlet/internal/alarmwake"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/authz/orgchart"
	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	codecompact "github.com/crewlet/crewlet/internal/code/compact"
	codehome "github.com/crewlet/crewlet/internal/code/home"
	"github.com/crewlet/crewlet/internal/code/host"
	"github.com/crewlet/crewlet/internal/code/query"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/objstore/collect"
	"github.com/crewlet/crewlet/internal/objstore/references"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/tracker"
)

// The native backends, wired: the half of this node's durable state that only a
// company can say anything about. The half every data node runs from boot is
// the core — see core.go, and why the line between them is drawn where it is;
// a node without `data` runs neither, and reaches both halves through a data
// node (remote.go).
//
// # What is per-NODE and what is per-EPOCH, and why the split is not obvious
//
// The apply loops, the indexer and the change feeds are per-NODE: they follow
// a LOG, and a log does not change when a company revision does. Rebuilding them on an apply would restart
// every node's applier and re-run a boot reconcile on every configuration
// change, which for a company that edits its org chart twice a day is a node
// that is never established.
//
// The parsers, the prompts and the tool wiring are per-EPOCH, because all
// three depend on the org chart: the lead map a fallback routes through, the
// default project a seat files into, the reserved containers. Those are
// exactly what an apply changes.
//
// # Seat acquisition waits on ESTABLISHMENT, and nothing else does
//
// A seat whose mailbox attached before its node's tracker was established
// would answer "there is no such task" to its own tools — which is an answer
// it acts on, by filing a duplicate or abandoning work it was told to do. So
// [Engine.StateLogHydrated] gates the claim, and /ready deliberately does not:
// the control plane's rule is that lag alone never sheds a node, and a node
// that is behind should stop taking new seats rather than be declared
// unhealthy.
//
// The gate is per DOMAIN rather than per node, from each domain's own
// declaration ([statelog.Domain.GatesSeatAdmission]): a compacted domain's gap
// is a coverage number rather than a fault, so withholding seats for one would
// be the outage the number exists to avoid. And the copy verdict is per domain
// too, for every domain (ADR-0049): a domain's copy that is WRONG goes out of
// service for its own operations, whatever it declares about seats.

// native holds this node's native-backend runtime: the tracker's and the
// knowledge base's read and write sides over the core's logs, the code host's
// write side and its git surface over the code log, the lexical index the
// tracker and the knowledge base are searched through, and the change feeds.
//
// EVERY FIELD IS WRITTEN ONCE, by [Engine.startNative], before the struct is
// published to [Engine.native] — which is why there is no mutex here and why
// adding one would be misleading rather than merely redundant. What the
// running node mutates afterwards is the one field that carries its own
// synchronisation: the wait group.
//
// IT HOLDS NO LOG. The logs are the core's and run from boot on every data
// node, this company's or not; what waits for a company is only which WRITERS
// and READERS exist over them, because that is the company's to say. A node
// without `data` holds neither: it runs [remoteNative] instead, and reaches the
// halves through a data node.
type native struct {
	// built is the native-backend choices of the company that started this
	// runtime, which are what it serves until the process restarts — the
	// halves below exist exactly where it says so, and an activation that
	// says otherwise is told so ([Engine.sayBackendSwitches]).
	built builtWith

	// indexer keeps the lexical search index behind the page projection.
	indexer *search.Indexer

	// writer is the tracker's write authority and pages the wiki's.
	writer *tracker.Writer
	pages  *pages.Store

	// trackerReader and pageReader are the read paths of the tool surfaces,
	// each nil where the company keeps that half elsewhere — which is what
	// every question about the TOOLS reads ([Engine.nativeHalves]).
	trackerReader *tracker.Reader
	pageReader    *pages.Reader

	// trackerObjects is the tracker's rows as the object store's passes
	// read them, set on every runtime [Engine.startNative] publishes — the
	// same reader as trackerReader where the company's tracker is native.
	//
	// ITS OWN FIELD because the question it answers is the LOG's, not the
	// tools': the core runs the tracker's log whatever `tracker.backend`
	// says, so a company on Jira, or on none, whose pages are the engine's
	// own still applies every `tracker_files` row its log carries — and one
	// that moved its tracker off native holds every project file it ever
	// stored. Those rows name objects the collector must keep and the audit
	// must check. Read from trackerReader, the answer was "no tracker" for
	// exactly those companies, and their collector never started.
	trackerObjects *tracker.Reader

	// codeReader is the code log's rows as every reader reads them, set on
	// every runtime [Engine.startNative] publishes — the code log is the
	// core's and runs in every register, whatever the company's tools are,
	// so its pack rows are the object store's to keep from the first push.
	codeReader *code.Reader

	// codeStore and committer are the code host's WRITE side — the
	// lifecycle store the repositories' reconcile and retention write
	// through, and the committer a push is decided by — built only where
	// the company that started this runtime set `code.native`
	// ([builtWith.runsCode]) and nil otherwise: a log runs in every
	// register, but a committer exists only where somebody asked for pushes
	// to be decided. Handed out only through [Engine.writeSide], so a node
	// in a maintenance mode decides nothing. The committer is also READ, at
	// each alarm evaluation, for what it refused busy for a backlog — on
	// every data node that holds one, the git role or not — and its reading
	// is assigned field by field in [Engine.codeReading] itself, where the
	// reading's producers gate finds each field written. changeWriter and
	// checksWriter are the committer's siblings, built and handed out with
	// it: the writers of changes' conversations, each on the change's own
	// subject, and of repositories' checks, each on its repository's checks
	// subject.
	codeStore    *code.Store
	committer    *code.Committer
	changeWriter *code.ChangeWriter
	checksWriter *code.ChecksWriter

	// codeHost is this node's git surface (internal/code/host) — its cache,
	// its git pool, the hook relay, and the engine writes it builds — built
	// only where the company that started this runtime set `code.native` AND
	// this node has the git role ([Engine.hostsGit]), nil otherwise. Read per
	// request through [Engine.CodeHost], and as the writing half of this
	// node's git half through [Engine.localGit] ([gitHalf]), never captured.
	codeHost *host.Server

	// gitReads is this node's server-side reads of a repository by object id
	// (internal/code/query), built beside codeHost over the same cache and
	// pool and nil where it is: what the estate's router and server ask of
	// this node as a repository's home git node — the reading half of its git
	// half, codeHost's engine writes the other ([gitHalf]) — through
	// [Engine.localGit], per request. It holds no goroutine, and the code
	// host's stop stops it first, cutting its reads in flight before the
	// cache they read ([native.stopCodeHost]).
	gitReads *query.Server

	// mergePreviews is this node's merge previews (internal/code/home's
	// Previews), built beside gitReads over the same cache, pool and share
	// and nil where it is: what the estate's router and server ask of this
	// node for a comparison's preview — the third half of its git half
	// ([gitHalf]) — answered by the merge a repository's home records a
	// change's mergeability by. It holds no goroutine, and the code host's
	// stop stops it with gitReads, cutting its previews in flight before the
	// cache they read.
	mergePreviews *codehome.Previews

	// gitCache is the cache codeHost serves from, built and stopped with it
	// and nil where it is: held only to be READ, at each alarm evaluation
	// ([Engine.codeReading]) — what a node's maintenance refused is no
	// request's question, so no route reaches it through the host.
	gitCache *cache.Cache

	// compactor merges the packs of the repositories this node maintains and
	// publishes their layers (internal/code/compact), built with codeHost
	// and nil where it is, started with the native half's duties
	// ([Engine.startNativeDuties]); stopped first in the code host's one
	// bounded stop. A CONCRETE type, never an interface this package declares,
	// because the alarm reading's producers gate follows a call only
	// through one ([Engine.codeReading]).
	compactor *codecompact.Compactor

	// home is the change work of the repositories this node is the home git
	// node of (internal/code/home), built and started with codeHost and the
	// compactor, and nil where they are; stopped first in the code host's one
	// bounded stop, before the compactor.
	home *codehome.Loop

	// gitCacheSize is how this node sized gitCache when it built the host —
	// what [Engine.warnTiers] compares with each applied epoch's repository
	// limit. Zero where there is no host.
	gitCacheSize codeCacheSize

	// codeHostStop is the one bounded stop the teardown and the shutdown
	// share ([native.stopCodeHost]).
	codeHostStop boundedStop

	// withdrawCodeStatus withdraws this node as an answerer of the code
	// host's status (codestatus.go), registered beside codeHost and nil where
	// it is; the code host's stop runs it first.
	withdrawCodeStatus queue.Unsubscribe

	// searcher answers the knowledge seam natively.
	searcher *pages.Searcher

	// itemSearch is the tracker's own ranked search, over the SAME index
	// and the SAME fan-out — see worksearch.go. Its own field because the
	// two verbs are the tracker's and the wiki's, and a caller holding one
	// must not be able to reach the other's corpus.
	itemSearch *tracker.Searcher

	// stopSlices withdraws this node as an answerer for the fleet's
	// search fan-out. Nil when there is no queue to serve on, which is
	// every embedded engine and every test.
	stopSlices queue.Unsubscribe

	// run is the context every goroutine this half started runs under, and
	// stop is what ends it.
	//
	// HELD, not re-derived. The feeds start later than the indexer — a feed
	// publishes onto the inbound edge, so it is armed with the rest of the
	// node rather than at construction — and a goroutine registered on done
	// but started under the CALLER's context would never be ended by stop,
	// which then blocks on the wait for ever. That is not a hypothetical: it
	// wedged every engine test that shut down before its own context
	// expired.
	run  context.Context
	stop context.CancelFunc
	done sync.WaitGroup
}

// errNoCore is what the native half refuses to start without on a data node:
// every one of its writers and readers is over one of the core's logs.
var errNoCore = errors.New("engine: the native tracker and knowledge base run " +
	"over the core runtime's state log, and this engine has not started one")

// startNative opens this node's native backends, once per process, over the
// core runtime [Engine.startCore] already published.
//
// ONCE, by whichever meets the node's first company: [New] for the company a
// node boots with, or the apply that hands a node that booted without one its
// first ([Engine.startNativeFor]). Never re-run for a later revision — the
// halves follow the fleet's logs rather than a revision — and it returns
// without waiting for hydration: seat acquisition is what waits, through
// [Engine.StateLogHydrated].
//
// IT DOES NOT APPLY THE CHART. The projects and containers a company's org
// names follow a PUBLISHED company, stamped with the instant its revision was
// activated, through [Engine.followCompany] — which both callers reach once
// they have installed one. Applying it here as well would write every project
// and container twice on a node's first company.
//
// A NODE WITHOUT `data` builds none of it: it holds no copy to read or write,
// and its seats' tools reach every half through a data node
// ([Engine.startRemote]).
//
// The store and the fleet are not nil-checked: [New] refuses a Backends
// without either, so every engine that reaches this holds both.
func (e *Engine) startNative(ctx context.Context, boot *config.Bootstrap, c *Company) error {
	// A NODE WITHOUT `data` RUNS NONE OF WHAT FOLLOWS — no log, no index,
	// no copy — and answers its seats' tools through a node that does.
	if !HoldsEstate(boot) {
		return e.startRemote(c)
	}
	core := e.core.Load()
	if core == nil || core.log == nil {
		return errNoCore
	}
	// WHICH HALVES THIS COMPANY RUNS, as ONE READING of its choices kept on
	// the runtime: the halves built below are derived from it, and so is
	// what every later activation is compared against
	// ([Engine.sayBackendSwitches]). The logs behind every half run whatever
	// this says — they are the core's — so a company on Jira and Confluence
	// still applies the tracker's and the knowledge base's logs, and simply
	// has no writer or reader over either.
	built := builtFrom(c.Config)
	runTracker, wiki := built.runsTracker(), built.runsWiki()
	nodeID, sl := core.nodeID, core.log

	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	n := &native{run: runCtx, stop: cancel, built: built}
	// ONE FAILURE PATH FOR EVERYTHING BELOW, armed before the first thing
	// that outlives this call and stood down only once the half is the
	// engine's. A slice answerer's registration is copied with
	// [context.WithoutCancel] inside the queue, because a registration
	// scope is not a process one, so a bare `cancel()` on the way out would
	// not withdraw it — and a caller that retried would be running two
	// answerers scanning an index only one of them maintains.
	started := false
	defer func() {
		if !started {
			n.shutdown(ctx)
		}
	}()

	var err error
	if runTracker {
		running := sl.Domain(tracker.Domain{}.Name())
		if running == nil {
			return fmt.Errorf("engine: this node runs no tracker domain, so the " +
				"tracker has nowhere to write — the domain is in the register " +
				"and its stream failed to come up")
		}
		n.writer, err = tracker.NewWriter(tracker.WriterDeps{
			Publisher: running.publisher,
			// THE APPLIER'S OWN MEASURED RATE, so a refusal's
			// `retry_after_seconds` is derived from what this node
			// actually applies rather than from the one-record-a-second
			// floor a nil reader falls back to — which reported a node
			// two thousand records behind as half an hour behind.
			Drain:  running.runner.Drain,
			DB:     sl.estate().Reader(),
			Claims: e.backends.Coord,
			NodeID: nodeID,
			// THE CHART, read PER CALL. A project's lead is the one
			// fact a wake's fallback recipient needs that the rows
			// cannot give — `tracker_projects` carries no column for
			// it, because the applier may not read an org — and the
			// caller that needs it is a fleet singleton on a tick with
			// no tool arguments to carry a seam through.
			Leads: liveLeads{engine: e},
			// AND THE CHART AGAIN, for the one custom-field type whose
			// value is a colleague: a people field resolves through the
			// company's own roster, which belongs to the EPOCH rather
			// than to a row — so the write resolves it and the record
			// carries the handle, and no applier ever reads an org.
			World: liveSeats{engine: e},
			// AND THE COMPANY'S CLOCK, read per call like the chart: a
			// date field or a project's target date given as an instant
			// is stored as the day it falls on on this clock (ADR-0018),
			// and the clock is the epoch's to change.
			Zone: e.Zone,
			// THE NODE'S OWN WRITER ACTS AS THE SYSTEM, and every
			// surface derives its own from it with Writer.As: a seat's
			// tools act as that seat, an operator's session as that
			// credential. What is left acting as the system is what the
			// node itself does — a duty finishing an abandoned walk, a
			// repair telling somebody they were unblocked — and
			// attributing those to a person would make a machine's
			// housekeeping indistinguishable from somebody's decision.
			Actor:     nodeID,
			ActorKind: tracker.AuthorSystem,
			// The process's one recorder, so every write outcome and
			// every rejection is counted rather than merely declared.
			Metrics: e.metrics,
		})
		if err != nil {
			return fmt.Errorf("engine: tracker writer: %w", err)
		}
		// THROUGH THE DOMAIN'S OWN READ AUTHORITY, so a level asked for is
		// a level served: the refusal ladder, the coverage probe and the
		// barrier a linearizable read waits through. Built beside the
		// runner rather than here, because the health it refuses on is
		// the same one seat admission reads.
		if n.trackerReader, err = tracker.NewReader(
			sl.estate().Reader(), running.reader); err != nil {
			return fmt.Errorf("engine: tracker reader: %w", err)
		}
	}
	// THE TRACKER'S ROWS FOR THE OBJECT STORE, whatever the tools are: the
	// tool surface's own reader where there is one, and otherwise one built
	// for the estate alone, over the log the core runs regardless — see
	// [native.trackerObjects].
	n.trackerObjects = n.trackerReader
	if n.trackerObjects == nil {
		running := sl.Domain(tracker.Domain{}.Name())
		if running == nil {
			return fmt.Errorf("engine: this node runs no tracker domain, so the " +
				"object store has no estate to read project files from — the " +
				"domain is in the register and its stream failed to come up")
		}
		if n.trackerObjects, err = tracker.NewReader(sl.estate().Reader(), running.reader); err != nil {
			return fmt.Errorf("engine: tracker reader for the object store: %w", err)
		}
	}
	// THE CODE LOG'S ROWS, UNCONDITIONALLY: there is no backend to choose for
	// the code host, so every data node runs the code log in its core, and
	// the pack bundles its rows name are kept, audited and backed up from
	// here.
	{
		running := sl.Domain(code.Domain{}.Name())
		if running == nil {
			return fmt.Errorf("engine: this node runs no code domain, so its rows " +
				"have no reader — the domain is in the register and its stream " +
				"failed to come up")
		}
		// THE CURRENT EPOCH'S LIMITS, read per call: a repository's detail
		// reports what a push to it is held to now, which is what an
		// import chunks its pushes by.
		if n.codeReader, err = code.NewReader(code.ReaderOptions{Log: running.reader, Limits: e.codeLimits,
			Roster: e.codeRoster}); err != nil {
			return fmt.Errorf("engine: code reader: %w", err)
		}
		// THE CODE HOST'S WRITE SIDE, where the company hosts code. Its log
		// runs in every register (above); its committer and its lifecycle
		// store exist only where a push may be decided.
		if built.runsCode() {
			if n.codeStore, err = code.NewStore(code.StoreOptions{
				Publisher: running.publisher, DB: sl.estate().Reader(), NodeID: nodeID,
				// A TOOL'S LIFECYCLE REQUEST is judged by the committer's
				// own chart, read per request off the current epoch, and held
				// to the epoch's code.scratch — and decided nowhere on a node
				// confirmed behind the company's configuration, since every
				// kind's "who may" reads the chart (Reconciler.Admits through
				// SetAdmits).
				SeatRules: e.codeSeatRules, Current: e.admits, Policy: e.codeLifecyclePolicy,
			}); err != nil {
				return fmt.Errorf("engine: code store: %w", err)
			}
			var publisher code.Publisher = running.publisher
			if e.committerPublisher != nil {
				publisher = e.committerPublisher(publisher)
			}
			if n.committer, err = code.NewCommitter(code.CommitterOptions{
				Publisher: publisher, DB: sl.estate().Reader(), NodeID: nodeID,
				// THE DECIDING NODE'S EPOCH, read per decision, so a
				// limit an activation moves holds from that activation
				// on — on every committer at once, since each reads its
				// own node's current epoch. And the chart and the
				// calendar a seat's push is judged by, from the same
				// current epoch, read once a batch that carries one.
				Limits: e.codeLimits, Retention: e.codeDeletedRetention, SeatRules: e.codeSeatRules,
				// THE POSTURE GATE: a node confirmed behind the company's
				// configuration decides no seat's push (Reconciler.Admits
				// through SetAdmits, read per proposal).
				Current: e.admits,
				Metrics: e.metrics, Logger: logging.Get("code"),
			}); err != nil {
				return fmt.Errorf("engine: code committer: %w", err)
			}
			// AND ITS SIBLING, which the estate asks a change's
			// conversation of on the same node: the same epoch's chart
			// a reviewer and a seat are judged by, the same posture gate
			// and the same retention its repo_deleted sentence names.
			if n.changeWriter, err = code.NewChangeWriter(code.ChangeWriterOptions{
				Publisher: running.publisher, DB: sl.estate().Reader(), NodeID: nodeID,
				Retention: e.codeDeletedRetention, SeatRules: e.codeSeatRules, Current: e.admits,
				Metrics: e.metrics, Logger: logging.Get("code"),
			}); err != nil {
				return fmt.Errorf("engine: code change writer: %w", err)
			}
			// AND THE WRITER OF REPOSITORIES' CHECKS, which the estate asks
			// a check's record of on the same node: the retention its
			// repo_deleted sentence names, and the node's event stream an
			// applied ending is announced on, once, from here.
			if n.checksWriter, err = code.NewChecksWriter(code.ChecksWriterOptions{
				Publisher: running.publisher, DB: sl.estate().Reader(), NodeID: nodeID,
				Retention: e.codeDeletedRetention, Events: e.checkEvents(),
				Metrics: e.metrics, Logger: logging.Get("code"),
			}); err != nil {
				return fmt.Errorf("engine: code checks writer: %w", err)
			}
		}
		// AND ITS GIT SURFACE, on a node with the git role: the cache it
		// serves persons' clones, fetches and pushes from, which reaches
		// the rows and the committers through the estate's router like
		// every other surface. Inside this function's failure path, whose
		// shutdown stops it first.
		if e.hostsGit(built) {
			var parts codeHostParts
			if parts, err = e.buildCodeHost(ctx, nodeID, boot, running); err != nil {
				return err
			}
			n.codeHost, n.gitReads, n.gitCache, n.gitCacheSize = parts.host, parts.reads, parts.cache,
				parts.size
			n.mergePreviews = parts.previews
			n.compactor, n.home = parts.compactor, parts.home
			n.withdrawCodeStatus = parts.withdrawStatus
		}
	}
	// THE LEXICAL INDEX COVERS BOTH CORPORA, so it is built under EITHER
	// backend rather than under the wiki's.
	//
	// It used to sit inside the block below, which was correct while the
	// index was the knowledge base's alone and silently wrong the moment
	// it stopped being: a company with `tracker.backend: native` and
	// Confluence for its knowledge indexed none of its own work items,
	// served no `search_work_items`, and went on paying the embedding
	// duty for a vector on every one of them — because that duty is armed
	// on the runtime existing, which is either backend.
	//
	// BEFORE the block, because the searcher built there takes it.
	//
	// AND IT DERIVES THE BACKLINKS from the same bodies it tokenises, through
	// the one grammar the knowledge base owns — see [search.Backlinks].
	n.indexer = search.NewIndexerOver(e.backends.Store, e.domainEstate(),
		lexicalSources(runTracker, wiki)).WithLinks(pages.Links)
	// ONE QUERY-VECTOR CACHE FOR BOTH SEARCHES on this node, so a phrase
	// the palette embedded for the work search is a hit when the same
	// phrase is searched as knowledge. Read through [Engine.queryModel]
	// per query, because an apply replaces the provider and can turn
	// `knowledge.vectors` off.
	vectors := search.NewQueryVectors(e.queryModel)
	// EVERY SCAN THIS NODE RUNS ASKS ITS COPIES' VERDICT FIRST — its own
	// searches' range and every peer's slice alike (ADR-0049): a range
	// over a copy out of service is counted missing, never answered from
	// rows this node has stopped serving.
	copies := e.copies()
	// AND THE TRACKER'S OWN SEARCH over it, with its own fan-out rather
	// than the knowledge searcher's: each verb's corpus filter is its own,
	// and neither can widen into the other's.
	//
	// ONLY WHERE THE COMPANY KEEPS ITS TRACKER HERE, unlike the index. The
	// index is built under either backend because it covers both corpora;
	// this searcher reads the tracker's corpus alone, and on a company whose
	// tracker is a vendor's — or none — that corpus is empty by
	// construction. Built there anyway it made `work_search` a question this
	// node answered with nothing, where every other work question is one it
	// does not have: "there is no tracker here" read as "the tracker holds
	// nothing matching", the one confusion [Engine.WorkSearch]'s untyped nil
	// exists to prevent.
	//
	// THE BUCKETS ARE DIVIDED AMONG THE DATA NODES ([Engine.dataRoster]):
	// only a node that holds the estate holds an index to scan.
	if runTracker {
		n.itemSearch = tracker.NewSearcher(e.domainEstate(), itemRanker{
			index: n.indexer,
			fan: &search.FanOut{
				Self:    nodeID,
				Local:   search.NodeScanner{Index: n.indexer, Copies: copies},
				Peers:   e.searchPeers(),
				Roster:  e.dataRoster,
				Corpus:  n.indexer.Corpus,
				Report:  e.reportSearch,
				Enter:   e.enterSearch,
				Vectors: vectors,
			},
		})
	}
	// AND THIS NODE ANSWERS FOR ITS PEERS. Registered here rather than
	// beside the coordinator because they are different jobs on one node:
	// every node with an index answers, whether or not anybody on it ever
	// searches.
	if e.backends.Queue != nil {
		n.stopSlices, err = search.ServeSlices(runCtx, e.backends.Queue, nodeID,
			search.NodeScanner{Index: n.indexer, Copies: copies})
		if err != nil {
			return fmt.Errorf("engine: serve search slices: %w", err)
		}
	}

	if wiki {
		running := sl.Domain(pages.Domain{}.Name())
		if running == nil {
			return fmt.Errorf("engine: this node runs no pages domain, so the " +
				"knowledge base has nowhere to write — the domain is in the " +
				"register and its stream failed to come up")
		}
		if n.pages, err = pages.NewStore(pages.Options{
			Publisher: running.publisher, DB: sl.estate().Reader(),
			// LIVE off the epoch for the reason the searcher below is:
			// `knowledge.skills_container`, `knowledge.root_space` and
			// every `space:` are Tier B, and this store is built once per
			// node while an apply can move any of them underneath it.
			Reserved: func() pages.Reserved {
				return reservedContainers(e.Company())
			},
		}); err != nil {
			return fmt.Errorf("engine: pages store: %w", err)
		}
		if n.pageReader, err = pages.NewReader(pages.ReaderOptions{
			DB: sl.estate().Reader(), Log: running.reader,
		}); err != nil {
			return fmt.Errorf("engine: pages reader: %w", err)
		}
		// LIVE off the epoch, not off the company this node booted
		// with: `knowledge.skills_container` is Tier B, and this
		// searcher is built once per node while an apply can move the
		// key underneath it.
		//
		// THE INDEX ITSELF IS NOT BUILT HERE — see above. It covers the
		// tracker too, and gating it on the WIKI's backend left a
		// tracker-native company on Confluence indexing none of its own
		// work items.
		n.searcher = pages.NewSearcher(pages.SearcherOptions{
			Index: n.indexer, SkillsContainer: e.skillsContainer,
			Node:    nodeID,
			Peers:   e.searchPeers(),
			Roster:  e.dataRoster,
			Report:  e.reportSearch,
			Enter:   e.enterSearch,
			Vectors: vectors,
			Copies:  copies,
		})
	}

	// NO PROJECTOR LOOP HERE ANY MORE. Both native backends are state-log
	// domains, and their apply loops are the state log's own — the core's,
	// started at boot and stopped with it, and reporting their position
	// rather than a hydration flag.
	n.done.Add(1)
	go func() {
		defer n.done.Done()
		n.indexer.Run(runCtx)
	}()

	// PUBLISHED, whole: every field above is written before the store,
	// which is what lets every reader load it without a lock.
	e.native.Store(n)
	// THE HALF IS THE ENGINE'S FROM HERE, so the cleanup above stands
	// down and [Engine.stopNative] — the same shutdown — is what ends it.
	// Its object collector is NOT started here: it appends, and a runtime
	// built in a capacity window must not ([Engine.startNativeCollector]).
	started = true
	log.InfoContext(ctx, "native_backends_started",
		"tracker", runTracker, "knowledge", wiki, "code", built.runsCode(), "git", n.codeHost != nil)
	return nil
}

// objectEstates is every domain of this runtime whose rows may name objects,
// as the object store's passes read them — the tracker's and the code log's,
// beside which [objectEstatesFor] adds the core's artifacts log. Empty only on
// a zero runtime.
//
// ONE ESTATE PER DOMAIN A DECLARATION NAMES, whatever backends the company
// chose: a domain's rows exist because its log runs, not because its tool
// surface does, and the core runs every domain's log on every data node.
// Offered only where the company's tools were, the tracker's estate was
// missing on every company whose knowledge base is the engine's own and whose
// tracker is not — Jira, or none — so the rows naming objects were read by
// nothing, and there was no collector at all: nothing collected, nothing
// audited, and NOTHING SAID SO. The empty list never reached
// [collect.Sources], which would have refused it by name —
// [Engine.startNativeCollector] returned before building the sources, with no
// warning and no status line, and the only sign was a `crewlet objects
// status` that never showed a pass. That early return is gone
// ([Engine.startCollectorOver]).
//
// The engine's own tests build the sources from this list against
// internal/objstore/references, so a consumer declaring a table in a new
// domain fails the build here rather than leaving the passes unstarted in
// production.
func (n *native) objectEstates() []collect.Estate {
	if n.trackerObjects == nil {
		// A ZERO RUNTIME — a test's — holds no domain's rows. Every
		// runtime [Engine.startNative] publishes sets the field, and a
		// collector over one that did not is refused by name
		// ([Engine.startCollectorOver]).
		return nil
	}
	estates := []collect.Estate{tracker.ObjectEstate{Reader: n.trackerObjects}}
	if n.codeReader != nil {
		estates = append(estates, code.ObjectEstate{Reader: n.codeReader})
	}
	return estates
}

// startNativeFor brings the native halves up for the first company an APPLY
// hands a node that is not running them, reporting whether it did.
//
// # The bug it fixes
//
// [Engine.startNative] was reachable from [New] alone. A node that booted with
// no company — the documented way to start one and then `PUT /config`, or
// create the company from the dashboard — skipped it, and no apply ever ran
// it: the tracker, the knowledge base and their tools were missing for the
// life of the process. Every tool call answered that the backend was not
// wired, no project existed for a task to be filed into, and the quickstart's
// "bootstrap live without restarting" held for everything but the company's
// own records. A restart brought them, because then the company arrived at
// boot.
//
// # Where it runs in an apply, and why there
//
// BEFORE the tools are equipped, because the native tools are registered only
// where their halves exist, and before the inbound edge is started, whose
// parsers include the native ones — both read the halves this brings up. It
// comes after the revision is BUILT, so a revision that cannot be built never
// starts anything.
//
// The loops [New] starts beside the halves once the node publishes — the
// embedding duty and the change feeds — start here too, on the same
// condition, so halves met at an apply are the halves a boot would have
// built. The logs they ride, the trim over them and the identity estate's
// duties are the CORE's, already running from boot. Nothing waits for
// hydration: seat acquisition does, through [Engine.StateLogHydrated].
//
// # Every company starts it
//
// There is no backend test here: the halves a company on Jira and Confluence
// runs are none, and the lexical index over nothing — which is still the
// answer "this company keeps no tracker here", held once rather than asked of
// every surface.
//
// # What a later refusal leaves
//
// The halves are NOT taken down if the apply is refused after this. They
// follow the fleet's logs rather than the revision, so a node running them
// with the previous epoch still serves that epoch correctly — and the retry
// finds them already up. Which halves it has (the tracker, the knowledge base,
// the code host) are the ones this company declared; a later revision that
// changes them takes effect on restart, as switching a backend always has —
// and says so at its activation ([Engine.sayBackendSwitches]).
func (e *Engine) startNativeFor(ctx context.Context, c *Company) (bool, error) {
	if e.native.Load() != nil || e.remote.Load() != nil || c == nil {
		return false, nil
	}
	if err := e.startNative(ctx, e.boot, c); err != nil {
		return false, err
	}
	if e.mode.Publishes() {
		e.startNativeDuties(ctx)
		e.startNativeCollector(ctx)
		e.startNativeFeeds(ctx)
	}
	return true, nil
}

// startNativeCollector starts the object store's collector duty on this data
// node: it deletes objects no row names and audits what the store has lost,
// both against the rows of every domain a declared table names — the native
// half's and the core's artifacts log alike. It runs wherever the node runs
// its native half, whatever backends the company chose
// ([native.objectEstates]): a company that moved its tracker off native still
// holds the rows that name its project files' objects. A runtime it cannot
// start over is logged as `object_collector_not_started`, never passed over in
// silence ([Engine.startCollectorOver]). ONE CALL FOR BOTH CALLERS, [New] and
// [Engine.startNativeFor], for [Engine.startNativeDuties]'s reason.
//
// # Only where the mode publishes
//
// Both callers reach it only past the gate that keeps every publisher off a
// node in a maintenance mode — [New]'s, and [Engine.startNativeFor]'s beside
// the duties — rather than through a check of its own, for the reason that
// gate gives. Every pass PINS the estate first — a linearizable read of the
// tracker's log, which appends a barrier to it — and a node in a capacity
// window appends nothing: its reader refuses that read
// ([statelog.RefuseMaintenance]), because a barrier is a record on a log whose
// usage the window is measuring. Given a pin that appends nothing instead, a
// collection would delete against an end nobody established, the one read the
// barrier exists for. So the collector stands down with the duties, and the
// window costs it nothing: no write lands a file whose object a collection
// would free.
//
// THE DECLARED LIST, read through each domain's estate — never a source written
// here — so the tables the collector keeps alive, and the Required ones among
// them the audit checks and the backup carries, are the ones the schema gate
// holds.
func (e *Engine) startNativeCollector(ctx context.Context) {
	if err := e.startCollectorOver(ctx, e.native.Load()); err != nil {
		log.WarnContext(ctx, "object_collector_not_started", "error", err)
	}
}

// startCollectorOver starts the collector over n's estates and the core's
// artifacts estate ([objectEstatesFor]), and answers why it did not — which
// [Engine.startNativeCollector] logs.
//
// A RUNTIME THAT OFFERS NO ESTATE IS REFUSED, NEVER SKIPPED. Every runtime
// [Engine.startNative] publishes offers one per declaring domain
// ([native.objectEstates]), so one that offers none is a wiring mistake, and
// [collect.Sources] refuses it by name — the declared table no estate reads —
// which is the line an operator can find. This step used to return early and
// silently on an empty list, and that is how every company whose tracker was
// not native went without a collector with nothing saying so. Only a node
// with no native half at all starts none without a word — nil, because it has
// met no company yet, or it holds no estate and asks a data node for every
// row.
func (e *Engine) startCollectorOver(ctx context.Context, n *native) error {
	if n == nil {
		return nil
	}
	refs, err := collect.Sources(references.All, objectEstatesFor(n, e.ArtifactsReader())...)
	if err != nil {
		return err
	}
	return e.startObjectCollector(ctx, refs)
}

// startNativeDuties arms the native half's fleet-singleton duties: the vector
// domain's one writer, which turns the tracker's and the knowledge base's
// sources into vector records, the trash reaper, which purges what has been in
// a trash past its term ([Engine.startTrashReaper]), and, where the half hosts
// code, the purge of deleted repositories ([Engine.startCodeRetention]) — and,
// where it serves git, this node's own COMPACTOR and its HOME'S CHANGE WORK,
// which no lease decides: every git node runs both, the compactor for the
// repositories it maintains and the change work for those it is home to. ONE
// CALL FOR BOTH CALLERS, [New] and [Engine.startNativeFor], so halves met at
// an apply cannot be missing a duty a boot would have armed. The trim, the
// identity estate's duties and the usage publisher are the core's, armed at
// boot by [Engine.startCoreDuties].
func (e *Engine) startNativeDuties(ctx context.Context) {
	c, n := e.core.Load(), e.native.Load()
	if n == nil || c == nil {
		return
	}
	// Without it every other half of semantic search is present and
	// correct over an empty corpus — which reports as a healthy domain
	// rather than as a missing one.
	e.startEmbedding(ctx, c.log)
	e.startTrashReaper(ctx)
	// AND THE CODE HOST'S PURGE, where the half hosts code: without it a
	// repository removed from code.repos is deleted and never purged, and
	// its name and its packs are held for ever.
	e.startCodeRetention(ctx, n)
	// AND THE COMPACTOR, where the half serves git ([Engine.buildCodeHost]
	// built it unstarted): its three loops, the compaction's, the publication's
	// and the expunge's, started HERE for the two reasons both callers reach
	// this past: the runtime it reads through is PUBLISHED — each loop's first
	// pass runs at once this node is listed among the git nodes, and
	// this node's own copy answers its due listings from the published
	// runtime, so a pass started before found its listing unserved — and the
	// node's mode PUBLISHES: a compaction stores its outputs, and a
	// publication its layer, in the object store and appends its record, and
	// a node a capacity window restarted to write nothing must start nothing
	// that writes on its own. Its stop is the code host's
	// ([native.stopCodeHost]).
	if n.compactor != nil {
		n.compactor.Start()
	}
	// AND THE HOME'S CHANGE WORK, for the compactor's two reasons: its first
	// pass asks the published runtime who is home and lists the rows its
	// copy answers, and every turn asks a repository's writers for records.
	if n.home != nil {
		n.home.Start()
	}
}

// stopNative ends this node's native halves. Nil-safe, which is the node that
// has met no company.
func (e *Engine) stopNative(ctx context.Context) { e.native.Load().shutdown(ctx) }

// shutdown ends everything this half started and WAITS for it.
//
// ONE IMPLEMENTATION FOR TWO CALLERS — [Engine.stopNative] and the failure path
// in [Engine.startNative] — because a half-built runtime leaks precisely what a
// built one does: an answerer registered on the broker is a claim on buckets a
// peer is counting on whether or not the node that made it went on to serve a
// seat. A second copy of this order is how one of them stops matching the
// other.
//
// Nil-safe throughout, because the failure path can reach it with no answerer
// registered, no code host built and nothing in the wait group.
//
// IT DOES NOT STOP THE LOGS, which are the core's: see [Engine.stopCore], which
// the teardown runs after this.
//
// # Why it takes a context it then strips
//
// The withdrawal below is a call to the broker, so it needs a context that is
// not already cancelled — and BOTH of the contexts in reach routinely are. The
// caller's is a shutdown signal or a failed boot; this runtime's own is
// cancelled by the [native.stop] two lines further down, which is what makes a
// second shutdown a no-op rather than a hang. So the caller's travels here for
// its VALUES — the trace the teardown belongs to — and [context.WithoutCancel]
// is what makes it usable, which is the rule this tree states once: a cleanup
// that inherits a dead context does nothing at all.
func (n *native) shutdown(ctx context.Context) {
	if n == nil {
		return
	}
	if n.stopSlices != nil {
		// FIRST, and before the context that would cancel an answerer
		// mid-scan: a node that has decided to go away must stop
		// claiming buckets its peers are counting on before it stops
		// being able to scan them. Withdrawing costs a coordinator one
		// missing assignment on its next search, where a registration
		// that outlived the scan costs it a silent empty slice it
		// counts as answered.
		_ = n.stopSlices(context.WithoutCancel(ctx))
	}
	// THE CODE HOST, THEN THE COMMITTER, BEFORE THE LOG THEY PUBLISH ON,
	// which the core stops after this. The teardown stopped both already,
	// the host first, ahead of the estate subject ([Engine.stopCodeHost],
	// [Engine.stopCommitter]); these are the failure path's stops. A second
	// one of either costs nothing: the host's stop runs once however many
	// ask for it, so a first stop cut short by its bound is not waited for
	// again ([native.stopCodeHost]), and the committer's first stop returns
	// only once its batches have.
	n.stopCodeHost(ctx)
	n.stopCommitter(ctx)
	n.stop()
	n.done.Wait()
}

// NativeStarted reports whether this node has brought its native halves up —
// whether it has met its first company — on a data node its own, and on one
// without `data` the halves it reaches through a data node ([remoteNative]).
//
// MONOTONIC: false until the halves are published and never false again, so a
// caller that reads this FIRST and a half's accessor AFTER cannot be told the
// half is absent by configuration when it is merely not up yet. The API's live
// sources are that caller: a half that is not up answers 503, and one this
// company does not run answers as a route or a question this node does not
// have.
func (e *Engine) NativeStarted() bool {
	return e.native.Load() != nil || e.remote.Load() != nil
}

// halves is which halves this company runs natively here, as one reading of
// this node's runtime found them ([Engine.nativeHalves]).
type halves struct {
	// tracker and wiki say which halves the company runs natively, however
	// this node reaches them: through its own copy on a data node, through a
	// data node on a node without `data`. Both false on a node running no
	// native runtime — none built yet, which is a node that booted with no
	// company and has not applied one — and on one whose company runs both
	// halves on vendors.
	tracker, wiki bool
	// code says the company hosts its repositories in the engine
	// (`code.native`) and this node's runtime was built to serve them —
	// through its own copy and committer on a data node, through a data node
	// on a node without `data`. Counted by the choice the runtime was built
	// with rather than by a reader, because the code log's reader exists in
	// every register whatever the company hosts.
	code bool
	// own is this data node's own runtime, for a reader that needs more of
	// it than which halves it runs (the ranked item search, [OperatorNative];
	// whether a half is reached through this node's copy or a data node's,
	// [Engine.skillSource]) — and nil on a node without `data`, which holds
	// no runtime of its own and reaches every half through a data node, and
	// on a node running none.
	// Taken in the same reading as the halves, so the two never describe
	// different runtimes.
	own *native
}

// nativeHalves is [halves] for e at this instant: THE ONE DERIVATION of which
// halves a node runs, read by seat admission ([Engine.StateLogHydrated]), by
// whether a node may keep its seats ([Engine.serviceable]), by every read seam
// the operator's surfaces are handed ([OperatorNative], [OperatorCode], and
// through the second the code tools a seat and an operator read with,
// [CodeToolDeps]), by the seats' own tracker and knowledge-base seams
// ([Engine.trackerHalves], [Engine.pageHalves]), by the inbound parsers
// ([Engine.nativeParsers]), by the
// knowledge searcher ([Engine.Knowledge]) and by where the skills are read
// ([Engine.skillSource]) — so a change to what counts as running a half (a new
// kind of runtime, a half that needs more than its reader) is made here once
// and cannot reach one of them and not the others. Each of those used to read
// the two runtime pointers itself, and the parsers counted a data node's
// halves by its WRITERS where everything else counted its readers: the same
// fact today, because one block builds both, and two facts the first time a
// runtime is built with one and not the other.
//
// One atomic load on a node without `data` and two on a data node, which holds
// no remote runtime: the two pointers are never both set, since a node builds
// one runtime or the other once per process.
func (e *Engine) nativeHalves() halves {
	if r := e.remote.Load(); r != nil {
		return halves{tracker: r.tracker, wiki: r.wiki, code: r.built.runsCode()}
	}
	if n := e.native.Load(); n != nil {
		return halves{tracker: n.trackerReader != nil, wiki: n.pageReader != nil,
			code: n.built.runsCode(), own: n}
	}
	return halves{}
}

// Tracker is this node's OWN copy's tracker read side, or nil — what this
// node's router answers from while its copy serves, and what a test
// reads to see one node's copy. A surface reads through [OperatorWork]
// instead, which answers from a peer's copy once this one is out of service.
func (e *Engine) Tracker() *tracker.Reader {
	n := e.native.Load()
	if n == nil {
		return nil
	}
	return n.trackerReader
}

// TrackerWriter is this node's tracker write side, or nil — nil in a
// maintenance mode too; see [Engine.writeSide].
func (e *Engine) TrackerWriter() *tracker.Writer { return e.writeSide().tracker }

// Pages is this node's OWN copy's knowledge read side, or nil — see
// [Engine.Tracker]; a surface reads through [OperatorPages].
func (e *Engine) Pages() *pages.Reader {
	n := e.native.Load()
	if n == nil {
		return nil
	}
	return n.pageReader
}

// PagesStore is this node's knowledge write side, or nil — nil in a
// maintenance mode too; see [Engine.writeSide].
func (e *Engine) PagesStore() *pages.Store { return e.writeSide().pages }

// NativeSearcher is the native knowledge searcher, or nil.
func (e *Engine) NativeSearcher() *pages.Searcher {
	n := e.native.Load()
	if n == nil {
		return nil
	}
	return n.searcher
}

// startNativeFeeds starts the change feeds, per node.
//
// A FLEET-WIDE GROUP rather than a duty: every data node pulls, so a change is
// handled by whichever gets there first and a lease flap on one node does not
// stall the company's notifications. Started here rather than per epoch,
// because a feed follows a DOMAIN and a domain does not change when a company
// revision does.
//
// WITH THE NATIVE HALF rather than the core, because what a feed does with a
// record is wake the seat it names — and a node that has met no company has
// no seats, no inbound edge to publish a wake onto and no parser to route one.
func (e *Engine) startNativeFeeds(ctx context.Context) {
	n, c := e.native.Load(), e.core.Load()
	if n == nil || c == nil || c.log == nil {
		return
	}

	// EVERY SOURCE IS A LOG NOW, and the coordination Feeder this function
	// used to require is gone with the last bucket family. A translator
	// says what it can read and a feed says how to run a durable consumer
	// over one domain's own stream; which estate the records are in was
	// the piece the domains replaced outright.
	//
	// IN THE REGISTER'S ORDER and through [Engine.feedFor], which holds each
	// feed to the group its domain declares — the consumer the trim waits
	// on. A domain whose feed disagrees with its declaration gets no feed
	// and an error naming it, rather than a trim that silently waits for
	// ever or silently waits for nothing.
	for _, domain := range registeredDomains() {
		running := c.log.Domain(domain.Name())
		if running == nil {
			continue
		}
		translator, opener, err := e.feedFor(running)
		if err != nil {
			log.ErrorContext(ctx, "changefeed_unavailable",
				"domain", domain.Name(), "error", err.Error())
			continue
		}
		if translator == nil {
			// THE DOMAIN DECLARES NO WAKE FEED, and its trim term is
			// absent for the same reason.
			continue
		}
		// THROUGH THE ONE DOOR every fleet-wide group passes
		// ([Engine.joins]): a feed reads its domain's log, applied
		// only where the estate is held.
		if !e.joins(ctx, translator.Source().Group) {
			continue
		}
		feed, err := changefeed.New(changefeed.Options{
			Opener: opener, Publisher: e.backends.Queue,
			Claims: e.backends.Fleet, Translator: translator,
			Metrics: e.metrics,
		})
		if err != nil {
			log.ErrorContext(ctx, "changefeed_unavailable",
				"source", translator.Source().Name, "error", err.Error())
			continue
		}
		n.done.Add(1)
		go func() {
			defer n.done.Done()
			// THE NATIVE HALF'S OWN CONTEXT, never the caller's: this
			// goroutine is joined by stopNative, which ends that one.
			//nolint:contextcheck // n.run is [context.WithoutCancel] of
			// the boot context: a feed started under the CALLER's would be one
			// [native.shutdown] can never end, and its wait would block for ever.
			if err := feed.Run(n.run); err != nil {
				log.ErrorContext(ctx, "changefeed_stopped",
					"source", translator.Source().Name, "error", err.Error(),
					"detail", "native writes still land; nothing is woken by them "+
						"until this node or a peer reopens the feed")
			}
		}()
	}
}

// nativeParsers is what the engine's own sources contribute to the inbound
// edge: the native backends' parsers, and the alarms' (internal/alarmwake).
//
// PER EPOCH, unlike everything else in this file: a parser's one
// company-derived input is the org chart — the native backends' LEAD MAP, the
// alarms' granted seats — which is exactly what an apply changes. See
// [Engine.reconcileNative].
func (e *Engine) nativeParsers(c *Company) ([]notify.Parser, []notify.Prompt) {
	if c == nil {
		return nil, nil
	}
	// THE ALARMS' WAKES, on every node whatever halves it runs: a
	// transition is routed by the chart's health grant alone — to the
	// seats the health tools admit, by the one rule that admits them
	// (orgchart.GrantedSeats) — and reads no row, so every node routes one
	// as a data node does.
	parsers := []notify.Parser{alarmwake.NewParser(alarmwake.ParserOptions{
		Granted: orgchart.GrantedSeats(c.Org, authz.SeatGrantHealth),
	})}
	prompts := []notify.Prompt{alarmwake.Prompt{}}
	// THE HALVES THIS COMPANY RUNS NATIVELY, wherever they are held: a
	// parser reads a record's own routing snapshot and the chart, never a
	// row, so a node that holds no data routes a native change exactly as
	// a data node does.
	run := e.nativeHalves()
	if run.tracker {
		// NO LEAD MAP AND NO BASE URL. The tracker's own parser reads a
		// record's routing snapshot, which the WRITER resolved at commit
		// — a mention resolved at read time names whoever holds the role
		// later, which is a different person.
		parsers = append(parsers, tracker.NewParser(tracker.ParserOptions{}))
		prompts = append(prompts, tracker.Prompt{})
	}
	if run.wiki {
		// THE LEAD MAP, and the base its notifications link a person to —
		// `api.external_url`, the address people reach this deployment at,
		// since the link is opened by whoever the change woke and never by a
		// vendor ([Engine.publicBase]).
		parsers = append(parsers, pages.NewParser(pages.ParserOptions{
			Leads: containerLeads(c.Org), BaseURL: e.publicBase(c),
		}))
		prompts = append(prompts, pages.Prompt{})
	}
	return parsers, prompts
}

// reconcileNative brings the native runtime to a newly applied epoch: it says
// which of the epoch's choices wait for a restart, and swaps the native
// parsers.
//
// NOT the appliers, the index, the stores or the feeds. Those follow a LOG,
// which no company revision changes; rebuilding them here would restart this
// node's apply loops and re-run a boot reconcile on every configuration edit.
//
// There is no retirement branch. Switching `tracker.backend` away from
// native is not a live gesture — the runtime, its halves and its feeds are
// built once per process, with the first company that runs one — so a
// revision that changes it takes effect on restart, and the parser staying
// registered until then is the honest state: the records are still there
// and still reachable. What this does instead is SAY so, first: see
// [Engine.sayBackendSwitches].
//
// # The applies are the published company's, not the inbound edge's
//
// The parsers ride the inbound edge ([Engine.swapNativeParsers]), and a node
// with none has nothing to swap. The chart's projects, the knowledge
// containers and the repositories code.repos declares do not: they are
// derived from the org chart and the company document rather than from the
// parsers, and are written through the runtime's own writers, so they follow
// the PUBLISHED company — [Engine.followCompany] is where they run, after the
// publish, whatever the edge is doing. Running them here as well would write
// them twice per apply and once against a company that is not current yet.
func (e *Engine) reconcileNative(ctx context.Context, c *Company) {
	built, runs := e.runtimeBuilt()
	if !runs {
		return
	}
	// THE CHOICES THIS RUNTIME WAS NOT BUILT WITH, said once per activation:
	// the runtime is built once per process, so a revision that moves a
	// native backend activates cleanly here and changes nothing until a
	// restart — and before this line nothing said so. FIRST: the line is
	// about the runtime, and a node with no edge (one in a maintenance mode)
	// runs one all the same.
	e.sayBackendSwitches(ctx, built, c)
	e.swapNativeParsers(ctx, c)
}

// swapNativeParsers swaps the native parsers on this node's inbound edge for
// the epoch c's, and does nothing on a node whose edge is not running.
//
// The same edge every vendor reconciler sits on, and for the same reason: a
// node that kept its boot-time parser would route the new revision's work by
// the old company's org chart — an item filed into a project whose lead moved
// would keep waking the seat that used to own it.
func (e *Engine) swapNativeParsers(ctx context.Context, c *Company) {
	e.notify.mu.Lock()
	svc := e.notify.service
	e.notify.mu.Unlock()
	if svc == nil {
		return
	}
	parsers, prompts := e.nativeParsers(c)
	for i, parser := range parsers {
		if err := svc.Replace(parser, prompts[i]); err != nil {
			// THE PREVIOUS PARSER KEEPS RUNNING, the same posture every
			// vendor takes: routing by a stale org chart is worse than
			// the new one and much better than not routing at all.
			log.ErrorContext(ctx, "native_reconcile_failed",
				"source", parser.Source(), "error", err.Error(),
				"detail", "the previous routing is still current")
		}
	}
}

// applyContainers makes the knowledge containers this company names exist.
//
// # The bug this fixes
//
// [pages.Store.EnsureContainer] HAD NO CALLER. Its own doc says it "runs on
// every boot for every unit's space" and nothing ever ran it, so
// `pages_containers` was empty in every deployment that ever existed: the
// Knowledge rail said "this node knows about no containers yet" beside a
// company whose agents had written pages, `GET /containers` answered `[]`
// for ever, and the one piece of the knowledge base an operator navigates by
// was unreachable from a running engine.
//
// A page merely NAMES its container — the string is on the page's own row —
// so the pages themselves were fine and only the thing that lists them was
// missing. Which is precisely why nothing caught it: every read that takes a
// container as a parameter worked, and only the read that ENUMERATES them was
// empty, which is indistinguishable from a company that has none.
//
// # Where the keys come from
//
// The org chart's spaces, plus the two RESERVED containers. A unit's `space:`
// is its knowledge identity and a seat's own `space:` is the same thing one
// level down — the same pair [chartProjects] reads for the tracker. The
// reserved two are the engine's own: tool skills, and the org root every
// seat's onboarding chain starts at. Both are materialised because the engine
// writes into them itself, and a container the engine writes into and cannot
// list is the same defect one layer in.
//
// # Stamped with the ACTIVATION, on the chart's terms
//
// c.ActivatedAt is the instant the revision this company was applied from was
// activated, and every container is stamped with it exactly as
// [Engine.applyChart] stamps a project — for the same two reasons: a node
// applying an older revision late must not rewrite the containers' names and
// purposes back to its own older view, and a reapply of one activation must
// write nothing. A company no activation has named yet has nothing to stamp,
// and the store refuses a zero ([pages.ErrNoActivation]); so it is not applied
// here, and the reconciler's first apply — which carries the pointer's instant
// — is. See [pages.Store.EnsureContainer].
//
// # Best effort, and idempotent
//
// A failure is LOGGED rather than raised, exactly as [Engine.applyChart]
// explains: this is one clause of a convergence and the rest of it stands
// without it. Running on every published company is free after the first,
// because EnsureContainer decides nothing when the row it finds already says
// what the company says at this activation — which is the guard its own doc
// was written around.
func (e *Engine) applyContainers(ctx context.Context, c *Company) {
	store := e.PagesStore()
	if store == nil || c == nil || c.ActivatedAt.IsZero() {
		return
	}
	var wrote []string
	for _, want := range chartContainers(c) {
		_, changed, err := store.EnsureContainer(ctx, c.ActivatedAt,
			want.Key, want.Name, want.Purpose)
		if err != nil {
			// EVERY CONTAINER IS ATTEMPTED. One key's refusal must not
			// leave the rest of a company's knowledge base unlistable,
			// and the caller is a reconcile that runs again.
			log.ErrorContext(ctx, "knowledge_container_not_applied",
				"container", want.Key, "error", err.Error(),
				"detail", "pages in it are readable by address and the "+
					"container will not appear in a listing; the next config "+
					"apply or restart retries it")
			continue
		}
		// ONLY WHAT WAS WRITTEN. This runs on every boot and every apply,
		// so the ordinary outcome is that every row already says what the
		// chart says — and a line naming all of them on every restart is
		// a log that reports a company nobody edited as one that changed.
		if changed {
			wrote = append(wrote, want.Key)
		}
	}
	if len(wrote) > 0 {
		log.InfoContext(ctx, "knowledge_containers_applied", "containers", wrote)
	}
}

// chartContainer is one container as the company declares it.
type chartContainer struct {
	Key     string
	Name    string
	Purpose string
}

// chartContainers is every knowledge container this company names.
//
// DE-DUPLICATED ON THE KEY and FIRST DECLARATION WINS, for the reason
// [chartProjects] gives for the same shape: two units legitimately share a
// space, and one container written twice in one pass would contend with
// itself at the broker and log a failure for a configuration that is fine.
//
// The reserved two come LAST, so a company that has somehow named one of them
// as a unit's space keeps the unit's own name on it rather than having the
// engine's generic label overwrite what a founder wrote. The config loader
// refuses that arrangement, so this is the belt to its braces.
func chartContainers(c *Company) []chartContainer {
	if c == nil || c.Config == nil {
		return nil
	}
	var out []chartContainer
	seen := map[string]bool{}
	add := func(key, name, purpose string) {
		// THE ONE CANONICAL FORM ([pages.ContainerKey]), which is what a
		// container key is everywhere else: a page carries `ENG` and a
		// chart that wrote `eng` — or ` ENG` — would create a second
		// container no page is in. A copy of the rule here is how the
		// forms drifted before there was one.
		key = pages.ContainerKey(key)
		if key == "" || seen[key] {
			return
		}
		seen[key] = true
		out = append(out, chartContainer{Key: key, Name: name, Purpose: purpose})
	}
	if c.Org != nil {
		for unit := range c.Org.AllUnits() {
			add(unit.Space, unit.Name, unit.Purpose)
		}
		for role := range c.Org.AllRoles() {
			// A SEAT'S OWN SPACE takes the SEAT's name, which is what a
			// founder naming one on a seat meant — a container for that
			// seat's own writing rather than for its unit's.
			add(role.Space, role.Name, "")
		}
	}
	add(c.Config.RootSpaceKey(), "Company",
		"The organisation's own pages, starting with the Onboarding page every seat reads first.")
	add(c.Config.SkillsContainerKey(), "Tool skills",
		"Prompt fragments the engine injects into a phase. Excluded from knowledge search.")
	return out
}

// applyChart makes the projects this company's chart names exist.
//
// # Why it runs here and not at the first write
//
// A create is a SEQUENCE — it takes the next number from its project's own
// counter and then writes the task — so the project has to be an object before
// anything can be filed in it. Creating it inside the create instead is the
// shape that gives two nodes two projects, two counters and two ENG-1s when
// they file at once. Deriving it from the chart makes it one arbitrated write
// per project, on that project's own subject.
//
// # And on every company publish rather than only at boot
//
// A founder opens a unit with a new `project:` key and the seats in it start
// filing immediately. A reconcile that only ran at boot would leave every one
// of those refused with "project X is not on this node" until somebody
// restarted the fleet — a failure whose remedy is invisible from the message.
// This runs from the steps every published company goes through
// ([Engine.followCompany]), at boot and on every apply.
//
// After the first node has done it every later pass is one local read per
// project: the decide compares the three chart-owned fields against the row
// and says nothing when they match, so the losers of the broker's arbitration
// and every later publish of the same company write nothing.
//
// # Stamped with the ACTIVATION
//
// c.ActivatedAt is the instant the revision this company was applied from was
// activated, and it is what every project is stamped with
// ([tracker.Project.ChartEpoch]): a node applying an older revision late must
// not walk the fleet's newer project names back to its own. The activation's
// instant and never this node's clock, because every node applies one
// activation separately and only the instant on the pointer is the same on all
// of them. A company no activation has named yet — a Tier B file a node booted
// with — applies nothing here; the reconciler's first apply carries the
// pointer's instant and applies it then.
//
// # Best effort, and what that costs
//
// A failure is LOGGED rather than raised, because this is one clause of an
// epoch apply and the rest of it — the routing, the models, the tools — is
// still correct without it. What a failure costs is exactly the projects that
// did not land, and the next apply or the next boot retries them.
func (e *Engine) applyChart(ctx context.Context, c *Company) {
	writer := e.TrackerWriter()
	if writer == nil || c == nil || c.Org == nil || c.ActivatedAt.IsZero() {
		return
	}
	chart := chartProjects(c.Org)
	if len(chart) == 0 {
		return
	}
	wrote, err := writer.ApplyChart(ctx, c.ActivatedAt, chart)
	if err != nil {
		log.ErrorContext(ctx, "tracker_chart_not_applied",
			"error", err.Error(), "wrote", wrote,
			"detail", "a project the chart names may not exist yet, and work "+
				"filed into it is refused until it does; the next config apply "+
				"or restart retries it")
		return
	}
	if len(wrote) > 0 {
		log.InfoContext(ctx, "tracker_chart_applied", "projects", wrote)
	}
}

// chartProjects is every project the org chart names, with the three fields
// the chart owns.
//
// DE-DUPLICATED ON THE KEY, because two units legitimately share a project —
// that is what [projectLeads] reports on — and one project written twice in
// one pass would contend with itself at the broker and log a failure for a
// configuration that is fine. The FIRST declaration wins, which matches how
// the lead is chosen for the same key.
func chartProjects(o *org.Organization) []tracker.ChartProject {
	var out []tracker.ChartProject
	seen := map[string]bool{}
	add := func(key, name, purpose, unit string) {
		// NORMALIZED THE WAY EVERY OTHER SCOPE IS, through the org's own
		// helper: a project key is compared upper everywhere — a task's
		// key is `ENG-42` and the column is an exact match — and a chart
		// that wrote `eng` would create a project no board could find.
		key = org.NormalizeScope(key)
		if key == "" || seen[key] {
			return
		}
		seen[key] = true
		out = append(out, tracker.ChartProject{
			Key: key, Name: name, Purpose: purpose, Unit: unit,
		})
	}
	// THE UNIT COLUMN IS THE UNIT'S KEY, never its name. The project's
	// unit is what a task filed into it is filed under, and a task's filed
	// unit is never rewritten — so writing the NAME here filed every item
	// in the company under a spelling that moves the day somebody corrects
	// the team's name, which is exactly what `id:` exists to prevent. The name is
	// the project's own display name beside it, and a reader resolves the
	// key back to the team's current name through the chart.
	for unit := range o.AllUnits() {
		add(unit.Project, unit.Name, unit.Purpose, unit.Key())
	}
	// A SEAT'S HOME UNIT THROUGH ONE INDEX of the chart ([org.Index.UnitFor]),
	// built at the first seat that names a project of its own — most name
	// none, and their home is never asked — because the chart's own
	// [org.Organization.UnitFor] walks every unit's seats until one holds
	// the seat, which asked of every seat on every apply cost seats².
	var homes *org.Index
	for role := range o.AllRoles() {
		// EVERY seat, not just the root-level ones: `Organization.Roles`
		// holds only the seats declared outside a unit, and a seat
		// nested in one that names its own project would otherwise get
		// no project at all — and file nothing, for ever.
		//
		// A ROLE'S OWN PROJECT takes the ROLE's name, because that is
		// what a founder naming one on a seat meant — a project for that
		// seat's work rather than for its unit's — and its home unit,
		// so the project still says where in the company it sits.
		if org.NormalizeScope(role.Project) == "" {
			continue
		}
		if homes == nil {
			homes = o.Index()
		}
		var home string
		if unit := homes.UnitFor(role); unit != nil {
			home = unit.Key()
		}
		add(role.Project, role.Name, "", home)
	}
	return out
}

// projectLeads maps a tracker project to the handle that owns it.
//
// THROUGH [org.Organization.LeadsBy], which is where "who owns this scope"
// lives: the tracker's only contribution is naming the field. The ambiguity
// report is logged rather than refused, because two units sharing a project
// is an ordinary arrangement and only a DISAGREEMENT about the lead matters.
func projectLeads(o *org.Organization) map[string]string {
	if o == nil {
		return nil
	}
	leads, report := o.LeadsBy(org.Scope{
		OfUnit: func(u *org.Unit) string { return u.Project },
		OfRole: func(r *org.Role) string { return r.Project },
	})
	for _, unled := range report.Unled {
		// LOUD, because the consequence is invisible: every item in that
		// project naming nobody routes to nobody, which looks exactly like
		// an item nobody filed.
		log.Warn("work_project_has_no_lead", "unit", unled.Unit, "project", unled.Scope)
	}
	for _, conflict := range report.Ambiguous {
		log.Warn("work_project_lead_ambiguous", "project", conflict.Scope,
			"declared_by", conflict.DeclaredBy, "chose", conflict.Chose,
			"candidates", conflict.Candidates)
	}
	return leads
}

// containerLeads maps a knowledge container to the handle that owns it.
func containerLeads(o *org.Organization) pages.Leads {
	if o == nil {
		return nil
	}
	leads, report := o.LeadsBy(org.Scope{
		OfUnit: func(u *org.Unit) string { return u.Space },
		OfRole: func(r *org.Role) string { return r.Space },
	})
	for _, unled := range report.Unled {
		log.Warn("pages_container_has_no_lead", "unit", unled.Unit, "container", unled.Scope)
	}
	for _, conflict := range report.Ambiguous {
		log.Warn("pages_container_lead_ambiguous", "container", conflict.Scope,
			"declared_by", conflict.DeclaredBy, "chose", conflict.Chose,
			"candidates", conflict.Candidates)
	}
	return pages.Leads(leads)
}

// ProjectOfSeat is one seat's home project, from a chart.
//
// EXPORTED because two surfaces need the same answer and a second walk is how
// one stops matching the other: the seat tools resolve it to default a create's
// container, and the API resolves it to scope `preset=my_queue`'s second arm —
// and a queue scoped to a different project from the one a create files into
// is the shape a person cannot diagnose from either screen.
func ProjectOfSeat(o *org.Organization, handle string) string {
	return scopeOfSeat(o, handle,
		func(u *org.Unit) string { return u.Project },
		func(r *org.Role) string { return r.Project })
}

// scopeOfSeat is the project or container a seat files into: its own, else
// its unit's, else its nearest ancestor's.
//
// THE WALK IS UPWARD, so a seat in a team with no space of its own writes in
// its department's rather than being told to name one. A seat with none
// anywhere returns empty, and the tool refuses rather than guessing — which
// is right: a page filed into a container nobody chose is one nobody finds.
func scopeOfSeat(o *org.Organization, handle string, of func(*org.Unit) string,
	own func(*org.Role) string) string {
	if o == nil || handle == "" {
		return ""
	}
	for role := range o.AllRoles() {
		if role.Handle() != handle {
			continue
		}
		if scope := strings.TrimSpace(own(role)); scope != "" {
			return scope
		}
		break
	}
	for unit := range o.AllUnits() {
		for _, role := range unit.Roles {
			if role.Handle() != handle {
				continue
			}
			if scope := ancestorScope(o, unit, of); scope != "" {
				return scope
			}
			return ""
		}
	}
	return ""
}

// ancestorScope walks a unit and its ancestors for the first scope declared.
func ancestorScope(o *org.Organization, unit *org.Unit, of func(*org.Unit) string) string {
	for u := unit; u != nil; u = parentOf(o, u) {
		if scope := strings.TrimSpace(of(u)); scope != "" {
			return scope
		}
	}
	return ""
}

// parentOf finds a unit's parent, or nil at the top.
func parentOf(o *org.Organization, child *org.Unit) *org.Unit {
	for unit := range o.AllUnits() {
		for _, candidate := range unit.Children {
			if candidate == child {
				return unit
			}
		}
	}
	return nil
}

// skillDetector answers whether a page body is a tool skill, for the
// projection's derived flag.
type skillDetector struct{}

// IsSkill reports a body that parses as a tool skill.
//
// THROUGH THE SKILLS PACKAGE'S OWN ADMISSION TEST, so the projected flag
// means exactly what the registry means by it: a page the sync would admit.
// A second heuristic here would let one page be a skill to the projection
// and prose to the loader — and the disagreement is silent, because each
// half is self-consistent.
func (skillDetector) IsSkill(body string) bool { return skills.IsSkill(body) }

// ---- what a seat's tools are given ------------------------------------- //

// workDeps is the tracker half of a seat's builtin surface, per epoch.
//
// The READER AND WRITER are this node's, and do not change with a revision;
// the DEFAULT PROJECT is the org chart's, and does. Both halves nil omits
// every tracker tool, which is what a company on Jira has — and omitting them is
// the point: a seat offered a tool against a tracker its company does not
// run would reach for it and fail at the call.
func (e *Engine) workDeps(c *Company) builtin.WorkDeps {
	halves, ok := e.trackerHalves()
	if !ok {
		return builtin.WorkDeps{}
	}
	return builtin.WorkDeps{
		Reader: halves.reader,
		// ONE WRITER PER ACTOR, derived from the turn's own seat: the
		// tracker's rule is that a writer acts as exactly one party, and
		// the party here is the immutable seat the tool surface bound
		// rather than anything a model can name. Its provenance carries
		// the turn's write log, so every task it commits to is one the
		// turn can be charged by (see [builtin.Actor.Provenance]).
		Writer: func(actor builtin.Actor) builtin.WorkWriter {
			return halves.as(actor)
		},
		// AND THE PROJECT SETTINGS, which every surface has rather than
		// the operator's alone: declaring a tag is open to every seat by
		// design, and a create refuses a label the project has not
		// declared — so a seat without this writer could never use the
		// `labels` argument on the tools it already holds. The authority
		// for every other facet is resolved per call.
		ProjectWriter: func(actor builtin.Actor) builtin.ProjectWriter {
			return halves.as(actor)
		},
		// AND THE DEPENDENCY SEQUENCE, which is the same writer in its
		// third shape: a dependency is two commits on two subjects, so
		// it needs the replicated estate to check its counterparties
		// before the first of them — and this writer has one.
		Dependencies: func(actor builtin.Actor) builtin.WorkDepender {
			return halves.as(actor)
		},
		Merges: func(actor builtin.Actor) builtin.WorkMerger {
			return halves.as(actor)
		},
		// AND THE CROSS-PROJECT MOVE, the same writer in the shape that
		// re-keys an item and its subtree into another project.
		Moves: func(actor builtin.Actor) builtin.WorkMover {
			return halves.as(actor)
		},
		// AND THE PROJECT'S FILES: the rows through the same halves, the
		// bytes through this node's own object client — which on a node
		// holding no data still writes to and reads from the data nodes
		// directly, never through the estate service.
		Files: halves.files,
		FileWriter: func(actor builtin.Actor) builtin.FileWriter {
			return halves.as(actor)
		},
		Objects: e.ObjectStore(),
		// THE RANKED SEARCH, which reads and therefore takes no actor:
		// the corpus is the same for everybody and there is nothing to
		// attribute. Nil where this node has no index, and the tool is
		// then not advertised at all.
		Search:   WorkSearcher(e),
		Mentions: seatMentions{org: c.Org},
		// THE ROSTER, read PER CALL for the reason the default project
		// and the unit seam are: a seat's tools are cloned into its
		// lease, an apply does not rebuild the clone, and a captured
		// chart would refuse a colleague who joined this morning and
		// admit one who left.
		Seats: func() []colleague.Seat {
			// AND WHAT THE PARTY REGISTRY WITHHOLDS NOW: a person the
			// directory has suspended is left out of the roster exactly as
			// an inbound message from them is left unattributed. Their
			// contact ids resolve through this node's chain, the one
			// ${VAR} lookup a chart still needs.
			return builtin.Corpus(e.Company().Org, e.LookupSecret, e.WithheldContacts())
		},
		// AND THE UNIT SEAM, read per call for the reason the default
		// project is: a seat's tools are cloned into its lease, an apply
		// does not rebuild the clone, and a captured chart would render
		// a project's unit against an org that has since moved.
		Units: liveUnits{engine: e},
		// PER CALL against the epoch current when the tool runs, not
		// against the one that equipped it: a seat's tools are cloned
		// into its lease and an apply does not rebuild the clone, so a
		// captured project would outlive the org chart that named it.
		DefaultProject: func(handle string) string {
			return ProjectOfSeat(e.Company().Org, handle)
		},
		// THE LEAD MAP IS READ PER CALL against the epoch current when the
		// tool runs, for the reason the default project is: a seat's
		// tools are cloned into its lease, an apply does not rebuild the
		// clone, and a captured map would route a change by the org chart
		// that has since moved.
		Leads: liveLeads{engine: e},
		// AND WHERE AN ASKER MAY PROMISE TO REPORT A DECISION, read per
		// call for the chart seams' reason: a captured chart would admit
		// a channel a unit stopped declaring and a surface the company
		// turned off.
		Channels: liveChannels{engine: e},
		Now:      func() time.Time { return time.Now().UTC() },
		// THE COMPANY'S CLOCK AS THIS EPOCH SETS IT (ADR-0018), and
		// deliberately not read per call like the chart seams above: a
		// turn pins its epoch, and a turn that resolved "due friday" on
		// one clock and cut its overdue marks on the next would be
		// running a company that never existed.
		Zone:  c.Config.Location,
		Await: halves.await,
	}
}

// trackerWriter is everything a seat's tools write to the tracker through,
// as one party.
type trackerWriter interface {
	builtin.WorkWriter
	builtin.ProjectWriter
	builtin.WorkDepender
	builtin.WorkMerger
	builtin.WorkMover
	builtin.FileWriter

	// RecordTurn is the one write the ENGINE makes as a seat rather than a
	// tool: a turn's spend onto the task it was spent on. On both halves
	// for the reason every tool's write is — see [Engine.recordTurnSpend].
	RecordTurn(ctx context.Context, opID string, turn tracker.TurnRecord) (tracker.WriteResult, error)
}

// trackerSeams are the tracker halves a seat's tools are handed: this node's
// own reader and writer where it holds data, a data node's otherwise.
type trackerSeams struct {
	reader builtin.WorkReader
	files  builtin.FileReader
	as     func(builtin.Actor) trackerWriter
	await  func(ctx context.Context, at statelog.Position) error
}

// trackerHalves is this node's tracker seams, or false where it runs no
// native tracker.
//
// THE ROUTER'S, ON EVERY NODE, so every tool is handed the same kind of half:
// a data node's router answers from its own copy and a stateless node's asks a
// data node, and a read after a write waits for the write whichever answers it
// — the node's floors ([estate.Session]), which the write raises and every
// holder, this node included, waits for before it reads. A tool whose reader
// was local and whose writer was remote would read its own write back from a
// copy that had not seen it; one whose every call goes through the router
// cannot.
//
// A node that hands out no tracker writer ([Engine.writers]) — any node in a
// maintenance mode, which may publish nothing — hands out no tracker tools
// either, rather than tools whose every write is refused.
func (e *Engine) trackerHalves() (trackerSeams, bool) {
	if writes, _ := e.writers(e.nativeHalves()); !writes {
		return trackerSeams{}, false
	}
	r := e.router
	return trackerSeams{
		reader: r.Work(),
		files:  r.Work(),
		as: func(actor builtin.Actor) trackerWriter {
			return r.WriterAs(remoteActor(actor))
		},
		// THE SESSION FLOOR IS THE WAIT: what the next read needs is that
		// whichever holder answers it has applied this far.
		await: r.Await,
	}, true
}

// ChartUnits is an org chart as the tracker's unit seam.
//
// THE ONE IMPLEMENTATION, here because this package is where a concrete thing
// is matched to a seam: the tracker holds no org — the applier may not read
// one, since two nodes briefly on different epochs would write different rows
// — so a stored unit is resolved at READ time, and every surface that renders,
// filters or writes one has to reach the same answer.
//
// THROUGH [org.Organization.UnitByRef], which is that answer: a stored unit
// carries whichever spelling of a unit was its address when the row was
// written — its key, or its name where the document gave it no key — and both
// resolve, however they are cased. Resolving through [org.Organization.Unit]
// instead matched EXACTLY, so `unit: engineering` on a company with a unit
// named "Engineering" was refused with "This company has no team".
func ChartUnits(o *org.Organization) tracker.Units { return chartUnits{org: o} }

type chartUnits struct{ org *org.Organization }

// ResolveUnit answers the unit a stored or typed reference names: its key, its
// name and its effective lead.
func (c chartUnits) ResolveUnit(ref string) (tracker.ChartUnit, bool) {
	if c.org == nil {
		return tracker.ChartUnit{}, false
	}
	unit := c.org.UnitByRef(ref)
	if unit == nil {
		return tracker.ChartUnit{}, false
	}
	lead := tracker.LeadRef{}
	// THE EFFECTIVE LEAD, which is the one inherited from an ancestor
	// where this unit declares none — because that is who actually hears
	// about the project's work, and rendering `none` beside a unit whose
	// parent has a lead sends a founder looking for a gap there is not.
	//
	// IT RESOLVES THE HANDLE `lead:` HOLDS, so the seat it answers is the
	// seat the engine runs. Deriving one from the lead's display name was a
	// SECOND derivation, and it disagreed with the first on every seat whose
	// operator declared a handle: a unit led by "Ada Okonkwo" with
	// `handle: ada` resolved to `ada-okonkwo`, which is nobody — so the lead
	// could not be looked up in the chart, filtered on, asked, or opened as
	// a person.
	if role := c.org.EffectiveLead(unit); role != nil {
		lead.Handle = role.Handle()
		lead.Kind = tracker.AuthorAgent
		if role.IsHuman() {
			lead.Kind = tracker.AuthorHuman
		}
	}
	// THE KEY AND THE NAME, because the callers ask in both directions:
	// what a write stores is the key ([org.Unit.Key]) and what a screen
	// reads is the name.
	return tracker.ChartUnit{Key: unit.Key(), Name: unit.Name, Lead: lead}, true
}

// liveUnits resolves against the epoch current when the tool RUNS.
type liveUnits struct{ engine *Engine }

func (l liveUnits) ResolveUnit(ref string) (tracker.ChartUnit, bool) {
	return ChartUnits(l.engine.Company().Org).ResolveUnit(ref)
}

// liveSeats resolves a people field's value to exactly one seat, against the
// CURRENT epoch, and answers its handle.
//
// EXACTLY ONE, and an ambiguous spelling is the same answer as an unknown one:
// both mean "this does not name a person", which is the only thing a stored
// value can be written from. A field holding a handle nobody has is a field
// every filter on it misses, silently, for as long as the value is there. A
// node holding no company names nobody, rather than dereferencing one.
//
// Per call for the reason every other live seam here is: the writer outlives a
// revision, and a captured chart would admit a colleague who has left.
type liveSeats struct{ engine *Engine }

func (l liveSeats) ResolveSeat(ref string) (string, bool) {
	company := l.engine.Company()
	if company == nil || company.Org == nil {
		return "", false
	}
	found := colleague.Resolve(ref, builtin.Corpus(company.Org,
		l.engine.LookupSecret, l.engine.WithheldContacts()))
	if len(found) != 1 {
		return "", false
	}
	return found[0].Seat.Handle, true
}

// liveChannels is the chart's chat surfaces and unit channels, against the
// CURRENT epoch — the [builtin.ChannelDirectory] a decision's inform is
// checked through.
type liveChannels struct{ engine *Engine }

var _ builtin.ChannelDirectory = liveChannels{}

// ChatSurfaces is the chat surfaces a seat can post on.
//
// BOTH HALVES, and each alone admits a promise nobody can keep: a company
// surface the seat holds no bot on is one whose tools the seat never gets,
// and a seat's bot on a surface the company turned off is a credential no
// transport or tool server is started for. Mattermost's company block is
// switched by `enabled`; Slack's is on by being declared — the rule
// [Engine.startNotifications] starts each transport by.
func (c liveChannels) ChatSurfaces(handle string) []tracker.InformSurface {
	company := c.engine.Company()
	if company == nil || company.Org == nil || company.Config == nil {
		return nil
	}
	seat := company.Org.SeatByHandle(handle)
	if seat == nil {
		return nil
	}
	var out []tracker.InformSurface
	if mm := company.Config.Integrations.Mattermost; mm != nil && mm.Enabled &&
		seat.Mattermost.BotToken != "" {
		out = append(out, tracker.InformMattermost)
	}
	if company.Config.Integrations.Slack != nil && seat.Slack.BotToken != "" {
		out = append(out, tracker.InformSlack)
	}
	return out
}

// UnitChannels is every channel a unit DECLARES, once each, in chart order.
//
// DECLARED rather than effective: an inherited channel is its ancestor's
// declaration, so the set is the same and the walk never lists one twice.
func (c liveChannels) UnitChannels() []string {
	company := c.engine.Company()
	if company == nil || company.Org == nil {
		return nil
	}
	var out []string
	for unit := range company.Org.AllUnits() {
		channel := strings.TrimSpace(unit.DeclaredChannel)
		if channel != "" && !slices.Contains(out, channel) {
			out = append(out, channel)
		}
	}
	return out
}

// liveLeads resolves a wake's two fallbacks against the CURRENT epoch.
type liveLeads struct{ engine *Engine }

// ProjectLead is who hears about unassigned work in a project.
func (l liveLeads) ProjectLead(project string) string {
	if project == "" {
		return ""
	}
	return projectLeads(l.engine.Company().Org)[strings.ToUpper(project)]
}

// UnitLead is who hears about work routed to a unit, keyed the way a row
// holds it — see [UnitLeadOf].
func (l liveLeads) UnitLead(unit string) string {
	return UnitLeadOf(l.engine.Company().Org, unit)
}

// UnitLeadOf is who hears about work routed to a unit, named by any spelling
// a row may hold for it.
//
// EVERY SPELLING, because a routing unit is a record of what was true when it
// was written: the unit's key, or its name on a unit the document gave no key.
// A walk that compared the stored value against each unit's `id` FIELD alone
// resolved a lead on no company that had not given its units ids: the fallback
// that reaches a unit's lead when a change named nobody else reached nobody,
// and looked exactly like a unit whose lead is unset.
//
// THROUGH [org.Organization.UnitByRef] rather than a walk of its own, which is
// what makes "every spelling" one rule rather than a claim each reader
// repeats: a private loop folded with [strings.EqualFold] while the chart
// claims a key under a different fold, so the two disagreed over characters
// that are real in a team name.
//
// A UNIT THAT EXISTS AND LEADS NOBODY answers empty, which is not the same as
// a unit nothing names — and the resolver draws that line once, for every
// caller.
func UnitLeadOf(o *org.Organization, unit string) string {
	if o == nil {
		return ""
	}
	found := o.UnitByRef(unit)
	if found == nil {
		return ""
	}
	if lead := o.EffectiveLead(found); lead != nil {
		return lead.Handle()
	}
	return ""
}

// pageDeps is the knowledge half, on the same terms.
func (e *Engine) pageDeps(c *Company) builtin.PageDeps {
	reader, writer, await, ok := e.pageHalves()
	if !ok {
		return builtin.PageDeps{}
	}
	return builtin.PageDeps{
		Reader:           reader,
		Writer:           writer,
		Mentions:         seatMentions{org: c.Org},
		DefaultContainer: LiveSeatSpace(e),
		// THE TOOL-SKILLS CONTAINER, read off the CURRENT epoch for the
		// reason the defaults are: a write into it is asked for the admin
		// a tool skill takes, and the store itself refuses a seat every
		// write there ([pages.Reserved]).
		SkillsContainer: e.skillsContainer,
		Await:           await,
	}
}

// LiveSkillsContainer is the tool-skills container off the CURRENT epoch, for
// the page tools a surface outside this package builds — the operator's
// assistant and the HTTP write surface — so a write into it is asked for the
// admin a tool skill takes wherever it arrives from. See
// [builtin.PageDeps.SkillPage].
func LiveSkillsContainer(e *Engine) func() string { return e.skillsContainer }

// pageHalves is this node's knowledge-base seams, on the terms
// [Engine.trackerHalves] gives: the router's on every node.
func (e *Engine) pageHalves() (builtin.PageReader, builtin.PageWriter,
	func(context.Context, statelog.Position) error, bool) {
	if _, writes := e.writers(e.nativeHalves()); !writes {
		return nil, nil, nil, false
	}
	return e.router.Pages(), e.router.Pages(), e.router.Await, true
}

// writers is which of the halves one reading of this node's runtime found
// (run) it hands a WRITER for — the tracker's and the knowledge base's. THE
// ONE GATE every surface that writes the company's records through this node's
// router is handed its writer behind: a seat's tools and the engine's write of
// a turn's spend ([Engine.trackerHalves], [Engine.pageHalves]), the operator's
// MCP and act transport, the purge and a project file's upload and removal
// ([OperatorWorkWriter], [OperatorPageWriter]).
//
// NONE IN A MODE THAT PUBLISHES NOTHING, WHATEVER THE NODE HOLDS. The mode is
// this node's promise that it starts nothing that writes
// ([statelog.MaintenanceMode.Publishes]), and a write it routes to a data node
// is one it starts. A node without `data` used to be exempt, on the reading
// that the data node's mode was that node's to answer for — but a capacity
// window puts every data node in the mode too, so the routed write was refused
// there, and only after the request had done its work: a file's upload
// streamed up to a gibibyte into the object store before its row was refused,
// as `unavailable`, which a client retries, and each attempt left an object the
// collector — which stands down in the mode — could not free until the window
// closed. And outside a window it was a node in the mode writing into a fleet
// that is not, which is the one thing its operator restarted it to rule out.
//
// AND ON A DATA NODE, ONLY THE HALVES ITS OWN WRITE SIDE HOLDS
// ([Engine.writeSide]): a half is counted by its reader ([Engine.nativeHalves]),
// and a writer is a fact of its own. A node without `data` holds no write side
// and writes every half it runs through a data node.
func (e *Engine) writers(run halves) (tracker, wiki bool) {
	if !e.mode.Publishes() {
		return false, false
	}
	if run.own == nil {
		return run.tracker, run.wiki
	}
	w := e.writeSide()
	return run.tracker && w.tracker != nil, run.wiki && w.pages != nil
}

// ---- what the operator's surfaces are given ------------------------------ //

// OperatorWork is the tracker's read side as the operator's surfaces reach it —
// the dashboard's and the REST routes' questions, and the operator's own MCP —
// or false where this company runs no native tracker here.
//
// THE ROUTER'S, ON EVERY NODE, exactly as a seat's tools are handed it
// ([Engine.trackerHalves]), and never this node's own copy read directly. Read
// directly, a data node whose tracker copy was out of service — wrong rather than
// behind, or its file shut — sent its own seats' calls to a peer's copy and
// went on answering its operator from the one it had stopped serving: a board,
// a person's inbox or a purge read off rows the node itself would not vouch
// for, or refused outright where the file was shut. Through the router the
// operator is answered as the seats are, from this node's copy while it serves
// and from another data node's while it does not.
func OperatorWork(e *Engine) (estate.Work, bool) {
	reads := OperatorNative(e)
	return reads.Work, reads.Tracker
}

// OperatorReads is every native read seam the operator's surfaces are handed —
// the tracker, the knowledge base and the ranked item search — as ONE READING
// of this node's runtime found them.
//
// ONE READING, because a surface that asks per request takes all three at once
// for one answer, and three separate accessors each loaded the runtime again:
// a node whose first apply published its runtime between two of them handed one
// answer a view with no tracker beside a knowledge base that had one — two
// moments in one answer. Taken here, the view describes the runtime as it was at
// one instant, at the cost of one atomic load on a node without `data` and two
// on a data node, and no lock, no I/O and no allocation.
type OperatorReads struct {
	// Work is the tracker's read side, the router's — the zero facade where
	// Tracker is not set, for [OperatorWork]'s rule.
	Work estate.Work
	// Pages is the knowledge base's read side, the router's, a page's
	// backlinks included — the zero facade where Wiki is not set, for
	// [OperatorPages]' rule.
	Pages estate.Pages
	// Tracker and Wiki say which halves this company runs natively here.
	Tracker, Wiki bool
	// Search says the ranked item search is served — through Work, which
	// is the seam it is. It needs the tracker AND this node's index, which
	// is why it is a half of its own: a runtime can hold the board with no
	// item searcher, and the index is built under either backend, so on a
	// company whose tracker is a vendor's it holds pages and no task.
	// [WorkSearcher]'s rule.
	Search bool
}

// OperatorNative is [OperatorReads] for e at this instant — the one reading
// [OperatorWork], [OperatorPages] and [WorkSearcher] are each a view of, so the
// rule for when a seam is handed out is stated once. Which halves the company
// runs is [Engine.nativeHalves]' answer rather than a derivation of its own, so
// the operator is handed a half exactly where seat admission waits on it.
func OperatorNative(e *Engine) OperatorReads {
	if e.router == nil {
		return OperatorReads{}
	}
	run := e.nativeHalves()
	out := OperatorReads{
		Tracker: run.tracker,
		Wiki:    run.wiki,
		// THE TRACKER'S SEARCH, so the tracker as well as the index: the
		// index is built under either backend, and on a company whose
		// tracker is a vendor's it holds no task to rank. A NODE WITHOUT
		// `data` holds no index of its own (`own` is nil) and asks a data
		// node for the ranked search as it asks for everything else, so it
		// serves one wherever it serves the board.
		Search: run.tracker && (run.own == nil || run.own.itemSearch != nil),
	}
	if out.Tracker {
		out.Work = e.router.Work()
	}
	if out.Wiki {
		out.Pages = e.router.Pages()
	}
	return out
}

// OperatorWorkWriter is the tracker's write side for those surfaces, acting as
// whichever party each call names — or false where this node hands out no
// tracker writer: a company on a vendor tracker, or any node in a maintenance
// mode, which may publish nothing, whether or not it holds data
// ([Engine.writers]).
func OperatorWorkWriter(e *Engine) (func(estate.Actor) estate.WorkWriter, bool) {
	if _, ok := e.trackerHalves(); !ok {
		return nil, false
	}
	return e.router.WriterAs, true
}

// OperatorPages is the knowledge base's read side as the operator's surfaces
// reach it, through the router for [OperatorWork]'s reason — or false where
// this company runs no native knowledge base here. A page's backlinks are read
// through it too ([estate.Pages.LinkedFrom]): they read the tracker's copy as
// well as the knowledge base's, and are answered only where both serve.
func OperatorPages(e *Engine) (estate.Pages, bool) {
	reads := OperatorNative(e)
	return reads.Pages, reads.Wiki
}

// OperatorPageWriter is the knowledge base's write side for those surfaces, or
// false where this node hands out none ([Engine.writers]).
func OperatorPageWriter(e *Engine) (estate.Pages, bool) {
	if _, _, _, ok := e.pageHalves(); !ok {
		return estate.Pages{}, false
	}
	return e.router.Pages(), true
}

// AwaitEstate waits until whichever holder answers this node's next read has
// applied at — the router's session floor, which a surface's write raises so
// its own next read sees it ([estate.Router.Await]) — the same wait a seat's
// tools are handed, and for the same reason: this node's own applier is not the
// one that answers once its copy of that domain is out of service.
func (e *Engine) AwaitEstate(ctx context.Context, at statelog.Position) error {
	if e.router == nil {
		return nil
	}
	return e.router.Await(ctx, at)
}

// reservedContainers are the containers a seat's own writes are held back
// from, each under its own rule, and every container the company DECLARES — the
// only ones a page is created in, by anybody — see [pages.Reserved].
//
// THE DECLARED SPACES ARE [chartContainers]'s, the very list the containers are
// made from on every apply: every unit's and seat's `space:`, the root and the
// tool skills, in one canonical form. A second walk of the chart here would be
// how a page could be refused a space whose container exists, or created in
// one that does not.
//
// NOTHING IS DECLARED by a node running no company, which is the meaningful
// zero: every create is refused until there is a chart to say where pages go.
func reservedContainers(current *Company) pages.Reserved {
	if current == nil || current.Config == nil {
		return pages.Reserved{}
	}
	held := pages.Reserved{
		Skills: strings.TrimSpace(current.Config.SkillsContainerKey()),
		Root:   strings.TrimSpace(current.Config.RootSpaceKey()),
	}
	for _, container := range chartContainers(current) {
		held.Declared = append(held.Declared, container.Key)
	}
	return held
}

// seatMentions resolves the handles a body names to the seats that exist.
//
// THE INTERSECTION IS THE WHOLE JOB. [notify.Mentions] is deliberately
// permissive — it yields `@here`, `@all` and every handle nobody has —
// because filtering there would make the grammar know the company. Here is
// where the company is known, so here is where a name that is not a seat is
// dropped: a comment naming an outsider must not produce a notification
// nobody can deliver.
type seatMentions struct{ org *org.Organization }

// LiveLeads and LiveUnits are the chart seams a surface outside this package
// needs, resolved PER CALL against the epoch current when the tool runs.
//
// EXPORTED BECAUSE THE OPERATOR SURFACE WENT WITHOUT THEM, and their absence
// was invisible rather than harmless: with no Leads an operator filing an
// unassigned task woke nobody at all — the lead fallback is precisely what
// catches a task naming nobody — and with no Units every project that surface
// listed rendered as belonging to no team.
func LiveLeads(e *Engine) tracker.Leads { return liveLeads{engine: e} }

// LiveUnits is the tracker's unit seam over the engine's current chart.
func LiveUnits(e *Engine) tracker.Units { return liveUnits{engine: e} }

// LiveChannels is the chart's chat surfaces and unit channels, read per call,
// for a surface built once that outlives every apply — the person-facing tool
// catalogue, whose `decision.inform` a nil directory would refuse while a
// seat's own was admitted.
func LiveChannels(e *Engine) builtin.ChannelDirectory { return liveChannels{engine: e} }

// LiveDefaultProject is [ProjectOfSeat] over the engine's CURRENT chart, read
// per call, for the surfaces an apply does not rebuild for a new chart — the
// operator surface, whose `create_work_item` files a PERSON's work, and the
// dashboard's `viewer`, which says where that work lands. "" before any epoch
// is running, and for a seat whose team and every team above it own no
// project: the tool then refuses rather than guessing.
//
// ONE DERIVATION FOR BOTH, because the viewer promising one project while the
// create files into another is the exact disagreement this seam exists to
// rule out. The operator surface used to be handed a stub answering "" for
// everybody, so a bound founder's create that named no project was refused
// while the viewer's doc — and every seat's own surface — said it defaulted.
func LiveDefaultProject(e *Engine) func(handle string) string {
	return func(handle string) string {
		c := e.Company()
		if c == nil {
			return ""
		}
		return ProjectOfSeat(c.Org, handle)
	}
}

// Zone is the company's clock (ADR-0018) as the CURRENT epoch sets it, or UTC
// before any epoch is running.
//
// For the surfaces that OUTLIVE an epoch — the scheduler's loop, built once,
// and the operator's surface, which no apply rebuilds for a new clock — so
// that an apply moving the clock moves them with it on their next read. A
// seat's own tools take the clock of the epoch their turn pinned instead; see
// [Engine.workDeps].
func (e *Engine) Zone() *time.Location {
	c := e.Company()
	if c == nil {
		return time.UTC
	}
	return c.Config.Location()
}

// LiveSeatSpace is the container a page lands in when its write names none: the
// space of the writing seat's own team, or of the seat itself, read off the
// chart CURRENT at the call.
//
// Exported because the OPERATOR surface needs the same rule, and there must
// not be a second copy of it: a person writes there AS their seat
// ([iam.ActorFor]), and without it their "Save as a page" from the dashboard
// named no container and was refused, while the same write from their own
// seat's turn landed in their team's space. Keyed on the actor's handle, as
// [LiveDefaultProject] is: a credential nobody is bound through writes under
// its login, which no seat matches, so its write names a container or is
// refused naming the argument rather than filed somewhere guessed for it.
func LiveSeatSpace(e *Engine) func(handle string) string {
	return func(handle string) string {
		c := e.Company()
		if c == nil || handle == "" {
			return ""
		}
		return scopeOfSeat(c.Org, handle,
			func(u *org.Unit) string { return u.Space },
			func(r *org.Role) string { return r.Space })
	}
}

// LiveMentions resolves @-mentions against the chart CURRENT when the comment
// is written, rather than the one that built the caller.
//
// The seat path captures its org deliberately — a seat's tools are cloned into
// its lease and rebuilt on an apply — but the operator surface is not rebuilt
// for a new chart, so it reads the engine per call. It is exported because the
// OPERATOR MCP needs the same rule and there must not be a second copy of it:
// built without one, that surface wrote comments whose @-mentions resolved to
// nothing and woke nobody, while its own tool description promised otherwise.
func LiveMentions(e *Engine) builtin.MentionResolver {
	return liveMentions{engine: e}
}

type liveMentions struct{ engine *Engine }

func (m liveMentions) Mentions(text string) []string {
	c := m.engine.Company()
	if c == nil {
		return nil
	}
	return seatMentions{org: c.Org}.Mentions(text)
}

// Mentions returns the handles this text addresses that are seats here.
func (m seatMentions) Mentions(text string) []string {
	if m.org == nil {
		return nil
	}
	var out []string
	for _, name := range notify.Mentions(text) {
		// A HUMAN SEAT COUNTS. `crewlet` is a transport like any other
		// on this backend — a person mentioned on a work item is
		// notified through whatever surface their contact declares —
		// so the lookup is over every seat, not only the agents.
		if m.org.SeatByHandle(name) != nil {
			out = append(out, name)
		}
	}
	return out
}

// nudgeSkills asks the skill sync to re-read the tool-skill container.
//
// # Why it is a nudge and not a read
//
// It is called from the pages applier's post-commit hook, which runs on the
// apply loop's own goroutine, so doing the read here would hold every subsequent
// change behind a page walk and a registry replace. So it is a request to the
// node's one sync loop, which coalesces however many arrive into one walk: the
// read is wholesale, and one re-read after N changes is the same answer as N
// of them.
//
// # And an announcement, for the nodes that apply nothing
//
// Every DATA node applies the page log itself, so every data node's own
// applier sees a skill page move and calls this. A node without `data` runs
// no applier, so it would hear of the move only on its periodic walk — which
// is why this ANNOUNCES rather than merely refreshes: the fleet nudge tells
// every such node to walk its container through a data node. Several data
// nodes announce one edit; a hearer coalesces them into one walk.
//
// NAMED AS THE NATIVE BACKEND'S, because the projection outlives an apply that
// moves the company's skills to Confluence, and the loop ignores a refresh
// from a backend its source is not on.
func (e *Engine) nudgeSkills() { e.skillSync.Announce(string(config.KnowledgeNative)) }

// walkRemoteSkills reads the tool-skill container through a data node, for a
// node that holds none.
//
// STALE, for the reason [Engine.walkNativeSkills] gives, and one hop further:
// the walk runs because a data node APPLIED the change and announced it, and
// the floor this node carries is its own writes' — so the answering node is
// at or past both.
func (e *Engine) walkRemoteSkills(ctx context.Context, container string) ([]skills.Page, error) {
	if e.remote.Load() == nil || e.router == nil {
		return nil, errors.New("engine: this node reaches no data node for its skills")
	}
	found, err := e.router.Pages().SkillPages(ctx, container,
		statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		return nil, err
	}
	out := make([]skills.Page, 0, len(found))
	for _, page := range found {
		out = append(out, skills.Page{
			ID: page.ID, Title: page.Title,
			Version: page.Version, Text: page.Body,
		})
	}
	return out, nil
}

// walkNativeSkills reads the tool-skill container out of this node's own page
// projection.
//
// # It waits for hydration first
//
// A walk over a container this node has not applied through is a PARTIAL set,
// and the registry replaces wholesale, so reading early would silently delete
// every skill the walk did not reach. The wait is cancelled with the walk: a
// source change or a stop ends it.
func (e *Engine) walkNativeSkills(ctx context.Context, container string) ([]skills.Page, error) {
	if !e.awaitHydration(ctx) {
		return nil, ctx.Err()
	}
	// STALE, and it is the strongest level this walk could HONESTLY name,
	// not a saving.
	//
	// What the walk must not do is rebuild the registry without the change
	// that asked for the rebuild. It cannot: [Engine.nudgeSkills] is called
	// from the applier's POST-COMMIT hook, so by the time the sync loop
	// runs, the record that woke it is already in this node's own committed
	// prefix, which is exactly what a stale read serves. The causality is
	// LOCAL, so no barrier and no high-water mark buys anything here.
	found, err := e.native.Load().pageReader.SkillPages(ctx, container,
		statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		return nil, err
	}
	out := make([]skills.Page, 0, len(found))
	for _, page := range found {
		out = append(out, skills.Page{
			ID: page.ID, Title: page.Title,
			Version: page.Version, Text: page.Body,
		})
	}
	return out, nil
}

// awaitHydration blocks until this node's own applied rows have caught up,
// reporting false if ctx ended first.
func (e *Engine) awaitHydration(ctx context.Context) bool {
	ticker := time.NewTicker(hydrationPoll)
	defer ticker.Stop()
	for !e.StateLogHydrated(ctx) {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
	return true
}

// hydrationPoll is how often the first skill read checks whether the
// projection has caught up.
//
// A quarter-second. The wait is bounded by a boot reconcile, which is
// hundreds of milliseconds on an ordinary company and minutes on a large one
// — so the cost of polling is a handful of atomic reads either way, and a
// condition variable here would be a second thing to keep correct for a wait
// that happens once per process.
const hydrationPoll = 250 * time.Millisecond

// searchPeers is the fleet half of the knowledge search's fan-out.
//
// NIL WHEN THERE IS NO QUEUE, which is a legal deployment rather than a
// degradation: an embedded engine with no broker holds the whole corpus and
// takes every bucket, exactly as a single node does.
func (e *Engine) searchPeers() search.Peers {
	if e.backends == nil || e.backends.Queue == nil {
		return nil
	}
	return search.Broker{Queue: e.backends.Queue}
}

// liveNodes answers which nodes are in the fleet NOW: every node a fan-out may
// hand a share of a question to — a search's bucket range, a history read.
//
// FROM THE LEASE VIEW rather than from the positions register, and the two
// differ in exactly the way that matters here: a position row is held by every
// node the trim has to wait for, INCLUDING one that has been gone for hours,
// while a lease expires. A dead node in the roster costs every fanned answer on
// this node a partial result for as long as its row survives — and a history
// read would name it as missing on every screen, for a node nobody can bring
// back.
//
// EVERY NODE, data or not: this is the fleet's SIZE, which the health envelope
// reports. A fan-out never reads it — a search's buckets and a history read go
// to the nodes that hold what they read, [Engine.dataRoster].
func (e *Engine) liveNodes(ctx context.Context) ([]string, error) {
	if e.backends == nil || e.backends.Coord == nil {
		return nil, nil
	}
	leases, err := e.backends.Coord.ListLive(ctx, coord.ClassNode)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(leases))
	for _, lease := range leases {
		if profile, ok := placement.FromLease(lease); ok {
			out = append(out, profile.ID)
		}
	}
	return out, nil
}

// LiveNodes is how many nodes hold a presence lease now — the fleet's size as
// every fan-out on this node sees it ([Engine.liveNodes]).
//
// AN ERROR IS A READ THAT DID NOT HAPPEN, never zero nodes: the health envelope
// leaves its count out on one, because "0 nodes" beside a page this node is
// serving is a claim nobody could act on, and the plane being unreachable is
// already the posture's to say.
//
// THE CALLER BOUNDS IT: this is a key-iterating scan the NATS client's per-API
// timeout does not cover, so it lasts as long as ctx does. The health envelope
// hands it [ProbeReadBudget].
func (e *Engine) LiveNodes(ctx context.Context) (int, error) {
	if e.backends == nil || e.backends.Coord == nil {
		return 0, errors.New("engine: no coordination backend to read the fleet's presence from")
	}
	nodes, err := e.liveNodes(ctx)
	if err != nil {
		return 0, err
	}
	return len(nodes), nil
}

// reportSearch counts what one answer covered, and what its scan cost on the
// path it was run for.
//
// THE ONLY THING THAT MAKES FOUR ALARMS ABLE TO FIRE. `search_slow`,
// `prefetch_slow`, `search_degraded` and `search_scoped` are each a property
// of the answers a node gave, and an alarm whose input nobody records is
// permanently silent — which looks exactly like a system with nothing wrong.
//
// THE PATH IS THE QUERY'S OWN ([search.FanQuery.Prefetch]), so a turn's
// knowledge prefetch and somebody's deliberate search are two series held to
// two budgets. Every scan was recorded `interactive` once, the prefetch series
// had no producer, and `prefetch_slow` read a percentile of nothing.
func (e *Engine) reportSearch(q search.FanQuery, answer search.Answer, took time.Duration) {
	if e.metrics == nil {
		return
	}
	e.metrics.Observe(metrics.TrackerSearchScanDuration, took,
		metrics.Attrs{"path": string(scanPathOf(q)), "rung": searchRung(answer)})
	coverage := "complete"
	if answer.Partial() {
		coverage = "scoped"
	}
	e.metrics.Add(metrics.TrackerSearchAnswers, 1,
		metrics.Attrs{"coverage": coverage, "semantic": semanticState(answer)})
}

// scanPath is what a search was run for, as the scan-duration histogram's
// `path` attribute names it — and the one name the alarm reading splits the
// histogram by ([retention.observed]), so the label and the budget it is held
// to are one value rather than two strings that must agree.
type scanPath string

const (
	// scanInteractive is somebody's deliberate search — a seat's tool, a
	// person's — held to statelog.InteractiveSearchTarget (`search_slow`).
	scanInteractive scanPath = "interactive"
	// scanPrefetch is a turn's knowledge prefetch, which the turn waits on
	// before its first token, held to statelog.PrefetchScanBudget
	// (`prefetch_slow`).
	scanPrefetch scanPath = "prefetch"
)

// scanPathOf is the path q was run for.
func scanPathOf(q search.FanQuery) scanPath {
	if q.Prefetch {
		return scanPrefetch
	}
	return scanInteractive
}

// searchRung is the ranking a scan actually ran, for the duration histogram.
//
// THE SERVED MODE, never a constant: every scan used to be recorded as
// `hybrid` whatever it ran, so the histogram the interactive target is read
// from mixed keyword-only scans — a fraction of the cost — into the figure for
// the fused ones, and a slow semantic half hid under a fast lexical majority.
func searchRung(answer search.Answer) string {
	if answer.Served == "" {
		return "none"
	}
	return string(answer.Served)
}

// semanticState is whether the meaning half ran, as the degraded-search alarm
// counts it.
//
// THREE VALUES, because two of them are the alarm and one must never be. The
// half that was ASKED FOR and did not run — a participant's vector scan that
// failed, or a query vector the provider could not compute — is `skipped`,
// the numerator of `search_degraded`. A half nobody asked for — a keyword
// search, or a company with no embeddings provider — is `off`: counting that
// as skipped fires the alarm on every search such a company ever runs, which
// is an alarm red for the life of the deployment.
func semanticState(answer search.Answer) string {
	switch {
	case answer.SemanticSkipped,
		answer.Degraded == knowledge.DegradedEmbeddingFailed,
		answer.Degraded == knowledge.DegradedSemanticPartial:
		return "skipped"
	case !answer.Served.Semantic() || answer.Served == "":
		return "off"
	}
	return "full"
}

// enterSearch counts one scan in, and its return counts it out.
//
// THE LEVEL NOTHING ELSE CAN SAMPLE. The supported-corpus table has a row per
// concurrency — through the full scan ≈ 345 000 sources inside the budget with
// one reader and ≈ 136 000 with eight, through an index at its probe ceiling
// ≈ 545 000 and ≈ 183 000 (see [search.SemanticScanBudget]) — and a node
// answering nine at once is past the last of them. A duration histogram
// cannot say which row: it records what the scans cost without recording how
// many were competing for the disk while they did.
//
// SET RATHER THAN ADDED, on both edges, because it is a gauge: the value is
// the count in flight at the moment a collector reads it, and the rolling
// window's peak over a day is the busiest this node got.
func (e *Engine) enterSearch() func() {
	if e.metrics == nil {
		return func() {}
	}
	e.metrics.Set(metrics.TrackerSearchConcurrency,
		float64(e.searching.Add(1)), nil)
	return func() {
		e.metrics.Set(metrics.TrackerSearchConcurrency,
			float64(e.searching.Add(-1)), nil)
	}
}
