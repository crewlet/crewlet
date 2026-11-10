package p

import (
	"bytes"
	"cmp"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestOtherComparisons(t *testing.T) {
	a, b := version{1}, version{2}
	_ = a.Compare(b) < 0 || a.Before(b) || b.After(a)
	_ = instant(time.Now()).Before(instant{}) || instant{}.After(instant{}) || instant{}.Compare(instant{}) == 0
	_ = page{}.After + page{}.Before
	_ = strings.Compare("a", "b") + bytes.Compare(nil, nil) + cmp.Compare(1, 2) + slices.Compare([]int{}, []int{})
}

func TestEqualDecidesNothingAboutElapsedTime(t *testing.T) {
	start := time.Now()
	if !start.Equal(start) {
		t.Fatal("not itself")
	}
	equal := start.Equal
	_ = equal
}

func TestTheWaitOfTheSameName(t *testing.T) {
	select {
	case <-time.After(time.Second): //fixedwait:liveness only bounds the case
	default:
	}
}
