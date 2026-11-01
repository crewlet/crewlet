package mirror

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// Name is the worker's name on its home loop (internal/code/home's
// Worker.Name): what crewlet.code.home.passes{worker} counts its turns under.
const Name = "mirrors"

// MaxAttempts is how many imports and syncs one node runs at once: TWO. An
// attempt spends most of its time on its fetch — one slot of the node's
// transfer lane, paced by the other host — and then one pack-objects in its
// maintenance lane, so two keep one large import from holding back every
// pull mirror the node is home to while holding at most one maintenance slot
// each, the share a home's merge takes (internal/code/home's Concurrency).
// A repository listed while two run waits for the next pass, a minute.
const MaxAttempts = 2

// RetryAfter is how long after a failed attempt this node lists the
// repository again: a minute, doubled with every failure in a row, and never
// more than code.MirrorInterval — so a source that is down, or a credential
// it refuses, is asked about four times in its first quarter of an hour and
// then once an interval, where a retry every pass would dial it sixty times
// an hour for an answer that has not changed. A permanent refusal ends an
// import instead ([Mirrors] never retries one).
func RetryAfter(failures int) time.Duration {
	d := time.Minute
	for range max(failures-1, 0) {
		if d >= code.MirrorInterval/2 {
			return code.MirrorInterval
		}
		d *= 2
	}
	return min(d, code.MirrorInterval)
}

// Estate is the code log as an attempt reaches it: the listings through Read,
// and an import's chunks and end and a sync through Import and MirrorSync. The
// router's code seam satisfies it (estate.CodeSeam), each write raising this
// node's floors past the record it landed in, so the copy the next step syncs
// holds it.
type Estate interface {
	Read(ctx context.Context, q code.ReadQuery) (code.ReadAnswer, error)
	Import(ctx context.Context, p code.ImportProposal) (code.SyncResult, error)
	MirrorSync(ctx context.Context, p code.MirrorSyncProposal) (code.SyncResult, error)
}

// Cache is this node's git cache as an attempt reaches it — the *cache.Cache
// satisfies it: the repository synced and pinned, its live packs — every one
// excluded from the cut — the scratch the fetch writes into, and the adoption
// of what landed.
type Cache interface {
	Sync(ctx context.Context, repoID string, fresh statelog.Freshness) (cache.Synced, error)
	Packs(s cache.Synced) []code.PackRow
	NewScratch(ctx context.Context, s cache.Synced, id string) (*cache.Scratch, error)
	Adopt(ctx context.Context, repoID string, sc *cache.Scratch, packs ...code.PackRef) error
}

// Pool is the node's git pool: the fetch and the listing of the source's HEAD
// in its transfer lane, the cut in its maintenance lane and the reads of the
// scratch in its read lane.
type Pool interface {
	Run(ctx context.Context, c gitproc.Cmd) (gitproc.Result, error)
}

// Dial is the one outbound dial, as the engine builds it: the source's URL
// resolved and judged by the public-address rule (gitproc.ResolveRemote,
// Tier A git.import_allow_private its one escape), and the header carrying the
// credential its ${VAR} reference resolves to — "" for a source fetched
// anonymously — added (gitproc.Remote.WithHeader). An error is the attempt's.
type Dial func(ctx context.Context, url, credential string) (gitproc.Remote, error)

// Options builds [Mirrors]. Every field but Faults, Metrics, Logger and Now is
// required, and New refuses a missing one by name.
type Options struct {
	Estate  Estate
	Cache   Cache
	Git     Pool
	Objects code.Putter
	Dial    Dial
	// NodeID is this node: the node every record an attempt lands names.
	NodeID string
	// Limits is the CURRENT epoch's code.limits, read at every attempt: the
	// blob limit an output is held to, the window its cut is given and the
	// bytes a fetch may bring.
	Limits func() code.Limits
	// Faults is nil in production (internal/code/codefault).
	Faults codefault.Injector
	// Metrics nil records nothing; Logger nil is the package's own
	// (component code.mirror); Now nil is time.Now.
	Metrics *metrics.Recorder
	Logger  *slog.Logger
	Now     func() time.Time
}

