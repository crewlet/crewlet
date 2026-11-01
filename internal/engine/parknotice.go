package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/backoff"
	"github.com/crewlet/crewlet/internal/events"
)

// A PROVIDER PARK'S NOTICES: ONE PER CHANGE, AND HELD WHILE THE BROKER IS DOWN.
//
// A seat's park, each renewal of it and its release are announced as events
// (`seat_provider_parked`, `seat_provider_released`), which every screen draws
// the seat's condition from (internal/api/livestate). They were published on
// the path that decided them, once, best effort, so a broker that refused cost
// a WARN per seat per decision and the notice itself: a screen went on drawing
// a seat parked after its release for as long as nothing else moved it. And a
// decision path that ran hot published on every pass: a probe re-armed for an
// instant already past renewed every seat's park as fast as its call
// returned, and one node logged 36 000 `seat_provider_park_not_published`
// lines in a three-minute broker outage.
//
// So a notice is handed to [parkNotices], which keeps the newest one per seat:
//
//   - ONCE PER CHANGE: a notice equal to the last one published for its seat
//     (or on its way) is dropped, so a renewal that moved nothing is no news.
//   - THE NEWEST WINS: a notice that arrives while an older one for the same
//     seat waits replaces it, so a seat parked and released during an outage
//     is published released, once, and never parked after it.
//   - PUBLISHED BY ITS CALLER while the broker answers, as before, so what the
//     caller decided is published before it returns; one caller at a time
//     drains, and a seat's notices go out in the order they were decided.
//   - HELD AND RETRIED ON A BACKOFF once the broker refuses: no caller
//     publishes while they are held, and each attempt publishes every waiting
//     notice in turn and stops at the first refusal, since a broker that
//     refused one refuses the rest. An outage costs one attempt per step of
//     the backoff, however many decisions it spans.
//   - SAID PER OUTAGE, NOT PER NOTICE: one WARN when publishing first fails,
//     and one INFO when the held notices land, with how many and for how long.
//
// WHO HAS TO AGREE ON IT: nobody. The park is this node's and holds whether or
// not a screen hears of it; what this keeps is that the screen hears its latest
// state once the broker answers. A node that stops drops what it held: its
// seats are given up with it, and the node that takes one announces its own.

// The schedule held notices are retried on.
const (
	// parkNoticeBase is the first wait: one second, the interval the broker
	// connection's own reconnect attempts are spaced by, so the first retry
	// follows the client's first attempt at its broker.
	parkNoticeBase = time.Second

	// parkNoticeCeiling is the longest: thirty seconds, so a screen hears a
	// seat's latest state within half a minute of the broker answering
	// however long it was gone, at two attempts a minute while it stays
	// down.
	parkNoticeCeiling = 30 * time.Second

	// parkNoticeJitter spreads each wait by a fifth either way, the spread
	// the engine's other retry schedules use.
	parkNoticeJitter = 0.2
)

// parkPublisher is how a notice reaches the broker.
type parkPublisher func(ctx context.Context, ev *events.Event) error

// parkNotice is one seat's notice and the bytes it is compared by.
type parkNotice struct {
	ev  *events.Event
	key []byte
}

// parkNotices is every seat's provider park notice this node has decided and
// not yet published (see the file's doc). The zero value is ready.
type parkNotices struct {
	// base is the retry schedule's first wait, zero for [parkNoticeBase]; it
	// scales the whole schedule. Only a case sets it, so a schedule plays out
	// in milliseconds.
	base time.Duration

	// logger is where an outage is said; nil is the package's own logger.
	logger *slog.Logger

	mu sync.Mutex

	// waiting is the newest notice of each seat not yet handed to the
	// broker, flying the one being handed over now, and sent the last one
	// the broker took, each by the seat's handle.
	waiting map[string]parkNotice
	flying  map[string]parkNotice
	sent    map[string][]byte

	// draining is whether a caller or the retry is publishing, so one
	// drains at a time and a seat's notices go out in order.
	draining bool

	// held is whether publishing has failed and the retry owns every
	// waiting notice: since when, after how many attempts, and the timer of
	// the next. retryCtx is what the retry publishes under, ended by stop.
	held      bool
	heldSince time.Time
	attempts  int
	timer     *time.Timer
	retryCtx  context.Context
	cancel    context.CancelFunc

	// retrying counts the retries publishing, which stop waits for.
	retrying sync.WaitGroup

	stopped bool
}

// post hands one seat's notice over, and publishes it before returning unless
// another caller is publishing (which then publishes it too) or notices are
// held.
func (n *parkNotices) post(ctx context.Context, publish parkPublisher, handle string, ev *events.Event) {
	key, err := json.Marshal(struct {
		Type string         `json:"type"`
		Data events.Payload `json:"data"`
	}{ev.Type, ev.Data})
	if err != nil {
		n.log().WarnContext(ctx, "seat_provider_park_notice_unencodable", "seat", handle,
			"type", ev.Type, "error", err.Error())
		return
	}
	n.mu.Lock()
	if n.stopped {
		n.mu.Unlock()
		return
	}
	if n.waiting == nil {
		n.waiting, n.flying, n.sent = map[string]parkNotice{}, map[string]parkNotice{}, map[string][]byte{}
	}
	// AGAINST WHAT THE BROKER WILL HAVE once what is on its way lands: a
	// notice equal to it is no change, and one waiting behind it is stale.
	ahead := n.sent[handle]
	if f, ok := n.flying[handle]; ok {
		ahead = f.key
	}
	switch {
	case bytes.Equal(ahead, key):
		delete(n.waiting, handle)
		n.mu.Unlock()
		return
	case bytes.Equal(n.waiting[handle].key, key):
		n.mu.Unlock()
		return
	}
	n.waiting[handle] = parkNotice{ev: ev, key: key}
	if n.draining || n.held {
		n.mu.Unlock()
		return
	}
	n.draining = true
	n.mu.Unlock()
	n.drain(ctx, publish, false)
}

