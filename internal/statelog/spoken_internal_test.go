package statelog

import (
	"testing"
	"time"
)

// AN ALARM'S DURATION IS SPELLED AS THE DASHBOARD SPELLS ONE ("85h 58m", not
// `time.Duration.String`'s "85h58m0s"). The cases are the dashboard's
// (lib/format.ts fmtDuration), plus the carries a grain boundary must hand up
// rather than print as "1000ms" or "60m".
func TestAnAlarmsDurationIsSpokenLikeTheDashboards(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{0, "0ms"},
		{340 * time.Millisecond, "340ms"},
		{999600 * time.Microsecond, "1s"},
		{1200 * time.Millisecond, "1.2s"},
		{11390 * time.Millisecond, "11s"},
		{9960 * time.Millisecond, "10s"},
		{45 * time.Second, "45s"},
		{59600 * time.Millisecond, "1m"},
		{2 * time.Minute, "2m"},
		{62 * time.Second, "1m 2s"},
		{30 * time.Minute, "30m"},
		{3599600 * time.Millisecond, "1h"},
		{24 * time.Hour, "24h"},
		{85*time.Hour + 58*time.Minute, "85h 58m"},
		{2*time.Hour + 3*time.Minute + 29*time.Second, "2h 3m"},
		{-45 * time.Second, "45s"},
	} {
		if got := spoken(tc.d); got != tc.want {
			t.Errorf("spoken(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}
