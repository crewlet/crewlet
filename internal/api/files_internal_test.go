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

// trickle is a body that hands over one byte a Read, each after a wait — a
// client sending as slowly as it likes.
type trickle struct{ wait time.Duration }

func (t trickle) Read(p []byte) (int, error) {
	time.Sleep(t.wait)
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
// Shortened to a pace of a second, which is what lets both run here.
func TestAPacedBodyChargesOnlyTheTimeInsideRead(t *testing.T) {
	t.Parallel()
	rc := http.NewResponseController(httptest.NewRecorder())

	slow := &pacedBody{r: trickle{wait: 300 * time.Millisecond}, rc: rc, pace: time.Second}
	buf := make([]byte, 64)
	var err error
	for reads := 0; reads < 10 && err == nil; reads++ {
		_, err = slow.Read(buf)
	}
	if !errors.Is(err, errFileBody) || !strings.Contains(err.Error(), "took more than") {
		t.Fatalf("a trickle read for 10 rounds ended with %v, want it refused as the client's", err)
	}

	waited := &pacedBody{r: prompt{}, rc: rc, pace: time.Second}
	for range 4 {
		// THE SERVER'S OWN WORK between reads: more than the pace in
		// all, none of it the client's.
		time.Sleep(400 * time.Millisecond)
		if _, err := waited.Read(buf); err != nil {
			t.Fatalf("a prompt client was refused for the server's own time: %v", err)
		}
	}

	// A MEBIBYTE THAT ARRIVES OPENS THE NEXT ONE'S WHOLE BUDGET: what was
	// left over is not carried, and what was spent is not either.
	whole := &pacedBody{r: io.LimitReader(prompt{}, 3<<20), rc: rc, pace: time.Hour}
	whole.budget, whole.left = time.Millisecond, 10
	if _, err := whole.Read(make([]byte, 20)); err != nil {
		t.Fatal(err)
	}
	if whole.budget != time.Hour || whole.left != 1<<20-10 {
		t.Fatalf("after crossing into a new mebibyte: budget %v, %d left; want the "+
			"whole pace and the mebibyte less what crossed", whole.budget, whole.left)
	}
}
