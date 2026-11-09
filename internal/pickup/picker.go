package pickup

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// The seams a [Picker] decides through, each kept to what it asks.

// Holder answers what only the node holding a seat knows about it.
type Holder interface {
	Conditions(ctx context.Context, seat Seat) Conditions
}

// ReadyReader reads one seat's ready work.
type ReadyReader interface {
	ReadyWork(ctx context.Context, q worklet.ReadyWorkQuery, now time.Time) (worklet.ReadyWork, error)
}

// Ledger is the fleet's completion ledger: which pickups a seat already had
// its turn on. It fails open, answering nothing worked on an error.
type Ledger interface {
	Worked(ctx context.Context, handle string, keys []string) map[string]bool
}

// Claims is the fleet's claim store, three-valued: an error is neither won
// nor lost.
type Claims interface {
	Claim(ctx context.Context, key string, ttl time.Duration, now time.Time) (bool, error)
	Release(ctx context.Context, key string) error
}

// Publisher sends a wake to a seat's inbox.
type Publisher interface {
	Publish(ctx context.Context, topic string, ev *events.Event) error
}

// Seat is one agent seat a pass decides for.
type Seat struct {
	ID     uuid.UUID
	Handle string
	Role   string
}

// Outcome is what a pass decided for one seat.
type Outcome struct {
	Seat Seat

	// Woken is a pickup published, naming Item.
	Woken bool
	Item  worklet.ReadyItem
	Wake  uuid.UUID

	// Skip is why nothing was published; Ready how many ready items the
	// seat held; Err the failure behind [SkipUnknown].
	Skip  Skip
	Ready int
	Err   error
}

// Settled reports an outcome a pass reached only after finding the seat
// idle: the decisions an operator counts, beside the conditions it reads.
func (o Outcome) Settled() bool {
	switch o.Skip {
	case "", SkipNothing, SkipOffered, SkipCooldown, SkipClaimed, SkipUnknown:
		return o.Woken || o.Skip != ""
	}
	return false
}

// Picker decides seats' pickups, pass by pass: the one sequence every node
// that holds seats runs.
type Picker struct {
	Holder    Holder
	Ready     ReadyReader
	Ledger    Ledger
	Claims    Claims
	Publisher Publisher
	Trace     func(ctx context.Context) events.TraceContext
	Now       func() time.Time

	// Cooldown is the least time between two pickups of one seat.
	Cooldown time.Duration

	mu   sync.Mutex
	idle map[uuid.UUID]bool
}

// Pass decides every seat once, and forgets the settle of any seat it was
// not handed, so a seat that comes back settles again.
func (p *Picker) Pass(ctx context.Context, seats []Seat) []Outcome {
	out := make([]Outcome, 0, len(seats))
	held := make(map[uuid.UUID]bool, len(seats))
	for _, seat := range seats {
		held[seat.ID] = true
		out = append(out, p.decide(ctx, seat))
	}
	p.forget(held)
	return out
}

// Forget drops every seat's settle, for a pass that decides nothing (the
// pickup switched off, a company with no model).
func (p *Picker) Forget() { p.forget(nil) }

func (p *Picker) forget(held map[uuid.UUID]bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id := range p.idle {
		if !held[id] {
			delete(p.idle, id)
		}
	}
}

// settle records whether this pass found the seat idle, and answers whether
// the previous pass did.
func (p *Picker) settle(seat uuid.UUID, idleNow bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.idle == nil {
		p.idle = map[uuid.UUID]bool{}
	}
	before := p.idle[seat]
	p.idle[seat] = idleNow
	return before
}

func (p *Picker) decide(ctx context.Context, seat Seat) Outcome {
	out := Outcome{Seat: seat}
	cond := p.Holder.Conditions(ctx, seat)
	cond.IdleBefore = p.settle(seat.ID, IdleNow(cond))
	if ok, skip := Idle(cond); !ok {
		out.Skip = skip
		return out
	}
	now := p.Now()
	// THE NODE'S OWN COPY: the settle gave the last turn's writes a pass to
	// land, and a stale answer costs at most a pickup the ledger refuses.
	ready, err := p.Ready.ReadyWork(ctx, worklet.ReadyWorkQuery{
		Handle: seat.Handle, Level: statelog.ReadStale,
	}, now)
	if err != nil {
		return failed(out, fmt.Errorf("read %s's ready work: %w", seat.Handle, err))
	}
	out.Ready = len(ready.Items)
	if out.Ready == 0 {
		out.Skip = SkipNothing
		return out
	}
	worked := p.Ledger.Worked(ctx, seat.Handle, WorkKeys(seat.ID, ready.Items))
	item, key, ok := Choose(seat.ID, ready.Items, worked)
	if !ok {
		out.Skip = SkipOffered
		return out
	}
	out.Item = item
	// THE SEAT'S COOLDOWN FIRST, then the pickup's own claim, held for the
	// fleet's longest claim so a copy waiting in the inbox is not published
	// again before its turn records it in the ledger. A pickup a
	// second node already claimed costs this seat one cooldown, where the
	// other order would spend the item's claim on a seat still cooling.
	if skip, err := p.claim(ctx, CooldownKey(seat.ID), p.Cooldown, now, SkipCooldown); skip != "" {
		out.Skip, out.Err = skip, err
		return out
	}
	if skip, err := p.claim(ctx, key.ClaimKey(), coord.MaxClaimTTL, now, SkipClaimed); skip != "" {
		out.Skip, out.Err = skip, err
		return out
	}
	var trace events.TraceContext
	if p.Trace != nil {
		trace = p.Trace(ctx)
	}
	ev := Event(seat.ID, seat.Role, item, key, out.Ready, trace)
	if err := p.Publisher.Publish(ctx, topics.AgentInbox(seat.ID), ev); err != nil {
		// NOTHING WAS SPENT THAT A RETRY CANNOT TAKE AGAIN: a copy that
		// landed after all carries the same id, which the inbox and the
		// ledger collapse, so both claims are given back for the next pass.
		release := context.WithoutCancel(ctx)
		for _, claim := range []string{key.ClaimKey(), CooldownKey(seat.ID)} {
			if rerr := p.Claims.Release(release, claim); rerr != nil {
				err = fmt.Errorf("%w; and the claim %s is kept until it lapses: %w", err, claim, rerr)
			}
		}
		return failed(out, fmt.Errorf("publish %s's pickup of %s: %w", seat.Handle, item.Key, err))
	}
	out.Woken, out.Wake = true, ev.ID
	return out
}

// claim takes one fleet claim, answering the skip that holds the seat back,
// or nothing when the claim is this node's.
func (p *Picker) claim(ctx context.Context, key string, ttl time.Duration, now time.Time,
	lost Skip) (Skip, error) {

	won, err := p.Claims.Claim(ctx, key, ttl, now)
	switch {
	case err != nil:
		// NOT TAKEN: a pickup is never urgent, and the next pass asks again.
		return SkipUnknown, fmt.Errorf("claim %s: %w", key, err)
	case !won:
		return lost, nil
	}
	return "", nil
}

func failed(out Outcome, err error) Outcome {
	out.Skip, out.Err = SkipUnknown, err
	return out
}
