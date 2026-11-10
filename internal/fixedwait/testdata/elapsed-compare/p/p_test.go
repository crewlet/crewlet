package p

import (
	"testing"
	"time"
)

func TestBefore(t *testing.T) {
	deadline := time.Now().Add(time.Second)
	if !time.Now().Before(deadline) {
		t.Fatal("late")
	}
}

func TestAfter(t *testing.T) {
	deadline := time.Now().Add(time.Second)
	if time.Now().After(deadline) {
		t.Fatal("late")
	}
}

func TestCompare(t *testing.T) {
	deadline := time.Now().Add(time.Second)
	if time.Now().Compare(deadline) >= 0 {
		t.Fatal("late")
	}
}

func TestAsMethodValues(t *testing.T) {
	start := time.Now()
	before, after, compare := start.Before, start.After, start.Compare
	_, _, _ = before(time.Now()), after(time.Now()), compare(time.Now())
}

func TestAsMethodExpressions(t *testing.T) {
	_ = time.Time.Before
	_ = (*time.Time).After
	_ = stamp.Compare
}

func TestThroughAnAliasAnEmbeddingAndAPointer(t *testing.T) {
	var a, b stamp
	_ = a.Before(b)
	var s stamped
	_ = s.After(time.Time{})
	at := &a
	_ = at.Compare(b)
}

func TestThroughAConversion(t *testing.T) {
	var i instant
	_ = time.Time(i).Before(time.Time{})
}
