package sandbox

import (
	"syscall"
	"testing"
)

// THE BOX'S SIGNAL NAMES ARE GO'S LINUX ONES, as envd (Go on Linux) writes
// them into an end event's status. boxSignals is a table rather than syscall's
// names because the engine may run on darwin, whose names differ; on Linux the
// two are held equal, so a Go release that renamed one fails here rather than
// leaving a signalled command unread.
func TestBoxSignalsAreGosLinuxDescriptions(t *testing.T) {
	t.Parallel()
	const highest = 31 // SIGSYS; above it Go names a signal by its number
	for n := 1; n <= highest; n++ {
		name := syscall.Signal(n).String()
		if got, _ := boxSignal("signal: " + name); got != n {
			t.Errorf("Go names signal %d %q, which reads as %d", n, name, got)
		}
	}
	if len(boxSignals) != highest {
		t.Errorf("the table holds %d names, want one per signal up to %d", len(boxSignals), highest)
	}
}
