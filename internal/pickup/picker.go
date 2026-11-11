package pickup

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
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

	// Holds reports whether the organisation still holds a seat: a requester
	// it no longer holds is worked for as a stranger. Nil holds every seat.
	Holds func(handle string) bool

	mu   sync.Mutex
	idle map[uuid.UUID]bool

	// offered is the fingerprint of each seat's ready set the last pass
	// found every item of already handed over, so an unchanged set costs
	// no ledger read ([worklet.ReadyWork.Fingerprint]), until the first
	// recheck of one of its items comes due.
	offered map[uuid.UUID]offeredSet
}

// offeredSet is a ready set every item of which was handed over: its
// fingerprint, and the instant the first of its keys changes on time alone
// (zero where none will).
type offeredSet struct {
	fingerprint string
	until       time.Time
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
	for id := range p.offered {
		if !held[id] {
			delete(p.offered, id)
		}
	}
}

// remember records, or with "" forgets, that a seat's ready set named by
// fingerprint was all handed over, until the instant one of its items comes
// due for a recheck (zero for never).
func (p *Picker) remember(seat uuid.UUID, fingerprint string, until time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if fingerprint == "" {
		delete(p.offered, seat)
		return
	}
	if p.offered == nil {
		p.offered = map[uuid.UUID]offeredSet{}
	}
	p.offered[seat] = offeredSet{fingerprint: fingerprint, until: until}
}

// allOffered reports a ready set remembered as all handed over at now: the
// same fingerprint, and no item of it come due for a recheck since.
func (p *Picker) allOffered(seat uuid.UUID, fingerprint string, now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	set, ok := p.offered[seat]
	return ok && fingerprint != "" && set.fingerprint == fingerprint &&
		(set.until.IsZero() || now.Before(set.until))
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
	cooled := false
	pick, err := p.choose(ctx, seat, now, func(c candidate) (bool, Skip, error) {
		return p.hold(ctx, seat.ID, c, now, &cooled)
	})
	out.Ready, out.Item = pick.ready, pick.item
	switch {
	case pick.skip != "":
		out.Skip, out.Err = pick.skip, err
		return out
	case err != nil:
		return failed(out, err)
	}
	key := pick.key
	var trace events.TraceContext
	if p.Trace != nil {
		trace = p.Trace(ctx)
	}
	item := pick.item
	if w := item.For; w != nil && w.Kind == types.WorkingForSeat && p.Holds != nil && !p.Holds(w.Seat) {
		item.For = &types.WorkingFor{Kind: types.WorkingForStranger}
	}
	ev := Event(seat.ID, seat.Role, item, key, out.Ready, pick.capped, trace)
	if err := p.Publisher.Publish(ctx, topics.AgentInbox(seat.ID), ev); err != nil {
		// NOTHING WAS SPENT THAT A RETRY CANNOT TAKE AGAIN: a copy that
		// landed after all carries the same id, which the inbox and the
		// ledger collapse, so both claims are given back for the next pass.
		release := context.WithoutCancel(ctx)
		claims := []string{key.ClaimKey(), key.ProgressKey()}
		if cooled {
			claims = append(claims, CooldownKey(seat.ID))
		}
		for _, claim := range claims {
			if rerr := p.Claims.Release(release, claim); rerr != nil {
				err = fmt.Errorf("%w; and the claim %s is kept until it lapses: %w", err, claim, rerr)
			}
		}
		return failed(out, fmt.Errorf("publish %s's pickup of %s: %w", seat.Handle, item.Key, err))
	}
	out.Woken, out.Wake = true, ev.ID
	return out
}

// picked is what [Picker.choose] found: the item and its key, and the ready
// count.
type picked struct {
	item   worklet.ReadyItem
	key    Key
	ready  int
	capped bool
	skip   Skip
}

// candidate is one unworked item [Picker.choose] offers, and whether the
// pass passed over a handed item to reach it.
type candidate struct {
	item   worklet.ReadyItem
	key    Key
	passed bool
}

