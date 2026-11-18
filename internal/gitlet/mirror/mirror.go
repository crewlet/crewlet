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

// MaxAttempts is how many attempts (imports, syncs and pushes) one node runs
// at once: TWO, so one large import cannot hold back every mirror the node is
// home to, while more would only queue on the one transfer-lane share their
// fetches and pushes take turns of (share, attempt.go). One due while two run
// WAITS here and starts as one ends ([Mirrors.Work]), not on the next pass.
// JUDGMENT, NOT A MEASUREMENT: an idle source's attempt is one negotiation of
// about a second, so the gitlet.MirrorInterval promise holds for some 1 800
// mirrors a node is home to while their sources are idle, fewer while attempts
// move bytes; past it, code_mirror_failing names the one waiting longest.
const MaxAttempts = 2

// RetryAfter is how long after a failed attempt this node lists the
// repository again: a minute, doubled with every failure in a row, capped at
// gitlet.MirrorInterval, so a down source or a refused credential is asked
// about four times in its first quarter hour and then once an interval, not
// sixty times an hour. A permanent refusal ends an import, never retried.
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

// Estate is the code log as an attempt reaches it: listings through Read, an
// import's chunks and end through Import, a sync through MirrorSync. The
// router's code seam (estate.CodeSeam) satisfies it, each write raising this
// node's floors past its record so the copy the next step syncs holds it.
type Estate interface {
	Read(ctx context.Context, q gitlet.ReadQuery) (gitlet.ReadAnswer, error)
	Import(ctx context.Context, p gitlet.ImportProposal) (gitlet.SyncResult, error)
	MirrorSync(ctx context.Context, p gitlet.MirrorSyncProposal) (gitlet.SyncResult, error)
}

// Cache is this node's git cache as an attempt reaches it (*cache.Cache
// satisfies it): the repository synced and pinned, its live packs (each
// excluded from the cut), the fetch's scratch, and the adoption of what landed.
type Cache interface {
	Sync(ctx context.Context, repoID string, fresh statelog.Freshness) (cache.Synced, error)
	Packs(s cache.Synced) ([]gitlet.PackRow, error)
	NewScratch(ctx context.Context, s cache.Synced, id string) (*cache.Scratch, error)
	Adopt(ctx context.Context, repoID string, sc *cache.Scratch, packs ...gitlet.PackRef) error
}

// Pool is the node's git pool: the fetch and the source's HEAD listing in its
// transfer lane, scratch reads in its read lane, and the cut in a maintenance
// slot the attempt waits for and holds (Reserve), since that lane refuses at
// once when full and the cut follows a fetch no attempt should make twice.
type Pool interface {
	Run(ctx context.Context, c gitproc.Cmd) (gitproc.Result, error)
	Reserve() (*gitproc.Reserved, error)
}

// Dial is the one outbound dial, as the engine builds it: the source's URL
// resolved and held to the public-address rule (gitproc.ResolveRemote; Tier A
// gitlet.import_allow_private its one escape), with the header carrying the
// credential its ${VAR} resolves to, "" for an anonymous source
// (gitproc.Remote.WithHeader). An error is the attempt's.
type Dial func(ctx context.Context, url, credential string) (gitproc.Remote, error)

// Publisher is a node's event stream as a push and an import's end announce
// themselves on it: queue.Queue satisfies it.
type Publisher interface {
	Publish(ctx context.Context, subject string, ev *events.Event) error
}

// eventBudget bounds one announcement (code_mirror_pushed, code_imported):
// FIVE SECONDS, the JetStream client's request timeout, as every Gitlet
// announcement is (internal/gitlet's checkEventBudget). What it accounts for
// has already landed, so only the attempt's end waits on it.
const eventBudget = 5 * time.Second

