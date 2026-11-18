package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crewlet/crewlet/internal/accessreview"
	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/colleague"
	"github.com/crewlet/crewlet/internal/agent/skills"
	"github.com/crewlet/crewlet/internal/alarmwake"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/atlaswake"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/authz/orgchart"
	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/decision"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/checkrun"
	gitletcompact "github.com/crewlet/crewlet/internal/gitlet/compact"
	gitlethome "github.com/crewlet/crewlet/internal/gitlet/home"
	"github.com/crewlet/crewlet/internal/gitlet/host"
	"github.com/crewlet/crewlet/internal/gitlet/mirror"
	"github.com/crewlet/crewlet/internal/gitlet/query"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/objstore/collect"
	"github.com/crewlet/crewlet/internal/objstore/references"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/worklet"
)

// The native backends, wired: the half of this node's durable state only a
// company can say anything about. The core is the other half (core.go; package
// doc, "The core runtime"); a node without `data` runs neither and reaches both
// through a data node (remote.go).
//
// The apply loops, the indexer and the change feeds are per-NODE: they follow a
// LOG, and rebuilding them on an apply would restart every applier and re-run a
// boot reconcile on every config edit. The parsers, the prompts and the tool
// wiring are per-EPOCH, because they read the org chart.

// Seat acquisition waits on ESTABLISHMENT ([Engine.StateLogHydrated]): a seat
// whose tracker is not established would act on "there is no such task" by
// filing a duplicate or abandoning work. /ready deliberately does not wait:
// lag alone never sheds a node. The gate is per DOMAIN
// ([statelog.Domain.GatesSeatAdmission]), since a compacted domain's gap is a
// coverage number, not a fault; the copy verdict is per domain for every
// domain (ADR-0079).

// native holds this node's native-backend runtime: the tracker's, the
// knowledge base's and chat's read and write sides over the core's logs,
// Gitlet's write side and git surface over the code log, the lexical index
// and chat's own, and the change feeds.
//
// EVERY FIELD IS WRITTEN ONCE, by [Engine.startNative], before the struct is
// published to [Engine.native], so no mutex; the wait group carries its own
// synchronisation. IT HOLDS NO LOG: the logs are the core's; only which
// WRITERS and READERS exist over them is the company's to say. A node
// without `data` runs [remoteNative] instead.
type native struct {
	// built is the native-backend choices of the company that started this
	// runtime, which it serves until the process restarts; an activation that
	// says otherwise is told so ([Engine.sayBackendSwitches]).
	built builtWith

	// indexer keeps the lexical search index behind this node's applied
	// pages and tasks.
	indexer *search.Indexer

	// writer is the tracker's write authority, pages the wiki's and chat
	// the company's own rooms'.
	writer *worklet.Writer
	pages  *doclet.Store
	chat   *chatlet.Store

	// trackerReader, pageReader and chatReader are the tool surfaces' read
	// paths, each nil where the company keeps that half elsewhere, which is
	// what every question about the TOOLS reads ([Engine.nativeHalves]).
	trackerReader *worklet.Reader
	pageReader    *doclet.Reader
	chatReader    *chatlet.Reader

	// trackerFiles reads the tracker's rows that name objects on every
	// company, whatever `tracker.backend` says: the log is the core's, so a
	// company that moved off Worklet still holds file rows the object store's
	// passes must read ([native.objectEstates]). It is trackerReader where the
	// company runs Worklet, never a second reader. Nil only on a zero runtime.
	trackerFiles *worklet.Reader

	// codeReader is the code log's rows as every reader reads them, set on
	// every published runtime: the code log runs in every register, so its
	// pack rows are the object store's to keep from the first push.
	codeReader *gitlet.Reader

	// codeStore and committer are Gitlet's WRITE side (the lifecycle store the
	// repositories' reconcile and retention write through, and the committer a
	// push is decided by), built only where the company set `code.backend:
	// gitlet` ([builtWith.runsCode]), and handed out only through
	// [Engine.writeSide], so a node in a maintenance mode decides nothing. The
	// committer is also READ by each alarm evaluation, field by field in
	// [Engine.codeReading], where the producers gate finds each field written.
	// changeWriter, checksWriter and marks are its siblings, built and handed out
	// with it: the writers of changes' conversations, repositories' checks and
	// persons' code inbox marks, each on its own subject.
	codeStore    *gitlet.Store
	committer    *gitlet.Committer
	changeWriter *gitlet.ChangeWriter
	checksWriter *gitlet.ChecksWriter
	marks        *gitlet.Marks

	// gitletHost is this node's git surface (internal/gitlet/host): its cache,
	// git pool, hook relay and engine writes, built only where the company set
	// `code.backend: gitlet` AND this node has the gitlet role
	// ([Engine.hostsGit]). Read per request through [Engine.GitletHost] and, as
	// the writing half of the git half, [Engine.localGit] ([gitHalf]); never
	// captured.
	gitletHost *host.Server

	// gitReads is this node's server-side reads of a repository by object id
	// (internal/gitlet/query), built beside gitletHost over the same cache and
	// pool, nil where it is: the reading half of this node's git half
	// ([gitHalf]) as a repository's home, reached per request through
	// [Engine.localGit]. It holds no goroutine; Gitlet's stop stops it first,
	// before the cache ([native.stopGitletHost]).
	gitReads *query.Server

	// mergePreviews is this node's merge previews (internal/gitlet/home's
	// Previews), built beside gitReads over the same cache, pool and share, and
	// nil where it is: the third part of its git half ([gitHalf]), answered by
	// the merge a repository's home records mergeability by. It holds no
	// goroutine; Gitlet's stop stops it with gitReads, before the cache.
	mergePreviews *gitlethome.Previews

	// gitCache is the cache gitletHost serves from, built and stopped with it
	// and nil where it is: held only to be READ by each alarm evaluation
	// ([Engine.codeReading]); no route reaches it.
	gitCache *cache.Cache

	// compactor merges the packs of the repositories this node maintains and
	// publishes their layers (internal/gitlet/compact), built with gitletHost
	// and nil where it is, started by [Engine.startNativeDuties], stopped in
	// Gitlet's one bounded stop. A CONCRETE type, never an interface here,
	// because the alarm reading's producers gate follows a call only through
	// one ([Engine.codeReading]).
	compactor *gitletcompact.Compactor

	// home is the change work of the repositories this node is home to
	// (internal/gitlet/home), built and started with gitletHost and the
	// compactor, nil where they are; stopped in Gitlet's one bounded stop,
	// before the compactor.
	home *gitlethome.Loop

	// mirrors is the imports and pull mirrors of the repositories this node
	// is home to (internal/gitlet/mirror), a worker of home built with it and
	// nil where it is; stopped after home, so no turn starts an attempt it
	// would not wait for.
	mirrors *mirror.Mirrors

	// checkRunner claims and runs the company's queued checks in boxes of
	// this node's sandbox catalogue (internal/gitlet/checkrun), built with
	// gitletHost and started with home, and nil where they are or where this
	// node sets no CREWLET_BOX_GIT_URL; stopped FIRST in Gitlet's one
	// bounded stop. A concrete type, as the compactor is.
	checkRunner *checkrun.Runner

	// gitCacheSize is how this node sized gitCache, which [Engine.warnTiers]
	// compares with each applied epoch's repository limit. Zero with no host.
	gitCacheSize codeCacheSize

	// gitletHostStop is the one bounded stop the teardown and the shutdown
	// share ([native.stopGitletHost]).
	gitletHostStop boundedStop

	// withdrawCodeStatus withdraws this node as an answerer of Gitlet's status
	// (codestatus.go), registered beside gitletHost and nil where it is;
	// Gitlet's stop runs it first.
	withdrawCodeStatus queue.Unsubscribe
	// withdrawCodeSearch withdraws this node as an answerer of the code
	// search scatter (codesearch.go), beside withdrawCodeStatus.
	withdrawCodeSearch queue.Unsubscribe

	// chatIndex is the chat corpus's own keyword index, separate from
	// [native.indexer] for ADR-0071's reason: chat is in neither half of the
	// knowledge corpus.
	chatIndex *search.ChatIndexer

	// changeIndex is this node's index of Gitlet changes' words (ADR-0156),
	// built where the company hosts code: first newest first, then walked forward.
	changeIndex *search.ChangeIndexer

	// searcher is Doclet's answer to the knowledge seam.
	searcher *doclet.Searcher

	// itemSearch is the tracker's own ranked search over the SAME index and
	// fan-out (worksearch.go). Its own field so a caller holding one verb
	// cannot reach the other's corpus.
	itemSearch *worklet.Searcher

	// stopSlices withdraws this node as an answerer for the fleet's search
	// fan-out. Nil with no queue to serve on (every embedded engine and test).
	stopSlices queue.Unsubscribe

	// feeds is the change feeds this node started, stored once at start and
	// read by every alarm evaluation ([native.feedsReading]). Nil before
	// then, and on a node in a mode that publishes nothing.
	feeds atomic.Pointer[[]*changefeed.Feed]

	// run is the context every goroutine this half started runs under, and
	// stop is what ends it.
	//
	// HELD, not re-derived: the feeds start later than the indexer, and a
	// goroutine registered on done but started under the CALLER's context
	// would never be ended by stop, which would then block for ever.
	run  context.Context
	stop context.CancelFunc
	done sync.WaitGroup
}

// errNoCore is what the native halves refuse to start without on a data node:
// every one of their writers and readers is over one of the core's logs.
var errNoCore = errors.New("engine: Worklet, Doclet and Chatlet run over the " +
	"core runtime's state log, and this engine has not started one")