// drain publishes every waiting notice, seat by seat in handle order, until
// none waits or the broker refuses one; it is called with draining set, and
// clears it. retry is whether the held retry is draining rather than a caller.
func (n *parkNotices) drain(ctx context.Context, publish parkPublisher, retry bool) {
	published := 0
	for {
		n.mu.Lock()
		if n.stopped || len(n.waiting) == 0 {
			n.drained(ctx, retry, published)
			n.mu.Unlock()
			return
		}
		handle := slices.Min(slices.Collect(maps.Keys(n.waiting)))
		notice := n.waiting[handle]
		delete(n.waiting, handle)
		n.flying[handle] = notice
		n.mu.Unlock()

		err := publish(ctx, notice.ev)

		n.mu.Lock()
		delete(n.flying, handle)
		if err == nil {
			n.sent[handle] = notice.key
			published++
			n.mu.Unlock()
			continue
		}
		// BACK IN LINE unless a newer notice for the seat arrived while
		// this one was out: the newer is what the seat's state now is.
		if _, newer := n.waiting[handle]; !newer {
			n.waiting[handle] = notice
		}
		n.refused(ctx, publish, err)
		n.mu.Unlock()
		return
	}
}

// drained ends a drain that found nothing left, saying so where it ends an
// outage. The caller holds the lock.
func (n *parkNotices) drained(ctx context.Context, retry bool, published int) {
	n.draining = false
	if !retry || !n.held || n.stopped {
		return
	}
	n.log().InfoContext(ctx, "seat_provider_park_notices_published", "published", published,
		"held_for", time.Since(n.heldSince).Round(time.Millisecond).String(),
		"attempts", n.attempts,
		"detail", "the broker took every seat's latest park notice it had held")
	n.held, n.attempts, n.timer = false, 0, nil
	n.cancel()
	n.retryCtx, n.cancel = nil, nil
}

// refused holds every waiting notice after a refusal and arms the next
// attempt, saying so on the first refusal of an outage. The caller holds the
// lock.
func (n *parkNotices) refused(ctx context.Context, publish parkPublisher, err error) {
	n.draining = false
	if n.stopped {
		return
	}
	n.attempts++
	wait := backoff.Jitter(backoff.Doubling(n.attempts, n.first(), n.first()*parkNoticeRatio),
		parkNoticeJitter)
	if !n.held {
		n.held, n.heldSince = true, time.Now()
		// THE FAILED CALL'S VALUES AND NONE OF ITS CANCELLATION: a caller
		// that gave up while its notice was out holds the notice like a
		// broker refusal does, and the retry that publishes it later must
		// not be born ended. Only stop ends it.
		n.retryCtx, n.cancel = context.WithCancel(context.WithoutCancel(ctx))
		n.log().WarnContext(ctx, "seat_provider_park_notices_held",
			"seats", strings.Join(slices.Sorted(maps.Keys(n.waiting)), ","),
			"error", err.Error(), "retry_in", wait.String(),
			"detail", "the parks hold whether or not a screen hears of them; each seat's "+
				"latest notice is published once the broker answers, and that is said "+
				"once, when they land")
	} else {
		n.log().DebugContext(ctx, "seat_provider_park_notices_retry_failed",
			"attempt", n.attempts, "seats", len(n.waiting), "error", err.Error(),
			"retry_in", wait.String())
	}
	retryCtx := n.retryCtx
	n.timer = time.AfterFunc(wait, func() { n.retry(retryCtx, publish) })
}

// retry is the held notices' next attempt.
func (n *parkNotices) retry(ctx context.Context, publish parkPublisher) {
	n.mu.Lock()
	if n.stopped || !n.held || n.draining {
		n.mu.Unlock()
		return
	}
	n.draining = true
	n.retrying.Add(1)
	n.mu.Unlock()
	defer n.retrying.Done()
	n.drain(ctx, publish, true)
}

// stop drops every held notice and ends a retry in flight, for a node
// shutting down, and returns once it has.
func (n *parkNotices) stop() {
	n.mu.Lock()
	n.stopped = true
	if n.timer != nil {
		n.timer.Stop()
	}
	if n.cancel != nil {
		n.cancel()
	}
	n.mu.Unlock()
	n.retrying.Wait()
}

// parkNoticeRatio is the ceiling's multiple of the first wait.
const parkNoticeRatio = parkNoticeCeiling / parkNoticeBase

// first is the schedule's first wait.
func (n *parkNotices) first() time.Duration {
	if n.base > 0 {
		return n.base
	}
	return parkNoticeBase
}

func (n *parkNotices) log() *slog.Logger {
	if n.logger != nil {
		return n.logger
	}
	return log
}
