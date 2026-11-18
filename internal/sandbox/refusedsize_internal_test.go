package sandbox

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/kept"
)

// A REFUSAL ON THE RUN'S ROW STATES A SIZE PAST ITS BOUND ROUNDED UP: an answer
// past what the row carries (AnswerTooLargeError, logged as
// `sandbox_answer_refused`) and a held call's arguments past their share. The
// bound is in kept.Size's units, which round whole KiB down, so a figure a byte
// past it in the same units reads as the bound itself ("weighs 512 KiB ..., and
// an answer may weigh 512 KiB") and the operator sees nothing wrong; stated in
// kept.SizeUp, it reads past.
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
