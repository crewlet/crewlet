package mirror

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/gitletfault"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// Name is the worker's name on its home loop (internal/gitlet/home's
// Worker.Name): what crewlet.code.home.passes{worker} counts its turns under.
const Name = "mirrors"

// MaxAttempts is how many attempts — imports, syncs and pushes — one node
// runs at once: TWO. An import or a sync spends most of its time on its fetch
// — one slot of the node's transfer lane, paced by the other host, and every
// attempt's fetch one share of that lane between them (share, attempt.go) —
// and then one pack-objects in a maintenance slot it waits for, and a push on
// its push, one transfer slot; so two keep one large import from holding back
// every mirror the node is home to, without more of them queueing on the one
// share they take turns of. A repository due while two run WAITS here, not for
// the next pass: each attempt that ends starts the waiting one this node
// attempted longest ago ([Mirrors.Work]), so the node brings mirrors level at
// two over an attempt's length, whatever the passes' pace. JUDGMENT, NOT A
// MEASUREMENT: an idle source's attempt — a sync that finds nothing, a push
// with nothing to send — is one negotiation with the other host, about a
// second, so the gitlet.MirrorInterval promise holds for some 1 800 mirrors a
// node is home to while their sources are idle, fewer in proportion while
// attempts move bytes or a long import holds one of the two; past it,
// code_mirror_failing names the mirror that has waited longest.
const MaxAttempts = 2

// RetryAfter is how long after a failed attempt this node lists the
// repository again: a minute, doubled with every failure in a row, and never
// more than gitlet.MirrorInterval — so a source that is down, or a credential
// it refuses, is asked about four times in its first quarter of an hour and
// then once an interval, where a retry every pass would dial it sixty times
// an hour for an answer that has not changed. A permanent refusal ends an
// import instead ([Mirrors] never retries one).
func RetryAfter(failures int) time.Duration {
	d := time.Minute
	for range max(failures-1, 0) {
		if d >= gitlet.MirrorInterval/2 {
			return gitlet.MirrorInterval
		}
		d *= 2
	}
	return min(d, gitlet.MirrorInterval)
}

// Estate is the code log as an attempt reaches it: the listings through Read,
// and an import's chunks and end and a sync through Import and MirrorSync. The
// router's code seam satisfies it (estate.CodeSeam), each write raising this
// node's floors past the record it landed in, so the copy the next step syncs
// holds it.
type Estate interface {
	Read(ctx context.Context, q gitlet.ReadQuery) (gitlet.ReadAnswer, error)
	Import(ctx context.Context, p gitlet.ImportProposal) (gitlet.SyncResult, error)
	MirrorSync(ctx context.Context, p gitlet.MirrorSyncProposal) (gitlet.SyncResult, error)
}

// Cache is this node's git cache as an attempt reaches it — the *cache.Cache
// satisfies it: the repository synced and pinned, its live packs — every one
// excluded from the cut — the scratch the fetch writes into, and the adoption
// of what landed.
type Cache interface {
	Sync(ctx context.Context, repoID string, fresh statelog.Freshness) (cache.Synced, error)
	Packs(s cache.Synced) ([]gitlet.PackRow, error)
	NewScratch(ctx context.Context, s cache.Synced, id string) (*cache.Scratch, error)
	Adopt(ctx context.Context, repoID string, sc *cache.Scratch, packs ...gitlet.PackRef) error
}

// Pool is the node's git pool: the fetch and the listing of the source's HEAD
// in its transfer lane, the reads of the scratch in its read lane, and the
// cut in a slot of its maintenance lane the attempt waits for and holds
// (Reserve), since that lane refuses a process at once when it is full and
// the cut comes after a fetch no attempt should make twice.
type Pool interface {
	Run(ctx context.Context, c gitproc.Cmd) (gitproc.Result, error)
	Reserve() (*gitproc.Reserved, error)
}

// Dial is the one outbound dial, as the engine builds it: the source's URL
// resolved and judged by the public-address rule (gitproc.ResolveRemote,
// Tier A gitlet.import_allow_private its one escape), and the header carrying the
// credential its ${VAR} reference resolves to — "" for a source fetched
// anonymously — added (gitproc.Remote.WithHeader). An error is the attempt's.
type Dial func(ctx context.Context, url, credential string) (gitproc.Remote, error)

