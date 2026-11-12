package httpx

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// RateLimit is how a vendor client waits out a vendor's 429: the vendor says
// how long, and a wait the call can afford is taken inside the call.
//
// THE VENDOR'S ANSWER IS THE ONLY COUNT THAT IS RIGHT. A vendor's limit is the
// organization's or the token's, spent by every node of the fleet and by every
// other tool the company points at the same account, so a client keeping its
// own tally of what is left would be guessing. The client therefore spends
// nothing up front and waits only when the vendor says so.
//
// A WAIT THE CALL CANNOT AFFORD IS HANDED BACK rather than taken: one longer
// than [RateLimit.Cap], one ending after the caller's deadline, or one more
// after [RateLimit.Retries] waits. The caller's own schedule (a reconcile
// pass's backoff, a person pressing the button again) decides when to ask
// again, and the 429 it is handed says how long the vendor asked for.
type RateLimit struct {
	// Retries is how many waits one call takes before it hands the 429 back.
	Retries int
	// Cap is the longest single wait taken inside a call.
	Cap time.Duration
	// Wait waits d or until ctx is done. Nil sleeps; a test records the wait
	// instead of spending it.
	Wait func(ctx context.Context, d time.Duration) error
}

// VendorRateLimit is the rule every vendor client's calls take.
//
// THREE WAITS OF AT MOST THIRTY SECONDS, the values Cloudflare's and
// DigitalOcean's clients each chose before this rule was shared. Every vendor
// call runs inside a pass, a probe or a person's request with a deadline of
// its own, and [fits] keeps a wait inside it; the cap bounds a caller that set
// none. A longer wait, such as the rest of an hourly window, is the caller's
// to schedule rather than a call's to sit through.
func VendorRateLimit() RateLimit {
	return RateLimit{Retries: 3, Cap: 30 * time.Second}
}

// DefaultRetryAfter is the wait taken on a 429 that names none: long enough
// that the next try is not the same instant, short enough to fit three
// within [VendorRateLimit]'s cap.
const DefaultRetryAfter = 5 * time.Second

// WaitOut decides the 429 a call met on its attempt'th try, counted from
// zero, which asked for wait. It takes the wait and answers nil when the call
// should be sent again; otherwise it answers why the call stops here, as a
// clause the caller's error can carry, and waits for nothing.
func (r RateLimit) WaitOut(ctx context.Context, attempt int, wait time.Duration) error {
	switch {
	case attempt >= r.Retries:
		return fmt.Errorf("it already waited %d times", attempt)
	case wait > r.Cap:
		return fmt.Errorf("the wait is longer than the %s one call waits", r.Cap)
	case !fits(ctx, wait):
		return errors.New("the wait ends after this call's deadline")
	}
	sleep := r.Wait
	if sleep == nil {
		sleep = Sleep
	}
	if err := sleep(ctx, wait); err != nil {
		return fmt.Errorf("it stopped waiting: %w", err)
	}
	return nil
}

// fits reports whether a wait ends before ctx's deadline.
func fits(ctx context.Context, d time.Duration) bool {
	deadline, ok := ctx.Deadline()
	return !ok || time.Until(deadline) > d
}

// Sleep waits for d or until ctx is done.
func Sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// RetryAfter reads a `Retry-After` header (RFC 9110, section 10.2.3): a
// number of seconds, or an HTTP date read against now. A date already past is
// a wait of zero. False for a header that is absent, negative or says neither.
func RetryAfter(h http.Header, now time.Time) (time.Duration, bool) {
	raw := strings.TrimSpace(h.Get("Retry-After"))
	if raw == "" {
		return 0, false
	}
	if seconds, err := strconv.Atoi(raw); err == nil {
		if seconds < 0 {
			return 0, false
		}
		return time.Duration(seconds) * time.Second, true
	}
	if at, err := http.ParseTime(raw); err == nil {
		return max(at.Sub(now), 0), true
	}
	return 0, false
}
