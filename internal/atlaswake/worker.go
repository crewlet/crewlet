package atlaswake

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/atlas"
)

// DutyName is the fleet singleton the duty claims.
const DutyName = "atlas-wake"

// Tick is how often the duty reads the atlas for drift. Five minutes, the
// floor of the atlas's own refresh interval: no integration's read moves a
// fact faster, and a wake that is at most one a day loses nothing to it.
const Tick = 5 * time.Minute

// PassDeadline bounds one pass: one read of the atlas and a few coordination
// writes per drifting entry, seconds on a healthy fleet. A pass past two
// minutes is waiting on a store that does not answer, and is cut off so the
// duty's lease, sized from it, is never outlived.
const PassDeadline = 2 * time.Minute

// writeAttempts is how many times one record is read and written again
// after another node moved it, before the pass leaves it to the next.
const writeAttempts = 3

// Owing is one entry an agent seat owns, as that seat reads it: its kind and
// what is open on it.
type Owing struct {
	Kind  atlas.Kind
	Items []Item
}

// Reading is one read of the atlas for drift: per owning seat's handle, each
// entry it owns. Whole is false where a source was not read in full, and a
// pass over such a read forgets nothing.
type Reading struct {
	Owing map[string]map[uuid.UUID]Owing
	Whole bool
}

// ReadingOf is the drift reading of v: who owns what ([Owned]) and what is
// open on each entry in the view seen answers for its owner. An entry the
// owner's own view leaves out has nothing open.
func ReadingOf(v atlas.View, whole bool, agent func(handle string) bool,
	seen func(handle string) (atlas.View, bool)) Reading {

	out := Reading{Owing: map[string]map[uuid.UUID]Owing{}, Whole: whole}
	for handle, entries := range Owned(v, agent) {
		view, ok := seen(handle)
		if !ok {
			continue
		}
		owing := make(map[uuid.UUID]Owing, len(entries))
		for _, id := range entries {
			if e, shown := view.Find(id); shown {
				owing[id] = Owing{Kind: e.Ref.Kind, Items: Drift(e)}
			}
		}
		out.Owing[handle] = owing
	}
	return out
}

// Kept is one record as the store answers it, with the revision a write of
// it is conditional on.
type Kept struct {
	Handle   string
	Entry    uuid.UUID
	Record   Record
	Revision uint64
}

// Store is where the records are kept (the coordination store). Every write
// answers false where another node moved the record first.
type Store interface {
	Load(ctx context.Context) ([]Kept, error)
	Get(ctx context.Context, handle string, entry uuid.UUID) (Kept, bool, error)
	Create(ctx context.Context, handle string, entry uuid.UUID, rec Record) (bool, error)
	Update(ctx context.Context, k Kept, rec Record) (bool, error)
	Delete(ctx context.Context, k Kept) (bool, error)
}

// Options is what a worker is built from.
type Options struct {
	Store Store

	// Read is the atlas read for drift; false where this node runs no
	// company yet, which wakes nobody.
	Read func(ctx context.Context) (Reading, bool, error)

	// Enabled is the company's switch (`atlas.owner_wakes`), read per pass.
	Enabled func() bool

	// TakesWork reports whether seat handle would start a turn now: not
	// paused, not stopped, not over its budget. Three-valued: an error is
	// unknown, and an unknown seat is not woken this pass.
	TakesWork func(ctx context.Context, handle string) (bool, error)

	// Publish puts one wake on the inbound topic.
	Publish func(ctx context.Context, r Request) error

	// ClaimDuty gates each pass on holding the fleet's duty; nil is the
	// single-node case. Admits gates it on the node's posture; nil admits.
	ClaimDuty func(ctx context.Context) (bool, error)
	Admits    func() bool

	// Tick overrides [Tick]; zero takes it. Now is the clock; nil is the
	// wall clock in UTC.
	Tick time.Duration
	Now  func() time.Time
}

// Worker is the loop.
type Worker struct {
	opts Options

	mu      sync.Mutex
	stop    context.CancelFunc
	stopped chan struct{}
}