// choose walks the seat's ready work page by page and asks the ledger key by
// key, offering each item the seat has not had until offer takes one. An item
// offer answers neither taken nor stopped is cooling, and the walk goes on.
func (p *Picker) choose(ctx context.Context, seat Seat, now time.Time,
	offer func(candidate) (bool, Skip, error)) (picked, error) {

	var out picked
	passed, cooling := false, false
	// THE FIRST RECHECK DUE among the items walked, which ends a remembered
	// all-offered set: its key changes then with nothing else moving.
	var until time.Time
	for page := 0; ; page++ {
		// THE NODE'S OWN COPY: the settle gave the last turn's writes a
		// pass to land, and a stale answer costs a pickup the ledger refuses.
		ready, err := p.Ready.ReadyWork(ctx, worklet.ReadyWorkQuery{
			Handle: seat.Handle, Level: statelog.ReadStale, Page: page,
		}, now)
		if err != nil {
			return out, fmt.Errorf("read %s's ready work: %w", seat.Handle, err)
		}
		out.ready, out.capped = ready.Total, ready.Capped
		if ready.Total == 0 {
			p.remember(seat.ID, "", time.Time{})
			out.skip = SkipNothing
			return out, nil
		}
		if p.allOffered(seat.ID, ready.Fingerprint, now) {
			out.skip = SkipOffered
			return out, nil
		}
		for _, item := range ready.Items {
			if next, ok := nextRecheck(item, now); ok && (until.IsZero() || next.Before(until)) {
				until = next
			}
			key := KeyOf(seat.ID, item, now)
			if p.Ledger.Worked(ctx, seat.Handle, []string{key.WorkKey()})[key.WorkKey()] {
				passed = true
				continue
			}
			taken, skip, err := offer(candidate{item: item, key: key, passed: passed})
			switch {
			case skip != "" || err != nil:
				out.skip = skip
				return out, err
			case taken:
				out.item, out.key = item, key
				p.remember(seat.ID, "", time.Time{})
				return out, nil
			}
			cooling = true
		}
		if !ready.More {
			// A COOLING ITEM IS NOT OFFERED YET, so the set is not
			// remembered as all offered.
			if cooling {
				p.remember(seat.ID, "", time.Time{})
				out.skip = SkipCooldown
				return out, nil
			}
			p.remember(seat.ID, ready.Fingerprint, until)
			out.skip = SkipOffered
			return out, nil
		}
	}
}

// hold takes one candidate's claims: the seat's cooldown, once a pass, when
// it passed over a handed item; the item's; then its progress. A progress
// claim held elsewhere answers neither taken nor stopped: the item is cooling.
func (p *Picker) hold(ctx context.Context, seat uuid.UUID, c candidate, now time.Time,
	cooled *bool) (bool, Skip, error) {

	// A DECLINE IS SPACED, WORK IS NOT: the cooldown is taken only when the
	// pickup passes over an item the seat was handed and left unchanged.
	if c.passed && !*cooled {
		if skip, err := p.claim(ctx, CooldownKey(seat), p.Cooldown, now, SkipCooldown); skip != "" {
			return false, skip, err
		}
		*cooled = true
	}
	// HELD FOR THE FLEET'S LONGEST CLAIM, so a copy waiting in the inbox is
	// not published again before its turn records it in the ledger.
	if skip, err := p.claim(ctx, c.key.ClaimKey(), coord.MaxClaimTTL, now, SkipClaimed); skip != "" {
		return false, skip, err
	}
	// THE SEAT'S OWN PROGRESS HANDS A STARTED ITEM BACK once per cooldown,
	// so a turn answering every pickup with a write cannot loop.
	skip, err := p.claim(ctx, c.key.ProgressKey(), p.Cooldown, now, SkipCooldown)
	if skip == "" {
		return true, "", nil
	}
	if rerr := p.Claims.Release(context.WithoutCancel(ctx), c.key.ClaimKey()); rerr != nil {
		err = errors.Join(err, rerr)
	}
	if err != nil {
		return false, skip, err
	}
	return false, "", nil
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