// startNative opens this node's native backends, once per process, over the
// core runtime [Engine.startCore] already published.
//
// ONCE, by whichever meets the node's first company: [New], or the apply
// handing a node that booted without one its first ([Engine.startNativeFor]).
// The halves follow the fleet's logs, not a revision. It neither waits for
// hydration (seat acquisition does, [Engine.StateLogHydrated]) nor applies
// the chart ([Engine.followCompany] does, after both callers publish one).
//
// A NODE WITHOUT `data` builds none of it ([Engine.startRemote]). The store
// and the fleet are not nil-checked: [New] refuses a Backends without either.
func (e *Engine) startNative(ctx context.Context, boot *config.Bootstrap, c *Company) error {
	// A NODE WITHOUT `data` answers its seats' tools through a node that does.
	if !HoldsEstate(boot) {
		return e.startRemote(c)
	}
	core := e.core.Load()
	if core == nil || core.log == nil {
		return errNoCore
	}
	// WHICH HALVES THIS COMPANY RUNS, as ONE READING kept on the runtime: the
	// halves below derive from it, and every later activation is compared
	// against it ([Engine.sayBackendSwitches]). The logs behind every half are
	// the core's and run whatever this says.
	built := builtFrom(c.Config)
	runTracker, wiki, rooms := built.runsTracker(), built.runsWiki(), built.runsChat()
	nodeID, sl := core.nodeID, core.log

	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	n := &native{run: runCtx, stop: cancel, built: built}
	// ONE FAILURE PATH FOR EVERYTHING BELOW, armed before the first thing that
	// outlives this call, stood down once the half is the engine's. A bare
	// `cancel()` would not withdraw a slice answerer (the queue copies its
	// registration with [context.WithoutCancel]), and a retry would then run
	// two answerers.
	started := false
	defer func() {
		if !started {
			n.shutdown(ctx)
		}
	}()

	var err error
	// THE TRACKER'S FILE ROWS ARE READ ON EVERY COMPANY, since its log runs
	// whatever the tracker is: a company that once ran Worklet still holds rows
	// naming its project files, which the collector, its audit and `crewlet
	// objects migrate` must read. A node whose tracker log did not come up is
	// refused, or every object they name would read as nobody's.
	running := sl.Domain(worklet.Domain{}.Name())
	if running == nil {
		return fmt.Errorf("engine: this node runs no tracker domain, so " +
			"neither Worklet nor the object store's passes have its rows to " +
			"read; the domain is in the register and its stream failed to come up")
	}
	if n.trackerFiles, err = worklet.NewReader(
		sl.estate().Reader(), running.reader); err != nil {
		return fmt.Errorf("engine: the tracker's file rows: %w", err)
	}
	if runTracker {
		n.writer, err = worklet.NewWriter(worklet.WriterDeps{
			Publisher: running.publisher,
			DB:        sl.estate().Reader(),
			Claims:    e.backends.Coord,
			NodeID:    nodeID,
			// THE CHART, read PER CALL: a wake's fallback recipient is the project's
			// lead, which no row carries (the applier may not read an org), and the
			// caller is a fleet singleton with no tool arguments to carry a seam.
			Leads: liveLeads{engine: e},
			// THE CHART AGAIN, for a people custom field: the write resolves it
			// against the EPOCH's roster and the record carries the handle, so no
			// applier ever reads an org.
			World: liveSeats{engine: e},
			// THE COMPANY'S CLOCK, read per call: an instant given for a date is
			// stored as its day on this clock (ADR-0018), the epoch's to change.
			Zone: e.Zone,
			// THE NODE'S OWN WRITER ACTS AS THE SYSTEM; every surface derives its
			// own with Writer.As (a seat as that seat, a session as its
			// credential). Only the node's own housekeeping acts as the system, so
			// it is never mistaken for somebody's decision.
			Actor:     nodeID,
			ActorKind: worklet.AuthorSystem,
		})
		if err != nil {
			return fmt.Errorf("engine: Worklet writer: %w", err)
		}
		// THROUGH THE DOMAIN'S OWN READ AUTHORITY, so a level asked for is a
		// level served (refusal ladder, coverage probe, linearizable barrier);
		// the same reader the file rows are read through.
		n.trackerReader = n.trackerFiles
	}
	// THE CODE LOG'S ROWS, UNCONDITIONALLY: every data node runs the code log
	// in its core, and its pack bundles are kept, audited and backed up from
	// here.
	{
		running := sl.Domain(gitlet.Domain{}.Name())
		if running == nil {
			return fmt.Errorf("engine: this node runs no code domain, so its rows " +
				"have no reader — the domain is in the register and its stream " +
				"failed to come up")
		}
		// A typed nil would read as an index that is there, so words is set only where one is.
		var words gitlet.ChangeWords
		if built.runsCode() {
			n.changeIndex = search.NewChangeIndexer(e.backends.Store, e.domainEstate(), gitlet.Domain{})
			words = n.changeIndex
		}
		// THE CURRENT EPOCH'S LIMITS, read per call: a repository's detail reports
		// what a push is held to now, which an import chunks its pushes by.
		if n.codeReader, err = gitlet.NewReader(gitlet.ReaderOptions{Log: running.reader, Limits: e.codeLimits,
			Now:         e.now,
			Roster:      e.codeRoster,
			ChangeWords: words}); err != nil {
			return fmt.Errorf("engine: code reader: %w", err)
		}
		// GITLET'S WRITE SIDE, where the company hosts code: its committer and
		// lifecycle store exist only where a push may be decided.
		if built.runsCode() {
			if n.codeStore, err = gitlet.NewStore(gitlet.StoreOptions{
				Publisher: running.publisher, DB: sl.estate().Reader(), NodeID: nodeID,
				// A TOOL'S LIFECYCLE REQUEST is judged by the committer's own chart,
				// read per request off the current epoch, held to its code.scratch,
				// and decided nowhere on a node confirmed behind the configuration
				// (Reconciler.Admits through SetAdmits). A push mirror set as a
				// person's seat is held to the person using Gitlet (turnRequesters,
				// ADR-0110).
				SeatRules: e.codeSeatRules, Current: e.admits, Policy: e.codeLifecyclePolicy,
				People: turnRequesters{engine: e},
			}); err != nil {
				return fmt.Errorf("engine: code store: %w", err)
			}
			var publisher gitlet.Publisher = running.publisher
			if e.committerPublisher != nil {
				publisher = e.committerPublisher(publisher)
			}
			if n.committer, err = gitlet.NewCommitter(gitlet.CommitterOptions{
				Publisher: publisher, DB: sl.estate().Reader(), NodeID: nodeID,
				// THE DECIDING NODE'S EPOCH, read per decision, so a limit an
				// activation moves holds from then on, on every committer at once;
				// the chart and calendar a seat's push is judged by come from it too.
				Limits: e.codeLimits, Retention: e.codeDeletedRetention, SeatRules: e.codeSeatRules,
				// THE POSTURE GATE: a node confirmed behind the configuration
				// decides no seat's push (Reconciler.Admits through SetAdmits).
				Current: e.admits,
				// THE EVENT STREAM a merge or opened change it landed is announced
				// on, once, naming each seat by the agent id its day is counted under.
				Events: e.codeEvents(), AgentIDs: e.codeAgentID,
				Metrics: e.metrics, Logger: logging.Get("gitlet"),
			}); err != nil {
				return fmt.Errorf("engine: code committer: %w", err)
			}
			// ITS SIBLING, answering a change's conversation: the same chart,
			// posture gate, retention and event stream. A person an ask names is
			// asked only where they use Gitlet (turnRequesters, ADR-0109,
			// ADR-0110).
			if n.changeWriter, err = gitlet.NewChangeWriter(gitlet.ChangeWriterOptions{
				Publisher: running.publisher, DB: sl.estate().Reader(), NodeID: nodeID,
				Retention: e.codeDeletedRetention, SeatRules: e.codeSeatRules, Current: e.admits,
				Events: e.codeEvents(), AgentIDs: e.codeAgentID, People: turnRequesters{engine: e},
				Metrics: e.metrics, Logger: logging.Get("gitlet"),
			}); err != nil {
				return fmt.Errorf("engine: code change writer: %w", err)
			}
			// THE REPOSITORIES' CHECKS WRITER: the same retention, and the event
			// stream an applied ending is announced on, once, from here.
			if n.checksWriter, err = gitlet.NewChecksWriter(gitlet.ChecksWriterOptions{
				Publisher: running.publisher, DB: sl.estate().Reader(), NodeID: nodeID,
				Retention: e.codeDeletedRetention, SeatRules: e.codeSeatRules, Current: e.admits,
				Events: e.codeEvents(), AgentIDs: e.codeAgentID, Metrics: e.metrics, Logger: logging.Get("gitlet"),
			}); err != nil {
				return fmt.Errorf("engine: code checks writer: %w", err)
			}
			// THE PERSONS' CODE INBOX MARKS WRITER: a mark is the person's own,
			// refused for anybody else, so it needs no chart and no posture gate.
			if n.marks, err = gitlet.NewMarks(gitlet.MarksOptions{
				Publisher: running.publisher, DB: sl.estate().Reader(), NodeID: nodeID,
			}); err != nil {
				return fmt.Errorf("engine: code marks writer: %w", err)
			}
		}
		// ITS GIT SURFACE, on a node with the gitlet role: the cache serving
		// persons' clones, fetches and pushes, reaching rows and committers
		// through the estate's router. Inside this function's failure path,
		// whose shutdown stops it first.
		if e.hostsGit(built) {
			var parts gitletHostParts
			if parts, err = e.buildGitletHost(ctx, nodeID, boot, running); err != nil {
				return err
			}
			n.gitletHost, n.gitReads, n.gitCache, n.gitCacheSize = parts.host, parts.reads, parts.cache,
				parts.size
			n.mergePreviews = parts.previews
			n.compactor, n.home, n.mirrors, n.checkRunner = parts.compactor, parts.home, parts.mirrors, parts.checks
			n.withdrawCodeStatus, n.withdrawCodeSearch = parts.withdrawStatus, parts.withdrawSearch
		}
	}
	// THE LEXICAL INDEX COVERS BOTH CORPORA, so it is built under EITHER
	// backend, BEFORE the wiki block whose searcher takes it: gated on the wiki,
	// a Worklet-and-Confluence company indexed none of its work items while still
	// paying the embedding duty for them.
	//
	// It derives the backlinks from the same bodies, through the knowledge
	// base's one grammar ([search.Backlinks]).
	n.indexer = search.NewIndexerOver(e.backends.Store, e.domainEstate(),
		lexicalSources(runTracker, wiki)).WithLinks(doclet.Links)
	// ONE QUERY-VECTOR CACHE FOR BOTH SEARCHES, so a phrase embedded for one
	// is a hit for the other. Read through [Engine.queryModel] per query: an
	// apply can replace the provider or turn `knowledge.vectors` off.
	vectors := search.NewQueryVectors(e.queryModel)
	// EVERY SCAN ASKS ITS COPIES' VERDICT FIRST, this node's range and every
	// peer's slice alike (ADR-0079): a range over a copy out of service is
	// counted missing, never answered from it.
	copies := e.copies()
	// THE TRACKER'S OWN SEARCH, with its own fan-out: each verb's corpus
	// filter is its own, and neither can widen into the other's.
	//
	// ONLY WHERE THE COMPANY KEEPS ITS TRACKER HERE, unlike the index: on a
	// vendor tracker the corpus is empty by construction, and a searcher would
	// read "there is no tracker here" as "nothing matching", the confusion
	// [Engine.WorkSearch]'s untyped nil exists to prevent.
	//
	// THE BUCKETS ARE DIVIDED AMONG THE DATA NODES ([Engine.dataRoster]): only
	// a node holding the estate holds an index to scan.
	if runTracker {
		n.itemSearch = worklet.NewSearcher(e.domainEstate(), itemRanker{
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
	// THIS NODE ANSWERS FOR ITS PEERS, separately from the coordinator: every
	// node with an index answers, whether or not anybody on it searches.
	if e.backends.Queue != nil {
		n.stopSlices, err = search.ServeSlices(runCtx, e.backends.Queue, nodeID,
			search.NodeScanner{Index: n.indexer, Copies: copies})
		if err != nil {
			return fmt.Errorf("engine: serve search slices: %w", err)
		}
	}

	if wiki {
		running := sl.Domain(doclet.Domain{}.Name())
		if running == nil {
			return fmt.Errorf("engine: this node runs no pages domain, so " +
				"Doclet has nowhere to write; the domain is in the register and " +
				"its stream failed to come up")
		}
		if n.pages, err = doclet.NewStore(doclet.Options{
			Publisher: running.publisher, DB: sl.estate().Reader(),
			// LIVE off the epoch: `knowledge.skills_container`,
			// `knowledge.root_space` and every `space:` are Tier B, and this store
			// is built once per node.
			Reserved: func() doclet.Reserved {
				return reservedContainers(e.Company())
			},
		}); err != nil {
			return fmt.Errorf("engine: pages store: %w", err)
		}
		if n.pageReader, err = doclet.NewReader(doclet.ReaderOptions{
			Log: running.reader,
		}); err != nil {
			return fmt.Errorf("engine: pages reader: %w", err)
		}
		// LIVE off the epoch, as the store's Reserved above. The index is built
		// above, not here: it covers the tracker too.
		n.searcher = doclet.NewSearcher(doclet.SearcherOptions{
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

	if rooms {
		running := sl.Domain(chatlet.Domain{}.Name())
		if running == nil {
			return fmt.Errorf("engine: this node runs no chat domain, so the " +
				"company's own rooms have nowhere to write — the domain is in " +
				"the register and its stream failed to come up")
		}
		if n.chat, err = chatlet.NewStore(chatlet.Options{
			Publisher: running.publisher, DB: sl.estate().Reader(),
			// THE CHART, READ PER CALL: who a message wakes is resolved at WRITE
			// time, and a captured chart would go on waking a seat that left.
			Roster: liveRoster{engine: e},
			// THE VISIBILITY A CREATE NAMING NONE GETS: founder policy, converted
			// once at this edge so the domain needs no config dependency. A nil
			// block answers false (public), the field's documented default.
			DefaultPrivate: c.Config.Chat.Chatlet.DefaultPrivate(),
		}); err != nil {
			return fmt.Errorf("engine: chat store: %w", err)
		}
		if n.chatReader, err = chatlet.NewReader(chatlet.ReaderOptions{
			Log: running.reader, Committed: running.runner.Committed,
		}); err != nil {
			return fmt.Errorf("engine: chat reader: %w", err)
		}
		// THE KEYWORD INDEX OVER THE ROOMS: `search_messages` is registered
		// only on a non-nil searcher. Built with the rooms, not beside the
		// knowledge indexer, because a vendor-chat company has no
		// `chat_messages`, and an indexer over nothing would wake every two
		// seconds for the life of the process.
		n.chatIndex = search.NewChatIndexer(e.backends.Store, e.domainEstate())
	}

	// No projector loop: every native backend is a state-log domain applied by
	// the core's own apply loop.
	n.done.Add(1)
	go func() {
		defer n.done.Done()
		n.indexer.Run(runCtx)
	}()
	// THE CHAT INDEX'S OWN LOOP, on the same terms: the two walks are not
	// alike ([search.ChatIndexer.Run]).
	if n.chatIndex != nil {
		n.done.Add(1)
		go func() {
			defer n.done.Done()
			n.chatIndex.Run(runCtx)
		}()
	}
	if n.changeIndex != nil {
		n.done.Add(1)
		go func() {
			defer n.done.Done()
			n.changeIndex.Run(runCtx)
		}()
	}

	// PUBLISHED, whole: every field above is written before the store, so
	// readers load it without a lock.
	e.native.Store(n)
	// THE HALF IS THE ENGINE'S FROM HERE: the cleanup above stands down and
	// [Engine.stopNative] ends it. Its object collector is NOT started here:
	// it appends, which a capacity window forbids ([Engine.startNativeCollector]).
	started = true
	log.InfoContext(ctx, "native_backends_started",
		"tracker", runTracker, "knowledge", wiki, "chat", rooms, "code", built.runsCode(),
		"git", n.gitletHost != nil)
	return nil
}

// objectEstates is every domain of this runtime whose rows may name objects,
// as the object store's passes read them: the tracker's (a project's file row)
// and the code log's (a push's pack); [objectEstatesFor] adds the core's
// artifacts log. Chat's rows only LINK a file and pages carry their bodies
// whole. Empty only on a zero runtime.
//
// WHATEVER BACKENDS THE COMPANY CHOSE ([native.trackerFiles]): the rows exist
// because the log runs, and the collector starts only with every declared
// table's estate. The engine's tests build the sources from this list against
// internal/objstore/references, so a table declared in a new domain fails the
// build rather than leaving the passes unstarted in production.
func (n *native) objectEstates() []collect.Estate {
	if n.trackerFiles == nil {
		// A ZERO RUNTIME (a test's) holds no domain's rows; a collector over one
		// is refused by name ([Engine.startCollectorOver]).
		return nil
	}
	estates := []collect.Estate{worklet.ObjectEstate{Reader: n.trackerFiles}}
	if n.codeReader != nil {
		estates = append(estates, gitlet.ObjectEstate{Reader: n.codeReader})
	}
	return estates
}

// startNativeFor brings the native halves up for the first company an APPLY
// hands a node that is not running them (booted with none, then `PUT
// /config`), reporting whether it did. It runs BEFORE the tools are equipped
// and the inbound edge started, which read these halves, and AFTER the
// revision is BUILT. The loops [New] starts beside the halves start here too;
// seat acquisition waits for hydration ([Engine.StateLogHydrated]). Every
// company starts it, even one running no half natively.
//
// A later refusal of the apply does NOT take the halves down: they follow the
// fleet's logs, not the revision, so the retry finds them up. A revision
// changing which halves run takes effect on restart
// ([Engine.sayBackendSwitches]).
func (e *Engine) startNativeFor(ctx context.Context, c *Company) (bool, error) {
	if e.native.Load() != nil || e.remote.Load() != nil || c == nil {
		return false, nil
	}
	// THE INBOUND EDGE'S MAILBOX BEFORE THE FEEDS publish onto it
	// ([Engine.ensureInboundMailbox]) and before the halves start, so a
	// refusal leaves nothing up for the retry to skip past.
	if e.mode.Publishes() {
		if err := e.ensureInboundMailbox(ctx); err != nil {
			return false, err
		}
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
// over every domain a declared table names ([objectEstatesFor]; the declared
// list, never sources written here), whatever backends the company chose
// ([native.objectEstates]). A runtime it cannot start over is logged as
// `object_collector_not_started`. ONE CALL FOR BOTH CALLERS, [New] and
// [Engine.startNativeFor].
//
// Both reach it only past their publish gate: every pass PINS the estate with
// a barrier append, which a maintenance mode forbids (package doc,
// "Maintenance mode"; [statelog.RefuseMaintenance]), and without which it
// would collect against an end nobody established.
func (e *Engine) startNativeCollector(ctx context.Context) {
	if err := e.startCollectorOver(ctx, e.native.Load()); err != nil {
		log.WarnContext(ctx, "object_collector_not_started", "error", err)
	}
}

// startCollectorOver starts the collector over n's estates and the core's
// artifacts, assistant and executor estates ([objectEstatesFor]), and answers
// why it did not — which [Engine.startNativeCollector] logs.
//
// A RUNTIME THAT OFFERS NO ESTATE IS REFUSED, NEVER SKIPPED: every published
// runtime offers one per declaring domain ([native.objectEstates]), so none is
// a wiring mistake, and [collect.Sources] refuses it by name, a line an
// operator can find. Only a node with no native half (n nil) starts none
// without a word.
func (e *Engine) startCollectorOver(ctx context.Context, n *native) error {
	if n == nil {
		return nil
	}
	refs, err := collect.Sources(references.All, objectEstatesFor(n, e.ArtifactsReader(),
		e.AssistantReader(), e.ExecutorReader())...)
	if err != nil {
		return err
	}
	return e.startObjectCollector(ctx, refs)
}

// startNativeDuties arms the native half's fleet-singleton duties: the vector
// domain's one writer, the trash reaper ([Engine.startTrashReaper]), chat's
// horizon ([Engine.startChatRetention]) and, where the half hosts code, the
// purge of deleted repositories ([Engine.startCodeRetention]); and, where it
// serves git, this node's own COMPACTOR and HOME'S CHANGE WORK, which no lease
// decides (every Gitlet node runs both). ONE CALL FOR BOTH CALLERS, [New] and
// [Engine.startNativeFor], so halves met at an apply miss no duty a boot arms.
// The core's duties are [Engine.startCoreDuties]'.
func (e *Engine) startNativeDuties(ctx context.Context) {
	c, n := e.core.Load(), e.native.Load()
	if n == nil || c == nil {
		return
	}
	// Without it semantic search runs over an empty corpus, which reports as
	// a healthy domain rather than a missing one.
	e.startEmbedding(ctx, c.log)
	e.startTrashReaper(ctx)
	// THE CHAT DOMAIN'S HORIZON: chat's retention cannot be a per-node sweep
	// (each clock would delete a different set of replicated messages), and
	// without it nothing publishes the cutoff `message_retention_days` names.
	e.startChatRetention(ctx)
	// GITLET'S PURGE, where the half hosts code: without it a repository
	// removed from code.repos keeps its name and packs for ever.
	e.startCodeRetention(ctx, c.nodeID, n)
	// THE COMPACTOR, where the half serves git ([Engine.buildGitletHost] built
	// it unstarted). Its three loops (compaction, publication, expunge) start
	// HERE because both callers reach this only once the runtime is PUBLISHED
	// (a pass started earlier found its listing unserved) and the mode
	// PUBLISHES (each loop writes objects and appends records). Its stop is
	// [native.stopGitletHost].
	if n.compactor != nil {
		n.compactor.Start()
	}
	// THE HOME'S CHANGE WORK, for the compactor's two reasons: its first pass
	// reads the published runtime, and every turn asks a writer for records.
	if n.home != nil {
		n.home.Start()
	}
	// THE CHECK RUNNER, for the same two reasons.
	if n.checkRunner != nil {
		n.checkRunner.Start()
	}
}

// stopNative ends this node's native halves. Nil-safe, which is the node that
// has met no company.
func (e *Engine) stopNative(ctx context.Context) { e.native.Load().shutdown(ctx) }

// shutdown ends everything this half started and WAITS for it.
//
// ONE IMPLEMENTATION for [Engine.stopNative] and [Engine.startNative]'s
// failure path: a half-built runtime leaks what a built one does (a broker
// answerer is a claim on buckets peers count on). Nil-safe throughout. IT
// DOES NOT STOP THE LOGS, which are the core's ([Engine.stopCore], after).
//
// ctx is taken for its VALUES (the teardown's trace) and stripped with
// [context.WithoutCancel]: the withdrawal calls the broker, and both the
// caller's context and this runtime's own ([native.stop]) are routinely dead.
func (n *native) shutdown(ctx context.Context) {
	if n == nil {
		return
	}
	if n.stopSlices != nil {
		// FIRST, before an answerer can be cancelled mid-scan: withdrawing costs
		// a coordinator one missing assignment, where a registration outliving
		// the scan costs it a silent empty slice counted as answered.
		_ = n.stopSlices(context.WithoutCancel(ctx))
	}
	// GITLET'S GIT SURFACE, THEN THE COMMITTER, BEFORE THE LOG THEY PUBLISH
	// ON, which the core stops after this. The teardown already stopped both
	// ([Engine.stopGitletHost], [Engine.stopCommitter]); these are the failure
	// path's, and a second stop of either costs nothing
	// ([native.stopGitletHost]).
	n.stopGitletHost(ctx)
	n.stopCommitter(ctx)
	n.stop()
	n.done.Wait()
}

// NativeStarted reports whether this node has brought its native halves up —
// whether it has met its first company — on a data node its own, and on one
// without `data` the halves it reaches through a data node ([remoteNative]).
//
// MONOTONIC: false until the halves are published and never false again, so a
// caller reading this FIRST and a half's accessor AFTER cannot mistake "not up
// yet" for "not configured". The API's live sources rely on that: a half not
// up answers 503, one the company does not run answers as an absent route.
func (e *Engine) NativeStarted() bool {
	return e.native.Load() != nil || e.remote.Load() != nil
}

// halves is which halves this company runs natively here, as one reading of
// this node's runtime found them ([Engine.nativeHalves]): the ones a seat's
// tools and the operator's surfaces read and write through the router.
type halves struct {
	// tracker, wiki and chat say which halves the company runs natively,
	// however this node reaches them. All false on a node with no native
	// runtime yet, and on one whose company runs every half on a vendor.
	tracker, wiki, chat bool
	// code says the company hosts its repositories in the engine
	// (`code.backend: gitlet`) and this runtime was built to serve them.
	// Counted by the built choice, not a reader, because the code log's reader
	// exists in every register whatever the company hosts.
	code bool
	// own is this data node's own runtime, for a reader needing more than which
	// halves run ([OperatorNative], [Engine.skillSource]); nil on a node without
	// `data` or running none. Taken in the same reading as the halves, so the
	// two never describe different runtimes.
	own *native
}

// any reports a company that runs at least one of Worklet, Doclet and
// Chatlet natively. Gitlet is not counted: seat admission and
// serviceability wait on the three halves' copies.
func (h halves) any() bool { return h.tracker || h.wiki || h.chat }

// nativeHalves is [halves] for e at this instant: THE ONE DERIVATION of which
// halves a node runs, read by seat admission ([Engine.StateLogHydrated]),
// [Engine.serviceable], the operator's read seams ([OperatorNative],
// [OperatorChat], [OperatorCode], [CodeToolDeps]), the seats' seams
// ([Engine.trackerHalves], [Engine.pageHalves], [Engine.chatHalves]), the
// inbound parsers ([Engine.nativeParsers]), [Engine.Knowledge] and
// [Engine.skillSource]. A change to what counts as running a half is made here
// once; each counts a data node's halves by its READERS, never its writers.
//
// One or two atomic loads: the two pointers are never both set, since a node
// builds one runtime or the other once per process.
func (e *Engine) nativeHalves() halves {
	if r := e.remote.Load(); r != nil {
		return halves{tracker: r.tracker, wiki: r.wiki, chat: r.chat, code: r.built.runsCode()}
	}
	if n := e.native.Load(); n != nil {
		return halves{tracker: n.trackerReader != nil, wiki: n.pageReader != nil,
			chat: n.chatReader != nil, code: n.built.runsCode(), own: n}
	}
	return halves{}
}

// Tracker is this node's OWN copy's tracker read side, or nil: what its
// router answers from while the copy serves, and what a test reads. A
// surface reads through [OperatorWork], which falls back to a peer's copy.
func (e *Engine) Tracker() *worklet.Reader {
	n := e.native.Load()
	if n == nil {
		return nil
	}
	return n.trackerReader
}

// TrackerWriter is this node's tracker write side, or nil (also in a
// maintenance mode; see [Engine.writeSide]).
func (e *Engine) TrackerWriter() *worklet.Writer { return e.writeSide().tracker }

// Pages is this node's OWN copy's knowledge read side, or nil — see
// [Engine.Tracker]; a surface reads through [OperatorPages].
func (e *Engine) Pages() *doclet.Reader {
	n := e.native.Load()
	if n == nil {
		return nil
	}
	return n.pageReader
}

// PagesStore is this node's knowledge write side, or nil (also in a
// maintenance mode; see [Engine.writeSide]).
func (e *Engine) PagesStore() *doclet.Store { return e.writeSide().pages }

// Chat is this node's OWN copy's chat read side, or nil — see
// [Engine.Tracker]; a surface reads through [OperatorChat].
func (e *Engine) Chat() *chatlet.Reader {
	n := e.native.Load()
	if n == nil {
		return nil
	}
	return n.chatReader
}

// ChatStore is this node's chat write side, or nil (also in a maintenance
// mode; see [Engine.writeSide]). A surface writes through [OperatorChatWriter].
func (e *Engine) ChatStore() *chatlet.Store {
	return e.writeSide().chat
}

// ChatIndex is this node's OWN chat keyword index, or nil — what this node's
// router answers a chat search from while its copy serves. A surface searches
// through [OperatorChatSearch], which resolves the viewer's rooms beside
// whichever index answers.
func (e *Engine) ChatIndex() *search.ChatIndexer {
	n := e.native.Load()
	if n == nil {
		return nil
	}
	return n.chatIndex
}

// DocletSearcher is Doclet's knowledge searcher, or nil.
func (e *Engine) DocletSearcher() *doclet.Searcher {
	n := e.native.Load()
	if n == nil {
		return nil
	}
	return n.searcher
}

// startNativeFeeds starts the change feeds, per node.
//
// A FLEET-WIDE GROUP, not a duty: every data node pulls, so whichever gets a
// change first handles it and one node's lease flap stalls nothing. Started
// once, not per epoch, because a feed follows a DOMAIN, which a revision does
// not change; with the native half, not the core, because a feed wakes seats,
// which a node that has met no company does not have.
func (e *Engine) startNativeFeeds(ctx context.Context) {
	n, c := e.native.Load(), e.core.Load()
	if n == nil || c == nil || c.log == nil {
		return
	}

	// IN THE REGISTER'S ORDER and through [Engine.feedFor], which holds each
	// feed to the group its domain declares (the consumer the trim waits on).
	// A mismatch gets no feed and an error naming it, rather than a trim that
	// silently waits for ever or for nothing.
	var started []*changefeed.Feed
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
			// THE DOMAIN DECLARES NO WAKE FEED, and no trim term either.
			continue
		}
		// THROUGH THE ONE DOOR every fleet-wide group passes ([Engine.joins]).
		if !e.joins(ctx, translator.Source().Group) {
			continue
		}
		// THE QUEUE'S DEAD-LETTER STREAM keeps a handed-back record for an hour
		// and the start replays it, through the ledger that accounts for every
		// entry a replay drops, so the alarm never counts one as a lost wake
		// (feeddeadletters.go).
		feed, err := changefeed.New(changefeed.Options{
			Opener: opener, Publisher: e.backends.Queue,
			Claims: e.backends.Fleet, Translator: translator,
			Metrics: e.metrics, Objectives: e.objectives,
			DeadLetters: e.feedDeadLetters(),
		})
		if err != nil {
			log.ErrorContext(ctx, "changefeed_unavailable",
				"source", translator.Source().Name, "error", err.Error())
			continue
		}
		started = append(started, feed)
	}
	// PUBLISHED BEFORE ANY RUNS, so the alarm reading sees every feed from
	// its first beat ([native.feedsReading]), a failing one included.
	n.feeds.Store(&started)
	for _, feed := range started {
		n.done.Add(1)
		go func() {
			defer n.done.Done()
			// THE NATIVE HALF'S OWN CONTEXT, joined by stopNative. Run returns only
			// then: a failing consumer reopens on its own and reports its outage
			// ([changefeed.Feed.Outage]).
			//nolint:contextcheck // n.run is [context.WithoutCancel] of
			// the boot context: a feed started under the CALLER's would be one
			// [native.shutdown] can never end, and its wait would block for ever.
			feed.Run(n.run)
		}()
	}
}

// feedsReading is [native.feedsReading] over this node's native half. A node
// that has met no company has started none, and reads no feed.
func (e *Engine) feedsReading(now time.Time, out *statelog.Reading) {
	e.native.Load().feedsReading(now, out)
}

// feedsReading fills the change feeds' half of an alarm reading: every feed
// this node started that is down, how long for and why.
//
// THIS NODE'S OWN FEEDS. A feed's group is the fleet's, so a peer's live feed
// still handles the records; the alarm names the node whose consumer is down,
// which on a single data node is every wake in the company.
func (n *native) feedsReading(now time.Time, out *statelog.Reading) {
	if n == nil {
		return
	}
	feeds := n.feeds.Load()
	if feeds == nil {
		return
	}
	for _, feed := range *feeds {
		outage, down := feed.Outage()
		if !down {
			continue
		}
		out.FeedsDown = append(out.FeedsDown, statelog.FeedOutage{
			Source: outage.Source, For: now.Sub(outage.Since),
			Failures: outage.Failures, Cause: outage.Cause,
		})
	}
}

// declaredInboundSources is every source this BUILD's own producers write an
// inbound delivery under, marked first-party (types.RawWebhook.FirstParty),
// whether or not the running company uses it. [Engine.nativeParsers] routes
// exactly these where their half runs; the notification service defers a
// first-party delivery from any OTHER source to a newer peer
// (notify.DeferNewerSource) and acknowledges a declared one this company does
// not run.
func declaredInboundSources() []string {
	return []string{alarmwake.Source, accessreview.Source, atlaswake.Source, worklet.Source,
		doclet.Source, chatlet.Source, gitlet.Source, artifactsdomain.Source}
}

// nativeParsers is what the engine's own sources contribute to the inbound
// edge: the native backends' parsers, the alarms' (internal/alarmwake), the
// access reviews' (internal/accessreview) and the atlas's drift wakes'
// (internal/atlaswake).
//
// PER EPOCH, unlike everything else in this file: a parser's one
// company-derived input is the org chart, which an apply changes. See
// [Engine.reconcileNative].
func (e *Engine) nativeParsers(c *Company) ([]notify.Parser, []notify.Prompt) {
	if c == nil {
		return nil, nil
	}
	// THE ALARMS' WAKES, on every node: routed by the chart's health grant
	// alone (orgchart.GrantedSeats), reading no row.
	parsers := []notify.Parser{alarmwake.NewParser(alarmwake.ParserOptions{
		Granted: orgchart.GrantedSeats(c.Org, authz.SeatGrantHealth),
	})}
	prompts := []notify.Prompt{alarmwake.Prompt{}}
	// THE ACCESS REVIEWS, on every node too: a review names its seat and
	// reads only the roster (internal/accessreview).
	parsers = append(parsers, accessreview.Parser{})
	prompts = append(prompts, accessreview.Prompt{})
	// THE ATLAS'S DRIFT WAKES, on every node: a wake names its seat and reads
	// no row (internal/atlaswake).
	parsers = append(parsers, atlaswake.Parser{})
	prompts = append(prompts, atlaswake.Prompt{})
	// A COMMENT ON AN ARTIFACT, on every node: the artifacts domain is no
	// half a company chooses, and the record carries its writer's routing.
	parsers = append(parsers, artifactsdomain.NewParser(artifactsdomain.ParserOptions{}))
	prompts = append(prompts, artifactsdomain.Prompt{})
	// THE HALVES THIS COMPANY RUNS NATIVELY, wherever they are held: a parser
	// reads a record's routing snapshot and the chart, never a row.
	run := e.nativeHalves()
	if run.tracker {
		// NO LEAD MAP AND NO BASE URL: the WRITER resolved the routing at
		// commit; resolving at read time would name whoever holds the role later.
		parsers = append(parsers, worklet.NewParser(worklet.ParserOptions{}))
		prompts = append(prompts, worklet.Prompt{})
	}
	if run.wiki {
		// THE LEAD MAP, and the base its links use: `api.external_url`, since the
		// link is opened by whoever the change woke ([Engine.publicBase]).
		parsers = append(parsers, doclet.NewParser(doclet.ParserOptions{
			Leads: containerLeads(c.Org), BaseURL: e.publicBase(c),
		}))
		prompts = append(prompts, doclet.Prompt{})
	}
	if run.chat {
		// NOTHING TO CONFIGURE, for the tracker arm's reason: the `answerer` seat
		// is resolved at WRITE time inside the decide and rides the record.
		parsers = append(parsers, chatlet.NewParser())
		// A CONSTRUCTOR, not `chatlet.Prompt{}`: this [notify.ChatPrompt] has
		// fields, and a zero value reads a DM as an ordinary room, landing a
		// person's consecutive messages in as many turns, silently.
		prompts = append(prompts, chatlet.NewPrompt())
	}
	if run.code {
		// NO LEAD MAP AND NO BASE URL, as the tracker's: the record carries its
		// writer's routing, and the woken seat reads the change with its code
		// tools rather than a link.
		parsers = append(parsers, gitlet.NewParser(gitlet.ParserOptions{}))
		prompts = append(prompts, gitlet.Prompt{})
	}
	return parsers, prompts
}

// reconcileNative brings the native runtime to a newly applied epoch: it says
// which of the epoch's choices wait for a restart, and swaps the native
// parsers. NOT the appliers, index, stores or feeds: they follow a LOG.
//
// No retirement branch: the runtime is built once per process, so switching
// `tracker.backend` away from native takes effect on restart, and the parser
// staying registered until then is honest ([Engine.sayBackendSwitches]).
//
// The chart's projects, knowledge containers and code.repos run in
// [Engine.followCompany] after the publish; running them here too would
// write them twice, once against a company not yet current.
func (e *Engine) reconcileNative(ctx context.Context, c *Company) {
	built, runs := e.runtimeBuilt()
	if !runs {
		return
	}
	// THE CHOICES THIS RUNTIME WAS NOT BUILT WITH, said once per activation:
	// they change nothing until a restart. FIRST, because a node with no edge
	// (maintenance mode) still runs a runtime.
	e.sayBackendSwitches(ctx, built, c)
	e.swapNativeParsers(ctx, c)
}

// swapNativeParsers swaps the native parsers on this node's inbound edge for
// the epoch c's, and does nothing on a node whose edge is not running.
//
// As every vendor reconciler: a boot-time parser would route the new
// revision's work by the old org chart, waking a project's former lead.
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
			// THE PREVIOUS PARSER KEEPS RUNNING, as every vendor's: a stale chart
			// routes better than no routing at all.
			log.ErrorContext(ctx, "native_reconcile_failed",
				"source", parser.Source(), "error", err.Error(),
				"detail", "the previous routing is still current")
		}
	}
}

// applyContainers makes the knowledge containers this company names exist:
// a page only NAMES its container, so without this every read that
// ENUMERATES them (`GET /containers`, the Knowledge rail) finds none.
//
// The keys are the chart's spaces (a unit's and a seat's own `space:`, the
// pair [chartProjects] reads) plus the two RESERVED containers, tool skills
// and the org root, which the engine writes into and so must list.
//
// Stamped with the ACTIVATION and best effort, as [Engine.applyChart]; the
// store refuses a zero ([doclet.ErrNoActivation]) and writes nothing when the
// row already matches ([doclet.Store.EnsureContainer]).
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
			// EVERY CONTAINER IS ATTEMPTED: one key's refusal must not leave the rest
			// unlistable, and the caller runs again.
			log.ErrorContext(ctx, "knowledge_container_not_applied",
				"container", want.Key, "error", err.Error(),
				"detail", "pages in it are readable by address and the "+
					"container will not appear in a listing; the next config "+
					"apply or restart retries it")
			continue
		}
		// ONLY WHAT WAS WRITTEN: this runs on every boot and apply, and naming
		// every unchanged row would report an unedited company as changed.
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
// DE-DUPLICATED ON THE KEY, FIRST DECLARATION WINS, for [chartProjects]'
// reason. The reserved two come LAST, so a unit's own name on one of them
// (which the config loader refuses anyway) is not overwritten.
func chartContainers(c *Company) []chartContainer {
	if c == nil || c.Config == nil {
		return nil
	}
	var out []chartContainer
	seen := map[string]bool{}
	add := func(key, name, purpose string) {
		// THE ONE CANONICAL FORM ([doclet.ContainerKey]): a chart writing `eng`
		// or ` ENG` must not create a second container no page is in.
		key = doclet.ContainerKey(key)
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
			// A SEAT'S OWN SPACE takes the SEAT's name: a container for that seat's
			// own writing rather than its unit's.
			add(role.Space, role.Name, "")
		}
	}
	// TRUE OF AN EMPTY SPACE TOO: the purpose heads the space beside its page
	// count, so it names the Onboarding page as a convention, not a promise.
	add(c.Config.RootSpaceKey(), "Company",
		"What is true of the whole company. A page here titled Onboarding is the first every seat reads.")
	add(c.Config.SkillsContainerKey(), "Tool skills",
		"Prompt fragments the engine injects into a phase. Excluded from knowledge search.")
	return out
}

// applyChart makes the projects this company's chart names exist.
//
// From the chart, not at the first write: a create takes the next number from
// its project's counter, so a project created inside it gives two nodes filing
// at once two projects and two ENG-1s. On every publish
// ([Engine.followCompany]), so a new `project:` key needs no restart; the
// decide writes nothing when the chart-owned fields match.
//
// Stamped with the ACTIVATION's instant ([worklet.Project.ChartEpoch]), never
// this node's clock, so a node applying an older revision late cannot walk
// newer names back. A failure is LOGGED, not raised: the rest of the apply
// stands, and the next apply or boot retries.
func (e *Engine) applyChart(ctx context.Context, c *Company) {
	writer := e.TrackerWriter()
	if writer == nil || c == nil || c.Org == nil || c.ActivatedAt.IsZero() {
		return
	}
	// THE SEATS' ORDER FIRST, whatever the projects do: `sort=assignee` reads
	// it from the tracker's own rows ([worklet.Writer.ApplyRoster]).
	ordered, err := writer.ApplyRoster(ctx, c.ActivatedAt, seatNames(c.Org))
	switch {
	case err != nil:
		log.ErrorContext(ctx, "tracker_roster_not_applied", "error", err.Error(),
			"detail", "work sorted by assignee keeps the order of the last chart "+
				"applied; the next config apply or restart retries it")
	case ordered:
		log.InfoContext(ctx, "tracker_roster_applied")
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

// seatNames is every seat the chart holds, by handle, with the name a person
// reads it by: what the seats' order is collated from.
func seatNames(o *org.Organization) map[string]string {
	out := map[string]string{}
	for role := range o.AllRoles() {
		out[role.Handle()] = role.Name
	}
	return out
}

// chartProjects is every project the org chart names, with the three fields
// the chart owns.
//
// DE-DUPLICATED ON THE KEY: two units legitimately share a project, and one
// project written twice in one pass would contend with itself at the broker
// and log a failure for a fine config. The FIRST declaration wins, as the
// lead does for the same key ([projectLeads]).
func chartProjects(o *org.Organization) []worklet.ChartProject {
	var out []worklet.ChartProject
	seen := map[string]bool{}
	add := func(key, name, purpose, unit string) {
		// NORMALIZED as every scope is ([org.NormalizeScope]): a project key is
		// compared upper everywhere, so `eng` must not create a project no board
		// can find.
		key = org.NormalizeScope(key)
		if key == "" || seen[key] {
			return
		}
		seen[key] = true
		out = append(out, worklet.ChartProject{
			Key: key, Name: name, Purpose: purpose, Unit: unit,
		})
	}
	// THE UNIT COLUMN IS THE UNIT'S KEY, never its name: a task's filed unit is
	// never rewritten, so a name would strand every item the day the team is
	// renamed, which is what `id:` exists to prevent. Readers resolve the key
	// back to the team's current name through the chart.
	for unit := range o.AllUnits() {
		add(unit.Project, unit.Name, unit.Purpose, unit.Key())
	}
	// A SEAT'S HOME UNIT THROUGH ONE INDEX ([org.Index.UnitFor]), built lazily
	// at the first seat naming its own project: [org.Organization.UnitFor] per
	// seat on every apply would cost seats².
	var homes *org.Index
	for role := range o.AllRoles() {
		// EVERY seat: `Organization.Roles` holds only seats declared outside a
		// unit, and a nested seat naming its own project would otherwise get none.
		//
		// A ROLE'S OWN PROJECT takes the ROLE's name (that seat's work rather than
		// its unit's) and its home unit, so it still says where it sits.
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
// THROUGH [org.Organization.LeadsBy], where "who owns this scope" lives. An
// ambiguity is logged, not refused: units sharing a project is ordinary, and
// only a DISAGREEMENT about the lead matters.
func projectLeads(o *org.Organization) map[string]string {
	if o == nil {
		return nil
	}
	leads, report := o.LeadsBy(org.Scope{
		OfUnit: func(u *org.Unit) string { return u.Project },
		OfRole: func(r *org.Role) string { return r.Project },
	})
	for _, unled := range report.Unled {
		// LOUD, because an item naming nobody in that project routes to nobody,
		// which looks exactly like an item nobody filed.
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
func containerLeads(o *org.Organization) doclet.Leads {
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
	return doclet.Leads(leads)
}

// ProjectOfSeat is one seat's home project, from a chart.
//
// EXPORTED so the seat tools (a create's default) and the API
// (`preset=my_queue`'s second arm) share one walk: a queue scoped to a
// different project than creates file into is undiagnosable from either screen.
func ProjectOfSeat(o *org.Organization, handle string) string {
	return scopeOfSeat(o, handle,
		func(u *org.Unit) string { return u.Project },
		func(r *org.Role) string { return r.Project })
}

// SpaceOfSeat is one seat's home space, from a chart: the container a page it
// writes naming none lands in. Exported for the reason [ProjectOfSeat] is: the
// page tools default to it ([LiveSeatSpace]) and `GET /viewer` says it, so the
// Dashboard Assistant's preview names the space the write will use.
func SpaceOfSeat(o *org.Organization, handle string) string {
	return scopeOfSeat(o, handle,
		func(u *org.Unit) string { return u.Space },
		func(r *org.Role) string { return r.Space })
}

// scopeOfSeat is the project or container a seat files into: its own, else
// its unit's, else its nearest ancestor's.
//
// THE WALK IS UPWARD, so a seat in a team with no space writes in its
// department's. A seat with none anywhere returns empty and the tool refuses:
// a page filed where nobody chose is one nobody finds.
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

// skillDetector answers whether a page body is a tool skill, for the flag the
// pages applier derives.
type skillDetector struct{}

// IsSkill reports a body that parses as a tool skill.
//
// THROUGH THE SKILLS PACKAGE'S OWN ADMISSION TEST, so the flag means exactly
// what the registry means: a second heuristic would let a page be a skill to
// the applier and prose to the loader, silently.
func (skillDetector) IsSkill(body string) bool { return skills.IsSkill(body) }

// ---- what a seat's tools are given ------------------------------------- //

// workDeps is the tracker half of a seat's builtin surface, per epoch.
//
// Both halves nil omits every tracker tool, as on a company on Jira: a seat
// offered a tool against a tracker its company does not run would fail at
// the call.
func (e *Engine) workDeps(c *Company) builtin.WorkDeps {
	halves, ok := e.trackerHalves()
	if !ok {
		return builtin.WorkDeps{}
	}
	return builtin.WorkDeps{
		Reader:       halves.reader,
		ArtifactLink: e.ArtifactLink,
		// ONE WRITER PER ACTOR: the party is the immutable seat the tool surface
		// bound, never anything a model can name. Its provenance carries the
		// turn's write log ([builtin.Actor.Provenance]).
		Writer: func(actor builtin.Actor) builtin.WorkWriter {
			return halves.as(actor)
		},
		// THE PROJECT SETTINGS for every seat: declaring a tag is open to all, and
		// a create refuses an undeclared label, so without this writer the
		// `labels` argument would be unusable. Other facets are authorized per call.
		ProjectWriter: func(actor builtin.Actor) builtin.ProjectWriter {
			return halves.as(actor)
		},
		// THE DEPENDENCY SEQUENCE: two commits on two subjects, so it needs the
		// replicated estate to check its counterparties first, which this writer has.
		Dependencies: func(actor builtin.Actor) builtin.WorkDepender {
			return halves.as(actor)
		},
		Merges: func(actor builtin.Actor) builtin.WorkMerger {
			return halves.as(actor)
		},
		// THE CROSS-PROJECT MOVE re-keys an item and its subtree.
		Moves: func(actor builtin.Actor) builtin.WorkMover {
			return halves.as(actor)
		},
		// THE PROJECT'S FILES: rows through the same halves, bytes through this
		// node's own object client, which reaches the data nodes directly even on
		// a node holding no data, never through the estate service.
		Files: halves.files,
		FileWriter: func(actor builtin.Actor) builtin.FileWriter {
			return halves.as(actor)
		},
		Objects: e.ObjectStore(),
		// THE RANKED SEARCH reads, so it takes no actor. Nil where this node has
		// no index, and the tool is then not advertised.
		Search:   WorkSearcher(e),
		Mentions: seatMentions{org: c.Org},
		// THE ROSTER and the chart seams below are read PER CALL against the epoch
		// current when the tool runs: a seat's tools are cloned into its lease and
		// an apply does not rebuild the clone, so a captured chart would refuse a
		// colleague who joined and admit one who left.
		FleetSeat: e.FleetSeat,
		Grants:    e.GrantDesk(),
		ChangeRef: e.ChangeRef,
		Seats:     e.seatCorpus,
		Units:     liveUnits{engine: e},
		DefaultProject: func(handle string) string {
			return ProjectOfSeat(e.Company().Org, handle)
		},
		Leads: liveLeads{engine: e},
		// Where an asker may promise to report a decision: a captured chart
		// would admit a channel or surface the company has since dropped.
		Channels: liveChannels{engine: e},
		Now:      func() time.Time { return time.Now().UTC() },
		// THE COMPANY'S CLOCK AS THIS EPOCH SETS IT (ADR-0018), deliberately not
		// per call: a turn pins its epoch, and resolving "due friday" on one
		// clock and overdue marks on the next mixes two companies.
		Zone:  c.Config.Location,
		Await: halves.await,
	}
}

// trackerWriter is everything a seat's tools write to the tracker through,
// as one party.
type trackerWriter interface {
	builtin.WorkWriter
	builtin.AskResolver
	builtin.ProjectWriter
	builtin.WorkDepender
	builtin.WorkMerger
	builtin.WorkMover
	builtin.FileWriter

	// RecordTurn is the one write the ENGINE makes as a seat rather than a
	// tool: a turn's spend onto its task. See [Engine.recordTurnSpend].
	RecordTurn(ctx context.Context, opID string, turn worklet.TurnRecord) (worklet.WriteResult, error)
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
// Worklet.
//
// THE ROUTER'S, ON EVERY NODE, so a tool's reader and writer are the same kind
// of half: a read after a write waits on the node's floors ([estate.Session])
// whichever holder answers. A local reader beside a remote writer would read
// its own write back from a copy that had not seen it.
//
// A node that hands out no tracker writer ([Engine.writers]) hands out no
// tracker tools either, rather than tools whose every write is refused.
func (e *Engine) trackerHalves() (trackerSeams, bool) {
	if writes, _, _ := e.writers(e.nativeHalves()); !writes {
		return trackerSeams{}, false
	}
	r := e.router
	return trackerSeams{
		reader: r.Work(),
		files:  r.Work(),
		as: func(actor builtin.Actor) trackerWriter {
			return r.WriterAs(remoteActor(actor))
		},
		// THE SESSION FLOOR IS THE WAIT: whichever holder answers the next read
		// has applied this far.
		await: r.Await,
	}, true
}

// ChartUnits is an org chart as the tracker's unit seam.
//
// THE ONE IMPLEMENTATION: the tracker holds no org (two nodes briefly on
// different epochs would write different rows), so a stored unit is resolved
// at READ time, and every surface rendering, filtering or writing one must
// reach the same answer.
//
// THROUGH [org.Organization.UnitByRef]: a stored unit carries its key, or its
// name where it has none, in any case, and both resolve.
func ChartUnits(o *org.Organization) worklet.Units { return chartUnits{org: o} }

type chartUnits struct{ org *org.Organization }

// ResolveUnit answers the unit a stored or typed reference names: its key, its
// name and its effective lead.
func (c chartUnits) ResolveUnit(ref string) (worklet.ChartUnit, bool) {
	if c.org == nil {
		return worklet.ChartUnit{}, false
	}
	unit := c.org.UnitByRef(ref)
	if unit == nil {
		return worklet.ChartUnit{}, false
	}
	lead := worklet.LeadRef{}
	// THE EFFECTIVE LEAD, inherited from an ancestor where this unit declares
	// none: that is who hears about the work, and `none` beside a unit whose
	// parent has a lead sends a founder looking for a gap that is not there.
	//
	// It resolves the handle `lead:` holds, never one derived from the display
	// name, which would miss every seat with a declared handle.
	if role := c.org.EffectiveLead(unit); role != nil {
		lead.Handle = role.Handle()
		lead.Kind = worklet.AuthorAgent
		if role.IsHuman() {
			lead.Kind = worklet.AuthorHuman
		}
	}
	// THE KEY AND THE NAME: a write stores the key ([org.Unit.Key]) and a
	// screen reads the name.
	return worklet.ChartUnit{Key: unit.Key(), Name: unit.Name, Lead: lead}, true
}

// liveUnits resolves against the epoch current when the tool RUNS.
type liveUnits struct{ engine *Engine }

func (l liveUnits) ResolveUnit(ref string) (worklet.ChartUnit, bool) {
	return ChartUnits(l.engine.Company().Org).ResolveUnit(ref)
}

// liveSeats resolves a people field's value to exactly one seat, against the
// CURRENT epoch, and answers its handle.
//
// EXACTLY ONE: an ambiguous spelling is answered as an unknown one, since a
// stored handle nobody has is silently missed by every filter on the field. A
// node holding no company names nobody.
//
// Per call, as every live seam here: the writer outlives a revision, and a
// captured chart would admit a colleague who has left.
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
// BOTH HALVES, since each alone admits a promise nobody can keep: a company
// surface the seat has no bot on gives it no tools, and a seat's bot on a
// surface the company turned off has no transport. Mattermost is switched by
// `enabled`, Slack is on by being declared: the rule
// [Engine.startNotifications] starts each transport by.
//
// THE ENGINE'S OWN CHAT NEEDS NO BOT: an agent seat of a Chatlet company posts
// as itself ([decision.InformChat]) into a public unit room.
func (c liveChannels) ChatSurfaces(handle string) []decision.InformSurface {
	company := c.engine.Company()
	if company == nil || company.Org == nil || company.Config == nil {
		return nil
	}
	seat := company.Org.SeatByHandle(handle)
	if seat == nil {
		return nil
	}
	var out []decision.InformSurface
	if mm := company.Config.Integrations.Mattermost; mm != nil && mm.Enabled &&
		seat.Mattermost.BotToken != "" {
		out = append(out, decision.InformMattermost)
	}
	if company.Config.Integrations.Slack != nil && seat.Slack.BotToken != "" {
		out = append(out, decision.InformSlack)
	}
	if company.Config.ChatBackendFor() == config.ChatChatlet && !seat.IsHuman() {
		out = append(out, decision.InformChat)
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

// liveRoster is chat's four-method view of the company, resolved against the
// epoch current when the WRITE runs.
//
// PER CALL, as every live seam here: the chat store outlives every revision,
// and a captured chart would publish a wake to a seat that has left.
//
// NO METHOD BLOCKS, as [chatlet.Roster] requires: each runs inside the
// decide's read transaction, so each is an in-memory chart walk and reaches
// no broker, coordination store or model.
type liveRoster struct{ engine *Engine }

// Seat reports whether the company still has this handle, and whether it is a
// PERSON.
//
// BOTH FROM ONE LOOKUP, one fact about one row: a missing handle is a wake
// nothing can run, and a person is addressable but never woken.
// [org.Organization.SeatByHandle], not the agent-only lookup, which would
// report every person as somebody the company does not employ.
func (l liveRoster) Seat(handle string) (exists, human bool) {
	company := l.engine.Company()
	if company == nil || company.Org == nil {
		return false, false
	}
	seat := company.Org.SeatByHandle(handle)
	if seat == nil {
		return false, false
	}
	return true, seat.IsHuman()
}

// Lead names the seat that answers for a unit's room, or empty.
//
// [unitRoomLead] against the CURRENT epoch, by [unitRoomOwner], the rule
// [applyChartRooms] staffs the room by, so a room staffed with nobody wakes no
// lead.
func (l liveRoster) Lead(unit, room string) string {
	company := l.engine.Company()
	if company == nil {
		return ""
	}
	return unitRoomLead(company.Org, unit, room)
}

// Senior names the most senior AGENT seat among handles, or empty:
// [seniorAgent] against the CURRENT epoch.
func (l liveRoster) Senior(handles []string) string {
	company := l.engine.Company()
	if company == nil {
		return ""
	}
	return seniorAgent(company.Org, handles)
}

// seniorAgent is who answers for a room made by hand: the most senior AGENT
// seat among its members' handles, by [org.Organization.Senior], or empty.
//
// IT IS WHAT [chatlet.Roster.Senior] ANSWERS, recorded as the room's answerer
// ([chatlet.ReasonAnswerer]). A PERSON IS NEVER IT, since a person runs no
// turn, and a handle the chart no longer holds is never ranked.
func seniorAgent(chart *org.Organization, handles []string) string {
	if chart == nil || len(handles) == 0 {
		return ""
	}
	named := make(map[string]bool, len(handles))
	for _, h := range handles {
		named[h] = true
	}
	senior := chart.Senior(func(r *org.Role) bool {
		return !r.IsHuman() && named[r.Handle()]
	})
	if senior == nil {
		return ""
	}
	return senior.Handle()
}

// Seats is every seat handle of the CURRENT epoch's chart, people and agents
// alike: the company an auto-join room accounts for when it is turned on.
func (l liveRoster) Seats() []string {
	company := l.engine.Company()
	if company == nil || company.Org == nil {
		return nil
	}
	return chartSeats(company.Org)
}

// chartSeats is every seat handle an org chart holds.
func chartSeats(o *org.Organization) []string {
	var out []string
	for role := range o.AllRoles() {
		out = append(out, role.Handle())
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
// EVERY SPELLING, because a routing unit records what was true when it was
// written: the unit's key, or its name on a unit given no key. Matching the
// `id` field alone resolves no lead on a company whose units have no ids.
//
// THROUGH [org.Organization.UnitByRef] rather than a walk of its own, so the
// fold matches the one the chart claims keys under.
//
// A UNIT THAT EXISTS AND LEADS NOBODY answers empty, which is not the same as
// a unit nothing names; the resolver draws that line for every caller.
func UnitLeadOf(o *org.Organization, unit string) string {
	if o == nil {
		return ""
	}
	return leadOfUnit(o, o.UnitByRef(unit))
}

// leadOfUnit is the handle of the seat that leads a unit the chart has
// already resolved, or empty — for nil, or for a unit that leads nobody.
//
// ONE ANSWER TO "WHO ANSWERS FOR THIS TEAM", shared by the tracker's unit
// fallback and the chat room's ([unitRoomLead]), so a person posting in a
// unit's room addresses the same lead an unassigned task there wakes.
func leadOfUnit(o *org.Organization, unit *org.Unit) string {
	if unit == nil {
		return ""
	}
	if lead := o.EffectiveLead(unit); lead != nil {
		return lead.Handle()
	}
	return ""
}

// seatCorpus is the roster a seat's tools read per call, AND WHAT THE PARTY
// REGISTRY WITHHOLDS NOW: a person the directory has suspended is left out
// exactly as an inbound message from them is left unattributed.
func (e *Engine) seatCorpus() []colleague.Seat {
	return builtin.Corpus(e.Company().Org, e.LookupSecret, e.WithheldContacts())
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
		Seats:            e.seatCorpus,
		DefaultContainer: LiveSeatSpace(e),
		// Read off the CURRENT epoch, as the defaults are: a write into it is asked
		// for the admin a tool skill takes, and the store refuses a seat every write
		// there ([doclet.Reserved]).
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

// LiveSeatCorpus is the roster a seat's tools read per call, for the page
// tools a surface outside this package builds ([builtin.PageDeps.Seats]).
func LiveSeatCorpus(e *Engine) func() []colleague.Seat { return e.seatCorpus }

// pageHalves is this node's knowledge-base seams, on the terms
// [Engine.trackerHalves] gives: the router's on every node.
func (e *Engine) pageHalves() (builtin.PageReader, builtin.PageWriter,
	func(context.Context, statelog.Position) error, bool) {
	if _, writes, _ := e.writers(e.nativeHalves()); !writes {
		return nil, nil, nil, false
	}
	return e.router.Pages(), e.router.Pages(), e.router.Await, true
}

// writers is which of the halves one reading of this node's runtime found
// (run) it hands a WRITER for. THE ONE GATE for every surface writing the
// company's records through this node's router: a seat's tools and a turn's
// spend ([Engine.trackerHalves], [Engine.pageHalves], [Engine.chatHalves]),
// the operator's MCP and act transport, the purge, project files, and chat's
// erase, prune and retention ([OperatorWorkWriter], [OperatorPageWriter],
// [OperatorChatWriter]).
//
// NONE IN A MODE THAT PUBLISHES NOTHING, even without `data`: a routed write
// is one it starts (package doc, "Maintenance mode"). On a data node, only the
// halves its own write side holds ([Engine.writeSide]), a fact apart from its
// readers ([Engine.nativeHalves]).
func (e *Engine) writers(run halves) (tracker, wiki, chat bool) {
	if !e.mode.Publishes() {
		return false, false, false
	}
	if run.own == nil {
		return run.tracker, run.wiki, run.chat
	}
	w := e.writeSide()
	return run.tracker && w.tracker != nil, run.wiki && w.pages != nil, run.chat && w.chat != nil
}

// chatDeps is the rooms half, on the same terms as the two above.
//
// NO ACTOR AND NO READ STATE, which tells a seat's deps from the operator
// surface's. No actor, so a write is attributed to the turn's own seat (a model
// that could name its author could speak as anybody); no read state, because a
// cursor is a PERSON's attention and a woken seat is not browsing. See
// [builtin.ChatDeps.ReadState].
func (e *Engine) chatDeps(c *Company) builtin.ChatDeps {
	rooms, ok := e.chatHalves()
	if !ok {
		return builtin.ChatDeps{}
	}
	return builtin.ChatDeps{
		Reader: rooms,
		Writer: rooms,
		// THE SAME INTERSECTION THE WIKI TAKES ([seatMentions]): a handle nobody
		// holds is dropped rather than becoming an undeliverable wake.
		Mentions: seatMentions{org: c.Org},
		// THE ROUTER'S SEARCH resolves the rooms the viewer may read on the
		// serving data node; the visible set is never something a tool hands in
		// ([builtin.ChatSearcher]).
		Search: e.router.ChatSearch(),
		// THE SESSION FLOOR IS THE WAIT, as for the tracker's and wiki's halves.
		Await: e.router.Await,
	}
}

// chatHalves is this node's chat seams, on the terms [Engine.trackerHalves]
// gives: the router's on every node. False where the company's chat is not
// Chatlet, and where this node hands out no chat writer ([Engine.writers]),
// which then hands out no chat tools rather than tools whose every write is
// refused.
func (e *Engine) chatHalves() (estate.Chat, bool) {
	if _, _, writes := e.writers(e.nativeHalves()); !writes {
		return estate.Chat{}, false
	}
	return e.router.Chat(), true
}

// ---- what the operator's surfaces are given ------------------------------ //

// OperatorWork is the tracker's read side as the operator's surfaces reach it —
// the dashboard's and the REST routes' questions, and the operator's own MCP —
// or false where this company runs no Worklet here.
//
// THE ROUTER'S, ON EVERY NODE, as a seat's tools get it
// ([Engine.trackerHalves]), never this node's own copy read directly: a data
// node whose copy is out of service must answer its operator from a peer's
// copy, as it answers its seats, not from rows it would not vouch for.
func OperatorWork(e *Engine) (estate.Work, bool) {
	reads := OperatorNative(e)
	return reads.Work, reads.Tracker
}

// OperatorReads is every native read seam the operator's surfaces are handed —
// the tracker, the knowledge base and the ranked item search — as ONE READING
// of this node's runtime found them.
//
// ONE READING, because a surface takes all three at once for one answer, and
// separate loads could straddle a runtime publish and mix two moments in one
// answer. It costs one or two atomic loads and no lock, I/O or allocation.
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
	// Search says the ranked item search is served, through Work. It needs the
	// tracker AND this node's index, which is built under either backend, so on
	// a company with a vendor tracker it holds pages and no task.
	// [WorkSearcher]'s rule.
	Search bool
}

// OperatorNative is [OperatorReads] for e at this instant, the one reading
// [OperatorWork], [OperatorPages] and [WorkSearcher] each view, so the rule is
// stated once. Which halves run is [Engine.nativeHalves]' answer, so the
// operator is handed a half exactly where seat admission waits on it.
func OperatorNative(e *Engine) OperatorReads {
	if e.router == nil {
		return OperatorReads{}
	}
	run := e.nativeHalves()
	out := OperatorReads{
		Tracker: run.tracker,
		Wiki:    run.wiki,
		// The tracker as well as the index (see Search). A NODE WITHOUT `data`
		// (`own` nil) asks a data node for the ranked search, so it serves one
		// wherever it serves the board.
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
// whichever party each call names, or false where this node hands out no
// tracker writer ([Engine.writers]).
func OperatorWorkWriter(e *Engine) (func(estate.Actor) estate.WorkWriter, bool) {
	if _, ok := e.trackerHalves(); !ok {
		return nil, false
	}
	return e.router.WriterAs, true
}

// OperatorPages is the knowledge base's read side as the operator's surfaces
// reach it, through the router for [OperatorWork]'s reason, or false where this
// company runs no Doclet here. A page's backlinks ([estate.Pages.LinkedFrom])
// read the tracker's copy too, and are answered only where both serve.
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

// OperatorChat is chat's read side as the operator's surfaces reach it, through
// the router for [OperatorWork]'s reason — or false where this company's chat
// is not Chatlet here.
func OperatorChat(e *Engine) (estate.Chat, bool) {
	if run := e.nativeHalves(); !run.chat || e.router == nil {
		return estate.Chat{}, false
	}
	return e.router.Chat(), true
}

// OperatorChatWriter is chat's write side for those surfaces — the act
// transport's chat tools, the erase, the prune and a room's retention — or
// false where this node hands out none ([Engine.writers]).
func OperatorChatWriter(e *Engine) (estate.Chat, bool) {
	if _, ok := e.chatHalves(); !ok {
		return estate.Chat{}, false
	}
	return e.router.Chat(), true
}

// OperatorChatSearch is the chat search those surfaces reach, through the
// router — the viewer's rooms resolved on whichever data node reads its index —
// or false where this company's chat is not Chatlet here.
func OperatorChatSearch(e *Engine) (estate.ChatSearch, bool) {
	if run := e.nativeHalves(); !run.chat || e.router == nil {
		return estate.ChatSearch{}, false
	}
	return e.router.ChatSearch(), true
}

// AwaitEstate raises the router's session floor to at ([estate.Router.Await]),
// so whichever holder answers this node's next read has applied that far: this
// node's own applier may not be the one answering. It returns at once; the wait
// is the next read's.
func (e *Engine) AwaitEstate(ctx context.Context, at statelog.Position) error {
	if e.router == nil {
		return nil
	}
	return e.router.Await(ctx, at)
}

// reservedContainers are the containers a seat's own writes are held back
// from, each under its own rule, and every container the company DECLARES (the
// only ones a page is created in, by anybody); see [doclet.Reserved].
//
// THE DECLARED SPACES ARE [chartContainers]'s, the list the containers are made
// from on every apply; a second walk of the chart could refuse a page a space
// whose container exists, or create one in a space that has none.
//
// NOTHING IS DECLARED by a node running no company, the meaningful zero: every
// create is refused until there is a chart.
func reservedContainers(current *Company) doclet.Reserved {
	if current == nil || current.Config == nil {
		return doclet.Reserved{}
	}
	held := doclet.Reserved{
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
// permissive (`@here`, `@all`, unknown handles) so the grammar need not know
// the company; here a name that is not a seat is dropped, so no notification
// is made that nobody can deliver.
type seatMentions struct{ org *org.Organization }

// LiveLeads and LiveUnits are the chart seams a surface outside this package
// needs, resolved PER CALL against the epoch current when the tool runs.
//
// Exported for the operator surface, which without them woke nobody on an
// unassigned task (the lead fallback) and showed every project teamless.
func LiveLeads(e *Engine) worklet.Leads { return liveLeads{engine: e} }

// LiveUnits is the tracker's unit seam over the engine's current chart.
func LiveUnits(e *Engine) worklet.Units { return liveUnits{engine: e} }

// LiveChannels is the chart's chat surfaces and unit channels, read per call,
// for a surface built once that outlives every apply (the person-facing tool
// catalogue, whose `decision.inform` a nil directory would refuse).
func LiveChannels(e *Engine) builtin.ChannelDirectory { return liveChannels{engine: e} }

// LiveDefaultProject is [ProjectOfSeat] over the engine's CURRENT chart, read
// per call, for the surfaces an apply does not rebuild: the operator surface,
// whose `create_work_item` files a PERSON's work, and the dashboard's
// `viewer`, which says where it lands. "" before any epoch is running, and for
// a seat whose team and every team above it own no project: the tool refuses.
//
// ONE DERIVATION FOR BOTH, so the viewer can never promise one project while
// the create files into another.
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
// For the surfaces that OUTLIVE an epoch (the scheduler's loop, the operator's
// surface), so an apply moving the clock moves them on their next read. A
// seat's own tools take the clock their turn pinned instead; see
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
// Exported so the OPERATOR surface, where a person writes AS their seat
// ([iam.ActorFor]), uses this rule rather than a second copy. Keyed on the
// actor's handle, as [LiveDefaultProject] is: a credential nobody is bound
// through matches no seat, so its write names a container or is refused
// naming the argument, never filed somewhere guessed.
func LiveSeatSpace(e *Engine) func(handle string) string {
	return func(handle string) string {
		c := e.Company()
		if c == nil || handle == "" {
			return ""
		}
		return SpaceOfSeat(c.Org, handle)
	}
}

// LiveMentions resolves @-mentions against the chart CURRENT when the comment
// is written, rather than the one that built the caller.
//
// The seat path captures its org deliberately (a seat's tools are rebuilt on an
// apply), but the operator surface is not rebuilt for a new chart, so it reads
// the engine per call. Exported so the OPERATOR MCP uses this rule rather than
// a second copy.
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
		// A HUMAN SEAT COUNTS: a person mentioned is notified through whatever
		// surface their contact declares, so the lookup is over every seat.
		if m.org.SeatByHandle(name) != nil {
			out = append(out, name)
		}
	}
	return out
}

// nudgeSkills asks the skill sync to re-read the tool-skill container.
//
// A nudge, not a read: it runs from the pages applier's post-commit hook on
// the apply loop's goroutine, where a read would hold every later change
// behind a page walk; the node's one sync loop coalesces nudges. It
// ANNOUNCES, because a node without `data` runs no applier.
//
// NAMED AS DOCLET'S, because the pages applier outlives an apply that moves
// the skills to Confluence, and the loop ignores a refresh from a backend its
// source is not on.
func (e *Engine) nudgeSkills() { e.skillSync.Announce(skillBackend(config.KnowledgeDoclet)) }

// walkRemoteSkills reads the tool-skill container through a data node, for a
// node that holds none.
//
// STALE, for the reason [Engine.walkDocletSkills] gives, one hop further: the
// walk runs because a data node APPLIED the change, and the floor this node
// carries is its own writes', so the answering node is at or past both.
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

// walkDocletSkills reads the tool-skill container out of this node's own
// applied pages.
//
// It waits for hydration first: a walk over a container not yet applied
// through is a PARTIAL set, and the registry replaces wholesale, so reading
// early would silently delete every skill the walk missed.
func (e *Engine) walkDocletSkills(ctx context.Context, container string) ([]skills.Page, error) {
	if !e.awaitHydration(ctx) {
		return nil, ctx.Err()
	}
	// STALE is the strongest level this walk could honestly name, not a saving:
	// [Engine.nudgeSkills] runs from the applier's POST-COMMIT hook, so the
	// record that woke the sync loop is already in this node's committed prefix.
	// The causality is LOCAL, so no barrier buys anything.
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

// hydrationPoll is how often the first skill read checks whether this node's
// copy has caught up with its logs.
//
// The wait is bounded by a boot reconcile (hundreds of milliseconds to
// minutes), so polling costs a handful of atomic reads, and a condition
// variable would be a second thing to keep correct for a once-per-process wait.
const hydrationPoll = 250 * time.Millisecond

// searchPeers is the fleet half of the knowledge search's fan-out.
//
// NIL WHEN THERE IS NO QUEUE, a legal deployment: an embedded engine with no
// broker holds the whole corpus and takes every bucket.
func (e *Engine) searchPeers() search.Peers {
	if e.backends == nil || e.backends.Queue == nil {
		return nil
	}
	return search.Broker{Queue: e.backends.Queue}
}

// liveNodes answers which nodes are in the fleet NOW: every node a fan-out may
// hand a share of a question to — a search's bucket range, a history read.
//
// FROM THE LEASE VIEW, not the positions register: a position row outlives a
// node gone for hours, while a lease expires, and a dead node in the roster
// would cost every fanned answer a partial result.
//
// EVERY NODE, data or not: this is the fleet's SIZE, which the health envelope
// reports. A fan-out reads [Engine.dataRoster] instead.
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
// AN ERROR IS A READ THAT DID NOT HAPPEN, never zero nodes: the health
// envelope leaves its count out rather than claim "0 nodes".
//
// THE CALLER BOUNDS IT: this key-iterating scan is not covered by the NATS
// client's per-API timeout. The health envelope hands it [ProbeReadBudget].
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
// Without it `search_slow`, `prefetch_slow`, `search_degraded` and
// `search_scoped` would be permanently silent, which looks exactly like
// nothing wrong.
//
// THE PATH IS THE QUERY'S OWN ([search.FanQuery.Prefetch]), so a turn's
// prefetch and a deliberate search are two series held to two budgets.
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
// `path` attribute names it, and the name the alarm reading splits it by
// ([retention.observed]), so label and budget are one value.
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
// THE SERVED MODE, never a constant, so cheap keyword-only scans do not hide a
// slow semantic half in the figure the interactive target is read from.
func searchRung(answer search.Answer) string {
	if answer.Served == "" {
		return "none"
	}
	return string(answer.Served)
}

// semanticState is whether the meaning half ran, as the degraded-search alarm
// counts it.
//
// THREE VALUES: a half ASKED FOR that did not run (a failed vector scan, a
// query vector the provider could not compute) is `skipped`, the numerator of
// `search_degraded`. A half nobody asked for (a keyword search, no embeddings
// provider) is `off`; counting it as skipped would hold the alarm red for the
// life of such a deployment.
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
// The concurrency level no histogram can show: the supported-corpus table has
// a row per concurrency (see [search.SemanticScanBudget]), nine at once is past
// the last, and a duration histogram records what scans cost, not how many
// competed for the disk.
//
// SET RATHER THAN ADDED, on both edges, because it is a gauge: the rolling
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
