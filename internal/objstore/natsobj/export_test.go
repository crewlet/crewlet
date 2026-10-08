package natsobj

import (
	"context"
	"time"

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
