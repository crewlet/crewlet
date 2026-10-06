package auxspend

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

var log = logging.Get("auxspend")

const (
	// FlushInterval is how often a node's ledger publishes what it holds.
	//
	// FIFTEEN SECONDS, the usage domain's own cadence (usage.FlushInterval,
	// held equal by this package's suite) and for its reason: it is the
	// cadence the live budget meters are pushed on, so the live rollup's
	// auxiliary spend trails the meter above it by at most one of the
	// meter's own refreshes. A named window then trails the counter by at
	// most two cadences — this flush, and the usage publisher's tick that
	// derives the day the record landed in. A turn's own buckets are
	// flushed as it ends ([Ledger.FlushTurn]), so a turn's page does not
	// wait for the timer; the timer is what every other stage relies on,
	// and the safety net under a turn whose end never flushed.
	FlushInterval = 15 * time.Second

	// MaxPending bounds the sealed records a ledger keeps for a retry after
	// their publish failed.
	//
	// FOUR THOUSAND AND NINETY-SIX: a record is about six hundred bytes, so
	// the backlog is under three megabytes, and a busy company keeps a few
	// dozen keys open per flush — so it holds a broker outage of most of an
	// hour before the oldest is dropped. A dropped record is spend the
	// counters still hold and the rollups do not, and it is logged with its
	// tokens so the gap can be named.
	MaxPending = 4096
)

// Seat is who an auxiliary call's spend belongs to: an agent seat, or a person
// — a person's question on the operator stage, or a background pass on a unit
// a person leads.
type Seat struct {
	// AgentID, Handle and Role name an agent seat.
	AgentID string
	Handle  string
	Role    string

	// Person and PersonRole name a person: the human seat's handle — the
	// one their credential is bound to, or the one leading the unit — and
	// its role. Set instead of the three above.
	Person     string
	PersonRole string
}

// who is the seat's identity in a bucket's key: the derived agent id, which is
// the one every node computes alike, or the person's handle.
func (s Seat) who() string {
	switch {
	case s.Person != "":
		return "person:" + s.Person
	case s.AgentID != "":
		return s.AgentID
	}
	return "handle:" + s.Handle
}

// Call is one completion the seam saw return: who it was for, why, what it
// cost and when.
type Call struct {
	Seat Seat
	Use  Use

	// Model is the model the completion reported, and ProviderKey the
	// configured entry that served it.
	Model       string
	ProviderKey string

	// Day is the company day the counters charged it in (`2026-09-23`).
	Day string

	Started, Ended time.Time

	// Failed is a call that returned an error. Still a call: it may have
	// been billed, and its tokens, where the provider reported any, are
	// spend like any other.
	Failed bool

	Spent Spent

	// Trace is the trace the call was made under. A coalesced record
	// carries its first call's.
	Trace events.TraceContext
}

// Publisher is the queue's publish, as a ledger uses it.
type Publisher interface {
	Publish(ctx context.Context, subject string, ev *events.Event) error
}

// key is one bucket's identity: every dimension a record is filed under.
type key struct {
	stage, who, turn, purpose, model, provider, day string
}

// bucket is one key's calls since its last flush.
type bucket struct {
	seat      Seat
	workKey   string
	spent     Spent
	failed    int
	started   time.Time
	ended     time.Time
	durations time.Duration
	trace     events.TraceContext
}

// sealed is a record a flush has cut from its bucket: its id and its instant
// fixed, so every attempt to publish it is the same event.
type sealed struct {
	turn  string
	ev    *events.Event
	spent Spent
}

// Ledger is one node's auxiliary spend between flushes.
//
// SAFE FOR CONCURRENT USE: every auxiliary call on the node adds to it, and a
// flush may run on the timer, at a turn's end and at the stop at once. A
// publish never runs under its lock, so a slow broker holds up no call.
type Ledger struct {
	pub    Publisher
	logger *slog.Logger

	mu      sync.Mutex
	open    map[key]*bucket
	pending []sealed

	runMu sync.Mutex
	stop  context.CancelFunc
	done  chan struct{}
}