// New builds the worker.
func New(opts Options) (*Worker, error) {
	switch {
	case opts.Store == nil:
		return nil, errors.New("atlaswake: a store is required: the once-a-day bound is kept there")
	case opts.Read == nil || opts.Enabled == nil:
		return nil, errors.New("atlaswake: the atlas read and the company's switch are required")
	case opts.TakesWork == nil:
		return nil, errors.New("atlaswake: the seat check is required: a seat that takes no work is not woken")
	case opts.Publish == nil:
		return nil, errors.New("atlaswake: a publisher is required")
	}
	if opts.Tick <= 0 {
		opts.Tick = Tick
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}
	return &Worker{opts: opts}, nil
}

// Start runs the loop until Stop; a second Start is a no-op.
func (w *Worker) Start(ctx context.Context) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stop != nil {
		return
	}
	// WithoutCancel: the loop's lifetime is Stop's, not the starting call's.
	loop, cancel := context.WithCancel(context.WithoutCancel(ctx))
	w.stop, w.stopped = cancel, make(chan struct{})
	go func(done chan struct{}) {
		defer close(done)
		ticker := time.NewTicker(w.opts.Tick)
		defer ticker.Stop()
		for {
			w.Pass(loop)
			select {
			case <-loop.Done():
				return
			case <-ticker.C:
			}
		}
	}(w.stopped)
}

// Stop ends the loop and waits for the pass in flight.
func (w *Worker) Stop() {
	w.mu.Lock()
	stop, stopped := w.stop, w.stopped
	w.stop, w.stopped = nil, nil
	w.mu.Unlock()
	if stop != nil {
		stop()
		<-stopped
	}
}

// pair names one record: a seat and an entry.
type pair struct {
	handle string
	entry  uuid.UUID
}

// Pass is one pass: read the atlas for drift, and move each seat's record on
// each entry one step ([Decide]). It answers how many wakes it published.
func (w *Worker) Pass(ctx context.Context) int {
	if w.opts.Admits != nil && !w.opts.Admits() {
		return 0
	}
	if claim := w.opts.ClaimDuty; claim != nil {
		held, err := claim(ctx)
		if err != nil {
			log.DebugContext(ctx, "atlas_wake_duty_unknown", "error", err.Error())
		}
		if err != nil || !held {
			return 0
		}
	}
	ctx, cancel := context.WithTimeout(ctx, PassDeadline)
	defer cancel()

	// SWITCHED OFF IS A WHOLE READ OF NOTHING: no wake, and each record is
	// kept through its day and dropped after it, so a switch turned off and
	// on again cannot wake a seat twice in one day.
	enabled := w.opts.Enabled()
	reading := Reading{Whole: true}
	if enabled {
		read, running, err := w.opts.Read(ctx)
		if err != nil {
			log.WarnContext(ctx, "atlas_wake_unread", "error", err.Error(),
				"detail", "the atlas was not read for drift; the next pass reads it again")
			return 0
		}
		if !running {
			return 0
		}
		reading = read
	}
	kept, err := w.opts.Store.Load(ctx)
	if err != nil {
		log.WarnContext(ctx, "atlas_wake_records_unread", "error", err.Error(),
			"detail", "without the records nothing says who was woken today, so nobody is woken")
		return 0
	}
	records := make(map[pair]Kept, len(kept))
	pairs := make([]pair, 0, len(kept))
	for _, k := range kept {
		p := pair{k.Handle, k.Entry}
		records[p] = k
		pairs = append(pairs, p)
	}
	for handle, owing := range reading.Owing {
		for entry := range owing {
			if p := (pair{handle, entry}); !slices.Contains(pairs, p) {
				pairs = append(pairs, p)
			}
		}
	}
	slices.SortFunc(pairs, func(a, b pair) int {
		return cmp.Or(cmp.Compare(a.handle, b.handle), cmp.Compare(a.entry.String(), b.entry.String()))
	})
	woken := 0
	for _, p := range pairs {
		if ctx.Err() != nil {
			break
		}
		k, found := records[p]
		if w.step(ctx, p, k, found, reading.Owing[p.handle][p.entry], reading.Whole, enabled) {
			woken++
		}
	}
	return woken
}

