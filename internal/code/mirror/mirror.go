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
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// Name is the worker's name on its home loop (internal/code/home's
// Worker.Name): what crewlet.code.home.passes{worker} counts its turns under.
const Name = "mirrors"

// MaxAttempts is how many attempts — imports, syncs and pushes — one node
// runs at once: TWO. An import or a sync spends most of its time on its fetch
// — one slot of the node's transfer lane, paced by the other host — and then
// one pack-objects in its maintenance lane, and a push on its push, one
// transfer slot; so two keep one large import from holding back every mirror
// the node is home to while holding at most one maintenance slot each, the
// share a home's merge takes (internal/code/home's Concurrency). A
// repository listed while two run waits for the next pass, a minute.
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

// Publisher is a node's event stream as a push announces itself on it:
// queue.Queue satisfies it.
type Publisher interface {
	Publish(ctx context.Context, subject string, ev *events.Event) error
}

// eventBudget bounds the announcement of one push: FIVE SECONDS, the
// JetStream client's own request timeout, as every code-host announcement is
// bounded (internal/code's checkEventBudget). The push has landed at the
// other host by then, so nothing waits on it but the attempt's end.
const eventBudget = 5 * time.Second

// PushPace is the soonest a push mirror is pushed again after its last
// attempt ended: ONE MINUTE, the home loop's backstop. A busy repository's
// every record nudges its home, and a push a record would dial the other host
// once a push; at most once a minute keeps a mirror a minute behind a green
// that moved, for one ls-remote a minute while the repository is busy and
// none while it is idle, when the backstop lists it once a MirrorInterval.
const PushPace = time.Minute

// AttemptDeadline is the GRACE code_mirror_failing gives an attempt in flight
// beyond the mirror's interval: FIFTEEN MINUTES, one more code.MirrorInterval
// — so a mirror is failing once it has not been brought level for two
// intervals running, two attempts missed rather than one slow one. An attempt
// itself is bounded by what it moves (gitproc.RemoteDeadline over the bytes
// it may send), which a first push of a large repository can take far longer
// than any constant: so this is the alarm's grace, never a deadline the
// attempt is killed at. JUDGMENT: the promise P8 states is the interval
// alone, and an alarm at exactly the interval would fire on every attempt
// that took a second past it.
const AttemptDeadline = code.MirrorInterval

// Options builds [Mirrors]. Every field but Events, Faults, Metrics, Logger
// and Now is required, and New refuses a missing one by name.
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
	// Roster is the chart this node holds NOW, read at every push: the seat
	// a push mirror pushes as is judged against it before each push
	// (code.MirrorSeatRefusal), since a later revision of the chart may take
	// the seat's read away.
	Roster func() code.Roster
	// Events is this node's event stream, which a push's account
	// (code_mirror_pushed) is published on; nil publishes none.
	Events Publisher
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
	kindPush
)

// listed is what the last listing said of one repository: what it owes, and
// from where.
type listed struct {
	kind   kind
	name   string            // the repository's name, as the listing read it
	source code.ImportSource // an import's source, or a pull mirror's
	seq    int64             // an import's chunks landed
	at     statelog.Position // the version its row was read at
}

// memory is what this node remembers of one repository's attempts: when the
// last one ended, and how many in a row failed.
type memory struct {
	ended    time.Time
	failures int
}

// mirrorKey is one WAY a repository is mirrored: IN (out false) — its import
// and then its pull mirror's syncs, which move its refs from the one source
// and so are ONE line of attempts: never two at once, and the import's last
// attempt holding the first sync back an interval as a sync holds the next —
// or OUT, its push mirror's pushes, which a repository may have beside them
// and which move nothing of its own.
type mirrorKey struct {
	repo string
	out  bool
}

// health is THE HOME'S FAILURE STATE of one mirror (M6-D16): when it was
// last brought level — or when this node first tried it, where it never has
// been here — when this node last started an attempt of it, and the name of
// its repository. In memory alone: a push to another host is no record, and
// the node that pushes is the one that knows whether it can.
type health struct {
	name    string
	level   time.Time
	started time.Time
}

