package host

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/gitletfault"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// Estate is what the receive pipeline and the fetch path ask the code domain:
// estate.CodeSeam on every node, or a single-node harness's in-process
// committer and reader (internal/gitlet/gitlettest).
type Estate interface {
	Resolve(ctx context.Context, q gitlet.ResolveQuery) (gitlet.RepoRef, error)
	RepoState(ctx context.Context, q gitlet.StateQuery) (gitlet.RepoSnapshot, error)
	Propose(ctx context.Context, p gitlet.Proposal) (gitlet.ProposeResult, error)
	PushOutcome(ctx context.Context, q gitlet.OutcomeQuery) (gitlet.PushOutcome, error)
	// Read reads Gitlet's rows by kind: a check's box's attempt
	// (gitlet.ReadCheckBox, checkadmit.go), a merge_change's change and its
	// target's merge gates (merge.go).
	Read(ctx context.Context, q gitlet.ReadQuery) (gitlet.ReadAnswer, error)
	// Merge asks the committer to decide a merge_change
	// (gitlet.Committer.ProposeMerge): like Propose, a door only the one
	// landing takes (TestEveryPushTakesTheOneLanding).
	Merge(ctx context.Context, p gitlet.MergeProposal) (gitlet.MergeResult, error)
	// Queue decides an enqueue: a merge_change onto a target that merges
	// through its queue builds nothing and is enqueued instead.
	Queue(ctx context.Context, p gitlet.QueueProposal) (gitlet.QueueResult, error)
}

// Boxes is what admits a box (boxadmit.go): an executor's tokens and the run
// rows its grants come from, and a check's tokens, whose box is admitted by
// its attempt's row in the Estate (checkadmit.go).
type Boxes struct {
	Tokens *BoxTokens
	Runs   Runs
	Checks *CheckTokens
}

// Runs is the executors' rows as a box's admission reads them: the sandbox
// pending store on the coordination store, which every node reaches.
type Runs interface {
	// Run is the row of turnID's run: found false for none; an error is the
	// store unreachable, UNKNOWN, never "no run".
	Run(ctx context.Context, turnID string) (BoxRun, bool, error)
	// RaiseFloor raises the run's code-log floor to at least pos, only while
	// ref is the row's current launch: a lower pos, a superseded launch or an
	// ended run is no write and no error. An error is the store unreachable
	// or a race lost past its retries.
	RaiseFloor(ctx context.Context, ref BoxRunRef, pos statelog.Position) error
	// Stopped is who paused seat asking its running work to stop
	// (coord.SeatPause.StopRunning), "" for none, as this node's pause watch
	// holds it. An error is a node that has not read the pauses yet: UNKNOWN,
	// never "not stopped".
	Stopped(ctx context.Context, seat string) (by string, err error)
	// Hold adds keys (each a seat branch's gitlet.RefHoldKey) to the code
	// holds of ref's run in one write, BEFORE the push naming them is
	// proposed: a parked run depends on what its box pushed. Holds are the
	// TURN'S, unioned across launches, so ref's launch is not compared: a
	// superseded launch keeps a branch longer, the safe direction. A row gone
	// or with its ending decided is nil, no write: an ended run depends on
	// nothing. sandbox.ErrCodeHoldsFull is the row at its bound, nothing
	// written; any other error is UNKNOWN, and the push is refused rather
	// than sent unheld.
	Hold(ctx context.Context, ref BoxRunRef, keys []string) error
}