// NewLedger is an empty ledger publishing through pub. A nil publisher is a
// ledger that publishes nothing and keeps nothing — a node with no queue,
// which records no event of any kind.
func NewLedger(pub Publisher) *Ledger {
	return &Ledger{pub: pub, logger: log, open: map[key]*bucket{}}
}

// Add files one call in its bucket. Nil-safe.
func (l *Ledger) Add(c Call) {
	if l == nil || l.pub == nil {
		return
	}
	k := key{
		stage: string(c.Use.Stage), who: c.Seat.who(), turn: c.Use.TurnID,
		purpose: string(c.Use.Purpose), model: c.Model, provider: c.ProviderKey, day: c.Day,
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.open[k]
	if b == nil {
		b = &bucket{seat: c.Seat, workKey: c.Use.WorkKey, started: c.Started,
			ended: c.Ended, trace: c.Trace}
		l.open[k] = b
	}
	spent := c.Spent
	spent.Calls = max(spent.Calls, 1)
	b.spent.add(spent)
	if c.Failed {
		b.failed++
	}
	if !c.Started.IsZero() && (b.started.IsZero() || c.Started.Before(b.started)) {
		b.started = c.Started
	}
	if c.Ended.After(b.ended) {
		b.ended = c.Ended
	}
	if took := c.Ended.Sub(c.Started); took > 0 {
		b.durations += took
	}
	if b.workKey == "" {
		b.workKey = c.Use.WorkKey
	}
}

// Flush publishes every bucket and every record still waiting from a failed
// publish, oldest first. It reports nothing: a record that could not be
// published is kept for the next flush, and one past [MaxPending] is dropped
// and logged.
func (l *Ledger) Flush(ctx context.Context) {
	l.flush(ctx, func(key) bool { return true }, true)
}

// FlushTurn publishes one turn's buckets now — called as the turn ends, so the
// turn's page reads what its context and its rewrites cost when it reads the
// turn, rather than a flush interval later.
func (l *Ledger) FlushTurn(ctx context.Context, turnID string) {
	if turnID == "" {
		return
	}
	l.flush(ctx, func(k key) bool { return k.turn == turnID }, false)
}

func (l *Ledger) flush(ctx context.Context, pick func(key) bool, retry bool) {
	if l == nil || l.pub == nil {
		return
	}
	l.mu.Lock()
	var out []sealed
	if retry {
		out, l.pending = l.pending, nil
	}
	for k, b := range l.open {
		if !pick(k) {
			continue
		}
		out = append(out, seal(k, b))
		delete(l.open, k)
	}
	l.mu.Unlock()

	// THE FIRST REFUSAL ENDS THE FLUSH. A broker that refused one record
	// is down for the next, and asking it for each of a backlog of
	// thousands would spend the flush on refusals and the log on one line
	// apiece; what is left is kept, untried, for the next flush, and the
	// refusal is said once, with what it holds back.
	var failed []sealed
	for i, s := range out {
		err := l.pub.Publish(ctx, topics.Event(s.ev.Type), s.ev)
		if err == nil {
			continue
		}
		failed = append([]sealed(nil), out[i:]...)
		var held Spent
		for _, f := range failed {
			held.add(f.spent)
		}
		l.logger.WarnContext(ctx, "auxiliary_spend_unpublished", "error", err.Error(),
			"records", len(failed), "tokens", held.Tokens(),
			"detail", "kept, and published again by the next flush")
		break
	}
	if len(failed) == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pending = append(failed, l.pending...)
	if over := len(l.pending) - MaxPending; over > 0 {
		var lost Spent
		for _, s := range l.pending[:over] {
			lost.add(s.spent)
		}
		l.logger.WarnContext(ctx, "auxiliary_spend_dropped", "records", over,
			"tokens", lost.Tokens(),
			"detail", "the counters hold this spend and the spend rollups will not: "+
				"the broker refused these records for longer than the ledger keeps them")
		l.pending = append([]sealed(nil), l.pending[over:]...)
	}
}

// seal cuts one bucket into its record.
func seal(k key, b *bucket) sealed {
	rec := types.AuxiliarySpend{
		Stage:   types.AuxStage(k.stage),
		Purpose: types.AuxPurpose(k.purpose),
		TurnID:  k.turn, WorkKey: b.workKey,
		Model: k.model, ProviderKey: k.provider, Day: k.day,
		Calls: b.spent.Calls, FailedCalls: b.failed,
		InputTokens: b.spent.Input, OutputTokens: b.spent.Output,
		TotalTokens:     b.spent.Tokens(),
		CacheReadTokens: b.spent.CacheRead, CacheWriteTokens: b.spent.CacheWrite,
		StartedAt:  b.started.UTC(),
		EndedAt:    b.ended.UTC(),
		DurationMS: int(b.durations / time.Millisecond),
	}
	source := b.seat.Role
	if b.seat.Person != "" {
		rec.ActorSeat, rec.ActorRole = b.seat.Person, b.seat.PersonRole
		source = b.seat.Person
	} else {
		rec.Agent, rec.AgentHandle, rec.RoleName = b.seat.AgentID, b.seat.Handle, b.seat.Role
	}
	// THE ID AND THE INSTANT ARE FIXED HERE, once — the id minted by
	// events.New, the instant set below — and the sealed event is what every
	// attempt publishes: a retry is then the same event, which the store's
	// (time, id) key and the live window's id index both collapse.
	ev := events.New(rec, b.trace)
	// THE LAST CALL'S INSTANT, not the flush's: it is the day the counters
	// charged these calls in, and the bucket's key keeps every call of it
	// inside that day.
	if !b.ended.IsZero() {
		ev.Timestamp = b.ended.UTC()
	}
	ev.Source = source
	return sealed{turn: k.turn, ev: ev, spent: b.spent}
}

// Run flushes on [FlushInterval] until [Ledger.Stop]. A second Run while one is
// running does nothing.
func (l *Ledger) Run(ctx context.Context) {
	if l == nil || l.pub == nil {
		return
	}
	l.runMu.Lock()
	if l.stop != nil {
		l.runMu.Unlock()
		return
	}
	loop, cancel := context.WithCancel(ctx)
	l.stop, l.done = cancel, make(chan struct{})
	done := l.done
	l.runMu.Unlock()

	go func() {
		defer close(done)
		ticker := time.NewTicker(FlushInterval)
		defer ticker.Stop()
		for {
			select {
			case <-loop.Done():
				return
			case <-ticker.C:
				l.Flush(loop)
			}
		}
	}()
}

// Stop ends the timer, waits for a flush in flight, and flushes once more on
// ctx — the stop's own budget, since the node is going and anything left is
// lost. It is ORDERED by its caller after every producer of auxiliary calls
// has stopped and before the stream closes. Nil-safe; a second Stop flushes
// what arrived since.
func (l *Ledger) Stop(ctx context.Context) {
	if l == nil {
		return
	}
	l.runMu.Lock()
	stop, done := l.stop, l.done
	l.stop, l.done = nil, nil
	l.runMu.Unlock()
	if stop != nil {
		stop()
		<-done
	}
	l.Flush(ctx)
	l.mu.Lock()
	left := len(l.pending)
	var lost Spent
	for _, s := range l.pending {
		lost.add(s.spent)
	}
	l.pending = nil
	l.mu.Unlock()
	if left > 0 {
		l.logger.WarnContext(ctx, "auxiliary_spend_lost_at_stop", "records", left,
			"tokens", lost.Tokens(),
			"detail", "the broker refused these records at the stop; the counters "+
				"hold this spend and the spend rollups will not")
	}
}
