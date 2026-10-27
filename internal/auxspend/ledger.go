package auxspend

import (
	"context"
	"time"

	"github.com/crewlet/crewlet/internal/coalesce"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/logging"
)

var log = logging.Get("auxspend")

// Seat is who an auxiliary call's spend belongs to: an agent seat, or a person
// — a person's question on the operator stage, or a background pass on a unit
// a person leads.
type Seat struct {
	// AgentID, Handle and Role name an agent seat.
	AgentID string
	Handle  string
	Role    string

	// Person and PersonRole name a person: the human seat's handle — the
	// one the asking principal acts as (iam.RecordOwner: the seat a person
	// holds, or the one the directory binds a token to), or the one
	// leading the unit — and its role. Set instead of the three above. A
	// seat HANDLE and never an identity-estate person id, so a person's
	// spend lands on the seat their work is charged to whoever holds it.
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

// Ledger is one node's auxiliary spend between flushes: a [coalesce.Ledger] of
// `auxiliary_spend` records, one per key per flush, published every
// [coalesce.FlushInterval] off the caller's path, a refused one kept sealed
// for the next flush, up to [coalesce.MaxPending].
//
// FIFTEEN SECONDS, the usage domain's cadence, which is also the cadence the
// live budget meters are pushed on, so the live rollup's auxiliary spend
// trails the meter above it by at most one of the meter's own refreshes, and a
// named window trails the counter by at most two cadences — this flush, and
// the usage publisher's tick that derives the day the record landed in. A
// turn's own buckets are flushed before its end and before its reflection
// pass's sentinel ([Ledger.FlushTurn]), so a turn's page does not wait for the
// timer; the timer is what the background and operator stages rely on, and the
// safety net under a flush that was refused.
//
// SAFE FOR CONCURRENT USE: every auxiliary call on the node adds to it, and a
// flush may run on the timer, at a turn's end and at the stop at once. A
// publish never runs under its lock, so a slow broker holds up no call.
type Ledger struct {
	l *coalesce.Ledger[key, bucket]
}

// NewLedger is an empty ledger publishing through pub. A nil publisher is a
// ledger that publishes nothing and keeps nothing — a node with no queue,
// which records no event of any kind.
func NewLedger(pub coalesce.Publisher) *Ledger {
	return &Ledger{l: coalesce.New(pub, coalesce.Records[key, bucket]{
		Type: types.AuxiliarySpend{}.EventType(),
		Seal: seal,
		Held: func(held []*bucket) []any {
			var spent Spent
			for _, b := range held {
				spent.add(b.spent)
			}
			return []any{"tokens", spent.Tokens()}
		},
		Lost:   "the counters hold this spend and the spend rollups will not",
		Logger: log,
	})}
}

// Add files one call in its bucket. Nil-safe.
func (l *Ledger) Add(c Call) {
	if l == nil {
		return
	}
	k := key{
		stage: string(c.Use.Stage), who: c.Seat.who(), turn: c.Use.TurnID,
		purpose: string(c.Use.Purpose), model: c.Model, provider: c.ProviderKey, day: c.Day,
	}
	l.l.Add(k, func(b *bucket, fresh bool) {
		if fresh {
			*b = bucket{seat: c.Seat, workKey: c.Use.WorkKey, started: c.Started,
				ended: c.Ended, trace: c.Trace}
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
	})
}

// Flush publishes every bucket and every record still waiting from a failed
// publish, oldest first ([coalesce.Ledger.Flush]). Nil-safe.
func (l *Ledger) Flush(ctx context.Context) {
	if l == nil {
		return
	}
	l.l.Flush(ctx)
}

// FlushTurn publishes one turn's buckets now, of every stage — called before a
// turn segment publishes its end, before its reflection pass publishes its
// sentinel, and once its conversation entry's rewrites are made. The turn's
// page asks for the turn again at the first two, so each record is on the
// stream before the event that makes a reader ask, rather than a flush
// interval after it. Nil-safe.
func (l *Ledger) FlushTurn(ctx context.Context, turnID string) {
	if l == nil || turnID == "" {
		return
	}
	l.l.FlushWhere(ctx, func(k key) bool { return k.turn == turnID })
}

// seal cuts one bucket into its record.
func seal(k key, b *bucket) *events.Event {
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
	return ev
}

// Run flushes on [coalesce.FlushInterval] until [Ledger.Stop]. A second Run
// while one is running does nothing. Nil-safe.
func (l *Ledger) Run(ctx context.Context) {
	if l == nil {
		return
	}
	l.l.Run(ctx)
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
	l.l.Stop(ctx)
}
