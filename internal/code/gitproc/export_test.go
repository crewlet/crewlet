package gitproc

import (
	"context"
	"net/url"
	"time"

	"github.com/crewlet/crewlet/internal/httpx"
)

// RemoteEnv is what a case resolves a remote against in place of the
// process's own: the resolver the remote's host and the network's NAT64
// prefixes are asked of, the proxy the process would name for a URL (nil
// names none), and the values SSL_CERT_FILE and SSL_CERT_DIR would hold.
type RemoteEnv struct {
	Resolver       httpx.Resolver
	Proxy          func(*url.URL) (*url.URL, error)
	CAFile, CADirs string
}

// ResolveRemoteWith is ResolveRemote against env: a remote a case builds
// reaching no network and no environment of the machine's.
func ResolveRemoteWith(ctx context.Context, env RemoteEnv, rawURL string, allowPrivate bool) (Remote, error) {
	proxy := env.Proxy
	if proxy == nil {
		proxy = func(*url.URL) (*url.URL, error) { return nil, nil }
	}
	return resolveRemote(ctx, rawURL, allowPrivate, remoteEnv{
		resolver: env.Resolver, proxy: proxy, caFile: env.CAFile, caDirs: env.CADirs,
	})
}

// SetTimings scales SlotWait and KillGrace for a pool a test then uses (call
// before any Run).
func SetTimings(p *Pool, slotWait, killGrace time.Duration) {
	p.slotWait, p.killGrace = slotWait, killGrace
}

// SetRemoteStall scales the stall p derives a remote run's TCP keepalive from
// (TransferStall: quiet for half of it before the first probe, the rest
// shared among the probes), in whole seconds, for a case that waits longer
// than a stall (call before any Run).
func SetRemoteStall(p *Pool, stall time.Duration) { p.remoteStall = stall }

// ResolveMemory is what New sizes the lanes by under o, the node's own
// memory read by read.
func ResolveMemory(o Options, read func() (Memory, error)) (Memory, error) {
	return resolveMemory(o, read)
}

// SetCaptureMax scales CaptureMax for the overflow case.
func SetCaptureMax(p *Pool, n int) { p.captureMax = n }

// ReadLane is the READ lane's held slots and outstanding loans, for the lane
// accounting cases.
func ReadLane(p *Pool) (held, lent int64) { return p.read.counts() }

// ShareForget is the decayed charge below which an idle account is forgotten.
const ShareForget = shareForget

// SetArrivalOrder replaces p's transfer lane with one of the same width that
// grants in ARRIVAL ORDER and keeps no book — the lane before the share, which
// BenchmarkTransferShare's control runs. Call before any Run.
func SetArrivalOrder(p *Pool) { p.transfer = newLane(ClassTransfer, p.transfer.size) }

// SetShareClock sets the clock p's transfer lane reads its book by — what a
// benchmark moves minutes on without waiting them. Call before any Run.
func SetShareClock(p *Pool, now func() time.Time) {
	p.transfer.book.now = now
	p.transfer.book.swept = now()
}

// ShareLane is the transfer lane's scheduler alone (share.go), driven by a
// case on a clock of its own: arrivals, settles and leaves, with no git
// process and no goroutine.
type ShareLane struct{ l *lane }

// NewShareLane is a share lane of slots on a node of cores read slots, on the
// clock now: charged as New charges a lane of that shape (occupancyRate).
func NewShareLane(cores, slots int, now func() time.Time) *ShareLane {
	book := newShareBook(now, slots, occupancyRate(int64(cores), int64(slots)), nil)
	return &ShareLane{l: newShareLane(int64(slots), book)}
}

// TransferLane is p's own transfer lane — as New sized it and charges it —
// driven as a ShareLane, with no git process. Move its clock with
// SetShareClock first.
func TransferLane(p *Pool) *ShareLane { return &ShareLane{l: p.transfer} }

// Turn is one arrival at a ShareLane: granted at once, or queued until a
// settle hands it a slot.
type Turn struct {
	s *ShareLane
	r *run
	w *waiter
}

