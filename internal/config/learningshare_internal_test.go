package config

import (
	"errors"
	"math"
	"testing"
)

// LEARNING'S SHARE IS A FRACTION ABOVE 0 AND AT MOST 1: unset reads the
// default, 1 is uncapped, and 0, a negative, anything above 1 and NaN (which
// every comparison would otherwise let through, uncapped) are refused.
func TestLearningsShareIsAFractionInItsRange(t *testing.T) {
	t.Parallel()
	if got := (&Learning{}).Share(); got != DefaultLearningShare {
		t.Errorf("an unset share reads %v, want %v", got, DefaultLearningShare)
	}
	for _, c := range []struct {
		name  string
		value float64
		ok    bool
	}{
		{"the default", 0.15, true}, {"uncapped", 1, true}, {"zero", 0, false},
		{"negative", -0.1, false}, {"above 1", 1.5, false}, {"NaN", math.NaN(), false},
	} {
		l := DefaultLearning()
		l.MaxShare = &c.value
		if refused := errors.Is(l.validate(Path{}), ErrOutOfRange); refused == c.ok {
			t.Errorf("max_share %s: refused %v, want refused %v", c.name, refused, !c.ok)
		}
	}
	l := DefaultLearning()
	if err := l.validate(Path{}); err != nil {
		t.Errorf("an unset share is refused: %v", err)
	}
}