// BoxRun is one executor as the host judges a box by it.
type BoxRun struct {
	// Ref is the row's turn id and its CURRENT launch.
	Ref BoxRunRef
	// Live is a row whose job is in its box: launching or running, and no
	// ending decided.
	Live bool
	// Status is the row's status, for a refusal's sentence.
	Status string
	// Seat is the handle of the seat whose run it is.
	Seat string
	// Ceiling is the colleague seat whose ask woke the launching turn (the
	// requester ceiling the launch resolved repositories under): its grant
	// bounds the box's beside the seat's, refusing a read, or a push outside
	// the seat's own namespace, it could not make (gitlet.Grant.Under). ""
	// for none.
	Ceiling string
	// WorkingFor is whom the launching turn works for (internal/sandbox's
	// PendingRun.CodeWorkingFor): the box's pushes are the turn's writes made
	// late, so each carries it to its committer (gitlet.Proposal.WrittenFor).
	// A CREDENTIAL acting as no seat bounds the box like a colleague, by its
	// own grant (reads everything, pushes nothing), so the box pushes only the
	// seat's own work (boxadmit.go's runCeiling). Nil for a run working for
	// its own seat or launched before the field. A value this build does not
	// know is a newer build's, carried as it stands: its committer answers it
	// as another build's node (gitlet's unknownToThisBuild), and a kind
	// admission cannot bound by is 503 carrier_unknown. One no checkout writes
	// (a stranger, the zero kind, an app-less credential) has the box refused
	// every request (gitlet.RunCarrierInvalid).
	WorkingFor *types.WorkingFor
	// Repos are the repositories the run was given.
	Repos []BoxGrant
	// Floor is the run's SESSION on the code log: a position at or past
	// everything its box has been shown and has pushed. Zero for none,
	// which a box reads past LINEARIZABLE.
	Floor statelog.Position
	// Provenance stamps the run's pushes, as the row holds it: TurnID the
	// turn, RunID its launch, Work* its work item (what a change its push
	// opens carries the item by), Model and Harness its launch's. The host
	// fits each to the code domain's bound where it stamps a push
	// (admission.principal).
	Provenance gitlet.RunProvenance
	// Holds are the turn's code holds as admission read them, keys this
	// build may not decode: a push holds only the seat branches it names not
	// already among them ([admission.holdRefs]), so re-pushing a held branch
	// costs the run store nothing. A set because a push names up to
	// gitlet.MaxCommandsPerPush refs against up to sandbox.MaxCodeHolds keys.
	Holds map[string]bool
}

// BoxGrant is one repository a run was given.
type BoxGrant struct {
	RepoID string
	// Name is the repository's name at launch: what the box's clone URL
	// carries and its request is matched by.
	Name string
	// Access is what the run's code_checkout asked for, capped by the seat's
	// own grant.
	Access gitlet.Access
	// Branch is the run's branch, refs/heads/…, for the brief and the log.
	Branch string
}

// grantNamed is the grant of the repository the run was given under name, and
// whether it was given one.
func (r BoxRun) grantNamed(name string) (BoxGrant, bool) {
	for _, g := range r.Repos {
		if g.Name == name {
			return g, true
		}
	}
	return BoxGrant{}, false
}

// Timing is every duration the host waits on that a test must scale
// (jsprovision.Timing's idiom): the engine passes Production, and New refuses
// a zero field by name.
type Timing struct {
	// PendingGrace is how long a `pending` push is waited on for its row.
	PendingGrace time.Duration
	// PendingFirst is the first wait between two outcome reads, doubling to
	// PendingMax.
	PendingFirst, PendingMax time.Duration
	// Keepalive is the interval of the session's stderr keepalive.
	Keepalive time.Duration
	// Handshake is how long a hook connection has to name its session.
	Handshake time.Duration
	// Stall is how long a transfer's git process may read and write nothing
	// before the pool ends it (gitproc.Cmd.Stall). Git keeps its own
	// five-second keepalives at any Timing, and a healthy receive-pack may be
	// silent between them (unpack, connectivity check, hook, decide and
	// apply), so a case setting it below five seconds holds only a stand-in
	// git to it, never a real transfer.
	Stall time.Duration
	// SpoolWait is the longest a fetch request waits for spool room before
	// it is answered 503 (spool.go).
	SpoolWait time.Duration
}

