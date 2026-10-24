package sandbox

import (
	"syscall"
	"testing"
)

// THE BOX'S SIGNAL NAMES ARE GO'S LINUX ONES, which is what envd — a Go
// program on Linux — writes into an end event's status. The table is the
// box's rather than read from syscall because the engine may run on darwin,
// whose descriptions differ; here, on Linux, the two are held equal, so a Go
// release that renamed one fails this rather than leaving a signalled command
// unread.
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
