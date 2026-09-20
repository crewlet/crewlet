package httpx_test

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/httpx"
)

// A CAP IS NOT A CUT. The whole reason [httpx.ReadBody] exists is that
// io.LimitReader stops at its limit and reports io.EOF, which everything
// downstream reads as a body that genuinely ended there — so a client that
// capped and then decoded turned a truncated answer into "unexpected end of
// JSON input", naming neither the endpoint nor the cap.
func TestABodyPastTheCeilingIsRefusedRatherThanCut(t *testing.T) {
	t.Parallel()

	const max = 16
	body := strings.NewReader(strings.Repeat("x", max+1))

	got, err := httpx.ReadBody(body, max)
	if err == nil {
		t.Fatalf("read %d bytes and reported success: a body past the "+
			"ceiling came back as a shorter body", len(got))
	}
	if !errors.Is(err, httpx.ErrResponseTooLarge) {
		t.Errorf("err = %v, want ErrResponseTooLarge — a caller deciding what "+
			"an oversized answer MEANS has to be able to tell it from a "+
			"syntax error", err)
	}
	if got != nil {
		t.Errorf("returned %d bytes beside the refusal; a refused read has no "+
			"payload, and handing one back is how the cut gets used anyway", len(got))
	}
}

// EXACTLY THE CEILING IS NOT PAST IT. The read takes one byte more than the
// cap to make an overrun visible, and an off-by-one there would refuse every
// answer that happens to fill the budget.
func TestABodyExactlyAtTheCeilingIsServedWhole(t *testing.T) {
	t.Parallel()

	const max = 16
	want := strings.Repeat("x", max)

	got, err := httpx.ReadBody(strings.NewReader(want), max)
	if err != nil {
		t.Fatalf("a body of exactly the ceiling was refused: %v", err)
	}
	if string(got) != want {
		t.Errorf("got %d bytes, want the whole %d", len(got), max)
	}
}

// A READ THAT DIED IS NOT AN OVERSIZED ONE, and the caller needs to tell them
// apart: one is an endpoint answering something absurd, the other is a
// connection that dropped mid-answer.
func TestAFailedReadKeepsItsOwnError(t *testing.T) {
	t.Parallel()

	boom := errors.New("connection reset")
	_, err := httpx.ReadBody(io.MultiReader(
		strings.NewReader("part"), errReader{boom}), 1024)
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want the read's own failure", err)
	}
	if errors.Is(err, httpx.ErrResponseTooLarge) {
		t.Error("a broken read was reported as an oversized answer")
	}
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }
