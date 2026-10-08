package natsobj

import "time"

// WalkAhead is [walkAhead], for the case that stages a listing past what one
// pull holds.
const WalkAhead = walkAhead

// PullExpiry is [pullExpiry], for the case that pins it.
const PullExpiry = pullExpiry

// ShortenPulls sets how long every pull b sends waits, so a case that loses a
// reader's consumer on purpose waits a second for the replacement rather than
// the production expiry.
func ShortenPulls(b *Backend, expiry time.Duration) { b.expiry = expiry }