// Production is the timing every node runs with.
//   - PendingGrace is statelog.StallGrace: a record unapplied longer is one
//     waiting will not tell this node about.
//   - PendingFirst is 250 ms: an apply takes 5–15 ms, so most pushes are
//     found on the first read; PendingMax 2 s, thirty estate reads per stuck push.
//   - Keepalive is git's own keepAlive default; Handshake 5 s, for a same-host hook.
//   - Stall is gitproc.TransferStall, a minute: twelve git keepalives missed.
//   - SpoolWait is 55 s, a Keepalive under the minute an operator's balancer
//     may idle a quiet /git/ connection out at (docs/guides/deployment.md), so
//     a spooling fetch's 503 and Retry-After beat that wordless cut however
//     late a loaded node's timer fires; less refuses checkouts served seconds
//     later.
var Production = Timing{
	PendingGrace: statelog.StallGrace,
	PendingFirst: 250 * time.Millisecond,
	PendingMax:   2 * time.Second,
	Keepalive:    keepalive,
	Handshake:    5 * time.Second,
	Stall:        gitproc.TransferStall,
	SpoolWait:    gitproc.TransferStall - keepalive,
}

// keepalive is Production.Keepalive: git's own uploadpack.keepAlive and
// receive.keepAlive default.
const keepalive = 5 * time.Second

// Options are how a Server is built. Every field but Current, ExternalBase,
// Faults, Logger, Metrics and Now is required, and New refuses a missing one
// by name. FETCHED IS REQUIRED LIKE AUDIT: an optional seam is the one a
// caller forgets, and an unrecorded read answers "who read it" short.
type Options struct {
	// Cache is this node's git cache: every directory a git process here
	// reads, and the scratch repositories pushes are received in.
	Cache *cache.Cache
	// Git runs every git process.
	Git *gitproc.Pool
	// Estate is the code domain's rows and its committer.
	Estate Estate
	// Objects stores a push's pack bundle (*objstore.Store in the engine),
	// held as gitlet.Putter because gitlet.StoreBundle, every pack writer's
	// one store of a bundle and its seal, calls it.
	Objects gitlet.Putter
	// Audit records one push through the operator surface's audit, which
	// outlives the request.
	Audit func(ctx context.Context, e types.OperatorActed)
	// Fetched records, before the response ends, each fetch of a
	// `visibility: unit` repository whose answer carried a pack (fetch.go) and
	// each download of one that began or was answered 304 (download.go), into
	// the node's reads ledger (internal/gitlet/readaudit). It returns at once,
	// so no answer waits on the broker, and the record outlives the request: a
	// client that hung up after its pack still read the repository.
	Fetched func(ctx context.Context, e types.CodeFetched)
	// Limits is the CURRENT epoch's code.limits.
	Limits func() gitlet.Limits
	// DeletedRetention is the CURRENT epoch's code.deleted_retention_days —
	// what a deleted repository's refusal names its purge by.
	DeletedRetention func() time.Duration
	// Roster is the CURRENT epoch's chart, read per request (the query
	// server's own), that judges a seat's grant for an engine write (write.go)
	// and a coding box (boxadmit.go). Nil is a node with no chart yet, which
	// builds no seat's write (gitlet.UnreadyNoChart) and admits no box.
	Roster func() gitlet.Roster
	// Signer names an engine-built commit's author and committer from the
	// serving node's epoch (write.go's Signer).
	Signer Signer
	// Share is the node's ONE seat git-work share (gitlet.GitShare), the
	// query server's own: an engine write's build holds one unit, given back
	// before its landing.
	Share *gitlet.GitShare
	// Resyncs is the node's ONE set of re-sync buckets (gitlet.NewResyncs),
	// the query server's own: an engine write's linearizable re-judgement of
	// a seat's refusal draws on its turn's bucket, as a git read's second look
	// does, and past it answers from the seat's session, saying so; a coding
	// box draws on its run's bucket, a check's box on its attempt's
	// (boxlimit.go). ONE, because the census gives a seat one term on the log.
	Resyncs *gitlet.Resyncs
	// Current reports whether this node serves the company's configuration:
	// false on a node confirmed behind, which builds no SEAT's write and
	// admits no coding box from a replaced chart (gitlet.UnreadyBehindConfig).
	// nil is current.
	Current func() bool
	// Boxes admits a box to /box/git/. Every part is required: a Gitlet node
	// serves boxes whatever its company runs, since any node may have launched
	// a box's run or made a check's box.
	Boxes Boxes
	// NodeID is this node, which a proposal names as the push's receiver.
	NodeID string
	// ExternalBase is `api.external_url` without its trailing slash
	// (config.API.ExternalBase()), on which the line git prints for a push's
	// change links its page (changeLink). Empty names the change alone: never
	// on an engine node serving a push, which arrives through the API listener
	// `api.external_url` is required beside.
	ExternalBase string
	// Timing is every scaled duration (Production in the engine).
	Timing Timing
	// Faults is nil in production (internal/gitlet/gitletfault).
	Faults gitletfault.Injector
	// Logger nil is the package's own (component gitlet.host).
	Logger *slog.Logger
	// Metrics nil records nothing.
	Metrics *metrics.Recorder
	// Now nil is time.Now.
	Now func() time.Time
}

