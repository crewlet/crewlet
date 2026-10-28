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

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// Estate is what the receive pipeline and the fetch path ask the code domain,
// wherever its rows live — declared here, by the consumer: estate.CodeSeam
// satisfies it on every node, and a single-node harness's in-process
// committer and reader do (internal/code/codetest).
type Estate interface {
	Resolve(ctx context.Context, q code.ResolveQuery) (code.RepoRef, error)
	RepoState(ctx context.Context, q code.StateQuery) (code.RepoSnapshot, error)
	Propose(ctx context.Context, p code.Proposal) (code.ProposeResult, error)
	PushOutcome(ctx context.Context, q code.OutcomeQuery) (code.PushOutcome, error)
}

// Timing is every duration the host waits on that a test must be able to
// scale (jsprovision.Timing's idiom): Production is the value the engine
// passes, and a field left zero is refused by name.
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
	// Stall is how long a transfer's git process — an advertisement,
	// upload-pack, receive-pack — may read and write nothing before the pool
	// ends it (gitproc.Cmd.Stall).
	Stall time.Duration
	// SpoolWait is the longest a fetch request waits its turn for room in
	// the spool allowance before it is answered 503 (spool.go).
	SpoolWait time.Duration
}

// Production is the timing every node runs with.
//
//   - PendingGrace is statelog.StallGrace (60 s): the grace after which the
//     framework itself calls an applied prefix stalled, so a record unapplied
//     for longer is one this node will not be told about by waiting.
//   - PendingFirst is 250 ms: an append resolves in about 1.5 ms and an apply
//     in 5–15 ms on a real cluster, so most pending pushes are found on the
//     first read.
//   - PendingMax is 2 s: sixty seconds at two-second steps is thirty reads per
//     stuck push — each an estate read, not a log append.
//   - Keepalive is 5 s: git's own uploadpack.keepAlive and receive.keepAlive
//     default, the interval git chose for the same balancer problem.
//   - Handshake is 5 s: the hook dials a socket on the same host, so a
//     connection that has not named its session in five seconds is not a hook.
//   - Stall is gitproc.TransferStall (a minute, objstore.Stall): git's
//     keepalives are five seconds apart, so twelve of them missed is a process
//     that stopped — and a case that holds a stand-in git still need not wait
//     a minute to see it ended.
//   - SpoolWait is gitproc.TransferStall less a Keepalive: 55 s. A fetch
//     waiting for room to spool has written its client nothing and reads
//     nothing more of its request, so its connection is QUIET, and what ends
//     a quiet connection from outside is a balancer's idle timeout — which
//     docs/guides/deployment.md has an operator set at a minute or more on
//     /git/, the silence the node itself allows a git transfer. Answered a
//     keepalive interval before that minute, the interval git keeps a
//     transfer alive at for the same balancer, its 503 and the Retry-After
//     in it reach a client behind a balancer that cuts at exactly a minute,
//     however late a loaded node's timer fires, rather than the balancer's
//     own cut, which git reports with no word of why. Waiting less would
//     refuse checkouts the node could have served a few seconds later; there
//     is no waiting longer behind such a balancer. A request whose own
//     context ends sooner stops waiting then.
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

// Options are how a Server is built. Every field but Current, Faults, Logger,
// Metrics and Now is required, and New refuses a missing one by name.
//
// FETCHED IS REQUIRED LIKE AUDIT: a code host that served a `visibility: unit`
// repository's contents to somebody and recorded nothing would answer "who
// read it" short, and an optional seam is exactly the one a caller forgets.
type Options struct {
	// Cache is this node's git cache: every directory a git process here
	// reads, and the scratch repositories a push is received in.
	Cache *cache.Cache
	// Git runs every git process.
	Git *gitproc.Pool
	// Estate is the code domain's rows and its committer.
	Estate Estate
	// Objects stores a push's pack bundle — *objstore.Store in the engine —
	// held as code.Putter because code.StoreBundle, the one store of a bundle
	// and its seal every writer of a stored pack takes, is what calls it.
	Objects code.Putter
	// Audit records one push — published by the engine through the operator
	// surface's audit, which outlives the request.
	Audit func(ctx context.Context, e types.OperatorActed)
	// Fetched records one fetch of a `visibility: unit` repository whose
	// answer carried a pack (fetch.go), before the response ends — filed by
	// the engine in the node's reads ledger (internal/code/readaudit), which
	// publishes every reader's reads of a flush as one record into the
	// node's event log, from which the usage domain derives who read it. It
	// returns at once, so no fetch's answer waits on the broker for its own
	// record, and the record outlives the request: a client that hung up
	// after its pack still read the repository.
	Fetched func(ctx context.Context, e types.CodeFetched)
	// Limits is the CURRENT epoch's code.limits.
	Limits func() code.Limits
	// DeletedRetention is the CURRENT epoch's code.deleted_retention_days —
	// what a deleted repository's refusal names its purge by.
	DeletedRetention func() time.Duration
	// Roster is the chart an engine write's grant is judged by (write.go):
	// the serving node's CURRENT epoch's, read per write — the query
	// server's own. A nil answer is a node with no chart yet, which builds no
	// seat's write (code.UnreadyNoChart).
	Roster func() code.Roster
	// Signer names an engine-built commit's author and committer from the
	// serving node's epoch (write.go's Signer).
	Signer Signer
	// Share is the node's seat git-work share — the query server's own, ONE a
	// node (code.GitShare): an engine write's build holds one unit of it,
	// given back before its landing.
	Share *code.GitShare
	// Current reports whether this node serves the company's configuration:
	// false on a node confirmed behind it, which builds no SEAT's write from
	// the chart the fleet replaced (code.UnreadyBehindConfig). nil is current.
	Current func() bool
	// NodeID is this node, which a proposal names as the one that received
	// the push.
	NodeID string
	// Timing is every scaled duration (Production in the engine).
	Timing Timing
	// Faults is nil in production (internal/code/codefault).
	Faults codefault.Injector
	// Logger nil is the package's own (component code.host).
	Logger *slog.Logger
	// Metrics nil records nothing.
	Metrics *metrics.Recorder
	// Now nil is time.Now.
	Now func() time.Time
}

