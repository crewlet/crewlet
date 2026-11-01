// Package clocktest forges the instants a host's clock reads when its two
// readings have come apart, for tests of code that measures time across them.
//
// # Why an instant has two readings, and when they part
//
// An instant read from time.Now carries a wall-clock reading and a monotonic
// one, and Go compares or subtracts two instants that both carry a monotonic
// reading on that reading alone. The two part in two ways a real host meets:
// the monotonic clock stops while the host sleeps (mach_absolute_time on
// macOS, CLOCK_MONOTONIC on Linux) while the wall clock runs on, and the wall
// clock is stepped (by NTP, or by hand) while the monotonic clock is not. Code
// that holds an instant against something that counts on the other clock (a
// vendor's reset, a lease another process expires) answers wrongly after
// either, and only an instant with parted readings can show it.
//
// # Why it edits the instant in place
//
// No exported call moves one reading without the other: Add moves both, and
// Round(0) drops the monotonic reading altogether. So [Parted] writes the
// wall seconds of the instant directly, and then checks that it moved exactly
// what it meant to, failing with the assumption it rests on named if a Go
// release lays time.Time out differently. A test that cannot forge its clock
// fails rather than passes, because a pass would prove nothing.
package clocktest

import (
	"strings"
	"testing"
	"time"
	"unsafe"
)

// AfterSleep is at as its host's clock reads it after the host slept for d:
// the wall reading d later, the monotonic reading where it was.
func AfterSleep(t testing.TB, at time.Time, d time.Duration) time.Time {
	t.Helper()
	return Parted(t, at, 0, d)
}

// Parted is at with its monotonic reading moved by monotonic and its wall
// reading moved by wall: a host that slept moves only the wall reading, and a
// wall clock stepped back moves the wall reading back while the monotonic one
// runs on. at must be read from time.Now, and the two moves must differ by a
// whole number of seconds.
func Parted(t testing.TB, at time.Time, monotonic, wall time.Duration) time.Time {
	t.Helper()
	// timeLayout is time.Time's own: a first word holding a has-monotonic
	// flag in its top bit, the wall clock's seconds since 1885 above bit 30
	// and its nanoseconds below; a second word holding the monotonic
	// reading; and the location.
	type timeLayout struct {
		wall uint64
		ext  int64
		loc  *time.Location
	}
	const wallSecondsShift = 30
	apart := wall - monotonic
	switch {
	case unsafe.Sizeof(timeLayout{}) != unsafe.Sizeof(time.Time{}):
		t.Fatal("clocktest: time.Time is not the three words this forgery writes")
	case !strings.Contains(at.String(), " m="):
		t.Fatalf("clocktest: %s carries no monotonic reading; read it from time.Now", at)
	case apart%time.Second != 0:
		t.Fatalf("clocktest: the readings can only part by whole seconds, not %s", apart)
	}
	forged := at.Add(monotonic)
	layout := (*timeLayout)(unsafe.Pointer(&forged))
	if seconds := int64(apart / time.Second); seconds >= 0 {
		layout.wall += uint64(seconds) << wallSecondsShift
	} else {
		layout.wall -= uint64(-seconds) << wallSecondsShift
	}
	if forged.Sub(at) != monotonic || forged.Round(0).Sub(at.Round(0)) != wall {
		t.Fatalf("clocktest: the forged instant moved %s on the monotonic clock and %s on "+
			"the wall, want %s and %s: time.Time is no longer laid out as this forgery "+
			"assumes", forged.Sub(at), forged.Round(0).Sub(at.Round(0)), monotonic, wall)
	}
	return forged
}