// ErrStopping is a request to Gitlet while it stops: the API answers
// it 503 with Retry-After StoppingRetryAfter.
var ErrStopping = errors.New("host: Gitlet is stopping")

// keepaliveTicker paces a push session's keepalive (session.go): a case's
// keepaliveTicks, else a ticker of every. A NIL HOOK IS PRODUCTION'S TICKER,
// so no way of building a Server (New, or a case's literal) leaves the
// keepalive nothing to tick on.
func (s *Server) keepaliveTicker(every time.Duration) (<-chan time.Time, func()) {
	if s.keepaliveTicks != nil {
		return s.keepaliveTicks(every)
	}
	t := time.NewTicker(every)
	return t.C, t.Stop
}

// ErrOptions is an Options value New refuses, wrapped naming every field.
var ErrOptions = errors.New("host: refused options")

// Server is one node's git surface.
type Server struct {
	o   Options
	log *slog.Logger
	now func() time.Time

	// life is the HOST's lifetime, the one stored context (as the
	// committer's is): Stop ends it, and every request's work context joins
	// it, so a stop reaches every git process and hook in flight.
	life context.Context
	end  context.CancelFunc

	mu       sync.Mutex
	stopped  bool
	inflight sync.WaitGroup

	relay  *relay
	heads  *allowance
	spools *spools

	// verifies is every verification this node keeps (verify.go).
	verifies verifications

	// requestMemory is RequestMemoryBytes, a field so the suite can reach
	// the spool without megabytes of fixtures (export_test.go).
	requestMemory int

	// checkProposal is gitlet.Proposal.ValidateUnstored, checked before the
	// bundle is stored: a field so the suite can stand in a receiver's bug,
	// which no push reaches (export_test.go).
	checkProposal func(gitlet.Proposal) error

	// runGit is o.Git.Run, which starts every git process: a field so the
	// suite can read each Cmd the pool is handed (export_test.go), since only
	// that Cmd proves a call site formed a push's deadlines from what the push
	// sends and its repository holds.
	runGit func(context.Context, gitproc.Cmd) (gitproc.Result, error)

	// keepaliveTicks, when set, replaces a push session's ticker
	// ([Server.keepaliveTicker]) so the suite sends each tick (export_test.go):
	// the client sees the lines only once the push ends, so under a clock
	// their count is a loaded runner's. Nil, as in production, is a ticker.
	keepaliveTicks func(every time.Duration) (ticks <-chan time.Time, stop func())

	// writes is every engine write running here by push id, which a second
	// write of the same id waits for (write.go's in-flight join).
	writesMu sync.Mutex
	writes   map[string]*inflightWrite
	// The engine write's bounds, fields so the suite can scale them
	// (export_test.go): syncWait its wait on its sync and outcome
	// (statelog.ReadBudget), buildBudget its build's git time
	// (gitlet.EngineWriteKind's BuildDeadline), and a chain's (chain.go)
	// chainMerges (gitlet.MergeBuildDeadline), chainPack
	// (gitlet.ChainPackDeadline) and chainObjects, its entries' uncompressed
	// bytes (gitlet.MaxChainObjectBytes).
	syncWait     time.Duration
	buildBudget  func(gitlet.EngineWriteKind) time.Duration
	chainMerges  time.Duration
	chainPack    time.Duration
	chainObjects int64

	// archives are the node's archive places and the principals making a
	// ZIP (download.go); the rest are its bounds, fields so the suite can
	// scale them (export_test.go).
	archives          *archivePlaces
	archiveDeadline   time.Duration
	archiveFree       int64
	archiveMaxEntries int64
	archiveMaxBytes   int64
	archiveMaxNames   int64
	archiveMaxName    int64
	archiveBound      func(treeMeasure) int64

	// runRequests is every executor's requests in flight here, by its
	// launch, under runsMu (boxlimit.go).
	runsMu      sync.Mutex
	runRequests map[string]int
}