// ErrStopping is a request to a code host that is stopping: the API answers
// it 503 with Retry-After StoppingRetryAfter.
var ErrStopping = errors.New("host: the code host is stopping")

// ErrOptions is an Options value New refuses, wrapped naming every field.
var ErrOptions = errors.New("host: refused options")

// Server is one node's git surface.
type Server struct {
	o   Options
	log *slog.Logger
	now func() time.Time

	// life is the HOST's lifetime — the one stored context, for the reason
	// the committer's is: Stop ends it, and every request's work context is
	// joined to it, so a stop reaches every git process and hook in flight.
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

	// checkProposal is code.Proposal.ValidateUnstored, the receiver's own
	// check of a proposal before its bundle is stored — a field so the suite
	// can stand in a receiver's bug, which no push reaches (export_test.go).
	checkProposal func(code.Proposal) error

	// runGit is o.Git.Run, through which every git process the server starts
	// goes — a field so the suite can read each Cmd as the pool is handed it
	// (export_test.go). A push's deadlines are formed at their call sites
	// from what the push sends and what its repository holds, and only the
	// Cmd that reaches the pool says a call site formed them from the right
	// ones: a helper that adds the history term proves nothing about a call
	// site that hands it no history.
	runGit func(context.Context, gitproc.Cmd) (gitproc.Result, error)

	// writes is every engine write running on this node, by its push id,
	// which a second write of the same push id waits for (write.go's
	// in-flight join).
	writesMu sync.Mutex
	writes   map[string]*inflightWrite
	// syncWait bounds an engine write's wait on its sync and on its push's
	// outcome (statelog.ReadBudget), and buildBudget is the git run time its
	// build is given (code.WriteBuildDeadline) — fields so the suite can
	// scale them (export_test.go).
	syncWait, buildBudget time.Duration
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
		checkProposal: code.Proposal.ValidateUnstored, runGit: o.Git.Run, syncWait: statelog.ReadBudget,
		buildBudget: code.WriteBuildDeadline}
	if s.log == nil {
		s.log = logging.Get("code.host")
	}
	if s.now == nil {
		s.now = time.Now
	}
	transfers := o.Git.Slots(gitproc.ClassTransfer)
	s.heads = newAllowance(RequestMemoryAllowance(transfers))
	s.spools = newSpools(spoolSizesFor(transfers))
	s.life, s.end = context.WithCancel(context.Background())
	return s, nil
}

// Start opens the hook relay's socket and its accept loop, and starts the
// cache's background work — both owned until Stop. An error (the socket cannot
// be made, or its path is too long) is the engine's to fail the boot with.
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
// work to end: THREE KILL GRACES. A cancelled git process is sent SIGTERM and,
// gitproc.KillGrace later, SIGKILL; a pipe a straggler still holds is closed a
// further KillGrace after that; and the third is for what follows a process's
// end — a handler's answer to a client that has gone, an upload abandoned
// under its own bound. Declared here, beside the stop whose work it measures,
// because every caller that stops a host as the engine does — the engine's
// teardown, and the topologies that stop a node the engine's way
// (internal/code/codetest) — must give it the same time.
const StopBudget = 3 * gitproc.KillGrace

// Stop refuses every new git request (503, Retry-After StoppingRetryAfter);
// cancels every in-flight request's work — a receive-pack is killed through
// its context, its hook's connection with it; waits for the in-flight
// handlers, until ctx ends; closes the relay's listener, waits for its
// connections and removes its directory; and stops the cache. Idempotent in
// effect: a second call waits again for whatever is still running. A caller
// bounds ctx by [StopBudget].
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

// begin admits one request to a running host, or answers it 503: a stopping
// host begins nothing. It answers the request's work context — the client's,
// joined to the host's lifetime, so a Stop reaches it — and what ends it.
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
func (s *Server) fault(ctx context.Context, p codefault.Point, at int64) error {
	if s.o.Faults == nil {
		return nil
	}
	return s.o.Faults.Hit(ctx, p, at)
}

// limits is the current epoch's code.limits.
func (s *Server) limits() code.Limits { return s.o.Limits() }
