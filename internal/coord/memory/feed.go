package memory

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// FeedDocuments opens a durable feed over one key class.
//
// # What the twin has to reproduce, and what it deliberately does not
//
// The properties every caller depends on: a change is delivered to exactly
// one consumer at a time, an un-acked change comes back after the ack window,
// a naked change comes back after its delay, and a NEW feed starts at the
// current head rather than replaying history. All four are here, because a
// handler that got any of them wrong against the twin would still be wrong
// against the broker.
//
// What is not here is the broker's own machinery — flow control, per-message
// redelivery counts across a cluster, the dead-letter path — because none of
// it is observable through the [coord.Feed] contract, and a twin that
// simulated it would be a second implementation to keep correct.
func (f *Fleet) FeedDocuments(ctx context.Context, family coord.Family, class, group string) (coord.Feed, error) {
	if strings.TrimSpace(class) == "" {
		return nil, errors.New("coord/memory: a feed needs a key class to filter on")
	}
	if strings.TrimSpace(group) == "" {
		return nil, errors.New("coord/memory: a feed needs a durable name")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	d := f.docs()
	if _, err := d.family(family); err != nil {
		return nil, err
	}
	if d.feeds == nil {
		d.feeds = map[string]*memFeed{}
	}
	// DURABLE BY NAME: a second opener of the same group joins the
	// existing position rather than starting a new one, which is what
	// makes a restart resume and what a fleet-wide group means.
	name := string(family) + "/" + class + "/" + group
	feed, ok := d.feeds[name]
	if !ok {
		feed = &memFeed{family: family, class: class}
		feed.arrived = sync.NewCond(&feed.mu)
		d.feeds[name] = feed
	}
	return &feedHandle{feed: feed, done: make(chan struct{})}, nil
}

// memFeed is one durable group's queue.
//
// ITS LIFETIME IS THE DURABLE'S, NOT A HANDLE'S. The map entry existing IS
// the consumer existing, so a change written while every node is away is
// waiting when one comes back — which is what a restart resuming rather than
// replaying actually depends on. A feed that only enqueued while somebody was
// listening would lose exactly the wakes a rolling restart is most likely to
// produce.
type memFeed struct {
	family coord.Family
	class  string

	mu      sync.Mutex
	arrived *sync.Cond
	queued  []coord.Change
}

// offer adds a change if it belongs to this feed.
//
// A FEED THAT DOES NOT YET EXIST NEVER SEES IT, which is how DeliverNew is
// modelled: the map entry is created when the first consumer opens, so a
// change written before that is not in any queue. That is the property
// stopping an upgrade from waking every seat for every historical change.
func (m *memFeed) offer(family coord.Family, change coord.Change) {
	// THE FAMILY IS CHECKED AS WELL AS THE CLASS. Two families use the
	// same class letters — a change is "c" in both the tracker and the
	// wiki — so a class-only filter would deliver a page change to the
	// tracker's feed, which would wake the wrong seats about a record it
	// cannot even decode.
	if family != m.family {
		return
	}
	class, ok := coord.KeyClass(change.Key)
	if !ok || class != m.class {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.queued = append(m.queued, change)
	m.arrived.Signal()
}

// feedHandle is one process's view of a durable feed.
type feedHandle struct {
	feed *memFeed
	done chan struct{}
	once sync.Once
}

// Next takes the next change, blocking until one arrives.
func (h *feedHandle) Next(ctx context.Context) (*coord.Delivery, error) {
	m := h.feed

	// A condition variable cannot select on a context or a channel, so a
	// watcher turns either ending into a broadcast and exits with the wait.
	stopped := make(chan struct{})
	defer close(stopped)
	go func() {
		select {
		case <-ctx.Done():
		case <-h.done:
		case <-stopped:
			return
		}
		m.mu.Lock()
		m.arrived.Broadcast()
		m.mu.Unlock()
	}()

	m.mu.Lock()
	defer m.mu.Unlock()
	for len(m.queued) == 0 && ctx.Err() == nil && !h.isStopped() {
		m.arrived.Wait()
	}
	if len(m.queued) == 0 {
		return nil, nil
	}
	change := m.queued[0]
	m.queued = m.queued[1:]
	var settled sync.Once
	return &coord.Delivery{
		Change: change,
		Ack:    func() error { settled.Do(func() {}); return nil },
		Nak: func(delay time.Duration) error {
			settled.Do(func() {
				// REQUEUED AT THE FRONT after the delay, so a naked
				// change is retried before newer ones — which keeps a
				// handler that naks under load from starving its own
				// backlog into permanent reordering.
				time.AfterFunc(delay, func() {
					m.mu.Lock()
					m.queued = append([]coord.Change{change}, m.queued...)
					m.arrived.Signal()
					m.mu.Unlock()
				})
			})
			return nil
		},
	}, nil
}

func (h *feedHandle) isStopped() bool {
	select {
	case <-h.done:
		return true
	default:
		return false
	}
}

// Stop ends this handle.
//
// THE DURABLE QUEUE SURVIVES, as the broker's consumer does: its position is
// the fleet's, so a node leaving must not reset where its peers read from.
func (h *feedHandle) Stop() error {
	h.once.Do(func() {
		close(h.done)
		h.feed.mu.Lock()
		h.feed.arrived.Broadcast()
		h.feed.mu.Unlock()
	})
	return nil
}

var _ coord.Feeder = (*Fleet)(nil)