// New builds a Server, refusing every required field missing, by name, in one
// error listing them all.
func New(o Options) (*Server, error) {
	var missing []string
	need := func(absent bool, name string) {
		if absent {
			missing = append(missing, name)
		}
	}
	need(o.Cache == nil, "Cache")
	need(o.Git == nil, "Git")
	need(o.Estate == nil, "Estate")
	need(o.Objects == nil, "Objects")
	need(o.Audit == nil, "Audit")
	need(o.Fetched == nil, "Fetched")
	need(o.Limits == nil, "Limits")
	need(o.DeletedRetention == nil, "DeletedRetention")
	need(o.Roster == nil, "Roster")
	need(o.Signer == nil, "Signer")
	need(o.Share == nil, "Share")
	need(o.Resyncs == nil, "Resyncs")
	need(o.Boxes.Tokens == nil, "Boxes.Tokens")
	need(o.Boxes.Runs == nil, "Boxes.Runs")
	need(o.Boxes.Checks == nil, "Boxes.Checks")
	need(strings.TrimSpace(o.NodeID) == "", "NodeID")
	need(o.Timing.PendingGrace <= 0, "Timing.PendingGrace")
	need(o.Timing.PendingFirst <= 0, "Timing.PendingFirst")
	need(o.Timing.PendingMax <= 0, "Timing.PendingMax")
	need(o.Timing.Keepalive <= 0, "Timing.Keepalive")
	need(o.Timing.Handshake <= 0, "Timing.Handshake")
	need(o.Timing.Stall <= 0, "Timing.Stall")
	need(o.Timing.SpoolWait <= 0, "Timing.SpoolWait")
	if len(missing) > 0 {
		return nil, fmt.Errorf("%w: %s missing or zero", ErrOptions, strings.Join(missing, ", "))
	}
	s := &Server{o: o, log: o.Logger, now: o.Now, requestMemory: RequestMemoryBytes,
		checkProposal: gitlet.Proposal.ValidateUnstored, runGit: o.Git.Run, syncWait: statelog.ReadBudget,
		buildBudget: gitlet.EngineWriteKind.BuildDeadline, chainMerges: gitlet.MergeBuildDeadline,
		chainPack: gitlet.ChainPackDeadline, chainObjects: gitlet.MaxChainObjectBytes}
	if s.log == nil {
		s.log = logging.Get("gitlet.host")
	}
	if s.now == nil {
		s.now = time.Now
	}
	transfers := o.Git.Slots(gitproc.ClassTransfer)
	s.heads = newAllowance(RequestMemoryAllowance(transfers))
	s.spools = newSpools(spoolSizesFor(transfers))
	s.archives = newArchivePlaces(Archives(o.Git.Slots(gitproc.ClassRead)), gitproc.SlotWait)
	s.archiveDeadline, s.archiveFree, s.archiveBound = ArchiveDeadline, ArchiveFreeBytes, archiveBound
	s.archiveMaxEntries, s.archiveMaxBytes, s.archiveMaxNames = MaxArchiveEntries, MaxArchiveTreeBytes,
		MaxArchiveNameBytes
	s.archiveMaxName = zipNameMax
	s.life, s.end = context.WithCancel(context.Background())
	return s, nil
}

