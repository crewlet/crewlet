package usage

import (
	"errors"
	"fmt"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
)

// THE FLUSH LOOP'S WARNING IS FOR WHAT THE NEXT TICK RETRIES, and nothing else.
//
// A refusal no retry changes was logged by the publish that met it, and the
// loop's own line says the day is re-derived and republished next tick — which
// is untrue of exactly those. So the loop is handed what is left once they are
// taken out of the tick's joined failures, however deep the join nests them.
func TestTheFlushLoopWarnsOnlyOfWhatTheNextTickRetries(t *testing.T) {
	t.Parallel()
	full := fmt.Errorf("usage: publish seat.x: %w", &statelog.Unavailable{Reason: statelog.ReasonLogFull})
	large := fmt.Errorf("usage: publish seat.y: %w", &statelog.Unavailable{Reason: statelog.ReasonRecordTooLarge})
	behind := fmt.Errorf("usage: publish seat.z: %w", &statelog.Unavailable{Reason: statelog.ReasonBehind})
	broken := errors.New("usage: fingerprint 2026-09-23: the store is closed")

	for _, tc := range []struct {
		name string
		in   error
		want []error
	}{
		{"nothing failed", nil, nil},
		{"only refusals no retry changes", errors.Join(full, errors.Join(large, nil)), nil},
		{"a refusal that clears on its own", errors.Join(full, behind), []error{behind}},
		{"a fault beside the refusals", errors.Join(errors.Join(large), broken, full), []error{broken}},
		{"one bare fault", broken, []error{broken}},
		{"one bare refusal", large, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := transient(tc.in)
			if (got == nil) != (len(tc.want) == 0) {
				t.Fatalf("transient(%v) = %v, want %v", tc.in, got, tc.want)
			}
			for _, want := range tc.want {
				if !errors.Is(got, want) {
					t.Errorf("transient(%v) = %v, which lost %v", tc.in, got, want)
				}
			}
			for _, permanent := range []error{full, large} {
				if got != nil && errors.Is(got, permanent) {
					t.Errorf("transient(%v) = %v, which kept the refusal %v that the "+
						"publish already logged", tc.in, got, permanent)
				}
			}
		})
	}
}
