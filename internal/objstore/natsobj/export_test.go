package natsobj

import (
	"context"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsprovision"
)

// WalkAhead is [walkAhead], for the case that stages a listing past what one
// pull holds.
const WalkAhead = walkAhead

// ReadAhead, PullHeartbeat, QuietWait and FirstPullRemakes are [readAhead],
// [pullHeartbeat], [quietWait] and [firstPullRemakes], for the cases that hold
// a read's waits to the floor and to the pulls it can lose.
const (
	ReadAhead        = readAhead
	PullHeartbeat    = pullHeartbeat
	QuietWait        = quietWait
	FirstPullRemakes = firstPullRemakes
)

// SwapStream puts what wrap makes of b's stream between b and the broker, so
// a case can stand where the broker's consumers are made.
func SwapStream(b *Backend, wrap func(jetstream.Stream) jetstream.Stream) {
	b.stream = wrap(b.stream)
}

// OpenAt is [Open] at timing, so a case that exhausts a lookup's ceiling on
// purpose spends a fraction of a second rather than the production thirty.
func OpenAt(ctx context.Context, js jetstream.JetStream, cfg Config, timing jsprovision.Timing) (*Backend, error) {
	return open(ctx, js, cfg, timing)
}
