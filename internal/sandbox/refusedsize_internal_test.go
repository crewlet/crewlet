package sandbox

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/kept"
)

// A REFUSAL ON THE RUN'S ROW STATES A SIZE PAST ITS BOUND ROUNDED UP. Two of
// the row's refusals say what they refused beside the bound they refused it
// against: an answer past what the row carries as one (the error a reply too
// large for its run is logged with, `sandbox_answer_too_large`), and a held
// call's arguments past their share. The bound is stated in kept.Size's units,
// which round whole KiB down, so a figure stated in the same units a byte past
// the bound read as the bound itself — "weighs 512 KiB on its row, and an
// answer may weigh 512 KiB" — and the operator reading it saw nothing wrong.
// Stated rounded up (kept.SizeUp), it reads past.
//
// Mutation: state either refused figure in kept.Size, and a byte past its
// bound reads as the bound.
func TestARefusalOnTheRowStatesASizePastItsBoundRoundedUp(t *testing.T) {
	t.Parallel()
	answer := (&AnswerTooLargeError{TurnID: "t1", Weighs: MaxAnswerBytes + 1, May: MaxAnswerBytes}).Error()
	if want := "weighs 513 KiB on its row, and an answer may weigh 512 KiB"; !strings.Contains(answer, want) {
		t.Errorf("an answer a byte past its share is refused as %q, want it to say %q", answer, want)
	}

	sealed := strings.Repeat("a", MaxHeldArgsBytes+1-kept.Quoted(""))
	if n := kept.Quoted(sealed); n != MaxHeldArgsBytes+1 {
		t.Fatalf("the premise: arguments sealed a byte past their share, not %d bytes", n)
	}
	err := HeldArgsFit(sealed)
	if !errors.Is(err, ErrHeldCallTooLarge) {
		t.Fatalf("HeldArgsFit = %v, want arguments a byte past their share refused", err)
	}
	if want := "weigh 257 KiB once sealed, and a held call's may weigh 256 KiB"; !strings.Contains(err.Error(), want) {
		t.Errorf("held arguments a byte past their share are refused as %q, want it to say %q", err, want)
	}
	if err := HeldArgsFit(sealed[1:]); err != nil {
		t.Errorf("arguments at their share are refused: %v", err)
	}
}
