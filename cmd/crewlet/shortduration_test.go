package main

import (
	"testing"
	"time"
)

// A DURATION IS SHORTENED BY ITS EMPTY UNITS ONLY, never into its own digits:
// the text these lines print names a wait an operator acts on, and "1" for ten
// seconds or "5" for fifty minutes is a different number.
func TestAShortDurationKeepsEveryDigitItHas(t *testing.T) {
	t.Parallel()
	for d, want := range map[time.Duration]string{
		24 * time.Hour:                  "24h",
		time.Hour:                       "1h",
		90 * time.Minute:                "1h30m",
		50 * time.Minute:                "50m",
		time.Minute:                     "1m",
		10 * time.Second:                "10s",
		30 * time.Second:                "30s",
		time.Hour + 10*time.Second:      "1h0m10s",
		20*time.Minute + 30*time.Second: "20m30s",
		1500 * time.Millisecond:         "1.5s",
	} {
		if got := shortDuration(d); got != want {
			t.Errorf("shortDuration(%v) = %q, want %q", d, got, want)
		}
	}
}