// announce publishes e, the account of something this node did for repoID,
// bounded by [eventBudget] and detached from the attempt's context, since what
// it accounts for has landed; a refusal is logged as unpublished with detail
// and undoes nothing. Events nil announces nothing.
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
// attempt ended: ONE MINUTE, the home loop's backstop. A busy repository
// nudges its home on every record; once a minute keeps a mirror a minute
// behind a moved green for one read of its rows and copy a minute. The other
// host is dialed only where what the mirror covers moved, or once an interval
// regardless ([Mirrors.stillLevel]).
const PushPace = time.Minute

// AttemptDeadline is the GRACE code_mirror_failing gives an attempt in flight
// beyond the mirror's interval: FIFTEEN MINUTES, one more gitlet.MirrorInterval,
// so a mirror fails after two intervals not brought level, not one slow
// attempt. Never a kill deadline: an attempt is bounded by what it moves
// (gitproc.RemoteDeadline), which a large first push can far exceed.
// JUDGMENT: P8 promises the interval alone, and an alarm at exactly it would
// fire on every attempt a second past it.
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
	// blob limit, the cut's window and the bytes a fetch may bring.
	Limits func() gitlet.Limits
	// Roster is the chart this node holds NOW, read at every push: the
	// mirror's seat is judged against it before each push
	// (gitlet.MirrorSeatRefusal), since a later chart may take its read away.
	Roster func() gitlet.Roster
	// People answers who holds a human seat and with which apps
	// (gitlet.People), read at every push as a person's seat: the mirror
	// publishes only what that seat reads, and a person reads only through
	// Gitlet (ADR-0110), which an administrator may take away after the
	// mirror was set.
	People gitlet.People
	// Events is this node's event stream for code_mirror_pushed and
	// code_imported; nil publishes none.
	Events Publisher
	// Faults is nil in production (internal/gitlet/gitletfault).
	Faults gitletfault.Injector
	// Metrics nil records nothing; Logger nil is the package's own
	// (component gitlet.mirror); Now nil is time.Now.
	Metrics *metrics.Recorder
	Logger  *slog.Logger
	Now     func() time.Time
	// Ended, nil in production, is told each attempt's end once the worker
	// has recorded it and unlocked: a case waits on it before moving Now,
	// since the end is stamped from Now after the dial.
	Ended func(repoID string, out bool, err error)
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

// mirrorKey is one WAY a repository is mirrored: IN (out false), its import
// then its pull mirror's syncs, ONE line of attempts since both move its refs
// from one source (never two at once; the first sync waits an interval after
// the import's last attempt, as each sync after the one before); or OUT, its
// push mirror's pushes, which move nothing of its own and so run beside them.
type mirrorKey struct {
	repo string
	out  bool
}

// health is THE HOME'S FAILURE STATE of one mirror (M6-D16): when it was last
// brought level (or first asked for here, if never), when a turn last asked
// for it whatever the ask became ([Mirrors.claim]), and its repository's name.
// In memory alone (package doc, "The push mirror follows green").
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

// pace is how long after its last attempt ended an attempt of kind k waits:
// an import none, a sync its interval, a push PushPace; [RetryAfter] after a
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
	// seen is every mirror the listing in progress has named, nil between
	// listings; its last page drops the rest ([Mirrors.prune]).
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

// Due is one page of the repositories this node owes an attempt: every import;
// then every push mirror (`mirrors_due` lists them first, by id) not pushed
// here within a gitlet.MirrorInterval; then every pull mirror whose last sync
// is an interval old, least recently synced first, so the listing stops at the
// first synced within it (a level sync moves that instant too,
// gitlet.MirrorSync.Current); less those this node attempted within that
// interval or failed within [RetryAfter]. Read at floor where set, stale where
// zero. It remembers what it lists for the turn that works it, every push
// mirror due or not so a nudge (a green row moved) finds it, and the last page
// forgets every mirror the listing did not name ([Mirrors.prune]).
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
				// A PUSH MIRROR'S CANDIDATE CARRIES NO VERSION: its attempt
				// reads the rows again at or past this listing's position, so
				// it pushes from a snapshot holding every record the listing saw.
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
// remembered of a mirror the listing did not name and that is not running
// (listing, memory, health, levels, queued attempt) is dropped, so the worker
// holds what its last whole listing named (the company's, as every Gitlet node
// lists them before placement), never every repository any listing ever
// named. A pull mirror past the stop, synced within its interval, is dropped
// too (brought level, so its health says nothing) and listed again once the
// interval passes. A listing that failed part way prunes nothing: the next
// begins at its first page. Called with mu held.
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

