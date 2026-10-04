package engine

import (
	"context"
	"testing"
	"time"
)

// A LOOP WAITS EXACTLY WHAT ITS RUN ANSWERED, every time — the link between a
// duty's pace and when it next runs.
func TestALoopWaitsWhatItsRunAnswered(t *testing.T) {
	t.Parallel()
	answers := []time.Duration{time.Second, 15 * time.Second, 3 * time.Second, time.Second}
	next := 0 // touched only by the loop's goroutine
	waits := make(chan time.Duration)
	l := startLoop(t.Context(), func(ctx context.Context, d time.Duration) {
		select {
		case waits <- d:
		case <-ctx.Done():
		}
	}, func(context.Context) time.Duration {
		d := answers[next%len(answers)]
		next++
		return d
	})
	for i, want := range answers {
		if got := <-waits; got != want {
			t.Fatalf("wait %d = %v, want %v", i, got, want)
		}
	}
	l.stop()
}