// Publisher is a node's event stream as a push and an import's end announce
// themselves on it: queue.Queue satisfies it.
type Publisher interface {
	Publish(ctx context.Context, subject string, ev *events.Event) error
}

// eventBudget bounds one announcement — a push's (code_mirror_pushed) or an
// import's end (code_imported): FIVE SECONDS, the JetStream client's own
// request timeout, as every Gitlet announcement is bounded (internal/gitlet's
// checkEventBudget). What it accounts for has landed by then — at the other
// host, or on the code log — so nothing waits on it but the attempt's end.
const eventBudget = 5 * time.Second

// announce publishes e — the account of something this node did for
// repository repoID — on this node's event stream, bounded by [eventBudget]
// and past the attempt's own context, since what it accounts for has already
// landed; a stream that does not take it is logged as unpublished, with
// detail, and what it accounts for stands. Events nil announces nothing.
func (m *Mirrors) announce(ctx context.Context, repoID string, e *events.Event, unpublished, detail string) {
	if m.o.Events == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), eventBudget)
	defer cancel()
	e.Source = m.o.NodeID
	if err := m.o.Events.Publish(ctx, topics.Event(e.Type), e); err != nil {
		m.log.Warn(unpublished, "repo", repoID, "error", err.Error(), "detail", detail)
	}
}

// PushPace is the soonest a push mirror is pushed again after its last
// attempt ended: ONE MINUTE, the home loop's backstop. A busy repository's
// every record nudges its home, and a push a record would read the rows once
// a record; at most once a minute keeps a mirror a minute behind a green that
// moved, for one read of its rows and its copy a minute while the repository
// is busy. The other host is dialed only where what the mirror covers moved —
// a green, a covered tip, its target or its patterns — or once an interval
// whatever moved ([Mirrors.stillLevel]): a seat's pushes to its own branches
// and a check's results dial nobody, and an idle repository is listed by the
// backstop once a MirrorInterval.
const PushPace = time.Minute

// AttemptDeadline is the GRACE code_mirror_failing gives an attempt in flight
// beyond the mirror's interval: FIFTEEN MINUTES, one more gitlet.MirrorInterval
// — so a mirror is failing once it has not been brought level for two
// intervals running, two attempts missed rather than one slow one. An attempt
// itself is bounded by what it moves (gitproc.RemoteDeadline over the bytes
// it may send), which a first push of a large repository can take far longer
// than any constant: so this is the alarm's grace, never a deadline the
// attempt is killed at. JUDGMENT: the promise P8 states is the interval
// alone, and an alarm at exactly the interval would fire on every attempt
// that took a second past it.
const AttemptDeadline = gitlet.MirrorInterval

