package gitproc

import (
	"context"
	"net/url"
	"time"

	"github.com/crewlet/crewlet/internal/httpx"
)

// RemoteEnv is what a case resolves a remote against in place of the
// process's own: the resolver for the remote's host and the network's NAT64
// prefixes, the proxy for a URL (nil names none), and SSL_CERT_FILE and
// SSL_CERT_DIR.
type RemoteEnv struct {
	Resolver       httpx.Resolver
	Proxy          func(*url.URL) (*url.URL, error)
	CAFile, CADirs string
}

// ResolveRemoteWith is ResolveRemote against env, reaching neither the
// machine's network nor its environment.
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

// SetRemoteStall scales TransferStall, which p derives a remote run's TCP
// keepalive from in whole seconds (Remote.config), for a case that waits
// longer than a stall (call before any Run).
func SetRemoteStall(p *Pool, stall time.Duration) { p.remoteStall = stall }

// SetClock has p read Result.Began and Result.Took from now rather than the
// machine's clock, so a case states them rather than ordering two wall-clock
// readings a coarse clock stamps equal. Call before any Run.
func SetClock(p *Pool, now func() time.Time) { p.now = now }

// SeeGrace has p arm the wait between a run's SIGTERM and SIGKILL through
// armed rather than a timer: armed gets the exact grace and answers the
// channel that ends the wait. Call before any Run.
func SeeGrace(p *Pool, armed func(grace time.Duration) <-chan time.Time) {
	p.grace = func(d time.Duration) (<-chan time.Time, func() bool) {
		return armed(d), func() bool { return true }
	}
}

// OnDrain has p tell fn each stream a run's drain begins to wait for, in
// order (stdout, stderr, the trace, last stdin), on the run's goroutine, so
// fn must not block. A case parks a run there before releasing its caller: a
// run in its drain has been reaped and swept, and one waiting for stdin waits
// only on its caller's Read. Call before any Run.
func OnDrain(p *Pool, fn func(stream string)) { p.draining = fn }

// ResolveMemory is what New sizes the lanes by under o, reading the node's
// memory with read.
func ResolveMemory(o Options, read func() (Memory, error)) (Memory, error) {
	return resolveMemory(o, read)
}

// SetCaptureMax scales CaptureMax for the overflow case.
func SetCaptureMax(p *Pool, n int) { p.captureMax = n }

// ReadLane is the READ lane's held slots and outstanding loans, for the lane
// accounting cases.
func ReadLane(p *Pool) (held, lent int64) { return p.read.counts() }

// Waiting is how many runs wait in p's lane of class c, queued and not yet
// granted. A case parks a run there before releasing a slot or ending the
// caller, so the release or cancel meets that waiter rather than a run still
// on its way to the lane.
func Waiting(p *Pool, c Class) int64 {
	l := p.lane(c)
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.waiting
}

// ShareForget is the decayed charge below which an idle account is forgotten.
const ShareForget = shareForget

// SetArrivalOrder replaces p's transfer lane with one as wide that grants in
// ARRIVAL ORDER and keeps no book: BenchmarkTransferShare's control. Call
// before any Run.
func SetArrivalOrder(p *Pool) { p.transfer = newLane(ClassTransfer, p.transfer.size) }

// SetRunUncapped lifts p's transfer lane's cap on the coding boxes' class
// (ShareClass.Cap): its book reckons caps for a lane twice as wide, so a
// box's half is the whole lane. BenchmarkTransferShare's runs-uncapped
// control. Call before any Run.
func SetRunUncapped(p *Pool) { p.transfer.book.slots = 2 * int(p.transfer.size) }

// SetWeights has p's transfer lane weigh the classes by weights in place of
// ShareClass.Weight, for BenchmarkTransferShare's runs arms' search for the
// weights at which a contended lane holds its pass rule. Every class needs a
// weight above zero. Call before any Run.
func SetWeights(p *Pool, weights map[ShareClass]int) {
	p.transfer.book.weight = func(c ShareClass) int { return weights[c] }
}

// TransferHeld is how many of p's transfer slots class c's runs hold now.
func TransferHeld(p *Pool, c ShareClass) int {
	p.transfer.mu.Lock()
	defer p.transfer.mu.Unlock()
	return p.transfer.book.classes[c].running
}

// SetShareClock sets the clock p's transfer lane's book reads, so a
// benchmark moves minutes without waiting them. Call before any Run.
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

// TransferLane is p's own transfer lane, as New sized and charges it, driven
// as a ShareLane with no git process. Move its clock with SetShareClock
// first.
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

// ReadTrace reads stream as a run's trace sink in dir reads its trace2 event
// stream, in pieces of at most piece bytes (0: whole), and answers the
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

// TraceHeld is the largest capacity a trace sink's held line reached while
// reading stream in pieces of piece bytes.
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