// Work STARTS repoID's attempts its listings named and none runs (its import
// or sync, and its push), each on the worker's own goroutine, and returns: an
// attempt dials another host, which a turn's backstop does not bound. One due
// while [MaxAttempts] run is QUEUED and started as one ends
// ([Mirrors.startQueued]), never left to the next pass, which would start at
// most MaxAttempts a pass and starve the mirrors listed behind them. An
// attempt no listing named, one running, or one within its pace ([pace],
// [RetryAfter]) is left to the next pass. ErrStopped once Stop has begun.
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
		// ASKED FOR, whatever becomes of the ask: a turn asks only for a
		// repository this node is home to, so the mirror is still this
		// node's to raise code_mirror_failing for ([Mirrors.claim]).
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

// claim records in key's health that a turn of this node asked for its mirror
// now, whether the ask started an attempt, queued one, or found one running or
// within its pace: so a mirror whose attempt runs past the promise, or that is
// queued behind attempts holding every slot, still counts toward
// code_mirror_failing here ([Mirrors.AlarmReading]). Stamping only a started
// attempt would miss both. An import has no health. Called with mu held.
func (m *Mirrors) claim(key mirrorKey, l listed, now time.Time) {
	if l.kind == kindImport {
		return
	}
	h, ok := m.health[key]
	if !ok {
		// NEVER BROUGHT LEVEL HERE: counted from the first ask, so a home
		// that just took a mirror over gives it the whole promise.
		h.level = now
	}
	h.name, h.asked = l.name, now
	m.health[key] = h
}

// startQueued starts queued attempts while fewer than [MaxAttempts] run, each
// time the one THIS NODE ATTEMPTED LONGEST AGO (never attempted first), ties
// to the one queued first. Never listing order: a failing mirror keeps its
// row's instant, so its place at the front of `mirrors_due`, and would jump
// every mirror behind it each time its RetryAfter passed. One whose listing
// was spent or pruned, that is running, or that is within its pace again is
// dropped for the next pass to list. Called with mu held, as an attempt ends.
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
// for none. A linear walk is enough: the queue holds at most the mirrors this
// node is home to, walked once an attempt against its dial of another host.
// Called with mu held.
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
// ends records its end, starts the queued attempts its slot frees
// ([Mirrors.startQueued]) and then tells [Options.Ended]. Called with mu
// held.
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
		if m.o.Ended != nil {
			// Deferred ahead of the lock: told after the record and the unlock.
			defer m.o.Ended(key.repo, key.out, err)
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		if !key.out {
			// What the listing said is SPENT by the attempt it started: an
			// import's Seq and version are stale once a chunk landed, so a
			// later nudge leaves the repository to the next listing, which
			// reads the rows again. A push mirror's listing only says it
			// exists (its attempt reads the rest), so a nudge still finds it.
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

// AlarmReading fills the reading's mirror half at the worker's clock: how many
// mirrors (syncs and pushes) this node is home to have not been brought level
// for longer than their promise, gitlet.MirrorInterval plus [AttemptDeadline]
// (ADR-0015), and the one failing longest by repository name, which raises
// code_mirror_failing. A mirror no turn here has ASKED for within the promise
// is no longer this node's (its home moved, or its mirror went) and is left
// out: the home loop asks for every mirror not brought level at least once an
// interval, so a queued mirror counts, and so does one whose own attempt
// outruns the promise, since this alarm never bounds an attempt. Assigned
// field by field so internal/engine's TestEveryAlarmReadingFieldHasAProducer
// reaches each one through this concrete method.
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
