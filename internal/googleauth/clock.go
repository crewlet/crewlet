package googleauth

import (
	"context"
	"time"
)

// Clock is the time a connection reads and waits by: the wall clock in
// production, a test's own so an hour's token or a rate limit's wait passes in
// no time.
type Clock struct {
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
}

// WallClock is the real time, waits ending early with their context.
func WallClock() Clock {
	return Clock{
		Now: func() time.Time { return time.Now().UTC() },
		Sleep: func(ctx context.Context, d time.Duration) error {
			timer := time.NewTimer(d)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			}
		},
	}
}
