package natsobj

import (
	"context"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsprovision"
)

// WalkAhead is [walkAhead], for the case that stages a listing past what one
// pull holds.
const WalkAhead = walkAhead

// PullExpiry is [pullExpiry], for the case that pins it.
const PullExpiry = pullExpiry

// ShortenPulls sets how long every pull b sends waits, so a case that loses a
// reader's consumer on purpose waits a second for the replacement rather than
// the production expiry.
func ShortenPulls(b *Backend, expiry time.Duration) { b.expiry = expiry }

// OpenAt is [Open] at timing, so a case that exhausts a lookup's ceiling on
// purpose spends a fraction of a second rather than the production thirty.
func OpenAt(ctx context.Context, js jetstream.JetStream, cfg Config, timing jsprovision.Timing) (*Backend, error) {
	return open(ctx, js, cfg, timing)
}

// PutWindow is [putWindow], for the case that holds a put to it.
const PutWindow = putWindow

// SetPace sets what each window of b's puts is given for its
// acknowledgements, so a case about a window that outlasts it waits seconds
// rather than the production pace.
func SetPace(b *Backend, pace time.Duration) { b.pace = pace }

// DelayAcks holds every acknowledgement of b's puts back for delay after the
// broker sends it, as a busy log's slow flushes do, and answers how many of
// a put's pieces were ever unacknowledged at once.
func DelayAcks(b *Backend, delay time.Duration) (peak func() int) {
	d := &delayedAcks{JetStream: b.puts, delay: delay}
	b.puts = d
	return func() int {
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.peak
	}
}

// delayedAcks is a publisher whose asynchronous acknowledgements arrive late.
type delayedAcks struct {
	jetstream.JetStream
	delay time.Duration

	mu         sync.Mutex
	open, peak int
}

func (d *delayedAcks) PublishMsgAsync(m *nats.Msg, opts ...jetstream.PublishOpt) (jetstream.PubAckFuture, error) {
	fut, err := d.JetStream.PublishMsgAsync(m, opts...)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	d.open++
	d.peak = max(d.peak, d.open)
	d.mu.Unlock()
	late := &lateFuture{msg: m, ok: make(chan *jetstream.PubAck, 1), err: make(chan error, 1)}
	go func() {
		select {
		case ack := <-fut.Ok():
			time.Sleep(d.delay)
			d.settled()
			late.ok <- ack
		case err := <-fut.Err():
			d.settled()
			late.err <- err
		}
	}()
	return late, nil
}

func (d *delayedAcks) settled() {
	d.mu.Lock()
	d.open--
	d.mu.Unlock()
}

// lateFuture is one delayed acknowledgement.
type lateFuture struct {
	msg *nats.Msg
	ok  chan *jetstream.PubAck
	err chan error
}

func (f *lateFuture) Ok() <-chan *jetstream.PubAck { return f.ok }
func (f *lateFuture) Err() <-chan error            { return f.err }
func (f *lateFuture) Msg() *nats.Msg               { return f.msg }
