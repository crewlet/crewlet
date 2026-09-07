package setupapi

import (
	"testing"
	"time"
)

// A service built with no clock reads the wall clock rather than panicking.
func TestAServiceWithNoClockStillReadsTime(t *testing.T) {
	t.Parallel()
	s := &Service{}
	if got := s.now(); got.IsZero() {
		t.Error("a service with no clock read the zero time")
	}
	fixed := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	s.clock = func() time.Time { return fixed }
	if got := s.now(); !got.Equal(fixed) {
		t.Errorf("now() = %s, want the injected clock", got)
	}
}