// Arrive is sh's arrival.
func (s *ShareLane) Arrive(sh Share) *Turn {
	s.l.mu.Lock()
	defer s.l.mu.Unlock()
	r, w := s.l.arrive(sh)
	return &Turn{s: s, r: r, w: w}
}

// Granted reports whether the turn holds a slot.
func (t *Turn) Granted() bool {
	t.s.l.mu.Lock()
	defer t.s.l.mu.Unlock()
	return t.r != nil || t.w.granted
}

// Settle ends a granted turn, given the CPU its process spent; its slot goes
// to whoever the lane names next.
func (t *Turn) Settle(cpu time.Duration) {
	r := t.r
	if r == nil {
		t.s.l.mu.Lock()
		r = t.w.run
		t.s.l.mu.Unlock()
	}
	t.s.l.release(r, cpu)
}

// Leave takes a turn still waiting out of its queue, as a waiter whose
// context ended does.
func (t *Turn) Leave() {
	t.s.l.mu.Lock()
	defer t.s.l.mu.Unlock()
	t.s.l.leave(t.w)
}

// Acquire is a waiting acquire of the lane, as the pool's admission makes
// one; release with Release.
func (s *ShareLane) Acquire(ctx context.Context, sh Share) (*Turn, error) {
	r, err := s.l.acquire(ctx, sh)
	if err != nil {
		return nil, err
	}
	return &Turn{s: s, r: r}, nil
}

// Counts is the lane's held slots and queued waiters.
func (s *ShareLane) Counts() (held, waiting int64) {
	s.l.mu.Lock()
	defer s.l.mu.Unlock()
	return s.l.held, s.l.waiting
}

// Usage is class c's standing now, as the next grant reads it.
func (s *ShareLane) Usage(c ShareClass) float64 {
	s.l.mu.Lock()
	defer s.l.mu.Unlock()
	return s.l.book.classes[c].value(s.l.book.now(), s.l.book.occupancy)
}

// Charge is sh's standing now, and false when the book has forgotten it.
func (s *ShareLane) Charge(sh Share) (float64, bool) {
	s.l.mu.Lock()
	defer s.l.mu.Unlock()
	a := s.l.book.byKey[sh]
	if a == nil {
		return 0, false
	}
	return a.value(s.l.book.now(), s.l.book.occupancy), true
}

// Principals is how many accounts the book holds.
func (s *ShareLane) Principals() int {
	s.l.mu.Lock()
	defer s.l.mu.Unlock()
	return len(s.l.book.byKey)
}

// FormatRow is what one of git's report formats classifies as.
type FormatRow struct {
	Kind error
	Part Part
}

// FormatTable is the classification table: every format as git spells it,
// by what it classifies as.
func FormatTable() map[string]FormatRow {
	out := map[string]FormatRow{}
	for r, list := range formatTable {
		for _, f := range list {
			out[f] = FormatRow{Kind: r.kind, Part: r.part}
		}
	}
	return out
}

// CanonicalFormat is the form a format is looked up by.
func CanonicalFormat(format string) string { return canonicalFormat(format) }

// ReadTrace reads stream as the trace sink of a run in dir reads its trace2
// event stream, written in pieces of at most piece bytes (0: whole): the
// summary, the kind an ExitError would carry, and the damage.
func ReadTrace(dir string, stream []byte, piece int) (t Trace, kind, damage error) {
	s := newTraceSink(dir)
	if piece <= 0 {
		piece = len(stream) + 1
	}
	for len(stream) > 0 {
		n := min(piece, len(stream))
		_, _ = s.Write(stream[:n])
		stream = stream[n:]
	}
	s.finish()
	return s.summary(), s.kind(), s.damage(time.Time{})
}

// TraceHeld is the most capacity the line a trace sink holds reached while
// it read stream in pieces of piece bytes: what it keeps of a line.
func TraceHeld(stream []byte, piece int) int {
	s := newTraceSink("")
	most := 0
	for len(stream) > 0 {
		n := min(piece, len(stream))
		_, _ = s.Write(stream[:n])
		stream = stream[n:]
		most = max(most, cap(s.line))
	}
	return most
}
