package api

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// stepClock is a pace clock that moves only when a case says time passed —
// a client's wait inside a read, or the server's own work between two.
type stepClock struct{ at time.Time }

func (c *stepClock) now() time.Time { return c.at }

func (c *stepClock) advance(d time.Duration) { c.at = c.at.Add(d) }

// trickle is a body that hands over one byte a Read, each after a wait — a
// client sending as slowly as it likes.
type trickle struct {
	clock *stepClock
	wait  time.Duration
}

func (t trickle) Read(p []byte) (int, error) {
	t.clock.advance(t.wait)
	p[0] = 'x'
	return 1, nil
}

// prompt is a body that answers at once.
type prompt struct{}

func (prompt) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// A TRICKLE IS REFUSED ONCE ITS MEBIBYTE'S BUDGET IS SPENT INSIDE READS, and a
// prompt client whose server is slow between reads never is. A deadline
// re-armed whole at every Read would admit a byte every twenty-nine seconds
// for ever; one charged by the wall clock would refuse the second client.
// Shortened to a pace of a second, and timed on a clock the case moves itself.
func TestAPacedBodyChargesOnlyTheTimeInsideRead(t *testing.T) {
	t.Parallel()
	rc := http.NewResponseController(httptest.NewRecorder())
	clock := &stepClock{at: time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)}

	slow := &pacedBody{r: trickle{clock: clock, wait: 300 * time.Millisecond}, rc: rc,
		pace: time.Second, now: clock.now}
	buf := make([]byte, 64)
	var err error
	reads := 0
	for ; reads < 10 && err == nil; reads++ {
		_, err = slow.Read(buf)
	}
	if !errors.Is(err, errFileBody) || !strings.Contains(err.Error(), "took more than") {
		t.Fatalf("a trickle read for 10 rounds ended with %v, want it refused as the client's", err)
	}
	// Four waits of 300 ms spend the second; the fifth read finds it gone.
	if reads != 5 {
		t.Errorf("the trickle was refused on read %d, want the fifth: the first after "+
			"its budget ran out", reads)
	}

	waited := &pacedBody{r: prompt{}, rc: rc, pace: time.Second, now: clock.now}
	for range 4 {
		// THE SERVER'S OWN WORK between reads: more than the pace in
		// all, none of it the client's.
		clock.advance(400 * time.Millisecond)
		if _, err := waited.Read(buf); err != nil {
			t.Fatalf("a prompt client was refused for the server's own time: %v", err)
		}
	}

	// A MEBIBYTE THAT ARRIVES OPENS THE NEXT ONE'S WHOLE BUDGET: what was
	// left over is not carried, and what was spent is not either.
	whole := &pacedBody{r: io.LimitReader(prompt{}, 3<<20), rc: rc, pace: time.Hour, now: clock.now}
	whole.budget, whole.left = time.Millisecond, 10
	if _, err := whole.Read(make([]byte, 20)); err != nil {
		t.Fatal(err)
	}
	if whole.budget != time.Hour || whole.left != 1<<20-10 {
		t.Fatalf("after crossing into a new mebibyte: budget %v, %d left; want the "+
			"whole pace and the mebibyte less what crossed", whole.budget, whole.left)
	}
}
