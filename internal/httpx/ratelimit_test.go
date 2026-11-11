package httpx_test

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/httpx"
)

// A WAIT IS TAKEN ONLY WHEN THE CALL CAN AFFORD IT: within its retries, under
// the cap and before the caller's deadline. Every other wait is handed back
// untaken, with the reason the caller's error carries. Mutation: drop any one
// of the three checks and its row takes a wait it should not.
func TestAWaitIsTakenOnlyWhenTheCallCanAffordIt(t *testing.T) {
	t.Parallel()
	soon, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	for _, tc := range []struct {
		name    string
		ctx     context.Context
		attempt int
		wait    time.Duration
		because string // empty: the wait is taken
	}{
		{"the first wait", context.Background(), 0, 5 * time.Second, ""},
		{"the last wait the retries allow", context.Background(), 2, 30 * time.Second, ""},
		{"one more than the retries", context.Background(), 3, time.Second, "already waited 3 times"},
		{"longer than the cap", context.Background(), 0, 31 * time.Second, "longer than the 30s"},
		{"past the deadline", soon, 0, 25 * time.Second, "after this call's deadline"},
		{"within the deadline", soon, 0, 5 * time.Second, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var waited []time.Duration
			limit := httpx.VendorRateLimit()
			limit.Wait = func(_ context.Context, d time.Duration) error {
				waited = append(waited, d)
				return nil
			}
			err := limit.WaitOut(tc.ctx, tc.attempt, tc.wait)
			if tc.because == "" {
				if err != nil || !slices.Equal(waited, []time.Duration{tc.wait}) {
					t.Errorf("WaitOut = %v after waiting %v, want the %s taken", err, waited, tc.wait)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.because) || len(waited) != 0 {
				t.Errorf("WaitOut = %v after waiting %v, want no wait, because %s",
					err, waited, tc.because)
			}
		})
	}
}

// A WAIT CUT SHORT BY ITS CALLER IS NOT A WAIT THAT ENDED: the call is not
// sent again into a context that is already done.
func TestAWaitCutShortIsHandedBack(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	limit := httpx.RateLimit{Retries: 3, Cap: time.Minute}
	err := limit.WaitOut(ctx, 0, time.Minute)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("WaitOut on a cancelled context = %v, want the cancellation", err)
	}
}

// RETRY-AFTER IS READ AS RFC 9110 WRITES IT: seconds or an HTTP date, and
// nothing else. A date already past is no wait at all; a negative number or
// words are no answer, so the vendor's own header or the default decides.
func TestRetryAfterReadsSecondsOrADate(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		raw  string
		want time.Duration
		ok   bool
	}{
		{"120", 2 * time.Minute, true},
		{" 7 ", 7 * time.Second, true},
		{"0", 0, true},
		{now.Add(45 * time.Second).Format(http.TimeFormat), 45 * time.Second, true},
		{now.Add(-time.Minute).Format(http.TimeFormat), 0, true},
		{"-5", 0, false},
		{"soon", 0, false},
		{"", 0, false},
	} {
		h := http.Header{}
		if tc.raw != "" {
			h.Set("Retry-After", tc.raw)
		}
		got, ok := httpx.RetryAfter(h, now)
		if got != tc.want || ok != tc.ok {
			t.Errorf("RetryAfter(%q) = %s, %t; want %s, %t", tc.raw, got, ok, tc.want, tc.ok)
		}
	}
}

// SLEEP ENDS WITH ITS CONTEXT, so a cancelled caller is not held for the rest
// of a vendor's window, and otherwise lasts exactly what it was asked. Inside a
// synctest bubble the clock moves only when the case is blocked, so each end is
// an exact instant. Mutation: ignore ctx in Sleep and the first wait ends an
// hour late.
func TestSleepEndsWithItsContext(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
		defer cancel()
		if err := httpx.Sleep(ctx, time.Hour); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Sleep past its context = %v, want the deadline", err)
		}
		if deadline := start.Add(10 * time.Millisecond); !time.Now().Equal(deadline) {
			t.Errorf("Sleep held its caller until %s, want its deadline %s", time.Now(), deadline)
		}
		if err := httpx.Sleep(t.Context(), time.Millisecond); err != nil {
			t.Errorf("a short Sleep = %v, want it to end", err)
		}
		if ended := start.Add(11 * time.Millisecond); !time.Now().Equal(ended) {
			t.Errorf("a 1ms Sleep ended at %s, want %s", time.Now(), ended)
		}
	})
}
