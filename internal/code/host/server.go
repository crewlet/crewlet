package host

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	"github.com/crewlet/crewlet/internal/objstore"
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

// Uploader stores a push's pack bundle; *objstore.Store satisfies it.
type Uploader interface {
	Put(ctx context.Context, r io.Reader, limit int64, m objstore.PutMeta) (objstore.Object, error)
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
//   - Stall is gitproc.TransferStall (a minute, objstore.ReadStall): git's
//     keepalives are five seconds apart, so twelve of them missed is a process
//     that stopped — and a case that holds a stand-in git still need not wait
//     a minute to see it ended.
var Production = Timing{
	PendingGrace: statelog.StallGrace,
	PendingFirst: 250 * time.Millisecond,
	PendingMax:   2 * time.Second,
	Keepalive:    5 * time.Second,
	Handshake:    5 * time.Second,
	Stall:        gitproc.TransferStall,
}

// Options are how a Server is built. Every field but Faults, Logger, Metrics
// and Now is required, and New refuses a missing one by name.
type Options struct {
	// Cache is this node's git cache: every directory a git process here
	// reads, and the scratch repositories a push is received in.
	Cache *cache.Cache
	// Git runs every git process.
	Git *gitproc.Pool
	// Estate is the code domain's rows and its committer.
	Estate Estate
	// Objects stores a push's pack bundle.
	Objects Uploader
	// Persons is the human seat a credential is bound to, "" for none.
	Persons func(operatorID string) (seat string)
	// Audit records one push — published by the engine through the operator
	// surface's audit, which outlives the request.
	Audit func(ctx context.Context, e types.OperatorActed)
	// Limits is the CURRENT epoch's code.limits.
	Limits func() code.Limits
	// DeletedRetention is the CURRENT epoch's code.deleted_retention_days —
	// what a deleted repository's refusal names its purge by.
	DeletedRetention func() time.Duration
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

	// requestMemory is RequestMemoryBytes, a field so the suite can reach
	// the spool without megabytes of fixtures (export_test.go).
	requestMemory int
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
	need(o.Persons == nil, "Persons")
	need(o.Audit == nil, "Audit")
	need(o.Limits == nil, "Limits")
	need(o.DeletedRetention == nil, "DeletedRetention")
	need(strings.TrimSpace(o.NodeID) == "", "NodeID")
	need(o.Timing.PendingGrace <= 0, "Timing.PendingGrace")
	need(o.Timing.PendingFirst <= 0, "Timing.PendingFirst")
	need(o.Timing.PendingMax <= 0, "Timing.PendingMax")
	need(o.Timing.Keepalive <= 0, "Timing.Keepalive")
	need(o.Timing.Handshake <= 0, "Timing.Handshake")
	need(o.Timing.Stall <= 0, "Timing.Stall")
	if len(missing) > 0 {
		return nil, fmt.Errorf("%w: %s missing or zero", ErrOptions, strings.Join(missing, ", "))
	}
	s := &Server{o: o, log: o.Logger, now: o.Now, requestMemory: RequestMemoryBytes}
	if s.log == nil {
		s.log = logging.Get("code.host")
	}
	if s.now == nil {
		s.now = time.Now
	}
	transfers := o.Git.Slots(gitproc.ClassTransfer)
	s.heads = newAllowance(RequestMemoryAllowance(transfers))
	s.spools = newSpools(SpoolAllowance(transfers), SpoolCredentialBytes(transfers), SpoolsPerCredential(transfers))
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

// Stop refuses every new git request (503, Retry-After StoppingRetryAfter);
// cancels every in-flight request's work — a receive-pack is killed through
// its context, its hook's connection with it; waits for the in-flight
// handlers, until ctx ends; closes the relay's listener, waits for its
// connections and removes its directory; and stops the cache. Idempotent.
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