// Start opens the hook relay's socket and accept loop and starts the cache's
// background work, both owned until Stop. An error (the socket cannot be
// made, or its path is too long) fails the engine's boot.
func (s *Server) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return ErrStopping
	}
	if s.relay != nil {
		return nil
	}
	r, err := listen(s)
	if err != nil {
		return err
	}
	s.relay = r
	// THE HOST'S LIFETIME, not the caller's: the relay and the cache's
	// background work run until Stop, however long Start's caller lives.
	go r.serve(s.life)      //nolint:contextcheck // see above
	s.o.Cache.Start(s.life) //nolint:contextcheck // see above
	return nil
}

// StopBudget is how long a caller of [Server.Stop] gives the host's cancelled
// work to end: THREE KILL GRACES (gitproc.KillGrace): SIGTERM to SIGKILL, then
// to closing a straggler's pipe, then what follows a process's end (an answer
// to a gone client, an upload abandoned under its bound). Declared beside
// Stop so every caller stopping a host as the engine does (its teardown,
// internal/gitlet/gitlettest) gives it the same time.
const StopBudget = 3 * gitproc.KillGrace

// Stop refuses every new git request (503, Retry-After StoppingRetryAfter);
// cancels every in-flight request's work (a receive-pack and its hook's
// connection with it); waits for the in-flight handlers until ctx ends; shuts
// the relay down; and stops the cache. A second call waits again for whatever
// still runs. A caller bounds ctx by [StopBudget].
func (s *Server) Stop(ctx context.Context) {
	s.mu.Lock()
	s.stopped = true
	r := s.relay
	s.mu.Unlock()
	s.end()
	waited := make(chan struct{})
	go func() {
		s.inflight.Wait()
		close(waited)
	}()
	select {
	case <-waited:
	case <-ctx.Done():
		s.log.Warn("code_host_stop_unfinished", "error", ctx.Err(),
			"detail", "git requests were still running when the stop's wait ended")
	}
	if r != nil {
		r.shutdown(ctx)
	}
	s.o.Cache.Stop(ctx)
}

// begin admits one request to a running host, or answers it 503. It returns
// the request's work context (the client's, joined to the host's lifetime so
// a Stop reaches it) and what ends it.
func (s *Server) begin(parent context.Context, w http.ResponseWriter) (context.Context, func(), bool) {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		stopping(w)
		return nil, nil, false
	}
	s.inflight.Add(1)
	s.mu.Unlock()
	ctx, cancel := context.WithCancel(parent)
	//nolint:contextcheck // The host's lifetime ends the request's work too: Stop must reach every git process in flight.
	stop := context.AfterFunc(s.life, cancel)
	return ctx, func() {
		stop()
		cancel()
		s.inflight.Done()
	}, true
}

// fault hits a push point: nil when no injector is set.
func (s *Server) fault(ctx context.Context, p gitletfault.Point, at int64) error {
	if s.o.Faults == nil {
		return nil
	}
	return s.o.Faults.Hit(ctx, p, at)
}

// limits is the current epoch's code.limits.
func (s *Server) limits() gitlet.Limits { return s.o.Limits() }