// Options builds [Mirrors]. Every field but Events, Faults, Metrics, Logger
// and Now is required, and New refuses a missing one by name.
type Options struct {
	Estate  Estate
	Cache   Cache
	Git     Pool
	Objects gitlet.Putter
	Dial    Dial
	// NodeID is this node: the node every record an attempt lands names.
	NodeID string
	// Limits is the CURRENT epoch's code.limits, read at every attempt: the
	// blob limit an output is held to, the window its cut is given and the
	// bytes a fetch may bring.
	Limits func() gitlet.Limits
	// Roster is the chart this node holds NOW, read at every push: the seat
	// a push mirror pushes as is judged against it before each push
	// (gitlet.MirrorSeatRefusal), since a later revision of the chart may take
	// the seat's read away.
	Roster func() gitlet.Roster
	// People answers who holds a human seat and with which apps
	// (gitlet.People), read at every push of a push mirror pushing as a
	// person's seat: the mirror publishes only what that seat reads, and a
	// person reads only through Gitlet (ADR-0110), which an administrator may
	// take away after the mirror was set.
	People gitlet.People
	// Events is this node's event stream, which a push's account
	// (code_mirror_pushed) and an import's end (code_imported) are published
	// on; nil publishes none.
	Events Publisher
	// Faults is nil in production (internal/gitlet/gitletfault).
	Faults gitletfault.Injector
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
	name   string              // the repository's name, as the listing read it
	source gitlet.ImportSource // an import's source, or a pull mirror's
	seq    int64               // an import's chunks landed
	at     statelog.Position   // the version its row was read at
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
// last brought level — or when this node first was asked for it, where it
// never has been here — when a turn of this node last asked for it, whatever
// the ask became: an attempt started or queued, or none while one runs or
// waits its pace ([Mirrors.claim]) — and the name of its repository. In
// memory alone: a push to another host is no record, and the node that pushes
// is the one that knows whether it can.
type health struct {
	name  string
	level time.Time
	asked time.Time
}

// queuedAttempt is an attempt due while [MaxAttempts] ran: the furthest floor
// a turn asked it at, and its place in arrival order, which breaks a tie
// between two never attempted here or ended at one instant.
type queuedAttempt struct {
	floor   statelog.Position
	arrived uint64
}

// pace is how long after its last attempt ended a repository's attempt of
// kind k waits before another — an import none, a pull mirror's sync its
// interval, a push mirror's push PushPace — beside [RetryAfter] after a
// failure.
func pace(k kind) time.Duration {
	switch k {
	case kindSync:
		return gitlet.MirrorInterval
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
	// levels is what each push mirror's last attempt left the other host
	// level with ([level]).
	levels map[mirrorKey]level
	// queued is every attempt due while [MaxAttempts] ran, started as one
	// ends ([Mirrors.startQueued]); arrivals numbers them.
	queued   map[mirrorKey]queuedAttempt
	arrivals uint64
	// seen is every mirror the listing in progress has named so far — nil
	// between listings — which its last page keeps and drops the rest of
	// ([Mirrors.prune]).
	seen    map[mirrorKey]bool
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
		"People": o.People == nil,
	} {
		if absent {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%w: missing %s", ErrOptions, strings.Join(slices.Sorted(slices.Values(missing)), ", "))
	}
	m := &Mirrors{o: o, log: o.Logger, now: o.Now, known: map[mirrorKey]listed{}, running: map[mirrorKey]bool{},
		memory: map[mirrorKey]memory{}, health: map[mirrorKey]health{}, levels: map[mirrorKey]level{},
		queued: map[mirrorKey]queuedAttempt{}}
	if m.log == nil {
		m.log = logging.Get("gitlet.mirror")
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
// first, by id — whose last push here is a gitlet.MirrorInterval old, or that
// was never pushed here; then every pull mirror whose last sync is a
// gitlet.MirrorInterval old — listed least recently synced first, so the
// listing stops at the first synced within it, a sync that found its source
// level moving that instant too (gitlet.MirrorSync.Current) — less the ones
// this node attempted within that interval with nothing to land, or failed
// within [RetryAfter]. Read at floor where it is set, stale where it is zero.
// What it lists it remembers, for the turn that works it — every push mirror
// among them, due or not, so a nudge of its repository (a green row moved)
// finds it — and the listing's last page forgets every mirror the listing did
// not name ([Mirrors.prune]).
func (m *Mirrors) Due(ctx context.Context, floor statelog.Position, cursor string) ([]string, string, error) {
	fresh := statelog.Freshness{Level: statelog.ReadStale}
	if !floor.IsZero() {
		fresh = statelog.FloorAt(statelog.ReadSession, floor)
	}
	switch {
	case cursor == "" || strings.HasPrefix(cursor, importsArm):
		ans, err := m.o.Estate.Read(ctx, gitlet.ReadQuery{Kind: gitlet.ReadImportsDue,
			Cursor: strings.TrimPrefix(cursor, importsArm), Fresh: fresh})
		if err != nil {
			return nil, "", fmt.Errorf("mirror: list the repositories being imported: %w", err)
		}
		now := m.now()
		var out []string
		m.mu.Lock()
		if cursor == "" {
			m.seen = map[mirrorKey]bool{}
		}
		for _, c := range ans.Imports {
			key := mirrorKey{repo: c.ID}
			m.remember(key, listed{kind: kindImport, name: c.Name, source: c.Source, seq: c.Seq, at: c.Version})
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
		ans, err := m.o.Estate.Read(ctx, gitlet.ReadQuery{Kind: gitlet.ReadMirrorsDue,
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
				m.remember(key, listed{kind: kindPush, name: c.Name, at: ans.Position})
				if !m.waiting(key, now, gitlet.MirrorInterval) {
					out = append(out, c.ID)
				}
				continue
			}
			if c.From == nil {
				continue
			}
			if !c.SyncedAt.IsZero() && now.Sub(c.SyncedAt) < gitlet.MirrorInterval {
				// The first synced within its interval: every one after
				// it was synced later still.
				m.prune()
				return out, "", nil
			}
			key := mirrorKey{repo: c.ID}
			m.remember(key, listed{kind: kindSync, name: c.Name, source: gitlet.ImportSource(*c.From), at: c.Version})
			if m.waiting(key, now, gitlet.MirrorInterval) {
				continue
			}
			out = append(out, c.ID)
		}
		if ans.Next != "" {
			return out, mirrorsArm + ans.Next, nil
		}
		m.prune()
		return out, "", nil
	}
	return nil, "", fmt.Errorf("mirror: %q is no cursor of this worker's listing", cursor)
}

// remember keeps what the listing in progress says of key, for the turn that
// works it, and marks key seen by that listing. Called with mu held.
func (m *Mirrors) remember(key mirrorKey, l listed) {
	m.known[key] = l
	if m.seen != nil {
		m.seen[key] = true
	}
}

// prune ends a listing that ran from its first page to its last: everything
// this node remembers of a mirror the listing did not name and that is not
// running — what a listing said of it, its attempts' memory, its health, what
// its push last left the other host level with and its queued attempt — is
// dropped. So the worker holds what its last whole
// listing named — the company's imports and mirrors, as every Gitlet node lists
// them before its placement says which are its own — never every repository
// any listing ever named: one deleted, one whose mirror went, an import that
// ended. A pull mirror past the listing's stop, synced within its interval, is
// dropped with them — brought level within it, so its health says nothing —
// and listed again once its interval has passed. A listing that failed part
// way prunes nothing: the next one begins at its first page. Called with mu
// held.
func (m *Mirrors) prune() {
	seen := m.seen
	m.seen = nil
	if seen == nil {
		return
	}
	gone := func(key mirrorKey) bool { return !seen[key] && !m.running[key] }
	maps.DeleteFunc(m.known, func(key mirrorKey, _ listed) bool { return gone(key) })
	maps.DeleteFunc(m.memory, func(key mirrorKey, _ memory) bool { return gone(key) })
	maps.DeleteFunc(m.health, func(key mirrorKey, _ health) bool { return gone(key) })
	maps.DeleteFunc(m.levels, func(key mirrorKey, _ level) bool { return gone(key) })
	maps.DeleteFunc(m.queued, func(key mirrorKey, _ queuedAttempt) bool { return gone(key) })
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
// another host, which a turn's one backstop does not bound. One due while
// [MaxAttempts] run is QUEUED instead, and started as soon as one of them ends
// ([Mirrors.startQueued]) — never dropped for the next pass to find, which
// would start at most MaxAttempts a pass however short the attempts, and leave
// every mirror past what a pass's slots reach unsynced for as long as the
// ones ahead of it in the listing stay due. An attempt no listing named — a
// nudge for a repository this worker owes nothing — or one running, or one
// within its pace of the last ([pace], [RetryAfter]), is left to the next
// pass. ErrStopped once Stop has begun.
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
		if !ok {
			continue
		}
		// ASKED FOR, whatever becomes of the ask: a turn of this node's home
		// loop asks only for a repository this node is home to, so a mirror
		// asked for here is still this node's to raise code_mirror_failing
		// for — its attempt running past the promise, queued behind attempts
		// holding every slot, or waiting out a RetryAfter.
		m.claim(key, l, now)
		if m.running[key] || m.waiting(key, now, pace(l.kind)) {
			continue
		}
		if len(m.running) < MaxAttempts {
			delete(m.queued, key)
			m.start(key, l, floor, now)
			continue
		}
		q, held := m.queued[key]
		if !held {
			m.arrivals++
			q.arrived = m.arrivals
		}
		q.floor = further(q.floor, floor)
		m.queued[key] = q
	}
	return nil
}

// further is the later of two floors: a queued attempt asked again is synced
// at the furthest record that asked for it.
func further(a, b statelog.Position) statelog.Position {
	if before, err := a.Before(b); err == nil && before {
		return b
	}
	return a
}

// claim records that a turn of this node asked for key's mirror now —
// whether the ask started an attempt, queued one, or found one running or
// within its pace — in its health: so a mirror whose attempt runs past the
// promise, or that is queued here behind attempts holding every slot, is
// still this node's to raise code_mirror_failing for
// ([Mirrors.AlarmReading]). Where the attempt STARTED was the proxy for
// "still home", the attempts that held both slots past the promise went
// uncounted with every mirror queued behind them that had not been asked
// for since. An import has no health. Called with mu held.
func (m *Mirrors) claim(key mirrorKey, l listed, now time.Time) {
	if l.kind == kindImport {
		return
	}
	h, ok := m.health[key]
	if !ok {
		// NEVER BROUGHT LEVEL HERE: counted from the first time this node
		// is asked for it, so a home that has just taken a mirror over
		// gives it the whole promise before it is failing.
		h.level = now
	}
	h.name, h.asked = l.name, now
	m.health[key] = h
}

// startQueued starts queued attempts while fewer than [MaxAttempts] run: each
// time, the one THIS NODE ATTEMPTED LONGEST AGO — one it never attempted
// first — and, between two alike, the one queued first. Never in the order
// they were listed: a mirror whose attempt failed keeps its row's instant,
// and so its place at the front of `mirrors_due`, until one lands, and an
// order by listing would start it ahead of every mirror behind it each time
// its RetryAfter passed. One whose listing was spent or pruned meanwhile, that is running,
// or that is within its pace again is dropped: the next pass lists it again
// if it is still owed. Called with mu held, as an attempt ends.
func (m *Mirrors) startQueued(now time.Time) {
	for len(m.running) < MaxAttempts && !m.stopped {
		key, q, ok := m.nextQueued()
		if !ok {
			return
		}
		delete(m.queued, key)
		l, known := m.known[key]
		if !known || m.running[key] || m.waiting(key, now, pace(l.kind)) {
			continue
		}
		m.start(key, l, q.floor, now)
	}
}

// nextQueued is the queued attempt [Mirrors.startQueued] starts next, false
// for none. A walk of the queue: it holds at most the mirrors this node is
// home to, and is walked once an attempt, against an attempt's dial of
// another host. Called with mu held.
func (m *Mirrors) nextQueued() (mirrorKey, queuedAttempt, bool) {
	var (
		best  mirrorKey
		bestQ queuedAttempt
		found bool
	)
	for key, q := range m.queued {
		if found {
			a, b := m.memory[key].ended, m.memory[best].ended
			if a.After(b) || (a.Equal(b) && q.arrived > bestQ.arrived) {
				continue
			}
		}
		best, bestQ, found = key, q, true
	}
	return best, bestQ, found
}

// start runs key's attempt on a goroutine of the worker's own, and as it
// ends starts the queued attempts its slot frees ([Mirrors.startQueued]).
// Called with mu held.
func (m *Mirrors) start(key mirrorKey, l listed, floor statelog.Position, now time.Time) {
	m.running[key] = true
	m.claim(key, l, now)
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
		m.startQueued(mem.ended)
	}()
}

// forget drops everything this node remembers of key's mirror: a mirror its
// repository no longer has. Called from its own attempt, with mu NOT held.
func (m *Mirrors) forget(key mirrorKey) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.known, key)
	delete(m.health, key)
	delete(m.levels, key)
}

// AlarmReading fills the reading's mirror half at the worker's clock: how
// many mirrors — pull mirrors' syncs and push mirrors' pushes — this node is
// home to have not been brought level for longer than their own refresh
// promise, gitlet.MirrorInterval plus [AttemptDeadline] (ADR-0015), and the one
// failing longest, by its repository's name — what raises
// code_mirror_failing. A mirror no turn of this node has ASKED for within
// that promise is no longer this node's — its repository's home moved, or
// its mirror went — and is left out: the home loop asks only for the
// repositories this node is home to, and asks for every mirror that is not
// brought level at least once an interval (the listing names it again once
// its last attempt is an interval old, a failing one's a [RetryAfter], at
// most an interval) — whatever the ask then finds: so a mirror queued behind
// attempts holding every slot is counted, and so is one whose own attempt
// has run longer than the promise — a first push of a large repository —
// until that attempt ends: an attempt is bounded by what it moves
// (gitproc.RemoteDeadline), never by this alarm. Assigned here, field by
// field, so the reading's producers gate reaches each one through this
// concrete method (internal/engine's TestEveryAlarmReadingFieldHasAProducer).
func (m *Mirrors) AlarmReading(out *statelog.Reading) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now, promise := m.now(), gitlet.MirrorInterval+AttemptDeadline
	var (
		failing int
		worst   health
	)
	for _, h := range m.health {
		if now.Sub(h.asked) > promise || now.Sub(h.level) <= promise {
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
func (m *Mirrors) fault(ctx context.Context, p gitletfault.Point) error {
	if m.o.Faults == nil {
		return nil
	}
	return m.o.Faults.Hit(ctx, p, 0)
}