// pace is how long after its last attempt ended a repository's attempt of
// kind k waits before another — an import none, a pull mirror's sync its
// interval, a push mirror's push PushPace — beside [RetryAfter] after a
// failure.
func pace(k kind) time.Duration {
	switch k {
	case kindSync:
		return code.MirrorInterval
	case kindPush:
		return PushPace
	}
	return 0
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
	known   map[mirrorKey]listed
	running map[mirrorKey]bool
	memory  map[mirrorKey]memory
	health  map[mirrorKey]health
	stopped bool
	wg      sync.WaitGroup
}

// New builds a node's mirrors; nothing runs until a turn of its loop asks
// ([Mirrors.Work]).
func New(o Options) (*Mirrors, error) {
	var missing []string
	for name, absent := range map[string]bool{
		"Estate": o.Estate == nil, "Cache": o.Cache == nil, "Git": o.Git == nil, "Objects": o.Objects == nil,
		"Dial": o.Dial == nil, "NodeID": o.NodeID == "", "Limits": o.Limits == nil, "Roster": o.Roster == nil,
	} {
		if absent {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%w: missing %s", ErrOptions, strings.Join(slices.Sorted(slices.Values(missing)), ", "))
	}
	m := &Mirrors{o: o, log: o.Logger, now: o.Now, known: map[mirrorKey]listed{}, running: map[mirrorKey]bool{},
		memory: map[mirrorKey]memory{}, health: map[mirrorKey]health{}}
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
// repository being imported; then every push mirror — `mirrors_due` lists them
// first, by id — whose last push here is a code.MirrorInterval old, or that
// was never pushed here; then every pull mirror whose last sync is a
// code.MirrorInterval old — listed least recently synced first, so the
// listing stops at the first synced within it — less the ones this node
// attempted within that interval with nothing to land, or failed within
// [RetryAfter]. Read at floor where it is set, stale where it is zero. What it
// lists it remembers, for the turn that works it — every push mirror among
// them, due or not, so a nudge of its repository (a green row moved) finds
// it.
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
			key := mirrorKey{repo: c.ID}
			m.known[key] = listed{kind: kindImport, name: c.Name, source: c.Source, seq: c.Seq, at: c.Version}
			if m.waiting(key, now, 0) {
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
			if c.To != nil {
				// A PUSH MIRROR'S CANDIDATE CARRIES NO VERSION of its own:
				// its attempt reads the rows again, at or past the position
				// this listing answered at, so the snapshot it pushes from
				// holds every record the listing saw.
				key := mirrorKey{repo: c.ID, out: true}
				m.known[key] = listed{kind: kindPush, name: c.Name, at: ans.Position}
				if !m.waiting(key, now, code.MirrorInterval) {
					out = append(out, c.ID)
				}
				continue
			}
			if c.From == nil {
				continue
			}
			if !c.SyncedAt.IsZero() && now.Sub(c.SyncedAt) < code.MirrorInterval {
				// The first synced within its interval: every one after
				// it was synced later still.
				return out, "", nil
			}
			key := mirrorKey{repo: c.ID}
			m.known[key] = listed{kind: kindSync, name: c.Name, source: code.ImportSource(*c.From), at: c.Version}
			if m.waiting(key, now, code.MirrorInterval) {
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

// waiting reports whether key's last attempt on this node ended too recently
// for another: within RetryAfter of a failure, or within after of a success.
// Called with mu held.
func (m *Mirrors) waiting(key mirrorKey, now time.Time, after time.Duration) bool {
	mem, ok := m.memory[key]
	switch {
	case !ok:
		return false
	case mem.failures > 0:
		return now.Sub(mem.ended) < RetryAfter(mem.failures)
	}
	return now.Sub(mem.ended) < after
}

// Work STARTS repoID's attempts where its listings named them and none is
// running — its import or its pull mirror's sync, and its push mirror's push
// — each on a goroutine of the worker's own, and returns: an attempt dials
// another host, which a turn's one backstop does not bound. An attempt no
// listing named — a nudge for a repository this worker owes nothing — or one
// running, or one within its pace of the last ([pace], [RetryAfter]), or one
// past [MaxAttempts] running, is left to the next pass. ErrStopped once Stop
// has begun.
func (m *Mirrors) Work(_ context.Context, repoID string, floor statelog.Position) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped {
		return ErrStopped
	}
	now := m.now()
	for _, out := range []bool{false, true} {
		key := mirrorKey{repo: repoID, out: out}
		l, ok := m.known[key]
		if !ok || m.running[key] || len(m.running) >= MaxAttempts || m.waiting(key, now, pace(l.kind)) {
			continue
		}
		m.start(key, l, floor, now)
	}
	return nil
}

// start runs key's attempt on a goroutine of the worker's own. Called with mu
// held.
func (m *Mirrors) start(key mirrorKey, l listed, floor statelog.Position, now time.Time) {
	m.running[key] = true
	if l.kind != kindImport {
		h, ok := m.health[key]
		if !ok {
			// NEVER BROUGHT LEVEL HERE: counted from the first attempt
			// this node makes, so a home that has just taken a mirror
			// over gives it the whole promise before it is failing.
			h.level = now
		}
		h.name, h.started = l.name, now
		m.health[key] = h
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		var err error
		if key.out {
			err = m.push(m.life, key.repo, l, floor)
		} else {
			err = m.attempt(m.life, key.repo, l, floor)
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		if !key.out {
			// What the listing said is SPENT by the attempt it started: an
			// import's Seq and version are behind once a chunk landed, so
			// a nudge after it — its own records' — finds the repository
			// unknown and leaves it to the next listing, which reads the
			// rows again. A push mirror's listing says only that it has
			// one: its attempt reads the rest at its own floor, so a nudge
			// of a green row that moved finds it still.
			delete(m.known, key)
		}
		delete(m.running, key)
		mem := m.memory[key]
		mem.ended = m.now()
		if err != nil {
			mem.failures++
		} else {
			mem.failures = 0
			if h, ok := m.health[key]; ok {
				h.level = mem.ended
				m.health[key] = h
			}
		}
		m.memory[key] = mem
	}()
}

// forget drops everything this node remembers of key's mirror: a mirror its
// repository no longer has. Called from its own attempt, with mu NOT held.
func (m *Mirrors) forget(key mirrorKey) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.known, key)
	delete(m.health, key)
}

// AlarmReading fills the reading's mirror half at the worker's clock: how
// many mirrors — pull mirrors' syncs and push mirrors' pushes — this node is
// home to have not been brought level for longer than their own refresh
// promise, code.MirrorInterval plus [AttemptDeadline] (ADR-0015), and the one
// failing longest, by its repository's name — what raises
// code_mirror_failing. A mirror this node has not started an attempt of
// within that promise is no longer this node's — its repository's home moved,
// or its mirror went — and is left out: the home tries a mirror at least once
// an interval, a failing one sooner ([RetryAfter]). So is one whose attempt
// has run longer than the promise — a first push of a large repository —
// until that attempt ends: an attempt is bounded by what it moves
// (gitproc.RemoteDeadline), never by this alarm, and a failed one's retry, a
// [RetryAfter] later, counts it again. Assigned here, field by
// field, so the reading's producers gate reaches each one through this
// concrete method (internal/engine's TestEveryAlarmReadingFieldHasAProducer).
func (m *Mirrors) AlarmReading(out *statelog.Reading) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now, promise := m.now(), code.MirrorInterval+AttemptDeadline
	var (
		failing int
		worst   health
	)
	for _, h := range m.health {
		if now.Sub(h.started) > promise || now.Sub(h.level) <= promise {
			continue
		}
		failing++
		if worst.level.IsZero() || h.level.Before(worst.level) {
			worst = h
		}
	}
	out.CodeMirrorFailing = failing
	if failing == 0 {
		return
	}
	out.CodeMirrorFailingRepo = worst.name
	out.CodeMirrorFailingFor = now.Sub(worst.level)
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