// step moves one record one step, reading it again where another node moved
// it; it reports whether a wake was published.
func (w *Worker) step(ctx context.Context, p pair, k Kept, found bool, owing Owing, whole, enabled bool) bool {
	for attempt := range writeAttempts {
		if attempt > 0 {
			var err error
			if k, found, err = w.opts.Store.Get(ctx, p.handle, p.entry); err != nil {
				w.unkept(ctx, p, err)
				return false
			}
		}
		kind := owing.Kind
		if kind == "" {
			kind = k.Record.Kind
		}
		d := Decide(k.Record, found, kind, owing.Items, whole, w.opts.Now())
		var landed bool
		var err error
		switch d.Step {
		case StepNone:
			return false
		case StepKeep:
			landed, err = w.opts.Store.Update(ctx, k, d.Next)
		case StepDrop:
			landed, err = w.opts.Store.Delete(ctx, k)
		case StepWake, StepPublish:
			if !enabled {
				// A CLAIM LEFT UNPUBLISHED WHEN THE SWITCH WENT OFF stays so.
				return false
			}
			var published bool
			published, landed, err = w.wake(ctx, p, k, found, d)
			if published {
				return true
			}
		}
		if err != nil {
			w.unkept(ctx, p, err)
			return false
		}
		if landed {
			return false
		}
	}
	log.DebugContext(ctx, "atlas_wake_record_moved", "agent", p.handle, "entry", p.entry.String(),
		"detail", "another node kept moving the record; the next pass decides it again")
	return false
}

// wake claims (where d is a new wake), publishes and confirms. landed is
// false only where the claim lost to another node, which decides again.
func (w *Worker) wake(ctx context.Context, p pair, k Kept, found bool, d Decision) (published, landed bool, err error) {
	// THE SEAT FIRST, AND NOTHING WRITTEN WHERE IT TAKES NO WORK: a paused,
	// stopped or spent seat is not woken and keeps its day, so the pass
	// after it resumes tells it what is still open.
	takes, err := w.opts.TakesWork(ctx, p.handle)
	if err != nil {
		log.DebugContext(ctx, "atlas_wake_seat_unknown", "agent", p.handle, "error", err.Error())
		return false, true, nil
	}
	if !takes {
		log.DebugContext(ctx, "atlas_wake_seat_takes_no_work", "agent", p.handle, "entry", p.entry.String())
		return false, true, nil
	}
	if d.Step == StepWake {
		// THE CLAIM BEFORE THE PUBLISH: whichever node's write lands owns
		// today's wake, and the loser publishes nothing.
		if found {
			landed, err = w.opts.Store.Update(ctx, k, d.Next)
		} else {
			landed, err = w.opts.Store.Create(ctx, p.handle, p.entry, d.Next)
		}
		if err != nil || !landed {
			return false, landed, err
		}
	}
	req := RequestFor(p.handle, p.entry, d.Next)
	if pubErr := w.opts.Publish(ctx, req); pubErr != nil {
		log.WarnContext(ctx, "atlas_wake_unpublished", "agent", p.handle, "entry", p.entry.String(),
			"error", pubErr.Error(), "detail", "the wake is claimed and is published again by the next pass")
		return false, true, nil
	}
	log.InfoContext(ctx, "atlas_wake_asked", "agent", p.handle, "entry", p.entry.String(),
		"kind", string(req.Kind), "changes", len(req.Changes), "day", req.Day)
	// CONFIRMED AT THE REVISION THE CLAIM NOW HAS. A confirm that fails
	// leaves the claim unpublished, and the next pass publishes the same
	// wake id again, which the inbox collapses.
	now, ok, err := w.opts.Store.Get(ctx, p.handle, p.entry)
	if err == nil && ok {
		_, err = w.opts.Store.Update(ctx, now, Confirmed(now.Record))
	}
	if err != nil {
		w.unkept(ctx, p, err)
	}
	return true, true, nil
}

func (w *Worker) unkept(ctx context.Context, p pair, err error) {
	log.WarnContext(ctx, "atlas_wake_record_unkept", "agent", p.handle, "entry", p.entry.String(),
		"error", err.Error(), "detail", "the record was not moved; the next pass decides it again")
}