// ErrOptions is an Options value New refuses, wrapped naming every field.
var ErrOptions = errors.New("mirror: refused options")

// ErrStopped is an attempt asked of [Mirrors] once it is stopping.
var ErrStopped = errors.New("mirror: stopped")

// kind is what one repository's attempt is.
type kind int

const (
	kindImport kind = iota + 1
	kindSync
)

// listed is what the last listing said of one repository: what it owes, and
// from where.
type listed struct {
	kind   kind
	source code.ImportSource // an import's source, or a pull mirror's
	seq    int64             // an import's chunks landed
	at     statelog.Position // the version its row was read at, an import's
}

// memory is what this node remembers of one repository's attempts: when the
// last one ended, and how many in a row failed.
type memory struct {
	ended    time.Time
	failures int
}

// Mirrors is a node's imports and pull mirrors, a worker of its home's loop
// (Name, Due, Work), whose attempts run on goroutines of its own until Stop.
type Mirrors struct {
	o   Options
	log *slog.Logger
	now func() time.Time

	life   context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	known   map[string]listed
	running map[string]bool
	memory  map[string]memory
	stopped bool
	wg      sync.WaitGroup
}

// New builds a node's mirrors; nothing runs until a turn of its loop asks
// ([Mirrors.Work]).
func New(o Options) (*Mirrors, error) {
	var missing []string
	for name, absent := range map[string]bool{
		"Estate": o.Estate == nil, "Cache": o.Cache == nil, "Git": o.Git == nil, "Objects": o.Objects == nil,
		"Dial": o.Dial == nil, "NodeID": o.NodeID == "", "Limits": o.Limits == nil,
	} {
		if absent {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%w: missing %s", ErrOptions, strings.Join(slices.Sorted(slices.Values(missing)), ", "))
	}
	m := &Mirrors{o: o, log: o.Logger, now: o.Now, known: map[string]listed{}, running: map[string]bool{},
		memory: map[string]memory{}}
	if m.log == nil {
		m.log = logging.Get("code.mirror")
	}
	if m.now == nil {
		m.now = time.Now
	}
	m.life, m.cancel = context.WithCancel(context.Background())
	return m, nil
}

// Name is the worker's name ([Name]).
func (m *Mirrors) Name() string { return Name }

// The cursor of a listing names its read: `i:<cursor>` pages `imports_due`,
// `m:<cursor>` pages `mirrors_due` — the imports' pages first, and the
// mirrors' from their first once the imports' end.
const (
	importsArm = "i:"
	mirrorsArm = "m:"
)

// Due is one page of the repositories this node owes an attempt: every
// repository being imported, then every pull mirror whose last sync is a
// code.MirrorInterval old — `mirrors_due` lists them least recently synced
// first, so the listing stops at the first synced within it — less the ones
// this node attempted within that interval with nothing to land, or failed
// within [RetryAfter]. Push mirrors, which the same read lists first, are
// none of this worker's. Read at floor where it is set, stale where it is
// zero. What it lists it remembers, for the turn that works it.
func (m *Mirrors) Due(ctx context.Context, floor statelog.Position, cursor string) ([]string, string, error) {
	fresh := statelog.Freshness{Level: statelog.ReadStale}
	if !floor.IsZero() {
		fresh = statelog.FloorAt(statelog.ReadSession, floor)
	}
	switch {
	case cursor == "" || strings.HasPrefix(cursor, importsArm):
		ans, err := m.o.Estate.Read(ctx, code.ReadQuery{Kind: code.ReadImportsDue,
			Cursor: strings.TrimPrefix(cursor, importsArm), Fresh: fresh})
		if err != nil {
			return nil, "", fmt.Errorf("mirror: list the repositories being imported: %w", err)
		}
		now := m.now()
		var out []string
		m.mu.Lock()
		for _, c := range ans.Imports {
			m.known[c.ID] = listed{kind: kindImport, source: c.Source, seq: c.Seq, at: c.Version}
			if m.waiting(c.ID, now, 0) {
				continue
			}
			out = append(out, c.ID)
		}
		m.mu.Unlock()
		if ans.Next != "" {
			return out, importsArm + ans.Next, nil
		}
		return out, mirrorsArm, nil
	case strings.HasPrefix(cursor, mirrorsArm):
		ans, err := m.o.Estate.Read(ctx, code.ReadQuery{Kind: code.ReadMirrorsDue,
			Cursor: strings.TrimPrefix(cursor, mirrorsArm), Fresh: fresh})
		if err != nil {
			return nil, "", fmt.Errorf("mirror: list the mirrors: %w", err)
		}
		now := m.now()
		var out []string
		m.mu.Lock()
		defer m.mu.Unlock()
		for _, c := range ans.Mirrors {
			if c.From == nil {
				continue
			}
			if !c.SyncedAt.IsZero() && now.Sub(c.SyncedAt) < code.MirrorInterval {
				// The first synced within its interval: every one after
				// it was synced later still.
				return out, "", nil
			}
			m.known[c.ID] = listed{kind: kindSync, source: code.ImportSource(*c.From), at: c.Version}
			if m.waiting(c.ID, now, code.MirrorInterval) {
				continue
			}
			out = append(out, c.ID)
		}
		if ans.Next != "" {
			return out, mirrorsArm + ans.Next, nil
		}
		return out, "", nil
	}
	return nil, "", fmt.Errorf("mirror: %q is no cursor of this worker's listing", cursor)
}

// waiting reports whether repo's last attempt on this node ended too recently
// for another: within RetryAfter of a failure, or within after of a success.
// Called with mu held.
func (m *Mirrors) waiting(repo string, now time.Time, after time.Duration) bool {
	mem, ok := m.memory[repo]
	switch {
	case !ok:
		return false
	case mem.failures > 0:
		return now.Sub(mem.ended) < RetryAfter(mem.failures)
	}
	return now.Sub(mem.ended) < after
}

// Work STARTS repoID's attempt where its last listing named one and none is
// running, on a goroutine of the worker's own, and returns: an attempt is a
// fetch from another host, which a turn's one backstop does not bound. A
// repository no listing named — a nudge for one this worker owes nothing — or
// one whose attempt is running, or one past [MaxAttempts] running, is left to
// the next pass. ErrStopped once Stop has begun.
func (m *Mirrors) Work(_ context.Context, repoID string, floor statelog.Position) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped {
		return ErrStopped
	}
	l, ok := m.known[repoID]
	if !ok || m.running[repoID] || len(m.running) >= MaxAttempts || m.waiting(repoID, m.now(),
		map[kind]time.Duration{kindImport: 0, kindSync: code.MirrorInterval}[l.kind]) {
		return nil
	}
	m.running[repoID] = true
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		err := m.attempt(m.life, repoID, l, floor)
		m.mu.Lock()
		defer m.mu.Unlock()
		// What the listing said is SPENT by the attempt it started: an
		// import's Seq and version are behind once a chunk landed, so a nudge
		// after it — its own records' — finds the repository unknown and
		// leaves it to the next listing, which reads the rows again.
		delete(m.known, repoID)
		delete(m.running, repoID)
		mem := m.memory[repoID]
		mem.ended = m.now()
		if err != nil {
			mem.failures++
		} else {
			mem.failures = 0
		}
		m.memory[repoID] = mem
	}()
	return nil
}

// Stop ends every attempt — each step's context cancelled, so a fetch or a
// cut in flight is killed and what it wrote leaves with its scratch — and
// waits for them until ctx ends. A second Stop waits again.
func (m *Mirrors) Stop(ctx context.Context) {
	m.mu.Lock()
	m.stopped = true
	m.mu.Unlock()
	m.cancel()
	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// count adds one to the counter name under attrs — nothing where Options left
// Metrics nil, as a case's worker does: a nil *metrics.Recorder records
// nothing only where its caller asks first.
func (m *Mirrors) count(name string, attrs metrics.Attrs) {
	if m.o.Metrics != nil {
		m.o.Metrics.Add(name, 1, attrs)
	}
}

// fault is the attempt's fault point p — nil in production, where every call
// is this nil check.
func (m *Mirrors) fault(ctx context.Context, p codefault.Point) error {
	if m.o.Faults == nil {
		return nil
	}
	return m.o.Faults.Hit(ctx, p, 0)
}
