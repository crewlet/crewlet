package procgrouptest

import "testing"

// A STAND-IN KEEPS THE OPTIONS ITS SUITE RAN WITH and loses only the exit
// sleep. Set rather than appended, the value would quietly drop a
// halt_on_error or a log_path a developer exported to chase a race — in the one
// process the race was likely to be in.
//
// Not parallel: it sets the process environment.
func TestAStandInKeepsTheSuitesRaceOptions(t *testing.T) {
	for _, tc := range []struct {
		name, suite, want string
	}{
		{"the suite set none", "", "atexit_sleep_ms=0"},
		{"the suite's own come first", "halt_on_error=1 log_path=/tmp/race",
			"halt_on_error=1 log_path=/tmp/race atexit_sleep_ms=0"},
		{"a sleep the suite chose is overridden by coming last", "atexit_sleep_ms=500",
			"atexit_sleep_ms=500 atexit_sleep_ms=0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(GORACE, tc.suite)
			if got := StandInRaceOptions(); got != tc.want {
				t.Errorf("StandInRaceOptions() = %q, want %q", got, tc.want)
			}
		})
	}
}
