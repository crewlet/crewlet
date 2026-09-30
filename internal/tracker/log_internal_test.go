package tracker

import (
	"log/slog"
	"testing"
)

// AN ABSENT LOGGER IS THE TRACKER'S OWN, AT BOTH PLACES THAT TAKE ONE.
//
// Both read an absent logger as a discarding one and the engine hands neither
// a logger, so the housekeeping duty's `tracker_one_sided_repair_failed` and
// the parser's `tracker_merge_marker_without_target` — the two lines that say
// a repair is NOT happening — went nowhere on every node. The tree-wide guard
// in internal/logging refuses a discarding handler; what it cannot see is a
// default that still writes but loses `component=tracker` (the process-wide
// `slog.Default()`) or drops a level, and that is what this pins.
func TestEveryTrackerLoggerGivenNoneIsThePackagesOwn(t *testing.T) {
	t.Parallel()
	for name, got := range map[string]*slog.Logger{
		"housekeeping duty": newDuty(DutyDeps{}).deps.Logger,
		"inbound parser":    NewParser(ParserOptions{}).logger,
	} {
		if got != log {
			t.Errorf("the %s given no logger holds %p, not the tracker's own "+
				"component logger %p", name, got, log)
		}
	}

	// AND ONE THAT IS HANDED IN IS THE ONE USED.
	mine := slog.New(slog.DiscardHandler)
	if got := newDuty(DutyDeps{Logger: mine}).deps.Logger; got != mine {
		t.Errorf("the duty replaced a logger it was handed with %p", got)
	}
	if got := NewParser(ParserOptions{Logger: mine}).logger; got != mine {
		t.Errorf("the parser replaced a logger it was handed with %p", got)
	}
}
